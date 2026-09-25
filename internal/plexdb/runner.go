package plexdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/integrations/plex"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

const (
	// StagingRoot is the directory under the config directory that holds staging directories.
	StagingRoot = "staging"
	// plexCheckTimeout bounds the optional Plex API calls of a backup job.
	plexCheckTimeout = 10 * time.Second
	// spaceMargin is kept free at the destination and in the staging filesystem.
	spaceMargin = 16 << 20
	// staleStaging is the age after which another job's staging directory is removed.
	staleStaging = 24 * time.Hour
	// maxManifestBytes bounds how much of a manifest is read.
	maxManifestBytes = 1 << 20
	// manifestFormat is the Manifest.Format this package writes.
	manifestFormat = 1
)

// Manifest is a version's manifest.json (design §2).
type Manifest struct {
	// Format is the manifest format (1).
	Format    int       `json:"format"`
	CreatedAt time.Time `json:"createdAt"`
	// Method is the library database's backup method.
	Method string `json:"method"`
	// PlexVersion is the Plex Media Server version, when Plex answered during the backup.
	PlexVersion     string `json:"plexVersion,omitempty"`
	SQLiteVersion   string `json:"sqliteVersion"`
	IntegrationID   int64  `json:"integrationId"`
	IntegrationName string `json:"integrationName"`
	JobID           int64  `json:"jobId"`
	// JobQueuedAt is when job JobID was queued. With the id it names the job: ids start over
	// with a new Bunkarr database while the destination keeps its versions.
	JobQueuedAt time.Time `json:"jobQueuedAt,omitzero"`
	// Result is IntegrityOK when every database passed Verify, else IntegrityFailed.
	Result string `json:"result"`
	// Integrity is the library database's verification.
	Integrity IntegrityReport `json:"integrity"`
	Files     []ManifestFile  `json:"files"`
	// Warnings are the backup's warnings (Preferences.xml not backed up, Plex's maintenance
	// window); a resumed job that finishes with the version reports them again.
	Warnings []string `json:"warnings,omitempty"`
}

// ManifestFile is one file of a version.
type ManifestFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	// SHA256 is the lowercase hex sha256 of the file.
	SHA256 string `json:"sha256"`
	Method string `json:"method"`
	// Integrity is the file's verification (databases only).
	Integrity *IntegrityReport `json:"integrity,omitempty"`
}

// Stats is a plexdb_backup job's stats JSON.
type Stats struct {
	DryRun bool `json:"dryRun,omitempty"`
	// Files and Bytes are what was backed up (a dry run: what would be).
	Files int64 `json:"files"`
	Bytes int64 `json:"bytes"`
	// Method is the library database's backup method.
	Method string `json:"method,omitempty"`
	// Integrity is the version's result (ok or failed).
	Integrity string `json:"integrity,omitempty"`
	// SnapshotID and Path identify the recorded version.
	SnapshotID int64  `json:"snapshotId,omitempty"`
	Path       string `json:"path,omitempty"`
	// MetadataItems and MediaParts are the library's row counts.
	MetadataItems int64 `json:"metadataItems,omitempty"`
	MediaParts    int64 `json:"mediaParts,omitempty"`
	// Recovered counts versions an interrupted job had written completely and this job recorded.
	Recovered int64 `json:"recovered,omitempty"`
	// Pruned counts versions deleted by the retention rules.
	Pruned     int64 `json:"pruned"`
	DurationMs int64 `json:"durationMs"`
}

// Options configures a Runner.
type Options struct {
	DB           *db.DB
	Integrations *integrations.Store
	Destinations *destinations.Store
	// ConfigDir is Bunkarr's config directory; staging directories are
	// <ConfigDir>/staging/plexdb-job<id>.
	ConfigDir string
	// Log receives events outside a job's own log (cleanup problems); nil discards them.
	Log *slog.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Location is the time zone of ISO weeks (pruning) and of the hour compared with Plex's
	// butler window; nil means time.Local (the container's TZ).
	Location *time.Location
	// Plex configures the client of the optional Plex API calls (butler window, version).
	Plex plex.Options
	// BusyTimeout and Attempts are passed to Backup (zero: its defaults).
	BusyTimeout time.Duration
	Attempts    int
}

// Runner runs jobs.TypePlexDBBackup jobs. It is safe for concurrent use (the job manager runs at
// most one job per integration: lock key "plexdb:<integrationId>").
type Runner struct {
	store        *Store
	integrations *integrations.Store
	destinations *destinations.Store
	configDir    string
	log          *slog.Logger
	now          func() time.Time
	loc          *time.Location
	plexOpts     plex.Options
	busyTimeout  time.Duration
	attempts     int
}

var _ jobs.Runner = (*Runner)(nil)

