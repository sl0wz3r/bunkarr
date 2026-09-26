package syncer

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// TestExpectedFilesTheScanNeverCatalogsAreNotWaitedFor: a symlink the *arr imported, a file under
// a symlinked or excluded folder and a file whose size on disk the catalog already has (the *arr's
// is stale) are reported without a wait; a file that is there but was not listed by the scan is
// waited for.
func TestExpectedFilesTheScanNeverCatalogsAreNotWaitedFor(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("A/real.mkv", content("r", 800), 1)
	h.writeSrc("A/Featurettes/f.mkv", content("f", 300), 2)
	h.writeSrc("A/transcoded.mkv", content("t", 500), 3)
	if err := os.Symlink(h.srcPath("A/real.mkv"), h.srcPath("A/imported.mkv")); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(h.base, "elsewhere")
	writeFileAt(t, filepath.Join(elsewhere, "e.mkv"), content("e", 100), baseTime)
	if err := os.Symlink(elsewhere, h.srcPath("A/Linked")); err != nil {
		t.Fatal(err)
	}
	src, err := h.cat.Update(h.ctx, h.src.ID, catalog.SourceInput{Name: h.src.Name, Path: h.src.Path, Exclude: []string{"Featurettes/"}})
	if err != nil {
		t.Fatal(err)
	}
	h.src = src
	h.mustSync(false, jobs.Params{})

	expected := []ExpectedFile{
		{RelPath: "A/imported.mkv", Size: 800, App: "Sonarr"},
		{RelPath: "A/Linked/e.mkv", Size: 100, App: "Sonarr"},
		{RelPath: "A/Featurettes/f.mkv", Size: 300, App: "Radarr"},
		{RelPath: "A/transcoded.mkv", Size: 900, App: "Radarr"},
		{RelPath: "A/real.mkv", Size: 800, App: "Radarr"},
	}
	var extra []ExpectedFile
	var before func()
	h.sync.expected = func(_ context.Context, _ catalog.Source, _ []string) ([]ExpectedFile, error) {
		if before != nil {
			before()
			before = nil
		}
		return append(slices.Clone(expected), extra...), nil
	}
	var sleeps []time.Duration
	h.sync.sleep = func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return nil
	}
	res, st, _ := h.mustTargeted("A")
	if len(sleeps) != 0 || st.ExpectedMissing != 0 || res.Warnings != 1 {
		t.Fatalf("sleeps %v stats %+v warnings %d\n%s", sleeps, st, res.Warnings, h.rep.dump())
	}
	for _, want := range []string{
		"Sonarr reports A/imported.mkv, which the scan of \"Movies\" never catalogs: A/imported.mkv is a symbolic link",
		"Sonarr reports A/Linked/e.mkv, which the scan of \"Movies\" never catalogs: A/Linked is a symbolic link",
		"Radarr reports A/Featurettes/f.mkv, which the scan of \"Movies\" never catalogs: A/Featurettes is excluded",
		"Radarr reports A/transcoded.mkv with 900 B, but the file in \"Movies\" has 500 B",
	} {
		if !h.rep.has(want) {
			t.Fatalf("no log %q:\n%s", want, h.rep.dump())
		}
	}
	if h.rep.has("not backed up yet") {
		t.Fatalf("a file that cannot show up was reported missing:\n%s", h.rep.dump())
	}

	// A file that is on disk, but was not listed when the scan read its folder, and one whose size
	// changed since the scan read it are waited for; the files that are not waited for are
	// reported once.
	h.rep = &recReporter{}
	h.writeSrc("A/growing.mkv", content("g", 400), 5)
	before = func() {
		h.writeSrc("A/appeared.mkv", content("a", 700), 4)
		h.writeSrc("A/growing.mkv", content("g", 600), 5)
	}
	extra = []ExpectedFile{{RelPath: "A/appeared.mkv", Size: 700, App: "Radarr"}, {RelPath: "A/growing.mkv", Size: 600, App: "Radarr"}}
	res, st, _ = h.mustTargeted("A")
	if !slices.Equal(sleeps, []time.Duration{5 * time.Second}) || st.FilesCopied != 2 || st.BytesCopied != 1300 ||
		st.ExpectedMissing != 0 || res.Warnings != 1 {
		t.Fatalf("sleeps %v stats %+v warnings %d\n%s", sleeps, st, res.Warnings, h.rep.dump())
	}
	if n := strings.Count(h.rep.dump(), "A/imported.mkv is a symbolic link"); n != 1 {
		t.Fatalf("reported %d times:\n%s", n, h.rep.dump())
	}
}

