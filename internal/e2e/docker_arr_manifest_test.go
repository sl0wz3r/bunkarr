//go:build e2e

package e2e

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/manifest"
	"github.com/sl0wz3r/bunkarr/internal/manifest/manifesttest"
)

// The manifest round trip against a real Radarr and Sonarr (docs/design/phase2-3.md acceptance 3,
// §15 Docker suite item 3). It runs only with BUNKARR_E2E_ARR set, needs Docker, the
// linuxserver/radarr and linuxserver/sonarr images (BUNKARR_E2E_RADARR_IMAGE and
// BUNKARR_E2E_SONARR_IMAGE override them) and internet access for the *arrs' metadata lookups
// (it skips with a message without it):
//
//	BUNKARR_E2E_ARR=1 go test -tags e2e -count=1 -run TestDockerArrManifestRoundTrip ./internal/e2e/
//
// The *arrs run in containers with the media folders bind-mounted from a temporary directory; the
// real bunkarr binary runs on the host with a source over the same folders and reaches the *arrs
// on their published loopback ports. The expected state is decoded by manifesttest from the
// *arrs' raw API JSON, without Bunkarr's *arr client or index.

const manifestArrKey = "e2e0123456789abcdef0123456789abc"

// manifestArr is one running *arr container.
type manifestArr struct {
	t      *testing.T
	kind   string // radarr or sonarr
	name   string // container name
	url    string // http://127.0.0.1:<port>
	prefix string // /api/v3
}

func manifestArrDocker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// startManifestArr runs an *arr container with mounts (host:container) and waits until its API
// answers.
func startManifestArr(t *testing.T, kind, image, prefix string, port int, mounts map[string]string) *manifestArr {
	t.Helper()
	var rnd [3]byte
	_, _ = rand.Read(rnd[:])
	name := fmt.Sprintf("bunkarr-e2e-manifest-%d-%s-%s", os.Getpid(), hex.EncodeToString(rnd[:]), kind)
	args := []string{"run", "-d", "--name", name, "-e", "PUID=1000", "-e", "PGID=1000", "-e", "TZ=Etc/UTC",
		"-e", strings.ToUpper(kind) + "__AUTH__APIKEY=" + manifestArrKey, "-p", fmt.Sprintf("127.0.0.1::%d", port)}
	for host, ctr := range mounts {
		args = append(args, "-v", host+":"+ctr)
	}
	manifestArrDocker(t, append(args, image)...)
	t.Cleanup(func() {
		if t.Failed() {
			out, _ := exec.Command("docker", "logs", "--tail", "60", name).CombinedOutput()
			t.Logf("---- docker logs %s ----\n%s", name, out)
		}
		_ = exec.Command("docker", "rm", "-f", "-v", name).Run()
	})
	hostPort := manifestArrDocker(t, "port", name, fmt.Sprint(port))
	hostPort = strings.SplitN(hostPort, "\n", 2)[0]
	a := &manifestArr{t: t, kind: kind, name: name, url: "http://" + hostPort, prefix: prefix}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		if _, err := a.try(http.MethodGet, "system/status", nil); err == nil {
			return a
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not answer within 3 minutes", kind)
		}
		time.Sleep(time.Second)
	}
}

func (a *manifestArr) try(method, target string, body any) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, a.url+a.prefix+"/"+target, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Api-Key", manifestArrKey)
	req.Header.Set("Content-Type", "application/json")
	res, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 300 {
		return b, fmt.Errorf("%s %s: HTTP %d: %s", method, target, res.StatusCode, b)
	}
	return b, nil
}

// do sends an API request that must succeed and decodes the answer into out (when not nil).
func (a *manifestArr) do(method, target string, body, out any) {
	a.t.Helper()
	b, err := a.try(method, target, body)
	if err != nil {
		a.t.Fatal(err)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			a.t.Fatalf("%s %s: %v", method, target, err)
		}
	}
}

// waitIdle waits until the *arr runs no command.
func (a *manifestArr) waitIdle() {
	a.t.Helper()
	time.Sleep(2 * time.Second)
	deadline := time.Now().Add(5 * time.Minute)
	for {
		var cmds []struct {
			Status string `json:"status"`
		}
		a.do(http.MethodGet, "command", nil, &cmds)
		busy := false
		for _, c := range cmds {
			busy = busy || c.Status == "queued" || c.Status == "started"
		}
		if !busy {
			return
		}
		if time.Now().After(deadline) {
			a.t.Fatalf("%s commands did not finish", a.kind)
		}
		time.Sleep(time.Second)
	}
}

