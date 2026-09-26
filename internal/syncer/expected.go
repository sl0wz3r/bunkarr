package syncer

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"strings"
	"syscall"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// Expected files of a webhook sync (docs/design/phase2-3.md §9.1). After the targeted scan, the
// files the *arr index expects under the targets are compared with the catalog. A file that is not
// live there with the *arr's size is looked up on disk the way the scan resolves it (one component
// at a time: the name its folder lists, lstat, the source's exclude patterns), and sorted:
//   - pending: it may still show up, so the targets are scanned again after 5, 15 and 30 s.
//     Nothing is at its path yet (a mount that shows a new file late), or a regular file is there
//     that the scan did not list, or its size on disk is not the catalog's (still being written,
//     or replaced while the scan read the attributes an NFS client had cached: the size is read
//     from an open file, which revalidates them). A lookup that fails with an error of a failing
//     filesystem (EIO, ESTALE: the rescan refuses then) is pending too.
//   - never: the scan never catalogs it, so it is reported once and not waited for. It is a
//     symbolic link (never followed: a debrid or rclone setup whose *arr imports symlinks), under
//     an excluded name or a symlinked folder, something other than a regular file, or in a folder
//     the scan cannot read (EACCES: an *arr running with another PUID or umask; the scan keeps
//     that folder's rows as they are).
//   - other spelling: a folder under the target lists a component of its path only under another
//     spelling (case, or Unicode normalization on a filesystem that ignores it: APFS, an SMB
//     share). The scan catalogs the names folders list, never the *arr's spelling, so it is
//     reported once and not waited for.
//   - target spelled otherwise: the component spelled otherwise is the target itself or a folder
//     above it. The targeted scan walks a target only under the spelling it was given
//     (catalog.scan.walkTarget: it is gone under that spelling), so nothing under it is scanned
//     and the file is not backed up by this sync. It is a warning counted as missing, with no
//     wait: rescanning the same spelling finds nothing more, and the next full sync backs it up.
//   - other size: the catalog lists it with the size it has on disk, which differs from the
//     *arr's (a file transcoded in place, until the *arr rescans it). This is one warning, with no
//     wait: the file is backed up as it is.

// expectedState is what a check found about an expected file that is not live with its size.
type expectedState int

const (
	expectedPending expectedState = iota
	expectedNever
	expectedOtherSpelling
	expectedTargetSpelling
	expectedOtherSize
)

// expectedCheck is an expected file that is not live in the catalog with the *arr's size.
type expectedCheck struct {
	ExpectedFile
	state expectedState
	// reason says why the scan never catalogs it under that path (expectedNever,
	// expectedOtherSpelling, expectedTargetSpelling).
	reason string
	// at is the path of the component spelled otherwise on disk (expectedOtherSpelling).
	at string
	// size is its size on disk and in the catalog (expectedOtherSize).
	size int64
}

// checkExpected returns the files the *arr index expects under scopes that the catalog does not
// list live with the *arr's size, each sorted as the comment at the top of this file says.
func (s *syncRun) checkExpected(ctx context.Context, src catalog.Source, scopes []string) ([]expectedCheck, error) {
	exp, err := s.r.expected(ctx, src, scopes)
	if err != nil {
		return nil, fmt.Errorf("read the files the *arr expects: %w", err)
	}
	if len(exp) == 0 {
		return nil, nil
	}
	locs := make([]catalog.Location, 0, len(exp))
	for _, e := range exp {
		locs = append(locs, catalog.Location{SourceID: src.ID, Rel: e.RelPath})
	}
	live, err := s.r.cat.LiveFilesAt(ctx, nil, locs)
	if err != nil {
		return nil, err
	}
	var (
		out   []expectedCheck
		root  *os.Root
		match excluder
		dirs  = folderNames{}
	)
	defer func() {
		if root != nil {
			_ = root.Close()
		}
	}()
	for _, e := range exp {
		f, cataloged := live[catalog.Location{SourceID: src.ID, Rel: e.RelPath}]
		if cataloged && f.Size == e.Size {
			continue
		}
		if root == nil {
			if root, err = os.OpenRoot(src.Path); err != nil {
				// The scan has just read the root: wait and scan again, as for a file not there yet.
				root = nil
				out = append(out, expectedCheck{ExpectedFile: e})
				continue
			}
			match = newExcluder(src.Exclude)
		}
		c := classifyExpected(root, match, dirs, e, f, cataloged)
		if c.state == expectedOtherSpelling && !insideAny(c.at, scopes) {
			// The scan walked no folder above the component spelled otherwise: that is the target
			// (or a folder above it), which is gone under the *arr's spelling.
			c.state = expectedTargetSpelling
		}
		out = append(out, c)
	}
	return out, nil
}

