package syncer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// stopResume runs the resume of a job that crashed so that it ends before it reaches its pending
// items: the destination is not mounted at restart ("unmounted"), or the user cancels the resumed
// job as it starts ("cancelled"). Such a job is failed or cancelled and never resumed again.
func (h *harness) stopResume(t *testing.T, j jobs.Job, how string) {
	t.Helper()
	j.Attempt, j.Trigger = 2, jobs.TriggerResume
	switch how {
	case "unmounted":
		marker := h.dstPath(filecopy.MarkerRel)
		if err := os.Rename(marker, marker+".away"); err != nil {
			t.Fatal(err)
		}
		if _, err := h.run(h.sync, j); !errors.Is(err, destinations.ErrNotMounted) {
			t.Fatalf("resume: err = %v, want ErrNotMounted", err)
		}
		if err := os.Rename(marker+".away", marker); err != nil {
			t.Fatal(err)
		}
	case "cancelled":
		ctx, cancel := context.WithCancel(h.ctx)
		cancel()
		_, err := h.sync.Run(ctx, j, h.env())
		h.closeJob(j.ID)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("resume: err = %v, want cancelled", err)
		}
	default:
		t.Fatalf("unknown stop %q", how)
	}
}

// crashThenStop crashes a sync at the first occurrence of point and stops its resume (stopResume).
func (h *harness) crashThenStop(t *testing.T, point, how string) jobs.Job {
	t.Helper()
	j := h.newJob(jobs.TypeSync, false, jobs.Params{})
	if crashed, _, err := runWithHook(h, j, faultinject.CrashAt(point, 1)); !crashed {
		t.Fatalf("no crash at %s: %v", point, err)
	}
	h.stopResume(t, j, how)
	return j
}

// retainedOf returns the retained records of a destination path.
func (h *harness) retainedOf(rel string) []Record {
	var out []Record
	for _, r := range h.records() {
		if r.State == StateRetained && r.RelPath == rel {
			out = append(out, r)
		}
	}
	return out
}

// TestRetainRenamedByAStoppedJobIsSettled: a retain renames a vanished file into retention and the
// process dies before the record is written; the resumed job stops before it reaches the retain
// (the share is not mounted yet, or the user cancels it), so it is never resumed again. The record
// still says present with no file there, and the content sits unrecorded in retention. The next
// job of the destination, whatever its type, settles the rename the record's intent describes (the
// file goes back to its path), and the next sync retains it properly: nothing is lost and nothing
// stays unrecorded.
func TestRetainRenamedByAStoppedJobIsSettled(t *testing.T) {
	for _, how := range []string{"unmounted", "cancelled"} {
		for _, next := range []string{"sync", "verify", "retention"} {
			t.Run(how+"/"+next, func(t *testing.T) {
				h := newHarness(t)
				h.writeSrc("k.mkv", content("k", 500), 1)
				h.writeSrc("a.mkv", content("a", 1800), 4)
				h.firstSync()
				h.removeSrc("a.mkv")
				h.crashThenStop(t, PointRetainAfterRename, how)
				switch next {
				case "verify":
					if _, st, _ := h.mustVerify(); st.FilesMissing != 0 {
						t.Fatalf("verify: %+v (the file was not put back first)\n%s", st, h.rep.dump())
					}
				case "retention":
					if _, _, _, err := h.runRetention(); err != nil {
						t.Fatal(err)
					}
				}
				if next != "sync" {
					if r, ok := h.liveRecord("movies/a.mkv"); !ok || r.State != StatePresent || r.RetainedPath != "" ||
						!exists(h.dstPath("movies/a.mkv")) {
						t.Fatalf("after the %s: a.mkv %+v, file there %v", next, r, exists(h.dstPath("movies/a.mkv")))
					}
				}
				if _, st, j := h.mustSync(false, jobs.Params{}); st.FilesRetained != 1 || st.FilesFailed != 0 {
					t.Fatalf("next sync: %+v items %+v\n%s", st, h.items(j.ID), h.rep.dump())
				}
				h.assertConverged(t)
				kept := h.retainedOf("movies/a.mkv")
				if len(kept) != 1 || kept[0].Reason != ReasonDeleted || fileHash(t, h.dstPath(kept[0].RetainedPath)) != strHash(content("a", 1800)) {
					t.Fatalf("retained %+v", kept)
				}
				if _, st, _ := h.mustSync(false, jobs.Params{}); st.FilesPlanned != 0 {
					t.Fatalf("not converged: %+v", st)
				}
			})
		}
	}
}

