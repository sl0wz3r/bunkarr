package rclone

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/enginetest"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
)

// objectStore is a fake rclone remote for the version store: objects by remote path
// ("BKCRYPT:.bunkarr/plex/p-1/<version>/manifest.json"), served to copy, copyto, lsjson, cat and
// purge.
type objectStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	modTime time.Time
	// failStat drops this object from listings (a version that did not arrive).
	failStat string
}

func newObjectStore() *objectStore {
	return &objectStore{objects: map[string][]byte{}, modTime: time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)}
}

func (s *objectStore) entry(rel string, size int) string {
	b, _ := json.Marshal(Object{Path: rel, Name: filepath.Base(rel), Size: int64(size), ModTime: s.modTime})
	return string(b)
}

func (s *objectStore) install(t *testing.T, f *enginetest.FakeRunner) {
	positionals := func(args []string) []string {
		var pos []string
		for i := 1; i < len(args); i++ {
			switch {
			case strings.HasPrefix(args[i], "-") && slices.Contains([]string{"--exclude", "--files-from-raw", "--max-delete",
				"--transfers", "--checkers", "--stats", "--stats-log-level", "--count", "--size"}, args[i]):
				i++
			case strings.HasPrefix(args[i], "-"):
			default:
				pos = append(pos, args[i])
			}
		}
		return pos
	}
	f.Handle(proc.Rclone, "copy", func(*enginetest.Call) enginetest.Script {
		return enginetest.Script{Hook: func(c *enginetest.Call) {
			pos := positionals(c.Args)
			exclude, _ := c.Flag("--exclude")
			_ = filepath.WalkDir(pos[0], func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				rel, _ := filepath.Rel(pos[0], p)
				if "/"+rel == exclude {
					return nil
				}
				b, _ := os.ReadFile(p)
				s.mu.Lock()
				s.objects[pos[1]+"/"+rel] = b
				s.mu.Unlock()
				return nil
			})
		}}
	})
	f.Handle(proc.Rclone, "copyto", func(*enginetest.Call) enginetest.Script {
		return enginetest.Script{Hook: func(c *enginetest.Call) {
			pos := positionals(c.Args)
			s.mu.Lock()
			defer s.mu.Unlock()
			if strings.HasPrefix(pos[0], "/") {
				b, _ := os.ReadFile(pos[0])
				s.objects[pos[1]] = b
				return
			}
			if err := os.WriteFile(pos[1], s.objects[pos[0]], 0o644); err != nil {
				t.Error(err)
			}
		}}
	})
	f.Handle(proc.Rclone, "lsjson", func(c *enginetest.Call) enginetest.Script {
		s.mu.Lock()
		defer s.mu.Unlock()
		dir := positionals(c.Args)[0]
		var out []string
		if c.Has("--files-from-raw") {
			for _, rel := range strings.Split(strings.TrimSuffix(string(c.DataFile("files")), "\n"), "\n") {
				if b, ok := s.objects[dir+"/"+rel]; ok && dir+"/"+rel != s.failStat {
					out = append(out, s.entry(rel, len(b)))
				}
			}
			return enginetest.Script{Stdout: lsjsonOut(out...)}
		}
		for _, k := range slices.Sorted(maps.Keys(s.objects)) {
			if rel, ok := strings.CutPrefix(k, dir+"/"); ok {
				out = append(out, s.entry(rel, len(s.objects[k])))
			}
		}
		if len(out) == 0 {
			return enginetest.Script{Exit: 3}
		}
		return enginetest.Script{Stdout: lsjsonOut(out...)}
	})
	f.Handle(proc.Rclone, "cat", func(c *enginetest.Call) enginetest.Script {
		s.mu.Lock()
		defer s.mu.Unlock()
		b, ok := s.objects[positionals(c.Args)[0]]
		if !ok {
			return enginetest.Script{Exit: 3}
		}
		return enginetest.Script{Stdout: strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")}
	})
	f.Handle(proc.Rclone, "purge", func(c *enginetest.Call) enginetest.Script {
		s.mu.Lock()
		defer s.mu.Unlock()
		dir := positionals(c.Args)[0]
		limit, _ := c.Flag("--max-delete")
		n, _ := strconv.Atoi(limit)
		var gone []string
		for k := range s.objects {
			if strings.HasPrefix(k, dir+"/") {
				gone = append(gone, k)
			}
		}
		if len(gone) > n {
			return enginetest.Script{Exit: 7, Stderr: []string{`{"time":"2026-09-27T13:53:10Z","level":"error","msg":"--max-delete threshold reached","object":"x"}`}}
		}
		for _, k := range gone {
			delete(s.objects, k)
		}
		return enginetest.Script{}
	})
}