// profile returns the id of the quality profile named name (the first one when none is).
func (a *manifestArr) profile(name string) int64 {
	a.t.Helper()
	var list []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	a.do(http.MethodGet, "qualityprofile", nil, &list)
	for _, p := range list {
		if p.Name == name {
			return p.ID
		}
	}
	return list[0].ID
}

// writeMedia writes a file of random bytes: the *arrs import an existing file from its name on a
// rescan, whatever its content.
func writeMedia(t *testing.T, p string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, size)
	_, _ = rand.Read(b)
	if err := os.WriteFile(p, b, 0o666); err != nil {
		t.Fatal(err)
	}
}

// setupManifestRadarr adds four movies to /movies (two with a file, one unmonitored, one without a
// file) and one with a file to /movies-4k, which no path mapping covers.
func setupManifestRadarr(t *testing.T, a *manifestArr, movies, movies4k string) {
	t.Helper()
	a.do(http.MethodPost, "rootfolder", map[string]any{"path": "/movies"}, nil)
	a.do(http.MethodPost, "rootfolder", map[string]any{"path": "/movies-4k"}, nil)
	var tag struct {
		ID int64 `json:"id"`
	}
	a.do(http.MethodPost, "tag", map[string]any{"label": "bunkarr-full"}, &tag)
	hd := a.profile("HD-1080p")
	for _, m := range []struct {
		tmdb      int64
		root      string
		monitored bool
		tags      []int64
		file      string
	}{
		{10331, "/movies", true, []int64{tag.ID}, "Bluray-1080p"},
		{3085, "/movies", false, []int64{}, "Bluray-1080p"},
		{961, "/movies", true, []int64{tag.ID}, ""},
		{653, "/movies-4k", true, []int64{}, "Bluray-2160p"},
	} {
		var movie map[string]any
		b, err := a.try(http.MethodGet, fmt.Sprintf("movie/lookup/tmdb?tmdbId=%d", m.tmdb), nil)
		if err != nil {
			t.Skipf("Radarr cannot look up movies (no internet access?): %v", err)
		}
		if err := json.Unmarshal(b, &movie); err != nil {
			t.Fatal(err)
		}
		movie["qualityProfileId"], movie["rootFolderPath"], movie["monitored"], movie["tags"] = hd, m.root, m.monitored, m.tags
		movie["minimumAvailability"] = "released"
		movie["addOptions"] = map[string]any{"searchForMovie": false, "monitor": "movieOnly"}
		var added struct {
			ID   int64  `json:"id"`
			Path string `json:"path"`
		}
		a.do(http.MethodPost, "movie", movie, &added)
		a.waitIdle()
		if m.file == "" {
			continue
		}
		base := movies
		if m.root == "/movies-4k" {
			base = movies4k
		}
		folder := path.Base(added.Path)
		writeMedia(t, filepath.Join(base, folder, folder+" ["+m.file+"].mkv"), 300_000+int(m.tmdb))
		a.do(http.MethodPost, "command", map[string]any{"name": "RescanMovie", "movieId": added.ID}, nil)
		a.waitIdle()
		var got struct {
			HasFile bool `json:"hasFile"`
		}
		a.do(http.MethodGet, fmt.Sprintf("movie/%d", added.ID), nil, &got)
		if !got.HasFile {
			t.Fatalf("Radarr did not import the file of tmdb %d", m.tmdb)
		}
	}
}

