package jobqueue

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// coalesceStore returns a store whose clock advances one second per call, so every job has its
// own queued_at.
func coalesceStore(t *testing.T) (*db.DB, *Store) {
	t.Helper()
	d := openDB(t)
	st := NewStore(d)
	clk := &manualClock{t: time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)}
	st.now = func() time.Time { clk.Add(time.Second); return clk.Now() }
	return d, st
}

// mustCreate runs createJob and fails the test on an error.
func mustCreate(t *testing.T, st *Store, spec jobs.Spec) (jobs.Job, enqueueOutcome) {
	t.Helper()
	j, out, err := st.createJob(context.Background(), spec)
	if err != nil {
		t.Fatalf("createJob(%s %+v): %v", spec.Type, spec.Params, err)
	}
	return j, out
}

// targetedSync is a webhook sync of destination 1, source src, with paths.
func targetedSync(src int64, paths ...string) jobs.Spec {
	return jobs.Spec{Type: jobs.TypeSync, Trigger: jobs.TriggerWebhook, Params: jobs.Params{DestinationID: 1, SourceIDs: []int64{src}, Paths: paths}}
}

// webhookRefresh is a webhook refresh of integration 1 with syncAfter.
func webhookRefresh(ids ...int64) jobs.Spec {
	return jobs.Spec{Type: jobs.TypeRefresh, Trigger: jobs.TriggerWebhook, Params: jobs.Params{IntegrationID: 1, ArrItemIDs: ids, SyncAfter: true}}
}

// jobCount returns how many jobs exist.
func jobCount(t *testing.T, st *Store) int64 {
	t.Helper()
	page, err := st.ListJobs(context.Background(), JobQuery{})
	if err != nil {
		t.Fatal(err)
	}
	return page.TotalRecords
}

