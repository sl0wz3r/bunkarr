package db

import (
	"context"
	"database/sql"
	"io/fs"
	"log/slog"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// openUnmigrated opens a database like Open (same pragmas) without applying any migration.
func openUnmigrated(t *testing.T, path string) *DB {
	t.Helper()
	base := "file:" + (&url.URL{Path: path}).EscapedPath()
	common := "&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)"
	w, err := sql.Open("sqlite", base+"?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_txlock=immediate"+common)
	if err != nil {
		t.Fatal(err)
	}
	w.SetMaxOpenConns(1)
	r, err := sql.Open("sqlite", base+"?_pragma=query_only(1)"+common)
	if err != nil {
		t.Fatal(err)
	}
	d := &DB{Path: path, w: w, r: r, log: slog.New(slog.DiscardHandler)}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// migrationsUpTo returns the embedded migrations with a version <= last.
func migrationsUpTo(t *testing.T, last string) fstest.MapFS {
	t.Helper()
	out := fstest.MapFS{}
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() > last {
			continue
		}
		b, err := fs.ReadFile(migrationsFS, "migrations/"+e.Name())
		if err != nil {
			t.Fatal(err)
		}
		out["migrations/"+e.Name()] = &fstest.MapFile{Data: b}
	}
	return out
}

// TestMigration0003KeepsPhase1Data upgrades a Phase 1 database holding every kind of row the
// rebuilt tables carry (jobs with items, logs and snapshots, schedules, hardlink records whose
// primary has a higher id than its dependent, retained rows) and checks that nothing was lost,
// that no foreign key action ran (the rebuild must not cascade), and that the references point at
// the new tables.
func TestMigration0003KeepsPhase1Data(t *testing.T) {
	ctx := context.Background()
	d := openUnmigrated(t, filepath.Join(t.TempDir(), "bunkarr.db"))
	if err := d.migrateFS(ctx, migrationsUpTo(t, "0002_zzz")); err != nil {
		t.Fatalf("migrate to Phase 1: %v", err)
	}
	const now = "2026-09-25T00:00:00.000000000Z"
	seed := []string{
		`INSERT INTO integrations (id, type, name, url, api_key, settings, created_at, updated_at)
			VALUES (3, 'plex', 'Plex', 'http://plex:32400', '', '{}', '` + now + `', '` + now + `')`,
		`INSERT INTO sources (id, name, path, dest_folder, created_at, updated_at)
			VALUES (1, 'Movies', '/media/movies', 'movies', '` + now + `', '` + now + `')`,
		`INSERT INTO destinations (id, name, engine, target, marker_id, fs_type, root_dev, created_at, updated_at)
			VALUES (2, 'UNAS', 'filecopy', '/backup', 'm-1', 'nfs', 1, '` + now + `', '` + now + `')`,
		`INSERT INTO catalog_files (id, source_id, rel_path, size, mtime_ns, dev, inode, nlink, first_seen_at, last_seen_at)
			VALUES (1, 1, 'a.mkv', 10, 1, 1, 1, 1, '` + now + `', '` + now + `')`,
		`INSERT INTO jobs (id, type, status, trigger, params, destination_id, queued_at)
			VALUES (10, 'sync', 'completed', 'manual', '{"destinationId":2}', 2, '` + now + `'),
			       (11, 'plexdb_backup', 'completed', 'schedule', '{"destinationId":2,"integrationId":3}', 2, '` + now + `')`,
		`INSERT INTO job_items (id, job_id, rel_path, action, status) VALUES (1, 10, 'movies/a.mkv', 'copy', 'done'),
			(2, 11, 'Preferences.xml', 'backup', 'done')`,
		`INSERT INTO job_logs (id, job_id, at, level, message) VALUES (1, 10, '` + now + `', 'info', 'Job started'),
			(2, 11, '` + now + `', 'info', 'Job started')`,
		`INSERT INTO snapshots (id, destination_id, job_id, kind, integration_id, engine_snapshot_id, created_at, size, method, integrity)
			VALUES (1, 2, 11, 'plexdb', 3, '.bunkarr/plex/plex-3/20260925T000000Z', '` + now + `', 5, 'online_backup', 'ok')`,
		`INSERT INTO schedules (job_type, params, cron, created_at, updated_at)
			VALUES ('sync', '{"destinationId":2}', '0 2 * * *', '` + now + `', '` + now + `')`,
		// The primary (id 7) was promoted after its dependent (id 5) was recorded: a higher id.
		`INSERT INTO destination_files (id, destination_id, source_id, rel_path, source_rel_path, size, mtime_ns, state)
			VALUES (7, 2, 1, 'movies/b.mkv', 'b.mkv', 10, 1, 'present')`,
		`INSERT INTO destination_files (id, destination_id, source_id, rel_path, source_rel_path, size, mtime_ns, link_of, state)
			VALUES (5, 2, 1, 'movies/c.mkv', 'c.mkv', 10, 1, 7, 'link_recorded'),
			       (6, 2, 1, 'movies/d.mkv', 'd.mkv', 10, 1, 7, 'linked')`,
		`INSERT INTO destination_files (id, destination_id, source_id, rel_path, source_rel_path, size, mtime_ns, state,
			retained_path, reason, retained_at, expires_at)
			VALUES (8, 2, 1, 'movies/old.mkv', 'old.mkv', 9, 1, 'retained', '.bunkarr/retention/x-job10/movies/old.mkv',
			'deleted', '` + now + `', '` + now + `')`,
	}
	for _, q := range seed {
		if _, err := d.w.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	counts := func() map[string]int {
		out := map[string]int{}
		for _, tbl := range []string{"jobs", "job_items", "job_logs", "snapshots", "schedules", "destination_files", "catalog_files"} {
			var n int
			if err := d.w.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+tbl).Scan(&n); err != nil {
				t.Fatalf("count %s: %v", tbl, err)
			}
			out[tbl] = n
		}
		return out
	}
	before := counts()

	if err := d.migrateFS(ctx, migrationsFS); err != nil {
		t.Fatalf("migrate to Phase 2/3: %v", err)
	}
	after := counts()
	for tbl, n := range before {
		if after[tbl] != n {
			t.Errorf("%s: %d rows before, %d after", tbl, n, after[tbl])
		}
	}

	var jobID sql.NullInt64
	if err := d.w.QueryRowContext(ctx, `SELECT job_id FROM snapshots WHERE id = 1`).Scan(&jobID); err != nil || jobID.Int64 != 11 {
		t.Errorf("snapshot job_id = %v, %v; want 11 (no ON DELETE SET NULL may run)", jobID, err)
	}
	var integ sql.NullInt64
	if err := d.w.QueryRowContext(ctx, `SELECT integration_id FROM jobs WHERE id = 11`).Scan(&integ); err != nil || integ.Int64 != 3 {
		t.Errorf("jobs.integration_id = %v, %v; want 3 (backfilled from params)", integ, err)
	}
	for id, want := range map[int]string{5: "link_recorded", 6: "linked", 7: "present", 8: "retained"} {
		var state string
		var linkOf sql.NullInt64
		if err := d.w.QueryRowContext(ctx, `SELECT state, link_of FROM destination_files WHERE id = ?`, id).Scan(&state, &linkOf); err != nil {
			t.Fatal(err)
		}
		if state != want || (want == "linked" || want == "link_recorded") && linkOf.Int64 != 7 {
			t.Errorf("destination_files %d = %s link_of %v; want %s", id, state, linkOf, want)
		}
	}

	rows, err := d.w.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	if rows.Next() {
		t.Error("foreign_key_check reports violations after the migration")
	}
	_ = rows.Close()
	var ok string
	if err := d.w.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&ok); err != nil || ok != "ok" {
		t.Errorf("integrity_check = %q, %v", ok, err)
	}

	// References point at the renamed tables, and the cascades still work.
	for _, tbl := range []string{"job_items", "job_logs", "snapshots", "manifests", "destination_files"} {
		var schema string
		if err := d.w.QueryRowContext(ctx, `SELECT sql FROM sqlite_schema WHERE type = 'table' AND name = ?`, tbl).Scan(&schema); err != nil {
			t.Fatalf("%s: %v", tbl, err)
		}
		if strings.Contains(schema, "_new") {
			t.Errorf("%s still references a _new table:\n%s", tbl, schema)
		}
	}
	if _, err := d.w.ExecContext(ctx, `DELETE FROM jobs WHERE id = 10`); err != nil {
		t.Fatal(err)
	}
	var items int
	_ = d.w.QueryRowContext(ctx, `SELECT COUNT(*) FROM job_items WHERE job_id = 10`).Scan(&items)
	if items != 0 {
		t.Errorf("deleting job 10 left %d items (cascade lost)", items)
	}
	if _, err := d.w.ExecContext(ctx, `DELETE FROM destination_files WHERE id = 7`); err == nil {
		t.Error("deleting a primary with dependents succeeded (RESTRICT lost)")
	}

	// The widened constraints accept the new values; the dropped columns are gone.
	for _, q := range []string{
		`INSERT INTO jobs (type, status, trigger, params, queued_at) VALUES ('refresh', 'queued', 'webhook', '{"integrationId":3}', '` + now + `')`,
		`INSERT INTO schedules (job_type, params, cron, created_at, updated_at) VALUES ('arr_backup', '{"integrationId":3}', '0 6 * * *', '` + now + `', '` + now + `')`,
		`UPDATE destination_files SET state = 'retained', retained_path = 'r', reason = 'released', expires_at = '` + now + `' WHERE id = 6`,
	} {
		if _, err := d.w.ExecContext(ctx, q); err != nil {
			t.Errorf("%v\n%s", err, q)
		}
	}
	for _, col := range []string{"tier", "media_ids", "arr_item_id"} {
		var n int
		_ = d.w.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('catalog_files') WHERE name = ?`, col).Scan(&n)
		if n != 0 {
			t.Errorf("catalog_files.%s still exists", col)
		}
	}
	for _, idx := range []string{"jobs_status", "jobs_finished", "jobs_destination", "jobs_integration", "job_items_job_status",
		"job_items_job_action", "job_logs_job", "snapshots_destination", "snapshots_path", "destination_files_live",
		"destination_files_source", "destination_files_link_of", "destination_files_expiry"} {
		var n int
		_ = d.w.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_schema WHERE type = 'index' AND name = ?`, idx).Scan(&n)
		if n != 1 {
			t.Errorf("index %s missing", idx)
		}
	}
}
