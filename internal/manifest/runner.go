package manifest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/mediaindex"
	"github.com/sl0wz3r/bunkarr/internal/snapshots"
)

// Fault points of a manifest_export job (the crash matrix, design §11.2, §15).
const (
	// PointAfterWrite: manifest.json, manifest.csv and SHA256SUMS are in .partial-job<id>/.
	PointAfterWrite = "manifest.afterWrite"
	// PointBeforeRename: the partial directory is about to be renamed to the version's name.
	PointBeforeRename = "manifest.beforeRename"
	// PointAfterRename: the version is complete under its name, not recorded yet.
	PointAfterRename = "manifest.afterRename"
	// PointBeforeRecord: the manifests row is about to be inserted.
	PointBeforeRecord = "manifest.beforeRecord"
	// PointAfterRecord: the version is recorded.
	PointAfterRecord = "manifest.afterRecord"
	// PointPruneAfterTrash: a pruned version was renamed to .prune-<version>, its row is still there.
	PointPruneAfterTrash = "manifest.pruneAfterTrash"
	// PointPruneAfterUnrecord: a pruned version's row is gone, its trash directory is not.
	PointPruneAfterUnrecord = "manifest.pruneAfterUnrecord"
	// PointPruneAfterRemove: a pruned version is gone.
	PointPruneAfterRemove = "manifest.pruneAfterRemove"
)

// spaceMargin is kept free at the destination when a version is written.
const spaceMargin = 16 << 20

// Options configures NewRunner.
type Options struct {
	// DB, Catalog, Integrations and Destinations are required.
	DB           *db.DB
	Catalog      *catalog.Store
	Integrations *integrations.Store
	Destinations *destinations.Store
	// Index reads the metadata index; nil builds one over DB and Catalog.
	Index *mediaindex.Store
	// Tiers decides tiers; nil is AllFull (Phase 2).
	Tiers Tiers
	// ConfigDir is Bunkarr's absolute config directory: on-the-spot exports and downloads are
	// staged in <ConfigDir>/staging (required).
	ConfigDir string
	// Log receives events outside a job's own log; nil discards them.
	Log *slog.Logger
	// Now is the clock (tests); nil is time.Now.
	Now func() time.Time
	// Location is the time zone of the retention's days and ISO weeks; nil is time.Local.
	Location *time.Location
	// Version is the generator version written into manifests; "" is version.Version.
	Version string
	// MaxExports bounds the on-the-spot exports running at once (default 2, design §11.2).
	MaxExports int
	// MaxBuilds bounds the manifests being built and written at once, by jobs of every
	// destination and on-the-spot exports together (default DefaultMaxBuilds).
	MaxBuilds int
}

// DefaultMaxExports is how many on-the-spot exports may run at once.
const DefaultMaxExports = 2

// DefaultMaxBuilds is how many manifests are built at once. A build holds the whole library in
// memory and a connection of the database's read pool for its read transaction, so builds and
// on-the-spot exports take turns. (The job manager already runs one manifest_export job at a
// time, lock key jobqueue.ManifestBuildKey, so a queued job never waits here in a worker for
// another job; it can only wait here for an on-the-spot export's build.)
const DefaultMaxBuilds = 1

// Runner runs manifest_export jobs (jobs.TypeManifestExport) and serves the on-the-spot export
// and the downloads of recorded versions. It is safe for concurrent use (the job manager runs at
// most one manifest_export job at a time: lock keys "manifest:<destinationId>" and
// jobqueue.ManifestBuildKey).
type Runner struct {
	store        *Store
	builder      *Builder
	destinations *destinations.Store
	db           *db.DB
	configDir    string
	log          *slog.Logger
	now          func() time.Time
	loc          *time.Location
	exports      chan struct{}
	// builds holds a token per manifest being built and written (MaxBuilds).
	builds chan struct{}
}

var _ jobs.Runner = (*Runner)(nil)

