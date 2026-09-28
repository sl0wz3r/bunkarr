package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sl0wz3r/bunkarr/internal/config"
	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/enginerun"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
	"github.com/sl0wz3r/bunkarr/internal/engines/rclone"
	"github.com/sl0wz3r/bunkarr/internal/engines/restic"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/syncer"
)

// The restic and rclone engines (docs/design/phase4.md §3.4, §4.4, §9.3, §10.1, §11.1): their
// availability found at start-up, the run directories swept before any command runs, the upload
// slots of engine syncs, the job gate of engine destinations (S21), the engine settings and the
// daily recovery kit reminder.

// Setting keys of the engines (settings table, plain values; phase4.md §13.1).
const (
	// SettingEnginesUploadSlots is how many engine syncs (not dry runs) upload at once (1-8,
	// default 2). Read at start-up: a change applies after a restart (§9.3).
	SettingEnginesUploadSlots = "engines.uploadSlots"
	// SettingEnginesRetryBudgetMinutes is how long an engine command may keep printing "returned
	// error, retrying" lines before it is stopped (1-1440 minutes, default 10; S26). Read by every
	// job.
	SettingEnginesRetryBudgetMinutes = "engines.retryBudgetMinutes"
)

// Engine setting defaults and ranges.
const (
	defaultUploadSlots        = 2
	maxUploadSlots            = 8
	defaultRetryBudgetMinutes = 10
	maxRetryBudgetMinutes     = 1440
	// uploadSlotPool names the jobqueue slot pool of engine syncs.
	uploadSlotPool = "upload"
	// DefaultEngineRetentionCron is an engine destination's retention schedule when it is created
	// without one: daily 04:30, the time of the global retention job (whose per-destination job it
	// then is, deduplicated).
	DefaultEngineRetentionCron = "30 4 * * *"
)

// EngineOptions is AppOptions.Engines: the exec runner and what discovery found at start-up
// (cmd/bunkarr: config.ResolveEngineBinaries, proc.NewExecRunner, engines.Discover).
type EngineOptions struct {
	// Runner runs every engine command: proc.NewExecRunner with the resolved binaries in
	// production, enginetest.FakeRunner in tests. nil: neither engine is available.
	Runner proc.Runner
	// Restic and Rclone are the binaries config.ResolveEngineBinaries found; restic's rclone
	// backend runs Rclone.Path (-o rclone.program=).
	Restic, Rclone config.EngineBinary
	// Availability is what engines.Discover found (GET /system/status, the create and test
	// refusals, the job gate of §11.1).
	Availability engines.Availability
	// RunDirs are the run directories of engine commands (secret files on tmpfs, S22); nil makes
	// them in the config directory with /dev/shm. NewApp sweeps what a crash left in them.
	RunDirs *proc.RunDirs
	// HostName and ProcessStart identify this process's restic locks (§6.7): the container's host
	// name and this process's start (defaults: os.Hostname and the time NewApp runs).
	HostName     string
	ProcessStart time.Time
	// CheckHost refuses a host an engine would dial when it is, or resolves to, a metadata or
	// link-local address (nil: netguard.CheckHost). Tests replace it.
	CheckHost func(ctx context.Context, host string) error
}

// engineWiring is the App's engine state that does not change after start-up.
type engineWiring struct {
	opts         EngineOptions
	availability engines.Availability
	runDirs      *proc.RunDirs
	hostName     string
	processStart time.Time
	// uploadSlots is the upload slot count in effect (read at start-up).
	uploadSlots int
	// gates keep the unlock endpoint and the version stores of a destination apart.
	gates *versionGates
	// resticCache is <config>/cache/restic, which holds each restic destination's cache in
	// <id> (RESTIC_CACHE_DIR).
	resticCache string
}

// unavailable is the status of an engine that has no runner.
func unavailable(name string, st engines.BinaryStatus) engines.BinaryStatus {
	if st.Reason == "" {
		st.Reason = name + " is not installed"
	}
	st.Available = false
	return st
}

