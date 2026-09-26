//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/integrations/httpread/readtest"
	"github.com/sl0wz3r/bunkarr/internal/integrations/maintainerr/maintainerrtest"
	"github.com/sl0wz3r/bunkarr/internal/integrations/plex"
	"github.com/sl0wz3r/bunkarr/internal/integrations/plex/plextest"
	"github.com/sl0wz3r/bunkarr/internal/integrations/seerr/seerrtest"
	"github.com/sl0wz3r/bunkarr/internal/integrations/tautulli/tautullitest"
)

// Phase 3 fixtures shared by the native tier suite (tiers_test.go) and the real-Radarr tier test
// (docker_arr_tiers_test.go), docs/design/phase2-3.md §15: a fake Plex (plextest) whose movie
// section lists the test's files, and fake Tautulli, Seerr and Maintainerr serving the slice 9
// recordings, linked to that Plex, all added through the API of a running binary. Each fake fails
// the test on a request outside its recorded allow-list (S16), so the binary's clients are checked
// too. The tier API shapes (§8.1, §8.6, §13) are declared here rather than imported, so a renamed
// JSON field fails the suite.

// tierPlexToken is the fake Plex server's token.
const tierPlexToken = "e2e-plex-token-not-secret-000001"

// plexMovie is one row of the fake Plex movie section (library section 1).
type plexMovie struct {
	RatingKey string
	Title     string
	Year      int
	TMDB      int64
	// File is the path as Plex sees it (/data/movies/...).
	File string
	Size int64
}

// maintMember is a member of Maintainerr's "Watched movies" collection (id 1: arrAction 0,
// deleteAfterDays 30), so pending deletion unless excluded.
type maintMember struct {
	RatingKey string
	TMDB      int64
}

// maintAddDate is the members' addDate: pending members are deleted 30 days after it.
const maintAddDate = "2026-09-26T00:00:00.000Z"

// maintDeleteAfter is the deletion date Maintainerr's reasons carry (addDate + 30 days).
const maintDeleteAfter = `"2026-10-26T00:00:00Z"`

// tierFakes are the fake Plex, Tautulli, Seerr and Maintainerr of a tier test, and their
// integrations once added.
type tierFakes struct {
	t     *testing.T
	plex  *plextest.Server
	lib   *plextest.Library
	taut  *tautullitest.Server
	seerr *seerrtest.Server
	maint *maintainerrtest.Server

	plexID, tautID, seerrID, maintID int64
}

// newTierFakes starts the fakes. The Plex movie section lists movies; the other sections are the
// recorded ones (they map to no source).
func newTierFakes(t *testing.T, movies []plexMovie) *tierFakes {
	t.Helper()
	f := &tierFakes{t: t, plex: plextest.NewServer(t, tierPlexToken)}
	f.lib = f.plex.ServeLibrary(t)
	f.setMovies(movies)
	f.taut = tautullitest.NewServer(t)
	f.seerr = seerrtest.NewServer(t)
	// 3.29.0: its collection handler honours exclusions and skips a collection without
	// deleteAfterDays (3.4.1's "No deadline" would make every recorded movie pending).
	f.maint = maintainerrtest.NewServer(t, maintainerrtest.V3290)
	return f
}

