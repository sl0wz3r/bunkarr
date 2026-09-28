package enginerun

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/engines/rclone"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/syncer"
)

// The copy, update and adopt items of an rclone sync (§7.3 step 3), in batches of at most
// settings.rclone.batchFiles files and batchBytes bytes per source:
//
//   - before the batch: the retention intents of the records the batch replaces (the backup-dir
//     path, reason replaced, or damaged for a missing record), each file's head/tail hash read
//     through the source's root, and a listing of the batch's backup-dir paths: an item whose path
//     there is taken (a resumed job) runs alone with a numbered retention name;
//   - rclone copy with --backup-dir (S23), --max-delete = the batch's items, and --max-duration
//     with --cutoff-mode soft inside a transfer window (§9.2);
//   - after the batch, whatever the exit: a listing of the batch's paths and of its backup-dir
//     paths, then one transaction decides every item: done when the object has the catalog's size
//     and mtime (adopted when nothing was copied or moved); an object in the backup dir is
//     recorded retained (replaced, displaced or damaged); a live record whose old version moved
//     while no matching object arrived turns missing; items without a matching object fail, or
//     stay pending after a window cutoff.

// copyWork is one copy item of a batch.
type copyWork struct {
	it jobs.Item
	d  engineDetail
	// rel is the file's path under the destination folder (the catalog path).
	rel string
	// rec is the item's live record (an update, a repair), if any.
	rec    syncer.Record
	hasRec bool
}

// ownsObject reports an item whose live record has an object of its own at the item's path
// (present, linked or missing; a recorded-only link has none): an object there is that record's
// version, anything else at the path is unmanaged.
func (w copyWork) ownsObject() bool { return w.hasRec && w.rec.State != syncer.StateLinkRecorded }

// backupPath is where copy --backup-dir moves the object at rel of a destination folder.
func (x *rcloneRun) backupPath(folder, rel string) string {
	return x.retDir + "/" + folder + "/" + rel
}

// copyItems runs the copy, update and adopt items, source by source, in batches.
func (x *rcloneRun) copyItems(ctx context.Context, items []jobs.Item) error {
	bySource := map[int64][]jobs.Item{}
	var order []int64
	for _, it := range items {
		d, err := parseItem(it)
		if err != nil {
			if err := x.failItem(ctx, it, err.Error()); err != nil {
				return err
			}
			continue
		}
		if _, ok := bySource[d.SourceID]; !ok {
			order = append(order, d.SourceID)
		}
		bySource[d.SourceID] = append(bySource[d.SourceID], it)
	}
	settings := x.d.Settings.Rclone
	maxFiles, maxBytes := 1000, int64(64<<30)
	if settings != nil {
		maxFiles, maxBytes = settings.BatchFiles, settings.BatchBytes
	}
	for _, id := range order {
		src, ok := x.sources[id]
		if !ok {
			for _, it := range bySource[id] {
				x.warnings++
				if err := x.finish(ctx, it.ID, jobs.ItemSkipped, 0, fmt.Sprintf("source %d is no longer synced to this destination", id)); err != nil {
					return err
				}
			}
			continue
		}
		queue := bySource[id]
		for len(queue) > 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
			batch, rest, overrun, err := x.cutBatch(ctx, src, queue, maxFiles, maxBytes)
			if err != nil {
				return err
			}
			queue = rest
			if len(batch) == 0 {
				continue
			}
			if err := x.copyBatch(ctx, src, batch, overrun); err != nil {
				return err
			}
		}
	}
	if x.windowWait {
		return x.deferral(x.s.now())
	}
	return nil
}

