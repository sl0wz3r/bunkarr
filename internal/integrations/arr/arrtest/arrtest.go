// Package arrtest is a fake Sonarr, Radarr or Lidarr for tests, built on net/http/httptest.
//
// It serves the responses recorded from real Sonarr 4.0.20.3014, Radarr 6.4.4.10685 and Lidarr
// 3.1.0.4875 (testdata/arr/<app>/ at the repository root; testdata/arr/record_slice2.py records
// the files the fixture spike did not) through an explicit route table: method, path and sorted
// query → a fixture. Every route that is not in the table answers 404, so a client request
// outside the allow-list of phase2-3.md §4.3 fails. Routes lists the table.
//
// Like the real applications it requires the X-Api-Key header on every API route (401 with an
// empty body otherwise), and it fails the test when a key is sent in a query (S8). POST command
// accepts only {"name":"Backup"}. Backup zips are served on the non-API route
// <base>/backup/<type>/<name> with Content-Type application/x-zip-compressed and without range
// support, as Lidarr 3.1.0 does; RequireLogin makes them answer 302 to /login like an *arr with
// Forms authentication.
//
// Hooks script failures and variants: SetStatus (404, 500), Redirect (302 to the login page),
// Delay (a slow answer), Oversize (a body larger than the client's cap), SetRootFolderAccessible
// (accessible: false), SetBackupPath (a system/backup path that points elsewhere), SetJSON and
// SetFixture (any other body), UseMovies4K (Radarr's second root folder /movies-4k with one
// movie). Requests returns what the server received.
package arrtest

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/integrations/arr"
)

// FixtureDir is the directory of an application's recorded responses (testdata/arr/<kind> at the
// repository root).
func FixtureDir(kind arr.Kind) string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return filepath.Join("testdata", "arr", string(kind))
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "testdata", "arr", string(kind))
}

// Fixture returns a recorded response body, e.g. Fixture(t, arr.KindRadarr, "movie.json"). A
// missing file fails the test.
func Fixture(t testing.TB, kind arr.Kind, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(FixtureDir(kind), name))
	if err != nil {
		t.Fatalf("arrtest: fixture %s/%s: %v", kind, name, err)
	}
	return b
}

// Request is one request the fake server received.
type Request struct {
	Method string
	// Path is the request path including the URL base; RawQuery its query.
	Path     string
	RawQuery string
	Header   http.Header
	Body     []byte
}

// route is one answer of the table.
type route struct {
	status      int
	contentType string
	body        []byte
	// oversize > 0 streams a body of that many bytes instead of body.
	oversize int64
	delay    time.Duration
	redirect bool
}

// Option configures NewServer.
type Option func(*Server)

// WithURLBase serves the application under a URL base such as "/radarr" (its "URL Base"
// setting), so the client's base URL is Server.URL.
func WithURLBase(base string) Option {
	return func(s *Server) { s.urlBase = strings.TrimRight(base, "/") }
}

// Server is a running fake *arr.
type Server struct {
	// URL is the base URL a client uses, including any URL base.
	URL string
	// Key is the API key the server accepts.
	Key string
	// Kind is the application the server pretends to be.
	Kind arr.Kind

	t       testing.TB
	srv     *httptest.Server
	urlBase string

	mu           sync.Mutex
	routes       map[string]*route
	requireLogin bool
	requests     []Request
}

// NewServer starts a fake *arr of the given kind that accepts key and serves the recorded
// responses. It is closed when the test ends.
func NewServer(t testing.TB, kind arr.Kind, key string, opts ...Option) *Server {
	t.Helper()
	if !kind.Valid() {
		t.Fatalf("arrtest: unknown kind %q", kind)
	}
	s := &Server{Key: key, Kind: kind, t: t, routes: map[string]*route{}}
	for _, o := range opts {
		o(s)
	}
	s.loadTable()
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	s.URL = s.srv.URL + s.urlBase
	t.Cleanup(s.srv.Close)
	return s
}

// Close stops the server (it is also closed when the test ends).
func (s *Server) Close() { s.srv.Close() }

// API returns the table key of an API route: API("GET", "moviefile?movieId=1") is
// "GET /api/v3/moviefile?movieId=1" (the query sorted, without the URL base).
func (s *Server) API(method, target string) string {
	return key(method, s.Kind.APIPrefix()+"/"+target)
}

// key normalizes "METHOD /path?query" with the query sorted.
func key(method, target string) string {
	p, q, _ := strings.Cut(target, "?")
	k := method + " " + p
	if q != "" {
		v, err := url.ParseQuery(q)
		if err == nil {
			q = v.Encode()
		}
		k += "?" + q
	}
	return k
}

