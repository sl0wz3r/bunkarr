package syncer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// retainedVersion describes a file kept in retention: what its retained row records.
type retainedVersion struct {
	mtimeNs      int64
	hash, reason string
}

// intentFixer settles retention intents (see store.go) for the item that wrote one and stops
// before recording the outcome, and for the next job of the destination when that item's job
// failed or was cancelled.
type intentFixer struct {
	store *Store
	h     *destinations.Handle
	rep   jobs.Reporter
	now   func() time.Time
	jobID int64
}

func (f *intentFixer) lstat(rel string) (filecopy.Stat, bool, error) {
	st, err := f.h.Lstat(rel)
	if notExist(err) {
		return filecopy.Stat{}, false, nil
	}
	if err != nil {
		return filecopy.Stat{}, false, sideErr(filecopy.SideDestination, "stat", rel, err)
	}
	return st, true, nil
}

// settle settles the retention intent of the live record rec, whose item will not finish it. A
// file renamed into retention goes back to its path (never over anything). A file found at both
// names (a hardlink into retention, or a new file already at the path), or one that cannot be moved
// back, stays in retention and is recorded there as a retained row: v describes it, or with v nil
// the intent's reason and no hash (its content was not checked). When the path then holds another
// file than the retained one (a new version renamed there, or a file that appeared), nothing says
// what that file is: a present or linked record becomes missing, so the next sync copies the
// source again and keeps that file in retention as damaged, never with the record's hash. Then the
// intent is cleared. Nothing is deleted. An error leaves the intent for a later job. Only telling
// whether the path holds the retained file (sameDestFile, which may read both) stops with ctx;
// that happens before anything changes.
func (f *intentFixer) settle(ctx context.Context, rec Record, v *retainedVersion) error {
	hctx := ctx
	ctx = context.WithoutCancel(ctx) // the settling must not stop half-way
	target := rec.RetainedPath
	ret, kept, err := f.lstat(target)
	if err != nil {
		return err
	}
	if kept && !ret.Regular() {
		return fmt.Errorf("%s is not a regular file (%s); the retention of %s is left as it is", target, ret.Mode.Type(), rec.RelPath)
	}
	record, unknown := kept, false
	if kept {
		cur, there, err := f.lstat(rec.RelPath)
		same := false
		if err == nil && there {
			if same, err = sameDestFile(hctx, f.h, rec.RelPath, cur, target, ret); err != nil {
				return err
			}
		}
		switch {
		case err == nil && !there:
			if err := filecopy.Move(f.h.Root, target, rec.RelPath, true); err != nil {
				f.rep.Log(slog.LevelWarn, "could not move a file back from retention; it is recorded there", "path", rec.RelPath,
					"retained", target, "error", err.Error())
			} else {
				record = false
				f.pruneRetention(path.Dir(target))
				f.rep.Log(slog.LevelInfo, "moved a file back from retention: it is not retained", "path", rec.RelPath)
			}
		case err == nil && !same:
			unknown = true
		}
	}
	return f.store.db.Write(ctx, func(tx *sql.Tx) error {
		if unknown {
			if _, err := tx.ExecContext(ctx, `UPDATE destination_files SET state = 'missing'
				WHERE id = ? AND retained_path = ? AND state IN ('present', 'linked')`, rec.ID, target); err != nil {
				return fmt.Errorf("mark record %d missing: %w", rec.ID, err)
			}
		}
		if record {
			if v == nil {
				v = &retainedVersion{mtimeNs: rec.MtimeNs, reason: rec.Reason}
				if v.reason == "" {
					v.reason = ReasonDeleted
				}
				if v.reason == ReasonDamaged {
					v.mtimeNs = ret.MtimeNs
				}
			}
			now := f.now()
			exp := expiry(f.h, now)
			row := Record{DestinationID: rec.DestinationID, SourceID: rec.SourceID, RelPath: rec.RelPath, SourceRelPath: rec.SourceRelPath,
				Size: ret.Size, MtimeNs: v.mtimeNs, Hash: v.hash, State: StateRetained, RetainedPath: target, Reason: v.reason,
				JobID: f.jobID, RetainedAt: &now, ExpiresAt: &exp}
			if exists, err := retainedExists(ctx, tx, row); err != nil {
				return err
			} else if !exists {
				if _, err := insertRecord(ctx, tx, row); err != nil {
					return err
				}
			}
			f.rep.Log(slog.LevelInfo, "recorded a file an interrupted item had kept in retention", "path", rec.RelPath, "retained", target)
		}
		return clearIntentTx(ctx, tx, rec.ID, target)
	})
}