// newEngineWiring settles o.Engines' defaults and sweeps the run directories: a crash or a kill
// -9 during an engine command leaves its secret files behind (S22). It runs before the HTTP
// listener and the job manager start, so no command of this process is running.
func newEngineWiring(o AppOptions, configDir string, log *slog.Logger) *engineWiring {
	e := o.Engines
	w := &engineWiring{opts: e, availability: e.Availability, runDirs: e.RunDirs, hostName: e.HostName, processStart: e.ProcessStart,
		gates: newVersionGates(), resticCache: filepath.Join(configDir, "cache", "restic")}
	if e.Runner == nil {
		w.availability = engines.Availability{Restic: unavailable("restic", e.Availability.Restic), Rclone: unavailable("rclone", e.Availability.Rclone)}
	}
	if w.runDirs == nil {
		w.runDirs = proc.NewRunDirs(configDir, proc.RunDirOptions{})
	}
	if err := w.runDirs.Sweep(); err != nil {
		log.Warn("Could not remove the run directories an earlier run left (they may hold secret files)", "error", err)
	}
	if w.hostName == "" {
		if h, err := os.Hostname(); err == nil {
			w.hostName = h
		}
	}
	if w.processStart.IsZero() {
		w.processStart = time.Now()
	}
	return w
}

// EngineAvailability returns what discovery found for restic and rclone at start-up.
func (a *App) EngineAvailability() engines.Availability { return a.engines.availability }

// engineRegistry returns the engines the destinations store creates, tests and attaches through:
// the engine service's (bound late: the service needs the store), or the test hook.
func (a *App) engineRegistry(o AppOptions) func(engines.Kind) (engines.Engine, bool) {
	if o.engineRegistry != nil {
		return o.engineRegistry
	}
	return func(k engines.Kind) (engines.Engine, bool) {
		if a.Engine == nil {
			return nil, false
		}
		return a.Engine.Engines()(k)
	}
}

// versionOpener returns the version store opener of the Plex DB, *arr and manifest runners
// (phase4.md §8.4): the engine service's, or the test hook, behind the destination's version gate
// (versionGates): no version store of a destination opens while its locks are being removed.
func (a *App) versionOpener(o AppOptions) engines.VersionOpener {
	open := o.openVersions
	if open == nil {
		open = func(ctx context.Context, destinationID int64, rt engines.Runtime) (engines.VersionStore, io.Closer, error) {
			if a.Engine == nil {
				return nil, nil, errors.New("the engines are not wired")
			}
			return a.Engine.OpenVersions(ctx, destinationID, rt)
		}
	}
	return func(ctx context.Context, destinationID int64, rt engines.Runtime) (engines.VersionStore, io.Closer, error) {
		leave, err := a.engines.gates.enter(ctx, destinationID)
		if err != nil {
			return nil, nil, err
		}
		vs, c, err := open(ctx, destinationID, rt)
		if err != nil {
			leave()
			return nil, nil, err
		}
		return vs, gatedCloser{c: c, leave: leave}, nil
	}
}

// gatedCloser closes a version store's session, then leaves the destination's version gate.
type gatedCloser struct {
	c     io.Closer
	leave func()
}

func (g gatedCloser) Close() error {
	defer g.leave()
	if g.c == nil {
		return nil
	}
	return g.c.Close()
}

// versionGates keep the unlock endpoint (§6.7) and the version stores of the Plex DB, *arr and
// manifest jobs (and downloads) of a destination apart. Those jobs hold no dest:<id> key, so the
// dispatcher may start one while restic unlock --remove-all runs; the unlock would then remove
// the new backup's lock, and a retention job's prune could delete the packs that backup has
// uploaded but not yet indexed. A version store opens only while no unlock of its destination
// runs (it waits for the unlock), and an unlock starts only while no version store of its
// destination is open.
type versionGates struct {
	mu sync.Mutex
	// open counts the open version stores per destination.
	open map[int64]int
	// unlocking holds a channel per destination whose locks are being removed, closed when done.
	unlocking map[int64]chan struct{}
}

