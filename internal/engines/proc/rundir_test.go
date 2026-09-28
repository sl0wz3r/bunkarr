package proc

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mode(t *testing.T, p string) fs.FileMode {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func gone(p string) bool {
	_, err := os.Lstat(p)
	return errors.Is(err, fs.ErrNotExist)
}

// TestRunDirOnTmpfs: with a tmpfs, secret files go to <shm>/bunkarr-run/<job>-<random>/ (0700)
// as 0600 files, data files to <config>/run/<job>-<random>-data/, and Remove deletes both (S22).
func TestRunDirOnTmpfs(t *testing.T) {
	dirs, config, shm := testRunDirs(t, true)
	rd, err := dirs.New(42)
	if err != nil {
		t.Fatal(err)
	}
	if rd.Warning() != "" {
		t.Errorf("warning on tmpfs: %q", rd.Warning())
	}
	if filepath.Dir(rd.SecretDir()) != filepath.Join(shm, runSubdir) || !strings.HasPrefix(filepath.Base(rd.SecretDir()), "42-") {
		t.Errorf("secret dir %s", rd.SecretDir())
	}
	if filepath.Dir(rd.DataDir()) != filepath.Join(config, "run") || filepath.Base(rd.DataDir()) != filepath.Base(rd.SecretDir())+"-data" {
		t.Errorf("data dir %s", rd.DataDir())
	}
	for _, p := range []string{rd.SecretDir(), rd.DataDir(), filepath.Join(shm, runSubdir), filepath.Join(config, "run")} {
		if m := mode(t, p); m != 0o700 {
			t.Errorf("%s mode %o", p, m)
		}
	}
	if err := rd.WriteSecret("password", []byte("pw")); err != nil {
		t.Fatal(err)
	}
	if m := mode(t, rd.SecretPath("password")); m != 0o600 {
		t.Errorf("secret file mode %o", m)
	}
	if err := rd.WriteSecret("password", []byte("again")); err == nil {
		t.Error("a second write of the same secret file succeeded (O_EXCL)")
	}
	// A symlink planted where a secret file goes is never followed.
	target := filepath.Join(t.TempDir(), "target")
	if err := os.Symlink(target, rd.SecretPath("known_hosts")); err != nil {
		t.Fatal(err)
	}
	if err := rd.WriteSecret("known_hosts", []byte("x")); err == nil || !gone(target) {
		t.Errorf("WriteSecret through a symlink: err %v, target written %v", err, !gone(target))
	}
	if err := rd.WriteData("files", []byte("a\x00b\x00")); err != nil || mode(t, rd.DataPath("files")) != 0o600 {
		t.Fatalf("WriteData: %v", err)
	}
	if err := rd.Remove(); err != nil {
		t.Fatal(err)
	}
	if !gone(rd.SecretDir()) || !gone(rd.DataDir()) {
		t.Error("Remove left a directory")
	}
	if err := rd.Remove(); err != nil {
		t.Errorf("second Remove: %v", err)
	}
}

// TestRunDirFallback: without a tmpfs the secret directory is in <config>/run, with a warning.
func TestRunDirFallback(t *testing.T) {
	dirs, config, shm := testRunDirs(t, false)
	rd, err := dirs.New(7)
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Remove()
	if filepath.Dir(rd.SecretDir()) != filepath.Join(config, "run") {
		t.Errorf("secret dir %s", rd.SecretDir())
	}
	if !strings.Contains(rd.Warning(), "not a tmpfs") {
		t.Errorf("warning %q", rd.Warning())
	}
	if !gone(filepath.Join(shm, runSubdir)) {
		t.Error("created the shm run directory without a tmpfs")
	}
	if mode(t, rd.SecretDir()) != 0o700 {
		t.Error("secret dir mode")
	}
}

// TestRunDirsSweep: start-up removes every leftover in both places.
func TestRunDirsSweep(t *testing.T) {
	dirs, config, shm := testRunDirs(t, true)
	var left []*RunDir
	for _, id := range []int64{1, 2} {
		rd, err := dirs.New(id)
		if err != nil {
			t.Fatal(err)
		}
		if err := rd.WriteSecret("password", []byte("x")); err != nil {
			t.Fatal(err)
		}
		left = append(left, rd)
	}
	if err := os.WriteFile(filepath.Join(config, "run", "stray"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := dirs.Sweep(); err != nil {
		t.Fatal(err)
	}
	for _, parent := range []string{filepath.Join(config, "run"), filepath.Join(shm, runSubdir)} {
		entries, err := os.ReadDir(parent)
		if err != nil || len(entries) != 0 {
			t.Errorf("%s after sweep: %v %v", parent, entries, err)
		}
	}
	// Nothing to sweep is fine.
	empty := NewRunDirs(t.TempDir(), RunDirOptions{ShmDir: t.TempDir()})
	if err := empty.Sweep(); err != nil {
		t.Errorf("Sweep of nothing: %v", err)
	}
}

func TestRunFileNamesArePlain(t *testing.T) {
	dirs, _, _ := testRunDirs(t, true)
	rd, err := dirs.New(1)
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Remove()
	for _, name := range []string{"../password", "a/b", "", ".", ".."} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("SecretPath(%q) did not panic", name)
				}
			}()
			rd.SecretPath(name)
		}()
	}
}
