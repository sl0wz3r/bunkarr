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
	"unicode/utf8"

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

	// A second Verify registers its stub again, on a driver of its own.
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
			f := integrityFilter{custom: custom}
			var kept []string
			for _, l := range tt.lines {
				if f.keep(l) {
					kept = append(kept, l)
				}
			}
			ignored, indexes := f.ignored, f.sorted()
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

// craftedDB creates a database at a new path with the statements of setup (run on drv, or on the
// process-wide driver when drv is nil), then replaces the sqlite_master text of the entries in
// rewrite (name → SQL) with writable_schema, the way an attacker builds a schema SQLite would
// not create; it returns the path.
func craftedDB(t *testing.T, drv *sqlite.Driver, setup []string, rewrite map[string]string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "crafted.db")
	var d *sql.DB
	if drv != nil {
		d = sql.OpenDB(testConnector{drv: drv, dsn: "file:" + p})
		d.SetMaxOpenConns(1)
	} else {
		d = openRW(t, p)
	}
	for _, s := range setup {
		mustExec(t, d, s)
	}
	if len(rewrite) > 0 {
		mustExec(t, d, `PRAGMA writable_schema = ON`)
		for name, text := range rewrite {
			mustExec(t, d, `UPDATE sqlite_master SET sql = ? WHERE name = ?`, text, name)
		}
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

// checkBounded fails unless every line is at most maxLineBytes (plus the "…" mark) of valid
// UTF-8 and there are at most maxReportedErrors+1 of them.
func checkBounded(t *testing.T, lines []string) {
	t.Helper()
	if len(lines) > maxReportedErrors+1 {
		t.Fatalf("%d lines kept", len(lines))
	}
	for _, l := range lines {
		if len(l) > maxLineBytes+len("…") || !utf8.ValidString(l) {
			t.Fatalf("a %d-byte line was kept: %.80q…", len(l), l)
		}
	}
}

// A crafted database makes integrity_check return a line per row, each naming a 4 KiB index:
// the lines are handled one at a time, cut, and reading stops once the report is full (the
// report used to hold all of them first: 825 MB for 200k rows).
func TestVerifyBoundsIntegrityLines(t *testing.T) {
	const rows = 2000
	long := "idx_" + strings.Repeat("n", 4096)
	for _, tt := range []struct {
		name    string
		collate string
	}{{"plain index", ""}, {"stub-collated index", " COLLATE revsort"}} {
		t.Run(tt.name, func(t *testing.T) {
			drv := &sqlite.Driver{}
			if err := drv.RegisterCollationUtf8("revsort", func(a, b string) int { return strings.Compare(b, a) }); err != nil {
				t.Fatal(err)
			}
			// The long index is pointed at the empty b-tree of an index on an empty table (and that
			// one at the long index's entries), so every row of t is missing from it.
			p := craftedDB(t, drv, []string{
				`CREATE TABLE t (a TEXT)`,
				fmt.Sprintf(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < %d) INSERT INTO t SELECT 'v' || i FROM n`, rows),
				`CREATE TABLE e (a TEXT)`,
				`CREATE INDEX "` + long + `" ON t (a` + tt.collate + `)`,
				`CREATE INDEX ei ON e (a)`,
			}, nil)
			d := openRW(t, p)
			var rootLong, rootE int64
			if err := d.QueryRow(`SELECT rootpage FROM sqlite_master WHERE name = ?`, long).Scan(&rootLong); err != nil {
				t.Fatal(err)
			}
			if err := d.QueryRow(`SELECT rootpage FROM sqlite_master WHERE name = 'ei'`).Scan(&rootE); err != nil {
				t.Fatal(err)
			}
			mustExec(t, d, `PRAGMA writable_schema = ON`)
			mustExec(t, d, `UPDATE sqlite_master SET rootpage = ? WHERE name = ?`, rootE, long)
			mustExec(t, d, `UPDATE sqlite_master SET rootpage = ? WHERE name = 'ei'`, rootLong)
			if err := d.Close(); err != nil {
				t.Fatal(err)
			}

			rep, err := Verify(context.Background(), p)
			if err != nil || rep.OK() || rep.IntegrityCheck != IntegrityFailed {
				t.Fatalf("verify: %+v, %v", rep.IntegrityCheck, err)
			}
			checkBounded(t, rep.Errors)
			checkBounded(t, rep.IgnoredIndexes)
			if tt.collate == "" {
				// Every line is a problem: reading stopped after the 51st.
				if len(rep.Errors) != maxReportedErrors+1 || rep.Errors[maxReportedErrors] != "… and more (the check stopped there)" {
					t.Fatalf("%d errors, last %.80q", len(rep.Errors), rep.Errors[len(rep.Errors)-1])
				}
				return
			}
			// The stub-collated index's lines are ignored and counted, never kept, and its name is
			// noted cut.
			if rep.IgnoredLines != rows || len(rep.IgnoredIndexes) != 1 || !strings.HasPrefix(rep.IgnoredIndexes[0], "idx_nnn") {
				t.Fatalf("ignored %d lines, indexes %d", rep.IgnoredLines, len(rep.IgnoredIndexes))
			}
		})
	}
	if got := cutLine(strings.Repeat("é", maxLineBytes)); !utf8.ValidString(got) || len(got) > maxLineBytes+len("…") {
		t.Fatalf("cutLine: %d bytes, valid UTF-8 %v", len(got), utf8.ValidString(got))
	}
}

// A crafted *arr database with a 64 KiB table name and NULLs in a NOT NULL column: quick_check's
// lines name the table, and each is cut (they used to be kept whole, 50 × 1 MiB for a 1 MiB name).
func TestQuickCheckBoundsLines(t *testing.T) {
	long := "t_" + strings.Repeat("n", 64<<10)
	p := craftedDB(t, nil, []string{
		`CREATE TABLE "` + long + `" (a INTEGER, b TEXT)`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 200) INSERT INTO "` + long + `" SELECT i, NULL FROM n`,
	}, map[string]string{long: `CREATE TABLE "` + long + `" (a INTEGER, b TEXT NOT NULL)`})
	problems, err := QuickCheck(context.Background(), p)
	if err != nil || len(problems) == 0 {
		t.Fatalf("problems %d, %v", len(problems), err)
	}
	checkBounded(t, problems)
	if !strings.HasPrefix(problems[0], "quick_check: NULL value in t_nnn") || problems[len(problems)-1] != "… and more (the check stopped there)" {
		t.Fatalf("first %.80q, last %.80q", problems[0], problems[len(problems)-1])
	}
}

// A schema names its collations in text the checks read (comments included): more than
// maxCollations, or one SQLite would read cut at a NUL, is refused, and the stubs of a check never
// reach a later one. A comment naming 400k collations used to register 400k stubs on the shared
// driver, every later Open then took 40 s; "nocase\x00x" replaced NOCASE for every later check.
func TestCollationLimits(t *testing.T) {
	ctx := context.Background()
	named := func(n int) string {
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, " COLLATE c%02d", i)
		}
		return craftedDB(t, nil, []string{`CREATE TABLE t (a TEXT /*` + b.String() + ` */)`}, nil)
	}
	if problems, err := QuickCheck(ctx, named(maxCollations)); err != nil || len(problems) != 0 {
		t.Fatalf("%d collations: %q, %v", maxCollations, problems, err)
	}
	for _, check := range []func(string) []string{
		func(p string) []string { problems, _ := QuickCheck(ctx, p); return problems },
		func(p string) []string { rep, _ := Verify(ctx, p); return rep.Errors },
	} {
		if problems := check(named(maxCollations + 1)); len(problems) != 1 || !strings.Contains(problems[0], "more than 16 collations") {
			t.Fatalf("%d collations: %q", maxCollations+1, problems)
		}
		long := craftedDB(t, nil, []string{`CREATE TABLE t (a TEXT /* COLLATE ` + strings.Repeat("x", maxCollationBytes+1) + ` */)`}, nil)
		if problems := check(long); len(problems) != 1 || !strings.Contains(problems[0], "longer than 64 bytes") {
			t.Fatalf("long name: %q", problems)
		}
	}

	// SQLite stops reading the schema text at the NUL, so the crafted database itself is fine; the
	// name after it is refused.
	nul := craftedDB(t, nil, []string{`CREATE TABLE t (a TEXT)`,
		`PRAGMA writable_schema = ON`,
		`UPDATE sqlite_master SET sql = sql || char(0) || ' COLLATE "nocase' || char(0) || 'x"' WHERE name = 't'`}, nil)
	if problems, err := QuickCheck(ctx, nul); err != nil || len(problems) != 1 || !strings.Contains(problems[0], "control character") {
		t.Fatalf("NUL in a name: %q, %v", problems, err)
	}
	// A healthy database with a NOCASE index on mixed-case values still passes afterwards.
	healthy := craftedDB(t, nil, []string{`CREATE TABLE u (a TEXT)`, `CREATE INDEX ui ON u (a COLLATE NOCASE)`,
		`INSERT INTO u VALUES ('b'), ('A'), ('c'), ('B'), ('a')`}, nil)
	if rep, err := Verify(ctx, healthy); err != nil || !rep.OK() {
		t.Fatalf("healthy NOCASE database after the crafted one: %+v, %v", rep, err)
	}
	// And the stub of one check is not on any shared driver.
	if _, err := QuickCheck(ctx, craftedDB(t, nil, []string{`CREATE TABLE t (a TEXT /* COLLATE zz_leak */)`}, nil)); err != nil {
		t.Fatal(err)
	}
	d := openDB(plainDriver, "file::memory:")
	defer d.Close()
	if _, err := d.Exec(`SELECT 'a' < 'b' COLLATE zz_leak`); err == nil || !strings.Contains(err.Error(), "no such collation sequence") {
		t.Fatalf("a check's stub outlived it: %v", err)
	}
}

// The checks never run SQL of the copy's own: quick_check would compute a virtual generated
// column, integrity_check an index expression or a partial index's WHERE, row by row and beyond
// the reach of cancellation. abs(-2^63) fails when computed, so a refusal that mentions no
// overflow proves nothing ran.
func TestChecksRefuseSchemaExpressions(t *testing.T) {
	ctx := context.Background()
	const boom = `abs(-9223372036854775807 - 1)`
	base := []string{`CREATE TABLE t (a INTEGER)`, `INSERT INTO t VALUES (1), (2)`}
	generated := craftedDB(t, nil, base, map[string]string{"t": `CREATE TABLE t (a INTEGER, b AS (` + boom + `) NOT NULL)`})
	expr := craftedDB(t, nil, append(base, `CREATE INDEX i ON t ((a + 0))`), map[string]string{"i": `CREATE INDEX i ON t ((` + boom + `))`})
	partial := craftedDB(t, nil, append(base, `CREATE INDEX i ON t (a) WHERE a > 0`), map[string]string{"i": `CREATE INDEX i ON t (a) WHERE ` + boom + ` > 0`})

	refused := func(t *testing.T, problems []string, want string) {
		t.Helper()
		if len(problems) != 1 || !strings.Contains(problems[0], want) || strings.Contains(problems[0], "overflow") {
			t.Fatalf("problems %q, want a refusal naming %s", problems, want)
		}
	}
	problems, err := QuickCheck(ctx, generated)
	if err != nil {
		t.Fatal(err)
	}
	refused(t, problems, "virtual generated column (t.b)")
	for p, want := range map[string]string{generated: "virtual generated column (t.b)", expr: "an expression or with a WHERE clause (i)",
		partial: "an expression or with a WHERE clause (i)"} {
		rep, err := Verify(ctx, p)
		if err != nil || rep.OK() {
			t.Fatalf("verify: %+v, %v", rep, err)
		}
		refused(t, rep.Errors, want)
	}
	// quick_check never computes index expressions, so an *arr database with such an index passes.
	for _, p := range []string{expr, partial} {
		if problems, err := QuickCheck(ctx, p); err != nil || len(problems) != 0 {
			t.Fatalf("quick check of an index expression: %q, %v", problems, err)
		}
	}

	// Plex's virtual tables (fts4, spellfix1) use modules modernc lacks; the schema checks leave
	// them alone.
	plex := cleanLibrary(t, 20, 20)
	d := openRW(t, plex)
	mustExec(t, d, `PRAGMA writable_schema = ON`)
	mustExec(t, d, `INSERT INTO sqlite_master VALUES ('table', 'fts4_tag_titles', 'fts4_tag_titles', 0, 'CREATE VIRTUAL TABLE fts4_tag_titles USING fts4(tag)')`)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if rep, err := Verify(ctx, plex); err != nil || !rep.OK() {
		t.Fatalf("verify with a virtual table: %+v, %v", rep, err)
	}
}
