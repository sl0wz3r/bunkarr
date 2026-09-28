package enginerun

import (
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/bwlimit"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/syncer"
)

// nightWindow is 01:00-07:00 UTC every day without grace.
func nightWindow(overrun bool, upKiB int64) *bwlimit.Config {
	return &bwlimit.Config{UploadKiBps: upKiB, Window: &bwlimit.Window{Days: bwlimit.Days, From: "01:00", To: "07:00", AllowOverrun: overrun}}
}

func mustDefer(t *testing.T, err error) *jobs.DeferredError {
	t.Helper()
	d, ok := jobs.AsDeferred(err)
	if !ok {
		t.Fatalf("err = %v, want a deferral", err)
	}
	return d
}

// TestWindowStartOutside: a job that starts outside the window defers to its next opening before
// it runs any command; a dry run runs at any time (§9.2, S27).
func TestWindowStartOutside(t *testing.T) {
	for _, kind := range []engines.Kind{engines.Restic, engines.Rclone} {
		t.Run(string(kind), func(t *testing.T) {
			h := newHarness(t, harnessOptions{kind: kind, bandwidth: nightWindow(false, 0)})
			h.writeSrc("a.mkv", content("a", 1000), 1)
			for _, typ := range []jobs.Type{jobs.TypeSync, jobs.TypeVerify, jobs.TypeRetention} {
				calls := len(h.runner.Calls())
				j := h.newJob(typ, false, jobs.Params{})
				_, err := h.runJob(j)
				h.closeJob(j.ID)
				d := mustDefer(t, err)
				if want := time.Date(2026, 9, 28, 1, 0, 0, 0, time.UTC); !d.Until.Equal(want) {
					t.Errorf("%s: until %s, want %s", typ, d.Until, want)
				}
				if n := len(h.runner.Calls()); n != calls {
					t.Errorf("%s: %d commands ran before the deferral", typ, n-calls)
				}
			}
			if _, j := h.mustRun(jobs.TypeSync, true, jobs.Params{}); len(h.items(j.ID)) != 1 {
				t.Error("the dry run did not plan")
			}
		})
	}
}

// TestResticWindowCut: at the window's end plus the grace restic is interrupted: the batch's items
// stay pending, nothing is recorded, the job defers to the next opening; a one-file batch cut in
// two windows fails with the oversize warning (the backstop, §9.2).
func TestResticWindowCut(t *testing.T) {
	h := newHarness(t, harnessOptions{kind: engines.Restic, bandwidth: nightWindow(false, 0)})
	h.writeSrc("a.mkv", content("a", 1000), 1)
	h.restic.hangBackup = true
	h.setClock(time.Date(2026, 9, 28, 6, 59, 59, 950_000_000, time.UTC))
	j := h.newJob(jobs.TypeSync, false, jobs.Params{})
	_, err := h.runJob(j)
	d := mustDefer(t, err)
	if want := time.Date(2026, 9, 29, 1, 0, 0, 0, time.UTC); !d.Until.Equal(want) {
		t.Errorf("until %s, want %s", d.Until, want)
	}
	for _, it := range h.items(j.ID) {
		if it.Status != jobs.ItemPending {
			t.Errorf("%s: %s after the cut", it.RelPath, it.Status)
		}
	}
	if rows, _ := h.svc.snapshotRows(h.ctx, h.dest.ID); len(rows) != 0 {
		t.Errorf("a cut batch was recorded: %+v", rows)
	}
	var progressed bool
	for _, p := range h.rep.progress {
		if p.Batch == 1 && p.WindowEndsAt != nil {
			progressed = true
		}
	}
	if !progressed {
		t.Error("no progress with the batch and the window's end")
	}
	// The next window cuts the same one-file batch again: it fails, the job completes.
	h.setClock(time.Date(2026, 9, 29, 6, 59, 59, 950_000_000, time.UTC))
	j.Deferrals = 1
	res, err := h.runJob(j)
	if err != nil {
		if _, ok := jobs.AsDeferred(err); !ok {
			t.Fatal(err)
		}
	}
	h.closeJob(j.ID)
	if s, msg := itemStatus(h.items(j.ID), h.dp("a.mkv")); s != jobs.ItemFailed || msg != cutTwiceMessage {
		t.Errorf("after the second cut: %s %q (res %+v, err %v)", s, msg, res, err)
	}
}

