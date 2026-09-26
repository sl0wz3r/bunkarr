// Package arrbackup backs up the configuration and database of Sonarr, Radarr and Lidarr into a
// destination as versioned snapshots of kind "arr" (docs/design/phase2-3.md §10, D3, D4, S17).
//
// The pieces:
//
//   - Runner is the jobs.TypeArrBackup job. It chooses a backup the *arr made itself (a fresh
//     scheduled one, else one it is asked to create with the Backup command), fetches the zip
//     from the *arr's Backups folder mounted read-only into Bunkarr (method "folder") or over
//     HTTP with the API key (method "http", which works only when the *arr does not require a
//     login for Bunkarr's address), stages it under <config>/staging/arrbackup-job<id>/,
//     verifies it there (VerifyZip), copies it with the filecopy engine (S7) into
//     .bunkarr/arr/<folder>/.partial-job<id>/, writes manifest.json, renames the directory to
//     the version's timestamp, records the snapshots row (kind arr) and prunes old versions.
//     Recovery, version names and pruning follow the Plex DB rules (phase1.md §5) through
//     internal/snapshots.
//   - VerifyZip checks a staged zip without trusting it: bounded entry count, name lengths and
//     declared sizes before anything is read, every entry's CRC streamed, config.xml and
//     <app>.db present at the top level, and PRAGMA quick_check (plexdb.QuickCheck) of the
//     database extracted to a fixed name in the staging directory.
//
// Sensitivity (S17). A backup zip holds everything the *arr stores: its config.xml with its API
// key, and its database with indexer and download-client credentials and Bunkarr's webhook key.
// It is stored as received, with files 0600 and directories 0700; it is read only to verify it;
// it is never served, logged or described beyond names, sizes and hashes (manifest.json and
// snapshots.manifest hold exactly that). A destination must enforce file modes
// (capabilities.enforcesModes) unless the integration sets backup.acceptInsecureModes.
//
// Paths (S18). A backup's location, in the Backups folder and over HTTP, is built only from its
// validated type (manual, scheduled or update) and name (^[A-Za-z0-9._-]{1,200}\.zip$); the path
// field of system/backup is never decoded. Backup-folder reads go through an os.Root on the
// folder, O_RDONLY|O_NOFOLLOW, regular files only.
//
// Writes to the *arr (S16). The only one is the Backup command, sent when no fresh scheduled
// backup exists and never by a dry run (S9). Bunkarr never deletes the manual backups it makes
// (a D13 question).
//
// Fault-injection points (internal/faultinject), in the order a backup reaches them:
// "arrbackup.afterCommand" (only when a Backup command is sent), "arrbackup.afterStage",
// "arrbackup.afterCopy", "arrbackup.beforeRename",
// "arrbackup.afterRename", "arrbackup.beforeRecord", "arrbackup.afterRecord"; then, for each
// version pruned, "arrbackup.pruneAfterTrash", "arrbackup.pruneAfterUnrecord",
// "arrbackup.pruneAfterRemove".
package arrbackup

import (
	"errors"
	"fmt"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr"
	"github.com/sl0wz3r/bunkarr/internal/snapshots"
)

// Fetch methods (Stats.Method, item details) and the snapshot methods they record.
const (
	// FetchFolder reads the zip from the *arr's Backups folder mounted into Bunkarr.
	FetchFolder = "folder"
	// FetchHTTP downloads the zip from <base>/backup/<type>/<name> with the API key.
	FetchHTTP = "http"
	// MethodFolder is snapshots.method of a version fetched from the Backups folder.
	MethodFolder = "arr_api_folder"
	// MethodHTTP is snapshots.method of a version downloaded over HTTP.
	MethodHTTP = "arr_api_http"
)

// Integrity results.
const (
	IntegrityOK     = snapshots.IntegrityOK
	IntegrityFailed = snapshots.IntegrityFailed
	// IntegrityNotChecked is a database that was not checked because the zip itself failed.
	IntegrityNotChecked = "not-checked"
)

