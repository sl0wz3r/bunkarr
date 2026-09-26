package syncer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/tiers"
)

// Tiers in a sync (docs/design/phase2-3.md §8.5, S6 amended, S10, S14, S15). Options.Tiers
// decides each live file's tier at the destination (internal/tiers); with no rules every file is
// full and the plan is Phase 1's. The planner then:
//   - plans full files exactly as in Phase 1; their items carry detail.tier (the decision);
//   - gives non-full files without a live record no item (a dry run lists them as skip items
//     "not copied"); they can still be the new side of a move, since the move pairing considers
//     every live file without a record, of any tier;
//   - keeps non-full files with a live record (S15): the record counts as seen, so it is never
//     taken for a vanished file, and it gets no update, repair or link (a dry run lists a skip
//     item "kept"). A tier change never removes a backup;
//   - releases kept records only in a sync with Params.ReleaseDemoted: a dry run plans a release
//     (a retain with reason released) for every kept record; a real run only for the records its
//     dry run (ReleaseOf) listed, at the same rule revision, that are still kept. Each release
//     item re-reads tiers.revision and the file's tier when it runs and is skipped when either
//     changed; it skips the reappeared check and the S6 wait (its source file is still there);
//   - counts a copy that is full only because a fact is unknown (unknownPromoted) as a change for
//     the mass-change guard, and holds those copies rather than failing the job when they are
//     what does not fit in the free space.

// Tiers is what the runners need from internal/tiers; *tiers.Engine implements it.
type Tiers interface {
	// Decisions decides the tiers of src's live files at destination destinationID (q nil: the
	// read pool).
	Decisions(ctx context.Context, q tiers.Queryer, destinationID int64, src catalog.Source) (*tiers.SourceDecisions, error)
	// Revision returns tiers.revision now.
	Revision(ctx context.Context) (int64, error)
	// MoveFlagsTx makes an exact-file flag follow a move, in the move's transaction.
	MoveFlagsTx(ctx context.Context, tx *sql.Tx, sourceID int64, from, to string) error
	// FlagsAfterSync lets folder flags follow the folder renames among a source's moves.
	FlagsAfterSync(ctx context.Context, sourceID int64, moves []tiers.Move) error
	// Coverage returns the irreplaceable-flag coverage of source paths (the retention hold).
	Coverage(ctx context.Context) (func(sourceID int64, rel string) (flagID int64, covered bool), error)
}

// ReasonReleased: a kept file (not full at the destination any more) the user released (S15).
const ReasonReleased = "released"

// Detail reasons of the tier items.
const (
	reasonKept      = "kept"
	reasonNotCopied = "not copied"
	// reasonReplacedInPlace is an update of a file the *arr replaced at the same path with equal
	// size and mtime (§8.5).
	reasonReplacedInPlace = "replaced in place"
)

// TierStats are the live files of a sync's scope by tier at the destination (§8.5).
type TierStats struct {
	Full            TierCount `json:"full"`
	Manifest        TierCount `json:"manifest"`
	Skip            TierCount `json:"skip"`
	UnknownPromoted TierCount `json:"unknownPromoted"`
}

// TierCount is a number of files and their bytes.
type TierCount struct {
	Files int64 `json:"files"`
	Bytes int64 `json:"bytes"`
}

func (c *TierCount) add(size int64) {
	c.Files++
	c.Bytes += size
}

// tierState is a sync attempt's tier bookkeeping.
type tierState struct {
	stats    TierStats
	kept     TierCount
	released TierCount
	moved    int64
	revision int64
	// stale are the stale references already warned about (one warning each per job).
	stale map[[2]int]bool
	// nonFull holds, per source, the paths this planning attempt decided are not full: a retain
	// does not wait for them (S6 amended). A resumed attempt holds none, so every file that is not
	// backed up blocks, as in Phase 1.
	nonFull map[int64]map[string]bool
	// release is the release plan of a ReleaseDemoted sync (nil otherwise).
	release *releasePlan
	// execDecisions caches, per source, the decisions a release item re-checks, with when they
	// were made.
	execDecisions map[int64]*timedDecisions
	// unknownBytes are the planned bytes of unknown-promoted copies.
	unknownBytes int64
	// newNonFull are the non-full files without a record, by size and mtime: the sources they
	// are in (movedToNonFull).
	newNonFull map[[2]int64][]int64
	// vanished are the planned retains of vanished records (size, mtime, source).
	vanished [][3]int64
}

