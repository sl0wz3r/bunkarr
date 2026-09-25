package jobqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

func TestCreateJobDedupe(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	st := NewStore(d)

	sync1 := jobs.Spec{Type: jobs.TypeSync, Trigger: jobs.TriggerSchedule, Params: jobs.Params{DestinationID: 4}}
	a, created, err := st.CreateJob(ctx, sync1)
	if err != nil || !created {
		t.Fatalf("CreateJob = %v, created %v", err, created)
	}
	if a.Status != jobs.StatusQueued || a.Attempt != 1 || a.Trigger != jobs.TriggerSchedule || string(a.Stats) != "{}" {
		t.Fatalf("new job = %+v", a)
	}

	// Identical (trigger does not matter) → the queued job.
	b, created, err := st.CreateJob(ctx, jobs.Spec{Type: jobs.TypeSync, Params: jobs.Params{DestinationID: 4}})
	if err != nil || created || b.ID != a.ID {
		t.Fatalf("identical spec: id %d created %v err %v; want %d, false", b.ID, created, err, a.ID)
	}
	// Dry run, other params → new jobs.
	c, created, _ := st.CreateJob(ctx, jobs.Spec{Type: jobs.TypeSync, DryRun: true, Params: jobs.Params{DestinationID: 4}})
	if !created || c.ID == a.ID {
		t.Fatal("a dry run must not dedupe with a real run")
	}
	e, created, _ := st.CreateJob(ctx, jobs.Spec{Type: jobs.TypeSync, Params: jobs.Params{DestinationID: 4, AllowChanges: true}})
	if !created || e.ID == a.ID {
		t.Fatal("allowChanges is part of the params")
	}
	// Source order and duplicates do not matter.
	s1, _, _ := st.CreateJob(ctx, jobs.Spec{Type: jobs.TypeScan, Params: jobs.Params{SourceIDs: []int64{3, 1, 3}}})
	s2, created, _ := st.CreateJob(ctx, jobs.Spec{Type: jobs.TypeScan, Params: jobs.Params{SourceIDs: []int64{1, 3}}})
	if created || s2.ID != s1.ID || !reflect.DeepEqual(s1.Params.SourceIDs, []int64{1, 3}) {
		t.Fatalf("scan dedupe: %+v vs %+v (created %v)", s1, s2, created)
	}

	// A running job is not "queued": an identical request queues a new one.
	if _, ok, err := st.markRunning(ctx, a.ID, time.Now()); !ok || err != nil {
		t.Fatalf("markRunning: %v %v", ok, err)
	}
	f, created, _ := st.CreateJob(ctx, sync1)
	if !created || f.ID == a.ID {
		t.Fatal("a running job must not absorb a new request")
	}

	var dest sql.NullInt64
	if err := d.Reader().QueryRowContext(ctx, `SELECT destination_id FROM jobs WHERE id = ?`, a.ID).Scan(&dest); err != nil || dest.Int64 != 4 {
		t.Fatalf("destination_id = %v, %v; want 4", dest, err)
	}
	if err := d.Reader().QueryRowContext(ctx, `SELECT destination_id FROM jobs WHERE id = ?`, s1.ID).Scan(&dest); err != nil || dest.Valid {
		t.Fatalf("scan destination_id = %v, %v; want NULL", dest, err)
	}
}

func TestCreateJobValidation(t *testing.T) {
	st := NewStore(openDB(t))
	cases := []struct {
		name string
		spec jobs.Spec
	}{
		{"unknown type", jobs.Spec{Type: "rsync"}},
		{"sync without destination", jobs.Spec{Type: jobs.TypeSync}},
		{"verify without destination", jobs.Spec{Type: jobs.TypeVerify}},
		{"backup without integration", jobs.Spec{Type: jobs.TypePlexDBBackup, Params: jobs.Params{DestinationID: 1}}},
		{"scan without sources", jobs.Spec{Type: jobs.TypeScan}},
		{"negative destination", jobs.Spec{Type: jobs.TypeSync, Params: jobs.Params{DestinationID: -1}}},
		{"zero source id", jobs.Spec{Type: jobs.TypeScan, Params: jobs.Params{SourceIDs: []int64{0}}}},
		{"unknown trigger", jobs.Spec{Type: jobs.TypeRetention, Trigger: "cron"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := st.CreateJob(context.Background(), tc.spec); !isValidation(err) {
				t.Fatalf("err = %v; want a ValidationError", err)
			}
		})
	}
	// Global retention needs no destination; manual is the default trigger.
	j, _, err := st.CreateJob(context.Background(), jobs.Spec{Type: jobs.TypeRetention})
	if err != nil || j.Trigger != jobs.TriggerManual {
		t.Fatalf("retention: %+v, %v", j, err)
	}
}

