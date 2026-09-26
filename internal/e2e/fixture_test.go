//go:build e2e

package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Fixture trees are generated at test time in temporary directories (never in the repository):
// deterministic pseudo-random content, nested directories, Unicode and punctuation in names,
// an empty file and a hardlinked pair (a download hardlinked into the library, as the *arr apps
// do).

// fileSpec is one generated file.
type fileSpec struct {
	rel  string
	size int
}

// libraryFiles is the main fixture: 34 files, one of them a second name of another.
var libraryFiles = []fileSpec{
	{"Movies/Alien (1979)/Alien (1979).mkv", 262144},
	{"Movies/Alien (1979)/Alien (1979).en.srt", 3000},
	{"Movies/Alien (1979)/poster.jpg", 20000},
	{"Movies/Amélie (2001)/Amélie (2001).mkv", 180000},
	{"Movies/Amélie (2001)/Amélie (2001).fr.srt", 2800},
	{"Movies/Cube (1997)/Cube (1997).mkv", 150000},
	{"Movies/Cube (1997)/Cube (1997).nfo", 1200},
	{"Movies/Dune (2021)/Dune.mkv", 400000},
	{"Movies/Heat (1995)/Heat (1995).mkv", 300000},
	{"Movies/Straße (2019)/Straße (2019).mkv", 90000},
	{"Movies/千と千尋の神隠し (2001)/千と千尋の神隠し (2001).mkv", 210000},
	{"Movies/Ødegaard Story (2020)/Ødegaard Story (2020).mkv", 70000},
	{"Movies/#Alive (2020)/#Alive (2020).mkv", 60000},
	{"Movies/'71 (2014)/'71 (2014).mkv", 55000},
	{"Movies/Up (2009)/Up (2009).mkv", 120000},
	{"Movies/Up (2009)/Up (2009).nfo", 900},
	{"Movies/Zodiac (2007)/Zodiac (2007).mkv", 130000},
	{"Movies/Zodiac (2007)/Featurettes/Behind the Scenes.mkv", 30000},
	{"Movies/Zodiac (2007)/Featurettes/Deleted Scenes.mkv", 25000},
	{"TV/The Show/tvshow.nfo", 700},
	{"TV/The Show/Season 01/The Show - S01E01.mkv", 41000},
	{"TV/The Show/Season 01/The Show - S01E02.mkv", 52000},
	{"TV/The Show/Season 01/The Show - S01E03.mkv", 63000},
	{"TV/The Show/Season 01/The Show - S01E04.mkv", 74000},
	{"TV/The Show/Season 01/The Show - S01E05.mkv", 85000},
	{"TV/The Show/Season 02/The Show - S02E01.mkv", 46000},
	{"TV/The Show/Season 02/The Show - S02E02.mkv", 57000},
	{"TV/The Show/Season 02/The Show - S02E03.mkv", 68000},
	{"TV/Kids Show/Season 01/Kids Show - S01E01.mkv", 33000},
	{"TV/Kids Show/Season 01/Kids Show - S01E02.mkv", 34000},
	{"Extras/a/b/c/d/deep.bin", 5000},
	{"Extras/empty.txt", 0},
	{"Extras/🎬 clip.mkv", 45000},
}

// The hardlinked pair of the library fixture: linkName is a second name of linkTarget.
const (
	linkTarget = "Movies/Heat (1995)/Heat (1995).mkv"
	linkName   = "Downloads/complete/Heat.1995.1080p.mkv"
)

// library describes a generated library fixture.
type library struct {
	dir string
	// files counts names (the hardlink counts twice); bytes counts every name, uniqueBytes each
	// inode once.
	files, bytes, uniqueBytes int64
	pairSize                  int64
}

// writeLibrary generates the main fixture in dir (which must exist or be creatable).
func writeLibrary(t *testing.T, dir string) library {
	t.Helper()
	lib := library{dir: dir}
	for _, f := range libraryFiles {
		writeFile(t, dir, f.rel, content(f.rel, 1, f.size))
		lib.files++
		lib.bytes += int64(f.size)
		lib.uniqueBytes += int64(f.size)
		if f.rel == linkTarget {
			lib.pairSize = int64(f.size)
		}
	}
	mkdirAll(t, filepath.Join(dir, filepath.Dir(linkName)))
	if err := os.Link(filepath.Join(dir, linkTarget), filepath.Join(dir, linkName)); err != nil {
		t.Fatalf("hardlink: %v", err)
	}
	lib.files++
	lib.bytes += lib.pairSize
	return lib
}

