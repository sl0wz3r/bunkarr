package arrbackup

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr"
	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/snapshots"
)

// manualName is the manual backup the recorded Backup command makes in the fixture.
const manualName = "radarr_backup_v6.4.4.10685_2026.09.25_12.30.35.zip"

func TestRunFolder(t *testing.T) {
	f := newFixture(t)
	job := f.newJob(t, false)
	res, err := f.run(t, job)
	if err != nil {
		t.Fatalf("run: %v\nlogs:\n%s", err, f.rep.text())
	}
	stats := res.Stats.(Stats)
	if res.Warnings != 0 || stats.Method != FetchFolder || stats.BackupName != manualName || stats.BackupType != arr.BackupManual ||
		stats.ReusedScheduled || stats.Unchanged || stats.Integrity != IntegrityOK || stats.SnapshotID == 0 || stats.Bytes != int64(len(f.entries[0].zip)) {
		t.Fatalf("result %+v", res)
	}
	if !strings.Contains(res.Summary, `"NAS"`) || !strings.Contains(res.Summary, "integrity ok") {
		t.Fatalf("summary %q", res.Summary)
	}
	snaps := f.assertConverged(t, 1)
	s := snaps[0]
	m := f.checkVersion(t, s)
	if s.Method != MethodFolder || s.JobID != job.ID || s.IntegrationID != f.integ.ID || m.Method != MethodFolder ||
		m.Backup.Name != manualName || m.Backup.Type != arr.BackupManual || !m.Backup.Time.Equal(cmdQueued) ||
		m.AppVersion != "6.4.4.10685" || m.Integrity != (ManifestIntegrity{Zip: IntegrityOK, Database: IntegrityOK}) ||
		len(m.Entries) != 2 || m.Entries[0].Name != ConfigXML || !m.JobQueuedAt.Equal(job.QueuedAt) {
		t.Fatalf("snapshot %+v, manifest %+v", s, m)
	}
	if !strings.HasPrefix(s.Path, ArrRoot+"/radarr-4k-") {
		t.Fatalf("path %s", s.Path)
	}
	// The folder method sends one Backup command and never downloads over HTTP.
	if f.commands() != 1 || f.downloads() != 0 {
		t.Fatalf("%d commands, %d downloads", f.commands(), f.downloads())
	}
	items, err := f.jobs.ListItems(f.ctx, job.ID, jobqueue.ItemQuery{})
	if err != nil || len(items.Records) != 1 || items.Records[0].Status != jobs.ItemDone || items.Records[0].Action != jobs.ActionBackup ||
		items.Records[0].RelPath != "manual/"+manualName {
		t.Fatalf("items %+v, %v", items, err)
	}
}

func TestRunHTTP(t *testing.T) {
	f := newFixture(t)
	f.httpSettings(t)
	res := f.runOK(t)
	stats := res.Stats.(Stats)
	if stats.Method != FetchHTTP || stats.Integrity != IntegrityOK {
		t.Fatalf("stats %+v", stats)
	}
	snaps := f.assertConverged(t, 1)
	if snaps[0].Method != MethodHTTP {
		t.Fatalf("snapshot %+v", snaps[0])
	}
	// The location comes from the type and name only, whatever the entry's path says (S18). The
	// first request is the 1-byte check made before the Backup command, the second the download.
	var got, ranges []string
	for _, r := range f.arr.Requests() {
		if strings.HasPrefix(r.Path, "/backup/") {
			got = append(got, r.Path)
			ranges = append(ranges, r.Header.Get("Range"))
			if r.Header.Get("X-Api-Key") != arrKey {
				t.Fatalf("the download was sent without the key header")
			}
		}
	}
	if len(got) != 2 || got[0] != "/backup/manual/"+manualName || got[1] != got[0] || ranges[0] != "bytes=0-0" || ranges[1] != "" {
		t.Fatalf("downloads %v, ranges %q", got, ranges)
	}
}

