package arr_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/integrations/arr"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr/arrtest"
)

// TestRadarrFixturesDecode decodes the recorded Radarr responses with their quirks.
func TestRadarrFixturesDecode(t *testing.T) {
	ctx := context.Background()
	srv := arrtest.NewServer(t, arr.KindRadarr, testKey)
	c := newClient(t, arr.KindRadarr, srv.URL, arr.Options{})

	movies, err := c.Movies(ctx)
	if err != nil || len(movies) != 4 {
		t.Fatalf("Movies = %d, %v", len(movies), err)
	}
	m1 := movies[0]
	if m1.ID != 1 || m1.Title != "Night of the Living Dead" || m1.Year != 1968 || m1.TMDBID != 10331 || m1.IMDBID != "tt0063350" ||
		m1.Path != "/movies/Night of the Living Dead (1968)" || m1.RootFolderPath != "/movies" || m1.QualityProfileID != 6 ||
		!m1.Monitored || !slices.Equal(m1.Tags, []int64{1}) || m1.MinimumAvailability != "released" || len(m1.Genres) == 0 ||
		!m1.Added.Equal(time.Date(2026, 9, 25, 12, 26, 17, 0, time.UTC)) {
		t.Fatalf("movie 1 = %+v", m1)
	}
	// folderName holds the full path, like path.
	if m1.FolderName != m1.Path {
		t.Fatalf("folderName %q, path %q", m1.FolderName, m1.Path)
	}
	// The list holds the nested movie file; movie 1's is file 4 (an upgrade replaced file 2).
	if !m1.HasFile || m1.MovieFileID != 4 || m1.MovieFile == nil || m1.MovieFile.ID != 4 || m1.MovieFile.MovieID != 1 ||
		m1.MovieFile.Size != 3412521 || m1.MovieFile.Quality.Name != "Bluray-1080p" ||
		m1.MovieFile.RelativePath != "Night of the Living Dead (1968) [Bluray-1080p].mkv" ||
		m1.MovieFile.Path != m1.Path+"/"+m1.MovieFile.RelativePath {
		t.Fatalf("movie 1 file = %+v", m1.MovieFile)
	}
	// A movie without a file: movieFileId 0 and no movieFile.
	m4 := movies[3]
	if m4.ID != 4 || m4.HasFile || m4.MovieFileID != 0 || m4.MovieFile != nil || m4.Monitored || m4.QualityProfileID != 4 ||
		!slices.Equal(m4.Tags, []int64{1, 2}) {
		t.Fatalf("movie 4 = %+v", m4)
	}
	if got := []int64{movies[1].MovieFileID, movies[2].MovieFileID}; !slices.Equal(got, []int64{1, 3}) {
		t.Fatalf("file ids of movies 2 and 3 = %v", got)
	}

	one, err := c.Movie(ctx, 1)
	if err != nil || !reflect.DeepEqual(one, m1) {
		t.Fatalf("Movie(1) = %+v, %v; want the list entry", one, err)
	}
	files, err := c.MovieFiles(ctx, 1)
	if err != nil || len(files) != 1 || files[0] != *m1.MovieFile {
		t.Fatalf("MovieFiles(1) = %+v, %v; want the nested file", files, err)
	}

	profiles, err := c.QualityProfiles(ctx)
	if err != nil || len(profiles) != 6 || profiles[5] != (arr.QualityProfile{ID: 6, Name: "HD - 720p/1080p", Cutoff: 7, UpgradeAllowed: true}) {
		t.Fatalf("QualityProfiles = %+v, %v", profiles, err)
	}
	tags, err := c.Tags(ctx)
	if err != nil || !slices.Equal(tags, []arr.Tag{{ID: 1, Label: "bunkarr-full"}, {ID: 2, Label: "irreplaceable"}}) {
		t.Fatalf("Tags = %+v, %v", tags, err)
	}
	roots, err := c.RootFolders(ctx)
	if err != nil || len(roots) != 1 || roots[0].ID != 1 || roots[0].Path != "/movies" || !roots[0].Accessible || roots[0].FreeSpace == 0 {
		t.Fatalf("RootFolders = %+v, %v", roots, err)
	}
	mm, err := c.MediaManagement(ctx)
	if err != nil || mm != (arr.MediaManagement{RecycleBin: "", RecycleBinCleanupDays: 7, FileDate: "none"}) {
		t.Fatalf("MediaManagement = %+v, %v", mm, err)
	}
}

