package enginerun

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/syncer"
)

// reconcile settles the retention intents stopped jobs left (§3.3), at the start of every rclone
// job that is not a dry run. A server-side move on S3 or B2 is a copy then a delete, so after a
// crash the object can be at the live path, at the retention path, or at both:
//   - only the retention path holds it: recorded retained, as filecopy (an update's old version
//     gets a retained row and the live record turns missing);
//   - only the live path holds it: the intent is cleared (nothing moved);
//   - both hold it, whatever the sizes: nothing is deleted. The retention object is recorded
//     retained with the intent's reason; an update's live record becomes present only when the
//     live object has the catalog's size and mtime (the upload finished), else missing; a retain
//     or release keeps its record live and runs again;
//   - neither: the intent is cleared and the record turns missing.
func (x *rcloneRun) reconcile(ctx context.Context) error {
	intents, err := x.s.files.Intents(ctx, x.d.ID)
	if err != nil || len(intents) == 0 {
		return err
	}
	var paths []string
	for _, r := range intents {
		paths = append(paths, r.RelPath, r.RetainedPath)
	}
	found, err := x.statMany(ctx, paths...)
	if err != nil {
		return err
	}
	now := x.s.now()
	settled := 0
	for _, r := range intents {
		live, lok := found[r.RelPath]
		ret, rok := found[r.RetainedPath]
		update := r.Reason == syncer.ReasonReplaced || r.Reason == syncer.ReasonDamaged
		var current bool
		if lok && rok && update {
			f, ok, err := x.s.catalogFile(ctx, r.SourceID, r.SourceRelPath)
			if err != nil {
				return err
			}
			current = ok && live.Size == f.Size && filecopy.MtimeMatch(live.MtimeNs(), f.MtimeNs, x.caps.MtimeGranularityNs, 0)
			if current {
				r.Size, r.MtimeNs = f.Size, f.MtimeNs
			}
		}
		err := x.write(ctx, func(tx *sql.Tx) error {
			switch {
			case rok && !lok && !update:
				err := x.s.files.RetainTx(ctx, tx, r.ID, syncer.Retain{RetainedPath: r.RetainedPath, Reason: r.Reason, JobID: x.job.ID,
					RetainedAt: now, ExpiresAt: x.expiry(now)})
				if !errors.Is(err, syncer.ErrRecordChanged) {
					return err
				}
				// Live hardlinks still depend on it: the object is recorded where it is.
				return x.retainedBeside(ctx, tx, r, ret.Size, ret.MtimeNs(), true, now)
			case rok && !lok:
				return x.retainedBeside(ctx, tx, r, ret.Size, ret.MtimeNs(), true, now)
			case rok && lok:
				if err := x.retainedBeside(ctx, tx, r, ret.Size, ret.MtimeNs(), false, now); err != nil {
					return err
				}
				if !update {
					return nil
				}
				if current {
					_, err := x.s.files.RecordContentTx(ctx, tx, syncer.ContentDone{DestinationID: x.d.ID, SourceID: r.SourceID,
						RecordID: r.ID, RelPath: r.RelPath, SourceRelPath: r.SourceRelPath, Size: r.Size, MtimeNs: r.MtimeNs,
						State: syncer.StatePresent, JobID: x.job.ID, CopiedAt: now})
					return err
				}
				return x.s.files.MarkMissingTx(ctx, tx, r.ID)
			case lok:
				return x.s.files.ClearIntentTx(ctx, tx, r.ID, r.RetainedPath)
			default:
				if err := x.s.files.ClearIntentTx(ctx, tx, r.ID, r.RetainedPath); err != nil {
					return err
				}
				return x.s.files.MarkMissingTx(ctx, tx, r.ID)
			}
		})
		if err != nil {
			return err
		}
		settled++
	}
	x.log(slog.LevelInfo, "settled the retention intents of stopped jobs", "count", settled)
	return nil
}

// retainedBeside records the object at an intent's retention path as a retained row beside the
// live record, clears the intent, and with missing marks the record missing (its live name holds
// nothing recorded).
func (x *rcloneRun) retainedBeside(ctx context.Context, tx *sql.Tx, r syncer.Record, size, mtimeNs int64, missing bool, now time.Time) error {
	hash := ""
	if size == r.Size {
		hash = r.Hash
	}
	if _, err := x.s.files.InsertRetainedTx(ctx, tx, syncer.RetainedVersion{DestinationID: x.d.ID, SourceID: r.SourceID, RelPath: r.RelPath,
		SourceRelPath: r.SourceRelPath, Size: size, MtimeNs: mtimeNs, Hash: hash, HeadTail: r.HeadTail, RetainedPath: r.RetainedPath,
		Reason: r.Reason, JobID: x.job.ID, RetainedAt: now, ExpiresAt: x.expiry(now)}); err != nil {
		return err
	}
	if err := x.s.files.ClearIntentTx(ctx, tx, r.ID, r.RetainedPath); err != nil {
		return err
	}
	if missing {
		return x.s.files.MarkMissingTx(ctx, tx, r.ID)
	}
	return nil
}
