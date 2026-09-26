package syncer

import (
	"fmt"
	"slices"
	"sort"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// targetedCrashPaths are the paths of the targeted sync of TestCrashMatrixTargeted.
var targetedCrashPaths = []string{"Heat (1995)", "Show/Season 1"}

// targetedCrashSetup syncs the first tree fully, then changes it: inside the paths an upgrade
// (the 720p name vanishes, a 1080p name appears), a rename, a same-path replacement, a new
// hardlinked pair and a vanished name in a folder with nothing new (D14); outside them a new file
// and a deleted one that the targeted sync must not touch.
func targetedCrashSetup(t *testing.T, c crashConfig) *harness {
	h := newHarness(t)
	c.apply(h)
	h.writeSrc("Heat (1995)/Heat 720p.mkv", content("h7", 2000), 1)
	h.writeSrc("Heat (1995)/Heat.en.srt", content("s", 300), 2)
	h.writeSrc("Show/Season 1/E01.mkv", content("e1", 1500), 3)
	h.writeSrc("Show/Season 1/E02.mkv", content("e2", 1600), 4)
	h.writeSrc("Show/Season 1/Old/E00.mkv", content("e0", 1400), 5)
	h.writeSrc("Other/o.mkv", content("o", 1700), 6)
	h.writeSrc("Other/gone.mkv", content("g", 1800), 7)
	h.firstSync()

	h.removeSrc("Heat (1995)/Heat 720p.mkv")
	h.writeSrc("Heat (1995)/Heat 1080p.mkv", content("h10", 2600), 10)      // upgrade: copy, then retain
	h.renameSrc("Show/Season 1/E01.mkv", "Show/Season 1/Show - S01E01.mkv") // rename: move
	h.writeSrc("Show/Season 1/E02.mkv", content("E2", 1650), 11)            // same-path replacement: update
	h.writeSrc("Show/Season 1/E03.mkv", content("e3", 1550), 12)            // copy + link
	h.linkSrc("Show/Season 1/E03.mkv", "Show/Season 1/E03-link.mkv")        //
	h.removeSrc("Show/Season 1/Old/E00.mkv")                                // D14: kept
	h.writeSrc("Other/new.mkv", content("n", 1900), 13)                     // outside: untouched
	h.removeSrc("Other/gone.mkv")                                           //
	return h
}

// TestCrashMatrixTargeted is the crash matrix for a targeted sync with an upgrade, a rename, an
// update, a new hardlink pair and a D14 deferral inside its paths: a crash at every occurrence of
// every fault point, a resume of the same job, and then the destination holds every version, the
// targeted sync converged (another one plans nothing), nothing outside the paths was touched, and
// a full sync afterwards converges the whole source.
func TestCrashMatrixTargeted(t *testing.T) {
	points := slices.Clone(crashPoints)
	sort.Strings(points)
	covered := map[string]bool{}
	for _, c := range crashConfigs {
		hits := map[string]int{}
		{
			h := targetedCrashSetup(t, c)
			j := h.targetedJob(jobs.TriggerWebhook, false, targetedCrashPaths...)
			crashed, res, err := runWithHook(h, j, counting(hits))
			if crashed || err != nil || res.Warnings != 0 {
				t.Fatalf("%s: clean run: %v %+v\n%s", c.name, err, res, h.rep.dump())
			}
			st := res.Stats.(SyncStats)
			if st.FilesCopied != 2 || st.FilesMoved != 1 || st.FilesUpdated != 1 || st.FilesRetained != 1 || st.RetainsDeferred != 1 {
				t.Fatalf("%s: clean run stats %+v", c.name, st)
			}
		}
		for _, point := range points {
			for n := 1; n <= hits[point]; n++ {
				if testing.Short() && n != 1 && n != hits[point] {
					continue
				}
				covered[point] = true
				t.Run(fmt.Sprintf("%s/%s#%d", c.name, point, n), func(t *testing.T) {
					h := targetedCrashSetup(t, c)
					j := h.targetedJob(jobs.TriggerWebhook, false, targetedCrashPaths...)
					if crashed, _, err := runWithHook(h, j, faultinject.CrashAt(point, n)); !crashed {
						t.Fatalf("did not crash at %s #%d (err %v)", point, n, err)
					}
					j.Attempt, j.Trigger = 2, jobs.TriggerResume
					res, err := h.sync.Run(h.ctx, j, h.env())
					if err != nil {
						t.Fatalf("resume: %v\n%s", err, h.rep.dump())
					}
					h.closeJob(j.ID)
					if res.Warnings != 0 {
						t.Errorf("resume warnings %d: %s\n%s", res.Warnings, res.Summary, h.rep.dump())
					}
					for _, it := range h.items(j.ID) {
						if it.Status == jobs.ItemPending || it.Status == jobs.ItemFailed {
							t.Errorf("item %+v", it)
						}
					}
					for _, v := range []string{content("h7", 2000), content("e2", 1600), content("e0", 1400), content("g", 1800)} {
						if !h.hasContent(t, v) {
							t.Errorf("lost content %.12q...", v)
						}
					}
					// Outside the paths: nothing copied, nothing retained.
					if exists(h.dstPath("movies/Other/new.mkv")) || !exists(h.dstPath("movies/Other/gone.mkv")) {
						t.Error("the targeted sync touched a file outside its paths")
					}
					if _, ok := h.liveRecord("movies/Show/Season 1/Old/E00.mkv"); !ok {
						t.Error("D14: the vanished name of a folder without new content was not kept")
					}
					if _, st, j2 := h.mustTargeted(targetedCrashPaths...); st.FilesPlanned != 0 {
						t.Errorf("a further targeted sync planned %d items: %+v", st.FilesPlanned, h.items(j2.ID))
					}
					if _, st, j2 := h.mustSync(false, jobs.Params{}); st.FilesCopied != 1 || st.FilesRetained != 2 || st.FilesFailed != 0 {
						t.Errorf("the full sync after it: %+v items %+v", st, h.items(j2.ID))
					}
					h.assertConverged(t)
				})
			}
		}
	}
	for _, p := range crashPoints {
		// A targeted sync with an empty destination path never displaces or adopts.
		if !covered[p] && p != PointDisplaceAfterRename && p != PointAdoptAfterSetMtime && p != PointPromoteAfterRename {
			t.Errorf("fault point %s was never reached by the targeted scenario", p)
		}
	}
}