// insideAny reports whether rel lies strictly inside one of the folders dirs.
func insideAny(rel string, dirs []string) bool {
	for _, d := range dirs {
		if strings.HasPrefix(rel, d+"/") {
			return true
		}
	}
	return false
}

// classifyExpected looks up e on disk through the source root: f is its live catalog row
// (cataloged false: none). dirs holds the folder listings read by this check.
func classifyExpected(root *os.Root, match excluder, dirs folderNames, e ExpectedFile, f catalog.LiveFile, cataloged bool) expectedCheck {
	c := expectedCheck{ExpectedFile: e, state: expectedPending}
	comps := strings.Split(e.RelPath, "/")
	for i, name := range comps {
		parent := "."
		if i > 0 {
			parent = strings.Join(comps[:i], "/")
		}
		rel := strings.Join(comps[:i+1], "/")
		leaf := i == len(comps)-1
		// The scan keys rows by the names folders list (catalog.scan.listedExactly), so a name the
		// folder lists only under another spelling is never cataloged under this one, although a
		// lookup on a case- or normalization-insensitive filesystem finds it.
		names, err := dirs.read(root, parent)
		if err != nil {
			return c.unreadableFolder(parent, err)
		}
		if _, listed := names[name]; !listed {
			if other := otherSpelling(names, name); other != "" {
				c.state, c.reason, c.at = expectedOtherSpelling, fmt.Sprintf("%s is spelled %s on disk", rel, path.Join(parent, other)), rel
				return c
			}
			if _, err := root.Lstat(rel); err != nil {
				return c // not there (yet)
			}
			// A lookup finds it: it was created after the folder was listed, or the filesystem
			// ignores case or normalization. The folder is listed again to tell.
			delete(dirs, parent)
			if names, err = dirs.read(root, parent); err != nil {
				return c.unreadableFolder(parent, err)
			}
			if _, listed := names[name]; !listed {
				c.state, c.reason, c.at = expectedOtherSpelling, fmt.Sprintf("%s is spelled differently on disk (case or Unicode normalization)", rel), rel
				return c
			}
		}
		fi, err := root.Lstat(rel)
		if err != nil {
			if lookupMayChange(err) {
				return c // gone since the listing, or a failing filesystem: scanning again tells
			}
			c.state, c.reason = expectedNever, fmt.Sprintf("%s cannot be read (%v)", rel, errnoOf(err))
			return c
		}
		mode := fi.Mode()
		switch {
		case mode&fs.ModeSymlink != 0:
			c.state, c.reason = expectedNever, fmt.Sprintf("%s is a symbolic link, which is never followed", rel)
			return c
		case !leaf && !mode.IsDir():
			return c
		case match.excluded(rel, name, !leaf):
			c.state, c.reason = expectedNever, fmt.Sprintf("%s is excluded", rel)
			return c
		case leaf && !mode.IsRegular():
			c.state, c.reason = expectedNever, fmt.Sprintf("it is not a regular file (%s)", mode.Type())
			return c
		case leaf && cataloged && currentSize(root, rel, fi.Size()) == f.Size:
			// The catalog is current: the *arr's size is the stale one.
			c.state, c.size = expectedOtherSize, f.Size
		}
	}
	return c
}

// unreadableFolder returns c for an error listing the folder dir: never when the scan cannot read
// that folder either, else pending.
func (c expectedCheck) unreadableFolder(dir string, err error) expectedCheck {
	if !lookupMayChange(err) {
		c.state, c.reason = expectedNever, fmt.Sprintf("the folder %s cannot be read (%v)", dir, errnoOf(err))
	}
	return c
}

// folderNames caches the entry names of the folders (relative to the source root) that one check
// listed.
type folderNames map[string]map[string]struct{}

// read returns the names the folder dir lists.
func (d folderNames) read(root *os.Root, dir string) (map[string]struct{}, error) {
	if names, ok := d[dir]; ok {
		return names, nil
	}
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
	d[dir] = names
	return names, nil
}

// otherSpelling returns the first (by byte order) name of names that equals name up to case, or
// "" when there is none. On a case-sensitive filesystem the *arr may still see such a name under
// its own spelling (an SMB share that ignores case).
func otherSpelling(names map[string]struct{}, name string) string {
	other := ""
	for n := range names {
		if strings.EqualFold(n, name) && (other == "" || n < other) {
			other = n
		}
	}
	return other
}

// lookupMayChange reports whether an error reading a path may be gone on the next scan, as the
// scan sorts errors (internal/catalog): the path is not there (yet), or the filesystem is failing
// (the rescan refuses then). Any other error (EACCES, EPERM) makes the scan keep the rows under
// that folder as they are, scan after scan.
func lookupMayChange(err error) bool {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return true
	}
	for _, e := range []syscall.Errno{syscall.EIO, syscall.ENOTCONN, syscall.ESTALE, syscall.ETIMEDOUT, syscall.EHOSTDOWN} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// errnoOf returns the system error inside err (without the path, which the message names), or err.
