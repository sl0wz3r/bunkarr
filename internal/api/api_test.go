package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sl0wz3r/bunkarr/internal/auth"
	"github.com/sl0wz3r/bunkarr/internal/config"
	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/engines/enginetest"
	"github.com/sl0wz3r/bunkarr/internal/integrations/plex"
)

type env struct {
	srv  *httptest.Server
	auth *auth.Service
	api  *Server
	app  *App
	db   *db.DB
	// base is a resolved (symlink-free) temp directory for sources and destinations; config is
	// Bunkarr's config directory inside it.
	base, config string
}

func newEnv(t *testing.T, web fstest.MapFS) *env {
	t.Helper()
	return newEnvWith(t, web, nil)
}

// newEnvWith builds a server with every Phase 1 service on a fresh database, started; tweak may
// adjust the App options.
func newEnvWith(t *testing.T, web fstest.MapFS, tweak func(*AppOptions)) *env {
	t.Helper()
	ctx := context.Background()
	base := resolvedTempDir(t)
	dir := filepath.Join(base, "config")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(ctx, filepath.Join(dir, "bunkarr.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	kr, _ := config.NewKeyring(make([]byte, 32))
	settings := config.NewSettings(d, kr)
	a := auth.New(d, settings, nil)
	a.SetBcryptCost(4)
	if err := a.Init(ctx); err != nil {
		t.Fatal(err)
	}
	o := AppOptions{DB: d, Keyring: kr, Settings: settings, ConfigDir: dir, ProgressEvery: 10 * time.Millisecond,
		ShutdownGrace: 5 * time.Second, Plex: plex.Options{Timeout: 5 * time.Second},
		// Run directories in temporary directories: NewApp sweeps them, never the host's /dev/shm.
		Engines: EngineOptions{RunDirs: enginetest.RunDirs(t)}}
	if tweak != nil {
		tweak(&o)
	}
	app, err := NewApp(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := app.Stop(sctx); err != nil {
			t.Errorf("stop app: %v", err)
		}
	})
	s := New(Options{Auth: a, DB: d, Env: config.Env{ConfigDir: dir, Port: 8787}, Web: web, App: app})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &env{srv: srv, auth: a, api: s, app: app, db: d, base: base, config: dir}
}

func (e *env) client(t *testing.T) *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar}
}

func (e *env) do(t *testing.T, c *http.Client, method, path, body string, hdr map[string]string) (int, map[string]any, http.Header) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if c == nil {
		c = http.DefaultClient
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return res.StatusCode, out, res.Header
}

func TestHealthIsPublic(t *testing.T) {
	e := newEnv(t, nil)
	code, body, hdr := e.do(t, nil, "GET", "/api/v1/health", "", nil)
	if code != 200 || body["status"] != "ok" {
		t.Fatalf("health: %d %v", code, body)
	}
	if hdr.Get("Cache-Control") != "no-store" || hdr.Get("X-Content-Type-Options") != "nosniff" || hdr.Get("Content-Security-Policy") == "" {
		t.Fatalf("missing security headers: %v", hdr)
	}
}

func TestSystemStatusRequiresAPIKey(t *testing.T) {
	e := newEnv(t, nil)
	if code, body, _ := e.do(t, nil, "GET", "/api/v1/system/status", "", nil); code != 401 || body["message"] == nil {
		t.Fatalf("without key: %d %v", code, body)
	}
	if code, body, _ := e.do(t, nil, "GET", "/api/v1/system/status", "", map[string]string{"X-Api-Key": "wrong"}); code != 401 || body["message"] != "invalid API key" {
		t.Fatalf("wrong key: %d %v", code, body)
	}
	code, body, _ := e.do(t, nil, "GET", "/api/v1/system/status", "", map[string]string{"X-Api-Key": e.auth.APIKey()})
	if code != 200 {
		t.Fatalf("with key: %d %v", code, body)
	}
	for _, k := range []string{"version", "uptimeSeconds", "databasePath", "startTime", "schemaVersion"} {
		if _, ok := body[k]; !ok {
			t.Errorf("status lacks %q: %v", k, body)
		}
	}
	if !strings.HasSuffix(body["databasePath"].(string), "bunkarr.db") {
		t.Errorf("databasePath = %v", body["databasePath"])
	}
	if code, _, _ := e.do(t, nil, "GET", "/api/v1/system/status?apikey="+e.auth.APIKey(), "", nil); code != 200 {
		t.Fatalf("query key: %d", code)
	}
}

