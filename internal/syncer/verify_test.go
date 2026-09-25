package syncer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

func (h *harness) mustVerify() (jobs.Result, VerifyStats, jobs.Job) {
	h.t.Helper()
	j := h.newJob(jobs.TypeVerify, false, jobs.Params{})
	res, err := h.run(h.verify, j)
	if err != nil {
		h.t.Fatalf("verify: %v\n%s", err, h.rep.dump())
	}
	return res, res.Stats.(VerifyStats), j
}

func TestVerifyCatchesDamagedFiles(t *testing.T) {
	h := newHarness(t)
	h.setSettings(func(s *destinations.Settings) { s.Verify.Mode = destinations.VerifyFull })
	h.writeSrc("ok.mkv", content("o", 4000), 1)
	h.writeSrc("truncated.mkv", content("t", 4000), 2)
	h.writeSrc("corrupt.mkv", content("c", 4000), 3)
	h.writeSrc("gone.mkv", content("g", 4000), 4)
	h.writeSrc("pair1.mkv", content("p", 4000), 5)
	h.linkSrc("pair1.mkv", "pair2.mkv")
	h.mustSync(false, jobs.Params{})

	res, st, _ := h.mustVerify()
	if st.FilesVerified != 6 || st.FilesMissing != 0 || res.Warnings != 0 {
		t.Fatalf("clean verify: %+v", st)
	}
	// Damage the destination: a truncated file, a flipped byte (same size and mtime), a deleted
	// file.
	keepMtime := func(p string, fn func()) {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		fn()
		if err := os.Chtimes(p, fi.ModTime(), fi.ModTime()); err != nil {
			t.Fatal(err)
		}
	}
	trunc := h.dstPath("movies/truncated.mkv")
	keepMtime(trunc, func() {
		if err := os.Truncate(trunc, 1000); err != nil {
			t.Fatal(err)
		}
	})
	corrupt := h.dstPath("movies/corrupt.mkv")
	keepMtime(corrupt, func() {
		f, err := os.OpenFile(corrupt, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteAt([]byte("X"), 2000); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	})
	if err := os.Remove(h.dstPath("movies/gone.mkv")); err != nil {
		t.Fatal(err)
	}

	res, st, j := h.mustVerify()
	if st.FilesMissing != 3 || res.Warnings != 3 {
		t.Fatalf("verify after damage: %+v warnings %d items %+v", st, res.Warnings, h.items(j.ID))
	}
	for _, rel := range []string{"movies/truncated.mkv", "movies/corrupt.mkv", "movies/gone.mkv"} {
		if r, _ := h.liveRecord(rel); r.State != StateMissing {
			t.Errorf("%s: state %s, want missing", rel, r.State)
		}
	}
	if r, _ := h.liveRecord("movies/ok.mkv"); r.State != StatePresent || r.VerifiedAt == nil {
		t.Errorf("ok.mkv: %+v", r)
	}
	if !strings.Contains(res.Summary, "3 missing or damaged") {
		t.Errorf("summary %q", res.Summary)
	}

	// The next sync copies them again; the damaged versions go to retention, never replaced.
	_, sst, sj := h.mustSync(false, jobs.Params{})
	if sst.FilesCopied != 3 || sst.FilesFailed != 0 {
		t.Fatalf("repair sync: %+v items %+v", sst, h.items(sj.ID))
	}
	h.assertConverged(t)
	damaged := 0
	for _, r := range h.records() {
		if r.State == StateRetained && r.Reason == ReasonDamaged {
			damaged++
		}
	}
	if damaged != 2 {
		t.Errorf("%d damaged versions retained, want 2 (the deleted one has no file)", damaged)
	}
	if _, st, _ := h.mustVerify(); st.FilesMissing != 0 {
		t.Fatalf("verify after repair: %+v", st)
	}
}

func TestDamagedHardlinkedPrimaryIsRepairedWithItsLinks(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("p.mkv", content("p", 3000), 1)
	h.linkSrc("p.mkv", "q.mkv")
	h.writeSrc("other.mkv", content("o", 10), 2)
	h.mustSync(false, jobs.Params{})
	if inode(t, h.dstPath("movies/p.mkv")) != inode(t, h.dstPath("movies/q.mkv")) {
		t.Fatal("setup: not linked")
	}
	// The shared inode is damaged; verify sampled only the primary (the sample is least recently
	// verified first, so mark the others verified).
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
	_, st, j := h.mustSync(false, jobs.Params{})
	if st.FilesCopied != 1 || st.FilesLinked != 1 || st.FilesFailed != 0 {
		t.Fatalf("repair: %+v items %+v", st, h.items(j.ID))
	}
	h.assertConverged(t)
	if fileHash(t, h.dstPath("movies/q.mkv")) != strHash(content("p", 3000)) {
		t.Fatal("the hardlink still has the damaged content")
	}
}

// TestRelinkAfterARepairKeepsAnIntactVersionAsReplaced: the primary's file was deleted at the
// destination; its hardlink still holds the intact inode. After the repair the hardlink is relinked
// and its old version kept in retention as a replaced version with its hash, not as damaged.
func TestRelinkAfterARepairKeepsAnIntactVersionAsReplaced(t *testing.T) {
	h := newHarness(t)
	h.setSettings(func(s *destinations.Settings) { s.Verify.Mode = destinations.VerifyFull })
	h.writeSrc("p.mkv", content("p", 3000), 1)
	h.linkSrc("p.mkv", "q.mkv")
	h.writeSrc("other.mkv", content("o", 10), 2)
	h.mustSync(false, jobs.Params{})
	if err := os.Remove(h.dstPath("movies/p.mkv")); err != nil {
		t.Fatal(err)
	}
	if _, st, _ := h.mustVerify(); st.FilesMissing != 1 {
		t.Fatalf("verify: %+v", st)
	}
	if r, _ := h.liveRecord("movies/q.mkv"); r.State != StateLinked {
		t.Fatalf("setup: q is %s", r.State)
	}
	if _, st, j := h.mustSync(false, jobs.Params{}); st.FilesFailed != 0 || st.FilesLinked != 1 {
		t.Fatalf("repair: %+v items %+v", st, h.items(j.ID))
	}
	h.assertConverged(t)
	var kept []Record
	for _, r := range h.records() {
		if r.State == StateRetained {
			kept = append(kept, r)
		}
	}
	if len(kept) != 1 || kept[0].RelPath != "movies/q.mkv" || kept[0].Reason != ReasonReplaced ||
		kept[0].Hash != filecopy.HashPrefix+strHash(content("p", 3000)) {
		t.Fatalf("retained %+v", kept)
	}
}

func TestDamagedHardlinkMarksItsPrimary(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("p.mkv", content("p", 3000), 1)
	h.linkSrc("p.mkv", "q.mkv")
	h.mustSync(false, jobs.Params{})
	// Same-size damage of the shared inode, and only the hardlink is in the sample.
	f, err := os.OpenFile(h.dstPath("movies/q.mkv"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("XX"), 10); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	h.setSettings(func(s *destinations.Settings) { s.Verify.Mode, s.Verify.SamplePercent = destinations.VerifySample, 1 })
	h.dbExec(`UPDATE destination_files SET verified_at = ? WHERE rel_path = 'movies/p.mkv'`, "2026-01-01T00:00:00.000000000Z")
	h.dbExec(`UPDATE destination_files SET verified_at = NULL WHERE rel_path = 'movies/q.mkv'`)
	res, st, _ := h.mustVerify()
	if st.FilesMissing != 1 || !h.rep.has("shares the damaged content") {
		t.Fatalf("verify: %+v %s", st, res.Summary)
	}
	for _, rel := range []string{"movies/p.mkv", "movies/q.mkv"} {
		if r, _ := h.liveRecord(rel); r.State != StateMissing {
			t.Fatalf("%s is %s", rel, r.State)
		}
	}
	_, sst, j := h.mustSync(false, jobs.Params{})
	if sst.FilesFailed != 0 {
		t.Fatalf("repair: %+v items %+v", sst, h.items(j.ID))
	}
	h.assertConverged(t)
	if inode(t, h.dstPath("movies/p.mkv")) != inode(t, h.dstPath("movies/q.mkv")) {
		t.Error("the repaired names are hardlinks again")
	}
}

func TestVerifyRecordsHashesAndSamples(t *testing.T) {
	h := newHarness(t)
	h.setSettings(func(s *destinations.Settings) { s.Verify.Mode = destinations.VerifyOff })
	for i := range 40 {
		h.writeSrc(fmt.Sprintf("f%02d.mkv", i), content(fmt.Sprint(i), 100+i), i)
	}
	h.mustSync(false, jobs.Params{})
	for _, r := range h.records() {
		if r.Hash != "" {
			t.Fatalf("%s hashed with verify off", r.RelPath)
		}
	}
	// Mode off: sizes are checked, nothing is re-read.
	_, st, _ := h.mustVerify()
	if st.FilesChecked != 40 || st.FilesPlanned != 0 {
		t.Fatalf("off: %+v", st)
	}
	// 10 %: 4 files per run, the least recently verified first, until every file has a hash.
	h.setSettings(func(s *destinations.Settings) {
		s.Verify.Mode, s.Verify.SamplePercent = destinations.VerifySample, 10
	})
	seen := map[string]bool{}
	for range 10 {
		h.advance(time.Second)
		_, st, j := h.mustVerify()
		if st.FilesVerified != 4 || st.HashesRecorded != 4 {
			t.Fatalf("sample: %+v", st)
		}
		for _, it := range h.items(j.ID) {
			if seen[it.RelPath] {
				t.Fatalf("%s sampled twice before every file was", it.RelPath)
			}
			seen[it.RelPath] = true
		}
	}
	for _, r := range h.records() {
		if !strings.HasPrefix(r.Hash, filecopy.HashPrefix) || r.VerifiedAt == nil {
			t.Fatalf("%s: hash %q verified %v", r.RelPath, r.Hash, r.VerifiedAt)
		}
	}
}

func TestVerifyResumesAfterCrash(t *testing.T) {
	for _, point := range []string{PointVerifyAfterMark, PointPlanAfterBatch} {
		t.Run(point, func(t *testing.T) {
			h := newHarness(t)
			h.setSettings(func(s *destinations.Settings) { s.Verify.Mode = destinations.VerifyFull })
			for i := range 6 {
				h.writeSrc(fmt.Sprintf("f%d.mkv", i), content(fmt.Sprint(i), 100+i), i)
			}
			h.mustSync(false, jobs.Params{})
			if err := os.Truncate(h.dstPath("movies/f3.mkv"), 5); err != nil {
				t.Fatal(err)
			}
			j := h.newJob(jobs.TypeVerify, false, jobs.Params{})
			func() {
				faultinject.SetHook(faultinject.CrashAt(point, 1))
				defer faultinject.SetHook(nil)
				defer func() {
					if p := recover(); p == nil {
						t.Fatal("no crash")
					} else if _, ok := p.(faultinject.Crash); !ok {
						panic(p)
					}
				}()
				_, _ = h.verify.Run(h.ctx, j, h.env())
			}()
			j.Attempt, j.Trigger = 2, jobs.TriggerResume
			res, err := h.run(h.verify, j)
			if err != nil {
				t.Fatal(err)
			}
			st := res.Stats.(VerifyStats)
			if st.FilesMissing != 1 || st.FilesVerified != 5 || res.Warnings != 1 {
				t.Fatalf("%+v items %+v", st, h.items(j.ID))
			}
			if r, _ := h.liveRecord("movies/f3.mkv"); r.State != StateMissing {
				t.Fatalf("state %s", r.State)
			}
		})
	}
}

func TestVerifyNeedsTheMarker(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("a.mkv", "a", 1)
	h.mustSync(false, jobs.Params{})
	if err := os.Remove(h.dstPath(filecopy.MarkerRel)); err != nil {
		t.Fatal(err)
	}
	j := h.newJob(jobs.TypeVerify, false, jobs.Params{})
	if _, err := h.run(h.verify, j); !errors.Is(err, destinations.ErrNotMounted) {
		t.Fatalf("err = %v", err)
	}
	if r, _ := h.liveRecord("movies/a.mkv"); r.State != StatePresent {
		t.Fatal("a verify of an unmounted destination must not mark files missing")
	}
}

// TestRecordedLinkOfAMissingPrimaryIsRepaired: on a destination that records hardlinks (no file of
// their own), verify finds the primary gone. The next sync copies it again although a
// recorded-only link still points at it, and when the primary's source was replaced meanwhile
// (a group split), the link gets its own copy of the content it recorded.
func TestRecordedLinkOfAMissingPrimaryIsRepaired(t *testing.T) {
	for _, split := range []bool{false, true} {
		t.Run(fmt.Sprintf("split=%v", split), func(t *testing.T) {
			h := newHarness(t)
			h.setSettings(func(s *destinations.Settings) {
				s.Hardlinks, s.Verify.Mode = destinations.HardlinksCopy, destinations.VerifyFull
			})
			h.writeSrc("p.mkv", content("p", 3000), 1)
			h.linkSrc("p.mkv", "q.mkv")
			h.writeSrc("other.mkv", content("o", 10), 2)
			h.mustSync(false, jobs.Params{})
			if r, _ := h.liveRecord("movies/q.mkv"); r.State != StateLinkRecorded {
				t.Fatalf("setup: q is %s", r.State)
			}
			if err := os.Remove(h.dstPath("movies/p.mkv")); err != nil {
				t.Fatal(err)
			}
			if _, st, _ := h.mustVerify(); st.FilesMissing != 1 {
				t.Fatalf("verify: %+v", st)
			}
			if split {
				h.writeSrc("p.mkv", content("P", 3100), 3) // a new file: q keeps the old content
			}
			_, st, j := h.mustSync(false, jobs.Params{})
			if st.FilesFailed != 0 {
				t.Fatalf("repair: %+v items %+v", st, h.items(j.ID))
			}
			h.assertConverged(t)
			if !h.hasContent(t, content("p", 3000)) {
				t.Fatal("the recorded content is not at the destination")
			}
			if _, st, _ := h.mustSync(false, jobs.Params{}); st.FilesPlanned != 0 {
				t.Fatalf("not converged: %+v", st)
			}
		})
	}
}

// TestRecordedLinkOfAMissingPrimaryKeepsItsContent: the primary's source was replaced (a group
// split) after verify found its file gone, so the content a recorded-only link recorded is only at
// the source; the link's own copy comes before the primary's repair. When that copy does not happen
// (the link's source cannot be read, the job stops after one repair), the primary is not repaired
// either: the link is never left recorded against a primary that holds other content.
func TestRecordedLinkOfAMissingPrimaryKeepsItsContent(t *testing.T) {
	setup := func(t *testing.T) *harness {
		h := newHarness(t)
		h.setSettings(func(s *destinations.Settings) {
			s.Hardlinks, s.Verify.Mode = destinations.HardlinksCopy, destinations.VerifyFull
		})
		h.writeSrc("p.mkv", content("p", 3000), 1)
		h.linkSrc("p.mkv", "q.mkv")
		h.writeSrc("other.mkv", content("o", 10), 2)
		h.mustSync(false, jobs.Params{})
		if err := os.Remove(h.dstPath("movies/p.mkv")); err != nil {
			t.Fatal(err)
		}
		if _, st, _ := h.mustVerify(); st.FilesMissing != 1 {
			t.Fatalf("verify: %+v", st)
		}
		h.writeSrc("p.mkv", content("P", 3100), 3) // a new file: q keeps the old content
		return h
	}
	// noSplitRecord fails when q is recorded as a link of a present p that holds other content.
	noSplitRecord := func(t *testing.T, h *harness, j jobs.Job) {
		t.Helper()
		q, _ := h.liveRecord("movies/q.mkv")
		p, _ := h.liveRecord("movies/p.mkv")
		if q.State == StateLinkRecorded && q.LinkOf == p.ID && p.State == StatePresent && p.Size != q.Size {
			t.Fatalf("q is recorded as a link of p, which holds other content: items %+v", h.items(j.ID))
		}
	}
	converges := func(t *testing.T, h *harness) {
		t.Helper()
		if _, st, j := h.mustSync(false, jobs.Params{}); st.FilesFailed != 0 {
			t.Fatalf("next sync: %+v items %+v", st, h.items(j.ID))
		}
		h.assertConverged(t)
		if !h.hasContent(t, content("p", 3000)) {
			t.Fatal("the content q recorded is not at the destination")
		}
	}
	t.Run("the link's copy fails", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads unreadable files")
		}
		h := setup(t)
		if err := os.Chmod(h.srcPath("q.mkv"), 0); err != nil {
			t.Fatal(err)
		}
		_, st, j := h.mustSync(false, jobs.Params{})
		if st.FilesFailed == 0 {
			t.Fatalf("the copy of an unreadable file must fail: %+v items %+v", st, h.items(j.ID))
		}
		noSplitRecord(t, h, j)
		if err := os.Chmod(h.srcPath("q.mkv"), 0o644); err != nil {
			t.Fatal(err)
		}
		converges(t, h)
	})
	t.Run("the job stops after one repair", func(t *testing.T) {
		h := setup(t)
		ctx, cancel := context.WithCancel(h.ctx)
		defer cancel()
		faultinject.SetHook(func(name string) {
			if name == PointRecordAfterDB {
				cancel()
			}
		})
		j := h.newJob(jobs.TypeSync, false, jobs.Params{})
		_, err := h.sync.Run(ctx, j, h.env())
		faultinject.SetHook(nil)
		h.closeJob(j.ID)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
		noSplitRecord(t, h, j)
		converges(t, h)
	})
}

