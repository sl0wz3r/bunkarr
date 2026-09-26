package syncer

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// baseTime is the mtime of test files: it has a nanosecond part, so granularity bugs show.
var baseTime = time.Date(2025, 3, 14, 15, 9, 26, 535897932, time.UTC)

// harness is a source, a destination on local temp directories, and the real stores.
type harness struct {
	t   *testing.T
	ctx context.Context

	db        *db.DB
	cat       *catalog.Store
	scanner   *catalog.Scanner
	dests     *destinations.Store
	jq        *jobqueue.Store
	store     *Store
	sync      *SyncRunner
	verify    *VerifyRunner
	retention *RetentionRunner

	base, srcDir, dstDir string
	src                  catalog.Source
	dest                 destinations.Destination

	mu    sync.Mutex
	clock time.Time
	rep   *recReporter
}

// templateDir holds migratedTemplate's database; TestMain removes it.
var templateDir string

// migratedTemplate is a database migrated once per test binary. newHarness copies it instead of
// migrating a new database for every harness: under -race the migrations were almost half of a
// crash-matrix case, and the package ran past go test's default 10-minute timeout.
var migratedTemplate = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "bunkarr-syncer-db-")
	if err != nil {
		return "", err
	}
	templateDir = dir
	p := filepath.Join(dir, "bunkarr.db")
	d, err := db.Open(context.Background(), p, nil)
	if err != nil {
		return "", err
	}
	// The last connection's close checkpoints the WAL into the database file.
	return p, d.Close()
})

func TestMain(m *testing.M) {
	code := m.Run()
	if templateDir != "" {
		_ = os.RemoveAll(templateDir)
	}
	os.Exit(code)
}

// openMigratedDB opens a fully migrated database at p (a copy of migratedTemplate), closed when
// the test ends.
func openMigratedDB(t *testing.T, p string) *db.DB {
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
			t.Fatalf("read the template database: %v", err)
		}
		if err := os.WriteFile(p+suffix, b, 0o600); err != nil {
			t.Fatalf("copy the template database: %v", err)
		}
	}
	d, err := db.Open(context.Background(), p, nil)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func resolvedTempDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, ctx: context.Background(), clock: time.Now().UTC(), rep: &recReporter{}}
	h.base = resolvedTempDir(t)
	h.srcDir = filepath.Join(h.base, "src")
	h.dstDir = filepath.Join(h.base, "dst")
	for _, d := range []string{h.srcDir, h.dstDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	d := openMigratedDB(t, filepath.Join(h.base, "bunkarr.db"))
	h.db = d
	h.store = NewStore(d)
	h.cat = catalog.NewStore(d, catalog.StoreOptions{HasBackups: h.store.HasBackups})
	h.scanner = catalog.NewScanner(h.cat, catalog.ScannerOptions{})
	h.dests = destinations.New(d, destinations.Options{Now: h.now})
	h.jq = jobqueue.NewStore(d)
	var err error
	h.src, err = h.cat.Create(h.ctx, catalog.SourceInput{Name: "Movies", Path: h.srcDir})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	h.dest, err = h.dests.Create(h.ctx, destinations.Input{Name: "nas", Target: h.dstDir, SourceIDs: []int64{h.src.ID}},
		destinations.CreateOptions{AllowLocal: true})
	if err != nil {
		t.Fatalf("create destination: %v", err)
	}
	h.newRunners()
	// Tests control the capabilities; the probe of the local temp directory (APFS is usually
	// case-insensitive) must not decide what a test exercises.
	h.setCaps(func(c *filecopy.Capabilities) {
		c.Hardlinks, c.CaseInsensitive, c.InvalidChars, c.TrailingDotSpace, c.MtimeGranularityNs = true, false, "", true, 1
		c.UnstableInodes = false
	})
	return h
}

// envTiersAllFull, when set, gives every harness the real tier engine with no rules (every file
// full): the whole Phase 1 and 2 suite then runs through the tier hook (design phase2-3.md §15,
// acceptance 8). Tests that install their own Tiers (withTiers) are unaffected.
//
//	BUNKARR_TEST_TIERS_ALL_FULL=1 go test ./internal/syncer/
const envTiersAllFull = "BUNKARR_TEST_TIERS_ALL_FULL"

