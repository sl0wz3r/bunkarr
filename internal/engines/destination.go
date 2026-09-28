package engines

import (
	"log/slog"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines/bwlimit"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// Destination is the engine-neutral view of one destination row, built with its Secrets from the
// same row by destinations.SecretsFor (§4.3): a secret goes only where it was saved.
type Destination struct {
	ID     int64
	Name   string
	Engine Kind
	Kind   DestKind
	// Target is the resolved path of a local destination, or a remote's display location
	// (sftp://user@host:22/path, s3:<endpoint>/<bucket>/<prefix>, b2:<bucket>/<prefix>).
	Target string
	Remote Remote
	// MarkerID is "restic:<repository id>", the rclone marker's uuid, or "pending:<uuid>" while a
	// create has not finished (S25).
	MarkerID string
	// EngineTag is a restic destination's random tag (D32).
	EngineTag  string
	Encryption EncryptionMode
	// FSType is a local destination's filesystem type (a local restic repository compares it,
	// S25), or the kind of a remote one.
	FSType string
	// Transfers is settings.transfers (1-32): rclone --transfers, restic rclone.connections.
	Transfers int
	// PackSizeMiB is settings.restic.packSizeMiB.
	PackSizeMiB int
	Bandwidth   bwlimit.Config
}

// Runtime is what one job hands an engine session: the job, its reporter, the exec runner and
// run directories, the transfer window and the retry budget (§3.1, S26, S27).
type Runtime struct {
	JobID    int64
	DryRun   bool
	Reporter jobs.Reporter
	Runner   proc.Runner
	RunDirs  *proc.RunDirs
	Window   Window
	// Now is the job's clock (time.Now when nil; tests set it).
	Now func() time.Time
	// Location is the container's time zone for windows, timetables and retention buckets.
	Location *time.Location
	Log      *slog.Logger
	// RetryBudget is engines.retryBudgetMinutes (default 10 min; 20 s for Test and Create).
	RetryBudget time.Duration
	// HostName is this container's host name and ProcessStart this process's start, for the
	// restic lock inspection (§6.7).
	HostName     string
	ProcessStart time.Time
}

// Clock returns rt.Now, or time.Now when it is nil.
func (rt Runtime) Clock() time.Time {
	if rt.Now != nil {
		return rt.Now()
	}
	return time.Now()
}