// Fault-injection points (see the package documentation).
const (
	// PointAfterCommand follows the Backup command and the job item that records its id, before
	// the *arr has made the backup.
	PointAfterCommand = "arrbackup.afterCommand"
	PointAfterStage   = "arrbackup.afterStage"
	PointAfterCopy    = "arrbackup.afterCopy"
	PointBeforeRename = "arrbackup.beforeRename"
	PointAfterRename  = "arrbackup.afterRename"
	PointBeforeRecord = "arrbackup.beforeRecord"
	PointAfterRecord  = "arrbackup.afterRecord"
	// PointPruneAfterTrash follows the rename of a pruned version to ".prune-<version>",
	// PointPruneAfterUnrecord the deletion of its row, PointPruneAfterRemove the deletion of its
	// files.
	PointPruneAfterTrash    = "arrbackup.pruneAfterTrash"
	PointPruneAfterUnrecord = "arrbackup.pruneAfterUnrecord"
	PointPruneAfterRemove   = "arrbackup.pruneAfterRemove"
)

// Names and limits.
const (
	// ArrRoot is the directory of every *arr backup version inside a destination.
	ArrRoot = filecopy.MetaDir + "/arr"
	// ManifestName is a version's manifest.
	ManifestName = "manifest.json"
	// StagingRoot is the directory under the config directory that holds staging directories.
	StagingRoot = "staging"
	// ConfigXML is the *arr's configuration file inside a backup zip.
	ConfigXML = "config.xml"
	// CheckDBName is the name the database entry is extracted to for quick_check.
	CheckDBName = "check.db"
	// MaxBackupBytes caps a backup zip (fetched from the folder or over HTTP).
	MaxBackupBytes = arr.MaxBackupBytes
	// MaxEntries is the most entries a backup zip may have.
	MaxEntries = 64
	// MaxEntryNameBytes is the longest entry name accepted.
	MaxEntryNameBytes = 255
	// MaxExpansion bounds the total uncompressed size of a zip to this many times its size, above
	// ExpansionAllowance.
	MaxExpansion = 20
	// ExpansionAllowance is the total uncompressed size a zip may always have, whatever its
	// ratio: a fresh *arr database is mostly empty pages (Radarr 6.4.4: a 604 KiB database in a
	// 26 KiB zip, 23.5 times). Extraction stays bounded by the declared size and the free-space
	// check.
	ExpansionAllowance = 256 << 20
	// MaxUncompressedBytes bounds the total uncompressed size of a zip.
	MaxUncompressedBytes = 32 << 30
	// FailedKeep is how long versions whose verification failed are kept for diagnosis.
	FailedKeep = snapshots.FailedKeep
	// ManifestFormat is Manifest.Format.
	ManifestFormat = 1
)

// Defaults of the runner's timing (Options overrides them).
const (
	// DefaultPollInterval is how often GET command/{id} is asked while the *arr makes a backup.
	DefaultPollInterval = time.Second
	// DefaultCommandTimeout bounds the wait for the Backup command.
	DefaultCommandTimeout = 10 * time.Minute
	// DefaultFolderRetries is how often a backup file smaller than the API's size is looked at
	// again (the *arr may still be writing it), DefaultFolderRetryDelay apart.
	DefaultFolderRetries    = 5
	DefaultFolderRetryDelay = time.Second
	// commandQueuedSlack is how much earlier than the command's queued time a manual backup's
	// time may be (the *arr stamps the file after it queued the command, both on its own clock).
	commandQueuedSlack = 5 * time.Second
)

var (
	// ErrIntegrity means the backup did not pass verification. The version is still stored
	// (integrity failed) for diagnosis.
	ErrIntegrity = errors.New("the *arr backup failed verification")
	// ErrInsecureModes means the destination does not enforce file modes and the integration
	// does not accept that (S17).
	ErrInsecureModes = errors.New("the destination does not enforce file permissions")
	// ErrNoBackup means the *arr did not list the backup it was asked to make.
	ErrNoBackup = errors.New("the *arr did not list the backup it made")
	// ErrCommand means the *arr's Backup command did not succeed.
	ErrCommand = errors.New("the *arr's Backup command did not succeed")
	// ErrFolder means the backup could not be read from the Backups folder.
	ErrFolder = errors.New("the backup could not be read from the Backups folder")
)

// LoginRequiredError is the failure of an HTTP download that the *arr answered with a redirect to
// its login page or 401 (Forms or Basic authentication for Bunkarr's address, design D3). Its
// message names the two settings that fix it.
type LoginRequiredError struct {
	// App is the application's name ("Radarr").
	App string
}