// newRunners (re)creates the job runners over the harness's stores.
func (h *harness) newRunners() {
	o := Options{DB: h.db, Store: h.store, Catalog: h.cat, Scanner: h.scanner, Destinations: h.dests, Now: h.now}
	if os.Getenv(envTiersAllFull) != "" {
		o.Tiers = realEngine(h.t, h)
	}
	h.sync = NewSyncRunner(o)
	h.sync.planBatch = 3
	h.sync.recheckEvery = 4
	h.verify = NewVerifyRunner(o)
	h.verify.planBatch = 3
	h.verify.recheckEvery = 4
	h.retention = NewRetentionRunner(RetentionOptions{Options: o, PruneHistory: h.jq.PruneHistory})
	h.retention.planBatch = 2
	h.retention.recheckEvery = 3
}

// inventInodes makes the destination number inodes per lookup, as a CIFS client mounted with
// noserverino does (Unraid mounts shares so): from now on every stat of a destination path the
// jobs and the capability probe make (destinations.Options.Lstat) returns a new inode number, so
// two names of one file never show the same number, nor does one name twice. The link count is
// the real one (the client reports the server's). The stored capabilities are left as they are.
func (h *harness) inventInodes() { h.inventInodesWith(false) }

// inventInodesWith is inventInodes; with nlink1 every stat also shows a link count of 1, as a CIFS
// client does for a name whose attributes a directory listing primed (the listing has no count).
func (h *harness) inventInodesWith(nlink1 bool) {
	var next atomic.Uint64
	next.Store(1 << 40)
	lstat := func(root *os.Root, rel string) (filecopy.Stat, error) {
		st, err := filecopy.Lstat(root, rel)
		if err == nil {
			st.Ino = next.Add(1)
			if nlink1 {
				st.Nlink = 1
			}
		}
		return st, err
	}
	h.dests = destinations.New(h.db, destinations.Options{Now: h.now, Lstat: lstat})
	h.newRunners()
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

// setCaps rewrites the destination's stored capabilities.
func (h *harness) setCaps(fn func(*filecopy.Capabilities)) {
	h.t.Helper()
	d, err := h.dests.Get(h.ctx, h.dest.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	caps := d.Capabilities
	fn(&caps)
	raw, _ := json.Marshal(caps)
	err = h.db.Write(h.ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(h.ctx, `UPDATE destinations SET capabilities = ? WHERE id = ?`, string(raw), h.dest.ID)
		return err
	})
	if err != nil {
		h.t.Fatal(err)
	}
}

// setSettings changes the destination's settings.
func (h *harness) setSettings(fn func(*destinations.Settings)) {
	h.t.Helper()
	d, err := h.dests.Get(h.ctx, h.dest.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	s := d.Settings
	fn(&s)
	if _, err := h.dests.Update(h.ctx, h.dest.ID, destinations.Input{Settings: &s}); err != nil {
		h.t.Fatal(err)
	}
}

// --- source tree helpers ---

func (h *harness) srcPath(rel string) string { return filepath.Join(h.srcDir, filepath.FromSlash(rel)) }

func (h *harness) dstPath(rel string) string { return filepath.Join(h.dstDir, filepath.FromSlash(rel)) }

// writeSrc writes a source file with an mtime of baseTime + n seconds.
func (h *harness) writeSrc(rel, content string, n int) {
	h.t.Helper()
	writeFileAt(h.t, h.srcPath(rel), content, baseTime.Add(time.Duration(n)*time.Second))
}

func writeFileAt(t *testing.T, p, content string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	// Replace, never write in place: a hardlinked name must keep its old content.
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

func (h *harness) linkSrc(existing, name string) {
	h.t.Helper()
	if err := os.MkdirAll(filepath.Dir(h.srcPath(name)), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.Link(h.srcPath(existing), h.srcPath(name)); err != nil {
		h.t.Fatal(err)
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

// newJob creates a job row (jobs are run directly, not by a Manager).
func (h *harness) newJob(typ jobs.Type, dryRun bool, p jobs.Params) jobs.Job {
	h.t.Helper()
	if p.DestinationID == 0 && typ != jobs.TypeRetention {
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

// closeJob marks a job finished so an identical one can be created.
func (h *harness) closeJob(id int64) {
	h.t.Helper()
	err := h.db.Write(h.ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(h.ctx, `UPDATE jobs SET status = 'completed', finished_at = ? WHERE id = ?`, db.FormatTime(h.now()), id)
		return err
	})
	if err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) env() jobs.Env { return jobs.Env{Reporter: h.rep, Items: h.jq} }

// run runs a runner on job and closes the job.
func (h *harness) run(r jobs.Runner, j jobs.Job) (jobs.Result, error) {
	res, err := r.Run(h.ctx, j, h.env())
	h.closeJob(j.ID)
	return res, err
}

// mustSync runs a sync and fails the test on an error.
func (h *harness) mustSync(dryRun bool, p jobs.Params) (jobs.Result, SyncStats, jobs.Job) {
	h.t.Helper()
	j := h.newJob(jobs.TypeSync, dryRun, p)
	res, err := h.run(h.sync, j)
	if err != nil {
		h.t.Fatalf("sync: %v\nlogs:\n%s", err, h.rep.dump())
	}
	st, ok := res.Stats.(SyncStats)
	if !ok {
		h.t.Fatalf("stats are %T", res.Stats)
	}
	return res, st, j
}

// items returns a job's items.
func (h *harness) items(jobID int64) []jobs.Item {
	h.t.Helper()
	page, err := h.jq.ListItems(h.ctx, jobID, jobqueue.ItemQuery{PageSize: jobqueue.MaxPageSize})
	if err != nil {
		h.t.Fatal(err)
	}
	return page.Records
}

func itemsBy(items []jobs.Item, action jobs.ItemAction, status jobs.ItemStatus) []string {
	var out []string
	for _, it := range items {
		if it.Action == action && (status == "" || it.Status == status) {
			out = append(out, it.RelPath)
		}
	}
	sort.Strings(out)
	return out
}

func (h *harness) records() []Record {
	h.t.Helper()
	recs, err := h.store.List(h.ctx, h.dest.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	return recs
}

func (h *harness) liveRecord(rel string) (Record, bool) {
	h.t.Helper()
	r, ok, err := h.store.LiveAt(h.ctx, h.dest.ID, rel)
	if err != nil {
		h.t.Fatal(err)
	}
	return r, ok
}

// --- filesystem inspection ---

func fileHash(t *testing.T, p string) string {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	hh := sha256.New()
	if _, err := io.Copy(hh, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(hh.Sum(nil))
}

func strHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// treeEntry describes one entry of a directory tree.
type treeEntry struct {
	mode  fs.FileMode
	size  int64
	mtime int64
	ino   uint64
}

// tree lists every entry under root (lstat) with its mode, size, mtime and inode.
func tree(t *testing.T, root string) map[string]treeEntry {
	t.Helper()
	out := map[string]treeEntry{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		e := treeEntry{mode: fi.Mode(), mtime: fi.ModTime().UnixNano()}
		if !fi.IsDir() {
			e.size = fi.Size()
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			e.ino = uint64(st.Ino)
		}
		out[filepath.ToSlash(rel)] = e
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// regularFiles returns the regular files under root (slash paths) and their sha256.
func regularFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for rel, e := range tree(t, root) {
		if e.mode.IsRegular() {
			out[rel] = fileHash(t, filepath.Join(root, filepath.FromSlash(rel)))
		}
	}
	return out
}

func inode(t *testing.T, p string) uint64 {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return uint64(fi.Sys().(*syscall.Stat_t).Ino)
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// assertConverged checks the destination against the source and the records (the crash-matrix
// invariants): every live source file is recorded with its content reachable at the destination,
// no temp file exists, every live and retained record matches the filesystem, every file of the
// mirror and of the retention tree is recorded, links point at present records.
func (h *harness) assertConverged(t *testing.T) {
	t.Helper()
	folder := h.src.DestFolder
	dstFiles := regularFiles(t, h.dstDir)
	for rel := range dstFiles {
		if filecopy.IsTempName(path.Base(rel)) {
			t.Errorf("temp file left at the destination: %s", rel)
		}
	}
	recs := h.records()
	byID := map[int64]Record{}
	live := map[string]Record{}
	retained := map[string]Record{}
	for _, r := range recs {
		byID[r.ID] = r
		if r.State.Live() {
			live[r.RelPath] = r
		} else {
			retained[r.RetainedPath] = r
		}
	}
	srcFiles := regularFiles(t, h.srcDir)
	for rel, want := range srcFiles {
		dest := path.Join(folder, rel)
		r, ok := live[dest]
		if !ok {
			t.Errorf("source file %s has no live record", rel)
			continue
		}
		holder := r
		switch r.State {
		case StateLinkRecorded:
			holder = byID[r.LinkOf]
			if holder.State != StatePresent {
				t.Errorf("%s links to record %d in state %s", dest, r.LinkOf, holder.State)
				continue
			}
		case StateMissing:
			t.Errorf("%s is recorded missing", dest)
			continue
		}
		got, ok := dstFiles[holder.RelPath]
		if !ok {
			t.Errorf("%s: no file at %s", dest, holder.RelPath)
			continue
		}
		if got != want {
			t.Errorf("%s: destination content differs from the source", dest)
		}
		fi, _ := os.Stat(h.srcPath(rel))
		if r.Size != fi.Size() {
			t.Errorf("%s: recorded size %d, source %d", dest, r.Size, fi.Size())
		}
	}
	for rel, r := range live {
		if r.SourceID == h.src.ID {
			if _, ok := srcFiles[r.SourceRelPath]; !ok {
				t.Errorf("live record %s has no source file %s", rel, r.SourceRelPath)
			}
		}
		if r.LinkOf != 0 {
			if p, ok := byID[r.LinkOf]; !ok || p.State != StatePresent {
				t.Errorf("%s links to record %d, which is not present", rel, r.LinkOf)
			}
		}
		fi, err := os.Lstat(h.dstPath(rel))
		switch r.State {
		case StatePresent, StateLinked:
			if err != nil {
				t.Errorf("record %s (%s): %v", rel, r.State, err)
			} else if fi.Size() != r.Size {
				t.Errorf("record %s: size %d, recorded %d", rel, fi.Size(), r.Size)
			} else if r.Hash != "" && r.Hash != filecopy.HashPrefix+dstFiles[rel] {
				t.Errorf("record %s: the file's content is not the recorded hash", rel)
			}
			if r.State == StateLinked && err == nil {
				if p := byID[r.LinkOf]; inode(t, h.dstPath(p.RelPath)) != inode(t, h.dstPath(rel)) {
					t.Errorf("record %s is linked but not a hardlink of %s", rel, p.RelPath)
				}
			}
		case StateLinkRecorded:
			if err == nil {
				t.Errorf("record %s is link_recorded but a file is there", rel)
			}
		}
	}
	for rel := range dstFiles {
		switch {
		case strings.HasPrefix(rel, filecopy.RetentionRoot+"/"):
			if _, ok := retained[rel]; !ok {
				t.Errorf("unrecorded file in retention: %s", rel)
			}
		case strings.HasPrefix(rel, filecopy.MetaDir+"/"):
		default:
			if _, ok := live[rel]; !ok {
				t.Errorf("unrecorded file in the mirror: %s", rel)
			}
		}
	}
	for p, r := range retained {
		fi, err := os.Lstat(h.dstPath(p))
		if err != nil {
			t.Errorf("retained record %s (%s): %v", p, r.RelPath, err)
		} else if fi.Size() != r.Size {
			t.Errorf("retained record %s: size %d, recorded %d", p, fi.Size(), r.Size)
		} else if r.Hash != "" && r.Hash != filecopy.HashPrefix+dstFiles[p] {
			t.Errorf("retained record %s: the file's content is not the recorded hash", p)
		}
	}
}

// hasContent reports whether any regular file under the destination has the given content.
func (h *harness) hasContent(t *testing.T, s string) bool {
	t.Helper()
	want := strHash(s)
	for _, got := range regularFiles(t, h.dstDir) {
		if got == want {
			return true
		}
	}
	return false
}

// recReporter records progress and log lines.
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

func (r *recReporter) phases() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, p := range r.progress {
		if len(out) == 0 || out[len(out)-1] != p.Phase {
			out = append(out, p.Phase)
		}
	}
	return out
}

var errTest = errors.New("test")

// sourceInput returns the test source's current settings as an update (enabled nil keeps it).
func sourceInput(h *harness, enabled *bool) catalog.SourceInput {
	h.t.Helper()
	src, err := h.cat.Get(h.ctx, h.src.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	return catalog.SourceInput{Name: src.Name, Path: src.Path, DestFolder: src.DestFolder, Exclude: src.Exclude, Enabled: enabled}
}

func sourceInputFor(name, p, folder string) catalog.SourceInput {
	return catalog.SourceInput{Name: name, Path: p, DestFolder: folder}
}

func jsonDecode(raw []byte, v any) error { return json.Unmarshal(raw, v) }
