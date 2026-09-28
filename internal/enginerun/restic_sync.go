package enginerun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/engines/restic"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/syncer"
)

// The restic sync (§6.2): preflight (S25, the recorded snapshots still exist, a resumed job reads
// back the batch snapshots its earlier attempts did not record), the shared plan with
// UpdateKept (D29), cumulative batches per source (one snapshot each, tagged with the batch),
// each recorded through the read-back of its content (restic_record.go), and the finish: the last
// batch complete, the retains and releases decided after every batch with the S6 wait. Backups
// take shared locks, so a sync runs restic unlock only when an exclusive lock refused a backup
// (exit 11), through the guarded unlock of §6.7, and then tries that backup once more.

// resticRun is one restic job attempt.
type resticRun struct {
	*jobRun
	repo *restic.Repo

	plan    *syncer.Plan
	roots   map[int64]*os.Root
	sources map[int64]catalog.Source

	// listed are the snapshot ids of the preflight listing; baseGone the sources whose base was
	// removed outside Bunkarr (no --parent until a batch records a new one).
	listed   map[string]bool
	baseGone map[int64]bool
	// lastBatch is the highest batch number this job used.
	lastBatch int
	// batches counts this attempt's batches; started says one ran (allowOverrun, §9.2).
	batches int
	started bool
	// doneFiles and doneBytes are the finished batches' progress.
	doneFiles, doneBytes int64
	// forgot counts the snapshots this attempt forgot (retention).
	forgot int64

	localTargets []string
}

// unstableInodeFS are the filesystems whose inode numbers are assigned at run time (FUSE such as
// Unraid's /mnt/user, CIFS/SMB): restic runs with --ignore-inode on their sources.
var unstableInodeFS = map[string]bool{"fuse": true, "cifs": true, "smb2": true, "smb": true}

func ignoreInode(src catalog.Source) bool {
	fs := strings.ToLower(src.FSType)
	return unstableInodeFS[fs] || strings.HasPrefix(fs, "fuse.")
}

// resticSync runs a sync of a restic destination.
func (s *Service) resticSync(ctx context.Context, r *jobRun) (jobs.Result, error) {
	repo, err := s.o.Restic.Connect(r.ed, r.sec, r.rt)
	if err != nil {
		return jobs.Result{}, fmt.Errorf("sync: %w", err)
	}
	if err := repo.CheckIdentity(ctx); err != nil {
		return jobs.Result{}, fmt.Errorf("sync: %w", err)
	}
	x := &resticRun{jobRun: r, repo: repo, baseGone: map[int64]bool{}}
	r.log(slog.LevelInfo, "sync started", "destination", r.d.Name, "engine", "restic", "attempt", r.job.Attempt, "dryRun", r.job.DryRun,
		"allowChanges", r.job.Params.AllowChanges)
	if !r.job.DryRun {
		if err := x.preflightListing(ctx); err != nil {
			return jobs.Result{}, fmt.Errorf("sync: %w", err)
		}
	}
	var free func() (int64, bool, error)
	if r.ed.Kind == engines.Local {
		target := r.ed.Target
		free = func() (int64, bool, error) {
			root, err := os.OpenRoot(target)
			if err != nil {
				return 0, false, err
			}
			defer root.Close()
			avail, _, err := filecopy.FreeSpace(root)
			return int64(min(avail, uint64(1<<62))), err == nil, err
		}
	}
	plan, err := s.planner.Plan(ctx, syncer.PlanInput{Job: r.job, Env: jobs.Env{Reporter: r.rep, Items: r.env.Items},
		Dest: syncer.PlanDestination{ID: r.d.ID, Name: r.d.Name, SourceIDs: r.d.SourceIDs, Settings: r.d.Settings, Caps: restic.Capabilities()},
		FS:   recordPlanFS{ctx: ctx, files: s.files, dest: r.d.ID}, FreeSpace: free, Options: syncer.PlanOptions{UpdateKept: true}})
	if err != nil {
		return jobs.Result{}, err
	}
	x.plan = plan
	if r.job.DryRun {
		return x.result(ctx)
	}
	roots, closeRoots, err := sourceRoots(plan)
	if err != nil {
		return jobs.Result{}, err
	}
	defer closeRoots()
	x.roots = roots
	x.sources = map[int64]catalog.Source{}
	for _, src := range plan.Sources() {
		x.sources[src.ID] = src
	}
	if x.localTargets, err = s.localTargets(ctx); err != nil {
		return jobs.Result{}, err
	}
	if err := x.reconcileJob(ctx, !plan.Planned()); err != nil {
		return jobs.Result{}, fmt.Errorf("sync: %w", err)
	}
	err = x.execute(ctx)
	plan.FlagsAfterSync(ctx)
	if err != nil {
		return jobs.Result{}, err
	}
	return x.result(ctx)
}

