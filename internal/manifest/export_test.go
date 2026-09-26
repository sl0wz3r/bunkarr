package manifest

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
)

// readStaged reads a staged file and checks its size and digest.
func readStaged(t *testing.T, f *StagedFile) []byte {
	t.Helper()
	r, err := f.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(data)) != f.Size || SHA256Hex(data) != f.SHA256 {
		t.Fatalf("staged %d bytes %s, file says %d %s", len(data), SHA256Hex(data), f.Size, f.SHA256)
	}
	return data
}

// stagingEntries lists the staging directory.
func (e *testEnv) stagingEntries() []string {
	e.t.Helper()
	es, err := os.ReadDir(filepath.Join(e.config, stagingDirName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, x := range es {
		out = append(out, x.Name())
	}
	return out
}

func TestExport(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		dest   int64
		format string
		name   string
	}{
		{0, FormatJSON, "bunkarr-manifest-all-20260925T120000Z.json"},
		{0, FormatCSV, "bunkarr-manifest-all-20260925T120000Z.csv"},
		{e.dest.ID, FormatJSON, "bunkarr-manifest-unas-20260925T120000Z.json"},
	} {
		f, err := e.runner.Export(e.ctx, c.dest, c.format)
		if err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
		data := readStaged(t, f)
		if f.Name != c.name {
			t.Fatalf("name %q, want %q", f.Name, c.name)
		}
		if info, err := os.Stat(filepath.Dir(f.path)); err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("staging directory %v, %v", info, err)
		}
		if c.format == FormatJSON {
			m, err := Parse(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			wantKind := ScopeExport
			if c.dest != 0 {
				wantKind = ScopeDestination
			}
			if m.Scope.Kind != wantKind || m.Job != nil || m.Summary.Items != 7 || f.ContentType != "application/json" {
				t.Fatalf("export %+v, %s", m.Scope, f.ContentType)
			}
		} else if !strings.HasPrefix(string(data), strings.Join(CSVHeader, ",")) || f.ContentType != "text/csv; charset=utf-8" {
			t.Fatalf("csv %q", data[:40])
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		_ = f.Close() // twice is fine
		if es := e.stagingEntries(); len(es) != 0 {
			t.Fatalf("staging left: %v", es)
		}
	}
	if _, err := e.runner.Export(e.ctx, 0, "xml"); err == nil {
		t.Fatal("an unknown format was accepted")
	}
	if _, err := e.runner.Export(e.ctx, 999, FormatJSON); !errors.Is(err, destinations.ErrNotFound) {
		t.Fatalf("unknown destination: %v", err)
	}
	// A failed export frees its slot.
	for range DefaultMaxExports + 1 {
		if _, err := e.runner.Export(e.ctx, 999, FormatJSON); errors.Is(err, ErrBusy) {
			t.Fatal("failed exports kept their slots")
		}
	}
}

func TestExportLimit(t *testing.T) {
	e := newEnv(t)
	var open []*StagedFile
	for range DefaultMaxExports {
		f, err := e.runner.Export(e.ctx, 0, FormatJSON)
		if err != nil {
			t.Fatal(err)
		}
		open = append(open, f)
	}
	if _, err := e.runner.Export(e.ctx, 0, FormatJSON); !errors.Is(err, ErrBusy) {
		t.Fatalf("a third export: %v", err)
	}
	_ = open[0].Close()
	f, err := e.runner.Export(e.ctx, 0, FormatCSV)
	if err != nil {
		t.Fatalf("after a slot was freed: %v", err)
	}
	_ = f.Close()
	_ = open[1].Close()
}

func TestExportFaultMidBuild(t *testing.T) {
	// A build that fails half-way stages nothing and frees its slot (the API answers 500 before
	// any byte).
	e := newEnv(t)
	faultinject.SetHook(faultinject.CrashAt(PointBuildItem, 3))
	func() {
		defer faultinject.SetHook(nil)
		defer func() {
			if _, ok := recover().(faultinject.Crash); !ok {
				t.Fatal("the build did not crash")
			}
		}()
		_, _ = e.runner.Export(e.ctx, 0, FormatJSON)
	}()
	if es := e.stagingEntries(); len(es) != 0 {
		t.Fatalf("staging after a failed build: %v", es)
	}
	for range DefaultMaxExports {
		f, err := e.runner.Export(e.ctx, 0, FormatJSON)
		if err != nil {
			t.Fatalf("the crashed export kept its slot: %v", err)
		}
		defer f.Close()
	}
}

func TestExportCleansStaleStaging(t *testing.T) {
	e := newEnv(t)
	base := filepath.Join(e.config, stagingDirName)
	stale := filepath.Join(base, exportPrefix+"old")
	fresh := filepath.Join(base, downloadPrefix+"new")
	other := filepath.Join(base, "plexdb-job3")
	for _, d := range []string{stale, fresh, other} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := e.clock.Now().Add(-2 * time.Hour)
	for _, d := range []string{stale, other} {
		if err := os.Chtimes(d, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(fresh, e.clock.Now(), e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	f, err := e.runner.Export(e.ctx, 0, FormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	for d, want := range map[string]bool{stale: false, fresh: true, other: true} {
		if _, err := os.Stat(d); (err == nil) != want {
			t.Errorf("%s exists: %v, want %v", filepath.Base(d), err == nil, want)
		}
	}
}

func TestDownload(t *testing.T) {
	e := newEnv(t)
	e.runOK()
	v := e.versions()[0]
	for _, format := range []string{FormatJSON, FormatCSV} {
		f, err := e.runner.Download(e.ctx, v.ID, format)
		if err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		data := readStaged(t, f)
		name := JSONName
		if format == FormatCSV {
			name = CSVName
		}
		disk, _ := os.ReadFile(filepath.Join(e.versionDir(v), name))
		if !bytes.Equal(data, disk) || f.Name != "bunkarr-manifest-unas-20260925T120000Z."+format {
			t.Fatalf("%s: %q, %d bytes (disk %d)", format, f.Name, len(data), len(disk))
		}
		if format == FormatJSON && Checksum(f.SHA256) != v.Checksum {
			t.Fatalf("digest %s, recorded %s", f.SHA256, v.Checksum)
		}
		_ = f.Close()
	}
	if _, err := e.runner.Download(e.ctx, 999, FormatJSON); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	if _, err := e.runner.Download(e.ctx, v.ID, "xml"); err == nil {
		t.Fatal("an unknown format was accepted")
	}
}

func TestDownloadDamaged(t *testing.T) {
	for name, damage := range map[string]func(dir string) error{
		"json": func(dir string) error { return os.WriteFile(filepath.Join(dir, JSONName), []byte("{}"), 0o644) },
		"sums": func(dir string) error {
			return os.WriteFile(filepath.Join(dir, SumsName), FormatSums(strings.Repeat("0", 64), strings.Repeat("0", 64)), 0o644)
		},
		"csv only": func(dir string) error { return os.WriteFile(filepath.Join(dir, CSVName), []byte("x"), 0o644) },
		"missing":  os.RemoveAll,
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.runOK()
			v := e.versions()[0]
			if err := damage(e.versionDir(v)); err != nil {
				t.Fatal(err)
			}
			format := FormatJSON
			if name == "csv only" {
				// The JSON still verifies; the CSV does not.
				f, err := e.runner.Download(e.ctx, v.ID, FormatJSON)
				if err != nil {
					t.Fatalf("json of a version with a damaged csv: %v", err)
				}
				_ = f.Close()
				format = FormatCSV
			}
			if _, err := e.runner.Download(e.ctx, v.ID, format); !errors.Is(err, ErrDamaged) {
				t.Fatalf("download: %v", err)
			}
			if got := e.versions()[0]; got.Integrity != IntegrityDamaged {
				t.Fatalf("version %+v", got)
			}
			if es := e.stagingEntries(); len(es) != 0 {
				t.Fatalf("staging left: %v", es)
			}
			// Marked damaged: refused without reading.
			if _, err := e.runner.Download(e.ctx, v.ID, FormatJSON); !errors.Is(err, ErrDamaged) {
				t.Fatalf("second download: %v", err)
			}
		})
	}
}

func TestDownloadNotMounted(t *testing.T) {
	e := newEnv(t)
	e.runOK()
	v := e.versions()[0]
	if err := os.Remove(filepath.Join(e.target, filepath.FromSlash(filecopy.MarkerRel))); err != nil {
		t.Fatal(err)
	}
	if _, err := e.runner.Download(e.ctx, v.ID, FormatJSON); !errors.Is(err, destinations.ErrNotMounted) {
		t.Fatalf("download: %v", err)
	}
	if e.versions()[0].Integrity != IntegrityOK {
		t.Fatal("an unmounted destination marked the version damaged")
	}
}

func TestStore(t *testing.T) {
	e := newEnv(t)
	s := e.runner.Store()
	job := e.newJob(false)
	at := e.clock.Now()
	a, err := s.Insert(e.ctx, Version{DestinationID: e.dest.ID, JobID: job.ID, CreatedAt: at, Path: Root + "/a", ItemCount: 1, FileCount: 2,
		Bytes: 3, Checksum: "sha256:a", ContentHash: "sha256:x", Integrity: IntegrityOK})
	if err != nil || a.ID == 0 || a.Format != 1 {
		t.Fatalf("insert %+v, %v", a, err)
	}
	b, _ := s.Insert(e.ctx, Version{DestinationID: e.dest.ID, CreatedAt: at.Add(time.Hour), Path: Root + "/b", Checksum: "sha256:b",
		ContentHash: "sha256:y", Integrity: IntegrityOK})
	for _, bad := range []Version{
		{DestinationID: e.dest.ID, Checksum: "c", ContentHash: "h", Integrity: IntegrityOK},
		{DestinationID: e.dest.ID, Path: "p", Integrity: IntegrityOK},
		{DestinationID: e.dest.ID, Path: "p", Checksum: "c", ContentHash: "h", Integrity: "fine"},
		{DestinationID: e.dest.ID, Path: Root + "/a", Checksum: "c", ContentHash: "h", Integrity: IntegrityOK}, // duplicate path
	} {
		if _, err := s.Insert(e.ctx, bad); err == nil {
			t.Fatalf("inserted %+v", bad)
		}
	}
	list, _ := s.List(e.ctx, e.dest.ID)
	if len(list) != 2 || list[0].ID != b.ID || list[1].JobID != job.ID || list[0].JobID != 0 {
		t.Fatalf("list %+v", list)
	}
	if n, ok, _ := s.NewestOK(e.ctx, e.dest.ID); !ok || n.ID != b.ID || n.ContentHash != "sha256:y" {
		t.Fatalf("newest %+v", n)
	}
	if err := s.MarkDamaged(e.ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if n, ok, _ := s.NewestOK(e.ctx, e.dest.ID); !ok || n.ID != a.ID {
		t.Fatalf("newest ok after marking %+v", n)
	}
	if got, _ := s.Get(e.ctx, b.ID); got.Integrity != IntegrityDamaged {
		t.Fatalf("get %+v", got)
	}
	if rec, _ := s.Recorded(e.ctx, e.dest.ID, Root+"/a"); !rec {
		t.Fatal("not recorded")
	}
	if err := s.Remove(e.ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(e.ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(e.ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get removed: %v", err)
	}
	if _, ok, _ := s.NewestOK(e.ctx, e.dest.ID); ok {
		t.Fatal("a damaged version counted as ok")
	}
}
