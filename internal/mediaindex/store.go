package mediaindex

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/db"
)

// ExternalIDs are an item's ids in the metadata databases; a zero value is unknown.
type ExternalIDs struct {
	TMDB   int64  `json:"tmdb,omitempty"`
	IMDB   string `json:"imdb,omitempty"`
	TVDB   int64  `json:"tvdb,omitempty"`
	TVMaze int64  `json:"tvmaze,omitempty"`
	// MBID is the MusicBrainz id (Lidarr's foreignArtistId).
	MBID string `json:"mbid,omitempty"`
}

// Item is an indexed *arr item: a Radarr movie, a Sonarr series or a Lidarr artist.
type Item struct {
	ID            int64
	IntegrationID int64
	// Kind is movie, series or artist.
	Kind string
	// ArrID is the item's id in the *arr.
	ArrID       int64
	Title       string
	Year        int
	ExternalIDs ExternalIDs
	// Path and RootFolder are as the *arr sees them (container paths).
	Path       string
	RootFolder string
	// QualityProfileID and MetadataProfileID (Lidarr only) name arr_meta rows.
	QualityProfileID  int64
	MetadataProfileID int64
	Monitored         bool
	// Tags are tag ids; their labels are in Meta.Tags.
	Tags    []int64
	Genres  []string
	AddedAt *time.Time
	// Detail holds the kind-specific fields a manifest round-trips (ItemDetail).
	Detail ItemDetail
	SeenAt time.Time
	// DeletedAt is set when the *arr no longer has the item; such an item supplies no facts.
	DeletedAt *time.Time
}

// ItemDetail are the kind-specific fields of an item (arr_items.detail, design §11.1).
type ItemDetail struct {
	// HasFile is whether the *arr reports files for the item (hasFile, movieFileId ≠ 0,
	// episodeFileCount > 0, trackFileCount > 0).
	HasFile bool `json:"hasFile"`
	// Radarr.
	MinimumAvailability string `json:"minimumAvailability,omitempty"`
	// Sonarr.
	SeriesType        string          `json:"seriesType,omitempty"`
	SeasonFolder      *bool           `json:"seasonFolder,omitempty"`
	MonitorNewItems   string          `json:"monitorNewItems,omitempty"`
	UseSceneNumbering *bool           `json:"useSceneNumbering,omitempty"`
	LanguageProfileID int64           `json:"languageProfileId,omitempty"`
	Seasons           []SeasonDetail  `json:"seasons,omitempty"`
	Episodes          []EpisodeDetail `json:"episodes,omitempty"`
	// Lidarr.
	Albums []AlbumDetail `json:"albums,omitempty"`
}

// SeasonDetail is a season of a Sonarr series.
type SeasonDetail struct {
	SeasonNumber int  `json:"seasonNumber"`
	Monitored    bool `json:"monitored"`
}

// EpisodeDetail is an episode of a Sonarr series.
type EpisodeDetail struct {
	Season    int  `json:"season"`
	Episode   int  `json:"episode"`
	Monitored bool `json:"monitored"`
}

// AlbumDetail is an album of a Lidarr artist.
type AlbumDetail struct {
	ID        int64  `json:"id"`
	MBID      string `json:"mbid"`
	Title     string `json:"title"`
	Monitored bool   `json:"monitored"`
}

// File is an indexed *arr file (a movie, episode or track file).
type File struct {
	ID            int64
	IntegrationID int64
	// ItemID is the arr_items row id (not the *arr's id).
	ItemID int64
	// ArrFileID is the *arr's file id; a same-path replacement gets a new one.
	ArrFileID int64
	// Path is as the *arr sees it.
	Path string
	// LocalPath is Path through the path mappings; "" when no mapping applies.
	LocalPath string
	// Location is the longest-prefix source that contains LocalPath; nil when none does.
	Location *catalog.Location
	Size     int64
	// Quality is the quality name ("Bluray-1080p").
	Quality   string
	DateAdded *time.Time
	Detail    FileDetail
	SeenAt    time.Time
}

// FileDetail are the kind-specific fields of a file (arr_files.detail).
type FileDetail struct {
	// RelativePath is the path inside the item's folder (Lidarr: computed from the path).
	RelativePath string `json:"relativePath,omitempty"`
	// Episodes are the episodes a Sonarr file holds (several for a multi-episode file).
	Episodes []FileEpisode `json:"episodes,omitempty"`
	// Album is the Lidarr album of a track file.
	Album *FileAlbum `json:"album,omitempty"`
}

