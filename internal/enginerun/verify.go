package enginerun

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/engines/rclone"
	"github.com/sl0wz3r/bunkarr/internal/engines/restic"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/syncer"
)

// Verify of engine destinations (§6.6, §7.6). The plan is persisted as verify items: restic's
// repository check, one listing per source (rclone), the content sample, and the config versions
// (rclone). The content sample takes the least recently verified records (missing ones first: a
// later verify tries them again) whose source still has the recorded size and mtime, up to
// samplePercent of the files and verify.sampleMaxBytes (params.readData: every file on rclone).
// A match sets verified_at and records the sha256; a mismatch marks the record missing, fails
// the item and counts a warning (the job's notification).

// runVerify runs a verify job of one restic or rclone destination.
func (s *Service) runVerify(ctx context.Context, kind engines.Kind, job jobs.Job, env jobs.Env) (jobs.Result, error) {
	r, err := s.prepare(ctx, kind, job, env, "verify")
	if err != nil {
		return jobs.Result{}, err
	}
	var res jobs.Result
	if kind == engines.Restic {
		res, err = s.resticVerify(ctx, r)
	} else {
		res, err = s.rcloneVerify(ctx, r)
	}
	return res, r.redactErr(err)
}

// verifyRun is one verify attempt.
type verifyRun struct {
	*jobRun
	roots   map[int64]*os.Root
	sources map[int64]catalog.Source
	stats   VerifyStats
}

func (s *Service) newVerifyRun(ctx context.Context, r *jobRun) (*verifyRun, func(), error) {
	v := &verifyRun{jobRun: r, roots: map[int64]*os.Root{}, sources: map[int64]catalog.Source{}}
	v.stats.Engine = string(r.kind)
	v.stats.DryRun = r.job.DryRun
	closeAll := func() {
		for _, root := range v.roots {
			_ = root.Close()
		}
	}
	for _, id := range r.d.SourceIDs {
		src, err := s.o.Catalog.Get(ctx, id)
		if errors.Is(err, catalog.ErrNotFound) {
			continue
		}
		if err != nil {
			closeAll()
			return nil, nil, err
		}
		if !src.Enabled {
			continue
		}
		v.sources[id] = src
		if root, err := os.OpenRoot(src.Path); err == nil {
			v.roots[id] = root
		} else {
			r.log(slog.LevelWarn, "source not readable: its files are not sampled", "source", src.Name, "error", err.Error())
		}
	}
	return v, closeAll, nil
}

// sampleLimits returns how many files and bytes the content sample takes of files present ones.
func (v *verifyRun) sampleLimits(present int64) (int64, int64) {
	st := v.d.Settings.Verify
	if v.job.Params.ReadData && v.kind == engines.Rclone {
		return present, math.MaxInt64
	}
	pct := st.SamplePercent
	if pct <= 0 {
		pct = destinations.DefaultSamplePercent
	}
	n := int64(math.Ceil(float64(present) * float64(pct) / 100))
	maxBytes := st.SampleMaxBytes
	if maxBytes <= 0 {
		maxBytes = destinations.DefaultResticSampleBytes
		if v.kind == engines.Rclone {
			maxBytes = destinations.DefaultRcloneSampleBytes
		}
	}
	return n, maxBytes
}

// planSample returns the content sample's items.
func (v *verifyRun) planSample(ctx context.Context) ([]jobs.Item, error) {
	if v.d.Settings.Verify.Mode == destinations.VerifyOff && !v.job.Params.ReadData {
		return nil, nil
	}
	baseOnly := v.kind == engines.Restic
	present, _, err := v.s.files.CountPresent(ctx, v.d.ID, baseOnly)
	if err != nil {
		return nil, err
	}
	limit, maxBytes := v.sampleLimits(present)
	if limit <= 0 {
		return nil, nil
	}
	cands, err := v.s.files.VerifyCandidates(ctx, v.d.ID, baseOnly, true, int(min(limit*4+100, math.MaxInt32)))
	if err != nil {
		return nil, err
	}
	var items []jobs.Item
	var n, total int64
	for _, rec := range cands {
		if n >= limit {
			break
		}
		root := v.roots[rec.SourceID]
		if root == nil {
			continue
		}
		st, err := filecopy.Lstat(root, rec.SourceRelPath)
		if err != nil || !st.Regular() || st.Size != rec.Size || st.MtimeNs != rec.MtimeNs {
			continue // the source changed since the backup: its content cannot be compared
		}
		if total+rec.Size > maxBytes {
			continue
		}
		d := engineDetail{Detail: syncer.Detail{SourceID: rec.SourceID, Source: rec.SourceRelPath, RecordID: rec.ID, Size: rec.Size,
			MtimeNs: rec.MtimeNs, Check: checkSample}}
		if rec.State == syncer.StateMissing {
			d.Reason = reasonRetry
		}
		items = append(items, jobs.Item{FileID: rec.ID, RelPath: rec.RelPath, Action: jobs.ActionVerify, Bytes: rec.Size, Detail: d.raw()})
		n++
		total += rec.Size
	}
	return items, nil
}

