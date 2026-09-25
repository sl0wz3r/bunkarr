package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/config"
	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/integrations/plex"
	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/notify"
	"github.com/sl0wz3r/bunkarr/internal/plexdb"
	"github.com/sl0wz3r/bunkarr/internal/syncer"
)

// Setting keys of the job system (settings table, plain values).
const (
	// SettingJobsWorkers is how many jobs run at once (default jobqueue.DefaultWorkers). Read at
	// start-up.
	SettingJobsWorkers = "jobs.workers"
	// SettingJobsHistoryDays is how many days finished jobs are kept (default 90). Read by every
	// global retention job.
	SettingJobsHistoryDays = "jobs.historyDays"
)

// Defaults of the schedules the API creates (design §4.5, §5).
const (
	// DefaultVerifyCron is a new destination's verify schedule: weekly, Sunday 05:00.
	DefaultVerifyCron = "0 5 * * 0"
	// DefaultPlexBackupCron is a Plex DB backup's schedule when it is enabled without one: daily
	// 06:00, outside Plex's default butler window (02-05).
	DefaultPlexBackupCron = "0 6 * * *"
	// defaultHistoryDays is SettingJobsHistoryDays' default.
	defaultHistoryDays = 90
	// maxWorkers bounds SettingJobsWorkers.
	maxWorkers = 32
)

// AppOptions configures NewApp.
type AppOptions struct {
	// DB is Bunkarr's database (required).
	DB *db.DB
	// Keyring seals integration tokens and notification URLs (ADR 0003).
	Keyring *config.Keyring
	// Settings is the settings store (jobs.workers, jobs.historyDays).
	Settings *config.Settings
	// ConfigDir is Bunkarr's absolute config directory: never a source or destination (S4),
	// home of the Plex DB staging directories.
	ConfigDir string
	// Log receives server-side events; nil discards them.
	Log *slog.Logger
	// Workers overrides the jobs.workers setting when > 0.
	Workers int
	// Location is the time zone of schedules and Plex backup pruning; nil means time.Local.
	Location *time.Location
	// Plex configures the Plex API client (tests shorten the timeout); zero values are defaults.
	Plex plex.Options
	// Notify configures the notification dispatcher (Describe is always set by NewApp).
	Notify notify.Options
	// ProgressEvery and ShutdownGrace are passed to the job manager (zero: its defaults).
	ProgressEvery time.Duration
	ShutdownGrace time.Duration
}

// App is Bunkarr's wired Phase 1 services: every store, the job manager with its runners, the
// scheduler and the notification dispatcher. Create it with NewApp, Start it once the HTTP
// listener is up and Stop it after the HTTP server stopped accepting requests.
type App struct {
	// Integrations stores Plex (and later *arr) connections; tokens are sealed.
	Integrations *integrations.Store
	// Catalog stores sources and their file catalog; its path guard enforces S4.
	Catalog *catalog.Store
	// Scanner scans sources (skipping destination targets and the config directory).
	Scanner *catalog.Scanner
	// Destinations stores destinations; its path guard enforces S4 against the sources.
	Destinations *destinations.Store
	// Files is what each destination holds (destination_files).
	Files *syncer.Store
	// Snapshots are the recorded Plex DB versions.
	Snapshots *plexdb.Store
	// Notifications stores the Apprise targets; Notifier sends to them when jobs finish.
	Notifications *notify.Store
	Notifier      *notify.Dispatcher
	// Jobs is the job manager with every runner registered; Scheduler enqueues from the
	// schedules table.
	Jobs      *jobqueue.Manager
	Scheduler *jobqueue.Scheduler

	settings *config.Settings
	log      *slog.Logger
	plexOpts plex.Options
}

