// Package syncer mirrors sources to filecopy destinations (design docs/design/phase1.md §4): it
// owns the destination_files table (what Bunkarr put at each destination), plans a sync by
// diffing the catalog against those records, guards the plan (mass-change hold S10b, names the
// destination cannot store S11, free space), and executes it with the filecopy engine. It also
// runs the verify (§4.5) and retention jobs.
//
// Safety rules that live here:
//   - S2: every destination operation goes through the job's destinations.Handle root; an
//     unrecorded file in the way is adopted (§4.4) or displaced into retention, never replaced.
//   - S5/S6: a file that disappears from a source is moved into retention and expired only by a
//     retention job after the destination's deletedDays; every copy, update, move and link runs
//     before any retain, an update's new version is complete and verified before the old one
//     moves to retention, and a retain waits while a file of its source folder is not backed up
//     (a Radarr upgrade whose new file failed to copy keeps the old version live).
//   - S7: every write is a temp file whose path is persisted in the item detail before it exists;
//     a resumed item removes or finishes it.
//   - S9: a dry run scans, plans and guards, persists the plan as items and writes nothing to the
//     destination.
//   - S10b/S11: see guards.go.
//   - D14 (phase2-3.md §9.1): a targeted sync (Params.Paths, the webhook path) scans and plans
//     only its paths, and retains a vanished name only when the name's folder gets new content in
//     the same plan (an upgrade or a rename); every other vanished name stays live and recorded
//     until the next untargeted sync, whose mass-change guard sees the whole change (targeted.go).
//
// Records whose source is no longer linked to the destination (or was deleted) are orphans: a
// sync never retains, moves or modifies them.
//
// Every executed step is idempotent. Effects are ordered filesystem → destination_files →
// ItemStore.Finish, and each item re-checks the source, the destination and its own detail when
// it runs, so a job resumed after a crash at any faultinject point converges (see the crash
// matrix in crash_test.go). A file with a record is renamed (or hardlinked) into retention only
// after the record holds a retention intent (store.go); what a job that is never resumed left
// half done is settled by the next sync, verify or retention job of the destination
// (reconcile.go). Whether two destination names are one file is decided by inode number only where
// the destination probe found inode numbers stable, else by content (identity.go: a CIFS mount with
// noserverino numbers inodes per lookup); a sync probes a destination whose capabilities come from
// an older probe again before it starts. Every sync that completes writes the hardlink manifest
// (manifest.go). Fault points of this package: plan.afterBatch,
// update.afterLinkOld, update.afterRenameOld, update.afterRenameNew, displace.afterRename,
// retain.afterRename, promote.afterRename, adopt.afterSetMtime, record.afterFS, record.afterDB,
// verify.afterMark; the filecopy engine adds copy.*, link.afterLink, move.afterRename and
// expire.afterRemove.
package syncer

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// Fault-injection points of this package (internal/faultinject).
const (
	// PointPlanAfterBatch: a batch of planned items is persisted, the plan is not complete.
	PointPlanAfterBatch = "plan.afterBatch"
	// PointUpdateAfterLinkOld: the old version is hardlinked into retention, the new one is not
	// in place yet.
	PointUpdateAfterLinkOld = "update.afterLinkOld"
	// PointUpdateAfterRenameOld: the old version is renamed into retention (no hardlinks), the
	// final name is free until the new version is renamed there.
	PointUpdateAfterRenameOld = "update.afterRenameOld"
	// PointUpdateAfterRenameNew: the new version is in place, nothing is recorded yet.
	PointUpdateAfterRenameNew = "update.afterRenameNew"
	// PointDisplaceAfterRename: an unmanaged file was moved into retention, not yet recorded.
	PointDisplaceAfterRename = "displace.afterRename"
	// PointRetainAfterRename: a vanished (or relinked) name was moved into retention, not yet
	// recorded.
	PointRetainAfterRename = "retain.afterRename"
	// PointPromoteAfterRename: a primary's file was renamed to its dependent's path, not yet
	// recorded.
	PointPromoteAfterRename = "promote.afterRename"
	// PointAdoptAfterSetMtime: an adopted file (size+hash) got the source's mtime, not yet
	// recorded.
	PointAdoptAfterSetMtime = "adopt.afterSetMtime"
	// PointRecordAfterFS: an item's filesystem work is done, destination_files is not updated.
	PointRecordAfterFS = "record.afterFS"
	// PointRecordAfterDB: destination_files is updated, the item is not finished.
	PointRecordAfterDB = "record.afterDB"
	// PointVerifyAfterMark: verify marked a record missing, the item is not finished.
	PointVerifyAfterMark = "verify.afterMark"
)

