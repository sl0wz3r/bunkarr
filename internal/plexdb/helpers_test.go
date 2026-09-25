package plexdb

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	_ "modernc.org/sqlite" // the "sqlite" driver the test fixtures write with

	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// testToken is the PlexOnlineToken of the fixture Preferences.xml.
const testToken = "fixture-plex-online-token-4f2a9c"

// plexDataDir creates a Plex data directory (without databases) and returns its path.
func plexDataDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "Plex Media Server")
	if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(DatabasesDir)), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// dbPath is the path of database name in a data directory.
func dbPath(dataDir, name string) string {
	return filepath.Join(dataDir, filepath.FromSlash(DatabasesDir), name)
}

// writePrefs writes a Preferences.xml holding testToken.
func writePrefs(t *testing.T, dataDir string) {
	t.Helper()
	body := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<Preferences MachineIdentifier="abc123" ProcessedMachineIdentifier="def456" PlexOnlineToken="%s" FriendlyName="test"/>
`, testToken)
	if err := os.WriteFile(filepath.Join(dataDir, PreferencesXML), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// openRW opens a database for writing with the process-wide "sqlite" driver (as Plex would).
func openRW(t *testing.T, path string, pragmas ...string) *sql.DB {
	t.Helper()
	dsn := "file:" + path + "?_pragma=busy_timeout(10000)"
	for _, p := range pragmas {
		dsn += "&_pragma=" + p
	}
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	d.SetMaxOpenConns(1)
	return d
}

// libraryAccounts is how many rows the acct table of a fixture library has; every balance
// starts at 1000, so a consistent copy always has sum(bal) = libraryAccounts*1000.
const libraryAccounts = 100

// makeLibrary creates a Plex-like library database at path with page size 1024 (Plex's) in WAL
// mode: Plex's metadata_items and media_parts tables (one part per item), invariant tables for
// the consistency tests (acct, seq, meta), and about fillerKiB KiB of filler rows. The returned
// handle is open; close it to leave a cleanly stopped database (no -wal).
func makeLibrary(t *testing.T, path string, items, fillerKiB int) *sql.DB {
	t.Helper()
	d := openRW(t, path, "page_size(1024)", "journal_mode(WAL)")
	stmts := []string{
		`CREATE TABLE metadata_items (id INTEGER PRIMARY KEY, title TEXT, title_sort TEXT)`,
		`CREATE INDEX index_title_sort ON metadata_items (title_sort)`,
		`CREATE TABLE media_parts (id INTEGER PRIMARY KEY, media_item_id INTEGER, file TEXT)`,
		`CREATE TABLE acct (id INTEGER PRIMARY KEY, bal INTEGER NOT NULL)`,
		`CREATE TABLE seq (n INTEGER PRIMARY KEY)`,
		`CREATE TABLE meta (k TEXT PRIMARY KEY, v INTEGER NOT NULL)`,
		`CREATE TABLE filler (id INTEGER PRIMARY KEY, k TEXT, b BLOB)`,
		`CREATE INDEX filler_k ON filler (k)`,
		`INSERT INTO meta VALUES ('last', 0)`,
	}
	for _, s := range stmts {
		if _, err := d.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= libraryAccounts; i++ {
		mustExec(t, tx, `INSERT INTO acct VALUES (?, 1000)`, i)
	}
	for i := 1; i <= items; i++ {
		mustExec(t, tx, `INSERT INTO metadata_items (title, title_sort) VALUES (?, ?)`, fmt.Sprintf("Movie %d", i), fmt.Sprintf("movie %05d", i))
		mustExec(t, tx, `INSERT INTO media_parts (media_item_id, file) VALUES (?, ?)`, i, fmt.Sprintf("/data/movies/Movie %d.mkv", i))
	}
	blob := make([]byte, 700)
	for i := 0; i < fillerKiB*1024/800; i++ {
		_, _ = rand.Read(blob[:16])
		mustExec(t, tx, `INSERT INTO filler (k, b) VALUES (?, ?)`, hex.EncodeToString(blob[:8]), blob)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return d
}

func mustExec(t *testing.T, e interface {
	Exec(string, ...any) (sql.Result, error)
}, q string, args ...any) {
	t.Helper()
	if _, err := e.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// writeTx is one writer transaction on a fixture library: a transfer between two accounts, a new
// sequence number recorded in meta, and a new item with its part.
func writeTx(d *sql.DB, n int) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	a, b := n%libraryAccounts+1, (n*7)%libraryAccounts+1
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE acct SET bal = bal - 3 WHERE id = ?`, []any{a}},
		{`UPDATE acct SET bal = bal + 3 WHERE id = ?`, []any{b}},
		{`INSERT INTO seq (n) VALUES (?)`, []any{n}},
		{`UPDATE meta SET v = ? WHERE k = 'last'`, []any{n}},
		{`INSERT INTO metadata_items (title, title_sort) VALUES (?, ?)`, []any{fmt.Sprintf("New %d", n), fmt.Sprintf("new %07d", n)}},
		{`INSERT INTO media_parts (media_item_id, file) VALUES (last_insert_rowid(), ?)`, []any{fmt.Sprintf("/data/new/%d.mkv", n)}},
	} {
		if _, err := tx.Exec(q.sql, q.args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// invariants reads a copy (immutable, through plexdb's driver) and checks the fixture's
// consistency invariants; it returns the last committed sequence number in the copy.
func invariants(t *testing.T, copyPath string) int64 {
	t.Helper()
	uri, err := fileURI(copyPath, "mode=ro&immutable=1")
	if err != nil {
		t.Fatal(err)
	}
	d := openDB(uri)
	defer d.Close()
	var sum, last, maxN, count, items, parts int64
	for q, dst := range map[string]*int64{
		`SELECT sum(bal) FROM acct`:           &sum,
		`SELECT v FROM meta WHERE k = 'last'`: &last,
		`SELECT coalesce(max(n), 0) FROM seq`: &maxN,
		`SELECT count(*) FROM seq`:            &count,
		`SELECT count(*) FROM metadata_items`: &items,
		`SELECT count(*) FROM media_parts`:    &parts,
	} {
		if err := d.QueryRow(q).Scan(dst); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if sum != libraryAccounts*1000 || last != maxN || last != count || items != parts {
		t.Fatalf("inconsistent copy: sum=%d last=%d max=%d count=%d items=%d parts=%d", sum, last, maxN, count, items, parts)
	}
	return last
}

// dirSnapshot lists the entries under dir with their size and mode, to prove a tree unchanged.
func dirSnapshot(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		out = append(out, fmt.Sprintf("%s %v %d %d", rel, fi.Mode(), fi.Size(), fi.ModTime().UnixNano()))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

// recReporter records a job's logs and progress.
type recReporter struct {
	mu   sync.Mutex
	logs []string
}

func (r *recReporter) Progress(jobs.Progress) {}

func (r *recReporter) Log(level slog.Level, msg string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, fmt.Sprintf("%s %s %v", level, msg, args))
}

func (r *recReporter) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.logs, "\n")
}

func (r *recReporter) has(sub string) bool { return strings.Contains(r.text(), sub) }

// containsAll reports whether s contains every one of subs.
func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
