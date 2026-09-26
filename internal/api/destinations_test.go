package api

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/snapshots"
)

// schedulesOf returns the schedules of one job type for a destination.
func (e *env) schedulesOf(t *testing.T, typ jobs.Type, destID int64) []scheduleView {
	t.Helper()
	var list []scheduleView
	e.call(t, 200, "GET", "/schedules", nil, &list)
	var out []scheduleView
	for _, sc := range list {
		if sc.JobType == typ && sc.Params.DestinationID == destID {
			out = append(out, sc)
		}
	}
	return out
}

func TestDestinationTestTarget(t *testing.T) {
	e := newEnv(t, nil)
	dst := e.mkdir(t, "nas")
	var res destinations.TestResult
	e.call(t, 200, "POST", "/destinations/test", map[string]any{"target": dst}, &res)
	if !res.OK || res.Marker != destinations.MarkerMissing || !res.Writable || !res.Local || res.Capabilities != nil || res.TotalBytes == 0 {
		t.Fatalf("test of an empty local target: %+v", res)
	}
	if got := entries(t, dst); len(got) != 0 {
		t.Fatalf("the test wrote to the target: %v", got)
	}
	e.call(t, 200, "POST", "/destinations/test", map[string]any{"target": filepath.Join(e.base, "missing")}, &res)
	if res.OK || !strings.Contains(res.Message, "does not exist") {
		t.Fatalf("missing target: %+v", res)
	}
	for _, body := range []any{map[string]any{}, map[string]any{"target": dst, "extra": 1}, `{"target": `} {
		if code, _ := e.status(t, "POST", "/destinations/test", body); code != 400 {
			t.Errorf("test %v: %d, want 400", body, code)
		}
	}
}

func TestDestinationCreateAndS3Refusals(t *testing.T) {
	e := newEnv(t, nil)
	dst := e.mkdir(t, "nas")
	// A local filesystem needs allowLocal (400 with the explanation).
	code, msg := e.status(t, "POST", "/destinations", map[string]any{"name": "NAS", "target": dst})
	if code != 400 || !strings.Contains(msg, "allowLocal") {
		t.Fatalf("local target without allowLocal: %d %q", code, msg)
	}
	if got := entries(t, dst); len(got) != 0 {
		t.Fatalf("a refused create wrote %v", got)
	}
	// A target that must exist, may not be relative, and a missing name.
	for _, body := range []map[string]any{
		{"name": "X", "target": filepath.Join(e.base, "nope"), "allowLocal": true},
		{"name": "X", "target": "relative/path", "allowLocal": true},
		{"name": "", "target": dst, "allowLocal": true},
		{"name": "X", "allowLocal": true},
		{"name": "X", "target": dst, "allowLocal": true, "settings": map[string]any{"hardlinks": "maybe"}},
		{"name": "X", "target": dst, "allowLocal": true, "schedule": map[string]any{"cron": "not a cron", "enabled": true}},
		{"name": "X", "target": dst, "allowLocal": true, "schedule": map[string]any{"cron": "", "enabled": true}},
		{"name": "X", "target": e.config, "allowLocal": true},
		{"name": "X", "target": "/", "allowLocal": true},
	} {
		if code, msg := e.status(t, "POST", "/destinations", body); code != 400 {
			t.Errorf("create %v: %d %q, want 400", body, code, msg)
		}
	}
	if got := entries(t, dst); len(got) != 0 {
		t.Fatalf("refused creates wrote %v", got)
	}

	var d destinationView
	e.call(t, 201, "POST", "/destinations", map[string]any{"name": "NAS", "target": dst, "allowLocal": true}, &d)
	if d.Name != "NAS" || d.Engine != "filecopy" || d.Target != dst || !d.Enabled || d.FSType == "" || d.Capabilities.CheckedAt.IsZero() {
		t.Fatalf("created: %+v", d)
	}
	if d.Settings != destinations.DefaultSettings() || d.Retention != destinations.DefaultRetention() {
		t.Fatalf("defaults: %+v %+v", d.Settings, d.Retention)
	}
	// Default schedules: no sync schedule until one is chosen, verify weekly on Sunday 05:00.
	if d.Schedule != (cronSchedule{}) || d.VerifySchedule != (cronSchedule{Cron: DefaultVerifyCron, Enabled: true}) || d.LastJob != nil {
		t.Fatalf("schedules: %+v %+v %+v", d.Schedule, d.VerifySchedule, d.LastJob)
	}
	if got := e.schedulesOf(t, jobs.TypeSync, d.ID); len(got) != 0 {
		t.Fatalf("sync schedules: %+v", got)
	}
	vs := e.schedulesOf(t, jobs.TypeVerify, d.ID)
	if len(vs) != 1 || vs[0].NextRunAt == nil || vs[0].NextRunAt.Weekday() != time.Sunday && vs[0].NextRunAt.Local().Weekday() != time.Sunday ||
		!strings.Contains(vs[0].Description, "NAS") {
		t.Fatalf("verify schedule: %+v", vs)
	}
	if _, err := os.Stat(filepath.Join(dst, ".bunkarr", "destination.json")); err != nil {
		t.Fatalf("marker: %v", err)
	}

	// The same name again; the same target again (its marker belongs to a destination).
	if code, msg := e.status(t, "POST", "/destinations", map[string]any{"name": "nas", "target": e.mkdir(t, "other"), "allowLocal": true}); code != 409 {
		t.Errorf("duplicate name: %d %q", code, msg)
	}
	if code, msg := e.status(t, "POST", "/destinations", map[string]any{"name": "Other", "target": dst, "allowLocal": true}); code != 400 || !strings.Contains(msg, "overlaps") {
		t.Errorf("same target: %d %q", code, msg)
	}

	// A target with a foreign marker: 409 until attach is set.
	e.call(t, 204, "DELETE", fmt.Sprintf("/destinations/%d", d.ID), nil, nil)
	if _, err := os.Stat(filepath.Join(dst, ".bunkarr", "destination.json")); err != nil {
		t.Fatalf("delete removed the marker: %v", err)
	}
	code, msg = e.status(t, "POST", "/destinations", map[string]any{"name": "NAS again", "target": dst, "allowLocal": true})
	if code != 409 || !strings.Contains(msg, "attach") {
		t.Fatalf("existing marker without attach: %d %q", code, msg)
	}
	var res destinations.TestResult
	e.call(t, 200, "POST", "/destinations/test", map[string]any{"target": dst}, &res)
	if res.Marker != destinations.MarkerForeign {
		t.Fatalf("test of a target with a marker: %+v", res)
	}
	e.call(t, 201, "POST", "/destinations", map[string]any{"name": "NAS again", "target": dst, "allowLocal": true, "attach": true}, &d)
}

