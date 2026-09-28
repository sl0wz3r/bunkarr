package syncer

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines/bwlimit"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// windowDay is a Monday; windows below are 01:00 onwards, in UTC.
var windowDay = time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)

// withWindow gives the harness destination bandwidth bw and runners that read windows in UTC,
// writing copies without the token bucket (the tests control time).
func (h *harness) withWindow(bw bwlimit.Config, at time.Time) {
	h.t.Helper()
	if _, err := h.dests.Update(h.ctx, h.dest.ID, destinations.Input{Bandwidth: &bw}); err != nil {
		h.t.Fatal(err)
	}
	h.mu.Lock()
	h.clock = at
	h.mu.Unlock()
	o := Options{DB: h.db, Store: h.store, Catalog: h.cat, Scanner: h.scanner, Destinations: h.dests, Now: h.now, Location: time.UTC}
	h.sync = NewSyncRunner(o)
	h.sync.planBatch = 3
	h.sync.recheckEvery = 4
	h.sync.win.limitWriter = func(w io.Writer, _ func() int64) io.Writer { return w }
	h.verify = NewVerifyRunner(o)
	h.retention = NewRetentionRunner(RetentionOptions{Options: o, PruneHistory: h.jq.PruneHistory})
}

func (h *harness) setClock(t time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clock = t
}

func window(from, to string, grace int, overrun bool) *bwlimit.Window {
	return &bwlimit.Window{Days: bwlimit.Days, From: from, To: to, GraceMinutes: grace, AllowOverrun: overrun}
}

// runRaw runs a job without closing it (a deferred job continues later).
func (h *harness) runRaw(r jobs.Runner, j jobs.Job) (jobs.Result, error) {
	return r.Run(h.ctx, j, h.env())
}

func mustDefer(t *testing.T, err error, until time.Time) {
	t.Helper()
	d, ok := jobs.AsDeferred(err)
	if !ok {
		t.Fatalf("err = %v, want a deferral", err)
	}
	if !d.Until.Equal(until) || !strings.Contains(d.Reason, "waiting for the transfer window (01:00–") {
		t.Fatalf("deferred until %s (%q), want %s", d.Until, d.Reason, until)
	}
}

