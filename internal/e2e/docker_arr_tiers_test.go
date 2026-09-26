//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Tiers against a real Radarr (docs/design/phase2-3.md §15 Docker suite item 5: acceptances 6 and
// 7, the fresh cases; the stale case runs in TestTiersE2E, which has the clock hook). It runs only
// with BUNKARR_E2E_ARR set (make test-arr), needs Docker, the linuxserver/radarr image
// (BUNKARR_E2E_RADARR_IMAGE overrides it) and internet access for Radarr's metadata lookups (it
// skips with a message without it):
//
//	BUNKARR_E2E_ARR=1 go test -tags e2e -count=1 -run TestDockerArrTiers ./internal/e2e/
//
// Radarr runs in a container with the movies folder bind-mounted from a temporary directory; the
// real bunkarr binary runs on the host with a source over the same folder, like
// TestDockerArrManifestRoundTrip. The Plex library index, Tautulli, Seerr and Maintainerr are the
// fakes of tierfakes_test.go, the Plex section listing the real Radarr's files. The rules use the
// real Radarr's tag and quality profiles: a dry run's reasons equal the expected table, a rule is
// changed and a new dry run shows the new reasons; then Maintainerr marks one movie pending, a real
// sync with "pending deletion → skip" does not copy it, and with the rule disabled it is copied.

// tierRadarrMovie is a movie the test adds to the real Radarr.
type tierRadarrMovie struct {
	id      int64 // the test's id (1-4)
	tmdb    int64
	title   string
	profile string
	full    bool // tagged bunkarr-full
	quality string
}