// setMovies replaces the rows of the Plex movie section. Each row is the recorded row of Night of
// the Living Dead with the movie's rating key, ids, title and file.
func (f *tierFakes) setMovies(movies []plexMovie) {
	f.t.Helper()
	var template json.RawMessage
	for _, raw := range f.lib.Rows("1", plex.TypeMovie) {
		var row struct {
			RatingKey string `json:"ratingKey"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			f.t.Fatal(err)
		}
		if row.RatingKey == "3" {
			template = raw
		}
	}
	if template == nil {
		f.t.Fatal("the recorded Plex movie section has no rating key 3")
	}
	rows := make([]json.RawMessage, 0, len(movies))
	for i, m := range movies {
		var row map[string]any
		if err := json.Unmarshal(template, &row); err != nil {
			f.t.Fatal(err)
		}
		for _, k := range []string{"thumb", "art", "Image", "UltraBlurColors", "slug", "summary", "tagline"} {
			delete(row, k)
		}
		row["ratingKey"], row["key"] = m.RatingKey, "/library/metadata/"+m.RatingKey
		row["guid"] = "plex://movie/e2e" + m.RatingKey
		row["title"], row["year"] = m.Title, m.Year
		row["Guid"] = []map[string]string{{"id": fmt.Sprintf("tmdb://%d", m.TMDB)}}
		row["Media"] = []map[string]any{{"id": 900 + i, "Part": []map[string]any{{"id": 900 + i,
			"key": fmt.Sprintf("/library/parts/%d/1790411047/file.mkv", 900+i), "file": m.File, "size": m.Size}}}}
		b, err := json.Marshal(row)
		if err != nil {
			f.t.Fatal(err)
		}
		rows = append(rows, b)
	}
	f.lib.SetListing("1", plex.TypeMovie, rows)
}

// setWatchedMovies makes Maintainerr's "Watched movies" collection hold exactly members (in its
// overlay data, its collection list and its member list), as if Maintainerr had marked them.
func (f *tierFakes) setWatchedMovies(members ...maintMember) {
	f.t.Helper()
	list := make([]map[string]any, 0, len(members))
	for i, m := range members {
		list = append(list, map[string]any{"id": 900 + i, "collectionId": 1, "mediaServerId": m.RatingKey, "tmdbId": m.TMDB,
			"tvdbId": nil, "addDate": maintAddDate, "image_path": "", "isManual": true})
	}
	rewrite := func(name string, key string, shorten bool) {
		var cols []map[string]any
		if err := json.Unmarshal(maintainerrtest.Fixture(f.t, name), &cols); err != nil {
			f.t.Fatalf("%s: %v", name, err)
		}
		for _, c := range cols {
			if n, _ := c["id"].(float64); n != 1 {
				continue
			}
			c["media"], c["mediaCount"] = list, len(list)
			if shorten && len(list) > 2 { // /api/collections lists at most two members
				c["media"] = list[:2]
			}
		}
		f.maint.Set(key, f.route(name, cols))
	}
	rewrite("v3.29.0/collections-overlay-data.json", maintainerrtest.OverlayKey, false)
	rewrite("v3.29.0/collections.json", maintainerrtest.CollectionsKey, true)
	f.maint.Set(maintainerrtest.MediaKey("1"), f.route("v3.29.0/collections-media-collectionId-1.json", list))
}

// route is a recorded Maintainerr route with a new body.
func (f *tierFakes) route(name string, body any) readtest.Route {
	f.t.Helper()
	r := f.maint.Recorded(name)
	b, err := json.Marshal(body)
	if err != nil {
		f.t.Fatal(err)
	}
	r.Body = b
	return r
}

// addIntegrations adds the Plex integration (its library index on, plexRoot mapped to local) and
// waits for its index, then Tautulli, Seerr and Maintainerr linked to it (Maintainerr stale after
// maintStaleHours) and waits for their refreshes.
func (f *tierFakes) addIntegrations(c *client, plexRoot, local string, maintStaleHours int) {
	f.t.Helper()
	f.plexID = createIntegration(c, map[string]any{"type": "plex", "name": "Plex", "url": f.plex.URL, "apiKey": tierPlexToken,
		"settings": map[string]any{"pathMappings": []map[string]string{{"plex": plexRoot, "local": local}}, "index": map[string]any{"enabled": true}}})
	waitRefreshes(c, f.plexID)
	linked := map[string]any{"plexIntegrationId": f.plexID}
	f.tautID = createIntegration(c, map[string]any{"type": "tautulli", "name": "Tautulli", "url": f.taut.URL, "apiKey": tautullitest.Key,
		"settings": linked})
	f.seerrID = createIntegration(c, map[string]any{"type": "seerr", "name": "Seerr", "url": f.seerr.URL, "apiKey": seerrtest.Key,
		"settings": linked})
	f.maintID = createIntegration(c, map[string]any{"type": "maintainerr", "name": "Maintainerr", "url": f.maint.URL,
		"settings": map[string]any{"plexIntegrationId": f.plexID, "refresh": map[string]any{"staleAfterHours": maintStaleHours}}})
	for _, id := range []int64{f.tautID, f.seerrID, f.maintID} {
		waitRefreshes(c, id)
	}
}

// createIntegration creates an integration and returns its id.
func createIntegration(c *client, body map[string]any) int64 {
	c.t.Helper()
	var it struct {
		ID int64 `json:"id"`
	}
	c.call(http.StatusCreated, "POST", "/integrations", body, &it)
	if it.ID == 0 {
		c.t.Fatalf("created integration %v: no id", body["name"])
	}
	return it.ID
}

// waitRefreshes waits until integration id has a refresh job and every one of them is final, and
// requires them to have completed.
func waitRefreshes(c *client, id int64) {
	c.t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		var page apiPage[bunkarrJob]
		c.call(http.StatusOK, "GET", "/jobs?pageSize=500&type=refresh", nil, &page)
		var mine []bunkarrJob
		busy := false
		for _, j := range page.Records {
			if j.Params.IntegrationID == id {
				mine = append(mine, j)
				busy = busy || !j.final()
			}
		}
		if len(mine) > 0 && !busy {
			for _, j := range mine {
				if j.Status != "completed" && j.Status != "completed_with_warnings" {
					c.t.Fatalf("refresh of integration %d: %+v", id, j)
				}
			}
			return
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("the refreshes of integration %d did not finish: %+v", id, mine)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// waitJobsIdle waits until no job is queued or running.
func waitJobsIdle(c *client) {
	c.t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		busy := 0
		for _, st := range []string{"queued", "running"} {
			var page apiPage[apiJob]
			c.call(http.StatusOK, "GET", "/jobs?pageSize=1&status="+st, nil, &page)
			busy += int(page.TotalRecords)
		}
		if busy == 0 {
			return
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("%d jobs still queued or running", busy)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// --- tier API shapes ---

// apiReasonSource is Reason.source.
type apiReasonSource struct {
	Kind          string `json:"kind"`
	IntegrationID int64  `json:"integrationId"`
}

// apiReason is a Reason (one evaluated condition).
type apiReason struct {
	RuleID         int64           `json:"ruleId"`
	ConditionIndex int             `json:"conditionIndex"`
	Field          string          `json:"field"`
	Op             string          `json:"op"`
	Value          json.RawMessage `json:"value"`
	Actual         json.RawMessage `json:"actual"`
	Result         string          `json:"result"`
	Source         apiReasonSource `json:"source"`
	Why            string          `json:"why"`
}

// apiDecision is a Decision (a file's tier at a destination and why).
type apiDecision struct {
	Tier            string      `json:"tier"`
	RuleID          int64       `json:"ruleId"`
	RuleName        string      `json:"ruleName"`
	Reasons         []apiReason `json:"reasons"`
	Unknown         []apiReason `json:"unknown"`
	UnknownPromoted bool        `json:"unknownPromoted"`
	Revision        int64       `json:"revision"`
}

// apiRule is a stored TierRule.
type apiRule struct {
	ID       int64  `json:"id"`
	Priority int    `json:"priority"`
	Name     string `json:"name"`
	Enabled  bool   `json:"enabled"`
}

// apiRuleSet is GET and PUT /tiers/rules.
type apiRuleSet struct {
	Revision int64     `json:"revision"`
	Rules    []apiRule `json:"rules"`
	Warnings []struct {
		RuleIndex      int    `json:"ruleIndex"`
		ConditionIndex int    `json:"conditionIndex"`
		Message        string `json:"message"`
	} `json:"warnings"`
}

// apiUnknownSource is a cache whose facts are unknown now.
type apiUnknownSource struct {
	IntegrationID int64  `json:"integrationId"`
	Name          string `json:"name"`
	Reason        string `json:"reason"`
}

// apiCount is a number of files and bytes.
type apiCount struct {
	Files int64 `json:"files"`
	Bytes int64 `json:"bytes"`
}

// apiTierPreview is POST /tiers/preview.
type apiTierPreview struct {
	ID             string             `json:"id"`
	Revision       json.RawMessage    `json:"revision"`
	UnknownSources []apiUnknownSource `json:"unknownSources"`
	Destinations   []struct {
		DestinationID int64    `json:"destinationId"`
		Stored        apiCount `json:"stored"`
		Full          apiCount `json:"full"`
		Manifest      apiCount `json:"manifest"`
		Skip          apiCount `json:"skip"`
		ToCopy        apiCount `json:"toCopy"`
		Kept          apiCount `json:"kept"`
	} `json:"destinations"`
}

// apiPreviewItem is one file of a preview at one destination.
type apiPreviewItem struct {
	DestinationID   int64       `json:"destinationId"`
	FileID          int64       `json:"fileId"`
	SourceID        int64       `json:"sourceId"`
	RelPath         string      `json:"relPath"`
	Size            int64       `json:"size"`
	Tier            string      `json:"tier"`
	RuleID          int64       `json:"ruleId"`
	RuleName        string      `json:"ruleName"`
	Reasons         []apiReason `json:"reasons"`
	Unknown         []apiReason `json:"unknown"`
	UnknownPromoted bool        `json:"unknownPromoted"`
	State           string      `json:"state"`
}

// decision returns the item's decision.
func (it apiPreviewItem) decision() apiDecision {
	return apiDecision{Tier: it.Tier, RuleID: it.RuleID, RuleName: it.RuleName, Reasons: it.Reasons, Unknown: it.Unknown,
		UnknownPromoted: it.UnknownPromoted}
}

// tierItemDetail is the part of a sync item's detail the tier tests check.
type tierItemDetail struct {
	Source         string       `json:"source"`
	Reason         string       `json:"reason"`
	Note           string       `json:"note"`
	RecordID       int64        `json:"recordId"`
	RetainedReason string       `json:"retainedReason"`
	Tier           *apiDecision `json:"tier"`
	TierRevision   int64        `json:"tierRevision"`
}

// tierItem is a sync job item with its detail.
type tierItem struct {
	apiItem
	Detail tierItemDetail `json:"detail"`
}

// tierSyncStats are the tier stats of a sync (§8.5).
type tierSyncStats struct {
	FilesPlanned  int64 `json:"filesPlanned"`
	FilesCopied   int64 `json:"filesCopied"`
	FilesRetained int64 `json:"filesRetained"`
	Tiers         *struct {
		Full            apiCount `json:"full"`
		Manifest        apiCount `json:"manifest"`
		Skip            apiCount `json:"skip"`
		UnknownPromoted apiCount `json:"unknownPromoted"`
	} `json:"tiers"`
	FilesKept     int64 `json:"filesKept"`
	FilesReleased int64 `json:"filesReleased"`
	TierRevision  int64 `json:"tierRevision"`
}

// saveRules replaces the tier rules (each a RuleInput as JSON) and returns the saved set.
func saveRules(c *client, rules ...map[string]any) apiRuleSet {
	c.t.Helper()
	var cur apiRuleSet
	c.call(http.StatusOK, "GET", "/tiers/rules", nil, &cur)
	if rules == nil {
		rules = []map[string]any{}
	}
	var saved apiRuleSet
	c.call(http.StatusOK, "PUT", "/tiers/rules", map[string]any{"revision": cur.Revision, "rules": rules}, &saved)
	if saved.Revision != cur.Revision+1 || len(saved.Rules) != len(rules) {
		c.t.Fatalf("saved rules: revision %d (was %d), %d rules", saved.Revision, cur.Revision, len(saved.Rules))
	}
	return saved
}

// tierPreview runs POST /tiers/preview (the saved rules) and returns it with its items at
// destination destID.
func tierPreview(c *client, destID int64) (apiTierPreview, []apiPreviewItem) {
	c.t.Helper()
	var p apiTierPreview
	c.call(http.StatusOK, "POST", "/tiers/preview", map[string]any{}, &p)
	var all []apiPreviewItem
	for page := 1; ; page++ {
		var pg apiPage[apiPreviewItem]
		c.call(http.StatusOK, "GET", fmt.Sprintf("/tiers/preview/%s/items?destinationId=%d&pageSize=500&page=%d", p.ID, destID, page), nil, &pg)
		all = append(all, pg.Records...)
		if len(pg.Records) == 0 || int64(len(all)) >= pg.TotalRecords {
			return p, all
		}
	}
}

// tierItems returns every item of a job, with details.
func tierItems(c *client, jobID int64, query string) []tierItem {
	c.t.Helper()
	var all []tierItem
	for page := 1; ; page++ {
		var p apiPage[tierItem]
		c.call(http.StatusOK, "GET", fmt.Sprintf("/jobs/%d/items?pageSize=500&page=%d&%s", jobID, page, query), nil, &p)
		all = append(all, p.Records...)
		if len(p.Records) == 0 || int64(len(all)) >= p.TotalRecords {
			return all
		}
	}
}

// --- expected decisions ---

// wantReason is an expected Reason. Value and Actual are JSON ("" for Actual: absent); Why is a
// pattern the whole text must match (nil: no text).
type wantReason struct {
	ruleID    int64
	cond      int
	field, op string
	value     string
	actual    string
	result    string
	kind      string
	integ     int64
	why       *regexp.Regexp
}

// wantDecision is an expected Decision.
type wantDecision struct {
	tier     string
	ruleID   int64
	ruleName string
	reasons  []wantReason
	unknown  []wantReason
	promoted bool
}

// compactJSON returns b compacted ("" for absent or null).
func compactJSON(b []byte) string {
	if len(bytes.TrimSpace(b)) == 0 || string(bytes.TrimSpace(b)) == "null" {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		return string(b)
	}
	return buf.String()
}

// diffReasons lists the differences between reasons and want.
func diffReasons(what string, got []apiReason, want []wantReason) []string {
	var out []string
	if got == nil {
		out = append(out, what+": null, want a list")
	}
	if len(got) != len(want) {
		return append(out, fmt.Sprintf("%s: %d reasons, want %d: %+v", what, len(got), len(want), got))
	}
	for i, w := range want {
		g := got[i]
		why := g.Why == ""
		if w.why != nil {
			why = w.why.MatchString(g.Why)
		}
		if g.RuleID != w.ruleID || g.ConditionIndex != w.cond || g.Field != w.field || g.Op != w.op || compactJSON(g.Value) != w.value ||
			compactJSON(g.Actual) != w.actual || g.Result != w.result || g.Source.Kind != w.kind || g.Source.IntegrationID != w.integ || !why {
			out = append(out, fmt.Sprintf("%s[%d]:\n   got  {rule %d cond %d %s %s %s actual %s → %s source %s/%d why %q}\n   want {rule %d cond %d %s %s %s actual %s → %s source %s/%d why %v}",
				what, i, g.RuleID, g.ConditionIndex, g.Field, g.Op, compactJSON(g.Value), compactJSON(g.Actual), g.Result, g.Source.Kind,
				g.Source.IntegrationID, g.Why, w.ruleID, w.cond, w.field, w.op, w.value, w.actual, w.result, w.kind, w.integ, w.why))
		}
	}
	return out
}

// diffDecision lists the differences between a decision and want.
func diffDecision(got apiDecision, want wantDecision) []string {
	var out []string
	if got.Tier != want.tier || got.RuleID != want.ruleID || got.RuleName != want.ruleName || got.UnknownPromoted != want.promoted {
		out = append(out, fmt.Sprintf("%s by %q (#%d, promoted %v), want %s by %q (#%d, promoted %v)", got.Tier, got.RuleName, got.RuleID,
			got.UnknownPromoted, want.tier, want.ruleName, want.ruleID, want.promoted))
	}
	out = append(out, diffReasons("reasons", got.Reasons, want.reasons)...)
	return append(out, diffReasons("unknown", got.Unknown, want.unknown)...)
}

// fallbackDecision is the built-in fallback: full, no rule matched.
func fallbackDecision(unknown ...wantReason) wantDecision {
	return wantDecision{tier: "full", ruleName: "no rule matched", unknown: unknown}
}
