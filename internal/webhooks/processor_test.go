package webhooks

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

func outcomeOf(r Record) Outcome {
	if r.Outcome == nil {
		return ""
	}
	return *r.Outcome
}

// TestProcessorPerItemDueTimes: a SeriesDelete with deletedFiles for A, then a Download for B one
// second later: B's refresh is queued at t ≈ 6 s, A's at t ≈ 60 s (design §15).
func TestProcessorPerItemDueTimes(t *testing.T) {
	h := newProcHarness(t)
	del := h.receive(integrations.TypeSonarr, h.sonarr, string(fixture(t, integrations.TypeSonarr, "SeriesDelete-deletedFiles.json")))
	h.at(time.Second)
	dl := h.receive(integrations.TypeSonarr, h.sonarr, `{"eventType":"Download","series":{"id":2,"title":"B"}}`)
	for _, step := range []time.Duration{2 * time.Second, 5900 * time.Millisecond} {
		h.at(step)
		if n := len(h.refreshes()); n != 0 {
			t.Fatalf("t=%s: %d refreshes", step, n)
		}
	}
	h.at(6 * time.Second)
	rs := h.refreshes()
	if len(rs) != 1 || !slices.Equal(rs[0].Params.ArrItemIDs, []int64{2}) || !rs[0].Params.SyncAfter || rs[0].Trigger != jobs.TriggerWebhook ||
		rs[0].Params.IntegrationID != h.sonarr {
		t.Fatalf("t=6s: refreshes = %+v", rs)
	}
	if e := h.event(dl.ID); outcomeOf(e) != OutcomeQueued || e.JobID == nil || *e.JobID != rs[0].ID {
		t.Fatalf("download event = %+v", e)
	}
	h.finishJobs() // a queued refresh would absorb A's items (coalescing)
	h.at(59 * time.Second)
	if n := len(h.refreshes()); n != 1 {
		t.Fatalf("t=59s: %d refreshes", n)
	}
	h.at(60 * time.Second)
	rs = h.refreshes()
	if len(rs) != 2 || !slices.Equal(rs[1].Params.ArrItemIDs, []int64{1}) {
		t.Fatalf("t=60s: refreshes = %+v", rs)
	}
	if e := h.event(del.ID); outcomeOf(e) != OutcomeQueued || e.ProcessedAt == nil {
		t.Fatalf("delete event = %+v", e)
	}
	if h.proc.Backlog() != 0 {
		t.Fatalf("backlog = %d", h.proc.Backlog())
	}
}

// TestProcessorUpgradePair: the upgrade's file delete and its Download, 3 minutes apart (a copy
// from another volume), give one refresh, 5 s after the Download.
func TestProcessorUpgradePair(t *testing.T) {
	h := newProcHarness(t)
	del := h.receive(integrations.TypeRadarr, h.radarr, string(fixture(t, integrations.TypeRadarr, "MovieFileDelete-upgrade.json")))
	for _, step := range []time.Duration{time.Minute, 2*time.Minute + 59*time.Second} {
		h.at(step)
		if n := len(h.refreshes()); n != 0 {
			t.Fatalf("t=%s: %d refreshes while the upgrade is held", step, n)
		}
	}
	h.at(3 * time.Minute)
	dl := h.receive(integrations.TypeRadarr, h.radarr, string(fixture(t, integrations.TypeRadarr, "Download-upgrade.json")))
	h.at(3*time.Minute + 4*time.Second)
	if n := len(h.refreshes()); n != 0 {
		t.Fatalf("flushed before the quiet window: %d", n)
	}
	h.at(3*time.Minute + 5*time.Second)
	rs := h.refreshes()
	if len(rs) != 1 || !slices.Equal(rs[0].Params.ArrItemIDs, []int64{1}) {
		t.Fatalf("refreshes = %+v", rs)
	}
	for _, id := range []int64{del.ID, dl.ID} {
		if e := h.event(id); e.JobID == nil || *e.JobID != rs[0].ID || outcomeOf(e) != OutcomeQueued {
			t.Fatalf("event %d = %+v", id, e)
		}
	}

	// A file delete whose Download never comes is flushed after the hold.
	h2 := newProcHarness(t)
	h2.receive(integrations.TypeRadarr, h2.radarr, string(fixture(t, integrations.TypeRadarr, "MovieFileDelete-upgrade.json")))
	h2.at(29 * time.Minute)
	if len(h2.refreshes()) != 0 {
		t.Fatal("flushed before the hold ended")
	}
	h2.at(30 * time.Minute)
	if len(h2.refreshes()) != 1 {
		t.Fatal("not flushed when the hold ended")
	}
}

