package syncer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// SyncRunner runs sync jobs (jobs.TypeSync, design §4.1): Params.DestinationID is the destination,
// Params.SourceIDs optionally narrows the linked sources, Params.AllowChanges runs what the
// mass-change guard would hold, and a dry run stops after persisting the plan.
type SyncRunner struct {
	base
	planBatch    int
	recheckEvery int
	// freeSpace is filecopy.FreeSpace; tests replace it.
	freeSpace func(*os.Root) (free, total uint64, err error)
}

// NewSyncRunner returns the sync job runner.
func NewSyncRunner(o Options) *SyncRunner {
	return &SyncRunner{base: newBase(o), planBatch: planBatch, recheckEvery: recheckEvery, freeSpace: filecopy.FreeSpace}
}

var _ jobs.Runner = (*SyncRunner)(nil)

// SyncStats is a sync job's stats JSON (design §6.1). Files* count items by outcome; a dry run
// counts what would happen. BytesPlanned and BytesCopied count content once per hardlink group
// (unique bytes): link items carry no bytes.
type SyncStats struct {
	DryRun         bool  `json:"dryRun"`
	FilesPlanned   int64 `json:"filesPlanned"`
	FilesCopied    int64 `json:"filesCopied"`
	FilesUpdated   int64 `json:"filesUpdated"`
	FilesMoved     int64 `json:"filesMoved"`
	FilesAdopted   int64 `json:"filesAdopted"`
	FilesLinked    int64 `json:"filesLinked"`
	FilesPromoted  int64 `json:"filesPromoted"`
	FilesRetained  int64 `json:"filesRetained"`
	FilesDisplaced int64 `json:"filesDisplaced"`
	FilesHeld      int64 `json:"filesHeld"`
	FilesFailed    int64 `json:"filesFailed"`
	FilesSkipped   int64 `json:"filesSkipped"`
	BytesPlanned   int64 `json:"bytesPlanned"`
	BytesCopied    int64 `json:"bytesCopied"`
	DurationMs     int64 `json:"durationMs"`
	// Sources summarizes each source's scan and plan (only for the attempt that planned).
	Sources []SourceSummary `json:"sources"`
}

// SourceSummary is one source's part of a sync.
type SourceSummary struct {
	SourceID int64  `json:"sourceId"`
	Name     string `json:"name"`
	// Files, Added, Changed, Deleted and Skipped are the source scan's counts.
	Files   int64 `json:"files"`
	Added   int64 `json:"added"`
	Changed int64 `json:"changed"`
	Deleted int64 `json:"deleted"`
	Skipped int64 `json:"skipped"`
	// Changes is what the mass-change guard counted; Held how many items it held.
	Changes int64 `json:"changes"`
	Held    int64 `json:"held"`
}

// syncRun is the state of one sync job attempt.
type syncRun struct {
	r   *SyncRunner
	job jobs.Job
	env jobs.Env
	rep jobs.Reporter
	h   *destinations.Handle

	sources []catalog.Source
	linked  map[int64]catalog.Source
	roots   map[int64]*os.Root

	retentionDir string
	linkMode     bool

	planned      bool // this attempt planned (not a resume of a complete plan)
	bytesPlanned int64
	summaries    []SourceSummary
	warnings     int
	scanSkipped  int64

	// displaced counts unmanaged files moved out of the way (this attempt); reclassified counts
	// done items whose outcome differs from their action ([planned][outcome], this attempt).
	displaced    int64
	reclassified map[[2]jobs.ItemAction]int64

	// catalogLive holds each source's live catalog paths, loaded when execution first needs them.
	catalogLive map[int64]map[string]bool
	// notCurrent maps, per source, a source directory to a live catalog file in it that has no
	// current record (not backed up), loaded when the first retain runs (notBackedUp).
	notCurrent map[int64]map[string]string

	progress     jobs.Progress
	sinceRecheck int
}

