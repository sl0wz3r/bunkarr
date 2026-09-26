package mediaindex

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/integrations/maintainerr"
)

// MaintainerrStats is index_state.stats of a Maintainerr cache.
type MaintainerrStats struct {
	// PlexIntegrationID is the setting at refresh time: the rows' keys belong to that server.
	PlexIntegrationID int64 `json:"plexIntegrationId"`
	// PlexInstanceID is the linked Plex index's instance id at refresh time (PlexInstanceID): the
	// tier facts count the rows only while the index is built from that same server. It is set only
	// when the collection members agree with that index (plexServerCheck): Maintainerr's API does
	// not say which Plex server it manages, so a Plex integration switched to another server must
	// not make the old server's rating keys count against the new one's index.
	PlexInstanceID string `json:"plexInstanceId"`
	// PlexMismatch, when set, is why the rows are not taken to be the linked Plex server's (the
	// tier facts are then unknown, S14).
	PlexMismatch string `json:"plexMismatch,omitempty"`
	// PlexIndexFresh is whether the linked Plex index was fresh during the refresh (when it was
	// not, every season- and episode-level member is undecided).
	PlexIndexFresh bool `json:"plexIndexFresh"`
	Collections    int  `json:"collections"`
	FallbackReads  int  `json:"fallbackReads"`
	maintainerr.PendingStats
}

// plexTree is the linked Plex index as maintainerr.Tree.
type plexTree map[string]maintainerr.PlexItem

func (t plexTree) Item(key string) (maintainerr.PlexItem, bool) {
	it, ok := t[key]
	return it, ok
}

// PlexTree reads a Plex integration's indexed items as the ancestry Maintainerr's pending
// computation needs.
func (s *Store) PlexTree(ctx context.Context, q Queryer, plexID int64) (maintainerr.Tree, error) {
	tree, _, err := s.plexTreeItems(ctx, q, plexID)
	return tree, err
}

