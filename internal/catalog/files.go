package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
)

// File is one catalog row: a regular file of a source as the last scans saw it.
type File struct {
	ID       int64
	SourceID int64
	// RelPath is slash-separated and relative to the source root.
	RelPath string
	Size    int64
	MtimeNs int64
	CtimeNs int64
	Dev     uint64
	Inode   uint64
	Nlink   uint64
	// HardlinkGroup is the per-scan surrogate "<sourceId>.<scanSeq>:<n>" shared by the names of one
	// hardlinked inode found by the latest scan ("" = not grouped). Never compare it across scans.
	HardlinkGroup string
	// DeletedAt is set when a scan no longer found the file (nil = live).
	DeletedAt *time.Time
	// LastSeenAt is the start of the last scan that saw the file.
	LastSeenAt time.Time
}

// Mtime returns the modification time.
func (f File) Mtime() time.Time { return time.Unix(0, f.MtimeNs).UTC() }

// Meta returns the file's recorded stat metadata.
func (f File) Meta() Meta {
	return Meta{Size: f.Size, MtimeNs: f.MtimeNs, CtimeNs: f.CtimeNs, Dev: f.Dev, Inode: f.Inode, Nlink: f.Nlink}
}

// catalogFileJSON is the API shape (CatalogFile).
type catalogFileJSON struct {
	ID            int64      `json:"id"`
	RelPath       string     `json:"relPath"`
	Size          int64      `json:"size"`
	Mtime         time.Time  `json:"mtime"`
	HardlinkGroup *string    `json:"hardlinkGroup"`
	Nlink         uint64     `json:"nlink"`
	Deleted       bool       `json:"deleted"`
	DeletedAt     *time.Time `json:"deletedAt,omitempty"`
}

// MarshalJSON renders the API's CatalogFile: {id, relPath, size, mtime, hardlinkGroup, nlink,
// deleted, deletedAt}.
func (f File) MarshalJSON() ([]byte, error) {
	out := catalogFileJSON{ID: f.ID, RelPath: f.RelPath, Size: f.Size, Mtime: f.Mtime(), Nlink: f.Nlink,
		Deleted: f.DeletedAt != nil, DeletedAt: f.DeletedAt}
	if f.HardlinkGroup != "" {
		g := f.HardlinkGroup
		out.HardlinkGroup = &g
	}
	return json.Marshal(out)
}

const fileColumns = `id, source_id, rel_path, size, mtime_ns, ctime_ns, dev, inode, nlink, hardlink_group, deleted_at, last_seen_at`

func scanFile(r rowScanner) (File, error) {
	var (
		f                File
		dev, ino, nlink  int64
		group, deletedAt sql.NullString
		lastSeen         string
	)
	if err := r.Scan(&f.ID, &f.SourceID, &f.RelPath, &f.Size, &f.MtimeNs, &f.CtimeNs, &dev, &ino, &nlink,
		&group, &deletedAt, &lastSeen); err != nil {
		return File{}, err
	}
	f.Dev, f.Inode, f.Nlink = uint64(dev), uint64(ino), uint64(nlink)
	f.HardlinkGroup = group.String
	if deletedAt.Valid {
		t, err := db.ParseTime(deletedAt.String)
		if err != nil {
			return File{}, fmt.Errorf("catalog file %d: deleted_at: %w", f.ID, err)
		}
		f.DeletedAt = &t
	}
	t, err := db.ParseTime(lastSeen)
	if err != nil {
		return File{}, fmt.Errorf("catalog file %d: last_seen_at: %w", f.ID, err)
	}
	f.LastSeenAt = t
	return f, nil
}

// Filter selects catalog rows in Files.
type Filter string

// Filters.
const (
	// FilterAll is every live file.
	FilterAll Filter = "all"
	// FilterHardlinked is live files in a hardlink group.
	FilterHardlinked Filter = "hardlinked"
	// FilterDeleted is files a scan no longer found.
	FilterDeleted Filter = "deleted"
)

// Page size limits for Files.
const (
	DefaultPageSize = 50
	MaxPageSize     = 500
)

// Query is a Files request. Page is 1-based (< 1 means 1); PageSize < 1 means DefaultPageSize and is
// capped at MaxPageSize; Search is a case-insensitive substring of the relative path; Filter ""
// means FilterAll.
type Query struct {
	Page     int
	PageSize int
	Search   string
	Filter   Filter
}

// FilePage is one page of Files, ordered by relative path.
type FilePage struct {
	Page         int    `json:"page"`
	PageSize     int    `json:"pageSize"`
	TotalRecords int64  `json:"totalRecords"`
	Records      []File `json:"records"`
}