func TestRunNeverUsesTheBackupPath(t *testing.T) {
	f := newFixture(t)
	f.httpSettings(t)
	f.arr.SetBackupPath(manualName, "/../../elsewhere/secret.zip")
	f.runOK(t)
	for _, r := range f.arr.Requests() {
		if strings.Contains(r.Path, "elsewhere") {
			t.Fatalf("a request followed the backup's path field: %s", r.Path)
		}
	}
}

func TestRunHTTPLoginRequired(t *testing.T) {
	f := newFixture(t)
	f.httpSettings(t)
	f.arr.RequireLogin(true)
	_, err := f.run(t, f.newJob(t, false))
	var le *LoginRequiredError
	if !errors.As(err, &le) || !errors.Is(err, arr.ErrLoginRequired) {
		t.Fatalf("err = %v", err)
	}
	// The download was checked before a backup was asked for (it would stay in the *arr for good).
	if f.commands() != 0 {
		t.Fatalf("%d Backup commands before the login failure", f.commands())
	}
	const want = "Radarr requires a login to download backups: set its Backups folder in Bunkarr (Settings → Connect → Radarr) " +
		"or set Authentication Required to 'Disabled for Local Addresses' in Radarr"
	if err.Error() != want {
		t.Fatalf("message %q", err.Error())
	}
	f.assertConverged(t, 0)
}

func TestRunReusesAFreshScheduledBackup(t *testing.T) {
	f := newFixture(t)
	scheduled := entry{id: 7, name: "radarr_backup_v6.4.4.10685_2026.09.21_06.00.00.zip", typ: arr.BackupScheduled,
		time: f.clock.Now().Add(-4 * 24 * time.Hour), zip: goodZip(t)}
	old := entry{id: 3, name: "radarr_backup_v6.4.4.10685_2026.09.14_06.00.00.zip", typ: arr.BackupScheduled,
		time: f.clock.Now().Add(-11 * 24 * time.Hour), zip: goodZip(t)}
	f.setEntries(t, f.entries[0], old, scheduled)
	res := f.runOK(t)
	stats := res.Stats.(Stats)
	if f.commands() != 0 || !stats.ReusedScheduled || stats.Unchanged || stats.BackupName != scheduled.name || stats.BackupType != arr.BackupScheduled {
		t.Fatalf("%d commands, stats %+v", f.commands(), stats)
	}
	if !strings.Contains(res.Summary, "its own scheduled backup") {
		t.Fatalf("summary %q", res.Summary)
	}
	f.assertConverged(t, 1)

	// Copied already: the next job copies nothing.
	f.clock.Set(f.clock.Now().Add(time.Hour))
	job := f.newJob(t, false)
	res, err := f.run(t, job)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	stats = res.Stats.(Stats)
	if f.commands() != 0 || !stats.Unchanged || !stats.ReusedScheduled || stats.SnapshotID == 0 || res.Warnings != 0 {
		t.Fatalf("%d commands, result %+v", f.commands(), res)
	}
	f.assertConverged(t, 1)
	items, err := f.jobs.ListItems(f.ctx, job.ID, jobqueue.ItemQuery{})
	if err != nil || len(items.Records) != 1 || items.Records[0].Action != jobs.ActionSkip || items.Records[0].Status != jobs.ItemSkipped {
		t.Fatalf("items %+v, %v", items, err)
	}

	// Older than maxScheduledAgeDays (7): a new backup is made.
	f.clock.Set(scheduled.time.Add(7 * 24 * time.Hour))
	res = f.runOK(t)
	if f.commands() != 1 || res.Stats.(Stats).ReusedScheduled || res.Stats.(Stats).BackupName != manualName {
		t.Fatalf("%d commands, stats %+v", f.commands(), res.Stats)
	}
}

