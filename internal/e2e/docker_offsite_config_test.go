//go:build e2e

package e2e

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/integrations/arr"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr/arrtest"
)

// The fake *arr of the config-version test (arrtest, Phase 2's fake Radarr, served inside the
// Docker network by this package's own test binary).
const (
	fakeArrEnv  = "BUNKARR_E2E_OFFSITE_FAKE_ARR"
	fakeArrKey  = "e2e0ffs1te0123456789abcdef012345"
	fakeArrPort = "7878"
	fakeArrHost = "radarr"
)

// TestOffsiteFakeArr is not an acceptance test. TestDockerOffsiteConfigVersions runs this
// package's test binary in a container with BUNKARR_E2E_OFFSITE_FAKE_ARR=radarr: this function then
// serves arrtest's fake Radarr on 0.0.0.0:7878 until the container is removed. Every second it lists
// a new manual backup with the current time (arrtest serves a valid zip for it), so every backup job
// gets a backup made after it was queued, as a real *arr's backup command would.
func TestOffsiteFakeArr(t *testing.T) {
	kind := os.Getenv(fakeArrEnv)
	if kind == "" {
		t.Skip(fakeArrEnv + " is not set (TestDockerOffsiteConfigVersions sets it inside its fake *arr container)")
	}
	srv := arrtest.NewServer(t, arr.Kind(kind), fakeArrKey)
	target := strings.TrimPrefix(srv.URL, "http://")
	ln, err := net.Listen("tcp", ":"+fakeArrPort)
	if err != nil {
		t.Fatal(err)
	}
	zipBody := fakeArrZip(t, kind)
	go func() {
		var entries []map[string]any
		for i := 1; ; i++ {
			now := time.Now().UTC().Truncate(time.Second)
			name := fmt.Sprintf("%s_backup_v6.4.4.10685_%s.zip", kind, now.Format("2006.01.02_15.04.05"))
			entries = append(entries, map[string]any{"name": name, "path": "/backup/manual/" + name, "type": "manual",
				"size": len(zipBody), "time": now.Format(time.RFC3339), "id": i})
			if len(entries) > 5 {
				entries = entries[1:]
			}
			srv.SetBackupZip("manual", name, zipBody)
			srv.SetBackups(entries)
			time.Sleep(time.Second)
		}
	}()
	t.Logf("fake %s on :%s (relayed to %s)", kind, fakeArrPort, target)
	for {
		c, err := ln.Accept()
		if err != nil {
			t.Fatal(err)
		}
		go relay(c, target)
	}
}

