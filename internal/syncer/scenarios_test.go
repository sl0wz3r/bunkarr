package syncer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

func TestMassChangeGuardEndToEnd(t *testing.T) {
	h := newHarness(t)
	for i := range 30 {
		h.writeSrc(fmt.Sprintf("f%02d.mkv", i), content(fmt.Sprint(i), 100+i), i)
	}
	h.writeSrc("shrinks.mkv", content("s", 1000), 40)
	h.mustSync(false, jobs.Params{})
	// 25 of 31 files disappear (an unmounted subfolder, a bad *arr import): more than 10 % and 20.
	for i := range 25 {
		h.removeSrc(fmt.Sprintf("f%02d.mkv", i))
	}
	h.writeSrc("shrinks.mkv", content("s", 100), 41)
	h.writeSrc("added.mkv", content("n", 10), 42)
	res, st, j := h.mustSync(false, jobs.Params{})
	if st.FilesHeld != 26 || st.FilesRetained != 0 || st.FilesCopied != 1 || res.Warnings == 0 {
		t.Fatalf("stats %+v warnings %d", st, res.Warnings)
	}
	if !strings.Contains(res.Summary, "26 held") || !h.rep.has("held") {
		t.Errorf("summary %q", res.Summary)
	}
	for _, it := range h.items(j.ID) {
		if it.Status == jobs.ItemHeld && !strings.HasPrefix(it.Error, "held") {
			t.Errorf("held item without a reason: %+v", it)
		}
	}
	h.assertConvergedExcept(t, 25)
	// The held changes stay held on the next plain sync, and run with allowChanges.
	if _, st, _ := h.mustSync(false, jobs.Params{}); st.FilesHeld != 26 {
		t.Fatalf("second plain sync: %+v", st)
	}
	_, st, _ = h.mustSync(false, jobs.Params{AllowChanges: true})
	if st.FilesHeld != 0 || st.FilesRetained != 25 || st.FilesUpdated != 1 {
		t.Fatalf("allowChanges: %+v", st)
	}
	h.assertConverged(t)
}

// assertConvergedExcept is assertConverged for a destination that still holds n live records of
// vanished source files (held retains).
func (h *harness) assertConvergedExcept(t *testing.T, n int) {
	t.Helper()
	stale := 0
	for _, r := range h.records() {
		if r.State.Live() && !exists(h.srcPath(r.SourceRelPath)) {
			stale++
		}
	}
	if stale != n {
		t.Fatalf("%d live records of vanished files, want %d", stale, n)
	}
}

func TestLinkFallsBackToCopy(t *testing.T) {
	t.Run("the primary cannot be copied", func(t *testing.T) {
		h := newHarness(t)
		h.setCaps(func(c *filecopy.Capabilities) { c.InvalidChars = ":" })
		h.writeSrc("a:1.mkv", content("a", 500), 1) // the group's first name: not storable
		h.linkSrc("a:1.mkv", "b.mkv")
		res, st, j := h.mustSync(false, jobs.Params{})
		if st.FilesFailed != 1 || st.FilesCopied != 1 || st.FilesLinked != 0 || res.Warnings != 1 {
			t.Fatalf("stats %+v items %+v", st, h.items(j.ID))
		}
		if r, _ := h.liveRecord("movies/b.mkv"); r.State != StatePresent {
			t.Fatalf("b.mkv: %+v", r)
		}
	})
	t.Run("the names are no longer one inode at execution", func(t *testing.T) {
		h := newHarness(t)
		h.writeSrc("a.mkv", content("a", 500), 1)
		h.linkSrc("a.mkv", "b.mkv")
		// After the primary is copied, b.mkv is replaced by an independent file with other content.
		replaced := false
		faultinject.SetHook(func(name string) {
			if name == "copy.afterRename" && !replaced {
				replaced = true
				h.writeSrc("b.mkv", content("B", 500), 1)
			}
		})
		defer faultinject.SetHook(nil)
		_, st, j := h.mustSync(false, jobs.Params{})
		faultinject.SetHook(nil)
		if st.FilesCopied != 2 || st.FilesLinked != 0 || st.BytesCopied != 1000 {
			t.Fatalf("stats %+v items %+v", st, h.items(j.ID))
		}
		if fileHash(t, h.dstPath("movies/b.mkv")) != strHash(content("B", 500)) {
			t.Fatal("b.mkv must have its own (new) content, never a link to a.mkv")
		}
		h.assertConverged(t)
	})
}

