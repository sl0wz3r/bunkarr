package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// Targeted scans (docs/design/phase2-3.md §9.1; safety rules S1, S4, S10a, S18). A webhook sync
// scans only the folders of the *arr items that changed: Scanner.ScanPaths reads and updates the
// catalog under those paths and nothing else.
//
//   - The root checks of a full scan apply (S10a): the root exists, is a directory and is on the
//     recorded filesystem.
//   - Each target is resolved one component at a time from the root with lstat. Every ancestor
//     must be a real directory (never a symlink), must not be excluded and must not be a
//     destination root or the config directory by (dev, ino) (S4); it is opened through the
//     root and its device and inode are checked again (S1). A target whose ancestors fail is
//     dropped with a warning and its rows are left alone.
//   - The target itself is walked exactly like the full scan walks a directory (excludes,
//     symlinks skipped, dev/ino checks): rows under it that the walk did not see are marked
//     deleted, except under directories that could not be read.
//   - A target that no longer exists marks its rows deleted, but only while its parent has
//     entries or the catalog has no other live rows under the parent: an empty parent (an
//     unmounted nested mount) is refused and the rows are kept. A target whose last name its
//     folder lists only under another spelling (case or normalization, on an insensitive
//     filesystem) is gone under the spelling given, as the full scan sees it; another spelling of
//     an ancestor drops the target.
//   - ESTALE is retried once, 5 s later, before it is fatal. Nothing is written before the walk
//     of every target is complete.
//   - A file of a target with a link count of 2 or more has its possible partners outside the
//     targets nominated by the (dev, ino) the catalog recorded for them; each is lstat'ed now and
//     grouped only when its current metadata equals the file's (on FUSE also its head/tail hash).
//     The recorded numbers only nominate: grouping uses stats read by this scan.
//   - The scan increments the source's scan_seq, so hardlink group ids never repeat, and
//     recomputes its statistics from the rows. last_scan_at keeps meaning the last full scan.

// TargetState is what a targeted scan did with one of its paths.
type TargetState string

// Target states.
const (
	// TargetScanned: the target was walked (a folder or a file); its rows reflect the disk.
	TargetScanned TargetState = "scanned"
	// TargetGone: the target no longer exists (or its folder lists its last name only under
	// another spelling); its rows were marked deleted.
	TargetGone TargetState = "gone"
	// TargetDropped: the target could not be resolved safely (a missing, symlinked, excluded or
	// forbidden ancestor, or a folder that could not be read); its rows were left alone.
	TargetDropped TargetState = "dropped"
	// TargetRefused: the target is gone, but its parent folder is empty while the catalog lists
	// other files in it (an unmounted share); its rows were left alone.
	TargetRefused TargetState = "refused"
)

// Target is one path of a targeted scan and what the scan did with it.
type Target struct {
	// Path is relative to the source root, slash-separated (jobs.ValidTargetPath).
	Path  string      `json:"path"`
	State TargetState `json:"state"`
	// Reason says why a target was dropped or refused.
	Reason string `json:"reason,omitempty"`
}

// Current reports whether the catalog under the target now reflects the disk (it was scanned or
// found gone), so a sync may plan it.
func (t Target) Current() bool { return t.State == TargetScanned || t.State == TargetGone }

// PathsResult is the result of a targeted scan: the counts of ScanResult (for the files under
// the targets) and each target's outcome, in path order.
type PathsResult struct {
	ScanResult
	Targets []Target `json:"targets"`
}

// Current returns the paths of the targets whose rows reflect the disk (Target.Current).
func (r PathsResult) Current() []string {
	var out []string
	for _, t := range r.Targets {
		if t.Current() {
			out = append(out, t.Path)
		}
	}
	return out
}

// estaleRetryWait is how long a targeted scan waits before it retries after ESTALE.
const estaleRetryWait = 5 * time.Second

