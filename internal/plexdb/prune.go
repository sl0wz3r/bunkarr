package plexdb

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
)

// pruneCandidate is a version as the retention rules see it.
type pruneCandidate struct {
	ID        int64
	CreatedAt time.Time
	OK        bool
}

// selectPrune applies the Plex DB version retention (design §5) to the versions of one
// integration at one destination and returns the ids of the versions to delete, oldest first:
//
//   - the newest daily versions with integrity ok are kept;
//   - so is the newest ok version of each of the weekly most recent ISO weeks (in loc) that have
//     an ok version;
//   - the newest ok version is never deleted;
//   - a version whose integrity failed is kept for FailedKeep after it was made, then deleted.
//
// daily and weekly below 1 count as 1 and 0.
func selectPrune(vs []pruneCandidate, daily, weekly int, now time.Time, loc *time.Location) []int64 {
	daily = max(daily, 1)
	weekly = max(weekly, 0)
	if loc == nil {
		loc = time.Local
	}
	sorted := slices.Clone(vs)
	slices.SortFunc(sorted, func(a, b pruneCandidate) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return cmp.Compare(b.ID, a.ID)
	})
	keep := map[int64]bool{}
	type week struct{ year, week int }
	weeks := map[week]bool{}
	nOK := 0
	for _, v := range sorted {
		if !v.OK {
			if now.Sub(v.CreatedAt) < FailedKeep {
				keep[v.ID] = true
			}
			continue
		}
		nOK++
		if nOK <= daily { // includes the newest ok version
			keep[v.ID] = true
		}
		y, w := v.CreatedAt.In(loc).ISOWeek()
		if k := (week{y, w}); !weeks[k] && len(weeks) < weekly {
			weeks[k] = true
			keep[v.ID] = true
		}
	}
	var remove []int64
	for i := len(sorted) - 1; i >= 0; i-- {
		if !keep[sorted[i].ID] {
			remove = append(remove, sorted[i].ID)
		}
	}
	return remove
}

// realDirs checks that rel and every directory above it inside root are real directories (not
// symlinks), so a path cannot be redirected to another part of the destination.
func realDirs(root *os.Root, rel string) error {
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

// trashPath is where trashVersion moves a version before it is deleted.
func trashPath(rel string) string {
	return path.Dir(rel) + "/.prune-" + path.Base(rel)
}

// trashVersion starts the deletion of the version directory rel, which must be
// ".bunkarr/plex/<folder>/<version>" with real directories all the way (the only kind of directory
// pruning deletes): it renames it to ".prune-<version>" (a stale ".prune-<version>" is removed
// first) and returns that path. A missing rel is not an error (an interrupted prune renamed it
// already). The caller deletes the version's row and then the trash (removeTrash), so a crash
// never leaves a half-deleted version under its name or a row without its directory; a trash left
// without a row is removed by the next backup (run.recover).
func trashVersion(root *os.Root, rel string) (string, error) {
	if _, _, ok := splitVersionPath(rel); !ok {
		return "", fmt.Errorf("refusing to delete %q: not a Plex snapshot directory", rel)
	}
	if err := realDirs(root, path.Dir(rel)); err != nil {
		return "", fmt.Errorf("delete %s: %w", rel, err)
	}
	trash := trashPath(rel)
	fi, err := root.Lstat(rel)
	switch {
	case err == nil && fi.IsDir():
		if err := removeTrash(root, trash); err != nil {
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

// removeTrash removes a ".prune-*" directory if it exists.
func removeTrash(root *os.Root, trash string) error {
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

// restoreVersion moves a version back from its ".prune-*" name when a prune that selected it was
// interrupted and the version is kept after all (the retention was raised meanwhile). integrity
// and manifest are the version's recorded ones. It reports whether it did. A ".prune-*" directory
// that is not complete (its removal had begun) is never moved back: see versionLost.
func restoreVersion(root *os.Root, rel, integrity string, manifest json.RawMessage) (bool, error) {
	if _, _, ok := splitVersionPath(rel); !ok {
		return false, nil
	}
	if _, err := root.Lstat(rel); !errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	trash := trashPath(rel)
	fi, err := root.Lstat(trash)
	if err != nil || !fi.IsDir() {
		return false, nil
	}
	if err := realDirs(root, path.Dir(rel)); err != nil {
		return false, fmt.Errorf("restore %s: %w", rel, err)
	}
	if err := checkComplete(root, trash, manifest, integrity == IntegrityOK); err != nil {
		return false, fmt.Errorf("restore %s: %w", rel, err)
	}
	if err := filecopy.RenameDir(root, trash, rel); err != nil {
		return false, fmt.Errorf("restore %s: %w", rel, err)
	}
	return true, nil
}

// errIncomplete is a version directory that lacks a file of its manifest.
var errIncomplete = errors.New("the version is incomplete")

// versionLost reports whether the recorded version rel (with its recorded integrity and manifest)
// is gone from the destination: its directory does not exist and neither does a complete
// ".prune-*" copy (a prune's row delete was lost after its files were deleted, or the row comes
// from an older copy of Bunkarr's database). When it cannot tell, it reports false.
func versionLost(root *os.Root, rel, integrity string, manifest json.RawMessage) (bool, error) {
	if _, _, ok := splitVersionPath(rel); !ok {
		return false, nil
	}
	if _, err := root.Lstat(rel); !errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err := realDirs(root, path.Dir(rel)); err != nil {
		return false, err
	}
	fi, err := root.Lstat(trashPath(rel))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return true, nil
	case err != nil:
		return false, err
	case !fi.IsDir():
		return false, nil
	}
	err = checkComplete(root, trashPath(rel), manifest, integrity == IntegrityOK)
	if errors.Is(err, errIncomplete) {
		return true, nil
	}
	return false, err
}

// checkComplete checks that dir holds manifest.json and every file of the manifest, with its
// recorded size when sizes is set (errIncomplete otherwise). Only a version whose integrity is ok
// is known to have matched its manifest when it was recorded: one recorded as failed may have been
// recorded with files that did not (adopt), so a size that differs does not mean it changed since.
func checkComplete(root *os.Root, dir string, manifest json.RawMessage, sizes bool) error {
	var m Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		return fmt.Errorf("the recorded manifest is not valid: %w", err)
	}
	if fi, err := root.Lstat(dir + "/" + ManifestName); errors.Is(err, fs.ErrNotExist) || (err == nil && !fi.Mode().IsRegular()) {
		return fmt.Errorf("%w: no %s", errIncomplete, ManifestName)
	} else if err != nil {
		return err
	}
	for _, f := range m.Files {
		if !validFileName(f.Name) {
			return fmt.Errorf("the recorded manifest names an unexpected file %q", f.Name)
		}
		fi, err := root.Lstat(dir + "/" + f.Name)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("%w: no %s", errIncomplete, f.Name)
		case err != nil:
			return err
		case !fi.Mode().IsRegular() || (sizes && fi.Size() != f.Size):
			return fmt.Errorf("%w: %s is not the recorded file", errIncomplete, f.Name)
		}
	}
	return nil
}
