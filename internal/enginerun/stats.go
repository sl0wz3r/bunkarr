package enginerun

import (
	"fmt"
	"strings"

	"github.com/sl0wz3r/bunkarr/internal/syncer"
)

// Stats of engine jobs (§11.4): the Phase 1-3 keys plus the engine's.

// SnapshotStat is one restic snapshot a sync made (per source and batch).
type SnapshotStat struct {
	SourceID        int64  `json:"sourceId"`
	SnapshotID      string `json:"snapshotId"`
	Batch           int    `json:"batch"`
	FilesNew        int64  `json:"filesNew"`
	FilesChanged    int64  `json:"filesChanged"`
	FilesUnmodified int64  `json:"filesUnmodified"`
	DataAdded       int64  `json:"dataAdded"`
}

// SyncStats is an engine sync's stats JSON: the Phase 1-3 sync stats plus the engine's.
type SyncStats struct {
	syncer.SyncStats
	Engine  string `json:"engine"`
	Batches int    `json:"batches"`
	// BytesUploaded is restic's data_added_packed summed over the job's snapshots, or the bytes
	// rclone reported transferred by this attempt.
	BytesUploaded int64 `json:"bytesUploaded"`
	// BytesRead is restic's total_bytes_processed summed over the job's snapshots.
	BytesRead int64          `json:"bytesRead"`
	Snapshots []SnapshotStat `json:"snapshots"`
	// Unchanged: nothing needed a new snapshot (restic).
	Unchanged bool `json:"unchanged"`
	// Deferrals is how often a transfer window deferred the job.
	Deferrals int `json:"deferrals"`
	// LimitKiBps is the upload limit in force when the job finished (0: none).
	LimitKiBps int64 `json:"limitKiBps"`
}

// CheckStat is a restic verify's repository check.
type CheckStat struct {
	NumErrors int `json:"numErrors"`
	// ReadSubset is the --read-data-subset read ("2/20"), "all" for --read-data, "" for a
	// structure check.
	ReadSubset string `json:"readSubset"`
}

// VerifyStats is an engine verify's stats JSON.
type VerifyStats struct {
	syncer.VerifyStats
	Engine          string     `json:"engine"`
	Check           *CheckStat `json:"check,omitempty"`
	SampleFiles     int64      `json:"sampleFiles"`
	SampleBytes     int64      `json:"sampleBytes"`
	VersionsChecked int64      `json:"versionsChecked"`
}

// RetentionStats is an engine retention job's stats JSON.
type RetentionStats struct {
	syncer.RetentionStats
	Engine             string `json:"engine"`
	SnapshotsForgotten int64  `json:"snapshotsForgotten"`
	SnapshotsKept      int64  `json:"snapshotsKept"`
	ForgetRequests     int64  `json:"forgetRequests"`
	Pruned             bool   `json:"pruned"`
	PruneDurationMs    int64  `json:"pruneDurationMs"`
	Cleanup            bool   `json:"cleanup"`
}

// resticSyncSummary is the one-sentence summary of a restic sync ("Backed up 1,234 files
// (12.3 GiB new, 2 batches) to B2; snapshot 1a2b3c4d").
func resticSyncSummary(st SyncStats, destName string) string {
	if st.DryRun {
		return syncer.SyncSummary(st.SyncStats)
	}
	files := st.FilesCopied + st.FilesUpdated + st.FilesMoved + st.FilesLinked + st.FilesPromoted
	var b strings.Builder
	if st.Unchanged || len(st.Snapshots) == 0 {
		fmt.Fprintf(&b, "Nothing new to back up to %s; no snapshot needed", destName)
	} else {
		batches := "1 batch"
		if st.Batches != 1 {
			batches = fmt.Sprintf("%d batches", st.Batches)
		}
		fmt.Fprintf(&b, "Backed up %s files (%s new, %s) to %s; snapshot %s", formatCount(files), formatBytes(st.BytesUploaded),
			batches, destName, short(st.Snapshots[len(st.Snapshots)-1].SnapshotID))
	}
	appendCounts(&b, st.SyncStats)
	return b.String()
}

// rcloneSyncSummary is the one-sentence summary of an rclone sync.
func rcloneSyncSummary(st SyncStats, destName string) string {
	s := syncer.SyncSummary(st.SyncStats)
	if st.DryRun {
		return s
	}
	return s + " (" + destName + ")"
}

func appendCounts(b *strings.Builder, st syncer.SyncStats) {
	if st.FilesRetained > 0 {
		fmt.Fprintf(b, "; %d retained", st.FilesRetained)
	}
	if st.FilesHeld > 0 {
		fmt.Fprintf(b, "; %d held", st.FilesHeld)
	}
	if st.FilesFailed > 0 {
		fmt.Fprintf(b, "; %d failed", st.FilesFailed)
	}
}
