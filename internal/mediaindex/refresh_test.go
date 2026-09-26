package mediaindex

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr/arrtest"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

func TestFullRefreshRadarr(t *testing.T) {
	e := newEnv(t, arr.KindRadarr)
	st, rep := e.full()
	want := Stats{IntegrationID: e.it.ID, IntegrationType: integrations.TypeRadarr, AppVersion: st.AppVersion,
		Items: 4, ItemsAdded: 4, Files: 3, FilesMapped: 3, UnmappedFolders: []UnmappedFolder{}, InaccessibleRootFolders: []string{},
		FileDate: "none", Requests: 6, FollowUpJobs: []int64{}, DurationMs: st.DurationMs}
	if !reflect.DeepEqual(st, want) {
		t.Fatalf("stats =\n%+v\nwant\n%+v\nlog: %s", st, want, rep)
	}
	if st.AppVersion == "" || len(rep.warns) != 0 {
		t.Fatalf("version %q, warnings %v", st.AppVersion, rep.warns)
	}
	items := e.items()
	nld := items[1]
	if nld.Title != "Night of the Living Dead" || nld.Year != 1968 || nld.Kind != KindMovie || nld.ExternalIDs.TMDB == 0 ||
		nld.ExternalIDs.IMDB == "" || nld.Path != "/movies/Night of the Living Dead (1968)" || nld.RootFolder != "/movies" ||
		nld.QualityProfileID != 6 || !slices.Equal(nld.Tags, []int64{1}) || !nld.Detail.HasFile || nld.Detail.MinimumAvailability == "" ||
		nld.AddedAt == nil || nld.DeletedAt != nil {
		t.Fatalf("movie 1 = %+v", nld)
	}
	if g := items[4]; g.Detail.HasFile || !slices.Equal(g.Tags, []int64{1, 2}) {
		t.Fatalf("movie 4 (no file) = %+v", g)
	}
	files := e.files()
	if len(files) != 3 {
		t.Fatalf("files = %v", files)
	}
	f := files[4]
	wantLocal := e.localOf("/movies/Night of the Living Dead (1968)/Night of the Living Dead (1968) [Bluray-1080p].mkv")
	if f.ItemID != nld.ID || f.Size != 3412521 || f.LocalPath != wantLocal || f.Location == nil || f.Location.SourceID != e.src.ID ||
		f.Location.Rel != "Night of the Living Dead (1968)/Night of the Living Dead (1968) [Bluray-1080p].mkv" ||
		f.Quality == "" || f.DateAdded == nil || f.Detail.RelativePath == "" {
		t.Fatalf("file 4 = %+v (loc %+v)", f, f.Location)
	}
	s := e.state()
	if s.Status != StatusOK || s.RefreshedAt == nil || !s.RefreshedAt.Equal(e.clock.Now()) || s.InstanceID != e.it.URL ||
		s.AppVersion != st.AppVersion || s.Error != "" {
		t.Fatalf("state = %+v", s)
	}
	m, err := e.runner.Store().Meta(context.Background(), nil, e.it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.QualityProfiles) != 6 || len(m.Tags) != 2 || m.Tags[0].Label != "bunkarr-full" || len(m.RootFolders) != 1 ||
		m.RootFolders[0].Path != "/movies" || !m.RootFolders[0].Accessible || m.RootFolders[0].SourceID == nil ||
		*m.RootFolders[0].SourceID != e.src.ID || len(m.MetadataProfiles) != 0 || m.RefreshedAt == nil {
		t.Fatalf("meta = %+v", m)
	}
	// The first complete refresh has nothing to reconcile against: no follow-up.
	if q := e.enq.queued(); len(q) != 0 {
		t.Fatalf("first refresh queued %v", q)
	}
	fr, err := e.runner.Store().Freshness(context.Background(), nil, e.it)
	if err != nil || !fr.Fresh || !fr.InstanceMatches || fr.StaleAfterHours != 24 {
		t.Fatalf("freshness = %+v, %v", fr, err)
	}
	// A second refresh changes nothing and queues nothing.
	st2, _ := e.full()
	if st2.ItemsAdded != 0 || st2.ItemsUpdated != 0 || st2.ItemsDeleted != 0 || st2.FilesDeleted != 0 || st2.ChangedItems != 0 ||
		len(e.enq.queued()) != 0 {
		t.Fatalf("second refresh = %+v, queued %v", st2, e.enq.queued())
	}
}

