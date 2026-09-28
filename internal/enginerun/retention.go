package enginerun

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/engines/rclone"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/syncer"
)

// Retention of engine destinations (§6.5, §7.5): the expiry of retained rows with the Phase 1-3
// holds (the irreplaceable flag) and the replaced-version hold (a replaced version is kept while
// the live record of its path is not present: "the new version is not backed up yet"); then on
// restic the forget of snapshots no rule keeps and the prune (restic_retention.go), on rclone the
// fenced batched delete and the daily cleanup of unfinished multipart uploads.

// runRetention runs a retention job of one restic or rclone destination (the global retention job
// is the syncer's: Dispatch sends DestinationID 0 there).
func (s *Service) runRetention(ctx context.Context, kind engines.Kind, job jobs.Job, env jobs.Env) (jobs.Result, error) {
	r, err := s.prepare(ctx, kind, job, env, "retention")
	if err != nil {
		return jobs.Result{}, err
	}
	var res jobs.Result
	if kind == engines.Restic {
		res, err = s.resticRetention(ctx, r)
	} else {
		res, err = s.rcloneRetention(ctx, r)
	}
	return res, r.redactErr(err)
}

// Hold texts of expire items.
const (
	holdIrreplaceable = "held by an irreplaceable flag"
	holdReplaced      = "the new version is not backed up yet"
)

// planExpiry persists the expire items of the destination's retained rows past their expiry
// (§6.5 step 2, §7.5), with the holds, and returns them. objects says the rows name objects
// (rclone: fenced to .bunkarr/retention/<run>/, S23); on restic a row names a version in a
// snapshot and only the row is deleted.
func (r *jobRun) planExpiry(ctx context.Context, objects bool) ([]jobs.Item, error) {
	now := r.s.now()
	all, err := r.s.files.Expired(ctx, r.d.ID, now)
	if err != nil {
		return nil, err
	}
	// Rows of sources that are no longer linked to the destination (or were deleted) are orphans
	// and are never expired (phase1.md S5, as the filecopy runner).
	var expired []syncer.Record
	for _, rec := range all {
		if r.linkedSource(rec.SourceID) {
			expired = append(expired, rec)
		}
	}
	if n := len(all) - len(expired); n > 0 {
		r.log(slog.LevelInfo, "expired retained files of sources no longer linked to this destination are kept", "files", n)
	}
	items := make([]jobs.Item, 0, len(expired))
	for _, rec := range expired {
		d := engineDetail{Detail: syncer.Detail{SourceID: rec.SourceID, Source: rec.SourceRelPath, RecordID: rec.ID, Size: rec.Size,
			RetainedPath: rec.RetainedPath, Reason: rec.Reason, Check: checkExpireRow}}
		it := jobs.Item{FileID: rec.ID, RelPath: rec.RelPath, Action: jobs.ActionExpire, Bytes: rec.Size}
		if objects {
			d.Check = checkExpireObject
			it.RelPath = rec.RetainedPath
		}
		if objects && !retentionFile(rec.RetainedPath) {
			it.Status, it.Error = jobs.ItemSkipped, fmt.Sprintf("%s is not inside a job retention directory; it is never deleted", rec.RetainedPath)
		} else if hold, err := r.expiryHold(ctx, rec); err != nil {
			return nil, err
		} else if hold != "" {
			it.Status, it.Error = jobs.ItemHeld, hold
		}
		it.Detail = d.raw()
		items = append(items, it)
	}
	return items, nil
}

// linkedSource reports whether a retained row's source is linked to the destination.
func (r *jobRun) linkedSource(sourceID int64) bool {
	return sourceID != 0 && slices.Contains(r.d.SourceIDs, sourceID)
}

