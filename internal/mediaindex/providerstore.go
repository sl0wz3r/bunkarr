package mediaindex

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
)

// Queries over the provider caches (the Plex library index, Tautulli, Seerr and Maintainerr), for
// the tier facts (internal/tiers) and the API. q may be nil (the read pool).

// PlexSection is a plex_sections row.
type PlexSection struct {
	IntegrationID int64             `json:"integrationId"`
	Key           string            `json:"key"`
	Title         string            `json:"title"`
	Type          string            `json:"type"`
	Locations     []SectionLocation `json:"locations"`
}

// PlexSections returns a Plex integration's indexed sections, by key.
func (s *Store) PlexSections(ctx context.Context, q Queryer, plexID int64) ([]PlexSection, error) {
	rows, err := orReader(s, q).QueryContext(ctx, `SELECT section_key, title, type, locations FROM plex_sections WHERE integration_id = ?
		ORDER BY CAST(section_key AS INTEGER), section_key`, plexID)
	if err != nil {
		return nil, fmt.Errorf("read the Plex sections: %w", err)
	}
	defer rows.Close()
	out := []PlexSection{}
	for rows.Next() {
		sec := PlexSection{IntegrationID: plexID}
		var locs string
		if err := rows.Scan(&sec.Key, &sec.Title, &sec.Type, &locs); err != nil {
			return nil, fmt.Errorf("read the Plex sections: %w", err)
		}
		if json.Unmarshal([]byte(locs), &sec.Locations) != nil || sec.Locations == nil {
			sec.Locations = []SectionLocation{}
		}
		out = append(out, sec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the Plex sections: %w", err)
	}
	return out, nil
}

// PlexItem is a plex_items row.
type PlexItem struct {
	IntegrationID  int64
	RatingKey      string
	Type           string
	SectionKey     string
	ParentKey      string
	GrandparentKey string
	// Index and ParentIndex: a season's number; an episode's number and its season's.
	Index       *int
	ParentIndex *int
	GUID        string
	// ExternalIDs are Guid[] ids by scheme: imdb, tmdb, tvdb.
	ExternalIDs map[string]string
	Title       string
	AddedAt     *time.Time
}

const plexItemColumns = `integration_id, rating_key, type, section_key, parent_key, grandparent_key, item_index, parent_index, guid, external_ids, title, added_at`

// EachPlexItem calls fn for every indexed item of a Plex integration.
func (s *Store) EachPlexItem(ctx context.Context, q Queryer, plexID int64, fn func(PlexItem) error) error {
	return each(ctx, orReader(s, q), "the Plex items", scanPlexItem, fn,
		`SELECT `+plexItemColumns+` FROM plex_items WHERE integration_id = ?`, plexID)
}

// plexKeysUnder selects the rating keys of Plex integration ?1's files at or under a local path
// (?2, with the range ?3..?4): the argument order of plexUnderArgs.
const plexKeysUnder = `SELECT rating_key FROM plex_files WHERE integration_id = ?1 AND (local_path = ?2 OR (local_path >= ?3 AND local_path < ?4))`

func plexUnderArgs(plexID int64, localPath string) []any {
	lo, hi := likePrefix(localPath)
	return []any{plexID, localPath, lo, hi}
}

// PlexItemsUnder calls fn for the items of a Plex integration that have a file at or under
// localPath, and for their parents and grandparents (an episode's season and show, a track's album
// and artist): what the tier facts of one source's files read, instead of every item of the
// server once per source.
func (s *Store) PlexItemsUnder(ctx context.Context, q Queryer, plexID int64, localPath string, fn func(PlexItem) error) error {
	return each(ctx, orReader(s, q), "the Plex items", scanPlexItem, fn,
		`WITH own(rating_key) AS (`+plexKeysUnder+`),
		keys(rating_key) AS (
			SELECT rating_key FROM own
			UNION SELECT parent_key FROM plex_items WHERE integration_id = ?1 AND rating_key IN (SELECT rating_key FROM own) AND parent_key IS NOT NULL
			UNION SELECT grandparent_key FROM plex_items WHERE integration_id = ?1 AND rating_key IN (SELECT rating_key FROM own) AND grandparent_key IS NOT NULL)
		SELECT `+plexItemColumns+` FROM plex_items WHERE integration_id = ?1 AND rating_key IN (SELECT rating_key FROM keys)`, plexUnderArgs(plexID, localPath)...)
}

// PlexGUIDKey is an item's guid and rating key.
type PlexGUIDKey struct {
	GUID, RatingKey string
}

// PlexGUIDKeysUnder calls fn for every item of a Plex integration that shares a guid with an item
// that has a file at or under localPath (those items included): the live items of each guid.
func (s *Store) PlexGUIDKeysUnder(ctx context.Context, q Queryer, plexID int64, localPath string, fn func(PlexGUIDKey) error) error {
	return each(ctx, orReader(s, q), "the Plex items", func(r rowScanner) (PlexGUIDKey, error) {
		var k PlexGUIDKey
		err := r.Scan(&k.GUID, &k.RatingKey)
		return k, err
	}, fn,
		`WITH guids(guid) AS (SELECT DISTINCT guid FROM plex_items WHERE integration_id = ?1 AND guid <> '' AND rating_key IN (`+plexKeysUnder+`))
		SELECT guid, rating_key FROM plex_items WHERE integration_id = ?1 AND guid IN (SELECT guid FROM guids)`, plexUnderArgs(plexID, localPath)...)
}

// PlexItemKnown reports whether a Plex integration's index has an item with this rating key.
func (s *Store) PlexItemKnown(ctx context.Context, q Queryer, plexID int64, ratingKey string) (bool, error) {
	var n int
	err := orReader(s, q).QueryRowContext(ctx, `SELECT count(*) FROM plex_items WHERE integration_id = ? AND rating_key = ?`, plexID, ratingKey).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("read the Plex items: %w", err)
	}
	return n > 0, nil
}