func newVersionGates() *versionGates {
	return &versionGates{open: map[int64]int{}, unlocking: map[int64]chan struct{}{}}
}

// enter waits until no unlock of destination id runs, then counts a version store of it open.
// leave (safe to call more than once) ends that.
func (g *versionGates) enter(ctx context.Context, id int64) (leave func(), err error) {
	for {
		g.mu.Lock()
		done, busy := g.unlocking[id]
		if !busy {
			g.open[id]++
			g.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					g.mu.Lock()
					if g.open[id]--; g.open[id] <= 0 {
						delete(g.open, id)
					}
					g.mu.Unlock()
				})
			}, nil
		}
		g.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	}
}

// lockOut starts an unlock of destination id: it fails (false) while a version store of it is
// open or another unlock of it runs; otherwise no version store of it opens until release.
func (g *versionGates) lockOut(id int64) (release func(), ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.open[id] > 0 || g.unlocking[id] != nil {
		return nil, false
	}
	done := make(chan struct{})
	g.unlocking[id] = done
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			delete(g.unlocking, id)
			g.mu.Unlock()
			close(done)
		})
	}, true
}

// newEngineService builds the restic and rclone drivers over the exec runner and the engine
// service that dispatches sync, verify and retention jobs (filecopy to fc).
func (a *App) newEngineService(o AppOptions, configDir string, planner *syncer.Planner, fc enginerun.FilecopyRunners) *enginerun.Service {
	w := a.engines
	eo := enginerun.Options{
		DB:           o.DB,
		Files:        a.Files,
		Planner:      planner,
		Catalog:      a.Catalog,
		Scanner:      a.Scanner,
		Destinations: a.Destinations,
		Filecopy:     fc,
		Availability: func() engines.Availability { return w.availability },
		ConfigDir:    configDir,
		RunDirs:      w.runDirs,
		RetryBudget:  a.retryBudget,
		Logger:       a.log.With("component", "engines"),
		Location:     o.Location,
		HostName:     w.hostName,
		ProcessStart: w.processStart,
		CheckHost:    w.opts.CheckHost,
		Tiers:        a.Tiers,
	}
	if r := w.opts.Runner; r != nil {
		rc := &rclone.Driver{Runner: r, RunDirs: w.runDirs, Log: a.log.With("component", "rclone"), Version: w.availability.Rclone.Version}
		rclonePath := ""
		if w.opts.Rclone.Available() {
			rclonePath = w.opts.Rclone.Path
		}
		eo.Runner = r
		eo.Rclone = rc
		eo.Restic = &restic.Driver{Runner: r, RunDirs: w.runDirs, Rclone: rc, CacheRoot: w.resticCache,
			RclonePath: rclonePath, Log: a.log.With("component", "restic"), Version: w.availability.Restic.Version}
	}
	return enginerun.New(eo)
}

// retryBudget is engines.retryBudgetMinutes (§10.1, S26).
func (a *App) retryBudget(ctx context.Context) time.Duration {
	return time.Duration(a.intSetting(ctx, SettingEnginesRetryBudgetMinutes, defaultRetryBudgetMinutes, 1, maxRetryBudgetMinutes)) * time.Minute
}

// uploadSlotPool reads engines.uploadSlots and returns the pool every engine sync that is not a
// dry run needs a slot of (§9.3, D27). Filecopy syncs, verify, retention and config version jobs
// hold none.
func (a *App) uploadSlotPool(ctx context.Context) jobqueue.SlotPool {
	a.engines.uploadSlots = a.intSetting(ctx, SettingEnginesUploadSlots, defaultUploadSlots, 1, maxUploadSlots)
	return jobqueue.SlotPool{Name: uploadSlotPool, Limit: a.engines.uploadSlots, Needs: a.needsUploadSlot}
}

