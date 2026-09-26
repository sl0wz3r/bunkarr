package plexdb

import (
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"modernc.org/sqlite"
)

// cleanLibrary creates a closed (self-contained) fixture library and returns its path.
func cleanLibrary(t *testing.T, items, fillerKiB int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), LibraryDB)
	if err := makeLibrary(t, p, items, fillerKiB).Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

// pageGeometry returns a database file's page size and page count from its header.
func pageGeometry(t *testing.T, p string) (size, count int64) {
	t.Helper()
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	size = int64(raw[16])<<8 | int64(raw[17])
	if size == 1 {
		size = 65536
	}
	return size, int64(len(raw)) / size
}

func TestVerifyGoodCopy(t *testing.T) {
	p := cleanLibrary(t, 300, 300)
	before := dirSnapshot(t, filepath.Dir(p))
	rep, err := Verify(context.Background(), p)
	if err != nil || !rep.OK() || rep.Result() != IntegrityOK {
		t.Fatalf("verify: %+v, %v", rep, err)
	}
	if rep.QuickCheck != IntegrityOK || rep.IntegrityCheck != IntegrityOK || rep.IgnoredLines != 0 ||
		len(rep.IgnoredIndexes) != 0 || len(rep.Collations) != 0 || rep.SQLiteVersion == "" {
		t.Fatalf("report %+v", rep)
	}
	if !rep.PlexLibrary || rep.MetadataItems != 300 || rep.MediaParts != 300 {
		t.Fatalf("counts %+v", rep)
	}
	if got := dirSnapshot(t, filepath.Dir(p)); !slices.Equal(got, before) {
		t.Fatalf("Verify created or changed files next to the copy:\nbefore %v\nafter  %v", before, got)
	}
}

// damage is one way of damaging a database file.
type damage struct {
	name   string
	damage func(t *testing.T, p string)
}

