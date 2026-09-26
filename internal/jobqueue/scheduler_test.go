package jobqueue

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

func TestValidateCron(t *testing.T) {
	cases := []struct {
		expr string
		ok   bool
	}{
		{"30 4 * * *", true},
		{"*/15 * * * *", true},
		{"0 6 * * 1-5", true},
		{"0 0 1 */2 *", true},
		{"0 3 * * SUN", true},
		{"  0 3 * * *  ", true},
		{"@hourly", true},
		{"@daily", true},
		{"@weekly", true},
		{"@monthly", true},
		{"", false},
		{"   ", false},
		{"0 30 4 * * *", false}, // seconds
		{"4 * * *", false},
		{"@every 1m", false},
		{"@yearly", false},
		{"@reboot", false},
		{"TZ=UTC 0 4 * * *", false},
		{"CRON_TZ=Europe/Berlin 0 4 * * *", false},
		{"TZ=UTC", false}, // robfig/cron panics on this one without the prefix check
		{"61 * * * *", false},
		{"* 25 * * *", false},
		{"*/0 * * * *", false},
		{"0 0 30 2 *", false}, // never fires
		{"a b c d e", false},
	}
	for _, tc := range cases {
		err := ValidateCron(tc.expr)
		if (err == nil) != tc.ok {
			t.Errorf("ValidateCron(%q) = %v; want ok=%v", tc.expr, err, tc.ok)
		}
		if err != nil && !isValidation(err) {
			t.Errorf("ValidateCron(%q) error %v is not a ValidationError", tc.expr, err)
		}
	}
}

// fakeClock drives the scheduler loop in tests.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at    time.Time
	ch    chan time.Time
	fired bool
	dead  bool
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) NewTimer(d time.Duration) (<-chan time.Time, func() bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.now.Add(d), ch: make(chan time.Time, 1)}
	if d <= 0 {
		t.ch <- c.now
		t.fired = true
	} else {
		c.timers = append(c.timers, t)
	}
	return t.ch, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		was := !t.fired && !t.dead
		t.dead = true
		return was
	}
}

// Set moves the clock (forwards or, like an NTP step, backwards) and fires due timers.
func (c *fakeClock) Set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
	live := c.timers[:0]
	for _, t := range c.timers {
		switch {
		case t.dead || t.fired:
		case !t.at.After(now):
			t.ch <- now
			t.fired = true
		default:
			live = append(live, t)
		}
	}
	c.timers = live
}

// fakeEnqueuer records specs.
type fakeEnqueuer struct {
	mu    sync.Mutex
	specs []jobs.Spec
	ch    chan jobs.Spec
}

func newFakeEnqueuer() *fakeEnqueuer { return &fakeEnqueuer{ch: make(chan jobs.Spec, 100)} }

func (f *fakeEnqueuer) Enqueue(ctx context.Context, spec jobs.Spec) (jobs.Job, error) {
	f.mu.Lock()
	f.specs = append(f.specs, spec)
	n := len(f.specs)
	f.mu.Unlock()
	f.ch <- spec
	return jobs.Job{ID: int64(n), Type: spec.Type, Trigger: spec.Trigger, Params: spec.Params, Status: jobs.StatusQueued}, nil
}

func (f *fakeEnqueuer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.specs)
}

func newTestScheduler(t *testing.T, st *Store, enq jobs.Enqueuer, clk *fakeClock) *Scheduler {
	t.Helper()
	s := NewScheduler(st, enq, nil, time.UTC)
	s.clk = clk
	t.Cleanup(s.Stop)
	return s
}

func at(h, m, s int) time.Time { return time.Date(2026, 9, 24, h, m, s, 0, time.UTC) }

