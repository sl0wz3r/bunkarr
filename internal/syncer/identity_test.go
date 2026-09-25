package syncer

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// The tests of this file run on a destination that numbers inodes per lookup (harness.inventInodes),
// as a CIFS client mounted with noserverino does: Unraid's Unassigned Devices mounts shares so.
// Hardlinks work there, but the names of one file never show the same inode number.

// unstableInodes makes the harness's destination number inodes per lookup and stores what the
// probe finds there.
func (h *harness) unstableInodes() {
	h.inventInodes()
	h.setCaps(func(c *filecopy.Capabilities) { c.Hardlinks, c.UnstableInodes = true, true })
}

// TestResumeAfterLinkOldOnUnstableInodes: an update hardlinked the old version of a.mkv into
// retention and the process died before the new version was renamed into place (the docker share
// test's TestKillResume/update.afterLinkOld on SMB). The resumed item must recognize the final name
// as the old version it kept, although the two names show different inode numbers, and finish the
// replacement; it used to take its own old version for an unmanaged file and displace it. "probed":
// the destination's capabilities say so from the start; "upgraded": the crashed attempt ran with
// capabilities from an older probe on a destination whose inode numbers it trusted, and the resume
// finds the destination numbering inodes per lookup (the sync probes it again first).
func TestResumeAfterLinkOldOnUnstableInodes(t *testing.T) {
	for _, tc := range []string{"probed", "upgraded"} {
		t.Run(tc, func(t *testing.T) {
			h := newHarness(t)
			if tc == "probed" {
				h.unstableInodes()
			}
			h.writeSrc("k.mkv", content("k", 500), 1)
			h.writeSrc("a.mkv", content("a", 1800), 2)
			h.firstSync()
			h.writeSrc("a.mkv", content("b", 1900), 3)
			j := h.newJob(jobs.TypeSync, false, jobs.Params{})
			if crashed, _, err := runWithHook(h, j, faultinject.CrashAt(PointUpdateAfterLinkOld, 1)); !crashed {
				t.Fatalf("no crash at %s: %v", PointUpdateAfterLinkOld, err)
			}
			if tc == "upgraded" {
				h.inventInodes()
				h.setCaps(func(c *filecopy.Capabilities) { c.UnstableInodes, c.ProbeVersion = false, 0 })
			}

			j.Attempt, j.Trigger = 2, jobs.TriggerResume
			res, err := h.run(h.sync, j)
			if err != nil {
				t.Fatalf("resume: %v\n%s", err, h.rep.dump())
			}
			st := res.Stats.(SyncStats)
			if res.Warnings != 0 || st.FilesUpdated != 1 || st.FilesDisplaced != 0 || st.FilesFailed != 0 {
				t.Fatalf("resume: %+v %s\n%s", st, res.Summary, h.rep.dump())
			}
			if h.rep.has("displaced an unmanaged file") {
				t.Fatalf("the resume displaced a file:\n%s", h.rep.dump())
			}
			kept := h.retainedOf("movies/a.mkv")
			if len(kept) != 1 || kept[0].Reason != ReasonReplaced ||
				fileHash(t, h.dstPath(kept[0].RetainedPath)) != strHash(content("a", 1800)) {
				t.Fatalf("retained a.mkv: %+v", kept)
			}
			if r, ok := h.liveRecord("movies/a.mkv"); !ok || r.State != StatePresent ||
				fileHash(t, h.dstPath("movies/a.mkv")) != strHash(content("b", 1900)) {
				t.Fatalf("live a.mkv: %+v", r)
			}
			h.assertConverged(t)
			if _, st, j2 := h.mustSync(false, jobs.Params{}); st.FilesPlanned != 0 {
				t.Fatalf("a further sync planned %+v", h.items(j2.ID))
			}
			if d, _ := h.dests.Get(h.ctx, h.dest.ID); !d.Capabilities.Current() || !d.Capabilities.UnstableInodes || d.Capabilities.InodeIdentity() {
				t.Fatalf("capabilities after the resume: %+v", d.Capabilities)
			}
		})
	}
}

