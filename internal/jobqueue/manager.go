package jobqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// Manager defaults (design §6.2).
const (
	// DefaultWorkers is how many jobs run at once unless Options.Workers says otherwise.
	DefaultWorkers = 2
	// DefaultRefreshWorkers is how many refresh jobs run at once, in their own pool that is not
	// counted against the workers (phase2-3.md §7.3, §12.1).
	DefaultRefreshWorkers = 2
	// DefaultMaxAttempts is how many crashed runs a job gets before recovery fails it.
	DefaultMaxAttempts = 3
	// DefaultProgressEvery is the minimum interval between progress writes.
	DefaultProgressEvery = 2 * time.Second
	// DefaultShutdownGrace is how long Stop waits for cancelled jobs.
	DefaultShutdownGrace = 20 * time.Second
)

// Fault points of the manager (internal/faultinject). A faultinject.Crash panic at either point,
// or inside a runner, is treated as a simulated process crash: the job row is left running, as a
// real crash leaves it, so the next Manager's Start recovers it.
const (
	// PointStarted is reached after a job was marked running, before its runner is called.
	PointStarted = "jobqueue.started"
	// PointBeforeFinish is reached after the runner returned, before the final state is written.
	PointBeforeFinish = "jobqueue.beforeFinish"
)

// pollEvery is how often the dispatcher looks for queued jobs without being woken (a safety net
// for jobs inserted with Store.CreateJob directly).
const pollEvery = 10 * time.Second

// queuedScan bounds how many queued jobs one dispatch pass considers.
const queuedScan = 1000

// Cancellation causes of a running job's context.
var (
	errUserCancel = errors.New("cancelled by user")
	errShutdown   = errors.New("interrupted by shutdown")
)

// Options configures a Manager. Zero values take the defaults.
type Options struct {
	// Workers is how many jobs run at once (setting jobs.workers; default 2), refresh jobs not
	// counted.
	Workers int
	// RefreshWorkers is how many refresh jobs run at once, in their own pool (default 2), so a
	// long sync and a verify cannot starve a webhook's targeted refresh.
	RefreshWorkers int
	// MaxAttempts is how many runs a job gets when the process keeps crashing during it: start-up
	// recovery fails a job whose attempt would exceed it (default 3).
	MaxAttempts int
	// ProgressEvery is the minimum interval between progress writes (default 2 s).
	ProgressEvery time.Duration
	// ShutdownGrace is how long Stop waits for cancelled jobs (default 20 s).
	ShutdownGrace time.Duration
	// Now is the clock for stored times, progress throttling and throughput (default time.Now).
	Now func() time.Time
}

// Manager state.
const (
	stateNew = iota
	stateStarted
	stateStopped
)

// Manager runs queued jobs (design §6.2). Create it with New, Register the runners and OnFinish
// hooks, then Start it; Stop it on shutdown. Safe for concurrent use.
type Manager struct {
	store *Store
	log   *slog.Logger
	opts  Options
	now   func() time.Time
	kick  chan struct{}

	mu         sync.Mutex
	state      int
	crashed    bool // a simulated crash (faultinject.Crash) happened: dispatch nothing more
	workers    int
	runners    map[jobs.Type]jobs.Runner
	hooks      []func(jobs.Job)
	running    map[int64]*activeJob
	keys       map[string]int64
	schedulers []*Scheduler
	base       context.Context
	stopLoop   context.CancelFunc
	loopDone   chan struct{}

	jobsRunning *inflight
	hooksActive *inflight
}

// activeJob is a job this Manager has taken from the queue.
type activeJob struct {
	id   int64
	keys []string
	// refresh is true for a job of the refresh pool.
	refresh bool
	cancel  context.CancelCauseFunc
	rep     *reporter

	// finMu orders the job's own final write against Stop giving up on it.
	finMu     sync.Mutex
	finished  bool
	abandoned bool
}

var _ jobs.Enqueuer = (*Manager)(nil)

