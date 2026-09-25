package syncer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"slices"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// RetentionOptions configures the retention runner.
type RetentionOptions struct {
	Options
	// Enqueuer queues the per-destination retention jobs of a global retention job.
	Enqueuer jobs.Enqueuer
	// PruneHistory deletes finished jobs older than cutoff (jobqueue.Store.PruneHistory); nil
	// skips pruning.
	PruneHistory func(ctx context.Context, cutoff time.Time) (int64, error)
	// HistoryDays returns how many days of finished jobs to keep (design §6.2 "90 days
	// (setting)"); nil or a value < 1 means 90.
	HistoryDays func(ctx context.Context) (int, error)
}

// RetentionRunner runs retention jobs (jobs.TypeRetention). Without a destination (the seeded
// daily schedule) it queues one retention job per enabled destination and prunes the job
// history. With a destination it expires the retained files whose expires_at has passed (design
// §4.2 expire): only inside .bunkarr/retention, only when the file still has the recorded size,
// never content a live record still links to; then the row is deleted and empty retention
// directories are pruned.
type RetentionRunner struct {
	base
	enq          jobs.Enqueuer
	prune        func(ctx context.Context, cutoff time.Time) (int64, error)
	historyDays  func(ctx context.Context) (int, error)
	planBatch    int
	recheckEvery int
}

// NewRetentionRunner returns the retention job runner.
func NewRetentionRunner(o RetentionOptions) *RetentionRunner {
	return &RetentionRunner{base: newBase(o.Options), enq: o.Enqueuer, prune: o.PruneHistory, historyDays: o.HistoryDays,
		planBatch: planBatch, recheckEvery: recheckEvery}
}

var _ jobs.Runner = (*RetentionRunner)(nil)

// RetentionStats is a retention job's stats JSON.
type RetentionStats struct {
	DryRun bool `json:"dryRun"`
	// JobsQueued and JobsPruned are set by the global job.
	JobsQueued int64 `json:"jobsQueued"`
	JobsPruned int64 `json:"jobsPruned"`
	// Files* and BytesExpired are set by a destination's job.
	FilesPlanned int64 `json:"filesPlanned"`
	FilesExpired int64 `json:"filesExpired"`
	// FilesKept were not expired: still referenced by a live record, or not the recorded file.
	FilesKept    int64 `json:"filesKept"`
	FilesFailed  int64 `json:"filesFailed"`
	BytesExpired int64 `json:"bytesExpired"`
	DurationMs   int64 `json:"durationMs"`
}

// Run implements jobs.Runner.
func (r *RetentionRunner) Run(ctx context.Context, job jobs.Job, env jobs.Env) (jobs.Result, error) {
	started := r.now()
	if job.Params.DestinationID == 0 {
		return r.runGlobal(ctx, job, env, started)
	}
	if env.Items == nil {
		return jobs.Result{}, errors.New("retention: the job has no item store")
	}
	h, err := openEnabled(ctx, r.dests, job.Params.DestinationID, "retention")
	if err != nil {
		return jobs.Result{}, err
	}
	defer h.Close()
	rr := &retentionRun{r: r, job: job, env: env, rep: reporterOf(env), h: h}
	rr.rep.Log(slog.LevelInfo, "retention started", "destination", h.Destination.Name, "deletedDays", h.Retention.DeletedDays)
	if err := prepareIdentity(ctx, h, rr.rep, job.DryRun, &rr.unsettled); err != nil {
		return jobs.Result{}, err
	}
	if !job.DryRun {
		// What a stopped sync left in retention is settled (recorded, or put back) first.
		n, err := reconcile(ctx, newIntentFixer(r.base, h, rr.rep, job.ID), env.Items, "")
		rr.unsettled += n
		if err != nil {
			if ctx.Err() != nil {
				return jobs.Result{}, ctx.Err()
			}
			return jobs.Result{}, fmt.Errorf("retention: %w", err)
		}
	}
	planned, err := env.Items.Planned(ctx, job.ID)
	if err != nil {
		return jobs.Result{}, err
	}
	if !planned {
		if err := env.Items.DeleteItems(ctx, job.ID); err != nil {
			return jobs.Result{}, err
		}
		if err := rr.plan(ctx, started); err != nil {
			return jobs.Result{}, err
		}
	}
	if !job.DryRun {
		if err := rr.execute(ctx); err != nil {
			if ctx.Err() != nil {
				return jobs.Result{}, ctx.Err()
			}
			return jobs.Result{}, err
		}
	}
	return rr.result(ctx, started)
}

