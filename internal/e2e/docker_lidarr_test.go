//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/manifest"
	"github.com/sl0wz3r/bunkarr/internal/manifest/manifesttest"
)

// The Lidarr part of the *arr suite (docs/design/phase2-3.md D2: Lidarr gets the index, webhooks,
// backups and manifests like Sonarr and Radarr). A real Lidarr of the fixture spike's version
// imports small real audio files (made with ffmpeg, tagged so Lidarr identifies them) into a
// media volume the Bunkarr image reads, and posts its webhooks to Bunkarr with the integration's
// webhook key as the Basic password. It checks, with Lidarr's quirks (D2):
//   - acceptance 4: Lidarr's Test event with the webhook key gets 200 and queues nothing; a wrong
//     key, the master API key and no key make Lidarr's test fail;
//   - acceptance 1: a ManualImport with "replace existing files" (Lidarr's Download) is copied by
//     one sync with trigger webhook and paths [the artist folder] within 60 s;
//   - the replacement quirk: FLAC files imported over the MP3s (isUpgrade=false, no deletedFiles,
//     no delete event) are copied and the MP3s retained, in one targeted sync, copies first;
//   - the missing events: a ManualImport without "replace existing files" and a deleted track file
//     send no webhook; the full refresh reconciles them (D8) with a targeted sync, which copies the
//     import and retains nothing (D14); the next full sync retains the deleted file;
//   - acceptance 3: the destination's manifest version (ParseDir), its download and the on-the-spot
//     export (Parse) round-trip against Lidarr's raw API JSON (manifesttest);
//   - acceptance 5: an arr_backup with Lidarr's Backups folder mounted gives an ok arr snapshot;
//   - an ArtistDelete and the AlbumDelete Lidarr sends for each album give one refresh.
// The *arr metadata lookups and the ffmpeg image need internet; without it the test skips.
//
//	BUNKARR_E2E_IMAGE=bunkarr:dev BUNKARR_E2E_ARR=1 go test -tags e2e -count=1 -run TestDockerArrLidarr ./internal/e2e/

// defaultLidarrImage is linuxserver/lidarr 3.1.0.4875-ls42 (the fixture spike's version), pinned
// by index digest; BUNKARR_E2E_LIDARR_IMAGE overrides it.
const defaultLidarrImage = "lscr.io/linuxserver/lidarr@sha256:044d616beb43c5e7810991242c6a9c42b93ff238c8c0850684939634ea751208"

// The recorded artist and album of the fixture spike (Scott Joplin, Ragtime).
const (
	lidarrArtistMBID = "aec8a328-d2e8-4780-b2ea-318c7f8d6f75"
	lidarrArtistDir  = "Scott Joplin"
)

// lidarrAPI is Lidarr's API v1, called with curl inside its container.
type lidarrAPI struct {
	d *dockerEnv
	c string
}

// do performs one call (p under /api/v1) and returns the status and the body.
func (a lidarrAPI) do(method, p string, body any) (int, []byte, error) {
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
	args = append(args, "http://127.0.0.1:8686/api/v1"+p)
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("docker", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return 0, nil, fmt.Errorf("Lidarr %s %s: %w: %s", method, p, err, strings.TrimSpace(stderr.String()))
	}
	out := stdout.Bytes()
	i := bytes.LastIndexByte(out, '\n')
	if i < 0 {
		return 0, nil, fmt.Errorf("Lidarr %s %s: no status in %q", method, p, out)
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(out[i+1:])))
	if err != nil {
		return 0, nil, fmt.Errorf("Lidarr %s %s: status %q", method, p, out[i+1:])
	}
	return code, out[:i], nil
}

// call performs a call, requires a 2xx status and decodes the answer into out.
func (a lidarrAPI) call(method, p string, body, out any) {
	a.d.t.Helper()
	code, b, err := a.do(method, p, body)
	if err != nil {
		a.d.t.Fatal(err)
	}
	if code < 200 || code > 299 {
		a.d.t.Fatalf("Lidarr %s %s: HTTP %d: %.500s", method, p, code, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			a.d.t.Fatalf("Lidarr %s %s: decode %.300s: %v", method, p, b, err)
		}
	}
}

