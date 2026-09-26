package arrbackup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/snapshots"
)

const (
	// spaceMargin is kept free at the destination and in the staging filesystem.
	spaceMargin = 16 << 20
	// staleStaging is the age after which another job's staging directory is removed.
	staleStaging = 24 * time.Hour
	// maxManifestBytes bounds how much of a manifest is read.
	maxManifestBytes = 1 << 20
	// stagingPrefix names a job's staging directory: <config>/staging/arrbackup-job<id>.
	stagingPrefix = "arrbackup-job"
	// maxPollErrors is how many GET command/{id} failures in a row end the wait.
	maxPollErrors = 3
	// dirPerm and filePerm are the modes of everything written for a version (S17).
	dirPerm  = 0o700
	filePerm = 0o600
)

// Options configures a Runner.
type Options struct {
	DB           *db.DB
	Integrations *integrations.Store
	Destinations *destinations.Store
	// ConfigDir is Bunkarr's config directory; staging directories are
	// <ConfigDir>/staging/arrbackup-job<id>.
	ConfigDir string
	// Log receives events outside a job's own log (cleanup problems); nil discards them.
	Log *slog.Logger
	// Now is the clock of version names, backup ages and pruning; nil means time.Now.
	Now func() time.Time
	// Location is the time zone of ISO weeks (pruning); nil means time.Local.
	Location *time.Location
	// Arr configures the *arr client (tests shorten timeouts); its HTTPClient must dial through
	// internal/netguard (the default does).
	Arr arr.Options
	// PollInterval, CommandTimeout, FolderRetries and FolderRetryDelay override the defaults of
	// the Backup command wait and of the Backups folder retries (tests shorten them).
	PollInterval     time.Duration
	CommandTimeout   time.Duration
	FolderRetries    int
	FolderRetryDelay time.Duration
}

// Runner runs jobs.TypeArrBackup jobs. It is safe for concurrent use (the job manager runs at most
// one job per integration: lock key "arrbackup:<integrationId>").
type Runner struct {
	database     *db.DB
	store        *snapshots.Store
	integrations *integrations.Store
	destinations *destinations.Store
	configDir    string
	log          *slog.Logger
	now          func() time.Time
	loc          *time.Location
	arrOpts      arr.Options
	poll         time.Duration
	cmdTimeout   time.Duration
	retries      int
	retryDelay   time.Duration
}

var _ jobs.Runner = (*Runner)(nil)