// localTargets returns the targets of every local destination (restic excludes them, S28).
func (s *Service) localTargets(ctx context.Context) ([]string, error) {
	list, err := s.o.Destinations.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, d := range list {
		if d.Kind == engines.Local && d.Target != "" {
			out = append(out, d.Target)
		}
	}
	return out, nil
}

func (x *resticRun) result(ctx context.Context) (jobs.Result, error) {
	st, warnings, err := x.syncStats(ctx, x.plan)
	if err != nil {
		return jobs.Result{}, err
	}
	if !x.job.DryRun {
		rows, err := x.s.jobSnapshots(ctx, x.d.ID, x.job.ID)
		if err != nil {
			return jobs.Result{}, err
		}
		for _, r := range rows {
			st.Snapshots = append(st.Snapshots, SnapshotStat{SourceID: r.row.SourceID, SnapshotID: r.row.SnapshotID, Batch: r.row.Batch,
				FilesNew: r.sum.FilesNew, FilesChanged: r.sum.FilesChanged, FilesUnmodified: r.sum.FilesUnmodified, DataAdded: r.sum.DataAdded})
			st.BytesUploaded += r.sum.DataAddedPacked
			st.BytesRead += r.sum.TotalBytesProcessed
		}
		st.Batches = len(rows)
		st.Unchanged = len(rows) == 0
	}
	return jobs.Result{Stats: st, Warnings: warnings, Summary: resticSyncSummary(st, x.d.Name)}, nil
}

// contentAction reports the actions whose file a batch backs up (§6.2 step 3: copy, update, and
// the new side of move, link and promote).
func contentAction(a jobs.ItemAction) bool {
	switch a {
	case jobs.ActionCopy, jobs.ActionUpdate, jobs.ActionAdopt, jobs.ActionMove, jobs.ActionLink, jobs.ActionPromote:
		return true
	}
	return false
}

// execute runs the content items source by source in batches, then the finish. The files that
// waited for a transfer window run first (loadWaits).
func (x *resticRun) execute(ctx context.Context) error {
	items, err := x.pending(ctx)
	if err != nil {
		return err
	}
	items = x.loadWaits(ctx, items, func(it jobs.Item) bool { return contentAction(it.Action) },
		func(id int64) bool { _, ok := x.sources[id]; return ok })
	defer x.flushWaits(ctx)
	bySource := map[int64][]jobs.Item{}
	var retains []jobs.Item
	var total, bytes int64
	var order []int64
	for _, it := range items {
		d, err := parseItem(it)
		if err != nil {
			if err := x.failItem(ctx, it, err.Error()); err != nil {
				return err
			}
			continue
		}
		switch {
		case it.Action == jobs.ActionRetain:
			retains = append(retains, it)
		case contentAction(it.Action):
			if _, ok := bySource[d.SourceID]; !ok {
				order = append(order, d.SourceID)
			}
			bySource[d.SourceID] = append(bySource[d.SourceID], it)
			if it.Action == jobs.ActionCopy || it.Action == jobs.ActionUpdate {
				bytes += it.Bytes
			}
		default:
			continue
		}
		total++
	}
	x.report(func(p *jobs.Progress) { p.Phase, p.FilesTotal, p.BytesTotal = "backing-up", total, bytes })
	for _, id := range order {
		src, ok := x.sources[id]
		if !ok {
			for _, it := range bySource[id] {
				x.warnings++
				if err := x.finish(ctx, it.ID, jobs.ItemSkipped, 0, fmt.Sprintf("source %d is no longer synced to this destination", id)); err != nil {
					return err
				}
			}
			continue
		}
		if err := x.runSource(ctx, src, bySource[id]); err != nil {
			return err
		}
	}
	if x.windowWait {
		return x.deferral(x.s.now())
	}
	return x.finishSync(ctx, retains)
}

