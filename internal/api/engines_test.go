package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/enginetest"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// allowFailingCommands makes every restic and rclone command fail at once (exit 1): the jobs a
// test queues without caring how they end (dry runs of engine destinations) then fail quickly
// instead of failing the test on an unexpected command.
func (ee *engineEnv) allowFailingCommands() {
	ee.runner.Expect(proc.Restic, enginetest.AnyArgs(), enginetest.Script{Exit: 1}).AnyTimes()
	ee.runner.Expect(proc.Rclone, enginetest.AnyArgs(), enginetest.Script{Exit: 1}).AnyTimes()
}

// attachedRestic creates a restic destination on S3 by attaching an existing repository with the
// user's password: its custody counts as confirmed at once (§5.2).
func (ee *engineEnv) attachedRestic(t *testing.T, name, prefix string) int64 {
	t.Helper()
	body := s3Body(name, "restic", "media", prefix)
	body["attach"] = true
	body["encryption"] = map[string]any{"mode": "restic", "generate": false, "secret": testUserSecret}
	return ee.createEngineDest(t, ee.session(t), body)
}

// TestEngineJobGate: the jobs of an engine destination whose recovery kit custody is not confirmed
// are refused, manual ones with 409 and scheduled ones by the scheduler (the schedule shows the
// reason); dry runs run (S21, §11.1). An engine that is not available blocks the same way.
func TestEngineJobGate(t *testing.T) {
	ee := newEngineEnv(t, engineEnvOptions{})
	ee.allowFailingCommands()
	c := ee.session(t)
	id := ee.createEngineDest(t, c, s3Body("Offsite", "restic", "media", "gate"))
	var plex struct {
		ID int64 `json:"id"`
	}
	ee.call(t, 201, "POST", "/integrations", map[string]any{"type": "plex", "name": "Plex", "url": "http://127.0.0.1:9"}, &plex)
	ee.sessionCall(t, c, 200, "PUT", fmt.Sprintf("/integrations/%d", plex.ID), map[string]any{"name": "Plex", "url": "http://127.0.0.1:9",
		"currentPassword": sessionPassword, "settings": map[string]any{"dataPath": "/plex",
			"backup": map[string]any{"targets": []any{map[string]any{"destinationId": id, "cron": "0 6 * * *", "enabled": true}}}}}, nil)

	base := fmt.Sprintf("/destinations/%d", id)
	for _, p := range []string{base + "/sync", base + "/verify", base + "/retention", base + "/manifest",
		fmt.Sprintf("/integrations/%d/plex/backup", plex.ID)} {
		if code, msg := ee.status(t, "POST", p, nil); code != 409 || !strings.Contains(msg, destinations.BlockedKit) {
			t.Errorf("POST %s before the kit is confirmed: %d %q", p, code, msg)
		}
	}
	for _, p := range []string{base + "/sync", base + "/retention"} {
		var j jobs.Job
		ee.call(t, 202, "POST", p, map[string]any{"dryRun": true}, &j)
		if !j.DryRun {
			t.Errorf("POST %s dry run: %+v", p, j)
		}
		ee.waitJob(t, j.ID)
	}

	var list []scheduleView
	ee.call(t, 200, "GET", "/schedules", nil, &list)
	seen := map[jobs.Type]bool{}
	for _, sc := range list {
		if sc.Params.DestinationID != id {
			continue
		}
		seen[sc.JobType] = true
		if !strings.Contains(sc.BlockedReason, destinations.BlockedKit) || sc.NextRunAt != nil {
			t.Errorf("schedule %s: blockedReason %q, nextRunAt %v", sc.JobType, sc.BlockedReason, sc.NextRunAt)
		}
		if code, msg := ee.status(t, "POST", fmt.Sprintf("/schedules/%d/run", sc.ID), nil); code != 409 || !strings.Contains(msg, destinations.BlockedKit) {
			t.Errorf("run schedule %s: %d %q", sc.JobType, code, msg)
		}
		if sc.JobType == jobs.TypeRetention {
			var j jobs.Job
			ee.call(t, 202, "POST", fmt.Sprintf("/schedules/%d/run", sc.ID), map[string]any{"dryRun": true}, &j)
			ee.waitJob(t, j.ID)
		}
	}
	if !seen[jobs.TypeVerify] || !seen[jobs.TypeRetention] || !seen[jobs.TypePlexDBBackup] {
		t.Fatalf("schedules of the destination: %v", seen)
	}
	_, err := scheduleGate{a: ee.app}.Enqueue(context.Background(), jobs.Spec{Type: jobs.TypeSync, Trigger: jobs.TriggerSchedule,
		Params: jobs.Params{DestinationID: id}})
	if err == nil || !strings.Contains(err.Error(), destinations.BlockedKit) {
		t.Fatalf("a scheduled sync before the kit is confirmed: %v", err)
	}

	// An engine that is not available.
	restic := engines.BinaryStatus{Reason: "restic is not installed"}
	ue := newEngineEnv(t, engineEnvOptions{availability: &engines.Availability{Restic: restic,
		Rclone: engines.BinaryStatus{Available: true, Version: "1.74.1"}}})
	uid := ue.attachedRestic(t, "No restic", "none")
	var v destinationView
	ue.call(t, 200, "GET", fmt.Sprintf("/destinations/%d", uid), nil, &v)
	if v.BlockedReason != restic.Reason || v.EngineVersion != "" || v.Encryption.KitConfirmedAt == nil {
		t.Fatalf("destination of an unavailable engine: %+v", v)
	}
	if code, msg := ue.status(t, "POST", fmt.Sprintf("/destinations/%d/sync", uid), nil); code != 409 || !strings.Contains(msg, restic.Reason) {
		t.Fatalf("sync with restic unavailable: %d %q", code, msg)
	}
	var st SystemStatus
	ue.call(t, 200, "GET", "/system/status", nil, &st)
	if st.Engines.Restic.Available || st.Engines.Restic.Reason != restic.Reason || !st.Engines.Rclone.Available || st.Engines.Rclone.Version != "1.74.1" {
		t.Fatalf("system status engines: %+v", st.Engines)
	}
}