// cutBatch takes the next batch off queue (§7.3, §9.2): items already recorded done are finished,
// a file that cannot fit a whole window fails (or, with allowOverrun, runs alone from a window's
// opening), and no file starts whose expected time is longer than the time left in the window
// plus the grace: it waits for the next window while the later files that fit run, and runs first
// in the next attempt, whatever plan that is (one whose turn always comes late starts with
// allowOverrun, else fails, waitNotFitting).
func (x *rcloneRun) cutBatch(ctx context.Context, src catalog.Source, queue []jobs.Item, maxFiles int, maxBytes int64) (
	batch []copyWork, rest []jobs.Item, overrun bool, err error) {
	now := x.s.now()
	if !x.open(now) {
		return nil, nil, false, x.deferral(now)
	}
	rate := x.rateAt(ctx, now)
	left, bounded := x.timeLeft(now)
	var bytes int64
	for i, it := range queue {
		d, err := parseItem(it)
		if err != nil {
			if err := x.failItem(ctx, it, err.Error()); err != nil {
				return nil, nil, false, err
			}
			continue
		}
		w := copyWork{it: it, d: d, rel: d.Source}
		if err := listableName(w.rel); err != nil {
			if err := x.failItem(ctx, it, err.Error()); err != nil {
				return nil, nil, false, err
			}
			continue
		}
		if w.rec, w.hasRec, err = x.itemRecord(ctx, it, d); err != nil {
			return nil, nil, false, err
		}
		if x.alreadyDone(w) {
			if err := x.done(ctx, it, &w.d, it.Bytes, "already done"); err != nil {
				return nil, nil, false, err
			}
			continue
		}
		if msg := x.oversize(it.Bytes, rate); msg != "" {
			if !x.win.AllowOverrun() {
				x.warn("a file is larger than the transfer window", "path", it.RelPath, "rate", formatRate(rate))
				if err := x.finish(ctx, it.ID, jobs.ItemFailed, 0, msg); err != nil {
					return nil, nil, false, err
				}
				continue
			}
			if len(batch) > 0 {
				return batch, queue[i:], false, nil
			}
			if !x.overrunMayStart(x.started, now, x.waitsOf(w.d)) {
				// It starts alone at a window's opening: the next attempt runs it first.
				if err := x.waitOverrun(ctx, it, w.d, now); err != nil {
					return nil, nil, false, err
				}
				continue
			}
			x.dropWait(w.d)
			return []copyWork{w}, queue[i+1:], true, nil
		}
		if bounded && rate > 0 && !x.fitsLeft(expected(it.Bytes, rate), left) {
			if len(batch) > 0 {
				return batch, queue[i:], false, nil
			}
			overrun, err := x.waitNotFitting(ctx, it, w.d, x.started, now, rate)
			if err != nil {
				return nil, nil, false, err
			}
			if overrun {
				x.dropWait(w.d)
				return []copyWork{w}, queue[i+1:], true, nil
			}
			continue
		}
		if len(batch) > 0 && (len(batch) >= maxFiles || bytes+it.Bytes > maxBytes) {
			return batch, queue[i:], false, nil
		}
		x.dropWait(w.d)
		batch = append(batch, w)
		bytes += it.Bytes
	}
	return batch, nil, false, nil
}

// itemRecord returns a copy item's live record: its planned record (an update, a repair) or the
// live record at its path.
func (x *rcloneRun) itemRecord(ctx context.Context, it jobs.Item, d engineDetail) (syncer.Record, bool, error) {
	if d.RecordID != 0 {
		rec, err := x.s.files.Get(ctx, d.RecordID)
		if err == nil && rec.State.Live() && rec.RelPath == it.RelPath {
			return rec, true, nil
		}
		if err != nil && !errors.Is(err, syncer.ErrNotFound) {
			return syncer.Record{}, false, err
		}
	}
	return x.s.files.LiveAt(ctx, x.d.ID, it.RelPath)
}

// alreadyDone reports an item whose record already holds the planned version (an earlier attempt
// recorded it and stopped before the item was finished).
func (x *rcloneRun) alreadyDone(w copyWork) bool {
	return w.hasRec && w.rec.State == syncer.StatePresent && w.rec.SourceID == w.d.SourceID && w.rec.Size == w.d.Size &&
		w.rec.MtimeNs == w.d.MtimeNs && w.rec.JobID == x.job.ID && w.rec.RetainedPath == ""
}

