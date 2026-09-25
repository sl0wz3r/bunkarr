package catalog

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sl0wz3r/bunkarr/internal/faultinject"
)

func TestScanIncremental(t *testing.T) {
	ctx := context.Background()
	st := newStore(t, StoreOptions{})
	sc := NewScanner(st, ScannerOptions{})
	root := tempDir(t)
	writeFiles(t, root, map[string]string{
		"a.mkv":       "aaaa",
		"dir/b.mkv":   "bbbbbbbb",
		"dir/c.srt":   "cc",
		"dir/d/e.mkv": "eeeee",
	})
	src := createSource(t, st, "Movies", root)

	rep := &recReporter{}
	r1, err := sc.Scan(ctx, src.ID, rep)
	if err != nil {
		t.Fatalf("scan 1: %v", err)
	}
	if r1.Files != 4 || r1.Added != 4 || r1.Changed != 0 || r1.Deleted != 0 || r1.Bytes != 19 {
		t.Fatalf("scan 1 = %+v", r1)
	}
	if len(rep.progress) == 0 || rep.progress[0].Phase != "scanning" {
		t.Fatalf("no scanning progress reported: %+v", rep.progress)
	}
	before := allRows(t, st, src.ID)
	oldB := before["dir/b.mkv"]

	// Change a.mkv, add new.mkv, delete dir/c.srt, rename dir/b.mkv -> dir/b2.mkv.
	writeFiles(t, root, map[string]string{"a.mkv": "aaaaaa", "new.mkv": "n"})
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(root, "a.mkv"), future, future); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "dir/c.srt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "dir/b.mkv"), filepath.Join(root, "dir/b2.mkv")); err != nil {
		t.Fatal(err)
	}
	r2 := mustScan(t, sc, src.ID)
	if r2.Files != 4 || r2.Added != 2 || r2.Changed != 1 || r2.Deleted != 2 {
		t.Fatalf("scan 2 = %+v", r2)
	}
	live := liveRows(t, st, src.ID)
	want := []string{"a.mkv", "dir/b2.mkv", "dir/d/e.mkv", "new.mkv"}
	got := keys(live)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("live = %v, want %v", got, want)
	}
	if live["a.mkv"].Size != 6 || live["a.mkv"].MtimeNs != future.UnixNano() {
		t.Fatalf("a.mkv not updated: %+v", live["a.mkv"])
	}
	if live["a.mkv"].ID != before["a.mkv"].ID {
		t.Fatal("a changed row got a new id")
	}
	// The planner's move detection: the vanished row keeps the inode it had, the new name has it.
	var gone []File
	if err := st.Deleted(ctx, src.ID, r2.StartedAt, func(f File) error { gone = append(gone, f); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(gone) != 2 || gone[0].RelPath != "dir/b.mkv" || gone[1].RelPath != "dir/c.srt" {
		t.Fatalf("Deleted = %+v", gone)
	}
	if gone[0].Inode != oldB.Inode || live["dir/b2.mkv"].Inode != oldB.Inode {
		t.Fatalf("rename lost the inode: old %d, deleted row %d, new row %d", oldB.Inode, gone[0].Inode, live["dir/b2.mkv"].Inode)
	}
	if !gone[0].DeletedAt.Equal(r2.StartedAt) {
		t.Fatalf("deleted_at = %v, want scan start %v", gone[0].DeletedAt, r2.StartedAt)
	}

	// Nothing changed: nothing counted, every live row seen by this scan.
	r3 := mustScan(t, sc, src.ID)
	if r3.Added != 0 || r3.Changed != 0 || r3.Deleted != 0 || r3.Files != 4 {
		t.Fatalf("scan 3 = %+v", r3)
	}
	for rel, f := range liveRows(t, st, src.ID) {
		if !f.LastSeenAt.Equal(r3.StartedAt) {
			t.Fatalf("%s last seen %v, want %v", rel, f.LastSeenAt, r3.StartedAt)
		}
	}
	// Earlier deletions are not reported as this scan's.
	n := 0
	if err := st.Deleted(ctx, src.ID, r3.StartedAt, func(File) error { n++; return nil }); err != nil || n != 0 {
		t.Fatalf("Deleted since scan 3 = %d, %v", n, err)
	}

	// A file that comes back is live again, same row.
	writeFiles(t, root, map[string]string{"dir/c.srt": "cc"})
	r4 := mustScan(t, sc, src.ID)
	if r4.Added != 1 {
		t.Fatalf("scan 4 = %+v", r4)
	}
	after := allRows(t, st, src.ID)
	if after["dir/c.srt"].DeletedAt != nil || after["dir/c.srt"].ID != before["dir/c.srt"].ID {
		t.Fatalf("reappeared row = %+v (before id %d)", after["dir/c.srt"], before["dir/c.srt"].ID)
	}

	got2, err := st.Get(ctx, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got2.LastScanStatus != ScanStatusOK || got2.LastScanAt == nil || got2.FSType == "" || got2.Stats.Files != 5 {
		t.Fatalf("source after scans = %+v", got2)
	}
}

func TestScanHardlinks(t *testing.T) {
	ctx := context.Background()
	st := newStore(t, StoreOptions{})
	sc := NewScanner(st, ScannerOptions{})
	base := tempDir(t)
	root := filepath.Join(base, "media")
	writeFiles(t, root, map[string]string{
		"torrents/x.mkv": strings.Repeat("x", 1000),
		"single.mkv":     strings.Repeat("s", 500),
	})
	for _, l := range []string{"movies/X (2020)/x.mkv", "movies/x-copy.mkv"} {
		p := filepath.Join(root, filepath.FromSlash(l))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(filepath.Join(root, "torrents/x.mkv"), p); err != nil {
			t.Fatal(err)
		}
	}
	// A fourth name outside the source: nlink 4, three names inside.
	if err := os.Link(filepath.Join(root, "torrents/x.mkv"), filepath.Join(base, "outside.mkv")); err != nil {
		t.Fatal(err)
	}
	src := createSource(t, st, "Media", root)

	r := mustScan(t, sc, src.ID)
	if r.Groups != 1 || r.GroupedFiles != 3 || r.WarningCount != 0 {
		t.Fatalf("scan = %+v", r)
	}
	got, err := st.Get(ctx, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := Stats{Files: 4, Bytes: 3500, UniqueBytes: 1500, HardlinkGroups: 1, HardlinkedFiles: 3}
	if got.Stats != want {
		t.Fatalf("stats = %+v, want %+v", got.Stats, want)
	}
	page, err := st.Files(ctx, src.ID, Query{Filter: FilterHardlinked})
	if err != nil {
		t.Fatal(err)
	}
	if page.TotalRecords != 3 {
		t.Fatalf("hardlinked = %d", page.TotalRecords)
	}
	g := page.Records[0].HardlinkGroup
	if g != fmt.Sprintf("%d.1:1", src.ID) {
		t.Fatalf("group id = %q", g)
	}
	for _, f := range page.Records {
		if f.HardlinkGroup != g || f.Nlink != 4 {
			t.Fatalf("member %+v", f)
		}
	}
	if gs, err := st.Stats(ctx); err != nil || gs.UniqueBytes != 1500 || gs.Bytes != 3500 || gs.Sources != 1 || gs.HardlinkedFiles != 3 {
		t.Fatalf("global stats = %+v, %v", gs, err)
	}

	// Per-scan ids: the next scan uses new ids; a name replaced by a new inode leaves the group.
	if err := os.Remove(filepath.Join(root, "movies/x-copy.mkv")); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, root, map[string]string{"movies/x-copy.mkv": strings.Repeat("x", 1000)})
	r2 := mustScan(t, sc, src.ID)
	if r2.Groups != 1 || r2.GroupedFiles != 2 {
		t.Fatalf("scan 2 = %+v", r2)
	}
	live := liveRows(t, st, src.ID)
	g2 := live["torrents/x.mkv"].HardlinkGroup
	if g2 == g || g2 != fmt.Sprintf("%d.2:1", src.ID) || live["movies/X (2020)/x.mkv"].HardlinkGroup != g2 {
		t.Fatalf("scan 2 group ids: %q (scan 1 %q)", g2, g)
	}
	if live["movies/x-copy.mkv"].HardlinkGroup != "" {
		t.Fatalf("replaced name still grouped: %+v", live["movies/x-copy.mkv"])
	}
	got, _ = st.Get(ctx, src.ID)
	if got.Stats.UniqueBytes != 2500 || got.Stats.Bytes != 3500 {
		t.Fatalf("stats after split = %+v", got.Stats)
	}
}

func TestScanNeverTouchesSource(t *testing.T) {
	st := newStore(t, StoreOptions{})
	sc := NewScanner(st, ScannerOptions{})
	// FUSE mode also opens files for head/tail hashing.
	sc.identityHook = func(id rootIdentity) rootIdentity { id.fuse = true; return id }
	root := tempDir(t)
	writeFiles(t, root, map[string]string{"a/1.mkv": "one", "b/2.mkv": strings.Repeat("2", 3<<20)})
	if err := os.Link(filepath.Join(root, "b/2.mkv"), filepath.Join(root, "a/2.mkv")); err != nil {
		t.Fatal(err)
	}
	src := createSource(t, st, "S", root)
	before := treeState(t, root)
	r := mustScan(t, sc, src.ID)
	if r.Groups != 1 {
		t.Fatalf("scan = %+v", r)
	}
	after := treeState(t, root)
	if len(after) != len(before) {
		t.Fatalf("entries changed: %d -> %d", len(before), len(after))
	}
	for p, v := range before {
		if after[p] != v {
			t.Fatalf("scan changed %s: %s -> %s", p, v, after[p])
		}
	}
}

func TestScanSkipsSymlinksAndSpecialFiles(t *testing.T) {
	st := newStore(t, StoreOptions{})
	sc := NewScanner(st, ScannerOptions{})
	var hashed []string
	sc.identityHook = func(id rootIdentity) rootIdentity { id.fuse = true; return id }
	sc.hashFile = func(r *os.Root, rel string, m Meta) (string, error) {
		hashed = append(hashed, rel)
		return headTailHash(r, rel, m)
	}
	base := tempDir(t)
	root := filepath.Join(base, "src")
	writeFiles(t, base, map[string]string{"secret/outside.txt": "outside"})
	writeFiles(t, root, map[string]string{"real.mkv": "r", "dir/inner.mkv": "i"})
	// The outside file is unreadable: reading it through the link would fail the test via a warning.
	if err := os.Chmod(filepath.Join(base, "secret/outside.txt"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(base, "secret/outside.txt"), 0o644) })
	for link, target := range map[string]string{
		"to-outside.mkv": filepath.Join(base, "secret/outside.txt"),
		"to-dir":         filepath.Join(base, "secret"),
		"to-inner.mkv":   "dir/inner.mkv",
		"loop":           ".",
	} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	// A FIFO: opening it for reading would block the scan forever.
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := createSource(t, st, "S", root)
	r := mustScan(t, sc, src.ID)
	if r.Files != 2 || r.Skipped[SkipSymlink] != 4 || r.Skipped[SkipFIFO] != 1 || r.WarningCount != 0 {
		t.Fatalf("scan = %+v", r)
	}
	got := keys(liveRows(t, st, src.ID))
	slices.Sort(got)
	if !slices.Equal(got, []string{"dir/inner.mkv", "real.mkv"}) {
		t.Fatalf("live = %v", got)
	}
	if len(hashed) != 0 {
		t.Fatalf("files opened: %v", hashed)
	}
	s, _ := st.Get(context.Background(), src.ID)
	if s.Stats.Skipped != 5 {
		t.Fatalf("stats.skipped = %d", s.Stats.Skipped)
	}
}

func TestScanExcludes(t *testing.T) {
	ctx := context.Background()
	st := newStore(t, StoreOptions{})
	sc := NewScanner(st, ScannerOptions{})
	root := tempDir(t)
	writeFiles(t, root, map[string]string{
		"keep.mkv":                "k",
		".DS_Store":               "x",
		"DESKTOP.INI":             "x",
		"._keep.mkv":              "x",
		"@eaDir/keep.mkv/SYNO":    "x",
		"dl/movie.mkv.partial":    "x",
		"dl/movie.mkv.!qB":        "x",
		".bunkarr/marker":         "x",
		"Show/S01/e1.mkv":         "e",
		"Show/S01/e1.nfo":         "n",
		"Extras/bonus.mkv":        "b",
		"Show/Extras/bonus2.mkv":  "b",
		"Movie/Samples/s.mkv":     "s",
		"Movie/Samples.mkv":       "file named like the dir pattern",
		"lost+found/x":            "x",
		"notes/lost+found.txt":    "a file, not the directory",
		".Trash-1000/files/x.mkv": "x",
	})
	src, err := st.Create(ctx, SourceInput{Name: "S", Path: root, Exclude: []string{"*.nfo", "/Extras/", "Samples/"}})
	if err != nil {
		t.Fatal(err)
	}
	r := mustScan(t, sc, src.ID)
	got := keys(liveRows(t, st, src.ID))
	slices.Sort(got)
	want := []string{"Movie/Samples.mkv", "Show/Extras/bonus2.mkv", "Show/S01/e1.mkv", "keep.mkv", "notes/lost+found.txt"}
	if !slices.Equal(got, want) {
		t.Fatalf("live = %v\nwant %v", got, want)
	}
	if r.Excluded != 12 {
		t.Fatalf("excluded = %d, want 12", r.Excluded)
	}

	// A new exclude pattern: its files are no longer backed up (marked deleted, retained by syncs).
	if _, err := st.Update(ctx, src.ID, SourceInput{Name: "S", Path: root, Exclude: []string{"*.nfo", "/Extras/", "Samples/", "Show/"}}); err != nil {
		t.Fatal(err)
	}
	r2 := mustScan(t, sc, src.ID)
	if r2.Deleted != 2 {
		t.Fatalf("scan 2 = %+v", r2)
	}
}

func TestScanUnreadableSubtreeKeepsRows(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	ctx := context.Background()
	st := newStore(t, StoreOptions{})
	sc := NewScanner(st, ScannerOptions{})
	root := tempDir(t)
	writeFiles(t, root, map[string]string{"locked/a.mkv": "a", "locked/deep/b.mkv": "b", "open/c.mkv": "c", "open/d.mkv": "d"})
	src := createSource(t, st, "S", root)
	mustScan(t, sc, src.ID)

	locked := filepath.Join(root, "locked")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	if err := os.Remove(filepath.Join(root, "open/d.mkv")); err != nil {
		t.Fatal(err)
	}
	r := mustScan(t, sc, src.ID)
	if r.Deleted != 1 || r.WarningCount != 1 || !slices.Equal(r.UnreadableDirs, []string{"locked"}) {
		t.Fatalf("scan = %+v", r)
	}
	live := liveRows(t, st, src.ID)
	for _, rel := range []string{"locked/a.mkv", "locked/deep/b.mkv", "open/c.mkv"} {
		if _, ok := live[rel]; !ok {
			t.Fatalf("%s was marked deleted", rel)
		}
	}
	if _, ok := live["open/d.mkv"]; ok {
		t.Fatal("open/d.mkv not marked deleted")
	}
	s, _ := st.Get(ctx, src.ID)
	if s.LastScanStatus != ScanStatusWarnings || s.Stats.Files != 3 {
		t.Fatalf("source = %+v", s)
	}
}

// catalogUnchanged asserts that a refused scan left the catalog exactly as before.
func catalogUnchanged(t *testing.T, st *Store, id int64, before map[string]File) {
	t.Helper()
	after := allRows(t, st, id)
	if len(after) != len(before) {
		t.Fatalf("rows changed: %v -> %v", keys(before), keys(after))
	}
	for rel, f := range before {
		a := after[rel]
		if a.DeletedAt != nil && f.DeletedAt == nil || a.Size != f.Size || !a.LastSeenAt.Equal(f.LastSeenAt) || a.HardlinkGroup != f.HardlinkGroup {
			t.Fatalf("%s changed: %+v -> %+v", rel, f, a)
		}
	}
}

func TestScanEmptyRootRefused(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		empty func(t *testing.T, root string)
	}{
		{"no entries", func(t *testing.T, root string) {
			for _, n := range []string{"a.mkv", "sub"} {
				if err := os.RemoveAll(filepath.Join(root, n)); err != nil {
					t.Fatal(err)
				}
			}
		}},
		{"only empty directories and excluded files", func(t *testing.T, root string) {
			if err := os.RemoveAll(filepath.Join(root, "sub")); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(root, "a.mkv")); err != nil {
				t.Fatal(err)
			}
			writeFiles(t, root, map[string]string{".DS_Store": "x"})
			if err := os.MkdirAll(filepath.Join(root, "Movies/Empty"), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t, StoreOptions{})
			sc := NewScanner(st, ScannerOptions{})
			root := tempDir(t)
			writeFiles(t, root, map[string]string{"a.mkv": "a", "sub/b.mkv": "b"})
			src := createSource(t, st, "S", root)
			mustScan(t, sc, src.ID)
			before := allRows(t, st, src.ID)
			statsBefore, _ := st.Get(ctx, src.ID)
			tc.empty(t, root)
			_, err := sc.Scan(ctx, src.ID, nil)
			if !errors.Is(err, ErrScanRefused) || !strings.Contains(err.Error(), "contains no files") {
				t.Fatalf("err = %v, want a refused empty scan", err)
			}
			catalogUnchanged(t, st, src.ID, before)
			s, _ := st.Get(ctx, src.ID)
			if s.LastScanStatus != ScanStatusFailed || s.Stats != statsBefore.Stats {
				t.Fatalf("source after refusal = %+v", s)
			}
		})
	}

	// A first scan of an empty source is fine: there is nothing to lose.
	st := newStore(t, StoreOptions{})
	src := createSource(t, st, "Empty", tempDir(t))
	if _, err := NewScanner(st, ScannerOptions{}).Scan(ctx, src.ID, nil); err != nil {
		t.Fatalf("scan of a new empty source: %v", err)
	}
}