// TestSystemStatusWithoutEngines: without a runner neither engine is available, with a reason.
func TestSystemStatusWithoutEngines(t *testing.T) {
	e := newEnv(t, nil)
	var raw map[string]json.RawMessage
	e.call(t, 200, "GET", "/system/status", nil, &raw)
	var got engines.Availability
	if err := json.Unmarshal(raw["engines"], &got); err != nil {
		t.Fatalf("engines: %s: %v", raw["engines"], err)
	}
	if got.Restic.Available || got.Rclone.Available || got.Restic.Reason != "restic is not installed" || got.Rclone.Reason != "rclone is not installed" {
		t.Fatalf("engines: %+v", got)
	}
	// Engine destinations cannot be created (400).
	c := e.session(t)
	if code, raw, _ := e.sessionRaw(t, c, "POST", "/destinations", withPassword(s3Body("X", "restic", "b", ""), sessionPassword)); code != 400 ||
		!strings.Contains(string(raw), "not installed") {
		t.Fatalf("create without engines: %d %s", code, raw)
	}
}

// TestEngineSettings checks GET and PUT /settings/engines: the defaults, the ranges, and that a
// changed slot count applies after a restart.
func TestEngineSettings(t *testing.T) {
	e := newEnv(t, nil)
	var s engineSettings
	e.call(t, 200, "GET", "/settings/engines", nil, &s)
	if s.UploadSlots != 2 || s.RetryBudgetMinutes != 10 || s.UploadSlotsInEffect != 2 || s.RestartRequired || s.Note == "" {
		t.Fatalf("defaults: %+v", s)
	}
	e.call(t, 200, "PUT", "/settings/engines", map[string]any{"uploadSlots": 4, "retryBudgetMinutes": 30}, &s)
	if s.UploadSlots != 4 || s.RetryBudgetMinutes != 30 || s.UploadSlotsInEffect != 2 || !s.RestartRequired || !strings.Contains(s.Note, "restart") {
		t.Fatalf("after PUT: %+v", s)
	}
	if got := e.app.retryBudget(context.Background()); got != 30*time.Minute {
		t.Fatalf("retry budget %s", got)
	}
	e.call(t, 200, "PUT", "/settings/engines", map[string]any{"retryBudgetMinutes": 5}, &s)
	if s.UploadSlots != 4 || s.RetryBudgetMinutes != 5 {
		t.Fatalf("partial PUT: %+v", s)
	}
	for _, body := range []any{
		map[string]any{"uploadSlots": 0}, map[string]any{"uploadSlots": 9}, map[string]any{"retryBudgetMinutes": 0},
		map[string]any{"retryBudgetMinutes": 1441}, map[string]any{"resticPath": "/tmp/evil"}, `{"uploadSlots": "2"}`,
	} {
		if code, msg := e.status(t, "PUT", "/settings/engines", body); code != 400 {
			t.Errorf("PUT %v: %d %q", body, code, msg)
		}
	}
	// The slot count in effect is read at start-up.
	e.app.engines.uploadSlots = 0
	pool := e.app.uploadSlotPool(context.Background())
	if pool.Limit != 4 || pool.Name != "upload" {
		t.Fatalf("slot pool %+v", pool)
	}
}

