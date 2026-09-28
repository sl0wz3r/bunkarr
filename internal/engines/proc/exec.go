package proc

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// ExecOptions configures NewExecRunner.
type ExecOptions struct {
	// ResticPath and RclonePath are the programs (config.ResolveEngineBinaries); "" makes Start
	// fail with ErrNoBinary for that engine.
	ResticPath string
	RclonePath string
	// Log receives the runner's own lines (refused commands, idle and retry warnings, the tmpfs
	// fallback); nil discards them.
	Log *slog.Logger
	// TermAfter and KillAfter override the stop escalation (DefaultTermAfter, DefaultKillAfter);
	// tests only.
	TermAfter time.Duration
	KillAfter time.Duration
	// IdleRepeat overrides IdleRepeat; tests only.
	IdleRepeat time.Duration
	// RetryQuiet ends a streak of retry lines: a gap this long (at most the command's
	// RetryBudget) without one (default 2 min).
	RetryQuiet time.Duration
	// ReapDelay is how long the output may stay open after the command exited (a grandchild
	// holding it) before the process group is killed and the output closed (default 5 s).
	ReapDelay time.Duration
}

// defaultPath is the child's PATH when Bunkarr has none.
const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// inheritedEnv are the only variables of Bunkarr's own environment a child gets (S22).
var inheritedEnv = []string{"PATH", "TZ", "LANG", "TMPDIR"}

// testHookBeforeStart runs after the secret files are written and before the process starts
// (tests: a panic there must still remove the run directory).
var testHookBeforeStart func(Cmd)