// TestWhatAStoppedJobMovedIntoRetentionIsRecorded: an unmanaged file displaced into retention, and
// the old version an update renamed or hardlinked into retention, by a job that crashed and whose
// resume stopped before reaching the item: the next sync records (or puts back) what is in the
// stopped job's retention directory and removes its temp file.
func TestWhatAStoppedJobMovedIntoRetentionIsRecorded(t *testing.T) {
	cases := []struct {
		name  string
		setup func(h *harness)
		point string
		keep  []string
	}{
		{
			name: "displaced",
			setup: func(h *harness) {
				h.writeSrc("k.mkv", content("k", 500), 1)
				h.firstSync()
				h.writeSrc("x.mkv", content("x", 1200), 2)
				writeFileAt(h.t, h.dstPath("movies/x.mkv"), "junk-x", baseTime)
			},
			point: PointDisplaceAfterRename,
			keep:  []string{"junk-x", content("x", 1200)},
		},
		{
			name: "update renamed the old version",
			setup: func(h *harness) {
				h.setCaps(func(c *filecopy.Capabilities) { c.Hardlinks = false })
				h.writeSrc("k.mkv", content("k", 500), 1)
				h.writeSrc("a.mkv", content("a", 1800), 2)
				h.firstSync()
				h.writeSrc("a.mkv", content("A", 1900), 3)
			},
			point: PointUpdateAfterRenameOld,
			keep:  []string{content("a", 1800), content("A", 1900)},
		},
		{
			name: "update hardlinked the old version",
			setup: func(h *harness) {
				h.writeSrc("k.mkv", content("k", 500), 1)
				h.writeSrc("a.mkv", content("a", 1800), 2)
				h.firstSync()
				h.writeSrc("a.mkv", content("A", 1900), 3)
			},
			point: PointUpdateAfterLinkOld,
			keep:  []string{content("a", 1800), content("A", 1900)},
		},
	}
	for _, c := range cases {
		for _, how := range []string{"unmounted", "cancelled"} {
			t.Run(c.name+"/"+how, func(t *testing.T) {
				h := newHarness(t)
				c.setup(h)
				h.crashThenStop(t, c.point, how)
				if _, st, j := h.mustSync(false, jobs.Params{}); st.FilesFailed != 0 {
					t.Fatalf("next sync: %+v items %+v\n%s", st, h.items(j.ID), h.rep.dump())
				}
				h.assertConverged(t)
				for _, v := range c.keep {
					if !h.hasContent(t, v) {
						t.Errorf("lost content %.12q...", v)
					}
				}
				if _, st, _ := h.mustSync(false, jobs.Params{}); st.FilesPlanned != 0 {
					t.Fatalf("not converged: %+v", st)
				}
			})
		}
	}
}

// cancelAtFile is a reporter that cancels the job when a file becomes current in a phase.
type cancelAtFile struct {
	*recReporter
	phase, file string
	cancel      func()
}

func (c *cancelAtFile) Progress(p jobs.Progress) {
	c.recReporter.Progress(p)
	if p.Phase == c.phase && p.CurrentFile == c.file {
		c.cancel()
	}
}

