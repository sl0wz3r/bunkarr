package rclone

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
)

// This file is the command layer of the rclone driver (docs/design/phase4.md §7, §10): Driver
// holds what every command needs (the runner, the run directories, the budgets), Conn binds one
// destination with its secrets and job runtime, and run builds, runs and parses one command:
//
//   - argv carries only paths, flags and non-secret options (S22); the child's environment comes
//     from Env (the closed option table of §4.4), RCLONE_BWLIMIT from BWLimit, and the pinned
//     known_hosts and the S3 CA certificate are secret files of the command's run directory;
//   - every remote path is Root plus a relative path this package validated (S2, fences.go);
//   - every line is already redacted by the runner (Cmd.Redact = Secrets.Values); stderr is
//     parsed as rclone's JSON log (log.go);
//   - budgets and retries follow S26: Test and Create 60 s per command and a 20 s retry budget,
//     listings, marker reads and small writes ListingBudget (10 min), transfers none; exit 5 is
//     retried once after RetryWait (1 min), and the exit classes of §10.3 become errors
//     (exit.go).

// Budgets and defaults (S26, §7.3).
const (
	// DefaultListingBudget bounds listings, marker reads and small writes.
	DefaultListingBudget = 10 * time.Minute
	// DefaultRetryWait is the wait before the one retry of an exit-5 command.
	DefaultRetryWait = time.Minute
	// DefaultRetryBudget is the retry budget of a job's commands when the runtime has none.
	DefaultRetryBudget = 10 * time.Minute
	// TestBudget bounds each command of Test and Create, and Test and Create themselves.
	TestBudget = 60 * time.Second
	// TestRetryBudget is the retry budget of Test and Create.
	TestRetryBudget = 20 * time.Second
	// DefaultTransfers is --transfers when the destination sets none (settings.transfers).
	DefaultTransfers = 4
	// StatsInterval is --stats of transfer commands.
	StatsInterval = "5s"
)

// Driver runs rclone commands for destinations. The wiring fills it once; Test and Create use
// its Runner and RunDirs, and a Conn uses the job runtime's when they are set.
type Driver struct {
	Runner  proc.Runner
	RunDirs *proc.RunDirs
	// Log receives the driver's own warnings (nil discards them).
	Log *slog.Logger
	// Version is the rclone version discovery found ("1.74.1"), reported by Test.
	Version string
	// ListingBudget overrides DefaultListingBudget (0: the default).
	ListingBudget time.Duration
	// RetryWait overrides DefaultRetryWait (0: the default; negative: no wait, tests only).
	RetryWait time.Duration
	// Now is the clock of the markers Create writes (time.Now when nil).
	Now func() time.Time
}

// Kind implements the Kind of engines.Engine.
func (d *Driver) Kind() engines.Kind { return engines.Rclone }

func (d *Driver) log() *slog.Logger {
	if d.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return d.Log
}

func (d *Driver) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Driver) listingBudget() time.Duration {
	if d.ListingBudget > 0 {
		return d.ListingBudget
	}
	return DefaultListingBudget
}

func (d *Driver) retryWait() time.Duration {
	switch {
	case d.RetryWait < 0:
		return 0
	case d.RetryWait == 0:
		return DefaultRetryWait
	}
	return d.RetryWait
}

// Conn is one destination's rclone access for one job, or for one Test or Create: the
// destination, its secrets and the job's runtime. It is not safe for concurrent use by
// commands that share a callback, but its commands may run concurrently otherwise.
type Conn struct {
	drv     *Driver
	dest    engines.Destination
	sec     engines.Secrets
	rt      engines.Runtime
	runner  proc.Runner
	dirs    *proc.RunDirs
	root    string
	bwlimit string
	// test gives every command TestBudget and TestRetryBudget (Test and Create).
	test bool
}

