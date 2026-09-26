package arrbackup

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeZip writes data to a zip file in a fresh directory and returns its path and a separate,
// empty work directory.
func writeZip(t *testing.T, data []byte) (zipPath, work string) {
	t.Helper()
	dir := t.TempDir()
	zipPath = filepath.Join(dir, "backup.zip")
	if err := os.WriteFile(zipPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	work = filepath.Join(dir, "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	return zipPath, work
}

// sparseDB is a small database padded with empty pages, which compresses far more than 20 times
// (as a fresh *arr database does).
func sparseDB(t *testing.T) []byte {
	t.Helper()
	b := makeDB(t, 10)
	return append(b, make([]byte, 64*4096)...)
}

// assertWorkEmpty checks that VerifyZip left nothing in its work directory.
func assertWorkEmpty(t *testing.T, work string) {
	t.Helper()
	if es, err := os.ReadDir(work); err != nil || len(es) != 0 {
		t.Fatalf("the work directory holds %v (%v)", es, err)
	}
}

// corruptStored flips a byte in the stored (uncompressed) data of entry name.
func corruptStored(t *testing.T, data []byte, name string) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	for _, zf := range zr.File {
		if zf.Name != name {
			continue
		}
		off, err := zf.DataOffset()
		if err != nil {
			t.Fatal(err)
		}
		out := bytes.Clone(data)
		out[off+2] ^= 0xff
		return out
	}
	t.Fatalf("no entry %s", name)
	return nil
}

// lyingBomb is a zip whose database entry declares small but inflates to much more, with a CRC of
// the declared prefix (archive/zip must stop it at the declared size).
func lyingBomb(t *testing.T, declared, actual int) []byte {
	t.Helper()
	plain := bytes.Repeat([]byte{0}, actual)
	var comp bytes.Buffer
	fw, err := flate.NewWriter(&comp, flate.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := fw.Close(); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	w, err := zw.Create(ConfigXML)
	if err == nil {
		_, err = w.Write(configXML())
	}
	if err != nil {
		t.Fatal(err)
	}
	raw, err := zw.CreateRaw(&zip.FileHeader{Name: "radarr.db", Method: zip.Deflate, CRC32: crc32.ChecksumIEEE(plain[:declared]),
		CompressedSize64: uint64(comp.Len()), UncompressedSize64: uint64(declared)})
	if err == nil {
		_, err = raw.Write(comp.Bytes())
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// declaredBomb is a zip whose database entry declares size bytes (its data is a few bytes: the
// entry must be refused from its header, before anything is read).
func declaredBomb(t *testing.T, size uint64) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	w, err := zw.Create(ConfigXML)
	if err == nil {
		_, err = w.Write(configXML())
	}
	if err != nil {
		t.Fatal(err)
	}
	raw, err := zw.CreateRaw(&zip.FileHeader{Name: "radarr.db", Method: zip.Store, CRC32: 1, CompressedSize64: 4, UncompressedSize64: size})
	if err == nil {
		_, err = raw.Write([]byte("abcd"))
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// manyEntries is a zip with n empty entries plus config.xml and radarr.db.
func manyEntries(t *testing.T, n int) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for i := range n {
		if _, err := zw.CreateHeader(&zip.FileHeader{Name: fmt.Sprintf("e%06d", i), Method: zip.Store}); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []zipFile{{name: ConfigXML, body: configXML()}, {name: "radarr.db", body: []byte("x")}} {
		w, err := zw.Create(f.name)
		if err == nil {
			_, err = w.Write(f.body)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestVerifyZip(t *testing.T) {
	good := goodZip(t)
	dbOnly := buildZip(t, zipFile{name: "radarr.db", body: makeDB(t, 200)})
	stored := buildZip(t, zipFile{name: ConfigXML, body: configXML(), store: true}, zipFile{name: "radarr.db", body: makeDB(t, 200), store: true})
	honestBomb := declaredBomb(t, ExpansionAllowance+1)
	sparse := buildZip(t, zipFile{name: ConfigXML, body: configXML()}, zipFile{name: "radarr.db", body: sparseDB(t)})
	tests := []struct {
		name     string
		data     []byte
		zip      string
		database string
		problem  string
		entries  int
	}{
		{"a good backup", good, IntegrityOK, IntegrityOK, "", 2},
		{"a good backup of stored entries", stored, IntegrityOK, IntegrityOK, "", 2},
		{"a mostly empty database (more than 20 times)", sparse, IntegrityOK, IntegrityOK, "", 2},
		{"no config.xml", dbOnly, IntegrityFailed, IntegrityNotChecked, "no config.xml", 1},
		{"no database", buildZip(t, zipFile{name: ConfigXML, body: configXML()}), IntegrityFailed, IntegrityNotChecked, "no radarr.db", 1},
		{"the database of another app", buildZip(t, zipFile{name: ConfigXML, body: configXML()}, zipFile{name: "sonarr.db", body: makeDB(t, 200)}),
			IntegrityFailed, IntegrityNotChecked, "no radarr.db", 2},
		{"the database in a folder", buildZip(t, zipFile{name: ConfigXML, body: configXML()}, zipFile{name: "x/radarr.db", body: makeDB(t, 200)}),
			IntegrityFailed, IntegrityNotChecked, "no radarr.db", 2},
		{"a bad CRC in config.xml", corruptStored(t, stored, ConfigXML), IntegrityFailed, IntegrityOK, "config.xml: CRC mismatch", 2},
		{"a bad CRC in the database", corruptStored(t, stored, "radarr.db"), IntegrityFailed, IntegrityNotChecked, "radarr.db: CRC mismatch", 2},
		{"quick_check failing", damagedDBZip(t), IntegrityOK, IntegrityFailed, "radarr.db: quick_check", 2},
		{"a zip bomb that declares its size", honestBomb, IntegrityFailed, IntegrityNotChecked, "expand to more than", 0},
		{"a zip bomb that lies about its size", lyingBomb(t, 4096, 64<<20), IntegrityFailed, IntegrityNotChecked, "radarr.db: ", 2},
		{"65 entries", manyEntries(t, 63), IntegrityFailed, IntegrityNotChecked, "65 entries", 0},
		{"a name of 256 bytes", buildZip(t, zipFile{name: strings.Repeat("n", 256)}, zipFile{name: ConfigXML, body: configXML()}),
			IntegrityFailed, IntegrityNotChecked, "longer than 255 bytes", 0},
		{"a name twice", buildZip(t, zipFile{name: ConfigXML, body: configXML()}, zipFile{name: ConfigXML, body: configXML()},
			zipFile{name: "radarr.db", body: makeDB(t, 200)}), IntegrityFailed, IntegrityNotChecked, "appears twice", 0},
		{"not a zip", []byte("<html>login</html>"), IntegrityFailed, IntegrityNotChecked, "not a valid zip", 0},
		{"a truncated zip", good[:len(good)/2], IntegrityFailed, IntegrityNotChecked, "not a valid zip", 0},
		{"an empty database", buildZip(t, zipFile{name: ConfigXML, body: configXML()}, zipFile{name: "radarr.db"}), IntegrityOK, IntegrityFailed,
			"the file is empty", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			zipPath, work := writeZip(t, tt.data)
			rep, err := VerifyZip(context.Background(), zipPath, "radarr", work)
			if err != nil {
				t.Fatalf("VerifyZip: %v", err)
			}
			if rep.Zip != tt.zip || rep.Database != tt.database || len(rep.Entries) != tt.entries {
				t.Fatalf("report %+v, want zip %s, database %s, %d entries", rep, tt.zip, tt.database, tt.entries)
			}
			if tt.problem != "" && !strings.Contains(strings.Join(rep.Problems, "\n"), tt.problem) {
				t.Fatalf("problems %q, want one with %q", rep.Problems, tt.problem)
			}
			if (tt.problem == "") != rep.OK() {
				t.Fatalf("OK() = %v with problems %q", rep.OK(), rep.Problems)
			}
			for _, p := range rep.Problems {
				if strings.Contains(p, zipSecret) {
					t.Fatalf("a problem quotes the zip's content: %q", p)
				}
			}
			assertWorkEmpty(t, work)
		})
	}
}

func TestVerifyZipEntries(t *testing.T) {
	cfg := configXML()
	zipPath, work := writeZip(t, buildZip(t, zipFile{name: ConfigXML, body: cfg}, zipFile{name: "sonarr.db", body: makeDB(t, 200)}))
	rep, err := VerifyZip(context.Background(), zipPath, "Sonarr", work)
	if err != nil || !rep.OK() {
		t.Fatalf("VerifyZip = %+v, %v", rep, err)
	}
	want := ZipEntry{Name: ConfigXML, Size: int64(len(cfg)), CRC32: fmt.Sprintf("%08x", crc32.ChecksumIEEE(cfg))}
	if rep.Entries[0] != want || rep.Entries[1].Name != "sonarr.db" || len(rep.Entries[1].CRC32) != 8 {
		t.Fatalf("entries %+v", rep.Entries)
	}
}

// TestVerifyZipRefusesManyEntriesBeforeReading: a zip announcing 10^5 entries (a zip64 end record)
// is refused from its end record, before the central directory is parsed or any entry is read.
func TestVerifyZipRefusesManyEntriesBeforeReading(t *testing.T) {
	data := manyEntries(t, 100_000)
	if n, ok := announcedEntries(bytes.NewReader(data), int64(len(data))); !ok || n != 100_002 {
		t.Fatalf("announcedEntries = %d, %v", n, ok)
	}
	zipPath, work := writeZip(t, data)
	rep, err := VerifyZip(context.Background(), zipPath, "radarr", work)
	if err != nil || rep.Zip != IntegrityFailed || rep.Entries != nil || !strings.Contains(strings.Join(rep.Problems, ""), "100002 entries") {
		t.Fatalf("VerifyZip = %+v, %v", rep, err)
	}
	assertWorkEmpty(t, work)
}

// TestVerifyZipLyingBombStopsAtTheDeclaredSize: the extracted copy never grows past the declared
// size. VerifyZip removes check.db when it returns, so the extraction step runs on its own here
// and the file it leaves is measured.
func TestVerifyZipLyingBombStopsAtTheDeclaredSize(t *testing.T) {
	zipPath, work := writeZip(t, lyingBomb(t, 4096, 64<<20))
	zf, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer zf.Close()
	root, err := os.OpenRoot(work)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var rep ZipReport
	for _, f := range zf.File {
		if f.Name != "radarr.db" {
			continue
		}
		ok, err := extractEntry(context.Background(), f, root, &rep)
		if err != nil || ok || rep.Zip != IntegrityFailed {
			t.Fatalf("extractEntry = %v, %v, %+v", ok, err, rep)
		}
	}
	fi, err := os.Stat(filepath.Join(work, CheckDBName))
	if err != nil || fi.Size() > 4096 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("check.db: %v, %v", fi, err)
	}
}

func TestVerifyZipErrors(t *testing.T) {
	zipPath, work := writeZip(t, goodZip(t))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := VerifyZip(ctx, zipPath, "radarr", work); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
	if _, err := VerifyZip(context.Background(), filepath.Join(work, "missing.zip"), "radarr", work); err == nil {
		t.Fatal("a missing zip was verified")
	}
	link := filepath.Join(t.TempDir(), "link.zip")
	if err := os.Symlink(zipPath, link); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyZip(context.Background(), link, "radarr", work); err == nil {
		t.Fatal("a symlink was followed")
	}
	if _, err := VerifyZip(context.Background(), zipPath, "radarr", filepath.Join(work, "missing")); err == nil {
		t.Fatal("a missing work directory was accepted")
	}
	// Not enough free space for the database plus the margin.
	old := checkSpaceMargin
	checkSpaceMargin = 1 << 62
	defer func() { checkSpaceMargin = old }()
	if _, err := VerifyZip(context.Background(), zipPath, "radarr", work); err == nil || !strings.Contains(err.Error(), "free space") {
		t.Fatalf("no free space: %v", err)
	}
	assertWorkEmpty(t, work)
}

func TestAnnouncedEntries(t *testing.T) {
	for _, n := range []int{0, 1, 64, 65535, 65536} {
		data := manyEntries(t, n)
		got, ok := announcedEntries(bytes.NewReader(data), int64(len(data)))
		if !ok || got != uint64(n+2) {
			t.Errorf("%d entries: announcedEntries = %d, %v", n+2, got, ok)
		}
	}
	for _, data := range [][]byte{nil, []byte("short"), bytes.Repeat([]byte("x"), 100)} {
		if _, ok := announcedEntries(bytes.NewReader(data), int64(len(data))); ok {
			t.Errorf("announcedEntries found a record in %q", data)
		}
	}
}