func TestSchedulerFiresAndRecordsRun(t *testing.T) {
	ctx := context.Background()
	st := NewStore(openDB(t))
	sc, err := st.UpsertSchedule(ctx, jobs.TypeSync, jobs.Params{DestinationID: 3}, "0 4 * * *", true)
	if err != nil {
		t.Fatal(err)
	}
	clk := &fakeClock{now: at(3, 59, 30)}
	enq := newFakeEnqueuer()
	s := newTestScheduler(t, st, enq, clk)
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if got := s.NextRun(sc.ID); !got.Equal(at(4, 0, 0)) {
		t.Fatalf("NextRun = %v", got)
	}
	// The seeded global retention schedule (04:30) is loaded too.
	if got := s.NextRun(1); !got.Equal(at(4, 30, 0)) {
		t.Fatalf("retention NextRun = %v", got)
	}

	clk.Set(at(4, 0, 0))
	spec := receive(t, "scheduled sync", enq.ch)
	if spec.Type != jobs.TypeSync || spec.Trigger != jobs.TriggerSchedule || spec.Params.DestinationID != 3 || spec.DryRun {
		t.Fatalf("spec = %+v", spec)
	}
	waitFor(t, "last run recorded", func() bool {
		got, _ := st.GetSchedule(ctx, sc.ID)
		return got.LastRunAt != nil && got.LastRunAt.Equal(at(4, 0, 0))
	})
	if got := s.NextRun(sc.ID); !got.Equal(at(4, 0, 0).Add(24 * time.Hour)) {
		t.Fatalf("NextRun after firing = %v", got)
	}

	clk.Set(at(4, 30, 5))
	if spec := receive(t, "retention", enq.ch); spec.Type != jobs.TypeRetention || !reflect.DeepEqual(spec.Params, jobs.Params{}) {
		t.Fatalf("spec = %+v", spec)
	}

	// Asleep for days: each schedule fires once, not once per missed slot.
	clk.Set(at(4, 0, 0).Add(72 * time.Hour))
	receive(t, "late sync", enq.ch)
	receive(t, "late retention", enq.ch)
	if got := s.NextRun(sc.ID); !got.Equal(at(4, 0, 0).Add(96 * time.Hour)) {
		t.Fatalf("NextRun after a long sleep = %v", got)
	}
	s.Stop()
	if n := enq.count(); n != 4 {
		t.Fatalf("enqueued %d jobs; want 4", n)
	}
}

func TestSchedulerSkipsWhenRunning(t *testing.T) {
	ctx := context.Background()
	st := NewStore(openDB(t))
	sc, _ := st.UpsertSchedule(ctx, jobs.TypeSync, jobs.Params{DestinationID: 3}, "*/5 * * * *", true)
	// A manual sync of the same destination (other params) is running.
	running, _, _ := st.CreateJob(ctx, jobs.Spec{Type: jobs.TypeSync, Params: jobs.Params{DestinationID: 3, AllowChanges: true}})
	st.markRunning(ctx, running.ID, time.Now())
	// A running dry run of it does not count.
	dry, _, _ := st.CreateJob(ctx, jobs.Spec{Type: jobs.TypeSync, DryRun: true, Params: jobs.Params{DestinationID: 4}})
	st.markRunning(ctx, dry.ID, time.Now())
	sc4, _ := st.UpsertSchedule(ctx, jobs.TypeSync, jobs.Params{DestinationID: 4}, "*/5 * * * *", true)

	clk := &fakeClock{now: at(10, 4, 0)}
	enq := newFakeEnqueuer()
	s := newTestScheduler(t, st, enq, clk)
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	clk.Set(at(10, 5, 0))
	if spec := receive(t, "sync of dest 4", enq.ch); spec.Params.DestinationID != 4 {
		t.Fatalf("spec = %+v", spec)
	}
	s.Stop() // waits for the fire in progress
	if n := enq.count(); n != 1 {
		t.Fatalf("enqueued %d; the sync of destination 3 must be skipped while it runs", n)
	}
	got, _ := st.GetSchedule(ctx, sc.ID)
	if got.LastRunAt != nil {
		t.Fatal("a skipped run was recorded as a run")
	}
	if got, _ := st.GetSchedule(ctx, sc4.ID); got.LastRunAt == nil {
		t.Fatal("run of destination 4 not recorded")
	}
}