// setupManifestSonarr adds a series with two episode files (one a multi-episode file) and a
// series without files.
func setupManifestSonarr(t *testing.T, a *manifestArr, tv string) {
	t.Helper()
	a.do(http.MethodPost, "rootfolder", map[string]any{"path": "/tv"}, nil)
	var tag struct {
		ID int64 `json:"id"`
	}
	a.do(http.MethodPost, "tag", map[string]any{"label": "irreplaceable"}, &tag)
	hd := a.profile("HD-1080p")
	for i, tvdb := range []int64{71471, 76479} {
		var found []map[string]any
		b, err := a.try(http.MethodGet, fmt.Sprintf("series/lookup?term=tvdb:%d", tvdb), nil)
		if err != nil || json.Unmarshal(b, &found) != nil || len(found) == 0 {
			t.Skipf("Sonarr cannot look up series (no internet access?): %v", err)
		}
		s := found[0]
		s["qualityProfileId"], s["languageProfileId"], s["rootFolderPath"], s["monitored"] = hd, 1, "/tv", true
		s["seasonFolder"], s["seriesType"], s["monitorNewItems"], s["tags"] = true, "standard", "all", []int64{tag.ID}
		s["addOptions"] = map[string]any{"monitor": "all", "searchForMissingEpisodes": false, "searchForCutoffUnmetEpisodes": false}
		var added struct {
			ID   int64  `json:"id"`
			Path string `json:"path"`
		}
		a.do(http.MethodPost, "series", s, &added)
		a.waitIdle()
		if i > 0 {
			continue
		}
		folder := filepath.Join(tv, path.Base(added.Path), "Season 1")
		name := path.Base(added.Path)
		writeMedia(t, filepath.Join(folder, name+" - S01E01 - The Clampetts Strike Oil WEBDL-1080p.mkv"), 200_001)
		writeMedia(t, filepath.Join(folder, name+" - S01E04-E05 - The Clampetts Meet Mrs. Drysdale HDTV-720p.mkv"), 200_002)
		a.do(http.MethodPost, "command", map[string]any{"name": "RescanSeries", "seriesId": added.ID}, nil)
		a.waitIdle()
		var files []json.RawMessage
		a.do(http.MethodGet, fmt.Sprintf("episodefile?seriesId=%d", added.ID), nil, &files)
		if len(files) != 2 {
			t.Fatalf("Sonarr imported %d files, want 2", len(files))
		}
	}
}

