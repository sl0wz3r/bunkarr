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
	// listed overrides the size listings report for an object (an SFTP server that lists one
	// size and serves another).
	listed map[string]int64
}

func newObjectStore() *objectStore {
	return &objectStore{objects: map[string][]byte{}, listed: map[string]int64{},
		modTime: time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)}
}

// entry renders the lsjson line of the object at key, listed as rel.
func (s *objectStore) entry(key, rel string) string {
	size, ok := s.listed[key]
	if !ok {
		size = int64(len(s.objects[key]))
	}
	b, _ := json.Marshal(Object{Path: rel, Name: filepath.Base(rel), Size: size, ModTime: s.modTime})
	return string(b)
}

func (s *objectStore) install(t *testing.T, f *enginetest.FakeRunner) {
	positionals := func(args []string) []string {
		var pos []string
		for i := 1; i < len(args); i++ {
			switch {
			case strings.HasPrefix(args[i], "-") && slices.Contains([]string{"--exclude", "--files-from-raw", "--max-delete",
				"--transfers", "--checkers", "--stats", "--stats-log-level", "--count", "--size", "--max-transfer",
				"--cutoff-mode"}, args[i]):
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
	f.Handle(proc.Rclone, "copyto", func(c *enginetest.Call) enginetest.Script {
		// A download stops at its --max-transfer cap, reaching it exactly included (rclone 1.74,
		// --cutoff-mode hard: exit 8, the partial file removed).
		if v, ok := c.Flag("--max-transfer"); ok {
			limit, _ := strconv.ParseInt(strings.TrimSuffix(v, "B"), 10, 64)
			s.mu.Lock()
			n := int64(len(s.objects[positionals(c.Args)[0]]))
			s.mu.Unlock()
			if n >= limit {
				return enginetest.Script{Exit: 8, Stderr: []string{`{"time":"2026-09-28T20:46:52Z","level":"notice",` +
					`"msg":"Failed to copyto: max transfer limit reached as set by --max-transfer"}`}}
			}
		}
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
				if _, ok := s.objects[dir+"/"+rel]; ok && dir+"/"+rel != s.failStat {
					out = append(out, s.entry(dir+"/"+rel, rel))
				}
			}
			return enginetest.Script{Stdout: lsjsonOut(out...)}
		}
		for _, k := range slices.Sorted(maps.Keys(s.objects)) {
			if rel, ok := strings.CutPrefix(k, dir+"/"); ok {
				out = append(out, s.entry(k, rel))
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
		if v, ok := c.Flag("--count"); ok {
			n, _ := strconv.Atoi(v)
			b = b[:min(n, len(b))]
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

// TestVersionStoreBoundedReads: whoever can write the bucket (or a compromised provider)
// decides whether a version's file name is an object or a prefix, and how large it is. ReadFile
// and Fetch list the name first and never download more than it was listed with (plus the
// cap's margin), nor a prefix standing in for a file, nor a file larger than the read's limit.
func TestVersionStoreBoundedReads(t *testing.T) {
	d, f := newTestDriver(t)
	store := newObjectStore()
	store.install(t, f)
	dest, sec := jobS3(true)
	vs := NewVersionStore(connect(t, d, dest, sec), isTestVersionDir)
	ctx := context.Background()
	manifest := `{"jobId":12,"integrationId":3,"files":[{"name":"com.plexapp.plugins.library.db"}]}` + "\n"
	logical := ".bunkarr/plex/plex-3/20260924T120000Z-job12"
	ref, err := vs.Put(ctx, engines.PutVersion{Kind: engines.VersionPlexDB, LogicalPath: logical, JobID: 12, IntegrationID: 3,
		Dir: stage(t, map[string]string{"manifest.json": manifest, "SHA256SUMS": "ab  com.plexapp.plugins.library.db\n",
			"com.plexapp.plugins.library.db": "sqlite"})})
	if err != nil {
		t.Fatal(err)
	}
	key := func(name string) string { return "BKCRYPT:" + logical + "/" + name }
	downloads := func() int { return len(f.CallsOf(proc.Rclone, "copyto")) - 1 } // the upload of manifest.json

	// A small file is downloaded, capped at three times its listed size plus 1 MiB.
	if got, err := vs.ReadFile(ctx, ref, ManifestName, 1<<20); err != nil || string(got) != manifest {
		t.Fatalf("ReadFile = %q %v", got, err)
	}
	dst := t.TempDir()
	if err := vs.Fetch(ctx, ref, "com.plexapp.plugins.library.db", dst); err != nil {
		t.Fatal(err)
	}
	copytos := f.CallsOf(proc.Rclone, "copyto")
	for i, want := range []int{len(manifest), len("sqlite")} {
		c := copytos[1+i]
		if v, _ := c.Flag("--max-transfer"); v != strconv.Itoa(3*want+1<<20)+"B" {
			t.Errorf("download %d: --max-transfer %q, want 3 × %d + 1 MiB", i, v, want)
		}
		if v, _ := c.Flag("--cutoff-mode"); v != "hard" {
			t.Errorf("download %d: --cutoff-mode %q", i, v)
		}
	}

	// manifest.json replaced by a large object (with crypt: a copy of another object's
	// ciphertext): only its first limit bytes are read, nothing is downloaded.
	big := strings.Repeat(`{"line":"`+strings.Repeat("x", 90)+`"}`+"\n", 30000) // 3 MB
	store.objects[key(ManifestName)] = []byte(big)
	before := downloads()
	if got, err := vs.ReadFile(ctx, ref, ManifestName, 1<<20); err != nil || string(got) != big[:1<<20] {
		t.Fatalf("ReadFile of a large manifest.json = %d bytes, %v; want its first MiB", len(got), err)
	}
	if downloads() != before {
		t.Fatal("a manifest.json larger than the read's limit was downloaded")
	}

	// SHA256SUMS deleted and objects created under the prefix "SHA256SUMS/": not a file.
	delete(store.objects, key("SHA256SUMS"))
	store.objects[key("SHA256SUMS/a")] = []byte(big)
	store.objects[key("SHA256SUMS/b/c")] = []byte(big)
	if _, err := vs.ReadFile(ctx, ref, "SHA256SUMS", 1<<20); !errors.Is(err, fs.ErrNotExist) || !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("ReadFile of a prefix: %v", err)
	}
	if err := vs.Fetch(ctx, ref, "SHA256SUMS", dst); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Fetch of a prefix: %v", err)
	}
	if downloads() != before {
		t.Fatal("a prefix standing in for a file was downloaded")
	}

	// A server that lists 6 bytes and serves 3 MB: the download stops at its cap.
	store.objects[key("com.plexapp.plugins.library.db")] = []byte(big)
	store.listed[key("com.plexapp.plugins.library.db")] = 6
	if err := vs.Fetch(ctx, ref, "com.plexapp.plugins.library.db", t.TempDir()); !errors.Is(err, ErrMaxTransfer) {
		t.Fatalf("Fetch of an object larger than listed: %v", err)
	}
	if _, err := vs.ReadFile(ctx, ref, "com.plexapp.plugins.library.db", 1<<20); !errors.Is(err, ErrMaxTransfer) {
		t.Fatalf("ReadFile of an object larger than listed: %v", err)
	}
	// One listed larger than it is: the cat of its first limit bytes comes back short.
	store.objects[key(ManifestName)] = []byte(manifest)
	store.listed[key(ManifestName)] = 1 << 30
	if _, err := vs.ReadFile(ctx, ref, ManifestName, 1<<20); err == nil {
		t.Fatal("a short read of a file listed larger than the limit was accepted")
	}
}

func TestTransferCap(t *testing.T) {
	for size, want := range map[int64]int64{-1: 1 << 20, 0: 1 << 20, 10: 30 + 1<<20, 1 << 40: 3<<40 + 1<<20,
		MaxDownloadCap / 3: MaxDownloadCap, 1 << 62: MaxDownloadCap} {
		if got := transferCap(size); got != want {
			t.Errorf("transferCap(%d) = %d, want %d", size, got, want)
		}
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