// expiryHold returns the hold that keeps an expired retained row ("" when none): the irreplaceable
// flag, or the replaced-version hold. It is evaluated when the plan is made and again right
// before the row expires (a resumed job's plan may be hours old).
func (r *jobRun) expiryHold(ctx context.Context, rec syncer.Record) (string, error) {
	if r.s.o.Tiers != nil && !r.coverageLoaded {
		covered, err := r.s.o.Tiers.Coverage(ctx)
		if err != nil {
			return "", err
		}
		r.coverage, r.coverageLoaded = covered, true
	}
	if r.coverage != nil && rec.SourceID != 0 {
		if _, ok := r.coverage(rec.SourceID, rec.SourceRelPath); ok {
			return holdIrreplaceable, nil
		}
	}
	if rec.Reason != syncer.ReasonReplaced {
		return "", nil
	}
	live, ok, err := r.s.files.LiveAt(ctx, r.d.ID, rec.RelPath)
	if err != nil {
		return "", err
	}
	if ok {
		if live.State != syncer.StatePresent {
			return holdReplaced, nil
		}
		return "", nil
	}
	// No live record at the path any more (the file was deleted; a renamed file's replaced
	// versions moved to its new path with its record, syncer.RecordContentTx): once a newer version
	// of the path was recorded (retained now, with its own expiry; an unmanaged object displaced
	// from the path is none) the hold is over; the newest version of a path whose successor never
	// got backed up stays held.
	rows, err := r.s.files.RetainedBySourcePath(ctx, r.d.ID, rec.SourceID, rec.SourceRelPath)
	if err != nil {
		return "", err
	}
	for _, o := range rows {
		if o.ID != rec.ID && o.RelPath == rec.RelPath && o.Reason != syncer.ReasonDisplaced && o.RetainedAt != nil && rec.RetainedAt != nil &&
			o.RetainedAt.After(*rec.RetainedAt) {
			return "", nil
		}
	}
	return holdReplaced, nil
}

// expireCheck re-checks an expire item's row right before it expires (the plan may be hours
// old): still past its expiry, its source still linked, and no hold. It finishes the item and
// returns false when the row stays.
func (r *jobRun) expireCheck(ctx context.Context, it jobs.Item, rec syncer.Record) (bool, error) {
	if rec.ExpiresAt == nil || rec.ExpiresAt.After(r.s.now()) {
		return false, r.finish(ctx, it.ID, jobs.ItemSkipped, 0, "not expired")
	}
	if !r.linkedSource(rec.SourceID) {
		return false, r.finish(ctx, it.ID, jobs.ItemSkipped, 0, "its source is no longer linked to this destination; kept")
	}
	hold, err := r.expiryHold(ctx, rec)
	if err != nil || hold == "" {
		return err == nil, err
	}
	r.log(slog.LevelInfo, "an expired retained file is held", "path", rec.RelPath, "reason", hold)
	return false, r.finish(ctx, it.ID, jobs.ItemHeld, 0, hold)
}

// retentionStats counts a retention job's expire items (the Phase 1 keys).
func (r *jobRun) retentionStats(ctx context.Context) (RetentionStats, int, error) {
	counts, err := r.counts(ctx)
	if err != nil {
		return RetentionStats{}, 0, err
	}
	st := RetentionStats{RetentionStats: syncer.RetentionStats{DryRun: r.job.DryRun}, Engine: string(r.kind)}
	warnings := r.warnings
	for status, c := range counts[jobs.ActionExpire] {
		st.FilesPlanned += c.Files
		switch status {
		case jobs.ItemDone:
			st.FilesExpired += c.Files
			st.BytesExpired += c.Bytes
		case jobs.ItemHeld:
			st.FilesHeld += c.Files
		case jobs.ItemFailed:
			st.FilesFailed += c.Files
			warnings += int(c.Files)
		case jobs.ItemSkipped:
			st.FilesKept += c.Files
		}
	}
	st.DurationMs = r.s.now().Sub(r.started).Milliseconds()
	return st, warnings, nil
}