func TestAdoptionWithCoarseMtimes(t *testing.T) {
	h := newHarness(t)
	h.setCaps(func(c *filecopy.Capabilities) { c.MtimeGranularityNs = 2_000_000_000 }) // FAT-like
	h.writeSrc("a.mkv", content("a", 800), 1)
	h.writeSrc("b.mkv", content("b", 800), 2)
	// rsync left copies whose mtimes the destination truncated to 2 s; b's content differs.
	srcA, _ := os.Stat(h.srcPath("a.mkv"))
	coarse := srcA.ModTime().Truncate(2 * time.Second)
	writeFileAt(t, h.dstPath("movies/a.mkv"), content("a", 800), coarse)
	writeFileAt(t, h.dstPath("movies/b.mkv"), content("x", 799), coarse)
	_, st, j := h.mustSync(false, jobs.Params{})
	if st.FilesAdopted != 1 || st.FilesCopied != 1 || st.FilesDisplaced != 1 || st.BytesCopied != 800 {
		t.Fatalf("stats %+v items %+v", st, h.items(j.ID))
	}
	h.assertConverged(t)
	if !h.hasContent(t, content("x", 799)) {
		t.Fatal("the displaced file must be kept in retention")
	}
	// A second sync considers the adopted file unchanged.
	if _, st, _ := h.mustSync(false, jobs.Params{}); st.FilesPlanned != 0 {
		t.Fatalf("second sync: %+v", st)
	}
}

func TestCaseInsensitiveDestinationNeverMergesNames(t *testing.T) {
	h := newHarness(t)
	writeFileAt(t, h.srcPath("probe"), "x", baseTime)
	if exists(h.srcPath("PROBE")) {
		t.Skip("the test's temp directory is case-insensitive; the source cannot hold both names (runs on Linux)")
	}
	h.removeSrc("probe")
	h.setCaps(func(c *filecopy.Capabilities) { c.CaseInsensitive = true })
	h.writeSrc("Movie.mkv", content("m", 300), 1)
	h.mustSync(false, jobs.Params{})
	// A second name that differs only in case: never written over the first.
	h.writeSrc("MOVIE.mkv", content("M", 301), 2)
	res, st, j := h.mustSync(false, jobs.Params{})
	if st.FilesFailed != 1 || res.Warnings != 1 {
		t.Fatalf("stats %+v items %+v", st, h.items(j.ID))
	}
	failed := h.items(j.ID)[0]
	if !strings.Contains(failed.Error, "differ only in case") {
		t.Fatalf("error %q", failed.Error)
	}
	if fileHash(t, h.dstPath("movies/Movie.mkv")) != strHash(content("m", 300)) {
		t.Fatal("the first name's backup changed")
	}
}

func TestMoveOntoAnUnmanagedFileDisplacesIt(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("a.mkv", content("a", 3000), 1)
	h.mustSync(false, jobs.Params{})
	h.renameSrc("a.mkv", "b/a.mkv")
	writeFileAt(t, h.dstPath("movies/b/a.mkv"), "someone else's", baseTime)
	_, st, j := h.mustSync(false, jobs.Params{})
	if st.FilesMoved != 1 || st.FilesDisplaced != 1 || st.BytesCopied != 0 {
		t.Fatalf("stats %+v items %+v", st, h.items(j.ID))
	}
	h.assertConverged(t)
	if !h.hasContent(t, "someone else's") {
		t.Fatal("the unmanaged file was lost")
	}
}

func TestUpdateOfAFileRemovedAtTheDestination(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("a.mkv", content("a", 300), 1)
	h.mustSync(false, jobs.Params{})
	if err := os.Remove(h.dstPath("movies/a.mkv")); err != nil {
		t.Fatal(err)
	}
	h.writeSrc("a.mkv", content("A", 310), 2)
	_, st, _ := h.mustSync(false, jobs.Params{})
	if st.FilesUpdated != 1 {
		t.Fatalf("stats %+v", st)
	}
	h.assertConverged(t)
	for _, r := range h.records() {
		if r.State == StateRetained {
			t.Fatalf("nothing to retain, got %+v", r)
		}
	}
}

