package arrbackup

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr"
	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// copyTree copies the regular files of directory src into dst (created 0700).
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	es, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range es {
		data, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestRecoverLeavesOtherIntegrationsVersionsAlone: integration ids start over with a new Bunkarr
// database, so a folder ".bunkarr/arr/<slug>-<id>" may hold the versions of an integration that
// no longer exists. Recovery records an unrecorded version only when its manifest is of this
// integration's application and it lies in this integration's folder; the others are left alone
// (never recorded, so never pruned), with a warning.
func TestRecoverLeavesOtherIntegrationsVersionsAlone(t *testing.T) {
	f := newFixture(t)
	f.runOK(t)
	made := f.snapshots(t)[0]
	var m Manifest
	if err := json.Unmarshal(made.Manifest, &m); err != nil {
		t.Fatal(err)
	}
	// The version becomes unrecorded (a crash before its insert): recovery records it (control).
	if err := f.runner.Store().Remove(f.ctx, made.ID); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(f.target, filepath.FromSlash(made.Path))
	version := filepath.Base(made.Path)
	id := itoa(f.integ.ID)
	// The lost database's jobs had this database's ids too, queued at other times.
	lostQueued := m.JobQueuedAt.Add(-40 * 24 * time.Hour)
	foreign := map[string]func(*Manifest){
		// A Sonarr of the lost database that had this id.
		"tv-" + id: func(m *Manifest) { m.App, m.IntegrationName, m.JobQueuedAt = string(arr.KindSonarr), "TV", lostQueued },
		// Another Radarr of the lost database that had this id.
		"radarr-hd-" + id: func(m *Manifest) { m.IntegrationName, m.JobQueuedAt = "Radarr HD", lostQueued },
	}
	for folder, edit := range foreign {
		dir := filepath.Join(f.target, ".bunkarr", "arr", folder, version)
		copyTree(t, src, dir)
		mm := m
		edit(&mm)
		raw, err := json.MarshalIndent(mm, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ManifestName), append(raw, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f.setRetention(t, 1, 0)
	f.clock.Set(f.clock.Now().Add(time.Hour))
	res := f.runOK(t)
	if got := res.Stats.(Stats).Recovered; got != 1 {
		t.Fatalf("recovered %d versions, want 1 (only this integration's own)\nlogs:\n%s", got, f.rep.text())
	}
	for _, s := range f.snapshots(t) {
		if !strings.HasPrefix(s.Path, FolderName(f.integ.Name, f.integ.ID)+"/") {
			t.Fatalf("a version of another integration was recorded: %+v", s)
		}
	}
	for folder := range foreign {
		dir := filepath.Join(f.target, ".bunkarr", "arr", folder, version)
		if _, err := os.Stat(filepath.Join(dir, m.Zip.Name)); err != nil {
			t.Fatalf("the version in %s was touched: %v", folder, err)
		}
		if !f.rep.has("left alone") || !f.rep.has(folder+"/"+version) {
			t.Fatalf("no warning about %s\nlogs:\n%s", folder, f.rep.text())
		}
	}
	if res.Warnings < len(foreign) {
		t.Fatalf("%d warnings, want at least %d", res.Warnings, len(foreign))
	}
}

// TestRecoverAdoptsItsOwnVersionAfterARename: the version a crashed attempt of this very job
// wrote is recognised by its manifest's job, even in the folder of the integration's old name.
func TestRecoverAdoptsItsOwnVersionAfterARename(t *testing.T) {
	f := newFixture(t)
	job := f.newJob(t, false)
	f.crashRun(t, job, PointBeforeRecord)
	enabled := true
	it, err := f.ints.Update(f.ctx, f.integ.ID, integrations.Input{Type: f.integ.Type, Name: "Movies UHD", URL: f.integ.URL,
		Enabled: &enabled, Settings: f.integ.Settings})
	if err != nil {
		t.Fatal(err)
	}
	job.Attempt, job.Trigger = 2, jobs.TriggerResume
	f.clock.Set(f.clock.Now().Add(time.Minute))
	res, err := f.run(t, job)
	if err != nil {
		t.Fatalf("resume: %v\nlogs:\n%s", err, f.rep.text())
	}
	snaps := f.snapshots(t)
	if len(snaps) != 1 || res.Stats.(Stats).Recovered != 1 || res.Stats.(Stats).SnapshotID != snaps[0].ID ||
		strings.HasPrefix(snaps[0].Path, FolderName(it.Name, it.ID)) || f.commands() != 1 {
		t.Fatalf("result %+v, snapshots %+v, %d commands\nlogs:\n%s", res, snaps, f.commands(), f.rep.text())
	}
}

// TestRunUnchangedChecksTheStoredCopy: a scheduled backup is reported as already at the
// destination only when its copy reads back as recorded. A damaged copy is marked failed (a dry
// run marks nothing) and the backup is copied again.
func TestRunUnchangedChecksTheStoredCopy(t *testing.T) {
	damages := map[string]func(t *testing.T, dir, zip string){
		"zip deleted": func(t *testing.T, dir, zip string) {
			if err := os.Remove(filepath.Join(dir, zip)); err != nil {
				t.Fatal(err)
			}
		},
		"zip truncated": func(t *testing.T, dir, zip string) {
			if err := os.Truncate(filepath.Join(dir, zip), 100); err != nil {
				t.Fatal(err)
			}
		},
		"zip changed, same size": func(t *testing.T, dir, zip string) {
			p := filepath.Join(dir, zip)
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			data[len(data)/2] ^= 0xff
			if err := os.WriteFile(p, data, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"manifest deleted": func(t *testing.T, dir, _ string) {
			if err := os.Remove(filepath.Join(dir, ManifestName)); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, damage := range damages {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			scheduled := entry{id: 7, name: "radarr_backup_v6.4.4.10685_2026.09.24_06.00.00.zip", typ: arr.BackupScheduled,
				time: f.clock.Now().Add(-24 * time.Hour), zip: goodZip(t)}
			f.setEntries(t, f.entries[0], scheduled)
			f.runOK(t)
			first := f.snapshots(t)[0]
			damage(t, filepath.Join(f.target, filepath.FromSlash(first.Path)), scheduled.name)

			f.clock.Set(f.clock.Now().Add(time.Hour))
			dry := f.newJob(t, true)
			res, err := f.run(t, dry)
			if err != nil {
				t.Fatalf("dry run: %v", err)
			}
			items, err := f.jobs.ListItems(f.ctx, dry.ID, jobqueue.ItemQuery{})
			if err != nil || len(items.Records) != 1 {
				t.Fatalf("items %+v, %v", items, err)
			}
			var d itemDetail
			_ = json.Unmarshal(items.Records[0].Detail, &d)
			if res.Stats.(Stats).Unchanged || d.Would != "copy-scheduled" || res.Warnings == 0 {
				t.Fatalf("dry run %+v, detail %+v", res, d)
			}
			if s := f.snapshots(t); len(s) != 1 || s[0].Integrity != IntegrityOK {
				t.Fatalf("a dry run changed the records: %+v", s)
			}

			res = f.runOK(t)
			stats := res.Stats.(Stats)
			if stats.Unchanged || !stats.ReusedScheduled || stats.BackupName != scheduled.name || stats.SnapshotID == first.ID ||
				res.Warnings == 0 || f.commands() != 0 {
				t.Fatalf("%d commands, result %+v\nlogs:\n%s", f.commands(), res, f.rep.text())
			}
			snaps := f.snapshots(t)
			if len(snaps) != 2 || snaps[0].ID != stats.SnapshotID || snaps[0].Integrity != IntegrityOK ||
				snaps[1].Path != first.Path || snaps[1].Integrity != IntegrityFailed {
				t.Fatalf("snapshots %+v", snaps)
			}
			f.checkVersion(t, snaps[0])

			// The new copy is the one found from now on.
			f.clock.Set(f.clock.Now().Add(time.Hour))
			res = f.runOK(t)
			if stats := res.Stats.(Stats); !stats.Unchanged || stats.SnapshotID != snaps[0].ID || res.Warnings != 0 {
				t.Fatalf("third run %+v\nlogs:\n%s", res, f.rep.text())
			}
		})
	}
}

// commandPolls counts the GET command/21 requests the fake Radarr received.
func (f *fixture) commandPolls() int {
	n := 0
	for _, r := range f.arr.Requests() {
		if r.Method == http.MethodGet && strings.HasSuffix(r.Path, "/command/21") {
			n++
		}
	}
	return n
}

// TestResumeFollowsTheInterruptedBackupCommand: Bunkarr stops (a crash, or a shutdown that
// re-queues the job) while the *arr is still making the backup it asked for. The resumed job
// follows that command instead of sending another: the *arr never prunes manual backups.
func TestResumeFollowsTheInterruptedBackupCommand(t *testing.T) {
	for _, stop := range []string{"crash", "shutdown"} {
		for _, method := range []string{FetchFolder, FetchHTTP} {
			t.Run(stop+" "+method, func(t *testing.T) {
				f := newFixture(t)
				if method == FetchHTTP {
					f.httpSettings(t)
				}
				manual := f.entries[0]
				// The *arr lists no backup yet and its command is still running.
				f.arr.SetBackups([]map[string]any{})
				completed := fixtureCommand(t)
				f.arr.SetJSON(http.MethodGet, "command/21", []byte(strings.Replace(string(completed), `"status": "completed"`, `"status": "started"`, 1)))
				job := f.newJob(t, false)
				if stop == "crash" {
					f.crashRun(t, job, PointAfterCommand)
				} else {
					ctx, cancel := context.WithCancel(f.ctx)
					go func() {
						deadline := time.Now().Add(10 * time.Second)
						for f.commandPolls() == 0 && time.Now().Before(deadline) {
							time.Sleep(time.Millisecond)
						}
						cancel()
					}()
					if _, err := f.runner.Run(ctx, job, jobs.Env{Reporter: f.rep, Items: f.jobs}); !errors.Is(err, context.Canceled) {
						t.Fatalf("first attempt: %v", err)
					}
				}
				if f.commands() != 1 {
					t.Fatalf("%d commands before the stop", f.commands())
				}

				job.Attempt, job.Trigger = 2, jobs.TriggerResume
				if stop == "shutdown" {
					job.Attempt = 1
				}
				f.clock.Set(f.clock.Now().Add(time.Minute))
				polls := f.commandPolls()
				done := make(chan struct{})
				var once sync.Once
				finish := func() {
					once.Do(func() {
						f.arr.SetBackups([]map[string]any{{"id": manual.id, "name": manual.name, "type": manual.typ, "size": len(manual.zip),
							"time": manual.time.UTC().Format(time.RFC3339), "path": "/backup/" + manual.typ + "/" + manual.name}})
						f.arr.SetJSON(http.MethodGet, "command/21", completed)
					})
				}
				go func() {
					defer close(done)
					deadline := time.Now().Add(10 * time.Second)
					for f.commandPolls() <= polls && time.Now().Before(deadline) {
						time.Sleep(time.Millisecond)
					}
					finish()
				}()
				res, err := f.run(t, job)
				<-done
				if err != nil {
					t.Fatalf("resume: %v\nlogs:\n%s", err, f.rep.text())
				}
				if f.commands() != 1 {
					t.Fatalf("the resume sent another Backup command (%d in all)\nlogs:\n%s", f.commands(), f.rep.text())
				}
				snaps := f.assertConverged(t, 1)
				if stats := res.Stats.(Stats); stats.BackupName != manualName || stats.SnapshotID != snaps[0].ID {
					t.Fatalf("stats %+v", stats)
				}
				items, err := f.jobs.ListItems(f.ctx, job.ID, jobqueue.ItemQuery{})
				if err != nil || len(items.Records) != 1 || items.Records[0].RelPath != "manual/"+manualName || items.Records[0].Status != jobs.ItemDone {
					t.Fatalf("items %+v, %v", items, err)
				}
			})
		}
	}
}

// TestResumeWithAnUnknownCommandAsksAgain: a command the *arr no longer knows (it was restarted
// before it made the backup) is not waited for; the resumed job asks for a backup.
func TestResumeWithAnUnknownCommandAsksAgain(t *testing.T) {
	f := newFixture(t)
	manual := f.entries[0]
	f.arr.SetBackups([]map[string]any{})
	job := f.newJob(t, false)
	f.crashRun(t, job, PointAfterCommand)
	f.arr.SetStatus(http.MethodGet, "command/21", http.StatusNotFound)
	// The new command makes the backup (the fake answers it with id 21 again).
	f.runner.poll = 50 * time.Millisecond
	completed := fixtureCommand(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(10 * time.Second)
		for f.commands() < 2 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		f.arr.SetBackups([]map[string]any{{"id": manual.id, "name": manual.name, "type": manual.typ, "size": len(manual.zip),
			"time": manual.time.UTC().Format(time.RFC3339), "path": "/backup/" + manual.typ + "/" + manual.name}})
		f.arr.SetJSON(http.MethodGet, "command/21", completed)
	}()
	job.Attempt, job.Trigger = 2, jobs.TriggerResume
	f.clock.Set(f.clock.Now().Add(time.Minute))
	_, err := f.run(t, job)
	<-done
	if err != nil {
		t.Fatalf("resume: %v\nlogs:\n%s", err, f.rep.text())
	}
	if f.commands() != 2 || !f.rep.has("unknown to Radarr") {
		t.Fatalf("%d commands\nlogs:\n%s", f.commands(), f.rep.text())
	}
	f.assertConverged(t, 1)
}

// TestRunRemovesStagingWhenItStopsBeforeTheBackup: an attempt that finds an earlier attempt's
// staging (the *arr's zip with its secrets) and fails before the backup starts removes it, and
// sweeps the stale staging of other jobs (S17).
func TestRunRemovesStagingWhenItStopsBeforeTheBackup(t *testing.T) {
	tests := map[string]func(t *testing.T, f *fixture){
		"integration disabled": func(t *testing.T, f *fixture) {
			off := false
			if _, err := f.ints.Update(f.ctx, f.integ.ID, integrations.Input{Name: f.integ.Name, URL: f.integ.URL, Enabled: &off}); err != nil {
				t.Fatal(err)
			}
		},
		"integration deleted": func(t *testing.T, f *fixture) {
			if err := f.ints.Delete(f.ctx, f.integ.ID); err != nil {
				t.Fatal(err)
			}
		},
		"destination not mounted": func(t *testing.T, f *fixture) {
			if err := os.RemoveAll(filepath.Join(f.target, ".bunkarr")); err != nil {
				t.Fatal(err)
			}
		},
		"insecure modes": func(t *testing.T, f *fixture) { f.setEnforcesModes(t, false) },
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			job := f.newJob(t, false)
			f.crashRun(t, job, PointAfterStage)
			staging := f.runner.StagingDir(job.ID)
			if es, err := os.ReadDir(staging); err != nil || len(es) == 0 {
				t.Fatalf("no staged zip after the crash: %v, %v", es, err)
			}
			stale := f.runner.StagingDir(999)
			if err := os.MkdirAll(stale, 0o700); err != nil {
				t.Fatal(err)
			}
			old := f.clock.Now().Add(-48 * time.Hour)
			if err := os.Chtimes(stale, old, old); err != nil {
				t.Fatal(err)
			}
			setup(t, f)
			job.Attempt, job.Trigger = 2, jobs.TriggerResume
			if _, err := f.run(t, job); err == nil {
				t.Fatal("the resume succeeded")
			}
			for _, p := range []string{staging, stale} {
				if _, err := os.Stat(p); !os.IsNotExist(err) {
					t.Fatalf("%s is left: %v", p, err)
				}
			}
		})
	}
}

// TestStagingOfAJobFailedByCrashRecoveryIsRemoved: a job that crash recovery fails for good is
// never run again; the OnFinish hook removes its staging.
func TestStagingOfAJobFailedByCrashRecoveryIsRemoved(t *testing.T) {
	f := newFixture(t)
	f.setEntries(t, entry{id: 1, name: manualName, typ: f.entries[0].typ, time: time.Now().Add(time.Minute), zip: f.entries[0].zip})
	crashed := make(chan struct{})
	hook := faultinject.CrashAt(PointAfterStage, 1)
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
	m1.OnFinish(f.runner.OnJobFinish)
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
	staging := f.runner.StagingDir(job.ID)
	if es, err := os.ReadDir(staging); err != nil || len(es) == 0 {
		t.Fatalf("no staged zip after the crash: %v, %v", es, err)
	}

	m2 := jobqueue.New(f.db, nil, jobqueue.Options{Workers: 1, MaxAttempts: 1})
	m2.Register(jobs.TypeArrBackup, f.runner)
	m2.OnFinish(f.runner.OnJobFinish)
	if err := m2.Start(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := m2.Stop(stopCtx); err != nil { // waits for the hooks
		t.Fatal(err)
	}
	final, err := f.jobs.GetJob(f.ctx, job.ID)
	if err != nil || final.Status != jobs.StatusFailed {
		t.Fatalf("job %+v, %v", final, err)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatalf("the staging of the failed job is left: %v", err)
	}
}

// TestSweepStaging: the start-up sweep removes stale staging and keeps fresh staging.
func TestSweepStaging(t *testing.T) {
	f := newFixture(t)
	fresh, stale := f.runner.StagingDir(5), f.runner.StagingDir(6)
	for _, p := range []string{fresh, stale} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := f.clock.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	f.runner.SweepStaging()
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh staging removed: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale staging left: %v", err)
	}
}

// TestResumeAfterTheArrStoppedTheCommandAsksAgain: Bunkarr and the *arr stop together while the
// *arr is making the backup (a host reboot, a compose-stack restart). At its start-up the *arr
// marks every started command "orphaned" and keeps it (GET command/{id} answers it, not 404). The
// resumed job asks for a new backup instead of failing on the ended command, and does not use the
// partial zip the *arr was writing when it stopped (listed as a manual backup since the job was
// queued).
func TestResumeAfterTheArrStoppedTheCommandAsksAgain(t *testing.T) {
	for _, status := range []string{"orphaned", "aborted", "cancelled"} {
		for _, stop := range []string{"crash", "shutdown"} {
			t.Run(status+" "+stop, func(t *testing.T) {
				f := newFixture(t)
				good := f.entries[0]
				partial := entry{id: 2, name: "radarr_backup_v6.4.4.10685_2026.09.25_12.30.05.zip", typ: arr.BackupManual,
					time: cmdQueued.Add(-30 * time.Second), zip: good.zip[:len(good.zip)/2]}
				f.arr.SetBackups([]map[string]any{})
				completed := fixtureCommand(t)
				f.arr.SetJSON(http.MethodGet, "command/21", []byte(strings.Replace(string(completed), `"status": "completed"`, `"status": "started"`, 1)))
				job := f.newJob(t, false)
				if stop == "crash" {
					f.crashRun(t, job, PointAfterCommand)
					job.Attempt = 2
				} else {
					ctx, cancel := context.WithCancel(f.ctx)
					go func() {
						deadline := time.Now().Add(10 * time.Second)
						for f.commandPolls() == 0 && time.Now().Before(deadline) {
							time.Sleep(time.Millisecond)
						}
						cancel()
					}()
					if _, err := f.runner.Run(ctx, job, jobs.Env{Reporter: f.rep, Items: f.jobs}); !errors.Is(err, context.Canceled) {
						t.Fatalf("first attempt: %v", err)
					}
				}
				if f.commands() != 1 {
					t.Fatalf("%d commands before the stop", f.commands())
				}
				// The *arr restarted: the command has ended, and the zip it was writing is listed.
				ended := strings.Replace(string(completed), `"status": "completed"`, `"status": "`+status+`"`, 1)
				f.arr.SetJSON(http.MethodGet, "command/21", []byte(strings.Replace(ended, `"successful"`, `"unsuccessful"`, 1)))
				f.setEntries(t, partial)
				// The new command makes the backup (the fake answers it with id 21 again).
				f.runner.poll = 50 * time.Millisecond
				done := make(chan struct{})
				go func() {
					defer close(done)
					deadline := time.Now().Add(10 * time.Second)
					for f.commands() < 2 && time.Now().Before(deadline) {
						time.Sleep(time.Millisecond)
					}
					f.setEntries(t, partial, good)
					f.arr.SetJSON(http.MethodGet, "command/21", completed)
				}()
				job.Trigger = jobs.TriggerResume
				f.clock.Set(f.clock.Now().Add(time.Minute))
				res, err := f.run(t, job)
				<-done
				if err != nil {
					t.Fatalf("resume: %v\nlogs:\n%s", err, f.rep.text())
				}
				if f.commands() != 2 || !f.rep.has("stopped the Backup command") {
					t.Fatalf("%d commands\nlogs:\n%s", f.commands(), f.rep.text())
				}
				snaps := f.assertConverged(t, 1)
				if stats := res.Stats.(Stats); stats.BackupName != good.name || stats.SnapshotID != snaps[0].ID || stats.Integrity != IntegrityOK {
					t.Fatalf("stats %+v\nlogs:\n%s", stats, f.rep.text())
				}
			})
		}
	}
}

// TestRecoverAdoptsAnEarlierJobsVersionAfterARename: a job stopped after its version was complete
// and before its record, and was not resumed (cancelled while queued, or failed before its
// recovery); then the integration was renamed. The next job of the integration records that
// version in the folder of the old name: its manifest names a job of this integration in this
// database (TestRecoverLeavesOtherIntegrationsVersionsAlone: a lost database's job does not count).
func TestRecoverAdoptsAnEarlierJobsVersionAfterARename(t *testing.T) {
	f := newFixture(t)
	oldFolder := FolderName(f.integ.Name, f.integ.ID)
	job := f.newJob(t, false)
	f.crashRun(t, job, PointBeforeRecord)
	enabled := true
	it, err := f.ints.Update(f.ctx, f.integ.ID, integrations.Input{Type: f.integ.Type, Name: "Movies UHD", URL: f.integ.URL,
		Enabled: &enabled, Settings: f.integ.Settings})
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Set(f.clock.Now().Add(time.Hour))
	res := f.runOK(t)
	if res.Stats.(Stats).Recovered != 1 || res.Warnings != 0 || f.rep.has("left alone") {
		t.Fatalf("result %+v\nlogs:\n%s", res, f.rep.text())
	}
	var old, made int
	for _, s := range f.snapshots(t) {
		switch {
		case strings.HasPrefix(s.Path, oldFolder+"/") && s.Integrity == IntegrityOK:
			old++
		case strings.HasPrefix(s.Path, FolderName(it.Name, it.ID)+"/"):
			made++
		}
	}
	if old != 1 || made != 1 {
		t.Fatalf("snapshots %+v", f.snapshots(t))
	}
}