type timedDecisions struct {
	d  *tiers.SourceDecisions
	at time.Time
}

// releasePlan says which kept records a release may retain.
type releasePlan struct {
	// all: a dry run lists every kept record.
	all bool
	// allowed are the records the confirmed dry run listed (a real run), by id, with what the
	// preview saw of each (a record id can be reused once its row is deleted).
	allowed map[int64]releasedRecord
	// revision is the rule revision the confirmed dry run evaluated (a real run): a file decided
	// at another revision is not released, and each release item re-checks against it.
	revision int64
	// staleFiles counts the kept files a real run did not release because their decision was
	// made at another revision than the preview's.
	staleFiles int64
}

// releasedRecord is a record as the release preview listed it.
type releasedRecord struct {
	sourceID      int64
	relPath       string
	sourceRelPath string
	size, mtimeNs int64
}

// matches reports whether r is still the record the preview listed.
func (w releasedRecord) matches(r *Record) bool {
	return w.sourceID == r.SourceID && w.relPath == r.RelPath && w.sourceRelPath == r.SourceRelPath && w.size == r.Size && w.mtimeNs == r.MtimeNs
}

// decisions returns the tier decisions of src at the job's destination, or nil when the runner
// has no Tiers (every file is full, items carry no tier). Stale references are warned about once
// per job; the non-full paths are remembered for the S6 check.
func (s *syncRun) decisions(ctx context.Context, src catalog.Source) (*tiers.SourceDecisions, error) {
	if s.r.tiers == nil {
		return nil, nil
	}
	d, err := s.r.tiers.Decisions(ctx, nil, s.h.Destination.ID, src)
	if err != nil {
		return nil, fmt.Errorf("decide the tiers of source %q: %w", src.Name, err)
	}
	s.tier.revision = d.Revision
	for _, w := range d.Stale {
		k := [2]int{w.RuleIndex, w.ConditionIndex}
		if s.tier.stale[k] {
			continue
		}
		if s.tier.stale == nil {
			s.tier.stale = map[[2]int]bool{}
		}
		s.tier.stale[k] = true
		s.warnings++
		s.rep.Log(slog.LevelWarn, fmt.Sprintf("tier rule %d, condition %d: %s", w.RuleIndex+1, w.ConditionIndex+1, w.Message))
	}
	for _, u := range d.Unknown {
		s.rep.Log(slog.LevelInfo, "tier facts are unknown (files they decide stay full)", "integration", u.Name, "reason", u.Reason)
	}
	return d, nil
}

// prepareRelease loads the release plan of a ReleaseDemoted sync. A real run releases only the
// records its dry run (ReleaseOf) listed, and nothing when the rules changed since that dry run.
func (s *syncRun) prepareRelease(ctx context.Context) error {
	p := s.job.Params
	if !p.ReleaseDemoted || s.r.tiers == nil {
		if p.ReleaseDemoted {
			s.warnings++
			s.rep.Log(slog.LevelWarn, "releaseDemoted: this Bunkarr has no tier engine; nothing is released")
		}
		return nil
	}
	if s.job.DryRun {
		s.tier.release = &releasePlan{all: true}
		return nil
	}
	rev, err := s.r.tiers.Revision(ctx)
	if err != nil {
		return err
	}
	if p.ReleaseRevision != rev {
		s.warnings++
		s.rep.Log(slog.LevelWarn, "rules changed since the preview; nothing is released (run the release preview again)",
			"previewRevision", p.ReleaseRevision, "revision", rev)
		s.tier.release = &releasePlan{allowed: map[int64]releasedRecord{}, revision: rev}
		return nil
	}
	allowed, refused, err := s.r.store.releasePreview(ctx, p.ReleaseOf, s.h.Destination.ID, rev)
	if err != nil {
		return err
	}
	if refused != "" {
		s.warnings++
		s.rep.Log(slog.LevelWarn, refused+"; nothing is released", "previewJob", p.ReleaseOf)
	}
	rp := &releasePlan{allowed: allowed, revision: rev}
	s.rep.Log(slog.LevelInfo, "release: the preview lists records to release", "previewJob", p.ReleaseOf, "records", len(rp.allowed))
	s.tier.release = rp
	return nil
}