// waitUp waits until Lidarr answers system/status.
func (a lidarrAPI) waitUp(timeout time.Duration) {
	a.d.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if code, _, err := a.do("GET", "/system/status", nil); err == nil && code == 200 {
			return
		}
		if !a.d.running(a.c) {
			a.d.t.Fatal("Lidarr stopped during start-up")
		}
		if time.Now().After(deadline) {
			a.d.t.Fatalf("Lidarr did not start within %s", timeout)
		}
		time.Sleep(time.Second)
	}
}

// command runs a Lidarr command and returns when Lidarr reports it completed.
func (a lidarrAPI) command(body map[string]any) time.Time {
	a.d.t.Helper()
	var c struct {
		ID int64 `json:"id"`
	}
	a.call("POST", "/command", body, &c)
	deadline := time.Now().Add(5 * time.Minute)
	for {
		var s struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		}
		a.call("GET", fmt.Sprintf("/command/%d", c.ID), nil, &s)
		switch s.Status {
		case "completed":
			return time.Now()
		case "failed", "aborted", "cancelled", "orphaned":
			a.d.t.Fatalf("Lidarr command %v: %s %s", body["name"], s.Status, s.Message)
		}
		if time.Now().After(deadline) {
			a.d.t.Fatalf("Lidarr command %v did not finish", body["name"])
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// waitIdle waits until Lidarr runs no command.
func (a lidarrAPI) waitIdle() {
	a.d.t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for {
		var cmds []struct {
			Status string `json:"status"`
		}
		a.call("GET", "/command", nil, &cmds)
		busy := false
		for _, c := range cmds {
			busy = busy || c.Status == "queued" || c.Status == "started"
		}
		if !busy {
			return
		}
		if time.Now().After(deadline) {
			a.d.t.Fatal("Lidarr's commands did not finish")
		}
		time.Sleep(time.Second)
	}
}

// webhookBody is a Webhook connection for url with Basic auth, with the triggers of design §16
// on (the grab, health, update and failure notifications off).
func (a lidarrAPI) webhookBody(url, username, password string) map[string]any {
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
		for _, k := range []string{"onReleaseImport", "onUpgrade", "onRename", "onTrackRetag", "onArtistAdd", "onArtistDelete", "onAlbumDelete"} {
			s[k] = true
		}
		return s
	}
	a.d.t.Fatal("Lidarr has no Webhook connection")
	return nil
}

// manualImport imports the audio files of a downloads folder as Lidarr identifies them from
// their tags (every file must be identified), replacing existing files or not, and returns when
// the command completed. With replace, Lidarr sends a Download webhook; without, none (spike).
func (a lidarrAPI) manualImport(folder string, replace bool) time.Time {
	a.d.t.Helper()
	var found []struct {
		Path   string `json:"path"`
		Artist *struct {
			ID int64 `json:"id"`
		} `json:"artist"`
		Album *struct {
			ID int64 `json:"id"`
		} `json:"album"`
		AlbumReleaseID int64 `json:"albumReleaseId"`
		Tracks         []struct {
			ID int64 `json:"id"`
		} `json:"tracks"`
		Quality json.RawMessage `json:"quality"`
	}
	a.call("GET", "/manualimport?folder="+url.QueryEscape(folder)+"&filterExistingFiles=false&replaceExistingFiles="+strconv.FormatBool(replace), nil, &found)
	if len(found) == 0 {
		a.d.t.Fatalf("Lidarr found nothing to import in %s", folder)
	}
	var files []map[string]any
	for _, f := range found {
		if f.Artist == nil || f.Album == nil || f.AlbumReleaseID == 0 || len(f.Tracks) == 0 {
			a.d.t.Fatalf("Lidarr did not identify %s: %+v", f.Path, f)
		}
		var ids []int64
		for _, tr := range f.Tracks {
			ids = append(ids, tr.ID)
		}
		files = append(files, map[string]any{"path": f.Path, "artistId": f.Artist.ID, "albumId": f.Album.ID, "albumReleaseId": f.AlbumReleaseID,
			"trackIds": ids, "quality": f.Quality, "indexerFlags": 0, "downloadId": "", "disableReleaseSwitching": false})
	}
	return a.command(map[string]any{"name": "ManualImport", "files": files, "importMode": "copy", "replaceExistingFiles": replace})
}

// getter reads Lidarr's API for manifesttest.
func (a lidarrAPI) getter() manifesttest.Getter {
	return func(target string) ([]byte, error) {
		code, b, err := a.do("GET", "/"+target, nil)
		if err != nil {
			return nil, err
		}
		if code != http.StatusOK {
			return nil, fmt.Errorf("GET %s: HTTP %d", target, code)
		}
		return b, nil
	}
}

// audioScript makes the downloads: tagged mono sine tones, each with its own frequency so no two
// files are equal, then hands the media and destination volumes to the *arr and Bunkarr user. It
// also makes Lidarr's Backups folder, which a fresh Lidarr creates only with its first backup (a
// bind mount of the host's folder always exists).
const audioScript = `set -eu
mk() {
  mkdir -p "$(dirname "$1")"
  ffmpeg -nostdin -hide_banner -loglevel error -f lavfi -i "sine=frequency=$2:duration=30" -ac 1 -metadata artist="Scott Joplin" \
    -metadata album_artist="Scott Joplin" -metadata album="Ragtime" -metadata date=1994 -metadata title="$3" -metadata track="$4" "$1"
}
mk "/data/downloads/mp3/01 - Swipesy Cake Walk.mp3" 440 "Swipesy Cake Walk" 1
mk "/data/downloads/mp3/02 - Lily Queen.mp3" 494 "Lily Queen" 2
mk "/data/downloads/flac/01 - Swipesy Cake Walk.flac" 523 "Swipesy Cake Walk" 1
mk "/data/downloads/flac/02 - Lily Queen.flac" 587 "Lily Queen" 2
mk "/data/downloads/extra/03 - Sunflower Slow Drag.mp3" 659 "Sunflower Slow Drag" 3
mkdir -p /data/music /lidarr-config/Backups
chown -R ` + arrIDs + `:` + arrIDs + ` /data /backup /lidarr-config`

// tracks returns the audio files of the artist folder (paths relative to the music source) with
// their sha256, as Bunkarr sees them.
func (s *arrSuite) tracks() map[string]string {
	s.t.Helper()
	out, _ := s.d.try("exec", s.bk, "sh", "-c", `find "$1" -type f \( -name '*.mp3' -o -name '*.flac' \) -exec sha256sum {} +`, "sh",
		path.Join("/media/music", lidarrArtistDir))
	files := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if hash, p, ok := strings.Cut(line, "  "); ok {
			files[strings.TrimPrefix(p, "/media/music/")] = hash
		}
	}
	return files
}

