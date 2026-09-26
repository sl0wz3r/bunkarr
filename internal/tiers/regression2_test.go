package tiers

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/integrations/httpread"
	"github.com/sl0wz3r/bunkarr/internal/integrations/maintainerr"
	"github.com/sl0wz3r/bunkarr/internal/integrations/plex"
	"github.com/sl0wz3r/bunkarr/internal/integrations/plex/plextest"
	"github.com/sl0wz3r/bunkarr/internal/mediaindex"
)

// TestFileAgeUsesPlexAddedAtInDecisions: file.age is the *arr's dateAdded, else Plex's addedAt,
// else first_seen_at (§8.2). A rule set that uses file.age but no Plex field must still read
// Plex's date in a sync (and a preview), as the Library item view does; otherwise a file Plex has
// had for years but Bunkarr first scanned recently is demoted.
func TestFileAgeUsesPlexAddedAtInDecisions(t *testing.T) {
	e := newLibEnv(t)
	e.exec(`UPDATE plex_items SET added_at = '2019-01-01T00:00:00.000000000Z' WHERE integration_id = ?`, e.plexIt.ID)
	dest := e.destination("d", e.srcs["movies"].ID)
	e.saveRules(
		RuleInput{Name: "old", Conditions: []Condition{cnd(FieldFileAge, OpOlderThan, 365)}, Action: Full},
		RuleInput{Name: "rest", Action: Manifest},
	)
	if f := e.facts("movies", nosferatu); f.Plex == nil || f.Plex.AddedAt == nil || f.Plex.AddedAt.Year() != 2019 {
		t.Fatalf("item view plex facts %+v", f.Plex)
	}
	got, _ := e.decisions(dest, e.srcs["movies"])
	if d := got[nosferatu]; d.Tier != Full {
		t.Fatalf("sync decides %s for a file Plex added in 2019: %+v", d.Tier, d)
	}
	// The preview of the saved rules: nosferatu counts under "old", never under "rest".
	pv, err := e.eng.Preview(e.ctx, PreviewRequest{DestinationIDs: []int64{dest}})
	if err != nil {
		t.Fatal(err)
	}
	page, err := e.eng.PreviewItems(pv.ID, ItemQuery{DestinationID: dest, Search: "Nosferatu (1922).mkv"})
	if err != nil || len(page.Records) != 1 || page.Records[0].Tier != Full || page.Records[0].RuleName != "old" {
		t.Fatalf("preview items %+v, %v", page.Records, err)
	}
}

// TestFactsUnmappedItemFolderIsUnknown: a Radarr movie whose path lies outside every root folder
// Radarr lists (a custom path, or a root folder removed from Radarr's settings) and that no path
// mapping covers: Bunkarr cannot place its file, so files outside every mapped folder are unknown
// (not unmanaged), and the tagged movie is not silently demoted (S14).
func TestFactsUnmappedItemFolderIsUnknown(t *testing.T) {
	l := newLibrary(t)
	dl := l.source("Downloads", "downloads")
	l.writeFile("downloads/Rushmore (1998)/Rushmore (1998).mkv", 2*mb)
	dl = l.scan(dl)
	dest := l.destination("nas2", dl.ID)
	l.specPreset()
	if got, sd := l.decisions(dest, dl); got["Rushmore (1998)/Rushmore (1998).mkv"].Tier != Manifest || len(sd.Unknown) != 0 {
		t.Fatalf("before: %+v, unknown %+v", got, sd.Unknown)
	}
	item := l.item(l.radarr, itemSpec{arrID: 9, title: "Rushmore", path: "/downloads/Rushmore (1998)", root: "/downloads", profile: 4, monitored: true,
		tags: []int64{1}})
	l.exec(`INSERT INTO arr_files (integration_id, item_id, arr_file_id, path, local_path, source_id, rel_path, size, quality, date_added, seen_at)
		VALUES (?, ?, 19, '/downloads/Rushmore (1998)/Rushmore (1998).mkv', NULL, NULL, NULL, ?, 'Bluray-1080p', ?, ?)`,
		l.radarr.ID, item, 2*mb, db.FormatTime(l.clock.Now()), db.FormatTime(l.clock.Now()))
	got, sd := l.decisions(dest, dl)
	d := got["Rushmore (1998)/Rushmore (1998).mkv"]
	if d.Tier != Full || !d.UnknownPromoted || len(d.Reasons) != 1 || !strings.Contains(d.Reasons[0].Why, "/downloads/Rushmore (1998)") {
		t.Errorf("rushmore %+v", d)
	}
	if len(sd.Unknown) != 1 || sd.Unknown[0].IntegrationID != l.radarr.ID || !strings.Contains(sd.Unknown[0].Reason, "no path mapping") {
		t.Errorf("unknown sources %+v", sd.Unknown)
	}
	// A mapped movie keeps its facts; only files outside every mapped folder are affected.
	movies, _ := l.decisions(l.dest, l.movies)
	if h := movies["Heat (1995)/Heat (1995).mkv"]; h.Tier != Full || h.UnknownPromoted {
		t.Errorf("heat %+v", h)
	}
	// Once a mapping covers the folder, the file is the item's again.
	l.exec(`UPDATE integrations SET settings = json_set(settings, '$.pathMappings[#]', json_object('arr', '/downloads', 'local', ?)) WHERE id = ?`,
		dl.Path, l.radarr.ID)
	got, sd = l.decisions(dest, dl)
	if d := got["Rushmore (1998)/Rushmore (1998).mkv"]; d.Tier != Full || d.UnknownPromoted || len(sd.Unknown) != 0 {
		t.Errorf("mapped: %+v, unknown %+v", d, sd.Unknown)
	}
}

