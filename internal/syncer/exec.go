package syncer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// phases is the execution order of design §4.1 step 5 (S6: new before old).
var phases = []struct {
	name    string
	actions []jobs.ItemAction
}{
	{"copying", []jobs.ItemAction{jobs.ActionPromote}},
	{"copying", []jobs.ItemAction{jobs.ActionMove}},
	{"copying", []jobs.ItemAction{jobs.ActionCopy, jobs.ActionUpdate, jobs.ActionAdopt}},
	{"copying", []jobs.ItemAction{jobs.ActionLink}},
	{"retaining", []jobs.ItemAction{jobs.ActionRetain}},
}

// itemError is a per-file problem that fails the item (never the job), whatever errno it wraps.
type itemError struct{ msg string }

// Error implements error.
func (e *itemError) Error() string { return e.msg }

func itemErr(format string, args ...any) error { return &itemError{msg: fmt.Sprintf(format, args...)} }

// heldError holds the item instead of running it (the S10b shrink rule, re-checked at execution).
type heldError struct{ msg string }

// Error implements error.
func (e *heldError) Error() string { return e.msg }

// execute runs the pending items phase by phase. Fatal errors (filecopy.Classify) stop the job;
// item errors fail the item with a warning.
func (s *syncRun) execute(ctx context.Context) error {
	counts, err := s.env.Items.Counts(ctx, s.job.ID)
	if err != nil {
		return err
	}
	for _, c := range counts {
		if c.Status == jobs.ItemPending {
			s.progress.FilesTotal += c.Files
			if c.Action == jobs.ActionCopy || c.Action == jobs.ActionUpdate {
				s.progress.BytesTotal += c.Bytes
			}
		}
	}
	for _, ph := range phases {
		if slices.Contains(ph.actions, jobs.ActionRetain) {
			if err := s.h.Recheck(); err != nil {
				return err
			}
		}
		s.progress.Phase = ph.name
		after := int64(0)
		for {
			batch, err := s.env.Items.Pending(ctx, s.job.ID, after, pendingBatch)
			if err != nil {
				return err
			}
			if len(batch) == 0 {
				break
			}
			for _, it := range batch {
				after = it.ID
				if !slices.Contains(ph.actions, it.Action) {
					continue
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := s.runItem(ctx, it); err != nil {
					return err
				}
				s.progress.FilesDone++
				s.rep.Progress(s.progress)
				s.sinceRecheck++
				if s.sinceRecheck >= s.r.recheckEvery {
					s.sinceRecheck = 0
					if err := s.h.Recheck(); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// runItem executes one item and records a per-file failure.
func (s *syncRun) runItem(ctx context.Context, it jobs.Item) error {
	d, err := parseDetail(it)
	if err != nil {
		return s.failItem(ctx, it, err)
	}
	x := &itemRun{s: s, it: it, d: d, dst: s.h.Root}
	s.progress.CurrentFile = it.RelPath
	s.rep.Progress(s.progress)
	if d.SourceID != 0 {
		x.src = s.roots[d.SourceID]
		if x.src == nil {
			// The source was unlinked, disabled or deleted since planning: its records are
			// orphans now and are never changed (design §4, orphan rule).
			s.warnings++
			return x.finish(ctx, jobs.ItemSkipped, 0, fmt.Sprintf("source %d is no longer synced to this destination", d.SourceID))
		}
	}
	err = x.run(ctx)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		x.putBack(ctx)
		x.cleanupTemp()
		return ctx.Err()
	}
	var he *heldError
	if errors.As(err, &he) {
		x.putBack(ctx)
		x.cleanupTemp()
		s.rep.Log(slog.LevelWarn, "item held", "action", string(it.Action), "path", it.RelPath, "reason", he.msg)
		return x.finish(ctx, jobs.ItemHeld, 0, he.msg)
	}
	var ie *itemError
	if !errors.As(err, &ie) && filecopy.Classify(err) == filecopy.Fatal {
		x.salvage(ctx)
		return fmt.Errorf("%s %s: %w", it.Action, it.RelPath, err)
	}
	x.putBack(ctx)
	x.cleanupTemp()
	return s.failItem(ctx, it, err)
}

// putBack runs when an item stops after this or an earlier attempt renamed (or hardlinked) a file
// with a live record into retention but before the outcome was recorded (a retain, or the old
// version of an update or relink): it settles the record's retention intent (intentFixer.settle),
// so the record does not say present while its file sits unrecorded in retention (a stopped item
// is not resumed; the next sync plans the name again). A file that went into retention goes back
// to its path; one that cannot go back, or whose path holds a file again, is recorded as retained
// with the item's description of it. Best effort: an intent left behind is settled by the next job.
func (x *itemRun) putBack(ctx context.Context) {
	if x.d.Retained == "" {
		return
	}
	wctx := context.WithoutCancel(ctx)
	var rec Record
	var ok bool
	var err error
	if x.it.Action == jobs.ActionRetain {
		rec, err = x.s.r.store.Get(wctx, x.d.RecordID)
		ok = err == nil
	} else {
		rec, ok, err = x.s.r.store.LiveAt(wctx, x.destID(), x.it.RelPath)
	}
	if err != nil || !ok || !rec.State.Live() || rec.RetainedPath != x.d.Retained {
		return // the outcome was recorded, or the rename never started
	}
	v := &retainedVersion{mtimeNs: x.d.RetainedMtimeNs, hash: x.d.RetainedHash, reason: x.d.RetainedReason}
	if v.reason == "" {
		v = nil
	}
	if err := x.fixer().settle(ctx, rec, v); err != nil {
		x.s.rep.Log(slog.LevelWarn, "could not settle a file this item moved into retention; the next job does", "path", rec.RelPath,
			"retained", x.d.Retained, "error", err.Error())
	}
}

// fixer settles retention intents for this job.
func (x *itemRun) fixer() *intentFixer {
	return newIntentFixer(x.s.r.base, x.s.h, x.s.rep, x.s.job.ID)
}

// run executes the item's action.
func (x *itemRun) run(ctx context.Context) error {
	if err := x.recordEarlierDisplace(ctx); err != nil {
		return err
	}
	switch x.it.Action {
	case jobs.ActionCopy, jobs.ActionUpdate:
		return x.putItem(ctx)
	case jobs.ActionAdopt:
		return x.adopt(ctx)
	case jobs.ActionMove:
		return x.move(ctx)
	case jobs.ActionLink:
		return x.link(ctx)
	case jobs.ActionPromote:
		return x.promote(ctx)
	case jobs.ActionRetain:
		return x.retain(ctx)
	default:
		return itemErr("unknown action %q", x.it.Action)
	}
}

func (s *syncRun) failItem(ctx context.Context, it jobs.Item, cause error) error {
	msg := cause.Error()
	s.rep.Log(slog.LevelWarn, "item failed", "action", string(it.Action), "path", it.RelPath, "error", msg)
	return s.env.Items.Finish(context.WithoutCancel(ctx), it.ID, jobs.ItemFailed, 0, msg)
}

// salvage runs when a fatal error stops the job. A failed job is never resumed, so what the item
// already moved into retention is settled now (else it stays there unrecorded and never expires):
// a displaced unmanaged file is recorded; a retain's file, or the old version an update or relink
// kept, goes back to its path or is recorded where it is (putBack). Its temp file is removed, even
// the new version of a half-done replacement (the next sync writes it again). Best effort: the
// destination may be gone (what is left, the next job settles).
func (x *itemRun) salvage(ctx context.Context) {
	x.putBack(ctx)
	if err := x.recordEarlierDisplace(ctx); err != nil {
		x.s.rep.Log(slog.LevelWarn, "could not record what the stopped item moved into retention", "path", x.it.RelPath, "error", err.Error())
	}
	x.removeTemp()
}

// itemRun executes one item.
type itemRun struct {
	s   *syncRun
	it  jobs.Item
	d   Detail
	src *os.Root
	dst *os.Root
}

func (x *itemRun) destID() int64 { return x.s.h.Destination.ID }

// saveDetail persists the item's detail. It does not stop for a cancelled job: execution state
// must reach the database before the step it describes.
func (x *itemRun) saveDetail(ctx context.Context) error {
	return x.s.env.Items.SetDetail(context.WithoutCancel(ctx), x.it.ID, x.d.raw())
}

func (x *itemRun) finish(ctx context.Context, status jobs.ItemStatus, bytes int64, msg string) error {
	wctx := context.WithoutCancel(ctx)
	if x.d.Outcome != "" {
		if err := x.s.env.Items.SetDetail(wctx, x.it.ID, x.d.raw()); err != nil {
			return err
		}
	}
	return x.s.env.Items.Finish(wctx, x.it.ID, status, bytes, msg)
}

func (x *itemRun) done(ctx context.Context, bytes int64) error {
	return x.finish(ctx, jobs.ItemDone, bytes, "")
}

// write runs fn in a database transaction that a cancelled job does not interrupt (the
// filesystem change it records has already happened).
func (x *itemRun) write(ctx context.Context, fn func(ctx context.Context, tx *sql.Tx) error) error {
	wctx := context.WithoutCancel(ctx)
	return x.s.r.store.db.Write(wctx, func(tx *sql.Tx) error { return fn(wctx, tx) })
}

// lstat looks at a destination path: exists is false when nothing is there.
func (x *itemRun) lstat(rel string) (filecopy.Stat, bool, error) {
	if rel == "" {
		return filecopy.Stat{}, false, nil
	}
	st, err := x.s.h.Lstat(rel)
	if notExist(err) {
		return filecopy.Stat{}, false, nil
	}
	if err != nil {
		return filecopy.Stat{}, false, sideErr(filecopy.SideDestination, "stat", rel, err)
	}
	return st, true, nil
}

// sourceStat looks at a file inside the item's source.
func (x *itemRun) sourceStat(rel string) (filecopy.Stat, bool, error) {
	st, err := filecopy.Lstat(x.src, rel)
	if notExist(err) {
		return filecopy.Stat{}, false, nil
	}
	if err != nil {
		return filecopy.Stat{}, false, sideErr(filecopy.SideSource, "stat", rel, err)
	}
	return st, true, nil
}

// sourceFile stats the item's source file and requires a regular file.
func (x *itemRun) sourceFile() (filecopy.Stat, error) {
	st, ok, err := x.sourceStat(x.d.Source)
	if err != nil {
		return st, err
	}
	if !ok {
		return st, itemErr("%s vanished from the source", x.d.Source)
	}
	if !st.Regular() {
		return st, itemErr("%s is no longer a regular file at the source", x.d.Source)
	}
	return st, nil
}

// cleanupTemp removes the item's recorded temp file (best effort), unless it is the new version of
// a half-done replacement (the old version already renamed into retention, nothing at the final
// path): then it stays for a later attempt to finish the pair.
func (x *itemRun) cleanupTemp() {
	if x.d.Temp == "" {
		return
	}
	if x.d.TempDone && x.d.Retained != "" {
		t, complete, terr := x.lstat(x.d.Temp)
		_, retained, rerr := x.lstat(x.d.Retained)
		_, final, ferr := x.lstat(x.it.RelPath)
		if terr != nil || rerr != nil || ferr != nil || (complete && t.Size == x.d.TempSize && retained && !final) {
			return
		}
	}
	x.removeTemp()
}

// removeTemp removes the item's recorded temp file (best effort).
func (x *itemRun) removeTemp() {
	if x.d.Temp == "" {
		return
	}
	if err := filecopy.CleanupTemp(x.dst, x.d.Temp); err != nil {
		x.s.rep.Log(slog.LevelWarn, "could not remove a temp file", "path", x.d.Temp, "error", err.Error())
	}
}

// sideErr tags a filesystem error with the tree it happened in (filecopy.Classify treats
// permission errors on the destination as fatal), keeping the cause: context errors are returned
// as they are, a *filecopy.Error keeps its operation and path.
func sideErr(side filecopy.Side, op, rel string, err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var fe *filecopy.Error
	if errors.As(err, &fe) {
		if fe.Side != "" {
			return err
		}
		c := *fe
		c.Side = side
		return &c
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	return &filecopy.Error{Side: side, Op: op, Path: rel, Err: err}
}

func (x *itemRun) now() time.Time { return x.s.r.now() }

func (x *itemRun) expiresAt(now time.Time) time.Time { return expiry(x.s.h, now) }

// retentionTarget picks where rel goes in this job's retention directory (the first free name).
// Callers record it in the detail before the rename, so a resumed item finds it.
func (x *itemRun) retentionTarget(rel string) (string, error) {
	return filecopy.RetentionTarget(x.dst, x.s.retentionDir, rel)
}

// retainedRow is the retained record of a file moved into retention.
func (x *itemRun) retainedRow(rel, sourceRel, retained string, size, mtimeNs int64, hash, reason string, now time.Time) Record {
	exp := x.expiresAt(now)
	return Record{DestinationID: x.destID(), SourceID: x.d.SourceID, RelPath: rel, SourceRelPath: sourceRel, Size: size,
		MtimeNs: mtimeNs, Hash: hash, State: StateRetained, RetainedPath: retained, Reason: reason, JobID: x.s.job.ID,
		RetainedAt: &now, ExpiresAt: &exp}
}

// insertRetained records a retained file unless it is already recorded (idempotent).
func (x *itemRun) insertRetained(ctx context.Context, tx *sql.Tx, rec Record) error {
	exists, err := retainedExists(ctx, tx, rec)
	if err != nil || exists {
		return err
	}
	_, err = insertRecord(ctx, tx, rec)
	return err
}

// pruneLive removes the empty directories a move or retain left in the source's folder (never
// the folder itself, never a directory that is not empty).
func (x *itemRun) pruneLive(rel string) {
	folder := x.s.linked[x.d.SourceID].DestFolder
	if folder == "" {
		return
	}
	for dir := path.Dir(rel); dir != "." && strings.HasPrefix(dir, folder+"/"); dir = path.Dir(dir) {
		if x.s.targeted() && !x.s.inTargets(x.d.SourceID, dir) {
			return // a targeted sync prunes inside its paths only (§9.1)
		}
		st, err := filecopy.Lstat(x.dst, dir)
		if err != nil || !st.Mode.IsDir() {
			return
		}
		if err := x.dst.Remove(dir); err != nil {
			return
		}
	}
}

// --- adoption and displacement (S2, §4.4) ---

// tryAdopt checks an unrecorded destination file against the source (§4.4). In size+hash mode it
// hashes both sides and, on a match, sets the destination mtime to the source's.
func (x *itemRun) tryAdopt(ctx context.Context, dest string, fin, src filecopy.Stat) (ok bool, hash string, err error) {
	set, caps := x.s.h.Settings, x.s.h.Capabilities
	if !fin.Regular() || fin.Size != src.Size {
		return false, "", nil
	}
	switch set.AdoptExisting {
	case destinations.AdoptSizeMtime:
		return filecopy.MtimeMatch(fin.MtimeNs, src.MtimeNs, caps.MtimeGranularityNs, set.MtimeWindowSec), "", nil
	case destinations.AdoptSizeHash:
		sh, _, err := filecopy.HashFile(ctx, x.src, x.d.Source, nil)
		if err != nil {
			return false, "", sideErr(filecopy.SideSource, "hash", x.d.Source, err)
		}
		dh, _, err := filecopy.HashFile(ctx, x.dst, dest, nil)
		if err != nil {
			return false, "", sideErr(filecopy.SideDestination, "hash", dest, err)
		}
		if sh != dh {
			return false, "", nil
		}
		after, err := x.sourceFile()
		if err != nil {
			return false, "", err
		}
		if after.Size != src.Size || after.MtimeNs != src.MtimeNs {
			return false, "", itemErr("%s changed while it was hashed", x.d.Source)
		}
		if !filecopy.MtimeMatch(fin.MtimeNs, src.MtimeNs, caps.MtimeGranularityNs, 0) {
			if err := filecopy.SetMtime(x.dst, dest, src.MtimeNs); err != nil {
				return false, "", err
			}
			faultinject.Point(PointAdoptAfterSetMtime)
		}
		return true, sh, nil
	default:
		return false, "", nil
	}
}

// recordAdopted records an adopted file as present.
func (x *itemRun) recordAdopted(ctx context.Context, dest string, src filecopy.Stat, hash string) error {
	faultinject.Point(PointRecordAfterFS)
	now := x.now()
	err := x.write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		rec := Record{DestinationID: x.destID(), SourceID: x.d.SourceID, RelPath: dest, SourceRelPath: x.d.Source, Size: src.Size,
			MtimeNs: src.MtimeNs, Hash: hash, State: StatePresent, JobID: x.s.job.ID, CopiedAt: &now}
		cur, ok, err := liveAtTx(ctx, tx, x.destID(), dest)
		if err != nil {
			return err
		}
		if ok {
			if cur.SourceID != x.d.SourceID {
				return itemErr("%s is recorded for another source", dest)
			}
			rec.ID = cur.ID
			return updateRecord(ctx, tx, rec)
		}
		_, err = insertRecord(ctx, tx, rec)
		return err
	})
	if err != nil {
		return err
	}
	faultinject.Point(PointRecordAfterDB)
	return nil
}

// displace moves an unmanaged file out of the way into retention and records it (S2).
func (x *itemRun) displace(ctx context.Context, dest string, st filecopy.Stat) error {
	if !st.Regular() {
		return itemErr("%s exists at the destination and is not a regular file (%s); it is left alone", dest, st.Mode.Type())
	}
	if x.d.Displaced != "" {
		if _, exists, err := x.lstat(x.d.Displaced); err != nil {
			return err
		} else if exists {
			// An earlier attempt displaced a file there: make sure it is recorded, then pick a
			// new name for this one.
			if err := x.recordDisplaced(ctx); err != nil {
				return err
			}
			x.d.Displaced = ""
		}
	}
	for attempt := 0; ; attempt++ {
		if x.d.Displaced == "" {
			target, err := x.retentionTarget(dest)
			if err != nil {
				return err
			}
			x.d.Displaced, x.d.DisplacedSize, x.d.DisplacedMtimeNs = target, st.Size, st.MtimeNs
			if err := x.saveDetail(ctx); err != nil {
				return err
			}
		}
		err := filecopy.Move(x.dst, dest, x.d.Displaced, true)
		if errors.Is(err, filecopy.ErrExists) && attempt < 3 {
			x.d.Displaced = ""
			continue
		}
		if err != nil {
			return err
		}
		break
	}
	faultinject.Point(PointDisplaceAfterRename)
	x.s.displaced++
	x.s.rep.Log(slog.LevelInfo, "displaced an unmanaged file into retention", "path", dest, "retained", x.d.Displaced)
	return x.recordDisplaced(ctx)
}

// recordEarlierDisplace records a file an earlier attempt displaced into retention (S2). A crash
// between that rename and its record leaves it unrecorded, and the resumed item finds the path free
// and never displaces (or records) anything again.
func (x *itemRun) recordEarlierDisplace(ctx context.Context) error {
	if x.d.Displaced == "" {
		return nil
	}
	if _, ok, err := x.lstat(x.d.Displaced); err != nil || !ok {
		return err
	}
	return x.recordDisplaced(ctx)
}

// recordDisplaced records the displaced file as retained (reason displaced).
func (x *itemRun) recordDisplaced(ctx context.Context) error {
	now := x.now()
	return x.write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return x.insertRetained(ctx, tx, x.retainedRow(x.it.RelPath, x.d.Source, x.d.Displaced, x.d.DisplacedSize,
			x.d.DisplacedMtimeNs, "", ReasonDisplaced, now))
	})
}

// --- copy, update, repair (and the copy a link, adoption or move falls back to) ---

func (x *itemRun) putItem(ctx context.Context) error {
	n, err := x.place(ctx)
	if err != nil {
		return err
	}
	return x.done(ctx, n)
}

// place makes the item's destination path hold the source file's current content as its own file
// (S7): an unrecorded file in the way is adopted or displaced; the new version is written to a
// temp file (recorded before it exists), verified, and renamed into place; a recorded old version
// goes to retention first — hardlinked there when the destination supports hardlinks (no gap),
// otherwise renamed (a resumed item finishes the half-done pair from its detail). It returns the
// bytes written by this or an earlier attempt, 0 when the file was adopted.
func (x *itemRun) place(ctx context.Context) (int64, error) {
	dest := x.it.RelPath
	caps, set := x.s.h.Capabilities, x.s.h.Settings

	rec, hasRec, err := x.s.r.store.LiveAt(ctx, x.destID(), dest)
	if err != nil {
		return 0, err
	}
	if hasRec && rec.SourceID != x.d.SourceID {
		return 0, itemErr("%s is recorded for another (or a removed) source; it is left alone", dest)
	}
	// ours: the file at dest (if any) is Bunkarr's recorded version of this path.
	ours := hasRec && (rec.State == StatePresent || rec.State == StateLinked || rec.State == StateMissing)

	// 1. What an earlier attempt left (a replacement it started passed checkDependents).
	tempSt, tempExists, err := x.lstat(x.d.Temp)
	if err != nil {
		return 0, err
	}
	tempComplete := tempExists && x.d.TempDone && tempSt.Regular() && tempSt.Size == x.d.TempSize
	retSt, retExists, err := x.lstat(x.d.Retained)
	if err != nil {
		return 0, err
	}
	fin, finExists, err := x.lstat(dest)
	if err != nil {
		return 0, err
	}
	// linkedOld: dest is the old version an earlier attempt hardlinked into retention.
	linkedOld := false
	if finExists && retExists {
		if linkedOld, err = sameDestFile(ctx, x.s.h, dest, fin, x.d.Retained, retSt); err != nil {
			return 0, err
		}
	}
	switch {
	case ours && retExists && tempComplete && !finExists:
		// The old version was renamed into retention: finish the pair.
		if err := filecopy.Commit(x.dst, x.d.Temp, dest, true); err != nil {
			return 0, err
		}
		faultinject.Point(PointUpdateAfterRenameNew)
		return x.d.TempSize, x.recordPlaced(ctx, true)
	case ours && tempComplete && linkedOld:
		// The old version was hardlinked into retention: finish the replacement.
		if err := filecopy.Commit(x.dst, x.d.Temp, dest, false); err != nil {
			return 0, err
		}
		faultinject.Point(PointUpdateAfterRenameNew)
		return x.d.TempSize, x.recordPlaced(ctx, true)
	case x.d.TempDone && !tempExists && finExists && fin.Regular() && fin.Size == x.d.TempSize &&
		filecopy.MtimeMatch(fin.MtimeNs, x.d.TempMtimeNs, caps.MtimeGranularityNs, 0) &&
		(x.d.Retained == "" || (retExists && !linkedOld)):
		// The temp file was renamed into place; the database write did not happen.
		return x.d.TempSize, x.recordPlaced(ctx, retExists)
	}

	src, err := x.sourceFile()
	if err != nil {
		return 0, err
	}
	if hasRec && rec.State == StatePresent && rec.Size == src.Size && rec.MtimeNs == src.MtimeNs && finExists &&
		fin.Regular() && fin.Size == rec.Size {
		// Already done (a resumed item whose database write happened).
		x.removeTemp()
		x.d.Outcome = outcomeAlreadyDone
		return x.d.TempSize, nil
	}
	// S10b: an update to nothing or to less than half its size is held; the source may have
	// shrunk since the plan passed the guard (or before a resume).
	if x.d.OldSize > 0 && !x.s.job.Params.AllowChanges && shrinks(src.Size, x.d.OldSize) {
		return 0, &heldError{msg: fmt.Sprintf("held: the new version is %d bytes, less than half of the backed-up %d bytes (possible truncation); run a sync with allowChanges to apply it",
			src.Size, x.d.OldSize)}
	}
	if ours {
		if err := x.checkDependents(ctx, rec, src); err != nil {
			return 0, err
		}
	}
	// Start the new version over from the source (a leftover temp file is incomplete or from a
	// state this attempt does not continue).
	x.removeTemp()
	x.d.Temp, x.d.TempDone = "", false

	// 2. An unrecorded file in the way: adopt it or displace it (S2).
	if !ours && finExists {
		ok, hash, err := x.tryAdopt(ctx, dest, fin, src)
		if err != nil {
			return 0, err
		}
		if ok {
			if err := x.recordAdopted(ctx, dest, src, hash); err != nil {
				return 0, err
			}
			x.d.Outcome = outcomeAdopted
			x.s.reclassify(x.it.Action, jobs.ActionAdopt)
			return 0, nil
		}
		if err := x.displace(ctx, dest, fin); err != nil {
			return 0, err
		}
		finExists = false
	}

	// 3. The new version, in a temp file next to dest.
	t, err := filecopy.WriteTemp(ctx, x.src, x.d.Source, x.dst, dest, filecopy.CopyOptions{
		Hash: set.Verify.Mode != destinations.VerifyOff,
		OnTemp: func(tempRel string) error {
			x.d.Temp, x.d.TempDone = tempRel, false
			return x.saveDetail(ctx)
		},
		Progress: func(n int64) {
			x.s.progress.BytesDone += n
			x.s.rep.Progress(x.s.progress)
		},
	})
	if err != nil {
		return 0, err
	}
	if set.Verify.Mode == destinations.VerifyFull {
		if err := filecopy.VerifyTemp(ctx, x.dst, t); err != nil {
			return 0, err
		}
	}
	x.d.TempDone, x.d.TempSize, x.d.TempMtimeNs, x.d.TempHash = true, t.Size, t.MtimeNs, t.Hash
	if err := x.saveDetail(ctx); err != nil {
		return 0, err
	}

	// 4. Keep the old version (S6), then rename the new one into place.
	fin, finExists, err = x.lstat(dest)
	if err != nil {
		return 0, err
	}
	retained := false
	if ours && (finExists || retExists) {
		if finExists && retExists {
			same, err := sameDestFile(ctx, x.s.h, dest, fin, x.d.Retained, retSt)
			if err != nil {
				return 0, err
			}
			if !same {
				// dest is not the version that went to retention: something else is in the way.
				if err := x.displace(ctx, dest, fin); err != nil {
					return 0, err
				}
				finExists = false
			}
		}
		if !retExists {
			if err := x.retainOld(ctx, rec, dest, fin); err != nil {
				return 0, err
			}
			if fin, finExists, err = x.lstat(dest); err != nil {
				return 0, err
			}
		}
		retained = true
	} else if !ours && finExists {
		// Appeared since step 2.
		return 0, itemErr("%s appeared at the destination during the copy; it is left alone", dest)
	}
	if err := filecopy.Commit(x.dst, x.d.Temp, dest, !finExists); err != nil {
		return 0, err
	}
	if retained {
		faultinject.Point(PointUpdateAfterRenameNew)
	}
	return t.Size, x.recordPlaced(ctx, retained)
}

// keepInRetention moves rel, the file of the live record rec (or, with link, hardlinks it), into
// this job's retention directory at x.d.Retained. A new name is chosen and recorded in the detail
// (with the description of the file, reason being deleted or replaced) before the rename, so a
// resumed item finds it; a name taken meanwhile is replaced by the next free one. Before each
// rename the record gets the retention intent (store.go), so the file is found even if this item
// never runs again.
func (x *itemRun) keepInRetention(ctx context.Context, rec Record, rel string, fin filecopy.Stat, link bool, reason string) error {
	for attempt := 0; ; attempt++ {
		if x.d.Retained == "" {
			v, err := x.oldVersion(ctx, rec, rel, fin, reason)
			if err != nil {
				return err
			}
			target, err := x.retentionTarget(rel)
			if err != nil {
				return err
			}
			x.d.Retained = target
			x.d.RetainedSize, x.d.RetainedMtimeNs, x.d.RetainedHash, x.d.RetainedReason = fin.Size, v.mtimeNs, v.hash, v.reason
			if err := x.saveDetail(ctx); err != nil {
				return err
			}
		}
		err := x.write(ctx, func(ctx context.Context, tx *sql.Tx) error {
			return setIntentTx(ctx, tx, x.destID(), rel, x.d.Retained, x.d.RetainedReason)
		})
		if err != nil {
			return err
		}
		if link {
			err = filecopy.Link(x.dst, rel, x.d.Retained)
		} else {
			err = filecopy.Move(x.dst, rel, x.d.Retained, true)
		}
		if errors.Is(err, filecopy.ErrExists) && attempt < 3 {
			x.d.Retained = ""
			continue
		}
		return err
	}
}

// oldVersion describes the recorded version rec found at the destination path rel (fin) before it
// goes to retention for reason (deleted or replaced). A hash is recorded only for content known to
// be the recorded content: a version verify marked missing is damaged (no hash); a suspect one (see
// suspect) is hashed and keeps the recorded hash only when its content has it, else it is damaged;
// otherwise it keeps the recorded hash when it has the recorded size.
func (x *itemRun) oldVersion(ctx context.Context, rec Record, rel string, fin filecopy.Stat, reason string) (retainedVersion, error) {
	v := retainedVersion{mtimeNs: rec.MtimeNs, reason: reason}
	damaged := retainedVersion{mtimeNs: fin.MtimeNs, reason: ReasonDamaged}
	if rec.State == StateMissing {
		return damaged, nil
	}
	suspect, err := x.suspect(ctx, rec, fin)
	if err != nil {
		return v, err
	}
	switch {
	case fin.Size != rec.Size && suspect:
		return damaged, nil
	case fin.Size != rec.Size:
		return v, nil
	case !suspect:
		v.hash = rec.Hash
		return v, nil
	case rec.Hash == "":
		return v, nil // nothing to check the content against: no hash
	}
	got, _, err := filecopy.HashFile(ctx, x.dst, rel, nil)
	if err != nil {
		if err = sideErr(filecopy.SideDestination, "hash", rel, err); ctx.Err() != nil || filecopy.Classify(err) == filecopy.Fatal {
			return v, err
		}
		return damaged, nil // unreadable: not known to be intact
	}
	if got != rec.Hash {
		return damaged, nil
	}
	v.hash = got
	return v, nil
}

// suspect reports whether the content of a recorded version (its file fin) must be checked before
// it is kept with its recorded hash: the item repairs damage verify found (a hardlink of a damaged
// primary may be the damaged inode, whatever is at the primary's path now), or the record is a
// hardlink of a primary verify marked missing, or one whose file is no longer the primary's file
// (the primary's damaged file was replaced by a repair, possibly earlier in this job, and the
// hardlink still holds the damaged inode) or not known to be it (inode numbers that do not
// identify files, see sameDestFile: checking the content against the recorded hash reads one
// file where comparing the two names would read both).
func (x *itemRun) suspect(ctx context.Context, rec Record, fin filecopy.Stat) (bool, error) {
	if x.d.Reason == "repair" {
		return true, nil
	}
	if rec.State != StateLinked || rec.LinkOf == 0 {
		return false, nil
	}
	p, err := x.s.r.store.Get(ctx, rec.LinkOf)
	if errors.Is(err, ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if p.State != StatePresent {
		return true, nil
	}
	if id, err := x.s.h.InodeIdentity(ctx); err != nil {
		return false, err
	} else if !id {
		return true, nil
	}
	pf, ok, err := x.lstat(p.RelPath)
	if err != nil {
		return false, err
	}
	return !ok || !sameFile(pf, fin), nil
}

// retainOld keeps the recorded old version at dest in retention before the new one replaces it:
// hardlinked when the destination supports hardlinks (dest never goes missing), else renamed.
func (x *itemRun) retainOld(ctx context.Context, rec Record, dest string, fin filecopy.Stat) error {
	link := x.s.h.Capabilities.Hardlinks
	if err := x.keepInRetention(ctx, rec, dest, fin, link, ReasonReplaced); err != nil {
		return err
	}
	if link {
		faultinject.Point(PointUpdateAfterLinkOld)
	} else {
		faultinject.Point(PointUpdateAfterRenameOld)
	}
	return nil
}

// recordPlaced records dest as present with the temp file's content (and the retained old
// version).
func (x *itemRun) recordPlaced(ctx context.Context, retained bool) error {
	faultinject.Point(PointRecordAfterFS)
	now := x.now()
	err := x.write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return placedTx(ctx, tx, x.s.h, x.s.job.ID, x.it.RelPath, x.d, retained, now)
	})
	if err != nil {
		return err
	}
	faultinject.Point(PointRecordAfterDB)
	return nil
}

// placedTx records the destination path dest of h as present with the new version the item detail
// d describes (its temp file, renamed into place by job jobID) and, with retained, the old version
// d kept in retention. The item records it after the rename; for an item whose job stopped in
// between, the next job of the destination does (intentFixer.finishPlaced).
func placedTx(ctx context.Context, tx *sql.Tx, h *destinations.Handle, jobID int64, dest string, d Detail, retained bool, now time.Time) error {
	destID := h.Destination.ID
	cur, ok, err := liveAtTx(ctx, tx, destID, dest)
	if err != nil {
		return err
	}
	rec := Record{DestinationID: destID, SourceID: d.SourceID, RelPath: dest, SourceRelPath: d.Source,
		Size: d.TempSize, MtimeNs: d.TempMtimeNs, Hash: d.TempHash, State: StatePresent, JobID: jobID, CopiedAt: &now}
	if ok {
		if cur.SourceID != d.SourceID {
			return itemErr("%s is recorded for another source", dest)
		}
		rec.ID = cur.ID
		if err := releaseDependents(ctx, tx, cur); err != nil {
			return err
		}
		if err := updateRecord(ctx, tx, rec); err != nil {
			return err
		}
	} else if _, err := insertRecord(ctx, tx, rec); err != nil {
		return err
	}
	if !retained || d.Retained == "" {
		return nil
	}
	exp := expiry(h, now)
	old := Record{DestinationID: destID, SourceID: d.SourceID, RelPath: dest, SourceRelPath: d.Source, Size: d.RetainedSize,
		MtimeNs: d.RetainedMtimeNs, Hash: d.RetainedHash, State: StateRetained, RetainedPath: d.Retained, Reason: d.RetainedReason,
		JobID: jobID, RetainedAt: &now, ExpiresAt: &exp}
	if exists, err := retainedExists(ctx, tx, old); err != nil || exists {
		return err
	}
	_, err = insertRecord(ctx, tx, old)
	return err
}

// checkDependents refuses to replace a record's content while a recorded-only link still needs
// that content (its promote failed or was held): the link has no file of its own. A link whose
// own source file changed too (the names were modified together) is relinked after the update and
// does not need the old content. A missing record's content is already gone or damaged: its repair
// restores the content of the links that recorded the content being written (src); a link of
// other content needs its own copy first (the planner plans it before the repair), or it would be
// left recorded against content that is nowhere at the destination.
func (x *itemRun) checkDependents(ctx context.Context, rec Record, src filecopy.Stat) error {
	deps, err := x.s.r.store.Dependents(ctx, rec.ID)
	if err != nil {
		return err
	}
	for _, d := range deps {
		if d.State != StateLinkRecorded {
			continue
		}
		if rec.State == StateMissing {
			if d.Size == src.Size && filecopy.MtimeMatch(d.MtimeNs, src.MtimeNs, x.s.h.Capabilities.MtimeGranularityNs, 0) {
				continue
			}
		} else if d.Size != rec.Size || d.MtimeNs != rec.MtimeNs {
			continue
		}
		needs := true
		if d.SourceID == x.d.SourceID {
			st, ok, err := x.sourceStat(d.SourceRelPath)
			if err != nil {
				return err
			}
			needs = ok && st.Size == d.Size && st.MtimeNs == d.MtimeNs
		}
		switch {
		case !needs:
		case rec.State == StateMissing:
			return itemErr("the hardlinked name %s recorded other content than %s gets now and has no copy of its own yet (not repaired; it is retried by the next sync)", d.RelPath, rec.RelPath)
		default:
			return itemErr("%s still holds the content of the hardlinked name %s (not replaced; it is retried by the next sync)", rec.RelPath, d.RelPath)
		}
	}
	return nil
}

// releaseDependents runs when a record's content is replaced: hardlinks made at the destination
// keep the old content in their own name, so they become present; recorded-only links of other
// content (names being relinked) keep pointing here. Hardlinks of a damaged (missing) primary
// share its damaged inode: they stay links and are relinked by the plan.
func releaseDependents(ctx context.Context, tx *sql.Tx, cur Record) error {
	if cur.State == StateMissing {
		return nil
	}
	deps, err := liveDependentsTx(ctx, tx, cur.ID)
	if err != nil {
		return err
	}
	for _, d := range deps {
		if d.State == StateLinked && d.Size == cur.Size && d.MtimeNs == cur.MtimeNs {
			d.State, d.LinkOf = StatePresent, 0
			if err := updateRecord(ctx, tx, d); err != nil {
				return err
			}
		}
	}
	return nil
}

// --- adopt ---

func (x *itemRun) adopt(ctx context.Context) error {
	if x.d.Temp != "" {
		return x.fallbackPlace(ctx) // an earlier attempt fell back to a copy: finish it
	}
	dest := x.it.RelPath
	rec, hasRec, err := x.s.r.store.LiveAt(ctx, x.destID(), dest)
	if err != nil {
		return err
	}
	src, err := x.sourceFile()
	if err != nil {
		return err
	}
	if hasRec {
		if rec.SourceID == x.d.SourceID && rec.State == StatePresent && rec.Size == src.Size && rec.MtimeNs == src.MtimeNs {
			x.d.Outcome = outcomeAlreadyDone
			return x.done(ctx, 0)
		}
		return x.fallbackPlace(ctx)
	}
	fin, finExists, err := x.lstat(dest)
	if err != nil {
		return err
	}
	if finExists {
		ok, hash, err := x.tryAdopt(ctx, dest, fin, src)
		if err != nil {
			return err
		}
		if ok {
			if err := x.recordAdopted(ctx, dest, src, hash); err != nil {
				return err
			}
			return x.done(ctx, 0)
		}
	}
	return x.fallbackPlace(ctx)
}

// fallbackPlace copies the file instead of the planned cheaper action.
func (x *itemRun) fallbackPlace(ctx context.Context) error {
	n, err := x.place(ctx)
	if err != nil {
		return err
	}
	if x.d.Outcome == "" {
		x.d.Outcome = outcomeFallbackCopy
		x.s.reclassify(x.it.Action, jobs.ActionCopy)
	}
	return x.done(ctx, n)
}

// --- move ---

func (x *itemRun) move(ctx context.Context) error {
	if x.d.Temp != "" {
		return x.fallbackPlace(ctx) // an earlier attempt fell back to a copy: finish it
	}
	to, from := x.it.RelPath, x.d.From
	v, err := x.s.r.store.Get(ctx, x.d.RecordID)
	if errors.Is(err, ErrNotFound) {
		return x.fallbackPlace(ctx)
	}
	if err != nil {
		return err
	}
	if v.State.Live() && v.RelPath == to && v.SourceRelPath == x.d.Source {
		x.d.Outcome = outcomeAlreadyDone
		return x.done(ctx, 0)
	}
	if !v.State.Live() || v.SourceID != x.d.SourceID || v.RelPath != from || (v.State != StatePresent && v.State != StateLinked) {
		return x.fallbackPlace(ctx)
	}
	fromSt, fromExists, err := x.lstat(from)
	if err != nil {
		return err
	}
	toSt, toExists, err := x.lstat(to)
	if err != nil {
		return err
	}
	// Renamed by an earlier attempt: record that whatever the source shows now (the next sync
	// updates, retains or copies back what changed there since).
	renamed := !fromExists && toExists && toSt.Regular() && toSt.Size == v.Size
	if !renamed {
		src, err := x.sourceFile()
		if err != nil {
			return err
		}
		if src.Size != x.d.Size || src.MtimeNs != x.d.MtimeNs || src.Size != v.Size {
			return x.fallbackPlace(ctx) // changed since planning: not the same content
		}
		if back, err := x.reappeared(ctx, v.SourceRelPath); err != nil {
			return err
		} else if back {
			return x.fallbackPlace(ctx) // the old name reappeared: keep it
		}
	}
	caseOnly := false
	if !renamed && fromExists && toExists && x.s.h.Capabilities.CaseInsensitive && filecopy.FoldKey(from) == filecopy.FoldKey(to) {
		if caseOnly, err = caseVariant(ctx, x.s.h, from, fromSt, to, toSt); err != nil {
			return err
		}
	}
	switch {
	case renamed:
	case caseOnly:
		// A case-only rename on a case-insensitive destination: the same file under both names.
		if err := filecopy.Move(x.dst, from, to, false); err != nil {
			return err
		}
	case fromExists:
		if !fromSt.Regular() || fromSt.Size != v.Size {
			return x.fallbackPlace(ctx)
		}
		if toExists {
			if _, recorded, err := x.s.r.store.LiveAt(ctx, x.destID(), to); err != nil {
				return err
			} else if recorded {
				return itemErr("%s is already recorded at the destination", to)
			}
			if err := x.displace(ctx, to, toSt); err != nil {
				return err
			}
		}
		if err := filecopy.Move(x.dst, from, to, true); err != nil {
			return err
		}
	default:
		return x.fallbackPlace(ctx) // the recorded file is gone
	}
	faultinject.Point(PointRecordAfterFS)
	err = x.write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		cur, ok, err := getTx(ctx, tx, v.ID)
		if err != nil {
			return err
		}
		if !ok {
			return itemErr("the record of %s vanished", from)
		}
		cur.RelPath, cur.SourceRelPath, cur.JobID = to, x.d.Source, x.s.job.ID
		if err := updateRecord(ctx, tx, cur); err != nil {
			if isUniqueViolation(err) {
				return itemErr("%s is already recorded at the destination", to)
			}
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	faultinject.Point(PointRecordAfterDB)
	x.pruneLive(from)
	return x.done(ctx, 0)
}

// --- link ---

func (x *itemRun) link(ctx context.Context) error {
	if x.d.Temp != "" {
		return x.fallbackPlace(ctx) // an earlier attempt fell back to a copy: finish it
	}
	dest := x.it.RelPath
	src, err := x.sourceFile()
	if err != nil {
		return err
	}
	mode := StateLinkRecorded
	if x.s.linkMode {
		mode = StateLinked
	}
	rec, hasRec, err := x.s.r.store.LiveAt(ctx, x.destID(), dest)
	if err != nil {
		return err
	}
	if hasRec && rec.SourceID != x.d.SourceID {
		return itemErr("%s is recorded for another (or a removed) source; it is left alone", dest)
	}

	// The primary must be recorded present with this name's content, and both source names must
	// still be the same inode (design §4.3); otherwise this name gets its own copy.
	prim, primOK, err := x.s.r.store.liveForSourcePath(ctx, x.destID(), x.d.SourceID, x.d.Primary)
	if err != nil {
		return err
	}
	valid := primOK && prim.State == StatePresent && prim.Size == src.Size && prim.MtimeNs == src.MtimeNs && (!hasRec || rec.ID != prim.ID)
	var primDest filecopy.Stat
	if valid {
		ps, ok, err := x.sourceStat(x.d.Primary)
		if err != nil {
			return err
		}
		valid = ok && sameFile(ps, src) && ps.Size == src.Size && ps.MtimeNs == src.MtimeNs
	}
	if valid {
		pd, ok, err := x.lstat(prim.RelPath)
		if err != nil {
			return err
		}
		valid = ok && pd.Regular() && pd.Size == prim.Size
		primDest = pd
	}
	if !valid {
		x.s.rep.Log(slog.LevelInfo, "hardlink not possible: copying the name instead", "path", dest, "primary", x.d.Primary)
		return x.fallbackPlace(ctx)
	}
	// isLink reports whether the file at dest (stat st) is the primary's hardlink. A name the live
	// record already has as this primary's hardlink, written by THIS job (an earlier attempt of
	// this item recorded it and died before the item was finished), is compared by content where
	// inode numbers do not identify files (sameDestFile): its link count can show 1 (attributes a
	// directory listing primed), which says nothing about a hardlink the job made and recorded
	// itself. Any other name — including one an earlier job recorded, which the planner relinks
	// deliberately, e.g. after its primary was repaired — is not taken for a hardlink on its
	// content alone (hardlinkOf).
	recorded := hasRec && rec.State == mode && rec.LinkOf == prim.ID && rec.JobID == x.s.job.ID
	isLink := func(st filecopy.Stat) (bool, error) {
		if recorded {
			return sameDestFile(ctx, x.s.h, dest, st, prim.RelPath, primDest)
		}
		return hardlinkOf(ctx, x.s.h, dest, st, prim.RelPath, primDest)
	}
	if recorded && rec.Size == src.Size && rec.MtimeNs == src.MtimeNs {
		if mode == StateLinkRecorded {
			x.d.Outcome = outcomeAlreadyDone
			return x.done(ctx, 0)
		}
		fin, ok, err := x.lstat(dest)
		if err != nil {
			return err
		}
		if ok {
			if linked, err := isLink(fin); err != nil {
				return err
			} else if linked {
				x.d.Outcome = outcomeAlreadyDone
				return x.done(ctx, 0)
			}
		}
	}
	ours := hasRec && (rec.State == StatePresent || rec.State == StateLinked || rec.State == StateMissing)
	if ours {
		if err := x.checkDependents(ctx, rec, src); err != nil {
			return err
		}
	}

	// What is at dest now: already the hardlink, the name's own old version (to retention), or an
	// unmanaged file (adopted as this name's own copy, or displaced).
	fin, finExists, err := x.lstat(dest)
	if err != nil {
		return err
	}
	retained := false
	if x.d.Retained != "" {
		if _, ok, err := x.lstat(x.d.Retained); err != nil {
			return err
		} else if ok {
			retained = true // moved by an earlier attempt
		}
	}
	linked := false // dest is already the hardlink (an earlier attempt made it)
	if finExists && mode == StateLinked {
		if linked, err = isLink(fin); err != nil {
			return err
		}
	}
	if finExists && !linked {
		switch {
		case ours && !retained:
			if err := x.retainName(ctx, rec, dest, fin); err != nil {
				return err
			}
			retained = true
		default:
			ok, hash, err := x.tryAdopt(ctx, dest, fin, src)
			if err != nil {
				return err
			}
			if ok {
				if err := x.recordAdopted(ctx, dest, src, hash); err != nil {
					return err
				}
				x.d.Outcome = outcomeAdopted
				x.s.reclassify(x.it.Action, jobs.ActionAdopt)
				return x.done(ctx, 0)
			}
			if err := x.displace(ctx, dest, fin); err != nil {
				return err
			}
		}
		finExists = false
	}
	if mode == StateLinked && !finExists {
		if err := filecopy.Link(x.dst, prim.RelPath, dest); err != nil {
			if !errors.Is(err, filecopy.ErrExists) {
				return err
			}
			cur, ok, serr := x.lstat(dest)
			if serr != nil || !ok {
				return err
			}
			if same, serr := hardlinkOf(ctx, x.s.h, dest, cur, prim.RelPath, primDest); serr != nil {
				return serr
			} else if !same {
				return err
			}
		}
	}
	faultinject.Point(PointRecordAfterFS)
	now := x.now()
	err = x.write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		p, ok, err := getTx(ctx, tx, prim.ID)
		if err != nil {
			return err
		}
		if !ok || p.State != StatePresent {
			return itemErr("the primary %s changed during the link", prim.RelPath)
		}
		nr := Record{DestinationID: x.destID(), SourceID: x.d.SourceID, RelPath: dest, SourceRelPath: x.d.Source, Size: src.Size,
			MtimeNs: src.MtimeNs, Hash: p.Hash, LinkOf: p.ID, State: mode, JobID: x.s.job.ID, CopiedAt: &now}
		cur, ok, err := liveAtTx(ctx, tx, x.destID(), dest)
		if err != nil {
			return err
		}
		if ok {
			nr.ID = cur.ID
			if err := repoint(ctx, tx, cur.ID, p.ID); err != nil {
				return err
			}
			if err := updateRecord(ctx, tx, nr); err != nil {
				return err
			}
		} else if _, err := insertRecord(ctx, tx, nr); err != nil {
			return err
		}
		if retained {
			return x.insertRetained(ctx, tx, x.retainedRow(dest, x.d.Source, x.d.Retained, x.d.RetainedSize, x.d.RetainedMtimeNs,
				x.d.RetainedHash, x.d.RetainedReason, now))
		}
		return nil
	})
	if err != nil {
		return err
	}
	faultinject.Point(PointRecordAfterDB)
	if mode == StateLinkRecorded {
		x.d.Outcome = outcomeLinkRecorded
	}
	return x.done(ctx, 0)
}