// needsUploadSlot is the upload pool's Needs: an engine sync that is not a dry run.
func (a *App) needsUploadSlot(ctx context.Context, job jobs.Job) (bool, error) {
	if job.Type != jobs.TypeSync || job.DryRun || job.Params.DestinationID == 0 {
		return false, nil
	}
	d, err := a.Destinations.Get(ctx, job.Params.DestinationID)
	if errors.Is(err, destinations.ErrNotFound) {
		return false, nil // the runner fails it on its Phase 1 path
	}
	if err != nil {
		return false, err
	}
	return d.IsEngine(), nil
}

// otherJobsPool names the jobqueue slot pool of the jobs that take no upload slot.
const otherJobsPool = "other"

// jobPools returns the job manager's worker count and slot pools for jobs.workers = workers: the
// engine syncs run on upload slots of their own, next to the jobs.workers workers of every other
// job (refresh jobs keep their own pool). An off-site seed can hold its upload slots for days at a
// bandwidth limit; were those slots workers of jobs.workers, the default two seeds would hold
// both default workers, and the nightly filecopy sync, the Plex DB and *arr backups and the
// manifest exports would wait for them. The manager runs workers + upload slots jobs at once;
// the "other" pool keeps the jobs that take no upload slot to jobs.workers, as before Phase 4.
func (a *App) jobPools(ctx context.Context, workers int) (int, []jobqueue.SlotPool) {
	upload := a.uploadSlotPool(ctx)
	other := jobqueue.SlotPool{Name: otherJobsPool, Limit: workers, Needs: func(ctx context.Context, job jobs.Job) (bool, error) {
		if job.Type == jobs.TypeRefresh {
			return false, nil // the refresh pool
		}
		up, err := a.needsUploadSlot(ctx, job)
		return !up, err
	}}
	return workers + upload.Limit, []jobqueue.SlotPool{upload, other}
}

// backupDestination is integrations.ValidateOptions.Destination: what the S17 check of a Plex DB
// or *arr backup target needs to know about its destination (§8.4 step 6).
func (a *App) backupDestination(ctx context.Context, id int64) (engines.DestKind, engines.EncryptionMode, bool, error) {
	d, err := a.Destinations.Get(ctx, id)
	if errors.Is(err, destinations.ErrNotFound) {
		return "", "", false, fmt.Errorf("%w: %d", integrations.ErrNoDestination, id)
	}
	if err != nil {
		return "", "", false, err
	}
	return d.Kind, d.Encryption.Mode, d.Capabilities.EnforcesModes, nil
}

// engineBlock returns why no job of destination d but a dry run may run, or "" (S21, S25, §11.1):
// its create did not finish, its recovery kit custody is not confirmed ("export and confirm the
// recovery kit first"), or its engine is not available on this server. Filecopy destinations are
// never blocked here.
func (a *App) engineBlock(d destinations.Destination) string {
	if !d.IsEngine() {
		return ""
	}
	if reason := destinations.Blocked(d); reason != "" {
		return reason
	}
	if st := a.engines.availability.Of(engines.Kind(d.Engine)); !st.Available {
		if st.Reason != "" {
			return st.Reason
		}
		return d.Engine + " is not available on this server"
	}
	return a.resticRcloneBlock(d.Engine, d.Kind)
}

// resticRcloneBlock returns why restic cannot reach a repository of kind on this server, or "":
// restic reaches every remote repository through its rclone backend (D21), so a restic
// destination on sftp, s3 or b2 needs rclone too. Local restic repositories and the other engines
// never get a reason here.
func (a *App) resticRcloneBlock(engine string, kind engines.DestKind) string {
	if engine != destinations.EngineRestic || !kind.Remote() {
		return ""
	}
	st := a.engines.availability.Rclone
	if st.Available {
		return ""
	}
	reason := st.Reason
	if reason == "" {
		reason = "rclone is not available on this server"
	}
	return fmt.Sprintf("restic reaches %s repositories through rclone: %s", kind, reason)
}

