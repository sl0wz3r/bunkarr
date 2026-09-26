package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr/arrtest"
	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/mediaindex"
)

const arrTestKey = "radarr-index-key-0123456789abcdef"

// arrSetup creates a source holding the fake Radarr's files and a Radarr integration mapping
// /movies to it; it waits for the refresh its creation queued.
func arrSetup(t *testing.T, e *env) (*arrtest.Server, integrations.Integration, int64) {
	t.Helper()
	fake := arrtest.NewServer(t, arr.KindRadarr, arrTestKey)
	movies := e.mkdir(t, "media/movies")
	var list []struct {
		MovieFile *struct {
			Path string `json:"path"`
			Size int64  `json:"size"`
		} `json:"movieFile"`
	}
	if err := json.Unmarshal(arrtest.Fixture(t, arr.KindRadarr, "movie.json"), &list); err != nil {
		t.Fatal(err)
	}
	for _, m := range list {
		if m.MovieFile == nil {
			continue
		}
		p := filepath.Join(movies, strings.TrimPrefix(m.MovieFile.Path, "/movies/"))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Truncate(m.MovieFile.Size)
		_ = f.Close()
	}
	srcID := e.createSource(t, "Movies", movies)
	var scan jobs.Job
	e.call(t, 202, "POST", fmt.Sprintf("/sources/%d/scan", srcID), nil, &scan)
	e.waitJob(t, scan.ID)
	var it integrations.Integration
	e.call(t, 201, "POST", "/integrations", map[string]any{"type": "radarr", "name": "Radarr", "url": fake.URL, "apiKey": arrTestKey,
		"settings": map[string]any{"pathMappings": []map[string]string{{"arr": "/movies", "local": movies}}}}, &it)
	j := waitRefresh(t, e, it.ID, 1)
	if j.Status != jobs.StatusCompleted || j.Trigger != jobs.TriggerManual {
		t.Fatalf("the refresh queued by the create = %+v", j)
	}
	return fake, it, srcID
}

