package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTestSource(t *testing.T) {
	base := tempDir(t)
	full := filepath.Join(base, "full")
	writeFiles(t, full, map[string]string{"a.mkv": "a", "b/c.mkv": "c"})
	empty := filepath.Join(base, "empty")
	if err := os.Mkdir(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(full, link); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		path    string
		ok      bool
		exists  bool
		isDir   bool
		entries int
		message string // substring
		warning string // substring of some warning
	}{
		{"ok", full, true, true, true, 2, "ok", ""},
		{"relative", "media", false, false, false, 0, "absolute", ""},
		{"missing", filepath.Join(base, "nope"), false, false, false, 0, "does not exist", ""},
		{"file", filepath.Join(full, "a.mkv"), false, true, false, 0, "not a directory", ""},
		{"root", "/", false, true, true, 0, "filesystem root", ""},
		{"empty", empty, true, true, true, 0, "ok", "empty"},
		{"symlink", link, true, true, true, 2, "ok", "symlink"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := TestSource(tc.path)
			if res.OK != tc.ok || res.Exists != tc.exists || res.IsDir != tc.isDir || res.Entries != tc.entries ||
				!strings.Contains(res.Message, tc.message) || res.Warnings == nil {
				t.Fatalf("TestSource(%s) = %+v", tc.path, res)
			}
			if res.OK && res.FSType == "" {
				t.Fatalf("no fsType: %+v", res)
			}
			if tc.warning != "" && !strings.Contains(strings.Join(res.Warnings, "\n"), tc.warning) {
				t.Fatalf("warnings %v lack %q", res.Warnings, tc.warning)
			}
		})
	}
	if res := TestSource(link); res.Path != full {
		t.Fatalf("resolved path = %q, want %q", res.Path, full)
	}

	if os.Geteuid() != 0 {
		locked := filepath.Join(base, "locked")
		if err := os.Mkdir(locked, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
		if res := TestSource(locked); res.OK || !strings.Contains(res.Message, "permission denied") {
			t.Fatalf("unreadable = %+v", res)
		}
	}
}
