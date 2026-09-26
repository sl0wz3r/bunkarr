package catalog

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// ScanReporter receives a scan's progress and job-log lines. jobs.Reporter satisfies it; nil is
// allowed.
type ScanReporter interface {
	Progress(p jobs.Progress)
	Log(level slog.Level, msg string, args ...any)
}

// Reasons in ScanResult.Skipped.
const (
	SkipSymlink = "symlink"
	SkipDevice  = "device"
	SkipSocket  = "socket"
	SkipFIFO    = "fifo"
	SkipOther   = "other"
	// SkipOverlap is a directory that is a destination root or the config directory (S4).
	SkipOverlap = "overlap"
)

// ScanResult describes one scan of one source.
type ScanResult struct {
	SourceID   int64  `json:"sourceId"`
	SourceName string `json:"sourceName"`
	// StartedAt is the scan's timestamp: rows it saw have LastSeenAt = StartedAt and rows it found
	// gone have DeletedAt = StartedAt (see Store.Deleted).
	StartedAt time.Time `json:"startedAt"`
	FSType    string    `json:"fsType"`
	FUSE      bool      `json:"fuse"`
	// Files and Bytes count the regular files seen (after excludes).
	Files int64 `json:"files"`
	Bytes int64 `json:"bytes"`
	// Added counts new rows and rows that reappeared; Changed counts rows whose size or mtime
	// changed (other metadata changes such as a new inode are recorded but not counted);
	// Deleted counts rows marked deleted.
	Added   int64 `json:"added"`
	Changed int64 `json:"changed"`
	Deleted int64 `json:"deleted"`
	// Skipped counts entries not cataloged, by reason (Skip* constants).
	Skipped map[string]int64 `json:"skipped"`
	// Excluded counts entries matched by an exclude pattern (a directory counts once).
	Excluded int64 `json:"excluded"`
	// UnreadableDirs are directories that could not be read; their rows were left unchanged.
	UnreadableDirs []string `json:"unreadableDirs"`
	Groups         int64    `json:"groups"`
	GroupedFiles   int64    `json:"groupedFiles"`
	// Warnings holds the first warnings (at most 100); WarningCount counts all of them.
	Warnings     []string      `json:"warnings"`
	WarningCount int           `json:"warningCount"`
	Duration     time.Duration `json:"-"`
	DurationMs   int64         `json:"durationMs"`
}

// SkippedTotal sums Skipped.
func (r ScanResult) SkippedTotal() int64 {
	var n int64
	for _, v := range r.Skipped {
		n += v
	}
	return n
}

// ScannerOptions configures a Scanner.
type ScannerOptions struct {
	// ForbiddenRoots returns the destination targets and the config directory. At scan start each
	// one that exists is stat'ed, and directories of the source with the same (dev, ino) are
	// skipped with a warning (safety rule S4: bind-mount aliases). nil means none.
	ForbiddenRoots func(ctx context.Context) ([]string, error)
	// Logger receives server-side log lines (nil discards them).
	Logger *slog.Logger
}

// Scanner scans sources into the catalog. It is safe for concurrent use; scans of one source are
// serialized by the source's lock.
type Scanner struct {
	store *Store
	opts  ScannerOptions
	log   *slog.Logger

	batchSize     int // catalog rows per write transaction
	progressEvery int64
	maxWarnings   int
	// estaleWait is how long a targeted scan waits before its one retry after ESTALE (§9.1).
	estaleWait time.Duration

	// Test hooks.
	beforeReadDir func(rel string) error          // error injected before reading a directory
	identityHook  func(rootIdentity) rootIdentity // rewrites the root's filesystem identity
	hashFile      func(*os.Root, string, Meta) (string, error)
}

// NewScanner returns a scanner that records into store.
func NewScanner(store *Store, o ScannerOptions) *Scanner {
	log := o.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Scanner{
		store:         store,
		opts:          o,
		log:           log,
		batchSize:     1000,
		progressEvery: 500,
		maxWarnings:   100,
		estaleWait:    estaleRetryWait,
		hashFile:      headTailHash,
	}
}

