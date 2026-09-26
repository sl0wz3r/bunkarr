package tiers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
)

// TestFactsDeletedArrStaysUnknown: deleting an *arr integration cascades its index, so without a
// record its files would look unmanaged and the spec preset would demote a tagged movie to
// manifest. S14: a deleted integration only makes a file more protected, until the user confirms.
func TestFactsDeletedArrStaysUnknown(t *testing.T) {
	l := newLibrary(t)
	l.specPreset()
	gone, err := l.eng.RememberDeletedArr(l.ctx, l.radarr)
	if err != nil || gone.Key == "" || len(gone.Folders) != 1 || gone.Folders[0] != filepath.Join(l.root, "movies") {
		t.Fatalf("record %+v, %v", gone, err)
	}
	if err := l.ints.Delete(l.ctx, l.radarr.ID); err != nil {
		t.Fatal(err)
	}
	got, sd := l.decisions(l.dest, l.movies)
	for _, rel := range []string{"Heat (1995)/Heat (1995).mkv", "Charade (1963)/Charade (1963).mkv", "Heat (1995)/Heat (1995).en.srt"} {
		d := got[rel]
		if d.Tier != Full || !d.UnknownPromoted {
			t.Errorf("%s after the delete: %+v", rel, d)
		}
	}
	if d := got["Heat (1995)/Heat (1995).mkv"]; len(d.Reasons) != 1 || !strings.Contains(d.Reasons[0].Why, "was deleted") {
		t.Errorf("heat reasons %+v", d.Reasons)
	}
	if len(sd.Unknown) != 1 || sd.Unknown[0].IntegrationID != l.radarr.ID || !strings.Contains(sd.Unknown[0].Reason, "confirm") {
		t.Errorf("unknown sources %+v", sd.Unknown)
	}
	// A file outside the deleted integration's folders is still unmanaged.
	if home, _ := l.decisions(l.dest, l.home); home["2019/birthday.mp4"].Tier != Manifest {
		t.Errorf("home video %+v", home["2019/birthday.mp4"])
	}
	// Once the user confirms, the files are unmanaged (manifest by the preset). A second
	// confirmation of the same record is ErrNotFound.
	if d, err := l.eng.ConfirmDeletedArr(l.ctx, gone.Key); err != nil || d.Key != gone.Key || d.IntegrationID != l.radarr.ID {
		t.Fatalf("confirm %+v, %v", d, err)
	}
	if _, err := l.eng.ConfirmDeletedArr(l.ctx, gone.Key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second confirm: %v", err)
	}
	if list, err := l.eng.DeletedArrs(l.ctx); err != nil || len(list) != 0 {
		t.Fatalf("records after forget %+v, %v", list, err)
	}
	got, sd = l.decisions(l.dest, l.movies)
	if d := got["Heat (1995)/Heat (1995).mkv"]; d.Tier != Manifest || d.UnknownPromoted || len(sd.Unknown) != 0 {
		t.Errorf("heat after confirming: %+v (%+v)", d, sd.Unknown)
	}
}

// TestFactsDeletedArrRecreated: a re-created integration over the same root folder decides its
// files again; the record then decides nothing and raises no notice.
func TestFactsDeletedArrRecreated(t *testing.T) {
	l := newLibrary(t)
	l.specPreset()
	if _, err := l.eng.RememberDeletedArr(l.ctx, l.radarr); err != nil {
		t.Fatal(err)
	}
	if err := l.ints.Delete(l.ctx, l.radarr.ID); err != nil {
		t.Fatal(err)
	}
	r2 := l.arr(integrations.TypeRadarr, "Radarr new", "/movies", "movies")
	l.rootFolder(r2, 1, "/movies", "movies")
	l.meta(r2, "tag", 1, "bunkarr-full", "")
	heat := l.item(r2, itemSpec{arrID: 1, title: "Heat", path: "/movies/Heat (1995)", root: "/movies", tags: []int64{1}})
	l.arrFile(r2, heat, 11, "/movies/Heat (1995)/Heat (1995).mkv", "movies/Heat (1995)/Heat (1995).mkv", 5*mb, l.clock.Now())
	l.fresh(r2)
	got, sd := l.decisions(l.dest, l.movies)
	if d := got["Heat (1995)/Heat (1995).mkv"]; d.Tier != Full || d.UnknownPromoted || d.RuleName != "Tagged bunkarr-full" {
		t.Errorf("heat %+v", d)
	}
	if len(sd.Unknown) != 0 {
		t.Errorf("unknown sources %+v", sd.Unknown)
	}
}

