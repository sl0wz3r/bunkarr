package jobqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/logging"
)

// Store persists jobs, job items, job logs and schedules. It implements jobs.ItemStore. Safe for
// concurrent use.
type Store struct {
	db  *db.DB
	now func() time.Time
	// pruneBatch bounds how many rows one PruneHistory transaction deletes.
	pruneBatch int
}

var _ jobs.ItemStore = (*Store)(nil)

// NewStore returns a store over d.
func NewStore(d *db.DB) *Store {
	return &Store{db: d, now: time.Now, pruneBatch: defaultPruneBatch}
}

// jobColumns is the column list scanJob reads, in order.
const jobColumns = `id, type, status, trigger, dry_run, params, attempt, progress, stats, warnings, summary, error, queued_at, started_at, finished_at`

// rowScanner is *sql.Row or *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(sc rowScanner) (jobs.Job, error) {
	var (
		j                          jobs.Job
		typ, status, trigger       string
		params, progress, stats    string
		errText, started, finished sql.NullString
		queued                     string
	)
	if err := sc.Scan(&j.ID, &typ, &status, &trigger, &j.DryRun, &params, &j.Attempt, &progress, &stats,
		&j.Warnings, &j.Summary, &errText, &queued, &started, &finished); err != nil {
		return j, err
	}
	j.Type, j.Status, j.Trigger = jobs.Type(typ), jobs.Status(status), jobs.Trigger(trigger)
	p, err := parseParams(params)
	if err != nil {
		return j, fmt.Errorf("job %d: %w", j.ID, err)
	}
	j.Params = p
	if progress != "" {
		if err := json.Unmarshal([]byte(progress), &j.Progress); err != nil {
			return j, fmt.Errorf("job %d: decode progress: %w", j.ID, err)
		}
	}
	if stats == "" {
		stats = "{}"
	}
	j.Stats = json.RawMessage(stats)
	j.Error = errText.String
	if j.QueuedAt, err = db.ParseTime(queued); err != nil {
		return j, fmt.Errorf("job %d: queued_at: %w", j.ID, err)
	}
	if j.StartedAt, err = parseNullTime(started); err != nil {
		return j, fmt.Errorf("job %d: started_at: %w", j.ID, err)
	}
	if j.FinishedAt, err = parseNullTime(finished); err != nil {
		return j, fmt.Errorf("job %d: finished_at: %w", j.ID, err)
	}
	return j, nil
}