// TestCancelledRetainOfAResumedJobPutsTheFileBack: the resumed job reaches a retain the crashed
// attempt had renamed, and the user cancels it right then (a cancelled job is not resumed). The
// retain puts the file back itself, before the job ends: the record says present and the file is
// there, nothing is left in the job's retention directory.
func TestCancelledRetainOfAResumedJobPutsTheFileBack(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("k.mkv", content("k", 500), 1)
	h.writeSrc("a.mkv", content("a", 1800), 4)
	h.firstSync()
	h.removeSrc("a.mkv")
	j := h.newJob(jobs.TypeSync, false, jobs.Params{})
	if crashed, _, err := runWithHook(h, j, faultinject.CrashAt(PointRetainAfterRename, 1)); !crashed {
		t.Fatalf("no crash: %v", err)
	}
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	rep := &cancelAtFile{recReporter: h.rep, phase: "retaining", file: "movies/a.mkv", cancel: cancel}
	j.Attempt, j.Trigger = 2, jobs.TriggerResume
	_, err := h.sync.Run(ctx, j, jobs.Env{Reporter: rep, Items: h.jq})
	h.closeJob(j.ID)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("resume: err = %v, want cancelled", err)
	}
	if r, ok := h.liveRecord("movies/a.mkv"); !ok || r.State != StatePresent || r.RetainedPath != "" ||
		fileHash(t, h.dstPath("movies/a.mkv")) != strHash(content("a", 1800)) {
		t.Fatalf("after the cancelled retain: %+v", r)
	}
	if dir := h.dstPath(filecopy.RetentionDir(j.QueuedAt, j.ID)); exists(dir) && len(regularFiles(t, dir)) != 0 {
		t.Fatalf("left in the job's retention directory: %v", regularFiles(t, dir))
	}
	if _, st, j2 := h.mustSync(false, jobs.Params{}); st.FilesRetained != 1 || st.FilesFailed != 0 {
		t.Fatalf("next sync: %+v items %+v", st, h.items(j2.ID))
	}
	h.assertConverged(t)
}

// damageSharedInode syncs p.mkv with its hardlink q.mkv (made at the destination), damages their
// shared inode (same size), and runs a verify that samples only p: p is marked missing, q stays
// linked. It returns the damaged content's hash.
func (h *harness) damageSharedInode(t *testing.T) string {
	t.Helper()
	h.writeSrc("p.mkv", content("p", 3000), 1)
	h.linkSrc("p.mkv", "q.mkv")
	h.writeSrc("other.mkv", content("o", 10), 2)
	h.mustSync(false, jobs.Params{})
	if inode(t, h.dstPath("movies/p.mkv")) != inode(t, h.dstPath("movies/q.mkv")) {
		t.Fatal("setup: not linked")
	}
	f, err := os.OpenFile(h.dstPath("movies/p.mkv"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("XX"), 10); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	h.setSettings(func(s *destinations.Settings) { s.Verify.Mode, s.Verify.SamplePercent = destinations.VerifySample, 1 })
	h.dbExec(`UPDATE destination_files SET verified_at = ? WHERE rel_path <> 'movies/p.mkv'`, "2026-01-01T00:00:00.000000000Z")
	if _, st, _ := h.mustVerify(); st.FilesMissing != 1 {
		t.Fatalf("verify: %+v", st)
	}
	if r, _ := h.liveRecord("movies/q.mkv"); r.State != StateLinked {
		t.Fatalf("setup: q is %s", r.State)
	}
	return fileHash(t, h.dstPath("movies/q.mkv"))
}

// TestDamagedVersionsAreNeverRetainedWithAHash: whatever path sends a damaged file to retention, its
// retained record says damaged and carries no hash (a recorded hash its content does not have would
// present corrupt data as an intact version). The hardlink of a primary verify found damaged keeps
// the damaged inode after the primary's file is removed at the destination, or after the primary is
// deleted at the source; the primary's own damaged file is damaged too.
func TestDamagedVersionsAreNeverRetainedWithAHash(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(h *harness)
	}{
		{"primary removed at the destination", func(h *harness) {
			if err := os.Remove(h.dstPath("movies/p.mkv")); err != nil {
				h.t.Fatal(err)
			}
		}},
		{"primary deleted at the source", func(h *harness) { h.removeSrc("p.mkv") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			damaged := h.damageSharedInode(t)
			tc.change(h)
			if _, st, j := h.mustSync(false, jobs.Params{}); st.FilesFailed != 0 {
				t.Fatalf("sync: %+v items %+v", st, h.items(j.ID))
			}
			h.assertConverged(t)
			n := 0
			for _, r := range h.records() {
				if r.State != StateRetained || fileHash(t, h.dstPath(r.RetainedPath)) != damaged {
					continue
				}
				n++
				if r.Reason != ReasonDamaged || r.Hash != "" {
					t.Errorf("damaged content of %s retained as %q with hash %q", r.RelPath, r.Reason, r.Hash)
				}
			}
			if n == 0 {
				t.Fatal("the damaged content is not in retention")
			}
			if fileHash(t, h.dstPath("movies/q.mkv")) != strHash(content("p", 3000)) {
				t.Fatal("q was not repaired")
			}
		})
	}
}