// CanonicalTargets validates target paths (jobs.ValidTargetPath) and returns them sorted,
// de-duplicated, without paths that lie inside another listed path.
func CanonicalTargets(paths []string) ([]string, error) {
	set := make(map[string]bool, len(paths))
	for _, p := range paths {
		if !jobs.ValidTargetPath(p) {
			return nil, invalid("paths", "invalid path %q: a clean, relative path inside the source is required", p)
		}
		set[p] = true
	}
	out := make([]string, 0, len(set))
	for p := range set {
		inside := false
		for d := path.Dir(p); d != "."; d = path.Dir(d) {
			inside = inside || set[d]
		}
		if !inside {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out, nil
}

// UnderAny reports whether rel is one of targets or lies inside one of them.
func UnderAny(rel string, targets []string) bool {
	for _, t := range targets {
		if rel == t || strings.HasPrefix(rel, t+"/") {
			return true
		}
	}
	return false
}

// ScanPaths takes the source's lock (waiting for it) and scans only the given paths (see the
// comment at the top of this file). paths must satisfy jobs.ValidTargetPath. A refused scan (an
// error wrapping ErrScanRefused: the root checks of S10a, or an I/O error of a lost mount) leaves
// the catalog unchanged. A cancelled context returns ctx.Err() and marks nothing deleted.
func (sc *Scanner) ScanPaths(ctx context.Context, sourceID int64, paths []string, rep ScanReporter) (PathsResult, error) {
	unlock, err := sc.store.LockSource(ctx, sourceID)
	if err != nil {
		return PathsResult{ScanResult: ScanResult{SourceID: sourceID}}, err
	}
	defer unlock()
	return sc.scanPaths(ctx, sourceID, paths, rep)
}

// ScanPathsLocked is ScanPaths for a caller that already holds Store.LockSource(sourceID) (a
// targeted sync keeping the catalog stable while it scans and plans).
func (sc *Scanner) ScanPathsLocked(ctx context.Context, sourceID int64, paths []string, rep ScanReporter) (PathsResult, error) {
	if !sc.store.locks.held(sourceID) {
		return PathsResult{ScanResult: ScanResult{SourceID: sourceID}}, fmt.Errorf("scan source %d: ScanPathsLocked called without holding the source lock", sourceID)
	}
	return sc.scanPaths(ctx, sourceID, paths, rep)
}

func (sc *Scanner) scanPaths(ctx context.Context, sourceID int64, paths []string, rep ScanReporter) (PathsResult, error) {
	started := time.Now().UTC()
	empty := PathsResult{ScanResult: ScanResult{SourceID: sourceID, StartedAt: started, Skipped: map[string]int64{},
		UnreadableDirs: []string{}, Warnings: []string{}}, Targets: []Target{}}
	targets, err := CanonicalTargets(paths)
	if err != nil {
		return empty, err
	}
	if len(targets) == 0 {
		return empty, invalid("paths", "a targeted scan needs at least one path")
	}
	if rep == nil {
		rep = nopReporter{}
	}
	src, err := sc.store.loadScanSource(ctx, sourceID)
	if err != nil {
		return empty, err
	}
	var (
		s   *scan
		out []Target
	)
	for attempt := 1; ; attempt++ {
		s = sc.newScan(src, rep, started)
		out, err = s.runTargets(ctx, targets)
		if err == nil || attempt > 1 || ctx.Err() != nil || !errors.Is(err, syscall.ESTALE) {
			break
		}
		msg := fmt.Sprintf("stale file handle while scanning %s; retrying once in %s", src.path, sc.estaleWait)
		rep.Log(slog.LevelWarn, msg, "source", src.name)
		sc.log.Warn(msg, "source", src.name)
		t := time.NewTimer(sc.estaleWait)
		select {
		case <-ctx.Done():
			t.Stop()
			return empty, ctx.Err()
		case <-t.C:
		}
	}
	s.res.Duration = time.Since(started)
	s.res.DurationMs = s.res.Duration.Milliseconds()
	res := PathsResult{ScanResult: s.res, Targets: out}
	if res.Targets == nil {
		res.Targets = []Target{}
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return res, ctxErr
		}
		sc.log.Warn("targeted scan failed", "source", src.name, "error", err)
		return res, fmt.Errorf("scan source %q: %w", src.name, err)
	}
	sc.log.Info("targeted scan finished", "source", src.name, "targets", len(targets), "files", s.res.Files,
		"added", s.res.Added, "changed", s.res.Changed, "deleted", s.res.Deleted, "warnings", s.res.WarningCount,
		"duration", s.res.Duration)
	return res, nil
}

// newScan returns the state of one scan attempt of src.
func (sc *Scanner) newScan(src scanInput, rep ScanReporter, started time.Time) *scan {
	return &scan{
		sc:        sc,
		src:       src,
		rep:       rep,
		stamp:     db.FormatTime(started),
		match:     newMatcher(src.exclude),
		forbidden: map[devIno]string{},
		links:     map[devIno][]linkCand{},
		res: ScanResult{SourceID: src.id, SourceName: src.name, StartedAt: started,
			Skipped: map[string]int64{}, UnreadableDirs: []string{}, Warnings: []string{}},
	}
}

// runTargets is one attempt of a targeted scan: the read-only walk of every target (and of the
// hardlink partners), then the commit.
func (s *scan) runTargets(ctx context.Context, targets []string) ([]Target, error) {
	if err := s.openRoot(); err != nil {
		return nil, err
	}
	defer s.root.Close()
	if err := s.loadForbidden(ctx); err != nil {
		return nil, err
	}
	s.rep.Log(slog.LevelInfo, "scanning paths of the source", "source", s.src.name, "paths", len(targets))
	out := make([]Target, 0, len(targets))
	for _, t := range targets {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		tgt, err := s.walkTarget(ctx, t)
		if err != nil {
			return nil, err
		}
		if !tgt.Current() {
			s.warn("%s was not scanned: %s; its catalog entries are kept", t, tgt.Reason)
		}
		out = append(out, tgt)
	}
	if err := s.flushLookup(ctx); err != nil {
		return nil, err
	}
	var scopes []string
	for _, t := range out {
		if t.Current() {
			scopes = append(scopes, t.Path)
		}
	}
	partners, err := s.nominatePartners(ctx, scopes)
	if err != nil {
		return nil, err
	}
	if err := s.flushLookup(ctx); err != nil {
		return nil, err
	}
	if err := s.buildGroups(ctx); err != nil {
		return nil, err
	}
	// Every target is walked and nothing has been written yet.
	if len(scopes) == 0 {
		return out, nil
	}
	if err := s.commit(ctx); err != nil {
		return nil, err
	}
	if err := s.finishTargets(ctx, scopes, partners); err != nil {
		return nil, err
	}
	return out, nil
}

// dropped returns a dropped target.
func dropped(t, format string, args ...any) Target {
	return Target{Path: t, State: TargetDropped, Reason: fmt.Sprintf(format, args...)}
}

// fatalOrDrop turns an error reading rel into a refusal of the whole scan (a lost mount) or a
// dropped target.
func fatalOrDrop(t, rel string, err error) (Target, error) {
	if isFatalIO(err) {
		return Target{}, refuse(err, "I/O error reading %s: %v; is the share still mounted? (nothing was changed)", rel, err)
	}
	return dropped(t, "cannot read %s: %v", rel, err), nil
}

// isNotExist reports errors that mean a path is not there (ENOENT, or a parent that is not a
// directory).
func isNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// where names the folder rel of the source in a message: its relative path, or the source's path
// for the root.
func (s *scan) where(rel string) string {
	if rel == "." {
		return s.src.path
	}
	return rel
}

// listedExactly reports whether the folder dir, whose path relative to the source is rel, lists
// an entry spelled exactly name, byte for byte. On a case- or normalization-insensitive
// filesystem (an SMB share, APFS) a lookup by another spelling of an entry finds it too, while
// the full scan keys catalog rows by the names folders list: a path resolved by lookup alone (an
// *arr's spelling of a folder, an old catalog key) would add rows under a second spelling of the
// same files. Each folder is listed once per scan attempt.
func (s *scan) listedExactly(dir *os.Root, rel, name string) (bool, error) {
	names, ok := s.names[rel]
	if !ok {
		var err error
		if names, err = readNames(dir, "."); err != nil {
			return false, err
		}
		if s.names == nil {
			s.names = map[string]map[string]struct{}{}
		}
		s.names[rel] = names
	}
	_, ok = names[name]
	return ok, nil
}

// readNames returns the entry names the folder dir of root lists.
func readNames(root *os.Root, dir string) (map[string]struct{}, error) {
	f, err := root.Open(dir)
	if err != nil {
		return nil, err
	}
	list, err := f.Readdirnames(-1)
	_ = f.Close()
	if err != nil {
		return nil, err
	}
	names := make(map[string]struct{}, len(list))
	for _, n := range list {
		names[n] = struct{}{}
	}
	return names, nil
}

// walkTarget resolves target t component by component and walks it. Each component must be
// listed by its folder under exactly that spelling (listedExactly), so every row under the
// target is keyed by the names on disk, like the full scan's.
func (s *scan) walkTarget(ctx context.Context, t string) (Target, error) {
	comps := strings.Split(t, "/")
	dir, rel := s.root, "."
	var opened []*os.Root
	defer func() {
		for _, r := range opened {
			_ = r.Close()
		}
	}()
	for _, name := range comps[:len(comps)-1] {
		child := joinRel(rel, name)
		fi, err := dir.Lstat(name)
		switch {
		case err != nil && isNotExist(err) && !isFatalIO(err):
			return dropped(t, "the folder %s does not exist", child), nil
		case err != nil:
			return fatalOrDrop(t, child, err)
		}
		exact, err := s.listedExactly(dir, rel, name)
		switch {
		case err != nil:
			return fatalOrDrop(t, s.where(rel), err)
		case !exact:
			return dropped(t, "the folder %s does not exist under that exact name (it is spelled differently on disk)", child), nil
		case fi.Mode()&fs.ModeSymlink != 0:
			return dropped(t, "%s is a symbolic link, which is never followed", child), nil
		case !fi.IsDir():
			return dropped(t, "%s is not a folder", child), nil
		case s.match.excluded(child, name, true):
			return dropped(t, "%s is excluded", child), nil
		}
		m, ok := MetaOf(fi)
		if !ok {
			return dropped(t, "%s has no device and inode information", child), nil
		}
		if p, bad := s.forbidden[devIno{m.Dev, m.Inode}]; bad {
			s.res.Skipped[SkipOverlap]++
			return dropped(t, "%s is the same directory as %s (a destination or the config directory)", child, p), nil
		}
		sub, err := dir.OpenRoot(name)
		if err != nil {
			return fatalOrDrop(t, child, err)
		}
		opened = append(opened, sub)
		// os.Root resolves a symlink that stays inside the root: this must still be the directory
		// that was lstat'ed.
		si, err := sub.Stat(".")
		if err != nil {
			return fatalOrDrop(t, child, err)
		}
		if sm, ok := MetaOf(si); !ok || sm.Dev != m.Dev || sm.Inode != m.Inode {
			return dropped(t, "%s changed during the scan", child), nil
		}
		dir, rel = sub, child
	}
	leaf := comps[len(comps)-1]
	fi, err := dir.Lstat(leaf)
	switch {
	case err != nil && isNotExist(err) && !isFatalIO(err):
		return s.goneTarget(ctx, dir, rel, t)
	case err != nil:
		return fatalOrDrop(t, t, err)
	}
	// Rows are keyed by the name the folder lists, never by the caller's spelling of it. A leaf the
	// lookup finds but the folder does not list (a case-only rename on a case-insensitive share,
	// or another normalization) is gone under this spelling, as the full scan sees it: its rows are
	// marked deleted, so a sync planning both spellings pairs the rename. The folder is listed
	// again first, in case the leaf was created after this attempt cached the listing.
	exact, err := s.listedExactly(dir, rel, leaf)
	if err == nil && !exact {
		delete(s.names, rel)
		exact, err = s.listedExactly(dir, rel, leaf)
	}
	switch {
	case err != nil:
		return fatalOrDrop(t, s.where(rel), err)
	case !exact:
		s.rep.Log(slog.LevelInfo, "the path is listed only under another spelling; its catalog entries under this one are marked deleted", "path", t)
		return s.goneTarget(ctx, dir, rel, t)
	}
	mode := fi.Mode()
	switch {
	case mode.IsDir():
		before := len(s.unreadable)
		if err := s.enterDir(ctx, dir, t, leaf, fi); err != nil {
			return Target{}, err
		}
		if slices.Contains(s.unreadable[before:], t) {
			// The target folder itself could not be read (or changed while it was opened): its
			// rows are kept, and it is not planned.
			return dropped(t, "the folder could not be read"), nil
		}
	case mode.IsRegular():
		if s.match.excluded(t, leaf, false) {
			s.res.Excluded++
			break
		}
		m, ok := MetaOf(fi)
		if !ok {
			s.res.Skipped[SkipOther]++
			break
		}
		if err := s.addFile(ctx, t, m); err != nil {
			return Target{}, err
		}
	default:
		if s.match.excluded(t, leaf, false) {
			s.res.Excluded++
			break
		}
		reason := skipReason(mode)
		s.res.Skipped[reason]++
		s.rep.Log(slog.LevelDebug, "skipped", "path", t, "reason", reason)
	}
	return Target{Path: t, State: TargetScanned}, nil
}

// goneTarget decides about a target that does not exist in its (existing) parent folder parent,
// whose path is parentRel, or that the parent lists only under another spelling: its rows are marked deleted unless the parent is empty while the
// catalog lists other live files under it (an unmounted nested mount looks exactly like that).
func (s *scan) goneTarget(ctx context.Context, parent *os.Root, parentRel, t string) (Target, error) {
	f, err := parent.Open(".")
	if err != nil {
		return fatalOrDrop(t, parentRel, err)
	}
	names, err := f.Readdirnames(1)
	_ = f.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return fatalOrDrop(t, parentRel, err)
	}
	if len(names) > 0 {
		return Target{Path: t, State: TargetGone}, nil
	}
	n, err := s.sc.store.liveUnderExcept(ctx, s.src.id, parentRel, t)
	if err != nil {
		return Target{}, err
	}
	if n > 0 {
		return Target{Path: t, State: TargetRefused, Reason: fmt.Sprintf("it is gone and %s is empty while the catalog lists %d other files in it; is the share mounted?",
			s.where(parentRel), n)}, nil
	}
	return Target{Path: t, State: TargetGone}, nil
}

