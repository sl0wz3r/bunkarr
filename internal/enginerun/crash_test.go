package enginerun

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/enginetest"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
	"github.com/sl0wz3r/bunkarr/internal/engines/restic"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/syncer"
)

// The crash matrix of the engines (§14.3): a restic and an rclone sync with a change set that
// includes an equal-size update, files deleted after the scan and a held update crash at every
// occurrence of every point, resume, and must converge.

// syncCrashPoints are the step boundaries of an engine sync.
var syncCrashPoints = []string{PointBeforeBatch, PointAfterBatchExit, PointAfterRecord, PointRcloneAfterIntent, PointRcloneAfterMove,
	PointRcloneAfterStat}

// crashScenario builds the destination for a crash case: an initial sync, then the change set.
// The files deleted after the scan vanish during the first backup or copy of the job.
type crashScenario struct {
	h *harness
	// versions are contents that must survive somewhere at the destination (S5).
	versions []string
	once     sync.Once
}

func newCrashScenario(t *testing.T, kind engines.Kind) *crashScenario {
	settings := &destinations.Settings{MaxChangeFiles: 1000, MaxChangePercent: 100}
	if kind == engines.Restic {
		settings.Restic = &destinations.ResticSettings{BatchFiles: 2}
	} else {
		settings.Rclone = &destinations.RcloneSettings{BatchFiles: 2}
	}
	h := newHarness(t, harnessOptions{kind: kind, settings: settings})
	c := &crashScenario{h: h}
	h.writeSrc("eq.mkv", content("e1", 2000), 1)
	h.writeSrc("upd.mkv", content("u1", 1800), 2)
	h.writeSrc("held/big.mkv", content("b1", 4000), 3)
	h.writeSrc("del.mkv", content("d1", 1500), 4)
	h.writeSrc("mv.mkv", content("m1", 1600), 5)
	h.writeSrc("vanish-old.mkv", content("v1", 1100), 6)
	h.writeSrc("keep/k.mkv", content("k1", 900), 7)
	h.mustSync(jobs.Params{})
	c.versions = []string{content("e1", 2000), content("u1", 1800), content("b1", 4000), content("d1", 1500), content("m1", 1600),
		content("v1", 1100)}
	// The change set.
	h.writeSrc("eq.mkv", content("E2", 2000), 20)     // equal-size update
	h.writeSrc("upd.mkv", content("u2", 1900), 21)    // update
	h.writeSrc("held/big.mkv", content("B", 100), 22) // held update (shrink), alone in its folder (S6)
	h.removeSrc("del.mkv")                            // retain
	h.renameSrc("mv.mkv", "moved/mv.mkv")             // move
	h.writeSrc("new.mkv", content("n1", 1300), 23)    // copy
	h.writeSrc("new2.mkv", content("n2", 1350), 25)   // copy (a second batch)
	// A new file that vanishes after the scan: alone in its folder, so its failed copy does not
	// make the root folder's retains wait (S6).
	h.writeSrc("fresh/vanish-new.mkv", content("vn", 1200), 24)
	vanish := func() {
		c.once.Do(func() {
			_ = os.Remove(h.srcPath("fresh/vanish-new.mkv"))
			_ = os.Remove(h.srcPath("vanish-old.mkv"))
		})
	}
	if kind == engines.Restic {
		h.restic.beforeBackup = func(*enginetest.Call) { vanish() }
	} else {
		h.rclone.beforeCopy = func([]string) { vanish() }
	}
	h.advance(time1h)
	return c
}

// crashHook panics at the n-th occurrence of point; at rclone.afterMove the fake first undoes the
// move's delete, as a crash between a server-side copy and its delete leaves both objects.
func (c *crashScenario) crashHook(point string, n int) func(string) {
	seen := 0
	return func(name string) {
		if name != point {
			return
		}
		seen++
		if seen != n {
			return
		}
		if point == PointRcloneAfterMove && c.h.rclone != nil {
			c.h.rclone.undoLastDelete()
		}
		panic(faultinject.Crash{Point: name})
	}
}

// countPoints runs the scenario without a crash and counts the points.
func countPoints(t *testing.T, kind engines.Kind) map[string]int {
	c := newCrashScenario(t, kind)
	hits := map[string]int{}
	var mu sync.Mutex
	faultinject.SetHook(func(name string) {
		mu.Lock()
		hits[name]++
		mu.Unlock()
	})
	defer faultinject.SetHook(nil)
	c.h.mustSync(jobs.Params{})
	return hits
}

