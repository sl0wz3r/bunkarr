package tiers

import (
	"context"
	"database/sql"
	"fmt"
	"path"
	"strings"
)

// This file reads the catalog's table (catalog_files) directly, read-only, through a Queryer:
// catalog.Store has no queries that take one or return first_seen_at, and the manifest builder
// evaluates tiers inside its read transaction (design §11.2; the same deviation as
// internal/manifest/queries.go, §14.2 "as built").

// catalogRow is a live catalog file as the tier engine needs it.
type catalogRow struct {
	id        int64
	rel       string
	size      int64
	mtimeNs   int64
	group     string
	firstSeen string
}

const catalogRowColumns = `id, rel_path, size, mtime_ns, hardlink_group, first_seen_at`

func scanCatalogRow(rows *sql.Rows) (catalogRow, error) {
	var (
		r     catalogRow
		group sql.NullString
	)
	if err := rows.Scan(&r.id, &r.rel, &r.size, &r.mtimeNs, &group, &r.firstSeen); err != nil {
		return r, err
	}
	r.group = group.String
	return r, nil
}

// underRange returns the half-open range [p+"/", p+"0") of the paths strictly under p ('0' is the
// byte after '/').
func underRange(p string) (lo, hi string) { return p + "/", p + "0" }

// liveRows calls fn for the live catalog files of a source, by path. dir "" is the whole source;
// otherwise only the files directly in dir ("." is the source root).
func liveRows(ctx context.Context, q Queryer, sourceID int64, dir string, fn func(catalogRow) error) error {
	query := `SELECT ` + catalogRowColumns + ` FROM catalog_files WHERE source_id = ? AND deleted_at IS NULL`
	args := []any{sourceID}
	if dir != "" && dir != "." {
		lo, hi := underRange(dir)
		query += ` AND rel_path >= ? AND rel_path < ?`
		args = append(args, lo, hi)
	}
	query += ` ORDER BY rel_path`
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("read the catalog of source %d: %w", sourceID, err)
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanCatalogRow(rows)
		if err != nil {
			return fmt.Errorf("read the catalog of source %d: %w", sourceID, err)
		}
		if dir != "" && path.Dir(r.rel) != dir {
			continue
		}
		if err := fn(r); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the catalog of source %d: %w", sourceID, err)
	}
	return nil
}

// fileRow returns one live catalog file and its source id (sql.ErrNoRows when it is not live).
func fileRow(ctx context.Context, q Queryer, fileID int64) (catalogRow, int64, error) {
	var (
		r     catalogRow
		src   int64
		group sql.NullString
	)
	err := q.QueryRowContext(ctx, `SELECT source_id, `+catalogRowColumns+` FROM catalog_files WHERE id = ? AND deleted_at IS NULL`, fileID).
		Scan(&src, &r.id, &r.rel, &r.size, &r.mtimeNs, &group, &r.firstSeen)
	r.group = group.String
	return r, src, err
}

// groupRows returns the live files of a source in a hardlink group.
func groupRows(ctx context.Context, q Queryer, sourceID int64, group string) ([]catalogRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+catalogRowColumns+` FROM catalog_files
		WHERE hardlink_group = ? AND source_id = ? AND deleted_at IS NULL ORDER BY rel_path`, group, sourceID)
	if err != nil {
		return nil, fmt.Errorf("read a hardlink group: %w", err)
	}
	defer rows.Close()
	var out []catalogRow
	for rows.Next() {
		r, err := scanCatalogRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// anyLiveUnder reports whether a live catalog file of a source lies at or under rel.
func anyLiveUnder(ctx context.Context, q Queryer, sourceID int64, rel string) (bool, error) {
	lo, hi := underRange(rel)
	var n int64
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT 1 FROM catalog_files WHERE source_id = ? AND deleted_at IS NULL
		AND (rel_path = ? OR (rel_path >= ? AND rel_path < ?)) LIMIT 1)`, sourceID, rel, lo, hi).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("read the catalog of source %d: %w", sourceID, err)
	}
	return n > 0, nil
}

// under reports whether p is dir or lies under it ("" contains everything).
func under(p, dir string) bool {
	return dir == "" || p == dir || strings.HasPrefix(p, dir+"/")
}
