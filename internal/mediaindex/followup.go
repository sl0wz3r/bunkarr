package mediaindex

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"

	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// Follow-up syncs (design §6.1). A refresh turns the folders of changed items into targeted syncs:
// each folder (the one the index had, and the one the item has now) is mapped and located in every
// source that contains it; per source, one sync with those paths is queued for every enabled
// destination linked to the source whose syncOnArrChange is on. A folder that is a source root,
// or more than jobs.MaxTargetPaths paths in one source, makes that source's sync untargeted. The
// current folder of an item that has files must exist: a missing one is a warning (a wrong
// mapping) and the source gets an untargeted sync instead.

// followUpTrigger is the trigger of a refresh's follow-up syncs: webhook for the webhook path
// (SyncAfter), else the refresh's own (a resumed refresh counts as start-up).
func (rn *run) followUpTrigger() jobs.Trigger {
	if rn.job.Params.SyncAfter {
		return jobs.TriggerWebhook
	}
	switch rn.job.Trigger {
	case jobs.TriggerSchedule, jobs.TriggerManual, jobs.TriggerStartup, jobs.TriggerWebhook:
		return rn.job.Trigger
	}
	return jobs.TriggerStartup
}

// followUpIntents queues the syncs of every pending intent of the job, then marks the intents
// done (failed when a sync could not be queued).
func (rn *run) followUpIntents(ctx context.Context) error {
	pending, itemIDs, _, err := rn.pendingIntents(ctx)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}
	if err := rn.ensureLocator(ctx); err != nil {
		return err
	}
	qerr := rn.queueFollowUps(ctx, pending)
	faultinject.Point(PointAfterFollowUps)
	if ctx.Err() != nil {
		// Cancelled or shut down while queuing: the intents stay pending, so a resume re-reads
		// them (and a cancel follows them up), instead of being marked failed.
		return ctx.Err()
	}
	return rn.settle(ctx, itemIDs, qerr)
}

