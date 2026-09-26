//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/integrations/arr"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr/arrtest"
	"github.com/sl0wz3r/bunkarr/internal/manifest"
	"github.com/sl0wz3r/bunkarr/internal/manifest/manifesttest"
)

// The webhook path of the real binary (docs/design/phase2-3.md §15 E2E): fake Radarr and Sonarr
// (arrtest, serving the recorded fixtures) and the recorded webhook payloads of testdata/webhooks,
// posted to the binary with each integration's webhook key. The processor's windows are shortened
// through internal/testhooks (the binary is built with -tags e2e).

const (
	// hookArrKey is the fake *arrs' API key.
	hookArrKey = "e2e0123456789abcdef0123456789ab"
	// hookQuiet, hookCap and hookDeleteDelay are the shortened webhook windows.
	hookQuiet       = time.Second
	hookCap         = 10 * time.Second
	hookDeleteDelay = 3 * time.Second
	// hookDeadline is acceptance 1's bound: the import is at the destination this long after it.
	hookDeadline = 60 * time.Second
)

// timedJob is a job with its params and times.
type timedJob struct {
	bunkarrJob
	QueuedAt   time.Time  `json:"queuedAt"`
	StartedAt  *time.Time `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt"`
}

// hookEnv is a running binary with a Movies and a TV source, two destinations holding both, and
// a fake Radarr and Sonarr whose webhooks it receives.
type hookEnv struct {
	t      *testing.T
	s      *server
	movies string
	tv     string
	dests  []apiDestination
	// targets are the destinations' target directories, in dests order.
	targets []string

	radarr, sonarr       *arrtest.Server
	radarrID, sonarrID   int64
	radarrKey, sonarrKey string
}

// hookFixture returns the body of a recorded webhook (testdata/webhooks/<app>/<name>).
func hookFixture(t *testing.T, app, name string) []byte {
	t.Helper()
	root, err := moduleRoot()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "testdata", "webhooks", app, name))
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Body json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(raw, &capture); err != nil || len(capture.Body) == 0 {
		t.Fatalf("webhook fixture %s/%s: %v", app, name, err)
	}
	return capture.Body
}

// arrFiles returns the path and size of every file a fake *arr's fixtures report (Radarr: the
// movies' nested files; Sonarr: every series' episode files).
func arrFiles(t *testing.T, kind arr.Kind) map[string]int64 {
	t.Helper()
	type file struct {
		Path string `json:"path"`
		Size int64  `json:"size"`
	}
	out := map[string]int64{}
	switch kind {
	case arr.KindRadarr:
		var list []struct {
			MovieFile *file `json:"movieFile"`
		}
		if err := json.Unmarshal(arrtest.Fixture(t, kind, "movie.json"), &list); err != nil {
			t.Fatal(err)
		}
		for _, m := range list {
			if m.MovieFile != nil {
				out[m.MovieFile.Path] = m.MovieFile.Size
			}
		}
	case arr.KindSonarr:
		for _, name := range []string{"episodefile-seriesId-1.json", "episodefile-seriesId-2.json"} {
			var list []file
			if err := json.Unmarshal(arrtest.Fixture(t, kind, name), &list); err != nil {
				t.Fatal(err)
			}
			for _, f := range list {
				out[f.Path] = f.Size
			}
		}
	}
	return out
}

// Local names of the Radarr fixtures' files (the *arr's /movies is the Movies source).
const (
	notldFile     = "Night of the Living Dead (1968)/Night of the Living Dead (1968) [Bluray-1080p].mkv"
	fridayOldFile = "His Girl Friday (1940)/His.Girl.Friday.1940.1080p.BluRay.x264-PDGRP.mkv"
	fridayFile    = "His Girl Friday (1940)/His Girl Friday (1940) [Bluray-1080p].mkv"
	charadeFile   = "Charade (1963)/Charade (1963) [Bluray-1080p].mkv"
)