func TestFullRefreshSonarrEpisodeMapping(t *testing.T) {
	e := newEnv(t, arr.KindSonarr)
	st, rep := e.full()
	if st.Items != 2 || st.Files != 6 || st.FilesMapped != 6 || st.FilesMismatched != 0 || st.Requests != 1+4+1+4 {
		t.Fatalf("stats = %+v\n%s", st, rep)
	}
	// The recycle bin /tv/.recycle is inside the source, which does not exclude it.
	if st.RecycleBin == nil || st.RecycleBin.Path != "/tv/.recycle" || st.RecycleBin.SourceID == nil || *st.RecycleBin.SourceID != e.src.ID ||
		st.RecycleBin.RelPath != ".recycle" || st.RecycleBin.Excluded || !rep.warned("recycle bin /tv/.recycle") {
		t.Fatalf("recycle bin = %+v, warnings %v", st.RecycleBin, rep.warns)
	}
	items := e.items()
	bh := items[1]
	d := bh.Detail
	if bh.Kind != KindSeries || bh.ExternalIDs != (ExternalIDs{TVDB: 71471, TMDB: 1930, IMDB: "tt0055662", TVMaze: 2139}) || d.SeriesType == "" || d.SeasonFolder == nil || d.UseSceneNumbering == nil ||
		len(d.Seasons) == 0 || len(d.Episodes) < 100 || !d.HasFile {
		t.Fatalf("series 1 = %+v", bh)
	}
	for i := 1; i < len(d.Episodes); i++ {
		a, b := d.Episodes[i-1], d.Episodes[i]
		if a.Season > b.Season || a.Season == b.Season && a.Episode >= b.Episode {
			t.Fatalf("episodes are not ordered: %v then %v", a, b)
		}
	}
	files := e.files()
	multi := files[7]
	wantEps := []FileEpisode{{EpisodeID: 16, SeasonNumber: 1, EpisodeNumber: 4}, {EpisodeID: 17, SeasonNumber: 1, EpisodeNumber: 5}}
	got := slices.Clone(multi.Detail.Episodes)
	for i := range got {
		got[i].TVDBID = 0
	}
	if !reflect.DeepEqual(got, wantEps) || !strings.Contains(multi.Detail.RelativePath, "S01E04-E05") {
		t.Fatalf("multi-episode file 7 = %+v", multi.Detail)
	}
	if one := files[5].Detail.Episodes; len(one) != 1 || one[0].EpisodeNumber != 1 {
		t.Fatalf("file 5 episodes = %+v", one)
	}
	if f3 := files[3]; f3.ItemID != items[2].ID {
		t.Fatalf("file 3 belongs to item %d, want series 2", f3.ItemID)
	}
}

func TestFullRefreshLidarr(t *testing.T) {
	e := newEnv(t, arr.KindLidarr)
	st, rep := e.full()
	if st.Items != 1 || st.Files != 19 || st.FilesMapped != 19 || st.FilesMismatched != 0 {
		t.Fatalf("stats = %+v\n%s", st, rep)
	}
	a := e.items()[1]
	if a.Kind != KindArtist || a.Title != "Scott Joplin" || a.ExternalIDs.MBID == "" || a.MetadataProfileID == 0 ||
		len(a.Detail.Albums) != 4 || a.Detail.Albums[0].MBID == "" || !a.Detail.HasFile {
		t.Fatalf("artist = %+v", a)
	}
	for _, f := range e.files() {
		if f.Detail.Album == nil || f.Detail.Album.ID == 0 || f.Detail.Album.Title == "" || f.Detail.RelativePath == "" ||
			strings.HasPrefix(f.Detail.RelativePath, "/") {
			t.Fatalf("track file = %+v", f)
		}
	}
	m, err := e.runner.Store().Meta(context.Background(), nil, e.it.ID)
	if err != nil || len(m.MetadataProfiles) == 0 {
		t.Fatalf("meta = %+v, %v", m, err)
	}
}

