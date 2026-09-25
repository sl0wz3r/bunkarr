//go:build e2e

package e2e

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// armOnce is the container command of the kill test: the fault point is armed only while its
// signal file does not exist, so `docker start` after the kill runs Bunkarr normally with the
// same container configuration. It then runs the image's own entrypoint (PUID/PGID, su-exec).
const armOnce = `if [ -e "$BUNKARR_FAULTPOINT_FILE" ]; then unset BUNKARR_FAULTPOINT BUNKARR_FAULTPOINT_FILE; fi; exec /entrypoint.sh`

// TestDockerKillResume is the container kill test of the Docker suite (design §9): the Bunkarr
// image runs with BUNKARR_FAULTPOINT=copy.afterWrite, a sync parks after writing its first temp
// file, the container is killed with `docker kill -s KILL` and started again with `docker start`;
// the job resumes (attempt 2) and completes, a full verify passes, and the backup volume holds
// exactly the source's files and no temp file.
func TestDockerKillResume(t *testing.T) {
	d := newDockerEnv(t)
	local := resolvedTempDir(t)
	lib := writeLibrary(t, filepath.Join(local, "movies"))

	cfg, media, backup := d.volume("config"), d.volume("media"), d.volume("backup")
	h := d.helper("fixture", d.image, "-v", media+":/media", "-v", backup+":/backup")
	d.cpIn(filepath.Join(local, "movies"), h, "/media/movies")
	// The media is root's and read-only for Bunkarr; the backup share is writable for PUID 1000.
	d.docker("exec", h, "sh", "-c", "chown -R 0:0 /media && chmod -R a+rX /media && chown 1000:1000 /backup")
	d.remove(h)

	const signal = "/config/faultpoint.hit"
	c := d.run("bunkarr", "-e", "PUID=1000", "-e", "PGID=1000",
		"-e", "BUNKARR_FAULTPOINT=copy.afterWrite", "-e", "BUNKARR_FAULTPOINT_FILE="+signal,
		"-v", cfg+":/config", "-v", media+":/media:ro", "-v", backup+":/backup",
		d.image, "sh", "-c", armOnce)
	api := newContainerAPI(d, c)
	api.waitHealthy(90 * time.Second)
	api.setup()
	src := api.createSource("Movies", "/media/movies")
	dest := api.createDestination("Backup", "/backup", src.ID)
	j := api.startSync(dest.ID, nil)

	deadline := time.Now().Add(3 * time.Minute)
	for !d.execOK(c, "test", "-e", signal) {
		if cur := api.job(j.ID); cur.final() {
			t.Fatalf("job %d ended %s (%q) without reaching copy.afterWrite", j.ID, cur.Status, cur.Error)
		}
		if time.Now().After(deadline) {
			t.Fatal("the container did not reach copy.afterWrite within 3 minutes")
		}
		time.Sleep(200 * time.Millisecond)
	}
	// Parked after writing a temp file: exactly one, and no final file holds it yet.
	tmp := d.docker("exec", c, "find", "/backup", "-name", ".bunkarr-tmp-*")
	if n := len(strings.Fields(tmp)); n == 0 || strings.Count(tmp, "\n") != 0 {
		t.Fatalf("temp files at the crash: %q, want exactly one", tmp)
	}

	d.docker("kill", "-s", "KILL", c)
	if d.running(c) {
		t.Fatal("the container survived docker kill -s KILL")
	}
	d.docker("start", c)
	api.waitHealthy(90 * time.Second)
	r := api.waitJob(j.ID, 5*time.Minute)
	requireStatus(t, r, "completed")
	if r.Attempt != 2 || r.Trigger != "resume" {
		t.Fatalf("resumed job %d: attempt %d, trigger %q; want attempt 2, trigger resume", r.ID, r.Attempt, r.Trigger)
	}
	requireSyncStats(t, r, false, wantSync{planned: lib.files, copied: lib.files - 1, updated: 0, moved: 0, linked: 1,
		retained: 0, held: 0, failed: 0, bytesPlanned: lib.uniqueBytes, bytesCopied: lib.uniqueBytes})
	if st := api.source(src.ID).Stats; st.Files != lib.files || st.UniqueBytes != lib.uniqueBytes || st.HardlinkGroups != 1 {
		t.Fatalf("source stats in the container: %+v (the fixture's hardlink must survive docker cp)", st)
	}
	requireVerifies(t, api.client, dest.ID)

	// The backup volume, read back through a helper: the mirror is exact, no temp file anywhere.
	h = d.helper("readback", d.image, "-v", backup+":/backup:ro")
	out := d.cpOut(h, "/backup", filepath.Join(local, "backup"))
	d.remove(h)
	verifyMirror(t, filepath.Join(local, "movies"), out, src.DestFolder)
	t.Logf("container kill test: job %d resumed as attempt %d and completed; %d files verified", r.ID, r.Attempt, lib.files)
}
