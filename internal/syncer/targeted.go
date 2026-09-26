package syncer

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// Targeted syncs (docs/design/phase2-3.md §9.1, D14, S10). A sync with Params.Paths (the webhook
// path: a refresh queues one per source with the folders of the items that changed) scans and
// plans only those paths of its one source:
//   - the scan is catalog.Scanner.ScanPathsLocked, under the source's lock; only the targets whose
//     rows reflect the disk (scanned or gone) are planned;
//   - a webhook sync then compares the files the *arr index expects under the targets with the
//     catalog and scans again after 5, 15 and 30 s while one may still show up (expected.go: a
//     mount that shows a new file late, a file still being written); the source's lock is free
//     during these waits. A file still missing is a warning; one the scan never catalogs (a
//     symlink, an excluded name, a folder it cannot read), one a folder lists under another
//     spelling, and one the catalog lists with its size on disk are reported without waiting;
//   - the plan covers the catalog files and live records at or under the targets, plus the other
//     names of their hardlink groups; moves are paired inside that scope only. The S11 name
//     checks read only the destination records the plan's new names can collide with, and the S6
//     check of a retain only its folder's records and files (not the whole destination or
//     source);
//   - a vanished name is retained only when its folder gets a copy, update, move or link in the
//     same plan (D14): an upgrade or a rename. Every other vanished name stays live and recorded
//     until the next untargeted sync, whose mass-change guard sees the whole change. So webhook
//     syncs cannot cut a mass deletion into jobs that each stay under the thresholds;
//   - the mass-change guard counts against the whole source's live files, as every sync does;
//   - empty folders are pruned inside the targets only; no manifest export follows.
// A follow-up sync (trigger webhook, or targeted) whose destination is not mounted ends
// completed_with_warnings ("destination not mounted; skipped") instead of failing.

// ExpectedFile is a file the *arr index expects under the paths of a webhook sync.
type ExpectedFile struct {
	// RelPath is the file's path inside the source.
	RelPath string
	// Size is the size the *arr reported.
	Size int64
	// App names the *arr that reported it ("Radarr"), for the warning.
	App string
}

// defaultExpectedWaits are the waits before the rescans of the expected files (§9.1: after 5, 15
// and 30 s), within expectedBudget in all.
var defaultExpectedWaits = []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second}

// expectedBudget bounds the time a webhook sync waits for expected files, with the locks held.
const expectedBudget = 60 * time.Second

// isFollowUp reports whether a sync was queued by a refresh or a webhook: trigger webhook, or
// targeted (only a refresh queues paths).
func isFollowUp(job jobs.Job) bool {
	return job.Trigger == jobs.TriggerWebhook || len(job.Params.Paths) > 0
}

// notMounted reports the S3 errors of destinations.Open that mean the destination is not there.
func notMounted(err error) bool {
	return errors.Is(err, destinations.ErrNotMounted) || errors.Is(err, destinations.ErrMarkerMismatch) ||
		errors.Is(err, destinations.ErrFSChanged)
}

// targeted reports whether the job is a targeted sync.
func (s *syncRun) targeted() bool { return len(s.job.Params.Paths) > 0 }

// errSourceChanged: the source was deleted, disabled, or given another path or destination folder
// while a webhook sync waited for expected files with the source's lock free.
var errSourceChanged = errors.New("the source was deleted, disabled or edited while the sync waited for the files the *arr expects")

// sourceLock is the lock of a source as a sync holds it while it scans and plans the source
// (catalog.Store.LockSource). A webhook sync frees it while it waits for expected files.
type sourceLock struct {
	cat    *catalog.Store
	id     int64
	unlock func()
}

func (l *sourceLock) acquire(ctx context.Context) error {
	unlock, err := l.cat.LockSource(ctx, l.id)
	if err != nil {
		return err
	}
	l.unlock = unlock
	return nil
}

// release frees the lock; it does nothing when the lock is not held.
func (l *sourceLock) release() {
	if l.unlock != nil {
		l.unlock()
		l.unlock = nil
	}
}