// settle waits until every webhook event of integ is processed and every job is final, twice in
// a row (a refresh queues syncs when it ends).
func (s *arrSuite) settle(integ int64) {
	s.t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	quiet := 0
	for quiet < 2 {
		busy := false
		var events apiPage[struct {
			ProcessedAt *string `json:"processedAt"`
		}]
		s.api.call(http.StatusOK, "GET", fmt.Sprintf("/webhooks/events?integrationId=%d&pageSize=500", integ), nil, &events)
		for _, e := range events.Records {
			busy = busy || e.ProcessedAt == nil
		}
		for _, typ := range []string{"refresh", "sync", "manifest_export", "arr_backup"} {
			for _, j := range s.jobs(typ, 0) {
				busy = busy || !j.final()
			}
		}
		if busy {
			quiet = 0
		} else {
			quiet++
		}
		if time.Now().After(deadline) {
			s.t.Fatal("Bunkarr did not settle")
		}
		time.Sleep(3 * time.Second)
	}
}

// lidarrEvents returns the integration's webhook events, newest first.
func (s *arrSuite) lidarrEvents(integ int64) []lidarrEvent {
	s.t.Helper()
	var events apiPage[lidarrEvent]
	s.api.call(http.StatusOK, "GET", fmt.Sprintf("/webhooks/events?integrationId=%d&pageSize=500", integ), nil, &events)
	return events.Records
}

