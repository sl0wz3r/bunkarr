package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations returns the migrations in fsys ("migrations/NNNN_description.sql"), sorted by
// version. Versions must be unique and positive.
func loadMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	seen := map[int]string{}
	var out []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			return nil, fmt.Errorf("migration %s: name must be NNNN_description.sql", e.Name())
		}
		v, err := strconv.Atoi(prefix)
		if err != nil || v <= 0 {
			return nil, fmt.Errorf("migration %s: invalid version prefix %q", e.Name(), prefix)
		}
		if other, dup := seen[v]; dup {
			return nil, fmt.Errorf("migrations %s and %s share version %d", other, e.Name(), v)
		}
		seen[v] = e.Name()
		body, err := fs.ReadFile(fsys, path.Join("migrations", e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", e.Name(), err)
		}
		out = append(out, migration{version: v, name: strings.TrimSuffix(e.Name(), ".sql"), sql: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// migrate applies every embedded migration not yet recorded in schema_migrations, each in its own
// transaction together with its schema_migrations row. A database carrying a migration this build
// does not know (written by a newer Bunkarr) is refused.
func (d *DB) migrate(ctx context.Context) error {
	return d.migrateFS(ctx, migrationsFS)
}

func (d *DB) migrateFS(ctx context.Context, fsys fs.FS) error {
	migrations, err := loadMigrations(fsys)
	if err != nil {
		return err
	}
	if _, err := d.w.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY NOT NULL,
		name       TEXT    NOT NULL,
		applied_at TEXT    NOT NULL
	) STRICT`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := d.appliedVersions(ctx)
	if err != nil {
		return err
	}
	known := map[int]bool{}
	latest := 0
	for _, m := range migrations {
		known[m.version] = true
		latest = max(latest, m.version)
	}
	for v := range applied {
		if !known[v] {
			return fmt.Errorf("database schema version %d is newer than this build of Bunkarr supports (latest %d); upgrade Bunkarr or restore a backup", v, latest)
		}
	}

	for _, m := range migrations {
		if applied[m.version] {
			continue
		}
		err := d.Write(ctx, func(tx *sql.Tx) error {
			// Re-check under the write lock: another process (e.g. `bunkarr reset-auth`) may have
			// applied it since appliedVersions ran.
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, m.version).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				return nil
			}
			if _, err := tx.ExecContext(ctx, m.sql); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
				m.version, m.name, FormatTime(time.Now()))
			return err
		})
		if err != nil {
			return fmt.Errorf("apply migration %s: %w", m.name, err)
		}
		d.log.Info("Applied database migration", "migration", m.name)
	}
	return nil
}

func (d *DB) appliedVersions(ctx context.Context) (map[int]bool, error) {
	rows, err := d.w.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()
	applied := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("read schema_migrations: %w", err)
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

// SchemaVersion returns the highest applied migration version.
func (d *DB) SchemaVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	err := d.r.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v)
	return int(v.Int64), err
}