// escapeLike escapes LIKE wildcards so s matches literally (ESCAPE '\').
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// Files returns a page of a source's catalog. An unknown filter is a ValidationError.
func (s *Store) Files(ctx context.Context, sourceID int64, q Query) (FilePage, error) {
	if q.Page < 1 {
		q.Page = 1
	}
	switch {
	case q.PageSize < 1:
		q.PageSize = DefaultPageSize
	case q.PageSize > MaxPageSize:
		q.PageSize = MaxPageSize
	}
	where := `source_id = ?`
	args := []any{sourceID}
	switch q.Filter {
	case "", FilterAll:
		where += ` AND deleted_at IS NULL`
	case FilterHardlinked:
		where += ` AND deleted_at IS NULL AND hardlink_group IS NOT NULL`
	case FilterDeleted:
		where += ` AND deleted_at IS NOT NULL`
	default:
		return FilePage{}, invalid("filter", "must be all, hardlinked or deleted, got %q", string(q.Filter))
	}
	if q.Search != "" {
		where += ` AND rel_path LIKE ? ESCAPE '\'`
		args = append(args, "%"+escapeLike(q.Search)+"%")
	}
	if _, err := s.Get(ctx, sourceID); err != nil {
		return FilePage{}, err
	}
	page := FilePage{Page: q.Page, PageSize: q.PageSize, Records: []File{}}
	r := s.db.Reader()
	if err := r.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_files WHERE `+where, args...).Scan(&page.TotalRecords); err != nil {
		return FilePage{}, fmt.Errorf("count catalog files: %w", err)
	}
	offset := int64(q.Page-1) * int64(q.PageSize)
	if offset >= page.TotalRecords {
		return page, nil
	}
	rows, err := r.QueryContext(ctx, `SELECT `+fileColumns+` FROM catalog_files WHERE `+where+
		` ORDER BY rel_path LIMIT ? OFFSET ?`, append(args, q.PageSize, offset)...)
	if err != nil {
		return FilePage{}, fmt.Errorf("list catalog files: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return FilePage{}, fmt.Errorf("list catalog files: %w", err)
		}
		page.Records = append(page.Records, f)
	}
	if err := rows.Err(); err != nil {
		return FilePage{}, fmt.Errorf("list catalog files: %w", err)
	}
	return page, nil
}

// iterBatch is the keyset page size of Live and Deleted.
const iterBatch = 1000

// iterate calls fn for each row matching where (after "source_id = ? AND"), in rel_path (byte)
// order, reading keyset pages so no read transaction stays open while fn runs.
func (s *Store) iterate(ctx context.Context, sourceID int64, where string, args []any, fn func(File) error) error {
	after := ""
	for {
		qargs := append([]any{sourceID, after}, args...)
		qargs = append(qargs, iterBatch)
		rows, err := s.db.Reader().QueryContext(ctx, `SELECT `+fileColumns+` FROM catalog_files
			WHERE source_id = ? AND rel_path > ? AND `+where+` ORDER BY rel_path LIMIT ?`, qargs...)
		if err != nil {
			return fmt.Errorf("read catalog: %w", err)
		}
		batch := make([]File, 0, iterBatch)
		for rows.Next() {
			f, err := scanFile(rows)
			if err != nil {
				_ = rows.Close()
				return fmt.Errorf("read catalog: %w", err)
			}
			batch = append(batch, f)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return fmt.Errorf("read catalog: %w", err)
		}
		for _, f := range batch {
			if err := fn(f); err != nil {
				return err
			}
		}
		if len(batch) < iterBatch {
			return nil
		}
		after = batch[len(batch)-1].RelPath
	}
}

// Live calls fn for every live file of a source, ordered by RelPath (byte order). An error from fn
// stops the iteration and is returned as is. Hold LockSource (and scan with ScanLocked) to keep
// the catalog stable across a scan and the iteration.
func (s *Store) Live(ctx context.Context, sourceID int64, fn func(File) error) error {
	return s.iterate(ctx, sourceID, `deleted_at IS NULL`, nil, fn)
}

// Deleted calls fn for every file of a source marked deleted at or after since, ordered by
// RelPath. Pass ScanResult.StartedAt to get the files the latest scan found gone; their Dev,
// Inode, Size and MtimeNs are what the previous scan recorded.
func (s *Store) Deleted(ctx context.Context, sourceID int64, since time.Time, fn func(File) error) error {
	return s.iterate(ctx, sourceID, `deleted_at IS NOT NULL AND deleted_at >= ?`, []any{db.FormatTime(since)}, fn)
}

// GlobalStats are the catalog totals over all sources (API: GET /catalog/stats).
type GlobalStats struct {
	Sources         int64 `json:"sources"`
	Files           int64 `json:"files"`
	Bytes           int64 `json:"bytes"`
	UniqueBytes     int64 `json:"uniqueBytes"`
	HardlinkGroups  int64 `json:"hardlinkGroups"`
	HardlinkedFiles int64 `json:"hardlinkedFiles"`
}

// Stats sums the per-source statistics of the last successful scans. Hardlink groups are per
// source, so names of one inode in two sources count in both.
func (s *Store) Stats(ctx context.Context) (GlobalStats, error) {
	srcs, err := s.List(ctx)
	if err != nil {
		return GlobalStats{}, err
	}
	var g GlobalStats
	for _, src := range srcs {
		g.Sources++
		g.Files += src.Stats.Files
		g.Bytes += src.Stats.Bytes
		g.UniqueBytes += src.Stats.UniqueBytes
		g.HardlinkGroups += src.Stats.HardlinkGroups
		g.HardlinkedFiles += src.Stats.HardlinkedFiles
	}
	return g, nil
}

// computeStats counts a source's live catalog rows (inside the scan's final transaction).
func computeStats(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, sourceID int64) (Stats, error) {
	var st Stats
	err := q.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(size), 0), COUNT(hardlink_group),
		COUNT(DISTINCT hardlink_group) FROM catalog_files WHERE source_id = ? AND deleted_at IS NULL`, sourceID).
		Scan(&st.Files, &st.Bytes, &st.HardlinkedFiles, &st.HardlinkGroups)
	if err != nil {
		return st, fmt.Errorf("catalog stats: %w", err)
	}
	var extra int64
	err = q.QueryRowContext(ctx, `SELECT COALESCE(SUM(size * (n - 1)), 0) FROM (
		SELECT MAX(size) AS size, COUNT(*) AS n FROM catalog_files
		WHERE source_id = ? AND deleted_at IS NULL AND hardlink_group IS NOT NULL GROUP BY hardlink_group)`, sourceID).
		Scan(&extra)
	if err != nil {
		return st, fmt.Errorf("catalog stats: %w", err)
	}
	st.UniqueBytes = st.Bytes - extra
	return st, nil
}
