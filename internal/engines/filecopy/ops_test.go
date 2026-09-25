package filecopy

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestLink(t *testing.T) {
	e := newEnv(t)
	writeFile(t, filepath.Join(e.dstDir, "Movies/a.mkv"), []byte("content"), srcMtime)
	if err := os.Mkdir(filepath.Join(e.dstDir, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Link(e.dst, "Movies/a.mkv", "Other/b.mkv"); err != nil {
		t.Fatalf("Link: %v", err)
	}
	a, _ := Lstat(e.dst, "Movies/a.mkv")
	b, _ := Lstat(e.dst, "Other/b.mkv")
	if a.Ino != b.Ino || a.Dev != b.Dev || a.Nlink != 2 {
		t.Errorf("not a hardlink: %+v / %+v", a, b)
	}
	if err := Link(e.dst, "Movies/a.mkv", "Other/b.mkv"); !errors.Is(err, ErrExists) {
		t.Errorf("link over an existing name: err = %v, want ErrExists", err)
	}
	if err := Link(e.dst, "dir", "dir2"); !errors.Is(err, ErrNotRegular) {
		t.Errorf("link of a directory: err = %v, want ErrNotRegular", err)
	}
	if err := Link(e.dst, "Movies/a.mkv", MarkerRel); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("link onto the marker: err = %v, want ErrInvalidPath", err)
	}
}

func TestMove(t *testing.T) {
	e := newEnv(t)
	writeFile(t, filepath.Join(e.dstDir, "Movies/a.mkv"), []byte("a"), srcMtime)
	writeFile(t, filepath.Join(e.dstDir, "Movies/b.mkv"), []byte("b"), srcMtime)
	if err := os.Mkdir(filepath.Join(e.dstDir, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Move(e.dst, "Movies/a.mkv", "Movies/b.mkv", true); !errors.Is(err, ErrExists) {
		t.Fatalf("move onto an existing file: err = %v, want ErrExists", err)
	}
	if got, _ := os.ReadFile(filepath.Join(e.dstDir, "Movies/b.mkv")); string(got) != "b" {
		t.Fatalf("existing file changed: %q", got)
	}
	if err := Move(e.dst, "Movies/a.mkv", "Renamed/Sub/a2.mkv", true); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(e.dstDir, "Renamed/Sub/a2.mkv")); string(got) != "a" {
		t.Errorf("moved content = %q", got)
	}
	if st, _ := Lstat(e.dst, "Renamed/Sub/a2.mkv"); st.MtimeNs != srcMtime.UnixNano() {
		t.Errorf("move changed the mtime")
	}
	if err := Move(e.dst, "dir", "dir2", true); !errors.Is(err, ErrNotRegular) {
		t.Errorf("move of a directory: err = %v, want ErrNotRegular", err)
	}
	if err := Move(e.dst, "Movies/missing", "x", true); !errors.Is(err, fs.ErrNotExist) || Classify(err) != Item {
		t.Errorf("move of a missing file: err = %v (%v)", err, Classify(err))
	}
	if err := Move(e.dst, "Movies/b.mkv", "Movies/a.mkv", false); err != nil {
		t.Errorf("replacing move: %v", err)
	}
}

func TestRenameDir(t *testing.T) {
	e := newEnv(t)
	partial := ".bunkarr/plex/main/.partial-job9"
	writeFile(t, filepath.Join(e.dstDir, partial, "manifest.json"), []byte("{}"), srcMtime)
	if err := os.MkdirAll(filepath.Join(e.dstDir, ".bunkarr/plex/main/20260924T060000Z"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(e.dstDir, "file"), []byte("x"), srcMtime)

	if err := RenameDir(e.dst, partial, ".bunkarr/plex/main/20260924T060000Z"); !errors.Is(err, ErrExists) {
		t.Fatalf("rename onto an existing empty directory: err = %v, want ErrExists", err)
	}
	if err := RenameDir(e.dst, "file", "file2"); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("RenameDir of a file: err = %v, want ErrInvalidPath", err)
	}
	if err := RenameDir(e.dst, partial, partial+"/inner"); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("RenameDir into itself: err = %v, want ErrInvalidPath", err)
	}
	if err := RenameDir(e.dst, MetaDir, "elsewhere"); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("RenameDir of %s: err = %v, want ErrInvalidPath", MetaDir, err)
	}
	if err := RenameDir(e.dst, partial, ".bunkarr/plex/main/20260925T060000Z"); err != nil {
		t.Fatalf("RenameDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.dstDir, ".bunkarr/plex/main/20260925T060000Z/manifest.json")); err != nil {
		t.Errorf("renamed directory content: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.dstDir, partial)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("old directory still present (err=%v)", err)
	}
}

func TestRenameCheckFirst(t *testing.T) {
	e := newEnv(t)
	writeFile(t, filepath.Join(e.dstDir, "a"), []byte("a"), srcMtime)
	writeFile(t, filepath.Join(e.dstDir, "b"), []byte("b"), srcMtime)
	if err := os.Mkdir(filepath.Join(e.dstDir, "emptydir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(e.dstDir, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{"b", "emptydir"} {
		if err := renameCheckFirst(e.dst, "a", to); !errors.Is(err, ErrExists) {
			t.Errorf("rename onto %s: err = %v, want ErrExists", to, err)
		}
	}
	if err := renameCheckFirst(e.dst, "dir", "emptydir"); !errors.Is(err, ErrExists) {
		t.Errorf("directory onto an empty directory: err = %v, want ErrExists", err)
	}
	if err := renameCheckFirst(e.dst, "a", "c"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(e.dstDir, "c")); string(got) != "a" {
		t.Errorf("renamed content = %q", got)
	}
}

func TestRetentionDir(t *testing.T) {
	q := time.Date(2026, 9, 24, 21, 5, 7, 999, time.FixedZone("x", 2*3600))
	if got, want := RetentionDir(q, 42), ".bunkarr/retention/20260924T190507Z-job42"; got != want {
		t.Errorf("RetentionDir = %q, want %q", got, want)
	}
}

func TestRetain(t *testing.T) {
	e := newEnv(t)
	dir := RetentionDir(srcMtime, 7)
	for i := range 3 {
		writeFile(t, filepath.Join(e.dstDir, "Movies/A/a.mkv"), []byte(fmt.Sprint("version ", i)), srcMtime)
		got, err := Retain(e.dst, "Movies/A/a.mkv", dir)
		if err != nil {
			t.Fatalf("Retain #%d: %v", i, err)
		}
		want := dir + "/Movies/A/a.mkv"
		if i > 0 {
			want = fmt.Sprintf("%s/Movies/A/a.%d.mkv", dir, i)
		}
		if got != want {
			t.Errorf("Retain #%d = %q, want %q", i, got, want)
		}
		if b, _ := os.ReadFile(filepath.Join(e.dstDir, got)); string(b) != fmt.Sprint("version ", i) {
			t.Errorf("retained #%d content = %q", i, b)
		}
	}
	if _, err := os.Lstat(filepath.Join(e.dstDir, "Movies/A/a.mkv")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("live file still present after retain")
	}
	if _, err := Retain(e.dst, "Movies/A/a.mkv", ".bunkarr/other"); !errors.Is(err, ErrOutsideRetention) && !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("retain into a non-retention dir: err = %v", err)
	}
	writeFile(t, filepath.Join(e.dstDir, "x"), []byte("x"), srcMtime)
	if _, err := Retain(e.dst, "x", ".bunkarr/other"); !errors.Is(err, ErrOutsideRetention) {
		t.Errorf("retain into a non-retention dir: err = %v, want ErrOutsideRetention", err)
	}
}

func TestSuffixed(t *testing.T) {
	tests := []struct {
		in   string
		n    int
		want string
	}{
		{"a/movie.mkv", 0, "a/movie.mkv"},
		{"a/movie.mkv", 2, "a/movie.2.mkv"},
		{"a/archive.tar.gz", 1, "a/archive.tar.1.gz"},
		{"a/.hidden", 1, "a/.hidden.1"},
		{"noext", 3, "noext.3"},
	}
	for _, tt := range tests {
		if got := suffixed(tt.in, tt.n); got != tt.want {
			t.Errorf("suffixed(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
		}
	}
}

func TestExpire(t *testing.T) {
	e := newEnv(t)
	dir := RetentionDir(srcMtime, 3)
	writeFile(t, filepath.Join(e.dstDir, dir, "Movies/A/a.mkv"), []byte("12345"), srcMtime)
	writeFile(t, filepath.Join(e.dstDir, dir, "Movies/B/b.mkv"), []byte("b"), srcMtime)
	writeFile(t, filepath.Join(e.dstDir, "Movies/A/a.mkv"), []byte("12345"), srcMtime)
	writeFile(t, filepath.Join(e.dstDir, ".bunkarr/retention/loose"), []byte("12345"), srcMtime)

	refused := []struct {
		rel string
		is  error
	}{
		{"Movies/A/a.mkv", ErrOutsideRetention},
		{MarkerRel, ErrOutsideRetention},
		{".bunkarr/retention/loose", ErrOutsideRetention},
		{".bunkarr/retention", ErrOutsideRetention},
		{dir + "/../../../Movies/A/a.mkv", ErrInvalidPath},
		{"../Movies/A/a.mkv", ErrInvalidPath},
		{"/" + dir + "/Movies/A/a.mkv", ErrInvalidPath},
		{dir + "/Movies/A", ErrNotRegular},
	}
	for _, tt := range refused {
		if err := Expire(e.dst, tt.rel, 5); !errors.Is(err, tt.is) {
			t.Errorf("Expire(%q): err = %v, want %v", tt.rel, err, tt.is)
		}
	}
	if err := Expire(e.dst, dir+"/Movies/A/a.mkv", 4); !errors.Is(err, ErrMismatch) {
		t.Errorf("wrong size: err = %v, want ErrMismatch", err)
	}
	if _, err := os.Stat(filepath.Join(e.dstDir, dir, "Movies/A/a.mkv")); err != nil {
		t.Fatalf("file removed despite a size mismatch: %v", err)
	}

	// A symlinked directory inside the retention tree must not lead the removal to a live file.
	if err := os.Symlink("../../../Movies", filepath.Join(e.dstDir, dir, "Evil")); err != nil {
		t.Fatal(err)
	}
	if err := Expire(e.dst, dir+"/Evil/A/a.mkv", 5); !errors.Is(err, ErrOutsideRetention) {
		t.Errorf("expire through a symlink: err = %v, want ErrOutsideRetention", err)
	}
	if _, err := os.Stat(filepath.Join(e.dstDir, "Movies/A/a.mkv")); err != nil {
		t.Fatalf("live file removed through a symlink: %v", err)
	}
	if err := os.Remove(filepath.Join(e.dstDir, dir, "Evil")); err != nil {
		t.Fatal(err)
	}

	if err := Expire(e.dst, dir+"/Movies/A/a.mkv", 5); err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.dstDir, dir, "Movies/A")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("empty directory not pruned (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(e.dstDir, dir, "Movies/B/b.mkv")); err != nil {
		t.Errorf("sibling removed: %v", err)
	}
	if err := Expire(e.dst, dir+"/Movies/A/a.mkv", 5); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("expire of a gone file: err = %v, want ErrNotExist", err)
	}
	if err := Expire(e.dst, dir+"/Movies/B/b.mkv", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.dstDir, dir)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("empty job directory not pruned (err=%v)", err)
	}
	if fi, err := os.Stat(filepath.Join(e.dstDir, RetentionRoot)); err != nil || !fi.IsDir() {
		t.Errorf("%s itself was removed (err=%v)", RetentionRoot, err)
	}
}

