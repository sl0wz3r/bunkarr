package syncer

import (
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/tiers"
)

// tiersCrashCase is a release sync whose plan has every tier item: a copy (full), no item for a
// manifest file (skip in the preview), a kept file, releases of kept files the preview listed, and
// a retain of an upgrade's old version whose new version is manifest. It returns the harness and
// the real job's params.
func tiersCrashCase(t *testing.T) (*harness, jobs.Params) {
	h := newHarness(t)
	crashConfigs[0].apply(h)
	ft := newFakeTiers(h)
	h.withTiers(ft)
	h.writeSrc("a.mkv", content("a", 3000), 1)
	h.writeSrc("b.mkv", content("b", 2000), 2)
	h.writeSrc("c.mkv", content("c", 1500), 3)
	h.writeSrc("d.mkv", content("d", 1200), 4)
	h.writeSrc("X/old.mkv", content("old", 1800), 5)
	if _, st, _ := h.mustSync(false, jobs.Params{}); st.FilesCopied != 5 {
		t.Fatalf("first sync %+v", st)
	}
	for _, rel := range []string{"a.mkv", "b.mkv", "c.mkv"} {
		ft.set(rel, tiers.Manifest)
	}
	ft.revision = 2
	h.writeSrc("c.mkv", content("C", 1600), 6) // a kept file changes at the source
	h.removeSrc("X/old.mkv")                   // an upgrade to a manifest-tier version
	h.writeSrc("X/new.mkv", content("new", 2500), 7)
	ft.set("X/new.mkv", tiers.Manifest)
	h.writeSrc("new.mkv", content("n", 1300), 8) // a full copy
	h.writeSrc("man.mkv", content("m", 1400), 9) // a manifest file: not copied
	ft.set("man.mkv", tiers.Manifest)
	_, pj := h.releasePreview()
	ft.set("d.mkv", tiers.Manifest) // kept, but not in the preview: never released
	return h, jobs.Params{ReleaseDemoted: true, AllowChanges: true, ReleaseOf: pj.ID, ReleaseRevision: 2}
}

// assertTiersConverged checks the tier crash-matrix invariants: no temp file; the records match
// the filesystem; no file of a non-full tier left the destination except the released ones; every
// released file is in retention with reason released; the upgrade's old version is retained; the
// full copy is there; the manifest files were never copied.
func (h *harness) assertTiersConverged(t *testing.T) {
	t.Helper()
	files := regularFiles(t, h.dstDir)
	live, retained := map[string]Record{}, map[string]Record{}
	for _, r := range h.records() {
		if r.State.Live() {
			live[r.RelPath] = r
		} else {
			retained[r.RetainedPath] = r
		}
	}
	for rel, sum := range files {
		switch {
		case filecopy.IsTempName(path.Base(rel)):
			t.Errorf("temp file left: %s", rel)
		case strings.HasPrefix(rel, filecopy.RetentionRoot+"/"):
			r, ok := retained[rel]
			if !ok {
				t.Errorf("unrecorded file in retention: %s", rel)
			} else if r.Hash != "" && r.Hash != filecopy.HashPrefix+sum {
				t.Errorf("retained %s: not the recorded content", rel)
			}
		case strings.HasPrefix(rel, filecopy.MetaDir+"/"):
		default:
			r, ok := live[rel]
			if !ok {
				t.Errorf("unrecorded file in the mirror: %s", rel)
			} else if r.Hash != "" && r.Hash != filecopy.HashPrefix+sum {
				t.Errorf("%s: not the recorded content", rel)
			}
		}
	}
	for rel, r := range live {
		if _, ok := files[rel]; !ok && r.State == StatePresent {
			t.Errorf("record %s has no file", rel)
		}
	}
	released := func(rel string) {
		t.Helper()
		if _, ok := live["movies/"+rel]; ok {
			t.Errorf("%s is still live", rel)
		}
		rs := h.retainedOf("movies/" + rel)
		if len(rs) != 1 || rs[0].Reason != ReasonReleased {
			t.Errorf("%s: retained %+v, want one released version", rel, rs)
		}
	}
	released("a.mkv")
	released("b.mkv")
	released("c.mkv")
	if r, ok := live["movies/d.mkv"]; !ok || files["movies/d.mkv"] != strHash(content("d", 1200)) || r.State != StatePresent {
		t.Error("the kept file d.mkv (not in the preview) left the destination or changed")
	}
	if rs := h.retainedOf("movies/X/old.mkv"); len(rs) != 1 || rs[0].Reason != ReasonDeleted {
		t.Errorf("the upgrade's old version: %+v", rs)
	}
	if files["movies/new.mkv"] != strHash(content("n", 1300)) {
		t.Error("the full copy is missing")
	}
	for _, rel := range []string{"movies/man.mkv", "movies/X/new.mkv"} {
		if _, ok := files[rel]; ok {
			t.Errorf("the manifest file %s was copied", rel)
		}
	}
}

