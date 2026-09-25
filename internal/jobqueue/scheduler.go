package jobqueue

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// maxSleep caps how long the scheduler sleeps before re-checking the wall clock, so a clock
// change (NTP, suspend/resume) delays a run by at most this much.
const maxSleep = time.Minute

// reloadTimeout bounds Reload's database read, which does not end with the caller's context.
const reloadTimeout = 30 * time.Second

// cronParser accepts the standard 5 fields and descriptors; parseCron narrows the descriptors
// to the supported ones. A Parser is an immutable value.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// ValidateCron checks a schedule expression: the standard 5 fields (minute hour day-of-month
// month day-of-week, robfig/cron syntax) or one of @hourly, @daily, @weekly, @monthly. Seconds,
// @every, time-zone prefixes and expressions that never fire are rejected with a
// ValidationError.
func ValidateCron(expr string) error {
	_, err := parseCron(expr)
	return err
}

func parseCron(expr string) (sched cron.Schedule, err error) {
	e := strings.TrimSpace(expr)
	switch {
	case e == "":
		return nil, ValidationError("cron expression is empty")
	case strings.HasPrefix(e, "TZ=") || strings.HasPrefix(e, "CRON_TZ="):
		return nil, ValidationError("cron time-zone prefixes are not supported: schedules run in the server's time zone (TZ)")
	case strings.HasPrefix(e, "@"):
		switch e {
		case "@hourly", "@daily", "@weekly", "@monthly":
		default:
			return nil, ValidationError(fmt.Sprintf("unsupported cron descriptor %q (use @hourly, @daily, @weekly, @monthly or 5 fields)", e))
		}
	default:
		if n := len(strings.Fields(e)); n != 5 {
			return nil, ValidationError(fmt.Sprintf("cron expression %q has %d fields; use 5 (minute hour day-of-month month day-of-week)", e, n))
		}
	}
	defer func() {
		// robfig/cron has panicked on malformed input in the past; an expression must never take
		// the process down.
		if p := recover(); p != nil {
			sched, err = nil, ValidationError(fmt.Sprintf("invalid cron expression %q", e))
		}
	}()
	sched, err = cronParser.Parse(e)
	if err != nil {
		return nil, ValidationError(fmt.Sprintf("invalid cron expression %q: %v", e, err))
	}
	if sched.Next(time.Now()).IsZero() {
		return nil, ValidationError(fmt.Sprintf("cron expression %q never fires", e))
	}
	return sched, nil
}

// clock is the scheduler's time source; tests substitute a fake one.
type clock interface {
	Now() time.Time
	// NewTimer returns a channel that receives after d and a function that stops the timer.
	NewTimer(d time.Duration) (<-chan time.Time, func() bool)
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) NewTimer(d time.Duration) (<-chan time.Time, func() bool) {
	t := time.NewTimer(d)
	return t.C, t.Stop
}

// entry is one enabled schedule and its next run.
type entry struct {
	sched Schedule
	spec  cron.Schedule
	next  time.Time
}

// Scheduler enqueues jobs from the schedules table when their cron expressions fire (design
// §6.2). A run missed while Bunkarr was down or asleep is not caught up: a schedule fires at most
// once when it wakes late and then continues from the current time. Safe for concurrent use.
type Scheduler struct {
	store *Store
	enq   jobs.Enqueuer
	log   *slog.Logger
	loc   *time.Location
	clk   clock
	// listSchedules reads the schedules (store.ListSchedules; tests wrap it).
	listSchedules func(ctx context.Context) ([]Schedule, error)

	reload chan struct{}

	// reloadMu serializes Reload from its read to its apply, so an older list never replaces a
	// newer one.
	reloadMu sync.Mutex

	mu        sync.Mutex
	entries   map[int64]*entry
	lastFired map[int64]time.Time // schedule id -> the last scheduled time it fired for
	cancel    context.CancelFunc
	done      chan struct{}
}

