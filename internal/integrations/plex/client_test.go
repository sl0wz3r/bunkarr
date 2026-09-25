package plex_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/integrations/plex"
	"github.com/sl0wz3r/bunkarr/internal/integrations/plex/plextest"
	"github.com/sl0wz3r/bunkarr/internal/logging"
	"github.com/sl0wz3r/bunkarr/internal/version"
)

// newClient returns a client for srv with the given token and options.
func newClient(t *testing.T, baseURL, token string, opts plex.Options) *plex.Client {
	t.Helper()
	c, err := plex.New(baseURL, token, opts)
	if err != nil {
		t.Fatalf("plex.New: %v", err)
	}
	return c
}

func TestPlextestCopiesMatchRecordings(t *testing.T) {
	for _, name := range []string{"identity.json", "sections.json", "sections-empty.json", "prefs.json"} {
		want, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(plextest.Recorded(name), want) {
			t.Errorf("plextest/testdata/%s differs from testdata/%s: copy the recording again", name, name)
		}
	}
	rec, err := os.ReadFile("testdata/unauthorized.txt")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(rec), plextest.UnauthorizedBody); n != 2 {
		t.Errorf("plextest.UnauthorizedBody appears %d times in the recorded 401 answers, want 2", n)
	}
}

func TestIdentitySendsNoToken(t *testing.T) {
	srv := plextest.NewServer(t, "identity-token-123")
	c := newClient(t, srv.URL, "identity-token-123", plex.Options{})
	id, err := c.Identity(context.Background())
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	want := plex.Identity{MachineIdentifier: "e004a7de01c19d82ea3a7fe4bfaa19e23a8344af", Version: "1.43.4.10903-e5521bd8c"}
	if id != want {
		t.Fatalf("Identity = %+v, want %+v", id, want)
	}
	reqs := srv.Requests()
	if len(reqs) != 1 || reqs[0].Path != plex.PathIdentity || reqs[0].Header.Get("X-Plex-Token") != "" {
		t.Fatalf("identity request = %+v; want one request without X-Plex-Token", reqs)
	}
}

func TestRequestHeaders(t *testing.T) {
	const token = "header-token-abcdef"
	tests := []struct {
		name     string
		clientID string
		wantID   string
	}{
		{"default identifier", "", plex.DefaultClientIdentifier},
		{"custom identifier", "bunkarr-7f3e", "bunkarr-7f3e"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := plextest.NewServer(t, token)
			c := newClient(t, srv.URL, token, plex.Options{ClientIdentifier: tt.clientID})
			if _, err := c.Sections(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Prefs(context.Background()); err != nil {
				t.Fatal(err)
			}
			for _, r := range srv.Requests() {
				h := r.Header
				if r.RawQuery != "" {
					t.Errorf("%s sent a query %q", r.Path, r.RawQuery)
				}
				checks := map[string]string{
					"X-Plex-Token":             token,
					"Accept":                   "application/json",
					"X-Plex-Product":           "Bunkarr",
					"X-Plex-Version":           version.Version,
					"X-Plex-Client-Identifier": tt.wantID,
					"User-Agent":               version.UserAgent(),
				}
				for k, v := range checks {
					if got := h.Get(k); got != v {
						t.Errorf("%s header %s = %q, want %q", r.Path, k, got, v)
					}
				}
			}
		})
	}
}

