package jobqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// Deferred jobs (docs/design/phase4.md §9.2, §11.2). A runner that returns a jobs.DeferredError
// (its destination's transfer window is closed) goes back to the queue with not_before = Until and
// deferrals + 1; its attempt, trigger, items and planned_at stay. The dispatcher starts no queued
// job before its not_before and wakes at the earliest one. A job counts as deferred while it is
// queued with deferrals > 0 or a not_before: the dedupe and the coalescing never return or merge
// into it, and a fire of its destination's sync schedule supersedes a deferred sync.

// deferredCond is the SQL condition of a deferred job (on a queued row).
const deferredCond = `(deferrals > 0 OR not_before IS NOT NULL)`

// errNoResumeTime is the error of a deferral without a usable time.
const errNoResumeTime = "deferred without a time to resume"

// DeferLogMessage returns the job log line and summary of a job deferred until until: "Waiting
// for the transfer window until <time>" (the time as the runner gave it, in its location).
func DeferLogMessage(until time.Time) string {
	return "Waiting for the transfer window until " + until.Format("2006-01-02 15:04 MST")
}

// deferJob puts a running job back in the queue until until (a jobs.DeferredError): status
// queued, not_before = until, deferrals + 1, the summary saying what it waits for; attempt,
// trigger, items and planned_at are kept. ok is false when the job was no longer running.
//
// A sync that superseded deferred syncs (superseded_by) continues their count: its deferrals
// become at least theirs. The seed's wait goes on in the new job, and the first wait of a job a
// fire queued outside the window is the wait its predecessor already counted, so that deferral
// adds nothing; each later one adds one. Without it every fire would restart the count, and the
// warning at longSeedDeferrals (internal/enginerun: "the seed needs more windows than two weeks",
// phase4.md §9.2) would never come for a seed superseded by a daily schedule. The runner warns
// from the count it started with plus one, which is the value stored here on every deferral that
// adds one.
func (s *Store) deferJob(ctx context.Context, id int64, until time.Time, summary string, progress []byte) (ok bool, err error) {
	err = s.db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE jobs SET status = 'queued', not_before = ?,
			deferrals = MAX(deferrals + 1, COALESCE((SELECT MAX(p.deferrals) FROM jobs p WHERE p.superseded_by = jobs.id), 0)),
			summary = ?, progress = ?, heartbeat_at = NULL WHERE id = ? AND status = 'running'`,
			db.FormatTime(until), summary, string(progress), id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		ok = n == 1
		return err
	})
	if err != nil {
		return false, fmt.Errorf("defer job %d: %w", id, err)
	}
	return ok, nil
}

// hasDeferred reports whether a deferred job of type t (not a dry run) of destination
// destinationID is queued: a scheduled verify or retention of that destination is then skipped,
// because the deferred job covers it (phase4.md §9.2).
func (s *Store) hasDeferred(ctx context.Context, t jobs.Type, destinationID int64) (bool, error) {
	var deferred bool
	if err := s.db.Reader().QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM jobs WHERE status = 'queued' AND dry_run = 0
		AND type = ? AND destination_id = ? AND `+deferredCond+`)`, string(t), destinationID).Scan(&deferred); err != nil {
		return false, fmt.Errorf("check deferred %s jobs: %w", t, err)
	}
	return deferred, nil
}

// superseded is a deferred job a scheduled sync superseded.
type superseded struct {
	id  int64
	job jobs.Job
}

// supersedeDeferredSyncs ends, as cancelled, the deferred syncs (not dry runs) of p's destination
// that a scheduled sync with params p covers, except job keep (the one the fire queued): its
// sources include theirs, it applies held changes when they do (AllowChanges), and they are not
// releases (ReleaseDemoted, whose approval a plain sync would drop). Each gets the job log line
// and summary msg, and superseded_by = keep, as do the jobs it superseded before: keep's plan
// carries their items' window cuts (carryWindowCuts). One transaction; it returns the jobs as
// they are now.
func (s *Store) supersedeDeferredSyncs(ctx context.Context, p jobs.Params, keep int64, msg string, now time.Time) ([]jobs.Job, error) {
	var out []jobs.Job
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		out = nil
		rows, err := tx.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE status = 'queued' AND dry_run = 0 AND type = 'sync'
			AND destination_id = ? AND id <> ? AND `+deferredCond+` ORDER BY id`, p.DestinationID, keep)
		if err != nil {
			return err
		}
		var cands []jobs.Job
		for rows.Next() {
			j, err := scanJob(rows)
			if err != nil {
				_ = rows.Close()
				return err
			}
			cands = append(cands, j)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		at := db.FormatTime(now)
		for _, j := range cands {
			if !coversSources(p.SourceIDs, sortedIDs(j.Params.SourceIDs)) || (j.Params.AllowChanges && !p.AllowChanges) ||
				j.Params.ReleaseDemoted {
				continue
			}
			done, err := scanJob(tx.QueryRowContext(ctx, `UPDATE jobs SET status = 'cancelled', summary = ?, finished_at = ?,
				not_before = NULL, superseded_by = ? WHERE id = ? AND status = 'queued' RETURNING `+jobColumns, msg+".", at, keep, j.ID))
			if err != nil {
				return fmt.Errorf("supersede job %d: %w", j.ID, err)
			}
			// The syncs j superseded earlier (it may have waited without a plan) now point at keep
			// too, so their window cuts reach its plan (carryWindowCuts).
			if _, err := tx.ExecContext(ctx, `UPDATE jobs SET superseded_by = ? WHERE superseded_by = ?`, keep, j.ID); err != nil {
				return fmt.Errorf("supersede job %d: %w", j.ID, err)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO job_logs (job_id, at, level, message, fields) VALUES (?, ?, 'info', ?, '{}')`,
				j.ID, at, msg); err != nil {
				return fmt.Errorf("log the supersede of job %d: %w", j.ID, err)
			}
			out = append(out, done)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("supersede deferred syncs of destination %d: %w", p.DestinationID, err)
	}
	return out, nil
}

