package syncer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

func TestInitialSyncThenOnlyChanges(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("a.mkv", content("a", 3000), 1)
	h.writeSrc("dir/b.mkv", content("b", 5000), 2)
	h.writeSrc("dir/sub/c.srt", content("c", 100), 3)
	h.writeSrc("gone.mkv", content("g", 700), 4)
	h.writeSrc("rename-me.mkv", content("r", 1234), 5)

	res, st, _ := h.mustSync(false, jobs.Params{})
	if st.FilesCopied != 5 || st.FilesFailed != 0 || res.Warnings != 0 {
		t.Fatalf("first sync: %+v warnings %d\n%s", st, res.Warnings, h.rep.dump())
	}
	if st.BytesPlanned != 3000+5000+100+700+1234 || st.BytesCopied != st.BytesPlanned {
		t.Errorf("bytes planned %d copied %d", st.BytesPlanned, st.BytesCopied)
	}
	h.assertConverged(t)
	for _, r := range h.records() {
		if !strings.HasPrefix(r.Hash, filecopy.HashPrefix) {
			t.Errorf("%s has no hash (verify mode sample hashes while copying)", r.RelPath)
		}
	}

	// Nothing changed: nothing planned.
	_, st, j := h.mustSync(false, jobs.Params{})
	if st.FilesPlanned != 0 || st.BytesCopied != 0 {
		t.Fatalf("unchanged source: %+v items %v", st, h.items(j.ID))
	}

	// One changed, one added, one deleted, one renamed (design §9 E2E 1).
	h.writeSrc("a.mkv", content("A", 3100), 10)
	h.writeSrc("new.mkv", content("n", 222), 11)
	h.removeSrc("gone.mkv")
	h.renameSrc("rename-me.mkv", "renamed/it.mkv")
	res, st, j = h.mustSync(false, jobs.Params{})
	want := SyncStats{FilesPlanned: 4, FilesCopied: 1, FilesUpdated: 1, FilesMoved: 1, FilesRetained: 1, BytesPlanned: 222 + 3100, BytesCopied: 222 + 3100}
	st.DurationMs, st.Sources = 0, nil
	if !reflect.DeepEqual(st, want) {
		t.Fatalf("second sync stats\n got %+v\nwant %+v\nitems %+v", st, want, h.items(j.ID))
	}
	if res.Warnings != 0 {
		t.Errorf("warnings %d", res.Warnings)
	}
	h.assertConverged(t)
	if !h.hasContent(t, content("a", 3000)) || !h.hasContent(t, content("g", 700)) {
		t.Error("the replaced and the deleted versions must be in retention")
	}
	if exists(h.dstPath("movies/rename-me.mkv")) || !exists(h.dstPath("movies/renamed/it.mkv")) {
		t.Error("the move did not rename at the destination")
	}
	reasons := map[string]string{}
	for _, r := range h.records() {
		if r.State == StateRetained {
			reasons[r.RelPath] = r.Reason
			if !strings.HasPrefix(r.RetainedPath, filecopy.RetentionDir(j.QueuedAt, j.ID)+"/movies/") {
				t.Errorf("retained at %s, want in the job's retention directory", r.RetainedPath)
			}
			if r.ExpiresAt == nil || r.ExpiresAt.Sub(*r.RetainedAt) != 30*24*time.Hour {
				t.Errorf("expiry %v after retention %v, want 30 days", r.ExpiresAt, r.RetainedAt)
			}
		}
	}
	if !reflect.DeepEqual(reasons, map[string]string{"movies/a.mkv": ReasonReplaced, "movies/gone.mkv": ReasonDeleted}) {
		t.Errorf("retained reasons %v", reasons)
	}
	if want := []string{"scanning", "planning", "copying", "retaining"}; !reflect.DeepEqual(h.rep.phases()[len(h.rep.phases())-4:], want) {
		t.Errorf("phases %v", h.rep.phases())
	}

	// And again: converged, nothing to do.
	_, st, _ = h.mustSync(false, jobs.Params{})
	if st.FilesPlanned != 0 {
		t.Fatalf("third sync planned %d items", st.FilesPlanned)
	}
	if exists(h.dstPath("movies/rename-me.mkv")) {
		t.Error("empty directories of moved files are pruned, files never")
	}
}

