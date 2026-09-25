package plexdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/config"
	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/integrations/plex/plextest"
	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/logging"
)

// plexAPIToken is the X-Plex-Token of the fixture integration.
const plexAPIToken = "fixture-plex-api-token-77aa01"

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

type fixture struct {
	ctx    context.Context
	db     *db.DB
	jobs   *jobqueue.Store
	ints   *integrations.Store
	dests  *destinations.Store
	runner *Runner
	clock  *clock
	plex   *plextest.Server
	data   string // Plex data path
	config string // Bunkarr's config directory
	target string // destination target
	integ  integrations.Integration
	dest   destinations.Destination
	rep    *recReporter
}

// newFixture sets up a database, a destination on a temp directory, a fake Plex server and a
// Plex integration whose data path holds a cleanly stopped library, a blobs database and
// Preferences.xml. The clock is at 12:00 UTC, outside Plex's butler window (02-05).
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
		dests: destinations.New(d, destinations.Options{}), clock: &clock{t: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)},
		config: filepath.Join(base, "config"), target: filepath.Join(base, "target"), rep: &recReporter{}}
	for _, dir := range []string{f.config, f.target} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if f.dest, err = f.dests.Create(ctx, destinations.Input{Name: "NAS", Target: f.target}, destinations.CreateOptions{AllowLocal: true}); err != nil {
		t.Fatal(err)
	}

	f.data = plexDataDir(t)
	if err := makeLibrary(t, dbPath(f.data, LibraryDB), 200, 200).Close(); err != nil {
		t.Fatal(err)
	}
	if err := makeLibrary(t, dbPath(f.data, BlobsDB), 1, 20).Close(); err != nil {
		t.Fatal(err)
	}
	writePrefs(t, f.data)

	f.plex = plextest.NewServer(t, plexAPIToken)
	f.plex.SetIdentity("fixture-machine", "1.43.4.10903-e5521bd8c")
	settings, _ := json.Marshal(map[string]any{"dataPath": f.data})
	if f.integ, err = f.ints.Create(ctx, integrations.Input{Type: integrations.TypePlex, Name: "Plex Server", URL: f.plex.URL,
		APIKey: plexAPIToken, Settings: settings}); err != nil {
		t.Fatal(err)
	}
	if f.runner, err = NewRunner(Options{DB: d, Integrations: f.ints, Destinations: f.dests, ConfigDir: f.config,
		Now: f.clock.Now, Location: time.UTC}); err != nil {
		t.Fatal(err)
	}
	return f
}

// newJob inserts a running plexdb_backup job row (the manager's part) and returns it.
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
		res, err := tx.ExecContext(f.ctx, `INSERT INTO jobs (type, status, trigger, dry_run, params, destination_id, queued_at, started_at)
			VALUES ('plexdb_backup', 'running', 'manual', ?, ?, ?, ?, ?)`, dry, string(raw), f.dest.ID, now, now)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return jobs.Job{ID: id, Type: jobs.TypePlexDBBackup, Status: jobs.StatusRunning, Trigger: jobs.TriggerManual,
		DryRun: dry, Params: params, Attempt: 1, QueuedAt: queued}
}

