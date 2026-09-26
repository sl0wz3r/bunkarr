package syncer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// targetedJob creates a targeted sync job of the harness's source with a trigger.
func (h *harness) targetedJob(trigger jobs.Trigger, dryRun bool, paths ...string) jobs.Job {
	h.t.Helper()
	j, created, err := h.jq.CreateJob(h.ctx, jobs.Spec{Type: jobs.TypeSync, Trigger: trigger, DryRun: dryRun,
		Params: jobs.Params{DestinationID: h.dest.ID, SourceIDs: []int64{h.src.ID}, Paths: paths}})
	if err != nil || !created {
		h.t.Fatalf("create targeted job: %v (created %v)", err, created)
	}
	return j
}

// mustTargeted runs a targeted sync (trigger webhook) and fails the test on an error.
func (h *harness) mustTargeted(paths ...string) (jobs.Result, SyncStats, jobs.Job) {
	h.t.Helper()
	j := h.targetedJob(jobs.TriggerWebhook, false, paths...)
	res, err := h.run(h.sync, j)
	if err != nil {
		h.t.Fatalf("targeted sync: %v\nlogs:\n%s", err, h.rep.dump())
	}
	return res, res.Stats.(SyncStats), j
}

// TestTargetedSyncPlansOnlyItsPaths: a targeted sync scans and copies only its paths.
func TestTargetedSyncPlansOnlyItsPaths(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("A/a.mkv", content("a", 1000), 1)
	h.writeSrc("B/b.mkv", content("b", 1100), 2)
	h.mustSync(false, jobs.Params{})
	h.writeSrc("A/new.mkv", content("n", 1200), 3)
	h.writeSrc("B/new.mkv", content("m", 1300), 4)
	res, st, j := h.mustTargeted("A")
	if !st.Targeted || !slices.Equal(st.Paths, []string{"A"}) || st.FilesCopied != 1 || st.FilesPlanned != 1 || res.Warnings != 0 {
		t.Fatalf("stats %+v\n%s", st, h.rep.dump())
	}
	if got := itemsBy(h.items(j.ID), jobs.ActionCopy, jobs.ItemDone); !slices.Equal(got, []string{"movies/A/new.mkv"}) {
		t.Fatalf("copies = %v", got)
	}
	if exists(h.dstPath("movies/B/new.mkv")) {
		t.Fatal("a file outside the paths was copied")
	}
	// A file path works as a target too; a dry run writes nothing.
	dj := h.targetedJob(jobs.TriggerWebhook, true, "B/new.mkv")
	dres, err := h.run(h.sync, dj)
	if err != nil || dres.Stats.(SyncStats).FilesCopied != 1 || exists(h.dstPath("movies/B/new.mkv")) {
		t.Fatalf("dry run = %+v, %v", dres, err)
	}
	// The next full sync picks up the rest; then everything converged.
	if _, st, _ := h.mustSync(false, jobs.Params{}); st.FilesCopied != 1 {
		t.Fatalf("full sync = %+v", st)
	}
	h.assertConverged(t)
	if _, st, _ := h.mustTargeted("A", "B"); st.FilesPlanned != 0 {
		t.Fatalf("converged targeted sync planned %d", st.FilesPlanned)
	}
}