// TestSyncProbesStaleCapabilities: capabilities stored by an older probe do not say whether the
// destination's inode numbers can be trusted. A sync probes such a destination again before it
// relies on them and stores the result; a dry run writes nothing (no probe), and current
// capabilities are not probed again.
func TestSyncProbesStaleCapabilities(t *testing.T) {
	h := newHarness(t)
	h.inventInodes()
	h.setCaps(func(c *filecopy.Capabilities) { c.UnstableInodes, c.ProbeVersion = false, 0 })
	h.writeSrc("a.mkv", content("a", 300), 1)
	caps := func() filecopy.Capabilities {
		d, err := h.dests.Get(h.ctx, h.dest.ID)
		if err != nil {
			t.Fatal(err)
		}
		return d.Capabilities
	}
	stale := caps()

	h.advance(time.Second)
	h.mustSync(true, jobs.Params{})
	if got := caps(); got.Current() || !got.CheckedAt.Equal(stale.CheckedAt) {
		t.Fatalf("a dry run probed the destination: %+v", got)
	}

	h.advance(time.Second)
	if _, st, _ := h.mustSync(false, jobs.Params{}); st.FilesCopied != 1 {
		t.Fatalf("sync: %+v", st)
	}
	probed := caps()
	if !probed.Current() || !probed.UnstableInodes || !probed.Hardlinks || !probed.CheckedAt.After(stale.CheckedAt) {
		t.Fatalf("capabilities after the sync: %+v (were %+v)", probed, stale)
	}
	if !h.rep.has("probed the destination again") {
		t.Fatalf("no log of the probe:\n%s", h.rep.dump())
	}

	h.advance(time.Second)
	h.mustSync(false, jobs.Params{})
	if got := caps(); !got.CheckedAt.Equal(probed.CheckedAt) {
		t.Fatalf("current capabilities were probed again: %+v", got)
	}
}