func errnoOf(err error) error {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno
	}
	return err
}

// currentSize returns the size of the regular file rel as an open file sees it: opening a file
// revalidates the attributes an NFS or SMB client caches (close-to-open consistency), which lstat
// may answer from its cache for up to a minute. It returns lstatSize when the file cannot be
// opened.
func currentSize(root *os.Root, rel string, lstatSize int64) int64 {
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return lstatSize
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return lstatSize
	}
	return fi.Size()
}

// reportExpected logs, once per path (reported), the expected files that are not waited for.
func (s *syncRun) reportExpected(src catalog.Source, checks []expectedCheck, reported map[string]bool) {
	for _, c := range checks {
		if c.state == expectedPending || reported[c.RelPath] {
			continue
		}
		reported[c.RelPath] = true
		app := cmp.Or(c.App, "The *arr")
		switch c.state {
		case expectedNever:
			s.rep.Log(slog.LevelInfo, fmt.Sprintf("%s reports %s, which the scan of %q never catalogs: %s; it is not backed up",
				app, c.RelPath, src.Name, c.reason), "source", src.Name)
		case expectedOtherSpelling:
			s.rep.Log(slog.LevelInfo, fmt.Sprintf("%s reports %s, but the scan of %q catalogs the names on disk: %s; it is not waited for",
				app, c.RelPath, src.Name, c.reason), "source", src.Name)
		case expectedTargetSpelling:
			s.warnings++
			s.expectedMissing++
			s.rep.Log(slog.LevelWarn, fmt.Sprintf("%s reports %s (%s), but %s and this sync scans only the *arr's spelling; it is not backed up yet (the next full sync backs it up)",
				app, c.RelPath, formatBytes(c.Size), c.reason), "source", src.Name)
		case expectedOtherSize:
			s.warnings++
			s.rep.Log(slog.LevelWarn, fmt.Sprintf("%s reports %s with %s, but the file in %q has %s; it is backed up as it is (the *arr may not have rescanned it since it changed)",
				app, c.RelPath, formatBytes(c.Size), src.Name, formatBytes(c.size)), "source", src.Name)
		}
	}
}

// pendingExpected returns the checks that are waited for.
func pendingExpected(checks []expectedCheck) []expectedCheck {
	var out []expectedCheck
	for _, c := range checks {
		if c.state == expectedPending {
			out = append(out, c)
		}
	}
	return out
}

// excluder is the scan's exclude matcher (internal/catalog/exclude.go) rebuilt from
// catalog.DefaultExcludes and the source's own patterns: the defaults match case-insensitively
// and the source's patterns as written. A trailing "/" matches directories only. A leading "/"
// anchors a pattern to the relative path; other patterns match the base name or the relative path.
type excluder []excludePattern

type excludePattern struct {
	glob              string
	dirOnly, anchored bool
	fold              bool
}

func newExcluder(own []string) excluder {
	var ex excluder
	for _, d := range catalog.DefaultExcludes() {
		ex = append(ex, parseExclude(d, true))
	}
	for _, o := range own {
		ex = append(ex, parseExclude(o, false))
	}
	return ex
}

func parseExclude(raw string, fold bool) excludePattern {
	p := excludePattern{glob: raw, fold: fold}
	if strings.HasSuffix(p.glob, "/") {
		p.dirOnly = true
		p.glob = strings.TrimRight(p.glob, "/")
	}
	if strings.HasPrefix(p.glob, "/") {
		p.anchored = true
		p.glob = strings.TrimLeft(p.glob, "/")
	}
	if fold {
		p.glob = strings.ToLower(p.glob)
	}
	return p
}

// excluded reports whether the entry at rel (relative to the source root) with base name base is
// excluded.
func (ex excluder) excluded(rel, base string, isDir bool) bool {
	for _, p := range ex {
		if p.dirOnly && !isDir {
			continue
		}
		r, b := rel, base
		if p.fold {
			r, b = strings.ToLower(rel), strings.ToLower(base)
		}
		if !p.anchored {
			if ok, _ := path.Match(p.glob, b); ok {
				return true
			}
		}
		if ok, _ := path.Match(p.glob, r); ok {
			return true
		}
	}
	return false
}

// onceWarned passes a rescan's log lines on, except the warnings already logged (a directory that
// cannot be read is reported by every scan of it).
type onceWarned struct {
	jobs.Reporter
	seen map[string]bool
}

func (o onceWarned) Log(level slog.Level, msg string, args ...any) {
	if level >= slog.LevelWarn && o.seen[msg] {
		return
	}
	o.Reporter.Log(level, msg, args...)
}