func parseNullTime(s sql.NullString) (*time.Time, error) {
	if !s.Valid {
		return nil, nil
	}
	t, err := db.ParseTime(s.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func nullInt(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: v != 0} }

// CreateJob queues a job for spec and reports whether it inserted a new one. In one transaction
// it:
//   - returns the identical queued job (type, canonical params, dry run) when there is one, except,
//     for a sync, verify or retention spec that is not a dry run, a re-queued job whose plan is
//     already complete (it resumes that plan without scanning again, so what changed since would
//     be lost; see resumesStoredPlan);
//   - otherwise, for a targeted spec that is not a dry run, coalesces it with queued jobs that are
//     not dry runs and never started (phase2-3.md §12.2, see coalesce): it returns a queued
//     untargeted job that covers it, or merges its paths or item ids into a queued targeted job
//     with equal other params (which keeps its place in the queue), or makes that job untargeted
//     when the merge exceeds jobs.MaxTargetPaths or jobs.MaxTargetItems;
//   - otherwise inserts a queued job.
//
// created is false whenever an existing job is returned; its QueuedAt is then earlier than the
// call. Params.DestinationID is also stored in jobs.destination_id and Params.IntegrationID in
// jobs.integration_id.
func (s *Store) CreateJob(ctx context.Context, spec jobs.Spec) (job jobs.Job, created bool, err error) {
	job, outcome, err := s.createJob(ctx, spec)
	return job, outcome == outcomeCreated, err
}

// createJob is CreateJob, reporting what it did.
func (s *Store) createJob(ctx context.Context, spec jobs.Spec) (job jobs.Job, outcome enqueueOutcome, err error) {
	spec, err = normalizeSpec(spec)
	if err != nil {
		return jobs.Job{}, 0, err
	}
	params, err := canonicalParams(spec.Params)
	if err != nil {
		return jobs.Job{}, 0, err
	}
	err = s.db.Write(ctx, func(tx *sql.Tx) error {
		now := s.now()
		// A queued sync, verify or retention job whose plan is already complete (re-queued after a
		// crash or a shutdown) resumes that plan without scanning again, so it would not do what
		// changed since: it is not the answer to an identical request that is not a dry run. Other
		// types that set planned_at (plexdb_backup, arr_backup) never resume a stored plan and keep
		// the exact dedupe.
		skipPlanned := !spec.DryRun && resumesStoredPlan(spec.Type)
		existing, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs
			WHERE status = 'queued' AND type = ? AND params = ? AND dry_run = ? AND (? = 0 OR planned_at IS NULL)
			ORDER BY id LIMIT 1`,
			string(spec.Type), params, spec.DryRun, skipPlanned))
		if err == nil {
			job, outcome = existing, outcomeExisting
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("find queued job: %w", err)
		}
		if !spec.DryRun && spec.Params.Targeted() {
			j, out, err := coalesce(ctx, tx, spec, now)
			if err != nil {
				return err
			}
			if out != outcomeCreated {
				job, outcome = j, out
				faultinject.Point(PointEnqueueBeforeCommit)
				return nil
			}
		}
		job, err = scanJob(tx.QueryRowContext(ctx, `INSERT INTO jobs (type, status, trigger, dry_run, params, destination_id, integration_id, queued_at)
			VALUES (?, 'queued', ?, ?, ?, ?, ?, ?) RETURNING `+jobColumns,
			string(spec.Type), string(spec.Trigger), spec.DryRun, params, nullInt(spec.Params.DestinationID),
			nullInt(spec.Params.IntegrationID), db.FormatTime(now)))
		if err != nil {
			return fmt.Errorf("insert job: %w", err)
		}
		outcome = outcomeCreated
		faultinject.Point(PointEnqueueBeforeCommit)
		return nil
	})
	if err != nil {
		return jobs.Job{}, 0, fmt.Errorf("create %s job: %w", spec.Type, err)
	}
	return job, outcome, nil
}

// GetJob returns job id as stored (without the live progress the Manager merges in).
func (s *Store) GetJob(ctx context.Context, id int64) (jobs.Job, error) {
	j, err := scanJob(s.db.Reader().QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return jobs.Job{}, fmt.Errorf("job %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return jobs.Job{}, fmt.Errorf("read job %d: %w", id, err)
	}
	return j, nil
}

// Job list states.
const (
	// StateActive selects queued and running jobs (oldest first).
	StateActive = "active"
	// StateFinished selects jobs in a final status (most recently finished first).
	StateFinished = "finished"
)

// JobQuery filters ListJobs. Zero values do not filter.
type JobQuery struct {
	// State is "", StateActive or StateFinished.
	State string
	Type  jobs.Type
	// Status selects one status; combined with a State that excludes it, nothing matches.
	Status jobs.Status
	// DestinationID selects the jobs of one destination (jobs.destination_id).
	DestinationID int64
	// IntegrationID selects the jobs of one integration (jobs.integration_id: refresh,
	// plexdb_backup, arr_backup).
	IntegrationID int64
	// Page is 1-based; PageSize defaults to DefaultPageSize and is capped at MaxPageSize.
	Page     int
	PageSize int
}

// ListJobs returns one page of jobs. Active jobs are ordered oldest first (queue order),
// finished jobs most recently finished first, and an unfiltered list newest first.
func (s *Store) ListJobs(ctx context.Context, q JobQuery) (Page[jobs.Job], error) {
	var (
		where []string
		args  []any
		order = "id DESC"
	)
	switch q.State {
	case "":
	case StateActive:
		where = append(where, "status IN ('queued', 'running')")
		order = "queued_at, id"
	case StateFinished:
		where = append(where, "status NOT IN ('queued', 'running')")
		order = "finished_at DESC, id DESC"
	default:
		return Page[jobs.Job]{}, ValidationError(fmt.Sprintf("unknown job state %q (active or finished)", q.State))
	}
	if q.Type != "" {
		if !validType(q.Type) {
			return Page[jobs.Job]{}, ValidationError(fmt.Sprintf("unknown job type %q", q.Type))
		}
		where = append(where, "type = ?")
		args = append(args, string(q.Type))
	}
	if q.Status != "" {
		if !validStatus(q.Status) {
			return Page[jobs.Job]{}, ValidationError(fmt.Sprintf("unknown job status %q", q.Status))
		}
		where = append(where, "status = ?")
		args = append(args, string(q.Status))
	}
	if q.DestinationID != 0 {
		where = append(where, "destination_id = ?")
		args = append(args, q.DestinationID)
	}
	if q.IntegrationID != 0 {
		where = append(where, "integration_id = ?")
		args = append(args, q.IntegrationID)
	}
	cond := ""
	if len(where) > 0 {
		cond = " WHERE " + strings.Join(where, " AND ")
	}
	page, size := normalizePage(q.Page, q.PageSize)
	out := Page[jobs.Job]{Page: page, PageSize: size, Records: []jobs.Job{}}
	err := s.readTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs`+cond, args...).Scan(&out.TotalRecords); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs`+cond+` ORDER BY `+order+` LIMIT ? OFFSET ?`,
			append(args, size, (page-1)*size)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			j, err := scanJob(rows)
			if err != nil {
				return err
			}
			out.Records = append(out.Records, j)
		}
		return rows.Err()
	})
	if err != nil {
		return Page[jobs.Job]{}, fmt.Errorf("list jobs: %w", err)
	}
	return out, nil
}

// ActiveForSource reports whether a queued or running job (dry runs included) lists source id in
// its params (sourceIds), for the source delete guard.
func (s *Store) ActiveForSource(ctx context.Context, id int64) (bool, error) {
	return s.activeFor(ctx, "source", `SELECT EXISTS (SELECT 1 FROM jobs, json_each(jobs.params, '$.sourceIds') AS src
		WHERE jobs.status IN ('queued', 'running') AND src.value = ?)`, id)
}

// ActiveForIntegration reports whether a queued or running job (dry runs included) has
// integration id in its params (integrationId, denormalized into jobs.integration_id), for the
// integration delete guard.
func (s *Store) ActiveForIntegration(ctx context.Context, id int64) (bool, error) {
	return s.activeFor(ctx, "integration", `SELECT EXISTS (SELECT 1 FROM jobs
		WHERE status IN ('queued', 'running') AND integration_id = ?)`, id)
}

// ActiveForDestination reports whether a queued or running job (dry runs included) works on
// destination id (sync, verify, retention, manifest_export, or a Plex database or *arr backup to
// it).
func (s *Store) ActiveForDestination(ctx context.Context, id int64) (bool, error) {
	return s.activeFor(ctx, "destination", `SELECT EXISTS (SELECT 1 FROM jobs
		WHERE status IN ('queued', 'running') AND destination_id = ?)`, id)
}

func (s *Store) activeFor(ctx context.Context, what, q string, id int64) (bool, error) {
	var active bool
	if err := s.db.Reader().QueryRowContext(ctx, q, id).Scan(&active); err != nil {
		return false, fmt.Errorf("check active jobs of %s %d: %w", what, id, err)
	}
	return active, nil
}

// readTx runs fn in a read-only transaction, so multi-query reads see one snapshot.
func (s *Store) readTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.Reader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	return fn(tx)
}

// jobExists reports whether job id exists, inside tx.
func jobExists(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, id int64) (bool, error) {
	var ok bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM jobs WHERE id = ?)`, id).Scan(&ok)
	return ok, err
}

// defaultPruneBatch bounds how many rows one PruneHistory transaction deletes, so running jobs'
// writes never wait long behind it.
const defaultPruneBatch = 5000

// PruneHistory deletes finished jobs whose finished_at is before cutoff, with their items and
// logs, and returns the number of jobs deleted. Items and logs are deleted first in bounded
// batches (each its own transaction); queued and running jobs are never touched.
func (s *Store) PruneHistory(ctx context.Context, cutoff time.Time) (int64, error) {
	before := db.FormatTime(cutoff)
	const old = `SELECT id FROM jobs WHERE status NOT IN ('queued', 'running') AND finished_at IS NOT NULL AND finished_at < ?`
	for _, table := range []string{"job_items", "job_logs"} {
		stmt := `DELETE FROM ` + table + ` WHERE id IN (SELECT id FROM ` + table + ` WHERE job_id IN (` + old + `) LIMIT ?)`
		for {
			var n int64
			err := s.db.Write(ctx, func(tx *sql.Tx) error {
				res, err := tx.ExecContext(ctx, stmt, before, s.pruneBatch)
				if err != nil {
					return err
				}
				n, err = res.RowsAffected()
				return err
			})
			if err != nil {
				return 0, fmt.Errorf("prune %s: %w", table, err)
			}
			if n < int64(s.pruneBatch) {
				break
			}
		}
	}
	var total int64
	for {
		var n int64
		err := s.db.Write(ctx, func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx, `DELETE FROM jobs WHERE id IN (`+old+` LIMIT ?)`, before, s.pruneBatch)
			if err != nil {
				return err
			}
			n, err = res.RowsAffected()
			return err
		})
		if err != nil {
			return total, fmt.Errorf("prune jobs: %w", err)
		}
		total += n
		if n < int64(s.pruneBatch) {
			return total, nil
		}
	}
}

