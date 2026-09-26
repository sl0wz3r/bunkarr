package tiers

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/integrations/httpread"
	"github.com/sl0wz3r/bunkarr/internal/integrations/maintainerr"
	"github.com/sl0wz3r/bunkarr/internal/integrations/maintainerr/maintainerrtest"
	"github.com/sl0wz3r/bunkarr/internal/integrations/plex"
	"github.com/sl0wz3r/bunkarr/internal/integrations/plex/plextest"
	"github.com/sl0wz3r/bunkarr/internal/integrations/seerr"
	"github.com/sl0wz3r/bunkarr/internal/integrations/seerr/seerrtest"
	"github.com/sl0wz3r/bunkarr/internal/integrations/tautulli"
	"github.com/sl0wz3r/bunkarr/internal/integrations/tautulli/tautullitest"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/mediaindex"
)

const libToken = "plex-token-tier-tests"

// libEnv is the tier env with the slice 9 recording: the recorded Plex library on disk (sources
// movies, tv and music), its Plex integration with the index on, and fake Tautulli, Seerr and
// Maintainerr, all refreshed through the real refresh runner.
type libEnv struct {
	*env
	plexSrv *plextest.Server
	lib     *plextest.Library
	plexIt  integrations.Integration
	runner  *mediaindex.Runner
	taut    *tautullitest.Server
	seerr   *seerrtest.Server
	maint   *maintainerrtest.Server
	tautIt  integrations.Integration
	seerrIt integrations.Integration
	maintIt integrations.Integration
	srcs    map[string]catalog.Source
	prov    *IndexProvider
	jobID   int64
}

// withProviders rebuilds the engine with providers (newEnv builds the stores it needs first).
func (e *env) withProviders(providers ...Provider) {
	o := e.eng.o
	o.Providers = providers
	e.eng = New(o)
}

func newLibEnv(t *testing.T) *libEnv {
	t.Helper()
	e := &libEnv{env: newEnv(t), srcs: map[string]catalog.Source{}}
	e.plexSrv = plextest.NewServer(t, libToken)
	e.lib = e.plexSrv.ServeLibrary(t)
	for _, st := range [][2]any{{"1", plex.TypeMovie}, {"2", plex.TypeEpisode}, {"3", plex.TypeTrack}} {
		for _, raw := range e.lib.Rows(st[0].(string), st[1].(int)) {
			var row struct {
				Media []struct {
					Part []struct {
						File string `json:"file"`
						Size int64  `json:"size"`
					} `json:"Part"`
				} `json:"Media"`
			}
			if err := json.Unmarshal(raw, &row); err != nil {
				t.Fatal(err)
			}
			for _, m := range row.Media {
				for _, p := range m.Part {
					e.writeFile(strings.TrimPrefix(p.File, "/data/"), p.Size)
				}
			}
		}
	}
	for _, name := range []string{"movies", "tv", "music"} {
		e.srcs[name] = e.scan(e.source(name, name))
	}
	settings, _ := json.Marshal(map[string]any{"pathMappings": []map[string]string{{"plex": "/data", "local": e.root}}, "index": map[string]any{"enabled": true}})
	var err error
	e.plexIt, err = e.ints.Create(e.ctx, integrations.Input{Type: integrations.TypePlex, Name: "Plex", URL: e.plexSrv.URL, APIKey: libToken, Settings: settings})
	if err != nil {
		t.Fatal(err)
	}
	e.taut = tautullitest.NewServer(t)
	e.seerr = seerrtest.NewServer(t)
	// 3.29.0: its collection handler honours exclusions and skips a collection without
	// deleteAfterDays (3.4.1's does neither, so its excluded members are undecided).
	e.maint = maintainerrtest.NewServer(t, maintainerrtest.V3290)
	e.runner, err = mediaindex.NewRunner(mediaindex.RefreshOptions{DB: e.db, Integrations: e.ints, Catalog: e.cat, Now: e.clock.Now,
		Plex:        plex.Options{HTTPClient: e.plexSrv.Client()},
		Tautulli:    tautulli.Options{Options: httpread.Options{HTTPClient: e.taut.Client()}},
		Seerr:       seerr.Options{Options: httpread.Options{HTTPClient: e.seerr.Client()}},
		Maintainerr: maintainerr.Options{Options: httpread.Options{HTTPClient: e.maint.Client()}}})
	if err != nil {
		t.Fatal(err)
	}
	linked := json.RawMessage(`{"plexIntegrationId":` + itoa64(e.plexIt.ID) + `}`)
	if e.tautIt, err = e.ints.Create(e.ctx, integrations.Input{Type: integrations.TypeTautulli, Name: "Tautulli", URL: e.taut.URL,
		APIKey: tautullitest.Key, Settings: linked}); err != nil {
		t.Fatal(err)
	}
	if e.seerrIt, err = e.ints.Create(e.ctx, integrations.Input{Type: integrations.TypeSeerr, Name: "Seerr", URL: e.seerr.URL,
		APIKey: seerrtest.Key, Settings: linked}); err != nil {
		t.Fatal(err)
	}
	if e.maintIt, err = e.ints.Create(e.ctx, integrations.Input{Type: integrations.TypeMaintainerr, Name: "Maintainerr", URL: e.maint.URL,
		Settings: linked}); err != nil {
		t.Fatal(err)
	}
	for _, it := range []integrations.Integration{e.plexIt, e.tautIt, e.seerrIt, e.maintIt} {
		e.refresh(it)
	}
	e.prov = NewIndexProvider(e.idx, e.ints, nil)
	e.withProviders(e.prov)
	return e
}

