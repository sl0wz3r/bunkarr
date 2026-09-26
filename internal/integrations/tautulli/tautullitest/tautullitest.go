// Package tautullitest is a fake Tautulli for tests. It serves the answers recorded from a real
// Tautulli 2.18.1 joined to a real Plex Media Server (testdata/tautulli/ at the repository root,
// recorded by testdata/tautulli/record_slice9.py; index.json lists each file with the exact request
// that produced it) through an explicit route table built from that index: a request outside it
// answers 404 and fails the test (S16).
//
// Like Tautulli it requires the API key: no key answers error-no-key.json and a wrong key
// error-bad-key.json (both 401). A key in the query fails the test (S8). Modes replay the other
// recordings: APIDisabled (404, "API not enabled"), Version217 (2.17.2, which answers a key sent
// only in the header with 400 "Parameter apikey is required"), and HistoryOff (keep_history 0 for
// section 2 and the user). Set and SetStatus script anything else.
package tautullitest

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/integrations/httpread/readtest"
)

// Key is the API key the fake accepts.
const Key = "0123456789abcdef0123456789abcdef"

// MachineIdentifier is the recorded Plex server's machineIdentifier (get_server_info).
const MachineIdentifier = "fc20cdf3f0f79321f15b0a4d85d1f8dd73200b0e"

// Dir is testdata/tautulli.
func Dir() string { return readtest.RepoTestdata("tautulli") }

// PlexDir is testdata/tautulli/plex: the linked Plex server's identity, sections and listings.
func PlexDir() string { return filepath.Join(Dir(), "plex") }

// Entry is one recorded file of index.json.
type Entry struct {
	File        string `json:"file"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	Query       string `json:"query"`
	Auth        string `json:"auth"`
	Status      int    `json:"status"`
	ContentType string `json:"contentType"`
}

// Index reads testdata/<app>/index.json.
func Index(t testing.TB, dir string) []Entry {
	t.Helper()
	var idx struct {
		Files []Entry `json:"files"`
	}
	if err := json.Unmarshal(readtest.Read(t, filepath.Join(dir, "index.json")), &idx); err != nil {
		t.Fatalf("index.json: %v", err)
	}
	return idx.Files
}

// Fixture returns a recorded body of testdata/tautulli.
func Fixture(t testing.TB, name string) []byte {
	t.Helper()
	return readtest.Read(t, filepath.Join(Dir(), name))
}

// Server is a running fake Tautulli.
type Server struct {
	*readtest.Server
	t       testing.TB
	entries map[string]Entry
	mode    struct {
		disabled bool
		v217     bool
	}
}

// special recordings are served by modes, not by the route table.
func special(e Entry) bool {
	return strings.HasPrefix(e.File, "plex/") || e.Auth != "header" || e.File == "error-api-disabled.json" ||
		e.File == "error-unknown-cmd.json" || strings.Contains(e.File, "-v2.17.2") || strings.Contains(e.File, "keep_history-0")
}

// NewServer starts a fake Tautulli that accepts Key.
func NewServer(t testing.TB) *Server {
	t.Helper()
	s := &Server{t: t, entries: map[string]Entry{}}
	routes := map[string]readtest.Route{}
	for _, e := range Index(t, Dir()) {
		s.entries[e.File] = e
		if special(e) {
			continue
		}
		routes[readtest.Key(e.Method, e.Path, e.Query)] = readtest.Route{Status: e.Status, ContentType: e.ContentType, Body: Fixture(t, e.File)}
	}
	s.Server = readtest.NewServer(t, readtest.Config{App: "Tautulli", Routes: routes, KeyParams: []string{"apikey"}, Auth: s.auth})
	return s
}

func (s *Server) recorded(name string) readtest.Route {
	e := s.entries[name]
	return readtest.Route{Status: e.Status, ContentType: e.ContentType, Body: Fixture(s.t, name)}
}

func (s *Server) auth(r *http.Request, key string) (bool, readtest.Route) {
	switch {
	case s.mode.v217 && r.URL.Query().Get("cmd") == "get_tautulli_info":
		return false, s.recorded("get_tautulli_info-v2.17.2.json")
	case key == "":
		return false, s.recorded("error-no-key.json")
	case key != Key:
		return false, s.recorded("error-bad-key.json")
	case s.mode.disabled:
		return false, s.recorded("error-api-disabled.json")
	}
	return true, readtest.Route{}
}

// APIDisabled makes every request answer 404 "API not enabled", as with api_enabled = 0.
func (s *Server) APIDisabled() { s.mode.disabled = true }

// Version217 makes get_tautulli_info answer like Tautulli 2.17.2 given the key in the header.
func (s *Server) Version217() { s.mode.v217 = true }

// HistoryOff replays the recordings made after keep_history was turned off for section 2
// (edit_library) and for the user (edit_user).
func (s *Server) HistoryOff() {
	s.Set(readtest.Key("GET", "/api/v2", "cmd=get_library&section_id=2"), s.recorded("get_library-section_id-2-keep_history-0.json"))
	s.Set(readtest.Key("GET", "/api/v2", "cmd=get_users"), s.recorded("get_users-keep_history-0.json"))
}

// HistoryKey is the route key of the refresh's get_history page of a section.
func HistoryKey(section string, start, length int) string {
	return readtest.Key("GET", "/api/v2", "cmd=get_history&section_id="+section+
		"&media_type=movie%2Cepisode%2Ctrack&grouping=0&include_activity=0&order_column=date&order_dir=asc&start="+
		itoa(start)+"&length="+itoa(length))
}

// SetHistory makes a section's get_history page answer the given rows and total (the envelope as
// Tautulli sends it).
func (s *Server) SetHistory(section string, start, length, recordsFiltered int, rows []map[string]any) {
	if rows == nil {
		rows = []map[string]any{}
	}
	body, err := json.Marshal(map[string]any{"response": map[string]any{"result": "success", "message": nil,
		"data": map[string]any{"recordsFiltered": recordsFiltered, "recordsTotal": recordsFiltered, "data": rows, "draw": 1}}})
	if err != nil {
		s.t.Fatal(err)
	}
	s.Set(HistoryKey(section, start, length), readtest.Route{Status: 200, ContentType: "application/json;charset=UTF-8", Body: body})
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