func TestScanMissingRootRefused(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		replace func(t *testing.T, root string)
		msg     string
	}{
		{"missing", func(t *testing.T, root string) {}, "does not exist"},
		{"file", func(t *testing.T, root string) {
			if err := os.WriteFile(root, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, "not a directory"},
		{"symlink", func(t *testing.T, root string) {
			other := tempDir(t)
			writeFiles(t, other, map[string]string{"a.mkv": "a"})
			if err := os.Symlink(other, root); err != nil {
				t.Fatal(err)
			}
		}, "symlink"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t, StoreOptions{})
			sc := NewScanner(st, ScannerOptions{})
			root := filepath.Join(tempDir(t), "media")
			writeFiles(t, root, map[string]string{"a.mkv": "a", "b.mkv": "b"})
			src := createSource(t, st, "S", root)
			mustScan(t, sc, src.ID)
			before := allRows(t, st, src.ID)
			if err := os.RemoveAll(root); err != nil {
				t.Fatal(err)
			}
			tc.replace(t, root)
			_, err := sc.Scan(ctx, src.ID, nil)
			if !errors.Is(err, ErrScanRefused) || !strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("err = %v, want refused (%s)", err, tc.msg)
			}
			catalogUnchanged(t, st, src.ID, before)
		})
	}
}

