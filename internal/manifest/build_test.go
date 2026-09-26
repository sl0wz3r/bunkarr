package manifest

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr/arrtest"
	"github.com/sl0wz3r/bunkarr/internal/manifest/manifesttest"
)

// livePlans decodes the fake *arrs' state from their raw API JSON (test-local, acceptance 3).
func (e *testEnv) livePlans() map[int64]Plan {
	e.t.Helper()
	out := map[int64]Plan{}
	for _, x := range []struct {
		it   integrations.Integration
		kind string
		url  string
	}{{e.rad, "radarr", e.radarr.URL}, {e.son, "sonarr", e.sonarr.URL}} {
		raw, err := manifesttest.LivePlan(x.kind, x.it.ID, manifesttest.HTTPGetter(x.url, "/api/v3", arrKey))
		if err != nil {
			e.t.Fatalf("decode the live %s state: %v", x.kind, err)
		}
		var p Plan
		if err := json.Unmarshal(raw, &p); err != nil {
			e.t.Fatal(err)
		}
		out[x.it.ID] = p
	}
	return out
}

// requireRoundTrip writes m, parses it back and compares its re-import plan with the live state
// of every *arr: there must be no difference, and every item of every *arr must be listed.
func requireRoundTrip(t *testing.T, m *Manifest, live map[int64]Plan) {
	t.Helper()
	data, err := EncodeJSON(m)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	plan := parsed.ReimportPlan()
	total := 0
	for id, lp := range live {
		total += len(lp.Items)
		if diffs := ComparePlan(plan.ForIntegration(id), lp); len(diffs) > 0 {
			for _, d := range diffs {
				t.Errorf("integration %d: %s", id, d)
			}
		}
	}
	if len(plan.Items) != total {
		t.Fatalf("the manifest lists %d items, the *arrs %d", len(plan.Items), total)
	}
}

func TestRoundTripAgainstTheRecordedArrState(t *testing.T) {
	// Acceptance 3 at unit level: for every item of the recorded Radarr (with the /movies-4k root
	// folder no mapping covers) and Sonarr, whatever the tiers and the path mappings, the
	// re-import plan of a destination version and of an export equals the *arr's state as a
	// test-local decoder reads it from the raw API JSON.
	e := newEnv(t)
	e.recordAll()
	live := e.livePlans()
	cases := []struct {
		name  string
		setup func()
	}{
		{"default tiers and mappings", func() {}},
		{"every file skip or manifest", func() {
			for _, rel := range []string{movieFile, sidecarFile, posterFile, otherFile} {
				e.tiers.set(rel, TierSkip)
			}
			e.tiers.set("tv/The Beverly Hillbillies/Season 1/The Beverly Hillbillies - S01E01 - The Clampetts Strike Oil WEBDL-1080p.mkv", TierManifest)
		}},
		{"Sonarr unmapped", func() {
			e.son = e.setMappings(e.son, map[string]string{})
			e.refresh(e.son.ID)
		}},
		{"a movie deleted in Radarr", func() {
			var movies []map[string]any
			if err := json.Unmarshal(mustGet(t, e.radarr.URL+"/api/v3/movie"), &movies); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(movies[1:])
			e.radarr.SetJSON("GET", "movie", raw)
			e.refresh(e.rad.ID)
			live = e.livePlans()
		}},
		{"Radarr mapped to the wrong folder", func() {
			e.rad = e.setMappings(e.rad, map[string]string{"/movies": filepath.Join(e.root, "tv")})
			e.refresh(e.rad.ID)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.setup()
			d := e.dest
			requireRoundTrip(t, e.build(&d), live)
			requireRoundTrip(t, e.build(nil), live)
		})
	}
	// A written version read with ParseDir round-trips too.
	e.runOK()
	vs := e.versions()
	m, err := ParseDir(filepath.Join(e.target, filepath.FromSlash(vs[0].Path)))
	if err != nil {
		t.Fatal(err)
	}
	requireRoundTrip(t, m, live)
}