// reappeared reports whether a vanished source path is back: a regular file is there again and the
// catalog lists it (a scan saw it after the plan). A path that is still on disk but left the
// catalog (newly excluded, now a symlink, renamed in case only on a case-insensitive source) did
// not come back; a file restored without a scan since is retained and copied again by the next
// sync.
func (x *itemRun) reappeared(ctx context.Context, rel string) (bool, error) {
	st, ok, err := x.sourceStat(rel)
	if err != nil || !ok || !st.Regular() {
		return false, err
	}
	return x.s.inCatalog(ctx, x.d.SourceID, rel)
}

// retainName moves a name's own recorded old version into retention (a relink).
func (x *itemRun) retainName(ctx context.Context, rec Record, dest string, fin filecopy.Stat) error {
	if !fin.Regular() {
		return itemErr("%s is not a regular file; it is left alone", dest)
	}
	if err := x.keepInRetention(ctx, rec, dest, fin, false, ReasonReplaced); err != nil {
		return err
	}
	faultinject.Point(PointRetainAfterRename)
	return nil
}

// --- promote ---

// promote makes a dependent name hold a primary's content before the primary is retained or
// updated (design §4.2): with a recorded-only link the primary's file is renamed to the
// dependent's path; a hardlink already holds the content. The other dependents are re-pointed.
func (x *itemRun) promote(ctx context.Context) error {
	t, err := x.s.r.store.Get(ctx, x.d.TargetID)
	if errors.Is(err, ErrNotFound) {
		return itemErr("the hardlinked name %s is no longer recorded", x.it.RelPath)
	}
	if err != nil {
		return err
	}
	if t.State == StatePresent && t.LinkOf == 0 {
		x.d.Outcome = outcomeAlreadyDone // the promote's database write happened
		return x.done(ctx, 0)
	}
	v, err := x.s.r.store.Get(ctx, x.d.RecordID)
	if errors.Is(err, ErrNotFound) {
		return itemErr("the record of %s vanished", x.d.From)
	}
	if err != nil {
		return err
	}
	if !v.State.Live() || !t.State.Live() || t.LinkOf != v.ID || (t.State != StateLinked && t.State != StateLinkRecorded) {
		return itemErr("the hardlink records of %s changed since planning", x.d.From)
	}
	var vs, tf filecopy.Stat
	var vExists, tExists, renamed bool
	if t.State == StateLinkRecorded {
		if vs, vExists, err = x.lstat(v.RelPath); err != nil {
			return err
		}
		if tf, tExists, err = x.lstat(t.RelPath); err != nil {
			return err
		}
		// Renamed by an earlier attempt: record that whatever the source shows now (the next sync
		// updates the name if it changed since).
		renamed = !vExists && tExists && tf.Regular() && tf.Size == v.Size
	}
	if !renamed {
		ts, ok, err := x.sourceStat(t.SourceRelPath)
		if err != nil {
			return err
		}
		if !ok || ts.Size != t.Size || ts.MtimeNs != t.MtimeNs {
			return itemErr("%s changed at the source since planning", t.SourceRelPath)
		}
		if x.d.For == "update" && !x.s.job.Params.AllowChanges {
			// The update this promote prepares is held when the primary's source shrank since
			// planning (S10b): so is the promote, or the primary would be left without its file.
			if ps, ok, err := x.sourceStat(v.SourceRelPath); err != nil {
				return err
			} else if ok && shrinks(ps.Size, v.Size) {
				return &heldError{msg: fmt.Sprintf("held with the update of %s: its new version is %d bytes, less than half of the backed-up %d bytes (possible truncation); run a sync with allowChanges to apply it",
					v.RelPath, ps.Size, v.Size)}
			}
		}
	}
	moved := false
	if t.State == StateLinkRecorded {
		switch {
		case renamed:
		case vExists && vs.Regular() && vs.Size == v.Size:
			if tExists {
				if err := x.displace(ctx, t.RelPath, tf); err != nil {
					return err
				}
			}
			if err := filecopy.Move(x.dst, v.RelPath, t.RelPath, true); err != nil {
				return err
			}
			faultinject.Point(PointPromoteAfterRename)
		default:
			return itemErr("%s is not at the destination (or not the recorded size); %s keeps its link", v.RelPath, t.RelPath)
		}
		moved = true
	} else {
		tf, ok, err := x.lstat(t.RelPath)
		if err != nil {
			return err
		}
		if !ok || !tf.Regular() || tf.Size != v.Size {
			return itemErr("the hardlink %s is not at the destination (or not the recorded size)", t.RelPath)
		}
	}
	faultinject.Point(PointRecordAfterFS)
	now := x.now()
	err = x.write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		t.State, t.LinkOf, t.Hash, t.JobID = StatePresent, 0, v.Hash, x.s.job.ID
		if moved {
			t.CopiedAt = &now
		}
		if err := updateRecord(ctx, tx, t); err != nil {
			return err
		}
		if err := repoint(ctx, tx, v.ID, t.ID); err != nil {
			return err
		}
		if !moved {
			return nil
		}
		if x.d.For == "retain" {
			return deleteRecord(ctx, tx, v.ID) // its content lives on under t
		}
		v.State = StateMissing // its file is t's now; the update writes the new version
		return updateRecord(ctx, tx, v)
	})
	if err != nil {
		return err
	}
	faultinject.Point(PointRecordAfterDB)
	if moved && x.d.For == "retain" {
		x.pruneLive(v.RelPath)
	}
	return x.done(ctx, 0)
}