// NewRunner returns the manifest_export runner.
func NewRunner(o Options) (*Runner, error) {
	switch {
	case o.Destinations == nil:
		return nil, errors.New("manifest: no destinations store")
	case !filepath.IsAbs(o.ConfigDir):
		return nil, fmt.Errorf("manifest: the config directory %q is not an absolute path", o.ConfigDir)
	}
	b, err := NewBuilder(BuilderOptions{DB: o.DB, Catalog: o.Catalog, Integrations: o.Integrations, Index: o.Index,
		Tiers: o.Tiers, Now: o.Now, Version: o.Version})
	if err != nil {
		return nil, err
	}
	r := &Runner{store: NewStore(o.DB), builder: b, destinations: o.Destinations, db: o.DB, configDir: o.ConfigDir,
		log: o.Log, now: b.o.Now, loc: o.Location}
	if r.log == nil {
		r.log = slog.New(slog.DiscardHandler)
	}
	if r.loc == nil {
		r.loc = time.Local
	}
	n := o.MaxExports
	if n <= 0 {
		n = DefaultMaxExports
	}
	r.exports = make(chan struct{}, n)
	if o.MaxBuilds <= 0 {
		o.MaxBuilds = DefaultMaxBuilds
	}
	r.builds = make(chan struct{}, o.MaxBuilds)
	return r, nil
}

