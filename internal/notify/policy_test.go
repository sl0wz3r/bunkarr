package notify

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/netguard"
)

// TestSendRefusesMetadataAddress checks that the default client dials through netguard (S16): an
// Apprise API URL on the cloud metadata address is refused before anything is sent, is not
// retried, and the error matches netguard.ErrBlocked.
func TestSendRefusesMetadataAddress(t *testing.T) {
	for _, api := range []string{"http://169.254.169.254", "http://169.254.169.254:8000/apprise", "http://[fe80::1]:8000"} {
		t.Run(api, func(t *testing.T) {
			c := NewClient()
			c.timeout = 2 * time.Second
			c.retryDelay = 10 * time.Millisecond
			start := time.Now()
			err := c.Send(context.Background(), Target{APIURL: api, ConfigKey: "k"}, Message{Body: "b", Type: TypeInfo})
			if !errors.Is(err, netguard.ErrBlocked) {
				t.Fatalf("err = %v, want netguard.ErrBlocked", err)
			}
			var serr *SendError
			if !errors.As(err, &serr) || serr.StatusCode != 0 || strings.Contains(err.Error(), "retry") ||
				!strings.Contains(err.Error(), "link-local or cloud metadata address") {
				t.Fatalf("err = %v", err)
			}
			if d := time.Since(start); d > time.Second {
				t.Fatalf("Send took %s: the address was dialled instead of refused", d)
			}
		})
	}
}

// jobNumRE finds the job id in a message body.
var jobNumRE = regexp.MustCompile(`Job: #(\d+)`)

