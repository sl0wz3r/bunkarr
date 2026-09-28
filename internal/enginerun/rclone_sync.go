package enginerun

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/engines/rclone"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/syncer"
)

// The rclone sync (§7.2, §7.3): the reconciliation of interrupted moves (rclone_reconcile.go), the
// shared plan, then the Phase 1 order: promote, move, copy/update in batches (rclone_copy.go),
// link, retain and release (rclone_retain.go), and the finish (links.tsv, SFTP rmdirs). Every
// target of a moveto or move is listed first and an occupied one displaced into retention (S23);
// every outcome comes from a listing (statMany), never from rclone's log.

// rcloneRun is one rclone job attempt.
type rcloneRun struct {
	*jobRun
	conn   *rclone.Conn
	caps   filecopy.Capabilities
	retDir string

	plan    *syncer.Plan
	roots   map[int64]*os.Root
	sources map[int64]catalog.Source

	// batches counts the transfer batches of this attempt (the marker re-check every 20, §7.3).
	batches int
	// started: a transfer of this attempt started (an oversize file with allowOverrun then waits
	// for the next opening, §9.2).
	started bool
	// uploaded is the bytes rclone reported transferred by this attempt.
	uploaded int64
	// touched are the destination folders a move or retain changed (SFTP rmdirs).
	touched map[string]bool
}

// rcloneSync runs a sync of an rclone destination.
func (s *Service) rcloneSync(ctx context.Context, r *jobRun) (jobs.Result, error) {
	conn, err := s.o.Rclone.Connect(r.ed, r.sec, r.rt)
	if err != nil {
		return jobs.Result{}, fmt.Errorf("sync: %w", err)
	}
	if _, err := conn.CheckMarker(ctx); err != nil {
		return jobs.Result{}, fmt.Errorf("sync: %w", err)
	}
	x := &rcloneRun{jobRun: r, conn: conn, caps: rclone.Capabilities(r.ed), retDir: filecopy.RetentionDir(r.job.QueuedAt, r.job.ID),
		touched: map[string]bool{}}
	r.log(slog.LevelInfo, "sync started", "destination", r.d.Name, "engine", "rclone", "attempt", r.job.Attempt, "dryRun", r.job.DryRun,
		"allowChanges", r.job.Params.AllowChanges)
	if !r.job.DryRun {
		if err := x.reconcile(ctx); err != nil {
			return jobs.Result{}, fmt.Errorf("sync: %w", err)
		}
	}
	plan, err := s.planner.Plan(ctx, syncer.PlanInput{Job: r.job, Env: jobs.Env{Reporter: r.rep, Items: r.env.Items},
		Dest: syncer.PlanDestination{ID: r.d.ID, Name: r.d.Name, SourceIDs: r.d.SourceIDs, Settings: r.d.Settings, Caps: x.caps},
		FS:   recordPlanFS{ctx: ctx, files: s.files, dest: r.d.ID}})
	if err != nil {
		return jobs.Result{}, err
	}
	x.plan = plan
	if r.job.DryRun {
		return x.result(ctx)
	}
	roots, closeRoots, err := sourceRoots(plan)
	if err != nil {
		return jobs.Result{}, err
	}
	defer closeRoots()
	x.roots = roots
	x.sources = map[int64]catalog.Source{}
	for _, src := range plan.Sources() {
		x.sources[src.ID] = src
	}
	err = x.execute(ctx)
	plan.FlagsAfterSync(ctx)
	if err != nil {
		return jobs.Result{}, err
	}
	x.finishSync(ctx)
	return x.result(ctx)
}

func (x *rcloneRun) result(ctx context.Context) (jobs.Result, error) {
	st, warnings, err := x.syncStats(ctx, x.plan)
	if err != nil {
		return jobs.Result{}, err
	}
	st.BytesUploaded = x.uploaded
	st.Batches = x.batches
	return jobs.Result{Stats: st, Warnings: warnings, Summary: rcloneSyncSummary(st, x.d.Name)}, nil
}