// sourceSet is the rule-1 part of a source's include set (§6.2 step 4): every live catalog file
// whose live record holds its catalog version and that has no pending item in this job, whatever
// its engine_ref.
type sourceSet struct {
	live     []string
	included map[string]bool
}

func (x *resticRun) sourceSet(ctx context.Context, src catalog.Source) (*sourceSet, error) {
	items, err := x.pending(ctx)
	if err != nil {
		return nil, err
	}
	pendingPaths := map[string]bool{}
	for _, it := range items {
		d, err := parseItem(it)
		if err == nil && d.SourceID == src.ID {
			pendingPaths[d.Source] = true
		}
	}
	recs, err := x.s.files.LiveForSource(ctx, x.d.ID, src.ID)
	if err != nil {
		return nil, err
	}
	bySource := make(map[string]syncer.Record, len(recs))
	for _, r := range recs {
		bySource[r.SourceRelPath] = r
	}
	set := &sourceSet{included: map[string]bool{}}
	err = x.s.o.Catalog.Live(ctx, src.ID, func(f catalog.File) error {
		set.live = append(set.live, f.RelPath)
		if r, ok := bySource[f.RelPath]; ok && !pendingPaths[f.RelPath] && r.Size == f.Size && r.MtimeNs == f.MtimeNs {
			set.included[f.RelPath] = true
		}
		return nil
	})
	return set, err
}

// batchWork is one content item of a batch.
type batchWork struct {
	it jobs.Item
	d  engineDetail
	// file is the item's file in the source and size/mtime the version it must hold.
	file          string
	size, mtimeNs int64
}

// runSource runs one source's content items in batches (§6.2 steps 3-5).
func (x *resticRun) runSource(ctx context.Context, src catalog.Source, items []jobs.Item) error {
	set, err := x.sourceSet(ctx, src)
	if err != nil {
		return err
	}
	queue := items
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		batch, rest, overrun, err := x.cutBatch(ctx, queue)
		if err != nil {
			return err
		}
		queue = rest
		if len(batch) == 0 {
			continue
		}
		if err := x.runBatch(ctx, src, set, batch, overrun); err != nil {
			return err
		}
	}
	return nil
}

// work resolves an item's file and version (a promote's is its target's).
func (x *resticRun) work(ctx context.Context, it jobs.Item) (batchWork, error) {
	d, err := parseItem(it)
	if err != nil {
		return batchWork{}, &itemErr{msg: err.Error()}
	}
	w := batchWork{it: it, d: d, file: d.Source, size: d.Size, mtimeNs: d.MtimeNs}
	if it.Action == jobs.ActionPromote {
		t, err := x.s.files.Get(ctx, d.TargetID)
		if err != nil {
			return batchWork{}, itemErrorf("the hardlinked name %s is no longer recorded", it.RelPath)
		}
		w.file, w.size, w.mtimeNs = t.SourceRelPath, t.Size, t.MtimeNs
	}
	return w, nil
}