// TestRadarrMovies4K decodes the recording with a second root folder that holds one movie.
func TestRadarrMovies4K(t *testing.T) {
	ctx := context.Background()
	srv := arrtest.NewServer(t, arr.KindRadarr, testKey)
	srv.UseMovies4K()
	c := newClient(t, arr.KindRadarr, srv.URL, arr.Options{})
	roots, err := c.RootFolders(ctx)
	if err != nil || len(roots) != 2 || roots[1].Path != "/movies-4k" || roots[1].ID != 2 {
		t.Fatalf("RootFolders = %+v, %v", roots, err)
	}
	movies, err := c.Movies(ctx)
	if err != nil || len(movies) != 5 {
		t.Fatalf("Movies = %d, %v", len(movies), err)
	}
	m5 := movies[4]
	if m5.ID != 5 || m5.Title != "Nosferatu" || m5.Year != 1922 || m5.RootFolderPath != "/movies-4k" ||
		m5.Path != "/movies-4k/Nosferatu (1922)" || !m5.HasFile || m5.MovieFileID != 5 || m5.MovieFile == nil ||
		m5.MovieFile.ID != 5 || m5.MovieFile.Quality.Name != "Bluray-2160p" || m5.QualityProfileID != 5 {
		t.Fatalf("movie 5 = %+v", m5)
	}
	one, err := c.Movie(ctx, 5)
	if err != nil || !reflect.DeepEqual(one, m5) {
		t.Fatalf("Movie(5) = %+v, %v", one, err)
	}
	files, err := c.MovieFiles(ctx, 5)
	if err != nil || len(files) != 1 || files[0] != *m5.MovieFile {
		t.Fatalf("MovieFiles(5) = %+v, %v", files, err)
	}
	srv.SetRootFolderAccessible("/movies-4k", false)
	roots, err = c.RootFolders(ctx)
	if err != nil || !roots[0].Accessible || roots[1].Accessible {
		t.Fatalf("after SetRootFolderAccessible: %+v, %v", roots, err)
	}
}