// TestUploadSlotPoolNeeds: engine syncs that are not dry runs need an upload slot; filecopy syncs,
// dry runs and other job types do not (§9.3).
func TestUploadSlotPoolNeeds(t *testing.T) {
	ee := newEngineEnv(t, engineEnvOptions{})
	engineID := ee.attachedRestic(t, "Offsite", "slots")
	fileID := ee.createDestination(t, "NAS", ee.mkdir(t, "nas"), nil, nil)
	pool := ee.app.uploadSlotPool(context.Background())
	for _, tt := range []struct {
		job  jobs.Job
		want bool
	}{
		{jobs.Job{Type: jobs.TypeSync, Params: jobs.Params{DestinationID: engineID}}, true},
		{jobs.Job{Type: jobs.TypeSync, DryRun: true, Params: jobs.Params{DestinationID: engineID}}, false},
		{jobs.Job{Type: jobs.TypeVerify, Params: jobs.Params{DestinationID: engineID}}, false},
		{jobs.Job{Type: jobs.TypePlexDBBackup, Params: jobs.Params{DestinationID: engineID, IntegrationID: 1}}, false},
		{jobs.Job{Type: jobs.TypeSync, Params: jobs.Params{DestinationID: fileID}}, false},
		{jobs.Job{Type: jobs.TypeSync, Params: jobs.Params{DestinationID: 999}}, false},
	} {
		got, err := pool.Needs(context.Background(), tt.job)
		if err != nil || got != tt.want {
			t.Errorf("%s dry %v of %d: %v %v, want %v", tt.job.Type, tt.job.DryRun, tt.job.Params.DestinationID, got, err, tt.want)
		}
	}
}