func TestRefreshUnmappedNoSourceAndMismatched(t *testing.T) {
	e := newEnv(t, arr.KindRadarr, func(s *arrtest.Server) { s.UseMovies4K() })
	// One catalog file has another size than Radarr reports.
	e.writeFile("/movies/Charade (1963)/Charade (1963) [Bluray-1080p].mkv", 10)
	e.scan()
	st, rep := e.full()
	if st.Items != 5 || st.Files != 4 || st.FilesMapped != 3 || st.FilesUnmapped != 1 || st.FilesMismatched != 1 {
		t.Fatalf("stats = %+v\n%s", st, rep)
	}
	if !reflect.DeepEqual(st.UnmappedFolders, []UnmappedFolder{{RootFolder: "/movies-4k", Files: 1, Reason: ReasonUnmapped}}) ||
		!rep.warned("add a mapping for /movies-4k") {
		t.Fatalf("unmapped folders = %+v, warnings %v", st.UnmappedFolders, rep.warns)
	}
	page, err := e.runner.Store().Unmapped(context.Background(), e.it.ID, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if page.TotalRecords != 2 || page.Records[0].Reason != ReasonUnmapped || page.Records[0].LocalPath != nil ||
		!strings.HasPrefix(page.Records[0].Path, "/movies-4k/") || page.Records[1].Reason != ReasonMismatched ||
		page.Records[1].CatalogSize == nil || *page.Records[1].CatalogSize != 10 || page.Records[1].Size != 3412521 {
		t.Fatalf("unmapped = %+v", page)
	}
	// Map /movies-4k to a folder in no source: the file is no-source now.
	settings, _ := json.Marshal(map[string]any{"pathMappings": []map[string]string{
		{"arr": "/movies", "local": e.localOf("/movies")}, {"arr": "/movies-4k", "local": "/elsewhere/4k"}}})
	it, err := e.ints.Update(context.Background(), e.it.ID, integrations.Input{Name: e.it.Name, URL: e.it.URL, Settings: settings})
	if err != nil {
		t.Fatal(err)
	}
	e.it = it
	st, _ = e.full()
	if !reflect.DeepEqual(st.UnmappedFolders, []UnmappedFolder{{RootFolder: "/movies-4k", Files: 1, Reason: ReasonNoSource}}) {
		t.Fatalf("unmapped folders = %+v", st.UnmappedFolders)
	}
	page, err = e.runner.Store().Unmapped(context.Background(), 0, 1, 1)
	if err != nil || page.TotalRecords != 2 || len(page.Records) != 1 || page.Records[0].Reason != ReasonNoSource ||
		page.Records[0].LocalPath == nil || !strings.HasPrefix(*page.Records[0].LocalPath, "/elsewhere/4k/") {
		t.Fatalf("unmapped page 1 = %+v, %v", page, err)
	}
	m, _ := e.runner.Store().Meta(context.Background(), nil, e.it.ID)
	if len(m.RootFolders) != 2 || m.RootFolders[1].LocalPath == nil || m.RootFolders[1].SourceID != nil {
		t.Fatalf("root folders = %+v", m.RootFolders)
	}
}

func TestRefreshFileDateWarning(t *testing.T) {
	e := newEnv(t, arr.KindRadarr)
	e.srv.SetJSON(http.MethodGet, "config/mediamanagement", []byte(`{"recycleBin":"","fileDate":"cinemas"}`))
	st, rep := e.full()
	if st.FileDate != "cinemas" || !rep.warned("Change File Date: cinemas") {
		t.Fatalf("fileDate = %q, warnings %v", st.FileDate, rep.warns)
	}
}

func TestRefreshWrongAppFails(t *testing.T) {
	e := newEnv(t, arr.KindRadarr)
	e.full()
	before := e.state()
	e.srv.SetJSON(http.MethodGet, "system/status", []byte(`{"appName":"Sonarr","version":"4.0.0"}`))
	_, _, err := e.refresh(jobs.Params{}, false, jobs.TriggerManual)
	if err == nil || !strings.Contains(err.Error(), "not the expected application") {
		t.Fatalf("err = %v", err)
	}
	after := e.state()
	if after.Status != StatusFailed || !after.RefreshedAt.Equal(*before.RefreshedAt) || after.Error == "" || len(e.items()) != 4 {
		t.Fatalf("state after a wrong app = %+v", after)
	}
	// The failed attempt does not change freshness (D15).
	fr, _ := e.runner.Store().Freshness(context.Background(), nil, e.it)
	if !fr.Fresh {
		t.Fatalf("a failed refresh made the cache not fresh: %+v", fr)
	}
}

// bigRadarr serves n movies, each with one file, under /movies (for the guard thresholds).
func bigRadarr(e *testEnv, ids []int) {
	e.t.Helper()
	list := make([]map[string]any, 0, len(ids))
	for _, i := range ids {
		folder := "/movies/Movie " + itoa(i)
		file := folder + "/Movie " + itoa(i) + ".mkv"
		list = append(list, map[string]any{"id": i, "title": "Movie " + itoa(i), "year": 2000, "path": folder,
			"rootFolderPath": "/movies", "hasFile": true, "movieFileId": 100 + i, "monitored": true, "qualityProfileId": 6,
			"tags": []int{}, "movieFile": map[string]any{"id": 100 + i, "movieId": i, "path": file, "relativePath": "Movie " + itoa(i) + ".mkv",
				"size": 10, "quality": map[string]any{"quality": map[string]any{"name": "Bluray-1080p"}}}})
	}
	b, _ := json.Marshal(list)
	e.srv.SetJSON(http.MethodGet, "movie", b)
}

func itoa(i int) string { return strconv.Itoa(i) }

func seq(from, to int) []int {
	var out []int
	for i := from; i <= to; i++ {
		out = append(out, i)
	}
	return out
}

func TestRefreshGuard(t *testing.T) {
	t.Run("empty list", func(t *testing.T) {
		e := newEnv(t, arr.KindRadarr)
		e.full()
		first := *e.state().RefreshedAt
		e.srv.SetJSON(http.MethodGet, "movie", []byte(`[]`))
		e.clock.Advance(time.Hour)
		st, rep := e.full()
		if !st.GuardHeld || st.ItemsDeleted != 0 || st.FilesDeleted != 0 || !strings.Contains(st.GuardReason, "listed no movies") ||
			!rep.warned("Refresh guard") {
			t.Fatalf("stats = %+v, warnings %v", st, rep.warns)
		}
		s := e.state()
		if !s.RefreshedAt.Equal(first) || s.Status != StatusOK || !strings.Contains(s.Error, "Refresh guard") {
			t.Fatalf("a held refresh moved refreshed_at or lost the reason: %+v", s)
		}
		for _, it := range e.items() {
			if it.DeletedAt != nil {
				t.Fatalf("item %d marked deleted", it.ArrID)
			}
		}
		if len(e.files()) != 3 {
			t.Fatal("files were removed")
		}
		// allowChanges applies what the guard held.
		res, _, err := e.refresh(jobs.Params{AllowChanges: true}, false, jobs.TriggerManual)
		if err != nil {
			t.Fatal(err)
		}
		st = res.Stats.(Stats)
		if st.GuardHeld || st.ItemsDeleted != 4 || st.FilesDeleted != 3 || len(e.files()) != 0 || !e.state().RefreshedAt.After(first) {
			t.Fatalf("allowChanges = %+v", st)
		}
	})
	t.Run("more than half", func(t *testing.T) {
		e := newEnv(t, arr.KindRadarr)
		bigRadarr(e, seq(1, 50))
		e.full()
		bigRadarr(e, seq(1, 20)) // 30 of 50 vanish
		st, _ := e.full()
		if !st.GuardHeld || st.ItemsDeleted != 0 || st.FilesDeleted != 0 || !strings.Contains(st.GuardReason, "30 of 50 movies") {
			t.Fatalf("stats = %+v", st)
		}
		if len(e.files()) != 50 {
			t.Fatalf("files = %d, want 50 kept", len(e.files()))
		}
		// 20 vanishing (not more than 20) goes through.
		bigRadarr(e, seq(1, 30))
		st, _ = e.full()
		if st.GuardHeld || st.ItemsDeleted != 20 || st.FilesDeleted != 20 {
			t.Fatalf("stats = %+v", st)
		}
	})
	t.Run("files only", func(t *testing.T) {
		e := newEnv(t, arr.KindRadarr)
		bigRadarr(e, seq(1, 50))
		e.full()
		// Every movie is still listed, but 30 lost their file.
		list := make([]map[string]any, 0, 50)
		for i := 1; i <= 50; i++ {
			m := map[string]any{"id": i, "title": "Movie " + itoa(i), "path": "/movies/Movie " + itoa(i), "rootFolderPath": "/movies",
				"hasFile": false, "movieFileId": 0, "monitored": true, "qualityProfileId": 6}
			if i <= 20 {
				m["hasFile"], m["movieFileId"] = true, 100+i
				m["movieFile"] = map[string]any{"id": 100 + i, "path": "/movies/Movie " + itoa(i) + "/Movie " + itoa(i) + ".mkv", "size": 10}
			}
			list = append(list, m)
		}
		b, _ := json.Marshal(list)
		e.srv.SetJSON(http.MethodGet, "movie", b)
		st, _ := e.full()
		if !st.GuardHeld || st.FilesDeleted != 0 || !strings.Contains(st.GuardReason, "30 of 50 files") {
			t.Fatalf("stats = %+v", st)
		}
	})
	t.Run("inaccessible root folder", func(t *testing.T) {
		e := newEnv(t, arr.KindRadarr)
		e.full()
		e.srv.SetRootFolderAccessible("/movies", false)
		e.editList("movie", "movie.json", func(list []map[string]any) []map[string]any {
			for _, m := range list {
				m["hasFile"], m["movieFileId"] = false, 0
				delete(m, "movieFile")
			}
			return list
		})
		st, rep := e.full()
		if st.FilesDeleted != 0 || len(e.files()) != 3 || !slices.Equal(st.InaccessibleRootFolders, []string{"/movies"}) ||
			!rep.warned("not accessible") {
			t.Fatalf("stats = %+v, files %d, warnings %v", st, len(e.files()), rep.warns)
		}
		m, _ := e.runner.Store().Meta(context.Background(), nil, e.it.ID)
		if m.RootFolders[0].Accessible {
			t.Fatal("root folder stored as accessible")
		}
	})
}

func TestReconcileQueuesTargetedSyncs(t *testing.T) {
	e := newEnv(t, arr.KindRadarr)
	e.full()
	// Radarr changed without a webhook: movie 2 got a new file (an upgrade: new id, new name),
	// movie 3 moved to another folder, movie 1 was deleted with its file, movie 6 was added with
	// a file, movie 4 (no file) got a new title only.
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
	res, rep, err := e.refresh(jobs.Params{}, false, jobs.TriggerSchedule)
	if err != nil {
		t.Fatal(err)
	}
	st := res.Stats.(Stats)
	if st.ChangedItems != 4 || st.ItemsAdded != 1 || st.ItemsUpdated != 2 || st.ItemsDeleted != 1 || st.FilesDeleted != 2 {
		t.Fatalf("stats = %+v\n%s", st, rep)
	}
	want := []string{"1/[" + itoa(int(e.src.ID)) + "]:Charade,Charade (1963),Heat (1995),His Girl Friday (1940),Night of the Living Dead (1968)"}
	if got := syncTargets(e.enq.queued()); !reflect.DeepEqual(got, want) {
		t.Fatalf("syncs = %v, want %v", got, want)
	}
	if q := e.enq.queued(); q[0].Trigger != jobs.TriggerSchedule || !reflect.DeepEqual(st.FollowUpJobs, []int64{q[0].ID}) {
		t.Fatalf("sync = %+v, followUpJobs %v", q[0], st.FollowUpJobs)
	}
	if it := e.items()[1]; it.DeletedAt == nil {
		t.Fatal("movie 1 not marked deleted")
	}
}

func TestReconcileDestinations(t *testing.T) {
	cases := []struct {
		name  string
		dests []FollowUpDestination
		want  int
	}{
		{"linked", []FollowUpDestination{{ID: 1, Enabled: true, SyncOnArrChange: true}}, 1},
		{"two linked", []FollowUpDestination{{ID: 1, Enabled: true, SyncOnArrChange: true}, {ID: 2, Enabled: true, SyncOnArrChange: true}}, 2},
		{"disabled", []FollowUpDestination{{ID: 1, Enabled: false, SyncOnArrChange: true}}, 0},
		{"syncOnArrChange off", []FollowUpDestination{{ID: 1, Enabled: true, SyncOnArrChange: false}}, 0},
		{"not linked", []FollowUpDestination{{ID: 1, Enabled: true, SyncOnArrChange: true, SourceIDs: []int64{999}}}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, arr.KindRadarr)
			for i := range c.dests {
				if c.dests[i].SourceIDs == nil {
					c.dests[i].SourceIDs = []int64{e.src.ID}
				}
			}
			e.dests = c.dests
			e.full()
			e.editList("movie", "movie.json", func(list []map[string]any) []map[string]any {
				movieByID(list, 2)["movieFile"].(map[string]any)["id"] = 77
				return list
			})
			e.full()
			if got := len(e.enq.queued()); got != c.want {
				t.Fatalf("queued %d syncs, want %d: %v", got, c.want, e.enq.queued())
			}
		})
	}
}

