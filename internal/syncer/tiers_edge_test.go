package syncer

import (
	"os"
	"slices"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/tiers"
)

// movedFlags returns the moves the fake engine's folder flags followed for the harness's source.
func (f *fakeTiers) movedFlags(sourceID int64) []tiers.Move {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Compact(slices.Clone(f.afterSync[sourceID]))
}

// crashAfterMove renames Old/c.mkv to New/c.mkv and runs a sync that crashes after the move is
// done (at the copy of another new file); it returns the crashed job.
func crashAfterMove(t *testing.T, h *harness) jobs.Job {
	t.Helper()
	h.writeSrc("Old/c.mkv", content("c", 500), 1)
	h.mustSync(false, jobs.Params{})
	h.renameSrc("Old/c.mkv", "New/c.mkv")
	h.writeSrc("z.mkv", content("z", 700), 3)
	j := h.newJob(jobs.TypeSync, false, jobs.Params{})
	if crashed, _, _ := runWithHook(h, j, faultinject.CrashAt("copy.beforeTemp", 1)); !crashed {
		t.Fatal("no crash")
	}
	if done := itemsBy(h.items(j.ID), jobs.ActionMove, jobs.ItemDone); len(done) != 1 {
		t.Fatalf("the move was not done before the crash: %v", h.itemList(j.ID))
	}
	return j
}

var wantFollowed = []tiers.Move{{From: "Old/c.mkv", To: "New/c.mkv"}}

// TestTiersFolderFlagsFollowResumeThatFailsEarly: a resumed attempt that fails before it executes
// (after a reboot the source's share is not mounted yet) still lets the folder flags follow the
// moves the crashed attempt executed: the failed job never runs again, and no later sync replays
// them.
func TestTiersFolderFlagsFollowResumeThatFailsEarly(t *testing.T) {
	h := newHarness(t)
	ft := newFakeTiers(h)
	h.withTiers(ft)
	j := crashAfterMove(t, h)
	ft.mu.Lock()
	ft.afterSync = map[int64][]tiers.Move{} // a real crash (SIGKILL) runs no defer
	ft.mu.Unlock()
	away := h.srcDir + ".away"
	if err := os.Rename(h.srcDir, away); err != nil {
		t.Fatal(err)
	}
	j.Attempt, j.Trigger = 2, jobs.TriggerResume
	if _, err := h.sync.Run(h.ctx, j, h.env()); err == nil {
		t.Fatal("the resume did not fail")
	}
	if got := ft.movedFlags(h.src.ID); !slices.Equal(got, wantFollowed) {
		t.Fatalf("the folder flags followed %v, want %v", got, wantFollowed)
	}
}