// TestSonarrFixturesDecode decodes the recorded Sonarr responses, including the mapping of
// episode files to episodes through episodeFileId (file 7 holds S01E04 and S01E05).
func TestSonarrFixturesDecode(t *testing.T) {
	ctx := context.Background()
	srv := arrtest.NewServer(t, arr.KindSonarr, testKey)
	c := newClient(t, arr.KindSonarr, srv.URL, arr.Options{})

	series, err := c.AllSeries(ctx)
	if err != nil || len(series) != 2 {
		t.Fatalf("AllSeries = %d, %v", len(series), err)
	}
	s1 := series[0]
	if s1.ID != 1 || s1.Title != "The Beverly Hillbillies" || s1.TVDBID != 71471 || s1.TMDBID != 1930 || s1.IMDBID != "tt0055662" ||
		s1.TVMazeID != 2139 || series[1].TVMazeID != 18552 ||
		s1.Path != "/tv/The Beverly Hillbillies" || s1.RootFolderPath != "/tv" || s1.QualityProfileID != 6 || s1.LanguageProfileID != 1 ||
		!s1.Monitored || s1.MonitorNewItems != "all" || s1.SeriesType != "standard" || !s1.SeasonFolder || s1.UseSceneNumbering ||
		!slices.Equal(s1.Tags, []int64{1}) || len(s1.Seasons) != 10 || s1.Seasons[0] != (arr.Season{SeasonNumber: 0, Monitored: false}) ||
		s1.Statistics.EpisodeFileCount != 6 || s1.Statistics.SizeOnDisk != 4729207 {
		t.Fatalf("series 1 = %+v", s1)
	}
	for id, want := range map[int64]arr.Series{1: series[0], 2: series[1]} {
		got, err := c.Series(ctx, id)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("Series(%d) = %+v, %v; want the list entry", id, got, err)
		}
	}

	files, err := c.EpisodeFiles(ctx, 1)
	if err != nil || len(files) != 5 {
		t.Fatalf("EpisodeFiles(1) = %+v, %v", files, err)
	}
	episodes, err := c.Episodes(ctx, 1)
	if err != nil || len(episodes) != 286 {
		t.Fatalf("Episodes(1) = %d, %v", len(episodes), err)
	}
	seasons := map[int]int{}
	byFile := map[int64][][2]int{}
	monitored := 0
	for _, e := range episodes {
		seasons[e.SeasonNumber]++
		if e.Monitored {
			monitored++
		}
		if e.SeriesID != 1 || e.TVDBID == 0 {
			t.Fatalf("episode %+v", e)
		}
		if e.EpisodeFileID != 0 {
			if !e.HasFile {
				t.Fatalf("episode %d has file %d but hasFile false", e.ID, e.EpisodeFileID)
			}
			byFile[e.EpisodeFileID] = append(byFile[e.EpisodeFileID], [2]int{e.SeasonNumber, e.EpisodeNumber})
		}
	}
	if len(seasons) != 10 || seasons[0] != 12 || seasons[1] != 36 || monitored != 274 {
		t.Fatalf("seasons %v, monitored %d", seasons, monitored)
	}
	want := map[int64][][2]int{4: {{1, 3}}, 5: {{1, 1}}, 6: {{1, 2}}, 7: {{1, 4}, {1, 5}}, 8: {{1, 6}}}
	if !reflect.DeepEqual(byFile, want) {
		t.Fatalf("episodes by file = %v, want %v", byFile, want)
	}
	for _, f := range files {
		if _, ok := want[f.ID]; !ok || f.SeriesID != 1 || f.SeasonNumber != 1 || f.Quality.Name == "" ||
			f.Path != s1.Path+"/"+f.RelativePath || !strings.HasPrefix(f.RelativePath, "Season 1/") {
			t.Fatalf("file %+v", f)
		}
	}

	files2, err := c.EpisodeFiles(ctx, 2)
	if err != nil || len(files2) != 1 || files2[0].ID != 3 {
		t.Fatalf("EpisodeFiles(2) = %+v, %v", files2, err)
	}
	ep2, err := c.Episodes(ctx, 2)
	if err != nil || len(ep2) != 102 {
		t.Fatalf("Episodes(2) = %d, %v", len(ep2), err)
	}
	var withFile []arr.Episode
	for _, e := range ep2 {
		if e.EpisodeFileID != 0 {
			withFile = append(withFile, e)
		}
	}
	if len(withFile) != 1 || withFile[0].EpisodeFileID != 3 || withFile[0].SeasonNumber != 1 || withFile[0].EpisodeNumber != 1 {
		t.Fatalf("series 2 episodes with a file = %+v", withFile)
	}

	roots, err := c.RootFolders(ctx)
	if err != nil || len(roots) != 1 || roots[0].Path != "/tv" {
		t.Fatalf("RootFolders = %+v, %v", roots, err)
	}
	backups, err := c.Backups(ctx)
	if err != nil || len(backups) != 1 || backups[0].Type != "manual" || arr.ValidateBackup(backups[0]) != nil {
		t.Fatalf("Backups = %+v, %v", backups, err)
	}
}

