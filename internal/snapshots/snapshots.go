// Package snapshots owns the snapshots table: the versioned backups Bunkarr keeps inside a
// destination, Plex DB versions (internal/plexdb, kind plexdb) and *arr configuration backups
// (internal/arrbackup, kind arr). It holds what those runners share (docs/design/phase2-3.md D11
// and §3, phase1.md §5):
//
//   - Store reads and writes the snapshots table.
//   - Keep and Prune are the version retention: the newest daily ok versions, the newest ok
//     version of each recent ISO week, failed versions for FailedKeep.
//   - Layout names and fences version directories,
//     <root>/<slug>-<integrationId>/<yyyymmddThhmmssZ>[-job<id>], with their siblings
//     ".partial-job<id>" (a version being written) and ".prune-<version>" (a version being
//     deleted), and moves versions into that trash and back with filecopy.RenameDir. Every
//     deletion is fenced by Layout.SplitVersionPath and RealDirs, so it can never reach outside
//     a version directory of its own kind.
package snapshots

import (
	"errors"
	"time"
)

// Kind is snapshots.kind: which runner made a version.
type Kind string

// Snapshot kinds.
const (
	// KindPlexDB is a Plex DB version (internal/plexdb).
	KindPlexDB Kind = "plexdb"
	// KindArr is an *arr configuration backup (internal/arrbackup).
	KindArr Kind = "arr"
)

// Valid reports whether k is a known kind.
func (k Kind) Valid() bool { return k == KindPlexDB || k == KindArr }

// Integrity results (snapshots.integrity).
const (
	IntegrityOK     = "ok"
	IntegrityFailed = "failed"
)

// FailedKeep is how long a version whose integrity check failed is kept for diagnosis before
// the retention deletes it.
const FailedKeep = 7 * 24 * time.Hour

// VersionLayout formats a version directory's name from its creation time (UTC).
const VersionLayout = "20060102T150405Z"

// ErrNotFound means no snapshot has the given id.
var ErrNotFound = errors.New("snapshot not found")