// itemByTitle returns the item with a title.
func itemByTitle(t *testing.T, m *Manifest, title string) Item {
	t.Helper()
	for _, it := range m.Items {
		if it.Title == title {
			return it
		}
	}
	t.Fatalf("no item %q", title)
	return Item{}
}

func TestBuildDestinationView(t *testing.T) {
	e := newEnv(t)
	d := e.dest
	m := e.build(&d)
	if m.Format != FormatName || m.FormatVersion != 1 || m.Scope.Kind != ScopeDestination || m.Scope.DestinationID != d.ID ||
		m.Scope.DestinationName != "UNAS" || m.Generator.Name != "Bunkarr" || m.Generator.Version != "test" || !m.CreatedAt.Equal(e.clock.Now()) {
		t.Fatalf("header %+v %+v %+v", m.Scope, m.Generator, m.CreatedAt)
	}
	if len(m.Integrations) != 2 || !m.Integrations[0].Fresh || m.Integrations[0].Status != "ok" || m.Integrations[0].AppVersion == "" ||
		len(m.Integrations[0].Tags) != 2 || len(m.Integrations[0].RootFolders) != 2 {
		t.Fatalf("integrations %+v", m.Integrations)
	}
	if len(m.Sources) != 1 || m.Sources[0].ID != e.src.ID || m.Sources[0].DestFolder != e.src.DestFolder {
		t.Fatalf("sources %+v", m.Sources)
	}
	// Radarr's 5 movies (one without a file, one in /movies-4k) and Sonarr's 2 series.
	if m.Summary.Items != 7 || m.Summary.UnlocatedItems != 1 {
		t.Fatalf("summary %+v", m.Summary)
	}
	notld := itemByTitle(t, m, "Night of the Living Dead")
	if !notld.Located || notld.QualityProfile == "" || len(notld.Tags) != 1 || notld.Tags[0] != "bunkarr-full" || len(notld.Files) != 1 {
		t.Fatalf("movie 1 %+v", notld)
	}
	f := notld.Files[0]
	if f.Source == nil || f.Source.ID != e.src.ID || f.Source.RelPath != movieFile || f.Tier == nil || *f.Tier != TierFull ||
		f.Rule == nil || f.Rule.ID != 0 || f.Rule.Name != FallbackRuleName || f.Kept == nil || *f.Kept || f.BackedUp == nil || *f.BackedUp || f.SHA256 != nil {
		t.Fatalf("movie 1 file %+v", f)
	}
	// The sidecar follows its media file (stem), the poster its folder; the home video is no
	// item's.
	if len(notld.ExtraFiles) != 2 || notld.ExtraFiles[0].RelPath != sidecarFile || notld.ExtraFiles[1].RelPath != posterFile {
		t.Fatalf("movie 1 extras %+v", notld.ExtraFiles)
	}
	if len(m.OtherFiles) != 1 || m.OtherFiles[0].RelPath != otherFile || *m.OtherFiles[0].Source != e.src.ID {
		t.Fatalf("other files %+v", m.OtherFiles)
	}
	general := itemByTitle(t, m, "The General")
	if !general.Located || len(general.Files) != 0 || general.Monitored {
		t.Fatalf("movie 4 %+v", general)
	}
	// Movie 5 lies under /movies-4k, which no mapping covers: listed, unlocated, its file at no
	// source and not at this destination.
	nos := itemByTitle(t, m, "Nosferatu")
	if nos.Located || len(nos.Files) != 1 || nos.Files[0].Source != nil || nos.Files[0].Tier != nil || *nos.Files[0].Kept || *nos.Files[0].BackedUp {
		t.Fatalf("movie 5 %+v", nos)
	}
	// Sonarr: the multi-episode file 7 lists S01E04 and S01E05; the detail holds every episode.
	hill := itemByTitle(t, m, "The Beverly Hillbillies")
	var multi *File
	for i := range hill.Files {
		if hill.Files[i].ArrFileID == 7 {
			multi = &hill.Files[i]
		}
	}
	if multi == nil || len(multi.Episodes) != 2 || multi.Episodes[0] != (FileEpisode{1, 4}) || multi.Episodes[1] != (FileEpisode{1, 5}) {
		t.Fatalf("file 7 %+v", multi)
	}
	if len(hill.Detail.Seasons) != 10 || len(hill.Detail.Episodes) != 286 || hill.Detail.SeriesType != "standard" ||
		hill.Detail.SeasonFolder == nil || !*hill.Detail.SeasonFolder {
		t.Fatalf("series detail: %d seasons, %d episodes, %+v", len(hill.Detail.Seasons), len(hill.Detail.Episodes), hill.Detail.SeriesType)
	}
	// Totals: 3 Radarr files at the source + 1 in /movies-4k, 6 Sonarr files, 3 extra files.
	if m.Summary.Files != 13 || m.Summary.Tiers.Full.Files != 12 || m.Summary.KeptFiles != 0 || m.Summary.BackedUpBytes != 0 || m.Summary.LeftOut != 0 {
		t.Fatalf("summary %+v", m.Summary)
	}

	// Recorded at the destination: backed up, with the record's hash.
	e.recordAll()
	m = e.build(&d)
	f = itemByTitle(t, m, "Night of the Living Dead").Files[0]
	if !*f.BackedUp || f.SHA256 == nil || *f.SHA256 != strings.Repeat("ab", 32) {
		t.Fatalf("recorded file %+v", f)
	}
	if m.Summary.BackedUpBytes != m.Summary.Tiers.Full.Bytes {
		t.Fatalf("summary %+v", m.Summary)
	}
}