// NewExecRunner returns the production Runner: os/exec without a shell, each command in its own
// process group (§10.1, S22, S26).
func NewExecRunner(o ExecOptions) Runner {
	if o.TermAfter <= 0 {
		o.TermAfter = DefaultTermAfter
	}
	if o.KillAfter <= 0 {
		o.KillAfter = DefaultKillAfter
	}
	if o.IdleRepeat <= 0 {
		o.IdleRepeat = IdleRepeat
	}
	if o.RetryQuiet <= 0 {
		o.RetryQuiet = 2 * time.Minute
	}
	if o.ReapDelay <= 0 {
		o.ReapDelay = 5 * time.Second
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return &execRunner{o: o}
}

type execRunner struct {
	o            ExecOptions
	fallbackOnce sync.Once
}

// Start implements Runner.
func (r *execRunner) Start(ctx context.Context, c Cmd) (_ Process, err error) {
	started := false
	defer func() {
		// Also on a panic: the deferred call runs while the panic unwinds.
		if !started && c.Dir != nil {
			_ = c.Dir.Remove()
		}
	}()
	if err := Validate(c); err != nil {
		r.o.Log.Error("engine command refused (nothing ran)", "argv", c.String(), "error", err)
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := r.o.ResticPath
	if c.Binary == Rclone {
		path = r.o.RclonePath
	}
	if path == "" {
		return nil, fmt.Errorf("%w: %s", ErrNoBinary, c.Binary)
	}
	home := os.TempDir()
	if c.Dir != nil {
		home = c.Dir.DataDir()
		if w := c.Dir.Warning(); w != "" {
			r.fallbackOnce.Do(func() { r.o.Log.Warn(w) })
		}
	}
	for _, name := range slices.Sorted(maps.Keys(c.SecretFiles)) {
		if err := c.Dir.WriteSecret(name, c.SecretFiles[name]); err != nil {
			return nil, fmt.Errorf("%s %s: secret file: %w", c.Binary, c.Args[0], err)
		}
	}
	if testHookBeforeStart != nil {
		testHookBeforeStart(c)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", c.Binary, err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_, _ = outR.Close(), outW.Close()
		return nil, fmt.Errorf("%s: %w", c.Binary, err)
	}
	cmd := exec.Command(path, c.Args...)
	cmd.Env = childEnv(c, home)
	cmd.Dir = home
	cmd.Stdin = c.Stdin
	cmd.Stdout = outW
	cmd.Stderr = errW
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = r.o.ReapDelay
	startErr := cmd.Start()
	_, _ = outW.Close(), errW.Close()
	if startErr != nil {
		_, _ = outR.Close(), errR.Close()
		return nil, fmt.Errorf("start %s: %w", c.Binary, startErr)
	}
	started = true
	r.o.Log.Debug("engine command started", "cmd", c, "pid", cmd.Process.Pid)
	p := &execProcess{
		o:         r.o,
		c:         c,
		cmd:       cmd,
		pgid:      cmd.Process.Pid,
		outR:      outR,
		errR:      errR,
		lines:     make(chan Line, 64),
		done:      make(chan struct{}),
		interrupt: make(chan struct{}),
		retryHit:  make(chan struct{}),
		start:     time.Now(),
	}
	p.lastLine.Store(p.start.UnixNano())
	go p.run(ctx)
	return p, nil
}

// childEnv builds the child's environment: PATH, TZ, LANG and TMPDIR from Bunkarr's own, HOME,
// then c.Env (whose names Validate checked). Nothing else of Bunkarr's environment is inherited.
func childEnv(c Cmd, home string) []string {
	env := map[string]string{}
	for _, k := range inheritedEnv {
		if v, ok := os.LookupEnv(k); ok && v != "" {
			env[k] = v
		}
	}
	if env["PATH"] == "" {
		env["PATH"] = defaultPath
	}
	env["HOME"] = home
	maps.Copy(env, c.Env)
	out := make([]string, 0, len(env))
	for _, k := range slices.Sorted(maps.Keys(env)) {
		out = append(out, k+"="+env[k])
	}
	return out
}

type execProcess struct {
	o    ExecOptions
	c    Cmd
	cmd  *exec.Cmd
	pgid int

	outR, errR *os.File
	lines      chan Line
	done       chan struct{}

	interrupt     chan struct{}
	interruptOnce sync.Once
	retryHit      chan struct{}
	retryOnce     sync.Once

	start    time.Time
	lastLine atomic.Int64 // unix nanos of the last line (or the start)
	sending  atomic.Int32 // readers waiting for the consumer to take a line

	mu          sync.Mutex
	tail        []string
	retryStart  time.Time
	lastRetryAt time.Time
	lastRetry   string

	status ExitStatus
	err    error
}

// Lines implements Process.
func (p *execProcess) Lines() <-chan Line { return p.lines }

// Interrupt implements Process.
func (p *execProcess) Interrupt() { p.interruptOnce.Do(func() { close(p.interrupt) }) }

// Wait implements Process.
func (p *execProcess) Wait() (ExitStatus, error) {
	for range p.lines {
	}
	<-p.done
	return p.status, p.err
}

// signal sends sig to the command's process group.
func (p *execProcess) signal(sig syscall.Signal) {
	if err := syscall.Kill(-p.pgid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		p.o.Log.Warn("signal engine process group", "cmd", p.c, "signal", unix.SignalName(sig), "error", err)
	}
}

// retryQuiet is the gap that ends a streak of retry lines.
func (p *execProcess) retryQuiet() time.Duration {
	return min(p.o.RetryQuiet, p.c.RetryBudget)
}

// noteRetry records a retry line and reports whether the streak outlasted the budget.
func (p *execProcess) noteRetry(text string, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.retryStart.IsZero() || now.Sub(p.lastRetryAt) > p.retryQuiet() {
		p.retryStart = now
	}
	p.lastRetryAt, p.lastRetry = now, text
	return now.Sub(p.retryStart) > p.c.RetryBudget
}

// retryExceeded reports whether the current streak is still going and outlasted the budget.
func (p *execProcess) retryExceeded(now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.retryStart.IsZero() && now.Sub(p.lastRetryAt) <= p.retryQuiet() && now.Sub(p.retryStart) > p.c.RetryBudget
}

// read turns one stream into redacted lines.
func (p *execProcess) read(f *os.File, stderr bool, wg *sync.WaitGroup) {
	defer wg.Done()
	br := bufio.NewReaderSize(f, 64*1024)
	// A line is read up to maxRead bytes; the rest of a longer one is discarded, and the kept
	// part is redacted as partial before it is cut to MaxLineBytes.
	const maxRead = MaxLineBytes + 64*1024
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		partial := false
		if len(buf)+len(chunk) > maxRead {
			buf = append(buf, chunk[:maxRead-len(buf)]...)
			partial = true
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = br.ReadSlice('\n')
			}
		} else {
			buf = append(buf, chunk...)
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
		}
		if len(buf) > 0 {
			p.emit(RedactLine(strings.TrimRight(string(buf), "\r\n"), partial, p.c.Redact), stderr)
		}
		buf = buf[:0]
		if err != nil {
			return
		}
	}
}

