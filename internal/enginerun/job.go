package enginerun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/logging"
)

// jobRun is one engine job's context: the destination row and its secrets (from one row,
// destinations.SecretsFor), the reporter that redacts them, the transfer window, the exec runtime
// and the progress.
type jobRun struct {
	s    *Service
	kind engines.Kind
	job  jobs.Job
	env  jobs.Env
	rep  jobs.Reporter
	d    destinations.Destination
	ed   engines.Destination
	sec  engines.Secrets
	win  engines.Window
	rt   engines.Runtime

	started  time.Time
	warnings int
	// windowWait: an item waits for the next window (the job defers after the others, §9.2).
	windowWait bool
	// waits are the destination's files that waited for a window (statWindowWaits), a sync's copy.
	waits windowWaits

	// coverage is the irreplaceable-flag coverage of the retention holds, loaded once per attempt.
	coverage       func(sourceID int64, rel string) (int64, bool)
	coverageLoaded bool

	mu       sync.Mutex
	progress jobs.Progress
}

// prepare is the preflight every engine job shares (§6.2 step 1, §6.5 step 1, §7.3, §9.2, S21,
// S25): the destination is enabled, its create finished, its kit custody is confirmed (dry runs
// excepted) and its engine available; outside the transfer window a job that is not a dry run
// defers before it does anything; every host the engine dials passes netguard; and the
// destination's location and secrets are read from its one row.
func (s *Service) prepare(ctx context.Context, kind engines.Kind, job jobs.Job, env jobs.Env, what string) (*jobRun, error) {
	if env.Items == nil {
		return nil, fmt.Errorf("%s: the job has no item store", what)
	}
	d, err := s.o.Destinations.Get(ctx, job.Params.DestinationID)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	if err := s.gate(d, job.DryRun); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	now := s.now()
	win := engines.WindowFor(d.ID, d.Bandwidth, s.loc)
	if !job.DryRun && !win.Always() && !win.IsOpen(now) {
		return nil, deferUntil(win, win.NextOpen(now))
	}
	if err := s.checkHosts(ctx, d); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	ed, sec, err := s.o.Destinations.SecretsFor(ctx, d.ID)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	if ed.Engine != kind {
		return nil, fmt.Errorf("%s: destination %q is not a %s destination", what, d.Name, kind)
	}
	base := env.Reporter
	if base == nil {
		base = nopReporter{}
	}
	r := &jobRun{s: s, kind: kind, job: job, env: env, d: d, ed: ed, sec: sec, win: win, started: now,
		rep: &redactingReporter{rep: base, values: sec.Values()}}
	r.rt = s.runtime(engines.Runtime{JobID: job.ID, DryRun: job.DryRun, Reporter: r.rep, Window: win, Log: s.log,
		RetryBudget: s.retryBudget(ctx)})
	if v := s.availability().Of(kind).Version; v != "" {
		version := string(kind) + " " + v
		if err := s.updateState(ctx, d.ID, func(st *EngineState) { st.EngineVersion = version }); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// log writes a job log line.
func (r *jobRun) log(level slog.Level, msg string, args ...any) { r.rep.Log(level, msg, args...) }

// warn writes a warning and counts it.
func (r *jobRun) warn(msg string, args ...any) {
	r.warnings++
	r.rep.Log(slog.LevelWarn, msg, args...)
}

// report publishes the progress with the limit in force and the window's end (§10.4).
func (r *jobRun) report(fn func(p *jobs.Progress)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if fn != nil {
		fn(&r.progress)
	}
	now := r.s.now()
	r.progress.LimitBytesPerSec = r.limitAt(now)
	r.progress.WindowEndsAt = nil
	if end := r.win.EndsAt(now); !end.IsZero() {
		r.progress.WindowEndsAt = &end
	}
	r.rep.Progress(r.progress)
}

// deleteDays is the destination's deletedDays (S5).
func (r *jobRun) expiry(now time.Time) time.Time {
	days := r.d.Retention.DeletedDays
	if days < 1 {
		days = destinations.DefaultDeletedDays
	}
	return now.Add(time.Duration(days) * 24 * time.Hour)
}

// --- windows and rates (§9.2, S27) ---

// deferUntil returns the deferral to next, or an error when the window never opens.
func deferUntil(w engines.Window, next time.Time) error {
	if next.IsZero() {
		return fmt.Errorf("the transfer window (%s) never opens: check its days", w.Describe())
	}
	return jobs.Defer(next, fmt.Sprintf("waiting for the transfer window (%s)", w.Describe()))
}

// limitAt is the upload limit in force at t in bytes per second (0: none).
func (r *jobRun) limitAt(t time.Time) int64 {
	up, _ := r.d.Bandwidth.InForce(t, r.s.loc)
	return up * 1024
}

// rateAt is the rate a file is expected to upload at (§9.2): the lower of the limit in force and
// the measured throughput; 0 when neither is known.
func (r *jobRun) rateAt(ctx context.Context, t time.Time) int64 {
	rate := r.limitAt(t)
	st, err := r.s.State(ctx, r.d.ID)
	if err == nil && st.ThroughputBps != nil && *st.ThroughputBps > 0 {
		if tp := int64(*st.ThroughputBps); rate <= 0 || tp < rate {
			rate = tp
		}
	}
	return rate
}

// open reports whether work may start at t.
func (r *jobRun) open(t time.Time) bool { return r.job.DryRun || r.win.Always() || r.win.IsOpen(t) }

// timeLeft is how long the window stays open after t (ok false: no end).
func (r *jobRun) timeLeft(t time.Time) (time.Duration, bool) {
	if r.win.Always() {
		return 0, false
	}
	end := r.win.EndsAt(t)
	if end.IsZero() {
		return 0, false
	}
	return end.Sub(t), true
}

// deferral is the deferral of a job that stops at t: to the window's next opening after the
// period that contains t (or that t is outside of).
func (r *jobRun) deferral(t time.Time) error {
	if end := r.win.EndsAt(t); !end.IsZero() {
		return deferUntil(r.win, r.win.NextOpen(end.Add(time.Second)))
	}
	return deferUntil(r.win, r.win.NextOpen(t))
}

// fitsLeft reports whether a transfer expected to take d may start with left of the window
// remaining: it is stopped at the window's end plus the grace, the same bound oversize measures a
// whole window by, so a file that is not oversize fits when it is the first transfer at a window's
// opening. A file that does not fit waits for the next window, where it runs first (loadWaits);
// notFitting ends the waits of a file whose turn always comes late (§9.2).
func (r *jobRun) fitsLeft(d, left time.Duration) bool { return d <= left+r.win.Grace() }

// waitForWindow leaves an item pending for the next window while later items of the attempt that
// fit the time left still run; the job defers when the attempt's items are done (§9.2). The wait
// is counted in the destination's waiting files, which a superseding job's plan keeps.
func (r *jobRun) waitForWindow(msg string, it jobs.Item, d engineDetail, now time.Time) {
	r.windowWait = true
	r.noteWait(d, now)
	r.log(slog.LevelInfo, msg, "path", it.RelPath)
}

// waitNotFitting handles a file that fits a whole window but not the time left at its turn
// (notFitting): it waits, or fails with a warning; overrun reports that it starts alone instead,
// without a time limit.
func (r *jobRun) waitNotFitting(ctx context.Context, it jobs.Item, d engineDetail, started bool, now time.Time, rate int64) (overrun bool, err error) {
	waits := r.waitsOf(d)
	switch r.notFitting(started, waits) {
	case fitOverrun:
		r.log(slog.LevelInfo, "a file that waited for a transfer window starts although it may run past the window's end", "path", it.RelPath)
		return true, nil
	case fitFail:
		r.dropWait(d)
		msg := notStartedMessage(waits+1, formatRate(rate))
		r.warn("a file never had enough of the transfer window left at its turn", "path", it.RelPath, "windows", waits+1)
		return false, r.finish(ctx, it.ID, jobs.ItemFailed, 0, msg)
	}
	r.waitForWindow("not enough time left in the transfer window for the file; it waits for the next window", it, d, now)
	return false, nil
}

// overrunStartSlack is how long after a window's opening an attempt may still start a file that
// cannot fit the window (allowOverrun, §9.2): later, the file waits for the next opening.
const overrunStartSlack = 30 * time.Minute

// overrunMayStart reports whether a file larger than the window may start now (allowOverrun,
// §9.2): it already waited for an opening once (waits, which the destination's waiting files keep
// across superseding plans, so a job that always starts late, behind other jobs, or that plans new
// files before it, still gets it done; such a file also runs first, loadWaits), or no transfer ran
// yet in this attempt (started) and the attempt began at the opening of the window's current
// period.
func (r *jobRun) overrunMayStart(started bool, now time.Time, waits int) bool {
	if waits > 0 {
		return true
	}
	if started {
		return false
	}
	end := r.win.EndsAt(now)
	if end.IsZero() {
		return true
	}
	opening := end.Add(-r.win.Length())
	return !r.started.Before(opening) && r.started.Sub(opening) <= overrunStartSlack
}

// waitOverrun leaves a file larger than the window pending for the next window's opening
// (allowOverrun, §9.2) and counts the wait in its detail and in the destination's waiting files.
func (r *jobRun) waitOverrun(ctx context.Context, it jobs.Item, d engineDetail, now time.Time) error {
	d.OverrunWaits++
	if err := r.setDetail(ctx, it.ID, d); err != nil {
		return err
	}
	r.waitForWindow("a file larger than the transfer window waits for the next opening", it, d, now)
	return nil
}

// expected is how long bytes take at rate.
func expected(bytes, rate int64) time.Duration {
	if rate <= 0 {
		return 0
	}
	return time.Duration(float64(bytes) / float64(rate) * float64(time.Second))
}

// oversize returns the warning for a file of size bytes that cannot be transferred within one
// whole window at rate (§9.2), or "" (no window, unknown rate, or it fits).
func (r *jobRun) oversize(size, rate int64) string {
	length := r.win.Length()
	if r.win.Always() || rate <= 0 || length <= 0 {
		return ""
	}
	if expected(size, rate) <= length+r.win.Grace() {
		return ""
	}
	return oversizeMessage(formatRate(rate))
}

// oversizeMessage is the item error of a file that cannot fit a transfer window (§9.2).
func oversizeMessage(rate string) string {
	return fmt.Sprintf("larger than the transfer window at %s: allow the window to overrun, or raise the limit", rate)
}

// notStartedMessage is the item error of a file that is not larger than the transfer window but
// found too little of it left at its turn in windows windows in a row (§9.2).
func notStartedMessage(windows int, rate string) string {
	return fmt.Sprintf("not started in %d transfer windows: too little of the window was left at its turn at %s; allow the window to overrun, or raise the limit",
		windows, rate)
}

// cutTwiceMessage is the backstop's item error: cut by the window's end in two windows.
var cutTwiceMessage = oversizeMessage("the rate reached (cut by the window's end in two windows)")

func formatRate(bps int64) string { return formatBytes(bps) + "/s" }

// windowStop is a timer that fires at the window's end plus the grace (a transfer is then
// interrupted, §9.2); a zero windowStop never fires.
type windowStop struct {
	C     <-chan struct{}
	fired func() bool
	stop  func()
}

// stopAtEnd returns the interrupt of a transfer that starts at t: it fires at the window's end
// plus the grace, and never when the window has no end or overrun is set.
func (r *jobRun) stopAtEnd(t time.Time, overrun bool) windowStop {
	end := r.win.EndsAt(t)
	if overrun || r.job.DryRun || r.win.Always() || end.IsZero() {
		return windowStop{fired: func() bool { return false }, stop: func() {}}
	}
	ch := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	fired := false
	d := end.Add(r.win.Grace()).Sub(r.s.now())
	timer := time.AfterFunc(max(d, 0), func() {
		mu.Lock()
		fired = true
		mu.Unlock()
		once.Do(func() { close(ch) })
	})
	return windowStop{C: ch, fired: func() bool {
		mu.Lock()
		defer mu.Unlock()
		return fired
	}, stop: func() { timer.Stop() }}
}

// --- reporters ---

type nopReporter struct{}

func (nopReporter) Progress(jobs.Progress)         {}
func (nopReporter) Log(slog.Level, string, ...any) {}

// redactingReporter redacts the destination's secrets (clear, obscured, escaped) from every job
// log line and progress file name before the job manager sees them (S22), on top of the
// registered secrets the manager redacts itself.
type redactingReporter struct {
	rep    jobs.Reporter
	values []string
}

func (r *redactingReporter) Progress(p jobs.Progress) {
	p.CurrentFile = logging.RedactValues(p.CurrentFile, r.values...)
	r.rep.Progress(p)
}

func (r *redactingReporter) Log(level slog.Level, msg string, args ...any) {
	out := make([]any, len(args))
	for i, a := range args {
		switch v := a.(type) {
		case string:
			out[i] = logging.RedactValues(v, r.values...)
		case error:
			out[i] = logging.RedactValues(v.Error(), r.values...)
		default:
			out[i] = a
		}
	}
	r.rep.Log(level, logging.RedactValues(msg, r.values...), out...)
}

// redactErr redacts the destination's secrets from an error that leaves the job (its text is
// stored as the job's error), keeping errors.Is.
func (r *jobRun) redactErr(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := jobs.AsDeferred(err); ok {
		return err
	}
	msg := err.Error()
	red := logging.RedactValues(msg, r.sec.Values()...)
	if red == msg {
		return err
	}
	return &redactedError{msg: red, err: err}
}

type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string        { return e.msg }
func (e *redactedError) Is(target error) bool { return errors.Is(e.err, target) }

// formatBytes renders n in binary units (1.5 GiB).
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// formatCount renders a count with thousands separators (1,234).
func formatCount(n int64) string {
	s := fmt.Sprint(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