// liveManifestPlan decodes an *arr's state from its raw API JSON.
func liveManifestPlan(t *testing.T, a *manifestArr, integrationID int64) manifest.Plan {
	t.Helper()
	raw, err := manifesttest.LivePlan(a.kind, integrationID, manifesttest.HTTPGetter(a.url, a.prefix, manifestArrKey))
	if err != nil {
		t.Fatal(err)
	}
	var p manifest.Plan
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// requireManifestRoundTrip compares a manifest's re-import plan with the live *arrs.
func requireManifestRoundTrip(t *testing.T, what string, m *manifest.Manifest, live map[int64]manifest.Plan) {
	t.Helper()
	plan := m.ReimportPlan()
	total := 0
	for id, lp := range live {
		total += len(lp.Items)
		for _, d := range manifest.ComparePlan(plan.ForIntegration(id), lp) {
			t.Errorf("%s: integration %d: %s", what, id, d)
		}
	}
	if len(plan.Items) != total {
		t.Fatalf("%s lists %d items, the *arrs %d", what, len(plan.Items), total)
	}
}

func TestDockerArrManifestRoundTrip(t *testing.T) {
	if os.Getenv("BUNKARR_E2E_ARR") == "" {
		t.Skip("BUNKARR_E2E_ARR is not set (the real Radarr/Sonarr suite)")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatalf("BUNKARR_E2E_ARR is set but docker is not installed: %v", err)
	}
	radarrImage := cmpOr(os.Getenv("BUNKARR_E2E_RADARR_IMAGE"), "lscr.io/linuxserver/radarr:latest")
	sonarrImage := cmpOr(os.Getenv("BUNKARR_E2E_SONARR_IMAGE"), "lscr.io/linuxserver/sonarr:latest")

	root := resolvedTempDir(t)
	media := filepath.Join(root, "media")
	movies, tv := filepath.Join(media, "movies"), filepath.Join(media, "tv")
	movies4k := filepath.Join(root, "movies-4k") // in no source, and no mapping covers it
	target := filepath.Join(root, "target")
	for _, d := range []string{movies, tv, movies4k, target} {
		if err := os.MkdirAll(d, 0o777); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(d, 0o777) // the containers' user writes folders here
	}
	radarr := startManifestArr(t, "radarr", radarrImage, "/api/v3", 7878, map[string]string{movies: "/movies", movies4k: "/movies-4k"})
	sonarr := startManifestArr(t, "sonarr", sonarrImage, "/api/v3", 8989, map[string]string{tv: "/tv"})
	setupManifestRadarr(t, radarr, movies, movies4k)
	setupManifestSonarr(t, sonarr, tv)

	s := newServer(t, root)
	s.start()
	s.setup()
	src := s.createSource("Media", media)
	var scan apiJob
	s.call(http.StatusAccepted, "POST", fmt.Sprintf("/sources/%d/scan", src.ID), nil, &scan)
	if j := s.waitJob(scan.ID, 2*time.Minute); j.Status != "completed" {
		t.Fatalf("scan %+v", j)
	}
	ids := map[string]int64{}
	for _, a := range []*manifestArr{radarr, sonarr} {
		local := movies
		if a.kind == "sonarr" {
			local = tv
		}
		var it struct {
			ID int64 `json:"id"`
		}
		s.call(http.StatusCreated, "POST", "/integrations", map[string]any{"type": a.kind, "name": strings.ToUpper(a.kind[:1]) + a.kind[1:],
			"url": a.url, "apiKey": manifestArrKey, "settings": map[string]any{"pathMappings": []map[string]string{{"arr": "/" + path.Base(local), "local": local}}}}, &it)
		ids[a.kind] = it.ID
	}
	// Each create queued a full refresh.
	deadline := time.Now().Add(3 * time.Minute)
	for {
		var page apiPage[apiJob]
		s.call(http.StatusOK, "GET", "/jobs?type=refresh&pageSize=50", nil, &page)
		done := 0
		for _, j := range page.Records {
			if j.final() {
				if j.Status != "completed" && j.Status != "completed_with_warnings" {
					t.Fatalf("refresh %+v", j)
				}
				done++
			}
		}
		if done >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the refreshes did not finish: %+v", page.Records)
		}
		time.Sleep(200 * time.Millisecond)
	}
	dest := s.createDestination("UNAS", target, src.ID)
	if j := s.runSync(dest.ID, nil); j.Status != "completed" {
		t.Fatalf("sync %+v", j)
	}

	var exp apiJob
	s.call(http.StatusAccepted, "POST", fmt.Sprintf("/destinations/%d/manifest", dest.ID), nil, &exp)
	// completed_with_warnings: the /movies-4k movie lies in no source.
	if j := s.waitJob(exp.ID, 2*time.Minute); j.Status != "completed_with_warnings" {
		t.Fatalf("manifest export %+v", j)
	}
	var versions []struct {
		ID       int64  `json:"id"`
		Path     string `json:"path"`
		Checksum string `json:"checksum"`
	}
	s.call(http.StatusOK, "GET", fmt.Sprintf("/destinations/%d/manifests", dest.ID), nil, &versions)
	if len(versions) != 1 {
		t.Fatalf("versions %+v", versions)
	}
	live := map[int64]manifest.Plan{ids["radarr"]: liveManifestPlan(t, radarr, ids["radarr"]), ids["sonarr"]: liveManifestPlan(t, sonarr, ids["sonarr"])}

	// The version at the destination, read with ParseDir.
	m, err := manifest.ParseDir(filepath.Join(target, filepath.FromSlash(versions[0].Path)))
	if err != nil {
		t.Fatalf("ParseDir: %v", err)
	}
	requireManifestRoundTrip(t, "the destination's version", m, live)
	var unlocated, backedUp int
	for _, it := range m.Items {
		if !it.Located {
			unlocated++
		}
		for _, f := range it.Files {
			if f.BackedUp != nil && *f.BackedUp {
				backedUp++
			}
		}
	}
	if unlocated != 1 || backedUp != 4 {
		t.Fatalf("unlocated %d, backed-up *arr files %d", unlocated, backedUp)
	}

	// The download of the version and the on-the-spot export, each read with Parse.
	for _, p := range []string{fmt.Sprintf("/manifests/%d/download", versions[0].ID), "/manifest/export",
		fmt.Sprintf("/manifest/export?destinationId=%d", dest.ID)} {
		req, _ := http.NewRequest(http.MethodGet, s.base+p, nil)
		req.Header.Set("X-Api-Key", s.key)
		res, err := s.hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		sum := sha256.Sum256(body)
		if res.StatusCode != http.StatusOK || res.Header.Get("X-Bunkarr-SHA256") != hex.EncodeToString(sum[:]) {
			t.Fatalf("GET %s: HTTP %d, digest %q", p, res.StatusCode, res.Header.Get("X-Bunkarr-SHA256"))
		}
		if bytes.Contains(body, []byte(manifestArrKey)) || bytes.Contains(body, []byte(radarr.url)) || bytes.Contains(body, []byte(sonarr.url)) {
			t.Fatalf("GET %s: the manifest names a key or an *arr URL", p)
		}
		parsed, err := manifest.Parse(bytes.NewReader(body))
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		requireManifestRoundTrip(t, "GET "+p, parsed, live)
	}
}

// cmpOr returns the first non-empty string.
func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