func TestScanFilesystemChangeRefused(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		change func(rootIdentity) rootIdentity
	}{
		{"fs type", func(id rootIdentity) rootIdentity { id.fsType = "tmpfs"; return id }},
		{"fs type on an anonymous device", func(id rootIdentity) rootIdentity {
			id.fsType = "nfs"
			id.dev++
			id.anonDev = true
			return id
		}},
		{"device", func(id rootIdentity) rootIdentity { id.dev++; id.anonDev = false; return id }},
		// NFS, btrfs, ZFS and tmpfs keep the root inode across a remount: another one is another
		// directory, such as the mount point of a dataset or export that failed to mount under a
		// parent of the same type, even when the device number was reused after a reboot.
		{"device and root inode on an anonymous device", func(id rootIdentity) rootIdentity {
			id.dev++
			id.ino++
			id.anonDev, id.stableIno = true, true
			return id
		}},
		{"root inode on an anonymous device", func(id rootIdentity) rootIdentity {
			id.ino++
			id.anonDev, id.stableIno = true, true
			return id
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t, StoreOptions{})
			sc := NewScanner(st, ScannerOptions{})
			root := tempDir(t)
			writeFiles(t, root, map[string]string{"a.mkv": "a", "b.mkv": "b"})
			src := createSource(t, st, "S", root)
			mustScan(t, sc, src.ID)
			before := allRows(t, st, src.ID)
			writeFiles(t, root, map[string]string{"new.mkv": "n"})
			if err := os.Remove(filepath.Join(root, "b.mkv")); err != nil {
				t.Fatal(err)
			}
			sc.identityHook = tc.change
			_, err := sc.Scan(ctx, src.ID, nil)
			if !errors.Is(err, ErrScanRefused) || !strings.Contains(err.Error(), "re-save the source without changing it") {
				t.Fatalf("err = %v, want refused", err)
			}
			catalogUnchanged(t, st, src.ID, before)

			// Re-saving the source accepts the new filesystem.
			if _, err := st.Update(ctx, src.ID, SourceInput{Name: "S", Path: root}); err != nil {
				t.Fatal(err)
			}
			r, err := sc.Scan(ctx, src.ID, nil)
			if err != nil || r.Added != 1 || r.Deleted != 1 {
				t.Fatalf("scan after re-save = %+v, %v", r, err)
			}
		})
	}
}