// damages are the damage cases of TestVerifyDetectsDamage and TestQuickCheck.
func damages() []damage {
	return []damage{
		{"corrupted page", func(t *testing.T, p string) {
			size, count := pageGeometry(t, p)
			f, err := os.OpenFile(p, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			junk := make([]byte, size)
			_, _ = rand.Read(junk)
			if _, err := f.WriteAt(junk, (count/2)*size); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
		}},
		{"truncated file", func(t *testing.T, p string) {
			size, count := pageGeometry(t, p)
			if err := os.Truncate(p, (count*6/10)*size); err != nil {
				t.Fatal(err)
			}
		}},
		{"not a database", func(t *testing.T, p string) {
			junk := make([]byte, 64<<10)
			_, _ = rand.Read(junk)
			if err := os.WriteFile(p, junk, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"empty file", func(t *testing.T, p string) {
			if err := os.WriteFile(p, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	}
}

func TestVerifyDetectsDamage(t *testing.T) {
	for _, tt := range damages() {
		t.Run(tt.name, func(t *testing.T) {
			p := cleanLibrary(t, 1000, 1000)
			tt.damage(t, p)
			rep, err := Verify(context.Background(), p)
			if err != nil {
				t.Fatalf("Verify returned an error instead of a failed report: %v", err)
			}
			if rep.OK() || rep.Result() != IntegrityFailed || len(rep.Errors) == 0 {
				t.Fatalf("damage not detected: %+v", rep)
			}
			if len(rep.Errors) > maxReportedErrors+1 {
				t.Fatalf("%d error lines kept", len(rep.Errors))
			}
			t.Logf("%s: quick=%s integrity=%s first error: %s", tt.name, rep.QuickCheck, rep.IntegrityCheck, rep.Errors[0])
		})
	}
}

func TestVerifyFileErrors(t *testing.T) {
	if _, err := Verify(context.Background(), filepath.Join(t.TempDir(), "missing.db")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing file: %v", err)
	}
	if _, err := Verify(context.Background(), t.TempDir()); err == nil {
		t.Fatal("a directory was verified")
	}
	p := cleanLibrary(t, 10, 10)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Verify(ctx, p); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}

// testConnector opens connections of a test's own driver.
type testConnector struct {
	drv *sqlite.Driver
	dsn string
}

func (c testConnector) Connect(context.Context) (driver.Conn, error) { return c.drv.Open(c.dsn) }
func (c testConnector) Driver() driver.Driver                        { return c.drv }

// collatedLibrary creates a database whose indexes use the collation "revsort" (reverse order),
// registered only on the driver that creates it, like Plex's icu_root: one index names it in a
// COLLATE clause, one inherits it from the column's declaration.
func collatedLibrary(t *testing.T) string {
	t.Helper()
	drv := &sqlite.Driver{}
	if err := drv.RegisterCollationUtf8("revsort", func(a, b string) int { return strings.Compare(b, a) }); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), LibraryDB)
	d := sql.OpenDB(testConnector{drv: drv, dsn: "file:" + p + "?_pragma=journal_mode(WAL)"})
	d.SetMaxOpenConns(1)
	for _, s := range []string{
		`CREATE TABLE metadata_items (id INTEGER PRIMARY KEY, title TEXT, title_sort TEXT COLLATE "RevSort")`,
		`CREATE INDEX index_title_rev ON metadata_items (title COLLATE revsort)`,
		`CREATE INDEX index_title_sort_rev ON metadata_items (title_sort)`,
		`CREATE INDEX index_title ON metadata_items (title)`,
		`CREATE TABLE media_parts (id INTEGER PRIMARY KEY, file TEXT)`,
	} {
		if _, err := d.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := range 400 {
		title := fmt.Sprintf("Title %03d %c", (i*37)%400, 'a'+rune(i%26))
		mustExec(t, tx, `INSERT INTO metadata_items (title, title_sort) VALUES (?, ?)`, title, strings.ToLower(title))
		mustExec(t, tx, `INSERT INTO media_parts (file) VALUES (?)`, title+".mkv")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestVerifyCustomCollation(t *testing.T) {
	p := collatedLibrary(t)
	rep, err := Verify(context.Background(), p)
	if err != nil || !rep.OK() {
		t.Fatalf("verify: %+v, %v", rep, err)
	}
	if !slices.Equal(rep.Collations, []string{"revsort"}) {
		t.Fatalf("collations %v", rep.Collations)
	}
	if rep.IgnoredLines == 0 || !slices.Equal(rep.IgnoredIndexes, []string{"index_title_rev", "index_title_sort_rev"}) {
		t.Fatalf("the stub did not produce the expected ignored lines: %+v", rep)
	}
	if rep.MetadataItems != 400 || rep.MediaParts != 400 {
		t.Fatalf("counts %+v", rep)
	}

	// The stub lives on plexdb's private driver only: Bunkarr's own connections (the
	// process-wide driver) still have no such collation.
	d, err := sql.Open("sqlite", "file:"+p+"?mode=ro&immutable=1")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var title string
	err = d.QueryRow(`SELECT title FROM metadata_items ORDER BY title COLLATE revsort LIMIT 1`).Scan(&title)
	if err == nil || !strings.Contains(err.Error(), "no such collation sequence") {
		t.Fatalf("the process-wide driver has the stub collation: %q, %v", title, err)
	}

	// A second Verify reuses the registered stub.
	if rep2, err := Verify(context.Background(), p); err != nil || !rep2.OK() || rep2.IgnoredLines != rep.IgnoredLines {
		t.Fatalf("second verify: %+v, %v", rep2, err)
	}
}

func TestFilterIntegrity(t *testing.T) {
	custom := map[string]bool{"index_title_sort_icu": true, "idx with space": true}
	tests := []struct {
		name        string
		lines       []string
		wantKept    []string
		wantIgnored int
		wantIndexes []string
	}{
		{"ok", []string{"ok"}, nil, 0, []string{}},
		{"custom index lines", []string{"row 5 missing from index index_title_sort_icu", "row 9 missing from index index_title_sort_icu"},
			nil, 2, []string{"index_title_sort_icu"}},
		{"index name with a space", []string{"row 1 missing from index idx with space"}, nil, 1, []string{"idx with space"}},
		{"plain index", []string{"row 5 missing from index index_title"}, []string{"row 5 missing from index index_title"}, 0, []string{}},
		{"wrong count on a custom index is never ignored", []string{"wrong # of entries in index index_title_sort_icu"},
			[]string{"wrong # of entries in index index_title_sort_icu"}, 0, []string{}},
		{"page damage", []string{"row 5 missing from index index_title_sort_icu", "Tree 199 page 501 cell 94: Offset 18055 out of range 289..1020"},
			[]string{"Tree 199 page 501 cell 94: Offset 18055 out of range 289..1020"}, 1, []string{"index_title_sort_icu"}},
		{"prefix is not enough", []string{"row 5 missing from index index_title_sort_icu_extra"},
			[]string{"row 5 missing from index index_title_sort_icu_extra"}, 0, []string{}},
		{"non-unique entry", []string{"non-unique entry in index index_title_sort_icu"},
			[]string{"non-unique entry in index index_title_sort_icu"}, 0, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kept, ignored, indexes := filterIntegrity(tt.lines, custom)
			if !slices.Equal(kept, tt.wantKept) || ignored != tt.wantIgnored || !slices.Equal(indexes, tt.wantIndexes) {
				t.Fatalf("got %q %d %q, want %q %d %q", kept, ignored, indexes, tt.wantKept, tt.wantIgnored, tt.wantIndexes)
			}
		})
	}
}

func TestCollateRe(t *testing.T) {
	tests := []struct {
		sql  string
		want []string
	}{
		{`CREATE INDEX i ON t (title_sort COLLATE icu_root)`, []string{"icu_root"}},
		{`CREATE INDEX i ON t (a collate NOCASE, b COLLATE "My ""Coll""")`, []string{"NOCASE", `My "Coll"`}},
		{"CREATE TABLE t (a TEXT COLLATE `x`, b TEXT COLLATE [y z], c TEXT COLLATE 'q')", []string{"x", "y z", "q"}},
		{`CREATE TABLE t (collated TEXT, a TEXT)`, nil},
	}
	for _, tt := range tests {
		var got []string
		for _, m := range collateRe.FindAllStringSubmatch(tt.sql, -1) {
			got = append(got, collateName(m))
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("%s: got %q, want %q", tt.sql, got, tt.want)
		}
	}
	for name, want := range map[string]bool{"binary": true, "NOCASE": true, "RTrim": true, "icu_root": false, "": false} {
		if isBuiltinCollation(name) != want {
			t.Errorf("isBuiltinCollation(%q) = %v", name, !want)
		}
	}
}

func TestQuickCheck(t *testing.T) {
	ctx := context.Background()
	t.Run("good copies", func(t *testing.T) {
		plain := filepath.Join(t.TempDir(), "sonarr.db")
		d := openRW(t, plain, "journal_mode(DELETE)")
		for _, q := range []string{`CREATE TABLE Series (Id INTEGER PRIMARY KEY, Title TEXT)`, `CREATE INDEX IX_Title ON Series (Title)`,
			`INSERT INTO Series (Title) VALUES ('The Office'), ('Heat')`} {
			mustExec(t, d, q)
		}
		if err := d.Close(); err != nil {
			t.Fatal(err)
		}
		for name, p := range map[string]string{"rollback journal": plain, "wal, closed": cleanLibrary(t, 50, 50),
			"unknown collation": collatedLibrary(t)} {
			before := dirSnapshot(t, filepath.Dir(p))
			problems, err := QuickCheck(ctx, p)
			if err != nil || problems == nil || len(problems) != 0 {
				t.Errorf("%s: problems %q, %v; want none", name, problems, err)
			}
			if got := dirSnapshot(t, filepath.Dir(p)); !slices.Equal(got, before) {
				t.Errorf("%s: QuickCheck created or changed files next to the copy:\nbefore %v\nafter  %v", name, before, got)
			}
		}
	})
	for _, tt := range damages() {
		t.Run(tt.name, func(t *testing.T) {
			p := cleanLibrary(t, 1000, 1000)
			tt.damage(t, p)
			problems, err := QuickCheck(ctx, p)
			if err != nil {
				t.Fatalf("QuickCheck returned an error instead of problems: %v", err)
			}
			if len(problems) == 0 || len(problems) > maxReportedErrors+1 {
				t.Fatalf("damage: %d problem lines %q", len(problems), problems)
			}
		})
	}
	t.Run("file errors", func(t *testing.T) {
		if _, err := QuickCheck(ctx, filepath.Join(t.TempDir(), "missing.db")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("missing file: %v", err)
		}
		if _, err := QuickCheck(ctx, t.TempDir()); err == nil {
			t.Fatal("a directory was checked")
		}
		link := filepath.Join(t.TempDir(), "link.db")
		if err := os.Symlink(cleanLibrary(t, 10, 10), link); err != nil {
			t.Fatal(err)
		}
		if _, err := QuickCheck(ctx, link); err == nil {
			t.Fatal("a symlink was followed")
		}
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := QuickCheck(cctx, cleanLibrary(t, 10, 10)); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled: %v", err)
		}
	})
}
