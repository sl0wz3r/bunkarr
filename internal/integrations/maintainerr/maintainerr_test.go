package maintainerr_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/integrations/httpread"
	"github.com/sl0wz3r/bunkarr/internal/integrations/httpread/readtest"
	"github.com/sl0wz3r/bunkarr/internal/integrations/maintainerr"
	"github.com/sl0wz3r/bunkarr/internal/integrations/maintainerr/maintainerrtest"
)

func newClient(t *testing.T, s *maintainerrtest.Server) *maintainerr.Client {
	t.Helper()
	c, err := maintainerr.New(s.URL, maintainerr.Options{Options: httpread.Options{HTTPClient: s.Client()}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var allowedPaths = []string{maintainerr.PathStatus, maintainerr.PathOverlayData, maintainerr.PathCollections,
	maintainerr.PathCollectionMedia, maintainerr.PathRules, maintainerr.PathExclusions}

func checkRequests(t *testing.T, s *maintainerrtest.Server) {
	t.Helper()
	for _, r := range s.Requests() {
		if r.Method != http.MethodGet || !slices.Contains(allowedPaths, r.Path) {
			t.Errorf("request %s %s outside the allow-list", r.Method, r.Path)
		}
		for _, bad := range []string{"activate", "deactivate", "settings", "database"} {
			if strings.Contains(r.Path, bad) {
				t.Errorf("forbidden path %s", r.Path)
			}
		}
	}
}

func TestStatusVersionGate(t *testing.T) {
	for _, tt := range []struct {
		version string
		want    error
	}{{"", nil}, {"3.4.0", nil}, {"3.3.0", maintainerr.ErrTooOld}} {
		s := maintainerrtest.NewServer(t, maintainerrtest.V341)
		if tt.version != "" {
			s.OldVersion(tt.version)
		}
		st, err := newClient(t, s).Status(context.Background())
		if !errors.Is(err, tt.want) {
			t.Fatalf("%q: Status = %+v, %v", tt.version, st, err)
		}
		if tt.version == "" && st.Version != "3.4.1" {
			t.Fatalf("version %q", st.Version)
		}
	}
}

func TestFetch(t *testing.T) {
	s := maintainerrtest.NewServer(t, maintainerrtest.V341)
	snap, err := newClient(t, s).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Version != "3.4.1" || len(snap.Collections) != 8 || len(snap.Groups) != 8 || !snap.GlobalKnown || snap.FallbackReads != 0 {
		t.Fatalf("snapshot: version %s, %d collections, %d groups, global %v, fallback %d", snap.Version, len(snap.Collections),
			len(snap.Groups), snap.GlobalKnown, snap.FallbackReads)
	}
	counts := map[int64]int{}
	for _, c := range snap.Collections {
		counts[c.ID] = len(c.Media)
	}
	if want := map[int64]int{1: 3, 2: 6, 3: 6, 4: 6, 5: 1, 6: 2, 7: 4, 8: 3}; !mapsEqual(counts, want) {
		t.Fatalf("members = %v, want %v", counts, want)
	}
	c1 := snap.Collections[0]
	if c1.ID != 1 || c1.LibraryID != "1" || c1.Type != "movie" || c1.ArrAction != 0 || c1.DeleteAfterDays == nil || *c1.DeleteAfterDays != 30 {
		t.Fatalf("collection 1 = %+v", c1)
	}
	if m := c1.Media[0]; m.RatingKey != "3" || !m.IsManual || m.TMDBID != 10331 || m.AddDate == nil || !m.AddDate.Equal(time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("member = %+v", m)
	}
	if c4 := snap.Collections[3]; c4.DeleteAfterDays != nil {
		t.Fatalf("collection 4 deleteAfterDays = %v", *c4.DeleteAfterDays)
	}
	keys := func(ex []maintainerr.Exclusion) []string {
		var out []string
		for _, e := range ex {
			out = append(out, e.Type+":"+e.RatingKey)
		}
		sort.Strings(out)
		return out
	}
	if got := keys(snap.Global); !slices.Equal(got, []string{"movie:9"}) {
		t.Fatalf("global exclusions = %v", got)
	}
	for g, want := range map[int64][]string{1: nil, 6: {"show:17"}, 7: {"season:25", "season:29", "season:33"}, 8: {"episode:30", "episode:31", "episode:32"}} {
		if got := keys(snap.GroupExclusions[g]); !slices.Equal(got, want) {
			t.Errorf("group %d exclusions = %v, want %v", g, got, want)
		}
	}
	checkRequests(t, s)
}

func mapsEqual(a, b map[int64]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestFetchFallsBackToCollectionMedia(t *testing.T) {
	s := maintainerrtest.NewServer(t, maintainerrtest.V341)
	var overlay []map[string]any
	if err := json.Unmarshal(maintainerrtest.Fixture(t, "collections-overlay-data.json"), &overlay); err != nil {
		t.Fatal(err)
	}
	// Collection 1 listed with one member of three, collection 2 not listed at all.
	overlay[0]["media"] = overlay[0]["media"].([]any)[:1]
	overlay = overlay[1:]
	overlay = append([]map[string]any{{"id": 1, "title": "Watched movies", "libraryId": "1", "isActive": true, "arrAction": 0,
		"deleteAfterDays": 30, "type": "movie", "mediaCount": 3, "media": []any{}}}, overlay[1:]...)
	body, _ := json.Marshal(overlay)
	s.Set(maintainerrtest.OverlayKey, readtest.Route{Body: body})
	snap, err := newClient(t, s).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.FallbackReads != 2 || s.Count(maintainerrtest.MediaKey("1")) != 1 || s.Count(maintainerrtest.MediaKey("2")) != 1 {
		t.Fatalf("fallback reads %d; requests %v", snap.FallbackReads, s.Requests())
	}
	for _, c := range snap.Collections {
		if (c.ID == 1 && len(c.Media) != 3) || (c.ID == 2 && len(c.Media) != 6) {
			t.Fatalf("collection %d has %d members", c.ID, len(c.Media))
		}
	}
	checkRequests(t, s)
}

func TestFetchFailsWhenAnExclusionCallFails(t *testing.T) {
	s := maintainerrtest.NewServer(t, maintainerrtest.V341)
	s.SetStatus(maintainerrtest.ExclusionKey("7"), http.StatusInternalServerError)
	if _, err := newClient(t, s).Fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("Fetch = %v, want the 500", err)
	}
}

// plexTree is the recorded Plex index (testdata/tautulli/plex) as a maintainerr.Tree.
type plexTree map[string]maintainerr.PlexItem

func (p plexTree) Item(key string) (maintainerr.PlexItem, bool) {
	it, ok := p[key]
	return it, ok
}

func loadTree(t *testing.T) plexTree {
	t.Helper()
	tree := plexTree{}
	files, err := filepath.Glob(filepath.Join(readtest.RepoTestdata("tautulli", "plex"), "library-sections-*-all-type-*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("plex listings: %v", err)
	}
	for _, f := range files {
		var body struct {
			MediaContainer struct {
				Metadata []struct {
					RatingKey            string `json:"ratingKey"`
					ParentRatingKey      string `json:"parentRatingKey"`
					GrandparentRatingKey string `json:"grandparentRatingKey"`
					Index                *int   `json:"index"`
					ParentIndex          *int   `json:"parentIndex"`
				} `json:"Metadata"`
			} `json:"MediaContainer"`
		}
		if err := json.Unmarshal(readtest.Read(t, f), &body); err != nil {
			t.Fatal(err)
		}
		for _, m := range body.MediaContainer.Metadata {
			tree[m.RatingKey] = maintainerr.PlexItem{Parent: m.ParentRatingKey, Grandparent: m.GrandparentRatingKey, Index: m.Index, ParentIndex: m.ParentIndex}
		}
	}
	return tree
}

// render lists pending members as "<collection>:<key>:<state>[:S<season>E<episode>]".
func render(list []maintainerr.PendingMember) []string {
	var out []string
	for _, p := range list {
		s := fmt.Sprintf("%d:%s:%s", p.CollectionID, p.RatingKey, p.State)
		if p.Season != nil {
			s += fmt.Sprintf(":S%d", *p.Season)
		}
		if p.Episode != nil {
			s += fmt.Sprintf("E%d", *p.Episode)
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func fetch(t *testing.T, variant string, setup func(*maintainerrtest.Server)) maintainerr.Snapshot {
	t.Helper()
	s := maintainerrtest.NewServer(t, variant)
	if setup != nil {
		setup(s)
	}
	snap, err := newClient(t, s).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	checkRequests(t, s)
	return snap
}

// TestPendingRecorded checks Pending against hand-written lists for the recordings (§15):
//   - 3.29.0: Watched movies keeps Night of the Living Dead (3, manual) and Nosferatu (4); The
//     General (9) is excluded globally. Unmonitor only (3), Do nothing (4), No deadline and
//     Inactive delete nothing. Watched shows: The Twilight Zone (28); Alfred Hitchcock Presents
//     (17) is excluded in group 6. All seasons: AHP S01 (18); TZ S01 and S02 (29, 33) and AHP S02
//     (25) are excluded in group 7. Watched episodes: AHP S02E01 (26); TZ S01E01 and E02 (30, 31)
//     are excluded in group 8. Tautulli played, whose members failed their rule check (none
//     pending), and Episodes, delete show if empty (arrAction 5): 26, 30 and 31.
//   - 3.4.1 runs the older handler (probes/): No deadline deletes every member at once (1-5 and
//     9), and the excluded members (9, 17, 25, 29, 33, 30, 31) are undecided, not dropped.
func TestPendingRecorded(t *testing.T) {
	tree := loadTree(t)
	tests := []struct {
		name    string
		variant string
		tree    maintainerr.Tree
		want    []string
	}{
		{"3.4.1", maintainerrtest.V341, tree, []string{"1:3:pending", "1:4:pending", "1:9:undecided",
			"4:1:pending", "4:2:pending", "4:3:pending", "4:4:pending", "4:5:pending", "4:9:undecided", "6:17:undecided", "6:28:pending",
			"7:18:pending:S1", "7:25:undecided:S2", "7:29:undecided:S1", "7:33:undecided:S2",
			"8:26:pending:S2E1", "8:30:undecided:S1E1", "8:31:undecided:S1E2"}},
		{"3.4.1 without a fresh Plex index", maintainerrtest.V341, nil, []string{"1:3:pending", "1:4:pending", "1:9:undecided",
			"4:1:pending", "4:2:pending", "4:3:pending", "4:4:pending", "4:5:pending", "4:9:undecided", "6:17:undecided", "6:28:pending",
			"7:18:undecided", "7:25:undecided", "7:29:undecided", "7:33:undecided", "8:26:undecided", "8:30:undecided", "8:31:undecided"}},
		{"3.29.0", maintainerrtest.V3290, tree, []string{"10:26:pending:S2E1", "10:30:pending:S1E1", "10:31:pending:S1E2",
			"1:3:pending", "1:4:pending", "6:28:pending", "7:18:pending:S1", "8:26:pending:S2E1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, stats := maintainerr.Pending(fetch(t, tt.variant, nil), tt.tree)
			if r := render(got); !slices.Equal(r, tt.want) {
				t.Fatalf("pending = %v\nwant %v", r, tt.want)
			}
			if stats.Pending+stats.Undecided != len(tt.want) {
				t.Fatalf("stats %+v", stats)
			}
		})
	}
	got, stats := maintainerr.Pending(fetch(t, maintainerrtest.V341, nil), tree)
	if !stats.OlderHandler || stats.NoWindow != 6 || stats.ExcludedUndecided != 8 || stats.Excluded != 0 {
		t.Fatalf("3.4.1 stats %+v", stats)
	}
	for _, p := range got {
		if p.CollectionID == 1 && p.RatingKey == "4" {
			if p.DeleteAfter == nil || !p.DeleteAfter.Equal(time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)) || p.LibraryID != "1" ||
				p.Level != "movie" || p.TMDBID != 653 || p.CollectionTitle != "Watched movies" {
				t.Fatalf("member 4 = %+v", p)
			}
		}
		// No deadline is due at once: deleteAfterDays null counts as 0.
		if p.CollectionID == 4 && (p.DeleteAfter == nil || !p.DeleteAfter.Equal(time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC))) {
			t.Fatalf("No deadline member %s deleteAfter %v", p.RatingKey, p.DeleteAfter)
		}
	}
}

// handlerActed reads a recorded log of Maintainerr's collection handler and returns the members it
// acted on as "<collection id>:<rating key>", leaving out "Unmonitor only", whose action deletes
// nothing.
func handlerActed(t *testing.T, snap maintainerr.Snapshot, parts ...string) map[string]bool {
	t.Helper()
	f, err := os.Open(filepath.Join(append([]string{maintainerrtest.Dir()}, parts...)...))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	titles := map[string]int64{}
	for _, c := range snap.Collections {
		titles[c.Title] = c.ID
	}
	handling := regexp.MustCompile(`Handling collection '(.+)'$`)
	media := regexp.MustCompile(`media with id (\d+)`)
	acted := map[string]bool{}
	current := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if m := handling.FindStringSubmatch(line); m != nil {
			current = m[1]
			continue
		}
		if m := media.FindStringSubmatch(line); m != nil && current != "" && current != "Unmonitor only" && strings.Contains(line, "CollectionHandler") {
			id, ok := titles[current]
			if !ok {
				t.Fatalf("%v: unknown collection %q", parts, current)
			}
			acted[fmt.Sprintf("%d:%s", id, m[1])] = true
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(acted) == 0 {
		t.Fatalf("%v: the handler acted on nothing", parts)
	}
	return acted
}

// TestPendingMatchesMaintainerrsHandler compares Pending for 3.29.0 with the members Maintainerr's
// own collection handler acted on when every window was 0 (probes/handle-zero.log), leaving out
// "Unmonitor only", whose action deletes nothing.
func TestPendingMatchesMaintainerrsHandler(t *testing.T) {
	snap := fetch(t, maintainerrtest.V3290, nil)
	acted := handlerActed(t, snap, "v3.29.0", "probes", "handle-zero.log")
	got, _ := maintainerr.Pending(snap, loadTree(t))
	mine := map[string]bool{}
	for _, p := range got {
		mine[fmt.Sprintf("%d:%s", p.CollectionID, p.RatingKey)] = true
	}
	if !mapsEqualB(mine, acted) {
		t.Fatalf("pending %v\nhandler acted on %v", mine, acted)
	}
}

func mapsEqualB(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// TestPendingMatchesOlderHandler compares Pending for 3.4.1 with what its collection handler did
// (probes/): nothing it deleted may be missing (absent is false, and a file Maintainerr deletes
// must never evaluate "pendingDelete" false), and every member Pending says is due was acted on.
//   - handle.log, the recorded windows: it handled No deadline (deleteAfterDays null) at once and
//     deleted all six movies, the globally excluded The General (9) too.
//   - handle-zero.log, every set window 0, on what handle.log left (No deadline empty): it ignored
//     every exclusion (9 global, 17 in group 6, 25/29/33 in group 7, 30/31 in group 8).
func TestPendingMatchesOlderHandler(t *testing.T) {
	recorded := time.Date(2026, 9, 26, 8, 29, 57, 0, time.UTC) // index.json recordedAt
	zeroWindows := func(s *maintainerrtest.Server) {
		// The overlay data after handle.log, every set deleteAfterDays changed to 0, as both lists.
		var cols []map[string]any
		if err := json.Unmarshal(maintainerrtest.Fixture(t, "probes/collections-overlay-data-after-handle.json"), &cols); err != nil {
			t.Fatal(err)
		}
		for _, c := range cols {
			if c["deleteAfterDays"] != nil {
				c["deleteAfterDays"] = 0
			}
		}
		body, err := json.Marshal(cols)
		if err != nil {
			t.Fatal(err)
		}
		s.Set(maintainerrtest.OverlayKey, readtest.Route{Body: body})
		s.Set(maintainerrtest.CollectionsKey, readtest.Route{Body: body})
	}
	tree := loadTree(t)
	for _, tt := range []struct {
		log   string
		setup func(*maintainerrtest.Server)
	}{{"handle.log", nil}, {"handle-zero.log", zeroWindows}} {
		t.Run(tt.log, func(t *testing.T) {
			snap := fetch(t, maintainerrtest.V341, tt.setup)
			acted := handlerActed(t, snap, "probes", tt.log)
			got, _ := maintainerr.Pending(snap, tree)
			due := map[string]bool{}
			for _, p := range got {
				key := fmt.Sprintf("%d:%s", p.CollectionID, p.RatingKey)
				if p.DeleteAfter != nil && !p.DeleteAfter.After(recorded) {
					due[key] = true
				}
				if acted[key] && p.State == maintainerr.StateUndecided && !slices.ContainsFunc(snap.Global, func(e maintainerr.Exclusion) bool {
					return e.RatingKey == p.RatingKey
				}) && !excludedInGroup(snap, p.RatingKey) {
					t.Errorf("%s is undecided without an exclusion", key)
				}
			}
			if !mapsEqualB(due, acted) {
				t.Fatalf("due %v\nhandler acted on %v", due, acted)
			}
		})
	}
}

func excludedInGroup(snap maintainerr.Snapshot, key string) bool {
	for _, ex := range snap.GroupExclusions {
		for _, e := range ex {
			if e.RatingKey == key {
				return true
			}
		}
	}
	return false
}

// TestPendingAfterGlobalShowExclusion: a global exclusion of The Twilight Zone (28) drops it and
// its episodes on 3.29.0; on 3.4.1, whose handler ignores exclusions, they are undecided.
func TestPendingAfterGlobalShowExclusion(t *testing.T) {
	for _, tt := range []struct {
		variant, probe string
		want           []string
	}{
		{maintainerrtest.V3290, "v3.29.0/probes/rules-exclusion-rulegroupId-8-after-global-show-exclusion.json",
			[]string{"10:26:pending:S2E1", "1:3:pending", "1:4:pending", "7:18:pending:S1", "8:26:pending:S2E1"}},
		{maintainerrtest.V341, "probes/rules-exclusion-rulegroupId-8-after-global-show-exclusion.json",
			[]string{"1:3:pending", "1:4:pending", "1:9:undecided", "4:1:pending", "4:2:pending", "4:3:pending", "4:4:pending", "4:5:pending",
				"4:9:undecided", "6:17:undecided", "6:28:undecided", "7:18:pending:S1", "7:25:undecided:S2", "7:29:undecided:S1",
				"7:33:undecided:S2", "8:26:pending:S2E1", "8:30:undecided:S1E1", "8:31:undecided:S1E2"}},
	} {
		t.Run(tt.variant, func(t *testing.T) {
			snap := fetch(t, tt.variant, func(s *maintainerrtest.Server) {
				s.Set(maintainerrtest.ExclusionKey("8"), s.Recorded(tt.probe))
			})
			got, _ := maintainerr.Pending(snap, loadTree(t))
			if r := render(got); !slices.Equal(r, tt.want) {
				t.Fatalf("pending = %v, want %v", r, tt.want)
			}
		})
	}
}

// TestPendingNoWindowBound runs the recorded 3.4.1 snapshot as 3.26, 3.27 and 3.28: No deadline (4)
// has no deleteAfterDays, which Maintainerr handles at once below 3.27.0 and never from it on
// (#3639), while exclusions stay unknown below 3.29.0.
func TestPendingNoWindowBound(t *testing.T) {
	tree := loadTree(t)
	older := []string{"1:3:pending", "1:4:pending", "1:9:undecided", "6:17:undecided", "6:28:pending",
		"7:18:pending:S1", "7:25:undecided:S2", "7:29:undecided:S1", "7:33:undecided:S2",
		"8:26:pending:S2E1", "8:30:undecided:S1E1", "8:31:undecided:S1E2"}
	noDeadline := []string{"4:1:pending", "4:2:pending", "4:3:pending", "4:4:pending", "4:5:pending", "4:9:undecided"}
	for _, tt := range []struct {
		version  string
		atOnce   bool
		noWindow int
	}{
		{"3.26.0", true, 6},
		{"3.27.0", false, 0},
		{"3.28.0", false, 0},
	} {
		t.Run(tt.version, func(t *testing.T) {
			snap := fetch(t, maintainerrtest.V341, nil)
			snap.Version = tt.version
			got, stats := maintainerr.Pending(snap, tree)
			want := slices.Clone(older)
			if tt.atOnce {
				want = append(want, noDeadline...)
				slices.Sort(want)
			}
			r := render(got)
			slices.Sort(r)
			slices.Sort(want)
			if !slices.Equal(r, want) {
				t.Fatalf("pending = %v\nwant %v", r, want)
			}
			if !stats.OlderHandler || stats.NoWindow != tt.noWindow || stats.Excluded != 0 {
				t.Fatalf("stats %+v", stats)
			}
			for _, p := range got {
				if p.CollectionID == 4 && (p.DeleteAfter == nil || !p.DeleteAfter.Equal(time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC))) {
					t.Fatalf("No deadline member %s deleteAfter %v", p.RatingKey, p.DeleteAfter)
				}
			}
		})
	}
}

func intp(n int) *int { return &n }

// TestPendingClauses checks each clause of §6.2 on a synthetic snapshot.
func TestPendingClauses(t *testing.T) {
	add := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	col := func(typ string, mods ...func(*maintainerr.Collection)) maintainerr.Collection {
		c := maintainerr.Collection{ID: 1, Title: "C", LibraryID: "2", IsActive: true, ArrAction: 0, DeleteAfterDays: intp(7), Type: typ,
			Media: []maintainerr.Member{{ID: 1, CollectionID: 1, RatingKey: "30", TVDBID: 73587, AddDate: &add}}}
		for _, m := range mods {
			m(&c)
		}
		return c
	}
	tree := plexTree{
		"30": {Parent: "29", Grandparent: "28", Index: intp(1), ParentIndex: intp(1)},
		"29": {Parent: "28", Index: intp(1)},
	}
	group := []maintainerr.RuleGroup{{ID: 5, CollectionID: 1}}
	tests := []struct {
		name    string
		version string // "" is 3.29.0
		col     maintainerr.Collection
		excl    []maintainerr.Exclusion
		glob    []maintainerr.Exclusion
		tree    maintainerr.Tree
		noGlb   bool
		want    string
		due     time.Time // zero: add + 7 days
	}{
		{name: "pending episode", col: col("episode"), tree: tree, want: "pending"},
		{name: "inactive", col: col("episode", func(c *maintainerr.Collection) { c.IsActive = false }), tree: tree},
		{name: "unmonitor only", col: col("episode", func(c *maintainerr.Collection) { c.ArrAction = 3 }), tree: tree},
		{name: "do nothing", col: col("episode", func(c *maintainerr.Collection) { c.ArrAction = 4 }), tree: tree},
		{name: "arrAction 5", col: col("episode", func(c *maintainerr.Collection) { c.ArrAction = 5 }), tree: tree, want: "pending"},
		{name: "no deadline", col: col("episode", func(c *maintainerr.Collection) { c.DeleteAfterDays = nil }), tree: tree},
		{name: "rule check failed", col: col("episode", func(c *maintainerr.Collection) { c.Media[0].RuleEvaluationFailed = true }), tree: tree},
		{name: "rule check failed, added by hand", col: col("episode", func(c *maintainerr.Collection) {
			c.Media[0].RuleEvaluationFailed, c.Media[0].IsManual = true, true
		}), tree: tree, want: "pending"},
		{name: "excluded itself", col: col("episode"), excl: []maintainerr.Exclusion{{RatingKey: "30", Type: "episode", RuleGroupID: 5}}, tree: tree},
		{name: "season excluded", col: col("episode"), excl: []maintainerr.Exclusion{{RatingKey: "29", Type: "season", RuleGroupID: 5}}, tree: tree},
		{name: "show excluded globally", col: col("episode"), glob: []maintainerr.Exclusion{{RatingKey: "28", Type: "show"}}, tree: tree},
		{name: "show excluded, season member", col: col("season", func(c *maintainerr.Collection) { c.Media[0].RatingKey = "29" }),
			excl: []maintainerr.Exclusion{{RatingKey: "28", Type: "show", RuleGroupID: 5}}, tree: tree},
		{name: "show excluded, ancestors unknown", col: col("episode"), excl: []maintainerr.Exclusion{{RatingKey: "28", Type: "show", RuleGroupID: 5}},
			tree: plexTree{}, want: "undecided"},
		{name: "episode exclusions only, ancestors unknown", col: col("episode"), excl: []maintainerr.Exclusion{{RatingKey: "31", Type: "episode", RuleGroupID: 5}},
			tree: plexTree{}, want: "pending"},
		{name: "Plex index not fresh", col: col("episode"), tree: nil, want: "undecided"},
		{name: "movie without a fresh Plex index", col: col("movie", func(c *maintainerr.Collection) { c.Media[0].RatingKey = "4" }), tree: nil, want: "pending"},
		{name: "global exclusions unreadable", col: col("movie"), noGlb: true, tree: tree, want: "undecided"},
		{name: "unknown level", col: col("album"), tree: tree},
		// Older handlers: below 3.27.0 no deadline is due at once; below 3.29.0 exclusions are unknown.
		{name: "older: no deadline", version: "3.4.1", col: col("episode", func(c *maintainerr.Collection) { c.DeleteAfterDays = nil }),
			tree: tree, want: "pending", due: add},
		{name: "older: no deadline, unmonitor only", version: "3.4.1", col: col("episode", func(c *maintainerr.Collection) {
			c.DeleteAfterDays, c.ArrAction = nil, 3
		}), tree: tree},
		{name: "older: no deadline, inactive", version: "3.4.1", col: col("episode", func(c *maintainerr.Collection) {
			c.DeleteAfterDays, c.IsActive = nil, false
		}), tree: tree},
		{name: "older: excluded itself", version: "3.4.1", col: col("episode"), excl: []maintainerr.Exclusion{{RatingKey: "30", Type: "episode", RuleGroupID: 5}},
			tree: tree, want: "undecided"},
		{name: "older: excluded globally", version: "3.28.9", col: col("movie"), glob: []maintainerr.Exclusion{{RatingKey: "30", Type: "movie"}},
			tree: tree, want: "undecided"},
		{name: "older: season excluded", version: "3.4.1", col: col("episode"), excl: []maintainerr.Exclusion{{RatingKey: "29", Type: "season", RuleGroupID: 5}},
			tree: tree, want: "undecided"},
		{name: "older: show excluded globally", version: "3.4.1", col: col("episode"), glob: []maintainerr.Exclusion{{RatingKey: "28", Type: "show"}},
			tree: tree, want: "undecided"},
		{name: "older: pending episode", version: "3.4.1", col: col("episode"), tree: tree, want: "pending"},
		{name: "unreadable version counts as older", version: "latest", col: col("movie", func(c *maintainerr.Collection) { c.DeleteAfterDays = nil }),
			tree: tree, want: "pending", due: add},
		{name: "newer: no deadline", version: "3.30.1", col: col("movie", func(c *maintainerr.Collection) { c.DeleteAfterDays = nil }), tree: tree},
		// Maintainerr 3.27.0 (#3639) treats no deadline as never, before it honours exclusions (3.29.0).
		{name: "3.26.9: no deadline is due at once", version: "3.26.9", col: col("movie", func(c *maintainerr.Collection) { c.DeleteAfterDays = nil }),
			tree: tree, want: "pending", due: add},
		{name: "3.27.0: no deadline", version: "3.27.0", col: col("movie", func(c *maintainerr.Collection) { c.DeleteAfterDays = nil }), tree: tree},
		{name: "3.28.0: no deadline", version: "3.28.0", col: col("episode", func(c *maintainerr.Collection) { c.DeleteAfterDays = nil }), tree: tree},
		{name: "3.28.0: no deadline, excluded itself", version: "3.28.0", col: col("episode", func(c *maintainerr.Collection) { c.DeleteAfterDays = nil }),
			excl: []maintainerr.Exclusion{{RatingKey: "30", Type: "episode", RuleGroupID: 5}}, tree: tree},
		{name: "3.28.0: excluded itself", version: "3.28.0", col: col("episode"), excl: []maintainerr.Exclusion{{RatingKey: "30", Type: "episode", RuleGroupID: 5}},
			tree: tree, want: "undecided"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			version := tt.version
			if version == "" {
				version = maintainerrtest.V3290
			}
			snap := maintainerr.Snapshot{Version: version, Collections: []maintainerr.Collection{tt.col}, Groups: group, GlobalKnown: !tt.noGlb,
				GroupExclusions: map[int64][]maintainerr.Exclusion{5: tt.excl}, Global: tt.glob}
			got, _ := maintainerr.Pending(snap, tt.tree)
			state := ""
			if len(got) == 1 {
				state = got[0].State
			} else if len(got) > 1 {
				t.Fatalf("got %d members", len(got))
			}
			if state != tt.want {
				t.Fatalf("state = %q, want %q", state, tt.want)
			}
			due := tt.due
			if due.IsZero() {
				due = add.AddDate(0, 0, 7)
			}
			if state != "" && !got[0].DeleteAfter.Equal(due) {
				t.Fatalf("deleteAfter = %v, want %v", got[0].DeleteAfter, due)
			}
		})
	}
}

func TestNoCredentialAndCaps(t *testing.T) {
	s := maintainerrtest.NewServer(t, maintainerrtest.V341)
	c := newClient(t, s)
	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, r := range s.Requests() {
		if r.Header.Get("X-Api-Key") != "" {
			t.Fatal("a key was sent")
		}
	}
	// A collection list that is not JSON fails without echoing the body.
	s.Set(maintainerrtest.CollectionsKey, readtest.Route{ContentType: "text/html", Body: []byte("<html>secret-page</html>")})
	_, err := c.Fetch(context.Background())
	if !errors.Is(err, maintainerr.ErrNotMaintainerr) || bytes.Contains([]byte(err.Error()), []byte("secret-page")) {
		t.Fatalf("Fetch = %v", err)
	}
}
