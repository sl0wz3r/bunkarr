//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// apiLog is a job log line.
type apiLog struct {
	Level   string `json:"level"`
	Message string `json:"message"`
}

// jobLogs returns up to 1000 log lines of a job.
func (s *client) jobLogs(jobID int64) []apiLog {
	s.t.Helper()
	var out []apiLog
	s.call(http.StatusOK, "GET", fmt.Sprintf("/jobs/%d/logs?limit=1000", jobID), nil, &out)
	return out
}

// appriseCall is one request a fake Apprise API received.
type appriseCall struct {
	Path  string
	URLs  string `json:"urls"`
	Title string `json:"title"`
	Body  string `json:"body"`
	Type  string `json:"type"`
}

// fakeApprise is an Apprise API (stateless POST /notify) that records what it receives.
type fakeApprise struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls []appriseCall
}

// newFakeApprise starts a fake Apprise API on 127.0.0.1, closed when the test ends.
func newFakeApprise(t *testing.T) *fakeApprise {
	t.Helper()
	f := &fakeApprise{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var c appriseCall
		if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&c) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		c.Path = r.URL.Path
		f.mu.Lock()
		f.calls = append(f.calls, c)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// received returns the calls so far.
func (f *fakeApprise) received() []appriseCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]appriseCall(nil), f.calls...)
}

// waitFor waits until n calls arrived (notifications are sent in the background).
func (f *fakeApprise) waitFor(t *testing.T, n int, timeout time.Duration) []appriseCall {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if got := f.received(); len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fake Apprise API received %d notifications within %s, want %d: %+v", len(f.received()), timeout, n, f.received())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestMassChangeGuard is design §9 E2E (7), safety rule S10b: deleting more than 10 % (and more
// than 20) of a source's files holds the deletions — nothing moves, everything else runs, the job
// ends completed_with_warnings and a warning notification says so — until a sync with
// allowChanges applies them. A dry run reports what would be held (and notifies nothing).
func TestMassChangeGuard(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	s := e.srv
	// 23 of 220 files is 10.5 %: above the default 10 % and above 20 files.
	const files, deleted, perDir = 220, 23, 20
	srcDir := e.dir("media/clips")
	total := writeFlat(t, srcDir, "Clips", files, perDir, 2000)
	target := e.target()
	src := s.createSource("Clips", srcDir)
	dest := s.createDestination("NAS", target, src.ID)
	folder := src.DestFolder

	// Warnings and failures go to a (fake) Apprise API; the Apprise URLs are write-only (S8).
	apprise := newFakeApprise(t)
	const appriseURLs = "json://127.0.0.1:9/e2e-guard-hook"
	var raw json.RawMessage
	s.call(http.StatusCreated, "POST", "/notifications", map[string]any{"name": "E2E", "kind": "apprise", "apiUrl": apprise.srv.URL,
		"urls": appriseURLs, "onFailure": true, "onWarning": true, "onSuccess": false}, &raw)
	if strings.Contains(string(raw), "e2e-guard-hook") || !strings.Contains(string(raw), `"hasUrls":true`) {
		t.Fatalf("created notification: %s (want hasUrls and no urls)", raw)
	}

	j := s.runSync(dest.ID, nil)
	requireStatus(t, j, "completed")
	requireSyncStats(t, j, false, wantSync{planned: files, copied: files, updated: 0, moved: 0, linked: 0, retained: 0, held: 0,
		failed: 0, bytesPlanned: total, bytesCopied: total})

	var gone []string
	for i := range deleted {
		rel := flatName("Clips", i*9, perDir)
		if err := os.Remove(filepath.Join(srcDir, filepath.FromSlash(rel))); err != nil {
			t.Fatal(err)
		}
		gone = append(gone, rel)
	}
	const added = "Clips/new/new-clip.bin"
	writeFile(t, srcDir, added, content(added, 1, 3000))

	// The preview shows the hold.
	before := snapshot(t, target)
	dry := s.runSync(dest.ID, map[string]any{"dryRun": true})
	requireStatus(t, dry, "completed_with_warnings")
	requireSyncStats(t, dry, true, wantSync{planned: deleted + 1, copied: 1, updated: 0, moved: 0, linked: 0, retained: 0,
		held: deleted, failed: 0, bytesPlanned: 3000, bytesCopied: 0})
	requireUnchanged(t, "a dry run", before, target)

	// The sync copies the new file and holds every deletion.
	j = s.runSync(dest.ID, nil)
	requireStatus(t, j, "completed_with_warnings")
	requireSyncStats(t, j, false, wantSync{planned: deleted + 1, copied: 1, updated: 0, moved: 0, linked: 0, retained: 0,
		held: deleted, failed: 0, bytesPlanned: 3000, bytesCopied: 3000})
	held := s.items(j.ID, "status=held")
	if len(held) != deleted {
		t.Fatalf("held items: %d, want %d", len(held), deleted)
	}
	for _, it := range held {
		if it.Action != "retain" {
			t.Fatalf("held item %+v is not a retain", it)
		}
	}
	for _, rel := range gone {
		if _, err := os.Stat(filepath.Join(target, folder, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("a held deletion moved %s: %v", rel, err)
		}
	}
	if m := retained(t, target, folder, gone[0]); len(m) != 0 {
		t.Fatalf("a held deletion is in retention: %v", m)
	}
	warned := false
	for _, l := range s.jobLogs(j.ID) {
		if l.Level == "warn" && strings.Contains(l.Message, "held") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("the job log has no warning about the held changes: %+v", s.jobLogs(j.ID))
	}
	// One warning notification: for the guarded sync, not for the dry run before it.
	calls := apprise.waitFor(t, 1, 30*time.Second)
	c := calls[0]
	if len(calls) != 1 || c.Path != "/notify" || c.Type != "warning" || c.URLs != appriseURLs ||
		!strings.Contains(c.Body, fmt.Sprintf("%d held by the mass-change guard", deleted)) || !strings.Contains(c.Body, fmt.Sprintf("Job: #%d", j.ID)) {
		t.Fatalf("notifications: %+v, want one warning about job %d's %d held changes", calls, j.ID, deleted)
	}

	// "Apply held changes".
	j = s.runSync(dest.ID, map[string]any{"allowChanges": true})
	requireStatus(t, j, "completed")
	requireSyncStats(t, j, false, wantSync{planned: deleted, copied: 0, updated: 0, moved: 0, linked: 0, retained: deleted,
		held: 0, failed: 0, bytesPlanned: 0, bytesCopied: 0})
	verifyMirror(t, srcDir, target, folder)
	for _, rel := range gone {
		if m := retained(t, target, folder, rel); len(m) != 1 {
			t.Fatalf("retained copies of %s: %v, want one", rel, m)
		}
	}
	// A completed sync notifies nobody here (onSuccess is off).
	time.Sleep(time.Second)
	if calls := apprise.received(); len(calls) != 1 {
		t.Fatalf("notifications after the completed sync: %+v", calls)
	}
}