// execute runs the pending items in the Phase 1 order (S6: new before old).
func (x *rcloneRun) execute(ctx context.Context) error {
	items, err := x.pending(ctx)
	if err != nil {
		return err
	}
	var promotes, moves, puts, links, retains []jobs.Item
	var total, bytes int64
	for _, it := range items {
		switch it.Action {
		case jobs.ActionPromote:
			promotes = append(promotes, it)
		case jobs.ActionMove:
			moves = append(moves, it)
		case jobs.ActionCopy, jobs.ActionUpdate, jobs.ActionAdopt:
			puts = append(puts, it)
			bytes += it.Bytes
		case jobs.ActionLink:
			links = append(links, it)
		case jobs.ActionRetain:
			retains = append(retains, it)
		default:
			continue
		}
		total++
	}
	x.report(func(p *jobs.Progress) {
		p.Phase, p.FilesTotal, p.BytesTotal = "moving", total, bytes
	})
	for _, it := range promotes {
		if err := x.step(ctx, it, x.promote); err != nil {
			return err
		}
	}
	for _, it := range moves {
		if err := x.step(ctx, it, x.move); err != nil {
			return err
		}
	}
	x.report(func(p *jobs.Progress) { p.Phase = "uploading" })
	// The files that waited for a transfer window run first (loadWaits).
	puts = x.loadWaits(ctx, puts, func(jobs.Item) bool { return true }, func(id int64) bool { _, ok := x.sources[id]; return ok })
	defer x.flushWaits(ctx)
	if err := x.copyItems(ctx, puts); err != nil {
		return err
	}
	for _, it := range links {
		if err := x.step(ctx, it, x.link); err != nil {
			return err
		}
	}
	if len(retains) > 0 {
		// The identity is checked again before retention work (S25).
		if _, err := x.conn.CheckMarker(ctx); err != nil {
			return err
		}
		x.report(func(p *jobs.Progress) { p.Phase = "retaining" })
		if err := x.retainItems(ctx, retains); err != nil {
			return err
		}
	}
	return nil
}

// itemErr is a per-file problem: it fails the item, never the job.
type itemErr struct{ msg string }

func (e *itemErr) Error() string { return e.msg }

func itemErrorf(format string, args ...any) error { return &itemErr{msg: fmt.Sprintf(format, args...)} }

// step runs one item inside the window: after the window's end nothing starts (the job defers).
// An item error fails the item; any other error stops the job.
func (x *rcloneRun) step(ctx context.Context, it jobs.Item, fn func(context.Context, jobs.Item, *engineDetail) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if now := x.s.now(); !x.open(now) {
		return x.deferral(now)
	}
	d, err := parseItem(it)
	if err != nil {
		return x.failItem(ctx, it, err.Error())
	}
	if d.SourceID != 0 && x.roots[d.SourceID] == nil {
		// The source was unlinked, disabled or deleted since planning: its records are orphans
		// and are never changed.
		x.warnings++
		return x.finish(ctx, it.ID, jobs.ItemSkipped, 0, fmt.Sprintf("source %d is no longer synced to this destination", d.SourceID))
	}
	x.report(func(p *jobs.Progress) { p.CurrentFile = it.RelPath })
	err = fn(ctx, it, &d)
	x.report(func(p *jobs.Progress) { p.FilesDone++ })
	var ie *itemErr
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ie):
		return x.failItem(ctx, it, ie.msg)
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, rclone.ErrFence):
		// A name rclone cannot be given (a line break, a control character on the command line;
		// the catalog allows them): nothing ran, and only this item fails.
		return x.failItem(ctx, it, unsupportedName(err))
	}
	return err
}

// listableName reports whether rclone can be given rel in a file list (no line break, no final
// "\r"); the item of a name it cannot be given fails instead of the job.
func listableName(rel string) error {
	if err := rclone.CheckFileListName(rel); err != nil {
		return itemErrorf("%s", unsupportedName(err))
	}
	return nil
}

// unsupportedName is the item error of a name rclone refuses.
func unsupportedName(err error) string {
	return "the name cannot be passed to rclone: " + strings.TrimPrefix(err.Error(), rclone.ErrFence.Error()+": ")
}