// markSample records one sample item's result: match sets verified_at and the hash (and a
// missing record present again); a mismatch marks the record missing and fails the item.
func (v *verifyRun) markSample(ctx context.Context, it jobs.Item, d engineDetail, match bool, hash, why string) error {
	rec, err := v.s.files.Get(ctx, d.RecordID)
	if errors.Is(err, syncer.ErrNotFound) {
		return v.finish(ctx, it.ID, jobs.ItemSkipped, 0, "the record is gone")
	}
	if err != nil {
		return err
	}
	if !rec.State.Live() || rec.Size != d.Size || rec.MtimeNs != d.MtimeNs {
		return v.finish(ctx, it.ID, jobs.ItemSkipped, 0, "the record changed since planning")
	}
	now := v.s.now()
	err = v.s.o.DB.Write(context.WithoutCancel(ctx), func(tx *sql.Tx) error {
		if !match {
			return v.s.files.MarkMissingTx(ctx, tx, rec.ID)
		}
		if err := v.s.files.SetVerifiedTx(ctx, tx, rec.ID, hash, now); err != nil {
			return err
		}
		return v.s.files.RestorePresentTx(ctx, tx, rec.ID)
	})
	if err != nil {
		return err
	}
	if !match {
		v.stats.FilesMissing++
		v.log(slog.LevelWarn, "a backed-up file is damaged or missing", "path", it.RelPath, "reason", why)
		return v.failItem(ctx, it, why)
	}
	v.stats.FilesVerified++
	v.stats.BytesVerified += rec.Size
	if rec.Hash == "" {
		v.stats.HashesRecorded++
	}
	return v.finish(ctx, it.ID, jobs.ItemDone, rec.Size, "")
}

// sourceHash hashes a source file after checking it still has the sampled version.
func (v *verifyRun) sourceHash(ctx context.Context, d engineDetail) (string, bool, error) {
	root := v.roots[d.SourceID]
	if root == nil {
		return "", false, nil
	}
	st, err := filecopy.Lstat(root, d.Source)
	if err != nil || st.Size != d.Size || st.MtimeNs != d.MtimeNs {
		return "", false, nil
	}
	h, _, err := filecopy.HashFile(ctx, root, d.Source, nil)
	if err != nil {
		return "", false, nil
	}
	return h, true, nil
}

// verifyResult completes the stats from the job's items and the attempt's tallies.
func (v *verifyRun) verifyResult(ctx context.Context) (jobs.Result, error) {
	items, ok, err := v.allItems(ctx, jobs.ActionVerify)
	if err != nil {
		return jobs.Result{}, err
	}
	st := v.stats
	if ok {
		st.FilesPlanned, st.SampleFiles, st.SampleBytes, st.FilesFailed, st.FilesSkipped = 0, 0, 0, 0, 0
		for _, it := range items {
			d, _ := parseItem(it)
			if d.Check == checkSample {
				st.FilesPlanned++
				st.SampleFiles++
				st.SampleBytes += it.Bytes
			}
			switch it.Status {
			case jobs.ItemFailed:
				st.FilesFailed++
			case jobs.ItemSkipped:
				st.FilesSkipped++
			}
		}
	}
	st.DurationMs = v.s.now().Sub(v.started).Milliseconds()
	warnings := v.warnings + int(st.FilesFailed)
	summary := fmt.Sprintf("Verified %d files at %s", st.FilesVerified, v.d.Name)
	if st.DryRun {
		summary = fmt.Sprintf("Would check %d files at %s", st.SampleFiles, v.d.Name)
	}
	if st.Check != nil {
		summary += fmt.Sprintf("; repository check: %d errors", st.Check.NumErrors)
	}
	if st.FilesMissing > 0 {
		summary += fmt.Sprintf("; %d damaged or missing", st.FilesMissing)
	}
	return jobs.Result{Stats: st, Warnings: warnings, Summary: summary}, nil
}

