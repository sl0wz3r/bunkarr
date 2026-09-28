package enginerun

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/syncer"
)

func newRclone(t *testing.T) *harness {
	return newHarness(t, harnessOptions{kind: engines.Rclone})
}

// TestRcloneSyncLifecycle: an initial sync, then one change, one addition, one deletion and one
// rename (acceptance 3): every item kind's outcome comes from the listings, the old versions are
// held (S5), and a further sync plans nothing.
func TestRcloneSyncLifecycle(t *testing.T) {
	h := newRclone(t)
	h.writeSrc("a.mkv", content("a1", 3000), 1)
	h.writeSrc("b.mkv", content("b", 2000), 2)
	h.writeSrc("c.mkv", content("c", 1500), 3)
	h.writeSrc("d/e.srt", content("e", 100), 4)
	st, _ := h.mustSync(jobs.Params{})
	if st.FilesCopied != 4 || st.Engine != "rclone" || st.BytesUploaded != 6600 {
		t.Fatalf("initial sync stats: %+v", st)
	}
	for _, rel := range []string{"a.mkv", "b.mkv", "c.mkv", "d/e.srt"} {
		r := h.mustLive(h.dp(rel))
		if r.State != syncer.StatePresent || r.HeadTail == "" {
			t.Errorf("%s: record %+v", rel, r)
		}
		if _, ok := h.rclone.get(h.dp(rel)); !ok {
			t.Errorf("%s: no object", rel)
		}
	}
	if _, ok := h.rclone.get(syncer.LinkManifestRel); !ok {
		t.Error("links.tsv not uploaded")
	}

	h.advance(time1h)
	h.writeSrc("a.mkv", content("a2", 3300), 20)  // update
	h.writeSrc("new.mkv", content("n", 1300), 21) // copy
	h.removeSrc("c.mkv")                          // retain
	h.renameSrc("b.mkv", "moved/b.mkv")           // move
	st, j := h.mustSync(jobs.Params{})
	if st.FilesCopied != 1 || st.FilesUpdated != 1 || st.FilesMoved != 1 || st.FilesRetained != 1 {
		t.Fatalf("second sync stats: %+v\n%s", st, h.rep.dump())
	}
	// The old a.mkv moved into retention server-side: not uploaded (only a2 and new.mkv are).
	if st.BytesUploaded != 3300+1300 {
		t.Errorf("second sync uploaded %d bytes, want %d", st.BytesUploaded, 3300+1300)
	}
	if r := h.mustLive(h.dp("a.mkv")); r.Size != 3300 {
		t.Errorf("a.mkv: %+v", r)
	}
	if r := h.mustLive(h.dp("moved/b.mkv")); r.HeadTail == "" || r.Size != 2000 {
		t.Errorf("moved/b.mkv: %+v", r)
	}
	if _, ok := h.live(h.dp("b.mkv")); ok {
		t.Error("b.mkv still recorded at its old path")
	}
	ret := h.retained()
	reasons := map[string]string{}
	for _, r := range ret {
		reasons[r.SourceRelPath] = r.Reason
		if !strings.HasPrefix(r.RetainedPath, filecopy.RetentionRoot+"/") {
			t.Errorf("retained %s at %s", r.RelPath, r.RetainedPath)
		}
		if o, ok := h.rclone.get(r.RetainedPath); !ok || int64(len(o.data)) != r.Size {
			t.Errorf("retained object %s: present %v", r.RetainedPath, ok)
		}
	}
	if reasons["a.mkv"] != syncer.ReasonReplaced || reasons["c.mkv"] != syncer.ReasonDeleted {
		t.Errorf("retained reasons: %v", reasons)
	}
	if len(h.callsOf(proc.Rclone, "sync")) != 0 {
		t.Error("rclone sync ran")
	}
	for _, c := range h.callsOf(proc.Rclone, "copy") {
		if !c.Has("--backup-dir") || !c.Has("--max-delete") {
			t.Errorf("copy without --backup-dir or --max-delete: %s", c)
		}
	}
	_ = j
	h.advance(time1h)
	st, j = h.mustSync(jobs.Params{})
	if st.FilesPlanned != 0 {
		t.Errorf("a further sync planned %d items: %v", st.FilesPlanned, itemsBy(h.items(j.ID), "", ""))
	}
}

