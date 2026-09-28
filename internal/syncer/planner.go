package syncer

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// The shared planner (docs/design/phase4.md §3.3, D17). Every engine plans a sync with the Phase
// 1-3 path: select the sources, scan each under its lock, load the tier decisions, plan the source
// (moves by head/tail hash, links, promotes, retains, releases, D14 for targeted syncs), apply the
// S10(b) mass-change guard and the S11 name checks, and persist the items with planned_at; a job
// whose plan is complete resumes it instead. What differs per engine is what the planner reads of
// the destination: an engines.PlanFS (head/tail hashes and stats of destination paths), the
// destination's capabilities, and its free space. The filecopy SyncRunner hands it the job's
// os.Root (rootPlanFS); the restic and rclone runners (internal/enginerun) their session's
// PlanFS. The planner reads each source through its own os.Root.

// Planner plans sync jobs for every engine (§3.3). SyncRunner embeds one; internal/enginerun
// makes its own with NewPlanner and calls Plan, then executes the persisted items itself.
type Planner struct {
	base
	planBatch int
	// expectedWaits and sleep pace the rescans of a webhook sync's expected files; tests shorten
	// them.
	expectedWaits []time.Duration
	sleep         func(ctx context.Context, d time.Duration) error
}

// NewPlanner returns a planner over the stores of o (Store, Catalog, Scanner, Destinations, Tiers,
// ExpectedFiles; Enqueuer and ManifestAfterSync for QueueManifestExport).
func NewPlanner(o Options) *Planner {
	return &Planner{base: newBase(o), planBatch: planBatch, expectedWaits: defaultExpectedWaits, sleep: sleepCtx}
}

// PlanDestination is what the planner needs to know of the destination of a sync.
type PlanDestination struct {
	ID   int64
	Name string
	// SourceIDs are the sources linked to the destination (destinations.Destination.SourceIDs).
	SourceIDs []int64
	// Settings are the destination's settings (adoption, hardlink mode, mass-change guard).
	Settings destinations.Settings
	// Caps is what the planner assumes of the destination (§3.3: restic hardlinks true in
	// recreate mode, rclone hardlinks false; case-sensitive; no invalid characters).
	Caps filecopy.Capabilities
}

// PlanOptions are the per-engine planning choices.
type PlanOptions struct {
	// UpdateKept plans a kept file (S15: not full here, with a live record) whose source changed as
	// an update instead of keeping the old version (restic, D29); its bytes count in bytesKept.
	UpdateKept bool
}

// PlanInput is one sync attempt's request to the planner.
type PlanInput struct {
	Job jobs.Job
	// Env is the job's environment (its item store and reporter).
	Env  jobs.Env
	Dest PlanDestination
	// FS is the destination side the planner reads (never writes). engines.ErrNoHeadTail from
	// DestHeadTail makes the move pairing skip that pair: the rename becomes copy + retain.
	FS engines.PlanFS
	// FreeSpace returns the destination's free bytes; known false skips the free-space check
	// (remote destinations). An error fails a real run and warns in a dry run, as on filecopy.
	// nil skips the check.
	FreeSpace func() (free int64, known bool, err error)
	Options   PlanOptions
}

// Plan is one sync attempt's plan: the sources it covers, what the planner found, and the tier
// and release bookkeeping the execution and the stats need. The items themselves are persisted in
// the job's item store. A Plan is used by one goroutine.
type Plan struct {
	s *syncRun
}

// Plan selects the job's sources and plans them (or, when the job's plan is complete, resumes
// it: the unknown-promoted copies are held again when they do not fit). A dry run's plan is its
// preview. The free-space check fails a real run whose copies do not fit (a dry run warns).
func (p *Planner) Plan(ctx context.Context, in PlanInput) (*Plan, error) {
	if in.Env.Items == nil {
		return nil, fmt.Errorf("plan: the job has no item store")
	}
	if in.FS == nil {
		return nil, fmt.Errorf("plan: no destination view")
	}
	s := &syncRun{r: p, job: in.Job, env: in.Env, rep: reporterOf(in.Env), linked: map[int64]catalog.Source{}, roots: map[int64]*os.Root{},
		dest: in.Dest, destFS: in.FS, free: in.FreeSpace, opts: in.Options}
	if s.free == nil {
		s.free = func() (int64, bool, error) { return 0, false, nil }
	}
	if err := s.prepare(ctx); err != nil {
		return nil, err
	}
	return &Plan{s: s}, nil
}

