package jobqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// deferSQL makes queued job id a deferred one (as a deferral leaves it).
func deferSQL(t *testing.T, st *Store, id int64, until time.Time) {
	t.Helper()
	execSQL(t, st.db, `UPDATE jobs SET status = 'queued', deferrals = deferrals + 1, not_before = ? WHERE id = ?`,
		until.UTC().Format("2006-01-02T15:04:05.000000000Z07:00"), id)
}

// logText returns a job's log lines joined.
func logText(t *testing.T, st *Store, id int64) string {
	t.Helper()
	logs, err := st.ListLogs(context.Background(), id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, l := range logs {
		b.WriteString(l.Message)
		b.WriteString("\n")
	}
	return b.String()
}

// TestDeferral: a runner's jobs.DeferredError re-queues the job with not_before and deferrals + 1,
// keeps attempt, trigger, items and planned_at, runs no hook, releases the worker, the lock keys
// and the slot, and starts the job again at not_before (phase4.md §11.2).
func TestDeferral(t *testing.T) {
	d := openDB(t)
	engineSync := func(ctx context.Context, j jobs.Job) (bool, error) { return j.Type == jobs.TypeSync && !j.DryRun, nil }
	m := newManager(t, d, Options{Workers: 1, Slots: []SlotPool{{Name: "upload", Limit: 1, Needs: engineSync}}})
	hooks := newHookRecorder(m)
	var (
		mu      sync.Mutex
		until   time.Time
		calls   = map[int64]int{}
		second  = make(chan jobs.Job, 4)
		otherGo = make(chan jobs.Job, 4)
	)
	m.Register(jobs.TypeSync, jobs.RunnerFunc(func(ctx context.Context, job jobs.Job, env jobs.Env) (jobs.Result, error) {
		mu.Lock()
		calls[job.ID]++
		n := calls[job.ID]
		mu.Unlock()
		if job.Params.DestinationID == 2 {
			otherGo <- job
			return jobs.Result{Summary: "other"}, nil
		}
		if n == 1 {
			if err := env.Items.AddItems(ctx, job.ID, []jobs.Item{{RelPath: "a.mkv", Action: jobs.ActionCopy, Status: jobs.ItemPending, Bytes: 5}}, true); err != nil {
				return jobs.Result{}, err
			}
			mu.Lock()
			until = time.Now().Add(400 * time.Millisecond)
			u := until
			mu.Unlock()
			return jobs.Result{}, fmt.Errorf("batch stopped: %w", jobs.Defer(u, "waiting for the transfer window (01:00-07:00)"))
		}
		second <- job
		return jobs.Result{Summary: "done"}, nil
	}))
	start(t, m)
	a := enqueue(t, m, jobs.Spec{Type: jobs.TypeSync, Trigger: jobs.TriggerWebhook, Params: jobs.Params{DestinationID: 1}})

	j := waitFor2(t, m.Store(), a.ID, func(j jobs.Job) bool { return j.Status == jobs.StatusQueued && j.Deferrals == 1 })
	mu.Lock()
	u := until
	mu.Unlock()
	if j.NotBefore == nil || !j.NotBefore.Equal(u.UTC().Round(0)) || j.Attempt != 1 || j.Trigger != jobs.TriggerWebhook ||
		!strings.HasPrefix(j.Summary, "Waiting for the transfer window until ") || j.FinishedAt != nil {
		t.Fatalf("deferred job = %+v (until %v)", j, u)
	}
	if planned, err := m.Store().Planned(context.Background(), a.ID); err != nil || !planned {
		t.Fatalf("planned_at lost: %v, %v", planned, err)
	}
	if items, err := m.Store().Pending(context.Background(), a.ID, 0, 10); err != nil || len(items) != 1 {
		t.Fatalf("items = %+v, %v", items, err)
	}
	if !strings.Contains(logText(t, m.Store(), a.ID), "Waiting for the transfer window until") {
		t.Fatalf("log = %s", logText(t, m.Store(), a.ID))
	}
	// Worker (1), keys and the upload slot (1) are free: another sync runs while it waits.
	b := enqueue(t, m, jobs.Spec{Type: jobs.TypeSync, Params: jobs.Params{DestinationID: 2}})
	receive(t, "other sync", otherGo)
	waitStatus(t, m.Store(), b.ID, jobs.StatusCompleted)
	// It starts again at not_before, with attempt and trigger unchanged, deferrals 1.
	got := receive(t, "second run", second)
	if time.Now().Before(u) {
		t.Fatal("the deferred job started before its not_before")
	}
	if got.Attempt != 1 || got.Trigger != jobs.TriggerWebhook || got.Deferrals != 1 || got.NotBefore != nil {
		t.Fatalf("resumed job = %+v", got)
	}
	done := waitStatus(t, m.Store(), a.ID, jobs.StatusCompleted)
	if done.Deferrals != 1 {
		t.Fatalf("finished job = %+v", done)
	}
	waitFor(t, "the finished job's hook", func() bool { return hooks.count(a.ID) == 1 })
	time.Sleep(20 * time.Millisecond)
	if c := hooks.count(a.ID); c != 1 {
		t.Fatalf("hooks ran %d times for the deferred job; want 1 (only when it finished)", c)
	}
}

// waitFor2 polls job id until ok holds and returns it.
func waitFor2(t *testing.T, st *Store, id int64, ok func(jobs.Job) bool) jobs.Job {
	t.Helper()
	var j jobs.Job
	waitFor(t, "job state", func() bool {
		var err error
		if j, err = st.GetJob(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		return ok(j)
	})
	return j
}

// TestDeferralWithoutAFutureTimeFails: a zero or past Until fails the job.
func TestDeferralWithoutAFutureTimeFails(t *testing.T) {
	for _, tc := range []struct {
		name  string
		until time.Time
	}{{"zero", time.Time{}}, {"past", time.Now().Add(-time.Minute)}} {
		t.Run(tc.name, func(t *testing.T) {
			m := newManager(t, openDB(t), Options{})
			hooks := newHookRecorder(m)
			m.Register(jobs.TypeSync, jobs.RunnerFunc(func(ctx context.Context, job jobs.Job, env jobs.Env) (jobs.Result, error) {
				return jobs.Result{}, jobs.Defer(tc.until, "window")
			}))
			start(t, m)
			a := enqueue(t, m, syncDest1)
			j := waitStatus(t, m.Store(), a.ID, jobs.StatusFailed)
			if !strings.Contains(j.Error, "deferred without a time to resume") || j.Deferrals != 0 {
				t.Fatalf("job = %+v", j)
			}
			receive(t, "hook", hooks.ch)
		})
	}
}

// TestCancelDeferredJob: cancelling a deferred job makes it cancelled and runs the hooks.
func TestCancelDeferredJob(t *testing.T) {
	d := openDB(t)
	m := newManager(t, d, Options{})
	hooks := newHookRecorder(m)
	a, _, err := m.Store().CreateJob(context.Background(), syncDest1)
	if err != nil {
		t.Fatal(err)
	}
	deferSQL(t, m.Store(), a.ID, time.Now().Add(time.Hour))
	m.Register(jobs.TypeSync, jobs.RunnerFunc(func(ctx context.Context, job jobs.Job, env jobs.Env) (jobs.Result, error) {
		return jobs.Result{}, errors.New("must not run")
	}))
	start(t, m)
	j, err := m.Cancel(context.Background(), a.ID)
	if err != nil || j.Status != jobs.StatusCancelled || !strings.Contains(j.Summary, "waited for its transfer window") || j.NotBefore != nil {
		t.Fatalf("Cancel = %+v, %v", j, err)
	}
	if got := receive(t, "hook", hooks.ch); got.ID != a.ID || got.Status != jobs.StatusCancelled {
		t.Fatalf("hook got %+v", got)
	}
}

// TestDedupeAndCoalescingSkipDeferredJobs: an identical request is never answered with a
// deferred job, and targeted requests never merge into one (it has a plan).
func TestDedupeAndCoalescingSkipDeferredJobs(t *testing.T) {
	ctx := context.Background()
	st := NewStore(openDB(t))
	future := time.Now().Add(time.Hour)

	a, created, err := st.CreateJob(ctx, syncDest1)
	if err != nil || !created {
		t.Fatal(err)
	}
	deferSQL(t, st, a.ID, future)
	b, created, err := st.CreateJob(ctx, syncDest1)
	if err != nil || !created || b.ID == a.ID {
		t.Fatalf("identical request returned the deferred job: %+v, created %v, %v", b, created, err)
	}

	targeted := func(p string) jobs.Spec {
		return jobs.Spec{Type: jobs.TypeSync, Trigger: jobs.TriggerWebhook, Params: jobs.Params{DestinationID: 7, SourceIDs: []int64{1}, Paths: []string{p}}}
	}
	c, _, err := st.CreateJob(ctx, targeted("Heat (1995)"))
	if err != nil {
		t.Fatal(err)
	}
	deferSQL(t, st, c.ID, future)
	e, created, err := st.CreateJob(ctx, targeted("Ronin (1998)"))
	if err != nil || !created || e.ID == c.ID {
		t.Fatalf("targeted request merged into the deferred job: %+v, %v, %v", e, created, err)
	}
	if got, _ := st.GetJob(ctx, c.ID); len(got.Params.Paths) != 1 {
		t.Fatalf("deferred job's paths changed: %v", got.Params.Paths)
	}
	// A deferred untargeted job does not cover a targeted request either.
	u, _, _ := st.CreateJob(ctx, jobs.Spec{Type: jobs.TypeSync, Params: jobs.Params{DestinationID: 8}})
	deferSQL(t, st, u.ID, future)
	g, created, err := st.CreateJob(ctx, jobs.Spec{Type: jobs.TypeSync, Params: jobs.Params{DestinationID: 8, SourceIDs: []int64{1}, Paths: []string{"x"}}})
	if err != nil || !created || g.ID == u.ID {
		t.Fatalf("a deferred untargeted job covered a targeted request: %+v, %v, %v", g, created, err)
	}
}

// TestDispatchSkipsNotBefore: a queued job whose not_before is in the future is not started;
// start-up recovery leaves it queued.
func TestDispatchSkipsNotBefore(t *testing.T) {
	d := openDB(t)
	st := NewStore(d)
	a, _, _ := st.CreateJob(context.Background(), syncDest1)
	deferSQL(t, st, a.ID, time.Now().Add(time.Hour))
	m := newManager(t, d, Options{})
	var ran atomic.Int32
	m.Register(jobs.TypeSync, jobs.RunnerFunc(func(ctx context.Context, job jobs.Job, env jobs.Env) (jobs.Result, error) {
		ran.Add(1)
		return jobs.Result{}, nil
	}))
	start(t, m)
	b := enqueue(t, m, syncDest2)
	waitStatus(t, m.Store(), b.ID, jobs.StatusCompleted)
	time.Sleep(50 * time.Millisecond)
	if j, _ := m.Store().GetJob(context.Background(), a.ID); j.Status != jobs.StatusQueued || ran.Load() != 1 {
		t.Fatalf("deferred job = %+v, runs %d", j, ran.Load())
	}
}

// TestSchedulerSupersedesDeferredSync: a fire of the destination's sync schedule cancels the
// deferred sync ("superseded by the scheduled run of <time>", hooks run, no failure) and queues
// a new job that plans again; a deferred sync the fire does not cover stays.
func TestSchedulerSupersedesDeferredSync(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	m := New(d, nil, Options{}) // not started: jobs stay queued
	hooks := newHookRecorder(m)
	st := m.Store()
	sched, err := st.UpsertSchedule(ctx, jobs.TypeSync, jobs.Params{DestinationID: 3}, "0 1 * * *", true)
	if err != nil {
		t.Fatal(err)
	}
	old, _, _ := st.CreateJob(ctx, jobs.Spec{Type: jobs.TypeSync, Trigger: jobs.TriggerSchedule, Params: jobs.Params{DestinationID: 3}})
	if err := st.AddItems(ctx, old.ID, []jobs.Item{{RelPath: "a", Action: jobs.ActionCopy, Status: jobs.ItemDone}}, true); err != nil {
		t.Fatal(err)
	}
	deferSQL(t, st, old.ID, at(1, 0, 0).Add(24*time.Hour))
	// A deferred release sync is not covered by a plain scheduled sync: it stays.
	rel, _, _ := st.CreateJob(ctx, jobs.Spec{Type: jobs.TypeSync, Params: jobs.Params{DestinationID: 3, ReleaseDemoted: true, ReleaseOf: 4, ReleaseRevision: 2}})
	deferSQL(t, st, rel.ID, at(1, 0, 0).Add(24*time.Hour))

	clk := &fakeClock{now: at(0, 59, 0)}
	s := newTestScheduler(t, st, m, clk)
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	clk.Set(at(1, 0, 0))
	got := receive(t, "superseded hook", hooks.ch)
	if got.ID != old.ID || got.Status != jobs.StatusCancelled || got.Error != "" ||
		!strings.Contains(got.Summary, "Superseded by the scheduled run of 2026-09-24 01:00") {
		t.Fatalf("superseded job = %+v", got)
	}
	if !strings.Contains(logText(t, st, old.ID), "Superseded by the scheduled run of 2026-09-24 01:00") {
		t.Fatalf("log = %s", logText(t, st, old.ID))
	}
	page, err := st.ListJobs(ctx, JobQuery{State: StateActive, Type: jobs.TypeSync})
	if err != nil {
		t.Fatal(err)
	}
	var fresh *jobs.Job
	for i, j := range page.Records {
		if j.ID != old.ID && j.ID != rel.ID {
			fresh = &page.Records[i]
		}
	}
	if fresh == nil || fresh.Deferrals != 0 || fresh.Trigger != jobs.TriggerSchedule || len(page.Records) != 2 {
		t.Fatalf("active syncs = %+v", page.Records)
	}
	if planned, _ := st.Planned(ctx, fresh.ID); planned {
		t.Fatal("the new job has a plan; it must plan again")
	}
	if j, _ := st.GetJob(ctx, rel.ID); j.Status != jobs.StatusQueued {
		t.Fatalf("release sync = %+v", j)
	}
	if _, ok := s.LastSkip(sched.ID); ok {
		t.Fatal("unexpected skip")
	}
}

// TestSupersedeCarriesWindowCuts: the plan of a sync that superseded deferred syncs (also one
// that was itself superseded before it planned) gets the window cuts of the same file (path,
// source, size) on their pending items, so the "cut in two consecutive windows" backstop still
// fails a file that never fits a window (phase4.md §9.2). A changed file, another source, a
// finished item and a new file get none.
func TestSupersedeCarriesWindowCuts(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	m := New(d, nil, Options{}) // not started: jobs stay queued
	hooks := newHookRecorder(m)
	st := m.Store()
	if _, err := st.UpsertSchedule(ctx, jobs.TypeSync, jobs.Params{DestinationID: 3}, "0 1 * * *", true); err != nil {
		t.Fatal(err)
	}
	cut := func(source, size int64, cuts int) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"sourceId":%d,"size":%d,"windowCuts":%d,"reason":"new"}`, source, size, cuts))
	}
	// Day 1: j1 ran in the window, big.mkv was cut once and j1 deferred.
	j1, _, _ := st.CreateJob(ctx, jobs.Spec{Type: jobs.TypeSync, Trigger: jobs.TriggerSchedule, Params: jobs.Params{DestinationID: 3}})
	if err := st.AddItems(ctx, j1.ID, []jobs.Item{
		{RelPath: "done.mkv", Action: jobs.ActionCopy, Status: jobs.ItemDone, Detail: cut(7, 10, 1)},
		{RelPath: "big.mkv", Action: jobs.ActionCopy, Detail: cut(7, 100, 1)},
		{RelPath: "changed.mkv", Action: jobs.ActionCopy, Detail: cut(7, 50, 1)},
		{RelPath: "other.mkv", Action: jobs.ActionCopy, Detail: cut(8, 60, 1)},
	}, true); err != nil {
		t.Fatal(err)
	}
	deferSQL(t, st, j1.ID, at(1, 0, 0).Add(24*time.Hour))

	clk := &fakeClock{now: at(0, 59, 0)}
	s := newTestScheduler(t, st, m, clk)
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	// Day 2 (the fire): j2 supersedes j1, then waits for the window without a plan.
	clk.Set(at(1, 0, 0))
	if got := receive(t, "j1 superseded", hooks.ch); got.ID != j1.ID || got.Status != jobs.StatusCancelled {
		t.Fatalf("superseded job = %+v", got)
	}
	active := func() jobs.Job {
		t.Helper()
		page, err := st.ListJobs(ctx, JobQuery{State: StateActive, Type: jobs.TypeSync})
		if err != nil || len(page.Records) != 1 {
			t.Fatalf("active syncs = %+v, %v", page.Records, err)
		}
		return page.Records[0]
	}
	j2 := active()
	deferSQL(t, st, j2.ID, at(1, 0, 0).Add(48*time.Hour))
	// Day 3: j3 supersedes j2 and plans; big.mkv keeps its cut, so the next cut fails it.
	clk.Set(at(1, 0, 0).Add(24 * time.Hour))
	if got := receive(t, "j2 superseded", hooks.ch); got.ID != j2.ID || got.Status != jobs.StatusCancelled {
		t.Fatalf("superseded job = %+v", got)
	}
	j3 := active()
	if j3.ID == j2.ID || j3.ID == j1.ID {
		t.Fatalf("no new job: %+v", j3)
	}
	plain := func(source, size int64) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"sourceId":%d,"size":%d,"reason":"new"}`, source, size))
	}
	if err := st.AddItems(ctx, j3.ID, []jobs.Item{
		{RelPath: "done.mkv", Action: jobs.ActionCopy, Detail: plain(7, 10)},
		{RelPath: "big.mkv", Action: jobs.ActionCopy, Detail: plain(7, 100)},
		{RelPath: "changed.mkv", Action: jobs.ActionCopy, Detail: plain(7, 51)},
	}, false); err != nil {
		t.Fatal(err)
	}
	if err := st.AddItems(ctx, j3.ID, []jobs.Item{
		{RelPath: "other.mkv", Action: jobs.ActionCopy, Detail: plain(7, 60)},
		{RelPath: "new.mkv", Action: jobs.ActionCopy, Detail: plain(7, 1)},
	}, true); err != nil {
		t.Fatal(err)
	}
	items, err := st.Pending(ctx, j3.ID, 0, 0)
	if err != nil || len(items) != 5 {
		t.Fatalf("items = %+v, %v", items, err)
	}
	for _, it := range items {
		var f struct {
			SourceID   int64  `json:"sourceId"`
			Size       int64  `json:"size"`
			Reason     string `json:"reason"`
			WindowCuts int    `json:"windowCuts"`
		}
		if err := json.Unmarshal(it.Detail, &f); err != nil {
			t.Fatalf("%s: %v", it.RelPath, err)
		}
		want := 0
		if it.RelPath == "big.mkv" {
			want = 1
		}
		if f.WindowCuts != want || f.SourceID != 7 || f.Reason != "new" {
			t.Errorf("%s: detail %s, want %d window cuts and the rest kept", it.RelPath, it.Detail, want)
		}
	}
	// A sync that superseded nothing carries nothing.
	other, _, _ := st.CreateJob(ctx, jobs.Spec{Type: jobs.TypeSync, Params: jobs.Params{DestinationID: 3, SourceIDs: []int64{7}}})
	if err := st.AddItems(ctx, other.ID, []jobs.Item{{RelPath: "big.mkv", Action: jobs.ActionCopy, Detail: plain(7, 100)}}, true); err != nil {
		t.Fatal(err)
	}
	if items, _ := st.Pending(ctx, other.ID, 0, 0); len(items) != 1 || strings.Contains(string(items[0].Detail), "windowCuts") {
		t.Fatalf("unrelated job's items = %+v", items)
	}
}