// notifiedJobs returns the ids of the jobs whose notifications f received, sorted.
func notifiedJobs(t *testing.T, f *fakeApprise) []int64 {
	t.Helper()
	var ids []int64
	for _, r := range f.requests() {
		body, _ := r.Body["body"].(string)
		m := jobNumRE.FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("no job id in %q", body)
		}
		id, _ := strconv.ParseInt(m[1], 10, 64)
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// TestPhase2NotificationPolicy checks docs/design/phase2-3.md §12.5: refreshes, manifest exports
// and follow-up syncs (trigger webhook, or queued by a refresh: they name their source) never send
// success, and notify a warning, and a failure, at most once per integration or destination per
// 24 h while the problem lasts; held changes always notify; other jobs (arr_backup, untargeted
// scheduled syncs) are unchanged.
func TestPhase2NotificationPolicy(t *testing.T) {
	ctx := context.Background()
	s, _, _ := openStore(t)
	f := newFakeApprise(t, fakeOpts{})
	if _, err := s.Create(ctx, Input{Name: "everything", APIURL: f.URL(), ConfigKey: "all",
		OnFailure: new(true), OnWarning: new(true), OnSuccess: new(true)}); err != nil {
		t.Fatal(err)
	}
	d := New(s, nil, Options{Client: testClient(), Workers: 1})

	t0 := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	held, _ := json.Marshal(map[string]any{"filesHeld": 3})
	type step struct {
		id     int64
		typ    jobs.Type
		trig   jobs.Trigger
		status jobs.Status
		params jobs.Params
		after  time.Duration // finish time after t0
		stats  json.RawMessage
		want   bool
	}
	refresh1 := jobs.Params{IntegrationID: 1}
	refresh2 := jobs.Params{IntegrationID: 2}
	dest := jobs.Params{DestinationID: 5}
	webhook := jobs.Params{DestinationID: 5, SourceIDs: []int64{1}, Paths: []string{"Heat (1995)"}}
	const (
		ok   = jobs.StatusCompleted
		warn = jobs.StatusCompletedWithWarnings
		fail = jobs.StatusFailed
		sch  = jobs.TriggerSchedule
		wh   = jobs.TriggerWebhook
	)
	steps := []step{
		// No success for refreshes, manifest exports and follow-up syncs.
		{1, jobs.TypeRefresh, wh, ok, refresh1, 0, nil, false},
		{2, jobs.TypeManifestExport, sch, ok, dest, 0, nil, false},
		{3, jobs.TypeSync, wh, ok, webhook, 0, nil, false},
		{4, jobs.TypeSync, sch, ok, jobs.Params{DestinationID: 5, SourceIDs: []int64{1}, Paths: []string{"A"}}, 0, nil, false},
		{5, jobs.TypeSync, wh, ok, jobs.Params{DestinationID: 5}, 0, nil, false},
		// Unchanged: an untargeted scheduled sync and an *arr backup notify success.
		{6, jobs.TypeSync, sch, ok, dest, 0, nil, true},
		{7, jobs.TypeArrBackup, sch, ok, jobs.Params{IntegrationID: 1, DestinationID: 5}, 0, nil, true},
		// Refresh warnings: once per integration per 24 h while the problem lasts.
		{10, jobs.TypeRefresh, wh, warn, refresh1, time.Hour, nil, true},
		{11, jobs.TypeRefresh, sch, warn, refresh1, 6 * time.Hour, nil, false},
		{12, jobs.TypeRefresh, sch, fail, refresh1, 12 * time.Hour, nil, true}, // a failure is never held back by a warning
		{18, jobs.TypeRefresh, sch, fail, refresh1, 13 * time.Hour, nil, false},
		{13, jobs.TypeRefresh, sch, warn, refresh2, 12 * time.Hour, nil, true},
		{14, jobs.TypeRefresh, sch, warn, refresh1, 25 * time.Hour, nil, true},
		{15, jobs.TypeRefresh, sch, ok, refresh1, 26 * time.Hour, nil, false}, // the problem is over
		{16, jobs.TypeRefresh, sch, fail, refresh1, 27 * time.Hour, nil, true},
		{17, jobs.TypeRefresh, sch, fail, refresh1, 28 * time.Hour, nil, false},
		// Manifest exports: once per destination per 24 h.
		{20, jobs.TypeManifestExport, sch, warn, dest, time.Hour, nil, true},
		{21, jobs.TypeManifestExport, sch, fail, dest, 2 * time.Hour, nil, true},
		{23, jobs.TypeManifestExport, sch, warn, dest, 3 * time.Hour, nil, false},
		{22, jobs.TypeManifestExport, sch, warn, jobs.Params{DestinationID: 6}, 2 * time.Hour, nil, true},
		// Follow-up syncs: once per destination per 24 h; held changes always notify.
		{30, jobs.TypeSync, wh, warn, webhook, time.Hour, nil, true},
		{31, jobs.TypeSync, wh, fail, webhook, 2 * time.Hour, nil, true},
		{35, jobs.TypeSync, wh, fail, webhook, 3 * time.Hour, nil, false},
		{32, jobs.TypeSync, sch, warn, jobs.Params{DestinationID: 5, SourceIDs: []int64{2}, Paths: []string{"B"}}, 3 * time.Hour, nil, false},
		{33, jobs.TypeSync, wh, warn, webhook, 4 * time.Hour, held, true},
		{34, jobs.TypeSync, wh, warn, webhook, 5 * time.Hour, held, true},
		// Untargeted scheduled syncs keep Phase 1's behaviour: every warning notifies.
		{40, jobs.TypeSync, sch, warn, dest, 2 * time.Hour, nil, true},
		{41, jobs.TypeSync, sch, warn, dest, 3 * time.Hour, nil, true},
	}
	var want []int64
	for _, st := range steps {
		fin := t0.Add(st.after)
		d.Handle(jobs.Job{ID: st.id, Type: st.typ, Trigger: st.trig, Status: st.status, Params: st.params,
			Stats: st.stats, FinishedAt: &fin})
		if st.want {
			want = append(want, st.id)
		}
	}
	if err := d.Close(ctx); err != nil {
		t.Fatal(err)
	}
	slices.Sort(want)
	if got := notifiedJobs(t, f); !slices.Equal(got, want) {
		t.Fatalf("notified jobs %v\nwant            %v", got, want)
	}
}

// TestPhase2FallbackTitles checks the job-type names used when Describe has nothing.
func TestPhase2FallbackTitles(t *testing.T) {
	for typ, want := range map[jobs.Type]string{
		jobs.TypeRefresh:        "Refresh",
		jobs.TypeArrBackup:      "Backup",
		jobs.TypeManifestExport: "Manifest export",
	} {
		if got := typeName(typ); got != want {
			t.Errorf("typeName(%s) = %q, want %q", typ, got, want)
		}
	}
}

// limitStep is one finished job of a §12.5 limit test.
type limitStep struct {
	id     int64
	typ    jobs.Type
	trig   jobs.Trigger
	status jobs.Status
	params jobs.Params
	after  time.Duration // finish time after the test's t0
	want   bool          // whether the target receives it
}

// runLimitSteps hands the steps to a dispatcher, one at a time (each is processed before the next
// is handled, as jobs that finish hours apart are), and checks which jobs f received.
func runLimitSteps(t *testing.T, d *Dispatcher, f *fakeApprise, t0 time.Time, steps []limitStep) {
	t.Helper()
	var want []int64
	for _, st := range steps {
		fin := t0.Add(st.after)
		d.Handle(jobs.Job{ID: st.id, Type: st.typ, Trigger: st.trig, Status: st.status, Params: st.params, FinishedAt: &fin})
		d.pending.Wait()
		if st.want {
			want = append(want, st.id)
		}
	}
	if err := d.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	slices.Sort(want)
	if got := notifiedJobs(t, f); !slices.Equal(got, want) {
		t.Fatalf("notified jobs %v\nwant            %v", got, want)
	}
}

// newLimitDispatcher returns a dispatcher and a fake Apprise API with one target subscribed to
// the given events.
func newLimitDispatcher(t *testing.T, onFailure, onWarning, onSuccess bool, o fakeOpts, queueSize int) (*Dispatcher, *fakeApprise) {
	t.Helper()
	s, _, _ := openStore(t)
	f := newFakeApprise(t, o)
	if _, err := s.Create(context.Background(), Input{Name: "target", APIURL: f.URL(), ConfigKey: "k",
		OnFailure: new(onFailure), OnWarning: new(onWarning), OnSuccess: new(onSuccess)}); err != nil {
		t.Fatal(err)
	}
	return New(s, nil, Options{Client: testClient(), Workers: 1, QueueSize: queueSize}), f
}

// TestPhase2LimitEndsOnlyWhenCovered checks that only a clean job that did all the notified job
// did ends a §12.5 problem: a targeted refresh never ends a full refresh's warning (it skips the
// full refresh's checks), a sync of source A never ends source B's failure, a sync of other paths
// never ends a path's failure. A full refresh, an untargeted sync and a covering targeted job do.
func TestPhase2LimitEndsOnlyWhenCovered(t *testing.T) {
	const (
		ok   = jobs.StatusCompleted
		warn = jobs.StatusCompletedWithWarnings
		fail = jobs.StatusFailed
		sch  = jobs.TriggerSchedule
		wh   = jobs.TriggerWebhook
	)
	full := jobs.Params{IntegrationID: 1}
	items := func(ids ...int64) jobs.Params { return jobs.Params{IntegrationID: 1, ArrItemIDs: ids, SyncAfter: true} }
	srcSync := func(src int64, paths ...string) jobs.Params {
		return jobs.Params{DestinationID: 5, SourceIDs: []int64{src}, Paths: paths}
	}
	t.Run("refresh", func(t *testing.T) {
		d, f := newLimitDispatcher(t, true, true, true, fakeOpts{}, 0)
		runLimitSteps(t, d, f, time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC), []limitStep{
			// The 6-hourly full refresh warns (Change File Date, unmapped root folder); imports
			// between them run clean targeted refreshes: one notification per 24 h.
			{1, jobs.TypeRefresh, sch, warn, full, 0, true},
			{2, jobs.TypeRefresh, wh, ok, items(7), time.Hour, false},
			{3, jobs.TypeRefresh, sch, warn, full, 6 * time.Hour, false},
			{4, jobs.TypeRefresh, wh, ok, items(8, 9), 7 * time.Hour, false},
			{5, jobs.TypeRefresh, sch, warn, full, 12 * time.Hour, false},
			{6, jobs.TypeRefresh, wh, ok, items(7), 13 * time.Hour, false},
			{7, jobs.TypeRefresh, sch, warn, full, 18 * time.Hour, false},
			// A clean full refresh ends it: the next warning notifies at once.
			{8, jobs.TypeRefresh, sch, ok, full, 19 * time.Hour, false},
			{9, jobs.TypeRefresh, sch, warn, full, 20 * time.Hour, true},
			{10, jobs.TypeRefresh, sch, ok, full, 21 * time.Hour, false},
			// A targeted refresh's failure ends with a clean refresh of the same or more items.
			{11, jobs.TypeRefresh, wh, fail, items(7), 22 * time.Hour, true},
			{12, jobs.TypeRefresh, wh, ok, items(8), 23 * time.Hour, false},
			{13, jobs.TypeRefresh, wh, fail, items(7), 24 * time.Hour, false},
			{14, jobs.TypeRefresh, wh, ok, items(7, 8), 25 * time.Hour, false},
			{15, jobs.TypeRefresh, wh, fail, items(7), 26 * time.Hour, true},
		})
	})
	t.Run("sync", func(t *testing.T) {
		d, f := newLimitDispatcher(t, true, true, false, fakeOpts{}, 0)
		t0 := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
		runLimitSteps(t, d, f, t0, []limitStep{
			// Source B's share is unmounted: its follow-up syncs fail; source A's succeed.
			{1, jobs.TypeSync, wh, fail, srcSync(2, "B1"), 0, true},
			{2, jobs.TypeSync, wh, ok, srcSync(1, "A1"), 10 * time.Minute, false},
			{3, jobs.TypeSync, wh, fail, srcSync(2, "B2"), 20 * time.Minute, false},
			{4, jobs.TypeSync, wh, ok, srcSync(1, "A2"), 30 * time.Minute, false},
			{5, jobs.TypeSync, wh, fail, srcSync(2, "B3"), 40 * time.Minute, false},
			// Nor does a clean sync of source A whole (a refresh queued it without paths).
			{18, jobs.TypeSync, sch, ok, srcSync(1), 42 * time.Minute, false},
			{19, jobs.TypeSync, wh, fail, srcSync(2, "B1"), 44 * time.Minute, false},
			// A clean sync of other paths of B does not end B1..B3's failure; a sync of B whole does.
			{6, jobs.TypeSync, wh, ok, srcSync(2, "B4"), 50 * time.Minute, false},
			{7, jobs.TypeSync, wh, fail, srcSync(2, "B1"), time.Hour, false},
			{8, jobs.TypeSync, sch, ok, srcSync(2), 70 * time.Minute, false},
			{9, jobs.TypeSync, wh, fail, srcSync(2, "B1"), 80 * time.Minute, true},
			// A clean sync of the failed path, or of a folder that contains it, ends it.
			{10, jobs.TypeSync, wh, ok, srcSync(2, "B1", "B5"), 90 * time.Minute, false},
			{11, jobs.TypeSync, wh, fail, srcSync(2, "Movies/Heat (1995)"), 100 * time.Minute, true},
			{12, jobs.TypeSync, wh, ok, srcSync(2, "Movies"), 110 * time.Minute, false},
			{13, jobs.TypeSync, wh, fail, srcSync(2, "B1"), 2 * time.Hour, true},
			// The destination's own untargeted sync (all sources) ends it too.
			{14, jobs.TypeSync, sch, ok, jobs.Params{DestinationID: 5}, 130 * time.Minute, false},
			{15, jobs.TypeSync, wh, fail, srcSync(2, "B1"), 140 * time.Minute, true},
			// Another destination's sync ends nothing here.
			{16, jobs.TypeSync, sch, ok, jobs.Params{DestinationID: 6}, 150 * time.Minute, false},
			{17, jobs.TypeSync, wh, fail, srcSync(2, "B1"), 160 * time.Minute, false},
		})
	})
}

