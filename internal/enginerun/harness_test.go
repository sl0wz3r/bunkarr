package enginerun

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
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
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/bwlimit"
	"github.com/sl0wz3r/bunkarr/internal/engines/enginetest"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
	"github.com/sl0wz3r/bunkarr/internal/engines/rclone"
	"github.com/sl0wz3r/bunkarr/internal/engines/restic"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/syncer"
)

// baseTime is the mtime of test files: it has a nanosecond part, so precision bugs show.
var baseTime = time.Date(2025, 3, 14, 15, 9, 26, 535897932, time.UTC)

// templateDir holds migratedTemplate's database; TestMain removes it.
var templateDir string

// migratedTemplate is a database migrated once per test binary; every harness copies it.
var migratedTemplate = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "bunkarr-enginerun-db-")
	if err != nil {
		return "", err
	}
	templateDir = dir
	p := filepath.Join(dir, "bunkarr.db")
	d, err := db.Open(context.Background(), p, nil)
	if err != nil {
		return "", err
	}
	return p, d.Close()
})

func TestMain(m *testing.M) {
	code := m.Run()
	if templateDir != "" {
		_ = os.RemoveAll(templateDir)
	}
	os.Exit(code)
}

func openMigratedDB(t testing.TB, p string) *db.DB {
	t.Helper()
	tmpl, err := migratedTemplate()
	if err != nil {
		t.Fatalf("migrate the template database: %v", err)
	}
	for _, suffix := range []string{"", "-wal"} {
		b, err := os.ReadFile(tmpl + suffix)
		if suffix != "" && errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p+suffix, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	d, err := db.Open(context.Background(), p, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// harness is one source, one engine destination (restic on a local path, or rclone on S3 with
// crypt), the real stores, the Service, and a FakeRunner answering through a fake repository or
// object store.
type harness struct {
	t   *testing.T
	ctx context.Context

	db      *db.DB
	cat     *catalog.Store
	scanner *catalog.Scanner
	dests   *destinations.Store
	jq      *jobqueue.Store
	files   *syncer.Store
	svc     *Service
	runner  *enginetest.FakeRunner
	planner *syncer.Planner

	base, srcDir, repoDir, configDir string
	src                              catalog.Source
	dest                             destinations.Destination

	restic *fakeRestic
	rclone *fakeRclone
	// fc records the jobs Dispatch sent to the filecopy runners.
	fc *recordingRunners

	mu    sync.Mutex
	clock time.Time
	rep   *recReporter
	avail engines.Availability
	tiers syncer.Tiers
}

type harnessOptions struct {
	kind engines.Kind
	// destKind is the rclone destination's kind (s3 by default; sftp with host keys).
	destKind engines.DestKind
	// plain: rclone without crypt.
	plain bool
	// settings and retention of the destination.
	settings  *destinations.Settings
	retention *destinations.Retention
	bandwidth *bwlimit.Config
	// unconfirmed leaves the recovery kit custody unconfirmed (S21).
	unconfirmed bool
}

func resolvedTempDir(t testing.TB) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func newHarness(t *testing.T, o harnessOptions) *harness {
	t.Helper()
	h := &harness{t: t, ctx: context.Background(), clock: time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC), rep: &recReporter{},
		fc: &recordingRunners{}}
	h.avail = engines.Availability{Restic: engines.BinaryStatus{Available: true, Version: "0.18.1", Path: "/usr/bin/restic"},
		Rclone: engines.BinaryStatus{Available: true, Version: "1.74.1", Path: "/usr/bin/rclone"}}
	h.base = resolvedTempDir(t)
	h.srcDir = filepath.Join(h.base, "src")
	h.repoDir = filepath.Join(h.base, "repo")
	h.configDir = filepath.Join(h.base, "config")
	for _, d := range []string{h.srcDir, h.repoDir, h.configDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	h.db = openMigratedDB(t, filepath.Join(h.base, "bunkarr.db"))
	h.files = syncer.NewStore(h.db)
	h.cat = catalog.NewStore(h.db, catalog.StoreOptions{HasBackups: h.files.HasBackups})
	h.scanner = catalog.NewScanner(h.cat, catalog.ScannerOptions{})
	h.jq = jobqueue.NewStore(h.db)
	kr, err := config.NewKeyring(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	h.runner = enginetest.NewFakeRunner(t)
	markerID := ""
	switch o.kind {
	case engines.Restic:
		h.restic = newFakeRestic(t, h)
		h.restic.install(h.runner)
		markerID = restic.MarkerID(h.restic.id)
	case engines.Rclone:
		markerID = "7d9c1f3e-0000-4000-8000-00000000c10e"
	}
	fakeCreate := &enginetest.FakeEngine{EngineKind: o.kind, OnCreate: func(context.Context, engines.Destination, engines.Secrets, bool) (engines.CreateResult, error) {
		return engines.CreateResult{MarkerID: markerID, Initialized: o.kind == engines.Restic}, nil
	}}
	h.dests = destinations.New(h.db, destinations.Options{Keyring: kr, Now: h.now,
		Engines:   func(k engines.Kind) (engines.Engine, bool) { return fakeCreate, k == o.kind },
		CheckHost: func(context.Context, string) error { return nil }})
	h.src, err = h.cat.Create(h.ctx, catalog.SourceInput{Name: "Movies", Path: h.srcDir})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	in := destinations.Input{Name: "offsite", Engine: string(o.kind), SourceIDs: []int64{h.src.ID}, Settings: o.settings, Retention: o.retention}
	in.Bandwidth = o.bandwidth
	opts := destinations.CreateOptions{}
	switch o.kind {
	case engines.Restic:
		in.Kind, in.Target = engines.Local, h.repoDir
		opts.AllowLocal = true
	case engines.Rclone:
		in.Kind = engines.S3
		in.Remote = []byte(`{"provider":"Minio","endpoint":"https://minio.example.com","region":"us-east-1","bucket":"bunkarr-bk","prefix":"media"}`)
		creds := destinations.CredentialsInput{}
		if err := json.Unmarshal([]byte(`{"accessKeyId":"AKIAENGINERUN0001","secretAccessKey":"s3cr3t/enginerun-access-key-0001"}`), &creds); err != nil {
			t.Fatal(err)
		}
		in.Credentials = &creds
		if o.plain {
			in.Encryption = &destinations.EncryptionInput{Mode: engines.EncryptionNone, AcceptUnencrypted: true}
		}
	}
	h.dest, err = h.dests.Create(h.ctx, in, opts)
	if err != nil {
		t.Fatalf("create destination: %v", err)
	}
	if !o.unconfirmed {
		h.exec(`UPDATE destinations SET kit_confirmed_at = ? WHERE id = ?`, db.FormatTime(h.now()), h.dest.ID)
	}
	if o.kind == engines.Rclone {
		ed, _, err := h.dests.SecretsFor(h.ctx, h.dest.ID)
		if err != nil {
			t.Fatal(err)
		}
		h.rclone = newFakeRclone(t, h, rclone.Root(ed))
		h.rclone.put(".bunkarr/destination.json", []byte(fmt.Sprintf(`{"id":%q,"name":"offsite","createdAt":"2026-09-01T00:00:00Z"}`, markerID)), h.now())
		h.rclone.install(h.runner)
	}
	h.newService()
	return h
}

// newService (re)creates the Service over the harness's stores.
func (h *harness) newService() {
	runDirs := enginetest.RunDirs(h.t)
	so := syncer.Options{DB: h.db, Store: h.files, Catalog: h.cat, Scanner: h.scanner, Destinations: h.dests, Now: h.now, Tiers: h.tiers,
		Location: time.UTC}
	h.planner = syncer.NewPlanner(so)
	h.svc = New(Options{DB: h.db, Files: h.files, Planner: h.planner, Catalog: h.cat, Scanner: h.scanner, Destinations: h.dests,
		Tiers: h.tiers, Filecopy: FilecopyRunners{Sync: h.fc.runner(jobs.TypeSync), Verify: h.fc.runner(jobs.TypeVerify),
			Retention: h.fc.runner(jobs.TypeRetention)},
		Restic:       &restic.Driver{Runner: h.runner, RunDirs: runDirs, CacheRoot: filepath.Join(h.configDir, "cache", "restic"), Now: h.now},
		Rclone:       &rclone.Driver{Runner: h.runner, RunDirs: runDirs, RetryWait: -1, Now: h.now},
		Availability: func() engines.Availability { return h.avail }, ConfigDir: h.configDir, RunDirs: runDirs, Runner: h.runner,
		Now: h.now, Location: time.UTC, HostName: "bunkarr-test", ProcessStart: h.clock.Add(-time.Hour),
		CheckHost: func(context.Context, string) error { return nil }})
}

func (h *harness) exec(q string, args ...any) {
	h.t.Helper()
	err := h.db.Write(h.ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(h.ctx, q, args...)
		return err
	})
	if err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) now() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.clock
}

func (h *harness) advance(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clock = h.clock.Add(d)
}

func (h *harness) setClock(t time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clock = t
}

// update changes the destination row.
func (h *harness) update(in destinations.Input) {
	h.t.Helper()
	d, err := h.dests.Update(h.ctx, h.dest.ID, in)
	if err != nil {
		h.t.Fatal(err)
	}
	h.dest = d
}

func (h *harness) settings(fn func(*destinations.Settings)) {
	h.t.Helper()
	d, err := h.dests.Get(h.ctx, h.dest.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	s := d.Settings
	fn(&s)
	h.update(destinations.Input{Settings: &s})
}

// --- source tree ---

func (h *harness) srcPath(rel string) string { return filepath.Join(h.srcDir, filepath.FromSlash(rel)) }

// writeSrc writes a source file with an mtime of baseTime + n seconds.
func (h *harness) writeSrc(rel, content string, n int) {
	h.t.Helper()
	writeFileAt(h.t, h.srcPath(rel), content, baseTime.Add(time.Duration(n)*time.Second))
}

func writeFileAt(t testing.TB, p, content string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	tmp := p + ".writing"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tmp, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, p); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) removeSrc(rel string) {
	h.t.Helper()
	if err := os.Remove(h.srcPath(rel)); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) renameSrc(from, to string) {
	h.t.Helper()
	if err := os.MkdirAll(filepath.Dir(h.srcPath(to)), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.Rename(h.srcPath(from), h.srcPath(to)); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) linkSrc(existing, name string) {
	h.t.Helper()
	if err := os.MkdirAll(filepath.Dir(h.srcPath(name)), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.Link(h.srcPath(existing), h.srcPath(name)); err != nil {
		h.t.Fatal(err)
	}
}

// content returns size-distinct content: a label repeated to n bytes.
func content(label string, n int) string {
	var b strings.Builder
	for b.Len() < n {
		b.WriteString(label)
		b.WriteByte('.')
	}
	return b.String()[:n]
}

// --- jobs ---

func (h *harness) newJob(typ jobs.Type, dryRun bool, p jobs.Params) jobs.Job {
	h.t.Helper()
	if p.DestinationID == 0 {
		p.DestinationID = h.dest.ID
	}
	j, created, err := h.jq.CreateJob(h.ctx, jobs.Spec{Type: typ, Trigger: jobs.TriggerManual, DryRun: dryRun, Params: p})
	if err != nil {
		h.t.Fatal(err)
	}
	if !created {
		h.t.Fatalf("job %d was reused", j.ID)
	}
	return j
}

func (h *harness) closeJob(id int64) {
	h.t.Helper()
	h.exec(`UPDATE jobs SET status = 'completed', finished_at = ? WHERE id = ?`, db.FormatTime(h.now()), id)
}

func (h *harness) env() jobs.Env { return jobs.Env{Reporter: h.rep, Items: h.jq} }

// runJob runs one attempt of a job through Dispatch (not closing it).
func (h *harness) runJob(j jobs.Job) (jobs.Result, error) {
	return h.svc.Dispatch(j.Type).Run(h.ctx, j, h.env())
}

// mustRun creates and runs a job to its end and fails the test on an error.
func (h *harness) mustRun(typ jobs.Type, dryRun bool, p jobs.Params) (jobs.Result, jobs.Job) {
	h.t.Helper()
	j := h.newJob(typ, dryRun, p)
	res, err := h.runJob(j)
	h.closeJob(j.ID)
	if err != nil {
		h.t.Fatalf("%s: %v\nlogs:\n%s", typ, err, h.rep.dump())
	}
	return res, j
}

func (h *harness) mustSync(p jobs.Params) (SyncStats, jobs.Job) {
	h.t.Helper()
	res, j := h.mustRun(jobs.TypeSync, false, p)
	st, ok := res.Stats.(SyncStats)
	if !ok {
		h.t.Fatalf("stats are %T", res.Stats)
	}
	return st, j
}

func (h *harness) items(jobID int64) []jobs.Item {
	h.t.Helper()
	var out []jobs.Item
	for page := 1; ; page++ {
		p, err := h.jq.ListItems(h.ctx, jobID, jobqueue.ItemQuery{Page: page, PageSize: jobqueue.MaxPageSize})
		if err != nil {
			h.t.Fatal(err)
		}
		out = append(out, p.Records...)
		if len(p.Records) < jobqueue.MaxPageSize {
			return out
		}
	}
}

func itemsBy(items []jobs.Item, action jobs.ItemAction, status jobs.ItemStatus) []string {
	var out []string
	for _, it := range items {
		if (action == "" || it.Action == action) && (status == "" || it.Status == status) {
			out = append(out, it.RelPath)
		}
	}
	slices.Sort(out)
	return out
}

func (h *harness) records() []syncer.Record {
	h.t.Helper()
	recs, err := h.files.List(h.ctx, h.dest.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	return recs
}

func (h *harness) live(rel string) (syncer.Record, bool) {
	h.t.Helper()
	r, ok, err := h.files.LiveAt(h.ctx, h.dest.ID, rel)
	if err != nil {
		h.t.Fatal(err)
	}
	return r, ok
}

func (h *harness) mustLive(rel string) syncer.Record {
	h.t.Helper()
	r, ok := h.live(rel)
	if !ok {
		h.t.Fatalf("no live record at %s; records: %+v", rel, h.records())
	}
	return r
}

func (h *harness) retained() []syncer.Record {
	var out []syncer.Record
	for _, r := range h.records() {
		if r.State == syncer.StateRetained {
			out = append(out, r)
		}
	}
	return out
}

// dp is a destination path of the source.
func (h *harness) dp(rel string) string { return h.src.DestFolder + "/" + rel }

// --- crash hooks ---

// runWithHook runs one attempt with hook installed and reports whether it crashed.
func (h *harness) runWithHook(j jobs.Job, hook func(string)) (crashed bool, res jobs.Result, err error) {
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
	res, err = h.runJob(j)
	return false, res, err
}

// --- reporters and filecopy stand-ins ---

type recReporter struct {
	mu       sync.Mutex
	progress []jobs.Progress
	logs     []string
}

func (r *recReporter) Progress(p jobs.Progress) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.progress) < 10000 {
		r.progress = append(r.progress, p)
	}
}

func (r *recReporter) Log(level slog.Level, msg string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, fmt.Sprintf("%s %s %v", level, msg, args))
}

func (r *recReporter) dump() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.logs, "\n")
}

func (r *recReporter) has(sub string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.ContainsFunc(r.logs, func(l string) bool { return strings.Contains(l, sub) })
}

type recordingRunners struct {
	mu   sync.Mutex
	jobs []jobs.Job
}

func (r *recordingRunners) runner(t jobs.Type) jobs.Runner {
	return jobs.RunnerFunc(func(_ context.Context, job jobs.Job, _ jobs.Env) (jobs.Result, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.jobs = append(r.jobs, job)
		return jobs.Result{Summary: "filecopy " + string(t)}, nil
	})
}

func (r *recordingRunners) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.jobs)
}

// callsOf returns the runner's commands of a binary and subcommand.
func (h *harness) callsOf(b proc.Binary, sub string) []*enginetest.Call {
	return h.runner.CallsOf(b, sub)
}

const time1h = time.Hour

func destinationsInput(bw bwlimit.Config) destinations.Input {
	return destinations.Input{Bandwidth: &bw}
}