// TestVerifyResumeMarksTheSharedPrimary: a verify attempt that stopped after marking a damaged
// hardlink but before marking the primary that shares its inode marks the primary on resume.
func TestVerifyResumeMarksTheSharedPrimary(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("p.mkv", content("p", 3000), 1)
	h.linkSrc("p.mkv", "q.mkv")
	h.mustSync(false, jobs.Params{})
	f, err := os.OpenFile(h.dstPath("movies/q.mkv"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("XX"), 10); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	h.setSettings(func(s *destinations.Settings) { s.Verify.Mode, s.Verify.SamplePercent = destinations.VerifySample, 1 })
	h.dbExec(`UPDATE destination_files SET verified_at = ? WHERE rel_path = 'movies/p.mkv'`, "2026-01-01T00:00:00.000000000Z")
	h.dbExec(`UPDATE destination_files SET verified_at = NULL WHERE rel_path = 'movies/q.mkv'`)
	j := h.newJob(jobs.TypeVerify, false, jobs.Params{})
	func() {
		faultinject.SetHook(faultinject.CrashAt(PointVerifyAfterMark, 1))
		defer faultinject.SetHook(nil)
		defer func() {
			if p := recover(); p == nil {
				t.Fatal("no crash")
			} else if _, ok := p.(faultinject.Crash); !ok {
				panic(p)
			}
		}()
		_, _ = h.verify.Run(h.ctx, j, h.env())
	}()
	// The state a crash between the two marks leaves: the hardlink marked, its primary not.
	h.dbExec(`UPDATE destination_files SET state = 'present' WHERE rel_path = 'movies/p.mkv'`)
	j.Attempt, j.Trigger = 2, jobs.TriggerResume
	if _, err := h.run(h.verify, j); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"movies/p.mkv", "movies/q.mkv"} {
		if r, _ := h.liveRecord(rel); r.State != StateMissing {
			t.Fatalf("%s is %s", rel, r.State)
		}
	}
}