// TestPhase2FailureNotHeldByUnsentWarning checks that a warning no target received never holds
// back a failure: a target that wants only failures gets the first failure after a warning, and
// a target that wants both gets both (warnings and failures are limited separately).
func TestPhase2FailureNotHeldByUnsentWarning(t *testing.T) {
	const (
		warn = jobs.StatusCompletedWithWarnings
		fail = jobs.StatusFailed
		sch  = jobs.TriggerSchedule
	)
	full := jobs.Params{IntegrationID: 1}
	t0 := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	t.Run("failures only", func(t *testing.T) {
		d, f := newLimitDispatcher(t, true, false, false, fakeOpts{}, 0)
		runLimitSteps(t, d, f, t0, []limitStep{
			{1, jobs.TypeRefresh, sch, warn, full, 0, false},
			{2, jobs.TypeRefresh, sch, fail, full, 6 * time.Hour, true},
			{3, jobs.TypeRefresh, sch, fail, full, 12 * time.Hour, false},
			{4, jobs.TypeRefresh, sch, fail, full, 18 * time.Hour, false},
		})
	})
	t.Run("warnings only", func(t *testing.T) {
		d, f := newLimitDispatcher(t, false, true, false, fakeOpts{}, 0)
		runLimitSteps(t, d, f, t0, []limitStep{
			{1, jobs.TypeRefresh, sch, fail, full, 0, false},
			{2, jobs.TypeRefresh, sch, warn, full, 6 * time.Hour, true},
			{3, jobs.TypeRefresh, sch, warn, full, 12 * time.Hour, false},
		})
	})
	t.Run("both", func(t *testing.T) {
		d, f := newLimitDispatcher(t, true, true, false, fakeOpts{}, 0)
		runLimitSteps(t, d, f, t0, []limitStep{
			{1, jobs.TypeRefresh, sch, warn, full, 0, true},
			{2, jobs.TypeRefresh, sch, fail, full, 6 * time.Hour, true},
			{3, jobs.TypeRefresh, sch, fail, full, 12 * time.Hour, false},
			{4, jobs.TypeRefresh, sch, warn, full, 18 * time.Hour, false},
		})
	})
}