// lidarrEvent is a WebhookEvent.
type lidarrEvent struct {
	ID        int64  `json:"id"`
	EventType string `json:"eventType"`
	Class     string `json:"class"`
	Outcome   string `json:"outcome"`
	JobID     *int64 `json:"jobId"`
	Summary   struct {
		ItemIDs []int64  `json:"itemIds"`
		Files   []string `json:"files"`
	} `json:"summary"`
}

// workItems splits a sync's items that are not skips by action, requiring each to be done.
func (s *arrSuite) workItems(items []arrItem) map[string][]arrItem {
	s.t.Helper()
	out := map[string][]arrItem{}
	for _, it := range items {
		if it.Action == "skip" {
			continue
		}
		if it.Status != "done" {
			s.t.Fatalf("item %+v", it)
		}
		out[it.Action] = append(out[it.Action], it)
	}
	return out
}

// diffTracks returns the artist folder's audio files added and removed since before.
func diffTracks(before, after map[string]string) (added, removed []string) {
	for p := range after {
		if _, ok := before[p]; !ok {
			added = append(added, p)
		}
	}
	for p := range before {
		if _, ok := after[p]; !ok {
			removed = append(removed, p)
		}
	}
	slices.Sort(added)
	slices.Sort(removed)
	return added, removed
}

// relPaths returns the items' relative paths, sorted.
func relPaths(items []arrItem) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.RelPath)
	}
	slices.Sort(out)
	return out
}

// under prefixes each path with the music source's destination folder.
func under(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, path.Join("music", p))
	}
	return out
}