// runGlobal queues a retention job for every enabled destination and prunes the job history.
func (r *RetentionRunner) runGlobal(ctx context.Context, job jobs.Job, env jobs.Env, started time.Time) (jobs.Result, error) {
	rep := reporterOf(env)
	st := RetentionStats{DryRun: job.DryRun}
	if r.enq == nil {
		return jobs.Result{}, errors.New("retention: no job queue to start the destinations' retention jobs")
	}
	list, err := r.dests.List(ctx)
	if err != nil {
		return jobs.Result{}, err
	}
	trigger := job.Trigger
	if trigger == jobs.TriggerResume || trigger == "" {
		trigger = jobs.TriggerSchedule
	}
	for _, d := range list {
		if !d.Enabled {
			continue
		}
		q, err := r.enq.Enqueue(ctx, jobs.Spec{Type: jobs.TypeRetention, Trigger: trigger, DryRun: job.DryRun,
			Params: jobs.Params{DestinationID: d.ID}})
		if err != nil {
			return jobs.Result{}, fmt.Errorf("queue retention of destination %q: %w", d.Name, err)
		}
		st.JobsQueued++
		rep.Log(slog.LevelInfo, "retention queued", "destination", d.Name, "jobId", q.ID)
	}
	if r.prune != nil && !job.DryRun {
		days := defaultHistoryDays
		if r.historyDays != nil {
			n, err := r.historyDays(ctx)
			if err != nil {
				return jobs.Result{}, fmt.Errorf("read the job history setting: %w", err)
			}
			if n >= 1 {
				days = n
			}
		}
		cutoff := r.now().Add(-time.Duration(days) * 24 * time.Hour)
		n, err := r.prune(ctx, cutoff)
		if err != nil {
			return jobs.Result{}, fmt.Errorf("prune job history: %w", err)
		}
		st.JobsPruned = n
		rep.Log(slog.LevelInfo, "job history pruned", "jobs", n, "olderThanDays", days)
	}
	st.DurationMs = r.now().Sub(started).Milliseconds()
	summary := fmt.Sprintf("Queued retention for %s; pruned %s from the history",
		plural(st.JobsQueued, "destination", "destinations"), plural(st.JobsPruned, "old job", "old jobs"))
	return jobs.Result{Stats: st, Summary: summary}, nil
}

type retentionRun struct {
	r   *RetentionRunner
	job jobs.Job
	env jobs.Env
	rep jobs.Reporter
	h   *destinations.Handle

	// unsettled counts what a stopped sync left in retention that could not be settled (reconcile),
	// and a failed check of the destination's inode numbers (prepareIdentity).
	unsettled int
	progress  jobs.Progress
}

// linked reports whether a source is linked to the destination (orphan rows are never expired).
func (rr *retentionRun) linked(sourceID int64) bool {
	return sourceID != 0 && slices.Contains(rr.h.Destination.SourceIDs, sourceID)
}

// plan persists one expire item per retained row whose expiry has passed. Rows of sources that are
// no longer linked to the destination (or were deleted) are orphans and are kept.
func (rr *retentionRun) plan(ctx context.Context, now time.Time) error {
	all, err := rr.r.store.Expired(ctx, rr.h.Destination.ID, now)
	if err != nil {
		return err
	}
	var recs []Record
	for _, rec := range all {
		if rr.linked(rec.SourceID) {
			recs = append(recs, rec)
		}
	}
	if n := len(all) - len(recs); n > 0 {
		rr.rep.Log(slog.LevelInfo, "expired retained files of sources no longer linked to this destination are kept", "files", n)
	}
	var batch []jobs.Item
	for i, rec := range recs {
		d := Detail{SourceID: rec.SourceID, Source: rec.SourceRelPath, RecordID: rec.ID, Size: rec.Size, RetainedPath: rec.RetainedPath,
			Reason: rec.Reason}
		batch = append(batch, jobs.Item{FileID: rec.ID, RelPath: rec.RetainedPath, Action: jobs.ActionExpire, Bytes: rec.Size, Detail: d.raw()})
		if len(batch) >= rr.r.planBatch && i < len(recs)-1 {
			if err := rr.env.Items.AddItems(ctx, rr.job.ID, batch, false); err != nil {
				return err
			}
			batch = batch[:0]
			faultinject.Point(PointPlanAfterBatch)
		}
	}
	return rr.env.Items.AddItems(ctx, rr.job.ID, batch, true)
}

// execute expires the pending items, re-checking the marker first (S3).
func (rr *retentionRun) execute(ctx context.Context) error {
	if err := rr.h.Recheck(); err != nil {
		return err
	}
	rr.progress = jobs.Progress{Phase: "expiring"}
	sinceRecheck := 0
	after := int64(0)
	for {
		batch, err := rr.env.Items.Pending(ctx, rr.job.ID, after, pendingBatch)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		for _, it := range batch {
			after = it.ID
			if err := ctx.Err(); err != nil {
				return err
			}
			if it.Action != jobs.ActionExpire {
				continue
			}
			if err := rr.expire(ctx, it); err != nil {
				return err
			}
			rr.progress.FilesDone++
			rr.progress.CurrentFile = it.RelPath
			rr.rep.Progress(rr.progress)
			if sinceRecheck++; sinceRecheck >= rr.r.recheckEvery {
				sinceRecheck = 0
				if err := rr.h.Recheck(); err != nil {
					return err
				}
			}
		}
	}
}