func TestMtimeMatch(t *testing.T) {
	const s = int64(time.Second)
	tests := []struct {
		a, b, g int64
		w       int
		want    bool
	}{
		{1000, 1000, 1, 0, true},
		{1000, 1001, 1, 0, false},
		{5*s + 999, 5 * s, s, 0, true},         // truncating 1 s filesystem
		{5*s + 600_000_000, 6 * s, s, 0, true}, // rounding 1 s filesystem
		{5 * s, 6 * s, s, 0, false},
		{5 * s, 6 * s, s, 1, true},
		{5 * s, 7 * s, 2 * s, 2, true},
		{5 * s, 8 * s, 2 * s, 2, false},
		{5 * s, 8 * s, 2 * s, 0, false},
		{5 * s, 5*s + 123_456_700, 100, 0, false},
		{5*s + 123_456_789, 5*s + 123_456_700, 100, 0, true},
		{7, 7, 0, 0, true}, // granularity 0 is treated as 1
	}
	for _, tt := range tests {
		if got := MtimeMatch(tt.a, tt.b, tt.g, tt.w); got != tt.want {
			t.Errorf("MtimeMatch(%d, %d, %d, %d) = %v, want %v", tt.a, tt.b, tt.g, tt.w, got, tt.want)
		}
	}
}