// acquireBuild waits until a manifest may be built (MaxBuilds), calling waiting first when it
// has to wait, and returns the function that frees the slot (safe to call more than once). It
// returns ctx's error when ctx ends while it waits.
func (r *Runner) acquireBuild(ctx context.Context, waiting func()) (func(), error) {
	select {
	case r.builds <- struct{}{}:
	default:
		if waiting != nil {
			waiting()
		}
		select {
		case r.builds <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	var once sync.Once
	return func() { once.Do(func() { <-r.builds }) }, nil
}

// acquireBuild is Runner.acquireBuild for a job: the wait is logged.
func (w *run) acquireBuild(ctx context.Context) (func(), error) {
	return w.r.acquireBuild(ctx, func() {
		w.rep.Log(slog.LevelInfo, "Waiting for another manifest to finish building")
	})
}

// Store returns the runner's manifests store.
func (r *Runner) Store() *Store { return r.store }

// Builder returns the runner's builder.
func (r *Runner) Builder() *Builder { return r.builder }

// Stats is a manifest_export job's stats JSON (design §12.4).
type Stats struct {
	DryRun         bool  `json:"dryRun"`
	Items          int64 `json:"items"`
	UnlocatedItems int64 `json:"unlocatedItems"`
	Files          int64 `json:"files"`
	Bytes          int64 `json:"bytes"`
	// StaleIntegrations counts the *arr integrations whose cache was not fresh.
	StaleIntegrations int64 `json:"staleIntegrations"`
	// Unchanged: the newest ok version holds this content and was read back intact; nothing was
	// written.
	Unchanged bool `json:"unchanged"`
	// DamagedFound counts versions found damaged (and marked) by this job.
	DamagedFound int64 `json:"damagedFound"`
	// ManifestID and Path are the version written, or the unchanged one.
	ManifestID int64  `json:"manifestId,omitempty"`
	Path       string `json:"path,omitempty"`
	// Recovered counts complete versions an interrupted job had written, recorded by this one.
	Recovered      int64 `json:"recovered,omitempty"`
	VersionsPruned int64 `json:"versionsPruned"`
	DurationMs     int64 `json:"durationMs"`
}

// run is one job's state.
type run struct {
	r       *Runner
	job     jobs.Job
	rep     jobs.Reporter
	h       *destinations.Handle
	started time.Time
	stats   Stats
	warns   int
}

// Run writes a manifest version of destination job.Params.DestinationID (design §11.2):
//
//  1. Preflight: the destination is opened with its S3 checks and must be enabled. What earlier
//     attempts left is cleaned up: .partial-job* directories are removed, interrupted prunes
//     finished, and a complete version directory without a row is checked against its
//     SHA256SUMS and recorded (damaged when it does not match). A resumed job whose own version
//     is recorded finishes with it.
//  2. The manifest is built in one read transaction.
//  3. When its content hash equals the newest ok version's, that version is read back and
//     compared with its checksum: intact, nothing is written ("unchanged"); damaged, it is marked
//     and a new version is written.
//  4. manifest.json, manifest.csv and SHA256SUMS are written into .partial-job<id>/, which is
//     renamed to the version's timestamp and recorded.
//  5. Old versions are pruned (manifestDays, manifestWeeks).
//
// A cache that is not fresh or an item that locates into no source is a warning. A dry run
// builds the manifest and reports its counts and whether it would be unchanged; it writes
// nothing (S9).
func (r *Runner) Run(ctx context.Context, job jobs.Job, env jobs.Env) (jobs.Result, error) {
	w := &run{r: r, job: job, rep: env.Reporter, started: r.now()}
	if w.rep == nil {
		w.rep = nopReporter{}
	}
	if job.Params.DestinationID <= 0 {
		return jobs.Result{}, errors.New("a manifest export needs a destinationId")
	}
	h, err := r.destinations.Open(ctx, job.Params.DestinationID)
	if err != nil {
		return jobs.Result{}, err
	}
	defer h.Close()
	if !h.Destination.Enabled {
		return jobs.Result{}, fmt.Errorf("destination %q is disabled", h.Destination.Name)
	}
	w.h = h
	if job.DryRun {
		return w.dryRun(ctx)
	}
	return w.export(ctx)
}

// warn records a job warning and logs it.
func (w *run) warn(msg string, args ...any) {
	w.warns++
	w.rep.Log(slog.LevelWarn, msg, args...)
}

// result builds the job result.
func (w *run) result(summary string) jobs.Result {
	w.stats.DurationMs = w.r.now().Sub(w.started).Milliseconds()
	return jobs.Result{Stats: w.stats, Warnings: w.warns, Summary: summary}
}

func (w *run) jobRef() *JobRef { return &JobRef{ID: w.job.ID, QueuedAt: w.job.QueuedAt} }

// build builds the destination's manifest and records its counts and warnings.
func (w *run) build(ctx context.Context) (*Manifest, string, error) {
	w.rep.Progress(jobs.Progress{Phase: "building"})
	m, err := w.r.builder.Build(ctx, BuildScope{Destination: &w.h.Destination, Job: w.jobRef()})
	if err != nil {
		return nil, "", err
	}
	hash, err := ContentHash(m)
	if err != nil {
		return nil, "", err
	}
	w.count(m)
	return m, hash, nil
}

// count takes a manifest's counts into the stats and warns about what makes it incomplete as a
// disaster record: caches that are not fresh and items in no source (S20).
func (w *run) count(m *Manifest) {
	s := m.Summary
	w.stats.Items, w.stats.UnlocatedItems, w.stats.Files, w.stats.Bytes = s.Items, s.UnlocatedItems, s.Files, s.Bytes
	stale := m.StaleIntegrations()
	w.stats.StaleIntegrations = int64(len(stale))
	for _, it := range stale {
		w.warn(fmt.Sprintf("The %s index of %q is not fresh: the manifest lists what it last knew; refresh the integration", appName(it.Type), it.Name),
			"integrationId", it.ID, "status", it.Status)
	}
	if s.UnlocatedItems > 0 {
		var titles []string
		for _, it := range m.Items {
			if !it.Located && len(titles) < 5 {
				titles = append(titles, it.Title)
			}
		}
		w.warn(fmt.Sprintf("%d *arr items lie in no source (check the path mappings); they are listed without files at a source", s.UnlocatedItems),
			"examples", strings.Join(titles, "; "))
	}
}

// appName names an integration type for messages.
func appName(t string) string { return integrations.Type(t).AppName() }

// unchanged reports whether the newest ok version holds content hash and reads back intact
// (design §11.2 step 3). A damaged one is marked (not in a dry run) and counted.
func (w *run) unchanged(ctx context.Context, hash string, dryRun bool) (bool, error) {
	v, ok, err := w.r.store.NewestOK(ctx, w.h.Destination.ID)
	if err != nil || !ok || v.ContentHash != hash {
		return false, err
	}
	_, err = CheckVersion(w.h.Root, v.Path, v.Checksum)
	switch {
	case err == nil:
		w.stats.Unchanged, w.stats.ManifestID, w.stats.Path = true, v.ID, v.Path
		return true, nil
	case errors.Is(err, ErrDamaged):
		w.stats.DamagedFound++
		if dryRun {
			w.warn("The newest manifest version is damaged; a real run writes a new version", "path", v.Path, "reason", err.Error())
			return false, nil
		}
		if merr := w.r.store.MarkDamaged(context.WithoutCancel(ctx), v.ID); merr != nil {
			return false, merr
		}
		w.warn("The newest manifest version was damaged at the destination; it is marked damaged and a new version is written",
			"path", v.Path, "reason", err.Error())
		return false, nil
	}
	return false, fmt.Errorf("read back the newest manifest version %s: %w", v.Path, err)
}

// dryRun builds the manifest and reports what a real run would do.
func (w *run) dryRun(ctx context.Context) (jobs.Result, error) {
	w.stats.DryRun = true
	release, err := w.acquireBuild(ctx)
	if err != nil {
		return jobs.Result{}, err
	}
	defer release()
	_, hash, err := w.build(ctx)
	if err != nil {
		return jobs.Result{}, err
	}
	unchanged, err := w.unchanged(ctx, hash, true)
	if err != nil {
		return jobs.Result{}, err
	}
	what := "a new version would be written"
	if unchanged {
		what = "unchanged since the newest version"
	}
	return w.result(fmt.Sprintf("Dry run: the manifest of %q lists %d items and %d files (%s); %s", w.h.Destination.Name,
		w.stats.Items, w.stats.Files, formatBytes(w.stats.Bytes), what)), nil
}

// export runs a real export (see Run).
func (w *run) export(ctx context.Context) (res jobs.Result, err error) {
	partial := Root + "/" + snapshots.PartialName(w.job.ID)
	defer func() {
		// A failed or cancelled job leaves nothing but what it recorded. (A crash panics past
		// this with err nil and leaves its state to the resumed job, as a real crash would.)
		if err != nil {
			if rerr := removeTree(w.h.Root, partial); rerr != nil {
				w.r.log.Error("Could not remove an unfinished manifest version", "path", partial, "error", rerr)
			}
		}
	}()
	own, err := w.recover(ctx)
	if err != nil {
		return jobs.Result{}, err
	}
	if own != nil {
		return w.finishOwn(ctx, own)
	}
	release, err := w.acquireBuild(ctx)
	if err != nil {
		return jobs.Result{}, err
	}
	defer release()
	m, hash, err := w.build(ctx)
	if err != nil {
		return jobs.Result{}, err
	}
	unchanged, err := w.unchanged(ctx, hash, false)
	if err != nil {
		return jobs.Result{}, err
	}
	if unchanged {
		release()
		w.rep.Log(slog.LevelInfo, "The manifest is unchanged since the newest version, which read back intact", "path", w.stats.Path)
		w.prune(ctx)
		return w.result(fmt.Sprintf("The manifest of %q is unchanged: %d items, %d files (%s)", w.h.Destination.Name,
			w.stats.Items, w.stats.Files, formatBytes(w.stats.Bytes))), nil
	}
	v, err := w.write(ctx, m, hash, partial)
	if err != nil {
		return jobs.Result{}, err
	}
	release() // pruning needs no manifest in memory
	w.stats.ManifestID, w.stats.Path = v.ID, v.Path
	w.rep.Log(slog.LevelInfo, "Recorded the manifest version", "path", v.Path, "items", v.ItemCount, "files", v.FileCount, "checksum", v.Checksum)
	w.prune(ctx)
	return w.result(w.summary()), nil
}

// summary is a written version's summary sentence.
func (w *run) summary() string {
	s := fmt.Sprintf("Wrote the manifest of %q: %d items, %d files (%s)", w.h.Destination.Name, w.stats.Items, w.stats.Files,
		formatBytes(w.stats.Bytes))
	if w.stats.VersionsPruned > 0 {
		s += fmt.Sprintf("; %d old versions pruned", w.stats.VersionsPruned)
	}
	return s
}

// write writes and records a version (design §11.2 step 5).
func (w *run) write(ctx context.Context, m *Manifest, hash, partial string) (Version, error) {
	w.rep.Progress(jobs.Progress{Phase: "writing"})
	// The files are streamed (a large library's manifest is hundreds of megabytes): their sizes
	// are counted first for the free-space check, then they are written through temp files.
	writeJSON := func(out io.Writer) error { return WriteJSON(out, m) }
	writeCSV := func(out io.Writer) error { return WriteCSV(out, m) }
	jsonSize, err := encodedSize(writeJSON)
	if err != nil {
		return Version{}, err
	}
	csvSize, err := encodedSize(writeCSV)
	if err != nil {
		return Version{}, err
	}
	if err := w.h.Recheck(); err != nil {
		return Version{}, err
	}
	need := uint64(jsonSize + csvSize + spaceMargin)
	if free, _, err := filecopy.FreeSpace(w.h.Root); err == nil && free < need {
		return Version{}, fmt.Errorf("not enough free space at destination %q for the manifest: it needs %s, %s is free",
			w.h.Destination.Name, formatBytes(int64(need)), formatBytes(int64(free)))
	}
	if err := realDirsIfPresent(w.h.Root, Root); err != nil {
		return Version{}, err
	}
	jsonHex, err := writeFileStream(w.h.Root, partial+"/"+JSONName, 0o644, writeJSON)
	if err != nil {
		return Version{}, fmt.Errorf("write %s: %w", JSONName, err)
	}
	csvHex, err := writeFileStream(w.h.Root, partial+"/"+CSVName, 0o644, writeCSV)
	if err != nil {
		return Version{}, fmt.Errorf("write %s: %w", CSVName, err)
	}
	if err := filecopy.WriteFileAtomic(w.h.Root, partial+"/"+SumsName, FormatSums(jsonHex, csvHex), 0o644, true); err != nil {
		return Version{}, fmt.Errorf("write %s: %w", SumsName, err)
	}
	faultinject.Point(PointAfterWrite)
	faultinject.Point(PointBeforeRename)
	rel, err := snapshots.RenameVersion(w.h.Root, partial, Root, snapshots.VersionName(m.CreatedAt), w.job.ID)
	if err != nil {
		return Version{}, err
	}
	faultinject.Point(PointAfterRename)
	faultinject.Point(PointBeforeRecord)
	// The version is complete at its final name: record it even if the job is being cancelled.
	v, err := w.r.store.Insert(context.WithoutCancel(ctx), Version{DestinationID: w.h.Destination.ID, JobID: w.job.ID,
		CreatedAt: m.CreatedAt, Path: rel, Format: FormatVersion, ItemCount: m.Summary.Items, FileCount: m.Summary.Files,
		Bytes: m.Summary.Bytes, Checksum: Checksum(jsonHex), ContentHash: hash, Integrity: IntegrityOK})
	if err != nil {
		return Version{}, err
	}
	faultinject.Point(PointAfterRecord)
	return v, nil
}

// recover cleans up after interrupted jobs of this destination (see Run step 1) and returns this
// job's own version when an earlier attempt recorded it (or this recovery did).
func (w *run) recover(ctx context.Context) (*ownVersion, error) {
	root := w.h.Root
	names, err := readDirNames(root, Root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", Root, err)
	}
	if err := realDirs(root, Root); err != nil {
		return nil, fmt.Errorf("check %s: %w", Root, err)
	}
	slices.Sort(names)
	for _, name := range names {
		rel := Root + "/" + name
		switch {
		case snapshots.IsPartialName(name):
			// Jobs of one destination never run at the same time: no job is writing it now.
			if err := removeTree(root, rel); err != nil {
				return nil, err
			}
			w.rep.Log(slog.LevelInfo, "Removed the unfinished version of an interrupted manifest export", "path", rel)
		case isPruned(name):
			v, _ := snapshots.PrunedVersion(name)
			recorded, err := w.r.store.Recorded(ctx, w.h.Destination.ID, Root+"/"+v)
			if err != nil {
				return nil, err
			}
			if recorded {
				continue // the next prune deletes it, or restores it when it is kept after all
			}
			if err := removeTree(root, rel); err != nil {
				w.warn("The rest of a pruned manifest version could not be deleted", "path", rel, "error", err.Error())
				continue
			}
			w.rep.Log(slog.LevelInfo, "Removed the rest of a manifest version an interrupted prune was deleting", "path", rel)
		case snapshots.IsVersionName(name):
			recorded, err := w.r.store.Recorded(ctx, w.h.Destination.ID, rel)
			if err != nil {
				return nil, err
			}
			if recorded {
				continue
			}
			if fi, err := root.Lstat(rel); err != nil || !fi.IsDir() {
				w.warn("An unrecorded entry among the manifest versions was left alone", "path", rel, "reason", "not a directory")
				continue
			}
			if err := w.adopt(ctx, rel); err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				w.warn("An unrecorded manifest version was left alone", "path", rel, "reason", err.Error())
			}
		}
	}
	return w.ownVersion(ctx)
}

func isPruned(name string) bool {
	_, ok := snapshots.PrunedVersion(name)
	return ok
}

// adopt checks a complete but unrecorded version directory against its SHA256SUMS and records it
// (damaged when it does not match). A directory without a readable manifest.json is not
// recorded.
func (w *run) adopt(ctx context.Context, rel string) error {
	m, c, err := ReadVersion(w.h.Root, rel, "")
	if m == nil {
		if err == nil {
			err = errors.New("no manifest")
		}
		return err
	}
	integrity := IntegrityOK
	if err != nil {
		integrity = IntegrityDamaged
		w.warn("An unrecorded manifest version does not match its SHA256SUMS; it is recorded as damaged", "path", rel, "reason", err.Error())
	}
	hash, herr := ContentHash(m)
	if herr != nil {
		return herr
	}
	created := m.CreatedAt
	if created.IsZero() {
		created = w.r.now()
	}
	_, err = w.r.store.Insert(context.WithoutCancel(ctx), Version{DestinationID: w.h.Destination.ID, JobID: w.job.ID, CreatedAt: created,
		Path: rel, Format: m.FormatVersion, ItemCount: m.Summary.Items, FileCount: m.Summary.Files, Bytes: m.Summary.Bytes,
		Checksum: Checksum(c.JSON), ContentHash: hash, Integrity: integrity})
	if err != nil {
		return err
	}
	w.stats.Recovered++
	writer := int64(0)
	if m.Job != nil {
		writer = m.Job.ID
	}
	w.rep.Log(slog.LevelInfo, "Recorded a manifest version an interrupted export had written", "path", rel, "integrity", integrity,
		"writtenByJob", writer)
	return nil
}

// ownVersion is a recorded version this job wrote in an earlier attempt, with its manifest.
type ownVersion struct {
	v Version
	m *Manifest
}

// ownVersion returns, for a resumed job, the intact recorded version whose manifest names this
// job (its id and queue time): an earlier attempt wrote it completely. A damaged one is marked
// and the export starts over.
func (w *run) ownVersion(ctx context.Context) (*ownVersion, error) {
	if w.job.Trigger != jobs.TriggerResume && w.job.Attempt <= 1 {
		return nil, nil
	}
	list, err := w.r.store.List(ctx, w.h.Destination.ID)
	if err != nil {
		return nil, err
	}
	for _, v := range list {
		if v.JobID != w.job.ID || v.Integrity != IntegrityOK {
			continue
		}
		m, _, err := ReadVersion(w.h.Root, v.Path, v.Checksum)
		switch {
		case errors.Is(err, ErrDamaged):
			if merr := w.r.store.MarkDamaged(context.WithoutCancel(ctx), v.ID); merr != nil {
				return nil, merr
			}
			w.stats.DamagedFound++
			w.warn("A manifest version of this job's earlier attempt is damaged; it is marked damaged", "path", v.Path, "reason", err.Error())
			continue
		case err != nil:
			return nil, err
		}
		if m.Job != nil && m.Job.ID == w.job.ID && m.Job.QueuedAt.Equal(w.job.QueuedAt) {
			return &ownVersion{v: v, m: m}, nil
		}
	}
	return nil, nil
}

// finishOwn completes a resumed job whose version an earlier attempt wrote, as that attempt
// would have: with its counts and warnings, then pruning.
func (w *run) finishOwn(ctx context.Context, own *ownVersion) (jobs.Result, error) {
	w.rep.Log(slog.LevelInfo, "An earlier attempt of this job wrote its manifest version; finishing with it", "path", own.v.Path)
	w.count(own.m)
	w.stats.ManifestID, w.stats.Path = own.v.ID, own.v.Path
	w.prune(ctx)
	return w.result(w.summary()), nil
}

// prune applies the retention to the destination's versions after a successful export; rows of
// versions gone from the destination are removed first. Problems are warnings: the export
// itself succeeded.
func (w *run) prune(ctx context.Context) {
	if err := w.h.Recheck(); err != nil {
		w.warn("Old manifest versions were not pruned", "error", err.Error())
		return
	}
	keep, err := keepOf(ctx, w.r.db.Reader(), w.h.Destination.ID)
	if err != nil {
		w.warn("Old manifest versions were not pruned", "error", err.Error())
		return
	}
	list, err := w.r.store.List(ctx, w.h.Destination.ID)
	if err != nil {
		w.warn("Old manifest versions were not pruned", "error", err.Error())
		return
	}
	var cands []Retained
	byID := map[int64]Version{}
	var present []Version
	for _, v := range list {
		if w.dropLost(ctx, v) {
			continue
		}
		cands = append(cands, Retained{ID: v.ID, CreatedAt: v.CreatedAt, OK: v.Integrity == IntegrityOK})
		byID[v.ID] = v
		present = append(present, v)
	}
	remove := PruneSet(cands, keep.Days, keep.Weeks, w.r.now(), w.r.loc)
	removeSet := map[int64]bool{}
	for _, id := range remove {
		removeSet[id] = true
	}
	for _, v := range present {
		if removeSet[v.ID] {
			continue
		}
		if restored, err := restoreVersion(w.h.Root, v); err != nil {
			w.warn("A kept manifest version could not be restored from an interrupted prune", "path", v.Path, "error", err.Error())
		} else if restored {
			w.rep.Log(slog.LevelInfo, "Restored a kept manifest version from an interrupted prune", "path", v.Path)
		}
	}
	for _, id := range remove {
		if ctx.Err() != nil {
			return
		}
		v := byID[id]
		// The version leaves its name, then its row, then its files.
		trash, err := trashVersion(w.h.Root, v.Path)
		if err != nil {
			w.warn("An old manifest version could not be deleted", "path", v.Path, "error", err.Error())
			continue
		}
		faultinject.Point(PointPruneAfterTrash)
		if err := w.r.store.Remove(context.WithoutCancel(ctx), id); err != nil {
			w.warn("An old manifest version was not deleted: its record could not be removed", "path", v.Path, "error", err.Error())
			continue
		}
		faultinject.Point(PointPruneAfterUnrecord)
		w.stats.VersionsPruned++
		w.rep.Log(slog.LevelInfo, "Pruned an old manifest version", "path", v.Path, "createdAt", v.CreatedAt, "integrity", v.Integrity)
		if err := snapshots.RemoveTrash(w.h.Root, trash); err != nil {
			w.warn("The files of a pruned manifest version could not be deleted; the next export retries", "path", trash, "error", err.Error())
			continue
		}
		faultinject.Point(PointPruneAfterRemove)
	}
}

// dropLost removes the row of a version that is gone from the destination (its directory and any
// complete .prune-* copy of it are missing), then what is left of its trash. It reports whether
// it removed the row.
func (w *run) dropLost(ctx context.Context, v Version) bool {
	if lost, err := versionLost(w.h.Root, v); err != nil || !lost {
		return false
	}
	if err := w.r.store.Remove(ctx, v.ID); err != nil {
		w.warn("A manifest version missing at the destination is still recorded: its record could not be removed", "path", v.Path, "error", err.Error())
		return false
	}
	w.warn("A recorded manifest version was missing at the destination; its record was removed", "path", v.Path, "createdAt", v.CreatedAt)
	if err := snapshots.RemoveTrash(w.h.Root, trashPath(v.Path)); err != nil {
		w.warn("The rest of a manifest version could not be deleted; the next export retries", "path", trashPath(v.Path), "error", err.Error())
	}
	return true
}

// isVersionPath reports whether rel is "<Root>/<version name>", the only kind of directory a prune
// deletes.
func isVersionPath(rel string) bool {
	dir, name := path.Split(rel)
	return dir == Root+"/" && snapshots.IsVersionName(name)
}

// trashPath is where a version goes while it is deleted: ".prune-<version>" next to it.
func trashPath(rel string) string { return Root + "/.prune-" + path.Base(rel) }

// trashVersion renames the version directory rel to its trash name (a stale trash of the same
// name is removed first) and returns the trash's path. A missing rel is not an error (an
// interrupted prune renamed it already).
func trashVersion(root *os.Root, rel string) (string, error) {
	if !isVersionPath(rel) {
		return "", fmt.Errorf("refusing to delete %q: not a manifest version directory", rel)
	}
	if err := realDirs(root, Root); err != nil {
		return "", fmt.Errorf("delete %s: %w", rel, err)
	}
	trash := trashPath(rel)
	fi, err := root.Lstat(rel)
	switch {
	case err == nil && fi.IsDir():
		if err := snapshots.RemoveTrash(root, trash); err != nil {
			return "", err
		}
		if err := filecopy.RenameDir(root, rel, trash); err != nil {
			return "", fmt.Errorf("delete %s: %w", rel, err)
		}
	case err == nil:
		return "", fmt.Errorf("delete %s: not a directory", rel)
	case !errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("delete %s: %w", rel, err)
	}
	return trash, nil
}

// restoreVersion moves a recorded version back from its trash when an interrupted prune had
// selected it and it is kept after all. Only a complete trash (matching the recorded checksum) is
// moved back.
func restoreVersion(root *os.Root, v Version) (bool, error) {
	if !isVersionPath(v.Path) {
		return false, nil
	}
	if _, err := root.Lstat(v.Path); !errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	trash := trashPath(v.Path)
	fi, err := root.Lstat(trash)
	if err != nil || !fi.IsDir() {
		return false, nil
	}
	if err := realDirs(root, Root); err != nil {
		return false, err
	}
	if _, err := CheckVersion(root, trash, v.Checksum); err != nil {
		return false, err
	}
	if err := filecopy.RenameDir(root, trash, v.Path); err != nil {
		return false, err
	}
	return true, nil
}

// versionLost reports whether a recorded version is gone: its directory does not exist and
// neither does a complete trash copy of it. When it cannot tell, it reports false.
func versionLost(root *os.Root, v Version) (bool, error) {
	if !isVersionPath(v.Path) {
		return false, nil
	}
	if _, err := root.Lstat(v.Path); !errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err := realDirs(root, Root); err != nil {
		return false, err
	}
	fi, err := root.Lstat(trashPath(v.Path))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return true, nil
	case err != nil:
		return false, err
	case !fi.IsDir():
		return false, nil
	}
	_, err = CheckVersion(root, trashPath(v.Path), v.Checksum)
	if errors.Is(err, ErrDamaged) {
		return true, nil
	}
	return false, err
}