// NewScheduler returns a scheduler that reads schedules from store and enqueues on enq, with
// cron expressions evaluated in loc (nil means the local time zone). When enq is a *Manager, the
// Manager's Stop also stops this scheduler.
func NewScheduler(store *Store, enq jobs.Enqueuer, log *slog.Logger, loc *time.Location) *Scheduler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if loc == nil {
		loc = time.Local
	}
	s := &Scheduler{
		store:     store,
		enq:       enq,
		log:       log,
		loc:       loc,
		clk:       realClock{},
		reload:    make(chan struct{}, 1),
		entries:   map[int64]*entry{},
		lastFired: map[int64]time.Time{},
	}
	s.listSchedules = store.ListSchedules
	if m, ok := enq.(*Manager); ok {
		m.attachScheduler(s)
	}
	return s
}

// Start loads the schedules and starts the scheduler loop, which runs until Stop or until ctx
// ends.
func (s *Scheduler) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.cancel != nil {
		s.mu.Unlock()
		return fmt.Errorf("scheduler: already started")
	}
	loopCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.done = make(chan struct{})
	s.mu.Unlock()
	if err := s.Reload(ctx); err != nil {
		cancel()
		close(s.done)
		return err
	}
	go s.loop(loopCtx)
	return nil
}

// Stop stops the loop and waits for it (a fire in progress completes). Idempotent.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

// Reload re-reads the schedules; call it after any schedule change. A schedule whose cron
// expression is unchanged keeps its pending next run; a new or changed one starts from now, but
// never from before the last time it fired, so a reload never fires a schedule twice for the
// same minute. The read does not end with ctx (an API request whose client went away after
// storing a change must still load it); it has its own timeout.
func (s *Scheduler) Reload(ctx context.Context) error {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reloadTimeout)
	defer cancel()
	list, err := s.listSchedules(rctx)
	if err != nil {
		return err
	}
	now := s.clk.Now()
	s.mu.Lock()
	entries := make(map[int64]*entry, len(list))
	for _, sc := range list {
		if !sc.Enabled {
			continue
		}
		if old := s.entries[sc.ID]; old != nil && old.sched.Cron == sc.Cron {
			entries[sc.ID] = &entry{sched: sc, spec: old.spec, next: old.next}
			continue
		}
		spec, err := parseCron(sc.Cron)
		if err != nil {
			s.log.Error("Schedule has an invalid cron expression; it will not run", "scheduleId", sc.ID, "jobType", string(sc.JobType), "cron", sc.Cron, "error", err)
			continue
		}
		base := now
		if lf, ok := s.lastFired[sc.ID]; ok && lf.After(base) {
			base = lf
		}
		if sc.LastRunAt != nil && sc.LastRunAt.After(base) {
			base = *sc.LastRunAt
		}
		entries[sc.ID] = &entry{sched: sc, spec: spec, next: spec.Next(base.In(s.loc))}
	}
	s.entries = entries
	s.mu.Unlock()
	select {
	case s.reload <- struct{}{}:
	default:
	}
	return nil
}

// NextRun returns when schedule id fires next (zero when it is disabled, invalid or unknown).
func (s *Scheduler) NextRun(id int64) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[id]; e != nil {
		return e.next
	}
	return time.Time{}
}

// RunNow enqueues schedule id's job now with trigger manual (disabled schedules too) and records
// the run. dryRun (optional, one value) true queues a dry run instead, e.g. a retention preview:
// a separate job from a real run (the enqueue dedupe compares the dry-run flag) that is not
// recorded as a run of the schedule.
func (s *Scheduler) RunNow(ctx context.Context, id int64, dryRun ...bool) (jobs.Job, error) {
	dry := len(dryRun) > 0 && dryRun[0]
	sc, err := s.store.GetSchedule(ctx, id)
	if err != nil {
		return jobs.Job{}, err
	}
	job, err := s.enq.Enqueue(ctx, jobs.Spec{Type: sc.JobType, Trigger: jobs.TriggerManual, DryRun: dry, Params: sc.Params})
	if err != nil {
		return jobs.Job{}, fmt.Errorf("run schedule %d: %w", id, err)
	}
	if dry {
		return job, nil
	}
	if err := s.store.MarkRun(ctx, id, s.clk.Now()); err != nil {
		s.log.Warn("Could not record a schedule run", "scheduleId", id, "error", err)
	}
	return job, nil
}