// cutBatch takes the next batch off queue (§6.2 step 3, §9.2): at most batchFiles items and
// batchBytes copy and update bytes, with a transfer window at most 0.8 x the window's length x
// the rate in force, and after a window cut at most the cut cap (the files and the bytes restic
// reads, moves included); a file larger than that gets a batch of its own that starts only when
// it fits the time left plus the grace (at the window's opening; until then it waits while later
// items that fit run, and it runs first in the next attempt, whatever plan that is; one whose turn
// always comes late starts with allowOverrun, else fails, waitNotFitting); a file that cannot fit
// a whole window fails (unless allowOverrun). Items an earlier attempt recorded are finished.
func (x *resticRun) cutBatch(ctx context.Context, queue []jobs.Item) (batch []batchWork, rest []jobs.Item, overrun bool, err error) {
	now := x.s.now()
	if !x.open(now) {
		return nil, nil, false, x.deferral(now)
	}
	rs := x.d.Settings.Restic
	maxFiles, maxBytes := destinations.DefaultResticBatchFiles, int64(destinations.DefaultResticBatchBytes)
	if rs != nil {
		maxFiles, maxBytes = rs.BatchFiles, rs.BatchBytes
	}
	rate := x.rateAt(ctx, now)
	left, bounded := x.timeLeft(now)
	if bounded && rate > 0 {
		if capBytes := int64(0.8 * x.win.Length().Seconds() * float64(rate)); capBytes > 0 {
			maxBytes = min(maxBytes, capBytes)
		}
	}
	var readCap int64
	if c, ok := x.cutCap(ctx); ok && bounded {
		maxFiles, readCap = min(maxFiles, max(c.Files, 1)), c.Bytes
	}
	var bytes, read int64
	for i, it := range queue {
		w, err := x.work(ctx, it)
		if err != nil {
			if err := x.failItem(ctx, it, err.Error()); err != nil {
				return nil, nil, false, err
			}
			continue
		}
		if done, err := x.alreadyDone(ctx, w); err != nil {
			return nil, nil, false, err
		} else if done {
			if err := x.finish(ctx, it.ID, jobs.ItemDone, contentBytes(it), ""); err != nil {
				return nil, nil, false, err
			}
			continue
		}
		b := contentBytes(it)
		if msg := x.oversize(b, rate); msg != "" {
			if !x.win.AllowOverrun() {
				x.warn("a file is larger than the transfer window", "path", it.RelPath, "rate", formatRate(rate))
				if err := x.finish(ctx, it.ID, jobs.ItemFailed, 0, msg); err != nil {
					return nil, nil, false, err
				}
				continue
			}
			if len(batch) > 0 {
				return batch, queue[i:], false, nil
			}
			if !x.overrunMayStart(x.started, now, x.waitsOf(w.d)) {
				if err := x.waitOverrun(ctx, it, w.d, now); err != nil {
					return nil, nil, false, err
				}
				continue
			}
			x.dropWait(w.d)
			return []batchWork{w}, queue[i+1:], true, nil
		}
		if bounded && rate > 0 && !x.fitsLeft(expected(bytes+b, rate), left) {
			if len(batch) > 0 {
				return batch, queue[i:], false, nil
			}
			overrun, err := x.waitNotFitting(ctx, it, w.d, x.started, now, rate)
			if err != nil {
				return nil, nil, false, err
			}
			if overrun {
				x.dropWait(w.d)
				return []batchWork{w}, queue[i+1:], true, nil
			}
			continue
		}
		if len(batch) > 0 && (len(batch) >= maxFiles || (b > 0 && bytes+b > maxBytes) || (readCap > 0 && read+w.size > readCap)) {
			return batch, queue[i:], false, nil
		}
		x.dropWait(w.d)
		batch = append(batch, w)
		bytes += b
		read += w.size
	}
	return batch, nil, false, nil
}

// statCutCap is the engine_state stat that holds a restic destination's cut cap (§9.2): the
// batch limits after the window's end cut a batch. restic reuses nothing an interrupted backup
// uploaded (§20.3), so a batch that does not fit a window would be cut in every window (with no
// known rate, or with renamed files that restic reads in full and that count no upload bytes):
// each cut halves the next batches, down to one file, whose second cut fails it (the two-window
// backstop, kept here so that it survives a job its schedule superseded). It lives in the
// destination's state, not in the job's items, because a superseding job plans new items.
const statCutCap = "resticCutCap"

// cutCapState is the value of statCutCap.
type cutCapState struct {
	// Files and Bytes cap the next batches: items, and bytes restic reads (every content item's
	// size).
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
	// Source, Path, Size and MtimeNs name the file of the last one-file batch the window's end
	// cut, and Cuts how many windows cut it.
	Source  int64  `json:"source,omitempty"`
	Path    string `json:"path,omitempty"`
	Size    int64  `json:"size,omitempty"`
	MtimeNs int64  `json:"mtimeNs,omitempty"`
	Cuts    int    `json:"cuts,omitempty"`
}

