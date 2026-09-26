package mediaindex

import (
	"strings"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/integrations/maintainerr"
)

// TestPlexServerCheck: Maintainerr's members must show, at every refresh, that they are the linked
// Plex index's items: TMDB or TVDB ids that agree, or, for Plex items without ids (legacy agents,
// unmatched items), most members naming an item of the collection's level in its library. No
// evidence is never accepted, whether or not an earlier check passed (S14).
func TestPlexServerCheck(t *testing.T) {
	movie := func(key, section string, ids map[string]string) PlexItem {
		return PlexItem{RatingKey: key, Type: maintainerr.LevelMovie, SectionKey: section, ExternalIDs: ids}
	}
	snap := func(keys ...string) maintainerr.Snapshot {
		c := maintainerr.Collection{ID: 1, Title: "Watched", LibraryID: "1", Type: maintainerr.LevelMovie}
		for i, k := range keys {
			c.Media = append(c.Media, maintainerr.Member{ID: int64(i + 1), RatingKey: k, TMDBID: 100 + int64(i)})
		}
		return maintainerr.Snapshot{Collections: []maintainerr.Collection{c}}
	}
	const instance = "http://plex:32400#machine-a"
	accepted := MaintainerrStats{PlexIntegrationID: 7, PlexInstanceID: instance}
	cases := []struct {
		name  string
		snap  maintainerr.Snapshot
		items map[string]PlexItem
		prev  MaintainerrStats
		want  string // "" accepted, else a fragment of the reason
	}{
		{"ids agree", snap("1", "2"), map[string]PlexItem{"1": movie("1", "1", map[string]string{"tmdb": "100"})}, MaintainerrStats{}, ""},
		{"no members", snap(), map[string]PlexItem{}, MaintainerrStats{}, ""},
		{"first check, no member indexed", snap("1", "2"), map[string]PlexItem{}, MaintainerrStats{}, "none of Maintainerr's collection members"},
		{"accepted before, no member indexed", snap("1", "2"), map[string]PlexItem{}, accepted, "none of Maintainerr's collection members"},
		{"ids differ", snap("1"), map[string]PlexItem{"1": movie("1", "1", map[string]string{"tmdb": "999"})}, MaintainerrStats{}, "name other items"},
		{"no ids, most match by key, level and library", snap("1", "2", "3"),
			map[string]PlexItem{"1": movie("1", "1", nil), "2": movie("2", "1", nil)}, MaintainerrStats{}, ""},
		{"no ids, few match", snap("1", "2", "3", "4"), map[string]PlexItem{"1": movie("1", "1", nil), "2": movie("2", "1", nil)}, accepted,
			"only 2 of Maintainerr's 4 collection members"},
		{"no ids, other library", snap("1", "2"), map[string]PlexItem{"1": movie("1", "5", nil), "2": movie("2", "5", nil)}, MaintainerrStats{},
			"name other items"},
		{"no ids, other level", snap("1"), map[string]PlexItem{"1": {RatingKey: "1", Type: "episode", SectionKey: "1"}}, MaintainerrStats{},
			"name other items"},
	}
	for _, c := range cases {
		got := plexServerCheck(c.snap, c.items, c.prev, 7, instance, "Plex")
		if c.want == "" && got != "" || c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
