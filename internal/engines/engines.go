// Package engines is the engine-neutral contract of Phase 4's backup engines
// (docs/design/phase4.md §3.1, §4, §8.1, §9, §10.1): the Engine and Session interfaces that
// internal/engines/restic and internal/engines/rclone implement, the destination, remote, secret
// and runtime types they are handed, the config-version store (VersionStore), the transfer window
// of a job (Window) and the discovery of the engine binaries (Discover). Filecopy stays the Phase
// 1 code and is not an Engine (§3.2).
//
// The package serves these safety rules:
//   - S21 and S22: Credentials, EncryptionSecret and Secrets never show a secret through String,
//     GoString, slog or JSON, and Secrets.Values lists every form of every secret (clear,
//     rclone-obscured, JSON-escaped) for the redaction of a command's output and argv;
//   - S25: Destination carries the marker id every Open checks, DialHosts names the hosts netguard
//     checks, and the sentinel errors carry the exact user texts of a failed identity check;
//   - S26: Runtime hands a job's exec runner, run directories and retry budget to the drivers;
//   - S27: Window is the transfer window a job defers at (with the BUNKARR_TEST_WINDOW hook of
//     e2e builds);
//   - S8 for engines: a Destination and its Secrets come from one row (destinations.SecretsFor),
//     and nothing here returns credentials without their location.
package engines

import (
	"context"
	"errors"
	"fmt"

	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
)

// Kind is a destination's engine.
type Kind string

// Engines. Filecopy is native (§3.2); Restic and Rclone are drivers for external binaries.
const (
	Filecopy Kind = "filecopy"
	Restic   Kind = "restic"
	Rclone   Kind = "rclone"
)

// DestKind is a destination's location type (§4.1).
type DestKind string

// Destination kinds.
const (
	Local DestKind = "local"
	SFTP  DestKind = "sftp"
	S3    DestKind = "s3"
	B2    DestKind = "b2"
)

// Valid reports whether k is a known kind.
func (k DestKind) Valid() bool {
	switch k {
	case Local, SFTP, S3, B2:
		return true
	}
	return false
}

// Remote reports whether k is an off-site kind (sftp, s3, b2): encrypted by default (S21) and
// under the fresh-password rule (S29).
func (k DestKind) Remote() bool { return k == SFTP || k == S3 || k == B2 }

// EncryptionMode is how a destination's data is encrypted (§5.1).
type EncryptionMode string

// Encryption modes.
const (
	// EncryptionNone: plain files (filecopy; rclone only with acceptUnencrypted).
	EncryptionNone EncryptionMode = "none"
	// EncryptionRestic: the repository's own encryption.
	EncryptionRestic EncryptionMode = "restic"
	// EncryptionCrypt: an rclone crypt remote over the whole destination root.
	EncryptionCrypt EncryptionMode = "crypt"
)

// Sentinel errors of the engines. Wrappers may add detail; errors.Is identifies them. The texts are
// the user-facing messages of phase4.md (S25, §4.6).
var (
	// ErrRepositoryMissing: restic exit 10, or an rclone bucket or path that does not exist.
	ErrRepositoryMissing = errors.New("repository missing (share not mounted, or wrong bucket or path?)")
	// ErrWrongPassword: restic exit 12 (never retried).
	ErrWrongPassword = errors.New("wrong repository password")
	// ErrAnotherRepository: the repository id differs from the destination's marker_id.
	ErrAnotherRepository = errors.New("another repository at this location")
	// ErrMarkerMissing: an rclone destination's .bunkarr/destination.json is missing or does not
	// parse (also a wrong crypt password under strict_names).
	ErrMarkerMissing = errors.New("marker missing")
	// ErrMarkerMismatch: the rclone marker belongs to another destination.
	ErrMarkerMismatch = errors.New("destination marker does not match (another destination at this location?)")
	// ErrHostKeyChanged: the SFTP server presented a key that is not pinned.
	ErrHostKeyChanged = errors.New("SFTP host key changed")
	// ErrLocked: restic exit 11, or a lock that may belong to a live process (§6.7).
	ErrLocked = errors.New("repository is locked by another process")
	// ErrPending: the destination's create stopped after restic init (marker_id "pending:…"); no
	// job runs until the create is finished (§4.5).
	ErrPending = errors.New("destination creation did not finish")
	// ErrEngineUnavailable: the engine's binary is missing, too old or unsafe (§4.4, §10.1).
	ErrEngineUnavailable = errors.New("engine unavailable")
	// ErrNoHeadTail: PlanFS has no head/tail hash for a record; the planner treats it as an item
	// error, so a rename becomes copy + retain (§3.3).
	ErrNoHeadTail = errors.New("no head/tail hash recorded")
)

