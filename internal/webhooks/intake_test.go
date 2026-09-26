package webhooks

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
)

// intakeHarness is an intake over the processor harness, served by a test HTTP server that calls
// Serve for integration h.radarr (the credential checks are internal/api's).
type intakeHarness struct {
	*procHarness
	in  *Intake
	srv *httptest.Server
}

func newIntakeHarness(t *testing.T, o IntakeOptions) *intakeHarness {
	t.Helper()
	h := &intakeHarness{procHarness: newProcHarness(t)}
	o.Store, o.Processor = h.store, h.proc
	if o.Now == nil {
		o.Now = func() time.Time { return h.now }
	}
	h.in = NewIntake(o)
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.in.Serve(w, r, h.radarr, integrations.TypeRadarr)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

// post sends a body with a content type and returns the status and answer.
func (h *intakeHarness) post(ctype string, body io.Reader) (int, string) {
	h.t.Helper()
	req, err := http.NewRequest("POST", h.srv.URL+"/radarr?apikey=0123456789abcdef0123456789abcdef", body)
	if err != nil {
		h.t.Fatal(err)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	req.Header.Set("X-Api-Key", "0123456789abcdef0123456789abcdef")
	req.SetBasicAuth("radarr", "0123456789abcdef0123456789abcdef")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func TestIntakeStoresAndAnswers(t *testing.T) {
	h := newIntakeHarness(t, IntakeOptions{})
	code, body := h.post("application/json; charset=utf-8", bytes.NewReader(fixture(t, integrations.TypeRadarr, "Download.json")))
	if code != 200 || strings.TrimSpace(body) != "{}" {
		t.Fatalf("Download = %d %s", code, body)
	}
	if h.proc.Backlog() != 1 {
		t.Fatalf("backlog = %d", h.proc.Backlog())
	}
	// A Test event is answered, recorded as test and processed at once: nothing is looked up.
	code, _ = h.post("application/json", bytes.NewReader(fixture(t, integrations.TypeRadarr, "Test.json")))
	if code != 200 || h.proc.Backlog() != 1 {
		t.Fatalf("Test = %d backlog %d", code, h.proc.Backlog())
	}
	page, err := h.store.List(h.ctx, Query{Outcome: "test"})
	if err != nil || page.TotalRecords != 1 || page.Records[0].ProcessedAt == nil {
		t.Fatalf("test events = %+v, %v", page, err)
	}
	h.at(10 * time.Second)
	if n := len(h.enq.specs); n != 1 {
		t.Fatalf("enqueues = %d (the Test event must queue nothing)", n)
	}
	// Neither the header, nor the query, nor the Basic credential is stored anywhere in the row.
	var dump strings.Builder
	rows, err := h.db.Reader().Query(`SELECT * FROM webhook_events`)
	if err != nil {
		t.Fatal(err)
	}
	cols, _ := rows.Columns()
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		for _, v := range vals {
			dump.WriteString(strings.ToLower(stringOf(v)) + "|")
		}
	}
	_ = rows.Close()
	for _, secret := range []string{"0123456789abcdef0123456789abcdef", "x-api-key", "apikey", "127.0.0.1", "authorization"} {
		if strings.Contains(dump.String(), secret) {
			t.Fatalf("the stored events contain %q", secret)
		}
	}
}

func stringOf(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case []byte:
		return string(x)
	}
	return fmt.Sprint(v)
}

func TestIntakeRefusals(t *testing.T) {
	h := newIntakeHarness(t, IntakeOptions{})
	for _, tc := range []struct {
		name  string
		ctype string
		body  io.Reader
		want  int
	}{
		{"no content type", "", strings.NewReader(`{"eventType":"Test"}`), http.StatusUnsupportedMediaType},
		{"form", "application/x-www-form-urlencoded", strings.NewReader(`eventType=Test`), http.StatusUnsupportedMediaType},
		{"not json", "application/json", strings.NewReader(`eventType=Test`), http.StatusBadRequest},
		{"no eventType", "application/json", strings.NewReader(`{"movie":{"id":1}}`), http.StatusBadRequest},
		{"declared too large", "application/json", bytes.NewReader(make([]byte, MaxBody+1)), http.StatusRequestEntityTooLarge},
		// Chunked, no Content-Length: the stream is cut at 16 MiB.
		{"streamed too large", "application/json", io.MultiReader(strings.NewReader(`{"eventType":"Rename","overview":"`),
			strings.NewReader(strings.Repeat("x", MaxBody)), strings.NewReader(`"}`)), http.StatusRequestEntityTooLarge},
	} {
		code, body := h.post(tc.ctype, tc.body)
		if code != tc.want {
			t.Errorf("%s: %d %s, want %d", tc.name, code, body, tc.want)
		}
	}
	if page, _ := h.store.List(h.ctx, Query{}); page.TotalRecords != 0 {
		t.Fatalf("refused requests stored %d events", page.TotalRecords)
	}
}

func TestIntakeRateAndBacklog(t *testing.T) {
	clock := t0
	var mu sync.Mutex
	h := newIntakeHarness(t, IntakeOptions{Burst: 3, Rate: 1, MaxBacklog: 5, Now: func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	}})
	body := string(fixture(t, integrations.TypeRadarr, "Download.json"))
	for i := range 3 {
		if code, _ := h.post("application/json", strings.NewReader(body)); code != 200 {
			t.Fatalf("event %d = %d", i, code)
		}
	}
	if code, _ := h.post("application/json", strings.NewReader(body)); code != http.StatusTooManyRequests {
		t.Fatalf("beyond the burst = %d, want 429", code)
	}
	mu.Lock()
	clock = clock.Add(2 * time.Second)
	mu.Unlock()
	for range 2 {
		if code, _ := h.post("application/json", strings.NewReader(body)); code != 200 {
			t.Fatalf("after 2 s = %d", code)
		}
	}
	// Five events are unprocessed: the intake answers 503 until the processor catches up.
	mu.Lock()
	clock = clock.Add(time.Hour)
	mu.Unlock()
	if code, _ := h.post("application/json", strings.NewReader(body)); code != http.StatusServiceUnavailable {
		t.Fatalf("with a full backlog = %d, want 503", code)
	}
	h.at(time.Hour)
	if code, _ := h.post("application/json", strings.NewReader(body)); code != 200 {
		t.Fatalf("after the flush = %d", code)
	}
}

