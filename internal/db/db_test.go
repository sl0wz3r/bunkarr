package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	d, err := Open(context.Background(), filepath.Join(t.TempDir(), "bunkarr.db"), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestOpenMigratesAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "bunkarr.db")
	d, err := Open(ctx, path, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	v, err := d.SchemaVersion(ctx)
	if err != nil || v < 1 {
		t.Fatalf("SchemaVersion = %d, %v", v, err)
	}
	var mode string
	if err := d.Reader().QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v; want wal", mode, err)
	}
	_ = d.Close()

	d2, err := Open(ctx, path, nil)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer d2.Close()
	v2, _ := d2.SchemaVersion(ctx)
	if v2 != v {
		t.Fatalf("schema version changed on reopen: %d -> %d", v, v2)
	}
}

func TestReaderIsReadOnly(t *testing.T) {
	d := openTest(t)
	_, err := d.Reader().ExecContext(context.Background(), `INSERT INTO settings (key, value, updated_at) VALUES ('x', 'y', 'z')`)
	if err == nil {
		t.Fatal("write through the read pool succeeded")
	}
}

func TestWriteRollsBackOnError(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	err := d.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at) VALUES ('a', 'b', 'c')`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at) VALUES ('a', 'dup', 'c')`)
		return err
	})
	if err == nil {
		t.Fatal("expected a unique constraint error")
	}
	var n int
	_ = d.Reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM settings`).Scan(&n)
	if n != 0 {
		t.Fatalf("rows after rollback = %d, want 0", n)
	}
}

func TestRefusesNewerSchema(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	future := fstest.MapFS{"migrations/0001_init.sql": {Data: []byte("SELECT 1;")}}
	if _, err := d.w.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, applied_at) VALUES (999, 'future', 'x')`); err != nil {
		t.Fatal(err)
	}
	err := d.migrateFS(ctx, future)
	if err == nil || !strings.Contains(err.Error(), "newer than this build") {
		t.Fatalf("err = %v, want a newer-schema refusal", err)
	}
}

func TestLoadMigrationsRejectsBadNames(t *testing.T) {
	for name, fsys := range map[string]fstest.MapFS{
		"no underscore": {"migrations/0001.sql": {}},
		"bad prefix":    {"migrations/abc_x.sql": {}},
		"duplicate":     {"migrations/0001_a.sql": {}, "migrations/0001_b.sql": {}},
	} {
		if _, err := loadMigrations(fsys); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