// TestTiersFolderFlagsFollowJobsThatNeverResume: a crashed sync cancelled while it waits in the
// queue for its resume never runs again; the job manager's OnFinish hook (registered by
// NewSyncRunner) lets the folder flags follow its moves. Crash recovery failing it after too many
// crashes ends the same way (failed). Completed jobs and dry runs are left alone.
func TestTiersFolderFlagsFollowJobsThatNeverResume(t *testing.T) {
	h := newHarness(t)
	ft := newFakeTiers(h)
	m := jobqueue.New(h.db, nil, jobqueue.Options{})
	o := Options{DB: h.db, Store: h.store, Catalog: h.cat, Scanner: h.scanner, Destinations: h.dests, Now: h.now, Tiers: ft, Enqueuer: m}
	h.sync = NewSyncRunner(o)
	h.sync.planBatch = 3
	j := crashAfterMove(t, h)
	ft.mu.Lock()
	ft.afterSync = map[int64][]tiers.Move{} // the crashed attempt never reached its follow-up
	ft.mu.Unlock()

	// The job is still queued (the harness runs jobs without a manager): cancel it there.
	if _, err := m.Cancel(h.ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(ft.movedFlags(h.src.ID)) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := ft.movedFlags(h.src.ID); !slices.Equal(got, wantFollowed) {
		t.Fatalf("after the cancel the folder flags followed %v, want %v", got, wantFollowed)
	}

	for _, c := range []struct {
		name   string
		status jobs.Status
		dryRun bool
		typ    jobs.Type
		want   bool
	}{
		{"failed by crash recovery", jobs.StatusFailed, false, jobs.TypeSync, true},
		{"completed", jobs.StatusCompleted, false, jobs.TypeSync, false},
		{"dry run", jobs.StatusFailed, true, jobs.TypeSync, false},
		{"another job type", jobs.StatusFailed, false, jobs.TypeVerify, false},
	} {
		ft.mu.Lock()
		ft.afterSync = map[int64][]tiers.Move{}
		ft.mu.Unlock()
		fj := j
		fj.Status, fj.DryRun, fj.Type = c.status, c.dryRun, c.typ
		h.sync.OnJobFinish(fj)
		if got := len(ft.movedFlags(h.src.ID)) > 0; got != c.want {
			t.Errorf("%s: flags followed %v, want %v", c.name, got, c.want)
		}
	}
}

// TestTiersFolderFlagsFollowRecordedMoveOfPendingItem: the process died after a move's record was
// committed but before its item was finished (the item stays pending). A job that then never
// executes again, cancelled while queued or a resume that fails before it executes, still lets
// the folder flags follow that move: no later sync replays it (its record has moved). A move
// whose record did not move is not followed.
func TestTiersFolderFlagsFollowRecordedMoveOfPendingItem(t *testing.T) {
	for _, c := range []struct {
		name  string
		point string
		want  []tiers.Move
	}{
		{"record committed", PointRecordAfterDB, wantFollowed},
		{"record not committed", PointRecordAfterFS, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			ft := newFakeTiers(h)
			h.withTiers(ft)
			h.writeSrc("Old/c.mkv", content("c", 500), 1)
			h.mustSync(false, jobs.Params{})
			h.renameSrc("Old/c.mkv", "New/c.mkv")
			j := h.newJob(jobs.TypeSync, false, jobs.Params{})
			if crashed, _, _ := runWithHook(h, j, faultinject.CrashAt(c.point, 1)); !crashed {
				t.Fatal("no crash")
			}
			if pending := itemsBy(h.items(j.ID), jobs.ActionMove, jobs.ItemPending); len(pending) != 1 {
				t.Fatalf("the move's item is not pending: %v", h.itemList(j.ID))
			}
			reset := func() {
				ft.mu.Lock()
				ft.afterSync = map[int64][]tiers.Move{} // a real crash runs no defer
				ft.mu.Unlock()
			}

			// Cancelled while queued for its resume.
			reset()
			fj := j
			fj.Status = jobs.StatusCancelled
			h.sync.OnJobFinish(fj)
			if got := ft.movedFlags(h.src.ID); !slices.Equal(got, c.want) {
				t.Fatalf("after the cancel the folder flags followed %v, want %v", got, c.want)
			}

			// A resume that fails before it executes (the source is not mounted yet).
			reset()
			if err := os.Rename(h.srcDir, h.srcDir+".away"); err != nil {
				t.Fatal(err)
			}
			j.Attempt, j.Trigger = 2, jobs.TriggerResume
			if _, err := h.sync.Run(h.ctx, j, h.env()); err == nil {
				t.Fatal("the resume did not fail")
			}
			if got := ft.movedFlags(h.src.ID); !slices.Equal(got, c.want) {
				t.Fatalf("after the failed resume the folder flags followed %v, want %v", got, c.want)
			}
		})
	}
}

// TestTiersUnknownRelinkHeld: the hardlink made at the destination of an unknown-promoted primary
// that verify marked missing is relinked after the primary's repair; when that repair is held
// (it does not fit in the free space), the relink is held with it instead of falling back to a
// copy of the same content.
func TestTiersUnknownRelinkHeld(t *testing.T) {
	h := newHarness(t)
	crashConfigs[0].apply(h) // hardlinks are made at the destination
	ft := newFakeTiers(h)
	h.withTiers(ft)
	prim := unknownLinkedPair(t, h, ft)
	h.writeSrc("new.mkv", content("n", 300), 3)
	h.sync.freeSpace = func(*os.Root) (uint64, uint64, error) { return 1<<30 + 1000, 1 << 40, nil }
	_, st, j := h.mustSync(false, jobs.Params{})
	assertRelinkHeld(t, h, j.ID, prim)
	if st.BytesCopied > 1000 {
		t.Fatalf("copied %d bytes with 1000 available: %v", st.BytesCopied, h.itemList(j.ID))
	}
}