func (rr *retentionRun) expire(ctx context.Context, it jobs.Item) error {
	wctx := context.WithoutCancel(ctx)
	finish := func(status jobs.ItemStatus, bytes int64, msg string) error {
		return rr.env.Items.Finish(wctx, it.ID, status, bytes, msg)
	}
	d, err := parseDetail(it)
	if err != nil {
		return finish(jobs.ItemFailed, 0, err.Error())
	}
	rec, err := rr.r.store.Get(ctx, d.RecordID)
	if errors.Is(err, ErrNotFound) {
		return finish(jobs.ItemDone, 0, "") // expired by an earlier attempt
	}
	if err != nil {
		return err
	}
	if rec.State != StateRetained || rec.RetainedPath != d.RetainedPath || rec.DestinationID != rr.h.Destination.ID {
		return finish(jobs.ItemSkipped, 0, "the record changed since planning")
	}
	if rec.ExpiresAt == nil || rec.ExpiresAt.After(rr.r.now()) {
		return finish(jobs.ItemSkipped, 0, "not expired")
	}
	if !rr.linked(rec.SourceID) {
		return finish(jobs.ItemSkipped, 0, "its source is no longer linked to this destination; kept")
	}
	deps, err := rr.r.store.Dependents(ctx, rec.ID)
	if err != nil {
		return err
	}
	if len(deps) > 0 {
		rr.rep.Log(slog.LevelWarn, "retained content is still referenced by a live record; kept", "path", rec.RetainedPath, "referencedBy", deps[0].RelPath)
		return finish(jobs.ItemSkipped, 0, fmt.Sprintf("still referenced by %s; kept", deps[0].RelPath))
	}
	err = filecopy.Expire(rr.h.Root, rec.RetainedPath, rec.Size)
	switch {
	case err == nil, errors.Is(err, fs.ErrNotExist):
	case errors.Is(err, filecopy.ErrMismatch), errors.Is(err, filecopy.ErrOutsideRetention), errors.Is(err, filecopy.ErrNotRegular),
		errors.Is(err, filecopy.ErrInvalidPath):
		rr.rep.Log(slog.LevelWarn, "retained file not expired", "path", rec.RetainedPath, "error", err.Error())
		return finish(jobs.ItemFailed, 0, err.Error()+"; kept")
	default:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if filecopy.Classify(err) == filecopy.Fatal {
			return fmt.Errorf("expire %s: %w", rec.RetainedPath, err)
		}
		rr.rep.Log(slog.LevelWarn, "retained file not expired", "path", rec.RetainedPath, "error", err.Error())
		return finish(jobs.ItemFailed, 0, err.Error())
	}
	faultinject.Point(PointRecordAfterFS)
	if err := rr.r.store.db.Write(wctx, func(tx *sql.Tx) error { return deleteRecord(wctx, tx, rec.ID) }); err != nil {
		return err
	}
	faultinject.Point(PointRecordAfterDB)
	return finish(jobs.ItemDone, rec.Size, "")
}

func (rr *retentionRun) result(ctx context.Context, started time.Time) (jobs.Result, error) {
	counts, err := rr.env.Items.Counts(context.WithoutCancel(ctx), rr.job.ID)
	if err != nil {
		return jobs.Result{}, err
	}
	st := RetentionStats{DryRun: rr.job.DryRun}
	for _, c := range counts {
		st.FilesPlanned += c.Files
		switch c.Status {
		case jobs.ItemDone:
			st.FilesExpired += c.Files
			st.BytesExpired += c.Bytes
		case jobs.ItemFailed:
			st.FilesFailed += c.Files
		case jobs.ItemSkipped:
			st.FilesKept += c.Files
		}
	}
	st.DurationMs = rr.r.now().Sub(started).Milliseconds()
	var summary string
	if rr.job.DryRun {
		summary = fmt.Sprintf("Dry run: would expire %s", plural(st.FilesPlanned, "retained file", "retained files"))
	} else {
		summary = fmt.Sprintf("Expired %s (%s)", plural(st.FilesExpired, "retained file", "retained files"), formatBytes(st.BytesExpired))
		if st.FilesKept+st.FilesFailed > 0 {
			summary += fmt.Sprintf("; %d kept", st.FilesKept+st.FilesFailed)
		}
	}
	return jobs.Result{Stats: st, Warnings: int(st.FilesFailed+st.FilesKept) + rr.unsettled, Summary: summary}, nil
}