// TestRcloneSyncOutcomes: the after-listing decides every copy item (§7.3): an unmanaged object
// that matches is adopted, one that differs is displaced into retention, a missing record's
// object is retained as damaged, and --max-delete is the batch's item count.
func TestRcloneSyncOutcomes(t *testing.T) {
	h := newRclone(t)
	h.writeSrc("keep.mkv", content("k", 1000), 1)
	h.mustSync(jobs.Params{})
	// An object that matches the source (adopted) and one that does not (displaced).
	h.writeSrc("adopt.mkv", content("ad", 1200), 2)
	fi, err := os.Stat(h.srcPath("adopt.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	h.rclone.put(h.dp("adopt.mkv"), []byte(content("ad", 1200)), fi.ModTime())
	h.writeSrc("junk.mkv", content("j", 900), 3)
	h.rclone.put(h.dp("junk.mkv"), []byte("someone else's file"), h.now())
	// A record verify found damaged: its object is replaced and kept as damaged.
	rec := h.mustLive(h.dp("keep.mkv"))
	h.exec(`UPDATE destination_files SET state = 'missing' WHERE id = ?`, rec.ID)
	h.rclone.put(h.dp("keep.mkv"), []byte("damaged"), h.now())
	h.advance(time1h)
	st, j := h.mustSync(jobs.Params{})
	if st.FilesAdopted != 1 || st.FilesDisplaced != 1 || st.FilesCopied != 2 {
		t.Fatalf("stats %+v\n%s", st, h.rep.dump())
	}
	reasons := map[string]string{}
	for _, r := range h.retained() {
		reasons[r.SourceRelPath] = r.Reason
	}
	if reasons["junk.mkv"] != syncer.ReasonDisplaced || reasons["keep.mkv"] != syncer.ReasonDamaged {
		t.Errorf("retained reasons %v", reasons)
	}
	for _, rel := range []string{"adopt.mkv", "junk.mkv", "keep.mkv"} {
		if r := h.mustLive(h.dp(rel)); r.State != syncer.StatePresent {
			t.Errorf("%s: %+v", rel, r)
		}
	}
	for _, c := range h.rclone.copies() {
		if c.maxDelete != len(c.files) {
			t.Errorf("copy of %d files with --max-delete %d", len(c.files), c.maxDelete)
		}
	}
	var adopted bool
	for _, it := range h.items(j.ID) {
		d, _ := parseItem(it)
		if it.RelPath == h.dp("adopt.mkv") && d.Outcome == "adopted" {
			adopted = true
		}
	}
	if !adopted {
		t.Error("the matching object was not recorded as adopted")
	}
}

// TestRcloneMoveOntoOccupiedTarget: the target of every moveto is listed first and an unmanaged
// object there is displaced into retention before the move (S2, S23).
func TestRcloneMoveOntoOccupiedTarget(t *testing.T) {
	h := newRclone(t)
	h.writeSrc("a.mkv", content("a", 2000), 1)
	h.mustSync(jobs.Params{})
	h.renameSrc("a.mkv", "b.mkv")
	h.rclone.put(h.dp("b.mkv"), []byte("unmanaged object"), h.now())
	h.advance(time1h)
	st, _ := h.mustSync(jobs.Params{})
	if st.FilesMoved != 1 || st.FilesDisplaced != 1 {
		t.Fatalf("stats %+v\n%s", st, h.rep.dump())
	}
	o, ok := h.rclone.get(h.dp("b.mkv"))
	if !ok || string(o.data) != content("a", 2000) {
		t.Fatal("b.mkv does not hold the moved object")
	}
	var displaced syncer.Record
	for _, r := range h.retained() {
		if r.Reason == syncer.ReasonDisplaced {
			displaced = r
		}
	}
	if got, ok := h.rclone.get(displaced.RetainedPath); !ok || string(got.data) != "unmanaged object" {
		t.Errorf("the unmanaged object is not in retention: %+v", displaced)
	}
	for _, c := range h.callsOf(proc.Rclone, "moveto") {
		if v, _ := c.Flag("--max-delete"); v != "1" {
			t.Errorf("moveto with --max-delete %q", v)
		}
	}
}

// TestRcloneRetentionCollision: an update whose backup-dir path is taken (a resumed job) runs
// alone: what is there is recorded, the live object moves to a numbered name, and the file is
// uploaded with copyto.
func TestRcloneRetentionCollision(t *testing.T) {
	h := newRclone(t)
	h.writeSrc("a.mkv", content("a1", 2000), 1)
	h.mustSync(jobs.Params{})
	h.writeSrc("a.mkv", content("a2", 2100), 5)
	h.advance(time1h)
	j := h.newJob(jobs.TypeSync, false, jobs.Params{})
	rd := filecopy.RetentionDir(j.QueuedAt, j.ID)
	h.rclone.put(rd+"/"+h.dp("a.mkv"), []byte("an object of an earlier attempt"), h.now())
	if _, err := h.runJob(j); err != nil {
		t.Fatalf("%v\n%s", err, h.rep.dump())
	}
	if r := h.mustLive(h.dp("a.mkv")); r.Size != 2100 || r.State != syncer.StatePresent {
		t.Errorf("a.mkv: %+v", r)
	}
	var paths []string
	for _, r := range h.retained() {
		paths = append(paths, r.RetainedPath)
	}
	want := []string{rd + "/" + h.dp("a.1.mkv"), rd + "/" + h.dp("a.mkv")}
	slices.Sort(paths)
	if !slices.Equal(paths, want) {
		t.Errorf("retained %v, want %v", paths, want)
	}
	if o, _ := h.rclone.get(rd + "/" + h.dp("a.1.mkv")); string(o.data) != content("a1", 2000) {
		t.Error("the old version is not at the numbered name")
	}
	if len(h.callsOf(proc.Rclone, "copyto")) == 0 {
		t.Error("the item did not run alone with copyto")
	}
}

// TestRcloneS6Wait: a retain waits while a new file of its folder is not backed up (phase1.md S6):
// the item fails with the S6 warning and the record stays live; the next sync retains it.
func TestRcloneS6Wait(t *testing.T) {
	h := newRclone(t)
	h.writeSrc("M/x-1080p.mkv", content("old", 2000), 1)
	h.mustSync(jobs.Params{})
	h.removeSrc("M/x-1080p.mkv")
	h.writeSrc("M/x-2160p.mkv", content("new", 4000), 2)
	h.rclone.copyErr["M/x-2160p.mkv"] = "upload failed"
	h.advance(time1h)
	_, j := h.mustSync(jobs.Params{})
	var s6 bool
	for _, it := range h.items(j.ID) {
		if it.Action == jobs.ActionRetain && it.Status == jobs.ItemFailed && strings.Contains(it.Error, "not retained yet") {
			s6 = true
		}
	}
	if !s6 {
		t.Fatalf("the retain did not wait: %+v", h.items(j.ID))
	}
	if r := h.mustLive(h.dp("M/x-1080p.mkv")); r.State != syncer.StatePresent {
		t.Errorf("old version: %+v", r)
	}
	delete(h.rclone.copyErr, "M/x-2160p.mkv")
	h.advance(time1h)
	st, _ := h.mustSync(jobs.Params{})
	if st.FilesCopied != 1 || st.FilesRetained != 1 {
		t.Errorf("the next sync: %+v", st)
	}
}

// TestRcloneLinksAndPromote: another name of a hardlink group is link_recorded and listed in
// links.tsv (rewritten only when it changed); when the primary vanishes its object moves to the
// surviving name (promote).
func TestRcloneLinksAndPromote(t *testing.T) {
	h := newRclone(t)
	h.writeSrc("p1.mkv", content("p", 1800), 1)
	h.linkSrc("p1.mkv", "p2.mkv")
	h.mustSync(jobs.Params{})
	r1, r2 := h.mustLive(h.dp("p1.mkv")), h.mustLive(h.dp("p2.mkv"))
	if r2.State != syncer.StateLinkRecorded || r2.LinkOf != r1.ID {
		t.Fatalf("p2: %+v (p1 %+v)", r2, r1)
	}
	if _, ok := h.rclone.get(h.dp("p2.mkv")); ok {
		t.Error("a recorded-only link has an object")
	}
	links, _ := h.rclone.get(syncer.LinkManifestRel)
	if !strings.Contains(string(links.data), "p2.mkv\t"+h.dp("p1.mkv")) {
		t.Errorf("links.tsv:\n%s", links.data)
	}
	rcats := len(h.callsOf(proc.Rclone, "rcat"))
	h.advance(time1h)
	h.mustSync(jobs.Params{})
	if n := len(h.callsOf(proc.Rclone, "rcat")); n != rcats {
		t.Errorf("links.tsv rewritten without a change (%d rcat, was %d)", n, rcats)
	}
	h.removeSrc("p1.mkv")
	h.advance(time1h)
	st, _ := h.mustSync(jobs.Params{})
	if st.FilesPromoted != 1 {
		t.Fatalf("stats %+v\n%s", st, h.rep.dump())
	}
	if r := h.mustLive(h.dp("p2.mkv")); r.State != syncer.StatePresent || r.LinkOf != 0 {
		t.Errorf("p2 after the promote: %+v", r)
	}
	if o, ok := h.rclone.get(h.dp("p2.mkv")); !ok || string(o.data) != content("p", 1800) {
		t.Error("p2 does not hold the object")
	}
	if _, ok := h.live(h.dp("p1.mkv")); ok {
		t.Error("p1 is still recorded")
	}
	if n := len(h.callsOf(proc.Rclone, "rcat")); n != rcats+1 {
		t.Errorf("links.tsv not rewritten after the change (%d rcat)", n)
	}
}

// TestRcloneCancelSettles: a sync cancelled while a copy runs still records, from the after-batch
// listing, the object --backup-dir moved out of the way (a cancelled job is never resumed, S23):
// no object in retention is left without its row.
func TestRcloneCancelSettles(t *testing.T) {
	h := newRclone(t)
	h.writeSrc("a.mkv", content("a", 1000), 1)
	h.rclone.put(h.dp("a.mkv"), []byte(content("x", 700)), h.now()) // unmanaged
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	h.rclone.beforeCopy = func([]string) { cancel() }
	j := h.newJob(jobs.TypeSync, false, jobs.Params{})
	_, err := h.svc.Dispatch(j.Type).Run(ctx, j, h.env())
	h.closeJob(j.ID)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cancellation", err)
	}
	moved := h.rclone.paths(".bunkarr/retention/")
	if len(moved) != 1 {
		t.Fatalf("objects in retention: %v", moved)
	}
	var recorded bool
	for _, r := range h.retained() {
		recorded = recorded || (r.RetainedPath == moved[0] && r.Reason == syncer.ReasonDisplaced && r.Size == 700)
	}
	if !recorded {
		t.Errorf("the displaced object %s has no retained row: %+v", moved[0], h.retained())
	}
}

// TestRcloneProgressAcrossBatches: the bytes done of an rclone sync add up over its batches
// (§10.4): they never fall back when the next batch's rclone starts counting from zero.
func TestRcloneProgressAcrossBatches(t *testing.T) {
	h := newHarness(t, harnessOptions{kind: engines.Rclone, settings: &destinations.Settings{Rclone: &destinations.RcloneSettings{BatchFiles: 1}}})
	h.writeSrc("a.mkv", content("a", 2000), 1)
	h.writeSrc("b.mkv", content("b", 1000), 2)
	h.mustSync(jobs.Params{})
	var last int64
	for _, p := range h.rep.progress {
		if p.Phase != "uploading" {
			continue
		}
		if p.BytesDone < last {
			t.Fatalf("bytes done fell from %d to %d", last, p.BytesDone)
		}
		last = p.BytesDone
	}
	if last != 3000 {
		t.Errorf("bytes done %d, want 3000", last)
	}
}

// TestRcloneUnsupportedNames: a name rclone cannot be given (a line break in a file list, a
// control character on the command line; the catalog allows both) fails only its own item: the
// job completes, and verify and retention skip it (§7.3).
func TestRcloneUnsupportedNames(t *testing.T) {
	h := newRclone(t)
	h.settings(func(s *destinations.Settings) { s.Verify.Mode, s.Verify.SamplePercent = destinations.VerifySample, 100 })
	h.writeSrc("a.mkv", content("a", 1000), 1)
	h.writeSrc("line\nbreak.mkv", content("n", 1100), 2)
	h.writeSrc("tab\tname.mkv", content("t", 1200), 3)
	st, j := h.mustSync(jobs.Params{})
	if s, _ := itemStatus(h.items(j.ID), h.dp("line\nbreak.mkv")); s != jobs.ItemFailed || st.FilesCopied != 2 {
		t.Errorf("line break: %s; stats %+v", s, st)
	}
	// A rename of the tab name needs moveto, which cannot name it.
	h.renameSrc("tab\tname.mkv", "tab\tmoved.mkv")
	h.advance(time1h)
	_, j = h.mustSync(jobs.Params{AllowChanges: true})
	if s, msg := itemStatus(h.items(j.ID), h.dp("tab\tmoved.mkv")); s != jobs.ItemFailed || !strings.Contains(msg, "cannot be passed to rclone") {
		t.Errorf("tab rename: %s %q", s, msg)
	}
	res, _ := h.mustRun(jobs.TypeVerify, false, jobs.Params{})
	if vs := verifyStats(t, res); vs.FilesMissing != 0 {
		t.Errorf("verify %+v", vs)
	}
}