func TestTargetedRefresh(t *testing.T) {
	t.Run("webhook follow-up with old and new folders", func(t *testing.T) {
		e := newEnv(t, arr.KindRadarr)
		e.full()
		refreshed := *e.state().RefreshedAt
		e.writeFile("/movies/Charade/Charade.mkv", 3412521)
		e.editList("movie/3", "movie.json", func(list []map[string]any) []map[string]any {
			m := movieByID(list, 3)
			m["path"] = "/movies/Charade"
			mf := m["movieFile"].(map[string]any)
			mf["id"], mf["path"] = 30, "/movies/Charade/Charade.mkv"
			m["movieFileId"] = 30
			return []map[string]any{m}
		})
		// movie/3 serves one object, not a list.
		var list []map[string]any
		_ = json.Unmarshal(mustJSON(t, e, "movie/3"), &list)
		b, _ := json.Marshal(list[0])
		e.srv.SetJSON(http.MethodGet, "movie/3", b)
		e.clock.Advance(time.Minute)
		res, rep, err := e.refresh(jobs.Params{ArrItemIDs: []int64{3}, SyncAfter: true}, false, jobs.TriggerWebhook)
		if err != nil {
			t.Fatalf("%v\n%s", err, rep)
		}
		st := res.Stats.(Stats)
		if !st.Targeted || st.Items != 1 || st.ItemsUpdated != 1 || st.FilesDeleted != 1 || st.Files != 1 {
			t.Fatalf("stats = %+v", st)
		}
		want := []string{"1/[" + itoa(int(e.src.ID)) + "]:Charade,Charade (1963)"}
		if got := syncTargets(e.enq.queued()); !reflect.DeepEqual(got, want) || e.enq.queued()[0].Trigger != jobs.TriggerWebhook {
			t.Fatalf("syncs = %v, want %v (%+v)", got, want, e.enq.queued())
		}
		s := e.state()
		if !s.RefreshedAt.Equal(refreshed) || !s.AttemptedAt.Equal(e.clock.Now()) || s.Status != StatusOK {
			t.Fatalf("a targeted refresh changed refreshed_at: %+v", s)
		}
		if f := e.files(); len(f) != 3 || f[30].ItemID == 0 || f[3].ID != 0 {
			t.Fatalf("files = %v", f)
		}
	})
	t.Run("404 with the expected app marks deleted", func(t *testing.T) {
		e := newEnv(t, arr.KindRadarr)
		e.full()
		e.srv.SetStatus(http.MethodGet, "movie/2", http.StatusNotFound)
		res, rep, err := e.refresh(jobs.Params{ArrItemIDs: []int64{2}, SyncAfter: true}, false, jobs.TriggerWebhook)
		if err != nil {
			t.Fatalf("%v\n%s", err, rep)
		}
		st := res.Stats.(Stats)
		if st.ItemsDeleted != 1 || st.FilesDeleted != 1 || e.items()[2].DeletedAt == nil {
			t.Fatalf("stats = %+v", st)
		}
		// The deleted item's old folder is synced (the targeted sync decides what is gone).
		want := []string{"1/[" + itoa(int(e.src.ID)) + "]:His Girl Friday (1940)"}
		if got := syncTargets(e.enq.queued()); !reflect.DeepEqual(got, want) {
			t.Fatalf("syncs = %v", got)
		}
	})
	t.Run("404 with a wrong app fails and deletes nothing", func(t *testing.T) {
		e := newEnv(t, arr.KindRadarr)
		e.full()
		e.srv.SetStatus(http.MethodGet, "movie/2", http.StatusNotFound)
		e.srv.SetJSON(http.MethodGet, "system/status", []byte(`{"appName":"Lidarr","version":"3.1.0"}`))
		_, _, err := e.refresh(jobs.Params{ArrItemIDs: []int64{2}}, false, jobs.TriggerWebhook)
		if err == nil || !strings.Contains(err.Error(), "nothing was marked deleted") {
			t.Fatalf("err = %v", err)
		}
		if e.items()[2].DeletedAt != nil || len(e.files()) != 3 || e.state().Status != StatusFailed {
			t.Fatal("a 404 behind a wrong app deleted the item")
		}
	})
	t.Run("missing located folder queues an untargeted sync", func(t *testing.T) {
		e := newEnv(t, arr.KindRadarr)
		e.full()
		e.editList("movie", "movie.json", nil2(func(list []map[string]any) {}))
		m := fixtureMovie(t, e, 2)
		m["path"] = "/movies/Nowhere (1940)"
		b, _ := json.Marshal(m)
		e.srv.SetJSON(http.MethodGet, "movie/2", b)
		res, rep, err := e.refresh(jobs.Params{ArrItemIDs: []int64{2}, SyncAfter: true}, false, jobs.TriggerWebhook)
		if err != nil {
			t.Fatal(err)
		}
		if !rep.warned("which does not exist: check the path mappings") || res.Warnings == 0 {
			t.Fatalf("warnings = %v", rep.warns)
		}
		want := []string{"1/[" + itoa(int(e.src.ID)) + "]:*"}
		if got := syncTargets(e.enq.queued()); !reflect.DeepEqual(got, want) {
			t.Fatalf("syncs = %v", got)
		}
	})
	t.Run("follow-up after a failure uses the index's folders", func(t *testing.T) {
		e := newEnv(t, arr.KindRadarr)
		e.full()
		e.srv.SetStatus(http.MethodGet, "movie/2", http.StatusInternalServerError)
		_, _, err := e.refresh(jobs.Params{ArrItemIDs: []int64{2}, SyncAfter: true}, false, jobs.TriggerWebhook)
		if err == nil {
			t.Fatal("refresh did not fail")
		}
		want := []string{"1/[" + itoa(int(e.src.ID)) + "]:His Girl Friday (1940)"}
		if got := syncTargets(e.enq.queued()); !reflect.DeepEqual(got, want) {
			t.Fatalf("syncs = %v", got)
		}
		// A failed targeted refresh leaves the facts known (D15).
		if fr, _ := e.runner.Store().Freshness(context.Background(), nil, e.it); !fr.Fresh || e.state().Status != StatusFailed {
			t.Fatalf("after a failed targeted refresh: %+v %+v", fr, e.state())
		}
		// Not after a cancel.
		e.enq.reset()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		e.jobID++
		_, err = e.runner.Run(ctx, jobs.Job{ID: e.jobID, Type: jobs.TypeRefresh, Params: jobs.Params{IntegrationID: e.it.ID,
			ArrItemIDs: []int64{2}, SyncAfter: true}}, jobs.Env{Reporter: &memReporter{}, Items: &memItems{}})
		if err == nil || len(e.enq.queued()) != 0 {
			t.Fatalf("cancelled refresh: err %v, queued %v", err, e.enq.queued())
		}
	})
	t.Run("unknown tag reads the metadata again", func(t *testing.T) {
		e := newEnv(t, arr.KindRadarr)
		e.full()
		e.srv.SetJSON(http.MethodGet, "tag", []byte(`[{"id":1,"label":"bunkarr-full"},{"id":2,"label":"irreplaceable"},{"id":3,"label":"new-tag"}]`))
		m := fixtureMovie(t, e, 2)
		m["tags"] = []int{3}
		b, _ := json.Marshal(m)
		e.srv.SetJSON(http.MethodGet, "movie/2", b)
		if _, _, err := e.refresh(jobs.Params{ArrItemIDs: []int64{2}}, false, jobs.TriggerManual); err != nil {
			t.Fatal(err)
		}
		meta, _ := e.runner.Store().Meta(context.Background(), nil, e.it.ID)
		if len(meta.Tags) != 3 || meta.Tags[2].Label != "new-tag" {
			t.Fatalf("tags = %+v", meta.Tags)
		}
		if len(e.enq.queued()) != 0 {
			t.Fatal("a refresh without syncAfter queued a sync")
		}
	})
	t.Run("arrItemIds on another type", func(t *testing.T) {
		e := newEnv(t, arr.KindRadarr)
		plex, err := e.ints.Create(context.Background(), integrations.Input{Type: integrations.TypePlex, Name: "Plex", URL: "http://127.0.0.1:1"})
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = e.refresh(jobs.Params{IntegrationID: plex.ID, ArrItemIDs: []int64{1}}, false, jobs.TriggerManual)
		if err == nil || !strings.Contains(err.Error(), "arrItemIds are only for") {
			t.Fatalf("err = %v", err)
		}
		_, _, err = e.refresh(jobs.Params{IntegrationID: plex.ID}, false, jobs.TriggerManual)
		if err == nil || !strings.Contains(err.Error(), "not available yet") {
			t.Fatalf("err = %v", err)
		}
	})
}