// Connect binds dest (a remote kind: sftp, s3 or b2) with its secrets and a job's runtime. It
// runs nothing. The runtime's Runner and RunDirs are used when set, else the driver's.
func (d *Driver) Connect(dest engines.Destination, s engines.Secrets, rt engines.Runtime) (*Conn, error) {
	if !dest.Kind.Remote() {
		return nil, fmt.Errorf("rclone: a destination of kind %q is not a remote (sftp, s3 or b2)", dest.Kind)
	}
	c := &Conn{drv: d, dest: dest, sec: s, rt: rt, runner: rt.Runner, dirs: rt.RunDirs, root: Root(dest)}
	if c.runner == nil {
		c.runner = d.Runner
	}
	if c.dirs == nil {
		c.dirs = d.RunDirs
	}
	if c.runner == nil || c.dirs == nil {
		return nil, errors.New("rclone: no command runner or run directories")
	}
	if c.root == "" {
		return nil, fmt.Errorf("rclone: the %s remote of destination %d is missing", dest.Kind, dest.ID)
	}
	if err := checkValue("location", c.root); err != nil {
		return nil, err
	}
	bw, err := BWLimit(dest.Bandwidth)
	if err != nil {
		return nil, fmt.Errorf("rclone: bandwidth: %w", err)
	}
	c.bwlimit = bw
	return c, nil
}

// Destination returns the bound destination.
func (c *Conn) Destination() engines.Destination { return c.dest }

// Root returns the destination root every command names (Root(dest)).
func (c *Conn) Root() string { return c.root }

// remote returns the remote path of a validated relative path ("" is the root).
func (c *Conn) remote(rel string) string {
	return joinRemote(c.root, rel)
}

func joinRemote(root, rel string) string {
	switch {
	case rel == "":
		return root
	case strings.HasSuffix(root, ":") || strings.HasSuffix(root, "/"):
		return root + rel
	}
	return root + "/" + rel
}

// transfers returns --transfers and --checkers (twice the transfers, §9.3).
func (c *Conn) transfers() (string, string) {
	t := c.dest.Transfers
	if t <= 0 {
		t = DefaultTransfers
	}
	t = min(t, 64)
	return strconv.Itoa(t), strconv.Itoa(min(2*t, 128))
}

// Log flags of the commands (§7.3, §7.7). Every command logs JSON to stderr; the commands whose
// INFO events and stats matter log at -v with stats every StatsInterval.
var (
	jsonLog     = []string{"--use-json-log"}
	transferLog = []string{"--use-json-log", "-v", "--stats", StatsInterval, "--stats-log-level", "NOTICE"}
)

// command is one rclone command to run.
type command struct {
	// words are the subcommand words ("copy"; "backend", "cleanup").
	words []string
	// args builds the positionals and flags after the words; rd is the command's run directory,
	// with the data files already written.
	args func(rd *proc.RunDir) []string
	// data are data files written to the run directory before the command starts (file lists).
	data map[string][]byte
	// stdin is the command's standard input (nil: none).
	stdin []byte
	// budget is the wall-clock budget (0: none; Test and Create always TestBudget).
	budget time.Duration
	// stdout receives each stdout line; nil collects them in outcome.stdout. An error stops the
	// command and is returned.
	stdout func(line string) error
	// stdoutLimit, when > 0, bounds what is collected in outcome.stdout: once that many bytes
	// arrived (each line counted with its "\n") Bunkarr stops the command, and what it collected
	// is the result (outcome.stdoutFull, an exit-0 status). rclone cat applies --count to each
	// object it prints, and a directory at the path (a prefix with objects under it, a directory
	// an SFTP server serves) prints every object under it, so --count alone bounds nothing.
	stdoutLimit int64
	// onLog receives each parsed stderr line.
	onLog func(LogLine)
	// interrupt, when closed, interrupts the command (a transfer window's end).
	interrupt <-chan struct{}
}

func (cmd command) name() string { return "rclone " + strings.Join(cmd.words, " ") }

