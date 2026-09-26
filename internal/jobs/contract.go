// Package jobs is the contract between Bunkarr's job manager (internal/jobqueue: a persistent,
// resumable queue with worker limits, cancellation, progress and the cron scheduler) and the job
// runners (internal/catalog, internal/syncer, internal/plexdb, and from Phases 2-3
// internal/mediaindex, internal/arrbackup, internal/manifest). It holds types, interfaces and
// pure helpers only, so runners never depend on the manager's implementation; see
// docs/design/phase1.md §6 and docs/design/phase2-3.md §12.
package jobs

import (
	"context"
	"encoding/json"
	"log/slog"
	"path"
	"strings"
	"time"
)

// Type is the kind of job.
type Type string

// Job types.
const (
	TypeScan         Type = "scan"
	TypeSync         Type = "sync"
	TypePlexDBBackup Type = "plexdb_backup"
	TypeRetention    Type = "retention"
	TypeVerify       Type = "verify"
)

// Job types of Phases 2 and 3 (docs/design/phase2-3.md §12.1).
const (
	// TypeRefresh refreshes the metadata cache of one integration (Params.IntegrationID): the
	// *arr index of Sonarr, Radarr or Lidarr, the Plex library index, or the Tautulli, Seerr or
	// Maintainerr facts. With ArrItemIDs it refreshes only those *arr items; with SyncAfter it
	// then queues the targeted syncs of their folders (the webhook path). A full *arr refresh
	// also queues targeted syncs for the items whose files changed (the reconcile, phase2-3.md
	// §6.1). Refresh jobs run in their own pool, not counted against jobs.workers (§12.1).
	TypeRefresh Type = "refresh"
	// TypeArrBackup backs up an *arr's configuration and database through its backup API
	// (Params.IntegrationID, Params.DestinationID; 0 = the integration's backup destination).
	TypeArrBackup Type = "arr_backup"
	// TypeManifestExport writes a manifest version (JSON and CSV of every library item and file)
	// to a destination (Params.DestinationID).
	TypeManifestExport Type = "manifest_export"
)

// Limits of targeted jobs. A merge that would exceed one of them makes the queued job untargeted
// instead (it then covers its whole scope), never drops the extra targets; a refresh keeps
// SyncAfter (phase2-3.md §12.2).
const (
	// MaxTargetPaths is the most Params.Paths a targeted sync carries.
	MaxTargetPaths = 1000
	// MaxTargetItems is the most Params.ArrItemIDs a targeted refresh carries.
	MaxTargetItems = 500
)

// Status is a job's lifecycle state.
type Status string

// Job statuses. queued and running are active; the others are final.
const (
	StatusQueued                Status = "queued"
	StatusRunning               Status = "running"
	StatusCompleted             Status = "completed"
	StatusCompletedWithWarnings Status = "completed_with_warnings"
	StatusFailed                Status = "failed"
	StatusCancelled             Status = "cancelled"
)

// Final reports whether s is a terminal status.
func (s Status) Final() bool {
	return s != StatusQueued && s != StatusRunning
}

// Trigger says what started a job.
type Trigger string

// Job triggers.
const (
	TriggerSchedule Trigger = "schedule"
	TriggerManual   Trigger = "manual"
	TriggerWebhook  Trigger = "webhook"
	// TriggerResume marks a job re-queued after the process stopped while it was running.
	TriggerResume  Trigger = "resume"
	TriggerStartup Trigger = "startup"
)

// Params selects what a job works on. Zero values mean "not applicable".
type Params struct {
	DestinationID int64   `json:"destinationId,omitempty"`
	SourceIDs     []int64 `json:"sourceIds,omitempty"`
	IntegrationID int64   `json:"integrationId,omitempty"`
	// AllowChanges runs changes the mass-change guard would hold (design S10b): on a sync, the
	// held retains, updates, releases and unknown-promoted copies; on a refresh, the deletions and
	// cache shrinks the refresh guard held (phase2-3.md S10).
	AllowChanges bool `json:"allowChanges,omitempty"`

	// Paths narrows a sync to these paths inside its one source (SourceIDs then holds exactly one
	// id): paths of files or folders that satisfy ValidTargetPath, e.g. "Heat (1995)". A targeted
	// sync scans and plans only these subtrees, and retains a vanished name only when its
	// directory gets new content in the same plan (phase2-3.md §9.1, D14). Canonical form: sorted,
	// de-duplicated, no path inside another listed path; at most MaxTargetPaths.
	Paths []string `json:"paths,omitempty"`
	// ArrItemIDs narrows a refresh of an *arr integration to these items: the *arr's own movie,
	// series or artist ids. Canonical form: sorted, de-duplicated; at most MaxTargetItems.
	ArrItemIDs []int64 `json:"arrItemIds,omitempty"`
	// SyncAfter makes a refresh queue follow-up syncs when it ends (also when it failed, never
	// when it was cancelled), for every enabled destination linked to the source whose
	// syncOnArrChange setting is on. With ArrItemIDs: a targeted sync of the items' old and new
	// folders per source (trigger webhook). Without ArrItemIDs (only an overflow merge creates
	// that, phase2-3.md §12.2): an untargeted sync of every source the integration's root folders
	// locate into.
	SyncAfter bool `json:"syncAfter,omitempty"`
	// ReleaseDemoted makes a sync release what it keeps at the destination although the file's
	// tier there is no longer full (phase2-3.md S15): those files move into retention with reason
	// released and expire after the destination's deletedDays. Counted by the mass-change guard.
	// A dry run lists the release items; a real run must name that dry run (ReleaseOf) and the
	// rule revision it evaluated (ReleaseRevision), and releases only the records the dry run
	// listed that are still not full when each item runs. Not allowed with Paths.
	ReleaseDemoted bool `json:"releaseDemoted,omitempty"`
	// ReleaseOf is the id of the finished dry-run sync (same destination, ReleaseDemoted) whose
	// release items a real ReleaseDemoted run applies.
	ReleaseOf int64 `json:"releaseOf,omitempty"`
	// ReleaseRevision is the tiers.revision that dry run evaluated (its stats.tierRevision). The
	// enqueue is refused ("rules changed since the preview") unless it equals the current
	// revision, and each release item re-checks it when it runs.
	ReleaseRevision int64 `json:"releaseRevision,omitempty"`
}