func TestFirstRunSetupLoginLogout(t *testing.T) {
	e := newEnv(t, nil)
	c := e.client(t)
	_, st, _ := e.do(t, c, "GET", "/api/v1/auth/status", "", nil)
	if st["setupRequired"] != true || st["authenticated"] != false {
		t.Fatalf("initial auth status: %v", st)
	}
	if code, _, _ := e.do(t, c, "POST", "/api/v1/auth/setup", `{"username":"admin","password":"short"}`, nil); code != 400 {
		t.Fatalf("short password: %d", code)
	}
	if code, _, _ := e.do(t, c, "POST", "/api/v1/auth/setup", `{"username":"admin","password":"correct horse","extra":1}`, nil); code != 400 {
		t.Fatalf("unknown field: %d", code)
	}
	code, body, hdr := e.do(t, c, "POST", "/api/v1/auth/setup", `{"username":"admin","password":"correct horse"}`, nil)
	if code != 201 || body["username"] != "admin" {
		t.Fatalf("setup: %d %v", code, body)
	}
	cookie := hdr.Get("Set-Cookie")
	for _, want := range []string{auth.SessionCookie + "=", "HttpOnly", "SameSite=Strict"} {
		if !strings.Contains(cookie, want) {
			t.Fatalf("cookie %q lacks %q", cookie, want)
		}
	}
	if code, _, _ := e.do(t, nil, "POST", "/api/v1/auth/setup", `{"username":"evil","password":"correct horse"}`, nil); code != 409 {
		t.Fatalf("second setup: %d, want 409", code)
	}
	_, st, _ = e.do(t, c, "GET", "/api/v1/auth/status", "", nil)
	if st["authenticated"] != true || st["username"] != "admin" || st["via"] != "session" {
		t.Fatalf("auth status after setup: %v", st)
	}
	if code, _, _ := e.do(t, c, "GET", "/api/v1/system/status", "", nil); code != 200 {
		t.Fatalf("status with session: %d", code)
	}
	if code, _, _ := e.do(t, c, "POST", "/api/v1/auth/logout", "", nil); code != 204 {
		t.Fatalf("logout: %d", code)
	}
	if code, _, _ := e.do(t, c, "GET", "/api/v1/system/status", "", nil); code != 401 {
		t.Fatalf("status after logout: %d", code)
	}
	if code, _, _ := e.do(t, c, "POST", "/api/v1/auth/login", `{"username":"admin","password":"correct horse"}`, nil); code != 200 {
		t.Fatalf("login: %d", code)
	}
	if code, _, _ := e.do(t, c, "GET", "/api/v1/system/status", "", nil); code != 200 {
		t.Fatalf("status after login: %d", code)
	}
}

func TestLoginRateLimited(t *testing.T) {
	e := newEnv(t, nil)
	_, _ = e.auth.Setup(context.Background(), "admin", "correct horse")
	for range 5 {
		if code, _, _ := e.do(t, nil, "POST", "/api/v1/auth/login", `{"username":"admin","password":"wrong-pass"}`, nil); code != 401 {
			t.Fatalf("bad login: %d", code)
		}
	}
	code, _, hdr := e.do(t, nil, "POST", "/api/v1/auth/login", `{"username":"admin","password":"correct horse"}`, nil)
	if code != 429 || hdr.Get("Retry-After") == "" {
		t.Fatalf("after 5 failures: %d (Retry-After %q), want 429", code, hdr.Get("Retry-After"))
	}
}

// heldBody reports the first Read of a request body on reading, then waits for gate.
type heldBody struct {
	io.ReadCloser
	once    sync.Once
	reading chan<- struct{}
	gate    <-chan struct{}
}