// NewApp constructs every Phase 1 service and wires them together: the S4 path guards between
// sources, destinations and the config directory; the catalog's backup check; the job runners
// (scan, sync, verify, retention, plexdb_backup); notifications on every finished job. It also
// registers the stored integration tokens and notification URLs as secrets, so they are redacted
// from logs from now on. Nothing runs until Start.
func NewApp(ctx context.Context, o AppOptions) (*App, error) {
	switch {
	case o.DB == nil:
		return nil, errors.New("app: no database")
	case o.Keyring == nil:
		return nil, errors.New("app: no keyring")
	case !filepath.IsAbs(o.ConfigDir):
		return nil, fmt.Errorf("app: the config directory %q is not an absolute path", o.ConfigDir)
	}
	log := o.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	configDir := filepath.Clean(o.ConfigDir)
	if r, err := filepath.EvalSymlinks(configDir); err == nil {
		configDir = r
	}
	a := &App{settings: o.Settings, log: log, plexOpts: o.Plex}

	guards := &pathGuards{configDir: configDir}
	a.Files = syncer.NewStore(o.DB)
	a.Catalog = catalog.NewStore(o.DB, catalog.StoreOptions{
		PathGuard:  guards.source,
		HasBackups: a.Files.HasBackups,
		// A source is busy while a job names it (a scan) or works on a destination it is linked to
		// (a sync scans and plans it). a.Jobs and a.Destinations are set below, before any request.
		ActiveJobs: func(ctx context.Context, sourceID int64) (bool, error) {
			if active, err := a.Jobs.Store().ActiveForSource(ctx, sourceID); err != nil || active {
				return active, err
			}
			dests, err := a.Destinations.List(ctx)
			if err != nil {
				return false, err
			}
			for _, d := range dests {
				if !slices.Contains(d.SourceIDs, sourceID) {
					continue
				}
				if active, err := a.Jobs.Store().ActiveForDestination(ctx, d.ID); err != nil || active {
					return active, err
				}
			}
			return false, nil
		},
	})
	localDevs, err := destinations.DevsOf("/", configDir)
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	a.Destinations = destinations.New(o.DB, destinations.Options{
		LocalDevs: localDevs,
		ConfigDir: configDir,
		PathGuard: guards.destination,
		Log:       log.With("component", "destinations"),
	})
	guards.sources, guards.dests = a.Catalog, a.Destinations
	a.Scanner = catalog.NewScanner(a.Catalog, catalog.ScannerOptions{ForbiddenRoots: guards.forbiddenRoots, Logger: log.With("component", "scanner")})
	a.Integrations = integrations.NewStore(o.DB, o.Keyring)
	a.Notifications = notify.NewStore(o.DB, o.Keyring)
	if err := a.Integrations.RegisterSecrets(ctx); err != nil {
		log.Warn("Some integration tokens could not be decrypted", "error", err)
	}
	if err := a.Notifications.RegisterSecrets(ctx); err != nil {
		log.Warn("Some notification URLs could not be decrypted", "error", err)
	}

	workers := o.Workers
	if workers <= 0 {
		workers = a.intSetting(ctx, SettingJobsWorkers, jobqueue.DefaultWorkers, 1, maxWorkers)
	}
	a.Jobs = jobqueue.New(o.DB, log.With("component", "jobs"), jobqueue.Options{
		Workers:       workers,
		ProgressEvery: o.ProgressEvery,
		ShutdownGrace: o.ShutdownGrace,
	})
	so := syncer.Options{DB: o.DB, Store: a.Files, Catalog: a.Catalog, Scanner: a.Scanner, Destinations: a.Destinations,
		Logger: log.With("component", "syncer")}
	a.Jobs.Register(jobs.TypeScan, catalog.NewScanRunner(a.Scanner))
	a.Jobs.Register(jobs.TypeSync, syncer.NewSyncRunner(so))
	a.Jobs.Register(jobs.TypeVerify, syncer.NewVerifyRunner(so))
	a.Jobs.Register(jobs.TypeRetention, syncer.NewRetentionRunner(syncer.RetentionOptions{
		Options:      so,
		Enqueuer:     a.Jobs,
		PruneHistory: a.Jobs.Store().PruneHistory,
		HistoryDays: func(ctx context.Context) (int, error) {
			return a.intSetting(ctx, SettingJobsHistoryDays, defaultHistoryDays, 1, 36500), nil
		},
	}))
	pr, err := plexdb.NewRunner(plexdb.Options{
		DB:           o.DB,
		Integrations: a.Integrations,
		Destinations: a.Destinations,
		ConfigDir:    configDir,
		Log:          log.With("component", "plexdb"),
		Location:     o.Location,
		Plex:         o.Plex,
	})
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	a.Jobs.Register(jobs.TypePlexDBBackup, pr)
	a.Snapshots = pr.Store()

	no := o.Notify
	no.Describe = func(ctx context.Context, job jobs.Job) string { return a.describe(ctx, job.Type, job.Params) }
	a.Notifier = notify.New(a.Notifications, log.With("component", "notify"), no)
	a.Jobs.OnFinish(a.Notifier.Handle)
	a.Scheduler = jobqueue.NewScheduler(a.Jobs.Store(), scheduleGate{a: a}, log.With("component", "scheduler"), o.Location)
	return a, nil
}