func TestDestinationSchedulesAndUpdate(t *testing.T) {
	e := newEnv(t, nil)
	srcID := e.createSource(t, "Movies", e.mkdir(t, "media/movies"))
	dst := e.mkdir(t, "nas")
	id := e.createDestination(t, "NAS", dst, []int64{srcID}, map[string]any{
		"schedule":       map[string]any{"cron": "0 2 * * *", "enabled": true},
		"verifySchedule": map[string]any{"cron": "0 3 * * 0", "enabled": false},
		"settings":       map[string]any{"verify": map[string]any{"mode": "full"}, "hardlinks": "copy"},
		"retention":      map[string]any{"deletedDays": 7},
	})
	path := fmt.Sprintf("/destinations/%d", id)
	var d destinationView
	e.call(t, 200, "GET", path, nil, &d)
	if d.Schedule != (cronSchedule{"0 2 * * *", true}) || d.VerifySchedule != (cronSchedule{"0 3 * * 0", false}) {
		t.Fatalf("schedules: %+v %+v", d.Schedule, d.VerifySchedule)
	}
	if d.Settings.Verify.Mode != destinations.VerifyFull || d.Settings.Hardlinks != destinations.HardlinksCopy || d.Retention.DeletedDays != 7 ||
		d.Retention.PlexDBDaily != destinations.DefaultPlexDBDaily || len(d.SourceIDs) != 1 || d.SourceIDs[0] != srcID {
		t.Fatalf("settings: %+v %+v %v", d.Settings, d.Retention, d.SourceIDs)
	}
	ss := e.schedulesOf(t, jobs.TypeSync, id)
	if len(ss) != 1 || !ss[0].Enabled || ss[0].NextRunAt == nil || ss[0].Description != "Sync to NAS" {
		t.Fatalf("sync schedule: %+v", ss)
	}
	if vs := e.schedulesOf(t, jobs.TypeVerify, id); len(vs) != 1 || vs[0].Enabled || vs[0].NextRunAt != nil {
		t.Fatalf("disabled verify schedule: %+v", vs)
	}

	// Update: rename, unlink the source, change the sync schedule, keep verify (not sent).
	e.call(t, 200, "PUT", path, map[string]any{"name": "UNAS", "sourceIds": []int64{},
		"schedule": map[string]any{"cron": "30 1 * * *", "enabled": true}}, &d)
	if d.Name != "UNAS" || len(d.SourceIDs) != 0 || d.Schedule != (cronSchedule{"30 1 * * *", true}) ||
		d.VerifySchedule != (cronSchedule{"0 3 * * 0", false}) || d.Settings.Verify.Mode != destinations.VerifyFull {
		t.Fatalf("after update: %+v", d)
	}
	// An empty, disabled schedule removes it.
	e.call(t, 200, "PUT", path, map[string]any{"schedule": map[string]any{"cron": "", "enabled": false}}, &d)
	if d.Schedule != (cronSchedule{}) || len(e.schedulesOf(t, jobs.TypeSync, id)) != 0 {
		t.Fatalf("sync schedule not removed: %+v", d.Schedule)
	}
	for _, body := range []map[string]any{
		{"target": e.mkdir(t, "elsewhere")},
		{"engine": "restic"},
		{"attach": true},
		{"allowLocal": true},
		{"verifySchedule": map[string]any{"cron": "* * *", "enabled": false}},
		{"sourceIds": []int64{999}},
		{"retention": map[string]any{"deletedDays": -1}},
		{"unknown": true},
	} {
		if code, msg := e.status(t, "PUT", path, body); code != 400 {
			t.Errorf("update %v: %d %q, want 400", body, code, msg)
		}
	}
	if code, _ := e.status(t, "PUT", "/destinations/999", map[string]any{"name": "x"}); code != 404 {
		t.Errorf("update of a missing destination: %d", code)
	}
	if code, _ := e.status(t, "GET", "/destinations/abc", nil); code != 400 {
		t.Errorf("malformed id: %d", code)
	}

	// Disabled: manual sync and verify are refused.
	e.call(t, 200, "PUT", path, map[string]any{"enabled": false}, &d)
	for _, p := range []string{path + "/sync", path + "/verify"} {
		if code, msg := e.status(t, "POST", p, nil); code != 409 || !strings.Contains(msg, "disabled") {
			t.Errorf("POST %s of a disabled destination: %d %q", p, code, msg)
		}
	}

	// Delete: schedules go, data stays.
	e.call(t, 204, "DELETE", path, nil, nil)
	if n := len(e.schedulesOf(t, jobs.TypeVerify, id)); n != 0 {
		t.Fatalf("%d verify schedules left", n)
	}
	if got := entries(t, dst); len(got) != 1 || got[0] != ".bunkarr" {
		t.Fatalf("delete touched the target: %v", got)
	}
	if code, _ := e.status(t, "GET", path, nil); code != 404 {
		t.Fatalf("deleted destination: %d", code)
	}
	if code, _ := e.status(t, "DELETE", path, nil); code != 404 {
		t.Fatalf("second delete: %d", code)
	}
}

