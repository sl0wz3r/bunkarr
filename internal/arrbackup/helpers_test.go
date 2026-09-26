package arrbackup

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite" // the "sqlite" driver the fixture databases are written with

	"github.com/sl0wz3r/bunkarr/internal/config"
	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr/arrtest"
	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/snapshots"
)

const (
	// arrKey is the fixture Radarr's API key.
	arrKey = "fixture-radarr-api-key-5d1e0c7a"
	// zipSecret is the API key inside the fixture zips' config.xml; it must never appear in a log,
	// a manifest or a snapshot row (S17).
	zipSecret = "zip-config-xml-secret-9b8a7f6e5d4c"
	// cmdQueued is the queued time of the recorded Backup command (command-backup-*.json).
	cmdQueuedRFC = "2026-09-25T12:30:35Z"
)

// cmdQueued is the queued time of the recorded Backup command.
var cmdQueued = mustTime(cmdQueuedRFC)

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// clock is a settable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

// recReporter records a job's logs.
type recReporter struct {
	mu   sync.Mutex
	logs []string
}

func (r *recReporter) Progress(jobs.Progress) {}

func (r *recReporter) Log(level slog.Level, msg string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, fmt.Sprintf("%s %s %v", level, msg, args))
}

func (r *recReporter) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.logs, "\n")
}

func (r *recReporter) has(sub string) bool { return strings.Contains(r.text(), sub) }