// NewRunner returns the arr_backup runner. DB, Integrations, Destinations and an absolute
// ConfigDir are required.
func NewRunner(o Options) (*Runner, error) {
	switch {
	case o.DB == nil:
		return nil, errors.New("arrbackup: no database")
	case o.Integrations == nil:
		return nil, errors.New("arrbackup: no integrations store")
	case o.Destinations == nil:
		return nil, errors.New("arrbackup: no destinations store")
	case !filepath.IsAbs(o.ConfigDir):
		return nil, fmt.Errorf("arrbackup: the config directory %q is not an absolute path", o.ConfigDir)
	}
	r := &Runner{
		database: o.DB, store: snapshots.NewStore(o.DB), integrations: o.Integrations, destinations: o.Destinations,
		configDir: o.ConfigDir, log: o.Log, now: o.Now, loc: o.Location, arrOpts: o.Arr,
		poll: o.PollInterval, cmdTimeout: o.CommandTimeout, retries: o.FolderRetries, retryDelay: o.FolderRetryDelay,
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
	if r.poll <= 0 {
		r.poll = DefaultPollInterval
	}
	if r.cmdTimeout <= 0 {
		r.cmdTimeout = DefaultCommandTimeout
	}
	if r.retries <= 0 {
		r.retries = DefaultFolderRetries
	}
	if r.retryDelay <= 0 {
		r.retryDelay = DefaultFolderRetryDelay
	}
	return r, nil
}

// Store returns the runner's snapshots store (every kind; the runner itself only reads and writes
// kind arr).
func (r *Runner) Store() *snapshots.Store { return r.store }

// StagingDir is the staging directory of job jobID.
func (r *Runner) StagingDir(jobID int64) string {
	return filepath.Join(r.configDir, StagingRoot, stagingPrefix+strconv.FormatInt(jobID, 10))
}

// run is one job's state.
type run struct {
	r       *Runner
	job     jobs.Job
	rep     jobs.Reporter
	items   jobs.ItemStore
	it      integrations.Integration
	as      integrations.ArrSettings
	app     string // "Radarr"
	kind    arr.Kind
	client  *arr.Client
	h       *destinations.Handle
	folder  string
	fetch   string // FetchFolder or FetchHTTP
	started time.Time
	stats   Stats
	warns   int
	// noted are the warnings about the version being written (its manifest keeps them).
	noted []string
	// invalid are the ids of listed backups already reported as invalid.
	invalid map[int64]bool
	// resumeCmd is the Backup command an interrupted attempt sent and was waiting for (0: none).
	resumeCmd int64
	// cmdItem is the pending item that records the Backup command being waited for.
	cmdItem jobs.Item
}

// newBackupRel is the item path of a backup the *arr is asked to make (a dry run's "create" item,
// and the item that records the Backup command while the *arr makes it).
const newBackupRel = arr.BackupManual + "/<new backup>"

// resumed reports whether an earlier attempt of the job may have run.
func (w *run) resumed() bool {
	return w.job.Trigger == jobs.TriggerResume || w.job.Attempt > 1
}

// Run backs up the *arr of job.Params.IntegrationID to destination job.Params.DestinationID (0:
// the integration's backup destination), design §10:
//
//  1. Preflight: an enabled Sonarr, Radarr or Lidarr; an enabled destination opened with its S3
//     checks; a destination that enforces file modes unless the integration accepts insecure
//     modes (S17); the *arr answers system/status as its application.
//  2. What an earlier attempt left is cleaned up as for Plex DB versions (phase1.md §5):
//     staging, ".partial-job*" directories, interrupted prunes; a complete, unrecorded version is
//     verified against its manifest and recorded; a resumed job whose own version is complete
//     finishes with it.
//  3. The backup: the newest scheduled one when it is younger than backup.maxScheduledAgeDays
//     (no command is sent; when this destination already holds an intact copy of it, the job
//     completes with unchanged and copies nothing; a copy that does not read back as recorded is
//     marked failed and the backup is copied again); otherwise POST command {"name":"Backup"},
//     whose id is recorded as a pending job item, polled until it completes, and the newest
//     manual backup made since. A resumed job follows the command an interrupted attempt recorded
//     (unless the *arr no longer knows it), else looks for a manual backup made since it was
//     queued, so a crash or a shutdown does not make the *arr create another one.
//  4. The zip is fetched into <config>/staging/arrbackup-job<id>/ (0700; the file 0600) from the
//     Backups folder or over HTTP, hashed and verified (VerifyZip).
//  5. It is copied with the filecopy engine into .bunkarr/arr/<folder>/.partial-job<id>/,
//     manifest.json is written, the directory is renamed to the version's timestamp, the
//     snapshots row (kind arr) is inserted and staging is removed.
//  6. A version that failed verification is kept (7 days) and the job fails with ErrIntegrity;
//     otherwise old versions are pruned (destination retention arrDaily and arrWeekly).
//
// A dry run runs the preflight, lists the *arr's backups and records one skip item for the
// backup it would copy or create, with the method; it sends no command and writes nothing (S9).
//
// Staging (S17: the zip holds the *arr's API key and passwords). Every run first sweeps the stale
// staging of other jobs (cleanStaleStaging), and the job's own staging directory is removed
// whenever Run returns, also when an attempt fails before the backup starts (the integration was
// disabled or deleted, the destination is not mounted) after an earlier attempt had staged the
// zip. A crash leaves it to the resumed attempt, or to OnJobFinish when crash recovery fails the
// job for good.
func (r *Runner) Run(ctx context.Context, job jobs.Job, env jobs.Env) (jobs.Result, error) {
	r.cleanStaleStaging(job.ID)
	res, err := r.runJob(ctx, job, env)
	if !job.DryRun {
		r.removeStaging(job.ID)
	}
	return res, err
}

// OnJobFinish is the job manager's OnFinish hook: it removes the staging directory of an
// arr_backup job that ended. A run removes its own when it returns; this covers a job that crash
// recovery failed for good without running it again, whose staged zip would otherwise stay (S17).
func (r *Runner) OnJobFinish(job jobs.Job) {
	if job.Type != jobs.TypeArrBackup || job.DryRun || !job.Status.Final() {
		return
	}
	r.removeStaging(job.ID)
}

// SweepStaging removes the staging directories of *arr backup jobs that nothing has touched for a
// day (left by a lost database or a failed removal). Call it at start-up, before jobs run.
func (r *Runner) SweepStaging() { r.cleanStaleStaging(0) }

// removeStaging removes job jobID's staging directory (best effort, logged).
func (r *Runner) removeStaging(jobID int64) {
	p := r.StagingDir(jobID)
	if err := os.RemoveAll(p); err != nil {
		r.log.Error("Could not remove an *arr backup staging directory", "path", p, "error", err)
	}
}

// runJob is Run without the staging cleanup.
func (r *Runner) runJob(ctx context.Context, job jobs.Job, env jobs.Env) (jobs.Result, error) {
	w := &run{r: r, job: job, rep: env.Reporter, items: env.Items, started: r.now(), invalid: map[int64]bool{}}
	if w.rep == nil {
		w.rep = nopReporter{}
	}
	if w.items == nil {
		w.items = nopItems{}
	}
	if job.Params.IntegrationID <= 0 {
		return jobs.Result{}, errors.New("an *arr backup needs an integrationId")
	}
	if err := w.load(ctx); err != nil {
		return jobs.Result{}, err
	}
	destID := job.Params.DestinationID
	if destID == 0 {
		destID = w.as.Backup.DestinationID
	}
	if destID == 0 {
		return jobs.Result{}, fmt.Errorf("%s %q has no backup destination: choose one in its backup settings", w.app, w.it.Name)
	}
	var err error
	if w.h, err = r.destinations.Open(ctx, destID); err != nil {
		return jobs.Result{}, err
	}
	defer w.h.Close()
	if !w.h.Destination.Enabled {
		return jobs.Result{}, fmt.Errorf("destination %q is disabled", w.h.Destination.Name)
	}
	if err := CheckModes(w.h.Capabilities, w.as.Backup.AcceptInsecureModes, w.h.Destination.Name, w.app); err != nil {
		return jobs.Result{}, err
	}
	w.folder = FolderName(w.it.Name, w.it.ID)
	w.fetch = FetchHTTP
	if w.as.BackupFolder != "" {
		w.fetch = FetchFolder
	}
	w.stats.Method = w.fetch
	if job.DryRun {
		return w.dryRun(ctx)
	}
	return w.backup(ctx)
}

// CheckModes is the S17 rule for a backup's destination: its capabilities must say it enforces
// file modes, or the integration must accept insecure modes. A destination probed before
// Bunkarr checked this counts as not enforcing them until it is tested again. It returns an error
// wrapping ErrInsecureModes that names the setting (backup.acceptInsecureModes); the API uses it
// when settings are saved.
func CheckModes(caps destinations.Capabilities, acceptInsecure bool, destination, app string) error {
	if caps.EnforcesModes || acceptInsecure {
		return nil
	}
	return fmt.Errorf("%w: destination %q does not keep files private (an SMB share without POSIX extensions), or was checked before "+
		"Bunkarr tested this (test the destination again). The %s backup holds its API key and passwords: set backup.acceptInsecureModes "+
		"(\"Accept insecure file modes\") to write it there anyway", ErrInsecureModes, destination, app)
}

// load reads the integration (an enabled *arr) and builds its client. The key is only sent to the
// URL it was saved with (S8).
func (w *run) load(ctx context.Context) error {
	id := w.job.Params.IntegrationID
	it, err := w.r.integrations.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("integration %d: %w", id, err)
	}
	if !it.Type.IsArr() {
		return fmt.Errorf("integration %q is a %s integration, not Sonarr, Radarr or Lidarr", it.Name, it.Type.AppName())
	}
	as, err := it.ArrSettings()
	if err != nil {
		return err
	}
	w.it, w.as, w.app, w.kind = it, as, it.Type.AppName(), arr.Kind(it.Type)
	if !it.Enabled {
		return fmt.Errorf("%s %q is disabled", w.app, it.Name)
	}
	key, err := w.r.integrations.TokenFor(ctx, it.ID, it.URL)
	if errors.Is(err, integrations.ErrURLChanged) {
		return fmt.Errorf("the URL of %s %q changed while the backup started; run it again", w.app, it.Name)
	}
	if err != nil {
		return err
	}
	if w.client, err = arr.New(w.kind, it.URL, key, w.r.arrOpts); err != nil {
		return fmt.Errorf("%s %q: %w", w.app, it.Name, err)
	}
	return nil
}

// warn records a job warning and logs it.
func (w *run) warn(msg string, args ...any) {
	w.warns++
	w.rep.Log(slog.LevelWarn, msg, args...)
}

// note records a warning that belongs to the version being written.
func (w *run) note(msg string, args ...any) {
	w.noted = append(w.noted, msg)
	w.warn(msg, args...)
}

// result builds the job result.
func (w *run) result(summary string) jobs.Result {
	w.stats.DurationMs = w.r.now().Sub(w.started).Milliseconds()
	return jobs.Result{Stats: w.stats, Warnings: w.warns, Summary: summary}
}

// choice is the backup a job copies.
type choice struct {
	b arr.Backup
	// reused is true for a scheduled backup the *arr made itself.
	reused bool
	// unchanged is set when that scheduled backup is already at the destination.
	unchanged *snapshots.Snapshot
}

// newestValid returns the newest entry of type typ in list whose type and name are valid and
// whose time is not before since (zero: any), and whether there is one. Invalid entries are
// reported once as a warning.
func (w *run) newestValid(list []arr.Backup, typ string, since time.Time) (arr.Backup, bool) {
	var best arr.Backup
	found := false
	for _, b := range list {
		if b.Type != typ {
			continue
		}
		if err := arr.ValidateBackup(b); err != nil {
			if !w.invalid[b.ID] {
				w.invalid[b.ID] = true
				w.warn("A backup the *arr listed was ignored: its type or name is not valid", "id", b.ID, "error", err.Error())
			}
			continue
		}
		if !since.IsZero() && b.Time.Before(since) {
			continue
		}
		if !found || b.Time.After(best.Time) || (b.Time.Equal(best.Time) && b.ID > best.ID) {
			best, found = b, true
		}
	}
	return best, found
}

// newestAny returns the newest valid backup of any type.
func (w *run) newestAny(list []arr.Backup) (arr.Backup, bool) {
	var newest arr.Backup
	found := false
	for _, typ := range []string{arr.BackupManual, arr.BackupScheduled, arr.BackupUpdate} {
		if b, ok := w.newestValid(list, typ, time.Time{}); ok && (!found || b.Time.After(newest.Time)) {
			newest, found = b, true
		}
	}
	return newest, found
}

