// Package db opens Bunkarr's SQLite database (pure Go, modernc.org/sqlite) in WAL mode and applies
// the embedded migrations.
//
// Two pools share the file: a single-connection writer (every write is serialized, transactions
// start with BEGIN IMMEDIATE so they never fail half-way on a lock upgrade) and a small read-only
// pool, so long reads never wait for writes and vice versa.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// TimeFormat is how every timestamp is stored: RFC3339 with nanoseconds, always UTC, so text
// comparison orders correctly.
const TimeFormat = "2006-01-02T15:04:05.000000000Z07:00"

// FormatTime renders t for storage.
func FormatTime(t time.Time) string { return t.UTC().Format(TimeFormat) }

// ParseTime reads a stored timestamp.
func ParseTime(s string) (time.Time, error) { return time.Parse(TimeFormat, s) }

// DB is the opened database.
type DB struct {
	// Path is the database file.
	Path string
	w    *sql.DB
	r    *sql.DB
	log  *slog.Logger
	// now names pre-migration copies (nil: time.Now).
	now func() time.Time
}

// Open opens (creating if needed) the database at path and migrates it to the latest schema.
// Before it migrates a database that already has a schema, it copies it into
// <dir of path>/backups (see BackupsDir) and refuses to migrate when the copy fails.
func Open(ctx context.Context, path string, log *slog.Logger) (*DB, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	base := "file:" + (&url.URL{Path: path}).EscapedPath()
	common := "&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)"

	// synchronous(FULL) on the writer: every commit is durable before the call returns. Job runners
	// record their intent (temp paths, retention paths) before touching the destination, and those
	// destination steps are fsynced; with NORMAL a power loss could keep the filesystem step but
	// lose the record that explains it. The cost is one WAL fsync per commit.
	w, err := sql.Open("sqlite", base+"?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_txlock=immediate"+common)
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}
	w.SetMaxOpenConns(1)
	w.SetConnMaxIdleTime(0)
	if err := w.PingContext(ctx); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}

	r, err := sql.Open("sqlite", base+"?_pragma=query_only(1)"+common)
	if err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("open database %s (read pool): %w", path, err)
	}
	r.SetMaxOpenConns(4)

	d := &DB{Path: path, w: w, r: r, log: log}
	if err := d.migrate(ctx); err != nil {
		_ = d.Close()
		return nil, err
	}
	return d, nil
}

// Close closes both pools.
func (d *DB) Close() error {
	return errors.Join(d.r.Close(), d.w.Close())
}

// Ping checks that the database answers a query.
func (d *DB) Ping(ctx context.Context) error {
	var one int
	return d.r.QueryRowContext(ctx, `SELECT 1`).Scan(&one)
}

// Reader is the read-only pool, for queries.
func (d *DB) Reader() *sql.DB { return d.r }

// Write runs fn in one write transaction and commits it when fn returns nil.
func (d *DB) Write(ctx context.Context, fn func(tx *sql.Tx) error) (err error) {
	tx, err := d.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin write: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