// New returns a Manager over d. log may be nil.
func New(d *db.DB, log *slog.Logger, o Options) *Manager {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if o.Workers < 1 {
		o.Workers = DefaultWorkers
	}
	if o.RefreshWorkers < 1 {
		o.RefreshWorkers = DefaultRefreshWorkers
	}
	if o.MaxAttempts < 1 {
		o.MaxAttempts = DefaultMaxAttempts
	}
	if o.ProgressEvery <= 0 {
		o.ProgressEvery = DefaultProgressEvery
	}
	if o.ShutdownGrace <= 0 {
		o.ShutdownGrace = DefaultShutdownGrace
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	st := NewStore(d)
	st.now = o.Now
	return &Manager{
		store:   st,
		log:     log,
		opts:    o,
		now:     o.Now,
		kick:    make(chan struct{}, 1),
		workers: o.Workers,
		runners: map[jobs.Type]jobs.Runner{},
		running: map[int64]*activeJob{},
		keys:    map[string]int64{},

		jobsRunning: newInflight(),
		hooksActive: newInflight(),
	}
}

// Store returns the manager's store (items, logs, schedules, history).
func (m *Manager) Store() *Store { return m.store }

// Register sets the runner of a job type. Register every runner before Start: a queued job whose
// type has no runner when a worker takes it fails.
func (m *Manager) Register(t jobs.Type, r jobs.Runner) {
	m.mu.Lock()
	m.runners[t] = r
	m.mu.Unlock()
}

// OnFinish adds a hook called with the job after its final state (completed,
// completed_with_warnings, failed, cancelled) is committed: once per job, in a goroutine, with
// panics recovered. Jobs re-queued by a graceful shutdown are not final.
func (m *Manager) OnFinish(fn func(jobs.Job)) {
	m.mu.Lock()
	m.hooks = append(m.hooks, fn)
	m.mu.Unlock()
}

// SetWorkers changes how many jobs run at once (at least 1; refresh jobs have their own pool).
// Running jobs are not interrupted when the limit shrinks.
func (m *Manager) SetWorkers(n int) {
	m.mu.Lock()
	m.workers = max(n, 1)
	m.mu.Unlock()
	m.wake()
}

// attachScheduler makes Stop stop s (NewScheduler calls it when the enqueuer is this Manager).
func (m *Manager) attachScheduler(s *Scheduler) {
	m.mu.Lock()
	m.schedulers = append(m.schedulers, s)
	m.mu.Unlock()
}

// ctx is the context of the manager's own database writes: never cancelled, so a final state
// is written even while the job's context is cancelled.
func (m *Manager) ctx() context.Context {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.base == nil {
		return context.Background()
	}
	return m.base
}

// Start recovers jobs a previous process left running (crashed: back to the queue with trigger
// resume and attempt+1, or failed with "crashed N times" when attempt would exceed MaxAttempts)
// and starts dispatching queued jobs. ctx bounds the recovery; the manager runs until Stop.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.state != stateNew {
		m.mu.Unlock()
		return errors.New("job manager: already started or stopped")
	}
	m.mu.Unlock()

	requeued, failed, err := m.store.recoverCrashed(ctx, m.opts.MaxAttempts, m.now())
	if err != nil {
		return err
	}
	for _, id := range requeued {
		m.log.Warn("Job was interrupted by a crash; it resumes", "jobId", id)
	}
	for _, id := range failed {
		job, err := m.store.GetJob(ctx, id)
		if err != nil {
			m.log.Error("Could not read a job failed by crash recovery", "jobId", id, "error", err)
			continue
		}
		m.log.Error("Job failed: the process crashed during it too often", "jobId", id, "error", job.Error)
		m.fireHooks(job)
	}

	base := context.WithoutCancel(ctx)
	loopCtx, stopLoop := context.WithCancel(base)
	m.mu.Lock()
	m.state = stateStarted
	m.base = base
	m.stopLoop = stopLoop
	m.loopDone = make(chan struct{})
	m.mu.Unlock()
	go m.loop(loopCtx)
	return nil
}

// Stop shuts down gracefully: it stops attached schedulers and the dispatcher, cancels running
// jobs, and waits up to ShutdownGrace (or until ctx ends) for them. A job interrupted this way
// goes back to the queue with trigger resume and its attempt unchanged; one that ignores the
// cancellation past the grace period is re-queued the same way and its late result discarded.
// Stop also waits, within the same deadline, for OnFinish hooks in flight.
func (m *Manager) Stop(ctx context.Context) error {
	m.mu.Lock()
	if m.state != stateStarted {
		m.state = stateStopped
		m.mu.Unlock()
		return nil
	}
	m.state = stateStopped
	scheds := slices.Clone(m.schedulers)
	stopLoop, loopDone := m.stopLoop, m.loopDone
	m.mu.Unlock()

	// One deadline for everything below (a context, not a timer: both waits must see it).
	graceCtx, cancelGrace := context.WithTimeout(ctx, m.opts.ShutdownGrace)
	defer cancelGrace()

	for _, s := range scheds {
		s.Stop()
	}
	stopLoop()
	<-loopDone

	m.mu.Lock()
	active := make([]*activeJob, 0, len(m.running))
	for _, aj := range m.running {
		active = append(active, aj)
	}
	m.mu.Unlock()
	for _, aj := range active {
		aj.cancel(errShutdown)
	}

	if !wait(graceCtx, m.jobsRunning) {
		m.mu.Lock()
		stragglers := make([]*activeJob, 0, len(m.running))
		for _, aj := range m.running {
			stragglers = append(stragglers, aj)
		}
		m.mu.Unlock()
		for _, aj := range stragglers {
			m.abandon(aj)
		}
	}
	wait(graceCtx, m.hooksActive)
	return nil
}

