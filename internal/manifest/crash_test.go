package manifest

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// crashPoints are the step boundaries of an export, in order: the manifest points and the
// filecopy point of the version directory's rename.
var crashPoints = []string{PointAfterWrite, PointBeforeRename, "move.afterRename", PointAfterRename, PointBeforeRecord, PointAfterRecord}

func TestCrashMatrixResume(t *testing.T) {
	// A job crashes at every step boundary and is resumed: exactly one intact, recorded version
	// remains, nothing is left under a "." name, the resumed job's stats name the version, and a
	// further export finds it unchanged.
	for i, point := range crashPoints {
		t.Run(point, func(t *testing.T) {
			e := newEnv(t)
			job := e.newJob(false)
			e.crashRun(job, point)
			crashedAt := e.clock.Now()

			job.Attempt, job.Trigger = 2, jobs.TriggerResume
			e.clock.Advance(time.Minute)
			res, rep, err := e.run(job)
			if err != nil {
				t.Fatalf("resume: %v\n%s", err, rep)
			}
			vs := e.assertConverged(1)
			st := res.Stats.(Stats)
			if st.ManifestID != vs[0].ID || vs[0].JobID != job.ID {
				t.Fatalf("stats %+v, version %+v", st, vs[0])
			}
			// From the rename on, the version is complete: the resume finishes with it (recording
			// it when the crash came before the insert) and writes no second one.
			renamed := i >= slices.Index(crashPoints, "move.afterRename")
			want := e.clock.Now()
			if renamed {
				want = crashedAt
			}
			if !vs[0].CreatedAt.Equal(want) {
				t.Fatalf("the version was made at %s, want %s (renamed: %v)", vs[0].CreatedAt, want, renamed)
			}
			if adopted := renamed && point != PointAfterRecord; adopted != (st.Recovered == 1) {
				t.Fatalf("recovered %d after a crash at %s", st.Recovered, point)
			}
			if st.Unchanged {
				t.Fatalf("the resumed job reported unchanged: %+v", st)
			}
			e.clock.Advance(time.Minute)
			if next, _ := e.runOK(); !next.Unchanged || next.ManifestID != vs[0].ID {
				t.Fatalf("the next export %+v", next)
			}
		})
	}
}

func TestCrashMatrixPrune(t *testing.T) {
	// A job crashes while pruning the version of the day before, and is resumed with the same
	// retention or after it was raised (the version is kept after all). Every row names an intact
	// version directory and nothing is left under a "." name.
	for _, point := range []string{PointPruneAfterTrash, PointPruneAfterUnrecord, PointPruneAfterRemove} {
		for _, raised := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s raised=%v", point, raised), func(t *testing.T) {
				e := newEnv(t)
				e.setRetention(1, 1)
				e.runOK()
				e.clock.Advance(24 * time.Hour)
				e.refresh(e.rad.ID)
				e.refresh(e.son.ID)
				e.writeFile("movies/new.mkv", 10) // a change: the job writes a version and prunes the old one
				e.scan()
				job := e.newJob(false)
				e.crashRun(job, point)
				if raised {
					e.setRetention(30, 12)
				}
				job.Attempt, job.Trigger = 2, jobs.TriggerResume
				e.clock.Advance(time.Minute)
				res, rep, err := e.run(job)
				if err != nil {
					t.Fatalf("resume: %v\n%s", err, rep)
				}
				// Only a version whose deletion was not committed (its row still there) can be
				// kept after all.
				want := 1
				if raised && point == PointPruneAfterTrash {
					want = 2
				}
				vs := e.assertConverged(want)
				if vs[0].JobID != job.ID || res.Stats.(Stats).ManifestID != vs[0].ID {
					t.Fatalf("versions %+v, stats %+v", vs, res.Stats)
				}
			})
		}
	}
}

func TestCrashMatrixManager(t *testing.T) {
	// The same crashes through the real job manager: the crashed job is recovered at the next
	// start (attempt 2, trigger resume) and completes with one version.
	for _, point := range []string{PointAfterWrite, PointAfterRename, PointAfterRecord} {
		t.Run(point, func(t *testing.T) {
			e := newEnv(t)
			crashed := make(chan struct{})
			hook := faultinject.CrashAt(point, 1)
			var once sync.Once
			faultinject.SetHook(func(name string) {
				defer func() {
					if p := recover(); p != nil {
						once.Do(func() { close(crashed) })
						panic(p)
					}
				}()
				hook(name)
			})
			defer faultinject.SetHook(nil)

			m1 := jobqueue.New(e.db, nil, jobqueue.Options{Workers: 1})
			m1.Register(jobs.TypeManifestExport, e.runner)
			if err := m1.Start(e.ctx); err != nil {
				t.Fatal(err)
			}
			job, err := m1.Enqueue(e.ctx, jobs.Spec{Type: jobs.TypeManifestExport, Trigger: jobs.TriggerManual, Params: jobs.Params{DestinationID: e.dest.ID}})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-crashed:
			case <-time.After(30 * time.Second):
				t.Fatal("the job did not reach the crash point")
			}
			store := jobqueue.NewStore(e.db)
			stopCtx, cancel := context.WithTimeout(e.ctx, 30*time.Second)
			defer cancel()
			waitFor(t, func() bool {
				j, err := store.GetJob(e.ctx, job.ID)
				return err == nil && j.Status == jobs.StatusRunning
			})
			if err := m1.Stop(stopCtx); err != nil {
				t.Fatal(err)
			}
			faultinject.SetHook(nil)

			m2 := jobqueue.New(e.db, nil, jobqueue.Options{Workers: 1})
			m2.Register(jobs.TypeManifestExport, e.runner)
			if err := m2.Start(e.ctx); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = m2.Stop(stopCtx) }()
			var final jobs.Job
			waitFor(t, func() bool {
				final, err = store.GetJob(e.ctx, job.ID)
				return err == nil && final.Status.Final()
			})
			// completed_with_warnings: the unlocated /movies-4k item.
			if final.Status != jobs.StatusCompletedWithWarnings || final.Attempt != 2 || final.Trigger != jobs.TriggerResume {
				t.Fatalf("job %+v", final)
			}
			e.assertConverged(1)
		})
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