func TestDestinationDeleteRefusedWhileJobsAreActive(t *testing.T) {
	e := newEnvWith(t, nil, func(o *AppOptions) { o.Workers = 1 })
	dst := e.mkdir(t, "nas")
	id := e.createDestination(t, "NAS", dst, nil, nil)
	// Stop the manager so the queued job stays queued.
	if err := e.app.Jobs.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	var j jobs.Job
	e.call(t, 202, "POST", fmt.Sprintf("/destinations/%d/sync", id), nil, &j)
	if code, msg := e.status(t, "DELETE", fmt.Sprintf("/destinations/%d", id), nil); code != 409 || !strings.Contains(msg, "queued or running") {
		t.Fatalf("delete with a queued job: %d %q", code, msg)
	}
	e.call(t, 200, "POST", fmt.Sprintf("/jobs/%d/cancel", j.ID), nil, &j)
	if j.Status != jobs.StatusCancelled {
		t.Fatalf("cancelled job: %+v", j)
	}
	e.call(t, 204, "DELETE", fmt.Sprintf("/destinations/%d", id), nil, nil)
}

func TestDestinationTestExistingAndNotMounted(t *testing.T) {
	e := newEnv(t, nil)
	dst := e.mkdir(t, "nas")
	id := e.createDestination(t, "NAS", dst, nil, nil)
	var res destinations.TestResult
	e.call(t, 200, "POST", fmt.Sprintf("/destinations/%d/test", id), nil, &res)
	if !res.OK || res.Marker != destinations.MarkerOK || res.Capabilities == nil || !res.Writable {
		t.Fatalf("test of a destination: %+v", res)
	}
	if code, _ := e.status(t, "POST", fmt.Sprintf("/destinations/%d/test", id), `{"target":"/x"}`); code != 400 {
		t.Fatalf("test with an unknown field: %d", code)
	}
	// The share "unmounted": the marker is gone.
	if err := os.Remove(filepath.Join(dst, ".bunkarr", "destination.json")); err != nil {
		t.Fatal(err)
	}
	e.call(t, 200, "POST", fmt.Sprintf("/destinations/%d/test", id), nil, &res)
	if res.OK || res.Marker != destinations.MarkerMissing || !strings.Contains(res.Message, "not mounted") {
		t.Fatalf("test without a marker: %+v", res)
	}
	// A sync fails with "destination not mounted?" and writes nothing.
	before := entries(t, filepath.Join(dst, ".bunkarr"))
	var j jobs.Job
	e.call(t, 202, "POST", fmt.Sprintf("/destinations/%d/sync", id), map[string]any{"dryRun": false}, &j)
	done := e.waitJob(t, j.ID)
	if done.Status != jobs.StatusFailed || !strings.Contains(done.Error, "not mounted") {
		t.Fatalf("sync without a marker: %s %q", done.Status, done.Error)
	}
	if got := entries(t, filepath.Join(dst, ".bunkarr")); strings.Join(got, ",") != strings.Join(before, ",") {
		t.Fatalf("the failed sync wrote: %v, before %v", got, before)
	}
	if got := entries(t, dst); len(got) != 1 {
		t.Fatalf("the failed sync wrote: %v", got)
	}
	if code, _ := e.status(t, "POST", "/destinations/999/test", nil); code != 404 {
		t.Fatalf("test of a missing destination: %d", code)
	}
}