// FileEpisode is an episode held by a Sonarr episode file.
type FileEpisode struct {
	EpisodeID     int64 `json:"episodeId"`
	SeasonNumber  int   `json:"seasonNumber"`
	EpisodeNumber int   `json:"episodeNumber"`
	TVDBID        int64 `json:"tvdbId,omitempty"`
}

// FileAlbum is the album of a Lidarr track file.
type FileAlbum struct {
	ID    int64  `json:"id"`
	MBID  string `json:"mbid,omitempty"`
	Title string `json:"title,omitempty"`
}

// Named is a quality or metadata profile.
type Named struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// TagLabel is an *arr tag.
type TagLabel struct {
	ID    int64  `json:"id"`
	Label string `json:"label"`
}

// RootFolder is an *arr root folder, mapped and located when the index was refreshed.
type RootFolder struct {
	ID int64 `json:"id"`
	// Path is as the *arr sees it.
	Path       string `json:"path"`
	Accessible bool   `json:"accessible"`
	// LocalPath is Path through the path mappings (nil when unmapped); SourceID the source it
	// lies in (nil when none).
	LocalPath *string `json:"localPath"`
	SourceID  *int64  `json:"sourceId"`
}

// Meta is an *arr's metadata as indexed (GET /integrations/{id}/arr/metadata).
type Meta struct {
	QualityProfiles  []Named      `json:"qualityProfiles"`
	MetadataProfiles []Named      `json:"metadataProfiles"`
	RootFolders      []RootFolder `json:"rootFolders"`
	Tags             []TagLabel   `json:"tags"`
	// RefreshedAt is the last complete refresh (nil before the first).
	RefreshedAt *time.Time `json:"refreshedAt"`
}

// Meta kinds (arr_meta.kind).
const (
	metaQualityProfile  = "quality_profile"
	metaMetadataProfile = "metadata_profile"
	metaRootFolder      = "root_folder"
	metaTag             = "tag"
)

// rootFolderDetail is arr_meta.detail of a root folder.
type rootFolderDetail struct {
	Accessible bool    `json:"accessible"`
	LocalPath  *string `json:"localPath"`
	SourceID   *int64  `json:"sourceId"`
}

// Meta returns an integration's indexed metadata. q may be nil (the read pool).
func (s *Store) Meta(ctx context.Context, q Queryer, integrationID int64) (Meta, error) {
	q = orReader(s, q)
	out := Meta{QualityProfiles: []Named{}, MetadataProfiles: []Named{}, RootFolders: []RootFolder{}, Tags: []TagLabel{}}
	rows, err := q.QueryContext(ctx, `SELECT kind, arr_id, name, detail FROM arr_meta WHERE integration_id = ? ORDER BY kind, arr_id`, integrationID)
	if err != nil {
		return Meta{}, fmt.Errorf("read the metadata of integration %d: %w", integrationID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			kind, name, detail string
			id                 int64
		)
		if err := rows.Scan(&kind, &id, &name, &detail); err != nil {
			return Meta{}, fmt.Errorf("read the metadata of integration %d: %w", integrationID, err)
		}
		switch kind {
		case metaQualityProfile:
			out.QualityProfiles = append(out.QualityProfiles, Named{ID: id, Name: name})
		case metaMetadataProfile:
			out.MetadataProfiles = append(out.MetadataProfiles, Named{ID: id, Name: name})
		case metaTag:
			out.Tags = append(out.Tags, TagLabel{ID: id, Label: name})
		case metaRootFolder:
			var d rootFolderDetail
			_ = json.Unmarshal([]byte(detail), &d)
			out.RootFolders = append(out.RootFolders, RootFolder{ID: id, Path: name, Accessible: d.Accessible, LocalPath: d.LocalPath, SourceID: d.SourceID})
		}
	}
	if err := rows.Err(); err != nil {
		return Meta{}, fmt.Errorf("read the metadata of integration %d: %w", integrationID, err)
	}
	st, err := s.State(ctx, q, integrationID)
	if err != nil {
		return Meta{}, err
	}
	out.RefreshedAt = st.RefreshedAt
	return out, nil
}

const itemColumns = `i.id, i.integration_id, i.kind, i.arr_id, i.title, i.year, i.external_ids, i.path, i.root_folder,
	i.quality_profile_id, i.metadata_profile_id, i.monitored, i.tags, i.genres, i.added_at, i.detail, i.seen_at, i.deleted_at`

