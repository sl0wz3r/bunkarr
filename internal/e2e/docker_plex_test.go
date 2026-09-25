//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Plex Media Server paths inside the pms-docker image.
const (
	plexData   = "/config/Library/Application Support/Plex Media Server"
	plexDBDir  = plexData + "/Plug-in Support/Databases"
	plexPrefs  = plexData + "/Preferences.xml"
	plexSQLite = "/usr/lib/plexmediaserver/Plex SQLite"
	libraryDB  = "com.plexapp.plugins.library.db"
	blobsDB    = "com.plexapp.plugins.library.blobs.db"
	// plexIDs is PMS's user and group (PLEX_UID/PLEX_GID) and Bunkarr's PUID/PGID: Bunkarr must
	// run as Plex's user to read Preferences.xml (0600).
	plexIDs = "1000"
)

// plexServer is a PMS container; its API is called with curl inside the container.
type plexServer struct {
	d    *dockerEnv
	name string
}

// get returns the JSON answer of a GET inside the container.
func (p plexServer) get(path string) ([]byte, error) {
	out, err := p.d.try("exec", p.name, "curl", "-sf", "-m", "30", "-H", "Accept: application/json", "http://127.0.0.1:32400"+path)
	return []byte(out), err
}

// mustGet is get that fails the test.
func (p plexServer) mustGet(path string) []byte {
	p.d.t.Helper()
	b, err := p.get(path)
	if err != nil {
		p.d.t.Fatalf("Plex %s GET %s: %v", p.name, path, err)
	}
	return b
}

// waitUp waits until PMS answers /identity.
func (p plexServer) waitUp(timeout time.Duration) {
	p.d.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := p.get("/identity"); err == nil {
			return
		}
		if !p.d.running(p.name) {
			p.d.t.Fatalf("Plex container %s stopped", p.name)
		}
		if time.Now().After(deadline) {
			p.d.t.Fatalf("Plex %s did not answer /identity within %s", p.name, timeout)
		}
		time.Sleep(time.Second)
	}
}

// plexSection is a library section as the test compares it.
type plexSection struct {
	Key       string
	Title     string
	Type      string
	Locations []string
}

// sections lists the library sections.
func (p plexServer) sections() []plexSection {
	p.d.t.Helper()
	var r struct {
		MediaContainer struct {
			Directory []struct {
				Key      string `json:"key"`
				Title    string `json:"title"`
				Type     string `json:"type"`
				Location []struct {
					Path string `json:"path"`
				} `json:"Location"`
			} `json:"Directory"`
		} `json:"MediaContainer"`
	}
	b := p.mustGet("/library/sections")
	if err := json.Unmarshal(b, &r); err != nil {
		p.d.t.Fatalf("Plex sections %s: %v", b, err)
	}
	var out []plexSection
	for _, dir := range r.MediaContainer.Directory {
		s := plexSection{Key: dir.Key, Title: dir.Title, Type: dir.Type}
		for _, l := range dir.Location {
			s.Locations = append(s.Locations, l.Path)
		}
		out = append(out, s)
	}
	return out
}

