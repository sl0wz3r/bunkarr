//go:build e2e

package e2e

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
)

// crashAt runs one kill -9 scenario (design §9 E2E (3)): the server is restarted with
// BUNKARR_FAULTPOINT=point (it writes the signal file and parks when it reaches the point),
// startJob queues the job to interrupt, the process is killed with SIGKILL once it is parked,
// atCrash may inspect what the crash left, and the server is restarted normally. The job must
// resume (same job, attempt 2, trigger resume) and complete; the resumed job is returned.
func crashAt(t *testing.T, e *env, point string, startJob func() apiJob, atCrash func()) apiJob {
	t.Helper()
	s := e.srv
	signal := filepath.Join(e.root, "faultpoint-"+point)
	s.stop()
	s.start("BUNKARR_FAULTPOINT="+point, "BUNKARR_FAULTPOINT_FILE="+signal)
	j := startJob()
	waitForFile(t, signal, 3*time.Minute, func() string {
		if cur := s.job(j.ID); cur.final() {
			return fmt.Sprintf("job %d ended %s (%q) without reaching %s", j.ID, cur.Status, cur.Error, point)
		}
		return ""
	})
	s.kill()
	if atCrash != nil {
		atCrash()
	}
	s.start()
	r := s.waitJob(j.ID, 5*time.Minute)
	requireStatus(t, r, "completed")
	if r.Attempt != 2 || r.Trigger != "resume" {
		t.Fatalf("resumed job %d: attempt %d, trigger %q; want attempt 2, trigger resume", r.ID, r.Attempt, r.Trigger)
	}
	return r
}

// requireVerifies runs a verify job (verify mode full: every file is re-hashed), requires it to
// find nothing missing or damaged and returns it.
func requireVerifies(t *testing.T, s *client, destID int64) apiJob {
	t.Helper()
	j := s.runVerify(destID)
	requireStatus(t, j, "completed")
	vs := decodeStats[verifyStats](t, j)
	if vs.FilesMissing != 0 || vs.FilesFailed != 0 || vs.FilesVerified == 0 {
		t.Fatalf("verify after the resume: %s", j.Stats)
	}
	return j
}