func scanItem(r rowScanner) (Item, error) {
	var (
		it                        Item
		ext, tags, genres, detail string
		added, deleted            sql.NullString
		seen                      string
		monitored                 int64
	)
	if err := r.Scan(&it.ID, &it.IntegrationID, &it.Kind, &it.ArrID, &it.Title, &it.Year, &ext, &it.Path, &it.RootFolder,
		&it.QualityProfileID, &it.MetadataProfileID, &monitored, &tags, &genres, &added, &detail, &seen, &deleted); err != nil {
		return Item{}, err
	}
	it.Monitored = monitored != 0
	if err := json.Unmarshal([]byte(ext), &it.ExternalIDs); err != nil {
		return Item{}, fmt.Errorf("item %d: external_ids: %w", it.ID, err)
	}
	if err := json.Unmarshal([]byte(tags), &it.Tags); err != nil {
		return Item{}, fmt.Errorf("item %d: tags: %w", it.ID, err)
	}
	if err := json.Unmarshal([]byte(genres), &it.Genres); err != nil {
		return Item{}, fmt.Errorf("item %d: genres: %w", it.ID, err)
	}
	if err := json.Unmarshal([]byte(detail), &it.Detail); err != nil {
		return Item{}, fmt.Errorf("item %d: detail: %w", it.ID, err)
	}
	if it.Tags == nil {
		it.Tags = []int64{}
	}
	if it.Genres == nil {
		it.Genres = []string{}
	}
	var err error
	if it.AddedAt, err = parseOptTime(added); err != nil {
		return Item{}, fmt.Errorf("item %d: added_at: %w", it.ID, err)
	}
	if it.DeletedAt, err = parseOptTime(deleted); err != nil {
		return Item{}, fmt.Errorf("item %d: deleted_at: %w", it.ID, err)
	}
	if it.SeenAt, err = db.ParseTime(seen); err != nil {
		return Item{}, fmt.Errorf("item %d: seen_at: %w", it.ID, err)
	}
	return it, nil
}

const fileColumns = `f.id, f.integration_id, f.item_id, f.arr_file_id, f.path, f.local_path, f.source_id, f.rel_path, f.size,
	f.quality, f.date_added, f.detail, f.seen_at`

func scanFile(r rowScanner) (File, error) {
	var (
		f                 File
		local, rel, added sql.NullString
		src               sql.NullInt64
		detail, seen      string
	)
	if err := r.Scan(&f.ID, &f.IntegrationID, &f.ItemID, &f.ArrFileID, &f.Path, &local, &src, &rel, &f.Size,
		&f.Quality, &added, &detail, &seen); err != nil {
		return File{}, err
	}
	f.LocalPath = local.String
	if src.Valid {
		f.Location = &catalog.Location{SourceID: src.Int64, Rel: rel.String}
	}
	if err := json.Unmarshal([]byte(detail), &f.Detail); err != nil {
		return File{}, fmt.Errorf("file %d: detail: %w", f.ID, err)
	}
	var err error
	if f.DateAdded, err = parseOptTime(added); err != nil {
		return File{}, fmt.Errorf("file %d: date_added: %w", f.ID, err)
	}
	if f.SeenAt, err = db.ParseTime(seen); err != nil {
		return File{}, fmt.Errorf("file %d: seen_at: %w", f.ID, err)
	}
	return f, nil
}

// each runs a query and calls scan then fn for every row.
func each[T any](ctx context.Context, q Queryer, what string, scan func(rowScanner) (T, error), fn func(T) error, query string, args ...any) error {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("read %s: %w", what, err)
	}
	defer rows.Close()
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return fmt.Errorf("read %s: %w", what, err)
		}
		if err := fn(v); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read %s: %w", what, err)
	}
	return nil
}

// EachItem calls fn for every item of an integration (0: of every *arr integration), ordered by
// integration, kind and *arr id. Deleted items are included only with includeDeleted. q may be
// nil (the read pool). It stops at fn's first error and returns it.
func (s *Store) EachItem(ctx context.Context, q Queryer, integrationID int64, includeDeleted bool, fn func(Item) error) error {
	query := `SELECT ` + itemColumns + ` FROM arr_items i WHERE (? = 0 OR i.integration_id = ?)`
	if !includeDeleted {
		query += ` AND i.deleted_at IS NULL`
	}
	query += ` ORDER BY i.integration_id, i.kind, i.arr_id`
	return each(ctx, orReader(s, q), "indexed items", scanItem, fn, query, integrationID, integrationID)
}