// The kernel assigns an anonymous device number (FUSE such as Unraid's /mnt/user, NFS, CIFS, btrfs,
// ZFS) at mount time, so it can change after a reboot or remount: the scan goes on and records the
// new number. The other S10a checks still refuse on such a device.
func TestScanAnonymousDeviceChangeAccepted(t *testing.T) {
	ctx := context.Background()
	st := newStore(t, StoreOptions{})
	sc := NewScanner(st, ScannerOptions{})
	remount := func(shift uint64) func(rootIdentity) rootIdentity {
		return func(id rootIdentity) rootIdentity { id.dev += shift; id.anonDev = true; return id }
	}
	sc.identityHook = remount(0)
	root := tempDir(t)
	writeFiles(t, root, map[string]string{"a.mkv": "a", "b.mkv": "b"})
	src := createSource(t, st, "S", root)
	mustScan(t, sc, src.ID)
	first, err := st.identity(ctx, src.ID)
	if err != nil || !first.rootDev.Valid {
		t.Fatalf("identity = %+v, %v", first, err)
	}
	recordedDev := func() uint64 {
		t.Helper()
		id, err := st.identity(ctx, src.ID)
		if err != nil || !id.rootDev.Valid {
			t.Fatalf("identity = %+v, %v", id, err)
		}
		return uint64(id.rootDev.Int64)
	}

	// Remounted with another device number.
	sc.identityHook = remount(7)
	writeFiles(t, root, map[string]string{"new.mkv": "n"})
	rep := &recReporter{}
	r, err := sc.Scan(ctx, src.ID, rep)
	if err != nil || r.Added != 1 {
		t.Fatalf("scan after a remount = %+v, %v", r, err)
	}
	if !rep.hasLog("device number changed") {
		t.Fatalf("the new device number was not logged: %q", rep.logs)
	}
	if got, want := recordedDev(), uint64(first.rootDev.Int64)+7; got != want {
		t.Fatalf("recorded root_dev = %d, want the new %d", got, want)
	}
	rep = &recReporter{}
	if _, err := sc.Scan(ctx, src.ID, rep); err != nil || rep.hasLog("device number changed") {
		t.Fatalf("scan with the recorded device = %v, logs %q", err, rep.logs)
	}

	// Another filesystem type, or an empty root while the catalog lists files, is still refused,
	// and a refused scan does not record the device number.
	before := allRows(t, st, src.ID)
	sc.identityHook = func(id rootIdentity) rootIdentity { id = remount(9)(id); id.fsType = "nfs"; return id }
	if _, err := sc.Scan(ctx, src.ID, nil); !errors.Is(err, ErrScanRefused) || !strings.Contains(err.Error(), "filesystem") {
		t.Fatalf("fs type change = %v, want refused", err)
	}
	catalogUnchanged(t, st, src.ID, before)
	sc.identityHook = remount(9)
	for _, name := range []string{"a.mkv", "b.mkv", "new.mkv"} {
		if err := os.Remove(filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sc.Scan(ctx, src.ID, nil); !errors.Is(err, ErrScanRefused) || !strings.Contains(err.Error(), "contains no files") {
		t.Fatalf("empty root = %v, want refused", err)
	}
	catalogUnchanged(t, st, src.ID, before)
	if got, want := recordedDev(), uint64(first.rootDev.Int64)+7; got != want {
		t.Fatalf("recorded root_dev after refused scans = %d, want %d", got, want)
	}
}

// A nested mount that failed leaves its mount point, a directory of the parent filesystem of the
// same type, in place of the source. Where root inodes survive a remount (NFS, btrfs, ZFS, tmpfs)
// the changed root inode refuses the scan even though the anonymous device number changed; FUSE
// and CIFS/SMB assign inode numbers at run time, so there only the other S10a checks apply.
func TestScanAnonymousDeviceRootInodeChange(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		stableIno bool
		devShift  uint64
		refused   bool
	}{
		{"remounted with stable inodes", true, 7, true},
		{"device number reused, stable inodes", true, 0, true},
		{"remounted with run-time inodes", false, 7, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t, StoreOptions{})
			sc := NewScanner(st, ScannerOptions{})
			anon := func(id rootIdentity) rootIdentity { id.anonDev, id.stableIno = true, tc.stableIno; return id }
			sc.identityHook = anon
			root := tempDir(t)
			writeFiles(t, root, map[string]string{"a.mkv": "a", "b.mkv": "b", "c/d.mkv": "d"})
			src := createSource(t, st, "S", root)
			mustScan(t, sc, src.ID)
			first, err := st.identity(ctx, src.ID)
			if err != nil || !first.rootDev.Valid || !first.rootIno.Valid {
				t.Fatalf("identity = %+v, %v", first, err)
			}
			before := allRows(t, st, src.ID)
			if err := os.RemoveAll(root); err != nil {
				t.Fatal(err)
			}
			writeFiles(t, root, map[string]string{"Downloads/new.mkv": "n"})
			sc.identityHook = func(id rootIdentity) rootIdentity {
				id = anon(id)
				id.dev = uint64(first.rootDev.Int64) + tc.devShift
				id.ino = uint64(first.rootIno.Int64) + 1
				return id
			}
			r, err := sc.Scan(ctx, src.ID, nil)
			if tc.refused {
				if !errors.Is(err, ErrScanRefused) || !strings.Contains(err.Error(), "different directory") ||
					!strings.Contains(err.Error(), "re-save the source") {
					t.Fatalf("err = %v, want refused", err)
				}
				catalogUnchanged(t, st, src.ID, before)
				return
			}
			if err != nil || r.Added != 1 || r.Deleted != 3 {
				t.Fatalf("scan = %+v, %v", r, err)
			}
			got, err := st.identity(ctx, src.ID)
			if err != nil || got.rootDev.Int64 != first.rootDev.Int64+int64(tc.devShift) || got.rootIno.Int64 != first.rootIno.Int64+1 {
				t.Fatalf("recorded identity = %+v, %v", got, err)
			}
		})
	}
}

