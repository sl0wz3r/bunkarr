package enginerun

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/engines"
)

// The tables this package owns (0004_phase4.sql): engine_snapshots (one row per restic snapshot a
// sync made and read back, §6.2), engine_forget (forget requests of the config runners, D28) and
// engine_state (per-destination engine state for the jobs and the UI).

// fullIDRe matches a full restic snapshot id.
var fullIDRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// SnapshotRow is one engine_snapshots row: a media snapshot of one source and batch.
type SnapshotRow struct {
	ID            int64     `json:"id"`
	DestinationID int64     `json:"destinationId"`
	SourceID      int64     `json:"sourceId"`
	JobID         int64     `json:"jobId"`
	SnapshotID    string    `json:"snapshotId"`
	CreatedAt     time.Time `json:"createdAt"`
	Batch         int       `json:"batch"`
	Complete      bool      `json:"complete"`
	Files         int64     `json:"files"`
	Bytes         int64     `json:"bytes"`
	DataAdded     int64     `json:"dataAdded"`
}

const snapshotColumns = `id, destination_id, source_id, job_id, snapshot_id, created_at, batch, complete, files, bytes, data_added`

func scanSnapshotRow(r interface{ Scan(...any) error }) (SnapshotRow, error) {
	var (
		row         SnapshotRow
		source, job sql.NullInt64
		created     string
		complete    int
	)
	if err := r.Scan(&row.ID, &row.DestinationID, &source, &job, &row.SnapshotID, &created, &row.Batch, &complete, &row.Files,
		&row.Bytes, &row.DataAdded); err != nil {
		return SnapshotRow{}, err
	}
	row.SourceID, row.JobID, row.Complete = source.Int64, job.Int64, complete == 1
	t, err := db.ParseTime(created)
	if err != nil {
		return SnapshotRow{}, fmt.Errorf("engine snapshot %d: created_at: %w", row.ID, err)
	}
	row.CreatedAt = t
	return row, nil
}