// scanTargets runs the targeted scan of src, then (for a webhook sync) the expected-files check
// with its rescans (expected.go). It returns the scan's counts and the targets that may be
// planned. lk is the source's lock, held on entry and on return. It is freed while the sync waits
// between rescans: the wait is a pause, so another destination's sync of the same source can scan
// and plan meanwhile, and the next rescan and the plan run under the lock again. A source that
// was deleted, disabled or edited meanwhile gives errSourceChanged.
//
// The counts are the first scan's plus each rescan's changes, except Files: after a wait,
// liveBefore is the number of live catalog files under the targets before the first wait (else
// -1). The files the targets gained or lost since, by this job's rescans or by another scan while
// the lock was free (another destination's sync of the source, a scan job), are the live files
// under them when the plan reads them less liveBefore (planSource adds that to Files). Files under
// a folder that cannot be read keep their rows, so they cancel out.
func (s *syncRun) scanTargets(ctx context.Context, src catalog.Source, lk *sourceLock) (res catalog.ScanResult, scopes []string, liveBefore int64, err error) {
	liveBefore = -1
	pr, err := s.r.scan.ScanPathsLocked(ctx, src.ID, s.job.Params.Paths, s.rep)
	if err != nil {
		return pr.ScanResult, nil, liveBefore, err
	}
	res = pr.ScanResult
	scopes = pr.Current()
	if len(scopes) < len(pr.Targets) {
		s.rep.Log(slog.LevelWarn, fmt.Sprintf("%d of %d paths were not scanned; they are synced by the next full sync",
			len(pr.Targets)-len(scopes), len(pr.Targets)), "source", src.Name)
	}
	webhook := s.job.Trigger == jobs.TriggerWebhook || s.job.Trigger == jobs.TriggerResume
	if len(scopes) == 0 || !webhook || s.r.expected == nil {
		return res, scopes, liveBefore, nil
	}
	checks, err := s.checkExpected(ctx, src, scopes)
	if err != nil {
		return res, nil, liveBefore, err
	}
	reported := map[string]bool{}
	s.reportExpected(src, checks, reported)
	pending := pendingExpected(checks)
	// A rescan walks targets the first scan counted already, and repeats its warnings.
	warned := map[string]bool{}
	for _, w := range res.Warnings {
		warned[w] = true
	}
	var waited time.Duration
	for _, wait := range s.r.expectedWaits {
		if len(pending) == 0 || waited+wait > expectedBudget {
			break
		}
		if liveBefore < 0 {
			if liveBefore, err = s.liveCountUnder(ctx, src.ID, scopes); err != nil {
				return res, nil, liveBefore, err
			}
		}
		s.rep.Log(slog.LevelInfo, fmt.Sprintf("%s expected by the *arr not visible yet; scanning again in %s",
			plural(int64(len(pending)), "file", "files"), wait), "source", src.Name)
		lk.release()
		err := s.r.sleep(ctx, wait)
		if err == nil {
			err = lk.acquire(ctx)
		}
		if err != nil {
			return res, nil, liveBefore, err
		}
		waited += wait
		if src, err = s.reloadSource(ctx, src); err != nil {
			return res, nil, liveBefore, err
		}
		var again []string
		for _, m := range pending {
			for _, t := range scopes {
				if catalog.UnderAny(m.RelPath, []string{t}) && !slices.Contains(again, t) {
					again = append(again, t)
				}
			}
		}
		pr, err := s.r.scan.ScanPathsLocked(ctx, src.ID, again, onceWarned{Reporter: s.rep, seen: warned})
		if err != nil {
			return res, nil, liveBefore, err
		}
		// A rescan's changes and the warnings no earlier scan gave are added (Files: see above).
		res.Added += pr.Added
		res.Changed += pr.Changed
		res.Deleted += pr.Deleted
		for _, w := range pr.Warnings {
			if !warned[w] {
				warned[w] = true
				res.Warnings = append(res.Warnings, w)
				res.WarningCount++
			}
		}
		if checks, err = s.checkExpected(ctx, src, scopes); err != nil {
			return res, nil, liveBefore, err
		}
		s.reportExpected(src, checks, reported)
		pending = pendingExpected(checks)
	}
	for _, m := range pending {
		s.warnings++
		s.expectedMissing++
		s.rep.Log(slog.LevelWarn, fmt.Sprintf("%s reports %s (%s), but it is not in %q with that size; it is not backed up yet",
			cmp.Or(m.App, "The *arr"), m.RelPath, formatBytes(m.Size), src.Name), "source", src.Name)
	}
	return res, scopes, liveBefore, nil
}