// TestLinkManifest: every sync that completes writes .bunkarr/links.tsv, the hardlinked names and
// their primaries, so a restore without Bunkarr's database can recreate the names that have no
// file of their own; a dry run does not write it and an unchanged manifest is not rewritten.
func TestLinkManifest(t *testing.T) {
	read := func(h *harness) []string {
		h.t.Helper()
		b, err := os.ReadFile(h.dstPath(LinkManifestRel))
		if err != nil {
			h.t.Fatal(err)
		}
		var lines []string
		for _, l := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
			if !strings.HasPrefix(l, "#") {
				lines = append(lines, l)
			}
		}
		return lines
	}
	for _, tc := range []struct {
		mode  destinations.HardlinkMode
		state string
	}{{destinations.HardlinksCopy, "link_recorded"}, {destinations.HardlinksRecreate, "linked"}} {
		t.Run(string(tc.mode), func(t *testing.T) {
			h := newHarness(t)
			h.setSettings(func(s *destinations.Settings) { s.Hardlinks = tc.mode })
			h.writeSrc("k.mkv", content("k", 500), 1)
			h.writeSrc("d/p1.mkv", content("p", 1800), 2)
			h.linkSrc("d/p1.mkv", "e/p2\tx.mkv") // a tab in a name is escaped
			h.mustSync(true, jobs.Params{})
			if exists(h.dstPath(LinkManifestRel)) {
				t.Fatal("a dry run wrote the manifest")
			}
			h.firstSync()
			want := "movies/e/p2\\tx.mkv\tmovies/d/p1.mkv\t" + tc.state
			if got := read(h); len(got) != 1 || got[0] != want {
				t.Fatalf("manifest %q, want [%q]", got, want)
			}
			ino := inode(t, h.dstPath(LinkManifestRel))
			if _, st, _ := h.mustSync(false, jobs.Params{}); st.FilesPlanned != 0 || inode(t, h.dstPath(LinkManifestRel)) != ino {
				t.Fatal("an up-to-date sync rewrote the manifest")
			}
			// The primary is deleted at the source: the link takes over the content.
			h.removeSrc("d/p1.mkv")
			h.firstSync()
			if got := read(h); len(got) != 0 {
				t.Fatalf("manifest %q, want no links", got)
			}
			for rel := range regularFiles(t, h.dstPath(filecopy.MetaDir)) {
				if filecopy.IsTempName(filepath.Base(rel)) {
					t.Fatalf("temp file left: %s", rel)
				}
			}
		})
	}
}

// TestRetainWaitsForTheNewVersionInItsFolder (S6): a Radarr upgrade replaces X-1080p.mkv with
// X-2160p.mkv in the same folder, and the new file cannot be backed up (unreadable, or a name the
// destination cannot store). The old version is not sent to retention while the new one is not
// backed up: it stays live and recorded, the retain fails with a warning, and a vanished file of
// another folder is retained as usual. Once the new file is backed up, the old one is retained.
func TestRetainWaitsForTheNewVersionInItsFolder(t *testing.T) {
	for _, tc := range []struct {
		name, newName string
		block, fix    func(h *harness)
	}{
		{
			name: "copy fails", newName: "X/X-2160p.mkv",
			block: func(h *harness) {
				if os.Geteuid() == 0 {
					h.t.Skip("root reads unreadable files")
				}
				if err := os.Chmod(h.srcPath("X/X-2160p.mkv"), 0); err != nil {
					h.t.Fatal(err)
				}
			},
			fix: func(h *harness) {
				if err := os.Chmod(h.srcPath("X/X-2160p.mkv"), 0o644); err != nil {
					h.t.Fatal(err)
				}
			},
		},
		{
			name: "name not storable", newName: "X/X:2160p.mkv",
			block: func(h *harness) { h.setCaps(func(c *filecopy.Capabilities) { c.InvalidChars = ":" }) },
			fix:   func(h *harness) { h.setCaps(func(c *filecopy.Capabilities) { c.InvalidChars = "" }) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.writeSrc("k.mkv", content("k", 500), 1)
			h.writeSrc("X/X-1080p.mkv", content("x", 1800), 2)
			h.writeSrc("Y/Y.mkv", content("y", 900), 3)
			h.firstSync()
			h.removeSrc("X/X-1080p.mkv")
			h.writeSrc(tc.newName, content("X", 2400), 4)
			h.removeSrc("Y/Y.mkv")
			tc.block(h)
			res, st, j := h.mustSync(false, jobs.Params{})
			if st.FilesRetained != 1 || st.FilesFailed != 2 || res.Warnings == 0 {
				t.Fatalf("sync: %+v items %+v\n%s", st, h.items(j.ID), h.rep.dump())
			}
			if got := itemsBy(h.items(j.ID), jobs.ActionRetain, jobs.ItemFailed); len(got) != 1 || got[0] != "movies/X/X-1080p.mkv" {
				t.Fatalf("failed retains %v", got)
			}
			if r, ok := h.liveRecord("movies/X/X-1080p.mkv"); !ok || r.State != StatePresent ||
				fileHash(t, h.dstPath("movies/X/X-1080p.mkv")) != strHash(content("x", 1800)) {
				t.Fatalf("the old version left the live tree: %+v", r)
			}
			if len(h.retainedOf("movies/Y/Y.mkv")) != 1 {
				t.Fatal("the vanished file of another folder is retained")
			}
			tc.fix(h)
			if _, st, j := h.mustSync(false, jobs.Params{}); st.FilesCopied != 1 || st.FilesRetained != 1 || st.FilesFailed != 0 {
				t.Fatalf("after the fix: %+v items %+v", st, h.items(j.ID))
			}
			h.assertConverged(t)
			if len(h.retainedOf("movies/X/X-1080p.mkv")) != 1 {
				t.Fatal("the old version is retained once the new one is backed up")
			}
		})
	}
}

