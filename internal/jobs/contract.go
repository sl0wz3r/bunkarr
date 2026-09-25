// Package jobs runs Bunkarr's background work: a persistent, resumable queue with worker limits,
// cancellation and progress, plus the cron scheduler. This file is the contract between the
// manager and the job runners (internal/syncer, internal/plexdb, ...); see docs/design/phase1.md §5.
package jobs

import (
	"context"
	"encoding/json"
	"log/slog"
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
	ActionCopy   ItemAction = "copy"   // new file at the destination
	ActionUpdate ItemAction = "update" // changed file: copy new, retain old (safety rule S6)
	ActionAdopt  ItemAction = "adopt"  // already at the destination with matching size+mtime
	ActionLink   ItemAction = "link"   // another name of an inode already copied (hardlink)
	ActionRetain ItemAction = "retain" // gone from the source: move into retention (S5)
	ActionExpire ItemAction = "expire" // retention period over: delete the retained copy
	ActionVerify ItemAction = "verify" // re-read and compare a destination file
	ActionBackup ItemAction = "backup" // a Plex DB backup file
	ActionSkip   ItemAction = "skip"   // recorded for the preview, nothing to do
)

// ItemStatus is an item's state.
type ItemStatus string

// Item statuses.
const (
	ItemPending ItemStatus = "pending"
	ItemDone    ItemStatus = "done"
	ItemFailed  ItemStatus = "failed"
	ItemSkipped ItemStatus = "skipped"
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

// ItemStore persists a job's items. Implemented by the manager's store.
type ItemStore interface {
	// HasItems reports whether the job already has a persisted plan (a resumed job).
	HasItems(ctx context.Context, jobID int64) (bool, error)
	// AddItems appends items (Status is normally ItemPending). Batches of any size.
	AddItems(ctx context.Context, jobID int64, items []Item) error
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
	// is returned instead of a new one.
	Enqueue(ctx context.Context, spec Spec) (Job, error)
}
