package tiers

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/integrations/httpread"
	"github.com/sl0wz3r/bunkarr/internal/integrations/maintainerr"
	"github.com/sl0wz3r/bunkarr/internal/integrations/plex"
	"github.com/sl0wz3r/bunkarr/internal/integrations/plex/plextest"
	"github.com/sl0wz3r/bunkarr/internal/mediaindex"
)

// TestDeletedArrKeepsUnmappedItemFolderUnknown: while Radarr is live, a movie folder no path
// mapping covers keeps the files outside every mapped folder unknown
// (TestFactsUnmappedItemFolderIsUnknown). Deleting that Radarr must not make the tagged file
// unmanaged, and so demoted (S14): the deleted record keeps the unmapped item folder, and the file
// stays unknown until the removal is confirmed.
func TestDeletedArrKeepsUnmappedItemFolderUnknown(t *testing.T) {
	l := newLibrary(t)
	dl := l.source("Downloads", "downloads")
	const rushmore = "Rushmore (1998)/Rushmore (1998).mkv"
	l.writeFile("downloads/"+rushmore, 2*mb)
	dl = l.scan(dl)
	dest := l.destination("nas2", dl.ID)
	l.specPreset()
	item := l.item(l.radarr, itemSpec{arrID: 9, title: "Rushmore", path: "/downloads/Rushmore (1998)", root: "/downloads", profile: 4, monitored: true,
		tags: []int64{1}})
	l.exec(`INSERT INTO arr_files (integration_id, item_id, arr_file_id, path, local_path, source_id, rel_path, size, quality, date_added, seen_at)
		VALUES (?, ?, 19, '/downloads/Rushmore (1998)/Rushmore (1998).mkv', NULL, NULL, NULL, ?, 'Bluray-1080p', ?, ?)`,
		l.radarr.ID, item, 2*mb, db.FormatTime(l.clock.Now()), db.FormatTime(l.clock.Now()))
	if got, _ := l.decisions(dest, dl); got[rushmore].Tier != Full || !got[rushmore].UnknownPromoted {
		t.Fatalf("live: %+v", got[rushmore])
	}
	gone, err := l.eng.RememberDeletedArr(l.ctx, l.radarr)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(gone.Unmapped, "/downloads/Rushmore (1998)") {
		t.Fatalf("deleted record: folders %v, unmapped %v", gone.Folders, gone.Unmapped)
	}
	if err := l.ints.Delete(l.ctx, l.radarr.ID); err != nil {
		t.Fatal(err)
	}
	got, sd := l.decisions(dest, dl)
	if d := got[rushmore]; d.Tier != Full || !d.UnknownPromoted || len(d.Reasons) == 0 || !strings.Contains(d.Reasons[0].Why, "was deleted") {
		t.Errorf("after the delete: %+v", d)
	}
	if !slices.ContainsFunc(sd.Unknown, func(u UnknownSource) bool {
		return u.IntegrationID == l.radarr.ID && strings.Contains(u.Reason, "was deleted")
	}) {
		t.Errorf("unknown sources %+v", sd.Unknown)
	}
	// The user confirms the removal: the file is unmanaged, and the rules decide it.
	if _, err := l.eng.ConfirmDeletedArr(l.ctx, gone.Key); err != nil {
		t.Fatal(err)
	}
	if got, _ := l.decisions(dest, dl); got[rushmore].Tier != Manifest || got[rushmore].UnknownPromoted {
		t.Errorf("after the confirmation: %+v", got[rushmore])
	}
}

// switchPlexServer points the Plex integration at another server serving the same recorded
// library (so the same rating keys), and refreshes its index.
func switchPlexServer(t *testing.T, e *libEnv) {
	t.Helper()
	other := plextest.NewServer(t, libToken)
	other.ServeLibrary(t)
	other.SetIdentity("another-machine", "1.43.4")
	if _, err := e.ints.Update(e.ctx, e.plexIt.ID, integrations.Input{Name: "Plex", URL: other.URL, APIKey: libToken}); err != nil {
		t.Fatal(err)
	}
	var err error
	e.runner, err = mediaindex.NewRunner(mediaindex.RefreshOptions{DB: e.db, Integrations: e.ints, Catalog: e.cat, Now: e.clock.Now,
		Plex:        plex.Options{HTTPClient: other.Client()},
		Maintainerr: maintainerr.Options{Options: httpread.Options{HTTPClient: e.maint.Client()}}})
	if err != nil {
		t.Fatal(err)
	}
	e.refresh(e.plexIt)
}

// TestMaintainerrFirstCheckNeedsAgreeingMembers: a Maintainerr integration created (or deleted
// and created again) while the linked Plex index is of a server whose rating keys name none of its
// members has no previous check; its rows must not count either: every member would be taken for
// key churn, and the TMDB fallback would mark this server's copy pending (S14).
func TestMaintainerrFirstCheckNeedsAgreeingMembers(t *testing.T) {
	e := newLibEnv(t)
	switchPlexServer(t, e)
	e.exec(`UPDATE plex_items SET rating_key = 'b' || rating_key, parent_key = 'b' || parent_key, grandparent_key = 'b' || grandparent_key WHERE integration_id = ?`, e.plexIt.ID)
	e.exec(`UPDATE plex_files SET rating_key = 'b' || rating_key WHERE integration_id = ?`, e.plexIt.ID)
	if err := e.ints.Delete(e.ctx, e.maintIt.ID); err != nil {
		t.Fatal(err)
	}
	linked, _ := json.Marshal(map[string]any{"plexIntegrationId": e.plexIt.ID})
	var err error
	if e.maintIt, err = e.ints.Create(e.ctx, integrations.Input{Type: integrations.TypeMaintainerr, Name: "Maintainerr", URL: e.maint.URL, Settings: linked}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		e.refresh(e.maintIt)
		if m := e.facts("movies", nosferatu).Maintainerr; m.Pending != Unknown || !strings.Contains(m.Why, "none of Maintainerr's collection members") ||
			!strings.Contains(m.Why, "link Maintainerr to the Plex server it manages") {
			t.Fatalf("refresh %d: %+v", i, m)
		}
	}
}