// --- Manager-internal state transitions. Each is conditional on the current status, so a late
// writer (a straggling runner after Stop gave up on it) can never overwrite a newer state.

// queuedJobs returns queued jobs in queue order (oldest first).
func (s *Store) queuedJobs(ctx context.Context, limit int) ([]jobs.Job, error) {
	rows, err := s.db.Reader().QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE status = 'queued' ORDER BY queued_at, id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list queued jobs: %w", err)
	}
	defer rows.Close()
	var out []jobs.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("list queued jobs: %w", err)
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list queued jobs: %w", err)
	}
	return out, nil
}

// markRunning moves a queued job to running. ok is false when the job is no longer queued (it
// was cancelled in the meantime).
func (s *Store) markRunning(ctx context.Context, id int64, now time.Time) (job jobs.Job, ok bool, err error) {
	at := db.FormatTime(now)
	err = s.db.Write(ctx, func(tx *sql.Tx) error {
		job, err = scanJob(tx.QueryRowContext(ctx, `UPDATE jobs SET status = 'running', started_at = COALESCE(started_at, ?), heartbeat_at = ?
			WHERE id = ? AND status = 'queued' RETURNING `+jobColumns, at, at, id))
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return jobs.Job{}, false, nil
	}
	if err != nil {
		return jobs.Job{}, false, fmt.Errorf("start job %d: %w", id, err)
	}
	return job, true, nil
}

// saveProgress persists a running job's progress and heartbeat.
func (s *Store) saveProgress(ctx context.Context, id int64, progress []byte, now time.Time) error {
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE jobs SET progress = ?, heartbeat_at = ? WHERE id = ? AND status = 'running'`,
			string(progress), db.FormatTime(now), id)
		if err != nil {
			return fmt.Errorf("save progress of job %d: %w", id, err)
		}
		return nil
	})
}

// finalState is what finishJob writes.
type finalState struct {
	status   jobs.Status
	stats    string
	warnings int
	summary  string
	errText  string
	progress []byte
	at       time.Time
}

// finishJob records a running job's final state. ok is false when the job was no longer running.
func (s *Store) finishJob(ctx context.Context, id int64, f finalState) (ok bool, err error) {
	err = s.db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE jobs SET status = ?, stats = ?, warnings = ?, summary = ?, error = ?, progress = ?,
			finished_at = ?, heartbeat_at = NULL WHERE id = ? AND status = 'running'`,
			string(f.status), f.stats, f.warnings, f.summary, nullString(f.errText), string(f.progress), db.FormatTime(f.at), id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		ok = n == 1
		return err
	})
	if err != nil {
		return false, fmt.Errorf("finish job %d: %w", id, err)
	}
	return ok, nil
}