func TestTwoSourcesOneDestination(t *testing.T) {
	h := newHarness(t)
	tvDir := resolvedTempDir(t)
	tv, err := h.cat.Create(h.ctx, catalog.SourceInput{Name: "TV", Path: tvDir})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.dests.SetSources(h.ctx, h.dest.ID, []int64{h.src.ID, tv.ID}); err != nil {
		t.Fatal(err)
	}
	h.writeSrc("m.mkv", content("m", 100), 1)
	writeFileAt(t, tvDir+"/Show/S01E01.mkv", content("e", 200), baseTime)
	_, st, _ := h.mustSync(false, jobs.Params{})
	if st.FilesCopied != 2 || len(st.Sources) != 2 {
		t.Fatalf("stats %+v", st)
	}
	if !exists(h.dstPath("tv/Show/S01E01.mkv")) || !exists(h.dstPath("movies/m.mkv")) {
		t.Fatal("each source mirrors into its folder")
	}
	// Params.SourceIDs narrows the sync; the other source's records are untouched.
	if err := os.Remove(tvDir + "/Show/S01E01.mkv"); err != nil {
		t.Fatal(err)
	}
	h.writeSrc("m2.mkv", content("n", 100), 2)
	_, st, _ = h.mustSync(false, jobs.Params{SourceIDs: []int64{h.src.ID}})
	if st.FilesCopied != 1 || st.FilesRetained != 0 {
		t.Fatalf("narrowed sync: %+v", st)
	}
	if r, ok := h.liveRecord("tv/Show/S01E01.mkv"); !ok || r.State != StatePresent {
		t.Fatal("the other source's record changed")
	}
}