func TestSyncWindowStartAndEnd(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("a.mkv", content("a", 100), 1)
	h.writeSrc("b.mkv", content("b", 200), 2)
	h.withWindow(bwlimit.Config{Window: window("01:00", "03:00", 15, false)}, windowDay.Add(4*time.Hour))
	// Outside the window: deferred before anything happens (a dry run runs).
	j := h.newJob(jobs.TypeSync, false, jobs.Params{})
	_, err := h.runRaw(h.sync, j)
	mustDefer(t, err, windowDay.Add(25*time.Hour))
	if len(h.items(j.ID)) != 0 {
		t.Errorf("a deferred start planned %v", actions(h.items(j.ID)))
	}
	if _, st, _ := h.mustSync(true, jobs.Params{}); st.FilesCopied != 2 {
		t.Errorf("dry run outside the window: %+v", st)
	}
	// Inside: the window ends after the first item; the second does not start.
	h.setClock(windowDay.Add(25*time.Hour + 30*time.Minute))
	advanced := false
	faultinject.SetHook(func(name string) {
		if name == PointRecordAfterDB && !advanced {
			advanced = true
			h.advance(2 * time.Hour)
		}
	})
	defer faultinject.SetHook(nil)
	_, err = h.runRaw(h.sync, j)
	mustDefer(t, err, windowDay.Add(49*time.Hour))
	items := h.items(j.ID)
	if len(itemsBy(items, jobs.ActionCopy, jobs.ItemDone)) != 1 || len(itemsBy(items, jobs.ActionCopy, jobs.ItemPending)) != 1 {
		t.Fatalf("items after the window closed: %+v", items)
	}
	faultinject.SetHook(nil)
	// The next window: the job continues with its plan and completes.
	h.setClock(windowDay.Add(49*time.Hour + time.Minute))
	res, err := h.run(h.sync, j)
	if err != nil {
		t.Fatalf("resumed: %v", err)
	}
	if st := res.Stats.(SyncStats); st.FilesCopied != 2 || st.FilesFailed != 0 {
		t.Errorf("resumed stats %+v", st)
	}
	for _, f := range []string{"a.mkv", "b.mkv"} {
		if _, err := os.Stat(h.dstPath(filepath.Join(h.src.DestFolder, f))); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	// Verify and retention defer outside the window too (not their dry runs).
	h.setClock(windowDay.Add(52 * time.Hour))
	if _, err := h.run(h.verify, h.newJob(jobs.TypeVerify, false, jobs.Params{})); err == nil {
		t.Error("verify ran outside the window")
	} else {
		mustDefer(t, err, windowDay.Add(73*time.Hour))
	}
	if _, err := h.run(h.retention, h.newJob(jobs.TypeRetention, false, jobs.Params{DestinationID: h.dest.ID})); err == nil {
		t.Error("retention ran outside the window")
	}
	if _, err := h.run(h.verify, h.newJob(jobs.TypeVerify, true, jobs.Params{})); err != nil {
		t.Errorf("verify dry run outside the window: %v", err)
	}
}

func TestSyncWindowCutsARunningCopy(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("big.mkv", content("big", 50_000), 1)
	h.withWindow(bwlimit.Config{Window: window("01:00", "03:00", 15, false)}, windowDay.Add(90*time.Minute))
	// The window's end (plus grace) arrives while the item copies.
	h.sync.win.afterFunc = func(d time.Duration, f func()) func() bool {
		h.advance(d)
		f()
		return func() bool { return false }
	}
	j := h.newJob(jobs.TypeSync, false, jobs.Params{})
	_, err := h.runRaw(h.sync, j)
	mustDefer(t, err, windowDay.Add(25*time.Hour))
	items := h.items(j.ID)
	if len(itemsBy(items, jobs.ActionCopy, jobs.ItemPending)) != 1 {
		t.Fatalf("items %+v", items)
	}
	if d, _ := ParseDetail(items[0]); d.WindowCuts != 1 {
		t.Errorf("detail %+v", d)
	}
	if len(h.records()) != 0 {
		t.Errorf("a cut copy was recorded: %+v", h.records())
	}
	assertNoTemp(t, h.dstDir)
	// Cut again in the next window: the item fails with the warning and the job completes.
	h.setClock(windowDay.Add(25*time.Hour + time.Minute))
	res, err := h.run(h.sync, j)
	if err != nil {
		t.Fatalf("second window: %v", err)
	}
	if st := res.Stats.(SyncStats); st.FilesFailed != 1 || res.Warnings == 0 {
		t.Errorf("stats %+v", st)
	}
	items = h.items(j.ID)
	if items[0].Status != jobs.ItemFailed || !strings.Contains(items[0].Error, "larger than the transfer window") {
		t.Errorf("item %+v", items[0])
	}
	assertNoTemp(t, h.dstDir)
}

func TestSyncWindowOversize(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("a-big.mkv", content("big", 1<<20), 1)
	h.writeSrc("b.mkv", content("b", 100), 2)
	// 1 MiB at 1 KiB/s takes 1024 s; the window is 10 minutes without grace.
	bw := bwlimit.Config{UploadKiBps: 1, Window: window("01:00", "01:10", 0, false)}
	h.withWindow(bw, windowDay.Add(time.Hour))
	res, st, j := h.mustSync(false, jobs.Params{})
	if st.FilesCopied != 1 || st.FilesFailed != 1 || res.Warnings == 0 {
		t.Fatalf("stats %+v", st)
	}
	failed := h.items(j.ID)
	for _, it := range failed {
		if it.Status == jobs.ItemFailed && it.Error != "larger than the transfer window at 1.0 KiB/s: allow the window to overrun, or raise the limit" {
			t.Errorf("error %q", it.Error)
		}
	}
	// With allowOverrun the big file runs first and alone, past the end; nothing starts after it.
	h2 := newHarness(t)
	h2.writeSrc("a-big.mkv", content("big", 1<<20), 1)
	h2.writeSrc("b.mkv", content("b", 100), 2)
	bw.Window.AllowOverrun = true
	h2.withWindow(bw, windowDay.Add(time.Hour))
	cutRequested := false
	h2.sync.win.afterFunc = func(time.Duration, func()) func() bool {
		cutRequested = true
		return func() bool { return false }
	}
	advanced := false
	faultinject.SetHook(func(name string) {
		if name == PointRecordAfterDB && !advanced {
			advanced = true
			h2.advance(20 * time.Minute)
		}
	})
	defer faultinject.SetHook(nil)
	j2 := h2.newJob(jobs.TypeSync, false, jobs.Params{})
	_, err := h2.runRaw(h2.sync, j2)
	mustDefer(t, err, windowDay.Add(25*time.Hour))
	if cutRequested {
		t.Error("the overrunning file had a stop timer")
	}
	items := h2.items(j2.ID)
	if got := itemsBy(items, jobs.ActionCopy, jobs.ItemDone); len(got) != 1 || !strings.HasSuffix(got[0], "a-big.mkv") {
		t.Errorf("done %v", got)
	}
	faultinject.SetHook(nil)
	// An oversize file that is not the attempt's first item waits for the next opening.
	h3 := newHarness(t)
	h3.writeSrc("a.mkv", content("a", 100), 1)
	h3.writeSrc("b-big.mkv", content("big", 1<<20), 2)
	h3.withWindow(bw, windowDay.Add(time.Hour))
	j3 := h3.newJob(jobs.TypeSync, false, jobs.Params{})
	_, err = h3.runRaw(h3.sync, j3)
	mustDefer(t, err, windowDay.Add(25*time.Hour))
	h3.setClock(windowDay.Add(25 * time.Hour))
	if res, err := h3.run(h3.sync, j3); err != nil || res.Stats.(SyncStats).FilesCopied != 2 {
		t.Errorf("next opening: %v %+v", err, res.Stats)
	}
}

func TestSyncBandwidthLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("takes 2 s")
	}
	h := newHarness(t)
	h.writeSrc("a.mkv", content("a", 128<<10), 1)
	if _, err := h.dests.Update(h.ctx, h.dest.ID, destinations.Input{Bandwidth: &bwlimit.Config{UploadKiBps: 64}}); err != nil {
		t.Fatal(err)
	}
	h.newRunners()
	start := time.Now()
	h.mustSync(false, jobs.Params{})
	// 128 KiB at 64 KiB/s: 2 s; the bucket holds 100 ms.
	if el := time.Since(start); el < 1800*time.Millisecond {
		t.Errorf("the copy took %s at 64 KiB/s", el)
	}
	var limit int64
	for _, p := range h.rep.progress {
		if p.LimitBytesPerSec > 0 {
			limit = p.LimitBytesPerSec
		}
	}
	if limit != 64<<10 {
		t.Errorf("progress limit %d", limit)
	}
}

// assertNoTemp fails when a Bunkarr temp file is left under dir.
func assertNoTemp(t *testing.T, dir string) {
	t.Helper()
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(d.Name(), ".bunkarr-tmp-") {
			t.Errorf("temp file left: %s", p)
		}
		return nil
	})
}