// plexTreeItems reads a Plex integration's indexed items: the tree, and the items by rating key.
func (s *Store) plexTreeItems(ctx context.Context, q Queryer, plexID int64) (plexTree, map[string]PlexItem, error) {
	tree, items := plexTree{}, map[string]PlexItem{}
	err := s.EachPlexItem(ctx, q, plexID, func(it PlexItem) error {
		tree[it.RatingKey] = maintainerr.PlexItem{Parent: it.ParentKey, Grandparent: it.GrandparentKey, Index: it.Index, ParentIndex: it.ParentIndex}
		items[it.RatingKey] = it
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return tree, items, nil
}

// plexEvidence counts the collection members whose rating key is in the linked Plex index and
// whose item agrees with the member or contradicts it. Rating keys are per server: on another
// server the same key names another item, or none. agree: the movie's or show's TMDB or TVDB id is
// the same on both sides. weak: no id can be compared (Plex has ids only from Guid[]: an item of a
// legacy agent, or an unmatched one, has none), but the item has the collection's level and is in
// the collection's library. disagree: another level, other ids, or (no id to compare) another
// library.
func plexEvidence(snap maintainerr.Snapshot, items map[string]PlexItem) (members, agree, weak, disagree int) {
	for _, c := range snap.Collections {
		for _, m := range c.Media {
			members++
			it, ok := items[m.RatingKey]
			if !ok {
				continue // not indexed yet, or key churn: no evidence
			}
			show := it
			switch c.Type {
			case maintainerr.LevelMovie, maintainerr.LevelShow:
			case maintainerr.LevelSeason:
				show = items[it.ParentKey]
			case maintainerr.LevelEpisode:
				show = items[it.GrandparentKey]
			default:
				continue
			}
			if it.Type != c.Type {
				disagree++
				continue
			}
			switch v := idsAgree(c.Type, m, show.ExternalIDs); {
			case v > 0:
				agree++
			case v < 0:
				disagree++
			case c.LibraryID == "" || it.SectionKey == "":
			case it.SectionKey == c.LibraryID:
				weak++
			default:
				disagree++
			}
		}
	}
	return members, agree, weak, disagree
}

// idsAgree compares a member's ids with its movie's or show's Plex ids: 1 same, -1 different, 0
// not comparable.
func idsAgree(level string, m maintainerr.Member, ids map[string]string) int {
	cmp := func(want int64, got string) int {
		if want == 0 || got == "" {
			return 0
		}
		if got == fmt.Sprint(want) {
			return 1
		}
		return -1
	}
	if level != maintainerr.LevelMovie {
		if v := cmp(m.TVDBID, ids["tvdb"]); v != 0 {
			return v
		}
	}
	return cmp(m.TMDBID, ids["tmdb"])
}

// plexMachineOf is the machine identifier of a Plex index instance id ("<url>#<machineIdentifier>").
func plexMachineOf(instance string) string {
	if i := strings.LastIndexByte(instance, '#'); i >= 0 {
		return instance[i+1:]
	}
	return ""
}

// plexServerCheck decides whether the rows count against the linked Plex index (instance, fresh
// now). Maintainerr's API does not say which Plex server it manages, so the members must show it,
// at every refresh (a check passed once proves nothing about a server the Plex integration was
// switched to since, nor about a new Maintainerr integration): not when members contradict the
// index as often as they agree with it; not when no member's ids agree with it, unless most members
// agree weakly (their items have no ids to compare: plexEvidence). With no evidence at all, a
// Maintainerr that manages another server would have every member taken for key churn, and its
// TMDB and TVDB ids would mark this server's copies pending (S14). It returns why not, or "".
func plexServerCheck(snap maintainerr.Snapshot, items map[string]PlexItem, prev MaintainerrStats, plexID int64, instance, plexName string) string {
	members, agree, weak, disagree := plexEvidence(snap, items)
	if disagree > 0 && disagree >= agree+weak {
		return fmt.Sprintf("%d of Maintainerr's collection members name other items in the Plex library index of %s: Maintainerr manages another Plex server (link it to that one)",
			disagree, plexName)
	}
	if members == 0 || agree > 0 || weak*2 > members {
		return ""
	}
	remedy := "link Maintainerr to the Plex server it manages; the members are checked again at every Maintainerr refresh"
	if weak > 0 {
		return fmt.Sprintf("only %d of Maintainerr's %d collection members match the Plex library index of %s by rating key, level and library, and their Plex items have no TMDB or TVDB id to compare: whether Maintainerr manages this server is not known (%s; Plex items matched by an agent that provides TMDB or TVDB ids can be compared)",
			weak, members, plexName, remedy)
	}
	accepted := prev.PlexIntegrationID == plexID && prev.PlexMismatch == "" && plexMachineOf(prev.PlexInstanceID) != ""
	if accepted && plexMachineOf(prev.PlexInstanceID) != plexMachineOf(instance) {
		return fmt.Sprintf("none of Maintainerr's collection members matches the Plex library index of %s, which was built from another Plex server since Maintainerr was last checked against it: whether Maintainerr manages this server is not known (%s)",
			plexName, remedy)
	}
	return fmt.Sprintf("none of Maintainerr's collection members matches the Plex library index of %s: whether Maintainerr manages this server is not known (%s)",
		plexName, remedy)
}

func (pr *providerRun) refreshMaintainerr(ctx context.Context) error {
	it := pr.it
	ms, err := it.MaintainerrSettings()
	if err != nil {
		return err
	}
	p, err := pr.linkedPlex(ctx, ms.PlexIntegrationID)
	if err != nil {
		return err
	}
	c, err := maintainerr.New(it.URL, pr.r.o.Maintainerr)
	if err != nil {
		return fmt.Errorf("Maintainerr %q: %w", it.Name, err)
	}
	snap, err := c.Fetch(ctx)
	pr.stats.Requests = c.Requests()
	if err != nil {
		return err
	}
	pr.stats.AppVersion = snap.Version
	stored := MaintainerrStats{PlexIntegrationID: p.ID, Collections: len(snap.Collections), FallbackReads: snap.FallbackReads}
	plexState, err := pr.r.store.State(ctx, nil, p.ID)
	if err != nil {
		return err
	}
	var prev MaintainerrStats
	if st, err := pr.r.store.State(ctx, nil, it.ID); err != nil {
		return err
	} else if len(st.Stats) > 0 {
		_ = json.Unmarshal(st.Stats, &prev) // unreadable: as if never checked
	}
	var tree maintainerr.Tree
	fr, err := pr.r.store.Freshness(ctx, nil, p)
	if err != nil {
		return err
	}
	if fr.Fresh {
		stored.PlexIndexFresh = true
		t, items, err := pr.r.store.plexTreeItems(ctx, nil, p.ID)
		if err != nil {
			return err
		}
		tree = t
		if why := plexServerCheck(snap, items, prev, p.ID, plexState.InstanceID, p.Name); why != "" {
			stored.PlexMismatch = why
			pr.warn("Maintainerr's collections do not match the linked Plex server, so its facts are unknown", "plex", p.Name, "reason", why)
		} else {
			stored.PlexInstanceID = plexState.InstanceID
		}
	} else {
		pr.warn("The linked Plex library index is not fresh, so season and episode members whose exclusions cannot be checked are undecided (unknown)",
			"plex", p.Name, "reason", fr.Reason)
		// The members cannot be checked against the index now: keep what the last refresh
		// decided, so a Plex server switched since then stays unknown.
		if prev.PlexIntegrationID == p.ID {
			stored.PlexInstanceID, stored.PlexMismatch = prev.PlexInstanceID, prev.PlexMismatch
		} else {
			stored.PlexMismatch = fmt.Sprintf("Maintainerr's collections were not checked against the Plex library index of %s yet (it was not fresh at the refresh)", p.Name)
		}
	}
	pending, pst := maintainerr.Pending(snap, tree)
	stored.PendingStats = pst
	pr.stats.Items = int64(len(pending))
	pr.stats.Cache = stored
	if pr.stats.ItemsBefore, err = pr.rowCount(ctx, "maintainerr_items"); err != nil {
		return err
	}
	pr.summary = fmt.Sprintf("Maintainerr %s: %d members pending deletion, %d undecided, in %d collections", it.Name, pst.Pending, pst.Undecided, stored.Collections)
	if pr.dry {
		return nil
	}
	// No shrink guard (S10): fewer pending rows can only protect more.
	t := &cacheTable{name: "maintainerr_items", keys: []string{"collection_id", "rating_key"},
		vals: []string{"plex_integration_id", "collection_title", "library_id", "level", "tmdb_id", "tvdb_id", "season_number", "episode_number",
			"state", "delete_after"},
		n: len(pending),
		row: func(dst []any, i int) []any {
			m := &pending[i]
			var after any
			if m.DeleteAfter != nil {
				after = db.FormatTime(*m.DeleteAfter)
			}
			return append(dst, m.CollectionID, m.RatingKey, p.ID, m.CollectionTitle, m.LibraryID, m.Level, nullInt(m.TMDBID), nullInt(m.TVDBID),
				nullIntPtr(m.Season), nullIntPtr(m.Episode), m.State, after)
		}}
	return pr.replace(ctx, []*cacheTable{t}, it.URL, stored)
}