// TestCoalesceCoveredByQueuedUntargetedJob covers the first three rows of phase2-3.md §12.2.
func TestCoalesceCoveredByQueuedUntargetedJob(t *testing.T) {
	t.Run("sync", func(t *testing.T) {
		_, st := coalesceStore(t)
		full, _ := mustCreate(t, st, jobs.Spec{Type: jobs.TypeSync, Trigger: jobs.TriggerSchedule, Params: jobs.Params{DestinationID: 1}})
		j, out := mustCreate(t, st, targetedSync(3, "Heat (1995)"))
		if out != outcomeCovered || j.ID != full.ID || !j.QueuedAt.Equal(full.QueuedAt) || len(j.Params.Paths) != 0 {
			t.Fatalf("targeted sync with a queued full sync: %s %+v", out, j)
		}
		if _, created, _ := st.CreateJob(context.Background(), targetedSync(3, "Other")); created {
			t.Fatal("CreateJob reported a covered spec as created")
		}

		// A queued untargeted sync of some sources covers only those.
		_, st = coalesceStore(t)
		some, _ := mustCreate(t, st, jobs.Spec{Type: jobs.TypeSync, Params: jobs.Params{DestinationID: 1, SourceIDs: []int64{3, 4}}})
		if j, out := mustCreate(t, st, targetedSync(4, "x")); out != outcomeCovered || j.ID != some.ID {
			t.Fatalf("source 4 of [3 4]: %s %+v", out, j)
		}
		if j, out := mustCreate(t, st, targetedSync(5, "x")); out != outcomeCreated || j.ID == some.ID {
			t.Fatalf("source 5 of [3 4]: %s %+v", out, j)
		}
		// Another destination's full sync covers nothing here.
		mustCreate(t, st, jobs.Spec{Type: jobs.TypeSync, Params: jobs.Params{DestinationID: 2}})
		if _, out := mustCreate(t, st, jobs.Spec{Type: jobs.TypeSync, Params: jobs.Params{DestinationID: 3, SourceIDs: []int64{1}, Paths: []string{"x"}}}); out != outcomeCreated {
			t.Fatalf("destination 3: %s", out)
		}
		// A request to apply held changes is not answered with a job that would hold them again.
		allow := targetedSync(3, "x")
		allow.Params.AllowChanges = true
		if j, out := mustCreate(t, st, allow); out != outcomeCreated || j.ID == some.ID || !j.Params.AllowChanges {
			t.Fatalf("allowChanges under a queued sync without it: %s %+v", out, j)
		}
		// A queued dry run covers nothing, and a dry run is never coalesced.
		_, st = coalesceStore(t)
		dry, _ := mustCreate(t, st, jobs.Spec{Type: jobs.TypeSync, DryRun: true, Params: jobs.Params{DestinationID: 1}})
		if j, out := mustCreate(t, st, targetedSync(3, "x")); out != outcomeCreated || j.ID == dry.ID {
			t.Fatalf("under a queued dry run: %s %+v", out, j)
		}
		dryTargeted := targetedSync(3, "y")
		dryTargeted.DryRun = true
		if j, out := mustCreate(t, st, dryTargeted); out != outcomeCreated || !reflect.DeepEqual(j.Params.Paths, []string{"y"}) {
			t.Fatalf("a targeted dry run: %s %+v", out, j)
		}
	})
	t.Run("refresh without syncAfter", func(t *testing.T) {
		_, st := coalesceStore(t)
		full, _ := mustCreate(t, st, jobs.Spec{Type: jobs.TypeRefresh, Trigger: jobs.TriggerSchedule, Params: jobs.Params{IntegrationID: 1}})
		j, out := mustCreate(t, st, jobs.Spec{Type: jobs.TypeRefresh, Params: jobs.Params{IntegrationID: 1, ArrItemIDs: []int64{7}}})
		if out != outcomeCovered || j.ID != full.ID {
			t.Fatalf("targeted refresh with a queued full refresh: %s %+v", out, j)
		}
		// Another integration's full refresh covers nothing here.
		if _, out := mustCreate(t, st, jobs.Spec{Type: jobs.TypeRefresh, Params: jobs.Params{IntegrationID: 2, ArrItemIDs: []int64{7}}}); out != outcomeCreated {
			t.Fatalf("integration 2: %s", out)
		}
	})
	t.Run("refresh with syncAfter", func(t *testing.T) {
		d, st := coalesceStore(t)
		// Only an overflow merge makes an untargeted refresh with syncAfter; insert one directly.
		execSQL(t, d, `INSERT INTO jobs (id, type, status, trigger, params, integration_id, queued_at)
			VALUES (40, 'refresh', 'queued', 'webhook', '{"integrationId":1,"syncAfter":true}', 1, '2026-09-25T09:00:00.000000000Z')`)
		j, out := mustCreate(t, st, webhookRefresh(7))
		if out != outcomeCovered || j.ID != 40 || !j.Params.SyncAfter {
			t.Fatalf("webhook refresh with a queued untargeted syncAfter refresh: %s %+v", out, j)
		}
		// It also covers a refresh without syncAfter.
		if j, out := mustCreate(t, st, jobs.Spec{Type: jobs.TypeRefresh, Params: jobs.Params{IntegrationID: 1, ArrItemIDs: []int64{8}}}); out != outcomeCovered || j.ID != 40 {
			t.Fatalf("plain targeted refresh: %s %+v", out, j)
		}
	})
}

// TestCoalesceWebhookRefreshKeepsSyncAfter: a queued full refresh (no syncAfter) never absorbs a
// webhook's refresh, so the item's folder is still synced (phase2-3.md §12.2, COR1/CMP1).
func TestCoalesceWebhookRefreshKeepsSyncAfter(t *testing.T) {
	d := openDB(t)
	m := newManager(t, d, Options{})
	full := enqueue(t, m, jobs.Spec{Type: jobs.TypeRefresh, Trigger: jobs.TriggerSchedule, Params: jobs.Params{IntegrationID: 1}})
	hook := enqueue(t, m, webhookRefresh(7))
	if hook.ID == full.ID || !hook.Params.SyncAfter || !reflect.DeepEqual(hook.Params.ArrItemIDs, []int64{7}) {
		t.Fatalf("the webhook refresh was absorbed: full %d, webhook %+v", full.ID, hook)
	}
	fr := newFollowUpRunner(m)
	m.Register(jobs.TypeRefresh, fr)
	m.Register(jobs.TypeSync, jobs.RunnerFunc(func(context.Context, jobs.Job, jobs.Env) (jobs.Result, error) { return jobs.Result{}, nil }))
	start(t, m)
	waitStatus(t, m.Store(), full.ID, jobs.StatusCompleted)
	waitStatus(t, m.Store(), hook.ID, jobs.StatusCompleted)
	syncs := fr.syncs()
	if len(syncs) != 1 || !reflect.DeepEqual(syncs[0].Params.Paths, []string{"Item 7"}) || syncs[0].Trigger != jobs.TriggerWebhook {
		t.Fatalf("follow-up syncs = %+v; want one of the item's folder", syncs)
	}
}

