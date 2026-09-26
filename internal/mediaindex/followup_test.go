package mediaindex

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// newRun returns a run of env e for calling the follow-up logic directly.
func newRun(t *testing.T, e *testEnv, job jobs.Job) (*run, *memReporter, *memItems) {
	t.Helper()
	loc, err := e.cat.Locator(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s, err := e.it.ArrSettings()
	if err != nil {
		t.Fatal(err)
	}
	rep, items := &memReporter{}, &memItems{}
	job.Params.IntegrationID = e.it.ID
	rn := &run{r: e.runner, job: job, env: jobs.Env{Reporter: rep, Items: items}, it: e.it, settings: s, app: "Radarr", kind: KindMovie,
		loc: loc, unmapped: map[string]*UnmappedFolder{}, stats: Stats{FollowUpJobs: []int64{}}}
	return rn, rep, items
}

func TestFollowUpTargets(t *testing.T) {
	e := newEnv(t, arr.KindRadarr)
	src := strconv.FormatInt(e.src.ID, 10)
	ctx := context.Background()
	cases := []struct {
		name    string
		intents []intent
		want    []string
		warn    string
	}{
		{"old and new folder", []intent{{Title: "Charade", OldFolder: "/movies/Charade (1963)", NewFolder: "/movies/Charade (1963)", HasFiles: true}},
			[]string{"1/[" + src + "]:Charade (1963)"}, ""},
		{"a folder at the source root", []intent{{Title: "Root", NewFolder: "/movies"}}, []string{"1/[" + src + "]:*"}, ""},
		{"unmapped folder", []intent{{Title: "Nosferatu", NewFolder: "/movies-4k/Nosferatu (1922)", RootFolder: "/movies-4k", HasFiles: true}},
			nil, "add a mapping for /movies-4k"},
		{"missing folder of an item with files", []intent{{Title: "Gone", NewFolder: "/movies/Gone (2000)", HasFiles: true}},
			[]string{"1/[" + src + "]:*"}, "which does not exist"},
		{"missing old folder is fine", []intent{{Title: "Moved", OldFolder: "/movies/Old", NewFolder: "/movies/Charade (1963)", HasFiles: true}},
			[]string{"1/[" + src + "]:Charade (1963),Old"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e.enq.reset()
			rn, rep, _ := newRun(t, e, jobs.Job{Trigger: jobs.TriggerManual})
			if err := rn.queueFollowUps(ctx, c.intents); err != nil {
				t.Fatal(err)
			}
			if got := syncTargets(e.enq.queued()); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("syncs = %v, want %v (%s)", got, c.want, rep)
			}
			if c.warn != "" && !rep.warned(c.warn) {
				t.Fatalf("warnings %v, want %q", rep.warns, c.warn)
			}
			if q := e.enq.queued(); len(q) > 0 && q[0].Trigger != jobs.TriggerManual {
				t.Fatalf("trigger = %s", q[0].Trigger)
			}
		})
	}
}

func TestFollowUpLimitsAndSources(t *testing.T) {
	e := newEnv(t, arr.KindRadarr)
	ctx := context.Background()
	// More than MaxTargetPaths folders: the source is synced whole.
	rn, _, _ := newRun(t, e, jobs.Job{Trigger: jobs.TriggerSchedule})
	targets := map[int64]map[string]bool{e.src.ID: {}}
	for i := range jobs.MaxTargetPaths + 1 {
		targets[e.src.ID]["f"+strconv.Itoa(i)] = true
	}
	if err := rn.queueSyncs(ctx, targets, nil); err != nil {
		t.Fatal(err)
	}
	if got := syncTargets(e.enq.queued()); !reflect.DeepEqual(got, []string{"1/[" + strconv.FormatInt(e.src.ID, 10) + "]:*"}) {
		t.Fatalf("syncs = %v", got)
	}
	// A disabled source gets no sync.
	e.enq.reset()
	off := false
	src, err := e.cat.Update(ctx, e.src.ID, catalog.SourceInput{Name: e.src.Name, Path: e.src.Path, Enabled: &off})
	if err != nil {
		t.Fatal(err)
	}
	rn, _, _ = newRun(t, e, jobs.Job{Trigger: jobs.TriggerSchedule})
	if err := rn.queueSyncs(ctx, map[int64]map[string]bool{src.ID: {"x": true}}, nil); err != nil || len(e.enq.queued()) != 0 {
		t.Fatalf("disabled source: %v, %v", err, e.enq.queued())
	}
}

func TestFollowUpTriggers(t *testing.T) {
	e := newEnv(t, arr.KindRadarr)
	for _, c := range []struct {
		job  jobs.Job
		want jobs.Trigger
	}{
		{jobs.Job{Trigger: jobs.TriggerSchedule}, jobs.TriggerSchedule},
		{jobs.Job{Trigger: jobs.TriggerStartup}, jobs.TriggerStartup},
		{jobs.Job{Trigger: jobs.TriggerResume}, jobs.TriggerStartup},
		{jobs.Job{Trigger: jobs.TriggerManual, Params: jobs.Params{SyncAfter: true}}, jobs.TriggerWebhook},
		{jobs.Job{Trigger: jobs.TriggerResume, Params: jobs.Params{SyncAfter: true}}, jobs.TriggerWebhook},
	} {
		rn, _, _ := newRun(t, e, c.job)
		if got := rn.followUpTrigger(); got != c.want {
			t.Errorf("%+v: trigger %s, want %s", c.job, got, c.want)
		}
	}
}