func TestDockerArrLidarr(t *testing.T) {
	d := newDockerEnv(t)
	if os.Getenv("BUNKARR_E2E_ARR") == "" {
		t.Skip("BUNKARR_E2E_ARR is not set (run docker/test-arr.sh)")
	}
	image := cmpOr(os.Getenv("BUNKARR_E2E_LIDARR_IMAGE"), defaultLidarrImage)
	if _, err := d.try("image", "inspect", "--format", "{{.Id}}", image); err != nil {
		t.Logf("pulling %s", image)
		d.docker("pull", "--quiet", image)
	}
	netName, _ := d.network("net")
	media, lidarrConfig, bkConfig, backup := d.volume("media"), d.volume("lidarr-config"), d.volume("bunkarr-config"), d.volume("backup")

	// 1. The downloads, made with ffmpeg (alpine's package: needs internet like Lidarr).
	ffmpeg, err := d.tryBuild("ffmpeg", "FROM alpine:3.24\nRUN apk add --no-cache ffmpeg\n")
	if err != nil {
		t.Skipf("cannot build an ffmpeg image (no internet?): %v", err)
	}
	d.oneShot("-v", media+":/data", "-v", backup+":/backup", "-v", lidarrConfig+":/lidarr-config", "--entrypoint", "sh", ffmpeg, "-c", audioScript)

	// 2. Lidarr with the media volume at /data, and Bunkarr reading it at /media plus Lidarr's
	// config (its Backups folder) read-only.
	lc := d.run("lidarr", "--network", netName, "-e", "PUID="+arrIDs, "-e", "PGID="+arrIDs, "-e", "TZ=Etc/UTC", "-e", "LIDARR__AUTH__APIKEY="+arrKey,
		"-v", media+":/data", "-v", lidarrConfig+":/config", image)
	lidarr := lidarrAPI{d: d, c: lc}
	lidarr.waitUp(3 * time.Minute)
	lidarr.call("POST", "/rootfolder", map[string]any{"name": "Music", "path": "/data/music", "defaultMetadataProfileId": 1,
		"defaultQualityProfileId": 1, "defaultMonitorOption": "none", "defaultNewItemMonitorOption": "none", "defaultTags": []int64{}}, nil)

	bk := d.run("bunkarr", "--network", netName, "-e", "PUID="+arrIDs, "-e", "PGID="+arrIDs,
		"-v", bkConfig+":/config", "-v", media+":/media:ro", "-v", lidarrConfig+":/arr/lidarr:ro", "-v", backup+":/backup", d.image)
	api := newContainerAPI(d, bk)
	api.waitHealthy(90 * time.Second)
	api.setup()
	s := &arrSuite{t: t, d: d, bk: bk, api: api}
	music := api.createSource("Music", "/media/music")
	dest := api.createDestination("Backup", "/backup", music.ID)
	s.destID = dest.ID
	var integ struct {
		ID int64 `json:"id"`
	}
	api.call(http.StatusCreated, "POST", "/integrations", map[string]any{"type": "lidarr", "name": "Lidarr", "url": "http://" + d.name("lidarr") + ":8686",
		"apiKey": arrKey, "settings": map[string]any{"pathMappings": []map[string]string{{"arr": "/data", "local": "/media"}},
			"backupFolder": "/arr/lidarr/Backups", "backup": map[string]any{"destinationId": dest.ID}}}, &integ)
	var key struct {
		Key string `json:"key"`
	}
	api.call(http.StatusOK, "POST", fmt.Sprintf("/integrations/%d/webhook/key", integ.ID), map[string]bool{"rotate": false}, &key)
	s.waitRefreshes(integ.ID, 2*time.Minute)

	// 3. Acceptance 4 with Lidarr's own webhook test.
	hook := fmt.Sprintf("http://%s:8787/api/v1/webhook/lidarr/%d", d.name("bunkarr"), integ.ID)
	before := s.maxJob()
	for name, body := range map[string]map[string]any{
		"wrong key":      lidarr.webhookBody(hook, "bunkarr", strings.Repeat("f", 32)),
		"master API key": lidarr.webhookBody(hook+"?apikey="+api.key, "", ""),
		"no key":         lidarr.webhookBody(hook, "", ""),
	} {
		if code, b, err := lidarr.do("POST", "/notification/test", body); err != nil || code < 400 {
			t.Fatalf("Lidarr's webhook test with %s: HTTP %d %s %v, want a failure", name, code, b, err)
		}
	}
	lidarr.call("POST", "/notification", lidarr.webhookBody(hook, "bunkarr", key.Key), nil)
	if ev := s.lidarrEvents(integ.ID); len(ev) == 0 || ev[0].EventType != "Test" || ev[0].Outcome != "test" {
		t.Fatalf("events after Lidarr's test: %+v", ev)
	}
	for _, j := range s.jobs("refresh", before) {
		if j.Trigger == "webhook" {
			t.Fatalf("the Test event queued a refresh: %+v", j)
		}
	}

	// 4. The artist (its ArtistAdd event is refreshed and synced too).
	var found []map[string]any
	if code, b, err := lidarr.do("GET", "/artist/lookup?term=lidarr:"+lidarrArtistMBID, nil); err != nil || code != 200 ||
		json.Unmarshal(b, &found) != nil || len(found) == 0 {
		t.Skipf("Lidarr cannot look up the artist (no internet?): HTTP %d %v", code, err)
	}
	artist := found[0]
	artist["qualityProfileId"], artist["metadataProfileId"], artist["rootFolderPath"], artist["monitored"] = 1, 1, "/data/music", true
	artist["monitorNewItems"], artist["tags"] = "all", []int64{}
	artist["addOptions"] = map[string]any{"monitor": "all", "searchForMissingAlbums": false}
	var added struct {
		ID int64 `json:"id"`
	}
	lidarr.call("POST", "/artist", artist, &added)
	lidarr.waitIdle()
	s.settle(integ.ID)

	// 5. Acceptance 1: a ManualImport replacing existing files (Lidarr's Download event) is copied
	// by one webhook sync of the artist folder within 60 s.
	s.lastJob = s.maxJob()
	old := s.tracks()
	done := lidarr.manualImport("/data/downloads/mp3", true)
	mp3s := s.tracks()
	addedFiles, removed := diffTracks(old, mp3s)
	if len(addedFiles) != 2 || len(removed) != 0 {
		t.Fatalf("the import added %v, removed %v", addedFiles, removed)
	}
	j, items := s.webhookSync(lidarrArtistDir, done.Add(importDeadline))
	if elapsed := time.Since(done); j.Status != "completed" || elapsed > importDeadline {
		t.Fatalf("webhook sync %+v, %s after the import", j, elapsed.Round(time.Second))
	}
	work := s.workItems(items)
	if len(work) != 1 || !slices.Equal(relPaths(work["copy"]), under(addedFiles)) {
		t.Fatalf("import: items %+v, want copies of %v", items, addedFiles)
	}
	for _, p := range addedFiles {
		if h := s.sha256(path.Join("/backup/music", p)); h == "" || h != mp3s[p] {
			t.Fatalf("the destination's %s differs from the source", p)
		}
	}
	t.Logf("import copied by sync %d, %s after the import", j.ID, time.Since(done).Round(time.Second))

	// 6. The replacement quirk: FLAC over MP3 (Download with isUpgrade=false and no deletedFiles,
	// no delete event first): one webhook sync copies the new files, then retains the old ones.
	s.lastJob = s.maxJob()
	done = lidarr.manualImport("/data/downloads/flac", true)
	flacs := s.tracks()
	addedFiles, removed = diffTracks(mp3s, flacs)
	if len(addedFiles) != 2 || len(removed) != 2 {
		t.Fatalf("the replacement added %v, removed %v", addedFiles, removed)
	}
	j, items = s.webhookSync(lidarrArtistDir, done.Add(importDeadline))
	work = s.workItems(items)
	if j.Status != "completed" || len(work) != 2 || !slices.Equal(relPaths(work["copy"]), under(addedFiles)) ||
		!slices.Equal(relPaths(work["retain"]), under(removed)) {
		t.Fatalf("replacement: sync %+v, items %+v", j, items)
	}
	for _, cp := range work["copy"] {
		for _, rt := range work["retain"] {
			if cp.ID > rt.ID {
				t.Fatalf("a retain (%d) came before a copy (%d)", rt.ID, cp.ID)
			}
		}
	}
	retained := s.retainedHashes()
	for _, p := range removed {
		if !slices.Contains(retained, mp3s[p]) {
			t.Fatalf("the replaced %s is not in retention", p)
		}
	}
	if ev := s.lidarrEvents(integ.ID); ev[0].EventType != "Download" || ev[0].Class != "download" || len(ev[0].Summary.Files) != 2 {
		t.Fatalf("the replacement's event: %+v", ev[0])
	}

	// 7. No webhook for a ManualImport without "replace existing files", nor for a deleted track
	// file: the full refresh reconciles each with a targeted sync of the artist folder (D8), which
	// copies the import and does not retain the deleted file (D14); the next full sync does.
	current := flacs
	reconcile := func(what string, change func()) (added, removed []string, work map[string][]arrItem) {
		t.Helper()
		eventsBefore := len(s.lidarrEvents(integ.ID))
		s.lastJob = s.maxJob()
		change()
		time.Sleep(15 * time.Second) // time for a webhook that must not come
		if n := len(s.lidarrEvents(integ.ID)); n != eventsBefore || len(s.jobs("sync", s.lastJob)) != 0 {
			t.Fatalf("%s: Lidarr sent %d events, %d syncs were queued", what, n-eventsBefore, len(s.jobs("sync", s.lastJob)))
		}
		after := s.tracks()
		added, removed = diffTracks(current, after)
		current = after
		var refresh apiJob
		api.call(http.StatusAccepted, "POST", fmt.Sprintf("/integrations/%d/refresh", integ.ID), map[string]any{}, &refresh)
		if r := api.waitJob(refresh.ID, 2*time.Minute); r.Status != "completed" {
			t.Fatalf("%s: refresh %+v", what, r)
		}
		var sync *bunkarrJob
		deadline := time.Now().Add(2 * time.Minute)
		for sync == nil || !sync.final() {
			for _, sj := range s.jobs("sync", s.lastJob) {
				if sj.Trigger == "manual" && slices.Equal(sj.Params.Paths, []string{lidarrArtistDir}) {
					sync = &sj
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: the full refresh queued no targeted sync of %s: %+v", what, lidarrArtistDir, s.jobs("sync", s.lastJob))
			}
			time.Sleep(time.Second)
		}
		if sync.Status != "completed" {
			t.Fatalf("%s: reconcile sync %+v", what, sync)
		}
		return added, removed, s.workItems(s.arrItems(sync.ID))
	}
	addedFiles, removed, work = reconcile("manual import", func() { lidarr.manualImport("/data/downloads/extra", false) })
	if len(addedFiles) != 1 || len(removed) != 0 || len(work) != 1 || !slices.Equal(relPaths(work["copy"]), under(addedFiles)) {
		t.Fatalf("manual import: added %v, removed %v, items %+v", addedFiles, removed, work)
	}
	var tfs []struct {
		ID   int64  `json:"id"`
		Path string `json:"path"`
	}
	lidarr.call("GET", fmt.Sprintf("/trackfile?artistId=%d", added.ID), nil, &tfs)
	addedFiles, removed, work = reconcile("track file delete", func() {
		for _, tf := range tfs {
			if strings.HasPrefix(path.Base(tf.Path), "02 - ") {
				lidarr.call("DELETE", fmt.Sprintf("/trackfile/%d", tf.ID), nil, nil)
			}
		}
	})
	if len(addedFiles) != 0 || len(removed) != 1 || len(work) != 0 {
		t.Fatalf("track file delete: added %v, removed %v, items %+v", addedFiles, removed, work)
	}
	full := api.runSync(dest.ID, nil)
	fullItems := s.workItems(s.arrItems(full.ID))
	rt := fullItems["retain"]
	if full.Status != "completed" || len(fullItems) != 1 || len(rt) != 1 || rt[0].RelPath != path.Join("music", removed[0]) || rt[0].Detail.RetainedReason != "deleted" {
		t.Fatalf("full sync %+v, items %+v", full, fullItems)
	}
	s.settle(integ.ID)

	// 8. Acceptance 3: the manifest round trip against Lidarr's raw API JSON.
	raw, err := manifesttest.LivePlan("lidarr", integ.ID, lidarr.getter())
	if err != nil {
		t.Fatal(err)
	}
	var live manifest.Plan
	if err := json.Unmarshal(raw, &live); err != nil {
		t.Fatal(err)
	}
	var exp apiJob
	api.call(http.StatusAccepted, "POST", fmt.Sprintf("/destinations/%d/manifest", dest.ID), nil, &exp)
	if j := api.waitJob(exp.ID, 2*time.Minute); j.Status != "completed" {
		t.Fatalf("manifest export %+v", j)
	}
	var versions []struct {
		ID   int64  `json:"id"`
		Path string `json:"path"`
	}
	api.call(http.StatusOK, "GET", fmt.Sprintf("/destinations/%d/manifests", dest.ID), nil, &versions)
	if len(versions) == 0 {
		t.Fatal("no manifest version")
	}
	dir := d.cpOut(bk, path.Join("/backup", versions[0].Path), filepath.Join(resolvedTempDir(t), "version"))
	m, err := manifest.ParseDir(dir)
	if err != nil {
		t.Fatalf("ParseDir: %v", err)
	}
	requireManifestRoundTrip(t, "the destination's version", m, map[int64]manifest.Plan{integ.ID: live})
	var files, backedUp int
	for _, it := range m.Items {
		if it.Kind != "artist" || it.ExternalIDs.MBID != lidarrArtistMBID || !it.Located || it.MetadataProfile == "" || len(it.Detail.Albums) == 0 {
			t.Fatalf("manifest item %+v", it)
		}
		for _, f := range it.Files {
			files++
			if f.AlbumID == 0 || f.Source == nil {
				t.Fatalf("manifest file %+v", f)
			}
			if f.BackedUp != nil && *f.BackedUp {
				backedUp++
			}
		}
	}
	if files != 2 || backedUp != 2 {
		t.Fatalf("the manifest lists %d track files, %d backed up", files, backedUp)
	}
	for _, p := range []string{fmt.Sprintf("/manifests/%d/download", versions[0].ID), "/manifest/export"} {
		code, body, _, err := api.wget("GET", p, nil)
		if err != nil || code != http.StatusOK {
			t.Fatalf("GET %s: HTTP %d, %v", p, code, err)
		}
		if bytes.Contains(body, []byte(arrKey)) || bytes.Contains(body, []byte(key.Key)) {
			t.Fatalf("GET %s: the manifest names a key", p)
		}
		parsed, err := manifest.Parse(bytes.NewReader(body))
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		requireManifestRoundTrip(t, "GET "+p, parsed, map[int64]manifest.Plan{integ.ID: live})
	}

	// 9. Acceptance 5: Lidarr's backup, copied from its Backups folder.
	var bj apiJob
	api.call(http.StatusAccepted, "POST", fmt.Sprintf("/integrations/%d/arr/backup", integ.ID), map[string]any{}, &bj)
	if j := api.waitJob(bj.ID, 5*time.Minute); j.Status != "completed" {
		t.Fatalf("arr_backup %+v", j)
	}
	var snaps []struct {
		Kind          string `json:"kind"`
		IntegrationID *int64 `json:"integrationId"`
		Integrity     string `json:"integrity"`
		Method        string `json:"method"`
	}
	api.call(http.StatusOK, "GET", fmt.Sprintf("/destinations/%d/snapshots", dest.ID), nil, &snaps)
	ok := false
	for _, sn := range snaps {
		ok = ok || sn.Kind == "arr" && sn.IntegrationID != nil && *sn.IntegrationID == integ.ID && sn.Integrity == "ok"
	}
	if !ok {
		t.Fatalf("snapshots %+v", snaps)
	}

	// 10. An ArtistDelete (files kept) and the AlbumDelete Lidarr sends for each album: one
	// refresh, whose targeted sync retains nothing (the files are still there).
	eventsBefore := len(s.lidarrEvents(integ.ID))
	s.lastJob = s.maxJob()
	lidarr.call("DELETE", fmt.Sprintf("/artist/%d?deleteFiles=false", added.ID), nil, nil)
	lidarr.waitIdle()
	s.settle(integ.ID)
	events := s.lidarrEvents(integ.ID)
	events = events[:len(events)-eventsBefore]
	jobIDs := map[int64]bool{}
	types := map[string]int{}
	for _, e := range events {
		types[e.EventType]++
		if e.JobID == nil || !slices.Equal(e.Summary.ItemIDs, []int64{added.ID}) {
			t.Fatalf("delete event %+v", e)
		}
		jobIDs[*e.JobID] = true
	}
	if types["ArtistDelete"] != 1 || types["AlbumDelete"] == 0 || len(jobIDs) != 1 {
		t.Fatalf("delete events %v in jobs %v", types, jobIDs)
	}
	for _, sj := range s.jobs("sync", s.lastJob) {
		for _, it := range s.arrItems(sj.ID) {
			if it.Action == "retain" {
				t.Fatalf("the artist delete retained %s", it.RelPath)
			}
		}
	}
	t.Logf("ArtistDelete and %d AlbumDelete events: one refresh", types["AlbumDelete"])
}