// followUpRunner is a refresh runner that queues follow-up syncs the way mediaindex does: a
// targeted syncAfter refresh a sync of each item's folder, an untargeted syncAfter refresh an
// untargeted sync of the source, a refresh without syncAfter nothing.
type followUpRunner struct {
	enq jobs.Enqueuer
	ch  chan jobs.Job
}

func newFollowUpRunner(enq jobs.Enqueuer) *followUpRunner {
	return &followUpRunner{enq: enq, ch: make(chan jobs.Job, 100)}
}

func (f *followUpRunner) Run(ctx context.Context, job jobs.Job, env jobs.Env) (jobs.Result, error) {
	if !job.Params.SyncAfter {
		return jobs.Result{}, nil
	}
	spec := jobs.Spec{Type: jobs.TypeSync, Trigger: jobs.TriggerWebhook, Params: jobs.Params{DestinationID: 1, SourceIDs: []int64{1}}}
	for _, id := range job.Params.ArrItemIDs {
		spec.Params.Paths = append(spec.Params.Paths, "Item "+strconv.FormatInt(id, 10))
	}
	j, err := f.enq.Enqueue(ctx, spec)
	if err != nil {
		return jobs.Result{}, err
	}
	f.ch <- j
	return jobs.Result{Stats: map[string]any{"followUpJobs": []int64{j.ID}}}, nil
}

func (f *followUpRunner) syncs() []jobs.Job {
	var out []jobs.Job
	for {
		select {
		case j := <-f.ch:
			out = append(out, j)
		default:
			return out
		}
	}
}

// TestCoalesceMerge covers the merge row of §12.2.
func TestCoalesceMerge(t *testing.T) {
	t.Run("sync paths", func(t *testing.T) {
		_, st := coalesceStore(t)
		first, _ := mustCreate(t, st, targetedSync(3, "Heat (1995)"))
		j, out := mustCreate(t, st, targetedSync(3, "Alien (1979)", "Heat (1995)/extras"))
		if out != outcomeMerged || j.ID != first.ID || !j.QueuedAt.Equal(first.QueuedAt) ||
			!reflect.DeepEqual(j.Params.Paths, []string{"Alien (1979)", "Heat (1995)"}) || j.Trigger != jobs.TriggerWebhook {
			t.Fatalf("merge: %s %+v", out, j)
		}
		if got, _ := st.GetJob(context.Background(), first.ID); !reflect.DeepEqual(got.Params, j.Params) {
			t.Fatalf("stored %+v; returned %+v", got.Params, j.Params)
		}
		// Paths already covered by the job change nothing.
		if j, out := mustCreate(t, st, targetedSync(3, "Alien (1979)/Alien.mkv")); out != outcomeCovered || j.ID != first.ID {
			t.Fatalf("covered paths: %s %+v", out, j)
		}
		// Other params differ: another source, allowChanges → separate jobs.
		if j, out := mustCreate(t, st, targetedSync(4, "Heat (1995)")); out != outcomeCreated || j.ID == first.ID {
			t.Fatalf("another source: %s %+v", out, j)
		}
		allow := targetedSync(3, "Heat (1995)")
		allow.Params.AllowChanges = true
		if j, out := mustCreate(t, st, allow); out != outcomeCreated || j.ID == first.ID {
			t.Fatalf("allowChanges: %s %+v", out, j)
		}
		if n := jobCount(t, st); n != 3 {
			t.Fatalf("%d jobs; want 3", n)
		}
	})
	t.Run("refresh ids", func(t *testing.T) {
		_, st := coalesceStore(t)
		first, _ := mustCreate(t, st, webhookRefresh(9, 3))
		j, out := mustCreate(t, st, webhookRefresh(5, 3))
		if out != outcomeMerged || j.ID != first.ID || !reflect.DeepEqual(j.Params.ArrItemIDs, []int64{3, 5, 9}) || !j.Params.SyncAfter {
			t.Fatalf("merge: %s %+v", out, j)
		}
		// Without syncAfter the other params differ: a separate job, into which later ones merge.
		plain := jobs.Spec{Type: jobs.TypeRefresh, Params: jobs.Params{IntegrationID: 1, ArrItemIDs: []int64{4}}}
		p1, out := mustCreate(t, st, plain)
		if out != outcomeCreated || p1.ID == first.ID {
			t.Fatalf("refresh without syncAfter: %s %+v", out, p1)
		}
		plain.Params.ArrItemIDs = []int64{6}
		if j, out := mustCreate(t, st, plain); out != outcomeMerged || j.ID != p1.ID || !reflect.DeepEqual(j.Params.ArrItemIDs, []int64{4, 6}) {
			t.Fatalf("merge without syncAfter: %s %+v", out, j)
		}
	})
}