var testVersionDir = regexp.MustCompile(`^\.bunkarr/(plex|arr)/[a-z0-9-]+/\d{8}T\d{6}Z(-job\d+)?$|^\.bunkarr/manifests/\d{8}T\d{6}Z(-job\d+)?$`)

func isTestVersionDir(rel string) bool { return testVersionDir.MatchString(rel) }

func stage(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestVersionStore(t *testing.T) {
	d, f := newTestDriver(t)
	store := newObjectStore()
	store.install(t, f)
	dest, sec := jobS3(true)
	vs := NewVersionStore(connect(t, d, dest, sec), isTestVersionDir)
	ctx := context.Background()

	manifest := `{"jobId":12,"integrationId":3,"files":[{"name":"com.plexapp.plugins.library.db"}]}` + "\n"
	logical := ".bunkarr/plex/plex-3/20260924T120000Z-job12"
	ref, err := vs.Put(ctx, engines.PutVersion{Kind: engines.VersionPlexDB, LogicalPath: logical, JobID: 12, IntegrationID: 3,
		Dir: stage(t, map[string]string{"manifest.json": manifest, "com.plexapp.plugins.library.db": "sqlite\x00bytes\r\n", "Preferences.xml": "<x/>"})})
	if err != nil {
		t.Fatal(err)
	}
	if ref != engines.Ref(logical) {
		t.Fatalf("ref %q", ref)
	}
	copies := f.CallsOf(proc.Rclone, "copy")
	copytos := f.CallsOf(proc.Rclone, "copyto")
	if len(copies) != 1 || len(copytos) != 1 || !copies[0].Has("--exclude") || copies[0].Has("--backup-dir") {
		t.Fatalf("copy %v, copyto %v", copies, copytos)
	}
	if v, _ := copies[0].Flag("--exclude"); v != "/manifest.json" {
		t.Fatalf("--exclude %q", v)
	}
	if !strings.HasSuffix(copytos[0].Args[2], logical+"/manifest.json") {
		t.Fatalf("manifest.json is not written last to its path: %v", copytos[0].Args)
	}
	// A leftover without manifest.json, and a manifest version.
	store.objects["BKCRYPT:.bunkarr/plex/plex-3/20260925T120000Z-job13/com.plexapp.plugins.library.db"] = []byte("partial")
	store.objects["BKCRYPT:.bunkarr/plex/plex-3/notes.txt"] = []byte("not a version")
	list, err := vs.List(ctx, ".bunkarr/plex")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("list %+v", list)
	}
	v := list[0]
	if v.LogicalPath != logical || !v.Complete || v.JobID != 12 || v.IntegrationID != 3 || v.Version != "20260924T120000Z-job12" ||
		!v.Time.Equal(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)) || len(v.Files) != 3 || v.Files["Preferences.xml"] != 4 {
		t.Fatalf("version %+v", v)
	}
	if l := list[1]; l.Complete || l.JobID != 0 || l.Files["com.plexapp.plugins.library.db"] != 7 {
		t.Fatalf("leftover %+v", l)
	}
	if list, err := vs.List(ctx, ".bunkarr/arr"); err != nil || len(list) != 0 {
		t.Fatalf("empty kind folder: %v %v", list, err)
	}

	got, err := vs.ReadFile(ctx, ref, "manifest.json", 1<<20)
	if err != nil || string(got) != manifest {
		t.Fatalf("ReadFile = %q %v", got, err)
	}
	if got, err := vs.ReadFile(ctx, ref, "manifest.json", 5); err != nil || string(got) != manifest[:5] {
		t.Fatalf("ReadFile limit = %q %v", got, err)
	}
	dst := t.TempDir()
	if err := vs.Fetch(ctx, ref, "com.plexapp.plugins.library.db", dst); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dst, "com.plexapp.plugins.library.db"))
	fi, _ := os.Stat(filepath.Join(dst, "com.plexapp.plugins.library.db"))
	if err != nil || string(b) != "sqlite\x00bytes\r\n" || fi.Mode().Perm() != 0o600 {
		t.Fatalf("Fetch = %q %v %v", b, err, fi.Mode())
	}
	if _, err := vs.ReadFile(ctx, "Movies", "a", 10); !errors.Is(err, ErrFence) {
		t.Fatalf("ReadFile outside a version: %v", err)
	}

	if err := vs.Remove(ctx, nil, ref, engines.VersionPlexDB); err != nil {
		t.Fatal(err)
	}
	purges := f.CallsOf(proc.Rclone, "purge")
	if len(purges) != 1 || purges[0].Args[1] != "BKCRYPT:"+logical {
		t.Fatalf("purge %v", purges)
	}
	if v, _ := purges[0].Flag("--max-delete"); v != "4" {
		t.Fatalf("--max-delete %s, want the 3 files + 1", v)
	}
	for k := range store.objects {
		if strings.HasPrefix(k, "BKCRYPT:"+logical+"/") {
			t.Fatalf("%s left after Remove", k)
		}
	}
	if err := vs.Remove(ctx, nil, ".bunkarr/retention/20260927T010000Z-job7", engines.VersionPlexDB); !errors.Is(err, ErrFence) {
		t.Fatalf("Remove of a retention directory: %v", err)
	}
	if err := vs.Remove(ctx, nil, "Movies", engines.VersionPlexDB); !errors.Is(err, ErrFence) {
		t.Fatalf("Remove of a destFolder: %v", err)
	}
}