// waitRefresh waits until integration id has n refresh jobs and the newest is final.
func waitRefresh(t *testing.T, e *env, id int64, n int) jobs.Job {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var page jobqueue.Page[jobs.Job]
		e.call(t, 200, "GET", "/jobs?type=refresh&pageSize=100", nil, &page)
		var mine []jobs.Job
		for _, j := range page.Records {
			if j.Params.IntegrationID == id {
				mine = append(mine, j)
			}
		}
		if len(mine) >= n {
			return e.waitJob(t, mine[0].ID)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no refresh job %d of integration %d", n, id)
	return jobs.Job{}
}

func TestArrIndexEndpoints(t *testing.T) {
	e := newEnv(t, nil)
	fake, it, srcID := arrSetup(t, e)

	var idx mediaindex.IndexView
	e.call(t, 200, "GET", fmt.Sprintf("/integrations/%d/index", it.ID), nil, &idx)
	if idx.Status != "ok" || !idx.Fresh || !idx.InstanceMatches || idx.RefreshedAt == nil || idx.StaleAfterHours != 24 ||
		!strings.Contains(string(idx.Stats), `"filesMapped":3`) || idx.AppVersion == "" {
		t.Fatalf("index = %+v %s", idx, idx.Stats)
	}
	var meta mediaindex.Meta
	e.call(t, 200, "GET", fmt.Sprintf("/integrations/%d/arr/metadata", it.ID), nil, &meta)
	if len(meta.QualityProfiles) != 6 || len(meta.Tags) != 2 || len(meta.RootFolders) != 1 || meta.RootFolders[0].SourceID == nil ||
		*meta.RootFolders[0].SourceID != srcID || meta.RefreshedAt == nil {
		t.Fatalf("metadata = %+v", meta)
	}
	var roots []arrRootFolderView
	e.call(t, 200, "GET", fmt.Sprintf("/integrations/%d/arr/rootfolders", it.ID), nil, &roots)
	if len(roots) != 1 || roots[0].Path != "/movies" || !roots[0].Exists || roots[0].SourceID == nil || *roots[0].SourceID != srcID {
		t.Fatalf("root folders = %+v", roots)
	}
	var page mediaindex.UnmappedPage
	e.call(t, 200, "GET", fmt.Sprintf("/catalog/unmapped?integrationId=%d", it.ID), nil, &page)
	if page.TotalRecords != 0 || page.Records == nil || page.PageSize != 50 {
		t.Fatalf("unmapped = %+v", page)
	}
	// The refresh schedule mirrors the settings and is overlaid on the integration.
	var sc []scheduleView
	e.call(t, 200, "GET", "/schedules", nil, &sc)
	found := false
	for _, s := range sc {
		if s.JobType == jobs.TypeRefresh && s.Params.IntegrationID == it.ID {
			found = true
			if s.Cron != integrations.DefaultArrRefreshCron || !s.Enabled || s.Description != "Refresh of Radarr" {
				t.Fatalf("refresh schedule = %+v", s)
			}
			e.call(t, 200, "PUT", fmt.Sprintf("/schedules/%d", s.ID), map[string]any{"cron": "0 3 * * *", "enabled": false}, nil)
		}
	}
	if !found {
		t.Fatalf("no refresh schedule in %+v", sc)
	}
	var got integrations.Integration
	e.call(t, 200, "GET", fmt.Sprintf("/integrations/%d", it.ID), nil, &got)
	as, _ := got.ArrSettings()
	if as.Refresh.Cron != "0 3 * * *" || as.Refresh.Enabled {
		t.Fatalf("integration refresh settings = %+v", as.Refresh)
	}

	// A manual refresh and a dry run.
	var job jobs.Job
	e.call(t, 202, "POST", fmt.Sprintf("/integrations/%d/refresh", it.ID), map[string]any{"dryRun": true}, &job)
	if job.Type != jobs.TypeRefresh || !job.DryRun || job.Trigger != jobs.TriggerManual {
		t.Fatalf("refresh = %+v", job)
	}
	if done := e.waitJob(t, job.ID); done.Status != jobs.StatusCompleted {
		t.Fatalf("dry run = %+v", done)
	}
	e.call(t, 202, "POST", fmt.Sprintf("/integrations/%d/refresh", it.ID), nil, &job)
	if done := e.waitJob(t, job.ID); done.Status != jobs.StatusCompleted || !strings.Contains(done.Summary, "4 movies") {
		t.Fatalf("refresh = %+v", done)
	}

	// Upstream failure of the live root folders: 502.
	fake.SetStatus("GET", "rootfolder", http.StatusInternalServerError)
	if code, _ := e.status(t, "GET", fmt.Sprintf("/integrations/%d/arr/rootfolders", it.ID), nil); code != http.StatusBadGateway {
		t.Fatalf("root folders upstream failure: %d", code)
	}

	// Not for other types.
	var plexIt integrations.Integration
	e.call(t, 201, "POST", "/integrations", map[string]any{"type": "plex", "name": "Plex", "url": "http://127.0.0.1:1", "apiKey": "tok"}, &plexIt)
	for _, p := range []string{"/refresh", "/webhook/key"} {
		if code, _ := e.status(t, "POST", fmt.Sprintf("/integrations/%d%s", plexIt.ID, p), nil); code != 400 {
			t.Fatalf("POST %s on Plex: %d", p, code)
		}
	}
	for _, p := range []string{"/arr/metadata", "/arr/rootfolders"} {
		if code, _ := e.status(t, "GET", fmt.Sprintf("/integrations/%d%s", plexIt.ID, p), nil); code != 400 {
			t.Fatalf("GET %s on Plex: %d", p, code)
		}
	}
	e.call(t, 200, "GET", fmt.Sprintf("/catalog/unmapped?integrationId=%d", plexIt.ID), nil, &page)
	if page.TotalRecords != 0 {
		t.Fatalf("Plex unmapped = %+v", page)
	}
	if code, _ := e.status(t, "GET", "/catalog/unmapped?integrationId=999", nil); code != 404 {
		t.Fatalf("unknown integration: %d", code)
	}
	if code, _ := e.status(t, "GET", fmt.Sprintf("/integrations/%d/index", 999), nil); code != 404 {
		t.Fatalf("unknown index: %d", code)
	}
	// A disabled integration is not refreshed.
	e.call(t, 200, "PUT", fmt.Sprintf("/integrations/%d", it.ID), map[string]any{"name": "Radarr", "url": it.URL, "enabled": false}, nil)
	if code, msg := e.status(t, "POST", fmt.Sprintf("/integrations/%d/refresh", it.ID), nil); code != 409 || !strings.Contains(msg, "disabled") {
		t.Fatalf("refresh of a disabled integration: %d %q", code, msg)
	}
}

func TestArrSaveQueuesRefresh(t *testing.T) {
	e := newEnv(t, nil)
	_, it, _ := arrSetup(t, e)
	settings := func(local string) map[string]any {
		return map[string]any{"pathMappings": []map[string]string{{"arr": "/movies", "local": local}}}
	}
	as, _ := it.ArrSettings()
	local := as.PathMappings[0].Local
	// A rename queues nothing.
	e.call(t, 200, "PUT", fmt.Sprintf("/integrations/%d", it.ID), map[string]any{"name": "Radarr 2", "url": it.URL, "settings": settings(local)}, nil)
	// New mappings queue a refresh.
	e.call(t, 200, "PUT", fmt.Sprintf("/integrations/%d", it.ID), map[string]any{"name": "Radarr 2", "url": it.URL,
		"settings": settings(filepath.Join(local, "x"))}, nil)
	waitRefresh(t, e, it.ID, 2)
	var page jobqueue.Page[jobs.Job]
	e.call(t, 200, "GET", "/jobs?type=refresh&pageSize=100", nil, &page)
	if page.TotalRecords != 2 {
		t.Fatalf("refresh jobs = %d, want 2 (create, new mappings)", page.TotalRecords)
	}
	// A new key queues one too.
	e.call(t, 200, "PUT", fmt.Sprintf("/integrations/%d", it.ID), map[string]any{"name": "Radarr 2", "url": it.URL, "apiKey": arrTestKey}, nil)
	waitRefresh(t, e, it.ID, 3)
}

func TestWebhookKeyEndpoint(t *testing.T) {
	e := newEnv(t, nil)
	_, it, _ := arrSetup(t, e)
	hex32 := regexp.MustCompile(`^[0-9a-f]{32}$`)
	var k1, k2, k3 struct {
		Key string `json:"key"`
	}
	e.call(t, 200, "POST", fmt.Sprintf("/integrations/%d/webhook/key", it.ID), map[string]any{"rotate": false}, &k1)
	e.call(t, 200, "POST", fmt.Sprintf("/integrations/%d/webhook/key", it.ID), nil, &k2)
	if !hex32.MatchString(k1.Key) || k1.Key != k2.Key {
		t.Fatalf("reveal = %q, %q", k1.Key, k2.Key)
	}
	e.call(t, 200, "POST", fmt.Sprintf("/integrations/%d/webhook/key", it.ID), map[string]any{"rotate": true}, &k3)
	if !hex32.MatchString(k3.Key) || k3.Key == k1.Key {
		t.Fatalf("rotate = %q", k3.Key)
	}
	if _, ok := e.app.Integrations.MatchWebhookKey(k1.Key); ok {
		t.Fatal("the old key still matches")
	}
	if id, ok := e.app.Integrations.MatchWebhookKey(k3.Key); !ok || id.IntegrationID != it.ID {
		t.Fatal("the new key does not match")
	}
	if code, _ := e.status(t, "POST", fmt.Sprintf("/integrations/%d/webhook/key", it.ID), map[string]any{"rotate": "yes"}); code != 400 {
		t.Fatalf("bad body: %d", code)
	}
	// No other response contains the webhook key or the *arr's API key.
	for _, p := range []string{"/integrations", fmt.Sprintf("/integrations/%d", it.ID), fmt.Sprintf("/integrations/%d/index", it.ID),
		fmt.Sprintf("/integrations/%d/arr/metadata", it.ID), fmt.Sprintf("/integrations/%d/arr/rootfolders", it.ID),
		"/catalog/unmapped", "/schedules", "/jobs?type=refresh"} {
		_, raw := e.raw(t, "GET", p, nil)
		if strings.Contains(string(raw), k3.Key) || strings.Contains(string(raw), arrTestKey) {
			t.Fatalf("GET %s leaks a key: %s", p, raw)
		}
	}
}

func TestStartupRefreshesAndSchedules(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	// A Phase 1 install (no *arr integration) queues no refresh at start.
	var page jobqueue.Page[jobs.Job]
	e.call(t, 200, "GET", "/jobs?type=refresh", nil, &page)
	if page.TotalRecords != 0 {
		t.Fatalf("refresh jobs without an *arr: %d", page.TotalRecords)
	}
	_, it, _ := arrSetup(t, e)
	e.app.queueStartupRefreshes(ctx)
	e.call(t, 200, "GET", "/jobs?type=refresh&pageSize=100", nil, &page)
	startup := 0
	for _, j := range page.Records {
		if j.Trigger == jobs.TriggerStartup && j.Params.IntegrationID == it.ID {
			startup++
		}
	}
	if startup != 1 {
		t.Fatalf("start-up refreshes = %d in %+v", startup, page.Records)
	}
	// A missing refresh schedule is created at start; an edited one is kept.
	st := e.app.Jobs.Store()
	list, _ := st.ListSchedules(ctx)
	for _, sc := range list {
		if sc.JobType == jobs.TypeRefresh {
			if err := st.DeleteSchedule(ctx, sc.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := e.app.ensureArrRefreshSchedules(ctx); err != nil {
		t.Fatal(err)
	}
	list, _ = st.ListSchedules(ctx)
	var ref *jobqueue.Schedule
	for i, sc := range list {
		if sc.JobType == jobs.TypeRefresh && sc.Params.IntegrationID == it.ID {
			ref = &list[i]
		}
	}
	if ref == nil || ref.Cron != integrations.DefaultArrRefreshCron {
		t.Fatalf("refresh schedule = %+v", ref)
	}
	if _, err := st.UpdateSchedule(ctx, ref.ID, "5 5 * * *", true); err != nil {
		t.Fatal(err)
	}
	if err := e.app.ensureArrRefreshSchedules(ctx); err != nil {
		t.Fatal(err)
	}
	after, _ := st.GetSchedule(ctx, ref.ID)
	if after.Cron != "5 5 * * *" {
		t.Fatalf("start-up overwrote an edited schedule: %+v", after)
	}
	// Deleting the integration removes its refresh schedule (once its refreshes finished).
	waitRefresh(t, e, it.ID, 2)
	e.call(t, 204, "DELETE", fmt.Sprintf("/integrations/%d", it.ID), nil, nil)
	list, _ = st.ListSchedules(ctx)
	for _, sc := range list {
		if sc.Params.IntegrationID == it.ID {
			t.Fatalf("schedule left: %+v", sc)
		}
	}
}