// TestRcloneWindowCutoff: a copy cut by --max-duration (exit 10) after an update's old version
// moved into the backup dir turns the live record missing (the replaced-version hold keeps the
// old version past deletedDays), leaves the items pending and defers (§7.3, §9.2, DS-9).
func TestRcloneWindowCutoff(t *testing.T) {
	h := newHarness(t, harnessOptions{kind: engines.Rclone, bandwidth: nightWindow(false, 0)})
	h.setClock(time.Date(2026, 9, 28, 2, 0, 0, 0, time.UTC))
	h.writeSrc("a.mkv", content("a1", 2000), 1)
	h.mustSync(jobs.Params{})
	h.writeSrc("a.mkv", content("a2", 2200), 5)
	h.rclone.cutAfterBackup["a.mkv"] = true
	h.advance(time1h)
	j := h.newJob(jobs.TypeSync, false, jobs.Params{})
	_, err := h.runJob(j)
	mustDefer(t, err)
	h.closeJob(j.ID)
	if r := h.mustLive(h.dp("a.mkv")); r.State != syncer.StateMissing {
		t.Errorf("live record %+v, want missing", r)
	}
	if s, _ := itemStatus(h.items(j.ID), h.dp("a.mkv")); s != jobs.ItemPending {
		t.Errorf("the cut item is %s", s)
	}
	for _, c := range h.callsOf(proc.Rclone, "copy") {
		if v, _ := c.Flag("--cutoff-mode"); !c.Has("--max-duration") || v != "soft" {
			t.Errorf("copy without --max-duration --cutoff-mode soft: %s", c)
		}
	}
	// Past deletedDays the replaced version is still held: the new one is not backed up.
	h.setClock(time.Date(2026, 11, 2, 2, 0, 0, 0, time.UTC))
	h.mustRun(jobs.TypeRetention, false, jobs.Params{})
	var held bool
	for _, r := range h.retained() {
		if r.Reason == syncer.ReasonReplaced {
			if _, ok := h.rclone.get(r.RetainedPath); ok {
				held = true
			}
		}
	}
	if !held {
		t.Error("the replaced version expired while the new one is not backed up")
	}
}

// TestWindowOversize: a file that cannot be transferred within one whole window at the rate in
// force fails its item with a warning and the job completes; with allowOverrun it runs alone
// from the window's opening without a time limit (§9.2).
func TestWindowOversize(t *testing.T) {
	for _, kind := range []engines.Kind{engines.Restic, engines.Rclone} {
		t.Run(string(kind), func(t *testing.T) {
			h := newHarness(t, harnessOptions{kind: kind, bandwidth: nightWindow(false, 1)}) // 1 KiB/s: 21.1 MiB per window
			h.setClock(time.Date(2026, 9, 28, 1, 0, 0, 0, time.UTC))
			h.writeSrc("small.mkv", content("s", 1000), 1)
			h.writeSrc("huge.mkv", content("h", 22<<20), 2)
			st, j := h.mustSync(jobs.Params{})
			if s, msg := itemStatus(h.items(j.ID), h.dp("huge.mkv")); s != jobs.ItemFailed || msg != oversizeMessage("1.0 KiB/s") {
				t.Errorf("huge.mkv: %s %q", s, msg)
			}
			if st.FilesCopied != 1 || st.FilesFailed != 1 {
				t.Errorf("stats %+v", st)
			}
			bw := nightWindow(true, 1)
			h.update(destinationsInput(*bw))
			h.advance(24 * time.Hour)
			st, j = h.mustSync(jobs.Params{})
			if s, _ := itemStatus(h.items(j.ID), h.dp("huge.mkv")); s != jobs.ItemDone {
				t.Errorf("with allowOverrun huge.mkv is %s\n%s", s, h.rep.dump())
			}
			if kind == engines.Rclone {
				c := h.callsOf(proc.Rclone, "copy")
				if last := c[len(c)-1]; last.Has("--max-duration") {
					t.Errorf("the overrun file ran with a time limit: %s", last)
				}
			}
		})
	}
}