// removeTree removes the directory rel under Root (every directory on the way must be a real
// directory); a missing rel is not an error.
func removeTree(root *os.Root, rel string) error {
	if !strings.HasPrefix(rel, Root+"/") || strings.Contains(strings.TrimPrefix(rel, Root+"/"), "/") {
		return fmt.Errorf("refusing to remove %q: not a directory of %s", rel, Root)
	}
	fi, err := root.Lstat(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove %s: %w", rel, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("remove %s: not a directory", rel)
	}
	if err := realDirs(root, rel); err != nil {
		return fmt.Errorf("remove %s: %w", rel, err)
	}
	if err := root.RemoveAll(rel); err != nil {
		return fmt.Errorf("remove %s: %w", rel, err)
	}
	return nil
}

// realDirsIfPresent checks the directories of rel that exist are real directories.
func realDirsIfPresent(root *os.Root, rel string) error {
	if err := realDirs(root, rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("check %s: %w", rel, err)
	}
	return nil
}

// readDirNames lists a directory inside root.
func readDirNames(root *os.Root, rel string) ([]string, error) {
	f, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Readdirnames(-1)
}

// formatBytes renders n in binary units (1.5 GiB).
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// nopReporter discards progress and logs (a runner driven without a job manager).
type nopReporter struct{}

// Progress implements jobs.Reporter.
func (nopReporter) Progress(jobs.Progress) {}

// Log implements jobs.Reporter.
func (nopReporter) Log(slog.Level, string, ...any) {}