// freshScheduled returns the newest scheduled backup when it is younger than
// backup.maxScheduledAgeDays.
func (w *run) freshScheduled(list []arr.Backup) (arr.Backup, bool) {
	b, ok := w.newestValid(list, arr.BackupScheduled, time.Time{})
	if !ok {
		return arr.Backup{}, false
	}
	maxAge := time.Duration(w.as.Backup.MaxScheduledAgeDays) * 24 * time.Hour
	if w.r.now().Sub(b.Time) >= maxAge {
		return arr.Backup{}, false
	}
	return b, true
}

// alreadyCopied returns this destination's ok version of this integration made from backup b
// (by name and type), when it is still there and reads back as recorded (intactCopy). bad reports
// that b itself failed verification when it was copied before: the job then makes a new backup
// instead of copying the known-bad one again (or reporting it as current). A copy that was
// damaged at the destination (the backup had passed verification) does not make b bad: it is
// marked failed (damagedCopy; not by a dry run) and b is copied again.
func (w *run) alreadyCopied(ctx context.Context, b arr.Backup) (s *snapshots.Snapshot, bad bool, err error) {
	snaps, err := w.r.store.ListFor(ctx, snapshots.KindArr, w.h.Destination.ID, w.it.ID)
	if err != nil {
		return nil, false, err
	}
	for _, sn := range snaps {
		var m Manifest
		if json.Unmarshal(sn.Manifest, &m) != nil || m.Backup.Name != b.Name || m.Backup.Type != b.Type {
			continue
		}
		if sn.Integrity != IntegrityOK {
			if m.Integrity.Result() == IntegrityOK {
				continue // the backup passed verification; its copy was found damaged later
			}
			w.warn("This scheduled backup failed verification when it was copied before; asking for a new backup", "backup", b.Name,
				"path", sn.Path)
			return nil, true, nil
		}
		if lost, err := versionLost(w.h.Root, sn.Path, sn.Integrity, sn.Manifest); err == nil && lost {
			continue // its record is dropped by the prune after this backup
		}
		if err := w.intactCopy(ctx, sn, m); err != nil {
			if ctx.Err() != nil {
				return nil, false, ctx.Err()
			}
			w.damagedCopy(ctx, sn, err)
			continue
		}
		return &sn, false, nil
	}
	return nil, false, nil
}

// errNotThere is an intactCopy failure that says nothing about the version's files: its directory
// is not at its path (an interrupted prune renamed it) or not a real directory.
var errNotThere = errors.New("the version directory is not at its path")

// intactCopy checks a recorded ok version before the job reports its backup as already at the
// destination, as a manifest version is read back before "unchanged": the version directory holds
// manifest.json and the zip, and the zip has the size and sha256 its row records.
func (w *run) intactCopy(ctx context.Context, s snapshots.Snapshot, m Manifest) error {
	if fi, err := w.h.Root.Lstat(s.Path); err != nil || !fi.IsDir() {
		return errNotThere
	}
	if err := snapshots.RealDirs(w.h.Root, s.Path); err != nil {
		return fmt.Errorf("%w: %v", errNotThere, err)
	}
	if err := checkComplete(w.h.Root, s.Path, s.Manifest, true); err != nil {
		return err
	}
	name, err := manifestZipName(m)
	if err != nil {
		return err
	}
	w.rep.Progress(jobs.Progress{Phase: "verifying", FilesTotal: 1, BytesTotal: m.Zip.Size, CurrentFile: name})
	_, err = filecopy.VerifyFile(ctx, w.h.Root, s.Path+"/"+name, m.Zip.Size, filecopy.HashPrefix+m.Zip.SHA256, nil)
	return err
}

// isDamage reports whether an intactCopy failure shows that a present version's files are not the
// recorded ones (rather than that they could not be read).
func isDamage(err error) bool {
	return errors.Is(err, snapshots.ErrIncomplete) || errors.Is(err, filecopy.ErrMismatch) ||
		errors.Is(err, filecopy.ErrNotRegular) || errors.Is(err, fs.ErrNotExist)
}

// damagedCopy handles a recorded ok copy of the chosen scheduled backup that did not read back
// (intactCopy failed with cause): it is not reported as current. A damaged one is marked failed,
// so it no longer counts as an ok version (it is kept FailedKeep for diagnosis, then pruned); a
// dry run only reports it.
func (w *run) damagedCopy(ctx context.Context, s snapshots.Snapshot, cause error) {
	switch {
	case !isDamage(cause):
		w.warn("A copy of this scheduled backup at the destination could not be read back; it is copied again", "path", s.Path,
			"reason", cause.Error())
		return
	case w.job.DryRun:
		w.warn("A copy of this scheduled backup at the destination is damaged; a real run marks it failed and copies the backup again",
			"path", s.Path, "reason", cause.Error())
		return
	}
	if err := w.h.Recheck(); err != nil {
		w.warn("A copy of this scheduled backup at the destination could not be read back; it is copied again", "path", s.Path,
			"reason", err.Error())
		return
	}
	if err := w.markFailed(ctx, s); err != nil {
		w.warn("A damaged copy of this scheduled backup could not be marked failed; the backup is copied again", "path", s.Path,
			"reason", cause.Error(), "error", err.Error())
		return
	}
	w.warn("A copy of this scheduled backup at the destination does not match its record; it is marked failed and the backup "+
		"is copied again", "path", s.Path, "reason", cause.Error())
}

// markFailed records version s as failed. The store changes rows only by insert and delete, so
// the row is replaced; if Bunkarr stops in between, the version is unrecorded and the next job's
// recovery records it again after comparing its zip with its manifest (failed, as it is damaged).
func (w *run) markFailed(ctx context.Context, s snapshots.Snapshot) error {
	bg := context.WithoutCancel(ctx)
	if err := w.r.store.Remove(bg, s.ID); err != nil {
		return err
	}
	s.Integrity = IntegrityFailed
	_, err := w.r.store.Insert(bg, s)
	return err
}

// choose picks the backup to copy (Run step 3).
func (w *run) choose(ctx context.Context) (choice, error) {
	list, err := w.client.Backups(ctx)
	if err != nil {
		return choice{}, fmt.Errorf("list %s's backups: %w", w.app, err)
	}
	if b, ok := w.freshScheduled(list); ok {
		done, bad, err := w.alreadyCopied(ctx, b)
		if err != nil {
			return choice{}, err
		}
		if !bad {
			w.rep.Log(slog.LevelInfo, "Using "+w.app+"'s own scheduled backup; no backup command is sent", "backup", b.Name,
				"time", b.Time)
			return choice{b: b, reused: true, unchanged: done}, nil
		}
	}
	if w.resumed() {
		// An earlier attempt may have asked the *arr for a backup already: use it rather than
		// make the *arr create another manual backup (it never prunes those). When that attempt
		// was stopped while the *arr was still making it, its command is followed. When the *arr
		// stopped that command itself (stopped), a new backup is asked for: a manual backup
		// listed since the job was queued may be the partial zip it was writing then.
		stopped := false
		if w.resumeCmd != 0 {
			b, ok, cmdStopped, err := w.resumeCommand(ctx, w.resumeCmd)
			if err != nil {
				return choice{}, err
			}
			if ok {
				return choice{b: b}, nil
			}
			stopped = cmdStopped
		}
		if !stopped {
			if b, ok := w.newestValid(list, arr.BackupManual, w.job.QueuedAt.Add(-commandQueuedSlack)); ok {
				w.rep.Log(slog.LevelInfo, "Using the manual backup made since this job was queued", "backup", b.Name, "time", b.Time)
				return choice{b: b}, nil
			}
		}
	}
	// A backup the job could not fetch would only add a manual backup the *arr keeps for good:
	// the method is checked first, on the newest backup the *arr lists (when there is one).
	if err := w.checkAccess(ctx, list); err != nil {
		return choice{}, err
	}
	b, err := w.createBackup(ctx)
	if err != nil {
		return choice{}, err
	}
	return choice{b: b}, nil
}

