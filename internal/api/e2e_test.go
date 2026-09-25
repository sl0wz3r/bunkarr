package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/syncer"
)

// syncStats decodes a sync job's stats.
func syncStats(t *testing.T, j jobs.Job) syncer.SyncStats {
	t.Helper()
	var st syncer.SyncStats
	if err := json.Unmarshal(j.Stats, &st); err != nil {
		t.Fatalf("sync stats %s: %v", j.Stats, err)
	}
	return st
}

// entries lists a directory's names.
func entries(t *testing.T, dir string) []string {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range list {
		out = append(out, e.Name())
	}
	return out
}

// TestEndToEndSync drives a backup through the API only: a source and a destination are created,
// a dry run previews the plan and writes nothing, a sync copies (a hardlinked pair once), a second
// sync copies nothing, a change is picked up, and a verify passes.
func TestEndToEndSync(t *testing.T) {
	e := newEnv(t, nil)
	mt := time.Date(2025, 3, 14, 15, 9, 26, 535897932, time.UTC)
	src := e.mkdir(t, "media/movies")
	writeFile(t, filepath.Join(src, "A (2020)", "A.mkv"), strings.Repeat("a", 1000), mt)
	writeFile(t, filepath.Join(src, "B (2021)", "B.mkv"), strings.Repeat("b", 2000), mt)
	if err := os.Link(filepath.Join(src, "B (2021)", "B.mkv"), filepath.Join(src, "B (2021)", "B-hardlink.mkv")); err != nil {
		t.Fatal(err)
	}
	dst := e.mkdir(t, "nas/backup")
	srcID := e.createSource(t, "Movies", src)
	destID := e.createDestination(t, "NAS", dst, []int64{srcID}, nil)
	syncPath := fmt.Sprintf("/destinations/%d/sync", destID)

	// Dry run: the plan is persisted as items, the destination is not touched.
	var j jobs.Job
	e.call(t, 202, "POST", syncPath, map[string]any{"dryRun": true}, &j)
	if j.Type != jobs.TypeSync || !j.DryRun || j.Params.DestinationID != destID || j.Status != jobs.StatusQueued {
		t.Fatalf("queued dry run: %+v", j)
	}
	dry := e.waitJob(t, j.ID)
	if dry.Status != jobs.StatusCompleted {
		t.Fatalf("dry run: %s %s", dry.Status, dry.Error)
	}
	// A dry run counts what would happen; it copies no bytes.
	if st := syncStats(t, dry); !st.DryRun || st.FilesPlanned != 3 || st.FilesCopied != 2 || st.FilesLinked != 1 || st.BytesPlanned != 3000 || st.BytesCopied != 0 {
		t.Fatalf("dry run stats: %s", dry.Stats)
	}
	if got := entries(t, dst); len(got) != 1 || got[0] != ".bunkarr" {
		t.Fatalf("dry run wrote to the destination: %v", got)
	}
	var items jobqueue.Page[jobs.Item]
	e.call(t, 200, "GET", fmt.Sprintf("/jobs/%d/items?action=copy&pageSize=10", dry.ID), nil, &items)
	if items.TotalRecords != 2 || len(items.Records) != 2 || items.Records[0].Status != jobs.ItemSkipped && items.Records[0].Status != jobs.ItemPending {
		t.Fatalf("dry run copy items: %+v", items)
	}
	var summary []jobs.ItemCount
	e.call(t, 200, "GET", fmt.Sprintf("/jobs/%d/items/summary", dry.ID), nil, &summary)
	var files, bytes int64
	for _, c := range summary {
		files += c.Files
		bytes += c.Bytes
	}
	if files != 3 || bytes != 3000 {
		t.Fatalf("dry run item summary: %+v", summary)
	}

	// Real sync.
	e.call(t, 202, "POST", syncPath, map[string]any{"dryRun": false}, &j)
	done := e.waitJob(t, j.ID)
	if done.Status != jobs.StatusCompleted {
		t.Fatalf("sync: %s %s", done.Status, done.Error)
	}
	st := syncStats(t, done)
	if st.FilesCopied != 2 || st.FilesLinked != 1 || st.BytesPlanned != 3000 || st.BytesCopied != 3000 {
		t.Fatalf("sync stats: %s", done.Stats)
	}
	for rel, want := range map[string]string{
		"movies/A (2020)/A.mkv":          strings.Repeat("a", 1000),
		"movies/B (2021)/B.mkv":          strings.Repeat("b", 2000),
		"movies/B (2021)/B-hardlink.mkv": strings.Repeat("b", 2000),
	} {
		got, err := os.ReadFile(filepath.Join(dst, rel))
		if err != nil || string(got) != want {
			t.Fatalf("%s at the destination: %v (%d bytes)", rel, err, len(got))
		}
		fi, err := os.Stat(filepath.Join(dst, rel))
		if err != nil || !fi.ModTime().Equal(mt) {
			t.Errorf("%s mtime %v, want %v", rel, fi.ModTime(), mt)
		}
	}
	var srcView catalog.Source
	e.call(t, 200, "GET", fmt.Sprintf("/sources/%d", srcID), nil, &srcView)
	if srcView.Stats.Files != 3 || srcView.Stats.UniqueBytes != 3000 || srcView.Stats.Bytes != 5000 || srcView.Stats.HardlinkGroups != 1 {
		t.Fatalf("source stats after the sync's scan: %+v", srcView.Stats)
	}
	var cat catalog.GlobalStats
	e.call(t, 200, "GET", "/catalog/stats", nil, &cat)
	if cat.Sources != 1 || cat.Files != 3 || cat.UniqueBytes != 3000 {
		t.Fatalf("catalog stats: %+v", cat)
	}

	// Nothing changed: nothing is copied.
	e.call(t, 202, "POST", syncPath, nil, &j)
	again := e.waitJob(t, j.ID)
	if st := syncStats(t, again); again.Status != jobs.StatusCompleted || st.FilesCopied != 0 || st.FilesUpdated != 0 || st.BytesCopied != 0 {
		t.Fatalf("second sync: %s %s", again.Status, again.Stats)
	}

	// One new file: only it is copied.
	writeFile(t, filepath.Join(src, "C (2022)", "C.mkv"), strings.Repeat("c", 500), mt)
	e.call(t, 202, "POST", syncPath, map[string]any{}, &j)
	third := e.waitJob(t, j.ID)
	if st := syncStats(t, third); third.Status != jobs.StatusCompleted || st.FilesCopied != 1 || st.BytesCopied != 500 {
		t.Fatalf("third sync: %s %s", third.Status, third.Stats)
	}

	// Verify.
	e.call(t, 202, "POST", fmt.Sprintf("/destinations/%d/verify", destID), nil, &j)
	if v := e.waitJob(t, j.ID); v.Status != jobs.StatusCompleted || v.Type != jobs.TypeVerify {
		t.Fatalf("verify: %s %s", v.Status, v.Error)
	}

	// History, logs and the destination's last job.
	var history jobqueue.Page[jobs.Job]
	e.call(t, 200, "GET", "/jobs?state=finished&type=sync&pageSize=2", nil, &history)
	if history.TotalRecords != 4 || len(history.Records) != 2 || history.Records[0].ID != third.ID {
		t.Fatalf("history: total %d, %d records, first %d", history.TotalRecords, len(history.Records), history.Records[0].ID)
	}
	var logs []jobqueue.LogEntry
	e.call(t, 200, "GET", fmt.Sprintf("/jobs/%d/logs?limit=500", done.ID), nil, &logs)
	if len(logs) == 0 {
		t.Fatal("the sync has no log lines")
	}
	var after []jobqueue.LogEntry
	e.call(t, 200, "GET", fmt.Sprintf("/jobs/%d/logs?afterId=%d", done.ID, logs[len(logs)-1].ID), nil, &after)
	if len(after) != 0 {
		t.Fatalf("logs after the last id: %+v", after)
	}
	var d destinationView
	e.call(t, 200, "GET", fmt.Sprintf("/destinations/%d", destID), nil, &d)
	if d.LastJob == nil || d.LastJob.Type != jobs.TypeVerify || d.LastJob.Status != jobs.StatusCompleted {
		t.Fatalf("destination's last job: %+v", d.LastJob)
	}
}
