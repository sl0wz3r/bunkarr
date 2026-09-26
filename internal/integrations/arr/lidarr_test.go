package arr_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/integrations/arr"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr/arrtest"
)

// TestLidarrURLBase: a Lidarr served under a URL base ("/lidarr") is reached at
// <base>/api/v1/... and its backups at <base>/backup/<type>/<name>, never at /api/v3.
func TestLidarrURLBase(t *testing.T) {
	ctx := context.Background()
	srv := arrtest.NewServer(t, arr.KindLidarr, testKey, arrtest.WithURLBase("/lidarr"))
	c := newClient(t, arr.KindLidarr, srv.URL+"/", arr.Options{})
	st, err := c.Status(ctx)
	if err != nil || st.AppName != "Lidarr" || st.Version != "3.1.0.4875" {
		t.Fatalf("Status = %+v, %v", st, err)
	}
	var names []string
	if err := c.EachArtist(ctx, func(a arr.Artist) error { names = append(names, a.ArtistName); return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := c.MetadataProfiles(ctx); err != nil {
		t.Fatal(err)
	}
	backups, err := c.Backups(ctx)
	if err != nil || len(backups) != 1 {
		t.Fatalf("Backups = %v, %v", backups, err)
	}
	var buf bytes.Buffer
	if n, err := c.DownloadBackup(ctx, backups[0], &buf); err != nil || n == 0 {
		t.Fatalf("DownloadBackup = %d, %v", n, err)
	}
	var paths []string
	for _, r := range srv.Requests() {
		paths = append(paths, r.Path)
	}
	want := []string{"/lidarr/api/v1/system/status", "/lidarr/api/v1/artist", "/lidarr/api/v1/metadataprofile",
		"/lidarr/api/v1/system/backup", "/lidarr/backup/manual/" + backups[0].Name}
	if !slices.Equal(paths, want) || !slices.Equal(names, []string{"Scott Joplin"}) {
		t.Fatalf("paths = %q, want %q (artists %v)", paths, want, names)
	}
}

// TestLidarrSavedAsAnotherApp: a Lidarr URL saved as Radarr or Sonarr has no /api/v3, and a
// Radarr or Sonarr URL saved as Lidarr no /api/v1; Status reports ErrWrongApp either way, so a
// refresh or a backup stops before any other request.
func TestLidarrSavedAsAnotherApp(t *testing.T) {
	ctx := context.Background()
	lidarr := arrtest.NewServer(t, arr.KindLidarr, testKey)
	for _, kind := range []arr.Kind{arr.KindRadarr, arr.KindSonarr} {
		_, err := newClient(t, kind, lidarr.URL, arr.Options{}).Status(ctx)
		if !errors.Is(err, arr.ErrWrongApp) || !strings.Contains(err.Error(), kind.AppName()) {
			t.Errorf("%s client against Lidarr: %v", kind, err)
		}
		other := arrtest.NewServer(t, kind, testKey)
		_, err = newClient(t, arr.KindLidarr, other.URL, arr.Options{}).Status(ctx)
		if !errors.Is(err, arr.ErrWrongApp) {
			t.Errorf("Lidarr client against %s: %v", kind, err)
		}
		for _, r := range other.Requests() {
			if r.Path != "/api/v1/system/status" {
				t.Errorf("Lidarr client sent %s %s to %s", r.Method, r.Path, kind)
			}
		}
	}
	// A Lidarr-shaped status from another URL layout (a proxy answering for Lidarr under /api/v3)
	// is still refused by its appName.
	fake := arrtest.NewServer(t, arr.KindRadarr, testKey)
	fake.SetJSON("GET", "system/status", arrtest.Fixture(t, arr.KindLidarr, "system-status.json"))
	st, err := newClient(t, arr.KindRadarr, fake.URL, arr.Options{}).Status(ctx)
	if !errors.Is(err, arr.ErrWrongApp) || st.AppName != "Lidarr" || !strings.Contains(err.Error(), "the server is Lidarr, not Radarr") {
		t.Fatalf("Status = %+v, %v", st, err)
	}
}

// TestLidarrQuirks decodes what the fixture spike found in Lidarr 3.1.0: the artist's MusicBrainz
// id is foreignArtistId, root folders have a name, the media management file date is
// "albumReleaseDate" when set (decoded as a string), and track files carry an album but no
// relative path.
func TestLidarrQuirks(t *testing.T) {
	ctx := context.Background()
	srv := arrtest.NewServer(t, arr.KindLidarr, testKey)
	c := newClient(t, arr.KindLidarr, srv.URL, arr.Options{})
	a, err := c.Artist(ctx, 1)
	if err != nil || a.ForeignArtistID != "aec8a328-d2e8-4780-b2ea-318c7f8d6f75" || a.MetadataProfileID != 1 || a.MonitorNewItems != "all" {
		t.Fatalf("Artist = %+v, %v", a, err)
	}
	rfs, err := c.RootFolders(ctx)
	if err != nil || len(rfs) != 1 || rfs[0].Name != "Music" || rfs[0].Path != "/music" || !rfs[0].Accessible {
		t.Fatalf("RootFolders = %+v, %v", rfs, err)
	}
	mm, err := c.MediaManagement(ctx)
	if err != nil || mm.FileDate != "none" || mm.RecycleBin != "" {
		t.Fatalf("MediaManagement = %+v, %v", mm, err)
	}
	srv.SetJSON("GET", "config/mediamanagement", []byte(`{"recycleBin":"/music/.recycle","fileDate":"albumReleaseDate","id":1}`))
	if mm, err = c.MediaManagement(ctx); err != nil || mm.FileDate != "albumReleaseDate" || mm.RecycleBin != "/music/.recycle" {
		t.Fatalf("MediaManagement = %+v, %v", mm, err)
	}
	tfs, err := c.TrackFiles(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, tf := range tfs {
		if tf.AlbumID != 13 && tf.AlbumID != 46 || !strings.HasPrefix(tf.Path, a.Path+"/") {
			t.Fatalf("track file %+v", tf)
		}
	}
	// The same-id refetch of an unknown artist is a 404 the refresh checks against system/status.
	if _, err := c.Artist(ctx, 99); !errors.Is(err, arr.ErrNotFound) {
		t.Fatalf("Artist(99) = %v", err)
	}
}
