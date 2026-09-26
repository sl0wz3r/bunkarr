package mediaindex

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// changeRadarr changes the fake Radarr after a first refresh: movie 1 deleted with its file,
// movie 2 upgraded (new file id and name), movie 3 moved to another folder, movie 6 added with a
// file, movie 4 renamed (no file change).
func changeRadarr(e *testEnv) {
	e.t.Helper()
	e.writeFile("/movies/His Girl Friday (1940)/His Girl Friday (1940) [Remux-2160p].mkv", 99)
	e.writeFile("/movies/Charade/Charade (1963) [Bluray-1080p].mkv", 3412521)
	e.writeFile("/movies/Heat (1995)/Heat (1995).mkv", 5)
	e.editList("movie", "movie.json", func(list []map[string]any) []map[string]any {
		var out []map[string]any
		for _, m := range list {
			switch m["id"] {
			case 1.0:
				continue
			case 2.0:
				mf := m["movieFile"].(map[string]any)
				mf["id"], mf["path"], mf["size"] = 77, "/movies/His Girl Friday (1940)/His Girl Friday (1940) [Remux-2160p].mkv", 99
				m["movieFileId"] = 77
			case 3.0:
				m["path"] = "/movies/Charade"
				m["movieFile"].(map[string]any)["path"] = "/movies/Charade/Charade (1963) [Bluray-1080p].mkv"
			case 4.0:
				m["title"] = "The General (1926 cut)"
			}
			out = append(out, m)
		}
		return append(out, map[string]any{"id": 6, "title": "Heat", "year": 1995, "path": "/movies/Heat (1995)", "rootFolderPath": "/movies",
			"hasFile": true, "movieFileId": 60, "monitored": true, "qualityProfileId": 6,
			"movieFile": map[string]any{"id": 60, "path": "/movies/Heat (1995)/Heat (1995).mkv", "size": 5}})
	})
	var list []map[string]any
	_ = json.Unmarshal(mustJSON(e.t, e, "movie"), &list)
	for _, m := range list {
		if m["id"] == 3.0 {
			b, _ := json.Marshal(m)
			e.srv.SetJSON(http.MethodGet, "movie/3", b)
		}
	}
}

// indexSnapshot is what the crash matrix compares: every item (path, deleted) and file (item,
// path) by *arr id.
type indexSnapshot struct {
	Items map[int64][2]any
	Files map[int64][2]any
}

func snapshot(e *testEnv) indexSnapshot {
	s := indexSnapshot{Items: map[int64][2]any{}, Files: map[int64][2]any{}}
	byRow := map[int64]int64{}
	for id, it := range e.items() {
		s.Items[id] = [2]any{it.Path, it.DeletedAt != nil}
		byRow[it.ID] = id
	}
	for id, f := range e.files() {
		s.Files[id] = [2]any{byRow[f.ItemID], f.Path}
	}
	return s
}

// TestCrashMatrixRefresh crashes a reconciling full refresh (batches of two rows) and a webhook
// refresh at every fault point, resumes the job, and checks that the index ends as a clean run
// leaves it, that no removal happened before the complete fetch, and that the follow-up syncs
// still cover every changed folder.
func TestCrashMatrixRefresh(t *testing.T) {
	// The clean run.
	clean := newEnv(t, arr.KindRadarr)
	clean.full()
	changeRadarr(clean)
	clean.full()
	want := snapshot(clean)
	wantSyncs := syncTargets(clean.enq.queued())
	if len(wantSyncs) != 1 {
		t.Fatalf("clean run queued %v", wantSyncs)
	}

	cases := []struct {
		point string
		n     int
	}{
		{PointAfterIntents, 1}, {PointAfterIntents, 2}, {PointAfterIntents, 4},
		{PointAfterBatch, 1}, {PointAfterBatch, 2}, {PointAfterBatch, 3},
		{PointBeforeMarkDeleted, 1}, {PointAfterFollowUps, 1},
	}
	for _, c := range cases {
		t.Run(c.point+"#"+itoa(c.n), func(t *testing.T) {
			e := newEnv(t, arr.KindRadarr)
			e.full()
			first := *e.state().RefreshedAt
			changeRadarr(e)
			r := e.newRunner(2)
			job := jobs.Job{ID: 50, Type: jobs.TypeRefresh, Trigger: jobs.TriggerSchedule, Attempt: 1, Params: jobs.Params{IntegrationID: e.it.ID}}
			items := &memItems{}
			crashed, err := crashRun(t, e, r, job, items, faultinject.CrashAt(c.point, c.n))
			if !crashed {
				t.Fatalf("did not reach %s #%d (err %v)", c.point, c.n, err)
			}
			if c.point != PointAfterFollowUps {
				if e.items()[1].DeletedAt != nil {
					t.Fatal("an item was marked deleted before the complete fetch")
				}
				if got := e.state().RefreshedAt; !got.Equal(first) {
					t.Fatalf("refreshed_at moved before the refresh completed: %v", got)
				}
			}
			e.clock.Advance(1)
			job.Attempt, job.Trigger = 2, jobs.TriggerResume
			if _, rep, err := e.runJob(r, job, items); err != nil {
				t.Fatalf("resume: %v\n%s", err, rep)
			}
			if got := snapshot(e); !reflect.DeepEqual(got, want) {
				t.Fatalf("index after resume =\n%v\nwant\n%v", got, want)
			}
			if got := syncTargets(e.enq.queued()); !reflect.DeepEqual(got, wantSyncs) {
				t.Fatalf("syncs = %v, want %v", got, wantSyncs)
			}
			for _, it := range items.all() {
				if it.Status != jobs.ItemDone {
					t.Fatalf("intent left %s: %+v", it.Status, it)
				}
			}
			if s := e.state(); s.RefreshedAt == nil || !s.RefreshedAt.After(first) || s.Status != StatusOK {
				t.Fatalf("state = %+v", s)
			}
		})
	}

	// The webhook path: a crash after the item's row changed must still sync its old folder.
	for _, point := range []string{PointAfterIntents, PointAfterBatch, PointAfterFollowUps} {
		t.Run("targeted "+point, func(t *testing.T) {
			e := newEnv(t, arr.KindRadarr)
			e.full()
			changeRadarr(e)
			job := jobs.Job{ID: 60, Type: jobs.TypeRefresh, Trigger: jobs.TriggerWebhook, Attempt: 1,
				Params: jobs.Params{IntegrationID: e.it.ID, ArrItemIDs: []int64{3}, SyncAfter: true}}
			items := &memItems{}
			if crashed, err := crashRun(t, e, e.runner, job, items, faultinject.CrashAt(point, 1)); !crashed {
				t.Fatalf("did not reach %s (err %v)", point, err)
			}
			e.clock.Advance(1)
			job.Attempt, job.Trigger = 2, jobs.TriggerResume
			if _, rep, err := e.runJob(e.runner, job, items); err != nil {
				t.Fatalf("resume: %v\n%s", err, rep)
			}
			want := []string{"1/[" + itoa(int(e.src.ID)) + "]:Charade,Charade (1963)"}
			if got := syncTargets(e.enq.queued()); !reflect.DeepEqual(got, want) {
				t.Fatalf("syncs = %v, want %v", got, want)
			}
			if q := e.enq.queued(); q[0].Trigger != jobs.TriggerWebhook {
				t.Fatalf("trigger = %s", q[0].Trigger)
			}
			if it := e.items()[3]; it.Path != "/movies/Charade" {
				t.Fatalf("movie 3 = %+v", it)
			}
		})
	}
}
