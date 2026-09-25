package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// TestCronCasesMatchServer: the web UI validates cron expressions against the same list
// (web/src/lib/cron.test.ts), so the list must say what the server really accepts.
func TestCronCasesMatchServer(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "web", "src", "lib", "cron-cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Cases []struct {
			Expr  string `json:"expr"`
			Valid bool   `json:"valid"`
			Why   string `json:"why"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Cases) < 20 {
		t.Fatalf("only %d cases", len(doc.Cases))
	}
	for _, c := range doc.Cases {
		err := jobqueue.ValidateCron(c.Expr)
		if (err == nil) != c.Valid {
			t.Errorf("%q (%s): ValidateCron = %v, the list says valid=%v", c.Expr, c.Why, err, c.Valid)
		}
	}
}

// TestWebCronDefaultsMatchServer: the forms' default schedules (web/src/lib/cron.ts) are the
// ones the server applies when a client sends none.
func TestWebCronDefaultsMatchServer(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "web", "src", "lib", "cron.ts"))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"DEFAULT_VERIFY_CRON": DefaultVerifyCron, "DEFAULT_PLEX_BACKUP_CRON": DefaultPlexBackupCron} {
		m := regexp.MustCompile(`export const ` + name + ` = '([^']*)';`).FindSubmatch(raw)
		if m == nil || string(m[1]) != want {
			t.Errorf("web %s = %q, the server's is %q", name, m, want)
		}
	}
}

// scheduleState is the part of GET /schedules' item these tests look at, decoded on its own so
// the test states the API shape it needs.
type scheduleState struct {
	ID            int64       `json:"id"`
	JobType       jobs.Type   `json:"jobType"`
	Params        jobs.Params `json:"params"`
	Enabled       bool        `json:"enabled"`
	NextRunAt     *time.Time  `json:"nextRunAt"`
	BlockedReason *string     `json:"blockedReason"`
}

func (e *env) scheduleStates(t *testing.T) []scheduleState {
	t.Helper()
	var list []scheduleState
	e.call(t, 200, "GET", "/schedules", nil, &list)
	return list
}