// Defaults of the runners.
const (
	// recheckEvery is how many items run between two destination marker re-checks (S3).
	recheckEvery = 500
	// planBatch is how many items are persisted per AddItems call while planning.
	planBatch = 1000
	// pendingBatch is how many pending items are read at a time while executing.
	pendingBatch = 500
	// freeSpaceReserve is kept free at the destination (design §4.1 guards).
	freeSpaceReserve = 1 << 30
	// defaultHistoryDays is how long finished jobs are kept when no setting says otherwise.
	defaultHistoryDays = 90
)

// Options wires the runners to the stores they use.
type Options struct {
	// DB is Bunkarr's database.
	DB *db.DB
	// Store is the destination_files store; nil means NewStore(DB).
	Store *Store
	// Catalog and Scanner are the source catalog and its scanner (a sync scans its sources).
	Catalog *catalog.Store
	Scanner *catalog.Scanner
	// Destinations opens destinations (S3 checks) and reads their settings.
	Destinations *destinations.Store
	// Logger receives server-side log lines; nil discards them.
	Logger *slog.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Enqueuer queues the manifest export that follows a full sync (phase2-3.md §9.2); nil
	// queues none.
	Enqueuer jobs.Enqueuer
	// ManifestAfterSync reports whether a sync of the destination that is neither a dry run nor
	// targeted is followed by a manifest export (the destination's manifest.afterSync; auto: when
	// an enabled *arr integration exists). nil means never.
	ManifestAfterSync func(ctx context.Context, destinationID int64) (bool, error)
	// ExpectedFiles returns the files the *arr index expects under the paths of a webhook sync
	// of src (phase2-3.md §9.1 "expected files"); nil checks none.
	ExpectedFiles func(ctx context.Context, src catalog.Source, paths []string) ([]ExpectedFile, error)
}

// base is what every runner shares.
type base struct {
	store *Store
	cat   *catalog.Store
	scan  *catalog.Scanner
	dests *destinations.Store
	log   *slog.Logger
	now   func() time.Time

	enq           jobs.Enqueuer
	manifestAfter func(ctx context.Context, destinationID int64) (bool, error)
	expected      func(ctx context.Context, src catalog.Source, paths []string) ([]ExpectedFile, error)
}

func newBase(o Options) base {
	b := base{store: o.Store, cat: o.Catalog, scan: o.Scanner, dests: o.Destinations, log: o.Logger, now: o.Now,
		enq: o.Enqueuer, manifestAfter: o.ManifestAfterSync, expected: o.ExpectedFiles}
	if b.store == nil {
		b.store = NewStore(o.DB)
	}
	if b.log == nil {
		b.log = slog.New(slog.DiscardHandler)
	}
	if b.now == nil {
		b.now = time.Now
	}
	return b
}

// nopReporter is used when a job has no reporter (tests).
type nopReporter struct{}

func (nopReporter) Progress(jobs.Progress)         {}
func (nopReporter) Log(slog.Level, string, ...any) {}

func reporterOf(env jobs.Env) jobs.Reporter {
	if env.Reporter == nil {
		return nopReporter{}
	}
	return env.Reporter
}

// formatBytes renders n in binary units (1.5 GiB).
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// plural returns "1 file" / "2 files".
func plural(n int64, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