// done finishes an item as done, recording an outcome that differs from its action.
func (x *rcloneRun) done(ctx context.Context, it jobs.Item, d *engineDetail, bytes int64, outcome string) error {
	if outcome != "" {
		d.Outcome = outcome
		if err := x.setDetail(ctx, it.ID, *d); err != nil {
			return err
		}
	}
	return x.finish(ctx, it.ID, jobs.ItemDone, bytes, "")
}

func (x *rcloneRun) write(ctx context.Context, fn func(tx *sql.Tx) error) error {
	return x.s.o.DB.Write(context.WithoutCancel(ctx), fn)
}

// statMany lists which of rels (root-relative) exist as objects.
func (x *rcloneRun) statMany(ctx context.Context, rels ...string) (map[string]rclone.Object, error) {
	var list []string
	seen := map[string]bool{}
	for _, r := range rels {
		if r != "" && !seen[r] {
			seen[r] = true
			list = append(list, r)
		}
	}
	return x.conn.StatMany(context.WithoutCancel(ctx), "", list)
}

// moveTo runs one server-side moveto (--max-delete 1); an object error fails the item.
func (x *rcloneRun) moveTo(ctx context.Context, from, to string) error {
	res, err := x.conn.MoveTo(ctx, from, to)
	faultinject.Point(PointRcloneAfterMove)
	if err != nil {
		return err
	}
	if msg := res.ObjectErrors[path.Base(from)]; msg != "" {
		return itemErrorf("moving %s: %s", from, msg)
	}
	for _, msg := range res.ObjectErrors {
		return itemErrorf("moving %s: %s", from, msg)
	}
	return nil
}

// freeRetention returns a free name for rel in this job's retention directory: the plain one, or
// a numbered one ("name.1.ext") when it is taken, as filecopy.RetentionTarget chooses (§7.3).
func (x *rcloneRun) freeRetention(ctx context.Context, rel string) (string, error) {
	base := x.retDir + "/" + rel
	for start := 0; start <= 9999; start += 20 {
		var cands []string
		for n := start; n < start+20; n++ {
			cands = append(cands, numbered(base, n))
		}
		found, err := x.statMany(ctx, cands...)
		if err != nil {
			return "", err
		}
		for _, c := range cands {
			if _, taken := found[c]; !taken {
				return c, nil
			}
		}
	}
	return "", itemErrorf("no free retention name for %s", rel)
}

// numbered returns p with ".<n>" inserted before its extension (p itself for n == 0).
func numbered(p string, n int) string {
	if n == 0 {
		return p
	}
	dir, base := path.Split(p)
	ext := path.Ext(base)
	if ext == base {
		ext = ""
	}
	return dir + strings.TrimSuffix(base, ext) + "." + strconv.Itoa(n) + ext
}

// displace moves an unmanaged object at the live path rel into retention and records it
// (reason displaced, S2, S23): the retention name is recorded in the item's detail first (the
// intent of an object without a record), then moveto --max-delete 1, a listing of both paths, and
// the retained row.
func (x *rcloneRun) displace(ctx context.Context, it jobs.Item, d *engineDetail, rel string, obj rclone.Object) error {
	dst, err := x.freeRetention(ctx, rel)
	if err != nil {
		return err
	}
	d.Displaced, d.DisplacedSize, d.DisplacedMtimeNs = dst, obj.Size, obj.MtimeNs()
	if err := x.setDetail(ctx, it.ID, *d); err != nil {
		return err
	}
	faultinject.Point(PointRcloneAfterIntent)
	if err := x.moveTo(ctx, rel, dst); err != nil {
		return err
	}
	found, err := x.statMany(ctx, rel, dst)
	if err != nil {
		return err
	}
	faultinject.Point(PointRcloneAfterStat)
	if err := x.recordDisplaced(ctx, it, d, rel, found); err != nil {
		return err
	}
	if _, still := found[rel]; still {
		return itemErrorf("%s could not be moved out of the way", rel)
	}
	x.log(slog.LevelInfo, "displaced an unmanaged object into retention", "path", rel, "retained", dst)
	return nil
}