// Run implements jobs.Runner.
func (r *SyncRunner) Run(ctx context.Context, job jobs.Job, env jobs.Env) (jobs.Result, error) {
	started := r.now()
	if env.Items == nil {
		return jobs.Result{}, errors.New("sync: the job has no item store")
	}
	destID := job.Params.DestinationID
	if destID == 0 {
		return jobs.Result{}, errors.New("sync: the job has no destination")
	}
	d, err := r.dests.Get(ctx, destID)
	if err != nil {
		return jobs.Result{}, fmt.Errorf("sync: %w", err)
	}
	if !d.Enabled {
		return jobs.Result{}, fmt.Errorf("sync: destination %q is disabled", d.Name)
	}
	h, err := r.dests.Open(ctx, destID)
	if err != nil {
		return jobs.Result{}, fmt.Errorf("sync: %w", err)
	}
	defer h.Close()

	s := &syncRun{r: r, job: job, env: env, rep: reporterOf(env), h: h, linked: map[int64]catalog.Source{}, roots: map[int64]*os.Root{},
		retentionDir: filecopy.RetentionDir(job.QueuedAt, job.ID)}
	defer s.closeRoots()
	s.rep.Log(slog.LevelInfo, "sync started", "destination", d.Name, "attempt", job.Attempt, "dryRun", job.DryRun,
		"allowChanges", job.Params.AllowChanges, "hardlinks", string(h.Settings.Hardlinks))
	if err := prepareIdentity(ctx, h, s.rep, job.DryRun, &s.warnings); err != nil {
		return jobs.Result{}, err
	}
	s.linkMode = h.Capabilities.Hardlinks && h.Settings.Hardlinks == destinations.HardlinksRecreate
	s.rep.Log(slog.LevelInfo, "destination capabilities", "hardlinks", h.Capabilities.Hardlinks, "linkMode", s.linkMode,
		"unstableInodes", h.Capabilities.UnstableInodes)
	if !job.DryRun {
		// What stopped jobs left in retention is settled before the plan reads the records.
		n, err := reconcile(ctx, newIntentFixer(r.base, h, s.rep, job.ID), env.Items, s.retentionDir)
		s.warnings += n
		if err != nil {
			if ctx.Err() != nil {
				return jobs.Result{}, ctx.Err()
			}
			return jobs.Result{}, fmt.Errorf("sync: %w", err)
		}
	}
	if err := s.selectSources(ctx); err != nil {
		return jobs.Result{}, err
	}

	planned, err := env.Items.Planned(ctx, job.ID)
	if err != nil {
		return jobs.Result{}, err
	}
	if planned {
		s.rep.Log(slog.LevelInfo, "the plan is complete: continuing with its pending items")
	} else {
		// A job with items but no complete plan was stopped while planning: plan again.
		if err := env.Items.DeleteItems(ctx, job.ID); err != nil {
			return jobs.Result{}, err
		}
		if err := s.scanAndPlan(ctx); err != nil {
			if ctx.Err() != nil {
				return jobs.Result{}, ctx.Err()
			}
			return jobs.Result{}, err
		}
		s.planned = true
		if err := s.checkFreeSpace(); err != nil {
			if !job.DryRun {
				return jobs.Result{}, err
			}
			s.warnings++
			s.rep.Log(slog.LevelWarn, err.Error())
		}
	}
	if job.DryRun {
		return s.result(ctx, started)
	}
	if err := s.openSources(); err != nil {
		return jobs.Result{}, err
	}
	if err := s.execute(ctx); err != nil {
		if ctx.Err() != nil {
			return jobs.Result{}, ctx.Err()
		}
		return jobs.Result{}, err
	}
	if err := writeLinkManifest(ctx, r.store, h); err != nil {
		s.warnings++
		s.rep.Log(slog.LevelWarn, "could not write the hardlink manifest", "path", LinkManifestRel, "error", err.Error())
	}
	return s.result(ctx, started)
}

