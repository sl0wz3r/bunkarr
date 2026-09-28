package enginerun

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/restic"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/syncer"
)

// The retention job of a restic destination (§6.5, S24, D28):
//  1. preflight: custody, window, the identity (--no-lock); a dry run stops its repository access
//     there;
//  2. expire: retained rows past their expiry with the holds; an expired row is deleted, nothing
//     in the repository;
//  3. forget: the keep set per group (restic.KeepMedia) and the forget requests of the config
//     runners (restic.FilterForget), both recomputed from a listing taken right before each forget
//     chunk (S24); forget by id in chunks of 100, one expire item per snapshot; the guarded unlock
//     right before the first forget only;
//  4. prune when params.prune is set or pruneEveryDays elapsed, after a forget pass that
//     succeeded, then restic check (structure), each after a guarded unlock;
//  5. a dry run lists its expire items and the snapshots it would forget (from the recorded
//     snapshots): no forget, no prune, no unlock.

// resticRetention runs the retention job of a restic destination.
func (s *Service) resticRetention(ctx context.Context, r *jobRun) (jobs.Result, error) {
	repo, err := s.o.Restic.Connect(r.ed, r.sec, r.rt)
	if err != nil {
		return jobs.Result{}, fmt.Errorf("retention: %w", err)
	}
	if err := repo.CheckIdentity(ctx); err != nil {
		return jobs.Result{}, fmt.Errorf("retention: %w", err)
	}
	x := &resticRun{jobRun: r, repo: repo, baseGone: map[int64]bool{}}
	r.log(slog.LevelInfo, "retention started", "destination", r.d.Name, "engine", "restic", "deletedDays", r.d.Retention.DeletedDays)
	planned, err := r.env.Items.Planned(ctx, r.job.ID)
	if err != nil {
		return jobs.Result{}, err
	}
	var st RetentionStats
	if !planned {
		if err := r.env.Items.DeleteItems(ctx, r.job.ID); err != nil {
			return jobs.Result{}, err
		}
		items, err := r.planExpiry(ctx, false)
		if err != nil {
			return jobs.Result{}, err
		}
		if r.job.DryRun {
			forget, kept, err := x.dryRunForgets(ctx)
			if err != nil {
				return jobs.Result{}, err
			}
			items = append(items, forget...)
			st.SnapshotsKept = kept
		}
		if err := r.addItems(ctx, items, true); err != nil {
			return jobs.Result{}, err
		}
	}
	if !r.job.DryRun {
		if err := x.expireRows(ctx); err != nil {
			return jobs.Result{}, err
		}
		kept, requests, err := x.forget(ctx)
		if err != nil {
			return jobs.Result{}, err
		}
		st.SnapshotsKept, st.ForgetRequests = kept, requests
		if err := x.pruneIfDue(ctx, &st); err != nil {
			return jobs.Result{}, err
		}
	}
	base, warnings, err := r.retentionStats(ctx)
	if err != nil {
		return jobs.Result{}, err
	}
	base.SnapshotsKept, base.ForgetRequests, base.Pruned, base.PruneDurationMs = st.SnapshotsKept, st.ForgetRequests, st.Pruned,
		st.PruneDurationMs
	// Forget items are expire items too: the Phase 1 keys count rows, the engine keys snapshots.
	forgotten, ok, err := x.forgetItemCounts(ctx)
	if err != nil {
		return jobs.Result{}, err
	}
	if !ok {
		forgotten = forgetCounts{done: x.forgot}
	}
	base.SnapshotsForgotten = forgotten.done
	if r.job.DryRun {
		base.SnapshotsForgotten = forgotten.all // would forget
	}
	base.FilesPlanned -= forgotten.all
	base.FilesExpired -= forgotten.done
	base.FilesFailed -= forgotten.failed
	base.FilesKept -= forgotten.skipped
	return jobs.Result{Stats: base, Warnings: warnings, Summary: retentionSummary(base)}, nil
}

