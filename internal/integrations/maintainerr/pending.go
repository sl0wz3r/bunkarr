package maintainerr

import (
	"slices"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/integrations/httpread"
)

// HandlerFixedIn is the oldest Maintainerr whose collection handler is recorded honouring
// exclusions (testdata/maintainerr/v3.29.0/probes). The recorded 3.4.1 handler
// (testdata/maintainerr/probes) deleted excluded members too (the global exclusion, a show,
// seasons and episodes). Below this version Pending treats excluded members as undecided.
var HandlerFixedIn = httpread.Version{3, 29, 0}

// NoWindowNeverFrom is the oldest Maintainerr whose collection handler skips a deleting collection
// without deleteAfterDays ("take action after days"). Upstream changed this in 3.27.0 (#3639,
// "Treat an unset deletion window as never instead of immediately"); the 3.29.0 recording shows it
// (v3.29.0/probes/handle-zero.log). Older handlers take a missing window as 0 and handle the
// collection at once (recorded for 3.4.1, probes/handle-zero.log). It is its own bound, below
// HandlerFixedIn: 3.27 and 3.28 never delete such a collection's members.
var NoWindowNeverFrom = httpread.Version{3, 27, 0}

// OlderHandler reports whether a Maintainerr of this version runs the older collection handler
// (below HandlerFixedIn). A version that cannot be read counts as older: Fetch never returns one.
func OlderHandler(version string) bool {
	v, ok := httpread.ParseVersion(version)
	return !ok || v.Less(HandlerFixedIn)
}

// NoWindowDueAtOnce reports whether a Maintainerr of this version handles a deleting collection
// without deleteAfterDays at once (below NoWindowNeverFrom). A version that cannot be read counts
// as at once, like OlderHandler.
func NoWindowDueAtOnce(version string) bool {
	v, ok := httpread.ParseVersion(version)
	return !ok || v.Less(NoWindowNeverFrom)
}

// Member states of a pending member (maintainerr_items.state).
const (
	// StatePending: Maintainerr will delete the member.
	StatePending = "pending"
	// StateUndecided: a show or season exclusion may cover the member, but its ancestors are not
	// known (the linked Plex index is not fresh, or does not hold it), so it is unknown (S14).
	StateUndecided = "undecided"
)

// DeletingActions are the arrActions that delete files: 0 delete, 1 and 2 (Sonarr: the show, the
// season), 5 (the episode, and the show when empty). 3 only unmonitors, 4 does nothing.
var DeletingActions = []int{0, 1, 2, 5}

// PlexItem is what the linked Plex index knows about a rating key.
type PlexItem struct {
	// Parent and Grandparent are the season's and the show's keys ("" when none).
	Parent      string
	Grandparent string
	// Index and ParentIndex: a season's number; an episode's number and its season's.
	Index       *int
	ParentIndex *int
}

// Tree answers what the linked Plex index knows. A nil Tree means the index is not fresh.
type Tree interface {
	Item(ratingKey string) (PlexItem, bool)
}

// PendingMember is a member Maintainerr will delete, or may (undecided).
type PendingMember struct {
	CollectionID    int64
	CollectionTitle string
	LibraryID       string
	// Level is the collection's type: movie, show, season or episode.
	Level     string
	RatingKey string
	TMDBID    int64
	TVDBID    int64
	// Season and Episode come from the linked Plex index (nil when unresolved or not applicable).
	Season  *int
	Episode *int
	State   string
	// DeleteAfter is addDate + deleteAfterDays (nil without an addDate).
	DeleteAfter *time.Time
}

// PendingStats count what Pending skipped.
type PendingStats struct {
	Members       int `json:"members"`
	Pending       int `json:"pending"`
	Undecided     int `json:"undecided"`
	Excluded      int `json:"excluded"`
	RuleFailed    int `json:"ruleFailed"`
	NotDeleting   int `json:"notDeleting"`
	UnknownLevels int `json:"unknownLevels"`
	// OlderHandler: the server runs the older collection handler (OlderHandler), and
	// ExcludedUndecided counts its excluded members (undecided instead of Excluded). NoWindow counts
	// the members of deleting collections without deleteAfterDays on a server that handles them at
	// once (NoWindowDueAtOnce: pending, due at addDate); from NoWindowNeverFrom on they are
	// NotDeleting.
	OlderHandler      bool `json:"olderHandler"`
	NoWindow          int  `json:"noWindow"`
	ExcludedUndecided int  `json:"excludedUndecided"`
}