// TestSkipRuleNeedsARunningJobOfTheScheduledSources: a scheduled sync or verify is skipped only
// while a job of its destination runs without paths for all the sources it would do. An
// untargeted follow-up sync of one source (a missing folder, a source root, an overflow) never
// makes the scheduled sync of every source skip its turn (phase2-3.md §12.1, D8).
func TestSkipRuleNeedsARunningJobOfTheScheduledSources(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		typ       jobs.Type
		running   jobs.Params
		scheduled jobs.Params
		want      bool
	}{
		{"one source under all", jobs.TypeSync, jobs.Params{DestinationID: 3, SourceIDs: []int64{1}}, jobs.Params{DestinationID: 3}, false},
		{"one source under two", jobs.TypeSync, jobs.Params{DestinationID: 3, SourceIDs: []int64{1}}, jobs.Params{DestinationID: 3, SourceIDs: []int64{1, 2}}, false},
		{"other source", jobs.TypeSync, jobs.Params{DestinationID: 3, SourceIDs: []int64{1}}, jobs.Params{DestinationID: 3, SourceIDs: []int64{2}}, false},
		{"same source", jobs.TypeSync, jobs.Params{DestinationID: 3, SourceIDs: []int64{1}}, jobs.Params{DestinationID: 3, SourceIDs: []int64{1}}, true},
		{"two sources over one", jobs.TypeSync, jobs.Params{DestinationID: 3, SourceIDs: []int64{1, 2}}, jobs.Params{DestinationID: 3, SourceIDs: []int64{2}}, true},
		{"all over one", jobs.TypeSync, jobs.Params{DestinationID: 3}, jobs.Params{DestinationID: 3, SourceIDs: []int64{2}}, true},
		{"all over all", jobs.TypeSync, jobs.Params{DestinationID: 3, AllowChanges: true}, jobs.Params{DestinationID: 3}, true},
		{"targeted", jobs.TypeSync, jobs.Params{DestinationID: 3, SourceIDs: []int64{1}, Paths: []string{"x"}}, jobs.Params{DestinationID: 3, SourceIDs: []int64{1}}, false},
		{"other destination", jobs.TypeSync, jobs.Params{DestinationID: 4}, jobs.Params{DestinationID: 3}, false},
		{"verify of one source under all", jobs.TypeVerify, jobs.Params{DestinationID: 3, SourceIDs: []int64{1}}, jobs.Params{DestinationID: 3}, false},
		{"verify of all", jobs.TypeVerify, jobs.Params{DestinationID: 3}, jobs.Params{DestinationID: 3, SourceIDs: []int64{1}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := NewStore(openDB(t))
			running, _, err := st.CreateJob(ctx, jobs.Spec{Type: tc.typ, Trigger: jobs.TriggerWebhook, Params: tc.running})
			if err != nil {
				t.Fatal(err)
			}
			if _, ok, err := st.markRunning(ctx, running.ID, time.Now()); !ok || err != nil {
				t.Fatalf("markRunning: %v %v", ok, err)
			}
			if got, err := st.hasRunning(ctx, tc.typ, tc.scheduled); got != tc.want || err != nil {
				t.Fatalf("hasRunning(%+v) with %+v running = %v, %v; want %v", tc.scheduled, tc.running, got, err, tc.want)
			}
		})
	}

	// Through the scheduler: the nightly sync of every source is queued, not skipped, while a
	// follow-up sync of one source runs.
	st := NewStore(openDB(t))
	sc, _ := st.UpsertSchedule(ctx, jobs.TypeSync, jobs.Params{DestinationID: 3}, "0 1 * * *", true)
	followUp, _, _ := st.CreateJob(ctx, jobs.Spec{Type: jobs.TypeSync, Trigger: jobs.TriggerWebhook, Params: jobs.Params{DestinationID: 3, SourceIDs: []int64{1}}})
	if _, ok, err := st.markRunning(ctx, followUp.ID, time.Now()); !ok || err != nil {
		t.Fatalf("markRunning: %v %v", ok, err)
	}
	clk := &fakeClock{now: at(0, 59, 0)}
	enq := newFakeEnqueuer()
	s := newTestScheduler(t, st, enq, clk)
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	clk.Set(at(1, 0, 0))
	if spec := receive(t, "nightly sync of destination 3", enq.ch); spec.Params.DestinationID != 3 || len(spec.Params.SourceIDs) != 0 {
		t.Fatalf("spec = %+v", spec)
	}
	s.Stop()
	if got, _ := st.GetSchedule(ctx, sc.ID); got.LastRunAt == nil {
		t.Fatal("the nightly sync was not recorded as a run")
	}
}

