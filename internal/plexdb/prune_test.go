package plexdb

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestSplitVersionPath(t *testing.T) {
	good := map[string][2]string{
		".bunkarr/plex/plex-1/20260924T120000Z":       {"plex-1", "20260924T120000Z"},
		".bunkarr/plex/plex-1/20260924T120000Z-job12": {"plex-1", "20260924T120000Z-job12"},
	}
	for rel, want := range good {
		folder, version, ok := splitVersionPath(rel)
		if !ok || folder != want[0] || version != want[1] {
			t.Errorf("splitVersionPath(%q) = %q %q %v", rel, folder, version, ok)
		}
	}
	for _, rel := range []string{
		"", "movies/20260924T120000Z", ".bunkarr/retention/20260924T120000Z",
		".bunkarr/plex/20260924T120000Z", ".bunkarr/plex/.hidden/20260924T120000Z",
		".bunkarr/plex/plex-1/../../movies", ".bunkarr/plex/plex-1/20260924T120000Z/sub",
		".bunkarr/plex/plex-1/latest", ".bunkarr/plex/plex-1/.partial-job3", "/.bunkarr/plex/plex-1/20260924T120000Z",
		".bunkarr/plex//20260924T120000Z",
	} {
		if _, _, ok := splitVersionPath(rel); ok {
			t.Errorf("splitVersionPath(%q) accepted", rel)
		}
	}
}

