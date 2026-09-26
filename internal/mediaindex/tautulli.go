package mediaindex

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/integrations/tautulli"
)

// Watch stat key types (watch_stats.key_type).
const (
	KeyRatingKey = "rating_key"
	KeyGUID      = "guid"
)

// TautulliStats is index_state.stats of a Tautulli cache (counts and section keys only, no
// names).
type TautulliStats struct {
	// PlexIntegrationID is the Plex integration whose keys the rows use (the setting at refresh
	// time; the tier facts count the rows only while it is unchanged).
	PlexIntegrationID int64 `json:"plexIntegrationId"`
	// PlexInstanceID is the linked Plex index's instance id at refresh time (PlexInstanceID): the
	// tier facts count the rows only while the index is built from that same server.
	PlexInstanceID string `json:"plexInstanceId"`
	// Sections are the section keys whose history was read.
	Sections []string `json:"sections"`
	// SectionsWithoutHistory are the sections whose history Tautulli does not keep (keep_history
	// 0, or a section Tautulli does not know): their play counts are lower bounds.
	SectionsWithoutHistory []string `json:"sectionsWithoutHistory"`
	// Users and UsersWithoutHistory count Tautulli's users and those with keep_history 0: any
	// such user makes every play count a lower bound.
	Users               int   `json:"users"`
	UsersWithoutHistory int   `json:"usersWithoutHistory"`
	Plays               int64 `json:"plays"`
	RatingKeys          int64 `json:"ratingKeys"`
	GUIDs               int64 `json:"guids"`
}

type watchAgg struct {
	plays int64
	last  time.Time
}

func (pr *providerRun) refreshTautulli(ctx context.Context) error {
	it := pr.it
	ts, err := it.TautulliSettings()
	if err != nil {
		return err
	}
	p, err := pr.linkedPlex(ctx, ts.PlexIntegrationID)
	if err != nil {
		return err
	}
	key, err := pr.r.o.Integrations.TokenFor(ctx, it.ID, it.URL)
	if err != nil {
		return fmt.Errorf("Tautulli %q: %w", it.Name, err)
	}
	c, err := tautulli.New(it.URL, key, pr.r.o.Tautulli)
	if err != nil {
		return fmt.Errorf("Tautulli %q: %w", it.Name, err)
	}
	pr.stats.Requests++
	info, err := c.Info(ctx)
	if err != nil {
		return err
	}
	pr.stats.AppVersion = info.Version
	pr.stats.Requests++
	si, err := c.ServerInfo(ctx)
	if err != nil {
		return err
	}
	pr.stats.Requests++
	mid, err := pr.plexIdentity(ctx, p)
	if err != nil {
		return err
	}
	if si.PMSIdentifier != mid {
		return fmt.Errorf("Tautulli %q watches another Plex server than %q (its server identifier differs): link it to the right Plex server", it.Name, p.Name)
	}
	// The sections come from the linked Plex's library index, built from that same server.
	st, err := pr.r.store.State(ctx, nil, p.ID)
	if err != nil {
		return err
	}
	if st.InstanceID != PlexInstanceID(p.URL, mid) {
		return fmt.Errorf("the Plex library index of %q is not built from this server yet: turn it on (Settings → Plex) and refresh it first", p.Name)
	}
	sections, err := pr.r.store.PlexSections(ctx, nil, p.ID)
	if err != nil {
		return err
	}
	stored := TautulliStats{PlexIntegrationID: p.ID, PlexInstanceID: st.InstanceID, Sections: []string{}, SectionsWithoutHistory: []string{}}
	for _, s := range sections {
		if s.Type != "movie" && s.Type != "show" && s.Type != "artist" {
			continue
		}
		pr.stats.Requests++
		lib, err := c.Library(ctx, s.Key)
		if err != nil {
			return err
		}
		stored.Sections = append(stored.Sections, s.Key)
		if !lib.Known || !lib.KeepHistory {
			stored.SectionsWithoutHistory = append(stored.SectionsWithoutHistory, s.Key)
		}
	}
	pr.stats.Requests++
	users, err := c.Users(ctx)
	if err != nil {
		return err
	}
	stored.Users = len(users)
	for _, u := range users {
		if !u.KeepHistory {
			stored.UsersWithoutHistory++
		}
	}
	byKey := map[string]*watchAgg{}
	byGUID := map[string]*watchAgg{}
	add := func(m map[string]*watchAgg, k string, stopped time.Time) {
		if k == "" {
			return
		}
		a := m[k]
		if a == nil {
			a = &watchAgg{}
			m[k] = a
		}
		a.plays++
		if stopped.After(a.last) {
			a.last = stopped
		}
	}
	h := c.NewHistoryReader()
	for _, sec := range stored.Sections {
		err := h.Section(ctx, sec, func(pl tautulli.Play) error {
			stored.Plays++
			add(byKey, pl.RatingKey, pl.Stopped)
			add(byGUID, pl.GUID, pl.Stopped)
			return nil
		})
		pr.stats.Requests += int64(h.Requests)
		h.Requests = 0
		if err != nil {
			return err
		}
	}
	stored.RatingKeys, stored.GUIDs = int64(len(byKey)), int64(len(byGUID))
	slices.Sort(stored.SectionsWithoutHistory)
	pr.stats.Items = stored.RatingKeys + stored.GUIDs
	pr.stats.Cache = stored
	if pr.stats.ItemsBefore, err = pr.rowCount(ctx, "watch_stats"); err != nil {
		return err
	}
	pr.summary = fmt.Sprintf("Tautulli %s: %d plays of %d items in %d sections", it.Name, stored.Plays, stored.RatingKeys, len(stored.Sections))
	if len(stored.SectionsWithoutHistory) > 0 || stored.UsersWithoutHistory > 0 {
		pr.info("Tautulli does not keep the history of some sections or users: play counts are lower bounds there",
			"sections", len(stored.SectionsWithoutHistory), "users", stored.UsersWithoutHistory)
	}
	changed, err := pr.instanceChanged(ctx, it.URL)
	if err != nil {
		return err
	}
	if held, err := pr.guard(ctx, pr.stats.ItemsBefore, pr.stats.Items, changed); err != nil || held {
		if held {
			pr.summary = fmt.Sprintf("Tautulli %s: refresh guard held the new play counts (%s)", it.Name, pr.stats.GuardReason)
		}
		return err
	}
	if pr.dry {
		return nil
	}
	type watchRow struct {
		typ, key string
		agg      *watchAgg
	}
	watches := make([]watchRow, 0, len(byKey)+len(byGUID))
	for _, set := range []struct {
		typ string
		m   map[string]*watchAgg
	}{{KeyRatingKey, byKey}, {KeyGUID, byGUID}} {
		for k, a := range set.m {
			watches = append(watches, watchRow{set.typ, k, a})
		}
	}
	t := &cacheTable{name: "watch_stats", keys: []string{"key_type", "key"}, vals: []string{"plays", "last_watched_at"}, n: len(watches),
		row: func(dst []any, i int) []any {
			w := watches[i]
			var last any
			if !w.agg.last.IsZero() {
				last = db.FormatTime(w.agg.last)
			}
			return append(dst, w.typ, w.key, w.agg.plays, last)
		}}
	return pr.replace(ctx, []*cacheTable{t}, it.URL, stored)
}