// newHookEnv starts the binary with shortened webhook windows, backs up both sources in full,
// then adds the fake Radarr and Sonarr and waits for their first refreshes.
//
// The Movies source starts as Radarr's recording was before its last steps: Night of the Living
// Dead in place, His Girl Friday under its name before the rename (Rename.json), and Charade's
// folder without its file (Download.json imports it).
func newHookEnv(t *testing.T) *hookEnv {
	t.Helper()
	root := resolvedTempDir(t)
	e := &hookEnv{t: t, s: newServer(t, root), movies: filepath.Join(root, "media", "movies"), tv: filepath.Join(root, "media", "tv")}
	sizes := arrFiles(t, arr.KindRadarr)
	for rel, arrPath := range map[string]string{
		notldFile:     "/movies/" + notldFile,
		fridayOldFile: "/movies/" + fridayFile,
	} {
		writeFile(t, e.movies, rel, content(rel, 1, int(sizes[arrPath])))
	}
	mkdirAll(t, filepath.Join(e.movies, "Charade (1963)"))
	for p, size := range arrFiles(t, arr.KindSonarr) {
		rel := strings.TrimPrefix(p, "/tv/")
		writeFile(t, e.tv, rel, content(rel, 1, int(size)))
	}

	e.s.start(
		"BUNKARR_TEST_WEBHOOK_QUIET="+hookQuiet.String(),
		"BUNKARR_TEST_WEBHOOK_CAP="+hookCap.String(),
		"BUNKARR_TEST_WEBHOOK_DELETE_DELAY="+hookDeleteDelay.String(),
	)
	e.s.setup()
	movies := e.s.createSource("Movies", e.movies)
	tv := e.s.createSource("TV", e.tv)
	for _, src := range []apiSource{movies, tv} {
		var scan apiJob
		e.s.call(http.StatusAccepted, "POST", fmt.Sprintf("/sources/%d/scan", src.ID), nil, &scan)
		if j := e.s.waitJob(scan.ID, time.Minute); j.Status != "completed" {
			t.Fatalf("scan %+v", j)
		}
	}
	for _, name := range []string{"NAS", "Offsite"} {
		target := filepath.Join(root, "backup-"+strings.ToLower(name))
		mkdirAll(t, target)
		e.targets = append(e.targets, target)
		d := e.s.createDestination(name, target, movies.ID, tv.ID)
		e.dests = append(e.dests, d)
		if j := e.s.runSync(d.ID, nil); j.Status != "completed" {
			t.Fatalf("first sync of %s: %+v", name, j)
		}
	}

	e.radarr = arrtest.NewServer(t, arr.KindRadarr, hookArrKey)
	e.sonarr = arrtest.NewServer(t, arr.KindSonarr, hookArrKey)
	e.radarrID, e.radarrKey = e.addArr("radarr", "Radarr", e.radarr.URL, "/movies", e.movies)
	e.sonarrID, e.sonarrKey = e.addArr("sonarr", "Sonarr", e.sonarr.URL, "/tv", e.tv)
	for _, id := range []int64{e.radarrID, e.sonarrID} {
		for _, j := range e.waitRefreshes(id) {
			if j.Status != "completed" && j.Status != "completed_with_warnings" {
				t.Fatalf("first refresh of integration %d: %+v", id, j)
			}
		}
	}
	e.waitIdle()
	return e
}

// addArr creates an *arr integration with one path mapping and returns its id and webhook key.
func (e *hookEnv) addArr(typ, name, url, arrRoot, local string) (int64, string) {
	e.t.Helper()
	var it struct {
		ID int64 `json:"id"`
	}
	e.s.call(http.StatusCreated, "POST", "/integrations", map[string]any{"type": typ, "name": name, "url": url, "apiKey": hookArrKey,
		"settings": map[string]any{"pathMappings": []map[string]string{{"arr": arrRoot, "local": local}}}}, &it)
	var k struct {
		Key string `json:"key"`
	}
	e.s.call(http.StatusOK, "POST", fmt.Sprintf("/integrations/%d/webhook/key", it.ID), map[string]bool{"rotate": false}, &k)
	if k.Key == "" {
		e.t.Fatalf("%s: no webhook key", name)
	}
	return it.ID, k.Key
}