// NewRunner returns the plexdb_backup runner. DB, Integrations, Destinations and an absolute
// ConfigDir are required.
func NewRunner(o Options) (*Runner, error) {
	switch {
	case o.DB == nil:
		return nil, errors.New("plexdb: no database")
	case o.Integrations == nil:
		return nil, errors.New("plexdb: no integrations store")
	case o.Destinations == nil:
		return nil, errors.New("plexdb: no destinations store")
	case !filepath.IsAbs(o.ConfigDir):
		return nil, fmt.Errorf("plexdb: the config directory %q is not an absolute path", o.ConfigDir)
	}
	r := &Runner{
		store: NewStore(o.DB), integrations: o.Integrations, destinations: o.Destinations,
		configDir: o.ConfigDir, log: o.Log, now: o.Now, loc: o.Location, plexOpts: o.Plex,
		busyTimeout: o.BusyTimeout, attempts: o.Attempts,
	}
	if r.log == nil {
		r.log = slog.New(slog.DiscardHandler)
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.loc == nil {
		r.loc = time.Local
	}
	if r.plexOpts.Timeout <= 0 {
		r.plexOpts.Timeout = plexCheckTimeout
	}
	return r, nil
}

// Store returns the runner's snapshots store.
func (r *Runner) Store() *Store { return r.store }

// StagingDir is the staging directory of job jobID.
func (r *Runner) StagingDir(jobID int64) string {
	return filepath.Join(r.configDir, StagingRoot, "plexdb-job"+strconv.FormatInt(jobID, 10))
}

// run is one job's state.
type run struct {
	r       *Runner
	job     jobs.Job
	rep     jobs.Reporter
	items   jobs.ItemStore
	it      integrations.Integration
	ps      integrations.PlexSettings
	h       *destinations.Handle
	folder  string
	started time.Time
	stats   Stats
	warns   int
	// noted are the warnings about the version being written (its manifest keeps them).
	noted []string
}

// Run backs up the Plex database of integration job.Params.IntegrationID to destination
// job.Params.DestinationID (0: the integration's backup destination), design §5:
//
//  1. Preflight: a Plex integration with a data path, both enabled; the destination opened with
//     its S3 checks (marker, filesystem type).
//  2. What an earlier attempt left is cleaned up: the staging directory and ".partial-job*"
//     directories of this integration are removed; a complete version directory without a row
//     (the process stopped between its rename and the insert) is verified against its manifest
//     and recorded. When the job is resumed and a recorded version is its own (the manifest names
//     its id and queue time), the job finishes with it as the attempt that wrote it would have
//     (its warnings, step 6) without backing up again. Otherwise the backup starts over.
//  3. Butler window: when Plex answers, a backup inside its maintenance window is a warning (the
//     job never fails for this) and the Plex version goes into the manifest.
//  4. Backup into <config>/staging/plexdb-job<id>/, Verify of each database there.
//  5. Each file is copied with the filecopy engine (S7: temp file, sha256 compared with the
//     staged copy, re-read unless the destination's verify mode is off, rename) into
//     .bunkarr/plex/<folder>/.partial-job<id>/; manifest.json is written; the directory is
//     renamed to the version's timestamp; the snapshot row is inserted; staging is removed.
//  6. When the version's integrity is ok, older versions are pruned (selectPrune); otherwise the
//     version is kept (7 days) and the job fails with ErrIntegrity.
//
// A dry run checks the same preconditions and reports, without staging or writing anything, what
// would be backed up: one skipped "backup" item per file with its size and WAL state.
func (r *Runner) Run(ctx context.Context, job jobs.Job, env jobs.Env) (jobs.Result, error) {
	w := &run{r: r, job: job, rep: env.Reporter, items: env.Items, started: r.now()}
	if w.rep == nil {
		w.rep = nopReporter{}
	}
	if w.items == nil {
		w.items = nopItems{}
	}
	if job.Params.IntegrationID <= 0 {
		return jobs.Result{}, errors.New("a Plex DB backup needs an integrationId")
	}
	var err error
	if w.it, w.ps, err = r.loadIntegration(ctx, job.Params.IntegrationID); err != nil {
		return jobs.Result{}, err
	}
	destID := job.Params.DestinationID
	if destID == 0 {
		destID = w.ps.Backup.DestinationID
	}
	if destID == 0 {
		return jobs.Result{}, fmt.Errorf("Plex %q has no backup destination", w.it.Name)
	}
	if w.h, err = r.destinations.Open(ctx, destID); err != nil {
		return jobs.Result{}, err
	}
	defer w.h.Close()
	if !w.h.Destination.Enabled {
		return jobs.Result{}, fmt.Errorf("destination %q is disabled", w.h.Destination.Name)
	}
	w.folder = FolderName(w.it.Name, w.it.ID)
	if job.DryRun {
		return w.dryRun(ctx)
	}
	return w.backup(ctx)
}

// loadIntegration returns an enabled Plex integration with a data path.
func (r *Runner) loadIntegration(ctx context.Context, id int64) (integrations.Integration, integrations.PlexSettings, error) {
	it, err := r.integrations.Get(ctx, id)
	if err != nil {
		return integrations.Integration{}, integrations.PlexSettings{}, fmt.Errorf("integration %d: %w", id, err)
	}
	ps, err := it.PlexSettings()
	if err != nil {
		return integrations.Integration{}, integrations.PlexSettings{}, err
	}
	switch {
	case !it.Enabled:
		return integrations.Integration{}, integrations.PlexSettings{}, fmt.Errorf("Plex %q is disabled", it.Name)
	case ps.DataPath == "":
		return integrations.Integration{}, integrations.PlexSettings{}, fmt.Errorf("Plex %q has no data path: set the Plex Media Server directory mounted into Bunkarr", it.Name)
	}
	return it, ps, nil
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

// relOf is the item path of a backed-up file (relative to the Plex data path).
func relOf(name string) string {
	if name == PreferencesXML {
		return PreferencesXML
	}
	return dbRel(name)
}

// itemDetail is the detail of a "backup" item.
type itemDetail struct {
	Name        string `json:"name"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256,omitempty"`
	Method      string `json:"method,omitempty"`
	Temp        string `json:"temp,omitempty"`
	WALPresent  bool   `json:"walPresent,omitempty"`
	WALSize     int64  `json:"walSize,omitempty"`
	Problem     string `json:"problem,omitempty"`
}

func (d itemDetail) json() json.RawMessage {
	b, err := json.Marshal(d)
	if err != nil {
		return json.RawMessage("{}")
	}
	return b
}

// backup runs a real backup (see Run).
func (w *run) backup(ctx context.Context) (res jobs.Result, err error) {
	staging := w.r.StagingDir(w.job.ID)
	partial := w.folder + "/" + partialName(w.job.ID)
	defer func() {
		// A failed or cancelled job leaves nothing but what it recorded. (A crash panics past
		// this with err nil and leaves its state to the resumed job, as a real crash would.)
		if err != nil {
			if rerr := os.RemoveAll(staging); rerr != nil {
				w.r.log.Error("Could not remove a Plex DB staging directory", "path", staging, "error", rerr)
			}
			if rerr := removeTree(w.h.Root, partial); rerr != nil {
				w.r.log.Error("Could not remove an unfinished Plex DB version", "path", partial, "error", rerr)
			}
		}
	}()

	if err := os.RemoveAll(staging); err != nil {
		return jobs.Result{}, fmt.Errorf("remove the staging directory of an earlier attempt: %w", err)
	}
	w.r.cleanStaleStaging(w.job.ID)
	own, err := w.recover(ctx)
	if err != nil {
		return jobs.Result{}, err
	}
	if own != nil {
		return w.finishRecovered(ctx, *own)
	}
	if err := w.items.DeleteItems(ctx, w.job.ID); err != nil {
		return jobs.Result{}, err
	}
	plexVersion := w.plexInfo(ctx)
	if err := w.checkSpace(); err != nil {
		return jobs.Result{}, err
	}

	w.rep.Progress(jobs.Progress{Phase: "backing-up", CurrentFile: LibraryDB})
	br, err := Backup(ctx, BackupOptions{DataPath: w.ps.DataPath, StagingDir: staging, BusyTimeout: w.r.busyTimeout,
		Attempts: w.r.attempts, Log: w.rep.Log})
	if err != nil {
		return jobs.Result{}, err
	}
	w.warns += len(br.Warnings) // Backup logged them
	w.noted = append(w.noted, br.Warnings...)
	faultinject.Point(PointAfterStage)

	reports, integrity, err := w.verifyStaged(ctx, br)
	if err != nil {
		return jobs.Result{}, err
	}
	created := w.r.now().UTC()
	versionName := created.Format(VersionLayout)
	byRel, err := w.addItems(ctx, br, w.folder+"/"+versionName)
	if err != nil {
		return jobs.Result{}, err
	}

	if err := w.h.Recheck(); err != nil {
		return jobs.Result{}, err
	}
	if err := w.copyFiles(ctx, br, staging, partial, byRel); err != nil {
		return jobs.Result{}, err
	}
	m := Manifest{Format: manifestFormat, CreatedAt: created, Method: br.Method, PlexVersion: plexVersion,
		SQLiteVersion: br.SQLiteVersion, IntegrationID: w.it.ID, IntegrationName: w.it.Name, JobID: w.job.ID,
		JobQueuedAt: w.job.QueuedAt, Result: integrity, Integrity: reports[LibraryDB], Files: []ManifestFile{},
		Warnings: w.noted}
	for _, f := range br.Files {
		mf := ManifestFile{Name: f.Name, Size: f.Size, SHA256: f.SHA256, Method: f.Method}
		if rep, ok := reports[f.Name]; ok {
			mf.Integrity = &rep
		}
		m.Files = append(m.Files, mf)
	}
	manifest, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return jobs.Result{}, fmt.Errorf("encode the manifest: %w", err)
	}
	if err := filecopy.WriteFileAtomic(w.h.Root, partial+"/"+ManifestName, append(manifest, '\n'), 0o644, true); err != nil {
		return jobs.Result{}, fmt.Errorf("write the manifest: %w", err)
	}

	faultinject.Point(PointBeforeRename)
	versionRel, err := renameVersion(w.h.Root, partial, w.folder, versionName, w.job.ID)
	if err != nil {
		return jobs.Result{}, err
	}
	faultinject.Point(PointAfterRename)
	faultinject.Point(PointBeforeRecord)
	// The version is complete at its final name: record it even if the job is being cancelled.
	rec := Snapshot{DestinationID: w.h.Destination.ID, IntegrationID: w.it.ID, JobID: w.job.ID, Path: versionRel,
		CreatedAt: created, Size: br.Size(), Method: br.Method, Integrity: integrity, Manifest: manifest}
	snap, err := w.r.store.insert(context.WithoutCancel(ctx), rec)
	if err != nil {
		if _, gerr := w.r.integrations.Get(context.WithoutCancel(ctx), w.it.ID); errors.Is(gerr, integrations.ErrNotFound) {
			// The integration was deleted during the backup: no later job would record or prune
			// the version, so it is recorded without the link, like the integration's other
			// versions.
			rec.IntegrationID = 0
			if snap, err = w.r.store.insert(context.WithoutCancel(ctx), rec); err == nil {
				w.warn("The Plex integration was deleted during the backup; its version was recorded without it", "path", versionRel)
			}
		}
	}
	if err != nil {
		return jobs.Result{}, err
	}
	faultinject.Point(PointAfterRecord)
	if err := os.RemoveAll(staging); err != nil {
		w.r.log.Error("Could not remove a Plex DB staging directory", "path", staging, "error", err)
	}
	w.rep.Log(slog.LevelInfo, "Recorded the Plex DB version", "path", versionRel, "integrity", integrity, "size", br.Size())

	lib := reports[LibraryDB]
	w.stats.Files, w.stats.Bytes = int64(len(br.Files)), br.Size()
	w.stats.Method, w.stats.Integrity = br.Method, integrity
	w.stats.SnapshotID, w.stats.Path = snap.ID, snap.Path
	w.stats.MetadataItems, w.stats.MediaParts = lib.MetadataItems, lib.MediaParts
	if integrity != IntegrityOK {
		return w.failedVerification(reports)
	}
	w.prune(ctx)
	return w.result(w.summary()), nil
}

// failedVerification is the outcome of a job whose recorded version failed verification (kept,
// no pruning).
func (w *run) failedVerification(reports map[string]IntegrityReport) (jobs.Result, error) {
	res := w.result(fmt.Sprintf("The Plex database backup of %q failed verification; the version is kept for 7 days for diagnosis", w.it.Name))
	return res, fmt.Errorf("%w: %s", ErrIntegrity, strings.Join(firstErrors(reports, 3), "; "))
}

// summary is a successful backup's summary sentence.
func (w *run) summary() string {
	s := fmt.Sprintf("Backed up the Plex database of %q to %q: %d files, %s, integrity ok", w.it.Name,
		w.h.Destination.Name, w.stats.Files, formatBytes(w.stats.Bytes))
	if w.stats.Pruned > 0 {
		s += fmt.Sprintf("; %d old versions pruned", w.stats.Pruned)
	}
	return s
}

// firstErrors returns up to n problem lines of the reports, library first.
func firstErrors(reports map[string]IntegrityReport, n int) []string {
	var out []string
	for _, name := range []string{LibraryDB, BlobsDB} {
		for _, e := range reports[name].Errors {
			if len(out) == n {
				return out
			}
			out = append(out, name+": "+e)
		}
	}
	return out
}

// verifyStaged verifies each staged database and returns the reports by file name and the
// version's integrity.
func (w *run) verifyStaged(ctx context.Context, br BackupResult) (map[string]IntegrityReport, string, error) {
	reports := map[string]IntegrityReport{}
	integrity := IntegrityOK
	for i, f := range br.Files {
		if f.Name == PreferencesXML {
			continue
		}
		w.rep.Progress(jobs.Progress{Phase: "verifying", FilesTotal: int64(len(br.Files)), FilesDone: int64(i), CurrentFile: f.Name})
		rep, err := Verify(ctx, f.Path)
		if err != nil {
			return nil, "", fmt.Errorf("verify %s: %w", f.Name, err)
		}
		if f.Name == LibraryDB && !rep.PlexLibrary {
			rep.Errors = append(rep.Errors, "not a Plex library database: the metadata_items and media_parts tables are missing")
		}
		reports[f.Name] = rep
		if rep.OK() {
			w.rep.Log(slog.LevelInfo, "The Plex database copy passed verification", "file", f.Name,
				"ignoredLines", rep.IgnoredLines, "metadataItems", rep.MetadataItems, "mediaParts", rep.MediaParts)
			continue
		}
		integrity = IntegrityFailed
		w.rep.Log(slog.LevelError, "The Plex database copy failed verification", "file", f.Name,
			"quickCheck", rep.QuickCheck, "integrityCheck", rep.IntegrityCheck, "errors", strings.Join(rep.Errors, "; "))
	}
	return reports, integrity, nil
}

// addItems records one pending "backup" item per staged file and returns them by item path.
func (w *run) addItems(ctx context.Context, br BackupResult, versionRel string) (map[string]jobs.Item, error) {
	list := make([]jobs.Item, 0, len(br.Files))
	for _, f := range br.Files {
		d := itemDetail{Name: f.Name, Source: f.Source, Destination: versionRel + "/" + f.Name, Size: f.Size, SHA256: f.SHA256, Method: f.Method}
		list = append(list, jobs.Item{RelPath: relOf(f.Name), Action: jobs.ActionBackup, Status: jobs.ItemPending, Bytes: f.Size, Detail: d.json()})
	}
	if err := w.items.AddItems(ctx, w.job.ID, list, true); err != nil {
		return nil, err
	}
	pending, err := w.items.Pending(ctx, w.job.ID, 0, len(list)+1)
	if err != nil {
		return nil, err
	}
	byRel := map[string]jobs.Item{}
	for _, it := range pending {
		byRel[it.RelPath] = it
	}
	return byRel, nil
}

// copyFiles copies the staged files into the partial directory with the filecopy engine (S7).
func (w *run) copyFiles(ctx context.Context, br BackupResult, staging, partial string, byRel map[string]jobs.Item) error {
	src, err := os.OpenRoot(staging)
	if err != nil {
		return fmt.Errorf("open the staging directory: %w", err)
	}
	defer src.Close()
	total := br.Size()
	var done int64
	for i, f := range br.Files {
		item := byRel[relOf(f.Name)]
		dstRel := partial + "/" + f.Name
		detail := itemDetail{Name: f.Name, Source: f.Source, Destination: dstRel, Size: f.Size, SHA256: f.SHA256, Method: f.Method}
		perm := filecopy.DefaultFilePerm
		if f.Name == PreferencesXML {
			perm = 0o600 // it holds the server's PlexOnlineToken
		}
		progress := jobs.Progress{Phase: "copying", FilesTotal: int64(len(br.Files)), FilesDone: int64(i), BytesTotal: total, BytesDone: done, CurrentFile: f.Name}
		w.rep.Progress(progress)
		t, err := filecopy.WriteTemp(ctx, src, f.Name, w.h.Root, dstRel, filecopy.CopyOptions{
			Hash:     true,
			FilePerm: perm,
			OnTemp: func(tempRel string) error {
				if item.ID == 0 {
					return nil
				}
				detail.Temp = tempRel
				return w.items.SetDetail(ctx, item.ID, detail.json())
			},
			Progress: func(delta int64) {
				done += delta
				progress.BytesDone = done
				w.rep.Progress(progress)
			},
		})
		if err != nil {
			return fmt.Errorf("copy %s to the destination: %w", f.Name, err)
		}
		if t.Hash != filecopy.HashPrefix+f.SHA256 {
			_ = filecopy.CleanupTemp(w.h.Root, t.Rel)
			return fmt.Errorf("copy %s to the destination: %w: sha256 %s, staged %s", f.Name, filecopy.ErrMismatch, t.Hash, f.SHA256)
		}
		if w.h.Settings.Verify.Mode != destinations.VerifyOff {
			if err := filecopy.VerifyTemp(ctx, w.h.Root, t); err != nil {
				_ = filecopy.CleanupTemp(w.h.Root, t.Rel)
				return fmt.Errorf("re-read %s at the destination: %w", f.Name, err)
			}
		}
		if err := filecopy.Commit(w.h.Root, t.Rel, dstRel, true); err != nil {
			_ = filecopy.CleanupTemp(w.h.Root, t.Rel)
			return fmt.Errorf("copy %s to the destination: %w", f.Name, err)
		}
		faultinject.Point(PointAfterCopy)
		if item.ID != 0 {
			if err := w.items.Finish(ctx, item.ID, jobs.ItemDone, t.Size, ""); err != nil {
				return err
			}
		}
	}
	w.rep.Progress(jobs.Progress{Phase: "finishing", FilesTotal: int64(len(br.Files)), FilesDone: int64(len(br.Files)), BytesTotal: total, BytesDone: done})
	return nil
}

// renameVersion renames the partial directory to the version's name ("<timestamp>", or
// "<timestamp>-job<id>" when a version of the same second exists) and returns its path.
func renameVersion(root *os.Root, partial, folder, versionName string, jobID int64) (string, error) {
	rel := folder + "/" + versionName
	err := filecopy.RenameDir(root, partial, rel)
	if errors.Is(err, filecopy.ErrExists) {
		rel = folder + "/" + versionName + "-job" + strconv.FormatInt(jobID, 10)
		err = filecopy.RenameDir(root, partial, rel)
	}
	if err != nil {
		return "", fmt.Errorf("rename the version directory: %w", err)
	}
	return rel, nil
}

// checkSpace fails the job before anything is written when the destination or the staging
// filesystem cannot hold the backup (the databases plus their WAL, an upper bound of a copy).
func (w *run) checkSpace() error {
	files, err := Inspect(w.ps.DataPath)
	if err != nil {
		return err
	}
	var need uint64
	for _, f := range files {
		if f.Present {
			need += uint64(f.Size + f.WALSize)
		}
	}
	if free, _, err := filecopy.FreeSpace(w.h.Root); err == nil && free < need+spaceMargin {
		return fmt.Errorf("not enough free space at destination %q: the Plex backup needs about %s, %s is free",
			w.h.Destination.Name, formatBytes(int64(need)), formatBytes(int64(free)))
	}
	if err := os.MkdirAll(filepath.Join(w.r.configDir, StagingRoot), 0o700); err != nil {
		return fmt.Errorf("create the staging directory: %w", err)
	}
	cfg, err := os.OpenRoot(filepath.Join(w.r.configDir, StagingRoot))
	if err != nil {
		return fmt.Errorf("open the staging directory: %w", err)
	}
	defer cfg.Close()
	if free, _, err := filecopy.FreeSpace(cfg); err == nil && free < need+spaceMargin {
		return fmt.Errorf("not enough free space in the config directory for staging the Plex backup: it needs about %s, %s is free",
			formatBytes(int64(need)), formatBytes(int64(free)))
	}
	return nil
}

// plexInfo asks Plex (when it answers) for its version and its butler window; a backup inside the
// window is a warning. It never fails the job. The token is only sent to the URL it was saved with
// (S8): when the URL changed since the job loaded the integration, Plex is not asked.
func (w *run) plexInfo(ctx context.Context) (version string) {
	token, err := w.r.integrations.TokenFor(ctx, w.it.ID, w.it.URL)
	if errors.Is(err, integrations.ErrURLChanged) {
		w.rep.Log(slog.LevelInfo, "Plex's maintenance window was not checked: the integration's URL changed during the backup")
		return ""
	}
	if err != nil {
		w.rep.Log(slog.LevelInfo, "Plex's maintenance window was not checked: the token could not be read", "error", err.Error())
		return ""
	}
	c, err := plex.New(w.it.URL, token, w.r.plexOpts)
	if err != nil {
		w.rep.Log(slog.LevelInfo, "Plex's maintenance window was not checked", "error", err.Error())
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, plexCheckTimeout)
	defer cancel()
	id, err := c.Identity(cctx)
	if err != nil {
		w.rep.Log(slog.LevelInfo, "Plex did not answer; its maintenance window was not checked", "error", err.Error())
		return ""
	}
	prefs, err := c.Prefs(cctx)
	if err != nil {
		w.rep.Log(slog.LevelInfo, "Plex's maintenance window was not checked", "error", err.Error())
		return id.Version
	}
	if hour := w.r.now().In(w.r.loc).Hour(); prefs.InButlerWindow(hour) {
		msg := fmt.Sprintf("The backup ran inside Plex's maintenance window (%02d:00–%02d:00), when Plex optimizes and backs up its database; schedule it outside the window",
			prefs.ButlerStartHour, prefs.ButlerEndHour)
		w.noted = append(w.noted, msg)
		w.warn(msg, "hour", hour)
	}
	return id.Version
}

// recover cleans up after interrupted jobs of this integration (see Run step 2). It returns this
// job's own version when an earlier attempt had written it completely (ownVersion).
func (w *run) recover(ctx context.Context) (*Snapshot, error) {
	root := w.h.Root
	folders, err := readDirNames(root, PlexRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", PlexRoot, err)
	}
	if err := realDirs(root, PlexRoot); err != nil {
		return nil, fmt.Errorf("check %s: %w", PlexRoot, err)
	}
	for _, folder := range folders {
		if folderIntegrationID(folder) != w.it.ID {
			continue
		}
		fdir := PlexRoot + "/" + folder
		if fi, err := root.Lstat(fdir); err != nil || !fi.IsDir() {
			continue
		}
		names, err := readDirNames(root, fdir)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", fdir, err)
		}
		for _, name := range names {
			rel := fdir + "/" + name
			switch {
			case partialNameRe.MatchString(name):
				// A version an interrupted job of this integration was writing; jobs of one
				// integration never run at the same time, so no job is writing it now.
				if err := removeTree(root, rel); err != nil {
					return nil, err
				}
				w.rep.Log(slog.LevelInfo, "Removed the unfinished version of an interrupted backup", "path", rel)
			case pruneNameRe.MatchString(name):
				// A version a prune was deleting. While its row exists, the next prune deletes it
				// or restores it; without one, the prune had deleted the row and stopped before
				// the files.
				recorded, err := w.r.store.recorded(ctx, w.h.Destination.ID, fdir+"/"+pruneNameRe.FindStringSubmatch(name)[1])
				if err != nil {
					return nil, err
				}
				if recorded {
					continue
				}
				if err := removeTree(root, rel); err != nil {
					w.warn("The rest of a pruned Plex DB version could not be deleted", "path", rel, "error", err.Error())
					continue
				}
				w.rep.Log(slog.LevelInfo, "Removed the rest of a Plex DB version an interrupted prune was deleting", "path", rel)
			case versionNameRe.MatchString(name):
				recorded, err := w.r.store.recorded(ctx, w.h.Destination.ID, rel)
				if err != nil {
					return nil, err
				}
				if recorded {
					continue
				}
				if fi, err := root.Lstat(rel); err != nil || !fi.IsDir() {
					w.warn("An unrecorded entry in the Plex DB versions was left alone", "path", rel, "reason", "not a directory")
					continue
				}
				if _, err := w.adopt(ctx, rel); err != nil {
					if ctx.Err() != nil {
						return nil, ctx.Err()
					}
					w.warn("An unrecorded Plex DB version was left alone", "path", rel, "reason", err.Error())
					continue
				}
			}
		}
	}
	return w.ownVersion(ctx)
}