// prepare selects the sources and plans them, or resumes a complete plan (design §4.1 steps 2-4;
// shared by every engine).
func (s *syncRun) prepare(ctx context.Context) error {
	if err := s.selectSources(ctx); err != nil {
		return err
	}
	planned, err := s.env.Items.Planned(ctx, s.job.ID)
	if err != nil {
		return err
	}
	if planned {
		s.rep.Log(slog.LevelInfo, "the plan is complete: continuing with its pending items")
		return s.recheckUnknownHold(ctx)
	}
	// A job with items but no complete plan was stopped while planning: plan again.
	if err := s.env.Items.DeleteItems(ctx, s.job.ID); err != nil {
		return err
	}
	if err := s.prepareRelease(ctx); err != nil {
		return err
	}
	if err := s.scanAndPlan(ctx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	s.planned = true
	s.movedToNonFull()
	s.staleReleases()
	if err := s.checkFreeSpace(ctx); err != nil {
		if !s.job.DryRun {
			return err
		}
		s.warnings++
		s.rep.Log(slog.LevelWarn, err.Error())
	}
	return nil
}

// Sources returns the sources the plan covers (linked, enabled, and as the scan saw them).
func (pl *Plan) Sources() []catalog.Source { return slices.Clone(pl.s.sources) }

// Source returns a source the plan covers.
func (pl *Plan) Source(id int64) (catalog.Source, bool) {
	src, ok := pl.s.linked[id]
	return src, ok
}

// Planned reports whether this attempt planned (false: it resumes a complete plan).
func (pl *Plan) Planned() bool { return pl.s.planned }

// Targeted reports whether the job is a targeted sync (Params.Paths, D14).
func (pl *Plan) Targeted() bool { return pl.s.targeted() }

// Summaries returns each planned source's scan and plan summary.
func (pl *Plan) Summaries() []SourceSummary { return slices.Clone(pl.s.summaries) }

// BytesPlanned is the bytes the plan copies or updates (this attempt's plan).
func (pl *Plan) BytesPlanned() int64 { return pl.s.bytesPlanned }

// Warnings returns the warnings counted so far (the scan's, the planner's, the guard's).
func (pl *Plan) Warnings() int { return pl.s.warnings }

// AddWarnings adds n warnings (the executor's) to the job's count.
func (pl *Plan) AddWarnings(n int) { pl.s.warnings += n }

// AddDisplaced counts unmanaged objects moved out of the way (rclone displaced, §7.3).
func (pl *Plan) AddDisplaced(n int64) { pl.s.displaced += n }

// Reclassify records that a done item of action from had the outcome of action to (an rclone
// copy that found the object already there is adopted).
func (pl *Plan) Reclassify(from, to jobs.ItemAction) { pl.s.reclassify(from, to) }

// NotBackedUp returns a live catalog file in the folder of sourceRel that is not backed up (no
// live record with its size and mtime that holds content), or "": the S6 retain wait of
// phase1.md and phase2-3.md. A retain or release of sourceRel waits while one exists (a file
// this planning attempt decided is not full does not block, S6 amended). The records are read
// once per source and attempt, so call it after every content item ran (§6.2 step 6, §7.3 step 5).
func (pl *Plan) NotBackedUp(ctx context.Context, sourceID int64, sourceRel string) (string, error) {
	return pl.s.notBackedUp(ctx, sourceID, sourceRel)
}

// CheckRelease re-checks a release item (a retain with reason released) when it runs (§8.5): the
// rules must still be at tierRevision (Detail.TierRevision) and the file still live and not full.
// It returns why to skip the item ("" to release).
func (pl *Plan) CheckRelease(ctx context.Context, rec Record, tierRevision int64) (string, error) {
	return pl.s.checkRelease(ctx, rec, tierRevision)
}

// FlagsAfterSync lets the folder flags follow the moves the job executed (phase2-3.md §8.7): an
// engine runner calls it after every attempt that executed, also one that failed.
func (pl *Plan) FlagsAfterSync(ctx context.Context) { pl.s.flagsAfterSync(ctx) }

// Stats computes the job's Phase 1-3 sync stats from its persisted items (so they cover earlier
// attempts), with the warnings count and the one-sentence summary. started is when the attempt
// started.
func (pl *Plan) Stats(ctx context.Context, started time.Time) (SyncStats, int, string, error) {
	res, err := pl.s.result(ctx, started)
	if err != nil {
		return SyncStats{}, 0, "", err
	}
	st, _ := res.Stats.(SyncStats)
	return st, res.Warnings, res.Summary, nil
}

// Result is Stats as a jobs.Result.
func (pl *Plan) Result(ctx context.Context, started time.Time) (jobs.Result, error) {
	return pl.s.result(ctx, started)
}

// SyncSummary is the one-sentence summary of sync stats ("Copied 3, retained 1 (1.2 GiB copied)").
func SyncSummary(st SyncStats) string { return syncSummary(st) }

// QueueManifestExport queues the manifest export that follows a sync that is neither a dry run nor
// targeted and was not cancelled, when Options.ManifestAfterSync allows it (phase2-3.md §9.2),
// also when the sync failed: the manifest is the only protection of the files that are not
// copied. It returns the queued job's id (0: none), which the caller puts in
// SyncStats.ManifestExportJob.
func (p *Planner) QueueManifestExport(ctx context.Context, job jobs.Job, runErr error) int64 {
	return p.queueManifestExport(ctx, job, runErr)
}

// FollowMoves lets the folder flags follow the moves job jobID executed (a no-op for moves already
// followed); it returns the number of warnings it logged. SyncRunner.OnJobFinish calls it for
// every sync that ended failed or cancelled, whatever its engine.
func (p *Planner) FollowMoves(ctx context.Context, jobID int64, rep jobs.Reporter) int {
	return p.followMoves(ctx, jobID, rep)
}

// ParseDetail decodes an item's detail (the planner's Detail JSON).
func ParseDetail(it jobs.Item) (Detail, error) { return parseDetail(it) }

// Raw encodes the detail for jobs.ItemStore.SetDetail.
func (d Detail) Raw() json.RawMessage { return d.raw() }