// TestRetainSettledByAnotherJobIsRecordedOnce: a sync crashes after a retain's rename; before it
// resumes, a verify settles the rename, but the file cannot go back (its folder is read-only), so it
// is recorded where it is. The resumed retain then finds its rename done and already recorded: the
// file has one retained record, and the vanished name's live record is gone.
func TestRetainSettledByAnotherJobIsRecordedOnce(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	h := newHarness(t)
	h.writeSrc("k.mkv", content("k", 500), 1)
	h.writeSrc("d/a.mkv", content("a", 1800), 4)
	h.writeSrc("d/b.mkv", content("b", 700), 5)
	h.firstSync()
	h.removeSrc("d/a.mkv")
	j := h.newJob(jobs.TypeSync, false, jobs.Params{})
	if crashed, _, err := runWithHook(h, j, faultinject.CrashAt(PointRetainAfterRename, 1)); !crashed {
		t.Fatalf("no crash: %v", err)
	}
	dir := h.dstPath("movies/d")
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	h.mustVerify()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if kept := h.retainedOf("movies/d/a.mkv"); len(kept) != 1 {
		t.Fatalf("after the verify: retained %+v", kept)
	}
	j.Attempt, j.Trigger = 2, jobs.TriggerResume
	if _, err := h.run(h.sync, j); err != nil {
		t.Fatalf("resume: %v\n%s", err, h.rep.dump())
	}
	if kept := h.retainedOf("movies/d/a.mkv"); len(kept) != 1 || fileHash(t, h.dstPath(kept[0].RetainedPath)) != strHash(content("a", 1800)) {
		t.Fatalf("retained %+v", kept)
	}
	if r, ok := h.liveRecord("movies/d/a.mkv"); ok {
		t.Fatalf("live record left: %+v", r)
	}
	h.assertConverged(t)
	if _, st, _ := h.mustSync(false, jobs.Params{}); st.FilesPlanned != 0 {
		t.Fatalf("not converged: %+v", st)
	}
}