func TestBuildExportScope(t *testing.T) {
	e := newEnv(t)
	e.recordAll()
	m := e.build(nil)
	if m.Scope.Kind != ScopeExport || m.Scope.DestinationID != 0 || m.Job != nil {
		t.Fatalf("scope %+v job %+v", m.Scope, m.Job)
	}
	for _, it := range m.Items {
		for _, f := range it.Files {
			if f.Tier != nil || f.Kept != nil || f.BackedUp != nil || f.Rule != nil || f.SHA256 != nil {
				t.Fatalf("export file %+v", f)
			}
		}
		for _, x := range it.ExtraFiles {
			if x.Tier != nil || x.Kept != nil || x.BackedUp != nil {
				t.Fatalf("export extra %+v", x)
			}
		}
	}
	if m.Summary.Files != 13 || m.Summary.Tiers.Full.Files != 0 || m.Summary.BackedUpBytes != 0 {
		t.Fatalf("summary %+v", m.Summary)
	}
	// A disabled source is not in the export scope; the items stay.
	disabled := false
	if _, err := e.cat.Update(e.ctx, e.src.ID, catalogInput(e, &disabled)); err != nil {
		t.Fatal(err)
	}
	m = e.build(nil)
	if len(m.OtherFiles) != 0 || m.Summary.Items != 7 {
		t.Fatalf("with the source disabled: %d other files, summary %+v", len(m.OtherFiles), m.Summary)
	}
}

