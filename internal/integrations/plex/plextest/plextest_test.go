package plextest_test

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/integrations/plex"
	"github.com/sl0wz3r/bunkarr/internal/integrations/plex/plextest"
)

// recordingTB captures Errorf calls instead of failing the test.
type recordingTB struct {
	testing.TB
	mu   sync.Mutex
	errs []string
}

func (r *recordingTB) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

func get(t *testing.T, url string, header map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(body)
}

func TestServerBehavesLikeRecordedPlex(t *testing.T) {
	const token = "fake-server-token-1"
	srv := plextest.NewServer(t, token)
	tests := []struct {
		name       string
		path       string
		header     map[string]string
		wantStatus int
		wantType   string
		wantBody   string
	}{
		{"identity without token", plex.PathIdentity, nil, 200, "application/json", string(plextest.Recorded("identity.json"))},
		{"sections without token", plex.PathSections, nil, 401, "text/html", plextest.UnauthorizedBody},
		{"sections with a wrong token", plex.PathSections, map[string]string{"X-Plex-Token": "bogus-token-for-test"}, 401, "text/html", plextest.UnauthorizedBody},
		{"sections with the token", plex.PathSections, map[string]string{"X-Plex-Token": token}, 200, "application/json", string(plextest.Recorded("sections.json"))},
		{"prefs with the token", plex.PathPrefs, map[string]string{"X-Plex-Token": token}, 200, "application/json", string(plextest.Recorded("prefs.json"))},
		{"unknown path", "/library/sections/99", map[string]string{"X-Plex-Token": token}, 404, "text/html", "404 Not Found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := get(t, srv.URL+tt.path, tt.header)
			if resp.StatusCode != tt.wantStatus || resp.Header.Get("Content-Type") != tt.wantType || !strings.Contains(body, tt.wantBody) {
				t.Fatalf("GET %s = %d %s %q", tt.path, resp.StatusCode, resp.Header.Get("Content-Type"), body)
			}
			if resp.Header.Get("X-Plex-Protocol") != "1.0" {
				t.Fatal("missing X-Plex-Protocol header")
			}
		})
	}
	if n := len(srv.Requests()); n != len(tests) {
		t.Fatalf("Requests recorded %d requests, want %d", n, len(tests))
	}
}

func TestServerFailsTheTestOnTokenInURL(t *testing.T) {
	rtb := &recordingTB{TB: t}
	srv := plextest.NewServer(rtb, "url-token-value-1")
	resp, _ := get(t, srv.URL+plex.PathSections+"?x-plex-token=url-token-value-1", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	rtb.mu.Lock()
	defer rtb.mu.Unlock()
	if len(rtb.errs) != 1 || !strings.Contains(rtb.errs[0], "S8") {
		t.Fatalf("recorded errors = %q, want one S8 failure", rtb.errs)
	}
}

func TestHandleAndOpenServer(t *testing.T) {
	srv := plextest.NewServer(t, "")
	srv.Handle(plex.PathSections, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	if resp, _ := get(t, srv.URL+plex.PathSections, nil); resp.StatusCode != http.StatusTeapot {
		t.Fatalf("custom handler not used: %d", resp.StatusCode)
	}
	srv.Handle(plex.PathSections, nil)
	if resp, _ := get(t, srv.URL+plex.PathSections, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("an empty token must accept every request, and Handle(nil) must restore the recording: %d", resp.StatusCode)
	}
	srv.SetIdentity("abc123", "1.2.3.4-x")
	if _, body := get(t, srv.URL+plex.PathIdentity, nil); !strings.Contains(body, `"machineIdentifier":"abc123"`) {
		t.Fatalf("SetIdentity not served: %s", body)
	}
	if plextest.Recorded("missing.json") != nil {
		t.Fatal("Recorded returned data for an unknown name")
	}
}