// abandon re-queues a job whose runner did not return within the grace period and makes its
// eventual result a no-op.
func (m *Manager) abandon(aj *activeJob) {
	aj.finMu.Lock()
	defer aj.finMu.Unlock()
	if aj.finished {
		return
	}
	aj.abandoned = true
	aj.rep.close()
	m.log.Warn("Job did not stop within the shutdown grace period; it resumes at the next start", "jobId", aj.id)
	if _, err := m.store.requeue(m.ctx(), aj.id, aj.rep.encode()); err != nil {
		m.log.Error("Could not re-queue an interrupted job; the next start treats it as crashed", "jobId", aj.id, "error", err)
	}
	m.release(aj)
}

// inflight counts goroutines of one kind. Unlike sync.WaitGroup, add may race with a waiter
// (a late OnFinish hook during Stop).
type inflight struct {
	mu   sync.Mutex
	n    int
	zero chan struct{} // closed while n == 0
}

func newInflight() *inflight {
	z := make(chan struct{})
	close(z)
	return &inflight{zero: z}
}

func (f *inflight) add() {
	f.mu.Lock()
	if f.n == 0 {
		f.zero = make(chan struct{})
	}
	f.n++
	f.mu.Unlock()
}

func (f *inflight) done() {
	f.mu.Lock()
	f.n--
	if f.n == 0 {
		close(f.zero)
	}
	f.mu.Unlock()
}

func (f *inflight) idle() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.zero
}

// wait waits until f is idle or ctx ends; it reports whether f became idle.
func wait(ctx context.Context, f *inflight) bool {
	select {
	case <-f.idle():
		return true
	case <-ctx.Done():
	}
	select {
	case <-f.idle():
		return true
	default:
		return false
	}
}

// Enqueue implements jobs.Enqueuer: it queues a job, returns the identical queued job (same
// type, canonical params and dry run), or coalesces a targeted spec with a queued job (see
// Store.CreateJob). An empty trigger means manual. An invalid spec is a ValidationError.
func (m *Manager) Enqueue(ctx context.Context, spec jobs.Spec) (jobs.Job, error) {
	job, outcome, err := m.store.createJob(ctx, spec)
	if err != nil {
		return jobs.Job{}, err
	}
	switch outcome {
	case outcomeCreated:
		m.log.Info("Job queued", "jobId", job.ID, "jobType", string(job.Type), "trigger", string(job.Trigger), "dryRun", job.DryRun)
	case outcomeMerged, outcomeCovered:
		m.log.Info("Job request coalesced with a queued job", "jobId", job.ID, "jobType", string(job.Type), "outcome", outcome.String(),
			"paths", len(job.Params.Paths), "arrItemIds", len(job.Params.ArrItemIDs))
	case outcomeOverflow:
		m.log.Info("Too many targets were merged into a queued job: it now covers its whole scope", "jobId", job.ID,
			"jobType", string(job.Type))
	}
	m.wake()
	return job, nil
}

// Cancel cancels a job: a queued job becomes cancelled at once; a running job's context is
// cancelled and the job becomes cancelled when its runner returns. It returns the job as it is
// now. A finished job (or one not running in this process) is ErrNotActive, an unknown one
// ErrNotFound.
func (m *Manager) Cancel(ctx context.Context, id int64) (jobs.Job, error) {
	if m.cancelRunning(id) {
		return m.Get(ctx, id)
	}
	ok, err := m.store.cancelQueued(ctx, id, m.now())
	if err != nil {
		return jobs.Job{}, err
	}
	if ok {
		job, err := m.store.GetJob(ctx, id)
		if err != nil {
			return jobs.Job{}, err
		}
		m.log.Info("Job cancelled before it started", "jobId", id, "jobType", string(job.Type))
		m.fireHooks(job)
		return job, nil
	}
	// Not queued any more: a worker may have just taken it.
	if m.cancelRunning(id) {
		return m.Get(ctx, id)
	}
	job, err := m.store.GetJob(ctx, id)
	if err != nil {
		return jobs.Job{}, err
	}
	return job, fmt.Errorf("job %d is %s: %w", id, job.Status, ErrNotActive)
}