// --- planner ---

// full reports whether a file is full at the destination (always, without Tiers).
func (f *planFile) full() bool { return f.dec == nil || f.dec.Tier == tiers.Full }

// withTier puts the file's decision into an item's detail.
func (f *planFile) withTier(d *Detail) {
	if f.dec != nil {
		dec := *f.dec
		d.Tier = &dec
	}
}

// tierItem is a dry run's skip item for a non-full file (kept or not copied).
func (p *sourcePlanner) tierItem(f *planFile, reason string, r *Record) {
	if !p.dryRun {
		return
	}
	d := p.fileDetail(f, reason)
	f.withTier(&d)
	rel := p.destPath(f.rel)
	if r != nil {
		d.RecordID, rel = r.ID, r.RelPath
		d.Note = fmt.Sprintf("kept at the destination: %s", f.dec.Summary())
	} else {
		d.Note = fmt.Sprintf("not copied: %s", f.dec.Summary())
	}
	p.skips = append(p.skips, &planItem{action: jobs.ActionSkip, rel: rel, fileID: f.id, bytes: f.size, d: d})
}

// keep plans a non-full file that has a live record (S15): nothing changes at the destination;
// a release plans its retain.
func (p *sourcePlanner) keep(f *planFile, r *Record) {
	if r.State == StateMissing {
		return // not repaired, and nothing to release
	}
	p.tierKept.add(f.size)
	p.tierItem(f, reasonKept, r)
	rp := p.release
	if rp == nil {
		return
	}
	// A dry run evaluates the rules of its own decisions; a real run releases only what its
	// confirmed preview listed (the same record, not a reused id), decided at the preview's
	// revision, and each item re-checks that revision when it runs (§8.5, S15).
	rev := f.dec.Revision
	if !rp.all {
		if w, ok := rp.allowed[r.ID]; !ok || !w.matches(r) {
			return
		}
		if rev != rp.revision {
			rp.staleFiles++
			return
		}
		rev = rp.revision
	}
	d := Detail{SourceID: p.sourceID, Source: f.rel, RecordID: r.ID, Size: r.Size, MtimeNs: r.MtimeNs, Reason: ReasonReleased,
		TierRevision: rev}
	f.withTier(&d)
	it := &planItem{action: jobs.ActionRetain, rel: r.RelPath, fileID: r.ID, bytes: r.Size, change: true, d: d}
	p.releases = append(p.releases, it)
	p.releasedCount.add(r.Size)
}

// sortReleases orders release items so that a recorded-only link goes before the primary whose
// content it records (the primary's retain refuses while a live recorded-only link needs it),
// then hardlinks, then files.
func sortReleases(items []*planItem, recState map[int64]State) {
	rank := func(it *planItem) int {
		switch recState[it.d.RecordID] {
		case StateLinkRecorded:
			return 0
		case StateLinked:
			return 1
		}
		return 2
	}
	slices.SortStableFunc(items, func(a, b *planItem) int { return rank(a) - rank(b) })
}

// countTier adds a planned file to the tier stats.
func (p *sourcePlanner) countTier(f *planFile) {
	if f.dec == nil {
		return
	}
	switch f.dec.Tier {
	case tiers.Full:
		p.tierStats.Full.add(f.size)
		if f.dec.UnknownPromoted {
			p.tierStats.UnknownPromoted.add(f.size)
		}
	case tiers.Manifest:
		p.tierStats.Manifest.add(f.size)
	default:
		p.tierStats.Skip.add(f.size)
	}
}

// replacedInPlace reports whether an unchanged present file was replaced at the same path with
// equal size and mtime (§8.5): the *arr reports a file there added after the record's copy, and
// the head and tail hashes of the source and destination files differ. When they are equal the
// check is remembered (replaceChecked): each *arr file is compared once, not on every sync.
func (p *sourcePlanner) replacedInPlace(f *planFile) bool {
	r := f.rec
	if p.arrAdded == nil || r == nil || r.State != StatePresent || r.CopiedAt == nil {
		return false
	}
	added, ok := p.arrAdded(f.rel)
	if !ok || !added.After(*r.CopiedAt) {
		return false
	}
	sh, err := p.fs.sourceHeadTail(f.rel)
	if err != nil {
		return false
	}
	dh, err := p.fs.destHeadTail(r.RelPath)
	if err != nil {
		return false
	}
	if sh != dh {
		return true
	}
	p.replaceChecked = append(p.replaceChecked, replaceCheck{id: r.ID, size: r.Size, mtimeNs: r.MtimeNs, copiedAt: *r.CopiedAt, added: added})
	return false
}