// carryCutsQuery reads the pending items with window cuts of the syncs a job superseded. Its item
// terms are those of the partial index job_items_window_cuts (migration 0004), so SQLite reads
// only the cut items of each superseded job, not all its pending items
// (TestCarryCutsQueryUsesTheCutIndex).
const carryCutsQuery = `SELECT i.rel_path, i.detail FROM job_items i JOIN jobs j ON j.id = i.job_id
	WHERE j.superseded_by = ? AND i.status = 'pending' AND json_valid(i.detail)
	AND json_extract(i.detail, '$.windowCuts') > 0`

// cutKey identifies a file across plans for carrying its window cuts: the item's path, the
// source and the size planned (a file of another size is another upload).
type cutKey struct {
	rel    string
	source int64
	size   int64
}

// cutFields are the item detail fields (syncer.Detail, which the engines' details embed) the
// carry reads.
type cutFields struct {
	SourceID   int64 `json:"sourceId"`
	Size       int64 `json:"size"`
	WindowCuts int   `json:"windowCuts"`
}

// carryWindowCuts gives the pending items of a plan being added to job jobID the window cuts
// (Detail.WindowCuts) that the same files have on the pending items of the deferred syncs jobID
// superseded (superseded_by, phase4.md §9.2): without them each scheduled fire would restart the
// count, and the "cut in two consecutive windows" backstop for an unknown rate would never fail
// a file that never fits (it would upload again from the start in every window). rows' details
// change in place.
//
// It runs for every page of the plan, inside the write transaction, and the chain can be long (a
// seed superseded every day for weeks, each job with many pending items): carryCutsQuery reads
// only the cut items, through the partial index job_items_window_cuts.
func carryWindowCuts(ctx context.Context, tx *sql.Tx, jobID int64, rows []itemRow) error {
	if len(rows) == 0 {
		return nil
	}
	res, err := tx.QueryContext(ctx, carryCutsQuery, jobID)
	if err != nil {
		return fmt.Errorf("read the window cuts of the superseded syncs: %w", err)
	}
	cuts := map[cutKey]int{}
	for res.Next() {
		var rel, detail string
		if err := res.Scan(&rel, &detail); err != nil {
			_ = res.Close()
			return fmt.Errorf("read the window cuts of the superseded syncs: %w", err)
		}
		var f cutFields
		if json.Unmarshal([]byte(detail), &f) != nil {
			continue
		}
		k := cutKey{rel: rel, source: f.SourceID, size: f.Size}
		cuts[k] = max(cuts[k], f.WindowCuts)
	}
	if err := res.Close(); err != nil {
		return err
	}
	if err := res.Err(); err != nil {
		return fmt.Errorf("read the window cuts of the superseded syncs: %w", err)
	}
	if len(cuts) == 0 {
		return nil
	}
	for i := range rows {
		r := &rows[i]
		if r.it.Status != jobs.ItemPending {
			continue
		}
		var f cutFields
		if json.Unmarshal([]byte(r.detail), &f) != nil {
			continue
		}
		n := cuts[cutKey{rel: r.it.RelPath, source: f.SourceID, size: f.Size}]
		if n <= f.WindowCuts {
			continue
		}
		var m map[string]json.RawMessage
		if json.Unmarshal([]byte(r.detail), &m) != nil || m == nil {
			continue
		}
		m["windowCuts"] = json.RawMessage(strconv.Itoa(n))
		b, err := json.Marshal(m)
		if err != nil {
			return fmt.Errorf("item %q: carry its window cuts: %w", r.it.RelPath, err)
		}
		r.detail = string(b)
	}
	return nil
}

// activeTypes are the job types that work on one destination (params.destinationId): the six of
// ActiveForDestinationAnyType.
var activeTypes = []jobs.Type{jobs.TypeSync, jobs.TypeVerify, jobs.TypeRetention, jobs.TypePlexDBBackup, jobs.TypeArrBackup,
	jobs.TypeManifestExport}

// ActiveForDestinationAnyType reports whether a running or deferred job of any type that works on
// a destination (sync, verify, retention, plexdb_backup, arr_backup, manifest_export) has
// destinationID as its params.destinationId (phase4.md §6.7, §9.3: "active"). The config version
// jobs hold no dest:<id> key but still run restic at the destination, so the unlock endpoint
// refuses while one runs. A queued job that never started is not active here (it does not run
// yet); a deferred one is (it holds a plan and resumes in the next window).
func (s *Store) ActiveForDestinationAnyType(ctx context.Context, destinationID int64) (bool, error) {
	args := []any{destinationID}
	in := ""
	for i, t := range activeTypes {
		if i > 0 {
			in += ", "
		}
		in += "?"
		args = append(args, string(t))
	}
	return s.activeFor(ctx, "destination", `SELECT EXISTS (SELECT 1 FROM jobs WHERE destination_id = ?
		AND type IN (`+in+`) AND (status = 'running' OR (status = 'queued' AND `+deferredCond+`)))`, args...)
}