// checkAccess fails before the Backup command is sent when the job's method cannot fetch a
// backup: the Backups folder cannot be read, or the *arr answers a download of its newest backup
// with a login page (LoginRequiredError). With no backup listed yet, only the folder is checked.
func (w *run) checkAccess(ctx context.Context, list []arr.Backup) error {
	if w.fetch == FetchFolder {
		if p := w.folderProblem(); p != "" {
			return fmt.Errorf("%w: %s", ErrFolder, p)
		}
		return nil
	}
	newest, found := w.newestAny(list)
	if !found {
		return nil
	}
	access, err := w.client.ProbeBackup(ctx, newest)
	if err != nil {
		return fmt.Errorf("check the download of %s's backups: %w", w.app, err)
	}
	if access == arr.BackupAccessLoginRequired {
		return &LoginRequiredError{App: w.app}
	}
	return nil
}

// createBackup sends the Backup command, records its id (saveCommand), waits for it and returns
// the manual backup it made.
func (w *run) createBackup(ctx context.Context) (arr.Backup, error) {
	w.rep.Progress(jobs.Progress{Phase: "backing-up", CurrentFile: w.app})
	cmd, err := w.client.StartBackup(ctx)
	if err != nil {
		return arr.Backup{}, fmt.Errorf("ask %s to make a backup: %w", w.app, err)
	}
	w.rep.Log(slog.LevelInfo, "Asked "+w.app+" to make a backup", "commandId", cmd.ID)
	w.saveCommand(ctx, cmd.ID)
	faultinject.Point(PointAfterCommand)
	return w.awaitBackup(ctx, cmd)
}

// resumeCommand follows the Backup command id that an interrupted attempt sent, instead of sending
// another. ok is false when the command cannot give the backup, and the caller then goes on as
// for a job without one: the *arr does not know it as a Backup command (it was replaced, or its
// commands were trimmed), or the *arr stopped it before it ended (stoppedCommand: a restart marks
// every started command "orphaned"), when stopped is set too.
func (w *run) resumeCommand(ctx context.Context, id int64) (b arr.Backup, ok, stopped bool, err error) {
	cmd, err := w.client.Command(ctx, id)
	switch {
	case errors.Is(err, arr.ErrNotFound):
		w.rep.Log(slog.LevelInfo, "The Backup command an earlier attempt sent is unknown to "+w.app+" now", "commandId", id)
		return arr.Backup{}, false, false, nil
	case err != nil:
		if ctx.Err() != nil {
			return arr.Backup{}, false, false, ctx.Err()
		}
		return arr.Backup{}, false, false, fmt.Errorf("follow %s's backup command: %w", w.app, err)
	case !strings.EqualFold(cmd.Name, "Backup"):
		w.rep.Log(slog.LevelInfo, "The command an earlier attempt sent is not a Backup command in "+w.app+" now", "commandId", id,
			"name", truncate(cmd.Name, 64))
		return arr.Backup{}, false, false, nil
	case stoppedCommand(cmd.Status):
		w.rep.Log(slog.LevelInfo, w.app+" stopped the Backup command an earlier attempt sent before it finished; asking for a new backup",
			"commandId", id, "status", truncate(cmd.Status, 32))
		return arr.Backup{}, false, true, nil
	}
	cmd.ID = id
	w.rep.Progress(jobs.Progress{Phase: "backing-up", CurrentFile: w.app})
	w.rep.Log(slog.LevelInfo, "Following the Backup command an earlier attempt sent to "+w.app, "commandId", id,
		"status", truncate(cmd.Status, 32))
	w.saveCommand(ctx, id) // again: DeleteItems removed it, and this attempt may be stopped too
	b, err = w.awaitBackup(ctx, cmd)
	return b, true, false, err
}

// stoppedCommand reports whether an *arr command status says the command ended without running
// to its end: "orphaned" (the *arr restarted while it ran), "aborted" or "cancelled". A "failed"
// command ran and failed; a resumed job reports it as the first attempt would have.
func stoppedCommand(status string) bool {
	switch status {
	case "orphaned", "aborted", "cancelled":
		return true
	}
	return false
}

// interruptedCommand returns the Backup command an interrupted attempt of this job recorded
// (saveCommand) and had not finished waiting for, or 0. It is read before the job's items are
// deleted.
func (w *run) interruptedCommand(ctx context.Context) (int64, error) {
	if !w.resumed() {
		return 0, nil
	}
	pending, err := w.items.Pending(ctx, w.job.ID, 0, 16)
	if err != nil {
		return 0, err
	}
	for _, it := range pending {
		if it.RelPath != newBackupRel || it.Action != jobs.ActionBackup {
			continue
		}
		var d itemDetail
		if json.Unmarshal(it.Detail, &d) == nil && d.CommandID > 0 {
			return d.CommandID, nil
		}
	}
	return 0, nil
}

// saveCommand records the Backup command being waited for as a pending item, so an attempt that
// resumes after a crash or a shutdown follows it (interruptedCommand) instead of sending another.
// It is written even while the job is being cancelled; a failure is a warning.
func (w *run) saveCommand(ctx context.Context, id int64) {
	bg := context.WithoutCancel(ctx)
	it := jobs.Item{RelPath: newBackupRel, Action: jobs.ActionBackup, Status: jobs.ItemPending,
		Detail: itemDetail{Method: w.fetch, CommandID: id}.json()}
	err := w.items.AddItems(bg, w.job.ID, []jobs.Item{it}, false)
	if err == nil {
		var pending []jobs.Item
		if pending, err = w.items.Pending(bg, w.job.ID, 0, 16); err == nil {
			for _, p := range pending {
				if p.RelPath == newBackupRel {
					it = p
				}
			}
		}
	}
	if err != nil {
		w.warn("The Backup command could not be recorded: if Bunkarr stops before "+w.app+" has made the backup, the resumed job "+
			"asks for another", "commandId", id, "error", err.Error())
	}
	w.cmdItem = it
}

// commandEnd settles the command's item once the wait ended with err: removed when the backup was
// found (the backup's own item follows), failed when the wait failed; kept pending when the job is
// being cancelled or stopped, for the resumed attempt.
func (w *run) commandEnd(ctx context.Context, err error) {
	bg := context.WithoutCancel(ctx)
	switch {
	case err == nil:
		// Only the command's item exists at this point (backup deleted the others).
		if derr := w.items.DeleteItems(bg, w.job.ID); derr != nil {
			w.rep.Log(slog.LevelWarn, "Could not remove the item of the finished Backup command", "error", derr.Error())
		}
	case ctx.Err() != nil:
	case w.cmdItem.ID != 0:
		if ferr := w.items.Finish(bg, w.cmdItem.ID, jobs.ItemFailed, 0, err.Error()); ferr != nil {
			w.rep.Log(slog.LevelWarn, "Could not record the failure of the Backup command", "error", ferr.Error())
		}
	}
	w.cmdItem = jobs.Item{}
}

// awaitBackup polls cmd until it ends and returns the manual backup it made.
func (w *run) awaitBackup(ctx context.Context, cmd arr.Command) (arr.Backup, error) {
	b, err := w.waitBackup(ctx, cmd)
	w.commandEnd(ctx, err)
	return b, err
}