// Engine is a destination's backup engine (spec §8: Plan, Run, Verify, List; Restore is Phase 5).
// restic and rclone implement it in internal/engines/restic and internal/engines/rclone.
type Engine interface {
	Kind() Kind
	// Open checks the destination's identity (S25) and returns a session for one job. It writes
	// nothing, so a dry run uses it too.
	Open(ctx context.Context, d Destination, s Secrets, rt Runtime) (Session, error)
	// Create initializes a new destination (restic init, or the rclone marker) or attaches an
	// existing one (attach). Test probes a location and writes nothing (§4.5).
	Create(ctx context.Context, d Destination, s Secrets, attach bool) (CreateResult, error)
	Test(ctx context.Context, d Destination, s Secrets) (TestResult, error)
}

// CreateResult is the outcome of Engine.Create.
type CreateResult struct {
	// MarkerID is the destination's new marker_id ("restic:<repository id>", or the rclone
	// marker's uuid). With an error it is the id of an rclone marker the create wrote and could
	// not remove: the caller removes it (rclone.Driver.RemoveMarker) or keeps the pending row,
	// whose secret alone reads it under crypt.
	MarkerID string
	// Initialized reports that restic init succeeded. It is set also when Create returns an
	// error afterwards: the pending row must then stay, because its secret now opens a
	// repository (§4.5, crash point create.afterInit).
	Initialized bool
	// Warnings are shown with the created destination (a non-empty rclone remote, another
	// Bunkarr's recent snapshots on attach, an unrestricted B2 key).
	Warnings []string
}

// Session is one job's access to a destination, returned by Engine.Open. Run, Verify and Retain
// of §3.1 are methods of internal/enginerun's concrete sessions, because they need the syncer's
// record store; their semantics are those of §6 and §7.
type Session interface {
	// Caps is what the planner assumes of the destination (§3.3).
	Caps() filecopy.Capabilities
	// PlanFS is the planner's view of the destination; it never writes.
	PlanFS() PlanFS
	// List returns the versions the destination holds: restic snapshots of sources, and
	// retention runs and config versions on both engines.
	List(ctx context.Context, f ListFilter) ([]Version, error)
	// Versions stores config versions (§8).
	Versions() VersionStore
	Close() error
}

// PlanFS is the destination side of the shared planner (§3.3); the planner reads sources itself.
type PlanFS interface {
	// DestHeadTail returns the head/tail hash of the destination file at destRel (its record's
	// head_tail on engines), or ErrNoHeadTail.
	DestHeadTail(destRel string) (string, error)
	// DestStat stats a destination path; engines report nothing there (ok false): restic holds no
	// unmanaged file, and rclone moves an object in the way into retention itself (S23).
	DestStat(destRel string) (filecopy.Stat, bool, error)
}

// Repository states of TestResult.Repository (restic).
const (
	RepositoryMissing       = "missing"
	RepositoryExists        = "exists"
	RepositoryWrongPassword = "wrong-password"
	RepositoryLocked        = "locked"
)

// Marker states of TestResult.Marker (rclone).
const (
	MarkerMissing    = "missing"
	MarkerOK         = "ok"
	MarkerUnreadable = "unreadable"
	MarkerForeign    = "foreign"
)

// TestResult is the answer of a destination test (§4.5), serialized as the API's
// {ok, reachable, repository, marker, entries, hostKeys, freeBytes, engineVersion, message,
// warnings}.
type TestResult struct {
	OK        bool `json:"ok"`
	Reachable bool `json:"reachable"`
	// Repository (restic): missing, exists, wrong-password, locked, or "".
	Repository string `json:"repository,omitempty"`
	// Marker (rclone): missing, ok, unreadable, foreign, or "".
	Marker string `json:"marker,omitempty"`
	// ID is the repository id (restic) or the marker's id (rclone); MarkerName the marker's
	// destination name.
	ID         string `json:"id,omitempty"`
	MarkerName string `json:"markerName,omitempty"`
	// Entries counts the first 1000 top-level entries.
	Entries int `json:"entries"`
	// HostKeys are the SFTP server's presented keys when none are pinned yet (§4.6).
	HostKeys []HostKeyInfo `json:"hostKeys,omitempty"`
	// FreeBytes is nil when unknown.
	FreeBytes     *int64   `json:"freeBytes"`
	EngineVersion string   `json:"engineVersion,omitempty"`
	Message       string   `json:"message,omitempty"`
	Warnings      []string `json:"warnings,omitempty"`
}

// HostKeyInfo is one SFTP host key as the host-key scan presents it (§4.6).
type HostKeyInfo struct {
	Type        string `json:"type"`
	Fingerprint string `json:"fingerprint"`
	// Key is the base64 SSH wire format (HostKey.Key).
	Key string `json:"key"`
}

// Registry maps an engine kind to its driver; the wiring fills it at start-up with the engines
// whose binaries are available.
type Registry map[Kind]Engine

// Get returns the engine of kind k, or ErrEngineUnavailable.
func (r Registry) Get(k Kind) (Engine, error) {
	if e, ok := r[k]; ok && e != nil {
		return e, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrEngineUnavailable, k)
}
