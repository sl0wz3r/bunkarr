//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// env is one test's world: a resolved temporary root holding the config directory, the sources
// and the destinations, and a running server with the first-run setup done.
type env struct {
	t    *testing.T
	root string
	srv  *server
}

// newEnv starts a fresh server for the test.
func newEnv(t *testing.T) *env {
	t.Helper()
	root := resolvedTempDir(t)
	e := &env{t: t, root: root, srv: newServer(t, root)}
	e.srv.start()
	e.srv.setup()
	return e
}

// dir creates and returns <root>/<rel>.
func (e *env) dir(rel string) string {
	e.t.Helper()
	p := filepath.Join(e.root, filepath.FromSlash(rel))
	mkdirAll(e.t, p)
	return p
}

// target creates and returns a directory for a destination target: <root>/nas, or, when
// $BUNKARR_E2E_TARGET_ROOT is set, a new directory under it, removed when the test ends (the
// network share test, docker/test-shares.sh, points it at a mounted SMB or NFS share).
func (e *env) target() string {
	e.t.Helper()
	root := os.Getenv("BUNKARR_E2E_TARGET_ROOT")
	if root == "" {
		return e.dir("nas")
	}
	dir, err := os.MkdirTemp(root, "nas-")
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			e.t.Logf("remove %s: %v", dir, err)
		}
	})
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		e.t.Fatal(err)
	}
	return dir
}

// requireStatus fails unless job j ended with status want.
func requireStatus(t *testing.T, j apiJob, want string) {
	t.Helper()
	if j.Status != want {
		t.Fatalf("job %d (%s): status %s, want %s; error %q, summary %q, stats %s", j.ID, j.Type, j.Status, want, j.Error, j.Summary, j.Stats)
	}
}

// wantSync is the sync stats a step expects (every field is compared).
type wantSync struct {
	planned, copied, updated, moved, linked, retained, held, failed, bytesPlanned, bytesCopied int64
}

// requireSyncStats compares a sync job's stats with want. No test here expects an adopted or a
// displaced file, so those must be zero.
func requireSyncStats(t *testing.T, j apiJob, dryRun bool, want wantSync) syncStats {
	t.Helper()
	st := decodeStats[syncStats](t, j)
	check := func(name string, got, want int64) {
		t.Helper()
		if got != want {
			t.Errorf("job %d: %s = %d, want %d (stats %s)", j.ID, name, got, want, j.Stats)
		}
	}
	if st.DryRun != dryRun {
		t.Errorf("job %d: dryRun = %v, want %v", j.ID, st.DryRun, dryRun)
	}
	check("filesPlanned", st.FilesPlanned, want.planned)
	check("filesCopied", st.FilesCopied, want.copied)
	check("filesUpdated", st.FilesUpdated, want.updated)
	check("filesMoved", st.FilesMoved, want.moved)
	check("filesLinked", st.FilesLinked, want.linked)
	check("filesRetained", st.FilesRetained, want.retained)
	check("filesHeld", st.FilesHeld, want.held)
	check("filesFailed", st.FilesFailed, want.failed)
	check("bytesPlanned", st.BytesPlanned, want.bytesPlanned)
	check("bytesCopied", st.BytesCopied, want.bytesCopied)
	check("filesAdopted", st.FilesAdopted, 0)
	check("filesDisplaced", st.FilesDisplaced, 0)
	if t.Failed() {
		t.FailNow()
	}
	return st
}

