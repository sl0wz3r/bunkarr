package mediaindex

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// Fault points of the refresh (the crash matrix, design §15).
const (
	// PointAfterIntents: a batch's reconcile intents (job items) are stored, its rows are not.
	PointAfterIntents = "refresh.afterIntents"
	// PointAfterBatch: a batch of items and files is committed.
	PointAfterBatch = "refresh.afterBatch"
	// PointBeforeMarkDeleted: the complete fetch is committed; nothing is marked deleted yet.
	PointBeforeMarkDeleted = "refresh.beforeMarkDeleted"
	// PointAfterFollowUps: the follow-up syncs are queued; their intents are not marked done.
	PointAfterFollowUps = "refresh.afterFollowUps"
)

// Defaults and limits of the refresh (design §6.1, S10).
const (
	// DefaultConcurrency is how many per-item requests run at once.
	DefaultConcurrency = 4
	// DefaultBatchSize is how many rows (items plus files) one write transaction holds.
	DefaultBatchSize = 1000
	// GuardMinimum: the refresh guard holds a deletion only when it exceeds this many rows (and
	// half of the integration's rows).
	GuardMinimum = 20
	// PurgeAfter is how long an item marked deleted stays in the index.
	PurgeAfter = 30 * 24 * time.Hour
	// maxUnmappedFolders bounds stats.unmappedFolders.
	maxUnmappedFolders = 20
)

// FollowUpDestination is a destination a follow-up sync may go to (design §6.1): it gets syncs
// of its linked sources when it is enabled and its syncOnArrChange setting is on.
type FollowUpDestination struct {
	ID              int64
	Enabled         bool
	SourceIDs       []int64
	SyncOnArrChange bool
}

// RefreshOptions configures NewRunner.
type RefreshOptions struct {
	// DB is Bunkarr's database (required).
	DB *db.DB
	// Integrations reads the integration, its settings and its key (required).
	Integrations *integrations.Store
	// Catalog locates the *arr's paths in the sources and checks folders and files (required).
	Catalog *catalog.Store
	// Enqueuer queues the follow-up syncs; nil queues none (a warning says so).
	Enqueuer jobs.Enqueuer
	// Destinations lists the destinations follow-up syncs may go to; nil means none.
	Destinations func(ctx context.Context) ([]FollowUpDestination, error)
	// Client configures the *arr clients (tests: the HTTP client); its default transport dials
	// through the outbound guard.
	Client arr.Options
	// Log receives server-side events; nil discards them.
	Log *slog.Logger
	// Now is the clock (tests); nil is time.Now.
	Now func() time.Time
	// Concurrency and BatchSize override DefaultConcurrency and DefaultBatchSize.
	Concurrency int
	BatchSize   int
}

// Runner runs refresh jobs (jobs.TypeRefresh) of Sonarr, Radarr and Lidarr integrations.
type Runner struct {
	o     RefreshOptions
	store *Store
	log   *slog.Logger
}