// Pending decides which members are pending deletion (design §6.2), mirroring Maintainerr's own
// collection worker. A member is pending when all of these hold:
//   - its collection is active;
//   - the collection's arrAction deletes files (DeletingActions);
//   - the collection's deleteAfterDays is set (below NoWindowNeverFrom, see below);
//   - the member did not fail its rule check, unless it was added by hand;
//   - the member is not excluded, by its rule group's exclusions or the global ones: an exclusion
//     of the member itself, or of its show or season.
//
// A season- or episode-level member is undecided when tree is nil (the linked Plex index is not
// fresh), or when the exclusions that apply hold a show or season exclusion and tree does not
// know the member's ancestors. A member whose exclusions cannot be read at all (no rule group
// exists to read the global ones through) is undecided. Members of collections of another level
// are counted and skipped.
//
// Older servers differ in two ways, with two version bounds:
//   - below NoWindowNeverFrom (NoWindowDueAtOnce), a deleting collection without deleteAfterDays
//     is handled at once, so its members are pending with DeleteAfter = addDate (due now);
//   - below HandlerFixedIn (OlderHandler), excluded members (by an exclusion of their own, or of
//     their show or season) are undecided, not dropped: the recorded 3.4.1 handler deleted them,
//     but no recording says from which version on the handler honours them, so whether it deletes
//     them is unknown (S14).
func Pending(s Snapshot, tree Tree) ([]PendingMember, PendingStats) {
	var (
		out   []PendingMember
		stats PendingStats
	)
	older := OlderHandler(s.Version)
	atOnce := NoWindowDueAtOnce(s.Version)
	stats.OlderHandler = older
	groupsOf := map[int64][]int64{}
	for _, g := range s.Groups {
		groupsOf[g.CollectionID] = append(groupsOf[g.CollectionID], g.ID)
	}
	for _, c := range s.Collections {
		stats.Members += len(c.Media)
		if !c.IsActive || !slices.Contains(DeletingActions, c.ArrAction) || (c.DeleteAfterDays == nil && !atOnce) {
			stats.NotDeleting += len(c.Media)
			continue
		}
		if c.Type != LevelMovie && c.Type != LevelShow && c.Type != LevelSeason && c.Type != LevelEpisode {
			stats.UnknownLevels += len(c.Media)
			continue
		}
		// Below NoWindowNeverFrom a missing deleteAfterDays counts as 0: every member is due at once.
		days := 0
		if c.DeleteAfterDays != nil {
			days = *c.DeleteAfterDays
		} else {
			stats.NoWindow += len(c.Media)
		}
		excl := map[string]Exclusion{}
		for _, e := range s.Global {
			excl[e.RatingKey] = e
		}
		for _, g := range groupsOf[c.ID] {
			for _, e := range s.GroupExclusions[g] {
				excl[e.RatingKey] = e
			}
		}
		containers := false
		for _, e := range excl {
			if e.Type == LevelShow || e.Type == LevelSeason {
				containers = true
				break
			}
		}
		for _, m := range c.Media {
			if m.RuleEvaluationFailed && !m.IsManual {
				stats.RuleFailed++
				continue
			}
			_, excluded := excl[m.RatingKey]
			if excluded && !older {
				stats.Excluded++
				continue
			}
			p := PendingMember{CollectionID: c.ID, CollectionTitle: c.Title, LibraryID: c.LibraryID, Level: c.Type, RatingKey: m.RatingKey,
				TMDBID: m.TMDBID, TVDBID: m.TVDBID, State: StatePending}
			if m.AddDate != nil {
				d := m.AddDate.AddDate(0, 0, days)
				p.DeleteAfter = &d
			}
			nested := c.Type == LevelSeason || c.Type == LevelEpisode
			var item PlexItem
			known := false
			if tree != nil {
				item, known = tree.Item(m.RatingKey)
			}
			if known {
				switch c.Type {
				case LevelSeason:
					p.Season = item.Index
				case LevelEpisode:
					p.Season, p.Episode = item.ParentIndex, item.Index
				}
			}
			switch {
			case excluded:
				stats.ExcludedUndecided++
				p.State = StateUndecided
			case !s.GlobalKnown:
				p.State = StateUndecided
			case nested && tree == nil:
				p.State = StateUndecided
			case nested && containers && !known:
				p.State = StateUndecided
			case nested && containers && covered(excl, item, c.Type):
				if !older {
					stats.Excluded++
					continue
				}
				stats.ExcludedUndecided++
				p.State = StateUndecided
			}
			if p.State == StatePending {
				stats.Pending++
			} else {
				stats.Undecided++
			}
			out = append(out, p)
		}
	}
	return out, stats
}

// covered reports whether an exclusion of the member's show (or, for an episode, its season)
// covers it.
func covered(excl map[string]Exclusion, item PlexItem, level string) bool {
	if item.Parent != "" {
		if _, ok := excl[item.Parent]; ok {
			return true
		}
	}
	if level == LevelEpisode && item.Grandparent != "" {
		if _, ok := excl[item.Grandparent]; ok {
			return true
		}
	}
	return false
}