// snapshotRows returns a destination's recorded media snapshots, oldest first.
func (s *Service) snapshotRows(ctx context.Context, destinationID int64) ([]SnapshotRow, error) {
	rows, err := s.o.DB.Reader().QueryContext(ctx, `SELECT `+snapshotColumns+` FROM engine_snapshots
		WHERE destination_id = ? ORDER BY created_at, batch, id`, destinationID)
	if err != nil {
		return nil, fmt.Errorf("read the snapshots of destination %d: %w", destinationID, err)
	}
	defer rows.Close()
	out := []SnapshotRow{}
	for rows.Next() {
		r, err := scanSnapshotRow(rows)
		if err != nil {
			return nil, fmt.Errorf("read the snapshots of destination %d: %w", destinationID, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// basesOf returns each source's base (its newest recorded snapshot, §6.2) from rows.
func basesOf(rows []SnapshotRow) map[int64]string {
	newest := map[int64]SnapshotRow{}
	for _, r := range rows {
		if r.SourceID == 0 {
			continue
		}
		cur, ok := newest[r.SourceID]
		if !ok || newerRow(r, cur) {
			newest[r.SourceID] = r
		}
	}
	out := make(map[int64]string, len(newest))
	for src, r := range newest {
		out[src] = r.SnapshotID
	}
	return out
}

// newerRow orders rows by when they were recorded (the row id), not by restic's snapshot time:
// the base is the snapshot the last read-back recorded the records against, and a host clock set
// back between two syncs would otherwise make an older snapshot the base.
func newerRow(a, b SnapshotRow) bool { return a.ID > b.ID }

// baseOf returns the base of one source ("" when it has none).
func (s *Service) baseOf(ctx context.Context, destinationID, sourceID int64) (string, error) {
	rows, err := s.snapshotRows(ctx, destinationID)
	if err != nil {
		return "", err
	}
	return basesOf(rows)[sourceID], nil
}

// insertSnapshotTx records a read-back snapshot (idempotent on the snapshot id).
func insertSnapshotTx(ctx context.Context, tx *sql.Tx, r SnapshotRow, summary []byte) error {
	if !fullIDRe.MatchString(r.SnapshotID) {
		return fmt.Errorf("record snapshot: %q is not a full snapshot id", r.SnapshotID)
	}
	if len(summary) == 0 {
		summary = []byte("{}")
	}
	complete := 0
	if r.Complete {
		complete = 1
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO engine_snapshots (destination_id, source_id, job_id, snapshot_id, created_at, batch,
		complete, files, bytes, data_added, summary) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (destination_id, snapshot_id) DO NOTHING`,
		r.DestinationID, nullID(r.SourceID), nullID(r.JobID), r.SnapshotID, db.FormatTime(r.CreatedAt), max(r.Batch, 1), complete,
		r.Files, r.Bytes, r.DataAdded, string(summary))
	if err != nil {
		return fmt.Errorf("record snapshot %s: %w", short(r.SnapshotID), err)
	}
	return nil
}

// markCompleteTx marks the newest row of a job and source complete (§6.2 step 6).
func markCompleteTx(ctx context.Context, tx *sql.Tx, destinationID, sourceID, jobID int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE engine_snapshots SET complete = 1 WHERE id = (SELECT id FROM engine_snapshots
		WHERE destination_id = ? AND source_id = ? AND job_id = ? ORDER BY batch DESC, id DESC LIMIT 1)`, destinationID, sourceID, jobID)
	if err != nil {
		return fmt.Errorf("mark the snapshot of source %d complete: %w", sourceID, err)
	}
	return nil
}

// deleteSnapshotRowTx deletes the row of a snapshot that is gone (forgotten, or removed outside
// Bunkarr).
func deleteSnapshotRowTx(ctx context.Context, tx *sql.Tx, destinationID int64, snapshotID string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM engine_snapshots WHERE destination_id = ? AND snapshot_id = ?`, destinationID,
		snapshotID); err != nil {
		return fmt.Errorf("delete the row of snapshot %s: %w", short(snapshotID), err)
	}
	return nil
}

// List returns the media snapshots recorded for a restic destination, oldest first (GET
// /destinations/{id}/snapshots: kind media, sourceId, engineRef, complete, files, dataAdded).
func (s *Service) List(ctx context.Context, destinationID int64) ([]engines.Version, error) {
	rows, err := s.snapshotRows(ctx, destinationID)
	if err != nil {
		return nil, err
	}
	out := make([]engines.Version, 0, len(rows))
	for _, r := range rows {
		out = append(out, engines.Version{Kind: engines.VersionMedia, Ref: engines.Ref(r.SnapshotID), SourceID: r.SourceID,
			JobID: r.JobID, Time: r.CreatedAt, Batch: r.Batch, Complete: r.Complete, Files: r.Files, Bytes: r.Bytes,
			DataAdded: r.DataAdded})
	}
	return out, nil
}

// --- engine_forget ---

// forgetRequest is one engine_forget row.
type forgetRequest struct {
	SnapshotID string
	Kind       string
	Reason     string
}

// RequestForget records that a config runner pruned the version in snapshot snapshotID of a
// restic destination (D28): the destination's next retention job forgets it after the S24 checks.
// It runs in the caller's transaction, with the delete of the version's row. A repeated request is
// kept once.
func (s *Service) RequestForget(ctx context.Context, tx *sql.Tx, destinationID int64, snapshotID, kind, reason string) error {
	if !fullIDRe.MatchString(snapshotID) {
		return fmt.Errorf("forget request: %q is not a full snapshot id", snapshotID)
	}
	switch kind {
	case engines.VersionPlexDB, engines.VersionArr, engines.VersionManifest:
	default:
		return fmt.Errorf("forget request: kind %q", kind)
	}
	if strings.TrimSpace(reason) == "" {
		reason = "pruned by its runner"
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO engine_forget (destination_id, snapshot_id, kind, reason, requested_at)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT (destination_id, snapshot_id) DO NOTHING`,
		destinationID, snapshotID, kind, reason, db.FormatTime(s.now()))
	if err != nil {
		return fmt.Errorf("request the forget of snapshot %s: %w", short(snapshotID), err)
	}
	return nil
}

// forgetRequests returns a destination's pending forget requests.
func (s *Service) forgetRequests(ctx context.Context, destinationID int64) ([]forgetRequest, error) {
	rows, err := s.o.DB.Reader().QueryContext(ctx, `SELECT snapshot_id, kind, reason FROM engine_forget WHERE destination_id = ?
		ORDER BY requested_at, snapshot_id`, destinationID)
	if err != nil {
		return nil, fmt.Errorf("read the forget requests of destination %d: %w", destinationID, err)
	}
	defer rows.Close()
	var out []forgetRequest
	for rows.Next() {
		var r forgetRequest
		if err := rows.Scan(&r.SnapshotID, &r.Kind, &r.Reason); err != nil {
			return nil, fmt.Errorf("read the forget requests of destination %d: %w", destinationID, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// deleteForgetTx removes a served (or obsolete) forget request.
func deleteForgetTx(ctx context.Context, tx *sql.Tx, destinationID int64, snapshotID string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM engine_forget WHERE destination_id = ? AND snapshot_id = ?`, destinationID,
		snapshotID); err != nil {
		return fmt.Errorf("delete the forget request of %s: %w", short(snapshotID), err)
	}
	return nil
}

// --- engine_state ---

// EngineState is a destination's engine state (engine_state), for the jobs and the UI.
type EngineState struct {
	DestinationID int64 `json:"destinationId"`
	// EngineVersion is the engine binary at the last job ("restic 0.18.1").
	EngineVersion string     `json:"engineVersion"`
	LastPruneAt   *time.Time `json:"lastPruneAt"`
	LastCheckAt   *time.Time `json:"lastCheckAt"`
	// ReadSubsetNext is n of the next restic check --read-data-subset=n/t (1..t).
	ReadSubsetNext int        `json:"readSubsetNext"`
	LastCleanupAt  *time.Time `json:"lastCleanupAt"`
	// ThroughputBps is the upload rate recent syncs measured (bytes/s, EWMA; nil: unknown).
	ThroughputBps *float64 `json:"throughputBps"`
	// Stats are repository or remote figures for the UI and the job's own bookkeeping.
	Stats     map[string]json.RawMessage `json:"stats"`
	UpdatedAt time.Time                  `json:"updatedAt"`
}

// StatSnapshotCount is the key of EngineState.Stats that State fills with the number of media
// snapshots recorded for a restic destination (engine_snapshots; set only when there is one): the
// snapshot count the destination card shows (phase4.md §15). It is derived when read, never stored.
const StatSnapshotCount = "snapshotCount"

// State returns a destination's engine state (a zero state when no job ran yet), with
// Stats[StatSnapshotCount] when media snapshots are recorded.
func (s *Service) State(ctx context.Context, destinationID int64) (EngineState, error) {
	st, err := readState(ctx, s.o.DB.Reader(), destinationID)
	if err != nil {
		return st, err
	}
	var n int64
	if err := s.o.DB.Reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM engine_snapshots WHERE destination_id = ?`, destinationID).
		Scan(&n); err != nil {
		return st, fmt.Errorf("count the snapshots of destination %d: %w", destinationID, err)
	}
	if n > 0 {
		st.setStat(StatSnapshotCount, n)
	}
	return st, nil
}

type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func readState(ctx context.Context, q queryRower, destinationID int64) (EngineState, error) {
	st := EngineState{DestinationID: destinationID, ReadSubsetNext: 1, Stats: map[string]json.RawMessage{}}
	var (
		prune, check, cleanup sql.NullString
		throughput            sql.NullFloat64
		stats, updated        string
	)
	err := q.QueryRowContext(ctx, `SELECT engine_version, last_prune_at, last_check_at, read_subset_next, last_cleanup_at,
		throughput_bps, stats, updated_at FROM engine_state WHERE destination_id = ?`, destinationID).
		Scan(&st.EngineVersion, &prune, &check, &st.ReadSubsetNext, &cleanup, &throughput, &stats, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return st, fmt.Errorf("read the engine state of destination %d: %w", destinationID, err)
	}
	for _, t := range []struct {
		src sql.NullString
		dst **time.Time
	}{{prune, &st.LastPruneAt}, {check, &st.LastCheckAt}, {cleanup, &st.LastCleanupAt}} {
		if !t.src.Valid {
			continue
		}
		v, err := db.ParseTime(t.src.String)
		if err != nil {
			return st, fmt.Errorf("engine state of destination %d: %w", destinationID, err)
		}
		*t.dst = &v
	}
	if throughput.Valid {
		v := throughput.Float64
		st.ThroughputBps = &v
	}
	if stats != "" {
		if err := json.Unmarshal([]byte(stats), &st.Stats); err != nil {
			st.Stats = map[string]json.RawMessage{}
		}
	}
	if st.UpdatedAt, err = db.ParseTime(updated); err != nil {
		return st, fmt.Errorf("engine state of destination %d: %w", destinationID, err)
	}
	if st.ReadSubsetNext < 1 {
		st.ReadSubsetNext = 1
	}
	return st, nil
}

// updateState changes a destination's engine state in one write transaction (the row is
// created when it does not exist).
func (s *Service) updateState(ctx context.Context, destinationID int64, fn func(*EngineState)) error {
	return s.o.DB.Write(context.WithoutCancel(ctx), func(tx *sql.Tx) error {
		st, err := readState(ctx, tx, destinationID)
		if err != nil {
			return err
		}
		fn(&st)
		stats, err := json.Marshal(st.Stats)
		if err != nil {
			return fmt.Errorf("engine state: %w", err)
		}
		var throughput sql.NullFloat64
		if st.ThroughputBps != nil {
			throughput = sql.NullFloat64{Float64: *st.ThroughputBps, Valid: true}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO engine_state (destination_id, engine_version, last_prune_at, last_check_at,
			read_subset_next, last_cleanup_at, throughput_bps, stats, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (destination_id) DO UPDATE SET engine_version = excluded.engine_version, last_prune_at = excluded.last_prune_at,
			last_check_at = excluded.last_check_at, read_subset_next = excluded.read_subset_next,
			last_cleanup_at = excluded.last_cleanup_at, throughput_bps = excluded.throughput_bps, stats = excluded.stats,
			updated_at = excluded.updated_at`,
			destinationID, st.EngineVersion, optTime(st.LastPruneAt), optTime(st.LastCheckAt), max(st.ReadSubsetNext, 1),
			optTime(st.LastCleanupAt), throughput, string(stats), db.FormatTime(s.now()))
		if err != nil {
			return fmt.Errorf("write the engine state of destination %d: %w", destinationID, err)
		}
		return nil
	})
}

// setStat stores one figure of EngineState.Stats.
func (st *EngineState) setStat(key string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	if st.Stats == nil {
		st.Stats = map[string]json.RawMessage{}
	}
	st.Stats[key] = b
}

// stat reads one figure of EngineState.Stats into v (false when it is not there).
func (st EngineState) stat(key string, v any) bool {
	raw, ok := st.Stats[key]
	return ok && json.Unmarshal(raw, v) == nil
}

// throughputWeight is the weight of a new measurement in the throughput EWMA.
const throughputWeight = 0.3

// noteThroughput folds a measured upload (bytes over d) into the destination's throughput EWMA
// (§9.2: the window fitting uses it when no limit is lower). Short or tiny transfers say nothing
// about the uplink and are ignored.
func (s *Service) noteThroughput(ctx context.Context, destinationID, bytes int64, d time.Duration) {
	if bytes < 1<<20 || d < 2*time.Second {
		return
	}
	rate := float64(bytes) / d.Seconds()
	err := s.updateState(ctx, destinationID, func(st *EngineState) {
		if st.ThroughputBps == nil || *st.ThroughputBps <= 0 {
			st.ThroughputBps = &rate
			return
		}
		v := (1-throughputWeight)*(*st.ThroughputBps) + throughputWeight*rate
		st.ThroughputBps = &v
	})
	if err != nil {
		s.log.Warn("could not record the measured throughput", "destination", destinationID, "error", err)
	}
}

// --- helpers ---

func nullID(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: v != 0} }

func optTime(t *time.Time) sql.NullString {
	if t == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: db.FormatTime(*t), Valid: true}
}

// short is the first 8 characters of a snapshot id.
func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// sortedKeys returns a map's keys sorted.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