// reasonRetry marks a sample item whose record was missing at planning (a later verify tries it
// again first).
const reasonRetry = "retry"

// markedThisJob reports a sample whose record was present at planning and is missing now: the
// listing of this job found its object gone or resized, so its content is not compared again.
func (v *verifyRun) markedThisJob(ctx context.Context, d engineDetail) (bool, error) {
	if d.Reason == reasonRetry {
		return false, nil
	}
	rec, err := v.s.files.Get(ctx, d.RecordID)
	if errors.Is(err, syncer.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return rec.State == syncer.StateMissing, nil
}

// --- restic (§6.6) ---

func (s *Service) resticVerify(ctx context.Context, r *jobRun) (jobs.Result, error) {
	repo, err := s.o.Restic.Connect(r.ed, r.sec, r.rt)
	if err != nil {
		return jobs.Result{}, fmt.Errorf("verify: %w", err)
	}
	if err := repo.CheckIdentity(ctx); err != nil {
		return jobs.Result{}, fmt.Errorf("verify: %w", err)
	}
	v, closeRoots, err := s.newVerifyRun(ctx, r)
	if err != nil {
		return jobs.Result{}, err
	}
	defer closeRoots()
	r.log(slog.LevelInfo, "verify started", "destination", r.d.Name, "engine", "restic", "mode", string(r.d.Settings.Verify.Mode),
		"readData", r.job.Params.ReadData)
	planned, err := r.env.Items.Planned(ctx, r.job.ID)
	if err != nil {
		return jobs.Result{}, err
	}
	if !planned {
		if err := r.env.Items.DeleteItems(ctx, r.job.ID); err != nil {
			return jobs.Result{}, err
		}
		check := engineDetail{Detail: syncer.Detail{Check: checkRepository}}
		items := []jobs.Item{{RelPath: "repository", Action: jobs.ActionVerify, Detail: check.raw()}}
		sample, err := v.planSample(ctx)
		if err != nil {
			return jobs.Result{}, err
		}
		v.stats.FilesChecked = int64(len(sample))
		if err := r.addItems(ctx, append(items, sample...), true); err != nil {
			return jobs.Result{}, err
		}
	}
	if !r.job.DryRun {
		x := &resticRun{jobRun: r, repo: repo}
		if err := v.resticExecute(ctx, x); err != nil {
			return jobs.Result{}, err
		}
	}
	return v.verifyResult(ctx)
}

func (v *verifyRun) resticExecute(ctx context.Context, x *resticRun) error {
	items, err := v.pending(ctx)
	if err != nil {
		return err
	}
	samples := map[int64][]jobs.Item{}
	for _, it := range items {
		d, err := parseItem(it)
		if err != nil {
			continue
		}
		switch d.Check {
		case checkRepository:
			if err := v.resticCheck(ctx, x, it); err != nil {
				return err
			}
		case checkSample:
			samples[d.SourceID] = append(samples[d.SourceID], it)
		}
	}
	for id, list := range samples {
		if err := ctx.Err(); err != nil {
			return err
		}
		if now := v.s.now(); !v.open(now) {
			return v.deferral(now)
		}
		if err := v.resticSample(ctx, x, id, list); err != nil {
			return err
		}
	}
	return nil
}

// resticCheck runs restic check with the rotating read-data subset (§6.6): t = ceil(100 /
// samplePercent), n rotates 1..t; full mode or params.readData read everything; off checks the
// structure only. check takes an exclusive lock, so the guarded unlock runs right before it.
func (v *verifyRun) resticCheck(ctx context.Context, x *resticRun, it jobs.Item) error {
	now := v.s.now()
	if !v.open(now) {
		return v.deferral(now)
	}
	state, err := v.s.State(ctx, v.d.ID)
	if err != nil {
		return err
	}
	var args restic.CheckArgs
	stat := &CheckStat{}
	subsetOf := 0
	switch {
	case v.job.Params.ReadData || v.d.Settings.Verify.Mode == destinations.VerifyFull:
		args.ReadData, stat.ReadSubset = true, "all"
	case v.d.Settings.Verify.Mode == destinations.VerifyOff:
	default:
		pct := v.d.Settings.Verify.SamplePercent
		if pct <= 0 {
			pct = destinations.DefaultSamplePercent
		}
		subsetOf = int(math.Ceil(100 / float64(pct)))
		n := state.ReadSubsetNext
		if n < 1 || n > subsetOf {
			n = 1
		}
		args.Subset = strconv.Itoa(n) + "/" + strconv.Itoa(subsetOf)
		stat.ReadSubset = args.Subset
	}
	if err := x.repo.GuardedUnlock(ctx, v.rt.HostName, v.rt.ProcessStart, nil); err != nil {
		return err
	}
	stop := v.stopAtEnd(now, false)
	defer stop.stop()
	args.Interrupt = stop.C
	res, err := x.repo.Check(ctx, args)
	if err != nil {
		x.releaseLock(ctx, err)
		if stop.fired() || errors.Is(err, restic.ErrInterrupted) {
			v.log(slog.LevelInfo, "the transfer window closed during the check; it runs again in the next window")
			return v.deferral(v.s.now())
		}
		return fmt.Errorf("restic check: %w", err)
	}
	stat.NumErrors = res.NumErrors
	v.stats.Check = stat
	checked := v.s.now()
	if err := v.s.updateState(ctx, v.d.ID, func(st *EngineState) {
		st.LastCheckAt = &checked
		if subsetOf > 0 {
			st.ReadSubsetNext = state.ReadSubsetNext%subsetOf + 1
		}
		st.setStat("checkErrors", res.NumErrors)
	}); err != nil {
		return err
	}
	if res.NumErrors > 0 {
		msg := fmt.Sprintf("restic check found %d errors (see the README's repair procedure)", res.NumErrors)
		v.log(slog.LevelWarn, msg)
		return v.finish(ctx, it.ID, jobs.ItemFailed, 0, msg)
	}
	return v.finish(ctx, it.ID, jobs.ItemDone, 0, "")
}

// resticSample restores one source's sample from its base into <config>/staging/verify-job<id>
// and compares each file's sha256 with the source's (§6.6).
func (v *verifyRun) resticSample(ctx context.Context, x *resticRun, sourceID int64, items []jobs.Item) error {
	src, ok := v.sources[sourceID]
	base, err := v.s.baseOf(ctx, v.d.ID, sourceID)
	if err != nil {
		return err
	}
	if !ok || base == "" {
		for _, it := range items {
			if err := v.finish(ctx, it.ID, jobs.ItemSkipped, 0, "the source has no snapshot to check"); err != nil {
				return err
			}
		}
		return nil
	}
	staging := filepath.Join(v.s.stagingDir(), fmt.Sprintf("verify-job%d", v.job.ID), strconv.FormatInt(sourceID, 10))
	if err := os.RemoveAll(staging); err != nil {
		return err
	}
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(filepath.Dir(staging)) }()
	var include []string
	var sampled []jobs.Item
	for _, it := range items {
		d, _ := parseItem(it)
		if strings.ContainsAny(d.Source, "\n\r") {
			// restic's include file cannot name it (a line break): it is not sampled.
			if err := v.finish(ctx, it.ID, jobs.ItemSkipped, 0, "the name contains a line break, which restic's include file cannot carry"); err != nil {
				return err
			}
			continue
		}
		include = append(include, d.Source)
		sampled = append(sampled, it)
	}
	if items = sampled; len(items) == 0 {
		return nil
	}
	// The sample is restored into the config directory (often the appdata pool Bunkarr's database
	// and Plex share): only what fits its free space is restored, and the rest waits for a later
	// verify (it stays among the least recently verified).
	if free, ok := stagingFreeSpace(staging); ok {
		var fits []jobs.Item
		include = include[:0]
		var total int64
		for _, it := range items {
			d, _ := parseItem(it)
			if total+d.Size+stagingMargin > free {
				if err := v.finish(ctx, it.ID, jobs.ItemSkipped, 0, "not enough free space in the config directory to restore it for the check"); err != nil {
					return err
				}
				continue
			}
			total += d.Size
			fits = append(fits, it)
			include = append(include, d.Source)
		}
		if len(fits) < len(items) {
			v.warn("the verify sample does not fit the config directory's free space; files were left out", "left out", len(items)-len(fits),
				"free", formatBytes(free))
		}
		if items = fits; len(items) == 0 {
			return nil
		}
	}
	partial := false
	if err := x.repo.Restore(ctx, restic.RestoreArgs{Snapshot: base, Subpath: filepath.Clean(src.Path), Target: filepath.Clean(staging),
		Include: include}); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !partialRestore(err) {
			return fmt.Errorf("restore the verify sample: %w", err)
		}
		if free, ok := stagingFreeSpace(staging); ok && free < stagingMargin {
			// The config directory filled up during the restore: the files that did not restore say
			// nothing about the repository, so nothing is marked missing.
			return fmt.Errorf("restore the verify sample: the config directory ran out of free space (%s free): %w", formatBytes(free), err)
		}
		// Some files did not restore (a damaged pack holds their data): they fail their comparison
		// below and their records become missing, so the next sync uploads them again (§6.6).
		partial = true
		v.log(slog.LevelWarn, "the verify sample did not restore completely; the files that did not restore are damaged in the repository",
			"error", err.Error())
	}
	restored, err := os.OpenRoot(staging)
	if err != nil {
		return err
	}
	defer restored.Close()
	for _, it := range items {
		d, _ := parseItem(it)
		want, ok, err := v.sourceHash(ctx, d)
		if err != nil {
			return err
		}
		if !ok {
			if err := v.finish(ctx, it.ID, jobs.ItemSkipped, 0, "the source changed since planning"); err != nil {
				return err
			}
			continue
		}
		got, _, herr := filecopy.HashFile(ctx, restored, d.Source, nil)
		switch {
		case herr != nil && partial:
			err = v.markSample(ctx, it, d, false, "", "could not be restored from the repository (damaged data)")
		case herr != nil:
			err = v.markSample(ctx, it, d, false, "", "not in the snapshot it is recorded in")
		case got != want:
			err = v.markSample(ctx, it, d, false, "", "content differs from the source")
		default:
			err = v.markSample(ctx, it, d, true, got, "")
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// stagingMargin is the free space a verify leaves in the config directory beside its staging.
const stagingMargin = 16 << 20

// stagingFreeSpace returns the free space of the filesystem that holds dir (ok false: unknown). A
// variable, so tests can shrink it.
var stagingFreeSpace = func(dir string) (int64, bool) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return 0, false
	}
	defer root.Close()
	free, _, err := filecopy.FreeSpace(root)
	if err != nil {
		return 0, false
	}
	return int64(min(free, uint64(math.MaxInt64))), true
}