func nil2(func([]map[string]any)) func([]map[string]any) []map[string]any {
	return func(l []map[string]any) []map[string]any { return l }
}

// fixtureMovie returns a movie of Radarr's movie.json as a JSON object.
func fixtureMovie(t *testing.T, e *testEnv, id float64) map[string]any {
	t.Helper()
	var list []map[string]any
	if err := json.Unmarshal(arrtest.Fixture(t, arr.KindRadarr, "movie.json"), &list); err != nil {
		t.Fatal(err)
	}
	return movieByID(list, id)
}

// mustJSON fetches what the fake serves for an API target.
func mustJSON(t *testing.T, e *testEnv, target string) []byte {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/api/v3/"+target, nil)
	req.Header.Set("X-Api-Key", testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b []byte
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		b = append(b, buf[:n]...)
		if err != nil {
			break
		}
	}
	return b
}

func TestInstanceChange(t *testing.T) {
	e := newEnv(t, arr.KindRadarr)
	e.full()
	other := arrtest.NewServer(t, arr.KindRadarr, testKey)
	other.SetJSON(http.MethodGet, "movie", []byte(`[]`))
	it, err := e.ints.Update(context.Background(), e.it.ID, integrations.Input{Name: e.it.Name, URL: other.URL, APIKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	e.it = it
	fr, _ := e.runner.Store().Freshness(context.Background(), nil, e.it)
	if fr.Fresh || fr.InstanceMatches || !strings.Contains(fr.Reason, "URL changed") {
		t.Fatalf("after a URL change: %+v", fr)
	}
	// A dry run replaces nothing and reports nothing deleted.
	res, _, err := e.refresh(jobs.Params{}, true, jobs.TriggerManual)
	if err != nil || res.Stats.(Stats).ItemsDeleted != 0 || len(e.items()) != 4 {
		t.Fatalf("dry run = %+v, %v", res.Stats, err)
	}
	// The next refresh replaces the old instance's cache, even with an empty answer (no guard:
	// nothing of this instance is known).
	st, _ := e.full()
	if st.GuardHeld || len(e.items()) != 0 || len(e.files()) != 0 {
		t.Fatalf("stats = %+v, items %d", st, len(e.items()))
	}
	s := e.state()
	if s.InstanceID != other.URL || s.RefreshedAt == nil {
		t.Fatalf("state = %+v", s)
	}
}

func TestInstanceChangeFailedFirstWriteIsNotFresh(t *testing.T) {
	e := newEnv(t, arr.KindRadarr)
	e.full()
	other := arrtest.NewServer(t, arr.KindRadarr, testKey)
	other.SetStatus(http.MethodGet, "movie", http.StatusInternalServerError)
	it, err := e.ints.Update(context.Background(), e.it.ID, integrations.Input{Name: e.it.Name, URL: other.URL, APIKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	e.it = it
	if _, _, err := e.refresh(jobs.Params{}, false, jobs.TriggerManual); err == nil {
		t.Fatal("refresh did not fail")
	}
	s := e.state()
	fr, _ := e.runner.Store().Freshness(context.Background(), nil, e.it)
	if s.RefreshedAt != nil || fr.Fresh || s.InstanceID != other.URL || len(e.items()) != 0 {
		t.Fatalf("a partial new instance looks fresh: %+v %+v", s, fr)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	e := newEnv(t, arr.KindRadarr)
	res, _, err := e.refresh(jobs.Params{}, true, jobs.TriggerManual)
	if err != nil {
		t.Fatal(err)
	}
	st := res.Stats.(Stats)
	if !st.DryRun || st.Items != 4 || st.ItemsAdded != 4 || st.Files != 3 {
		t.Fatalf("stats = %+v", st)
	}
	if s := e.state(); s.Status != StatusNever || s.AttemptedAt != nil || len(e.items()) != 0 {
		t.Fatalf("a dry run wrote: %+v", s)
	}
	e.full()
	e.srv.SetJSON(http.MethodGet, "movie", []byte(`[]`))
	res, _, err = e.refresh(jobs.Params{AllowChanges: true}, true, jobs.TriggerManual)
	if err != nil {
		t.Fatal(err)
	}
	if st := res.Stats.(Stats); st.ItemsDeleted != 4 || st.FilesDeleted != 3 || len(e.enq.queued()) != 0 {
		t.Fatalf("dry run = %+v, queued %v", st, e.enq.queued())
	}
	for _, it := range e.items() {
		if it.DeletedAt != nil {
			t.Fatal("a dry run marked an item deleted")
		}
	}
	// A targeted dry run neither writes nor queues.
	e.srv.SetStatus(http.MethodGet, "movie/2", http.StatusNotFound)
	res, _, err = e.refresh(jobs.Params{ArrItemIDs: []int64{2}, SyncAfter: true}, true, jobs.TriggerWebhook)
	if err != nil || res.Stats.(Stats).ItemsDeleted != 1 || e.items()[2].DeletedAt != nil || len(e.enq.queued()) != 0 {
		t.Fatalf("targeted dry run = %+v, %v", res.Stats, err)
	}
}

func TestPurgeDeletedItems(t *testing.T) {
	e := newEnv(t, arr.KindRadarr)
	e.full()
	e.editList("movie", "movie.json", func(l []map[string]any) []map[string]any { return l[1:] })
	e.full()
	if e.items()[1].DeletedAt == nil {
		t.Fatal("movie 1 not marked deleted")
	}
	e.clock.Advance(PurgeAfter - time.Hour)
	e.full()
	if _, ok := e.items()[1]; !ok {
		t.Fatal("purged too early")
	}
	e.clock.Advance(2 * time.Hour)
	e.full()
	if _, ok := e.items()[1]; ok {
		t.Fatal("not purged after 30 days")
	}
	// A deleted item that comes back is live again.
	e.srv.SetFixture(http.MethodGet, "movie", "movie.json")
	st, _ := e.full()
	if st.ItemsAdded != 1 || e.items()[1].DeletedAt != nil {
		t.Fatalf("stats = %+v", st)
	}
}

func TestRefreshDisabledAndMissingIntegration(t *testing.T) {
	e := newEnv(t, arr.KindRadarr)
	off := false
	if _, err := e.ints.Update(context.Background(), e.it.ID, integrations.Input{Name: e.it.Name, URL: e.it.URL, Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.refresh(jobs.Params{}, false, jobs.TriggerManual); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("err = %v", err)
	}
	if _, _, err := e.refresh(jobs.Params{IntegrationID: 999}, false, jobs.TriggerManual); err == nil || !strings.Contains(err.Error(), "no longer exists") {
		t.Fatalf("err = %v", err)
	}
}

func TestRefreshRequestsStayInTheAllowList(t *testing.T) {
	for _, kind := range []arr.Kind{arr.KindRadarr, arr.KindSonarr, arr.KindLidarr} {
		t.Run(string(kind), func(t *testing.T) {
			e := newEnv(t, kind)
			e.full()
			if _, _, err := e.refresh(jobs.Params{ArrItemIDs: []int64{1}, SyncAfter: true}, false, jobs.TriggerWebhook); err != nil {
				t.Fatal(err)
			}
			routes := map[string]bool{}
			for _, r := range e.srv.Routes() {
				routes[r] = true
			}
			for _, r := range e.srv.Requests() {
				k := r.Method + " " + r.Path
				if r.RawQuery != "" {
					k += "?" + r.RawQuery
				}
				if !routes[k] {
					t.Errorf("request outside the route table: %s", k)
				}
				if r.Method != http.MethodGet {
					t.Errorf("the refresh sent %s %s", r.Method, r.Path)
				}
			}
		})
	}
}

func TestFreshnessOf(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	it := integrations.Integration{Type: integrations.TypeRadarr, URL: "http://radarr:7878"}
	at := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }
	cases := []struct {
		name   string
		st     State
		fresh  bool
		reason string
	}{
		{"never", State{InstanceID: it.URL}, false, "not been refreshed"},
		{"fresh", State{InstanceID: it.URL, RefreshedAt: at(23 * time.Hour)}, true, ""},
		{"stale", State{InstanceID: it.URL, RefreshedAt: at(31 * time.Hour)}, false, "Radarr cache is 31 h old"},
		{"exactly stale", State{InstanceID: it.URL, RefreshedAt: at(24 * time.Hour)}, false, "24 h old"},
		{"other instance", State{InstanceID: "http://old:7878", RefreshedAt: at(time.Hour)}, false, "URL changed"},
		{"failed attempt keeps fresh", State{InstanceID: it.URL, RefreshedAt: at(time.Hour), Status: StatusFailed, Error: "x"}, true, ""},
		{"days", State{InstanceID: it.URL, RefreshedAt: at(100 * time.Hour)}, false, "4 d old"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := FreshnessOf(c.st, it, 24*time.Hour, now)
			if f.Fresh != c.fresh || !strings.Contains(f.Reason, c.reason) || f.StaleAfterHours != 24 {
				t.Fatalf("FreshnessOf = %+v", f)
			}
		})
	}
}

func TestStaleAfter(t *testing.T) {
	arrIt := integrations.Integration{Type: integrations.TypeSonarr, Settings: json.RawMessage(`{"refresh":{"staleAfterHours":5}}`)}
	if d, ok := StaleAfter(arrIt); !ok || d != 5*time.Hour {
		t.Fatalf("StaleAfter(sonarr) = %v, %v", d, ok)
	}
	if d, ok := StaleAfter(integrations.Integration{Type: integrations.TypeRadarr}); !ok || d != 24*time.Hour {
		t.Fatalf("StaleAfter(default) = %v, %v", d, ok)
	}
	if _, ok := StaleAfter(integrations.Integration{Type: integrations.TypePlex}); ok {
		t.Fatal("Plex has a staleAfterHours")
	}
	if d, ok := StaleAfter(integrations.Integration{Type: integrations.TypeTautulli, Settings: json.RawMessage(`{"plexIntegrationId":1}`)}); !ok || d != 72*time.Hour {
		t.Fatalf("StaleAfter(tautulli) = %v, %v", d, ok)
	}
}