// replaceCheck is a same-path check whose head and tail hashes matched: the record holds the
// content of the *arr file added at added.
type replaceCheck struct {
	id, size, mtimeNs int64
	copiedAt, added   time.Time
}

// markReplaceChecked remembers the same-path checks that found the backup equal to the *arr's file:
// copied_at moves to that file's dateAdded, so the next sync compares again only when the *arr
// adds a newer file at the path. A row that changed since planning is left alone.
func (s *Store) markReplaceChecked(ctx context.Context, checks []replaceCheck) error {
	if len(checks) == 0 {
		return nil
	}
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		for _, c := range checks {
			_, err := tx.ExecContext(ctx, `UPDATE destination_files SET copied_at = ?
				WHERE id = ? AND state = 'present' AND size = ? AND mtime_ns = ? AND copied_at = ?`,
				db.FormatTime(c.added), c.id, c.size, c.mtimeNs, db.FormatTime(c.copiedAt))
			if err != nil {
				return fmt.Errorf("record the same-path check of %d: %w", c.id, err)
			}
		}
		return nil
	})
}

// --- after planning ---

// holdUnknownCopies runs when the planned copies do not fit in the free space but the copies
// whose tier was decided do: every unknown-promoted copy is held (S10, §8.5) and the rest runs. A
// copy an earlier attempt already started (its temp file is recorded) is left to finish. The
// unknown-promoted links of the held copies are held with them (holdLinksOfHeld).
func (s *syncRun) holdUnknownCopies(ctx context.Context, avail int64) error {
	var held int64
	for after := int64(0); ; {
		batch, err := s.env.Items.Pending(ctx, s.job.ID, after, pendingBatch)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}
		for _, it := range batch {
			after = it.ID
			if it.Action != jobs.ActionCopy {
				continue
			}
			d, err := parseDetail(it)
			if err != nil || d.Tier == nil || !d.Tier.UnknownPromoted || d.Temp != "" {
				continue
			}
			msg := fmt.Sprintf("held: tier unknown because %s; not enough free space", d.Tier.UnknownWhy())
			if err := s.env.Items.Finish(ctx, it.ID, jobs.ItemHeld, 0, msg); err != nil {
				return err
			}
			held++
		}
	}
	links, err := s.holdLinksOfHeld(ctx)
	if err != nil {
		return err
	}
	s.bytesPlanned -= s.tier.unknownBytes
	s.warnings++
	msg := fmt.Sprintf("%s held: they are full only because a fact is unknown, and together with them the copies do not fit in the free space (%s free)",
		plural(held, "copy", "copies"), formatBytes(avail))
	if links > 0 {
		msg += fmt.Sprintf("; %s of them held too", plural(links, "hardlink", "hardlinks"))
	}
	s.rep.Log(slog.LevelWarn, msg)
	return nil
}

// holdLinksOfHeld holds the pending unknown-promoted links (new names, relinks and the relinks of
// a repaired primary alike) whose primary's copy is held, by this attempt or an earlier one: the
// primary is then not at the destination, and the link would fall back to a copy of the same
// content — the bytes the hold kept from being copied. Only the unknown-promoted copies are held
// (§8.5): a link whose own tier was decided runs, falling back to a copy (the only copy of the
// content that a decided-full name gets). A link whose fallback copy an earlier attempt started
// is left to finish. It returns how many links it held.
func (s *syncRun) holdLinksOfHeld(ctx context.Context) (int64, error) {
	heldCopies, err := s.r.store.heldCopies(ctx, s.job.ID)
	if err != nil || len(heldCopies) == 0 {
		return 0, err
	}
	var held int64
	for after := int64(0); ; {
		batch, err := s.env.Items.Pending(ctx, s.job.ID, after, pendingBatch)
		if err != nil {
			return held, err
		}
		if len(batch) == 0 {
			return held, nil
		}
		for _, it := range batch {
			after = it.ID
			if it.Action != jobs.ActionLink {
				continue
			}
			d, err := parseDetail(it)
			if err != nil || d.Temp != "" || d.Tier == nil || !d.Tier.UnknownPromoted {
				continue
			}
			prim, ok := heldCopies[catalog.Location{SourceID: d.SourceID, Rel: d.Primary}]
			if !ok {
				continue
			}
			msg := fmt.Sprintf("held with %s (copy): the hardlink needs its primary at the destination", prim)
			if err := s.env.Items.Finish(ctx, it.ID, jobs.ItemHeld, 0, msg); err != nil {
				return held, err
			}
			held++
		}
	}
}