func TestRunMaxScheduledAgeDays(t *testing.T) {
	f := newFixture(t)
	f.setSettings(t, map[string]any{"backupFolder": f.backups, "backup": map[string]any{"destinationId": f.dest.ID, "maxScheduledAgeDays": 1}})
	f.setEntries(t, f.entries[0], entry{id: 9, name: "radarr_backup_v6.4.4.10685_2026.09.23_06.00.00.zip", typ: arr.BackupScheduled,
		time: f.clock.Now().Add(-36 * time.Hour), zip: goodZip(t)})
	res := f.runOK(t)
	if f.commands() != 1 || res.Stats.(Stats).ReusedScheduled {
		t.Fatalf("%d commands, stats %+v", f.commands(), res.Stats)
	}
}

func TestRunAFailedCopyOfTheScheduledBackupMakesANewOne(t *testing.T) {
	f := newFixture(t)
	scheduled := entry{id: 7, name: "radarr_backup_v6.4.4.10685_2026.09.24_06.00.00.zip", typ: arr.BackupScheduled,
		time: f.clock.Now().Add(-24 * time.Hour), zip: damagedDBZip(t)}
	f.setEntries(t, f.entries[0], scheduled)
	if _, err := f.run(t, f.newJob(t, false)); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("first run: %v", err)
	}
	f.clock.Set(f.clock.Now().Add(time.Hour))
	res := f.runOK(t)
	if f.commands() != 1 || res.Stats.(Stats).BackupName != manualName || res.Warnings == 0 {
		t.Fatalf("%d commands, result %+v\nlogs:\n%s", f.commands(), res, f.rep.text())
	}
}

func TestRunCommandPolling(t *testing.T) {
	f := newFixture(t)
	started := strings.Replace(string(fixtureCommand(t)), `"status": "completed"`, `"status": "started"`, 1)
	f.arr.SetJSON(http.MethodGet, "command/21", []byte(started))
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(50 * time.Millisecond)
		f.arr.SetJSON(http.MethodGet, "command/21", fixtureCommand(t))
	}()
	f.runOK(t)
	<-done
	polls := 0
	for _, r := range f.arr.Requests() {
		if strings.HasSuffix(r.Path, "/command/21") {
			polls++
		}
	}
	if polls < 2 {
		t.Fatalf("the command was polled %d times", polls)
	}
	f.assertConverged(t, 1)
}

// fixtureCommand is the recorded finished Backup command.
func fixtureCommand(t *testing.T) []byte {
	t.Helper()
	return mustFixture(t, "command-backup-get.json")
}

func TestRunCommandFailures(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, f *fixture)
		want  error
	}{
		{"unsuccessful", func(t *testing.T, f *fixture) {
			f.arr.SetJSON(http.MethodGet, "command/21", []byte(strings.Replace(string(fixtureCommand(t)), `"successful"`, `"unsuccessful"`, 1)))
		}, ErrCommand},
		{"failed", func(t *testing.T, f *fixture) {
			f.arr.SetJSON(http.MethodGet, "command/21", []byte(strings.Replace(string(fixtureCommand(t)), `"status": "completed"`, `"status": "failed"`, 1)))
		}, ErrCommand},
		{"never finishes", func(t *testing.T, f *fixture) {
			f.arr.SetJSON(http.MethodGet, "command/21", []byte(strings.Replace(string(fixtureCommand(t)), `"status": "completed"`, `"status": "started"`, 1)))
		}, ErrCommand},
		{"no backup listed", func(t *testing.T, f *fixture) {
			f.setEntries(t, entry{id: 2, name: "radarr_backup_v6.4.4.10685_2026.09.20_12.00.00.zip", typ: arr.BackupManual,
				time: cmdQueued.Add(-5 * 24 * time.Hour), zip: goodZip(t)})
		}, ErrNoBackup},
		{"the command endpoint fails", func(t *testing.T, f *fixture) {
			f.arr.SetStatus(http.MethodGet, "command/21", http.StatusInternalServerError)
		}, arr.ErrNotArr},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.runner.cmdTimeout = 200 * time.Millisecond
			tt.setup(t, f)
			_, err := f.run(t, f.newJob(t, false))
			if err == nil {
				t.Fatal("the job succeeded")
			}
			if tt.name == "the command endpoint fails" {
				if !strings.Contains(err.Error(), "follow Radarr's backup command") {
					t.Fatalf("err = %v", err)
				}
			} else if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			f.assertConverged(t, 0)
		})
	}
}

