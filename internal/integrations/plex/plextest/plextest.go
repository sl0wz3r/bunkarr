// Package plextest is a fake Plex Media Server for tests of packages that talk to Plex, built on
// net/http/httptest.
//
// It serves responses recorded from a real Plex Media Server 1.43.4.10903 (see
// docs/spikes/0001-plex-db-backup.md): GET /identity (answered without a token, like the real
// server), GET /library/sections (a TV section with two locations and a Movies section) and
// GET /:/prefs (151 settings, butler window 2-5, database backup on). The files in testdata/ are
// embedded copies of internal/integrations/plex/testdata; a test in package plex keeps them
// identical. Every other path requires the X-Plex-Token header and answers 401 with the same
// text/html body the real server sends. A token sent in the URL query fails the test (design S8).
//
// Hooks (SetSections, SetIdentity, SetPref, Handle, ...) customise the answers; Requests returns
// what the server received.
//
// NewPlexTV starts a fake plex.tv (PINs, account, resources) for "Sign in with Plex"; together
// with NewServer it runs the whole sign-in flow without the internet.
package plextest

import (
	"embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/integrations/plex"
)

//go:embed testdata/*.json
var recorded embed.FS

// Recorded returns a recorded response body by name: "identity.json", "sections.json",
// "sections-empty.json" (a server without libraries) or "prefs.json"; nil for any other name.
func Recorded(name string) []byte {
	b, err := recorded.ReadFile("testdata/" + name)
	if err != nil {
		return nil
	}
	return b
}

// UnauthorizedBody is the body of the real server's 401 answer (Content-Type text/html).
const UnauthorizedBody = `<html><head><title>Unauthorized</title></head><body><h1>401 Unauthorized</h1></body></html>`

// notFoundBody is the body of the real server's 404 answer (Content-Type text/html).
const notFoundBody = `<html><head><title>Not Found</title></head><body><h1>404 Not Found</h1></body></html>`

// Request is one request the fake server received.
type Request struct {
	Method   string
	Path     string
	RawQuery string
	Header   http.Header
}

// Server is a running fake Plex server.
type Server struct {
	// URL is the server's base URL, e.g. http://127.0.0.1:53817.
	URL string
	// Token is the X-Plex-Token the server accepts; "" accepts every request.
	Token string

	t   testing.TB
	srv *httptest.Server

	mu       sync.Mutex
	identity []byte
	sections []byte
	prefs    []byte
	handlers map[string]http.HandlerFunc
	requests []Request
}

// NewServer starts a fake Plex server that accepts token and serves the recorded responses. It is
// closed when the test ends.
func NewServer(t testing.TB, token string) *Server {
	t.Helper()
	return newServer(t, token, false)
}

// NewTLSServer is NewServer over HTTPS with httptest's self-signed certificate (for 127.0.0.1).
// Client returns an HTTP client that trusts it.
func NewTLSServer(t testing.TB, token string) *Server {
	t.Helper()
	return newServer(t, token, true)
}

// Client returns an HTTP client for the server (for a TLS server, one that trusts its
// certificate).
func (s *Server) Client() *http.Client { return s.srv.Client() }

func newServer(t testing.TB, token string, useTLS bool) *Server {
	t.Helper()
	s := &Server{
		Token:    token,
		t:        t,
		identity: Recorded("identity.json"),
		sections: Recorded("sections.json"),
		prefs:    Recorded("prefs.json"),
		handlers: map[string]http.HandlerFunc{},
	}
	if s.identity == nil || s.sections == nil || s.prefs == nil {
		t.Fatal("plextest: recorded responses are missing from the embedded testdata")
	}
	if useTLS {
		s.srv = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	} else {
		s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	}
	s.URL = s.srv.URL
	t.Cleanup(s.srv.Close)
	return s
}

// Close stops the server (it is also closed when the test ends).
func (s *Server) Close() { s.srv.Close() }

// SetIdentity replaces the machine identifier and version /identity reports.
func (s *Server) SetIdentity(machineIdentifier, version string) {
	s.t.Helper()
	body, err := json.Marshal(map[string]any{"MediaContainer": map[string]any{
		"size": 0, "apiVersion": "1.2.3", "claimed": false,
		"machineIdentifier": machineIdentifier, "version": version,
	}})
	if err != nil {
		s.t.Fatalf("plextest: encode identity: %v", err)
	}
	s.setBody(&s.identity, body)
}