func TestLockKeys(t *testing.T) {
	cases := []struct {
		typ    jobs.Type
		params jobs.Params
		want   []string
	}{
		{jobs.TypeSync, jobs.Params{DestinationID: 3, SourceIDs: []int64{1}}, []string{"dest:3"}},
		{jobs.TypeVerify, jobs.Params{DestinationID: 3}, []string{"dest:3"}},
		{jobs.TypeRetention, jobs.Params{DestinationID: 3}, []string{"dest:3"}},
		{jobs.TypeRetention, jobs.Params{}, nil},
		{jobs.TypePlexDBBackup, jobs.Params{IntegrationID: 7, DestinationID: 3}, []string{"plexdb:7"}},
		{jobs.TypeScan, jobs.Params{SourceIDs: []int64{9, 2, 9}}, []string{"source:2", "source:9"}},
	}
	for _, tc := range cases {
		if got := LockKeys(tc.typ, tc.params); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("LockKeys(%s, %+v) = %v; want %v", tc.typ, tc.params, got, tc.want)
		}
	}
}

func TestListJobsFiltersAndPaging(t *testing.T) {
	ctx := context.Background()
	st := NewStore(openDB(t))
	clock := &manualClock{t: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)}
	st.now = clock.Now

	var ids []int64
	for i := range 5 {
		clock.Add(time.Minute)
		// Distinct sources keep the dedupe from merging these jobs.
		j, _, err := st.CreateJob(ctx, jobs.Spec{Type: jobs.TypeSync, Params: jobs.Params{DestinationID: int64(i%2 + 1), SourceIDs: []int64{int64(i + 1)}}})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, j.ID)
	}
	scan, _, _ := st.CreateJob(ctx, jobs.Spec{Type: jobs.TypeScan, Params: jobs.Params{SourceIDs: []int64{1}}})
	// ids[0] completed, ids[1] failed, ids[2] running; the rest stay queued.
	for i, status := range []jobs.Status{jobs.StatusCompleted, jobs.StatusFailed} {
		if _, ok, _ := st.markRunning(ctx, ids[i], clock.Now()); !ok {
			t.Fatal("markRunning")
		}
		clock.Add(time.Minute)
		if ok, err := st.finishJob(ctx, ids[i], finalState{status: status, stats: `{"n":1}`, at: clock.Now(), progress: []byte("{}")}); !ok || err != nil {
			t.Fatalf("finishJob: %v %v", ok, err)
		}
	}
	st.markRunning(ctx, ids[2], clock.Now())

	list := func(q JobQuery) Page[jobs.Job] {
		t.Helper()
		p, err := st.ListJobs(ctx, q)
		if err != nil {
			t.Fatalf("ListJobs(%+v): %v", q, err)
		}
		return p
	}
	idsOf := func(p Page[jobs.Job]) []int64 {
		out := []int64{}
		for _, j := range p.Records {
			out = append(out, j.ID)
		}
		return out
	}

	if got := idsOf(list(JobQuery{State: StateActive})); !reflect.DeepEqual(got, []int64{ids[2], ids[3], ids[4], scan.ID}) {
		t.Fatalf("active = %v", got)
	}
	fin := list(JobQuery{State: StateFinished})
	if got := idsOf(fin); !reflect.DeepEqual(got, []int64{ids[1], ids[0]}) || fin.TotalRecords != 2 {
		t.Fatalf("finished = %v (total %d); want most recent first", got, fin.TotalRecords)
	}
	if fin.Records[1].FinishedAt == nil || string(fin.Records[1].Stats) != `{"n":1}` {
		t.Fatalf("finished job = %+v", fin.Records[1])
	}
	if got := idsOf(list(JobQuery{Type: jobs.TypeScan})); !reflect.DeepEqual(got, []int64{scan.ID}) {
		t.Fatalf("type scan = %v", got)
	}
	if got := idsOf(list(JobQuery{Status: jobs.StatusFailed})); !reflect.DeepEqual(got, []int64{ids[1]}) {
		t.Fatalf("status failed = %v", got)
	}
	if got := idsOf(list(JobQuery{DestinationID: 2})); !reflect.DeepEqual(got, []int64{ids[3], ids[1]}) {
		t.Fatalf("destination 2 = %v", got)
	}
	p2 := list(JobQuery{Page: 2, PageSize: 4})
	if p2.TotalRecords != 6 || p2.Page != 2 || p2.PageSize != 4 || len(p2.Records) != 2 {
		t.Fatalf("page 2 = %+v", p2)
	}
	if p := list(JobQuery{Page: 9}); len(p.Records) != 0 || p.Records == nil {
		t.Fatalf("empty page must have an empty (non-nil) record list: %+v", p)
	}
	if p := list(JobQuery{PageSize: 100000}); p.PageSize != MaxPageSize {
		t.Fatalf("page size not capped: %d", p.PageSize)
	}
	for _, q := range []JobQuery{{State: "done"}, {Type: "x"}, {Status: "x"}} {
		if _, err := st.ListJobs(ctx, q); !isValidation(err) {
			t.Errorf("ListJobs(%+v) err = %v; want ValidationError", q, err)
		}
	}
	if _, err := st.GetJob(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetJob(999) = %v", err)
	}
}

