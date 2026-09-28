// Package enginerun runs the sync, verify and retention jobs of restic and rclone destinations
// (docs/design/phase4.md §3.4, §6, §7, §9.2, §11): Dispatch sends each job of those types to the
// Phase 1-3 syncer runners (filecopy destinations, a destination that is gone, the global
// retention job) or to this package's engine runners, which plan with the shared syncer.Planner
// and execute through the drivers of internal/engines/restic and internal/engines/rclone. It owns
// the tables engine_snapshots (restic snapshots of sources), engine_forget (forget requests of
// the config runners) and engine_state (prune, check and cleanup times, the check subset
// rotation, the measured throughput), and implements engines.Engine for both drivers (Engines)
// and engines.VersionOpener (OpenVersions) for the config runners.
//
// The safety rules it carries out:
//   - S21: no job but a dry run runs before the destination's recovery kit custody is confirmed
//     ("export and confirm the recovery kit first"), nor on a pending create or an unavailable
//     engine;
//   - S25: every job first checks the destination's identity read-only (restic cat config with
//     --no-lock, the rclone marker) through the driver, again every 20 batches and before
//     retention work; netguard checks every host the engine dials at job start;
//   - S5, S6, S7, D31 (restic): a batch counts only through the read-back of its snapshot's
//     content (restic ls): items are done when the snapshot holds their file with the plan's size
//     and mtime, every other live record keeps (or gets) a reference to a snapshot that holds its
//     version, updates keep their old version as a replaced row, and retains wait for their
//     folder's new files (restic_record.go);
//   - S2, S23 (rclone): every command names validated paths under the destination root, copies
//     move replaced objects into the job's retention directory (--backup-dir), every move target
//     is listed first and an occupied one displaced, intents precede moves, the after-listing
//     decides every outcome, and the reconciliation of interrupted moves never deletes
//     (rclone_*.go);
//   - S24, D28 (restic retention): forgets only by id, of snapshots restic.KeepMedia does not
//     keep and of requests restic.FilterForget accepts, both recomputed from a listing taken right
//     before each forget; unlock only guarded and right before an exclusive command, never in a
//     dry run or a sync;
//   - S9: a dry run plans (its items are the preview) and checks the identity, and writes nothing
//     to the destination;
//   - S27: jobs start only inside the transfer window (jobs.Defer otherwise), stop cleanly at its
//     end (restic SIGINT, rclone --max-duration and SIGINT) with the stopped items pending, and a
//     file that cannot fit a whole window fails its item instead of deferring forever;
//   - S22: every child line is redacted by the exec layer with the destination's secrets, and
//     the job's own log lines are redacted with them too.
//
// Fault points: engine.beforeBatch, engine.afterBatchExit, engine.afterRecord,
// rclone.afterIntent, rclone.afterMove, rclone.afterStat and restic.afterForget (the crash
// matrices of §14.3). The unit tests run against enginetest.FakeRunner with a stateful fake
// repository and object store; the real-binary tests (build tag enginebin) run in containers:
//
//	make test-engines                                        (every engine package)
//	ENGINES_PACKAGES=./internal/enginerun/... sh docker/test-engines.sh
package enginerun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
	"github.com/sl0wz3r/bunkarr/internal/engines/rclone"
	"github.com/sl0wz3r/bunkarr/internal/engines/restic"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/netguard"
	"github.com/sl0wz3r/bunkarr/internal/syncer"
)

// Fault points of this package (internal/faultinject, the crash matrices of §14.3).
const (
	// PointBeforeBatch: a batch is about to run (restic backup, rclone copy).
	PointBeforeBatch = "engine.beforeBatch"
	// PointAfterBatchExit: the engine finished a batch, nothing of it is recorded.
	PointAfterBatchExit = "engine.afterBatchExit"
	// PointAfterRecord: a batch is recorded, its items are not finished.
	PointAfterRecord = "engine.afterRecord"
	// PointRcloneAfterIntent: retention intents are recorded, the move has not run.
	PointRcloneAfterIntent = "rclone.afterIntent"
	// PointRcloneAfterMove: a server-side move ran (on S3 a copy then a delete: the crash matrix's
	// fake leaves both objects here), nothing is listed or recorded.
	PointRcloneAfterMove = "rclone.afterMove"
	// PointRcloneAfterStat: the after-listing of a batch or move is done, nothing is recorded.
	PointRcloneAfterStat = "rclone.afterStat"
	// PointResticAfterForget: a forget chunk succeeded, its rows and requests are not updated.
	PointResticAfterForget = "restic.afterForget"
)