func TestSyncThroughTheJobManager(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("a.mkv", content("a", 100), 1)
	h.writeSrc("b.mkv", content("b", 100), 2)
	h.linkSrc("b.mkv", "c.mkv")
	m := jobqueue.New(h.db, nil, jobqueue.Options{ProgressEvery: time.Millisecond})
	m.Register(jobs.TypeSync, h.sync)
	m.Register(jobs.TypeVerify, h.verify)
	if err := m.Start(h.ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	}()
	wait := func(j jobs.Job) jobs.Job {
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			got, err := m.Get(h.ctx, j.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status.Final() {
				return got
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("job %d did not finish", j.ID)
		return jobs.Job{}
	}
	j, err := m.Enqueue(h.ctx, jobs.Spec{Type: jobs.TypeSync, Params: jobs.Params{DestinationID: h.dest.ID}})
	if err != nil {
		t.Fatal(err)
	}
	got := wait(j)
	if got.Status != jobs.StatusCompleted || got.Warnings != 0 {
		t.Fatalf("job %+v", got)
	}
	var st SyncStats
	if err := json.Unmarshal(got.Stats, &st); err != nil {
		t.Fatal(err)
	}
	if st.FilesCopied != 2 || st.FilesLinked != 1 || st.BytesPlanned != 200 {
		t.Fatalf("stats %s", got.Stats)
	}
	if !strings.HasPrefix(got.Summary, "Copied 2, linked 1") {
		t.Errorf("summary %q", got.Summary)
	}
	v, err := m.Enqueue(h.ctx, jobs.Spec{Type: jobs.TypeVerify, Params: jobs.Params{DestinationID: h.dest.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if got := wait(v); got.Status != jobs.StatusCompleted {
		t.Fatalf("verify %+v", got)
	}
	h.assertConverged(t)
}

func TestItemDetailsAreThePreview(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("a.mkv", content("a", 100), 1)
	h.mustSync(false, jobs.Params{})
	h.writeSrc("a.mkv", content("A", 120), 2)
	_, _, j := h.mustSync(true, jobs.Params{})
	items := h.items(j.ID)
	if len(items) != 1 || items[0].Action != jobs.ActionUpdate {
		t.Fatalf("items %+v", items)
	}
	var d Detail
	if err := json.Unmarshal(items[0].Detail, &d); err != nil {
		t.Fatal(err)
	}
	r, _ := h.liveRecord("movies/a.mkv")
	if d.Source != "a.mkv" || d.Size != 120 || d.OldSize != 100 || d.RecordID != r.ID || d.Reason != "changed" || d.Temp != "" {
		t.Fatalf("detail %+v", d)
	}
	if !slices.Contains(h.rep.phases(), "planning") {
		t.Error("no planning phase reported")
	}
}

func TestItemErrorsDoNotStopTheJob(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("a.mkv", content("a", 300), 1)
	h.writeSrc("b.mkv", content("b", 300), 2)
	h.writeSrc("c.mkv", content("c", 300), 3)
	// b.mkv vanishes between planning and its copy (the free-space check runs in between).
	h.sync.freeSpace = func(*os.Root) (uint64, uint64, error) {
		h.removeSrc("b.mkv")
		return 1 << 50, 1 << 50, nil
	}
	res, st, j := h.mustSync(false, jobs.Params{})
	if st.FilesCopied != 2 || st.FilesFailed != 1 || res.Warnings != 1 {
		t.Fatalf("stats %+v warnings %d", st, res.Warnings)
	}
	failed := h.items(j.ID)
	for _, it := range failed {
		if it.RelPath == "movies/b.mkv" && (it.Status != jobs.ItemFailed || !strings.Contains(it.Error, "vanished")) {
			t.Fatalf("item %+v", it)
		}
	}
	if !h.rep.has("item failed") {
		t.Error("the failure is logged")
	}
}

func TestChangedDuringCopyFailsTheItem(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("growing.mkv", content("g", 3000), 1)
	h.writeSrc("ok.mkv", content("o", 300), 2)
	appended := false
	faultinject.SetHook(func(name string) {
		if name == "copy.afterWrite" && !appended {
			appended = true
			f, err := os.OpenFile(h.srcPath("growing.mkv"), os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = f.WriteString("more")
			_ = f.Close()
		}
	})
	defer faultinject.SetHook(nil)
	res, st, j := h.mustSync(false, jobs.Params{})
	faultinject.SetHook(nil)
	if st.FilesCopied != 1 || st.FilesFailed != 1 || res.Warnings != 1 {
		t.Fatalf("stats %+v items %+v", st, h.items(j.ID))
	}
	for _, it := range h.items(j.ID) {
		if it.Status == jobs.ItemFailed && !strings.Contains(it.Error, filecopy.ErrSourceChanged.Error()) {
			t.Errorf("error %q", it.Error)
		}
	}
	for rel := range regularFiles(t, h.dstDir) {
		if filecopy.IsTempName(path.Base(rel)) || rel == "movies/growing.mkv" {
			t.Fatalf("left %s", rel)
		}
	}
	// The next sync copies it.
	if _, st, _ := h.mustSync(false, jobs.Params{}); st.FilesCopied != 1 {
		t.Fatalf("retry: %+v", st)
	}
	h.assertConverged(t)
}

func TestDestinationPermissionDeniedIsFatal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	h := newHarness(t)
	h.writeSrc("d/a.mkv", content("a", 300), 1)
	h.writeSrc("d/b.mkv", content("b", 300), 2)
	folder := h.dstPath("movies")
	h.sync.freeSpace = func(*os.Root) (uint64, uint64, error) {
		if err := os.Mkdir(folder, 0o555); err != nil {
			t.Fatal(err)
		}
		return 1 << 50, 1 << 50, nil
	}
	t.Cleanup(func() { _ = os.Chmod(folder, 0o755) })
	j := h.newJob(jobs.TypeSync, false, jobs.Params{})
	_, err := h.run(h.sync, j)
	if err == nil || filecopy.Classify(err) != filecopy.Fatal {
		t.Fatalf("err = %v, want a fatal error", err)
	}
	if n := len(itemsBy(h.items(j.ID), jobs.ActionCopy, jobs.ItemFailed)); n != 0 {
		t.Fatalf("a fatal error stops the job instead of failing every item (%d failed)", n)
	}
}

func TestSettingsChangeBetweenSyncs(t *testing.T) {
	// Records made in one hardlink mode are handled after a switch to the other.
	h := newHarness(t)
	h.writeSrc("a.mkv", content("a", 100), 1)
	h.linkSrc("a.mkv", "b.mkv")
	h.mustSync(false, jobs.Params{})
	h.setSettings(func(s *destinations.Settings) { s.Hardlinks = destinations.HardlinksCopy })
	h.linkSrc("a.mkv", "c.mkv")
	h.removeSrc("a.mkv")
	_, st, j := h.mustSync(false, jobs.Params{})
	if st.FilesFailed != 0 {
		t.Fatalf("stats %+v items %+v", st, h.items(j.ID))
	}
	h.assertConverged(t)
	if _, st, _ := h.mustSync(false, jobs.Params{}); st.FilesPlanned != 0 {
		t.Fatalf("not converged: %+v", st)
	}
}

// TestShrinkAfterPlanningIsHeld: the S10b shrink rule holds an update whose source was truncated
// after the plan passed the guard (while other files were copied, or before a resume).
func TestShrinkAfterPlanningIsHeld(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("big.mkv", content("b", 1000), 1)
	h.mustSync(false, jobs.Params{})
	h.writeSrc("big.mkv", content("B", 1010), 2) // grows: an update the guard lets through
	h.writeSrc("0new.mkv", content("n", 100), 3) // copied before the update
	truncated := false
	faultinject.SetHook(func(name string) {
		if name == "copy.afterRename" && !truncated {
			truncated = true
			if err := os.Truncate(h.srcPath("big.mkv"), 0); err != nil {
				t.Error(err)
			}
		}
	})
	defer faultinject.SetHook(nil)
	res, st, j := h.mustSync(false, jobs.Params{})
	faultinject.SetHook(nil)
	if !truncated || st.FilesHeld != 1 || st.FilesUpdated != 0 || st.FilesCopied != 1 || res.Warnings == 0 {
		t.Fatalf("stats %+v warnings %d items %+v", st, res.Warnings, h.items(j.ID))
	}
	for _, it := range h.items(j.ID) {
		if it.RelPath == "movies/big.mkv" && (it.Status != jobs.ItemHeld || !strings.HasPrefix(it.Error, "held")) {
			t.Fatalf("item %+v", it)
		}
	}
	if fileHash(t, h.dstPath("movies/big.mkv")) != strHash(content("b", 1000)) {
		t.Fatal("the backed-up version was replaced by the truncated file")
	}
	for _, r := range h.records() {
		if r.State == StateRetained {
			t.Fatalf("nothing is retained while the update is held: %+v", r)
		}
	}
	// "Apply held changes" runs it; the old version is kept in retention.
	if _, st, _ := h.mustSync(false, jobs.Params{AllowChanges: true}); st.FilesUpdated != 1 || st.FilesHeld != 0 {
		t.Fatalf("allowChanges: %+v", st)
	}
	h.assertConverged(t)
	if !h.hasContent(t, content("b", 1000)) {
		t.Fatal("the old version is not in retention")
	}
}

// TestFileThatLeavesTheCatalogIsRetained: a file that stays on disk but leaves the catalog (newly
// excluded, replaced by a symlink) is retained once, not skipped as "back at the source" and
// planned again by every sync.
func TestFileThatLeavesTheCatalogIsRetained(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("a.mkv", content("a", 100), 1)
	h.writeSrc("a.nfo", content("n", 10), 2)
	h.writeSrc("b.mkv", content("b", 100), 3)
	h.mustSync(false, jobs.Params{})
	in := sourceInput(h, nil)
	in.Exclude = append(in.Exclude, "*.nfo")
	if _, err := h.cat.Update(h.ctx, h.src.ID, in); err != nil {
		t.Fatal(err)
	}
	h.removeSrc("b.mkv")
	if err := os.Symlink("a.mkv", h.srcPath("b.mkv")); err != nil {
		t.Fatal(err)
	}
	_, st, j := h.mustSync(false, jobs.Params{})
	if got := itemsBy(h.items(j.ID), jobs.ActionRetain, jobs.ItemDone); !slices.Equal(got, []string{"movies/a.nfo", "movies/b.mkv"}) {
		t.Fatalf("retained %v: %+v items %+v", got, st, h.items(j.ID))
	}
	for _, rel := range []string{"movies/a.nfo", "movies/b.mkv"} {
		if _, ok := h.liveRecord(rel); ok {
			t.Errorf("%s is still live", rel)
		}
	}
	if !h.hasContent(t, content("n", 10)) || !h.hasContent(t, content("b", 100)) {
		t.Fatal("the retained versions are kept")
	}
	if _, st, j := h.mustSync(false, jobs.Params{}); st.FilesPlanned != 0 {
		t.Fatalf("planned again: %+v items %+v", st, h.items(j.ID))
	}
}

// hookReporter calls onProgress before recording each progress report.
type hookReporter struct {
	*recReporter
	onProgress func(jobs.Progress)
}

func (r *hookReporter) Progress(p jobs.Progress) {
	r.onProgress(p)
	r.recReporter.Progress(p)
}

// TestSourceEditedWhileAnotherIsScanned: a source's path and destFolder may change (it has no
// backups yet, its lock is free) while the sync scans another source; the sync plans and copies
// it with what its own scan saw.
func TestSourceEditedWhileAnotherIsScanned(t *testing.T) {
	h := newHarness(t)
	tvOld, tvNew := resolvedTempDir(t), resolvedTempDir(t)
	tv, err := h.cat.Create(h.ctx, catalog.SourceInput{Name: "TV", Path: tvOld})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.dests.SetSources(h.ctx, h.dest.ID, []int64{h.src.ID, tv.ID}); err != nil {
		t.Fatal(err)
	}
	h.writeSrc("m.mkv", content("m", 100), 1)
	writeFileAt(t, tvOld+"/old.mkv", content("o", 200), baseTime)
	writeFileAt(t, tvNew+"/new.mkv", content("n", 300), baseTime)
	edited := false
	rep := &hookReporter{recReporter: h.rep, onProgress: func(p jobs.Progress) {
		if p.Phase == "scanning" && p.CurrentFile == h.src.Name && !edited {
			edited = true
			if _, err := h.cat.Update(h.ctx, tv.ID, catalog.SourceInput{Name: "TV", Path: tvNew, DestFolder: "series"}); err != nil {
				t.Error(err)
			}
		}
	}}
	j := h.newJob(jobs.TypeSync, false, jobs.Params{})
	res, err := h.sync.Run(h.ctx, j, jobs.Env{Reporter: rep, Items: h.jq})
	h.closeJob(j.ID)
	if err != nil || !edited {
		t.Fatalf("sync: %v (edited %v)", err, edited)
	}
	if st := res.Stats.(SyncStats); st.FilesFailed != 0 || st.FilesCopied != 2 {
		t.Fatalf("stats %+v items %+v", st, h.items(j.ID))
	}
	if !exists(h.dstPath("series/new.mkv")) || fileHash(t, h.dstPath("series/new.mkv")) != strHash(content("n", 300)) {
		t.Fatal("the edited source is mirrored into its new folder")
	}
	if exists(h.dstPath("tv")) {
		t.Fatal("nothing goes to the old folder")
	}
	if r, ok := h.liveRecord("series/new.mkv"); !ok || r.SourceID != tv.ID || r.SourceRelPath != "new.mkv" {
		t.Fatalf("record %+v", r)
	}
}

// TestFatalErrorCleansUpTheItem: a job stopped by a fatal destination error is failed, never
// resumed, so the item removes its temp file and records what it already moved into retention.
func TestFatalErrorCleansUpTheItem(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	setup := func(t *testing.T) *harness {
		h := newHarness(t)
		h.setCaps(func(c *filecopy.Capabilities) { c.Hardlinks = false })
		h.writeSrc("d/a.mkv", content("a", 500), 1)
		h.mustSync(false, jobs.Params{})
		h.writeSrc("d/a.mkv", content("A", 600), 2)
		return h
	}
	t.Run("the temp file is removed", func(t *testing.T) {
		h := setup(t)
		// Retention cannot be written: the old version cannot be kept, the update stops.
		ret := h.dstPath(filecopy.RetentionRoot)
		if err := os.MkdirAll(ret, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(ret, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(ret, 0o755) })
		j := h.newJob(jobs.TypeSync, false, jobs.Params{})
		if _, err := h.run(h.sync, j); err == nil || filecopy.Classify(err) != filecopy.Fatal {
			t.Fatalf("err = %v, want a fatal error", err)
		}
		for rel := range regularFiles(t, h.dstDir) {
			if filecopy.IsTempName(path.Base(rel)) {
				t.Fatalf("temp file left by the failed job: %s", rel)
			}
		}
		if err := os.Chmod(ret, 0o755); err != nil {
			t.Fatal(err)
		}
		h.mustSync(false, jobs.Params{})
		h.assertConverged(t)
	})
	t.Run("the old version in retention is recorded", func(t *testing.T) {
		h := setup(t)
		dir := h.dstPath("movies/d")
		faultinject.SetHook(func(name string) {
			if name == PointUpdateAfterRenameOld {
				// The new version cannot be renamed into place: a fatal error between the renames.
				if err := os.Chmod(dir, 0o555); err != nil {
					t.Error(err)
				}
			}
		})
		defer faultinject.SetHook(nil)
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
		j := h.newJob(jobs.TypeSync, false, jobs.Params{})
		if _, err := h.run(h.sync, j); err == nil || filecopy.Classify(err) != filecopy.Fatal {
			t.Fatalf("err = %v, want a fatal error", err)
		}
		faultinject.SetHook(nil)
		var kept []Record
		for _, r := range h.records() {
			if r.State == StateRetained {
				kept = append(kept, r)
			}
		}
		if len(kept) != 1 || kept[0].Reason != ReasonReplaced || kept[0].RelPath != "movies/d/a.mkv" ||
			fileHash(t, h.dstPath(kept[0].RetainedPath)) != strHash(content("a", 500)) {
			t.Fatalf("retained records %+v", kept)
		}
	})
	t.Run("a half-done replacement's temp file is removed", func(t *testing.T) {
		h := setup(t)
		// A crash between the renames: the old version is in retention, the new one complete in its
		// temp file, nothing at the final path.
		j := h.newJob(jobs.TypeSync, false, jobs.Params{})
		if crashed, _, _ := runWithHook(h, j, faultinject.CrashAt(PointUpdateAfterRenameOld, 1)); !crashed {
			t.Fatal("no crash")
		}
		// The resume stops with a fatal error (retention cannot be read): the job fails and is never
		// resumed, so the temp file must not stay behind.
		dirs, err := filepath.Glob(h.dstPath(filecopy.RetentionRoot + "/*"))
		if err != nil || len(dirs) != 1 {
			t.Fatalf("retention dirs %v %v", dirs, err)
		}
		if err := os.Chmod(dirs[0], 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dirs[0], 0o755) })
		j.Attempt, j.Trigger = 2, jobs.TriggerResume
		if _, err := h.run(h.sync, j); err == nil || filecopy.Classify(err) != filecopy.Fatal {
			t.Fatalf("resume: err = %v, want a fatal error", err)
		}
		for rel := range regularFiles(t, h.dstPath("movies")) {
			if filecopy.IsTempName(path.Base(rel)) {
				t.Fatalf("temp file left by the failed job: %s", rel)
			}
		}
		if err := os.Chmod(dirs[0], 0o755); err != nil {
			t.Fatal(err)
		}
		// The next sync writes the new version again; the old one is still in retention.
		if _, st, j := h.mustSync(false, jobs.Params{}); st.FilesFailed != 0 || st.FilesUpdated != 1 {
			t.Fatalf("next sync: %+v items %+v", st, h.items(j.ID))
		}
		if fileHash(t, h.dstPath("movies/d/a.mkv")) != strHash(content("A", 600)) || !h.hasContent(t, content("a", 500)) {
			t.Fatal("both versions must be at the destination")
		}
	})
}

// TestShrinkAfterPlanningHoldsThePromote: on a destination that records hardlinks, an update of a
// primary whose other name keeps the old content first promotes that name; when the primary's
// source was truncated after planning, the promote is held with the update (the primary keeps its
// file and record).
func TestShrinkAfterPlanningHoldsThePromote(t *testing.T) {
	h := newHarness(t)
	h.setSettings(func(s *destinations.Settings) { s.Hardlinks = destinations.HardlinksCopy })
	h.writeSrc("h1.mkv", content("h", 1600), 1)
	h.linkSrc("h1.mkv", "h2.mkv")
	h.writeSrc("k.mkv", content("k", 10), 2)
	h.mustSync(false, jobs.Params{})
	h.writeSrc("h1.mkv", content("H", 1700), 3) // a new file: h2 keeps the old content
	h.sync.freeSpace = func(*os.Root) (uint64, uint64, error) {
		if err := os.Truncate(h.srcPath("h1.mkv"), 0); err != nil { // after the plan, before execution
			t.Error(err)
		}
		return 1 << 50, 1 << 50, nil
	}
	_, st, j := h.mustSync(false, jobs.Params{})
	h.sync.freeSpace = func(*os.Root) (uint64, uint64, error) { return 1 << 50, 1 << 50, nil }
	if st.FilesHeld != 2 || st.FilesPromoted != 0 || st.FilesUpdated != 0 {
		t.Fatalf("stats %+v items %+v", st, h.items(j.ID))
	}
	if r, _ := h.liveRecord("movies/h1.mkv"); r.State != StatePresent || fileHash(t, h.dstPath("movies/h1.mkv")) != strHash(content("h", 1600)) {
		t.Fatalf("h1: %+v", r)
	}
	if r, _ := h.liveRecord("movies/h2.mkv"); r.State != StateLinkRecorded {
		t.Fatalf("h2: %+v", r)
	}
	// "Apply held changes": the old content moves to h2, h1 gets the (empty) new version.
	if _, st, j := h.mustSync(false, jobs.Params{AllowChanges: true}); st.FilesHeld != 0 || st.FilesFailed != 0 {
		t.Fatalf("allowChanges: %+v items %+v", st, h.items(j.ID))
	}
	h.assertConverged(t)
}

// TestShrinkAfterThePromoteHoldsTheRepair: the primary's source is truncated after its promote ran
// (its old content now lives under the other name, its record is missing) but before its update:
// the update is held, and so is the copy the next sync plans for the missing record (S10b holds
// the truncated version until a sync with allowChanges).
func TestShrinkAfterThePromoteHoldsTheRepair(t *testing.T) {
	h := newHarness(t)
	h.setSettings(func(s *destinations.Settings) { s.Hardlinks = destinations.HardlinksCopy })
	h.writeSrc("h1.mkv", content("h", 1600), 1)
	h.linkSrc("h1.mkv", "h2.mkv")
	h.writeSrc("k.mkv", content("k", 10), 2)
	h.mustSync(false, jobs.Params{})
	h.writeSrc("h1.mkv", content("H", 1700), 3) // a new file: h2 keeps the old content
	faultinject.SetHook(func(name string) {
		if name == PointPromoteAfterRename {
			if err := os.Truncate(h.srcPath("h1.mkv"), 0); err != nil {
				t.Error(err)
			}
		}
	})
	_, st, j := h.mustSync(false, jobs.Params{})
	faultinject.SetHook(nil)
	if st.FilesPromoted != 1 || st.FilesHeld != 1 || st.FilesUpdated != 0 {
		t.Fatalf("stats %+v items %+v", st, h.items(j.ID))
	}
	_, st, j = h.mustSync(false, jobs.Params{})
	if st.FilesHeld != 1 || st.FilesCopied != 0 {
		t.Fatalf("the truncated version was copied without allowChanges: %+v items %+v", st, h.items(j.ID))
	}
	if r, _ := h.liveRecord("movies/h1.mkv"); r.State != StateMissing || exists(h.dstPath("movies/h1.mkv")) {
		t.Fatalf("h1: %+v", r)
	}
	if _, st, j := h.mustSync(false, jobs.Params{AllowChanges: true}); st.FilesHeld != 0 || st.FilesFailed != 0 || st.FilesCopied != 1 {
		t.Fatalf("allowChanges: %+v items %+v", st, h.items(j.ID))
	}
	h.assertConverged(t)
	if !h.hasContent(t, content("h", 1600)) {
		t.Fatal("lost the old content")
	}
}

// TestSourceDisabledWhileAnotherIsScanned: a source disabled after the sync started is not synced.
func TestSourceDisabledWhileAnotherIsScanned(t *testing.T) {
	h := newHarness(t)
	tvDir := resolvedTempDir(t)
	tv, err := h.cat.Create(h.ctx, catalog.SourceInput{Name: "TV", Path: tvDir})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.dests.SetSources(h.ctx, h.dest.ID, []int64{h.src.ID, tv.ID}); err != nil {
		t.Fatal(err)
	}
	h.writeSrc("m.mkv", content("m", 100), 1)
	writeFileAt(t, tvDir+"/e.mkv", content("e", 200), baseTime)
	disabled := false
	rep := &hookReporter{recReporter: h.rep, onProgress: func(p jobs.Progress) {
		if p.Phase == "scanning" && p.CurrentFile == h.src.Name && !disabled {
			disabled = true
			off := false
			if _, err := h.cat.Update(h.ctx, tv.ID, catalog.SourceInput{Name: "TV", Path: tvDir, DestFolder: tv.DestFolder, Enabled: &off}); err != nil {
				t.Error(err)
			}
		}
	}}
	j := h.newJob(jobs.TypeSync, false, jobs.Params{})
	res, err := h.sync.Run(h.ctx, j, jobs.Env{Reporter: rep, Items: h.jq})
	h.closeJob(j.ID)
	if err != nil || !disabled {
		t.Fatalf("sync: %v (disabled %v)", err, disabled)
	}
	if st := res.Stats.(SyncStats); st.FilesCopied != 1 || st.FilesFailed != 0 || len(st.Sources) != 1 {
		t.Fatalf("stats %+v", st)
	}
	if exists(h.dstPath(tv.DestFolder)) {
		t.Fatal("the disabled source was synced")
	}
}
