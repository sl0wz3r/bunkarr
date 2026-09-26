//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/integrations/arr"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr/arrtest"
)

// Phase 3 against the real binary (docs/design/phase2-3.md §15 E2E): the tier dry run whose
// reasons equal the pinned table (acceptance 6), acceptance 7 with its stale case (the freshness
// clock skew of internal/testhooks makes the Maintainerr cache stale) and acceptance 9 (a demoted
// backup stays at the destination until a confirmed release, which releases exactly the files its
// dry run listed that are still not full). The *arr is the fake Radarr serving the recorded
// fixtures (movies 1-3 have a file, movie 4 none); Plex, Tautulli, Seerr and Maintainerr are the
// fakes of tierfakes_test.go, and Maintainerr marks movie 3 (Charade) pending.

// radarrMovie is a movie of the recorded Radarr with a file: its path inside the Movies source.
type radarrMovie struct {
	rel  string
	size int64
	tmdb int64
}

// radarrMovies returns the recorded Radarr's movies that have a file, by id.
func radarrMovies(t *testing.T) map[int64]radarrMovie {
	t.Helper()
	var list []struct {
		ID        int64 `json:"id"`
		TMDBID    int64 `json:"tmdbId"`
		MovieFile *struct {
			Path string `json:"path"`
			Size int64  `json:"size"`
		} `json:"movieFile"`
	}
	if err := json.Unmarshal(arrtest.Fixture(t, arr.KindRadarr, "movie.json"), &list); err != nil {
		t.Fatal(err)
	}
	out := map[int64]radarrMovie{}
	for _, m := range list {
		if m.MovieFile != nil {
			out[m.ID] = radarrMovie{rel: strings.TrimPrefix(m.MovieFile.Path, "/movies/"), size: m.MovieFile.Size, tmdb: m.TMDBID}
		}
	}
	return out
}

// tierEnv is a running binary with a Movies source holding the recorded Radarr's files, two
// destinations (NAS and Offsite) that have not synced yet, and every integration refreshed.
type tierEnv struct {
	t        *testing.T
	s        *server
	movies   string
	skewFile string
	src      apiSource
	nas, off apiDestination
	targets  map[int64]string
	radarrID int64
	fakes    *tierFakes
	films    map[int64]radarrMovie
}

func newTierEnv(t *testing.T) *tierEnv {
	t.Helper()
	root := resolvedTempDir(t)
	e := &tierEnv{t: t, s: newServer(t, root), movies: filepath.Join(root, "media", "movies"), skewFile: filepath.Join(root, "clock-skew"),
		targets: map[int64]string{}, films: radarrMovies(t)}
	if len(e.films) != 3 {
		t.Fatalf("recorded Radarr movies with a file: %+v", e.films)
	}
	for _, m := range e.films {
		writeFile(t, e.movies, m.rel, content(m.rel, 1, int(m.size)))
	}
	e.s.start("BUNKARR_TEST_CLOCK_SKEW_FILE=" + e.skewFile)
	e.s.setup()
	e.src = e.s.createSource("Movies", e.movies)
	var scan apiJob
	e.s.call(http.StatusAccepted, "POST", fmt.Sprintf("/sources/%d/scan", e.src.ID), nil, &scan)
	if j := e.s.waitJob(scan.ID, time.Minute); j.Status != "completed" {
		t.Fatalf("scan %+v", j)
	}
	e.src = e.s.source(e.src.ID)
	for _, name := range []string{"NAS", "Offsite"} {
		target := filepath.Join(root, "backup-"+strings.ToLower(name))
		mkdirAll(t, target)
		d := e.s.createDestination(name, target, e.src.ID)
		e.targets[d.ID] = target
		if name == "NAS" {
			e.nas = d
		} else {
			e.off = d
		}
	}

	radarr := arrtest.NewServer(t, arr.KindRadarr, hookArrKey)
	e.radarrID = createIntegration(e.s.client, map[string]any{"type": "radarr", "name": "Radarr", "url": radarr.URL, "apiKey": hookArrKey,
		"settings": map[string]any{"pathMappings": []map[string]string{{"arr": "/movies", "local": e.movies}}}})
	waitRefreshes(e.s.client, e.radarrID)

	// Plex lists the three files: Night of the Living Dead and His Girl Friday under their recorded
	// rating keys, Charade under a new one. Maintainerr's "Watched movies" holds Nosferatu (not in
	// this library), The General (excluded globally) and Charade.
	plexKeys := map[int64]string{1: "3", 2: "1", 3: "480"}
	titles := map[int64]string{1: "Night of the Living Dead", 2: "His Girl Friday", 3: "Charade"}
	var rows []plexMovie
	for _, id := range []int64{1, 2, 3} {
		m := e.films[id]
		rows = append(rows, plexMovie{RatingKey: plexKeys[id], Title: titles[id], Year: 1960, TMDB: m.tmdb, File: "/data/movies/" + m.rel, Size: m.size})
	}
	e.fakes = newTierFakes(t, rows)
	e.fakes.setWatchedMovies(maintMember{"4", 653}, maintMember{"9", 961}, maintMember{"480", e.films[3].tmdb})
	e.fakes.addIntegrations(e.s.client, "/data/movies", e.movies, 1)
	waitJobsIdle(e.s.client)
	return e
}