// TestTiersUnknownRelinkHeldOnResume: an attempt that held the unknown-promoted repair and stopped
// before holding its relink: the resumed attempt holds the relink.
func TestTiersUnknownRelinkHeldOnResume(t *testing.T) {
	h := newHarness(t)
	crashConfigs[0].apply(h)
	ft := newFakeTiers(h)
	h.withTiers(ft)
	prim := unknownLinkedPair(t, h, ft)
	h.writeSrc("new.mkv", content("n", 300), 3)
	j := h.newJob(jobs.TypeSync, false, jobs.Params{})
	if crashed, _, _ := runWithHook(h, j, faultinject.CrashAt("copy.beforeTemp", 1)); !crashed {
		t.Fatal("no crash")
	}
	held := false
	for _, it := range h.items(j.ID) {
		if it.Action == jobs.ActionCopy && it.RelPath == prim.RelPath && it.Status == jobs.ItemPending {
			if err := h.jq.Finish(h.ctx, it.ID, jobs.ItemHeld, 0, "held: tier unknown because the cache is old; not enough free space"); err != nil {
				t.Fatal(err)
			}
			held = true
		}
	}
	if !held {
		t.Fatalf("no pending repair of the primary: %v", h.itemList(j.ID))
	}
	h.sync.freeSpace = func(*os.Root) (uint64, uint64, error) { return 1<<30 + 1000, 1 << 40, nil }
	j.Attempt, j.Trigger = 2, jobs.TriggerResume
	if _, err := h.sync.Run(h.ctx, j, h.env()); err != nil {
		t.Fatal(err)
	}
	h.closeJob(j.ID)
	assertRelinkHeld(t, h, j.ID, prim)
}

// TestTiersDecidedLinkOfHeldUnknownPrimary: a new hardlinked pair whose primary (the group's first
// name, a/x.mkv) is unknown-promoted and whose other name (b/x.mkv) is decided full. The
// unknown-promoted copies do not fit in the free space; they are held and the rest runs (§8.5):
// the decided-full name is backed up (its link falls back to a copy of the content, which fits).
// Also when the hold happens on a resume (recheckUnknownHold).
func TestTiersDecidedLinkOfHeldUnknownPrimary(t *testing.T) {
	for _, c := range []struct {
		name   string
		cfg    crashConfig
		resume bool
	}{
		{"recreate", crashConfigs[0], false},
		{"copy", crashConfigs[1], false},
		{"recreate/resume", crashConfigs[0], true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			c.cfg.apply(h)
			ft := newFakeTiers(h)
			h.withTiers(ft)
			h.writeSrc("0.mkv", content("d", 300), 1) // decided full; the first copy
			h.writeSrc("a/x.mkv", content("x", 5000), 2)
			h.linkSrc("a/x.mkv", "b/x.mkv")
			h.writeSrc("u2.mkv", content("u2", 3000), 3)
			ft.setUnknown("a/x.mkv")
			ft.setUnknown("u2.mkv")
			lowSpace := func(*os.Root) (uint64, uint64, error) { return 1<<30 + 5500, 1 << 40, nil }
			j := h.newJob(jobs.TypeSync, false, jobs.Params{})
			if c.resume {
				// The planning attempt had room and stopped at its first copy (0.mkv); the resume
				// finds less free space.
				if crashed, _, _ := runWithHook(h, j, faultinject.CrashAt("copy.beforeTemp", 1)); !crashed {
					t.Fatal("no crash")
				}
				h.sync.freeSpace = lowSpace
				j.Attempt, j.Trigger = 2, jobs.TriggerResume
			} else {
				h.sync.freeSpace = lowSpace
			}
			if _, err := h.sync.Run(h.ctx, j, h.env()); err != nil {
				t.Fatal(err)
			}
			h.closeJob(j.ID)
			items := h.items(j.ID)
			if held := itemsBy(items, jobs.ActionCopy, jobs.ItemHeld); !slices.Equal(held, []string{"movies/a/x.mkv", "movies/u2.mkv"}) {
				t.Fatalf("the unknown-promoted copies were not held: %v", h.itemList(j.ID))
			}
			for _, it := range items {
				if it.RelPath == "movies/b/x.mkv" && it.Status != jobs.ItemDone {
					t.Fatalf("the decided-full name's item is %s (%s): %v", it.Status, it.Error, h.itemList(j.ID))
				}
			}
			if _, ok := h.liveRecord("movies/b/x.mkv"); !ok || !exists(h.dstPath("movies/b/x.mkv")) {
				t.Fatalf("the decided-full name b/x.mkv is not backed up: %v", h.itemList(j.ID))
			}
			if _, ok := h.liveRecord("movies/0.mkv"); !ok {
				t.Fatalf("the decided copy did not run: %v", h.itemList(j.ID))
			}
		})
	}
}