// emit records and forwards one redacted line.
func (p *execProcess) emit(text string, stderr bool) {
	now := time.Now()
	p.lastLine.Store(now.UnixNano())
	if stderr {
		p.mu.Lock()
		if len(p.tail) == StderrTailLines {
			p.tail = append(p.tail[:0], p.tail[1:]...)
		}
		p.tail = append(p.tail, tailLine(text))
		p.mu.Unlock()
		if p.c.RetryBudget > 0 && strings.Contains(text, RetryMarker) && p.noteRetry(text, now) {
			p.retryOnce.Do(func() { close(p.retryHit) })
		}
	}
	p.sending.Add(1)
	p.lines <- Line{Stderr: stderr, Text: text}
	p.sending.Add(-1)
}

// run supervises the command until it ended, its output is drained and its run directory is gone.
func (p *execProcess) run(ctx context.Context) {
	defer close(p.done)
	var readers sync.WaitGroup
	readers.Add(2)
	go p.read(p.outR, false, &readers)
	go p.read(p.errR, true, &readers)
	readersDone := make(chan struct{})
	go func() {
		readers.Wait()
		close(p.lines)
		close(readersDone)
	}()
	exited := make(chan error, 1)
	go func() { exited <- p.cmd.Wait() }()

	var budget <-chan time.Time
	if p.c.Budget > 0 {
		t := time.NewTimer(p.c.Budget)
		defer t.Stop()
		budget = t.C
	}
	idleTimeout := p.c.IdleTimeout
	if idleTimeout == 0 {
		idleTimeout = DefaultIdleTimeout
	}
	var idle <-chan time.Time
	var idleTimer *time.Timer
	threshold := idleTimeout
	if idleTimeout > 0 {
		idleTimer = time.NewTimer(idleTimeout)
		defer idleTimer.Stop()
		idle = idleTimer.C
	}
	var retryTick <-chan time.Time
	if p.c.RetryBudget > 0 {
		t := time.NewTicker(max(10*time.Millisecond, min(time.Second, p.c.RetryBudget/10)))
		defer t.Stop()
		retryTick = t.C
	}

	ctxDone := ctx.Done()
	interrupt := (<-chan struct{})(p.interrupt)
	retryHit := (<-chan struct{})(p.retryHit)
	var term, kill <-chan time.Time
	stopping := false
	stop := func(flag *bool, why string) {
		*flag = true
		if stopping {
			return
		}
		stopping = true
		p.o.Log.Debug("stopping engine command", "cmd", p.c, "reason", why)
		p.signal(syscall.SIGINT)
		term = time.After(p.o.TermAfter)
	}

	var waitErr error
loop:
	for {
		select {
		case waitErr = <-exited:
			break loop
		case <-ctxDone:
			ctxDone = nil
			stop(&p.status.Cancelled, "cancelled")
		case <-budget:
			budget = nil
			p.o.Log.Warn("engine command ran out of its time budget", "cmd", p.c, "budget", p.c.Budget)
			stop(&p.status.BudgetExceeded, "budget")
		case <-interrupt:
			interrupt = nil
			stop(&p.status.Interrupted, "interrupted")
		case <-retryHit:
			retryHit = nil
			p.stopRetry(stop)
		case now := <-retryTick:
			if p.retryExceeded(now) {
				retryTick = nil
				p.stopRetry(stop)
			}
		case <-term:
			term = nil
			p.signal(syscall.SIGTERM)
			kill = time.After(p.o.KillAfter)
		case <-kill:
			kill = nil
			p.signal(syscall.SIGKILL)
		case <-idle:
			silent := time.Since(time.Unix(0, p.lastLine.Load()))
			if silent < idleTimeout {
				threshold = idleTimeout
			}
			if silent >= threshold {
				p.o.Log.Warn("engine command printed nothing for a while", "cmd", p.c, "silent", silent.Round(time.Second))
				if p.c.OnIdle != nil {
					p.c.OnIdle(silent)
				}
				threshold += p.o.IdleRepeat
			}
			idleTimer.Reset(threshold - silent)
		}
	}

	// The command exited. Give its output a moment; if it stays open, a grandchild (rclone serve
	// restic under restic) holds it: kill what is left of the group. The run directory goes as
	// soon as the command is gone, even when nobody reads the output.
	select {
	case <-readersDone:
	case <-time.After(p.o.ReapDelay):
		p.signal(syscall.SIGKILL)
	}
	if p.c.Dir != nil {
		if err := p.c.Dir.Remove(); err != nil {
			p.o.Log.Error("remove engine run directory", "cmd", p.c, "error", err)
		}
	}
	// Drain the output. A consumer that is slow to read keeps the readers sending, which is
	// fine; output that stays open with nothing arriving and nobody sending is held by a process
	// outside the group, and is closed.
	for done := false; !done; {
		last := p.lastLine.Load()
		select {
		case <-readersDone:
			done = true
		case <-time.After(p.o.ReapDelay):
			if p.lastLine.Load() == last && p.sending.Load() == 0 {
				p.o.Log.Warn("engine command output held open after it exited; closing it", "cmd", p.c)
				_, _ = p.outR.Close(), p.errR.Close()
			}
		}
	}
	_, _ = p.outR.Close(), p.errR.Close()
	p.finish(ctx, waitErr)
}

