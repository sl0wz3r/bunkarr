package manifest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
)

// Integrity of a recorded version (manifests.integrity).
const (
	IntegrityOK      = "ok"
	IntegrityDamaged = "damaged"
)

// Version is a recorded manifest version (a manifests row; the API's Manifest).
type Version struct {
	ID            int64 `json:"id"`
	DestinationID int64 `json:"destinationId"`
	// JobID is the job that recorded the version (0 once that job's history was deleted).
	JobID     int64     `json:"jobId"`
	CreatedAt time.Time `json:"createdAt"`
	// Path is the version directory relative to the destination target.
	Path      string `json:"path"`
	Format    int    `json:"format"`
	ItemCount int64  `json:"itemCount"`
	FileCount int64  `json:"fileCount"`
	// Bytes is the size of the files the manifest lists.
	Bytes int64 `json:"bytes"`
	// Checksum is "sha256:<hex>" of manifest.json as written.
	Checksum string `json:"checksum"`
	// ContentHash is ContentHash of the manifest (the "unchanged" check).
	ContentHash string `json:"-"`
	// Integrity is IntegrityOK or IntegrityDamaged.
	Integrity string `json:"integrity"`
}

// Store reads and writes the manifests table. It is safe for concurrent use.
type Store struct {
	db *db.DB
}

// NewStore returns a Store over d.
func NewStore(d *db.DB) *Store { return &Store{db: d} }

const versionColumns = `id, destination_id, job_id, created_at, path, format, item_count, file_count, bytes, checksum, content_hash, integrity`

type rowScanner interface{ Scan(dest ...any) error }

func scanVersion(r rowScanner) (Version, error) {
	var (
		v       Version
		jobID   sql.NullInt64
		created string
	)
	if err := r.Scan(&v.ID, &v.DestinationID, &jobID, &created, &v.Path, &v.Format, &v.ItemCount, &v.FileCount, &v.Bytes,
		&v.Checksum, &v.ContentHash, &v.Integrity); err != nil {
		return Version{}, err
	}
	v.JobID = jobID.Int64
	t, err := db.ParseTime(created)
	if err != nil {
		return Version{}, fmt.Errorf("manifest %d: created_at: %w", v.ID, err)
	}
	v.CreatedAt = t
	return v, nil
}

// List returns a destination's versions, newest first.
func (s *Store) List(ctx context.Context, destinationID int64) ([]Version, error) {
	rows, err := s.db.Reader().QueryContext(ctx, `SELECT `+versionColumns+` FROM manifests WHERE destination_id = ?
		ORDER BY created_at DESC, id DESC`, destinationID)
	if err != nil {
		return nil, fmt.Errorf("list manifests: %w", err)
	}
	defer rows.Close()
	out := []Version{}
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, fmt.Errorf("list manifests: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list manifests: %w", err)
	}
	return out, nil
}

// Get returns one version (ErrNotFound when there is none).
func (s *Store) Get(ctx context.Context, id int64) (Version, error) {
	v, err := scanVersion(s.db.Reader().QueryRowContext(ctx, `SELECT `+versionColumns+` FROM manifests WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Version{}, ErrNotFound
	}
	if err != nil {
		return Version{}, fmt.Errorf("get manifest %d: %w", id, err)
	}
	return v, nil
}

// NewestOK returns a destination's newest version whose integrity is ok; ok is false when it has
// none.
func (s *Store) NewestOK(ctx context.Context, destinationID int64) (Version, bool, error) {
	v, err := scanVersion(s.db.Reader().QueryRowContext(ctx, `SELECT `+versionColumns+` FROM manifests
		WHERE destination_id = ? AND integrity = 'ok' ORDER BY created_at DESC, id DESC LIMIT 1`, destinationID))
	if errors.Is(err, sql.ErrNoRows) {
		return Version{}, false, nil
	}
	if err != nil {
		return Version{}, false, fmt.Errorf("read the newest manifest of destination %d: %w", destinationID, err)
	}
	return v, true, nil
}

// Recorded reports whether a version directory of a destination has a row.
func (s *Store) Recorded(ctx context.Context, destinationID int64, path string) (bool, error) {
	var one int
	err := s.db.Reader().QueryRowContext(ctx, `SELECT 1 FROM manifests WHERE destination_id = ? AND path = ?`, destinationID, path).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("look up manifest %s: %w", path, err)
	}
	return true, nil
}

// Insert records a version and returns it with its id. Path, Checksum, ContentHash and a known
// Integrity are required; ID is ignored.
func (s *Store) Insert(ctx context.Context, v Version) (Version, error) {
	switch {
	case v.Path == "":
		return Version{}, errors.New("record manifest: no path")
	case v.Checksum == "" || v.ContentHash == "":
		return Version{}, fmt.Errorf("record manifest %s: no checksum", v.Path)
	case v.Integrity != IntegrityOK && v.Integrity != IntegrityDamaged:
		return Version{}, fmt.Errorf("record manifest %s: unknown integrity %q", v.Path, v.Integrity)
	}
	if v.Format == 0 {
		v.Format = FormatVersion
	}
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO manifests (destination_id, job_id, created_at, path, format, item_count,
			file_count, bytes, checksum, content_hash, integrity) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			v.DestinationID, sql.NullInt64{Int64: v.JobID, Valid: v.JobID != 0}, db.FormatTime(v.CreatedAt), v.Path, v.Format,
			v.ItemCount, v.FileCount, v.Bytes, v.Checksum, v.ContentHash, v.Integrity)
		if err != nil {
			return err
		}
		v.ID, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return Version{}, fmt.Errorf("record manifest %s: %w", v.Path, err)
	}
	v.CreatedAt = v.CreatedAt.UTC()
	return v, nil
}

// MarkDamaged sets a version's integrity to damaged.
func (s *Store) MarkDamaged(ctx context.Context, id int64) error {
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE manifests SET integrity = 'damaged' WHERE id = ?`, id); err != nil {
			return fmt.Errorf("mark manifest %d damaged: %w", id, err)
		}
		return nil
	})
}

// Remove deletes a version's row (after its directory left its name). Removing a row that does
// not exist is not an error.
func (s *Store) Remove(ctx context.Context, id int64) error {
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM manifests WHERE id = ?`, id); err != nil {
			return fmt.Errorf("remove manifest %d: %w", id, err)
		}
		return nil
	})
}
