package db

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/faultinject"
)

// phase1DB returns a database at schema version 2 (Phase 1) holding one setting, and its path.
func phase1DB(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bunkarr.db")
	d := openUnmigrated(t, path)
	ctx := context.Background()
	if err := d.migrateFS(ctx, migrationsUpTo(t, "0002_zzz")); err != nil {
		t.Fatalf("migrate to Phase 1: %v", err)
	}
	if _, err := d.w.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at) VALUES ('phase1.marker', 'kept', 'x')`); err != nil {
		t.Fatal(err)
	}
	return d, path
}

// readCopy opens a pre-migration copy read-only and returns its schema version and whether it
// holds the Phase 1 marker setting and the Phase 2/3 manifests table.
func readCopy(t *testing.T, path string) (version int, marker, manifests bool) {
	t.Helper()
	c, err := sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var n int
	_ = c.QueryRow(`SELECT COUNT(*) FROM settings WHERE key = 'phase1.marker' AND value = 'kept'`).Scan(&n)
	marker = n == 1
	_ = c.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = 'manifests'`).Scan(&n)
	manifests = n == 1
	var check string
	if err := c.QueryRow(`PRAGMA integrity_check`).Scan(&check); err != nil || check != "ok" {
		t.Fatalf("integrity_check of %s = %q, %v", path, check, err)
	}
	return version, marker, manifests
}

func TestPreMigrationCopyIsReadableAtTheOldVersion(t *testing.T) {
	ctx := context.Background()
	d, path := phase1DB(t)
	d.now = func() time.Time { return time.Date(2026, 9, 25, 10, 15, 0, 0, time.FixedZone("CEST", 2*3600)) }
	if err := d.migrateFS(ctx, migrationsFS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if v, _ := d.SchemaVersion(ctx); v < 3 {
		t.Fatalf("schema version %d after the migration", v)
	}
	copies, err := ListPreMigrationCopies(path)
	if err != nil || len(copies) != 1 {
		t.Fatalf("copies = %+v, %v", copies, err)
	}
	c := copies[0]
	dir := filepath.Join(filepath.Dir(path), BackupsDir)
	if c.Path != filepath.Join(dir, "bunkarr-v2-20260925T081500Z.db") || c.Version != 2 ||
		!c.CreatedAt.Equal(time.Date(2026, 9, 25, 8, 15, 0, 0, time.UTC)) {
		t.Fatalf("copy %+v", c)
	}
	if fi, err := os.Stat(c.Path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("copy mode %v, %v; want 0600", fi.Mode(), err)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("backups directory mode %v, %v; want 0700", fi.Mode(), err)
	}
	v, marker, manifests := readCopy(t, c.Path)
	if v != 2 || !marker || manifests {
		t.Fatalf("copy: version %d, marker %v, manifests table %v; want 2, true, false", v, marker, manifests)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("backups directory holds %d entries; want only the copy", len(entries))
	}

	// Nothing pending: no new copy.
	if err := d.migrateFS(ctx, migrationsFS); err != nil {
		t.Fatal(err)
	}
	if copies, _ := ListPreMigrationCopies(path); len(copies) != 1 {
		t.Fatalf("a migration with nothing pending made a copy: %+v", copies)
	}
}

func TestNewDatabaseGetsNoCopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bunkarr.db")
	d, err := Open(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), BackupsDir)); !os.IsNotExist(err) {
		t.Fatalf("a new database got a backups directory: %v", err)
	}
}