// TestExpectedFileWaitsFreeTheSourceLock: while a webhook sync waits between rescans, the source's
// lock is free (another destination's sync of the source scans and plans meanwhile); a source
// disabled meanwhile is not planned.
func TestExpectedFileWaitsFreeTheSourceLock(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("A/a.mkv", content("a", 800), 1)
	h.mustSync(false, jobs.Params{})
	h.sync.expected = func(_ context.Context, _ catalog.Source, _ []string) ([]ExpectedFile, error) {
		return []ExpectedFile{{RelPath: "A/late.mkv", Size: 1000, App: "Radarr"}}, nil
	}
	var free []bool
	h.sync.sleep = func(ctx context.Context, _ time.Duration) error {
		lctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		unlock, err := h.cat.LockSource(lctx, h.src.ID)
		free = append(free, err == nil)
		if err == nil {
			unlock()
		}
		if len(free) == 2 {
			h.writeSrc("A/late.mkv", content("l", 1000), 2)
		}
		return nil
	}
	res, st, _ := h.mustTargeted("A")
	if !slices.Equal(free, []bool{true, true}) || st.FilesCopied != 1 || st.ExpectedMissing != 0 || res.Warnings != 0 {
		t.Fatalf("lock free during the waits: %v; stats %+v\n%s", free, st, h.rep.dump())
	}

	h.writeSrc("A/b.mkv", content("b", 900), 3)
	h.sync.expected = func(_ context.Context, _ catalog.Source, _ []string) ([]ExpectedFile, error) {
		return []ExpectedFile{{RelPath: "A/never.mkv", Size: 5, App: "Radarr"}}, nil
	}
	h.sync.sleep = func(context.Context, time.Duration) error {
		off := false
		_, err := h.cat.Update(h.ctx, h.src.ID, catalog.SourceInput{Name: h.src.Name, Path: h.src.Path, Enabled: &off})
		return err
	}
	_, st, _ = h.mustTargeted("A")
	if st.FilesPlanned != 0 || !h.rep.has("while the sync waited for the files the *arr expects: not synced") {
		t.Fatalf("stats %+v\n%s", st, h.rep.dump())
	}
}

// TestExpectedFileRescansCountOnce: the rescans of a webhook sync neither count the target's files
// again nor repeat the warnings of the first scan.
func TestExpectedFileRescansCountOnce(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a folder without permissions")
	}
	h := newHarness(t)
	for _, n := range []string{"1", "2", "3"} {
		h.writeSrc("A/"+n+".mkv", content(n, 100), 1)
	}
	h.writeSrc("A/locked/x.mkv", content("x", 100), 2)
	h.mustSync(false, jobs.Params{})
	locked := h.srcPath("A/locked")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	h.sync.expected = func(_ context.Context, _ catalog.Source, _ []string) ([]ExpectedFile, error) {
		return []ExpectedFile{{RelPath: "A/never.mkv", Size: 5, App: "Radarr"}}, nil
	}
	sleeps := 0
	h.sync.sleep = func(context.Context, time.Duration) error { sleeps++; return nil }
	res, st, _ := h.mustTargeted("A")
	// One warning for the folder that cannot be read, one for the file that never showed up.
	if sleeps != 3 || len(st.Sources) != 1 || st.Sources[0].Files != 3 || res.Warnings != 2 || st.ExpectedMissing != 1 {
		t.Fatalf("sleeps %d warnings %d stats %+v\n%s", sleeps, res.Warnings, st, h.rep.dump())
	}
	if n := strings.Count(h.rep.dump(), "cannot read directory A/locked"); n != 1 {
		t.Fatalf("the warning was logged %d times:\n%s", n, h.rep.dump())
	}
}

