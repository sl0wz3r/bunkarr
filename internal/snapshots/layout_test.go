package snapshots

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var (
	plexLayout = Layout{Root: ".bunkarr/plex", DefaultSlug: "plex"}
	arrLayout  = Layout{Root: ".bunkarr/arr", DefaultSlug: "arr"}
)

func TestFolderNames(t *testing.T) {
	for _, tt := range []struct {
		layout Layout
		name   string
		id     int64
		want   string
	}{
		{plexLayout, "Plex Server", 1, ".bunkarr/plex/plex-server-1"},
		{plexLayout, "  Wohnzimmer (4K)!! ", 12, ".bunkarr/plex/wohnzimmer-4k-12"},
		{plexLayout, "千と千尋", 3, ".bunkarr/plex/plex-3"},
		{plexLayout, "../../etc", 4, ".bunkarr/plex/etc-4"},
		{plexLayout, strings.Repeat("a", 60), 5, ".bunkarr/plex/" + strings.Repeat("a", 40) + "-5"},
		{arrLayout, "Radarr 4K", 7, ".bunkarr/arr/radarr-4k-7"},
		{arrLayout, "", 8, ".bunkarr/arr/arr-8"},
	} {
		if got := tt.layout.Folder(tt.name, tt.id); got != tt.want {
			t.Errorf("Folder(%q, %d) = %q, want %q", tt.name, tt.id, got, tt.want)
		}
		if got := FolderIntegrationID(strings.TrimPrefix(tt.want, tt.layout.Root+"/")); got != tt.id {
			t.Errorf("FolderIntegrationID(%q) = %d", tt.want, got)
		}
	}
	for _, base := range []string{"plex", ".partial-job3", "plex-x", "plex-0", "", "-"} {
		if FolderIntegrationID(base) != 0 {
			t.Errorf("FolderIntegrationID(%q) != 0", base)
		}
	}
	if s := Slug("a" + strings.Repeat("-", 38) + "bcd"); s != "a-bcd" {
		t.Errorf("Slug collapses runs: %q", s)
	}
	if s := Slug(strings.Repeat("a", 39) + " b"); s != strings.Repeat("a", 39) {
		t.Errorf("Slug trims a cut dash: %q", s)
	}
}

func TestVersionNames(t *testing.T) {
	at := time.Date(2026, 9, 24, 12, 0, 5, 0, time.FixedZone("UTC+2", 2*3600))
	if got := VersionName(at); got != "20260924T100005Z" {
		t.Fatalf("VersionName = %q", got)
	}
	for name, want := range map[string]bool{
		"20260924T120000Z": true, "20260924T120000Z-job12": true, "20260924T120000Z-job": false, "latest": false,
		".partial-job3": false, "20260924T120000": false, "20260924T120000Z/x": false,
	} {
		if IsVersionName(name) != want {
			t.Errorf("IsVersionName(%q) = %v", name, !want)
		}
	}
	if PartialName(12) != ".partial-job12" || !IsPartialName(".partial-job12") || IsPartialName(".partial-jobx") ||
		IsPartialName("partial-job1") {
		t.Error("partial names")
	}
	if v, ok := PrunedVersion(".prune-20260924T120000Z-job3"); !ok || v != "20260924T120000Z-job3" {
		t.Errorf("PrunedVersion = %q, %v", v, ok)
	}
	for _, name := range []string{".prune-latest", "prune-20260924T120000Z", ".prune-", ".partial-job3"} {
		if _, ok := PrunedVersion(name); ok {
			t.Errorf("PrunedVersion(%q) accepted", name)
		}
	}
}

func TestSplitVersionPath(t *testing.T) {
	good := map[string][2]string{
		".bunkarr/plex/plex-1/20260924T120000Z":       {"plex-1", "20260924T120000Z"},
		".bunkarr/plex/plex-1/20260924T120000Z-job12": {"plex-1", "20260924T120000Z-job12"},
	}
	for rel, want := range good {
		folder, version, ok := plexLayout.SplitVersionPath(rel)
		if !ok || folder != want[0] || version != want[1] {
			t.Errorf("SplitVersionPath(%q) = %q %q %v", rel, folder, version, ok)
		}
	}
	for _, rel := range []string{
		"", "movies/20260924T120000Z", ".bunkarr/retention/20260924T120000Z",
		".bunkarr/plex/20260924T120000Z", ".bunkarr/plex/.hidden/20260924T120000Z",
		".bunkarr/plex/plex-1/../../movies", ".bunkarr/plex/plex-1/20260924T120000Z/sub",
		".bunkarr/plex/plex-1/latest", ".bunkarr/plex/plex-1/.partial-job3", "/.bunkarr/plex/plex-1/20260924T120000Z",
		".bunkarr/plex//20260924T120000Z",
		// another kind's versions are outside this layout
		".bunkarr/arr/radarr-4/20260924T120000Z",
		".bunkarr/plexdb/plex-1/20260924T120000Z",
	} {
		if _, _, ok := plexLayout.SplitVersionPath(rel); ok {
			t.Errorf("SplitVersionPath(%q) accepted", rel)
		}
	}
	if _, _, ok := (Layout{}).SplitVersionPath("/x/20260924T120000Z"); ok {
		t.Error("a layout without a root accepted a path")
	}
}