// heldCopies returns the job's held copy items by source path, with their destination paths. It
// reads the job tables of internal/jobqueue, read-only (jobs.ItemStore lists pending items only).
func (s *Store) heldCopies(ctx context.Context, jobID int64) (map[catalog.Location]string, error) {
	rows, err := s.db.Reader().QueryContext(ctx, `SELECT rel_path, detail FROM job_items WHERE job_id = ? AND status = 'held' AND action = 'copy' ORDER BY id`, jobID)
	if err != nil {
		return nil, fmt.Errorf("read the held copies of job %d: %w", jobID, err)
	}
	defer rows.Close()
	out := map[catalog.Location]string{}
	for rows.Next() {
		var rel, raw string
		if err := rows.Scan(&rel, &raw); err != nil {
			return nil, fmt.Errorf("read the held copies of job %d: %w", jobID, err)
		}
		if d, err := parseDetail(jobs.Item{Detail: []byte(raw)}); err == nil && d.SourceID != 0 && d.Source != "" {
			out[catalog.Location{SourceID: d.SourceID, Rel: d.Source}] = rel
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the held copies of job %d: %w", jobID, err)
	}
	return out, nil
}

// recheckUnknownHold runs when a sync resumes a complete plan: the attempt that planned it may have
// stopped before or while it held the unknown-promoted copies that do not fit in the free space
// (S14), so the pending copies are checked again and those copies held when they do not fit. A
// resumed plan is otherwise not checked for free space (as in Phase 1).
func (s *syncRun) recheckUnknownHold(ctx context.Context) error {
	if s.r.tiers == nil || s.job.DryRun {
		return nil
	}
	var total, unknown int64
	for after := int64(0); ; {
		batch, err := s.env.Items.Pending(ctx, s.job.ID, after, pendingBatch)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}
		for _, it := range batch {
			after = it.ID
			if it.Action != jobs.ActionCopy && it.Action != jobs.ActionUpdate {
				continue
			}
			total += it.Bytes
			if d, err := parseDetail(it); err == nil && it.Action == jobs.ActionCopy && d.Tier != nil && d.Tier.UnknownPromoted && d.Temp == "" {
				unknown += it.Bytes
			}
		}
	}
	if unknown > 0 {
		free, _, err := s.r.freeSpace(s.h.Root)
		if err != nil {
			s.warnings++
			s.rep.Log(slog.LevelWarn, "could not check the free space for the copies whose tier is unknown", "error", err.Error())
		} else if avail := availableSpace(free); total > avail {
			return s.holdUnknownCopies(ctx, avail)
		}
	}
	// The links of the copies an earlier attempt held (it may have stopped before it held them).
	_, err := s.holdLinksOfHeld(ctx)
	return err
}

// staleReleases warns when a real release run kept files it would have released because the rules
// changed while it planned (their decision is not the preview's revision).
func (s *syncRun) staleReleases() {
	if rp := s.tier.release; rp != nil && rp.staleFiles > 0 {
		s.warnings++
		s.rep.Log(slog.LevelWarn, fmt.Sprintf("rules changed since the preview; %s not released (run the release preview again)",
			plural(rp.staleFiles, "kept file", "kept files")), "previewRevision", rp.revision)
	}
}

