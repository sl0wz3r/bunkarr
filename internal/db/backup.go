package db

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"syscall"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/faultinject"
)

// Pre-migration copies (docs/design/phase2-3.md §14.1). Before it applies any pending migration
// to a database that already has a schema, migrate writes a consistent copy of it with VACUUM
// INTO to <database dir>/backups/bunkarr-v<old version>-<yyyymmddThhmmssZ>.db (mode 0600, in a
// 0700 directory) and refuses to migrate when that fails. Migration 0003 rebuilds tables and a
// Bunkarr that predates a migration refuses the newer schema, so this copy is the way back: stop
// Bunkarr, put the copy in place of the database, run the old image.
const (
	// BackupsDir is the directory of the pre-migration copies, next to the database file.
	BackupsDir = "backups"
	// KeepPreMigrationCopies is how many pre-migration copies are kept (the newest).
	KeepPreMigrationCopies = 3
)

// Fault points of the pre-migration copy (internal/faultinject), in the order they are reached.
const (
	// PointBackupAfterCopy follows the complete, fsynced and checked copy under its temporary
	// name, before the rename to its final name.
	PointBackupAfterCopy = "db.backupAfterCopy"
	// PointBackupAfterRename follows the rename (and the directory fsync), before older copies are
	// pruned and before the first migration is applied.
	PointBackupAfterRename = "db.backupAfterRename"
)

// copyTimeLayout formats the time part of a pre-migration copy's name (UTC).
const copyTimeLayout = "20060102T150405Z"

var (
	// copyNameRe matches a pre-migration copy: version, time, and a sequence number when two
	// copies were made in one second.
	copyNameRe = regexp.MustCompile(`^bunkarr-v(\d+)-(\d{8}T\d{6}Z)(?:-(\d+))?\.db$`)
	// partialCopyRe matches a copy being written (removed by the next copy).
	partialCopyRe = regexp.MustCompile(`^\.bunkarr-v\d+-\d{8}T\d{6}Z(?:-\d+)?\.db\.partial$`)
)

// PreMigrationCopy is one pre-migration copy found in the backups directory.
type PreMigrationCopy struct {
	// Path is the copy's file.
	Path string
	// Version is the schema version of the copy (the version before the migration).
	Version int
	// CreatedAt is when the copy was made (from its name).
	CreatedAt time.Time
	seq       int
}

// ListPreMigrationCopies returns the pre-migration copies next to the database at dbPath, newest
// first. Files whose names do not match the copies' pattern are ignored.
func ListPreMigrationCopies(dbPath string) ([]PreMigrationCopy, error) {
	dir := filepath.Join(filepath.Dir(dbPath), BackupsDir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list pre-migration copies: %w", err)
	}
	var out []PreMigrationCopy
	for _, e := range entries {
		m := copyNameRe.FindStringSubmatch(e.Name())
		if m == nil || !e.Type().IsRegular() {
			continue
		}
		v, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		at, err := time.Parse(copyTimeLayout, m[2])
		if err != nil {
			continue
		}
		seq := 0
		if m[3] != "" {
			if seq, err = strconv.Atoi(m[3]); err != nil {
				continue
			}
		}
		out = append(out, PreMigrationCopy{Path: filepath.Join(dir, e.Name()), Version: v, CreatedAt: at, seq: seq})
	}
	slices.SortFunc(out, func(a, b PreMigrationCopy) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return cmp.Compare(b.seq, a.seq)
	})
	return out, nil
}