// TestPhase2UndeliveredNotificationFreesLimit checks that a limited notification that reached no
// target (the Apprise API failed, or the queue was full) does not use up the 24 h window.
func TestPhase2UndeliveredNotificationFreesLimit(t *testing.T) {
	full := jobs.Params{IntegrationID: 1}
	t0 := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	t.Run("send failed", func(t *testing.T) {
		d, f := newLimitDispatcher(t, true, true, false, fakeOpts{statuses: []int{500, 500}}, 0)
		for i, after := range []time.Duration{0, 6 * time.Hour, 12 * time.Hour} {
			fin := t0.Add(after)
			d.Handle(jobs.Job{ID: int64(i + 1), Type: jobs.TypeRefresh, Trigger: jobs.TriggerSchedule, Status: jobs.StatusFailed,
				Params: full, FinishedAt: &fin})
			d.pending.Wait()
		}
		if err := d.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		// Job 1: two failed attempts; job 2 delivered; job 3 held back by job 2.
		if got := requestJobs(t, f); !slices.Equal(got, []int64{1, 1, 2}) {
			t.Fatalf("requests for jobs %v, want [1 1 2]", got)
		}
	})
	t.Run("queue full", func(t *testing.T) {
		block := make(chan struct{})
		d, f := newLimitDispatcher(t, true, true, true, fakeOpts{block: block}, 1)
		fin := t0
		// Job 1 (not limited) holds the worker, job 2 fills the queue, job 3 is dropped.
		d.Handle(jobs.Job{ID: 1, Type: jobs.TypeArrBackup, Status: jobs.StatusFailed, FinishedAt: &fin})
		deadline := time.Now().Add(5 * time.Second)
		for len(f.requests()) == 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		d.Handle(jobs.Job{ID: 2, Type: jobs.TypeArrBackup, Status: jobs.StatusFailed, FinishedAt: &fin})
		d.Handle(jobs.Job{ID: 3, Type: jobs.TypeRefresh, Status: jobs.StatusFailed, Params: full, FinishedAt: &fin})
		close(block)
		d.pending.Wait()
		fin2 := t0.Add(time.Hour)
		d.Handle(jobs.Job{ID: 4, Type: jobs.TypeRefresh, Status: jobs.StatusFailed, Params: full, FinishedAt: &fin2})
		if err := d.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := notifiedJobs(t, f); !slices.Equal(got, []int64{1, 2, 4}) {
			t.Fatalf("notified jobs %v, want [1 2 4]", got)
		}
	})
	t.Run("no subscriber", func(t *testing.T) {
		// No target wants warnings at the first one; a target added an hour later gets the next
		// warning at once.
		d, f := newLimitDispatcher(t, true, false, false, fakeOpts{}, 0)
		warning := func(id int64, fin time.Time) {
			d.Handle(jobs.Job{ID: id, Type: jobs.TypeRefresh, Status: jobs.StatusCompletedWithWarnings, Params: full, FinishedAt: &fin})
			d.pending.Wait()
		}
		warning(1, t0)
		if _, err := d.store.Create(context.Background(), Input{Name: "warnings", APIURL: f.URL(), ConfigKey: "w",
			OnFailure: new(false), OnWarning: new(true), OnSuccess: new(false)}); err != nil {
			t.Fatal(err)
		}
		warning(2, t0.Add(time.Hour))
		if err := d.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := notifiedJobs(t, f); !slices.Equal(got, []int64{2}) {
			t.Fatalf("notified jobs %v, want [2]", got)
		}
	})
	t.Run("closed", func(t *testing.T) {
		d, _ := newLimitDispatcher(t, true, true, false, fakeOpts{}, 0)
		if err := d.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		fin := t0
		d.Handle(jobs.Job{ID: 1, Type: jobs.TypeRefresh, Status: jobs.StatusFailed, Params: full, FinishedAt: &fin})
		if slots := limitSlots(d); slots != 0 {
			t.Fatalf("%d slots taken by a job dropped after Close, want 0", slots)
		}
	})
	t.Run("shutting down", func(t *testing.T) {
		d, f := newLimitDispatcher(t, true, true, true, fakeOpts{block: make(chan struct{})}, 0)
		fin := t0
		// Job 1 (not limited) holds the worker; job 2 waits in the queue and is dropped by Close.
		d.Handle(jobs.Job{ID: 1, Type: jobs.TypeArrBackup, Status: jobs.StatusFailed, FinishedAt: &fin})
		deadline := time.Now().Add(5 * time.Second)
		for len(f.requests()) == 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		d.Handle(jobs.Job{ID: 2, Type: jobs.TypeRefresh, Status: jobs.StatusFailed, Params: full, FinishedAt: &fin})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := d.Close(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("Close = %v, want context.Canceled", err)
		}
		if slots := limitSlots(d); slots != 0 {
			t.Fatalf("%d slots taken by a job dropped by Close, want 0", slots)
		}
	})
}