// movedToNonFull counts backed-up files whose content (size and mtime) reappears in another
// source of the destination as a non-full file without a record: content that moved to another
// source cannot be paired as a move (§8.5).
func (s *syncRun) movedToNonFull() {
	for _, v := range s.tier.vanished {
		for _, src := range s.tier.newNonFull[[2]int64{v[0], v[1]}] {
			if src != v[2] {
				s.tier.moved++
				break
			}
		}
	}
}

// --- execution ---

// checkRelease re-checks a release item when it runs (§8.5): the rules must still be at the
// revision the preview evaluated, and the file must still be live and not full. It returns the
// reason to skip the item ("" to release).
func (x *itemRun) checkRelease(ctx context.Context, v Record) (string, error) {
	t := x.s.r.tiers
	if t == nil {
		return "no tier engine; not released", nil
	}
	rev, err := t.Revision(ctx)
	if err != nil {
		return "", err
	}
	if rev != x.d.TierRevision {
		return "rules changed since the preview; not released", nil
	}
	src, ok := x.s.linked[v.SourceID]
	if !ok {
		return "its source is no longer synced to this destination; not released", nil
	}
	cache := x.s.tier.execDecisions[v.SourceID]
	if cache == nil || x.now().Sub(cache.at) > time.Minute {
		d, err := t.Decisions(ctx, nil, x.destID(), src)
		if err != nil {
			return "", err
		}
		cache = &timedDecisions{d: d, at: x.now()}
		if x.s.tier.execDecisions == nil {
			x.s.tier.execDecisions = map[int64]*timedDecisions{}
		}
		x.s.tier.execDecisions[v.SourceID] = cache
	}
	loc := catalog.Location{SourceID: v.SourceID, Rel: v.SourceRelPath}
	live, err := x.s.r.cat.LiveFilesAt(ctx, nil, []catalog.Location{loc})
	if err != nil {
		return "", err
	}
	f, ok := live[loc]
	if !ok {
		return "the file is gone from the source; the next sync retains it", nil
	}
	if dec := cache.d.Decide(f.ID); dec.Tier == tiers.Full {
		return "tier is full again; not released", nil
	}
	return "", nil
}

// flagsAfterSync lets the folder flags follow the moves the job executed (§8.7). It runs after
// every attempt that executed, also one that failed or was cancelled; an attempt that stopped
// before executing replays them too (SyncRunner.run), and so does OnJobFinish for a job that
// ended without running again. The moves are read back from the job's items (doneMoves), so every
// attempt also replays the moves of the attempts before it (following a move again is a no-op).
// Best effort: a failure is a warning, the flags keep their paths.
func (s *syncRun) flagsAfterSync(ctx context.Context) {
	s.warnings += s.r.followMoves(ctx, s.job.ID, s.rep)
}