// restoreErrorsRe is the exit message of a restic restore that restored what it could but met
// errors on some files ("There were 2 errors").
var restoreErrorsRe = regexp.MustCompile(`There were [0-9]+ errors?`)

// partialRestore reports whether a restore failed only on some of its files: exit 1 with restic's
// "There were N errors" (as when a damaged pack holds data of a sampled file, §20.4). Any other
// failure (the repository unreachable, the wrong password, a lock) fails the job instead, so an
// unreadable repository never marks the sampled records missing.
func partialRestore(err error) bool {
	var re *restic.Error
	return errors.As(err, &re) && errors.Is(re.Err, restic.ErrFailed) && re.Code == 1 && restoreErrorsRe.MatchString(re.Message)
}

// --- rclone (§7.6) ---

func (s *Service) rcloneVerify(ctx context.Context, r *jobRun) (jobs.Result, error) {
	conn, err := s.o.Rclone.Connect(r.ed, r.sec, r.rt)
	if err != nil {
		return jobs.Result{}, fmt.Errorf("verify: %w", err)
	}
	if _, err := conn.CheckMarker(ctx); err != nil {
		return jobs.Result{}, fmt.Errorf("verify: %w", err)
	}
	v, closeRoots, err := s.newVerifyRun(ctx, r)
	if err != nil {
		return jobs.Result{}, err
	}
	defer closeRoots()
	x := &rcloneRun{jobRun: r, conn: conn, caps: rclone.Capabilities(r.ed), touched: map[string]bool{}}
	r.log(slog.LevelInfo, "verify started", "destination", r.d.Name, "engine", "rclone", "mode", string(r.d.Settings.Verify.Mode),
		"readData", r.job.Params.ReadData)
	if !r.job.DryRun {
		if err := x.reconcile(ctx); err != nil {
			return jobs.Result{}, fmt.Errorf("verify: %w", err)
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
		var items []jobs.Item
		for _, id := range sortedIDs(v.sources) {
			src := v.sources[id]
			d := engineDetail{Detail: syncer.Detail{SourceID: id, Check: checkListing}}
			items = append(items, jobs.Item{RelPath: src.DestFolder, Action: jobs.ActionVerify, Detail: d.raw()})
		}
		sample, err := v.planSample(ctx)
		if err != nil {
			return jobs.Result{}, err
		}
		items = append(items, sample...)
		for _, c := range configFolders {
			d := engineDetail{Detail: syncer.Detail{Check: checkVersion, Reason: c.kind}}
			items = append(items, jobs.Item{RelPath: c.folder, Action: jobs.ActionVerify, Detail: d.raw()})
		}
		v.stats.FilesChecked = int64(len(sample))
		if err := r.addItems(ctx, items, true); err != nil {
			return jobs.Result{}, err
		}
	}
	if !r.job.DryRun {
		if err := v.rcloneExecute(ctx, x); err != nil {
			return jobs.Result{}, err
		}
	}
	return v.verifyResult(ctx)
}

func sortedIDs[V any](m map[int64]V) []int64 {
	out := make([]int64, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func (v *verifyRun) rcloneExecute(ctx context.Context, x *rcloneRun) error {
	items, err := v.pending(ctx)
	if err != nil {
		return err
	}
	samples := map[int64][]jobs.Item{}
	var versions []jobs.Item
	for _, it := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		if now := v.s.now(); !v.open(now) {
			return v.deferral(now)
		}
		d, err := parseItem(it)
		if err != nil {
			continue
		}
		switch d.Check {
		case checkListing:
			if err := v.rcloneListing(ctx, x, it, d); err != nil {
				return err
			}
		case checkSample:
			samples[d.SourceID] = append(samples[d.SourceID], it)
		case checkVersion:
			versions = append(versions, it)
		}
	}
	for _, id := range sortedIDs(samples) {
		if now := v.s.now(); !v.open(now) {
			return v.deferral(now)
		}
		if err := v.rcloneSample(ctx, x, id, samples[id]); err != nil {
			return err
		}
	}
	for _, it := range versions {
		if now := v.s.now(); !v.open(now) {
			return v.deferral(now)
		}
		if err := v.rcloneVersions(ctx, x, it); err != nil {
			return err
		}
	}
	return nil
}

// rcloneListing compares one destination folder's listing with the live records (§7.6): a missing
// object, or one of another size, marks its record missing; the next sync repairs it.
func (v *verifyRun) rcloneListing(ctx context.Context, x *rcloneRun, it jobs.Item, d engineDetail) error {
	src, ok := v.sources[d.SourceID]
	if !ok {
		return v.finish(ctx, it.ID, jobs.ItemSkipped, 0, "the source is no longer synced to this destination")
	}
	objects := map[string]int64{}
	err := x.conn.LsJSON(ctx, src.DestFolder, true, func(o rclone.Object) error {
		objects[o.Path] = o.Size
		return nil
	})
	var re *rclone.Error
	if err != nil && !(errors.As(err, &re) && errors.Is(re.Err, rclone.ErrPathNotFound)) {
		return err
	}
	recs, err := v.s.files.LiveForSource(ctx, v.d.ID, src.ID)
	if err != nil {
		return err
	}
	var bad []syncer.Record
	for _, r := range recs {
		if r.State != syncer.StatePresent {
			continue
		}
		rel := strings.TrimPrefix(r.RelPath, src.DestFolder+"/")
		if size, ok := objects[rel]; !ok || size != r.Size {
			bad = append(bad, r)
		}
	}
	if len(bad) > 0 {
		if err := v.s.o.DB.Write(context.WithoutCancel(ctx), func(tx *sql.Tx) error {
			for _, r := range bad {
				if err := v.s.files.MarkMissingTx(ctx, tx, r.ID); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
		for _, r := range bad {
			v.log(slog.LevelWarn, "a backed-up object is missing or has another size", "path", r.RelPath)
		}
		v.stats.FilesMissing += int64(len(bad))
		v.warnings++
		return v.finish(ctx, it.ID, jobs.ItemFailed, 0, fmt.Sprintf("%d objects missing or of another size (marked missing; the next sync copies them again)", len(bad)))
	}
	return v.finish(ctx, it.ID, jobs.ItemDone, 0, "")
}

// rcloneSample checks one source's sample with rclone check --download --combined (§7.6): "="
// sets verified_at and records the source's sha256; "*", "+" and "-" mark the record missing;
// "!" (and a file the report does not name) fails the item.
func (v *verifyRun) rcloneSample(ctx context.Context, x *rcloneRun, sourceID int64, items []jobs.Item) error {
	src, ok := v.sources[sourceID]
	if !ok {
		for _, it := range items {
			if err := v.finish(ctx, it.ID, jobs.ItemSkipped, 0, "the source is no longer synced to this destination"); err != nil {
				return err
			}
		}
		return nil
	}
	var files []string
	var run []jobs.Item
	for _, it := range items {
		d, _ := parseItem(it)
		if err := listableName(d.Source); err != nil {
			if err := v.finish(ctx, it.ID, jobs.ItemSkipped, 0, err.Error()); err != nil {
				return err
			}
			continue
		}
		if skip, err := v.markedThisJob(ctx, d); err != nil {
			return err
		} else if skip {
			if err := v.finish(ctx, it.ID, jobs.ItemSkipped, 0, "marked missing by the listing"); err != nil {
				return err
			}
			continue
		}
		files = append(files, d.Source)
		run = append(run, it)
	}
	if len(run) == 0 {
		return nil
	}
	items = run
	stop := v.stopAtEnd(v.s.now(), false)
	defer stop.stop()
	marks, err := x.conn.Check(ctx, rclone.CheckInput{SourceRoot: src.Path, DestFolder: src.DestFolder, Files: files, Interrupt: stop.C})
	if err != nil {
		if stop.fired() {
			return v.deferral(v.s.now())
		}
		return fmt.Errorf("rclone check: %w", err)
	}
	for _, it := range items {
		d, _ := parseItem(it)
		switch marks[d.Source] {
		case rclone.MarkMatch:
			hash, ok, err := v.sourceHash(ctx, d)
			if err != nil {
				return err
			}
			if !ok {
				err = v.finish(ctx, it.ID, jobs.ItemSkipped, 0, "the source changed since planning")
			} else {
				err = v.markSample(ctx, it, d, true, hash, "")
			}
			if err != nil {
				return err
			}
		case rclone.MarkDiffer, rclone.MarkMissingOnDest, rclone.MarkMissingOnSource:
			if err := v.markSample(ctx, it, d, false, "", "content differs from the source"); err != nil {
				return err
			}
		default:
			if err := v.failItem(ctx, it, "could not be checked"); err != nil {
				return err
			}
		}
	}
	return nil
}

// versionFile is one file a config version's manifest names.
type versionFile struct {
	name   string
	size   int64
	sha256 string
}

// manifestFiles reads the files a version's manifest.json names (Plex DB: files[]; *arr: zip)
// and, for manifest versions, SHA256SUMS.
func manifestFiles(manifest, sums []byte) []versionFile {
	var out []versionFile
	var m struct {
		Files []struct {
			Name   string `json:"name"`
			Size   int64  `json:"size"`
			SHA256 string `json:"sha256"`
		} `json:"files"`
		Zip *struct {
			Name   string `json:"name"`
			Size   int64  `json:"size"`
			SHA256 string `json:"sha256"`
		} `json:"zip"`
	}
	if json.Unmarshal(manifest, &m) == nil {
		for _, f := range m.Files {
			out = append(out, versionFile{name: f.Name, size: f.Size, sha256: f.SHA256})
		}
		if m.Zip != nil {
			out = append(out, versionFile{name: m.Zip.Name, size: m.Zip.Size, sha256: m.Zip.SHA256})
		}
	}
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		hex, name, ok := strings.Cut(sc.Text(), "  ")
		if ok && name != "" {
			out = append(out, versionFile{name: name, size: -1, sha256: hex})
		}
	}
	return out
}

// rcloneVersions checks the config versions of one kind folder (§7.6): every complete version's
// files are listed by the sizes its manifest names, and the newest version of each folder is
// downloaded and hashed against its manifest.
func (v *verifyRun) rcloneVersions(ctx context.Context, x *rcloneRun, it jobs.Item) error {
	vs := rclone.NewVersionStore(x.conn, isVersionDir)
	list, err := vs.List(ctx, it.RelPath)
	if err != nil {
		return err
	}
	newest := map[string]engines.StoredVersion{}
	var problems []string
	checked := int64(0)
	for _, sv := range list {
		if !sv.Complete {
			continue
		}
		checked++
		raw, err := vs.ReadFile(ctx, sv.Ref, rclone.ManifestName, 1<<20)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: manifest unreadable: %v", sv.LogicalPath, err))
			continue
		}
		sums, _ := vs.ReadFile(ctx, sv.Ref, "SHA256SUMS", 1<<20)
		for _, f := range manifestFiles(raw, sums) {
			size, ok := sv.Files[f.name]
			if !ok || (f.size >= 0 && size != f.size) {
				problems = append(problems, fmt.Sprintf("%s: %s is missing or of another size", sv.LogicalPath, f.name))
			}
		}
		folder := strings.TrimSuffix(sv.LogicalPath, "/"+sv.Version)
		if cur, ok := newest[folder]; !ok || sv.Time.After(cur.Time) {
			newest[folder] = sv
		}
	}
	staging := filepath.Join(v.s.stagingDir(), fmt.Sprintf("verify-job%d", v.job.ID), "versions")
	defer func() { _ = os.RemoveAll(filepath.Dir(staging)) }()
	for _, folder := range sortedKeys(newest) {
		sv := newest[folder]
		raw, err := vs.ReadFile(ctx, sv.Ref, rclone.ManifestName, 1<<20)
		if err != nil {
			continue
		}
		sums, _ := vs.ReadFile(ctx, sv.Ref, "SHA256SUMS", 1<<20)
		dir := filepath.Join(staging, strconv.FormatInt(checked, 10))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		var need int64
		for _, f := range manifestFiles(raw, sums) {
			need += max(sv.Files[f.name], 0)
		}
		if free, ok := stagingFreeSpace(dir); ok && need+stagingMargin > free {
			// Not downloaded into a config directory that cannot hold it: a failed download would
			// report an intact version as damaged.
			v.warn("the newest config version does not fit the config directory's free space; its checksums were not checked",
				"version", sv.LogicalPath, "size", formatBytes(need), "free", formatBytes(free))
			continue
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			return err
		}
		for _, f := range manifestFiles(raw, sums) {
			if f.sha256 == "" || strings.Contains(f.name, "/") || f.name == rclone.ManifestName {
				continue
			}
			if err := vs.Fetch(ctx, sv.Ref, f.name, dir); err != nil {
				problems = append(problems, fmt.Sprintf("%s: %s could not be downloaded: %v", sv.LogicalPath, f.name, err))
				continue
			}
			got, _, err := filecopy.HashFile(ctx, root, f.name, nil)
			if err != nil || strings.TrimPrefix(got, filecopy.HashPrefix) != strings.ToLower(f.sha256) {
				problems = append(problems, fmt.Sprintf("%s: %s does not match its checksum", sv.LogicalPath, f.name))
			}
		}
		_ = root.Close()
	}
	v.stats.VersionsChecked += checked
	if len(problems) > 0 {
		for _, p := range problems {
			v.log(slog.LevelWarn, "a config version is damaged", "problem", p)
		}
		return v.finish(ctx, it.ID, jobs.ItemFailed, 0, problems[0])
	}
	return v.finish(ctx, it.ID, jobs.ItemDone, 0, "")
}