// TestDamagedHardlinkIsCheckedAfterItsPrimaryIsRepaired: p and its destination hardlink q share a
// damaged inode; verify sampled only p (missing), q stays linked. The next sync repairs p in the
// copy phase, before q's own item runs: q was deleted at the source (its retain) or replaced there
// (its update). q's file is still the damaged inode although p is present again: its retained
// version says damaged and carries no hash.
func TestDamagedHardlinkIsCheckedAfterItsPrimaryIsRepaired(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(h *harness)
	}{
		{"deleted at the source", func(h *harness) { h.removeSrc("q.mkv") }},
		{"replaced at the source", func(h *harness) {
			h.removeSrc("q.mkv")
			h.writeSrc("q.mkv", content("Q", 3100), 9)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			damaged := h.damageSharedInode(t)
			tc.change(h)
			if _, st, j := h.mustSync(false, jobs.Params{}); st.FilesFailed != 0 {
				t.Fatalf("sync: %+v items %+v", st, h.items(j.ID))
			}
			h.assertConverged(t)
			kept := h.retainedOf("movies/q.mkv")
			if len(kept) != 1 || fileHash(t, h.dstPath(kept[0].RetainedPath)) != damaged {
				t.Fatalf("retained q: %+v", kept)
			}
			if kept[0].Reason != ReasonDamaged || kept[0].Hash != "" {
				t.Errorf("q's damaged content retained as %q with hash %q", kept[0].Reason, kept[0].Hash)
			}
		})
	}
}

// TestReplacementCommittedByAStoppedJobIsFinished: an update renamed its new version into place
// (the old one renamed, or hardlinked, into retention) and the process died before the record was
// written; the resume stopped before reaching the item. The next job of the destination finishes
// the replacement from the item's detail: the live record describes the new version, the old one
// is retained once (replaced), no copy of the new version goes to retention, and nothing is left
// to plan.
func TestReplacementCommittedByAStoppedJobIsFinished(t *testing.T) {
	for _, tc := range []struct {
		name  string
		links bool
		point string
	}{
		{"renamed", false, PointUpdateAfterRenameNew},
		{"hardlinked", true, PointRecordAfterFS},
	} {
		for _, size := range []int{1800, 1900} {
			for _, how := range []string{"unmounted", "cancelled"} {
				for _, next := range []string{"sync", "verify"} {
					t.Run(fmt.Sprintf("%s/size%d/%s/%s", tc.name, size, how, next), func(t *testing.T) {
						h := newHarness(t)
						h.setCaps(func(c *filecopy.Capabilities) { c.Hardlinks = tc.links })
						h.writeSrc("k.mkv", content("k", 500), 1)
						h.writeSrc("a.mkv", content("a", 1800), 2)
						h.firstSync()
						h.writeSrc("a.mkv", content("b", size), 3)
						h.crashThenStop(t, tc.point, how)
						if next == "verify" {
							if res, st, _ := h.mustVerify(); res.Warnings != 0 || st.FilesMissing != 0 {
								t.Fatalf("verify: %+v %+v\n%s", res, st, h.rep.dump())
							}
						}
						if _, st, j := h.mustSync(false, jobs.Params{}); st.FilesPlanned != 0 {
							t.Fatalf("next sync: %+v items %+v\n%s", st, h.items(j.ID), h.rep.dump())
						}
						h.assertConverged(t)
						newHash := strHash(content("b", size))
						if r, ok := h.liveRecord("movies/a.mkv"); !ok || r.State != StatePresent || r.Size != int64(size) ||
							r.RetainedPath != "" || fileHash(t, h.dstPath("movies/a.mkv")) != newHash {
							t.Fatalf("live a.mkv: %+v", r)
						}
						kept := h.retainedOf("movies/a.mkv")
						if len(kept) != 1 || kept[0].Reason != ReasonReplaced ||
							fileHash(t, h.dstPath(kept[0].RetainedPath)) != strHash(content("a", 1800)) {
							t.Fatalf("retained %+v", kept)
						}
						for rel, got := range regularFiles(t, h.dstPath(filecopy.RetentionRoot)) {
							if got == newHash {
								t.Errorf("the new version is in retention too: %s", rel)
							}
						}
					})
				}
			}
		}
	}
}