// Error implements error.
func (e *LoginRequiredError) Error() string {
	return fmt.Sprintf("%[1]s requires a login to download backups: set its Backups folder in Bunkarr (Settings → Connect → %[1]s) "+
		"or set Authentication Required to 'Disabled for Local Addresses' in %[1]s", e.App)
}

// Unwrap returns arr.ErrLoginRequired.
func (e *LoginRequiredError) Unwrap() error { return arr.ErrLoginRequired }

// layout is where *arr versions live: .bunkarr/arr/<slug>-<integrationId>/<version>.
var layout = snapshots.Layout{Root: ArrRoot, DefaultSlug: "arr"}

// FolderName returns the directory, relative to the destination target, that holds the versions
// of an *arr integration: ".bunkarr/arr/<slug of the name>-<id>".
func FolderName(integrationName string, integrationID int64) string {
	return layout.Folder(integrationName, integrationID)
}

// Manifest is a version's manifest.json (design §3). It holds names, sizes and hashes only.
type Manifest struct {
	// Format is ManifestFormat.
	Format    int       `json:"format"`
	CreatedAt time.Time `json:"createdAt"`
	// App is the integration type ("radarr") and AppVersion the version the *arr reported.
	App             string `json:"app"`
	AppVersion      string `json:"appVersion"`
	IntegrationID   int64  `json:"integrationId"`
	IntegrationName string `json:"integrationName"`
	JobID           int64  `json:"jobId"`
	// JobQueuedAt is when job JobID was queued; with the id it names the job that wrote the
	// version (ids start over with a new Bunkarr database).
	JobQueuedAt time.Time `json:"jobQueuedAt,omitzero"`
	// Method is MethodFolder or MethodHTTP.
	Method    string            `json:"method"`
	Backup    ManifestBackup    `json:"backup"`
	Zip       ManifestZip       `json:"zip"`
	Entries   []ZipEntry        `json:"entries"`
	Integrity ManifestIntegrity `json:"integrity"`
	// Sensitive is always true: the zip holds the *arr's secrets (S17).
	Sensitive bool `json:"sensitive"`
	// Warnings are the backup's warnings; a resumed job that finishes with the version reports
	// them again.
	Warnings []string `json:"warnings,omitempty"`
}

// ManifestBackup is the *arr's own description of the backup.
type ManifestBackup struct {
	Name string    `json:"name"`
	Type string    `json:"type"`
	Time time.Time `json:"time"`
}

// ManifestZip is the stored zip file.
type ManifestZip struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	// SHA256 is the lowercase hex sha256 of the zip.
	SHA256 string `json:"sha256"`
}

// ManifestIntegrity is the version's verification: the zip's structure and CRCs, and the
// database's quick_check.
type ManifestIntegrity struct {
	Zip      string `json:"zip"`
	Database string `json:"database"`
}

// Result is IntegrityOK when both checks passed, else IntegrityFailed.
func (i ManifestIntegrity) Result() string {
	if i.Zip == IntegrityOK && i.Database == IntegrityOK {
		return IntegrityOK
	}
	return IntegrityFailed
}

// Stats is an arr_backup job's stats JSON (design §12.4).
type Stats struct {
	DryRun bool `json:"dryRun"`
	// Method is FetchFolder or FetchHTTP.
	Method     string `json:"method,omitempty"`
	BackupName string `json:"backupName,omitempty"`
	BackupType string `json:"backupType,omitempty"`
	// ReusedScheduled is true when the *arr's own scheduled backup was copied (no command sent).
	ReusedScheduled bool `json:"reusedScheduled"`
	// Unchanged is true when that scheduled backup was already at the destination.
	Unchanged bool  `json:"unchanged"`
	Bytes     int64 `json:"bytes"`
	// Integrity is the version's result (ok or failed).
	Integrity  string `json:"integrity,omitempty"`
	SnapshotID int64  `json:"snapshotId,omitempty"`
	Path       string `json:"path,omitempty"`
	// Recovered counts versions an interrupted job had written completely and this job recorded.
	Recovered      int64 `json:"recovered,omitempty"`
	VersionsPruned int64 `json:"versionsPruned"`
	DurationMs     int64 `json:"durationMs"`
}
