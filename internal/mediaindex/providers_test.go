package mediaindex

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/integrations/maintainerr/maintainerrtest"
	"github.com/sl0wz3r/bunkarr/internal/integrations/plex/plextest"
	"github.com/sl0wz3r/bunkarr/internal/integrations/seerr/seerrtest"
	"github.com/sl0wz3r/bunkarr/internal/integrations/tautulli/tautullitest"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

func TestRefreshes(t *testing.T) {
	plexOn := integrations.Integration{Type: integrations.TypePlex, Settings: json.RawMessage(`{"index":{"enabled":true}}`)}
	plexOff := integrations.Integration{Type: integrations.TypePlex, Settings: json.RawMessage(`{}`)}
	for _, tt := range []struct {
		it   integrations.Integration
		want bool
	}{
		{integrations.Integration{Type: integrations.TypeRadarr}, true},
		{integrations.Integration{Type: integrations.TypeTautulli}, true},
		{integrations.Integration{Type: integrations.TypeSeerr}, true},
		{integrations.Integration{Type: integrations.TypeMaintainerr}, true},
		{plexOn, true},
		{plexOff, false},
	} {
		if got := Refreshes(tt.it); got != tt.want {
			t.Errorf("Refreshes(%s %s) = %v", tt.it.Type, tt.it.Settings, got)
		}
	}
	// Start-up refreshes stay *arr-only (design §6.1: an upgraded install queues nothing new).
	if Supports(integrations.TypeTautulli) || Supports(integrations.TypePlex) {
		t.Fatal("Supports widened")
	}
}