// The classification behind anonDev and stableIno, on every platform.
func TestNewRootIdentity(t *testing.T) {
	m := Meta{Dev: unix.Mkdev(0, 52), Inode: 3}
	disk := Meta{Dev: unix.Mkdev(8, 17), Inode: 3}
	for _, tc := range []struct {
		fsType             string
		fuse               bool
		m                  Meta
		anonDev, stableIno bool
	}{
		{"nfs", false, m, true, true},
		{"cifs", false, m, true, false},
		{"fuse", true, m, true, false},
		{"ext4", false, disk, false, true},
	} {
		got := newRootIdentity(tc.fsType, tc.fuse, tc.m)
		want := rootIdentity{fsType: tc.fsType, fuse: tc.fuse, anonDev: tc.anonDev, stableIno: tc.stableIno, dev: tc.m.Dev, ino: tc.m.Inode}
		if got != want {
			t.Errorf("newRootIdentity(%s) = %+v, want %+v", tc.fsType, got, want)
		}
	}
}

// rootIdentityOf classifies the opened root like newRootIdentity does its statfs and stat.
func TestRootIdentityOf(t *testing.T) {
	dir := tempDir(t)
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	got, err := rootIdentityOf(root)
	if err != nil {
		t.Fatal(err)
	}
	fsType, fuse, err := fsTypeOfPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := MetaOf(fi)
	if want := newRootIdentity(fsType, fuse, m); got != want || got.anonDev != anonymousDev(fsType, m.Dev) {
		t.Fatalf("rootIdentityOf = %+v, want %+v", got, want)
	}
}