// preMigrationCopy writes the pre-migration copy of the database at schema version version (see
// BackupsDir), then prunes the copies beyond KeepPreMigrationCopies. The copy is written under a
// temporary name, fsynced, checked (it opens, passes quick_check and has schema version version)
// and renamed, so a crash never leaves a damaged file under a copy's name, and the copies kept are
// always complete ones. A failed prune is only logged.
func (d *DB) preMigrationCopy(ctx context.Context, version int) (string, error) {
	dir := filepath.Join(filepath.Dir(d.Path), BackupsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	removePartialCopies(dir, d.log.Warn)

	now := time.Now
	if d.now != nil {
		now = d.now
	}
	base := fmt.Sprintf("bunkarr-v%d-%s", version, now().UTC().Format(copyTimeLayout))
	name := base + ".db"
	for seq := 2; ; seq++ {
		if _, err := os.Lstat(filepath.Join(dir, name)); errors.Is(err, fs.ErrNotExist) {
			break
		} else if err != nil {
			return "", fmt.Errorf("check %s: %w", name, err)
		}
		name = base + "-" + strconv.Itoa(seq) + ".db"
	}
	final := filepath.Join(dir, name)
	tmp := filepath.Join(dir, "."+name+".partial")

	// VACUUM INTO accepts an existing empty file, so the copy is created with mode 0600 from the
	// start.
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("create %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("create %s: %w", tmp, err)
	}
	if err := d.copyInto(ctx, tmp, version); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	faultinject.Point(PointBackupAfterCopy)
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("rename the copy to %s: %w", final, err)
	}
	if err := syncDir(dir); err != nil {
		return "", fmt.Errorf("sync %s: %w", dir, err)
	}
	faultinject.Point(PointBackupAfterRename)

	copies, err := ListPreMigrationCopies(d.Path)
	if err != nil {
		d.log.Warn("Old pre-migration copies of the database were not pruned", "error", err)
		return final, nil
	}
	// The copy just written is always kept (it is the only copy of the current version), even when
	// older copies carry later times (a clock that was ahead): the others are pruned to the newest
	// KeepPreMigrationCopies-1.
	others := slices.DeleteFunc(copies, func(c PreMigrationCopy) bool { return c.Path == final })
	for _, c := range others[min(len(others), KeepPreMigrationCopies-1):] {
		if err := os.Remove(c.Path); err != nil {
			d.log.Warn("An old pre-migration copy of the database could not be deleted", "path", c.Path, "error", err)
			continue
		}
		d.log.Info("Deleted an old pre-migration copy of the database", "path", c.Path)
	}
	return final, nil
}

// copyInto writes a consistent copy of the database into the empty file path with VACUUM INTO,
// fsyncs it (SQLite does not), and checks it.
func (d *DB) copyInto(ctx context.Context, path string, version int) error {
	if _, err := d.w.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		return fmt.Errorf("copy the database into %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open the copy %s: %w", path, err)
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("sync the copy %s: %w", path, err)
	}
	return checkCopy(ctx, path, version)
}

// checkCopy opens the copy at path read-only and immutable (nothing is created next to it) and
// checks that it passes quick_check and has schema version version.
func checkCopy(ctx context.Context, path string, version int) error {
	c, err := sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?mode=ro&immutable=1")
	if err != nil {
		return fmt.Errorf("open the copy %s: %w", path, err)
	}
	defer c.Close()
	var check string
	if err := c.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&check); err != nil {
		return fmt.Errorf("check the copy %s: %w", path, err)
	}
	if check != "ok" {
		return fmt.Errorf("check the copy %s: quick_check: %s", path, check)
	}
	var v sql.NullInt64
	if err := c.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		return fmt.Errorf("check the copy %s: %w", path, err)
	}
	if int(v.Int64) != version {
		return fmt.Errorf("check the copy %s: schema version %d, want %d", path, v.Int64, version)
	}
	return nil
}

// removePartialCopies removes the temporary files of copies an earlier run did not finish.
func removePartialCopies(dir string, warn func(msg string, args ...any)) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		warn("Could not look for unfinished pre-migration copies", "path", dir, "error", err)
		return
	}
	for _, e := range entries {
		if !partialCopyRe.MatchString(e.Name()) || !e.Type().IsRegular() {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			warn("Could not delete an unfinished pre-migration copy", "path", filepath.Join(dir, e.Name()), "error", err)
		}
	}
}

// syncDir fsyncs a directory, so a rename in it is durable.
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if errors.Is(err, syscall.EINVAL) {
		return nil // some filesystems cannot fsync a directory
	}
	return err
}