// TestUnlockDestination: POST /destinations/{id}/unlock is refused while any job of the
// destination runs (a plexdb_backup included, which holds no dest:<id> key), runs restic unlock
// --remove-all after the identity check otherwise, and applies to restic destinations only.
func TestUnlockDestination(t *testing.T) {
	ee := newEngineEnv(t, engineEnvOptions{})
	id := ee.attachedRestic(t, "Offsite", "unlock")
	d, err := ee.app.Destinations.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/destinations/%d/unlock", id)

	// A running Plex DB backup to the destination.
	var jobID int64
	now := db.FormatTime(time.Now())
	if err := ee.db.Write(context.Background(), func(tx *sql.Tx) error {
		return tx.QueryRow(`INSERT INTO jobs (type, status, trigger, params, destination_id, integration_id, queued_at, started_at)
			VALUES ('plexdb_backup', 'running', 'manual', ?, ?, 1, ?, ?) RETURNING id`,
			fmt.Sprintf(`{"integrationId":1,"destinationId":%d}`, id), id, now, now).Scan(&jobID)
	}); err != nil {
		t.Fatal(err)
	}
	if code, msg := ee.status(t, "POST", path, map[string]any{"removeAll": true}); code != 409 || !strings.Contains(msg, "running") {
		t.Fatalf("unlock during a plexdb_backup: %d %q", code, msg)
	}
	if n := len(ee.runner.Calls()); n != 0 {
		t.Fatalf("%d commands ran for a refused unlock", n)
	}
	if err := ee.db.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE jobs SET status = 'completed', finished_at = ? WHERE id = ?`, now, jobID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	repoID := strings.TrimPrefix(d.MarkerID, "restic:")
	ee.runner.Expect(proc.Restic, enginetest.Prefix("cat", "config"),
		enginetest.Script{Stdout: []string{fmt.Sprintf(`{"version":2,"id":%q,"chunker_polynomial":"3dd3fbd3fea3a5"}`, repoID)}})
	unlock := ee.runner.Expect(proc.Restic, enginetest.Prefix("unlock"), enginetest.Script{})
	ee.call(t, 204, "POST", path, map[string]any{"removeAll": true}, nil)
	calls := ee.runner.CallsOf(proc.Restic, "unlock")
	if unlock.Calls() != 1 || len(calls) != 1 || !calls[0].Has("--remove-all") {
		t.Fatalf("unlock commands: %v", calls)
	}
	cats := ee.runner.CallsOf(proc.Restic, "cat")
	if len(cats) != 1 || !cats[0].Has("--no-lock") {
		t.Fatalf("identity check before the unlock: %v", cats)
	}

	// Only restic destinations have locks.
	nas := ee.createDestination(t, "NAS", ee.mkdir(t, "nas"), nil, nil)
	if code, _ := ee.status(t, "POST", fmt.Sprintf("/destinations/%d/unlock", nas), nil); code != 400 {
		t.Fatalf("unlock of a filecopy destination: %d", code)
	}
	if code, _ := ee.status(t, "POST", "/destinations/999/unlock", nil); code != 404 {
		t.Fatalf("unlock of a missing destination: %d", code)
	}
}

// TestRetentionAndVerifyEndpoints: POST /destinations/{id}/retention {dryRun, prune} and POST
// /destinations/{id}/verify {readData} queue their jobs with the new params.
func TestRetentionAndVerifyEndpoints(t *testing.T) {
	ee := newEngineEnv(t, engineEnvOptions{})
	ee.allowFailingCommands()
	nas := ee.createDestination(t, "NAS", ee.mkdir(t, "nas"), nil, nil)
	var j jobs.Job
	ee.call(t, 202, "POST", fmt.Sprintf("/destinations/%d/retention", nas), nil, &j)
	if j.Type != jobs.TypeRetention || j.Params.DestinationID != nas || j.Params.Prune || j.DryRun {
		t.Fatalf("retention job: %+v", j)
	}
	if done := ee.waitJob(t, j.ID); done.Status != jobs.StatusCompleted {
		t.Fatalf("retention of a filecopy destination: %s %q", done.Status, done.Error)
	}
	if code, msg := ee.status(t, "POST", fmt.Sprintf("/destinations/%d/retention", nas), map[string]any{"prune": true}); code != 400 {
		t.Fatalf("prune of a filecopy destination: %d %q", code, msg)
	}
	ee.call(t, 202, "POST", fmt.Sprintf("/destinations/%d/verify", nas), map[string]any{"readData": true}, &j)
	if j.Type != jobs.TypeVerify || !j.Params.ReadData {
		t.Fatalf("verify job: %+v", j)
	}
	ee.waitJob(t, j.ID)

	restic := ee.attachedRestic(t, "Offsite", "ret")
	ee.call(t, 202, "POST", fmt.Sprintf("/destinations/%d/retention", restic), map[string]any{"dryRun": true, "prune": true}, &j)
	if !j.DryRun || !j.Params.Prune || j.Params.DestinationID != restic {
		t.Fatalf("restic retention job: %+v", j)
	}
	ee.waitJob(t, j.ID)
	for _, body := range []any{map[string]any{"prune": "yes"}, map[string]any{"sourceIds": []int{1}}} {
		if code, _ := ee.status(t, "POST", fmt.Sprintf("/destinations/%d/retention", restic), body); code != 400 {
			t.Errorf("retention %v: %d", body, code)
		}
	}
	ee.call(t, 200, "PUT", fmt.Sprintf("/destinations/%d", restic), map[string]any{"enabled": false}, nil)
	if code, _ := ee.status(t, "POST", fmt.Sprintf("/destinations/%d/retention", restic), map[string]any{"dryRun": true}); code != 409 {
		t.Fatalf("retention of a disabled destination: %d", code)
	}
}

// TestDestinationTestRoutes: POST /destinations/{id}/test of an engine destination takes no
// connection fields (400, and nothing runs) and tests the stored location; POST
// /destinations/test of an SFTP location without pinned host keys answers the presented keys and
// runs no engine command (the typed password never reaches an unverified host); the host-key scan
// never authenticates.
func TestDestinationTestRoutes(t *testing.T) {
	ee := newEngineEnv(t, engineEnvOptions{})
	c := ee.session(t)
	id := ee.createEngineDest(t, c, s3Body("Offsite", "restic", "media", "test"))
	path := fmt.Sprintf("/destinations/%d/test", id)
	calls := ee.engineCalls()
	for _, body := range []map[string]any{
		{"remote": map[string]any{"bucket": "evil"}},
		{"credentials": map[string]any{"accessKeyId": "x"}},
		{"target": "/tmp"},
		{"encryption": map[string]any{"mode": "none"}},
	} {
		if code, msg := ee.status(t, "POST", path, body); code != 400 || msg != msgTestWithoutConnection {
			t.Errorf("test with %v: %d %q", body, code, msg)
		}
	}
	if code, _ := ee.status(t, "POST", path, map[string]any{"verbose": true}); code != 400 {
		t.Fatalf("test with an unknown field: %d", code)
	}
	if n := ee.engineCalls(); n != calls || len(ee.runner.Calls()) != 0 {
		t.Fatalf("connection fields reached an engine: %d engine calls, %d commands", n-calls, len(ee.runner.Calls()))
	}
	var res engines.TestResult
	ee.call(t, 200, "POST", path, nil, &res)
	last := ee.restic.Calls()[len(ee.restic.Calls())-1]
	if !res.OK || last.Method != "Test" || last.Dest.ID != id || last.Dest.Remote.S3 == nil || last.Dest.Remote.S3.Bucket != "media" {
		t.Fatalf("test of the stored destination: %+v, call %+v", res, last)
	}

	// SFTP without pinned keys: the server's keys, no engine command, no authentication.
	key, signer := hostKeyPair(t)
	host, port, attempts := sshServer(t, signer)
	calls = ee.engineCalls()
	body := map[string]any{"kind": "sftp", "engine": "rclone", "remote": map[string]any{"host": host, "port": port, "user": "backup", "path": "/b"},
		"credentials": map[string]any{"password": testSFTPPass}}
	code, raw := ee.raw(t, "POST", "/destinations/test", body)
	if code != 200 {
		t.Fatalf("test of an SFTP location: %d %s", code, raw)
	}
	if err := json.Unmarshal(raw, &res); err != nil || res.OK || len(res.HostKeys) != 1 || res.HostKeys[0].Key != key.Key ||
		!strings.HasPrefix(res.HostKeys[0].Fingerprint, "SHA256:") {
		t.Fatalf("SFTP test without host keys: %s", raw)
	}
	if strings.Contains(string(raw), testSFTPPass) {
		t.Fatal("the answer holds the typed password")
	}
	if n := ee.engineCalls(); n != calls || len(ee.runner.Calls()) != 0 || attempts() != 0 {
		t.Fatalf("an SFTP test without host keys ran %d engine calls, %d commands, %d authentications", n-calls, len(ee.runner.Calls()), attempts())
	}
	var keys []engines.HostKeyInfo
	ee.call(t, 200, "POST", "/destinations/sftp/hostkeys", map[string]any{"host": host, "port": port}, &keys)
	if len(keys) != 1 || keys[0].Type != key.Type || keys[0].Key != key.Key || attempts() != 0 {
		t.Fatalf("host keys: %+v (auth attempts %d)", keys, attempts())
	}
	for _, body := range []any{
		map[string]any{"host": "", "port": 22}, map[string]any{"host": "169.254.169.254"}, map[string]any{"host": "h", "port": 70000},
		map[string]any{"host": "h", "user": "x"},
	} {
		if code, msg := ee.status(t, "POST", "/destinations/sftp/hostkeys", body); code != 400 {
			t.Errorf("host keys %v: %d %q", body, code, msg)
		}
	}
	// POST /destinations/test never takes an id.
	if code, _ := ee.status(t, "POST", "/destinations/test", map[string]any{"id": id}); code != 400 {
		t.Fatalf("test with an id: %d", code)
	}
}

// TestDestinationSnapshotsMergesMediaSnapshots: GET /destinations/{id}/snapshots lists a restic
// destination's media snapshots (kind media, sourceId, engineRef, complete, files, dataAdded)
// with its config versions, newest first; a filecopy destination's list keeps its Phase 1 shape.
func TestDestinationSnapshotsMergesMediaSnapshots(t *testing.T) {
	ee := newEngineEnv(t, engineEnvOptions{})
	src := ee.createSource(t, "Movies", ee.mkdir(t, "media/movies"))
	id := ee.attachedRestic(t, "Offsite", "snaps")
	t0 := time.Date(2026, 9, 1, 2, 0, 0, 0, time.UTC)
	if err := ee.db.Write(context.Background(), func(tx *sql.Tx) error {
		for i, snap := range []string{strings.Repeat("a", 64), strings.Repeat("b", 64)} {
			if _, err := tx.Exec(`INSERT INTO engine_snapshots (destination_id, source_id, snapshot_id, created_at, batch, complete, files, bytes, data_added)
				VALUES (?, ?, ?, ?, ?, ?, 10, 1000, 100)`, id, src, snap, db.FormatTime(t0.Add(time.Duration(i)*24*time.Hour)), i+1, i); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var list []map[string]any
	ee.call(t, 200, "GET", fmt.Sprintf("/destinations/%d/snapshots", id), nil, &list)
	if len(list) != 2 {
		t.Fatalf("snapshots: %+v", list)
	}
	first := list[0]
	if first["kind"] != "media" || first["engineRef"] != strings.Repeat("b", 64) || first["complete"] != true || first["files"] != float64(10) ||
		first["dataAdded"] != float64(100) || first["sourceId"] != float64(src) || list[1]["complete"] != false {
		t.Fatalf("media snapshots: %+v", list)
	}
	nas := ee.createDestination(t, "NAS", ee.mkdir(t, "nas"), nil, nil)
	code, raw := ee.raw(t, "GET", fmt.Sprintf("/destinations/%d/snapshots", nas), nil)
	if code != 200 || strings.TrimSpace(string(raw)) != "[]" {
		t.Fatalf("filecopy snapshots: %d %s", code, raw)
	}
}

// TestDeleteEngineDestination: deleting a destination whose recovery kit custody was never
// confirmed needs confirmLoseSecret=true (409 otherwise); its schedules and backup targets go
// with it.
func TestDeleteEngineDestination(t *testing.T) {
	ee := newEngineEnv(t, engineEnvOptions{})
	c := ee.session(t)
	id := ee.createEngineDest(t, c, s3Body("Offsite", "restic", "media", "del"))
	var plex struct {
		ID int64 `json:"id"`
	}
	ee.call(t, 201, "POST", "/integrations", map[string]any{"type": "plex", "name": "Plex", "url": "http://127.0.0.1:9"}, &plex)
	ee.sessionCall(t, c, 200, "PUT", fmt.Sprintf("/integrations/%d", plex.ID), map[string]any{"name": "Plex", "url": "http://127.0.0.1:9",
		"currentPassword": sessionPassword, "settings": map[string]any{"dataPath": "/plex",
			"backup": map[string]any{"targets": []any{map[string]any{"destinationId": id, "cron": "0 6 * * *", "enabled": true}}}}}, nil)
	path := fmt.Sprintf("/destinations/%d", id)
	if code, msg := ee.status(t, "DELETE", path, nil); code != 409 || !strings.Contains(msg, "confirmLoseSecret") {
		t.Fatalf("delete with unconfirmed custody: %d %q", code, msg)
	}
	if code, _ := ee.status(t, "DELETE", path+"?confirmLoseSecret=maybe", nil); code != 400 {
		t.Fatalf("delete with a bad flag: %d", code)
	}
	ee.call(t, 204, "DELETE", path+"?confirmLoseSecret=true", nil, nil)
	if n := len(ee.schedulesOf(t, jobs.TypeRetention, id)) + len(ee.schedulesOf(t, jobs.TypeVerify, id)) + len(ee.plexSchedules(t, plex.ID)); n != 0 {
		t.Fatalf("%d schedules left", n)
	}
	var it struct {
		Settings struct {
			Backup struct {
				DestinationID int64            `json:"destinationId"`
				Targets       []map[string]any `json:"targets"`
			} `json:"backup"`
		} `json:"settings"`
	}
	ee.call(t, 200, "GET", fmt.Sprintf("/integrations/%d", plex.ID), nil, &it)
	if it.Settings.Backup.DestinationID != 0 || it.Settings.Backup.Targets == nil || len(it.Settings.Backup.Targets) != 0 {
		t.Fatalf("backup settings after the delete: %+v", it.Settings.Backup)
	}
}

// TestPathGuardsSkipRemoteDestinations: SFTP, S3 and B2 destinations have no local path, so the
// S4 checks and the scanner's forbidden roots skip them; a local restic repository is guarded like
// a filecopy target.
func TestPathGuardsSkipRemoteDestinations(t *testing.T) {
	ee := newEngineEnv(t, engineEnvOptions{})
	c := ee.session(t)
	ee.createEngineDest(t, c, s3Body("Offsite", "restic", "media", "guards"))
	repo := ee.mkdir(t, "repo")
	local := map[string]any{"name": "Local restic", "kind": "local", "engine": "restic", "target": repo, "allowLocal": true}
	ee.call(t, 201, "POST", "/destinations", local, nil)
	g := &pathGuards{configDir: ee.config, sources: ee.app.Catalog, dests: ee.app.Destinations}
	roots, err := g.forbiddenRoots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 2 || roots[0] != ee.config || roots[1] != repo {
		t.Fatalf("forbidden roots: %v", roots)
	}
	if err := g.source(context.Background(), ee.mkdir(t, "media/movies")); err != nil {
		t.Fatalf("a source beside an S3 destination: %v", err)
	}
	if err := g.source(context.Background(), repo+"/inside"); err == nil {
		t.Fatal("a source inside a local restic repository was accepted")
	}
	// A source is created and scanned beside them.
	src := ee.createSource(t, "Movies", ee.mkdir(t, "media/movies"))
	var j jobs.Job
	ee.call(t, 202, "POST", fmt.Sprintf("/sources/%d/scan", src), nil, &j)
	if done := ee.waitJob(t, j.ID); done.Status != jobs.StatusCompleted {
		t.Fatalf("scan: %s %q", done.Status, done.Error)
	}
}

// TestEngineSyncsKeepTheOtherJobsWorkers: with the defaults (jobs.workers 2, engines.uploadSlots
// 2), two engine syncs that hold their upload slots for days (off-site seeds at a bandwidth
// limit) leave both workers of the other jobs free: two filecopy syncs run next to them, and a
// third waits, as it did before Phase 4.
func TestEngineSyncsKeepTheOtherJobsWorkers(t *testing.T) {
	ee := newEngineEnv(t, engineEnvOptions{})
	offsite := []int64{ee.attachedRestic(t, "Seed A", "seed-a"), ee.attachedRestic(t, "Seed B", "seed-b")}
	var nas []int64
	for i := range 3 {
		nas = append(nas, ee.createDestination(t, fmt.Sprintf("NAS %d", i), ee.mkdir(t, fmt.Sprintf("nas%d", i)), nil, nil))
	}
	release := make(chan struct{})
	var mu sync.Mutex
	var started []int64
	ee.app.Jobs.Register(jobs.TypeSync, jobs.RunnerFunc(func(ctx context.Context, job jobs.Job, _ jobs.Env) (jobs.Result, error) {
		mu.Lock()
		started = append(started, job.Params.DestinationID)
		mu.Unlock()
		select {
		case <-release:
		case <-ctx.Done():
		}
		return jobs.Result{}, nil
	}))
	var queued []int64
	for _, id := range append(slices.Clone(offsite), nas...) {
		j, err := ee.app.Jobs.Enqueue(context.Background(), jobs.Spec{Type: jobs.TypeSync, Trigger: jobs.TriggerManual,
			Params: jobs.Params{DestinationID: id}})
		if err != nil {
			t.Fatal(err)
		}
		queued = append(queued, j.ID)
	}
	defer func() {
		close(release)
		for _, id := range queued {
			ee.waitJob(t, id)
		}
	}()
	running := func() []int64 {
		mu.Lock()
		defer mu.Unlock()
		out := slices.Clone(started)
		slices.Sort(out)
		return out
	}
	want := append(slices.Clone(offsite), nas[0], nas[1])
	slices.Sort(want)
	deadline := time.Now().Add(10 * time.Second)
	for !slices.Equal(running(), want) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := running(); !slices.Equal(got, want) {
		t.Fatalf("running syncs of destinations %v, want %v: two engine syncs took the workers of the other jobs", got, want)
	}
	// jobs.workers still bounds the other jobs: the third filecopy sync waits.
	time.Sleep(300 * time.Millisecond)
	if got := running(); slices.Contains(got, nas[2]) {
		t.Fatalf("running syncs of destinations %v: a third filecopy sync started with jobs.workers 2", got)
	}
	j, err := ee.app.Jobs.Get(context.Background(), queued[len(queued)-1])
	if err != nil || j.Status != jobs.StatusQueued {
		t.Fatalf("third filecopy sync: %+v %v", j, err)
	}
}

// TestUnlockHoldsOffVersionStores: the Plex DB, *arr and manifest jobs hold no dest:<id> key, so
// one can start while POST /destinations/{id}/unlock {removeAll} is on its way (its identity check
// takes seconds against a real repository). Such a job waits for the unlock before it opens the
// destination's version store, so restic unlock --remove-all never removes its backup's lock
// (§6.7); and an unlock is refused while a version store of the destination is open.
func TestUnlockHoldsOffVersionStores(t *testing.T) {
	ee := newEngineEnv(t, engineEnvOptions{})
	id := ee.attachedRestic(t, "Offsite", "unlock-race")
	d, err := ee.app.Destinations.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	inCheck, proceed := make(chan struct{}), make(chan struct{})
	ee.runner.Expect(proc.Restic, enginetest.Prefix("cat", "config"), enginetest.Script{
		Hook:   func(*enginetest.Call) { close(inCheck); <-proceed },
		Stdout: []string{fmt.Sprintf(`{"version":2,"id":%q,"chunker_polynomial":"3dd3fbd3fea3a5"}`, strings.TrimPrefix(d.MarkerID, "restic:"))}})
	storedAtUnlock := -1
	ee.runner.Expect(proc.Restic, enginetest.Prefix("unlock"), enginetest.Script{Hook: func(*enginetest.Call) {
		storedAtUnlock = len(ee.versions.Stored())
	}})
	unlocked := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/v1/destinations/%d/unlock", ee.srv.URL, id), strings.NewReader(`{"removeAll":true}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Api-Key", ee.auth.APIKey())
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			unlocked <- 0
			return
		}
		_ = res.Body.Close()
		unlocked <- res.StatusCode
	}()
	select {
	case <-inCheck:
	case <-time.After(10 * time.Second):
		t.Fatal("the unlock did not reach its identity check")
	}
	// A manifest export starts during the identity check.
	var j jobs.Job
	ee.call(t, 202, "POST", fmt.Sprintf("/destinations/%d/manifest", id), nil, &j)
	deadline := time.Now().Add(10 * time.Second)
	for {
		cur, err := ee.app.Jobs.Get(context.Background(), j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if cur.Status != jobs.StatusQueued {
			break
		}
		if time.Now().After(deadline) {
			close(proceed)
			t.Fatalf("the manifest export did not start: %+v", cur)
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	if n := len(ee.versions.Stored()); n != 0 {
		close(proceed)
		t.Fatalf("the manifest export stored %d versions while restic unlock --remove-all was on its way", n)
	}
	close(proceed)
	if code := <-unlocked; code != http.StatusNoContent {
		t.Fatalf("unlock: %d", code)
	}
	if done := ee.waitJob(t, j.ID); done.Status != jobs.StatusCompleted {
		t.Fatalf("manifest export after the unlock: %s %q", done.Status, done.Error)
	}
	if storedAtUnlock != 0 || len(ee.versions.Stored()) != 1 {
		t.Fatalf("versions stored when the unlock ran: %d (want 0), after it: %d (want 1)", storedAtUnlock, len(ee.versions.Stored()))
	}

	// While a version store of the destination is open, the unlock is refused and runs nothing.
	open := ee.app.versionOpener(AppOptions{openVersions: func(context.Context, int64, engines.Runtime) (engines.VersionStore, io.Closer, error) {
		return ee.versions, io.NopCloser(nil), nil
	}})
	_, closer, err := open(context.Background(), id, engines.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	calls := len(ee.runner.Calls())
	if code, msg := ee.status(t, "POST", fmt.Sprintf("/destinations/%d/unlock", id), map[string]any{"removeAll": true}); code != 409 ||
		!strings.Contains(msg, "using its repository") {
		t.Fatalf("unlock while a version store is open: %d %q", code, msg)
	}
	if n := len(ee.runner.Calls()); n != calls {
		t.Fatalf("%d commands ran for a refused unlock", n-calls)
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	ee.runner.Expect(proc.Restic, enginetest.Prefix("cat", "config"),
		enginetest.Script{Stdout: []string{fmt.Sprintf(`{"version":2,"id":%q,"chunker_polynomial":"3dd3fbd3fea3a5"}`, strings.TrimPrefix(d.MarkerID, "restic:"))}})
	ee.runner.Expect(proc.Restic, enginetest.Prefix("unlock"), enginetest.Script{})
	ee.call(t, 204, "POST", fmt.Sprintf("/destinations/%d/unlock", id), map[string]any{"removeAll": true}, nil)
}

// TestDeleteRemovesResticCache: deleting a restic destination removes its restic cache
// (<config>/cache/restic/<id>, up to several GiB; ids are never reused), and only its own.
func TestDeleteRemovesResticCache(t *testing.T) {
	ee := newEngineEnv(t, engineEnvOptions{})
	gone := ee.attachedRestic(t, "Old", "cache-old")
	kept := ee.attachedRestic(t, "Kept", "cache-kept")
	cache := func(id int64) string { return filepath.Join(ee.config, "cache", "restic", fmt.Sprint(id)) }
	for _, id := range []int64{gone, kept} {
		if err := os.MkdirAll(filepath.Join(cache(id), "0123abcd", "index"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cache(id), "0123abcd", "index", "f"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ee.call(t, 204, "DELETE", fmt.Sprintf("/destinations/%d", gone), nil, nil)
	if _, err := os.Stat(cache(gone)); !os.IsNotExist(err) {
		t.Fatalf("the deleted destination's restic cache is still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache(kept), "0123abcd", "index", "f")); err != nil {
		t.Fatalf("another destination's restic cache: %v", err)
	}
}

// TestResticRemoteNeedsRclone: restic reaches every remote repository through its rclone backend
// (D21), so while rclone is not available a restic destination on S3 is blocked like one whose
// engine is missing (blockedReason, 409 for a sync), and a restic S3 location cannot be tested or
// created (400); nothing reaches an engine. A local restic repository needs no rclone.
func TestResticRemoteNeedsRclone(t *testing.T) {
	ee := newEngineEnv(t, engineEnvOptions{})
	id := ee.attachedRestic(t, "Offsite", "needs-rclone")
	ee.app.engines.availability.Rclone = engines.BinaryStatus{Reason: "rclone is not installed"}
	const want = "restic reaches s3 repositories through rclone: rclone is not installed"
	calls := ee.engineCalls() + len(ee.runner.Calls())

	var v destinationView
	ee.call(t, 200, "GET", fmt.Sprintf("/destinations/%d", id), nil, &v)
	if v.BlockedReason != want {
		t.Fatalf("blockedReason %q, want %q", v.BlockedReason, want)
	}
	if code, msg := ee.status(t, "POST", fmt.Sprintf("/destinations/%d/sync", id), nil); code != 409 || !strings.Contains(msg, want) {
		t.Fatalf("sync: %d %q", code, msg)
	}
	if code, msg := ee.status(t, "POST", fmt.Sprintf("/destinations/%d/test", id), nil); code != 400 || msg != want {
		t.Fatalf("test of the stored destination: %d %q", code, msg)
	}
	probe := s3Body("", "restic", "media", "probe")
	delete(probe, "name")
	if code, msg := ee.status(t, "POST", "/destinations/test", probe); code != 400 || msg != want {
		t.Fatalf("test of a new location: %d %q", code, msg)
	}
	c := ee.session(t)
	if code, raw, _ := ee.sessionRaw(t, c, "POST", "/destinations", withPassword(s3Body("New", "restic", "media", "new"), sessionPassword)); code != 400 ||
		!strings.Contains(string(raw), want) {
		t.Fatalf("create: %d %s", code, raw)
	}
	if n := ee.engineCalls() + len(ee.runner.Calls()); n != calls {
		t.Fatalf("%d engine calls while rclone is not available", n-calls)
	}
	for _, tt := range []struct {
		engine string
		kind   engines.DestKind
	}{{destinations.EngineRestic, engines.Local}, {destinations.EngineRclone, engines.S3}, {destinations.EngineFilecopy, engines.Local}} {
		if reason := ee.app.resticRcloneBlock(tt.engine, tt.kind); reason != "" {
			t.Errorf("%s on %s: %q", tt.engine, tt.kind, reason)
		}
	}
}