// destinationBlock is engineBlock for destination id, naming it: "" when it can run, or does not
// exist (the runner's Phase 1 error path reports that).
func (a *App) destinationBlock(ctx context.Context, id int64) string {
	if id == 0 {
		return ""
	}
	d, err := a.Destinations.Get(ctx, id)
	if err != nil {
		return ""
	}
	if reason := a.engineBlock(d); reason != "" {
		return fmt.Sprintf("destination %q: %s", d.Name, reason)
	}
	return ""
}

// removeResticCache removes a deleted restic destination's cache (<config>/cache/restic/<id>),
// which can reach several GiB: destination ids are never reused, so nothing else would. Best
// effort: a failure is logged.
func (a *App) removeResticCache(d destinations.Destination) {
	if d.Engine != destinations.EngineRestic || d.ID <= 0 || a.engines.resticCache == "" {
		return
	}
	dir := filepath.Join(a.engines.resticCache, strconv.FormatInt(d.ID, 10))
	if err := os.RemoveAll(dir); err != nil {
		a.log.Warn("Could not remove the restic cache of a deleted destination", "destinationId", d.ID, "path", dir, "error", err)
	}
}

// checkRunnable refuses a manual job of destination d that is not a dry run while engineBlock
// blocks it (409, S21, §11.1).
func (a *App) checkRunnable(d destinations.Destination, dryRun bool) error {
	if dryRun {
		return nil
	}
	if reason := a.engineBlock(d); reason != "" {
		return errorf(http.StatusConflict, "destination %q: %s", d.Name, reason)
	}
	return nil
}

// --- settings ---

// engineSettings is GET and PUT /settings/engines (phase4.md §12).
type engineSettings struct {
	// UploadSlots is engines.uploadSlots (1-8): how many engine syncs upload at once.
	UploadSlots int `json:"uploadSlots"`
	// RetryBudgetMinutes is engines.retryBudgetMinutes (1-1440).
	RetryBudgetMinutes int `json:"retryBudgetMinutes"`
	// UploadSlotsInEffect is the slot count the job manager uses now (read at start-up), and
	// RestartRequired is set while it differs from UploadSlots.
	UploadSlotsInEffect int  `json:"uploadSlotsInEffect"`
	RestartRequired     bool `json:"restartRequired"`
	// Note says when a changed slot count applies.
	Note string `json:"note"`
}

// engineSettingsNote is engineSettings.Note.
const engineSettingsNote = "The number of upload slots is read when Bunkarr starts: a change applies after a restart. The retry budget applies to the next job."

func (s *Server) engineSettingsRoutes(r chi.Router) {
	r.Get("/settings/engines", s.getEngineSettings)
	r.Put("/settings/engines", s.putEngineSettings)
}

func (s *Server) engineSettings(ctx context.Context) engineSettings {
	a := s.app
	v := engineSettings{
		UploadSlots:         a.intSetting(ctx, SettingEnginesUploadSlots, defaultUploadSlots, 1, maxUploadSlots),
		RetryBudgetMinutes:  a.intSetting(ctx, SettingEnginesRetryBudgetMinutes, defaultRetryBudgetMinutes, 1, maxRetryBudgetMinutes),
		UploadSlotsInEffect: a.engines.uploadSlots,
		Note:                engineSettingsNote,
	}
	v.RestartRequired = v.UploadSlots != v.UploadSlotsInEffect
	return v
}

func (s *Server) getEngineSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.engineSettings(r.Context()))
}