// expireRows runs the expire items of restic rows: the row is deleted (the version stays in its
// snapshot until the forget of a snapshot no record references).
func (r *jobRun) expireRow(ctx context.Context, it jobs.Item) error {
	d, err := parseItem(it)
	if err != nil {
		return r.failItem(ctx, it, err.Error())
	}
	rec, err := r.s.files.Get(ctx, d.RecordID)
	if errors.Is(err, syncer.ErrNotFound) {
		return r.finish(ctx, it.ID, jobs.ItemDone, 0, "")
	}
	if err != nil {
		return err
	}
	if rec.State != syncer.StateRetained || rec.RetainedPath != d.RetainedPath {
		return r.finish(ctx, it.ID, jobs.ItemSkipped, 0, "the record changed since planning")
	}
	if ok, err := r.expireCheck(ctx, it, rec); err != nil || !ok {
		return err
	}
	if err := r.s.o.DB.Write(context.WithoutCancel(ctx), func(tx *sql.Tx) error {
		return r.s.files.DeleteRecordTx(ctx, tx, rec.ID)
	}); err != nil {
		return err
	}
	return r.finish(ctx, it.ID, jobs.ItemDone, rec.Size, "")
}

// --- rclone ---

// rcloneRetention runs the retention job of an rclone destination (§7.5).
func (s *Service) rcloneRetention(ctx context.Context, r *jobRun) (jobs.Result, error) {
	conn, err := s.o.Rclone.Connect(r.ed, r.sec, r.rt)
	if err != nil {
		return jobs.Result{}, fmt.Errorf("retention: %w", err)
	}
	if _, err := conn.CheckMarker(ctx); err != nil {
		return jobs.Result{}, fmt.Errorf("retention: %w", err)
	}
	x := &rcloneRun{jobRun: r, conn: conn, caps: rclone.Capabilities(r.ed), retDir: filecopy.RetentionDir(r.job.QueuedAt, r.job.ID),
		touched: map[string]bool{}}
	r.log(slog.LevelInfo, "retention started", "destination", r.d.Name, "engine", "rclone", "deletedDays", r.d.Retention.DeletedDays)
	if !r.job.DryRun {
		if err := x.reconcile(ctx); err != nil {
			return jobs.Result{}, fmt.Errorf("retention: %w", err)
		}
	}
	planned, err := r.env.Items.Planned(ctx, r.job.ID)
	if err != nil {
		return jobs.Result{}, err
	}
	if !planned {
		if err := r.env.Items.DeleteItems(ctx, r.job.ID); err != nil {
			return jobs.Result{}, err
		}
		items, err := r.planExpiry(ctx, true)
		if err != nil {
			return jobs.Result{}, err
		}
		if err := r.addItems(ctx, items, true); err != nil {
			return jobs.Result{}, err
		}
	}
	cleanup := false
	if !r.job.DryRun {
		if err := x.expireObjects(ctx); err != nil {
			return jobs.Result{}, err
		}
		if cleanup, err = x.cleanup(ctx); err != nil {
			return jobs.Result{}, err
		}
	}
	st, warnings, err := r.retentionStats(ctx)
	if err != nil {
		return jobs.Result{}, err
	}
	st.Cleanup = cleanup
	return jobs.Result{Stats: st, Warnings: warnings, Summary: retentionSummary(st)}, nil
}

// expireBatch is how many retained objects one rclone delete removes.
const expireBatch = 1000

// expireObjects deletes the expired retained objects (§7.5): each object is listed first and one
// of another size is skipped with a warning (as filecopy.Expire), then rclone delete of the
// batch's paths under .bunkarr/retention with --max-delete, a listing, and the rows of the objects
// gone.
func (x *rcloneRun) expireObjects(ctx context.Context) error {
	items, err := x.pending(ctx)
	if err != nil {
		return err
	}
	var expire []jobs.Item
	for _, it := range items {
		if it.Action == jobs.ActionExpire {
			expire = append(expire, it)
		}
	}
	for start := 0; start < len(expire); start += expireBatch {
		if err := ctx.Err(); err != nil {
			return err
		}
		if now := x.s.now(); !x.open(now) {
			return x.deferral(now)
		}
		if x.batches > 0 && x.batches%recheckBatches == 0 {
			if _, err := x.conn.CheckMarker(ctx); err != nil {
				return err
			}
		}
		x.batches++
		if err := x.expireBatch(ctx, expire[start:min(start+expireBatch, len(expire))]); err != nil {
			return err
		}
	}
	if x.ed.Kind == engines.SFTP && len(expire) > 0 {
		if err := x.conn.Rmdirs(ctx, filecopy.RetentionRoot, true); err != nil {
			x.log(slog.LevelInfo, "could not remove empty retention directories", "error", err.Error())
		}
	}
	return nil
}