// recordDisplaced records the object a displacement moved (found lists the retention name), and
// clears the detail's intent.
func (x *rcloneRun) recordDisplaced(ctx context.Context, it jobs.Item, d *engineDetail, rel string, found map[string]rclone.Object) error {
	if d.Displaced == "" {
		return nil
	}
	if obj, ok := found[d.Displaced]; ok {
		now := x.s.now()
		err := x.write(ctx, func(tx *sql.Tx) error {
			_, err := x.s.files.InsertRetainedTx(ctx, tx, syncer.RetainedVersion{DestinationID: x.d.ID, SourceID: d.SourceID, RelPath: rel,
				SourceRelPath: d.Source, Size: obj.Size, MtimeNs: obj.MtimeNs(), RetainedPath: d.Displaced, Reason: syncer.ReasonDisplaced,
				JobID: x.job.ID, RetainedAt: now, ExpiresAt: x.expiry(now)})
			return err
		})
		if err != nil {
			return err
		}
		x.plan.AddDisplaced(1)
	}
	d.Displaced, d.DisplacedSize, d.DisplacedMtimeNs = "", 0, 0
	return x.setDetail(ctx, it.ID, *d)
}

// recordEarlierDisplace records an object an earlier attempt of the item displaced but did not
// record (a crash between the moveto and the row).
func (x *rcloneRun) recordEarlierDisplace(ctx context.Context, it jobs.Item, d *engineDetail) error {
	if d.Displaced == "" {
		return nil
	}
	found, err := x.statMany(ctx, d.Displaced)
	if err != nil {
		return err
	}
	return x.recordDisplaced(ctx, it, d, it.RelPath, found)
}

// --- promote ---

// promote makes a recorded-only link hold its primary's object before the primary is retained or
// updated (§7.3 step 1): the target is listed (an unmanaged object there is displaced first), the
// primary's object is moved to it with moveto --max-delete 1, both paths are listed, then the
// record (syncer.Store.PromoteTx).
func (x *rcloneRun) promote(ctx context.Context, it jobs.Item, d *engineDetail) error {
	t, err := x.s.files.Get(ctx, d.TargetID)
	if errors.Is(err, syncer.ErrNotFound) {
		return itemErrorf("the hardlinked name %s is no longer recorded", it.RelPath)
	}
	if err != nil {
		return err
	}
	if t.State == syncer.StatePresent && t.LinkOf == 0 {
		return x.done(ctx, it, d, 0, "already done")
	}
	v, err := x.s.files.Get(ctx, d.RecordID)
	if errors.Is(err, syncer.ErrNotFound) {
		return itemErrorf("the record of %s vanished", d.From)
	}
	if err != nil {
		return err
	}
	if !v.State.Live() || t.LinkOf != v.ID || t.State != syncer.StateLinkRecorded {
		return itemErrorf("the hardlink records of %s changed since planning", d.From)
	}
	if err := x.recordEarlierDisplace(ctx, it, d); err != nil {
		return err
	}
	found, err := x.statMany(ctx, v.RelPath, t.RelPath)
	if err != nil {
		return err
	}
	vo, vok := found[v.RelPath]
	to, tok := found[t.RelPath]
	renamed := !vok && tok && to.Size == v.Size
	if !renamed {
		st, err := filecopy.Lstat(x.roots[d.SourceID], t.SourceRelPath)
		if err != nil || st.Size != t.Size || st.MtimeNs != t.MtimeNs {
			return itemErrorf("%s changed at the source since planning", t.SourceRelPath)
		}
		if d.For == syncer.PromoteForUpdate && !x.job.Params.AllowChanges {
			if ps, err := filecopy.Lstat(x.roots[d.SourceID], v.SourceRelPath); err == nil && shrinks(ps.Size, v.Size) {
				msg := fmt.Sprintf("held with the update of %s: its new version is %d bytes, less than half of the backed-up %d bytes (possible truncation); run a sync with allowChanges to apply it",
					v.RelPath, ps.Size, v.Size)
				x.log(slog.LevelWarn, "item held", "action", string(it.Action), "path", it.RelPath, "reason", msg)
				return x.finish(ctx, it.ID, jobs.ItemHeld, 0, msg)
			}
		}
		if !vok || vo.Size != v.Size {
			return itemErrorf("%s is not at the destination (or not the recorded size); %s keeps its link", v.RelPath, t.RelPath)
		}
		if tok {
			if err := x.displace(ctx, it, d, t.RelPath, to); err != nil {
				return err
			}
		}
		if err := x.moveTo(ctx, v.RelPath, t.RelPath); err != nil {
			return err
		}
		if found, err = x.statMany(ctx, v.RelPath, t.RelPath); err != nil {
			return err
		}
		faultinject.Point(PointRcloneAfterStat)
		if to, tok = found[t.RelPath]; !tok || to.Size != v.Size {
			return itemErrorf("%s did not arrive at %s", v.RelPath, t.RelPath)
		}
	}
	// A server-side move that copied and did not delete leaves the primary's object at its old
	// path too: after a promote for a retain nothing records that path any more, so the copy left
	// there is displaced into retention (an update's item moves it into its backup dir itself).
	leftover, still := found[v.RelPath]
	now := x.s.now()
	err = x.write(ctx, func(tx *sql.Tx) error {
		return x.s.files.PromoteTx(ctx, tx, v.ID, t.ID, true, d.For, x.job.ID, now)
	})
	if errors.Is(err, syncer.ErrNotDependent) {
		return itemErrorf("%v", err)
	}
	if err != nil {
		return err
	}
	x.touched[x.sources[d.SourceID].DestFolder] = true
	if still && d.For == syncer.PromoteForRetain {
		if err := x.displace(ctx, it, d, v.RelPath, leftover); err != nil {
			return err
		}
	}
	return x.done(ctx, it, d, 0, "")
}