// ErrItemNotFound means the index has no such item.
var ErrItemNotFound = errors.New("item not found in the index")

// Item returns an item by its *arr id (deleted or not), or ErrItemNotFound. q may be nil.
func (s *Store) Item(ctx context.Context, q Queryer, integrationID int64, kind string, arrID int64) (Item, error) {
	it, err := scanItem(orReader(s, q).QueryRowContext(ctx, `SELECT `+itemColumns+` FROM arr_items i
		WHERE i.integration_id = ? AND i.kind = ? AND i.arr_id = ?`, integrationID, kind, arrID))
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, ErrItemNotFound
	}
	if err != nil {
		return Item{}, fmt.Errorf("read item %s %d of integration %d: %w", kind, arrID, integrationID, err)
	}
	return it, nil
}

// EachFile calls fn for every file of an integration's items (0: of every *arr integration),
// ordered by item and file id; files of deleted items only with includeDeleted. q may be nil.
func (s *Store) EachFile(ctx context.Context, q Queryer, integrationID int64, includeDeleted bool, fn func(File) error) error {
	query := `SELECT ` + fileColumns + ` FROM arr_files f JOIN arr_items i ON i.id = f.item_id WHERE (? = 0 OR f.integration_id = ?)`
	if !includeDeleted {
		query += ` AND i.deleted_at IS NULL`
	}
	query += ` ORDER BY f.integration_id, f.item_id, f.arr_file_id`
	return each(ctx, orReader(s, q), "indexed files", scanFile, fn, query, integrationID, integrationID)
}

// FilesOfItem returns the files of one item (its arr_items id), by *arr file id. q may be nil.
func (s *Store) FilesOfItem(ctx context.Context, q Queryer, itemID int64) ([]File, error) {
	out := []File{}
	err := each(ctx, orReader(s, q), "indexed files", scanFile, func(f File) error {
		out = append(out, f)
		return nil
	}, `SELECT `+fileColumns+` FROM arr_files f WHERE f.item_id = ? ORDER BY f.arr_file_id`, itemID)
	return out, err
}

// FilesUnder calls fn for the files of non-deleted items whose mapped local path is localPath or
// lies under it (a source's path: every file a catalog file of that source could match, whatever
// source the row's location names, since sources may overlap), ordered by local path. Deleted
// items supply no facts, so their files are left out. q may be nil.
func (s *Store) FilesUnder(ctx context.Context, q Queryer, localPath string, fn func(File) error) error {
	lo, hi := likePrefix(localPath)
	return each(ctx, orReader(s, q), "indexed files", scanFile, fn, `SELECT `+fileColumns+` FROM arr_files f
		JOIN arr_items i ON i.id = f.item_id AND i.deleted_at IS NULL
		WHERE f.local_path = ? OR (f.local_path >= ? AND f.local_path < ?)
		ORDER BY f.local_path, f.integration_id, f.arr_file_id`, localPath, lo, hi)
}

// FilesAt returns the files of non-deleted items whose mapped local path is exactly localPath:
// usually one, several when two *arr integrations claim the file (design D12). q may be nil.
func (s *Store) FilesAt(ctx context.Context, q Queryer, localPath string) ([]File, error) {
	out := []File{}
	err := each(ctx, orReader(s, q), "indexed files", scanFile, func(f File) error {
		out = append(out, f)
		return nil
	}, `SELECT `+fileColumns+` FROM arr_files f JOIN arr_items i ON i.id = f.item_id AND i.deleted_at IS NULL
		WHERE f.local_path = ? ORDER BY f.integration_id, f.arr_file_id`, localPath)
	return out, err
}

// ItemsByID returns items by their arr_items ids (deleted or not); ids not found are absent.
func (s *Store) ItemsByID(ctx context.Context, q Queryer, ids []int64) (map[int64]Item, error) {
	out := make(map[int64]Item, len(ids))
	q = orReader(s, q)
	for len(ids) > 0 {
		n := min(len(ids), 500)
		chunk := ids[:n]
		ids = ids[n:]
		args := make([]any, n)
		for i, id := range chunk {
			args[i] = id
		}
		err := each(ctx, q, "indexed items", scanItem, func(it Item) error {
			out[it.ID] = it
			return nil
		}, `SELECT `+itemColumns+` FROM arr_items i WHERE i.id IN (`+placeholders(n)+`)`, args...)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, 0, 2*n)
	for i := range n {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '?')
	}
	return string(b)
}