// limitSlots returns how many §12.5 slots d holds.
func limitSlots(d *Dispatcher) int {
	d.limitMu.Lock()
	defer d.limitMu.Unlock()
	return len(d.notified)
}

// TestPhase2MaybeDeliveredKeepsLimit checks that a send that may have reached some services uses
// up the 24 h window like a delivered one: a 424 (some of the key's services got it) and a timeout
// after the request went out (a slow Apprise that delivers). Otherwise the services that work get
// every repeat of a lasting problem.
func TestPhase2MaybeDeliveredKeepsLimit(t *testing.T) {
	t0 := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	warnings := func(t *testing.T, d *Dispatcher, after ...time.Duration) {
		t.Helper()
		for i, a := range after {
			fin := t0.Add(a)
			d.Handle(jobs.Job{ID: int64(i + 1), Type: jobs.TypeRefresh, Trigger: jobs.TriggerSchedule,
				Status: jobs.StatusCompletedWithWarnings, Params: jobs.Params{IntegrationID: 1}, FinishedAt: &fin})
			d.pending.Wait()
		}
		if err := d.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("424", func(t *testing.T) {
		d, f := newLimitDispatcher(t, true, true, false, fakeOpts{statuses: []int{424, 424, 424, 424}}, 0)
		warnings(t, d, 0, 6*time.Hour, 12*time.Hour, 18*time.Hour)
		if got := requestJobs(t, f); !slices.Equal(got, []int64{1}) {
			t.Fatalf("requests for jobs %v, want [1]", got)
		}
	})
	t.Run("timeout after sending", func(t *testing.T) {
		d, f := newLimitDispatcher(t, true, true, false, fakeOpts{block: make(chan struct{})}, 0)
		d.client.timeout = 50 * time.Millisecond
		warnings(t, d, 0, 6*time.Hour, 12*time.Hour)
		// Job 1 twice (a timeout is retried once); jobs 2 and 3 are held back.
		if got := requestJobs(t, f); !slices.Equal(got, []int64{1, 1}) {
			t.Fatalf("requests for jobs %v, want [1 1]", got)
		}
	})
}

// requestJobs returns the job id of every request f received, in order.
func requestJobs(t *testing.T, f *fakeApprise) []int64 {
	t.Helper()
	var ids []int64
	for _, r := range f.requests() {
		body, _ := r.Body["body"].(string)
		m := jobNumRE.FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("no job id in %q", body)
		}
		id, _ := strconv.ParseInt(m[1], 10, 64)
		ids = append(ids, id)
	}
	return ids
}

