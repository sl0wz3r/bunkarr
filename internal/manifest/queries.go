package manifest

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Read-only queries of other packages' tables that a manifest reads inside its read transaction
// (design §11.2 step 2). The design (§14.2) has the owners expose them on a db.Queryer; until
// internal/catalog and internal/syncer do, they are kept here, read-only and in this one file:
// catalog_files (internal/catalog), destination_files (internal/syncer) and the retention column
// of destinations (internal/destinations).

// catalogRow is a live catalog file.
type catalogRow struct {
	id      int64
	rel     string
	size    int64
	mtimeNs int64
}

// liveCatalog calls fn for every live (not deleted) catalog file of a source, ordered by
// relative path.
func liveCatalog(ctx context.Context, q Queryer, sourceID int64, fn func(catalogRow) error) error {
	rows, err := q.QueryContext(ctx, `SELECT id, rel_path, size, mtime_ns FROM catalog_files
		WHERE source_id = ? AND deleted_at IS NULL ORDER BY rel_path`, sourceID)
	if err != nil {
		return fmt.Errorf("read the catalog of source %d: %w", sourceID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var c catalogRow
		if err := rows.Scan(&c.id, &c.rel, &c.size, &c.mtimeNs); err != nil {
			return fmt.Errorf("read the catalog of source %d: %w", sourceID, err)
		}
		if err := fn(c); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the catalog of source %d: %w", sourceID, err)
	}
	return nil
}

// Record states (destination_files.state) a manifest reads.
const (
	statePresent      = "present"
	stateLinked       = "linked"
	stateLinkRecorded = "link_recorded"
)

// recordRow is a live destination_files row (present, linked, link_recorded or missing).
type recordRow struct {
	id int64
	// sourceID is 0 when the source was deleted.
	sourceID  int64
	sourceRel string
	// relPath is the record's path relative to the destination target (unique among the live
	// records of a destination).
	relPath string
	size    int64
	mtimeNs int64
	hash    string
	linkOf  int64
	state   string
}

// liveRecords returns the live records of a destination by id.
func liveRecords(ctx context.Context, q Queryer, destinationID int64) (map[int64]*recordRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, source_id, rel_path, source_rel_path, size, mtime_ns, hash, link_of, state
		FROM destination_files WHERE destination_id = ? AND state IN ('present', 'linked', 'link_recorded', 'missing')
		ORDER BY id`, destinationID)
	if err != nil {
		return nil, fmt.Errorf("read the records of destination %d: %w", destinationID, err)
	}
	defer rows.Close()
	out := map[int64]*recordRow{}
	for rows.Next() {
		var (
			r              recordRow
			source, linkOf sql.NullInt64
			hash           sql.NullString
		)
		if err := rows.Scan(&r.id, &source, &r.relPath, &r.sourceRel, &r.size, &r.mtimeNs, &hash, &linkOf, &r.state); err != nil {
			return nil, fmt.Errorf("read the records of destination %d: %w", destinationID, err)
		}
		r.sourceID, r.linkOf, r.hash = source.Int64, linkOf.Int64, hash.String
		out[r.id] = &r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the records of destination %d: %w", destinationID, err)
	}
	return out, nil
}

// Retention of manifest versions (design §11.2 step 6, Destination.retention).
const (
	// DefaultKeepDays keeps the newest version of each of the last 30 days that have one.
	DefaultKeepDays = 30
	// MaxKeepDays bounds manifestDays.
	MaxKeepDays = 3650
	// DefaultKeepWeeks keeps the newest version of each of the last 12 ISO weeks that have one.
	DefaultKeepWeeks = 12
	// MaxKeepWeeks bounds manifestWeeks.
	MaxKeepWeeks = 520
)

// Keep is a destination's manifest retention.
type Keep struct {
	// Days is manifestDays (1-3650); Weeks is manifestWeeks (0-520).
	Days  int
	Weeks int
}

// ParseKeep reads manifestDays and manifestWeeks from a destination's retention JSON. A missing
// or out-of-range value takes its default: manifestDays is 1-3650 (0 is out of range: the newest
// day is always kept), manifestWeeks 0-520, where 0 keeps no weekly versions (design §11.2 step
// 6).
func ParseKeep(raw string) Keep {
	k := Keep{Days: DefaultKeepDays, Weeks: DefaultKeepWeeks}
	var r struct {
		ManifestDays  *int `json:"manifestDays"`
		ManifestWeeks *int `json:"manifestWeeks"`
	}
	if raw == "" || json.Unmarshal([]byte(raw), &r) != nil {
		return k
	}
	if d := r.ManifestDays; d != nil && *d >= 1 && *d <= MaxKeepDays {
		k.Days = *d
	}
	if wk := r.ManifestWeeks; wk != nil && *wk >= 0 && *wk <= MaxKeepWeeks {
		k.Weeks = *wk
	}
	return k
}

// keepOf reads a destination's manifest retention (defaults when it has none).
func keepOf(ctx context.Context, q Queryer, destinationID int64) (Keep, error) {
	var raw string
	err := q.QueryRowContext(ctx, `SELECT retention FROM destinations WHERE id = ?`, destinationID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return ParseKeep(""), nil
	}
	if err != nil {
		return Keep{}, fmt.Errorf("read the retention of destination %d: %w", destinationID, err)
	}
	return ParseKeep(raw), nil
}