// TestExpectedFilesInFoldersTheScanCannotReadAreNotWaitedFor: a file the *arr reports in a folder
// the scan cannot read (an *arr running with another PUID or umask) is reported without a wait:
// the scan keeps that folder's rows as they are, scan after scan. Both ways a folder can refuse:
// no permission at all (its listing fails), and read without search (the lookup of the file
// fails).
func TestExpectedFilesInFoldersTheScanCannotReadAreNotWaitedFor(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a folder without permissions")
	}
	h := newHarness(t)
	h.writeSrc("Show/Season 01/e1.mkv", content("1", 100), 1)
	h.writeSrc("Show/Season 02/e1.mkv", content("2", 200), 2)
	h.writeSrc("Show/Season 03/e1.mkv", content("3", 300), 3)
	for dir, mode := range map[string]os.FileMode{"Show/Season 02": 0o200, "Show/Season 03": 0o400} {
		p := h.srcPath(dir)
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(p, 0o755) })
	}
	h.sync.expected = func(_ context.Context, _ catalog.Source, _ []string) ([]ExpectedFile, error) {
		return []ExpectedFile{
			{RelPath: "Show/Season 01/e1.mkv", Size: 100, App: "Sonarr"},
			{RelPath: "Show/Season 02/e1.mkv", Size: 200, App: "Sonarr"},
			{RelPath: "Show/Season 03/e1.mkv", Size: 300, App: "Sonarr"},
		}, nil
	}
	var sleeps []time.Duration
	h.sync.sleep = func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return nil
	}
	_, st, _ := h.mustTargeted("Show")
	if len(sleeps) != 0 || st.ExpectedMissing != 0 || st.FilesCopied != 1 {
		t.Fatalf("sleeps %v stats %+v\n%s", sleeps, st, h.rep.dump())
	}
	for _, want := range []string{
		"Sonarr reports Show/Season 02/e1.mkv, which the scan of \"Movies\" never catalogs: the folder Show/Season 02 cannot be read (permission denied)",
		"Sonarr reports Show/Season 03/e1.mkv, which the scan of \"Movies\" never catalogs: Show/Season 03/e1.mkv cannot be read (permission denied)",
	} {
		if !h.rep.has(want) {
			t.Fatalf("no log %q:\n%s", want, h.rep.dump())
		}
	}
	if h.rep.has("not backed up yet") {
		t.Fatalf("a file the scan cannot read was waited for:\n%s", h.rep.dump())
	}
}

// TestExpectedFilesSpelledOtherwiseOnDiskAreNotWaitedFor: a file whose path the *arr spells
// otherwise than the folders list it (case: a case-insensitive filesystem or SMB share; Unicode
// normalization on APFS) is cataloged and backed up under the names on disk. It is reported once,
// neither waited for nor reported missing.
func TestExpectedFilesSpelledOtherwiseOnDiskAreNotWaitedFor(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("Show/season 1/e1.mkv", content("1", 100), 1)
	expected := []ExpectedFile{{RelPath: "Show/Season 1/e1.mkv", Size: 100, App: "Sonarr"}}
	wants := []string{"Sonarr reports Show/Season 1/e1.mkv, but the scan of \"Movies\" catalogs the names on disk: Show/Season 1 is spelled Show/season 1 on disk"}
	// Another normalization of a name is found by a lookup only where the filesystem ignores it.
	nfd, nfc := "Cafe\u0301", "Caf\u00e9"
	writeFileAt(t, filepath.Join(h.base, "probe", nfd), "p", baseTime)
	if _, err := os.Lstat(filepath.Join(h.base, "probe", nfc)); err == nil {
		h.writeSrc("Show/"+nfd+"/e2.mkv", content("2", 200), 2)
		expected = append(expected, ExpectedFile{RelPath: "Show/" + nfc + "/e2.mkv", Size: 200, App: "Sonarr"})
		wants = append(wants, "Sonarr reports Show/"+nfc+"/e2.mkv, but the scan of \"Movies\" catalogs the names on disk: Show/"+nfc+
			" is spelled differently on disk (case or Unicode normalization)")
	} else {
		t.Logf("the filesystem of %s tells Unicode normalizations apart: that case is not checked", h.base)
	}
	h.sync.expected = func(_ context.Context, _ catalog.Source, _ []string) ([]ExpectedFile, error) {
		return expected, nil
	}
	var sleeps []time.Duration
	h.sync.sleep = func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return nil
	}
	res, st, _ := h.mustTargeted("Show")
	if len(sleeps) != 0 || st.ExpectedMissing != 0 || res.Warnings != 0 || st.FilesCopied != int64(len(expected)) {
		t.Fatalf("sleeps %v stats %+v warnings %d\n%s", sleeps, st, res.Warnings, h.rep.dump())
	}
	for _, want := range wants {
		if !h.rep.has(want) {
			t.Fatalf("no log %q:\n%s", want, h.rep.dump())
		}
	}
	if !exists(h.dstPath("movies/Show/season 1/e1.mkv")) {
		t.Fatalf("not backed up under the name on disk: %v", tree(t, h.dstDir))
	}
}

