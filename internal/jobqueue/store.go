package jobqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
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

// CreateJob inserts a queued job for spec. When an identical job (type, canonical params, dry
// run) is still queued, that job is returned instead and created is false. The check and the
// insert are one transaction. Params.DestinationID is also stored in jobs.destination_id.
func (s *Store) CreateJob(ctx context.Context, spec jobs.Spec) (job jobs.Job, created bool, err error) {
	spec, err = normalizeSpec(spec)
	if err != nil {
		return jobs.Job{}, false, err
	}
	params, err := canonicalParams(spec.Params)
	if err != nil {
		return jobs.Job{}, false, err
	}
	err = s.db.Write(ctx, func(tx *sql.Tx) error {
		created = false
		existing, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs
			WHERE status = 'queued' AND type = ? AND params = ? AND dry_run = ? ORDER BY id LIMIT 1`,
			string(spec.Type), params, spec.DryRun))
		if err == nil {
			job = existing
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("find queued job: %w", err)
		}
		job, err = scanJob(tx.QueryRowContext(ctx, `INSERT INTO jobs (type, status, trigger, dry_run, params, destination_id, queued_at)
			VALUES (?, 'queued', ?, ?, ?, ?, ?) RETURNING `+jobColumns,
			string(spec.Type), string(spec.Trigger), spec.DryRun, params, nullInt(spec.Params.DestinationID), db.FormatTime(s.now())))
		if err != nil {
			return fmt.Errorf("insert job: %w", err)
		}
		created = true
		return nil
	})
	if err != nil {
		return jobs.Job{}, false, fmt.Errorf("create %s job: %w", spec.Type, err)
	}
	return job, created, nil
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
// integration id in its params (integrationId), for the integration delete guard.
func (s *Store) ActiveForIntegration(ctx context.Context, id int64) (bool, error) {
	return s.activeFor(ctx, "integration", `SELECT EXISTS (SELECT 1 FROM jobs
		WHERE status IN ('queued', 'running') AND json_extract(params, '$.integrationId') = ?)`, id)
}

// ActiveForDestination reports whether a queued or running job (dry runs included) works on
// destination id (sync, verify, retention, or a Plex database backup to it).
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

// hasRunning reports whether a non-dry-run job of type t for the same work as p is running: for
// a sync or verify, any one of the same destination (design §6.2: "a scheduled sync for a
// destination whose sync is already running is skipped"); otherwise the same canonical params,
// so a Plex DB backup is never skipped for another server's backup to the same destination.
func (s *Store) hasRunning(ctx context.Context, t jobs.Type, p jobs.Params) (bool, error) {
	var (
		q    string
		args []any
	)
	if (t == jobs.TypeSync || t == jobs.TypeVerify) && p.DestinationID != 0 {
		q = `SELECT EXISTS (SELECT 1 FROM jobs WHERE status = 'running' AND dry_run = 0 AND type = ? AND destination_id = ?)`
		args = []any{string(t), p.DestinationID}
	} else {
		params, err := canonicalParams(p)
		if err != nil {
			return false, err
		}
		q = `SELECT EXISTS (SELECT 1 FROM jobs WHERE status = 'running' AND dry_run = 0 AND type = ? AND params = ?)`
		args = []any{string(t), params}
	}
	var running bool
	if err := s.db.Reader().QueryRowContext(ctx, q, args...).Scan(&running); err != nil {
		return false, fmt.Errorf("check running %s jobs: %w", t, err)
	}
	return running, nil
}

// redactText is logging.RedactSecrets; named for readability at the call sites.
func redactText(s string) string { return logging.RedactSecrets(s) }
