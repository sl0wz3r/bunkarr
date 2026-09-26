package catalog

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Locating external paths (docs/design/phase2-3.md §4.1, safety rule S18). Other applications
// (the *arrs, Plex) report container paths; after their integration's path mappings turned one
// into a path as Bunkarr sees it, Locate finds the sources that contain it. This is the only way
// external paths reach the catalog: a located path is used as a catalog key (source id plus
// relative path) and as a scan scope inside the source's os.Root (S1), never opened as it is.

// Location is where a local path lies in a source.
type Location struct {
	// SourceID is the containing source.
	SourceID int64 `json:"sourceId"`
	// Rel is the path relative to the source root, slash-separated; "" is the source root itself.
	Rel string `json:"relPath"`
}

// Locator locates local paths in a fixed set of sources: the refresh loads the sources once
// (they are few) and locates every path of an *arr against them.
type Locator struct {
	// sources are ordered by path length, longest first, then by id.
	sources []locatorSource
}

type locatorSource struct {
	id      int64
	path    string
	enabled bool
}

// NewLocator returns a locator over sources (their stored, resolved paths).
func NewLocator(sources []Source) *Locator {
	l := &Locator{sources: make([]locatorSource, 0, len(sources))}
	for _, s := range sources {
		p := path.Clean(s.Path)
		if !path.IsAbs(p) {
			continue
		}
		l.sources = append(l.sources, locatorSource{id: s.ID, path: p, enabled: s.Enabled})
	}
	slices.SortStableFunc(l.sources, func(a, b locatorSource) int {
		if len(a.path) != len(b.path) {
			return len(b.path) - len(a.path)
		}
		return cmp.Compare(a.id, b.id)
	})
	return l
}

// Locator returns a locator over every source (enabled or not) as stored now.
func (s *Store) Locator(ctx context.Context) (*Locator, error) {
	list, err := s.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("locate: %w", err)
	}
	return NewLocator(list), nil
}

// Locate returns every source whose path is a path-segment-boundary prefix of localPath (/media
// contains /media and /media/movies, never /media2), longest prefix first; sources may overlap.
// Only clean absolute paths are located: any other path, and a path inside no source, gives none
// (design S18: such a path is unmapped, never an error).
func (l *Locator) Locate(localPath string) []Location {
	if !LocatablePath(localPath) {
		return nil
	}
	var out []Location
	for _, src := range l.sources {
		if rel, ok := locateRel(localPath, src.path); ok {
			out = append(out, Location{SourceID: src.id, Rel: rel})
		}
	}
	return out
}

// Path returns the local path of a location: the source's path joined with Rel. ok is false for
// a source the locator does not know.
func (l *Locator) Path(loc Location) (string, bool) {
	for _, src := range l.sources {
		if src.id == loc.SourceID {
			if loc.Rel == "" {
				return src.path, true
			}
			return path.Join(src.path, loc.Rel), true
		}
	}
	return "", false
}

// Enabled reports whether the locator knows source id and it is enabled.
func (l *Locator) Enabled(id int64) bool {
	for _, src := range l.sources {
		if src.id == id {
			return src.enabled
		}
	}
	return false
}

// Locate is Locator.Locate over the sources as stored now.
func (s *Store) Locate(ctx context.Context, localPath string) ([]Location, error) {
	l, err := s.Locator(ctx)
	if err != nil {
		return nil, err
	}
	return l.Locate(localPath), nil
}

// LocatablePath reports whether p is a clean absolute slash path (path.Clean(p) == p), the only form
// Locate accepts.
func LocatablePath(p string) bool {
	return p != "" && path.IsAbs(p) && path.Clean(p) == p && !strings.ContainsRune(p, 0)
}

// locateRel returns p relative to root when root is a segment-boundary prefix of p.
func locateRel(p, root string) (string, bool) {
	switch {
	case p == root:
		return "", true
	case root == "/":
		return strings.TrimPrefix(p, "/"), true
	case strings.HasPrefix(p, root+"/"):
		return p[len(root)+1:], true
	}
	return "", false
}

// LiveFile is a live catalog file found at a location.
type LiveFile struct {
	ID      int64
	Size    int64
	MtimeNs int64
}

