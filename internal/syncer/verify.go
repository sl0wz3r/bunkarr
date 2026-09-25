package syncer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"syscall"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// VerifyRunner runs verify jobs (jobs.TypeVerify, design §4.5): every live file record of the
// destination is checked for existence and size (cheap); a sample (settings.verify.samplePercent,
// the least recently verified first, so files without a hash come first; mode full: all) is
// re-read with the page cache dropped and hashed. A missing, short or mismatching file fails its
// item and marks the record missing, so the next sync copies it again. Files copied with
// verification off get their hash recorded on their first verify.
type VerifyRunner struct {
	base
	planBatch    int
	recheckEvery int
}

// NewVerifyRunner returns the verify job runner.
func NewVerifyRunner(o Options) *VerifyRunner {
	return &VerifyRunner{base: newBase(o), planBatch: planBatch, recheckEvery: recheckEvery}
}

var _ jobs.Runner = (*VerifyRunner)(nil)

// VerifyStats is a verify job's stats JSON.
type VerifyStats struct {
	DryRun bool `json:"dryRun"`
	// FilesChecked is the number of records whose file was stat'ed (by the attempt that planned).
	FilesChecked int64 `json:"filesChecked"`
	FilesPlanned int64 `json:"filesPlanned"`
	// FilesVerified were re-read and matched.
	FilesVerified int64 `json:"filesVerified"`
	// FilesMissing were missing, short or mismatching (their records are now missing).
	FilesMissing int64 `json:"filesMissing"`
	// FilesFailed could not be checked (e.g. unreadable).
	FilesFailed    int64 `json:"filesFailed"`
	FilesSkipped   int64 `json:"filesSkipped"`
	HashesRecorded int64 `json:"hashesRecorded"`
	BytesVerified  int64 `json:"bytesVerified"`
	DurationMs     int64 `json:"durationMs"`
}

// Verify item checks (Detail.Check).
const (
	checkStat = "stat"
	checkHash = "hash"
)

// Run implements jobs.Runner.
func (r *VerifyRunner) Run(ctx context.Context, job jobs.Job, env jobs.Env) (jobs.Result, error) {
	started := r.now()
	rep := reporterOf(env)
	if env.Items == nil {
		return jobs.Result{}, errors.New("verify: the job has no item store")
	}
	h, err := openEnabled(ctx, r.dests, job.Params.DestinationID, "verify")
	if err != nil {
		return jobs.Result{}, err
	}
	defer h.Close()
	v := &verifyRun{r: r, job: job, env: env, rep: rep, h: h}
	rep.Log(slog.LevelInfo, "verify started", "destination", h.Destination.Name, "mode", string(h.Settings.Verify.Mode),
		"samplePercent", h.Settings.Verify.SamplePercent, "attempt", job.Attempt)
	if err := prepareIdentity(ctx, h, rep, job.DryRun, &v.unsettled); err != nil {
		return jobs.Result{}, err
	}
	if !job.DryRun {
		// A file a stopped sync left in retention goes back before its record is checked.
		n, err := reconcile(ctx, newIntentFixer(r.base, h, rep, job.ID), env.Items, "")
		v.unsettled += n
		if err != nil {
			if ctx.Err() != nil {
				return jobs.Result{}, ctx.Err()
			}
			return jobs.Result{}, fmt.Errorf("verify: %w", err)
		}
	}
	planned, err := env.Items.Planned(ctx, job.ID)
	if err != nil {
		return jobs.Result{}, err
	}
	if !planned {
		if err := env.Items.DeleteItems(ctx, job.ID); err != nil {
			return jobs.Result{}, err
		}
		if err := v.plan(ctx); err != nil {
			if ctx.Err() != nil {
				return jobs.Result{}, ctx.Err()
			}
			return jobs.Result{}, err
		}
	}
	if !job.DryRun {
		if err := v.execute(ctx); err != nil {
			if ctx.Err() != nil {
				return jobs.Result{}, ctx.Err()
			}
			return jobs.Result{}, err
		}
	}
	return v.result(ctx, started)
}