// followMoves lets the folder flags follow the moves job jobID executed; it returns the number of
// warnings it logged.
func (r *SyncRunner) followMoves(ctx context.Context, jobID int64, rep jobs.Reporter) int {
	if r.tiers == nil || jobID == 0 {
		return 0
	}
	ctx = context.WithoutCancel(ctx)
	moves, err := r.store.doneMoves(ctx, jobID)
	if err != nil {
		rep.Log(slog.LevelWarn, "flags could not follow the renamed folders", "error", err.Error())
		return 1
	}
	ids := make([]int64, 0, len(moves))
	for id := range moves {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	warnings := 0
	for _, id := range ids {
		if err := r.tiers.FlagsAfterSync(ctx, id, moves[id]); err != nil {
			warnings++
			rep.Log(slog.LevelWarn, "flags could not follow the renamed folders", "sourceId", id, "error", err.Error())
		}
	}
	return warnings
}

// OnJobFinish is the job manager's OnFinish hook (NewSyncRunner registers it). A sync that ended
// failed or cancelled may have executed moves that no attempt followed: one cancelled while it
// was queued for its resume, or failed by crash recovery after too many crashes, never runs again.
// The folder flags follow its moves now (a no-op when an attempt already did).
func (r *SyncRunner) OnJobFinish(job jobs.Job) {
	if job.Type != jobs.TypeSync || job.DryRun || r.tiers == nil ||
		(job.Status != jobs.StatusFailed && job.Status != jobs.StatusCancelled) {
		return
	}
	r.followMoves(context.Background(), job.ID, logReporter{log: r.log.With("jobId", job.ID)})
}

// logReporter is a jobs.Reporter that writes to the server log (a hook that runs outside a job).
type logReporter struct{ log *slog.Logger }

func (logReporter) Progress(jobs.Progress) {}
func (l logReporter) Log(level slog.Level, msg string, args ...any) {
	l.log.Log(context.Background(), level, msg, args...)
}

// doneMoves returns the moves a job executed, per source: its done move items that renamed a
// source path (not the ones that fell back to a copy), and its pending ones whose record already
// moved (the process died after the move's record was committed and before its item was
// finished; move finds such an item already done, but the job may never execute again). It reads
// the job tables of internal/jobqueue, read-only (jobs.ItemStore lists pending items only).
func (s *Store) doneMoves(ctx context.Context, jobID int64) (map[int64][]tiers.Move, error) {
	type moveItem struct {
		rel     string
		pending bool
		d       Detail
	}
	var items []moveItem
	err := func() error {
		rows, err := s.db.Reader().QueryContext(ctx, `SELECT rel_path, status, detail FROM job_items
			WHERE job_id = ? AND action = 'move' AND status IN ('done', 'pending') ORDER BY id`, jobID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var rel, status, raw string
			if err := rows.Scan(&rel, &status, &raw); err != nil {
				return err
			}
			d, err := parseDetail(jobs.Item{Detail: []byte(raw)})
			if err != nil || d.SourceID == 0 || d.FromSource == "" || d.FromSource == d.Source ||
				(d.Outcome != "" && d.Outcome != outcomeAlreadyDone) {
				continue
			}
			pending := status == string(jobs.ItemPending)
			if pending && d.RecordID == 0 {
				continue
			}
			items = append(items, moveItem{rel: rel, pending: pending, d: d})
		}
		return rows.Err()
	}()
	if err != nil {
		return nil, fmt.Errorf("read the moves of job %d: %w", jobID, err)
	}
	out := map[int64][]tiers.Move{}
	for _, it := range items {
		if it.pending {
			// The check move makes before it reports an item already done.
			rec, err := s.Get(ctx, it.d.RecordID)
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("read the moves of job %d: %w", jobID, err)
			}
			if !rec.State.Live() || rec.RelPath != it.rel || rec.SourceRelPath != it.d.Source {
				continue
			}
		}
		out[it.d.SourceID] = append(out[it.d.SourceID], tiers.Move{From: it.d.FromSource, To: it.d.Source})
	}
	return out, nil
}

// blocksRetain reports whether a live catalog file that is not backed up makes a retain in its
// folder wait (S6 amended): unless this planning attempt decided it is not full.
func (s *syncRun) blocksRetain(sourceID int64, rel string) bool {
	return !s.tier.nonFull[sourceID][rel]
}

// releasePreview returns the records the release preview (a dry-run sync) listed for release,
// after checking that it is a finished dry-run sync of this destination with releaseDemoted that
// evaluated revision; otherwise why it is refused. Its items are read whatever their status (the
// mass-change guard holds a large preview's release items). This reads the job tables of
// internal/jobqueue, read-only: jobs.ItemStore lists pending items only.
func (s *Store) releasePreview(ctx context.Context, jobID, destinationID, revision int64) (map[int64]releasedRecord, string, error) {
	allowed := map[int64]releasedRecord{}
	var ok int64
	err := s.db.Reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE id = ? AND type = 'sync' AND dry_run = 1
		AND destination_id = ? AND status IN ('completed', 'completed_with_warnings')
		AND json_extract(params, '$.releaseDemoted') = 1 AND json_extract(stats, '$.tierRevision') = ?`,
		jobID, destinationID, revision).Scan(&ok)
	if err != nil {
		return nil, "", fmt.Errorf("read the release preview (job %d): %w", jobID, err)
	}
	if ok == 0 {
		return allowed, fmt.Sprintf("job %d is not a finished release preview of this destination at the current rule revision", jobID), nil
	}
	rows, err := s.db.Reader().QueryContext(ctx, `SELECT rel_path, detail FROM job_items WHERE job_id = ? AND action = 'retain' ORDER BY id`, jobID)
	if err != nil {
		return nil, "", fmt.Errorf("read the release preview (job %d): %w", jobID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var rel, raw string
		if err := rows.Scan(&rel, &raw); err != nil {
			return nil, "", err
		}
		d, err := parseDetail(jobs.Item{Detail: []byte(raw)})
		if err == nil && d.Reason == ReasonReleased && d.RecordID != 0 {
			allowed[d.RecordID] = releasedRecord{sourceID: d.SourceID, relPath: rel, sourceRelPath: d.Source, size: d.Size, mtimeNs: d.MtimeNs}
		}
	}
	return allowed, "", rows.Err()
}

// releasedBy returns the records a job released at a destination.
func (s *Store) releasedBy(ctx context.Context, destinationID, jobID int64) (TierCount, error) {
	var c TierCount
	err := s.db.Reader().QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(size), 0) FROM destination_files
		WHERE destination_id = ? AND job_id = ? AND state = 'retained' AND reason = 'released'`, destinationID, jobID).Scan(&c.Files, &c.Bytes)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return c, fmt.Errorf("count released files: %w", err)
	}
	return c, nil
}