func TestFollowUpEnqueueFailureMarksIntents(t *testing.T) {
	e := newEnv(t, arr.KindRadarr)
	e.full()
	changeRadarr(e)
	e.enq.fail = errors.New("queue is closed")
	res, rep, err := e.refresh(jobs.Params{}, false, jobs.TriggerSchedule)
	if err != nil {
		t.Fatal(err)
	}
	if res.Warnings == 0 || !rep.warned("Could not queue the follow-up syncs") {
		t.Fatalf("warnings = %v", rep.warns)
	}
}

func TestUntargetedSyncAfterQueuesEverySource(t *testing.T) {
	e := newEnv(t, arr.KindRadarr)
	e.full()
	res, rep, err := e.refresh(jobs.Params{SyncAfter: true}, false, jobs.TriggerWebhook)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"1/[" + strconv.FormatInt(e.src.ID, 10) + "]:*"}
	if got := syncTargets(e.enq.queued()); !reflect.DeepEqual(got, want) || e.enq.queued()[0].Trigger != jobs.TriggerWebhook {
		t.Fatalf("syncs = %v (%s)", got, rep)
	}
	if st := res.Stats.(Stats); len(st.FollowUpJobs) != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestTargetedDryRunCountsFileRemovals(t *testing.T) {
	e := newEnv(t, arr.KindRadarr)
	e.full()
	m := fixtureMovie(t, e, 2)
	m["hasFile"], m["movieFileId"] = false, 0
	delete(m, "movieFile")
	b, _ := json.Marshal(m)
	e.srv.SetJSON(http.MethodGet, "movie/2", b)
	res, _, err := e.refresh(jobs.Params{ArrItemIDs: []int64{2}}, true, jobs.TriggerManual)
	if err != nil {
		t.Fatal(err)
	}
	if st := res.Stats.(Stats); st.FilesDeleted != 1 || st.ItemsUpdated != 1 || len(e.files()) != 3 {
		t.Fatalf("stats = %+v", st)
	}
	// The real run removes it.
	res, _, err = e.refresh(jobs.Params{ArrItemIDs: []int64{2}}, false, jobs.TriggerManual)
	if err != nil || res.Stats.(Stats).FilesDeleted != 1 || len(e.files()) != 2 {
		t.Fatalf("real run = %+v, %v", res.Stats, err)
	}
}

func TestTargetedRefreshSonarrAndLidarr(t *testing.T) {
	for _, kind := range []arr.Kind{arr.KindSonarr, arr.KindLidarr} {
		t.Run(string(kind), func(t *testing.T) {
			e := newEnv(t, kind)
			e.full()
			res, rep, err := e.refresh(jobs.Params{ArrItemIDs: []int64{1}, SyncAfter: true}, false, jobs.TriggerWebhook)
			if err != nil {
				t.Fatalf("%v\n%s", err, rep)
			}
			st := res.Stats.(Stats)
			if st.Items != 1 || st.ItemsUpdated != 0 || st.FilesDeleted != 0 || len(e.enq.queued()) != 1 {
				t.Fatalf("stats = %+v, queued %v", st, e.enq.queued())
			}
		})
	}
}

func TestRefreshOverlappingSourcesLocateLongest(t *testing.T) {
	e := newEnv(t, arr.KindRadarr)
	inner, err := e.cat.Create(context.Background(), catalog.SourceInput{Name: "Charade", Path: e.localOf("/movies/Charade (1963)")})
	if err != nil {
		t.Fatal(err)
	}
	e.dests = append(e.dests, FollowUpDestination{ID: 2, Enabled: true, SourceIDs: []int64{inner.ID}, SyncOnArrChange: true})
	e.full()
	f := e.files()[3]
	if f.Location == nil || f.Location.SourceID != inner.ID || f.Location.Rel != "Charade (1963) [Bluray-1080p].mkv" {
		t.Fatalf("file 3 location = %+v", f.Location)
	}
	// A change of movie 3 syncs both sources: the outer by folder, the inner whole (its root).
	e.editList("movie", "movie.json", func(list []map[string]any) []map[string]any {
		movieByID(list, 3)["movieFile"].(map[string]any)["id"] = 33
		return list
	})
	e.full()
	want := []string{"1/[" + strconv.FormatInt(e.src.ID, 10) + "]:Charade (1963)", "2/[" + strconv.FormatInt(inner.ID, 10) + "]:*"}
	if got := syncTargets(e.enq.queued()); !reflect.DeepEqual(got, want) {
		t.Fatalf("syncs = %v, want %v", got, want)
	}
}