// TestExpectedFilesUnderATargetSpelledOtherwiseAreWarned: when the target itself (the *arr's
// spelling of the item folder) is listed only under another spelling, the targeted scan walks
// nothing under it (it is gone under this spelling), so the file is not backed up by this sync. It
// is a warning counted as missing (not an INFO line saying the names on disk are cataloged), with
// no wait: the next full sync backs it up.
func TestExpectedFilesUnderATargetSpelledOtherwiseAreWarned(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("show/Season 1/e1.mkv", content("1", 100), 1)
	h.sync.expected = func(_ context.Context, _ catalog.Source, _ []string) ([]ExpectedFile, error) {
		return []ExpectedFile{{RelPath: "Show/Season 1/e1.mkv", Size: 100, App: "Sonarr"}}, nil
	}
	var sleeps []time.Duration
	h.sync.sleep = func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return nil
	}
	res, st, _ := h.mustTargeted("Show")
	if len(sleeps) != 0 || st.ExpectedMissing != 1 || res.Warnings != 1 || st.FilesCopied != 0 {
		t.Fatalf("sleeps %v stats %+v warnings %d\n%s", sleeps, st, res.Warnings, h.rep.dump())
	}
	want := "Sonarr reports Show/Season 1/e1.mkv (100 B), but Show is spelled show on disk and this sync scans only the *arr's spelling; it is not backed up yet (the next full sync backs it up)"
	if !h.rep.has(want) {
		t.Fatalf("no log %q:\n%s", want, h.rep.dump())
	}
	if h.rep.has("catalogs the names on disk") {
		t.Fatalf("reported as cataloged under the names on disk:\n%s", h.rep.dump())
	}
}