func (s *Server) loadTable() {
	s.t.Helper()
	fixture := func(name string) []byte { return Fixture(s.t, s.Kind, name) }
	ok := func(body []byte) *route {
		return &route{status: http.StatusOK, contentType: "application/json", body: body}
	}
	api := func(method, target, name string) { s.routes[s.API(method, target)] = ok(fixture(name)) }

	api(http.MethodGet, "system/status", "system-status.json")
	api(http.MethodGet, "qualityprofile", "qualityprofile.json")
	api(http.MethodGet, "rootfolder", "rootfolder.json")
	api(http.MethodGet, "tag", "tag.json")
	api(http.MethodGet, "config/mediamanagement", "config-mediamanagement.json")
	api(http.MethodGet, "system/backup", "system-backup.json")
	post := fixture("command-backup-post.json")
	s.routes[s.API(http.MethodPost, "command")] = &route{status: http.StatusCreated, contentType: "application/json", body: post}
	get := fixture("command-backup-get.json")
	var cmd struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(get, &cmd); err != nil || cmd.ID == 0 {
		s.t.Fatalf("arrtest: command-backup-get.json has no id: %v", err)
	}
	s.routes[s.API(http.MethodGet, "command/"+strconv.FormatInt(cmd.ID, 10))] = ok(get)

	switch s.Kind {
	case arr.KindRadarr:
		api(http.MethodGet, "movie", "movie.json")
		api(http.MethodGet, "movie/1", "movie-1.json")
		// GET movie/{id} answers exactly the list entry (movie-1.json proves it); the other
		// movies are served from the list.
		s.addListEntries("movie", fixture("movie.json"))
		api(http.MethodGet, "moviefile?movieId=1", "moviefile-movieId-1.json")
	case arr.KindSonarr:
		api(http.MethodGet, "series", "series.json")
		api(http.MethodGet, "series/1", "series-1.json")
		api(http.MethodGet, "series/2", "series-2.json")
		api(http.MethodGet, "episodefile?seriesId=1", "episodefile-seriesId-1.json")
		api(http.MethodGet, "episodefile?seriesId=2", "episodefile-seriesId-2.json")
		api(http.MethodGet, "episode?seriesId=1", "episode-seriesId-1.json")
		api(http.MethodGet, "episode?seriesId=2", "episode-seriesId-2.json")
	case arr.KindLidarr:
		api(http.MethodGet, "artist", "artist.json")
		api(http.MethodGet, "artist/1", "artist-1.json")
		api(http.MethodGet, "trackfile?artistId=1", "trackfile-artistId-1.json")
		api(http.MethodGet, "album?artistId=1", "album-artistId-1.json")
		api(http.MethodGet, "metadataprofile", "metadataprofile.json")
	}
	s.addBackupRoutes(fixture("system-backup.json"))
}

// addListEntries adds GET <p>/{id} for every element of a list fixture that has no route yet.
func (s *Server) addListEntries(p string, list []byte) {
	var items []json.RawMessage
	if err := json.Unmarshal(list, &items); err != nil {
		s.t.Fatalf("arrtest: %s list: %v", p, err)
	}
	for _, raw := range items {
		var id struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(raw, &id); err != nil {
			s.t.Fatalf("arrtest: %s list entry: %v", p, err)
		}
		k := s.API(http.MethodGet, p+"/"+strconv.FormatInt(id.ID, 10))
		if _, ok := s.routes[k]; !ok {
			s.routes[k] = &route{status: http.StatusOK, contentType: "application/json", body: pretty(raw)}
		}
	}
}

func pretty(raw []byte) []byte {
	var b bytes.Buffer
	if err := json.Indent(&b, raw, "", "  "); err != nil {
		return raw
	}
	return b.Bytes()
}

// addBackupRoutes serves a zip for every entry of a system/backup body.
func (s *Server) addBackupRoutes(list []byte) {
	var entries []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(list, &entries); err != nil {
		s.t.Fatalf("arrtest: system/backup: %v", err)
	}
	for _, e := range entries {
		s.mu.Lock()
		_, ok := s.routes[key(http.MethodGet, "/backup/"+e.Type+"/"+e.Name)]
		s.mu.Unlock()
		if !ok {
			s.SetBackupZip(e.Type, e.Name, s.BackupZip())
		}
	}
}