// jobsOf returns the jobs of a type with an id above after, oldest first.
func (e *hookEnv) jobsOf(typ string, after int64) []timedJob {
	e.t.Helper()
	var page apiPage[timedJob]
	e.s.call(http.StatusOK, "GET", "/jobs?pageSize=500&type="+typ, nil, &page)
	var out []timedJob
	for _, j := range page.Records {
		if j.ID > after {
			out = append(out, j)
		}
	}
	slices.SortFunc(out, func(a, b timedJob) int { return int(a.ID - b.ID) })
	return out
}

// maxJob returns the highest job id.
func (e *hookEnv) maxJob() int64 {
	e.t.Helper()
	var page apiPage[apiJob]
	e.s.call(http.StatusOK, "GET", "/jobs?pageSize=1", nil, &page)
	if len(page.Records) == 0 {
		return 0
	}
	return page.Records[0].ID
}

// waitRefreshes waits until integration id has a refresh and all of them are final.
func (e *hookEnv) waitRefreshes(id int64) []timedJob {
	e.t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		var mine []timedJob
		busy := false
		for _, j := range e.jobsOf("refresh", 0) {
			if j.Params.IntegrationID == id {
				mine = append(mine, j)
				busy = busy || !j.final()
			}
		}
		if len(mine) > 0 && !busy {
			return mine
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("the refreshes of integration %d did not finish: %+v", id, mine)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// waitIdle waits until no job is queued or running (follow-up manifest exports included).
func (e *hookEnv) waitIdle() {
	e.t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		busy := 0
		for _, st := range []string{"queued", "running"} {
			var page apiPage[apiJob]
			e.s.call(http.StatusOK, "GET", "/jobs?pageSize=1&status="+st, nil, &page)
			busy += int(page.TotalRecords)
		}
		if busy == 0 {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("%d jobs still queued or running", busy)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// post sends a webhook body to /api/v1/webhook/<app>/<id> with the key as the Basic password
// (as the *arrs send it) and returns the status.
func (e *hookEnv) post(app string, id int64, key string, body []byte) int {
	e.t.Helper()
	code, _ := e.postWith(fmt.Sprintf("/webhook/%s/%d", app, id), body, func(r *http.Request) { r.SetBasicAuth(app, key) })
	return code
}

// postWith sends a webhook body to path (under /api/v1) after tweak and returns the status and
// the answer.
func (e *hookEnv) postWith(p string, body []byte, tweak func(*http.Request)) (int, string) {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.s.base+p, bytes.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if tweak != nil {
		tweak(req)
	}
	res, err := e.s.hc.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

// folderSyncs waits, until deadline, for one finished sync with trigger webhook and paths
// [folder] per destination among the jobs after `after` that has work (an item that is not a
// skip) and for every sync after `after` to be final. It returns them in dests order with their
// items.
func (e *hookEnv) folderSyncs(after int64, folder string, deadline time.Time) ([]timedJob, [][]arrItem) {
	e.t.Helper()
	for {
		found := make([]timedJob, len(e.dests))
		items := make([][]arrItem, len(e.dests))
		allFinal, complete := true, true
		for _, j := range e.jobsOf("sync", after) {
			if !j.final() {
				allFinal = false
				continue
			}
			if j.Trigger != "webhook" || !slices.Equal(j.Params.Paths, []string{folder}) {
				continue
			}
			its := e.items(j.ID)
			if len(work(its)) == 0 {
				continue
			}
			for i, d := range e.dests {
				if d.ID == j.Params.DestinationID {
					if found[i].ID != 0 {
						e.t.Fatalf("two webhook syncs of %s did work on %s: %d and %d", folder, d.Name, found[i].ID, j.ID)
					}
					found[i], items[i] = j, its
				}
			}
		}
		for _, j := range found {
			complete = complete && j.ID != 0
		}
		if complete && allFinal {
			return found, items
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("no webhook sync of %q per destination finished in time: %+v", folder, e.jobsOf("sync", after))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// items returns a job's items with their detail.
func (e *hookEnv) items(jobID int64) []arrItem {
	e.t.Helper()
	var p apiPage[arrItem]
	e.s.call(http.StatusOK, "GET", fmt.Sprintf("/jobs/%d/items?pageSize=500", jobID), nil, &p)
	return p.Records
}

// work returns the items that are not skips.
func work(items []arrItem) []arrItem {
	var out []arrItem
	for _, it := range items {
		if it.Action != "skip" {
			out = append(out, it)
		}
	}
	return out
}

// requireSame fails unless the destination's copy of the Movies file rel has the source's content.
func (e *hookEnv) requireSame(target, rel string) {
	e.t.Helper()
	want, _, err := sha256File(filepath.Join(e.movies, filepath.FromSlash(rel)))
	if err != nil {
		e.t.Fatal(err)
	}
	got, _, err := sha256File(filepath.Join(target, "movies", filepath.FromSlash(rel)))
	if err != nil || got != want {
		e.t.Fatalf("%s at %s: %v (sha256 %s, source %s)", rel, target, err, got, want)
	}
}

// setMovieWithoutFile makes the fake Radarr report movie id without a file (GET movie and
// GET movie/{id}), as after a manual file delete.
func (e *hookEnv) setMovieWithoutFile(id int64) {
	e.t.Helper()
	var list []map[string]any
	if err := json.Unmarshal(arrtest.Fixture(e.t, arr.KindRadarr, "movie.json"), &list); err != nil {
		e.t.Fatal(err)
	}
	for _, m := range list {
		if int64(m["id"].(float64)) != id {
			continue
		}
		delete(m, "movieFile")
		m["hasFile"], m["movieFileId"] = false, 0
		if st, ok := m["statistics"].(map[string]any); ok {
			st["movieFileCount"], st["sizeOnDisk"] = 0, 0
		}
		one, err := json.Marshal(m)
		if err != nil {
			e.t.Fatal(err)
		}
		e.radarr.SetJSON(http.MethodGet, fmt.Sprintf("movie/%d", id), one)
	}
	all, err := json.Marshal(list)
	if err != nil {
		e.t.Fatal(err)
	}
	e.radarr.SetJSON(http.MethodGet, "movie", all)
}

// TestWebhookPath is the E2E webhook path (§15 E2E, acceptances 1 and 4 against fakes): a
// Download becomes a targeted refresh and a targeted sync of the item's folder within 60 s; a
// burst of 200 events gives one refresh and one sync per destination; a Rename is a move; a
// manual file delete is retained only by the next full sync; a series delete waits for the
// delete delay; an import during a full refresh is synced after it; and the manifest of the
// result round-trips against the fake *arrs' state.
func TestWebhookPath(t *testing.T) {
	e := newHookEnv(t)

	t.Run("authentication", func(t *testing.T) {
		after := e.maxJob()
		test := hookFixture(t, "radarr", "Test.json")
		if code := e.post("radarr", e.radarrID, e.radarrKey, test); code != http.StatusOK {
			t.Fatalf("Test with the webhook key: HTTP %d", code)
		}
		if code := e.post("sonarr", e.sonarrID, e.sonarrKey, hookFixture(t, "sonarr", "Test.json")); code != http.StatusOK {
			t.Fatalf("Sonarr Test with the webhook key: HTTP %d", code)
		}
		for name, tweak := range map[string]func(*http.Request){
			"a wrong key":         func(r *http.Request) { r.SetBasicAuth("radarr", strings.Repeat("x", len(e.radarrKey))) },
			"no key":              nil,
			"the master API key":  func(r *http.Request) { r.Header.Set("X-Api-Key", e.s.key) },
			"Sonarr's key":        func(r *http.Request) { r.SetBasicAuth("radarr", e.sonarrKey) },
			"the key as a header": func(r *http.Request) { r.Header.Set("X-Api-Key", e.radarrKey) }, // the right key: 200
		} {
			code, body := e.postWith(fmt.Sprintf("/webhook/radarr/%d", e.radarrID), test, tweak)
			want := http.StatusUnauthorized
			if name == "the key as a header" {
				want = http.StatusOK
			}
			if code != want {
				t.Errorf("%s: HTTP %d, want %d: %s", name, code, want, body)
			}
		}
		// The webhook key opens no other route.
		req, _ := http.NewRequest(http.MethodGet, e.s.base+"/system/status", nil)
		req.Header.Set("X-Api-Key", e.radarrKey)
		res, err := e.s.hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("the webhook key on /system/status: HTTP %d", res.StatusCode)
		}
		time.Sleep(hookQuiet + time.Second)
		if jobs := e.jobsOf("refresh", after); len(jobs) != 0 {
			t.Fatalf("Test events queued %+v", jobs)
		}
	})

	t.Run("import", func(t *testing.T) {
		after := e.maxJob()
		sizes := arrFiles(t, arr.KindRadarr)
		writeFile(t, e.movies, charadeFile, content(charadeFile, 1, int(sizes["/movies/"+charadeFile])))
		imported := time.Now()
		if code := e.post("radarr", e.radarrID, e.radarrKey, hookFixture(t, "radarr", "Download.json")); code != http.StatusOK {
			t.Fatalf("Download: HTTP %d", code)
		}
		syncs, items := e.folderSyncs(after, "Charade (1963)", imported.Add(hookDeadline))
		for i, j := range syncs {
			if j.Status != "completed" || j.FinishedAt == nil || j.FinishedAt.Sub(imported) > hookDeadline {
				t.Fatalf("webhook sync %+v (import at %s)", j, imported)
			}
			w := work(items[i])
			if len(w) != 1 || w[0].Action != "copy" || w[0].Status != "done" || w[0].RelPath != "movies/"+charadeFile ||
				w[0].Bytes != sizes["/movies/"+charadeFile] {
				t.Fatalf("sync %d items %+v, want one done copy of %s", j.ID, items[i], charadeFile)
			}
			e.requireSame(e.targets[i], charadeFile)
			t.Logf("%s: the import was copied by sync %d, %s after it", e.dests[i].Name, j.ID, j.FinishedAt.Sub(imported).Round(time.Millisecond))
		}
		refreshes := e.jobsOf("refresh", after)
		if len(refreshes) != 1 || refreshes[0].Trigger != "webhook" || !slices.Equal(refreshes[0].Params.ArrItemIDs, []int64{3}) {
			t.Fatalf("refreshes %+v", refreshes)
		}
		var events apiPage[struct {
			EventType string  `json:"eventType"`
			Outcome   *string `json:"outcome"`
			JobID     *int64  `json:"jobId"`
		}]
		e.s.call(http.StatusOK, "GET", fmt.Sprintf("/webhooks/events?integrationId=%d&eventType=Download", e.radarrID), nil, &events)
		if len(events.Records) != 1 || events.Records[0].Outcome == nil || *events.Records[0].Outcome != "queued" {
			t.Fatalf("webhook events %+v", events.Records)
		}
		e.waitIdle()
	})

	t.Run("rename", func(t *testing.T) {
		after := e.maxJob()
		if err := os.Rename(filepath.Join(e.movies, fridayOldFile), filepath.Join(e.movies, fridayFile)); err != nil {
			t.Fatal(err)
		}
		if code := e.post("radarr", e.radarrID, e.radarrKey, hookFixture(t, "radarr", "Rename.json")); code != http.StatusOK {
			t.Fatalf("Rename: HTTP %d", code)
		}
		syncs, items := e.folderSyncs(after, "His Girl Friday (1940)", time.Now().Add(hookDeadline))
		for i, j := range syncs {
			w := work(items[i])
			st := decodeStats[syncStats](t, j.apiJob)
			if j.Status != "completed" || len(w) != 1 || w[0].Action != "move" || w[0].Status != "done" ||
				w[0].RelPath != "movies/"+fridayFile || st.BytesCopied != 0 {
				t.Fatalf("rename sync %+v: stats %+v, items %+v", j, st, items[i])
			}
			e.requireSame(e.targets[i], fridayFile)
			if _, err := os.Stat(filepath.Join(e.targets[i], "movies", filepath.FromSlash(fridayOldFile))); !os.IsNotExist(err) {
				t.Fatalf("the old name is still at %s: %v", e.dests[i].Name, err)
			}
		}
		e.waitIdle()
	})

	t.Run("burst", func(t *testing.T) {
		after := e.maxJob()
		bodies := [][]byte{hookFixture(t, "radarr", "Download.json"), hookFixture(t, "radarr", "Rename.json"),
			hookFixture(t, "radarr", "Download-upgrade.json")}
		start := time.Now()
		for i := range 200 {
			if code := e.post("radarr", e.radarrID, e.radarrKey, bodies[i%3]); code != http.StatusOK {
				t.Fatalf("burst event %d: HTTP %d", i, code)
			}
		}
		t.Logf("200 events posted in %s", time.Since(start).Round(time.Millisecond))
		deadline := time.Now().Add(hookDeadline)
		for {
			syncs := e.jobsOf("sync", after)
			final := len(syncs) >= len(e.dests)
			for _, j := range syncs {
				final = final && j.final()
			}
			if final {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the burst's syncs did not finish: %+v", syncs)
			}
			time.Sleep(100 * time.Millisecond)
		}
		time.Sleep(hookQuiet + 2*time.Second) // nothing else follows
		refreshes, syncs := e.jobsOf("refresh", after), e.jobsOf("sync", after)
		if len(refreshes) != 1 || refreshes[0].Trigger != "webhook" || !slices.Equal(sortedIDs(refreshes[0].Params.ArrItemIDs), []int64{1, 2, 3}) {
			t.Fatalf("refreshes after the burst: %+v", refreshes)
		}
		if len(syncs) != len(e.dests) {
			t.Fatalf("syncs after the burst: %+v", syncs)
		}
		for _, j := range syncs {
			if j.Trigger != "webhook" || len(j.Params.Paths) != 3 || j.Status != "completed" {
				t.Fatalf("burst sync %+v", j)
			}
		}
		e.waitIdle()
	})

	t.Run("manual file delete", func(t *testing.T) {
		after := e.maxJob()
		if err := os.Remove(filepath.Join(e.movies, fridayFile)); err != nil {
			t.Fatal(err)
		}
		e.setMovieWithoutFile(2)
		// A manual file delete is of class change (§7.2): refreshed after the quiet window.
		if code := e.post("radarr", e.radarrID, e.radarrKey, hookFixture(t, "radarr", "MovieFileDelete-manual.json")); code != http.StatusOK {
			t.Fatalf("MovieFileDelete: HTTP %d", code)
		}
		if refresh := e.waitWebhookRefresh(after, e.radarrID); !slices.Equal(refresh.Params.ArrItemIDs, []int64{2}) {
			t.Fatalf("refresh %+v", refresh)
		}
		// One targeted sync per destination runs for the folder and retains nothing (D14).
		deadline := time.Now().Add(hookDeadline)
		var syncs []timedJob
		for {
			syncs = nil
			final := true
			for _, j := range e.jobsOf("sync", after) {
				syncs = append(syncs, j)
				final = final && j.final()
			}
			if len(syncs) >= len(e.dests) && final {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the delete's syncs did not finish: %+v", syncs)
			}
			time.Sleep(100 * time.Millisecond)
		}
		for _, j := range syncs {
			if j.Trigger != "webhook" || !slices.Equal(j.Params.Paths, []string{"His Girl Friday (1940)"}) || j.Status != "completed" {
				t.Fatalf("delete sync %+v", j)
			}
			if w := work(e.items(j.ID)); len(w) != 0 {
				t.Fatalf("the targeted sync %d did work for a delete: %+v", j.ID, w)
			}
		}
		for _, target := range e.targets {
			if _, err := os.Stat(filepath.Join(target, "movies", filepath.FromSlash(fridayFile))); err != nil {
				t.Fatalf("the deleted file left %s before a full sync: %v", target, err)
			}
		}
		e.waitIdle()
		// The next full sync retains it, with reason deleted.
		for i, d := range e.dests {
			j := e.s.runSync(d.ID, nil)
			w := work(e.items(j.ID))
			if j.Status != "completed" || len(w) != 1 || w[0].Action != "retain" || w[0].Status != "done" ||
				w[0].RelPath != "movies/"+fridayFile || w[0].Detail.RetainedReason != "deleted" {
				t.Fatalf("full sync of %s: %+v, items %+v", d.Name, j, w)
			}
			if got := retainedLiteral(t, e.targets[i], "movies/"+fridayFile); len(got) != 1 {
				t.Fatalf("retention of %s holds %v", d.Name, got)
			}
		}
		e.waitIdle()
	})

	t.Run("series delete waits for the delete delay", func(t *testing.T) {
		after := e.maxJob()
		deleted := time.Now()
		if code := e.post("sonarr", e.sonarrID, e.sonarrKey, hookFixture(t, "sonarr", "SeriesDelete-deletedFiles.json")); code != http.StatusOK {
			t.Fatalf("SeriesDelete: HTTP %d", code)
		}
		refresh := e.waitWebhookRefresh(after, e.sonarrID)
		if d := refresh.QueuedAt.Sub(deleted); d < hookDeleteDelay-100*time.Millisecond {
			t.Fatalf("the series delete's refresh was queued %s after it, before the delete delay %s", d, hookDeleteDelay)
		}
		if !slices.Equal(refresh.Params.ArrItemIDs, []int64{1}) {
			t.Fatalf("refresh %+v", refresh)
		}
		e.waitIdle()
	})

	t.Run("import during a full refresh", func(t *testing.T) {
		after := e.maxJob()
		e.radarr.Delay(http.MethodGet, "movie", 4*time.Second)
		var full apiJob
		e.s.call(http.StatusAccepted, "POST", fmt.Sprintf("/integrations/%d/refresh", e.radarrID), nil, &full)
		for e.s.job(full.ID).Status == "queued" {
			time.Sleep(20 * time.Millisecond)
		}
		const sidecar = "Charade (1963)/Charade (1963) [Bluray-1080p].en.srt"
		writeFile(t, e.movies, sidecar, content(sidecar, 1, 4096))
		if code := e.post("radarr", e.radarrID, e.radarrKey, hookFixture(t, "radarr", "Download.json")); code != http.StatusOK {
			t.Fatalf("Download: HTTP %d", code)
		}
		fullDone := e.s.waitJob(full.ID, time.Minute)
		if fullDone.Status != "completed" && fullDone.Status != "completed_with_warnings" {
			t.Fatalf("full refresh %+v", fullDone)
		}
		var fullJob timedJob
		e.s.call(http.StatusOK, "GET", fmt.Sprintf("/jobs/%d", full.ID), nil, &fullJob)
		syncs, items := e.folderSyncs(after, "Charade (1963)", time.Now().Add(hookDeadline))
		for i, j := range syncs {
			if j.StartedAt == nil || j.StartedAt.Before(*fullJob.FinishedAt) {
				t.Fatalf("the import's sync %d started at %v, before the full refresh finished at %v", j.ID, j.StartedAt, fullJob.FinishedAt)
			}
			w := work(items[i])
			if j.Status != "completed" || len(w) != 1 || w[0].Action != "copy" || w[0].RelPath != "movies/"+sidecar {
				t.Fatalf("sync %+v items %+v", j, items[i])
			}
			e.requireSame(e.targets[i], sidecar)
			e.requireSame(e.targets[i], charadeFile)
		}
		e.waitIdle()
		e.radarr.Delay(http.MethodGet, "movie", 0)
	})

	t.Run("manifest round trip", func(t *testing.T) {
		var exp apiJob
		e.s.call(http.StatusAccepted, "POST", fmt.Sprintf("/destinations/%d/manifest", e.dests[0].ID), nil, &exp)
		if j := e.s.waitJob(exp.ID, time.Minute); j.Status != "completed" && j.Status != "completed_with_warnings" {
			t.Fatalf("manifest export %+v", j)
		}
		var versions []struct {
			ID   int64  `json:"id"`
			Path string `json:"path"`
		}
		e.s.call(http.StatusOK, "GET", fmt.Sprintf("/destinations/%d/manifests", e.dests[0].ID), nil, &versions)
		if len(versions) == 0 {
			t.Fatal("no manifest version")
		}
		live := map[int64]manifest.Plan{}
		for id, a := range map[int64]struct {
			kind string
			srv  *arrtest.Server
		}{e.radarrID: {"radarr", e.radarr}, e.sonarrID: {"sonarr", e.sonarr}} {
			raw, err := manifesttest.LivePlan(a.kind, id, manifesttest.HTTPGetter(a.srv.URL, "/api/v3", hookArrKey))
			if err != nil {
				t.Fatal(err)
			}
			var p manifest.Plan
			if err := json.Unmarshal(raw, &p); err != nil {
				t.Fatal(err)
			}
			live[id] = p
		}
		m, err := manifest.ParseDir(filepath.Join(e.targets[0], filepath.FromSlash(versions[0].Path)))
		if err != nil {
			t.Fatalf("ParseDir: %v", err)
		}
		requireManifestRoundTrip(t, "the destination's newest version", m, live)
		for _, p := range []string{fmt.Sprintf("/manifests/%d/download", versions[0].ID), "/manifest/export"} {
			req, _ := http.NewRequest(http.MethodGet, e.s.base+p, nil)
			req.Header.Set("X-Api-Key", e.s.key)
			res, err := e.s.hc.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode != http.StatusOK || bytes.Contains(body, []byte(hookArrKey)) {
				t.Fatalf("GET %s: HTTP %d (or it holds the *arr key)", p, res.StatusCode)
			}
			parsed, err := manifest.Parse(bytes.NewReader(body))
			if err != nil {
				t.Fatalf("GET %s: %v", p, err)
			}
			requireManifestRoundTrip(t, "GET "+p, parsed, live)
		}
		// The movie without a file is listed with no file; Charade's file is backed up.
		var charade, friday bool
		for _, it := range m.Items {
			if len(it.Files) == 1 && path.Base(it.Files[0].RelativePath) == path.Base(charadeFile) {
				charade = it.Files[0].BackedUp != nil && *it.Files[0].BackedUp
			}
			if it.Title == "His Girl Friday" {
				friday = len(it.Files) == 0
			}
		}
		if !charade || !friday {
			t.Fatalf("manifest items: Charade backed up %v, His Girl Friday without files %v", charade, friday)
		}
	})
}

// waitWebhookRefresh waits for the first refresh with trigger webhook of integration id after
// `after`.
func (e *hookEnv) waitWebhookRefresh(after, id int64) timedJob {
	e.t.Helper()
	deadline := time.Now().Add(hookDeadline)
	for {
		for _, j := range e.jobsOf("refresh", after) {
			if j.Trigger == "webhook" && j.Params.IntegrationID == id {
				return j
			}
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("no webhook refresh of integration %d", id)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// retainedLiteral returns the retained versions of rel (a destination-relative slash path) under
// target. Unlike retained it matches rel literally ("[Bluray-1080p]" is not a glob class).
func retainedLiteral(t *testing.T, target, rel string) []string {
	t.Helper()
	root := filepath.Join(target, ".bunkarr", "retention")
	var out []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if r, _ := filepath.Rel(root, p); !d.IsDir() && strings.HasSuffix(filepath.ToSlash(r), "/"+rel) {
			out = append(out, p)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return out
}

// sortedIDs returns a sorted copy of ids.
func sortedIDs(ids []int64) []int64 {
	out := slices.Clone(ids)
	slices.Sort(out)
	return out
}