// fakeArrZip is a backup zip the *arr backup verification accepts: config.xml with the fake's key
// and a small SQLite database (arrtest's own zip has an empty one, which the check refuses).
func fakeArrZip(t *testing.T, kind string) []byte {
	t.Helper()
	dbFile := filepath.Join(t.TempDir(), kind+".db")
	d, err := sql.Open("sqlite", "file:"+dbFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`CREATE TABLE Movies (Id INTEGER PRIMARY KEY, Title TEXT)`, `INSERT INTO Movies VALUES (1, 'Nosferatu'), (2, 'Metropolis')`} {
		if _, err := d.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	dbBytes, err := os.ReadFile(dbFile)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for _, f := range []struct {
		name string
		body []byte
	}{
		{"config.xml", []byte("<Config>\n  <ApiKey>" + fakeArrKey + "</ApiKey>\n  <AuthenticationMethod>Forms</AuthenticationMethod>\n</Config>\n")},
		{kind + ".db", dbBytes},
	} {
		w, err := zw.Create(f.name)
		if err == nil {
			_, err = w.Write(f.body)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// relay copies a connection to target and back.
func relay(c net.Conn, target string) {
	defer c.Close()
	u, err := net.Dial("tcp", target)
	if err != nil {
		return
	}
	defer u.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(u, c); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, u); done <- struct{}{} }()
	<-done
}

// startFakeArr builds this package's test binary for the image's platform and runs it as the fake
// Radarr (network alias radarr), with the arr fixtures at the path arrtest reads them from (the
// module's absolute path, compiled into the binary).
func (o *offsite) startFakeArr() {
	o.t.Helper()
	bin := o.crossBuild(o.t.TempDir(), "./internal/e2e", "e2e", true)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	addFile := func(name string, mode int64, data []byte) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(data)), Typeflag: tar.TypeReg, ModTime: time.Now()}); err != nil {
			o.t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			o.t.Fatal(err)
		}
	}
	data, err := os.ReadFile(bin)
	if err != nil {
		o.t.Fatal(err)
	}
	addFile("fake/e2e.test", 0o755, data)
	fixtures := filepath.Join(o.root, "testdata", "arr")
	err = filepath.WalkDir(fixtures, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		addFile(strings.TrimPrefix(filepath.ToSlash(p), "/"), 0o644, b)
		return nil
	})
	if err != nil {
		o.t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		o.t.Fatal(err)
	}
	c := o.d.name("radarr")
	o.d.containers = append(o.d.containers, c)
	o.d.docker("create", "--name", c, "--network", o.net, "--network-alias", fakeArrHost, "-e", fakeArrEnv+"=radarr",
		"--entrypoint", "/fake/e2e.test", o.image, "-test.run", "^TestOffsiteFakeArr$", "-test.timeout", "0", "-test.v")
	cp := exec.Command("docker", "cp", "-", c+":/")
	cp.Stdin = &buf
	if out, err := cp.CombinedOutput(); err != nil {
		o.t.Fatalf("docker cp into %s: %v\n%s", c, err, out)
	}
	o.d.docker("start", c)
	deadline := time.Now().Add(time.Minute)
	for !o.d.execOK(o.tools, "curl", "-sf", "-o", "/dev/null", "-H", "X-Api-Key: "+fakeArrKey, "http://"+fakeArrHost+":"+fakeArrPort+"/api/v3/system/status") {
		if time.Now().After(deadline) {
			out, _ := exec.Command("docker", "logs", "--tail", "30", c).CombinedOutput()
			o.t.Fatalf("the fake Radarr did not answer within a minute:\n%s", out)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// writePlexData writes a stopped Plex server's data directory into the media volume: a library
// database (rollback journal, no -wal: what a cleanly stopped Plex leaves) and Preferences.xml with
// a PlexOnlineToken.
func (o *offsite) writePlexData(dir, token string) {
	o.t.Helper()
	local := o.t.TempDir()
	dbFile := filepath.Join(local, "com.plexapp.plugins.library.db")
	d, err := sql.Open("sqlite", "file:"+dbFile)
	if err != nil {
		o.t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE metadata_items (id INTEGER PRIMARY KEY, title TEXT, title_sort TEXT)`,
		`CREATE TABLE media_parts (id INTEGER PRIMARY KEY, media_item_id INTEGER, file TEXT)`,
		`INSERT INTO metadata_items VALUES (1, 'Nosferatu', 'Nosferatu'), (2, 'Metropolis', 'Metropolis')`,
		`INSERT INTO media_parts VALUES (1, 1, '/movies/Nosferatu (1922)/Nosferatu (1922).mkv'), (2, 2, '/movies/Metropolis (1927)/Metropolis (1927).mkv')`,
	} {
		if _, err := d.Exec(s); err != nil {
			o.t.Fatalf("%s: %v", s, err)
		}
	}
	if err := d.Close(); err != nil {
		o.t.Fatal(err)
	}
	prefs := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<Preferences MachineIdentifier="e2e-offsite" ProcessedMachineIdentifier="e2e-offsite-p" PlexOnlineToken="%s" FriendlyName="e2e"/>
`, token)
	if err := os.WriteFile(filepath.Join(local, "Preferences.xml"), []byte(prefs), 0o600); err != nil {
		o.t.Fatal(err)
	}
	dst := offsiteMedia + "/" + dir
	o.toolsSh(`mkdir -p "$1/Plug-in Support/Databases"`, dst)
	o.d.docker("cp", dbFile, o.tools+":"+dst+"/Plug-in Support/Databases/com.plexapp.plugins.library.db")
	o.d.docker("cp", filepath.Join(local, "Preferences.xml"), o.tools+":"+dst+"/Preferences.xml")
	// Plex's files are the Plex user's; Bunkarr (PUID 1000) reads them.
	o.toolsSh(`chown -R 1000:1000 "$1" && chmod -R u+rwX,go-w "$1"`, dst)
}

// TestDockerOffsiteConfigVersions is acceptance 7 (docs/design/phase4.md §8, §14.6 item 7): a Plex
// DB backup (a stopped Plex server's data directory with Preferences.xml), an *arr backup (Phase 2's
// fake Radarr) and a manifest export each reach a restic and an rclone crypt destination; they are
// recorded and listed; a manifest downloads through the API with its checksum verified; three more
// Plex DB and *arr versions with a retention of daily 2 leave the two newest of each: rclone purges
// the older version directories at once, restic forgets their snapshots at the next retention run.
func TestDockerOffsiteConfigVersions(t *testing.T) {
	o := newOffsite(t)
	o.startMinIO()
	o.startFakeArr()
	plexToken := "plex-online-token-" + randomHex(10)
	o.secrets.add("PlexOnlineToken", plexToken)
	o.secrets.add("Radarr API key", fakeArrKey)
	o.writePlexData("plex/Plex Media Server", plexToken)
	o.writeRandom(map[string]int64{
		"arr/movies/Nosferatu (1922)/Nosferatu (1922).mkv":   500000,
		"arr/movies/Metropolis (1927)/Metropolis (1927).mkv": 600000,
	})
	b := o.startBunkarr("bunkarr", bunkarrOpts{})
	src := b.createSource("Movies", "arr/movies")
	ret := map[string]any{"deletedDays": 30, "plexDbDaily": 2, "plexDbWeekly": 0, "arrDaily": 2, "arrWeekly": 0}
	rs, rk := b.createOffsite(b.resticS3("Config restic", "cfg-restic", []int64{src.ID}, map[string]any{"retention": ret}))
	cs, ck := b.createOffsite(b.cryptS3("Config crypt", "cfg-crypt", []int64{src.ID}, map[string]any{"retention": ret}))
	dests := []offDest{rs, cs}
	for _, d := range dests {
		requireStatus(t, b.sync(d.ID, nil), "completed")
	}
	targets := []map[string]any{{"destinationId": rs.ID, "cron": "", "enabled": false}, {"destinationId": cs.ID, "cron": "", "enabled": false}}

	// Off-site backup targets need the session and the password (S29).
	var plex struct {
		ID int64 `json:"id"`
	}
	b.api.call(viaSession, 201, "POST", "/integrations", map[string]any{"type": "plex", "name": "Plex", "url": "http://plex.invalid:32400",
		"apiKey": "plex-api-token-" + randomHex(8), "currentPassword": e2ePassword,
		"settings": map[string]any{"dataPath": offsiteMedia + "/plex/Plex Media Server", "backup": map[string]any{"targets": targets}}}, &plex)
	var radarr struct {
		ID int64 `json:"id"`
	}
	b.api.call(viaSession, 201, "POST", "/integrations", map[string]any{"type": "radarr", "name": "Radarr",
		"url": "http://" + fakeArrHost + ":" + fakeArrPort, "apiKey": fakeArrKey, "currentPassword": e2ePassword,
		"settings": map[string]any{"pathMappings": []map[string]string{{"arr": "/movies", "local": offsiteMedia + "/arr/movies"}},
			"backup": map[string]any{"targets": targets}}}, &radarr)
	b.waitIdle(5 * time.Minute)

	backup := func(kind string, integ, dest int64) apiJob {
		t.Helper()
		path := fmt.Sprintf("/integrations/%d/plex/backup", integ)
		if kind == "arr" {
			path = fmt.Sprintf("/integrations/%d/arr/backup", integ)
		}
		j := b.run(path, map[string]any{"destinationId": dest}, 10*time.Minute)
		if j.Status != "completed" && j.Status != "completed_with_warnings" {
			requireStatus(t, j, "completed")
		}
		time.Sleep(1100 * time.Millisecond) // version names are to the second
		return j
	}
	for _, d := range dests {
		backup("plexdb", plex.ID, d.ID)
		backup("arr", radarr.ID, d.ID)
		requireStatus(t, b.run(fmt.Sprintf("/destinations/%d/manifest", d.ID), nil, 10*time.Minute), "completed")
		o.requireVersions(b, d, map[string]int{"plexdb": 1, "arr": 1})
		o.requireManifestDownload(b, d)
	}
	// Three more versions of each: with daily 2 (weekly 0) the two newest stay.
	for i := 0; i < 3; i++ {
		for _, d := range dests {
			backup("plexdb", plex.ID, d.ID)
			backup("arr", radarr.ID, d.ID)
		}
	}
	for _, d := range dests {
		o.requireVersions(b, d, map[string]int{"plexdb": 2, "arr": 2})
	}
	// rclone: the pruned version directories are gone (purged at once).
	for _, dir := range []string{".bunkarr/plex", ".bunkarr/arr"} {
		out, err := o.engineCLI(ck, "rclone", "lsf", "-R", "--files-only", "bunkarr-crypt:"+dir)
		if err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(out, "manifest.json"); n != 2 {
			t.Fatalf("rclone destination: %d versions with a manifest.json under %s, want 2:\n%s", n, dir, out)
		}
	}
	// restic: the pruned versions' snapshots are forgotten by the next retention run.
	for _, kind := range []string{"plexdb", "arr"} {
		if n := len(o.resticSnapshots(rk, "bunkarr-dest:"+rk.EngineTag+",bunkarr-kind:"+kind)); n != 4 {
			t.Fatalf("restic destination before retention: %d %s snapshots, want 4 (2 kept, 2 waiting for the forget)", n, kind)
		}
	}
	r := b.retention(rs.ID, nil)
	if r.Status != "completed" && r.Status != "completed_with_warnings" {
		requireStatus(t, r, "completed")
	}
	if st := decodeStats[retentionStatsE2E](t, r); st.SnapshotsForgotten != 4 {
		t.Fatalf("restic retention forgot %d snapshots, want the 4 pruned versions: %s", st.SnapshotsForgotten, r.Stats)
	}
	for _, kind := range []string{"plexdb", "arr"} {
		if n := len(o.resticSnapshots(rk, "bunkarr-dest:"+rk.EngineTag+",bunkarr-kind:"+kind)); n != 2 {
			t.Fatalf("restic destination after retention: %d %s snapshots, want 2", n, kind)
		}
	}
	for _, d := range dests {
		requireVerified(t, b.verify(d.ID))
	}
	o.auditAll()
}

// requireVersions requires destination d to list exactly want versions per kind (snapshots of
// kind plexdb and arr, each ok, and on restic with its snapshot as engineRef).
func (o *offsite) requireVersions(b *bunkarrC, d offDest, want map[string]int) {
	o.t.Helper()
	var list []struct {
		Kind      string `json:"kind"`
		Path      string `json:"path"`
		Integrity string `json:"integrity"`
		EngineRef string `json:"engineRef"`
	}
	b.api.call(viaKey, 200, "GET", fmt.Sprintf("/destinations/%d/snapshots", d.ID), nil, &list)
	got := map[string]int{}
	for _, s := range list {
		if _, ok := want[s.Kind]; !ok {
			continue
		}
		got[s.Kind]++
		if s.Integrity != "ok" || (d.Engine == "restic") != (len(s.EngineRef) == 64) {
			o.t.Fatalf("%s: version %+v", d.Name, s)
		}
	}
	for k, n := range want {
		if got[k] != n {
			o.t.Fatalf("%s lists %d %s versions, want %d: %+v", d.Name, got[k], k, n, list)
		}
	}
}

// requireManifestDownload downloads destination d's newest manifest through the API and checks it
// against the X-Bunkarr-SHA256 header and the recorded checksum.
func (o *offsite) requireManifestDownload(b *bunkarrC, d offDest) {
	o.t.Helper()
	var list []struct {
		ID       int64  `json:"id"`
		Checksum string `json:"checksum"`
	}
	b.api.call(viaKey, 200, "GET", fmt.Sprintf("/destinations/%d/manifests", d.ID), nil, &list)
	if len(list) == 0 {
		o.t.Fatalf("%s: no manifest listed", d.Name)
	}
	m := list[0]
	code, body, hdr, err := b.api.send(viaKey, "GET", fmt.Sprintf("/manifests/%d/download", m.ID), nil, true)
	if err != nil || code != 200 {
		o.t.Fatalf("%s: download manifest %d: HTTP %d %v: %s", d.Name, m.ID, code, err, body)
	}
	sum := sha256.Sum256(body)
	hexSum := hex.EncodeToString(sum[:])
	header := ""
	for _, l := range strings.Split(hdr, "\n") {
		if k, v, ok := strings.Cut(l, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "X-Bunkarr-SHA256") {
			header = strings.TrimSpace(v)
		}
	}
	if header != hexSum || m.Checksum != "sha256:"+hexSum {
		o.t.Fatalf("%s: manifest %d: %d bytes with sha256 %s; header %q, recorded checksum %q", d.Name, m.ID, len(body), hexSum,
			header, m.Checksum)
	}
	o.t.Logf("%s: manifest %d downloaded (%s bytes, sha256 %.12s) and verified", d.Name, m.ID, strconv.Itoa(len(body)), hexSum)
}