// TestTargetedSyncUpgradeRenameAndDeletes: in the targets, an upgrade (a new name in a folder whose
// old name vanished) copies the new file before it retains the old one, and a rename is a move;
// a vanished name in a folder that got nothing new is left live for the next full sync (D14).
func TestTargetedSyncUpgradeRenameAndDeletes(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("Heat (1995)/Heat 720p.mkv", content("h7", 2000), 1)
	h.writeSrc("Heat (1995)/Heat.nfo", content("nfo", 100), 2)
	h.writeSrc("Ran (1985)/Ran.mkv", content("r", 2100), 3)
	h.writeSrc("Up (2009)/Up.mkv", content("u", 2200), 4)
	h.mustSync(false, jobs.Params{})

	h.removeSrc("Heat (1995)/Heat 720p.mkv")
	h.writeSrc("Heat (1995)/Heat 1080p.mkv", content("h10", 2500), 5)
	h.renameSrc("Ran (1985)/Ran.mkv", "Ran (1985)/Ran (1985).mkv")
	h.removeSrc("Up (2009)/Up.mkv")
	res, st, j := h.mustTargeted("Heat (1995)", "Ran (1985)", "Up (2009)")
	if st.FilesCopied != 1 || st.FilesRetained != 1 || st.FilesMoved != 1 || st.RetainsDeferred != 1 || st.BytesCopied != 2500 || res.Warnings != 0 {
		t.Fatalf("stats %+v\n%s", st, h.rep.dump())
	}
	items := h.items(j.ID)
	var copyID, retainID int64
	for _, it := range items {
		switch {
		case it.Action == jobs.ActionCopy && it.Status == jobs.ItemDone:
			copyID = it.ID
		case it.Action == jobs.ActionRetain && it.Status == jobs.ItemDone:
			retainID = it.ID
			if it.RelPath != "movies/Heat (1995)/Heat 720p.mkv" {
				t.Fatalf("retained %s", it.RelPath)
			}
		}
	}
	if copyID == 0 || retainID == 0 || copyID > retainID {
		t.Fatalf("the copy (%d) must come before the retain (%d): %+v", copyID, retainID, items)
	}
	if !h.hasContent(t, content("h7", 2000)) {
		t.Fatal("the old version is not in retention")
	}
	// D14: Up's vanished file stays live and recorded until the next untargeted sync.
	if r, ok := h.liveRecord("movies/Up (2009)/Up.mkv"); !ok || r.State != StatePresent || !exists(h.dstPath("movies/Up (2009)/Up.mkv")) {
		t.Fatalf("Up's record = %+v %v", r, ok)
	}
	_, st, j = h.mustSync(false, jobs.Params{})
	if st.FilesRetained != 1 || !slices.Equal(itemsBy(h.items(j.ID), jobs.ActionRetain, jobs.ItemDone), []string{"movies/Up (2009)/Up.mkv"}) {
		t.Fatalf("full sync = %+v", st)
	}
	h.assertConverged(t)
	for _, r := range h.records() {
		if r.State == StateRetained && r.SourceRelPath == "Up (2009)/Up.mkv" && r.Reason != ReasonDeleted {
			t.Fatalf("retained with reason %s", r.Reason)
		}
	}
}

// TestTargetedSyncsNeverSliceAMassDeletion: 100 items deleted one webhook sync at a time retain
// nothing; the next full sync sees all 100 vanished files and holds them (S10b, D14).
func TestTargetedSyncsNeverSliceAMassDeletion(t *testing.T) {
	h := newHarness(t)
	for i := range 100 {
		h.writeSrc(fmt.Sprintf("M%03d/m.mkv", i), content(fmt.Sprint(i), 100+i), i)
	}
	h.writeSrc("keep/k.mkv", content("k", 50), 200)
	h.mustSync(false, jobs.Params{})
	for i := range 100 {
		if err := os.RemoveAll(h.srcPath(fmt.Sprintf("M%03d", i))); err != nil {
			t.Fatal(err)
		}
		_, st, _ := h.mustTargeted(fmt.Sprintf("M%03d", i))
		if st.FilesRetained != 0 || st.FilesPlanned != 0 || st.RetainsDeferred != 1 {
			t.Fatalf("targeted sync %d: %+v", i, st)
		}
	}
	res, st, _ := h.mustSync(false, jobs.Params{})
	if st.FilesHeld != 100 || st.FilesRetained != 0 || res.Warnings == 0 {
		t.Fatalf("full sync: %+v", st)
	}
}

// TestTargetedGuardCountsTheWholeSource: 25 updates inside a target are 7.7 % of the source's
// 325 files, below the guard's 10 %; counted against the target alone they would be held.
func TestTargetedGuardCountsTheWholeSource(t *testing.T) {
	h := newHarness(t)
	for i := range 300 {
		h.writeSrc(fmt.Sprintf("other/%03d.mkv", i), content("o", 10+i), i)
	}
	for i := range 25 {
		h.writeSrc(fmt.Sprintf("T/%02d.mkv", i), content("t", 100+i), i)
	}
	h.mustSync(false, jobs.Params{})
	for i := range 25 {
		h.writeSrc(fmt.Sprintf("T/%02d.mkv", i), content("u", 100+i), 1000+i)
	}
	_, st, _ := h.mustTargeted("T")
	if st.FilesUpdated != 25 || st.FilesHeld != 0 {
		t.Fatalf("stats %+v", st)
	}
}