// shrinks is the S10(b) shrink rule: less than half, or empty.
func shrinks(newSize, oldSize int64) bool {
	return newSize < oldSize && (newSize == 0 || newSize*2 < oldSize)
}

// --- move ---

// move renames an object server-side (§7.3 step 2): the target is listed first and an occupied
// one displaced, then moveto --max-delete 1, a listing of both paths, and the record, which takes
// the new path with its head_tail. A recorded object that is gone or changed fails the item; the
// next sync copies the file and retains the old name.
func (x *rcloneRun) move(ctx context.Context, it jobs.Item, d *engineDetail) error {
	v, err := x.s.files.Get(ctx, d.RecordID)
	if errors.Is(err, syncer.ErrNotFound) {
		return itemErrorf("the record of %s vanished", d.From)
	}
	if err != nil {
		return err
	}
	if v.State.Live() && v.RelPath == it.RelPath {
		return x.done(ctx, it, d, 0, "already done")
	}
	if !v.State.Live() || v.RelPath != d.From {
		return itemErrorf("the record of %s changed since planning", d.From)
	}
	if err := x.recordEarlierDisplace(ctx, it, d); err != nil {
		return err
	}
	found, err := x.statMany(ctx, d.From, it.RelPath)
	if err != nil {
		return err
	}
	fo, fok := found[d.From]
	to, tok := found[it.RelPath]
	switch {
	case !fok && tok && to.Size == v.Size:
		// Moved by an earlier attempt.
	case fok && fo.Size == v.Size:
		if tok {
			if _, recorded, err := x.s.files.LiveAt(ctx, x.d.ID, it.RelPath); err != nil {
				return err
			} else if recorded {
				return itemErrorf("%s is already recorded at the destination", it.RelPath)
			}
			if err := x.displace(ctx, it, d, it.RelPath, to); err != nil {
				return err
			}
		}
		if err := x.moveTo(ctx, d.From, it.RelPath); err != nil {
			return err
		}
		if found, err = x.statMany(ctx, d.From, it.RelPath); err != nil {
			return err
		}
		faultinject.Point(PointRcloneAfterStat)
		if to, tok = found[it.RelPath]; !tok || to.Size != v.Size {
			return itemErrorf("%s did not arrive at %s", d.From, it.RelPath)
		}
	case !fok && !tok:
		return itemErrorf("the recorded object %s is missing; the next sync copies the file", d.From)
	default:
		return itemErrorf("the recorded object %s is missing or changed; the next sync copies the file", d.From)
	}
	now := x.s.now()
	err = x.write(ctx, func(tx *sql.Tx) error {
		if _, err := x.s.files.RecordContentTx(ctx, tx, syncer.ContentDone{DestinationID: x.d.ID, SourceID: d.SourceID, RecordID: v.ID,
			RelPath: it.RelPath, SourceRelPath: d.Source, Size: v.Size, MtimeNs: v.MtimeNs, HeadTail: v.HeadTail, State: syncer.StatePresent,
			JobID: x.job.ID, CopiedAt: now, Moved: true}); err != nil {
			return err
		}
		if t := x.s.o.Tiers; t != nil && v.SourceRelPath != d.Source {
			return t.MoveFlagsTx(ctx, tx, d.SourceID, v.SourceRelPath, d.Source)
		}
		return nil
	})
	if errors.Is(err, syncer.ErrRecordChanged) {
		return itemErrorf("%v", err)
	}
	if err != nil {
		return err
	}
	x.touched[x.sources[d.SourceID].DestFolder] = true
	if left, still := found[d.From]; still {
		// A server-side move that copied and did not delete: the old path is unmanaged now.
		if err := x.displace(ctx, it, d, d.From, left); err != nil {
			return err
		}
	}
	return x.done(ctx, it, d, 0, "")
}