// expireRows runs the expire items of retained rows.
func (x *resticRun) expireRows(ctx context.Context) error {
	items, err := x.pending(ctx)
	if err != nil {
		return err
	}
	for _, it := range items {
		if it.Action != jobs.ActionExpire {
			continue
		}
		d, err := parseItem(it)
		if err != nil || d.Check != checkExpireRow {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if now := x.s.now(); !x.open(now) {
			return x.deferral(now)
		}
		if err := x.expireRow(ctx, it); err != nil {
			return err
		}
	}
	return nil
}

// keepInput builds restic.KeepInput from a listing and the database.
func (x *resticRun) keepInput(ctx context.Context, snaps []restic.Snapshot) (restic.KeepInput, error) {
	in := restic.KeepInput{Snapshots: snaps, Recorded: map[string]restic.Recorded{}, Refs: map[string]bool{},
		LiveSources: map[int64]bool{}, EngineTag: x.ed.EngineTag, Now: x.s.now(), Loc: x.s.loc}
	rows, err := x.s.snapshotRows(ctx, x.d.ID)
	if err != nil {
		return in, err
	}
	for _, r := range rows {
		in.Recorded[r.SnapshotID] = restic.Recorded{SourceID: r.SourceID, Complete: r.Complete, Batch: r.Batch}
	}
	in.Base = basesOf(rows)
	refs, err := x.s.files.Refs(ctx, x.d.ID)
	if err != nil {
		return in, err
	}
	cfg, err := x.s.configRefs(ctx, x.d.ID)
	if err != nil {
		return in, err
	}
	for _, r := range append(refs, cfg...) {
		in.Refs[r] = true
	}
	srcs, err := x.s.o.Catalog.List(ctx)
	if err != nil {
		return in, err
	}
	for _, s := range srcs {
		in.LiveSources[s.ID] = true
	}
	if in.LiveIntegrations, err = x.s.liveIntegrations(ctx); err != nil {
		return in, err
	}
	k := x.d.Retention.SnapshotKeep()
	in.Retention = restic.Retention{Daily: k.Daily, Weekly: k.Weekly, Monthly: k.Monthly, Yearly: k.Yearly}
	return in, nil
}

// configRefs returns the snapshots the config version rows of a destination reference.
func (s *Service) configRefs(ctx context.Context, destinationID int64) ([]string, error) {
	if s.o.ConfigRefs != nil {
		return s.o.ConfigRefs(ctx, destinationID)
	}
	rows, err := s.o.DB.Reader().QueryContext(ctx, `SELECT engine_ref FROM snapshots WHERE destination_id = ? AND engine_ref IS NOT NULL
		UNION SELECT engine_ref FROM manifests WHERE destination_id = ? AND engine_ref IS NOT NULL`, destinationID, destinationID)
	if err != nil {
		return nil, fmt.Errorf("read the config version references of destination %d: %w", destinationID, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// liveIntegrations returns the ids of the integrations that exist.
func (s *Service) liveIntegrations(ctx context.Context) (map[int64]bool, error) {
	if s.o.LiveIntegrations != nil {
		return s.o.LiveIntegrations(ctx)
	}
	rows, err := s.o.DB.Reader().QueryContext(ctx, `SELECT id FROM integrations`)
	if err != nil {
		return nil, fmt.Errorf("read the integrations: %w", err)
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// groupName renders a snapshot's group for an item's detail.
func groupName(g restic.Group) string {
	switch g.Kind {
	case engines.VersionMedia:
		return "media:source " + strconv.FormatInt(g.SourceID, 10)
	case engines.VersionPlexDB, engines.VersionArr:
		return g.Kind + ":integration " + strconv.FormatInt(g.IntegrationID, 10)
	}
	return g.Kind
}

// forgetItem is the expire item of one snapshot.
func (x *resticRun) forgetItem(s restic.Snapshot, reason string, request bool) jobs.Item {
	g, _ := restic.ParseGroup(s.Tags, x.ed.EngineTag)
	d := engineDetail{Detail: syncer.Detail{Reason: reason, Check: checkExpireSnapshot}, Snapshot: s.ID, Group: groupName(g), Request: request}
	return jobs.Item{RelPath: "snapshots/" + s.ID, Action: jobs.ActionExpire, Detail: d.raw()}
}

// dryRunForgets lists the snapshots the forget step would forget, from the recorded snapshots
// (a dry run reads nothing more from the repository than the identity, S9). The snapshots of a
// deleted source (source_id NULL) form orphan groups, which the forget keeps, and a request whose
// snapshot a record or config version references is dropped as the forget drops it; the checks
// that need the repository's listing (the newest config version of its group, another engine_tag)
// run only in the real job.
func (x *resticRun) dryRunForgets(ctx context.Context) ([]jobs.Item, int64, error) {
	rows, err := x.s.snapshotRows(ctx, x.d.ID)
	if err != nil {
		return nil, 0, err
	}
	var snaps []restic.Snapshot
	var orphans int64
	for _, r := range rows {
		if r.SourceID == 0 {
			orphans++
			continue
		}
		tags, err := restic.Tags(restic.TagInput{EngineTag: x.ed.EngineTag, Kind: engines.VersionMedia, JobID: max(r.JobID, 1),
			SourceID: r.SourceID, Batch: max(r.Batch, 1)})
		if err != nil {
			return nil, 0, err
		}
		snaps = append(snaps, restic.Snapshot{ID: r.SnapshotID, Time: r.CreatedAt, Tags: tags})
	}
	in, err := x.keepInput(ctx, snaps)
	if err != nil {
		return nil, 0, err
	}
	forget, keep := restic.KeepMedia(in)
	byID := map[string]restic.Snapshot{}
	for _, s := range snaps {
		byID[s.ID] = s
	}
	var items []jobs.Item
	for _, id := range forget {
		items = append(items, x.forgetItem(byID[id], "not kept by the retention", false))
	}
	reqs, err := x.s.forgetRequests(ctx, x.d.ID)
	if err != nil {
		return nil, 0, err
	}
	for _, r := range reqs {
		if in.Refs[r.SnapshotID] {
			continue
		}
		items = append(items, x.forgetItem(restic.Snapshot{ID: r.SnapshotID}, r.Reason, true))
	}
	return items, int64(len(keep)) + orphans, nil
}

// forget runs the forget step (§6.5 step 3). It returns how many snapshots the last listing kept
// and how many requests were pending.
func (x *resticRun) forget(ctx context.Context) (kept, requests int64, err error) {
	reqs, err := x.s.forgetRequests(ctx, x.d.ID)
	if err != nil {
		return 0, 0, err
	}
	requests = int64(len(reqs))
	snaps, err := x.repo.Snapshots(ctx, nil, nil)
	if err != nil {
		return 0, 0, err
	}
	in, err := x.keepInput(ctx, snaps)
	if err != nil {
		return 0, 0, err
	}
	forget, keep := restic.KeepMedia(in)
	kept = int64(len(keep))
	reqIDs := make([]string, 0, len(reqs))
	reqReason := map[string]string{}
	for _, r := range reqs {
		reqIDs = append(reqIDs, r.SnapshotID)
		reqReason[r.SnapshotID] = r.Reason
	}
	fr, dropped := restic.FilterForget(in, reqIDs)
	listed := map[string]restic.Snapshot{}
	for _, s := range snaps {
		listed[s.ID] = s
	}
	// Requests of snapshots that are gone are served.
	var gone []string
	for id, why := range dropped {
		if why == "not in the repository" {
			gone = append(gone, id)
		}
	}
	if len(gone) > 0 {
		if err := x.s.o.DB.Write(context.WithoutCancel(ctx), func(tx *sql.Tx) error {
			for _, id := range gone {
				if err := deleteForgetTx(ctx, tx, x.d.ID, id); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return 0, 0, err
		}
	}
	// One expire item per snapshot (a resumed job keeps its pending ones).
	pending, err := x.forgetPending(ctx)
	if err != nil {
		return 0, 0, err
	}
	var add []jobs.Item
	for _, id := range forget {
		if _, ok := pending[id]; !ok {
			add = append(add, x.forgetItem(listed[id], "not kept by the retention", false))
		}
	}
	for _, id := range fr {
		if _, ok := pending[id]; !ok {
			add = append(add, x.forgetItem(listed[id], reqReason[id], true))
		}
	}
	if len(add) > 0 {
		if err := x.addItems(ctx, add, false); err != nil {
			return 0, 0, err
		}
		if pending, err = x.forgetPending(ctx); err != nil {
			return 0, 0, err
		}
	}
	if len(pending) == 0 {
		return kept, requests, nil
	}
	ids := make([]string, 0, len(pending))
	for id := range pending {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	unlocked := false
	for chunk := range slices.Chunk(ids, restic.ForgetChunk) {
		if err := ctx.Err(); err != nil {
			return 0, 0, err
		}
		if now := x.s.now(); !x.open(now) {
			return 0, 0, x.deferral(now)
		}
		if err := x.forgetChunk(ctx, chunk, pending, &unlocked, &kept); err != nil {
			return 0, 0, err
		}
	}
	return kept, requests, nil
}

// forgetPending returns the job's pending forget items by snapshot.
func (x *resticRun) forgetPending(ctx context.Context) (map[string]jobs.Item, error) {
	items, err := x.pending(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]jobs.Item{}
	for _, it := range items {
		d, err := parseItem(it)
		if err == nil && it.Action == jobs.ActionExpire && d.Check == checkExpireSnapshot && d.Snapshot != "" {
			out[d.Snapshot] = it
		}
	}
	return out, nil
}

// forgetChunk forgets one chunk after the S24 re-check: the repository is listed again right
// before the forget, and every snapshot that became protected (the newest of a group, a base, a
// reference, another engine_tag, …) is dropped with its reason.
func (x *resticRun) forgetChunk(ctx context.Context, chunk []string, pending map[string]jobs.Item, unlocked *bool, kept *int64) error {
	snaps, err := x.repo.Snapshots(ctx, nil, nil)
	if err != nil {
		return err
	}
	in, err := x.keepInput(ctx, snaps)
	if err != nil {
		return err
	}
	forget, keep := restic.KeepMedia(in)
	*kept = int64(len(keep))
	var reqIDs []string
	for _, id := range chunk {
		if d, _ := parseItem(pending[id]); d.Request {
			reqIDs = append(reqIDs, id)
		}
	}
	fr, dropped := restic.FilterForget(in, reqIDs)
	allowed := map[string]bool{}
	for _, id := range append(forget, fr...) {
		allowed[id] = true
	}
	listed := map[string]bool{}
	for _, s := range snaps {
		listed[s.ID] = true
	}
	var ids []string
	for _, id := range chunk {
		it := pending[id]
		switch {
		case !listed[id]:
			if err := x.forgotten(ctx, []string{id}, pending); err != nil {
				return err
			}
		case !allowed[id]:
			why := keep[id]
			if why == "" {
				why = dropped[id]
			}
			if why == "" {
				why = "kept"
			}
			x.log(slog.LevelInfo, "a snapshot is kept after all (S24 re-check)", "snapshot", short(id), "reason", why)
			if err := x.finish(ctx, it.ID, jobs.ItemSkipped, 0, "kept: "+why); err != nil {
				return err
			}
		default:
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	if !*unlocked {
		// The identity again right before the first command that writes (S25), then the unlock.
		if err := x.repo.CheckIdentity(ctx); err != nil {
			return err
		}
		if err := x.repo.GuardedUnlock(ctx, x.rt.HostName, x.rt.ProcessStart, nil); err != nil {
			return err
		}
		*unlocked = true
	}
	done, err := x.repo.Forget(ctx, ids)
	if err != nil {
		x.releaseLock(ctx, err)
	}
	if len(done) > 0 {
		faultinject.Point(PointResticAfterForget)
		if rerr := x.forgotten(ctx, done, pending); rerr != nil {
			return rerr
		}
	}
	return err
}

// forgotten records forgotten (or already gone) snapshots: their rows and requests are deleted and
// their items finished.
func (x *resticRun) forgotten(ctx context.Context, ids []string, pending map[string]jobs.Item) error {
	err := x.s.o.DB.Write(context.WithoutCancel(ctx), func(tx *sql.Tx) error {
		for _, id := range ids {
			if err := deleteSnapshotRowTx(ctx, tx, x.d.ID, id); err != nil {
				return err
			}
			if err := deleteForgetTx(ctx, tx, x.d.ID, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, id := range ids {
		if it, ok := pending[id]; ok {
			if err := x.finish(ctx, it.ID, jobs.ItemDone, 0, ""); err != nil {
				return err
			}
			x.forgot++
		}
	}
	return nil
}

type forgetCounts struct{ all, done, failed, skipped int64 }

// forgetItemCounts counts the job's forget items (the expire items of snapshots) by status; ok is
// false when the item store cannot list items (then the attempt's own count is used).
func (x *resticRun) forgetItemCounts(ctx context.Context) (forgetCounts, bool, error) {
	var c forgetCounts
	items, ok, err := x.allItems(ctx, jobs.ActionExpire)
	if err != nil || !ok {
		return c, ok, err
	}
	for _, it := range items {
		d, err := parseItem(it)
		if err != nil || d.Check != checkExpireSnapshot {
			continue
		}
		c.all++
		switch it.Status {
		case jobs.ItemDone:
			c.done++
		case jobs.ItemFailed:
			c.failed++
		case jobs.ItemSkipped:
			c.skipped++
		}
	}
	return c, true, nil
}

// statCheckAfterPrune is the engine_state stat set from a prune until the restic check after it
// succeeded (S24: a check follows every prune, also when the window's end or a restart stopped
// that check).
const statCheckAfterPrune = "checkAfterPrune"

// pruneIfDue prunes when params.prune is set or pruneEveryDays elapsed since the last prune (§6.5
// step 4), then checks the structure; both after a guarded unlock. A prune the window's end
// interrupts defers the job (it is safe to run again); a check after a prune that did not finish
// runs in the next retention job even when no prune is due.
func (x *resticRun) pruneIfDue(ctx context.Context, st *RetentionStats) error {
	state, err := x.s.State(ctx, x.d.ID)
	if err != nil {
		return err
	}
	every := 7
	maxUnused := "10%"
	if rs := x.d.Settings.Restic; rs != nil {
		every, maxUnused = rs.PruneEveryDays, rs.PruneMaxUnused
	}
	now := x.s.now()
	due := x.job.Params.Prune || state.LastPruneAt == nil || now.Sub(*state.LastPruneAt) >= time.Duration(every)*24*time.Hour
	var checkPending bool
	state.stat(statCheckAfterPrune, &checkPending)
	if !due && !checkPending {
		return nil
	}
	if !x.open(now) {
		return x.deferral(now)
	}
	if _, err := x.repo.Snapshots(ctx, nil, nil); err != nil {
		return err
	}
	if err := x.repo.CheckIdentity(ctx); err != nil {
		return err
	}
	if err := x.repo.GuardedUnlock(ctx, x.rt.HostName, x.rt.ProcessStart, nil); err != nil {
		return err
	}
	stop := x.stopAtEnd(now, false)
	defer stop.stop()
	if due {
		up, down := x.d.Bandwidth.InForce(now, x.s.loc)
		started := x.s.now()
		err = x.repo.Prune(ctx, restic.PruneArgs{MaxUnused: maxUnused, LimitUpKiB: up, LimitDownKiB: down, Interrupt: stop.C})
		if err != nil {
			x.releaseLock(ctx, err)
			if stop.fired() || errors.Is(err, restic.ErrInterrupted) {
				x.log(slog.LevelInfo, "the transfer window closed during prune; it runs again in the next window")
				return x.deferral(x.s.now())
			}
			return fmt.Errorf("prune: %w", err)
		}
		st.Pruned = true
		st.PruneDurationMs = x.s.now().Sub(started).Milliseconds()
		pruned := x.s.now()
		if err := x.s.updateState(ctx, x.d.ID, func(es *EngineState) {
			es.LastPruneAt = &pruned
			es.setStat(statCheckAfterPrune, true)
		}); err != nil {
			return err
		}
		if err := x.repo.GuardedUnlock(ctx, x.rt.HostName, x.rt.ProcessStart, nil); err != nil {
			return err
		}
	} else {
		x.log(slog.LevelInfo, "the check after the last prune did not finish; it runs now")
	}
	res, err := x.repo.Check(ctx, restic.CheckArgs{Interrupt: stop.C})
	if err != nil {
		x.releaseLock(ctx, err)
		if stop.fired() || errors.Is(err, restic.ErrInterrupted) {
			x.log(slog.LevelInfo, "the transfer window closed during the check after prune; it runs again in the next window")
			return x.deferral(x.s.now())
		}
		return fmt.Errorf("check after prune: %w", err)
	}
	checked := x.s.now()
	if err := x.s.updateState(ctx, x.d.ID, func(es *EngineState) {
		es.LastCheckAt = &checked
		delete(es.Stats, statCheckAfterPrune)
	}); err != nil {
		return err
	}
	if res.NumErrors > 0 {
		return fmt.Errorf("restic check after prune found %d errors (see the README's repair procedure)", res.NumErrors)
	}
	return nil
}

// releaseTimeout bounds releaseLock.
const releaseTimeout = time.Minute

// releaseLock removes the exclusive lock a check, prune or forget left when it did not end
// normally (err: the window's end, the job's cancellation, a time budget): the interrupt stops
// restic's rclone backend with restic, so restic cannot remove its lock (§20.3, children.go), and
// a stale exclusive lock refuses every backup (exit 11) and every config version job until it is
// gone. The guarded unlock (§6.7) recognizes the lock of this process's own child. It runs without
// the job's cancellation, bounded by releaseTimeout; a failure is a warning (the next backup or
// exclusive command runs the guarded unlock too).
func (x *resticRun) releaseLock(ctx context.Context, err error) {
	if errors.Is(err, engines.ErrLocked) || errors.Is(err, restic.ErrForeignLock) {
		return // the command did not take its lock
	}
	uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	if uerr := x.repo.GuardedUnlock(uctx, x.rt.HostName, x.rt.ProcessStart, nil); uerr != nil {
		x.log(slog.LevelWarn, "could not remove the lock of the stopped restic command", "error", uerr.Error())
	}
}