// TestCaseOnlyRenameOnUnstableInodes: a case-only rename on a case-insensitive destination is a
// move of the file to its new spelling. The two spellings are one file, but a destination that
// numbers inodes per lookup shows two numbers for them: the move must not take the new spelling
// for an unmanaged file in the way (it used to displace the file itself and then fail).
func TestCaseOnlyRenameOnUnstableInodes(t *testing.T) {
	h := newHarness(t)
	writeFileAt(t, h.srcPath("probe"), "x", baseTime)
	if !exists(h.srcPath("PROBE")) {
		t.Skip("the test's temp directory is case-sensitive (runs on macOS)")
	}
	h.removeSrc("probe")
	h.unstableInodes()
	h.setCaps(func(c *filecopy.Capabilities) { c.CaseInsensitive = true })
	h.writeSrc("k.mkv", content("k", 500), 1)
	h.writeSrc("movie.mkv", content("m", 1300), 2)
	h.firstSync()
	h.renameSrc("movie.mkv", "Movie.mkv")

	res, st, j := h.mustSync(false, jobs.Params{})
	if res.Warnings != 0 || st.FilesMoved != 1 || st.FilesDisplaced != 0 || st.FilesFailed != 0 || st.BytesCopied != 0 {
		t.Fatalf("sync: %+v items %+v\n%s", st, h.items(j.ID), h.rep.dump())
	}
	entries, err := os.ReadDir(h.dstPath("movies"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(entries, func(e os.DirEntry) bool { return e.Name() == "Movie.mkv" }) {
		t.Fatalf("the destination lists %v, want Movie.mkv", entries)
	}
	h.assertConverged(t)
	if len(h.displaced(t)) != 0 {
		t.Fatal("the rename displaced a file")
	}
	if _, st, _ := h.mustSync(false, jobs.Params{}); st.FilesPlanned != 0 {
		t.Fatalf("a further sync planned %+v", st)
	}
}

// TestDamagedHardlinkMarksItsPrimaryOnUnstableInodes is TestDamagedHardlinkMarksItsPrimary on a
// destination that numbers inodes per lookup: verify finds the hardlink damaged, and its primary,
// one file with it, holds the same damaged content, so it is marked missing too (the inode numbers
// cannot tell that they are one file).
func TestDamagedHardlinkMarksItsPrimaryOnUnstableInodes(t *testing.T) {
	h := newHarness(t)
	h.unstableInodes()
	h.writeSrc("p.mkv", content("p", 3000), 1)
	h.linkSrc("p.mkv", "q.mkv")
	h.mustSync(false, jobs.Params{})
	if inode(t, h.dstPath("movies/p.mkv")) != inode(t, h.dstPath("movies/q.mkv")) {
		t.Fatal("setup: not linked")
	}
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
		t.Fatalf("verify: %+v %s\n%s", st, res.Summary, h.rep.dump())
	}
	for _, rel := range []string{"movies/p.mkv", "movies/q.mkv"} {
		if r, _ := h.liveRecord(rel); r.State != StateMissing {
			t.Fatalf("%s is %s", rel, r.State)
		}
	}
	if _, sst, j := h.mustSync(false, jobs.Params{}); sst.FilesFailed != 0 {
		t.Fatalf("repair: %+v items %+v", sst, h.items(j.ID))
	}
	h.assertConverged(t)
}

// TestStoppedLinkedUpdateOnUnstableInodesIsSettled: an update hardlinked the old version into
// retention, the process died, and the resumed job stopped before reaching the item. The next job
// settles the retention intent: the path still holds the recorded version (one file with the
// retained name, though the inode numbers differ), so the record keeps vouching for it and nothing
// is kept as damaged or displaced; the next sync then applies the update.
func TestStoppedLinkedUpdateOnUnstableInodesIsSettled(t *testing.T) {
	for _, how := range []string{"unmounted", "cancelled"} {
		for _, next := range []string{"sync", "verify"} {
			t.Run(how+"/"+next, func(t *testing.T) {
				h := newHarness(t)
				h.unstableInodes()
				h.writeSrc("k.mkv", content("k", 500), 1)
				h.writeSrc("a.mkv", content("a", 1800), 2)
				h.firstSync()
				h.writeSrc("a.mkv", content("b", 1900), 3)
				h.crashThenStop(t, PointUpdateAfterLinkOld, how)
				if next == "verify" {
					if res, st, _ := h.mustVerify(); res.Warnings != 0 || st.FilesMissing != 0 {
						t.Fatalf("verify: %+v %+v\n%s", res, st, h.rep.dump())
					}
				}
				res, st, j := h.mustSync(false, jobs.Params{})
				if res.Warnings != 0 || st.FilesUpdated != 1 || st.FilesDisplaced != 0 || st.FilesFailed != 0 {
					t.Fatalf("next sync: %+v items %+v\n%s", st, h.items(j.ID), h.rep.dump())
				}
				h.assertConverged(t)
				for _, r := range h.retainedOf("movies/a.mkv") {
					if r.Reason != ReasonReplaced || fileHash(t, h.dstPath(r.RetainedPath)) != strHash(content("a", 1800)) {
						t.Errorf("retained a.mkv: %+v", r)
					}
				}
				if fileHash(t, h.dstPath("movies/a.mkv")) != strHash(content("b", 1900)) {
					t.Error("a.mkv is not the new version")
				}
			})
		}
	}
}

// crashAfterLinkOld syncs a.mkv, changes it and crashes the update after the old version was
// hardlinked into retention, before the new version was renamed into place (the docker share
// test's TestKillResume/update.afterLinkOld). It returns the crashed job.
func (h *harness) crashAfterLinkOld(t *testing.T) jobs.Job {
	t.Helper()
	h.writeSrc("k.mkv", content("k", 500), 1)
	h.writeSrc("a.mkv", content("a", 1800), 2)
	h.firstSync()
	h.writeSrc("a.mkv", content("b", 1900), 3)
	j := h.newJob(jobs.TypeSync, false, jobs.Params{})
	if crashed, _, err := runWithHook(h, j, faultinject.CrashAt(PointUpdateAfterLinkOld, 1)); !crashed {
		t.Fatalf("no crash at %s: %v", PointUpdateAfterLinkOld, err)
	}
	return j
}

// settleAfterLinkOld runs the job named next after crashAfterLinkOld: the crashed sync's resume
// ("sync"), or a verify or retention job after its resume was cancelled (they settle what it left
// in retention, then a sync applies the update). The job that settles the interrupted update must
// end with wantWarnings warnings, the update must be applied, the old version kept as replaced and
// nothing displaced.
func (h *harness) settleAfterLinkOld(t *testing.T, j jobs.Job, next string, wantWarnings int) {
	t.Helper()
	switch next {
	case "sync":
		j.Attempt, j.Trigger = 2, jobs.TriggerResume
		res, err := h.run(h.sync, j)
		if err != nil {
			t.Fatalf("resume: %v\n%s", err, h.rep.dump())
		}
		st := res.Stats.(SyncStats)
		if res.Warnings != wantWarnings || st.FilesUpdated != 1 || st.FilesDisplaced != 0 || st.FilesFailed != 0 {
			t.Fatalf("resume: %+v warnings %d %s\n%s", st, res.Warnings, res.Summary, h.rep.dump())
		}
	case "verify", "retention":
		h.stopResume(t, j, "cancelled")
		var res jobs.Result
		if next == "verify" {
			res, _, _ = h.mustVerify()
		} else {
			var err error
			if res, _, _, err = h.runRetention(); err != nil {
				t.Fatalf("retention: %v\n%s", err, h.rep.dump())
			}
		}
		if res.Warnings != wantWarnings {
			t.Fatalf("%s: %+v\n%s", next, res, h.rep.dump())
		}
		// The sync that applies the update compares no names: it does not check (no warning).
		res, st, j2 := h.mustSync(false, jobs.Params{})
		if res.Warnings != 0 || st.FilesUpdated != 1 || st.FilesDisplaced != 0 || st.FilesFailed != 0 {
			t.Fatalf("next sync: %+v items %+v\n%s", st, h.items(j2.ID), h.rep.dump())
		}
	default:
		t.Fatalf("unknown job %q", next)
	}
	if h.rep.has("displaced an unmanaged file") || len(h.displaced(t)) != 0 {
		t.Fatalf("a file was displaced:\n%s", h.rep.dump())
	}
	// A verify or retention job records the old version the update kept; the sync that applies
	// the update then keeps it once more.
	kept := h.retainedOf("movies/a.mkv")
	if len(kept) == 0 {
		t.Fatalf("the old version of a.mkv is not retained\n%s", h.rep.dump())
	}
	for _, r := range kept {
		if r.Reason != ReasonReplaced || fileHash(t, h.dstPath(r.RetainedPath)) != strHash(content("a", 1800)) {
			t.Fatalf("retained a.mkv: %+v\n%s", r, h.rep.dump())
		}
	}
	if r, ok := h.liveRecord("movies/a.mkv"); !ok || r.State != StatePresent ||
		fileHash(t, h.dstPath("movies/a.mkv")) != strHash(content("b", 1900)) {
		t.Fatalf("live a.mkv: %+v", r)
	}
	h.assertConverged(t)
}

// storedDest returns the destination as stored.
func (h *harness) storedDest(t *testing.T) destinations.Destination {
	t.Helper()
	d, err := h.dests.Get(h.ctx, h.dest.ID)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// blockProbe puts a file where the capability probe makes its run directories
// (.bunkarr/probe), so every probe of the destination fails.
func (h *harness) blockProbe(t *testing.T) {
	t.Helper()
	p := h.dstPath(filecopy.ProbeDir)
	if err := os.RemoveAll(p); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestInodeIdentityCheckedByEveryJob: a destination probed with stable inode numbers (current
// capabilities that trust them) whose inode numbers are numbered per lookup later: the share was
// remounted with noserverino, or the CIFS client turned server inode numbers off at run time. What
// the probe found is not trusted for long: whichever job runs next checks the inode numbers again
// before it compares two names, stores that they no longer identify files, and recognizes the
// final name as the old version it hardlinked into retention (by content). It used to trust the
// probe's finding, take its own old version for an unmanaged file and displace it.
func TestInodeIdentityCheckedByEveryJob(t *testing.T) {
	for _, next := range []string{"sync", "verify", "retention"} {
		t.Run(next, func(t *testing.T) {
			h := newHarness(t)
			if c := h.storedDest(t).Capabilities; !c.InodeIdentity() {
				t.Fatalf("setup: capabilities %+v do not trust inode numbers", c)
			}
			j := h.crashAfterLinkOld(t)
			h.inventInodes() // the stored capabilities still trust the inode numbers
			h.settleAfterLinkOld(t, j, next, 0)
			if c := h.storedDest(t).Capabilities; !c.Current() || !c.UnstableInodes || c.InodeIdentity() || !c.Hardlinks {
				t.Fatalf("stored capabilities: %+v", c)
			}
			if !h.rep.has("inode numbers no longer identify files") {
				t.Fatalf("the change is not logged:\n%s", h.rep.dump())
			}
		})
	}
}

// TestInodeIdentityCheckFailureDistrustsInodes: when a job cannot check the destination's inode
// numbers again (here the probe cannot make its directory), that is a warning, and the job trusts
// no inode numbers: it compares names by content (and gets the interrupted update right on a
// destination whose inode numbers changed behind the stored capabilities). Nothing is stored.
func TestInodeIdentityCheckFailureDistrustsInodes(t *testing.T) {
	for _, next := range []string{"sync", "verify", "retention"} {
		t.Run(next, func(t *testing.T) {
			h := newHarness(t)
			j := h.crashAfterLinkOld(t)
			h.inventInodes()
			h.blockProbe(t)
			before := h.storedDest(t)
			h.settleAfterLinkOld(t, j, next, 1)
			if !h.rep.has("could not check whether the destination's inode numbers identify files") {
				t.Fatalf("no warning:\n%s", h.rep.dump())
			}
			if after := h.storedDest(t); after.Capabilities != before.Capabilities || !after.UpdatedAt.Equal(before.UpdatedAt) {
				t.Fatalf("capabilities stored: %+v (were %+v)", after.Capabilities, before.Capabilities)
			}
		})
	}
}

// TestInodeIdentityTrustedAgain: a destination whose inode numbers were found numbered per lookup
// identifies files by them again (remounted with serverino): the next job that compares two names
// stores that and trusts them. A check that finds what is stored stores nothing.
func TestInodeIdentityTrustedAgain(t *testing.T) {
	h := newHarness(t)
	h.setCaps(func(c *filecopy.Capabilities) { c.UnstableInodes = true })
	j := h.crashAfterLinkOld(t)
	h.settleAfterLinkOld(t, j, "sync", 0)
	d := h.storedDest(t)
	if c := d.Capabilities; c.UnstableInodes || !c.InodeIdentity() {
		t.Fatalf("stored capabilities: %+v", c)
	}
	if !h.rep.has("inode numbers identify files again") {
		t.Fatalf("the change is not logged:\n%s", h.rep.dump())
	}

	// Another interrupted update: checked again, unchanged, nothing stored.
	h.advance(time.Second)
	h.writeSrc("a.mkv", content("c", 2000), 4)
	j = h.newJob(jobs.TypeSync, false, jobs.Params{})
	if crashed, _, err := runWithHook(h, j, faultinject.CrashAt(PointUpdateAfterLinkOld, 1)); !crashed {
		t.Fatalf("no crash at %s: %v", PointUpdateAfterLinkOld, err)
	}
	j.Attempt, j.Trigger = 2, jobs.TriggerResume
	if res, err := h.run(h.sync, j); err != nil || res.Warnings != 0 || res.Stats.(SyncStats).FilesDisplaced != 0 {
		t.Fatalf("resume: %+v %v\n%s", res, err, h.rep.dump())
	}
	if !h.rep.has("checked the destination's inode numbers (unchanged) [inodeIdentity true]") {
		t.Fatalf("the resume did not check:\n%s", h.rep.dump())
	}
	if got := h.storedDest(t); got.Capabilities != d.Capabilities || !got.UpdatedAt.Equal(d.UpdatedAt) {
		t.Fatalf("an unchanged check stored %+v at %v (was %+v at %v)", got.Capabilities, got.UpdatedAt, d.Capabilities, d.UpdatedAt)
	}
	h.assertConverged(t)
}

// TestIdentityCheckedOnlyWhenNeeded: checking the destination's inode numbers writes (in
// .bunkarr/probe), so a job checks only before it first compares two names. A dry run cannot
// check: it trusts no inode numbers (whatever is stored) and stores nothing. A sync that fails
// its source checks (an emptied source root) leaves the destination exactly as it was, and one
// with nothing to compare checks nothing (here the probe could not run: no warning).
func TestIdentityCheckedOnlyWhenNeeded(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("a.mkv", content("a", 300), 1)
	h.firstSync()
	before := tree(t, h.dstDir)
	h.advance(time.Second)
	// An emptied source root: the sync fails before it changes anything.
	away := filepath.Join(h.base, "away")
	if err := os.Rename(h.srcDir, away); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(h.srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := h.run(h.sync, h.newJob(jobs.TypeSync, false, jobs.Params{})); err == nil {
		t.Fatalf("a sync of an empty source root succeeded\n%s", h.rep.dump())
	}
	if after := tree(t, h.dstDir); !reflect.DeepEqual(after, before) {
		t.Fatalf("a failed sync changed the destination:\nbefore %v\nafter  %v", before, after)
	}
	if err := os.Remove(h.srcDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(away, h.srcDir); err != nil {
		t.Fatal(err)
	}

	h.blockProbe(t)
	stored := h.storedDest(t)
	if res, _, _ := h.mustSync(true, jobs.Params{}); res.Warnings != 0 {
		t.Fatalf("dry-run sync: %+v\n%s", res, h.rep.dump())
	}
	if !h.rep.has("inode numbers are neither checked (that writes) nor trusted [inodeIdentity false]") {
		t.Fatalf("the dry run trusted inode numbers:\n%s", h.rep.dump())
	}
	for _, typ := range []jobs.Type{jobs.TypeVerify, jobs.TypeRetention} {
		p := jobs.Params{}
		r := jobs.Runner(h.verify)
		if typ == jobs.TypeRetention {
			p.DestinationID, r = h.dest.ID, h.retention
		}
		if res, err := h.run(r, h.newJob(typ, true, p)); err != nil || res.Warnings != 0 {
			t.Fatalf("dry-run %s: %+v %v\n%s", typ, res, err, h.rep.dump())
		}
	}
	// Nothing to compare: nothing checked.
	h.writeSrc("b.mkv", content("b", 400), 2)
	if res, st, _ := h.mustSync(false, jobs.Params{}); res.Warnings != 0 || st.FilesCopied != 1 {
		t.Fatalf("sync: %+v %+v\n%s", res, st, h.rep.dump())
	}
	if h.rep.has("could not check") || h.rep.has("checked the destination's inode numbers") {
		t.Fatalf("a job checked the inode numbers without comparing names:\n%s", h.rep.dump())
	}
	if got := h.storedDest(t); got.Capabilities != stored.Capabilities {
		t.Fatalf("capabilities stored: %+v (were %+v)", got.Capabilities, stored.Capabilities)
	}
}

// TestLinkTakesNoCopyForAHardlink: the source has a hardlinked pair a/b and the destination
// already holds that content under both names, unmanaged (a re-attached destination): as two
// separate copies, or as one file with two names. A separate copy is never recorded as a hardlink
// of the other name, where inode numbers identify files or not: where they do not, the content
// comparison cannot tell a copy from a hardlink, but the copy's link count of 1 can (it used to be
// recorded as linked). It is adopted as a file of its own, like on a destination whose inode
// numbers identify files. Names that are one file become the hardlink; where the client shows a
// link count of 1 for them (attributes a directory listing primed) the second name is adopted as a
// file of its own instead (never a copy recorded as a hardlink).
func TestLinkTakesNoCopyForAHardlink(t *testing.T) {
	for _, inodes := range []string{"stable", "per lookup", "per lookup, link count 1"} {
		for _, dest := range []string{"copies", "hardlinked"} {
			t.Run(inodes+"/"+dest, func(t *testing.T) {
				h := newHarness(t)
				switch inodes {
				case "per lookup":
					h.unstableInodes()
				case "per lookup, link count 1":
					h.inventInodesWith(true)
					h.setCaps(func(c *filecopy.Capabilities) { c.Hardlinks, c.UnstableInodes = true, true })
				}
				h.writeSrc("k.mkv", content("k", 500), 1)
				h.writeSrc("a.mkv", content("x", 1800), 2)
				h.linkSrc("a.mkv", "b.mkv")
				mt := baseTime.Add(2 * time.Second)
				writeFileAt(t, h.dstPath("movies/a.mkv"), content("x", 1800), mt)
				if dest == "copies" {
					writeFileAt(t, h.dstPath("movies/b.mkv"), content("x", 1800), mt)
				} else if err := os.Link(h.dstPath("movies/a.mkv"), h.dstPath("movies/b.mkv")); err != nil {
					t.Fatal(err)
				}
				wantLinked := dest == "hardlinked" && inodes != "per lookup, link count 1"

				res, st, j := h.mustSync(false, jobs.Params{})
				if res.Warnings != 0 || st.FilesFailed != 0 || st.FilesDisplaced != 0 || st.FilesCopied != 1 {
					t.Fatalf("sync: %+v items %+v\n%s", st, h.items(j.ID), h.rep.dump())
				}
				ra, _ := h.liveRecord("movies/a.mkv")
				rb, _ := h.liveRecord("movies/b.mkv")
				if ra.State != StatePresent {
					t.Fatalf("a.mkv is %s", ra.State)
				}
				switch {
				case wantLinked && (rb.State != StateLinked || rb.LinkOf != ra.ID):
					t.Fatalf("b.mkv is %s (link of %d), want linked to a.mkv", rb.State, rb.LinkOf)
				case !wantLinked && rb.State != StatePresent:
					t.Fatalf("b.mkv is %s (link of %d), want present: a copy of its own", rb.State, rb.LinkOf)
				}
				if sameInode := inode(t, h.dstPath("movies/a.mkv")) == inode(t, h.dstPath("movies/b.mkv")); sameInode != (dest == "hardlinked") {
					t.Fatalf("the destination's names are one file: %v", sameInode)
				}
				h.assertConverged(t)
				if _, st, j := h.mustSync(false, jobs.Params{}); st.FilesPlanned != 0 {
					t.Fatalf("a further sync planned %+v", h.items(j.ID))
				}
			})
		}
	}
}

// TestLinkResumeOnUnstableInodes: a link item made the hardlink and the process died before it was
// recorded. The resume finds the name there: with the server's link count it is recognized as
// the hardlink (by content); with a link count of 1 (attributes a directory listing primed) it is
// adopted as a file of its own. Either way nothing is displaced or copied and the records match.
func TestLinkResumeOnUnstableInodes(t *testing.T) {
	for _, nlink1 := range []bool{false, true} {
		t.Run(fmt.Sprintf("linkCount1=%v", nlink1), func(t *testing.T) {
			h := newHarness(t)
			h.inventInodesWith(nlink1)
			h.setCaps(func(c *filecopy.Capabilities) { c.Hardlinks, c.UnstableInodes = true, true })
			h.writeSrc("k.mkv", content("k", 500), 1)
			h.firstSync()
			h.pairSrc()
			j := h.newJob(jobs.TypeSync, false, jobs.Params{})
			if crashed, _, err := runWithHook(h, j, faultinject.CrashAt("link.afterLink", 1)); !crashed {
				t.Fatalf("no crash at link.afterLink: %v", err)
			}
			j.Attempt, j.Trigger = 2, jobs.TriggerResume
			res, err := h.run(h.sync, j)
			if err != nil {
				t.Fatalf("resume: %v\n%s", err, h.rep.dump())
			}
			st := res.Stats.(SyncStats)
			if res.Warnings != 0 || st.FilesDisplaced != 0 || st.FilesFailed != 0 || st.FilesCopied != 1 ||
				st.FilesLinked+st.FilesAdopted != 1 || (st.FilesAdopted == 1) != nlink1 {
				t.Fatalf("resume: %+v items %+v\n%s", st, h.items(j.ID), h.rep.dump())
			}
			r1, _ := h.liveRecord("movies/p1.mkv")
			r2, _ := h.liveRecord("movies/p2.mkv")
			if want := map[bool]State{false: StateLinked, true: StatePresent}[nlink1]; r2.State != want || r1.State != StatePresent {
				t.Fatalf("p1.mkv %s, p2.mkv %s (want %s)", r1.State, r2.State, want)
			}
			if inode(t, h.dstPath("movies/p1.mkv")) != inode(t, h.dstPath("movies/p2.mkv")) {
				t.Fatal("p2.mkv is no longer a hardlink of p1.mkv")
			}
			h.assertConverged(t)
			if _, st, j := h.mustSync(false, jobs.Params{}); st.FilesPlanned != 0 {
				t.Fatalf("a further sync planned %+v", h.items(j.ID))
			}
		})
	}
}

// TestLinkRecordedResumeOnUnstableInodes: a link item recorded the hardlink and the process died
// before the item was finished. The resume finds the name already recorded as the hardlink of the
// primary and leaves it so, also where inode numbers do not identify files and the client shows a
// link count of 1 for it (attributes a directory listing primed): the link count cannot tell a
// hardlink the job already recorded from a copy, the content can. The name used to be moved into
// retention as a replaced version and linked again.
func TestLinkRecordedResumeOnUnstableInodes(t *testing.T) {
	for _, inodes := range []string{"stable", "per lookup", "per lookup, link count 1"} {
		t.Run(inodes, func(t *testing.T) {
			h := newHarness(t)
			switch inodes {
			case "per lookup":
				h.inventInodesWith(false)
				h.setCaps(func(c *filecopy.Capabilities) { c.Hardlinks, c.UnstableInodes = true, true })
			case "per lookup, link count 1":
				h.inventInodesWith(true)
				h.setCaps(func(c *filecopy.Capabilities) { c.Hardlinks, c.UnstableInodes = true, true })
			}
			h.writeSrc("k.mkv", content("k", 500), 1)
			h.firstSync()
			h.pairSrc()
			j := h.newJob(jobs.TypeSync, false, jobs.Params{})
			// The first record the job writes is p1.mkv's copy, the second p2.mkv's link.
			if crashed, _, err := runWithHook(h, j, faultinject.CrashAt(PointRecordAfterDB, 2)); !crashed {
				t.Fatalf("no crash at %s: %v", PointRecordAfterDB, err)
			}
			r1, _ := h.liveRecord("movies/p1.mkv")
			if r2, _ := h.liveRecord("movies/p2.mkv"); r2.State != StateLinked || r2.LinkOf != r1.ID {
				t.Fatalf("before the resume p2.mkv is %s (link of %d), want recorded as the link of p1.mkv", r2.State, r2.LinkOf)
			}
			j.Attempt, j.Trigger = 2, jobs.TriggerResume
			res, err := h.run(h.sync, j)
			if err != nil {
				t.Fatalf("resume: %v\n%s", err, h.rep.dump())
			}
			st := res.Stats.(SyncStats)
			if res.Warnings != 0 || st.FilesFailed != 0 || st.FilesDisplaced != 0 || st.FilesRetained != 0 || st.FilesAdopted != 0 {
				t.Fatalf("resume: %+v items %+v\n%s", st, h.items(j.ID), h.rep.dump())
			}
			if ret := h.retainedOf("movies/p2.mkv"); len(ret) != 0 {
				t.Fatalf("the resume retained p2.mkv, the hardlink it had recorded: %+v", ret)
			}
			r1, _ = h.liveRecord("movies/p1.mkv")
			if r2, _ := h.liveRecord("movies/p2.mkv"); r1.State != StatePresent || r2.State != StateLinked || r2.LinkOf != r1.ID {
				t.Fatalf("p1.mkv %s, p2.mkv %s (link of %d), want p2.mkv linked to p1.mkv (%d)", r1.State, r2.State, r2.LinkOf, r1.ID)
			}
			if inode(t, h.dstPath("movies/p1.mkv")) != inode(t, h.dstPath("movies/p2.mkv")) {
				t.Fatal("p2.mkv is no longer a hardlink of p1.mkv")
			}
			h.assertConverged(t)
			if _, st, j := h.mustSync(false, jobs.Params{}); st.FilesPlanned != 0 {
				t.Fatalf("a further sync planned %+v", h.items(j.ID))
			}
		})
	}
}

// TestRelinkAfterARepairOnUnstableInodes: after verify marks a hardlinked primary missing and the
// next sync repairs it, the other name is relinked to the repaired file on every kind of
// destination; a record from an earlier job is never taken for a finished link on content alone.
func TestRelinkAfterARepairOnUnstableInodes(t *testing.T) {
	for _, inodes := range []string{"stable", "per lookup", "per lookup, link count 1"} {
		t.Run(inodes, func(t *testing.T) {
			h := newHarness(t)
			switch inodes {
			case "per lookup":
				h.inventInodesWith(false)
				h.setCaps(func(c *filecopy.Capabilities) { c.Hardlinks, c.UnstableInodes = true, true })
			case "per lookup, link count 1":
				h.inventInodesWith(true)
				h.setCaps(func(c *filecopy.Capabilities) { c.Hardlinks, c.UnstableInodes = true, true })
			}
			h.setSettings(func(s *destinations.Settings) { s.Verify.Mode = destinations.VerifyFull })
			h.writeSrc("p.mkv", content("p", 3000), 1)
			h.linkSrc("p.mkv", "q.mkv")
			h.writeSrc("other.mkv", content("o", 10), 2)
			h.mustSync(false, jobs.Params{})
			if inode(t, h.dstPath("movies/p.mkv")) != inode(t, h.dstPath("movies/q.mkv")) {
				t.Fatal("setup: not linked")
			}
			rp0, _ := h.liveRecord("movies/p.mkv")
			if err := os.Remove(h.dstPath("movies/p.mkv")); err != nil {
				t.Fatal(err)
			}
			if _, st, _ := h.mustVerify(); st.FilesMissing != 1 {
				t.Fatalf("verify: %+v", st)
			}
			rq, _ := h.liveRecord("movies/q.mkv")
			if rq.State != StateLinked {
				t.Fatalf("setup: q is %s", rq.State)
			}
			t.Logf("p id %d, q linkOf %d", rp0.ID, rq.LinkOf)
			_, st, j := h.mustSync(false, jobs.Params{})
			t.Logf("repair: %+v items %+v", st, h.items(j.ID))
			sameIno := inode(t, h.dstPath("movies/p.mkv")) == inode(t, h.dstPath("movies/q.mkv"))
			var kept []Record
			for _, r := range h.records() {
				if r.State == StateRetained {
					kept = append(kept, r)
				}
			}
			rp, _ := h.liveRecord("movies/p.mkv")
			rq, _ = h.liveRecord("movies/q.mkv")
			t.Logf("p %s id %d; q %s linkOf %d; same inode %v; retained %d", rp.State, rp.ID, rq.State, rq.LinkOf, sameIno, len(kept))
			if st.FilesFailed != 0 || st.FilesLinked != 1 {
				t.Errorf("repair stats: %+v", st)
			}
			if !sameIno {
				t.Errorf("q is recorded %s (link of %d) but is not a hardlink of p", rq.State, rq.LinkOf)
			}
			if len(kept) != 1 {
				t.Errorf("retained %+v", kept)
			}
			h.assertConverged(t)
			if _, st, j := h.mustSync(false, jobs.Params{}); st.FilesPlanned != 0 {
				t.Fatalf("a further sync planned %+v", h.items(j.ID))
			}
		})
	}
}