func TestRunFolderProblems(t *testing.T) {
	name := filepath.Join("manual", manualName)
	tests := []struct {
		name  string
		setup func(t *testing.T, f *fixture)
		want  string
	}{
		{"missing", func(t *testing.T, f *fixture) {
			if err := os.Remove(filepath.Join(f.backups, name)); err != nil {
				t.Fatal(err)
			}
		}, "is not in"},
		{"smaller for good", func(t *testing.T, f *fixture) {
			if err := os.Truncate(filepath.Join(f.backups, name), 100); err != nil {
				t.Fatal(err)
			}
		}, "has 100 of the"},
		{"larger", func(t *testing.T, f *fixture) {
			fh, err := os.OpenFile(filepath.Join(f.backups, name), os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = fh.Write([]byte("more"))
			_ = fh.Close()
		}, "more than the"},
		{"a symlink", func(t *testing.T, f *fixture) {
			p := filepath.Join(f.backups, name)
			if err := os.Rename(p, p+".real"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(p+".real", p); err != nil {
				t.Fatal(err)
			}
		}, "not a regular file"},
		{"no folder", func(t *testing.T, f *fixture) {
			if err := os.RemoveAll(f.backups); err != nil {
				t.Fatal(err)
			}
		}, "cannot be opened"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			tt.setup(t, f)
			_, err := f.run(t, f.newJob(t, false))
			if !errors.Is(err, ErrFolder) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			f.assertConverged(t, 0)
		})
	}
}

func TestRunFolderWaitsWhileTheFileGrows(t *testing.T) {
	f := newFixture(t)
	p := filepath.Join(f.backups, "manual", manualName)
	if err := os.WriteFile(p, f.entries[0].zip[:1000], 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(25 * time.Millisecond)
		_ = os.WriteFile(p, f.entries[0].zip, 0o600)
	}()
	f.runOK(t)
	<-done
	f.assertConverged(t, 1)
}

func TestRunInsecureModes(t *testing.T) {
	f := newFixture(t)
	f.setEnforcesModes(t, false)
	_, err := f.run(t, f.newJob(t, false))
	if !errors.Is(err, ErrInsecureModes) || !strings.Contains(err.Error(), "backup.acceptInsecureModes") {
		t.Fatalf("err = %v", err)
	}
	if f.commands() != 0 {
		t.Fatalf("a command was sent before the preflight failed")
	}
	f.assertConverged(t, 0)
	f.setSettings(t, map[string]any{"backupFolder": f.backups, "backup": map[string]any{"destinationId": f.dest.ID, "acceptInsecureModes": true}})
	f.runOK(t)
	f.assertConverged(t, 1)
}

func TestCheckModes(t *testing.T) {
	var caps destinations.Capabilities
	if err := CheckModes(caps, false, "NAS", "Radarr"); !errors.Is(err, ErrInsecureModes) || !strings.Contains(err.Error(), `"NAS"`) {
		t.Fatalf("CheckModes = %v", err)
	}
	if err := CheckModes(caps, true, "NAS", "Radarr"); err != nil {
		t.Fatal(err)
	}
	caps.EnforcesModes = true
	if err := CheckModes(caps, false, "NAS", "Radarr"); err != nil {
		t.Fatal(err)
	}
}

func TestRunDryRun(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T, f *fixture)
		would   string
		rel     string
		problem string
	}{
		{"create over the folder", func(*testing.T, *fixture) {}, "create", "manual/<new backup>", ""},
		{"create over HTTP", func(t *testing.T, f *fixture) { f.httpSettings(t) }, "create", "manual/<new backup>", ""},
		{"login required", func(t *testing.T, f *fixture) { f.httpSettings(t); f.arr.RequireLogin(true) }, "create", "manual/<new backup>",
			"requires a login"},
		{"no folder", func(t *testing.T, f *fixture) { _ = os.RemoveAll(f.backups) }, "create", "manual/<new backup>", "cannot be opened"},
		{"copy the scheduled backup", func(t *testing.T, f *fixture) {
			f.setEntries(t, entry{id: 5, name: "radarr_backup_v6.4.4.10685_2026.09.24_06.00.00.zip", typ: arr.BackupScheduled,
				time: f.clock.Now().Add(-time.Hour), zip: goodZip(t)})
		}, "copy-scheduled", "scheduled/radarr_backup_v6.4.4.10685_2026.09.24_06.00.00.zip", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			tt.setup(t, f)
			job := f.newJob(t, true)
			res, err := f.run(t, job)
			if err != nil {
				t.Fatalf("dry run: %v", err)
			}
			stats := res.Stats.(Stats)
			if !stats.DryRun || (tt.problem != "") != (res.Warnings > 0) {
				t.Fatalf("result %+v", res)
			}
			if f.commands() != 0 {
				t.Fatal("a dry run sent the Backup command (S9)")
			}
			items, err := f.jobs.ListItems(f.ctx, job.ID, jobqueue.ItemQuery{})
			if err != nil || len(items.Records) != 1 {
				t.Fatalf("items %+v, %v", items, err)
			}
			it := items.Records[0]
			var d itemDetail
			_ = json.Unmarshal(it.Detail, &d)
			if it.Action != jobs.ActionSkip || it.RelPath != tt.rel || d.Would != tt.would || d.Method != stats.Method ||
				!strings.Contains(d.Problem, tt.problem) || (tt.problem == "") != (it.Status == jobs.ItemSkipped) {
				t.Fatalf("item %+v, detail %+v", it, d)
			}
			// Nothing written: no version folder, no staging.
			if _, err := os.Stat(filepath.Join(f.target, filepath.FromSlash(ArrRoot))); !os.IsNotExist(err) {
				t.Fatalf("a dry run wrote at the destination: %v", err)
			}
			if _, err := os.Stat(filepath.Join(f.config, StagingRoot)); !os.IsNotExist(err) {
				t.Fatalf("a dry run staged: %v", err)
			}
		})
	}
}

