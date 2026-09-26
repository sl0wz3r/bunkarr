// Package readtest is the fake-server core of the Tautulli, Seerr and Maintainerr fakes
// (tautullitest, seerrtest, maintainerrtest): an explicit route table of recorded answers, built on
// net/http/httptest.
//
// A route is keyed by method, path and sorted query (RouteKey). A request whose key is not in the
// table answers 404 and fails the test: every client may send only the requests of its allow-list
// (S16), and the table holds exactly those. A request that carries an API key in its query also
// fails the test (S8). The application's own authentication is a hook (Config.Auth). Requests
// returns what the server received; Set and SetStatus script other answers.
package readtest

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// RepoTestdata returns the repository's testdata/<parts...> directory.
func RepoTestdata(parts ...string) string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return filepath.Join(append([]string{"testdata"}, parts...)...)
	}
	root := filepath.Join(filepath.Dir(file), "..", "..", "..", "..")
	return filepath.Join(append([]string{root, "testdata"}, parts...)...)
}

// Read returns a recorded file; a missing file fails the test.
func Read(t testing.TB, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readtest: fixture %s: %v", path, err)
	}
	return b
}

// Route is one recorded answer.
type Route struct {
	Status      int
	ContentType string
	Body        []byte
	// Delay holds the answer back (a slow server).
	Delay time.Duration
}

// Request is one request the server received.
type Request struct {
	Method   string
	Path     string
	RawQuery string
	Header   http.Header
}

// Config configures NewServer.
type Config struct {
	// App names the application in test failures.
	App string
	// Routes is the route table, by RouteKey.
	Routes map[string]Route
	// Auth decides whether a request is authenticated. It returns ok, or the answer to send
	// instead. nil accepts every request.
	Auth func(r *http.Request, key string) (ok bool, deny Route)
	// KeyParams are query parameters that must never carry a credential (e.g. "apikey").
	KeyParams []string
}

// Server is a running fake.
type Server struct {
	// URL is the base URL a client uses.
	URL string

	t   testing.TB
	cfg Config
	srv *httptest.Server

	mu       sync.Mutex
	routes   map[string]Route
	requests []Request
}

// RouteKey is the table key of a request: "GET /path?a=1&b=2" (query sorted and encoded as
// url.Values.Encode does, without the query when it is empty).
func RouteKey(method, path string, query url.Values) string {
	k := method + " " + path
	if len(query) > 0 {
		k += "?" + query.Encode()
	}
	return k
}

// Key builds a RouteKey from a raw query string ("cmd=get_users").
func Key(method, path, rawQuery string) string {
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		panic(err)
	}
	return RouteKey(method, path, q)
}

// NewServer starts a fake that is closed when the test ends.
func NewServer(t testing.TB, cfg Config) *Server {
	t.Helper()
	s := &Server{t: t, cfg: cfg, routes: map[string]Route{}}
	for k, r := range cfg.Routes {
		s.routes[k] = r
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	s.URL = s.srv.URL
	t.Cleanup(s.srv.Close)
	return s
}

// Close stops the server.
func (s *Server) Close() { s.srv.Close() }

// Client returns an HTTP client for the server.
func (s *Server) Client() *http.Client { return s.srv.Client() }

// Set replaces (or adds) the answer of a route.
func (s *Server) Set(key string, r Route) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes[key] = r
}

// SetStatus makes a route answer status with an empty JSON object.
func (s *Server) SetStatus(key string, status int) {
	s.Set(key, Route{Status: status, ContentType: "application/json", Body: []byte(`{}`)})
}

// Routes returns the table's keys, sorted.
func (s *Server) Routes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.routes))
	for k := range s.routes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Requests returns the requests received so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// Count returns how many received requests had the route key.
func (s *Server) Count(key string) int {
	n := 0
	for _, r := range s.Requests() {
		if Key(r.Method, r.Path, r.RawQuery) == key {
			n++
		}
	}
	return n
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery, Header: r.Header.Clone()})
	s.mu.Unlock()
	q := r.URL.Query()
	for _, p := range s.cfg.KeyParams {
		if q.Has(p) {
			s.t.Errorf("%s fake: a credential parameter %q was sent in the query of %s", s.cfg.App, p, r.URL.Path)
		}
	}
	if s.cfg.Auth != nil {
		if ok, deny := s.cfg.Auth(r, r.Header.Get("X-Api-Key")); !ok {
			write(w, deny)
			return
		}
	}
	key := RouteKey(r.Method, r.URL.Path, q)
	s.mu.Lock()
	rt, ok := s.routes[key]
	s.mu.Unlock()
	if !ok {
		s.t.Errorf("%s fake: request outside the allow-list: %s", s.cfg.App, key)
		http.NotFound(w, r)
		return
	}
	if rt.Delay > 0 {
		select {
		case <-time.After(rt.Delay):
		case <-r.Context().Done():
			return
		}
	}
	write(w, rt)
}

func write(w http.ResponseWriter, rt Route) {
	ct := rt.ContentType
	if ct == "" {
		ct = "application/json"
	}
	w.Header().Set("Content-Type", ct)
	status := rt.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(rt.Body)
}

// HeaderValue returns the X-Api-Key of a recorded request.
func (r Request) HeaderValue(name string) string { return r.Header.Get(name) }

// HasPrefix reports whether the request path starts with p.
func (r Request) HasPrefix(p string) bool { return strings.HasPrefix(r.Path, p) }