// refusedError is a scan stopped by a safety rule; it matches ErrScanRefused.
type refusedError struct {
	msg   string
	cause error
}

func (e *refusedError) Error() string        { return ErrScanRefused.Error() + ": " + e.msg }
func (e *refusedError) Is(target error) bool { return target == ErrScanRefused }
func (e *refusedError) Unwrap() error        { return e.cause }

func refuse(cause error, format string, args ...any) error {
	return &refusedError{msg: fmt.Sprintf(format, args...), cause: cause}
}

// isFatalIO reports errors that mean the filesystem itself is failing (a lost mount): the scan
// must stop instead of treating files as gone.
func isFatalIO(err error) bool {
	for _, e := range []syscall.Errno{syscall.EIO, syscall.ENOTCONN, syscall.ESTALE, syscall.ETIMEDOUT, syscall.EHOSTDOWN} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// Scan takes the source's lock (waiting for it), scans the source and updates the catalog.
//
// The walk is read-only and writes nothing: a refused scan (an error wrapping ErrScanRefused:
// the root is missing, not a directory, on another filesystem than recorded, empty while the
// catalog has live files, or any ENOTCONN/EIO/ESTALE) leaves the catalog unchanged and only
// records last_scan_status = failed. "Another filesystem" is another filesystem type or another
// device number, except that a device number the kernel assigns at mount time (FUSE such as
// Unraid's /mnt/user, NFS, CIFS, btrfs, ZFS) may change after a reboot: it is then logged and
// recorded. On such a device another root inode is another directory, unless the filesystem
// assigns inode numbers at run time (FUSE, CIFS/SMB). After the walk the changes are written in batches, then one transaction marks unseen
// rows deleted (except under unreadable directories), stores this scan's hardlink groups, the
// statistics and the source's filesystem identity. A cancelled context returns ctx.Err() and never
// marks deletions.
func (sc *Scanner) Scan(ctx context.Context, sourceID int64, rep ScanReporter) (ScanResult, error) {
	unlock, err := sc.store.LockSource(ctx, sourceID)
	if err != nil {
		return ScanResult{SourceID: sourceID}, err
	}
	defer unlock()
	return sc.scan(ctx, sourceID, rep)
}

// ScanLocked is Scan for a caller that already holds Store.LockSource(sourceID) (a sync keeping the
// catalog stable while it scans and plans).
func (sc *Scanner) ScanLocked(ctx context.Context, sourceID int64, rep ScanReporter) (ScanResult, error) {
	if !sc.store.locks.held(sourceID) {
		return ScanResult{SourceID: sourceID}, fmt.Errorf("scan source %d: ScanLocked called without holding the source lock", sourceID)
	}
	return sc.scan(ctx, sourceID, rep)
}

type nopReporter struct{}

func (nopReporter) Progress(jobs.Progress)         {}
func (nopReporter) Log(slog.Level, string, ...any) {}

// scanInput is what a scan needs from the sources table.
type scanInput struct {
	id      int64
	name    string
	path    string
	exclude []string
	ident   identity
	live    int64
}

func (s *Store) loadScanSource(ctx context.Context, id int64) (scanInput, error) {
	src := scanInput{id: id}
	var exclude string
	err := s.db.Reader().QueryRowContext(ctx, `SELECT name, path, exclude, fs_type, root_dev, root_ino FROM sources WHERE id = ?`, id).
		Scan(&src.name, &src.path, &exclude, &src.ident.fsType, &src.ident.rootDev, &src.ident.rootIno)
	if errors.Is(err, sql.ErrNoRows) {
		return src, fmt.Errorf("source %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return src, fmt.Errorf("load source %d: %w", id, err)
	}
	if err := json.Unmarshal([]byte(exclude), &src.exclude); err != nil {
		return src, fmt.Errorf("source %d: exclude: %w", id, err)
	}
	if err := s.db.Reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_files WHERE source_id = ? AND deleted_at IS NULL`, id).
		Scan(&src.live); err != nil {
		return src, fmt.Errorf("load source %d: %w", id, err)
	}
	return src, nil
}

func (s *Store) recordScanFailure(ctx context.Context, id int64) error {
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE sources SET last_scan_at = ?, last_scan_status = ? WHERE id = ?`,
			db.FormatTime(time.Now()), ScanStatusFailed, id)
		if err != nil {
			return fmt.Errorf("record failed scan of source %d: %w", id, err)
		}
		return nil
	})
}