func TestDockerArrTiers(t *testing.T) {
	if os.Getenv("BUNKARR_E2E_ARR") == "" {
		t.Skip("BUNKARR_E2E_ARR is not set (the real Radarr/Sonarr suite)")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatalf("BUNKARR_E2E_ARR is set but docker is not installed: %v", err)
	}
	image := cmpOr(os.Getenv("BUNKARR_E2E_RADARR_IMAGE"), "lscr.io/linuxserver/radarr:latest")
	root := resolvedTempDir(t)
	movies := filepath.Join(root, "media", "movies")
	for _, d := range []string{movies, filepath.Join(root, "nas")} {
		if err := os.MkdirAll(d, 0o777); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(d, 0o777) // the container's user writes folders here
	}
	radarr := startManifestArr(t, "radarr", image, "/api/v3", 7878, map[string]string{movies: "/movies"})

	// Radarr: A tagged bunkarr-full (HD-1080p), B (HD-720p), C (HD-1080p) and X (HD-720p), each with
	// a file.
	list := []tierRadarrMovie{
		{id: 1, tmdb: 10331, title: "Night of the Living Dead", profile: "HD-1080p", full: true, quality: "Bluray-1080p"},
		{id: 2, tmdb: 3085, title: "His Girl Friday", profile: "HD-720p", quality: "Bluray-720p"},
		{id: 3, tmdb: 961, title: "The General", profile: "HD-1080p", quality: "Bluray-1080p"},
		{id: 4, tmdb: 653, title: "Nosferatu", profile: "HD-720p", quality: "Bluray-720p"},
	}
	radarr.do(http.MethodPost, "rootfolder", map[string]any{"path": "/movies"}, nil)
	var tag struct {
		ID int64 `json:"id"`
	}
	radarr.do(http.MethodPost, "tag", map[string]any{"label": "bunkarr-full"}, &tag)
	films := map[int64]radarrMovie{}
	for _, m := range list {
		var movie map[string]any
		b, err := radarr.try(http.MethodGet, fmt.Sprintf("movie/lookup/tmdb?tmdbId=%d", m.tmdb), nil)
		if err != nil {
			t.Skipf("Radarr cannot look up movies (no internet access?): %v", err)
		}
		if err := json.Unmarshal(b, &movie); err != nil {
			t.Fatal(err)
		}
		tags := []int64{}
		if m.full {
			tags = []int64{tag.ID}
		}
		movie["qualityProfileId"], movie["rootFolderPath"], movie["monitored"], movie["tags"] = radarr.profile(m.profile), "/movies", true, tags
		movie["minimumAvailability"] = "released"
		movie["addOptions"] = map[string]any{"searchForMovie": false, "monitor": "movieOnly"}
		var added struct {
			ID   int64  `json:"id"`
			Path string `json:"path"`
		}
		radarr.do(http.MethodPost, "movie", movie, &added)
		radarr.waitIdle()
		folder := path.Base(added.Path)
		writeMedia(t, filepath.Join(movies, folder, folder+" ["+m.quality+"].mkv"), 300_000+int(m.tmdb))
		radarr.do(http.MethodPost, "command", map[string]any{"name": "RescanMovie", "movieId": added.ID}, nil)
		radarr.waitIdle()
		var got struct {
			HasFile   bool `json:"hasFile"`
			MovieFile *struct {
				Path string `json:"path"`
				Size int64  `json:"size"`
			} `json:"movieFile"`
		}
		radarr.do(http.MethodGet, fmt.Sprintf("movie/%d", added.ID), nil, &got)
		if !got.HasFile || got.MovieFile == nil {
			t.Fatalf("Radarr did not import the file of tmdb %d", m.tmdb)
		}
		films[m.id] = radarrMovie{rel: strings.TrimPrefix(got.MovieFile.Path, "/movies/"), size: got.MovieFile.Size, tmdb: m.tmdb}
	}

	s := newServer(t, root)
	s.start()
	s.setup()
	e := &tierEnv{t: t, s: s, movies: movies, targets: map[int64]string{}, films: films}
	e.src = s.createSource("Movies", movies)
	var scan apiJob
	s.call(http.StatusAccepted, "POST", fmt.Sprintf("/sources/%d/scan", e.src.ID), nil, &scan)
	if j := s.waitJob(scan.ID, 2*time.Minute); j.Status != "completed" {
		t.Fatalf("scan %+v", j)
	}
	e.src = s.source(e.src.ID)
	e.nas = s.createDestination("NAS", filepath.Join(root, "nas"), e.src.ID)
	e.targets[e.nas.ID] = filepath.Join(root, "nas")
	e.radarrID = createIntegration(s.client, map[string]any{"type": "radarr", "name": "Radarr", "url": radarr.url, "apiKey": manifestArrKey,
		"settings": map[string]any{"pathMappings": []map[string]string{{"arr": "/movies", "local": movies}}}})
	waitRefreshes(s.client, e.radarrID)
	var rows []plexMovie
	for _, m := range list {
		f := films[m.id]
		rows = append(rows, plexMovie{RatingKey: fmt.Sprint(2000 + m.id), Title: m.title, Year: 1950, TMDB: m.tmdb, File: "/data/movies/" + f.rel, Size: f.size})
	}
	e.fakes = newTierFakes(t, rows)
	// Maintainerr marks X pending (and keeps the recorded, globally excluded The General).
	e.fakes.setWatchedMovies(maintMember{"2004", 653}, maintMember{"9", 961})
	e.fakes.addIntegrations(s.client, "/data/movies", movies, 24)
	waitJobsIdle(s.client)
	c := s.client

	// The rules on the real Radarr's tag and quality profiles.
	tagR := func(ruleID int64) wantReason {
		return wantReason{ruleID: ruleID, field: "arr.tag", op: "has", value: `"bunkarr-full"`, actual: `["bunkarr-full"]`, result: "true", kind: "arr", integ: e.radarrID}
	}
	profileR := func(ruleID int64, profile string) wantReason {
		return wantReason{ruleID: ruleID, field: "arr.qualityProfile", op: "is", value: `"` + profile + `"`, actual: `"` + profile + `"`, result: "true",
			kind: "arr", integ: e.radarrID}
	}
	r1 := map[string]any{"name": "Tagged", "conditions": []map[string]any{{"field": "arr.tag", "op": "has", "value": "bunkarr-full"}}, "action": "full"}
	r2 := map[string]any{"name": "Profile", "conditions": []map[string]any{{"field": "arr.qualityProfile", "op": "is", "value": "HD-720p"}}, "action": "skip"}
	r3 := map[string]any{"name": "Else", "conditions": []map[string]any{}, "action": "manifest"}
	rs := saveRules(c, r1, r2, r3)
	if len(rs.Warnings) != 0 {
		t.Fatalf("the rules name values the real Radarr does not know: %+v", rs.Warnings)
	}
	id1, id2, id3 := rs.Rules[0].ID, rs.Rules[1].ID, rs.Rules[2].ID
	r1["id"], r2["id"], r3["id"] = id1, id2, id3
	full := wantDecision{tier: "full", ruleID: id1, ruleName: "Tagged", reasons: []wantReason{tagR(id1)}}
	profile := func(p string) wantDecision {
		return wantDecision{tier: "skip", ruleID: id2, ruleName: "Profile", reasons: []wantReason{profileR(id2, p)}}
	}
	manifest := wantDecision{tier: "manifest", ruleID: id3, ruleName: "Else"}
	e.requireTable("tag, profile HD-720p, else", e.nas, map[int64]wantDecision{1: full, 2: profile("HD-720p"), 3: manifest, 4: profile("HD-720p")})

	// A rule is changed: the profile rule now matches HD-1080p.
	r2["conditions"] = []map[string]any{{"field": "arr.qualityProfile", "op": "is", "value": "HD-1080p"}}
	saveRules(c, r1, r2, r3)
	e.requireTable("profile rule changed", e.nas, map[int64]wantDecision{1: full, 2: manifest, 3: profile("HD-1080p"), 4: manifest})

	// Acceptance 7: with "pending deletion → skip" the real sync does not copy X; disabled, it does.
	r0 := map[string]any{"name": "Maintainerr", "conditions": []map[string]any{{"field": "maintainerr.pendingDelete", "op": "is", "value": true}}, "action": "skip"}
	rs = saveRules(c, r0)
	r0["id"] = rs.Rules[0].ID
	e.requireTable("pending → skip", e.nas, map[int64]wantDecision{1: fallbackDecision(), 2: fallbackDecision(), 3: fallbackDecision(),
		4: {tier: "skip", ruleID: rs.Rules[0].ID, ruleName: "Maintainerr", reasons: []wantReason{{ruleID: rs.Rules[0].ID, field: "maintainerr.pendingDelete",
			op: "is", value: "true", actual: maintDeleteAfter, result: "true", kind: "maintainerr", integ: e.fakes.maintID}}}})
	_, st := e.syncOK("pending → skip", e.nas, nil)
	e.requireAt("pending → skip", e.nas, 1, 2, 3)
	if st.FilesCopied != 3 || st.Tiers == nil || st.Tiers.Skip.Files != 1 {
		t.Fatalf("stats with pending → skip: %+v", st)
	}
	r0["enabled"] = false
	saveRules(c, r0)
	_, st = e.syncOK("pending → skip disabled", e.nas, nil)
	e.requireAt("pending → skip disabled", e.nas, 1, 2, 3, 4)
	if st.FilesCopied != 1 {
		t.Fatalf("stats with the rule disabled: %+v", st)
	}
}
