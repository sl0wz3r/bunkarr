package mediaindex

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/config"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// TestOverflowRefreshFailureQueuesUntargetedSyncs: an untargeted refresh with syncAfter (an
// overflow merge absorbed the webhook's items) that fails still queues the untargeted sync of
// every source a root folder locates into (design §6.1), since the items it absorbed are gone.
func TestOverflowRefreshFailureQueuesUntargetedSyncs(t *testing.T) {
	cases := []struct {
		name string
		kind arr.Kind
		// fail makes the refresh fail.
		fail func(e *testEnv)
	}{
		{"the item list fails", arr.KindRadarr, func(e *testEnv) { e.srv.SetStatus(http.MethodGet, "movie", http.StatusInternalServerError) }},
		// The metadata fails: the root folders the index has are used.
		{"the root folders fail", arr.KindRadarr, func(e *testEnv) { e.srv.SetStatus(http.MethodGet, "rootfolder", http.StatusInternalServerError) }},
		// A per-series request fails after batches were committed.
		{"a series fails mid-fetch", arr.KindSonarr, func(e *testEnv) {
			e.srv.SetStatus(http.MethodGet, "episodefile?seriesId=2", http.StatusInternalServerError)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, c.kind)
			e.full()
			c.fail(e)
			e.runner = e.newRunner(1)
			_, rep, err := e.refresh(jobs.Params{SyncAfter: true}, false, jobs.TriggerWebhook)
			if err == nil {
				t.Fatal("refresh did not fail")
			}
			want := []string{"1/[" + itoa(int(e.src.ID)) + "]:*"}
			if got := syncTargets(e.enq.queued()); !reflect.DeepEqual(got, want) {
				t.Fatalf("syncs = %v, want %v\n%s", got, want, rep)
			}
			if q := e.enq.queued(); q[0].Trigger != jobs.TriggerWebhook {
				t.Fatalf("trigger = %s", q[0].Trigger)
			}
			if e.state().Status != StatusFailed {
				t.Fatalf("state = %+v", e.state())
			}
		})
	}
}

