package enginerun

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/engines/rclone"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/syncer"
)

// The retain and release items of an rclone sync (§7.3 step 5), after every content item: the S6
// wait first (a retain waits while a full file of its folder is not backed up), then the
// intents, the retention targets listed (a taken name gets a numbered one, moved alone), one
// server-side rclone move per destination folder with --max-delete, a listing of both sides, and
// the settle rules of §3.3 (never a delete).

// retainWork is one retain or release that moves an object.
type retainWork struct {
	it     jobs.Item
	d      engineDetail
	rec    syncer.Record
	gone   []int64
	reason string
	target string
}

// retainItems runs the retain and release items.
func (x *rcloneRun) retainItems(ctx context.Context, items []jobs.Item) error {
	byFolder := map[string][]retainWork{}
	var folders []string
	for _, it := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		if now := x.s.now(); !x.open(now) {
			return x.deferral(now)
		}
		w, ok, err := x.prepareRetain(ctx, it)
		var ie *itemErr
		switch {
		case errors.As(err, &ie):
			if err := x.failItem(ctx, it, ie.msg); err != nil {
				return err
			}
			continue
		case err != nil:
			return err
		case !ok:
			continue
		}
		folder := x.sources[w.d.SourceID].DestFolder
		if _, seen := byFolder[folder]; !seen {
			folders = append(folders, folder)
		}
		byFolder[folder] = append(byFolder[folder], w)
	}
	for _, folder := range folders {
		if err := x.retainBatch(ctx, folder, byFolder[folder]); err != nil {
			return err
		}
	}
	return nil
}

// prepareRetain checks one retain or release (phase1.md §4.2, phase2-3.md S6 and S15) and
// finishes the ones that move nothing. ok reports an object to move.
func (x *rcloneRun) prepareRetain(ctx context.Context, it jobs.Item) (retainWork, bool, error) {
	d, err := parseItem(it)
	if err != nil {
		return retainWork{}, false, itemErrorf("%v", err)
	}
	w := retainWork{it: it, d: d}
	if x.roots[d.SourceID] == nil {
		x.warnings++
		return w, false, x.finish(ctx, it.ID, jobs.ItemSkipped, 0, fmt.Sprintf("source %d is no longer synced to this destination", d.SourceID))
	}
	v, err := x.s.files.Get(ctx, d.RecordID)
	if errors.Is(err, syncer.ErrNotFound) {
		return w, false, x.done(ctx, it, &w.d, 0, "record removed")
	}
	if err != nil {
		return w, false, err
	}
	if v.State == syncer.StateRetained && v.SourceID == d.SourceID && v.SourceRelPath == d.Source {
		return w, false, x.done(ctx, it, &w.d, v.Size, "already done")
	}
	if !v.State.Live() || v.SourceID != d.SourceID || v.SourceRelPath != d.Source {
		return w, false, x.finish(ctx, it.ID, jobs.ItemSkipped, 0, "the record changed since planning")
	}
	w.rec = v
	if err := listableName(v.RelPath); err != nil {
		return w, false, err
	}
	release := d.Reason == syncer.ReasonReleased
	w.reason = syncer.ReasonDeleted
	if release {
		w.reason = syncer.ReasonReleased
		why, err := x.plan.CheckRelease(ctx, v, d.TierRevision)
		if err != nil {
			return w, false, err
		}
		if why != "" {
			return w, false, x.finish(ctx, it.ID, jobs.ItemSkipped, 0, why)
		}
	} else if back, err := x.s.reappeared(ctx, x.roots[d.SourceID], d.SourceID, v.SourceRelPath); err != nil {
		return w, false, err
	} else if back {
		return w, false, x.finish(ctx, it.ID, jobs.ItemSkipped, 0, "it reappeared at the source; not retained")
	}
	deps, err := x.s.files.Dependents(ctx, v.ID)
	if err != nil {
		return w, false, err
	}
	blocking := 0
	for _, dep := range deps {
		if dep.State == syncer.StateLinkRecorded && dep.SourceID == d.SourceID {
			if back, err := x.s.reappeared(ctx, x.roots[d.SourceID], d.SourceID, dep.SourceRelPath); err != nil {
				return w, false, err
			} else if !back {
				w.gone = append(w.gone, dep.ID)
				continue
			}
		}
		blocking++
	}
	if blocking > 0 {
		return w, false, itemErrorf("%d hardlinked names still depend on the content of %s; it is not retained", blocking, v.RelPath)
	}
	if v.State == syncer.StateLinkRecorded || v.State == syncer.StateMissing {
		// No object of its own (a recorded-only link, or verify found it gone): the record goes.
		if err := x.write(ctx, func(tx *sql.Tx) error { return x.deleteRecords(ctx, tx, append(w.gone, v.ID)) }); err != nil {
			return w, false, err
		}
		return w, false, x.done(ctx, it, &w.d, 0, "record removed")
	}
	if !release {
		// S6: the new version of a vanished name must be backed up before the old one goes.
		name, err := x.plan.NotBackedUp(ctx, d.SourceID, v.SourceRelPath)
		if err != nil {
			return w, false, err
		}
		if name != "" {
			return w, false, itemErrorf("not retained yet: %s in the same folder is not backed up (its copy failed or was held); %s stays until it is",
				name, v.RelPath)
		}
	}
	return w, true, nil
}