// TestCoalesceOverflow: a merge beyond the limits makes the queued job untargeted; a refresh
// keeps syncAfter and, when it runs, queues the untargeted follow-up syncs.
func TestCoalesceOverflow(t *testing.T) {
	ctx := context.Background()
	t.Run("sync", func(t *testing.T) {
		_, st := coalesceStore(t)
		first, _ := mustCreate(t, st, targetedSync(3, manyPaths("Movie", jobs.MaxTargetPaths)...))
		j, out := mustCreate(t, st, targetedSync(3, "One More"))
		want := jobs.Params{DestinationID: 1, SourceIDs: []int64{3}}
		if out != outcomeOverflow || j.ID != first.ID || !reflect.DeepEqual(j.Params, want) || !j.QueuedAt.Equal(first.QueuedAt) {
			t.Fatalf("overflow: %s %+v", out, j)
		}
		logs, err := st.ListLogs(ctx, first.ID, 0, 0)
		if err != nil || len(logs) != 1 || !strings.Contains(logs[0].Message, "syncs its whole source") {
			t.Fatalf("logs = %+v, %v", logs, err)
		}
		// The untargeted job now covers every later targeted sync of the source.
		if j, out := mustCreate(t, st, targetedSync(3, "Later")); out != outcomeCovered || j.ID != first.ID {
			t.Fatalf("after the overflow: %s %+v", out, j)
		}
	})
	t.Run("refresh keeps syncAfter and queues follow-ups", func(t *testing.T) {
		d := openDB(t)
		m := newManager(t, d, Options{})
		first := enqueue(t, m, webhookRefresh(manyIDs(1, jobs.MaxTargetItems)...))
		j := enqueue(t, m, webhookRefresh(1000))
		if j.ID != first.ID || !reflect.DeepEqual(j.Params, jobs.Params{IntegrationID: 1, SyncAfter: true}) {
			t.Fatalf("overflow: %+v", j)
		}
		if again := enqueue(t, m, webhookRefresh(2000)); again.ID != first.ID {
			t.Fatalf("a later webhook refresh was not absorbed by the untargeted syncAfter refresh: %+v", again)
		}
		logs, _ := m.Store().ListLogs(ctx, first.ID, 0, 0)
		if len(logs) != 1 || !strings.Contains(logs[0].Message, "refreshes the whole integration") ||
			!strings.Contains(logs[0].Message, "queues a sync") {
			t.Fatalf("logs = %+v", logs)
		}
		fr := newFollowUpRunner(m)
		m.Register(jobs.TypeRefresh, fr)
		m.Register(jobs.TypeSync, jobs.RunnerFunc(func(context.Context, jobs.Job, jobs.Env) (jobs.Result, error) { return jobs.Result{}, nil }))
		start(t, m)
		waitStatus(t, m.Store(), first.ID, jobs.StatusCompleted)
		syncs := fr.syncs()
		if len(syncs) != 1 || syncs[0].Params.Targeted() || !slices.Equal(syncs[0].Params.SourceIDs, []int64{1}) {
			t.Fatalf("follow-ups = %+v; want one untargeted sync of source 1", syncs)
		}
	})
}