func scanPlexItem(r rowScanner) (PlexItem, error) {
	var (
		it              PlexItem
		parent, grand   sql.NullString
		index, pindex   sql.NullInt64
		ids             string
		added           sql.NullString
		err             error
		indexV, pindexV int
	)
	if err = r.Scan(&it.IntegrationID, &it.RatingKey, &it.Type, &it.SectionKey, &parent, &grand, &index, &pindex, &it.GUID, &ids, &it.Title, &added); err != nil {
		return it, err
	}
	it.ParentKey, it.GrandparentKey = parent.String, grand.String
	if index.Valid {
		indexV = int(index.Int64)
		it.Index = &indexV
	}
	if pindex.Valid {
		pindexV = int(pindex.Int64)
		it.ParentIndex = &pindexV
	}
	if json.Unmarshal([]byte(ids), &it.ExternalIDs) != nil || it.ExternalIDs == nil {
		it.ExternalIDs = map[string]string{}
	}
	if it.AddedAt, err = parseOptTime(added); err != nil {
		return it, fmt.Errorf("added_at: %w", err)
	}
	return it, nil
}

// PlexFile is a plex_files row.
type PlexFile struct {
	IntegrationID int64
	RatingKey     string
	// File is as Plex sees it; LocalPath through the path mappings ("" when unmapped).
	File      string
	LocalPath string
	// Location is the longest-prefix source that contains LocalPath; nil when none does.
	Location *catalog.Location
}

// PlexFilesUnder calls fn for every Plex integration's indexed file whose mapped local path is
// localPath or lies under it (every server's: a file may be in several).
func (s *Store) PlexFilesUnder(ctx context.Context, q Queryer, localPath string, fn func(PlexFile) error) error {
	lo, hi := likePrefix(localPath)
	return each(ctx, orReader(s, q), "the Plex files", scanPlexFile, fn,
		`SELECT integration_id, rating_key, file, local_path, source_id, rel_path FROM plex_files
		WHERE local_path = ? OR (local_path >= ? AND local_path < ?)`, localPath, lo, hi)
}

// EachPlexFile calls fn for every indexed file of a Plex integration.
func (s *Store) EachPlexFile(ctx context.Context, q Queryer, plexID int64, fn func(PlexFile) error) error {
	return each(ctx, orReader(s, q), "the Plex files", scanPlexFile, fn,
		`SELECT integration_id, rating_key, file, local_path, source_id, rel_path FROM plex_files WHERE integration_id = ?`, plexID)
}

func scanPlexFile(r rowScanner) (PlexFile, error) {
	var (
		f     PlexFile
		local sql.NullString
		src   sql.NullInt64
		rel   sql.NullString
	)
	if err := r.Scan(&f.IntegrationID, &f.RatingKey, &f.File, &local, &src, &rel); err != nil {
		return f, err
	}
	f.LocalPath = local.String
	if src.Valid {
		f.Location = &catalog.Location{SourceID: src.Int64, Rel: rel.String}
	}
	return f, nil
}

// WatchStat is a watch_stats row: the plays of a Plex rating key or guid.
type WatchStat struct {
	// KeyType is rating_key or guid.
	KeyType     string
	Key         string
	Plays       int64
	LastWatched *time.Time
}

