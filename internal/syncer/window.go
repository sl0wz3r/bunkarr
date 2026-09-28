package syncer

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/bwlimit"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// Transfer windows and bandwidth limits of filecopy destinations (docs/design/phase4.md §9.1,
// §9.2, S27):
//   - a sync, verify or retention job (not a dry run) that starts outside the destination's
//     window returns jobs.Defer(next opening, "waiting for the transfer window (01:00–07:00)")
//     before it does anything; the manager re-queues it with its plan;
//   - a sync's executor checks the window between items: after the window's end no item starts,
//     and the job defers with its pending items. An item still copying at the end plus the grace
//     is cancelled (its temp file removed; it stays pending) and the job defers. An item cut by
//     the window's end in two windows fails with the oversize warning (a backstop when no limit
//     tells the rate);
//   - a copy whose size at the upload limit in force takes longer than the whole window plus the
//     grace fails its item ("larger than the transfer window at <rate>: allow the window to
//     overrun, or raise the limit"), so the job completes instead of deferring forever; with
//     window.allowOverrun it starts alone at a window's opening (the first item of an attempt) and
//     may run past the end, and nothing else starts after the end;
//   - copies are written through a token bucket (bwlimit.NewWriter) that reads the upload limit in
//     force again every second, so a timetable change point applies during a copy;
//   - verify and retention check the window between their items.
// Progress carries the limit in force and the window's end.

// windowOptions configures the windows of the filecopy runners.
type windowOptions struct {
	loc *time.Location
	// afterFunc runs f after d (time.AfterFunc); stop cancels it. Tests replace it.
	afterFunc func(d time.Duration, f func()) (stop func() bool)
	// limitWriter is bwlimit.NewWriter; tests replace it.
	limitWriter func(w io.Writer, rate func() int64) io.Writer
}

func newWindowOptions(o Options) windowOptions {
	loc := o.Location
	if loc == nil {
		loc = time.Local
	}
	return windowOptions{loc: loc, afterFunc: func(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop },
		limitWriter: bwlimit.NewWriter}
}

// windowRun is one job's transfer window and bandwidth limit.
type windowRun struct {
	w           engines.Window
	bw          bwlimit.Config
	loc         *time.Location
	now         func() time.Time
	afterFunc   func(d time.Duration, f func()) (stop func() bool)
	limitWriter func(w io.Writer, rate func() int64) io.Writer
	// limited: the destination has an upload limit (base or timetable).
	limited bool
	// started: an item of this attempt started (an oversize item with allowOverrun waits for the
	// next opening then).
	started bool
}

// forDestination returns destination d's window and limit for one job, or nil when it has neither
// (the Phase 1-3 behaviour).
func (o windowOptions) forDestination(d destinations.Destination, now func() time.Time) *windowRun {
	w := engines.WindowFor(d.ID, d.Bandwidth, o.loc)
	limited := d.Bandwidth.UploadKiBps > 0
	for _, e := range d.Bandwidth.Timetable {
		limited = limited || e.UploadKiBps > 0
	}
	if w.Always() && !limited {
		return nil
	}
	return &windowRun{w: w, bw: d.Bandwidth, loc: o.loc, now: now, afterFunc: o.afterFunc, limitWriter: o.limitWriter, limited: limited}
}

// startCheck returns the deferral of a job of destination d that starts at t outside its window
// (nil when it may run).
func (o windowOptions) startCheck(d destinations.Destination, t time.Time) error {
	w := engines.WindowFor(d.ID, d.Bandwidth, o.loc)
	if w.Always() || w.IsOpen(t) {
		return nil
	}
	return deferUntil(w, w.NextOpen(t))
}

// deferUntil returns the deferral to next, or an error when the window never opens.
func deferUntil(w engines.Window, next time.Time) error {
	if next.IsZero() {
		return fmt.Errorf("the transfer window (%s) never opens: check its days", w.Describe())
	}
	return jobs.Defer(next, fmt.Sprintf("waiting for the transfer window (%s)", w.Describe()))
}

// open reports whether an item may start at t.
func (w *windowRun) open(t time.Time) bool { return w.w.Always() || w.w.IsOpen(t) }

// deferral returns the deferral of a job that stops at t (outside the window).
func (w *windowRun) deferral(t time.Time) error { return deferUntil(w.w, w.w.NextOpen(t)) }

// nextPeriod returns the deferral to the opening after the period that contains t.
func (w *windowRun) nextPeriod(t time.Time) error {
	end := w.w.EndsAt(t)
	if end.IsZero() {
		return w.deferral(t)
	}
	return deferUntil(w.w, w.w.NextOpen(end))
}