// pruneRetention removes dir and its parents while they are empty directories inside a job's
// retention directory (a file moved back left them). Best effort.
func (f *intentFixer) pruneRetention(dir string) {
	for strings.HasPrefix(dir, filecopy.RetentionRoot+"/") {
		st, err := filecopy.Lstat(f.h.Root, dir)
		if err != nil || !st.Mode.IsDir() || f.h.Root.Remove(dir) != nil {
			return
		}
		dir = path.Dir(dir)
	}
}

// reconcile settles what earlier jobs of the destination left half done because the item that
// started it never ran again (its job failed or was cancelled before reaching the item, and such a
// job is never resumed), so nothing stays unrecorded in retention and no record says present while
// its file is in retention:
//   - for the pending items of the jobs that own a directory in .bunkarr/retention, a replacement
//     an item completed at the destination is recorded (finishPlaced), an unmanaged file an item
//     displaced into retention is recorded (reason displaced, an orphan when its source is gone),
//     and the item's temp file is removed (sweep);
//   - every other retention intent (a file with a live record renamed or hardlinked into
//     retention, the outcome not recorded) is settled (intentFixer.settle).
//
// A sync skips its own retention directory: its pending items finish what is there. It runs at the
// start of every sync, verify and retention job that is not a dry run (S9), after the destination
// was opened; items reads the stopped jobs' items (only the jobs.ItemStore contract is used). It
// returns the number of intents or files it could not settle (warnings) and fatal errors.
func reconcile(ctx context.Context, f *intentFixer, items jobs.ItemStore, ownDir string) (int, error) {
	warnings := 0
	fail := func(what string, err error) error {
		if ctx.Err() != nil || filecopy.Classify(err) == filecopy.Fatal {
			return err
		}
		warnings++
		f.rep.Log(slog.LevelWarn, "could not settle what an interrupted job left in retention", "path", what, "error", err.Error())
		return nil
	}
	if err := sweep(ctx, f, items, ownDir, fail); err != nil {
		return warnings, err
	}
	intents, err := f.store.intents(ctx, f.h.Destination.ID)
	if err != nil {
		return warnings, err
	}
	for _, rec := range intents {
		if ownDir != "" && strings.HasPrefix(rec.RetainedPath, ownDir+"/") {
			continue
		}
		if err := f.settle(ctx, rec, nil); err != nil {
			if err := fail(rec.RelPath, err); err != nil {
				return warnings, err
			}
		}
	}
	return warnings, nil
}

