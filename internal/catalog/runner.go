package catalog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// ScanRunner runs scan jobs (jobs.TypeScan).
type ScanRunner struct {
	scanner *Scanner
}

// NewScanRunner returns the scan job runner.
func NewScanRunner(sc *Scanner) *ScanRunner {
	return &ScanRunner{scanner: sc}
}

// ScanJobStats is a scan job's stats JSON.
type ScanJobStats struct {
	SourcesScanned int64        `json:"sourcesScanned"`
	SourcesFailed  int64        `json:"sourcesFailed"`
	Files          int64        `json:"files"`
	Bytes          int64        `json:"bytes"`
	Added          int64        `json:"added"`
	Changed        int64        `json:"changed"`
	Deleted        int64        `json:"deleted"`
	Skipped        int64        `json:"skipped"`
	Excluded       int64        `json:"excluded"`
	Groups         int64        `json:"groups"`
	DurationMs     int64        `json:"durationMs"`
	Sources        []ScanResult `json:"sources"`
}

// Run scans job.Params.SourceIDs (when empty: every enabled source; explicitly named sources are
// scanned even when disabled). Sources are scanned one after another. A source whose scan fails
// (for example refused by S10a) is logged and the others are still scanned; the job then fails
// with every source's error. A scan writes only the catalog, so a dry-run scan job is an ordinary
// scan, and a resumed job simply scans again. Cancellation returns ctx.Err().
func (r *ScanRunner) Run(ctx context.Context, job jobs.Job, env jobs.Env) (jobs.Result, error) {
	rep := env.Reporter
	if rep == nil {
		rep = nopReporter{}
	}
	ids := job.Params.SourceIDs
	if len(ids) == 0 {
		srcs, err := r.scanner.store.List(ctx)
		if err != nil {
			return jobs.Result{}, err
		}
		for _, s := range srcs {
			if s.Enabled {
				ids = append(ids, s.ID)
			}
		}
		if len(ids) == 0 {
			rep.Log(slog.LevelInfo, "no enabled sources to scan")
			return jobs.Result{Stats: ScanJobStats{Sources: []ScanResult{}}, Summary: "No enabled sources to scan"}, nil
		}
	}
	stats := ScanJobStats{Sources: []ScanResult{}}
	warnings := 0
	var errs []error
	for _, id := range ids {
		res, err := r.scanner.Scan(ctx, id, rep)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return jobs.Result{}, ctxErr
		}
		if err != nil {
			stats.SourcesFailed++
			rep.Log(slog.LevelError, "scan failed", "sourceId", id, "error", err.Error())
			errs = append(errs, err)
			continue
		}
		stats.SourcesScanned++
		stats.Files += res.Files
		stats.Bytes += res.Bytes
		stats.Added += res.Added
		stats.Changed += res.Changed
		stats.Deleted += res.Deleted
		stats.Skipped += res.SkippedTotal()
		stats.Excluded += res.Excluded
		stats.Groups += res.Groups
		stats.DurationMs += res.DurationMs
		stats.Sources = append(stats.Sources, res)
		warnings += res.WarningCount
		rep.Log(slog.LevelInfo, "scan finished", "source", res.SourceName, "files", res.Files, "added", res.Added,
			"changed", res.Changed, "deleted", res.Deleted, "skipped", res.SkippedTotal(), "warnings", res.WarningCount)
	}
	if len(errs) > 0 {
		return jobs.Result{}, errors.Join(errs...)
	}
	return jobs.Result{Stats: stats, Warnings: warnings, Summary: scanSummary(stats)}, nil
}

func scanSummary(st ScanJobStats) string {
	var b strings.Builder
	noun := "sources"
	if st.SourcesScanned == 1 {
		noun = "source"
	}
	fmt.Fprintf(&b, "Scanned %d %s: %d files (%s), %d added, %d changed, %d deleted",
		st.SourcesScanned, noun, st.Files, formatBytes(st.Bytes), st.Added, st.Changed, st.Deleted)
	if st.Skipped > 0 {
		fmt.Fprintf(&b, ", %d skipped", st.Skipped)
	}
	return b.String()
}

// formatBytes renders n in binary units (1.5 GiB).
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