func TestSchedulerReloadNeverFiresTwiceForOneMinute(t *testing.T) {
	ctx := context.Background()
	st := NewStore(openDB(t))
	sc, _ := st.UpsertSchedule(ctx, jobs.TypeVerify, jobs.Params{DestinationID: 1}, "0 4 * * *", true)
	clk := &fakeClock{now: at(3, 59, 50)}
	enq := newFakeEnqueuer()
	s := newTestScheduler(t, st, enq, clk)
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	clk.Set(at(4, 0, 0))
	receive(t, "fire", enq.ch)
	waitFor(t, "last run", func() bool {
		got, _ := st.GetSchedule(ctx, sc.ID)
		return got.LastRunAt != nil
	})

	// The wall clock steps back 5 s and the schedule is edited to another expression matching
	// 04:00; the reload must not fire 04:00 again.
	clk.Set(at(3, 59, 55))
	if _, err := st.UpdateSchedule(ctx, sc.ID, "0 4,5 * * *", true); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if got := s.NextRun(sc.ID); !got.Equal(at(5, 0, 0)) {
		t.Fatalf("NextRun after reload = %v; want 05:00", got)
	}
	// Reloading an unchanged schedule keeps its next run.
	if err := s.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if got := s.NextRun(sc.ID); !got.Equal(at(5, 0, 0)) {
		t.Fatalf("NextRun after second reload = %v", got)
	}
	clk.Set(at(4, 0, 30))
	time.Sleep(50 * time.Millisecond)

	// A new process (no in-memory history) with a skewed clock uses last_run_at.
	s.Stop()
	s2 := newTestScheduler(t, st, enq, &fakeClock{now: at(3, 59, 58)})
	if err := s2.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if got := s2.NextRun(sc.ID); !got.Equal(at(5, 0, 0)) {
		t.Fatalf("new scheduler NextRun = %v; want 05:00", got)
	}
	s2.Stop()
	if n := enq.count(); n != 1 {
		t.Fatalf("fired %d times; want 1", n)
	}
}

func TestSchedulerReloadAddsAndRemoves(t *testing.T) {
	ctx := context.Background()
	st := NewStore(openDB(t))
	clk := &fakeClock{now: at(8, 0, 0)}
	enq := newFakeEnqueuer()
	s := newTestScheduler(t, st, enq, clk)
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	sc, _ := st.UpsertSchedule(ctx, jobs.TypePlexDBBackup, jobs.Params{IntegrationID: 2, DestinationID: 1}, "0 6 * * *", true)
	if !s.NextRun(sc.ID).IsZero() {
		t.Fatal("schedule known before Reload")
	}
	if err := s.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if got := s.NextRun(sc.ID); !got.Equal(at(6, 0, 0).Add(24 * time.Hour)) {
		t.Fatalf("NextRun = %v", got)
	}
	st.UpdateSchedule(ctx, sc.ID, "0 6 * * *", false)
	s.Reload(ctx)
	if !s.NextRun(sc.ID).IsZero() {
		t.Fatal("disabled schedule still scheduled")
	}
	// A disabled schedule can still be run by hand.
	job, err := s.RunNow(ctx, sc.ID)
	if err != nil || job.Trigger != jobs.TriggerManual || job.Params.IntegrationID != 2 {
		t.Fatalf("RunNow = %+v, %v", job, err)
	}
	if got, _ := st.GetSchedule(ctx, sc.ID); got.LastRunAt == nil {
		t.Fatal("RunNow not recorded")
	}
	if _, err := s.RunNow(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RunNow(unknown) = %v", err)
	}
	// An invalid expression in the table (written behind the store's back) is skipped.
	execSQL(t, st.db, `UPDATE schedules SET cron = 'nonsense', enabled = 1 WHERE id = ?`, sc.ID)
	if err := s.Reload(ctx); err != nil || !s.NextRun(sc.ID).IsZero() {
		t.Fatalf("invalid schedule loaded: %v", err)
	}
}

func TestSchedulerWithManagerSkipsAndStopsWithManager(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	m := newManager(t, d, Options{})
	g := newGatedRunner(true)
	m.Register(jobs.TypeSync, g)
	start(t, m)
	sc, _ := m.Store().UpsertSchedule(ctx, jobs.TypeSync, jobs.Params{DestinationID: 1}, "* * * * *", true)
	clk := &fakeClock{now: at(9, 0, 30)}
	s := NewScheduler(m.Store(), m, nil, time.UTC)
	s.clk = clk
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	clk.Set(at(9, 1, 0))
	first := receive(t, "scheduled job running", g.started)
	if first.Trigger != jobs.TriggerSchedule {
		t.Fatalf("job = %+v", first)
	}
	clk.Set(at(9, 2, 0)) // still running: skipped
	waitFor(t, "next minute scheduled", func() bool { return s.NextRun(sc.ID).Equal(at(9, 3, 0)) })
	stop(t, m) // also stops the scheduler
	close(g.release)
	s.mu.Lock()
	done := s.done
	s.mu.Unlock()
	select {
	case <-done:
	default:
		t.Fatal("Manager.Stop did not stop the attached scheduler")
	}
	page, _ := m.Store().ListJobs(ctx, JobQuery{Type: jobs.TypeSync})
	if page.TotalRecords != 1 {
		t.Fatalf("jobs = %d; want 1 (the second minute skipped while the first ran)", page.TotalRecords)
	}
}