func TestBuildTiersKeptAndLeftOut(t *testing.T) {
	// Design D9 and S20: a skip *arr file is listed; a skip extra without a record is left out
	// and counted; a kept skip extra is listed; a manifest-tier file is listed.
	e := newEnv(t)
	his := "movies/His Girl Friday (1940)/His Girl Friday (1940) [Bluray-1080p].mkv"
	e.tiers.set(his, TierSkip)
	e.tiers.set(sidecarFile, TierSkip)
	e.tiers.set(posterFile, TierSkip)
	e.tiers.set(otherFile, TierManifest)
	p := e.catalogFile(posterFile)
	e.addRecord(posterFile, p.Size, p.MtimeNs, "present", 0)
	d := e.dest
	m := e.build(&d)

	f := itemByTitle(t, m, "His Girl Friday").Files[0]
	if f.Tier == nil || *f.Tier != TierSkip || f.Rule.ID != 7 || f.Rule.Name != "test rule" || *f.Kept || *f.BackedUp {
		t.Fatalf("skip *arr file %+v", f)
	}
	notld := itemByTitle(t, m, "Night of the Living Dead")
	if len(notld.ExtraFiles) != 1 || notld.ExtraFiles[0].RelPath != posterFile || *notld.ExtraFiles[0].Tier != TierSkip ||
		!*notld.ExtraFiles[0].Kept || !*notld.ExtraFiles[0].BackedUp || notld.SkippedFiles != 1 {
		t.Fatalf("movie 1 extras %+v, skipped %d", notld.ExtraFiles, notld.SkippedFiles)
	}
	if len(m.OtherFiles) != 1 || *m.OtherFiles[0].Tier != TierManifest || *m.OtherFiles[0].Kept {
		t.Fatalf("other files %+v", m.OtherFiles)
	}
	s := m.Summary
	if s.LeftOut != 1 || s.KeptFiles != 1 || s.Tiers.Skip.Files != 2 || s.Tiers.Manifest.Files != 1 || s.Files != 12 {
		t.Fatalf("summary %+v", s)
	}
}

func TestBuildBackedUp(t *testing.T) {
	// backedUp needs a live record with the file's size and mtime: a present record, or a linked
	// or link_recorded one whose primary is present or linked (a missing primary does not count).
	e := newEnv(t)
	rels := []string{
		"movies/His Girl Friday (1940)/His Girl Friday (1940) [Bluray-1080p].mkv",
		"movies/Charade (1963)/Charade (1963) [Bluray-1080p].mkv",
		movieFile,
	}
	files := map[string][2]int64{}
	for _, rel := range rels {
		c := e.catalogFile(rel)
		files[rel] = [2]int64{c.Size, c.MtimeNs}
	}
	// His Girl Friday: link_recorded to a present primary (Charade's content, same size and
	// mtime here) → backed up. Charade: linked to a missing primary → not backed up.
	primary := e.addRecord("primary.mkv", files[rels[0]][0], files[rels[0]][1], "present", 0)
	e.addRecord(rels[0], files[rels[0]][0], files[rels[0]][1], "link_recorded", primary)
	missing := e.addRecord("gone-primary.mkv", files[rels[1]][0], files[rels[1]][1], "missing", 0)
	e.addRecord(rels[1], files[rels[1]][0], files[rels[1]][1], "linked", missing)
	// Night of the Living Dead: present but with another mtime (the source changed since) → not
	// backed up.
	e.addRecord(rels[2], files[rels[2]][0], files[rels[2]][1]+1, "present", 0)
	d := e.dest
	m := e.build(&d)
	for title, want := range map[string]bool{"His Girl Friday": true, "Charade": false, "Night of the Living Dead": false} {
		if f := itemByTitle(t, m, title).Files[0]; *f.BackedUp != want {
			t.Errorf("%s: backedUp %v, want %v", title, *f.BackedUp, want)
		}
	}
	// The records of files that are not in the catalog (primary.mkv, gone-primary.mkv) are
	// listed as other files: the destination holds them (S20).
	var others []string
	for _, x := range m.OtherFiles {
		others = append(others, x.RelPath)
		if x.RelPath == "primary.mkv" && (x.Tier != nil || !*x.BackedUp) {
			t.Fatalf("orphan record %+v", x)
		}
		if x.RelPath == "gone-primary.mkv" && *x.BackedUp {
			t.Fatalf("missing orphan record %+v", x)
		}
	}
	if strings.Join(others, ",") != "gone-primary.mkv,home videos/birthday.mp4,primary.mkv" {
		t.Fatalf("other files %v", others)
	}
}

