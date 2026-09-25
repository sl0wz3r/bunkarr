package jobqueue

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/logging"
)

var (
	syncDest1  = jobs.Spec{Type: jobs.TypeSync, Params: jobs.Params{DestinationID: 1}}
	syncDest1b = jobs.Spec{Type: jobs.TypeSync, Params: jobs.Params{DestinationID: 1, AllowChanges: true}}
	syncDest2  = jobs.Spec{Type: jobs.TypeSync, Params: jobs.Params{DestinationID: 2}}
)

// gatedRunner reports each job it starts and blocks until released (or, with honorCtx, until
// its context is cancelled, returning ctx.Err()).
type gatedRunner struct {
	started  chan jobs.Job
	release  chan struct{}
	honorCtx bool

	mu      sync.Mutex
	active  map[string]int // lock key -> jobs running under it
	overlap []string
	running int
	maxRun  int
}

func newGatedRunner(honorCtx bool) *gatedRunner {
	return &gatedRunner{started: make(chan jobs.Job, 100), release: make(chan struct{}), honorCtx: honorCtx, active: map[string]int{}}
}

func (g *gatedRunner) Run(ctx context.Context, job jobs.Job, env jobs.Env) (jobs.Result, error) {
	keys := LockKeys(job.Type, job.Params)
	g.mu.Lock()
	for _, k := range keys {
		g.active[k]++
		if g.active[k] > 1 {
			g.overlap = append(g.overlap, k)
		}
	}
	g.running++
	g.maxRun = max(g.maxRun, g.running)
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		for _, k := range keys {
			g.active[k]--
		}
		g.running--
		g.mu.Unlock()
	}()
	g.started <- job
	if g.honorCtx {
		select {
		case <-g.release:
		case <-ctx.Done():
			return jobs.Result{}, fmt.Errorf("copying: %w", ctx.Err())
		}
	} else {
		<-g.release
	}
	return jobs.Result{Stats: map[string]int{"filesCopied": 1}, Summary: "done"}, nil
}

func registerAll(m *Manager, r jobs.Runner) {
	for _, t := range []jobs.Type{jobs.TypeScan, jobs.TypeSync, jobs.TypePlexDBBackup, jobs.TypeRetention, jobs.TypeVerify} {
		m.Register(t, r)
	}
}

func TestRunCompletesWithStatsAndWarnings(t *testing.T) {
	d := openDB(t)
	m := newManager(t, d, Options{})
	hooks := newHookRecorder(m)
	m.Register(jobs.TypeSync, jobs.RunnerFunc(func(ctx context.Context, job jobs.Job, env jobs.Env) (jobs.Result, error) {
		if job.Status != jobs.StatusRunning || job.StartedAt == nil || job.Attempt != 1 {
			return jobs.Result{}, fmt.Errorf("runner got job %+v", job)
		}
		w := 0
		if job.Params.DestinationID == 2 {
			w = 3
		}
		return jobs.Result{Stats: struct {
			FilesCopied int `json:"filesCopied"`
		}{7}, Warnings: w, Summary: "Copied 7 files."}, nil
	}))
	start(t, m)
	a := enqueue(t, m, syncDest1)
	b := enqueue(t, m, syncDest2)

	ja := waitStatus(t, m.Store(), a.ID, jobs.StatusCompleted)
	if string(ja.Stats) != `{"filesCopied":7}` || ja.Summary != "Copied 7 files." || ja.FinishedAt == nil || ja.Error != "" {
		t.Fatalf("completed job = %+v", ja)
	}
	jb := waitStatus(t, m.Store(), b.ID, jobs.StatusCompletedWithWarnings)
	if jb.Warnings != 3 {
		t.Fatalf("warnings = %d", jb.Warnings)
	}
	receive(t, "hook", hooks.ch)
	receive(t, "hook", hooks.ch)
	logs, err := m.Store().ListLogs(context.Background(), a.ID, 0, 0)
	if err != nil || len(logs) < 2 || logs[0].Message != "Job started" || logs[len(logs)-1].Message != "Job finished" {
		t.Fatalf("lifecycle logs = %+v, %v", logs, err)
	}
}