// ownVersion returns the version an earlier attempt of this job wrote completely and that is
// recorded now (by that attempt, by the adoption above, or by another job's): its manifest names
// this job (ownManifest) and its directory exists. It returns nil for a job that is not resumed,
// when there is none, or when the version's files did not match its manifest (then the backup
// starts over).
func (w *run) ownVersion(ctx context.Context) (*Snapshot, error) {
	if w.job.Trigger != jobs.TriggerResume && w.job.Attempt <= 1 {
		return nil, nil // only a resumed job had an earlier attempt
	}
	snaps, err := w.r.store.ListFor(ctx, w.h.Destination.ID, w.it.ID)
	if err != nil {
		return nil, err
	}
	for _, s := range snaps {
		if !ownManifest(s, w.job) {
			continue
		}
		if fi, err := w.h.Root.Lstat(s.Path); err != nil || !fi.IsDir() {
			continue
		}
		var m Manifest
		if s.Integrity != IntegrityOK && (json.Unmarshal(s.Manifest, &m) != nil || m.Result == IntegrityOK) {
			continue
		}
		return &s, nil
	}
	return nil, nil
}

// ownManifest reports whether a recorded version's manifest names job as its writer: its id and
// its queue time (a version made under an earlier Bunkarr database can name the same id).
func ownManifest(s Snapshot, job jobs.Job) bool {
	var m struct {
		JobID       int64     `json:"jobId"`
		JobQueuedAt time.Time `json:"jobQueuedAt"`
	}
	return json.Unmarshal(s.Manifest, &m) == nil && m.JobID == job.ID && m.JobQueuedAt.Equal(job.QueuedAt)
}