// Start removes schedules whose destination, integration or source no longer exists, then starts
// the job manager (which first recovers jobs a crash left running) and the scheduler.
func (a *App) Start(ctx context.Context) error {
	if err := a.pruneOrphanSchedules(ctx); err != nil {
		a.log.Warn("Could not check the schedules for deleted destinations and integrations", "error", err)
	}
	if err := a.Jobs.Start(ctx); err != nil {
		return fmt.Errorf("start the job manager: %w", err)
	}
	// The scheduler runs until Stop, not until ctx ends: shutdown stops it after the HTTP server.
	if err := a.Scheduler.Start(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("start the scheduler: %w", err)
	}
	return nil
}

// Stop stops the scheduler, then the job manager (running jobs are cancelled and re-queued to
// resume at the next start; it waits up to the manager's grace period or until ctx ends), then
// the notification dispatcher (queued notifications are sent until ctx ends). Close the database
// afterwards.
func (a *App) Stop(ctx context.Context) error {
	a.Scheduler.Stop()
	var errs []error
	if err := a.Jobs.Stop(ctx); err != nil {
		errs = append(errs, fmt.Errorf("stop the job manager: %w", err))
	}
	if err := a.Notifier.Close(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// intSetting reads an integer setting in [lo, hi]; a missing, unreadable or out-of-range value
// gives def (logged when it was set).
func (a *App) intSetting(ctx context.Context, key string, def, lo, hi int) int {
	if a.settings == nil {
		return def
	}
	raw, ok, err := a.settings.Get(ctx, key)
	if err != nil {
		a.log.Warn("Could not read a setting; using the default", "key", key, "default", def, "error", err)
		return def
	}
	if !ok {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < lo || n > hi {
		a.log.Warn("Setting is out of range; using the default", "key", key, "value", raw, "min", lo, "max", hi, "default", def)
		return def
	}
	return n
}

// scheduleGate is the scheduler's Enqueuer. A scheduled job whose destination or Plex
// integration is disabled would only fail (and notify) every time it fires, so it is refused
// (the scheduler logs the refusal); everything else goes to the job manager.
type scheduleGate struct{ a *App }

// Enqueue implements jobs.Enqueuer.
func (g scheduleGate) Enqueue(ctx context.Context, spec jobs.Spec) (jobs.Job, error) {
	if spec.Trigger == jobs.TriggerSchedule {
		if err := g.a.checkEnabled(ctx, spec.Params); err != nil {
			return jobs.Job{}, err
		}
	}
	return g.a.Jobs.Enqueue(ctx, spec)
}

// checkEnabled fails when p's destination or integration exists but is disabled.
func (a *App) checkEnabled(ctx context.Context, p jobs.Params) error {
	if p.DestinationID != 0 {
		d, err := a.Destinations.Get(ctx, p.DestinationID)
		if err == nil && !d.Enabled {
			return fmt.Errorf("destination %q is disabled; its scheduled job was not queued", d.Name)
		}
	}
	if p.IntegrationID != 0 {
		it, err := a.Integrations.Get(ctx, p.IntegrationID)
		if err == nil && !it.Enabled {
			return fmt.Errorf("integration %q is disabled; its scheduled job was not queued", it.Name)
		}
	}
	return nil
}

// pruneOrphanSchedules deletes schedules that refer to a destination, integration or source
// that no longer exists (left behind if the process stopped between deleting one and its
// schedules).
func (a *App) pruneOrphanSchedules(ctx context.Context) error {
	list, err := a.Jobs.Store().ListSchedules(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, sc := range list {
		gone, err := a.referencesMissing(ctx, sc.Params)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !gone {
			continue
		}
		if err := a.Jobs.Store().DeleteSchedule(ctx, sc.ID); err != nil && !errors.Is(err, jobqueue.ErrNotFound) {
			errs = append(errs, err)
			continue
		}
		a.log.Info("Removed the schedule of a deleted destination, integration or source", "scheduleId", sc.ID, "jobType", string(sc.JobType))
	}
	return errors.Join(errs...)
}

// referencesMissing reports whether p names a destination, integration or source that does not
// exist.
func (a *App) referencesMissing(ctx context.Context, p jobs.Params) (bool, error) {
	if p.DestinationID != 0 {
		if _, err := a.Destinations.Get(ctx, p.DestinationID); errors.Is(err, destinations.ErrNotFound) {
			return true, nil
		} else if err != nil {
			return false, err
		}
	}
	if p.IntegrationID != 0 {
		if _, err := a.Integrations.Get(ctx, p.IntegrationID); errors.Is(err, integrations.ErrNotFound) {
			return true, nil
		} else if err != nil {
			return false, err
		}
	}
	for _, id := range p.SourceIDs {
		if _, err := a.Catalog.Get(ctx, id); errors.Is(err, catalog.ErrNotFound) {
			return true, nil
		} else if err != nil {
			return false, err
		}
	}
	return false, nil
}

// describe names the work of a job type with params, e.g. "Sync to UNAS" (notification titles,
// schedule descriptions). Names that cannot be read fall back to "#<id>".
func (a *App) describe(ctx context.Context, t jobs.Type, p jobs.Params) string {
	dest := func() string {
		if d, err := a.Destinations.Get(ctx, p.DestinationID); err == nil {
			return d.Name
		}
		return "destination #" + strconv.FormatInt(p.DestinationID, 10)
	}
	sources := func() string {
		names := make([]string, 0, len(p.SourceIDs))
		for _, id := range p.SourceIDs {
			if s, err := a.Catalog.Get(ctx, id); err == nil {
				names = append(names, s.Name)
			} else {
				names = append(names, "source #"+strconv.FormatInt(id, 10))
			}
		}
		return strings.Join(names, ", ")
	}
	switch t {
	case jobs.TypeSync:
		s := "Sync to " + dest()
		if len(p.SourceIDs) > 0 {
			s += " (" + sources() + ")"
		}
		return s
	case jobs.TypeVerify:
		return "Verify " + dest()
	case jobs.TypeRetention:
		if p.DestinationID == 0 {
			return "Retention: expire deleted files and prune the job history"
		}
		return "Retention on " + dest()
	case jobs.TypePlexDBBackup:
		name := "Plex #" + strconv.FormatInt(p.IntegrationID, 10)
		if it, err := a.Integrations.Get(ctx, p.IntegrationID); err == nil {
			name = it.Name
		}
		s := "Plex database backup of " + name
		if p.DestinationID != 0 {
			s += " to " + dest()
		}
		return s
	case jobs.TypeScan:
		if len(p.SourceIDs) == 0 {
			return "Scan of all enabled sources"
		}
		return "Scan of " + sources()
	}
	return string(t)
}
