package jobqueue

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// PointEnqueueBeforeCommit is reached inside CreateJob's transaction, after the new job was
// inserted or a queued job was changed by a merge, before the commit: a crash there leaves the
// queue as it was (crash-matrix tests).
const PointEnqueueBeforeCommit = "jobqueue.enqueueBeforeCommit"

// enqueueOutcome says what CreateJob did.
type enqueueOutcome int

const (
	// outcomeCreated: a new job was inserted.
	outcomeCreated enqueueOutcome = iota + 1
	// outcomeExisting: an identical queued job was returned (phase1.md §6.2 dedupe).
	outcomeExisting
	// outcomeCovered: a queued job already covers the targeted spec and was returned unchanged.
	outcomeCovered
	// outcomeMerged: the spec's paths or item ids were merged into a queued targeted job.
	outcomeMerged
	// outcomeOverflow: the merge would have exceeded the target limit, so the queued job became
	// untargeted (a refresh keeps syncAfter).
	outcomeOverflow
)

// String names the outcome for logs.
func (o enqueueOutcome) String() string {
	switch o {
	case outcomeCreated:
		return "created"
	case outcomeExisting:
		return "existing"
	case outcomeCovered:
		return "covered"
	case outcomeMerged:
		return "merged"
	case outcomeOverflow:
		return "overflow"
	}
	return "unknown"
}