// --- link ---

// link records another name of a hardlink group as link_recorded (§7.3 step 4): rclone keeps one
// object per group, and links.tsv lists the names. The primary must be recorded present with the
// name's size and mtime, and both source names must be one file; otherwise the name is copied as
// its own object. A name's own old object (a relink) moves into retention first.
func (x *rcloneRun) link(ctx context.Context, it jobs.Item, d *engineDetail) error {
	root := x.roots[d.SourceID]
	src := x.sources[d.SourceID]
	st, err := filecopy.Lstat(root, d.Source)
	if err != nil || !st.Regular() {
		return itemErrorf("%s is gone from the source", d.Source)
	}
	prim, primOK, err := x.s.files.LiveBySourcePath(ctx, x.d.ID, d.SourceID, d.Primary)
	if err != nil {
		return err
	}
	valid := primOK && prim.State == syncer.StatePresent && prim.Size == st.Size &&
		filecopy.MtimeMatch(prim.MtimeNs, st.MtimeNs, x.caps.MtimeGranularityNs, 0) && prim.RelPath != it.RelPath
	if valid {
		ps, err := filecopy.Lstat(root, d.Primary)
		valid = err == nil && ps.Dev == st.Dev && ps.Ino == st.Ino
	}
	rec, hasRec, err := x.s.files.LiveAt(ctx, x.d.ID, it.RelPath)
	if err != nil {
		return err
	}
	if hasRec && rec.SourceID != d.SourceID {
		return itemErrorf("%s is recorded for another (or a removed) source; it is left alone", it.RelPath)
	}
	if !valid {
		x.log(slog.LevelInfo, "hardlink not possible: copying the name instead", "path", it.RelPath, "primary", d.Primary)
		x.plan.Reclassify(jobs.ActionLink, jobs.ActionCopy)
		return x.copyAlone(ctx, src, it, *d)
	}
	if hasRec && rec.State == syncer.StateLinkRecorded && rec.LinkOf == prim.ID && rec.Size == st.Size && rec.MtimeNs == st.MtimeNs {
		return x.done(ctx, it, d, 0, "already done")
	}
	if hasRec && rec.State != syncer.StateLinkRecorded {
		// The name's own object (its old version) goes into retention first.
		if err := x.retainOwnObject(ctx, rec); err != nil {
			return err
		}
	}
	now := x.s.now()
	err = x.write(ctx, func(tx *sql.Tx) error {
		recID := int64(0)
		if hasRec {
			recID = rec.ID
		}
		_, err := x.s.files.RecordContentTx(ctx, tx, syncer.ContentDone{DestinationID: x.d.ID, SourceID: d.SourceID, RecordID: recID,
			RelPath: it.RelPath, SourceRelPath: d.Source, Size: st.Size, MtimeNs: st.MtimeNs, HeadTail: prim.HeadTail,
			State: syncer.StateLinkRecorded, LinkOf: prim.ID, JobID: x.job.ID, CopiedAt: now})
		return err
	})
	if errors.Is(err, syncer.ErrRecordChanged) {
		return itemErrorf("%v", err)
	}
	if err != nil {
		return err
	}
	return x.done(ctx, it, d, 0, "recorded")
}