// TestPhase2RefreshQueuedUntargetedSync checks that a sync a refresh queued without paths (a
// missing folder, a source root, too many folders: it names its one source, trigger like its
// refresh) is a follow-up sync: no success, and its warnings are limited.
func TestPhase2RefreshQueuedUntargetedSync(t *testing.T) {
	queuedByRefresh := jobs.Params{DestinationID: 5, SourceIDs: []int64{2}}
	d, f := newLimitDispatcher(t, true, true, true, fakeOpts{}, 0)
	runLimitSteps(t, d, f, time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC), []limitStep{
		{1, jobs.TypeSync, jobs.TriggerSchedule, jobs.StatusCompleted, queuedByRefresh, 0, false},
		{2, jobs.TypeSync, jobs.TriggerStartup, jobs.StatusCompleted, queuedByRefresh, 0, false},
		{3, jobs.TypeSync, jobs.TriggerManual, jobs.StatusCompletedWithWarnings, queuedByRefresh, time.Hour, true},
		{4, jobs.TypeSync, jobs.TriggerResume, jobs.StatusCompletedWithWarnings, queuedByRefresh, 2 * time.Hour, false},
		// The destination's own syncs (no sourceIds) are unchanged.
		{5, jobs.TypeSync, jobs.TriggerManual, jobs.StatusCompleted, jobs.Params{DestinationID: 5}, 3 * time.Hour, true},
		{6, jobs.TypeSync, jobs.TriggerSchedule, jobs.StatusCompletedWithWarnings, jobs.Params{DestinationID: 5}, 4 * time.Hour, true},
		{7, jobs.TypeSync, jobs.TriggerSchedule, jobs.StatusCompletedWithWarnings, jobs.Params{DestinationID: 5}, 5 * time.Hour, true},
	})
}

