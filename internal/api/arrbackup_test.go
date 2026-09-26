package api

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite" // the "sqlite" driver the fixture database is written with

	"github.com/sl0wz3r/bunkarr/internal/arrbackup"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr/arrtest"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/snapshots"
)

const (
	backupTestKey = "radarr-backup-key-0123456789abcdef"
	// backupZipSecret is the key inside the fixture zip's config.xml: no response may contain it.
	backupZipSecret = "zip-secret-in-config-xml-77aa55"
	// backupName is the manual backup the recorded Backup command makes (its time is the
	// command's queued time).
	backupName = "radarr_backup_v6.4.4.10685_2026.09.25_12.30.35.zip"
)

// radarrBackupZip is a valid Radarr backup zip: config.xml with backupZipSecret and a small
// radarr.db.
func radarrBackupZip(t *testing.T) []byte {
	t.Helper()
	p := filepath.Join(t.TempDir(), "radarr.db")
	d, err := sql.Open("sqlite", "file:"+p+"?_pragma=journal_mode(delete)")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`CREATE TABLE Movies (Id INTEGER PRIMARY KEY, Title TEXT)`, `INSERT INTO Movies (Title) VALUES ('Heat')`} {
		if _, err := d.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	dbb, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for name, body := range map[string][]byte{"config.xml": []byte("<Config><ApiKey>" + backupZipSecret + "</ApiKey></Config>"), "radarr.db": dbb} {
		w, err := zw.Create(name)
		if err == nil {
			_, err = w.Write(body)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// backupSetup starts a fake Radarr that lists one manual backup (also written to a Backups
// folder), and a destination.
func backupSetup(t *testing.T, e *env) (fake *arrtest.Server, folder string, destID int64) {
	t.Helper()
	fake = arrtest.NewServer(t, arr.KindRadarr, backupTestKey)
	zipData := radarrBackupZip(t)
	fake.SetBackupZip(arr.BackupManual, backupName, zipData)
	fake.SetBackups([]map[string]any{{"id": 1, "name": backupName, "type": "manual", "size": len(zipData), "time": "2026-09-25T12:30:35Z"}})
	folder = e.mkdir(t, "radarr-backups")
	writeFile(t, filepath.Join(folder, "manual", backupName), string(zipData), mustTime(t, "2026-09-25T12:30:35Z"))
	destID = e.createDestination(t, "NAS", e.mkdir(t, "nas"), nil, nil)
	return fake, folder, destID
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

// createRadarr creates a Radarr integration with settings and returns it.
func (e *env) createRadarr(t *testing.T, url string, settings map[string]any) integrations.Integration {
	t.Helper()
	var it integrations.Integration
	e.call(t, 201, "POST", "/integrations", map[string]any{"type": "radarr", "name": "Radarr", "url": url, "apiKey": backupTestKey,
		"settings": settings}, &it)
	return it
}

// arrBackupSchedules returns the arr_backup schedules.
func (e *env) arrBackupSchedules(t *testing.T) []scheduleView {
	t.Helper()
	var list []scheduleView
	e.call(t, 200, "GET", "/schedules", nil, &list)
	var out []scheduleView
	for _, sc := range list {
		if sc.JobType == jobs.TypeArrBackup {
			out = append(out, sc)
		}
	}
	return out
}

func TestArrBackupEndpoints(t *testing.T) {
	e := newEnv(t, nil)
	fake, folder, destID := backupSetup(t, e)
	it := e.createRadarr(t, fake.URL, map[string]any{"backupFolder": folder, "backup": map[string]any{"destinationId": destID, "enabled": true}})

	// The schedule mirrors the settings (weekly by default) and is shown back in the settings.
	scheds := e.arrBackupSchedules(t)
	if len(scheds) != 1 || scheds[0].Cron != integrations.DefaultArrBackupCron || !scheds[0].Enabled ||
		!sameParams(scheds[0].Params, jobs.Params{IntegrationID: it.ID, DestinationID: destID}) || scheds[0].Description != "Backup of Radarr to NAS" {
		t.Fatalf("schedules %+v", scheds)
	}
	e.call(t, 200, "PUT", fmt.Sprintf("/schedules/%d", scheds[0].ID), map[string]any{"cron": "0 7 * * 1"}, nil)
	var got integrations.Integration
	e.call(t, 200, "GET", fmt.Sprintf("/integrations/%d", it.ID), nil, &got)
	as, err := got.ArrSettings()
	if err != nil || as.Backup.Cron != "0 7 * * 1" || !as.Backup.Enabled || as.BackupFolder != folder {
		t.Fatalf("settings %+v, %v", as, err)
	}

	// A dry run sends no command and records nothing.
	var job jobs.Job
	e.call(t, 202, "POST", fmt.Sprintf("/integrations/%d/arr/backup", it.ID), map[string]any{"dryRun": true}, &job)
	if j := e.waitJob(t, job.ID); j.Status != jobs.StatusCompleted || !j.DryRun {
		t.Fatalf("dry run %+v", j)
	}
	for _, r := range fake.Requests() {
		if r.Method == "POST" {
			t.Fatalf("the dry run sent %s %s", r.Method, r.Path)
		}
	}

	// Back up now.
	e.call(t, 202, "POST", fmt.Sprintf("/integrations/%d/arr/backup", it.ID), nil, &job)
	if !sameParams(job.Params, jobs.Params{IntegrationID: it.ID, DestinationID: destID}) {
		t.Fatalf("job params %+v", job.Params)
	}
	j := e.waitJob(t, job.ID)
	if j.Status != jobs.StatusCompleted || !strings.Contains(string(j.Stats), `"method":"folder"`) {
		t.Fatalf("backup %+v %s", j, j.Stats)
	}

	code, raw := e.raw(t, "GET", fmt.Sprintf("/integrations/%d/arr/snapshots", it.ID), nil)
	var snaps []snapshots.Snapshot
	if code != 200 || json.Unmarshal(raw, &snaps) != nil || len(snaps) != 1 || snaps[0].Kind != snapshots.KindArr ||
		snaps[0].Integrity != "ok" || snaps[0].Method != arrbackup.MethodFolder || snaps[0].DestinationID != destID {
		t.Fatalf("GET arr/snapshots: %d %s", code, raw)
	}
	code, raw2 := e.raw(t, "GET", fmt.Sprintf("/destinations/%d/snapshots", destID), nil)
	if code != 200 || !strings.Contains(string(raw2), `"kind":"arr"`) {
		t.Fatalf("GET destination snapshots: %d %s", code, raw2)
	}
	for _, body := range [][]byte{raw, raw2} {
		if strings.Contains(string(body), backupZipSecret) || strings.Contains(string(body), backupTestKey) {
			t.Fatalf("a response holds a secret: %s", body)
		}
	}
}

func TestArrBackupRefusals(t *testing.T) {
	e := newEnv(t, nil)
	fake, folder, destID := backupSetup(t, e)
	noDest := e.createRadarr(t, fake.URL, map[string]any{"backupFolder": folder})
	var plexIt integrations.Integration
	e.call(t, 201, "POST", "/integrations", map[string]any{"type": "plex", "name": "Plex", "url": "http://127.0.0.1:9"}, &plexIt)

	for _, tt := range []struct {
		path string
		body any
		want int
		msg  string
	}{
		{fmt.Sprintf("/integrations/%d/arr/backup", noDest.ID), nil, 400, "choose a destination"},
		{fmt.Sprintf("/integrations/%d/arr/backup", noDest.ID), map[string]any{"destinationId": 999}, 400, "does not exist"},
		{fmt.Sprintf("/integrations/%d/arr/backup", plexIt.ID), nil, 400, "not Sonarr, Radarr or Lidarr"},
		{fmt.Sprintf("/integrations/%d/arr/snapshots", plexIt.ID), nil, 400, "not Sonarr, Radarr or Lidarr"},
		{"/integrations/999/arr/backup", nil, 404, ""},
	} {
		method := "POST"
		if strings.HasSuffix(tt.path, "snapshots") {
			method = "GET"
		}
		if code, msg := e.status(t, method, tt.path, tt.body); code != tt.want || !strings.Contains(msg, tt.msg) {
			t.Errorf("%s %s: %d %q, want %d %q", method, tt.path, code, msg, tt.want, tt.msg)
		}
	}

	// A destination that does not keep files private: saving it as the backup destination is a
	// 400 naming the flag, and so is a manual backup there (409); with the flag both work.
	setEnforcesModes(t, e, destID, false)
	code, msg := e.status(t, "PUT", fmt.Sprintf("/integrations/%d", noDest.ID), map[string]any{"name": "Radarr", "url": fake.URL,
		"settings": map[string]any{"backupFolder": folder, "backup": map[string]any{"destinationId": destID}}})
	if code != 400 || !strings.Contains(msg, "backup.acceptInsecureModes") {
		t.Fatalf("save to an insecure destination: %d %q", code, msg)
	}
	if code, msg := e.status(t, "POST", fmt.Sprintf("/integrations/%d/arr/backup", noDest.ID), map[string]any{"destinationId": destID}); code != 409 ||
		!strings.Contains(msg, "backup.acceptInsecureModes") {
		t.Fatalf("back up to an insecure destination: %d %q", code, msg)
	}
	e.call(t, 200, "PUT", fmt.Sprintf("/integrations/%d", noDest.ID), map[string]any{"name": "Radarr", "url": fake.URL,
		"settings": map[string]any{"backupFolder": folder, "backup": map[string]any{"destinationId": destID, "acceptInsecureModes": true}}}, nil)
	var job jobs.Job
	e.call(t, 202, "POST", fmt.Sprintf("/integrations/%d/arr/backup", noDest.ID), map[string]any{"dryRun": true}, &job)
	e.waitJob(t, job.ID)

	// Disabled integration: 409.
	e.call(t, 200, "PUT", fmt.Sprintf("/integrations/%d", noDest.ID), map[string]any{"name": "Radarr", "url": fake.URL, "enabled": false}, nil)
	if code, msg := e.status(t, "POST", fmt.Sprintf("/integrations/%d/arr/backup", noDest.ID), nil); code != 409 || !strings.Contains(msg, "disabled") {
		t.Fatalf("disabled: %d %q", code, msg)
	}
	// A missing backup destination in the settings: 400.
	if code, msg := e.status(t, "PUT", fmt.Sprintf("/integrations/%d", noDest.ID), map[string]any{"name": "Radarr", "url": fake.URL,
		"settings": map[string]any{"backup": map[string]any{"destinationId": 777}}}); code != 400 || !strings.Contains(msg, "does not exist") {
		t.Fatalf("missing destination: %d %q", code, msg)
	}
}

// setEnforcesModes stores capabilities.enforcesModes for a destination (as a probe of an SMB share
// without POSIX extensions, or a probe older than the field, would).
func setEnforcesModes(t *testing.T, e *env, destID int64, on bool) {
	t.Helper()
	d, err := e.app.Destinations.Get(context.Background(), destID)
	if err != nil {
		t.Fatal(err)
	}
	caps := d.Capabilities
	caps.EnforcesModes = on
	raw, _ := json.Marshal(caps)
	if err := e.db.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(context.Background(), `UPDATE destinations SET capabilities = ? WHERE id = ?`, string(raw), destID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestArrBackupScheduleFollowsSettings(t *testing.T) {
	e := newEnv(t, nil)
	fake, folder, destID := backupSetup(t, e)
	other := e.createDestination(t, "USB", e.mkdir(t, "usb"), nil, nil)
	it := e.createRadarr(t, fake.URL, map[string]any{"backupFolder": folder})
	if s := e.arrBackupSchedules(t); len(s) != 0 {
		t.Fatalf("a backup without a destination has schedules %+v", s)
	}
	put := func(backup map[string]any) {
		t.Helper()
		e.call(t, 200, "PUT", fmt.Sprintf("/integrations/%d", it.ID), map[string]any{"name": "Radarr", "url": fake.URL,
			"settings": map[string]any{"backupFolder": folder, "backup": backup}}, nil)
	}
	put(map[string]any{"destinationId": destID, "enabled": true, "cron": "0 3 * * *"})
	if s := e.arrBackupSchedules(t); len(s) != 1 || s[0].Params.DestinationID != destID || s[0].Cron != "0 3 * * *" || !s[0].Enabled {
		t.Fatalf("schedules %+v", s)
	}
	// Another destination moves the schedule.
	put(map[string]any{"destinationId": other, "enabled": true, "cron": "0 3 * * *"})
	if s := e.arrBackupSchedules(t); len(s) != 1 || s[0].Params.DestinationID != other || s[0].Description != "Backup of Radarr to USB" {
		t.Fatalf("schedules %+v", s)
	}
	// Disabled with a cron: kept, disabled. Disabled without one: removed.
	put(map[string]any{"destinationId": other, "enabled": false, "cron": "0 3 * * *"})
	if s := e.arrBackupSchedules(t); len(s) != 1 || s[0].Enabled {
		t.Fatalf("schedules %+v", s)
	}
	put(map[string]any{"destinationId": other})
	if s := e.arrBackupSchedules(t); len(s) != 0 {
		t.Fatalf("schedules %+v", s)
	}
	// Deleting the integration removes its schedules.
	put(map[string]any{"destinationId": other, "enabled": true})
	e.call(t, 204, "DELETE", fmt.Sprintf("/integrations/%d", it.ID), nil, nil)
	if s := e.arrBackupSchedules(t); len(s) != 0 {
		t.Fatalf("schedules after the delete %+v", s)
	}
}

func TestDescribeArrBackup(t *testing.T) {
	e := newEnv(t, nil)
	fake, _, destID := backupSetup(t, e)
	it := e.createRadarr(t, fake.URL, nil)
	ctx := context.Background()
	if got := e.app.describe(ctx, jobs.TypeArrBackup, jobs.Params{IntegrationID: it.ID}); got != "Backup of Radarr" {
		t.Errorf("describe = %q", got)
	}
	e.call(t, 200, "PUT", fmt.Sprintf("/integrations/%d", it.ID), map[string]any{"name": "Radarr 4K", "url": fake.URL}, nil)
	if got := e.app.describe(ctx, jobs.TypeArrBackup, jobs.Params{IntegrationID: it.ID, DestinationID: destID}); got != "Backup of Radarr Radarr 4K to NAS" {
		t.Errorf("describe = %q", got)
	}
	if got := e.app.describe(ctx, jobs.TypeArrBackup, jobs.Params{IntegrationID: 999}); got != "Backup of *arr *arr #999" {
		t.Errorf("describe = %q", got)
	}
}