// retainOwnObject moves a live record's own object into retention (reason replaced) before the
// name becomes a recorded-only link: the intent, moveto to a free retention name, a listing, then
// the retained row; the record keeps its path.
func (x *rcloneRun) retainOwnObject(ctx context.Context, rec syncer.Record) error {
	found, err := x.statMany(ctx, rec.RelPath)
	if err != nil {
		return err
	}
	obj, ok := found[rec.RelPath]
	if !ok {
		return nil
	}
	dst, err := x.freeRetention(ctx, rec.RelPath)
	if err != nil {
		return err
	}
	reason := syncer.ReasonReplaced
	if rec.State == syncer.StateMissing {
		reason = syncer.ReasonDamaged
	}
	if err := x.write(ctx, func(tx *sql.Tx) error {
		return x.s.files.SetIntentTx(ctx, tx, x.d.ID, rec.RelPath, dst, reason)
	}); err != nil {
		if errors.Is(err, syncer.ErrRecordChanged) {
			return itemErrorf("%v", err)
		}
		return err
	}
	faultinject.Point(PointRcloneAfterIntent)
	if err := x.moveTo(ctx, rec.RelPath, dst); err != nil {
		return err
	}
	after, err := x.statMany(ctx, rec.RelPath, dst)
	if err != nil {
		return err
	}
	faultinject.Point(PointRcloneAfterStat)
	now := x.s.now()
	return x.write(ctx, func(tx *sql.Tx) error {
		if ret, ok := after[dst]; ok {
			hash := ""
			if ret.Size == rec.Size {
				hash = rec.Hash
			}
			if _, err := x.s.files.InsertRetainedTx(ctx, tx, syncer.RetainedVersion{DestinationID: x.d.ID, SourceID: rec.SourceID,
				RelPath: rec.RelPath, SourceRelPath: rec.SourceRelPath, Size: ret.Size, MtimeNs: obj.MtimeNs(), Hash: hash, HeadTail: rec.HeadTail,
				RetainedPath: dst, Reason: reason, JobID: x.job.ID, RetainedAt: now, ExpiresAt: x.expiry(now)}); err != nil {
				return err
			}
		}
		return x.s.files.ClearIntentTx(ctx, tx, rec.ID, dst)
	})
}

// --- finish ---

// finishSync uploads links.tsv when its content changed (§7.3 step 6) and, on SFTP, removes the
// directories moves and retains left empty inside the destination folders. Failures are
// warnings: the next sync tries again.
func (x *rcloneRun) finishSync(ctx context.Context) {
	b, err := x.s.files.LinkManifest(ctx, x.d.ID)
	if err == nil {
		sum := sha256.Sum256(b)
		digest := hex.EncodeToString(sum[:])
		st, serr := x.s.State(ctx, x.d.ID)
		var last string
		if serr == nil {
			st.stat(statLinksSHA256, &last)
		}
		if last != digest {
			if err = x.conn.Rcat(ctx, syncer.LinkManifestRel, b); err == nil {
				err = x.s.updateState(ctx, x.d.ID, func(st *EngineState) { st.setStat(statLinksSHA256, digest) })
			}
		}
	}
	if err != nil {
		x.warn("could not upload the hardlink manifest", "path", syncer.LinkManifestRel, "error", err.Error())
	}
	if x.ed.Kind != engines.SFTP {
		return
	}
	for _, folder := range sortedKeys(x.touched) {
		if folder == "" {
			continue
		}
		if err := x.conn.Rmdirs(ctx, folder, true); err != nil {
			x.log(slog.LevelInfo, "could not remove empty directories", "folder", folder, "error", err.Error())
		}
	}
}

// statLinksSHA256 is the engine_state stat that holds the sha256 of the last links.tsv uploaded.
const statLinksSHA256 = "linksSha256"
