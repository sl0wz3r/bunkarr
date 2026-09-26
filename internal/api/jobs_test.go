package api

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

func TestJobsListingAndValidation(t *testing.T) {
	e := newEnv(t, nil)
	// Keep jobs queued: stop the manager.
	if err := e.app.Jobs.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	srcA := e.createSource(t, "A", e.mkdir(t, "a"))
	srcB := e.createSource(t, "B", e.mkdir(t, "b"))
	var ja, jb jobs.Job
	e.call(t, 202, "POST", fmt.Sprintf("/sources/%d/scan", srcA), nil, &ja)
	e.call(t, 202, "POST", fmt.Sprintf("/sources/%d/scan", srcB), nil, &jb)
	// An identical queued job is returned instead of a new one.
	var again jobs.Job
	e.call(t, 202, "POST", fmt.Sprintf("/sources/%d/scan", srcA), nil, &again)
	if again.ID != ja.ID {
		t.Fatalf("dedupe: %d, want %d", again.ID, ja.ID)
	}

	var page jobqueue.Page[jobs.Job]
	e.call(t, 200, "GET", "/jobs?state=active&type=scan", nil, &page)
	if page.TotalRecords != 2 || page.Records[0].ID != ja.ID || page.Page != 1 || page.PageSize != 50 {
		t.Fatalf("active jobs: %+v", page)
	}
	e.call(t, 200, "GET", "/jobs?state=finished", nil, &page)
	if page.TotalRecords != 0 || page.Records == nil {
		t.Fatalf("finished jobs: %+v", page)
	}
	e.call(t, 200, "GET", "/jobs?page=2&pageSize=1", nil, &page)
	if page.TotalRecords != 2 || len(page.Records) != 1 || page.Records[0].ID != ja.ID {
		t.Fatalf("page 2 of all jobs (newest first): %+v", page)
	}
	for _, q := range []string{"state=done", "type=backup", "status=ok", "page=0", "pageSize=501", "pageSize=-1", "destinationId=x"} {
		if code, _ := e.status(t, "GET", "/jobs?"+q, nil); code != 400 {
			t.Errorf("jobs?%s: %d, want 400", q, code)
		}
	}

	// Cancel: a queued job at once; a finished one is a conflict.
	var c jobs.Job
	e.call(t, 200, "POST", fmt.Sprintf("/jobs/%d/cancel", jb.ID), nil, &c)
	if c.Status != jobs.StatusCancelled || c.FinishedAt == nil {
		t.Fatalf("cancelled: %+v", c)
	}
	if code, msg := e.status(t, "POST", fmt.Sprintf("/jobs/%d/cancel", jb.ID), nil); code != 409 || !strings.Contains(msg, "cancelled") {
		t.Fatalf("cancel of a cancelled job: %d %q", code, msg)
	}
	for _, p := range []string{"/jobs/999", "/jobs/999/items", "/jobs/999/items/summary", "/jobs/999/logs"} {
		if code, _ := e.status(t, "GET", p, nil); code != 404 {
			t.Errorf("GET %s: %d, want 404", p, code)
		}
	}
	if code, _ := e.status(t, "POST", "/jobs/999/cancel", nil); code != 404 {
		t.Errorf("cancel of a missing job: %d", code)
	}
	for _, p := range []string{"/jobs/0", "/jobs/-1", "/jobs/x"} {
		if code, _ := e.status(t, "GET", p, nil); code != 400 {
			t.Errorf("GET %s: %d, want 400", p, code)
		}
	}
	jp := fmt.Sprintf("/jobs/%d", ja.ID)
	var items jobqueue.Page[jobs.Item]
	e.call(t, 200, "GET", jp+"/items", nil, &items)
	if items.TotalRecords != 0 || items.Records == nil {
		t.Fatalf("items of a queued job: %+v", items)
	}
	var counts []jobs.ItemCount
	e.call(t, 200, "GET", jp+"/items/summary", nil, &counts)
	if counts == nil || len(counts) != 0 {
		t.Fatalf("summary of a queued job: %#v", counts)
	}
	var logs []jobqueue.LogEntry
	e.call(t, 200, "GET", jp+"/logs", nil, &logs)
	if logs == nil {
		t.Fatal("logs: null")
	}
	for _, q := range []string{"/items?action=fly", "/items?status=maybe", "/items?pageSize=0", "/logs?limit=0", "/logs?limit=1001", "/logs?afterId=-1"} {
		if code, _ := e.status(t, "GET", jp+q, nil); code != 400 {
			t.Errorf("GET %s: %d, want 400", jp+q, code)
		}
	}
}