// TestVerifyCountsUncheckableFilesAsFailed: a file verify cannot read (here: its directory became a
// symlink out of the destination after planning) is neither marked missing nor counted as
// missing or damaged: it could not be checked.
func TestVerifyCountsUncheckableFilesAsFailed(t *testing.T) {
	h := newHarness(t)
	h.setSettings(func(s *destinations.Settings) { s.Verify.Mode = destinations.VerifyFull })
	for i, rel := range []string{"x/a.mkv", "x/b.mkv", "x/c.mkv", "y.mkv"} {
		h.writeSrc(rel, content(rel, 100+i), i)
	}
	h.mustSync(false, jobs.Params{})
	outside := resolvedTempDir(t)
	moved := false
	faultinject.SetHook(func(name string) {
		// The first batch (the three files of x/) is planned.
		if name == PointPlanAfterBatch && !moved {
			moved = true
			if err := os.Rename(h.dstPath("movies/x"), filepath.Join(outside, "x")); err != nil {
				t.Error(err)
			}
			if err := os.Symlink(filepath.Join(outside, "x"), h.dstPath("movies/x")); err != nil {
				t.Error(err)
			}
		}
	})
	defer faultinject.SetHook(nil)
	res, st, j := h.mustVerify()
	faultinject.SetHook(nil)
	if !moved || st.FilesFailed != 3 || st.FilesMissing != 0 || st.FilesVerified != 1 || res.Warnings != 3 {
		t.Fatalf("stats %+v warnings %d items %+v", st, res.Warnings, h.items(j.ID))
	}
	if !strings.Contains(res.Summary, "3 could not be checked") || strings.Contains(res.Summary, "missing") {
		t.Errorf("summary %q", res.Summary)
	}
	for _, rel := range []string{"movies/x/a.mkv", "movies/x/b.mkv", "movies/x/c.mkv"} {
		if r, _ := h.liveRecord(rel); r.State != StatePresent {
			t.Errorf("%s: %s, want present (not checked)", rel, r.State)
		}
	}
}

