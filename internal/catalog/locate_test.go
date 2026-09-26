package catalog

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"
)

func TestLocatorLocate(t *testing.T) {
	l := NewLocator([]Source{
		{ID: 1, Path: "/media"},
		{ID: 2, Path: "/media/movies"},
		{ID: 3, Path: "/media2"},
		{ID: 4, Path: "/media/movies"}, // the same path twice: both contain it, lower id first
		{ID: 5, Path: "relative"},      // never located
	})
	cases := []struct {
		name string
		in   string
		want []Location
	}{
		{"root of a source", "/media2", []Location{{SourceID: 3, Rel: ""}}},
		{"overlapping sources, longest first", "/media/movies/Heat (1995)/Heat.mkv", []Location{
			{SourceID: 2, Rel: "Heat (1995)/Heat.mkv"}, {SourceID: 4, Rel: "Heat (1995)/Heat.mkv"}, {SourceID: 1, Rel: "movies/Heat (1995)/Heat.mkv"}}},
		{"segment boundary", "/media/movies-4k/x.mkv", []Location{{SourceID: 1, Rel: "movies-4k/x.mkv"}}},
		{"no prefix match on a partial name", "/media22/x", nil},
		{"outside every source", "/data/x", nil},
		{"relative path refused", "media/movies", nil},
		{"unclean path refused", "/media/movies/../tv", nil},
		{"trailing slash refused", "/media/", nil},
		{"empty", "", nil},
		{"dot-dot inside a name is fine", "/media/Movie..Name/a.mkv", []Location{{SourceID: 1, Rel: "Movie..Name/a.mkv"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := l.Locate(c.in); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("Locate(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
	if p, ok := l.Path(Location{SourceID: 2, Rel: "a/b"}); !ok || p != "/media/movies/a/b" {
		t.Fatalf("Path = %q, %v", p, ok)
	}
	if p, ok := l.Path(Location{SourceID: 3}); !ok || p != "/media2" {
		t.Fatalf("Path(root) = %q, %v", p, ok)
	}
	if _, ok := l.Path(Location{SourceID: 9}); ok {
		t.Fatal("Path of an unknown source succeeded")
	}
}

func TestLocatorRootSource(t *testing.T) {
	l := NewLocator([]Source{{ID: 7, Path: "/"}})
	if got := l.Locate("/tv/x.mkv"); !reflect.DeepEqual(got, []Location{{SourceID: 7, Rel: "tv/x.mkv"}}) {
		t.Fatalf("Locate = %v", got)
	}
	if got := l.Locate("/"); !reflect.DeepEqual(got, []Location{{SourceID: 7, Rel: ""}}) {
		t.Fatalf("Locate(/) = %v", got)
	}
}

func TestStoreLocateOverlappingSources(t *testing.T) {
	st := newStore(t, StoreOptions{})
	root := tempDir(t)
	writeFiles(t, root, map[string]string{"movies/Heat (1995)/Heat.mkv": "x"})
	outer := createSource(t, st, "All", root)
	inner := createSource(t, st, "Movies", filepath.Join(root, "movies"))
	got, err := st.Locate(context.Background(), filepath.ToSlash(filepath.Join(root, "movies", "Heat (1995)")))
	if err != nil {
		t.Fatal(err)
	}
	want := []Location{{SourceID: inner.ID, Rel: "Heat (1995)"}, {SourceID: outer.ID, Rel: "movies/Heat (1995)"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Locate = %v, want %v", got, want)
	}
}

func TestLiveFilesAt(t *testing.T) {
	st := newStore(t, StoreOptions{})
	sc := NewScanner(st, ScannerOptions{})
	root := tempDir(t)
	writeFiles(t, root, map[string]string{"a/x.mkv": "12345", "a/y.mkv": "1", "gone.mkv": "g"})
	src := createSource(t, st, "S", root)
	mustScan(t, sc, src.ID)
	if err := os.Remove(filepath.Join(root, "gone.mkv")); err != nil {
		t.Fatal(err)
	}
	mustScan(t, sc, src.ID)
	locs := []Location{{src.ID, "a/x.mkv"}, {src.ID, "a/x.mkv"}, {src.ID, "gone.mkv"}, {src.ID, "missing.mkv"}, {src.ID, ""}, {src.ID + 9, "a/x.mkv"}}
	got, err := st.LiveFilesAt(context.Background(), nil, locs)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("LiveFilesAt = %v, want only a/x.mkv", got)
	}
	f, ok := got[Location{src.ID, "a/x.mkv"}]
	if !ok || f.Size != 5 || f.ID == 0 || f.MtimeNs == 0 {
		t.Fatalf("a/x.mkv = %+v, %v", f, ok)
	}
	// Through a read transaction as well.
	tx, err := st.db.Reader().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	got, err = st.LiveFilesAt(context.Background(), tx, []Location{{src.ID, "a/y.mkv"}})
	if err != nil || len(got) != 1 {
		t.Fatalf("LiveFilesAt(tx) = %v, %v", got, err)
	}
}

func TestLiveFilesAtBatches(t *testing.T) {
	st := newStore(t, StoreOptions{})
	sc := NewScanner(st, ScannerOptions{})
	root := tempDir(t)
	files := map[string]string{}
	var locs []Location
	for i := range liveAtBatch + 7 {
		rel := filepath.ToSlash(filepath.Join("d", string(rune('a'+i%26)), strconv.Itoa(i)+".mkv"))
		files[rel] = "x"
		locs = append(locs, Location{Rel: rel})
	}
	writeFiles(t, root, files)
	src := createSource(t, st, "S", root)
	mustScan(t, sc, src.ID)
	for i := range locs {
		locs[i].SourceID = src.ID
	}
	got, err := st.LiveFilesAt(context.Background(), nil, locs)
	if err != nil || len(got) != len(locs) {
		t.Fatalf("LiveFilesAt = %d files, %v; want %d", len(got), err, len(locs))
	}
}

func TestDirExists(t *testing.T) {
	st := newStore(t, StoreOptions{})
	root := tempDir(t)
	writeFiles(t, root, map[string]string{"Heat (1995)/Heat.mkv": "x", "file.mkv": "y"})
	if err := os.Symlink(filepath.Join(root, "Heat (1995)"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	src := createSource(t, st, "S", root)
	ctx := context.Background()
	cases := []struct {
		rel  string
		want bool
	}{
		{"", true},
		{"Heat (1995)", true},
		{"Missing (2000)", false},
		{"file.mkv", false},
		{"file.mkv/below", false},
		{"link", false},
		{"../outside", false},
		{"a/../b", false},
	}
	for _, c := range cases {
		got, err := st.DirExists(ctx, Location{SourceID: src.ID, Rel: c.rel})
		if err != nil || got != c.want {
			t.Errorf("DirExists(%q) = %v, %v; want %v", c.rel, got, err, c.want)
		}
	}
	if _, err := st.DirExists(ctx, Location{SourceID: src.ID + 1}); err == nil {
		t.Error("DirExists of an unknown source did not fail")
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if got, err := st.DirExists(ctx, Location{SourceID: src.ID, Rel: "Heat (1995)"}); err != nil || got {
		t.Errorf("DirExists in a missing source root = %v, %v; want false", got, err)
	}
}

// On a case- or normalization-insensitive filesystem DirExists is false for another spelling of
// a folder: a targeted sync of it would key catalog rows by a name that is not on disk.
func TestDirExistsExactNames(t *testing.T) {
	st := newStore(t, StoreOptions{})
	root := tempDir(t)
	if !finds(t, root, "heat (1995)", "Heat (1995)") {
		t.Skip("the filesystem of the temporary directory is case-sensitive")
	}
	writeFiles(t, root, map[string]string{"Movies/Heat (1995)/Heat.mkv": "x"})
	src := createSource(t, st, "S", root)
	ctx := context.Background()
	for _, c := range []struct {
		rel  string
		want bool
	}{
		{"Movies", true},
		{"Movies/Heat (1995)", true},
		{"movies", false},
		{"movies/Heat (1995)", false},
		{"Movies/heat (1995)", false},
		{"MOVIES/HEAT (1995)", false},
	} {
		got, err := st.DirExists(ctx, Location{SourceID: src.ID, Rel: c.rel})
		if err != nil || got != c.want {
			t.Errorf("DirExists(%q) = %v, %v; want %v", c.rel, got, err, c.want)
		}
	}
}

func listingReads(st *Store) int {
	st.dirs.mu.Lock()
	defer st.dirs.mu.Unlock()
	return st.dirs.reads
}

// A refresh checks one folder per changed item, most of them in the same library folder:
// DirExists reads each folder's listing once while the folder is unchanged, not once per call
// (5000 checks in a 5000-entry folder took 31 times as long when every call read the listing).
func TestDirExistsReusesListings(t *testing.T) {
	st := newStore(t, StoreOptions{})
	st.dirs.racy = 20 * time.Millisecond
	root := tempDir(t)
	const n = 300
	for i := range n {
		if err := os.MkdirAll(filepath.Join(root, "Movies", fmt.Sprintf("Movie %03d", i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	src := createSource(t, st, "S", root)
	ctx := context.Background()
	time.Sleep(60 * time.Millisecond) // every folder's last change is older than the racy window
	for i := range n {
		rel := fmt.Sprintf("Movies/Movie %03d", i)
		if got, err := st.DirExists(ctx, Location{SourceID: src.ID, Rel: rel}); err != nil || !got {
			t.Fatalf("DirExists(%q) = %v, %v; want true", rel, got, err)
		}
	}
	if got, err := st.DirExists(ctx, Location{SourceID: src.ID, Rel: "Movies/Missing"}); err != nil || got {
		t.Fatalf("DirExists(Movies/Missing) = %v, %v; want false", got, err)
	}
	if r := listingReads(st); r != 2 {
		t.Fatalf("%d listings read for %d checks in one folder; want 2 (the root and Movies)", r, n+1)
	}
}

// A cached listing is not used once its folder changed: a folder created since, or a case-only
// rename on a case-insensitive filesystem, is answered from a new listing.
func TestDirExistsSeesChangesAfterCaching(t *testing.T) {
	st := newStore(t, StoreOptions{})
	st.dirs.racy = 20 * time.Millisecond
	root := tempDir(t)
	writeFiles(t, root, map[string]string{"Movies/Heat (1995)/h.mkv": "h"})
	src := createSource(t, st, "S", root)
	ctx := context.Background()
	exists := func(rel string) bool {
		t.Helper()
		got, err := st.DirExists(ctx, Location{SourceID: src.ID, Rel: rel})
		if err != nil {
			t.Fatalf("DirExists(%q): %v", rel, err)
		}
		return got
	}
	time.Sleep(60 * time.Millisecond)
	if !exists("Movies/Heat (1995)") || exists("Movies/New") {
		t.Fatal("initial answers are wrong")
	}
	if err := os.Mkdir(filepath.Join(root, "Movies/New"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !exists("Movies/New") {
		t.Fatal("a folder created after Movies was listed is not found")
	}
	// A tool that restores a folder's mtime after changing it (rsync -t) leaves only its ctime
	// changed.
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(filepath.Join(root, "Movies"), old, old); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	if !exists("Movies/Heat (1995)") { // lists Movies again
		t.Fatal("Movies/Heat (1995) is not found")
	}
	if err := os.Mkdir(filepath.Join(root, "Movies/Restored"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(root, "Movies"), old, old); err != nil {
		t.Fatal(err)
	}
	if !exists("Movies/Restored") {
		t.Fatal("a folder created after Movies was listed is not found once Movies' mtime is restored")
	}
	if !finds(t, root, "heat (1995)", "Heat (1995)") {
		return
	}
	time.Sleep(60 * time.Millisecond)
	if !exists("Movies/Heat (1995)") {
		t.Fatal("Movies/Heat (1995) is not found")
	}
	if err := os.Rename(filepath.Join(root, "Movies/Heat (1995)"), filepath.Join(root, "Movies/heat (1995)")); err != nil {
		t.Fatal(err)
	}
	if exists("Movies/Heat (1995)") || !exists("Movies/heat (1995)") {
		t.Fatal("a case-only rename after Movies was listed is answered from the old listing")
	}
}

// On a union filesystem a folder's stat does not follow its listing: mergerfs (func.getattr=ff)
// stats a folder on the first branch that has it, with a path-hashed inode, while it lists every
// branch, so a folder created on another branch leaves the stat as it was. A folder the lookup finds
// but a kept listing lacks is listed again rather than reported missing (which sent a refresh to a
// whole-source sync with a warning to check correct path mappings); names the listing has are still
// answered from it.
func TestDirExistsRelistsWhenKeptListingLacksAFoundName(t *testing.T) {
	st := newStore(t, StoreOptions{})
	st.dirs.racy = 20 * time.Millisecond
	root := tempDir(t)
	writeFiles(t, root, map[string]string{"Movies/A (2024)/a.mkv": "a"})
	src := createSource(t, st, "S", root)
	ctx := context.Background()
	exists := func(rel string) bool {
		t.Helper()
		got, err := st.DirExists(ctx, Location{SourceID: src.ID, Rel: rel})
		if err != nil {
			t.Fatalf("DirExists(%q): %v", rel, err)
		}
		return got
	}
	time.Sleep(60 * time.Millisecond)
	if !exists("Movies/A (2024)") {
		t.Fatal("Movies/A (2024) is not found")
	}
	if err := os.Mkdir(filepath.Join(root, "Movies/B (2024)"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Simulate the union filesystem: the kept listing of Movies (without B) now carries the stat
	// Movies has after B was created, as if the stat had not changed.
	fi, err := os.Stat(filepath.Join(root, "Movies"))
	if err != nil {
		t.Fatal(err)
	}
	stamp, ok := stampOf(fi)
	if !ok {
		t.Skip("no folder stat on this platform")
	}
	st.dirs.mu.Lock()
	l := st.dirs.m[listingKey{src.Path, "Movies"}]
	if l == nil {
		st.dirs.mu.Unlock()
		t.Fatal("the listing of Movies is not kept")
	}
	l.stamp = stamp
	st.dirs.mu.Unlock()
	before := listingReads(st)
	if !exists("Movies/B (2024)") {
		t.Fatal("a folder the lookup finds but a kept listing lacks is reported missing")
	}
	if r := listingReads(st) - before; r != 1 {
		t.Fatalf("%d listings read; want 1 (Movies again)", r)
	}
	before = listingReads(st)
	if !exists("Movies/A (2024)") {
		t.Fatal("Movies/A (2024) is not found")
	}
	if r := listingReads(st) - before; r > 1 {
		t.Fatalf("%d listings read for a name a kept listing has", r)
	}
}

// A listing read within the racy window of its folder's last change is not kept: a change in the
// same timestamp tick (a coarse clock, FAT's 2 s mtime) would leave the folder's stat as it was.
func TestDirExistsDoesNotKeepRacyListings(t *testing.T) {
	st := newStore(t, StoreOptions{})
	root := tempDir(t)
	writeFiles(t, root, map[string]string{"Movies/Heat (1995)/h.mkv": "h"})
	src := createSource(t, st, "S", root)
	ctx := context.Background()
	// An old mtime does not make a folder whose ctime is recent safe to keep.
	old := time.Now().Add(-time.Hour)
	for _, dir := range []string{root, filepath.Join(root, "Movies")} {
		if err := os.Chtimes(dir, old, old); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if got, err := st.DirExists(ctx, Location{SourceID: src.ID, Rel: "Movies/Heat (1995)"}); err != nil || !got {
			t.Fatalf("DirExists = %v, %v", got, err)
		}
	}
	if r := listingReads(st); r != 4 {
		t.Fatalf("%d listings read; want 4 (folders changed just now are listed on every call)", r)
	}
}
