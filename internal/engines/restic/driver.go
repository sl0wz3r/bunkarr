package restic

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
	"github.com/sl0wz3r/bunkarr/internal/engines/rclone"
)

// This file is the command layer of the restic driver (docs/design/phase4.md §6, §10): Driver
// holds what every command needs, Repo binds one destination with its secrets and job runtime,
// and run builds, runs and parses one command:
//
//   - the repository password reaches restic only as RESTIC_PASSWORD_FILE, a 0600 secret file of
//     the command's run directory on tmpfs; the storage credentials only through the
//     RCLONE_CONFIG_BKDEST_* variables that restic's rclone backend (D21) inherits (Env); argv
//     carries paths, flags, tags and -o rclone.program / rclone.connections only (S22);
//   - read-only commands run with --no-lock, exclusive ones with --retry-lock 30m (§6.7);
//   - budgets follow S26 (Test and Create 60 s and a 20 s retry budget; listings, identity
//     reads and unlock ListingBudget; backups, check and prune none), and restic's exit codes
//     become the errors of §10.3.

// Budgets and defaults (S26).
const (
	DefaultListingBudget = 10 * time.Minute
	DefaultRetryBudget   = 10 * time.Minute
	TestBudget           = 60 * time.Second
	TestRetryBudget      = 20 * time.Second
	// RetryLock is --retry-lock of exclusive operations and version backups (§6.7).
	RetryLock = 30 * time.Minute
	// ForgetChunk is how many snapshot ids one forget names (§6.5 step 3).
	ForgetChunk = 100
	// KeyFile is the file whose presence excludes a directory from every backup (S28): the
	// config directory always holds it.
	KeyFile = "bunkarr.key"
)

// Driver runs restic commands for destinations. The wiring fills it once.
type Driver struct {
	Runner  proc.Runner
	RunDirs *proc.RunDirs
	// Rclone probes the storage of remote repositories in Test (§4.5 step 3).
	Rclone *rclone.Driver
	// CacheRoot is <config>/cache/restic; each destination's cache is CacheRoot/<id>
	// (RESTIC_CACHE_DIR). A Test without a destination id uses a cache in its run directory.
	CacheRoot string
	// RclonePath is the rclone program restic's rclone backend runs (-o rclone.program=; remote
	// kinds only). It is config.ResolveEngineBinaries' validated path.
	RclonePath string
	Log        *slog.Logger
	// Version is the restic version discovery found, reported by Test.
	Version string
	// ListingBudget overrides DefaultListingBudget.
	ListingBudget time.Duration
	// Now is the clock of Create's attach warning (time.Now when nil).
	Now func() time.Time
	// sleep replaces GuardedUnlock's wait in tests (sleepCtx when nil).
	sleep func(ctx context.Context, d time.Duration) error
}

// Kind implements the Kind of engines.Engine.
func (d *Driver) Kind() engines.Kind { return engines.Restic }

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

// sleepCtx waits dur or until ctx ends.
func (d *Driver) sleepCtx(ctx context.Context, dur time.Duration) error {
	if d.sleep != nil {
		return d.sleep(ctx, dur)
	}
	t := time.NewTimer(dur)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (d *Driver) listingBudget() time.Duration {
	if d.ListingBudget > 0 {
		return d.ListingBudget
	}
	return DefaultListingBudget
}

// Repo is one destination's restic repository for one job, or for one Test or Create.
type Repo struct {
	drv      *Driver
	dest     engines.Destination
	sec      engines.Secrets
	rt       engines.Runtime
	runner   proc.Runner
	dirs     *proc.RunDirs
	cacheDir string
	bwlimit  string
	test     bool
}

// Connect binds dest (any kind) with its secrets and a job's runtime. It runs nothing. The
// runtime's Runner and RunDirs are used when set, else the driver's.
func (d *Driver) Connect(dest engines.Destination, s engines.Secrets, rt engines.Runtime) (*Repo, error) {
	if _, err := Repository(dest); err != nil {
		return nil, err
	}
	if s.Encryption.ResticPassword == "" {
		return nil, errors.New("restic: no repository password")
	}
	r := &Repo{drv: d, dest: dest, sec: s, rt: rt, runner: rt.Runner, dirs: rt.RunDirs}
	if r.runner == nil {
		r.runner = d.Runner
	}
	if r.dirs == nil {
		r.dirs = d.RunDirs
	}
	if r.runner == nil || r.dirs == nil {
		return nil, errors.New("restic: no command runner or run directories")
	}
	if dest.Kind.Remote() {
		if !filepath.IsAbs(d.RclonePath) {
			return nil, errors.New("restic: no rclone program for the rclone backend")
		}
		bw, err := rclone.BWLimit(dest.Bandwidth)
		if err != nil {
			return nil, fmt.Errorf("restic: bandwidth: %w", err)
		}
		r.bwlimit = bw
	}
	if d.CacheRoot != "" && dest.ID > 0 {
		r.cacheDir = filepath.Join(d.CacheRoot, strconv.FormatInt(dest.ID, 10))
	}
	return r, nil
}

// Destination returns the bound destination.
func (r *Repo) Destination() engines.Destination { return r.dest }

// command is one restic command to run.
type command struct {
	words []string
	// args builds the flags and positionals after the words (the run directory holds the data
	// files already).
	args   func(rd *proc.RunDir) []string
	data   map[string][]byte
	budget time.Duration
	// stdout receives each stdout line; nil collects them.
	stdout    func(line string) error
	onLine    func(Line)
	interrupt <-chan struct{}
}

func (cmd command) name() string { return "restic " + strings.Join(cmd.words, " ") }

// outcome is what one command printed and how it ended.
type outcome struct {
	status    proc.ExitStatus
	stdout    []string
	exitError *ExitError
	errors    []ErrorLine
	summary   *Summary
	// lastText is the last stderr line that is not JSON (a retry line, the signal line).
	lastText string
	hostKey  bool
}

func (o outcome) message() string {
	switch {
	case o.exitError != nil && o.exitError.Message != "":
		return strings.TrimSpace(o.exitError.Message)
	case o.lastText != "":
		return o.lastText
	}
	for i := len(o.status.StderrTail) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(o.status.StderrTail[i]); t != "" {
			return t
		}
	}
	return ""
}