// writeFlat generates n files named <prefix>/dir-XX/file-XXXX.bin of size bytes (plus i%97, so
// sizes differ), perDir files per directory. It returns the total size.
func writeFlat(t *testing.T, dir, prefix string, n, perDir, size int) int64 {
	t.Helper()
	var total int64
	for i := range n {
		rel := flatName(prefix, i, perDir)
		sz := size + i%97
		writeFile(t, dir, rel, content(rel, 1, sz))
		total += int64(sz)
	}
	return total
}

// flatName is the relative path of writeFlat's i-th file.
func flatName(prefix string, i, perDir int) string {
	return fmt.Sprintf("%s/dir-%02d/file-%04d.bin", prefix, i/perDir, i)
}

// content returns size deterministic pseudo-random bytes for (name, version).
func content(name string, version, size int) []byte {
	seed := sha256.Sum256(fmt.Appendf(nil, "%s#%d", name, version))
	b := make([]byte, size)
	_, _ = rand.NewChaCha8(seed).Read(b)
	return b
}

// writeFile writes rel under dir, creating parent directories.
func writeFile(t *testing.T, dir, rel string, data []byte) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	mkdirAll(t, filepath.Dir(p))
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

// resolvedTempDir returns a new test temporary directory with symlinks resolved (on macOS
// /var is a symlink to /private/var; Bunkarr stores resolved paths).
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// fileSum is a regular file's size and sha256.
type fileSum struct {
	Size int64
	SHA  string
}

// hashTree returns every regular file under dir (relative slash paths) with its size and sha256.
// Anything else than a directory or a regular file fails the test.
func hashTree(t *testing.T, dir string) map[string]fileSum {
	t.Helper()
	out := map[string]fileSum{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file (%s)", p, d.Type())
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		sum, size, err := sha256File(p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = fileSum{Size: size, SHA: sum}
		return nil
	})
	if err != nil {
		t.Fatalf("hash %s: %v", dir, err)
	}
	return out
}

