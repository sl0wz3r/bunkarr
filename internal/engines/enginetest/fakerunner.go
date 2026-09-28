// Package enginetest holds the test doubles of the Phase 4 engines (docs/design/phase4.md §14.2):
// FakeRunner, a proc.Runner that answers engine commands with scripts (written in the test or
// loaded from the spike's fixtures in testdata/restic and testdata/rclone) and asserts everything
// the real runner guarantees; FakeEngine and FakeSession, programmable engines.Engine and
// engines.Session; FakeVersionStore, an in-memory engines.VersionStore; and, in builds with the
// enginebin tag, RealEnv, the environment of the real-binary tests (make test-engines).
package enginetest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
)

// Script is how FakeRunner answers one command.
type Script struct {
	// Stdout and Stderr are emitted in this order (all stdout lines, then all stderr lines),
	// then Lines as given (for exact interleaving).
	Stdout []string
	Stderr []string
	Lines  []proc.Line
	// Exit is the exit code.
	Exit int
	// Delay is waited before anything is emitted (cut short by Interrupt or cancellation).
	Delay time.Duration
	// Hook runs first, while the command's secret and data files exist: it can read the include
	// list, change the fake repository or object store, or block.
	Hook func(*Call)
	// UntilInterrupted keeps the command running after its output until Interrupt or the
	// context's cancellation (a transfer cut by the window's end).
	UntilInterrupted bool
	// InterruptExit is the exit code after an Interrupt or a cancellation (default 130).
	InterruptExit int
	// Adjust changes the final status (RetryBudgetExceeded, BudgetExceeded, a signal).
	Adjust func(*proc.ExitStatus)
}

// Call is one command FakeRunner ran.
type Call struct {
	Binary proc.Binary
	Args   []string
	Env    map[string]string
	// Stdin is everything the command's Stdin held.
	Stdin []byte
	// SecretFiles are the secret files as they were on disk while the command ran.
	SecretFiles map[string][]byte
	Dir         *proc.RunDir
	Cmd         proc.Cmd
	// Status is the exit status the command ended with (set when it ended).
	Status proc.ExitStatus
	// Expectation is the one that answered (nil for a handler or an unexpected command).
	Expectation *Expectation
}

// Subcommand is the first argument ("backend cleanup" for rclone's backend commands).
func (c *Call) Subcommand() string {
	switch {
	case len(c.Args) == 0:
		return ""
	case c.Args[0] == "backend" && len(c.Args) > 1:
		return "backend " + c.Args[1]
	}
	return c.Args[0]
}

// Has reports whether flag is present ("--no-lock", or "--flag=v").
func (c *Call) Has(flag string) bool {
	return slices.ContainsFunc(c.Args, func(a string) bool { return a == flag || strings.HasPrefix(a, flag+"=") })
}

// Flag returns the value of a value flag ("--flag v" or "--flag=v"), the first when repeated.
func (c *Call) Flag(flag string) (string, bool) {
	if v := c.FlagValues(flag); len(v) > 0 {
		return v[0], true
	}
	return "", false
}

// FlagValues returns every value of a (repeatable) value flag, such as restic's --tag.
func (c *Call) FlagValues(flag string) []string {
	var out []string
	for i, a := range c.Args {
		if v, ok := strings.CutPrefix(a, flag+"="); ok {
			out = append(out, v)
		} else if a == flag && i+1 < len(c.Args) {
			out = append(out, c.Args[i+1])
		}
	}
	return out
}

// DataFile reads a data file of the command's run directory (an include list, a sample list)
// while the command runs; nil when it does not exist.
func (c *Call) DataFile(name string) []byte {
	if c.Dir == nil {
		return nil
	}
	b, err := os.ReadFile(c.Dir.DataPath(name))
	if err != nil {
		return nil
	}
	return b
}

// ArgsMatcher decides whether an expectation answers a command's arguments.
type ArgsMatcher func(args []string) bool