func TestRunnerErrorFailsJobWithRedactedMessage(t *testing.T) {
	const secret = "manager-err-secret-0a1b2c3d"
	logging.RegisterSecret(secret)
	d := openDB(t)
	m := newManager(t, d, Options{})
	m.Register(jobs.TypeSync, jobs.RunnerFunc(func(ctx context.Context, job jobs.Job, env jobs.Env) (jobs.Result, error) {
		return jobs.Result{Stats: map[string]int{"filesCopied": 2}}, fmt.Errorf("destination not mounted? (token %s)", secret)
	}))
	start(t, m)
	j := enqueue(t, m, syncDest1)
	got := waitStatus(t, m.Store(), j.ID, jobs.StatusFailed)
	if strings.Contains(got.Error, secret) || !strings.Contains(got.Error, "destination not mounted?") {
		t.Fatalf("error = %q", got.Error)
	}
	if string(got.Stats) != `{"filesCopied":2}` {
		t.Fatalf("stats of a failed job = %s", got.Stats)
	}
}

func TestRecoveryRequeuesCrashedJob(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	st := NewStore(d)
	j, _, err := st.CreateJob(ctx, syncDest1)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.markRunning(ctx, j.ID, time.Now()); !ok {
		t.Fatal("markRunning")
	}

	m := newManager(t, d, Options{})
	seen := make(chan jobs.Job, 1)
	m.Register(jobs.TypeSync, jobs.RunnerFunc(func(ctx context.Context, job jobs.Job, env jobs.Env) (jobs.Result, error) {
		seen <- job
		return jobs.Result{}, nil
	}))
	start(t, m)
	got := receive(t, "resumed run", seen)
	if got.ID != j.ID || got.Trigger != jobs.TriggerResume || got.Attempt != 2 {
		t.Fatalf("runner saw %+v; want job %d, trigger resume, attempt 2", got, j.ID)
	}
	if !got.QueuedAt.Equal(j.QueuedAt) {
		t.Fatalf("queued_at changed on resume: %v -> %v", j.QueuedAt, got.QueuedAt)
	}
	waitStatus(t, m.Store(), j.ID, jobs.StatusCompleted)
}

