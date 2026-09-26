// Package maintainerrtest is a fake Maintainerr for tests. It serves the answers recorded from
// real Maintainerr 3.4.1 and 3.29.0 instances joined to a real Plex Media Server
// (testdata/maintainerr/ and testdata/maintainerr/v3.29.0/ at the repository root, recorded by
// testdata/tautulli/record_slice9.py; index.json lists each file with its exact request) through
// an explicit route table built from that index: a request outside it answers 404 and fails the
// test (S16). Maintainerr has no API authentication: any credential sent (an X-Api-Key header or a
// key in the query) fails the test.
//
// The recorded scene (index.json "scenario"): eight collections (watched movies with a manual
// member, unmonitor only, do nothing, no deadline, inactive, shows, seasons, episodes) plus, in
// 3.29.0, a Tautulli-based collection whose members failed their rule check and an arrAction 5
// collection; a global exclusion of The General, a show exclusion in group 6, show and season
// exclusions in group 7 and a season exclusion in group 8. The probes/ logs show which members
// Maintainerr's own handler acted on.
package maintainerrtest

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/integrations/httpread/readtest"
)

// Variants of the recording.
const (
	V341  = "3.4.1"
	V3290 = "3.29.0"
)

// Dir is testdata/maintainerr.
func Dir() string { return readtest.RepoTestdata("maintainerr") }

// Fixture returns a recorded body of testdata/maintainerr.
func Fixture(t testing.TB, name string) []byte {
	t.Helper()
	return readtest.Read(t, filepath.Join(Dir(), name))
}

type entry struct {
	File        string `json:"file"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	Query       string `json:"query"`
	Status      int    `json:"status"`
	ContentType string `json:"contentType"`
}

// Server is a running fake Maintainerr.
type Server struct {
	*readtest.Server
	t       testing.TB
	entries map[string]entry
}

// NewServer starts a fake Maintainerr of the given recorded variant (V341 or V3290).
func NewServer(t testing.TB, variant string) *Server {
	t.Helper()
	var idx struct {
		Files []entry `json:"files"`
	}
	if err := json.Unmarshal(Fixture(t, "index.json"), &idx); err != nil {
		t.Fatalf("maintainerr index.json: %v", err)
	}
	s := &Server{t: t, entries: map[string]entry{}}
	routes := map[string]readtest.Route{}
	for _, e := range idx.Files {
		s.entries[e.File] = e
		name := e.File
		switch variant {
		case V341:
			if strings.Contains(name, "/") {
				continue
			}
		case V3290:
			var ok bool
			if name, ok = strings.CutPrefix(name, "v3.29.0/"); !ok || strings.Contains(name, "/") {
				continue
			}
		default:
			t.Fatalf("maintainerrtest: unknown variant %q", variant)
		}
		if strings.HasPrefix(name, "error-") || strings.Contains(name, "-v3.") {
			continue
		}
		routes[readtest.Key(e.Method, e.Path, e.Query)] = readtest.Route{Status: e.Status, ContentType: e.ContentType, Body: Fixture(t, e.File)}
	}
	s.Server = readtest.NewServer(t, readtest.Config{App: "Maintainerr", Routes: routes, KeyParams: []string{"apikey", "apiKey", "api_key", "token"},
		Auth: func(r *http.Request, key string) (bool, readtest.Route) {
			if key != "" || r.Header.Get("Authorization") != "" {
				t.Errorf("Maintainerr fake: a credential was sent to %s", r.URL.Path)
			}
			return true, readtest.Route{}
		}})
	return s
}

// Recorded returns the route of a recorded file.
func (s *Server) Recorded(name string) readtest.Route {
	e, ok := s.entries[name]
	if !ok {
		s.t.Fatalf("maintainerrtest: no recording %s", name)
	}
	return readtest.Route{Status: e.Status, ContentType: e.ContentType, Body: Fixture(s.t, name)}
}

// StatusKey, OverlayKey, CollectionsKey, RulesKey, MediaKey and ExclusionKey are route keys.
var (
	StatusKey      = readtest.Key("GET", "/api/app/status", "")
	OverlayKey     = readtest.Key("GET", "/api/collections/overlay-data", "")
	CollectionsKey = readtest.Key("GET", "/api/collections", "")
	RulesKey       = readtest.Key("GET", "/api/rules", "")
)

// MediaKey is the route key of a collection's member list.
func MediaKey(id string) string {
	return readtest.Key("GET", "/api/collections/media/", "collectionId="+id)
}

// ExclusionKey is the route key of a rule group's exclusions.
func ExclusionKey(id string) string {
	return readtest.Key("GET", "/api/rules/exclusion", "rulegroupId="+id)
}

// OldVersion makes the status (and overlay-data) answer like the recorded 3.3.0 (or 3.4.0)
// instance.
func (s *Server) OldVersion(version string) {
	s.Set(StatusKey, s.Recorded("app-status-v"+version+".json"))
	if version == "3.3.0" || version == "3.4.0" {
		s.Set(OverlayKey, s.Recorded("collections-overlay-data-v"+version+".json"))
	}
}