func (sc *Scanner) scan(ctx context.Context, sourceID int64, rep ScanReporter) (ScanResult, error) {
	started := time.Now().UTC()
	if rep == nil {
		rep = nopReporter{}
	}
	src, err := sc.store.loadScanSource(ctx, sourceID)
	if err != nil {
		return ScanResult{SourceID: sourceID}, err
	}
	s := &scan{
		sc:        sc,
		src:       src,
		rep:       rep,
		stamp:     db.FormatTime(started),
		match:     newMatcher(src.exclude),
		forbidden: map[devIno]string{},
		links:     map[devIno][]linkCand{},
		res: ScanResult{SourceID: sourceID, SourceName: src.name, StartedAt: started,
			Skipped: map[string]int64{}, UnreadableDirs: []string{}, Warnings: []string{}},
	}
	err = s.run(ctx)
	s.res.Duration = time.Since(started)
	s.res.DurationMs = s.res.Duration.Milliseconds()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			sc.log.Info("scan cancelled", "source", src.name)
			return s.res, ctxErr
		}
		sc.log.Warn("scan failed", "source", src.name, "error", err)
		if ferr := sc.store.recordScanFailure(ctx, sourceID); ferr != nil {
			sc.log.Error("record failed scan", "source", src.name, "error", ferr)
		}
		return s.res, fmt.Errorf("scan source %q: %w", src.name, err)
	}
	sc.log.Info("scan finished", "source", src.name, "files", s.res.Files, "added", s.res.Added,
		"changed", s.res.Changed, "deleted", s.res.Deleted, "skipped", s.res.SkippedTotal(),
		"warnings", s.res.WarningCount, "duration", s.res.Duration)
	return s.res, nil
}

// rootIdentity is the scanned root's filesystem identity.
type rootIdentity struct {
	fsType string
	fuse   bool
	// anonDev: dev is not stable across mounts (anonymousDev), so a changed dev is not refused.
	anonDev bool
	// stableIno: the root's inode number survives a remount (stableRootIno).
	stableIno bool
	dev       uint64
	ino       uint64
}

// newRootIdentity classifies a root on fsType whose stat metadata is m.
func newRootIdentity(fsType string, fuse bool, m Meta) rootIdentity {
	return rootIdentity{fsType: fsType, fuse: fuse, anonDev: anonymousDev(fsType, m.Dev), stableIno: stableRootIno(fsType),
		dev: m.Dev, ino: m.Inode}
}

// walked is a regular file seen by the walk.
type walked struct {
	rel string
	m   Meta
}

// scan is the state of one scan.
type scan struct {
	sc        *Scanner
	src       scanInput
	rep       ScanReporter
	stamp     string
	match     *matcher
	root      *os.Root
	ident     rootIdentity
	forbidden map[devIno]string
	res       ScanResult
	// devChangedFrom is the recorded root_dev when an anonymous device's number changed.
	devChangedFrom sql.NullInt64

	lookup     []walked // walked files waiting for their catalog lookup
	pending    []walked // new, changed or reappeared files to write
	seen       []int64  // ids of unchanged rows (only last_seen_at is written)
	links      map[devIno][]linkCand
	groups     [][]linkCand
	unreadable []string
	progressAt int64
	// names caches, per folder (relative path), the entry names it lists: a targeted scan reads
	// them to resolve a path by its exact spelling (listedExactly).
	names map[string]map[string]struct{}
}

func (s *scan) warn(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	s.res.WarningCount++
	switch n := len(s.res.Warnings); {
	case n < s.sc.maxWarnings:
		s.res.Warnings = append(s.res.Warnings, msg)
		s.rep.Log(slog.LevelWarn, msg, "source", s.src.name)
	case s.res.WarningCount == s.sc.maxWarnings+1:
		s.rep.Log(slog.LevelWarn, "more warnings are not logged individually", "source", s.src.name)
	}
}