// Unmapped reasons (GET /catalog/unmapped).
const (
	// ReasonUnmapped: no path mapping of the integration covers the path.
	ReasonUnmapped = "unmapped"
	// ReasonNoSource: the mapped path lies in no source.
	ReasonNoSource = "no-source"
	// ReasonMismatched: no live catalog file has the mapped path and the *arr's size (not
	// scanned yet, or a wrong mapping), so the row supplies no facts (S18).
	ReasonMismatched = "mismatched"
)

// UnmappedFile is a file of another application that does not apply to a catalog file.
type UnmappedFile struct {
	IntegrationID int64 `json:"integrationId"`
	// Path is as the application sees it; LocalPath through the path mappings (absent when
	// unmapped).
	Path      string  `json:"path"`
	LocalPath *string `json:"localPath,omitempty"`
	// Size is the size the application reports.
	Size int64 `json:"size"`
	// Reason is unmapped, no-source or mismatched.
	Reason string `json:"reason"`
	// CatalogSize is the size of the catalog file at the path when the sizes differ.
	CatalogSize *int64 `json:"catalogSize,omitempty"`
}

// UnmappedPage is one page of Unmapped (the API's Paged shape).
type UnmappedPage struct {
	Page         int            `json:"page"`
	PageSize     int            `json:"pageSize"`
	TotalRecords int64          `json:"totalRecords"`
	Records      []UnmappedFile `json:"records"`
}