// TestFactsUnmappedRootFolderIsUnknown: an *arr root folder with no path mapping (both containers
// mount the same paths, so no mapping was set) leaves Bunkarr unable to tell which files are the
// *arr's: nothing is unmanaged, so the spec preset does not silently demote tagged files (S14).
func TestFactsUnmappedRootFolderIsUnknown(t *testing.T) {
	l := newLibrary(t)
	l.specPreset()
	l.exec(`UPDATE integrations SET settings = '{"pathMappings":[]}' WHERE id = ?`, l.radarr.ID)
	l.exec(`UPDATE arr_meta SET detail = '{"accessible":true}' WHERE integration_id = ? AND kind = 'root_folder'`, l.radarr.ID)
	l.exec(`UPDATE arr_files SET local_path = NULL, source_id = NULL, rel_path = NULL WHERE integration_id = ?`, l.radarr.ID)
	got, sd := l.decisions(l.dest, l.movies)
	d := got["Heat (1995)/Heat (1995).mkv"]
	if d.Tier != Full || !d.UnknownPromoted || len(d.Reasons) != 1 || !strings.Contains(d.Reasons[0].Why, "no path mapping") {
		t.Errorf("heat %+v", d)
	}
	if len(sd.Unknown) != 1 || sd.Unknown[0].IntegrationID != l.radarr.ID || !strings.Contains(sd.Unknown[0].Reason, "/movies") {
		t.Errorf("unknown sources %+v", sd.Unknown)
	}
	// The home video may be in that root folder too, as far as Bunkarr can tell.
	if home, _ := l.decisions(l.dest, l.home); home["2019/birthday.mp4"].Tier != Full || !home["2019/birthday.mp4"].UnknownPromoted {
		t.Errorf("home video %+v", home["2019/birthday.mp4"])
	}
}

// TestMediaFileOfStems: the sidecar lookup by dot-prefix finds the longest stem among the
// directory's *arr files (the first file of a shared stem wins), never the file itself.
func TestMediaFileOfStems(t *testing.T) {
	mk := func(rel string) *Facts { return &Facts{RelPath: rel} }
	x, xmp4, xen, hidden, noext := mk("d/X.mkv"), mk("d/X.mp4"), mk("d/X.en.mkv"), mk("d/.hidden"), mk("d/README")
	rows := map[*Facts]bool{x: true, xmp4: true, xen: true, hidden: true, noext: true}
	stems := mediaStems([]*Facts{x, xmp4, xen, hidden, noext, mk("d/Y.srt")}, rows)
	for _, tt := range []struct {
		rel  string
		want *Facts
	}{
		{"d/X.fr.srt", x},
		{"d/X.en.srt", xen},
		{"d/X.en.forced.srt", xen},
		{"d/X.nfo", x},
		{"d/Xa.srt", nil},
		{"d/.hidden.srt", hidden},
		{"d/README.txt", noext},
		{"d/Y.srt", nil},
	} {
		if got := mediaFileOf(mk(tt.rel), stems); got != tt.want {
			t.Errorf("%s follows %v, want %v", tt.rel, got, tt.want)
		}
	}
}