// cutCap returns the destination's cut cap.
func (x *resticRun) cutCap(ctx context.Context) (cutCapState, bool) {
	st, err := x.s.State(ctx, x.d.ID)
	if err != nil {
		return cutCapState{}, false
	}
	var c cutCapState
	ok := st.stat(statCutCap, &c)
	return c, ok && c.Files > 0
}

// setCutCap stores (c.Files > 0) or removes the destination's cut cap.
func (x *resticRun) setCutCap(ctx context.Context, c cutCapState) error {
	return x.s.updateState(ctx, x.d.ID, func(st *EngineState) {
		if c.Files <= 0 {
			delete(st.Stats, statCutCap)
			return
		}
		st.setStat(statCutCap, c)
	})
}

// readBytes is what restic reads of a batch: every item's file, moves and links included.
func readBytes(batch []batchWork) int64 {
	var n int64
	for _, w := range batch {
		n += w.size
	}
	return n
}

// relaxCutCap doubles the cut cap after a batch completed within half the time the window had
// left when it started (the next batches may be larger), and removes it when it no longer limits
// anything or the destination has no window any more.
func (x *resticRun) relaxCutCap(ctx context.Context, batch []batchWork, src catalog.Source, took, left time.Duration, bounded bool) error {
	c, ok := x.cutCap(ctx)
	if !ok {
		return nil
	}
	if c.Path != "" && len(batch) == 1 && batch[0].d.SourceID == src.ID && batch[0].file == c.Path {
		c.Source, c.Path, c.Size, c.MtimeNs, c.Cuts = 0, "", 0, 0, 0
	}
	switch {
	case !bounded:
		c.Files = 0
	case 2*took <= left:
		c.Files, c.Bytes = c.Files*2, c.Bytes*2
		maxFiles, maxBytes := destinations.DefaultResticBatchFiles, int64(destinations.DefaultResticBatchBytes)
		if rs := x.d.Settings.Restic; rs != nil {
			maxFiles, maxBytes = rs.BatchFiles, rs.BatchBytes
		}
		if c.Files >= maxFiles && c.Bytes >= maxBytes && c.Path == "" {
			c.Files = 0
		}
	}
	return x.setCutCap(ctx, c)
}

// contentBytes is what an item uploads at most: copy and update bytes (moves, links and promotes
// count none).
func contentBytes(it jobs.Item) int64 {
	if it.Action == jobs.ActionCopy || it.Action == jobs.ActionUpdate || it.Action == jobs.ActionAdopt {
		return it.Bytes
	}
	return 0
}

// alreadyDone reports an item an earlier attempt recorded (its batch's transaction committed and
// the process stopped before the item was finished).
func (x *resticRun) alreadyDone(ctx context.Context, w batchWork) (bool, error) {
	if w.it.Action == jobs.ActionPromote {
		t, err := x.s.files.Get(ctx, w.d.TargetID)
		return err == nil && t.State == syncer.StatePresent && t.LinkOf == 0 && t.JobID == x.job.ID, nil
	}
	rec, ok, err := x.s.files.LiveAt(ctx, x.d.ID, w.it.RelPath)
	if err != nil || !ok {
		return false, err
	}
	return (rec.State == syncer.StatePresent || rec.State == syncer.StateLinked) && rec.SourceID == w.d.SourceID &&
		rec.SourceRelPath == w.file && rec.Size == w.size && rec.MtimeNs == w.mtimeNs && rec.JobID == x.job.ID && rec.EngineRef == "", nil
}