// sha256File returns a file's sha256 and size.
func sha256File(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// diffSums describes how two hashTree results differ ("" when equal).
func diffSums(want, got map[string]fileSum) string {
	var out []string
	for k, w := range want {
		g, ok := got[k]
		switch {
		case !ok:
			out = append(out, "missing: "+k)
		case g != w:
			out = append(out, fmt.Sprintf("differs: %s (want %d bytes %.12s, got %d bytes %.12s)", k, w.Size, w.SHA, g.Size, g.SHA))
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			out = append(out, "unexpected: "+k)
		}
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// tempFiles lists the names under dir that are Bunkarr temp files (.bunkarr-tmp-*).
func tempFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(d.Name(), ".bunkarr-tmp-") {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return out
}

// verifyMirror is the acceptance check of a destination: <target>/<destFolder> holds exactly the
// source's files with their sizes and hashes, and no temp file is left anywhere in the target.
func verifyMirror(t *testing.T, srcDir, target, destFolder string) {
	t.Helper()
	if d := diffSums(hashTree(t, srcDir), hashTree(t, filepath.Join(target, destFolder))); d != "" {
		t.Fatalf("the destination does not mirror the source:\n%s", d)
	}
	if tmp := tempFiles(t, target); len(tmp) > 0 {
		t.Fatalf("temp files left at the destination: %v", tmp)
	}
}

// entry is one path of a tree snapshot.
type entry struct {
	Dir      bool
	Mode     fs.FileMode
	Size     int64
	MtimeNs  int64
	CtimeNs  int64
	Ino      uint64
	Nlink    uint64
	Uid, Gid uint32
	SHA      string
}

// snapshot records every entry under dir (type, mode, size, mtime, inode change time, inode, link
// count, owner and the sha256 of files) so a test can prove that nothing at all changed, not even a
// directory's mtime: a chmod, chown or utimes that puts the old values back still moves the ctime.
func snapshot(t *testing.T, dir string) map[string]entry {
	t.Helper()
	out := map[string]entry{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		e := entry{Dir: d.IsDir(), Mode: fi.Mode(), MtimeNs: fi.ModTime().UnixNano()}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			e.Ino, e.Nlink = uint64(st.Ino), uint64(st.Nlink)
			e.CtimeNs, e.Uid, e.Gid = ctimeNs(st), st.Uid, st.Gid
		}
		if fi.Mode().IsRegular() {
			e.Size = fi.Size()
			if e.SHA, _, err = sha256File(p); err != nil {
				return err
			}
		}
		out[filepath.ToSlash(rel)] = e
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", dir, err)
	}
	return out
}

// diffSnapshots describes how two snapshots differ ("" when equal).
func diffSnapshots(before, after map[string]entry) string {
	var out []string
	for k, b := range before {
		a, ok := after[k]
		switch {
		case !ok:
			out = append(out, "removed: "+k)
		case a != b:
			out = append(out, fmt.Sprintf("changed: %s\n  before %+v\n  after  %+v", k, b, a))
		}
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			out = append(out, "added: "+k)
		}
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// requireUnchanged fails when dir differs from the snapshot before.
func requireUnchanged(t *testing.T, what string, before map[string]entry, dir string) {
	t.Helper()
	if d := diffSnapshots(before, snapshot(t, dir)); d != "" {
		t.Fatalf("%s changed the destination:\n%s", what, d)
	}
}

// requireSourceUnchanged fails when the source tree dir differs from the snapshot taken before
// job j: safety rule S1, Bunkarr never modifies source media (content, mode, mtime, ctime, inode,
// link count and owner of every entry). The test's own edits come before the snapshot.
func requireSourceUnchanged(t *testing.T, j apiJob, before map[string]entry, dir string) {
	t.Helper()
	if d := diffSnapshots(before, snapshot(t, dir)); d != "" {
		t.Fatalf("job %d (%s) changed the source %s (S1):\n%s", j.ID, j.Type, dir, d)
	}
}

// untouched runs job (it starts one job and waits for it) and requires the source tree dir to be
// unchanged by it (S1).
func untouched(t *testing.T, dir string, job func() apiJob) apiJob {
	t.Helper()
	before := snapshot(t, dir)
	j := job()
	requireSourceUnchanged(t, j, before, dir)
	return j
}

// inode returns a file's inode number and link count.
func inode(t *testing.T, p string) (ino, nlink uint64) {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("%s: no inode number", p)
	}
	return uint64(st.Ino), uint64(st.Nlink)
}

// links describes a file's inode number and link count for messages (on the share's server, see
// sameFile).
func links(t *testing.T, p string) string {
	t.Helper()
	ino, nlink := inode(t, onServer(t, p))
	return fmt.Sprintf("inode %d, %d links", ino, nlink)
}

// sameFile reports whether the paths a and b are one file (hardlinks): the same inode number. A
// CIFS client mounted with noserverino (as Unraid mounts shares) numbers inodes itself and gets
// link counts from directory listings (always 1), so for targets on a share the share test also
// mounts the share's directory on the server (BUNKARR_E2E_TARGET_BACKING), where the numbers
// are compared.
func sameFile(t *testing.T, a, b string) bool {
	t.Helper()
	inoA, _ := inode(t, onServer(t, a))
	inoB, _ := inode(t, onServer(t, b))
	return inoA == inoB
}

// onServer maps a path under $BUNKARR_E2E_TARGET_ROOT to the same path in the share's directory
// on the server, $BUNKARR_E2E_TARGET_BACKING, when that is set.
func onServer(t *testing.T, p string) string {
	t.Helper()
	backing := os.Getenv("BUNKARR_E2E_TARGET_BACKING")
	if backing == "" {
		return p
	}
	rel, err := filepath.Rel(os.Getenv("BUNKARR_E2E_TARGET_ROOT"), p)
	if err != nil || !filepath.IsLocal(rel) {
		t.Fatalf("%s is not under BUNKARR_E2E_TARGET_ROOT (%v)", p, err)
	}
	return filepath.Join(backing, rel)
}

// retained returns the retained copies of rel (relative to the destination folder) under the
// target's retention directory.
func retained(t *testing.T, target, destFolder, rel string) []string {
	t.Helper()
	// The names are literal: an *arr's "[Bluray-1080p]" is not a character class.
	literal := strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`).Replace
	m, err := filepath.Glob(filepath.Join(target, ".bunkarr", "retention", "*", literal(destFolder), literal(filepath.FromSlash(rel))))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// waitForFile waits until path exists, failing the test after timeout or when stop reports
// a reason to give up (e.g. the job already finished).
func waitForFile(t *testing.T, path string, timeout time.Duration, stop func() string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if why := stop(); why != "" {
			t.Fatalf("waiting for %s: %s", filepath.Base(path), why)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not appear within %s", path, timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