func (s *Server) putEngineSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UploadSlots        *int `json:"uploadSlots"`
		RetryBudgetMinutes *int `json:"retryBudgetMinutes"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		s.fail(w, r, "save engine settings", err)
		return
	}
	if s.app.settings == nil {
		s.fail(w, r, "save engine settings", errors.New("no settings store"))
		return
	}
	for _, f := range []struct {
		name    string
		v       *int
		max     int
		setting string
	}{
		{"uploadSlots", body.UploadSlots, maxUploadSlots, SettingEnginesUploadSlots},
		{"retryBudgetMinutes", body.RetryBudgetMinutes, maxRetryBudgetMinutes, SettingEnginesRetryBudgetMinutes},
	} {
		if f.v != nil && (*f.v < 1 || *f.v > f.max) {
			s.fail(w, r, "save engine settings", errorf(http.StatusBadRequest, "%s must be a whole number from 1 to %d", f.name, f.max))
			return
		}
	}
	ctx := r.Context()
	for _, f := range []struct {
		v       *int
		setting string
	}{{body.UploadSlots, SettingEnginesUploadSlots}, {body.RetryBudgetMinutes, SettingEnginesRetryBudgetMinutes}} {
		if f.v == nil {
			continue
		}
		if err := s.app.settings.Set(ctx, f.setting, strconv.Itoa(*f.v)); err != nil {
			s.fail(w, r, "save engine settings", err)
			return
		}
	}
	v := s.engineSettings(ctx)
	s.log.Info("Engine settings changed", "uploadSlots", v.UploadSlots, "retryBudgetMinutes", v.RetryBudgetMinutes,
		"restartRequired", v.RestartRequired)
	writeJSON(w, http.StatusOK, v)
}

// --- recovery kit reminders ---

// Kit reminder timing: the destinations are checked every kitReminderCheck, and a destination
// whose recovery kit custody is not confirmed gets a warning once per kitReminderInterval, the
// first one kitReminderInterval after it was created (S21, §5.2, §11.5).
const (
	kitReminderCheck    = time.Hour
	kitReminderInterval = 24 * time.Hour
	// kitReminderFirst delays the first check after start-up.
	kitReminderFirst = time.Minute
)

// kitReminders sends "Recovery kit not confirmed for <destination>" (a warning) once a day per
// destination while its recovery kit custody is not confirmed, so nothing but dry runs runs for it.
// The last reminder of each destination is kept in memory only: after a restart a destination
// older than a day is reminded at the first check.
type kitReminders struct {
	a   *App
	now func() time.Time

	mu     sync.Mutex
	sent   map[int64]time.Time
	cancel context.CancelFunc
	done   chan struct{}
}

func newKitReminders(a *App) *kitReminders {
	return &kitReminders{a: a, now: time.Now, sent: map[int64]time.Time{}}
}

// start runs the reminder loop until stop.
func (k *kitReminders) start() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	k.cancel, k.done = cancel, make(chan struct{})
	go func() {
		defer close(k.done)
		t := time.NewTimer(kitReminderFirst)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				k.check(ctx)
				t.Reset(kitReminderCheck)
			}
		}
	}()
}

// stop ends the loop and waits for it (safe when it never started).
func (k *kitReminders) stop() {
	k.mu.Lock()
	cancel, done := k.cancel, k.done
	k.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

// check sends the reminders that are due.
func (k *kitReminders) check(ctx context.Context) {
	list, err := k.a.Destinations.List(ctx)
	if err != nil {
		if ctx.Err() == nil {
			k.a.log.Warn("Could not check the recovery kits of the destinations", "error", err)
		}
		return
	}
	now := k.now()
	k.mu.Lock()
	defer k.mu.Unlock()
	live := map[int64]bool{}
	for _, d := range list {
		live[d.ID] = true
		if !d.HasSecret() || d.Encryption.KitConfirmedAt != nil {
			delete(k.sent, d.ID)
			continue
		}
		since := d.CreatedAt
		if last, ok := k.sent[d.ID]; ok && last.After(since) {
			since = last
		}
		if now.Sub(since) < kitReminderInterval {
			continue
		}
		k.sent[d.ID] = now
		k.a.log.Warn("Recovery kit not confirmed: the destination runs no backup until it is", "destinationId", d.ID, "destination", d.Name)
		k.a.Notifier.Warn("Recovery kit not confirmed for "+d.Name, fmt.Sprintf("Destination %q runs no backup until its recovery kit custody "+
			"is confirmed: export the recovery kit and type its check code (or, for an encryption password you chose, type it again). "+
			"Without the kit, a lost /config makes this backup unreadable.", d.Name))
	}
	for id := range k.sent {
		if !live[id] {
			delete(k.sent, id)
		}
	}
}