// mkVersion creates a version directory with one file under target.
func mkVersion(t *testing.T, target, rel string) {
	t.Helper()
	dir := filepath.Join(target, filepath.FromSlash(rel))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data.zip"), []byte("zip"), 0o600); err != nil {
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

func openRoot(t *testing.T) (string, *os.Root) {
	t.Helper()
	target := t.TempDir()
	root, err := os.OpenRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return target, root
}

// removeVersion deletes a version as a prune does, without the row.
func removeVersion(l Layout, root *os.Root, rel string) error {
	trash, err := l.TrashVersion(root, rel)
	if err != nil {
		return err
	}
	return RemoveTrash(root, trash)
}

// completeIf is a complete callback: the directory must hold data.zip of 3 bytes.
func completeIf(root *os.Root) func(dir string) error {
	return func(dir string) error {
		fi, err := root.Lstat(dir + "/data.zip")
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("%w: no data.zip", ErrIncomplete)
		case err != nil:
			return err
		case !fi.Mode().IsRegular() || fi.Size() != 3:
			return fmt.Errorf("%w: data.zip is not the recorded file", ErrIncomplete)
		}
		return nil
	}
}

func TestTrashAndRemoveVersion(t *testing.T) {
	target, root := openRoot(t)
	v := ".bunkarr/arr/radarr-4/20260924T120000Z"
	mkVersion(t, target, v)
	if err := removeVersion(arrLayout, root, v); err != nil {
		t.Fatal(err)
	}
	if exists(t, filepath.Join(target, v)) || exists(t, filepath.Join(target, TrashPath(v))) {
		t.Fatal("the version was not removed")
	}
	if !exists(t, filepath.Join(target, ".bunkarr/arr/radarr-4")) {
		t.Fatal("the folder was removed")
	}
	if err := removeVersion(arrLayout, root, v); err != nil {
		t.Fatalf("removing a missing version: %v", err)
	}

	// A prune interrupted after the rename: the leftover is removed first.
	v2 := ".bunkarr/arr/radarr-4/20260923T120000Z"
	mkVersion(t, target, TrashPath(v2))
	mkVersion(t, target, v2)
	if err := removeVersion(arrLayout, root, v2); err != nil || exists(t, filepath.Join(target, TrashPath(v2))) ||
		exists(t, filepath.Join(target, v2)) {
		t.Fatalf("leftover not removed: %v", err)
	}

	// Paths outside the layout are refused, whatever is there: another kind's versions too.
	mkVersion(t, target, "movies/20260924T120000Z")
	mkVersion(t, target, ".bunkarr/plex/plex-1/20260924T120000Z")
	for _, rel := range []string{"movies/20260924T120000Z", ".bunkarr/arr/radarr-4/../../movies/20260924T120000Z",
		".bunkarr/arr/radarr-4", ".bunkarr/plex/plex-1/20260924T120000Z"} {
		if err := removeVersion(arrLayout, root, rel); err == nil {
			t.Errorf("removeVersion(%q) accepted", rel)
		}
	}
	for _, p := range []string{"movies/20260924T120000Z", ".bunkarr/plex/plex-1/20260924T120000Z"} {
		if !exists(t, filepath.Join(target, p, "data.zip")) {
			t.Fatalf("%s was deleted", p)
		}
	}

	// A symlinked folder cannot redirect the deletion into the live tree.
	if err := os.Symlink("../../movies", filepath.Join(target, ".bunkarr/arr/evil-2")); err != nil {
		t.Fatal(err)
	}
	if err := removeVersion(arrLayout, root, ".bunkarr/arr/evil-2/20260924T120000Z"); err == nil {
		t.Fatal("a version under a symlinked folder was deleted")
	}
	if !exists(t, filepath.Join(target, "movies/20260924T120000Z", "data.zip")) {
		t.Fatal("the deletion followed a symlink")
	}

	// A version that is a file is refused.
	if err := os.WriteFile(filepath.Join(target, ".bunkarr/arr/radarr-4/20260922T120000Z"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := arrLayout.TrashVersion(root, ".bunkarr/arr/radarr-4/20260922T120000Z"); err == nil {
		t.Fatal("a file was trashed as a version")
	}
	if err := RemoveTrash(root, ".bunkarr/arr/radarr-4/20260922T120000Z"); err == nil {
		t.Fatal("RemoveTrash removed a file")
	}
}

func TestRestoreAndLostVersion(t *testing.T) {
	target, root := openRoot(t)
	complete := completeIf(root)

	// A version kept after all is moved back from an interrupted prune.
	v := ".bunkarr/arr/radarr-4/20260922T120000Z"
	mkVersion(t, target, TrashPath(v))
	if lost, err := arrLayout.VersionLost(root, v, complete); err != nil || lost {
		t.Fatalf("a complete trash counts as lost: %v %v", lost, err)
	}
	if ok, err := arrLayout.RestoreVersion(root, v, complete); err != nil || !ok || !exists(t, filepath.Join(target, v, "data.zip")) {
		t.Fatalf("restore: %v %v", ok, err)
	}
	if ok, err := arrLayout.RestoreVersion(root, v, complete); err != nil || ok {
		t.Fatalf("restore of a present version: %v %v", ok, err)
	}
	if lost, err := arrLayout.VersionLost(root, v, complete); err != nil || lost {
		t.Fatalf("a present version counts as lost: %v %v", lost, err)
	}

	// An incomplete trash (its removal had begun) is never moved back: the version is lost, as
	// is one whose directory and trash are both gone.
	for _, tc := range []struct {
		name   string
		damage func(trash string) error
	}{
		{"a file removed", func(trash string) error { return os.Remove(filepath.Join(trash, "data.zip")) }},
		{"a file of another size", func(trash string) error { return os.WriteFile(filepath.Join(trash, "data.zip"), []byte("z"), 0o600) }},
		{"all removed", func(trash string) error { return os.RemoveAll(trash) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := ".bunkarr/arr/radarr-4/20260921T120000Z"
			mkVersion(t, target, TrashPath(v))
			trash := filepath.Join(target, filepath.FromSlash(TrashPath(v)))
			defer os.RemoveAll(trash)
			if err := tc.damage(trash); err != nil {
				t.Fatal(err)
			}
			if lost, err := arrLayout.VersionLost(root, v, complete); err != nil || !lost {
				t.Fatalf("VersionLost: %v %v", lost, err)
			}
			if ok, err := arrLayout.RestoreVersion(root, v, complete); ok || (err == nil) != (tc.name == "all removed") {
				t.Fatalf("RestoreVersion: %v %v", ok, err)
			}
			if exists(t, filepath.Join(target, v)) {
				t.Fatal("an incomplete version was restored")
			}
		})
	}

	// Outside the layout nothing is lost or restored.
	outside := ".bunkarr/plex/plex-1/20260920T120000Z"
	mkVersion(t, target, TrashPath(outside))
	if lost, err := arrLayout.VersionLost(root, outside, complete); err != nil || lost {
		t.Fatalf("VersionLost outside the layout: %v %v", lost, err)
	}
	if ok, err := arrLayout.RestoreVersion(root, outside, complete); err != nil || ok {
		t.Fatalf("RestoreVersion outside the layout: %v %v", ok, err)
	}
	// A complete check that cannot tell is not "lost".
	v3 := ".bunkarr/arr/radarr-4/20260919T120000Z"
	mkVersion(t, target, TrashPath(v3))
	boom := errors.New("cannot read")
	if lost, err := arrLayout.VersionLost(root, v3, func(string) error { return boom }); lost || !errors.Is(err, boom) {
		t.Fatalf("VersionLost with an unreadable trash: %v %v", lost, err)
	}
}

func TestRenameVersion(t *testing.T) {
	target, root := openRoot(t)
	folder := ".bunkarr/arr/radarr-4"
	mkVersion(t, target, folder+"/"+PartialName(5))
	rel, err := RenameVersion(root, folder+"/"+PartialName(5), folder, "20260924T120000Z", 5)
	if err != nil || rel != folder+"/20260924T120000Z" || !exists(t, filepath.Join(target, rel, "data.zip")) {
		t.Fatalf("RenameVersion = %q, %v", rel, err)
	}
	// The name is taken (two versions in one second): the job id is appended.
	mkVersion(t, target, folder+"/"+PartialName(6))
	rel, err = RenameVersion(root, folder+"/"+PartialName(6), folder, "20260924T120000Z", 6)
	if err != nil || rel != folder+"/20260924T120000Z-job6" {
		t.Fatalf("RenameVersion of a taken name = %q, %v", rel, err)
	}
	if _, err := RenameVersion(root, folder+"/"+PartialName(7), folder, "20260924T130000Z", 7); err == nil {
		t.Fatal("renaming a missing partial directory succeeded")
	}
}

func TestRealDirs(t *testing.T) {
	target, root := openRoot(t)
	if err := os.MkdirAll(filepath.Join(target, "a/b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := RealDirs(root, "a/b"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "a/f"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("b", filepath.Join(target, "a/l")); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"a/f", "a/l", "a/missing"} {
		if err := RealDirs(root, rel); err == nil {
			t.Errorf("RealDirs(%q) accepted", rel)
		}
	}
}
