package filecopy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sl0wz3r/bunkarr/internal/faultinject"
)

// srcMtime is the modification time given to test source files (nanosecond precision on
// purpose).
var srcMtime = time.Unix(1_600_000_000, 123_456_789)

type env struct {
	srcDir, dstDir string
	src, dst       *os.Root
}

func newEnv(t *testing.T) *env {
	t.Helper()
	base := t.TempDir()
	e := &env{srcDir: filepath.Join(base, "src"), dstDir: filepath.Join(base, "dst")}
	for _, d := range []string{e.srcDir, e.dstDir} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	if e.src, err = os.OpenRoot(e.srcDir); err != nil {
		t.Fatal(err)
	}
	if e.dst, err = os.OpenRoot(e.dstDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = e.src.Close()
		_ = e.dst.Close()
	})
	return e
}

// writeSrc creates a source file with the given content and srcMtime.
func (e *env) writeSrc(t *testing.T, rel string, data []byte) {
	t.Helper()
	writeFile(t, filepath.Join(e.srcDir, rel), data, srcMtime)
}

func writeFile(t *testing.T, p string, data []byte, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func content(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return HashPrefix + hex.EncodeToString(s[:])
}

// temps lists the temp files under dir (recursively).
func temps(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if IsTempName(d.Name()) {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestWriteTempAndCommit(t *testing.T) {
	e := newEnv(t)
	data := content(3*copyBufSize + 12345)
	e.writeSrc(t, "Movies/A (2020)/A.mkv", data)

	var recorded string
	var progressed int64
	tmp, err := WriteTemp(context.Background(), e.src, "Movies/A (2020)/A.mkv", e.dst, "Movies/Movies/A (2020)/A.mkv", CopyOptions{
		Hash: true,
		OnTemp: func(rel string) error {
			recorded = rel
			if _, err := e.dst.Lstat(rel); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("temp file exists before OnTemp returned (err=%v)", err)
			}
			return nil
		},
		Progress: func(d int64) { progressed += d },
	})
	if err != nil {
		t.Fatalf("WriteTemp: %v", err)
	}
	if tmp.Rel != recorded {
		t.Errorf("Temp.Rel = %q, OnTemp got %q", tmp.Rel, recorded)
	}
	if path.Dir(tmp.Rel) != "Movies/Movies/A (2020)" || !IsTempName(path.Base(tmp.Rel)) {
		t.Errorf("temp %q is not a temp file next to the final path", tmp.Rel)
	}
	if tmp.Size != int64(len(data)) || progressed != tmp.Size {
		t.Errorf("size %d, progress %d, want %d", tmp.Size, progressed, len(data))
	}
	if tmp.Hash != sha(data) {
		t.Errorf("hash %s, want %s", tmp.Hash, sha(data))
	}
	if tmp.MtimeNs != srcMtime.UnixNano() {
		t.Errorf("MtimeNs %d, want %d", tmp.MtimeNs, srcMtime.UnixNano())
	}
	if err := VerifyTemp(context.Background(), e.dst, tmp); err != nil {
		t.Fatalf("VerifyTemp: %v", err)
	}
	if err := Commit(e.dst, tmp.Rel, "Movies/Movies/A (2020)/A.mkv", true); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(e.dstDir, "Movies/Movies/A (2020)/A.mkv"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("final content differs (err=%v)", err)
	}
	st, err := Lstat(e.dst, "Movies/Movies/A (2020)/A.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if st.MtimeNs != srcMtime.UnixNano() {
		t.Errorf("final mtime %d, want %d", st.MtimeNs, srcMtime.UnixNano())
	}
	if left := temps(t, e.dstDir); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
}

func TestWriteTempEmptyFileWithoutHash(t *testing.T) {
	e := newEnv(t)
	e.writeSrc(t, "empty", nil)
	tmp, err := WriteTemp(context.Background(), e.src, "empty", e.dst, "d/empty", CopyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if tmp.Size != 0 || tmp.Hash != "" {
		t.Errorf("Temp = %+v, want size 0 and no hash", tmp)
	}
	if err := Commit(e.dst, tmp.Rel, "d/empty", true); err != nil {
		t.Fatal(err)
	}
}

func TestWriteTempSourceErrors(t *testing.T) {
	e := newEnv(t)
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "secret"), []byte("outside"), srcMtime)
	e.writeSrc(t, "real", []byte("data"))
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(e.srcDir, "abs-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(e.srcDir, "rel-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(e.srcDir, "dir-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(e.srcDir, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name, rel string
		is        error
	}{
		{"missing", "nope", fs.ErrNotExist},
		{"symlink out of the root", "abs-link", ErrNotRegular},
		{"symlink inside the root", "rel-link", ErrNotRegular},
		{"through a symlinked directory", "dir-link/secret", nil},
		{"directory", "dir", ErrNotRegular},
		{"escaping path", "../dst/x", ErrInvalidPath},
		{"absolute path", "/etc/passwd", ErrInvalidPath},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := WriteTemp(context.Background(), e.src, tt.rel, e.dst, "out/file", CopyOptions{})
			if err == nil {
				t.Fatal("WriteTemp succeeded")
			}
			if tt.is != nil && !errors.Is(err, tt.is) {
				t.Errorf("err = %v, want %v", err, tt.is)
			}
			var fe *Error
			if !errors.As(err, &fe) || fe.Side != SideSource {
				t.Errorf("err = %v, want a source-side *Error", err)
			}
			if c := Classify(err); c != Item {
				t.Errorf("Classify = %v, want item", c)
			}
			if left := temps(t, e.dstDir); len(left) != 0 {
				t.Errorf("temp files left: %v", left)
			}
		})
	}
}