func TestHardlinkedPairCopiedOnce(t *testing.T) {
	for _, mode := range []destinations.HardlinkMode{destinations.HardlinksRecreate, destinations.HardlinksCopy} {
		t.Run(string(mode), func(t *testing.T) {
			h := newHarness(t)
			h.setSettings(func(s *destinations.Settings) { s.Hardlinks = mode })
			h.writeSrc("torrents/movie.mkv", content("m", 4096), 1)
			h.linkSrc("torrents/movie.mkv", "library/Movie (2020)/movie.mkv")
			h.writeSrc("single.nfo", content("s", 10), 2)
			_, st, j := h.mustSync(false, jobs.Params{})
			src, err := h.cat.Get(h.ctx, h.src.ID)
			if err != nil {
				t.Fatal(err)
			}
			if st.BytesPlanned != 4096+10 || st.BytesPlanned != src.Stats.UniqueBytes || st.BytesCopied != st.BytesPlanned {
				t.Fatalf("bytes planned %d copied %d, unique %d", st.BytesPlanned, st.BytesCopied, src.Stats.UniqueBytes)
			}
			if st.FilesCopied != 2 || st.FilesLinked != 1 {
				t.Fatalf("stats %+v items %+v", st, h.items(j.ID))
			}
			h.assertConverged(t)
			lib, tor := h.dstPath("movies/library/Movie (2020)/movie.mkv"), h.dstPath("movies/torrents/movie.mkv")
			r, _ := h.liveRecord("movies/torrents/movie.mkv")
			switch mode {
			case destinations.HardlinksRecreate:
				if inode(t, lib) != inode(t, tor) {
					t.Error("recreate: the names must be one inode at the destination")
				}
			case destinations.HardlinksCopy:
				if exists(tor) == exists(lib) {
					t.Error("copy: exactly one name holds the content")
				}
			}
			_ = r
		})
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("a.mkv", content("a", 300), 1)
	h.writeSrc("b.mkv", content("b", 300), 2)
	h.mustSync(false, jobs.Params{})
	h.writeSrc("a.mkv", content("A", 301), 3)
	h.writeSrc("c.mkv", content("c", 300), 4)
	h.removeSrc("b.mkv")
	h.writeSrc("x.mkv", content("x", 50), 5)
	writeFileAt(t, h.dstPath("movies/x.mkv"), "unmanaged", baseTime) // would be displaced
	before := tree(t, h.dstDir)
	recsBefore := h.records()

	res, st, j := h.mustSync(true, jobs.Params{})
	if !st.DryRun || st.FilesCopied != 2 || st.FilesUpdated != 1 || st.FilesRetained != 1 {
		t.Fatalf("dry run stats %+v items %+v", st, h.items(j.ID))
	}
	if !strings.HasPrefix(res.Summary, "Dry run: would") {
		t.Errorf("summary %q", res.Summary)
	}
	if after := tree(t, h.dstDir); !reflect.DeepEqual(before, after) {
		t.Fatalf("a dry run changed the destination:\nbefore %v\nafter  %v", before, after)
	}
	if !reflect.DeepEqual(recsBefore, h.records()) {
		t.Fatal("a dry run changed the records")
	}
	for _, it := range h.items(j.ID) {
		if it.Status != jobs.ItemPending {
			t.Errorf("preview item %s %s is %s", it.Action, it.RelPath, it.Status)
		}
		if it.RelPath == "movies/x.mkv" {
			var d Detail
			_ = jsonUnmarshal(it.Detail, &d)
			if !d.Displace {
				t.Error("the preview shows the displacement")
			}
		}
	}
}

func TestMarkerRemovedFailsWithoutWriting(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("a.mkv", content("a", 300), 1)
	if err := os.Remove(h.dstPath(filecopy.MarkerRel)); err != nil {
		t.Fatal(err)
	}
	before := tree(t, h.dstDir)
	j := h.newJob(jobs.TypeSync, false, jobs.Params{})
	_, err := h.run(h.sync, j)
	if !errors.Is(err, destinations.ErrNotMounted) {
		t.Fatalf("err = %v, want ErrNotMounted", err)
	}
	if after := tree(t, h.dstDir); !reflect.DeepEqual(before, after) {
		t.Fatalf("the failed sync wrote to the destination: %v", after)
	}
	if len(h.items(j.ID)) != 0 || len(h.records()) != 0 {
		t.Fatal("nothing is planned or recorded")
	}
}

func TestMarkerRemovedMidJobStopsTheJob(t *testing.T) {
	h := newHarness(t)
	for i := range 12 {
		h.writeSrc(path.Join("d", string(rune('a'+i))+".mkv"), content(string(rune('a'+i)), 100+i), i)
	}
	h.sync.recheckEvery = 3
	calls := 0
	h.sync.freeSpace = func(r *os.Root) (uint64, uint64, error) {
		calls++
		return filecopy.FreeSpace(r)
	}
	j := h.newJob(jobs.TypeSync, false, jobs.Params{})
	// Remove the marker after planning: the first re-check (after 3 items) stops the job.
	h.sync.freeSpace = func(r *os.Root) (uint64, uint64, error) {
		if err := os.Remove(h.dstPath(filecopy.MarkerRel)); err != nil {
			t.Fatal(err)
		}
		return 1 << 50, 1 << 50, nil
	}
	_, err := h.run(h.sync, j)
	if !errors.Is(err, destinations.ErrNotMounted) {
		t.Fatalf("err = %v", err)
	}
	done := itemsBy(h.items(j.ID), jobs.ActionCopy, jobs.ItemDone)
	if len(done) != 3 {
		t.Fatalf("%d items ran before the re-check, want 3", len(done))
	}
}

func TestFreeSpaceGuard(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("a.mkv", content("a", 3000), 1)
	h.sync.freeSpace = func(*os.Root) (uint64, uint64, error) { return 1<<30 + 2000, 1 << 40, nil }
	j := h.newJob(jobs.TypeSync, false, jobs.Params{})
	_, err := h.run(h.sync, j)
	if err == nil || !strings.Contains(err.Error(), "not enough free space") {
		t.Fatalf("err = %v", err)
	}
	if n := len(regularFiles(t, h.dstDir)); n != 1 { // the marker
		t.Fatalf("%d files at the destination", n)
	}
	// A dry run reports it.
	res, _, _ := h.mustSync(true, jobs.Params{})
	if res.Warnings == 0 || !h.rep.has("not enough free space") {
		t.Fatalf("dry run warnings %d", res.Warnings)
	}
}

func TestResumeOfPlannedJobSkipsScanAndPlan(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("a.mkv", content("a", 300), 1)
	j := h.newJob(jobs.TypeSync, true, jobs.Params{})
	if _, err := h.sync.Run(h.ctx, j, h.env()); err != nil {
		t.Fatal(err)
	}
	// A new file after planning is not part of this job's plan.
	h.writeSrc("b.mkv", content("b", 300), 2)
	j.Attempt, j.Trigger = 2, jobs.TriggerResume
	res, err := h.sync.Run(h.ctx, j, h.env())
	if err != nil {
		t.Fatal(err)
	}
	if st := res.Stats.(SyncStats); st.FilesPlanned != 1 {
		t.Fatalf("resumed plan has %d items", st.FilesPlanned)
	}
}

func TestDisabledDestinationAndSources(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("a.mkv", content("a", 300), 1)
	disabled := false
	if _, err := h.cat.Update(h.ctx, h.src.ID, sourceInput(h, &disabled)); err != nil {
		t.Fatal(err)
	}
	res, st, _ := h.mustSync(false, jobs.Params{})
	if st.FilesPlanned != 0 || !strings.HasPrefix(res.Summary, "Up to date") {
		t.Fatalf("a disabled source is not synced: %+v %q", st, res.Summary)
	}
	if _, err := h.dests.Update(h.ctx, h.dest.ID, destinations.Input{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	j := h.newJob(jobs.TypeSync, false, jobs.Params{})
	if _, err := h.run(h.sync, j); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("err = %v", err)
	}
}

func TestCancelRemovesTempFiles(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("big.mkv", content("b", 3<<20), 1)
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	j := h.newJob(jobs.TypeSync, false, jobs.Params{})
	rep := &cancelReporter{recReporter: h.rep, cancel: cancel}
	_, err := h.sync.Run(ctx, j, jobs.Env{Reporter: rep, Items: h.jq})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if !rep.fired {
		t.Fatal("the job was not cancelled while copying")
	}
	var d Detail
	it := h.items(j.ID)[0]
	if err := json.Unmarshal(it.Detail, &d); err != nil || d.Temp == "" || it.Status != jobs.ItemPending {
		t.Fatalf("the temp path is recorded before the temp file exists: %+v %v", it, err)
	}
	for rel := range regularFiles(t, h.dstDir) {
		if strings.Contains(rel, filecopy.TempPrefix) {
			t.Fatalf("temp file left after cancel: %s", rel)
		}
	}
}

// cancelReporter cancels the job at the first byte copied.
type cancelReporter struct {
	*recReporter
	cancel func()
	fired  bool
}

func (c *cancelReporter) Progress(p jobs.Progress) {
	if p.Phase == "copying" && p.BytesDone > 0 {
		c.fired = true
		c.cancel()
	}
	c.recReporter.Progress(p)
}

func TestHasBackups(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("a.mkv", content("a", 30), 1)
	if has, err := h.store.HasBackups(h.ctx, h.src.ID); err != nil || has {
		t.Fatalf("before: %v %v", has, err)
	}
	h.mustSync(false, jobs.Params{})
	if has, err := h.store.HasBackups(h.ctx, h.src.ID); err != nil || !has {
		t.Fatalf("after: %v %v", has, err)
	}
	// The catalog refuses a destFolder change now.
	in := sourceInput(h, nil)
	in.DestFolder = "other"
	if _, err := h.cat.Update(h.ctx, h.src.ID, in); err == nil {
		t.Fatal("destFolder changed although a destination holds backups")
	}
}

func TestSourceNoLongerLinkedIsNeverTouched(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("a.mkv", content("a", 30), 1)
	h.mustSync(false, jobs.Params{})
	before := h.records()
	// Unlink the source and delete its files: the records are orphans now.
	if err := h.dests.SetSources(h.ctx, h.dest.ID, nil); err != nil {
		t.Fatal(err)
	}
	h.removeSrc("a.mkv")
	_, st, _ := h.mustSync(false, jobs.Params{})
	if st.FilesPlanned != 0 {
		t.Fatalf("an unlinked source was planned: %+v", st)
	}
	if !reflect.DeepEqual(before, h.records()) || !exists(h.dstPath("movies/a.mkv")) {
		t.Fatal("orphan records or files changed")
	}
	// Deleting the source makes source_id NULL: still untouched.
	if err := h.cat.Delete(h.ctx, h.src.ID); err != nil {
		t.Fatal(err)
	}
	recs := h.records()
	if len(recs) != 1 || recs[0].SourceID != 0 || recs[0].State != StatePresent {
		t.Fatalf("records after source deletion: %+v", recs)
	}
}

func TestOrphanPathIsNeverOverwritten(t *testing.T) {
	h := newHarness(t)
	h.writeSrc("a.mkv", content("a", 30), 1)
	h.mustSync(false, jobs.Params{})
	// Delete the source (its records become orphans), then create a new source with the same
	// destination folder and a file at the same path.
	if err := h.dests.SetSources(h.ctx, h.dest.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.cat.Delete(h.ctx, h.src.ID); err != nil {
		t.Fatal(err)
	}
	dir2 := resolvedTempDir(t)
	writeFileAt(t, dir2+"/a.mkv", content("z", 31), baseTime)
	src2, err := h.cat.Create(h.ctx, sourceInputFor("Movies 2", dir2, "movies"))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.dests.SetSources(h.ctx, h.dest.ID, []int64{src2.ID}); err != nil {
		t.Fatal(err)
	}
	res, st, j := h.mustSync(false, jobs.Params{})
	if st.FilesFailed != 1 || res.Warnings == 0 {
		t.Fatalf("stats %+v items %+v", st, h.items(j.ID))
	}
	if fileHash(t, h.dstPath("movies/a.mkv")) != strHash(content("a", 30)) {
		t.Fatal("the orphan's file was overwritten")
	}
}

// jsonUnmarshal decodes an item detail.
func jsonUnmarshal(raw []byte, v any) error { return jsonDecode(raw, v) }

// dbExec runs a statement in a write transaction (test fixtures).
func (h *harness) dbExec(q string, args ...any) {
	h.t.Helper()
	err := h.db.Write(h.ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(h.ctx, q, args...)
		return err
	})
	if err != nil {
		h.t.Fatal(err)
	}
}

// TestRetainedLookupUsesAnIndex: every update, displace and relink looks up its retained row inside
// the write transaction; the lookup must not visit every retained row of the destination, also for
// an orphan row (no source).
func TestRetainedLookupUsesAnIndex(t *testing.T) {
	h := newHarness(t)
	for _, sourceID := range []int64{h.src.ID, 0} {
		rec := Record{DestinationID: h.dest.ID, SourceID: sourceID, SourceRelPath: "a.mkv", RetainedPath: ".bunkarr/retention/x/movies/a.mkv"}
		rows, err := h.db.Reader().QueryContext(h.ctx, `EXPLAIN QUERY PLAN `+retainedLookup, retainedLookupArgs(rec)...)
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
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()
		if got := strings.Join(plan, "; "); !strings.Contains(got, "destination_files_source") {
			t.Fatalf("source %d: query plan %q: want the (destination, source, source path) index", sourceID, got)
		}
	}
}