// sweep runs sweepItem for the pending items of every job that owns a directory in
// .bunkarr/retention, except ownDir. It runs before the intents are settled: a file moved back
// can leave a job's directory empty, and it is then removed.
func sweep(ctx context.Context, f *intentFixer, items jobs.ItemStore, ownDir string, fail func(string, error) error) error {
	dir, err := f.h.Root.Open(filecopy.RetentionRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return sideErr(filecopy.SideDestination, "open", filecopy.RetentionRoot, err)
	}
	entries, err := dir.ReadDir(-1)
	_ = dir.Close()
	if err != nil {
		return sideErr(filecopy.SideDestination, "read", filecopy.RetentionRoot, err)
	}
	for _, e := range entries {
		jobDir := filecopy.RetentionRoot + "/" + e.Name()
		i := strings.LastIndex(e.Name(), "-job")
		if !e.IsDir() || i < 0 || jobDir == ownDir {
			continue
		}
		jobID, err := strconv.ParseInt(e.Name()[i+len("-job"):], 10, 64)
		if err != nil || jobID <= 0 {
			continue
		}
		after := int64(0)
		for {
			batch, err := items.Pending(ctx, jobID, after, pendingBatch)
			if err != nil {
				return err
			}
			if len(batch) == 0 {
				break
			}
			for _, it := range batch {
				after = it.ID
				if err := sweepItem(ctx, f, jobID, jobDir, it); err != nil {
					if err := fail(it.RelPath, err); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// syncActions are the item actions of a sync job (the only jobs with retention directories).
var syncActions = map[jobs.ItemAction]bool{jobs.ActionCopy: true, jobs.ActionUpdate: true, jobs.ActionAdopt: true,
	jobs.ActionMove: true, jobs.ActionLink: true, jobs.ActionPromote: true, jobs.ActionRetain: true}

// sweepItem records the unmanaged file a pending item of the stopped job jobID displaced into its
// retention directory jobDir and the replacement it completed (finishPlaced), and removes the
// item's temp file.
func sweepItem(ctx context.Context, f *intentFixer, jobID int64, jobDir string, it jobs.Item) error {
	if !syncActions[it.Action] {
		return nil
	}
	d, err := parseDetail(it)
	if err != nil {
		return nil // not a sync item's detail
	}
	if d.Displaced != "" && strings.HasPrefix(d.Displaced, jobDir+"/") {
		st, ok, err := f.lstat(d.Displaced)
		if err != nil {
			return err
		}
		if ok && st.Regular() {
			now := f.now()
			exp := expiry(f.h, now)
			row := Record{DestinationID: f.h.Destination.ID, SourceID: d.SourceID, RelPath: it.RelPath, SourceRelPath: d.Source,
				Size: st.Size, MtimeNs: st.MtimeNs, State: StateRetained, RetainedPath: d.Displaced, Reason: ReasonDisplaced,
				JobID: jobID, RetainedAt: &now, ExpiresAt: &exp}
			wctx := context.WithoutCancel(ctx)
			err := f.store.db.Write(wctx, func(tx *sql.Tx) error {
				// The source may have been deleted since the job stopped: the row is then an orphan.
				if ok, err := sourceExists(wctx, tx, row.SourceID); err != nil {
					return err
				} else if !ok {
					row.SourceID = 0
				}
				if exists, err := retainedExists(wctx, tx, row); err != nil || exists {
					return err
				}
				_, err := insertRecord(wctx, tx, row)
				return err
			})
			if err != nil {
				return err
			}
		}
	}
	if d.Temp != "" && d.TempDone && strings.HasPrefix(d.Retained, jobDir+"/") {
		if err := f.finishPlaced(ctx, jobID, it, d); err != nil {
			return err
		}
	}
	if d.Temp != "" {
		if err := filecopy.CleanupTemp(f.h.Root, d.Temp); err != nil {
			return err
		}
	}
	return nil
}

// finishPlaced records the replacement a pending item of the stopped job jobID completed at the
// destination but not in the database: the old version is in retention (d.Retained), the temp
// file is gone, and the path holds another file than the retained one with the temp file's size
// and mtime (what a resumed item takes for its renamed temp file, see itemRun.place). While the
// live record still has the item's retention intent, it gets the new version and the old one its
// retained row, as the item would have recorded them (placedTx). Anything else is left to settle.
func (f *intentFixer) finishPlaced(ctx context.Context, jobID int64, it jobs.Item, d Detail) error {
	if _, ok, err := f.lstat(d.Temp); err != nil || ok {
		return err
	}
	fin, ok, err := f.lstat(it.RelPath)
	if err != nil || !ok || !fin.Regular() || fin.Size != d.TempSize ||
		!filecopy.MtimeMatch(fin.MtimeNs, d.TempMtimeNs, f.h.Capabilities.MtimeGranularityNs, 0) {
		return err
	}
	ret, ok, err := f.lstat(d.Retained)
	if err != nil || !ok {
		return err
	}
	if same, err := sameDestFile(ctx, f.h, it.RelPath, fin, d.Retained, ret); err != nil || same {
		return err
	}
	wctx := context.WithoutCancel(ctx)
	return f.store.db.Write(wctx, func(tx *sql.Tx) error {
		cur, ok, err := liveAtTx(wctx, tx, f.h.Destination.ID, it.RelPath)
		if err != nil || !ok || cur.RetainedPath != d.Retained || cur.SourceID != d.SourceID {
			return err // recorded already, or not this item's replacement
		}
		if err := placedTx(wctx, tx, f.h, jobID, it.RelPath, d, true, f.now()); err != nil {
			return err
		}
		f.rep.Log(slog.LevelInfo, "recorded the new version an interrupted item had put in place", "path", it.RelPath,
			"retained", d.Retained)
		return nil
	})
}

// expiry is when a file retained now at h expires (retention.deletedDays).
func expiry(h *destinations.Handle, now time.Time) time.Time {
	days := h.Retention.DeletedDays
	if days < 1 {
		days = destinations.DefaultDeletedDays
	}
	return now.Add(time.Duration(days) * 24 * time.Hour)
}

// newIntentFixer returns the fixer of a job on the open destination h.
func newIntentFixer(b base, h *destinations.Handle, rep jobs.Reporter, jobID int64) *intentFixer {
	return &intentFixer{store: b.store, h: h, rep: rep, now: b.now, jobID: jobID}
}
