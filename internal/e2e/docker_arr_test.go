//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The *arr acceptance suite (docs/design/phase2-3.md §15 Docker suite, items 1 and 2, and
// acceptance 4 with the *arrs' own webhook tests): real Radarr and Sonarr containers of the
// fixture spike's versions import small real MKVs (made with ffmpeg) into a media volume that the
// Bunkarr image reads; each *arr's webhook connection is configured through its API with the
// integration's webhook key as the Basic password, and the test measures what Bunkarr does:
//   - acceptance 1: an import is at the destination within 60 s of the import command, copied by
//     a sync with trigger webhook and paths [the item folder] whose only item that is not a skip
//     is a copy of the imported file (Sonarr: into a series whose other episodes are backed up);
//   - acceptance 2: a 720p → 1080p upgrade copies the new file before it retains the old one, in
//     one targeted sync, also when the new file is copied from another volume; a same-path
//     replacement is an update whose old version is retained as replaced;
//   - acceptance 4: each *arr's Test event gets 200 with the webhook key and queues nothing; a
//     wrong key, Bunkarr's master API key and a session cookie get 401, and the webhook key
//     gets 401 on another route.
// The *arrs need internet for their metadata lookups; without it the test skips.

// The *arr images of the fixture spike (Sonarr 4.0.20.3014, Radarr 6.4.4.10685), pinned by index
// digest; BUNKARR_E2E_SONARR_IMAGE and BUNKARR_E2E_RADARR_IMAGE override them.
const (
	defaultSonarrImage = "lscr.io/linuxserver/sonarr@sha256:a5c1a5fecbef946927ab90ad68df319ac5fe644057e5fc18cd993f01ac07b2b2"
	defaultRadarrImage = "lscr.io/linuxserver/radarr@sha256:adb6c09d6b729ea5e642c99cea35af72702ef476bf4763f153299ac5db9f0b4f"
	// arrKey is the throw-away *arr containers' API key.
	arrKey = "0123456789abcdef0123456789abcdef"
	// arrIDs is the *arrs' PUID/PGID and Bunkarr's: the imported files are the *arrs', Bunkarr
	// reads them.
	arrIDs = "1000"
	// importDeadline is acceptance 1's bound: the copy is done this long after the import.
	importDeadline = 60 * time.Second
)

// arrAPI is an *arr's API, called with curl inside its container.
type arrAPI struct {
	d    *dockerEnv
	c    string
	app  string
	port string
}

// do performs one call (path under /api/v3) and returns the status and the body.
func (a arrAPI) do(method, p string, body any) (int, []byte, error) {
	args := []string{"exec", "-i", a.c, "curl", "-sS", "-m", "300", "-X", method, "-H", "X-Api-Key: " + arrKey,
		"-H", "Accept: application/json", "-w", "\n%{http_code}"}
	var stdin io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		args = append(args, "-H", "Content-Type: application/json", "--data-binary", "@-")
		stdin = bytes.NewReader(b)
	}
	args = append(args, "http://127.0.0.1:"+a.port+"/api/v3"+p)
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("docker", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return 0, nil, fmt.Errorf("%s %s %s: %w: %s", a.app, method, p, err, strings.TrimSpace(stderr.String()))
	}
	out := stdout.Bytes()
	i := bytes.LastIndexByte(out, '\n')
	if i < 0 {
		return 0, nil, fmt.Errorf("%s %s %s: no status in %q", a.app, method, p, out)
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(out[i+1:])))
	return code, out[:i], err
}

// call performs a call, requires one of the 2xx statuses and decodes the answer into out.
func (a arrAPI) call(method, p string, body, out any) {
	a.d.t.Helper()
	code, b, err := a.do(method, p, body)
	if err != nil {
		a.d.t.Fatal(err)
	}
	if code < 200 || code > 299 {
		a.d.t.Fatalf("%s %s %s: HTTP %d: %s", a.app, method, p, code, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			a.d.t.Fatalf("%s %s %s: decode %.300s: %v", a.app, method, p, b, err)
		}
	}
}

// waitUp waits until the *arr answers system/status.
func (a arrAPI) waitUp(timeout time.Duration) {
	a.d.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if code, _, err := a.do("GET", "/system/status", nil); err == nil && code == 200 {
			return
		}
		if !a.d.running(a.c) {
			a.d.t.Fatalf("%s stopped during start-up", a.app)
		}
		if time.Now().After(deadline) {
			a.d.t.Fatalf("%s did not start within %s", a.app, timeout)
		}
		time.Sleep(time.Second)
	}
}

