package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/config"
	"github.com/sl0wz3r/bunkarr/internal/db"
)

// testKeyring returns a keyring with a fixed master key derived from seed.
func testKeyring(t *testing.T, seed byte) *config.Keyring {
	t.Helper()
	master := bytes.Repeat([]byte{seed}, 32)
	kr, err := config.NewKeyring(master)
	if err != nil {
		t.Fatal(err)
	}
	return kr
}

// openStore opens a migrated database in a temp dir and returns a store with a fixed clock.
func openStore(t *testing.T) (*Store, *db.DB, *config.Keyring) {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "bunkarr.db"), nil)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	kr := testKeyring(t, 7)
	s := NewStore(d, kr)
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	return s, d, kr
}

// testClient is a Client with short timings.
func testClient() *Client {
	c := NewClient()
	c.timeout = 5 * time.Second
	c.retryDelay = 10 * time.Millisecond
	return c
}

// received is one request the fake Apprise API got.
type received struct {
	Method      string
	Path        string
	ContentType string
	UserAgent   string
	Body        map[string]any
}

// fakeOpts configures a fake Apprise API.
type fakeOpts struct {
	// statuses are answered in order; 200 once they run out.
	statuses []int
	// block, when set, holds every request until it is closed (or the client gives up).
	block chan struct{}
	// location is sent with 3xx statuses.
	location string
	// respBody is written with every response (e.g. to check it never reaches an error).
	respBody string
}

// fakeApprise records requests and answers with scripted statuses.
type fakeApprise struct {
	srv  *httptest.Server
	opts fakeOpts
	mu   sync.Mutex
	reqs []received
	next int
}

func newFakeApprise(t *testing.T, o fakeOpts) *fakeApprise {
	t.Helper()
	f := &fakeApprise{opts: o}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(func() {
		if o.block != nil {
			select {
			case <-o.block:
			default:
				close(o.block)
			}
		}
		f.srv.Close()
	})
	return f
}

func (f *fakeApprise) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	rec := received{Method: r.Method, Path: r.URL.Path, ContentType: r.Header.Get("Content-Type"), UserAgent: r.UserAgent()}
	_ = json.Unmarshal(raw, &rec.Body)
	f.mu.Lock()
	f.reqs = append(f.reqs, rec)
	status := http.StatusOK
	if f.next < len(f.opts.statuses) {
		status = f.opts.statuses[f.next]
		f.next++
	}
	f.mu.Unlock()
	if f.opts.block != nil {
		select {
		case <-f.opts.block:
		case <-r.Context().Done():
			return
		}
	}
	if status >= 300 && status < 400 && f.opts.location != "" {
		w.Header().Set("Location", f.opts.location)
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, f.opts.respBody)
}

// URL is the fake's base URL.
func (f *fakeApprise) URL() string { return f.srv.URL }

// requests returns a copy of what was received so far.
func (f *fakeApprise) requests() []received {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]received(nil), f.reqs...)
}

// reset forgets the recorded requests and the scripted statuses.
func (f *fakeApprise) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs, f.next, f.opts.statuses = nil, 0, nil
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// testLogger logs JSON to a buffer WITHOUT the logging package's redaction, so tests see exactly
// what this package puts into log records.
func testLogger() (*slog.Logger, *syncBuffer) {
	var buf syncBuffer
	return slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}
