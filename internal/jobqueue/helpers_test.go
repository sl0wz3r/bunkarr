package jobqueue

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

const waitTimeout = 10 * time.Second

func openDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "bunkarr.db"), nil)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// newManager returns a manager that is stopped when the test ends.
func newManager(t *testing.T, d *db.DB, o Options) *Manager {
	t.Helper()
	m := New(d, nil, o)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
		defer cancel()
		_ = m.Stop(ctx)
	})
	return m
}

func start(t *testing.T, m *Manager) {
	t.Helper()
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
}

func stop(t *testing.T, m *Manager) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	if err := m.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func enqueue(t *testing.T, m *Manager, spec jobs.Spec) jobs.Job {
	t.Helper()
	j, err := m.Enqueue(context.Background(), spec)
	if err != nil {
		t.Fatalf("Enqueue(%+v): %v", spec, err)
	}
	return j
}

// waitFor polls cond until it holds or the test times out.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitStatus waits until job id has status s and returns it.
func waitStatus(t *testing.T, st *Store, id int64, s jobs.Status) jobs.Job {
	t.Helper()
	var j jobs.Job
	waitFor(t, "job status "+string(s), func() bool {
		var err error
		j, err = st.GetJob(context.Background(), id)
		if err != nil {
			t.Fatalf("GetJob(%d): %v", id, err)
		}
		return j.Status == s
	})
	return j
}

// receive waits for a value on ch.
func receive[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(waitTimeout):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

// execSQL runs a statement through the writer (test setup only).
func execSQL(t *testing.T, d *db.DB, q string, args ...any) {
	t.Helper()
	if err := d.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(context.Background(), q, args...)
		return err
	}); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func isValidation(err error) bool {
	var ve ValidationError
	return errors.As(err, &ve)
}

// hookRecorder counts OnFinish calls per job.
type hookRecorder struct {
	mu    sync.Mutex
	calls map[int64]int
	last  map[int64]jobs.Job
	ch    chan jobs.Job
}

func newHookRecorder(m *Manager) *hookRecorder {
	h := &hookRecorder{calls: map[int64]int{}, last: map[int64]jobs.Job{}, ch: make(chan jobs.Job, 100)}
	m.OnFinish(func(j jobs.Job) {
		h.mu.Lock()
		h.calls[j.ID]++
		h.last[j.ID] = j
		h.mu.Unlock()
		h.ch <- j
	})
	return h
}

func (h *hookRecorder) lastStatus(id int64) jobs.Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.last[id].Status
}

func (h *hookRecorder) count(id int64) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls[id]
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func jsonLogger(w *syncBuffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// manualClock is a settable Now for the manager.
type manualClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *manualClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}
