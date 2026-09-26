package arrbackup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/config"
	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr/arrtest"
	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/snapshots"
)

// Lidarr's backups (design D2, §10): API v1, a database named lidarr.db in the zip, and backup
// zips served as application/x-zip-compressed (as recorded from Lidarr 3.1.0).

const (
	lidarrKey = "fixture-lidarr-api-key-8c2b19e4"
	// lidarrManual is the manual backup the recorded Lidarr Backup command makes.
	lidarrManual = "lidarr_backup_v3.1.0.4875_2026.09.25_12.39.28.zip"
)

// lidarrQueued is the queued time of the recorded Lidarr Backup command.
var lidarrQueued = mustTime("2026-09-25T12:39:28Z")

// lidarrZip is a valid Lidarr backup zip: config.xml and lidarr.db.
func lidarrZip(t testing.TB) []byte {
	t.Helper()
	return buildZip(t, zipFile{name: ConfigXML, body: configXML()}, zipFile{name: "lidarr.db", body: makeDB(t, 50)})
}

// newLidarrFixture is newFixture with a fake Lidarr: the folder method, and one manual backup made
// by the recorded Backup command.
func newLidarrFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(ctx, filepath.Join(base, "bunkarr.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	kr, err := config.NewKeyring(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{ctx: ctx, db: d, jobs: jobqueue.NewStore(d), ints: integrations.NewStore(d, kr),
		dests: destinations.New(d, destinations.Options{}), clock: &clock{t: lidarrQueued.Add(-time.Minute)},
		config: filepath.Join(base, "config"), target: filepath.Join(base, "target"), backups: filepath.Join(base, "lidarr-backups"),
		rep: &recReporter{}}
	for _, dir := range []string{f.config, f.target, f.backups} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if f.dest, err = f.dests.Create(ctx, destinations.Input{Name: "NAS", Target: f.target}, destinations.CreateOptions{AllowLocal: true}); err != nil {
		t.Fatal(err)
	}
	f.arr = arrtest.NewServer(t, arr.KindLidarr, lidarrKey)
	f.setEntries(t, entry{id: 1624317952, name: lidarrManual, typ: arr.BackupManual, time: lidarrQueued, zip: lidarrZip(t)})
	settings, _ := json.Marshal(map[string]any{"backupFolder": f.backups, "backup": map[string]any{"destinationId": f.dest.ID}})
	if f.integ, err = f.ints.Create(ctx, integrations.Input{Type: integrations.TypeLidarr, Name: "Lidarr", URL: f.arr.URL,
		APIKey: lidarrKey, Settings: settings}); err != nil {
		t.Fatal(err)
	}
	f.newRunner(t)
	return f
}

// lidarrVersion checks the one recorded version of a Lidarr backup and returns its manifest.
func (f *fixture) lidarrVersion(t *testing.T, method string) Manifest {
	t.Helper()
	snaps := f.snapshots(t)
	if len(snaps) != 1 {
		t.Fatalf("snapshots %+v", snaps)
	}
	s := snaps[0]
	var m Manifest
	if err := json.Unmarshal(s.Manifest, &m); err != nil {
		t.Fatal(err)
	}
	if s.Kind != snapshots.KindArr || s.Integrity != IntegrityOK || s.Method != method || s.IntegrationID != f.integ.ID ||
		!strings.HasPrefix(s.Path, FolderName(f.integ.Name, f.integ.ID)+"/") || !strings.HasPrefix(s.Path, ArrRoot+"/lidarr-") {
		t.Fatalf("snapshot %+v", s)
	}
	var names []string
	for _, e := range m.Entries {
		names = append(names, e.Name)
	}
	if m.App != "lidarr" || m.AppVersion != "3.1.0.4875" || m.Backup.Name != lidarrManual || !m.Backup.Time.Equal(lidarrQueued) ||
		m.Zip.Name != lidarrManual || !slices.Equal(names, []string{ConfigXML, "lidarr.db"}) || !m.Sensitive ||
		m.Integrity != (ManifestIntegrity{Zip: IntegrityOK, Database: IntegrityOK}) {
		t.Fatalf("manifest %+v", m)
	}
	zipPath := filepath.Join(f.target, filepath.FromSlash(s.Path), m.Zip.Name)
	for p, mode := range map[string]os.FileMode{zipPath: 0o600, filepath.Dir(zipPath): 0o700} {
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != mode {
			t.Fatalf("%s: %v, %v", p, fi, err)
		}
	}
	rep, err := VerifyZip(f.ctx, zipPath, "lidarr", t.TempDir())
	if err != nil || !rep.OK() {
		t.Fatalf("the stored zip does not verify: %+v, %v", rep, err)
	}
	return m
}

func TestLidarrBackupFolder(t *testing.T) {
	f := newLidarrFixture(t)
	res := f.runOK(t)
	st := res.Stats.(Stats)
	if st.Method != FetchFolder || st.BackupName != lidarrManual || st.ReusedScheduled || st.Integrity != IntegrityOK {
		t.Fatalf("stats %+v", st)
	}
	f.lidarrVersion(t, MethodFolder)
	// The Backup command and its polling go to API v1; nothing is downloaded over HTTP.
	var got []string
	for _, r := range f.arr.Requests() {
		got = append(got, r.Method+" "+r.Path)
	}
	for _, want := range []string{"GET /api/v1/system/status", "GET /api/v1/system/backup", "POST /api/v1/command", "GET /api/v1/command/40"} {
		if !slices.Contains(got, want) {
			t.Fatalf("requests %v lack %s", got, want)
		}
	}
	for _, r := range got {
		if strings.Contains(r, "/api/v3/") || strings.Contains(r, " /backup/") {
			t.Fatalf("request %s", r)
		}
	}
}