func TestPlexIndexRefresh(t *testing.T) {
	e := newProvEnv(t)
	stats, _ := e.mustRun(e.plexIt)
	cache := stats.Cache.(PlexIndexStats)
	if cache.Sections != 3 || cache.Items != 32 || cache.Files != 22 || cache.FilesMapped != 22 || cache.FilesUnmapped != 0 ||
		cache.MachineIdentifier != plextest.LibraryMachineIdentifier {
		t.Fatalf("stats = %+v", cache)
	}
	want := map[string]int64{"movie": 6, "show": 2, "season": 4, "episode": 10, "artist": 2, "album": 2, "track": 6}
	for k, v := range want {
		if cache.ItemsByType[k] != v {
			t.Errorf("%s: %d", k, cache.ItemsByType[k])
		}
	}
	st := e.state(e.plexIt.ID)
	if st.Status != StatusOK || st.RefreshedAt == nil || st.InstanceID != e.plexIt.URL+"#"+plextest.LibraryMachineIdentifier {
		t.Fatalf("state = %+v", st)
	}
	if f := e.fresh(e.plexIt); !f.Fresh || !f.InstanceMatches || f.StaleAfterHours != 72 {
		t.Fatalf("freshness = %+v", f)
	}
	secs, err := e.runner.Store().PlexSections(context.Background(), nil, e.plexIt.ID)
	if err != nil || len(secs) != 3 {
		t.Fatalf("sections %v, %v", secs, err)
	}
	tv := secs[1]
	if tv.Key != "2" || tv.Type != "show" || len(tv.Locations) != 1 || tv.Locations[0].Path != "/data/tv" ||
		tv.Locations[0].SourceID == nil || *tv.Locations[0].SourceID != e.sources["tv"].ID || *tv.Locations[0].RelPath != "" {
		t.Fatalf("tv section = %+v", tv)
	}
	items := map[string]PlexItem{}
	if err := e.runner.Store().EachPlexItem(context.Background(), nil, e.plexIt.ID, func(it PlexItem) error {
		items[it.RatingKey] = it
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ep := items["31"] // The Twilight Zone S01E02
	if ep.Type != "episode" || ep.SectionKey != "2" || ep.ParentKey != "29" || ep.GrandparentKey != "28" || *ep.Index != 2 || *ep.ParentIndex != 1 ||
		ep.ExternalIDs["tvdb"] == "" || ep.AddedAt == nil {
		t.Fatalf("episode 31 = %+v", ep)
	}
	if s := items["33"]; s.Type != "season" || *s.Index != 2 || s.ParentKey != "28" {
		t.Fatalf("season 33 = %+v", s)
	}
	if m := items["3"]; m.Type != "movie" || m.ExternalIDs["tmdb"] != "10331" || m.Index != nil {
		t.Fatalf("movie 3 = %+v", m)
	}
	var files []PlexFile
	if err := e.runner.Store().PlexFilesUnder(context.Background(), nil, e.sources["tv"].Path, func(f PlexFile) error {
		files = append(files, f)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(files) != 10 {
		t.Fatalf("%d tv files", len(files))
	}
	for _, f := range files {
		if f.Location == nil || f.Location.SourceID != e.sources["tv"].ID || !strings.HasPrefix(f.LocalPath, e.sources["tv"].Path+"/") {
			t.Fatalf("file %+v", f)
		}
	}
	for _, r := range e.plexSrv.Requests() {
		if strings.Contains(r.RawQuery, plexToken) || (r.Path != "/identity" && r.Header.Get("X-Plex-Token") != plexToken) {
			t.Fatalf("token handling of %s", r.Path)
		}
	}
}

func TestPlexIndexDryRunWritesNothing(t *testing.T) {
	e := newProvEnv(t)
	res, _, err := e.run(e.plexIt, jobs.Params{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stats.(ProviderStats).Items != 32 || e.count("plex_items", e.plexIt.ID) != 0 || e.state(e.plexIt.ID).Status != StatusNever {
		t.Fatal("the dry run wrote")
	}
}

func TestPlexIndexUnmappedFiles(t *testing.T) {
	e := newProvEnv(t)
	if _, err := e.ints.Update(context.Background(), e.plexIt.ID, integrations.Input{Name: "Plex", URL: e.plexIt.URL,
		Settings: json.RawMessage(`{"pathMappings":[{"plex":"/data/movies","local":"` + e.sources["movies"].Path + `"}],"index":{"enabled":true}}`)}); err != nil {
		t.Fatal(err)
	}
	it, _ := e.ints.Get(context.Background(), e.plexIt.ID)
	stats, rep := e.mustRun(it)
	if stats.FilesMapped != 6 || stats.FilesUnmapped != 16 || !strings.Contains(rep.String(), "map to no source") {
		t.Fatalf("stats %+v", stats)
	}
	var unmapped int
	_ = e.runner.Store().EachPlexFile(context.Background(), nil, it.ID, func(f PlexFile) error {
		if f.LocalPath == "" && f.Location == nil {
			unmapped++
		}
		return nil
	})
	if unmapped != 16 {
		t.Fatalf("%d unmapped rows", unmapped)
	}
}

// TestPlexIndexFailureKeepsTheIndex: a failed listing records the attempt; the rows and
// refreshed_at stay, and the index stays fresh (D15).
func TestPlexIndexFailureKeepsTheIndex(t *testing.T) {
	e := newProvEnv(t)
	e.mustRun(e.plexIt)
	before := e.state(e.plexIt.ID)
	e.clock.Advance(time.Hour)
	e.plexSrv.Handle("/library/sections/2/all", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })
	if _, _, err := e.run(e.plexIt, jobs.Params{}, false); err == nil {
		t.Fatal("the refresh did not fail")
	}
	after := e.state(e.plexIt.ID)
	if after.Status != StatusFailed || !after.RefreshedAt.Equal(*before.RefreshedAt) || e.count("plex_items", e.plexIt.ID) != 32 {
		t.Fatalf("state after failure = %+v", after)
	}
	if !e.fresh(e.plexIt).Fresh {
		t.Fatal("a failed attempt made the index stale")
	}
}

// TestPlexIndexInstanceChange: another machine at the same URL replaces the index; a new URL makes
// it not fresh until the next refresh.
func TestPlexIndexInstanceChange(t *testing.T) {
	e := newProvEnv(t)
	e.mustRun(e.plexIt)
	e.plexSrv.SetIdentity("another-machine", "1.43.4")
	e.lib.SetListing("1", 1, e.lib.Rows("1", 1)[:2])
	_, rep := e.mustRun(e.plexIt)
	if st := e.state(e.plexIt.ID); st.InstanceID != e.plexIt.URL+"#another-machine" || !strings.Contains(rep.String(), "replaced") {
		t.Fatalf("state = %+v", st)
	}
	if n := e.count("plex_items", e.plexIt.ID); n != 28 {
		t.Fatalf("%d items after the replacement", n)
	}
	moved := plextest.NewServer(t, plexToken)
	if _, err := e.ints.Update(context.Background(), e.plexIt.ID, integrations.Input{Name: "Plex", URL: moved.URL, APIKey: plexToken}); err != nil {
		t.Fatal(err)
	}
	if f := e.fresh(e.plexIt); f.Fresh || f.InstanceMatches {
		t.Fatalf("freshness after a URL change = %+v", f)
	}
}

func TestPlexIndexTurnedOff(t *testing.T) {
	e := newProvEnv(t)
	e.mustRun(e.plexIt)
	if _, err := e.ints.Update(context.Background(), e.plexIt.ID, integrations.Input{Name: "Plex", URL: e.plexIt.URL,
		Settings: json.RawMessage(`{"index":{"enabled":false}}`)}); err != nil {
		t.Fatal(err)
	}
	if f := e.fresh(e.plexIt); f.Fresh || !strings.Contains(f.Reason, "turned off") {
		t.Fatalf("freshness = %+v", f)
	}
	it, _ := e.ints.Get(context.Background(), e.plexIt.ID)
	if Refreshes(it) {
		t.Fatal("a turned-off index refreshes")
	}
}

func watchRows(t *testing.T, e *provEnv, id int64) map[string]WatchStat {
	t.Helper()
	out := map[string]WatchStat{}
	if err := e.runner.Store().EachWatchStat(context.Background(), nil, id, func(w WatchStat) error {
		out[w.KeyType+":"+w.Key] = w
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTautulliRefresh(t *testing.T) {
	e := newProvEnv(t)
	taut := e.addTautulli()
	// Without the Plex index there are no sections to read.
	if _, _, err := e.run(taut, jobs.Params{}, false); err == nil || !strings.Contains(err.Error(), "library index") {
		t.Fatalf("refresh without the Plex index: %v", err)
	}
	e.mustRun(e.plexIt)
	stats, _ := e.mustRun(taut)
	cache := stats.Cache.(TautulliStats)
	if cache.PlexIntegrationID != e.plexIt.ID || !slices.Equal(cache.Sections, []string{"1", "2", "3"}) || len(cache.SectionsWithoutHistory) != 0 ||
		cache.Users != 1 || cache.UsersWithoutHistory != 0 || cache.Plays != 13 || stats.AppVersion != "v2.18.1" {
		t.Fatalf("stats = %+v", cache)
	}
	rows := watchRows(t, e, taut.ID)
	nosferatu := rows["rating_key:4"]
	if nosferatu.Plays != 3 || nosferatu.LastWatched == nil {
		t.Fatalf("Nosferatu = %+v", nosferatu)
	}
	if g := rows["guid:plex://movie/5d7768278718ba001e311d5d"]; g.Plays != 3 || !g.LastWatched.Equal(*nosferatu.LastWatched) {
		t.Fatalf("Nosferatu by guid = %+v", g)
	}
	for key, plays := range map[string]int64{"rating_key:9": 1, "rating_key:2": 1, "rating_key:30": 2, "rating_key:31": 1, "rating_key:26": 1, "rating_key:45": 2, "rating_key:40": 1, "rating_key:42": 1} {
		if rows[key].Plays != plays {
			t.Errorf("%s: %d plays, want %d", key, rows[key].Plays, plays)
		}
	}
	if _, ok := rows["rating_key:3"]; ok {
		t.Error("an unwatched movie has a row")
	}
	if f := e.fresh(taut); !f.Fresh {
		t.Fatalf("freshness = %+v", f)
	}
	link, _ := e.runner.Store().StoredPlexLink(context.Background(), nil, taut.ID)
	if link != e.plexIt.ID {
		t.Fatalf("stored link %d", link)
	}
}

func TestTautulliHistoryOff(t *testing.T) {
	e := newProvEnv(t)
	taut := e.addTautulli()
	e.mustRun(e.plexIt)
	e.taut.HistoryOff()
	stats, rep := e.mustRun(taut)
	cache := stats.Cache.(TautulliStats)
	if !slices.Equal(cache.SectionsWithoutHistory, []string{"2"}) || cache.UsersWithoutHistory != 1 || !strings.Contains(rep.String(), "lower bounds") {
		t.Fatalf("stats = %+v", cache)
	}
	var stored TautulliStats
	if err := json.Unmarshal(e.state(taut.ID).Stats, &stored); err != nil || stored.UsersWithoutHistory != 1 {
		t.Fatalf("stored stats %s", e.state(taut.ID).Stats)
	}
	if strings.Contains(string(e.state(taut.ID).Stats), "Local") {
		t.Fatal("a user name was stored")
	}
}

// TestTautulliEmptyHistoryIsHeld: an empty history while the cache has rows keeps the old rows and
// refreshed_at (shrink guard, S10); allowChanges applies it.
func TestTautulliEmptyHistoryIsHeld(t *testing.T) {
	e := newProvEnv(t)
	taut := e.addTautulli()
	e.mustRun(e.plexIt)
	e.mustRun(taut)
	before := e.state(taut.ID)
	rowsBefore := e.count("watch_stats", taut.ID)
	for _, sec := range []string{"1", "2", "3"} {
		e.taut.SetHistory(sec, 0, 1000, 0, nil)
	}
	e.clock.Advance(time.Hour)
	res, rep, err := e.run(taut, jobs.Params{}, false)
	if err != nil {
		t.Fatal(err)
	}
	stats := res.Stats.(ProviderStats)
	if !stats.GuardHeld || res.Warnings == 0 || !rep.warned("Refresh guard") {
		t.Fatalf("stats %+v, warnings %d", stats, res.Warnings)
	}
	after := e.state(taut.ID)
	if e.count("watch_stats", taut.ID) != rowsBefore || !after.RefreshedAt.Equal(*before.RefreshedAt) || after.Status != StatusOK ||
		!strings.Contains(after.Error, "Refresh guard") {
		t.Fatalf("state %+v", after)
	}
	if _, _, err := e.run(taut, jobs.Params{AllowChanges: true}, false); err != nil {
		t.Fatal(err)
	}
	if e.count("watch_stats", taut.ID) != 0 || e.state(taut.ID).RefreshedAt.Equal(*before.RefreshedAt) {
		t.Fatal("allowChanges did not apply the empty history")
	}
}

func TestTautulliWatchesAnotherServer(t *testing.T) {
	e := newProvEnv(t)
	taut := e.addTautulli()
	e.mustRun(e.plexIt)
	e.plexSrv.SetIdentity("another-machine", "1.43.4")
	if _, _, err := e.run(taut, jobs.Params{}, false); err == nil || !strings.Contains(err.Error(), "another Plex server") {
		t.Fatalf("error = %v", err)
	}
	if e.count("watch_stats", taut.ID) != 0 || e.state(taut.ID).Status != StatusFailed {
		t.Fatal("rows were written")
	}
}

func TestTautulliURLChangeReplacesTheCache(t *testing.T) {
	e := newProvEnv(t)
	taut := e.addTautulli()
	e.mustRun(e.plexIt)
	e.mustRun(taut)
	moved := tautullitest.NewServer(t)
	for _, sec := range []string{"1", "2", "3"} {
		moved.SetHistory(sec, 0, 1000, 0, nil)
	}
	e.runner.o.Tautulli.HTTPClient = moved.Client()
	if _, err := e.ints.Update(context.Background(), taut.ID, integrations.Input{Name: taut.Name, URL: moved.URL, APIKey: tautullitest.Key}); err != nil {
		t.Fatal(err)
	}
	if e.fresh(taut).Fresh {
		t.Fatal("the cache of the old URL is still fresh")
	}
	cur, _ := e.ints.Get(context.Background(), taut.ID)
	stats, _ := e.mustRun(cur)
	// Another instance's cache is replaced, never guarded.
	if stats.GuardHeld || e.count("watch_stats", taut.ID) != 0 || !e.fresh(cur).Fresh {
		t.Fatalf("stats %+v", stats)
	}
}

func seerrRows(t *testing.T, e *provEnv, id int64) map[int64]SeerrRequest {
	t.Helper()
	out := map[int64]SeerrRequest{}
	if err := e.runner.Store().EachSeerrRequest(context.Background(), nil, id, func(r SeerrRequest) error {
		out[r.RequestID] = r
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSeerrRefresh(t *testing.T) {
	e := newProvEnv(t)
	s := e.addSeerr(true)
	stats, _ := e.mustRun(s)
	cache := stats.Cache.(SeerrStats)
	if cache.Requests != 10 || cache.Counted != 9 || cache.PlexIntegrationID != e.plexIt.ID || stats.AppVersion != "3.4.1" || stats.Requests != 3 {
		t.Fatalf("stats %+v / %+v", stats, cache)
	}
	rows := seerrRows(t, e, s.ID)
	if r := rows[6]; r.MediaType != "tv" || r.Status != 2 || !slices.Equal(r.Seasons, []int{1}) || r.TVDBID != 73587 || r.TMDBID != 6357 ||
		r.RatingKey != "28" || r.UserID != 2 || r.RequestedAt == nil {
		t.Fatalf("request 6 = %+v", r)
	}
	if r := rows[4]; !r.Is4K || r.Status != 4 {
		t.Fatalf("request 4 = %+v", r)
	}
	if r := rows[1]; r.RatingKey != "" || len(r.Seasons) != 0 {
		t.Fatalf("request 1 = %+v", r)
	}
	if strings.Contains(string(e.state(s.ID).Stats), "@") {
		t.Fatal("an e-mail was stored")
	}
}

// TestSeerrFailedPageKeepsTheCache: page 3 failing keeps the old rows and refreshed_at (§15).
func TestSeerrFailedPageKeepsTheCache(t *testing.T) {
	e := newProvEnv(t)
	s := e.addSeerr(false)
	e.runner.o.Seerr.PageSize = 4
	e.mustRun(s)
	before := e.state(s.ID)
	e.seerr.SetStatus(seerrtest.RequestKey(4, 8), 500)
	e.clock.Advance(time.Hour)
	if _, _, err := e.run(s, jobs.Params{}, false); err == nil {
		t.Fatal("no error")
	}
	after := e.state(s.ID)
	if e.count("seerr_requests", s.ID) != 10 || !after.RefreshedAt.Equal(*before.RefreshedAt) || after.Status != StatusFailed {
		t.Fatalf("state %+v", after)
	}
}

func maintainerrRows(t *testing.T, e *provEnv, id int64) []string {
	t.Helper()
	var out []string
	if err := e.runner.Store().EachMaintainerrItem(context.Background(), nil, id, func(m MaintainerrItem) error {
		s := fmt.Sprintf("%d:%s:%s:%s:%s", m.CollectionID, m.Level, m.RatingKey, m.State, m.LibraryID)
		if m.Season != nil {
			s += fmt.Sprintf(":S%d", *m.Season)
		}
		if m.Episode != nil {
			s += fmt.Sprintf("E%d", *m.Episode)
		}
		if m.PlexIntegrationID != e.plexIt.ID {
			t.Errorf("row %s has plex integration %d", s, m.PlexIntegrationID)
		}
		out = append(out, s)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func TestMaintainerrRefresh(t *testing.T) {
	e := newProvEnv(t)
	m := e.addMaintainerr(maintainerrtest.V341)
	// The Plex index is not refreshed yet: season and episode members are undecided.
	stats, rep := e.mustRun(m)
	if !rep.warned("not fresh") || stats.Cache.(MaintainerrStats).PlexIndexFresh {
		t.Fatalf("stats %+v", stats)
	}
	// 3.4.1 runs Maintainerr's older collection handler (maintainerr.OlderHandler): No deadline (4)
	// deletes every member at once, and excluded members are undecided, not dropped.
	want := []string{"1:movie:3:pending:1", "1:movie:4:pending:1", "1:movie:9:undecided:1", "4:movie:1:pending:1", "4:movie:2:pending:1",
		"4:movie:3:pending:1", "4:movie:4:pending:1", "4:movie:5:pending:1", "4:movie:9:undecided:1", "6:show:17:undecided:2", "6:show:28:pending:2",
		"7:season:18:undecided:2", "7:season:25:undecided:2", "7:season:29:undecided:2", "7:season:33:undecided:2",
		"8:episode:26:undecided:2", "8:episode:30:undecided:2", "8:episode:31:undecided:2"}
	if got := maintainerrRows(t, e, m.ID); !slices.Equal(got, want) {
		t.Fatalf("rows = %v", got)
	}
	e.mustRun(e.plexIt)
	stats, _ = e.mustRun(m)
	cache := stats.Cache.(MaintainerrStats)
	if !cache.PlexIndexFresh || cache.Pending != 10 || cache.Undecided != 8 || !cache.OlderHandler || cache.NoWindow != 6 || cache.Collections != 8 ||
		stats.AppVersion != "3.4.1" {
		t.Fatalf("stats %+v", cache)
	}
	want = []string{"1:movie:3:pending:1", "1:movie:4:pending:1", "1:movie:9:undecided:1", "4:movie:1:pending:1", "4:movie:2:pending:1",
		"4:movie:3:pending:1", "4:movie:4:pending:1", "4:movie:5:pending:1", "4:movie:9:undecided:1", "6:show:17:undecided:2", "6:show:28:pending:2",
		"7:season:18:pending:2:S1", "7:season:25:undecided:2:S2", "7:season:29:undecided:2:S1", "7:season:33:undecided:2:S2",
		"8:episode:26:pending:2:S2E1", "8:episode:30:undecided:2:S1E1", "8:episode:31:undecided:2:S1E2"}
	if got := maintainerrRows(t, e, m.ID); !slices.Equal(got, want) {
		t.Fatalf("rows = %v", got)
	}
}

// TestMaintainerrFailedExclusionKeepsTheCache: one exclusion call answering 500 keeps the old rows.
func TestMaintainerrFailedExclusionKeepsTheCache(t *testing.T) {
	e := newProvEnv(t)
	m := e.addMaintainerr(maintainerrtest.V341)
	e.mustRun(e.plexIt)
	e.mustRun(m)
	before := maintainerrRows(t, e, m.ID)
	e.maint.SetStatus(maintainerrtest.ExclusionKey("6"), 500)
	if _, _, err := e.run(m, jobs.Params{}, false); err == nil {
		t.Fatal("no error")
	}
	if got := maintainerrRows(t, e, m.ID); !slices.Equal(got, before) || e.state(m.ID).Status != StatusFailed || !e.fresh(m).Fresh {
		t.Fatalf("rows %v", got)
	}
}

func TestMaintainerrOldVersionRefused(t *testing.T) {
	e := newProvEnv(t)
	m := e.addMaintainerr(maintainerrtest.V341)
	e.maint.OldVersion("3.3.0")
	if _, _, err := e.run(m, jobs.Params{}, false); err == nil || !strings.Contains(err.Error(), "3.4.0") {
		t.Fatalf("error %v", err)
	}
}

func TestProviderRefreshRefusesWebhookParams(t *testing.T) {
	e := newProvEnv(t)
	s := e.addSeerr(false)
	if _, _, err := e.run(s, jobs.Params{ArrItemIDs: []int64{1}}, false); err == nil {
		t.Fatal("arrItemIds accepted")
	}
}

// TestProviderCrashMatrix crashes each provider refresh at its fault points: the cache is either
// the old one or the new one, never partial, and a rerun completes it.
func TestProviderCrashMatrix(t *testing.T) {
	points := []string{PointProviderFetched, PointProviderReplaced}
	kinds := []string{"plex", "tautulli", "seerr", "maintainerr"}
	for _, kind := range kinds {
		for _, point := range points {
			t.Run(kind+"/"+point, func(t *testing.T) {
				e := newProvEnv(t)
				var it integrations.Integration
				table := ""
				switch kind {
				case "plex":
					it, table = e.plexIt, "plex_items"
				case "tautulli":
					e.mustRun(e.plexIt)
					it, table = e.addTautulli(), "watch_stats"
				case "seerr":
					it, table = e.addSeerr(false), "seerr_requests"
				case "maintainerr":
					e.mustRun(e.plexIt)
					it, table = e.addMaintainerr(maintainerrtest.V341), "maintainerr_items"
				}
				full := func() int64 {
					e.mustRun(it)
					n := e.count(table, it.ID)
					err := e.db.Write(context.Background(), func(tx *sql.Tx) error {
						if _, err := tx.Exec(`DELETE FROM `+table+` WHERE integration_id = ?`, it.ID); err != nil {
							return err
						}
						_, err := tx.Exec(`DELETE FROM index_state WHERE integration_id = ?`, it.ID)
						return err
					})
					if err != nil {
						t.Fatal(err)
					}
					return n
				}
				want := full()
				crashed := func() (c bool) {
					defer func() {
						if r := recover(); r != nil {
							if _, ok := r.(faultinject.Crash); !ok {
								panic(r)
							}
							c = true
						}
					}()
					faultinject.SetHook(faultinject.CrashAt(point, 1))
					defer faultinject.SetHook(nil)
					_, _, _ = e.run(it, jobs.Params{}, false)
					return false
				}()
				if !crashed {
					t.Fatalf("no crash at %s", point)
				}
				got := e.count(table, it.ID)
				st := e.state(it.ID)
				switch point {
				case PointProviderFetched:
					if got != 0 || st.RefreshedAt != nil {
						t.Fatalf("after a crash before the write: %d rows, state %+v", got, st)
					}
				case PointProviderReplaced:
					if got != want || st.RefreshedAt == nil {
						t.Fatalf("after a crash after the write: %d rows (want %d), state %+v", got, want, st)
					}
				}
				e.mustRun(it)
				if e.count(table, it.ID) != want || !e.fresh(it).Fresh {
					t.Fatal("the rerun did not complete the cache")
				}
			})
		}
	}
}

func TestProviderStatsJSON(t *testing.T) {
	e := newProvEnv(t)
	stats, _ := e.mustRun(e.plexIt)
	b, err := json.Marshal(stats)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"integrationId", "integrationType", "items", "files", "requests", "guardHeld", "followUpJobs", "durationMs", "cache"} {
		if _, ok := m[k]; !ok {
			t.Errorf("stats lack %q", k)
		}
	}
}