// cancelRunning cancels job id if this manager runs it.
func (m *Manager) cancelRunning(id int64) bool {
	m.mu.Lock()
	aj := m.running[id]
	m.mu.Unlock()
	if aj == nil {
		return false
	}
	aj.cancel(errUserCancel)
	m.log.Info("Job cancellation requested", "jobId", id)
	return true
}

// Get returns job id, with the live progress of a running job.
func (m *Manager) Get(ctx context.Context, id int64) (jobs.Job, error) {
	j, err := m.store.GetJob(ctx, id)
	if err != nil {
		return jobs.Job{}, err
	}
	m.merge(&j)
	return j, nil
}

// List returns one page of jobs (see Store.ListJobs), with the live progress of running jobs.
func (m *Manager) List(ctx context.Context, q JobQuery) (Page[jobs.Job], error) {
	p, err := m.store.ListJobs(ctx, q)
	if err != nil {
		return p, err
	}
	for i := range p.Records {
		m.merge(&p.Records[i])
	}
	return p, nil
}

func (m *Manager) merge(j *jobs.Job) {
	if j.Status != jobs.StatusRunning {
		return
	}
	m.mu.Lock()
	aj := m.running[j.ID]
	m.mu.Unlock()
	if aj != nil {
		j.Progress = aj.rep.snapshot()
	}
}

// wake asks the dispatcher for a pass.
func (m *Manager) wake() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

// loop is the dispatcher: it starts queued jobs whenever something may have changed.
func (m *Manager) loop(ctx context.Context) {
	defer close(m.loopDone)
	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	for {
		m.dispatch(ctx)
		select {
		case <-ctx.Done():
			return
		case <-m.kick:
		case <-tick.C:
		}
	}
}

// refreshPool reports whether jobs of type t run in the refresh pool.
func refreshPool(t jobs.Type) bool { return t == jobs.TypeRefresh }

// poolsFullLocked reports whether the general pool and the refresh pool are both full, and
// whether the pool of type t is. Caller holds m.mu.
func (m *Manager) poolsFullLocked(t jobs.Type) (all, own bool) {
	refresh := 0
	for _, aj := range m.running {
		if aj.refresh {
			refresh++
		}
	}
	general := len(m.running) - refresh
	generalFull, refreshFull := general >= m.workers, refresh >= m.opts.RefreshWorkers
	if refreshPool(t) {
		return generalFull && refreshFull, refreshFull
	}
	return generalFull && refreshFull, generalFull
}

// dispatch starts the oldest queued jobs whose lock keys are free, while their pool has a free
// worker (refresh jobs have their own pool). A job whose keys are held or whose pool is full is
// skipped, so it does not block younger jobs with free keys in another pool.
func (m *Manager) dispatch(ctx context.Context) {
	m.mu.Lock()
	full, _ := m.poolsFullLocked("")
	full = full || m.crashed
	m.mu.Unlock()
	if full {
		return
	}
	queued, err := m.store.queuedJobs(ctx, queuedScan)
	if err != nil {
		if ctx.Err() == nil {
			m.log.Error("Could not read the job queue", "error", err)
		}
		return
	}
	for _, job := range queued {
		if ctx.Err() != nil {
			return
		}
		m.mu.Lock()
		all, own := m.poolsFullLocked(job.Type)
		if all || m.crashed || m.state != stateStarted {
			m.mu.Unlock()
			return
		}
		if _, dup := m.running[job.ID]; dup || own {
			m.mu.Unlock()
			continue
		}
		runner := m.runners[job.Type]
		if runner == nil {
			m.mu.Unlock()
			m.failUnrunnable(ctx, job)
			continue
		}
		keys := LockKeys(job.Type, job.Params)
		if m.keysHeldLocked(keys) {
			m.mu.Unlock()
			continue
		}
		jctx, cancel := context.WithCancelCause(m.base)
		aj := &activeJob{id: job.ID, keys: keys, refresh: refreshPool(job.Type), cancel: cancel, rep: newReporter(m, job)}
		m.running[job.ID] = aj
		for _, k := range keys {
			m.keys[k] = job.ID
		}
		m.mu.Unlock()

		started, ok, err := m.store.markRunning(ctx, job.ID, m.now())
		if err != nil {
			// The write failed (read-only or failing disk): end this pass without waking the
			// dispatcher, so the next poll retries instead of a busy loop.
			m.log.Error("Could not start job", "jobId", job.ID, "error", err)
			m.forget(aj)
			return
		}
		if !ok {
			m.release(aj)
			continue
		}
		m.jobsRunning.add()
		go m.execute(jctx, aj, runner, started)
	}
}