// coalesce applies phase2-3.md §12.2 to a targeted spec (Params.Targeted) that is not a dry run
// and has no identical queued job, inside CreateJob's write transaction. Only queued jobs that are
// not dry runs and have never started are considered: a running job is never merged into, and a
// job re-queued after a crash or a shutdown may already have its plan, which a merge would not
// change. In queue order it looks for:
//
//  1. a queued untargeted job of the same type and scope that covers the spec (coversSpec): it is
//     returned unchanged (outcomeCovered);
//  2. a queued targeted job of the same type whose other params are equal: the spec's paths or
//     item ids are merged into it (outcomeMerged; the job keeps its queued_at, so its place), or,
//     when the union exceeds jobs.MaxTargetPaths or jobs.MaxTargetItems, it becomes untargeted,
//     keeping its other params, SyncAfter included, and says so in its job log (outcomeOverflow).
//
// It returns outcomeCreated when neither exists: the caller inserts a new job.
func coalesce(ctx context.Context, tx *sql.Tx, spec jobs.Spec, now time.Time) (jobs.Job, enqueueOutcome, error) {
	var (
		cond string
		arg  int64
	)
	switch spec.Type {
	case jobs.TypeSync:
		cond, arg = `destination_id = ?`, spec.Params.DestinationID
	case jobs.TypeRefresh:
		cond, arg = `integration_id = ?`, spec.Params.IntegrationID
	default:
		return jobs.Job{}, outcomeCreated, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs
		WHERE status = 'queued' AND started_at IS NULL AND dry_run = 0 AND type = ? AND `+cond+`
		ORDER BY queued_at, id`, string(spec.Type), arg)
	if err != nil {
		return jobs.Job{}, 0, fmt.Errorf("find queued %s jobs to coalesce with: %w", spec.Type, err)
	}
	var candidates []jobs.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			_ = rows.Close()
			return jobs.Job{}, 0, fmt.Errorf("find queued %s jobs to coalesce with: %w", spec.Type, err)
		}
		candidates = append(candidates, j)
	}
	if err := rows.Close(); err != nil {
		return jobs.Job{}, 0, err
	}
	if err := rows.Err(); err != nil {
		return jobs.Job{}, 0, err
	}
	for _, c := range candidates {
		if coversSpec(c.Params, spec) {
			return c, outcomeCovered, nil
		}
	}
	rest, err := canonicalParams(withoutTargets(spec.Params))
	if err != nil {
		return jobs.Job{}, 0, err
	}
	for _, c := range candidates {
		if !c.Params.Targeted() {
			continue
		}
		other, err := canonicalParams(withoutTargets(c.Params))
		if err != nil {
			return jobs.Job{}, 0, err
		}
		if other == rest {
			return mergeInto(ctx, tx, c, spec, now)
		}
	}
	return jobs.Job{}, outcomeCreated, nil
}

// coversSpec reports whether a queued job with params q already does all a targeted spec asks
// for (phase2-3.md §12.2):
//   - q is untargeted;
//   - a sync: q's sources are all sources, or include the spec's one source;
//   - a refresh: q has SyncAfter when the spec does, so a scheduled full refresh waiting for a
//     slot never swallows a webhook's follow-up syncs (a full refresh without SyncAfter queues
//     syncs only for what it finds changed, which does not include an item the index already
//     had in its new state);
//   - q has AllowChanges when the spec does, so a request to apply held changes is never answered
//     with a job that holds them again.
func coversSpec(q jobs.Params, spec jobs.Spec) bool {
	if q.Targeted() || (spec.Params.AllowChanges && !q.AllowChanges) {
		return false
	}
	switch spec.Type {
	case jobs.TypeSync:
		return len(q.SourceIDs) == 0 || (len(spec.Params.SourceIDs) == 1 && slices.Contains(q.SourceIDs, spec.Params.SourceIDs[0]))
	case jobs.TypeRefresh:
		return !spec.Params.SyncAfter || q.SyncAfter
	}
	return false
}

// withoutTargets returns p without its paths and item ids: the "other params" two targeted jobs
// must share to be merged.
func withoutTargets(p jobs.Params) jobs.Params {
	p.Paths, p.ArrItemIDs = nil, nil
	return p
}

// mergeInto merges spec's targets into the queued targeted job q (see coalesce).
func mergeInto(ctx context.Context, tx *sql.Tx, q jobs.Job, spec jobs.Spec, now time.Time) (jobs.Job, enqueueOutcome, error) {
	p := q.Params
	outcome := outcomeMerged
	var overflowMsg string
	switch spec.Type {
	case jobs.TypeSync:
		p.Paths = canonicalPaths(append(slices.Clone(p.Paths), spec.Params.Paths...))
		if len(p.Paths) > jobs.MaxTargetPaths {
			p.Paths, outcome = nil, outcomeOverflow
			overflowMsg = fmt.Sprintf("More paths were merged into this queued sync than one sync takes (%d): it syncs its whole source instead",
				jobs.MaxTargetPaths)
		}
	case jobs.TypeRefresh:
		p.ArrItemIDs = sortedIDs(append(slices.Clone(p.ArrItemIDs), spec.Params.ArrItemIDs...))
		if len(p.ArrItemIDs) > jobs.MaxTargetItems {
			p.ArrItemIDs, outcome = nil, outcomeOverflow
			overflowMsg = fmt.Sprintf("More items were merged into this queued refresh than one refresh takes (%d): it refreshes the whole integration instead",
				jobs.MaxTargetItems)
			if p.SyncAfter {
				overflowMsg += ", then queues a sync of every source the integration's root folders are in"
			}
		}
	}
	oldParams, err := canonicalParams(q.Params)
	if err != nil {
		return jobs.Job{}, 0, err
	}
	params, err := canonicalParams(p)
	if err != nil {
		return jobs.Job{}, 0, err
	}
	if params == oldParams {
		return q, outcomeCovered, nil // the spec's targets were all in the job already
	}
	merged, err := scanJob(tx.QueryRowContext(ctx, `UPDATE jobs SET params = ? WHERE id = ? AND status = 'queued'
		RETURNING `+jobColumns, params, q.ID))
	if err != nil {
		return jobs.Job{}, 0, fmt.Errorf("merge into job %d: %w", q.ID, err)
	}
	if outcome == outcomeOverflow {
		if _, err := tx.ExecContext(ctx, `INSERT INTO job_logs (job_id, at, level, message, fields) VALUES (?, ?, 'info', ?, '{}')`,
			q.ID, db.FormatTime(now), overflowMsg+"."); err != nil {
			return jobs.Job{}, 0, fmt.Errorf("log the merge into job %d: %w", q.ID, err)
		}
	}
	return merged, outcome, nil
}