func (s *scan) run(ctx context.Context) error {
	if err := s.openRoot(); err != nil {
		return err
	}
	defer s.root.Close()
	if err := s.loadForbidden(ctx); err != nil {
		return err
	}
	s.rep.Log(slog.LevelInfo, "scanning source", "source", s.src.name, "path", s.src.path, "fsType", s.ident.fsType)
	if err := s.walkDir(ctx, s.root, "."); err != nil {
		return err
	}
	if err := s.flushLookup(ctx); err != nil {
		return err
	}
	if s.res.Files == 0 && s.src.live > 0 {
		return refuse(nil, "%s contains no files but the catalog lists %d; is the share mounted? (nothing was changed)", s.src.path, s.src.live)
	}
	if err := s.buildGroups(ctx); err != nil {
		return err
	}
	// The walk is complete and nothing has been written yet.
	if err := s.commit(ctx); err != nil {
		return err
	}
	if err := s.finish(ctx); err != nil {
		return err
	}
	if s.devChangedFrom.Valid {
		msg := fmt.Sprintf("the %s device number changed from %d to %d on %s (it is assigned at mount time); recorded the new one",
			s.ident.fsType, uint64(s.devChangedFrom.Int64), s.ident.dev, s.src.path)
		s.rep.Log(slog.LevelInfo, msg, "source", s.src.name)
		s.sc.log.Info(msg, "source", s.src.name)
	}
	return nil
}

// openRoot opens the source root and checks it against the recorded identity (S10a).
func (s *scan) openRoot() error {
	p := s.src.path
	li, err := os.Lstat(p)
	if err != nil {
		switch {
		case isFatalIO(err):
			return refuse(err, "I/O error on the source root %s: %v (is the share still mounted?)", p, err)
		case errors.Is(err, fs.ErrNotExist):
			return refuse(err, "the source root %s does not exist (is the share mounted?)", p)
		default:
			return refuse(err, "cannot read the source root %s: %v", p, err)
		}
	}
	if li.Mode()&fs.ModeSymlink != 0 {
		return refuse(nil, "the source root %s is now a symlink; re-save the source to accept it", p)
	}
	if !li.IsDir() {
		return refuse(nil, "the source root %s is not a directory", p)
	}
	root, err := os.OpenRoot(p)
	if err != nil {
		return refuse(err, "cannot open the source root %s: %v", p, err)
	}
	ident, err := rootIdentityOf(root)
	if err != nil {
		_ = root.Close()
		if isFatalIO(err) {
			return refuse(err, "I/O error on the source root %s: %v (is the share still mounted?)", p, err)
		}
		return refuse(err, "cannot read the source root %s: %v", p, err)
	}
	if s.sc.identityHook != nil {
		ident = s.sc.identityHook(ident)
	}
	rec := s.src.ident
	if rec.fsType.Valid && rec.fsType.String != ident.fsType {
		_ = root.Close()
		return refuse(nil, "%s is now on a %s filesystem but was scanned on %s: is the right share or disk mounted? (re-save the source without changing it to accept the change; nothing was changed)",
			p, ident.fsType, rec.fsType.String)
	}
	devChanged := rec.rootDev.Valid && uint64(rec.rootDev.Int64) != ident.dev
	switch {
	case devChanged && !ident.anonDev:
		_ = root.Close()
		return refuse(nil, "%s is now on a different device (%d, recorded %d): is the right share or disk mounted? (re-save the source without changing it to accept the change; nothing was changed)",
			p, ident.dev, uint64(rec.rootDev.Int64))
	case ident.anonDev && ident.stableIno && rec.rootIno.Valid && uint64(rec.rootIno.Int64) != ident.ino:
		// The device number says nothing here (assigned at mount time, and reusable after a
		// reboot), but the root inode survives a remount: another one is another directory, such as
		// the mount point of a dataset or export that failed to mount under a parent of the same type.
		_ = root.Close()
		return refuse(nil, "%s is now a different directory (inode %d, recorded %d): is the right share, pool or dataset mounted? (re-save the source without changing it to accept the change; nothing was changed)",
			p, ident.ino, uint64(rec.rootIno.Int64))
	case devChanged:
		// The kernel assigns this number at mount time, so a reboot or remount can change it: the
		// filesystem type, root inode (where stable), empty-root and I/O error checks still apply,
		// and finish records the new number.
		s.devChangedFrom = rec.rootDev
	}
	s.root = root
	s.ident = ident
	s.res.FSType = ident.fsType
	s.res.FUSE = ident.fuse
	return nil
}