// liveCountUnder returns the number of live catalog files of a source at or under paths.
func (s *syncRun) liveCountUnder(ctx context.Context, sourceID int64, paths []string) (int64, error) {
	files, err := s.r.cat.LiveUnder(ctx, sourceID, paths)
	return int64(len(files)), err
}

// reloadSource reads src again after its lock was free: it returns errSourceChanged when the
// source was deleted or disabled, or its path or destination folder changed.
func (s *syncRun) reloadSource(ctx context.Context, src catalog.Source) (catalog.Source, error) {
	cur, err := s.r.cat.Get(ctx, src.ID)
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		return src, errSourceChanged
	case err != nil:
		return src, err
	case !cur.Enabled || cur.Path != src.Path || cur.DestFolder != src.DestFolder:
		return src, errSourceChanged
	}
	s.setSource(cur)
	return cur, nil
}

// scopedPlanInput returns what a targeted sync plans for src: the live catalog files at or under
// scopes plus the other names of their hardlink groups (sorted by path), and the live records of
// those source paths and of every source path at or under scopes (sorted by source path).
func (s *syncRun) scopedPlanInput(ctx context.Context, src catalog.Source, scopes []string) ([]*planFile, []Record, error) {
	inScope, err := s.r.cat.LiveUnder(ctx, src.ID, scopes)
	if err != nil {
		return nil, nil, err
	}
	var groups []string
	for _, f := range inScope {
		if f.HardlinkGroup != "" && !slices.Contains(groups, f.HardlinkGroup) {
			groups = append(groups, f.HardlinkGroup)
		}
	}
	partners, err := s.r.cat.LiveInGroups(ctx, src.ID, groups)
	if err != nil {
		return nil, nil, err
	}
	var files []*planFile
	var extra []string
	seen := map[string]bool{}
	for _, f := range append(inScope, partners...) {
		if seen[f.RelPath] {
			continue
		}
		seen[f.RelPath] = true
		files = append(files, &planFile{id: f.ID, rel: f.RelPath, size: f.Size, mtimeNs: f.MtimeNs, group: f.HardlinkGroup})
		if !catalog.UnderAny(f.RelPath, scopes) {
			extra = append(extra, f.RelPath)
		}
	}
	slices.SortFunc(files, func(a, b *planFile) int { return strings.Compare(a.rel, b.rel) })
	recs, err := s.r.store.liveForSourceUnder(ctx, s.h.Destination.ID, src.ID, scopes)
	if err != nil {
		return nil, nil, err
	}
	for _, rel := range extra {
		r, ok, err := s.r.store.liveForSourcePath(ctx, s.h.Destination.ID, src.ID, rel)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			recs = append(recs, r)
		}
	}
	slices.SortFunc(recs, func(a, b Record) int {
		return cmp.Or(strings.Compare(a.SourceRelPath, b.SourceRelPath), cmp.Compare(a.ID, b.ID))
	})
	return files, recs, nil
}

// The scoped reads of a targeted sync (after "SELECT <columns> FROM destination_files"). Each one
// is an index range on a path, so its cost follows the scope, not the destination or source. An
// exact path and the paths inside it are two queries: for "path = ? OR path in a range" SQLite
// reads every record of the destination (or of the source).
const (
	// args: destination, source, source path.
	whereLiveAtSourcePath = `WHERE destination_id = ? AND source_id = ? AND state IN ` + liveStates +
		` AND source_rel_path = ? ORDER BY id`
	// args: destination, source, folder + "/", folder + "0".
	whereLiveInSourceFolder = `WHERE destination_id = ? AND source_id = ? AND state IN ` + liveStates +
		` AND source_rel_path >= ? AND source_rel_path < ? ORDER BY source_rel_path, id`
	// args: destination, source, folder + "/", folder + "0"; live and retained records.
	whereInSourceFolder = `WHERE destination_id = ? AND source_id = ? AND source_rel_path >= ? AND source_rel_path < ?
		ORDER BY source_rel_path, id`
	// args: destination, folder + "/", folder + "0".
	whereLiveInFolder = `WHERE destination_id = ? AND state IN ` + liveStates + ` AND rel_path >= ? AND rel_path < ? ORDER BY id`
	// args: destination, prefix: the first live path at or after prefix.
	queryNextLivePath = `SELECT rel_path FROM destination_files WHERE destination_id = ? AND state IN ` + liveStates +
		` AND rel_path >= ? ORDER BY rel_path LIMIT 1`
)