// waitBackup is awaitBackup without the item.
func (w *run) waitBackup(ctx context.Context, cmd arr.Command) (arr.Backup, error) {
	wctx, cancel := context.WithTimeout(ctx, w.r.cmdTimeout)
	defer cancel()
	errs := 0
	for !commandEnded(cmd.Status) {
		select {
		case <-wctx.Done():
			if ctx.Err() != nil {
				return arr.Backup{}, ctx.Err()
			}
			return arr.Backup{}, fmt.Errorf("%w: %s did not finish its backup within %s (status %q)", ErrCommand, w.app, w.r.cmdTimeout, cmd.Status)
		case <-time.After(w.r.poll):
		}
		next, err := w.client.Command(wctx, cmd.ID)
		if err != nil {
			if ctx.Err() != nil {
				return arr.Backup{}, ctx.Err()
			}
			if errs++; errs >= maxPollErrors {
				return arr.Backup{}, fmt.Errorf("follow %s's backup command: %w", w.app, err)
			}
			w.rep.Log(slog.LevelInfo, "Could not read the state of the backup command; asking again", "error", err.Error())
			continue
		}
		errs = 0
		cmd = next
	}
	if cmd.Status != "completed" || !strings.EqualFold(cmd.Result, "successful") {
		return arr.Backup{}, fmt.Errorf("%w: status %q, result %q: %s", ErrCommand, cmd.Status, cmd.Result, truncate(cmd.Message, 200))
	}
	since := cmd.Queued.Add(-commandQueuedSlack)
	for attempt := 0; ; attempt++ {
		list, err := w.client.Backups(ctx)
		if err != nil {
			return arr.Backup{}, fmt.Errorf("list %s's backups: %w", w.app, err)
		}
		if b, ok := w.newestValid(list, arr.BackupManual, since); ok {
			w.rep.Log(slog.LevelInfo, w.app+" made a backup", "backup", b.Name, "size", b.Size)
			return b, nil
		}
		if attempt >= w.r.retries {
			return arr.Backup{}, fmt.Errorf("%w: no manual backup made since %s", ErrNoBackup, since.UTC().Format(time.RFC3339))
		}
		if err := sleep(ctx, w.r.retryDelay); err != nil {
			return arr.Backup{}, err
		}
	}
}

// commandEnded reports whether an *arr command status is final.
func commandEnded(status string) bool {
	switch status {
	case "completed", "failed", "aborted", "cancelled", "orphaned":
		return true
	}
	return false
}

// itemDetail is the detail of a job's item.
type itemDetail struct {
	Method      string    `json:"method"`
	BackupName  string    `json:"backupName,omitempty"`
	BackupType  string    `json:"backupType,omitempty"`
	BackupTime  time.Time `json:"backupTime,omitzero"`
	Size        int64     `json:"size,omitempty"`
	SHA256      string    `json:"sha256,omitempty"`
	Destination string    `json:"destination,omitempty"`
	Temp        string    `json:"temp,omitempty"`
	// Would is what a dry run would do: "copy-scheduled", "create" or "unchanged".
	Would string `json:"would,omitempty"`
	// SnapshotID is the version an unchanged job found.
	SnapshotID int64  `json:"snapshotId,omitempty"`
	Problem    string `json:"problem,omitempty"`
	// CommandID is the *arr's id of the Backup command the job waits for (its newBackupRel item).
	CommandID int64 `json:"commandId,omitempty"`
}

func (d itemDetail) json() json.RawMessage {
	b, err := json.Marshal(d)
	if err != nil {
		return json.RawMessage("{}")
	}
	return b
}

// relOf is a backup's item path: "<type>/<name>".
func relOf(b arr.Backup) string { return b.Type + "/" + b.Name }

// staged is a backup zip fetched into staging.
type staged struct {
	path   string
	size   int64
	sha256 string
}

// backup runs a real backup (see Run).
func (w *run) backup(ctx context.Context) (res jobs.Result, err error) {
	staging := w.r.StagingDir(w.job.ID)
	partial := w.folder + "/" + snapshots.PartialName(w.job.ID)
	defer func() {
		// A failed or cancelled job leaves nothing but what it recorded. (A crash panics past this
		// with err nil and leaves its state to the resumed job, as a real crash would.)
		if err != nil {
			if rerr := os.RemoveAll(staging); rerr != nil {
				w.r.log.Error("Could not remove an *arr backup staging directory", "path", staging, "error", rerr)
			}
			if rerr := removeTree(w.h.Root, partial); rerr != nil {
				w.r.log.Error("Could not remove an unfinished *arr backup version", "path", partial, "error", rerr)
			}
		}
	}()

	if err := os.RemoveAll(staging); err != nil {
		return jobs.Result{}, fmt.Errorf("remove the staging directory of an earlier attempt: %w", err)
	}
	own, err := w.recover(ctx)
	if err != nil {
		return jobs.Result{}, err
	}
	if own != nil {
		return w.finishRecovered(ctx, *own)
	}
	if w.resumeCmd, err = w.interruptedCommand(ctx); err != nil {
		return jobs.Result{}, err
	}
	if err := w.items.DeleteItems(ctx, w.job.ID); err != nil {
		return jobs.Result{}, err
	}
	status, err := w.client.Status(ctx)
	if err != nil {
		return jobs.Result{}, fmt.Errorf("%s %q: %w", w.app, w.it.Name, err)
	}
	c, err := w.choose(ctx)
	if err != nil {
		return jobs.Result{}, err
	}
	b := c.b
	w.stats.BackupName, w.stats.BackupType, w.stats.ReusedScheduled = b.Name, b.Type, c.reused
	if c.unchanged != nil {
		return w.finishUnchanged(ctx, b, *c.unchanged)
	}
	if b.Size > MaxBackupBytes {
		return jobs.Result{}, fmt.Errorf("%s's backup %s is %s, more than the %s Bunkarr accepts", w.app, b.Name,
			formatBytes(b.Size), formatBytes(MaxBackupBytes))
	}

	if err := w.prepareStaging(staging, b.Size); err != nil {
		return jobs.Result{}, err
	}
	w.rep.Progress(jobs.Progress{Phase: "fetching", FilesTotal: 1, BytesTotal: b.Size, CurrentFile: b.Name})
	st, err := w.fetchBackup(ctx, b, staging)
	if err != nil {
		return jobs.Result{}, err
	}
	faultinject.Point(PointAfterStage)

	w.rep.Progress(jobs.Progress{Phase: "verifying", FilesTotal: 1, BytesTotal: st.size, CurrentFile: b.Name})
	report, err := VerifyZip(ctx, st.path, string(w.kind), staging)
	if err != nil {
		return jobs.Result{}, err
	}
	integrity := report.Integrity()
	if report.OK() {
		w.rep.Log(slog.LevelInfo, "The backup passed verification", "backup", b.Name, "entries", len(report.Entries))
	} else {
		w.rep.Log(slog.LevelError, "The backup failed verification", "backup", b.Name, "zip", integrity.Zip,
			"database", integrity.Database, "problems", strings.Join(report.Problems, "; "))
	}
	created := w.r.now().UTC()
	versionName := snapshots.VersionName(created)

	detail := itemDetail{Method: w.fetch, BackupName: b.Name, BackupType: b.Type, BackupTime: b.Time, Size: st.size,
		SHA256: st.sha256, Destination: w.folder + "/" + versionName + "/" + b.Name}
	item, err := w.addItem(ctx, jobs.Item{RelPath: relOf(b), Action: jobs.ActionBackup, Status: jobs.ItemPending, Bytes: st.size,
		Detail: detail.json()})
	if err != nil {
		return jobs.Result{}, err
	}
	if err := w.h.Recheck(); err != nil {
		return jobs.Result{}, err
	}
	if free, _, err := filecopy.FreeSpace(w.h.Root); err == nil && free < uint64(st.size)+spaceMargin {
		return jobs.Result{}, fmt.Errorf("not enough free space at destination %q: the backup needs %s, %s is free",
			w.h.Destination.Name, formatBytes(st.size), formatBytes(int64(free)))
	}
	if err := w.copyZip(ctx, staging, partial, b.Name, st, item, detail); err != nil {
		return jobs.Result{}, err
	}
	m := Manifest{Format: ManifestFormat, CreatedAt: created, App: string(w.kind), AppVersion: status.Version,
		IntegrationID: w.it.ID, IntegrationName: w.it.Name, JobID: w.job.ID, JobQueuedAt: w.job.QueuedAt,
		Method: snapshotMethod(w.fetch), Backup: ManifestBackup{Name: b.Name, Type: b.Type, Time: b.Time},
		Zip: ManifestZip{Name: b.Name, Size: st.size, SHA256: st.sha256}, Entries: report.Entries, Integrity: integrity,
		Sensitive: true, Warnings: w.noted}
	if m.Entries == nil {
		m.Entries = []ZipEntry{}
	}
	manifest, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return jobs.Result{}, fmt.Errorf("encode the manifest: %w", err)
	}
	if err := filecopy.WriteFileAtomic(w.h.Root, partial+"/"+ManifestName, append(manifest, '\n'), filePerm, true); err != nil {
		return jobs.Result{}, fmt.Errorf("write the manifest: %w", err)
	}

	faultinject.Point(PointBeforeRename)
	versionRel, err := snapshots.RenameVersion(w.h.Root, partial, w.folder, versionName, w.job.ID)
	if err != nil {
		return jobs.Result{}, err
	}
	faultinject.Point(PointAfterRename)
	faultinject.Point(PointBeforeRecord)
	snap, err := w.record(ctx, snapshots.Snapshot{DestinationID: w.h.Destination.ID, Kind: snapshots.KindArr, IntegrationID: w.it.ID,
		JobID: w.job.ID, Path: versionRel, CreatedAt: created, Size: st.size, Method: m.Method, Integrity: integrity.Result(),
		Manifest: manifest})
	if err != nil {
		return jobs.Result{}, err
	}
	faultinject.Point(PointAfterRecord)
	if err := os.RemoveAll(staging); err != nil {
		w.r.log.Error("Could not remove an *arr backup staging directory", "path", staging, "error", err)
	}
	w.rep.Log(slog.LevelInfo, "Recorded the "+w.app+" backup version", "path", versionRel, "integrity", snap.Integrity, "size", st.size)
	w.stats.Bytes, w.stats.Integrity = st.size, snap.Integrity
	w.stats.SnapshotID, w.stats.Path = snap.ID, snap.Path
	if snap.Integrity != IntegrityOK {
		return w.failedVerification(report.Problems)
	}
	w.prune(ctx)
	return w.result(w.summary()), nil
}