// TestPhase2LimitSurvivesClockSetBack checks that after the clock was set back by more than the
// window, the limit starts a new window instead of letting every warning through.
func TestPhase2LimitSurvivesClockSetBack(t *testing.T) {
	full := jobs.Params{IntegrationID: 1}
	const (
		warn = jobs.StatusCompletedWithWarnings
		sch  = jobs.TriggerSchedule
	)
	d, f := newLimitDispatcher(t, true, true, false, fakeOpts{}, 0)
	runLimitSteps(t, d, f, time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC), []limitStep{
		{1, jobs.TypeRefresh, sch, warn, full, 0, true},
		// NTP sets the clock back 48 h.
		{2, jobs.TypeRefresh, sch, warn, full, -48 * time.Hour, true},
		{3, jobs.TypeRefresh, sch, warn, full, -42 * time.Hour, false},
		{4, jobs.TypeRefresh, sch, warn, full, -36 * time.Hour, false},
		{5, jobs.TypeRefresh, sch, jobs.StatusCompleted, full, -35 * time.Hour, false},
		{6, jobs.TypeRefresh, sch, warn, full, -34 * time.Hour, true},
	})
}

// TestPhase2LimitAfterShortClockSetBack checks a clock set back by less than the window: a job
// that finished well before the last notified one of its key measures that slot on the new clock
// from then on, so a clean job ends the problem, and a lasting problem notifies again about 24 h
// later (not 24 h plus the set-back). Hooks that ran slightly out of order still end nothing.
func TestPhase2LimitAfterShortClockSetBack(t *testing.T) {
	full := jobs.Params{IntegrationID: 1}
	const (
		ok   = jobs.StatusCompleted
		warn = jobs.StatusCompletedWithWarnings
		fail = jobs.StatusFailed
		sch  = jobs.TriggerSchedule
	)
	t0 := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		steps []limitStep
	}{
		// In each case but the last, NTP sets the clock back 12 h after job 1.
		{"clean job ends it", []limitStep{
			{1, jobs.TypeRefresh, sch, fail, full, 0, true},
			{2, jobs.TypeRefresh, sch, ok, full, -11 * time.Hour, false},
			{3, jobs.TypeRefresh, sch, fail, full, -10 * time.Hour, true},
		}},
		{"recovers and comes back", []limitStep{
			{1, jobs.TypeRefresh, sch, fail, full, 0, true},
			{2, jobs.TypeRefresh, sch, fail, full, -11 * time.Hour, false},
			{3, jobs.TypeRefresh, sch, ok, full, -10 * time.Hour, false},
			{4, jobs.TypeRefresh, sch, fail, full, -9 * time.Hour, true},
			{5, jobs.TypeRefresh, sch, fail, full, -8 * time.Hour, false},
		}},
		{"lasting problem", []limitStep{
			{1, jobs.TypeRefresh, sch, warn, full, 0, true},
			{2, jobs.TypeRefresh, sch, warn, full, -11 * time.Hour, false},
			{3, jobs.TypeRefresh, sch, warn, full, 12 * time.Hour, false},
			{4, jobs.TypeRefresh, sch, warn, full, 13 * time.Hour, true},
		}},
		{"hooks out of order", []limitStep{
			{1, jobs.TypeRefresh, sch, fail, full, 0, true},
			{2, jobs.TypeRefresh, sch, ok, full, -30 * time.Second, false}, // finished before job 1
			{3, jobs.TypeRefresh, sch, fail, full, time.Minute, false},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, f := newLimitDispatcher(t, true, true, false, fakeOpts{}, 0)
			runLimitSteps(t, d, f, t0, tc.steps)
		})
	}
}