// locateQueryer runs read queries; *sql.DB and *sql.Tx satisfy it.
type locateQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// liveAtBatch bounds the relative paths of one query of LiveFilesAt.
const liveAtBatch = 500

// LiveFilesAt returns the live (not deleted) catalog files at the given locations, keyed by
// location; a location without one is absent. q is the read pool or a read transaction (nil: the
// read pool), so a caller can read the catalog and its own tables from one snapshot.
func (s *Store) LiveFilesAt(ctx context.Context, q locateQueryer, locs []Location) (map[Location]LiveFile, error) {
	if q == nil {
		q = s.db.Reader()
	}
	bySource := map[int64][]string{}
	for _, l := range locs {
		if l.Rel != "" {
			bySource[l.SourceID] = append(bySource[l.SourceID], l.Rel)
		}
	}
	out := make(map[Location]LiveFile, len(locs))
	for src, rels := range bySource {
		slices.Sort(rels)
		rels = slices.Compact(rels)
		for len(rels) > 0 {
			n := min(len(rels), liveAtBatch)
			chunk := rels[:n]
			rels = rels[n:]
			args := make([]any, 0, n+1)
			args = append(args, src)
			for _, r := range chunk {
				args = append(args, r)
			}
			rows, err := q.QueryContext(ctx, `SELECT id, rel_path, size, mtime_ns FROM catalog_files
				WHERE source_id = ? AND deleted_at IS NULL AND rel_path IN (?`+strings.Repeat(",?", n-1)+`)`, args...)
			if err != nil {
				return nil, fmt.Errorf("read catalog files of source %d: %w", src, err)
			}
			for rows.Next() {
				var (
					f   LiveFile
					rel string
				)
				if err := rows.Scan(&f.ID, &rel, &f.Size, &f.MtimeNs); err != nil {
					_ = rows.Close()
					return nil, fmt.Errorf("read catalog files of source %d: %w", src, err)
				}
				out[Location{SourceID: src, Rel: rel}] = f
			}
			if err := rows.Close(); err != nil {
				return nil, fmt.Errorf("read catalog files of source %d: %w", src, err)
			}
			if err := rows.Err(); err != nil {
				return nil, fmt.Errorf("read catalog files of source %d: %w", src, err)
			}
		}
	}
	return out, nil
}

// DirExists reports whether a location is a directory inside its source, looked up through the
// source's os.Root without following a symlink at the location itself (design §6.1: a refresh
// checks an item's located folder before it queues a targeted sync of it). A location that does
// not exist, is not a directory, is a symlink or exists only under another spelling of one of its
// names (a case- or normalization-insensitive filesystem) gives false; an unknown source gives an error
// wrapping ErrNotFound, and an unreadable source root an error.
func (s *Store) DirExists(ctx context.Context, loc Location) (bool, error) {
	src, err := s.Get(ctx, loc.SourceID)
	if err != nil {
		return false, err
	}
	if loc.Rel != "" && !locateRelOK(loc.Rel) {
		return false, nil
	}
	root, err := os.OpenRoot(src.Path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("open source %q: %w", src.Name, err)
	}
	defer root.Close()
	name := loc.Rel
	if name == "" {
		name = "."
	}
	fi, err := root.Lstat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("look up %q in source %q: %w", loc.Rel, src.Name, err)
	case !fi.IsDir():
		return false, nil
	}
	// On a case- or normalization-insensitive filesystem (an SMB share, APFS) the lookup also
	// finds another spelling of the folder; a targeted sync of that spelling could not scan it
	// (the scan keys rows by the names on disk), so every name must be listed exactly as given.
	if loc.Rel == "" {
		return true, nil
	}
	dir := "."
	for _, name := range strings.Split(loc.Rel, "/") {
		ok, err := s.dirs.lists(root, src.Path, dir, name)
		switch {
		case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
			return false, nil
		case err != nil:
			return false, fmt.Errorf("list %q in source %q: %w", dir, src.Name, err)
		case !ok:
			return false, nil
		}
		dir = path.Join(dir, name)
	}
	return true, nil
}