func TestAdoptCheck(t *testing.T) {
	e := newEnv(t)
	writeFile(t, filepath.Join(e.dstDir, "Movies/a.mkv"), []byte("12345"), srcMtime)
	if err := os.Mkdir(filepath.Join(e.dstDir, "Movies/dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	ns := srcMtime.UnixNano()
	tests := []struct {
		name          string
		rel           string
		size, mtimeNs int64
		g             int64
		w             int
		exists, match bool
	}{
		{"missing", "Movies/none.mkv", 5, ns, 1, 0, false, false},
		{"same", "Movies/a.mkv", 5, ns, 1, 0, true, true},
		{"other size", "Movies/a.mkv", 6, ns, 1, 0, true, false},
		{"other mtime", "Movies/a.mkv", 5, ns + 1, 1, 0, true, false},
		{"within granularity", "Movies/a.mkv", 5, ns + 1, int64(time.Second), 0, true, true},
		{"within window", "Movies/a.mkv", 5, ns + 2*int64(time.Second), 1, 2, true, true},
		{"directory", "Movies/dir", 5, ns, 1, 0, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exists, match, st, err := AdoptCheck(e.dst, tt.rel, tt.size, tt.mtimeNs, tt.g, tt.w)
			if err != nil {
				t.Fatal(err)
			}
			if exists != tt.exists || match != tt.match {
				t.Errorf("AdoptCheck = %v, %v; want %v, %v", exists, match, tt.exists, tt.match)
			}
			if exists && st.Mode == 0 && !st.Mode.IsDir() && st.Size == 0 {
				t.Errorf("no stat returned: %+v", st)
			}
		})
	}
	writeFile(t, filepath.Join(e.dstDir, "file"), []byte("x"), srcMtime)
	if _, _, _, err := AdoptCheck(e.dst, "file/sub", 1, ns, 1, 0); err == nil {
		t.Errorf("AdoptCheck below a regular file returned no error")
	}
}

func TestSetMtime(t *testing.T) {
	e := newEnv(t)
	writeFile(t, filepath.Join(e.dstDir, "a"), []byte("x"), time.Now())
	if err := SetMtime(e.dst, "a", srcMtime.UnixNano()); err != nil {
		t.Fatal(err)
	}
	if st, _ := Lstat(e.dst, "a"); st.MtimeNs != srcMtime.UnixNano() {
		t.Errorf("mtime = %d, want %d", st.MtimeNs, srcMtime.UnixNano())
	}
}

func TestFreeSpaceAndType(t *testing.T) {
	e := newEnv(t)
	free, total, err := FreeSpace(e.dst)
	if err != nil {
		t.Fatal(err)
	}
	if total == 0 || free > total {
		t.Errorf("FreeSpace = %d free of %d", free, total)
	}
	typ, err := FSType(e.dst)
	if err != nil || typ == "" {
		t.Errorf("FSType = %q, %v", typ, err)
	}
	st, err := RootStat(e.dst)
	if err != nil || !st.Mode.IsDir() || st.Dev == 0 && st.Ino == 0 {
		t.Errorf("RootStat = %+v, %v", st, err)
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want Class
	}{
		{"nil", nil, 0},
		{"dest ENOSPC", dstErr("write", "f", syscall.ENOSPC), Fatal},
		{"dest EDQUOT", dstErr("write", "f", syscall.EDQUOT), Fatal},
		{"dest EIO", dstErr("write", "f", syscall.EIO), Fatal},
		{"dest ENOTCONN", dstErr("open", "f", syscall.ENOTCONN), Fatal},
		{"dest ESTALE", dstErr("open", "f", syscall.ESTALE), Fatal},
		{"dest EROFS", dstErr("create", "f", syscall.EROFS), Fatal},
		{"dest EHOSTDOWN", dstErr("create", "f", syscall.EHOSTDOWN), Fatal},
		{"dest EACCES", dstErr("create", "f", syscall.EACCES), Fatal},
		{"dest EPERM", dstErr("create", "f", syscall.EPERM), Fatal},
		{"source EIO", srcErr("read", "f", syscall.EIO), Fatal},
		{"source ENOTCONN", srcErr("read", "f", syscall.ENOTCONN), Fatal},
		{"source ENOENT", srcErr("lstat", "f", syscall.ENOENT), Item},
		{"source EACCES", srcErr("open", "f", syscall.EACCES), Item},
		{"source changed", srcErr("copy", "f", ErrSourceChanged), Item},
		{"dest ENOENT", dstErr("verify", "f", syscall.ENOENT), Item},
		{"dest exists", dstErr("rename", "f", ErrExists), Item},
		{"dest name too long", dstErr("create", "f", syscall.ENAMETOOLONG), Item},
		{"dest invalid name", dstErr("create", "f", syscall.EINVAL), Item},
		{"wrapped fatal", fmt.Errorf("copy item 3: %w", dstErr("write", "f", syscall.ENOSPC)), Fatal},
		{"plain ENOSPC", &os.PathError{Op: "write", Path: "f", Err: syscall.ENOSPC}, Fatal},
		{"plain EACCES", &os.PathError{Op: "open", Path: "f", Err: syscall.EACCES}, Item},
		{"cancelled", context.Canceled, Fatal},
		{"other", errors.New("something"), Item},
	}
	for _, tt := range tests {
		if got := Classify(tt.err); got != tt.want {
			t.Errorf("%s: Classify(%v) = %v, want %v", tt.name, tt.err, got, tt.want)
		}
	}
}
