package syncer

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

func (h *harness) runRetention() (jobs.Result, RetentionStats, jobs.Job, error) {
	h.t.Helper()
	j := h.newJob(jobs.TypeRetention, false, jobs.Params{DestinationID: h.dest.ID})
	res, err := h.run(h.retention, j)
	st, _ := res.Stats.(RetentionStats)
	return res, st, j, err
}

// retainedFixture syncs three files and deletes them at the source, so they are retained.
func retainedFixture(t *testing.T) *harness {
	h := newHarness(t)
	h.writeSrc("a.mkv", content("a", 300), 1)
	h.writeSrc("d/b.mkv", content("b", 400), 2)
	h.writeSrc("d/e/c.mkv", content("c", 500), 3)
	h.writeSrc("keep.mkv", content("k", 50), 4)
	h.mustSync(false, jobs.Params{})
	h.removeSrc("a.mkv")
	h.removeSrc("d/b.mkv")
	h.removeSrc("d/e/c.mkv")
	if _, st, _ := h.mustSync(false, jobs.Params{}); st.FilesRetained != 3 {
		t.Fatalf("retained %d", st.FilesRetained)
	}
	return h
}

func retainedRecords(h *harness) []Record {
	var out []Record
	for _, r := range h.records() {
		if r.State == StateRetained {
			out = append(out, r)
		}
	}
	return out
}

func TestRetentionExpiryFence(t *testing.T) {
	h := retainedFixture(t)
	// Before the retention period ends nothing is expired.
	h.advance(29 * 24 * time.Hour)
	_, st, _, err := h.runRetention()
	if err != nil || st.FilesPlanned != 0 {
		t.Fatalf("early: %+v %v", st, err)
	}
	if len(retainedRecords(h)) != 3 {
		t.Fatal("retained rows disappeared early")
	}
	// After it, every retained file is deleted with its row, and the empty directories go.
	h.advance(2 * 24 * time.Hour)
	res, st, _, err := h.runRetention()
	if err != nil || st.FilesExpired != 3 || st.BytesExpired != 1200 || res.Warnings != 0 {
		t.Fatalf("expire: %+v %v %s", st, err, h.rep.dump())
	}
	if len(retainedRecords(h)) != 0 {
		t.Fatal("rows left")
	}
	if entries, err := os.ReadDir(h.dstPath(filecopy.RetentionRoot)); err != nil || len(entries) != 0 {
		t.Fatalf("retention tree not pruned: %v %v", entries, err)
	}
	h.assertConverged(t)
	if !exists(h.dstPath("movies/keep.mkv")) {
		t.Fatal("a live file was touched")
	}
}