// Args matches exactly these arguments.
func Args(want ...string) ArgsMatcher {
	return func(args []string) bool { return slices.Equal(args, want) }
}

// Prefix matches arguments that start with these words ("backup", or "cat", "config").
func Prefix(words ...string) ArgsMatcher {
	return func(args []string) bool { return len(args) >= len(words) && slices.Equal(args[:len(words)], words) }
}

// Contains matches arguments that include every one of parts.
func Contains(parts ...string) ArgsMatcher {
	return func(args []string) bool {
		for _, p := range parts {
			if !slices.Contains(args, p) {
				return false
			}
		}
		return true
	}
}

// AnyArgs matches every command of the binary.
func AnyArgs() ArgsMatcher { return func([]string) bool { return true } }

// Expectation is one expected command: it answers the first matching commands, Times of them.
type Expectation struct {
	binary proc.Binary
	match  ArgsMatcher
	script Script
	times  int // -1: any number
	calls  int
	env    []string
}

// Times makes the expectation answer n commands (default 1).
func (e *Expectation) Times(n int) *Expectation { e.times = n; return e }

// AnyTimes makes the expectation answer any number of commands, also none.
func (e *Expectation) AnyTimes() *Expectation { e.times = -1; return e }

// Env pins the command's exact set of environment names.
func (e *Expectation) Env(keys ...string) *Expectation {
	e.env = slices.Sorted(slices.Values(keys))
	return e
}

// Calls is how many commands the expectation answered.
func (e *Expectation) Calls() int { return e.calls }

type handlerKey struct {
	binary     proc.Binary
	subcommand string
}

// FakeRunner is a proc.Runner that runs scripts instead of programs. Every command goes through
// proc.Validate (so the allow-list and the no-secret-in-argv rule hold as in production); its
// secret files are written into its run directory with the real proc.RunDir (0600) and exist
// while the script runs; every sftp command must name a non-empty 0600 known_hosts file; and the
// run directory is gone when Wait returns. A command that no expectation or handler answers fails
// the test and exits 2.
type FakeRunner struct {
	t        testing.TB
	mu       sync.Mutex
	expect   []*Expectation
	handlers map[handlerKey]func(*Call) Script
	calls    []*Call
}

// NewFakeRunner returns a FakeRunner that fails t at cleanup when an expectation was not met.
func NewFakeRunner(t testing.TB) *FakeRunner {
	f := &FakeRunner{t: t, handlers: map[handlerKey]func(*Call) Script{}}
	t.Cleanup(func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, e := range f.expect {
			if e.times > 0 && e.calls < e.times {
				t.Errorf("enginetest: expected %s command ran %d of %d times", e.binary, e.calls, e.times)
			}
		}
	})
	return f
}

// Expect adds an expected command of binary whose arguments match m, answered with s.
func (f *FakeRunner) Expect(binary proc.Binary, m ArgsMatcher, s Script) *Expectation {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := &Expectation{binary: binary, match: m, script: s, times: 1}
	f.expect = append(f.expect, e)
	return e
}

// Handle answers every command of binary with this subcommand ("snapshots", "backend cleanup")
// that no expectation answers, through h: the way to build a stateful fake repository or object
// store. h runs while the command runs.
func (f *FakeRunner) Handle(binary proc.Binary, subcommand string, h func(*Call) Script) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[handlerKey{binary, subcommand}] = h
}

// Calls returns the commands run so far, in order.
func (f *FakeRunner) Calls() []*Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// CallsOf returns the commands of binary with this subcommand.
func (f *FakeRunner) CallsOf(binary proc.Binary, subcommand string) []*Call {
	var out []*Call
	for _, c := range f.Calls() {
		if c.Binary == binary && c.Subcommand() == subcommand {
			out = append(out, c)
		}
	}
	return out
}