// TestLidarrFixturesDecode decodes the recorded Lidarr responses: the artist's mbId is null (the
// MusicBrainz id is foreignArtistId), a quality profile's cutoff is a group id, track files have
// no relative path.
func TestLidarrFixturesDecode(t *testing.T) {
	ctx := context.Background()
	srv := arrtest.NewServer(t, arr.KindLidarr, testKey)
	c := newClient(t, arr.KindLidarr, srv.URL, arr.Options{})

	artists, err := c.Artists(ctx)
	if err != nil || len(artists) != 1 {
		t.Fatalf("Artists = %+v, %v", artists, err)
	}
	a := artists[0]
	if a.ID != 1 || a.ArtistName != "Scott Joplin" || a.ForeignArtistID != "aec8a328-d2e8-4780-b2ea-318c7f8d6f75" ||
		a.Path != "/music/Scott Joplin" || a.RootFolderPath != "/music" || a.QualityProfileID != 1 || a.MetadataProfileID != 1 ||
		!a.Monitored || !slices.Equal(a.Tags, []int64{1}) || a.Statistics.TrackFileCount != 19 {
		t.Fatalf("artist = %+v", a)
	}
	one, err := c.Artist(ctx, 1)
	if err != nil || !reflect.DeepEqual(one, a) {
		t.Fatalf("Artist(1) = %+v, %v", one, err)
	}
	tracks, err := c.TrackFiles(ctx, 1)
	if err != nil || len(tracks) != 19 {
		t.Fatalf("TrackFiles = %d, %v", len(tracks), err)
	}
	albums, err := c.Albums(ctx, 1)
	if err != nil || len(albums) != 4 {
		t.Fatalf("Albums = %d, %v", len(albums), err)
	}
	albumIDs := map[int64]bool{}
	for _, al := range albums {
		albumIDs[al.ID] = true
		if al.ArtistID != 1 || al.ForeignAlbumID == "" || al.Title == "" {
			t.Fatalf("album %+v", al)
		}
	}
	for _, tf := range tracks {
		if tf.ArtistID != 1 || !albumIDs[tf.AlbumID] || !strings.HasPrefix(tf.Path, a.Path+"/") || tf.Size == 0 || tf.Quality.Name == "" {
			t.Fatalf("track file %+v", tf)
		}
	}
	profiles, err := c.QualityProfiles(ctx)
	if err != nil || len(profiles) != 3 || profiles[1].Cutoff < 1000 {
		t.Fatalf("QualityProfiles = %+v, %v (a cutoff may be a group id >= 1000)", profiles, err)
	}
	meta, err := c.MetadataProfiles(ctx)
	if err != nil || !slices.Equal(meta, []arr.MetadataProfile{{ID: 1, Name: "Standard"}, {ID: 2, Name: "None"}}) {
		t.Fatalf("MetadataProfiles = %+v, %v", meta, err)
	}
	roots, err := c.RootFolders(ctx)
	if err != nil || len(roots) != 1 || roots[0].Name != "Music" || roots[0].Path != "/music" || !roots[0].Accessible {
		t.Fatalf("RootFolders = %+v, %v", roots, err)
	}
	mm, err := c.MediaManagement(ctx)
	if err != nil || mm.FileDate != "none" || mm.RecycleBin != "" {
		t.Fatalf("MediaManagement = %+v, %v", mm, err)
	}
	st, err := c.Status(ctx)
	if err != nil || st.Version != "3.1.0.4875" {
		t.Fatalf("Status = %+v, %v", st, err)
	}
}