// command runs an *arr command and waits for it; it returns when the *arr reported it completed.
func (a arrAPI) command(body map[string]any) time.Time {
	a.d.t.Helper()
	var c struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	}
	a.call("POST", "/command", body, &c)
	deadline := time.Now().Add(5 * time.Minute)
	for {
		var s struct {
			Status  string `json:"status"`
			Result  string `json:"result"`
			Message string `json:"message"`
		}
		a.call("GET", fmt.Sprintf("/command/%d", c.ID), nil, &s)
		switch s.Status {
		case "completed":
			return time.Now()
		case "failed", "aborted", "cancelled", "orphaned":
			a.d.t.Fatalf("%s command %v: %s %s", a.app, body["name"], s.Status, s.Message)
		}
		if time.Now().After(deadline) {
			a.d.t.Fatalf("%s command %v did not finish", a.app, body["name"])
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// allowUpgrades lets quality profile 6 ("HD - 720p/1080p") upgrade up to Bluray-1080p.
func (a arrAPI) allowUpgrades() {
	a.d.t.Helper()
	var profile map[string]any
	a.call("GET", "/qualityprofile/6", nil, &profile)
	for _, it := range profile["items"].([]any) {
		m := it.(map[string]any)
		if q, ok := m["quality"].(map[string]any); ok && q["name"] == "Bluray-1080p" {
			profile["cutoff"] = q["id"]
		}
	}
	profile["upgradeAllowed"] = true
	a.call("PUT", "/qualityprofile/6", profile, nil)
}

// rename turns renaming on with a file name format without the quality, so an upgrade replaces
// the file at the same path.
func (a arrAPI) rename(field, format string) {
	a.d.t.Helper()
	var naming map[string]any
	a.call("GET", "/config/naming", nil, &naming)
	naming[field] = format
	if a.app == "Radarr" {
		naming["renameMovies"] = true
	} else {
		naming["renameEpisodes"] = true
	}
	a.call("PUT", fmt.Sprintf("/config/naming/%v", naming["id"]), naming, nil)
}

// webhookBody is a Webhook connection for url with auth: the schema's Webhook entry, the
// triggers of design §16 on.
func (a arrAPI) webhookBody(url, username, password string) map[string]any {
	a.d.t.Helper()
	var schemas []map[string]any
	a.call("GET", "/notification/schema", nil, &schemas)
	for _, s := range schemas {
		if s["implementation"] != "Webhook" {
			continue
		}
		s["name"] = "Bunkarr"
		for _, f := range s["fields"].([]any) {
			m := f.(map[string]any)
			switch m["name"] {
			case "url":
				m["value"] = url
			case "method":
				m["value"] = 1
			case "username":
				m["value"] = username
			case "password":
				m["value"] = password
			}
		}
		for k := range s {
			if strings.HasPrefix(k, "on") && !slices.Contains([]string{"onGrab", "onHealthIssue", "onHealthRestored",
				"onApplicationUpdate", "onManualInteractionRequired", "onImportComplete"}, k) {
				s[k] = true
			}
		}
		return s
	}
	a.d.t.Fatalf("%s has no Webhook connection", a.app)
	return nil
}

// mediaScript makes the MKVs: black video of the release's resolution, silent audio, long enough
// that the *arrs do not take them for samples, each with its own title so no two are equal.
const mediaScript = `set -eu
mk() {
  mkdir -p "$(dirname "$1")"
  ffmpeg -nostdin -hide_banner -loglevel error -f lavfi -i "color=c=black:s=$2:r=1/10" -f lavfi -i anullsrc=r=48000:cl=mono \
    -t "$3" -c:v libx264 -preset ultrafast -crf 51 -c:a libopus -b:a 6k -shortest -metadata title="$(basename "$1")" "$1"
}
while read -r file size secs; do mk "$file" "$size" "$secs"; done <<EOF
$FILES
EOF
mkdir -p /data/movies /data/tv
chown -R ` + arrIDs + `:` + arrIDs + ` /data /downloads2 /backup`

// release is one generated download: its folder (in the downloads volume the *arr imports from)
// and its file.
type release struct {
	dir, file string
	size      string
	secs      int
}

func (r release) path() string { return r.dir + "/" + r.file }

func movie(root, name, quality, size string) release {
	n := strings.NewReplacer("(", "", ")", "").Replace(name)
	rel := n + "." + quality + ".BluRay.x264-BKR"
	return release{dir: root + "/" + rel, file: rel + ".mkv", size: size, secs: 5400}
}

func episode(root, ep, quality, source, size string) release {
	rel := "The.Beverly.Hillbillies." + ep + "." + quality + "." + source + ".x264-BKR"
	return release{dir: root + "/" + rel, file: rel + ".mkv", size: size, secs: 1500}
}

// bunkarrJob is a Job with the params this suite checks.
type bunkarrJob struct {
	apiJob
	Params struct {
		DestinationID int64    `json:"destinationId"`
		IntegrationID int64    `json:"integrationId"`
		SourceIDs     []int64  `json:"sourceIds"`
		Paths         []string `json:"paths"`
		ArrItemIDs    []int64  `json:"arrItemIds"`
		SyncAfter     bool     `json:"syncAfter"`
	} `json:"params"`
}

// itemDetail is the part of a sync item's detail the suite checks.
type itemDetail struct {
	RetainedReason string `json:"retainedReason"`
	Source         string `json:"source"`
}

// arrItem is a job item with its detail.
type arrItem struct {
	apiItem
	Detail itemDetail `json:"detail"`
}

// arrSuite is the running environment.
type arrSuite struct {
	t      *testing.T
	d      *dockerEnv
	bk     string
	api    *containerAPI
	destID int64
	// lastJob is the highest job id seen before the current step.
	lastJob int64
}

// jobs returns the jobs of a type with an id above after, oldest first.
func (s *arrSuite) jobs(typ string, after int64) []bunkarrJob {
	s.t.Helper()
	var page apiPage[bunkarrJob]
	s.api.call(http.StatusOK, "GET", "/jobs?pageSize=500&type="+typ, nil, &page)
	var out []bunkarrJob
	for _, j := range page.Records {
		if j.ID > after {
			out = append(out, j)
		}
	}
	slices.SortFunc(out, func(a, b bunkarrJob) int { return int(a.ID - b.ID) })
	return out
}

// maxJob returns the highest job id.
func (s *arrSuite) maxJob() int64 {
	s.t.Helper()
	var page apiPage[bunkarrJob]
	s.api.call(http.StatusOK, "GET", "/jobs?pageSize=1", nil, &page)
	if len(page.Records) == 0 {
		return 0
	}
	return page.Records[0].ID
}

// arrItems returns a job's items.
func (s *arrSuite) arrItems(jobID int64) []arrItem {
	s.t.Helper()
	var p apiPage[arrItem]
	s.api.call(http.StatusOK, "GET", fmt.Sprintf("/jobs/%d/items?pageSize=500", jobID), nil, &p)
	return p.Records
}

// waitRefreshes waits until every refresh job of integ is final.
func (s *arrSuite) waitRefreshes(integ int64, timeout time.Duration) {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		busy := false
		for _, j := range s.jobs("refresh", 0) {
			if j.Params.IntegrationID == integ && !j.final() {
				busy = true
			}
		}
		if !busy {
			return
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("the refreshes of integration %d did not finish", integ)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// webhookSync waits, until deadline, for the webhook sync (a job after s.lastJob with trigger
// webhook and paths [folder]) that has items, and for every sync job after s.lastJob to be final.
// It returns that job and its items.
func (s *arrSuite) webhookSync(folder string, deadline time.Time) (bunkarrJob, []arrItem) {
	s.t.Helper()
	for {
		var (
			found    *bunkarrJob
			items    []arrItem
			allFinal = true
		)
		for _, j := range s.jobs("sync", s.lastJob) {
			if !j.final() {
				allFinal = false
				continue
			}
			if j.Trigger != "webhook" || !slices.Equal(j.Params.Paths, []string{folder}) {
				continue
			}
			if its := s.arrItems(j.ID); len(its) > 0 {
				if found != nil {
					s.t.Fatalf("two webhook syncs of %s did work: %d and %d", folder, found.ID, j.ID)
				}
				j := j
				found, items = &j, its
			}
		}
		if found != nil && allFinal {
			return *found, items
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("no webhook sync of %q finished in time (jobs %+v)", folder, s.jobs("sync", s.lastJob))
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// sha256 returns the sha256 of a file in the Bunkarr container ("" when it does not exist).
func (s *arrSuite) sha256(p string) string {
	s.t.Helper()
	out, err := s.d.try("exec", s.bk, "sha256sum", p)
	if err != nil {
		return ""
	}
	return strings.Fields(out)[0]
}

// retainedHashes returns the sha256 of every file in the destination's retention.
func (s *arrSuite) retainedHashes() []string {
	s.t.Helper()
	out, _ := s.d.try("exec", s.bk, "sh", "-c", "find /backup/.bunkarr/retention -type f -exec sha256sum {} +")
	var hashes []string
	for _, line := range strings.Split(out, "\n") {
		if hash, _, ok := strings.Cut(line, "  "); ok {
			hashes = append(hashes, hash)
		}
	}
	return hashes
}

// size returns the size of a file in the Bunkarr container.
func (s *arrSuite) size(p string) int64 {
	s.t.Helper()
	out := s.d.docker("exec", s.bk, "stat", "-c", "%s", p)
	n, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		s.t.Fatalf("size of %s: %q", p, out)
	}
	return n
}

// mkvs returns the MKV files of an item folder of a source (paths relative to the source) with
// their sha256, as Bunkarr sees them.
func (s *arrSuite) mkvs(destFolder, folder string) map[string]string {
	s.t.Helper()
	root := path.Join("/media", destFolder)
	out, _ := s.d.try("exec", s.bk, "sh", "-c", `find "$1" -type f -name '*.mkv' -exec sha256sum {} +`, "sh", path.Join(root, folder))
	files := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if hash, p, ok := strings.Cut(line, "  "); ok {
			files[strings.TrimPrefix(p, root+"/")] = hash
		}
	}
	return files
}

// importStep is one import: the item folder's files before it, and when the *arr completed it.
type importStep struct {
	destFolder, folder string
	before             map[string]string
	done               time.Time
}

// changes returns the files of the step's folder that were added and removed, and those whose
// content changed at the same path.
func (s *arrSuite) changes(st importStep) (added, removed, changed []string, after map[string]string) {
	after = s.mkvs(st.destFolder, st.folder)
	for p, h := range after {
		switch old, ok := st.before[p]; {
		case !ok:
			added = append(added, p)
		case old != h:
			changed = append(changed, p)
		}
	}
	for p := range st.before {
		if _, ok := after[p]; !ok {
			removed = append(removed, p)
		}
	}
	return added, removed, changed, after
}

// requireImport checks acceptance 1 for an import: exactly one new file in the item folder, and
// the webhook sync with paths [folder] copied exactly that file within importDeadline, with the
// source's content.
func (s *arrSuite) requireImport(st importStep) {
	s.t.Helper()
	added, removed, changed, after := s.changes(st)
	if len(added) != 1 || len(removed) != 0 || len(changed) != 0 {
		s.t.Fatalf("the import into %s added %v, removed %v, changed %v", st.folder, added, removed, changed)
	}
	rel := added[0]
	j, items := s.webhookSync(st.folder, st.done.Add(importDeadline))
	if elapsed := time.Since(st.done); elapsed > importDeadline {
		s.t.Fatalf("the webhook sync of %s finished %s after the import (bound %s)", rel, elapsed.Round(time.Second), importDeadline)
	}
	if j.Status != "completed" {
		s.t.Fatalf("webhook sync %+v", j)
	}
	var work []arrItem
	for _, it := range items {
		if it.Action != "skip" {
			work = append(work, it)
		}
	}
	want := path.Join(st.destFolder, rel)
	if len(work) != 1 || work[0].Action != "copy" || work[0].Status != "done" || work[0].RelPath != want ||
		work[0].Bytes != s.size(path.Join("/media", want)) {
		s.t.Fatalf("import of %s: items %+v, want one done copy of %s", rel, work, want)
	}
	if h := s.sha256(path.Join("/backup", want)); h == "" || h != after[rel] {
		s.t.Fatalf("the destination's %s differs from the source (%s)", want, h)
	}
	s.t.Logf("import of %s copied by sync %d, %s after the import", rel, j.ID, time.Since(st.done).Round(time.Second))
}

// requireUpgrade checks acceptance 2 for an upgrade: either the old file was replaced by a new
// name (one webhook sync copied the new file before it retained the old one) or the same path
// holds new content (one update whose old version is retained as replaced); the old version is
// in retention.
func (s *arrSuite) requireUpgrade(st importStep) {
	s.t.Helper()
	added, removed, changed, after := s.changes(st)
	samePath := len(added) == 0 && len(removed) == 0 && len(changed) == 1
	if !samePath && (len(added) != 1 || len(removed) != 1 || len(changed) != 0) {
		s.t.Fatalf("the upgrade in %s added %v, removed %v, changed %v", st.folder, added, removed, changed)
	}
	j, items := s.webhookSync(st.folder, st.done.Add(importDeadline))
	if j.Status != "completed" {
		s.t.Fatalf("upgrade sync %+v", j)
	}
	byAction := map[string][]arrItem{}
	for _, it := range items {
		if it.Action != "skip" {
			if it.Status != "done" {
				s.t.Fatalf("upgrade item %+v", it)
			}
			byAction[it.Action] = append(byAction[it.Action], it)
		}
	}
	var oldHash, newRel string
	switch {
	case !samePath:
		newRel, oldHash = added[0], st.before[removed[0]]
		cp, rt := byAction["copy"], byAction["retain"]
		if len(byAction) != 2 || len(cp) != 1 || len(rt) != 1 || cp[0].RelPath != path.Join(st.destFolder, newRel) ||
			rt[0].RelPath != path.Join(st.destFolder, removed[0]) || cp[0].ID > rt[0].ID {
			s.t.Fatalf("upgrade %s → %s: items %+v, want a copy, then a retain", removed[0], newRel, items)
		}
	default:
		newRel, oldHash = changed[0], st.before[changed[0]]
		up := byAction["update"]
		if len(byAction) != 1 || len(up) != 1 || up[0].RelPath != path.Join(st.destFolder, newRel) || up[0].Detail.RetainedReason != "replaced" {
			s.t.Fatalf("same-path upgrade of %s: items %+v", newRel, items)
		}
	}
	if h := s.sha256(path.Join("/backup", st.destFolder, newRel)); h == "" || h != after[newRel] {
		s.t.Fatalf("the destination's %s differs from the source", newRel)
	}
	if !slices.Contains(s.retainedHashes(), oldHash) {
		s.t.Fatalf("the old version replaced by %s is not in retention", newRel)
	}
	s.t.Logf("upgrade to %s done by sync %d, %s after the import", newRel, j.ID, time.Since(st.done).Round(time.Second))
}

// tryBuild builds an image from a Dockerfile without a context, like dockerEnv.build, but returns
// the error (the ffmpeg image needs internet).
func (d *dockerEnv) tryBuild(short, dockerfile string) (string, error) {
	tag := d.name(short) + ":test"
	builder, err := d.try("context", "show")
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	cmd := exec.Command("docker", "build", "--builder", builder, "-q", "-t", tag, "-")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(dockerfile), &out, &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker build %s: %w: %s", tag, err, out.String())
	}
	d.images = append(d.images, tag)
	return tag, nil
}

func TestDockerArr(t *testing.T) {
	d := newDockerEnv(t)
	if os.Getenv("BUNKARR_E2E_ARR") == "" {
		t.Skip("BUNKARR_E2E_ARR is not set (run docker/test-arr.sh)")
	}
	images := map[string]string{"sonarr": defaultSonarrImage, "radarr": defaultRadarrImage}
	for app, env := range map[string]string{"sonarr": "BUNKARR_E2E_SONARR_IMAGE", "radarr": "BUNKARR_E2E_RADARR_IMAGE"} {
		if v := os.Getenv(env); v != "" {
			images[app] = v
		}
		if _, err := d.try("image", "inspect", "--format", "{{.Id}}", images[app]); err != nil {
			t.Logf("pulling %s", images[app])
			d.docker("pull", "--quiet", images[app])
		}
	}
	netName, _ := d.network("net")
	media, dl2, bkConfig, backup := d.volume("media"), d.volume("downloads2"), d.volume("bunkarr-config"), d.volume("backup")

	// 1. The releases, made with ffmpeg (alpine's package: needs internet like the *arrs).
	ffmpeg, err := d.tryBuild("ffmpeg", "FROM alpine:3.24\nRUN apk add --no-cache ffmpeg\n")
	if err != nil {
		t.Skipf("cannot build an ffmpeg image (no internet?): %v", err)
	}
	const dl, dlOther = "/data/downloads", "/downloads2"
	var (
		m1a, m1b = movie(dl, "Night.of.the.Living.Dead.1968", "720p", "1280x720"), movie(dl, "Night.of.the.Living.Dead.1968", "1080p", "1920x1080")
		m2a, m2b = movie(dl, "His.Girl.Friday.1940", "720p", "1280x720"), movie(dlOther, "His.Girl.Friday.1940", "1080p", "1920x1080")
		m3a, m3b = movie(dl, "Charade.1963", "720p", "1280x720"), movie(dl, "Charade.1963", "1080p", "1920x1080")
		e1a, e1b = episode(dl, "S01E01", "720p", "HDTV", "1280x720"), episode(dl, "S01E01", "1080p", "WEB-DL", "1920x1080")
		e2a, e2b = episode(dl, "S01E02", "720p", "HDTV", "1280x720"), episode(dlOther, "S01E02", "1080p", "WEB-DL", "1920x1080")
		e3a, e3b = episode(dl, "S01E03", "720p", "HDTV", "1280x720"), episode(dl, "S01E03", "1080p", "WEB-DL", "1920x1080")
	)
	var files strings.Builder
	for _, r := range []release{m1a, m1b, m2a, m2b, m3a, m3b, e1a, e1b, e2a, e2b, e3a, e3b} {
		fmt.Fprintf(&files, "%s %s %d\n", r.path(), r.size, r.secs)
	}
	d.oneShot("-v", media+":/data", "-v", dl2+":/downloads2", "-v", backup+":/backup", "-e", "FILES="+strings.TrimSpace(files.String()), "--entrypoint", "sh", ffmpeg,
		"-c", mediaScript)

	// 2. Radarr and Sonarr: the media volume at /data (downloads and library on one filesystem,
	// so imports move), a second downloads volume at /downloads2 (imports copy).
	arrRun := func(app, port string) arrAPI {
		env := strings.ToUpper(app) + "__AUTH__APIKEY=" + arrKey
		c := d.run(app, "--network", netName, "-e", "PUID="+arrIDs, "-e", "PGID="+arrIDs, "-e", "TZ=Etc/UTC", "-e", env,
			"-v", media+":/data", "-v", dl2+":/downloads2", images[app])
		return arrAPI{d: d, c: c, app: strings.ToUpper(app[:1]) + app[1:], port: port}
	}
	radarr, sonarr := arrRun("radarr", "7878"), arrRun("sonarr", "8989")
	radarr.waitUp(3 * time.Minute)
	sonarr.waitUp(3 * time.Minute)
	radarr.call("POST", "/rootfolder", map[string]string{"path": "/data/movies"}, nil)
	sonarr.call("POST", "/rootfolder", map[string]string{"path": "/data/tv"}, nil)
	radarr.allowUpgrades()
	sonarr.allowUpgrades()

	// 3. Bunkarr: the media read-only at /media, the destination volume at /backup.
	bk := d.run("bunkarr", "--network", netName, "-e", "PUID="+arrIDs, "-e", "PGID="+arrIDs,
		"-v", bkConfig+":/config", "-v", media+":/media:ro", "-v", backup+":/backup", d.image)
	api := newContainerAPI(d, bk)
	api.waitHealthy(90 * time.Second)
	api.setup()
	s := &arrSuite{t: t, d: d, bk: bk, api: api}
	movies, tv := api.createSource("Movies", "/media/movies"), api.createSource("TV", "/media/tv")
	s.destID = api.createDestination("Backup", "/backup", movies.ID, tv.ID).ID
	type integ struct {
		ID  int64
		key string
	}
	integrate := func(app, name, port string) integ {
		var it struct {
			ID int64 `json:"id"`
		}
		api.call(http.StatusCreated, "POST", "/integrations", map[string]any{"type": app, "name": name, "url": "http://" + d.name(app) + ":" + port,
			"apiKey": arrKey, "settings": map[string]any{"pathMappings": []map[string]string{{"arr": "/data", "local": "/media"}}}}, &it)
		var k struct {
			Key string `json:"key"`
		}
		api.call(http.StatusOK, "POST", fmt.Sprintf("/integrations/%d/webhook/key", it.ID), map[string]bool{"rotate": false}, &k)
		s.waitRefreshes(it.ID, 2*time.Minute)
		return integ{ID: it.ID, key: k.Key}
	}
	ri, si := integrate("radarr", "Radarr", "7878"), integrate("sonarr", "Sonarr", "8989")

	// 4. Acceptance 4: each *arr's Test event with the webhook key (Basic password) gets 200 and
	// queues nothing; a wrong key and the master API key make the *arr's test fail.
	hook := func(app string, id int64) string {
		return fmt.Sprintf("http://%s:8787/api/v1/webhook/%s/%d", d.name("bunkarr"), app, id)
	}
	before := s.maxJob()
	for _, tc := range []struct {
		a     arrAPI
		app   string
		it    integ
		label string
	}{{radarr, "radarr", ri, "Radarr"}, {sonarr, "sonarr", si, "Sonarr"}} {
		for name, body := range map[string]map[string]any{
			"wrong key":      tc.a.webhookBody(hook(tc.app, tc.it.ID), "bunkarr", strings.Repeat("f", 32)),
			"master API key": tc.a.webhookBody(hook(tc.app, tc.it.ID)+"?apikey="+api.key, "", ""),
			"no key":         tc.a.webhookBody(hook(tc.app, tc.it.ID), "", ""),
		} {
			if code, b, err := tc.a.do("POST", "/notification/test", body); err != nil || code < 400 {
				t.Fatalf("%s webhook test with %s: HTTP %d %s %v, want a failure", tc.label, name, code, b, err)
			}
		}
		// Created with the key: the *arr sends its Test event and requires a 2xx.
		tc.a.call("POST", "/notification", tc.a.webhookBody(hook(tc.app, tc.it.ID), "bunkarr", tc.it.key), nil)
		var events apiPage[struct {
			EventType string `json:"eventType"`
			Outcome   string `json:"outcome"`
		}]
		api.call(http.StatusOK, "GET", fmt.Sprintf("/webhooks/events?integrationId=%d", tc.it.ID), nil, &events)
		if events.TotalRecords == 0 || events.Records[0].EventType != "Test" || events.Records[0].Outcome != "test" {
			t.Fatalf("%s events after its test: %+v", tc.label, events)
		}
	}
	// From inside the network: a session cookie alone and the master key get 401; the webhook key
	// gets 401 on another route.
	bkURL := "http://" + d.name("bunkarr") + ":8787/api/v1"
	curl := func(args ...string) string {
		out, _ := d.try(append([]string{"exec", radarr.c, "curl", "-s", "-o", "/dev/null", "-w", "%{http_code}"}, args...)...)
		return out
	}
	login := fmt.Sprintf(`{"username":%q,"password":%q}`, e2eUser, e2ePassword)
	if code := curl("-c", "/tmp/bk-cookies", "-H", "Content-Type: application/json", "-d", login, bkURL+"/auth/login"); code != "200" {
		t.Fatalf("login from the Radarr container: %s", code)
	}
	for name, args := range map[string][]string{
		"session cookie":     {"-b", "/tmp/bk-cookies", "-H", "Content-Type: application/json", "-d", "{}", hook("radarr", ri.ID)},
		"master key":         {"-H", "X-Api-Key: " + api.key, "-H", "Content-Type: application/json", "-d", "{}", hook("radarr", ri.ID)},
		"webhook key on API": {"-H", "X-Api-Key: " + ri.key, bkURL + "/system/status"},
	} {
		if code := curl(args...); code != "401" {
			t.Errorf("%s: HTTP %s, want 401", name, code)
		}
	}
	if webhookRefreshes := func() int {
		n := 0
		for _, j := range s.jobs("refresh", before) {
			if j.Trigger == "webhook" {
				n++
			}
		}
		return n
	}(); webhookRefreshes != 0 {
		t.Fatalf("the Test events queued %d refreshes", webhookRefreshes)
	}

	// 5. The library: the *arrs add their items (their add events are refreshed and synced too).
	var m1, m2, m3 struct {
		ID int64 `json:"id"`
	}
	addMovie := func(tmdb int, out any) {
		var lookup map[string]any
		code, b, err := radarr.do("GET", fmt.Sprintf("/movie/lookup/tmdb?tmdbId=%d", tmdb), nil)
		if err != nil || code != 200 || json.Unmarshal(b, &lookup) != nil {
			t.Skipf("Radarr cannot look up tmdb %d (no internet?): HTTP %d %v", tmdb, code, err)
		}
		lookup["qualityProfileId"], lookup["rootFolderPath"], lookup["monitored"] = 6, "/data/movies", true
		lookup["minimumAvailability"] = "released"
		lookup["addOptions"] = map[string]any{"searchForMovie": false, "monitor": "movieOnly"}
		radarr.call("POST", "/movie", lookup, out)
	}
	addMovie(10331, &m1)
	addMovie(3085, &m2)
	addMovie(4808, &m3)
	var series []map[string]any
	if code, b, err := sonarr.do("GET", "/series/lookup?term=tvdb:71471", nil); err != nil || code != 200 || json.Unmarshal(b, &series) != nil || len(series) == 0 {
		t.Skipf("Sonarr cannot look up tvdb 71471 (no internet?): HTTP %d %v", code, err)
	}
	show := series[0]
	show["qualityProfileId"], show["languageProfileId"], show["rootFolderPath"], show["monitored"] = 6, 1, "/data/tv", true
	show["seasonFolder"], show["seriesType"] = true, "standard"
	show["addOptions"] = map[string]any{"monitor": "all", "searchForMissingEpisodes": false, "searchForCutoffUnmetEpisodes": false}
	sonarr.call("POST", "/series", show, nil)
	time.Sleep(30 * time.Second) // the add events' refreshes and syncs settle

	// run imports a release and returns the step (the folder's files before, the completion).
	run := func(a arrAPI, command, destFolder, folder string, r release, mode string) importStep {
		st := importStep{destFolder: destFolder, folder: folder, before: s.mkvs(destFolder, folder)}
		s.lastJob = s.maxJob()
		st.done = a.command(map[string]any{"name": command, "path": r.dir, "importMode": mode})
		return st
	}
	movieRun := func(folder string, r release, mode string) importStep {
		return run(radarr, "DownloadedMoviesScan", "movies", folder, r, mode)
	}
	tvRun := func(r release, mode string) importStep {
		return run(sonarr, "DownloadedEpisodesScan", "tv", "The Beverly Hillbillies", r, mode)
	}

	// 6. Radarr. Acceptance 1: an import is copied, alone, within 60 s. Acceptance 2: the 720p →
	// 1080p upgrade moved into place (copy, then retain); the new file copied from another volume
	// (the delete event comes first, the Download after the copy); the same path (renaming
	// without the quality in the name): an update.
	nold, hgf, charade := "Night of the Living Dead (1968)", "His Girl Friday (1940)", "Charade (1963)"
	s.requireImport(movieRun(nold, m1a, "Move"))
	s.requireUpgrade(movieRun(nold, m1b, "Move"))
	s.requireImport(movieRun(hgf, m2a, "Move"))
	s.requireUpgrade(movieRun(hgf, m2b, "Copy"))
	radarr.rename("standardMovieFormat", "{Movie Title} ({Release Year})")
	s.requireImport(movieRun(charade, m3a, "Move"))
	s.requireUpgrade(movieRun(charade, m3b, "Move"))

	// 7. Sonarr: acceptance 1 into a series whose other episodes are backed up (E02 after E01),
	// then the same upgrades.
	s.requireImport(tvRun(e1a, "Move"))
	s.requireImport(tvRun(e2a, "Move"))
	s.requireUpgrade(tvRun(e1b, "Move"))
	s.requireUpgrade(tvRun(e2b, "Copy"))
	sonarr.rename("standardEpisodeFormat", "{Series Title} - S{season:00}E{episode:00}")
	s.requireImport(tvRun(e3a, "Move"))
	s.requireUpgrade(tvRun(e3b, "Move"))

	// Every event was received and processed; none failed.
	var events apiPage[struct {
		EventType string `json:"eventType"`
		Outcome   string `json:"outcome"`
	}]
	api.call(http.StatusOK, "GET", "/webhooks/events?pageSize=500", nil, &events)
	downloads := 0
	for _, e := range events.Records {
		if e.Outcome == "failed" || e.Outcome == "" {
			t.Errorf("event %+v", e)
		}
		if e.EventType == "Download" {
			downloads++
		}
	}
	if downloads < 12 {
		t.Errorf("%d Download events, want at least 12", downloads)
	}
}