func TestLidarrBackupHTTP(t *testing.T) {
	f := newLidarrFixture(t)
	f.httpSettings(t)
	f.runOK(t)
	f.lidarrVersion(t, MethodHTTP)
	var downloads []string
	for _, r := range f.arr.Requests() {
		if strings.HasPrefix(r.Path, "/backup/") {
			downloads = append(downloads, r.Path)
			if r.Header.Get("X-Api-Key") != lidarrKey || r.RawQuery != "" {
				t.Fatalf("download %+v", r)
			}
		}
	}
	// The 1-byte check before the command, then the download (served as
	// application/x-zip-compressed, like Lidarr 3.1.0).
	if !slices.Equal(downloads, []string{"/backup/manual/" + lidarrManual, "/backup/manual/" + lidarrManual}) {
		t.Fatalf("downloads %v", downloads)
	}
}

func TestLidarrBackupLoginRequired(t *testing.T) {
	f := newLidarrFixture(t)
	f.httpSettings(t)
	f.arr.RequireLogin(true)
	_, err := f.run(t, f.newJob(t, false))
	const want = "Lidarr requires a login to download backups: set its Backups folder in Bunkarr (Settings → Connect → Lidarr) " +
		"or set Authentication Required to 'Disabled for Local Addresses' in Lidarr"
	var le *LoginRequiredError
	if !errors.As(err, &le) || err.Error() != want || f.commands() != 0 {
		t.Fatalf("err = %v, %d commands", err, f.commands())
	}
}

// TestLidarrBackupOfAnotherApp: a zip without lidarr.db (another *arr's backup) is recorded as
// failed; a Radarr URL saved as Lidarr fails before any Backup command is sent.
func TestLidarrBackupOfAnotherApp(t *testing.T) {
	f := newLidarrFixture(t)
	f.setEntries(t, entry{id: 1, name: lidarrManual, typ: arr.BackupManual, time: lidarrQueued, zip: goodZip(t)}) // radarr.db
	_, err := f.run(t, f.newJob(t, false))
	if !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), "lidarr.db") {
		t.Fatalf("err = %v", err)
	}
	if snaps := f.snapshots(t); len(snaps) != 1 || snaps[0].Integrity != IntegrityFailed {
		t.Fatalf("snapshots %+v", snaps)
	}

	g := newLidarrFixture(t)
	radarr := arrtest.NewServer(t, arr.KindRadarr, lidarrKey)
	enabled := true
	it, err := g.ints.Update(g.ctx, g.integ.ID, integrations.Input{Name: g.integ.Name, URL: radarr.URL, APIKey: lidarrKey, Enabled: &enabled,
		Settings: json.RawMessage(`{"backup":{"destinationId":` + jsonInt(g.dest.ID) + `}}`)})
	if err != nil {
		t.Fatal(err)
	}
	g.integ = it
	_, err = g.run(t, g.newJob(t, false))
	if !errors.Is(err, arr.ErrWrongApp) {
		t.Fatalf("err = %v", err)
	}
	for _, r := range radarr.Requests() {
		if r.Method != "GET" || strings.HasPrefix(r.Path, "/backup/") {
			t.Fatalf("a Lidarr backup sent %s %s to a Radarr", r.Method, r.Path)
		}
	}
	if len(g.snapshots(t)) != 0 {
		t.Fatal("a version was recorded")
	}
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// TestCrashMatrixLidarr: a Lidarr backup crashed at every step and resumed converges to one
// complete, verified version holding lidarr.db, with one Backup command in all and nothing left
// behind.
func TestCrashMatrixLidarr(t *testing.T) {
	for _, point := range crashPoints {
		for _, method := range []string{FetchFolder, FetchHTTP} {
			t.Run(point+" "+method, func(t *testing.T) {
				f := newLidarrFixture(t)
				want := MethodFolder
				if method == FetchHTTP {
					f.httpSettings(t)
					want = MethodHTTP
				}
				job := f.newJob(t, false)
				f.crashRun(t, job, point)
				job.Attempt, job.Trigger = 2, jobs.TriggerResume
				f.clock.Set(f.clock.Now().Add(time.Minute))
				if _, err := f.run(t, job); err != nil {
					t.Fatalf("resume: %v\nlogs:\n%s", err, f.rep.text())
				}
				if f.commands() != 1 {
					t.Fatalf("%d Backup commands in all", f.commands())
				}
				f.lidarrVersion(t, want)
				for _, e := range f.dirEntries(t) {
					if strings.HasPrefix(e, ".") {
						t.Fatalf("leftover %s", e)
					}
				}
				if st, err := os.ReadDir(filepath.Join(f.config, StagingRoot)); err == nil && len(st) != 0 {
					t.Fatalf("staging left: %v", st)
				}
			})
		}
	}
}