func (b *heldBody) Read(p []byte) (int, error) {
	b.once.Do(func() {
		b.reading <- struct{}{}
		<-b.gate
	})
	return b.ReadCloser.Read(p)
}

// TestLoginLimiterHoldsForConcurrentGuesses: a guess counts against the limiter before its
// password is checked, so guesses sent together cannot all pass the limiter before the first
// failure lands (checking it, then counting after the bcrypt compare, let 60 wrong logins sent at
// once from one address all be checked). Every body is held until each request is reading its
// body or has its answer, so all 20 have passed the limiter before any password is checked: 5 are
// checked (401), 15 are 429.
func TestLoginLimiterHoldsForConcurrentGuesses(t *testing.T) {
	e := newEnv(t, nil)
	if _, err := e.auth.Setup(context.Background(), "admin", "correct horse"); err != nil {
		t.Fatal(err)
	}
	const n = 20
	reading, gate := make(chan struct{}, n), make(chan struct{})
	h := e.api.Handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = &heldBody{ReadCloser: r.Body, reading: reading, gate: gate}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	})
	codes := make(chan int, n)
	for range n {
		go func() {
			res, err := http.Post(srv.URL+"/api/v1/auth/login", "application/json", strings.NewReader(`{"username":"admin","password":"wrong-pass"}`))
			if err != nil {
				codes <- 0
				return
			}
			_ = res.Body.Close()
			codes <- res.StatusCode
		}()
	}
	got := map[int]int{}
	timeout := time.After(20 * time.Second)
	for pending := n; pending > 0; pending-- {
		select {
		case <-reading:
		case code := <-codes:
			got[code]++
		case <-timeout:
			t.Fatalf("the requests did not all get past the limiter: %v", got)
		}
	}
	close(gate)
	for answered := got[401] + got[429] + got[0]; answered < n; answered++ {
		select {
		case code := <-codes:
			got[code]++
		case <-timeout:
			t.Fatalf("not every login was answered: %v", got)
		}
	}
	if got[401] != 5 || got[429] != n-5 {
		t.Fatalf("20 wrong logins sent together: %v, want 5×401 and 15×429", got)
	}
}

