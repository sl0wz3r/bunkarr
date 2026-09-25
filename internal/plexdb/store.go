package plexdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
)

// kindPlexDB is snapshots.kind for Plex DB versions.
const kindPlexDB = "plexdb"

// Snapshot is a recorded Plex DB version (a row of the snapshots table; design §7 Snapshot).
type Snapshot struct {
	ID            int64 `json:"id"`
	DestinationID int64 `json:"destinationId"`
	// IntegrationID is the Plex integration backed up (0 once that integration was deleted).
	IntegrationID int64 `json:"integrationId"`
	// JobID is the job that recorded the version (0 once that job's history was deleted).
	JobID int64 `json:"jobId"`
	// Path is the version directory relative to the destination target
	// (".bunkarr/plex/<folder>/<yyyymmddThhmmssZ>").
	Path      string    `json:"path"`
	CreatedAt time.Time `json:"createdAt"`
	// Size is the total size of the version's backup files (without the manifest).
	Size int64 `json:"size"`
	// Method is the library database's backup method.
	Method string `json:"method"`
	// Integrity is IntegrityOK or IntegrityFailed.
	Integrity string `json:"integrity"`
	// Manifest is the version's manifest.json (a Manifest).
	Manifest json.RawMessage `json:"manifest"`
}

// Store reads and writes the snapshots table. It is safe for concurrent use.
type Store struct {
	db *db.DB
}

// NewStore returns a Store over d.
func NewStore(d *db.DB) *Store {
	return &Store{db: d}
}

const snapshotColumns = `id, destination_id, integration_id, job_id, engine_snapshot_id, created_at, size, method, integrity, manifest`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanSnapshot(r rowScanner) (Snapshot, error) {
	var (
		s                    Snapshot
		integrationID, jobID sql.NullInt64
		created, manifest    string
	)
	if err := r.Scan(&s.ID, &s.DestinationID, &integrationID, &jobID, &s.Path, &created, &s.Size, &s.Method, &s.Integrity, &manifest); err != nil {
		return Snapshot{}, err
	}
	s.IntegrationID, s.JobID = integrationID.Int64, jobID.Int64
	t, err := db.ParseTime(created)
	if err != nil {
		return Snapshot{}, fmt.Errorf("snapshot %d: created_at: %w", s.ID, err)
	}
	s.CreatedAt = t
	if manifest == "" || !json.Valid([]byte(manifest)) {
		manifest = "{}"
	}
	s.Manifest = json.RawMessage(manifest)
	return s, nil
}

// List returns the Plex DB versions recorded for a destination, newest first.
func (s *Store) List(ctx context.Context, destinationID int64) ([]Snapshot, error) {
	return s.query(ctx, `SELECT `+snapshotColumns+` FROM snapshots WHERE destination_id = ? AND kind = ?
		ORDER BY created_at DESC, id DESC`, destinationID, kindPlexDB)
}

// ListFor returns the versions of one Plex integration at a destination, newest first.
func (s *Store) ListFor(ctx context.Context, destinationID, integrationID int64) ([]Snapshot, error) {
	return s.query(ctx, `SELECT `+snapshotColumns+` FROM snapshots WHERE destination_id = ? AND integration_id = ? AND kind = ?
		ORDER BY created_at DESC, id DESC`, destinationID, integrationID, kindPlexDB)
}

func (s *Store) query(ctx context.Context, q string, args ...any) ([]Snapshot, error) {
	rows, err := s.db.Reader().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}
	defer rows.Close()
	out := []Snapshot{}
	for rows.Next() {
		sn, err := scanSnapshot(rows)
		if err != nil {
			return nil, fmt.Errorf("list snapshots: %w", err)
		}
		out = append(out, sn)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}
	return out, nil
}

// Get returns one snapshot (ErrNotFound when there is none).
func (s *Store) Get(ctx context.Context, id int64) (Snapshot, error) {
	sn, err := scanSnapshot(s.db.Reader().QueryRowContext(ctx, `SELECT `+snapshotColumns+` FROM snapshots WHERE id = ? AND kind = ?`, id, kindPlexDB))
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, ErrNotFound
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("get snapshot %d: %w", id, err)
	}
	return sn, nil
}

// recorded reports whether a version directory of a destination has a row.
func (s *Store) recorded(ctx context.Context, destinationID int64, path string) (bool, error) {
	var one int
	err := s.db.Reader().QueryRowContext(ctx, `SELECT 1 FROM snapshots WHERE destination_id = ? AND engine_snapshot_id = ?`, destinationID, path).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("look up snapshot %s: %w", path, err)
	}
	return true, nil
}

// insert records a version and returns it with its id.
func (s *Store) insert(ctx context.Context, sn Snapshot) (Snapshot, error) {
	if len(sn.Manifest) == 0 {
		sn.Manifest = json.RawMessage("{}")
	}
	if !json.Valid(sn.Manifest) {
		return Snapshot{}, errors.New("record snapshot: the manifest is not valid JSON")
	}
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO snapshots (destination_id, job_id, kind, integration_id, engine_snapshot_id, created_at, size, method, integrity, manifest)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sn.DestinationID, nullID(sn.JobID), kindPlexDB, nullID(sn.IntegrationID), sn.Path, db.FormatTime(sn.CreatedAt),
			sn.Size, sn.Method, sn.Integrity, string(sn.Manifest))
		if err != nil {
			return err
		}
		sn.ID, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return Snapshot{}, fmt.Errorf("record snapshot %s: %w", sn.Path, err)
	}
	sn.CreatedAt = sn.CreatedAt.UTC()
	return sn, nil
}

// remove deletes a snapshot's row (after its directory was deleted).
func (s *Store) remove(ctx context.Context, id int64) error {
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM snapshots WHERE id = ?`, id); err != nil {
			return fmt.Errorf("remove snapshot %d: %w", id, err)
		}
		return nil
	})
}

// nullID stores 0 as NULL.
func nullID(id int64) sql.NullInt64 {
	return sql.NullInt64{Int64: id, Valid: id != 0}
}