func rootIdentityOf(root *os.Root) (rootIdentity, error) {
	fi, err := root.Stat(".")
	if err != nil {
		return rootIdentity{}, err
	}
	m, ok := MetaOf(fi)
	if !ok {
		return rootIdentity{}, fmt.Errorf("stat %s: no device and inode information", root.Name())
	}
	f, err := root.Open(".")
	if err != nil {
		return rootIdentity{}, err
	}
	defer f.Close()
	name, fuse, err := fsTypeOfFile(f)
	if err != nil {
		return rootIdentity{}, err
	}
	return newRootIdentity(name, fuse, m), nil
}

// loadForbidden stats the destination roots and the config directory (S4).
func (s *scan) loadForbidden(ctx context.Context) error {
	if s.sc.opts.ForbiddenRoots == nil {
		return nil
	}
	paths, err := s.sc.opts.ForbiddenRoots(ctx)
	if err != nil {
		return fmt.Errorf("list destination and config roots: %w", err)
	}
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			continue // not mounted or gone: nothing to alias
		}
		if m, ok := MetaOf(fi); ok {
			s.forbidden[devIno{m.Dev, m.Inode}] = p
		}
	}
	if p, bad := s.forbidden[devIno{s.ident.dev, s.ident.ino}]; bad {
		return refuse(nil, "the source root %s is the same directory as %s (a destination or the config directory)", s.src.path, p)
	}
	return nil
}

func joinRel(dir, name string) string {
	if dir == "." {
		return name
	}
	return dir + "/" + name
}

// dirError handles a directory that could not be opened or read.
func (s *scan) dirError(rel string, err error) error {
	switch {
	case isFatalIO(err):
		return refuse(err, "I/O error reading %s: %v; is the share still mounted? (nothing was changed)", rel, err)
	case rel == ".":
		return refuse(err, "cannot read the source root %s: %v", s.src.path, err)
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		// Removed or renamed while scanning: its rows are marked deleted like any unseen file.
		s.rep.Log(slog.LevelDebug, "directory vanished during the scan", "dir", rel)
		return nil
	default:
		s.unreadable = append(s.unreadable, rel)
		s.res.UnreadableDirs = append(s.res.UnreadableDirs, rel)
		s.warn("cannot read directory %s: %v; its files keep their catalog entries", rel, err)
		return nil
	}
}

// walkDir catalogs the directory dir (opened as an os.Root) whose path relative to the source is
// rel, recursing into subdirectories. Entries are lstat'ed (ReadDir on an os.Root directory uses
// fstatat without following), symlinks are never followed.
func (s *scan) walkDir(ctx context.Context, dir *os.Root, rel string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.sc.beforeReadDir != nil {
		if err := s.sc.beforeReadDir(rel); err != nil {
			return s.dirError(rel, err)
		}
	}
	f, err := dir.Open(".")
	if err != nil {
		return s.dirError(rel, err)
	}
	entries, err := f.ReadDir(-1)
	_ = f.Close()
	if err != nil {
		return s.dirError(rel, err)
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return cmp.Compare(a.Name(), b.Name()) })
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := e.Name()
		child := joinRel(rel, name)
		info, err := e.Info()
		if err != nil {
			if isFatalIO(err) {
				return refuse(err, "I/O error reading %s: %v; is the share still mounted? (nothing was changed)", child, err)
			}
			continue // vanished
		}
		mode := info.Mode()
		switch {
		case mode.IsDir():
			if err := s.enterDir(ctx, dir, child, name, info); err != nil {
				return err
			}
		case mode.IsRegular():
			if s.match.excluded(child, name, false) {
				s.res.Excluded++
				continue
			}
			m, ok := MetaOf(info)
			if !ok {
				s.res.Skipped[SkipOther]++
				continue
			}
			if err := s.addFile(ctx, child, m); err != nil {
				return err
			}
		default:
			if s.match.excluded(child, name, false) {
				s.res.Excluded++
				continue
			}
			reason := skipReason(mode)
			s.res.Skipped[reason]++
			s.rep.Log(slog.LevelDebug, "skipped", "path", child, "reason", reason)
		}
	}
	return nil
}