func TestRetentionKeepsWhatItMustNotDelete(t *testing.T) {
	h := retainedFixture(t)
	recs := retainedRecords(h)
	// 1. A retained file whose size changed is not the file that was retained.
	if err := os.WriteFile(h.dstPath(recs[0].RetainedPath), []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 2. A live record that still links to retained content keeps it.
	live, _ := h.liveRecord("movies/keep.mkv")
	h.dbExec(`UPDATE destination_files SET state = 'link_recorded', link_of = ? WHERE id = ?`, recs[1].ID, live.ID)
	// 3. A row pointing outside the retention tree is refused.
	h.dbExec(`UPDATE destination_files SET retained_path = 'movies/keep.mkv' WHERE id = ?`, recs[2].ID)
	h.advance(31 * 24 * time.Hour)
	res, st, j, err := h.runRetention()
	if err != nil {
		t.Fatal(err)
	}
	if st.FilesExpired != 0 || res.Warnings != 3 {
		t.Fatalf("%+v warnings %d items %+v", st, res.Warnings, h.items(j.ID))
	}
	if len(retainedRecords(h)) != 3 || !exists(h.dstPath("movies/keep.mkv")) || !exists(h.dstPath(recs[1].RetainedPath)) {
		t.Fatal("something was deleted")
	}
}

func TestRetentionKeepsOrphans(t *testing.T) {
	h := retainedFixture(t)
	if err := h.dests.SetSources(h.ctx, h.dest.ID, nil); err != nil {
		t.Fatal(err)
	}
	h.advance(31 * 24 * time.Hour)
	_, st, _, err := h.runRetention()
	if err != nil || st.FilesPlanned != 0 || len(retainedRecords(h)) != 3 {
		t.Fatalf("orphan retained rows were expired: %+v %v", st, err)
	}
}

func TestRetentionNeedsTheMarker(t *testing.T) {
	h := retainedFixture(t)
	if err := os.Remove(h.dstPath(filecopy.MarkerRel)); err != nil {
		t.Fatal(err)
	}
	h.advance(31 * 24 * time.Hour)
	if _, _, _, err := h.runRetention(); !errors.Is(err, destinations.ErrNotMounted) {
		t.Fatalf("err = %v", err)
	}
	if len(retainedRecords(h)) != 3 {
		t.Fatal("expired without the marker")
	}
}

func TestRetentionResumesAfterCrash(t *testing.T) {
	// Three expired rows, planned in batches of two: one plan batch boundary, three of the rest.
	for point, times := range map[string]int{"expire.afterRemove": 3, PointRecordAfterFS: 3, PointRecordAfterDB: 3, PointPlanAfterBatch: 1} {
		for n := 1; n <= times; n++ {
			t.Run(point, func(t *testing.T) {
				h := retainedFixture(t)
				h.advance(31 * 24 * time.Hour)
				j := h.newJob(jobs.TypeRetention, false, jobs.Params{DestinationID: h.dest.ID})
				crashed := func() (crashed bool) {
					faultinject.SetHook(faultinject.CrashAt(point, n))
					defer faultinject.SetHook(nil)
					defer func() {
						if p := recover(); p != nil {
							if _, ok := p.(faultinject.Crash); !ok {
								panic(p)
							}
							crashed = true
						}
					}()
					_, _ = h.retention.Run(h.ctx, j, h.env())
					return false
				}()
				if !crashed {
					t.Fatalf("no crash at %s #%d", point, n)
				}
				j.Attempt, j.Trigger = 2, jobs.TriggerResume
				res, err := h.run(h.retention, j)
				if err != nil || res.Warnings != 0 {
					t.Fatalf("resume: %v %+v", err, res)
				}
				if len(retainedRecords(h)) != 0 {
					t.Fatal("rows left")
				}
				h.assertConverged(t)
			})
		}
	}
}

// fakeEnqueuer records the specs it is given.
type fakeEnqueuer struct {
	mu    sync.Mutex
	specs []jobs.Spec
}

func (f *fakeEnqueuer) Enqueue(_ context.Context, s jobs.Spec) (jobs.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.specs = append(f.specs, s)
	return jobs.Job{ID: int64(len(f.specs)), Type: s.Type, Params: s.Params}, nil
}

func TestGlobalRetentionQueuesDestinationsAndPrunesHistory(t *testing.T) {
	h := newHarness(t)
	// A second, disabled destination gets no job.
	off := false
	other := resolvedTempDir(t)
	if _, err := h.dests.Create(h.ctx, destinations.Input{Name: "off", Target: other, Enabled: &off}, destinations.CreateOptions{AllowLocal: true}); err != nil {
		t.Fatal(err)
	}
	// An old finished job is pruned; a recent one is kept.
	old := h.newJob(jobs.TypeSync, false, jobs.Params{})
	h.dbExec(`UPDATE jobs SET status = 'completed', finished_at = ? WHERE id = ?`, db.FormatTime(h.now().Add(-100*24*time.Hour)), old.ID)
	recent := h.newJob(jobs.TypeVerify, false, jobs.Params{})
	h.closeJob(recent.ID)

	enq := &fakeEnqueuer{}
	r := NewRetentionRunner(RetentionOptions{Options: Options{DB: h.db, Store: h.store, Catalog: h.cat, Scanner: h.scanner,
		Destinations: h.dests, Now: h.now}, Enqueuer: enq, PruneHistory: h.jq.PruneHistory})
	j := h.newJob(jobs.TypeRetention, false, jobs.Params{})
	j.Trigger = jobs.TriggerSchedule
	res, err := r.Run(h.ctx, j, h.env())
	if err != nil {
		t.Fatal(err)
	}
	st := res.Stats.(RetentionStats)
	if st.JobsQueued != 1 || st.JobsPruned != 1 {
		t.Fatalf("%+v", st)
	}
	if len(enq.specs) != 1 || enq.specs[0].Params.DestinationID != h.dest.ID || enq.specs[0].Type != jobs.TypeRetention ||
		enq.specs[0].Trigger != jobs.TriggerSchedule {
		t.Fatalf("specs %+v", enq.specs)
	}
	if _, err := h.jq.GetJob(h.ctx, old.ID); err == nil {
		t.Error("the old job was not pruned")
	}
	if _, err := h.jq.GetJob(h.ctx, recent.ID); err != nil {
		t.Error("the recent job was pruned")
	}
	// The setting changes the history length.
	r.historyDays = func(context.Context) (int, error) { return 1, nil }
	h.dbExec(`UPDATE jobs SET finished_at = ? WHERE id = ?`, db.FormatTime(h.now().Add(-2*24*time.Hour)), recent.ID)
	h.closeJob(j.ID)
	j2 := h.newJob(jobs.TypeRetention, false, jobs.Params{})
	if res, err := r.Run(h.ctx, j2, h.env()); err != nil || res.Stats.(RetentionStats).JobsPruned < 1 {
		t.Fatalf("history days: %+v %v", res, err)
	}
}