// SetSections replaces the library sections with sections, encoded the way Plex does
// (MediaContainer.Directory[].Location[]).
func (s *Server) SetSections(sections []plex.Section) {
	s.t.Helper()
	type location struct {
		ID   int64  `json:"id"`
		Path string `json:"path"`
	}
	type directory struct {
		Key      string     `json:"key"`
		Type     string     `json:"type"`
		Title    string     `json:"title"`
		Agent    string     `json:"agent"`
		Scanner  string     `json:"scanner"`
		Language string     `json:"language"`
		Location []location `json:"Location"`
	}
	dirs := make([]directory, 0, len(sections))
	for _, sec := range sections {
		d := directory{Key: sec.Key, Type: sec.Type, Title: sec.Title, Agent: sec.Agent, Scanner: sec.Scanner,
			Language: "en-US", Location: []location{}}
		for _, l := range sec.Locations {
			d.Location = append(d.Location, location{ID: l.ID, Path: l.Path})
		}
		dirs = append(dirs, d)
	}
	container := map[string]any{"size": len(dirs), "allowSync": false, "title1": "Plex Library"}
	if len(dirs) > 0 {
		container["Directory"] = dirs // Plex omits Directory when there are no sections
	}
	body, err := json.Marshal(map[string]any{"MediaContainer": container})
	if err != nil {
		s.t.Fatalf("plextest: encode sections: %v", err)
	}
	s.setBody(&s.sections, body)
}

// SetSectionsJSON replaces the raw body of GET /library/sections.
func (s *Server) SetSectionsJSON(body []byte) { s.setBody(&s.sections, body) }

// SetPrefsJSON replaces the raw body of GET /:/prefs.
func (s *Server) SetPrefsJSON(body []byte) { s.setBody(&s.prefs, body) }

// SetPref sets the value of one setting in GET /:/prefs (adding it if absent), e.g.
// SetPref("ButlerStartHour", 22).
func (s *Server) SetPref(id string, value any) {
	s.t.Helper()
	s.mu.Lock()
	cur := s.prefs
	s.mu.Unlock()
	var doc struct {
		MediaContainer map[string]json.RawMessage `json:"MediaContainer"`
	}
	if err := json.Unmarshal(cur, &doc); err != nil || doc.MediaContainer == nil {
		s.t.Fatalf("plextest: current prefs are not a Plex MediaContainer: %v", err)
	}
	var settings []map[string]any
	if raw, ok := doc.MediaContainer["Setting"]; ok {
		if err := json.Unmarshal(raw, &settings); err != nil {
			s.t.Fatalf("plextest: decode prefs settings: %v", err)
		}
	}
	found := false
	for _, st := range settings {
		if st["id"] == id {
			st["value"] = value
			found = true
		}
	}
	if !found {
		settings = append(settings, map[string]any{"id": id, "value": value, "default": value})
	}
	raw, err := json.Marshal(settings)
	if err != nil {
		s.t.Fatalf("plextest: encode prefs settings: %v", err)
	}
	doc.MediaContainer["Setting"] = raw
	doc.MediaContainer["size"] = json.RawMessage(strconv.Itoa(len(settings)))
	body, err := json.Marshal(doc)
	if err != nil {
		s.t.Fatalf("plextest: encode prefs: %v", err)
	}
	s.setBody(&s.prefs, body)
}

// Handle replaces the answer for path (exact match, any method). The token check still runs
// first for every path except /identity.
func (s *Server) Handle(path string, h http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h == nil {
		delete(s.handlers, path)
		return
	}
	s.handlers[path] = h
}

// Requests returns the requests received so far, oldest first.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Request, len(s.requests))
	copy(out, s.requests)
	return out
}

func (s *Server) setBody(dst *[]byte, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	*dst = append([]byte(nil), body...)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery, Header: r.Header.Clone()})
	h := s.handlers[r.URL.Path]
	identity, sections, prefs := s.identity, s.sections, s.prefs
	s.mu.Unlock()

	for k := range r.URL.Query() {
		if strings.EqualFold(k, "X-Plex-Token") {
			s.t.Errorf("plextest: %s %s sent X-Plex-Token in the URL query (design S8: header only)", r.Method, r.URL.Path)
			http.Error(w, "token in URL", http.StatusBadRequest)
			return
		}
	}
	w.Header().Set("X-Plex-Protocol", "1.0")
	w.Header().Set("Cache-Control", "no-cache")
	if r.URL.Path != plex.PathIdentity && s.Token != "" && r.Header.Get("X-Plex-Token") != s.Token {
		writeHTML(w, http.StatusUnauthorized, UnauthorizedBody)
		return
	}
	if h != nil {
		h(w, r)
		return
	}
	if r.Method != http.MethodGet {
		writeHTML(w, http.StatusNotFound, notFoundBody)
		return
	}
	switch r.URL.Path {
	case plex.PathIdentity:
		writeJSON(w, identity)
	case plex.PathSections:
		writeJSON(w, sections)
	case plex.PathPrefs:
		writeJSON(w, prefs)
	default:
		writeHTML(w, http.StatusNotFound, notFoundBody)
	}
}

func writeJSON(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func writeHTML(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}