// TestTargetedSyncLinksToAPartnerOutsideItsPaths: a new name hardlinked to a backed-up file
// outside the paths (a download folder in the same source) is linked, not copied again.
func TestTargetedSyncLinksToAPartnerOutsideItsPaths(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("downloads/e1.mkv", content("e", 1500), 1)
	h.writeSrc("tv/S1/e0.mkv", content("z", 900), 2)
	h.mustSync(false, jobs.Params{})
	h.linkSrc("downloads/e1.mkv", "tv/S1/e1.mkv")
	_, st, j := h.mustTargeted("tv/S1")
	if st.FilesLinked != 1 || st.FilesCopied != 0 || st.BytesCopied != 0 {
		t.Fatalf("stats %+v items %+v\n%s", st, h.items(j.ID), h.rep.dump())
	}
	h.assertConverged(t)
}

// TestTargetedSyncExpectedFiles: a webhook sync waits for the files the *arr expects (a mount that
// shows a new file late) and scans again; a file that never shows up is a warning.
func TestTargetedSyncExpectedFiles(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("A/old.mkv", content("o", 800), 1)
	h.mustSync(false, jobs.Params{})
	var (
		mu       sync.Mutex
		expected = []ExpectedFile{{RelPath: "A/late.mkv", Size: 1000, App: "Radarr"}}
		sleeps   []time.Duration
	)
	h.sync.expected = func(_ context.Context, src catalog.Source, paths []string) ([]ExpectedFile, error) {
		if src.ID != h.src.ID || !slices.Equal(paths, []string{"A"}) {
			return nil, errors.New("unexpected call")
		}
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(expected), nil
	}
	h.sync.sleep = func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		if len(sleeps) == 2 { // the file shows up after the second listing
			h.writeSrc("A/late.mkv", content("l", 1000), 2)
		}
		return nil
	}
	res, st, _ := h.mustTargeted("A")
	if st.FilesCopied != 1 || st.ExpectedMissing != 0 || res.Warnings != 0 ||
		!slices.Equal(sleeps, []time.Duration{5 * time.Second, 15 * time.Second}) {
		t.Fatalf("stats %+v sleeps %v\n%s", st, sleeps, h.rep.dump())
	}
	// A file that never shows up: three rescans, then a warning naming it and its *arr.
	sleeps = nil
	mu.Lock()
	expected = append(expected, ExpectedFile{RelPath: "A/never.mkv", Size: 5, App: "Radarr"})
	mu.Unlock()
	res, st, _ = h.mustTargeted("A")
	if st.ExpectedMissing != 1 || res.Warnings != 1 || len(sleeps) != 3 || !h.rep.has("Radarr reports A/never.mkv") {
		t.Fatalf("stats %+v warnings %d sleeps %v\n%s", st, res.Warnings, sleeps, h.rep.dump())
	}
	// Only webhook syncs wait: a manual targeted sync does not.
	sleeps = nil
	j := h.targetedJob(jobs.TriggerManual, false, "A")
	if _, err := h.run(h.sync, j); err != nil || len(sleeps) != 0 {
		t.Fatalf("manual targeted sync: %v, sleeps %v", err, sleeps)
	}
}