// outcome is what one command printed and how it ended.
type outcome struct {
	status proc.ExitStatus
	stdout []string
	// stdoutFull: stdout reached command.stdoutLimit and Bunkarr stopped the command.
	stdoutFull bool
	// objectErrors maps an object named by an ERROR line to its last message.
	objectErrors map[string]string
	// lastError is the last error-level (or failure notice) message, redacted.
	lastError string
	events    []ObjectEvent
	stats     *Stats
	// hostKey: a line reported an SFTP host key mismatch or an unknown key.
	hostKey bool
	// undecryptable: a crypt listing met names it cannot decrypt (strict_names).
	undecryptable bool
	// maxDelete: a line reported the --max-delete threshold.
	maxDelete bool
}

// note records one parsed stderr line.
func (o *outcome) note(l LogLine) {
	text := l.Msg
	if hostKeyFailure(text) {
		o.hostKey = true
	}
	if strings.Contains(text, "undecryptable") {
		o.undecryptable = true
	}
	if strings.Contains(text, "max-delete") && strings.Contains(text, "threshold") {
		o.maxDelete = true
	}
	if l.Stats != nil {
		s := *l.Stats
		o.stats = &s
	}
	if ev, ok := l.Event(); ok {
		o.events = append(o.events, ev)
		if ev.Kind == EventError {
			if o.objectErrors == nil {
				o.objectErrors = map[string]string{}
			}
			o.objectErrors[ev.Object] = ev.Message
		}
	}
	switch l.Level {
	case LevelError, LevelCritical:
		o.lastError = l.Text()
	case LevelNotice:
		if strings.HasPrefix(l.Msg, "Failed to ") {
			o.lastError = l.Text()
		}
	}
}

// hostKeyFailure reports an SFTP host key that is not pinned (rclone's knownhosts check, also
// through restic's rclone backend).
func hostKeyFailure(text string) bool {
	return strings.Contains(text, "knownhosts: key mismatch") || strings.Contains(text, "knownhosts: key is unknown") ||
		strings.Contains(text, "knownhosts: key is revoked")
}

// message returns the most telling line of a failed command.
func (o outcome) message() string {
	if o.lastError != "" {
		return o.lastError
	}
	for i := len(o.status.StderrTail) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(o.status.StderrTail[i]); t != "" {
			return t
		}
	}
	return ""
}

// run runs cmd and retries it once after RetryWait when it exits 5 (§10.3). The error is non-nil
// only when the command could not run or the context was cancelled; the exit is in outcome.
func (c *Conn) run(ctx context.Context, cmd command) (outcome, error) {
	out, err := c.exec(ctx, cmd)
	if err != nil || out.status.Code != 5 || out.status.Stopped() {
		return out, err
	}
	c.drv.log().Warn("rclone reported a temporary error; retrying once", "command", cmd.name(), "message", out.message(),
		"destination", c.dest.ID)
	if w := c.drv.retryWait(); w > 0 {
		t := time.NewTimer(w)
		select {
		case <-ctx.Done():
			t.Stop()
			return out, ctx.Err()
		case <-cmd.interrupt:
			t.Stop()
			out.status.Interrupted = true
			return out, nil
		case <-t.C:
		}
	}
	return c.exec(ctx, cmd)
}

