package syncer

import (
	"fmt"
	"os"
	"path"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// crashPoints are the step boundaries a sync passes (design §9 crash matrix).
var crashPoints = []string{
	"scan.afterBatch",
	PointPlanAfterBatch,
	"copy.beforeTemp", "copy.afterWrite", "copy.afterSync", "copy.afterChtimes", "copy.afterRename",
	PointUpdateAfterLinkOld, PointUpdateAfterRenameOld, PointUpdateAfterRenameNew,
	"link.afterLink", "move.afterRename",
	PointDisplaceAfterRename, PointRetainAfterRename, PointPromoteAfterRename, PointAdoptAfterSetMtime,
	PointRecordAfterFS, PointRecordAfterDB,
}

// crashConfig is a destination configuration the matrix runs under.
type crashConfig struct {
	name  string
	caps  bool // capabilities.hardlinks
	mode  destinations.HardlinkMode
	adopt destinations.AdoptMode
	// unstable: the destination numbers inodes per lookup (capabilities.unstableInodes, a CIFS
	// mount with noserverino): hardlinks work, their names never show the same inode number.
	unstable bool
}

var crashConfigs = []crashConfig{
	{"recreate", true, destinations.HardlinksRecreate, destinations.AdoptSizeMtime, false},
	{"copy", true, destinations.HardlinksCopy, destinations.AdoptSizeMtime, false},
	{"no-hardlinks", false, destinations.HardlinksRecreate, destinations.AdoptSizeHash, false},
	{"unstable-inodes", true, destinations.HardlinksRecreate, destinations.AdoptSizeMtime, true},
}

func (c crashConfig) apply(h *harness) {
	if c.unstable {
		h.inventInodes()
	}
	h.setCaps(func(cp *filecopy.Capabilities) { cp.Hardlinks, cp.UnstableInodes = c.caps, c.unstable })
	h.setSettings(func(s *destinations.Settings) {
		s.Hardlinks, s.AdoptExisting = c.mode, c.adopt
		s.Verify.Mode = destinations.VerifyFull // re-read every temp file too
		s.MaxChangeFiles = 1000
		s.MaxChangePercent = 100
	})
}

// unmanaged puts a file Bunkarr has no record for at the destination: junk is displaced; a copy of
// the source file is adopted (with the source's mtime, or another one in size+hash mode).
func (h *harness) unmanaged(c crashConfig, rel, data string, n int) {
	mt := baseTime.Add(time.Duration(n) * time.Second)
	if c.adopt == destinations.AdoptSizeHash {
		mt = mt.Add(-time.Hour)
	}
	writeFileAt(h.t, h.dstPath("movies/"+rel), data, mt)
}

// writeInPlace changes a file's content without a new inode (every hardlink sees it).
func (h *harness) writeInPlace(rel, data string, n int) {
	h.t.Helper()
	if err := os.WriteFile(h.srcPath(rel), []byte(data), 0o644); err != nil {
		h.t.Fatal(err)
	}
	mt := baseTime.Add(time.Duration(n) * time.Second)
	if err := os.Chtimes(h.srcPath(rel), mt, mt); err != nil {
		h.t.Fatal(err)
	}
}

// crashSync1 is the first sync's tree: new files, hardlinked pairs, an unmanaged file in the way
// and one to adopt.
func crashSync1(h *harness, c crashConfig) {
	h.writeSrc("a.mkv", content("a1", 3000), 1)
	h.writeSrc("b.mkv", content("b", 2000), 2)
	h.writeSrc("c.mkv", content("c", 1500), 3)
	h.writeSrc("d/p1.mkv", content("p", 1800), 4)
	h.linkSrc("d/p1.mkv", "d/p2.mkv")
	h.writeSrc("e.mkv", content("e", 900), 5)
	h.writeSrc("g/h1.mkv", content("h", 1600), 6)
	h.linkSrc("g/h1.mkv", "g/h2.mkv")
	h.writeSrc("s1.mkv", content("s", 1100), 7)
	h.linkSrc("s1.mkv", "s2.mkv")
	h.writeSrc("z.mkv", content("z", 1000), 8)
	h.unmanaged(c, "z.mkv", "junk-z", 8)
	h.writeSrc("w.mkv", content("w", 1050), 9)
	h.unmanaged(c, "w.mkv", content("w", 1050), 9)
}

// crashSync2 changes the tree for the second sync: every action of design §4.2.
func crashSync2(h *harness, c crashConfig) {
	h.writeSrc("a.mkv", content("a2", 3300), 20)     // update
	h.renameSrc("b.mkv", "moved/b.mkv")              // move
	h.removeSrc("c.mkv")                             // retain
	h.removeSrc("d/p1.mkv")                          // promote (the primary vanished)
	h.writeSrc("g/h1.mkv", content("H", 1700), 21)   // group split: promote + update
	h.writeInPlace("s1.mkv", content("S", 1150), 22) // both names changed: update + relink
	h.writeSrc("new.mkv", content("n", 1300), 23)    // copy
	h.writeSrc("n1.mkv", content("q", 1400), 24)     // copy + link
	h.linkSrc("n1.mkv", "n2.mkv")                    //
	h.writeSrc("x.mkv", content("x", 1200), 25)      // displace + copy
	h.unmanaged(c, "x.mkv", "junk-x", 25)            //
	h.writeSrc("y.mkv", content("y", 1250), 26)      // adopt
	h.unmanaged(c, "y.mkv", content("y", 1250), 26)  //
}

// oldVersions are contents that must survive somewhere at the destination after sync 2.
var oldVersions = []string{content("a1", 3000), content("c", 1500), content("p", 1800), content("h", 1600),
	content("s", 1100), "junk-x", "junk-z", content("b", 2000)}

// counting returns a hook that counts how often each point is reached.
func counting(hits map[string]int) func(string) {
	return func(name string) { hits[name]++ }
}

// runWithHook runs one sync attempt with hook installed and reports whether it crashed.
func runWithHook(h *harness, j jobs.Job, hook func(string)) (crashed bool, res jobs.Result, err error) {
	faultinject.SetHook(hook)
	defer faultinject.SetHook(nil)
	defer func() {
		if p := recover(); p != nil {
			if _, ok := p.(faultinject.Crash); !ok {
				panic(p)
			}
			crashed = true
		}
	}()
	res, err = h.sync.Run(h.ctx, j, h.env())
	return false, res, err
}

// crashCase sets up a harness at the given phase (1: the first sync, 2: the change sync).
func crashCase(t *testing.T, c crashConfig, phase int) *harness {
	h := newHarness(t)
	c.apply(h)
	crashSync1(h, c)
	if phase == 2 {
		if _, st, _ := h.mustSync(false, jobs.Params{}); st.FilesFailed != 0 {
			t.Fatalf("sync 1 failed items: %+v\n%s", st, h.rep.dump())
		}
		crashSync2(h, c)
	}
	return h
}

// TestCrashMatrix crashes a sync at every occurrence of every fault point, resumes the same job
// (attempt 2, the plan as persisted) and checks that the destination converged: every live source
// file is recorded with its content at the destination, nothing is left in a temp file, the
// records match the filesystem, every replaced or deleted version is in retention, and a further
// sync has nothing to do.
func TestCrashMatrix(t *testing.T) {
	covered := map[string]bool{}
	for _, c := range crashConfigs {
		for _, phase := range []int{1, 2} {
			// A clean run tells how often each point is reached, and what it displaces.
			hits := map[string]int{}
			var displaced []string
			{
				h := crashCase(t, c, phase)
				j := h.newJob(jobs.TypeSync, false, jobs.Params{})
				if _, _, err := runWithHook(h, j, counting(hits)); err != nil {
					t.Fatalf("%s phase %d: clean run: %v", c.name, phase, err)
				}
				displaced = h.displaced(t)
			}
			points := slices.Clone(crashPoints)
			sort.Strings(points)
			for _, point := range points {
				for n := 1; n <= hits[point]; n++ {
					if testing.Short() && n != 1 && n != hits[point] {
						continue // -short: the first and the last occurrence of each point
					}
					covered[point] = true
					name := fmt.Sprintf("%s/sync%d/%s#%d", c.name, phase, point, n)
					t.Run(name, func(t *testing.T) {
						h := crashCase(t, c, phase)
						j := h.newJob(jobs.TypeSync, false, jobs.Params{})
						crashed, _, err := runWithHook(h, j, faultinject.CrashAt(point, n))
						if !crashed {
							t.Fatalf("did not crash at %s #%d (err %v)", point, n, err)
						}
						// Resume: same job, attempt 2, whatever the plan state is.
						j.Attempt, j.Trigger = 2, jobs.TriggerResume
						res, err := h.sync.Run(h.ctx, j, h.env())
						if err != nil {
							t.Fatalf("resume: %v\n%s", err, h.rep.dump())
						}
						h.closeJob(j.ID)
						if res.Warnings != 0 {
							t.Errorf("resume warnings %d: %s\n%s", res.Warnings, res.Summary, h.rep.dump())
						}
						h.assertConverged(t)
						// A resumed item never takes a file Bunkarr put there for one in the way.
						if got := h.displaced(t); !slices.Equal(got, displaced) {
							t.Errorf("displaced %v, a clean run displaces %v\n%s", got, displaced, h.rep.dump())
						}
						if phase == 2 {
							for _, v := range oldVersions {
								if !h.hasContent(t, v) {
									t.Errorf("lost content %.12q...", v)
								}
							}
						} else if !h.hasContent(t, "junk-z") {
							t.Error("lost the displaced file")
						}
						// Converged: another sync has nothing to do.
						_, st, j2 := h.mustSync(false, jobs.Params{})
						if st.FilesPlanned != 0 {
							t.Errorf("a further sync planned %d items: %+v", st.FilesPlanned, h.items(j2.ID))
						}
					})
				}
			}
		}
	}
	for _, p := range crashPoints {
		if !covered[p] {
			t.Errorf("fault point %s was never reached by the scenarios", p)
		}
	}
}

// displaced returns the contents (sha256) of the files recorded as displaced into retention,
// sorted.
func (h *harness) displaced(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, r := range h.records() {
		if r.State == StateRetained && r.Reason == ReasonDisplaced {
			out = append(out, fileHash(t, h.dstPath(r.RetainedPath)))
		}
	}
	sort.Strings(out)
	return out
}

// recordsLinks reports whether the configuration records hardlinks instead of making them
// (link_recorded rows, no file of their own).
func (c crashConfig) recordsLinks() bool { return !c.caps || c.mode != destinations.HardlinksRecreate }

// resumeScenario is a crash-matrix scenario for resume paths the change sync of TestCrashMatrix
// does not reach.
type resumeScenario struct {
	name string
	// linkRecorded limits the scenario to configurations that record links (recordsLinks).
	linkRecorded bool
	// setup runs the first sync and changes the trees for the sync that crashes.
	setup func(h *harness, c crashConfig)
	// between changes the source after the crash, before the resume (and a scan sees it); the
	// destination then converges with one more sync.
	between func(h *harness)
	// resumeMayFail is part of the error of items the resume may fail because of the change
	// between (the next sync resolves them).
	resumeMayFail string
	// keep are contents that must be at the destination afterwards.
	keep []string
}

// firstSync runs a sync that must not fail any item.
func (h *harness) firstSync() {
	h.t.Helper()
	if _, st, j := h.mustSync(false, jobs.Params{}); st.FilesFailed != 0 || st.FilesHeld != 0 {
		h.t.Fatalf("setup sync: %+v items %+v\n%s", st, h.items(j.ID), h.rep.dump())
	}
}

// pairSrc writes p1.mkv with a hardlink p2.mkv and a file that keeps the source from being empty.
func (h *harness) pairSrc() {
	h.writeSrc("k.mkv", content("k", 500), 1)
	h.writeSrc("p1.mkv", content("p", 1800), 4)
	h.linkSrc("p1.mkv", "p2.mkv")
}

var resumeScenarios = []resumeScenario{
	{
		// An unmanaged file at the new name of a rename is displaced by the move.
		name: "displace-by-move",
		setup: func(h *harness, c crashConfig) {
			h.writeSrc("k.mkv", content("k", 500), 1)
			h.writeSrc("b.mkv", content("b", 2000), 2)
			h.firstSync()
			h.renameSrc("b.mkv", "c.mkv")
			h.unmanaged(c, "c.mkv", "junk-c", 2)
		},
		keep: []string{content("b", 2000), "junk-c"},
	},
	{
		// An unmanaged file at the second name of a new hardlink group is displaced by the link.
		name: "displace-by-link",
		setup: func(h *harness, c crashConfig) {
			h.writeSrc("k.mkv", content("k", 500), 1)
			h.firstSync()
			h.writeSrc("n1.mkv", content("q", 1400), 3)
			h.linkSrc("n1.mkv", "n2.mkv")
			h.unmanaged(c, "n2.mkv", "junk-n2", 3)
		},
		keep: []string{content("q", 1400), "junk-n2"},
	},
	{
		// The primary of a recorded-only link vanishes while an unmanaged file sits at the link's
		// path: the promote displaces it.
		name:         "displace-by-promote",
		linkRecorded: true,
		setup: func(h *harness, c crashConfig) {
			h.pairSrc()
			h.firstSync()
			h.unmanaged(c, "p2.mkv", "junk-p2", 5)
			h.removeSrc("p1.mkv")
		},
		keep: []string{content("p", 1800), "junk-p2"},
	},
	{
		// Verify found the primary of a recorded-only link gone: the sync copies it again.
		name:         "missing-primary",
		linkRecorded: true,
		setup: func(h *harness, c crashConfig) {
			h.pairSrc()
			h.firstSync()
			if err := os.Remove(h.dstPath("movies/p1.mkv")); err != nil {
				h.t.Fatal(err)
			}
			if _, st, _ := h.mustVerify(); st.FilesMissing != 1 {
				h.t.Fatalf("verify: %+v", st)
			}
		},
		keep: []string{content("p", 1800)},
	},
	{
		// Verify found the primary of a recorded-only link damaged: the sync copies it again and
		// keeps the damaged version in retention.
		name:         "damaged-primary",
		linkRecorded: true,
		setup: func(h *harness, c crashConfig) {
			h.pairSrc()
			h.firstSync()
			writeFileAt(h.t, h.dstPath("movies/p1.mkv"), content("X", 1800), baseTime.Add(4*time.Second))
			if _, st, _ := h.mustVerify(); st.FilesMissing != 1 {
				h.t.Fatalf("verify: %+v", st)
			}
		},
		keep: []string{content("p", 1800), content("X", 1800)},
	},
	{
		// Verify found the primary of a recorded-only link gone, and the primary's source was
		// replaced since (a group split): the link gets its own copy of the old content, then the
		// primary is copied again with the new one.
		name:         "split-link-of-missing-primary",
		linkRecorded: true,
		setup: func(h *harness, c crashConfig) {
			h.pairSrc()
			h.firstSync()
			if err := os.Remove(h.dstPath("movies/p1.mkv")); err != nil {
				h.t.Fatal(err)
			}
			if _, st, _ := h.mustVerify(); st.FilesMissing != 1 {
				h.t.Fatalf("verify: %+v", st)
			}
			h.writeSrc("p1.mkv", content("P", 1900), 6)
		},
		keep: []string{content("p", 1800), content("P", 1900)},
	},
	{
		// Every name of a hardlink group is deleted at the source.
		name: "group-vanishes",
		setup: func(h *harness, c crashConfig) {
			h.pairSrc()
			h.firstSync()
			h.removeSrc("p1.mkv")
			h.removeSrc("p2.mkv")
		},
		keep: []string{content("p", 1800)},
	},
	{
		// Every name of a hardlink group is deleted; the second name comes back before the resume.
		// A retain that already renamed the primary puts it back (the link still needs the content)
		// and fails; the next sync promotes it to the link's name.
		name: "group-vanishes-then-link-back",
		setup: func(h *harness, c crashConfig) {
			h.pairSrc()
			h.firstSync()
			h.removeSrc("p1.mkv")
			h.removeSrc("p2.mkv")
		},
		between:       func(h *harness) { h.writeSrc("p2.mkv", content("p", 1800), 4) },
		resumeMayFail: "still depend on the content",
		keep:          []string{content("p", 1800)},
	},
	{
		// Every name of a hardlink group is deleted; the first name comes back before the resume.
		name: "group-vanishes-then-primary-back",
		setup: func(h *harness, c crashConfig) {
			h.pairSrc()
			h.firstSync()
			h.removeSrc("p1.mkv")
			h.removeSrc("p2.mkv")
		},
		between: func(h *harness) { h.writeSrc("p1.mkv", content("p", 1800), 4) },
		keep:    []string{content("p", 1800)},
	},
	{
		// A deleted file comes back (restored, re-imported) before the resume.
		name: "retain-then-source-back",
		setup: func(h *harness, c crashConfig) {
			h.writeSrc("k.mkv", content("k", 500), 1)
			h.writeSrc("a.mkv", content("a", 3000), 2)
			h.firstSync()
			h.removeSrc("a.mkv")
		},
		between: func(h *harness) { h.writeSrc("a.mkv", content("a", 3000), 2) },
		keep:    []string{content("a", 3000)},
	},
	{
		// The primary of a recorded-only link vanishes; the link's own source file changes before
		// the resume.
		name:         "promote-then-link-changes",
		linkRecorded: true,
		setup: func(h *harness, c crashConfig) {
			h.pairSrc()
			h.firstSync()
			h.removeSrc("p1.mkv")
		},
		between: func(h *harness) { h.writeSrc("p2.mkv", content("P", 1900), 6) },
		keep:    []string{content("p", 1800), content("P", 1900)},
	},
	{
		// The old name of a renamed file comes back before the resume.
		name: "move-then-old-name-back",
		setup: func(h *harness, c crashConfig) {
			h.writeSrc("k.mkv", content("k", 500), 1)
			h.writeSrc("b.mkv", content("b", 2000), 2)
			h.firstSync()
			h.renameSrc("b.mkv", "c.mkv")
		},
		between: func(h *harness) { h.writeSrc("b.mkv", content("b", 2000), 2) },
		keep:    []string{content("b", 2000)},
	},
}

// TestCrashMatrixResumePaths is the crash matrix of TestCrashMatrix for resumeScenarios: every
// occurrence of every fault point, then a resume of the same job, which must end without
// warnings or pending items, and a destination that converged (after one more sync when the
// source changed in between) and keeps every version.
func TestCrashMatrixResumePaths(t *testing.T) {
	points := slices.Clone(crashPoints)
	sort.Strings(points)
	for _, sc := range resumeScenarios {
		for _, c := range crashConfigs {
			if sc.linkRecorded && !c.recordsLinks() {
				continue
			}
			setup := func(t *testing.T) *harness {
				h := newHarness(t)
				c.apply(h)
				sc.setup(h, c)
				return h
			}
			hits := map[string]int{}
			var displaced []string
			{
				h := setup(t)
				j := h.newJob(jobs.TypeSync, false, jobs.Params{})
				crashed, res, err := runWithHook(h, j, counting(hits))
				if crashed || err != nil || res.Warnings != 0 {
					t.Errorf("%s/%s: clean run: %v %+v\n%s", sc.name, c.name, err, res, h.rep.dump())
					continue
				}
				displaced = h.displaced(t)
			}
			for _, point := range points {
				for n := 1; n <= hits[point]; n++ {
					if testing.Short() && n != 1 && n != hits[point] {
						continue
					}
					t.Run(fmt.Sprintf("%s/%s/%s#%d", sc.name, c.name, point, n), func(t *testing.T) {
						h := setup(t)
						j := h.newJob(jobs.TypeSync, false, jobs.Params{})
						if crashed, _, err := runWithHook(h, j, faultinject.CrashAt(point, n)); !crashed {
							t.Fatalf("did not crash at %s #%d (err %v)", point, n, err)
						}
						if sc.between != nil {
							sc.between(h)
							h.rescan() // the catalog sees the change (execution consults it)
						}
						j.Attempt, j.Trigger = 2, jobs.TriggerResume
						res, err := h.sync.Run(h.ctx, j, h.env())
						if err != nil {
							t.Fatalf("resume: %v\n%s", err, h.rep.dump())
						}
						h.closeJob(j.ID)
						allowed := 0
						for _, it := range h.items(j.ID) {
							if it.Status == jobs.ItemPending {
								t.Errorf("item left pending: %+v", it)
							}
							if it.Status == jobs.ItemFailed && sc.resumeMayFail != "" && strings.Contains(it.Error, sc.resumeMayFail) {
								allowed++
							}
						}
						if res.Warnings != allowed {
							t.Errorf("resume warnings %d: %s\n%+v\n%s", res.Warnings, res.Summary, h.items(j.ID), h.rep.dump())
						}
						if sc.between != nil {
							if _, st, j2 := h.mustSync(false, jobs.Params{}); st.FilesFailed != 0 {
								t.Errorf("sync after the resume: %+v items %+v", st, h.items(j2.ID))
							}
						}
						h.assertConverged(t)
						if got := h.displaced(t); sc.between == nil && !slices.Equal(got, displaced) {
							t.Errorf("displaced %v, a clean run displaces %v\n%s", got, displaced, h.rep.dump())
						}
						for _, v := range sc.keep {
							if !h.hasContent(t, v) {
								t.Errorf("lost content %.12q...", v)
							}
						}
						if _, st, j2 := h.mustSync(false, jobs.Params{}); st.FilesPlanned != 0 {
							t.Errorf("a further sync planned %d items: %+v", st.FilesPlanned, h.items(j2.ID))
						}
					})
				}
			}
		}
	}
}

// rescan runs a scan of the test source (as a scan job between a crash and the resume would).
func (h *harness) rescan() {
	h.t.Helper()
	if _, err := h.scanner.Scan(h.ctx, h.src.ID, nil); err != nil {
		h.t.Fatal(err)
	}
}

// TestInterruptedRetainOfAGroupWhoseLinkComesBack: both names of a recorded-link group vanish, the
// primary's retain stops after its rename, and the link's name comes back (and is scanned) before
// the resume. The link still needs the content: the resumed retain puts the file back instead of
// leaving the primary recorded with no file, and the next sync promotes it to the link's name.
func TestInterruptedRetainOfAGroupWhoseLinkComesBack(t *testing.T) {
	for _, point := range []string{PointRetainAfterRename, PointRecordAfterFS} {
		t.Run(point, func(t *testing.T) {
			h := newHarness(t)
			h.setSettings(func(s *destinations.Settings) { s.Hardlinks = destinations.HardlinksCopy })
			h.pairSrc()
			h.firstSync()
			h.removeSrc("p1.mkv")
			h.removeSrc("p2.mkv")
			j := h.newJob(jobs.TypeSync, false, jobs.Params{})
			if crashed, _, err := runWithHook(h, j, faultinject.CrashAt(point, 1)); !crashed {
				t.Fatalf("no crash: %v", err)
			}
			h.writeSrc("p2.mkv", content("p", 1800), 4)
			h.rescan()
			j.Attempt, j.Trigger = 2, jobs.TriggerResume
			if _, err := h.run(h.sync, j); err != nil {
				t.Fatalf("resume: %v\n%s", err, h.rep.dump())
			}
			if r, ok := h.liveRecord("movies/p1.mkv"); ok && r.State == StatePresent && !exists(h.dstPath("movies/p1.mkv")) {
				t.Fatalf("p1 is recorded present with no file: items %+v", h.items(j.ID))
			}
			if _, st, j2 := h.mustSync(false, jobs.Params{}); st.FilesFailed != 0 {
				t.Fatalf("next sync: %+v items %+v", st, h.items(j2.ID))
			}
			h.assertConverged(t)
			if !h.hasContent(t, content("p", 1800)) {
				t.Fatal("lost the content")
			}
			if _, st, _ := h.mustSync(false, jobs.Params{}); st.FilesPlanned != 0 {
				t.Fatalf("not converged: %+v", st)
			}
		})
	}
}

// TestFatalErrorAfterARetainsRenamePutsTheFileBack: a retain whose rename an earlier attempt did
// stops with a fatal error (a hardlink's directory cannot be read). The job fails and is never
// resumed: the file goes back to its path instead of staying unrecorded in retention while the
// record says present.
func TestFatalErrorAfterARetainsRenamePutsTheFileBack(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	h := newHarness(t)
	h.writeSrc("k.mkv", content("k", 500), 1)
	h.writeSrc("a/p.mkv", content("p", 1800), 4)
	h.linkSrc("a/p.mkv", "b/q.mkv")
	h.firstSync()
	if r, _ := h.liveRecord("movies/b/q.mkv"); r.State != StateLinked {
		t.Fatalf("setup: q is %s", r.State)
	}
	h.removeSrc("a/p.mkv")
	h.removeSrc("b/q.mkv")
	j := h.newJob(jobs.TypeSync, false, jobs.Params{})
	if crashed, _, err := runWithHook(h, j, faultinject.CrashAt(PointRetainAfterRename, 1)); !crashed {
		t.Fatalf("no crash: %v", err)
	}
	dir := h.dstPath("movies/b")
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	j.Attempt, j.Trigger = 2, jobs.TriggerResume
	if _, err := h.run(h.sync, j); err == nil || filecopy.Classify(err) != filecopy.Fatal {
		t.Fatalf("resume: err = %v, want a fatal error", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if !exists(h.dstPath("movies/a/p.mkv")) {
		t.Fatal("the renamed file was not put back")
	}
	if _, st, j2 := h.mustSync(false, jobs.Params{}); st.FilesFailed != 0 || st.FilesRetained != 2 {
		t.Fatalf("next sync: %+v items %+v", st, h.items(j2.ID))
	}
	h.assertConverged(t)
	if !h.hasContent(t, content("p", 1800)) {
		t.Fatal("lost the content")
	}
}

// TestCrashTwice crashes the resume as well (at the first point it reaches after the first
// crash's point) and resumes a third time.
func TestCrashTwice(t *testing.T) {
	for _, c := range crashConfigs {
		for _, point := range []string{PointRecordAfterFS, "copy.afterWrite", PointUpdateAfterRenameNew, PointRetainAfterRename, PointPlanAfterBatch} {
			t.Run(c.name+"/"+point, func(t *testing.T) {
				h := crashCase(t, c, 2)
				j := h.newJob(jobs.TypeSync, false, jobs.Params{})
				if crashed, _, err := runWithHook(h, j, faultinject.CrashAt(point, 1)); !crashed {
					t.Skipf("point not reached (err %v)", err)
				}
				j.Attempt, j.Trigger = 2, jobs.TriggerResume
				_, _, _ = runWithHook(h, j, faultinject.CrashAt(PointRecordAfterFS, 2))
				j.Attempt = 3
				if _, err := h.sync.Run(h.ctx, j, h.env()); err != nil {
					t.Fatalf("third attempt: %v", err)
				}
				h.closeJob(j.ID)
				h.assertConverged(t)
				for _, v := range oldVersions {
					if !h.hasContent(t, v) {
						t.Errorf("lost content %.12q...", v)
					}
				}
				if _, st, _ := h.mustSync(false, jobs.Params{}); st.FilesPlanned != 0 {
					t.Errorf("a further sync planned %d items", st.FilesPlanned)
				}
			})
		}
	}
}

// TestFallbackCopyResumes crashes inside the copy a link fell back to: the resume finishes that
// copy (it does not switch back to linking and forget the temp file).
func TestFallbackCopyResumes(t *testing.T) {
	for _, point := range []string{"copy.afterWrite", "copy.afterRename", PointRecordAfterFS} {
		t.Run(point, func(t *testing.T) {
			h := newHarness(t)
			h.setCaps(func(c *filecopy.Capabilities) { c.InvalidChars = ":" })
			h.writeSrc("a:1.mkv", content("a", 500), 1) // the primary cannot be stored
			h.linkSrc("a:1.mkv", "b.mkv")
			j := h.newJob(jobs.TypeSync, false, jobs.Params{})
			if crashed, _, _ := runWithHook(h, j, faultinject.CrashAt(point, 1)); !crashed {
				t.Fatal("no crash")
			}
			j.Attempt, j.Trigger = 2, jobs.TriggerResume
			if _, err := h.run(h.sync, j); err != nil {
				t.Fatal(err)
			}
			for rel := range regularFiles(t, h.dstDir) {
				if filecopy.IsTempName(path.Base(rel)) {
					t.Fatalf("temp file left: %s", rel)
				}
			}
			if r, ok := h.liveRecord("movies/b.mkv"); !ok || r.State != StatePresent ||
				fileHash(t, h.dstPath("movies/b.mkv")) != strHash(content("a", 500)) {
				t.Fatalf("b.mkv: %+v", r)
			}
		})
	}
}

// TestHalfDoneRenamePairIsFinished crashes between the two renames of an update on a destination
// without hardlinks and removes the source file before the resume: the complete, verified temp
// file is still renamed into place (the old version is in retention), and recorded.
func TestHalfDoneRenamePairIsFinished(t *testing.T) {
	h := newHarness(t)
	h.setCaps(func(c *filecopy.Capabilities) { c.Hardlinks = false })
	h.writeSrc("a.mkv", content("a", 500), 1)
	h.writeSrc("keep.mkv", content("k", 50), 1) // an empty source would stop the resume (unmounted?)
	h.mustSync(false, jobs.Params{})
	h.writeSrc("a.mkv", content("A", 600), 2)
	j := h.newJob(jobs.TypeSync, false, jobs.Params{})
	if crashed, _, _ := runWithHook(h, j, faultinject.CrashAt(PointUpdateAfterRenameOld, 1)); !crashed {
		t.Fatal("no crash")
	}
	if exists(h.dstPath("movies/a.mkv")) {
		t.Fatal("the crash point is between the renames: the final name is free")
	}
	h.removeSrc("a.mkv")
	j.Attempt, j.Trigger = 2, jobs.TriggerResume
	res, err := h.run(h.sync, j)
	if err != nil || res.Warnings != 0 {
		t.Fatalf("resume: %v %+v\n%s", err, res, h.rep.dump())
	}
	if fileHash(t, h.dstPath("movies/a.mkv")) != strHash(content("A", 600)) || !h.hasContent(t, content("a", 500)) {
		t.Fatal("both versions must be at the destination")
	}
	for rel := range regularFiles(t, h.dstDir) {
		if filecopy.IsTempName(path.Base(rel)) {
			t.Fatalf("temp file left: %s", rel)
		}
	}
	// The next sync retains the vanished file normally.
	if _, st, _ := h.mustSync(false, jobs.Params{}); st.FilesRetained != 1 {
		t.Fatalf("next sync: %+v", st)
	}
	h.assertConverged(t)
}

// TestResumeWithTheSourceGoneRetainsNothing: a job planned with a healthy source resumes while the
// source share is unmounted (an empty mountpoint). It must fail instead of taking every file for
// deleted.
func TestResumeWithTheSourceGoneRetainsNothing(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("a.mkv", content("a", 500), 1)
	h.writeSrc("b.mkv", content("b", 500), 2)
	h.mustSync(false, jobs.Params{})
	h.removeSrc("a.mkv") // one planned retain
	h.writeSrc("c.mkv", content("c", 500), 3)
	j := h.newJob(jobs.TypeSync, false, jobs.Params{})
	if crashed, _, _ := runWithHook(h, j, faultinject.CrashAt(PointRecordAfterDB, 1)); !crashed {
		t.Fatal("no crash")
	}
	for _, f := range []string{"b.mkv", "c.mkv"} {
		h.removeSrc(f)
	}
	before := h.records()
	j.Attempt, j.Trigger = 2, jobs.TriggerResume
	if _, err := h.run(h.sync, j); err == nil || !strings.Contains(err.Error(), "is it mounted") {
		t.Fatalf("err = %v", err)
	}
	if !reflect.DeepEqual(before, h.records()) {
		t.Fatal("records changed")
	}
}

// TestCleanRunOfEveryAction checks the change sync's outcome per configuration without crashes.
func TestCleanRunOfEveryAction(t *testing.T) {
	for _, c := range crashConfigs {
		t.Run(c.name, func(t *testing.T) {
			h := crashCase(t, c, 2)
			res, st, j := h.mustSync(false, jobs.Params{})
			if st.FilesFailed != 0 || st.FilesHeld != 0 || res.Warnings != 0 {
				t.Fatalf("%+v\n%+v\n%s", st, h.items(j.ID), h.rep.dump())
			}
			items := h.items(j.ID)
			if got := itemsBy(items, jobs.ActionMove, jobs.ItemDone); !slices.Equal(got, []string{"movies/moved/b.mkv"}) {
				t.Errorf("moves %v", got)
			}
			if got := itemsBy(items, jobs.ActionUpdate, jobs.ItemDone); !slices.Equal(got, []string{"movies/a.mkv", "movies/g/h1.mkv", "movies/s1.mkv"}) {
				t.Errorf("updates %v", got)
			}
			if got := itemsBy(items, jobs.ActionPromote, jobs.ItemDone); !slices.Equal(got, []string{"movies/d/p2.mkv", "movies/g/h2.mkv"}) {
				t.Errorf("promotes %v", got)
			}
			if got := itemsBy(items, jobs.ActionAdopt, jobs.ItemDone); !slices.Equal(got, []string{"movies/y.mkv"}) {
				t.Errorf("adopts %v", got)
			}
			if st.FilesDisplaced != 1 {
				t.Errorf("displaced %d", st.FilesDisplaced)
			}
			h.assertConverged(t)
			for _, v := range oldVersions {
				if !h.hasContent(t, v) {
					t.Errorf("lost content %.12q...", v)
				}
			}
			if c.mode == destinations.HardlinksRecreate && c.caps {
				if inode(t, h.dstPath("movies/s1.mkv")) != inode(t, h.dstPath("movies/s2.mkv")) {
					t.Error("s1 and s2 are relinked")
				}
				if inode(t, h.dstPath("movies/n1.mkv")) != inode(t, h.dstPath("movies/n2.mkv")) {
					t.Error("n1 and n2 are linked")
				}
			}
		})
	}
}