func TestPreMigrationCopiesKeepTheNewestThree(t *testing.T) {
	ctx := context.Background()
	d, path := phase1DB(t)
	dir := filepath.Join(filepath.Dir(path), BackupsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Four older copies (one of an older version with a larger number, one made in the same
	// second as another) and files that are not copies.
	for _, name := range []string{"bunkarr-v1-20260101T000000Z.db", "bunkarr-v10-20250101T000000Z.db",
		"bunkarr-v2-20260301T000000Z.db", "bunkarr-v2-20260301T000000Z-2.db", "notes.txt", "bunkarr-v2-latest.db",
		".bunkarr-v2-20260301T000000Z.db.partial"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	d.now = func() time.Time { return time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC) }
	if err := d.migrateFS(ctx, migrationsFS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var names []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := []string{"bunkarr-v2-20260301T000000Z-2.db", "bunkarr-v2-20260301T000000Z.db", "bunkarr-v2-20260925T000000Z.db",
		"bunkarr-v2-latest.db", "notes.txt"}
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Fatalf("backups directory = %q; want %q", names, want)
	}
}

// TestPreMigrationCopyKeptWhenOlderCopiesHaveLaterTimes: three older copies carry later times than
// the new one (made while the clock was ahead, then corrected). The new copy is the only copy of
// the current version and must survive the prune; the newest two of the others are kept with it.
func TestPreMigrationCopyKeptWhenOlderCopiesHaveLaterTimes(t *testing.T) {
	ctx := context.Background()
	d, path := phase1DB(t)
	dir := filepath.Join(filepath.Dir(path), BackupsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"bunkarr-v1-20270101T000000Z.db", "bunkarr-v1-20270201T000000Z.db",
		"bunkarr-v1-20270301T000000Z.db"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	d.now = func() time.Time { return time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC) }
	if err := d.migrateFS(ctx, migrationsFS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var names []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := []string{"bunkarr-v1-20270201T000000Z.db", "bunkarr-v1-20270301T000000Z.db", "bunkarr-v2-20260925T000000Z.db"}
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Fatalf("backups directory = %q; want %q", names, want)
	}
	if v, marker, _ := readCopy(t, filepath.Join(dir, "bunkarr-v2-20260925T000000Z.db")); v != 2 || !marker {
		t.Fatalf("new copy: version %d, marker %v", v, marker)
	}
}

func TestPreMigrationCopyNameCollision(t *testing.T) {
	ctx := context.Background()
	d, path := phase1DB(t)
	at := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return at }
	first, err := d.preMigrationCopy(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.preMigrationCopy(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(first) != "bunkarr-v2-20260925T000000Z.db" || filepath.Base(second) != "bunkarr-v2-20260925T000000Z-2.db" {
		t.Fatalf("copies %s, %s", first, second)
	}
	copies, _ := ListPreMigrationCopies(path)
	if len(copies) != 2 || copies[0].Path != second {
		t.Fatalf("newest first: %+v", copies)
	}
}

func TestMigrationRefusedWhenTheCopyFails(t *testing.T) {
	ctx := context.Background()
	d, path := phase1DB(t)
	// A file where the backups directory belongs: the copy cannot be written.
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), BackupsDir), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := d.migrateFS(ctx, migrationsFS)
	if err == nil || !strings.Contains(err.Error(), "refusing to migrate") {
		t.Fatalf("migrate = %v; want a refusal", err)
	}
	if v, _ := d.SchemaVersion(ctx); v != 2 {
		t.Fatalf("schema version %d; the migration ran without its copy", v)
	}
}

// TestCrashMatrixPreMigrationCopy crashes at each fault point of the copy: no migration has run,
// no file under a copy's name is incomplete, and the next start copies again and migrates.
func TestCrashMatrixPreMigrationCopy(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		point      string
		wantCopies int
	}{
		{PointBackupAfterCopy, 1},   // the unfinished copy is removed, a new one made
		{PointBackupAfterRename, 2}, // the finished copy stays, a second one is made
	} {
		t.Run(tc.point, func(t *testing.T) {
			d, path := phase1DB(t)
			clock := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
			d.now = func() time.Time { clock = clock.Add(time.Minute); return clock }
			faultinject.SetHook(faultinject.CrashAt(tc.point, 1))
			crashed := func() (c bool) {
				defer func() {
					if p := recover(); p != nil {
						if _, ok := p.(faultinject.Crash); !ok {
							panic(p)
						}
						c = true
					}
				}()
				_ = d.migrateFS(ctx, migrationsFS)
				return false
			}()
			faultinject.SetHook(nil)
			if !crashed {
				t.Fatal("the fault point was not reached")
			}
			if v, _ := d.SchemaVersion(ctx); v != 2 {
				t.Fatalf("schema version %d after the crash; no migration may run before the copy is complete", v)
			}
			// The next start.
			if err := d.migrateFS(ctx, migrationsFS); err != nil {
				t.Fatalf("migrate after the crash: %v", err)
			}
			if v, _ := d.SchemaVersion(ctx); v < 3 {
				t.Fatalf("schema version %d after the restart", v)
			}
			copies, err := ListPreMigrationCopies(path)
			if err != nil || len(copies) != tc.wantCopies {
				t.Fatalf("copies = %+v, %v; want %d", copies, err, tc.wantCopies)
			}
			for _, c := range copies {
				if v, marker, _ := readCopy(t, c.Path); v != 2 || !marker {
					t.Fatalf("copy %s: version %d, marker %v", c.Path, v, marker)
				}
			}
			entries, _ := os.ReadDir(filepath.Join(filepath.Dir(path), BackupsDir))
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".partial") {
					t.Fatalf("an unfinished copy was left: %s", e.Name())
				}
			}
		})
	}
}