// TestSchedulesOfDisabledDestinationsAndIntegrationsAreBlocked: the scheduler refuses the jobs
// of a disabled destination or Plex integration (scheduleGate), so GET /schedules says why
// instead of promising a next run, and Run now answers 409 instead of queueing a job that fails.
func TestSchedulesOfDisabledDestinationsAndIntegrationsAreBlocked(t *testing.T) {
	e := newEnv(t, nil)
	destID := e.createDestination(t, "NAS", e.mkdir(t, "nas"), nil, map[string]any{"schedule": map[string]any{"cron": "0 2 * * *", "enabled": true}})
	plexBody := func(enabled bool) map[string]any {
		return map[string]any{"type": "plex", "name": "Plex", "url": "http://127.0.0.1:9", "apiKey": "blocked-test-token", "enabled": enabled,
			"settings": map[string]any{"dataPath": "/plex", "backup": map[string]any{"destinationId": destID, "cron": "0 6 * * *", "enabled": true}}}
	}
	var it struct {
		ID int64 `json:"id"`
	}
	e.call(t, 201, "POST", "/integrations", plexBody(true), &it)

	byType := func() map[jobs.Type]scheduleState {
		t.Helper()
		out := map[jobs.Type]scheduleState{}
		for _, sc := range e.scheduleStates(t) {
			out[sc.JobType] = sc
		}
		for _, typ := range []jobs.Type{jobs.TypeSync, jobs.TypeVerify, jobs.TypeRetention, jobs.TypePlexDBBackup} {
			if _, ok := out[typ]; !ok {
				t.Fatalf("no %s schedule: %+v", typ, out)
			}
		}
		return out
	}
	runnable := func(what string, sc scheduleState) {
		t.Helper()
		if sc.BlockedReason == nil || *sc.BlockedReason != "" || sc.NextRunAt == nil {
			t.Errorf("%s: %+v, want runnable with a next run", what, sc)
		}
	}
	blocked := func(what string, sc scheduleState, want string) {
		t.Helper()
		if sc.BlockedReason == nil || !strings.Contains(*sc.BlockedReason, want) || sc.NextRunAt != nil {
			t.Errorf("%s: %+v, want blocked (%q) without a next run", what, sc, want)
		}
	}
	for typ, sc := range byType() {
		runnable(string(typ), sc)
	}

	// Disable the destination: its sync, verify and Plex DB backup schedules are blocked.
	e.call(t, 200, "PUT", fmt.Sprintf("/destinations/%d", destID), map[string]any{"enabled": false}, nil)
	list := byType()
	for _, typ := range []jobs.Type{jobs.TypeSync, jobs.TypeVerify, jobs.TypePlexDBBackup} {
		blocked(string(typ), list[typ], `destination "NAS" is disabled`)
	}
	runnable("retention", list[jobs.TypeRetention])
	for _, typ := range []jobs.Type{jobs.TypeSync, jobs.TypeVerify, jobs.TypePlexDBBackup} {
		code, msg := e.status(t, "POST", fmt.Sprintf("/schedules/%d/run", list[typ].ID), nil)
		if code != 409 || !strings.Contains(msg, "disabled") {
			t.Errorf("run %s now: %d %q, want 409", typ, code, msg)
		}
	}
	var page jobqueue.Page[jobs.Job]
	e.call(t, 200, "GET", fmt.Sprintf("/jobs?destinationId=%d", destID), nil, &page)
	if page.TotalRecords != 0 {
		t.Fatalf("blocked schedules queued jobs: %+v", page.Records)
	}

	// Enable it again and disable the Plex server instead.
	e.call(t, 200, "PUT", fmt.Sprintf("/destinations/%d", destID), map[string]any{"enabled": true}, nil)
	e.call(t, 200, "PUT", fmt.Sprintf("/integrations/%d", it.ID), plexBody(false), nil)
	list = byType()
	for _, typ := range []jobs.Type{jobs.TypeSync, jobs.TypeVerify, jobs.TypeRetention} {
		runnable(string(typ), list[typ])
	}
	blocked("plexdb_backup", list[jobs.TypePlexDBBackup], `Plex server "Plex" is disabled`)
	if code, _ := e.status(t, "POST", fmt.Sprintf("/schedules/%d/run", list[jobs.TypePlexDBBackup].ID), nil); code != 409 {
		t.Errorf("run the Plex DB backup of a disabled server now: %d, want 409", code)
	}
	var sc scheduleState
	e.call(t, 200, "PUT", fmt.Sprintf("/schedules/%d", list[jobs.TypePlexDBBackup].ID), map[string]any{"cron": "0 7 * * *"}, &sc)
	blocked("updated plexdb_backup", sc, "disabled")
}