// TestMaintainerrServerSwitchWithoutExternalIDs: Plex items of a legacy agent (or unmatched ones)
// have no TMDB or TVDB ids to compare with Maintainerr's members. When Maintainerr moved with the
// Plex integration to the new server (every member's rating key names an item of the collection's
// level in the collection's library), its facts count again instead of staying unknown for good.
func TestMaintainerrServerSwitchWithoutExternalIDs(t *testing.T) {
	e := newLibEnv(t)
	noIDs := func() { e.exec(`UPDATE plex_items SET external_ids = '{}' WHERE integration_id = ?`, e.plexIt.ID) }
	noIDs()
	e.refresh(e.maintIt)
	if m := e.facts("movies", nosferatu).Maintainerr; m.Pending != True {
		t.Fatalf("before the switch: %+v", m)
	}
	switchPlexServer(t, e)
	noIDs()
	if m := e.facts("movies", nosferatu).Maintainerr; m.Pending != Unknown {
		t.Fatalf("switched, Maintainerr not refreshed: %+v", m)
	}
	e.refresh(e.maintIt)
	if m := e.facts("movies", nosferatu).Maintainerr; m.Pending != True {
		t.Fatalf("members match by key, level and library: %+v", m)
	}
	// The same keys in another library of the server: not the server Maintainerr manages.
	e.exec(`UPDATE plex_items SET section_key = '9' || section_key WHERE integration_id = ?`, e.plexIt.ID)
	e.refresh(e.maintIt)
	if m := e.facts("movies", nosferatu).Maintainerr; m.Pending != Unknown || !strings.Contains(m.Why, "name other items") {
		t.Fatalf("members in other libraries: %+v", m)
	}
}

// fourKCopy adds a 4K copy of Nosferatu in its own Bunkarr source (movies4k): a Plex item of the
// same library (section 1) with the same guid as the HD item of the "movies" source (key 4).
func fourKCopy(t *testing.T, e *libEnv) string {
	t.Helper()
	var guid string
	if err := e.db.Reader().QueryRowContext(e.ctx, `SELECT guid FROM plex_items WHERE integration_id = ? AND rating_key = '4'`, e.plexIt.ID).Scan(&guid); err != nil || guid == "" {
		t.Fatalf("nosferatu guid %q: %v", guid, err)
	}
	rel := "Nosferatu (1922) 4K/Nosferatu (1922) 4K.mkv"
	e.writeFile("movies4k/"+rel, 4096)
	e.srcs["movies4k"] = e.scan(e.source("movies4k", "movies4k"))
	e.exec(`INSERT INTO plex_items (integration_id, rating_key, type, section_key, guid, external_ids, title) VALUES (?, '9060', 'movie', '1', ?, '{"tmdb":"653"}', 'Nosferatu')`, e.plexIt.ID, guid)
	e.exec(`INSERT INTO plex_files (integration_id, rating_key, file, local_path, source_id, rel_path) VALUES (?, '9060', '/data/movies4k/n.mkv', ?, ?, ?)`,
		e.plexIt.ID, filepath.Join(e.root, "movies4k", rel), e.srcs["movies4k"].ID, rel)
	return rel
}

// TestGuidSiblingInOtherSource: a Load reads the Plex items of its own source only, but a guid's
// items across the whole server (PlexGUIDKeysUnder): the 4K copy in its own source is a sibling of
// the HD item in another source, so it does not get the HD item's plays, and plays left by a key
// the index no longer has are still seen as another item's.
func TestGuidSiblingInOtherSource(t *testing.T) {
	e := newLibEnv(t)
	rel := fourKCopy(t, e)
	if w := e.facts("movies4k", rel).Watch; !w.Known || w.Plays != 0 {
		t.Fatalf("4K copy in its own source: %+v", w)
	}
	e.exec(`DELETE FROM watch_stats WHERE key_type = 'rating_key' AND key = '4'`)
	if w := e.facts("movies4k", rel).Watch; w.Known || !strings.Contains(w.Why, "share this item's guid") {
		t.Fatalf("4K copy with the HD item's plays gone: %+v", w)
	}
}

// TestMaintainerrRowOfOtherSourceItem: a Load reads the Plex items of its own source only, but
// whether a Maintainerr row's rating key is in the index is a question about the whole server
// (plexKnown): a pending row for the HD item of another source is not key churn, so it does not
// mark the 4K copy (same TMDB id) pending.
func TestMaintainerrRowOfOtherSourceItem(t *testing.T) {
	e := newLibEnv(t)
	rel := fourKCopy(t, e)
	e.exec(`INSERT INTO maintainerr_items (integration_id, plex_integration_id, collection_id, collection_title, library_id, level, rating_key, tmdb_id, state)
		VALUES (?, ?, 777, 'HD cleanup', '1', 'movie', '4', 653, 'pending')`, e.maintIt.ID, e.plexIt.ID)
	if m := e.facts("movies4k", rel).Maintainerr; m == nil || m.Pending != False {
		t.Fatalf("4K copy: %+v", m)
	}
	if m := e.facts("movies", nosferatu).Maintainerr; m == nil || m.Pending != True {
		t.Fatalf("HD item: %+v", m)
	}
}
