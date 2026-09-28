package enginerun

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/bwlimit"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/syncer"
	"github.com/sl0wz3r/bunkarr/internal/tiers"
)

// fakeTiers is a syncer.Tiers whose only behaviour is the irreplaceable-flag coverage (the
// retention hold); the planner of the tests has no tier engine.
type fakeTiers struct {
	covered map[string]bool
	// onCoverage runs at every Coverage call.
	onCoverage func()
}

func (f *fakeTiers) Decisions(context.Context, tiers.Queryer, int64, catalog.Source) (*tiers.SourceDecisions, error) {
	return nil, nil
}
func (f *fakeTiers) Revision(context.Context) (int64, error)                           { return 0, nil }
func (f *fakeTiers) MoveFlagsTx(context.Context, *sql.Tx, int64, string, string) error { return nil }
func (f *fakeTiers) FlagsAfterSync(context.Context, int64, []tiers.Move) error         { return nil }
func (f *fakeTiers) Coverage(context.Context) (func(int64, string) (int64, bool), error) {
	if f.onCoverage != nil {
		f.onCoverage()
	}
	return func(_ int64, rel string) (int64, bool) { return 1, f.covered[rel] }, nil
}

// withRetentionTiers gives the Service (not the planner) a tier engine with coverage.
func (h *harness) withRetentionTiers(covered ...string) *fakeTiers {
	ft := &fakeTiers{covered: map[string]bool{}}
	for _, c := range covered {
		ft.covered[c] = true
	}
	h.svc.o.Tiers = ft
	return ft
}