// otherJob inserts a finished scan job (it uses up a job id).
func (f *fixture) otherJob(t *testing.T) {
	t.Helper()
	if err := f.db.Write(f.ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(f.ctx, `INSERT INTO jobs (type, status, trigger, params, queued_at) VALUES ('scan', 'completed', 'manual', '{}', ?)`,
			db.FormatTime(f.clock.Now()))
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// wipeHistory deletes the job history and the snapshot rows, as a new config directory (or an
// older copy of Bunkarr's database) would: job ids start over, the destination keeps its versions.
func (f *fixture) wipeHistory(t *testing.T) {
	t.Helper()
	if err := f.db.Write(f.ctx, func(tx *sql.Tx) error {
		for _, q := range []string{`DELETE FROM snapshots`, `DELETE FROM job_items`, `DELETE FROM job_logs`, `DELETE FROM jobs`} {
			if _, err := tx.ExecContext(f.ctx, q); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// run runs a job with the fixture's reporter and the job store as ItemStore.
func (f *fixture) run(t *testing.T, job jobs.Job) (jobs.Result, error) {
	t.Helper()
	return f.runner.Run(f.ctx, job, jobs.Env{Reporter: f.rep, Items: f.jobs})
}

// folderDir is the absolute path of the integration's snapshot folder.
func (f *fixture) folderDir() string {
	return filepath.Join(f.target, filepath.FromSlash(FolderName(f.integ.Name, f.integ.ID)))
}

// entries lists the names in the integration's snapshot folder.
func (f *fixture) entries(t *testing.T) []string {
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
func (f *fixture) snapshots(t *testing.T) []Snapshot {
	t.Helper()
	s, err := f.runner.Store().List(f.ctx, f.dest.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// checkVersion checks a recorded version against the destination: every manifest file present
// with its size and sha256, the library copy verified and consistent, Preferences.xml 0600.
func (f *fixture) checkVersion(t *testing.T, s Snapshot) Manifest {
	t.Helper()
	var m Manifest
	if err := json.Unmarshal(s.Manifest, &m); err != nil {
		t.Fatalf("manifest of %s: %v", s.Path, err)
	}
	onDisk, err := os.ReadFile(filepath.Join(f.target, filepath.FromSlash(s.Path), ManifestName))
	if err != nil || !json.Valid(onDisk) {
		t.Fatalf("manifest.json of %s: %v", s.Path, err)
	}
	var names []string
	var size int64
	for _, mf := range m.Files {
		names = append(names, mf.Name)
		p := filepath.Join(f.target, filepath.FromSlash(s.Path), mf.Name)
		n, sum, err := hashPath(p)
		if err != nil || n != mf.Size || sum != mf.SHA256 {
			t.Fatalf("%s: %d %s, manifest %d %s (%v)", p, n, sum, mf.Size, mf.SHA256, err)
		}
		size += n
	}
	if !slices.Equal(names, []string{LibraryDB, BlobsDB, PreferencesXML}) || size != s.Size {
		t.Fatalf("version files %v (%d bytes), row size %d", names, size, s.Size)
	}
	lib := filepath.Join(f.target, filepath.FromSlash(s.Path), LibraryDB)
	if rep, err := Verify(f.ctx, lib); err != nil || !rep.OK() {
		t.Fatalf("verify %s: %+v, %v", lib, rep, err)
	}
	invariants(t, lib)
	fi, err := os.Stat(filepath.Join(f.target, filepath.FromSlash(s.Path), PreferencesXML))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("Preferences.xml at the destination: %v, %v", fi, err)
	}
	return m
}

// assertConverged checks the state every backup must end in: exactly one version per recorded
// row, no partial or trash directory, no temp file, no staging directory.
func (f *fixture) assertConverged(t *testing.T, wantVersions int) []Snapshot {
	t.Helper()
	snaps := f.snapshots(t)
	var dirs []string
	for _, e := range f.entries(t) {
		if strings.HasPrefix(e, ".") {
			t.Fatalf("leftover %s in %s", e, f.folderDir())
		}
		dirs = append(dirs, e)
	}
	if len(dirs) != wantVersions || len(snaps) != wantVersions {
		t.Fatalf("%d version directories %v and %d rows, want %d", len(dirs), dirs, len(snaps), wantVersions)
	}
	for _, s := range snaps {
		if s.Integrity != IntegrityOK || !slices.Contains(dirs, filepath.Base(s.Path)) {
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

func TestRunBackup(t *testing.T) {
	f := newFixture(t)
	job := f.newJob(t, false)
	res, err := f.run(t, job)
	if err != nil {
		t.Fatalf("run: %v\nlogs:\n%s", err, f.rep.text())
	}
	stats, ok := res.Stats.(Stats)
	if !ok || res.Warnings != 0 || stats.Files != 3 || stats.Integrity != IntegrityOK || stats.Method != MethodOnlineBackupImmutable ||
		stats.MetadataItems != 200 || stats.MediaParts != 200 || stats.SnapshotID == 0 {
		t.Fatalf("result %+v", res)
	}
	if !strings.Contains(res.Summary, "integrity ok") || !strings.Contains(res.Summary, `"NAS"`) {
		t.Fatalf("summary %q", res.Summary)
	}
	snaps := f.assertConverged(t, 1)
	s := snaps[0]
	wantPath := FolderName("Plex Server", f.integ.ID) + "/20260924T120000Z"
	if s.Path != wantPath || s.Path != stats.Path || s.IntegrationID != f.integ.ID || s.JobID != job.ID ||
		!s.CreatedAt.Equal(f.clock.Now()) || s.Method != MethodOnlineBackupImmutable {
		t.Fatalf("snapshot %+v, want path %s", s, wantPath)
	}
	m := f.checkVersion(t, s)
	if m.Format != 1 || m.JobID != job.ID || m.IntegrationID != f.integ.ID || m.Result != IntegrityOK ||
		m.PlexVersion != "1.43.4.10903-e5521bd8c" || m.SQLiteVersion == "" || !m.Integrity.OK() || m.Integrity.MetadataItems != 200 {
		t.Fatalf("manifest %+v", m)
	}
	if got, err := f.runner.Store().Get(f.ctx, s.ID); err != nil || got.Path != s.Path {
		t.Fatalf("Get: %+v, %v", got, err)
	}
	if _, err := f.runner.Store().Get(f.ctx, s.ID+100); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get unknown: %v", err)
	}

	items, err := f.jobs.ListItems(f.ctx, job.ID, jobqueue.ItemQuery{})
	if err != nil {
		t.Fatal(err)
	}
	var rels []string
	for _, it := range items.Records {
		if it.Action != jobs.ActionBackup || it.Status != jobs.ItemDone || it.Bytes == 0 {
			t.Fatalf("item %+v", it)
		}
		rels = append(rels, it.RelPath)
	}
	if !slices.Equal(rels, []string{DatabasesDir + "/" + LibraryDB, DatabasesDir + "/" + BlobsDB, PreferencesXML}) {
		t.Fatalf("items %v", rels)
	}
	if !logging.ContainsSecret(testToken) {
		t.Fatal("the PlexOnlineToken was not registered as a secret")
	}
	if strings.Contains(f.rep.text(), testToken) || strings.Contains(f.rep.text(), plexAPIToken) {
		t.Fatal("a token reached the job log")
	}
	for _, r := range f.plex.Requests() {
		if strings.Contains(r.RawQuery, plexAPIToken) {
			t.Fatalf("the token was sent in a URL: %s?%s", r.Path, r.RawQuery)
		}
	}
}

func TestRunDryRun(t *testing.T) {
	f := newFixture(t)
	// Plex is running: the library has a -wal.
	live := openRW(t, dbPath(f.data, LibraryDB))
	defer live.Close()
	if err := writeTx(live, 1); err != nil {
		t.Fatal(err)
	}
	before := dirSnapshot(t, f.target)
	dataBefore := dirSnapshot(t, f.data)
	job := f.newJob(t, true)
	res, err := f.run(t, job)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if got := dirSnapshot(t, f.target); !slices.Equal(got, before) {
		t.Fatalf("the dry run changed the destination:\nbefore %v\nafter  %v", before, got)
	}
	if got := dirSnapshot(t, f.data); !slices.Equal(got, dataBefore) {
		t.Fatal("the dry run changed Plex's directory")
	}
	if _, err := os.Stat(filepath.Join(f.config, StagingRoot)); !os.IsNotExist(err) {
		t.Fatalf("the dry run created a staging directory: %v", err)
	}
	if len(f.snapshots(t)) != 0 {
		t.Fatal("the dry run recorded a snapshot")
	}
	stats := res.Stats.(Stats)
	if !stats.DryRun || stats.Files != 3 || stats.Bytes == 0 || stats.Method != MethodOnlineBackup || res.Warnings != 0 ||
		!strings.HasPrefix(res.Summary, "Dry run: would back up 3 files") {
		t.Fatalf("result %+v", res)
	}
	items, err := f.jobs.ListItems(f.ctx, job.ID, jobqueue.ItemQuery{})
	if err != nil || len(items.Records) != 3 {
		t.Fatalf("items %+v, %v", items, err)
	}
	var d itemDetail
	if err := json.Unmarshal(items.Records[0].Detail, &d); err != nil {
		t.Fatal(err)
	}
	if items.Records[0].Status != jobs.ItemSkipped || !d.WALPresent || d.WALSize == 0 || d.Method != MethodOnlineBackup ||
		!strings.HasPrefix(d.Destination, FolderName(f.integ.Name, f.integ.ID)+"/") {
		t.Fatalf("library item %+v, detail %+v", items.Records[0], d)
	}

	// A dry run without Preferences.xml reports it (a warning), still without writing.
	if err := os.Remove(filepath.Join(f.data, PreferencesXML)); err != nil {
		t.Fatal(err)
	}
	res, err = f.run(t, f.newJob(t, true))
	if err != nil || res.Warnings != 1 || res.Stats.(Stats).Files != 2 {
		t.Fatalf("dry run without Preferences.xml: %+v, %v", res, err)
	}
	if got := dirSnapshot(t, f.target); !slices.Equal(got, before) {
		t.Fatal("the dry run changed the destination")
	}
}

func TestRunButlerWindow(t *testing.T) {
	f := newFixture(t)
	f.clock.Set(time.Date(2026, 9, 24, 3, 15, 0, 0, time.UTC))
	res, err := f.run(t, f.newJob(t, false))
	if err != nil || res.Warnings != 1 || !f.rep.has("maintenance window (02:00–05:00)") {
		t.Fatalf("result %+v, %v\nlogs:\n%s", res, err, f.rep.text())
	}
	f.assertConverged(t, 1)

	// Plex unreachable: no warning, no failure.
	f.plex.Close()
	f.clock.Set(time.Date(2026, 9, 25, 3, 15, 0, 0, time.UTC))
	res, err = f.run(t, f.newJob(t, false))
	if err != nil || res.Warnings != 0 {
		t.Fatalf("with Plex down: %+v, %v", res, err)
	}
	var m Manifest
	snaps := f.assertConverged(t, 2)
	if err := json.Unmarshal(snaps[0].Manifest, &m); err != nil || m.PlexVersion != "" {
		t.Fatalf("manifest %+v, %v", m, err)
	}
}

// restoreOnDelete is a job's item store whose DeleteItems, which the backup calls right before it
// asks Plex for its butler window, first runs restore.
type restoreOnDelete struct {
	jobs.ItemStore
	restore func()
}

func (r restoreOnDelete) DeleteItems(ctx context.Context, jobID int64) error {
	r.restore()
	return r.ItemStore.DeleteItems(ctx, jobID)
}

func TestRunSendsTheTokenOnlyToItsURL(t *testing.T) {
	// Someone who may change the integration points it at their server (with a token of their
	// own, which a URL change needs) and starts a backup; the owner restores the real URL and
	// token while it runs. The job loaded the other URL at its start: the real token, read later,
	// must not go there (S8), so the butler window is not checked.
	f := newFixture(t)
	other := plextest.NewServer(t, "")
	realURL := f.integ.URL
	if _, err := f.ints.Update(f.ctx, f.integ.ID, integrations.Input{Name: f.integ.Name, URL: other.URL, APIKey: "junk-token-0000000000"}); err != nil {
		t.Fatal(err)
	}
	job := f.newJob(t, false)
	items := restoreOnDelete{ItemStore: f.jobs, restore: func() {
		if _, err := f.ints.Update(f.ctx, f.integ.ID, integrations.Input{Name: f.integ.Name, URL: realURL, APIKey: plexAPIToken}); err != nil {
			t.Error(err)
		}
	}}
	res, err := f.runner.Run(f.ctx, job, jobs.Env{Reporter: f.rep, Items: items})
	for _, r := range other.Requests() {
		if r.Header.Get("X-Plex-Token") == plexAPIToken {
			t.Fatalf("the other server received the saved token: %s", r.Path)
		}
	}
	if err != nil || res.Warnings != 0 || !f.rep.has("the integration's URL changed") {
		t.Fatalf("result %+v, %v\nlogs:\n%s", res, err, f.rep.text())
	}
	f.assertConverged(t, 1)
}

func TestRunIntegrityFailure(t *testing.T) {
	f := newFixture(t)
	f.runOK(t) // a good version first
	damageLibrary(t, f.data)
	f.clock.Set(f.clock.Now().Add(24 * time.Hour))
	res, err := f.run(t, f.newJob(t, false))
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("err = %v", err)
	}
	if st := res.Stats.(Stats); st.Integrity != IntegrityFailed || st.SnapshotID == 0 || st.Pruned != 0 {
		t.Fatalf("stats %+v", st)
	}
	snaps := f.snapshots(t)
	if len(snaps) != 2 || snaps[0].Integrity != IntegrityFailed || snaps[1].Integrity != IntegrityOK {
		t.Fatalf("snapshots %+v", snaps)
	}
	var m Manifest
	if err := json.Unmarshal(snaps[0].Manifest, &m); err != nil || m.Result != IntegrityFailed || len(m.Integrity.Errors) == 0 {
		t.Fatalf("manifest %+v, %v", m, err)
	}
	if st, err := os.ReadDir(filepath.Join(f.config, StagingRoot)); err == nil && len(st) != 0 {
		t.Fatalf("staging left: %v", st)
	}
}

// damageLibrary zeroes a page in the middle of the source library: a copy carries the damage.
func damageLibrary(t *testing.T, dataDir string) {
	t.Helper()
	p := dbPath(dataDir, LibraryDB)
	size, count := pageGeometry(t, p)
	fh, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.WriteAt(make([]byte, size), (count/2)*size); err != nil {
		t.Fatal(err)
	}
	if err := fh.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestResumeAfterRecordingAFailedVersion(t *testing.T) {
	// A job that recorded a version that failed verification and crashed before it returned
	// ends as it would have: failed with ErrIntegrity, without backing up again.
	f := newFixture(t)
	damageLibrary(t, f.data)
	job := f.newJob(t, false)
	f.crashRun(t, job, PointAfterRecord)
	job.Attempt, job.Trigger = 2, jobs.TriggerResume
	f.clock.Set(f.clock.Now().Add(time.Minute))
	res, err := f.run(t, job)
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("err = %v\nlogs:\n%s", err, f.rep.text())
	}
	snaps := f.snapshots(t)
	if len(snaps) != 1 || snaps[0].Integrity != IntegrityFailed || snaps[0].JobID != job.ID {
		t.Fatalf("snapshots %+v", snaps)
	}
	if st := res.Stats.(Stats); st.SnapshotID != snaps[0].ID || st.Integrity != IntegrityFailed || st.Files != 3 || st.Pruned != 0 {
		t.Fatalf("stats %+v", st)
	}
	if !strings.Contains(err.Error(), LibraryDB+": ") || !strings.Contains(res.Summary, "failed verification") {
		t.Fatalf("err %q, summary %q", err, res.Summary)
	}
	if st, err := os.ReadDir(filepath.Join(f.config, StagingRoot)); err == nil && len(st) != 0 {
		t.Fatalf("staging left: %v", st)
	}
}

func TestResumeWithADamagedOwnVersion(t *testing.T) {
	// A version the crashed attempt renamed into place whose files no longer match its manifest
	// is recorded as failed, and the resumed job backs up again.
	f := newFixture(t)
	job := f.newJob(t, false)
	f.crashRun(t, job, PointBeforeRecord)
	orphan := f.entries(t)[0]
	if err := os.WriteFile(filepath.Join(f.folderDir(), orphan, BlobsDB), []byte("damaged"), 0o644); err != nil {
		t.Fatal(err)
	}
	job.Attempt, job.Trigger = 2, jobs.TriggerResume
	f.clock.Set(f.clock.Now().Add(time.Minute))
	res, err := f.run(t, job)
	if err != nil {
		t.Fatalf("resume: %v\nlogs:\n%s", err, f.rep.text())
	}
	snaps := f.snapshots(t)
	if len(snaps) != 2 || snaps[0].Integrity != IntegrityOK || filepath.Base(snaps[1].Path) != orphan || snaps[1].Integrity != IntegrityFailed {
		t.Fatalf("snapshots %+v", snaps)
	}
	if st := res.Stats.(Stats); st.SnapshotID != snaps[0].ID || st.Recovered != 1 {
		t.Fatalf("stats %+v", st)
	}
	f.checkVersion(t, snaps[0])
}

func TestReusedJobIDBacksUp(t *testing.T) {
	// Job ids start over with a new Bunkarr database while the destination keeps versions whose
	// manifests name the old ids. A job with such an id backs up as any other: on its first
	// attempt and when resumed, it neither finishes with the old version nor fails with its
	// integrity.
	for _, tc := range []struct {
		name          string
		oldFailed     bool // the old version failed verification
		crashedBefore bool // the new job crashed before its rename and is resumed
	}{
		{"first attempt", false, false},
		{"first attempt, the old version failed", true, false},
		{"resumed", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			lib := dbPath(f.data, LibraryDB)
			good, err := os.ReadFile(lib)
			if err != nil {
				t.Fatal(err)
			}
			for range 4 {
				f.otherJob(t)
			}
			if tc.oldFailed {
				damageLibrary(t, f.data)
			}
			old := f.newJob(t, false)
			if _, err := f.run(t, old); (err != nil) != tc.oldFailed {
				t.Fatalf("the old install's job: %v", err)
			}
			if err := os.WriteFile(lib, good, 0o644); err != nil {
				t.Fatal(err)
			}
			f.wipeHistory(t)
			f.clock.Set(f.clock.Now().AddDate(0, 0, 1))
			f.runOK(t) // the new install's first job records the old version and makes its own
			for range 3 {
				f.otherJob(t)
			}
			f.clock.Set(f.clock.Now().AddDate(0, 0, 1))
			job := f.newJob(t, false)
			if job.ID != old.ID {
				t.Fatalf("job id %d, want %d", job.ID, old.ID)
			}
			if tc.crashedBefore {
				f.crashRun(t, job, PointBeforeRename)
				job.Attempt, job.Trigger = 2, jobs.TriggerResume
			}
			before := len(f.snapshots(t))
			res, err := f.run(t, job)
			if err != nil {
				t.Fatalf("run: %v\nlogs:\n%s", err, f.rep.text())
			}
			snaps := f.snapshots(t)
			if len(snaps) != before+1 || snaps[0].JobID != job.ID || !snaps[0].CreatedAt.Equal(f.clock.Now()) ||
				res.Stats.(Stats).SnapshotID != snaps[0].ID {
				t.Fatalf("no new version: %d rows before, snapshots %+v, stats %+v", before, snaps, res.Stats)
			}
			f.checkVersion(t, snaps[0])
		})
	}
}

func TestResumeReportsTheFirstAttemptsWarnings(t *testing.T) {
	// A job resumed after its version was renamed into place (recorded or not) finishes with that
	// version and reports the warnings the attempt that wrote it had: Preferences.xml not backed
	// up, the backup ran inside Plex's maintenance window.
	for _, point := range []string{PointAfterRename, PointAfterRecord} {
		t.Run(point, func(t *testing.T) {
			f := newFixture(t)
			if err := os.Remove(filepath.Join(f.data, PreferencesXML)); err != nil {
				t.Fatal(err)
			}
			f.clock.Set(time.Date(2026, 9, 24, 3, 15, 0, 0, time.UTC))
			job := f.newJob(t, false)
			f.crashRun(t, job, point)
			crashedAt := f.clock.Now()

			f.rep = &recReporter{}
			job.Attempt, job.Trigger = 2, jobs.TriggerResume
			f.clock.Set(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)) // outside the window
			res, err := f.run(t, job)
			if err != nil {
				t.Fatalf("resume: %v\nlogs:\n%s", err, f.rep.text())
			}
			snaps := f.snapshots(t)
			if len(snaps) != 1 || !snaps[0].CreatedAt.Equal(crashedAt) || res.Stats.(Stats).Files != 2 {
				t.Fatalf("snapshots %+v, stats %+v", snaps, res.Stats)
			}
			if res.Warnings != 2 || !f.rep.has("maintenance window (02:00–05:00)") ||
				!f.rep.has("Preferences.xml was not backed up: it does not exist") {
				t.Fatalf("%d warnings\nlogs:\n%s", res.Warnings, f.rep.text())
			}
		})
	}
}

// runOK runs a backup job that must succeed.
func (f *fixture) runOK(t *testing.T) jobs.Result {
	t.Helper()
	res, err := f.run(t, f.newJob(t, false))
	if err != nil {
		t.Fatalf("run: %v\nlogs:\n%s", err, f.rep.text())
	}
	return res
}

func TestRunPrunes(t *testing.T) {
	f := newFixture(t)
	if _, err := f.dests.Update(f.ctx, f.dest.ID, destinations.Input{Retention: &destinations.Retention{PlexDBDaily: 3, PlexDBWeekly: 2}}); err != nil {
		t.Fatal(err)
	}
	// Things pruning must never touch: a directory without a row and the live tree.
	stray := FolderName(f.integ.Name, f.integ.ID) + "/20200101T000000Z"
	mkVersion(t, f.target, stray)
	mkVersion(t, f.target, "movies/20200101T000000Z")
	// A symlink named like a version, pointing into the live tree.
	if err := os.Symlink("../../../movies/20200101T000000Z", filepath.Join(f.folderDir(), "20190101T000000Z")); err != nil {
		t.Fatal(err)
	}

	// Ten daily backups from Monday 2026-09-07 (ISO week 37) to Wednesday 09-16 (week 38).
	start := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	var pruned int64
	for day := range 10 {
		f.clock.Set(start.AddDate(0, 0, day))
		pruned += f.runOK(t).Stats.(Stats).Pruned
	}
	// Kept: the 3 newest (09-16, 09-15, 09-14, all week 38) and the newest of week 37 (09-13).
	var got []string
	for _, s := range f.snapshots(t) {
		got = append(got, filepath.Base(s.Path))
	}
	want := []string{"20260916T120000Z", "20260915T120000Z", "20260914T120000Z", "20260913T120000Z"}
	if !slices.Equal(got, want) || pruned != 6 {
		t.Fatalf("kept %v (pruned %d), want %v", got, pruned, want)
	}
	var dirs []string
	for _, e := range f.entries(t) {
		dirs = append(dirs, e)
	}
	if !slices.Equal(dirs, []string{"20190101T000000Z", "20200101T000000Z", "20260913T120000Z", "20260914T120000Z", "20260915T120000Z", "20260916T120000Z"}) {
		t.Fatalf("directories %v", dirs)
	}
	if !exists(t, filepath.Join(f.target, "movies/20200101T000000Z", LibraryDB)) {
		t.Fatal("pruning touched the live tree")
	}
	if !f.rep.has("An unrecorded Plex DB version was left alone") || !f.rep.has("20190101T000000Z reason not a directory") {
		t.Fatalf("the stray directory was not reported:\n%s", f.rep.text())
	}
}

func TestRunPreflightFailures(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, f *fixture) jobs.Job
		want  string
		isErr error
	}{
		{"no integration", func(t *testing.T, f *fixture) jobs.Job {
			j := f.newJob(t, false)
			j.Params.IntegrationID = 0
			return j
		}, "needs an integrationId", nil},
		{"unknown integration", func(t *testing.T, f *fixture) jobs.Job {
			j := f.newJob(t, false)
			j.Params.IntegrationID = 999
			return j
		}, "", integrations.ErrNotFound},
		{"integration disabled", func(t *testing.T, f *fixture) jobs.Job {
			disabled := false
			if _, err := f.ints.Update(f.ctx, f.integ.ID, integrations.Input{Name: f.integ.Name, URL: f.integ.URL, Enabled: &disabled}); err != nil {
				t.Fatal(err)
			}
			return f.newJob(t, false)
		}, "is disabled", nil},
		{"no data path", func(t *testing.T, f *fixture) jobs.Job {
			if _, err := f.ints.Update(f.ctx, f.integ.ID, integrations.Input{Name: f.integ.Name, URL: f.integ.URL, Settings: json.RawMessage(`{"dataPath":""}`)}); err != nil {
				t.Fatal(err)
			}
			return f.newJob(t, false)
		}, "no data path", nil},
		{"no destination", func(t *testing.T, f *fixture) jobs.Job {
			j := f.newJob(t, false)
			j.Params.DestinationID = 0
			return j
		}, "no backup destination", nil},
		{"destination disabled", func(t *testing.T, f *fixture) jobs.Job {
			disabled := false
			if _, err := f.dests.Update(f.ctx, f.dest.ID, destinations.Input{Enabled: &disabled}); err != nil {
				t.Fatal(err)
			}
			return f.newJob(t, false)
		}, "is disabled", nil},
		{"destination not mounted", func(t *testing.T, f *fixture) jobs.Job {
			if err := os.Remove(filepath.Join(f.target, filecopy.MarkerRel)); err != nil {
				t.Fatal(err)
			}
			return f.newJob(t, false)
		}, "", destinations.ErrNotMounted},
		{"no library database", func(t *testing.T, f *fixture) jobs.Job {
			if err := os.Remove(dbPath(f.data, LibraryDB)); err != nil {
				t.Fatal(err)
			}
			return f.newJob(t, false)
		}, "", ErrNoDatabase},
	}
	for _, tt := range tests {
		for _, dry := range []bool{false, true} {
			t.Run(tt.name+map[bool]string{false: "", true: " (dry run)"}[dry], func(t *testing.T) {
				f := newFixture(t)
				job := tt.setup(t, f)
				job.DryRun = dry
				before := dirSnapshot(t, f.target)
				_, err := f.run(t, job)
				if err == nil || (tt.isErr != nil && !errors.Is(err, tt.isErr)) || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("err = %v, want %q / %v", err, tt.want, tt.isErr)
				}
				if got := dirSnapshot(t, f.target); !slices.Equal(got, before) {
					t.Fatalf("a failed preflight wrote to the destination:\nbefore %v\nafter  %v", before, got)
				}
				if st, err := os.ReadDir(filepath.Join(f.config, StagingRoot)); err == nil && len(st) != 0 {
					t.Fatalf("staging left: %v", st)
				}
			})
		}
	}
}

func TestRunCancelled(t *testing.T) {
	for _, point := range []string{PointAfterStage, PointAfterCopy} {
		t.Run(point, func(t *testing.T) {
			f := newFixture(t)
			ctx, cancel := context.WithCancel(f.ctx)
			defer cancel()
			faultinject.SetHook(func(name string) {
				if name == point {
					cancel()
				}
			})
			defer faultinject.SetHook(nil)
			_, err := f.runner.Run(ctx, f.newJob(t, false), jobs.Env{Reporter: f.rep, Items: f.jobs})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v", err)
			}
			f.assertConverged(t, 0)
		})
	}
}

func TestRunDefaultsToTheIntegrationsDestination(t *testing.T) {
	f := newFixture(t)
	settings, _ := json.Marshal(map[string]any{"dataPath": f.data, "backup": map[string]any{"destinationId": f.dest.ID}})
	if _, err := f.ints.Update(f.ctx, f.integ.ID, integrations.Input{Name: f.integ.Name, URL: f.integ.URL, Settings: settings}); err != nil {
		t.Fatal(err)
	}
	job := f.newJob(t, false)
	job.Params.DestinationID = 0
	if _, err := f.run(t, job); err != nil {
		t.Fatal(err)
	}
	f.assertConverged(t, 1)
}

func TestRunWithoutManager(t *testing.T) {
	// A test binary can drive the runner without a job manager: no reporter, no item store.
	f := newFixture(t)
	res, err := f.runner.Run(f.ctx, f.newJob(t, false), jobs.Env{})
	if err != nil || res.Stats.(Stats).Files != 3 {
		t.Fatalf("%+v, %v", res, err)
	}
	f.assertConverged(t, 1)
}

// crashRun runs job and expects it to stop with a faultinject.Crash at point.
func (f *fixture) crashRun(t *testing.T, job jobs.Job, point string) {
	t.Helper()
	faultinject.SetHook(faultinject.CrashAt(point, 1))
	defer faultinject.SetHook(nil)
	defer func() {
		p := recover()
		if c, ok := p.(faultinject.Crash); !ok || c.Point != point {
			t.Fatalf("the run did not crash at %s: %v", point, p)
		}
	}()
	_, _ = f.run(t, job)
}

// crashPoints are the step boundaries of a backup, in order: the plexdb points and the filecopy
// engine's points inside them.
var crashPoints = []string{
	PointAfterStage, "copy.afterWrite", "copy.afterRename", PointAfterCopy, PointBeforeRename,
	"move.afterRename", PointAfterRename, PointBeforeRecord, PointAfterRecord,
}

func TestCrashMatrixResume(t *testing.T) {
	for i, point := range crashPoints {
		t.Run(point, func(t *testing.T) {
			f := newFixture(t)
			job := f.newJob(t, false)
			f.crashRun(t, job, point)
			crashedAt := f.clock.Now()

			job.Attempt, job.Trigger = 2, jobs.TriggerResume
			f.clock.Set(f.clock.Now().Add(time.Minute))
			res, err := f.run(t, job)
			if err != nil {
				t.Fatalf("resume: %v\nlogs:\n%s", err, f.rep.text())
			}
			snaps := f.assertConverged(t, 1)
			stats := res.Stats.(Stats)
			if stats.SnapshotID != snaps[0].ID || snaps[0].JobID != job.ID {
				t.Fatalf("stats %+v, snapshot %+v", stats, snaps[0])
			}
			// move.afterRename is the filecopy point right after the version directory's rename.
			// From there on the version is complete: the resume finishes with it (recording it
			// when the crash came before the insert) and takes no second backup.
			renamed := i >= slices.Index(crashPoints, "move.afterRename")
			wantCreated := f.clock.Now()
			if renamed {
				wantCreated = crashedAt
			}
			if !snaps[0].CreatedAt.Equal(wantCreated) {
				t.Fatalf("the version was made at %s, want %s (renamed: %v)", snaps[0].CreatedAt, wantCreated, renamed)
			}
			adopted := renamed && point != PointAfterRecord
			if adopted != (stats.Recovered == 1) {
				t.Fatalf("recovered %d after a crash at %s", stats.Recovered, point)
			}
			items, err := f.jobs.ListItems(f.ctx, job.ID, jobqueue.ItemQuery{})
			if err != nil || len(items.Records) != 3 {
				t.Fatalf("items %+v, %v", items, err)
			}
			for _, it := range items.Records {
				if it.Status != jobs.ItemDone {
					t.Fatalf("item %+v after the resume", it)
				}
			}
		})
	}
}

// setRetention sets the destination's Plex DB version retention.
func (f *fixture) setRetention(t *testing.T, daily, weekly int) {
	t.Helper()
	if _, err := f.dests.Update(f.ctx, f.dest.ID, destinations.Input{Retention: &destinations.Retention{PlexDBDaily: daily, PlexDBWeekly: weekly}}); err != nil {
		t.Fatal(err)
	}
}

func TestCrashMatrixPrune(t *testing.T) {
	// A job crashes while pruning the version of the day before, and is resumed with the same
	// retention or after the retention was raised (the version is kept after all). Every row
	// must name a complete version directory, nothing may be left under a "." name, and the
	// resume takes no second backup.
	for _, point := range []string{PointPruneAfterTrash, PointPruneAfterUnrecord, PointPruneAfterRemove} {
		for _, raised := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s raised=%v", point, raised), func(t *testing.T) {
				f := newFixture(t)
				f.setRetention(t, 1, 1)
				f.runOK(t)
				f.clock.Set(f.clock.Now().AddDate(0, 0, 1))
				job := f.newJob(t, false)
				f.crashRun(t, job, point)
				crashedAt := f.clock.Now()
				if raised {
					f.setRetention(t, 14, 8)
				}

				job.Attempt, job.Trigger = 2, jobs.TriggerResume
				f.clock.Set(f.clock.Now().Add(time.Minute))
				res, err := f.run(t, job)
				if err != nil {
					t.Fatalf("resume: %v\nlogs:\n%s", err, f.rep.text())
				}
				// Only a version whose deletion had not been committed (its row still there) can
				// be kept after all.
				want := 1
				if raised && point == PointPruneAfterTrash {
					want = 2
				}
				snaps := f.assertConverged(t, want)
				if snaps[0].JobID != job.ID || !snaps[0].CreatedAt.Equal(crashedAt) || res.Stats.(Stats).SnapshotID != snaps[0].ID {
					t.Fatalf("snapshots %+v, stats %+v", snaps, res.Stats)
				}
			})
		}
	}
}