func TestBuildStaleAndSecrets(t *testing.T) {
	e := newEnv(t)
	// An index error that names the integration's address is scrubbed; no key or URL appears.
	u := e.radarr.URL
	host := strings.TrimPrefix(u, "http://")
	if err := e.db.Write(e.ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(e.ctx, `UPDATE index_state SET error = ? WHERE integration_id = ?`,
			"GET "+u+"/api/v3/movie: dial tcp "+host+": connection refused", e.rad.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(25 * time.Hour) // staleAfterHours is 24
	d := e.dest
	m := e.build(&d)
	if len(m.StaleIntegrations()) != 2 || m.Integrations[0].Fresh {
		t.Fatalf("stale %+v", m.Integrations)
	}
	if m.Integrations[0].LastError == nil || strings.Contains(*m.Integrations[0].LastError, "127.0.0.1") {
		t.Fatalf("lastError %v", m.Integrations[0].LastError)
	}
	data, _ := EncodeJSON(m)
	csv, _ := EncodeCSV(m)
	for _, s := range []string{arrKey, u, host, e.sonarr.URL, "127.0.0.1"} {
		if bytes.Contains(data, []byte(s)) || bytes.Contains(csv, []byte(s)) {
			t.Fatalf("the manifest contains %q", s)
		}
	}
	// Going stale changes the content hash (status and fresh are in it).
	e.clock.Advance(-25 * time.Hour)
	fresh := e.build(&d)
	h1, _ := ContentHash(fresh)
	h2, _ := ContentHash(m)
	if h1 == h2 {
		t.Fatal("a cache going stale did not change the content hash")
	}
}

// renameTiers renames a catalog file and its *arr file (as a concurrent scan and refresh would)
// while the build's read transaction is open.
type renameTiers struct {
	e    *testEnv
	from string
	to   string
	done bool
}

func (r *renameTiers) Decide(ctx context.Context, tr TierRead, destinationID, sourceID int64) (func(int64) Decision, error) {
	if !r.done {
		r.done = true
		local := filepath.Join(r.e.root, filepath.FromSlash(r.to))
		err := r.e.db.Write(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `UPDATE catalog_files SET rel_path = ? WHERE source_id = ? AND rel_path = ?`, r.to, sourceID, r.from); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `UPDATE arr_files SET local_path = ?, rel_path = ? WHERE rel_path = ?`, local, r.to, r.from)
			return err
		})
		if err != nil {
			return nil, err
		}
	}
	return AllFull{}.Decide(ctx, tr, destinationID, sourceID)
}