// TestCarryCutsQueryUsesTheCutIndex: the carry, which runs for every plan page inside the write
// transaction, reads only the cut items of the superseded syncs through the partial index
// job_items_window_cuts; with job_items_job_status it read every pending item of the whole chain
// (a seed superseded daily for weeks: about 1.2 s per 1000-item page at 20 x 50k pending items,
// holding the only write connection). An item detail that is not JSON still inserts (the index's
// json_valid term comes first).
func TestCarryCutsQueryUsesTheCutIndex(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	rows, err := d.Reader().QueryContext(ctx, `EXPLAIN QUERY PLAN `+carryCutsQuery, 5)
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	text := strings.Join(plan, "\n")
	if !strings.Contains(text, "SEARCH i USING INDEX job_items_window_cuts (job_id=? AND status=? AND <expr>>?)") ||
		!strings.Contains(text, "jobs_superseded_by") {
		t.Fatalf("carry plan:\n%s", text)
	}

	st := NewStore(d)
	j, _, err := st.CreateJob(ctx, jobs.Spec{Type: jobs.TypeSync, Params: jobs.Params{DestinationID: 3}})
	if err != nil {
		t.Fatal(err)
	}
	execSQL(t, d, `INSERT INTO job_items (job_id, rel_path, action, status, bytes, detail) VALUES (?, 'x', 'copy', 'pending', 0, 'not json')`, j.ID)
	execSQL(t, d, `INSERT INTO job_items (job_id, rel_path, action, status, bytes, detail) VALUES (?, 'y', 'copy', 'pending', 0, '{"windowCuts":1}')`, j.ID)
	var n int
	if err := d.Reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM job_items INDEXED BY job_items_window_cuts
		WHERE job_id = ? AND status = 'pending' AND json_valid(detail) AND json_extract(detail, '$.windowCuts') > 0`, j.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("cut items in the index = %d, %v; want 1", n, err)
	}
}

// TestSupersedeContinuesDeferralCount: a sync that superseded deferred syncs continues their
// deferral count, so the "seed needs more windows than two weeks" warning (at 14, from the count
// the runner started with plus one) still comes for a seed that a daily schedule supersedes every
// day. Scenario: daily fire at 00:00, window 01:00-07:00; each job defers at the fire (the wait its
// predecessor already counted: no increase) and at the window's end (+1). Jobs of other types and
// a sync that superseded nothing count as before.
func TestSupersedeContinuesDeferralCount(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	st := NewStore(d)
	p := jobs.Params{DestinationID: 3}
	// run starts job id as the dispatcher does and returns the count the runner starts with.
	run := func(id int64) int {
		t.Helper()
		j, ok, err := st.markRunning(ctx, id, at(1, 0, 0))
		if err != nil || !ok {
			t.Fatalf("start job %d: %v, %v", id, ok, err)
		}
		return j.Deferrals
	}
	deferred := func(id int64) int {
		t.Helper()
		if ok, err := st.deferJob(ctx, id, at(1, 0, 0).Add(24*time.Hour), "waiting", nil); err != nil || !ok {
			t.Fatalf("defer job %d: %v, %v", id, ok, err)
		}
		j, err := st.GetJob(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return j.Deferrals
	}
	j1, _, err := st.CreateJob(ctx, jobs.Spec{Type: jobs.TypeSync, Trigger: jobs.TriggerSchedule, Params: p})
	if err != nil {
		t.Fatal(err)
	}
	run(j1.ID)
	if n := deferred(j1.ID); n != 1 { // day 1, 00:00: outside the window
		t.Fatalf("j1 first deferral = %d, want 1", n)
	}
	run(j1.ID)
	if n := deferred(j1.ID); n != 2 { // day 1, 07:00: the window's end
		t.Fatalf("j1 second deferral = %d, want 2", n)
	}
	prev := j1.ID
	warnedAt := 0
	for day := 2; day <= 20; day++ {
		// The fire supersedes the deferred job; the new one defers at once (same wait), then
		// resumes in the window and defers at its end.
		j, _, err := st.CreateJob(ctx, jobs.Spec{Type: jobs.TypeSync, Trigger: jobs.TriggerSchedule, Params: p})
		if err != nil || j.ID == prev {
			t.Fatalf("day %d: job %+v, %v", day, j, err)
		}
		if got, err := st.supersedeDeferredSyncs(ctx, p, j.ID, "Superseded", at(0, 0, 0)); err != nil || len(got) != 1 || got[0].ID != prev {
			t.Fatalf("day %d: superseded %+v, %v", day, got, err)
		}
		if start := run(j.ID); start != 0 {
			t.Fatalf("day %d: a fresh job starts at %d", day, start)
		}
		if n := deferred(j.ID); n != day {
			t.Fatalf("day %d: deferral at the fire = %d, want %d (the predecessor's)", day, n, day)
		}
		start := run(j.ID)
		n := deferred(j.ID)
		if n != start+1 || n != day+1 {
			t.Fatalf("day %d: deferral at the window's end = %d (started at %d), want %d", day, n, start, day+1)
		}
		if warnedAt == 0 && (start+1)%14 == 0 { // the runner's longSeedDeferrals check
			warnedAt = day
		}
		prev = j.ID
	}
	if warnedAt != 13 {
		t.Fatalf("the two-week warning came on day %d, want 13", warnedAt)
	}

	// A sync that superseded nothing and a verify count their own deferrals only.
	for _, spec := range []jobs.Spec{{Type: jobs.TypeSync, Params: jobs.Params{DestinationID: 4}}, {Type: jobs.TypeVerify, Params: p}} {
		j, _, err := st.CreateJob(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		run(j.ID)
		if n := deferred(j.ID); n != 1 {
			t.Fatalf("%s job: deferrals = %d, want 1", spec.Type, n)
		}
	}
}

// TestSchedulerSkipsVerifyAndRetentionWhileDeferred: a fire of verify or retention of a
// destination whose job of that type is deferred queues nothing, and LastSkip says why.
func TestSchedulerSkipsVerifyAndRetentionWhileDeferred(t *testing.T) {
	ctx := context.Background()
	st := NewStore(openDB(t))
	v, _ := st.UpsertSchedule(ctx, jobs.TypeVerify, jobs.Params{DestinationID: 3}, "0 2 * * *", true)
	r, _ := st.UpsertSchedule(ctx, jobs.TypeRetention, jobs.Params{DestinationID: 3}, "0 2 * * *", true)
	r4, _ := st.UpsertSchedule(ctx, jobs.TypeRetention, jobs.Params{DestinationID: 4}, "0 2 * * *", true)
	for _, typ := range []jobs.Type{jobs.TypeVerify, jobs.TypeRetention} {
		j, _, _ := st.CreateJob(ctx, jobs.Spec{Type: typ, Params: jobs.Params{DestinationID: 3}})
		deferSQL(t, st, j.ID, at(1, 0, 0).Add(24*time.Hour))
	}
	clk := &fakeClock{now: at(1, 59, 0)}
	enq := newFakeEnqueuer()
	s := newTestScheduler(t, st, enq, clk)
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	clk.Set(at(2, 0, 0))
	if spec := receive(t, "retention of dest 4", enq.ch); spec.Params.DestinationID != 4 {
		t.Fatalf("spec = %+v", spec)
	}
	s.Stop()
	if n := enq.count(); n != 1 {
		t.Fatalf("enqueued %d; want only the retention of destination 4", n)
	}
	for _, id := range []int64{v.ID, r.ID} {
		sk, ok := s.LastSkip(id)
		if !ok || sk.Reason != SkipDeferred || !sk.At.Equal(at(2, 0, 0)) {
			t.Fatalf("LastSkip(%d) = %+v, %v", id, sk, ok)
		}
	}
	if _, ok := s.LastSkip(r4.ID); ok {
		t.Fatal("a queued run recorded a skip")
	}
}

// TestSlots: with 2 upload slots a third engine sync waits, holding no worker, and starts when a
// slot is freed; a filecopy sync (no slot) runs meanwhile (phase4.md §9.3).
func TestSlots(t *testing.T) {
	d := openDB(t)
	engine := map[int64]bool{1: true, 2: true, 4: true}
	var needs atomic.Int32
	m := newManager(t, d, Options{Workers: 4, Slots: []SlotPool{{Name: "upload", Limit: 2, Needs: func(ctx context.Context, j jobs.Job) (bool, error) {
		needs.Add(1)
		return j.Type == jobs.TypeSync && !j.DryRun && engine[j.Params.DestinationID], nil
	}}}})
	g := newGatedRunner(false)
	registerAll(m, g)
	start(t, m)
	var ids []int64
	for _, dest := range []int64{1, 2, 4} {
		ids = append(ids, enqueue(t, m, jobs.Spec{Type: jobs.TypeSync, Params: jobs.Params{DestinationID: dest}}).ID)
	}
	first := map[int64]bool{}
	for range 2 {
		first[receive(t, "engine sync", g.started).Params.DestinationID] = true
	}
	filecopy := enqueue(t, m, jobs.Spec{Type: jobs.TypeSync, Params: jobs.Params{DestinationID: 3}})
	if got := receive(t, "filecopy sync", g.started); got.ID != filecopy.ID {
		t.Fatalf("started %+v; want the filecopy sync", got)
	}
	if j, _ := m.Store().GetJob(context.Background(), ids[2]); j.Status != jobs.StatusQueued || !first[1] || !first[2] {
		t.Fatalf("third engine sync = %+v (first %v)", j, first)
	}
	// Two of the three running jobs end, so at least one engine sync frees its slot.
	g.release <- struct{}{}
	g.release <- struct{}{}
	third := receive(t, "third engine sync", g.started)
	if third.ID != ids[2] {
		t.Fatalf("started %+v", third)
	}
	close(g.release)
	if needs.Load() == 0 {
		t.Fatal("Needs was never asked")
	}
}

func TestPruneAndReadDataValidation(t *testing.T) {
	st := NewStore(openDB(t))
	ctx := context.Background()
	bad := []jobs.Spec{
		{Type: jobs.TypeRetention, Params: jobs.Params{Prune: true}},
		{Type: jobs.TypeVerify, Params: jobs.Params{DestinationID: 1, Prune: true}},
		{Type: jobs.TypeSync, Params: jobs.Params{DestinationID: 1, Prune: true}},
		{Type: jobs.TypeSync, Params: jobs.Params{DestinationID: 1, ReadData: true}},
		{Type: jobs.TypeRetention, Params: jobs.Params{DestinationID: 1, ReadData: true}},
		{Type: jobs.TypeManifestExport, Params: jobs.Params{DestinationID: 1, ReadData: true}},
	}
	for _, spec := range bad {
		if _, _, err := st.CreateJob(ctx, spec); !isValidation(err) {
			t.Errorf("CreateJob(%+v) = %v; want a ValidationError", spec, err)
		}
	}
	for _, spec := range []jobs.Spec{
		{Type: jobs.TypeRetention, Params: jobs.Params{DestinationID: 1, Prune: true}},
		{Type: jobs.TypeVerify, Params: jobs.Params{DestinationID: 1, ReadData: true}},
	} {
		if _, _, err := st.CreateJob(ctx, spec); err != nil {
			t.Errorf("CreateJob(%+v) = %v", spec, err)
		}
	}
}

// TestHoldKeys: a key a running job holds is busy; a held key keeps jobs that need it queued
// until it is released.
func TestHoldKeys(t *testing.T) {
	m := newManager(t, openDB(t), Options{Workers: 4})
	g := newGatedRunner(false)
	registerAll(m, g)
	start(t, m)
	a := enqueue(t, m, syncDest1)
	receive(t, "sync of dest 1", g.started)
	if _, err := m.HoldKeys(context.Background(), "dest:1"); !errors.Is(err, ErrBusy) {
		t.Fatalf("HoldKeys(dest:1) = %v; want ErrBusy", err)
	}
	release, err := m.HoldKeys(context.Background(), "dest:2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.HoldKeys(context.Background(), "dest:2"); !errors.Is(err, ErrBusy) {
		t.Fatalf("second hold = %v; want ErrBusy", err)
	}
	b := enqueue(t, m, syncDest2)
	time.Sleep(50 * time.Millisecond)
	if j, _ := m.Store().GetJob(context.Background(), b.ID); j.Status != jobs.StatusQueued {
		t.Fatalf("job started while its key was held: %+v", j)
	}
	release()
	release()
	if got := receive(t, "sync of dest 2", g.started); got.ID != b.ID {
		t.Fatalf("started %+v", got)
	}
	close(g.release)
	waitStatus(t, m.Store(), a.ID, jobs.StatusCompleted)
	waitStatus(t, m.Store(), b.ID, jobs.StatusCompleted)
}

// TestActiveForDestinationAnyType: a running job of any destination type (a Plex DB backup
// included) or a deferred one is active; a queued one that never started is not.
func TestActiveForDestinationAnyType(t *testing.T) {
	ctx := context.Background()
	st := NewStore(openDB(t))
	active := func(id int64) bool {
		t.Helper()
		ok, err := st.ActiveForDestinationAnyType(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	p, _, _ := st.CreateJob(ctx, jobs.Spec{Type: jobs.TypePlexDBBackup, Params: jobs.Params{IntegrationID: 1, DestinationID: 5}})
	if active(5) {
		t.Fatal("a queued Plex DB backup counts as active")
	}
	if _, ok, err := st.markRunning(ctx, p.ID, time.Now()); err != nil || !ok {
		t.Fatal(err)
	}
	if !active(5) || active(6) {
		t.Fatal("a running Plex DB backup is not active for its destination (or is for another)")
	}
	s, _, _ := st.CreateJob(ctx, jobs.Spec{Type: jobs.TypeSync, Params: jobs.Params{DestinationID: 6}})
	deferSQL(t, st, s.ID, time.Now().Add(time.Hour))
	if !active(6) {
		t.Fatal("a deferred sync is not active")
	}
	// A refresh (no destination) never counts.
	r, _, _ := st.CreateJob(ctx, jobs.Spec{Type: jobs.TypeRefresh, Params: jobs.Params{IntegrationID: 7}})
	st.markRunning(ctx, r.ID, time.Now())
	if active(7) {
		t.Fatal("a refresh counts as a job of destination 7")
	}
}

// TestSyncSchedules: the schedules of a type matching an integration become exactly the wanted
// set; kept rows keep their id and last_run_at; other integrations' schedules are untouched.
func TestSyncSchedules(t *testing.T) {
	ctx := context.Background()
	st := NewStore(openDB(t))
	params := func(integ, dest int64) jobs.Params { return jobs.Params{IntegrationID: integ, DestinationID: dest} }
	match := jobs.Params{IntegrationID: 1}
	first, err := st.SyncSchedules(ctx, jobs.TypePlexDBBackup, match, []ScheduleSpec{
		{Params: params(1, 10), Cron: "0 3 * * *", Enabled: true},
		{Params: params(1, 11), Cron: "0 4 * * 0", Enabled: true},
		{Params: params(1, 12), Cron: "0 5 * * *", Enabled: false},
	})
	if err != nil || len(first) != 3 {
		t.Fatalf("SyncSchedules = %+v, %v", first, err)
	}
	other, _ := st.UpsertSchedule(ctx, jobs.TypePlexDBBackup, params(2, 10), "0 3 * * *", true)
	arr, _ := st.UpsertSchedule(ctx, jobs.TypeArrBackup, params(1, 10), "0 3 * * *", true)
	ran := time.Date(2026, 9, 20, 3, 0, 0, 0, time.UTC)
	if err := st.MarkRun(ctx, first[0].ID, ran); err != nil {
		t.Fatal(err)
	}
	got, err := st.SyncSchedules(ctx, jobs.TypePlexDBBackup, match, []ScheduleSpec{
		{Params: params(1, 10), Cron: "0 3 * * *", Enabled: true},
		{Params: params(1, 11), Cron: "30 4 * * 0", Enabled: false},
	})
	if err != nil || len(got) != 2 {
		t.Fatalf("SyncSchedules = %+v, %v", got, err)
	}
	if got[0].ID != first[0].ID || got[0].LastRunAt == nil || !got[0].LastRunAt.Equal(ran) {
		t.Fatalf("kept schedule = %+v", got[0])
	}
	if got[1].ID != first[1].ID || got[1].Cron != "30 4 * * 0" || got[1].Enabled {
		t.Fatalf("changed schedule = %+v", got[1])
	}
	if _, err := st.GetSchedule(ctx, first[2].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("dropped target's schedule still exists: %v", err)
	}
	for _, id := range []int64{other.ID, arr.ID} {
		if _, err := st.GetSchedule(ctx, id); err != nil {
			t.Fatalf("schedule %d of another integration or type was touched: %v", id, err)
		}
	}
	for name, want := range map[string][]ScheduleSpec{
		"other integration": {{Params: params(2, 10), Cron: "0 3 * * *"}},
		"duplicate":         {{Params: params(1, 10), Cron: "0 3 * * *"}, {Params: params(1, 10), Cron: "0 4 * * *"}},
		"bad cron":          {{Params: params(1, 10), Cron: "nope"}},
	} {
		if _, err := st.SyncSchedules(ctx, jobs.TypePlexDBBackup, match, want); !isValidation(err) {
			t.Errorf("%s: err = %v; want a ValidationError", name, err)
		}
	}
	if _, err := st.SyncSchedules(ctx, jobs.TypePlexDBBackup, jobs.Params{}, nil); !isValidation(err) {
		t.Errorf("empty match: err = %v", err)
	}
	if got, err := st.SyncSchedules(ctx, jobs.TypePlexDBBackup, match, nil); err != nil || len(got) != 0 {
		t.Fatalf("clearing = %+v, %v", got, err)
	}
	if _, err := st.GetSchedule(ctx, first[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("clearing kept a schedule")
	}
}