func (s *Scheduler) loop(ctx context.Context) {
	defer close(s.done)
	for {
		var (
			timer <-chan time.Time
			stop  func() bool
		)
		if next := s.earliest(); !next.IsZero() {
			timer, stop = s.clk.NewTimer(min(next.Sub(s.clk.Now()), maxSleep))
		}
		select {
		case <-ctx.Done():
			if stop != nil {
				stop()
			}
			return
		case <-s.reload:
			if stop != nil {
				stop()
			}
		case <-timer:
			s.fireDue(ctx)
		}
	}
}

// earliest returns the soonest next run (zero when nothing is scheduled).
func (s *Scheduler) earliest() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	var first time.Time
	for _, e := range s.entries {
		if !e.next.IsZero() && (first.IsZero() || e.next.Before(first)) {
			first = e.next
		}
	}
	return first
}

// due is one schedule firing for its scheduled time at.
type due struct {
	sched Schedule
	at    time.Time
}

// fireDue fires every schedule whose next run has come, each once, then advances it from now.
func (s *Scheduler) fireDue(ctx context.Context) {
	now := s.clk.Now()
	var fire []due
	s.mu.Lock()
	for id, e := range s.entries {
		if e.next.IsZero() || e.next.After(now) {
			continue
		}
		fire = append(fire, due{sched: e.sched, at: e.next})
		s.lastFired[id] = e.next
		base := now
		if e.next.After(base) {
			base = e.next
		}
		e.next = e.spec.Next(base.In(s.loc))
	}
	s.mu.Unlock()
	sort.Slice(fire, func(i, j int) bool {
		if !fire[i].at.Equal(fire[j].at) {
			return fire[i].at.Before(fire[j].at)
		}
		return fire[i].sched.ID < fire[j].sched.ID
	})
	// A due schedule is consumed above, so its fire completes even when Stop cancels ctx
	// meanwhile (otherwise the run would be lost, or enqueued but not recorded).
	fctx := context.WithoutCancel(ctx)
	for _, d := range fire {
		s.fire(fctx, d)
	}
}

// fire enqueues one scheduled job, unless the same work is already running.
func (s *Scheduler) fire(ctx context.Context, d due) {
	sc := d.sched
	running, err := s.store.hasRunning(ctx, sc.JobType, sc.Params)
	if err != nil {
		s.log.Error("Scheduled job not queued", "scheduleId", sc.ID, "jobType", string(sc.JobType), "error", err)
		return
	}
	if running {
		s.log.Info("Scheduled job skipped: the same job is still running", "scheduleId", sc.ID, "jobType", string(sc.JobType), "scheduledFor", d.at)
		return
	}
	job, err := s.enq.Enqueue(ctx, jobs.Spec{Type: sc.JobType, Trigger: jobs.TriggerSchedule, Params: sc.Params})
	if err != nil {
		s.log.Error("Scheduled job not queued", "scheduleId", sc.ID, "jobType", string(sc.JobType), "error", err)
		return
	}
	if err := s.store.MarkRun(ctx, sc.ID, s.clk.Now()); err != nil {
		s.log.Warn("Could not record a schedule run", "scheduleId", sc.ID, "error", err)
	}
	s.log.Info("Scheduled job queued", "scheduleId", sc.ID, "jobType", string(sc.JobType), "jobId", job.ID, "scheduledFor", d.at)
}