func itoa64(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func (e *libEnv) refresh(it integrations.Integration) {
	e.t.Helper()
	cur, err := e.ints.Get(e.ctx, it.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	e.jobID++
	_, err = e.runner.Run(e.ctx, jobs.Job{ID: e.jobID, Type: jobs.TypeRefresh, Trigger: jobs.TriggerManual, Params: jobs.Params{IntegrationID: cur.ID}},
		jobs.Env{Reporter: nopReporter{}, Items: &memItems{}})
	if err != nil {
		e.t.Fatalf("refresh %s: %v", cur.Name, err)
	}
}

// facts returns the facts of a file of a source (by its path relative to the source).
func (e *libEnv) facts(src, rel string) *Facts {
	e.t.Helper()
	ft, err := e.eng.FactsFor(e.ctx, e.fileID(e.srcs[src], rel), nil)
	if err != nil {
		e.t.Fatal(err)
	}
	return ft.Facts
}

const (
	nosferatu = "Nosferatu (1922)/Nosferatu (1922).mkv"
	general   = "The General (1926)/The General (1926).mkv"
	sherlock  = "Sherlock Jr. (1924)/Sherlock Jr. (1924).mkv"
	notld     = "Night of the Living Dead (1968)/Night of the Living Dead (1968).mkv"
	metro     = "Metropolis (1927)/Metropolis (1927).mkv"
	tzS01E01  = "The Twilight Zone (1959)/Season 01/The Twilight Zone (1959) - S01E01.mkv"
	tzS02E01  = "The Twilight Zone (1959)/Season 02/The Twilight Zone (1959) - S02E01.mkv"
	ahpS01E02 = "Alfred Hitchcock Presents (1955)/Season 01/Alfred Hitchcock Presents (1955) - S01E02.mkv"
	ahpS02E01 = "Alfred Hitchcock Presents (1955)/Season 02/Alfred Hitchcock Presents (1955) - S02E01.mkv"
	ahpS02E02 = "Alfred Hitchcock Presents (1955)/Season 02/Alfred Hitchcock Presents (1955) - S02E02.mkv"
)

func TestIndexProviderFieldsAvailable(t *testing.T) {
	e := newEnv(t)
	fields, err := e.eng.Fields(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fields {
		if f.Field == FieldTautulliPlayCount && f.Available {
			t.Fatal("tautulli.playCount available without a provider")
		}
	}
	e.withProviders(NewIndexProvider(e.idx, e.ints, nil))
	fields, err = e.eng.Fields(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fields {
		if !f.Available {
			t.Errorf("%s is unavailable: %s", f.Field, f.Reason)
		}
	}
}

func TestIndexProviderPlexFacts(t *testing.T) {
	e := newLibEnv(t)
	f := e.facts("movies", nosferatu)
	if f.Plex == nil || !f.Plex.Known || f.Plex.Section != itoa64(e.plexIt.ID)+":1" || f.Plex.AddedAt == nil {
		t.Fatalf("plex facts %+v", f.Plex)
	}
	if f := e.facts("tv", tzS01E01); f.Plex.Section != itoa64(e.plexIt.ID)+":2" {
		t.Fatalf("tv section %+v", f.Plex)
	}
	// A file no Plex item holds is still placed by its section's location.
	e.writeFile("movies/Extra/trailer.mkv", 10)
	e.srcs["movies"] = e.scan(e.srcs["movies"])
	if f := e.facts("movies", "Extra/trailer.mkv"); !f.Plex.Known || f.Plex.Section != itoa64(e.plexIt.ID)+":1" || f.Plex.AddedAt != nil {
		t.Fatalf("location fallback %+v", f.Plex)
	}
	// A stale index makes the section unknown.
	e.clock.Advance(73 * time.Hour)
	if f := e.facts("movies", nosferatu); f.Plex.Known || !strings.Contains(f.Plex.Why, "old") {
		t.Fatalf("stale plex facts %+v", f.Plex)
	}
}

func TestIndexProviderWatchFacts(t *testing.T) {
	e := newLibEnv(t)
	cases := []struct {
		src, rel string
		plays    int64
		watched  bool
	}{
		{"movies", nosferatu, 3, true},
		{"movies", general, 1, true},
		{"movies", notld, 0, false},
		{"tv", tzS01E01, 2, true},
		{"tv", ahpS02E01, 1, true},
		{"tv", ahpS02E02, 0, false},
		{"music", "Scott Joplin/Piano Rags/01 - Maple Leaf Rag.mp3", 2, true},
	}
	for _, c := range cases {
		w := e.facts(c.src, c.rel).Watch
		if w == nil || !w.Known || w.Plays != c.plays || (w.LastWatched != nil) != c.watched || w.LowerBound || w.IntegrationID != e.tautIt.ID {
			t.Errorf("%s: watch %+v", c.rel, w)
		}
	}
	// Evaluated: no row → never true, olderThan true, newerThan false (§8.2).
	ev := NewEvaluator(nil, 0, e.clock.Now())
	f := e.facts("movies", notld)
	for op, want := range map[string]Result{OpNever: True, OpOlderThan: True, OpNewerThan: False} {
		c := cnd(FieldTautulliLastWatch, op, 30)
		if op == OpNever {
			c = Condition{Field: FieldTautulliLastWatch, Op: OpNever}
		}
		cc, err := compileCondition(c, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := ev.evalCondition(cc, f).Result; got != want {
			t.Errorf("lastWatched %s = %v, want %v", op, got, want)
		}
	}
}

func TestIndexProviderWatchLowerBound(t *testing.T) {
	e := newLibEnv(t)
	e.taut.HistoryOff()
	e.refresh(e.tautIt)
	if w := e.facts("tv", tzS01E01).Watch; !w.Known || !w.LowerBound || w.Plays != 2 {
		t.Fatalf("watch %+v", w)
	}
	// The user without history makes every count a lower bound, movies too.
	if w := e.facts("movies", nosferatu).Watch; !w.LowerBound {
		t.Fatalf("watch %+v", w)
	}
	ev := NewEvaluator(nil, 0, e.clock.Now())
	f := e.facts("tv", tzS01E01)
	for _, tt := range []struct {
		c    Condition
		want Result
	}{
		{cnd(FieldTautulliPlayCount, OpGte, 1), True},
		{cnd(FieldTautulliPlayCount, OpGt, 5), Unknown},
		{cnd(FieldTautulliPlayCount, OpLt, 5), Unknown},
		{cnd(FieldTautulliPlayCount, OpEq, 2), Unknown},
	} {
		cc, _ := compileCondition(tt.c, 0, 0)
		if got := ev.evalCondition(cc, f).Result; got != tt.want {
			t.Errorf("%s %s: %v, want %v", tt.c.Op, tt.c.Value, got, tt.want)
		}
	}
}

func TestIndexProviderRequestFacts(t *testing.T) {
	e := newLibEnv(t)
	cases := []struct {
		src, rel string
		want     Result
		users    []int64
	}{
		{"movies", nosferatu, True, []int64{1}}, // request 3, completed, by the admin
		{"movies", "His Girl Friday (1940)/His Girl Friday (1940).mkv", True, []int64{2}},
		{"movies", metro, True, []int64{1}},   // the failed 4K request counts
		{"movies", general, False, []int64{}}, // never requested
		{"tv", tzS01E01, True, []int64{2}},    // season 1 requested by user 2
		{"tv", tzS02E01, True, []int64{3}},    // season 2 pending, by user 3
		{"tv", ahpS02E02, True, []int64{1}},   // seasons 1-7
		{"music", "Scott Joplin/Piano Rags/01 - Maple Leaf Rag.mp3", Unknown, []int64{}},
	}
	for _, c := range cases {
		r := e.facts(c.src, c.rel).Requests
		if r == nil || r.Requested != c.want || !slices.Equal(r.Users, c.users) {
			t.Errorf("%s: requests %+v", c.rel, r)
		}
	}
}

// TestIndexProviderSeerrSeasons covers the season rules of §8.2 with a Sonarr item: an empty
// seasons list covers every season, a season outside every request is false, and a file whose
// season is unknown (an extra) is true only when a request covers every season.
func TestIndexProviderSeerrSeasons(t *testing.T) {
	e := newLibEnv(t)
	sonarr := e.arr(integrations.TypeSonarr, "Sonarr", "/tv", "tv")
	e.fresh(sonarr)
	e.rootFolder(sonarr, 1, "/tv", "tv")
	tz := e.item(sonarr, itemSpec{kind: mediaindex.KindSeries, arrID: 1, title: "The Twilight Zone", path: "/tv/The Twilight Zone (1959)", root: "/tv",
		ext: mediaindex.ExternalIDs{TVDB: 73587, TMDB: 6357}})
	e.arrFile(sonarr, tz, 11, "/tv/"+tzS01E01, "tv/"+tzS01E01, 4096, e.clock.Now())
	e.exec(`UPDATE arr_files SET detail = '{"episodes":[{"episodeId":1,"seasonNumber":1,"episodeNumber":1}]}' WHERE arr_file_id = 11`)
	// A new season 3 file that neither Plex nor any request knows, and an extra.
	e.writeFile("tv/The Twilight Zone (1959)/Season 03/The Twilight Zone (1959) - S03E01.mkv", 4096)
	e.writeFile("tv/The Twilight Zone (1959)/poster-extra.mkv", 10)
	e.srcs["tv"] = e.scan(e.srcs["tv"])
	e.arrFile(sonarr, tz, 12, "/tv/The Twilight Zone (1959)/Season 03/The Twilight Zone (1959) - S03E01.mkv",
		"tv/The Twilight Zone (1959)/Season 03/The Twilight Zone (1959) - S03E01.mkv", 4096, e.clock.Now())
	e.exec(`UPDATE arr_files SET detail = '{"episodes":[{"episodeId":2,"seasonNumber":3,"episodeNumber":1}]}' WHERE arr_file_id = 12`)
	if r := e.facts("tv", tzS01E01).Requests; r.Requested != True || !slices.Equal(r.Users, []int64{2}) {
		t.Fatalf("S01E01 %+v", r)
	}
	if r := e.facts("tv", "The Twilight Zone (1959)/Season 03/The Twilight Zone (1959) - S03E01.mkv").Requests; r.Requested != False {
		t.Fatalf("S03E01 %+v", r)
	}
	// The extra's season is unknown and no request covers every season: unknown.
	if r := e.facts("tv", "The Twilight Zone (1959)/poster-extra.mkv").Requests; r.Requested != Unknown {
		t.Fatalf("extra %+v", r)
	}
	// A request with an empty seasons list covers every season, the extra too.
	e.exec(`UPDATE seerr_requests SET seasons = '[]' WHERE request_id = 7`)
	if r := e.facts("tv", "The Twilight Zone (1959)/poster-extra.mkv").Requests; r.Requested != True || !slices.Equal(r.Users, []int64{3}) {
		t.Fatalf("extra with an all-season request %+v", r)
	}
	if r := e.facts("tv", "The Twilight Zone (1959)/Season 03/The Twilight Zone (1959) - S03E01.mkv").Requests; r.Requested != True {
		t.Fatalf("S03E01 with an all-season request %+v", r)
	}
	// A stale Seerr cache makes everything unknown.
	e.clock.Advance(73 * time.Hour)
	e.fresh(sonarr)
	e.fresh(e.plexIt)
	if r := e.facts("tv", tzS01E01).Requests; r.Requested != Unknown || !strings.Contains(r.Why, "Seerr") {
		t.Fatalf("stale %+v", r)
	}
}

func TestIndexProviderMaintainerrFacts(t *testing.T) {
	e := newLibEnv(t)
	cases := []struct {
		src, rel string
		want     Result
	}{
		{"movies", nosferatu, True}, // collection 1
		{"movies", notld, True},     // collection 1, added by hand
		{"movies", general, False},  // excluded globally
		{"movies", sherlock, False}, // only in collections that delete nothing
		{"tv", tzS01E01, True},      // its show is pending (collection 6)
		{"tv", ahpS01E02, True},     // its season is pending (collection 7)
		{"tv", ahpS02E01, True},     // the episode is pending (collection 8)
		{"tv", ahpS02E02, False},    // nothing covers it
		{"music", "Scott Joplin/Piano Rags/01 - Maple Leaf Rag.mp3", False},
	}
	for _, c := range cases {
		m := e.facts(c.src, c.rel).Maintainerr
		if m == nil || m.Pending != c.want || m.IntegrationID != e.maintIt.ID {
			t.Errorf("%s: maintainerr %+v", c.rel, m)
		}
		if c.want == True && m.DeleteAfter == nil {
			t.Errorf("%s: no deletion date", c.rel)
		}
	}
}

// TestIndexProviderMaintainerrScoping: rating keys only mean something on their own server; the
// 4K library does not match the HD collection's tmdb; an undecided row is unknown; a new episode
// with no Plex item whose series has a season-level row is unknown.
func TestIndexProviderMaintainerrScoping(t *testing.T) {
	e := newLibEnv(t)
	// A second Plex server whose items reuse the same rating keys for other files.
	other := plextest.NewServer(t, libToken)
	other.ServeLibrary(t)
	other.SetIdentity("another-machine", "1.43.4")
	e.writeFile("movies-b/Nosferatu (1922)/Nosferatu (1922).mkv", 4096)
	e.srcs["movies-b"] = e.scan(e.source("movies-b", "movies-b"))
	settings, _ := json.Marshal(map[string]any{"pathMappings": []map[string]string{{"plex": "/data/movies", "local": filepath.Join(e.root, "movies-b")}},
		"index": map[string]any{"enabled": true}})
	plexB, err := e.ints.Create(e.ctx, integrations.Input{Type: integrations.TypePlex, Name: "Plex B", URL: other.URL, APIKey: libToken, Settings: settings})
	if err != nil {
		t.Fatal(err)
	}
	e.runner, _ = mediaindex.NewRunner(mediaindex.RefreshOptions{DB: e.db, Integrations: e.ints, Catalog: e.cat, Now: e.clock.Now,
		Plex: plex.Options{HTTPClient: other.Client()}})
	e.refresh(plexB)
	if m := e.facts("movies-b", "Nosferatu (1922)/Nosferatu (1922).mkv").Maintainerr; m.Pending == True {
		t.Fatalf("server B's file matched server A's pending key: %+v", m)
	}
	// Key churn: the row's key is no longer in the index; the tmdb fallback applies only in the
	// row's library (1), so a 4K copy in library 9 does not match.
	e.exec(`UPDATE maintainerr_items SET rating_key = '4000' WHERE rating_key = '4' AND collection_id = 1`)
	e.exec(`UPDATE plex_items SET external_ids = '{"tmdb":"653"}' WHERE rating_key = '4' AND integration_id = ?`, e.plexIt.ID)
	if m := e.facts("movies", nosferatu).Maintainerr; m.Pending != True {
		t.Fatalf("fallback in the row's library: %+v", m)
	}
	e.exec(`UPDATE maintainerr_items SET library_id = '9' WHERE rating_key = '4000'`)
	if m := e.facts("movies", nosferatu).Maintainerr; m.Pending != False {
		t.Fatalf("4K library matched the HD collection: %+v", m)
	}
	// An undecided row is unknown.
	e.exec(`UPDATE maintainerr_items SET state = 'undecided' WHERE rating_key = '28'`)
	if m := e.facts("tv", tzS01E01).Maintainerr; m.Pending != Unknown || !strings.Contains(m.Why, "exclusion") {
		t.Fatalf("undecided %+v", m)
	}
	// A new AHP S01 episode with no Plex item: its series matches the season-level row → unknown.
	sonarr := e.arr(integrations.TypeSonarr, "Sonarr", "/tv", "tv")
	e.fresh(sonarr)
	e.rootFolder(sonarr, 1, "/tv", "tv")
	ahp := e.item(sonarr, itemSpec{kind: mediaindex.KindSeries, arrID: 2, title: "AHP", path: "/tv/Alfred Hitchcock Presents (1955)", root: "/tv",
		ext: mediaindex.ExternalIDs{TVDB: 73614, TMDB: 5273}})
	rel := "Alfred Hitchcock Presents (1955)/Season 01/Alfred Hitchcock Presents (1955) - S01E04.mkv"
	e.writeFile("tv/"+rel, 4096)
	e.srcs["tv"] = e.scan(e.srcs["tv"])
	e.arrFile(sonarr, ahp, 21, "/tv/"+rel, "tv/"+rel, 4096, e.clock.Now())
	if m := e.facts("tv", rel).Maintainerr; m.Pending != Unknown {
		t.Fatalf("new episode %+v", m)
	}
}

// TestIndexProviderServerSwitch: pointing the Plex integration at another server makes the Tautulli
// and Maintainerr facts unknown until the Plex index refreshes (§15), and still unknown once it has
// (its rating keys are the new server's, the caches' rows the old one's) until those caches are
// refreshed against it themselves. The same server behind a new URL keeps its keys.
func TestIndexProviderServerSwitch(t *testing.T) {
	e := newLibEnv(t)
	other := plextest.NewServer(t, libToken)
	other.ServeLibrary(t)
	other.SetIdentity("another-machine", "1.43.4")
	if _, err := e.ints.Update(e.ctx, e.plexIt.ID, integrations.Input{Name: "Plex", URL: other.URL, APIKey: libToken}); err != nil {
		t.Fatal(err)
	}
	f := e.facts("movies", nosferatu)
	if f.Watch.Known || f.Maintainerr.Pending != Unknown {
		t.Fatalf("after the switch: watch %+v, maintainerr %+v", f.Watch, f.Maintainerr)
	}
	e.runner, _ = mediaindex.NewRunner(mediaindex.RefreshOptions{DB: e.db, Integrations: e.ints, Catalog: e.cat, Now: e.clock.Now,
		Plex:        plex.Options{HTTPClient: other.Client()},
		Maintainerr: maintainerr.Options{Options: httpread.Options{HTTPClient: e.maint.Client()}}})
	e.refresh(e.plexIt)
	f = e.facts("movies", nosferatu)
	if f.Watch.Known || !strings.Contains(f.Watch.Why, "another Plex server") || f.Maintainerr.Pending != Unknown ||
		!strings.Contains(f.Maintainerr.Why, "another Plex server") {
		t.Fatalf("after the index refresh: watch %+v, maintainerr %+v", f.Watch, f.Maintainerr)
	}
	us, err := e.prov.Unknown(e.ctx, e.db.Reader(), e.clock.Now())
	if err != nil || len(us) != 2 || us[0].IntegrationID != e.tautIt.ID || us[1].IntegrationID != e.maintIt.ID {
		t.Fatalf("unknown sources %+v, %v", us, err)
	}
	// Seerr's rating keys are the old server's too (the file has no *arr item, so no TMDB id).
	if r := f.Requests; r.Requested != Unknown || !strings.Contains(r.Why, "another Plex server") {
		t.Fatalf("requests %+v", r)
	}
	// Maintainerr refreshed against the new server's index counts again: the fake server serves the
	// same library, so its members agree with the new index (TestMaintainerrServerSwitchNeedsAgreeingMembers
	// covers a server whose keys name other items).
	e.refresh(e.maintIt)
	if m := e.facts("movies", nosferatu).Maintainerr; m.Pending != True {
		t.Fatalf("after the Maintainerr refresh: %+v", m)
	}
}

// TestIndexProviderSameServerNewURL: the same Plex server (machine identifier) behind a new URL
// keeps the Tautulli and Maintainerr facts once its index is refreshed.
func TestIndexProviderSameServerNewURL(t *testing.T) {
	e := newLibEnv(t)
	other := plextest.NewServer(t, libToken)
	other.ServeLibrary(t)
	if _, err := e.ints.Update(e.ctx, e.plexIt.ID, integrations.Input{Name: "Plex", URL: other.URL, APIKey: libToken}); err != nil {
		t.Fatal(err)
	}
	e.runner, _ = mediaindex.NewRunner(mediaindex.RefreshOptions{DB: e.db, Integrations: e.ints, Catalog: e.cat, Now: e.clock.Now,
		Plex: plex.Options{HTTPClient: other.Client()}})
	e.refresh(e.plexIt)
	if f := e.facts("movies", nosferatu); !f.Watch.Known || f.Maintainerr.Pending != True {
		t.Fatalf("after the index refresh: watch %+v, maintainerr %+v", f.Watch, f.Maintainerr)
	}
}

// TestIndexProviderAcceptance7: with "pending deletion → skip" Maintainerr's pending movie is skip;
// with the Maintainerr cache stale it is full again (unknown is never true, S14).
func TestIndexProviderAcceptance7(t *testing.T) {
	e := newLibEnv(t)
	dest := e.destination("d", e.srcs["movies"].ID)
	e.saveRules(RuleInput{Name: "Maintainerr deletes", Conditions: []Condition{cnd(FieldMaintainerrPending, OpIs, true)}, Action: Skip})
	got, _ := e.decisions(dest, e.srcs["movies"])
	if got[nosferatu].Tier != Skip || got[general].Tier != Full || got[notld].Tier != Skip {
		t.Fatalf("decisions %v", got)
	}
	e.clock.Advance(25 * time.Hour)
	got, sd := e.decisions(dest, e.srcs["movies"])
	if got[nosferatu].Tier != Full || got[nosferatu].UnknownPromoted {
		t.Fatalf("stale Maintainerr: %+v", got[nosferatu])
	}
	unknown := false
	for _, r := range got[nosferatu].Unknown {
		unknown = unknown || (r.Field == FieldMaintainerrPending && strings.Contains(r.Why, "Maintainerr"))
	}
	if !unknown {
		t.Fatalf("no unknown reason: %+v (%+v)", got[nosferatu], sd.Unknown)
	}
	us, err := e.prov.Unknown(e.ctx, e.db.Reader(), e.clock.Now())
	if err != nil || len(us) != 1 || us[0].IntegrationID != e.maintIt.ID {
		t.Fatalf("unknown sources %+v, %v", us, err)
	}
}

func TestIndexProviderNoIntegrations(t *testing.T) {
	e := newEnv(t)
	e.writeFile("m/a.mkv", 10)
	s := e.scan(e.source("m", "m"))
	e.withProviders(NewIndexProvider(e.idx, e.ints, nil))
	ft, err := e.eng.FactsFor(e.ctx, e.fileID(s, "a.mkv"), nil)
	if err != nil {
		t.Fatal(err)
	}
	f := ft.Facts
	if f.Watch.Known || !strings.Contains(f.Watch.Why, "no Tautulli") || f.Requests.Requested != Unknown || f.Maintainerr.Pending != Unknown || f.Plex != nil {
		t.Fatalf("facts %+v %+v %+v %+v", f.Watch, f.Requests, f.Maintainerr, f.Plex)
	}
}

func TestIndexProviderSuggestionsAndKnown(t *testing.T) {
	e := newLibEnv(t)
	e.prov.seerrUsers = func(context.Context, integrations.Integration) ([]Suggestion, error) {
		return []Suggestion{{Value: int64(1), Label: "fixture-admin"}, {Value: int64(9), Label: "no requests"}}, nil
	}
	secs, err := e.prov.Suggestions(e.ctx, FieldPlexSection)
	if err != nil || len(secs) != 3 || secs[1].Value != itoa64(e.plexIt.ID)+":2" || secs[1].Label != "Plex: TV Shows" {
		t.Fatalf("sections %+v, %v", secs, err)
	}
	users, err := e.prov.Suggestions(e.ctx, FieldSeerrRequestedBy)
	if err != nil || len(users) != 2 {
		t.Fatalf("users %+v, %v", users, err)
	}
	for _, tt := range []struct {
		field, value string
		ids          []int64
		want         bool
	}{
		{FieldPlexSection, itoa64(e.plexIt.ID) + ":2", nil, true},
		{FieldPlexSection, itoa64(e.plexIt.ID) + ":7", nil, false},
		{FieldSeerrRequestedBy, "", []int64{1, 2, 3}, true},
		{FieldSeerrRequestedBy, "", []int64{9}, true},
		{FieldSeerrRequestedBy, "", []int64{42}, false},
	} {
		got, err := e.prov.Known(e.ctx, e.db.Reader(), tt.field, tt.value, tt.ids, e.clock.Now())
		if err != nil || got != tt.want {
			t.Errorf("Known(%s %s %v) = %v, %v", tt.field, tt.value, tt.ids, got, err)
		}
	}
}
