package enginerun

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines/enginetest"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
	"github.com/sl0wz3r/bunkarr/internal/engines/restic"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/syncer"
)

// firstSnapshot runs an initial sync of files and returns its snapshot.
func (h *harness) firstSnapshot(files map[string]string) string {
	h.t.Helper()
	n := 0
	for _, rel := range sortedKeys(files) {
		n++
		h.writeSrc(rel, files[rel], n)
	}
	st, _ := h.mustSync(jobs.Params{})
	if len(st.Snapshots) != 1 {
		h.t.Fatalf("initial sync made %d snapshots", len(st.Snapshots))
	}
	return st.Snapshots[0].SnapshotID
}

func itemStatus(items []jobs.Item, rel string) (jobs.ItemStatus, string) {
	for _, it := range items {
		if it.RelPath == rel {
			return it.Status, it.Error
		}
	}
	return "", ""
}

// TestResticReadBack: what a snapshot holds is decided by its content listing only (§6.2 step 5,
// D31, the cases of §14.1): (a) a file of a whole directory deleted after the scan (exit 0, no
// error line), (b) an unchanged file that cannot be read (exit 3), (c) a file changed after the
// scan, (d) an update and a move's new side that fail. Items the snapshot does not hold fail,
// their records keep (or get) a reference to a snapshot that holds their version, and no NULL
// record's version is missing from the base.
func TestResticReadBack(t *testing.T) {
	h := newRestic(t)
	s1 := h.firstSnapshot(map[string]string{"D/x.mkv": content("x", 1000), "D/y.mkv": content("y", 1100), "z.mkv": content("z", 1200),
		"m.mkv": content("m", 1300), "k.mkv": content("k", 1400)})
	h.writeSrc("D/new.mkv", content("n", 900), 20)  // (a) a copy whose file vanishes after the scan
	h.writeSrc("z.mkv", content("Z", 1250), 21)     // (c) an update whose file changes after the scan
	h.renameSrc("m.mkv", "m2.mkv")                  // (d) a move whose new side vanishes
	h.restic.unreadable[h.srcDir+"/D/y.mkv"] = true // (b) an unchanged file restic cannot read
	h.restic.beforeBackup = func(*enginetest.Call) {
		_ = os.Remove(h.srcPath("D/x.mkv")) // (a) an unchanged file of a whole directory
		_ = os.Remove(h.srcPath("D/new.mkv"))
		writeFileAt(h.t, h.srcPath("z.mkv"), content("t", 10), baseTime.Add(99*time.Second))
		_ = os.Remove(h.srcPath("m2.mkv"))
	}
	h.advance(time1h)
	st, j := h.mustSync(jobs.Params{})
	if len(st.Snapshots) != 1 || st.FilesFailed != 3 {
		t.Fatalf("stats %+v\n%s", st, h.rep.dump())
	}
	s2 := st.Snapshots[0].SnapshotID
	items := h.items(j.ID)
	for _, rel := range []string{"D/new.mkv", "z.mkv", "m2.mkv"} {
		if s, msg := itemStatus(items, h.dp(rel)); s != jobs.ItemFailed || msg != defaultNotHeld {
			t.Errorf("%s: %s %q", rel, s, msg)
		}
	}
	if _, ok := h.live(h.dp("D/new.mkv")); ok {
		t.Error("a failed copy got a record")
	}
	for rel, want := range map[string]string{"D/x.mkv": s1, "D/y.mkv": s1, "z.mkv": s1, "m.mkv": s1, "k.mkv": ""} {
		r := h.mustLive(h.dp(rel))
		if r.EngineRef != want {
			t.Errorf("%s: engine_ref %q, want %q", rel, short(r.EngineRef), short(want))
		}
	}
	if r := h.mustLive(h.dp("z.mkv")); r.Size != 1200 {
		t.Errorf("the failed update changed its record: %+v", r)
	}
	if s2 == s1 {
		t.Fatal("no new snapshot")
	}
	h.assertResticRefs(t)
	// The next sync backs the file up again (the unreadable file is readable again, the
	// truncated one is planned as an update of its new content) and clears the references it
	// holds; references are never moved to another snapshot.
	h.restic.beforeBackup = nil
	delete(h.restic.unreadable, h.srcDir+"/D/y.mkv")
	h.writeSrc("later.mkv", content("l", 600), 60) // a content item, so the sync makes a snapshot
	h.advance(time1h)
	h.mustSync(jobs.Params{})
	if r := h.mustLive(h.dp("D/y.mkv")); r.EngineRef != "" {
		t.Errorf("D/y.mkv still references %s after a snapshot holds it", short(r.EngineRef))
	}
	h.assertResticRefs(t)
}

