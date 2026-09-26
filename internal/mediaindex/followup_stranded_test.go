package mediaindex

import (
	"context"
	"database/sql"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr"
	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// syncPaths flattens queued syncs into "dest/sources:path" entries ("*" for untargeted), so syncs
// the real queue would merge compare equal to one sync with all their paths.
func syncPaths(list []jobs.Job) []string {
	var out []string
	for _, s := range syncTargets(list) {
		prefix, paths, _ := strings.Cut(s, ":")
		for _, p := range strings.Split(paths, ",") {
			if e := prefix + ":" + p; !slices.Contains(out, e) {
				out = append(out, e)
			}
		}
	}
	slices.Sort(out)
	return out
}

// TestStrandedIntentsFollowedUpByNextRefresh: a full refresh that crashed after it rewrote the
// index leaves its reconcile intents pending. When jobqueue then ends the job without running it
// again (start-up recovery fails it after too many crashes, or the resumed job is cancelled
// before it starts), the next refresh of the integration queues their syncs: it would not find
// those changes itself, since the index has them already.
func TestStrandedIntentsFollowedUpByNextRefresh(t *testing.T) {
	clean := newEnv(t, arr.KindRadarr)
	clean.full()
	changeRadarr(clean)
	clean.full()
	want := syncPaths(clean.enq.queued())

	cases := []struct {
		name   string
		status jobs.Status
		// end ends the crashed job (left running) the way jobqueue does, without its runner.
		end func(t *testing.T, e *testEnv, id int64)
	}{
		{"failed by crash recovery", jobs.StatusFailed, func(t *testing.T, e *testEnv, _ int64) {
			m := jobqueue.New(e.db, nil, jobqueue.Options{MaxAttempts: 1})
			if err := m.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := m.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
		}},
		{"resumed, then cancelled before it started", jobs.StatusCancelled, func(t *testing.T, e *testEnv, id int64) {
			// Start-up recovery re-queues it (as jobqueue's recoverCrashed does; a started
			// manager would dispatch it), then the user cancels the queued job.
			err := e.db.Write(context.Background(), func(tx *sql.Tx) error {
				_, err := tx.ExecContext(context.Background(), `UPDATE jobs SET status = 'queued', trigger = 'resume',
					attempt = attempt + 1, heartbeat_at = NULL WHERE id = ? AND status = 'running'`, id)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := jobqueue.New(e.db, nil, jobqueue.Options{}).Cancel(context.Background(), id); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			e := newEnv(t, arr.KindRadarr)
			e.full()
			changeRadarr(e)
			store := jobqueue.NewStore(e.db)
			spec := jobs.Spec{Type: jobs.TypeRefresh, Trigger: jobs.TriggerSchedule, Params: jobs.Params{IntegrationID: e.it.ID}}
			crashedJob, _, err := store.CreateJob(ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			err = e.db.Write(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `UPDATE jobs SET status = 'running', started_at = queued_at WHERE id = ?`, crashedJob.ID)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			// Crash after the index rows were rewritten: the intents are pending, nothing is queued.
			if crashed, rep, err := runWith(e, crashedJob, store, faultinject.CrashAt(PointBeforeMarkDeleted, 1)); !crashed {
				t.Fatalf("did not crash (err %v)\n%s", err, rep)
			}
			if p, err := store.Pending(ctx, crashedJob.ID, 0, 100); err != nil || len(p) == 0 || len(e.enq.queued()) != 0 {
				t.Fatalf("after the crash: pending %v (%v), queued %v", p, err, e.enq.queued())
			}
			c.end(t, e, crashedJob.ID)
			if j, _ := store.GetJob(ctx, crashedJob.ID); j.Status != c.status {
				t.Fatalf("crashed job is %s", j.Status)
			}

			e.clock.Advance(1)
			spec.Trigger = jobs.TriggerStartup
			next, _, err := store.CreateJob(ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			if _, rep, err := runWith(e, next, store, nil); err != nil {
				t.Fatalf("next refresh: %v\n%s", err, rep)
			}
			if got := syncPaths(e.enq.queued()); !reflect.DeepEqual(got, want) {
				t.Fatalf("syncs = %v, want %v", got, want)
			}
			if p, err := store.Pending(ctx, crashedJob.ID, 0, 100); err != nil || len(p) != 0 {
				t.Fatalf("crashed job's intents still pending: %v %v", p, err)
			}
		})
	}
}

// runWith runs job with a real item store, with a faultinject hook; it reports a crash.
func runWith(e *testEnv, job jobs.Job, items jobs.ItemStore, hook func(string)) (crashed bool, rep *memReporter, err error) {
	e.t.Helper()
	faultinject.SetHook(hook)
	defer faultinject.SetHook(nil)
	rep = &memReporter{}
	defer func() {
		if p := recover(); p != nil {
			if _, ok := p.(faultinject.Crash); !ok {
				panic(p)
			}
			crashed = true
		}
	}()
	_, err = e.runner.Run(context.Background(), job, jobs.Env{Reporter: rep, Items: items})
	return false, rep, err
}

// TestStrandedIntentJobsSelection: only this integration's non-dry refresh jobs that ended failed,
// or cancelled without syncAfter (its webhook events are re-armed instead), with pending intents
// count; never the running job itself.
func TestStrandedIntentJobsSelection(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, arr.KindRadarr)
	store := jobqueue.NewStore(e.db)
	type job struct {
		status    jobs.Status
		dry       bool
		syncAfter bool
		other     bool // another integration
		pending   bool
	}
	cases := []job{
		{status: jobs.StatusFailed, pending: true},                     // 0: yes
		{status: jobs.StatusCancelled, pending: true},                  // 1: yes
		{status: jobs.StatusCancelled, syncAfter: true, pending: true}, // 2: re-armed
		{status: jobs.StatusFailed, syncAfter: true, pending: true},    // 3: yes
		{status: jobs.StatusFailed},                                    // 4: nothing pending
		{status: jobs.StatusCompleted, pending: true},                  // 5: its runner followed up
		{status: jobs.StatusQueued, pending: true},                     // 6: will run
		{status: jobs.StatusFailed, dry: true, pending: true},          // 7: dry run
		{status: jobs.StatusFailed, other: true, pending: true},        // 8: other integration
	}
	other, err := e.ints.Create(ctx, integrations.Input{Type: e.it.Type, Name: "other", URL: e.srv.URL, APIKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for i, c := range cases {
		p := jobs.Params{IntegrationID: e.it.ID, SyncAfter: c.syncAfter}
		if c.other {
			p.IntegrationID = other.ID
		}
		if c.syncAfter {
			p.ArrItemIDs = []int64{int64(i + 1)}
		}
		j, _, err := store.CreateJob(ctx, jobs.Spec{Type: jobs.TypeRefresh, Trigger: jobs.TriggerManual, DryRun: c.dry, Params: p})
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if c.pending {
			if err := store.AddItems(ctx, j.ID, []jobs.Item{{RelPath: "/movies/x", Action: jobs.ActionSkip, Detail: []byte(`{"arrChange":{"arrId":1}}`)}}, false); err != nil {
				t.Fatal(err)
			}
		}
		err = e.db.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE jobs SET status = ? WHERE id = ?`, string(c.status), j.ID)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, j.ID)
	}
	got, err := e.runner.Store().strandedIntentJobs(ctx, e.it.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{ids[0], ids[1], ids[3]}; !reflect.DeepEqual(got, want) {
		t.Fatalf("stranded = %v, want %v (jobs %v)", got, want, ids)
	}
	if got, _ := e.runner.Store().strandedIntentJobs(ctx, e.it.ID, ids[1]); !reflect.DeepEqual(got, []int64{ids[0], ids[3]}) {
		t.Fatalf("excluding job %d: %v", ids[1], got)
	}
}

// overflowJob creates a running overflow refresh (untargeted, with syncAfter) in the job store:
// created targeted, then made untargeted in place, as an overflow merge leaves it.
func overflowJob(t *testing.T, e *testEnv, store *jobqueue.Store) jobs.Job {
	t.Helper()
	ctx := context.Background()
	j, _, err := store.CreateJob(ctx, jobs.Spec{Type: jobs.TypeRefresh, Trigger: jobs.TriggerWebhook,
		Params: jobs.Params{IntegrationID: e.it.ID, ArrItemIDs: []int64{1}, SyncAfter: true}})
	if err != nil {
		t.Fatal(err)
	}
	err = e.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE jobs SET status = 'running', started_at = queued_at,
			params = json_remove(params, '$.arrItemIds') WHERE id = ?`, j.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	j.Params.ArrItemIDs = nil
	return j
}

// TestStrandedOverflowRefreshFollowedUpByNextRefresh: an overflow webhook refresh records no
// intents (it syncs every source of the root folders when it ends, also when it failed). When it
// crashes after it rewrote the index and start-up recovery fails it without running it again, the
// next refresh queues that untargeted sync: it would not find the imports the overflow absorbed
// itself, since the index has them already.
func TestStrandedOverflowRefreshFollowedUpByNextRefresh(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, arr.KindRadarr)
	e.full()
	changeRadarr(e)
	store := jobqueue.NewStore(e.db)
	crashedJob := overflowJob(t, e, store)
	if crashed, rep, err := runWith(e, crashedJob, store, faultinject.CrashAt(PointBeforeMarkDeleted, 1)); !crashed {
		t.Fatalf("did not crash (err %v)\n%s", err, rep)
	}
	if q := e.enq.queued(); len(q) != 0 {
		t.Fatalf("queued before the crash: %v", syncPaths(q))
	}
	m := jobqueue.New(e.db, nil, jobqueue.Options{MaxAttempts: 1})
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if j, _ := store.GetJob(ctx, crashedJob.ID); j.Status != jobs.StatusFailed {
		t.Fatalf("crashed job is %s", j.Status)
	}

	e.clock.Advance(1)
	spec := jobs.Spec{Type: jobs.TypeRefresh, Trigger: jobs.TriggerSchedule, Params: jobs.Params{IntegrationID: e.it.ID}}
	next, _, err := store.CreateJob(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, rep, err := runWith(e, next, store, nil); err != nil {
		t.Fatalf("next refresh: %v\n%s", err, rep)
	}
	whole := "1/[" + itoa(int(e.src.ID)) + "]:*"
	got := syncPaths(e.enq.queued())
	if !slices.Contains(got, whole) {
		t.Fatalf("syncs = %v, want the untargeted %s among them", got, whole)
	}
	if p, err := store.Pending(ctx, crashedJob.ID, 0, 100); err != nil || len(p) != 0 {
		t.Fatalf("crashed job's items still pending: %v %v", p, err)
	}
	// Followed up once: a later refresh queues nothing more.
	e.clock.Advance(1)
	later, _, err := store.CreateJob(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, rep, err := runWith(e, later, store, nil); err != nil {
		t.Fatalf("later refresh: %v\n%s", err, rep)
	}
	if again := syncPaths(e.enq.queued()); !reflect.DeepEqual(again, got) {
		t.Fatalf("syncs after a later refresh = %v, want still %v", again, got)
	}
}

// TestOverflowRefreshSettlesItsMark: an overflow refresh whose runner sees it end (completed, or
// failed) queues its untargeted sync itself and leaves nothing pending, so no later refresh
// queues that sync again.
func TestOverflowRefreshSettlesItsMark(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed", true: "failed"}[fail], func(t *testing.T) {
			ctx := context.Background()
			e := newEnv(t, arr.KindRadarr)
			e.full()
			if fail {
				e.srv.SetStatus(http.MethodGet, "movie", http.StatusInternalServerError)
			}
			store := jobqueue.NewStore(e.db)
			j := overflowJob(t, e, store)
			if _, rep, err := runWith(e, j, store, nil); (err != nil) != fail {
				t.Fatalf("err = %v\n%s", err, rep)
			}
			whole := []string{"1/[" + itoa(int(e.src.ID)) + "]:*"}
			if got := syncPaths(e.enq.queued()); !reflect.DeepEqual(got, whole) {
				t.Fatalf("syncs = %v, want %v", got, whole)
			}
			if p, err := store.Pending(ctx, j.ID, 0, 100); err != nil || len(p) != 0 {
				t.Fatalf("items still pending: %v %v", p, err)
			}
		})
	}
}