func TestWriteTempSourceChangedDuringCopy(t *testing.T) {
	tests := []struct {
		name   string
		change func(t *testing.T, p string)
	}{
		{"appended", func(t *testing.T, p string) {
			f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err := f.Write([]byte("more")); err != nil {
				t.Fatal(err)
			}
		}},
		{"truncated", func(t *testing.T, p string) {
			if err := os.Truncate(p, 10); err != nil {
				t.Fatal(err)
			}
		}},
		{"touched", func(t *testing.T, p string) {
			if err := os.Chtimes(p, time.Now(), time.Now()); err != nil {
				t.Fatal(err)
			}
		}},
		{"rewritten with the same size", func(t *testing.T, p string) {
			if err := os.WriteFile(p, content(3*copyBufSize), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.writeSrc(t, "f", bytes.Repeat([]byte{1}, 3*copyBufSize))
			p := filepath.Join(e.srcDir, "f")
			calls := 0
			_, err := WriteTemp(context.Background(), e.src, "f", e.dst, "f", CopyOptions{
				Hash: true,
				Progress: func(int64) {
					calls++
					if calls == 1 {
						tt.change(t, p)
					}
				},
			})
			if !errors.Is(err, ErrSourceChanged) {
				t.Fatalf("err = %v, want ErrSourceChanged", err)
			}
			if Classify(err) != Item {
				t.Errorf("Classify(%v) = %v, want item", err, Classify(err))
			}
			if left := temps(t, e.dstDir); len(left) != 0 {
				t.Errorf("temp files left: %v", left)
			}
		})
	}
}

// failingWriter fails with err once more than limit bytes were written.
type failingWriter struct {
	w     io.Writer
	limit int64
	n     int64
	err   error
}

func (f *failingWriter) Write(p []byte) (int, error) {
	if f.n+int64(len(p)) > f.limit {
		return 0, f.err
	}
	f.n += int64(len(p))
	return f.w.Write(p)
}

func TestWriteTempDestinationFull(t *testing.T) {
	e := newEnv(t)
	e.writeSrc(t, "big", content(4*copyBufSize))
	_, err := WriteTemp(context.Background(), e.src, "big", e.dst, "Movies/big", CopyOptions{
		WrapWriter: func(w io.Writer) io.Writer {
			return &failingWriter{w: w, limit: 2 * copyBufSize, err: &os.PathError{Op: "write", Path: "x", Err: syscall.ENOSPC}}
		},
	})
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("err = %v, want ENOSPC", err)
	}
	if Classify(err) != Fatal {
		t.Errorf("Classify = %v, want fatal", Classify(err))
	}
	if left := temps(t, e.dstDir); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
	if _, err := os.Lstat(filepath.Join(e.dstDir, "Movies/big")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("final file exists after a failed copy (err=%v)", err)
	}
}

