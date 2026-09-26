// Package mediaindex is Bunkarr's metadata index: what other applications know about the
// library, cached for tier facts, scan targets, the Library item view and manifests
// (docs/design/phase2-3.md §6). It owns the tables index_state, arr_items, arr_files and arr_meta
// (and, for the later Plex, Tautulli, Seerr and Maintainerr caches, plex_sections, plex_items,
// plex_files, watch_stats, seerr_requests and maintainerr_items); other packages use its
// exported queries.
//
// The index is filled by refresh jobs (Runner, job type refresh). An *arr refresh reads Sonarr,
// Radarr or Lidarr through the allow-listed client (internal/integrations/arr); the full refresh
// replaces the integration's cache and reconciles (queues targeted syncs for items whose files or
// folder changed, design D8); a targeted refresh (Params.ArrItemIDs, the webhook path) updates
// only those items and, with Params.SyncAfter, queues the syncs of their folders.
//
// Safety rules that live here:
//   - S10 (refresh guard): a full refresh fetches everything before it deletes anything, and it
//     does not mark more than half of the items (and more than 20) deleted, nor delete more than
//     half of the files (and more than 20), nor anything when the *arr answered an empty list,
//     unless the job has AllowChanges. Files under a root folder the *arr reports as not
//     accessible are never deleted. A held refresh does not move refreshed_at.
//   - S14 / D15 (freshness): a cache is fresh only while its last complete refresh is younger
//     than the integration's staleAfterHours and was made for the integration's current URL;
//     a failed or targeted refresh never changes that.
//   - S18 (paths): every path the *arr reports goes through the integration's path mappings and
//     is located in the sources (catalog.Locator); what maps nowhere is counted, never an error.
//   - S9 (dry run): a dry-run refresh fetches and counts, and writes and queues nothing.
package mediaindex

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/testhooks"
)

// Queryer runs read queries: *sql.DB (the read pool) and *sql.Tx satisfy it, so a caller can run
// the index's queries inside its own read transaction (a manifest reads the catalog, the index
// and the records from one snapshot, design §11.2). It has the method set of db.Queryer.
type Queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Index statuses (index_state.status).
const (
	StatusNever  = "never"
	StatusOK     = "ok"
	StatusFailed = "failed"
)

// State is an integration's index_state row: when its cache was last refreshed completely, the
// outcome of the last attempt, and which instance the cache describes.
type State struct {
	IntegrationID int64
	// Status is the outcome of the last attempt (full or targeted): never, ok or failed.
	Status string
	// RefreshedAt is the start of the last complete (full) refresh; nil before the first one.
	RefreshedAt *time.Time
	// AttemptedAt is the start of the last attempt, full or targeted; Error its error ("" when it
	// succeeded).
	AttemptedAt *time.Time
	Error       string
	// InstanceID is the integration URL the cache was built from.
	InstanceID string
	// AppVersion is the application's version as it reported it.
	AppVersion string
	// Stats are the counts of the last full refresh plus notes (the recycle bin, unmapped root
	// folders); a JSON object.
	Stats json.RawMessage
}

// Freshness is whether an integration's cache is fresh (design §6, D15), and why not.
type Freshness struct {
	// Fresh: the last complete refresh is younger than staleAfterHours and was made for the
	// integration's current URL.
	Fresh bool `json:"fresh"`
	// InstanceMatches is whether the cache was built from the integration's current URL.
	InstanceMatches bool `json:"instanceMatches"`
	// StaleAfterHours is the integration's setting.
	StaleAfterHours int `json:"staleAfterHours"`
	// Reason says why the cache is not fresh ("" when it is), e.g. "Radarr cache is 31 h old".
	Reason string `json:"reason,omitempty"`
}