// record inserts the version's row even when the job is being cancelled (the version is complete
// at its final name). When the integration was deleted during the backup, no later job would
// record or prune the version, so it is recorded without the link.
func (w *run) record(ctx context.Context, rec snapshots.Snapshot) (snapshots.Snapshot, error) {
	bg := context.WithoutCancel(ctx)
	snap, err := w.r.store.Insert(bg, rec)
	if err == nil {
		return snap, nil
	}
	if _, gerr := w.r.integrations.Get(bg, w.it.ID); errors.Is(gerr, integrations.ErrNotFound) {
		rec.IntegrationID = 0
		if snap, err = w.r.store.Insert(bg, rec); err == nil {
			w.warn("The integration was deleted during the backup; its version was recorded without it", "path", rec.Path)
			return snap, nil
		}
	}
	return snapshots.Snapshot{}, err
}

// snapshotMethod maps a fetch method to snapshots.method.
func snapshotMethod(fetch string) string {
	if fetch == FetchFolder {
		return MethodFolder
	}
	return MethodHTTP
}

// addItem records one item and returns it with its id.
func (w *run) addItem(ctx context.Context, it jobs.Item) (jobs.Item, error) {
	if err := w.items.AddItems(ctx, w.job.ID, []jobs.Item{it}, true); err != nil {
		return jobs.Item{}, err
	}
	if it.Status != jobs.ItemPending {
		return it, nil
	}
	pending, err := w.items.Pending(ctx, w.job.ID, 0, 2)
	if err != nil {
		return jobs.Item{}, err
	}
	for _, p := range pending {
		if p.RelPath == it.RelPath {
			return p, nil
		}
	}
	return it, nil
}

// finishUnchanged completes a job whose scheduled backup is already at the destination.
func (w *run) finishUnchanged(ctx context.Context, b arr.Backup, s snapshots.Snapshot) (jobs.Result, error) {
	d := itemDetail{Method: w.fetch, BackupName: b.Name, BackupType: b.Type, BackupTime: b.Time, Size: b.Size, Would: "unchanged",
		SnapshotID: s.ID, Destination: s.Path + "/" + b.Name}
	if _, err := w.addItem(ctx, jobs.Item{RelPath: relOf(b), Action: jobs.ActionSkip, Status: jobs.ItemSkipped, Bytes: b.Size,
		Detail: d.json()}); err != nil {
		return jobs.Result{}, err
	}
	w.stats.Unchanged = true
	w.stats.Integrity, w.stats.SnapshotID, w.stats.Path = s.Integrity, s.ID, s.Path
	w.rep.Log(slog.LevelInfo, "The scheduled backup is already at the destination; nothing was copied", "backup", b.Name, "path", s.Path)
	return w.result(fmt.Sprintf("%s's scheduled backup %s of %q is already at %q; nothing to copy", w.app, b.Name, w.it.Name,
		w.h.Destination.Name)), nil
}

// failedVerification is the outcome of a job whose recorded version failed verification (kept,
// no pruning).
func (w *run) failedVerification(problems []string) (jobs.Result, error) {
	res := w.result(fmt.Sprintf("The %s backup of %q failed verification; the version is kept for 7 days for diagnosis", w.app, w.it.Name))
	if len(problems) > 3 {
		problems = problems[:3]
	}
	return res, fmt.Errorf("%w: %s", ErrIntegrity, strings.Join(problems, "; "))
}

// summary is a successful backup's summary sentence.
func (w *run) summary() string {
	s := fmt.Sprintf("Backed up %s %q to %q: %s (%s), integrity ok", w.app, w.it.Name, w.h.Destination.Name, w.stats.BackupName,
		formatBytes(w.stats.Bytes))
	if w.stats.ReusedScheduled {
		s += "; its own scheduled backup was copied"
	}
	if w.stats.VersionsPruned > 0 {
		s += fmt.Sprintf("; %d old versions pruned", w.stats.VersionsPruned)
	}
	return s
}

// prepareStaging creates the job's staging directory (0700) after checking that the staging
// filesystem can hold the zip.
func (w *run) prepareStaging(staging string, size int64) error {
	base := filepath.Join(w.r.configDir, StagingRoot)
	if err := os.MkdirAll(base, dirPerm); err != nil {
		return fmt.Errorf("create the staging directory: %w", err)
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return fmt.Errorf("open the staging directory: %w", err)
	}
	free, _, ferr := filecopy.FreeSpace(root)
	_ = root.Close()
	if ferr == nil && size > 0 && free < uint64(size)+spaceMargin {
		return fmt.Errorf("not enough free space in the config directory for staging the %s backup: it needs %s, %s is free",
			w.app, formatBytes(size), formatBytes(int64(free)))
	}
	if err := os.Mkdir(staging, dirPerm); err != nil {
		return fmt.Errorf("create the staging directory: %w", err)
	}
	return os.Chmod(staging, dirPerm)
}