func TestScanFatalIORefused(t *testing.T) {
	ctx := context.Background()
	for _, errno := range []syscall.Errno{syscall.EIO, syscall.ENOTCONN, syscall.ESTALE} {
		for _, at := range []string{".", "b", "b/c"} {
			t.Run(fmt.Sprintf("%s at %s", errno, at), func(t *testing.T) {
				st := newStore(t, StoreOptions{})
				sc := NewScanner(st, ScannerOptions{})
				sc.batchSize = 1 // batches would be committed early if the walk wrote anything
				root := tempDir(t)
				writeFiles(t, root, map[string]string{"a.mkv": "a", "b/x.mkv": "x", "b/c/y.mkv": "y", "d/z.mkv": "z"})
				src := createSource(t, st, "S", root)
				mustScan(t, sc, src.ID)
				before := allRows(t, st, src.ID)
				writeFiles(t, root, map[string]string{"a.mkv": "changed", "new.mkv": "n"})
				if err := os.Remove(filepath.Join(root, "d/z.mkv")); err != nil {
					t.Fatal(err)
				}
				sc.beforeReadDir = func(rel string) error {
					if rel == at {
						return &os.PathError{Op: "readdirent", Path: rel, Err: errno}
					}
					return nil
				}
				_, err := sc.Scan(ctx, src.ID, nil)
				if !errors.Is(err, ErrScanRefused) || !errors.Is(err, errno) {
					t.Fatalf("err = %v, want refused with %v", err, errno)
				}
				catalogUnchanged(t, st, src.ID, before)
			})
		}
	}
}