func TestRunIntegrityFailure(t *testing.T) {
	f := newFixture(t)
	f.setEntries(t, entry{id: 1, name: manualName, typ: arr.BackupManual, time: cmdQueued, zip: damagedDBZip(t)})
	res, err := f.run(t, f.newJob(t, false))
	if !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), "quick_check") {
		t.Fatalf("err = %v", err)
	}
	stats := res.Stats.(Stats)
	snaps := f.snapshots(t)
	if len(snaps) != 1 || snaps[0].Integrity != IntegrityFailed || stats.Integrity != IntegrityFailed || stats.SnapshotID != snaps[0].ID {
		t.Fatalf("snapshots %+v, stats %+v", snaps, stats)
	}
	var m Manifest
	if err := json.Unmarshal(snaps[0].Manifest, &m); err != nil || m.Integrity != (ManifestIntegrity{Zip: IntegrityOK, Database: IntegrityFailed}) {
		t.Fatalf("manifest %+v, %v", m, err)
	}
	// Kept for diagnosis, pruned after FailedKeep.
	f.setEntries(t, entry{id: 1, name: manualName, typ: arr.BackupManual, time: cmdQueued, zip: goodZip(t)})
	f.clock.Set(f.clock.Now().Add(FailedKeep + time.Hour))
	f.runOK(t)
	for _, s := range f.snapshots(t) {
		if s.Integrity == IntegrityFailed {
			t.Fatalf("the failed version %s was not pruned after 7 days", s.Path)
		}
	}
	f.assertConverged(t, 1)
}