// Defaults of the engine runners.
const (
	// recheckBatches is how many batches run between two identity checks (S25).
	recheckBatches = 20
	// pendingPage is how many pending items are read at a time.
	pendingPage = 1000
	// planPage is how many items are persisted per AddItems call.
	planPage = 1000
)

// FilecopyRunners are the Phase 1-3 runners of internal/syncer, which Dispatch keeps for
// filecopy destinations, a destination that no longer exists and the global retention job.
type FilecopyRunners struct {
	Sync, Verify, Retention jobs.Runner
}

// Options wires a Service.
type Options struct {
	DB *db.DB
	// Files is the destination_files store (nil: syncer.NewStore(DB)).
	Files *syncer.Store
	// Planner is the planner shared with the filecopy runner (nil: one is made from the other
	// fields, with Enqueuer and ManifestAfterSync for the manifest export after a full sync).
	Planner      *syncer.Planner
	Catalog      *catalog.Store
	Scanner      *catalog.Scanner
	Destinations *destinations.Store
	// Tiers decides tiers and holds irreplaceable files (nil: every file full, no flags).
	Tiers    syncer.Tiers
	Filecopy FilecopyRunners
	Restic   *restic.Driver
	Rclone   *rclone.Driver
	// Availability reports the engines discovery found (nil: both unavailable).
	Availability func() engines.Availability
	// ConfigDir is Bunkarr's config directory: restic excludes it (S28), verify restores into
	// <ConfigDir>/staging, and config versions are staged there.
	ConfigDir string
	// RunDirs and Runner run the engine commands (nil: the drivers').
	RunDirs *proc.RunDirs
	Runner  proc.Runner
	// Enqueuer and ManifestAfterSync queue the manifest export after a full sync (used when
	// Planner is nil).
	Enqueuer          jobs.Enqueuer
	ManifestAfterSync func(ctx context.Context, destinationID int64) (bool, error)
	// RetryBudget returns engines.retryBudgetMinutes (nil: the drivers' default of 10 minutes).
	RetryBudget func(ctx context.Context) time.Duration
	Logger      *slog.Logger
	Now         func() time.Time
	// Location is the container's time zone for windows, timetables and retention buckets (nil:
	// time.Local).
	Location *time.Location
	// HostName and ProcessStart identify this process's restic locks (§6.7).
	HostName     string
	ProcessStart time.Time

	// CheckHost refuses a host the engine would dial (nil: netguard.CheckHost).
	CheckHost func(ctx context.Context, host string) error
	// AliasDirs returns the directories of a source that a scan skipped as aliases of the config
	// directory or a destination target (S4), relative to the source root, for restic's exclude
	// file (nil: none are known; the absolute paths of the config directory and of every local
	// destination target are always excluded).
	AliasDirs func(ctx context.Context, src catalog.Source) ([]string, error)
	// ConfigRefs returns the restic snapshots the config version rows of a destination reference
	// (snapshots.engine_ref, manifests.engine_ref), which retention never forgets (S24). nil reads
	// both columns directly.
	ConfigRefs func(ctx context.Context, destinationID int64) ([]string, error)
	// LiveIntegrations returns the ids of the integrations that exist (a Plex DB or *arr snapshot
	// group of another id is an orphan group, kept until a user action). nil reads them directly.
	LiveIntegrations func(ctx context.Context) (map[int64]bool, error)
}

// Service runs engine jobs and serves the engine operations of the API and the config runners.
// It is safe for concurrent use.
type Service struct {
	o       Options
	files   *syncer.Store
	planner *syncer.Planner
	log     *slog.Logger
	now     func() time.Time
	loc     *time.Location
}