// Start implements proc.Runner.
func (f *FakeRunner) Start(ctx context.Context, cmd proc.Cmd) (proc.Process, error) {
	f.t.Helper()
	if err := proc.Validate(cmd); err != nil {
		f.t.Errorf("enginetest: %v (argv: %s)", err, cmd)
		if cmd.Dir != nil {
			_ = cmd.Dir.Remove()
		}
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		if cmd.Dir != nil {
			_ = cmd.Dir.Remove()
		}
		return nil, err
	}
	call := &Call{Binary: cmd.Binary, Args: slices.Clone(cmd.Args), Env: maps.Clone(cmd.Env), Dir: cmd.Dir, Cmd: cmd,
		SecretFiles: map[string][]byte{}}
	if call.Env == nil {
		call.Env = map[string]string{}
	}

	f.mu.Lock()
	var exp *Expectation
	for _, e := range f.expect {
		if e.binary == cmd.Binary && (e.times < 0 || e.calls < e.times) && e.match(cmd.Args) {
			exp = e
			break
		}
	}
	var handler func(*Call) Script
	if exp != nil {
		exp.calls++
		call.Expectation = exp
	} else {
		handler = f.handlers[handlerKey{cmd.Binary, call.Subcommand()}]
	}
	f.calls = append(f.calls, call)
	f.mu.Unlock()

	unexpected := exp == nil && handler == nil
	if unexpected {
		f.t.Errorf("enginetest: unexpected command: %s", cmd)
	}
	if exp != nil && exp.env != nil {
		if got := slices.Sorted(maps.Keys(call.Env)); !slices.Equal(got, exp.env) {
			f.t.Errorf("enginetest: %s: environment %v, want exactly %v", cmd, got, exp.env)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(cmd.SecretFiles)) {
		if err := cmd.Dir.WriteSecret(name, cmd.SecretFiles[name]); err != nil {
			f.t.Errorf("enginetest: %s: secret file %s: %v", cmd, name, err)
		}
	}
	f.checkFiles(call)
	if cmd.Stdin != nil {
		b, err := io.ReadAll(cmd.Stdin)
		if err != nil {
			f.t.Errorf("enginetest: %s: read stdin: %v", cmd, err)
		}
		call.Stdin = b
	}
	p := &fakeProcess{
		f:         f,
		call:      call,
		lines:     make(chan proc.Line, 64),
		done:      make(chan struct{}),
		interrupt: make(chan struct{}),
	}
	go p.run(ctx, exp, handler, unexpected)
	return p, nil
}

// checkFiles asserts the secret files (0600, content as given) and, for sftp, the known_hosts
// file (a non-empty 0600 file), and records the secret files.
func (f *FakeRunner) checkFiles(call *Call) {
	f.t.Helper()
	for name, want := range call.Cmd.SecretFiles {
		p := call.Dir.SecretPath(name)
		fi, err := os.Lstat(p)
		if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
			f.t.Errorf("enginetest: %s: secret file %s is not a 0600 file (%v)", call.Cmd, name, err)
			continue
		}
		got, _ := os.ReadFile(p)
		if string(got) != string(want) {
			f.t.Errorf("enginetest: %s: secret file %s has other content", call.Cmd, name)
		}
		call.SecretFiles[name] = got
	}
	if call.Env[proc.RemoteEnv(proc.RemoteDest, "TYPE")] == "sftp" {
		p := call.Env[proc.RemoteEnv(proc.RemoteDest, "KNOWN_HOSTS_FILE")]
		fi, err := os.Lstat(p)
		switch {
		case p == "":
			f.t.Errorf("enginetest: %s: an sftp command without RCLONE_CONFIG_BKDEST_KNOWN_HOSTS_FILE", call.Cmd)
		case err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 || fi.Size() == 0:
			f.t.Errorf("enginetest: %s: known_hosts %s is not a non-empty 0600 file (%v)", call.Cmd, p, err)
		}
	}
}