// runBatch backs up one batch (§6.2 step 4) and records it (step 5).
func (x *resticRun) runBatch(ctx context.Context, src catalog.Source, set *sourceSet, batch []batchWork, overrun bool) error {
	x.lastBatch++
	x.batches++
	k := x.lastBatch
	if x.batches > 1 && x.batches%recheckBatches == 1 {
		if err := x.repo.CheckIdentity(ctx); err != nil {
			return err
		}
	}
	root := x.roots[src.ID]
	inBatch := map[string]bool{}
	for i := range batch {
		w := &batch[i]
		inBatch[w.file] = true
		w.d.Batch = k
		if w.it.Action == jobs.ActionCopy || w.it.Action == jobs.ActionUpdate || w.it.Action == jobs.ActionLink || w.it.Action == jobs.ActionAdopt {
			w.d.HeadTail, w.d.HeadTailSize, w.d.HeadTailMtimeNs = headTail(root, w.file)
		}
	}
	files := restic.CompressIncludes(src.Path, set.live, func(rel string) bool { return set.included[rel] || inBatch[rel] })
	if len(files) == 0 {
		for _, w := range batch {
			if err := x.failItem(ctx, w.it, "not in the catalog any more"); err != nil {
				return err
			}
		}
		return nil
	}
	excludes, iexcludes, err := x.excludes(ctx, src)
	if err != nil {
		return err
	}
	base, err := x.s.baseOf(ctx, x.d.ID, src.ID)
	if err != nil {
		return err
	}
	parent := base
	if x.baseGone[src.ID] || (base != "" && !x.listed[base]) {
		parent = ""
	}
	tags, err := restic.Tags(restic.TagInput{EngineTag: x.ed.EngineTag, Kind: engines.VersionMedia, JobID: x.job.ID, SourceID: src.ID, Batch: k})
	if err != nil {
		return err
	}
	now := x.s.now()
	left, bounded := x.timeLeft(now)
	up, down := x.d.Bandwidth.InForce(now, x.s.loc)
	stop := x.stopAtEnd(now, overrun)
	defer stop.stop()
	x.started = true
	batches := x.lastBatch
	x.report(func(p *jobs.Progress) { p.Batch, p.Batches = k, max(p.Batches, batches) })
	faultinject.Point(PointBeforeBatch)
	args := restic.BackupArgs{FilesFrom: files, Excludes: excludes, IExcludes: iexcludes, Parent: parent,
		IgnoreInode: ignoreInode(src), Tags: tags, LimitUpKiB: up, LimitDownKiB: down, Interrupt: stop.C,
		OnStatus: func(st restic.Status) { x.onStatus(src, st) }}
	res, err := x.repo.Backup(ctx, args)
	if errors.Is(err, engines.ErrLocked) && ctx.Err() == nil && !stop.fired() {
		// An exclusive lock blocks the backup (exit 11). A check, prune or forget that was
		// interrupted, or a container stopped while one ran, leaves its exclusive lock behind
		// (restic's rclone backend stops with it, §20.3), and restic does not skip stale locks:
		// the guarded unlock (§6.7) removes a stale one, then the backup runs once more.
		x.log(slog.LevelInfo, "the repository is locked; removing stale locks and trying once more", "error", err.Error())
		if uerr := x.repo.GuardedUnlock(ctx, x.rt.HostName, x.rt.ProcessStart, nil); uerr != nil {
			return fmt.Errorf("%w (the stale locks were not removed: %v)", err, uerr)
		}
		if stop.fired() {
			// The window closed while the unlock waited: the batch never ran, so it is no cut.
			return x.deferral(x.s.now())
		}
		res, err = x.repo.Backup(ctx, args)
	}
	faultinject.Point(PointAfterBatchExit)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err // the batch's items stay pending; the resumed job runs it again
	}
	if res.Interrupted {
		return x.cut(ctx, src, batch)
	}
	took := x.s.now().Sub(now)
	// The upload rate is measured only on batches whose time went into uploading new data: renamed
	// or linked files are read in full and upload nothing, and deduplicated content uploads little,
	// so such a batch would measure the reading and understate the uplink (§9.2).
	var newBytes int64
	for _, w := range batch {
		newBytes += contentBytes(w.it)
	}
	if 2*newBytes >= readBytes(batch) && 2*res.Summary.DataAdded >= newBytes {
		x.s.noteThroughput(ctx, x.d.ID, res.Summary.DataAddedPacked, took)
	}
	done, err := x.recordBatch(ctx, src, k, batch, res.SnapshotID, res.Errors, res.Summary, base, false)
	if err != nil {
		return err
	}
	delete(x.baseGone, src.ID)
	for _, w := range batch {
		if done[w.it.ID] {
			set.included[w.file] = true
		}
	}
	x.doneFiles += res.Summary.TotalFilesProcessed
	x.doneBytes += res.Summary.TotalBytesProcessed
	return x.relaxCutCap(ctx, batch, src, took, left, bounded)
}