// TestAFileInTheWayOfAStoppedReplacementIsNotTrusted: an update renamed the old version into
// retention (no hardlinks) and the job stopped before the new version was in place; then another
// file with the old version's size appeared at the path. The next job cannot move the old version
// back, so it records it in retention, and the live record no longer vouches for what is at its
// path (missing): the next sync keeps that file as damaged, without the old version's hash, and
// copies the source again.
func TestAFileInTheWayOfAStoppedReplacementIsNotTrusted(t *testing.T) {
	for _, next := range []string{"sync", "verify"} {
		t.Run(next, func(t *testing.T) {
			h := newHarness(t)
			h.setCaps(func(c *filecopy.Capabilities) { c.Hardlinks = false })
			h.writeSrc("k.mkv", content("k", 500), 1)
			h.writeSrc("a.mkv", content("a", 1800), 2)
			h.firstSync()
			h.writeSrc("a.mkv", content("b", 1900), 3)
			h.crashThenStop(t, PointUpdateAfterRenameOld, "unmounted")
			writeFileAt(t, h.dstPath("movies/a.mkv"), content("j", 1800), baseTime)
			if next == "verify" {
				h.mustVerify()
			}
			if _, st, j := h.mustSync(false, jobs.Params{}); st.FilesFailed != 0 {
				t.Fatalf("next sync: %+v items %+v\n%s", st, h.items(j.ID), h.rep.dump())
			}
			h.assertConverged(t)
			got := map[string]Record{}
			for _, r := range h.retainedOf("movies/a.mkv") {
				got[fileHash(t, h.dstPath(r.RetainedPath))] = r
			}
			if r, ok := got[strHash(content("a", 1800))]; !ok || r.Reason != ReasonReplaced {
				t.Errorf("the old version: %+v", r)
			}
			if r, ok := got[strHash(content("j", 1800))]; !ok || r.Reason != ReasonDamaged || r.Hash != "" {
				t.Errorf("the file that was in the way: %+v", r)
			}
			if fileHash(t, h.dstPath("movies/a.mkv")) != strHash(content("b", 1900)) {
				t.Error("a.mkv is not the new version")
			}
		})
	}
}

// TestADisplacedFileOfADeletedSourceIsRecorded: a sync displaced an unmanaged file into retention
// and crashed before recording it; its resume stopped, and the source was then deleted (a stopped
// job does not keep it). The next jobs record the displaced file once, as an orphan (no source),
// without a warning.
func TestADisplacedFileOfADeletedSourceIsRecorded(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("k.mkv", content("k", 500), 1)
	h.firstSync()
	h.writeSrc("x.mkv", content("x", 1200), 2)
	writeFileAt(t, h.dstPath("movies/x.mkv"), "junk-x", baseTime)
	h.crashThenStop(t, PointDisplaceAfterRename, "unmounted")
	if err := h.cat.Delete(h.ctx, h.src.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if res, _, _ := h.mustVerify(); res.Warnings != 0 {
			t.Fatalf("verify %d: %+v\n%s", i, res, h.rep.dump())
		}
	}
	var displaced []Record
	for _, r := range h.records() {
		if r.State == StateRetained && r.Reason == ReasonDisplaced {
			displaced = append(displaced, r)
		}
	}
	if len(displaced) != 1 || displaced[0].SourceID != 0 || fileHash(t, h.dstPath(displaced[0].RetainedPath)) != strHash("junk-x") {
		t.Fatalf("displaced records %+v", displaced)
	}
	retained := map[string]bool{}
	for _, r := range h.records() {
		retained[r.RetainedPath] = r.State == StateRetained
	}
	for rel := range regularFiles(t, h.dstPath(filecopy.RetentionRoot)) {
		if !retained[filecopy.RetentionRoot+"/"+rel] {
			t.Errorf("unrecorded file in retention: %s", rel)
		}
	}
}

// TestLinkManifestEscapesALeadingHash: a destination folder may start with '#', like the manifest's
// comment lines; every path that starts with '#' is written as \#, so a reader that skips comment
// lines still finds every link.
func TestLinkManifestEscapesALeadingHash(t *testing.T) {
	h := newHarness(t)
	in := sourceInput(h, nil)
	in.DestFolder = "#movies"
	src, err := h.cat.Update(h.ctx, h.src.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	h.src = src
	h.writeSrc("k.mkv", content("k", 500), 1)
	h.writeSrc("p1.mkv", content("p", 1800), 2)
	h.linkSrc("p1.mkv", "p2.mkv")
	h.firstSync()
	b, err := os.ReadFile(h.dstPath(LinkManifestRel))
	if err != nil {
		t.Fatal(err)
	}
	var data []string
	for _, l := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		if !strings.HasPrefix(l, "#") {
			data = append(data, l)
		}
	}
	if want := `\#movies/p2.mkv` + "\t" + `\#movies/p1.mkv` + "\tlinked"; len(data) != 1 || data[0] != want {
		t.Fatalf("manifest entries %q, want [%q]\n%s", data, want, b)
	}
}
