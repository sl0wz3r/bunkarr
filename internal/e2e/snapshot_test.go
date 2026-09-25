//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

// TestSnapshotSeesMetadataChanges proves that the S1 source snapshot catches what S1 bans even
// when a change is put back: a chmod, utimes or chown leaves the inode change time (ctime) moved
// although mode and mtime can be restored, and a chown shows in the owner.
func TestSnapshotSeesMetadataChanges(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(t *testing.T, dir, file string)
	}{
		{"chmod and back", func(t *testing.T, dir, file string) {
			for _, m := range []os.FileMode{0o600, 0o644} {
				if err := os.Chmod(file, m); err != nil {
					t.Fatal(err)
				}
			}
		}},
		{"directory chmod and back", func(t *testing.T, dir, file string) {
			for _, m := range []os.FileMode{0o700, 0o755} {
				if err := os.Chmod(dir, m); err != nil {
					t.Fatal(err)
				}
			}
		}},
		{"utimes restoring mtime", func(t *testing.T, dir, file string) {
			fi, err := os.Stat(file)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(file, time.Unix(1, 0), fi.ModTime()); err != nil {
				t.Fatal(err)
			}
		}},
		{"chgrp", func(t *testing.T, dir, file string) {
			if err := os.Lchown(file, -1, otherGroup(t, file)); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, "Movies/a.mkv", content("a", 1, 1000))
			// Past the coarse clock tick of the creation, so the mutation moves ctime.
			time.Sleep(50 * time.Millisecond)
			before := snapshot(t, dir)
			m.mutate(t, filepath.Join(dir, "Movies"), filepath.Join(dir, "Movies", "a.mkv"))
			if d := diffSnapshots(before, snapshot(t, dir)); d == "" {
				t.Fatalf("the snapshot did not see a %s", m.name)
			}
		})
	}
}

// otherGroup returns a group id other than file's that the test process can chown it to, or
// skips the test.
func otherGroup(t *testing.T, file string) int {
	t.Helper()
	fi, err := os.Lstat(file)
	if err != nil {
		t.Fatal(err)
	}
	gid := int(fi.Sys().(*syscall.Stat_t).Gid)
	if os.Geteuid() == 0 {
		return gid + 1
	}
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(groups, func(g int) bool { return g != gid }); i >= 0 {
		return groups[i]
	}
	t.Skip("the test user is in only one group")
	return 0
}