// run runs one command.
func (r *Repo) run(ctx context.Context, cmd command) (outcome, error) {
	var out outcome
	rd, err := r.dirs.New(r.rt.JobID)
	if err != nil {
		return out, fmt.Errorf("%s: %w", cmd.name(), err)
	}
	pc, err := r.build(rd, cmd)
	if err != nil {
		_ = rd.Remove()
		return out, fmt.Errorf("%s: %w", cmd.name(), err)
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	started := time.Now()
	p, err := r.runner.Start(cctx, pc)
	if err != nil {
		_ = rd.Remove()
		return out, fmt.Errorf("%s: %w", cmd.name(), err)
	}
	done := make(chan struct{})
	childrenFile := ""
	if !slices.Contains(pc.Args, "--no-lock") {
		// It may take a lock: its child is recorded for the next process too (children.go).
		childrenFile = r.drv.childrenPath()
	}
	ended := trackChildren(started, done, childrenFile, func(err error) {
		r.drv.log().Warn("Could not record a restic process for the lock check after a restart (§6.7)", "error", err)
	})
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
	for l := range p.Lines() {
		if strings.Contains(l.Text, "knownhosts: key mismatch") || strings.Contains(l.Text, "knownhosts: key is unknown") {
			out.hostKey = true
		}
		parsed, isJSON := ParseLine(l.Text)
		switch {
		case isJSON && parsed.ExitError != nil:
			out.exitError = parsed.ExitError
		case isJSON && parsed.Error != nil:
			out.errors = append(out.errors, *parsed.Error)
		case isJSON && parsed.Summary != nil:
			out.summary = parsed.Summary
		case l.Stderr && !isJSON && strings.TrimSpace(l.Text) != "":
			out.lastText = strings.TrimSpace(strings.TrimPrefix(l.Text, "\x1b[2K"))
		}
		if isJSON && cmd.onLine != nil && stopErr == nil {
			cmd.onLine(parsed)
		}
		if l.Stderr {
			continue
		}
		switch {
		case stopErr != nil:
		case cmd.stdout != nil:
			if err := cmd.stdout(l.Text); err != nil {
				stopErr = err
				cancel()
			}
		default:
			out.stdout = append(out.stdout, l.Text)
		}
	}
	st, err := p.Wait()
	out.status = st
	close(done)
	ended(time.Now())
	switch {
	case stopErr != nil:
		return out, stopErr
	case err != nil:
		if ctxErr := ctx.Err(); ctxErr != nil {
			return out, ctxErr
		}
		return out, fmt.Errorf("%s: %w", cmd.name(), err)
	}
	return out, nil
}

// build turns cmd into a proc.Cmd: the password file and the SFTP known_hosts or S3 CA
// certificate as secret files, the environment of Env, and -o rclone.program and
// rclone.connections for remote kinds.
func (r *Repo) build(rd *proc.RunDir, cmd command) (proc.Cmd, error) {
	for name, content := range cmd.data {
		if err := rd.WriteData(name, content); err != nil {
			return proc.Cmd{}, err
		}
	}
	pc := proc.Cmd{Binary: proc.Restic, Dir: rd, Redact: r.sec.Values(),
		SecretFiles: map[string][]byte{"password": []byte(r.sec.Encryption.ResticPassword)}}
	var knownHosts, caCert string
	if r.dest.Kind == engines.SFTP && r.dest.Remote.SFTP != nil {
		s := r.dest.Remote.SFTP
		kh, err := rclone.KnownHosts(s.Host, s.Port, s.HostKeys)
		if err != nil {
			return proc.Cmd{}, err
		}
		pc.SecretFiles["known_hosts"] = []byte(kh)
		knownHosts = rd.SecretPath("known_hosts")
	}
	if s3 := r.dest.Remote.S3; r.dest.Kind == engines.S3 && s3 != nil && s3.CACert != "" {
		pc.SecretFiles["ca.pem"] = []byte(s3.CACert)
		caCert = rd.SecretPath("ca.pem")
	}
	cache := r.cacheDir
	if cache == "" {
		cache = filepath.Join(rd.DataDir(), "cache")
	}
	env, err := Env(r.dest, r.sec, rd.SecretPath("password"), cache, r.bwlimit, knownHosts, caCert)
	if err != nil {
		return proc.Cmd{}, err
	}
	pc.Env = env
	pc.Args = append(append([]string{}, cmd.words...), cmd.args(rd)...)
	if r.dest.Kind.Remote() {
		pc.Args = append(pc.Args, "-o", "rclone.program="+r.drv.RclonePath)
		if t := r.dest.Transfers; t > 0 {
			pc.Args = append(pc.Args, "-o", "rclone.connections="+strconv.Itoa(min(t, 128)))
		}
	}
	pc.Budget = cmd.budget
	pc.RetryBudget = r.rt.RetryBudget
	if pc.RetryBudget <= 0 {
		pc.RetryBudget = DefaultRetryBudget
	}
	if r.test {
		pc.Budget, pc.RetryBudget = TestBudget, TestRetryBudget
	}
	name := cmd.name()
	pc.OnIdle = func(silent time.Duration) {
		if r.rt.Reporter != nil {
			r.rt.Reporter.Log(slog.LevelWarn, "engine command printed nothing for a while", "command", name,
				"silent", silent.Round(time.Second).String())
		}
	}
	return pc, nil
}

// Errors of the restic driver besides the engines sentinels (ErrRepositoryMissing,
// ErrWrongPassword, ErrLocked, ErrHostKeyChanged). *Error wraps one of them.
var (
	// ErrFailed: exit 1 (or another code a command does not expect), with exit_error's message.
	ErrFailed = errors.New("restic failed")
	// ErrInterrupted: Bunkarr interrupted the command (a transfer window's end) where the caller
	// did not expect it.
	ErrInterrupted = errors.New("interrupted")
	// ErrBudget: the command ran longer than its wall-clock budget (S26).
	ErrBudget = errors.New("ran longer than its time budget")
	// ErrRetryBudget: restic kept retrying ("returned error, retrying") longer than the retry
	// budget (S26); the message is the last such line.
	ErrRetryBudget = errors.New("kept retrying longer than the retry budget")
	// ErrNoSummary: a backup ended without a summary line.
	ErrNoSummary = errors.New("backup ended without a summary")
)

// Error is a failed restic command.
type Error struct {
	// Command is "restic <subcommand>".
	Command string
	Code    int
	// Message is exit_error's message or the last stderr line (redacted).
	Message string
	Err     error
}

// Error implements error.
func (e *Error) Error() string {
	s := fmt.Sprintf("%s: %v", e.Command, e.Err)
	if e.Code != 0 {
		s += fmt.Sprintf(" (exit %d)", e.Code)
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// Unwrap returns the sentinel.
func (e *Error) Unwrap() error { return e.Err }

// Class names what an exit code means (§10.3).
func Class(code int) string {
	switch code {
	case 0:
		return "ok"
	case 3:
		return "partial"
	case 10:
		return "repository-missing"
	case 11:
		return "locked"
	case 12:
		return "wrong-password"
	case 130, 143:
		return "signal"
	}
	return "fatal"
}

// fail turns a failed outcome into an *Error (§10.3).
func fail(cmd command, out outcome) *Error {
	st := out.status
	e := &Error{Command: cmd.name(), Code: st.Code, Message: out.message()}
	switch {
	case st.BudgetExceeded:
		e.Err = ErrBudget
	case st.RetryBudgetExceeded:
		e.Err, e.Message = ErrRetryBudget, st.LastRetryLine
	case st.Interrupted:
		e.Err = ErrInterrupted
	case out.hostKey:
		e.Err = engines.ErrHostKeyChanged
	case st.Code == 10:
		e.Err = engines.ErrRepositoryMissing
	case st.Code == 11:
		e.Err = engines.ErrLocked
	case st.Code == 12:
		e.Err = engines.ErrWrongPassword
	default:
		e.Err = ErrFailed
	}
	return e
}

// check returns nil for exit 0 and an *Error for anything else.
func check(cmd command, out outcome) error {
	if out.status.Code == 0 && !out.status.Stopped() {
		return nil
	}
	return fail(cmd, out)
}