// setHardlinkCapability rewrites the stored capability probe of a destination (server stopped),
// simulating a share without hardlink support (SMB without Unix extensions): updates then keep
// the old version by renaming it into retention, so the final name is free between the two
// renames. Temporary directories on the test hosts (APFS, ext4) always support hardlinks.
func setHardlinkCapability(t *testing.T, cfgDir string, destID int64, v bool) {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(cfgDir, "bunkarr.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	err = d.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE destinations SET capabilities = json_set(capabilities, '$.hardlinks', json(?)) WHERE id = ?`,
			strconv.FormatBool(v), destID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return fmt.Errorf("updated %d destinations (%v)", n, err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("set the hardlink capability: %v", err)
	}
}

// TestKillResume kills the process (SIGKILL) while a sync is parked at a fault point, restarts
// it, and requires the job to resume and the destination to verify with no temp file left:
// in the middle of a copy (after the temp file is written), between the two renames of an update
// on a destination without hardlinks, after the old version of an update was hardlinked into
// retention, and while planning (after the first persisted batch of items). Every job, the
// crashed and resumed one included, must leave the source tree as the test left it (S1).
func TestKillResume(t *testing.T) {
	t.Parallel()

	t.Run("copy.afterWrite", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		s := e.srv
		srcDir := e.dir("media/library")
		lib := writeLibrary(t, srcDir)
		target := e.target()
		src := s.createSource("Library", srcDir)
		dest := s.createDestination("NAS", target, src.ID)

		srcBefore := snapshot(t, srcDir)
		r := crashAt(t, e, "copy.afterWrite", func() apiJob { return s.startSync(dest.ID, nil) }, func() {
			// Killed after writing a temp file, before its rename: no final file holds it.
			if tmp := tempFiles(t, target); len(tmp) != 1 {
				t.Fatalf("temp files at the crash: %v, want exactly one", tmp)
			}
		})
		requireSourceUnchanged(t, r, srcBefore, srcDir)
		requireSyncStats(t, r, false, wantSync{planned: lib.files, copied: lib.files - 1, updated: 0, moved: 0, linked: 1,
			retained: 0, held: 0, failed: 0, bytesPlanned: lib.uniqueBytes, bytesCopied: lib.uniqueBytes})
		verifyMirror(t, srcDir, target, src.DestFolder)
		untouched(t, srcDir, func() apiJob { return requireVerifies(t, s.client, dest.ID) })
	})

	t.Run("update.afterRenameOld", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		s := e.srv
		srcDir := e.dir("media/shows")
		total := writeFlat(t, srcDir, "Shows", 30, 10, 20000)
		target := e.target()
		src := s.createSource("Shows", srcDir)
		dest := s.createDestination("NAS", target, src.ID)
		s.stop()
		setHardlinkCapability(t, s.cfg, dest.ID, false)

		changed := flatName("Shows", 7, 10)
		finalPath := filepath.Join(target, src.DestFolder, filepath.FromSlash(changed))
		var old fileSum
		var srcBefore map[string]entry
		newContent := content(changed, 2, 30000)
		r := crashAt(t, e, "update.afterRenameOld", func() apiJob {
			if s.destination(dest.ID).Capabilities.Hardlinks {
				t.Fatal("the destination still reports hardlink support")
			}
			// Copies only: the fault point is not reached.
			j := untouched(t, srcDir, func() apiJob { return s.runSync(dest.ID, nil) })
			requireStatus(t, j, "completed")
			requireSyncStats(t, j, false, wantSync{planned: 30, copied: 30, updated: 0, moved: 0, linked: 0, retained: 0,
				held: 0, failed: 0, bytesPlanned: total, bytesCopied: total})
			old = hashTree(t, filepath.Join(target, src.DestFolder))[changed]
			writeFile(t, srcDir, changed, newContent)
			srcBefore = snapshot(t, srcDir)
			return s.startSync(dest.ID, nil)
		}, func() {
			// Between the renames: the old version is in retention, the final name is free and the
			// verified new version waits in its temp file.
			if _, err := os.Lstat(finalPath); !os.IsNotExist(err) {
				t.Fatalf("the final name exists between the two renames (%v)", err)
			}
			if m := retained(t, target, src.DestFolder, changed); len(m) != 1 {
				t.Fatalf("retained old versions at the crash: %v", m)
			}
			if tmp := tempFiles(t, target); len(tmp) != 1 {
				t.Fatalf("temp files at the crash: %v, want exactly one", tmp)
			}
		})
		requireSourceUnchanged(t, r, srcBefore, srcDir)
		requireSyncStats(t, r, false, wantSync{planned: 1, copied: 0, updated: 1, moved: 0, linked: 0, retained: 0, held: 0,
			failed: 0, bytesPlanned: int64(len(newContent)), bytesCopied: int64(len(newContent))})
		verifyMirror(t, srcDir, target, src.DestFolder)
		requireRetainedVersion(t, target, src.DestFolder, changed, old)
		untouched(t, srcDir, func() apiJob { return requireVerifies(t, s.client, dest.ID) })
	})

	t.Run("update.afterLinkOld", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		s := e.srv
		srcDir := e.dir("media/library")
		lib := writeLibrary(t, srcDir)
		target := e.target()
		src := s.createSource("Library", srcDir)
		dest := s.createDestination("NAS", target, src.ID)
		if !s.destination(dest.ID).Capabilities.Hardlinks {
			t.Skip("the temporary directory's filesystem has no hardlinks")
		}
		const changed = "Movies/Alien (1979)/Alien (1979).mkv"
		finalPath := filepath.Join(target, src.DestFolder, filepath.FromSlash(changed))
		var old fileSum
		var srcBefore map[string]entry
		newContent := content(changed, 2, 300000)
		r := crashAt(t, e, "update.afterLinkOld", func() apiJob {
			j := untouched(t, srcDir, func() apiJob { return s.runSync(dest.ID, nil) })
			requireStatus(t, j, "completed")
			requireSyncStats(t, j, false, wantSync{planned: lib.files, copied: lib.files - 1, updated: 0, moved: 0, linked: 1,
				retained: 0, held: 0, failed: 0, bytesPlanned: lib.uniqueBytes, bytesCopied: lib.uniqueBytes})
			old = hashTree(t, filepath.Join(target, src.DestFolder))[changed]
			writeFile(t, srcDir, changed, newContent)
			srcBefore = snapshot(t, srcDir)
			return s.startSync(dest.ID, nil)
		}, func() {
			// The old version is hardlinked into retention and still at its name (no gap).
			m := retained(t, target, src.DestFolder, changed)
			if len(m) != 1 || !sameFile(t, m[0], finalPath) {
				t.Fatalf("retained old versions at the crash: %v (want one hardlink of %s, %s)", m, finalPath, links(t, finalPath))
			}
			if tmp := tempFiles(t, target); len(tmp) != 1 {
				t.Fatalf("temp files at the crash: %v, want exactly one", tmp)
			}
		})
		requireSourceUnchanged(t, r, srcBefore, srcDir)
		requireSyncStats(t, r, false, wantSync{planned: 1, copied: 0, updated: 1, moved: 0, linked: 0, retained: 0, held: 0,
			failed: 0, bytesPlanned: int64(len(newContent)), bytesCopied: int64(len(newContent))})
		verifyMirror(t, srcDir, target, src.DestFolder)
		requireRetainedVersion(t, target, src.DestFolder, changed, old)
		untouched(t, srcDir, func() apiJob { return requireVerifies(t, s.client, dest.ID) })
	})

	t.Run("plan.afterBatch", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		s := e.srv
		// More than one plan batch (1000 items), so the plan is persisted in two parts.
		const files = 1050
		srcDir := e.dir("media/bulk")
		total := writeFlat(t, srcDir, "Bulk", files, 50, 100)
		target := e.target()
		src := s.createSource("Bulk", srcDir)
		dest := s.createDestination("NAS", target, src.ID)

		srcBefore := snapshot(t, srcDir)
		r := crashAt(t, e, "plan.afterBatch", func() apiJob { return s.startSync(dest.ID, nil) }, func() {
			// Killed while planning: nothing was copied yet.
			if _, err := os.Lstat(filepath.Join(target, src.DestFolder)); !os.IsNotExist(err) {
				t.Fatalf("the destination folder exists after a crash during planning (%v)", err)
			}
		})
		requireSourceUnchanged(t, r, srcBefore, srcDir)
		// The half-persisted plan was discarded and made again: every file exactly once.
		requireSyncStats(t, r, false, wantSync{planned: files, copied: files, updated: 0, moved: 0, linked: 0, retained: 0,
			held: 0, failed: 0, bytesPlanned: total, bytesCopied: total})
		verifyMirror(t, srcDir, target, src.DestFolder)
		untouched(t, srcDir, func() apiJob { return requireVerifies(t, s.client, dest.ID) })
	})
}

// requireRetainedVersion requires exactly one retained copy of rel, holding the old content.
func requireRetainedVersion(t *testing.T, target, folder, rel string, old fileSum) {
	t.Helper()
	m := retained(t, target, folder, rel)
	if len(m) != 1 {
		t.Fatalf("retained versions of %s: %v, want one", rel, m)
	}
	sum, size, err := sha256File(m[0])
	if err != nil || sum != old.SHA || size != old.Size {
		t.Fatalf("retained %s: %d bytes %s (%v), want the old %d bytes %s", rel, size, sum, err, old.Size, old.SHA)
	}
}
