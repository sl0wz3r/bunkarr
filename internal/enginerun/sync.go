package enginerun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/syncer"
)

// runSync runs a sync job of a restic or rclone destination: the shared preflight, the engine's
// sync, and the manifest export that follows a full sync as on filecopy (phase2-3.md §9.2): also
// after a failed sync, never after a deferral, a dry run, a targeted sync or a cancellation.
func (s *Service) runSync(ctx context.Context, kind engines.Kind, job jobs.Job, env jobs.Env) (jobs.Result, error) {
	r, err := s.prepare(ctx, kind, job, env, "sync")
	var res jobs.Result
	if err == nil {
		if kind == engines.Restic {
			res, err = s.resticSync(ctx, r)
		} else {
			res, err = s.rcloneSync(ctx, r)
		}
		err = r.redactErr(err)
	}
	if _, deferred := jobs.AsDeferred(err); deferred {
		if r != nil {
			r.deferralWarning(ctx)
		}
		return jobs.Result{}, err
	}
	if id := s.planner.QueueManifestExport(ctx, job, err); id != 0 {
		if st, ok := res.Stats.(SyncStats); ok && err == nil {
			st.ManifestExportJob = id
			res.Stats = st
		}
		if env.Reporter != nil {
			env.Reporter.Log(slog.LevelInfo, "queued the manifest export that follows the sync", "jobId", id)
		}
	}
	return res, err
}

// sourceRoots opens the plan's sources for execution (S1: every source path an engine reads is
// the catalog's relative path joined to the source's recorded root). A resumed plan checks that a
// root whose catalog lists files is not empty (an unmounted share).
func sourceRoots(plan *syncer.Plan) (map[int64]*os.Root, func(), error) {
	roots := map[int64]*os.Root{}
	closeAll := func() {
		for _, r := range roots {
			_ = r.Close()
		}
	}
	for _, src := range plan.Sources() {
		root, err := os.OpenRoot(src.Path)
		if err != nil {
			closeAll()
			return nil, nil, fmt.Errorf("source %q: open %s: %w (is it mounted?)", src.Name, src.Path, err)
		}
		roots[src.ID] = root
		if plan.Planned() || src.Stats.Files == 0 {
			continue
		}
		f, err := root.Open(".")
		if err != nil {
			closeAll()
			return nil, nil, fmt.Errorf("source %q: read %s: %w (is it mounted?)", src.Name, src.Path, err)
		}
		names, _ := f.Readdirnames(1)
		_ = f.Close()
		if len(names) == 0 {
			closeAll()
			return nil, nil, fmt.Errorf("source %q: %s is empty but its catalog lists %d files; is it mounted? (nothing was changed)",
				src.Name, src.Path, src.Stats.Files)
		}
	}
	return roots, closeAll, nil
}

// absPath is a source file's absolute path: the source's recorded root joined with the catalog's
// relative path (S1).
func absPath(src catalog.Source, rel string) string {
	return path.Join(filepath.ToSlash(src.Path), rel)
}

// headTail reads a source file's head/tail hash through its root with the size and mtime it had
// (§6.2 step 3, §7.3); a file that cannot be read gives "".
func headTail(root *os.Root, rel string) (hash string, size, mtimeNs int64) {
	if root == nil {
		return "", 0, 0
	}
	st, err := filecopy.Lstat(root, rel)
	if err != nil || !st.Regular() {
		return "", 0, 0
	}
	h, err := filecopy.HeadTailHash(root, rel)
	if err != nil {
		return "", 0, 0
	}
	after, err := filecopy.Lstat(root, rel)
	if err != nil || after.Size != st.Size || after.MtimeNs != st.MtimeNs {
		return "", 0, 0
	}
	return h, st.Size, st.MtimeNs
}

// storedHeadTail returns the head/tail hash to record for an item whose version (size, mtime) was
// read back: the one read before the upload when the file had that version then.
func storedHeadTail(d engineDetail, size, mtimeNs int64) string {
	if d.HeadTail != "" && d.HeadTailSize == size && d.HeadTailMtimeNs == mtimeNs {
		return d.HeadTail
	}
	return ""
}

// reappeared reports whether a vanished source path is back: a regular file is there again and
// the catalog lists it.
func (s *Service) reappeared(ctx context.Context, root *os.Root, sourceID int64, rel string) (bool, error) {
	if root == nil {
		return false, nil
	}
	st, err := filecopy.Lstat(root, rel)
	if err != nil || !st.Regular() {
		return false, nil
	}
	return s.inCatalog(ctx, sourceID, rel)
}

// inCatalog reports whether the catalog lists rel as a live file of the source.
func (s *Service) inCatalog(ctx context.Context, sourceID int64, rel string) (bool, error) {
	f, ok, err := s.catalogFile(ctx, sourceID, rel)
	_ = f
	return ok, err
}

// catalogFile returns the live catalog row of a source path.
func (s *Service) catalogFile(ctx context.Context, sourceID int64, rel string) (catalog.File, bool, error) {
	files, err := s.o.Catalog.LiveUnder(ctx, sourceID, []string{rel})
	if err != nil {
		return catalog.File{}, false, err
	}
	for _, f := range files {
		if f.RelPath == rel {
			return f, true, nil
		}
	}
	return catalog.File{}, false, nil
}

// syncStats completes an engine sync's stats from the plan's.
func (r *jobRun) syncStats(ctx context.Context, plan *syncer.Plan) (SyncStats, int, error) {
	base, warnings, _, err := plan.Stats(ctx, r.started)
	if err != nil {
		return SyncStats{}, 0, err
	}
	st := SyncStats{SyncStats: base, Engine: string(r.kind), Snapshots: []SnapshotStat{}, Deferrals: r.job.Deferrals,
		LimitKiBps: r.limitAt(r.s.now()) / 1024}
	return st, warnings + r.warnings, nil
}

// cancelled reports the job's cancellation.
func cancelled(ctx context.Context, err error) bool {
	return ctx.Err() != nil && (err == nil || errors.Is(err, ctx.Err()) || errors.Is(err, context.Canceled))
}

// longSeedDeferrals is the deferral count at which a sync warns that its seed needs more windows
// than two weeks (§9.2).
const longSeedDeferrals = 14

// deferralWarning warns when a sync has been deferred longSeedDeferrals times in a row (and every
// longSeedDeferrals deferrals after that).
func (r *jobRun) deferralWarning(ctx context.Context) {
	n := r.job.Deferrals + 1
	if n%longSeedDeferrals != 0 {
		return
	}
	rate := "an unknown rate"
	if bps := r.rateAt(ctx, r.s.now()); bps > 0 {
		rate = formatRate(bps)
	}
	r.rep.Log(slog.LevelWarn, fmt.Sprintf("the seed needs more windows than two weeks at %s", rate), "deferrals", n)
}

// firstCutWarning warns at the first deferral whose plan holds an item the window's end cut: with
// no known rate its fit cannot be checked, and a second cut fails it (§9.2).
func (r *jobRun) firstCutWarning(rel string) {
	r.rep.Log(slog.LevelWarn, "a file was cut by the transfer window's end; it may not fit a window (a second cut fails it)", "path", rel)
}