// stopRetry stops a command whose retry lines outlasted the budget.
func (p *execProcess) stopRetry(stop func(*bool, string)) {
	p.mu.Lock()
	p.status.LastRetryLine = p.lastRetry
	p.mu.Unlock()
	p.o.Log.Warn("engine command kept retrying", "cmd", p.c, "retryBudget", p.c.RetryBudget, "lastRetry", p.status.LastRetryLine)
	stop(&p.status.RetryBudgetExceeded, "retry budget")
}

// finish fills in the exit status after the process ended.
func (p *execProcess) finish(ctx context.Context, waitErr error) {
	st := p.cmd.ProcessState
	var exitErr *exec.ExitError
	switch {
	case st == nil:
		p.err = fmt.Errorf("wait for %s: %w", p.c.Binary, waitErr)
	default:
		if ws, ok := st.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			p.status.Code = 128 + int(ws.Signal())
			p.status.Signal = unix.SignalName(ws.Signal())
		} else {
			p.status.Code = st.ExitCode()
		}
		if waitErr != nil && !errors.As(waitErr, &exitErr) && !errors.Is(waitErr, exec.ErrWaitDelay) {
			p.err = fmt.Errorf("wait for %s: %w", p.c.Binary, waitErr)
		}
	}
	p.mu.Lock()
	p.status.StderrTail = slices.Clone(p.tail)
	p.mu.Unlock()
	if p.status.Cancelled && p.err == nil {
		p.err = ctx.Err()
	}
	p.o.Log.Debug("engine command finished", "cmd", p.c, "code", p.status.Code, "signal", p.status.Signal,
		"duration", time.Since(p.start).Round(time.Millisecond))
}
