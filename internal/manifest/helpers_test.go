package manifest

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/config"
	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr/arrtest"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/mediaindex"
)

// arrKey is the API key of the fake *arrs.
const arrKey = "0123456789abcdef0123456789abcdef"

// testClock is a settable time source.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

func (c *testClock) Advance(d time.Duration) { c.Set(c.Now().Add(d)) }

// testEnv is a database with a source over a media root holding the recorded *arrs' files (sparse
// files of the recorded sizes) plus a sidecar, an extra and a file of no item; fake Radarr (with
// the unmapped /movies-4k root folder) and Sonarr integrations whose index was refreshed from the
// recordings; a destination linked to the source; and the manifest runner.
type testEnv struct {
	t       *testing.T
	ctx     context.Context
	db      *db.DB
	ints    *integrations.Store
	cat     *catalog.Store
	scanner *catalog.Scanner
	dests   *destinations.Store
	index   *mediaindex.Runner
	runner  *Runner
	tiers   *fakeTiers
	clock   *testClock
	base    string
	root    string // media root: the *arrs' /movies and /tv are <root>/movies and <root>/tv
	target  string
	config  string
	src     catalog.Source
	dest    destinations.Destination
	radarr  *arrtest.Server
	sonarr  *arrtest.Server
	rad     integrations.Integration
	son     integrations.Integration
	jobs    int64
}

// Paths of the extra files newEnv adds to the fixtures' files.
const (
	movieDir    = "movies/Night of the Living Dead (1968)"
	movieFile   = movieDir + "/Night of the Living Dead (1968) [Bluray-1080p].mkv"
	sidecarFile = movieDir + "/Night of the Living Dead (1968) [Bluray-1080p].en.srt"
	posterFile  = movieDir + "/poster.jpg"
	otherFile   = "home videos/birthday.mp4"
)

func resolvedTemp(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	ctx := context.Background()
	e := &testEnv{t: t, ctx: ctx, clock: &testClock{now: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)}, tiers: &fakeTiers{}}
	e.base = resolvedTemp(t)
	e.root = filepath.Join(e.base, "media")
	e.target = filepath.Join(e.base, "target")
	e.config = filepath.Join(e.base, "config")
	for _, d := range []string{e.root, e.target, e.config, filepath.Join(e.root, "movies"), filepath.Join(e.root, "tv")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	if e.db, err = db.Open(ctx, filepath.Join(e.config, "bunkarr.db"), nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.db.Close() })
	kr, err := config.NewKeyring(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	e.ints = integrations.NewStore(e.db, kr)
	e.cat = catalog.NewStore(e.db, catalog.StoreOptions{})
	e.scanner = catalog.NewScanner(e.cat, catalog.ScannerOptions{})
	e.dests = destinations.New(e.db, destinations.Options{})

	e.radarr = arrtest.NewServer(t, arr.KindRadarr, arrKey)
	e.radarr.UseMovies4K()
	e.sonarr = arrtest.NewServer(t, arr.KindSonarr, arrKey)
	for p, size := range fixtureFiles(t, arr.KindRadarr, "movie-movies-4k.json") {
		if strings.HasPrefix(p, "/movies/") {
			e.writeFile(strings.TrimPrefix(p, "/"), size)
		}
	}
	for p, size := range fixtureFiles(t, arr.KindSonarr, "") {
		e.writeFile(strings.TrimPrefix(p, "/"), size)
	}
	e.writeFile(sidecarFile, 1200)
	e.writeFile(posterFile, 3000)
	e.writeFile(otherFile, 5000)

	if e.src, err = e.cat.Create(ctx, catalog.SourceInput{Name: "Media", Path: e.root}); err != nil {
		t.Fatal(err)
	}
	e.scan()
	e.rad = e.createArr(integrations.TypeRadarr, "Radarr", e.radarr.URL, map[string]string{"/movies": filepath.Join(e.root, "movies")})
	e.son = e.createArr(integrations.TypeSonarr, "Sonarr", e.sonarr.URL, map[string]string{"/tv": filepath.Join(e.root, "tv")})
	if e.index, err = mediaindex.NewRunner(mediaindex.RefreshOptions{DB: e.db, Integrations: e.ints, Catalog: e.cat, Now: e.clock.Now}); err != nil {
		t.Fatal(err)
	}
	e.refresh(e.rad.ID)
	e.refresh(e.son.ID)
	if e.dest, err = e.dests.Create(ctx, destinations.Input{Name: "UNAS", Target: e.target, SourceIDs: []int64{e.src.ID}},
		destinations.CreateOptions{AllowLocal: true}); err != nil {
		t.Fatal(err)
	}
	e.runner = e.newRunner()
	return e
}