// TestResticErrorLine: an item whose file restic reported unreadable fails with the error line's
// text (exit 3 records the snapshot).
func TestResticErrorLine(t *testing.T) {
	h := newRestic(t)
	h.firstSnapshot(map[string]string{"a.mkv": content("a", 1000)})
	h.writeSrc("b.mkv", content("b", 900), 5)
	h.restic.unreadable[h.srcDir+"/b.mkv"] = true
	h.advance(time1h)
	_, j := h.mustSync(jobs.Params{})
	if s, msg := itemStatus(h.items(j.ID), h.dp("b.mkv")); s != jobs.ItemFailed || !strings.HasPrefix(msg, "could not be read: open ") {
		t.Errorf("b.mkv: %s %q", s, msg)
	}
}

// TestResticResumeReconcile: (e) a crash after restic's exit: the resumed job reads back the
// batch snapshot its earlier attempt made (items held are done, the others stay pending and run
// again); nothing is assumed done because a snapshot exists.
func TestResticResumeReconcile(t *testing.T) {
	h := newRestic(t)
	h.firstSnapshot(map[string]string{"a.mkv": content("a", 1000)})
	h.writeSrc("b.mkv", content("b", 900), 5)
	h.writeSrc("c.mkv", content("c", 800), 6)
	h.restic.beforeBackup = func(*enginetest.Call) {
		// c.mkv is not readable during the first attempt's backup only.
		h.restic.unreadable[h.srcDir+"/c.mkv"] = true
	}
	h.advance(time1h)
	j := h.newJob(jobs.TypeSync, false, jobs.Params{})
	crashed, _, err := h.runWithHook(j, faultinject.CrashAt(PointAfterBatchExit, 1))
	if !crashed {
		t.Fatalf("no crash: %v", err)
	}
	rows, _ := h.svc.jobSnapshots(h.ctx, h.dest.ID, j.ID)
	if len(rows) != 0 {
		t.Fatal("the crashed batch was recorded")
	}
	h.restic.beforeBackup = nil
	delete(h.restic.unreadable, h.srcDir+"/c.mkv")
	j.Attempt, j.Trigger = 2, jobs.TriggerResume
	if _, err := h.runJob(j); err != nil {
		t.Fatalf("resume: %v\n%s", err, h.rep.dump())
	}
	h.closeJob(j.ID)
	rows, _ = h.svc.jobSnapshots(h.ctx, h.dest.ID, j.ID)
	if len(rows) != 2 || rows[0].row.Batch != 1 || rows[1].row.Batch != 2 || !rows[1].row.Complete || rows[0].row.Complete {
		t.Fatalf("job snapshots %+v", rows)
	}
	items := h.items(j.ID)
	for _, rel := range []string{"b.mkv", "c.mkv"} {
		if s, _ := itemStatus(items, h.dp(rel)); s != jobs.ItemDone {
			t.Errorf("%s: %s", rel, s)
		}
	}
	// The second attempt's backup included only what the reconciled batch did not hold.
	last := h.callsOf(proc.Restic, "backup")
	if tags := last[len(last)-1].FlagValues("--tag"); !slices.Contains(tags, restic.BatchTag(2)) {
		t.Errorf("the rerun batch is tagged %v", tags)
	}
	h.assertResticRefs(t)
}