// exec runs cmd once.
func (c *Conn) exec(ctx context.Context, cmd command) (outcome, error) {
	var out outcome
	rd, err := c.dirs.New(c.rt.JobID)
	if err != nil {
		return out, fmt.Errorf("%s: %w", cmd.name(), err)
	}
	pc, err := c.build(rd, cmd)
	if err != nil {
		_ = rd.Remove()
		return out, fmt.Errorf("%s: %w", cmd.name(), err)
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	p, err := c.runner.Start(cctx, pc)
	if err != nil {
		_ = rd.Remove()
		return out, fmt.Errorf("%s: %w", cmd.name(), err)
	}
	done := make(chan struct{})
	defer close(done)
	if cmd.interrupt != nil {
		go func() {
			select {
			case <-cmd.interrupt:
				p.Interrupt()
			case <-done:
			}
		}()
	}
	var stopErr error
	var collected int64
	for l := range p.Lines() {
		if !l.Stderr {
			switch {
			case stopErr != nil || out.stdoutFull:
			case cmd.stdout != nil:
				if err := cmd.stdout(l.Text); err != nil {
					stopErr = err
					cancel()
				}
			default:
				out.stdout = append(out.stdout, l.Text)
				collected += int64(len(l.Text)) + 1
				if cmd.stdoutLimit > 0 && collected >= cmd.stdoutLimit {
					out.stdoutFull = true
					cancel()
				}
			}
			continue
		}
		ll := ParseLogLine(l.Text)
		out.note(ll)
		if cmd.onLog != nil && stopErr == nil {
			cmd.onLog(ll)
		}
	}
	st, err := p.Wait()
	out.status = st
	switch {
	case stopErr != nil:
		return out, stopErr
	case out.stdoutFull && ctx.Err() == nil:
		// Stopped by Bunkarr because it printed enough: how it ended is not the result.
		out.status = proc.ExitStatus{}
		return out, nil
	case err != nil:
		if ctxErr := ctx.Err(); ctxErr != nil {
			return out, ctxErr
		}
		return out, fmt.Errorf("%s: %w", cmd.name(), err)
	}
	return out, nil
}

// build turns cmd into a proc.Cmd with its environment, secret files and data files.
func (c *Conn) build(rd *proc.RunDir, cmd command) (proc.Cmd, error) {
	for name, content := range cmd.data {
		if err := rd.WriteData(name, content); err != nil {
			return proc.Cmd{}, err
		}
	}
	pc := proc.Cmd{Binary: proc.Rclone, Dir: rd, SecretFiles: map[string][]byte{}, Redact: c.sec.Values()}
	in := EnvInput{Dest: c.dest, Secrets: c.sec, BWLimit: c.bwlimit}
	if c.dest.Kind == engines.SFTP && c.dest.Remote.SFTP != nil {
		r := c.dest.Remote.SFTP
		kh, err := KnownHosts(r.Host, r.Port, r.HostKeys)
		if err != nil {
			return proc.Cmd{}, err
		}
		pc.SecretFiles["known_hosts"] = []byte(kh)
		in.KnownHostsPath = rd.SecretPath("known_hosts")
	}
	var caFlag []string
	if s3 := c.dest.Remote.S3; c.dest.Kind == engines.S3 && s3 != nil && s3.CACert != "" {
		pc.SecretFiles["ca.pem"] = []byte(s3.CACert)
		in.CACertPath = rd.SecretPath("ca.pem")
		caFlag = []string{"--ca-cert", in.CACertPath}
	}
	env, err := Env(in)
	if err != nil {
		return proc.Cmd{}, err
	}
	pc.Env = env
	pc.Args = append(append(append([]string{}, cmd.words...), cmd.args(rd)...), caFlag...)
	if cmd.stdin != nil {
		pc.Stdin = bytes.NewReader(cmd.stdin)
	}
	pc.Budget = cmd.budget
	pc.RetryBudget = c.rt.RetryBudget
	if pc.RetryBudget <= 0 {
		pc.RetryBudget = DefaultRetryBudget
	}
	if c.test {
		pc.Budget = TestBudget
		pc.RetryBudget = TestRetryBudget
	}
	name := cmd.name()
	pc.OnIdle = func(silent time.Duration) {
		if c.rt.Reporter != nil {
			c.rt.Reporter.Log(slog.LevelWarn, "engine command printed nothing for a while", "command", name,
				"silent", silent.Round(time.Second).String())
		}
	}
	return pc, nil
}