// liveForSourceUnder returns the live records of a source at a destination whose source path is
// one of paths or lies inside one of them.
func (s *Store) liveForSourceUnder(ctx context.Context, destinationID, sourceID int64, paths []string) ([]Record, error) {
	var out []Record
	for _, p := range paths {
		at, err := s.query(ctx, whereLiveAtSourcePath, destinationID, sourceID, p)
		if err != nil {
			return nil, err
		}
		under, err := s.query(ctx, whereLiveInSourceFolder, destinationID, sourceID, p+"/", p+"0")
		if err != nil {
			return nil, err
		}
		out = append(append(out, at...), under...)
	}
	return out, nil
}

// targetNames returns the name index (S11) of a targeted sync of src: files are the plan's files
// (those at or under scopes and their hardlink partners). A plan creates names only at the
// destination paths of its files, so the index holds only the live records at or under the
// scopes' destination paths and at the partners' ones, on a case-insensitive destination under
// any spelling of them that differs only in case, instead of every record of the destination.
func (s *syncRun) targetNames(ctx context.Context, src catalog.Source, scopes []string, files []*planFile) (*nameIndex, error) {
	var under, at []string
	for _, sc := range scopes {
		under = append(under, path.Join(src.DestFolder, sc))
	}
	for _, f := range files {
		if !catalog.UnderAny(f.rel, scopes) {
			at = append(at, path.Join(src.DestFolder, f.rel))
		}
	}
	recs, err := s.r.store.liveNear(ctx, s.h.Destination.ID, under, at, s.h.Capabilities.CaseInsensitive)
	if errors.Is(err, errNoFoldedLookup) {
		return s.destinationNames(ctx)
	}
	if err != nil {
		return nil, err
	}
	names := newNameIndex(s.h.Capabilities)
	for _, rec := range recs {
		_, planned := s.linked[rec.SourceID]
		names.addRecord(rec.RelPath, !planned)
	}
	return names, nil
}

// errNoFoldedLookup: a path that is not valid UTF-8 (or holds U+FFFD) has no case-folded lookup;
// filecopy.FoldKey turns every invalid byte into U+FFFD.
var errNoFoldedLookup = errors.New("no case-folded lookup for this path")

// liveNear returns the live records of a destination at or under one of the paths under, or at
// one of the paths at, ordered by id. With fold, a record whose path equals one of them up to case
// (filecopy.FoldKey), or lies under one up to case, is returned too.
func (s *Store) liveNear(ctx context.Context, destinationID int64, under, at []string, fold bool) ([]Record, error) {
	hasPrefix := map[string]bool{}
	spellings := func(p string) ([]string, error) {
		if !fold {
			return []string{p}, nil
		}
		return s.foldedSpellings(ctx, destinationID, p, hasPrefix)
	}
	seen := map[int64]bool{}
	var out []Record
	add := func(recs ...Record) {
		for _, r := range recs {
			if !seen[r.ID] {
				seen[r.ID] = true
				out = append(out, r)
			}
		}
	}
	for _, p := range under {
		sp, err := spellings(p)
		if err != nil {
			return nil, err
		}
		for _, v := range sp {
			r, ok, err := s.LiveAt(ctx, destinationID, v)
			if err != nil {
				return nil, err
			}
			if ok {
				add(r)
			}
			recs, err := s.query(ctx, whereLiveInFolder, destinationID, v+"/", v+"0")
			if err != nil {
				return nil, err
			}
			add(recs...)
		}
	}
	for _, p := range at {
		sp, err := spellings(p)
		if err != nil {
			return nil, err
		}
		for _, v := range sp {
			r, ok, err := s.LiveAt(ctx, destinationID, v)
			if err != nil {
				return nil, err
			}
			if ok {
				add(r)
			}
		}
	}
	slices.SortFunc(out, func(a, b Record) int { return cmp.Compare(a.ID, b.ID) })
	return out, nil
}