// TestProcessorQuietWindowCap: events that keep coming postpone the flush by 5 s each, but not
// beyond 20 s after the first.
func TestProcessorQuietWindowCap(t *testing.T) {
	h := newProcHarness(t)
	for s := 0; s <= 18; s += 3 {
		h.at(time.Duration(s) * time.Second)
		h.receive(integrations.TypeRadarr, h.radarr, `{"eventType":"Rename","movie":{"id":7}}`)
	}
	h.at(19 * time.Second)
	if len(h.refreshes()) != 0 {
		t.Fatal("flushed before the cap")
	}
	h.at(20 * time.Second)
	rs := h.refreshes()
	if len(rs) != 1 {
		t.Fatalf("refreshes at the cap = %d", len(rs))
	}
	h.finishJobs()
	// Two integrations flush separately, and items of one integration share one job.
	h.receive(integrations.TypeRadarr, h.radarr, `{"eventType":"Download","movie":{"id":9}}`)
	h.receive(integrations.TypeRadarr, h.radarr, `{"eventType":"Download","movie":{"id":8}}`)
	h.receive(integrations.TypeSonarr, h.sonarr, `{"eventType":"SeriesAdd","series":{"id":3}}`)
	h.at(26 * time.Second)
	rs = h.refreshes()
	if len(rs) != 3 || !slices.Equal(rs[1].Params.ArrItemIDs, []int64{8, 9}) || rs[2].Params.IntegrationID != h.sonarr {
		t.Fatalf("refreshes = %+v", rs)
	}
}

// TestProcessorCoalescedOutcome: an item flushed while its earlier refresh is still queued is
// merged into it and recorded as coalesced.
func TestProcessorCoalescedOutcome(t *testing.T) {
	h := newProcHarness(t)
	h.receive(integrations.TypeRadarr, h.radarr, `{"eventType":"Download","movie":{"id":1}}`)
	h.at(5 * time.Second)
	second := h.receive(integrations.TypeRadarr, h.radarr, `{"eventType":"Download","movie":{"id":2}}`)
	h.at(10 * time.Second)
	rs := h.refreshes()
	if len(rs) != 1 || !slices.Equal(rs[0].Params.ArrItemIDs, []int64{1, 2}) {
		t.Fatalf("refreshes = %+v", rs)
	}
	if e := h.event(second.ID); outcomeOf(e) != OutcomeCoalesced || *e.JobID != rs[0].ID {
		t.Fatalf("second event = %+v", e)
	}
}

// TestProcessorFailures: events without an item, an integration that was disabled, and an
// enqueue that fails three times are marked failed; two failures are retried.
func TestProcessorFailures(t *testing.T) {
	h := newProcHarness(t)
	noItem := h.receive(integrations.TypeRadarr, h.radarr, `{"eventType":"Download"}`)
	h.at(0)
	if e := h.event(noItem.ID); outcomeOf(e) != OutcomeFailed {
		t.Fatalf("event without an item = %+v", e)
	}

	h.enq.fails = 2
	ok := h.receive(integrations.TypeRadarr, h.radarr, `{"eventType":"Download","movie":{"id":4}}`)
	h.at(5 * time.Second)
	h.at(10 * time.Second)
	if outcomeOf(h.event(ok.ID)) != "" {
		t.Fatal("marked after a failed enqueue")
	}
	h.at(15 * time.Second)
	if e := h.event(ok.ID); outcomeOf(e) != OutcomeQueued || h.enq.calls() != 3 {
		t.Fatalf("after two failures = %+v (calls %d)", e, h.enq.calls())
	}

	h.enq.fails = 3
	bad := h.receive(integrations.TypeRadarr, h.radarr, `{"eventType":"Download","movie":{"id":5}}`)
	for _, s := range []time.Duration{20, 25, 30} {
		h.at(s * time.Second)
	}
	if e := h.event(bad.ID); outcomeOf(e) != OutcomeFailed || e.JobID != nil {
		t.Fatalf("after three failures = %+v", e)
	}

	// A disabled integration's events fail without a job.
	disabled := false
	h.proc.o.Enabled = func(context.Context, int64) (bool, error) { return disabled, nil }
	gone := h.receive(integrations.TypeRadarr, h.radarr, `{"eventType":"Download","movie":{"id":6}}`)
	h.at(40 * time.Second)
	if e := h.event(gone.ID); outcomeOf(e) != OutcomeFailed {
		t.Fatalf("disabled integration's event = %+v", e)
	}
	if h.proc.Backlog() != 0 {
		t.Fatalf("backlog = %d", h.proc.Backlog())
	}
}