// TestJobsFilterByIntegration: GET /jobs?integrationId= lists only the jobs of that integration
// (design phase2-3.md §13), here the refreshes queued when two *arr integrations were created.
func TestJobsFilterByIntegration(t *testing.T) {
	e := newEnv(t, nil)
	// Keep jobs queued: stop the manager.
	if err := e.app.Jobs.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	var a, b struct {
		ID int64 `json:"id"`
	}
	e.call(t, 201, "POST", "/integrations", map[string]any{"type": "sonarr", "name": "Sonarr", "url": "http://127.0.0.1:9"}, &a)
	e.call(t, 201, "POST", "/integrations", map[string]any{"type": "radarr", "name": "Radarr", "url": "http://127.0.0.1:9"}, &b)
	for _, id := range []int64{a.ID, b.ID} {
		var page jobqueue.Page[jobs.Job]
		e.call(t, 200, "GET", fmt.Sprintf("/jobs?state=active&integrationId=%d", id), nil, &page)
		if page.TotalRecords != 1 || page.Records[0].Type != jobs.TypeRefresh || page.Records[0].Params.IntegrationID != id {
			t.Fatalf("jobs of integration %d: %+v", id, page)
		}
	}
	var all jobqueue.Page[jobs.Job]
	e.call(t, 200, "GET", "/jobs?state=active&type=refresh", nil, &all)
	if all.TotalRecords != 2 {
		t.Fatalf("refresh jobs: %+v", all)
	}
	var none jobqueue.Page[jobs.Job]
	e.call(t, 200, "GET", "/jobs?integrationId=999", nil, &none)
	if none.TotalRecords != 0 {
		t.Fatalf("jobs of an unknown integration: %+v", none)
	}
	for _, q := range []string{"integrationId=x", "integrationId=-1"} {
		if code, _ := e.status(t, "GET", "/jobs?"+q, nil); code != 400 {
			t.Errorf("jobs?%s: %d, want 400", q, code)
		}
	}
}

func TestSchedules(t *testing.T) {
	e := newEnv(t, nil)
	var list []scheduleView
	e.call(t, 200, "GET", "/schedules", nil, &list)
	// The seeded daily retention.
	if len(list) != 1 || list[0].JobType != jobs.TypeRetention || list[0].Cron != "30 4 * * *" || !list[0].Enabled || list[0].NextRunAt == nil ||
		!strings.HasPrefix(list[0].Description, "Retention") {
		t.Fatalf("schedules: %+v", list)
	}
	id := list[0].ID
	path := fmt.Sprintf("/schedules/%d", id)
	var sc scheduleView
	e.call(t, 200, "PUT", path, map[string]any{"cron": "0 3 * * *", "enabled": false}, &sc)
	if sc.Cron != "0 3 * * *" || sc.Enabled || sc.NextRunAt != nil {
		t.Fatalf("disabled: %+v", sc)
	}
	e.call(t, 200, "PUT", path, map[string]any{"enabled": true}, &sc)
	if sc.Cron != "0 3 * * *" || !sc.Enabled || sc.NextRunAt == nil || sc.NextRunAt.Local().Hour() != 3 {
		t.Fatalf("enabled: %+v", sc)
	}
	for _, b := range []any{map[string]any{"cron": "61 * * * *"}, map[string]any{"cron": "@every 1m"}, map[string]any{"crn": "x"}, "[]"} {
		if code, _ := e.status(t, "PUT", path, b); code != 400 {
			t.Errorf("update %v: %d, want 400", b, code)
		}
	}
	if code, _ := e.status(t, "PUT", "/schedules/999", map[string]any{"cron": "0 3 * * *"}); code != 404 {
		t.Errorf("update of a missing schedule: %d", code)
	}
	// Run now: the global retention job, which queues per-destination jobs and completes.
	var j jobs.Job
	e.call(t, 202, "POST", path+"/run", nil, &j)
	if j.Type != jobs.TypeRetention || j.Trigger != jobs.TriggerManual {
		t.Fatalf("run: %+v", j)
	}
	if done := e.waitJob(t, j.ID); done.Status != jobs.StatusCompleted {
		t.Fatalf("retention: %s %s", done.Status, done.Error)
	}
	e.call(t, 200, "GET", "/schedules", nil, &list)
	if list[0].LastRunAt == nil {
		t.Fatalf("lastRunAt not recorded: %+v", list[0])
	}
	if code, _ := e.status(t, "POST", "/schedules/999/run", nil); code != 404 {
		t.Errorf("run of a missing schedule: %d", code)
	}
}

// TestScheduledJobsOfDisabledDestinationsAreNotQueued: the scheduler's enqueuer refuses a
// scheduled job whose destination is disabled (it would only fail and notify).
func TestScheduledJobsOfDisabledDestinationsAreNotQueued(t *testing.T) {
	e := newEnv(t, nil)
	id := e.createDestination(t, "NAS", e.mkdir(t, "nas"), nil, nil)
	gate := scheduleGate{a: e.app}
	spec := jobs.Spec{Type: jobs.TypeVerify, Trigger: jobs.TriggerSchedule, Params: jobs.Params{DestinationID: id}}
	e.call(t, 200, "PUT", fmt.Sprintf("/destinations/%d", id), map[string]any{"enabled": false}, nil)
	if _, err := gate.Enqueue(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("scheduled verify of a disabled destination: %v", err)
	}
	spec.Trigger = jobs.TriggerManual
	j, err := gate.Enqueue(context.Background(), spec)
	if err != nil {
		t.Fatalf("manual job: %v", err)
	}
	if done := e.waitJob(t, j.ID); done.Status != jobs.StatusFailed {
		t.Fatalf("manual verify of a disabled destination: %s", done.Status)
	}
}

func TestWorkersSetting(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int
	}{{"", 2}, {"4", 4}, {"0", 2}, {"x", 2}, {"1000", 2}} {
		e := newEnv(t, nil)
		ctx := context.Background()
		if tc.value != "" {
			if err := e.app.settings.Set(ctx, SettingJobsWorkers, tc.value); err != nil {
				t.Fatal(err)
			}
		}
		if got := e.app.intSetting(ctx, SettingJobsWorkers, jobqueue.DefaultWorkers, 1, maxWorkers); got != tc.want {
			t.Errorf("jobs.workers %q: %d, want %d", tc.value, got, tc.want)
		}
	}
}