// FreshnessOf decides the freshness of a cache (design D15): st is the integration's state (the
// zero State when it has none), it the integration as stored now, staleAfter its staleAfterHours
// and now the current time. The status and error of the last attempt never matter.
func FreshnessOf(st State, it integrations.Integration, staleAfter time.Duration, now time.Time) Freshness {
	f := Freshness{StaleAfterHours: int(staleAfter / time.Hour), InstanceMatches: st.InstanceID != "" && st.InstanceID == it.URL}
	app := it.Type.AppName()
	switch {
	case st.RefreshedAt == nil:
		f.Reason = fmt.Sprintf("%s has not been refreshed completely yet", app)
	case !f.InstanceMatches:
		f.Reason = fmt.Sprintf("%s's URL changed since its last complete refresh", app)
	case now.Sub(*st.RefreshedAt) >= staleAfter:
		f.Reason = fmt.Sprintf("%s cache is %s old (stale after %d h)", app, ageText(now.Sub(*st.RefreshedAt)), f.StaleAfterHours)
	default:
		f.Fresh = true
	}
	return f
}

// ageText renders an age for a reason: "31 h", "3 d".
func ageText(d time.Duration) string {
	h := int(d.Hours())
	if h >= 72 {
		return fmt.Sprintf("%d d", h/24)
	}
	return fmt.Sprintf("%d h", h)
}

// StaleAfter returns an integration's staleAfterHours setting as a duration, and false for a type
// whose settings have none (or that cannot be read).
func StaleAfter(it integrations.Integration) (time.Duration, bool) {
	var hours int
	switch {
	case it.Type.IsArr():
		s, err := it.ArrSettings()
		if err != nil {
			return 0, false
		}
		hours = s.Refresh.StaleAfterHours
	case it.Type == integrations.TypeTautulli:
		s, err := it.TautulliSettings()
		if err != nil {
			return 0, false
		}
		hours = s.Refresh.StaleAfterHours
	case it.Type == integrations.TypeSeerr:
		s, err := it.SeerrSettings()
		if err != nil {
			return 0, false
		}
		hours = s.Refresh.StaleAfterHours
	case it.Type == integrations.TypeMaintainerr:
		s, err := it.MaintainerrSettings()
		if err != nil {
			return 0, false
		}
		hours = s.Refresh.StaleAfterHours
	default:
		return 0, false
	}
	if hours <= 0 {
		return 0, false
	}
	return time.Duration(hours) * time.Hour, true
}

// Store reads (and the refresh writes) the index tables.
type Store struct {
	db      *db.DB
	catalog *catalog.Store
	now     func() time.Time
	// matchChecks counts the files Unmapped checked against the catalog (tests).
	matchChecks atomic.Int64
}

// NewStore returns a store over d. cat locates and matches catalog files for the unmapped
// listing (Unmapped).
func NewStore(d *db.DB, cat *catalog.Store) *Store {
	return &Store{db: d, catalog: cat, now: time.Now}
}

// Reader returns the read pool, the default Queryer.
func (s *Store) Reader() Queryer { return s.db.Reader() }

func orReader(s *Store, q Queryer) Queryer {
	if q == nil {
		return s.db.Reader()
	}
	return q
}

// State returns an integration's index_state row; the zero State with Status never (and no
// error) when it has none. q may be nil (the read pool).
func (s *Store) State(ctx context.Context, q Queryer, integrationID int64) (State, error) {
	st, err := scanState(orReader(s, q).QueryRowContext(ctx, stateSelect+` WHERE integration_id = ?`, integrationID))
	if errors.Is(err, sql.ErrNoRows) {
		return State{IntegrationID: integrationID, Status: StatusNever, Stats: json.RawMessage(`{}`)}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("read the index state of integration %d: %w", integrationID, err)
	}
	return st, nil
}

// States returns every integration's index_state row, by integration id. q may be nil.
func (s *Store) States(ctx context.Context, q Queryer) (map[int64]State, error) {
	rows, err := orReader(s, q).QueryContext(ctx, stateSelect+` ORDER BY integration_id`)
	if err != nil {
		return nil, fmt.Errorf("read index states: %w", err)
	}
	defer rows.Close()
	out := map[int64]State{}
	for rows.Next() {
		st, err := scanState(rows)
		if err != nil {
			return nil, fmt.Errorf("read index states: %w", err)
		}
		out[st.IntegrationID] = st
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read index states: %w", err)
	}
	return out, nil
}