// prefixRange returns the bounds of the relative paths strictly inside the folder p: every such
// path is >= lo and < hi in byte order ("/" + 1 is "0").
func prefixRange(p string) (lo, hi string) { return p + "/", p + "0" }

// liveUnderExcept counts the live rows of a source under the folder parent ("." = the whole
// source) that are neither the path except nor inside it.
func (s *Store) liveUnderExcept(ctx context.Context, sourceID int64, parent, except string) (int64, error) {
	elo, ehi := prefixRange(except)
	q := `SELECT COUNT(*) FROM catalog_files WHERE source_id = ? AND deleted_at IS NULL
		AND rel_path <> ? AND NOT (rel_path >= ? AND rel_path < ?)`
	args := []any{sourceID, except, elo, ehi}
	if parent != "." {
		lo, hi := prefixRange(parent)
		q += ` AND rel_path >= ? AND rel_path < ?`
		args = append(args, lo, hi)
	}
	var n int64
	if err := s.db.Reader().QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count catalog files under %s: %w", parent, err)
	}
	return n, nil
}

// nominatePartners finds, for every file of the targets with a link count of 2 or more, the live
// rows outside the targets that the catalog recorded with the same (dev, ino), lstats each one
// now and adds it to the scan (and to its hardlink candidates) when its current metadata equals
// the file's. It returns the partners' relative paths.
func (s *scan) nominatePartners(ctx context.Context, scopes []string) ([]string, error) {
	if len(s.links) == 0 {
		return nil, nil
	}
	keys := make([]devIno, 0, len(s.links))
	for k := range s.links {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b devIno) int {
		if a.dev != b.dev {
			return cmpUint(a.dev, b.dev)
		}
		return cmpUint(a.ino, b.ino)
	})
	var partners []string
	for _, k := range keys {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cands := s.links[k]
		want := cands[0].m
		rels, err := s.sc.store.relsWithInode(ctx, s.src.id, k)
		if err != nil {
			return nil, err
		}
		for _, rel := range rels {
			if UnderAny(rel, scopes) || slices.ContainsFunc(cands, func(c linkCand) bool { return c.rel == rel }) {
				continue
			}
			m, ok, err := s.lstatPartner(rel)
			if err != nil {
				return nil, err
			}
			if !ok || m != want {
				s.rep.Log(slog.LevelDebug, "not a hardlink partner any more", "path", rel)
				continue
			}
			s.links[k] = append(s.links[k], linkCand{rel: rel, m: m})
			s.lookup = append(s.lookup, walked{rel: rel, m: m})
			partners = append(partners, rel)
		}
	}
	return partners, nil
}