// fetchBackup copies backup b into staging with the job's method and returns it hashed.
func (w *run) fetchBackup(ctx context.Context, b arr.Backup, staging string) (staged, error) {
	p := filepath.Join(staging, b.Name)
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, filePerm)
	if err != nil {
		return staged{}, fmt.Errorf("create the staged backup: %w", err)
	}
	h := sha256.New()
	out := io.MultiWriter(f, h)
	var n int64
	if w.fetch == FetchFolder {
		n, err = w.readFromFolder(ctx, b, out)
	} else {
		n, err = w.download(ctx, b, out)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil && cerr != nil {
		err = fmt.Errorf("write the staged backup: %w", cerr)
	}
	if err != nil {
		return staged{}, err
	}
	if b.Size > 0 && n != b.Size {
		return staged{}, fmt.Errorf("the backup %s has %d bytes, %s reported %d", b.Name, n, w.app, b.Size)
	}
	w.rep.Log(slog.LevelInfo, "Fetched the backup", "backup", b.Name, "method", w.fetch, "size", n)
	return staged{path: p, size: n, sha256: hex.EncodeToString(h.Sum(nil))}, nil
}

// download fetches b over HTTP (the non-API route, the key header, no redirects).
func (w *run) download(ctx context.Context, b arr.Backup, out io.Writer) (int64, error) {
	n, err := w.client.DownloadBackup(ctx, b, out)
	if errors.Is(err, arr.ErrLoginRequired) {
		return n, &LoginRequiredError{App: w.app}
	}
	if err != nil {
		return n, fmt.Errorf("download %s's backup: %w", w.app, err)
	}
	return n, nil
}

// readFromFolder copies <backupFolder>/<type>/<name> into out (S18: an os.Root on the folder,
// O_RDONLY|O_NOFOLLOW, a regular file). While the file is smaller than the API's size (the *arr
// may still be writing it) it is looked at again, up to FolderRetries times.
func (w *run) readFromFolder(ctx context.Context, b arr.Backup, out io.Writer) (int64, error) {
	rel, err := arr.BackupPath(b)
	if err != nil {
		return 0, err
	}
	root, err := os.OpenRoot(w.as.BackupFolder)
	if err != nil {
		return 0, fmt.Errorf("%w: the Backups folder %s cannot be opened (%v): mount %s's Backups folder into Bunkarr, read-only",
			ErrFolder, w.as.BackupFolder, pathError(err), w.app)
	}
	defer root.Close()
	var fi fs.FileInfo
	for attempt := 0; ; attempt++ {
		fi, err = root.Lstat(rel)
		switch {
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			return 0, fmt.Errorf("%w: %s: %v", ErrFolder, rel, pathError(err))
		case err == nil && !fi.Mode().IsRegular():
			return 0, fmt.Errorf("%w: %s is not a regular file", ErrFolder, rel)
		case err == nil && (b.Size <= 0 || fi.Size() == b.Size):
		case err == nil && fi.Size() > b.Size:
			return 0, fmt.Errorf("%w: %s has %d bytes, more than the %d %s reported", ErrFolder, rel, fi.Size(), b.Size, w.app)
		default:
			if attempt < w.r.retries {
				if err := sleep(ctx, w.r.retryDelay); err != nil {
					return 0, err
				}
				continue
			}
			if errors.Is(err, fs.ErrNotExist) {
				return 0, fmt.Errorf("%w: %s is not in %s (is it %s's Backups folder?)", ErrFolder, rel, w.as.BackupFolder, w.app)
			}
			return 0, fmt.Errorf("%w: %s has %d of the %d bytes %s reported", ErrFolder, rel, fi.Size(), b.Size, w.app)
		}
		break
	}
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return 0, fmt.Errorf("%w: %s: %v", ErrFolder, rel, pathError(err))
	}
	defer f.Close()
	ofi, err := f.Stat()
	if err != nil || !ofi.Mode().IsRegular() || !os.SameFile(fi, ofi) {
		return 0, fmt.Errorf("%w: %s changed while it was opened", ErrFolder, rel)
	}
	limit := int64(MaxBackupBytes)
	if b.Size > 0 {
		limit = b.Size
	}
	n, err := io.Copy(out, &ctxReader{ctx: ctx, r: io.LimitReader(f, limit+1)})
	if err != nil {
		if ctx.Err() != nil {
			return n, ctx.Err()
		}
		return n, fmt.Errorf("%w: read %s: %v", ErrFolder, rel, pathError(err))
	}
	if n > limit {
		return n, fmt.Errorf("%w: %s grew while it was read", ErrFolder, rel)
	}
	return n, nil
}