// BackupZip returns a small backup zip like the application's own: config.xml (holding the
// server's key) and an empty <app>.db (an empty file is a valid, empty SQLite database).
func (s *Server) BackupZip() []byte {
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for _, f := range []struct{ name, body string }{
		{"config.xml", "<Config>\n  <ApiKey>" + s.Key + "</ApiKey>\n  <AuthenticationMethod>Forms</AuthenticationMethod>\n</Config>\n"},
		{string(s.Kind) + ".db", ""},
	} {
		w, err := zw.Create(f.name)
		if err == nil {
			_, err = w.Write([]byte(f.body))
		}
		if err != nil {
			s.t.Fatalf("arrtest: build backup zip: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		s.t.Fatalf("arrtest: build backup zip: %v", err)
	}
	return b.Bytes()
}

// SetBackupZip serves body at the non-API route <base>/backup/<typ>/<name>.
func (s *Server) SetBackupZip(typ, name string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes[key(http.MethodGet, "/backup/"+typ+"/"+name)] = &route{status: http.StatusOK,
		contentType: "application/x-zip-compressed", body: append([]byte(nil), body...)}
}

// RequireLogin makes backup downloads answer 302 to the login page, like an *arr whose
// Authentication is Forms (the API key does not authenticate them, spike).
func (s *Server) RequireLogin(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requireLogin = on
}

// Routes returns the table's keys, sorted ("GET /api/v3/movie", "GET /backup/manual/x.zip", ...).
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

// mutate applies fn to the route of an API target, which must exist.
func (s *Server) mutate(method, target string, fn func(*route)) {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.routes[s.API(method, target)]
	if !ok {
		s.t.Fatalf("arrtest: no route %s", s.API(method, target))
	}
	cp := *r
	fn(&cp)
	s.routes[s.API(method, target)] = &cp
}

// SetJSON serves body (200, application/json) for an API target such as "movie/7" or
// "moviefile?movieId=2", adding the route when it is not in the table.
func (s *Server) SetJSON(method, target string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes[s.API(method, target)] = &route{status: http.StatusOK, contentType: "application/json",
		body: append([]byte(nil), body...)}
}

// SetFixture serves a recorded fixture of the server's kind for an API target.
func (s *Server) SetFixture(method, target, name string) {
	s.t.Helper()
	s.SetJSON(method, target, Fixture(s.t, s.Kind, name))
}

// Remove takes an API target out of the table (it then answers 404).
func (s *Server) Remove(method, target string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.routes, s.API(method, target))
}

// SetStatus makes an API target answer status with a small JSON error body (e.g. 404, 500).
func (s *Server) SetStatus(method, target string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes[s.API(method, target)] = &route{status: status, contentType: "application/json",
		body: []byte(fmt.Sprintf(`{"message":"%s"}`, http.StatusText(status)))}
}

// Redirect makes an API target answer 302 to the login page.
func (s *Server) Redirect(method, target string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes[s.API(method, target)] = &route{redirect: true}
}

// Delay makes an existing API target wait d (or until the request is cancelled) before it
// answers.
func (s *Server) Delay(method, target string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.routes[s.API(method, target)]
	if !ok {
		s.t.Errorf("arrtest: no route %s", s.API(method, target))
		return
	}
	cp := *r
	cp.delay = d
	s.routes[s.API(method, target)] = &cp
}

// Oversize makes an API target answer 200 with a body of n bytes (a JSON list of one long
// string), streamed.
func (s *Server) Oversize(method, target string, n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes[s.API(method, target)] = &route{status: http.StatusOK, contentType: "application/json", oversize: n}
}

// SetRootFolderAccessible sets accessible on the root folder with the given path in GET
// rootfolder (an unmounted share reports false).
func (s *Server) SetRootFolderAccessible(path string, accessible bool) {
	s.t.Helper()
	s.editList(http.MethodGet, "rootfolder", func(m map[string]any) {
		if m["path"] == path {
			m["accessible"] = accessible
		}
	})
}

// SetBackupPath sets the path field of the system/backup entry with the given name (the client
// never decodes it, S18).
func (s *Server) SetBackupPath(name, path string) {
	s.t.Helper()
	s.editList(http.MethodGet, "system/backup", func(m map[string]any) {
		if m["name"] == name {
			m["path"] = path
		}
	})
}

// SetBackups replaces GET system/backup with entries and serves a zip for each.
func (s *Server) SetBackups(entries []map[string]any) {
	s.t.Helper()
	body, err := json.Marshal(entries)
	if err != nil {
		s.t.Fatalf("arrtest: encode backups: %v", err)
	}
	s.SetJSON(http.MethodGet, "system/backup", body)
	s.addBackupRoutes(body)
}

// editList decodes the JSON list of an API target, applies fn to each element and stores it.
func (s *Server) editList(method, target string, fn func(map[string]any)) {
	s.t.Helper()
	s.mutate(method, target, func(r *route) {
		var list []map[string]any
		d := json.NewDecoder(bytes.NewReader(r.body))
		d.UseNumber()
		if err := d.Decode(&list); err != nil {
			s.t.Fatalf("arrtest: %s is not a JSON list: %v", target, err)
		}
		for _, m := range list {
			fn(m)
		}
		b, err := json.MarshalIndent(list, "", "  ")
		if err != nil {
			s.t.Fatalf("arrtest: encode %s: %v", target, err)
		}
		r.body = b
	})
}

// UseMovies4K switches a Radarr to the recording with a second root folder, /movies-4k, that holds
// movie 5 (Nosferatu, 1922) with a 2160p file: GET rootfolder lists both root folders, GET movie
// the four movies plus movie 5, and movie/5 and moviefile?movieId=5 are served.
func (s *Server) UseMovies4K() {
	s.t.Helper()
	if s.Kind != arr.KindRadarr {
		s.t.Fatalf("arrtest: UseMovies4K needs a Radarr, not %s", s.Kind)
	}
	s.SetFixture(http.MethodGet, "rootfolder", "rootfolder-movies-4k.json")
	s.SetFixture(http.MethodGet, "movie", "movie-movies-4k.json")
	s.SetFixture(http.MethodGet, "movie/5", "movie-5.json")
	s.SetFixture(http.MethodGet, "moviefile?movieId=5", "moviefile-movieId-5.json")
}

// Requests returns the requests received so far, oldest first.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// ResetRequests forgets the requests received so far.
func (s *Server) ResetRequests() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = nil
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Body != nil {
		body, _ = readAtMost(r, 1<<20)
	}
	s.mu.Lock()
	s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery,
		Header: r.Header.Clone(), Body: body})
	requireLogin := s.requireLogin
	s.mu.Unlock()

	for k := range r.URL.Query() {
		if strings.EqualFold(k, "apikey") || strings.EqualFold(k, "X-Api-Key") {
			s.t.Errorf("arrtest: %s %s sent the API key in the URL query (design S8: header only)", r.Method, r.URL.Path)
			http.Error(w, "key in URL", http.StatusBadRequest)
			return
		}
	}
	p := r.URL.Path
	if s.urlBase != "" {
		if !strings.HasPrefix(p, s.urlBase+"/") {
			notFound(w)
			return
		}
		p = strings.TrimPrefix(p, s.urlBase)
	}
	isAPI := strings.HasPrefix(p, "/api/")
	if isAPI && r.Header.Get("X-Api-Key") != s.Key {
		w.WriteHeader(http.StatusUnauthorized) // the real servers send no body
		return
	}
	if !isAPI && strings.HasPrefix(p, "/backup/") && requireLogin {
		loginRedirect(w, r, s.urlBase)
		return
	}
	if r.Method == http.MethodPost && p == s.Kind.APIPrefix()+"/command" {
		var cmd map[string]any
		if err := json.Unmarshal(body, &cmd); err != nil || len(cmd) != 1 || cmd["name"] != "Backup" {
			s.t.Errorf("arrtest: POST command with body %q: only {\"name\":\"Backup\"} is allowed (S16)", body)
			http.Error(w, "command not allowed", http.StatusBadRequest)
			return
		}
	}
	k := key(r.Method, p)
	if r.URL.RawQuery != "" {
		k = key(r.Method, p+"?"+r.URL.RawQuery)
	}
	s.mu.Lock()
	rt := s.routes[k]
	s.mu.Unlock()
	if rt == nil {
		notFound(w)
		return
	}
	if rt.delay > 0 {
		select {
		case <-time.After(rt.delay):
		case <-r.Context().Done():
			return
		}
	}
	switch {
	case rt.redirect:
		loginRedirect(w, r, s.urlBase)
	case rt.oversize > 0:
		w.Header().Set("Content-Type", rt.contentType)
		w.WriteHeader(rt.status)
		writeOversize(w, rt.oversize)
	default:
		if rt.contentType != "" {
			w.Header().Set("Content-Type", rt.contentType)
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(rt.body)))
		w.WriteHeader(rt.status)
		_, _ = w.Write(rt.body)
	}
}

func readAtMost(r *http.Request, n int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r.Body, n))
}

// loginRedirect answers like an *arr with Forms authentication.
func loginRedirect(w http.ResponseWriter, r *http.Request, base string) {
	w.Header().Set("Location", base+"/login?returnUrl="+url.QueryEscape(r.URL.Path))
	w.WriteHeader(http.StatusFound)
}

func notFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"message":"NotFound"}`))
}

// writeOversize streams a JSON list holding one string, n bytes in total (at least 4).
func writeOversize(w http.ResponseWriter, n int64) {
	if n < 4 {
		n = 4
	}
	_, _ = w.Write([]byte(`["`))
	chunk := bytes.Repeat([]byte("x"), 64<<10)
	for left := n - 4; left > 0; {
		c := chunk
		if int64(len(c)) > left {
			c = c[:left]
		}
		if _, err := w.Write(c); err != nil {
			return
		}
		left -= int64(len(c))
	}
	_, _ = w.Write([]byte(`"]`))
}