// Unmapped lists the files of non-deleted items of an integration (0: of every *arr integration)
// that do not apply to a catalog file: unmapped, in no source, or mismatched (no live catalog
// file at the mapped path, in any source that contains it, with the *arr's size). It is ordered
// by reason (unmapped, no-source, mismatched), then path. page is 1-based.
//
// The unmapped and no-source groups are counted and paged in SQL. Only the located files whose
// recorded location (still the source's path joined with the relative path) has no live catalog
// file of the *arr's size are checked against the catalog (every containing source, since sources
// may overlap), in chunks, keeping only the page's rows.
func (s *Store) Unmapped(ctx context.Context, integrationID int64, page, pageSize int) (UnmappedPage, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 50
	}
	out := UnmappedPage{Page: page, PageSize: pageSize, Records: []UnmappedFile{}}
	// The page is the rows [start, end) of the whole ordered listing.
	start := (int64(page) - 1) * int64(pageSize)
	end := start + int64(pageSize)
	q := s.db.Reader()
	const scope = ` FROM arr_files f JOIN arr_items i ON i.id = f.item_id AND i.deleted_at IS NULL
		WHERE (? = 0 OR f.integration_id = ?) AND `
	for _, g := range []struct{ reason, cond string }{
		{ReasonUnmapped, `f.local_path IS NULL`},
		{ReasonNoSource, `f.local_path IS NOT NULL AND f.source_id IS NULL`},
	} {
		var n int64
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*)`+scope+g.cond, integrationID, integrationID).Scan(&n); err != nil {
			return out, fmt.Errorf("count unmapped files: %w", err)
		}
		base := out.TotalRecords
		out.TotalRecords += n
		from, to := max(start, base), min(end, base+n)
		if from >= to {
			continue
		}
		err := each(ctx, q, "unmapped files", scanFile, func(f File) error {
			u := UnmappedFile{IntegrationID: f.IntegrationID, Path: f.Path, Size: f.Size, Reason: g.reason}
			if f.LocalPath != "" {
				lp := f.LocalPath
				u.LocalPath = &lp
			}
			out.Records = append(out.Records, u)
			return nil
		}, `SELECT `+fileColumns+scope+g.cond+` ORDER BY f.path, f.id LIMIT ? OFFSET ?`,
			integrationID, integrationID, to-from, from-base)
		if err != nil {
			return out, err
		}
	}
	if s.catalog == nil {
		return out, nil
	}
	base := out.TotalRecords
	var n int64
	err := s.mismatched(ctx, integrationID, func(u UnmappedFile) {
		if i := base + n; i >= start && i < end {
			out.Records = append(out.Records, u)
		}
		n++
	})
	if err != nil {
		return out, err
	}
	out.TotalRecords += n
	return out, nil
}

// mismatchedChunk bounds the candidate files mismatched checks against the catalog at once.
const mismatchedChunk = 500

// mismatchedQuery reads the next chunk of candidate files after (path, id). The keyset is a row
// value that the index arr_files_path (path, id) serves: SQLite plans each chunk as a range scan
// of it that starts where the last one ended, so listing every candidate reads each arr_files row
// once (TestUnmappedMismatchedQueryIsARangeScan). Without that index it scans and sorts every
// chunk. The index is not forced with INDEXED BY: a database that applied the earlier text of
// migration 0003 lacks it, and there INDEXED BY fails every listing with "no such index".
const mismatchedQuery = `SELECT ` + fileColumns + ` FROM arr_files f
		JOIN arr_items i ON i.id = f.item_id AND i.deleted_at IS NULL
		WHERE (f.path, f.id) > (?, ?) AND (? = 0 OR f.integration_id = ?)
			AND f.local_path IS NOT NULL AND f.source_id IS NOT NULL
			AND NOT (
				EXISTS (SELECT 1 FROM sources s WHERE s.id = f.source_id AND f.local_path =
					CASE WHEN f.rel_path = '' THEN s.path WHEN s.path = '/' THEN '/' || f.rel_path ELSE s.path || '/' || f.rel_path END)
				AND EXISTS (SELECT 1 FROM catalog_files c WHERE c.source_id = f.source_id AND c.rel_path = f.rel_path
					AND c.deleted_at IS NULL AND c.size = f.size))
		ORDER BY f.path, f.id LIMIT ?`

// mismatched calls fn, ordered by path, for every located file of non-deleted items of an
// integration (0: of every *arr integration) that no live catalog file matches (the path in any
// containing source, and the size). A file whose recorded location is still the source's path
// joined with the relative path, and holds a live catalog file of its size, matches without
// further checks; the others (few: the mismatched ones) are checked against the catalog.
func (s *Store) mismatched(ctx context.Context, integrationID int64, fn func(UnmappedFile)) error {
	loc, err := s.catalog.Locator(ctx)
	if err != nil {
		return err
	}
	q := s.db.Reader()
	var (
		afterPath string
		afterID   int64 = -1
	)
	for {
		var chunk []File
		err := each(ctx, q, "located files", scanFile, func(f File) error {
			chunk = append(chunk, f)
			return nil
		}, mismatchedQuery, afterPath, afterID, integrationID, integrationID, mismatchedChunk)
		if err != nil {
			return err
		}
		if len(chunk) == 0 {
			return nil
		}
		last := chunk[len(chunk)-1]
		afterPath, afterID = last.Path, last.ID
		s.matchChecks.Add(int64(len(chunk)))
		m, err := newMatcher(ctx, s.catalog, loc, chunk)
		if err != nil {
			return err
		}
		for _, f := range chunk {
			ok, catSize := m.match(f.LocalPath, f.Size)
			if ok {
				continue
			}
			lp := f.LocalPath
			u := UnmappedFile{IntegrationID: f.IntegrationID, Path: f.Path, LocalPath: &lp, Size: f.Size, Reason: ReasonMismatched}
			if catSize >= 0 {
				cs := catSize
				u.CatalogSize = &cs
			}
			fn(u)
		}
		if len(chunk) < mismatchedChunk {
			return nil
		}
	}
}

// matcher answers whether a live catalog file has a local path and size (design S18: an *arr file
// applies to a catalog file only when both are equal).
type matcher struct {
	loc  *catalog.Locator
	live map[catalog.Location]catalog.LiveFile
}

func newMatcher(ctx context.Context, cat *catalog.Store, loc *catalog.Locator, files []File) (*matcher, error) {
	var keys []catalog.Location
	for _, f := range files {
		if f.LocalPath != "" {
			keys = append(keys, loc.Locate(f.LocalPath)...)
		}
	}
	live, err := cat.LiveFilesAt(ctx, nil, keys)
	if err != nil {
		return nil, err
	}
	return &matcher{loc: loc, live: live}, nil
}

// match reports whether a live catalog file at localPath (in any source that contains it) has
// size; catSize is the size of a live file found there with another size (-1 when none).
func (m *matcher) match(localPath string, size int64) (ok bool, catSize int64) {
	catSize = -1
	for _, l := range m.loc.Locate(localPath) {
		if f, found := m.live[l]; found {
			if f.Size == size {
				return true, f.Size
			}
			catSize = f.Size
		}
	}
	return false, catSize
}