// copyBatch runs one batch.
func (x *rcloneRun) copyBatch(ctx context.Context, src catalog.Source, batch []copyWork, overrun bool) error {
	x.batches++
	if x.batches > 1 && x.batches%recheckBatches == 1 {
		if _, err := x.conn.CheckMarker(ctx); err != nil {
			return err
		}
	}
	k := x.batches
	x.report(func(p *jobs.Progress) { p.Batch, p.Batches = k, max(p.Batches, k) })
	root := x.roots[src.ID]
	// Items whose backup-dir path is taken (a resumed job) run alone with a numbered name.
	rels := make([]string, len(batch))
	for i, w := range batch {
		rels[i] = w.rel
	}
	taken, err := x.conn.StatMany(ctx, x.retDir+"/"+src.DestFolder, rels)
	if err != nil {
		return err
	}
	// A repair (the record is missing) whose object is still there runs alone too: copy would
	// skip an object that matches in size and modification time although verify found its
	// content damaged, so the object moves into retention (damaged) before the upload.
	var repairs []string
	for _, w := range batch {
		if w.hasRec && w.rec.State == syncer.StateMissing {
			repairs = append(repairs, w.rel)
		}
	}
	damaged := map[string]rclone.Object{}
	if len(repairs) > 0 {
		if damaged, err = x.conn.StatMany(ctx, src.DestFolder, repairs); err != nil {
			return err
		}
	}
	var run []copyWork
	for _, w := range batch {
		_, isTaken := taken[w.rel]
		_, isDamaged := damaged[w.rel]
		if isTaken || isDamaged {
			if err := x.alone(ctx, src, w); err != nil {
				return err
			}
			continue
		}
		run = append(run, w)
	}
	if len(run) == 0 {
		return nil
	}
	// Intents on the records whose own object the batch replaces; head/tail hashes of the files.
	var changed []copyWork
	err = x.write(ctx, func(tx *sql.Tx) error {
		changed = changed[:0]
		for _, w := range run {
			if !w.ownsObject() {
				continue
			}
			reason := syncer.ReasonReplaced
			if w.rec.State == syncer.StateMissing {
				reason = syncer.ReasonDamaged
			}
			err := x.s.files.SetIntentTx(ctx, tx, x.d.ID, w.it.RelPath, x.backupPath(src.DestFolder, w.rel), reason)
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
		run = slices.DeleteFunc(run, func(r copyWork) bool { return r.it.ID == w.it.ID })
	}
	if len(run) == 0 {
		return nil
	}
	faultinject.Point(PointRcloneAfterIntent)
	var files []string
	for i := range run {
		w := &run[i]
		w.d.HeadTail, w.d.HeadTailSize, w.d.HeadTailMtimeNs = headTail(root, w.rel)
		w.d.Batch = k
		files = append(files, w.rel)
	}
	now := x.s.now()
	in := rclone.CopyInput{SourceRoot: src.Path, DestFolder: src.DestFolder, Files: files, RetentionDir: x.retDir, MaxDelete: len(run),
		OnStats: x.onStats}
	if left, bounded := x.timeLeft(now); bounded && !overrun {
		in.MaxDuration = left
	}
	stop := x.stopAtEnd(now, overrun)
	defer stop.stop()
	in.Interrupt = stop.C
	x.started = true
	faultinject.Point(PointBeforeBatch)
	res, runErr := x.conn.Copy(ctx, in)
	faultinject.Point(PointAfterBatchExit)
	if cancelled(ctx, runErr) && ctx.Err() != nil {
		// The job was cancelled: the after-listing still records what moved (a cancelled job
		// is never resumed), so it runs without the job's cancellation; its errors are secondary.
		_ = x.settleBatch(context.WithoutCancel(ctx), src, run, res, true)
		return ctx.Err()
	}
	up := uploadedBytes(res.Stats)
	x.uploaded += up
	x.s.noteThroughput(ctx, x.d.ID, up, x.s.now().Sub(now))
	cut := res.Cutoff || res.Interrupted
	if err := x.settleBatch(ctx, src, run, res, cut); err != nil {
		return err
	}
	if runErr != nil {
		return runErr
	}
	if cut {
		if err := x.noteCuts(ctx, run, res); err != nil {
			return err
		}
		x.log(slog.LevelInfo, "the transfer window closed: the rest of the batch stays pending")
		return x.deferral(x.s.now())
	}
	return nil
}

// onStats turns a stats line into progress (§10.4): the bytes of the finished batches plus the
// current batch's upload (its rclone counts from zero, server-side copies included).
func (x *rcloneRun) onStats(st rclone.Stats) {
	x.report(func(p *jobs.Progress) {
		p.BytesDone = x.uploaded + uploadedBytes(st)
		if f := st.CurrentFile(); f != "" {
			p.CurrentFile = f
		}
	})
}

// settleBatch lists the batch's paths and backup-dir paths and records every item (§7.3). cut
// keeps the items without a matching object pending (a window cutoff or a cancellation).
func (x *rcloneRun) settleBatch(ctx context.Context, src catalog.Source, run []copyWork, res rclone.TransferResult, cut bool) error {
	rels := make([]string, len(run))
	for i, w := range run {
		rels[i] = w.rel
	}
	live, err := x.conn.StatMany(context.WithoutCancel(ctx), src.DestFolder, rels)
	if err != nil {
		return err
	}
	back, err := x.conn.StatMany(context.WithoutCancel(ctx), x.retDir+"/"+src.DestFolder, rels)
	if err != nil {
		return err
	}
	faultinject.Point(PointRcloneAfterStat)
	copied := map[string]bool{}
	for _, ev := range res.Events {
		if ev.Kind == rclone.EventCopiedNew || ev.Kind == rclone.EventCopiedReplaced {
			copied[ev.Object] = true
		}
	}
	type outcome struct {
		w       copyWork
		status  jobs.ItemStatus
		msg     string
		adopted bool
	}
	var outcomes []outcome
	now := x.s.now()
	err = x.write(ctx, func(tx *sql.Tx) error {
		outcomes = outcomes[:0]
		for _, w := range run {
			obj, ok := live[w.rel]
			old, moved := back[w.rel]
			if moved {
				if err := x.recordBackup(ctx, tx, src, w, old, now); err != nil {
					return err
				}
			}
			if ok && obj.Size == w.d.Size && filecopy.MtimeMatch(obj.MtimeNs(), w.d.MtimeNs, x.caps.MtimeGranularityNs, 0) {
				recID := int64(0)
				if w.hasRec {
					recID = w.rec.ID
				}
				if _, err := x.s.files.RecordContentTx(ctx, tx, syncer.ContentDone{DestinationID: x.d.ID, SourceID: w.d.SourceID,
					RecordID: recID, RelPath: w.it.RelPath, SourceRelPath: w.d.Source, Size: w.d.Size, MtimeNs: w.d.MtimeNs,
					HeadTail: storedHeadTail(w.d, w.d.Size, w.d.MtimeNs), State: syncer.StatePresent, JobID: x.job.ID, CopiedAt: now}); err != nil {
					if errors.Is(err, syncer.ErrRecordChanged) {
						outcomes = append(outcomes, outcome{w: w, status: jobs.ItemFailed, msg: err.Error()})
						continue
					}
					return err
				}
				outcomes = append(outcomes, outcome{w: w, status: jobs.ItemDone, adopted: !copied[w.rel] && !moved})
				continue
			}
			if w.ownsObject() {
				if moved {
					// The old version moved into retention and no matching object arrived: the live
					// name holds nothing recorded (S6: the replaced-version hold keeps the old one).
					if err := x.s.files.MarkMissingTx(ctx, tx, w.rec.ID); err != nil {
						return err
					}
				}
				if err := x.s.files.ClearIntentTx(ctx, tx, w.rec.ID, x.backupPath(src.DestFolder, w.rel)); err != nil {
					return err
				}
			}
			if cut {
				outcomes = append(outcomes, outcome{w: w, status: jobs.ItemPending})
				continue
			}
			msg := res.ObjectErrors[w.rel]
			if msg == "" {
				msg = "not transferred"
				if ok {
					msg = "not transferred (the object at the destination is not the scanned file; it changed during the sync?)"
				}
			}
			outcomes = append(outcomes, outcome{w: w, status: jobs.ItemFailed, msg: msg})
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, o := range outcomes {
		switch o.status {
		case jobs.ItemDone:
			outcomeName := ""
			if o.adopted {
				outcomeName = "adopted"
				x.plan.Reclassify(o.w.it.Action, jobs.ActionAdopt)
			}
			if err := x.done(ctx, o.w.it, &o.w.d, o.w.it.Bytes, outcomeName); err != nil {
				return err
			}
			x.report(func(p *jobs.Progress) { p.FilesDone++ })
		case jobs.ItemFailed:
			if err := x.failItem(ctx, o.w.it, o.msg); err != nil {
				return err
			}
		}
	}
	return nil
}

// recordBackup records the object copy --backup-dir moved out of an item's path: an update's
// old version (replaced), a missing record's damaged object (damaged), or an object Bunkarr had
// no record for (displaced).
func (x *rcloneRun) recordBackup(ctx context.Context, tx *sql.Tx, src catalog.Source, w copyWork, obj rclone.Object, now time.Time) error {
	v := syncer.RetainedVersion{DestinationID: x.d.ID, SourceID: w.d.SourceID, RelPath: w.it.RelPath, SourceRelPath: w.d.Source,
		Size: obj.Size, MtimeNs: obj.MtimeNs(), RetainedPath: x.backupPath(src.DestFolder, w.rel), Reason: syncer.ReasonDisplaced,
		JobID: x.job.ID, RetainedAt: now, ExpiresAt: x.expiry(now)}
	if w.ownsObject() {
		v.Reason, v.SourceRelPath, v.HeadTail = syncer.ReasonReplaced, w.rec.SourceRelPath, w.rec.HeadTail
		if w.rec.State == syncer.StateMissing {
			v.Reason = syncer.ReasonDamaged
		}
		if obj.Size == w.rec.Size {
			v.Hash = w.rec.Hash
		}
	} else {
		x.plan.AddDisplaced(1)
	}
	_, err := x.s.files.InsertRetainedTx(ctx, tx, v)
	return err
}

// noteCuts counts the window cuts of the items that were transferring when the window closed; an
// item cut in two windows fails with the oversize warning (the backstop when no rate says a file
// cannot fit, §9.2).
func (x *rcloneRun) noteCuts(ctx context.Context, run []copyWork, res rclone.TransferResult) error {
	transferring := map[string]bool{}
	for _, t := range res.Stats.Transferring {
		transferring[t.Name] = true
	}
	for _, w := range run {
		if !transferring[w.rel] && len(run) > 1 {
			continue
		}
		rec, has, err := x.itemRecord(ctx, w.it, w.d)
		if err != nil {
			return err
		}
		if has && rec.State == syncer.StatePresent && rec.Size == w.d.Size && rec.MtimeNs == w.d.MtimeNs {
			continue
		}
		w.d.WindowCuts++
		if w.d.WindowCuts >= 2 {
			x.warn("an item was cut by the transfer window's end twice", "path", w.it.RelPath)
			if err := x.finish(ctx, w.it.ID, jobs.ItemFailed, 0, cutTwiceMessage); err != nil {
				return err
			}
			continue
		}
		x.firstCutWarning(w.it.RelPath)
		if err := x.setDetail(ctx, w.it.ID, w.d); err != nil {
			return err
		}
	}
	return nil
}

// alone runs an item whose backup-dir path is taken (§7.3): what is there is recorded (it came
// from this job's earlier attempt), an object already matching the plan is recorded done, and
// otherwise the live object moves to a numbered retention name and the file is uploaded with
// copyto.
func (x *rcloneRun) alone(ctx context.Context, src catalog.Source, w copyWork) error {
	bp := x.backupPath(src.DestFolder, w.rel)
	found, err := x.statMany(ctx, bp, w.it.RelPath)
	if err != nil {
		return err
	}
	now := x.s.now()
	if old, ok := found[bp]; ok {
		if err := x.write(ctx, func(tx *sql.Tx) error { return x.recordBackup(ctx, tx, src, w, old, now) }); err != nil {
			return err
		}
	}
	obj, ok := found[w.it.RelPath]
	if ok && obj.Size == w.d.Size && filecopy.MtimeMatch(obj.MtimeNs(), w.d.MtimeNs, x.caps.MtimeGranularityNs, 0) {
		// A repair's object matches in size and mtime although verify found its content damaged:
		// it is the damaged object itself unless this job already moved that one into retention
		// (then the object is this job's upload).
		fresh := true
		if w.hasRec && w.rec.State == syncer.StateMissing {
			if fresh, err = x.retainedByThisJob(ctx, w); err != nil {
				return err
			}
		}
		if fresh {
			return x.recordAlone(ctx, w, now)
		}
	}
	return x.step(ctx, w.it, func(ctx context.Context, it jobs.Item, d *engineDetail) error {
		if ok {
			if w.ownsObject() {
				if err := x.retainOwnObject(ctx, w.rec); err != nil {
					return err
				}
				// The record is replaced: its old version is retained, the live name is empty.
				if err := x.write(ctx, func(tx *sql.Tx) error { return x.s.files.MarkMissingTx(ctx, tx, w.rec.ID) }); err != nil {
					return err
				}
			} else if err := x.displace(ctx, it, d, w.it.RelPath, obj); err != nil {
				return err
			}
		}
		return x.upload(ctx, src, w)
	})
}

// retainedByThisJob reports whether this job already recorded an object moved out of the item's
// path into retention (a repair's damaged object, by an earlier attempt's copy --backup-dir or
// retainOwnObject).
func (x *rcloneRun) retainedByThisJob(ctx context.Context, w copyWork) (bool, error) {
	rows, err := x.s.files.RetainedBySourcePath(ctx, x.d.ID, w.rec.SourceID, w.rec.SourceRelPath)
	if err != nil {
		return false, err
	}
	for _, r := range rows {
		if r.JobID == x.job.ID && r.RelPath == w.it.RelPath {
			return true, nil
		}
	}
	return false, nil
}

// copyAlone uploads one item as its own object (a link that cannot be recorded as one).
func (x *rcloneRun) copyAlone(ctx context.Context, src catalog.Source, it jobs.Item, d engineDetail) error {
	w := copyWork{it: it, d: d, rel: d.Source}
	var err error
	if w.rec, w.hasRec, err = x.itemRecord(ctx, it, d); err != nil {
		return err
	}
	found, err := x.statMany(ctx, it.RelPath)
	if err != nil {
		return err
	}
	if obj, ok := found[it.RelPath]; ok {
		if w.ownsObject() {
			if err := x.retainOwnObject(ctx, w.rec); err != nil {
				return err
			}
		} else if err := x.displace(ctx, it, &w.d, it.RelPath, obj); err != nil {
			return err
		}
	}
	return x.upload(ctx, src, w)
}

// upload copies one file to its free live path (rclone copyto; the caller made sure the path is
// free) and records it after a listing.
func (x *rcloneRun) upload(ctx context.Context, src catalog.Source, w copyWork) error {
	w.d.HeadTail, w.d.HeadTailSize, w.d.HeadTailMtimeNs = headTail(x.roots[src.ID], w.rel)
	local := filepath.Join(src.Path, filepath.FromSlash(w.rel))
	x.started = true
	faultinject.Point(PointBeforeBatch)
	err := x.conn.CopyTo(ctx, local, w.it.RelPath)
	faultinject.Point(PointAfterBatchExit)
	if err != nil {
		var re *rclone.Error
		if errors.As(err, &re) && (re.Class == rclone.ExitObjects || re.Class == rclone.ExitItemNotFound) {
			return itemErrorf("%s", re.Message)
		}
		return err
	}
	found, err := x.statMany(ctx, w.it.RelPath)
	if err != nil {
		return err
	}
	faultinject.Point(PointRcloneAfterStat)
	obj, ok := found[w.it.RelPath]
	if !ok || obj.Size != w.d.Size || !filecopy.MtimeMatch(obj.MtimeNs(), w.d.MtimeNs, x.caps.MtimeGranularityNs, 0) {
		return itemErrorf("not transferred")
	}
	return x.recordAlone(ctx, w, x.s.now())
}

// recordAlone records one uploaded item as present and finishes it.
func (x *rcloneRun) recordAlone(ctx context.Context, w copyWork, now time.Time) error {
	rec, has, err := x.itemRecord(ctx, w.it, w.d)
	if err != nil {
		return err
	}
	recID := int64(0)
	if has {
		recID = rec.ID
	}
	err = x.write(ctx, func(tx *sql.Tx) error {
		_, err := x.s.files.RecordContentTx(ctx, tx, syncer.ContentDone{DestinationID: x.d.ID, SourceID: w.d.SourceID, RecordID: recID,
			RelPath: w.it.RelPath, SourceRelPath: w.d.Source, Size: w.d.Size, MtimeNs: w.d.MtimeNs,
			HeadTail: storedHeadTail(w.d, w.d.Size, w.d.MtimeNs), State: syncer.StatePresent, JobID: x.job.ID, CopiedAt: now})
		return err
	})
	if errors.Is(err, syncer.ErrRecordChanged) {
		return x.failItem(ctx, w.it, err.Error())
	}
	if err != nil {
		return err
	}
	return x.done(ctx, w.it, &w.d, w.it.Bytes, "")
}

// uploadedBytes is what an rclone copy sent to the remote. rclone's bytes also count server-side
// copies, which never cross the network: on S3 an old version moved into --backup-dir is a
// server-side copy (phase4.md §20.4), so serverSideCopyBytes is subtracted. Where the backend can
// rename (SFTP), --backup-dir moves the old version server-side as a checking transfer: rclone
// 1.74 adds it to serverSideMoveBytes but not to bytes (fs/operations move with isTransfer
// false), so subtracting serverSideMoveBytes as well would drop the upload of the new version.
func uploadedBytes(st rclone.Stats) int64 {
	return max(st.Bytes-st.ServerSideCopyBytes, 0)
}