// TestFollowUpSyncOfAnUnmountedDestination: a webhook or targeted sync whose destination is not
// mounted ends with a warning instead of failing; any other sync still fails.
func TestFollowUpSyncOfAnUnmountedDestination(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("A/a.mkv", content("a", 100), 1)
	h.mustSync(false, jobs.Params{})
	if err := os.Remove(h.dstPath(filecopy.MarkerRel)); err != nil {
		t.Fatal(err)
	}
	j := h.targetedJob(jobs.TriggerSchedule, false, "A")
	res, err := h.run(h.sync, j)
	if err != nil || res.Warnings != 1 || !strings.Contains(res.Summary, "not mounted") || res.Stats.(SyncStats).Skipped == "" {
		t.Fatalf("targeted = %+v, %v", res, err)
	}
	j, _, err = h.jq.CreateJob(h.ctx, jobs.Spec{Type: jobs.TypeSync, Trigger: jobs.TriggerWebhook, Params: jobs.Params{DestinationID: h.dest.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := h.run(h.sync, j); err != nil || res.Warnings != 1 {
		t.Fatalf("webhook = %+v, %v", res, err)
	}
	j = h.newJob(jobs.TypeSync, false, jobs.Params{})
	if _, err := h.run(h.sync, j); !errors.Is(err, destinations.ErrNotMounted) {
		t.Fatalf("manual full sync = %v, want not mounted", err)
	}
}

// recEnqueuer records specs.
type recEnqueuer struct {
	mu    sync.Mutex
	specs []jobs.Spec
}

func (f *recEnqueuer) Enqueue(_ context.Context, spec jobs.Spec) (jobs.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.specs = append(f.specs, spec)
	return jobs.Job{ID: int64(1000 + len(f.specs)), Type: spec.Type, Params: spec.Params, QueuedAt: time.Now()}, nil
}

// TestManifestExportFollowsFullSyncs (§9.2): a full sync queues a manifest export with its
// trigger, also when it fails; a dry run, a targeted sync, a cancelled sync and a destination
// whose setting says no queue none.
func TestManifestExportFollowsFullSyncs(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("A/a.mkv", content("a", 100), 1)
	enq := &recEnqueuer{}
	allow := true
	h.sync.enq = enq
	h.sync.manifestAfter = func(_ context.Context, id int64) (bool, error) {
		if id != h.dest.ID {
			return false, errors.New("wrong destination")
		}
		return allow, nil
	}
	_, st, _ := h.mustSync(false, jobs.Params{})
	if len(enq.specs) != 1 || enq.specs[0].Type != jobs.TypeManifestExport || enq.specs[0].Trigger != jobs.TriggerManual ||
		enq.specs[0].Params.DestinationID != h.dest.ID || st.ManifestExportJob != 1001 {
		t.Fatalf("specs %+v stats %+v", enq.specs, st)
	}
	h.mustSync(true, jobs.Params{})
	h.mustTargeted("A")
	if len(enq.specs) != 1 {
		t.Fatalf("a dry run or a targeted sync queued %d", len(enq.specs)-1)
	}
	// A failed sync (its source vanished: the scan is refused) is followed too.
	if err := os.Rename(h.srcDir, h.srcDir+".gone"); err != nil {
		t.Fatal(err)
	}
	j := h.newJob(jobs.TypeSync, false, jobs.Params{})
	if _, err := h.run(h.sync, j); err == nil || len(enq.specs) != 2 {
		t.Fatalf("failed sync: %v, specs %d", err, len(enq.specs))
	}
	// Cancelled: none.
	ctx, cancel := context.WithCancel(h.ctx)
	cancel()
	j = h.newJob(jobs.TypeSync, false, jobs.Params{})
	if _, err := h.sync.Run(ctx, j, h.env()); err == nil || len(enq.specs) != 2 {
		t.Fatalf("cancelled sync: %v, specs %d", err, len(enq.specs))
	}
	h.closeJob(j.ID)
	allow = false
	j = h.newJob(jobs.TypeSync, false, jobs.Params{})
	_, _ = h.run(h.sync, j)
	if len(enq.specs) != 2 {
		t.Fatal("queued although the setting says no")
	}
}

func TestInTargets(t *testing.T) {
	s := &syncRun{job: jobs.Job{Params: jobs.Params{Paths: []string{"A", "B/b"}}},
		linked: map[int64]catalog.Source{1: {ID: 1, DestFolder: "movies"}}}
	for dest, want := range map[string]bool{"movies/A": true, "movies/A/x": true, "movies/AB": false, "movies/B": false,
		"movies/B/b/c": true, "movies": false} {
		if got := s.inTargets(1, dest); got != want {
			t.Errorf("inTargets(%q) = %v", dest, got)
		}
	}
}