// foldedSpellings returns the spellings of p that differ from it only in case (every rune in the
// same unicode.SimpleFold orbit, as filecopy.FoldKey compares) and begin the path of a live record
// of the destination. It builds them one rune at a time and keeps a spelling only while a live
// path begins with it (one index lookup each, cached in hasPrefix), so it reads a few index
// entries per letter of p instead of every record.
func (s *Store) foldedSpellings(ctx context.Context, destinationID int64, p string, hasPrefix map[string]bool) ([]string, error) {
	if !utf8.ValidString(p) || strings.ContainsRune(p, utf8.RuneError) {
		return nil, errNoFoldedLookup
	}
	cands := []string{""}
	for _, r := range p {
		orbit := foldOrbit(r)
		var next []string
		for _, c := range cands {
			for _, v := range orbit {
				q := c + string(v)
				if len(orbit) > 1 {
					ok, err := s.livePrefix(ctx, destinationID, q, hasPrefix)
					if err != nil {
						return nil, err
					}
					if !ok {
						continue
					}
				}
				next = append(next, q)
			}
		}
		if len(next) == 0 {
			return nil, nil
		}
		cands = next
	}
	return cands, nil
}

// livePrefix reports whether the path of a live record of the destination begins with prefix.
func (s *Store) livePrefix(ctx context.Context, destinationID int64, prefix string, cache map[string]bool) (bool, error) {
	if ok, done := cache[prefix]; done {
		return ok, nil
	}
	var next string
	err := s.reader(queryNextLivePath, []any{destinationID, prefix}).QueryRowContext(ctx, queryNextLivePath, destinationID, prefix).Scan(&next)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("read destination files: %w", err)
	}
	ok := err == nil && strings.HasPrefix(next, prefix)
	cache[prefix] = ok
	return ok, nil
}

// foldOrbit returns r and every rune that folds with it (unicode.SimpleFold), r first.
func foldOrbit(r rune) []rune {
	out := []rune{r}
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		out = append(out, f)
	}
	return out
}

// forSourceUnder returns the live and retained records of a source at a destination whose source
// path lies inside the folder dir.
func (s *Store) forSourceUnder(ctx context.Context, destinationID, sourceID int64, dir string) ([]Record, error) {
	return s.query(ctx, whereInSourceFolder, destinationID, sourceID, dir+"/", dir+"0")
}

// inTargets reports whether the destination path dest lies at or under one of the job's targets
// in the source's folder: a targeted sync prunes empty folders only there.
func (s *syncRun) inTargets(sourceID int64, dest string) bool {
	folder := s.linked[sourceID].DestFolder
	for _, t := range s.job.Params.Paths {
		td := path.Join(folder, t)
		if dest == td || strings.HasPrefix(dest, td+"/") {
			return true
		}
	}
	return false
}

// queueManifestExport queues the manifest export that follows a sync that is neither a dry run
// nor targeted and did not end cancelled, when the destination's setting allows it (§9.2). It
// returns the queued job's id (0: none). A failure is logged, never the sync's.
func (r *SyncRunner) queueManifestExport(ctx context.Context, job jobs.Job, runErr error) int64 {
	if r.enq == nil || r.manifestAfter == nil || job.DryRun || len(job.Params.Paths) > 0 || job.Params.DestinationID == 0 {
		return 0
	}
	if runErr != nil && ctx.Err() != nil {
		return 0 // cancelled
	}
	ctx = context.WithoutCancel(ctx)
	ok, err := r.manifestAfter(ctx, job.Params.DestinationID)
	if err != nil {
		r.log.Warn("Could not decide about the manifest export after a sync", "jobId", job.ID, "error", err)
		return 0
	}
	if !ok {
		return 0
	}
	mj, err := r.enq.Enqueue(ctx, jobs.Spec{Type: jobs.TypeManifestExport, Trigger: job.Trigger,
		Params: jobs.Params{DestinationID: job.Params.DestinationID}})
	if err != nil {
		r.log.Warn("Could not queue the manifest export after a sync", "jobId", job.ID, "error", err)
		return 0
	}
	return mj.ID
}

// sleepCtx waits d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