// makeDB returns the bytes of a small, valid SQLite database (rollback journal, one file) with
// rows rows, like an *arr's database in its backup zip.
func makeDB(t testing.TB, rows int) []byte {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fixture.db")
	d, err := sql.Open("sqlite", "file:"+p+"?_pragma=journal_mode(delete)")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	stmts := []string{
		`CREATE TABLE Config (Id INTEGER PRIMARY KEY, Key TEXT NOT NULL UNIQUE, Value TEXT NOT NULL)`,
		`CREATE TABLE Movies (Id INTEGER PRIMARY KEY, Title TEXT NOT NULL, Path TEXT NOT NULL)`,
		`CREATE INDEX IX_Movies_Title ON Movies (Title)`,
		`INSERT INTO Config (Key, Value) VALUES ('downloadclientpassword', '` + zipSecret + `')`,
	}
	for _, s := range stmts {
		if _, err := d.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	for i := range rows {
		if _, err := d.Exec(`INSERT INTO Movies (Title, Path) VALUES (?, ?)`, fmt.Sprintf("Movie %d", i),
			fmt.Sprintf("/movies/Movie %d (19%02d)", i, i%100)); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// zipFile is one entry of a test zip.
type zipFile struct {
	name string
	body []byte
	// store writes the entry uncompressed (so a test can damage its data in place).
	store bool
}

// buildZip returns a zip of files.
func buildZip(t testing.TB, files ...zipFile) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for _, f := range files {
		h := &zip.FileHeader{Name: f.name, Method: zip.Deflate}
		if f.store {
			h.Method = zip.Store
		}
		w, err := zw.CreateHeader(h)
		if err == nil {
			_, err = w.Write(f.body)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// configXML is an *arr's config.xml holding zipSecret as its API key.
func configXML() []byte {
	return []byte("<Config>\n  <ApiKey>" + zipSecret + "</ApiKey>\n  <AuthenticationMethod>Forms</AuthenticationMethod>\n</Config>\n")
}

// goodZip is a valid Radarr backup zip.
func goodZip(t testing.TB) []byte {
	t.Helper()
	return buildZip(t, zipFile{name: ConfigXML, body: configXML()}, zipFile{name: "radarr.db", body: makeDB(t, 200)})
}

// damagedDBZip is a Radarr backup zip whose database fails quick_check (a page in the middle is
// overwritten; the zip itself is valid).
func damagedDBZip(t testing.TB) []byte {
	t.Helper()
	dbb := makeDB(t, 2000)
	const page = 4096
	mid := (len(dbb) / page / 2) * page
	for i := mid; i < mid+page && i < len(dbb); i++ {
		dbb[i] = byte(i*7 + 3)
	}
	return buildZip(t, zipFile{name: ConfigXML, body: configXML()}, zipFile{name: "radarr.db", body: dbb})
}

// entry is a system/backup element for the fake *arr.
type entry struct {
	id   int64
	name string
	typ  string
	time time.Time
	zip  []byte
}

// fixture is a database, a destination on a temp directory, a fake Radarr, its Backups folder
// and a Radarr integration.
type fixture struct {
	ctx     context.Context
	db      *db.DB
	jobs    *jobqueue.Store
	ints    *integrations.Store
	dests   *destinations.Store
	runner  *Runner
	clock   *clock
	arr     *arrtest.Server
	backups string // the Radarr Backups folder as Bunkarr sees it
	config  string // Bunkarr's config directory
	target  string // destination target
	integ   integrations.Integration
	dest    destinations.Destination
	rep     *recReporter
	entries []entry
}

// newFixture sets up the fixture with the folder method and one manual backup made by the
// recorded Backup command (its time equals the command's queued time). The clock is a minute
// before that command, so a job queued now is queued before the *arr made the backup.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	base := t.TempDir()
	if r, err := filepath.EvalSymlinks(base); err == nil {
		base = r
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
		dests: destinations.New(d, destinations.Options{}), clock: &clock{t: cmdQueued.Add(-time.Minute)},
		config: filepath.Join(base, "config"), target: filepath.Join(base, "target"), backups: filepath.Join(base, "radarr-backups"),
		rep: &recReporter{}}
	for _, dir := range []string{f.config, f.target, f.backups} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if f.dest, err = f.dests.Create(ctx, destinations.Input{Name: "NAS", Target: f.target}, destinations.CreateOptions{AllowLocal: true}); err != nil {
		t.Fatal(err)
	}
	if !f.dest.Capabilities.EnforcesModes {
		t.Fatalf("the test destination does not enforce modes: %+v", f.dest.Capabilities)
	}
	f.arr = arrtest.NewServer(t, arr.KindRadarr, arrKey)
	f.setEntries(t, entry{id: 1, name: "radarr_backup_v6.4.4.10685_2026.09.25_12.30.35.zip", typ: arr.BackupManual, time: cmdQueued, zip: goodZip(t)})
	settings, _ := json.Marshal(map[string]any{"backupFolder": f.backups, "backup": map[string]any{"destinationId": f.dest.ID}})
	if f.integ, err = f.ints.Create(ctx, integrations.Input{Type: integrations.TypeRadarr, Name: "Radarr 4K", URL: f.arr.URL,
		APIKey: arrKey, Settings: settings}); err != nil {
		t.Fatal(err)
	}
	f.newRunner(t)
	return f
}

// newRunner (re)builds the runner with short waits.
func (f *fixture) newRunner(t *testing.T) {
	t.Helper()
	var err error
	if f.runner, err = NewRunner(Options{DB: f.db, Integrations: f.ints, Destinations: f.dests, ConfigDir: f.config,
		Now: f.clock.Now, Location: time.UTC, PollInterval: 5 * time.Millisecond, CommandTimeout: 2 * time.Second,
		FolderRetries: 3, FolderRetryDelay: 20 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
}

// setEntries makes the fake Radarr list entries (and serve their zips over HTTP), and writes
// them into the Backups folder.
func (f *fixture) setEntries(t *testing.T, es ...entry) {
	t.Helper()
	f.entries = es
	list := make([]map[string]any, 0, len(es))
	for _, e := range es {
		f.arr.SetBackupZip(e.typ, e.name, e.zip)
		list = append(list, map[string]any{"id": e.id, "name": e.name, "type": e.typ, "size": len(e.zip),
			"time": e.time.UTC().Format(time.RFC3339), "path": "/backup/" + e.typ + "/" + e.name})
		dir := filepath.Join(f.backups, e.typ)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, e.name), e.zip, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f.arr.SetBackups(list)
}

// setSettings replaces the integration's settings (the key is kept).
func (f *fixture) setSettings(t *testing.T, settings map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(settings)
	enabled := f.integ.Enabled
	it, err := f.ints.Update(f.ctx, f.integ.ID, integrations.Input{Type: f.integ.Type, Name: f.integ.Name, URL: f.integ.URL,
		Enabled: &enabled, Settings: raw})
	if err != nil {
		t.Fatal(err)
	}
	f.integ = it
}

// httpSettings switches the integration to the HTTP method.
func (f *fixture) httpSettings(t *testing.T) {
	t.Helper()
	f.setSettings(t, map[string]any{"backup": map[string]any{"destinationId": f.dest.ID}})
}

// newJob inserts a running arr_backup job row (the manager's part) and returns it.
func (f *fixture) newJob(t *testing.T, dry bool) jobs.Job {
	t.Helper()
	params := jobs.Params{IntegrationID: f.integ.ID, DestinationID: f.dest.ID}
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	queued := f.clock.Now().UTC()
	now := db.FormatTime(queued)
	var id int64
	err = f.db.Write(f.ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(f.ctx, `INSERT INTO jobs (type, status, trigger, dry_run, params, destination_id, integration_id, queued_at, started_at)
			VALUES ('arr_backup', 'running', 'manual', ?, ?, ?, ?, ?, ?)`, dry, string(raw), f.dest.ID, f.integ.ID, now, now)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return jobs.Job{ID: id, Type: jobs.TypeArrBackup, Status: jobs.StatusRunning, Trigger: jobs.TriggerManual,
		DryRun: dry, Params: params, Attempt: 1, QueuedAt: queued}
}

// run runs a job with the fixture's reporter and the job store as ItemStore.
func (f *fixture) run(t *testing.T, job jobs.Job) (jobs.Result, error) {
	t.Helper()
	return f.runner.Run(f.ctx, job, jobs.Env{Reporter: f.rep, Items: f.jobs})
}

// runOK runs a new job and requires it to succeed.
func (f *fixture) runOK(t *testing.T) jobs.Result {
	t.Helper()
	res, err := f.run(t, f.newJob(t, false))
	if err != nil {
		t.Fatalf("run: %v\nlogs:\n%s", err, f.rep.text())
	}
	return res
}

// folderDir is the absolute path of the integration's version folder.
func (f *fixture) folderDir() string {
	return filepath.Join(f.target, filepath.FromSlash(FolderName(f.integ.Name, f.integ.ID)))
}

// dirEntries lists the names in the integration's version folder.
func (f *fixture) dirEntries(t *testing.T) []string {
	t.Helper()
	es, err := os.ReadDir(f.folderDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

// snapshots returns the recorded versions, newest first.
func (f *fixture) snapshots(t *testing.T) []snapshots.Snapshot {
	t.Helper()
	s, err := f.runner.Store().List(f.ctx, f.dest.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// commands counts the Backup commands the fake Radarr received.
func (f *fixture) commands() int {
	n := 0
	for _, r := range f.arr.Requests() {
		if r.Method == "POST" && strings.HasSuffix(r.Path, "/command") {
			n++
		}
	}
	return n
}

// downloads counts the backup downloads the fake Radarr answered (range probes included).
func (f *fixture) downloads() int {
	n := 0
	for _, r := range f.arr.Requests() {
		if strings.HasPrefix(r.Path, "/backup/") {
			n++
		}
	}
	return n
}

// checkVersion checks a recorded version against the destination: the zip with its size and
// sha256 and mode 0600, the directories 0700, manifest.json equal to the row's manifest.
func (f *fixture) checkVersion(t *testing.T, s snapshots.Snapshot) Manifest {
	t.Helper()
	var m Manifest
	if err := json.Unmarshal(s.Manifest, &m); err != nil {
		t.Fatalf("manifest of %s: %v", s.Path, err)
	}
	dir := filepath.Join(f.target, filepath.FromSlash(s.Path))
	onDisk, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if err != nil || !bytes.Equal(bytes.TrimSpace(onDisk), bytes.TrimSpace(s.Manifest)) {
		t.Fatalf("manifest.json of %s differs from the row (%v)", s.Path, err)
	}
	zipPath := filepath.Join(dir, m.Zip.Name)
	data, err := os.ReadFile(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if int64(len(data)) != m.Zip.Size || hex.EncodeToString(sum[:]) != m.Zip.SHA256 || s.Size != m.Zip.Size {
		t.Fatalf("%s: %d bytes %x, manifest %+v, row size %d", zipPath, len(data), sum, m.Zip, s.Size)
	}
	for p, want := range map[string]os.FileMode{zipPath: 0o600, filepath.Join(dir, ManifestName): 0o600, dir: 0o700,
		f.folderDir(): 0o700, filepath.Join(f.target, filepath.FromSlash(ArrRoot)): 0o700} {
		fi, err := os.Stat(p)
		if err != nil || fi.Mode().Perm() != want {
			t.Fatalf("%s: mode %v, want %v (%v)", p, fi.Mode().Perm(), want, err)
		}
	}
	if !m.Sensitive || m.App != "radarr" || m.IntegrationID != f.integ.ID || m.Format != ManifestFormat {
		t.Fatalf("manifest %+v", m)
	}
	return m
}

// assertConverged checks the state every backup must end in: exactly one version per recorded
// row, no partial or trash directory, no temp file, no staging directory.
func (f *fixture) assertConverged(t *testing.T, wantVersions int) []snapshots.Snapshot {
	t.Helper()
	snaps := f.snapshots(t)
	var dirs []string
	for _, e := range f.dirEntries(t) {
		if strings.HasPrefix(e, ".") {
			t.Fatalf("leftover %s in %s", e, f.folderDir())
		}
		dirs = append(dirs, e)
	}
	if len(dirs) != wantVersions || len(snaps) != wantVersions {
		t.Fatalf("%d version directories %v and %d rows, want %d", len(dirs), dirs, len(snaps), wantVersions)
	}
	for _, s := range snaps {
		if s.Kind != snapshots.KindArr || s.Integrity != IntegrityOK || !slices.Contains(dirs, filepath.Base(s.Path)) {
			t.Fatalf("snapshot %+v does not match the directories %v", s, dirs)
		}
		f.checkVersion(t, s)
	}
	_ = filepath.WalkDir(f.target, func(p string, d os.DirEntry, err error) error {
		if err == nil && filecopy.IsTempName(d.Name()) {
			t.Fatalf("temp file left: %s", p)
		}
		return nil
	})
	if st, err := os.ReadDir(filepath.Join(f.config, StagingRoot)); err == nil && len(st) != 0 {
		t.Fatalf("staging left: %v", st)
	}
	return snaps
}

// setCapabilities stores capabilities for the destination (as an older or another probe would).
func (f *fixture) setEnforcesModes(t *testing.T, on bool) {
	t.Helper()
	caps := f.dest.Capabilities
	caps.EnforcesModes = on
	raw, _ := json.Marshal(caps)
	if err := f.db.Write(f.ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(f.ctx, `UPDATE destinations SET capabilities = ? WHERE id = ?`, string(raw), f.dest.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// setRetention sets the destination's *arr version retention.
func (f *fixture) setRetention(t *testing.T, daily, weekly int) {
	t.Helper()
	if _, err := f.dests.Update(f.ctx, f.dest.ID, destinations.Input{Retention: &destinations.Retention{ArrDaily: daily, ArrWeekly: weekly}}); err != nil {
		t.Fatal(err)
	}
}