// TestResticBatches: content items are cut into batches of batchFiles, each a cumulative
// snapshot tagged with its batch; only the last is complete; --parent is the previous batch.
func TestResticBatches(t *testing.T) {
	h := newHarness(t, harnessOptions{kind: "restic", settings: &destinations.Settings{Restic: &destinations.ResticSettings{BatchFiles: 2}}})
	for i := range 5 {
		h.writeSrc("f"+string(rune('a'+i))+".mkv", content("f"+string(rune('a'+i)), 500+i), i)
	}
	st, j := h.mustSync(jobs.Params{})
	if st.Batches != 3 || len(st.Snapshots) != 3 || st.FilesCopied != 5 {
		t.Fatalf("stats %+v", st)
	}
	rows, _ := h.svc.jobSnapshots(h.ctx, h.dest.ID, j.ID)
	for i, r := range rows {
		if r.row.Batch != i+1 || r.row.Complete != (i == 2) {
			t.Errorf("row %d: %+v", i, r.row)
		}
	}
	backups := h.callsOf(proc.Restic, "backup")
	for i, c := range backups {
		if i == 0 {
			continue
		}
		if p, _ := c.Flag("--parent"); p != rows[i-1].row.SnapshotID {
			t.Errorf("batch %d: --parent %q", i+1, short(p))
		}
	}
	// Batch k includes batches 1..k (cumulative), not later ones.
	if n := len(h.restic.snapshot(rows[0].row.SnapshotID).files); n != 2 {
		t.Errorf("batch 1 holds %d files", n)
	}
	if n := len(h.restic.snapshot(rows[2].row.SnapshotID).files); n != 5 {
		t.Errorf("batch 3 holds %d files", n)
	}
	h.assertResticRefs(t)
}

// TestResticHeldUpdate: an update the mass-change guard holds keeps its old version referenced
// in every sync it stays held (S10(b), §6.4), and when the file is then deleted its retained row
// references that snapshot.
func TestResticHeldUpdate(t *testing.T) {
	h := newRestic(t)
	h.settings(func(s *destinations.Settings) { s.MaxChangeFiles, s.MaxChangePercent = 1000, 100 })
	s1 := h.firstSnapshot(map[string]string{"big.mkv": content("b", 4000), "other.mkv": content("o", 500)})
	h.writeSrc("big.mkv", content("B", 100), 30) // shrinks to less than half: always held
	for i := range 3 {
		h.writeSrc("new"+string(rune('0'+i))+".mkv", content("n", 300+i), 40+i)
		h.advance(time1h)
		_, j := h.mustSync(jobs.Params{})
		if s, _ := itemStatus(h.items(j.ID), h.dp("big.mkv")); s != jobs.ItemHeld {
			t.Fatalf("sync %d: big.mkv %s", i+1, s)
		}
		if r := h.mustLive(h.dp("big.mkv")); r.EngineRef != s1 || r.Size != 4000 {
			t.Fatalf("sync %d: big.mkv %+v, want its version in %s", i+1, r, short(s1))
		}
		h.assertResticRefs(t)
	}
	h.removeSrc("big.mkv")
	h.advance(time1h)
	h.mustSync(jobs.Params{})
	var ret syncer.Record
	for _, r := range h.retained() {
		if r.SourceRelPath == "big.mkv" {
			ret = r
		}
	}
	if ret.EngineRef != s1 || ret.Size != 4000 {
		t.Errorf("the held-then-deleted file's row %+v, want a reference to %s", ret, short(s1))
	}
	h.assertResticRefs(t)
}

// TestResticRevertedFile: a file whose held change was reverted is in the include set again
// whatever its reference, and the read-back clears the reference (§6.2 step 4).
func TestResticRevertedFile(t *testing.T) {
	h := newRestic(t)
	h.settings(func(s *destinations.Settings) { s.MaxChangeFiles, s.MaxChangePercent = 1000, 100 })
	s1 := h.firstSnapshot(map[string]string{"a.mkv": content("a", 4000), "b.mkv": content("b", 100)})
	h.writeSrc("a.mkv", content("A", 10), 50) // held (shrink)
	h.writeSrc("n1.mkv", content("n", 200), 51)
	h.advance(time1h)
	h.mustSync(jobs.Params{})
	if r := h.mustLive(h.dp("a.mkv")); r.EngineRef != s1 {
		t.Fatalf("held a.mkv: %+v", r)
	}
	h.writeSrc("a.mkv", content("a", 4000), 1) // reverted: the recorded version again
	h.writeSrc("n2.mkv", content("m", 210), 52)
	h.advance(time1h)
	h.mustSync(jobs.Params{})
	last := h.callsOf(proc.Restic, "backup")
	if inc := string(h.restic.lastInclude(last[len(last)-1])); inc != h.srcDir {
		t.Errorf("include list %q, want the whole source", inc)
	}
	if r := h.mustLive(h.dp("a.mkv")); r.EngineRef != "" {
		t.Errorf("the reverted file still references %s", short(r.EngineRef))
	}
	h.assertResticRefs(t)
}

