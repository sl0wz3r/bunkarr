package plextest

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// LibraryDir is testdata/tautulli/plex at the repository root: the answers of the Plex Media
// Server the Tautulli, Seerr and Maintainerr recordings were joined to (§18 slice 9). It holds
// /identity, /library/sections and every section's listing by type (and paging samples).
func LibraryDir() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return filepath.Join("testdata", "tautulli", "plex")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "testdata", "tautulli", "plex")
}

// LibraryMachineIdentifier is the recorded server's machineIdentifier.
const LibraryMachineIdentifier = "fc20cdf3f0f79321f15b0a4d85d1f8dd73200b0e"

var listingFile = regexp.MustCompile(`^library-sections-(\d+)-all-type-(\d+)\.json$`)

// Library is the recorded library as the fake serves it.
type Library struct {
	s *Server
	// listings holds each section's rows by type: "2/4" → the Metadata rows.
	listings map[string][]json.RawMessage
}

// ServeLibrary makes the server answer like the recorded library (LibraryDir): its identity, its
// sections and GET /library/sections/{key}/all?type=N, paged by X-Plex-Container-Start and
// X-Plex-Container-Size (query or header) from the full recorded listing, as Plex pages. A type a
// section was not recorded with answers an empty listing.
func (s *Server) ServeLibrary(t testing.TB) *Library {
	t.Helper()
	dir := LibraryDir()
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("plextest: %v", err)
		}
		return b
	}
	s.setBody(&s.identity, read("identity.json"))
	s.setBody(&s.sections, read("library-sections.json"))
	lib := &Library{s: s, listings: map[string][]json.RawMessage{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("plextest: %v", err)
	}
	var keys []string
	for _, e := range entries {
		m := listingFile.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		var body struct {
			MediaContainer struct {
				Metadata []json.RawMessage `json:"Metadata"`
			} `json:"MediaContainer"`
		}
		if err := json.Unmarshal(read(e.Name()), &body); err != nil {
			t.Fatalf("plextest: %s: %v", e.Name(), err)
		}
		lib.listings[m[1]+"/"+m[2]] = body.MediaContainer.Metadata
		if !contains(keys, m[1]) {
			keys = append(keys, m[1])
		}
	}
	for _, k := range keys {
		s.Handle("/library/sections/"+k+"/all", lib.handler(k))
	}
	return lib
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// SetListing replaces the rows of a section's listing of one type.
func (l *Library) SetListing(section string, typ int, rows []json.RawMessage) {
	l.s.mu.Lock()
	defer l.s.mu.Unlock()
	l.listings[section+"/"+strconv.Itoa(typ)] = rows
	if l.s.handlers["/library/sections/"+section+"/all"] == nil {
		l.s.handlers["/library/sections/"+section+"/all"] = l.handler(section)
	}
}

// Rows returns a section's recorded rows of one type.
func (l *Library) Rows(section string, typ int) []json.RawMessage {
	l.s.mu.Lock()
	defer l.s.mu.Unlock()
	return append([]json.RawMessage(nil), l.listings[section+"/"+strconv.Itoa(typ)]...)
}

func (l *Library) handler(section string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		param := func(name string, def int) int {
			v := q.Get(name)
			if v == "" {
				v = r.Header.Get(name)
			}
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return def
			}
			return n
		}
		l.s.mu.Lock()
		rows := l.listings[section+"/"+q.Get("type")]
		l.s.mu.Unlock()
		start := min(max(param("X-Plex-Container-Start", 0), 0), len(rows))
		size := param("X-Plex-Container-Size", len(rows))
		end := min(start+max(size, 0), len(rows))
		page := rows[start:end]
		if page == nil {
			page = []json.RawMessage{}
		}
		body, _ := json.Marshal(map[string]any{"MediaContainer": map[string]any{
			"size": len(page), "totalSize": len(rows), "offset": start, "librarySectionID": section, "Metadata": page}})
		writeJSON(w, body)
	}
}
