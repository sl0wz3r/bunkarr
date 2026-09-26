package snapshots

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
)

// maxSlugLen bounds the name part of a version folder.
const maxSlugLen = 40

var (
	// versionNameRe matches a version directory's name.
	versionNameRe = regexp.MustCompile(`^\d{8}T\d{6}Z(?:-job\d+)?$`)
	// partialNameRe matches a directory being written by a job.
	partialNameRe = regexp.MustCompile(`^\.partial-job(\d+)$`)
	// pruneNameRe matches a version directory being deleted.
	pruneNameRe = regexp.MustCompile(`^\.prune-(\d{8}T\d{6}Z(?:-job\d+)?)$`)
)

// ErrIncomplete is a version directory that lacks a file its manifest names (or holds one that
// is not the recorded file). The complete callbacks of Layout.RestoreVersion and
// Layout.VersionLost wrap it.
var ErrIncomplete = errors.New("the version is incomplete")

// Layout is where one kind of version lives inside a destination: Root/<folder>/<version>, with
// <folder> = "<slug of the integration's name>-<integration id>" and <version> =
// "<yyyymmddThhmmssZ>[-job<id>]". Folder names, the -job suffix, recovery and pruning follow
// phase1.md §5 for every kind.
type Layout struct {
	// Root is the directory of every folder of this kind, relative to the destination target,
	// slash-separated and clean, e.g. ".bunkarr/plex" or ".bunkarr/arr".
	Root string
	// DefaultSlug is the folder's name part when the integration's name has none, e.g. "plex".
	DefaultSlug string
}

// Folder returns the directory, relative to the destination target, that holds the versions of
// one integration: "<Root>/<slug of name>-<id>". The id keeps two integrations with similar names
// apart and lets a job recognise its integration's folder after a rename.
func (l Layout) Folder(name string, id int64) string {
	return l.Root + "/" + l.FolderBase(name, id)
}

// FolderBase is Folder's last component.
func (l Layout) FolderBase(name string, id int64) string {
	s := Slug(name)
	if s == "" {
		s = l.DefaultSlug
	}
	return s + "-" + strconv.FormatInt(id, 10)
}

// Slug lowercases name and keeps [a-z0-9_]; every other run of characters becomes one "-". The
// result has no leading or trailing "-" and at most 40 bytes.
func Slug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > maxSlugLen {
		s = strings.TrimRight(s[:maxSlugLen], "-")
	}
	return s
}