// New returns a Service.
func New(o Options) *Service {
	s := &Service{o: o, files: o.Files, planner: o.Planner, log: o.Logger, now: o.Now, loc: o.Location}
	if s.files == nil {
		s.files = syncer.NewStore(o.DB)
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.loc == nil {
		s.loc = time.Local
	}
	if s.planner == nil {
		s.planner = syncer.NewPlanner(syncer.Options{DB: o.DB, Store: s.files, Catalog: o.Catalog, Scanner: o.Scanner,
			Destinations: o.Destinations, Tiers: o.Tiers, Enqueuer: o.Enqueuer, ManifestAfterSync: o.ManifestAfterSync,
			Logger: o.Logger, Now: s.now, Location: s.loc})
	}
	if s.o.CheckHost == nil {
		s.o.CheckHost = netguard.CheckHost
	}
	return s
}

// stagingDir is <config>/staging.
func (s *Service) stagingDir() string { return filepath.Join(s.o.ConfigDir, "staging") }

func (s *Service) availability() engines.Availability {
	if s.o.Availability == nil {
		return engines.Availability{Restic: engines.BinaryStatus{Reason: "restic is not installed"},
			Rclone: engines.BinaryStatus{Reason: "rclone is not installed"}}
	}
	return s.o.Availability()
}

func (s *Service) retryBudget(ctx context.Context) time.Duration {
	if s.o.RetryBudget == nil {
		return 0
	}
	return s.o.RetryBudget(ctx)
}

// Dispatch returns the runner of job type t (sync, verify or retention): it reads the job's
// destination and runs the syncer's runner for a filecopy destination, a destination that is gone
// (its Phase 1 error path) and the global retention job (DestinationID 0), and this package's
// runner for a restic or rclone destination (§3.4). The engine of a destination never changes.
func (s *Service) Dispatch(t jobs.Type) jobs.Runner {
	return jobs.RunnerFunc(func(ctx context.Context, job jobs.Job, env jobs.Env) (jobs.Result, error) {
		kind, err := s.engineOf(ctx, job)
		if err != nil {
			return jobs.Result{}, err
		}
		if kind == engines.Restic || kind == engines.Rclone {
			switch t {
			case jobs.TypeSync:
				return s.runSync(ctx, kind, job, env)
			case jobs.TypeVerify:
				return s.runVerify(ctx, kind, job, env)
			case jobs.TypeRetention:
				return s.runRetention(ctx, kind, job, env)
			}
			return jobs.Result{}, fmt.Errorf("enginerun: no %s runner for engine destinations", t)
		}
		fc := s.filecopyRunner(t)
		if fc == nil {
			return jobs.Result{}, fmt.Errorf("enginerun: no filecopy runner for %s jobs", t)
		}
		return fc.Run(ctx, job, env)
	})
}

func (s *Service) filecopyRunner(t jobs.Type) jobs.Runner {
	switch t {
	case jobs.TypeSync:
		return s.o.Filecopy.Sync
	case jobs.TypeVerify:
		return s.o.Filecopy.Verify
	case jobs.TypeRetention:
		return s.o.Filecopy.Retention
	}
	return nil
}

// engineOf returns the engine of the job's destination: filecopy for no destination, a missing
// one and a filecopy row.
func (s *Service) engineOf(ctx context.Context, job jobs.Job) (engines.Kind, error) {
	id := job.Params.DestinationID
	if id == 0 || s.o.Destinations == nil {
		return engines.Filecopy, nil
	}
	d, err := s.o.Destinations.Get(ctx, id)
	if errors.Is(err, destinations.ErrNotFound) {
		return engines.Filecopy, nil
	}
	if err != nil {
		return "", fmt.Errorf("%s: %w", job.Type, err)
	}
	switch k := engines.Kind(d.Engine); k {
	case engines.Restic, engines.Rclone:
		return k, nil
	}
	return engines.Filecopy, nil
}

// Engines returns the engine registry the destinations store creates, tests and attaches
// through: the restic and rclone engines whose binaries are available.
func (s *Service) Engines() func(engines.Kind) (engines.Engine, bool) {
	return func(k engines.Kind) (engines.Engine, bool) {
		if s.availability().Check(k) != nil {
			return nil, false
		}
		switch {
		case k == engines.Restic && s.o.Restic != nil:
			return &resticEngine{s: s}, true
		case k == engines.Rclone && s.o.Rclone != nil:
			return &rcloneEngine{s: s}, true
		}
		return nil, false
	}
}