// selectSources loads the destination's linked, enabled sources (narrowed by Params.SourceIDs).
func (s *syncRun) selectSources(ctx context.Context) error {
	for _, id := range s.h.Destination.SourceIDs {
		if len(s.job.Params.SourceIDs) > 0 && !slices.Contains(s.job.Params.SourceIDs, id) {
			continue
		}
		src, err := s.r.cat.Get(ctx, id)
		if errors.Is(err, catalog.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if !src.Enabled {
			s.rep.Log(slog.LevelInfo, "source is disabled: not synced", "source", src.Name)
			continue
		}
		s.sources = append(s.sources, src)
		s.linked[src.ID] = src
	}
	if len(s.sources) == 0 {
		s.rep.Log(slog.LevelInfo, "no enabled sources are linked to this destination")
	}
	return nil
}

// setSource replaces the job's copy of a source.
func (s *syncRun) setSource(src catalog.Source) {
	s.linked[src.ID] = src
	if i := slices.IndexFunc(s.sources, func(c catalog.Source) bool { return c.ID == src.ID }); i >= 0 {
		s.sources[i] = src
	}
}

// dropSource removes a source from the job (it is not synced).
func (s *syncRun) dropSource(id int64) {
	delete(s.linked, id)
	s.sources = slices.DeleteFunc(s.sources, func(c catalog.Source) bool { return c.ID == id })
}

func (s *syncRun) closeRoots() {
	for _, r := range s.roots {
		_ = r.Close()
	}
}

// openSources opens each source root for execution. The scan checked the roots (S10a) when this
// attempt planned; a resumed job checks at least that each root is a directory that is not empty
// while its catalog lists files (an unmounted share), so it never takes an empty mountpoint for a
// source whose files all vanished.
func (s *syncRun) openSources() error {
	for _, src := range s.sources {
		root, err := os.OpenRoot(src.Path)
		if err != nil {
			return fmt.Errorf("source %q: open %s: %w (is it mounted?)", src.Name, src.Path, err)
		}
		s.roots[src.ID] = root
		if s.planned || src.Stats.Files == 0 {
			continue
		}
		f, err := root.Open(".")
		if err != nil {
			return fmt.Errorf("source %q: read %s: %w (is it mounted?)", src.Name, src.Path, err)
		}
		names, err := f.Readdirnames(1)
		_ = f.Close()
		if len(names) == 0 {
			return fmt.Errorf("source %q: %s is empty but its catalog lists %d files; is it mounted? (nothing was changed)", src.Name, src.Path, src.Stats.Files)
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("source %q: read %s: %w", src.Name, src.Path, err)
		}
	}
	return nil
}

// scanAndPlan scans each source (under its lock) and persists the plan (design §4.1 steps 2-4).
func (s *syncRun) scanAndPlan(ctx context.Context) error {
	names := newNameIndex(s.h.Capabilities)
	err := s.r.store.eachLive(ctx, s.h.Destination.ID, func(rec Record) error {
		_, planned := s.linked[rec.SourceID]
		names.addRecord(rec.RelPath, !planned)
		return nil
	})
	if err != nil {
		return err
	}
	var batch []jobs.Item
	flush := func(final bool) error {
		if err := s.env.Items.AddItems(ctx, s.job.ID, batch, final); err != nil {
			return err
		}
		batch = batch[:0]
		if !final {
			faultinject.Point(PointPlanAfterBatch)
		}
		return nil
	}
	for _, src := range slices.Clone(s.sources) { // planSource refreshes s.sources
		items, err := s.planSource(ctx, src, names)
		if err != nil {
			return err
		}
		for _, it := range items {
			batch = append(batch, it.item())
			if len(batch) >= s.r.planBatch {
				if err := flush(false); err != nil {
					return err
				}
			}
		}
	}
	return flush(true)
}

// planSource scans one source and plans it, holding the source's lock so the catalog does not
// change in between.
func (s *syncRun) planSource(ctx context.Context, src catalog.Source, names *nameIndex) ([]*planItem, error) {
	unlock, err := s.r.cat.LockSource(ctx, src.ID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	// The source may have been edited since selectSources (its path and destFolder can change
	// while its lock is free and it has no backups): plan and execute with what the scan sees.
	fresh, err := s.r.cat.Get(ctx, src.ID)
	if err != nil && !errors.Is(err, catalog.ErrNotFound) {
		return nil, err
	}
	if err != nil || !fresh.Enabled {
		s.rep.Log(slog.LevelInfo, "source was deleted or disabled since the sync started: not synced", "source", src.Name)
		s.dropSource(src.ID)
		return nil, nil
	}
	if fresh.Path != src.Path || fresh.DestFolder != src.DestFolder {
		s.rep.Log(slog.LevelInfo, "source was edited since the sync started", "source", fresh.Name, "path", fresh.Path,
			"destFolder", fresh.DestFolder)
	}
	src = fresh
	s.setSource(src)
	s.report(jobs.Progress{Phase: "scanning", CurrentFile: src.Name})
	res, err := s.r.scan.ScanLocked(ctx, src.ID, s.rep)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("scan source %q: %w", src.Name, err)
	}
	s.warnings += res.WarningCount
	s.scanSkipped += res.SkippedTotal()
	sum := SourceSummary{SourceID: src.ID, Name: src.Name, Files: res.Files, Added: res.Added, Changed: res.Changed,
		Deleted: res.Deleted, Skipped: res.SkippedTotal()}

	s.report(jobs.Progress{Phase: "planning", CurrentFile: src.Name})
	var files []*planFile
	err = s.r.cat.Live(ctx, src.ID, func(f catalog.File) error {
		files = append(files, &planFile{id: f.ID, rel: f.RelPath, size: f.Size, mtimeNs: f.MtimeNs, group: f.HardlinkGroup})
		return nil
	})
	if err != nil {
		return nil, err
	}
	recs, err := s.r.store.LiveForSource(ctx, s.h.Destination.ID, src.ID)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(src.Path)
	if err != nil {
		return nil, fmt.Errorf("source %q: open %s: %w", src.Name, src.Path, err)
	}
	defer root.Close()
	p := &sourcePlanner{sourceID: src.ID, destFolder: src.DestFolder, caps: s.h.Capabilities, settings: s.h.Settings,
		names: names, fs: livePlanFS{src: root, dst: s.h.Root}, files: files}
	for i := range recs {
		p.recs = append(p.recs, &recs[i])
	}
	if err := p.plan(ctx); err != nil {
		return nil, fmt.Errorf("plan source %q: %w", src.Name, err)
	}
	items := p.items()
	g := applyGuard(items, int64(len(files)), s.h.Settings, s.job.Params.AllowChanges)
	sum.Changes, sum.Held = g.changes, g.held
	s.summaries = append(s.summaries, sum)
	for _, w := range p.warnings {
		s.rep.Log(slog.LevelWarn, w, "source", src.Name)
	}
	if g.held > 0 {
		msg := fmt.Sprintf("%s held (not run): %s", plural(g.held, "change", "changes"), g.message)
		if !g.limited {
			msg = fmt.Sprintf("%s held (not run): updates that shrink a file to less than half its size, or to nothing, need a sync with allowChanges", plural(g.held, "change", "changes"))
		}
		s.rep.Log(slog.LevelWarn, msg, "source", src.Name)
	}
	var bytes int64
	for _, it := range items {
		if it.status == "" && (it.action == jobs.ActionCopy || it.action == jobs.ActionUpdate) {
			bytes += it.bytes
		}
	}
	s.bytesPlanned += bytes
	s.rep.Log(slog.LevelInfo, "planned", "source", src.Name, "items", len(items), "changes", g.changes, "held", g.held,
		"bytesToCopy", bytes)
	return items, nil
}

// checkFreeSpace is the free-space guard (design §4.1): the bytes to copy must fit in the free
// space less 1 GiB.
func (s *syncRun) checkFreeSpace() error {
	if s.bytesPlanned == 0 {
		return nil
	}
	free, _, err := s.r.freeSpace(s.h.Root)
	if err != nil {
		return fmt.Errorf("check free space: %w", err)
	}
	var avail int64
	if free > freeSpaceReserve {
		avail = int64(min(free-freeSpaceReserve, uint64(1<<62)))
	}
	if s.bytesPlanned > avail {
		return fmt.Errorf("not enough free space at the destination: %s to copy, %s free (1 GiB is kept free); nothing was copied",
			formatBytes(s.bytesPlanned), formatBytes(int64(min(free, uint64(1<<62)))))
	}
	return nil
}

// inCatalog reports whether a source path is live in the source's catalog. The catalog is read
// once per source and attempt, the first time a vanished name is found on disk again.
func (s *syncRun) inCatalog(ctx context.Context, sourceID int64, rel string) (bool, error) {
	live, ok := s.catalogLive[sourceID]
	if !ok {
		live = map[string]bool{}
		err := s.r.cat.Live(ctx, sourceID, func(f catalog.File) error {
			live[f.RelPath] = true
			return nil
		})
		if err != nil {
			return false, err
		}
		if s.catalogLive == nil {
			s.catalogLive = map[int64]map[string]bool{}
		}
		s.catalogLive[sourceID] = live
	}
	return live[rel], nil
}

// notBackedUp returns a live catalog file in the directory of the source path rel that is not
// backed up: it has no current live record (its copy or update failed or was held, in this job or
// before) and its content is not kept in retention either (a name that came back after its retain
// had started is copied again by the next sync). It returns "" when there is none. A retain of a
// vanished name in that directory waits for it (S6: a Radarr upgrade whose new file failed to copy
// keeps the old version live). The catalog and the records are read once per source and attempt,
// when the first retain runs: every copy of the job has run by then.
func (s *syncRun) notBackedUp(ctx context.Context, sourceID int64, rel string) (string, error) {
	dirs, ok := s.notCurrent[sourceID]
	if !ok {
		live, err := s.r.store.LiveForSource(ctx, s.h.Destination.ID, sourceID)
		if err != nil {
			return "", err
		}
		kept, err := s.r.store.retainedForSource(ctx, s.h.Destination.ID, sourceID)
		if err != nil {
			return "", err
		}
		bySource := make(map[string][]Record, len(live))
		for _, r := range append(live, kept...) {
			// A missing record, a damaged version and a displaced file do not hold the source's content.
			if r.State != StateMissing && r.Reason != ReasonDamaged && r.Reason != ReasonDisplaced {
				bySource[r.SourceRelPath] = append(bySource[r.SourceRelPath], r)
			}
		}
		dirs = map[string]string{}
		err = s.r.cat.Live(ctx, sourceID, func(f catalog.File) error {
			backedUp := false
			for _, r := range bySource[f.RelPath] {
				backedUp = backedUp || (r.Size == f.Size && filecopy.MtimeMatch(r.MtimeNs, f.MtimeNs, s.h.Capabilities.MtimeGranularityNs, 0))
			}
			if dir := path.Dir(f.RelPath); !backedUp && dirs[dir] == "" {
				dirs[dir] = f.RelPath
			}
			return nil
		})
		if err != nil {
			return "", err
		}
		if s.notCurrent == nil {
			s.notCurrent = map[int64]map[string]string{}
		}
		s.notCurrent[sourceID] = dirs
	}
	return dirs[path.Dir(rel)], nil
}

// reclassify records that a done item of action from had the outcome of action to.
func (s *syncRun) reclassify(from, to jobs.ItemAction) {
	if from == to {
		return
	}
	if s.reclassified == nil {
		s.reclassified = map[[2]jobs.ItemAction]int64{}
	}
	s.reclassified[[2]jobs.ItemAction{from, to}]++
}

func (s *syncRun) report(p jobs.Progress) {
	s.progress.Phase = p.Phase
	s.progress.CurrentFile = p.CurrentFile
	s.rep.Progress(s.progress)
}

// livePlanFS is the planner's view of the real source and destination.
type livePlanFS struct{ src, dst *os.Root }

func (l livePlanFS) sourceHeadTail(rel string) (string, error) {
	return filecopy.HeadTailHash(l.src, rel)
}

func (l livePlanFS) destHeadTail(rel string) (string, error) {
	return filecopy.HeadTailHash(l.dst, rel)
}

func (l livePlanFS) destStat(rel string) (filecopy.Stat, bool, error) {
	st, err := filecopy.Lstat(l.dst, rel)
	if errors.Is(err, fs.ErrNotExist) {
		return filecopy.Stat{}, false, nil
	}
	if err != nil {
		return filecopy.Stat{}, false, err
	}
	return st, true, nil
}

// notExist reports errors that mean a path is not there (ENOENT, or a parent that is not a
// directory).
func notExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// result computes the stats (from the persisted items, so they cover earlier attempts) and the
// summary.
func (s *syncRun) result(ctx context.Context, started time.Time) (jobs.Result, error) {
	counts, err := s.env.Items.Counts(context.WithoutCancel(ctx), s.job.ID)
	if err != nil {
		return jobs.Result{}, err
	}
	st := SyncStats{DryRun: s.job.DryRun, Sources: s.summaries}
	if st.Sources == nil {
		st.Sources = []SourceSummary{}
	}
	var plannedBytes int64
	for _, c := range counts {
		st.FilesPlanned += c.Files
		counted := c.Status == jobs.ItemDone || (s.job.DryRun && c.Status == jobs.ItemPending)
		switch c.Status {
		case jobs.ItemHeld:
			st.FilesHeld += c.Files
		case jobs.ItemFailed:
			st.FilesFailed += c.Files
		case jobs.ItemSkipped:
			st.FilesSkipped += c.Files
		}
		if (c.Action == jobs.ActionCopy || c.Action == jobs.ActionUpdate) && (c.Status == jobs.ItemPending || c.Status == jobs.ItemDone) {
			plannedBytes += c.Bytes
		}
		if !counted {
			continue
		}
		switch c.Action {
		case jobs.ActionCopy:
			st.FilesCopied += c.Files
		case jobs.ActionUpdate:
			st.FilesUpdated += c.Files
		case jobs.ActionMove:
			st.FilesMoved += c.Files
		case jobs.ActionAdopt:
			st.FilesAdopted += c.Files
		case jobs.ActionLink:
			st.FilesLinked += c.Files
		case jobs.ActionPromote:
			st.FilesPromoted += c.Files
		case jobs.ActionRetain:
			st.FilesRetained += c.Files
		}
		if c.Status == jobs.ItemDone && (c.Action == jobs.ActionCopy || c.Action == jobs.ActionUpdate || c.Action == jobs.ActionLink ||
			c.Action == jobs.ActionAdopt || c.Action == jobs.ActionMove) {
			st.BytesCopied += c.Bytes
		}
	}
	// Outcomes that differ from the planned action (a link or move that became a copy, a copy
	// that found the file already there and adopted it).
	counter := func(a jobs.ItemAction) *int64 {
		switch a {
		case jobs.ActionCopy:
			return &st.FilesCopied
		case jobs.ActionUpdate:
			return &st.FilesUpdated
		case jobs.ActionMove:
			return &st.FilesMoved
		case jobs.ActionAdopt:
			return &st.FilesAdopted
		case jobs.ActionLink:
			return &st.FilesLinked
		}
		return new(int64)
	}
	for k, n := range s.reclassified {
		*counter(k[0]) -= n
		*counter(k[1]) += n
	}
	st.FilesDisplaced = s.displaced
	st.FilesSkipped += s.scanSkipped
	if s.planned {
		st.BytesPlanned = s.bytesPlanned
	} else {
		st.BytesPlanned = plannedBytes
	}
	st.DurationMs = s.r.now().Sub(started).Milliseconds()
	warnings := s.warnings + int(st.FilesFailed)
	if st.FilesHeld > 0 {
		warnings++
	}
	return jobs.Result{Stats: st, Warnings: warnings, Summary: syncSummary(st)}, nil
}

// syncSummary is the job's one-sentence summary.
func syncSummary(st SyncStats) string {
	var parts []string
	add := func(n int64, verb string) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", verb, n))
		}
	}
	if st.DryRun {
		add(st.FilesCopied, "copy")
		add(st.FilesUpdated, "update")
		add(st.FilesMoved, "move")
		add(st.FilesAdopted, "adopt")
		add(st.FilesLinked, "link")
		add(st.FilesPromoted, "promote")
		add(st.FilesRetained, "retain")
	} else {
		add(st.FilesCopied, "copied")
		add(st.FilesUpdated, "updated")
		add(st.FilesMoved, "moved")
		add(st.FilesAdopted, "adopted")
		add(st.FilesLinked, "linked")
		add(st.FilesPromoted, "promoted")
		add(st.FilesRetained, "retained")
		add(st.FilesDisplaced, "displaced")
	}
	var b strings.Builder
	switch {
	case st.DryRun && len(parts) == 0:
		b.WriteString("Dry run: nothing to do")
	case st.DryRun:
		fmt.Fprintf(&b, "Dry run: would %s (%s to copy)", strings.Join(parts, ", "), formatBytes(st.BytesPlanned))
	case len(parts) == 0:
		b.WriteString("Up to date: nothing to copy")
	default:
		fmt.Fprintf(&b, "%s%s (%s copied)", strings.ToUpper(parts[0][:1]), strings.Join(parts, ", ")[1:], formatBytes(st.BytesCopied))
	}
	if st.FilesHeld > 0 {
		fmt.Fprintf(&b, "; %d held by the mass-change guard", st.FilesHeld)
	}
	if st.FilesFailed > 0 {
		fmt.Fprintf(&b, "; %d failed", st.FilesFailed)
	}
	return b.String()
}