// graceWindow is 01:00-01:10 UTC every day with 5 minutes of grace.
func graceWindow(upKiB int64) *bwlimit.Config {
	return &bwlimit.Config{UploadKiBps: upKiB, Window: &bwlimit.Window{Days: bwlimit.Days, From: "01:00", To: "01:10", GraceMinutes: 5}}
}

// TestWindowGraceBand: a file whose expected time is longer than the window but not longer than
// the window plus the grace is not oversize, so it starts at the window's opening (it is stopped
// only at the end plus the grace); it never waits forever (§9.2).
func TestWindowGraceBand(t *testing.T) {
	for _, kind := range []engines.Kind{engines.Restic, engines.Rclone} {
		t.Run(string(kind), func(t *testing.T) {
			h := newHarness(t, harnessOptions{kind: kind, bandwidth: graceWindow(1)})
			h.setClock(time.Date(2026, 9, 28, 1, 0, 0, 0, time.UTC))
			h.writeSrc("big.mkv", content("b", 12*60*1024), 1) // 12 minutes at 1 KiB/s
			h.writeSrc("z.mkv", content("z", 100), 2)
			st, j := h.mustSync(jobs.Params{})
			for _, rel := range []string{"big.mkv", "z.mkv"} {
				if s, msg := itemStatus(h.items(j.ID), h.dp(rel)); s != jobs.ItemDone {
					t.Errorf("%s: %s %q (stats %+v)\n%s", rel, s, msg, st, h.rep.dump())
				}
			}
		})
	}
}

// TestWindowWaitingFileSkipped: a file that does not fit the time left waits for the next window
// while a later file that fits still runs in this one; the job then defers and the file runs at
// the next opening (§9.2).
func TestWindowWaitingFileSkipped(t *testing.T) {
	for _, kind := range []engines.Kind{engines.Restic, engines.Rclone} {
		t.Run(string(kind), func(t *testing.T) {
			h := newHarness(t, harnessOptions{kind: kind, bandwidth: nightWindow(false, 1)})
			h.setClock(time.Date(2026, 9, 28, 6, 50, 0, 0, time.UTC))
			h.writeSrc("a.mkv", content("a", 1<<20), 1) // about 17 minutes at 1 KiB/s
			h.writeSrc("z.mkv", content("z", 1000), 2)
			j := h.newJob(jobs.TypeSync, false, jobs.Params{})
			_, err := h.runJob(j)
			mustDefer(t, err)
			items := h.items(j.ID)
			if s, _ := itemStatus(items, h.dp("z.mkv")); s != jobs.ItemDone {
				t.Errorf("z.mkv is %s: the file that fits did not run", s)
			}
			if s, _ := itemStatus(items, h.dp("a.mkv")); s != jobs.ItemPending {
				t.Errorf("a.mkv is %s", s)
			}
			h.setClock(time.Date(2026, 9, 29, 1, 0, 0, 0, time.UTC))
			j.Deferrals = 1
			if _, err := h.runJob(j); err != nil {
				t.Fatalf("the next window: %v\n%s", err, h.rep.dump())
			}
			h.closeJob(j.ID)
			if s, _ := itemStatus(h.items(j.ID), h.dp("a.mkv")); s != jobs.ItemDone {
				t.Errorf("a.mkv is %s after the next window", s)
			}
		})
	}
}