// validFileName reports whether a manifest names one of the files a version can hold.
func validFileName(name string) bool {
	return name == LibraryDB || name == BlobsDB || name == PreferencesXML
}

// adopt verifies an unrecorded, complete version directory against its manifest and records it:
// integrity ok only when the manifest says ok and every file has its recorded size and sha256.
// The row names this job (the manifest keeps the job that wrote the version).
func (w *run) adopt(ctx context.Context, rel string) (Snapshot, error) {
	raw, err := readSmallFile(w.h.Root, rel+"/"+ManifestName, maxManifestBytes)
	if err != nil {
		return Snapshot{}, fmt.Errorf("no readable manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return Snapshot{}, fmt.Errorf("the manifest is not valid: %w", err)
	}
	if m.IntegrationID != w.it.ID {
		return Snapshot{}, fmt.Errorf("the manifest names integration %d", m.IntegrationID)
	}
	integrity := m.Result
	if integrity != IntegrityOK {
		integrity = IntegrityFailed
	}
	var size int64
	hasLibrary := false
	for _, f := range m.Files {
		if !validFileName(f.Name) {
			return Snapshot{}, fmt.Errorf("the manifest names an unexpected file %q", f.Name)
		}
		hasLibrary = hasLibrary || f.Name == LibraryDB
		size += f.Size
		if _, err := filecopy.VerifyFile(ctx, w.h.Root, rel+"/"+f.Name, f.Size, filecopy.HashPrefix+f.SHA256, nil); err != nil {
			if ctx.Err() != nil {
				return Snapshot{}, ctx.Err()
			}
			integrity = IntegrityFailed
			w.rep.Log(slog.LevelWarn, "A file of an unrecorded Plex DB version does not match its manifest", "path", rel, "file", f.Name, "error", err.Error())
		}
	}
	if !hasLibrary {
		integrity = IntegrityFailed
	}
	createdAt := m.CreatedAt
	if createdAt.IsZero() {
		createdAt = w.r.now()
	}
	snap, err := w.r.store.insert(context.WithoutCancel(ctx), Snapshot{DestinationID: w.h.Destination.ID,
		IntegrationID: w.it.ID, JobID: w.job.ID, Path: rel, CreatedAt: createdAt, Size: size, Method: m.Method,
		Integrity: integrity, Manifest: raw})
	if err != nil {
		return Snapshot{}, err
	}
	w.stats.Recovered++
	w.rep.Log(slog.LevelInfo, "Recorded a Plex DB version an interrupted backup had written", "path", rel,
		"integrity", integrity, "writtenByJob", m.JobID)
	return snap, nil
}

// finishRecovered completes a job whose version an earlier attempt had written completely, as
// that attempt would have: with its warnings; a version that failed verification fails the job
// with ErrIntegrity, an ok one is followed by pruning.
func (w *run) finishRecovered(ctx context.Context, snap Snapshot) (jobs.Result, error) {
	w.rep.Log(slog.LevelInfo, "An earlier attempt of this job wrote its Plex DB version; finishing with it", "path", snap.Path,
		"integrity", snap.Integrity)
	var m Manifest
	_ = json.Unmarshal(snap.Manifest, &m)
	for _, msg := range m.Warnings {
		w.warn(msg, "path", snap.Path)
	}
	w.stats.Files, w.stats.Bytes = int64(len(m.Files)), snap.Size
	w.stats.Method, w.stats.Integrity = snap.Method, snap.Integrity
	w.stats.SnapshotID, w.stats.Path = snap.ID, snap.Path
	w.stats.MetadataItems, w.stats.MediaParts = m.Integrity.MetadataItems, m.Integrity.MediaParts
	if snap.Integrity != IntegrityOK {
		reports := map[string]IntegrityReport{}
		for _, f := range m.Files {
			if f.Integrity != nil {
				reports[f.Name] = *f.Integrity
			}
		}
		return w.failedVerification(reports)
	}
	w.prune(ctx)
	return w.result(w.summary()), nil
}

// prune applies the version retention to this integration's versions at the destination after a
// successful backup; rows of versions gone from the destination are removed first (dropLost).
// Problems are warnings: the backup itself succeeded.
func (w *run) prune(ctx context.Context) {
	if err := w.h.Recheck(); err != nil {
		w.warn("Old Plex DB versions were not pruned", "error", err.Error())
		return
	}
	snaps, err := w.r.store.ListFor(ctx, w.h.Destination.ID, w.it.ID)
	if err != nil {
		w.warn("Old Plex DB versions were not pruned", "error", err.Error())
		return
	}
	cands := make([]pruneCandidate, 0, len(snaps))
	byID := map[int64]Snapshot{}
	present := make([]Snapshot, 0, len(snaps))
	for _, s := range snaps {
		if w.dropLost(ctx, s) {
			continue
		}
		cands = append(cands, pruneCandidate{ID: s.ID, CreatedAt: s.CreatedAt, OK: s.Integrity == IntegrityOK})
		byID[s.ID] = s
		present = append(present, s)
	}
	remove := selectPrune(cands, w.h.Retention.PlexDBDaily, w.h.Retention.PlexDBWeekly, w.r.now(), w.r.loc)
	removeSet := map[int64]bool{}
	for _, id := range remove {
		removeSet[id] = true
	}
	for _, s := range present {
		if removeSet[s.ID] {
			continue
		}
		if restored, err := restoreVersion(w.h.Root, s.Path, s.Integrity, s.Manifest); err != nil {
			w.warn("A kept Plex DB version could not be restored from an interrupted prune", "path", s.Path, "error", err.Error())
		} else if restored {
			w.rep.Log(slog.LevelInfo, "Restored a kept Plex DB version from an interrupted prune", "path", s.Path)
		}
	}
	for _, id := range remove {
		if ctx.Err() != nil {
			return
		}
		s := byID[id]
		// The version leaves its name, then its row, then its files (see trashVersion).
		trash, err := trashVersion(w.h.Root, s.Path)
		if err != nil {
			w.warn("An old Plex DB version could not be deleted", "path", s.Path, "error", err.Error())
			continue
		}
		faultinject.Point(PointPruneAfterTrash)
		// The version has left its name: drop its row even if the job is being cancelled.
		if err := w.r.store.remove(context.WithoutCancel(ctx), id); err != nil {
			// The next prune deletes it, or restores it when it is kept after all.
			w.warn("An old Plex DB version was not deleted: its record could not be removed", "path", s.Path, "error", err.Error())
			continue
		}
		faultinject.Point(PointPruneAfterUnrecord)
		w.stats.Pruned++
		w.rep.Log(slog.LevelInfo, "Pruned an old Plex DB version", "path", s.Path, "createdAt", s.CreatedAt, "integrity", s.Integrity)
		if err := removeTrash(w.h.Root, trash); err != nil {
			w.warn("The files of a pruned Plex DB version could not be deleted; the next backup retries", "path", trash, "error", err.Error())
			continue
		}
		faultinject.Point(PointPruneAfterRemove)
	}
}

// dropLost removes the row of a version that is gone from the destination (versionLost), so it
// takes no retention slot, then what is left of its ".prune-*" directory, as a prune does. It
// reports whether it removed the row.
func (w *run) dropLost(ctx context.Context, s Snapshot) bool {
	if lost, err := versionLost(w.h.Root, s.Path, s.Integrity, s.Manifest); err != nil || !lost {
		return false // when it cannot tell, the restore or the deletion below reports the problem
	}
	if err := w.r.store.remove(ctx, s.ID); err != nil {
		w.warn("A Plex DB version missing at the destination is still recorded: its record could not be removed", "path", s.Path, "error", err.Error())
		return false
	}
	w.warn("A recorded Plex DB version was missing or incomplete at the destination; its record was removed", "path", s.Path,
		"createdAt", s.CreatedAt)
	if err := removeTrash(w.h.Root, trashPath(s.Path)); err != nil {
		w.warn("The rest of a Plex DB version could not be deleted; the next backup retries", "path", trashPath(s.Path), "error", err.Error())
	}
	return true
}

// dryRun reports what a backup would copy (see Run).
func (w *run) dryRun(ctx context.Context) (jobs.Result, error) {
	files, err := Inspect(w.ps.DataPath)
	if err != nil {
		return jobs.Result{}, err
	}
	lib := files[0]
	switch {
	case !lib.Present:
		return jobs.Result{}, fmt.Errorf("%w: %s", ErrNoDatabase, lib.Path)
	case !lib.Readable:
		return jobs.Result{}, fmt.Errorf("the Plex library database %s cannot be read: %s", lib.Path, lib.Problem)
	}
	w.plexInfo(ctx)
	if err := w.items.DeleteItems(ctx, w.job.ID); err != nil {
		return jobs.Result{}, err
	}
	w.stats.DryRun = true
	w.stats.Method = lib.Method
	versionRel := w.folder + "/<" + VersionLayout + ">"
	var list []jobs.Item
	var need int64
	for _, f := range files {
		if !f.Present && f.Name == BlobsDB {
			continue
		}
		d := itemDetail{Name: f.Name, Source: f.Path, Destination: versionRel + "/" + f.Name, Size: f.Size,
			Method: f.Method, WALPresent: f.WALPresent, WALSize: f.WALSize, Problem: f.Problem}
		item := jobs.Item{RelPath: f.Rel, Action: jobs.ActionBackup, Status: jobs.ItemSkipped, Bytes: f.Size, Detail: d.json()}
		if f.Present && f.Readable {
			w.stats.Files++
			w.stats.Bytes += f.Size
			need += f.Size + f.WALSize
			w.rep.Log(slog.LevelInfo, "Would back up", "file", f.Rel, "size", f.Size, "walPresent", f.WALPresent,
				"walSize", f.WALSize, "method", f.Method)
		} else {
			item.Status, item.Error = jobs.ItemFailed, f.Problem
			w.warn("Would not back up "+f.Name, "problem", f.Problem)
		}
		list = append(list, item)
	}
	if err := w.items.AddItems(ctx, w.job.ID, list, true); err != nil {
		return jobs.Result{}, err
	}
	if free, _, err := filecopy.FreeSpace(w.h.Root); err == nil && uint64(need)+spaceMargin > free {
		w.warn("The destination does not have enough free space for this backup", "need", need, "free", free)
	}
	return w.result(fmt.Sprintf("Dry run: would back up %d files (%s) of Plex %q to %q (%s)", w.stats.Files,
		formatBytes(w.stats.Bytes), w.it.Name, w.h.Destination.Name, lib.Method)), nil
}

// cleanStaleStaging removes staging directories of other Plex DB jobs that nothing has touched for
// a day (jobs that failed for good after crashes). Best effort.
func (r *Runner) cleanStaleStaging(jobID int64) {
	dir := filepath.Join(r.configDir, StagingRoot)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	own := "plexdb-job" + strconv.FormatInt(jobID, 10)
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "plexdb-job") || e.Name() == own {
			continue
		}
		fi, err := e.Info()
		if err != nil || r.now().Sub(fi.ModTime()) < staleStaging {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if err := os.RemoveAll(p); err != nil {
			r.log.Warn("Could not remove a stale Plex DB staging directory", "path", p, "error", err)
			continue
		}
		r.log.Info("Removed a stale Plex DB staging directory", "path", p)
	}
}