// TestSyncLifecycle is design §9 E2E (1), (2), (4), (5) and (6) against one server: a full sync
// of the library fixture, an incremental sync after one change, one addition, one deletion and
// one rename, a verify, a dry run, a sync without the destination marker and a scan/sync of an
// emptied source root. Every job must leave the source tree exactly as the test left it (S1).
func TestSyncLifecycle(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	s := e.srv
	srcDir := e.dir("media/library")
	lib := writeLibrary(t, srcDir)
	target := e.target()
	src := s.createSource("Library", srcDir)
	dest := s.createDestination("NAS", target, src.ID)
	if dest.Target != target {
		t.Fatalf("destination target %q, want %q", dest.Target, target)
	}
	folder := src.DestFolder
	if folder == "" {
		t.Fatalf("source has no destFolder: %+v", src)
	}
	// The steps build on each other: stop at the first failure.
	step := func(name string, f func(t *testing.T)) {
		t.Helper()
		if !t.Run(name, f) {
			t.FailNow()
		}
	}

	// syncJob runs a sync and requires it to leave the source as the test left it (S1).
	syncJob := func(t *testing.T, body map[string]any) apiJob {
		t.Helper()
		return untouched(t, srcDir, func() apiJob { return s.runSync(dest.ID, body) })
	}

	step("full sync", func(t *testing.T) {
		j := syncJob(t, nil)
		requireStatus(t, j, "completed")
		// (2) The hardlinked pair is one copy and one link; bytes count its content once.
		requireSyncStats(t, j, false, wantSync{planned: lib.files, copied: lib.files - 1, updated: 0, moved: 0, linked: 1,
			retained: 0, held: 0, failed: 0, bytesPlanned: lib.uniqueBytes, bytesCopied: lib.uniqueBytes})
		verifyMirror(t, srcDir, target, folder)

		got := s.source(src.ID)
		want := sourceStats{Files: lib.files, Bytes: lib.bytes, UniqueBytes: lib.uniqueBytes, HardlinkGroups: 1}
		if got.Stats != want || got.LastScanStatus != "ok" {
			t.Fatalf("source after the scan: stats %+v (want %+v), lastScanStatus %q", got.Stats, want, got.LastScanStatus)
		}
		// Recreate mode on a destination that supports hardlinks: the second name is a hardlink.
		if s.destination(dest.ID).Capabilities.Hardlinks {
			a, b := filepath.Join(target, folder, linkTarget), filepath.Join(target, folder, linkName)
			if !sameFile(t, a, b) {
				t.Fatalf("the hardlinked pair is two files at the destination (%s; %s)", links(t, a), links(t, b))
			}
		}
	})

	step("second sync copies nothing", func(t *testing.T) {
		j := syncJob(t, nil)
		requireStatus(t, j, "completed")
		requireSyncStats(t, j, false, wantSync{planned: 0, copied: 0, updated: 0, moved: 0, linked: 0, retained: 0, held: 0,
			failed: 0, bytesPlanned: 0, bytesCopied: 0})
		verifyMirror(t, srcDir, target, folder)
	})

	step("incremental sync", func(t *testing.T) {
		const (
			changed = "Movies/Alien (1979)/Alien (1979).en.srt"
			added   = "Movies/Brazil (1985)/Brazil (1985).mkv"
			deleted = "Movies/Cube (1997)/Cube (1997).nfo"
			oldName = "Movies/Dune (2021)/Dune.mkv"
			newName = "Movies/Dune (2021)/Dune (2021).mkv"
		)
		oldChanged := hashTree(t, filepath.Join(srcDir, "Movies/Alien (1979)"))["Alien (1979).en.srt"]
		oldDeleted := hashTree(t, filepath.Join(srcDir, "Movies/Cube (1997)"))["Cube (1997).nfo"]
		newChanged := content(changed, 2, 4500)
		newAdded := content(added, 1, 75000)
		writeFile(t, srcDir, changed, newChanged)
		writeFile(t, srcDir, added, newAdded)
		if err := os.Remove(filepath.Join(srcDir, deleted)); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(srcDir, oldName), filepath.Join(srcDir, newName)); err != nil {
			t.Fatal(err)
		}

		j := syncJob(t, nil)
		requireStatus(t, j, "completed")
		changedBytes := int64(len(newChanged) + len(newAdded))
		requireSyncStats(t, j, false, wantSync{planned: 4, copied: 1, updated: 1, moved: 1, linked: 0, retained: 1, held: 0,
			failed: 0, bytesPlanned: changedBytes, bytesCopied: changedBytes})
		verifyMirror(t, srcDir, target, folder)

		// S5/S6: the deleted file and the replaced version are kept in retention.
		for rel, want := range map[string]fileSum{deleted: oldDeleted, changed: oldChanged} {
			m := retained(t, target, folder, rel)
			if len(m) != 1 {
				t.Fatalf("retained copies of %s: %v, want one", rel, m)
			}
			if sum, size, err := sha256File(m[0]); err != nil || sum != want.SHA || size != want.Size {
				t.Fatalf("retained %s: %d bytes %s (%v), want the old %d bytes %s", rel, size, sum, err, want.Size, want.SHA)
			}
		}
		// A rename is a rename at the destination: nothing of the old name is retained.
		if m := retained(t, target, folder, oldName); len(m) != 0 {
			t.Fatalf("the renamed file was retained under its old name: %v", m)
		}
		if st := s.source(src.ID).Stats; st.Files != lib.files || st.UniqueBytes != lib.uniqueBytes-1200+75000+1500 {
			t.Fatalf("source stats after the changes: %+v", st)
		}
	})

	step("verify", func(t *testing.T) {
		j := untouched(t, srcDir, func() apiJob { return s.runVerify(dest.ID) })
		requireStatus(t, j, "completed")
		vs := decodeStats[verifyStats](t, j)
		if vs.FilesMissing != 0 || vs.FilesFailed != 0 || vs.FilesVerified == 0 {
			t.Fatalf("verify stats: %s", j.Stats)
		}
	})

	step("dry run writes nothing", func(t *testing.T) {
		writeFile(t, srcDir, "Movies/Cube (1997)/Cube (1997).nfo", content("restored nfo", 1, 1300))
		writeFile(t, srcDir, "Movies/Up (2009)/Up (2009).nfo", content("Movies/Up (2009)/Up (2009).nfo", 2, 1000))
		if err := os.Remove(filepath.Join(srcDir, "Extras/a/b/c/d/deep.bin")); err != nil {
			t.Fatal(err)
		}
		before := snapshot(t, target)
		j := syncJob(t, map[string]any{"dryRun": true})
		requireStatus(t, j, "completed")
		// A dry run counts what would happen and copies nothing.
		requireSyncStats(t, j, true, wantSync{planned: 3, copied: 1, updated: 1, moved: 0, linked: 0, retained: 1, held: 0,
			failed: 0, bytesPlanned: 2300, bytesCopied: 0})
		requireUnchanged(t, "a dry run", before, target)
		// Its items are the preview.
		items := s.items(j.ID, "")
		byAction := map[string]int{}
		for _, it := range items {
			byAction[it.Action]++
		}
		if len(items) != 3 || byAction["copy"] != 1 || byAction["update"] != 1 || byAction["retain"] != 1 {
			t.Fatalf("dry run items: %+v", items)
		}

		// The same plan, for real.
		j = syncJob(t, nil)
		requireStatus(t, j, "completed")
		requireSyncStats(t, j, false, wantSync{planned: 3, copied: 1, updated: 1, moved: 0, linked: 0, retained: 1, held: 0,
			failed: 0, bytesPlanned: 2300, bytesCopied: 2300})
		verifyMirror(t, srcDir, target, folder)
	})

	step("missing destination marker", func(t *testing.T) {
		writeFile(t, srcDir, "Movies/Zodiac (2007)/Zodiac (2007).nfo", content("zodiac nfo", 1, 800))
		marker := filepath.Join(target, ".bunkarr", "destination.json")
		aside := target + ".marker-aside" // on the target's filesystem
		if err := os.Rename(marker, aside); err != nil {
			t.Fatal(err)
		}
		before := snapshot(t, target)
		j := syncJob(t, nil)
		requireStatus(t, j, "failed")
		if !strings.Contains(j.Error, "not mounted") {
			t.Fatalf("sync without a marker failed with %q, want a \"destination not mounted?\" error", j.Error)
		}
		requireUnchanged(t, "a sync without the marker", before, target)
		// The share is back: the next sync catches up.
		if err := os.Rename(aside, marker); err != nil {
			t.Fatal(err)
		}
		j = syncJob(t, nil)
		requireStatus(t, j, "completed")
		requireSyncStats(t, j, false, wantSync{planned: 1, copied: 1, updated: 0, moved: 0, linked: 0, retained: 0, held: 0,
			failed: 0, bytesPlanned: 800, bytesCopied: 800})
		verifyMirror(t, srcDir, target, folder)
	})

	step("emptied source root", func(t *testing.T) {
		srcBefore := s.source(src.ID)
		deletedBefore := s.deletedFiles(src.ID)
		// The root itself stays (the root-identity checks of S10a pass on every filesystem) and its
		// entries move away, as when the disk holding a user share's files is missing on Unraid.
		away := filepath.Join(e.root, "media", "library.away")
		mkdirAll(t, away)
		moveEntries(t, srcDir, away)
		before := snapshot(t, target)
		awayBefore := snapshot(t, away)

		scan := untouched(t, srcDir, func() apiJob {
			var scan apiJob
			s.call(http.StatusAccepted, "POST", fmt.Sprintf("/sources/%d/scan", src.ID), nil, &scan)
			return s.waitJob(scan.ID, time.Minute)
		})
		requireStatus(t, scan, "failed")
		if !strings.Contains(scan.Error, "contains no files") {
			t.Fatalf("scan of an empty root failed with %q", scan.Error)
		}
		j := syncJob(t, nil)
		requireStatus(t, j, "failed")
		if !strings.Contains(j.Error, "contains no files") {
			t.Fatalf("sync of an empty root failed with %q", j.Error)
		}
		requireUnchanged(t, "a sync of an empty source", before, target)
		requireSourceUnchanged(t, j, awayBefore, away)
		// The catalog is untouched: nothing was marked deleted, so nothing can be retained.
		after := s.source(src.ID)
		if after.Stats != srcBefore.Stats || after.LastScanStatus != "failed" {
			t.Fatalf("source after the failed scans: %+v (before %+v)", after, srcBefore)
		}
		if n := s.deletedFiles(src.ID); n != deletedBefore {
			t.Fatalf("deleted catalog files: %d, before %d", n, deletedBefore)
		}

		// Back: nothing to do.
		moveEntries(t, away, srcDir)
		j = syncJob(t, nil)
		requireStatus(t, j, "completed")
		requireSyncStats(t, j, false, wantSync{planned: 0, copied: 0, updated: 0, moved: 0, linked: 0, retained: 0, held: 0,
			failed: 0, bytesPlanned: 0, bytesCopied: 0})
		verifyMirror(t, srcDir, target, folder)
	})
}

// moveEntries renames every entry of the directory from into the directory to.
func moveEntries(t *testing.T, from, to string) {
	t.Helper()
	entries, err := os.ReadDir(from)
	if err != nil {
		t.Fatal(err)
	}
	for _, de := range entries {
		if err := os.Rename(filepath.Join(from, de.Name()), filepath.Join(to, de.Name())); err != nil {
			t.Fatal(err)
		}
	}
}

// deletedFiles returns how many catalog files of a source are marked deleted.
func (s *client) deletedFiles(sourceID int64) int64 {
	s.t.Helper()
	var p apiPage[struct {
		ID int64 `json:"id"`
	}]
	s.call(http.StatusOK, "GET", fmt.Sprintf("/sources/%d/files?filter=deleted&pageSize=1", sourceID), nil, &p)
	return p.TotalRecords
}