func TestCrashMatrixEngines(t *testing.T) {
	for _, kind := range []engines.Kind{engines.Restic, engines.Rclone} {
		hits := countPoints(t, kind)
		t.Logf("%s: %v", kind, hits)
		for _, point := range syncCrashPoints {
			for n := 1; n <= hits[point]; n++ {
				t.Run(fmt.Sprintf("%s/%s#%d", kind, point, n), func(t *testing.T) {
					c := newCrashScenario(t, kind)
					h := c.h
					j := h.newJob(jobs.TypeSync, false, jobs.Params{})
					crashed, _, err := h.runWithHook(j, c.crashHook(point, n))
					if !crashed {
						t.Fatalf("did not crash at %s #%d (err %v)", point, n, err)
					}
					for attempt := 2; attempt <= 4; attempt++ {
						j.Attempt, j.Trigger = attempt, jobs.TriggerResume
						if _, err = h.runJob(j); err == nil {
							break
						}
					}
					h.closeJob(j.ID)
					if err != nil {
						t.Fatalf("resume: %v\n%s", err, h.rep.dump())
					}
					c.assertConverged(t)
					// Two more syncs settle what the change set left (the vanished recorded file is
					// retained); the second plans nothing but the held update.
					h.advance(time1h)
					h.mustSync(jobs.Params{})
					c.assertConverged(t)
					h.advance(time1h)
					_, j2 := h.mustSync(jobs.Params{})
					for _, it := range h.items(j2.ID) {
						if it.Status != jobs.ItemHeld || !strings.HasSuffix(it.RelPath, "big.mkv") {
							t.Errorf("a further sync planned %s %s (%s)", it.Action, it.RelPath, it.Status)
						}
					}
				})
			}
		}
	}
}

// assertConverged checks the destination against the records after a recovered job.
func (c *crashScenario) assertConverged(t *testing.T) {
	t.Helper()
	h := c.h
	if h.restic != nil {
		h.assertResticRefs(t)
		// Nothing partial: every recorded snapshot exists; no version is lost (S5): each old
		// version is in a snapshot a record references or in the base.
		rows, _ := h.svc.snapshotRows(h.ctx, h.dest.ID)
		for _, r := range rows {
			if h.restic.snapshot(r.SnapshotID) == nil {
				t.Errorf("recorded snapshot %s does not exist", short(r.SnapshotID))
			}
		}
		kept := map[string]bool{}
		for _, id := range basesOf(rows) {
			kept[id] = true
		}
		for _, r := range h.records() {
			if r.EngineRef != "" {
				kept[r.EngineRef] = true
			}
		}
		for _, v := range c.versions {
			found := false
			for id := range kept {
				if s := h.restic.snapshot(id); s != nil {
					for _, o := range s.files {
						found = found || string(o.data) == v
					}
				}
			}
			if !found {
				t.Errorf("a version of %d bytes is in no kept snapshot", len(v))
			}
		}
		return
	}
	// rclone: every record matches an object, every object is recorded, no version was lost, and
	// nothing was deleted.
	objs := map[string]bool{}
	for _, p := range h.rclone.paths("") {
		objs[p] = true
	}
	recorded := map[string]bool{}
	for _, r := range h.records() {
		switch r.State {
		case syncer.StatePresent:
			o, ok := h.rclone.get(r.RelPath)
			if !ok || int64(len(o.data)) != r.Size {
				t.Errorf("present %s: object %v", r.RelPath, ok)
			}
			recorded[r.RelPath] = true
		case syncer.StateRetained:
			o, ok := h.rclone.get(r.RetainedPath)
			if !ok || int64(len(o.data)) != r.Size {
				t.Errorf("retained %s: object %v (%d bytes recorded)", r.RetainedPath, ok, r.Size)
			}
			recorded[r.RetainedPath] = true
		case syncer.StateMissing:
			recorded[r.RelPath] = true // its object (if any) is replaced by the next sync
		}
		if r.State.Live() && r.RetainedPath != "" {
			t.Errorf("an intent is left on %s", r.RelPath)
		}
	}
	for p := range objs {
		if strings.HasPrefix(p, filecopy.MetaDir+"/") && !strings.HasPrefix(p, filecopy.RetentionRoot+"/") {
			continue
		}
		if !recorded[p] {
			t.Errorf("unrecorded object %s", p)
		}
	}
	for _, v := range c.versions {
		found := false
		for p := range objs {
			if o, _ := h.rclone.get(p); string(o.data) == v {
				found = true
			}
		}
		if !found {
			t.Errorf("a version of %d bytes was lost", len(v))
		}
	}
	for _, sub := range []string{"delete", "deletefile", "purge"} {
		if n := len(h.callsOf(proc.Rclone, sub)); n != 0 {
			t.Errorf("a sync ran rclone %s", sub)
		}
	}
}