func TestPruneDropsALostVersion(t *testing.T) {
	// The row of a version a prune had deleted comes back (its delete was lost, or Bunkarr's
	// database was restored from an older copy) while the retention now keeps the version:
	// what is left of its ".prune-*" directory is not restored under its name, and its row does
	// not take a retention slot: it is removed, and a version the retention keeps stays.
	for _, point := range []string{PointPruneAfterUnrecord, PointPruneAfterRemove} {
		t.Run(point, func(t *testing.T) {
			f := newFixture(t)
			f.setRetention(t, 1, 1)
			f.runOK(t)
			lost := f.snapshots(t)[0]
			f.clock.Set(f.clock.Now().AddDate(0, 0, 1))
			job := f.newJob(t, false)
			f.crashRun(t, job, point)
			if point == PointPruneAfterUnrecord {
				// The removal of the ".prune-*" directory had begun.
				trash := filepath.Join(f.target, filepath.FromSlash(trashPath(lost.Path)))
				if err := os.Remove(filepath.Join(trash, BlobsDB)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.runner.Store().insert(f.ctx, lost); err != nil {
				t.Fatal(err)
			}
			f.setRetention(t, 14, 8)
			job.Attempt, job.Trigger = 2, jobs.TriggerResume
			f.clock.Set(f.clock.Now().Add(time.Minute))
			res, err := f.run(t, job)
			if err != nil {
				t.Fatalf("resume: %v\nlogs:\n%s", err, f.rep.text())
			}
			snaps := f.assertConverged(t, 1)
			if snaps[0].JobID != job.ID || res.Warnings != 1 || !f.rep.has("missing or incomplete at the destination") {
				t.Fatalf("snapshots %+v, %d warnings\nlogs:\n%s", snaps, res.Warnings, f.rep.text())
			}
		})
	}
}

func TestPruneFinishesDeletingADamagedFailedVersion(t *testing.T) {
	// A version recorded as failed because its files do not match its manifest (an interrupted
	// backup's version, damaged before a later job recorded it) is pruned after FailedKeep; the
	// prune stops between the rename and the row delete. The resumed job finishes that prune: the
	// version was not lost, its files never matched the manifest.
	f := newFixture(t)
	first := f.newJob(t, false)
	f.crashRun(t, first, PointBeforeRecord)
	orphan := f.entries(t)[0]
	if err := os.WriteFile(filepath.Join(f.folderDir(), orphan, BlobsDB), []byte("damaged"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.removeStaging(t, first.ID)
	f.clock.Set(f.clock.Now().Add(time.Minute))
	f.runOK(t) // records the damaged version as failed and makes its own
	if snaps := f.snapshots(t); len(snaps) != 2 || filepath.Base(snaps[1].Path) != orphan || snaps[1].Integrity != IntegrityFailed {
		t.Fatalf("snapshots %+v", snaps)
	}
	f.clock.Set(f.clock.Now().AddDate(0, 0, 8))
	job := f.newJob(t, false)
	f.crashRun(t, job, PointPruneAfterTrash)
	job.Attempt, job.Trigger = 2, jobs.TriggerResume
	f.clock.Set(f.clock.Now().Add(time.Minute))
	res, err := f.run(t, job)
	if err != nil {
		t.Fatalf("resume: %v\nlogs:\n%s", err, f.rep.text())
	}
	snaps := f.assertConverged(t, 2)
	if snaps[0].JobID != job.ID || res.Warnings != 0 || res.Stats.(Stats).Pruned != 1 || f.rep.has("missing or incomplete") {
		t.Fatalf("snapshots %+v, result %+v\nlogs:\n%s", snaps, res, f.rep.text())
	}
}

func TestPruneDoesNotCountAMissingVersion(t *testing.T) {
	// A recorded version whose directory is gone from the destination does not take a retention
	// slot: its row is removed and the older version the retention keeps stays.
	f := newFixture(t)
	f.runOK(t)
	f.clock.Set(f.clock.Now().AddDate(0, 0, 1))
	f.runOK(t)
	missing := f.snapshots(t)[0]
	if err := os.RemoveAll(filepath.Join(f.target, filepath.FromSlash(missing.Path))); err != nil {
		t.Fatal(err)
	}
	f.setRetention(t, 2, 0)
	f.clock.Set(f.clock.Now().AddDate(0, 0, 1))
	res := f.runOK(t)
	snaps := f.assertConverged(t, 2)
	if snaps[1].CreatedAt.Equal(missing.CreatedAt) || res.Warnings != 1 || res.Stats.(Stats).Pruned != 0 {
		t.Fatalf("snapshots %+v, result %+v", snaps, res)
	}
}

func TestRunIntegrationDeletedDuringTheBackup(t *testing.T) {
	// The integration is deleted while its backup runs: the version, already complete at its
	// name, is recorded without the integration link (as the integration's other versions are
	// kept), never left unrecorded where no later job would record or prune it.
	f := newFixture(t)
	faultinject.SetHook(func(name string) {
		if name == PointBeforeRecord {
			if err := f.ints.Delete(f.ctx, f.integ.ID); err != nil {
				t.Error(err)
			}
		}
	})
	defer faultinject.SetHook(nil)
	res, err := f.run(t, f.newJob(t, false))
	if err != nil {
		t.Fatalf("run: %v\nlogs:\n%s", err, f.rep.text())
	}
	snaps := f.assertConverged(t, 1)
	if snaps[0].IntegrationID != 0 || res.Stats.(Stats).SnapshotID != snaps[0].ID || res.Warnings != 1 ||
		!f.rep.has("deleted during the backup") {
		t.Fatalf("snapshot %+v, result %+v\nlogs:\n%s", snaps[0], res, f.rep.text())
	}
}

func TestRunRemovesAnUnrecordedPruneLeftover(t *testing.T) {
	// ".prune-<version>" without a row is what a prune leaves when it stops after deleting the
	// row: the next backup removes it. (One whose version is still recorded is left to pruning:
	// TestCrashMatrixPrune.)
	f := newFixture(t)
	f.runOK(t)
	kept := f.entries(t)[0]
	leftover := trashPath(FolderName(f.integ.Name, f.integ.ID) + "/20260101T000000Z")
	mkVersion(t, f.target, leftover)
	f.clock.Set(f.clock.Now().Add(time.Hour))
	f.runOK(t)
	if exists(t, filepath.Join(f.target, filepath.FromSlash(leftover))) {
		t.Fatalf("%s was not removed", leftover)
	}
	snaps := f.assertConverged(t, 2)
	if filepath.Base(snaps[1].Path) != kept {
		t.Fatalf("snapshots %+v", snaps)
	}
	if !f.rep.has("interrupted prune") {
		t.Fatalf("the removal was not logged:\n%s", f.rep.text())
	}
}

func TestCrashMatrixManager(t *testing.T) {
	// The same crashes through the real job manager: the crashed job is recovered at the next
	// start (attempt 2, trigger resume) and completes.
	for _, point := range []string{PointAfterStage, PointBeforeRename, PointAfterRename, PointAfterRecord} {
		t.Run(point, func(t *testing.T) {
			f := newFixture(t)
			crashed := make(chan struct{})
			hook := faultinject.CrashAt(point, 1)
			var once sync.Once
			faultinject.SetHook(func(name string) {
				defer func() {
					if p := recover(); p != nil {
						once.Do(func() { close(crashed) })
						panic(p)
					}
				}()
				hook(name)
			})
			defer faultinject.SetHook(nil)

			m1 := jobqueue.New(f.db, nil, jobqueue.Options{Workers: 1})
			m1.Register(jobs.TypePlexDBBackup, f.runner)
			if err := m1.Start(f.ctx); err != nil {
				t.Fatal(err)
			}
			job, err := m1.Enqueue(f.ctx, jobs.Spec{Type: jobs.TypePlexDBBackup, Params: jobs.Params{IntegrationID: f.integ.ID, DestinationID: f.dest.ID}})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-crashed:
			case <-time.After(30 * time.Second):
				t.Fatal("the job did not reach the crash point")
			}
			stopCtx, cancel := context.WithTimeout(f.ctx, 30*time.Second)
			defer cancel()
			waitUntil(t, "the simulated crash", func() bool {
				j, err := f.jobs.GetJob(f.ctx, job.ID)
				return err == nil && j.Status == jobs.StatusRunning
			})
			if err := m1.Stop(stopCtx); err != nil {
				t.Fatal(err)
			}
			faultinject.SetHook(nil)

			m2 := jobqueue.New(f.db, nil, jobqueue.Options{Workers: 1})
			m2.Register(jobs.TypePlexDBBackup, f.runner)
			if err := m2.Start(f.ctx); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = m2.Stop(stopCtx) }()
			var final jobs.Job
			deadline := time.Now().Add(30 * time.Second)
			for {
				final, err = f.jobs.GetJob(f.ctx, job.ID)
				if err != nil {
					t.Fatal(err)
				}
				if final.Status.Final() {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("the resumed job did not finish: %+v", final)
				}
				time.Sleep(5 * time.Millisecond)
			}
			if final.Status != jobs.StatusCompleted || final.Attempt != 2 || final.Trigger != jobs.TriggerResume {
				t.Fatalf("job %+v", final)
			}
			f.assertConverged(t, 1)
		})
	}
}

func TestRunRecordsAnotherJobsOrphan(t *testing.T) {
	// A job that crashed after its rename and failed for good leaves a complete, unrecorded
	// version; the next job records it and makes its own.
	f := newFixture(t)
	first := f.newJob(t, false)
	f.crashRun(t, first, PointAfterRename)
	// A job that never resumes leaves its staging directory until the stale cleanup (a day).
	f.removeStaging(t, first.ID)
	f.clock.Set(f.clock.Now().Add(time.Hour))
	res := f.runOK(t)
	if res.Stats.(Stats).Recovered != 1 {
		t.Fatalf("stats %+v", res.Stats)
	}
	snaps := f.assertConverged(t, 2)
	var m Manifest
	if err := json.Unmarshal(snaps[1].Manifest, &m); err != nil || m.JobID != first.ID {
		t.Fatalf("the older version's manifest names job %d, want %d (%v)", m.JobID, first.ID, err)
	}

	// A damaged unrecorded version is recorded as failed (and pruned after 7 days), never
	// deleted without a record.
	third := f.newJob(t, false)
	f.crashRun(t, third, PointBeforeRecord)
	f.removeStaging(t, third.ID)
	var orphan string
	for _, e := range f.entries(t) {
		if !slices.ContainsFunc(snaps, func(s Snapshot) bool { return filepath.Base(s.Path) == e }) {
			orphan = e
		}
	}
	if err := os.WriteFile(filepath.Join(f.folderDir(), orphan, BlobsDB), []byte("damaged"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.clock.Set(f.clock.Now().Add(time.Hour))
	f.runOK(t)
	var failed []string
	for _, s := range f.snapshots(t) {
		if s.Integrity == IntegrityFailed {
			failed = append(failed, filepath.Base(s.Path))
		}
	}
	if !slices.Equal(failed, []string{orphan}) {
		t.Fatalf("failed versions %v, want [%s]", failed, orphan)
	}
	// Eight days later it is pruned.
	f.clock.Set(f.clock.Now().AddDate(0, 0, 8))
	f.runOK(t)
	for _, s := range f.snapshots(t) {
		if s.Integrity == IntegrityFailed {
			t.Fatalf("the failed version %s was not pruned after 7 days", s.Path)
		}
	}
	if exists(t, filepath.Join(f.folderDir(), orphan)) {
		t.Fatal("the pruned version's directory still exists")
	}
}

// removeStaging removes a job's staging directory.
func (f *fixture) removeStaging(t *testing.T, jobID int64) {
	t.Helper()
	if err := os.RemoveAll(f.runner.StagingDir(jobID)); err != nil {
		t.Fatal(err)
	}
}

func TestCleanStaleStaging(t *testing.T) {
	f := newFixture(t)
	now := f.clock.Now()
	for name, age := range map[string]time.Duration{
		"plexdb-job5": 48 * time.Hour, // stale: removed
		"plexdb-job6": time.Hour,      // another job may still use it: kept
		"plexdb-job7": 48 * time.Hour, // the running job's own: kept
		"other-dir":   48 * time.Hour, // not Bunkarr's plexdb staging: kept
	} {
		dir := filepath.Join(f.config, StagingRoot, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, LibraryDB), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		touch(t, dir, now.Add(-age))
	}
	f.runner.cleanStaleStaging(7)
	var left []string
	es, err := os.ReadDir(filepath.Join(f.config, StagingRoot))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range es {
		left = append(left, e.Name())
	}
	if !slices.Equal(left, []string{"other-dir", "plexdb-job6", "plexdb-job7"}) {
		t.Fatalf("left %v", left)
	}
}

func TestFolderName(t *testing.T) {
	for _, tt := range []struct {
		name string
		id   int64
		want string
	}{
		{"Plex Server", 1, ".bunkarr/plex/plex-server-1"},
		{"  Wohnzimmer (4K)!! ", 12, ".bunkarr/plex/wohnzimmer-4k-12"},
		{"千と千尋", 3, ".bunkarr/plex/plex-3"},
		{"../../etc", 4, ".bunkarr/plex/etc-4"},
		{strings.Repeat("a", 60), 5, ".bunkarr/plex/" + strings.Repeat("a", 40) + "-5"},
	} {
		if got := FolderName(tt.name, tt.id); got != tt.want {
			t.Errorf("FolderName(%q, %d) = %q, want %q", tt.name, tt.id, got, tt.want)
		}
		if got := folderIntegrationID(strings.TrimPrefix(tt.want, PlexRoot+"/")); got != tt.id {
			t.Errorf("folderIntegrationID(%q) = %d", tt.want, got)
		}
	}
	for _, base := range []string{"plex", ".partial-job3", "plex-x", "plex-0", ""} {
		if folderIntegrationID(base) != 0 {
			t.Errorf("folderIntegrationID(%q) != 0", base)
		}
	}
}