// TestProcessorRestartAfterCrashMergesIntoTheSameJob: a crash between the enqueue and the write
// that marks the events leaves them unprocessed; after the restart the processor reads them from
// their class and targets and its enqueue merges them into the job already queued.
func TestProcessorRestartAfterCrashMergesIntoTheSameJob(t *testing.T) {
	h := newProcHarness(t)
	ev := h.receive(integrations.TypeRadarr, h.radarr, string(fixture(t, integrations.TypeRadarr, "Download.json")))
	func() {
		faultinject.SetHook(faultinject.CrashAt(PointAfterEnqueue, 1))
		defer faultinject.SetHook(nil)
		defer func() {
			if p := recover(); p != nil {
				if _, ok := p.(faultinject.Crash); !ok {
					panic(p)
				}
			}
		}()
		h.at(5 * time.Second)
		t.Fatal("did not crash")
	}()
	if outcomeOf(h.event(ev.ID)) != "" {
		t.Fatal("the crashed flush marked the event")
	}
	jobsBefore := h.refreshes()
	if len(jobsBefore) != 1 {
		t.Fatalf("refreshes after the crash = %+v", jobsBefore)
	}
	// Restart.
	h.proc = h.newProcessor()
	if err := h.proc.Start(h.ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.proc.Stop(context.Background()) }()
	deadline := time.Now().Add(5 * time.Second)
	for outcomeOf(h.event(ev.ID)) == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	e := h.event(ev.ID)
	if outcomeOf(e) != OutcomeCoalesced || e.JobID == nil || *e.JobID != jobsBefore[0].ID || len(h.refreshes()) != 1 {
		t.Fatalf("after the restart: event %+v, refreshes %+v", e, h.refreshes())
	}
}

// TestProcessorCancelledRefreshRearms: when a refresh that carries webhook events is cancelled,
// its events become unprocessed again and their items get a new refresh; a cancelled sync or a
// finished refresh re-arms nothing.
func TestProcessorCancelledRefreshRearms(t *testing.T) {
	h := newProcHarness(t)
	ev := h.receive(integrations.TypeRadarr, h.radarr, `{"eventType":"Download","movie":{"id":3}}`)
	h.at(5 * time.Second)
	rs := h.refreshes()
	if len(rs) != 1 {
		t.Fatal("no refresh")
	}
	job := rs[0]
	for _, j := range []jobs.Job{{ID: job.ID, Type: jobs.TypeRefresh, Status: jobs.StatusCompleted}, {ID: job.ID, Type: jobs.TypeSync, Status: jobs.StatusCancelled}} {
		h.proc.OnJobFinish(j)
		if outcomeOf(h.event(ev.ID)) != OutcomeQueued {
			t.Fatalf("%s %s re-armed the event", j.Type, j.Status)
		}
	}
	cancelJob(t, h.db, job.ID)
	job.Status = jobs.StatusCancelled
	h.proc.OnJobFinish(job)
	if e := h.event(ev.ID); e.ProcessedAt != nil || e.JobID != nil || h.proc.Backlog() != 1 {
		t.Fatalf("re-armed event = %+v backlog %d", e, h.proc.Backlog())
	}
	h.at(6 * time.Second)
	rs = h.refreshes()
	if len(rs) != 2 || !slices.Equal(rs[1].Params.ArrItemIDs, []int64{3}) {
		t.Fatalf("refreshes after the cancel = %+v", rs)
	}
	if e := h.event(ev.ID); *e.JobID != rs[1].ID || outcomeOf(e) != OutcomeQueued {
		t.Fatalf("event after the new flush = %+v", e)
	}
}

