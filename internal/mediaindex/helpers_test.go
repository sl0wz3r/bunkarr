package mediaindex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/config"
	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr/arrtest"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

const testKey = "0123456789abcdef0123456789abcdef"

// testEnv is a database with an *arr integration pointing at a fake *arr, a source holding the
// *arr's files (as sparse files of the recorded sizes), and a refresh runner.
type testEnv struct {
	t       *testing.T
	db      *db.DB
	ints    *integrations.Store
	cat     *catalog.Store
	scanner *catalog.Scanner
	runner  *Runner
	srv     *arrtest.Server
	it      integrations.Integration
	src     catalog.Source
	// root is the media root: the *arr's /movies (/tv, /music) is <root>/movies (...).
	root  string
	enq   *memEnqueuer
	dests []FollowUpDestination
	clock *testClock
	jobID int64
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// arrFolder is the *arr's root folder of each kind in the fixtures.
var arrFolder = map[arr.Kind]string{arr.KindRadarr: "/movies", arr.KindSonarr: "/tv", arr.KindLidarr: "/music"}

func openDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "bunkarr.db"), nil)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func resolvedTemp(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// newEnv starts a fake *arr of kind, creates its integration (mapping the fixtures' root folder to
// <root>/<folder>), a source over that folder holding the fixtures' files, and one destination
// linked to it for follow-ups.
func newEnv(t *testing.T, kind arr.Kind, opts ...func(*arrtest.Server)) *testEnv {
	t.Helper()
	e := &testEnv{t: t, db: openDB(t), enq: &memEnqueuer{}, clock: &testClock{now: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)}}
	kr, err := config.NewKeyring(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	e.ints = integrations.NewStore(e.db, kr)
	e.cat = catalog.NewStore(e.db, catalog.StoreOptions{})
	e.scanner = catalog.NewScanner(e.cat, catalog.ScannerOptions{})
	e.srv = arrtest.NewServer(t, kind, testKey)
	for _, o := range opts {
		o(e.srv)
	}
	e.root = resolvedTemp(t)
	folder := arrFolder[kind]
	local := filepath.Join(e.root, strings.TrimPrefix(folder, "/"))
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	e.writeFixtureFiles(kind)
	e.src, err = e.cat.Create(context.Background(), catalog.SourceInput{Name: "Media", Path: local})
	if err != nil {
		t.Fatal(err)
	}
	e.scan()
	settings, _ := json.Marshal(map[string]any{"pathMappings": []map[string]string{{"arr": folder, "local": local}}})
	e.it, err = e.ints.Create(context.Background(), integrations.Input{Type: integrations.Type(kind), Name: string(kind),
		URL: e.srv.URL, APIKey: testKey, Settings: settings})
	if err != nil {
		t.Fatal(err)
	}
	e.dests = []FollowUpDestination{{ID: 1, Enabled: true, SourceIDs: []int64{e.src.ID}, SyncOnArrChange: true}}
	e.runner = e.newRunner(DefaultBatchSize)
	return e
}

func (e *testEnv) newRunner(batch int) *Runner {
	e.t.Helper()
	r, err := NewRunner(RefreshOptions{DB: e.db, Integrations: e.ints, Catalog: e.cat, Enqueuer: e.enq,
		Destinations: func(context.Context) ([]FollowUpDestination, error) { return slices.Clone(e.dests), nil },
		Now:          e.clock.Now, BatchSize: batch})
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

// writeFixtureFiles creates, under root, a sparse file of the recorded size for every file the
// fake *arr reports.
func (e *testEnv) writeFixtureFiles(kind arr.Kind) {
	e.t.Helper()
	for p, size := range fixtureFiles(e.t, kind, e.srv) {
		e.writeFile(p, size)
	}
}

// writeFile creates a sparse file at an *arr path (under root).
func (e *testEnv) writeFile(arrPath string, size int64) {
	e.t.Helper()
	p := filepath.Join(e.root, filepath.FromSlash(strings.TrimPrefix(arrPath, "/")))
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

// fixtureFiles returns the path and size of every file the fake *arr reports.
func fixtureFiles(t *testing.T, kind arr.Kind, srv *arrtest.Server) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	decode := func(name string, v any) {
		if err := json.Unmarshal(arrtest.Fixture(t, kind, name), v); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	type file struct {
		Path string `json:"path"`
		Size int64  `json:"size"`
	}
	switch kind {
	case arr.KindRadarr:
		var movies []struct {
			MovieFile *file `json:"movieFile"`
		}
		decode("movie.json", &movies)
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

func (e *testEnv) scan() {
	e.t.Helper()
	if _, err := e.scanner.Scan(context.Background(), e.src.ID, nil); err != nil {
		e.t.Fatalf("scan: %v", err)
	}
}

// refresh runs a refresh job to completion.
func (e *testEnv) refresh(p jobs.Params, dry bool, trigger jobs.Trigger) (jobs.Result, *memReporter, error) {
	e.t.Helper()
	e.jobID++
	return e.runJob(e.runner, jobs.Job{ID: e.jobID, Type: jobs.TypeRefresh, Trigger: trigger, DryRun: dry, Params: p, Attempt: 1}, &memItems{})
}

func (e *testEnv) runJob(r *Runner, job jobs.Job, items *memItems) (jobs.Result, *memReporter, error) {
	e.t.Helper()
	if job.Params.IntegrationID == 0 {
		job.Params.IntegrationID = e.it.ID
	}
	rep := &memReporter{}
	res, err := r.Run(context.Background(), job, jobs.Env{Reporter: rep, Items: items})
	return res, rep, err
}

// full runs a full refresh (trigger schedule) that must succeed.
func (e *testEnv) full() (Stats, *memReporter) {
	e.t.Helper()
	res, rep, err := e.refresh(jobs.Params{}, false, jobs.TriggerSchedule)
	if err != nil {
		e.t.Fatalf("full refresh: %v (log: %s)", err, rep)
	}
	return res.Stats.(Stats), rep
}

func (e *testEnv) state() State {
	e.t.Helper()
	st, err := e.runner.Store().State(context.Background(), nil, e.it.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return st
}

// items returns the integration's items by *arr id (deleted ones too).
func (e *testEnv) items() map[int64]Item {
	e.t.Helper()
	out := map[int64]Item{}
	err := e.runner.Store().EachItem(context.Background(), nil, e.it.ID, true, func(it Item) error {
		out[it.ArrID] = it
		return nil
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

// files returns the integration's files (of live and deleted items) by *arr file id.
func (e *testEnv) files() map[int64]File {
	e.t.Helper()
	out := map[int64]File{}
	err := e.runner.Store().EachFile(context.Background(), nil, e.it.ID, true, func(f File) error {
		out[f.ArrFileID] = f
		return nil
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

// editList decodes a JSON list fixture served for target, applies fn and serves the result.
func (e *testEnv) editList(target, fixture string, fn func([]map[string]any) []map[string]any) {
	e.t.Helper()
	var list []map[string]any
	if err := json.Unmarshal(arrtest.Fixture(e.t, e.srv.Kind, fixture), &list); err != nil {
		e.t.Fatal(err)
	}
	list = fn(list)
	b, err := json.Marshal(list)
	if err != nil {
		e.t.Fatal(err)
	}
	e.srv.SetJSON("GET", target, b)
}

// movie returns a copy of a movie of Radarr's movie.json list.
func movieByID(list []map[string]any, id float64) map[string]any {
	for _, m := range list {
		if m["id"] == id {
			return m
		}
	}
	return nil
}

// memItems is an in-memory jobs.ItemStore.
type memItems struct {
	mu      sync.Mutex
	items   []jobs.Item
	next    int64
	planned bool
}

func (m *memItems) Planned(context.Context, int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.planned, nil
}

func (m *memItems) AddItems(_ context.Context, jobID int64, items []jobs.Item, final bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, it := range items {
		m.next++
		it.ID, it.JobID = m.next, jobID
		if it.Status == "" {
			it.Status = jobs.ItemPending
		}
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
		if it.Status == jobs.ItemPending && it.ID > after {
			out = append(out, it)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

func (m *memItems) SetDetail(_ context.Context, id int64, d json.RawMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.items {
		if m.items[i].ID == id {
			m.items[i].Detail = d
		}
	}
	return nil
}

func (m *memItems) Finish(_ context.Context, id int64, st jobs.ItemStatus, bytes int64, msg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.items {
		if m.items[i].ID == id {
			m.items[i].Status, m.items[i].Bytes, m.items[i].Error = st, bytes, msg
			return nil
		}
	}
	return errors.New("no such item")
}

func (m *memItems) Counts(context.Context, int64) ([]jobs.ItemCount, error) { return nil, nil }

func (m *memItems) all() []jobs.Item {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.items)
}

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
	line := fmt.Sprintf("%s %s %v", level, msg, args)
	r.lines = append(r.lines, line)
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

// memEnqueuer records specs; an identical queued spec returns the existing job, like the queue.
type memEnqueuer struct {
	mu   sync.Mutex
	jobs []jobs.Job
	fail error
}

func (m *memEnqueuer) Enqueue(_ context.Context, spec jobs.Spec) (jobs.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return jobs.Job{}, m.fail
	}
	for _, j := range m.jobs {
		if j.Type == spec.Type && j.DryRun == spec.DryRun && reflect.DeepEqual(j.Params, spec.Params) {
			return j, nil
		}
	}
	j := jobs.Job{ID: int64(1000 + len(m.jobs)), Type: spec.Type, Trigger: spec.Trigger, DryRun: spec.DryRun, Params: spec.Params,
		Status: jobs.StatusQueued, QueuedAt: time.Now()}
	m.jobs = append(m.jobs, j)
	return j, nil
}

func (m *memEnqueuer) queued() []jobs.Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.jobs)
}

func (m *memEnqueuer) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobs = nil
}

// syncTargets summarizes queued syncs as "dest/source:path,path" ("*" for untargeted).
func syncTargets(list []jobs.Job) []string {
	var out []string
	for _, j := range list {
		if j.Type != jobs.TypeSync {
			continue
		}
		p := "*"
		if len(j.Params.Paths) > 0 {
			p = strings.Join(j.Params.Paths, ",")
		}
		out = append(out, fmt.Sprintf("%d/%v:%s", j.Params.DestinationID, j.Params.SourceIDs, p))
	}
	slices.Sort(out)
	return out
}

// crashRun runs a job and recovers a faultinject crash; it reports whether it crashed.
func crashRun(t *testing.T, e *testEnv, r *Runner, job jobs.Job, items *memItems, hook func(string)) (crashed bool, err error) {
	t.Helper()
	faultinject.SetHook(hook)
	defer faultinject.SetHook(nil)
	defer func() {
		if p := recover(); p != nil {
			if _, ok := p.(faultinject.Crash); !ok {
				panic(p)
			}
			crashed = true
		}
	}()
	_, _, err = e.runJob(r, job, items)
	return false, err
}

// localOf returns the local path of an *arr path in env e.
func (e *testEnv) localOf(arrPath string) string {
	return path.Join(filepath.ToSlash(e.root), strings.TrimPrefix(arrPath, "/"))
}