// TestRunSecretsNeverLeak: the key in the zip's config.xml (and the database's passwords) appear
// in no job log, manifest.json, snapshot row, item or job result (S17).
func TestRunSecretsNeverLeak(t *testing.T) {
	f := newFixture(t)
	m1 := jobqueue.New(f.db, nil, jobqueue.Options{Workers: 1})
	m1.Register(jobs.TypeArrBackup, f.runner)
	if err := m1.Start(f.ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m1.Stop(context.Background()) }()
	var finals []jobs.Job
	for _, dry := range []bool{true, false} {
		job, err := m1.Enqueue(f.ctx, jobs.Spec{Type: jobs.TypeArrBackup, DryRun: dry, Params: jobs.Params{IntegrationID: f.integ.ID}})
		if err != nil {
			t.Fatal(err)
		}
		finals = append(finals, waitFinal(t, f, job.ID))
	}
	var text []string
	for _, j := range finals {
		if j.Status != jobs.StatusCompleted {
			t.Fatalf("job %+v", j)
		}
		raw, _ := json.Marshal(j)
		text = append(text, string(raw))
		logs, err := f.jobs.ListLogs(f.ctx, j.ID, 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range logs {
			raw, _ := json.Marshal(l)
			text = append(text, string(raw))
		}
		items, err := f.jobs.ListItems(f.ctx, j.ID, jobqueue.ItemQuery{})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ = json.Marshal(items)
		text = append(text, string(raw))
	}
	for _, s := range f.snapshots(t) {
		raw, _ := json.Marshal(s)
		text = append(text, string(raw))
		disk, err := os.ReadFile(filepath.Join(f.target, filepath.FromSlash(s.Path), ManifestName))
		if err != nil {
			t.Fatal(err)
		}
		text = append(text, string(disk))
	}
	all := strings.Join(text, "\n")
	if strings.Contains(all, zipSecret) || strings.Contains(all, "downloadclientpassword") {
		t.Fatalf("a secret of the backup leaked:\n%s", all)
	}
	if !strings.Contains(all, manualName) {
		t.Fatalf("the check saw nothing:\n%s", all)
	}
}

// waitFinal waits until job id is final.
func waitFinal(t *testing.T, f *fixture, id int64) jobs.Job {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		j, err := f.jobs.GetJob(f.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if j.Status.Final() {
			return j
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %d did not finish: %+v", id, j)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRunPrunes(t *testing.T) {
	f := newFixture(t)
	f.setRetention(t, 2, 1)
	var made []time.Time
	for day := range 5 {
		f.clock.Set(cmdQueued.Add(30*time.Minute).AddDate(0, 0, day))
		res := f.runOK(t)
		made = append(made, f.clock.Now().UTC())
		want := int64(0)
		if day >= 2 {
			want = 1
		}
		if res.Stats.(Stats).VersionsPruned != want {
			t.Fatalf("day %d: pruned %d, want %d", day, res.Stats.(Stats).VersionsPruned, want)
		}
	}
	snaps := f.assertConverged(t, 2)
	if !snaps[0].CreatedAt.Equal(made[4]) || !snaps[1].CreatedAt.Equal(made[3]) {
		t.Fatalf("kept %v and %v", snaps[0].CreatedAt, snaps[1].CreatedAt)
	}
}

func TestRunPreflightFailures(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, f *fixture) jobs.Job
		want  string
	}{
		{"disabled integration", func(t *testing.T, f *fixture) jobs.Job {
			off := false
			if _, err := f.ints.Update(f.ctx, f.integ.ID, integrations.Input{Name: f.integ.Name, URL: f.integ.URL, Enabled: &off}); err != nil {
				t.Fatal(err)
			}
			return f.newJob(t, false)
		}, "is disabled"},
		{"disabled destination", func(t *testing.T, f *fixture) jobs.Job {
			off := false
			if _, err := f.dests.Update(f.ctx, f.dest.ID, destinations.Input{Enabled: &off}); err != nil {
				t.Fatal(err)
			}
			return f.newJob(t, false)
		}, `destination "NAS" is disabled`},
		{"no destination", func(t *testing.T, f *fixture) jobs.Job {
			f.setSettings(t, map[string]any{"backupFolder": f.backups})
			j := f.newJob(t, false)
			j.Params.DestinationID = 0
			return j
		}, "has no backup destination"},
		{"no integration id", func(t *testing.T, f *fixture) jobs.Job {
			j := f.newJob(t, false)
			j.Params.IntegrationID = 0
			return j
		}, "needs an integrationId"},
		{"a Plex integration", func(t *testing.T, f *fixture) jobs.Job {
			it, err := f.ints.Create(f.ctx, integrations.Input{Type: integrations.TypePlex, Name: "Plex", URL: "http://127.0.0.1:1"})
			if err != nil {
				t.Fatal(err)
			}
			j := f.newJob(t, false)
			j.Params.IntegrationID = it.ID
			return j
		}, "not Sonarr, Radarr or Lidarr"},
		{"another application", func(t *testing.T, f *fixture) jobs.Job {
			f.arr.SetFixture(http.MethodGet, "system/status", "system-status.json")
			var st map[string]any
			_ = json.Unmarshal(mustFixture(t, "system-status.json"), &st)
			st["appName"] = "Sonarr"
			raw, _ := json.Marshal(st)
			f.arr.SetJSON(http.MethodGet, "system/status", raw)
			return f.newJob(t, false)
		}, "not the expected application"},
		{"the destination is gone", func(t *testing.T, f *fixture) jobs.Job {
			if err := os.RemoveAll(filepath.Join(f.target, ".bunkarr")); err != nil {
				t.Fatal(err)
			}
			return f.newJob(t, false)
		}, "not mounted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			job := tt.setup(t, f)
			if _, err := f.run(t, job); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if f.commands() != 0 {
				t.Fatal("a command was sent")
			}
		})
	}
}

func mustFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "arr", "radarr", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRunDefaultsToTheIntegrationsDestination(t *testing.T) {
	f := newFixture(t)
	job := f.newJob(t, false)
	job.Params.DestinationID = 0
	if _, err := f.run(t, job); err != nil {
		t.Fatal(err)
	}
	f.assertConverged(t, 1)
}

func TestRunCancelled(t *testing.T) {
	f := newFixture(t)
	f.arr.SetJSON(http.MethodGet, "command/21", []byte(strings.Replace(string(fixtureCommand(t)), `"status": "completed"`, `"status": "started"`, 1)))
	ctx, cancel := context.WithCancel(f.ctx)
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	_, err := f.runner.Run(ctx, f.newJob(t, false), jobs.Env{Reporter: f.rep, Items: f.jobs})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	f.assertConverged(t, 0)
}

func TestRunWithoutManager(t *testing.T) {
	f := newFixture(t)
	if _, err := f.runner.Run(f.ctx, f.newJob(t, false), jobs.Env{}); err != nil {
		t.Fatal(err)
	}
	f.assertConverged(t, 1)
}

func TestRunIntegrationDeletedDuringTheBackup(t *testing.T) {
	f := newFixture(t)
	faultinject.SetHook(func(name string) {
		if name == PointBeforeRecord {
			if err := f.ints.Delete(f.ctx, f.integ.ID); err != nil {
				t.Error(err)
			}
		}
	})
	defer faultinject.SetHook(nil)
	res, err := f.run(t, f.newJob(t, false))
	if err != nil || res.Warnings != 1 {
		t.Fatalf("run: %+v, %v", res, err)
	}
	snaps := f.snapshots(t)
	if len(snaps) != 1 || snaps[0].IntegrationID != 0 {
		t.Fatalf("snapshots %+v", snaps)
	}
}