// Freshness reads an integration's state and decides its freshness now. A type without
// staleAfterHours is never fresh.
func (s *Store) Freshness(ctx context.Context, q Queryer, it integrations.Integration) (Freshness, error) {
	st, err := s.State(ctx, q, it.ID)
	if err != nil {
		return Freshness{}, err
	}
	stale, ok := StaleAfter(it)
	if !ok {
		return Freshness{Reason: fmt.Sprintf("%s has no metadata cache", it.Type.AppName())}, nil
	}
	return FreshnessOf(st, it, stale, testhooks.FreshnessNow(s.now())), nil
}

const stateSelect = `SELECT integration_id, status, refreshed_at, attempted_at, error, instance_id, app_version, stats FROM index_state`

type rowScanner interface{ Scan(dest ...any) error }

func scanState(r rowScanner) (State, error) {
	var (
		st                  State
		refreshed, attempts sql.NullString
		errText             sql.NullString
		stats               string
	)
	if err := r.Scan(&st.IntegrationID, &st.Status, &refreshed, &attempts, &errText, &st.InstanceID, &st.AppVersion, &stats); err != nil {
		return State{}, err
	}
	var err error
	if st.RefreshedAt, err = parseOptTime(refreshed); err != nil {
		return State{}, fmt.Errorf("refreshed_at: %w", err)
	}
	if st.AttemptedAt, err = parseOptTime(attempts); err != nil {
		return State{}, fmt.Errorf("attempted_at: %w", err)
	}
	st.Error = errText.String
	st.Stats = json.RawMessage(stats)
	if !json.Valid(st.Stats) {
		st.Stats = json.RawMessage(`{}`)
	}
	return st, nil
}

func parseOptTime(s sql.NullString) (*time.Time, error) {
	if !s.Valid || s.String == "" {
		return nil, nil
	}
	t, err := db.ParseTime(s.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func formatOptTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return db.FormatTime(*t)
}

// IndexView is GET /integrations/{id}/index (design §13).
type IndexView struct {
	Status      string          `json:"status"`
	RefreshedAt *time.Time      `json:"refreshedAt"`
	AttemptedAt *time.Time      `json:"attemptedAt"`
	Error       *string         `json:"error"`
	AppVersion  string          `json:"appVersion"`
	Stats       json.RawMessage `json:"stats"`
	Freshness
}

// IndexStatus returns the API view of an integration's index.
func (s *Store) IndexStatus(ctx context.Context, it integrations.Integration) (IndexView, error) {
	st, err := s.State(ctx, nil, it.ID)
	if err != nil {
		return IndexView{}, err
	}
	out := IndexView{Status: st.Status, RefreshedAt: st.RefreshedAt, AttemptedAt: st.AttemptedAt, AppVersion: st.AppVersion, Stats: st.Stats}
	if st.Error != "" {
		e := st.Error
		out.Error = &e
	}
	if stale, ok := StaleAfter(it); ok {
		out.Freshness = FreshnessOf(st, it, stale, testhooks.FreshnessNow(s.now()))
	} else {
		out.Freshness = Freshness{Reason: fmt.Sprintf("%s has no metadata cache", it.Type.AppName())}
	}
	return out, nil
}

// isArrKind reports whether k is an arr_items kind.
func isArrKind(k string) bool { return k == KindMovie || k == KindSeries || k == KindArtist }

// Item kinds (arr_items.kind).
const (
	KindMovie  = "movie"
	KindSeries = "series"
	KindArtist = "artist"
)

// KindOf returns the arr_items kind of an *arr integration type ("" for other types).
func KindOf(t integrations.Type) string {
	switch t {
	case integrations.TypeRadarr:
		return KindMovie
	case integrations.TypeSonarr:
		return KindSeries
	case integrations.TypeLidarr:
		return KindArtist
	}
	return ""
}

// likePrefix returns the half-open range [p+"/", p+"0") of the paths strictly under p ('0' is
// the byte after '/'), for an index range scan of the descendants of p.
func likePrefix(p string) (lo, hi string) {
	p = strings.TrimSuffix(p, "/")
	return p + "/", p + "0"
}