// TestCoalesceNeverIntoARunningOrResumedJob: a running job is never merged into or treated as
// covering, and neither is a queued job that already ran (re-queued after a crash or a shutdown):
// its plan may be complete, so new targets would be lost.
func TestCoalesceNeverIntoARunningOrResumedJob(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		first jobs.Spec
		next  jobs.Spec
	}{
		{"targeted sync", targetedSync(3, "A"), targetedSync(3, "B")},
		{"untargeted sync", jobs.Spec{Type: jobs.TypeSync, Params: jobs.Params{DestinationID: 1}}, targetedSync(3, "B")},
		{"targeted refresh", webhookRefresh(1), webhookRefresh(2)},
		{"untargeted refresh", jobs.Spec{Type: jobs.TypeRefresh, Params: jobs.Params{IntegrationID: 1}},
			jobs.Spec{Type: jobs.TypeRefresh, Params: jobs.Params{IntegrationID: 1, ArrItemIDs: []int64{2}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, st := coalesceStore(t)
			first, _ := mustCreate(t, st, tc.first)
			if _, ok, err := st.markRunning(ctx, first.ID, time.Now()); !ok || err != nil {
				t.Fatalf("markRunning: %v %v", ok, err)
			}
			j, out := mustCreate(t, st, tc.next)
			if out != outcomeCreated || j.ID == first.ID {
				t.Fatalf("with the first job running: %s %+v", out, j)
			}
			if got, _ := st.GetJob(ctx, first.ID); !reflect.DeepEqual(got.Params, first.Params) {
				t.Fatalf("the running job changed: %+v", got.Params)
			}
			// The second one is re-queued as after a shutdown: it has started before.
			if _, ok, err := st.markRunning(ctx, j.ID, time.Now()); !ok || err != nil {
				t.Fatalf("markRunning: %v %v", ok, err)
			}
			if ok, err := st.requeue(ctx, j.ID, []byte(`{}`)); !ok || err != nil {
				t.Fatalf("requeue: %v %v", ok, err)
			}
			k, out := mustCreate(t, st, tc.next)
			if out != outcomeExisting || k.ID != j.ID {
				// An identical spec still finds it while its plan is not complete (phase1.md
				// dedupe; it plans again when it resumes) …
				t.Fatalf("identical spec after the re-queue: %s %+v", out, k)
			}
			other := tc.next
			if other.Type == jobs.TypeSync {
				other.Params.Paths = []string{"C"}
			} else {
				other.Params.ArrItemIDs = []int64{3}
			}
			k, out = mustCreate(t, st, other)
			if out != outcomeCreated || k.ID == j.ID || k.ID == first.ID {
				// … but nothing is merged into it.
				t.Fatalf("new targets after the re-queue: %s %+v", out, k)
			}
		})
	}
}

// TestDedupeNeverIntoAResumedJobWithACompletePlan: a job re-queued after a shutdown or a crash
// whose plan was complete resumes that plan without scanning again, so an identical request that
// is not a dry run gets a new job (a webhook sync of the same folder after a new import would
// otherwise never copy it). A dry run keeps the exact dedupe.
func TestDedupeNeverIntoAResumedJobWithACompletePlan(t *testing.T) {
	ctx := context.Background()
	item := []jobs.Item{{RelPath: "Show/Season 1/E01.mkv", Action: jobs.ActionCopy, Bytes: 10}}
	for _, tc := range []struct {
		name    string
		spec    jobs.Spec
		requeue func(st *Store, id int64) error
		want    enqueueOutcome
	}{
		{"targeted sync after a shutdown", targetedSync(3, "Show/Season 1"), shutdownRequeue, outcomeCreated},
		{"targeted sync after a crash", targetedSync(3, "Show/Season 1"), crashRequeue, outcomeCreated},
		{"untargeted sync after a shutdown", jobs.Spec{Type: jobs.TypeSync, Params: jobs.Params{DestinationID: 1}}, shutdownRequeue, outcomeCreated},
		{"dry run after a shutdown", jobs.Spec{Type: jobs.TypeSync, DryRun: true, Params: jobs.Params{DestinationID: 1}}, shutdownRequeue, outcomeExisting},
		{"verify after a shutdown", jobs.Spec{Type: jobs.TypeVerify, Params: jobs.Params{DestinationID: 1}}, shutdownRequeue, outcomeCreated},
		{"retention after a crash", jobs.Spec{Type: jobs.TypeRetention, Params: jobs.Params{DestinationID: 1}}, crashRequeue, outcomeCreated},
		// plexdb_backup and arr_backup also set planned_at (AddItems final) but never resume a stored
		// plan (a Plex DB backup starts over, an arr_backup follows its recorded Backup command): a
		// resumed one keeps the exact dedupe (phase1.md §6.2), so "Back up now" does not send a
		// second Backup command.
		{"plexdb_backup after a shutdown", jobs.Spec{Type: jobs.TypePlexDBBackup, Params: jobs.Params{IntegrationID: 5, DestinationID: 1}}, shutdownRequeue, outcomeExisting},
		{"arr_backup after a shutdown", jobs.Spec{Type: jobs.TypeArrBackup, Params: jobs.Params{IntegrationID: 2, DestinationID: 3}}, shutdownRequeue, outcomeExisting},
		{"arr_backup after a crash", jobs.Spec{Type: jobs.TypeArrBackup, Params: jobs.Params{IntegrationID: 2, DestinationID: 3}}, crashRequeue, outcomeExisting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, st := coalesceStore(t)
			s, _ := mustCreate(t, st, tc.spec)
			if _, ok, err := st.markRunning(ctx, s.ID, time.Now()); !ok || err != nil {
				t.Fatalf("markRunning: %v %v", ok, err)
			}
			if err := st.AddItems(ctx, s.ID, item, true); err != nil {
				t.Fatal(err)
			}
			if err := tc.requeue(st, s.ID); err != nil {
				t.Fatal(err)
			}
			if got, _ := st.GetJob(ctx, s.ID); got.Status != jobs.StatusQueued {
				t.Fatalf("job %d is %s after the re-queue; want queued", s.ID, got.Status)
			}
			j, out := mustCreate(t, st, tc.spec)
			if out != tc.want || (out == outcomeCreated) == (j.ID == s.ID) {
				t.Fatalf("identical spec with a resumed job whose plan is complete: %s job %d (resumed %d); want %s",
					out, j.ID, s.ID, tc.want)
			}
			if out != outcomeCreated {
				return
			}
			// The new job has not started, so the next identical spec is deduped into it.
			if k, out := mustCreate(t, st, tc.spec); out != outcomeExisting || k.ID != j.ID {
				t.Fatalf("second identical spec: %s job %d; want existing %d", out, k.ID, j.ID)
			}
		})
	}
}

