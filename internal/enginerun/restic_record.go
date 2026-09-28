package enginerun

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/restic"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/syncer"
)

// The read-back and the reference rules of a restic batch (§6.2 step 5, §6.3, D31, S7): a batch
// counts only through what its snapshot holds. After restic's exit 0 or 3:
//   - the snapshot must exist with the batch's tags (restic snapshots --no-lock <id>);
//   - its content (restic ls --json --no-lock) is streamed into a per-job temporary table;
//   - one transaction records the snapshot (it becomes the source's base), every content item it
//     holds with the plan's size and mtime (an update's old version first gets a replaced row
//     referencing the old record's snapshot or the previous base), fails the others ("could not be
//     read: ..." from restic's error line, else "not in the snapshot ..."), and applies the
//     reference rule to every live record of the source: held -> engine_ref NULL, else
//     engine_ref = COALESCE(engine_ref, previous base). A reference is never moved.

// defaultNotHeld is the error of an item the snapshot does not hold without an error line.
const defaultNotHeld = "not in the snapshot (vanished, excluded or changed during the backup)"

// mtimeHolds compares a snapshot's mtime with a record's: to the nanosecond when both have it,
// else to the second (syncer's read-back table uses the same rule).
func mtimeHolds(snap, rec int64) bool {
	if snap == rec {
		return true
	}
	if snap%1_000_000_000 == 0 || rec%1_000_000_000 == 0 {
		return snap/1_000_000_000 == rec/1_000_000_000
	}
	return false
}

// jobSnapshot is a recorded snapshot of a job with its summary.
type jobSnapshot struct {
	row SnapshotRow
	sum restic.Summary
}

// jobSnapshots returns the snapshots a job recorded, in batch order.
func (s *Service) jobSnapshots(ctx context.Context, destinationID, jobID int64) ([]jobSnapshot, error) {
	rows, err := s.o.DB.Reader().QueryContext(ctx, `SELECT `+snapshotColumns+`, summary FROM engine_snapshots
		WHERE destination_id = ? AND job_id = ? ORDER BY batch, id`, destinationID, jobID)
	if err != nil {
		return nil, fmt.Errorf("read the snapshots of job %d: %w", jobID, err)
	}
	defer rows.Close()
	var out []jobSnapshot
	for rows.Next() {
		var js jobSnapshot
		var summary string
		var (
			source, job sql.NullInt64
			created     string
			complete    int
		)
		if err := rows.Scan(&js.row.ID, &js.row.DestinationID, &source, &job, &js.row.SnapshotID, &created, &js.row.Batch, &complete,
			&js.row.Files, &js.row.Bytes, &js.row.DataAdded, &summary); err != nil {
			return nil, fmt.Errorf("read the snapshots of job %d: %w", jobID, err)
		}
		js.row.SourceID, js.row.JobID, js.row.Complete = source.Int64, job.Int64, complete == 1
		if js.row.CreatedAt, err = db.ParseTime(created); err != nil {
			return nil, fmt.Errorf("read the snapshots of job %d: %w", jobID, err)
		}
		_ = json.Unmarshal([]byte(summary), &js.sum)
		out = append(out, js)
	}
	return out, rows.Err()
}

// readBackTable is the job's temporary read-back table (on the writer connection).
func readBackTable(jobID int64) string { return fmt.Sprintf("readback_%d", jobID) }

// readBack streams a snapshot's file nodes into the job's temporary table and returns the nodes
// of the paths in want.
func (x *resticRun) readBack(ctx context.Context, snapshotID string, want map[string]bool) (map[string]restic.Node, error) {
	name := readBackTable(x.job.ID)
	write := func(fn func(tx *sql.Tx) error) error { return x.s.o.DB.Write(context.WithoutCancel(ctx), fn) }
	if err := write(func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS `+name+` (path TEXT PRIMARY KEY, size INTEGER NOT NULL,
			mtime_ns INTEGER NOT NULL)`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM temp.`+name)
		return err
	}); err != nil {
		return nil, fmt.Errorf("read-back table: %w", err)
	}
	nodes := map[string]restic.Node{}
	var buf []restic.Node
	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		err := write(func(tx *sql.Tx) error {
			stmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO temp.`+name+` (path, size, mtime_ns) VALUES (?, ?, ?)`)
			if err != nil {
				return err
			}
			defer stmt.Close()
			for _, n := range buf {
				if _, err := stmt.ExecContext(ctx, n.Path, n.Size, n.MtimeNs); err != nil {
					return err
				}
			}
			return nil
		})
		buf = buf[:0]
		return err
	}
	err := x.repo.Ls(ctx, snapshotID, func(n restic.Node) error {
		if n.Type != "file" {
			return nil
		}
		if want[n.Path] {
			nodes[n.Path] = n
		}
		buf = append(buf, n)
		if len(buf) >= 2000 {
			return flush()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := flush(); err != nil {
		return nil, fmt.Errorf("read-back table: %w", err)
	}
	return nodes, nil
}

// dropReadBack removes the job's temporary table.
func (x *resticRun) dropReadBack(ctx context.Context) {
	_ = x.s.o.DB.Write(context.WithoutCancel(ctx), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.`+readBackTable(x.job.ID))
		return err
	})
}

