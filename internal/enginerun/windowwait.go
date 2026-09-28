package enginerun

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// statWindowWaits is the engine_state stat that holds the files that waited for a transfer window
// (§9.2): a file larger than the window with allowOverrun that did not start at an opening, or a
// file that found too little of the window left at its turn. Each fire of the sync schedule
// supersedes a deferred sync with a fresh plan whose items start with no wait, so without it a
// file whose turn always came late (the job started behind another destination's upload, or new
// files were planned before it every night) would wait forever. It lives in the destination's
// state, as the restic cut cap does, because a superseding job plans new items.
const statWindowWaits = "windowWaits"

// maxWindowWaits bounds statWindowWaits: the files that waited first keep their place, and a file
// that waits while the list is full is added at a later wait. A seed that reaches a window's end
// makes every remaining file wait, and only the first few ever need the opening.
const maxWindowWaits = 32

// maxFitWaits is how many windows a file that fits a whole window (with the grace) may wait for
// lack of time left at its turn: in the next one, when it is again the attempt's first transfer
// and still does not fit, it fails with notStartedMessage (with allowOverrun it starts instead
// after its first wait).
const maxFitWaits = 2

// windowWait is one file of statWindowWaits: the item's source, path in the source, size and mtime
// (another version is another upload), how many windows it waited in, and the opening of the last
// one (a window counts once however many attempts run in it).
type windowWait struct {
	Source  int64     `json:"source"`
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	MtimeNs int64     `json:"mtimeNs"`
	Waits   int       `json:"waits"`
	Opening time.Time `json:"opening"`
}

func (w windowWait) is(d engineDetail) bool {
	return w.Source == d.SourceID && w.Path == d.Source && w.Size == d.Size && w.MtimeNs == d.MtimeNs
}

// windowWaits is an attempt's copy of statWindowWaits, written back once (flushWaits).
type windowWaits struct {
	list  []windowWait
	dirty bool
}

// loadWaits reads the destination's waiting files at the start of a sync's content phase, drops
// those of the job's sources (scope) that no content item (content) of its plan is pending for
// (done, failed, or not planned any more) and those of sources no longer linked, and returns
// items with the waiting files first, oldest wait first, the rest in plan order: a file that
// waited runs at the next opening before the files planned after it.
func (r *jobRun) loadWaits(ctx context.Context, items []jobs.Item, content func(jobs.Item) bool, scope func(sourceID int64) bool) []jobs.Item {
	if r.job.DryRun {
		return items
	}
	st, err := r.s.State(ctx, r.d.ID)
	if err != nil {
		r.log(slog.LevelWarn, "could not read the files waiting for the transfer window", "error", err.Error())
		return items
	}
	var list []windowWait
	if !st.stat(statWindowWaits, &list) || len(list) == 0 {
		return items
	}
	r.waits.list = list
	if r.win.Always() {
		r.waits.list, r.waits.dirty = nil, true
		return items
	}
	details := make([]engineDetail, len(items))
	candidate := make([]bool, len(items))
	for i, it := range items {
		if content(it) {
			d, err := parseItem(it)
			details[i], candidate[i] = d, err == nil
		}
	}
	pendingAt := func(w windowWait) int {
		for i, d := range details {
			if candidate[i] && w.is(d) {
				return i
			}
		}
		return -1
	}
	kept := r.waits.list[:0]
	for _, w := range r.waits.list {
		if !r.linkedSource(w.Source) || (scope(w.Source) && pendingAt(w) < 0) {
			r.waits.dirty = true
			continue
		}
		kept = append(kept, w)
	}
	r.waits.list = kept
	first := make([]jobs.Item, 0, len(items))
	taken := map[int]bool{}
	for _, w := range r.waits.list {
		if i := pendingAt(w); i >= 0 && !taken[i] {
			taken[i] = true
			first = append(first, items[i])
		}
	}
	if len(first) == 0 {
		return items
	}
	for i, it := range items {
		if !taken[i] {
			first = append(first, it)
		}
	}
	return first
}

// waitsOf is how many windows an item's file waited in (its entry of statWindowWaits, or the
// item's own count of overrun waits when the list had no room).
func (r *jobRun) waitsOf(d engineDetail) int {
	n := d.OverrunWaits
	for _, w := range r.waits.list {
		if w.is(d) {
			n = max(n, w.Waits)
		}
	}
	return n
}

// noteWait counts a window an item's file waited in (once per window period).
func (r *jobRun) noteWait(d engineDetail, now time.Time) {
	var opening time.Time
	if end := r.win.EndsAt(now); !end.IsZero() {
		opening = end.Add(-r.win.Length())
	}
	for i := range r.waits.list {
		w := &r.waits.list[i]
		if !w.is(d) {
			continue
		}
		if !w.Opening.Equal(opening) {
			w.Waits++
			w.Opening = opening
			r.waits.dirty = true
		}
		return
	}
	if len(r.waits.list) >= maxWindowWaits {
		return
	}
	r.waits.list = append(r.waits.list, windowWait{Source: d.SourceID, Path: d.Source, Size: d.Size, MtimeNs: d.MtimeNs, Waits: 1,
		Opening: opening})
	r.waits.dirty = true
}

// dropWait removes an item's file from the waiting files: it starts a transfer (its place at the
// opening is used; a file that waits again joins the end of the list) or it failed.
func (r *jobRun) dropWait(d engineDetail) {
	n := len(r.waits.list)
	r.waits.list = slices.DeleteFunc(r.waits.list, func(w windowWait) bool { return w.is(d) })
	if len(r.waits.list) != n {
		r.waits.dirty = true
	}
}

// flushWaits writes the waiting files back when they changed. A failure only loses this
// attempt's counts (the files then wait once more), so it is a warning.
func (r *jobRun) flushWaits(ctx context.Context) {
	if !r.waits.dirty || r.job.DryRun {
		return
	}
	list := r.waits.list
	err := r.s.updateState(ctx, r.d.ID, func(st *EngineState) {
		if len(list) == 0 {
			delete(st.Stats, statWindowWaits)
			return
		}
		st.setStat(statWindowWaits, list)
	})
	if err != nil {
		r.log(slog.LevelWarn, "could not record the files waiting for the transfer window", "error", err.Error())
		return
	}
	r.waits.dirty = false
}

// fitAction is what happens to a file that fits a whole window but not the time left at its turn.
type fitAction int

const (
	fitWait    fitAction = iota // it waits for the next window
	fitOverrun                  // it starts alone and may run past the end (allowOverrun)
	fitFail                     // it fails with notStartedMessage
)

// notFitting decides for a file that fits a whole window (with the grace) but not the time left
// (§9.2). It waits for the next window, where it runs first; with allowOverrun, once it waited, it
// starts alone and may run past the end; without, when it waited in maxFitWaits windows and is
// again the attempt's first transfer, it fails with a warning instead of waiting forever (the
// attempts always begin late, e.g. behind another destination's upload).
func (r *jobRun) notFitting(started bool, waits int) fitAction {
	switch {
	case waits > 0 && r.win.AllowOverrun():
		return fitOverrun
	case waits >= maxFitWaits && !started:
		return fitFail
	}
	return fitWait
}