// cancellingRunner returns a runner whose destination listing cancels the job's context first:
// the user cancels (or the process shuts down) while the follow-up syncs are being queued.
func cancellingRunner(t *testing.T, e *testEnv, cancel context.CancelFunc) *Runner {
	t.Helper()
	r, err := NewRunner(RefreshOptions{DB: e.db, Integrations: e.ints, Catalog: e.cat, Enqueuer: e.enq, Now: e.clock.Now,
		Destinations: func(ctx context.Context) ([]FollowUpDestination, error) {
			cancel()
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return slices.Clone(e.dests), nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestCancelDuringFollowUpsIsNotACompletedRefresh: a cancel (or shutdown) while the follow-up
// syncs are queued ends the refresh as cancelled, not completed with a warning, so a shut-down job
// resumes its pending intents and a cancelled webhook refresh has its events re-armed.
func TestCancelDuringFollowUpsIsNotACompletedRefresh(t *testing.T) {
	t.Run("targeted", func(t *testing.T) {
		e := newEnv(t, arr.KindRadarr)
		e.full()
		changeRadarr(e)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		job := jobs.Job{ID: 70, Type: jobs.TypeRefresh, Trigger: jobs.TriggerWebhook, Attempt: 1,
			Params: jobs.Params{IntegrationID: e.it.ID, ArrItemIDs: []int64{3}, SyncAfter: true}}
		items := &memItems{}
		_, err := cancellingRunner(t, e, cancel).Run(ctx, job, jobs.Env{Reporter: &memReporter{}, Items: items})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if q := e.enq.queued(); len(q) != 0 {
			t.Fatalf("a cancelled webhook refresh queued %v", q)
		}
		pending := 0
		for _, it := range items.all() {
			if it.Status == jobs.ItemPending {
				pending++
			}
		}
		if pending == 0 {
			t.Fatalf("the intents were not left pending: %+v", items.all())
		}
		// The shut-down job resumes: its pending intents (the old folder too) are followed up.
		e.clock.Advance(1)
		job.Attempt, job.Trigger = 2, jobs.TriggerResume
		if _, rep, err := e.runJob(e.runner, job, items); err != nil {
			t.Fatalf("resume: %v\n%s", err, rep)
		}
		want := []string{"1/[" + itoa(int(e.src.ID)) + "]:Charade,Charade (1963)"}
		if got := syncTargets(e.enq.queued()); !reflect.DeepEqual(got, want) {
			t.Fatalf("syncs = %v, want %v", got, want)
		}
	})
	t.Run("full", func(t *testing.T) {
		clean := newEnv(t, arr.KindRadarr)
		clean.full()
		changeRadarr(clean)
		clean.full()
		wantSyncs := syncTargets(clean.enq.queued())

		e := newEnv(t, arr.KindRadarr)
		e.full()
		changeRadarr(e)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		items := &memItems{}
		_, err := cancellingRunner(t, e, cancel).Run(ctx, jobs.Job{ID: 71, Type: jobs.TypeRefresh, Trigger: jobs.TriggerSchedule,
			Attempt: 1, Params: jobs.Params{IntegrationID: e.it.ID}}, jobs.Env{Reporter: &memReporter{}, Items: items})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		// The reconcile's changes are in the index already: the cancel still follows them up.
		if got := syncTargets(e.enq.queued()); !reflect.DeepEqual(got, wantSyncs) {
			t.Fatalf("syncs = %v, want %v", got, wantSyncs)
		}
		for _, it := range items.all() {
			if it.Status != jobs.ItemDone {
				t.Fatalf("intent left %s: %+v", it.Status, it)
			}
		}
	})
}

// TestCancelledReconcileFollowsUpWrittenChanges: a full refresh cancelled after a batch rewrote
// the index still queues the syncs of the changes it recorded, since the next refresh compares
// with the rewritten index and finds no change.
func TestCancelledReconcileFollowsUpWrittenChanges(t *testing.T) {
	e := newEnv(t, arr.KindRadarr)
	e.full()
	changeRadarr(e)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	faultinject.SetHook(func(point string) {
		if point == PointAfterBatch {
			cancel()
		}
	})
	defer faultinject.SetHook(nil)
	items := &memItems{}
	_, _, err := func() (jobs.Result, *memReporter, error) {
		rep := &memReporter{}
		res, err := e.newRunner(2).Run(ctx, jobs.Job{ID: 72, Type: jobs.TypeRefresh, Trigger: jobs.TriggerSchedule, Attempt: 1,
			Params: jobs.Params{IntegrationID: e.it.ID}}, jobs.Env{Reporter: rep, Items: items})
		return res, rep, err
	}()
	faultinject.SetHook(nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	// Movie 2's upgrade was written by the first batch: its folder is synced.
	if f := e.files(); f[77].ID == 0 {
		t.Fatalf("the first batch was not written: %v", f)
	}
	got := syncTargets(e.enq.queued())
	if len(got) != 1 || !strings.Contains(got[0], "His Girl Friday (1940)") {
		t.Fatalf("syncs = %v", got)
	}
	if len(items.all()) == 0 {
		t.Fatal("no intent was recorded")
	}
	for _, it := range items.all() {
		if it.Status != jobs.ItemDone {
			t.Fatalf("intent left %s: %+v", it.Status, it)
		}
	}
}

// TestEarlyRefreshFailureIsRecordedAndFollowedUp: a webhook refresh that fails before it reaches
// the *arr (here its key cannot be unsealed: the keyring changed) records the failure and queues
// the follow-up sync of the index's folders (design §6.1).
func TestEarlyRefreshFailureIsRecordedAndFollowedUp(t *testing.T) {
	e := newEnv(t, arr.KindRadarr)
	e.full()
	kr, err := config.NewKeyring(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	e.ints = integrations.NewStore(e.db, kr)
	e.runner = e.newRunner(DefaultBatchSize)
	_, rep, err := e.refresh(jobs.Params{ArrItemIDs: []int64{2}, SyncAfter: true}, false, jobs.TriggerWebhook)
	if err == nil {
		t.Fatal("refresh did not fail")
	}
	want := []string{"1/[" + itoa(int(e.src.ID)) + "]:His Girl Friday (1940)"}
	if got := syncTargets(e.enq.queued()); !reflect.DeepEqual(got, want) {
		t.Fatalf("syncs = %v, want %v\n%s", got, want, rep)
	}
	if s := e.state(); s.Status != StatusFailed || s.Error == "" {
		t.Fatalf("state = %+v", s)
	}
}