// settle marks the job items ids (intents, root-folder sync marks) done once their syncs are
// queued, or failed with qerr when they could not be; it returns qerr joined with the errors of
// marking them.
func (rn *run) settle(ctx context.Context, ids []int64, qerr error) error {
	status, msg := jobs.ItemDone, ""
	if qerr != nil {
		status, msg = jobs.ItemFailed, qerr.Error()
	}
	errs := []error{qerr}
	for _, id := range ids {
		if err := rn.env.Items.Finish(ctx, id, status, 0, msg); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// followUpAfterFailure queues, after a failed refresh, the syncs of the pending intents and, for a
// webhook refresh, of each requested item's folder as the index has it; for an untargeted refresh
// with SyncAfter (an overflow merge: the webhook's items are no longer named), the untargeted
// sync of every source a root folder of the integration locates into, which settles its mark.
func (rn *run) followUpAfterFailure(ctx context.Context) error {
	pending, itemIDs, marks, err := rn.pendingIntents(ctx)
	if err != nil {
		return err
	}
	overflow := rn.job.Params.SyncAfter && len(rn.job.Params.ArrItemIDs) == 0
	if len(pending) == 0 && !rn.job.Params.SyncAfter {
		return nil
	}
	if err := rn.ensureLocator(ctx); err != nil {
		return err
	}
	var whole map[int64]bool
	if overflow {
		if whole, err = rn.rootFolderSources(ctx); err != nil {
			return err
		}
	}
	covered := map[int64]bool{}
	for _, in := range pending {
		covered[in.ArrID] = true
	}
	list := pending
	if rn.job.Params.SyncAfter {
		for _, id := range rn.job.Params.ArrItemIDs {
			if covered[id] {
				continue
			}
			it, err := rn.r.store.Item(ctx, nil, rn.it.ID, rn.kind, id)
			if errors.Is(err, ErrItemNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			// The index's folder: not checked for existence (the refresh did not confirm it).
			list = append(list, intent{Kind: it.Kind, ArrID: it.ArrID, Title: it.Title, NewFolder: it.Path, RootFolder: it.RootFolder, Reason: "requested"})
		}
	}
	// With nothing to queue this queues nothing; the marks are settled all the same.
	qerr := rn.queueFollowUpsWith(ctx, list, whole)
	return rn.settle(ctx, append(itemIDs, marks...), qerr)
}

// followUpStranded queues the syncs that earlier refresh jobs of the integration left pending
// when they ended without their runner (Store.strandedIntentJobs): the syncs of their intents,
// and for an overflow refresh (a root-folder sync mark) the untargeted sync of every source a root
// folder locates into, as the index has them. The index has their changes already, so no later
// refresh finds them again. The intents' folders are not checked for existence (this refresh did
// not confirm them). The items are marked done once their syncs are queued; when that fails they
// stay pending, and the next refresh tries again.
func (rn *run) followUpStranded(ctx context.Context) error {
	jobIDs, err := rn.r.store.strandedIntentJobs(ctx, rn.it.ID, rn.job.ID)
	if err != nil || len(jobIDs) == 0 {
		return err
	}
	var (
		list     []intent
		itemIDs  []int64
		overflow []int64
	)
	for _, id := range jobIDs {
		in, ids, marks, err := rn.intentsOf(ctx, id)
		if err != nil {
			return err
		}
		for i := range in {
			in[i].HasFiles = false
		}
		list, itemIDs = append(list, in...), append(itemIDs, ids...)
		if len(marks) > 0 {
			overflow, itemIDs = append(overflow, id), append(itemIDs, marks...)
		}
	}
	if len(itemIDs) == 0 {
		return nil
	}
	var whole map[int64]bool
	if len(overflow) > 0 {
		if whole, err = rn.rootFolderSources(ctx); err != nil {
			return err
		}
		rn.info("Earlier webhook refreshes of the whole library ended without queuing their syncs: every source the root folders locate into is synced now", "jobs", overflow)
	}
	if len(list) > 0 {
		rn.info(fmt.Sprintf("%d change(s) indexed by earlier refreshes that ended without queuing their syncs are synced now", len(list)), "jobs", jobIDs)
	}
	if err := rn.queueFollowUpsWith(ctx, list, whole); err != nil {
		return err
	}
	return rn.settle(ctx, itemIDs, nil)
}

// ensureLocator loads the locator when the attempt failed before it did.
func (rn *run) ensureLocator(ctx context.Context) error {
	if rn.loc != nil {
		return nil
	}
	loc, err := rn.r.o.Catalog.Locator(ctx)
	if err != nil {
		return err
	}
	rn.loc = loc
	return nil
}

// rootFolderSources returns the sources the integration's root folders locate into: as this
// attempt fetched them, or else as the index has them (the metadata was not read).
func (rn *run) rootFolderSources(ctx context.Context) (map[int64]bool, error) {
	if rn.rootSources != nil {
		return rn.rootSources, nil
	}
	m, err := rn.r.store.Meta(ctx, nil, rn.it.ID)
	if err != nil {
		return nil, err
	}
	out := map[int64]bool{}
	for _, rf := range m.RootFolders {
		_, locs := rn.mapLocate(cleanArrPath(rf.Path))
		for _, l := range locs {
			out[l.SourceID] = true
		}
	}
	return out, nil
}

// followUpUntargeted queues an untargeted sync of every source a root folder of the integration
// locates into (an untargeted refresh with SyncAfter, made by an overflow merge, design §12.2),
// then settles the job's root-folder sync mark.
func (rn *run) followUpUntargeted(ctx context.Context) error {
	qerr := rn.queueSyncs(ctx, nil, rn.rootSources)
	if ctx.Err() != nil {
		// Cancelled or shut down while queuing: the mark stays pending (a shut-down job resumes;
		// a cancelled webhook refresh has its events re-armed and is not stranded).
		return ctx.Err()
	}
	_, _, marks, err := rn.pendingIntents(ctx)
	if err != nil {
		return errors.Join(qerr, err)
	}
	return rn.settle(ctx, marks, qerr)
}

// queueFollowUps maps and locates the intents' folders and queues the syncs.
func (rn *run) queueFollowUps(ctx context.Context, list []intent) error {
	return rn.queueFollowUpsWith(ctx, list, nil)
}

// queueFollowUpsWith is queueFollowUps with the sources in whole synced untargeted as well.
func (rn *run) queueFollowUpsWith(ctx context.Context, list []intent, whole map[int64]bool) error {
	targets := map[int64]map[string]bool{}
	untargeted := maps.Clone(whole)
	if untargeted == nil {
		untargeted = map[int64]bool{}
	}
	type folderKey struct {
		folder string
		check  bool
	}
	done := map[folderKey]bool{}
	for _, in := range list {
		for _, f := range []folderKey{{cleanArrPath(in.OldFolder), false}, {cleanArrPath(in.NewFolder), in.HasFiles}} {
			if f.folder == "" || done[f] {
				continue
			}
			done[f] = true
			local, locs := rn.mapLocate(f.folder)
			switch {
			case local == "":
				root := in.RootFolder
				if root == "" {
					root = path.Dir(f.folder)
				}
				rn.warn(fmt.Sprintf("%s folder %s (%s) is not covered by a path mapping, so it is not synced: add a mapping for %s",
					rn.app, f.folder, in.Title, root))
				continue
			case len(locs) == 0:
				rn.warn(fmt.Sprintf("%s folder %s (%s) maps to %s, which is in no source, so it is not synced", rn.app, f.folder, in.Title, local))
				continue
			}
			for _, l := range locs {
				if f.check {
					ok, err := rn.r.o.Catalog.DirExists(ctx, l)
					if err != nil || !ok {
						rn.warn(fmt.Sprintf("%s folder %s maps to %s, which does not exist: check the path mappings. The whole source is synced instead",
							rn.app, f.folder, local), "sourceId", l.SourceID)
						untargeted[l.SourceID] = true
						continue
					}
				}
				if l.Rel == "" {
					untargeted[l.SourceID] = true
					continue
				}
				if targets[l.SourceID] == nil {
					targets[l.SourceID] = map[string]bool{}
				}
				targets[l.SourceID][l.Rel] = true
			}
		}
	}
	return rn.queueSyncs(ctx, targets, untargeted)
}

// queueSyncs queues, per source, a sync with its target paths (none: untargeted) for every enabled
// destination linked to it whose syncOnArrChange is on. Disabled sources are skipped.
func (rn *run) queueSyncs(ctx context.Context, targets map[int64]map[string]bool, untargeted map[int64]bool) error {
	sources := map[int64]bool{}
	for id := range targets {
		sources[id] = true
	}
	for id, on := range untargeted {
		if on {
			sources[id] = true
		}
	}
	if len(sources) == 0 {
		return nil
	}
	if rn.r.o.Enqueuer == nil || rn.r.o.Destinations == nil {
		rn.warn("Follow-up syncs are not available in this process; they were not queued")
		return nil
	}
	dests, err := rn.r.o.Destinations(ctx)
	if err != nil {
		return fmt.Errorf("list destinations: %w", err)
	}
	slices.SortFunc(dests, func(a, b FollowUpDestination) int { return cmp.Compare(a.ID, b.ID) })
	trigger := rn.followUpTrigger()
	var errs []error
	for _, src := range slices.Sorted(maps.Keys(sources)) {
		if !rn.loc.Enabled(src) {
			rn.info("A changed folder lies in a disabled source; it is not synced", "sourceId", src)
			continue
		}
		var paths []string
		if !untargeted[src] {
			paths = slices.Sorted(maps.Keys(targets[src]))
			if len(paths) > jobs.MaxTargetPaths {
				rn.info(fmt.Sprintf("More than %d changed folders in one source: it is synced whole", jobs.MaxTargetPaths), "sourceId", src)
				paths = nil
			}
		}
		for _, d := range dests {
			if !d.Enabled || !d.SyncOnArrChange || !slices.Contains(d.SourceIDs, src) {
				continue
			}
			job, err := rn.r.o.Enqueuer.Enqueue(ctx, jobs.Spec{Type: jobs.TypeSync, Trigger: trigger,
				Params: jobs.Params{DestinationID: d.ID, SourceIDs: []int64{src}, Paths: paths}})
			if err != nil {
				errs = append(errs, fmt.Errorf("queue a sync of source %d to destination %d: %w", src, d.ID, err))
				continue
			}
			if !slices.Contains(rn.stats.FollowUpJobs, job.ID) {
				rn.stats.FollowUpJobs = append(rn.stats.FollowUpJobs, job.ID)
			}
		}
	}
	if len(rn.stats.FollowUpJobs) > 0 {
		rn.info("Queued the follow-up syncs", "jobs", rn.stats.FollowUpJobs)
	}
	return errors.Join(errs...)
}
