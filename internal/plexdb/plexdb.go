// Package plexdb backs up Plex Media Server's library database and Preferences.xml into a
// destination as versioned snapshots (docs/design/phase1.md §5, ADR 0005).
//
// The pieces, usable on their own (the acceptance tests drive Backup and Verify from a test
// binary):
//
//   - Backup takes consistent copies of com.plexapp.plugins.library.db and, when present,
//     com.plexapp.plugins.library.blobs.db with SQLite's online backup API (one Step(-1) through
//     a mode=ro connection; mode=ro&immutable=1 guarded by a size/mtime/inode check when Plex is
//     stopped cleanly and no -wal exists), and copies Preferences.xml (read twice, compared) into
//     a local staging directory, with sha256 sums.
//   - Verify checks a copy opened mode=ro&immutable=1: quick_check and a filtered
//     integrity_check that ignores only "row N missing from index X" for indexes that use a
//     collation Plex's SQLite has and Bunkarr's does not (icu_root), with binary stubs registered
//     for those collations.
//   - Runner is the jobs.TypePlexDBBackup job: staging under <config>/staging/plexdb-job<id>/,
//     verification, the filecopy engine (safety rule S7) into
//     .bunkarr/plex/<folder>/.partial-job<id>/, manifest.json, a rename to the version's
//     timestamp, the snapshots row, and version pruning.
//   - Store reads the snapshots table.
//
// Plex's files are never written: the databases are opened read-only by SQLite and never copied
// as raw files, Preferences.xml is opened O_RDONLY|O_NOFOLLOW. Preferences.xml holds the server's
// PlexOnlineToken: its content is never logged, the token is registered with
// logging.RegisterSecret when the file is read, and the staged and destination copies are 0600.
//
// Fault-injection points (internal/faultinject), in the order a backup reaches them:
// "plexdb.afterStage", "plexdb.afterCopy" (after each file), "plexdb.beforeRename",
// "plexdb.afterRename", "plexdb.beforeRecord", "plexdb.afterRecord"; then, for each version
// pruned, "plexdb.pruneAfterTrash", "plexdb.pruneAfterUnrecord", "plexdb.pruneAfterRemove".
package plexdb

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
)

// Names of Plex's files, relative to the Plex data path ("Plex Media Server" directory), and of
// the files of a snapshot version.
const (
	// DatabasesDir holds Plex's databases, relative to the data path.
	DatabasesDir = "Plug-in Support/Databases"
	// LibraryDB is Plex's library database.
	LibraryDB = "com.plexapp.plugins.library.db"
	// BlobsDB is Plex's blobs database (backed up when present).
	BlobsDB = "com.plexapp.plugins.library.blobs.db"
	// PreferencesXML is Plex's preferences file, directly in the data path. It contains the
	// server's PlexOnlineToken.
	PreferencesXML = "Preferences.xml"
	// ManifestName is the manifest of a snapshot version.
	ManifestName = "manifest.json"
)

// Backup methods, stored in snapshots.method and the manifest.
const (
	// MethodOnlineBackup is SQLite's online backup API through a mode=ro connection (a -wal
	// existed: Plex running or crashed).
	MethodOnlineBackup = "online_backup"
	// MethodOnlineBackupImmutable is the online backup API through a mode=ro&immutable=1
	// connection, guarded by a before/after stat (no -wal: Plex stopped cleanly).
	MethodOnlineBackupImmutable = "online_backup_immutable"
	// MethodCopy is a plain copy read twice and compared (Preferences.xml).
	MethodCopy = "copy"
)

// Integrity results (snapshots.integrity, IntegrityReport fields).
const (
	IntegrityOK     = "ok"
	IntegrityFailed = "failed"
)

// Defaults.
const (
	// DefaultBusyTimeout is the busy_timeout of the connection to Plex's live database.
	DefaultBusyTimeout = 5 * time.Second
	// DefaultAttempts is how many times a database copy is tried when the file changes under an
	// immutable read, or a copy fails.
	DefaultAttempts = 3
	// FailedKeep is how long versions whose integrity check failed are kept for diagnosis.
	FailedKeep = 7 * 24 * time.Hour
)