// addTierCounts adds a planned source's tier counts to the attempt's.
func (s *syncRun) addTierCounts(p *sourcePlanner, sourceID int64) {
	t := &s.tier
	for _, pair := range [][2]*TierCount{{&t.stats.Full, &p.tierStats.Full}, {&t.stats.Manifest, &p.tierStats.Manifest},
		{&t.stats.Skip, &p.tierStats.Skip}, {&t.stats.UnknownPromoted, &p.tierStats.UnknownPromoted},
		{&t.kept, &p.tierKept}, {&t.released, &p.releasedCount}} {
		pair[0].Files += pair[1].Files
		pair[0].Bytes += pair[1].Bytes
	}
	for _, f := range p.nonFullNew {
		if t.newNonFull == nil {
			t.newNonFull = map[[2]int64][]int64{}
		}
		k := [2]int64{f.size, f.mtimeNs}
		t.newNonFull[k] = append(t.newNonFull[k], sourceID)
	}
}

// tierResult fills the tier stats of a sync's result.
func (s *syncRun) tierResult(ctx context.Context, st *SyncStats) error {
	if s.r.tiers == nil {
		return nil
	}
	if s.planned {
		ts := s.tier.stats
		st.Tiers = &ts
	} else if rev, err := s.r.tiers.Revision(ctx); err == nil {
		s.tier.revision = rev
	}
	st.FilesKept, st.BytesKept = s.tier.kept.Files, s.tier.kept.Bytes
	st.MovedToNonFull = s.tier.moved
	st.StaleReferences = int64(len(s.tier.stale))
	st.TierRevision = s.tier.revision
	if s.job.DryRun {
		st.FilesReleased, st.BytesReleased = s.tier.released.Files, s.tier.released.Bytes
	} else if s.job.Params.ReleaseDemoted {
		c, err := s.r.store.releasedBy(context.WithoutCancel(ctx), s.h.Destination.ID, s.job.ID)
		if err != nil {
			return err
		}
		st.FilesReleased, st.BytesReleased = c.Files, c.Bytes
		// A released file is no longer kept: the stats say what stays at the destination
		// (a dry run's kept files include the ones it would release, as the preview's do).
		st.FilesKept, st.BytesKept = max(0, st.FilesKept-c.Files), max(0, st.BytesKept-c.Bytes)
	}
	st.FilesRetained = max(0, st.FilesRetained-st.FilesReleased)
	return nil
}

// flagHold returns why a retained record must not expire: an irreplaceable flag covers its
// source path (phase2-3.md §8.7). The coverage is read once per retention attempt.
func (rr *retentionRun) flagHold(ctx context.Context, rec Record) (string, error) {
	t := rr.r.tiers
	if t == nil || rec.SourceID == 0 {
		return "", nil
	}
	if rr.covered == nil {
		fn, err := t.Coverage(ctx)
		if err != nil {
			return "", fmt.Errorf("read the irreplaceable flags: %w", err)
		}
		rr.covered = fn
	}
	if id, ok := rr.covered(rec.SourceID, rec.SourceRelPath); ok {
		return fmt.Sprintf("irreplaceable (flag #%d): not expired; remove the flag to let it expire", id), nil
	}
	return "", nil
}

var _ Tiers = (*tiers.Engine)(nil)
