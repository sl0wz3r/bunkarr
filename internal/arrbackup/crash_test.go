package arrbackup

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/snapshots"
)

// crashRun runs job and expects it to stop with a faultinject.Crash at point.
func (f *fixture) crashRun(t *testing.T, job jobs.Job, point string) {
	t.Helper()
	faultinject.SetHook(faultinject.CrashAt(point, 1))
	defer faultinject.SetHook(nil)
	defer func() {
		p := recover()
		if c, ok := p.(faultinject.Crash); !ok || c.Point != point {
			t.Fatalf("the run did not crash at %s: %v", point, p)
		}
	}()
	_, _ = f.run(t, job)
}

// crashPoints are the step boundaries of a backup, in order: the arrbackup points and the
// filecopy engine's points inside them.
var crashPoints = []string{
	PointAfterCommand, PointAfterStage, "copy.afterWrite", "copy.afterRename", PointAfterCopy, PointBeforeRename,
	"move.afterRename", PointAfterRename, PointBeforeRecord, PointAfterRecord,
}

// TestCrashMatrixResume: a crash at every step, then the resumed job (attempt 2, trigger resume)
// converges to exactly one complete version, recorded once, and the *arr is never asked for a
// second backup: the resume uses the manual backup made since the job was queued.
func TestCrashMatrixResume(t *testing.T) {
	for i, point := range crashPoints {
		for _, method := range []string{FetchFolder, FetchHTTP} {
			t.Run(point+" "+method, func(t *testing.T) {
				f := newFixture(t)
				if method == FetchHTTP {
					f.httpSettings(t)
				}
				job := f.newJob(t, false)
				f.crashRun(t, job, point)
				crashedAt := f.clock.Now()
				if f.commands() != 1 {
					t.Fatalf("%d commands before the crash", f.commands())
				}

				job.Attempt, job.Trigger = 2, jobs.TriggerResume
				f.clock.Set(f.clock.Now().Add(time.Minute))
				res, err := f.run(t, job)
				if err != nil {
					t.Fatalf("resume: %v\nlogs:\n%s", err, f.rep.text())
				}
				if f.commands() != 1 {
					t.Fatalf("the resume sent another Backup command (%d in all)", f.commands())
				}
				snaps := f.assertConverged(t, 1)
				stats := res.Stats.(Stats)
				if stats.SnapshotID != snaps[0].ID || snaps[0].JobID != job.ID || stats.BackupName != manualName {
					t.Fatalf("stats %+v, snapshot %+v", stats, snaps[0])
				}
				// From the version directory's rename on, the version is complete: the resume
				// finishes with it (recording it when the crash came before the insert).
				renamed := i >= slices.Index(crashPoints, "move.afterRename")
				wantCreated := f.clock.Now()
				if renamed {
					wantCreated = crashedAt
				}
				if !snaps[0].CreatedAt.Equal(wantCreated) {
					t.Fatalf("the version was made at %s, want %s (renamed: %v)", snaps[0].CreatedAt, wantCreated, renamed)
				}
				if adopted := renamed && point != PointAfterRecord; adopted != (stats.Recovered == 1) {
					t.Fatalf("recovered %d after a crash at %s", stats.Recovered, point)
				}
			})
		}
	}
}

func TestCrashMatrixPrune(t *testing.T) {
	// A job crashes while pruning the version of the day before, and is resumed with the same
	// retention or after the retention was raised (the version is kept after all). Every row must
	// name a complete version directory, nothing may be left under a "." name, and the resume
	// takes no second backup.
	for _, point := range []string{PointPruneAfterTrash, PointPruneAfterUnrecord, PointPruneAfterRemove} {
		for _, raised := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s raised=%v", point, raised), func(t *testing.T) {
				f := newFixture(t)
				f.setRetention(t, 1, 1)
				f.runOK(t)
				f.clock.Set(f.clock.Now().AddDate(0, 0, 1))
				job := f.newJob(t, false)
				f.crashRun(t, job, point)
				crashedAt := f.clock.Now()
				commands := f.commands()
				if raised {
					f.setRetention(t, 14, 8)
				}
				job.Attempt, job.Trigger = 2, jobs.TriggerResume
				f.clock.Set(f.clock.Now().Add(time.Minute))
				res, err := f.run(t, job)
				if err != nil {
					t.Fatalf("resume: %v\nlogs:\n%s", err, f.rep.text())
				}
				want := 1
				if raised && point == PointPruneAfterTrash {
					want = 2
				}
				snaps := f.assertConverged(t, want)
				if snaps[0].JobID != job.ID || !snaps[0].CreatedAt.Equal(crashedAt) || res.Stats.(Stats).SnapshotID != snaps[0].ID ||
					f.commands() != commands {
					t.Fatalf("snapshots %+v, stats %+v, %d commands (was %d)", snaps, res.Stats, f.commands(), commands)
				}
			})
		}
	}
}