// EachWatchStat calls fn for every row of a Tautulli integration.
func (s *Store) EachWatchStat(ctx context.Context, q Queryer, tautulliID int64, fn func(WatchStat) error) error {
	return each(ctx, orReader(s, q), "the play counts", func(r rowScanner) (WatchStat, error) {
		var (
			w    WatchStat
			last sql.NullString
			err  error
		)
		if err = r.Scan(&w.KeyType, &w.Key, &w.Plays, &last); err != nil {
			return w, err
		}
		w.LastWatched, err = parseOptTime(last)
		return w, err
	}, fn, `SELECT key_type, key, plays, last_watched_at FROM watch_stats WHERE integration_id = ?`, tautulliID)
}

// SeerrRequest is a seerr_requests row.
type SeerrRequest struct {
	RequestID int64
	Status    int
	// MediaType is movie or tv.
	MediaType string
	TMDBID    int64
	TVDBID    int64
	RatingKey string
	Is4K      bool
	// Seasons are the requested seasons (tv); empty covers every season.
	Seasons     []int
	UserID      int64
	RequestedAt *time.Time
}

// EachSeerrRequest calls fn for every request of a Seerr integration.
func (s *Store) EachSeerrRequest(ctx context.Context, q Queryer, seerrID int64, fn func(SeerrRequest) error) error {
	return each(ctx, orReader(s, q), "the Seerr requests", func(r rowScanner) (SeerrRequest, error) {
		var (
			x          SeerrRequest
			tmdb, tvdb sql.NullInt64
			seasons    string
			rk         sql.NullString
			at         sql.NullString
			err        error
		)
		if err = r.Scan(&x.RequestID, &x.Status, &x.MediaType, &tmdb, &tvdb, &rk, &x.Is4K, &seasons, &x.UserID, &at); err != nil {
			return x, err
		}
		x.TMDBID, x.TVDBID, x.RatingKey = tmdb.Int64, tvdb.Int64, rk.String
		if json.Unmarshal([]byte(seasons), &x.Seasons) != nil || x.Seasons == nil {
			x.Seasons = []int{}
		}
		x.RequestedAt, err = parseOptTime(at)
		return x, err
	}, fn, `SELECT request_id, status, media_type, tmdb_id, tvdb_id, rating_key, is_4k, seasons, user_id, requested_at
		FROM seerr_requests WHERE integration_id = ? ORDER BY request_id`, seerrID)
}

// MaintainerrItem is a maintainerr_items row: a member that is pending deletion or undecided.
type MaintainerrItem struct {
	PlexIntegrationID int64
	CollectionID      int64
	CollectionTitle   string
	LibraryID         string
	// Level is movie, show, season or episode.
	Level     string
	RatingKey string
	TMDBID    int64
	TVDBID    int64
	Season    *int
	Episode   *int
	// State is pending or undecided.
	State       string
	DeleteAfter *time.Time
}

// EachMaintainerrItem calls fn for every row of a Maintainerr integration.
func (s *Store) EachMaintainerrItem(ctx context.Context, q Queryer, maintainerrID int64, fn func(MaintainerrItem) error) error {
	return each(ctx, orReader(s, q), "the Maintainerr items", func(r rowScanner) (MaintainerrItem, error) {
		var (
			m               MaintainerrItem
			tmdb, tvdb      sql.NullInt64
			season, episode sql.NullInt64
			after           sql.NullString
			err             error
		)
		if err = r.Scan(&m.PlexIntegrationID, &m.CollectionID, &m.CollectionTitle, &m.LibraryID, &m.Level, &m.RatingKey, &tmdb, &tvdb,
			&season, &episode, &m.State, &after); err != nil {
			return m, err
		}
		m.TMDBID, m.TVDBID = tmdb.Int64, tvdb.Int64
		if season.Valid {
			v := int(season.Int64)
			m.Season = &v
		}
		if episode.Valid {
			v := int(episode.Int64)
			m.Episode = &v
		}
		m.DeleteAfter, err = parseOptTime(after)
		return m, err
	}, fn, `SELECT plex_integration_id, collection_id, collection_title, library_id, level, rating_key, tmdb_id, tvdb_id, season_number,
		episode_number, state, delete_after FROM maintainerr_items WHERE integration_id = ? ORDER BY collection_id, rating_key`, maintainerrID)
}

// StoredPlexLink returns the plexIntegrationId a Tautulli, Seerr or Maintainerr cache was built
// for (index_state.stats.plexIntegrationId; 0 when none is recorded).
func (s *Store) StoredPlexLink(ctx context.Context, q Queryer, integrationID int64) (int64, error) {
	st, err := s.State(ctx, q, integrationID)
	if err != nil {
		return 0, err
	}
	var v struct {
		PlexIntegrationID int64 `json:"plexIntegrationId"`
	}
	_ = json.Unmarshal(st.Stats, &v)
	return v.PlexIntegrationID, nil
}