// TestBlockedReasonMatchesTheSchedulerGate: GET /schedules reports a schedule as blocked
// (blockedReason) exactly when the scheduler's gate (App.checkEnabled) refuses to queue its job,
// for every mix of a disabled destination, Plex server and other integration, and for params
// naming ones that do not exist.
func TestBlockedReasonMatchesTheSchedulerGate(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	destID := e.createDestination(t, "NAS", e.mkdir(t, "nas"), nil, map[string]any{"schedule": map[string]any{"cron": "0 2 * * *", "enabled": true}})
	plexBody := func(enabled bool) map[string]any {
		return map[string]any{"type": "plex", "name": "Plex", "url": "http://127.0.0.1:9", "apiKey": "gate-test-token", "enabled": enabled,
			"settings": map[string]any{"dataPath": "/plex", "backup": map[string]any{"destinationId": destID, "cron": "0 6 * * *", "enabled": true}}}
	}
	sonarrBody := func(enabled bool) map[string]any {
		return map[string]any{"type": "sonarr", "name": "Sonarr", "url": "http://127.0.0.1:9", "enabled": enabled}
	}
	var plexIt, sonarrIt struct {
		ID int64 `json:"id"`
	}
	e.call(t, 201, "POST", "/integrations", plexBody(true), &plexIt)
	e.call(t, 201, "POST", "/integrations", sonarrBody(true), &sonarrIt)

	params := []jobs.Params{
		{},
		{DestinationID: destID},
		{IntegrationID: plexIt.ID},
		{IntegrationID: sonarrIt.ID},
		{IntegrationID: plexIt.ID, DestinationID: destID},
		{IntegrationID: sonarrIt.ID, DestinationID: destID},
		{DestinationID: 999999},
		{IntegrationID: 999999},
	}
	list, err := e.app.Jobs.Store().ListSchedules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, sc := range list {
		params = append(params, sc.Params)
	}
	blockedSeen, runnableSeen := false, false
	for mask := 0; mask < 8; mask++ {
		destOn, plexOn, sonarrOn := mask&1 == 0, mask&2 == 0, mask&4 == 0
		e.call(t, 200, "PUT", fmt.Sprintf("/destinations/%d", destID), map[string]any{"enabled": destOn}, nil)
		e.call(t, 200, "PUT", fmt.Sprintf("/integrations/%d", plexIt.ID), plexBody(plexOn), nil)
		e.call(t, 200, "PUT", fmt.Sprintf("/integrations/%d", sonarrIt.ID), sonarrBody(sonarrOn), nil)
		for _, p := range params {
			reason := e.api.blockedReason(ctx, p)
			gate := e.app.checkEnabled(ctx, p)
			if (reason != "") != (gate != nil) {
				t.Errorf("destination on %v, Plex on %v, Sonarr on %v, params %+v: blockedReason %q, the scheduler's gate %v",
					destOn, plexOn, sonarrOn, p, reason, gate)
			}
			blockedSeen = blockedSeen || reason != ""
			runnableSeen = runnableSeen || reason == ""
		}
	}
	if !blockedSeen || !runnableSeen {
		t.Fatalf("the cases did not cover both outcomes (blocked %v, runnable %v)", blockedSeen, runnableSeen)
	}
}

