package engines

import (
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines/bwlimit"
	"github.com/sl0wz3r/bunkarr/internal/testhooks"
)

// Window is a job's transfer window (§9.2, S27): the destination's bandwidth.window in the
// container's time zone, or, in an e2e build whose BUNKARR_TEST_WINDOW names the destination,
// absolute times (closed before open, open until close, closed until reopen, open after). The
// zero Window is always open.
type Window struct {
	w    *bwlimit.Window
	loc  *time.Location
	test *absoluteWindow
}

type absoluteWindow struct {
	open, close, reopen time.Time
}

// WindowFor returns the window of destination destinationID with bandwidth bw in loc (nil: the
// process's local zone). The testhooks override applies when it names the destination; the
// destination's own grace and allowOverrun still apply then (0 and false without a window).
func WindowFor(destinationID int64, bw bwlimit.Config, loc *time.Location) Window {
	if loc == nil {
		loc = time.Local
	}
	w := Window{w: bw.Window, loc: loc}
	if open, closeAt, reopen, ok := testhooks.Window(destinationID); ok {
		w.test = &absoluteWindow{open: open, close: closeAt, reopen: reopen}
	}
	return w
}

// Always reports whether the window is always open (no window).
func (w Window) Always() bool { return w.w == nil && w.test == nil }

// IsOpen reports whether a job may run at t.
func (w Window) IsOpen(t time.Time) bool {
	if w.test != nil {
		return (!t.Before(w.test.open) && t.Before(w.test.close)) || !t.Before(w.test.reopen)
	}
	return w.w.Open(t, w.loc)
}

// NextOpen returns t when the window is open at t, else when it opens next (the time a deferred
// job resumes). The zero time means never (a window without valid days).
func (w Window) NextOpen(t time.Time) time.Time {
	if w.test != nil {
		switch {
		case w.IsOpen(t):
			return t
		case t.Before(w.test.open):
			return w.test.open
		default:
			return w.test.reopen
		}
	}
	return w.w.NextOpen(t, w.loc)
}

// EndsAt returns the end of the open period that contains t: the job stops at EndsAt plus Grace
// (Progress.WindowEndsAt). The zero time means no end (always open, or the test window after it
// reopened) or that t is outside the window.
func (w Window) EndsAt(t time.Time) time.Time {
	if w.test != nil {
		if !t.Before(w.test.open) && t.Before(w.test.close) {
			return w.test.close
		}
		return time.Time{}
	}
	return w.w.EndsAt(t, w.loc)
}

// Grace is how long a job may run past EndsAt before it is stopped.
func (w Window) Grace() time.Duration { return w.w.Grace() }

// AllowOverrun reports whether a file that cannot fit a whole window may start alone at the
// window's opening and run past its end.
func (w Window) AllowOverrun() bool { return w.w != nil && w.w.AllowOverrun }

// Length is the length of one open period (0 when always open).
func (w Window) Length() time.Duration {
	if w.test != nil {
		return w.test.close.Sub(w.test.open)
	}
	return w.w.Length()
}

// Describe renders the window for messages ("01:00–07:00"; "" when always open), as in
// "waiting for the transfer window (01:00–07:00)".
func (w Window) Describe() string {
	if w.test != nil {
		return w.test.open.In(w.loc).Format("15:04:05") + "–" + w.test.close.In(w.loc).Format("15:04:05")
	}
	return w.w.Describe()
}