// onStatus turns a status line into progress (§10.4): the batch's counts and totals plus the
// finished batches' (restic counts every file it processes, unchanged ones included), and the
// current file relative to the source.
func (x *resticRun) onStatus(src catalog.Source, st restic.Status) {
	x.report(func(p *jobs.Progress) {
		p.FilesDone = x.doneFiles + st.FilesDone
		p.BytesDone = x.doneBytes + st.BytesDone
		if st.TotalFiles > 0 || st.TotalBytes > 0 {
			p.FilesTotal = x.doneFiles + max(st.TotalFiles, st.FilesDone)
			p.BytesTotal = x.doneBytes + max(st.TotalBytes, st.BytesDone)
		}
		if len(st.CurrentFiles) > 0 {
			p.CurrentFile = strings.TrimPrefix(strings.TrimPrefix(st.CurrentFiles[0], src.Path), "/")
		}
	})
}

// cut handles a batch the window's end interrupted (§9.2): its items stay pending and the job
// defers. The destination's cut cap halves the next batches (files and read bytes); a one-file
// batch cut in two windows fails with the oversize warning (the backstop when no rate says the
// file cannot fit), counted in its item and in the cut cap (a superseding job's items start at 0).
func (x *resticRun) cut(ctx context.Context, src catalog.Source, batch []batchWork) error {
	prev, _ := x.cutCap(ctx)
	next := cutCapState{Files: max(len(batch)/2, 1), Bytes: max(readBytes(batch)/2, 1)}
	if len(batch) == 1 {
		w := batch[0]
		next.Bytes = max(w.size, 1)
		cuts := w.d.WindowCuts
		if prev.Path == w.file && prev.Source == src.ID && prev.Size == w.size && prev.MtimeNs == w.mtimeNs {
			cuts = max(cuts, prev.Cuts)
		}
		w.d.WindowCuts = cuts + 1
		if w.d.WindowCuts >= 2 {
			x.warn("an item was cut by the transfer window's end twice", "path", w.it.RelPath)
			if err := x.finish(ctx, w.it.ID, jobs.ItemFailed, 0, cutTwiceMessage); err != nil {
				return err
			}
		} else {
			next.Source, next.Path, next.Size, next.MtimeNs, next.Cuts = src.ID, w.file, w.size, w.mtimeNs, w.d.WindowCuts
			x.firstCutWarning(w.it.RelPath)
			if err := x.setDetail(ctx, w.it.ID, w.d); err != nil {
				return err
			}
		}
	} else {
		x.log(slog.LevelInfo, "the next batches are smaller so that they fit the transfer window", "files", next.Files,
			"bytes", formatBytes(next.Bytes))
	}
	if err := x.setCutCap(ctx, next); err != nil {
		return err
	}
	x.log(slog.LevelInfo, "the transfer window closed: the batch stays pending and runs again in the next window")
	return x.deferral(x.s.now())
}

// excludes translates the source's exclusions into restic's exclude files (§6.2 step 4, S28):
// the config directory, every local destination target and the source's alias directories, the
// catalog's default patterns (case-insensitive) and the source's own (case-sensitive).
func (x *resticRun) excludes(ctx context.Context, src catalog.Source) ([]string, []string, error) {
	var aliases []string
	if fn := x.s.o.AliasDirs; fn != nil {
		var err error
		if aliases, err = fn(ctx, src); err != nil {
			return nil, nil, err
		}
	}
	var defaults, own []restic.ExcludePattern
	for _, p := range catalog.DefaultExcludePatterns() {
		defaults = append(defaults, restic.ExcludePattern(p))
	}
	for _, p := range catalog.ParseExcludePatterns(src.Exclude) {
		own = append(own, restic.ExcludePattern(p))
	}
	ex, iex, dropped := restic.TranslateExcludes(restic.TranslateInput{SourceRoot: src.Path, ConfigDir: x.s.o.ConfigDir,
		LocalTargets: x.localTargets, AliasDirs: aliases, Defaults: defaults, Own: own})
	for _, d := range dropped {
		x.log(slog.LevelDebug, "an exclusion is not passed to restic (restic could exclude more than the catalog)", "what", d)
	}
	return ex, iex, nil
}