// finishJobs marks every queued job completed (none is merged into any more).
func (h *procHarness) finishJobs() {
	h.t.Helper()
	err := h.db.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE jobs SET status = 'completed', finished_at = ? WHERE status = 'queued'`, db.FormatTime(t0))
		return err
	})
	if err != nil {
		h.t.Fatal(err)
	}
}

func cancelJob(t *testing.T, d *db.DB, id int64) {
	t.Helper()
	err := d.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE jobs SET status = 'cancelled', finished_at = ? WHERE id = ?`, db.FormatTime(t0), id)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestProcessorLoop runs the real loop with short windows.
func TestProcessorLoop(t *testing.T) {
	d := openDB(t)
	st := NewStore(d)
	id := addIntegration(t, d, integrations.TypeRadarr, "Radarr", true)
	enq := &storeEnqueuer{st: newJobStore(d)}
	p := NewProcessor(ProcessorOptions{Store: st, Enqueuer: enq, Quiet: 20 * time.Millisecond, Cap: 100 * time.Millisecond,
		DeleteDelay: 50 * time.Millisecond, UpgradeHold: time.Second})
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background()); err == nil {
		t.Fatal("started twice")
	}
	ev, _ := Parse(integrations.TypeRadarr, strings.NewReader(`{"eventType":"Download","movie":{"id":1}}`))
	rec, err := st.Insert(context.Background(), id, integrations.TypeRadarr, ev, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	p.Notify(rec)
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, _ := st.Get(context.Background(), rec.ID)
		if r.ProcessedAt != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the loop did not flush the event")
		}
		time.Sleep(5 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestStorePrune prunes by age, by rows and by payload bytes.
func TestStorePrune(t *testing.T) {
	h := newProcHarness(t)
	body := string(fixture(t, integrations.TypeRadarr, "Download.json"))
	var ids []int64
	for i := range 6 {
		h.now = t0.Add(time.Duration(i) * 24 * time.Hour)
		ids = append(ids, h.receive(integrations.TypeRadarr, h.radarr, body).ID)
	}
	// Age: 30 days after the second event, the first two are older than 30 days.
	h.now = t0.Add(31*24*time.Hour + time.Minute)
	n, err := h.store.Prune(h.ctx)
	if err != nil || n != 2 {
		t.Fatalf("prune by age = %d, %v", n, err)
	}
	// Rows: keep 3.
	h.store.maxRows = 3
	if n, err := h.store.Prune(h.ctx); err != nil || n != 1 {
		t.Fatalf("prune by rows = %d, %v", n, err)
	}
	if _, err := h.store.Get(h.ctx, ids[2]); err == nil {
		t.Fatal("the oldest row survived")
	}
	// Bytes: room for two payloads, checked on insert; the processor then prunes the oldest down
	// to 90% of the budget (room for one).
	size := len(h.event(ids[5]).Payload)
	h.store.maxRows = MaxRows
	h.store.maxBytes = int64(2*size + 10)
	last := h.receive(integrations.TypeRadarr, h.radarr, body)
	if !h.store.overBudget() {
		t.Fatal("the insert did not take the payloads over the budget")
	}
	h.proc.step(h.ctx, h.now)
	page, err := h.store.List(h.ctx, Query{})
	if err != nil || page.TotalRecords != 1 || page.Records[0].ID != last.ID {
		t.Fatalf("after the byte budget: %+v, %v", page, err)
	}
	// The hourly prune enforces the budget too.
	h.receive(integrations.TypeRadarr, h.radarr, body)
	h.receive(integrations.TypeRadarr, h.radarr, body)
	if n, err := h.store.Prune(h.ctx); err != nil || n != 2 {
		t.Fatalf("prune by bytes = %d, %v", n, err)
	}
}

func TestStoreListAndActivity(t *testing.T) {
	h := newProcHarness(t)
	h.receive(integrations.TypeRadarr, h.radarr, string(fixture(t, integrations.TypeRadarr, "Test.json")))
	h.receive(integrations.TypeRadarr, h.radarr, string(fixture(t, integrations.TypeRadarr, "Grab.json")))
	dl := h.receive(integrations.TypeRadarr, h.radarr, string(fixture(t, integrations.TypeRadarr, "Download.json")))
	h.receive(integrations.TypeSonarr, h.sonarr, string(fixture(t, integrations.TypeSonarr, "Download.json")))

	page, err := h.store.List(h.ctx, Query{IntegrationID: h.radarr})
	if err != nil || page.TotalRecords != 3 {
		t.Fatalf("list = %+v, %v", page, err)
	}
	got := page.Records[0]
	if got.ID != dl.ID || got.Summary.Title == "" || len(got.Summary.Files) != 1 || got.Payload != nil || got.Class != ClassDownload {
		t.Fatalf("newest = %+v", got)
	}
	for _, tc := range []struct {
		q    Query
		want int64
	}{
		{Query{Outcome: "test"}, 1},
		{Query{Outcome: "ignored"}, 1},
		{Query{Outcome: "pending"}, 2},
		{Query{EventType: "Download"}, 2},
		{Query{IntegrationID: h.sonarr, EventType: "Download"}, 1},
	} {
		p, err := h.store.List(h.ctx, tc.q)
		if err != nil || p.TotalRecords != tc.want {
			t.Errorf("List(%+v) = %d, %v; want %d", tc.q, p.TotalRecords, err, tc.want)
		}
	}
	if _, err := h.store.List(h.ctx, Query{Outcome: "bogus"}); err == nil {
		t.Fatal("an unknown outcome was accepted")
	}
	full, err := h.store.Get(h.ctx, dl.ID)
	if err != nil || len(full.Payload) == 0 || !strings.Contains(string(full.Payload), `"eventType":"Download"`) {
		t.Fatalf("Get = %+v, %v", full, err)
	}
	if _, err := h.store.Get(h.ctx, 9999); err == nil {
		t.Fatal("Get of an unknown id")
	}
	a, err := h.store.Activity(h.ctx, h.radarr, h.now)
	if err != nil || a.Last24h != 3 || a.LastTestAt == nil || a.LastEventAt == nil || len(a.Recent) != 3 {
		t.Fatalf("activity = %+v, %v", a, err)
	}
}

// TestStoreInsertRejectsBadEvents: an unknown class or an oversized payload is never stored.
func TestStoreInsertRejectsBadEvents(t *testing.T) {
	h := newProcHarness(t)
	if _, err := h.store.Insert(h.ctx, h.radarr, integrations.TypeRadarr, Event{EventType: "x", Class: "bogus", Payload: []byte("{}")}, t0); err == nil {
		t.Fatal("an unknown class was stored")
	}
	if _, err := h.store.Insert(h.ctx, h.radarr, integrations.TypeRadarr, Event{EventType: "x", Class: ClassChange,
		Payload: []byte(`"` + strings.Repeat("x", MaxPayload) + `"`)}, t0); err == nil {
		t.Fatal("an oversized payload was stored")
	}
}

// TestProcessorBurstIsOneRefresh: the items of a burst are due a little apart; the first flush
// takes the others that only wait for their quiet window, but never an item waiting for a delete's
// delay or an upgrade's hold.
func TestProcessorBurstIsOneRefresh(t *testing.T) {
	h := newProcHarness(t)
	for i := range 30 {
		h.at(time.Duration(i) * 100 * time.Millisecond)
		h.receive(integrations.TypeRadarr, h.radarr, fmt.Sprintf(`{"eventType":"Download","movie":{"id":%d}}`, 1+i%3))
	}
	h.receive(integrations.TypeRadarr, h.radarr, `{"eventType":"MovieDelete","deletedFiles":true,"movie":{"id":7}}`)
	h.receive(integrations.TypeRadarr, h.radarr, `{"eventType":"MovieFileDelete","deleteReason":"upgrade","movie":{"id":8}}`)
	h.at(7700 * time.Millisecond) // item 1's last event was at 2.7 s
	rs := h.refreshes()
	if len(rs) != 1 || !slices.Equal(rs[0].Params.ArrItemIDs, []int64{1, 2, 3}) {
		t.Fatalf("refreshes = %+v", rs)
	}
	if h.proc.Backlog() != 2 {
		t.Fatalf("backlog = %d (the delete and the upgrade wait)", h.proc.Backlog())
	}
}

// TestProcessorStopAfterFailedStart: when Start fails before the loop runs (a SIGTERM during
// start-up cancels its context), Stop returns at once instead of waiting for a loop that never
// ran until the shutdown deadline, which would leave the job manager no time to stop.
func TestProcessorStopAfterFailedStart(t *testing.T) {
	h := newProcHarness(t)
	h.receive(integrations.TypeRadarr, h.radarr, `{"eventType":"Download","movie":{"id":1}}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.proc.Start(ctx); err == nil {
		t.Fatal("Start with a cancelled context succeeded")
	}
	stopCtx, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	began := time.Now()
	if err := h.proc.Stop(stopCtx); err != nil {
		t.Fatalf("Stop after a failed Start = %v after %s", err, time.Since(began))
	}
}

// cancellingEnqueuer cancels the job its next Enqueue returns and runs the OnFinish hook before
// returning it: the user cancelled the refresh between the enqueue and the write that marks the
// events.
type cancellingEnqueuer struct {
	*storeEnqueuer
	h      *procHarness
	cancel bool
}

func (e *cancellingEnqueuer) Enqueue(ctx context.Context, spec jobs.Spec) (jobs.Job, error) {
	j, err := e.storeEnqueuer.Enqueue(ctx, spec)
	if err == nil && e.cancel {
		e.cancel = false
		cancelJob(e.h.t, e.h.db, j.ID)
		cancelled := j
		cancelled.Status = jobs.StatusCancelled
		e.h.proc.OnJobFinish(cancelled)
	}
	return j, err
}

// TestProcessorRefreshCancelledBeforeMark: a refresh cancelled after Enqueue returned it but
// before its events were marked is not left holding them: the events stay unprocessed and their
// items get a new refresh.
func TestProcessorRefreshCancelledBeforeMark(t *testing.T) {
	h := newProcHarness(t)
	ce := &cancellingEnqueuer{storeEnqueuer: h.enq, h: h, cancel: true}
	h.proc = NewProcessor(ProcessorOptions{Store: h.store, Enqueuer: ce, Now: func() time.Time { return h.now },
		Quiet: DefaultQuiet, Cap: DefaultCap, DeleteDelay: DefaultDeleteDelay, UpgradeHold: DefaultUpgradeHold})
	ev := h.receive(integrations.TypeRadarr, h.radarr, `{"eventType":"Download","movie":{"id":3}}`)
	h.at(5 * time.Second)
	rs := h.refreshes()
	if len(rs) != 1 {
		t.Fatalf("refreshes = %+v", rs)
	}
	cancelled := rs[0].ID
	if e := h.event(ev.ID); e.ProcessedAt != nil || e.JobID != nil {
		t.Fatalf("the event was marked into the cancelled refresh %d: %+v", cancelled, e)
	}
	if h.proc.Backlog() != 1 {
		t.Fatalf("backlog = %d", h.proc.Backlog())
	}
	h.at(10 * time.Second)
	rs = h.refreshes()
	if len(rs) != 2 || !slices.Equal(rs[1].Params.ArrItemIDs, []int64{3}) {
		t.Fatalf("refreshes after the cancel = %+v", rs)
	}
	if e := h.event(ev.ID); e.JobID == nil || *e.JobID != rs[1].ID || outcomeOf(e) != OutcomeQueued {
		t.Fatalf("event after the new flush = %+v", e)
	}
}

// TestProcessorStartRearmsEventsOfCancelledRefresh: a refresh cancelled while the process stops
// before its OnFinish hook re-armed the events: the next start re-arms them and their items get a
// new refresh.
func TestProcessorStartRearmsEventsOfCancelledRefresh(t *testing.T) {
	h := newProcHarness(t)
	ev := h.receive(integrations.TypeRadarr, h.radarr, `{"eventType":"Download","movie":{"id":3}}`)
	other := h.receive(integrations.TypeRadarr, h.radarr, `{"eventType":"Download","movie":{"id":4}}`)
	h.at(5 * time.Second)
	rs := h.refreshes()
	if len(rs) != 1 {
		t.Fatalf("refreshes = %+v", rs)
	}
	// A completed refresh keeps its events; the cancelled one (no hook ran) gives them back.
	h.finishJobs()
	kept := h.receive(integrations.TypeRadarr, h.radarr, `{"eventType":"Download","movie":{"id":5}}`)
	h.at(10 * time.Second)
	rs = h.refreshes()
	if len(rs) != 2 {
		t.Fatalf("refreshes = %+v", rs)
	}
	h.finishJobs()
	cancelJob(t, h.db, rs[0].ID)
	h.proc = h.newProcessor()
	if err := h.proc.Start(h.ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.proc.Stop(context.Background()) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if e := h.event(ev.ID); e.JobID != nil && *e.JobID != rs[0].ID {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	all := h.refreshes()
	if len(all) != 3 || !slices.Equal(all[2].Params.ArrItemIDs, []int64{3, 4}) {
		t.Fatalf("refreshes after the restart = %+v", all)
	}
	for _, id := range []int64{ev.ID, other.ID} {
		if e := h.event(id); e.JobID == nil || *e.JobID != all[2].ID || outcomeOf(e) != OutcomeQueued {
			t.Fatalf("event %d after the restart = %+v", id, e)
		}
	}
	if e := h.event(kept.ID); e.JobID == nil || *e.JobID != rs[1].ID || outcomeOf(e) != OutcomeQueued {
		t.Fatalf("the event of the completed refresh = %+v", e)
	}
}

// TestStoreBudgetPruneHysteresis: once the payloads exceed their budget, the oldest events are
// pruned down to 90% of it, so the next prune comes only after a tenth of the budget arrived
// again (not on every insert), and the stored total is kept without reading every payload again.
func TestStoreBudgetPruneHysteresis(t *testing.T) {
	h := newProcHarness(t)
	body := string(fixture(t, integrations.TypeRadarr, "Download.json"))
	first := h.receive(integrations.TypeRadarr, h.radarr, body)
	size := int64(len(h.event(first.ID).Payload))
	h.store.maxBytes = 100 * size
	count := func() int64 {
		t.Helper()
		page, err := h.store.List(h.ctx, Query{PageSize: 1})
		if err != nil {
			t.Fatal(err)
		}
		return page.TotalRecords
	}
	prunes, prev := 0, count()
	for range 300 {
		h.receive(integrations.TypeRadarr, h.radarr, body)
		h.proc.step(h.ctx, h.now)
		n := count()
		if n > 100 {
			t.Fatalf("%d events are stored, over the budget of 100", n)
		}
		if n <= prev { // the insert did not add a row: a prune ran
			prunes++
			if n > 90 {
				t.Fatalf("a budget prune kept %d events, want at most 90 (90%% of the budget)", n)
			}
		}
		prev = n
	}
	if prunes == 0 || prunes > 25 {
		t.Fatalf("%d budget prunes for 300 inserts; want a few (one per tenth of the budget)", prunes)
	}
	var total int64
	if err := h.db.Reader().QueryRow(`SELECT SUM(length(CAST(payload AS BLOB))) FROM webhook_events`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	h.store.mu.Lock()
	tracked, known, measures := h.store.bytes, h.store.bytesKnown, h.store.measures
	h.store.mu.Unlock()
	if !known || tracked != total {
		t.Fatalf("tracked payload bytes = %d (known %v), stored %d", tracked, known, total)
	}
	if measures != 1 {
		t.Fatalf("the payloads were measured %d times; want once", measures)
	}
}

// TestProcessorBudgetPruneRetriedWithoutEvents: the payloads exceed their budget and the budget
// prune fails (a disk error) while no item is pending; the step asks to run again after
// RetryDelay, and that retry prunes the table, so it does not stay over budget until the next
// event or the hourly prune.
func TestProcessorBudgetPruneRetriedWithoutEvents(t *testing.T) {
	h := newProcHarness(t)
	h.store.maxBytes = 1
	exec := func(q string) {
		t.Helper()
		if err := h.db.Write(h.ctx, func(tx *sql.Tx) error { _, err := tx.Exec(q); return err }); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE TRIGGER webhook_events_no_delete BEFORE DELETE ON webhook_events BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`)
	h.receive(integrations.TypeRadarr, h.radarr, `{"eventType":"Test","movie":{"id":1}}`) // no item to schedule
	if !h.store.overBudget() {
		t.Fatal("the insert did not take the payloads over their budget")
	}
	next := h.proc.step(h.ctx, h.now)
	if want := h.now.Add(h.proc.o.RetryDelay); !next.Equal(want) {
		t.Fatalf("after a failed budget prune, next wake-up = %v; want %v (now + RetryDelay)", next, want)
	}
	exec(`DROP TRIGGER webhook_events_no_delete`)
	h.now = next
	if next := h.proc.step(h.ctx, h.now); !next.IsZero() {
		t.Fatalf("after the retried prune, next wake-up = %v; want none", next)
	}
	if h.store.overBudget() {
		t.Fatal("the retried budget prune left the payloads over their budget")
	}
}