func TestSchedulesStore(t *testing.T) {
	ctx := context.Background()
	st := NewStore(openDB(t))

	a, err := st.UpsertSchedule(ctx, jobs.TypeSync, jobs.Params{DestinationID: 1}, "0 3 * * *", true)
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.UpsertSchedule(ctx, jobs.TypeSync, jobs.Params{DestinationID: 1}, " 0 4 * * * ", false)
	if err != nil || b.ID != a.ID || b.Cron != "0 4 * * *" || b.Enabled {
		t.Fatalf("upsert of the same key = %+v, %v", b, err)
	}
	scanA, _ := st.UpsertSchedule(ctx, jobs.TypeScan, jobs.Params{SourceIDs: []int64{2, 1}}, "@daily", true)
	scanB, _ := st.UpsertSchedule(ctx, jobs.TypeScan, jobs.Params{SourceIDs: []int64{1, 2, 2}}, "@hourly", true)
	if scanA.ID != scanB.ID || !reflect.DeepEqual(scanB.Params.SourceIDs, []int64{1, 2}) {
		t.Fatalf("params must be keyed canonically: %+v vs %+v", scanA, scanB)
	}
	verify, _ := st.UpsertSchedule(ctx, jobs.TypeVerify, jobs.Params{DestinationID: 1}, "@weekly", true)
	backup, _ := st.UpsertSchedule(ctx, jobs.TypePlexDBBackup, jobs.Params{IntegrationID: 7, DestinationID: 1}, "0 6 * * *", true)
	other, _ := st.UpsertSchedule(ctx, jobs.TypeSync, jobs.Params{DestinationID: 2}, "0 3 * * *", true)

	for _, bad := range []struct {
		typ  jobs.Type
		p    jobs.Params
		cron string
	}{
		{jobs.TypeSync, jobs.Params{DestinationID: 1}, "0 0 3 * * *"},
		{jobs.TypeSync, jobs.Params{}, "0 3 * * *"},
		{"nope", jobs.Params{}, "0 3 * * *"},
	} {
		if _, err := st.UpsertSchedule(ctx, bad.typ, bad.p, bad.cron, true); !isValidation(err) {
			t.Errorf("UpsertSchedule(%v, %+v, %q) = %v; want ValidationError", bad.typ, bad.p, bad.cron, err)
		}
	}

	u, err := st.UpdateSchedule(ctx, a.ID, "@daily", true)
	if err != nil || u.Cron != "@daily" || !u.Enabled {
		t.Fatalf("UpdateSchedule = %+v, %v", u, err)
	}
	if _, err := st.UpdateSchedule(ctx, a.ID, "every day", true); !isValidation(err) {
		t.Fatalf("UpdateSchedule(bad cron) = %v", err)
	}
	if _, err := st.UpdateSchedule(ctx, 999, "@daily", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateSchedule(unknown) = %v", err)
	}
	ranAt := time.Date(2026, 9, 1, 4, 30, 0, 0, time.UTC)
	if err := st.MarkRun(ctx, a.ID, ranAt); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetSchedule(ctx, a.ID); got.LastRunAt == nil || !got.LastRunAt.Equal(ranAt) {
		t.Fatalf("LastRunAt = %v", got.LastRunAt)
	}

	if _, err := st.DeleteSchedulesFor(ctx, jobs.Params{}); !isValidation(err) {
		t.Fatalf("DeleteSchedulesFor({}) = %v; want ValidationError", err)
	}
	n, err := st.DeleteSchedulesFor(ctx, jobs.Params{DestinationID: 1})
	if err != nil || n != 3 {
		t.Fatalf("DeleteSchedulesFor(dest 1) = %d, %v; want 3", n, err)
	}
	for _, id := range []int64{a.ID, verify.ID, backup.ID} {
		if _, err := st.GetSchedule(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("schedule %d survived: %v", id, err)
		}
	}
	if n, _ := st.DeleteSchedulesFor(ctx, jobs.Params{SourceIDs: []int64{2}}); n != 1 {
		t.Fatalf("DeleteSchedulesFor(source 2) = %d", n)
	}
	list, _ := st.ListSchedules(ctx)
	var ids []int64
	for _, sc := range list {
		ids = append(ids, sc.ID)
	}
	if !reflect.DeepEqual(ids, []int64{1, other.ID}) {
		t.Fatalf("remaining schedules = %v; want the seeded retention and %d", ids, other.ID)
	}
	if err := st.DeleteSchedule(ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteSchedule(ctx, other.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second DeleteSchedule = %v", err)
	}
}

func TestSchedulerPlexBackupNotSkippedForAnotherIntegration(t *testing.T) {
	ctx := context.Background()
	st := NewStore(openDB(t))
	// Two Plex servers back up to destination 7 on the same cron.
	scA, _ := st.UpsertSchedule(ctx, jobs.TypePlexDBBackup, jobs.Params{IntegrationID: 1, DestinationID: 7}, "0 6 * * *", true)
	scB, _ := st.UpsertSchedule(ctx, jobs.TypePlexDBBackup, jobs.Params{IntegrationID: 2, DestinationID: 7}, "0 6 * * *", true)
	// Server 1's backup to destination 7 is still running.
	running, _, _ := st.CreateJob(ctx, jobs.Spec{Type: jobs.TypePlexDBBackup, Params: jobs.Params{IntegrationID: 1, DestinationID: 7}})
	if _, ok, err := st.markRunning(ctx, running.ID, time.Now()); !ok || err != nil {
		t.Fatalf("markRunning: %v %v", ok, err)
	}

	clk := &fakeClock{now: at(5, 59, 0)}
	enq := newFakeEnqueuer()
	s := newTestScheduler(t, st, enq, clk)
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	clk.Set(at(6, 0, 0))
	if spec := receive(t, "backup of server 2", enq.ch); spec.Params.IntegrationID != 2 {
		t.Fatalf("spec = %+v", spec)
	}
	s.Stop()
	if n := enq.count(); n != 1 {
		t.Fatalf("enqueued %d; want only server 2's backup (server 1's is running)", n)
	}
	if got, _ := st.GetSchedule(ctx, scA.ID); got.LastRunAt != nil {
		t.Fatal("the skipped backup of server 1 was recorded as a run")
	}
	if got, _ := st.GetSchedule(ctx, scB.ID); got.LastRunAt == nil {
		t.Fatal("backup of server 2 not recorded")
	}

	// Syncs and verifies keep the per-destination rule: any running one of the destination counts.
	verify, _, _ := st.CreateJob(ctx, jobs.Spec{Type: jobs.TypeVerify, Params: jobs.Params{DestinationID: 3, AllowChanges: true}})
	st.markRunning(ctx, verify.ID, time.Now())
	if ok, err := st.hasRunning(ctx, jobs.TypeVerify, jobs.Params{DestinationID: 3}); !ok || err != nil {
		t.Fatalf("hasRunning(verify of destination 3) = %v, %v; want true", ok, err)
	}
}

func TestSchedulerConcurrentReloadsKeepTheNewestSchedules(t *testing.T) {
	ctx := context.Background()
	st := NewStore(openDB(t))
	s := newTestScheduler(t, st, newFakeEnqueuer(), &fakeClock{now: at(8, 0, 0)})
	// The first reload reads the schedules, then stalls before applying them.
	readA, releaseA := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	s.listSchedules = func(ctx context.Context) ([]Schedule, error) {
		list, err := st.ListSchedules(ctx)
		if calls.Add(1) == 1 {
			close(readA)
			<-releaseA
		}
		return list, err
	}
	errA := make(chan error, 1)
	go func() { errA <- s.Reload(ctx) }()
	<-readA

	// Meanwhile another request adds a schedule and reloads.
	sc, err := st.UpsertSchedule(ctx, jobs.TypeSync, jobs.Params{DestinationID: 2}, "0 6 * * *", true)
	if err != nil {
		t.Fatal(err)
	}
	errB := make(chan error, 1)
	go func() { errB <- s.Reload(ctx) }()
	var bErr error
	bDone := false
	select {
	case bErr = <-errB: // unserialized: the second reload applies first
		bDone = true
	case <-time.After(200 * time.Millisecond): // serialized: it waits for the first one
	}
	close(releaseA)
	if err := receive(t, "first reload", errA); err != nil {
		t.Fatal(err)
	}
	if !bDone {
		bErr = receive(t, "second reload", errB)
	}
	if bErr != nil {
		t.Fatal(bErr)
	}
	if s.NextRun(sc.ID).IsZero() {
		t.Fatal("a stale reload overwrote the newer schedule list")
	}
}

func TestSchedulerReloadOutlivesTheCallersContext(t *testing.T) {
	ctx := context.Background()
	st := NewStore(openDB(t))
	s := newTestScheduler(t, st, newFakeEnqueuer(), &fakeClock{now: at(8, 0, 0)})
	var hasDeadline bool
	s.listSchedules = func(ctx context.Context) ([]Schedule, error) {
		_, hasDeadline = ctx.Deadline()
		return st.ListSchedules(ctx)
	}
	sc, err := st.UpsertSchedule(ctx, jobs.TypeSync, jobs.Params{DestinationID: 2}, "0 6 * * *", true)
	if err != nil {
		t.Fatal(err)
	}
	// The HTTP client disconnected right after the schedule was stored.
	reqCtx, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.Reload(reqCtx); err != nil {
		t.Fatalf("Reload with a cancelled request context: %v", err)
	}
	if s.NextRun(sc.ID).IsZero() {
		t.Fatal("the stored schedule was not loaded")
	}
	if !hasDeadline {
		t.Fatal("Reload's database read has no timeout")
	}
}

// RunNow with dryRun queues a dry run of the schedule's job (a retention preview): a separate job
// from the real run, deduped only with another queued dry run, and not recorded as a run of the
// schedule.
func TestRunNowDryRunIsASeparateJob(t *testing.T) {
	ctx := context.Background()
	m := newManager(t, openDB(t), Options{}) // not started: the jobs stay queued
	st := m.Store()
	s := newTestScheduler(t, st, m, &fakeClock{now: at(8, 0, 0)})
	sc, err := st.UpsertSchedule(ctx, jobs.TypeRetention, jobs.Params{DestinationID: 1}, "30 4 * * *", true)
	if err != nil {
		t.Fatal(err)
	}

	dry, err := s.RunNow(ctx, sc.ID, true)
	if err != nil || !dry.DryRun || dry.Type != jobs.TypeRetention || dry.Trigger != jobs.TriggerManual ||
		dry.Params.DestinationID != 1 || dry.Status != jobs.StatusQueued {
		t.Fatalf("RunNow(dryRun) = %+v, %v", dry, err)
	}
	if stored, _ := st.GetJob(ctx, dry.ID); !stored.DryRun {
		t.Fatalf("stored dry run = %+v", stored)
	}
	if got, _ := st.GetSchedule(ctx, sc.ID); got.LastRunAt != nil {
		t.Fatalf("a dry run was recorded as a run of the schedule: lastRunAt = %v", got.LastRunAt)
	}
	if again, err := s.RunNow(ctx, sc.ID, true); err != nil || again.ID != dry.ID {
		t.Fatalf("second RunNow(dryRun) = %+v, %v; want the queued dry run %d", again, err, dry.ID)
	}

	run, err := s.RunNow(ctx, sc.ID, false)
	if err != nil || run.DryRun || run.ID == dry.ID {
		t.Fatalf("RunNow(real) = %+v, %v; want a new job, not the queued dry run %d", run, err, dry.ID)
	}
	if again, err := s.RunNow(ctx, sc.ID); err != nil || again.ID != run.ID {
		t.Fatalf("RunNow without a flag = %+v, %v; want the queued real run %d", again, err, run.ID)
	}
	if got, _ := st.GetSchedule(ctx, sc.ID); got.LastRunAt == nil || !got.LastRunAt.Equal(at(8, 0, 0)) {
		t.Fatalf("real run not recorded: lastRunAt = %v", got.LastRunAt)
	}
	page, _ := st.ListJobs(ctx, JobQuery{Type: jobs.TypeRetention})
	if page.TotalRecords != 2 {
		t.Fatalf("jobs = %d; want 2 (one dry run, one real run)", page.TotalRecords)
	}
}