// openEnabled loads a destination, refuses a disabled one and opens it (S3 checks).
func openEnabled(ctx context.Context, dests *destinations.Store, id int64, what string) (*destinations.Handle, error) {
	if id == 0 {
		return nil, fmt.Errorf("%s: the job has no destination", what)
	}
	d, err := dests.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	if !d.Enabled {
		return nil, fmt.Errorf("%s: destination %q is disabled", what, d.Name)
	}
	h, err := dests.Open(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return h, nil
}

type verifyRun struct {
	r   *VerifyRunner
	job jobs.Job
	env jobs.Env
	rep jobs.Reporter
	h   *destinations.Handle

	checked, hashesRecorded int64
	// unchecked counts the items this attempt failed without marking their record (unreadable).
	unchecked int64
	// unsettled counts what a stopped sync left in retention that could not be settled (reconcile),
	// and a failed check of the destination's inode numbers (prepareIdentity).
	unsettled int
	progress  jobs.Progress
}

// plan stats every live file record and persists an item for each problem and each sampled file.
func (v *verifyRun) plan(ctx context.Context) error {
	destID := v.h.Destination.ID
	set := v.h.Settings.Verify
	var total int64
	err := v.r.store.db.Reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM destination_files
		WHERE destination_id = ? AND state IN ('present', 'linked')`, destID).Scan(&total)
	if err != nil {
		return fmt.Errorf("count destination files: %w", err)
	}
	var n int64
	switch set.Mode {
	case destinations.VerifyFull:
		n = total
	case destinations.VerifySample:
		pct := int64(set.SamplePercent)
		if pct <= 0 {
			pct = destinations.DefaultSamplePercent
		}
		n = (total*pct + 99) / 100
	}
	sample := map[int64]bool{}
	if n > 0 {
		rows, err := v.r.store.db.Reader().QueryContext(ctx, `SELECT id FROM destination_files
			WHERE destination_id = ? AND state IN ('present', 'linked')
			ORDER BY verified_at IS NOT NULL, verified_at, id LIMIT ?`, destID, n)
		if err != nil {
			return fmt.Errorf("choose the verify sample: %w", err)
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return fmt.Errorf("choose the verify sample: %w", err)
			}
			sample[id] = true
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return fmt.Errorf("choose the verify sample: %w", err)
		}
	}
	v.progress = jobs.Progress{Phase: "planning", FilesTotal: total}
	var batch []jobs.Item
	flush := func(final bool) error {
		if err := v.env.Items.AddItems(ctx, v.job.ID, batch, final); err != nil {
			return err
		}
		batch = batch[:0]
		if !final {
			faultinject.Point(PointPlanAfterBatch)
		}
		return nil
	}
	sinceRecheck := 0
	err = v.r.store.eachLive(ctx, destID, func(rec Record) error {
		if rec.State != StatePresent && rec.State != StateLinked {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if sinceRecheck++; sinceRecheck >= v.r.recheckEvery {
			sinceRecheck = 0
			if err := v.h.Recheck(); err != nil {
				return err
			}
		}
		v.checked++
		v.progress.FilesDone = v.checked
		v.progress.CurrentFile = rec.RelPath
		v.rep.Progress(v.progress)
		d := Detail{SourceID: rec.SourceID, Source: rec.SourceRelPath, RecordID: rec.ID, Size: rec.Size, MtimeNs: rec.MtimeNs}
		problem, err := statProblem(v.h, rec)
		if err != nil {
			return err
		}
		switch {
		case problem != "":
			d.Check, d.Reason = checkStat, problem
		case sample[rec.ID]:
			d.Check = checkHash
		default:
			return nil
		}
		batch = append(batch, jobs.Item{FileID: rec.ID, RelPath: rec.RelPath, Action: jobs.ActionVerify, Bytes: rec.Size, Detail: d.raw()})
		if len(batch) >= v.r.planBatch {
			return flush(false)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return flush(true)
}

// statProblem returns why a record's file fails the cheap check ("" when it passes). Errors other
// than "not there" are returned.
func statProblem(h *destinations.Handle, rec Record) (string, error) {
	st, err := filecopy.Lstat(h.Root, rec.RelPath)
	switch {
	case notExist(err):
		return "missing", nil
	case err != nil:
		return "", sideErr(filecopy.SideDestination, "stat", rec.RelPath, err)
	case !st.Regular():
		return fmt.Sprintf("not a regular file (%s)", st.Mode.Type()), nil
	case st.Size != rec.Size:
		return fmt.Sprintf("size %d, recorded %d", st.Size, rec.Size), nil
	}
	return "", nil
}

// execute checks the pending items.
func (v *verifyRun) execute(ctx context.Context) error {
	v.progress = jobs.Progress{Phase: "verifying"}
	counts, err := v.env.Items.Counts(ctx, v.job.ID)
	if err != nil {
		return err
	}
	for _, c := range counts {
		if c.Status == jobs.ItemPending {
			v.progress.FilesTotal += c.Files
			v.progress.BytesTotal += c.Bytes
		}
	}
	sinceRecheck := 0
	after := int64(0)
	for {
		batch, err := v.env.Items.Pending(ctx, v.job.ID, after, pendingBatch)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		for _, it := range batch {
			after = it.ID
			if err := ctx.Err(); err != nil {
				return err
			}
			if it.Action != jobs.ActionVerify {
				continue
			}
			if err := v.verifyItem(ctx, it); err != nil {
				return err
			}
			v.progress.FilesDone++
			v.rep.Progress(v.progress)
			if sinceRecheck++; sinceRecheck >= v.r.recheckEvery {
				sinceRecheck = 0
				if err := v.h.Recheck(); err != nil {
					return err
				}
			}
		}
	}
}

func (v *verifyRun) verifyItem(ctx context.Context, it jobs.Item) error {
	wctx := context.WithoutCancel(ctx)
	d, err := parseDetail(it)
	if err != nil {
		v.unchecked++ // nothing was marked: it could not be checked
		return v.env.Items.Finish(wctx, it.ID, jobs.ItemFailed, 0, err.Error())
	}
	v.progress.CurrentFile = it.RelPath
	rec, err := v.r.store.Get(ctx, d.RecordID)
	if errors.Is(err, ErrNotFound) {
		return v.env.Items.Finish(wctx, it.ID, jobs.ItemSkipped, 0, "no longer recorded")
	}
	if err != nil {
		return err
	}
	if rec.State == StateMissing {
		// Marked by an earlier attempt of this job (its item finish did not happen, and it may have
		// stopped before marking the primary that shares a damaged hardlink's inode).
		if err := v.markSharedInode(ctx, rec); err != nil {
			return err
		}
		return v.env.Items.Finish(wctx, it.ID, jobs.ItemFailed, 0, "missing or damaged at the destination; the next sync copies it again")
	}
	if (rec.State != StatePresent && rec.State != StateLinked) || rec.RelPath != it.RelPath {
		return v.env.Items.Finish(wctx, it.ID, jobs.ItemSkipped, 0, "the record changed since planning")
	}
	var problem string
	switch d.Check {
	case checkStat:
		p, err := statProblem(v.h, rec)
		if err != nil {
			return v.itemError(ctx, it, err)
		}
		if p == "" {
			return v.env.Items.Finish(wctx, it.ID, jobs.ItemDone, 0, "") // it is back: nothing to mark
		}
		problem = p
	default:
		got, err := filecopy.VerifyFile(ctx, v.h.Root, rec.RelPath, rec.Size, rec.Hash, func(n int64) {
			v.progress.BytesDone += n
			v.rep.Progress(v.progress)
		})
		switch {
		case err == nil:
			if rec.Hash == "" {
				v.hashesRecorded++
			}
			if err := v.r.store.setVerified(wctx, rec.ID, got, v.r.now()); err != nil {
				return err
			}
			return v.env.Items.Finish(wctx, it.ID, jobs.ItemDone, rec.Size, "")
		case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR), errors.Is(err, filecopy.ErrMismatch), errors.Is(err, filecopy.ErrNotRegular):
			problem = err.Error()
		default:
			return v.itemError(ctx, it, err)
		}
	}
	if err := v.r.store.markMissing(wctx, rec.ID); err != nil {
		return err
	}
	if err := v.markSharedInode(ctx, rec); err != nil {
		return err
	}
	faultinject.Point(PointVerifyAfterMark)
	msg := fmt.Sprintf("%s: %s; the record is marked missing and the next sync copies it again", rec.RelPath, problem)
	v.rep.Log(slog.LevelWarn, "verify failed", "path", rec.RelPath, "problem", problem)
	return v.env.Items.Finish(wctx, it.ID, jobs.ItemFailed, 0, msg)
}

// markSharedInode marks the primary of a damaged hardlink (linked, or already marked missing) missing
// too when both names are still one file at the destination (sameDestFile: where inode numbers do
// not identify files, when the primary holds the same content): the damage is in the content they
// share. (A damaged primary's hardlinks are relinked by the next sync's repair.) Only comparing the
// names stops with ctx.
func (v *verifyRun) markSharedInode(ctx context.Context, rec Record) error {
	if (rec.State != StateLinked && rec.State != StateMissing) || rec.LinkOf == 0 {
		return nil
	}
	wctx := context.WithoutCancel(ctx)
	prim, err := v.r.store.Get(wctx, rec.LinkOf)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if prim.State != StatePresent {
		return nil
	}
	a, aerr := v.h.Lstat(rec.RelPath)
	b, berr := v.h.Lstat(prim.RelPath)
	if aerr != nil || berr != nil {
		return nil
	}
	if same, err := sameDestFile(ctx, v.h, rec.RelPath, a, prim.RelPath, b); err != nil {
		if ctx.Err() != nil || filecopy.Classify(err) == filecopy.Fatal {
			return err
		}
		v.rep.Log(slog.LevelInfo, "could not compare a damaged hardlink with its primary; the primary is checked on its own",
			"path", prim.RelPath, "hardlink", rec.RelPath, "error", err.Error())
		return nil
	} else if !same {
		return nil
	}
	v.rep.Log(slog.LevelWarn, "verify failed on a hardlink: its primary shares the damaged content", "path", prim.RelPath, "hardlink", rec.RelPath)
	return v.r.store.markMissing(wctx, prim.ID)
}

// itemError fails the item unless err is fatal (then it stops the job).
func (v *verifyRun) itemError(ctx context.Context, it jobs.Item, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if filecopy.Classify(err) == filecopy.Fatal {
		return fmt.Errorf("verify %s: %w", it.RelPath, err)
	}
	v.rep.Log(slog.LevelWarn, "could not verify", "path", it.RelPath, "error", err.Error())
	v.unchecked++
	return v.env.Items.Finish(context.WithoutCancel(ctx), it.ID, jobs.ItemFailed, 0, err.Error())
}

func (v *verifyRun) result(ctx context.Context, started time.Time) (jobs.Result, error) {
	counts, err := v.env.Items.Counts(context.WithoutCancel(ctx), v.job.ID)
	if err != nil {
		return jobs.Result{}, err
	}
	st := VerifyStats{DryRun: v.job.DryRun, FilesChecked: v.checked, HashesRecorded: v.hashesRecorded}
	for _, c := range counts {
		st.FilesPlanned += c.Files
		switch c.Status {
		case jobs.ItemDone:
			st.FilesVerified += c.Files
			st.BytesVerified += c.Bytes
		case jobs.ItemFailed:
			st.FilesMissing += c.Files
		case jobs.ItemSkipped:
			st.FilesSkipped += c.Files
		}
	}
	// Failed items are missing or damaged (their record is marked) unless they could not be read:
	// this attempt counted those (an earlier attempt's are counted as missing).
	st.FilesFailed = min(v.unchecked, st.FilesMissing)
	st.FilesMissing -= st.FilesFailed
	st.DurationMs = v.r.now().Sub(started).Milliseconds()
	var summary string
	switch {
	case v.job.DryRun:
		summary = fmt.Sprintf("Dry run: would verify %s", plural(st.FilesPlanned, "file", "files"))
	case st.FilesMissing > 0:
		summary = fmt.Sprintf("Verified %s (%s); %d missing or damaged, marked for the next sync", plural(st.FilesVerified, "file", "files"),
			formatBytes(st.BytesVerified), st.FilesMissing)
	case st.FilesFailed == 0:
		summary = fmt.Sprintf("Verified %s (%s): all ok", plural(st.FilesVerified, "file", "files"), formatBytes(st.BytesVerified))
	default:
		summary = fmt.Sprintf("Verified %s (%s)", plural(st.FilesVerified, "file", "files"), formatBytes(st.BytesVerified))
	}
	if st.FilesFailed > 0 {
		summary += fmt.Sprintf("; %d could not be checked", st.FilesFailed)
	}
	if st.FilesChecked > 0 {
		summary += fmt.Sprintf("; %d checked for size", st.FilesChecked)
	}
	return jobs.Result{Stats: st, Warnings: int(st.FilesMissing+st.FilesFailed) + v.unsettled, Summary: summary}, nil
}