func skipReason(mode fs.FileMode) string {
	switch {
	case mode&fs.ModeSymlink != 0:
		return SkipSymlink
	case mode&fs.ModeDevice != 0, mode&fs.ModeCharDevice != 0:
		return SkipDevice
	case mode&fs.ModeSocket != 0:
		return SkipSocket
	case mode&fs.ModeNamedPipe != 0:
		return SkipFIFO
	default:
		return SkipOther
	}
}

// enterDir walks the subdirectory name of dir unless it is excluded or forbidden (S4).
func (s *scan) enterDir(ctx context.Context, dir *os.Root, rel, name string, info fs.FileInfo) error {
	if s.match.excluded(rel, name, true) {
		s.res.Excluded++
		return nil
	}
	m, ok := MetaOf(info)
	if !ok {
		s.res.Skipped[SkipOther]++
		return nil
	}
	if p, bad := s.forbidden[devIno{m.Dev, m.Inode}]; bad {
		s.res.Skipped[SkipOverlap]++
		s.warn("skipped %s: it is the same directory as %s (a destination or the config directory)", rel, p)
		return nil
	}
	sub, err := dir.OpenRoot(name)
	if err != nil {
		return s.dirError(rel, err)
	}
	defer sub.Close()
	// os.Root resolves a symlink that stays inside the root; make sure this is still the directory
	// that was lstat'ed, so a directory swapped for a link is never followed.
	si, err := sub.Stat(".")
	if err != nil {
		return s.dirError(rel, err)
	}
	if sm, ok := MetaOf(si); !ok || sm.Dev != m.Dev || sm.Inode != m.Inode {
		s.unreadable = append(s.unreadable, rel)
		s.res.UnreadableDirs = append(s.res.UnreadableDirs, rel)
		s.warn("directory %s changed during the scan; its files keep their catalog entries", rel)
		return nil
	}
	return s.walkDir(ctx, sub, rel)
}

func (s *scan) addFile(ctx context.Context, rel string, m Meta) error {
	s.res.Files++
	s.res.Bytes += m.Size
	if m.Nlink >= 2 {
		k := devIno{m.Dev, m.Inode}
		s.links[k] = append(s.links[k], linkCand{rel: rel, m: m})
	}
	s.lookup = append(s.lookup, walked{rel: rel, m: m})
	if s.res.Files >= s.progressAt {
		s.progressAt = s.res.Files + s.sc.progressEvery
		s.rep.Progress(jobs.Progress{Phase: "scanning", FilesDone: s.res.Files, BytesDone: s.res.Bytes, CurrentFile: rel})
	}
	if len(s.lookup) >= s.sc.batchSize {
		return s.flushLookup(ctx)
	}
	return nil
}

// existingRow is a catalog row as the lookup reads it.
type existingRow struct {
	id      int64
	m       Meta
	deleted bool
}