// movies returns the number of movies of a section and how many are watched.
func (p plexServer) movies(key string) (count, watched int) {
	p.d.t.Helper()
	var r struct {
		MediaContainer struct {
			Metadata []struct {
				ViewCount int `json:"viewCount"`
			} `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	b := p.mustGet("/library/sections/" + key + "/all?type=1")
	if err := json.Unmarshal(b, &r); err != nil {
		p.d.t.Fatalf("Plex movies: %v", err)
	}
	for _, m := range r.MediaContainer.Metadata {
		if m.ViewCount > 0 {
			watched++
		}
	}
	return len(r.MediaContainer.Metadata), watched
}

// ipOn returns a container's IPv4 address on a network.
func (d *dockerEnv) ipOn(c, network string) string {
	d.t.Helper()
	ip := d.docker("inspect", "--format", `{{with index .NetworkSettings.Networks "`+network+`"}}{{.IPAddress}}{{end}}`, c)
	if net.ParseIP(ip) == nil {
		d.t.Fatalf("container %s has no address on %s: %q", c, network, ip)
	}
	return ip
}

// plexNetmask formats a subnet the way PMS's allowedNetworks wants it: 172.18.0.0/255.255.0.0.
func plexNetmask(n *net.IPNet) string {
	return n.IP.String() + "/" + net.IP(n.Mask).String()
}

// addAllowedNetworks adds allowedNetworks to a stopped PMS's Preferences.xml (as Plex's user, so
// the file keeps its owner). Explicit subnets are required: 0.0.0.0/0.0.0.0 is not honored
// (spike 0001).
func (d *dockerEnv) addAllowedNetworks(plexImage, configVolume, allowed string) {
	d.t.Helper()
	d.oneShot("--user", plexIDs+":"+plexIDs, "-v", configVolume+":/config", "-e", "PREFS="+plexPrefs, "-e", "ALLOWED="+allowed,
		"--entrypoint", "bash", plexImage, "-c",
		`set -euo pipefail; grep -q '<Preferences ' "$PREFS"; sed -i "s|/>\$| allowedNetworks=\"$ALLOWED\"/>|" "$PREFS"; grep -q "allowedNetworks=\"$ALLOWED\"" "$PREFS"`)
}

// movieTitles are the generated library: plain titles and titles with Unicode and punctuation.
func movieTitles() []string {
	titles := []string{"Amélie (2001)", "Ødegaard Story (2020)", "Straße (2019)", "千と千尋の神隠し (2001)", "기생충 (2019)",
		"#Alive (2020)", "'71 (2014)", "Léon (1994)", "Æon Flux (2005)", "Das Boot (1981)"}
	for i := 1; i <= 50; i++ {
		titles = append(titles, fmt.Sprintf("E2E Movie %03d (%d)", i, 1900+i))
	}
	return titles
}

// loadScript is the write load of spike 0001, run in a container on the test network (so PMS
// sees it as a remote client admitted by allowedNetworks): a refresh of the section every 2 s,
// and scrobble of every movie in ascending id order then unscrobble in descending order, one call
// (one PMS transaction) per item, forever. A consistent snapshot therefore has at most one
// watched/unwatched transition in movie-id order.
const loadScript = `set -u
keys=$(curl -sf "$PLEX/library/sections/$SECTION/all?type=1" | grep -o 'ratingKey="[0-9]*"' | tr -dc '0-9\n' | sort -n)
if [ -z "$keys" ]; then echo "load: no movies (is allowedNetworks set?)"; exit 1; fi
( while :; do curl -s -o /dev/null "$PLEX/library/sections/$SECTION/refresh"; sleep 2; done ) &
pass=0; calls=0
while :; do
  pass=$((pass + 1))
  if [ $((pass % 2)) -eq 1 ]; then act=scrobble; order=$keys; else act=unscrobble; order=$(printf '%s\n' "$keys" | sort -rn); fi
  for k in $order; do
    code=$(curl -s -o /dev/null -w '%{http_code}' "$PLEX/:/$act?key=$k&identifier=com.plexapp.plugins.library")
    calls=$((calls + 1))
    if [ "$code" != 200 ]; then echo "load: HTTP $code for $act $k"; fi
  done
  echo "load: pass=$pass act=$act calls=$calls"
done`

// checkScript inspects one backup version with Plex's own SQLite (the ground truth of ADR 0005)
// on copies in /tmp (the backup volume is mounted read-only): sha256 of every file, PRAGMA
// integrity_check of both databases, and the movies' watched state in id order.
const checkScript = `set -euo pipefail
cd "/backup/$V"
sha256sum -- *
for f in com.plexapp.plugins.library.db com.plexapp.plugins.library.blobs.db; do
  cp -- "$f" "/tmp/$f"
  echo "integrity $f $("$SQLITE" "/tmp/$f" 'PRAGMA integrity_check;' | tr '\n' ' ')"
done
"$SQLITE" /tmp/com.plexapp.plugins.library.db "SELECT m.id, coalesce(max(s.view_count), 0) FROM metadata_items m LEFT JOIN metadata_item_settings s ON s.guid = m.guid WHERE m.metadata_type = 1 GROUP BY m.id ORDER BY m.id;" | sed 's/^/row /'`

// restoreScript is the restore procedure of ADR 0005 on a stopped PMS, run as Plex's user: the
// fresh databases are moved aside with their -wal/-shm removed (a stale WAL would be replayed onto
// the restored database), the backup's databases are copied in with mode 0644, and the server's
// own Preferences.xml is kept (restoring another server's would clone its identity).
const restoreScript = `set -euo pipefail
db="$DBDIR"; aside=/config/restore-aside
mkdir -p "$aside"
for f in com.plexapp.plugins.library.db com.plexapp.plugins.library.blobs.db; do
  if [ -e "$db/$f" ]; then mv -- "$db/$f" "$aside/"; fi
  rm -f -- "$db/$f-wal" "$db/$f-shm"
  cp -- "/backup/$V/$f" "$db/$f"
  chmod 0644 "$db/$f"
done
ls -ln "$db"`

// backupCheck is what checkScript found in one version.
type backupCheck struct {
	sha256    map[string]string
	integrity map[string]string
	movies    int
	watched   int
	// transitions counts watched/unwatched changes in movie-id order.
	transitions int
}

var (
	shaLine       = regexp.MustCompile(`^([0-9a-f]{64})  (.+)$`)
	integrityLine = regexp.MustCompile(`^integrity (\S+) (.*)$`)
	rowLine       = regexp.MustCompile(`^row (\d+)\|(\d+)$`)
)

// parseCheck parses checkScript's output.
func parseCheck(t *testing.T, out string) backupCheck {
	t.Helper()
	c := backupCheck{sha256: map[string]string{}, integrity: map[string]string{}}
	prev := -1
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case shaLine.MatchString(line):
			m := shaLine.FindStringSubmatch(line)
			c.sha256[m[2]] = m[1]
		case integrityLine.MatchString(line):
			m := integrityLine.FindStringSubmatch(line)
			c.integrity[m[1]] = strings.TrimSpace(m[2])
		case rowLine.MatchString(line):
			m := rowLine.FindStringSubmatch(line)
			vc, _ := strconv.Atoi(m[2])
			w := 0
			if vc > 0 {
				w = 1
				c.watched++
			}
			if prev >= 0 && w != prev {
				c.transitions++
			}
			prev = w
			c.movies++
		case strings.TrimSpace(line) != "":
			t.Fatalf("unexpected output of the backup check: %q", line)
		}
	}
	return c
}

// plexSnapshot is a Snapshot with the manifest fields the test checks.
type plexSnapshot struct {
	ID        int64  `json:"id"`
	Path      string `json:"path"`
	Integrity string `json:"integrity"`
	Method    string `json:"method"`
	Manifest  struct {
		Method      string `json:"method"`
		PlexVersion string `json:"plexVersion"`
		Result      string `json:"result"`
		Integrity   struct {
			QuickCheck     string `json:"quickCheck"`
			IntegrityCheck string `json:"integrityCheck"`
			MetadataItems  int64  `json:"metadataItems"`
		} `json:"integrity"`
		Files []struct {
			Name   string `json:"name"`
			Size   int64  `json:"size"`
			SHA256 string `json:"sha256"`
		} `json:"files"`
	} `json:"manifest"`
}

// plexStats are a plexdb_backup job's stats.
type plexStats struct {
	Files         int64  `json:"files"`
	Integrity     string `json:"integrity"`
	Method        string `json:"method"`
	Path          string `json:"path"`
	MetadataItems int64  `json:"metadataItems"`
}

// TestDockerPlexRestore is acceptance 4 (design §9 Docker suite, ADR 0005 test procedure): a
// scratch PMS (pinned image, allowedNetworks with the test network's explicit subnet) with a
// Movies library over generated fake files runs under a scrobble/refresh write load while the
// Bunkarr image backs its database up from a read-only mount of Plex's config. Every version must
// pass Bunkarr's verification and `Plex SQLite ... "PRAGMA integrity_check"` (ok), be a consistent
// snapshot of the load, and match its manifest; the newest one is restored into a second, fresh
// PMS, which must then serve the same sections, the same number of movies and the same watched
// count as the backup. Before the backups, the Plex integration's library side is exercised
// against the same PMS: Bunkarr lists its sections with the Movies location mapped into its own
// mount (path mapping /data → /media), a source is created from that location as the Library's
// "Import from Plex" does, and a sync of it to a second destination delivers every movie file.
func TestDockerPlexRestore(t *testing.T) {
	d := newDockerEnv(t)
	plexImage := os.Getenv("BUNKARR_E2E_PLEX_IMAGE")
	if plexImage == "" {
		t.Skip("BUNKARR_E2E_PLEX_IMAGE is not set (run docker/test-plex-restore.sh)")
	}
	if _, err := d.try("image", "inspect", "--format", "{{.Id}}", plexImage); err != nil {
		t.Logf("pulling %s", plexImage)
		d.docker("pull", "--quiet", plexImage)
	}

	netName, subnet := d.network("net")
	allowed := plexNetmask(subnet)
	media, cfgA, cfgB := d.volume("media"), d.volume("plex-a"), d.volume("plex-b")
	bkConfig, backup, mirror := d.volume("bunkarr-config"), d.volume("backup"), d.volume("mirror")

	// 1. Fake movie files (1 KiB of noise each), read-only for PMS and Bunkarr alike.
	local := resolvedTempDir(t)
	titles := movieTitles()
	for _, title := range titles {
		writeFile(t, filepath.Join(local, "movies"), title+"/"+title+".mkv", content(title, 1, 1024))
	}
	h := d.helper("fixture", d.image, "-v", media+":/data", "-v", backup+":/backup", "-v", mirror+":/mirror")
	d.cpIn(filepath.Join(local, "movies"), h, "/data/movies")
	d.docker("exec", h, "sh", "-c", "chown -R 0:0 /data && chmod -R a+rX /data && chown "+plexIDs+":"+plexIDs+" /backup /mirror")
	d.remove(h)

	// 2. Plex A: the first start writes Preferences.xml; allowedNetworks is added while stopped.
	plexRun := func(short, cfg string) plexServer {
		c := d.run(short, "--network", netName, "-e", "TZ=UTC", "-e", "PLEX_UID="+plexIDs, "-e", "PLEX_GID="+plexIDs,
			"-v", cfg+":/config", "-v", media+":/data:ro", plexImage)
		return plexServer{d: d, name: c}
	}
	a := plexRun("plex-a", cfgA)
	a.waitUp(3 * time.Minute)
	d.docker("stop", "-t", "60", a.name)
	d.addAllowedNetworks(plexImage, cfgA, allowed)
	d.docker("start", a.name)
	a.waitUp(3 * time.Minute)
	// PMS answers /identity before its library accepts sections (400 for a few seconds): retry
	// until one POST succeeds (a refused one creates nothing).
	deadline := time.Now().Add(2 * time.Minute)
	for len(a.sections()) == 0 {
		_, err := d.try("exec", a.name, "curl", "-sf", "-X", "POST",
			"http://127.0.0.1:32400/library/sections?name=Movies&type=movie&agent=tv.plex.agents.none&scanner=Plex%20Movie&language=xn&location=/data/movies")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Plex A did not create the Movies section: %v", err)
		}
		time.Sleep(time.Second)
	}
	var section plexSection
	deadline = time.Now().Add(5 * time.Minute)
	for {
		if secs := a.sections(); len(secs) == 1 {
			section = secs[0]
			if n, _ := a.movies(section.Key); n == len(titles) {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("Plex A did not list %d movies within 5 minutes: %+v", len(titles), a.sections())
		}
		time.Sleep(2 * time.Second)
	}
	// Clients address PMS by IP: an unclaimed PMS treats a request whose Host header is a name it
	// does not know (the container name) as non-local and answers 401 even from an allowed network.
	plexURL := "http://" + net.JoinHostPort(d.ipOn(a.name, netName), "32400")
	t.Logf("Plex A at %s: section %q (%s) with %d movies; allowedNetworks=%s", plexURL, section.Title, section.Key, len(titles), allowed)

	// 3. The write load, over the network.
	load := d.run("load", "--network", netName, "-e", "PLEX="+plexURL, "-e", "SECTION="+section.Key,
		"--entrypoint", "bash", plexImage, "-c", loadScript)
	deadline = time.Now().Add(2 * time.Minute)
	for {
		if _, w := a.movies(section.Key); w > 0 && strings.Contains(d.docker("logs", load), "pass=1 ") {
			break
		}
		if !d.running(load) {
			t.Fatalf("the load stopped: %s", d.docker("logs", load))
		}
		if time.Now().After(deadline) {
			t.Fatalf("the load made no progress within 2 minutes: %s", d.docker("logs", load))
		}
		time.Sleep(500 * time.Millisecond)
	}

	// 4. Bunkarr: Plex's config read-only at /plex, the media Plex sees at /data read-only at
	// /media, the backup shares at /backup and /mirror, on the network.
	bk := d.run("bunkarr", "--network", netName, "-e", "PUID="+plexIDs, "-e", "PGID="+plexIDs,
		"-v", bkConfig+":/config", "-v", cfgA+":/plex:ro", "-v", media+":/media:ro", "-v", backup+":/backup",
		"-v", mirror+":/mirror", d.image)
	api := newContainerAPI(d, bk)
	api.waitHealthy(90 * time.Second)
	api.setup()
	dest := api.createDestination("Backup", "/backup")
	var test struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
		Version string `json:"version"`
	}
	api.call(http.StatusOK, "POST", "/integrations/test", map[string]any{"type": "plex", "url": plexURL}, &test)
	if !test.OK {
		t.Fatalf("Bunkarr cannot reach Plex A at %s: %+v", plexURL, test)
	}
	var integ struct {
		ID int64 `json:"id"`
	}
	api.call(http.StatusCreated, "POST", "/integrations", map[string]any{"type": "plex", "name": "Plex A", "url": plexURL,
		"settings": map[string]any{"dataPath": "/plex/Library/Application Support/Plex Media Server",
			"pathMappings": []map[string]string{{"plex": "/data", "local": "/media"}}}}, &integ)
	requirePlexImportSync(t, d, api, integ.ID, section, filepath.Join(local, "movies"), mirror)

	const versions = 3
	for i := range versions {
		var j apiJob
		api.call(http.StatusAccepted, "POST", fmt.Sprintf("/integrations/%d/plex/backup", integ.ID), map[string]any{"destinationId": dest.ID}, &j)
		j = api.waitJob(j.ID, 10*time.Minute)
		requirePlexBackupJob(t, api.client, j)
		st := decodeStats[plexStats](t, j)
		t.Logf("backup %d: job %d %s, %d files, method %s, %d metadata items, %s", i+1, j.ID, j.Status, st.Files, st.Method, st.MetadataItems, st.Path)
		time.Sleep(1100 * time.Millisecond) // versions are named by the second
	}
	if !d.running(load) {
		t.Fatalf("the load stopped during the backups: %s", d.docker("logs", load))
	}
	if logs := d.docker("logs", load); strings.Contains(logs, "HTTP 401") {
		t.Fatalf("Plex refused the load: %s", logs)
	}

	// 5. Every version: Bunkarr's verification, Plex SQLite, the manifest, a consistent snapshot.
	var snaps []plexSnapshot
	api.call(http.StatusOK, "GET", fmt.Sprintf("/destinations/%d/snapshots", dest.ID), nil, &snaps)
	if len(snaps) != versions {
		t.Fatalf("snapshots: %d, want %d: %+v", len(snaps), versions, snaps)
	}
	var newest plexSnapshot
	var newestCheck backupCheck
	for _, s := range snaps {
		m := s.Manifest
		if s.Integrity != "ok" || m.Result != "ok" || m.Integrity.QuickCheck != "ok" || m.Integrity.IntegrityCheck != "ok" {
			t.Fatalf("snapshot %d did not pass Bunkarr's verification: %+v", s.ID, s)
		}
		if m.PlexVersion == "" {
			t.Fatalf("snapshot %d: no Plex version in the manifest (Bunkarr could not reach Plex A)", s.ID)
		}
		out := d.oneShot("-v", backup+":/backup:ro", "-e", "V="+s.Path, "-e", "SQLITE="+plexSQLite, "--entrypoint", "bash", plexImage, "-c", checkScript)
		c := parseCheck(t, out)
		for _, db := range []string{libraryDB, blobsDB} {
			if c.integrity[db] != "ok" {
				t.Fatalf("snapshot %d: Plex SQLite integrity_check of %s: %q", s.ID, db, c.integrity[db])
			}
		}
		names := map[string]bool{}
		for _, f := range m.Files {
			names[f.Name] = true
			if c.sha256[f.Name] != f.SHA256 {
				t.Fatalf("snapshot %d: %s has sha256 %s, the manifest says %s", s.ID, f.Name, c.sha256[f.Name], f.SHA256)
			}
		}
		for _, want := range []string{libraryDB, blobsDB, "Preferences.xml"} {
			if !names[want] {
				t.Fatalf("snapshot %d: %s is not in the manifest: %+v", s.ID, want, m.Files)
			}
		}
		if c.movies != len(titles) || c.transitions > 1 {
			t.Fatalf("snapshot %d is not a consistent copy: %d movies (want %d), %d watched/unwatched transitions (want at most 1)",
				s.ID, c.movies, len(titles), c.transitions)
		}
		t.Logf("snapshot %d (%s): Plex SQLite integrity ok; %d movies, %d watched, %d transition(s); Plex %s",
			s.ID, s.Path, c.movies, c.watched, c.transitions, m.PlexVersion)
		if s.ID > newest.ID {
			newest, newestCheck = s, c
		}
	}
	d.remove(load)

	// 6. Restore the newest version into a fresh Plex B (ADR 0005 restore procedure).
	b := plexRun("plex-b", cfgB)
	b.waitUp(3 * time.Minute)
	d.docker("stop", "-t", "60", b.name)
	d.oneShot("--user", plexIDs+":"+plexIDs, "-v", cfgB+":/config", "-v", backup+":/backup:ro", "-e", "V="+newest.Path,
		"-e", "DBDIR="+plexDBDir, "--entrypoint", "bash", plexImage, "-c", restoreScript)
	d.addAllowedNetworks(plexImage, cfgB, allowed)
	d.docker("start", b.name)
	b.waitUp(3 * time.Minute)

	secsA, secsB := a.sections(), b.sections()
	if fmt.Sprint(secsA) != fmt.Sprint(secsB) {
		t.Fatalf("restored sections differ:\nPlex A %+v\nPlex B %+v", secsA, secsB)
	}
	count, watched := b.movies(secsB[0].Key)
	if count != newestCheck.movies || watched != newestCheck.watched {
		t.Fatalf("restored library: %d movies, %d watched; the backup has %d movies, %d watched",
			count, watched, newestCheck.movies, newestCheck.watched)
	}
	integrity := d.docker("exec", "--user", plexIDs+":"+plexIDs, b.name, plexSQLite, "-readonly", plexDBDir+"/"+libraryDB, "PRAGMA integrity_check;")
	if integrity != "ok" {
		t.Fatalf("Plex SQLite integrity_check of the restored, running database: %q", integrity)
	}
	t.Logf("restored into Plex B: sections %+v, %d movies, %d watched, integrity ok", secsB, count, watched)
}

// apiPlexSection is a section of GET /integrations/{id}/plex/sections.
type apiPlexSection struct {
	Key       string `json:"key"`
	Title     string `json:"title"`
	Type      string `json:"type"`
	Locations []struct {
		Path      string `json:"path"`
		LocalPath string `json:"localPath"`
		Exists    bool   `json:"exists"`
	} `json:"locations"`
}

// requirePlexImportSync is the library side of the Plex integration (design §7, UI "Import from
// Plex") against the real PMS: Bunkarr lists Plex's sections with the Movies location
// (/data/movies for Plex) mapped to /media/movies, which exists in its container; a source
// created from that location keeps its Plex fields; a sync of it to a destination at /mirror
// (volume mirror) delivers exactly the files of localMovies, the fixture Plex serves.
func requirePlexImportSync(t *testing.T, d *dockerEnv, api *containerAPI, integID int64, section plexSection, localMovies, mirror string) {
	t.Helper()
	var secs []apiPlexSection
	api.call(http.StatusOK, "GET", fmt.Sprintf("/integrations/%d/plex/sections", integID), nil, &secs)
	if len(secs) != 1 || secs[0].Key != section.Key || secs[0].Title != "Movies" || secs[0].Type != "movie" || len(secs[0].Locations) != 1 {
		t.Fatalf("Plex sections through Bunkarr: %+v; want the one Movies section %+v", secs, section)
	}
	sec, loc := secs[0], secs[0].Locations[0]
	if loc.Path != "/data/movies" || loc.LocalPath != "/media/movies" || !loc.Exists {
		t.Fatalf("Movies location through Bunkarr: %+v; want /data/movies mapped to /media/movies, existing", loc)
	}

	var src apiSource
	api.call(http.StatusCreated, "POST", "/sources", map[string]any{"name": sec.Title, "path": loc.LocalPath,
		"plexIntegrationId": integID, "plexSectionId": sec.Key, "plexPath": loc.Path}, &src)
	src = api.source(src.ID)
	if src.Path != "/media/movies" || src.PlexIntegrationID == nil || *src.PlexIntegrationID != integID ||
		src.PlexSectionID != sec.Key || src.PlexPath != "/data/movies" {
		t.Fatalf("source imported from Plex: %+v", src)
	}

	want := hashTree(t, localMovies)
	var total int64
	for _, f := range want {
		total += f.Size
	}
	files := int64(len(want))
	dest := api.createDestination("Mirror", "/mirror", src.ID)
	j := api.runSync(dest.ID, nil)
	requireStatus(t, j, "completed")
	requireSyncStats(t, j, false, wantSync{planned: files, copied: files, updated: 0, moved: 0, linked: 0, retained: 0,
		held: 0, failed: 0, bytesPlanned: total, bytesCopied: total})
	if st := api.source(src.ID).Stats; st.Files != files || st.UniqueBytes != total {
		t.Fatalf("stats of the source imported from Plex: %+v, want %d files, %d bytes", st, files, total)
	}

	h := d.helper("mirror-readback", d.image, "-v", mirror+":/mirror:ro")
	out := d.cpOut(h, "/mirror", filepath.Join(filepath.Dir(localMovies), "mirror"))
	d.remove(h)
	verifyMirror(t, localMovies, out, src.DestFolder)
	t.Logf("Plex import: section %q location %s → %s, source %d (%s) synced to %s/%s: %d files verified",
		sec.Title, loc.Path, loc.LocalPath, src.ID, src.Name, dest.Target, src.DestFolder, files)
}

// requirePlexBackupJob requires a plexdb_backup job to have completed with an ok version. A
// warning is accepted only for a backup inside Plex's maintenance window (the test may run then).
func requirePlexBackupJob(t *testing.T, api *client, j apiJob) {
	t.Helper()
	switch j.Status {
	case "completed":
	case "completed_with_warnings":
		for _, l := range api.jobLogs(j.ID) {
			if l.Level != "warn" {
				continue
			}
			if !strings.Contains(l.Message, "maintenance window") {
				t.Fatalf("backup job %d warned: %s", j.ID, l.Message)
			}
			t.Logf("backup job %d (accepted warning): %s", j.ID, l.Message)
		}
	default:
		requireStatus(t, j, "completed")
	}
	st := decodeStats[plexStats](t, j)
	if st.Integrity != "ok" || st.Files < 3 || st.MetadataItems == 0 {
		t.Fatalf("backup job %d stats: %s", j.ID, j.Stats)
	}
}