func (h *harness) insertRetained(v syncer.RetainedVersion) int64 {
	h.t.Helper()
	var id int64
	err := h.db.Write(h.ctx, func(tx *sql.Tx) error {
		var err error
		id, err = h.files.InsertRetainedTx(h.ctx, tx, v)
		return err
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

// TestRcloneRetentionExpiry: expired retained objects are deleted in a fenced batch with
// --max-delete (§7.5, S23), with the holds: the irreplaceable flag, the replaced-version hold
// (the live record of the path is not present), the size check, and a retained path outside
// .bunkarr/retention/ never reaches a command.
func TestRcloneRetentionExpiry(t *testing.T) {
	h := newRclone(t)
	for _, f := range []string{"del.mkv", "flag.mkv", "upd.mkv", "size.mkv", "keep.mkv"} {
		h.writeSrc(f, content(f, 1000), 1)
	}
	h.mustSync(jobs.Params{})
	h.removeSrc("del.mkv")
	h.removeSrc("flag.mkv")
	h.removeSrc("size.mkv")
	h.writeSrc("upd.mkv", content("U", 1100), 2)
	h.advance(time1h)
	h.mustSync(jobs.Params{})
	// The update's new version turns missing: its old version is held.
	h.exec(`UPDATE destination_files SET state = 'missing' WHERE id = ?`, h.mustLive(h.dp("upd.mkv")).ID)
	byPath := map[string]syncer.Record{}
	for _, r := range h.retained() {
		byPath[r.SourceRelPath] = r
	}
	h.rclone.put(byPath["size.mkv"].RetainedPath, []byte("not the retained version"), h.now())
	outside := h.insertRetained(syncer.RetainedVersion{DestinationID: h.dest.ID, SourceID: h.src.ID, RelPath: h.dp("x.mkv"),
		SourceRelPath: "x.mkv", Size: 10, RetainedPath: h.dp("x.mkv"), Reason: syncer.ReasonDeleted, RetainedAt: h.now(),
		ExpiresAt: h.now()})
	h.withRetentionTiers("flag.mkv")
	h.advance(31 * 24 * time.Hour)
	res, j := h.mustRun(jobs.TypeRetention, false, jobs.Params{})
	st := res.Stats.(RetentionStats)
	if st.FilesExpired != 1 || st.FilesHeld != 2 {
		t.Errorf("stats %+v\n%s", st, h.rep.dump())
	}
	if _, ok := h.rclone.get(byPath["del.mkv"].RetainedPath); ok {
		t.Error("the expired object is still there")
	}
	for _, rel := range []string{"flag.mkv", "upd.mkv", "size.mkv"} {
		if _, ok := h.rclone.get(byPath[rel].RetainedPath); !ok {
			t.Errorf("%s's retained object was deleted", rel)
		}
	}
	items := h.items(j.ID)
	for _, it := range items {
		switch {
		case strings.HasSuffix(it.RelPath, "upd.mkv") && (it.Status != jobs.ItemHeld || it.Error != holdReplaced):
			t.Errorf("upd.mkv: %s %q", it.Status, it.Error)
		case strings.HasSuffix(it.RelPath, "flag.mkv") && (it.Status != jobs.ItemHeld || it.Error != holdIrreplaceable):
			t.Errorf("flag.mkv: %s %q", it.Status, it.Error)
		case strings.HasSuffix(it.RelPath, "size.mkv") && it.Status != jobs.ItemSkipped:
			t.Errorf("size.mkv: %s", it.Status)
		case it.FileID == outside && it.Status != jobs.ItemSkipped:
			t.Errorf("a retained path outside retention: %s", it.Status)
		}
	}
	for _, c := range h.callsOf(proc.Rclone, "delete") {
		if !c.Has("--max-delete") {
			t.Errorf("delete without --max-delete: %s", c)
		}
		for _, l := range h.rclone.deleteLists {
			for _, f := range l {
				if strings.Contains(f, "x.mkv") && !strings.Contains(f, "retention") {
					t.Errorf("a path outside retention reached delete: %s", f)
				}
			}
		}
	}
	if len(h.callsOf(proc.Rclone, "deletefile"))+len(h.callsOf(proc.Rclone, "purge")) != 0 {
		t.Error("other deleting commands ran")
	}
}

// TestRcloneCleanup: unfinished multipart uploads are removed at most once a day, scoped to the
// destination's own bucket and prefix, with max-age at least 7 days and twice the longest
// expected upload at the lowest rate in force (§7.5).
func TestRcloneCleanup(t *testing.T) {
	h := newRclone(t)
	h.writeSrc("big.mkv", content("b", 4<<20), 1)
	h.mustSync(jobs.Params{})
	h.mustRun(jobs.TypeRetention, false, jobs.Params{})
	if len(h.rclone.cleanups) != 1 {
		t.Fatalf("cleanups %v", h.rclone.cleanups)
	}
	if got := h.rclone.cleanups[0]; !strings.Contains(got, "BKDEST:bunkarr-bk/media -o max-age=168h0m0s") {
		t.Errorf("cleanup %q", got)
	}
	h.advance(time1h)
	h.mustRun(jobs.TypeRetention, false, jobs.Params{})
	if len(h.rclone.cleanups) != 1 {
		t.Errorf("a second cleanup the same day: %v", h.rclone.cleanups)
	}
	// A 1 KiB/s limit: 4 MiB takes about 68 minutes, well below 7 days.
	bw := h.dest.Bandwidth
	bw.UploadKiBps = 1
	h.update(destinations.Input{Bandwidth: &bw})
	h.advance(25 * time.Hour)
	h.mustRun(jobs.TypeRetention, false, jobs.Params{})
	if len(h.rclone.cleanups) != 2 || !strings.Contains(h.rclone.cleanups[1], "max-age=168h0m0s") {
		t.Errorf("cleanups %v", h.rclone.cleanups)
	}
	// A huge file at a tiny rate raises the age above 7 days: twice its time at the rate.
	f, err := os.Create(h.srcPath("huge.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1 << 30); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if _, err := h.scanner.Scan(h.ctx, h.src.ID, h.rep); err != nil {
		t.Fatal(err)
	}
	x := &rcloneRun{jobRun: &jobRun{s: h.svc, d: h.dest}}
	age, err := x.cleanupAge(h.ctx, EngineState{})
	if want := 2 * (1 << 30) / 1024 * time.Second; err != nil || age < want || age > want+2*time.Hour {
		t.Errorf("cleanup age %s (%v), want about %s", age, err, want)
	}
}

// newEngineHarness is a harness of kind with deletedDays 30 (restic keeps one daily snapshot).
func newEngineHarness(t *testing.T, kind engines.Kind, bw *bwlimit.Config) *harness {
	if kind == engines.Restic {
		return newHarness(t, harnessOptions{kind: kind, bandwidth: bw, retention: &destinations.Retention{DeletedDays: 30,
			SnapshotDaily: intp(1), SnapshotWeekly: intp(0), SnapshotMonthly: intp(0), SnapshotYearly: intp(0)}})
	}
	return newHarness(t, harnessOptions{kind: kind, bandwidth: bw})
}

// TestReplacedHoldAfterDelete: a replaced version whose path has no live record any more is held
// only while no newer version of the path was recorded: updated and then deleted, both versions
// expire in turn (§6.5, §7.5); the newest version of a path whose successor never arrived stays.
func TestReplacedHoldAfterDelete(t *testing.T) {
	for _, kind := range []engines.Kind{engines.Restic, engines.Rclone} {
		t.Run(string(kind), func(t *testing.T) {
			h := newEngineHarness(t, kind, nil)
			h.writeSrc("keep.mkv", content("k", 900), 9)
			h.writeSrc("upd.mkv", content("u1", 1000), 1)
			h.mustSync(jobs.Params{})
			h.writeSrc("upd.mkv", content("u2", 1100), 2)
			h.advance(time1h)
			h.mustSync(jobs.Params{})
			h.removeSrc("upd.mkv")
			h.advance(24 * time.Hour)
			h.mustSync(jobs.Params{})
			if n := len(h.retained()); n != 2 {
				t.Fatalf("retained rows %+v", h.retained())
			}
			h.advance(400 * 24 * time.Hour)
			res, _ := h.mustRun(jobs.TypeRetention, false, jobs.Params{})
			if st := res.Stats.(RetentionStats); st.FilesExpired != 2 || st.FilesHeld != 0 {
				t.Errorf("stats %+v\n%s", st, h.rep.dump())
			}
			if left := h.retained(); len(left) != 0 {
				t.Errorf("retained rows left %+v", left)
			}
		})
	}
	t.Run("successor never arrived", func(t *testing.T) {
		h := newEngineHarness(t, engines.Rclone, nightWindow(false, 0))
		h.setClock(time.Date(2026, 9, 28, 2, 0, 0, 0, time.UTC))
		h.writeSrc("keep.mkv", content("k", 900), 9)
		h.writeSrc("a.mkv", content("a1", 2000), 1)
		h.mustSync(jobs.Params{})
		h.writeSrc("a.mkv", content("a2", 2200), 5)
		h.rclone.cutAfterBackup["a.mkv"] = true
		h.advance(time1h)
		j := h.newJob(jobs.TypeSync, false, jobs.Params{})
		_, _ = h.runJob(j)
		h.closeJob(j.ID)
		delete(h.rclone.cutAfterBackup, "a.mkv")
		h.removeSrc("a.mkv")
		h.advance(time1h)
		h.mustSync(jobs.Params{})
		if _, ok := h.live(h.dp("a.mkv")); ok {
			t.Fatalf("a live record is left: %+v", h.records())
		}
		h.setClock(time.Date(2027, 11, 2, 2, 0, 0, 0, time.UTC))
		res, _ := h.mustRun(jobs.TypeRetention, false, jobs.Params{})
		if st := res.Stats.(RetentionStats); st.FilesExpired != 0 || st.FilesHeld != 1 {
			t.Errorf("stats %+v\n%s", st, h.rep.dump())
		}
	})
}

// TestReplacedHoldAfterRename: a replaced version follows its live record when the file is
// renamed, so the replaced-version hold ends once the new version is present under its new name
// (§6.5, §7.5); before, the old path had neither a live record nor a newer version, and the row
// (with, on restic, the snapshot it references) was held forever.
func TestReplacedHoldAfterRename(t *testing.T) {
	for _, kind := range []engines.Kind{engines.Restic, engines.Rclone} {
		t.Run(string(kind), func(t *testing.T) {
			h := newEngineHarness(t, kind, nil)
			h.writeSrc("keep.mkv", content("k", 900), 9)
			h.writeSrc("upd.mkv", content("u1", 1000), 1)
			h.mustSync(jobs.Params{})
			h.writeSrc("upd.mkv", content("u2", 1100), 2)
			h.advance(time1h)
			h.mustSync(jobs.Params{})
			h.renameSrc("upd.mkv", "renamed.mkv")
			h.advance(24 * time.Hour)
			st, _ := h.mustSync(jobs.Params{})
			if st.FilesMoved != 1 {
				t.Fatalf("the rename is no move: %+v", st)
			}
			ret := h.retained()
			if len(ret) != 1 || ret[0].RelPath != h.dp("renamed.mkv") || ret[0].SourceRelPath != "renamed.mkv" || ret[0].Reason != syncer.ReasonReplaced {
				t.Fatalf("the replaced version did not follow its live record: %+v", ret)
			}
			h.advance(400 * 24 * time.Hour)
			res, _ := h.mustRun(jobs.TypeRetention, false, jobs.Params{})
			if st := res.Stats.(RetentionStats); st.FilesExpired != 1 || st.FilesHeld != 0 {
				t.Errorf("stats %+v\n%s", st, h.rep.dump())
			}
			if left := h.retained(); len(left) != 0 {
				t.Errorf("retained rows left %+v", left)
			}
			if r := h.mustLive(h.dp("renamed.mkv")); r.State != syncer.StatePresent || r.Size != 1100 {
				t.Errorf("the live record %+v", r)
			}
		})
	}
}

// TestRetentionKeepsOrphans: the retained rows of a source unlinked from the destination are
// orphans and never expire (phase1.md S5, as the filecopy runner).
func TestRetentionKeepsOrphans(t *testing.T) {
	for _, kind := range []engines.Kind{engines.Restic, engines.Rclone} {
		t.Run(string(kind), func(t *testing.T) {
			h := newEngineHarness(t, kind, nil)
			h.writeSrc("keep.mkv", content("k", 900), 1)
			h.writeSrc("del.mkv", content("d", 1000), 2)
			h.mustSync(jobs.Params{})
			h.removeSrc("del.mkv")
			h.advance(time1h)
			h.mustSync(jobs.Params{})
			ret := h.retained()
			if len(ret) != 1 {
				t.Fatalf("retained %+v", ret)
			}
			h.update(destinations.Input{SourceIDs: []int64{}})
			h.advance(40 * 24 * time.Hour)
			res, _ := h.mustRun(jobs.TypeRetention, false, jobs.Params{})
			if st := res.Stats.(RetentionStats); st.FilesExpired != 0 {
				t.Errorf("stats %+v", st)
			}
			if left := h.retained(); len(left) != 1 {
				t.Errorf("the orphan row expired: %+v", left)
			}
			if kind == engines.Rclone {
				if _, ok := h.rclone.get(ret[0].RetainedPath); !ok {
					t.Error("the orphan object was deleted")
				}
			}
		})
	}
}

// TestRetentionHoldsAtExecution: a retention job's plan is checked again when an item runs: a
// file flagged irreplaceable after the plan was made (the job deferred at the window's end) is
// held when the resumed job reaches it.
func TestRetentionHoldsAtExecution(t *testing.T) {
	for _, kind := range []engines.Kind{engines.Restic, engines.Rclone} {
		t.Run(string(kind), func(t *testing.T) {
			h := newEngineHarness(t, kind, nightWindow(false, 0))
			h.setClock(time.Date(2026, 9, 28, 2, 0, 0, 0, time.UTC))
			h.writeSrc("keep.mkv", content("k", 900), 1)
			h.writeSrc("del.mkv", content("d", 1000), 2)
			h.mustSync(jobs.Params{})
			h.removeSrc("del.mkv")
			h.advance(time1h)
			h.mustSync(jobs.Params{})
			ret := h.retained()
			if len(ret) != 1 {
				t.Fatalf("retained %+v", ret)
			}
			ft := h.withRetentionTiers()
			planning := true
			ft.onCoverage = func() {
				if planning {
					planning = false
					h.setClock(time.Date(2026, 11, 10, 7, 30, 0, 0, time.UTC)) // the window closes after the plan
				}
			}
			h.setClock(time.Date(2026, 11, 10, 6, 0, 0, 0, time.UTC))
			j := h.newJob(jobs.TypeRetention, false, jobs.Params{})
			_, err := h.runJob(j)
			mustDefer(t, err)
			ft.covered["del.mkv"] = true
			h.setClock(time.Date(2026, 11, 11, 1, 30, 0, 0, time.UTC))
			j.Deferrals = 1
			res, err := h.runJob(j)
			h.closeJob(j.ID)
			if err != nil {
				t.Fatalf("resumed retention: %v", err)
			}
			if st := res.Stats.(RetentionStats); st.FilesExpired != 0 || st.FilesHeld != 1 {
				t.Errorf("stats %+v", st)
			}
			if left := h.retained(); len(left) != 1 {
				t.Errorf("the flagged row expired: %+v", left)
			}
			if kind == engines.Rclone {
				if _, ok := h.rclone.get(ret[0].RetainedPath); !ok {
					t.Error("the flagged object was deleted")
				}
			}
		})
	}
}