// TestWriteTempRealENOSPC fills a small filesystem. It runs only when BUNKARR_TEST_ENOSPC_DIR
// names a writable directory on a filesystem smaller than 8 MiB, e.g. in Docker:
//
//	docker run --rm --tmpfs /small:size=2m -e BUNKARR_TEST_ENOSPC_DIR=/small -v "$PWD":/src -w /src \
//	    golang:1.27 go test -run RealENOSPC ./internal/engines/filecopy/
func TestWriteTempRealENOSPC(t *testing.T) {
	dir := os.Getenv("BUNKARR_TEST_ENOSPC_DIR")
	if dir == "" {
		t.Skip("BUNKARR_TEST_ENOSPC_DIR not set")
	}
	e := newEnv(t)
	e.writeSrc(t, "big", content(8<<20))
	dst, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if free, _, err := FreeSpace(dst); err != nil || free >= 8<<20 {
		t.Fatalf("FreeSpace = %d, %v; the test needs less than 8 MiB free", free, err)
	}
	_, err = WriteTemp(context.Background(), e.src, "big", dst, "enospc/big", CopyOptions{Hash: true})
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("err = %v, want ENOSPC", err)
	}
	if Classify(err) != Fatal {
		t.Errorf("Classify = %v, want fatal", Classify(err))
	}
	if left := temps(t, dir); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
	_ = os.RemoveAll(filepath.Join(dir, "enospc"))
}

func TestWriteTempCancelled(t *testing.T) {
	e := newEnv(t)
	e.writeSrc(t, "big", content(4*copyBufSize))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := WriteTemp(ctx, e.src, "big", e.dst, "big", CopyOptions{Progress: func(int64) { cancel() }})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if left := temps(t, e.dstDir); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
}

func TestWriteTempOnTempErrorWritesNothing(t *testing.T) {
	e := newEnv(t)
	e.writeSrc(t, "f", []byte("x"))
	boom := errors.New("db down")
	_, err := WriteTemp(context.Background(), e.src, "f", e.dst, "d/f", CopyOptions{OnTemp: func(string) error { return boom }})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if left := temps(t, e.dstDir); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
}

func TestWriteTempNeverEscapesDestination(t *testing.T) {
	e := newEnv(t)
	outside := t.TempDir()
	e.writeSrc(t, "f", []byte("payload"))
	if err := os.Symlink(outside, filepath.Join(e.dstDir, "abs")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../"+filepath.Base(outside), filepath.Join(e.dstDir, "rel")); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"abs/f", "rel/f", "abs/sub/f", "../f"} {
		if _, err := WriteTemp(context.Background(), e.src, "f", e.dst, rel, CopyOptions{}); err == nil {
			t.Errorf("WriteTemp to %q succeeded", rel)
		}
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("files were written outside the destination: %v", entries)
	}
}

func TestWriteTempRefusesMarker(t *testing.T) {
	e := newEnv(t)
	e.writeSrc(t, "f", []byte("x"))
	for _, rel := range []string{MarkerRel, MetaDir} {
		if _, err := WriteTemp(context.Background(), e.src, "f", e.dst, rel, CopyOptions{}); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("WriteTemp to %q: err = %v, want ErrInvalidPath", rel, err)
		}
	}
}

func TestTempNameLength(t *testing.T) {
	long := strings.Repeat("é", 127) + "x" // 255 bytes
	n := tempName(long)
	if len(n) > 255 {
		t.Errorf("temp name is %d bytes", len(n))
	}
	if !utf8.ValidString(n) || !IsTempName(n) {
		t.Errorf("temp name %q is not a valid temp name", n)
	}
	if a, b := tempName("x"), tempName("x"); a == b {
		t.Errorf("two temp names are equal: %q", a)
	}
}

// crashAt runs fn with a crash-matrix hook that panics at point and reports whether it crashed
// there.
func crashAt(t *testing.T, point string, fn func()) (crashed bool) {
	t.Helper()
	faultinject.SetHook(faultinject.CrashAt(point, 1))
	defer faultinject.SetHook(nil)
	defer func() {
		if r := recover(); r != nil {
			if c, ok := r.(faultinject.Crash); ok && c.Point == point {
				crashed = true
				return
			}
			panic(r)
		}
	}()
	fn()
	return false
}