// blockingReader blocks the first Read until release is closed.
type blockingReader struct {
	started chan struct{}
	release chan struct{}
	r       io.Reader
	once    sync.Once
}

func (b *blockingReader) Read(p []byte) (int, error) {
	b.once.Do(func() {
		close(b.started)
		<-b.release
	})
	return b.r.Read(p)
}

// TestIntakeBodySlots: a request waits at most SlotWait for one of the Slots body readers.
func TestIntakeBodySlots(t *testing.T) {
	h := newIntakeHarness(t, IntakeOptions{Slots: 1, SlotWait: 100 * time.Millisecond})
	slow := &blockingReader{started: make(chan struct{}), release: make(chan struct{}),
		r: bytes.NewReader(fixture(t, integrations.TypeRadarr, "Download.json"))}
	done := make(chan int)
	go func() {
		req, _ := http.NewRequest("POST", h.srv.URL, slow)
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- 0
			return
		}
		_ = res.Body.Close()
		done <- res.StatusCode
	}()
	<-slow.started
	time.Sleep(50 * time.Millisecond) // the slow request holds the only slot
	if code, _ := h.post("application/json", bytes.NewReader(fixture(t, integrations.TypeRadarr, "Download.json"))); code != http.StatusServiceUnavailable {
		t.Fatalf("while the slot is taken = %d, want 503", code)
	}
	close(slow.release)
	if code := <-done; code != 200 {
		t.Fatalf("slow request = %d", code)
	}
	if code, _ := h.post("application/json", bytes.NewReader(fixture(t, integrations.TypeRadarr, "Download.json"))); code != 200 {
		t.Fatalf("after the slot is free = %d", code)
	}
}

// TestIntakeCrashAfterStore: a crash after the event is stored and before the processor hears of
// it loses nothing: the restarted processor reads it from the table.
func TestIntakeCrashAfterStore(t *testing.T) {
	h := newIntakeHarness(t, IntakeOptions{})
	req := httptest.NewRequest("POST", "/radarr", bytes.NewReader(fixture(t, integrations.TypeRadarr, "Download.json")))
	req.Header.Set("Content-Type", "application/json")
	func() {
		faultinject.SetHook(faultinject.CrashAt(PointAfterStore, 1))
		defer faultinject.SetHook(nil)
		defer func() {
			if p := recover(); p != nil {
				if _, ok := p.(faultinject.Crash); !ok {
					panic(p)
				}
			}
		}()
		h.in.Serve(httptest.NewRecorder(), req, h.radarr, integrations.TypeRadarr)
		t.Fatal("did not crash")
	}()
	if h.proc.Backlog() != 0 {
		t.Fatal("the processor heard of the event")
	}
	h.proc = h.newProcessor()
	if err := h.proc.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.proc.Stop(context.Background()) }()
	if h.proc.Backlog() != 1 {
		t.Fatalf("backlog after the restart = %d", h.proc.Backlog())
	}
}

func TestIntakeGenericRouteUses(t *testing.T) {
	clock := t0
	in := NewIntake(IntakeOptions{Store: NewStore(openDB(t)), Processor: NewProcessor(ProcessorOptions{}), Now: func() time.Time { return clock }})
	in.NoteGenericRoute(3)
	clock = clock.Add(6 * 24 * time.Hour)
	in.NoteGenericRoute(4)
	if got := in.GenericRouteUses(); len(got) != 2 {
		t.Fatalf("uses = %v", got)
	}
	clock = clock.Add(2 * 24 * time.Hour)
	if got := in.GenericRouteUses(); len(got) != 1 || got[4].IsZero() {
		t.Fatalf("uses after 8 days = %v", got)
	}
}

// TestIntakeStoredEventIsProcessedWhenThePruneFails: the payload budget is exceeded and the prune
// fails (a disk error); the event is stored all the same, so the intake answers 200 and the
// processor hears of it at once (the prune is retried off the request path).
func TestIntakeStoredEventIsProcessedWhenThePruneFails(t *testing.T) {
	h := newIntakeHarness(t, IntakeOptions{})
	h.store.maxBytes = 1
	err := h.db.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER webhook_events_no_delete BEFORE DELETE ON webhook_events BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	code, body := h.post("application/json", bytes.NewReader(fixture(t, integrations.TypeRadarr, "Download.json")))
	if code != 200 {
		t.Fatalf("Download with a failing prune = %d %s", code, body)
	}
	if h.proc.Backlog() != 1 {
		t.Fatalf("backlog = %d: the processor was not told", h.proc.Backlog())
	}
	h.at(10 * time.Second)
	if n := len(h.refreshes()); n != 1 {
		t.Fatalf("refreshes = %d", n)
	}
}
