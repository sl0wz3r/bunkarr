package manifest

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr/arrtest"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/manifest/manifesttest"
)

// Lidarr in manifests (design D2, §11.1): an artist item with its metadata profile and albums,
// track files with their album, rows of manifest.csv with the artist's MusicBrainz id.

const (
	// lidarrMBID is the recorded artist's MusicBrainz id (Lidarr's foreignArtistId).
	lidarrMBID = "aec8a328-d2e8-4780-b2ea-318c7f8d6f75"
	// lidarrCover is an album cover in the artist's folder: no track file claims it.
	lidarrCover = "music/Scott Joplin/Ragtime (1994)/cover.jpg"
)

// addLidarr adds the recorded Lidarr to the environment: its track files (and an album cover)
// under <root>/music, an integration mapping /music there, and a refreshed index.
func (e *testEnv) addLidarr() (*arrtest.Server, integrations.Integration) {
	e.t.Helper()
	srv := arrtest.NewServer(e.t, arr.KindLidarr, arrKey)
	for p, size := range fixtureFiles(e.t, arr.KindLidarr, "") {
		e.writeFile(strings.TrimPrefix(p, "/"), size)
	}
	e.writeFile(lidarrCover, 7000)
	e.scan()
	it := e.createArr(integrations.TypeLidarr, "Lidarr", srv.URL, map[string]string{"/music": filepath.Join(e.root, "music")})
	e.refresh(it.ID)
	return srv, it
}