func TestPruneHistory(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	st := NewStore(d)
	clock := &manualClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	st.now = clock.Now
	st.pruneBatch = 7

	finished := func(at time.Time) int64 {
		t.Helper()
		j, _, err := st.CreateJob(ctx, jobs.Spec{Type: jobs.TypeSync, Params: jobs.Params{DestinationID: at.Unix()}})
		if err != nil {
			t.Fatal(err)
		}
		st.markRunning(ctx, j.ID, at)
		if ok, err := st.finishJob(ctx, j.ID, finalState{status: jobs.StatusCompleted, stats: "{}", at: at, progress: []byte("{}")}); !ok || err != nil {
			t.Fatalf("finishJob: %v %v", ok, err)
		}
		return j.ID
	}
	old := finished(clock.Now())
	old2 := finished(clock.Now().Add(time.Hour))
	items := make([]jobs.Item, 20) // more than two prune batches
	for i := range items {
		items[i] = jobs.Item{RelPath: "f", Action: jobs.ActionCopy}
	}
	if err := st.AddItems(ctx, old, items, true); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := st.AppendLog(ctx, old, slog.LevelInfo, "line", nil); err != nil {
			t.Fatal(err)
		}
	}
	oldQueued, _, _ := st.CreateJob(ctx, jobs.Spec{Type: jobs.TypeRetention})
	oldRunning, _, _ := st.CreateJob(ctx, jobs.Spec{Type: jobs.TypeVerify, Params: jobs.Params{DestinationID: 1}})
	st.markRunning(ctx, oldRunning.ID, clock.Now())
	recent := finished(clock.Now().Add(100 * 24 * time.Hour))

	n, err := st.PruneHistory(ctx, clock.Now().Add(90*24*time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("PruneHistory = %d, %v; want 2", n, err)
	}
	for _, id := range []int64{old, old2} {
		if _, err := st.GetJob(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("old job %d still there: %v", id, err)
		}
	}
	for _, table := range []string{"job_items", "job_logs"} {
		var c int
		if err := d.Reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE job_id = ?`, old).Scan(&c); err != nil || c != 0 {
			t.Fatalf("%s rows left = %d, %v", table, c, err)
		}
	}
	for _, id := range []int64{oldQueued.ID, oldRunning.ID, recent} {
		if _, err := st.GetJob(ctx, id); err != nil {
			t.Fatalf("job %d pruned: %v", id, err)
		}
	}
}

func TestJobJSONShape(t *testing.T) {
	st := NewStore(openDB(t))
	j, _, err := st.CreateJob(context.Background(), jobs.Spec{Type: jobs.TypeSync, Params: jobs.Params{DestinationID: 1}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["stats"].(map[string]any); !ok {
		t.Fatalf("stats must be a JSON object: %s", b)
	}
}

func TestActiveForSourceAndIntegration(t *testing.T) {
	ctx := context.Background()
	st := NewStore(openDB(t))
	check := func(what string, fn func(context.Context, int64) (bool, error), id int64, want bool) {
		t.Helper()
		got, err := fn(ctx, id)
		if err != nil || got != want {
			t.Fatalf("%s(%d) = %v, %v; want %v", what, id, got, err, want)
		}
	}
	src, integ := st.ActiveForSource, st.ActiveForIntegration

	check("ActiveForSource", src, 3, false)
	check("ActiveForIntegration", integ, 5, false)
	scan, _, _ := st.CreateJob(ctx, jobs.Spec{Type: jobs.TypeScan, Params: jobs.Params{SourceIDs: []int64{12, 3}}})
	backup, _, _ := st.CreateJob(ctx, jobs.Spec{Type: jobs.TypePlexDBBackup, DryRun: true, Params: jobs.Params{IntegrationID: 5, DestinationID: 4}})
	// Queued jobs (dry runs too) count; ids are matched exactly, never by another field.
	check("ActiveForSource", src, 3, true)
	check("ActiveForSource", src, 12, true)
	check("ActiveForSource", src, 1, false)
	check("ActiveForSource", src, 2, false)
	check("ActiveForSource", src, 4, false) // the backup's destination
	check("ActiveForIntegration", integ, 5, true)
	check("ActiveForIntegration", integ, 4, false)
	check("ActiveForIntegration", integ, 12, false)

	// Running jobs count; finished ones do not.
	if _, ok, err := st.markRunning(ctx, scan.ID, time.Now()); !ok || err != nil {
		t.Fatalf("markRunning: %v %v", ok, err)
	}
	check("ActiveForSource", src, 3, true)
	if ok, err := st.finishJob(ctx, scan.ID, finalState{status: jobs.StatusCompleted, stats: "{}", progress: []byte("{}"), at: time.Now()}); !ok || err != nil {
		t.Fatalf("finishJob: %v %v", ok, err)
	}
	check("ActiveForSource", src, 3, false)
	if ok, err := st.cancelQueued(ctx, backup.ID, time.Now()); !ok || err != nil {
		t.Fatalf("cancelQueued: %v %v", ok, err)
	}
	check("ActiveForIntegration", integ, 5, false)
}