// TestResticS6Wait: an upgrade whose new file cannot be read keeps the old version live and
// referenced (the S6 wait, decided after the last batch): retention with every keep value 0
// keeps its snapshot after deletedDays.
func TestResticS6Wait(t *testing.T) {
	h := newRestic(t)
	s1 := h.firstSnapshot(map[string]string{"M/x-1080p.mkv": content("old", 2000), "keep.mkv": content("k", 100)})
	h.removeSrc("M/x-1080p.mkv")
	h.writeSrc("M/x-2160p.mkv", content("new", 3000), 20)
	h.restic.unreadable[h.srcDir+"/M/x-2160p.mkv"] = true
	h.advance(time1h)
	_, j := h.mustSync(jobs.Params{})
	var waiting bool
	for _, it := range h.items(j.ID) {
		if it.Action == jobs.ActionRetain && it.Status == jobs.ItemFailed && strings.Contains(it.Error, "not retained yet") {
			waiting = true
		}
	}
	if !waiting {
		t.Fatalf("the retain did not wait: %+v", h.items(j.ID))
	}
	if r := h.mustLive(h.dp("M/x-1080p.mkv")); r.EngineRef != s1 {
		t.Fatalf("old version %+v, want a reference to %s", r, short(s1))
	}
	zero := 0
	h.update(destinations.Input{Retention: &destinations.Retention{DeletedDays: 30, SnapshotDaily: &zero, SnapshotWeekly: &zero,
		SnapshotMonthly: &zero, SnapshotYearly: &zero}})
	h.advance(40 * 24 * time.Hour)
	h.mustRun(jobs.TypeRetention, false, jobs.Params{})
	if h.restic.snapshot(s1) == nil {
		t.Error("retention forgot the snapshot of the old version")
	}
	h.assertResticRefs(t)
}

// TestResticTargetedSync: a targeted sync plans only its paths and still makes a whole-source
// snapshot (§6.2).
func TestResticTargetedSync(t *testing.T) {
	h := newRestic(t)
	h.firstSnapshot(map[string]string{"A/a.mkv": content("a", 1000), "B/b.mkv": content("b", 900)})
	h.writeSrc("A/a2.mkv", content("c", 700), 30)
	h.advance(time1h)
	st, _ := h.mustSync(jobs.Params{SourceIDs: []int64{h.src.ID}, Paths: []string{"A"}})
	if !st.Targeted || st.FilesCopied != 1 || len(st.Snapshots) != 1 {
		t.Fatalf("stats %+v", st)
	}
	s := h.restic.snapshot(st.Snapshots[0].SnapshotID)
	if len(s.files) != 3 {
		t.Errorf("the targeted snapshot holds %d files, want the whole source (3)", len(s.files))
	}
	h.assertResticRefs(t)
}

// TestResticSnapshotRemovedOutside: a recorded snapshot missing from the preflight listing is a
// warning; its row goes, retained rows that reference it are deleted (version lost), the records
// it held become missing and are uploaded again, and no --parent is passed.
func TestResticSnapshotRemovedOutside(t *testing.T) {
	h := newRestic(t)
	s1 := h.firstSnapshot(map[string]string{"a.mkv": content("a", 1000), "b.mkv": content("b", 900)})
	h.removeSrc("b.mkv")
	h.writeSrc("c.mkv", content("c", 800), 10)
	h.advance(time1h)
	st, _ := h.mustSync(jobs.Params{})
	s2 := st.Snapshots[0].SnapshotID
	h.restic.removeSnapshot(s1) // b.mkv's retained version is lost
	h.restic.removeSnapshot(s2) // the base: a.mkv and c.mkv are missing now
	h.advance(time1h)
	st, _ = h.mustSync(jobs.Params{})
	if !h.rep.has("was removed outside Bunkarr") || !h.rep.has("version lost") {
		t.Errorf("no warnings:\n%s", h.rep.dump())
	}
	if st.FilesCopied != 2 {
		t.Errorf("stats %+v: the missing records are not uploaded again", st)
	}
	last := h.callsOf(proc.Restic, "backup")
	if last[len(last)-1].Has("--parent") {
		t.Error("a --parent was passed although the base is gone")
	}
	if len(h.retained()) != 0 {
		t.Errorf("retained rows of the removed snapshot are left: %+v", h.retained())
	}
	rows, _ := h.svc.snapshotRows(h.ctx, h.dest.ID)
	for _, r := range rows {
		if r.SnapshotID == s1 || r.SnapshotID == s2 {
			t.Errorf("the row of removed snapshot %s is left", short(r.SnapshotID))
		}
	}
	h.assertResticRefs(t)
}