// NewRunner returns the refresh runner.
func NewRunner(o RefreshOptions) (*Runner, error) {
	switch {
	case o.DB == nil:
		return nil, errors.New("mediaindex: no database")
	case o.Integrations == nil:
		return nil, errors.New("mediaindex: no integrations store")
	case o.Catalog == nil:
		return nil, errors.New("mediaindex: no catalog")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Concurrency <= 0 {
		o.Concurrency = DefaultConcurrency
	}
	if o.BatchSize <= 0 {
		o.BatchSize = DefaultBatchSize
	}
	log := o.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	st := NewStore(o.DB, o.Catalog)
	st.now = o.Now
	return &Runner{o: o, store: st, log: log}, nil
}

// Store returns the index store the runner writes.
func (r *Runner) Store() *Store { return r.store }

// Supports reports whether refresh jobs of integration type t can run: Sonarr, Radarr and
// Lidarr. (The Plex library index and the Tautulli, Seerr and Maintainerr caches are later
// slices of design §18.)
func Supports(t integrations.Type) bool { return t.IsArr() }

// UnmappedFolder counts an *arr root folder's files that map to no source (stats).
type UnmappedFolder struct {
	// RootFolder is as the *arr sees it.
	RootFolder string `json:"rootFolder"`
	Files      int64  `json:"files"`
	// Reason is unmapped (no path mapping) or no-source (mapped into no source).
	Reason string `json:"reason"`
}

// RecycleBin is an *arr's recycle bin as Bunkarr sees it (stats and warnings, design §6.1).
type RecycleBin struct {
	Path      string  `json:"path"`
	LocalPath *string `json:"localPath"`
	SourceID  *int64  `json:"sourceId"`
	RelPath   string  `json:"relPath"`
	// Excluded is whether the source's excludes skip it; inside a source and not excluded, every
	// upgrade's old file is copied twice.
	Excluded bool `json:"excluded"`
}

// Stats are a refresh job's stats (design §12.4).
type Stats struct {
	IntegrationID   int64             `json:"integrationId"`
	IntegrationType integrations.Type `json:"integrationType"`
	DryRun          bool              `json:"dryRun"`
	Targeted        bool              `json:"targeted"`
	AppVersion      string            `json:"appVersion,omitempty"`
	// Items is how many items the *arr listed (targeted: found); ItemsAdded, ItemsUpdated and
	// ItemsDeleted are the index changes (a dry run: what they would be).
	Items        int64 `json:"items"`
	ItemsAdded   int64 `json:"itemsAdded"`
	ItemsUpdated int64 `json:"itemsUpdated"`
	ItemsDeleted int64 `json:"itemsDeleted"`
	// Files is how many files the *arr reported; FilesMapped are located in a source,
	// FilesUnmapped are not (no mapping, or no source), FilesMismatched are located but match no
	// live catalog file by path and size; FilesDeleted left the index.
	Files           int64            `json:"files"`
	FilesMapped     int64            `json:"filesMapped"`
	FilesUnmapped   int64            `json:"filesUnmapped"`
	FilesMismatched int64            `json:"filesMismatched"`
	FilesDeleted    int64            `json:"filesDeleted"`
	UnmappedFolders []UnmappedFolder `json:"unmappedFolders"`
	// ChangedItems is the size of the changed set: items whose files or folder changed, new items
	// with files, and items marked deleted that had files.
	ChangedItems            int64       `json:"changedItems"`
	InaccessibleRootFolders []string    `json:"inaccessibleRootFolders"`
	RecycleBin              *RecycleBin `json:"recycleBin"`
	FileDate                string      `json:"fileDate,omitempty"`
	// Requests is how many requests were sent to the *arr.
	Requests int64 `json:"requests"`
	// GuardHeld is set when the refresh guard (S10) held deletions; GuardReason says which.
	GuardHeld   bool   `json:"guardHeld"`
	GuardReason string `json:"guardReason,omitempty"`
	// FollowUpJobs are the syncs the refresh queued.
	FollowUpJobs []int64 `json:"followUpJobs"`
	DurationMs   int64   `json:"durationMs"`
}

// Run implements jobs.Runner.
func (r *Runner) Run(ctx context.Context, job jobs.Job, env jobs.Env) (jobs.Result, error) {
	start := r.o.Now()
	p := job.Params
	if p.IntegrationID <= 0 {
		return jobs.Result{}, errors.New("refresh: no integration")
	}
	it, err := r.o.Integrations.Get(ctx, p.IntegrationID)
	if errors.Is(err, integrations.ErrNotFound) {
		return jobs.Result{}, fmt.Errorf("integration %d no longer exists", p.IntegrationID)
	}
	if err != nil {
		return jobs.Result{}, err
	}
	if !Supports(it.Type) {
		if len(p.ArrItemIDs) > 0 {
			return jobs.Result{}, fmt.Errorf("%q is a %s integration: arrItemIds are only for Sonarr, Radarr and Lidarr", it.Name, it.Type.AppName())
		}
		return jobs.Result{}, fmt.Errorf("refreshing %s integrations is not available yet", it.Type.AppName())
	}
	if !it.Enabled {
		return jobs.Result{}, fmt.Errorf("%s %q is disabled", it.Type.AppName(), it.Name)
	}
	settings, err := it.ArrSettings()
	if err != nil {
		return jobs.Result{}, err
	}
	rn := &run{
		r: r, job: job, env: env, it: it, settings: settings, app: it.Type.AppName(),
		kind: KindOf(it.Type), runAt: start.UTC(), dry: job.DryRun,
		stats: Stats{IntegrationID: it.ID, IntegrationType: it.Type, DryRun: job.DryRun, Targeted: len(p.ArrItemIDs) > 0,
			UnmappedFolders: []UnmappedFolder{}, InaccessibleRootFolders: []string{}, FollowUpJobs: []int64{}},
		unmapped: map[string]*UnmappedFolder{},
	}
	res, err := rn.execute(ctx)
	rn.stats.DurationMs = r.o.Now().Sub(start).Milliseconds()
	if f := rn.fetch; f != nil {
		rn.stats.Requests = f.requests.Load()
	}
	if err != nil {
		return jobs.Result{Stats: rn.stats, Warnings: rn.warnings}, err
	}
	res.Stats, res.Warnings = rn.stats, rn.warnings
	return res, nil
}

// run is one refresh attempt.
type run struct {
	r        *Runner
	job      jobs.Job
	env      jobs.Env
	it       integrations.Integration
	settings integrations.ArrSettings
	app      string
	kind     string
	// runAt is this attempt's start: every row it writes gets seen_at = runAt, so rows with
	// another seen_at were not seen by it.
	runAt    time.Time
	dry      bool
	fetch    *fetcher
	loc      *catalog.Locator
	state    State
	stats    Stats
	warnings int
	// first: the first complete refresh of this instance (nothing to reconcile against).
	first bool
	// instanceChanged: the cache describes another URL; the first write replaces it.
	instanceChanged bool
	// started: the attempt's first write is committed (index_state records it).
	started bool
	// wantIntents: changed items are recorded as intents and followed up.
	wantIntents bool
	// intents found in memory in this attempt (all of them; persisted ones too).
	intents []intent
	// seen items and files (dry runs, which write no seen_at).
	seenItems map[int64]bool
	seenFiles map[int64]bool
	// inaccessible root folders (as the *arr sees them).
	inaccessible []string
	// rootSources are the sources the root folders locate into (untargeted follow-ups).
	rootSources map[int64]bool
	unmapped    map[string]*UnmappedFolder
	batch       []fetched
	batchRows   int
}

// warn logs a job warning.
func (rn *run) warn(msg string, args ...any) {
	rn.warnings++
	rn.env.Reporter.Log(slog.LevelWarn, msg, args...)
}

func (rn *run) info(msg string, args ...any) {
	rn.env.Reporter.Log(slog.LevelInfo, msg, args...)
}

func (rn *run) execute(ctx context.Context) (jobs.Result, error) {
	res, err := rn.attempt(ctx)
	if err == nil {
		return res, nil
	}
	return jobs.Result{}, rn.failed(ctx, err)
}

// failed handles an attempt that returned err, from its first step on: a cancel (or shutdown)
// writes nothing more, except that the reconcile intents a full refresh recorded are followed up;
// a failure is recorded and followed up (design §6.1). It returns the job's error.
func (rn *run) failed(ctx context.Context, err error) error {
	if rn.dry {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	bg := context.WithoutCancel(ctx)
	if ctx.Err() != nil {
		// Cancelled or shut down. The SyncAfter follow-up is not queued: a cancelled webhook
		// refresh has its events re-armed, and a shut-down one resumes. The intents a reconcile
		// recorded are: the index already has their changes, so neither the next refresh nor a
		// resume would find them again (a resume finds nothing pending and queues no duplicate).
		if !rn.job.Params.SyncAfter {
			if ferr := rn.followUpIntents(bg); ferr != nil {
				rn.warn("Could not queue the follow-up syncs of the changes already indexed", "error", ferr.Error())
			}
		}
		return ctx.Err()
	}
	if serr := rn.r.store.recordFailure(bg, rn.it.ID, rn.runAt, err); serr != nil {
		rn.r.log.Error("Could not record a failed refresh", "integrationId", rn.it.ID, "error", serr)
	}
	// Follow-ups after a failure: the webhook's items with the folders the index has (an overflow
	// refresh: every source of the root folders), and the changes this attempt (or an earlier
	// one) already wrote to the index.
	if ferr := rn.followUpAfterFailure(bg); ferr != nil {
		rn.warn("Could not queue the follow-up syncs", "error", ferr.Error())
	}
	return err
}

// attempt runs the refresh.
func (rn *run) attempt(ctx context.Context) (jobs.Result, error) {
	var err error
	if rn.loc, err = rn.r.o.Catalog.Locator(ctx); err != nil {
		return jobs.Result{}, err
	}
	if !rn.dry {
		if err := rn.followUpStranded(ctx); err != nil {
			if ctx.Err() != nil {
				return jobs.Result{}, ctx.Err()
			}
			rn.warn("Could not queue the follow-up syncs an earlier refresh left pending; the next refresh tries again", "error", err.Error())
		}
	}
	if rn.state, err = rn.r.store.State(ctx, nil, rn.it.ID); err != nil {
		return jobs.Result{}, err
	}
	// Rows this attempt writes carry seen_at = runAt, and "not seen" means another seen_at: runAt
	// must differ from every earlier attempt's, even with a coarse or skewed clock.
	last, err := rn.r.store.lastSeen(ctx, rn.it.ID)
	if err != nil {
		return jobs.Result{}, err
	}
	if !rn.runAt.After(last) {
		rn.runAt = last.Add(time.Microsecond)
	}
	rn.instanceChanged = rn.state.InstanceID != rn.it.URL
	rn.first = rn.state.RefreshedAt == nil || rn.instanceChanged
	token, err := rn.r.o.Integrations.TokenFor(ctx, rn.it.ID, rn.it.URL)
	if errors.Is(err, integrations.ErrURLChanged) {
		return jobs.Result{}, fmt.Errorf("the URL of %s %q changed while the refresh started; the next refresh uses the new one", rn.app, rn.it.Name)
	}
	if err != nil {
		return jobs.Result{}, err
	}
	c, err := arr.New(arr.Kind(rn.it.Type), rn.it.URL, token, rn.r.o.Client)
	if err != nil {
		return jobs.Result{}, fmt.Errorf("%s %q: %w", rn.app, rn.it.Name, err)
	}
	rn.fetch = &fetcher{c: c, kind: arr.Kind(rn.it.Type), concurrency: rn.r.o.Concurrency}
	if rn.dry {
		rn.seenItems, rn.seenFiles = map[int64]bool{}, map[int64]bool{}
	}

	if len(rn.job.Params.ArrItemIDs) > 0 {
		rn.wantIntents = !rn.dry && rn.job.Params.SyncAfter
		return rn.targeted(ctx)
	}
	rn.wantIntents = !rn.dry && !rn.first && !rn.job.Params.SyncAfter
	if rn.job.Params.SyncAfter {
		rn.info("This refresh covers the whole library (more than the targeted limit of items was queued); when it ends it queues a sync of every source the root folders locate into")
		if !rn.dry {
			if err := rn.markRootFolderSync(ctx); err != nil {
				return jobs.Result{}, err
			}
		}
	}
	return rn.full(ctx)
}

// full is the full refresh (design §6.1).
func (rn *run) full(ctx context.Context) (jobs.Result, error) {
	itemsBefore, filesBefore, err := rn.r.store.counts(ctx, rn.it.ID)
	if err != nil {
		return jobs.Result{}, err
	}
	st, err := rn.status(ctx)
	if err != nil {
		return jobs.Result{}, err
	}
	rn.stats.AppVersion = st.Version
	m, err := rn.fetch.fetchMeta(ctx, true)
	if err != nil {
		return jobs.Result{}, err
	}
	rows := rn.metaRows(m)
	rn.checkMediaManagement(m.mediaManagement)
	if !rn.dry {
		if err := rn.begin(ctx, st.Version, rows); err != nil {
			return jobs.Result{}, err
		}
	}
	rn.env.Reporter.Progress(jobs.Progress{Phase: "refreshing"})
	err = rn.fetch.fetchAll(ctx, func(f fetched) error {
		rn.stats.Items++
		return rn.add(ctx, f)
	})
	if err == nil {
		err = rn.flush(ctx)
	}
	if err != nil {
		return jobs.Result{}, err
	}
	faultinject.Point(PointBeforeMarkDeleted)

	gone, err := rn.goneRows(ctx)
	if err != nil {
		return jobs.Result{}, err
	}
	decision := rn.guard(gone, itemsBefore, filesBefore)
	if decision.held != "" {
		rn.stats.GuardHeld, rn.stats.GuardReason = true, decision.held
		rn.warn("Refresh guard: nothing was removed from the index ("+decision.held+"). If "+rn.app+
			" really lost these, run the refresh again with \"Apply held changes\"; until then the index keeps them and its facts age into unknown",
			"items", len(gone.items), "files", len(gone.files))
	}
	rn.stats.ItemsDeleted = int64(len(decision.items))
	rn.stats.FilesDeleted = int64(len(decision.files))
	var goneIntents []intent
	for _, g := range decision.items {
		if g.hadFiles {
			goneIntents = append(goneIntents, intent{Kind: g.kind, ArrID: g.arrID, Title: g.title, OldFolder: g.path, RootFolder: g.root, Reason: "deleted"})
		}
	}
	if !rn.first {
		rn.stats.ChangedItems += int64(len(goneIntents))
	}
	rn.intents = append(rn.intents, goneIntents...)
	rn.finalizeStats()
	if !rn.dry {
		if rn.wantIntents && len(goneIntents) > 0 {
			if err := rn.persistIntents(ctx, goneIntents); err != nil {
				return jobs.Result{}, err
			}
		}
		if err := rn.finishFull(ctx, decision); err != nil {
			return jobs.Result{}, err
		}
	}
	if !rn.dry {
		var ferr error
		switch {
		case rn.job.Params.SyncAfter:
			ferr = rn.followUpUntargeted(ctx)
		case rn.wantIntents:
			ferr = rn.followUpIntents(ctx)
		}
		if err := rn.followUpError(ctx, ferr); err != nil {
			return jobs.Result{}, err
		}
	}
	return jobs.Result{Summary: rn.summary()}, nil
}

// followUpError turns the error of queuing the follow-up syncs into the job's: a cancel (or
// shutdown) meanwhile ends the job as cancelled (a shut-down job resumes and re-reads its pending
// intents; a cancelled webhook refresh has its events re-armed), anything else is a warning.
func (rn *run) followUpError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	rn.warn("Could not queue the follow-up syncs", "error", err.Error())
	return nil
}

// status reads system/status: the version, and ErrWrongApp for another application.
func (rn *run) status(ctx context.Context) (arr.Status, error) {
	rn.fetch.count(1)
	st, err := rn.fetch.c.Status(ctx)
	if err != nil {
		return arr.Status{}, fmt.Errorf("%s %q: %w", rn.app, rn.it.Name, err)
	}
	return st, nil
}

// targeted is the targeted refresh of Params.ArrItemIDs (design §6.1).
func (rn *run) targeted(ctx context.Context) (jobs.Result, error) {
	ids := rn.job.Params.ArrItemIDs
	if len(ids) > jobs.MaxTargetItems {
		return jobs.Result{}, fmt.Errorf("a targeted refresh names at most %d items, not %d", jobs.MaxTargetItems, len(ids))
	}
	var (
		found []fetched
		gone  []int64
	)
	err := pool(ctx, rn.r.o.Concurrency, len(ids), func(ctx context.Context, i int) (fetched, error) {
		f, ok, err := rn.fetch.fetchOne(ctx, ids[i])
		if !ok && errors.Is(err, errItemGone) {
			return fetched{arrID: ids[i]}, nil // kind "" marks a 404
		}
		return f, err
	}, func(f fetched) error {
		if f.kind == "" {
			gone = append(gone, f.arrID)
		} else {
			found = append(found, f)
		}
		return nil
	})
	if err != nil {
		return jobs.Result{}, fmt.Errorf("%s %q: %w", rn.app, rn.it.Name, err)
	}
	rn.stats.Items = int64(len(found))
	var version string
	if len(gone) > 0 {
		// A 404 means "deleted" only when the URL still answers as the expected application: a
		// proxy misroute or a changed URL base must not delete anything.
		st, err := rn.status(ctx)
		if err != nil {
			return jobs.Result{}, fmt.Errorf("%s answered 404 for %d item(s), but %w; nothing was marked deleted", rn.app, len(gone), err)
		}
		version = st.Version
		rn.stats.AppVersion = version
	}
	// An unknown tag or profile id makes the metadata be read again first.
	var metaRows []metaRow
	known, err := rn.r.store.knownMeta(ctx, rn.it.ID)
	if err != nil {
		return jobs.Result{}, err
	}
	if rn.instanceChanged || known.missing(found) {
		m, err := rn.fetch.fetchMeta(ctx, false)
		if err != nil {
			return jobs.Result{}, err
		}
		metaRows = rn.metaRows(m)
	} else {
		rn.inaccessible = known.inaccessible
	}
	slices.SortFunc(found, func(a, b fetched) int { return cmp.Compare(a.arrID, b.arrID) })
	slices.Sort(gone)
	if !rn.dry {
		if err := rn.begin(ctx, version, metaRows); err != nil {
			return jobs.Result{}, err
		}
	}
	for _, f := range found {
		if err := rn.add(ctx, f); err != nil {
			return jobs.Result{}, err
		}
	}
	if err := rn.flush(ctx); err != nil {
		return jobs.Result{}, err
	}
	if err := rn.markTargetedGone(ctx, gone); err != nil {
		return jobs.Result{}, err
	}
	if !rn.dry {
		if err := rn.r.store.recordTargeted(ctx, rn.it.ID, rn.runAt, version); err != nil {
			return jobs.Result{}, err
		}
	}
	rn.finalizeStats()
	if rn.wantIntents {
		if err := rn.followUpError(ctx, rn.followUpIntents(ctx)); err != nil {
			return jobs.Result{}, err
		}
	}
	return jobs.Result{Summary: rn.summary()}, nil
}

// checkMediaManagement warns about a recycle bin inside a source that its excludes do not cover,
// and about a file date setting other than none (design §6.1).
func (rn *run) checkMediaManagement(mm *arr.MediaManagement) {
	if mm == nil {
		return
	}
	rn.stats.FileDate = mm.FileDate
	if mm.FileDate != "" && !strings.EqualFold(mm.FileDate, "none") {
		rn.warn(fmt.Sprintf("%s sets each file's date (Change File Date: %s): every version of a title then has the same modification time, so an equal-size replacement looks unchanged. Set it to None in %s → Settings → Media Management",
			rn.app, mm.FileDate, rn.app))
	}
	if mm.RecycleBin == "" {
		return
	}
	rb := &RecycleBin{Path: mm.RecycleBin}
	rn.stats.RecycleBin = rb
	local, ok := rn.settings.MapPath(mm.RecycleBin)
	if !ok {
		return
	}
	rb.LocalPath = &local
	locs := rn.loc.Locate(local)
	if len(locs) == 0 {
		return
	}
	sid := locs[0].SourceID
	rb.SourceID, rb.RelPath = &sid, locs[0].Rel
	rb.Excluded = rn.recycleExcluded(locs[0])
	if !rb.Excluded {
		rn.warn(fmt.Sprintf("%s's recycle bin %s is inside a source that does not exclude it: every upgraded file would be backed up twice. Exclude /%s/ from that source (Settings → Connect offers it)",
			rn.app, mm.RecycleBin, rb.RelPath), "sourceId", sid)
	}
}

// recycleExcluded reports whether a source's excludes (its own and the defaults) skip a folder.
func (rn *run) recycleExcluded(l catalog.Location) bool {
	if l.Rel == "" {
		return false
	}
	src, err := rn.r.o.Catalog.Get(context.Background(), l.SourceID)
	if err != nil {
		return false
	}
	p, _ := rn.loc.Path(l)
	loc, ok := integrations.LocateInSources([]integrations.SourceRef{{ID: src.ID, Path: src.Path, Exclude: src.Exclude}},
		catalog.DefaultExcludes(), p)
	return ok && loc.Excluded
}

// mapLocate maps an *arr path and locates it: local is "" when no mapping applies; locs is empty
// when it lies in no source.
func (rn *run) mapLocate(p string) (local string, locs []catalog.Location) {
	local, ok := rn.settings.MapPath(p)
	if !ok {
		return "", nil
	}
	return local, rn.loc.Locate(local)
}

// countUnmapped counts a file that maps to no source under its root folder.
func (rn *run) countUnmapped(root, reason string) {
	k := root + "\x00" + reason
	u := rn.unmapped[k]
	if u == nil {
		u = &UnmappedFolder{RootFolder: root, Reason: reason}
		rn.unmapped[k] = u
	}
	u.Files++
}

// finalizeStats orders the unmapped folders (most files first, at most 20).
func (rn *run) finalizeStats() {
	list := make([]UnmappedFolder, 0, len(rn.unmapped))
	for _, u := range rn.unmapped {
		list = append(list, *u)
	}
	slices.SortFunc(list, func(a, b UnmappedFolder) int {
		return cmp.Or(cmp.Compare(b.Files, a.Files), strings.Compare(a.RootFolder, b.RootFolder), strings.Compare(a.Reason, b.Reason))
	})
	if len(list) > maxUnmappedFolders {
		list = list[:maxUnmappedFolders]
	}
	rn.stats.UnmappedFolders = list
	for _, u := range list {
		if u.Reason == ReasonUnmapped {
			rn.warn(fmt.Sprintf("%d %s file(s) under %s are not covered by a path mapping: add a mapping for %s", u.Files, rn.app, u.RootFolder, u.RootFolder))
		} else {
			rn.warn(fmt.Sprintf("%d %s file(s) under %s map into no source", u.Files, rn.app, u.RootFolder))
		}
	}
}

func (rn *run) summary() string {
	noun := map[string]string{KindMovie: "movies", KindSeries: "series", KindArtist: "artists"}[rn.kind]
	verb := "Refreshed"
	if rn.dry {
		verb = "Preview of the refresh of"
	}
	s := fmt.Sprintf("%s %s %q: %d %s, %d files (%d added, %d updated, %d removed)", verb, rn.app, rn.it.Name,
		rn.stats.Items, noun, rn.stats.Files, rn.stats.ItemsAdded, rn.stats.ItemsUpdated, rn.stats.ItemsDeleted)
	if rn.stats.GuardHeld {
		s += "; removals held by the refresh guard"
	}
	if n := len(rn.stats.FollowUpJobs); n > 0 {
		s += fmt.Sprintf("; queued %d sync(s)", n)
	}
	return s
}

// intent is a changed item whose folders a follow-up sync scans (design §6.1): stored as a job
// item (action skip, pending) before the index row changes, so a crash after the change still
// follows it up when the job resumes.
type intent struct {
	Kind  string `json:"kind"`
	ArrID int64  `json:"arrId"`
	Title string `json:"title"`
	// OldFolder is the folder the index had (""when the item is new); NewFolder the one it has now
	// ("" when the item was deleted). Both as the *arr sees them.
	OldFolder string `json:"oldFolder,omitempty"`
	NewFolder string `json:"newFolder,omitempty"`
	// RootFolder is the item's root folder (the mapping to add when its folder maps nowhere).
	RootFolder string `json:"rootFolder,omitempty"`
	// HasFiles: the item has files now, so its new folder must exist.
	HasFiles bool `json:"hasFiles"`
	// Reason is added, files, moved or deleted (the webhook path: requested).
	Reason string `json:"reason"`
}

// intentDetail is a job item's detail: a reconcile intent (ArrChange), or the mark of an
// overflow refresh that the untargeted sync of every source of the root folders is owed
// (SyncRootFolders, markRootFolderSync).
type intentDetail struct {
	ArrChange       *intent `json:"arrChange,omitempty"`
	SyncRootFolders bool    `json:"syncRootFolders,omitempty"`
}

func (in intent) folder() string {
	if in.NewFolder != "" {
		return in.NewFolder
	}
	return in.OldFolder
}

// persistIntents stores intents as pending job items.
func (rn *run) persistIntents(ctx context.Context, list []intent) error {
	items := make([]jobs.Item, 0, len(list))
	for _, in := range list {
		d, err := json.Marshal(intentDetail{ArrChange: &in})
		if err != nil {
			return err
		}
		items = append(items, jobs.Item{RelPath: in.folder(), Action: jobs.ActionSkip, Status: jobs.ItemPending, Detail: d})
	}
	if err := rn.env.Items.AddItems(ctx, rn.job.ID, items, false); err != nil {
		return fmt.Errorf("record the changed items: %w", err)
	}
	faultinject.Point(PointAfterIntents)
	return nil
}

// markRootFolderSync records, before an overflow refresh writes to the index, that the untargeted
// sync of every source of the root folders is owed (Params.SyncAfter: also when the refresh
// fails). The mark is a pending job item: the runner settles it when it queues that sync, and
// followUpStranded finds it when the job ends without its runner (start-up recovery failed it
// after crashes, or the runner panicked). An overflow refresh records no intents, so without the
// mark nothing would sync the imports it absorbed once the index has them. A resumed attempt keeps
// the mark it has.
func (rn *run) markRootFolderSync(ctx context.Context) error {
	_, _, marks, err := rn.pendingIntents(ctx)
	if err != nil || len(marks) > 0 {
		return err
	}
	d, err := json.Marshal(intentDetail{SyncRootFolders: true})
	if err != nil {
		return err
	}
	if err := rn.env.Items.AddItems(ctx, rn.job.ID, []jobs.Item{{Action: jobs.ActionSkip, Status: jobs.ItemPending, Detail: d}}, false); err != nil {
		return fmt.Errorf("record the follow-up syncs: %w", err)
	}
	return nil
}

// pendingIntents reads the intents stored by this job (this attempt and earlier ones) that were
// not followed up yet, and its pending root-folder sync marks.
func (rn *run) pendingIntents(ctx context.Context) ([]intent, []int64, []int64, error) {
	return rn.intentsOf(ctx, rn.job.ID)
}

// intentsOf reads the intents job jobID stored that were not followed up yet and their item ids,
// and the item ids of its pending root-folder sync marks (markRootFolderSync).
func (rn *run) intentsOf(ctx context.Context, jobID int64) ([]intent, []int64, []int64, error) {
	var (
		out   []intent
		ids   []int64
		marks []int64
		after int64
	)
	for {
		items, err := rn.env.Items.Pending(ctx, jobID, after, 500)
		if err != nil {
			return nil, nil, nil, err
		}
		if len(items) == 0 {
			return out, ids, marks, nil
		}
		for _, it := range items {
			after = it.ID
			var d intentDetail
			if json.Unmarshal(it.Detail, &d) != nil {
				continue
			}
			switch {
			case d.SyncRootFolders:
				marks = append(marks, it.ID)
			case d.ArrChange != nil && d.ArrChange.ArrID != 0:
				out = append(out, *d.ArrChange)
				ids = append(ids, it.ID)
			}
		}
	}
}

// under reports whether p is root or lies under it (on a segment boundary).
func under(p, root string) bool {
	root = strings.TrimRight(root, "/")
	if root == "" {
		return strings.HasPrefix(p, "/")
	}
	return p == root || strings.HasPrefix(p, root+"/")
}

// cleanArrPath returns p cleaned when it is absolute ("" otherwise).
func cleanArrPath(p string) string {
	if !path.IsAbs(p) {
		return ""
	}
	return path.Clean(p)
}