type expireWork struct {
	it  jobs.Item
	rec syncer.Record
}

func (x *rcloneRun) expireBatch(ctx context.Context, items []jobs.Item) error {
	var work []expireWork
	var paths []string
	for _, it := range items {
		d, err := parseItem(it)
		if err != nil {
			if err := x.failItem(ctx, it, err.Error()); err != nil {
				return err
			}
			continue
		}
		rec, err := x.s.files.Get(ctx, d.RecordID)
		if errors.Is(err, syncer.ErrNotFound) {
			if err := x.finish(ctx, it.ID, jobs.ItemDone, 0, ""); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if rec.State != syncer.StateRetained || rec.RetainedPath != d.RetainedPath || !retentionFile(rec.RetainedPath) {
			if err := x.finish(ctx, it.ID, jobs.ItemSkipped, 0, "the record changed since planning"); err != nil {
				return err
			}
			continue
		}
		if ok, err := x.expireCheck(ctx, it, rec); err != nil {
			return err
		} else if !ok {
			continue
		}
		if err := listableName(rec.RetainedPath); err != nil {
			if err := x.finish(ctx, it.ID, jobs.ItemSkipped, 0, err.Error()+"; kept"); err != nil {
				return err
			}
			continue
		}
		work = append(work, expireWork{it: it, rec: rec})
		paths = append(paths, rec.RetainedPath)
	}
	if len(work) == 0 {
		return nil
	}
	found, err := x.statMany(ctx, paths...)
	if err != nil {
		return err
	}
	var del []expireWork
	var gone []expireWork
	for _, w := range work {
		obj, ok := found[w.rec.RetainedPath]
		switch {
		case !ok:
			gone = append(gone, w)
		case obj.Size != w.rec.Size:
			x.warn("a retained object is not the recorded one; it is not deleted", "path", w.rec.RetainedPath, "size", obj.Size,
				"recorded", w.rec.Size)
			if err := x.finish(ctx, w.it.ID, jobs.ItemSkipped, 0, "the object's size differs from the recorded one"); err != nil {
				return err
			}
		default:
			del = append(del, w)
		}
	}
	if len(del) > 0 {
		var files []string
		for _, w := range del {
			files = append(files, w.rec.RetainedPath)
		}
		res, runErr := x.conn.Delete(ctx, files, len(files))
		if runErr != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		after, err := x.statMany(ctx, files...)
		if err != nil {
			return err
		}
		for _, w := range del {
			if _, still := after[w.rec.RetainedPath]; still {
				msg := res.ObjectErrors[w.rec.RetainedPath]
				if msg == "" {
					msg = "not deleted"
				}
				if err := x.failItem(ctx, w.it, msg); err != nil {
					return err
				}
				continue
			}
			gone = append(gone, w)
		}
		if err := x.forgetGone(ctx, gone); err != nil {
			return err
		}
		// A delete that stopped (exit 7: --max-delete reached) fails the job after the objects
		// it removed are recorded.
		return runErr
	}
	return x.forgetGone(ctx, gone)
}

// forgetGone deletes the rows of expired objects that are gone and finishes their items.
func (x *rcloneRun) forgetGone(ctx context.Context, gone []expireWork) error {
	if len(gone) == 0 {
		return nil
	}
	if err := x.write(ctx, func(tx *sql.Tx) error {
		for _, w := range gone {
			if err := x.s.files.DeleteRecordTx(ctx, tx, w.rec.ID); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	for _, w := range gone {
		if err := x.finish(ctx, w.it.ID, jobs.ItemDone, w.rec.Size, ""); err != nil {
			return err
		}
	}
	return nil
}

// cleanupEvery is how often the unfinished multipart uploads are removed (§7.5).
const cleanupEvery = 24 * time.Hour

// cleanup removes unfinished multipart uploads under the destination's own bucket and prefix at
// most once a day, on S3 and B2 (§7.5): rclone backend cleanup with max-age the larger of 7 days
// and twice the longest expected single-file upload, so an upload in progress is never aborted.
func (x *rcloneRun) cleanup(ctx context.Context) (bool, error) {
	if x.ed.Kind != engines.S3 && x.ed.Kind != engines.B2 {
		return false, nil
	}
	st, err := x.s.State(ctx, x.d.ID)
	if err != nil {
		return false, err
	}
	now := x.s.now()
	if st.LastCleanupAt != nil && now.Sub(*st.LastCleanupAt) < cleanupEvery {
		return false, nil
	}
	age, err := x.cleanupAge(ctx, st)
	if err != nil {
		return false, err
	}
	if err := x.conn.BackendCleanup(ctx, age); err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		x.warn("could not remove unfinished uploads", "error", err.Error())
		return false, nil
	}
	if err := x.s.updateState(ctx, x.d.ID, func(st *EngineState) { st.LastCleanupAt = &now }); err != nil {
		return false, err
	}
	x.log(slog.LevelInfo, "removed unfinished uploads older than the cleanup age", "maxAge", age.String())
	return true, nil
}

// cleanupAge is backend cleanup's max-age (§7.5): the larger of 7 days and twice the time the
// largest file of the destination's sources takes at the lowest rate in force (the lowest upload
// limit of the base and the timetable, or the measured throughput when lower).
func (x *rcloneRun) cleanupAge(ctx context.Context, st EngineState) (time.Duration, error) {
	rate := int64(0)
	consider := func(kib int64) {
		if b := kib * 1024; b > 0 && (rate == 0 || b < rate) {
			rate = b
		}
	}
	consider(x.d.Bandwidth.UploadKiBps)
	for _, e := range x.d.Bandwidth.Timetable {
		consider(e.UploadKiBps)
	}
	if st.ThroughputBps != nil && *st.ThroughputBps > 0 {
		if tp := int64(*st.ThroughputBps); rate == 0 || tp < rate {
			rate = tp
		}
	}
	age := rclone.MinCleanupAge
	if rate <= 0 {
		return age, nil
	}
	var largest int64
	for _, id := range x.d.SourceIDs {
		err := x.s.o.Catalog.Live(ctx, id, func(f catalog.File) error {
			largest = max(largest, f.Size)
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	if twice := 2 * expected(largest, rate); twice > age {
		age = twice.Round(time.Hour) + time.Hour
	}
	return age, nil
}

// retentionSummary is the one-sentence summary of an engine retention job.
func retentionSummary(st RetentionStats) string {
	s := fmt.Sprintf("Expired %d files (%s)", st.FilesExpired, formatBytes(st.BytesExpired))
	if st.DryRun {
		s = fmt.Sprintf("Would expire %d files", st.FilesPlanned-st.FilesHeld-st.FilesKept)
	}
	if st.FilesHeld > 0 {
		s += fmt.Sprintf("; %d held", st.FilesHeld)
	}
	switch {
	case st.Engine == string(engines.Restic) && st.DryRun:
		s += fmt.Sprintf("; would forget %d snapshots", st.SnapshotsForgotten)
	case st.Engine == string(engines.Restic):
		s += fmt.Sprintf("; forgot %d snapshots, kept %d", st.SnapshotsForgotten, st.SnapshotsKept)
	}
	if st.Pruned {
		s += "; pruned"
	}
	if st.Cleanup {
		s += "; removed unfinished uploads"
	}
	if st.FilesFailed > 0 {
		s += fmt.Sprintf("; %d failed", st.FilesFailed)
	}
	return s
}