func TestScanCancelDuringWalkWritesNothing(t *testing.T) {
	st := newStore(t, StoreOptions{})
	sc := NewScanner(st, ScannerOptions{})
	sc.batchSize = 1
	root := tempDir(t)
	writeFiles(t, root, map[string]string{"a/1.mkv": "1", "b/2.mkv": "2", "c/3.mkv": "3"})
	src := createSource(t, st, "S", root)
	mustScan(t, sc, src.ID)
	before := allRows(t, st, src.ID)
	writeFiles(t, root, map[string]string{"a/new.mkv": "n"})
	if err := os.Remove(filepath.Join(root, "c/3.mkv")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sc.beforeReadDir = func(rel string) error {
		if rel == "b" {
			cancel()
		}
		return nil
	}
	_, err := sc.Scan(ctx, src.ID, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	catalogUnchanged(t, st, src.ID, before)
	s, _ := st.Get(context.Background(), src.ID)
	if s.LastScanStatus != ScanStatusOK {
		t.Fatalf("a cancelled scan changed the status to %q", s.LastScanStatus)
	}
}

func TestScanCancelOrCrashDuringCommitMarksNoDeletions(t *testing.T) {
	for _, mode := range []string{"cancel", "crash"} {
		t.Run(mode, func(t *testing.T) {
			st := newStore(t, StoreOptions{})
			sc := NewScanner(st, ScannerOptions{})
			sc.batchSize = 2
			root := tempDir(t)
			files := map[string]string{}
			for i := range 6 {
				files[fmt.Sprintf("f%d.mkv", i)] = "x"
			}
			writeFiles(t, root, files)
			src := createSource(t, st, "S", root)
			mustScan(t, sc, src.ID)
			writeFiles(t, root, map[string]string{"n1.mkv": "1", "n2.mkv": "2", "n3.mkv": "3", "f0.mkv": "changed"})
			if err := os.Remove(filepath.Join(root, "f5.mkv")); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "cancel":
				faultinject.SetHook(func(name string) {
					if name == "scan.afterBatch" {
						cancel()
					}
				})
			case "crash":
				faultinject.SetHook(faultinject.CrashAt("scan.afterBatch", 1))
			}
			t.Cleanup(func() { faultinject.SetHook(nil) })
			err := func() (err error) {
				defer func() {
					if p := recover(); p != nil {
						if _, ok := p.(faultinject.Crash); !ok {
							panic(p)
						}
						err = errors.New("crashed")
					}
				}()
				_, err = sc.Scan(ctx, src.ID, nil)
				return err
			}()
			if err == nil {
				t.Fatal("scan was not interrupted")
			}
			faultinject.SetHook(nil)
			// One batch (f0 changed, n1 added) was committed, no deletion was marked.
			rows := allRows(t, st, src.ID)
			if f := rows["f5.mkv"]; f.DeletedAt != nil {
				t.Fatal("interrupted scan marked a deletion")
			}
			if len(rows) != 7 || rows["f0.mkv"].Size != int64(len("changed")) {
				t.Fatalf("rows after the first batch = %v", keys(rows))
			}
			// The lock was released and a new scan completes the job.
			r, err := sc.Scan(context.Background(), src.ID, nil)
			if err != nil || r.Deleted != 1 || len(liveRows(t, st, src.ID)) != 8 {
				t.Fatalf("rescan = %+v, %v", r, err)
			}
		})
	}
}

func TestScanFUSEGroupsNeedEqualContent(t *testing.T) {
	st := newStore(t, StoreOptions{})
	sc := NewScanner(st, ScannerOptions{})
	sc.identityHook = func(id rootIdentity) rootIdentity { id.fuse = true; return id }
	root := tempDir(t)
	writeFiles(t, root, map[string]string{"a/x.mkv": "same", "c/y.mkv": "other"})
	if err := os.Link(filepath.Join(root, "a/x.mkv"), filepath.Join(root, "b-x.mkv")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, "c/y.mkv"), filepath.Join(root, "d-y.mkv")); err != nil {
		t.Fatal(err)
	}
	src := createSource(t, st, "S", root)
	r := mustScan(t, sc, src.ID)
	if !r.FUSE || r.Groups != 2 {
		t.Fatalf("scan = %+v", r)
	}
	// Simulate unstable FUSE inode numbers: one name's content differs.
	sc.hashFile = func(root *os.Root, rel string, m Meta) (string, error) {
		if rel == "d-y.mkv" {
			return "different", nil
		}
		return headTailHash(root, rel, m)
	}
	r = mustScan(t, sc, src.ID)
	if r.Groups != 1 || r.WarningCount != 1 || !strings.Contains(r.Warnings[0], "not their content") {
		t.Fatalf("scan 2 = %+v", r)
	}
	// An I/O error while hashing is fatal like any other.
	sc.hashFile = func(*os.Root, string, Meta) (string, error) { return "", syscall.EIO }
	if _, err := sc.Scan(context.Background(), src.ID, nil); !errors.Is(err, ErrScanRefused) {
		t.Fatalf("err = %v", err)
	}
}

