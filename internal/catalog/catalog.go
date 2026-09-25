// Package catalog owns Bunkarr's sources (the media folders it backs up) and their file catalog:
// source configuration and validation, the incremental read-only scanner with hardlink detection,
// catalog queries and statistics, a per-source lock, and the scan job runner.
//
// Safety rules from docs/design/phase1.md that live here:
//   - S1: a source is only ever read. The scanner opens the source through os.Root, lstats entries,
//     never follows symlinks and opens files only read-only (hashing on FUSE sources).
//   - S4: sources are checked against destinations and the config directory when saved (an
//     injected PathGuard) and, at scan time, directories that are a destination root or the
//     config directory by (dev, ino) are skipped.
//   - S10a: a scan that finds the source missing, on another filesystem, empty while the catalog
//     has files, or failing with ENOTCONN/EIO/ESTALE is refused and leaves the catalog untouched;
//     an unreadable subdirectory keeps its rows; deletions are recorded only after the whole walk.
//     A changed device number is accepted (and recorded) only on filesystems whose device number
//     the kernel assigns at mount time (FUSE, NFS, CIFS, btrfs, ZFS, ...), and there, unless inode
//     numbers are assigned at run time (FUSE, CIFS/SMB), only with the recorded root inode.
//
// The package owns the sources and catalog_files tables; other packages use its exported API.
package catalog

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/sl0wz3r/bunkarr/internal/db"
)

var (
	// ErrNotFound means the source does not exist.
	ErrNotFound = errors.New("not found")
	// ErrConflict means the request conflicts with the current state: a duplicate name or
	// destination folder, a change the existing backups forbid, or a scan in progress. Errors
	// wrapping it carry a human message (API: 409).
	ErrConflict = errors.New("conflict")
	// ErrScanRefused marks a scan stopped by safety rule S10a (or S4 for the root): the catalog was
	// not changed. The message says why.
	ErrScanRefused = errors.New("scan refused")
)

// ValidationError is invalid input (API: 400). Field is the JSON name of the offending field.
type ValidationError struct {
	Field   string
	Message string
	// Err is the underlying error, if any (for example the PathGuard's).
	Err error
}

// Error implements error.
func (e *ValidationError) Error() string {
	if e.Field == "" {
		return e.Message
	}
	return e.Field + ": " + e.Message
}

// Unwrap returns the underlying error.
func (e *ValidationError) Unwrap() error { return e.Err }

func invalid(field, format string, args ...any) error {
	return &ValidationError{Field: field, Message: fmt.Sprintf(format, args...)}
}

// conflictError is a human-readable error that matches ErrConflict.
type conflictError struct{ msg string }

func (e *conflictError) Error() string        { return e.msg }
func (e *conflictError) Is(target error) bool { return target == ErrConflict }

func conflict(format string, args ...any) error {
	return &conflictError{msg: fmt.Sprintf(format, args...)}
}

// StoreOptions wires the hooks the catalog needs from packages it must not import.
type StoreOptions struct {
	// PathGuard checks a resolved source path against destinations and the config directory
	// (safety rule S4). Its error rejects the path (reported as a ValidationError on "path" that
	// wraps it). nil skips the check.
	PathGuard func(ctx context.Context, path string) error
	// HasBackups reports whether any destination holds files of the source. While it does, the
	// source's destFolder is immutable and its path may only change to the same directory.
	// nil means never.
	HasBackups func(ctx context.Context, sourceID int64) (bool, error)
	// ActiveJobs reports whether queued or running jobs use the source: a scan of it or a sync of
	// a destination it is linked to. A sync executes its plan without the source's lock, so Delete
	// and an Update that changes path or destFolder ask it while holding the lock and refuse with
	// ErrConflict while it is true. nil means never.
	ActiveJobs func(ctx context.Context, sourceID int64) (bool, error)
}

// Store is the sources and catalog store. It is safe for concurrent use.
type Store struct {
	db    *db.DB
	opts  StoreOptions
	locks *sourceLocks

	identityHook func(rootIdentity) rootIdentity // test hook: rewrites the new path's filesystem identity in checkSameRoot
}

// NewStore returns a store over d.
func NewStore(d *db.DB, o StoreOptions) *Store {
	return &Store{db: d, opts: o, locks: newSourceLocks()}
}

// Meta is the stat metadata the catalog records for a regular file.
type Meta struct {
	Size    int64
	MtimeNs int64
	CtimeNs int64
	// Dev is st_dev as an unsigned bit pattern (darwin's signed 32-bit dev_t is not sign-extended).
	Dev   uint64
	Inode uint64
	Nlink uint64
}

// MetaOf extracts Meta from an lstat or stat result (os.Lstat, os.Root.Lstat, fs.DirEntry.Info
// of an os.Root directory). ok is false when fi carries no system stat. Use it to compare a file
// with its catalog row, so both sides convert st_dev and st_ino the same way.
func MetaOf(fi fs.FileInfo) (m Meta, ok bool) {
	if fi == nil {
		return Meta{}, false
	}
	return metaOf(fi)
}
