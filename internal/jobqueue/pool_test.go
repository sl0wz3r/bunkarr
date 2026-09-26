package jobqueue

import (
	"context"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// TestRefreshPool: refresh jobs run in their own pool of RefreshWorkers slots, not counted
// against Workers, so a long sync cannot starve them and they cannot starve the sync
// (phase2-3.md §7.3, §12.1). Refreshes of one integration still never overlap.
func TestRefreshPool(t *testing.T) {
	d := openDB(t)
	m := newManager(t, d, Options{Workers: 1})
	syncs, refreshes := newGatedRunner(false), newGatedRunner(false)
	m.Register(jobs.TypeSync, syncs)
	m.Register(jobs.TypeRefresh, refreshes)

	sync1 := enqueue(t, m, syncDest1)
	sync2 := enqueue(t, m, syncDest2) // waits: the one general worker is busy
	r1 := enqueue(t, m, jobs.Spec{Type: jobs.TypeRefresh, Params: jobs.Params{IntegrationID: 1}})
	r1b := enqueue(t, m, webhookRefresh(7)) // integration 1 again: waits for integration:1
	r2 := enqueue(t, m, jobs.Spec{Type: jobs.TypeRefresh, Params: jobs.Params{IntegrationID: 2}})
	r3 := enqueue(t, m, jobs.Spec{Type: jobs.TypeRefresh, Params: jobs.Params{IntegrationID: 3}}) // waits: the pool has 2 slots
	start(t, m)

	if j := receive(t, "sync start", syncs.started); j.ID != sync1.ID {
		t.Fatalf("sync %d started first; want %d", j.ID, sync1.ID)
	}
	got := map[int64]bool{}
	got[receive(t, "refresh start", refreshes.started).ID] = true
	got[receive(t, "refresh start", refreshes.started).ID] = true
	if !got[r1.ID] || !got[r2.ID] {
		t.Fatalf("refreshes started %v; want %d and %d while the sync runs", got, r1.ID, r2.ID)
	}
	select {
	case j := <-refreshes.started:
		t.Fatalf("a third refresh (%d) started with 2 refresh slots", j.ID)
	case j := <-syncs.started:
		t.Fatalf("a second sync (%d) started with 1 worker", j.ID)
	case <-time.After(150 * time.Millisecond):
	}

	close(refreshes.release)
	for _, id := range []int64{r1.ID, r1b.ID, r2.ID, r3.ID} {
		waitStatus(t, m.Store(), id, jobs.StatusCompleted)
	}
	// The refreshes finished while the general worker was still busy with the first sync.
	if j, _ := m.Get(context.Background(), sync2.ID); j.Status != jobs.StatusQueued {
		t.Fatalf("sync %d is %s; want queued behind the running sync", sync2.ID, j.Status)
	}
	close(syncs.release)
	waitStatus(t, m.Store(), sync1.ID, jobs.StatusCompleted)
	waitStatus(t, m.Store(), sync2.ID, jobs.StatusCompleted)

	refreshes.mu.Lock()
	defer refreshes.mu.Unlock()
	if refreshes.maxRun != 2 {
		t.Fatalf("at most %d refreshes ran at once; want 2", refreshes.maxRun)
	}
	if len(refreshes.overlap) != 0 {
		t.Fatalf("refreshes of one integration overlapped: %v", refreshes.overlap)
	}
}

// TestRefreshPoolSize: Options.RefreshWorkers sets the pool; SetWorkers does not change it.
func TestRefreshPoolSize(t *testing.T) {
	d := openDB(t)
	m := newManager(t, d, Options{Workers: 1, RefreshWorkers: 1})
	g := newGatedRunner(false)
	m.Register(jobs.TypeRefresh, g)
	m.SetWorkers(4)
	a := enqueue(t, m, jobs.Spec{Type: jobs.TypeRefresh, Params: jobs.Params{IntegrationID: 1}})
	b := enqueue(t, m, jobs.Spec{Type: jobs.TypeRefresh, Params: jobs.Params{IntegrationID: 2}})
	start(t, m)
	if j := receive(t, "refresh start", g.started); j.ID != a.ID {
		t.Fatalf("started %d; want %d", j.ID, a.ID)
	}
	select {
	case j := <-g.started:
		t.Fatalf("a second refresh (%d) started with 1 refresh slot", j.ID)
	case <-time.After(150 * time.Millisecond):
	}
	close(g.release)
	waitStatus(t, m.Store(), b.ID, jobs.StatusCompleted)
}

// TestSchedulerDoesNotSkipFullSyncForRunningTargetedSync: the amended skip rule (phase2-3.md
// §12.1). A running targeted sync never makes a scheduled full sync skip its turn; the full sync
// is queued and waits for dest:<id>. A running full sync still skips it. For refreshes, a running
// targeted refresh never skips a scheduled full one.
func TestSchedulerDoesNotSkipFullSyncForRunningTargetedSync(t *testing.T) {
	ctx := context.Background()
	st := NewStore(openDB(t))
	run := func(spec jobs.Spec) {
		t.Helper()
		j, _, err := st.CreateJob(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok, err := st.markRunning(ctx, j.ID, time.Now()); !ok || err != nil {
			t.Fatalf("markRunning: %v %v", ok, err)
		}
	}
	run(jobs.Spec{Type: jobs.TypeSync, Trigger: jobs.TriggerWebhook, Params: jobs.Params{DestinationID: 3, SourceIDs: []int64{1}, Paths: []string{"Heat (1995)"}}})
	run(jobs.Spec{Type: jobs.TypeSync, Params: jobs.Params{DestinationID: 4, SourceIDs: []int64{1}}})
	run(webhookRefresh(7))
	run(jobs.Spec{Type: jobs.TypeRefresh, Params: jobs.Params{IntegrationID: 2}})
	for _, tc := range []struct {
		typ  jobs.Type
		p    jobs.Params
		skip bool
	}{
		{jobs.TypeSync, jobs.Params{DestinationID: 3}, false},                        // only a targeted sync runs
		{jobs.TypeVerify, jobs.Params{DestinationID: 3}, false},                      // no verify runs
		{jobs.TypeSync, jobs.Params{DestinationID: 4}, false},                        // only a sync of source 1 runs
		{jobs.TypeSync, jobs.Params{DestinationID: 4, SourceIDs: []int64{1}}, true},  // a full sync of source 1 runs
		{jobs.TypeRefresh, jobs.Params{IntegrationID: 1}, false},                     // only a targeted refresh runs
		{jobs.TypeRefresh, jobs.Params{IntegrationID: 2}, true},                      // the same full refresh runs
		{jobs.TypeManifestExport, jobs.Params{DestinationID: 3}, false},              // nothing of it runs
		{jobs.TypeArrBackup, jobs.Params{IntegrationID: 1, DestinationID: 3}, false}, // nothing of it runs
	} {
		if got, err := st.hasRunning(ctx, tc.typ, tc.p); err != nil || got != tc.skip {
			t.Errorf("hasRunning(%s %+v) = %v, %v; want %v", tc.typ, tc.p, got, err, tc.skip)
		}
	}

	// With the scheduler and a manager: the scheduled full sync is queued while the targeted one
	// runs, waits for dest:1, then runs.
	d := openDB(t)
	m := newManager(t, d, Options{})
	g := newGatedRunner(false)
	m.Register(jobs.TypeSync, g)
	start(t, m)
	targeted := enqueue(t, m, targetedSync(1, "Heat (1995)"))
	if j := receive(t, "targeted sync start", g.started); j.ID != targeted.ID {
		t.Fatalf("started %d; want %d", j.ID, targeted.ID)
	}
	sc, err := m.Store().UpsertSchedule(ctx, jobs.TypeSync, jobs.Params{DestinationID: 1}, "0 2 * * *", true)
	if err != nil {
		t.Fatal(err)
	}
	clk := &fakeClock{now: at(1, 59, 0)}
	s := NewScheduler(m.Store(), m, nil, time.UTC)
	s.clk = clk
	t.Cleanup(s.Stop)
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	clk.Set(at(2, 0, 0))
	waitFor(t, "the scheduled run recorded", func() bool {
		got, _ := m.Store().GetSchedule(ctx, sc.ID)
		return got.LastRunAt != nil
	})
	page, _ := m.Store().ListJobs(ctx, JobQuery{Type: jobs.TypeSync, Status: jobs.StatusQueued})
	if page.TotalRecords != 1 || page.Records[0].Trigger != jobs.TriggerSchedule || page.Records[0].Params.Targeted() {
		t.Fatalf("queued syncs = %+v; want the scheduled full sync", page.Records)
	}
	full := page.Records[0]
	select {
	case j := <-g.started:
		t.Fatalf("job %d started while the targeted sync holds dest:1", j.ID)
	case <-time.After(100 * time.Millisecond):
	}
	close(g.release)
	waitStatus(t, m.Store(), full.ID, jobs.StatusCompleted)
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.overlap) != 0 {
		t.Fatalf("syncs of one destination overlapped: %v", g.overlap)
	}
}
