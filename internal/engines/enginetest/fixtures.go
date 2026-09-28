package enginetest

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// FixtureFile is one file of the engine spike (testdata/restic, testdata/rclone) with what its
// index.json says about it.
type FixtureFile struct {
	Engine, Name string
	// Command is the command line that produced it (index.json).
	Command string
	// Exit is its exit code (index.json; for a note such as "1 after SIGTERM; …" the leading
	// number, -1 when there is none).
	Exit int
	// Stderr: the file holds stderr (its name contains ".stderr.").
	Stderr bool
	Raw    []byte
	// Lines are Raw's lines without line breaks (no trailing empty line).
	Lines []string
}

// fixtureIndex is the part of index.json FixtureFile reads.
type fixtureIndex struct {
	Fixtures map[string]struct {
		Cmd  string          `json:"cmd"`
		Exit json.RawMessage `json:"exit"`
	} `json:"fixtures"`
}

var (
	rootOnce sync.Once
	rootDir  string
	rootErr  error
)

// TestdataDir returns the repository's testdata directory (found from the working directory up
// to go.mod).
func TestdataDir(t testing.TB) string {
	t.Helper()
	rootOnce.Do(func() {
		dir, err := os.Getwd()
		if err != nil {
			rootErr = err
			return
		}
		for {
			if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
				rootDir = filepath.Join(dir, "testdata")
				return
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				rootErr = errors.New("go.mod not found above the working directory")
				return
			}
			dir = parent
		}
	})
	if rootErr != nil {
		t.Fatalf("enginetest: %v", rootErr)
	}
	return rootDir
}

func loadIndex(t testing.TB, engine string) fixtureIndex {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(TestdataDir(t), engine, "index.json"))
	if err != nil {
		t.Fatalf("enginetest: %v", err)
	}
	var idx fixtureIndex
	if err := json.Unmarshal(raw, &idx); err != nil {
		t.Fatalf("enginetest: %s/index.json: %v", engine, err)
	}
	return idx
}

var leadingInt = regexp.MustCompile(`^\s*(\d+)`)

// FixtureNames lists the fixtures index.json names for engine ("restic" or "rclone"), sorted.
func FixtureNames(t testing.TB, engine string) []string {
	t.Helper()
	var out []string
	for name := range loadIndex(t, engine).Fixtures {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// Fixture loads testdata/<engine>/<name> with its exit code from index.json. A file the index
// does not name fails the test.
func Fixture(t testing.TB, engine, name string) FixtureFile {
	t.Helper()
	entry, ok := loadIndex(t, engine).Fixtures[name]
	if !ok {
		t.Fatalf("enginetest: %s/index.json does not name %s", engine, name)
	}
	raw, err := os.ReadFile(filepath.Join(TestdataDir(t), engine, name))
	if errors.Is(err, fs.ErrNotExist) || err != nil {
		t.Fatalf("enginetest: fixture %s/%s: %v", engine, name, err)
	}
	f := FixtureFile{Engine: engine, Name: name, Command: entry.Cmd, Exit: -1, Stderr: strings.Contains(name, ".stderr."), Raw: raw}
	var n int
	var s string
	switch {
	case json.Unmarshal(entry.Exit, &n) == nil:
		f.Exit = n
	case json.Unmarshal(entry.Exit, &s) == nil:
		if m := leadingInt.FindStringSubmatch(s); m != nil {
			f.Exit, _ = strconv.Atoi(m[1])
		}
	}
	text := strings.TrimSuffix(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	if text != "" {
		f.Lines = strings.Split(text, "\n")
	}
	return f
}

// FixtureScript is a Script that prints the fixture (on stderr for a ".stderr." file) and exits
// with its code (0 when the index gives none).
func FixtureScript(t testing.TB, engine, name string) Script {
	t.Helper()
	f := Fixture(t, engine, name)
	s := Script{Exit: max(f.Exit, 0)}
	if f.Stderr {
		s.Stderr = f.Lines
	} else {
		s.Stdout = f.Lines
	}
	return s
}