// pathError drops the path an *os.PathError repeats (the caller names the file).
func pathError(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// copyZip copies the staged zip into the partial directory with the filecopy engine (S7: temp
// file, sha256 compared with the staged copy, re-read unless the destination's verify mode is
// off, rename), files 0600 and directories 0700 (S17).
func (w *run) copyZip(ctx context.Context, staging, partial, name string, st staged, item jobs.Item, detail itemDetail) error {
	if err := w.h.Root.MkdirAll(partial, dirPerm); err != nil {
		return fmt.Errorf("create %s: %w", partial, err)
	}
	for _, dir := range []string{ArrRoot, w.folder, partial} {
		// A destination that does not keep modes (accepted with acceptInsecureModes) may refuse
		// the change as well; one that keeps them must take it.
		if err := w.h.Root.Chmod(dir, dirPerm); err != nil && w.h.Capabilities.EnforcesModes {
			return fmt.Errorf("set the mode of %s: %w", dir, err)
		}
	}
	src, err := os.OpenRoot(staging)
	if err != nil {
		return fmt.Errorf("open the staging directory: %w", err)
	}
	defer src.Close()
	dstRel := partial + "/" + name
	progress := jobs.Progress{Phase: "copying", FilesTotal: 1, BytesTotal: st.size, CurrentFile: name}
	w.rep.Progress(progress)
	t, err := filecopy.WriteTemp(ctx, src, name, w.h.Root, dstRel, filecopy.CopyOptions{
		Hash:     true,
		FilePerm: filePerm,
		DirPerm:  dirPerm,
		OnTemp: func(tempRel string) error {
			if item.ID == 0 {
				return nil
			}
			detail.Temp = tempRel
			return w.items.SetDetail(ctx, item.ID, detail.json())
		},
		Progress: func(delta int64) {
			progress.BytesDone += delta
			w.rep.Progress(progress)
		},
	})
	if err != nil {
		return fmt.Errorf("copy %s to the destination: %w", name, err)
	}
	if t.Hash != filecopy.HashPrefix+st.sha256 {
		_ = filecopy.CleanupTemp(w.h.Root, t.Rel)
		return fmt.Errorf("copy %s to the destination: %w: sha256 %s, staged %s", name, filecopy.ErrMismatch, t.Hash, st.sha256)
	}
	if w.h.Settings.Verify.Mode != destinations.VerifyOff {
		if err := filecopy.VerifyTemp(ctx, w.h.Root, t); err != nil {
			_ = filecopy.CleanupTemp(w.h.Root, t.Rel)
			return fmt.Errorf("re-read %s at the destination: %w", name, err)
		}
	}
	if err := filecopy.Commit(w.h.Root, t.Rel, dstRel, true); err != nil {
		_ = filecopy.CleanupTemp(w.h.Root, t.Rel)
		return fmt.Errorf("copy %s to the destination: %w", name, err)
	}
	faultinject.Point(PointAfterCopy)
	if item.ID != 0 {
		if err := w.items.Finish(ctx, item.ID, jobs.ItemDone, t.Size, ""); err != nil {
			return err
		}
	}
	w.rep.Progress(jobs.Progress{Phase: "finishing", FilesTotal: 1, FilesDone: 1, BytesTotal: st.size, BytesDone: st.size})
	return nil
}

// dryRun reports what a backup would do (see Run): it lists the *arr's backups and checks that
// the method can reach them, sends no command and writes nothing.
func (w *run) dryRun(ctx context.Context) (jobs.Result, error) {
	w.stats.DryRun = true
	status, err := w.client.Status(ctx)
	if err != nil {
		return jobs.Result{}, fmt.Errorf("%s %q: %w", w.app, w.it.Name, err)
	}
	list, err := w.client.Backups(ctx)
	if err != nil {
		return jobs.Result{}, fmt.Errorf("list %s's backups: %w", w.app, err)
	}
	if err := w.items.DeleteItems(ctx, w.job.ID); err != nil {
		return jobs.Result{}, err
	}
	d := itemDetail{Method: w.fetch, Destination: w.folder + "/<" + snapshots.VersionLayout + ">"}
	item := jobs.Item{Action: jobs.ActionSkip, Status: jobs.ItemSkipped}
	var summary string
	b, fresh := w.freshScheduled(list)
	var done *snapshots.Snapshot
	if fresh {
		var bad bool
		if done, bad, err = w.alreadyCopied(ctx, b); err != nil {
			return jobs.Result{}, err
		}
		fresh = !bad
	}
	if fresh {
		d.BackupName, d.BackupType, d.BackupTime, d.Size = b.Name, b.Type, b.Time, b.Size
		item.RelPath, item.Bytes = relOf(b), b.Size
		w.stats.BackupName, w.stats.BackupType, w.stats.ReusedScheduled = b.Name, b.Type, true
		if done != nil {
			d.Would, d.SnapshotID = "unchanged", done.ID
			w.stats.Unchanged = true
			summary = fmt.Sprintf("Dry run: %s's scheduled backup %s is already at %q; nothing would be copied", w.app, b.Name, w.h.Destination.Name)
		} else {
			d.Would = "copy-scheduled"
			summary = fmt.Sprintf("Dry run: would copy %s's scheduled backup %s (%s) to %q (%s)", w.app, b.Name, formatBytes(b.Size),
				w.h.Destination.Name, w.fetch)
		}
		d.Problem = w.accessProblem(ctx, b, true)
	} else {
		d.Would = "create"
		item.RelPath = newBackupRel
		summary = fmt.Sprintf("Dry run: would ask %s %q to make a backup (%s %s) and copy it to %q (%s)", w.app, w.it.Name, w.app,
			status.Version, w.h.Destination.Name, w.fetch)
		// The access is checked on the newest backup of any type the *arr lists.
		if newest, found := w.newestAny(list); found {
			d.Problem = w.accessProblem(ctx, newest, false)
		} else if w.fetch == FetchFolder {
			d.Problem = w.folderProblem()
		} else {
			w.rep.Log(slog.LevelInfo, "The download over HTTP could not be checked: "+w.app+" lists no backup yet")
		}
	}
	if d.Problem != "" {
		item.Status, item.Error = jobs.ItemFailed, d.Problem
		w.warn("The backup would fail: "+d.Problem, "method", w.fetch)
	}
	item.Detail = d.json()
	if err := w.items.AddItems(ctx, w.job.ID, []jobs.Item{item}, true); err != nil {
		return jobs.Result{}, err
	}
	w.rep.Log(slog.LevelInfo, summary)
	if item.Bytes > 0 {
		if free, _, err := filecopy.FreeSpace(w.h.Root); err == nil && uint64(item.Bytes)+spaceMargin > free {
			w.warn("The destination does not have enough free space for this backup", "need", item.Bytes, "free", free)
		}
	}
	w.stats.Bytes = item.Bytes
	return w.result(summary), nil
}

// accessProblem checks, without copying it, that the job's method can read backup b: its file in
// the Backups folder (exact is set when b itself would be copied, so its size must match), or a
// 1-byte range GET over HTTP. It returns "" when it can.
func (w *run) accessProblem(ctx context.Context, b arr.Backup, exact bool) string {
	if w.fetch == FetchFolder {
		if p := w.folderProblem(); p != "" {
			return p
		}
		root, err := os.OpenRoot(w.as.BackupFolder)
		if err != nil {
			return fmt.Sprintf("the Backups folder %s cannot be opened: %v", w.as.BackupFolder, pathError(err))
		}
		defer root.Close()
		rel, err := arr.BackupPath(b)
		if err != nil {
			return err.Error()
		}
		fi, err := root.Lstat(rel)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return fmt.Sprintf("%s is not in %s (is it %s's Backups folder?)", rel, w.as.BackupFolder, w.app)
		case err != nil:
			return fmt.Sprintf("%s cannot be read: %v", rel, pathError(err))
		case !fi.Mode().IsRegular():
			return rel + " is not a regular file"
		case exact && b.Size > 0 && fi.Size() != b.Size:
			return fmt.Sprintf("%s has %d bytes, %s reported %d", rel, fi.Size(), w.app, b.Size)
		}
		f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return fmt.Sprintf("%s cannot be read: %v", rel, pathError(err))
		}
		_ = f.Close()
		return ""
	}
	access, err := w.client.ProbeBackup(ctx, b)
	switch {
	case err != nil:
		return fmt.Sprintf("the download could not be checked: %v", err)
	case access == arr.BackupAccessLoginRequired:
		return (&LoginRequiredError{App: w.app}).Error()
	}
	return ""
}

// folderProblem checks that the Backups folder is a readable directory.
func (w *run) folderProblem() string {
	root, err := os.OpenRoot(w.as.BackupFolder)
	if err != nil {
		return fmt.Sprintf("the Backups folder %s cannot be opened (%v): mount %s's Backups folder into Bunkarr, read-only",
			w.as.BackupFolder, pathError(err), w.app)
	}
	defer root.Close()
	d, err := root.Open(".")
	if err == nil {
		_, err = d.Readdirnames(1)
		_ = d.Close()
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Sprintf("the Backups folder %s cannot be read: %v", w.as.BackupFolder, pathError(err))
	}
	return ""
}

// cleanStaleStaging removes staging directories of other *arr backup jobs (not jobID's) that
// nothing has touched for a day: jobs whose own removal (Run, OnJobFinish) did not happen, e.g.
// with a lost database. Best effort.
func (r *Runner) cleanStaleStaging(jobID int64) {
	dir := filepath.Join(r.configDir, StagingRoot)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	own := stagingPrefix + strconv.FormatInt(jobID, 10)
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), stagingPrefix) || e.Name() == own {
			continue
		}
		if _, err := strconv.ParseInt(strings.TrimPrefix(e.Name(), stagingPrefix), 10, 64); err != nil {
			continue
		}
		fi, err := e.Info()
		if err != nil || r.now().Sub(fi.ModTime()) < staleStaging {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if err := os.RemoveAll(p); err != nil {
			r.log.Warn("Could not remove a stale *arr backup staging directory", "path", p, "error", err)
			continue
		}
		r.log.Info("Removed a stale *arr backup staging directory", "path", p)
	}
}

// sleep waits d or until ctx ends.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// truncate shortens s to n bytes.
func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
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