func TestCleanStaleStaging(t *testing.T) {
	f := newFixture(t)
	base := filepath.Join(f.config, StagingRoot)
	now := f.clock.Now()
	for name, age := range map[string]time.Duration{
		"arrbackup-job5": 48 * time.Hour, // stale: removed
		"arrbackup-job6": time.Hour,      // another job may still use it: kept
		"arrbackup-job7": 48 * time.Hour, // the running job's own: kept
		"plexdb-job8":    48 * time.Hour, // not ours: kept
		"arrbackup-jobx": 48 * time.Hour, // not a job's: kept
	} {
		p := filepath.Join(base, name)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	f.runner.cleanStaleStaging(7)
	es, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "arrbackup-job6,arrbackup-job7,arrbackup-jobx,plexdb-job8" {
		t.Fatalf("left %v", names)
	}
}

func TestFolderName(t *testing.T) {
	if got := FolderName("Radarr 4K", 3); got != ".bunkarr/arr/radarr-4k-3" {
		t.Fatalf("FolderName = %s", got)
	}
	if got := FolderName("!!!", 4); got != ".bunkarr/arr/arr-4" {
		t.Fatalf("FolderName = %s", got)
	}
}

// TestRunRecordsAnotherJobsOrphan: a job that crashed after its rename and failed for good leaves
// a complete, unrecorded version; the next job records it and makes its own. A damaged one is
// recorded as failed, never deleted without a record.
func TestRunRecordsAnotherJobsOrphan(t *testing.T) {
	f := newFixture(t)
	first := f.newJob(t, false)
	f.crashRun(t, first, PointAfterRename)
	_ = os.RemoveAll(f.runner.StagingDir(first.ID))
	f.clock.Set(f.clock.Now().Add(time.Hour))
	res := f.runOK(t)
	if res.Stats.(Stats).Recovered != 1 {
		t.Fatalf("stats %+v", res.Stats)
	}
	f.assertConverged(t, 2)

	third := f.newJob(t, false)
	f.crashRun(t, third, PointBeforeRecord)
	_ = os.RemoveAll(f.runner.StagingDir(third.ID))
	var orphan string
	recorded := map[string]bool{}
	for _, s := range f.snapshots(t) {
		recorded[filepath.Base(s.Path)] = true
	}
	for _, e := range f.dirEntries(t) {
		if !recorded[e] {
			orphan = e
		}
	}
	if err := os.WriteFile(filepath.Join(f.folderDir(), orphan, manualName), []byte("damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.clock.Set(f.clock.Now().Add(time.Hour))
	f.runOK(t)
	var failed []string
	for _, s := range f.snapshots(t) {
		if s.Integrity == IntegrityFailed {
			failed = append(failed, filepath.Base(s.Path))
		}
	}
	if len(failed) != 1 || failed[0] != orphan {
		t.Fatalf("failed versions %v, want [%s]", failed, orphan)
	}
}

// TestSnapshotKindIsolation: *arr recovery and pruning never touch Plex DB versions or rows of the
// same destination.
func TestSnapshotKindIsolation(t *testing.T) {
	f := newFixture(t)
	plexDir := filepath.Join(f.target, ".bunkarr", "plex", "plex-"+itoa(f.integ.ID), "20260101T000000Z")
	if err := os.MkdirAll(plexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := f.runner.Store().Insert(f.ctx, snapshots.Snapshot{DestinationID: f.dest.ID, Kind: snapshots.KindPlexDB, IntegrationID: f.integ.ID,
		Path: ".bunkarr/plex/plex-" + itoa(f.integ.ID) + "/20260101T000000Z", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Integrity: IntegrityOK}); err != nil {
		t.Fatal(err)
	}
	f.setRetention(t, 1, 1)
	for day := range 3 {
		f.clock.Set(cmdQueued.Add(30*time.Minute).AddDate(0, 0, day))
		f.runOK(t)
	}
	if _, err := os.Stat(plexDir); err != nil {
		t.Fatalf("the Plex version was touched: %v", err)
	}
	var plex int
	for _, s := range f.snapshots(t) {
		if s.Kind == snapshots.KindPlexDB {
			plex++
		}
	}
	if plex != 1 {
		t.Fatalf("%d Plex rows left", plex)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