// recordBatch reads back and records batch k's snapshot (see the file comment). pendingOnMiss (a
// resumed job's reconciliation) leaves the items the snapshot does not hold pending instead of
// failing them: their error lines died with the process, and the batch runs again. It returns the
// items recorded done.
func (x *resticRun) recordBatch(ctx context.Context, src catalog.Source, k int, batch []batchWork, snapshotID string,
	errs []restic.ErrorLine, sum restic.Summary, base string, pendingOnMiss bool) (map[int64]bool, error) {
	snaps, err := x.repo.Snapshots(ctx, nil, []string{snapshotID})
	if err != nil {
		return nil, fmt.Errorf("read back snapshot %s: %w", short(snapshotID), err)
	}
	var snap *restic.Snapshot
	for i := range snaps {
		if snaps[i].ID == snapshotID || strings.HasPrefix(snaps[i].ID, snapshotID) {
			snap = &snaps[i]
		}
	}
	want := []string{restic.DestTag(x.ed.EngineTag), restic.JobTag(x.job.ID), restic.BatchTag(k), restic.SourceTag(src.ID)}
	if snap == nil || !fullIDRe.MatchString(snap.ID) || !snap.HasTags(want...) {
		return nil, fmt.Errorf("snapshot %s of batch %d is not in the repository with its tags", short(snapshotID), k)
	}
	paths := map[string]bool{}
	for _, w := range batch {
		paths[absPath(src, w.file)] = true
	}
	nodes, err := x.readBack(ctx, snap.ID, paths)
	if err != nil {
		return nil, fmt.Errorf("read back snapshot %s: %w", short(snap.ID), err)
	}
	defer x.dropReadBack(ctx)
	summary, _ := json.Marshal(sum)
	type outcome struct {
		w      batchWork
		status jobs.ItemStatus
		msg    string
	}
	var outcomes []outcome
	now := x.s.now()
	err = x.s.o.DB.Write(context.WithoutCancel(ctx), func(tx *sql.Tx) error {
		outcomes = outcomes[:0]
		if err := insertSnapshotTx(ctx, tx, SnapshotRow{DestinationID: x.d.ID, SourceID: src.ID, JobID: x.job.ID, SnapshotID: snap.ID,
			CreatedAt: snap.Time, Batch: k, Files: sum.TotalFilesProcessed, Bytes: sum.TotalBytesProcessed, DataAdded: sum.DataAdded}, summary); err != nil {
			return err
		}
		recorded := map[string]int64{}
		for _, w := range batch {
			abs := absPath(src, w.file)
			n, ok := nodes[abs]
			if !ok || n.Size != w.size || !mtimeHolds(n.MtimeNs, w.mtimeNs) {
				if pendingOnMiss {
					outcomes = append(outcomes, outcome{w: w, status: jobs.ItemPending})
					continue
				}
				outcomes = append(outcomes, outcome{w: w, status: jobs.ItemFailed, msg: notHeldMessage(errs, abs)})
				continue
			}
			id, err := x.recordItemTx(ctx, tx, src, w, base, now, recorded)
			if errors.Is(err, syncer.ErrRecordChanged) || errors.Is(err, syncer.ErrNotDependent) {
				outcomes = append(outcomes, outcome{w: w, status: jobs.ItemFailed, msg: err.Error()})
				continue
			}
			if err != nil {
				return err
			}
			recorded[w.file] = id
			outcomes = append(outcomes, outcome{w: w, status: jobs.ItemDone})
		}
		_, _, err := x.s.files.ApplyReadBackTx(ctx, tx, syncer.ReadBack{DestinationID: x.d.ID, SourceID: src.ID, Base: base,
			Table: "temp." + readBackTable(x.job.ID), Root: src.Path})
		return err
	})
	if err != nil {
		return nil, err
	}
	if x.listed == nil {
		x.listed = map[string]bool{}
	}
	x.listed[snap.ID] = true
	faultinject.Point(PointAfterRecord)
	done := map[int64]bool{}
	for _, o := range outcomes {
		switch o.status {
		case jobs.ItemDone:
			done[o.w.it.ID] = true
			if err := x.finish(ctx, o.w.it.ID, jobs.ItemDone, contentBytes(o.w.it), ""); err != nil {
				return nil, err
			}
		case jobs.ItemFailed:
			if err := x.failItem(ctx, o.w.it, o.msg); err != nil {
				return nil, err
			}
		}
	}
	x.log(slog.LevelInfo, "batch recorded", "batch", k, "snapshot", short(snap.ID), "source", src.Name, "done", len(done),
		"items", len(batch))
	return done, nil
}

