package jobqueue

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// Schedule is a row of the schedules table: a job enqueued on a cron expression. The next run
// time is not stored; Scheduler.NextRun computes it.
type Schedule struct {
	ID      int64       `json:"id"`
	JobType jobs.Type   `json:"jobType"`
	Params  jobs.Params `json:"params"`
	// Cron is a standard 5-field expression or @hourly/@daily/@weekly/@monthly, evaluated in the
	// scheduler's time zone (the container's TZ).
	Cron      string     `json:"cron"`
	Enabled   bool       `json:"enabled"`
	LastRunAt *time.Time `json:"lastRunAt"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
}

const scheduleColumns = `id, job_type, params, cron, enabled, last_run_at, created_at, updated_at`

func scanSchedule(sc rowScanner) (Schedule, error) {
	var (
		s                Schedule
		typ, params      string
		lastRun          sql.NullString
		created, updated string
	)
	if err := sc.Scan(&s.ID, &typ, &params, &s.Cron, &s.Enabled, &lastRun, &created, &updated); err != nil {
		return s, err
	}
	s.JobType = jobs.Type(typ)
	var err error
	if s.Params, err = parseParams(params); err != nil {
		return s, fmt.Errorf("schedule %d: %w", s.ID, err)
	}
	if s.LastRunAt, err = parseNullTime(lastRun); err != nil {
		return s, fmt.Errorf("schedule %d: last_run_at: %w", s.ID, err)
	}
	if s.CreatedAt, err = db.ParseTime(created); err != nil {
		return s, fmt.Errorf("schedule %d: created_at: %w", s.ID, err)
	}
	if s.UpdatedAt, err = db.ParseTime(updated); err != nil {
		return s, fmt.Errorf("schedule %d: updated_at: %w", s.ID, err)
	}
	return s, nil
}

// ListSchedules returns every schedule, ordered by id.
func (s *Store) ListSchedules(ctx context.Context) ([]Schedule, error) {
	rows, err := s.db.Reader().QueryContext(ctx, `SELECT `+scheduleColumns+` FROM schedules ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list schedules: %w", err)
	}
	defer rows.Close()
	out := []Schedule{}
	for rows.Next() {
		sc, err := scanSchedule(rows)
		if err != nil {
			return nil, fmt.Errorf("list schedules: %w", err)
		}
		out = append(out, sc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list schedules: %w", err)
	}
	return out, nil
}

