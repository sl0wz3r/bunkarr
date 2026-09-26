package webhooks

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// t0 is the fake clock's start.
var t0 = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

func openDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "bunkarr.db"), nil)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// addIntegration inserts an *arr integration row and returns its id.
func addIntegration(t *testing.T, d *db.DB, typ integrations.Type, name string, enabled bool) int64 {
	t.Helper()
	var id int64
	err := d.Write(context.Background(), func(tx *sql.Tx) error {
		return tx.QueryRow(`INSERT INTO integrations (type, name, url, enabled, created_at, updated_at) VALUES (?, ?, 'http://arr', ?, ?, ?) RETURNING id`,
			string(typ), name, enabled, db.FormatTime(t0), db.FormatTime(t0)).Scan(&id)
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func newJobStore(d *db.DB) *jobqueue.Store { return jobqueue.NewStore(d) }

// storeEnqueuer queues jobs through a jobqueue store (the manager's coalescing, no runner), and
// can be told to fail.
type storeEnqueuer struct {
	st *jobqueue.Store

	mu    sync.Mutex
	fails int
	specs []jobs.Spec
}

func (e *storeEnqueuer) Enqueue(ctx context.Context, spec jobs.Spec) (jobs.Job, error) {
	e.mu.Lock()
	e.specs = append(e.specs, spec)
	if e.fails > 0 {
		e.fails--
		e.mu.Unlock()
		return jobs.Job{}, errors.New("database is locked")
	}
	e.mu.Unlock()
	j, _, err := e.st.CreateJob(ctx, spec)
	return j, err
}

func (e *storeEnqueuer) calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.specs)
}

// procHarness is a store, a job store and a processor on a fake clock, driven by step.
type procHarness struct {
	t     *testing.T
	ctx   context.Context
	db    *db.DB
	store *Store
	jobs  *jobqueue.Store
	enq   *storeEnqueuer
	proc  *Processor
	now   time.Time
	// radarr, sonarr are integration ids.
	radarr, sonarr int64
}

func newProcHarness(t *testing.T) *procHarness {
	t.Helper()
	d := openDB(t)
	h := &procHarness{t: t, ctx: context.Background(), db: d, store: NewStore(d), jobs: jobqueue.NewStore(d), now: t0}
	h.store.now = func() time.Time { return h.now }
	h.enq = &storeEnqueuer{st: h.jobs}
	h.radarr = addIntegration(t, d, integrations.TypeRadarr, "Radarr", true)
	h.sonarr = addIntegration(t, d, integrations.TypeSonarr, "Sonarr", true)
	h.proc = h.newProcessor()
	return h
}

// newProcessor returns a processor over the harness's store (a restart makes a new one).
func (h *procHarness) newProcessor() *Processor {
	return NewProcessor(ProcessorOptions{Store: h.store, Enqueuer: h.enq, Now: func() time.Time { return h.now },
		Quiet: DefaultQuiet, Cap: DefaultCap, DeleteDelay: DefaultDeleteDelay, UpgradeHold: DefaultUpgradeHold})
}

// receive parses body as an event of app, stores it at the harness's clock and notifies the
// processor.
func (h *procHarness) receive(app integrations.Type, integ int64, body string) Record {
	h.t.Helper()
	ev, err := Parse(app, strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	rec, err := h.store.Insert(h.ctx, integ, app, ev, h.now)
	if err != nil {
		h.t.Fatal(err)
	}
	h.proc.Notify(rec)
	return rec
}

// at moves the clock to t0+d and runs one processor step.
func (h *procHarness) at(d time.Duration) {
	h.now = t0.Add(d)
	h.proc.step(h.ctx, h.now)
}

// refreshes returns the refresh jobs, oldest first.
func (h *procHarness) refreshes() []jobs.Job {
	h.t.Helper()
	page, err := h.jobs.ListJobs(h.ctx, jobqueue.JobQuery{Type: jobs.TypeRefresh, PageSize: 100})
	if err != nil {
		h.t.Fatal(err)
	}
	out := page.Records
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// event reads a stored event.
func (h *procHarness) event(id int64) Record {
	h.t.Helper()
	r, err := h.store.Get(h.ctx, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}