// unknownLinkedPair backs up u/a.mkv and its hardlink u/b.mkv (a hardlink at the destination),
// makes both unknown-promoted and marks the primary's damaged file missing; it returns the
// primary's record.
func unknownLinkedPair(t *testing.T, h *harness, ft *fakeTiers) Record {
	t.Helper()
	h.writeSrc("decided.mkv", content("d", 300), 1)
	h.writeSrc("u/a.mkv", content("u", 5000), 2)
	h.linkSrc("u/a.mkv", "u/b.mkv")
	h.mustSync(false, jobs.Params{})
	var prim, link Record
	for _, r := range h.records() {
		switch {
		case r.State == StatePresent && r.SourceRelPath != "decided.mkv":
			prim = r
		case r.State == StateLinked:
			link = r
		}
	}
	if prim.ID == 0 || link.ID == 0 || link.LinkOf != prim.ID {
		t.Fatalf("records %+v", h.records())
	}
	ft.setUnknown("u/a.mkv")
	ft.setUnknown("u/b.mkv")
	if err := os.Remove(h.dstPath(prim.RelPath)); err != nil {
		t.Fatal(err)
	}
	h.dbExec(`UPDATE destination_files SET state = 'missing' WHERE id = ?`, prim.ID)
	return prim
}

func assertRelinkHeld(t *testing.T, h *harness, jobID int64, prim Record) {
	t.Helper()
	var copyHeld, linkHeld bool
	for _, it := range h.items(jobID) {
		d := detailOf(t, it)
		switch it.Action {
		case jobs.ActionCopy:
			if it.RelPath == prim.RelPath {
				copyHeld = it.Status == jobs.ItemHeld
			}
		case jobs.ActionLink:
			if d.Outcome == outcomeFallbackCopy {
				t.Fatalf("the relink of a held repair copied the content: %s (%s)", it.RelPath, it.Status)
			}
			linkHeld = it.Status == jobs.ItemHeld
		}
	}
	if !copyHeld || !linkHeld {
		t.Fatalf("repair held %v, relink held %v: %v", copyHeld, linkHeld, h.itemList(jobID))
	}
}

// TestTiersMissingFullPrimaryLosesKeptLink: a full primary that verify marked missing, whose
// source got new content, with a kept (not full) recorded-only link of the old content: the link
// gets no copy (S15) and its content went with the damaged file, so it no longer blocks the
// primary's repair; its record becomes missing (nothing is deleted) and it is copied again once
// it is full.
func TestTiersMissingFullPrimaryLosesKeptLink(t *testing.T) {
	h := newHarness(t)
	crashConfigs[1].apply(h) // hardlinks are recorded, not made
	ft := newFakeTiers(h)
	h.withTiers(ft)
	h.writeSrc("dl/x.mkv", content("x", 2000), 1)
	h.linkSrc("dl/x.mkv", "lib/x.mkv")
	h.mustSync(false, jobs.Params{})
	var prim, link Record
	for _, r := range h.records() {
		switch r.State {
		case StatePresent:
			prim = r
		case StateLinkRecorded:
			link = r
		}
	}
	if prim.ID == 0 || link.ID == 0 || link.LinkOf != prim.ID {
		t.Fatalf("records %+v", h.records())
	}
	// The primary's name gets a new file (the group splits); the link keeps the old content and
	// becomes manifest; verify found the primary's destination file damaged.
	h.removeSrc(prim.SourceRelPath)
	h.writeSrc(prim.SourceRelPath, content("y", 2500), 5)
	ft.set(link.SourceRelPath, tiers.Manifest)
	if err := os.Remove(h.dstPath(prim.RelPath)); err != nil {
		t.Fatal(err)
	}
	h.dbExec(`UPDATE destination_files SET state = 'missing' WHERE id = ?`, prim.ID)

	_, st, j := h.mustSync(false, jobs.Params{})
	if st.FilesCopied != 1 || st.FilesFailed != 0 {
		t.Fatalf("stats %+v items %v", st, h.itemList(j.ID))
	}
	if r, _ := h.liveRecord(prim.RelPath); r.State != StatePresent || r.Size != 2500 || !exists(h.dstPath(prim.RelPath)) {
		t.Fatalf("the full primary was not repaired: %+v", r)
	}
	r, ok := h.liveRecord(link.RelPath)
	if !ok || r.State != StateMissing || r.LinkOf != 0 || r.Size != link.Size {
		t.Fatalf("the kept link's record: %+v", r)
	}
	if _, st, j := h.mustSync(false, jobs.Params{}); st.FilesPlanned != 0 {
		t.Fatalf("a further sync planned %v", h.itemList(j.ID))
	}
	// Full again: it gets its own copy.
	ft.set(link.SourceRelPath, tiers.Full)
	if _, st, j := h.mustSync(false, jobs.Params{}); st.FilesCopied != 1 {
		t.Fatalf("stats %+v items %v", st, h.itemList(j.ID))
	}
	if r, _ := h.liveRecord(link.RelPath); r.State != StatePresent || !exists(h.dstPath(link.RelPath)) {
		t.Fatalf("the link was not copied once full: %+v", r)
	}
}