// GetSchedule returns schedule id.
func (s *Store) GetSchedule(ctx context.Context, id int64) (Schedule, error) {
	sc, err := scanSchedule(s.db.Reader().QueryRowContext(ctx, `SELECT `+scheduleColumns+` FROM schedules WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Schedule{}, fmt.Errorf("schedule %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Schedule{}, fmt.Errorf("read schedule %d: %w", id, err)
	}
	return sc, nil
}

// UpsertSchedule creates the schedule for (jobType, params) or, when it exists, sets its cron and
// enabled flag. Params are compared in canonical form. The cron expression is validated with
// ValidateCron. Call Scheduler.Reload afterwards.
func (s *Store) UpsertSchedule(ctx context.Context, jobType jobs.Type, params jobs.Params, cron string, enabled bool) (Schedule, error) {
	if err := validateParams(jobType, params, false); err != nil {
		return Schedule{}, err
	}
	cron = strings.TrimSpace(cron)
	if err := ValidateCron(cron); err != nil {
		return Schedule{}, err
	}
	p, err := canonicalParams(params)
	if err != nil {
		return Schedule{}, err
	}
	now := db.FormatTime(s.now())
	var sc Schedule
	err = s.db.Write(ctx, func(tx *sql.Tx) error {
		sc, err = scanSchedule(tx.QueryRowContext(ctx, `INSERT INTO schedules (job_type, params, cron, enabled, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (job_type, params) DO UPDATE SET cron = excluded.cron, enabled = excluded.enabled, updated_at = excluded.updated_at
			RETURNING `+scheduleColumns, string(jobType), p, cron, enabled, now, now))
		return err
	})
	if err != nil {
		return Schedule{}, fmt.Errorf("save %s schedule: %w", jobType, err)
	}
	return sc, nil
}

// UpdateSchedule sets a schedule's cron expression (validated with ValidateCron) and enabled flag.
// Call Scheduler.Reload afterwards.
func (s *Store) UpdateSchedule(ctx context.Context, id int64, cron string, enabled bool) (Schedule, error) {
	cron = strings.TrimSpace(cron)
	if err := ValidateCron(cron); err != nil {
		return Schedule{}, err
	}
	var sc Schedule
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		var err error
		sc, err = scanSchedule(tx.QueryRowContext(ctx, `UPDATE schedules SET cron = ?, enabled = ?, updated_at = ? WHERE id = ?
			RETURNING `+scheduleColumns, cron, enabled, db.FormatTime(s.now()), id))
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return Schedule{}, fmt.Errorf("schedule %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Schedule{}, fmt.Errorf("update schedule %d: %w", id, err)
	}
	return sc, nil
}

// DeleteSchedule removes schedule id. Call Scheduler.Reload afterwards.
func (s *Store) DeleteSchedule(ctx context.Context, id int64) error {
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM schedules WHERE id = ?`, id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("schedule %d: %w", id, ErrNotFound)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("delete schedule %d: %w", id, err)
	}
	return nil
}

// DeleteSchedulesFor removes every schedule whose params match: each non-zero field of match
// must equal the schedule's (SourceIDs: the schedule's sources include all of match's). E.g.
// jobs.Params{DestinationID: 3} removes all schedules of destination 3 (sync, verify, Plex DB
// backups to it). A match with no non-zero selector is refused. Returns the number removed; call
// Scheduler.Reload afterwards.
func (s *Store) DeleteSchedulesFor(ctx context.Context, match jobs.Params) (int64, error) {
	if match.DestinationID == 0 && match.IntegrationID == 0 && len(match.SourceIDs) == 0 {
		return 0, ValidationError("DeleteSchedulesFor needs a destinationId, integrationId or sourceIds")
	}
	var deleted int64
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		deleted = 0
		rows, err := tx.QueryContext(ctx, `SELECT id, params FROM schedules`)
		if err != nil {
			return err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			var raw string
			if err := rows.Scan(&id, &raw); err != nil {
				rows.Close()
				return err
			}
			p, err := parseParams(raw)
			if err != nil {
				rows.Close()
				return fmt.Errorf("schedule %d: %w", id, err)
			}
			if paramsMatch(p, match) {
				ids = append(ids, id)
			}
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `DELETE FROM schedules WHERE id = ?`, id); err != nil {
				return err
			}
		}
		deleted = int64(len(ids))
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("delete schedules: %w", err)
	}
	return deleted, nil
}

// paramsMatch reports whether p has every non-zero selector of match.
func paramsMatch(p, match jobs.Params) bool {
	if match.DestinationID != 0 && p.DestinationID != match.DestinationID {
		return false
	}
	if match.IntegrationID != 0 && p.IntegrationID != match.IntegrationID {
		return false
	}
	for _, id := range match.SourceIDs {
		if !slices.Contains(p.SourceIDs, id) {
			return false
		}
	}
	return true
}

