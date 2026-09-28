package engines

import (
	"context"
	"database/sql"
	"io"
	"time"
)

// Ref identifies a stored version: a restic snapshot id, or the rclone version path.
type Ref string

// Version kinds of Version.Kind and ListFilter.Kind.
const (
	VersionMedia     = "media"
	VersionPlexDB    = "plexdb"
	VersionArr       = "arr"
	VersionManifest  = "manifest"
	VersionRetention = "retention"
)

// Version is one version a destination holds (Session.List): a restic snapshot of a source
// (media, per batch), a retention run, or a config version.
type Version struct {
	Kind          string    `json:"kind"`
	Ref           Ref       `json:"ref"`
	Path          string    `json:"path,omitempty"`
	SourceID      int64     `json:"sourceId,omitempty"`
	IntegrationID int64     `json:"integrationId,omitempty"`
	JobID         int64     `json:"jobId,omitempty"`
	Time          time.Time `json:"time"`
	Batch         int       `json:"batch,omitempty"`
	Complete      bool      `json:"complete"`
	Files         int64     `json:"files"`
	Bytes         int64     `json:"bytes"`
	DataAdded     int64     `json:"dataAdded,omitempty"`
}

// ListFilter narrows Session.List ("" and 0 = any).
type ListFilter struct {
	Kind          string
	SourceID      int64
	IntegrationID int64
}

// VersionStore keeps config versions (".bunkarr/<kind>/<folder>/<version>", phase1.md §2,
// phase2-3.md §3) at an engine destination (§8.1).
type VersionStore interface {
	// Put stores a complete version from a local directory (manifest.json among its files) and
	// returns its reference only after it reads back complete: a restic snapshot id, or the
	// rclone version path.
	Put(ctx context.Context, v PutVersion) (Ref, error)
	// List returns the versions under a kind folder, complete ones and leftovers.
	List(ctx context.Context, kindFolder string) ([]StoredVersion, error)
	// ReadFile reads one small file of a version (manifest.json, SHA256SUMS), at most limit bytes.
	ReadFile(ctx context.Context, ref Ref, name string, limit int64) ([]byte, error)
	// Fetch copies one file of a version into a local directory (downloads, verify).
	Fetch(ctx context.Context, ref Ref, name, dstDir string) error
	// Remove deletes a version: at once on rclone (fenced, S23); on restic by an engine_forget
	// request in the caller's transaction (D28).
	Remove(ctx context.Context, tx *sql.Tx, ref Ref, kind string) error
}

// PutVersion is one version to store.
type PutVersion struct {
	// Kind is plexdb, arr or manifest.
	Kind string
	// LogicalPath is ".bunkarr/<kind>/<folder>/<version>".
	LogicalPath string
	// Dir is the staged local directory (manifest.json inside).
	Dir string
	// Time is the version's time (restic --time).
	Time          time.Time
	JobID         int64
	IntegrationID int64
}

// StoredVersion is one version VersionStore.List found.
type StoredVersion struct {
	Ref         Ref
	LogicalPath string
	// Version is the version name (the last element of LogicalPath).
	Version string
	Time    time.Time
	// Complete: restic, a snapshot with this row's tags; rclone, manifest.json present.
	Complete bool
	// Files maps each file name to its size.
	Files         map[string]int64
	JobID         int64
	IntegrationID int64
}

// VersionOpener opens the VersionStore of a destination for one job (implemented by enginerun,
// handed to the plexdb, arrbackup and manifest runners by the api wiring). The closer ends the
// session.
type VersionOpener func(ctx context.Context, destinationID int64, rt Runtime) (VersionStore, io.Closer, error)