func TestCrashDuringCopyLeavesOnlyATempFile(t *testing.T) {
	for _, point := range []string{"copy.beforeTemp", "copy.afterWrite", "copy.afterSync", "copy.afterChtimes"} {
		t.Run(point, func(t *testing.T) {
			e := newEnv(t)
			data := content(2*copyBufSize + 3)
			e.writeSrc(t, "f", data)
			var recorded string
			crashed := crashAt(t, point, func() {
				_, _ = WriteTemp(context.Background(), e.src, "f", e.dst, "Movies/f", CopyOptions{
					Hash:   true,
					OnTemp: func(rel string) error { recorded = rel; return nil },
				})
			})
			if !crashed {
				t.Fatalf("did not reach %s", point)
			}
			if _, err := os.Lstat(filepath.Join(e.dstDir, "Movies/f")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("a final file exists after a crash at %s (err=%v)", point, err)
			}
			left := temps(t, e.dstDir)
			if point != "copy.beforeTemp" && len(left) != 1 {
				t.Fatalf("temp files after the crash: %v, want exactly the recorded one", left)
			}
			// The resumed job removes the temp file it recorded, then copies again.
			if err := CleanupTemp(e.dst, recorded); err != nil {
				t.Fatalf("CleanupTemp: %v", err)
			}
			if left := temps(t, e.dstDir); len(left) != 0 {
				t.Fatalf("temp files after cleanup: %v", left)
			}
			tmp, err := WriteTemp(context.Background(), e.src, "f", e.dst, "Movies/f", CopyOptions{Hash: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := Commit(e.dst, tmp.Rel, "Movies/f", true); err != nil {
				t.Fatal(err)
			}
			if _, err := VerifyFile(context.Background(), e.dst, "Movies/f", int64(len(data)), sha(data), nil); err != nil {
				t.Fatalf("destination does not verify after resume: %v", err)
			}
		})
	}
}

func TestCrashAfterRenameLeavesCompleteFinalFile(t *testing.T) {
	e := newEnv(t)
	data := content(1000)
	e.writeSrc(t, "f", data)
	tmp, err := WriteTemp(context.Background(), e.src, "f", e.dst, "f", CopyOptions{Hash: true})
	if err != nil {
		t.Fatal(err)
	}
	if !crashAt(t, "copy.afterRename", func() { _ = Commit(e.dst, tmp.Rel, "f", true) }) {
		t.Fatal("did not reach copy.afterRename")
	}
	if _, err := VerifyFile(context.Background(), e.dst, "f", 1000, sha(data), nil); err != nil {
		t.Fatalf("final file after the crash: %v", err)
	}
	if left := temps(t, e.dstDir); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
	// Resume: the temp file is gone; cleaning it up again is a no-op.
	if err := CleanupTemp(e.dst, tmp.Rel); err != nil {
		t.Errorf("CleanupTemp of a committed temp: %v", err)
	}
}

func TestCommitNoReplace(t *testing.T) {
	e := newEnv(t)
	e.writeSrc(t, "f", []byte("new"))
	writeFile(t, filepath.Join(e.dstDir, "f"), []byte("unmanaged"), srcMtime)
	tmp, err := WriteTemp(context.Background(), e.src, "f", e.dst, "f", CopyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	err = Commit(e.dst, tmp.Rel, "f", true)
	if !errors.Is(err, ErrExists) {
		t.Fatalf("Commit over an existing file: err = %v, want ErrExists", err)
	}
	if Classify(err) != Item {
		t.Errorf("Classify = %v, want item", Classify(err))
	}
	if got, _ := os.ReadFile(filepath.Join(e.dstDir, "f")); string(got) != "unmanaged" {
		t.Fatalf("existing file was changed: %q", got)
	}
	if _, err := e.dst.Lstat(tmp.Rel); err != nil {
		t.Fatalf("temp file gone after a refused commit: %v", err)
	}
	// Without noReplace (update after the old version was linked into retention) it replaces.
	if err := Commit(e.dst, tmp.Rel, "f", false); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(e.dstDir, "f")); string(got) != "new" {
		t.Fatalf("content after replace = %q", got)
	}
}

func TestCommitRefusesBadPaths(t *testing.T) {
	e := newEnv(t)
	writeFile(t, filepath.Join(e.dstDir, "plain"), []byte("x"), srcMtime)
	writeFile(t, filepath.Join(e.dstDir, TempPrefix+"x"), []byte("x"), srcMtime)
	tests := []struct {
		temp, final string
		is          error
	}{
		{"plain", "other", ErrNotTemp},
		{TempPrefix + "x", MarkerRel, ErrInvalidPath},
		{TempPrefix + "x", "../escape", ErrInvalidPath},
		{TempPrefix + "missing", "f", fs.ErrNotExist},
	}
	for _, tt := range tests {
		if err := Commit(e.dst, tt.temp, tt.final, true); !errors.Is(err, tt.is) {
			t.Errorf("Commit(%q, %q): err = %v, want %v", tt.temp, tt.final, err, tt.is)
		}
	}
}

func TestVerifyFile(t *testing.T) {
	e := newEnv(t)
	data := content(5000)
	writeFile(t, filepath.Join(e.dstDir, "f"), data, srcMtime)
	ctx := context.Background()

	got, err := VerifyFile(ctx, e.dst, "f", 5000, "", nil)
	if err != nil || got != sha(data) {
		t.Fatalf("VerifyFile without a hash = %q, %v; want %q", got, err, sha(data))
	}
	var read int64
	if _, err := VerifyFile(ctx, e.dst, "f", 5000, sha(data), func(d int64) { read += d }); err != nil || read != 5000 {
		t.Fatalf("VerifyFile = %v, progress %d", err, read)
	}
	if _, err := VerifyFile(ctx, e.dst, "f", 4999, sha(data), nil); !errors.Is(err, ErrMismatch) {
		t.Errorf("wrong size: err = %v, want ErrMismatch", err)
	}
	damaged := bytes.Clone(data)
	damaged[2500] ^= 0xff
	writeFile(t, filepath.Join(e.dstDir, "f"), damaged, srcMtime)
	got, err = VerifyFile(ctx, e.dst, "f", 5000, sha(data), nil)
	if !errors.Is(err, ErrMismatch) || got != sha(damaged) {
		t.Errorf("damaged: got %q, err = %v; want ErrMismatch and the damaged hash", got, err)
	}
	if Classify(err) != Item {
		t.Errorf("Classify(mismatch) = %v, want item", Classify(err))
	}
	if _, err := VerifyFile(ctx, e.dst, "missing", 1, "", nil); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing: err = %v, want ErrNotExist", err)
	}
}

func TestHashes(t *testing.T) {
	e := newEnv(t)
	small := content(1500)
	big := content(3*HeadTailBytes + 17)
	writeFile(t, filepath.Join(e.dstDir, "small"), small, srcMtime)
	writeFile(t, filepath.Join(e.dstDir, "big"), big, srcMtime)
	ctx := context.Background()

	h, n, err := HashFile(ctx, e.dst, "big", nil)
	if err != nil || h != sha(big) || n != int64(len(big)) {
		t.Fatalf("HashFile = %q, %d, %v", h, n, err)
	}
	ht, err := HeadTailHash(e.dst, "small")
	if err != nil {
		t.Fatal(err)
	}
	if want := HeadTailPrefix + strings.TrimPrefix(sha(small), HashPrefix); ht != want {
		t.Errorf("HeadTailHash of a small file = %q, want the whole-file digest %q", ht, want)
	}
	htBig, err := HeadTailHash(e.dst, "big")
	if err != nil {
		t.Fatal(err)
	}
	wantBig := sha256.Sum256(append(bytes.Clone(big[:HeadTailBytes]), big[len(big)-HeadTailBytes:]...))
	if htBig != HeadTailPrefix+hex.EncodeToString(wantBig[:]) {
		t.Errorf("HeadTailHash of a big file = %q", htBig)
	}

	// A change in the middle is invisible to the head/tail hash; one in the tail is not.
	middle := bytes.Clone(big)
	middle[len(big)/2] ^= 1
	writeFile(t, filepath.Join(e.dstDir, "middle"), middle, srcMtime)
	if h, _ := HeadTailHash(e.dst, "middle"); h != htBig {
		t.Errorf("middle change altered the head/tail hash")
	}
	tail := bytes.Clone(big)
	tail[len(big)-1] ^= 1
	writeFile(t, filepath.Join(e.dstDir, "tail"), tail, srcMtime)
	if h, _ := HeadTailHash(e.dst, "tail"); h == htBig {
		t.Errorf("tail change did not alter the head/tail hash")
	}
	// Between 1 and 2 MiB the ranges must not overlap: the result is the whole-file digest.
	mid := content(HeadTailBytes + 100)
	writeFile(t, filepath.Join(e.dstDir, "mid"), mid, srcMtime)
	if h, _ := HeadTailHash(e.dst, "mid"); h != HeadTailPrefix+strings.TrimPrefix(sha(mid), HashPrefix) {
		t.Errorf("HeadTailHash of a 1-2 MiB file is not the whole-file digest")
	}
	if err := os.Symlink("big", filepath.Join(e.dstDir, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := HeadTailHash(e.dst, "link"); !errors.Is(err, ErrNotRegular) {
		t.Errorf("HeadTailHash of a symlink: err = %v, want ErrNotRegular", err)
	}
}

func TestCleanupTemp(t *testing.T) {
	e := newEnv(t)
	writeFile(t, filepath.Join(e.dstDir, "d", TempPrefix+"f-abc"), []byte("partial"), srcMtime)
	writeFile(t, filepath.Join(e.dstDir, "d", "f"), []byte("live"), srcMtime)
	if err := os.Mkdir(filepath.Join(e.dstDir, TempPrefix+"dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CleanupTemp(e.dst, "d/"+TempPrefix+"f-abc"); err != nil {
		t.Fatal(err)
	}
	if left := temps(t, filepath.Join(e.dstDir, "d")); len(left) != 0 {
		t.Errorf("temp left: %v", left)
	}
	if err := CleanupTemp(e.dst, "d/"+TempPrefix+"f-abc"); err != nil {
		t.Errorf("second cleanup: %v", err)
	}
	if err := CleanupTemp(e.dst, "d/f"); !errors.Is(err, ErrNotTemp) {
		t.Errorf("cleanup of a live file: err = %v, want ErrNotTemp", err)
	}
	if err := CleanupTemp(e.dst, TempPrefix+"dir"); !errors.Is(err, ErrNotRegular) {
		t.Errorf("cleanup of a directory: err = %v, want ErrNotRegular", err)
	}
	if _, err := os.Stat(filepath.Join(e.dstDir, "d", "f")); err != nil {
		t.Errorf("live file removed: %v", err)
	}
}

func TestWriteFileAtomic(t *testing.T) {
	e := newEnv(t)
	if err := WriteFileAtomic(e.dst, MarkerRel, []byte(`{"id":"a"}`), 0o644, true); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(e.dst, MarkerRel, []byte(`{"id":"b"}`), 0o644, true); !errors.Is(err, ErrExists) {
		t.Fatalf("second no-replace write: err = %v, want ErrExists", err)
	}
	if got, _ := os.ReadFile(filepath.Join(e.dstDir, MarkerRel)); string(got) != `{"id":"a"}` {
		t.Errorf("marker = %q", got)
	}
	if err := WriteFileAtomic(e.dst, MarkerRel, []byte(`{"id":"c"}`), 0o644, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(e.dstDir, MarkerRel)); string(got) != `{"id":"c"}` {
		t.Errorf("marker after replace = %q", got)
	}
	if left := temps(t, e.dstDir); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
}

func TestDestinationPermissionDeniedIsFatal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permissions")
	}
	e := newEnv(t)
	e.writeSrc(t, "f", []byte("x"))
	if err := os.Mkdir(filepath.Join(e.dstDir, "ro"), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(e.dstDir, "ro"), 0o755) })
	_, err := WriteTemp(context.Background(), e.src, "f", e.dst, "ro/f", CopyOptions{})
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("err = %v, want permission denied", err)
	}
	if Classify(err) != Fatal {
		t.Errorf("Classify = %v, want fatal", Classify(err))
	}
	// The same error on the source side only fails the item.
	if err := os.Chmod(filepath.Join(e.srcDir, "f"), 0); err != nil {
		t.Fatal(err)
	}
	_, err = WriteTemp(context.Background(), e.src, "f", e.dst, "g", CopyOptions{})
	if !errors.Is(err, fs.ErrPermission) || Classify(err) != Item {
		t.Errorf("unreadable source: err = %v (%v), want permission denied / item", err, Classify(err))
	}
}