// MarkRun records that schedule id ran at at.
func (s *Store) MarkRun(ctx context.Context, id int64, at time.Time) error {
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE schedules SET last_run_at = ? WHERE id = ?`, db.FormatTime(at), id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("schedule %d: %w", id, ErrNotFound)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("mark schedule %d run: %w", id, err)
	}
	return nil
}

// ScheduleSpec is one schedule SyncSchedules wants.
type ScheduleSpec struct {
	Params  jobs.Params
	Cron    string
	Enabled bool
}

// SyncSchedules makes the schedules of jobType whose params match (paramsMatch, as
// DeleteSchedulesFor) exactly want, in one transaction: a wanted schedule that exists (same
// canonical params) gets want's cron and enabled flag and keeps its id and last_run_at; a missing
// one is created; a matching one that is not wanted is deleted. It is how the backup targets of
// an integration (up to 4, one plexdb_backup or arr_backup schedule per target, phase4.md §8.5)
// are mirrored. Every wanted params must match match, and no two may be equal; each is validated
// like UpsertSchedule's (ValidationError). A match with no non-zero selector is refused. It
// returns the matching schedules after the change, ordered by id; call Scheduler.Reload
// afterwards.
func (s *Store) SyncSchedules(ctx context.Context, jobType jobs.Type, match jobs.Params, want []ScheduleSpec) ([]Schedule, error) {
	if match.DestinationID == 0 && match.IntegrationID == 0 && len(match.SourceIDs) == 0 {
		return nil, ValidationError("SyncSchedules needs a destinationId, integrationId or sourceIds to match")
	}
	type wanted struct {
		params  string
		cron    string
		enabled bool
	}
	ws := make([]wanted, 0, len(want))
	seen := map[string]bool{}
	for _, w := range want {
		if err := validateParams(jobType, w.Params, false); err != nil {
			return nil, err
		}
		if !paramsMatch(normalizeParams(w.Params), match) {
			return nil, ValidationError(fmt.Sprintf("a %s schedule's params do not match the schedules being set", jobType))
		}
		cron := strings.TrimSpace(w.Cron)
		if err := ValidateCron(cron); err != nil {
			return nil, err
		}
		p, err := canonicalParams(w.Params)
		if err != nil {
			return nil, err
		}
		if seen[p] {
			return nil, ValidationError(fmt.Sprintf("two %s schedules with the same params", jobType))
		}
		seen[p] = true
		ws = append(ws, wanted{params: p, cron: cron, enabled: w.Enabled})
	}
	now := db.FormatTime(s.now())
	var out []Schedule
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		out = nil
		rows, err := tx.QueryContext(ctx, `SELECT id, params FROM schedules WHERE job_type = ?`, string(jobType))
		if err != nil {
			return err
		}
		var drop []int64
		for rows.Next() {
			var id int64
			var raw string
			if err := rows.Scan(&id, &raw); err != nil {
				_ = rows.Close()
				return err
			}
			p, err := parseParams(raw)
			if err != nil {
				_ = rows.Close()
				return fmt.Errorf("schedule %d: %w", id, err)
			}
			if !paramsMatch(p, match) {
				continue
			}
			c, err := canonicalParams(p)
			if err != nil {
				_ = rows.Close()
				return err
			}
			if !seen[c] {
				drop = append(drop, id)
			}
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range drop {
			if _, err := tx.ExecContext(ctx, `DELETE FROM schedules WHERE id = ?`, id); err != nil {
				return err
			}
		}
		for _, w := range ws {
			sc, err := scanSchedule(tx.QueryRowContext(ctx, `INSERT INTO schedules (job_type, params, cron, enabled, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?)
				ON CONFLICT (job_type, params) DO UPDATE SET cron = excluded.cron, enabled = excluded.enabled,
					updated_at = CASE WHEN schedules.cron = excluded.cron AND schedules.enabled = excluded.enabled
						THEN schedules.updated_at ELSE excluded.updated_at END
				RETURNING `+scheduleColumns, string(jobType), w.params, w.cron, w.enabled, now, now))
			if err != nil {
				return err
			}
			out = append(out, sc)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("set %s schedules: %w", jobType, err)
	}
	slices.SortFunc(out, func(a, b Schedule) int { return cmp.Compare(a.ID, b.ID) })
	if out == nil {
		out = []Schedule{}
	}
	return out, nil
}