// ValidTargetPath reports whether p may appear in Params.Paths: a clean (path.Clean(p) == p),
// relative, slash-separated path that is not "" or ".", has no ".." element and no NUL byte.
// Names that only start with a dot or contain ".." inside a name (".hack SIGN (2002)",
// "Movie..Name") are valid. The source's os.Root is the real fence (safety rule S1); this keeps
// params canonical and refuses paths that could never name something inside a source.
func ValidTargetPath(p string) bool {
	if p == "" || p == "." || p == ".." || path.IsAbs(p) || strings.HasPrefix(p, "../") || strings.ContainsRune(p, 0) {
		return false
	}
	return path.Clean(p) == p
}

// Targeted reports whether p narrows its job to some paths (sync) or items (refresh).
func (p Params) Targeted() bool {
	return len(p.Paths) > 0 || len(p.ArrItemIDs) > 0
}

// Spec is a request to run a job.
type Spec struct {
	Type    Type
	Trigger Trigger
	DryRun  bool
	Params  Params
}

// Job is a queued, running or finished job.
type Job struct {
	ID         int64           `json:"id"`
	Type       Type            `json:"type"`
	Status     Status          `json:"status"`
	Trigger    Trigger         `json:"trigger"`
	DryRun     bool            `json:"dryRun"`
	Params     Params          `json:"params"`
	Attempt    int             `json:"attempt"`
	Progress   Progress        `json:"progress"`
	Stats      json.RawMessage `json:"stats"`
	Warnings   int             `json:"warnings"`
	Summary    string          `json:"summary"`
	Error      string          `json:"error,omitempty"`
	QueuedAt   time.Time       `json:"queuedAt"`
	StartedAt  *time.Time      `json:"startedAt"`
	FinishedAt *time.Time      `json:"finishedAt"`
}

// Progress is a running job's progress. Runners set the counters; the manager fills in
// BytesPerSec and ETASeconds.
type Progress struct {
	// Phase is a short word: scanning, planning, copying, verifying, backing-up, finishing.
	Phase       string  `json:"phase,omitempty"`
	FilesTotal  int64   `json:"filesTotal"`
	FilesDone   int64   `json:"filesDone"`
	BytesTotal  int64   `json:"bytesTotal"`
	BytesDone   int64   `json:"bytesDone"`
	CurrentFile string  `json:"currentFile,omitempty"`
	BytesPerSec float64 `json:"bytesPerSec"`
	ETASeconds  int64   `json:"etaSeconds"`
}

// Result is what a runner returns on success (err == nil). Warnings > 0 makes the job
// completed_with_warnings. Stats must marshal to a JSON object.
type Result struct {
	Stats    any
	Warnings int
	// Summary is one human sentence for the history list and notifications.
	Summary string
}

// ItemAction is what a job item does.
type ItemAction string

// Item actions.
const (
	ActionCopy    ItemAction = "copy"    // new file at the destination
	ActionUpdate  ItemAction = "update"  // changed file: copy new, retain old (safety rule S6)
	ActionMove    ItemAction = "move"    // renamed at the source, same content: rename at the destination
	ActionAdopt   ItemAction = "adopt"   // already at the destination with matching size+mtime
	ActionLink    ItemAction = "link"    // another name of an inode already copied (hardlink)
	ActionPromote ItemAction = "promote" // make a surviving name hold content before its primary is retained
	ActionRetain  ItemAction = "retain"  // gone from the source: move into retention (S5)
	ActionExpire  ItemAction = "expire"  // retention period over: delete the retained copy
	ActionVerify  ItemAction = "verify"  // re-read and compare a destination file
	ActionBackup  ItemAction = "backup"  // a Plex DB backup file
	ActionSkip    ItemAction = "skip"    // recorded for the preview, nothing to do
)

