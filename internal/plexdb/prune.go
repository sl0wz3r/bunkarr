package plexdb

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/sl0wz3r/bunkarr/internal/snapshots"
)

// realDirs checks that rel and every directory above it inside root are real directories (not
// symlinks), so a path cannot be redirected to another part of the destination.
func realDirs(root *os.Root, rel string) error {
	return snapshots.RealDirs(root, rel)
}

// isPruned reports whether name is a ".prune-<version>" directory (a version a prune was
// deleting).
func isPruned(name string) bool {
	_, ok := snapshots.PrunedVersion(name)
	return ok
}

// prunedVersion returns the version name of a ".prune-<version>" directory.
func prunedVersion(name string) string {
	v, _ := snapshots.PrunedVersion(name)
	return v
}

// trashPath is where trashVersion moves a version before it is deleted.
func trashPath(rel string) string {
	return snapshots.TrashPath(rel)
}

// trashVersion starts the deletion of the version directory rel, which must be
// ".bunkarr/plex/<folder>/<version>" with real directories all the way (the only kind of directory
// pruning deletes): see snapshots.Layout.TrashVersion. The caller deletes the version's row and
// then the trash (removeTrash), so a crash never leaves a half-deleted version under its name or a
// row without its directory; a trash left without a row is removed by the next backup
// (run.recover).
func trashVersion(root *os.Root, rel string) (string, error) {
	return layout.TrashVersion(root, rel)
}

// removeTrash removes a ".prune-*" directory if it exists.
func removeTrash(root *os.Root, trash string) error {
	return snapshots.RemoveTrash(root, trash)
}

// restoreVersion moves a version back from its ".prune-*" name when a prune that selected it was
// interrupted and the version is kept after all (the retention was raised meanwhile). integrity
// and manifest are the version's recorded ones. It reports whether it did. A ".prune-*" directory
// that is not complete (its removal had begun) is never moved back: see versionLost.
func restoreVersion(root *os.Root, rel, integrity string, manifest json.RawMessage) (bool, error) {
	return layout.RestoreVersion(root, rel, completeCheck(root, manifest, integrity))
}

// errIncomplete is a version directory that lacks a file of its manifest.
var errIncomplete = snapshots.ErrIncomplete

// versionLost reports whether the recorded version rel (with its recorded integrity and manifest)
// is gone from the destination: its directory does not exist and neither does a complete
// ".prune-*" copy (a prune's row delete was lost after its files were deleted, or the row comes
// from an older copy of Bunkarr's database). When it cannot tell, it reports false.
func versionLost(root *os.Root, rel, integrity string, manifest json.RawMessage) (bool, error) {
	return layout.VersionLost(root, rel, completeCheck(root, manifest, integrity))
}

// completeCheck is checkComplete for a recorded version: sizes are compared only for a version
// whose integrity is ok.
func completeCheck(root *os.Root, manifest json.RawMessage, integrity string) func(dir string) error {
	return func(dir string) error { return checkComplete(root, dir, manifest, integrity == IntegrityOK) }
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