func (e *testEnv) newRunner() *Runner {
	e.t.Helper()
	r, err := NewRunner(Options{DB: e.db, Catalog: e.cat, Integrations: e.ints, Destinations: e.dests, Index: e.index.Store(),
		Tiers: e.tiers, ConfigDir: e.config, Now: e.clock.Now, Location: time.UTC, Version: "test"})
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

// createArr creates an *arr integration with path mappings (arr → local).
func (e *testEnv) createArr(typ integrations.Type, name, url string, mappings map[string]string) integrations.Integration {
	e.t.Helper()
	var list []map[string]string
	for _, k := range slices.Sorted(maps.Keys(mappings)) {
		list = append(list, map[string]string{"arr": k, "local": mappings[k]})
	}
	settings, _ := json.Marshal(map[string]any{"pathMappings": list})
	it, err := e.ints.Create(e.ctx, integrations.Input{Type: typ, Name: name, URL: url, APIKey: arrKey, Settings: settings})
	if err != nil {
		e.t.Fatal(err)
	}
	return it
}

// setMappings replaces an integration's path mappings.
func (e *testEnv) setMappings(it integrations.Integration, mappings map[string]string) integrations.Integration {
	e.t.Helper()
	var list []map[string]string
	for _, k := range slices.Sorted(maps.Keys(mappings)) {
		list = append(list, map[string]string{"arr": k, "local": mappings[k]})
	}
	if list == nil {
		list = []map[string]string{}
	}
	settings, _ := json.Marshal(map[string]any{"pathMappings": list})
	out, err := e.ints.Update(e.ctx, it.ID, integrations.Input{Name: it.Name, URL: it.URL, Settings: settings})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

// writeFile creates a sparse file of size at rel under the media root.
func (e *testEnv) writeFile(rel string, size int64) {
	e.t.Helper()
	p := filepath.Join(e.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	f, err := os.Create(p)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		e.t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		e.t.Fatal(err)
	}
}

func (e *testEnv) scan() {
	e.t.Helper()
	if _, err := e.scanner.Scan(e.ctx, e.src.ID, nil); err != nil {
		e.t.Fatalf("scan: %v", err)
	}
}

// refresh runs a full refresh of an integration that must succeed.
func (e *testEnv) refresh(id int64) {
	e.t.Helper()
	e.jobs++
	job := jobs.Job{ID: 10000 + e.jobs, Type: jobs.TypeRefresh, Trigger: jobs.TriggerSchedule, Params: jobs.Params{IntegrationID: id}, Attempt: 1}
	if _, err := e.index.Run(e.ctx, job, jobs.Env{Reporter: &memReporter{}, Items: &memItems{}}); err != nil {
		e.t.Fatalf("refresh %d: %v", id, err)
	}
}

// fixtureFiles returns the path and size of every file a recorded *arr reports (Radarr: in
// movieList, default movie.json).
func fixtureFiles(t *testing.T, kind arr.Kind, movieList string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	type file struct {
		Path string `json:"path"`
		Size int64  `json:"size"`
	}
	decode := func(name string, v any) {
		if err := json.Unmarshal(arrtest.Fixture(t, kind, name), v); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	switch kind {
	case arr.KindRadarr:
		if movieList == "" {
			movieList = "movie.json"
		}
		var movies []struct {
			MovieFile *file `json:"movieFile"`
		}
		decode(movieList, &movies)
		for _, m := range movies {
			if m.MovieFile != nil {
				out[m.MovieFile.Path] = m.MovieFile.Size
			}
		}
	case arr.KindSonarr:
		for _, n := range []string{"episodefile-seriesId-1.json", "episodefile-seriesId-2.json"} {
			var fs []file
			decode(n, &fs)
			for _, f := range fs {
				out[f.Path] = f.Size
			}
		}
	case arr.KindLidarr:
		var fs []file
		decode("trackfile-artistId-1.json", &fs)
		for _, f := range fs {
			out[f.Path] = f.Size
		}
	}
	return out
}

// newJob inserts a running manifest_export job row (the job manager's part) and returns it.
func (e *testEnv) newJob(dry bool) jobs.Job {
	e.t.Helper()
	return e.newJobFor(e.dest.ID, dry)
}

// newJobFor is newJob for destination destID.
func (e *testEnv) newJobFor(destID int64, dry bool) jobs.Job {
	e.t.Helper()
	params := jobs.Params{DestinationID: destID}
	raw, _ := json.Marshal(params)
	queued := e.clock.Now().UTC()
	var id int64
	err := e.db.Write(e.ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(e.ctx, `INSERT INTO jobs (type, status, trigger, dry_run, params, destination_id, queued_at, started_at)
			VALUES ('manifest_export', 'running', 'manual', ?, ?, ?, ?, ?)`, dry, string(raw), destID, db.FormatTime(queued), db.FormatTime(queued))
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return jobs.Job{ID: id, Type: jobs.TypeManifestExport, Status: jobs.StatusRunning, Trigger: jobs.TriggerManual, DryRun: dry,
		Params: params, Attempt: 1, QueuedAt: queued}
}

// run runs a job with a recording reporter.
func (e *testEnv) run(job jobs.Job) (jobs.Result, *memReporter, error) {
	e.t.Helper()
	rep := &memReporter{}
	res, err := e.runner.Run(e.ctx, job, jobs.Env{Reporter: rep, Items: &memItems{}})
	return res, rep, err
}

// runOK runs a new export job that must succeed and returns its stats.
func (e *testEnv) runOK() (Stats, *memReporter) {
	e.t.Helper()
	res, rep, err := e.run(e.newJob(false))
	if err != nil {
		e.t.Fatalf("export: %v\n%s", err, rep)
	}
	return res.Stats.(Stats), rep
}

// crashRun runs job and expects it to stop with a faultinject.Crash at point.
func (e *testEnv) crashRun(job jobs.Job, point string) {
	e.t.Helper()
	faultinject.SetHook(faultinject.CrashAt(point, 1))
	defer faultinject.SetHook(nil)
	defer func() {
		p := recover()
		if c, ok := p.(faultinject.Crash); !ok || c.Point != point {
			e.t.Fatalf("the run did not crash at %s: %v", point, p)
		}
	}()
	_, _, _ = e.run(job)
}

// versions returns the recorded versions, newest first.
func (e *testEnv) versions() []Version {
	e.t.Helper()
	list, err := e.runner.Store().List(e.ctx, e.dest.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return list
}

// entries lists the names in .bunkarr/manifests at the destination.
func (e *testEnv) entries() []string {
	e.t.Helper()
	es, err := os.ReadDir(filepath.Join(e.target, filepath.FromSlash(Root)))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, x := range es {
		out = append(out, x.Name())
	}
	return out
}

// assertConverged checks that every recorded version names a complete, intact directory and
// nothing else is left: no partial or trash directory, no temp file.
func (e *testEnv) assertConverged(want int) []Version {
	e.t.Helper()
	vs := e.versions()
	var dirs []string
	for _, n := range e.entries() {
		if strings.HasPrefix(n, ".") {
			e.t.Fatalf("leftover %s in %s", n, Root)
		}
		dirs = append(dirs, n)
	}
	if len(dirs) != want || len(vs) != want {
		e.t.Fatalf("%d version directories %v and %d rows, want %d", len(dirs), dirs, len(vs), want)
	}
	for _, v := range vs {
		if v.Integrity != IntegrityOK || !slices.Contains(dirs, filepath.Base(v.Path)) {
			e.t.Fatalf("version %+v does not match the directories %v", v, dirs)
		}
		m, err := ParseDir(filepath.Join(e.target, filepath.FromSlash(v.Path)))
		if err != nil {
			e.t.Fatalf("ParseDir %s: %v", v.Path, err)
		}
		if h, _ := ContentHash(m); h != v.ContentHash || m.Summary.Items != v.ItemCount || m.Summary.Files != v.FileCount {
			e.t.Fatalf("version %s: hash %s, row %+v", v.Path, h, v)
		}
	}
	_ = filepath.WalkDir(e.target, func(p string, d os.DirEntry, err error) error {
		if err == nil && filecopy.IsTempName(d.Name()) {
			e.t.Fatalf("temp file left: %s", p)
		}
		return nil
	})
	return vs
}

// build builds the destination's manifest (nil destination: the export scope).
func (e *testEnv) build(dest *destinations.Destination) *Manifest {
	e.t.Helper()
	m, err := e.runner.Builder().Build(e.ctx, BuildScope{Destination: dest})
	if err != nil {
		e.t.Fatalf("build: %v", err)
	}
	return m
}

// addRecord inserts a destination_files row for a file of the source; linkOf 0 is none. It
// returns the row id.
func (e *testEnv) addRecord(rel string, size, mtimeNs int64, state string, linkOf int64) int64 {
	e.t.Helper()
	var id int64
	err := e.db.Write(e.ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(e.ctx, `INSERT INTO destination_files (destination_id, source_id, rel_path, source_rel_path, size,
			mtime_ns, hash, link_of, state, copied_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			e.dest.ID, e.src.ID, e.src.DestFolder+"/"+rel, rel, size, mtimeNs, "sha256:"+strings.Repeat("ab", 32),
			sql.NullInt64{Int64: linkOf, Valid: linkOf != 0}, state, db.FormatTime(e.clock.Now()))
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

// catalogFile returns the live catalog file at rel.
func (e *testEnv) catalogFile(rel string) catalog.File {
	e.t.Helper()
	var out catalog.File
	found := false
	err := e.cat.Live(e.ctx, e.src.ID, func(f catalog.File) error {
		if f.RelPath == rel {
			out, found = f, true
		}
		return nil
	})
	if err != nil || !found {
		e.t.Fatalf("catalog file %s: found %v, %v", rel, found, err)
	}
	return out
}

// recordAll records every live catalog file of the source as present at the destination.
func (e *testEnv) recordAll() {
	e.t.Helper()
	var files []catalog.File
	if err := e.cat.Live(e.ctx, e.src.ID, func(f catalog.File) error { files = append(files, f); return nil }); err != nil {
		e.t.Fatal(err)
	}
	for _, f := range files {
		e.addRecord(f.RelPath, f.Size, f.MtimeNs, "present", 0)
	}
}

// fakeTiers decides by relative path: tiers[rel], default full.
type fakeTiers struct {
	mu    sync.Mutex
	tiers map[string]Tier
}

func (f *fakeTiers) set(rel string, t Tier) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tiers == nil {
		f.tiers = map[string]Tier{}
	}
	f.tiers[rel] = t
}

func (f *fakeTiers) Decide(ctx context.Context, q Queryer, destinationID, sourceID int64) (func(int64) Decision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	byID := map[int64]Tier{}
	for rel, t := range f.tiers {
		var id int64
		err := q.QueryRowContext(ctx, `SELECT id FROM catalog_files WHERE source_id = ? AND rel_path = ?`, sourceID, rel).Scan(&id)
		if err == nil {
			byID[id] = t
		}
	}
	return func(id int64) Decision {
		if t, ok := byID[id]; ok {
			return Decision{Tier: t, RuleID: 7, RuleName: "test rule"}
		}
		return Decision{Tier: TierFull, RuleName: FallbackRuleName}
	}, nil
}

// memItems is an in-memory jobs.ItemStore.
type memItems struct {
	mu      sync.Mutex
	items   []jobs.Item
	next    int64
	planned bool
}

func (m *memItems) Planned(context.Context, int64) (bool, error) { return m.planned, nil }

func (m *memItems) AddItems(_ context.Context, jobID int64, items []jobs.Item, final bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, it := range items {
		m.next++
		it.ID, it.JobID = m.next, jobID
		m.items = append(m.items, it)
	}
	if final {
		m.planned = true
	}
	return nil
}

func (m *memItems) DeleteItems(context.Context, int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items, m.planned = nil, false
	return nil
}

func (m *memItems) Pending(_ context.Context, _ int64, after int64, limit int) ([]jobs.Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []jobs.Item
	for _, it := range m.items {
		if it.Status == jobs.ItemPending && it.ID > after && len(out) < limit {
			out = append(out, it)
		}
	}
	return out, nil
}

func (m *memItems) SetDetail(context.Context, int64, json.RawMessage) error { return nil }

func (m *memItems) Finish(_ context.Context, id int64, st jobs.ItemStatus, bytes int64, msg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.items {
		if m.items[i].ID == id {
			m.items[i].Status, m.items[i].Bytes, m.items[i].Error = st, bytes, msg
		}
	}
	return nil
}

func (m *memItems) Counts(context.Context, int64) ([]jobs.ItemCount, error) { return nil, nil }

// memReporter records a job's log.
type memReporter struct {
	mu    sync.Mutex
	lines []string
	warns []string
}

func (r *memReporter) Progress(jobs.Progress) {}

func (r *memReporter) Log(level slog.Level, msg string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, fmt.Sprintf("%s %s %v", level, msg, args))
	if level >= slog.LevelWarn {
		r.warns = append(r.warns, msg)
	}
}

func (r *memReporter) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, "\n")
}

// warned reports whether a warning contains s.
func (r *memReporter) warned(s string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, w := range r.warns {
		if strings.Contains(w, s) {
			return true
		}
	}
	return false
}