func TestVersionStorePutFailures(t *testing.T) {
	d, f := newTestDriver(t)
	store := newObjectStore()
	store.install(t, f)
	dest, sec := jobS3(false)
	vs := NewVersionStore(connect(t, d, dest, sec), isTestVersionDir)
	ctx := context.Background()
	logical := ".bunkarr/manifests/20260924T120000Z-job12"
	if _, err := vs.Put(ctx, engines.PutVersion{Kind: engines.VersionManifest, LogicalPath: logical,
		Dir: stage(t, map[string]string{"manifest.csv": "a"})}); err == nil || !strings.Contains(err.Error(), "manifest.json") {
		t.Fatalf("a version without manifest.json: %v", err)
	}
	if _, err := vs.Put(ctx, engines.PutVersion{Kind: engines.VersionManifest, LogicalPath: "Movies/x",
		Dir: stage(t, map[string]string{"manifest.json": "{}"})}); !errors.Is(err, ErrFence) {
		t.Fatalf("a version outside .bunkarr: %v", err)
	}
	store.failStat = "BKDEST:media/bk/" + logical + "/SHA256SUMS"
	if _, err := vs.Put(ctx, engines.PutVersion{Kind: engines.VersionManifest, LogicalPath: logical,
		Dir: stage(t, map[string]string{"manifest.json": `{"job":{"id":12}}`, "SHA256SUMS": "x  manifest.json\n"})}); err == nil ||
		!strings.Contains(err.Error(), "SHA256SUMS is missing") {
		t.Fatalf("a file that did not arrive: %v", err)
	}
	// manifest.json alone: no copy, only copyto.
	before := len(f.CallsOf(proc.Rclone, "copy"))
	store.failStat = ""
	if _, err := vs.Put(ctx, engines.PutVersion{Kind: engines.VersionManifest, LogicalPath: logical,
		Dir: stage(t, map[string]string{"manifest.json": `{"job":{"id":12}}`})}); err != nil {
		t.Fatal(err)
	}
	if len(f.CallsOf(proc.Rclone, "copy")) != before {
		t.Fatal("a copy ran for manifest.json alone")
	}
	list, err := vs.List(ctx, ".bunkarr/manifests")
	if err != nil || len(list) != 1 || list[0].JobID != 12 || !list[0].Complete {
		t.Fatalf("list %+v %v", list, err)
	}
}

func TestVersionIDs(t *testing.T) {
	big := `{"format":"bunkarr-manifest","job":{"id":44,"queuedAt":"2026-09-27T00:00:00Z"},"items":[` +
		strings.Repeat(`{"path":"x"},`, 10000)
	for _, tc := range []struct {
		head        string
		job, integr int64
	}{
		{`{"jobId":12,"integrationId":3}`, 12, 3},
		{`{"integrationId":3,"other":{"a":[1,2]},"jobId":7}`, 7, 3},
		{big[:versionIDLimit], 44, 0},
		{`not json`, 0, 0},
		{``, 0, 0},
	} {
		j, i := versionIDs([]byte(tc.head))
		if j != tc.job || i != tc.integr {
			t.Errorf("versionIDs(%.40q) = %d %d, want %d %d", tc.head, j, i, tc.job, tc.integr)
		}
	}
}