// requeue puts a running job interrupted by a graceful shutdown back in the queue with trigger
// resume, without incrementing attempt.
func (s *Store) requeue(ctx context.Context, id int64, progress []byte) (ok bool, err error) {
	err = s.db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE jobs SET status = 'queued', trigger = 'resume', progress = ?, heartbeat_at = NULL
			WHERE id = ? AND status = 'running'`, string(progress), id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		ok = n == 1
		return err
	})
	if err != nil {
		return false, fmt.Errorf("re-queue job %d: %w", id, err)
	}
	return ok, nil
}

// cancelQueued cancels a job that has not started. ok is false when it was not queued.
func (s *Store) cancelQueued(ctx context.Context, id int64, now time.Time) (ok bool, err error) {
	return s.finishQueued(ctx, id, jobs.StatusCancelled, "", "Cancelled before it started.", now)
}

// finishQueued moves a queued job straight to a final status.
func (s *Store) finishQueued(ctx context.Context, id int64, status jobs.Status, errText, summary string, now time.Time) (ok bool, err error) {
	err = s.db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE jobs SET status = ?, error = ?, summary = ?, finished_at = ?
			WHERE id = ? AND status = 'queued'`, string(status), nullString(errText), summary, db.FormatTime(now), id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		ok = n == 1
		return err
	})
	if err != nil {
		return false, fmt.Errorf("finish queued job %d: %w", id, err)
	}
	return ok, nil
}