func TestPruneDropsALostVersion(t *testing.T) {
	// The row of a version a prune had deleted comes back (its delete was lost) while the
	// retention now keeps it: what is left of its ".prune-*" directory is not restored, its row is
	// removed with a warning.
	f := newFixture(t)
	f.setRetention(t, 1, 1)
	f.runOK(t)
	lost := f.snapshots(t)[0]
	f.clock.Set(f.clock.Now().AddDate(0, 0, 1))
	job := f.newJob(t, false)
	f.crashRun(t, job, PointPruneAfterUnrecord)
	// The removal of the ".prune-*" directory had begun.
	var m Manifest
	_ = json.Unmarshal(lost.Manifest, &m)
	if err := os.Remove(filepath.Join(f.target, filepath.FromSlash(snapshots.TrashPath(lost.Path)), m.Zip.Name)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.runner.Store().Insert(f.ctx, lost); err != nil {
		t.Fatal(err)
	}
	f.setRetention(t, 14, 8)
	job.Attempt, job.Trigger = 2, jobs.TriggerResume
	f.clock.Set(f.clock.Now().Add(time.Minute))
	res, err := f.run(t, job)
	if err != nil {
		t.Fatalf("resume: %v\nlogs:\n%s", err, f.rep.text())
	}
	snaps := f.assertConverged(t, 1)
	if snaps[0].JobID != job.ID || res.Warnings != 1 || !f.rep.has("missing or incomplete at the destination") {
		t.Fatalf("snapshots %+v, %d warnings\nlogs:\n%s", snaps, res.Warnings, f.rep.text())
	}
}

func TestCrashMatrixManager(t *testing.T) {
	// The same crashes through the real job manager: the crashed job is recovered at the next
	// start (attempt 2, trigger resume) and completes with one version and one Backup command.
	for _, point := range []string{PointAfterCommand, PointAfterStage, PointAfterCopy, PointBeforeRename, PointAfterRename, PointAfterRecord} {
		t.Run(point, func(t *testing.T) {
			f := newFixture(t)
			// The manager queues on the real clock: the *arr makes its backup after that.
			f.setEntries(t, entry{id: 1, name: manualName, typ: f.entries[0].typ, time: time.Now().Add(time.Minute), zip: f.entries[0].zip})
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

			m1 := jobqueue.New(f.db, nil, jobqueue.Options{Workers: 1})
			m1.Register(jobs.TypeArrBackup, f.runner)
			if err := m1.Start(f.ctx); err != nil {
				t.Fatal(err)
			}
			job, err := m1.Enqueue(f.ctx, jobs.Spec{Type: jobs.TypeArrBackup, Params: jobs.Params{IntegrationID: f.integ.ID, DestinationID: f.dest.ID}})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-crashed:
			case <-time.After(30 * time.Second):
				t.Fatal("the job did not reach the crash point")
			}
			stopCtx, cancel := context.WithTimeout(f.ctx, 30*time.Second)
			defer cancel()
			if err := m1.Stop(stopCtx); err != nil {
				t.Fatal(err)
			}
			faultinject.SetHook(nil)

			m2 := jobqueue.New(f.db, nil, jobqueue.Options{Workers: 1})
			m2.Register(jobs.TypeArrBackup, f.runner)
			if err := m2.Start(f.ctx); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = m2.Stop(stopCtx) }()
			final := waitFinal(t, f, job.ID)
			if final.Status != jobs.StatusCompleted || final.Attempt != 2 || final.Trigger != jobs.TriggerResume {
				t.Fatalf("job %+v", final)
			}
			if f.commands() != 1 {
				t.Fatalf("%d Backup commands", f.commands())
			}
			f.assertConverged(t, 1)
		})
	}
}