// TestPlexIndexReadIsScopedToTheSource: a Load (once per source, per sync, release check and
// preview) reads the Plex items of the source's files and their ancestors, not every item of the
// server: 200k unrelated items cost nothing.
func TestPlexIndexReadIsScopedToTheSource(t *testing.T) {
	e := newLibEnv(t)
	e.exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 5000)
		INSERT INTO plex_items (integration_id, rating_key, type, section_key, guid, external_ids, title)
		SELECT ?, 'x'||i, 'episode', '2', 'plex://episode/x'||i, '{"tvdb":"1"}', 'ep '||i FROM n`, e.plexIt.ID)
	var total int
	if err := e.db.Reader().QueryRowContext(e.ctx, `SELECT count(*) FROM plex_items WHERE integration_id = ?`, e.plexIt.ID).Scan(&total); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"movies", "tv"} {
		lc, err := e.prov.newLoad(e.ctx, e.db.Reader(), e.clock.Now())
		if err != nil {
			t.Fatal(err)
		}
		lc.src = e.srcs[name]
		d, err := lc.plexOf(e.plexIt.ID)
		if err != nil || d == nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(d.items) == 0 || len(d.items) > 100 {
			t.Fatalf("%s: read %d of %d Plex items", name, len(d.items), total)
		}
		for local, keys := range d.keysAt {
			for _, k := range keys {
				it, ok := d.items[k]
				if !ok {
					t.Fatalf("%s: item %s of %s not read", name, k, local)
				}
				for _, anc := range []string{it.ParentKey, it.GrandparentKey} {
					if _, ok := d.items[anc]; anc != "" && !ok {
						t.Fatalf("%s: ancestor %s of item %s not read", name, anc, k)
					}
				}
			}
		}
		if d.byGUID != nil {
			t.Fatalf("%s: the guid siblings were read without a need", name)
		}
	}
	// The facts are those of a full read.
	if w := e.facts("movies", nosferatu).Watch; !w.Known || w.Plays != 3 {
		t.Fatalf("nosferatu %+v", w)
	}
	if f := e.facts("tv", tzS01E01); f.Plex == nil || !f.Plex.Known || f.Maintainerr == nil || f.Maintainerr.Pending != True {
		t.Fatalf("tz s01e01 plex %+v maintainerr %+v", f.Plex, f.Maintainerr)
	}
}

// TestSeerrUserWithoutRequestAfterRestart: after a restart no Seerr user list is in memory, and a
// sync never reads one live; a rule naming a real Seerr user with no cached request must not warn
// as a stale reference on every sync (the job would end completed_with_warnings) until someone
// opens the rule editor.
func TestSeerrUserWithoutRequestAfterRestart(t *testing.T) {
	e := newLibEnv(t)
	var fetches atomic.Int64
	users := func(context.Context, integrations.Integration) ([]Suggestion, error) {
		fetches.Add(1)
		return []Suggestion{{Value: int64(900), Label: "mom"}}, nil
	}
	e.prov.seerrUsers = users
	dest := e.destination("d", e.srcs["movies"].ID)
	rev, err := e.eng.Revision(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	rules := []RuleInput{{Name: "mom", Conditions: []Condition{cnd(FieldSeerrRequestedBy, OpIn, []int{900})}, Action: Full}, {Name: "rest", Action: Manifest}}
	if _, warns, err := e.eng.SaveRules(e.ctx, rev, rules); err != nil || len(warns) != 0 {
		t.Fatalf("save: %+v, %v", warns, err)
	}
	// The restart: a new provider, nothing in memory, Seerr reachable.
	p2 := NewIndexProvider(e.idx, e.ints, users)
	e.withProviders(p2)
	before := fetches.Load()
	if _, sd := e.decisions(dest, e.srcs["movies"]); len(sd.Stale) != 0 {
		t.Fatalf("stale references after a restart %+v", sd.Stale)
	}
	if fetches.Load() != before {
		t.Fatal("the sync read the Seerr users live")
	}
	// Once the editor read the list, a user it lacks is stale in a sync.
	p2.mu.Lock()
	p2.users[e.seerrIt.ID] = userCache{at: time.Now(), list: []Suggestion{{Value: int64(901)}}}
	p2.mu.Unlock()
	if _, sd := e.decisions(dest, e.srcs["movies"]); len(sd.Stale) != 1 {
		t.Fatalf("stale references with the list read %+v", sd.Stale)
	}
}

// TestSeerrUserLookupIsMemoizedPerCall: one staleness check (a save, a preview, a sync) reads the
// Seerr users once, so all its conditions see one list even when the list changes meanwhile.
func TestSeerrUserLookupIsMemoizedPerCall(t *testing.T) {
	e := newLibEnv(t)
	e.prov.seerrUsers = func(context.Context, integrations.Integration) ([]Suggestion, error) {
		return nil, errors.New("unreachable")
	}
	set := func(id int64) {
		e.prov.mu.Lock()
		e.prov.users[e.seerrIt.ID] = userCache{at: time.Now(), list: []Suggestion{{Value: id}}}
		e.prov.mu.Unlock()
	}
	q, now := e.db.Reader(), e.clock.Now()
	set(900)
	ctx := withKnownMemo(e.ctx, false)
	if ok, err := e.prov.Known(ctx, q, FieldSeerrRequestedBy, "", []int64{900}, now); err != nil || !ok {
		t.Fatalf("900 in the list: %v, %v", ok, err)
	}
	set(901)
	if ok, err := e.prov.Known(ctx, q, FieldSeerrRequestedBy, "", []int64{900}, now); err != nil || !ok {
		t.Fatalf("the second condition of the call read the list again: %v, %v", ok, err)
	}
	if ok, err := e.prov.Known(withKnownMemo(e.ctx, false), q, FieldSeerrRequestedBy, "", []int64{900}, now); err != nil || ok {
		t.Fatalf("a new call kept the old list: %v, %v", ok, err)
	}
}

// TestMaintainerrServerSwitchNeedsAgreeingMembers: Maintainerr's API does not say which Plex server
// it manages. After the Plex integration is switched to another server, a Maintainerr refresh must
// not make the old server's rating keys count against the new server's index (a pendingDelete on
// an unrelated file): its members must agree with the new index first (S14).
func TestMaintainerrServerSwitchNeedsAgreeingMembers(t *testing.T) {
	e := newLibEnv(t)
	other := plextest.NewServer(t, libToken)
	other.ServeLibrary(t)
	other.SetIdentity("another-machine", "1.43.4")
	if _, err := e.ints.Update(e.ctx, e.plexIt.ID, integrations.Input{Name: "Plex", URL: other.URL, APIKey: libToken}); err != nil {
		t.Fatal(err)
	}
	e.runner, _ = mediaindex.NewRunner(mediaindex.RefreshOptions{DB: e.db, Integrations: e.ints, Catalog: e.cat, Now: e.clock.Now,
		Plex:        plex.Options{HTTPClient: other.Client()},
		Maintainerr: maintainerr.Options{Options: httpread.Options{HTTPClient: e.maint.Client()}}})
	e.refresh(e.plexIt)
	unknownBecause := func(step, want string) {
		t.Helper()
		if m := e.facts("movies", nosferatu).Maintainerr; m.Pending != Unknown || !strings.Contains(m.Why, want) {
			t.Fatalf("%s: %+v", step, m)
		}
		us, err := e.prov.Unknown(e.ctx, e.db.Reader(), e.clock.Now())
		if err != nil || !slices.ContainsFunc(us, func(u UnknownSource) bool { return u.IntegrationID == e.maintIt.ID && strings.Contains(u.Reason, want) }) {
			t.Fatalf("%s: unknown sources %+v, %v", step, us, err)
		}
	}
	// The new server's keys name none of Maintainerr's members.
	e.exec(`UPDATE plex_items SET rating_key = 'b' || rating_key, parent_key = 'b' || parent_key, grandparent_key = 'b' || grandparent_key WHERE integration_id = ?`, e.plexIt.ID)
	e.exec(`UPDATE plex_files SET rating_key = 'b' || rating_key WHERE integration_id = ?`, e.plexIt.ID)
	e.refresh(e.maintIt)
	unknownBecause("keys absent", "none of Maintainerr's collection members")
	// The mismatch persists across refreshes while Maintainerr still manages the old server (the
	// rejected check is the previous one now: a refresh must not accept on no evidence).
	e.refresh(e.maintIt)
	unknownBecause("keys absent, refreshed again", "none of Maintainerr's collection members")
	// The new server's keys name other movies and shows.
	e.refresh(e.plexIt)
	e.exec(`UPDATE plex_items SET external_ids = json_object('tmdb', '9' || rating_key, 'tvdb', '9' || rating_key) WHERE integration_id = ?`, e.plexIt.ID)
	e.refresh(e.maintIt)
	unknownBecause("keys of other items", "name other items")
	// Maintainerr now manages that server: its members agree with the index, and count again.
	e.refresh(e.plexIt)
	e.refresh(e.maintIt)
	if m := e.facts("movies", nosferatu).Maintainerr; m.Pending != True {
		t.Fatalf("members agree: %+v", m)
	}
}

// TestMaintainerrRefreshWithStalePlexKeepsTheCheckedServer: a Maintainerr refresh while the
// linked Plex index is stale cannot check its members against it, so it keeps the server the last
// check accepted: rows checked against the old server never count against the new one's index.
func TestMaintainerrRefreshWithStalePlexKeepsTheCheckedServer(t *testing.T) {
	e := newLibEnv(t)
	other := plextest.NewServer(t, libToken)
	other.ServeLibrary(t)
	other.SetIdentity("another-machine", "1.43.4")
	if _, err := e.ints.Update(e.ctx, e.plexIt.ID, integrations.Input{Name: "Plex", URL: other.URL, APIKey: libToken}); err != nil {
		t.Fatal(err)
	}
	e.runner, _ = mediaindex.NewRunner(mediaindex.RefreshOptions{DB: e.db, Integrations: e.ints, Catalog: e.cat, Now: e.clock.Now,
		Plex:        plex.Options{HTTPClient: other.Client()},
		Maintainerr: maintainerr.Options{Options: httpread.Options{HTTPClient: e.maint.Client()}}})
	e.refresh(e.plexIt)
	e.clock.Advance(73 * time.Hour) // the new server's index goes stale
	e.refresh(e.maintIt)
	e.refresh(e.plexIt)
	if m := e.facts("movies", nosferatu).Maintainerr; m.Pending != Unknown || !strings.Contains(m.Why, "another Plex server") {
		t.Fatalf("maintainerr %+v", m)
	}
}