// TestVerifyCountsUnreadableItemDetailsAsFailed: an item whose detail cannot be read is failed
// without marking anything: it could not be checked (it is not missing or damaged).
func TestVerifyCountsUnreadableItemDetailsAsFailed(t *testing.T) {
	h := newHarness(t)
	h.setSettings(func(s *destinations.Settings) { s.Verify.Mode = destinations.VerifyFull })
	for i, rel := range []string{"a.mkv", "b.mkv", "c.mkv"} {
		h.writeSrc(rel, content(rel, 100+i), i)
	}
	h.mustSync(false, jobs.Params{})
	writeFileAt(t, h.dstPath("movies/a.mkv"), content("A", 100), baseTime) // damaged, same size
	j := h.newJob(jobs.TypeVerify, false, jobs.Params{})
	func() {
		faultinject.SetHook(faultinject.CrashAt(PointVerifyAfterMark, 1))
		defer faultinject.SetHook(nil)
		defer func() {
			if p := recover(); p == nil {
				t.Fatal("no crash")
			} else if _, ok := p.(faultinject.Crash); !ok {
				panic(p)
			}
		}()
		_, _ = h.verify.Run(h.ctx, j, h.env())
	}()
	var target int64
	for _, it := range h.items(j.ID) {
		if it.RelPath == "movies/c.mkv" {
			target = it.ID
		}
	}
	if err := h.jq.SetDetail(h.ctx, target, []byte(`{"recordId":"not a number"}`)); err != nil {
		t.Fatal(err)
	}
	j.Attempt, j.Trigger = 2, jobs.TriggerResume
	res, err := h.run(h.verify, j)
	if err != nil {
		t.Fatal(err)
	}
	if st := res.Stats.(VerifyStats); st.FilesMissing != 1 || st.FilesFailed != 1 || st.FilesVerified != 1 || res.Warnings != 2 {
		t.Fatalf("stats %+v items %+v", st, h.items(j.ID))
	}
	if r, _ := h.liveRecord("movies/c.mkv"); r.State != StatePresent {
		t.Fatalf("c.mkv: %s", r.State)
	}
}