// Fault-injection points (see the package documentation).
const (
	PointAfterStage   = "plexdb.afterStage"
	PointAfterCopy    = "plexdb.afterCopy"
	PointBeforeRename = "plexdb.beforeRename"
	PointAfterRename  = "plexdb.afterRename"
	PointBeforeRecord = "plexdb.beforeRecord"
	PointAfterRecord  = "plexdb.afterRecord"
	// PointPruneAfterTrash follows the rename of a pruned version to ".prune-<version>",
	// PointPruneAfterUnrecord the deletion of its row, PointPruneAfterRemove the deletion of its
	// files.
	PointPruneAfterTrash    = "plexdb.pruneAfterTrash"
	PointPruneAfterUnrecord = "plexdb.pruneAfterUnrecord"
	PointPruneAfterRemove   = "plexdb.pruneAfterRemove"
)

// PlexRoot is the directory of all Plex snapshots inside a destination.
const PlexRoot = filecopy.MetaDir + "/plex"

// VersionLayout formats a version directory's name from the backup time (UTC).
const VersionLayout = "20060102T150405Z"

var (
	// ErrNoDatabase means the data path holds no Plex library database.
	ErrNoDatabase = errors.New("no Plex library database found (check the Plex data path)")
	// ErrUnstable means a database kept changing while it was copied with immutable=1 (Plex was
	// starting or stopping), on every attempt.
	ErrUnstable = errors.New("the database kept changing while it was copied")
	// ErrIntegrity means the backup did not pass verification. The version is still stored (with
	// integrity "failed") for diagnosis.
	ErrIntegrity = errors.New("the Plex database backup failed verification")
	// ErrNotFound means no snapshot has the given id.
	ErrNotFound = errors.New("snapshot not found")
)

// maxSlugLen bounds the name part of a snapshot folder.
const maxSlugLen = 40

// FolderName returns the directory, relative to the destination target, that holds the
// snapshot versions of a Plex integration: ".bunkarr/plex/<slug of the name>-<id>". The id
// keeps two integrations with similar names apart and lets a job recognise its integration's
// folders after a rename.
func FolderName(integrationName string, integrationID int64) string {
	return PlexRoot + "/" + folderBase(integrationName, integrationID)
}

// folderBase is FolderName's last component.
func folderBase(name string, id int64) string {
	s := slug(name)
	if s == "" {
		s = "plex"
	}
	return s + "-" + strconv.FormatInt(id, 10)
}

// slug lowercases name and keeps [a-z0-9_]; every other run of characters becomes one "-".
func slug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > maxSlugLen {
		s = strings.TrimRight(s[:maxSlugLen], "-")
	}
	return s
}

// folderIntegrationID returns the integration id at the end of a folder base name ("plex-3" →
// 3), or 0.
func folderIntegrationID(base string) int64 {
	i := strings.LastIndexByte(base, '-')
	if i < 0 || strings.HasPrefix(base, ".") {
		return 0
	}
	id, err := strconv.ParseInt(base[i+1:], 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

var (
	// versionNameRe matches a version directory's name.
	versionNameRe = regexp.MustCompile(`^\d{8}T\d{6}Z(?:-job\d+)?$`)
	// partialNameRe matches a directory being written by a job.
	partialNameRe = regexp.MustCompile(`^\.partial-job(\d+)$`)
	// pruneNameRe matches a version directory being deleted.
	pruneNameRe = regexp.MustCompile(`^\.prune-(\d{8}T\d{6}Z(?:-job\d+)?)$`)
)

// partialName is the directory a job writes its version into.
func partialName(jobID int64) string {
	return ".partial-job" + strconv.FormatInt(jobID, 10)
}

// splitVersionPath checks that rel is ".bunkarr/plex/<folder>/<version>" (a folder that does not
// start with "." and a version name made by the runner) and returns folder and version. It is the
// fence of every deletion this package makes.
func splitVersionPath(rel string) (folder, version string, ok bool) {
	rest, found := strings.CutPrefix(rel, PlexRoot+"/")
	if !found {
		return "", "", false
	}
	folder, version, found = strings.Cut(rest, "/")
	if !found || folder == "" || strings.HasPrefix(folder, ".") || strings.Contains(version, "/") ||
		!versionNameRe.MatchString(version) {
		return "", "", false
	}
	return folder, version, true
}
