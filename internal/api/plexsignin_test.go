package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/config"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/integrations/plex"
	"github.com/sl0wz3r/bunkarr/internal/integrations/plex/plextest"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/logging"
)

const (
	siAccountToken = "account-token-7f3a9c2e1b44"
	siServerToken  = "server-token-5d8e1a90c3f2"
	siSharedToken  = "shared-token-0b7c6d5e4f3a"
	siMachine      = "machine-tower-0001"
	siOwnedNoToken = "machine-notoken-0002"
	siShared       = "machine-friend-0003"
	siSharedNoTok  = "machine-friend-0004"
)

// siClock is the sign-in registry's clock in these tests.
type siClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *siClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *siClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// lockedBuffer is a log sink safe for concurrent writes.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// siEnv is an API server whose plex.tv is a fake with one account: an owned server with a token
// (Tower, served by pms), an owned server without one, a shared server with a token and a shared
// server without one.
type siEnv struct {
	*env
	tv      *plextest.PlexTV
	pms     *plextest.Server
	other   *plextest.Server
	clock   *siClock
	logs    *lockedBuffer
	mu      sync.Mutex
	answers [][]byte
}

func newSignInEnv(t *testing.T) *siEnv {
	t.Helper()
	tv := plextest.NewPlexTV(t)
	// Tests set the registry's clock ahead: PINs must not expire at plex.tv meanwhile.
	tv.SetPinTTL(24 * time.Hour)
	logs := &lockedBuffer{}
	// The production logger (its redaction included), at debug level so the request log shows.
	logger, closer, err := logging.New(logging.Options{Level: "debug", Stdout: logs})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer.Close() })
	e := newEnvWith(t, nil, func(o *AppOptions) {
		o.Plex.PlexTVURL, o.Plex.ClientsPlexTVURL = tv.URL, tv.URL
		o.Log = logger
	})
	// The same app behind a server that logs to the buffer.
	s := New(Options{Auth: e.auth, DB: e.db, Env: config.Env{ConfigDir: e.config, Port: 8787}, App: e.app, Log: logger})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	e.srv, e.api = srv, s

	clock := &siClock{t: time.Now()}
	e.app.signIns.mu.Lock()
	e.app.signIns.now = clock.Now
	e.app.signIns.mu.Unlock()

	pms := plextest.NewServer(t, siServerToken)
	pms.SetIdentity(siMachine, "1.43.4.10903-e5521bd8c")
	other := plextest.NewServer(t, siAccountToken)
	other.SetIdentity(siOwnedNoToken, "1.42.0")
	conn := func(u string) plextest.Connection {
		pu, _ := url.Parse(u)
		port, _ := strconv.Atoi(pu.Port())
		return plextest.Connection{URI: u, Address: pu.Hostname(), Port: port, Protocol: "http", Local: true}
	}
	tv.AddAccount(plextest.Account{Token: siAccountToken, Username: "alice", Email: "alice@example.com", Servers: []plextest.Resource{
		{Name: "Zeta Shared", ClientIdentifier: siShared, Owned: false, AccessToken: siSharedToken, ProductVersion: "1.40.0",
			Connections: []plextest.Connection{{URI: "https://203-0-113-9.abc.plex.direct:32400", Address: "203.0.113.9", Port: 32400, Protocol: "https"}}},
		{Name: "Tower", ClientIdentifier: siMachine, Owned: true, AccessToken: siServerToken, ProductVersion: "1.43.4", Platform: "Linux",
			Connections: []plextest.Connection{conn(pms.URL)}},
		{Name: "Basement", ClientIdentifier: siOwnedNoToken, Owned: true, Connections: []plextest.Connection{conn(other.URL)}},
		{Name: "Alpha Shared", ClientIdentifier: siSharedNoTok, Owned: false},
	}})
	return &siEnv{env: e, tv: tv, pms: pms, other: other, clock: clock, logs: logs}
}

// req sends a request with the API key and keeps the raw answer for the secret checks.
func (e *siEnv) req(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	code, raw := e.raw(t, method, path, body)
	e.mu.Lock()
	e.answers = append(e.answers, raw)
	e.mu.Unlock()
	return code, raw
}

func (e *siEnv) must(t *testing.T, want int, method, path string, body, out any) {
	t.Helper()
	code, raw := e.req(t, method, path, body)
	if code != want {
		t.Fatalf("%s %s: status %d, want %d: %s", method, path, code, want, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s %s: decode %s: %v", method, path, raw, err)
		}
	}
}