// notHeldMessage is the error of an item the snapshot does not hold: restic's error line for
// its file when there is one.
func notHeldMessage(errs []restic.ErrorLine, abs string) string {
	for _, e := range errs {
		if e.Item == abs {
			return "could not be read: " + e.Message
		}
	}
	return defaultNotHeld
}

// recordItemTx records one held content item (§6.2 step 5) and returns its record's id.
func (x *resticRun) recordItemTx(ctx context.Context, tx *sql.Tx, src catalog.Source, w batchWork, base string, now time.Time,
	recorded map[string]int64) (int64, error) {
	d := w.d
	if w.it.Action == jobs.ActionPromote {
		if err := x.s.files.PromoteTx(ctx, tx, d.RecordID, d.TargetID, false, d.For, x.job.ID, now); err != nil {
			return 0, err
		}
		return d.TargetID, nil
	}
	c := syncer.ContentDone{DestinationID: x.d.ID, SourceID: d.SourceID, RelPath: w.it.RelPath, SourceRelPath: d.Source, Size: d.Size,
		MtimeNs: d.MtimeNs, HeadTail: storedHeadTail(d, d.Size, d.MtimeNs), State: syncer.StatePresent, JobID: x.job.ID, CopiedAt: now}
	var old *syncer.Record
	if d.RecordID != 0 {
		rec, err := x.s.files.Get(ctx, d.RecordID)
		if err != nil && !errors.Is(err, syncer.ErrNotFound) {
			return 0, err
		}
		if err == nil && rec.State.Live() {
			c.RecordID = rec.ID
			old = &rec
		}
	}
	switch w.it.Action {
	case jobs.ActionMove:
		c.Moved = true
		if old != nil && c.HeadTail == "" {
			c.HeadTail = old.HeadTail
		}
	case jobs.ActionLink:
		if x.d.Settings.Hardlinks != destinations.HardlinksRecreate {
			// Copy mode: every name is a file of its own in the snapshot.
			break
		}
		if id, ok := recorded[d.Primary]; ok {
			c.State, c.LinkOf = syncer.StateLinked, id
		} else if prim, ok, err := x.s.files.LiveBySourcePath(ctx, x.d.ID, d.SourceID, d.Primary); err != nil {
			return 0, err
		} else if ok && prim.State == syncer.StatePresent && prim.Size == d.Size && mtimeHolds(prim.MtimeNs, d.MtimeNs) {
			c.State, c.LinkOf = syncer.StateLinked, prim.ID
		}
	}
	id, err := x.s.files.RecordContentTx(ctx, tx, c)
	if err != nil {
		return 0, err
	}
	if old != nil && replacesVersion(w.it, *old) {
		// An update keeps its old version as a row of its own (§6.2 step 5, D29 included).
		ref := old.EngineRef
		if ref == "" {
			ref = base
		}
		if ref == "" {
			x.log(slog.LevelWarn, "the old version of an update is in no snapshot; it is not kept", "path", old.RelPath)
			return id, nil
		}
		reason := syncer.ReasonReplaced
		if old.State == syncer.StateMissing {
			reason = syncer.ReasonDamaged
		}
		if _, err := x.s.files.InsertRetainedTx(ctx, tx, syncer.RetainedVersion{DestinationID: x.d.ID, SourceID: old.SourceID,
			RelPath: old.RelPath, SourceRelPath: old.SourceRelPath, Size: old.Size, MtimeNs: old.MtimeNs, Hash: old.Hash, HeadTail: old.HeadTail,
			EngineRef: ref, RetainedPath: absPath(src, old.SourceRelPath), Reason: reason, JobID: x.job.ID, RetainedAt: now,
			ExpiresAt: x.expiry(now)}); err != nil {
			return 0, err
		}
	}
	if t := x.s.o.Tiers; t != nil && w.it.Action == jobs.ActionMove && old != nil && old.SourceRelPath != d.Source {
		if err := t.MoveFlagsTx(ctx, tx, d.SourceID, old.SourceRelPath, d.Source); err != nil {
			return 0, err
		}
	}
	return id, nil
}