// mkVersion creates a version directory with one file under root.
func mkVersion(t *testing.T, target, rel string) {
	t.Helper()
	dir := filepath.Join(target, filepath.FromSlash(rel))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, LibraryDB), []byte("db"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(t *testing.T, p string) bool {
	t.Helper()
	_, err := os.Lstat(p)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

// removeVersion deletes a version as prune does, without the row: trashVersion, then removeTrash.
func removeVersion(root *os.Root, rel string) error {
	trash, err := trashVersion(root, rel)
	if err != nil {
		return err
	}
	return removeTrash(root, trash)
}

func TestRemoveVersion(t *testing.T) {
	target := t.TempDir()
	root, err := os.OpenRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	v := ".bunkarr/plex/plex-1/20260924T120000Z"
	mkVersion(t, target, v)
	if err := removeVersion(root, v); err != nil {
		t.Fatal(err)
	}
	if exists(t, filepath.Join(target, v)) || exists(t, filepath.Join(target, trashPath(v))) {
		t.Fatal("the version was not removed")
	}
	if !exists(t, filepath.Join(target, ".bunkarr/plex/plex-1")) {
		t.Fatal("the folder was removed")
	}
	if err := removeVersion(root, v); err != nil {
		t.Fatalf("removing a missing version: %v", err)
	}

	// A prune interrupted after the rename: the leftover is removed.
	v2 := ".bunkarr/plex/plex-1/20260923T120000Z"
	mkVersion(t, target, trashPath(v2))
	if err := removeVersion(root, v2); err != nil || exists(t, filepath.Join(target, trashPath(v2))) {
		t.Fatalf("leftover not removed: %v", err)
	}

	// Paths outside the Plex tree are refused, whatever is there.
	mkVersion(t, target, "movies/20260924T120000Z")
	for _, rel := range []string{"movies/20260924T120000Z", ".bunkarr/plex/plex-1/../../movies/20260924T120000Z", ".bunkarr/plex/plex-1"} {
		if err := removeVersion(root, rel); err == nil {
			t.Errorf("removeVersion(%q) accepted", rel)
		}
	}
	if !exists(t, filepath.Join(target, "movies/20260924T120000Z", LibraryDB)) {
		t.Fatal("a directory outside the Plex tree was deleted")
	}

	// A symlinked folder cannot redirect the deletion into the live tree.
	if err := os.Symlink("../../movies", filepath.Join(target, ".bunkarr/plex/evil-2")); err != nil {
		t.Fatal(err)
	}
	if err := removeVersion(root, ".bunkarr/plex/evil-2/20260924T120000Z"); err == nil {
		t.Fatal("a version under a symlinked folder was deleted")
	}
	if !exists(t, filepath.Join(target, "movies/20260924T120000Z", LibraryDB)) {
		t.Fatal("the deletion followed a symlink")
	}

	// A version kept after all is moved back from an interrupted prune.
	v3 := ".bunkarr/plex/plex-1/20260922T120000Z"
	m3 := mkCompleteVersion(t, target, trashPath(v3))
	if lost, err := versionLost(root, v3, IntegrityOK, m3); err != nil || lost {
		t.Fatalf("a complete trash counts as lost: %v %v", lost, err)
	}
	if ok, err := restoreVersion(root, v3, IntegrityOK, m3); err != nil || !ok || !exists(t, filepath.Join(target, v3, LibraryDB)) {
		t.Fatalf("restore: %v %v", ok, err)
	}
	if ok, err := restoreVersion(root, v3, IntegrityOK, m3); err != nil || ok {
		t.Fatalf("restore of a present version: %v %v", ok, err)
	}
	if lost, err := versionLost(root, v3, IntegrityOK, m3); err != nil || lost {
		t.Fatalf("a present version counts as lost: %v %v", lost, err)
	}
}

func TestRestoreVersionRefusesAnIncompleteVersion(t *testing.T) {
	// A ".prune-*" directory whose removal had begun is never moved back under the version's
	// name: the version is lost, as is one whose directory and trash are both gone.
	target := t.TempDir()
	root, err := os.OpenRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, tc := range []struct {
		name   string
		damage func(trash string) error
	}{
		{"a file removed", func(trash string) error { return os.Remove(filepath.Join(trash, LibraryDB)) }},
		{"the manifest removed", func(trash string) error { return os.Remove(filepath.Join(trash, ManifestName)) }},
		{"a file of another size", func(trash string) error { return os.WriteFile(filepath.Join(trash, LibraryDB), []byte("d"), 0o644) }},
		{"all removed", func(trash string) error { return os.RemoveAll(trash) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := ".bunkarr/plex/plex-1/20260921T120000Z"
			m := mkCompleteVersion(t, target, trashPath(v))
			defer os.RemoveAll(filepath.Join(target, filepath.FromSlash(trashPath(v))))
			if err := tc.damage(filepath.Join(target, filepath.FromSlash(trashPath(v)))); err != nil {
				t.Fatal(err)
			}
			if lost, err := versionLost(root, v, IntegrityOK, m); err != nil || !lost {
				t.Fatalf("versionLost: %v %v", lost, err)
			}
			if ok, err := restoreVersion(root, v, IntegrityOK, m); ok || (err == nil) != (tc.name == "all removed") {
				t.Fatalf("restoreVersion: %v %v", ok, err)
			}
			if exists(t, filepath.Join(target, v)) {
				t.Fatal("an incomplete version was restored")
			}
		})
	}
}

func TestAFailedVersionIsCompleteWithFilesOfAnotherSize(t *testing.T) {
	// A version recorded as failed may have been recorded with files that do not match its
	// manifest: a ".prune-*" copy that holds all of them is complete (not lost), and it is moved
	// back when the version is kept after all. A missing file still makes it incomplete.
	target := t.TempDir()
	root, err := os.OpenRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	v := ".bunkarr/plex/plex-1/20260921T120000Z"
	m := mkCompleteVersion(t, target, trashPath(v))
	trash := filepath.Join(target, filepath.FromSlash(trashPath(v)))
	if err := os.WriteFile(filepath.Join(trash, LibraryDB), []byte("damaged"), 0o644); err != nil {
		t.Fatal(err)
	}
	if lost, err := versionLost(root, v, IntegrityOK, m); err != nil || !lost {
		t.Fatalf("an ok version with a file of another size: lost %v, %v", lost, err)
	}
	if lost, err := versionLost(root, v, IntegrityFailed, m); err != nil || lost {
		t.Fatalf("a failed version with a file of another size: lost %v, %v", lost, err)
	}
	if ok, err := restoreVersion(root, v, IntegrityFailed, m); err != nil || !ok || !exists(t, filepath.Join(target, v, LibraryDB)) {
		t.Fatalf("restore of a failed version: %v %v", ok, err)
	}
	if err := os.Rename(filepath.Join(target, filepath.FromSlash(v)), trash); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(trash, LibraryDB)); err != nil {
		t.Fatal(err)
	}
	if lost, err := versionLost(root, v, IntegrityFailed, m); err != nil || !lost {
		t.Fatalf("a failed version missing a file: lost %v, %v", lost, err)
	}
	if ok, err := restoreVersion(root, v, IntegrityFailed, m); ok || err == nil {
		t.Fatalf("restore of a failed version missing a file: %v %v", ok, err)
	}
}

// mkCompleteVersion creates a version directory with a library file and its manifest.json and
// returns the manifest.
func mkCompleteVersion(t *testing.T, target, rel string) json.RawMessage {
	t.Helper()
	mkVersion(t, target, rel)
	m, err := json.Marshal(Manifest{Format: manifestFormat, Result: IntegrityOK, Files: []ManifestFile{{Name: LibraryDB, Size: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, filepath.FromSlash(rel), ManifestName), m, 0o644); err != nil {
		t.Fatal(err)
	}
	return m
}