// TestWindowOverrunStartsAtOpening: with allowOverrun a file larger than the window starts only
// at a window's opening: a job that begins mid-window leaves it for the next opening (§9.2).
func TestWindowOverrunStartsAtOpening(t *testing.T) {
	for _, kind := range []engines.Kind{engines.Restic, engines.Rclone} {
		t.Run(string(kind), func(t *testing.T) {
			h := newHarness(t, harnessOptions{kind: kind, bandwidth: nightWindow(true, 1)})
			h.setClock(time.Date(2026, 9, 28, 6, 50, 0, 0, time.UTC))
			h.writeSrc("huge.mkv", content("h", 22<<20), 1)
			h.writeSrc("small.mkv", content("s", 1000), 2)
			j := h.newJob(jobs.TypeSync, false, jobs.Params{})
			_, err := h.runJob(j)
			mustDefer(t, err)
			items := h.items(j.ID)
			if s, _ := itemStatus(items, h.dp("huge.mkv")); s != jobs.ItemPending {
				t.Errorf("huge.mkv started mid-window: %s", s)
			}
			if s, _ := itemStatus(items, h.dp("small.mkv")); s != jobs.ItemDone {
				t.Errorf("small.mkv is %s", s)
			}
			// A later attempt that also begins late still starts it (it waited once).
			h.setClock(time.Date(2026, 9, 29, 3, 0, 0, 0, time.UTC))
			j.Deferrals = 1
			if _, err := h.runJob(j); err != nil {
				t.Fatalf("the next window: %v\n%s", err, h.rep.dump())
			}
			h.closeJob(j.ID)
			if s, _ := itemStatus(h.items(j.ID), h.dp("huge.mkv")); s != jobs.ItemDone {
				t.Errorf("huge.mkv is %s after the next window", s)
			}
		})
	}
}

// TestResticCutBatchShrinks: a multi-file batch the window's end cuts (no known rate, or renamed
// files that restic reads in full) makes the next batches smaller, so the job progresses in the
// next window instead of running the same batch into the window's end forever; the cut cap lives
// in the destination's state, so a superseding job keeps it (§9.2).
func TestResticCutBatchShrinks(t *testing.T) {
	for _, moves := range []bool{false, true} {
		t.Run(map[bool]string{false: "copies", true: "moves"}[moves], func(t *testing.T) {
			h := newHarness(t, harnessOptions{kind: engines.Restic, bandwidth: nightWindow(false, 0)})
			h.setClock(time.Date(2026, 9, 28, 2, 0, 0, 0, time.UTC))
			names := []string{"a.mkv", "b.mkv", "c.mkv", "d.mkv"}
			for i, n := range names {
				h.writeSrc("in/"+n, content(n, 1000+i), i+1)
			}
			if moves {
				h.mustSync(jobs.Params{})
				for _, n := range names {
					h.renameSrc("in/"+n, "renamed/"+n)
				}
			}
			h.restic.hangNewOver = 2
			h.setClock(time.Date(2026, 9, 28, 6, 59, 59, 950_000_000, time.UTC))
			j := h.newJob(jobs.TypeSync, false, jobs.Params{AllowChanges: true})
			_, err := h.runJob(j)
			mustDefer(t, err)
			h.closeJob(j.ID) // superseded by the next scheduled run
			h.setClock(time.Date(2026, 9, 29, 6, 59, 59, 950_000_000, time.UTC))
			j2 := h.newJob(jobs.TypeSync, false, jobs.Params{AllowChanges: true})
			if _, err := h.runJob(j2); err != nil {
				t.Fatalf("the next window: %v\n%s", err, h.rep.dump())
			}
			h.closeJob(j2.ID)
			for _, it := range h.items(j2.ID) {
				if it.Status != jobs.ItemDone {
					t.Errorf("%s %s: %s %q", it.Action, it.RelPath, it.Status, it.Error)
				}
			}
		})
	}
}