// Folder listings DirExists reads are kept per source root and folder, so a refresh checking the
// folders of many items in one library folder reads that folder once, not once per item. A kept
// listing is used only while its folder's stat (device, inode, size, mtime and ctime) is
// unchanged, for at most listingTTL, and only to confirm a name (a miss lists the folder again). A
// listing whose folder changed within the racy window of the read is not kept: another change in
// the same timestamp tick (a coarse kernel clock, FAT's 2 s mtime) would leave the stat as it
// was. DirExists only chooses between a targeted and a
// whole-source sync, and a targeted scan lists every folder again itself, so a stale answer never
// reaches the catalog.
const (
	listingTTL        = 30 * time.Second
	listingRacyWindow = 2 * time.Second
	maxListedNames    = 1 << 20 // names kept across all listings
)

// dirListings is the listing cache of a Store. The zero value is ready to use.
type dirListings struct {
	mu    sync.Mutex
	m     map[listingKey]*dirListing
	names int       // names held by m
	swept time.Time // when expired listings were last dropped
	reads int       // listings read (tests)
	racy  time.Duration
}

type listingKey struct{ root, dir string }

type dirListing struct {
	stamp dirStamp
	read  time.Time
	names map[string]struct{}
}

type dirStamp struct {
	dev, ino           uint64
	size, mtime, ctime int64
}

func stampOf(fi fs.FileInfo) (dirStamp, bool) {
	m, ok := MetaOf(fi)
	return dirStamp{m.Dev, m.Inode, m.Size, m.MtimeNs, m.CtimeNs}, ok
}

// lists reports whether the folder dir of root, the source root at rootPath, lists an entry
// spelled exactly name, byte for byte. Callers ask only about names the lookup found, so a kept
// listing answers only when it has the name: one that lacks it is read again, because on a union
// filesystem (mergerfs stats a folder on its first branch but lists every branch) a folder created
// on another branch leaves the stat as it was.
func (c *dirListings) lists(root *os.Root, rootPath, dir, name string) (bool, error) {
	key := listingKey{rootPath, dir}
	c.mu.Lock()
	l := c.m[key]
	c.mu.Unlock()
	if l != nil && time.Since(l.read) < listingTTL {
		if _, found := l.names[name]; found {
			fi, err := root.Stat(dir)
			if err != nil {
				return false, err
			}
			if st, ok := stampOf(fi); ok && st == l.stamp {
				return true, nil
			}
		}
	}
	start := time.Now()
	f, err := root.Open(dir)
	if err != nil {
		return false, err
	}
	fi, err := f.Stat() // before the read: a change during it makes the next stat differ
	if err != nil {
		_ = f.Close()
		return false, err
	}
	list, err := f.Readdirnames(-1)
	_ = f.Close()
	if err != nil {
		return false, err
	}
	names := make(map[string]struct{}, len(list))
	for _, n := range list {
		names[n] = struct{}{}
	}
	_, found := names[name]
	st, ok := stampOf(fi)
	c.keep(key, &dirListing{stamp: st, read: start, names: names}, ok)
	return found, nil
}

// keep counts a listing read and keeps it when its stat is known and not racy.
func (c *dirListings) keep(key listingKey, l *dirListing, stamped bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads++
	if old := c.m[key]; old != nil {
		c.names -= len(old.names)
		delete(c.m, key)
	}
	racy := c.racy
	if racy == 0 {
		racy = listingRacyWindow
	}
	changed := time.Unix(0, max(l.stamp.mtime, l.stamp.ctime))
	if !stamped || !changed.Before(l.read.Add(-racy)) || len(l.names) > maxListedNames {
		return
	}
	if c.m == nil {
		c.m = map[listingKey]*dirListing{}
	}
	if now := time.Now(); now.Sub(c.swept) >= listingTTL {
		for k, v := range c.m {
			if now.Sub(v.read) >= listingTTL {
				c.names -= len(v.names)
				delete(c.m, k)
			}
		}
		c.swept = now
	}
	if c.names+len(l.names) > maxListedNames {
		clear(c.m)
		c.names = 0
	}
	c.m[key] = l
	c.names += len(l.names)
}

// locateRelOK reports whether rel is a clean, relative slash path without ".." elements.
func locateRelOK(rel string) bool {
	if rel == "" || path.IsAbs(rel) || path.Clean(rel) != rel || strings.ContainsRune(rel, 0) {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, "../")
}