// replacesVersion reports an item whose record held another version of the file: an update, or a
// relink of a name whose own content changed.
func replacesVersion(it jobs.Item, old syncer.Record) bool {
	switch it.Action {
	case jobs.ActionUpdate:
		return true
	case jobs.ActionLink:
		return old.State != syncer.StateMissing && old.LinkOf == 0
	}
	return false
}

// --- preflight and reconciliation (§6.2 step 1) ---

// preflightListing checks that the recorded snapshots still exist. A recorded snapshot removed
// outside Bunkarr is a warning: its row is deleted, retained rows that reference it are deleted
// ("version lost"), live records that reference it, and when it was a source's base that source's
// records in the base, become missing (the plan uploads them again), and no --parent is passed for
// that source.
func (x *resticRun) preflightListing(ctx context.Context) error {
	snaps, err := x.repo.Snapshots(ctx, nil, nil)
	if err != nil {
		return err
	}
	x.listed = map[string]bool{}
	for _, s := range snaps {
		x.listed[s.ID] = true
	}
	rows, err := x.s.snapshotRows(ctx, x.d.ID)
	if err != nil {
		return err
	}
	bases := basesOf(rows)
	var removed []SnapshotRow
	for _, r := range rows {
		if !x.listed[r.SnapshotID] {
			removed = append(removed, r)
		}
	}
	if len(removed) == 0 {
		return nil
	}
	var lost []syncer.Record
	var missing int64
	err = x.s.o.DB.Write(context.WithoutCancel(ctx), func(tx *sql.Tx) error {
		lost, missing = nil, 0
		for _, r := range removed {
			if err := deleteSnapshotRowTx(ctx, tx, x.d.ID, r.SnapshotID); err != nil {
				return err
			}
			l, err := x.s.files.DeleteRetainedByRefTx(ctx, tx, x.d.ID, r.SnapshotID)
			if err != nil {
				return err
			}
			lost = append(lost, l...)
			n, err := x.s.files.MarkMissingByRefTx(ctx, tx, x.d.ID, r.SnapshotID)
			if err != nil {
				return err
			}
			missing += n
			if r.SourceID != 0 && bases[r.SourceID] == r.SnapshotID {
				n, err := x.s.files.MarkMissingBaseTx(ctx, tx, x.d.ID, r.SourceID)
				if err != nil {
					return err
				}
				missing += n
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, r := range removed {
		x.warn(fmt.Sprintf("snapshot %s was removed outside Bunkarr", short(r.SnapshotID)), "snapshot", r.SnapshotID, "source", r.SourceID)
		if r.SourceID != 0 && bases[r.SourceID] == r.SnapshotID {
			x.baseGone[r.SourceID] = true
			x.log(slog.LevelWarn, "the source's newest snapshot is gone: the next backup reads every file (no --parent)", "source", r.SourceID)
		}
	}
	for _, rec := range lost {
		x.warn("version lost", "path", rec.RelPath, "retained", rec.RetainedPath, "snapshot", short(rec.EngineRef))
	}
	if missing > 0 {
		x.log(slog.LevelWarn, "records whose snapshot is gone are marked missing and uploaded again", "count", missing)
	}
	return nil
}

// reconcileJob reads back the batch snapshots earlier attempts of this job made but did not record
// (a crash between restic's exit and the record), in batch order, exactly as a batch is recorded,
// except that items the snapshot does not hold stay pending (resumed: the plan is complete). It
// also sets the job's next batch number. Nothing is assumed done because a snapshot exists.
func (x *resticRun) reconcileJob(ctx context.Context, resumed bool) error {
	rows, err := x.s.jobSnapshots(ctx, x.d.ID, x.job.ID)
	if err != nil {
		return err
	}
	recorded := map[string]bool{}
	for _, r := range rows {
		recorded[r.row.SnapshotID] = true
		x.lastBatch = max(x.lastBatch, r.row.Batch)
	}
	if !resumed {
		return nil
	}
	snaps, err := x.repo.Snapshots(ctx, []string{restic.JobTag(x.job.ID)}, nil)
	if err != nil {
		return err
	}
	type unrec struct {
		snap restic.Snapshot
		info restic.TagInfo
	}
	var todo []unrec
	for _, s := range snaps {
		info, ok := restic.ParseTags(s.Tags, x.ed.EngineTag)
		if !ok || info.Kind != engines.VersionMedia || info.JobID != x.job.ID {
			continue
		}
		x.lastBatch = max(x.lastBatch, info.Batch)
		if !recorded[s.ID] {
			todo = append(todo, unrec{snap: s, info: info})
		}
	}
	if len(todo) == 0 {
		return nil
	}
	slices.SortFunc(todo, func(a, b unrec) int {
		if a.info.Batch != b.info.Batch {
			return a.info.Batch - b.info.Batch
		}
		return a.snap.Time.Compare(b.snap.Time)
	})
	items, err := x.pending(ctx)
	if err != nil {
		return err
	}
	for _, u := range todo {
		src, ok := x.sources[u.info.SourceID]
		if !ok {
			x.log(slog.LevelWarn, "an unrecorded snapshot of this job belongs to a source that is no longer synced; it is not recorded",
				"snapshot", short(u.snap.ID))
			continue
		}
		var batch []batchWork
		for _, it := range items {
			if !contentAction(it.Action) {
				continue
			}
			w, err := x.work(ctx, it)
			if err != nil || w.d.SourceID != src.ID {
				continue
			}
			batch = append(batch, w)
		}
		base, err := x.s.baseOf(ctx, x.d.ID, src.ID)
		if err != nil {
			return err
		}
		var sum restic.Summary
		if u.snap.Summary != nil {
			sum = *u.snap.Summary
		}
		x.log(slog.LevelInfo, "recording a batch snapshot an earlier attempt made", "snapshot", short(u.snap.ID), "batch", u.info.Batch)
		if _, err := x.recordBatch(ctx, src, u.info.Batch, batch, u.snap.ID, nil, sum, base, true); err != nil {
			return err
		}
		if items, err = x.pending(ctx); err != nil {
			return err
		}
	}
	return nil
}

// --- finish (§6.2 step 6) ---

// finishSync marks each source's last batch complete and decides the retains and releases after
// every content item, with the S6 wait: a waiting retain fails with the S6 warning and keeps its
// record live with engine_ref = COALESCE(engine_ref, base); every other becomes retained with
// engine_ref = COALESCE(engine_ref, base) and the absolute path it had in that snapshot.
func (x *resticRun) finishSync(ctx context.Context, retains []jobs.Item) error {
	type decided struct {
		it      jobs.Item
		d       engineDetail
		rec     syncer.Record
		reason  string
		wait    string
		release bool
	}
	var list []decided
	for _, it := range retains {
		if err := ctx.Err(); err != nil {
			return err
		}
		d, err := parseItem(it)
		if err != nil {
			if err := x.failItem(ctx, it, err.Error()); err != nil {
				return err
			}
			continue
		}
		if x.roots[d.SourceID] == nil {
			x.warnings++
			if err := x.finish(ctx, it.ID, jobs.ItemSkipped, 0, fmt.Sprintf("source %d is no longer synced to this destination", d.SourceID)); err != nil {
				return err
			}
			continue
		}
		v, err := x.s.files.Get(ctx, d.RecordID)
		if errors.Is(err, syncer.ErrNotFound) {
			if err := x.finish(ctx, it.ID, jobs.ItemDone, 0, ""); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if v.State == syncer.StateRetained && v.SourceID == d.SourceID && v.SourceRelPath == d.Source {
			if err := x.finish(ctx, it.ID, jobs.ItemDone, v.Size, ""); err != nil {
				return err
			}
			continue
		}
		if !v.State.Live() || v.SourceID != d.SourceID || v.SourceRelPath != d.Source {
			if err := x.finish(ctx, it.ID, jobs.ItemSkipped, 0, "the record changed since planning"); err != nil {
				return err
			}
			continue
		}
		dc := decided{it: it, d: d, rec: v, reason: syncer.ReasonDeleted}
		if d.Reason == syncer.ReasonReleased {
			dc.reason, dc.release = syncer.ReasonReleased, true
			why, err := x.plan.CheckRelease(ctx, v, d.TierRevision)
			if err != nil {
				return err
			}
			if why != "" {
				if err := x.finish(ctx, it.ID, jobs.ItemSkipped, 0, why); err != nil {
					return err
				}
				continue
			}
		} else {
			back, err := x.s.reappeared(ctx, x.roots[d.SourceID], d.SourceID, v.SourceRelPath)
			if err != nil {
				return err
			}
			if back {
				if err := x.finish(ctx, it.ID, jobs.ItemSkipped, 0, "it reappeared at the source; not retained"); err != nil {
					return err
				}
				continue
			}
			if v.State != syncer.StateMissing {
				name, err := x.plan.NotBackedUp(ctx, d.SourceID, v.SourceRelPath)
				if err != nil {
					return err
				}
				if name != "" {
					dc.wait = fmt.Sprintf("not retained yet: %s in the same folder is not backed up (its copy failed or was held); %s stays until it is",
						name, v.RelPath)
				}
			}
		}
		list = append(list, dc)
	}
	bases := map[int64]string{}
	rows, err := x.s.snapshotRows(ctx, x.d.ID)
	if err != nil {
		return err
	}
	for src, id := range basesOf(rows) {
		bases[src] = id
	}
	jobRows, err := x.s.jobSnapshots(ctx, x.d.ID, x.job.ID)
	if err != nil {
		return err
	}
	type outcome struct {
		dc     decided
		status jobs.ItemStatus
		msg    string
	}
	var outcomes []outcome
	now := x.s.now()
	err = x.s.o.DB.Write(context.WithoutCancel(ctx), func(tx *sql.Tx) error {
		outcomes = outcomes[:0]
		marked := map[int64]bool{}
		for _, r := range jobRows {
			if r.row.SourceID != 0 && !marked[r.row.SourceID] {
				marked[r.row.SourceID] = true
				if err := markCompleteTx(ctx, tx, x.d.ID, r.row.SourceID, x.job.ID); err != nil {
					return err
				}
			}
		}
		for _, dc := range list {
			base := bases[dc.d.SourceID]
			switch {
			case dc.rec.State == syncer.StateMissing:
				// Nothing holds its version any more: the record goes.
				if err := x.s.files.DeleteRecordTx(ctx, tx, dc.rec.ID); err != nil {
					if errors.Is(err, syncer.ErrRecordChanged) {
						outcomes = append(outcomes, outcome{dc: dc, status: jobs.ItemFailed, msg: err.Error()})
						continue
					}
					return err
				}
				outcomes = append(outcomes, outcome{dc: dc, status: jobs.ItemDone})
			case dc.wait != "":
				if base != "" {
					if err := x.s.files.PinTx(ctx, tx, dc.rec.ID, base); err != nil {
						return err
					}
				}
				outcomes = append(outcomes, outcome{dc: dc, status: jobs.ItemFailed, msg: dc.wait})
			default:
				src := x.sources[dc.d.SourceID]
				err := x.s.files.RetainTx(ctx, tx, dc.rec.ID, syncer.Retain{Base: base, RetainedPath: absPath(src, dc.rec.SourceRelPath),
					Reason: dc.reason, JobID: x.job.ID, RetainedAt: now, ExpiresAt: x.expiry(now)})
				if errors.Is(err, syncer.ErrRecordChanged) {
					outcomes = append(outcomes, outcome{dc: dc, status: jobs.ItemFailed, msg: err.Error()})
					continue
				}
				if err != nil {
					return err
				}
				outcomes = append(outcomes, outcome{dc: dc, status: jobs.ItemDone})
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, o := range outcomes {
		if o.status == jobs.ItemDone {
			if err := x.finish(ctx, o.dc.it.ID, jobs.ItemDone, o.dc.rec.Size, ""); err != nil {
				return err
			}
			continue
		}
		if err := x.failItem(ctx, o.dc.it, o.msg); err != nil {
			return err
		}
	}
	return nil
}