func (m *Manager) keysHeldLocked(keys []string) bool {
	for _, k := range keys {
		if _, held := m.keys[k]; held {
			return true
		}
	}
	return false
}

// release forgets an active job (see forget), then wakes the dispatcher.
func (m *Manager) release(aj *activeJob) {
	m.forget(aj)
	m.wake()
}

// forget forgets an active job, cancels its context (stray goroutines of a finished runner stop;
// a cause set earlier is kept) and frees its keys.
func (m *Manager) forget(aj *activeJob) {
	aj.cancel(nil)
	m.mu.Lock()
	if m.running[aj.id] == aj {
		delete(m.running, aj.id)
	}
	for _, k := range aj.keys {
		if m.keys[k] == aj.id {
			delete(m.keys, k)
		}
	}
	m.mu.Unlock()
}

// failUnrunnable fails a queued job whose type has no registered runner.
func (m *Manager) failUnrunnable(ctx context.Context, job jobs.Job) {
	msg := fmt.Sprintf("no runner is registered for job type %q", job.Type)
	ok, err := m.store.finishQueued(ctx, job.ID, jobs.StatusFailed, msg, "", m.now())
	if err != nil {
		m.log.Error("Could not fail a job without a runner", "jobId", job.ID, "error", err)
		return
	}
	if !ok {
		return
	}
	m.log.Error("Job failed", "jobId", job.ID, "jobType", string(job.Type), "error", msg)
	if j, err := m.store.GetJob(ctx, job.ID); err == nil {
		m.fireHooks(j)
	}
}

// outcome is what a runner invocation produced.
type outcome struct {
	res      jobs.Result
	err      error
	panicked bool
	panicVal any
	stack    []byte
}

// execute runs one job and records its result.
func (m *Manager) execute(ctx context.Context, aj *activeJob, runner jobs.Runner, job jobs.Job) {
	defer m.jobsRunning.done()
	aj.rep.Log(slog.LevelInfo, "Job started", "attempt", job.Attempt, "trigger", string(job.Trigger), "dryRun", job.DryRun)
	out := m.invoke(ctx, runner, job, aj.rep)
	if out.panicked {
		if c, ok := out.panicVal.(faultinject.Crash); ok {
			m.simulateCrash(aj, c)
			return
		}
	}
	m.finish(ctx, aj, job, out)
}

// invoke calls the runner with panics recovered.
func (m *Manager) invoke(ctx context.Context, runner jobs.Runner, job jobs.Job, rep *reporter) (out outcome) {
	defer func() {
		if p := recover(); p != nil {
			out = outcome{panicked: true, panicVal: p, stack: debug.Stack()}
		}
	}()
	faultinject.Point(PointStarted)
	if ctx.Err() != nil {
		// Cancelled between being taken from the queue and starting.
		return outcome{err: ctx.Err()}
	}
	res, err := runner.Run(ctx, job, jobs.Env{Reporter: rep, Items: m.store})
	faultinject.Point(PointBeforeFinish)
	return outcome{res: res, err: err}
}

// simulateCrash handles a faultinject.Crash: the job row stays running (as after a real crash)
// and this manager starts nothing more; a new Manager's Start recovers the job.
func (m *Manager) simulateCrash(aj *activeJob, c faultinject.Crash) {
	aj.finMu.Lock()
	aj.finished = true
	aj.finMu.Unlock()
	aj.rep.close()
	aj.cancel(nil)
	m.log.Warn("Simulated crash (fault injection): job left running", "jobId", aj.id, "point", c.Point)
	m.mu.Lock()
	m.crashed = true
	if m.running[aj.id] == aj {
		delete(m.running, aj.id)
	}
	m.mu.Unlock()
}