// shutdownRequeue re-queues running job id as a graceful shutdown does.
func shutdownRequeue(st *Store, id int64) error {
	ok, err := st.requeue(context.Background(), id, []byte(`{}`))
	if err == nil && !ok {
		err = fmt.Errorf("job %d was not running", id)
	}
	return err
}

// crashRequeue re-queues running job id as start-up recovery after a crash does.
func crashRequeue(st *Store, id int64) error {
	requeued, _, err := st.recoverCrashed(context.Background(), 5, time.Now())
	if err == nil && !slices.Contains(requeued, id) {
		err = fmt.Errorf("job %d was not re-queued: %v", id, requeued)
	}
	return err
}

// TestCrashMatrixEnqueue crashes inside CreateJob's transaction, after each kind of write and
// before the commit: nothing of it is kept, and the retried enqueue gives the same result as a
// run without the crash.
func TestCrashMatrixEnqueue(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		setup []jobs.Spec
		spec  jobs.Spec
		want  enqueueOutcome
	}{
		{"insert", nil, targetedSync(3, "A"), outcomeCreated},
		{"merge", []jobs.Spec{targetedSync(3, "A")}, targetedSync(3, "B"), outcomeMerged},
		{"overflow", []jobs.Spec{targetedSync(3, manyPaths("M", jobs.MaxTargetPaths)...)}, targetedSync(3, "B"), outcomeOverflow},
		{"refresh overflow", []jobs.Spec{webhookRefresh(manyIDs(1, jobs.MaxTargetItems)...)}, webhookRefresh(9999), outcomeOverflow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, st := coalesceStore(t)
			for _, s := range tc.setup {
				mustCreate(t, st, s)
			}
			before, _ := st.ListJobs(ctx, JobQuery{})
			faultinject.SetHook(faultinject.CrashAt(PointEnqueueBeforeCommit, 1))
			crashed := func() (c bool) {
				defer func() {
					if p := recover(); p != nil {
						if _, ok := p.(faultinject.Crash); !ok {
							panic(p)
						}
						c = true
					}
				}()
				_, _, _ = st.createJob(ctx, tc.spec)
				return false
			}()
			faultinject.SetHook(nil)
			if !crashed {
				t.Fatal("the crash point was not reached")
			}
			after, _ := st.ListJobs(ctx, JobQuery{})
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("the crashed enqueue changed the queue:\nbefore %+v\nafter  %+v", before, after)
			}
			for _, j := range after.Records {
				if logs, _ := st.ListLogs(ctx, j.ID, 0, 0); len(logs) != 0 {
					t.Fatalf("the crashed enqueue logged on job %d: %+v", j.ID, logs)
				}
			}
			if _, out := mustCreate(t, st, tc.spec); out != tc.want {
				t.Fatalf("retry: %s; want %s", out, tc.want)
			}
		})
	}
}