// FolderIntegrationID returns the integration id at the end of a folder base name ("plex-3" →
// 3), or 0 for a name that is not a folder of an integration.
func FolderIntegrationID(base string) int64 {
	i := strings.LastIndexByte(base, '-')
	if i < 0 || strings.HasPrefix(base, ".") {
		return 0
	}
	id, err := strconv.ParseInt(base[i+1:], 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

// VersionName returns the name of a version directory created at t: t in UTC formatted with
// VersionLayout (RenameVersion adds "-job<id>" when that name is taken).
func VersionName(t time.Time) string {
	return t.UTC().Format(VersionLayout)
}

// IsVersionName reports whether name is a version directory's name.
func IsVersionName(name string) bool {
	return versionNameRe.MatchString(name)
}

// PartialName is the directory a job writes its version into, inside the integration's folder.
func PartialName(jobID int64) string {
	return ".partial-job" + strconv.FormatInt(jobID, 10)
}

// IsPartialName reports whether name is a directory a job was writing a version into.
func IsPartialName(name string) bool {
	return partialNameRe.MatchString(name)
}

// PrunedVersion returns the version name of a ".prune-<version>" directory (a version a prune was
// deleting) and whether name is one.
func PrunedVersion(name string) (string, bool) {
	m := pruneNameRe.FindStringSubmatch(name)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// SplitVersionPath checks that rel is "<Root>/<folder>/<version>" (a folder that does not start
// with "." and a version name made by a runner) and returns folder and version. It is the fence
// of every deletion made through l.
func (l Layout) SplitVersionPath(rel string) (folder, version string, ok bool) {
	if l.Root == "" {
		return "", "", false
	}
	rest, found := strings.CutPrefix(rel, l.Root+"/")
	if !found {
		return "", "", false
	}
	folder, version, found = strings.Cut(rest, "/")
	if !found || folder == "" || strings.HasPrefix(folder, ".") || strings.Contains(version, "/") ||
		!versionNameRe.MatchString(version) {
		return "", "", false
	}
	return folder, version, true
}

// RealDirs checks that rel and every directory above it inside root are real directories (not
// symlinks), so a path cannot be redirected to another part of the destination.
func RealDirs(root *os.Root, rel string) error {
	parts := strings.Split(rel, "/")
	for i := range parts {
		p := strings.Join(parts[:i+1], "/")
		fi, err := root.Lstat(p)
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return fmt.Errorf("%s is not a directory", p)
		}
	}
	return nil
}

// TrashPath is where TrashVersion moves the version directory rel before it is deleted:
// ".prune-<version>" next to it.
func TrashPath(rel string) string {
	return path.Dir(rel) + "/.prune-" + path.Base(rel)
}

// TrashVersion starts the deletion of the version directory rel, which must be
// "<Root>/<folder>/<version>" with real directories all the way (the only kind of directory a
// prune deletes): it renames it to ".prune-<version>" (a stale ".prune-<version>" is removed
// first) and returns that path. A missing rel is not an error (an interrupted prune renamed it
// already). The caller deletes the version's row and then the trash (RemoveTrash), so a crash
// never leaves a half-deleted version under its name or a row without its directory; a trash
// left without a row is removed by the next run's recovery.
func (l Layout) TrashVersion(root *os.Root, rel string) (string, error) {
	if _, _, ok := l.SplitVersionPath(rel); !ok {
		return "", fmt.Errorf("refusing to delete %q: not a version directory under %s", rel, l.Root)
	}
	if err := RealDirs(root, path.Dir(rel)); err != nil {
		return "", fmt.Errorf("delete %s: %w", rel, err)
	}
	trash := TrashPath(rel)
	fi, err := root.Lstat(rel)
	switch {
	case err == nil && fi.IsDir():
		if err := RemoveTrash(root, trash); err != nil {
			return "", err
		}
		if err := filecopy.RenameDir(root, rel, trash); err != nil {
			return "", fmt.Errorf("delete %s: %w", rel, err)
		}
	case err == nil:
		return "", fmt.Errorf("delete %s: not a directory", rel)
	case !errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("delete %s: %w", rel, err)
	}
	return trash, nil
}

// RemoveTrash removes a ".prune-*" directory (or any directory the caller fenced) if it exists.
// A non-directory is refused.
func RemoveTrash(root *os.Root, trash string) error {
	fi, err := root.Lstat(trash)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete %s: %w", trash, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("delete %s: not a directory", trash)
	}
	if err := root.RemoveAll(trash); err != nil {
		return fmt.Errorf("delete %s: %w", trash, err)
	}
	return nil
}

// RestoreVersion moves a version back from its ".prune-*" name when a prune that selected it was
// interrupted and the version is kept after all (the retention was raised meanwhile). complete
// checks that a directory holds the whole recorded version (wrapping ErrIncomplete when it does
// not). It reports whether it moved the version back. A ".prune-*" directory that is not complete
// (its removal had begun) is never moved back: see VersionLost.
func (l Layout) RestoreVersion(root *os.Root, rel string, complete func(dir string) error) (bool, error) {
	if _, _, ok := l.SplitVersionPath(rel); !ok {
		return false, nil
	}
	if _, err := root.Lstat(rel); !errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	trash := TrashPath(rel)
	fi, err := root.Lstat(trash)
	if err != nil || !fi.IsDir() {
		return false, nil
	}
	if err := RealDirs(root, path.Dir(rel)); err != nil {
		return false, fmt.Errorf("restore %s: %w", rel, err)
	}
	if err := complete(trash); err != nil {
		return false, fmt.Errorf("restore %s: %w", rel, err)
	}
	if err := filecopy.RenameDir(root, trash, rel); err != nil {
		return false, fmt.Errorf("restore %s: %w", rel, err)
	}
	return true, nil
}

// VersionLost reports whether the recorded version rel is gone from the destination: its
// directory does not exist and neither does a complete ".prune-*" copy (complete returned an
// error wrapping ErrIncomplete). That happens when a prune's row delete was lost after its files
// were deleted, or when the row comes from an older copy of Bunkarr's database. When it cannot
// tell, it reports false.
func (l Layout) VersionLost(root *os.Root, rel string, complete func(dir string) error) (bool, error) {
	if _, _, ok := l.SplitVersionPath(rel); !ok {
		return false, nil
	}
	if _, err := root.Lstat(rel); !errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err := RealDirs(root, path.Dir(rel)); err != nil {
		return false, err
	}
	fi, err := root.Lstat(TrashPath(rel))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return true, nil
	case err != nil:
		return false, err
	case !fi.IsDir():
		return false, nil
	}
	err = complete(TrashPath(rel))
	if errors.Is(err, ErrIncomplete) {
		return true, nil
	}
	return false, err
}

// RenameVersion renames a job's partial directory to the version's name, folder + "/" +
// versionName, or folder + "/" + versionName + "-job<id>" when that name is taken (two versions
// in one second), and returns the version's path.
func RenameVersion(root *os.Root, partial, folder, versionName string, jobID int64) (string, error) {
	rel := folder + "/" + versionName
	err := filecopy.RenameDir(root, partial, rel)
	if errors.Is(err, filecopy.ErrExists) {
		rel = folder + "/" + versionName + "-job" + strconv.FormatInt(jobID, 10)
		err = filecopy.RenameDir(root, partial, rel)
	}
	if err != nil {
		return "", fmt.Errorf("rename the version directory: %w", err)
	}
	return rel, nil
}