// flushLookup compares the walked files waiting in s.lookup with their catalog rows (read only)
// and queues the differences.
func (s *scan) flushLookup(ctx context.Context) error {
	if len(s.lookup) == 0 {
		return nil
	}
	args := make([]any, 0, len(s.lookup)+1)
	args = append(args, s.src.id)
	for _, w := range s.lookup {
		args = append(args, w.rel)
	}
	q := `SELECT id, rel_path, size, mtime_ns, ctime_ns, dev, inode, nlink, deleted_at IS NOT NULL
		FROM catalog_files WHERE source_id = ? AND rel_path IN (?` + strings.Repeat(",?", len(s.lookup)-1) + `)`
	rows, err := s.sc.store.db.Reader().QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("read catalog: %w", err)
	}
	existing := make(map[string]existingRow, len(s.lookup))
	for rows.Next() {
		var (
			r               existingRow
			rel             string
			dev, ino, nlink int64
		)
		if err := rows.Scan(&r.id, &rel, &r.m.Size, &r.m.MtimeNs, &r.m.CtimeNs, &dev, &ino, &nlink, &r.deleted); err != nil {
			_ = rows.Close()
			return fmt.Errorf("read catalog: %w", err)
		}
		r.m.Dev, r.m.Inode, r.m.Nlink = uint64(dev), uint64(ino), uint64(nlink)
		existing[rel] = r
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return fmt.Errorf("read catalog: %w", err)
	}
	for _, w := range s.lookup {
		r, ok := existing[w.rel]
		switch {
		case !ok, r.deleted:
			s.res.Added++
			s.pending = append(s.pending, w)
		case r.m != w.m:
			if r.m.Size != w.m.Size || r.m.MtimeNs != w.m.MtimeNs {
				s.res.Changed++
			}
			s.pending = append(s.pending, w)
		default:
			s.seen = append(s.seen, r.id)
		}
	}
	s.lookup = s.lookup[:0]
	return nil
}

// buildGroups computes this scan's hardlink groups (design §4.3). On FUSE sources every group is
// confirmed by equal head/tail hashes.
func (s *scan) buildGroups(ctx context.Context) error {
	groups, warnings := planGroups(s.links)
	for _, w := range warnings {
		s.warn("%s", w)
	}
	if !s.ident.fuse {
		s.groups = groups
	} else {
		for _, g := range groups {
			if err := ctx.Err(); err != nil {
				return err
			}
			ok, err := s.confirmFUSE(g)
			if err != nil {
				return err
			}
			if ok {
				s.groups = append(s.groups, g)
			}
		}
	}
	for _, g := range s.groups {
		s.res.Groups++
		s.res.GroupedFiles += int64(len(g))
	}
	return nil
}

func (s *scan) confirmFUSE(g []linkCand) (bool, error) {
	var first string
	for i, c := range g {
		h, err := s.sc.hashFile(s.root, c.rel, c.m)
		if err != nil {
			if isFatalIO(err) {
				return false, refuse(err, "I/O error reading %s: %v; is the share still mounted? (nothing was changed)", c.rel, err)
			}
			s.warn("hardlinks not grouped: cannot confirm %s on the FUSE source: %v", c.rel, err)
			return false, nil
		}
		if i == 0 {
			first = h
		} else if h != first {
			s.warn("hardlinks not grouped: %s and %s share an inode number but not their content (FUSE inode numbers are not stable)", g[0].rel, c.rel)
			return false, nil
		}
	}
	return true, nil
}

// upsertFile writes a new, changed or reappeared file. A changed row loses its hardlink group
// until the final transaction assigns this scan's groups, so a group never joins rows whose
// metadata changed since the scan that grouped them.
const upsertFile = `INSERT INTO catalog_files (source_id, rel_path, size, mtime_ns, ctime_ns, dev, inode, nlink,
	first_seen_at, last_seen_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT (source_id, rel_path) DO UPDATE SET size = excluded.size, mtime_ns = excluded.mtime_ns,
	ctime_ns = excluded.ctime_ns, dev = excluded.dev, inode = excluded.inode, nlink = excluded.nlink,
	hardlink_group = NULL, last_seen_at = excluded.last_seen_at, deleted_at = NULL`