// deleteRecords deletes records that hold no object (recorded-only links, missing records).
func (x *rcloneRun) deleteRecords(ctx context.Context, tx *sql.Tx, ids []int64) error {
	for _, id := range ids {
		if err := x.s.files.DeleteRecordTx(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}

// retainBatch moves the objects of one destination folder into this job's retention directory.
func (x *rcloneRun) retainBatch(ctx context.Context, folder string, work []retainWork) error {
	var paths []string
	for _, w := range work {
		paths = append(paths, w.rec.RelPath, x.retDir+"/"+w.rec.RelPath)
	}
	found, err := x.statMany(ctx, paths...)
	if err != nil {
		return err
	}
	var batch []retainWork
	for _, w := range work {
		if _, ok := found[w.rec.RelPath]; !ok {
			// Nothing at the destination (removed by hand): the record goes.
			x.log(slog.LevelWarn, "a vanished file was not at the destination either; its record was removed", "path", w.rec.RelPath)
			if err := x.write(ctx, func(tx *sql.Tx) error { return x.deleteRecords(ctx, tx, append(w.gone, w.rec.ID)) }); err != nil {
				return err
			}
			if err := x.done(ctx, w.it, &w.d, 0, "record removed"); err != nil {
				return err
			}
			continue
		}
		w.target = x.retDir + "/" + w.rec.RelPath
		if _, taken := found[w.target]; taken {
			if err := x.retainAlone(ctx, w); err != nil {
				return err
			}
			continue
		}
		batch = append(batch, w)
	}
	if len(batch) == 0 {
		return nil
	}
	var changed []retainWork
	err = x.write(ctx, func(tx *sql.Tx) error {
		changed = changed[:0]
		for _, w := range batch {
			err := x.s.files.SetIntentTx(ctx, tx, x.d.ID, w.rec.RelPath, w.target, w.reason)
			if errors.Is(err, syncer.ErrRecordChanged) {
				changed = append(changed, w)
				continue
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, w := range changed {
		if err := x.failItem(ctx, w.it, "the record changed since planning"); err != nil {
			return err
		}
		batch = slices.DeleteFunc(batch, func(b retainWork) bool { return b.it.ID == w.it.ID })
	}
	if len(batch) == 0 {
		return nil
	}
	faultinject.Point(PointRcloneAfterIntent)
	rels := make([]string, len(batch))
	for i, w := range batch {
		rels[i] = w.d.Source
	}
	now := x.s.now()
	in := rclone.MoveInput{SrcDir: folder, DstDir: x.retDir + "/" + folder, Files: rels, MaxDelete: len(batch)}
	if left, bounded := x.timeLeft(now); bounded {
		in.MaxDuration = left
	}
	stop := x.stopAtEnd(now, false)
	defer stop.stop()
	in.Interrupt = stop.C
	x.batches++
	faultinject.Point(PointBeforeBatch)
	res, runErr := x.conn.Move(ctx, in)
	faultinject.Point(PointRcloneAfterMove)
	if runErr != nil && ctx.Err() != nil {
		return ctx.Err() // the next job settles the intents (§3.3)
	}
	var targets []string
	for _, w := range batch {
		targets = append(targets, w.rec.RelPath, w.target)
	}
	after, err := x.statMany(ctx, targets...)
	if err != nil {
		return err
	}
	faultinject.Point(PointRcloneAfterStat)
	cut := res.Cutoff || res.Interrupted
	type outcome struct {
		w      retainWork
		status jobs.ItemStatus
		msg    string
	}
	var outcomes []outcome
	err = x.write(ctx, func(tx *sql.Tx) error {
		outcomes = outcomes[:0]
		for _, w := range batch {
			st, msg, err := x.settleRetain(ctx, tx, w, after, res, now)
			if err != nil {
				return err
			}
			if cut && st == jobs.ItemFailed {
				st = jobs.ItemPending
			}
			outcomes = append(outcomes, outcome{w: w, status: st, msg: msg})
		}
		return nil
	})
	if err != nil {
		return err
	}
	x.touched[folder] = true
	for _, o := range outcomes {
		switch o.status {
		case jobs.ItemDone:
			if err := x.done(ctx, o.w.it, &o.w.d, o.w.rec.Size, ""); err != nil {
				return err
			}
		case jobs.ItemFailed:
			if err := x.failItem(ctx, o.w.it, o.msg); err != nil {
				return err
			}
		}
	}
	if runErr != nil {
		return runErr
	}
	if cut {
		x.log(slog.LevelInfo, "the transfer window closed: the remaining retains stay pending")
		return x.deferral(x.s.now())
	}
	return nil
}

// settleRetain decides one moved retain by the listing (§3.3): only the retention path holds it:
// retained; only the live path: nothing moved; both (a server-side move that copied and did not
// delete): the copy in retention is recorded and the live object stays for a later retain (never
// a delete).
func (x *rcloneRun) settleRetain(ctx context.Context, tx *sql.Tx, w retainWork, after map[string]rclone.Object, res rclone.TransferResult,
	now time.Time) (jobs.ItemStatus, string, error) {
	_, live := after[w.rec.RelPath]
	ret, inRet := after[w.target]
	switch {
	case inRet && !live:
		for _, id := range w.gone {
			if err := x.s.files.DeleteRecordTx(ctx, tx, id); err != nil {
				return "", "", err
			}
		}
		err := x.s.files.RetainTx(ctx, tx, w.rec.ID, syncer.Retain{RetainedPath: w.target, Reason: w.reason, JobID: x.job.ID,
			RetainedAt: now, ExpiresAt: x.expiry(now)})
		if errors.Is(err, syncer.ErrRecordChanged) {
			return jobs.ItemFailed, err.Error(), x.recordRetainedCopy(ctx, tx, w, ret, now)
		}
		return jobs.ItemDone, "", err
	case inRet && live:
		if err := x.recordRetainedCopy(ctx, tx, w, ret, now); err != nil {
			return "", "", err
		}
		return jobs.ItemFailed, "moved only partly (the object is at both paths); the next sync retains the live one", nil
	case live:
		if err := x.s.files.ClearIntentTx(ctx, tx, w.rec.ID, w.target); err != nil {
			return "", "", err
		}
		msg := res.ObjectErrors[w.d.Source]
		if msg == "" {
			msg = "not moved into retention"
		}
		return jobs.ItemFailed, msg, nil
	default:
		if err := x.s.files.ClearIntentTx(ctx, tx, w.rec.ID, w.target); err != nil {
			return "", "", err
		}
		return jobs.ItemFailed, "the object vanished during the move", nil
	}
}

// recordRetainedCopy records an object in retention beside a record that stays live, and clears
// the record's intent.
func (x *rcloneRun) recordRetainedCopy(ctx context.Context, tx *sql.Tx, w retainWork, obj rclone.Object, now time.Time) error {
	hash := ""
	if obj.Size == w.rec.Size {
		hash = w.rec.Hash
	}
	if _, err := x.s.files.InsertRetainedTx(ctx, tx, syncer.RetainedVersion{DestinationID: x.d.ID, SourceID: w.rec.SourceID,
		RelPath: w.rec.RelPath, SourceRelPath: w.rec.SourceRelPath, Size: obj.Size, MtimeNs: obj.MtimeNs(), Hash: hash, HeadTail: w.rec.HeadTail,
		RetainedPath: w.target, Reason: w.reason, JobID: x.job.ID, RetainedAt: now, ExpiresAt: x.expiry(now)}); err != nil {
		return err
	}
	return x.s.files.ClearIntentTx(ctx, tx, w.rec.ID, w.target)
}

// retainAlone moves one object whose retention name is taken to a numbered name (moveto).
func (x *rcloneRun) retainAlone(ctx context.Context, w retainWork) error {
	target, err := x.freeRetention(ctx, w.rec.RelPath)
	var ie *itemErr
	if errors.As(err, &ie) {
		return x.failItem(ctx, w.it, ie.msg)
	}
	if err != nil {
		return err
	}
	w.target = target
	if err := x.write(ctx, func(tx *sql.Tx) error {
		return x.s.files.SetIntentTx(ctx, tx, x.d.ID, w.rec.RelPath, w.target, w.reason)
	}); err != nil {
		return err
	}
	faultinject.Point(PointRcloneAfterIntent)
	res, err := x.conn.MoveTo(ctx, w.rec.RelPath, w.target)
	faultinject.Point(PointRcloneAfterMove)
	if errors.Is(err, rclone.ErrFence) {
		// Nothing ran (a name rclone's command line cannot carry): the intent goes, the item fails.
		if cerr := x.write(ctx, func(tx *sql.Tx) error { return x.s.files.ClearIntentTx(ctx, tx, w.rec.ID, w.target) }); cerr != nil {
			return cerr
		}
		return x.failItem(ctx, w.it, unsupportedName(err))
	}
	if err != nil {
		return err
	}
	after, err := x.statMany(ctx, w.rec.RelPath, w.target)
	if err != nil {
		return err
	}
	faultinject.Point(PointRcloneAfterStat)
	now := x.s.now()
	var st jobs.ItemStatus
	var msg string
	if err := x.write(ctx, func(tx *sql.Tx) error {
		var err error
		st, msg, err = x.settleRetain(ctx, tx, w, after, res, now)
		return err
	}); err != nil {
		return err
	}
	if st == jobs.ItemDone {
		return x.done(ctx, w.it, &w.d, w.rec.Size, "")
	}
	return x.failItem(ctx, w.it, msg)
}

// retentionFile reports whether rel is a file inside a job retention directory (the fence of
// every delete, S23).
func retentionFile(rel string) bool {
	return rclone.CheckRetentionFile(rel) == nil && filecopy.RetentionRoot != ""
}
