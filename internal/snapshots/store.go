package snapshots

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
)

// Snapshot is a recorded version (a row of the snapshots table; phase1.md §7 Snapshot, plus
// kind from phase2-3.md §13).
type Snapshot struct {
	ID            int64 `json:"id"`
	DestinationID int64 `json:"destinationId"`
	// Kind says which runner made the version: KindPlexDB or KindArr.
	Kind Kind `json:"kind"`
	// IntegrationID is the integration backed up (0 once that integration was deleted).
	IntegrationID int64 `json:"integrationId"`
	// JobID is the job that recorded the version (0 once that job's history was deleted).
	JobID int64 `json:"jobId"`
	// Path is the version directory relative to the destination target
	// ("<layout root>/<folder>/<yyyymmddThhmmssZ>[-job<id>]").
	Path      string    `json:"path"`
	CreatedAt time.Time `json:"createdAt"`
	// Size is the total size of the version's backup files (without its manifest.json).
	Size int64 `json:"size"`
	// Method is how the version was made (plexdb: online_backup or online_backup_immutable; arr:
	// arr_api_folder or arr_api_http).
	Method string `json:"method"`
	// Integrity is IntegrityOK or IntegrityFailed.
	Integrity string `json:"integrity"`
	// Manifest is the version's manifest.json as recorded.
	Manifest json.RawMessage `json:"manifest"`
}

// Version returns the retention view of s (see Keep and Prune).
func (s Snapshot) Version() Version {
	return Version{ID: s.ID, CreatedAt: s.CreatedAt, OK: s.Integrity == IntegrityOK}
}

// Store reads and writes the snapshots table. It is safe for concurrent use.
type Store struct {
	db *db.DB
}

// NewStore returns a Store over d.
func NewStore(d *db.DB) *Store {
	return &Store{db: d}
}

const snapshotColumns = `id, destination_id, kind, integration_id, job_id, engine_snapshot_id, created_at, size, method, integrity, manifest`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanSnapshot(r rowScanner) (Snapshot, error) {
	var (
		s                    Snapshot
		kind                 string
		integrationID, jobID sql.NullInt64
		created, manifest    string
	)
	if err := r.Scan(&s.ID, &s.DestinationID, &kind, &integrationID, &jobID, &s.Path, &created, &s.Size, &s.Method,
		&s.Integrity, &manifest); err != nil {
		return Snapshot{}, err
	}
	s.Kind = Kind(kind)
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

// List returns every version recorded for a destination, of every kind, newest first.
func (s *Store) List(ctx context.Context, destinationID int64) ([]Snapshot, error) {
	return s.query(ctx, `SELECT `+snapshotColumns+` FROM snapshots WHERE destination_id = ?
		ORDER BY created_at DESC, id DESC`, destinationID)
}

// ListFor returns the versions of one kind of one integration at a destination, newest first.
func (s *Store) ListFor(ctx context.Context, kind Kind, destinationID, integrationID int64) ([]Snapshot, error) {
	return s.query(ctx, `SELECT `+snapshotColumns+` FROM snapshots WHERE destination_id = ? AND integration_id = ? AND kind = ?
		ORDER BY created_at DESC, id DESC`, destinationID, integrationID, string(kind))
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

// Get returns one snapshot of any kind (ErrNotFound when there is none).
func (s *Store) Get(ctx context.Context, id int64) (Snapshot, error) {
	sn, err := scanSnapshot(s.db.Reader().QueryRowContext(ctx, `SELECT `+snapshotColumns+` FROM snapshots WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, ErrNotFound
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("get snapshot %d: %w", id, err)
	}
	return sn, nil
}

// Recorded reports whether a version directory of a destination has a row (of any kind: the
// directory is the row's identity, unique per destination).
func (s *Store) Recorded(ctx context.Context, destinationID int64, path string) (bool, error) {
	var one int
	err := s.db.Reader().QueryRowContext(ctx, `SELECT 1 FROM snapshots WHERE destination_id = ? AND engine_snapshot_id = ?`,
		destinationID, path).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("look up snapshot %s: %w", path, err)
	}
	return true, nil
}

// Insert records a version and returns it with its id. Kind, Path and Integrity are required;
// an empty Manifest is stored as {}. ID is ignored.
func (s *Store) Insert(ctx context.Context, sn Snapshot) (Snapshot, error) {
	switch {
	case !sn.Kind.Valid():
		return Snapshot{}, fmt.Errorf("record snapshot %s: unknown kind %q", sn.Path, sn.Kind)
	case sn.Path == "":
		return Snapshot{}, errors.New("record snapshot: no path")
	case sn.Integrity != IntegrityOK && sn.Integrity != IntegrityFailed:
		return Snapshot{}, fmt.Errorf("record snapshot %s: unknown integrity %q", sn.Path, sn.Integrity)
	}
	if len(sn.Manifest) == 0 {
		sn.Manifest = json.RawMessage("{}")
	}
	if !json.Valid(sn.Manifest) {
		return Snapshot{}, errors.New("record snapshot: the manifest is not valid JSON")
	}
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO snapshots (destination_id, job_id, kind, integration_id, engine_snapshot_id, created_at, size, method, integrity, manifest)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sn.DestinationID, nullID(sn.JobID), string(sn.Kind), nullID(sn.IntegrationID), sn.Path, db.FormatTime(sn.CreatedAt),
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

// Remove deletes a snapshot's row (after its directory left its name; see Layout.TrashVersion).
// Removing a row that does not exist is not an error.
func (s *Store) Remove(ctx context.Context, id int64) error {
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