func TestRecoveryFailsAfterMaxAttempts(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	st := NewStore(d)
	j, _, _ := st.CreateJob(ctx, syncDest1)
	st.markRunning(ctx, j.ID, time.Now())
	execSQL(t, d, `UPDATE jobs SET attempt = 3 WHERE id = ?`, j.ID)

	m := newManager(t, d, Options{MaxAttempts: 3})
	hooks := newHookRecorder(m)
	var ran atomic.Bool
	m.Register(jobs.TypeSync, jobs.RunnerFunc(func(ctx context.Context, job jobs.Job, env jobs.Env) (jobs.Result, error) {
		ran.Store(true)
		return jobs.Result{}, nil
	}))
	start(t, m)
	got, err := m.Get(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != jobs.StatusFailed || got.Error != "crashed 3 times" || got.FinishedAt == nil || got.Attempt != 3 {
		t.Fatalf("job after recovery = %+v", got)
	}
	h := receive(t, "hook", hooks.ch)
	if h.ID != j.ID || h.Status != jobs.StatusFailed {
		t.Fatalf("hook got %+v", h)
	}
	time.Sleep(50 * time.Millisecond)
	if ran.Load() {
		t.Fatal("a job that crashed too often was run again")
	}
}

func TestRecoveryMessageSingular(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	st := NewStore(d)
	j, _, _ := st.CreateJob(ctx, syncDest1)
	st.markRunning(ctx, j.ID, time.Now())
	m := newManager(t, d, Options{MaxAttempts: 1})
	start(t, m)
	if got, _ := m.Get(ctx, j.ID); got.Status != jobs.StatusFailed || got.Error != "crashed 1 time" {
		t.Fatalf("job = %+v", got)
	}
}

func TestGracefulStopRequeuesWithoutAttemptIncrement(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	m := newManager(t, d, Options{})
	hooks := newHookRecorder(m)
	g := newGatedRunner(true)
	m.Register(jobs.TypeSync, g)
	start(t, m)
	j := enqueue(t, m, syncDest1)
	receive(t, "start", g.started)
	stop(t, m)

	got, err := m.Store().GetJob(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != jobs.StatusQueued || got.Trigger != jobs.TriggerResume || got.Attempt != 1 {
		t.Fatalf("after Stop: %+v; want queued, resume, attempt 1", got)
	}
	if hooks.count(j.ID) != 0 {
		t.Fatal("OnFinish fired for a job re-queued by shutdown")
	}

	// The next process resumes it, still attempt 1.
	m2 := newManager(t, d, Options{})
	seen := make(chan jobs.Job, 1)
	m2.Register(jobs.TypeSync, jobs.RunnerFunc(func(ctx context.Context, job jobs.Job, env jobs.Env) (jobs.Result, error) {
		seen <- job
		return jobs.Result{}, nil
	}))
	start(t, m2)
	r := receive(t, "resumed run", seen)
	if r.ID != j.ID || r.Trigger != jobs.TriggerResume || r.Attempt != 1 {
		t.Fatalf("resumed run saw %+v", r)
	}
	waitStatus(t, m2.Store(), j.ID, jobs.StatusCompleted)
}

func TestStopGraceExpiredRequeuesAndIgnoresLateResult(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	m := newManager(t, d, Options{ShutdownGrace: 100 * time.Millisecond})
	hooks := newHookRecorder(m)
	g := newGatedRunner(false) // ignores cancellation
	m.Register(jobs.TypeSync, g)
	start(t, m)
	j := enqueue(t, m, syncDest1)
	receive(t, "start", g.started)

	began := time.Now()
	stop(t, m)
	if el := time.Since(began); el > 5*time.Second {
		t.Fatalf("Stop took %v with a 100ms grace", el)
	}
	got, _ := m.Store().GetJob(ctx, j.ID)
	if got.Status != jobs.StatusQueued || got.Trigger != jobs.TriggerResume || got.Attempt != 1 {
		t.Fatalf("after Stop: %+v", got)
	}
	close(g.release) // the runner returns late, successfully
	waitFor(t, "runner return", func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.running == 0
	})
	time.Sleep(50 * time.Millisecond)
	got, _ = m.Store().GetJob(ctx, j.ID)
	if got.Status != jobs.StatusQueued || hooks.count(j.ID) != 0 {
		t.Fatalf("late result overwrote the re-queue: %+v (hooks %d)", got, hooks.count(j.ID))
	}
}

func TestCancelQueuedJob(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	m := newManager(t, d, Options{Workers: 1})
	hooks := newHookRecorder(m)
	g := newGatedRunner(true)
	registerAll(m, g)
	start(t, m)
	a := enqueue(t, m, syncDest1)
	receive(t, "start", g.started)
	b := enqueue(t, m, syncDest2) // waits for the only worker

	got, err := m.Cancel(ctx, b.ID)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if got.Status != jobs.StatusCancelled || got.FinishedAt == nil {
		t.Fatalf("cancelled queued job = %+v", got)
	}
	if h := receive(t, "hook", hooks.ch); h.ID != b.ID || h.Status != jobs.StatusCancelled {
		t.Fatalf("hook = %+v", h)
	}
	if _, err := m.Cancel(ctx, b.ID); !errors.Is(err, ErrNotActive) {
		t.Fatalf("second Cancel = %v; want ErrNotActive", err)
	}
	if _, err := m.Cancel(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Cancel(unknown) = %v; want ErrNotFound", err)
	}
	close(g.release)
	waitStatus(t, m.Store(), a.ID, jobs.StatusCompleted)
	select {
	case j := <-g.started:
		t.Fatalf("cancelled job %d ran", j.ID)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestCancelRunningJob(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	m := newManager(t, d, Options{})
	hooks := newHookRecorder(m)
	g := newGatedRunner(true)
	m.Register(jobs.TypeSync, g)
	start(t, m)
	j := enqueue(t, m, syncDest1)
	receive(t, "start", g.started)

	if _, err := m.Cancel(ctx, j.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	got := waitStatus(t, m.Store(), j.ID, jobs.StatusCancelled)
	if got.Error != "" || got.Summary != "Cancelled." || got.FinishedAt == nil {
		t.Fatalf("cancelled job = %+v", got)
	}
	receive(t, "hook", hooks.ch)
	if hooks.count(j.ID) != 1 {
		t.Fatalf("hooks = %d", hooks.count(j.ID))
	}
}

func TestEnqueueDedupeThroughManager(t *testing.T) {
	d := openDB(t)
	m := newManager(t, d, Options{})
	a := enqueue(t, m, syncDest1)
	b := enqueue(t, m, jobs.Spec{Type: jobs.TypeSync, Trigger: jobs.TriggerSchedule, Params: jobs.Params{DestinationID: 1}})
	c := enqueue(t, m, jobs.Spec{Type: jobs.TypeSync, DryRun: true, Params: jobs.Params{DestinationID: 1}})
	if a.ID != b.ID || c.ID == a.ID {
		t.Fatalf("dedupe: a=%d b=%d c=%d", a.ID, b.ID, c.ID)
	}
	if _, err := m.Enqueue(context.Background(), jobs.Spec{Type: jobs.TypeSync}); !isValidation(err) {
		t.Fatalf("invalid spec: %v", err)
	}
}

func TestPerDestinationSerialization(t *testing.T) {
	d := openDB(t)
	m := newManager(t, d, Options{Workers: 4})
	g := newGatedRunner(false)
	registerAll(m, g)
	start(t, m)

	s1 := enqueue(t, m, syncDest1)
	first := receive(t, "first sync", g.started)
	if first.ID != s1.ID {
		t.Fatalf("started %d first", first.ID)
	}
	s2 := enqueue(t, m, syncDest1b)
	v := enqueue(t, m, jobs.Spec{Type: jobs.TypeVerify, Params: jobs.Params{DestinationID: 1}})
	backup := enqueue(t, m, jobs.Spec{Type: jobs.TypePlexDBBackup, Params: jobs.Params{IntegrationID: 5, DestinationID: 1}})

	// The Plex DB backup of the same destination does not wait for the sync.
	if j := receive(t, "backup start", g.started); j.ID != backup.ID {
		t.Fatalf("started %d while sync %d runs; want the backup %d", j.ID, s1.ID, backup.ID)
	}
	select {
	case j := <-g.started:
		t.Fatalf("job %d started while dest:1 is held", j.ID)
	case <-time.After(100 * time.Millisecond):
	}
	for _, id := range []int64{s2.ID, v.ID} {
		if j, _ := m.Get(context.Background(), id); j.Status != jobs.StatusQueued {
			t.Fatalf("job %d is %s while dest:1 is held", id, j.Status)
		}
	}
	close(g.release)
	j1 := waitStatus(t, m.Store(), s1.ID, jobs.StatusCompleted)
	j2 := waitStatus(t, m.Store(), s2.ID, jobs.StatusCompleted)
	jv := waitStatus(t, m.Store(), v.ID, jobs.StatusCompleted)
	waitStatus(t, m.Store(), backup.ID, jobs.StatusCompleted)
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.overlap) > 0 {
		t.Fatalf("jobs overlapped on %v", g.overlap)
	}
	if j2.StartedAt.Before(*j1.FinishedAt) || jv.StartedAt.Before(*j1.FinishedAt) {
		t.Fatal("a dest:1 job started before the first sync finished")
	}
}

func TestWorkerLimit(t *testing.T) {
	d := openDB(t)
	m := newManager(t, d, Options{Workers: 2})
	g := newGatedRunner(false)
	registerAll(m, g)
	var ids []int64
	for i := range 4 {
		ids = append(ids, enqueue(t, m, jobs.Spec{Type: jobs.TypeScan, Params: jobs.Params{SourceIDs: []int64{int64(i + 1)}}}).ID)
	}
	start(t, m)
	receive(t, "start", g.started)
	receive(t, "start", g.started)
	select {
	case j := <-g.started:
		t.Fatalf("a third job (%d) started with 2 workers", j.ID)
	case <-time.After(100 * time.Millisecond):
	}
	page, err := m.List(context.Background(), JobQuery{Status: jobs.StatusRunning})
	if err != nil || page.TotalRecords != 2 {
		t.Fatalf("running = %d, %v", page.TotalRecords, err)
	}
	close(g.release)
	for _, id := range ids {
		waitStatus(t, m.Store(), id, jobs.StatusCompleted)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.maxRun != 2 {
		t.Fatalf("max concurrent = %d; want 2", g.maxRun)
	}
}

func TestBlockedJobDoesNotBlockFreeJob(t *testing.T) {
	d := openDB(t)
	m := newManager(t, d, Options{Workers: 2})
	g := newGatedRunner(false)
	registerAll(m, g)
	a := enqueue(t, m, syncDest1)
	b := enqueue(t, m, syncDest1b) // blocked behind a (dest:1)
	c := enqueue(t, m, syncDest2)  // younger, free key
	start(t, m)

	got := map[int64]bool{}
	got[receive(t, "start", g.started).ID] = true
	got[receive(t, "start", g.started).ID] = true
	if !got[a.ID] || !got[c.ID] {
		t.Fatalf("started %v; want %d and %d (and %d blocked)", got, a.ID, c.ID, b.ID)
	}
	close(g.release)
	waitStatus(t, m.Store(), b.ID, jobs.StatusCompleted)
}

func TestRunnerPanicFailsJob(t *testing.T) {
	const secret = "panic-secret-token-5e6f7a8b"
	logging.RegisterSecret(secret)
	d := openDB(t)
	logs := &syncBuffer{}
	m := New(d, jsonLogger(logs), Options{})
	t.Cleanup(func() { _ = m.Stop(context.Background()) })
	hooks := newHookRecorder(m)
	m.Register(jobs.TypeSync, jobs.RunnerFunc(func(ctx context.Context, job jobs.Job, env jobs.Env) (jobs.Result, error) {
		panic("boom with " + secret)
	}))
	start(t, m)
	j := enqueue(t, m, syncDest1)
	got := waitStatus(t, m.Store(), j.ID, jobs.StatusFailed)
	if got.Error != "runner panicked: boom with "+logging.Redacted {
		t.Fatalf("error = %q", got.Error)
	}
	receive(t, "hook", hooks.ch)
	out := logs.String()
	if strings.Contains(out, secret) || !strings.Contains(out, "goroutine") {
		t.Fatalf("process log must hold the redacted stack: %s", out)
	}
	// The worker survived: the next job runs.
	m.Register(jobs.TypeSync, jobs.RunnerFunc(func(ctx context.Context, job jobs.Job, env jobs.Env) (jobs.Result, error) {
		return jobs.Result{}, nil
	}))
	waitStatus(t, m.Store(), enqueue(t, m, syncDest2).ID, jobs.StatusCompleted)
}

func TestOnFinishOncePerFinalState(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	m := newManager(t, d, Options{Workers: 4})
	m.OnFinish(func(jobs.Job) { panic("a broken hook") })
	hooks := newHookRecorder(m)
	outcomes := map[int64]func(ctx context.Context) (jobs.Result, error){}
	var mu sync.Mutex
	started := make(chan int64, 10)
	m.Register(jobs.TypeSync, jobs.RunnerFunc(func(ctx context.Context, job jobs.Job, env jobs.Env) (jobs.Result, error) {
		mu.Lock()
		f := outcomes[job.Params.DestinationID]
		mu.Unlock()
		started <- job.ID
		return f(ctx)
	}))
	set := func(dest int64, f func(ctx context.Context) (jobs.Result, error)) {
		mu.Lock()
		outcomes[dest] = f
		mu.Unlock()
	}
	set(1, func(context.Context) (jobs.Result, error) { return jobs.Result{}, nil })
	set(2, func(context.Context) (jobs.Result, error) { return jobs.Result{Warnings: 1}, nil })
	set(3, func(context.Context) (jobs.Result, error) { return jobs.Result{}, errors.New("disk full") })
	set(4, func(ctx context.Context) (jobs.Result, error) { <-ctx.Done(); return jobs.Result{}, ctx.Err() })

	want := map[int64]jobs.Status{}
	byDest := map[int64]int64{}
	for dest, st := range map[int64]jobs.Status{1: jobs.StatusCompleted, 2: jobs.StatusCompletedWithWarnings, 3: jobs.StatusFailed, 4: jobs.StatusCancelled} {
		j := enqueue(t, m, jobs.Spec{Type: jobs.TypeSync, Params: jobs.Params{DestinationID: dest}})
		want[j.ID] = st
		byDest[dest] = j.ID
	}
	queued := enqueue(t, m, jobs.Spec{Type: jobs.TypeVerify, Params: jobs.Params{DestinationID: 9}}) // not started yet
	if _, err := m.Cancel(ctx, queued.ID); err != nil {
		t.Fatal(err)
	}
	want[queued.ID] = jobs.StatusCancelled
	start(t, m)
	for range 4 {
		receive(t, "start", started)
	}
	if _, err := m.Cancel(ctx, byDest[4]); err != nil {
		t.Fatal(err)
	}
	for id, st := range want {
		waitStatus(t, m.Store(), id, st)
	}
	for range want {
		receive(t, "hook", hooks.ch)
	}
	stop(t, m)
	for id := range want {
		if n := hooks.count(id); n != 1 {
			t.Errorf("job %d: OnFinish called %d times", id, n)
		}
		if got := hooks.lastStatus(id); got != want[id] {
			t.Errorf("job %d: hook saw %s; want %s", id, got, want[id])
		}
	}
}

func TestUnregisteredTypeFails(t *testing.T) {
	d := openDB(t)
	m := newManager(t, d, Options{})
	start(t, m)
	j := enqueue(t, m, jobs.Spec{Type: jobs.TypeVerify, Params: jobs.Params{DestinationID: 1}})
	got := waitStatus(t, m.Store(), j.ID, jobs.StatusFailed)
	if !strings.Contains(got.Error, "no runner") {
		t.Fatalf("error = %q", got.Error)
	}
}

func TestLiveProgressMerged(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	m := newManager(t, d, Options{ProgressEvery: time.Hour})
	g := newGatedRunner(false)
	reported := make(chan struct{})
	m.Register(jobs.TypeSync, jobs.RunnerFunc(func(ctx context.Context, job jobs.Job, env jobs.Env) (jobs.Result, error) {
		env.Reporter.Progress(jobs.Progress{Phase: "copying", FilesTotal: 10, FilesDone: 1})
		env.Reporter.Progress(jobs.Progress{Phase: "copying", FilesTotal: 10, FilesDone: 4, CurrentFile: "a/b.mkv"})
		close(reported)
		return g.Run(ctx, job, env)
	}))
	start(t, m)
	j := enqueue(t, m, syncDest1)
	receive(t, "progress", reported)

	live, err := m.Get(ctx, j.ID)
	if err != nil || live.Progress.FilesDone != 4 || live.Progress.CurrentFile != "a/b.mkv" {
		t.Fatalf("live progress = %+v, %v", live.Progress, err)
	}
	stored, _ := m.Store().GetJob(ctx, j.ID)
	if stored.Progress.FilesDone != 1 {
		t.Fatalf("stored progress = %+v; want the first (throttled) value", stored.Progress)
	}
	page, _ := m.List(ctx, JobQuery{State: StateActive})
	if len(page.Records) != 1 || page.Records[0].Progress.FilesDone != 4 {
		t.Fatalf("List progress = %+v", page.Records)
	}
	close(g.release)
	done := waitStatus(t, m.Store(), j.ID, jobs.StatusCompleted)
	if done.Progress.FilesDone != 4 {
		t.Fatalf("final progress not persisted: %+v", done.Progress)
	}
}

func TestSimulatedCrashResumes(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	t.Cleanup(func() { faultinject.SetHook(nil) })
	faultinject.SetHook(faultinject.CrashAt(PointBeforeFinish, 1))

	m := newManager(t, d, Options{})
	runs := make(chan jobs.Job, 2)
	runner := jobs.RunnerFunc(func(ctx context.Context, job jobs.Job, env jobs.Env) (jobs.Result, error) {
		runs <- job
		return jobs.Result{}, nil
	})
	m.Register(jobs.TypeSync, runner)
	start(t, m)
	j := enqueue(t, m, syncDest1)
	receive(t, "first run", runs)
	waitFor(t, "simulated crash", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.crashed
	})
	stop(t, m)
	faultinject.SetHook(nil)
	if got, _ := m.Store().GetJob(ctx, j.ID); got.Status != jobs.StatusRunning {
		t.Fatalf("after a simulated crash the job is %s; want running (as a real crash leaves it)", got.Status)
	}

	m2 := newManager(t, d, Options{})
	m2.Register(jobs.TypeSync, runner)
	start(t, m2)
	r := receive(t, "resumed run", runs)
	if r.Attempt != 2 || r.Trigger != jobs.TriggerResume {
		t.Fatalf("resumed run = %+v", r)
	}
	waitStatus(t, m2.Store(), j.ID, jobs.StatusCompleted)
}

// A crash right after the job was marked running, before its runner was called, leaves the job
// running with attempt 1; the next start resumes it as attempt 2 and the runner runs once.
func TestSimulatedCrashAtStartedResumesAsSecondAttempt(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	t.Cleanup(func() { faultinject.SetHook(nil) })
	faultinject.SetHook(faultinject.CrashAt(PointStarted, 1))

	m := newManager(t, d, Options{})
	runs := make(chan jobs.Job, 2)
	runner := jobs.RunnerFunc(func(ctx context.Context, job jobs.Job, env jobs.Env) (jobs.Result, error) {
		runs <- job
		return jobs.Result{}, nil
	})
	m.Register(jobs.TypeSync, runner)
	start(t, m)
	j := enqueue(t, m, syncDest1)
	waitFor(t, "simulated crash", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.crashed
	})
	stop(t, m)
	faultinject.SetHook(nil)
	if len(runs) != 0 {
		t.Fatalf("the runner was called before the crash at %s", PointStarted)
	}
	got, _ := m.Store().GetJob(ctx, j.ID)
	if got.Status != jobs.StatusRunning || got.Attempt != 1 || got.StartedAt == nil {
		t.Fatalf("after a crash at %s the job is %+v; want running, attempt 1 (as a real crash leaves it)", PointStarted, got)
	}

	m2 := newManager(t, d, Options{})
	m2.Register(jobs.TypeSync, runner)
	start(t, m2)
	r := receive(t, "resumed run", runs)
	if r.ID != j.ID || r.Attempt != 2 || r.Trigger != jobs.TriggerResume {
		t.Fatalf("resumed run = %+v; want job %d, attempt 2, trigger resume", r, j.ID)
	}
	done := waitStatus(t, m2.Store(), j.ID, jobs.StatusCompleted)
	if done.Attempt != 2 || len(runs) != 0 {
		t.Fatalf("finished job = %+v, extra runs = %d; want attempt 2 and one run", done, len(runs))
	}
}

func TestStartTwiceAndStopBeforeStart(t *testing.T) {
	d := openDB(t)
	m := New(d, nil, Options{})
	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err == nil {
		t.Fatal("Start after Stop succeeded")
	}
	m2 := newManager(t, d, Options{})
	start(t, m2)
	if err := m2.Start(context.Background()); err == nil {
		t.Fatal("second Start succeeded")
	}
}

func TestStopReturnsAfterGraceWithStragglerAndSlowHook(t *testing.T) {
	d := openDB(t)
	m := newManager(t, d, Options{ShutdownGrace: 100 * time.Millisecond})
	hookEntered, hookRelease := make(chan struct{}), make(chan struct{})
	m.OnFinish(func(jobs.Job) {
		close(hookEntered)
		<-hookRelease // a notification stuck on a slow network
	})
	hold := make(chan struct{})
	t.Cleanup(func() { close(hookRelease); close(hold) })
	straggler := make(chan struct{})
	m.Register(jobs.TypeSync, jobs.RunnerFunc(func(ctx context.Context, job jobs.Job, env jobs.Env) (jobs.Result, error) {
		if job.Params.DestinationID == 2 {
			close(straggler)
			<-hold // ignores cancellation
		}
		return jobs.Result{}, nil
	}))
	start(t, m)
	enqueue(t, m, syncDest1)
	receive(t, "hook in flight", hookEntered)
	b := enqueue(t, m, syncDest2)
	receive(t, "straggler running", straggler)

	stopped := make(chan struct{})
	go func() {
		_ = m.Stop(context.Background()) // no deadline of its own: the grace period must bound it
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return after the grace period")
	}
	if got, _ := m.Store().GetJob(context.Background(), b.ID); got.Status != jobs.StatusQueued || got.Trigger != jobs.TriggerResume {
		t.Fatalf("straggler = %+v", got)
	}
}

func TestSetWorkersAtRuntime(t *testing.T) {
	d := openDB(t)
	m := newManager(t, d, Options{Workers: 1})
	g := newGatedRunner(false)
	registerAll(m, g)
	start(t, m)
	a := enqueue(t, m, syncDest1)
	b := enqueue(t, m, syncDest2)
	receive(t, "first", g.started)
	select {
	case j := <-g.started:
		t.Fatalf("job %d started with 1 worker busy", j.ID)
	case <-time.After(50 * time.Millisecond):
	}
	m.SetWorkers(2)
	receive(t, "second after SetWorkers", g.started)
	close(g.release)
	waitStatus(t, m.Store(), a.ID, jobs.StatusCompleted)
	waitStatus(t, m.Store(), b.ID, jobs.StatusCompleted)
}

func TestDispatcherDoesNotSpinWhenAJobCannotStart(t *testing.T) {
	d := openDB(t)
	var logs syncBuffer
	m := New(d, jsonLogger(&logs), Options{})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
		defer cancel()
		_ = m.Stop(ctx)
	})
	m.Register(jobs.TypeSync, jobs.RunnerFunc(func(ctx context.Context, job jobs.Job, env jobs.Env) (jobs.Result, error) {
		return jobs.Result{}, nil
	}))
	// Marking a job running fails every time (read-only remount, I/O error), while reads work.
	execSQL(t, d, `CREATE TRIGGER fail_start BEFORE UPDATE OF status ON jobs WHEN NEW.status = 'running'
		BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`)
	j := enqueue(t, m, syncDest1)
	start(t, m)
	waitFor(t, "a failed start", func() bool { return strings.Contains(logs.String(), "Could not start job") })
	time.Sleep(300 * time.Millisecond)
	if n := strings.Count(logs.String(), "Could not start job"); n > 3 {
		t.Fatalf("the dispatcher retried a failing start %d times in 300 ms; it must wait for the next poll", n)
	}

	// Writes work again: the next pass starts the job.
	execSQL(t, d, `DROP TRIGGER fail_start`)
	m.wake()
	waitStatus(t, m.Store(), j.ID, jobs.StatusCompleted)
}