// TestCrashMatrixTiers crashes a release sync at every syncer and filecopy point it reaches,
// resumes it, settles with one more sync, and checks the tier invariants and that a further sync
// plans nothing (phase2-3.md §15).
func TestCrashMatrixTiers(t *testing.T) {
	hits := map[string]int{}
	{
		h, p := tiersCrashCase(t)
		j := h.newJob(jobs.TypeSync, false, p)
		if crashed, res, err := runWithHook(h, j, counting(hits)); crashed || err != nil || res.Warnings != 0 {
			t.Fatalf("clean run: %v %v %+v\n%s", crashed, err, res, h.rep.dump())
		}
		h.closeJob(j.ID)
		h.assertTiersConverged(t)
	}
	points := slices.Clone(crashPoints)
	sort.Strings(points)
	reached := 0
	for _, point := range points {
		for n := 1; n <= hits[point]; n++ {
			if testing.Short() && n != 1 && n != hits[point] {
				continue
			}
			reached++
			t.Run(fmt.Sprintf("%s#%d", point, n), func(t *testing.T) {
				h, p := tiersCrashCase(t)
				j := h.newJob(jobs.TypeSync, false, p)
				crashed, _, err := runWithHook(h, j, faultinject.CrashAt(point, n))
				if !crashed {
					t.Fatalf("did not crash at %s #%d (err %v)", point, n, err)
				}
				j.Attempt, j.Trigger = 2, jobs.TriggerResume
				if _, err := h.sync.Run(h.ctx, j, h.env()); err != nil {
					t.Fatalf("resume: %v\n%s", err, h.rep.dump())
				}
				h.closeJob(j.ID)
				// A resumed plan holds no tier decisions: a retain waits for every file of its
				// folder that is not backed up (S6 amended); the next sync plans again and retains.
				h.mustSync(false, jobs.Params{})
				h.assertTiersConverged(t)
				if _, st, j2 := h.mustSync(false, jobs.Params{}); st.FilesPlanned != 0 {
					t.Errorf("a further sync planned %d items: %v", st.FilesPlanned, h.itemList(j2.ID))
				}
			})
		}
	}
	for _, p := range []string{"copy.afterRename", PointRetainAfterRename, PointRecordAfterFS, PointRecordAfterDB} {
		if hits[p] == 0 {
			t.Errorf("the scenario never reaches %s", p)
		}
	}
	if reached == 0 {
		t.Fatal("no crash point reached")
	}
}

// TestTiersResumedRetainWaits: a crash after planning, then a resume, with a manifest-tier file in
// the retain's folder: the retain waits with a warning, and the next sync retains (§15).
func TestTiersResumedRetainWaits(t *testing.T) {
	h, p := tiersCrashCase(t)
	j := h.newJob(jobs.TypeSync, false, p)
	if crashed, _, _ := runWithHook(h, j, faultinject.CrashAt("copy.beforeTemp", 1)); !crashed {
		t.Fatal("no crash")
	}
	j.Attempt, j.Trigger = 2, jobs.TriggerResume
	res, err := h.sync.Run(h.ctx, j, h.env())
	if err != nil {
		t.Fatal(err)
	}
	h.closeJob(j.ID)
	if res.Warnings == 0 {
		t.Fatalf("the resumed retain did not wait: %+v", res)
	}
	var waited bool
	for _, it := range h.items(j.ID) {
		if it.RelPath == "movies/X/old.mkv" && it.Status == jobs.ItemFailed && strings.Contains(it.Error, "X/new.mkv in the same folder is not backed up") {
			waited = true
		}
	}
	if !waited {
		t.Fatalf("items %v", h.itemList(j.ID))
	}
	if _, ok := h.liveRecord("movies/X/old.mkv"); !ok {
		t.Fatal("the old version left while its retain waited")
	}
	_, st, _ := h.mustSync(false, jobs.Params{})
	if st.FilesRetained != 1 {
		t.Fatalf("the next sync %+v", st)
	}
	h.assertTiersConverged(t)
}