// lidarrLive decodes the fake Lidarr's state from its raw API JSON.
func lidarrLive(t *testing.T, srv *arrtest.Server, id int64) Plan {
	t.Helper()
	raw, err := manifesttest.LivePlan("lidarr", id, manifesttest.HTTPGetter(srv.URL, "/api/v1", arrKey))
	if err != nil {
		t.Fatal(err)
	}
	var p Plan
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLidarrManifestItem(t *testing.T) {
	e := newEnv(t)
	srv, lid := e.addLidarr()
	e.recordAll()
	d := e.dest
	m := e.build(&d)
	var integ *Integration
	for i := range m.Integrations {
		if m.Integrations[i].ID == lid.ID {
			integ = &m.Integrations[i]
		}
	}
	if integ == nil || integ.Type != "lidarr" || !integ.Fresh || len(integ.MetadataProfiles) != 2 || len(integ.RootFolders) != 1 ||
		integ.RootFolders[0].Path != "/music" || integ.RootFolders[0].SourceID == nil {
		t.Fatalf("integration %+v", integ)
	}
	a := itemByTitle(t, m, "Scott Joplin")
	albums := map[int64]Album{}
	for _, al := range a.Detail.Albums {
		albums[al.ID] = al
	}
	if a.Kind != "artist" || a.IntegrationID != lid.ID || a.ArrID != 1 || a.ExternalIDs != (ExternalIDs{MBID: lidarrMBID}) || a.Year != 0 ||
		!a.Located || a.Path != "/music/Scott Joplin" || a.RootFolder != "/music" || a.QualityProfile != "Any" ||
		a.MetadataProfile != "Standard" || !a.Monitored || !slices.Equal(a.Tags, []string{"bunkarr-full"}) || len(a.Genres) == 0 ||
		a.Detail.MonitorNewItems != "all" || len(albums) != 4 || albums[46].Title != "Ragtime" ||
		albums[46].MBID != "bc350371-b6c2-46ab-86dd-57732fb052ee" || !albums[46].Monitored || len(a.Files) != 19 {
		t.Fatalf("artist %+v", a)
	}
	for _, f := range a.Files {
		album, ok := albums[f.AlbumID]
		if !ok || f.Episodes != nil || f.Source == nil || f.Source.RelPath != "music/Scott Joplin/"+f.RelativePath ||
			!strings.HasPrefix(f.RelativePath, album.Title+" (") || f.Path != a.Path+"/"+f.RelativePath ||
			f.Tier == nil || *f.Tier != TierFull || f.BackedUp == nil || !*f.BackedUp || f.SHA256 == nil || f.Quality == "" {
			t.Fatalf("track file %+v", f)
		}
	}
	// The album cover is attributed to the artist by folder, not listed as an other file.
	if len(a.ExtraFiles) != 1 || a.ExtraFiles[0].RelPath != lidarrCover {
		t.Fatalf("extras %+v", a.ExtraFiles)
	}
	for _, o := range m.OtherFiles {
		if strings.HasPrefix(o.RelPath, "music/") {
			t.Fatalf("other file %+v", o)
		}
	}
	live := e.livePlans()
	live[lid.ID] = lidarrLive(t, srv, lid.ID)
	requireRoundTrip(t, m, live)
}

func TestLidarrManifestCSV(t *testing.T) {
	e := newEnv(t)
	_, lid := e.addLidarr()
	d := e.dest
	m := e.build(&d)
	data, err := EncodeCSV(m)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(bytes.NewReader(data)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	col := map[string]int{}
	for i, h := range rows[0] {
		col[h] = i
	}
	var tracks, extras int
	for _, r := range rows[1:] {
		if r[col["kind"]] != "artist" {
			continue
		}
		if r[col["integration"]] != lid.Name || r[col["arrId"]] != "1" || r[col["title"]] != "Scott Joplin" || r[col["mbid"]] != lidarrMBID ||
			r[col["year"]] != "" || r[col["tmdbId"]] != "" || r[col["tvdbId"]] != "" || r[col["imdbId"]] != "" || r[col["season"]] != "" ||
			r[col["episode"]] != "" || r[col["qualityProfile"]] != "Any" || r[col["rootFolder"]] != "/music" || r[col["monitored"]] != "true" ||
			r[col["tags"]] != "bunkarr-full" || r[col["located"]] != "true" || r[col["source"]] != "Media" || r[col["tier"]] != "full" {
			t.Fatalf("artist row %q", r)
		}
		switch r[col["arrPath"]] {
		case "":
			extras++
			if r[col["relPath"]] != lidarrCover || r[col["quality"]] != "" {
				t.Fatalf("extra row %q", r)
			}
		default:
			tracks++
			if r[col["relPath"]] != strings.TrimPrefix(r[col["arrPath"]], "/") || r[col["quality"]] == "" || r[col["size"]] == "" {
				t.Fatalf("track row %q", r)
			}
		}
	}
	if tracks != 19 || extras != 1 {
		t.Fatalf("%d track rows, %d extra rows", tracks, extras)
	}
}

// TestLidarrComparePlanFindsChanges: what a restore would send to Lidarr (the metadata profile,
// each album's monitored state, the track files) is compared, and every difference is reported.
func TestLidarrComparePlanFindsChanges(t *testing.T) {
	e := newEnv(t)
	srv, lid := e.addLidarr()
	d := e.dest
	m := e.build(&d)
	plan := m.ReimportPlan().ForIntegration(lid.ID)
	if diffs := ComparePlan(plan, lidarrLive(t, srv, lid.ID)); len(diffs) != 0 {
		t.Fatalf("unchanged Lidarr: %v", diffs)
	}
	edit := func(target, fixture string, fn func([]map[string]any)) {
		t.Helper()
		var list []map[string]any
		if err := json.Unmarshal(arrtest.Fixture(t, arr.KindLidarr, fixture), &list); err != nil {
			t.Fatal(err)
		}
		fn(list)
		b, _ := json.Marshal(list)
		srv.SetJSON(http.MethodGet, target, b)
	}
	edit("artist", "artist.json", func(l []map[string]any) { l[0]["metadataProfileId"] = 2 })
	edit("album?artistId=1", "album-artistId-1.json", func(l []map[string]any) {
		for _, al := range l {
			if al["id"] == 1.0 {
				al["monitored"] = false
			}
		}
	})
	var renamed string
	edit("trackfile?artistId=1", "trackfile-artistId-1.json", func(l []map[string]any) {
		l[0]["size"] = 1.0
		renamed = l[1]["path"].(string)
		l[1]["path"] = path.Dir(renamed) + "/renamed.mp3"
	})
	fields := map[string]bool{}
	for _, d := range ComparePlan(plan, lidarrLive(t, srv, lid.ID)) {
		if !strings.Contains(d.Item, "artist mbid:"+lidarrMBID) {
			t.Fatalf("difference %s", d)
		}
		fields[d.Field] = true
	}
	rel := strings.TrimPrefix(renamed, "/music/Scott Joplin/")
	for _, want := range []string{"metadataProfile", "detail.albums", "files[" + rel + "]", "files[" + path.Dir(rel) + "/renamed.mp3]"} {
		if !fields[want] {
			t.Errorf("no difference in %s: %v", want, fields)
		}
	}
	sized := false
	for f := range fields {
		sized = sized || strings.HasSuffix(f, "].size")
	}
	if !sized {
		t.Errorf("the size change was not reported: %v", fields)
	}
}

// TestLidarrUnlocatedArtist: an artist whose folder maps into no source is still listed, with
// every track file and no source, and the export warns about it.
func TestLidarrUnlocatedArtist(t *testing.T) {
	e := newEnv(t)
	srv, lid := e.addLidarr()
	lid = e.setMappings(lid, map[string]string{})
	e.refresh(lid.ID)
	d := e.dest
	m := e.build(&d)
	a := itemByTitle(t, m, "Scott Joplin")
	if a.Located || len(a.Files) != 19 || len(a.Detail.Albums) != 4 {
		t.Fatalf("artist %+v", a)
	}
	for _, f := range a.Files {
		if f.Source != nil || f.Tier != nil || f.BackedUp == nil || *f.BackedUp {
			t.Fatalf("track file %+v", f)
		}
	}
	if m.Summary.UnlocatedItems < 1 {
		t.Fatalf("summary %+v", m.Summary)
	}
	if diffs := ComparePlan(m.ReimportPlan().ForIntegration(lid.ID), lidarrLive(t, srv, lid.ID)); len(diffs) != 0 {
		t.Fatalf("an unlocated artist does not round-trip: %v", diffs)
	}
}

// TestCrashMatrixLidarr: a manifest export with a Lidarr, crashed at every step and resumed,
// leaves one intact version whose re-import plan still matches every *arr, Lidarr included.
func TestCrashMatrixLidarr(t *testing.T) {
	for _, point := range crashPoints {
		t.Run(point, func(t *testing.T) {
			e := newEnv(t)
			srv, lid := e.addLidarr()
			job := e.newJob(false)
			e.crashRun(job, point)
			job.Attempt, job.Trigger = 2, jobs.TriggerResume
			e.clock.Advance(time.Minute)
			if _, rep, err := e.run(job); err != nil {
				t.Fatalf("resume: %v\n%s", err, rep)
			}
			vs := e.assertConverged(1)
			m, err := ParseDir(filepath.Join(e.target, filepath.FromSlash(vs[0].Path)))
			if err != nil {
				t.Fatal(err)
			}
			live := e.livePlans()
			live[lid.ID] = lidarrLive(t, srv, lid.ID)
			requireRoundTrip(t, m, live)
		})
	}
}
