package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeProgram creates an executable file with mode perm.
func writeProgram(t *testing.T, path string, perm os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestResolveEngineBinaries: a binary path with a space or a quote, a relative path, a missing
// file, a directory, a file that is not executable or that Bunkarr's user can write makes the
// engine unavailable with the reason; a read-only program (also reached through a symlink) is
// usable (docs/design/phase4.md §4.4, §14.1 config).
func TestResolveEngineBinaries(t *testing.T) {
	dir := t.TempDir()
	good := writeProgram(t, filepath.Join(dir, "bin", "restic"), 0o555)
	link := filepath.Join(dir, "link-restic")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	spaced := writeProgram(t, filepath.Join(dir, "my bin", "restic"), 0o555)
	quoted := writeProgram(t, filepath.Join(dir, "q'bin", "restic"), 0o555)
	notExec := writeProgram(t, filepath.Join(dir, "noexec", "restic"), 0o444)
	writable := writeProgram(t, filepath.Join(dir, "writable", "restic"), 0o755)
	otherWritable := writeProgram(t, filepath.Join(dir, "otherw", "restic"), 0o555)
	if err := os.Chmod(otherWritable, 0o557); err != nil {
		t.Fatal(err)
	}
	resolvedGood, _ := filepath.EvalSymlinks(good)
	root := os.Getuid() == 0

	tests := []struct {
		name     string
		path     string
		wantPath string
		wantErr  string // substring; "" = usable
		skip     bool
	}{
		{"read-only program", good, resolvedGood, "", false},
		{"through a symlink", link, resolvedGood, "", false},
		{"space in the path", spaced, "", "whitespace", false},
		{"quote in the path", quoted, "", "quote", false},
		{"relative path", "bin/restic", "", "not an absolute path", false},
		{"missing file", filepath.Join(dir, "nope", "restic"), "", "not installed", false},
		{"a directory", filepath.Join(dir, "bin"), "", "not a regular file", false},
		{"not executable", notExec, "", "not executable", false},
		{"writable by our user", writable, "", "writable by Bunkarr's user", root},
		{"writable by anyone", otherWritable, "", "writable by Bunkarr's user", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.skip {
				t.Skip("root can write every file; the owner check does not apply")
			}
			restic, rclone := ResolveEngineBinaries(Env{ResticPath: tc.path, RclonePath: good})
			if tc.wantErr == "" {
				if !restic.Available() || restic.Err != "" || restic.Path != tc.wantPath {
					t.Fatalf("restic = %+v, want usable at %s", restic, tc.wantPath)
				}
			} else if restic.Available() || !strings.Contains(restic.Err, tc.wantErr) {
				t.Fatalf("restic = %+v, want unavailable with %q", restic, tc.wantErr)
			}
			if !rclone.Available() {
				t.Fatalf("rclone = %+v: one engine's problem made the other unavailable", rclone)
			}
		})
	}
}

// TestResolveEngineBinariesLookPath: without a configured path the program is looked up in PATH,
// and a missing one is "not installed".
func TestResolveEngineBinariesLookPath(t *testing.T) {
	dir := t.TempDir()
	writeProgram(t, filepath.Join(dir, "rclone"), 0o555)
	t.Setenv("PATH", dir)
	restic, rclone := ResolveEngineBinaries(Env{})
	if restic.Available() || restic.Err != "restic is not installed" {
		t.Errorf("restic = %+v", restic)
	}
	want, _ := filepath.EvalSymlinks(filepath.Join(dir, "rclone"))
	if !rclone.Available() || rclone.Path != want {
		t.Errorf("rclone = %+v, want %s", rclone, want)
	}
}

func TestEnvFromOSEnginePaths(t *testing.T) {
	t.Setenv("BUNKARR_RESTIC_PATH", "/opt/restic ")
	t.Setenv("BUNKARR_RCLONE_PATH", "/opt/rclone")
	e, err := EnvFromOS()
	if err != nil {
		t.Fatal(err)
	}
	if e.ResticPath != "/opt/restic " || e.RclonePath != "/opt/rclone" {
		t.Fatalf("paths %q %q (a trailing space must be kept so the check refuses it)", e.ResticPath, e.RclonePath)
	}
}

// TestKeyringDerive: Derive is deterministic per master key and info, and independent across
// infos and master keys.
func TestKeyringDerive(t *testing.T) {
	master := bytes.Repeat([]byte{7}, masterKeyLen)
	k1, err := NewKeyring(master)
	if err != nil {
		t.Fatal(err)
	}
	k2, _ := NewKeyring(bytes.Clone(master))
	other, _ := NewKeyring(bytes.Repeat([]byte{8}, masterKeyLen))
	a := k1.Derive("bunkarr rclone obscure v1", 32)
	if len(a) != 32 || !bytes.Equal(a, k2.Derive("bunkarr rclone obscure v1", 32)) {
		t.Fatal("Derive is not deterministic")
	}
	if bytes.Equal(a, k1.Derive("bunkarr other purpose", 32)) || bytes.Equal(a, other.Derive("bunkarr rclone obscure v1", 32)) {
		t.Fatal("Derive gave the same key for another info or master key")
	}
	master[0] = 9 // the keyring keeps its own copy
	if !bytes.Equal(a, k1.Derive("bunkarr rclone obscure v1", 32)) {
		t.Fatal("the keyring shares the caller's master key slice")
	}
}
