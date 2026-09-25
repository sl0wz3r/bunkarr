package plexdb

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// pruneNow is Thursday 2026-09-24 12:00 UTC, in ISO week 39 (Monday 09-21 to Sunday 09-27).
var pruneNow = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

// dayVersions returns one version per day for days [0, n) before pruneNow; the version of day d
// has id d, integrity ok unless d is in failed.
func dayVersions(n int, failed ...int) []pruneCandidate {
	out := make([]pruneCandidate, 0, n)
	for d := range n {
		out = append(out, pruneCandidate{ID: int64(d), CreatedAt: pruneNow.AddDate(0, 0, -d), OK: !slices.Contains(failed, d)})
	}
	return out
}

// idsExcept returns the ids [0, n) that are not in keep, oldest (highest day) first.
func idsExcept(n int, keep ...int64) []int64 {
	var out []int64
	for d := int64(n - 1); d >= 0; d-- {
		if !slices.Contains(keep, d) {
			out = append(out, d)
		}
	}
	return out
}

func span(from, to int64) []int64 {
	var out []int64
	for i := from; i <= to; i++ {
		out = append(out, i)
	}
	return out
}

func TestSelectPrune(t *testing.T) {
	berlinSummer := time.FixedZone("UTC+2", 2*3600)
	tests := []struct {
		name          string
		versions      []pruneCandidate
		daily, weekly int
		loc           *time.Location
		want          []int64
	}{
		{name: "fewer than the daily count", versions: dayVersions(5), daily: 14, weekly: 8, want: nil},
		{name: "daily only", versions: dayVersions(20), daily: 14, weekly: 0, want: idsExcept(20, span(0, 13)...)},
		{
			// Days 0-13 are the daily versions; the 8 most recent weeks (39 … 32) keep their
			// newest version: days 0 (w39), 4 (Sunday of w38), 11, 18, 25, 32, 39, 46.
			name: "defaults: 14 daily and 8 weekly", versions: dayVersions(60), daily: 14, weekly: 8,
			want: idsExcept(60, append(span(0, 13), 18, 25, 32, 39, 46)...),
		},
		{
			name: "the newest ok version survives a newer failed one", versions: dayVersions(3, 0), daily: 1, weekly: 0,
			want: []int64{2},
		},
		{
			name:     "only old ok versions: the newest ok one is kept",
			versions: append(dayVersions(10, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9), pruneCandidate{ID: 30, CreatedAt: pruneNow.AddDate(0, 0, -30), OK: true}),
			daily:    1, weekly: 0,
			want: []int64{9, 8, 7},
		},
		{
			name:     "failed versions are kept for 7 days",
			versions: dayVersions(10, 5, 6, 7, 8),
			daily:    14, weekly: 0,
			want: []int64{8, 7},
		},
		{
			name: "several versions a day: the daily count is a number of versions",
			versions: []pruneCandidate{
				{ID: 1, CreatedAt: pruneNow, OK: true},
				{ID: 2, CreatedAt: pruneNow.Add(-time.Hour), OK: true},
				{ID: 3, CreatedAt: pruneNow.Add(-2 * time.Hour), OK: true},
				{ID: 4, CreatedAt: pruneNow.AddDate(0, 0, -1), OK: true},
			},
			daily: 2, weekly: 1,
			want: []int64{4, 3},
		},
		{
			name: "weeks without versions are not counted",
			versions: []pruneCandidate{
				{ID: 1, CreatedAt: pruneNow, OK: true},                   // w39
				{ID: 2, CreatedAt: pruneNow.AddDate(0, 0, -1), OK: true}, // w39
				{ID: 3, CreatedAt: pruneNow.AddDate(0, 0, -63), OK: true},
				{ID: 4, CreatedAt: pruneNow.AddDate(0, 0, -64), OK: true},
				{ID: 5, CreatedAt: pruneNow.AddDate(0, 0, -140), OK: true},
			},
			daily: 1, weekly: 2,
			want: []int64{5, 4, 2},
		},
		{
			name: "same instant, id breaks the tie",
			versions: []pruneCandidate{
				{ID: 7, CreatedAt: pruneNow, OK: true},
				{ID: 8, CreatedAt: pruneNow, OK: true},
			},
			daily: 1, weekly: 0,
			want: []int64{7},
		},
		{
			// 2026-09-20 23:30 UTC is Sunday (week 38) in UTC but Monday (week 39) at UTC+2.
			name: "ISO weeks in UTC",
			versions: []pruneCandidate{
				{ID: 1, CreatedAt: pruneNow, OK: true},
				{ID: 2, CreatedAt: time.Date(2026, 9, 21, 0, 30, 0, 0, time.UTC), OK: true},
				{ID: 3, CreatedAt: time.Date(2026, 9, 20, 23, 30, 0, 0, time.UTC), OK: true},
			},
			daily: 1, weekly: 2, loc: time.UTC,
			want: []int64{2},
		},
		{
			name: "ISO weeks in the local zone",
			versions: []pruneCandidate{
				{ID: 1, CreatedAt: pruneNow, OK: true},
				{ID: 2, CreatedAt: time.Date(2026, 9, 21, 0, 30, 0, 0, time.UTC), OK: true},
				{ID: 3, CreatedAt: time.Date(2026, 9, 20, 23, 30, 0, 0, time.UTC), OK: true},
			},
			daily: 1, weekly: 2, loc: berlinSummer,
			want: []int64{3, 2},
		},
		{name: "a daily count below 1 keeps the newest", versions: dayVersions(3), daily: 0, weekly: 0, want: []int64{2, 1}},
		{name: "nothing to prune", versions: nil, daily: 14, weekly: 8, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loc := tt.loc
			if loc == nil {
				loc = time.UTC
			}
			got := selectPrune(tt.versions, tt.daily, tt.weekly, pruneNow, loc)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("remove %v, want %v", got, tt.want)
			}
		})
	}
}

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