// finish maps a runner outcome to the job's final state (or a re-queue after a shutdown),
// commits it, and fires the OnFinish hooks.
func (m *Manager) finish(ctx context.Context, aj *activeJob, job jobs.Job, out outcome) {
	cause := context.Cause(ctx)
	var (
		status  jobs.Status
		errText string
	)
	switch {
	case out.panicked:
		msg := redactText(fmt.Sprint(out.panicVal))
		m.log.Error("Job runner panicked", "jobId", job.ID, "jobType", string(job.Type), "panic", msg, "stack", redactText(string(out.stack)))
		aj.rep.Log(slog.LevelError, "Runner panicked", "panic", msg)
		status, errText = jobs.StatusFailed, "runner panicked: "+msg
	case out.err == nil:
		status = jobs.StatusCompleted
		if out.res.Warnings > 0 {
			status = jobs.StatusCompletedWithWarnings
		}
	case ctx.Err() != nil && errors.Is(cause, errShutdown):
		m.requeueInterrupted(aj)
		return
	case ctx.Err() != nil && errors.Is(cause, errUserCancel):
		status = jobs.StatusCancelled
	default:
		status, errText = jobs.StatusFailed, redactText(out.err.Error())
	}

	stats := m.statsJSON(aj, out.res.Stats)
	summary := redactText(out.res.Summary)
	if status == jobs.StatusCancelled && summary == "" {
		summary = "Cancelled."
	}
	if errText != "" {
		aj.rep.Log(slog.LevelError, "Job finished", "status", string(status), "error", errText)
	} else {
		aj.rep.Log(slog.LevelInfo, "Job finished", "status", string(status), "warnings", out.res.Warnings)
	}

	aj.finMu.Lock()
	if aj.abandoned {
		aj.finMu.Unlock()
		return
	}
	aj.rep.close()
	ok, err := m.store.finishJob(m.ctx(), job.ID, finalState{
		status:   status,
		stats:    stats,
		warnings: max(out.res.Warnings, 0),
		summary:  summary,
		errText:  errText,
		progress: aj.rep.encode(),
		at:       m.now(),
	})
	aj.finished = true
	aj.finMu.Unlock()
	m.release(aj)
	if err != nil {
		m.log.Error("Could not record a job's result; the next start treats it as crashed", "jobId", job.ID, "status", string(status), "error", err)
		return
	}
	if !ok {
		return
	}
	final, err := m.store.GetJob(m.ctx(), job.ID)
	if err != nil {
		m.log.Error("Could not read a finished job", "jobId", job.ID, "error", err)
		return
	}
	m.fireHooks(final)
}

// requeueInterrupted puts a job whose runner returned because of a graceful shutdown back in the
// queue (trigger resume, attempt unchanged).
func (m *Manager) requeueInterrupted(aj *activeJob) {
	aj.finMu.Lock()
	if aj.abandoned {
		aj.finMu.Unlock()
		return
	}
	aj.rep.Log(slog.LevelInfo, "Interrupted by shutdown; the job resumes at the next start")
	aj.rep.close()
	if _, err := m.store.requeue(m.ctx(), aj.id, aj.rep.encode()); err != nil {
		m.log.Error("Could not re-queue an interrupted job; the next start treats it as crashed", "jobId", aj.id, "error", err)
	}
	aj.finished = true
	aj.finMu.Unlock()
	m.release(aj)
}

// statsJSON renders a runner's Stats; anything that is not a JSON object is replaced by {} and
// logged.
func (m *Manager) statsJSON(aj *activeJob, stats any) string {
	if stats == nil {
		return "{}"
	}
	b, err := json.Marshal(stats)
	if err == nil && len(b) > 0 && b[0] == '{' {
		s := redactText(string(b))
		if json.Valid([]byte(s)) {
			return s
		}
	}
	aj.rep.Log(slog.LevelWarn, "Job stats are not a JSON object; they were not stored")
	return "{}"
}

// fireHooks calls the OnFinish hooks with job in a goroutine, recovering panics.
func (m *Manager) fireHooks(job jobs.Job) {
	m.mu.Lock()
	hooks := slices.Clone(m.hooks)
	m.mu.Unlock()
	if len(hooks) == 0 {
		return
	}
	m.hooksActive.add()
	go func() {
		defer m.hooksActive.done()
		for _, h := range hooks {
			func() {
				defer func() {
					if p := recover(); p != nil {
						m.log.Error("OnFinish hook panicked", "jobId", job.ID, "panic", redactText(fmt.Sprint(p)), "stack", redactText(string(debug.Stack())))
					}
				}()
				h(job)
			}()
		}
	}()
}