// retainedFiles lists the regular files under a destination's retention folder.
func retainedFiles(t *testing.T, target string) []string {
	t.Helper()
	var out []string
	root := filepath.Join(target, ".bunkarr", "retention")
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && p == root {
				return nil
			}
			return err
		}
		if d.Type().IsRegular() {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestRunScheduleDryRunPreviewsRetention: POST /schedules/{id}/run {"dryRun": true} previews the
// retention schedule (the only job that permanently deletes backup data): the destination's
// retention job it queues is a dry run whose items list what would expire, and it deletes
// nothing. A preview is not a run of the schedule (lastRunAt). Without a body the run is real
// and expires what the preview listed; a malformed body is refused.
func TestRunScheduleDryRunPreviewsRetention(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	mt := time.Date(2025, 3, 14, 15, 9, 26, 0, time.UTC)
	src := e.mkdir(t, "media/movies")
	writeFile(t, filepath.Join(src, "Gone (2020)", "Gone.mkv"), strings.Repeat("g", 1000), mt)
	writeFile(t, filepath.Join(src, "Kept (2021)", "Kept.mkv"), strings.Repeat("k", 500), mt)
	dst := e.mkdir(t, "nas/backup")
	destID := e.createDestination(t, "NAS", dst, []int64{e.createSource(t, "Movies", src)}, nil)
	syncNow := func() {
		t.Helper()
		var j jobs.Job
		e.call(t, 202, "POST", fmt.Sprintf("/destinations/%d/sync", destID), nil, &j)
		if done := e.waitJob(t, j.ID); done.Status != jobs.StatusCompleted {
			t.Fatalf("sync: %s %s", done.Status, done.Error)
		}
	}
	syncNow()
	// Gone from the source: the next sync moves it into retention, and its retention period is
	// then set to have passed.
	if err := os.RemoveAll(filepath.Join(src, "Gone (2020)")); err != nil {
		t.Fatal(err)
	}
	syncNow()
	if got := retainedFiles(t, dst); len(got) != 1 {
		t.Fatalf("retained files after the sync: %v", got)
	}
	err := e.db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE destination_files SET expires_at = ? WHERE state = 'retained'`, db.FormatTime(time.Now().Add(-time.Hour)))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("%d retained records", n)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var retention scheduleState
	for _, sc := range e.scheduleStates(t) {
		if sc.JobType == jobs.TypeRetention && sc.Params.DestinationID == 0 {
			retention = sc
		}
	}
	if retention.ID == 0 {
		t.Fatal("no retention schedule")
	}
	runPath := fmt.Sprintf("/schedules/%d/run", retention.ID)
	lastRun := func() *time.Time {
		t.Helper()
		sc, err := e.app.Jobs.Store().GetSchedule(ctx, retention.ID)
		if err != nil {
			t.Fatal(err)
		}
		return sc.LastRunAt
	}
	// destinationRetention waits for the destination's retention job queued after job id after.
	destinationRetention := func(after int64) jobs.Job {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			var page jobqueue.Page[jobs.Job]
			e.call(t, 200, "GET", fmt.Sprintf("/jobs?type=retention&destinationId=%d", destID), nil, &page)
			for _, j := range page.Records {
				if j.ID > after {
					return e.waitJob(t, j.ID)
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("the retention job did not queue a job for the destination")
		return jobs.Job{}
	}

	for _, bad := range []string{`{"dryRun": "yes"}`, `{"dryrun": true, "x": 1}`, `[true]`} {
		if code, _ := e.status(t, "POST", runPath, bad); code != 400 {
			t.Errorf("run with body %s: %d, want 400", bad, code)
		}
	}

	// Preview.
	var global jobs.Job
	e.call(t, 202, "POST", runPath, map[string]any{"dryRun": true}, &global)
	if global.Type != jobs.TypeRetention || !global.DryRun || global.Trigger != jobs.TriggerManual {
		t.Fatalf("queued preview: %+v", global)
	}
	if done := e.waitJob(t, global.ID); done.Status != jobs.StatusCompleted || !done.DryRun {
		t.Fatalf("preview: %+v", done)
	}
	dry := destinationRetention(global.ID)
	if dry.Status != jobs.StatusCompleted || !dry.DryRun {
		t.Fatalf("the destination's retention preview: %+v", dry)
	}
	var items jobqueue.Page[jobs.Item]
	e.call(t, 200, "GET", fmt.Sprintf("/jobs/%d/items?action=expire", dry.ID), nil, &items)
	if items.TotalRecords != 1 || !strings.Contains(items.Records[0].RelPath, "Gone.mkv") || items.Records[0].Bytes != 1000 {
		t.Fatalf("the preview's expire items: %+v", items)
	}
	if got := retainedFiles(t, dst); len(got) != 1 {
		t.Fatalf("the preview deleted retained files: %v", got)
	}
	if lr := lastRun(); lr != nil {
		t.Fatalf("a preview was recorded as a run of the schedule: %v", lr)
	}

	// Run now (no body): expires the file the preview listed.
	var real jobs.Job
	e.call(t, 202, "POST", runPath, nil, &real)
	if real.DryRun || real.ID == global.ID {
		t.Fatalf("queued run: %+v", real)
	}
	if done := e.waitJob(t, real.ID); done.Status != jobs.StatusCompleted {
		t.Fatalf("run: %+v", done)
	}
	if expired := destinationRetention(dry.ID); expired.Status != jobs.StatusCompleted || expired.DryRun {
		t.Fatalf("the destination's retention: %+v", expired)
	}
	if got := retainedFiles(t, dst); len(got) != 0 {
		t.Fatalf("retained files after the run: %v", got)
	}
	if lastRun() == nil {
		t.Fatal("the run was not recorded")
	}
	if _, err := os.Stat(filepath.Join(dst, "movies", "Kept (2021)", "Kept.mkv")); err != nil {
		t.Fatal(err)
	}
}