// TestFixtureProvenance checks the joins testdata/arr/record_slice2.py made when it recorded the
// files the fixture spike had not: every derived body agrees with what the spike recorded.
func TestFixtureProvenance(t *testing.T) {
	load := func(kind arr.Kind, name string) any {
		t.Helper()
		var v any
		if err := json.Unmarshal(arrtest.Fixture(t, kind, name), &v); err != nil {
			t.Fatalf("%s/%s: %v", kind, name, err)
		}
		return v
	}
	list := func(kind arr.Kind, name string) []any { return load(kind, name).([]any) }

	// GET <items>/{id} answers exactly the list entry: recorded for all three apps.
	for _, tt := range []struct {
		kind       arr.Kind
		list, item string
		index      int
	}{
		{arr.KindSonarr, "series.json", "series-1.json", 0},
		{arr.KindRadarr, "movie.json", "movie-1.json", 0},
		{arr.KindLidarr, "artist.json", "artist-1.json", 0},
		// derived from it:
		{arr.KindSonarr, "series.json", "series-2.json", 1},
		// recorded in the 4K instance:
		{arr.KindRadarr, "movie-movies-4k.json", "movie-5.json", 4},
	} {
		if !reflect.DeepEqual(list(tt.kind, tt.list)[tt.index], load(tt.kind, tt.item)) {
			t.Errorf("%s/%s differs from entry %d of %s", tt.kind, tt.item, tt.index, tt.list)
		}
	}

	// The all-season episode list holds the spike's season 1 recording unchanged.
	all := list(arr.KindSonarr, "episode-seriesId-1.json")
	season1 := list(arr.KindSonarr, "episode-seriesId-1-season-1.json")
	var got []any
	for _, e := range all {
		if e.(map[string]any)["seasonNumber"].(float64) == 1 {
			got = append(got, e)
		}
	}
	if !reflect.DeepEqual(got, season1) {
		t.Error("season 1 of episode-seriesId-1.json differs from episode-seriesId-1-season-1.json")
	}
	byID := map[float64]any{}
	for _, e := range all {
		byID[e.(map[string]any)["id"].(float64)] = e
	}
	for _, e := range list(arr.KindSonarr, "episode-episodeFileId-7.json") {
		if !reflect.DeepEqual(byID[e.(map[string]any)["id"].(float64)], e) {
			t.Errorf("episode-episodeFileId-7.json entry %v differs from episode-seriesId-1.json", e.(map[string]any)["id"])
		}
	}
	// Every recorded episode file is named by an episode, and no episode names another file.
	for _, tt := range []struct{ files, episodes string }{
		{"episodefile-seriesId-1.json", "episode-seriesId-1.json"},
		{"episodefile-seriesId-2.json", "episode-seriesId-2.json"},
	} {
		fileIDs := map[float64]bool{}
		for _, f := range list(arr.KindSonarr, tt.files) {
			fileIDs[f.(map[string]any)["id"].(float64)] = true
		}
		named := map[float64]bool{}
		for _, e := range list(arr.KindSonarr, tt.episodes) {
			m := e.(map[string]any)
			if id := m["episodeFileId"].(float64); id != 0 {
				named[id] = true
				if !fileIDs[id] || m["hasFile"] != true {
					t.Errorf("%s: episode %v names file %v", tt.episodes, m["id"], id)
				}
			} else if m["hasFile"] != false {
				t.Errorf("%s: episode %v has no file id but hasFile", tt.episodes, m["id"])
			}
		}
		if !reflect.DeepEqual(named, fileIDs) {
			t.Errorf("%s names files %v, %s lists %v", tt.episodes, named, tt.files, fileIDs)
		}
	}
	// The 4K list is the spike's four movies plus movie 5.
	movies4k := list(arr.KindRadarr, "movie-movies-4k.json")
	if len(movies4k) != 5 || !reflect.DeepEqual(movies4k[:4], list(arr.KindRadarr, "movie.json")) {
		t.Error("movie-movies-4k.json does not start with the spike's movie.json")
	}
	m5 := movies4k[4].(map[string]any)
	f5 := list(arr.KindRadarr, "moviefile-movieId-5.json")[0].(map[string]any)
	if m5["movieFileId"].(float64) != 5 || m5["movieFile"].(map[string]any)["id"].(float64) != 5 || f5["id"].(float64) != 5 ||
		f5["path"] != m5["movieFile"].(map[string]any)["path"] {
		t.Error("movie 5 and its file disagree")
	}

	// No recorded body holds an API key.
	for _, kind := range []arr.Kind{arr.KindSonarr, arr.KindRadarr, arr.KindLidarr} {
		entries, err := os.ReadDir(arrtest.FixtureDir(kind))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			b, err := os.ReadFile(filepath.Join(arrtest.FixtureDir(kind), e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(strings.ToLower(string(b)), "apikey") || strings.Contains(string(b), "0123456789abcdef0123456789abcdef") {
				if e.Name() != "notification-schema-webhook.json" && e.Name() != "notification.json" {
					t.Errorf("%s/%s may hold an API key", kind, e.Name())
				}
			}
		}
	}
}

// TestEveryFixtureDecodesAsJSON reads every recorded body.
func TestEveryFixtureDecodesAsJSON(t *testing.T) {
	for _, kind := range []arr.Kind{arr.KindSonarr, arr.KindRadarr, arr.KindLidarr} {
		entries, err := os.ReadDir(arrtest.FixtureDir(kind))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) < 18 {
			t.Fatalf("%s: only %d fixtures", kind, len(entries))
		}
		for _, e := range entries {
			var v any
			if err := json.Unmarshal(arrtest.Fixture(t, kind, e.Name()), &v); err != nil {
				t.Errorf("%s/%s: %v", kind, e.Name(), err)
			}
		}
	}
}