type fakeProcess struct {
	f             *FakeRunner
	call          *Call
	lines         chan proc.Line
	done          chan struct{}
	interrupt     chan struct{}
	interruptOnce sync.Once
	status        proc.ExitStatus
	err           error
}

// Lines implements proc.Process.
func (p *fakeProcess) Lines() <-chan proc.Line { return p.lines }

// Interrupt implements proc.Process.
func (p *fakeProcess) Interrupt() { p.interruptOnce.Do(func() { close(p.interrupt) }) }

// Wait implements proc.Process.
func (p *fakeProcess) Wait() (proc.ExitStatus, error) {
	for range p.lines {
	}
	<-p.done
	if d := p.call.Dir; d != nil {
		for _, dir := range []string{d.SecretDir(), d.DataDir()} {
			if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
				p.f.t.Errorf("enginetest: %s: run directory %s left after Wait", p.call.Cmd, dir)
			}
		}
	}
	return p.status, p.err
}

func (p *fakeProcess) run(ctx context.Context, exp *Expectation, handler func(*Call) Script, unexpected bool) {
	defer close(p.done)
	st := &p.status
	var tail []string
	emit := func(l proc.Line) {
		l.Text = proc.RedactLine(l.Text, false, p.call.Cmd.Redact)
		if l.Stderr {
			tail = append(tail, l.Text)
		}
		p.lines <- l
	}
	stopped := func() bool {
		select {
		case <-p.interrupt:
			st.Interrupted = true
			return true
		case <-ctx.Done():
			st.Cancelled = true
			return true
		default:
			return false
		}
	}
	var s Script
	switch {
	case unexpected:
		s = Script{Stderr: []string{"enginetest: unexpected command: " + p.call.Cmd.String()}, Exit: 2}
	case exp != nil:
		s = exp.script
	default:
		s = handler(p.call)
	}
	if s.Delay > 0 {
		select {
		case <-time.After(s.Delay):
		case <-p.interrupt:
		case <-ctx.Done():
		}
	}
	if !stopped() {
		if s.Hook != nil {
			s.Hook(p.call)
		}
		for _, l := range s.Stdout {
			emit(proc.Line{Text: l})
		}
		for _, l := range s.Stderr {
			emit(proc.Line{Stderr: true, Text: l})
		}
		for _, l := range s.Lines {
			emit(l)
		}
		if s.UntilInterrupted {
			select {
			case <-p.interrupt:
			case <-ctx.Done():
			}
		}
	}
	st.Code = s.Exit
	if stopped() {
		st.Code = s.InterruptExit
		if st.Code == 0 {
			st.Code = 130
		}
	}
	if len(tail) > proc.StderrTailLines {
		tail = tail[len(tail)-proc.StderrTailLines:]
	}
	for _, l := range tail {
		if len(l) > proc.StderrTailBytes {
			l = l[:proc.StderrTailBytes]
		}
		st.StderrTail = append(st.StderrTail, l)
	}
	if s.Adjust != nil {
		s.Adjust(st)
	}
	if st.Cancelled {
		p.err = ctx.Err()
	}
	if d := p.call.Dir; d != nil {
		if err := d.Remove(); err != nil {
			p.f.t.Errorf("enginetest: remove run directory: %v", err)
		}
	}
	p.f.mu.Lock()
	p.call.Status = *st
	p.f.mu.Unlock()
	close(p.lines)
}

// RunDirs returns real run directories in temporary directories whose shm directory reports
// itself as a tmpfs, for the commands a FakeRunner runs.
func RunDirs(t testing.TB) *proc.RunDirs {
	t.Helper()
	return proc.NewRunDirs(t.TempDir(), proc.RunDirOptions{ShmDir: t.TempDir(), StatFS: func(string) (int64, error) {
		return 0x01021994, nil
	}})
}

// String renders a call for failure messages.
func (c *Call) String() string { return fmt.Sprintf("%s %s", c.Binary, strings.Join(c.Args, " ")) }
