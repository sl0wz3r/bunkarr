package integrations

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/integrations/arr"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr/arrtest"
)

const arrKey = "arr-check-key-0123456789abcdef"

const radarrBackup = "radarr_backup_v6.4.4.10685_2026.09.25_12.30.35.zip"

func mkdir(t *testing.T, p string) string {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestArrTestRadarr tests a Radarr with two root folders, one mapped into a source and one
// (/movies-4k) that no mapping covers, and a mounted backup folder.
func TestArrTestRadarr(t *testing.T) {
	ctx := context.Background()
	srv := arrtest.NewServer(t, arr.KindRadarr, arrKey)
	srv.UseMovies4K()
	dir := t.TempDir()
	movies := mkdir(t, filepath.Join(dir, "media", "movies"))
	backups := mkdir(t, filepath.Join(dir, "radarr-backups", "manual"))
	if err := os.WriteFile(filepath.Join(backups, radarrBackup), []byte("zip"), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := &ArrSettings{PathMappings: []ArrPathMapping{{Arr: "/movies", Local: movies}}, BackupFolder: filepath.Join(dir, "radarr-backups")}
	opts := ArrTestOptions{Sources: []SourceRef{{ID: 7, Path: filepath.Join(dir, "media")}, {ID: 9, Path: movies}}}

	res := TestArr(ctx, TypeRadarr, srv.URL, arrKey, settings, opts)
	if !res.OK || res.Message != "Connected to Radarr 6.4.4.10685." || res.AppName != "Radarr" || res.Version != "6.4.4.10685" {
		t.Fatalf("TestArr = %+v", res)
	}
	if len(res.RootFolders) != 2 {
		t.Fatalf("root folders = %+v", res.RootFolders)
	}
	rf := res.RootFolders[0]
	if rf.Path != "/movies" || !rf.Accessible || rf.LocalPath == nil || *rf.LocalPath != movies || rf.SourceID == nil || *rf.SourceID != 9 ||
		!rf.Exists || rf.Reason != "" {
		t.Fatalf("/movies = %+v (the longest source prefix is source 9)", rf)
	}
	rf = res.RootFolders[1]
	if rf.Path != "/movies-4k" || rf.LocalPath != nil || rf.SourceID != nil || rf.Exists || !strings.Contains(rf.Reason, "no path mapping") {
		t.Fatalf("/movies-4k = %+v", rf)
	}
	if res.Backup == nil || *res.Backup != (ArrBackupAccess{Folder: "ok", HTTP: "ok"}) {
		t.Fatalf("backup = %+v", res.Backup)
	}
	if res.ManualBackups == nil || *res.ManualBackups != (ArrBackupCount{Count: 1, Bytes: 92309}) {
		t.Fatalf("manual backups = %+v", res.ManualBackups)
	}
	if res.RecycleBin != nil || res.FileDate != "none" {
		t.Fatalf("recycle bin %+v, file date %q", res.RecycleBin, res.FileDate)
	}
	body, err := json.Marshal(res)
	if err != nil || strings.Contains(string(body), arrKey) || !strings.Contains(string(body), `"recycleBin":null`) ||
		!strings.Contains(string(body), `"localPath":null`) {
		t.Fatalf("JSON %s, %v", body, err)
	}
	// Only allowed requests were sent: status, root folders, media management, backups and the
	// one-byte probe of the newest backup.
	var got []string
	for _, r := range srv.Requests() {
		got = append(got, r.Method+" "+r.Path)
	}
	want := []string{"GET /api/v3/system/status", "GET /api/v3/rootfolder", "GET /api/v3/config/mediamanagement",
		"GET /api/v3/system/backup", "GET /backup/manual/" + radarrBackup}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("requests %q, want %q", got, want)
	}

	// The same *arr with an inaccessible folder, a missing local folder and a login for backups.
	srv.SetRootFolderAccessible("/movies", false)
	srv.RequireLogin(true)
	if err := os.Remove(filepath.Join(backups, radarrBackup)); err != nil {
		t.Fatal(err)
	}
	settings.PathMappings = append(settings.PathMappings, ArrPathMapping{Arr: "/movies-4k", Local: filepath.Join(dir, "gone")})
	res = TestArr(ctx, TypeRadarr, srv.URL, arrKey, settings, opts)
	if !res.OK || !strings.Contains(res.RootFolders[0].Reason, "not accessible") || res.RootFolders[1].Exists ||
		!strings.Contains(res.RootFolders[1].Reason, "does not exist") || !strings.Contains(res.RootFolders[1].Reason, "not inside a source") {
		t.Fatalf("root folders = %+v", res.RootFolders)
	}
	if *res.Backup != (ArrBackupAccess{Folder: "missing", HTTP: "login-required"}) {
		t.Fatalf("backup = %+v", res.Backup)
	}
}

func TestArrTestWithoutSettings(t *testing.T) {
	srv := arrtest.NewServer(t, arr.KindLidarr, arrKey)
	res := TestArr(context.Background(), TypeLidarr, srv.URL, arrKey, nil, ArrTestOptions{})
	if !res.OK || res.Backup == nil || res.Backup.Folder != "not-set" || res.Backup.HTTP != "ok" || len(res.RootFolders) != 1 ||
		res.RootFolders[0].LocalPath != nil || res.RootFolders[0].SourceID != nil {
		t.Fatalf("TestArr = %+v", res)
	}
}

func TestArrTestRecycleBin(t *testing.T) {
	ctx := context.Background()
	srv := arrtest.NewServer(t, arr.KindSonarr, arrKey)
	dir := t.TempDir()
	tv := mkdir(t, filepath.Join(dir, "tv"))
	settings := &ArrSettings{PathMappings: []ArrPathMapping{{Arr: "/tv", Local: tv}}}
	setBin := func(p string) {
		b, _ := json.Marshal(map[string]any{"recycleBin": p, "recycleBinCleanupDays": 7, "fileDate": "localAirDate"})
		srv.SetJSON(http.MethodGet, "config/mediamanagement", b)
	}
	for _, tt := range []struct {
		name     string
		bin      string
		exclude  []string
		defaults []string
		rel      string
		source   bool
		excluded bool
	}{
		{"inside the source", "/tv/.recycle", nil, nil, ".recycle", true, false},
		{"excluded by the offered pattern", "/tv/.recycle", []string{"/.recycle/"}, nil, ".recycle", true, true},
		{"excluded by a default", "/tv/#Recycle", nil, []string{"#recycle/"}, "#Recycle", true, true},
		{"a case-sensitive own pattern", "/tv/.Recycle", []string{".recycle/"}, nil, ".Recycle", true, false},
		{"under an excluded ancestor", "/tv/trash/bin", []string{"/trash/"}, nil, "trash/bin", true, true},
		{"anchored pattern elsewhere", "/tv/a/.recycle", []string{"/.recycle/"}, nil, "a/.recycle", true, false},
		{"unmapped", "/downloads/.recycle", nil, nil, "", false, false},
	} {
		setBin(tt.bin)
		res := TestArr(ctx, TypeSonarr, srv.URL, arrKey, settings,
			ArrTestOptions{Sources: []SourceRef{{ID: 3, Path: tv, Exclude: tt.exclude}}, DefaultExcludes: tt.defaults})
		rb := res.RecycleBin
		if !res.OK || rb == nil || rb.Path != tt.bin || (rb.SourceID != nil) != tt.source || rb.RelPath != tt.rel || rb.Excluded != tt.excluded {
			t.Errorf("%s: recycle bin %+v (ok %v)", tt.name, rb, res.OK)
		}
		if res.FileDate != "localAirDate" {
			t.Errorf("%s: file date %q", tt.name, res.FileDate)
		}
	}
}

func TestArrTestFailures(t *testing.T) {
	ctx := context.Background()
	sonarr := arrtest.NewServer(t, arr.KindSonarr, arrKey)

	res := TestArr(ctx, TypeSonarr, sonarr.URL, "wrong-key-0123456789", nil, ArrTestOptions{})
	if res.OK || !strings.Contains(res.Message, "Sonarr rejected the API key") || strings.Contains(res.Message, "wrong-key-0123456789") {
		t.Fatalf("wrong key: %+v", res)
	}
	res = TestArr(ctx, TypeSonarr, sonarr.URL, "", nil, ArrTestOptions{})
	if res.OK || !strings.Contains(res.Message, "requires an API key") {
		t.Fatalf("no key: %+v", res)
	}
	res = TestArr(ctx, TypeRadarr, sonarr.URL, arrKey, nil, ArrTestOptions{})
	if res.OK || res.AppName != "Sonarr" || res.Message != "The server at this URL is Sonarr, not Radarr: check the URL and the type." {
		t.Fatalf("Sonarr tested as Radarr: %+v", res)
	}
	res = TestArr(ctx, TypeLidarr, sonarr.URL, arrKey, nil, ArrTestOptions{})
	if res.OK || !strings.Contains(res.Message, "is not Lidarr") || res.AppName != "" {
		t.Fatalf("Sonarr tested as Lidarr: %+v", res)
	}
	sonarr.SetStatus(http.MethodGet, "rootfolder", http.StatusInternalServerError)
	res = TestArr(ctx, TypeSonarr, sonarr.URL, arrKey, nil, ArrTestOptions{})
	if res.OK || !strings.HasPrefix(res.Message, "Connected to Sonarr 4.0.20.3014, but reading its root folders failed") ||
		res.Backup == nil || res.RootFolders != nil {
		t.Fatalf("root folders failing: %+v", res)
	}
	sonarr.Redirect(http.MethodGet, "system/status")
	res = TestArr(ctx, TypeSonarr, sonarr.URL, arrKey, nil, ArrTestOptions{})
	if res.OK || !strings.Contains(res.Message, "did not answer like Sonarr") {
		t.Fatalf("redirect: %+v", res)
	}
	sonarr.Close()
	res = TestArr(ctx, TypeSonarr, sonarr.URL, arrKey, nil, ArrTestOptions{})
	if res.OK || !strings.HasPrefix(res.Message, "Could not reach Sonarr") || strings.Contains(res.Message, strings.TrimPrefix(sonarr.URL, "http://")) {
		t.Fatalf("unreachable: %+v", res)
	}
	res = TestArr(ctx, TypeRadarr, "http://169.254.169.254", arrKey, nil, ArrTestOptions{})
	if res.OK || !strings.Contains(res.Message, "does not connect to this address") {
		t.Fatalf("metadata address: %+v", res)
	}
	res = TestArr(ctx, TypePlex, "http://plex", arrKey, nil, ArrTestOptions{})
	if res.OK || !strings.Contains(res.Message, "not Sonarr, Radarr or Lidarr") {
		t.Fatalf("plex: %+v", res)
	}
	res = TestArr(ctx, TypeRadarr, "ftp://x", arrKey, nil, ArrTestOptions{})
	if res.OK || !strings.Contains(res.Message, "cannot be used") {
		t.Fatalf("bad URL: %+v", res)
	}
}

func TestArrTestBackupFolder(t *testing.T) {
	ctx := context.Background()
	srv := arrtest.NewServer(t, arr.KindRadarr, arrKey)
	dir := t.TempDir()
	folder := filepath.Join(dir, "backups")
	check := func(want string) {
		t.Helper()
		res := TestArr(ctx, TypeRadarr, srv.URL, arrKey, &ArrSettings{BackupFolder: folder}, ArrTestOptions{})
		if res.Backup == nil || res.Backup.Folder != want {
			t.Fatalf("folder = %+v, want %q", res.Backup, want)
		}
	}
	check("missing")
	mkdir(t, filepath.Join(folder, "manual"))
	check("missing") // the folder exists, the newest backup is not in it
	target := filepath.Join(folder, "manual", radarrBackup)
	if err := os.Symlink("/etc/hosts", target); err != nil {
		t.Fatal(err)
	}
	check("unreadable") // a symlink is never followed
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("zip"), 0o600); err != nil {
		t.Fatal(err)
	}
	check("ok")
	if os.Geteuid() != 0 {
		if err := os.Chmod(target, 0); err != nil {
			t.Fatal(err)
		}
		check("unreadable")
		_ = os.Chmod(target, 0o600)
	}
	// No backup at all: the folder itself is checked.
	srv.SetBackups([]map[string]any{})
	check("ok")
	res := TestArr(ctx, TypeRadarr, srv.URL, arrKey, nil, ArrTestOptions{})
	if res.Backup.HTTP != "unknown" || *res.ManualBackups != (ArrBackupCount{}) {
		t.Fatalf("no backups: %+v %+v", res.Backup, res.ManualBackups)
	}
	// A file where the folder should be.
	folder = target
	check("missing")
}

func TestLocateInSources(t *testing.T) {
	sources := []SourceRef{{ID: 1, Path: "/media"}, {ID: 2, Path: "/media/movies/"}, {ID: 3, Path: "/media/movies-4k"}, {ID: 4, Path: "relative"}}
	for _, tt := range []struct {
		p      string
		id     int64
		rel    string
		exists bool
	}{
		{"/media/movies/Heat (1995)", 2, "Heat (1995)", true},
		{"/media/movies", 2, "", true},
		{"/media/movies-4k/Nosferatu (1922)", 3, "Nosferatu (1922)", true},
		{"/media/moviesX/a", 1, "moviesX/a", true},
		{"/media//tv/../tv/x", 1, "tv/x", true},
		{"/other", 0, "", false},
		{"relative/x", 0, "", false},
	} {
		loc, ok := LocateInSources(sources, nil, tt.p)
		if ok != tt.exists || loc.SourceID != tt.id || loc.RelPath != tt.rel {
			t.Errorf("LocateInSources(%q) = %+v, %v", tt.p, loc, ok)
		}
	}
}