// removeTree removes the directory rel inside the Plex tree (every directory on the way must be a
// real directory); a missing rel is not an error.
func removeTree(root *os.Root, rel string) error {
	if !strings.HasPrefix(rel, PlexRoot+"/") {
		return fmt.Errorf("refusing to remove %q: outside %s", rel, PlexRoot)
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

// readDirNames lists a directory inside root.
func readDirNames(root *os.Root, rel string) ([]string, error) {
	f, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Readdirnames(-1)
}

// readSmallFile reads the regular file rel (never through a final symlink), at most limit bytes.
func readSmallFile(root *os.Root, rel string, limit int64) ([]byte, error) {
	fi, err := root.Lstat(rel)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, filecopy.ErrNotRegular
	}
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("larger than %d bytes", limit)
	}
	return data, nil
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

// nopItems is an ItemStore that stores nothing (a runner driven without a job manager).
type nopItems struct{}

// Planned implements jobs.ItemStore.
func (nopItems) Planned(context.Context, int64) (bool, error) { return false, nil }

// AddItems implements jobs.ItemStore.
func (nopItems) AddItems(context.Context, int64, []jobs.Item, bool) error { return nil }

// DeleteItems implements jobs.ItemStore.
func (nopItems) DeleteItems(context.Context, int64) error { return nil }

// Pending implements jobs.ItemStore.
func (nopItems) Pending(context.Context, int64, int64, int) ([]jobs.Item, error) { return nil, nil }

// SetDetail implements jobs.ItemStore.
func (nopItems) SetDetail(context.Context, int64, json.RawMessage) error { return nil }

// Finish implements jobs.ItemStore.
func (nopItems) Finish(context.Context, int64, jobs.ItemStatus, int64, string) error { return nil }

// Counts implements jobs.ItemStore.
func (nopItems) Counts(context.Context, int64) ([]jobs.ItemCount, error) { return nil, nil }