// --- retain ---

// retain moves a vanished name into retention (S5). A recorded-only link has no file: its record
// is removed. Remaining hardlinks of the name are re-pointed to one of them (it holds the same
// inode); recorded-only links whose own name vanished too are removed with it (their retain
// finds nothing left to do); other recorded-only links block the retain (their content would go).
func (x *itemRun) retain(ctx context.Context) error {
	v, err := x.s.r.store.Get(ctx, x.d.RecordID)
	if errors.Is(err, ErrNotFound) {
		x.d.Outcome = outcomeRecordRemoved
		return x.done(ctx, 0)
	}
	if err != nil {
		return err
	}
	if v.State == StateRetained && (x.d.Retained == "" || v.RetainedPath == x.d.Retained) {
		x.d.Outcome = outcomeAlreadyDone
		return x.done(ctx, v.Size)
	}
	if !v.State.Live() || v.SourceID != x.d.SourceID || v.SourceRelPath != x.d.Source {
		return x.finish(ctx, jobs.ItemSkipped, 0, "the record changed since planning")
	}
	fin, finExists, err := x.lstat(v.RelPath)
	if err != nil {
		return err
	}
	// Moved into retention by an earlier attempt: finish that whatever the source shows now (a
	// file that came back is copied again by the next sync).
	retained := false
	if x.d.Retained != "" && !finExists {
		if _, ok, err := x.lstat(x.d.Retained); err != nil {
			return err
		} else if ok {
			retained = true
		}
	}
	if !retained {
		if back, err := x.reappeared(ctx, v.SourceRelPath); err != nil {
			return err
		} else if back {
			return x.finish(ctx, jobs.ItemSkipped, 0, "it reappeared at the source; not retained")
		}
	}
	deps, err := x.s.r.store.Dependents(ctx, v.ID)
	if err != nil {
		return err
	}
	var heir *Record
	var gone []int64 // recorded-only links whose own name vanished too
	for i := range deps {
		d := &deps[i]
		switch {
		case d.State == StateLinkRecorded && d.SourceID == x.d.SourceID:
			back, err := x.reappeared(ctx, d.SourceRelPath)
			if err != nil {
				return err
			}
			if !back {
				gone = append(gone, d.ID)
			}
		case d.State == StateLinked && heir == nil:
			st, ok, err := x.lstat(d.RelPath)
			if err != nil {
				return err
			}
			if ok && st.Regular() && st.Size == v.Size {
				heir = d
			}
		}
	}
	if n := len(deps) - len(gone); n > 0 && heir == nil {
		return itemErr("%s names still depend on the content of %s; it is not retained", plural(int64(n), "hardlinked", "hardlinked"), v.RelPath)
	}
	if v.State == StateLinkRecorded {
		faultinject.Point(PointRecordAfterFS)
		if err := x.write(ctx, func(ctx context.Context, tx *sql.Tx) error { return deleteRecord(ctx, tx, v.ID) }); err != nil {
			return err
		}
		faultinject.Point(PointRecordAfterDB)
		x.d.Outcome = outcomeRecordRemoved
		return x.done(ctx, 0)
	}

	if !retained && finExists {
		if !fin.Regular() {
			return itemErr("%s is not a regular file; it is left alone", v.RelPath)
		}
		// S6: the new version of a vanished name (a Radarr upgrade renames X-1080p.mkv to
		// X-2160p.mkv) must be backed up before the old one goes to retention.
		if name, err := x.s.notBackedUp(ctx, x.d.SourceID, v.SourceRelPath); err != nil {
			return err
		} else if name != "" {
			return itemErr("not retained yet: %s in the same folder is not backed up (its copy failed or was held); %s stays until it is",
				name, v.RelPath)
		}
		if err := x.keepInRetention(ctx, v, v.RelPath, fin, false, ReasonDeleted); err != nil {
			return err
		}
		faultinject.Point(PointRetainAfterRename)
		retained = true
	}
	faultinject.Point(PointRecordAfterFS)
	now := x.now()
	err = x.write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		for _, id := range gone {
			if err := deleteRecord(ctx, tx, id); err != nil {
				return err
			}
		}
		if heir != nil {
			h, ok, err := getTx(ctx, tx, heir.ID)
			if err != nil {
				return err
			}
			if ok {
				h.State, h.LinkOf = StatePresent, 0
				if err := updateRecord(ctx, tx, h); err != nil {
					return err
				}
				if err := repoint(ctx, tx, v.ID, h.ID); err != nil {
					return err
				}
			}
		}
		if !retained {
			// Nothing at the destination (verify marked it missing, or it was removed by hand).
			return deleteRecord(ctx, tx, v.ID)
		}
		reason := x.d.RetainedReason
		if reason == "" {
			reason = ReasonDeleted
		}
		exp := x.expiresAt(now)
		row := x.retainedRow(v.RelPath, v.SourceRelPath, x.d.Retained, x.d.RetainedSize, x.d.RetainedMtimeNs, x.d.RetainedHash, reason, now)
		if done, err := retainedExists(ctx, tx, row); err != nil {
			return err
		} else if done {
			// A stopped attempt's retention was settled by recording the file where it is.
			return deleteRecord(ctx, tx, v.ID)
		}
		v.State, v.RetainedPath, v.Reason, v.LinkOf, v.JobID = StateRetained, x.d.Retained, reason, 0, x.s.job.ID
		v.Size, v.Hash, v.RetainedAt, v.ExpiresAt = x.d.RetainedSize, x.d.RetainedHash, &now, &exp
		if x.d.RetainedMtimeNs != 0 {
			v.MtimeNs = x.d.RetainedMtimeNs
		}
		return updateRecord(ctx, tx, v)
	})
	if err != nil {
		return err
	}
	faultinject.Point(PointRecordAfterDB)
	x.pruneLive(v.RelPath)
	if !retained {
		x.d.Outcome = outcomeRecordRemoved
		if v.State != StateMissing {
			x.s.rep.Log(slog.LevelWarn, "a vanished file was not at the destination either; its record was removed", "path", v.RelPath)
		}
		return x.done(ctx, 0)
	}
	return x.done(ctx, x.d.RetainedSize)
}