// recoverCrashed handles jobs a previous process left running (it crashed or was killed): each
// goes back to the queue with trigger resume and attempt+1, or, when that exceeds maxAttempts,
// fails with "crashed N times". One transaction.
func (s *Store) recoverCrashed(ctx context.Context, maxAttempts int, now time.Time) (requeued, failed []int64, err error) {
	err = s.db.Write(ctx, func(tx *sql.Tx) error {
		requeued, failed = nil, nil
		rows, err := tx.QueryContext(ctx, `UPDATE jobs SET status = 'failed', error = 'crashed ' || attempt || CASE attempt WHEN 1 THEN ' time' ELSE ' times' END,
			finished_at = ?, heartbeat_at = NULL WHERE status = 'running' AND attempt + 1 > ? RETURNING id`, db.FormatTime(now), maxAttempts)
		if err != nil {
			return err
		}
		if failed, err = scanIDs(rows); err != nil {
			return err
		}
		rows, err = tx.QueryContext(ctx, `UPDATE jobs SET status = 'queued', trigger = 'resume', attempt = attempt + 1, heartbeat_at = NULL
			WHERE status = 'running' RETURNING id`)
		if err != nil {
			return err
		}
		requeued, err = scanIDs(rows)
		return err
	})
	if err != nil {
		return nil, nil, fmt.Errorf("recover interrupted jobs: %w", err)
	}
	return requeued, failed, nil
}

func scanIDs(rows *sql.Rows) ([]int64, error) {
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// hasRunning reports whether a non-dry-run job of type t for the same work as p is running
// (the scheduler's skip rule, phase1.md §6.2 as amended by phase2-3.md §12.1): for a sync or
// verify, one of the same destination that runs without paths and covers p's sources (it runs
// for all sources, or for every source p names), so a running targeted (webhook) sync, or an
// untargeted follow-up sync of one source, never makes a scheduled full sync skip its turn (the
// full sync is queued and waits for dest:<id>); otherwise one with the same canonical params, so
// a Plex DB backup is never skipped for another server's backup to the same destination, and a
// running targeted refresh never skips a scheduled full one.
func (s *Store) hasRunning(ctx context.Context, t jobs.Type, p jobs.Params) (bool, error) {
	if (t == jobs.TypeSync || t == jobs.TypeVerify) && p.DestinationID != 0 {
		return s.hasRunningCovering(ctx, t, p)
	}
	params, err := canonicalParams(p)
	if err != nil {
		return false, err
	}
	var running bool
	if err := s.db.Reader().QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM jobs
		WHERE status = 'running' AND dry_run = 0 AND type = ? AND params = ?)`, string(t), params).Scan(&running); err != nil {
		return false, fmt.Errorf("check running %s jobs: %w", t, err)
	}
	return running, nil
}

// hasRunningCovering is hasRunning for a sync or verify of a destination: whether a non-dry-run
// job of type t and p's destination runs without paths for sources that include all of p's.
func (s *Store) hasRunningCovering(ctx context.Context, t jobs.Type, p jobs.Params) (bool, error) {
	rows, err := s.db.Reader().QueryContext(ctx, `SELECT params FROM jobs WHERE status = 'running' AND dry_run = 0 AND type = ?
		AND destination_id = ? AND json_extract(params, '$.paths') IS NULL`, string(t), p.DestinationID)
	if err != nil {
		return false, fmt.Errorf("check running %s jobs: %w", t, err)
	}
	defer rows.Close()
	want := sortedIDs(p.SourceIDs)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return false, fmt.Errorf("check running %s jobs: %w", t, err)
		}
		rp, err := parseParams(raw)
		if err != nil {
			return false, fmt.Errorf("check running %s jobs: %w", t, err)
		}
		if coversSources(rp.SourceIDs, want) {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("check running %s jobs: %w", t, err)
	}
	return false, nil
}

// coversSources reports whether a job for the sources have (empty: all sources) does every
// source of want (empty: all sources).
func coversSources(have, want []int64) bool {
	if len(have) == 0 {
		return true
	}
	if len(want) == 0 {
		return false
	}
	for _, id := range want {
		if !slices.Contains(have, id) {
			return false
		}
	}
	return true
}

// redactText is logging.RedactSecrets; named for readability at the call sites.
func redactText(s string) string { return logging.RedactSecrets(s) }