// skew sets the freshness clock skew of the running binary ("" removes it).
func (e *tierEnv) skew(d string) {
	e.t.Helper()
	if d == "" {
		if err := os.Remove(e.skewFile); err != nil && !os.IsNotExist(err) {
			e.t.Fatal(err)
		}
		return
	}
	if err := os.WriteFile(e.skewFile, []byte(d+"\n"), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// destPath is the destination path (relative to the target) of movie id's file.
func (e *tierEnv) destPath(id int64) string { return path.Join(e.src.DestFolder, e.films[id].rel) }

// atDest reports whether movie id's file is at destination d (and then that it is the source's
// content).
func (e *tierEnv) atDest(d apiDestination, id int64) bool {
	e.t.Helper()
	p := filepath.Join(e.targets[d.ID], filepath.FromSlash(e.destPath(id)))
	got, _, err := sha256File(p)
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		e.t.Fatal(err)
	}
	want, _, err := sha256File(filepath.Join(e.movies, filepath.FromSlash(e.films[id].rel)))
	if err != nil {
		e.t.Fatal(err)
	}
	if got != want {
		e.t.Fatalf("%s at %s differs from the source", e.films[id].rel, d.Name)
	}
	return true
}

// requireAt requires exactly the movies in want (of e.films) at destination d.
func (e *tierEnv) requireAt(what string, d apiDestination, want ...int64) {
	e.t.Helper()
	ids := make([]int64, 0, len(e.films))
	for id := range e.films {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		if got := e.atDest(d, id); got != slices.Contains(want, id) {
			e.t.Fatalf("%s: movie %d at %s: %v, want %v", what, id, d.Name, got, !got)
		}
	}
}

// requireTable requires every live file's decision at destination d to equal want (by Radarr
// movie id), both in a tier preview (GET /tiers/preview/{id}/items) and on the items of a dry-run
// sync; it returns the dry run.
func (e *tierEnv) requireTable(what string, d apiDestination, want map[int64]wantDecision) apiJob {
	e.t.Helper()
	byRel := map[string]int64{}
	for id, m := range e.films {
		byRel[m.rel] = id
	}
	fail := func(where string, id int64, diffs []string) {
		e.t.Helper()
		if len(diffs) > 0 {
			e.t.Errorf("%s, %s, movie %d (%s):\n  %s", what, where, id, e.films[id].rel, strings.Join(diffs, "\n  "))
		}
	}
	_, items := tierPreview(e.s.client, d.ID)
	seen := map[int64]bool{}
	for _, it := range items {
		id, ok := byRel[it.RelPath]
		if !ok || it.SourceID != e.src.ID || seen[id] {
			e.t.Fatalf("%s: preview item %+v", what, it)
		}
		seen[id] = true
		fail("preview", id, diffDecision(it.decision(), want[id]))
	}
	if len(seen) != len(want) {
		e.t.Fatalf("%s: the preview lists %d files, want %d", what, len(seen), len(want))
	}

	dry := e.s.runSync(d.ID, map[string]any{"dryRun": true})
	if dry.Status != "completed" && dry.Status != "completed_with_warnings" {
		e.t.Fatalf("%s: dry run %+v", what, dry)
	}
	seen = map[int64]bool{}
	for _, it := range tierItems(e.s.client, dry.ID, "") {
		id, ok := byRel[it.Detail.Source]
		if !ok {
			continue // a folder or a record without a live file
		}
		if it.Detail.Tier == nil {
			e.t.Fatalf("%s: dry-run item %d (%s %s) has no tier decision", what, it.ID, it.Action, it.RelPath)
		}
		seen[id] = true
		fail(fmt.Sprintf("dry-run item %d (%s)", it.ID, it.Action), id, diffDecision(*it.Detail.Tier, want[id]))
	}
	if len(seen) != len(want) {
		e.t.Fatalf("%s: the dry run has items for %d files, want %d", what, len(seen), len(want))
	}
	if e.t.Failed() {
		e.t.FailNow()
	}
	return dry
}

// syncOK runs a real sync of d and requires it to complete.
func (e *tierEnv) syncOK(what string, d apiDestination, body map[string]any) (apiJob, tierSyncStats) {
	e.t.Helper()
	j := e.s.runSync(d.ID, body)
	if j.Status != "completed" && j.Status != "completed_with_warnings" {
		e.t.Fatalf("%s: sync %+v", what, j)
	}
	return j, decodeStats[tierSyncStats](e.t, j)
}

// TestTiersE2E runs, against one binary: the facts of the fakes joined per file; no rules (all
// full); the pinned table, R0 inserted, R0 unknown with the Maintainerr cache stale, and a changed
// rule (preview and dry run each time); acceptance 7 (enabled, stale, disabled); acceptance 9 (kept,
// release preview, a 409 after the rules change, a release that skips a file flagged since).
func TestTiersE2E(t *testing.T) {
	e := newTierEnv(t)
	srcBefore := snapshot(t, e.movies)
	c := e.s.client
	arrR := func(ruleID int64, labels string, res string) wantReason {
		return wantReason{ruleID: ruleID, field: "arr.tag", op: "has", value: `"bunkarr-full"`, actual: labels, result: res, kind: "arr", integ: e.radarrID}
	}
	maintR := func(ruleID int64, actual, res string, why *regexp.Regexp) wantReason {
		return wantReason{ruleID: ruleID, field: "maintainerr.pendingDelete", op: "is", value: "true", actual: actual, result: res,
			kind: "maintainerr", integ: e.fakes.maintID, why: why}
	}
	staleWhy := regexp.MustCompile(`^Maintainerr cache is .+ old \(stale after 1 h\)$`)

	// The facts of the three files are known and joined: Radarr, Plex, Tautulli, Seerr, Maintainerr.
	{
		_, items := tierPreview(c, e.nas.ID)
		for _, it := range items {
			var fd struct {
				Facts struct {
					Arr struct {
						State string `json:"state"`
					} `json:"arr"`
					Plex *struct {
						Known   bool   `json:"known"`
						Section string `json:"section"`
					} `json:"plex"`
					Watch *struct {
						Known bool `json:"known"`
					} `json:"watch"`
					Requests *struct {
						Requested string  `json:"requested"`
						Users     []int64 `json:"users"`
					} `json:"requests"`
					Maintainerr *struct {
						Pending string `json:"pending"`
					} `json:"maintainerr"`
					Unknown []json.RawMessage `json:"unknown"`
				} `json:"facts"`
			}
			c.call(http.StatusOK, "GET", fmt.Sprintf("/catalog/files/%d", it.FileID), nil, &fd)
			f := fd.Facts
			charade, friday := it.RelPath == e.films[3].rel, it.RelPath == e.films[2].rel
			if f.Arr.State != "item" || f.Plex == nil || !f.Plex.Known || f.Plex.Section != fmt.Sprintf("%d:1", e.fakes.plexID) ||
				f.Watch == nil || !f.Watch.Known || f.Requests == nil || f.Requests.Requested != map[bool]string{true: "true", false: "false"}[friday] ||
				f.Maintainerr == nil || f.Maintainerr.Pending != map[bool]string{true: "true", false: "false"}[charade] || len(f.Unknown) != 0 {
				t.Fatalf("facts of %s: %+v", it.RelPath, f)
			}
		}
	}

	// No rules: every file is full by the built-in fallback (the user's decision D1).
	e.requireTable("no rules", e.nas, map[int64]wantDecision{1: fallbackDecision(), 2: fallbackDecision(), 3: fallbackDecision()})

	// Acceptance 6, the pinned table: R1 `arr.tag has bunkarr-full` → full, R2 (no conditions) →
	// manifest.
	r1 := map[string]any{"name": "R1", "conditions": []map[string]any{{"field": "arr.tag", "op": "has", "value": "bunkarr-full"}}, "action": "full"}
	r2 := map[string]any{"name": "R2", "conditions": []map[string]any{}, "action": "manifest"}
	rs := saveRules(c, r1, r2)
	id1, id2 := rs.Rules[0].ID, rs.Rules[1].ID
	r1["id"], r2["id"] = id1, id2
	full1 := wantDecision{tier: "full", ruleID: id1, ruleName: "R1", reasons: []wantReason{arrR(id1, `["bunkarr-full"]`, "true")}}
	byR2 := func(unknown ...wantReason) wantDecision {
		return wantDecision{tier: "manifest", ruleID: id2, ruleName: "R2", unknown: unknown}
	}
	dry := e.requireTable("R1, R2", e.nas, map[int64]wantDecision{1: full1, 2: byR2(), 3: byR2()})
	// The job items can be filtered by tier: the two files that are not copied.
	if skips := tierItems(c, dry.ID, "tier=manifest"); len(skips) != 2 || skips[0].Action != "skip" || skips[0].Detail.Reason != "not copied" {
		t.Fatalf("dry-run items of tier manifest: %+v", skips)
	}

	// R0 `maintainerr.pendingDelete is true` → skip is inserted first; Charade is pending.
	r0 := map[string]any{"name": "R0", "conditions": []map[string]any{{"field": "maintainerr.pendingDelete", "op": "is", "value": true}}, "action": "skip"}
	rs = saveRules(c, r0, r1, r2)
	id0 := rs.Rules[0].ID
	r0["id"] = id0
	skip0 := wantDecision{tier: "skip", ruleID: id0, ruleName: "R0", reasons: []wantReason{maintR(id0, maintDeleteAfter, "true", nil)}}
	e.requireTable("R0, R1, R2", e.nas, map[int64]wantDecision{1: full1, 2: byR2(), 3: skip0})

	// The Maintainerr cache is stale (older than its staleAfterHours of 1 h): R0 is unknown
	// everywhere, but skip is less protective than manifest, so Charade is manifest by R2.
	e.skew("2h")
	r0Unknown := maintR(id0, "", "unknown", staleWhy)
	full1Stale := full1
	full1Stale.unknown = []wantReason{r0Unknown}
	e.requireTable("R0, R1, R2 with Maintainerr stale", e.nas, map[int64]wantDecision{1: full1Stale, 2: byR2(r0Unknown), 3: byR2(r0Unknown)})
	p, _ := tierPreview(c, e.nas.ID)
	if len(p.UnknownSources) != 1 || p.UnknownSources[0].IntegrationID != e.fakes.maintID || !staleWhy.MatchString(p.UnknownSources[0].Reason) {
		t.Fatalf("unknown sources with Maintainerr stale: %+v", p.UnknownSources)
	}
	e.skew("")

	// A rule is changed: R1 also matches a Seerr request by user 2 (His Girl Friday's requester).
	r1["match"] = "any"
	r1["conditions"] = []map[string]any{{"field": "arr.tag", "op": "has", "value": "bunkarr-full"}, {"field": "seerr.requestedBy", "op": "in", "value": []int64{2}}}
	saveRules(c, r0, r1, r2)
	e.requireTable("R1 changed", e.nas, map[int64]wantDecision{1: full1, 3: skip0, 2: {tier: "full", ruleID: id1, ruleName: "R1",
		reasons: []wantReason{{ruleID: id1, cond: 1, field: "seerr.requestedBy", op: "in", value: "[2]", actual: "[2]", result: "true", kind: "seerr", integ: e.fakes.seerrID}}}})

	// Acceptance 7: with "pending deletion → skip", the pending Charade is not copied.
	saveRules(c, r0)
	_, st := e.syncOK("pending → skip", e.nas, nil)
	e.requireAt("pending → skip", e.nas, 1, 2)
	if st.Tiers == nil || st.Tiers.Full.Files != 2 || st.Tiers.Skip.Files != 1 || st.FilesCopied != 2 {
		t.Fatalf("stats with pending → skip: %+v", st)
	}
	// With the Maintainerr cache stale it is copied: unknown is never true (S14).
	e.skew("2h")
	j, st := e.syncOK("pending → skip, Maintainerr stale", e.nas, nil)
	e.requireAt("pending → skip, Maintainerr stale", e.nas, 1, 2, 3)
	copies := tierItems(c, j.ID, "action=copy")
	if len(copies) != 1 || copies[0].Detail.Source != e.films[3].rel || copies[0].Status != "done" || copies[0].Detail.Tier == nil {
		t.Fatalf("stale sync copies: %+v", copies)
	}
	if d := diffDecision(*copies[0].Detail.Tier, fallbackDecision(r0Unknown)); len(d) > 0 {
		t.Fatalf("stale copy's decision:\n  %s", strings.Join(d, "\n  "))
	}
	e.skew("")
	// With the rule disabled every file is copied.
	r0["enabled"] = false
	saveRules(c, r0)
	e.syncOK("pending → skip disabled", e.off, nil)
	e.requireAt("pending → skip disabled", e.off, 1, 2, 3)
	r0["enabled"] = true

	// Acceptance 9: Charade is backed up at NAS (copied while the cache was stale) and is skip again.
	// A demoted backup is kept: no retain, the file stays (S15).
	saveRules(c, r0)
	_, st = e.syncOK("demoted: Charade", e.nas, nil)
	if st.FilesKept != 1 || st.FilesRetained != 0 || st.FilesPlanned != 0 {
		t.Fatalf("stats with Charade demoted: %+v", st)
	}
	e.requireAt("demoted: Charade", e.nas, 1, 2, 3)
	dry = e.s.runSync(e.nas.ID, map[string]any{"dryRun": true})
	kept := tierItems(c, dry.ID, "action=skip")
	if len(kept) != 1 || kept[0].Detail.Reason != "kept" || kept[0].Detail.Source != e.films[3].rel || kept[0].Detail.Tier == nil ||
		kept[0].Detail.Tier.Tier != "skip" || kept[0].Detail.Tier.RuleID != id0 {
		t.Fatalf("dry-run kept items: %+v", kept)
	}
	// Everything else demoted to manifest: still nothing leaves the destination. (R2 was deleted
	// with the last save: it comes back as a new rule.)
	delete(r2, "id")
	rs = saveRules(c, r0, r2)
	r2["id"] = rs.Rules[1].ID
	_, st = e.syncOK("demoted: all", e.nas, nil)
	if st.FilesKept != 3 || st.FilesRetained != 0 || st.FilesPlanned != 0 || st.Tiers.Full.Files != 0 {
		t.Fatalf("stats with every file demoted: %+v", st)
	}
	e.requireAt("demoted: all", e.nas, 1, 2, 3)

	// The release preview lists the three kept records.
	releasePreview := func() (apiJob, int64) {
		t.Helper()
		dry := e.s.runSync(e.nas.ID, map[string]any{"dryRun": true, "releaseDemoted": true})
		st := decodeStats[tierSyncStats](t, dry)
		rel := tierItems(c, dry.ID, "action=retain")
		if dry.Status != "completed" || st.FilesReleased != 3 || len(rel) != 3 || st.TierRevision == 0 {
			t.Fatalf("release preview %+v: %+v", dry, rel)
		}
		for _, it := range rel {
			if it.Detail.Reason != "released" || it.Detail.TierRevision != st.TierRevision {
				t.Fatalf("release preview item %+v", it)
			}
		}
		return dry, st.TierRevision
	}
	p1, rev1 := releasePreview()
	// The rules change after the preview: its release is refused.
	saveRules(c, r0, r2)
	code, body, err := c.do("POST", fmt.Sprintf("/destinations/%d/sync", e.nas.ID), map[string]any{"releaseDemoted": true, "releaseOf": p1.ID, "releaseRevision": rev1})
	if err != nil || code != http.StatusConflict {
		t.Fatalf("release of a preview of other rules: HTTP %d %s (%v), want 409", code, body, err)
	}
	p2, rev2 := releasePreview()
	// His Girl Friday is flagged irreplaceable after the preview: full again, it is not released.
	friday := e.films[2].rel
	c.call(http.StatusCreated, "POST", "/tiers/flags", map[string]any{"flag": "irreplaceable", "target": map[string]any{"sourceId": e.src.ID, "relPath": friday}}, nil)
	j, st = e.syncOK("release", e.nas, map[string]any{"releaseDemoted": true, "releaseOf": p2.ID, "releaseRevision": rev2})
	released := tierItems(c, j.ID, "action=retain")
	var got []string
	for _, it := range released {
		if it.Status != "done" || it.Detail.Reason != "released" {
			t.Fatalf("release item %+v", it)
		}
		got = append(got, it.Detail.Source)
	}
	slices.Sort(got)
	want := []string{e.films[3].rel, e.films[1].rel}
	slices.Sort(want)
	if !slices.Equal(got, want) || st.FilesReleased != 2 || st.FilesKept != 0 {
		t.Fatalf("released %v (stats %+v), want %v", got, st, want)
	}
	e.requireAt("released", e.nas, 2)
	for _, id := range []int64{1, 3} {
		r := retained(t, e.targets[e.nas.ID], e.src.DestFolder, e.films[id].rel)
		if len(r) != 1 {
			t.Fatalf("retained copies of movie %d: %v", id, r)
		}
		sum, _, err := sha256File(r[0])
		want, _, _ := sha256File(filepath.Join(e.movies, filepath.FromSlash(e.films[id].rel)))
		if err != nil || sum != want {
			t.Fatalf("retained copy of movie %d: %v", id, err)
		}
	}
	// A further sync plans nothing; the source was never touched (S1).
	_, st = e.syncOK("after the release", e.nas, nil)
	if st.FilesPlanned != 0 || st.FilesRetained != 0 || st.FilesKept != 0 {
		t.Fatalf("sync after the release: %+v", st)
	}
	requireUnchanged(t, "the Movies source", srcBefore, e.movies)
}
