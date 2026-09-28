package enginetest

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestFixturesLoad: every file of testdata/restic and testdata/rclone is named by its index.json
// and loads with an exit code; the notes' exit codes parse.
func TestFixturesLoad(t *testing.T) {
	for _, engine := range []string{"restic", "rclone"} {
		names := FixtureNames(t, engine)
		entries, err := os.ReadDir(filepath.Join(TestdataDir(t), engine))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.Name() != "index.json" && !slices.Contains(names, e.Name()) {
				t.Errorf("%s/%s is not in index.json", engine, e.Name())
			}
		}
		for _, name := range names {
			f := Fixture(t, engine, name)
			if f.Exit < 0 || len(f.Raw) == 0 || len(f.Lines) == 0 || f.Command == "" {
				t.Errorf("%s/%s: exit %d, %d bytes, %d lines", engine, name, f.Exit, len(f.Raw), len(f.Lines))
			}
		}
	}
	for _, tc := range []struct {
		engine, name string
		exit         int
		stderr       bool
	}{
		{"restic", "backup-killed.jsonl", 137, false},
		{"restic", "s3-bad-credentials-retrying.stderr.txt", 1, true},
		{"restic", "wrong-password.stderr.jsonl", 12, true},
		{"rclone", "exit10-max-duration-soft.txt", 10, false},
		{"rclone", "sync-max-delete.jsonl", 7, false},
	} {
		f := Fixture(t, tc.engine, tc.name)
		s := FixtureScript(t, tc.engine, tc.name)
		if f.Exit != tc.exit || f.Stderr != tc.stderr || s.Exit != tc.exit || (len(s.Stderr) > 0) != tc.stderr {
			t.Errorf("%s/%s: exit %d stderr %v", tc.engine, tc.name, f.Exit, f.Stderr)
		}
	}
}