// ItemStatus is an item's state.
type ItemStatus string

// Item statuses.
const (
	ItemPending ItemStatus = "pending"
	ItemDone    ItemStatus = "done"
	ItemFailed  ItemStatus = "failed"
	ItemSkipped ItemStatus = "skipped"
	// ItemHeld is a change the mass-change guard did not run (design S10b); a later sync with
	// Params.AllowChanges plans and runs it again.
	ItemHeld ItemStatus = "held"
)

// Item is one unit of planned work, persisted so a dry run can be previewed and a resumed job can
// continue where it stopped.
type Item struct {
	ID      int64           `json:"id"`
	JobID   int64           `json:"jobId"`
	FileID  int64           `json:"fileId,omitempty"`
	RelPath string          `json:"relPath"`
	Action  ItemAction      `json:"action"`
	Status  ItemStatus      `json:"status"`
	Bytes   int64           `json:"bytes"`
	Error   string          `json:"error,omitempty"`
	Detail  json.RawMessage `json:"detail,omitempty"`
}

// ItemStore persists a job's items. Implemented by internal/jobqueue.
type ItemStore interface {
	// Planned reports whether the job's plan is complete (jobs.planned_at is set). A resumed job
	// with a complete plan executes its pending items; one with items but no complete plan was
	// killed while planning and must DeleteItems and plan again.
	Planned(ctx context.Context, jobID int64) (bool, error)
	// AddItems appends items (Status is normally ItemPending). final=true marks the plan complete
	// in the same transaction as this batch (use an empty batch if the last one was already added).
	AddItems(ctx context.Context, jobID int64, items []Item, final bool) error
	// DeleteItems removes all of the job's items and clears planned_at.
	DeleteItems(ctx context.Context, jobID int64) error
	// Pending returns up to limit items with status pending and id > afterID, in id order.
	Pending(ctx context.Context, jobID int64, afterID int64, limit int) ([]Item, error)
	// SetDetail replaces an item's detail (e.g. to record a temp path before creating it).
	SetDetail(ctx context.Context, itemID int64, detail json.RawMessage) error
	// Finish records an item's outcome.
	Finish(ctx context.Context, itemID int64, status ItemStatus, bytes int64, errMsg string) error
	// Counts returns the number of items and their bytes by action and status.
	Counts(ctx context.Context, jobID int64) ([]ItemCount, error)
}

// ItemCount is one row of ItemStore.Counts.
type ItemCount struct {
	Action ItemAction `json:"action"`
	Status ItemStatus `json:"status"`
	Files  int64      `json:"files"`
	Bytes  int64      `json:"bytes"`
}

// Reporter is handed to a running job.
type Reporter interface {
	// Progress replaces the job's progress (the manager throttles persistence).
	Progress(p Progress)
	// Log writes a job log line (persisted, secrets redacted). args are slog key/value pairs.
	Log(level slog.Level, msg string, args ...any)
}

// Env is what a runner gets besides the job itself.
type Env struct {
	Reporter Reporter
	Items    ItemStore
}

// Runner executes one type of job. Run must be idempotent and resumable (see the package doc and
// docs/design/phase1.md §5): a job re-run after a crash has Attempt > 1 and Trigger resume, and
// must skip work already done. Returning an error fails the job; returning ctx.Err() after the
// context was cancelled marks it cancelled.
type Runner interface {
	Run(ctx context.Context, job Job, env Env) (Result, error)
}

// RunnerFunc adapts a function to Runner.
type RunnerFunc func(ctx context.Context, job Job, env Env) (Result, error)

// Run implements Runner.
func (f RunnerFunc) Run(ctx context.Context, job Job, env Env) (Result, error) {
	return f(ctx, job, env)
}

// Enqueuer queues jobs. Implemented by the Manager; used by the API, the scheduler and runners
// that start follow-up work.
type Enqueuer interface {
	// Enqueue queues a job. If an identical job (type, params, dry run) is still queued, that job
	// is returned instead of a new one. Targeted jobs (Params.Targeted) that are not dry runs are
	// coalesced with queued, not running, jobs that are not dry runs (phase2-3.md §12.2):
	//   - one that a queued untargeted job of the same type and scope already covers returns that
	//     job. A refresh with SyncAfter is covered only by a queued refresh with SyncAfter, so a
	//     scheduled full refresh waiting for a slot never swallows a webhook's follow-up syncs;
	//   - one whose other params (SyncAfter included) equal a queued targeted job's is merged into
	//     it: paths or item ids united, the queued job keeps its place, and the returned Job is the
	//     merged one;
	//   - a merge beyond MaxTargetPaths / MaxTargetItems makes the queued job untargeted; a
	//     refresh keeps SyncAfter.
	// The returned Job's QueuedAt is earlier than the call when an existing job was returned or
	// merged into (the webhook processor records such events as "coalesced").
	Enqueue(ctx context.Context, spec Spec) (Job, error)
}