// TestCrashMatrixRetention: a crash after any forget chunk (with held items and replaced rows)
// never makes a later run forget a referenced, base or newest snapshot.
func TestCrashMatrixRetention(t *testing.T) {
	build := func(t *testing.T) (*harness, []string) {
		h := newHarness(t, harnessOptions{kind: engines.Restic, settings: &destinations.Settings{MaxChangeFiles: 1000, MaxChangePercent: 100},
			retention: &destinations.Retention{DeletedDays: 30, SnapshotDaily: intp(0), SnapshotWeekly: intp(0), SnapshotMonthly: intp(0),
				SnapshotYearly: intp(0)}})
		h.firstSnapshot(map[string]string{"held.mkv": content("h", 4000), "upd.mkv": content("u", 1000), "gone.mkv": content("g", 900)})
		h.writeSrc("held.mkv", content("H", 10), 30)  // held: its version stays referenced
		h.writeSrc("upd.mkv", content("U", 1100), 31) // a replaced row
		h.removeSrc("gone.mkv")                       // a retained row
		h.advance(time1h)
		h.mustSync(jobs.Params{})
		// 150 unrecorded snapshots of this source (a crashed, cancelled job's batches): two chunks.
		tags, _ := restic.Tags(restic.TagInput{EngineTag: h.dest.EngineTag, Kind: engines.VersionMedia, JobID: 999, SourceID: h.src.ID, Batch: 1})
		var extra []string
		for i := range 150 {
			extra = append(extra, h.restic.addSnapshot(h.now().Add(-time.Duration(200-i)*time.Hour), tags, nil))
		}
		h.advance(time1h)
		return h, extra
	}
	protected := func(h *harness) []string {
		rows, _ := h.svc.snapshotRows(h.ctx, h.dest.ID)
		var out []string
		for _, id := range basesOf(rows) {
			out = append(out, id)
		}
		for _, r := range h.records() {
			if r.EngineRef != "" {
				out = append(out, r.EngineRef)
			}
		}
		return out
	}
	for n := 1; n <= 2; n++ {
		t.Run(fmt.Sprintf("afterForget#%d", n), func(t *testing.T) {
			h, extra := build(t)
			j := h.newJob(jobs.TypeRetention, false, jobs.Params{})
			// check runs a step and asserts it forgot none of the snapshots protected before it.
			check := func(step string, run func()) {
				t.Helper()
				keep := protected(h)
				run()
				ids := h.restic.ids()
				for _, id := range keep {
					if !slices.Contains(ids, id) {
						t.Errorf("%s: a protected snapshot %s was forgotten", step, short(id))
					}
				}
			}
			check("crashed run", func() {
				crashed, _, err := h.runWithHook(j, faultinject.CrashAt(PointResticAfterForget, n))
				if !crashed {
					t.Fatalf("no crash (%v)", err)
				}
			})
			// A sync may run before the retention job resumes.
			h.writeSrc("later.mkv", content("l", 500), 40)
			h.advance(time1h)
			h.mustSync(jobs.Params{})
			check("resumed run", func() {
				j.Attempt, j.Trigger = 2, jobs.TriggerResume
				if _, err := h.runJob(j); err != nil {
					t.Fatalf("resume: %v\n%s", err, h.rep.dump())
				}
				h.closeJob(j.ID)
			})
			h.advance(time1h)
			check("later run", func() { h.mustRun(jobs.TypeRetention, false, jobs.Params{}) })
			ids := h.restic.ids()
			for _, id := range extra {
				if slices.Contains(ids, id) {
					t.Errorf("an unprotected snapshot %s is left", short(id))
					break
				}
			}
			h.assertResticRefs(t)
		})
	}
}