func TestSectionsRecorded(t *testing.T) {
	srv := plextest.NewServer(t, "sections-token-1")
	c := newClient(t, srv.URL, "sections-token-1", plex.Options{})
	got, err := c.Sections(context.Background())
	if err != nil {
		t.Fatalf("Sections: %v", err)
	}
	want := []plex.Section{
		{Key: "2", Title: "TV Shows", Type: "show", Agent: "tv.plex.agents.none", Scanner: "Plex TV Series",
			Locations: []plex.Location{{ID: 2, Path: "/data/tv"}, {ID: 3, Path: "/data/tv-kids"}}},
		{Key: "1", Title: "Movies", Type: "movie", Agent: "tv.plex.agents.none", Scanner: "Plex Movie",
			Locations: []plex.Location{{ID: 1, Path: "/data/movies"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Sections = %+v\nwant %+v", got, want)
	}
}

func TestSectionsVariants(t *testing.T) {
	custom := []plex.Section{
		{Key: "7", Title: "Music", Type: "artist", Agent: "tv.plex.agents.music", Scanner: "Plex Music",
			Locations: []plex.Location{{ID: 9, Path: "/data/music"}, {ID: 10, Path: "/data/music-lossless"}, {ID: 11, Path: "/archive/music"}}},
		{Key: "8", Title: "Empty", Type: "photo", Locations: []plex.Location{}},
	}
	tests := []struct {
		name  string
		setup func(*plextest.Server)
		want  []plex.Section
	}{
		{"recorded server without libraries", func(s *plextest.Server) { s.SetSectionsJSON(plextest.Recorded("sections-empty.json")) }, []plex.Section{}},
		{"SetSections round trip", func(s *plextest.Server) { s.SetSections(custom) }, custom},
		{"SetSections empty", func(s *plextest.Server) { s.SetSections(nil) }, []plex.Section{}},
		{"numeric key and string id", func(s *plextest.Server) {
			s.SetSectionsJSON([]byte(`{"MediaContainer":{"Directory":[{"key":4,"title":"T","type":"movie","Location":[{"id":"12","path":"/m"}]}]}}`))
		}, []plex.Section{{Key: "4", Title: "T", Type: "movie", Locations: []plex.Location{{ID: 12, Path: "/m"}}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := plextest.NewServer(t, "variants-token-1")
			tt.setup(srv)
			got, err := newClient(t, srv.URL, "variants-token-1", plex.Options{}).Sections(context.Background())
			if err != nil {
				t.Fatalf("Sections: %v", err)
			}
			if got == nil || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Sections = %#v\nwant %#v", got, tt.want)
			}
		})
	}
}

func TestPrefs(t *testing.T) {
	recordedDefaults := plex.Prefs{ButlerStartHour: 2, ButlerEndHour: 5, ButlerTaskBackupDatabase: true,
		ButlerDatabaseBackupPath: "/config/Library/Application Support/Plex Media Server/Plug-in Support/Databases"}
	tests := []struct {
		name    string
		setup   func(*plextest.Server)
		want    plex.Prefs
		wantErr error
	}{
		{"recorded", func(*plextest.Server) {}, recordedDefaults, nil},
		{"changed window", func(s *plextest.Server) {
			s.SetPref("ButlerStartHour", 23)
			s.SetPref("ButlerEndHour", 4)
			s.SetPref("ButlerTaskBackupDatabase", false)
		}, plex.Prefs{ButlerStartHour: 23, ButlerEndHour: 4, ButlerDatabaseBackupPath: recordedDefaults.ButlerDatabaseBackupPath}, nil},
		{"string values", func(s *plextest.Server) {
			s.SetPref("ButlerStartHour", "1")
			s.SetPref("ButlerTaskBackupDatabase", "0")
		}, plex.Prefs{ButlerStartHour: 1, ButlerEndHour: 5, ButlerDatabaseBackupPath: recordedDefaults.ButlerDatabaseBackupPath}, nil},
		{"setting without a value keeps the default", func(s *plextest.Server) {
			s.SetPrefsJSON([]byte(`{"MediaContainer":{"Setting":[{"id":"ButlerStartHour","type":"int"}]}}`))
		}, plex.Prefs{ButlerStartHour: 2, ButlerEndHour: 5, ButlerTaskBackupDatabase: true}, nil},
		{"missing settings take Plex defaults", func(s *plextest.Server) {
			s.SetPrefsJSON([]byte(`{"MediaContainer":{"size":0}}`))
		}, plex.Prefs{ButlerStartHour: 2, ButlerEndHour: 5, ButlerTaskBackupDatabase: true}, nil},
		{"hour out of range", func(s *plextest.Server) { s.SetPref("ButlerEndHour", 30) }, plex.Prefs{}, plex.ErrNotPlex},
		{"not a boolean", func(s *plextest.Server) { s.SetPref("ButlerTaskBackupDatabase", "maybe") }, plex.Prefs{}, plex.ErrNotPlex},
		{"no container", func(s *plextest.Server) { s.SetPrefsJSON([]byte(`{"settings":[]}`)) }, plex.Prefs{}, plex.ErrNotPlex},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := plextest.NewServer(t, "prefs-token-123")
			tt.setup(srv)
			got, err := newClient(t, srv.URL, "prefs-token-123", plex.Options{}).Prefs(context.Background())
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Prefs error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("Prefs = %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}

func TestInButlerWindow(t *testing.T) {
	tests := []struct {
		start, end int
		in, out    []int
	}{
		{2, 5, []int{2, 3, 4}, []int{0, 1, 5, 6, 23}},
		{23, 3, []int{23, 0, 1, 2}, []int{3, 4, 22}},
		{4, 4, nil, []int{0, 4, 12, 23}},
	}
	for _, tt := range tests {
		p := plex.Prefs{ButlerStartHour: tt.start, ButlerEndHour: tt.end}
		for _, h := range tt.in {
			if !p.InButlerWindow(h) {
				t.Errorf("window %d-%d: hour %d should be inside", tt.start, tt.end, h)
			}
		}
		for _, h := range tt.out {
			if p.InButlerWindow(h) {
				t.Errorf("window %d-%d: hour %d should be outside", tt.start, tt.end, h)
			}
		}
	}
}

func TestUnauthorized(t *testing.T) {
	srv := plextest.NewServer(t, "the-right-token-42")
	for _, token := range []string{"", "a-wrong-token-value-77"} {
		c := newClient(t, srv.URL, token, plex.Options{})
		for name, call := range map[string]func() error{
			"sections": func() error { _, err := c.Sections(context.Background()); return err },
			"prefs":    func() error { _, err := c.Prefs(context.Background()); return err },
		} {
			err := call()
			var perr *plex.Error
			if !errors.Is(err, plex.ErrUnauthorized) || !errors.As(err, &perr) || perr.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s with token %q: %v, want ErrUnauthorized with status 401", name, token, err)
			}
			if errors.Is(err, plex.ErrNotPlex) {
				t.Fatalf("%s: a 401 html body must not be decoded as JSON: %v", name, err)
			}
			if token != "" && strings.Contains(err.Error(), token) {
				t.Fatalf("%s: error contains the token: %v", name, err)
			}
		}
	}
}

func TestTimeout(t *testing.T) {
	const token = "timeout-token-1234"
	srv := plextest.NewServer(t, token)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	srv.Handle(plex.PathSections, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	c := newClient(t, srv.URL, token, plex.Options{Timeout: 150 * time.Millisecond})
	start := time.Now()
	_, err := c.Sections(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "no answer within 150ms") {
		t.Fatalf("Sections = %v, want a timeout", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("timeout took %s", d)
	}
	if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "http://") {
		t.Fatalf("timeout error leaks the token or URL: %v", err)
	}
	if res := c.Test(context.Background()); res.OK || !strings.Contains(res.Message, "did not answer within") {
		t.Fatalf("Test = %+v, want a timeout message", res)
	}
}

func TestCallerCancellation(t *testing.T) {
	srv := plextest.NewServer(t, "")
	c := newClient(t, srv.URL, "", plex.Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Identity(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Identity with a cancelled context = %v, want context.Canceled", err)
	}
	if res := c.Test(ctx); res.OK || !strings.Contains(res.Message, "cancelled") {
		t.Fatalf("Test = %+v", res)
	}
}

func TestNotPlex(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		handler http.HandlerFunc
		call    func(*plex.Client) error
		wantErr error
		status  int
	}{
		{"identity html", plex.PathIdentity, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>router login</html>"))
		}, identity, plex.ErrNotPlex, 200},
		{"identity other json", plex.PathIdentity, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"status":"ok","version":"2"}`))
		}, identity, plex.ErrNotPlex, 200},
		{"identity 404", plex.PathIdentity, func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		}, identity, plex.ErrNotPlex, 404},
		{"sections without container", plex.PathSections, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"sections":[]}`))
		}, sections, plex.ErrNotPlex, 200},
		{"sections wrong shape", plex.PathSections, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"MediaContainer":{"Directory":{"key":"1"}}}`))
		}, sections, plex.ErrNotPlex, 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := plextest.NewServer(t, "")
			srv.Handle(tt.path, tt.handler)
			err := tt.call(newClient(t, srv.URL, "", plex.Options{}))
			var perr *plex.Error
			if !errors.Is(err, tt.wantErr) || !errors.As(err, &perr) || perr.StatusCode != tt.status || perr.Path != tt.path {
				t.Fatalf("err = %#v (%v), want %v at %s with status %d", err, err, tt.wantErr, tt.path, tt.status)
			}
		})
	}
}

func identity(c *plex.Client) error { _, err := c.Identity(context.Background()); return err }
func sections(c *plex.Client) error { _, err := c.Sections(context.Background()); return err }

func TestUnexpectedStatusAndLargeBody(t *testing.T) {
	srv := plextest.NewServer(t, "")
	srv.Handle(plex.PathSections, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	srv.Handle(plex.PathPrefs, func(w http.ResponseWriter, r *http.Request) {
		chunk := bytes.Repeat([]byte(" "), 1<<20)
		for range 9 {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	})
	c := newClient(t, srv.URL, "", plex.Options{})
	_, err := c.Sections(context.Background())
	var perr *plex.Error
	if !errors.As(err, &perr) || perr.StatusCode != 500 || errors.Is(err, plex.ErrNotPlex) || strings.Contains(err.Error(), "boom") {
		t.Fatalf("Sections = %v, want a 500 error without the body", err)
	}
	if _, err := c.Prefs(context.Background()); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("Prefs with a 9 MiB body = %v, want a size error", err)
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	const token = "redirect-token-5678"
	var mu sync.Mutex
	var leaked []string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		leaked = append(leaked, r.Header.Get("X-Plex-Token"))
		mu.Unlock()
	}))
	t.Cleanup(other.Close)
	srv := plextest.NewServer(t, token)
	srv.Handle(plex.PathSections, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
	})
	_, err := newClient(t, srv.URL, token, plex.Options{HTTPClient: &http.Client{}}).Sections(context.Background())
	var perr *plex.Error
	if !errors.As(err, &perr) || perr.StatusCode != http.StatusFound {
		t.Fatalf("Sections = %v, want the 302 as an error", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(leaked) != 0 {
		t.Fatalf("the redirect was followed and the other server received %d requests", len(leaked))
	}
}

func TestTLSVerification(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(plextest.Recorded("identity.json"))
	}))
	t.Cleanup(srv.Close)
	if _, err := newClient(t, srv.URL, "", plex.Options{}).Identity(context.Background()); err == nil {
		t.Fatal("a self-signed certificate was accepted by the default client")
	}
	id, err := newClient(t, srv.URL, "", plex.Options{HTTPClient: srv.Client()}).Identity(context.Background())
	if err != nil || id.MachineIdentifier == "" {
		t.Fatalf("Identity with a client that trusts the test CA = %+v, %v", id, err)
	}
}

func TestBasePath(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		_, _ = w.Write(plextest.Recorded("identity.json"))
	}))
	t.Cleanup(srv.Close)
	if _, err := newClient(t, srv.URL+"/plex/", "", plex.Options{}).Identity(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 1 || paths[0] != "/plex/identity" {
		t.Fatalf("paths = %v, want [/plex/identity]", paths)
	}
}

func TestNewRejectsUnsafeURLs(t *testing.T) {
	for _, u := range []string{"", "plex:32400", "ftp://plex", "http://", "http://user:hunter2secret@plex:32400",
		"http://plex:32400/?X-Plex-Token=hunter2secret", "http://plex:32400/#x", "http://plex:bad"} {
		c, err := plex.New(u, "tok", plex.Options{})
		if err == nil || c != nil {
			t.Errorf("New(%q) accepted", u)
			continue
		}
		if strings.Contains(err.Error(), "hunter2secret") {
			t.Errorf("New(%q) error repeats a credential: %v", u, err)
		}
	}
	if _, err := plex.New("http://plex:32400", "bad\r\ntoken", plex.Options{}); err == nil {
		t.Error("New accepted a token with a line break")
	}
}

func TestTest(t *testing.T) {
	const token = "test-method-token-99"
	tests := []struct {
		name      string
		token     string
		setup     func(*plextest.Server)
		wantOK    bool
		wantMsg   string
		wantIdent bool
	}{
		{"ok", token, func(*plextest.Server) {}, true, "Connected to Plex Media Server 1.43.4.10903-e5521bd8c (2 libraries).", true},
		{"one library", token, func(s *plextest.Server) {
			s.SetSections([]plex.Section{{Key: "1", Title: "Movies", Type: "movie"}})
		}, true, "(1 library)", true},
		{"wrong token", "wrong-token-value-00", func(*plextest.Server) {}, false, "rejected the token", true},
		{"no token", "", func(*plextest.Server) {}, false, "requires a token", true},
		{"not plex", token, func(s *plextest.Server) {
			s.Handle(plex.PathIdentity, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`<html></html>`)) })
		}, false, "did not answer like a Plex Media Server", false},
		{"server error", token, func(s *plextest.Server) {
			s.Handle(plex.PathSections, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) })
		}, false, "unexpected status (502 Bad Gateway)", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := plextest.NewServer(t, token)
			tt.setup(srv)
			res := newClient(t, srv.URL, tt.token, plex.Options{}).Test(context.Background())
			if res.OK != tt.wantOK || !strings.Contains(res.Message, tt.wantMsg) {
				t.Fatalf("Test = %+v, want ok=%v and a message containing %q", res, tt.wantOK, tt.wantMsg)
			}
			if tt.wantIdent != (res.MachineIdentifier != "" && res.Version != "") {
				t.Fatalf("Test = %+v, want identity reported: %v", res, tt.wantIdent)
			}
			if tt.token != "" && strings.Contains(res.Message, tt.token) {
				t.Fatalf("Test message contains the token: %q", res.Message)
			}
			for _, r := range srv.Requests() {
				if r.Path == plex.PathIdentity && r.Header.Get("X-Plex-Token") != "" {
					t.Fatal("Test sent the token to /identity before the server identified as Plex")
				}
				if r.Path != plex.PathIdentity && !tt.wantIdent {
					t.Fatalf("Test went on to %s after the identity check failed", r.Path)
				}
			}
		})
	}
	// Unreachable server.
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	res := newClient(t, url, token, plex.Options{}).Test(context.Background())
	if res.OK || !strings.Contains(res.Message, "Could not reach Plex") || strings.Contains(res.Message, token) {
		t.Fatalf("Test against a closed port = %+v", res)
	}
}

// TestTokenNeverInErrorsOrLogs drives every failure path with a debug logger that does NOT redact
// (so it shows exactly what the client emits) and with Bunkarr's redacting logger, and checks the
// token appears in neither the errors nor the log output, even when the server echoes it back.
func TestTokenNeverInErrorsOrLogs(t *testing.T) {
	const token = "NEVER-LOG-ME-token-3141592"
	var raw, redacted bytes.Buffer
	rawLog := slog.New(slog.NewJSONHandler(&raw, &slog.HandlerOptions{Level: slog.LevelDebug}))
	bunkarrLog, closer, err := logging.New(logging.Options{Level: "debug", StdoutFormat: "text", Stdout: &redacted})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	srv := plextest.NewServer(t, token)
	echo := func(status int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = fmt.Fprintf(w, "you sent %s via %s", r.Header.Get("X-Plex-Token"), r.URL)
		}
	}
	srv.Handle("/library/sections", echo(http.StatusInternalServerError))
	srv.Handle(plex.PathPrefs, echo(http.StatusOK))
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	slow := plextest.NewServer(t, token)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	slow.Handle(plex.PathSections, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})

	var errs []error
	for _, logger := range []*slog.Logger{rawLog, bunkarrLog} {
		clients := []*plex.Client{
			newClient(t, srv.URL, token, plex.Options{Logger: logger}),
			newClient(t, deadURL, token, plex.Options{Logger: logger}),
			newClient(t, slow.URL, token, plex.Options{Logger: logger, Timeout: 100 * time.Millisecond}),
			newClient(t, srv.URL, token+"-wrong", plex.Options{Logger: logger}),
		}
		for _, c := range clients {
			_, err := c.Sections(context.Background())
			errs = append(errs, err)
			_, err = c.Prefs(context.Background())
			errs = append(errs, err)
			_, err = c.Identity(context.Background())
			errs = append(errs, err)
			res := c.Test(context.Background())
			errs = append(errs, errors.New(res.Message))
		}
	}
	failures := 0
	for _, err := range errs {
		if err == nil {
			continue
		}
		failures++
		t.Logf("error: %v", err)
		for _, s := range []string{err.Error(), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err)} {
			if strings.Contains(s, token) {
				t.Errorf("error contains the token: %s", s)
			}
			if strings.Contains(s, "/library/sections?") || strings.Contains(s, "http://127.0.0.1") {
				t.Errorf("error contains a URL: %s", s)
			}
		}
	}
	if failures < 16 {
		t.Fatalf("only %d calls failed; the scenarios did not exercise the error paths", failures)
	}
	for name, out := range map[string]string{"raw": raw.String(), "bunkarr": redacted.String()} {
		if !strings.Contains(out, "Plex request") {
			t.Fatalf("%s log has no request lines: %s", name, out)
		}
		if strings.Contains(out, token) {
			t.Errorf("%s log contains the token:\n%s", name, out)
		}
	}
}

func TestConcurrentUse(t *testing.T) {
	srv := plextest.NewServer(t, "concurrent-token-1")
	c := newClient(t, srv.URL, "concurrent-token-1", plex.Options{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := c.Sections(context.Background()); err != nil {
				t.Error(err)
			}
			if _, err := c.Prefs(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}

// TestReflectedTokenIsRedacted uses a server that answers with a malformed status line holding
// the token it received; Go's transport quotes that line in its error, so the client must redact.
func TestReflectedTokenIsRedacted(t *testing.T) {
	const token = "reflected-token-2718281"
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				req, err := http.ReadRequest(bufio.NewReader(conn))
				if err != nil {
					return
				}
				_, _ = fmt.Fprintf(conn, "garbage %s\r\n\r\n", req.Header.Get("X-Plex-Token"))
			}()
		}
	}()
	var raw bytes.Buffer
	c := newClient(t, "http://"+ln.Addr().String(), token, plex.Options{
		Logger: slog.New(slog.NewTextHandler(&raw, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	_, err = c.Sections(context.Background())
	if err == nil || !strings.Contains(err.Error(), "malformed HTTP") {
		t.Fatalf("Sections = %v, want a malformed response error", err)
	}
	if strings.Contains(err.Error(), token) || strings.Contains(raw.String(), token) {
		t.Fatalf("reflected token not redacted:\nerror: %v\nlog: %s", err, raw.String())
	}
	if !strings.Contains(err.Error(), logging.Redacted) {
		t.Fatalf("expected the redaction marker in %v", err)
	}
}