func TestDestinationOverlapWithSources(t *testing.T) {
	e := newEnv(t, nil)
	media := e.mkdir(t, "media")
	e.createSource(t, "Movies", e.mkdir(t, "media/movies"))
	for _, target := range []string{media, filepath.Join(media, "movies"), e.mkdir(t, "media/movies/backup")} {
		code, msg := e.status(t, "POST", "/destinations", map[string]any{"name": "X", "target": target, "allowLocal": true})
		if code != 400 || !strings.Contains(msg, "source") {
			t.Errorf("target %s overlapping a source: %d %q", target, code, msg)
		}
		var res destinations.TestResult
		e.call(t, 200, "POST", "/destinations/test", map[string]any{"target": target}, &res)
		if res.OK {
			t.Errorf("test of %s overlapping a source: %+v", target, res)
		}
	}
}

func TestDestinationSnapshots(t *testing.T) {
	e := newEnv(t, nil)
	id := e.createDestination(t, "NAS", e.mkdir(t, "nas"), nil, nil)
	var list []snapshots.Snapshot
	e.call(t, 200, "GET", fmt.Sprintf("/destinations/%d/snapshots", id), nil, &list)
	if list == nil || len(list) != 0 {
		t.Fatalf("snapshots: %#v", list)
	}
	if code, _ := e.status(t, "GET", "/destinations/999/snapshots", nil); code != 404 {
		t.Fatalf("snapshots of a missing destination: %d", code)
	}
}

// TestDestinationLastSyncIgnoresOtherJobs: the Destinations page shows the last sync (design
// §10: "list with last sync status"). Verify, retention and preview jobs that come later leave
// it alone; lastJob stays the newest job of any type.
func TestDestinationLastSyncIgnoresOtherJobs(t *testing.T) {
	e := newEnv(t, nil)
	id := e.createDestination(t, "NAS", e.mkdir(t, "nas"), nil, nil)
	path := fmt.Sprintf("/destinations/%d", id)
	// Decoded on its own, so the test states the API shape it needs.
	type view struct {
		LastJob  *jobs.Job `json:"lastJob"`
		LastSync *jobs.Job `json:"lastSync"`
	}
	var v view
	e.call(t, 200, "GET", path, nil, &v)
	if v.LastJob != nil || v.LastSync != nil {
		t.Fatalf("new destination: %+v", v)
	}

	var j jobs.Job
	e.call(t, 202, "POST", path+"/sync", nil, &j)
	sync := e.waitJob(t, j.ID)
	e.call(t, 202, "POST", path+"/verify", nil, &j)
	e.waitJob(t, j.ID)
	ret, err := e.app.Jobs.Enqueue(t.Context(), jobs.Spec{Type: jobs.TypeRetention, Trigger: jobs.TriggerManual, Params: jobs.Params{DestinationID: id}})
	if err != nil {
		t.Fatal(err)
	}
	e.waitJob(t, ret.ID)
	e.call(t, 202, "POST", path+"/sync", map[string]any{"dryRun": true}, &j)
	preview := e.waitJob(t, j.ID)

	check := func(what string, v view) {
		t.Helper()
		if v.LastSync == nil || v.LastSync.ID != sync.ID || v.LastSync.Type != jobs.TypeSync || v.LastSync.DryRun {
			t.Errorf("%s: lastSync %+v, want job %d", what, v.LastSync, sync.ID)
		}
		if v.LastJob == nil || v.LastJob.ID != preview.ID {
			t.Errorf("%s: lastJob %+v, want job %d", what, v.LastJob, preview.ID)
		}
	}
	e.call(t, 200, "GET", path, nil, &v)
	check("GET /destinations/{id}", v)
	var list []view
	e.call(t, 200, "GET", "/destinations", nil, &list)
	if len(list) != 1 {
		t.Fatalf("list: %+v", list)
	}
	check("GET /destinations", list[0])
}