type siCreated struct {
	ID        string    `json:"id"`
	AuthURL   string    `json:"authUrl"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// start starts a sign-in and returns it with its PIN code (read from authUrl).
func (e *siEnv) start(t *testing.T) (siCreated, string) {
	t.Helper()
	var c siCreated
	e.must(t, 201, "POST", "/plex/signin", nil, &c)
	u, err := url.Parse(c.AuthURL)
	if err != nil || u.Scheme != "https" || u.Host != "app.plex.tv" || u.Path != "/auth" {
		t.Fatalf("authUrl = %q", c.AuthURL)
	}
	q, err := url.ParseQuery(strings.TrimPrefix(u.Fragment, "?"))
	if err != nil || q.Get("code") == "" || q.Get("clientID") != e.app.plexOpts.ClientIdentifier || q.Get("context[device][product]") != "Bunkarr" {
		t.Fatalf("authUrl fragment = %q", u.Fragment)
	}
	return c, q.Get("code")
}

type siStatus struct {
	Status    string    `json:"status"`
	ExpiresAt time.Time `json:"expiresAt"`
	Username  string    `json:"username"`
	Warning   string    `json:"warning"`
}

func (e *siEnv) status(t *testing.T, id string) siStatus {
	t.Helper()
	var st siStatus
	e.must(t, 200, "GET", "/plex/signin/"+id, nil, &st)
	return st
}

// approve approves the PIN at the fake plex.tv and polls until the sign-in is authenticated.
func (e *siEnv) approve(t *testing.T, id, code string) siStatus {
	t.Helper()
	e.tv.Approve(code, siAccountToken)
	e.clock.Advance(3 * time.Second)
	st := e.status(t, id)
	if st.Status != "authenticated" {
		t.Fatalf("status after approval = %+v", st)
	}
	return st
}

// signedIn returns the id of an authenticated sign-in.
func (e *siEnv) signedIn(t *testing.T) string {
	t.Helper()
	c, code := e.start(t)
	e.approve(t, c.ID, code)
	return c.ID
}

// checkNoSecrets fails when any kept answer holds a token, the PIN id, or the PIN code outside an
// authUrl.
func (e *siEnv) checkNoSecrets(t *testing.T) {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	var pinIDs []string
	for _, id := range e.tv.PinIDs() {
		pinIDs = append(pinIDs, strconv.FormatInt(id, 10))
	}
	codeRe := regexp.MustCompile(`code[0-9a-z]+x7q2lye02n52jq3f`)
	for _, raw := range e.answers {
		s := string(raw)
		for _, secret := range append([]string{siAccountToken, siServerToken, siSharedToken, "player-token-never-used", "alice@example.com", "accessToken", "authToken"}, pinIDs...) {
			if strings.Contains(s, secret) {
				t.Errorf("an answer contains %q: %s", secret, s)
			}
		}
		for _, loc := range codeRe.FindAllStringIndex(s, -1) {
			before := s[:loc[0]]
			if i := strings.LastIndex(before, `"authUrl":"https://app.plex.tv/auth#?`); i < 0 || strings.Contains(before[i:], `",`) {
				t.Errorf("an answer contains the PIN code outside authUrl: %s", s)
			}
		}
	}
}

// TestPlexSignInFlow is acceptance 10: sign in, pick a server, test its connections and save,
// against a fake plex.tv and a fake PMS. The stored token is the server's own token.
func TestPlexSignInFlow(t *testing.T) {
	e := newSignInEnv(t)
	c, code := e.start(t)
	if !signInIDPattern.MatchString(c.ID) {
		t.Fatalf("sign-in id %q", c.ID)
	}
	if until := time.Until(c.ExpiresAt); until > signInPendingTTL+time.Minute || until <= 0 {
		t.Errorf("pending expiresAt in %s", until)
	}
	if st := e.status(t, c.ID); st.Status != "pending" || st.Username != "" {
		t.Fatalf("status = %+v", st)
	}
	// Servers are not listed while pending.
	if code, _ := e.req(t, "GET", "/plex/signin/"+c.ID+"/servers", nil); code != 409 {
		t.Fatalf("servers while pending: %d", code)
	}
	st := e.approve(t, c.ID, code)
	if st.Username != "alice" || time.Until(st.ExpiresAt) < signInClaimedTTL-time.Minute {
		t.Fatalf("status = %+v", st)
	}

	var servers []map[string]any
	e.must(t, 200, "GET", "/plex/signin/"+c.ID+"/servers", nil, &servers)
	var names []string
	for _, s := range servers {
		names = append(names, s["name"].(string))
	}
	if strings.Join(names, ",") != "Basement,Tower,Alpha Shared,Zeta Shared" {
		t.Fatalf("servers (owned first, then by name) = %v", names)
	}
	tower := servers[1]
	if tower["id"] != siMachine || tower["owned"] != true || tower["hasAccessToken"] != true || tower["platform"] != "Linux" {
		t.Errorf("Tower = %v", tower)
	}
	if servers[0]["hasAccessToken"] != false {
		t.Errorf("Basement = %v", servers[0])
	}
	conns := tower["connections"].([]any)
	if len(conns) != 1 || conns[0].(map[string]any)["uri"] != e.pms.URL || conns[0].(map[string]any)["local"] != true {
		t.Errorf("Tower connections = %v", conns)
	}

	var probe struct {
		Recommended *string            `json:"recommended"`
		Results     []plex.ProbeResult `json:"results"`
	}
	e.must(t, 200, "POST", "/plex/signin/"+c.ID+"/servers/"+siMachine+"/test", nil, &probe)
	if len(probe.Results) != 1 || !probe.Results[0].OK || !probe.Results[0].TokenAccepted || probe.Results[0].URI != e.pms.URL {
		t.Fatalf("probe = %+v", probe)
	}
	if probe.Recommended != nil {
		t.Errorf("an http connection was recommended: %q", *probe.Recommended)
	}

	// Test with the sign-in: the sign-in stays usable.
	var tr integrationTestResult
	e.must(t, 200, "POST", "/integrations/test", map[string]any{"type": "plex", "url": e.pms.URL,
		"plexSignIn": map[string]any{"id": c.ID, "serverId": siMachine}}, &tr)
	if !tr.OK || tr.MachineIdentifier != siMachine {
		t.Fatalf("test = %+v", tr)
	}

	var it integrations.Integration
	e.must(t, 201, "POST", "/integrations", map[string]any{"type": "plex", "name": "Tower", "url": e.pms.URL + "/",
		"plexSignIn": map[string]any{"id": c.ID, "serverId": siMachine}}, &it)
	if !it.HasAPIKey || it.URL != e.pms.URL {
		t.Fatalf("created = %+v", it)
	}
	tok, err := e.app.Integrations.TokenFor(context.Background(), it.ID, e.pms.URL)
	if err != nil || tok != siServerToken {
		t.Fatalf("TokenFor = %q, %v; want the server's own token", tok, err)
	}
	// The sign-in is consumed: a second use is refused and it is gone.
	if code, msg := e.status2(t, "POST", "/integrations", map[string]any{"type": "plex", "name": "Tower 2", "url": e.pms.URL,
		"plexSignIn": map[string]any{"id": c.ID, "serverId": siMachine}}); code != 400 || !strings.Contains(msg, "sign in with Plex again") {
		t.Fatalf("second use: %d %q", code, msg)
	}
	if code, _ := e.req(t, "GET", "/plex/signin/"+c.ID, nil); code != 404 {
		t.Fatalf("consumed sign-in: %d", code)
	}
	// Plex requests from the saved integration work with the stored token.
	e.must(t, 200, "GET", fmt.Sprintf("/integrations/%d/plex/sections", it.ID), nil, nil)

	e.checkNoSecrets(t)
	// Every plex.tv request identified this install.
	for _, r := range e.tv.Requests() {
		if r.Header.Get("X-Plex-Client-Identifier") != e.app.plexOpts.ClientIdentifier {
			t.Errorf("plex.tv %s without the install's client id", r.Path)
		}
	}
	// Logs carry the id prefix, the username and counts, never a token, the PIN or the full id.
	logs := e.logs.String()
	for _, secret := range append([]string{siAccountToken, siServerToken, siSharedToken, code, c.ID}, e.pinIDs()...) {
		if strings.Contains(logs, secret) {
			t.Errorf("the logs contain %q", secret)
		}
	}
	for _, want := range []string{"Plex sign-in started", "Plex sign-in approved", "signIn=" + c.ID[:8], "username=alice", "servers=4"} {
		if !strings.Contains(logs, want) {
			t.Errorf("the logs lack %q:\n%s", want, logs)
		}
	}
}

func (e *siEnv) pinIDs() []string {
	var out []string
	for _, id := range e.tv.PinIDs() {
		out = append(out, strconv.FormatInt(id, 10))
	}
	return out
}

// status2 is env.status that also keeps the answer.
func (e *siEnv) status2(t *testing.T, method, path string, body any) (int, string) {
	t.Helper()
	code, raw := e.req(t, method, path, body)
	var m struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &m)
	return code, m.Message
}

// A URL that answers /identity as another server never receives the token (Test and Save).
func TestPlexSignInRefusesAURLOfAnotherServer(t *testing.T) {
	e := newSignInEnv(t)
	id := e.signedIn(t)
	evil := plextest.NewServer(t, "")
	evil.SetIdentity("machine-evil", "1.0")
	ref := map[string]any{"id": id, "serverId": siMachine}
	for path, body := range map[string]map[string]any{
		"/integrations/test": {"type": "plex", "url": evil.URL, "plexSignIn": ref},
		"/integrations":      {"type": "plex", "name": "Evil", "url": evil.URL, "plexSignIn": ref},
	} {
		code, msg := e.status2(t, "POST", path, body)
		if code != 400 || !strings.Contains(msg, "answered as another Plex server") || !strings.Contains(msg, "token was not sent") {
			t.Errorf("%s: %d %q", path, code, msg)
		}
	}
	unreachable := "http://" + freeLoopbackAddr(t)
	if code, msg := e.status2(t, "POST", "/integrations", map[string]any{"type": "plex", "name": "X", "url": unreachable, "plexSignIn": ref}); code != 502 ||
		!strings.Contains(msg, "the token was not sent") {
		t.Errorf("unreachable: %d %q", code, msg)
	}
	for _, r := range evil.Requests() {
		if r.Header.Get("X-Plex-Token") != "" || r.Path != plex.PathIdentity {
			t.Errorf("the other server received %s with token %q", r.Path, r.Header.Get("X-Plex-Token"))
		}
	}
	// The refused saves did not use up the sign-in.
	e.must(t, 201, "POST", "/integrations", map[string]any{"type": "plex", "name": "Tower", "url": e.pms.URL, "plexSignIn": ref}, nil)
	e.checkNoSecrets(t)
}

func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := srv.Listener.Addr().String()
	srv.Close()
	return addr
}

// The token choice: the server's own token; the account token for an owned server only with
// useAccountToken; never for a shared server. apiKey or clearApiKey with plexSignIn is refused.
func TestPlexSignInTokenChoice(t *testing.T) {
	e := newSignInEnv(t)
	id := e.signedIn(t)
	ref := func(server string, account bool) map[string]any {
		return map[string]any{"id": id, "serverId": server, "useAccountToken": account}
	}
	bad := []struct {
		name string
		body map[string]any
		msg  string
	}{
		{"apiKey too", map[string]any{"type": "plex", "name": "T", "url": e.pms.URL, "apiKey": "typed-token-123456", "plexSignIn": ref(siMachine, false)}, "not both"},
		{"clearApiKey too", map[string]any{"type": "plex", "name": "T", "url": e.pms.URL, "clearApiKey": true, "plexSignIn": ref(siMachine, false)}, "not both"},
		{"not plex", map[string]any{"type": "sonarr", "name": "T", "url": e.pms.URL, "plexSignIn": ref(siMachine, false)}, "only for Plex"},
		{"unknown server", map[string]any{"type": "plex", "name": "T", "url": e.pms.URL, "plexSignIn": ref("machine-unknown", false)}, "not among"},
		{"bad server id", map[string]any{"type": "plex", "name": "T", "url": e.pms.URL, "plexSignIn": ref("../x", false)}, "not a Plex server id"},
		{"owned without token", map[string]any{"type": "plex", "name": "T", "url": e.other.URL, "plexSignIn": ref(siOwnedNoToken, false)}, "useAccountToken"},
		{"shared without token", map[string]any{"type": "plex", "name": "T", "url": e.other.URL, "plexSignIn": ref(siSharedNoTok, true)}, "enter a token for it manually"},
		{"unknown sign-in", map[string]any{"type": "plex", "name": "T", "url": e.pms.URL, "plexSignIn": map[string]any{"id": strings.Repeat("0", 32), "serverId": siMachine}}, "sign in with Plex again"},
		{"malformed sign-in id", map[string]any{"type": "plex", "name": "T", "url": e.pms.URL, "plexSignIn": map[string]any{"id": "XYZ", "serverId": siMachine}}, "sign in with Plex again"},
		{"unknown field", map[string]any{"type": "plex", "name": "T", "url": e.pms.URL, "plexSignIn": map[string]any{"id": id, "serverId": siMachine, "token": "x"}}, "unknown field"},
	}
	for _, tt := range bad {
		if code, msg := e.status2(t, "POST", "/integrations", tt.body); code != 400 || !strings.Contains(msg, tt.msg) {
			t.Errorf("%s: %d %q, want 400 %q", tt.name, code, msg, tt.msg)
		}
	}
	for _, r := range append(e.pms.Requests(), e.other.Requests()...) {
		if r.Header.Get("X-Plex-Token") != "" {
			t.Errorf("a refused request sent a token to %s", r.Path)
		}
	}
	// An owned server without a token of its own, with the explicit account-token choice.
	var it integrations.Integration
	e.must(t, 201, "POST", "/integrations", map[string]any{"type": "plex", "name": "Basement", "url": e.other.URL,
		"plexSignIn": ref(siOwnedNoToken, true)}, &it)
	if tok, err := e.app.Integrations.TokenFor(context.Background(), it.ID, e.other.URL); err != nil || tok != siAccountToken {
		t.Fatalf("TokenFor = %q, %v; want the account token", tok, err)
	}
	e.checkNoSecrets(t)
}

// An update can take its token from a sign-in, also together with a URL change.
func TestPlexSignInUpdate(t *testing.T) {
	e := newSignInEnv(t)
	old := plextest.NewServer(t, "old-token-1234567")
	var it integrations.Integration
	e.must(t, 201, "POST", "/integrations", map[string]any{"type": "plex", "name": "Tower", "url": old.URL, "apiKey": "old-token-1234567"}, &it)
	id := e.signedIn(t)
	path := fmt.Sprintf("/integrations/%d", it.ID)
	// A test of an update form: id plus the sign-in, at the new URL.
	var tr integrationTestResult
	e.must(t, 200, "POST", "/integrations/test", map[string]any{"id": it.ID, "url": e.pms.URL,
		"plexSignIn": map[string]any{"id": id, "serverId": siMachine}}, &tr)
	if !tr.OK {
		t.Fatalf("test = %+v", tr)
	}
	e.must(t, 200, "PUT", path, map[string]any{"name": "Tower", "url": e.pms.URL, "plexSignIn": map[string]any{"id": id, "serverId": siMachine}}, &it)
	if tok, err := e.app.Integrations.TokenFor(context.Background(), it.ID, e.pms.URL); err != nil || tok != siServerToken || it.URL != e.pms.URL {
		t.Fatalf("after update: %+v, TokenFor = %q, %v", it, tok, err)
	}
	for _, r := range old.Requests() {
		if r.Header.Get("X-Plex-Token") == siServerToken {
			t.Error("the server token went to the old URL")
		}
	}
	e.checkNoSecrets(t)
}

// A failed save releases the sign-in; a concurrent second save is refused.
func TestPlexSignInIsUsedOnce(t *testing.T) {
	e := newSignInEnv(t)
	id := e.signedIn(t)
	ref := map[string]any{"id": id, "serverId": siMachine}
	// A name that is too long fails in the store after the token was resolved: the sign-in is
	// still usable.
	if code, _ := e.status2(t, "POST", "/integrations", map[string]any{"type": "plex", "name": strings.Repeat("n", 200), "url": e.pms.URL, "plexSignIn": ref}); code != 400 {
		t.Fatalf("long name: %d", code)
	}
	var wg sync.WaitGroup
	codes := make([]int, 6)
	for i := range codes {
		wg.Go(func() {
			codes[i], _ = e.raw(t, "POST", "/integrations", map[string]any{"type": "plex", "name": fmt.Sprintf("Tower %d", i), "url": e.pms.URL, "plexSignIn": ref})
		})
	}
	wg.Wait()
	created := 0
	for _, c := range codes {
		switch c {
		case 201:
			created++
		case 400:
		default:
			t.Errorf("status %d", c)
		}
	}
	if created != 1 {
		t.Fatalf("%d integrations were created from one sign-in (%v)", created, codes)
	}
}

// Pending sign-ins expire after 10 minutes (or when plex.tv forgets the PIN); expired ones answer
// "expired" for a while, then 404. At most 8 are live.
func TestPlexSignInExpiryAndLimit(t *testing.T) {
	e := newSignInEnv(t)
	a, _ := e.start(t)
	e.clock.Advance(signInPendingTTL + time.Second)
	if st := e.status(t, a.ID); st.Status != "expired" {
		t.Fatalf("status after 10 min = %+v", st)
	}
	if code, _ := e.req(t, "GET", "/plex/signin/"+a.ID+"/servers", nil); code != 409 {
		t.Fatalf("servers of an expired sign-in: %d", code)
	}
	e.clock.Advance(signInForgetAfter + time.Second)
	if code, _ := e.req(t, "GET", "/plex/signin/"+a.ID, nil); code != 404 {
		t.Fatalf("forgotten sign-in: %d", code)
	}

	// plex.tv forgetting the PIN expires the sign-in.
	b, code := e.start(t)
	e.tv.Expire(code)
	e.clock.Advance(3 * time.Second)
	if st := e.status(t, b.ID); st.Status != "expired" {
		t.Fatalf("status after the PIN expired = %+v", st)
	}

	// An approved sign-in lives 20 minutes.
	c := e.signedIn(t)
	e.clock.Advance(signInClaimedTTL - time.Minute)
	if st := e.status(t, c); st.Status != "authenticated" {
		t.Fatalf("status at 19 min = %+v", st)
	}
	e.clock.Advance(2 * time.Minute)
	if code, msg := e.status2(t, "POST", "/integrations", map[string]any{"type": "plex", "name": "T", "url": e.pms.URL,
		"plexSignIn": map[string]any{"id": c, "serverId": siMachine}}); code != 400 || !strings.Contains(msg, "sign in with Plex again") {
		t.Fatalf("expired sign-in used: %d %q", code, msg)
	}

	// The limit: 8 live (expired ones do not count), the 9th gets 429 without asking plex.tv.
	var ids []string
	for range signInMaxLive {
		s, _ := e.start(t)
		ids = append(ids, s.ID)
	}
	pins := e.tv.PinCount()
	if code, msg := e.status2(t, "POST", "/plex/signin", nil); code != 429 || !strings.Contains(msg, "already in progress") {
		t.Fatalf("9th sign-in: %d %q", code, msg)
	}
	if e.tv.PinCount() != pins {
		t.Fatal("the 9th sign-in created a PIN")
	}
	e.must(t, 204, "DELETE", "/plex/signin/"+ids[0], nil, nil)
	e.must(t, 204, "DELETE", "/plex/signin/"+ids[0], nil, nil) // idempotent
	e.must(t, 201, "POST", "/plex/signin", nil, nil)
	e.checkNoSecrets(t)
}

// plex.tv failures: a failed PIN creation answers 502 and frees its slot; a failed check keeps
// the sign-in pending with a warning.
func TestPlexSignInPlexTVFailures(t *testing.T) {
	e := newSignInEnv(t)
	e.tv.Fail(plextest.PathPins, http.StatusServiceUnavailable)
	for range signInMaxLive + 1 {
		if code, msg := e.status2(t, "POST", "/plex/signin", nil); code != 502 || !strings.Contains(msg, "plex.tv did not create") {
			t.Fatalf("create with plex.tv down: %d %q", code, msg)
		}
	}
	e.tv.Fail(plextest.PathPins, 0)
	c, code := e.start(t)
	e.tv.Fail(plextest.PathPin, http.StatusInternalServerError)
	e.clock.Advance(3 * time.Second)
	if st := e.status(t, c.ID); st.Status != "pending" || !strings.Contains(st.Warning, "did not answer") {
		t.Fatalf("status with plex.tv failing = %+v", st)
	}
	e.tv.Fail(plextest.PathPin, 0)
	// The resources fail at the claim: the sign-in is authenticated, the servers list answers 502
	// until plex.tv answers again.
	e.tv.Fail(plextest.PathResources, http.StatusServiceUnavailable)
	e.approve(t, c.ID, code)
	if code, _ := e.req(t, "GET", "/plex/signin/"+c.ID+"/servers", nil); code != 502 {
		t.Fatalf("servers with plex.tv failing: %d", code)
	}
	e.tv.Fail(plextest.PathResources, 0)
	e.must(t, 200, "GET", "/plex/signin/"+c.ID+"/servers", nil, nil)
	e.checkNoSecrets(t)
}

// PIN checks run at most every 2 s, server lists at most every 10 s.
func TestPlexSignInThrottle(t *testing.T) {
	e := newSignInEnv(t)
	c, code := e.start(t)
	for range 5 {
		e.status(t, c.ID)
	}
	if n := e.tv.CountRequests(plextest.PathPin); n != 1 {
		t.Fatalf("PIN checks = %d, want 1", n)
	}
	e.clock.Advance(signInCheckEvery)
	e.status(t, c.ID)
	if n := e.tv.CountRequests(plextest.PathPin); n != 2 {
		t.Fatalf("PIN checks = %d, want 2", n)
	}
	e.approve(t, c.ID, code)
	for range 3 {
		e.must(t, 200, "GET", "/plex/signin/"+c.ID+"/servers", nil, nil)
	}
	if n := e.tv.CountRequests(plextest.PathResources); n != 1 {
		t.Fatalf("resource fetches = %d, want 1 (the claim's)", n)
	}
	e.clock.Advance(signInResourcesEvery)
	e.must(t, 200, "GET", "/plex/signin/"+c.ID+"/servers", nil, nil)
	if n := e.tv.CountRequests(plextest.PathResources); n != 2 {
		t.Fatalf("resource fetches = %d, want 2", n)
	}
	// Once approved, the PIN is not checked again.
	pinChecks := e.tv.CountRequests(plextest.PathPin)
	e.clock.Advance(time.Minute)
	e.status(t, c.ID)
	if e.tv.CountRequests(plextest.PathPin) != pinChecks {
		t.Fatal("an approved sign-in checked its PIN again")
	}
}

// A sign-in belongs to its principal: another session, the API key or an unknown id get 404.
// Cross-site POSTs are refused (CSRF).
func TestPlexSignInPrincipalsAndCSRF(t *testing.T) {
	e := newSignInEnv(t)
	if _, err := e.auth.Setup(context.Background(), "admin", "correct horse"); err != nil {
		t.Fatal(err)
	}
	login := func() *http.Client {
		c := e.client(t)
		if code, _, _ := e.do(t, c, "POST", "/api/v1/auth/login", `{"username":"admin","password":"correct horse"}`, nil); code != 200 {
			t.Fatalf("login: %d", code)
		}
		return c
	}
	alice, bob := login(), login()
	code, body, _ := e.do(t, alice, "POST", "/api/v1/plex/signin", "", nil)
	if code != 201 {
		t.Fatalf("create with a session: %d %v", code, body)
	}
	id := body["id"].(string)
	if code, _, _ := e.do(t, alice, "GET", "/api/v1/plex/signin/"+id, "", nil); code != 200 {
		t.Fatalf("creator: %d", code)
	}
	if code, _, _ := e.do(t, bob, "GET", "/api/v1/plex/signin/"+id, "", nil); code != 404 {
		t.Fatalf("another session: %d", code)
	}
	if code, _ := e.req(t, "GET", "/plex/signin/"+id, nil); code != 404 {
		t.Fatalf("the API key: %d", code)
	}
	// Another principal's DELETE does nothing (but answers 204).
	if code, _, _ := e.do(t, bob, "DELETE", "/api/v1/plex/signin/"+id, "", nil); code != 204 {
		t.Fatalf("another session's delete: %d", code)
	}
	if code, _, _ := e.do(t, alice, "GET", "/api/v1/plex/signin/"+id, "", nil); code != 200 {
		t.Fatalf("the sign-in was deleted by another principal: %d", code)
	}
	for _, p := range []string{"/plex/signin/" + strings.Repeat("a", 32), "/plex/signin/NOT-AN-ID", "/plex/signin/" + strings.Repeat("A", 32)} {
		if code, _ := e.req(t, "GET", p, nil); code != 404 {
			t.Errorf("GET %s: %d", p, code)
		}
	}
	if code, _ := e.req(t, "DELETE", "/plex/signin/nope", nil); code != 404 {
		t.Errorf("DELETE malformed: %d", code)
	}
	// Cross-site POSTs with the session cookie are refused before the handler.
	pins := e.tv.PinCount()
	if code, _, _ := e.do(t, alice, "POST", "/api/v1/plex/signin", "", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}); code != 403 {
		t.Fatalf("cross-site create: %d", code)
	}
	if code, _, _ := e.do(t, alice, "POST", "/api/v1/plex/signin/"+id+"/servers/"+siMachine+"/test", "", map[string]string{"Sec-Fetch-Site": "cross-site"}); code != 403 {
		t.Fatalf("cross-site probe: %d", code)
	}
	if e.tv.PinCount() != pins {
		t.Fatal("a cross-site request created a PIN")
	}
	// A request body is not accepted on create.
	if code, _ := e.req(t, "POST", "/plex/signin", map[string]any{"token": "x"}); code != 400 {
		t.Fatalf("create with a body: %d", code)
	}
}

// The probe endpoint: unknown servers 404; the account token only with useAccountToken for an
// owned server; a shared server's local addresses are never probed.
func TestPlexSignInProbe(t *testing.T) {
	e := newSignInEnv(t)
	id := e.signedIn(t)
	if code, _ := e.req(t, "POST", "/plex/signin/"+id+"/servers/machine-unknown/test", nil); code != 404 {
		t.Fatalf("unknown server: %d", code)
	}
	var probe plexProbeAnswer
	e.must(t, 200, "POST", "/plex/signin/"+id+"/servers/"+siOwnedNoToken+"/test", nil, &probe)
	if len(probe.Results) != 1 || !probe.Results[0].IdentityMatches || probe.Results[0].TokenAccepted {
		t.Fatalf("probe without a token = %+v", probe.Results)
	}
	for _, r := range e.other.Requests() {
		if r.Header.Get("X-Plex-Token") != "" {
			t.Fatal("the account token was sent without useAccountToken")
		}
	}
	e.must(t, 200, "POST", "/plex/signin/"+id+"/servers/"+siOwnedNoToken+"/test", map[string]any{"useAccountToken": true}, &probe)
	if !probe.Results[0].OK || !probe.Results[0].TokenAccepted {
		t.Fatalf("probe with the account token = %+v", probe.Results)
	}
	e.checkNoSecrets(t)
}

func TestSecurityHeadersAllowPopups(t *testing.T) {
	e := newEnv(t, nil)
	_, _, hdr := e.do(t, nil, "GET", "/api/v1/health", "", nil)
	if got := hdr.Get("Cross-Origin-Opener-Policy"); got != "same-origin-allow-popups" {
		t.Fatalf("COOP = %q, want same-origin-allow-popups (design D10)", got)
	}
}

// App.Stop forgets every sign-in.
func TestPlexSignInsDoNotSurviveStop(t *testing.T) {
	e := newSignInEnv(t)
	id := e.signedIn(t)
	e.app.signIns.clear()
	if code, _ := e.req(t, "GET", "/plex/signin/"+id, nil); code != 404 {
		t.Fatalf("after clear: %d", code)
	}
	e.app.signIns.mu.Lock()
	n := len(e.app.signIns.m)
	e.app.signIns.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d sign-ins left", n)
	}
}

// The install's client identifier: a UUIDv4 created once and stored, never "bunkarr", sent by
// every Plex request including the plexdb runner's.
func TestPlexClientIdentifier(t *testing.T) {
	uuidRe := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	e := newEnv(t, nil)
	id := e.app.plexOpts.ClientIdentifier
	if !uuidRe.MatchString(id) || id == plex.DefaultClientIdentifier {
		t.Fatalf("client identifier = %q", id)
	}
	kr, _ := config.NewKeyring(make([]byte, 32))
	settings := config.NewSettings(e.db, kr)
	stored, ok, err := settings.Get(context.Background(), SettingPlexClientIdentifier)
	if err != nil || !ok || stored != id {
		t.Fatalf("stored = %q, %v, %v", stored, ok, err)
	}
	// A second start reads it back.
	again, err := plexClientIdentifier(context.Background(), settings)
	if err != nil || again != id {
		t.Fatalf("second read = %q, %v", again, err)
	}
	// Without a settings store: a fresh identifier, not stored.
	if tmp, err := plexClientIdentifier(context.Background(), nil); err != nil || !uuidRe.MatchString(tmp) || tmp == id {
		t.Fatalf("without settings = %q, %v", tmp, err)
	}
	// An explicit identifier (tests) wins; the plex.tv URLs default to plex.tv.
	o, err := appPlexOptions(context.Background(), settings, plex.Options{ClientIdentifier: "explicit-id"})
	if err != nil || o.ClientIdentifier != "explicit-id" || o.PlexTVURL != plex.PlexTVURL || o.ClientsPlexTVURL != plex.ClientsPlexTVURL {
		t.Fatalf("appPlexOptions = %+v, %v", o, err)
	}
	if PlexOptions(true).Device != "Docker" || PlexOptions(false).Device != "" {
		t.Fatal("PlexOptions")
	}

	// The plexdb runner sends it: a real backup asks Plex for its version and butler window.
	pms := plextest.NewServer(t, plexToken)
	data := filepath.Join(e.base, "plexdata")
	writeTinyPlexData(t, data)
	dest := e.createDestination(t, "NAS", e.mkdir(t, "nas"), nil, nil)
	var it integrations.Integration
	e.call(t, 201, "POST", "/integrations", map[string]any{"type": "plex", "name": "Plex", "url": pms.URL, "apiKey": plexToken,
		"settings": map[string]any{"dataPath": data}}, &it)
	var j jobs.Job
	e.call(t, 202, "POST", fmt.Sprintf("/integrations/%d/plex/backup", it.ID), map[string]any{"destinationId": dest}, &j)
	if j = e.waitJob(t, j.ID); j.Status != jobs.StatusCompleted && j.Status != jobs.StatusCompletedWithWarnings {
		t.Fatalf("backup job = %+v", j)
	}
	var sawPlexdb bool
	for _, r := range pms.Requests() {
		if got := r.Header.Get("X-Plex-Client-Identifier"); got != id {
			t.Errorf("%s: X-Plex-Client-Identifier = %q, want %q", r.Path, got, id)
		}
		if r.Path == plex.PathPrefs {
			sawPlexdb = true
		}
	}
	if !sawPlexdb {
		t.Fatal("the plexdb runner did not ask Plex for its preferences")
	}
}

// writeTinyPlexData writes a Plex data directory the plexdb runner can back up: a small library
// database with Plex's tables, and Preferences.xml.
func writeTinyPlexData(t *testing.T, dir string) {
	t.Helper()
	dbDir := filepath.Join(dir, "Plug-in Support", "Databases")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	d, err := sql.Open("sqlite", "file:"+filepath.Join(dbDir, "com.plexapp.plugins.library.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE metadata_items (id INTEGER PRIMARY KEY, title TEXT)`,
		`CREATE TABLE media_parts (id INTEGER PRIMARY KEY, media_item_id INTEGER, file TEXT)`,
		`INSERT INTO metadata_items (title) VALUES ('Night of the Living Dead')`,
		`INSERT INTO media_parts (media_item_id, file) VALUES (1, '/data/movies/notld.mkv')`,
	} {
		if _, err := d.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	prefs := `<?xml version="1.0" encoding="utf-8"?>` + "\n" + `<Preferences MachineIdentifier="abc" PlexOnlineToken="fixture-online-token-9a8b7c" FriendlyName="t"/>` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "Preferences.xml"), []byte(prefs), 0o600); err != nil {
		t.Fatal(err)
	}
}