func TestBuildReadsOneSnapshot(t *testing.T) {
	// A file renamed while the manifest is built appears exactly once, as it was when the build's
	// read transaction began (design §11.2 step 2).
	e := newEnv(t)
	renamed := movieDir + "/renamed.mkv"
	e.tiers = nil
	r := &renameTiers{e: e, from: movieFile, to: renamed}
	b, err := NewBuilder(BuilderOptions{DB: e.db, Catalog: e.cat, Integrations: e.ints, Index: e.index.Store(), Tiers: r, Now: e.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	d := e.dest
	count := func(m *Manifest, rel string) int {
		n := 0
		for _, it := range m.Items {
			for _, f := range it.Files {
				if f.Source != nil && f.Source.RelPath == rel {
					n++
				}
			}
			for _, x := range it.ExtraFiles {
				if x.RelPath == rel {
					n++
				}
			}
		}
		for _, x := range m.OtherFiles {
			if x.RelPath == rel {
				n++
			}
		}
		return n
	}
	m, err := b.Build(e.ctx, BuildScope{Destination: &d})
	if err != nil {
		t.Fatal(err)
	}
	if !r.done || count(m, movieFile) != 1 || count(m, renamed) != 0 {
		t.Fatalf("during the rename: old %d, new %d", count(m, movieFile), count(m, renamed))
	}
	m, err = b.Build(e.ctx, BuildScope{Destination: &d})
	if err != nil {
		t.Fatal(err)
	}
	if count(m, movieFile) != 0 || count(m, renamed) != 1 {
		t.Fatalf("after the rename: old %d, new %d", count(m, movieFile), count(m, renamed))
	}
	if f := itemByTitle(t, m, "Night of the Living Dead").Files[0]; f.Source == nil || f.Source.RelPath != renamed {
		t.Fatalf("the renamed *arr file %+v", f)
	}
}

func TestBuildWithoutArrIntegrations(t *testing.T) {
	// A Plex-only install: no items, every file an other file; a disabled *arr lists nothing.
	e := newEnv(t)
	for _, it := range []integrations.Integration{e.rad, e.son} {
		off := false
		if _, err := e.ints.Update(e.ctx, it.ID, integrations.Input{Name: it.Name, URL: it.URL, Enabled: &off}); err != nil {
			t.Fatal(err)
		}
	}
	d := e.dest
	m := e.build(&d)
	if len(m.Items) != 0 || len(m.Integrations) != 0 || len(m.OtherFiles) != 12 || m.Summary.Files != 12 {
		t.Fatalf("%d items, %d integrations, %d other files", len(m.Items), len(m.Integrations), len(m.OtherFiles))
	}
	// A destination without sources lists items only.
	if _, err := e.dests.Update(e.ctx, d.ID, destinationsInput([]int64{})); err != nil {
		t.Fatal(err)
	}
	d, _ = e.dests.Get(e.ctx, d.ID)
	m = e.build(&d)
	if len(m.OtherFiles) != 0 || len(m.Sources) != 0 {
		t.Fatalf("no sources: %+v", m.Sources)
	}
	raw, _ := json.Marshal(m)
	if !json.Valid(raw) {
		t.Fatal("invalid JSON")
	}
}

func destinationsInput(sources []int64) destinations.Input {
	return destinations.Input{SourceIDs: sources}
}

func catalogInput(e *testEnv, enabled *bool) catalog.SourceInput {
	return catalog.SourceInput{Name: e.src.Name, Path: e.src.Path, Enabled: enabled}
}

func TestRoundTripLidarr(t *testing.T) {
	e := newEnv(t)
	lidarr := arrtest.NewServer(t, arr.KindLidarr, arrKey)
	for p, size := range fixtureFiles(t, arr.KindLidarr, "") {
		e.writeFile(strings.TrimPrefix(p, "/"), size)
	}
	e.scan()
	lid := e.createArr(integrations.TypeLidarr, "Lidarr", lidarr.URL, map[string]string{"/music": filepath.Join(e.root, "music")})
	e.refresh(lid.ID)
	raw, err := manifesttest.LivePlan("lidarr", lid.ID, manifesttest.HTTPGetter(lidarr.URL, "/api/v1", arrKey))
	if err != nil {
		t.Fatal(err)
	}
	var live Plan
	if err := json.Unmarshal(raw, &live); err != nil {
		t.Fatal(err)
	}
	d := e.dest
	m := e.build(&d)
	all := e.livePlans()
	all[lid.ID] = live
	requireRoundTrip(t, m, all)
	artist := itemByTitle(t, m, "Scott Joplin")
	if artist.Kind != "artist" || artist.MetadataProfile == "" || len(artist.Detail.Albums) != 4 || len(artist.Files) != 19 ||
		artist.Files[0].AlbumID != 13 || artist.Files[0].Source == nil {
		t.Fatalf("artist %+v", artist)
	}
}

// insertRecord inserts a present destination_files row of the destination for source src at
// "<src.DestFolder>/<rel>" and returns its id.
func (e *testEnv) insertRecord(src catalog.Source, rel string, size int64) int64 {
	e.t.Helper()
	var id int64
	err := e.db.Write(e.ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(e.ctx, `INSERT INTO destination_files (destination_id, source_id, rel_path, source_rel_path, size,
			mtime_ns, hash, state, copied_at) VALUES (?, ?, ?, ?, ?, ?, ?, 'present', ?)`,
			e.dest.ID, src.ID, src.DestFolder+"/"+rel, rel, size, int64(1), "sha256:"+strings.Repeat("cd", 32), db.FormatTime(e.clock.Now()))
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func TestBuildListsEveryRecordOfDeletedSources(t *testing.T) {
	// Two sources that both held "Avatar (2009)/Avatar (2009).mkv" were backed up to the
	// destination and then deleted: their records lose the source (ON DELETE SET NULL) but stay
	// at the destination. Both are listed, each at its path at the destination (S20), and the
	// summary counts both.
	e := newEnv(t)
	const rel = "Avatar (2009)/Avatar (2009).mkv"
	var srcs []catalog.Source
	for i, name := range []string{"Movies", "Movies 4K"} {
		dir := filepath.Join(e.base, "gone", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		src, err := e.cat.Create(e.ctx, catalog.SourceInput{Name: name, Path: dir})
		if err != nil {
			t.Fatal(err)
		}
		srcs = append(srcs, src)
		e.insertRecord(src, rel, int64(1000*(i+1)))
	}
	for _, s := range srcs {
		if err := e.cat.Delete(e.ctx, s.ID); err != nil {
			t.Fatal(err)
		}
	}
	d := e.dest
	m := e.build(&d)
	got := map[string]int64{}
	for _, x := range m.OtherFiles {
		if x.Source == nil {
			got[x.RelPath] = x.Size
			if !*x.BackedUp || x.Tier != nil || *x.Kept {
				t.Errorf("orphan record %+v", x)
			}
		}
	}
	want := map[string]int64{srcs[0].DestFolder + "/" + rel: 1000, srcs[1].DestFolder + "/" + rel: 2000}
	if len(got) != len(want) || got[srcs[0].DestFolder+"/"+rel] != 1000 || got[srcs[1].DestFolder+"/"+rel] != 2000 {
		t.Fatalf("records of deleted sources listed %v, want %v", got, want)
	}
	// Every listed file is counted once: the orphans add two files and 3000 bytes to the
	// destination's other listings.
	var files, bytes int64
	for _, it := range m.Items {
		for _, f := range it.Files {
			files, bytes = files+1, bytes+f.Size
		}
		for _, f := range it.ExtraFiles {
			files, bytes = files+1, bytes+f.Size
		}
	}
	for _, f := range m.OtherFiles {
		files, bytes = files+1, bytes+f.Size
	}
	if m.Summary.Files != files || m.Summary.Bytes != bytes {
		t.Fatalf("summary %d files %d bytes, listed %d files %d bytes", m.Summary.Files, m.Summary.Bytes, files, bytes)
	}
}

func TestBuildListsASecondRecordOfOneSourceFile(t *testing.T) {
	// Two live records of one source file (the source file's record, and one at another path of
	// the destination) are both listed: the second at its path at the destination.
	e := newEnv(t)
	c := e.catalogFile(otherFile)
	e.addRecord(otherFile, c.Size, c.MtimeNs, "present", 0)
	err := e.db.Write(e.ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(e.ctx, `INSERT INTO destination_files (destination_id, source_id, rel_path, source_rel_path, size,
			mtime_ns, state, copied_at) VALUES (?, ?, ?, ?, ?, ?, 'present', ?)`,
			e.dest.ID, e.src.ID, "old folder/"+otherFile, otherFile, c.Size, c.MtimeNs, db.FormatTime(e.clock.Now()))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	d := e.dest
	m := e.build(&d)
	var listed []string
	for _, x := range m.OtherFiles {
		src := int64(0)
		if x.Source != nil {
			src = *x.Source
		}
		listed = append(listed, fmt.Sprintf("%d:%s", src, x.RelPath))
	}
	want := fmt.Sprintf("0:old folder/%s,%d:%s", otherFile, e.src.ID, otherFile)
	if strings.Join(listed, ",") != want {
		t.Fatalf("other files %v, want %s", listed, want)
	}
}
