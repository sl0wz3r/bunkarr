// Package seerrtest is a fake Seerr for tests. It serves the answers recorded from a real Seerr
// 3.4.1 joined to a real Plex Media Server (testdata/seerr/ at the repository root, recorded by
// testdata/tautulli/record_slice9.py; index.json lists each file with its exact request) through
// an explicit route table built from that index: a request outside it answers 404 and fails the
// test (S16).
//
// Like Seerr, GET /api/v1/status needs no key and every other route requires X-Api-Key: none
// answers error-no-key.json (401) and a wrong one error-bad-key.json (403). A key in the query, or
// an X-API-User header, fails the test. The recorded users include one with only an e-mail
// (user 3), whose displayName is that e-mail. Set and SetStatus script anything else.
package seerrtest

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/integrations/httpread/readtest"
)

// Key is the API key the fake accepts.
const Key = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="

// Dir is testdata/seerr.
func Dir() string { return readtest.RepoTestdata("seerr") }

// Fixture returns a recorded body of testdata/seerr.
func Fixture(t testing.TB, name string) []byte {
	t.Helper()
	return readtest.Read(t, filepath.Join(Dir(), name))
}

type entry struct {
	File        string `json:"file"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	Query       string `json:"query"`
	Auth        string `json:"auth"`
	Status      int    `json:"status"`
	ContentType string `json:"contentType"`
}

// Server is a running fake Seerr.
type Server struct {
	*readtest.Server
	t       testing.TB
	entries map[string]entry
}

// NewServer starts a fake Seerr that accepts Key.
func NewServer(t testing.TB) *Server {
	t.Helper()
	var idx struct {
		Files []entry `json:"files"`
	}
	if err := json.Unmarshal(Fixture(t, "index.json"), &idx); err != nil {
		t.Fatalf("seerr index.json: %v", err)
	}
	s := &Server{t: t, entries: map[string]entry{}}
	routes := map[string]readtest.Route{}
	for _, e := range idx.Files {
		s.entries[e.File] = e
		if strings.HasPrefix(e.File, "error-") || strings.Contains(e.File, "-v3.3.0") {
			continue
		}
		routes[readtest.Key(e.Method, e.Path, e.Query)] = readtest.Route{Status: e.Status, ContentType: e.ContentType, Body: Fixture(t, e.File)}
	}
	s.Server = readtest.NewServer(t, readtest.Config{App: "Seerr", Routes: routes, KeyParams: []string{"apikey", "apiKey", "api_key"}, Auth: s.auth})
	return s
}

func (s *Server) recorded(name string) readtest.Route {
	e := s.entries[name]
	return readtest.Route{Status: e.Status, ContentType: e.ContentType, Body: Fixture(s.t, name)}
}

func (s *Server) auth(r *http.Request, key string) (bool, readtest.Route) {
	if r.Header.Get("X-API-User") != "" {
		s.t.Errorf("Seerr fake: X-API-User was sent")
	}
	switch {
	case r.URL.Path == "/api/v1/status":
		return true, readtest.Route{}
	case key == "":
		return false, s.recorded("error-no-key.json")
	case key != Key:
		return false, s.recorded("error-bad-key.json")
	}
	return true, readtest.Route{}
}

// RequestKey is the route key of a request page.
func RequestKey(take, skip int) string {
	return readtest.Key("GET", "/api/v1/request", "take="+strconv.Itoa(take)+"&skip="+strconv.Itoa(skip)+"&sort=added&sortDirection=asc")
}

// UserKey is the route key of a user page.
func UserKey(take, skip int) string {
	return readtest.Key("GET", "/api/v1/user", "take="+strconv.Itoa(take)+"&skip="+strconv.Itoa(skip))
}

// SetPage makes a request page answer results with the total results (pageInfo as Seerr sends
// it).
func (s *Server) SetPage(take, skip, results int, rows []map[string]any) {
	if rows == nil {
		rows = []map[string]any{}
	}
	body, err := json.Marshal(map[string]any{"pageInfo": map[string]any{"pages": 1, "pageSize": take, "results": results, "page": 1}, "results": rows})
	if err != nil {
		s.t.Fatal(err)
	}
	s.Set(RequestKey(take, skip), readtest.Route{Status: 200, ContentType: "application/json; charset=utf-8", Body: body})
}