// commit writes the queued changes and the last_seen_at of unchanged rows, batchSize rows per
// transaction. It never marks deletions.
func (s *scan) commit(ctx context.Context) error {
	bs := s.sc.batchSize
	for i := 0; i < len(s.pending); i += bs {
		chunk := s.pending[i:min(i+bs, len(s.pending))]
		err := s.sc.store.db.Write(ctx, func(tx *sql.Tx) error {
			stmt, err := tx.PrepareContext(ctx, upsertFile)
			if err != nil {
				return err
			}
			defer stmt.Close()
			for _, w := range chunk {
				if _, err := stmt.ExecContext(ctx, s.src.id, w.rel, w.m.Size, w.m.MtimeNs, w.m.CtimeNs,
					int64(w.m.Dev), int64(w.m.Inode), int64(w.m.Nlink), s.stamp, s.stamp); err != nil {
					return fmt.Errorf("%s: %w", w.rel, err)
				}
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("record scanned files: %w", err)
		}
		faultinject.Point("scan.afterBatch")
	}
	for i := 0; i < len(s.seen); i += bs {
		chunk := s.seen[i:min(i+bs, len(s.seen))]
		args := make([]any, 0, len(chunk)+1)
		args = append(args, s.stamp)
		for _, id := range chunk {
			args = append(args, id)
		}
		err := s.sc.store.db.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE catalog_files SET last_seen_at = ? WHERE id IN (?`+
				strings.Repeat(",?", len(chunk)-1)+`)`, args...)
			return err
		})
		if err != nil {
			return fmt.Errorf("record scanned files: %w", err)
		}
		faultinject.Point("scan.afterBatch")
	}
	return nil
}

func (s *scan) underUnreadable(rel string) bool {
	for _, d := range s.unreadable {
		if strings.HasPrefix(rel, d+"/") {
			return true
		}
	}
	return false
}

// finish marks unseen rows deleted, stores this scan's hardlink groups, the statistics and the
// root's identity, in one transaction.
func (s *scan) finish(ctx context.Context) error {
	status := ScanStatusOK
	if s.res.WarningCount > 0 {
		status = ScanStatusWarnings
	}
	id := s.src.id
	var deleted int64
	err := s.sc.store.db.Write(ctx, func(tx *sql.Tx) error {
		var seq int64
		if err := tx.QueryRowContext(ctx, `SELECT scan_seq FROM sources WHERE id = ?`, id).Scan(&seq); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("source %d: %w", id, ErrNotFound)
			}
			return err
		}
		seq++

		gone, err := s.unseen(ctx, tx)
		if err != nil {
			return err
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
		deleted = int64(len(gone))

		if _, err := tx.ExecContext(ctx, `UPDATE catalog_files SET hardlink_group = NULL WHERE source_id = ? AND hardlink_group IS NOT NULL`, id); err != nil {
			return fmt.Errorf("clear hardlink groups: %w", err)
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

		st, err := computeStats(ctx, tx, id)
		if err != nil {
			return err
		}
		st.Skipped = s.res.SkippedTotal()
		stats, err := json.Marshal(st)
		if err != nil {
			return fmt.Errorf("catalog stats: %w", err)
		}
		_, err = tx.ExecContext(ctx, `UPDATE sources SET fs_type = ?, root_dev = ?, root_ino = ?, scan_seq = ?, stats = ?,
			last_scan_at = ?, last_scan_status = ? WHERE id = ?`,
			s.ident.fsType, int64(s.ident.dev), int64(s.ident.ino), seq, string(stats), db.FormatTime(time.Now()), status, id)
		if err != nil {
			return fmt.Errorf("record scan of source %d: %w", id, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.res.Deleted = deleted
	return nil
}

// unseen returns the ids of live rows this scan did not see, except those under unreadable
// directories.
func (s *scan) unseen(ctx context.Context, tx *sql.Tx) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, rel_path FROM catalog_files WHERE source_id = ? AND deleted_at IS NULL AND last_seen_at <> ?`,
		s.src.id, s.stamp)
	if err != nil {
		return nil, fmt.Errorf("find deleted files: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var (
			id  int64
			rel string
		)
		if err := rows.Scan(&id, &rel); err != nil {
			return nil, fmt.Errorf("find deleted files: %w", err)
		}
		if !s.underUnreadable(rel) {
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("find deleted files: %w", err)
	}
	return ids, nil
}