func TestScanSkipsDestinationAndConfigDirs(t *testing.T) {
	ctx := context.Background()
	st := newStore(t, StoreOptions{})
	root := tempDir(t)
	writeFiles(t, root, map[string]string{"a.mkv": "a", "backup/.bunkarr/destination.json": "{}", "backup/media/a.mkv": "a", "cfg/bunkarr.db": "x"})
	forbidden := []string{filepath.Join(root, "backup"), filepath.Join(root, "cfg"), filepath.Join(root, "no-such-dir")}
	sc := NewScanner(st, ScannerOptions{ForbiddenRoots: func(context.Context) ([]string, error) { return forbidden, nil }})
	src := createSource(t, st, "S", root)
	r := mustScan(t, sc, src.ID)
	if r.Files != 1 || r.Skipped[SkipOverlap] != 2 || r.WarningCount != 2 {
		t.Fatalf("scan = %+v", r)
	}
	// An alias of the root itself (e.g. a bind mount of the destination) refuses the scan.
	forbidden = []string{root}
	if _, err := sc.Scan(ctx, src.ID, nil); !errors.Is(err, ErrScanRefused) {
		t.Fatalf("err = %v", err)
	}
	sc2 := NewScanner(st, ScannerOptions{ForbiddenRoots: func(context.Context) ([]string, error) { return nil, errors.New("db down") }})
	if _, err := sc2.Scan(ctx, src.ID, nil); err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("err = %v", err)
	}
}

func TestScanLocking(t *testing.T) {
	ctx := context.Background()
	st := newStore(t, StoreOptions{})
	sc := NewScanner(st, ScannerOptions{})
	root := tempDir(t)
	writeFiles(t, root, map[string]string{"a.mkv": "a"})
	src := createSource(t, st, "S", root)

	if _, err := sc.ScanLocked(ctx, src.ID, nil); err == nil {
		t.Fatal("ScanLocked without the lock succeeded")
	}
	unlock, err := st.LockSource(ctx, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sc.ScanLocked(ctx, src.ID, nil); err != nil {
		t.Fatalf("ScanLocked: %v", err)
	}
	// Scan waits for the lock; a deadline ends the wait.
	tctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := sc.Scan(tctx, src.ID, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Scan while locked = %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := sc.Scan(ctx, src.ID, nil)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("Scan did not wait for the lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatalf("Scan after unlock: %v", err)
	}
	if _, err := sc.Scan(ctx, 999, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Scan of a missing source = %v", err)
	}
}