// BenchmarkSidecarsFlatDirectory: a flat directory of files without *arr rows is linear, not
// quadratic, in its size.
func BenchmarkSidecarsFlatDirectory(b *testing.B) {
	var files []*Facts
	rows := map[*Facts]bool{}
	for i := range 30000 {
		f := &Facts{RelPath: path.Join("Singles", fmt.Sprintf("track %05d.mp3", i))}
		files = append(files, f)
		if i%100 == 0 {
			rows[f] = true
		}
	}
	b.ResetTimer()
	for range b.N {
		stems := mediaStems(files, rows)
		for _, f := range files {
			if !rows[f] {
				mediaFileOf(f, stems)
			}
		}
	}
}

// TestIndexProviderMixedSections: a file whose Plex items are in two libraries of one server has
// no single plex.section (D12: mixed evidence is unknown), not the first item's.
func TestIndexProviderMixedSections(t *testing.T) {
	e := newLibEnv(t)
	local := filepath.Join(e.root, "movies", nosferatu)
	e.exec(`INSERT INTO plex_items (integration_id, rating_key, type, section_key, guid, title) VALUES (?, '9001', 'movie', '5', 'plex://movie/kids', 'Nosferatu')`, e.plexIt.ID)
	e.exec(`INSERT INTO plex_files (integration_id, rating_key, file, local_path, source_id, rel_path) VALUES (?, '9001', '/data/kids/n.mkv', ?, ?, ?)`,
		e.plexIt.ID, local, e.srcs["movies"].ID, nosferatu)
	f := e.facts("movies", nosferatu)
	if f.Plex == nil || f.Plex.Known || !strings.Contains(f.Plex.Why, "several Plex libraries") {
		t.Fatalf("plex facts %+v", f.Plex)
	}
	ev := NewEvaluator(nil, 0, e.clock.Now())
	cc, err := compileCondition(cnd(FieldPlexSection, OpIs, itoa64(e.plexIt.ID)+":1"), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := ev.evalCondition(cc, f).Result; got != Unknown {
		t.Fatalf("plex.section is 1: %v", got)
	}
}

// TestIndexProviderGUIDSharedByLiveItems: the Tautulli guid fallback is for rating-key churn; a 4K
// copy that shares the HD item's guid does not get the HD item's plays.
func TestIndexProviderGUIDSharedByLiveItems(t *testing.T) {
	e := newLibEnv(t)
	var guid string
	if err := e.db.Reader().QueryRowContext(e.ctx, `SELECT guid FROM plex_items WHERE integration_id = ? AND rating_key = '4'`, e.plexIt.ID).Scan(&guid); err != nil || guid == "" {
		t.Fatalf("nosferatu guid %q: %v", guid, err)
	}
	rel := "Nosferatu 4K/Nosferatu (1922) 4K.mkv"
	e.writeFile("movies/"+rel, 4096)
	e.srcs["movies"] = e.scan(e.srcs["movies"])
	e.exec(`INSERT INTO plex_items (integration_id, rating_key, type, section_key, guid, title) VALUES (?, '9060', 'movie', '1', ?, 'Nosferatu')`, e.plexIt.ID, guid)
	e.exec(`INSERT INTO plex_files (integration_id, rating_key, file, local_path, source_id, rel_path) VALUES (?, '9060', '/data/movies-4k/n.mkv', ?, ?, ?)`,
		e.plexIt.ID, filepath.Join(e.root, "movies", rel), e.srcs["movies"].ID, rel)
	if w := e.facts("movies", rel).Watch; !w.Known || w.Plays != 0 || w.LastWatched != nil {
		t.Fatalf("4K copy %+v", w)
	}
	if w := e.facts("movies", nosferatu).Watch; !w.Known || w.Plays != 3 {
		t.Fatalf("HD %+v", w)
	}
	// Plays under a rating key that is gone: which of the two items they belong to is unknown.
	e.exec(`DELETE FROM watch_stats WHERE key_type = 'rating_key' AND key = '4'`)
	if w := e.facts("movies", rel).Watch; w.Known || !strings.Contains(w.Why, "share this item's guid") {
		t.Fatalf("4K copy with orphan plays %+v", w)
	}
	// With one live item the guid row is its history (key churn).
	e.exec(`DELETE FROM plex_items WHERE rating_key = '9060'`)
	if w := e.facts("movies", nosferatu).Watch; !w.Known || w.Plays != 3 {
		t.Fatalf("HD after churn %+v", w)
	}
}

// TestSeerrUsersNeverFetchedInDecisions: a sync's decisions (and a manifest build) never wait on
// Seerr; the editor's fetch has a failure cache.
func TestSeerrUsersNeverFetchedInDecisions(t *testing.T) {
	e := newLibEnv(t)
	var calls atomic.Int64
	e.prov.seerrUsers = func(ctx context.Context, _ integrations.Integration) ([]Suggestion, error) {
		calls.Add(1)
		if _, ok := ctx.Deadline(); !ok {
			t.Error("the live user fetch has no deadline")
		}
		return nil, errors.New("Seerr is unreachable")
	}
	dest := e.destination("d", e.srcs["movies"].ID)
	e.saveRules(RuleInput{Name: "by 9", Conditions: []Condition{cnd(FieldSeerrRequestedBy, OpIn, []int{9}), cnd(FieldSeerrRequestedBy, OpIn, []int{42})},
		Action: Manifest})
	if n := calls.Load(); n != 1 {
		t.Fatalf("the save fetched the users %d times, want 1 (memoized per call)", n)
	}
	// No user list was read, so users 9 and 42 may exist: no stale reference (a warning on every
	// sync after a restart otherwise).
	for range 3 {
		if _, sd := e.decisions(dest, e.srcs["movies"]); len(sd.Stale) != 0 {
			t.Fatalf("stale references %+v", sd.Stale)
		}
	}
	if _, err := e.eng.DecisionsByID(e.ctx, nil, dest, e.srcs["movies"].ID); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("decisions fetched the Seerr users (%d fetches)", n)
	}
	// The editor: a failure is not retried at once.
	for range 3 {
		if _, err := e.eng.Fields(e.ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("the failed fetch was retried %d times", n-1)
	}
	// Once read, the list serves the decisions without a fetch: a user it lacks is stale.
	e.prov.mu.Lock()
	e.prov.users[e.seerrIt.ID] = userCache{at: time.Now().Add(-time.Hour), list: []Suggestion{{Value: int64(9)}}}
	e.prov.mu.Unlock()
	if _, sd := e.decisions(dest, e.srcs["movies"]); len(sd.Stale) != 1 || !strings.Contains(sd.Stale[0].Message, "42") {
		t.Fatalf("stale references with the list read %+v", sd.Stale)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("decisions fetched the Seerr users (%d fetches)", n)
	}
}

// countingProvider counts its loads.
type countingProvider struct {
	fakeProvider
	loads atomic.Int64
}

func (p *countingProvider) Load(ctx context.Context, q Queryer, src catalog.Source, files []*Facts, now time.Time) error {
	p.loads.Add(1)
	return p.fakeProvider.Load(ctx, q, src, files, now)
}

// TestProvidersLoadOnlyForTheirFields: the spec preset (arr.tag) loads no provider facts; a rule on
// a provider's field does.
func TestProvidersLoadOnlyForTheirFields(t *testing.T) {
	p := &countingProvider{fakeProvider: fakeProvider{fields: []string{FieldMaintainerrPending}, pending: map[string]bool{"Charade (1963)/Charade (1963).mkv": true}}}
	l := newLibrary(t, p)
	l.specPreset()
	l.decisions(l.dest, l.movies)
	if n := p.loads.Load(); n != 0 {
		t.Fatalf("the provider was loaded %d times for rules that do not use it", n)
	}
	l.saveRules(RuleInput{Name: "maint", Conditions: []Condition{cnd(FieldMaintainerrPending, OpIs, true)}, Action: Skip})
	got, _ := l.decisions(l.dest, l.movies)
	if p.loads.Load() != 1 || got["Charade (1963)/Charade (1963).mkv"].Tier != Skip {
		t.Fatalf("loads %d, charade %+v", p.loads.Load(), got["Charade (1963)/Charade (1963).mkv"])
	}
	// The Library item view shows every fact.
	if _, err := l.eng.FactsFor(l.ctx, l.fileID(l.movies, "Charade (1963)/Charade (1963).mkv"), nil); err != nil || p.loads.Load() != 2 {
		t.Fatalf("facts for one file: loads %d, %v", p.loads.Load(), err)
	}
}

// TestIndexCandidatesAreBuckets: the Seerr and Maintainerr joins look rows up by id instead of
// scanning every cached row per file.
func TestIndexCandidatesAreBuckets(t *testing.T) {
	e := newLibEnv(t)
	lc, err := e.prov.newLoad(e.ctx, e.db.Reader(), e.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	sx, err := lc.seerrIndex(e.seerrIt.ID)
	if err != nil || len(sx.reqs) < 3 {
		t.Fatalf("seerr index %+v, %v", sx, err)
	}
	r := sx.reqs[0]
	for _, i := range sx.candidates(r.MediaType, r.TMDBID, 0, nil) {
		if sx.reqs[i].TMDBID != r.TMDBID || sx.reqs[i].MediaType != r.MediaType {
			t.Fatalf("candidate %+v for tmdb %d", sx.reqs[i], r.TMDBID)
		}
	}
	if got := sx.candidates("movie", 0, 0, nil); len(got) != 0 {
		t.Fatalf("candidates without ids %v", got)
	}
	mx, err := lc.maintainerrIndex(e.maintIt.ID)
	if err != nil || len(mx.rows) == 0 {
		t.Fatalf("maintainerr index %+v, %v", mx, err)
	}
	for _, i := range mx.byKeys([]string{"4"}) {
		if mx.rows[i].RatingKey != "4" {
			t.Fatalf("row %+v for key 4", mx.rows[i])
		}
	}
}

// racingQueryer commits a rule save between Store.Rules' first revision read and its rules read,
// once.
type racingQueryer struct {
	Queryer
	save func()
	done bool
}

func (q *racingQueryer) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if !q.done && strings.Contains(query, "FROM tier_rules") {
		q.done = true
		q.save()
	}
	return q.Queryer.QueryContext(ctx, query, args...)
}

// TestRulesRevisionMatchesRules: a save between the two reads never pairs the old revision with
// the new rules (a release checked against that revision would pass after a rule change).
func TestRulesRevisionMatchesRules(t *testing.T) {
	e := newEnv(t)
	e.saveRules(RuleInput{Name: "old", Action: Full})
	q := &racingQueryer{Queryer: e.db.Reader(), save: func() { e.saveRules(RuleInput{Name: "new", Action: Skip}) }}
	rs, err := e.eng.Store().Rules(e.ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if rs.Revision != 2 || len(rs.Rules) != 1 || rs.Rules[0].Name != "new" {
		t.Fatalf("revision %d with rules %+v", rs.Revision, rs.Rules)
	}
}

// TestPreviewCacheExpiresWithoutRequests: a preview leaves memory at its TTL even when no request
// comes, and the kept previews are bounded by their items.
func TestPreviewCacheExpiresWithoutRequests(t *testing.T) {
	c := newPreviewCache(time.Now)
	c.ttl = 20 * time.Millisecond
	c.put(&storedPreview{TierPreview: TierPreview{ID: "a", CreatedAt: time.Now()}})
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		n := len(c.list)
		c.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the preview was not dropped at its TTL")
		}
		time.Sleep(5 * time.Millisecond)
	}
	c = newPreviewCache(time.Now)
	c.maxItems = 10
	for _, id := range []string{"x", "y", "z"} {
		c.put(&storedPreview{TierPreview: TierPreview{ID: id, CreatedAt: time.Now()}, items: make([]PreviewItem, 6)})
	}
	if _, ok := c.get("x"); ok {
		t.Error("the oldest preview was kept past the item bound")
	}
	if _, ok := c.get("z"); !ok {
		t.Error("the newest preview was dropped")
	}
}