// TestClassifyExpectedListsAgainBeforeCallingANameMisspelled: a file created after its folder's
// listing was cached is found by the lookup but missing from that listing; the folder is listed
// again, so it is waited for (not reported as spelled otherwise on disk).
func TestClassifyExpectedListsAgainBeforeCallingANameMisspelled(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("A/new.mkv", content("n", 100), 1)
	root, err := os.OpenRoot(h.srcDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	stale := folderNames{".": {"A": {}}, "A": {}} // listed before new.mkv was created
	c := classifyExpected(root, newExcluder(nil), stale, ExpectedFile{RelPath: "A/new.mkv", Size: 100}, catalog.LiveFile{}, false)
	if c.state != expectedPending {
		t.Fatalf("state %v reason %q, want pending", c.state, c.reason)
	}
	if _, ok := stale["A"]["new.mkv"]; !ok {
		t.Fatalf("the folder was not listed again: %v", stale)
	}
}

// TestExpectedFileRescansCountWhatAnotherScanCataloged: the files a webhook sync reports for its
// targets are those under them after its last rescan, also when another scan cataloged the late
// file while the source's lock was free (another destination's sync of the source, a scan job).
// Added counts the rows this job's scans added, as for every sync.
func TestExpectedFileRescansCountWhatAnotherScanCataloged(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("A/a.mkv", content("a", 800), 1)
	h.mustSync(false, jobs.Params{})
	late := "A/late.mkv"
	h.sync.expected = func(_ context.Context, _ catalog.Source, _ []string) ([]ExpectedFile, error) {
		return []ExpectedFile{{RelPath: late, Size: 1000, App: "Radarr"}}, nil
	}
	sleeps := 0
	otherScan := true
	h.sync.sleep = func(ctx context.Context, _ time.Duration) error {
		sleeps++
		h.writeSrc(late, content("l", 1000), 2)
		if otherScan {
			if _, err := h.scanner.ScanPaths(ctx, h.src.ID, []string{"A"}, &recReporter{}); err != nil {
				return err
			}
		}
		return nil
	}
	_, st, _ := h.mustTargeted("A")
	if sleeps != 1 || st.FilesCopied != 1 || st.ExpectedMissing != 0 || len(st.Sources) != 1 ||
		st.Sources[0].Files != 2 || st.Sources[0].Added != 0 {
		t.Fatalf("sleeps %d stats %+v\n%s", sleeps, st, h.rep.dump())
	}

	// The job's own rescan catalogs it.
	late, otherScan, sleeps = "A/later.mkv", false, 0
	_, st, _ = h.mustTargeted("A")
	if sleeps != 1 || st.FilesCopied != 1 || len(st.Sources) != 1 || st.Sources[0].Files != 3 || st.Sources[0].Added != 1 {
		t.Fatalf("sleeps %d stats %+v\n%s", sleeps, st, h.rep.dump())
	}
}

// TestTargetedRetainOfAReappearedNameReadsOnlyThatPath: a targeted sync's retain whose vanished
// name came back (a scan cataloged it after the plan) is skipped; it looks up that one path in the
// catalog, not the whole source (a catalog row outside the target that cannot be read, which a
// sync reading the whole source fails on).
func TestTargetedRetainOfAReappearedNameReadsOnlyThatPath(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("A/old.mkv", content("o", 800), 1)
	h.writeSrc("B/b.mkv", content("b", 900), 2)
	h.mustSync(false, jobs.Params{})
	h.dbExec(`UPDATE catalog_files SET last_seen_at = 'not a time' WHERE rel_path = 'B/b.mkv'`)

	h.removeSrc("A/old.mkv")
	h.writeSrc("A/new.mkv", content("n", 1000), 3)
	back := false
	faultinject.SetHook(func(name string) {
		if name != "copy.afterRename" || back {
			return
		}
		back = true
		// Restored after the plan, and cataloged by another scan before the retain runs.
		h.writeSrc("A/old.mkv", content("o", 800), 1)
		if _, err := h.scanner.ScanPaths(h.ctx, h.src.ID, []string{"A"}, &recReporter{}); err != nil {
			t.Error(err)
		}
	})
	defer faultinject.SetHook(nil)
	res, st, j := h.mustTargeted("A")
	faultinject.SetHook(nil)
	if !back || st.FilesCopied != 1 || st.FilesRetained != 0 || st.FilesFailed != 0 || res.Warnings != 0 {
		t.Fatalf("stats %+v warnings %d items %+v\n%s", st, res.Warnings, h.items(j.ID), h.rep.dump())
	}
	skipped := false
	for _, it := range h.items(j.ID) {
		skipped = skipped || (it.Action == jobs.ActionRetain && it.Status == jobs.ItemSkipped && strings.Contains(it.Error, "reappeared at the source"))
	}
	if !skipped {
		t.Fatalf("the retain of the reappeared name was not skipped: %+v", h.items(j.ID))
	}
	if r, ok := h.liveRecord("movies/A/old.mkv"); !ok || r.State != StatePresent {
		t.Fatalf("record %+v %v", r, ok)
	}
}

// TestExcluderMatchesTheScan: the syncer's copy of the exclude matcher agrees with the scan about
// every file below (defaults, which ignore case, and the source's own patterns).
func TestExcluderMatchesTheScan(t *testing.T) {
	h := newHarness(t)
	own := []string{"*.srt", "/Top/", "Sample/", "tmp*", "/A/exact.mkv", "Extras"}
	src, err := h.cat.Update(h.ctx, h.src.ID, catalog.SourceInput{Name: h.src.Name, Path: h.src.Path, Exclude: own})
	if err != nil {
		t.Fatal(err)
	}
	files := []string{
		"A/movie.mkv", "A/movie.srt", "C/movie.SRT", "Top/x.mkv", "A/Top/x.mkv", "A/Sample/s.mkv", "C/sample/s.mkv",
		"B/Sample", "A/tmpfile.mkv", "tmpdir/x.mkv", "A/exact.mkv", "B/A/exact.mkv", "A/Extras/e.mkv", "B/Extras",
		"A/@eaDir/thumb.jpg", "C/@EADIR/thumb.jpg", "A/.DS_Store", "C/.ds_store", "A/._movie.mkv", "A/movie.mkv.partial~",
		"A/movie.PART", ".bunkarr/x", "A/.bunkarr/x", "A/Thumbs.db", "A/lost+found/x", "A/#recycle/x", "A/.Trash-1000/x",
	}
	for i, f := range files {
		h.writeSrc(f, content("c", 10+i), i)
	}
	if _, err := h.scanner.Scan(h.ctx, src.ID, h.rep); err != nil {
		t.Fatal(err)
	}
	match := newExcluder(src.Exclude)
	for _, f := range files {
		comps := strings.Split(f, "/")
		excluded := false
		for i := range comps {
			excluded = excluded || match.excluded(strings.Join(comps[:i+1], "/"), comps[i], i < len(comps)-1)
		}
		live, err := h.cat.LiveFilesAt(h.ctx, nil, []catalog.Location{{SourceID: src.ID, Rel: f}})
		if err != nil {
			t.Fatal(err)
		}
		if cataloged := len(live) == 1; cataloged == excluded {
			t.Errorf("%s: cataloged %v, excluded by the syncer's matcher %v", f, cataloged, excluded)
		}
	}
}

// TestTargetedSyncReadsOnlyTheRecordsOfItsFolders: a webhook sync of an upgrade reads the
// destination records and the catalog rows of its target only, not those of the whole destination
// or source (here a record and a catalog row outside the target that cannot be read, which a
// sync reading everything fails on). The S6 check of its retain and the S11 case check still
// work.
func TestTargetedSyncReadsOnlyTheRecordsOfItsFolders(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file without permissions")
	}
	h := newHarness(t)
	h.setCaps(func(c *filecopy.Capabilities) { c.CaseInsensitive = true })
	h.writeSrc("A/old.mkv", content("o", 800), 1)
	h.writeSrc("B/b.mkv", content("b", 900), 2)
	h.mustSync(false, jobs.Params{})
	err := h.db.Write(h.ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(h.ctx, `UPDATE destination_files SET verified_at = 'not a time' WHERE rel_path = 'movies/B/b.mkv'`); err != nil {
			return err
		}
		_, err := tx.ExecContext(h.ctx, `UPDATE catalog_files SET last_seen_at = 'not a time' WHERE rel_path = 'B/b.mkv'`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	// An upgrade whose new file cannot be copied: the old one is not retained (S6).
	h.removeSrc("A/old.mkv")
	h.writeSrc("A/new.mkv", content("n", 1000), 3)
	if err := os.Chmod(h.srcPath("A/new.mkv"), 0); err != nil {
		t.Fatal(err)
	}
	j := h.targetedJob(jobs.TriggerWebhook, false, "A")
	res, err := h.run(h.sync, j)
	if err != nil {
		t.Fatalf("targeted sync: %v\n%s", err, h.rep.dump())
	}
	st := res.Stats.(SyncStats)
	if st.FilesFailed != 2 || st.FilesRetained != 0 || !h.rep.has("not retained yet: A/new.mkv in the same folder is not backed up") {
		t.Fatalf("stats %+v\n%s", st, h.rep.dump())
	}
	if r, ok := h.liveRecord("movies/A/old.mkv"); !ok || r.State != StatePresent {
		t.Fatalf("old version %+v %v", r, ok)
	}
	// Once it can be copied, the old version is retained after it.
	if err := os.Chmod(h.srcPath("A/new.mkv"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, st, _ = h.mustTargeted("A")
	if st.FilesCopied != 1 || st.FilesRetained != 1 || res.Warnings != 0 {
		t.Fatalf("stats %+v warnings %d\n%s", st, res.Warnings, h.rep.dump())
	}

	// A new name that differs only in case from a record of a removed source is still refused.
	err = h.db.Write(h.ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(h.ctx, `INSERT INTO destination_files (destination_id, source_id, rel_path, source_rel_path, size,
			mtime_ns, state) VALUES (?, NULL, 'MOVIES/a/clash.mkv', 'a/clash.mkv', 1, 1, 'present')`, h.dest.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	h.writeSrc("A/Clash.mkv", content("c", 300), 4)
	j = h.targetedJob(jobs.TriggerWebhook, false, "A")
	if res, err = h.run(h.sync, j); err != nil {
		t.Fatalf("targeted sync: %v\n%s", err, h.rep.dump())
	}
	if st := res.Stats.(SyncStats); st.FilesFailed != 1 || st.FilesCopied != 0 || !h.rep.has("differ only in case") {
		t.Fatalf("stats %+v\n%s", st, h.rep.dump())
	}
}

// TestLiveNearFindsEverySpelling: liveNear returns exactly the live records at or under a path (or
// at a path), up to case on a case-insensitive destination, as filecopy.FoldKey compares.
func TestLiveNearFindsEverySpelling(t *testing.T) {
	h := newHarness(t)
	paths := []string{
		"movies/Heat (1995)/Heat.mkv", "movies/heat (1995)/heat.nfo", "MOVIES/HEAT (1995)/x", "movies/Heat (1995) Extended/h.mkv",
		"movies/Heat (1995)", "movies/Heat (1995)x", "movies/Ran/r.mkv", "movies/\u212aelvin/k.mkv", "movies/kelvin/k2.mkv",
		"movies/\u017ftar/s.mkv", "movies/Star/s2.mkv", "movies/\u00c9t\u00e9/e.mkv", "movies/\u00e9T\u00c9/e2.mkv", "movies/Ete/e3.mkv",
		"tv/Show/S01/e1.mkv", "tv/show/s01/E1.mkv", "tv/Show/S01/e10.mkv",
	}
	err := h.db.Write(h.ctx, func(tx *sql.Tx) error {
		for _, p := range paths {
			if _, err := tx.ExecContext(h.ctx, `INSERT INTO destination_files (destination_id, source_id, rel_path, source_rel_path, size,
				mtime_ns, state) VALUES (?, NULL, ?, ?, 1, 1, 'present')`, h.dest.ID, p, p); err != nil {
				return err
			}
		}
		// A retained record is not live.
		_, err := tx.ExecContext(h.ctx, `INSERT INTO destination_files (destination_id, source_id, rel_path, source_rel_path, size,
			mtime_ns, state, retained_path, expires_at) VALUES (?, NULL, 'movies/heat (1995)/old.mkv', 'x', 1, 1, 'retained', 'r', '2030-01-01T00:00:00Z')`, h.dest.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		under, at []string
	}{
		{under: []string{"movies/Heat (1995)"}},
		{under: []string{"movies/kelvin", "movies/star", "movies/\u00e9t\u00e9"}},
		{under: []string{"TV/SHOW/s01"}, at: []string{"tv/SHOW/S01/E10.MKV", "movies/ran/R.MKV", "movies/none"}},
		{under: []string{"nothing"}},
	} {
		for _, fold := range []bool{false, true} {
			recs, err := h.store.liveNear(h.ctx, h.dest.ID, tc.under, tc.at, fold)
			if err != nil {
				t.Fatal(err)
			}
			var got, want []string
			for _, r := range recs {
				got = append(got, r.RelPath)
			}
			key := func(s string) string {
				if fold {
					return filecopy.FoldKey(s)
				}
				return s
			}
			for _, p := range paths {
				match := false
				for _, u := range tc.under {
					match = match || key(p) == key(u) || strings.HasPrefix(key(p), key(u)+"/")
				}
				for _, a := range tc.at {
					match = match || key(p) == key(a)
				}
				if match {
					want = append(want, p)
				}
			}
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("under %q at %q fold %v:\n got %q\nwant %q", tc.under, tc.at, fold, got, want)
			}
		}
	}
}

// TestScopedQueriesUseIndexRanges: the reads a targeted sync makes of the destination records
// (scopedPlanInput, targetNames, notBackedUpIn) are index ranges on a path, so their cost follows
// the scope: never a pass over every record of the destination or the source. The query plans
// checked are those of the SQL the functions run (Store.traceRead).
func TestScopedQueriesUseIndexRanges(t *testing.T) {
	h := newHarness(t)
	err := h.db.Write(h.ctx, func(tx *sql.Tx) error {
		for _, p := range []string{"A/a.mkv", "A/B/b.mkv", "c.mkv"} {
			if _, err := tx.ExecContext(h.ctx, `INSERT INTO destination_files (destination_id, source_id, rel_path, source_rel_path, size,
				mtime_ns, state) VALUES (?, ?, ?, ?, 1, 1, 'present')`, h.dest.ID, h.src.ID, "movies/"+p, p); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	type read struct {
		query string
		args  []any
	}
	var ran []read
	h.store.traceRead = func(q string, args []any) { ran = append(ran, read{q, slices.Clone(args)}) }
	defer func() { h.store.traceRead = nil }()
	d, s := h.dest.ID, h.src.ID
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"liveForSourceUnder", func() error {
			recs, err := h.store.liveForSourceUnder(h.ctx, d, s, []string{"A", "c.mkv"})
			if err == nil && len(recs) != 3 {
				err = fmt.Errorf("%d records, want 3", len(recs))
			}
			return err
		}},
		{"liveForSourcePath", func() error { _, _, err := h.store.liveForSourcePath(h.ctx, d, s, "c.mkv"); return err }},
		{"forSourceUnder", func() error { _, err := h.store.forSourceUnder(h.ctx, d, s, "A"); return err }},
		{"liveNear", func() error {
			_, err := h.store.liveNear(h.ctx, d, []string{"movies/A"}, []string{"movies/c.mkv"}, false)
			return err
		}},
		{"liveNear folded", func() error {
			recs, err := h.store.liveNear(h.ctx, d, []string{"MOVIES/a"}, []string{"Movies/C.mkv"}, true)
			if err == nil && len(recs) != 3 {
				err = fmt.Errorf("%d records, want 3", len(recs))
			}
			return err
		}},
	} {
		ran = nil
		if err := tc.call(); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(ran) == 0 {
			t.Fatalf("%s ran no traced read", tc.name)
		}
		for _, r := range ran {
			rows, err := h.db.Reader().QueryContext(h.ctx, "EXPLAIN QUERY PLAN "+r.query, r.args...)
			if err != nil {
				t.Fatal(err)
			}
			var plan []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				plan = append(plan, detail)
			}
			if err := rows.Close(); err != nil {
				t.Fatal(err)
			}
			ranged := false
			for _, p := range plan {
				switch {
				case strings.HasPrefix(p, "SCAN destination_files"):
					t.Errorf("%s: %s\nplan %q reads every record", tc.name, r.query, plan)
				case strings.HasPrefix(p, "SEARCH destination_files"):
					// rel_path or source_rel_path, equal or in a range.
					if !strings.Contains(p, "rel_path=?") && !strings.Contains(p, "rel_path>?") {
						t.Errorf("%s: %s\nplan %q: a search without a path reads every record of the destination or source", tc.name, r.query, plan)
					}
					ranged = true
				}
			}
			if !ranged {
				t.Errorf("%s: %s\nplan %q, want an index range on a path", tc.name, r.query, plan)
			}
		}
	}
}