func TestCrossSiteRequestsRefused(t *testing.T) {
	e := newEnv(t, nil)
	code, _, _ := e.do(t, nil, "POST", "/api/v1/auth/setup", `{"username":"admin","password":"correct horse"}`,
		map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"})
	if code != 403 {
		t.Fatalf("cross-site setup: %d, want 403", code)
	}
	code, _, _ = e.do(t, nil, "POST", "/api/v1/settings/general/apikey", "",
		map[string]string{"Sec-Fetch-Site": "cross-site", "X-Api-Key": e.auth.APIKey()})
	if code != 403 {
		t.Fatalf("cross-site key regeneration: %d, want 403", code)
	}
	code, _, _ = e.do(t, nil, "POST", "/api/v1/settings/general/apikey", "",
		map[string]string{"Sec-Fetch-Site": "same-origin", "X-Api-Key": e.auth.APIKey()})
	if code != 200 {
		t.Fatalf("same-origin key regeneration: %d, want 200", code)
	}
}

// TestDNSRebindingRefused checks the requests of a DNS rebinding page (ADR 0002): the user's own
// browser on the LAN, same-origin to itself, under the attacker's host name. It can neither create
// the first user nor use the local-address bypass; the server's own address still can.
func TestDNSRebindingRefused(t *testing.T) {
	e := newEnv(t, nil)
	rebound := func(method, path, body string) int {
		t.Helper()
		req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
		req.Host = "rebind.attacker.example:8787"
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.Header.Set("Origin", "http://rebind.attacker.example:8787")
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.Header.Get("Set-Cookie") != "" {
			t.Fatalf("%s %s under a rebound name set a cookie", method, path)
		}
		return res.StatusCode
	}
	if code := rebound("POST", "/api/v1/auth/setup", `{"username":"attacker","password":"attacker-pass"}`); code != 403 {
		t.Fatalf("setup under a rebound name: %d, want 403", code)
	}
	if req, err := e.auth.SetupRequired(context.Background()); err != nil || !req {
		t.Fatalf("a user was created under a rebound name (setup required %v, %v)", req, err)
	}
	if code, _, _ := e.do(t, nil, "POST", "/api/v1/auth/setup", `{"username":"admin","password":"correct horse"}`, nil); code != 201 {
		t.Fatalf("setup under the server's address: %d, want 201", code)
	}
	if err := e.auth.SetMode(context.Background(), auth.ModeLocalDisabled); err != nil {
		t.Fatal(err)
	}
	if code := rebound("GET", "/api/v1/settings/general", ""); code != 401 {
		t.Fatalf("local bypass under a rebound name: %d, want 401", code)
	}
	if code := rebound("POST", "/api/v1/settings/general/apikey", ""); code != 401 {
		t.Fatalf("API key regeneration under a rebound name: %d, want 401", code)
	}
	if code, _, _ := e.do(t, nil, "GET", "/api/v1/settings/general", "", nil); code != 200 {
		t.Fatalf("local bypass under the server's address: %d, want 200", code)
	}
}

func TestGeneralSettings(t *testing.T) {
	e := newEnv(t, nil)
	key := map[string]string{"X-Api-Key": e.auth.APIKey()}
	code, body, _ := e.do(t, nil, "GET", "/api/v1/settings/general", "", key)
	if code != 200 || body["apiKey"] != e.auth.APIKey() || body["authenticationRequired"] != "enabled" || body["port"] != float64(8787) {
		t.Fatalf("get: %d %v", code, body)
	}
	if code, _, _ := e.do(t, nil, "PUT", "/api/v1/settings/general", `{"authenticationRequired":"never"}`, key); code != 400 {
		t.Fatalf("invalid mode: %d", code)
	}
	code, body, _ = e.do(t, nil, "PUT", "/api/v1/settings/general", `{"authenticationRequired":"disabled_for_local_addresses"}`, key)
	if code != 200 || body["authenticationRequired"] != "disabled_for_local_addresses" {
		t.Fatalf("put: %d %v", code, body)
	}
	// httptest serves on 127.0.0.1: now reachable without credentials.
	if code, _, _ := e.do(t, nil, "GET", "/api/v1/system/status", "", nil); code != 200 {
		t.Fatalf("local bypass: %d", code)
	}
	old := e.auth.APIKey()
	code, body, _ = e.do(t, nil, "POST", "/api/v1/settings/general/apikey", "", key)
	if code != 200 || body["apiKey"] == old {
		t.Fatalf("regenerate: %d %v", code, body)
	}
}

func TestChangeCredentialsNeedsSession(t *testing.T) {
	e := newEnv(t, nil)
	_, _ = e.auth.Setup(context.Background(), "admin", "correct horse")
	code, _, _ := e.do(t, nil, "PUT", "/api/v1/auth/credentials", `{"currentPassword":"correct horse","newPassword":"another one"}`,
		map[string]string{"X-Api-Key": e.auth.APIKey()})
	if code != 403 {
		t.Fatalf("with API key: %d, want 403", code)
	}
	c := e.client(t)
	e.do(t, c, "POST", "/api/v1/auth/login", `{"username":"admin","password":"correct horse"}`, nil)
	if code, _, _ := e.do(t, c, "PUT", "/api/v1/auth/credentials", `{"currentPassword":"nope","newPassword":"another one"}`, nil); code != 400 {
		t.Fatalf("wrong current password: %d", code)
	}
	if code, _, _ := e.do(t, c, "PUT", "/api/v1/auth/credentials", `{"currentPassword":"correct horse","newPassword":"another one"}`, nil); code != 200 {
		t.Fatalf("change: %d", code)
	}
	if code, _, _ := e.do(t, nil, "POST", "/api/v1/auth/login", `{"username":"admin","password":"another one"}`, nil); code != 200 {
		t.Fatalf("login with new password: %d", code)
	}
}

func TestUnknownAPIPathIsJSON404(t *testing.T) {
	e := newEnv(t, fstest.MapFS{"index.html": {Data: []byte("<html>app</html>")}})
	code, body, _ := e.do(t, nil, "GET", "/api/v1/nope", "", nil)
	if code != 404 || body["message"] != "not found" {
		t.Fatalf("unknown API path: %d %v", code, body)
	}
}

func TestSPA(t *testing.T) {
	e := newEnv(t, fstest.MapFS{
		"index.html":        {Data: []byte("<html>app</html>")},
		"assets/app-abc.js": {Data: []byte("console.log(1)")},
	})
	get := func(path string) (int, string, http.Header) {
		res, err := http.Get(e.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b), res.Header
	}
	if code, body, _ := get("/settings/general"); code != 200 || body != "<html>app</html>" {
		t.Fatalf("client route: %d %q", code, body)
	}
	if code, _, hdr := get("/assets/app-abc.js"); code != 200 || !strings.Contains(hdr.Get("Cache-Control"), "immutable") {
		t.Fatalf("asset: %d %v", code, hdr)
	}
	if code, _, _ := get("/assets/missing.js"); code != 404 {
		t.Fatalf("missing asset: %d", code)
	}
	if code, _, _ := get("/../../etc/passwd"); code != 200 {
		t.Fatalf("traversal attempt should get the app shell, got %d", code)
	}
}

func TestSPANotBuilt(t *testing.T) {
	e := newEnv(t, fstest.MapFS{".gitkeep": {}})
	res, err := http.Get(e.srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 503 {
		t.Fatalf("unbuilt UI: %d, want 503", res.StatusCode)
	}
}

// TestOpenAPIMatchesRoutes keeps openapi.json and the router in step: every API route is
// documented and every documented operation exists.
func TestOpenAPIMatchesRoutes(t *testing.T) {
	e := newEnv(t, nil)
	var spec struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(openAPISpec, &spec); err != nil {
		t.Fatal(err)
	}
	documented := map[string]bool{}
	for p, ops := range spec.Paths {
		for m := range ops {
			documented[strings.ToUpper(m)+" /api/v1"+p] = true
		}
	}
	routed := map[string]bool{}
	err := chi.Walk(e.api.Handler().(chi.Routes), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		route = strings.TrimSuffix(route, "/")
		if strings.HasPrefix(route, "/api/v1/") {
			routed[method+" "+route] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var missing, extra []string
	for r := range routed {
		if !documented[r] {
			missing = append(missing, r)
		}
	}
	for d := range documented {
		if !routed[d] {
			extra = append(extra, d)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("openapi.json out of step with the router:\n  undocumented routes: %v\n  documented but not routed: %v", missing, extra)
	}
}

// TestChangeCredentialsIsRateLimited: the current password gates off-site targets and the
// recovery kit (S29), so wrong guesses here count against the login limiter: after 5 failures the
// endpoint answers 429 with Retry-After even for the right password, and login is blocked too.
func TestChangeCredentialsIsRateLimited(t *testing.T) {
	e := newEnv(t, nil)
	_, _ = e.auth.Setup(context.Background(), "admin", "correct horse")
	c := e.client(t)
	if code, _, _ := e.do(t, c, "POST", "/api/v1/auth/login", `{"username":"admin","password":"correct horse"}`, nil); code != 200 {
		t.Fatalf("login: %d", code)
	}
	for i := range 5 {
		if code, _, _ := e.do(t, c, "PUT", "/api/v1/auth/credentials", `{"currentPassword":"guess-`+string(rune('a'+i))+`","newPassword":"another one"}`, nil); code != 400 {
			t.Fatalf("wrong guess %d: %d, want 400", i+1, code)
		}
	}
	code, _, hdr := e.do(t, c, "PUT", "/api/v1/auth/credentials", `{"currentPassword":"correct horse","newPassword":"another one"}`, nil)
	if code != 429 || hdr.Get("Retry-After") == "" {
		t.Fatalf("after 5 wrong guesses: %d (Retry-After %q), want 429", code, hdr.Get("Retry-After"))
	}
	if code, _, _ := e.do(t, nil, "POST", "/api/v1/auth/login", `{"username":"admin","password":"correct horse"}`, nil); code != 429 {
		t.Fatalf("login after the guesses: %d, want 429 (shared limiter)", code)
	}
}
