package mediaindex

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/integrations/seerr"
)

// SeerrStats is index_state.stats of a Seerr cache.
type SeerrStats struct {
	// PlexIntegrationID is the setting at refresh time (0: none; the rating-key fallback is then
	// off).
	PlexIntegrationID int64 `json:"plexIntegrationId"`
	// PlexInstanceID is that Plex index's instance id at refresh time (PlexInstanceID): the
	// rating-key fallback counts only while the index is built from that same server.
	PlexInstanceID string `json:"plexInstanceId,omitempty"`
	Requests       int64  `json:"requests"`
	// Counted are the requests with status 1, 2, 4 or 5.
	Counted int64 `json:"counted"`
	// Skipped are requests of another media type than movie or tv.
	Skipped int64 `json:"skipped"`
}

func (pr *providerRun) refreshSeerr(ctx context.Context) error {
	it := pr.it
	ss, err := it.SeerrSettings()
	if err != nil {
		return err
	}
	key, err := pr.r.o.Integrations.TokenFor(ctx, it.ID, it.URL)
	if err != nil {
		return fmt.Errorf("Seerr %q: %w", it.Name, err)
	}
	c, err := seerr.New(it.URL, key, pr.r.o.Seerr)
	if err != nil {
		return fmt.Errorf("Seerr %q: %w", it.Name, err)
	}
	defer func() { pr.stats.Requests = c.Sent() }()
	st, err := c.Status(ctx)
	if err != nil {
		return err
	}
	pr.stats.AppVersion = st.Version
	if _, err := c.Me(ctx); err != nil {
		return err
	}
	stored := SeerrStats{PlexIntegrationID: ss.PlexIntegrationID}
	if ss.PlexIntegrationID != 0 {
		pst, err := pr.r.store.State(ctx, nil, ss.PlexIntegrationID)
		if err != nil {
			return err
		}
		stored.PlexInstanceID = pst.InstanceID
	}
	var rows []seerr.Request
	_, err = c.Requests(ctx, func(r seerr.Request) error {
		if r.Type != "movie" && r.Type != "tv" {
			stored.Skipped++
			return nil
		}
		rows = append(rows, r)
		if seerr.Counted(r.Status) {
			stored.Counted++
		}
		return nil
	})
	if err != nil {
		return err
	}
	stored.Requests = int64(len(rows))
	pr.stats.Items = stored.Requests
	pr.stats.Cache = stored
	if pr.stats.ItemsBefore, err = pr.rowCount(ctx, "seerr_requests"); err != nil {
		return err
	}
	pr.summary = fmt.Sprintf("Seerr %s: %d requests (%d counted)", it.Name, stored.Requests, stored.Counted)
	changed, err := pr.instanceChanged(ctx, it.URL)
	if err != nil {
		return err
	}
	if held, err := pr.guard(ctx, pr.stats.ItemsBefore, pr.stats.Items, changed); err != nil || held {
		if held {
			pr.summary = fmt.Sprintf("Seerr %s: refresh guard held the new requests (%s)", it.Name, pr.stats.GuardReason)
		}
		return err
	}
	if pr.dry {
		return nil
	}
	seasons := make([]string, len(rows))
	for i, r := range rows {
		b, err := json.Marshal(r.Seasons)
		if err != nil {
			return err
		}
		seasons[i] = string(b)
	}
	t := &cacheTable{name: "seerr_requests", keys: []string{"request_id"},
		vals: []string{"status", "media_type", "tmdb_id", "tvdb_id", "rating_key", "is_4k", "seasons", "user_id", "requested_at"},
		n:    len(rows),
		row: func(dst []any, i int) []any {
			r := &rows[i]
			var at any
			if r.CreatedAt != nil {
				at = db.FormatTime(*r.CreatedAt)
			}
			return append(dst, r.ID, r.Status, r.Type, nullInt(r.TMDBID), nullInt(r.TVDBID), nullText(r.RatingKey), r.Is4K, seasons[i], r.UserID, at)
		}}
	return pr.replace(ctx, []*cacheTable{t}, it.URL, stored)
}