func cmpUint(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// relsWithInode returns the relative paths of a source's live rows recorded with (dev, ino).
func (s *Store) relsWithInode(ctx context.Context, sourceID int64, k devIno) ([]string, error) {
	rows, err := s.db.Reader().QueryContext(ctx, `SELECT rel_path FROM catalog_files
		WHERE source_id = ? AND dev = ? AND inode = ? AND deleted_at IS NULL ORDER BY rel_path`, sourceID, int64(k.dev), int64(k.ino))
	if err != nil {
		return nil, fmt.Errorf("find hardlink partners: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var rel string
		if err := rows.Scan(&rel); err != nil {
			return nil, fmt.Errorf("find hardlink partners: %w", err)
		}
		out = append(out, rel)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("find hardlink partners: %w", err)
	}
	return out, nil
}

// lstatPartner lstats the regular file rel through the source root, resolving it component by
// component like a target: every name listed by its folder under exactly that spelling, every
// ancestor a real directory that is neither excluded nor forbidden, the file itself regular and
// not excluded. ok is false when it is not such a file.
func (s *scan) lstatPartner(rel string) (Meta, bool, error) {
	comps := strings.Split(rel, "/")
	dir, cur := s.root, "."
	var opened []*os.Root
	defer func() {
		for _, r := range opened {
			_ = r.Close()
		}
	}()
	fail := func(err error) (Meta, bool, error) {
		if isFatalIO(err) {
			return Meta{}, false, refuse(err, "I/O error reading %s: %v; is the share still mounted? (nothing was changed)", rel, err)
		}
		return Meta{}, false, nil
	}
	for _, name := range comps[:len(comps)-1] {
		child := joinRel(cur, name)
		fi, err := dir.Lstat(name)
		if err != nil {
			return fail(err)
		}
		exact, err := s.listedExactly(dir, cur, name)
		if err != nil {
			return fail(err)
		}
		if !exact || !fi.IsDir() || fi.Mode()&fs.ModeSymlink != 0 || s.match.excluded(child, name, true) {
			return Meta{}, false, nil
		}
		m, ok := MetaOf(fi)
		if !ok {
			return Meta{}, false, nil
		}
		if _, bad := s.forbidden[devIno{m.Dev, m.Inode}]; bad {
			return Meta{}, false, nil
		}
		sub, err := dir.OpenRoot(name)
		if err != nil {
			return fail(err)
		}
		opened = append(opened, sub)
		si, err := sub.Stat(".")
		if err != nil {
			return fail(err)
		}
		if sm, ok := MetaOf(si); !ok || sm.Dev != m.Dev || sm.Inode != m.Inode {
			return Meta{}, false, nil
		}
		dir, cur = sub, child
	}
	leaf := comps[len(comps)-1]
	fi, err := dir.Lstat(leaf)
	if err != nil {
		return fail(err)
	}
	exact, err := s.listedExactly(dir, cur, leaf)
	if err != nil {
		return fail(err)
	}
	if !exact || !fi.Mode().IsRegular() || s.match.excluded(rel, leaf, false) {
		return Meta{}, false, nil
	}
	m, ok := MetaOf(fi)
	return m, ok, nil
}

// finishTargets marks the rows under the scanned targets that the walk did not see deleted
// (except under unreadable folders), regroups the hardlinks of the targets and their partners
// with this scan's group ids, and stores the new scan_seq and statistics, in one transaction.
func (s *scan) finishTargets(ctx context.Context, scopes, partners []string) error {
	id := s.src.id
	var deleted int64
	err := s.sc.store.db.Write(ctx, func(tx *sql.Tx) error {
		var (
			seq      int64
			rawStats string
		)
		if err := tx.QueryRowContext(ctx, `SELECT scan_seq, stats FROM sources WHERE id = ?`, id).Scan(&seq, &rawStats); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("source %d: %w", id, ErrNotFound)
			}
			return err
		}
		seq++
		for _, t := range scopes {
			lo, hi := prefixRange(t)
			rows, err := tx.QueryContext(ctx, `SELECT id, rel_path FROM catalog_files WHERE source_id = ? AND deleted_at IS NULL
				AND last_seen_at <> ? AND (rel_path = ? OR (rel_path >= ? AND rel_path < ?))`, id, s.stamp, t, lo, hi)
			if err != nil {
				return fmt.Errorf("find deleted files: %w", err)
			}
			var gone []int64
			for rows.Next() {
				var (
					rid int64
					rel string
				)
				if err := rows.Scan(&rid, &rel); err != nil {
					_ = rows.Close()
					return fmt.Errorf("find deleted files: %w", err)
				}
				if !s.underUnreadable(rel) {
					gone = append(gone, rid)
				}
			}
			err = rows.Err()
			_ = rows.Close()
			if err != nil {
				return fmt.Errorf("find deleted files: %w", err)
			}
			for i := 0; i < len(gone); i += s.sc.batchSize {
				chunk := gone[i:min(i+s.sc.batchSize, len(gone))]
				args := make([]any, 0, len(chunk)+1)
				args = append(args, s.stamp)
				for _, gid := range chunk {
					args = append(args, gid)
				}
				if _, err := tx.ExecContext(ctx, `UPDATE catalog_files SET deleted_at = ?, hardlink_group = NULL WHERE id IN (?`+
					strings.Repeat(",?", len(chunk)-1)+`)`, args...); err != nil {
					return fmt.Errorf("mark deleted files: %w", err)
				}
			}
			deleted += int64(len(gone))
			// This scan's groups replace the old ones of every row it saw.
			if _, err := tx.ExecContext(ctx, `UPDATE catalog_files SET hardlink_group = NULL WHERE source_id = ?
				AND hardlink_group IS NOT NULL AND (rel_path = ? OR (rel_path >= ? AND rel_path < ?))`, id, t, lo, hi); err != nil {
				return fmt.Errorf("clear hardlink groups: %w", err)
			}
		}
		stmtClear, err := tx.PrepareContext(ctx, `UPDATE catalog_files SET hardlink_group = NULL WHERE source_id = ? AND rel_path = ?`)
		if err != nil {
			return fmt.Errorf("clear hardlink groups: %w", err)
		}
		defer stmtClear.Close()
		for _, rel := range partners {
			if _, err := stmtClear.ExecContext(ctx, id, rel); err != nil {
				return fmt.Errorf("clear hardlink groups: %w", err)
			}
		}
		if len(s.groups) > 0 {
			stmt, err := tx.PrepareContext(ctx, `UPDATE catalog_files SET hardlink_group = ? WHERE source_id = ? AND rel_path = ?`)
			if err != nil {
				return fmt.Errorf("store hardlink groups: %w", err)
			}
			defer stmt.Close()
			for n, g := range s.groups {
				gid := fmt.Sprintf("%d.%d:%d", id, seq, n+1)
				for _, c := range g {
					if _, err := stmt.ExecContext(ctx, gid, id, c.rel); err != nil {
						return fmt.Errorf("store hardlink groups: %w", err)
					}
				}
			}
		}
		var old Stats
		if err := json.Unmarshal([]byte(rawStats), &old); err != nil {
			return fmt.Errorf("source %d: stats: %w", id, err)
		}
		st, err := computeStats(ctx, tx, id)
		if err != nil {
			return err
		}
		// Only the full scan sees every entry that is not cataloged.
		st.Skipped = old.Skipped
		stats, err := json.Marshal(st)
		if err != nil {
			return fmt.Errorf("catalog stats: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sources SET scan_seq = ?, stats = ? WHERE id = ?`, seq, string(stats), id); err != nil {
			return fmt.Errorf("record targeted scan of source %d: %w", id, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.res.Deleted = deleted
	return nil
}

// LiveUnder returns the live files of a source that are one of paths or lie inside one of them
// (a targeted sync plans only those), ordered by relative path. paths must satisfy
// jobs.ValidTargetPath.
func (s *Store) LiveUnder(ctx context.Context, sourceID int64, paths []string) ([]File, error) {
	targets, err := CanonicalTargets(paths)
	if err != nil {
		return nil, err
	}
	var out []File
	for _, t := range targets {
		lo, hi := prefixRange(t)
		files, err := s.queryFiles(ctx, `WHERE source_id = ? AND deleted_at IS NULL AND (rel_path = ? OR (rel_path >= ? AND rel_path < ?))`,
			sourceID, t, lo, hi)
		if err != nil {
			return nil, err
		}
		out = append(out, files...)
	}
	slices.SortFunc(out, func(a, b File) int { return strings.Compare(a.RelPath, b.RelPath) })
	return out, nil
}

// LiveInGroups returns the live files of a source that belong to the given hardlink groups (the
// partners of a targeted sync's files), ordered by relative path.
func (s *Store) LiveInGroups(ctx context.Context, sourceID int64, groups []string) ([]File, error) {
	var out []File
	for len(groups) > 0 {
		chunk := groups[:min(len(groups), 500)]
		groups = groups[len(chunk):]
		args := []any{sourceID}
		for _, g := range chunk {
			args = append(args, g)
		}
		files, err := s.queryFiles(ctx, `WHERE source_id = ? AND deleted_at IS NULL AND hardlink_group IN (?`+
			strings.Repeat(",?", len(chunk)-1)+`)`, args...)
		if err != nil {
			return nil, err
		}
		out = append(out, files...)
	}
	slices.SortFunc(out, func(a, b File) int { return strings.Compare(a.RelPath, b.RelPath) })
	return out, nil
}

// queryFiles returns the catalog rows matching where.
func (s *Store) queryFiles(ctx context.Context, where string, args ...any) ([]File, error) {
	rows, err := s.db.Reader().QueryContext(ctx, `SELECT `+fileColumns+` FROM catalog_files `+where+` ORDER BY rel_path`, args...)
	if err != nil {
		return nil, fmt.Errorf("read catalog: %w", err)
	}
	defer rows.Close()
	var out []File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, fmt.Errorf("read catalog: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read catalog: %w", err)
	}
	return out, nil
}
