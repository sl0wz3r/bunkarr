package manifest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// signalRunner is a runner that closes ran when it runs.
type signalRunner struct{ ran chan struct{} }

func (s *signalRunner) Run(context.Context, jobs.Job, jobs.Env) (jobs.Result, error) {
	close(s.ran)
	return jobs.Result{Summary: "ok"}, nil
}

func TestWaitingExportDoesNotHoldAWorker(t *testing.T) {
	// Builds take turns, but a manifest_export job waiting for its turn must not park in one of
	// the manager's general workers (default 2): the dispatcher keeps it queued (lock key
	// jobqueue.ManifestBuildKey), so a verify still starts while another destination's manifest
	// is being built and written, however long that takes on a slow destination.
	e := newEnv(t)
	target2 := filepath.Join(e.base, "target2")
	if err := os.MkdirAll(target2, 0o755); err != nil {
		t.Fatal(err)
	}
	d2, err := e.dests.Create(e.ctx, destinations.Input{Name: "Offsite", Target: target2, SourceIDs: []int64{e.src.ID}},
		destinations.CreateOptions{AllowLocal: true})
	if err != nil {
		t.Fatal(err)
	}
	gate := &gateTiers{entered: make(chan struct{}, 16), release: make(chan struct{})}
	released := false
	defer func() {
		if !released {
			close(gate.release)
		}
	}()
	r, err := NewRunner(Options{DB: e.db, Catalog: e.cat, Integrations: e.ints, Destinations: e.dests, Index: e.index.Store(),
		Tiers: gate, ConfigDir: e.config, Now: e.clock.Now, Location: time.UTC, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	m := jobqueue.New(e.db, nil, jobqueue.Options{Workers: 2})
	m.Register(jobs.TypeManifestExport, r)
	verify := &signalRunner{ran: make(chan struct{})}
	m.Register(jobs.TypeVerify, verify)
	if err := m.Start(e.ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	}()
	enqueue := func(typ jobs.Type, destID int64) jobs.Job {
		t.Helper()
		j, err := m.Enqueue(e.ctx, jobs.Spec{Type: typ, Trigger: jobs.TriggerManual, Params: jobs.Params{DestinationID: destID}})
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	status := func(id int64) jobs.Status {
		t.Helper()
		j, err := m.Get(e.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return j.Status
	}

	a := enqueue(jobs.TypeManifestExport, e.dest.ID)
	select {
	case <-gate.entered: // A is building (its destination is slow)
	case <-time.After(10 * time.Second):
		t.Fatal("the first manifest job never started building")
	}
	b := enqueue(jobs.TypeManifestExport, d2.ID)
	time.Sleep(300 * time.Millisecond)
	if st := status(b.ID); st != jobs.StatusQueued {
		t.Fatalf("the second manifest job is %s while the first builds; want it queued", st)
	}
	enqueue(jobs.TypeVerify, d2.ID)
	select {
	case <-verify.ran:
	case <-time.After(5 * time.Second):
		t.Fatalf("a verify did not start while a manifest was building (second manifest job %s)", status(b.ID))
	}

	close(gate.release)
	released = true
	deadline := time.Now().Add(20 * time.Second)
	for _, j := range []jobs.Job{a, b} {
		for st := status(j.ID); st != jobs.StatusCompleted && st != jobs.StatusCompletedWithWarnings; st = status(j.ID) {
			if st.Final() || time.Now().After(deadline) {
				t.Fatalf("manifest job %d is %s", j.ID, st)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	gate.mu.Lock()
	peak := gate.peak
	gate.mu.Unlock()
	if peak != 1 {
		t.Fatalf("%d manifests were built at once", peak)
	}
}