// rate returns the upload limit in force at t in bytes per second (0: none).
func (w *windowRun) rate(t time.Time) int64 {
	up, _ := w.bw.InForce(t, w.loc)
	return up * 1024
}

// oversize returns the warning for a copy of size bytes that cannot be transferred within one
// whole window at the limit in force at t, or "" (no window, no limit, or it fits).
func (w *windowRun) oversize(size int64, t time.Time) string {
	rate, length := w.rate(t), w.w.Length()
	if rate <= 0 || length <= 0 || w.w.Always() {
		return ""
	}
	need := time.Duration(float64(size) / float64(rate) * float64(time.Second))
	if need <= length+w.w.Grace() {
		return ""
	}
	return oversizeMessage(fmt.Sprintf("%s/s", formatBytes(rate)))
}

// oversizeMessage is the item error of a file that cannot fit a transfer window (§9.2).
func oversizeMessage(rate string) string {
	return fmt.Sprintf("larger than the transfer window at %s: allow the window to overrun, or raise the limit", rate)
}

// progress fills the limit in force and the window's end at t.
func (w *windowRun) progress(p *jobs.Progress, t time.Time) {
	p.LimitBytesPerSec = w.rate(t)
	p.WindowEndsAt = nil
	if end := w.w.EndsAt(t); !end.IsZero() {
		p.WindowEndsAt = &end
	}
}

// copyWrapper returns CopyOptions.WrapWriter: the token bucket at the upload limit in force (read
// again every second), or nil without a limit.
func (s *syncRun) copyWrapper() func(io.Writer) io.Writer {
	w := s.win
	if w == nil || !w.limited {
		return nil
	}
	return func(dst io.Writer) io.Writer {
		return w.limitWriter(dst, func() int64 { return w.rate(w.now()) })
	}
}

// runWindowed runs an item inside the destination's window: an item never starts after the
// window's end (the job defers), an oversize copy fails (or, with allowOverrun, runs alone from a
// window's opening), and a running item is cancelled at the end plus the grace (it stays pending
// and the job defers).
func (s *syncRun) runWindowed(ctx context.Context, it jobs.Item) error {
	w := s.win
	if w == nil {
		return s.runItem(ctx, it)
	}
	now := s.r.now()
	if !w.open(now) {
		return w.deferral(now)
	}
	w.progress(&s.progress, now)
	overrun := false
	if it.Action == jobs.ActionCopy || it.Action == jobs.ActionUpdate {
		if msg := w.oversize(it.Bytes, now); msg != "" {
			if !w.w.AllowOverrun() {
				return s.failItem(ctx, it, itemErr("%s", msg))
			}
			if w.started {
				// It starts alone at a window's opening: the next attempt runs it first.
				s.rep.Log(slog.LevelInfo, "a file larger than the transfer window waits for the next opening", "path", it.RelPath)
				return w.nextPeriod(now)
			}
			overrun = true
		}
	}
	w.started = true
	itemCtx := ctx
	var cut atomic.Bool
	if end := w.w.EndsAt(now); !overrun && !end.IsZero() {
		c, cancel := context.WithCancel(ctx)
		defer cancel()
		stop := w.afterFunc(end.Add(w.w.Grace()).Sub(now), func() {
			cut.Store(true)
			cancel()
		})
		defer stop()
		itemCtx = c
	}
	err := s.runItem(itemCtx, it)
	if err != nil && cut.Load() && ctx.Err() == nil {
		return s.windowCut(ctx, it)
	}
	return err
}

// windowCut handles an item the window's end cancelled: it stays pending and the job defers; an
// item cut in an earlier window too fails with the oversize warning instead (the rate is unknown,
// and a job never defers forever).
func (s *syncRun) windowCut(ctx context.Context, it jobs.Item) error {
	now := s.r.now()
	x := s.lastItem
	if x != nil && x.it.ID == it.ID {
		if x.d.WindowCuts >= 1 {
			s.rep.Log(slog.LevelWarn, "an item was cut by the transfer window's end twice", "path", it.RelPath)
			return s.failItem(ctx, it, itemErr("%s", oversizeMessage("the rate reached (cut by the window's end in two windows)")))
		}
		x.d.WindowCuts++
		if err := x.saveDetail(ctx); err != nil {
			return err
		}
	}
	s.rep.Log(slog.LevelInfo, "the transfer window closed: the item stays pending", "path", it.RelPath)
	return s.win.deferral(now)
}

// checkWindow is the verify and retention runners' check between steps: nil while the window is
// open, else the deferral.
func checkWindow(w *windowRun, now time.Time) error {
	if w == nil || w.open(now) {
		return nil
	}
	return w.deferral(now)
}
