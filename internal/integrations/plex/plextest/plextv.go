package plextest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// PlexTV is a fake plex.tv and clients.plex.tv (one server serves both; point
// plex.Options.PlexTVURL and ClientsPlexTVURL at URL). It serves the requests of "Sign in with
// Plex" (design §5) the way plex.tv answers them:
//   - POST /api/v2/pins?strong=true creates a PIN (201, authToken null);
//   - GET /api/v2/pins/{id}?code=<code> answers the PIN, with its authToken once Approve was
//     called, and 404 for an unknown or expired PIN, a wrong code or another client identifier;
//   - GET /api/v2/user answers the account (with an e-mail address, the token and a subscription,
//     which a client must not decode);
//   - GET /api/v2/resources answers the account's servers plus a player device.
//
// Tokens are accepted only in the X-Plex-Token header: a token in a URL query fails the test, as
// does a request without X-Plex-Client-Identifier or X-Plex-Product, a PIN check without the code,
// or a PIN creation that carries a token.
type PlexTV struct {
	// URL is the server's base URL, e.g. http://127.0.0.1:53817.
	URL string

	t   testing.TB
	srv *httptest.Server

	mu       sync.Mutex
	nextID   int64
	pinTTL   time.Duration
	pins     map[int64]*fakePin
	accounts map[string]Account
	failures map[string]int
	requests []Request
}

// Account is a plex.tv account of the fake, identified by its account token.
type Account struct {
	Token    string
	Username string
	// Email is served (plex.tv does) so tests can check it is never decoded or returned.
	Email   string
	Servers []Resource
}

// Resource is one server of an account, as clients.plex.tv lists it.
type Resource struct {
	Name             string
	ClientIdentifier string
	ProductVersion   string
	Platform         string
	Owned            bool
	// AccessToken is the server's own token ("" when plex.tv lists none).
	AccessToken string
	Connections []Connection
}

// Connection is one address of a Resource.
type Connection struct {
	URI      string
	Address  string
	Port     int
	Protocol string
	Local    bool
	Relay    bool
	IPv6     bool
}

type fakePin struct {
	id        int64
	code      string
	clientID  string
	authToken string
	expiresAt time.Time
	expired   bool
}

// Paths of the fake, as Fail takes them.
const (
	PathPins      = "/api/v2/pins"
	PathPin       = "/api/v2/pins/{id}"
	PathUser      = "/api/v2/user"
	PathResources = "/api/v2/resources"
)

// NewPlexTV starts a fake plex.tv. It is closed when the test ends.
func NewPlexTV(t testing.TB) *PlexTV {
	t.Helper()
	p := &PlexTV{
		t:        t,
		nextID:   564964750,
		pinTTL:   30 * time.Minute,
		pins:     map[int64]*fakePin{},
		accounts: map[string]Account{},
		failures: map[string]int{},
	}
	p.srv = httptest.NewServer(http.HandlerFunc(p.serve))
	p.URL = p.srv.URL
	t.Cleanup(p.srv.Close)
	return p
}

// Close stops the server (it is also closed when the test ends).
func (p *PlexTV) Close() { p.srv.Close() }

// AddAccount adds (or replaces) an account.
func (p *PlexTV) AddAccount(a Account) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.accounts[a.Token] = a
}

// SetPinTTL sets the lifetime of PINs created from now on (default 30 minutes).
func (p *PlexTV) SetPinTTL(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pinTTL = d
}

// Approve claims the PIN with code for the account whose token is accountToken, as the user
// approving it on app.plex.tv. It fails the test when no such PIN exists.
func (p *PlexTV) Approve(code, accountToken string) {
	p.t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pin := range p.pins {
		if pin.code == code {
			pin.authToken = accountToken
			return
		}
	}
	p.t.Errorf("plextest: Approve: no PIN with the code %q", code)
}

// Expire makes the PIN with code answer 404 from now on.
func (p *PlexTV) Expire(code string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pin := range p.pins {
		if pin.code == code {
			pin.expired = true
		}
	}
}

// PinCount is how many PINs were created.
func (p *PlexTV) PinCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pins)
}

// PinIDs returns the ids of the PINs created so far (for tests that check they never leak).
func (p *PlexTV) PinIDs() []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]int64, 0, len(p.pins))
	for id := range p.pins {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// Fail makes every request to path (PathPins, PathPin, PathUser or PathResources) answer status
// with a JSON error body; status 0 restores the normal answers.
func (p *PlexTV) Fail(path string, status int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if status == 0 {
		delete(p.failures, path)
		return
	}
	p.failures[path] = status
}

// Requests returns the requests received so far, oldest first.
func (p *PlexTV) Requests() []Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.requests)
}

// CountRequests returns how many requests were made to path (PathPins, PathPin, ...).
func (p *PlexTV) CountRequests(path string) int {
	n := 0
	for _, r := range p.Requests() {
		if routeOf(r.Method, r.Path) == path {
			n++
		}
	}
	return n
}

func routeOf(method, path string) string {
	switch {
	case method == http.MethodPost && path == PathPins:
		return PathPins
	case method == http.MethodGet && strings.HasPrefix(path, PathPins+"/"):
		return PathPin
	case method == http.MethodGet && (path == PathUser || path == PathResources):
		return path
	}
	return ""
}

func (p *PlexTV) serve(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.requests = append(p.requests, Request{Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery, Header: r.Header.Clone()})
	route := routeOf(r.Method, r.URL.Path)
	failure := p.failures[route]
	p.mu.Unlock()

	for k := range r.URL.Query() {
		if strings.EqualFold(k, "X-Plex-Token") {
			p.t.Errorf("plextest: plex.tv %s %s sent X-Plex-Token in the URL query (design S8: header only)", r.Method, r.URL.Path)
			tvError(w, http.StatusBadRequest, "token in URL")
			return
		}
	}
	for _, h := range []string{"X-Plex-Client-Identifier", "X-Plex-Product"} {
		if r.Header.Get(h) == "" {
			p.t.Errorf("plextest: plex.tv %s %s without %s", r.Method, r.URL.Path, h)
			tvError(w, http.StatusBadRequest, "missing "+h)
			return
		}
	}
	if failure != 0 {
		tvError(w, failure, http.StatusText(failure))
		return
	}
	switch route {
	case PathPins:
		p.createPin(w, r)
	case PathPin:
		p.checkPin(w, r)
	case PathUser:
		p.user(w, r)
	case PathResources:
		p.resources(w, r)
	default:
		tvError(w, http.StatusNotFound, "Not found")
	}
}

func (p *PlexTV) createPin(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Plex-Token") != "" {
		p.t.Errorf("plextest: POST %s sent a token", PathPins)
	}
	if r.URL.Query().Get("strong") != "true" {
		p.t.Errorf("plextest: POST %s without strong=true", PathPins)
	}
	p.mu.Lock()
	p.nextID++
	id := p.nextID
	now := time.Now().UTC()
	pin := &fakePin{
		id:        id,
		code:      "code" + strconv.FormatInt(id, 36) + "x7q2lye02n52jq3f",
		clientID:  r.Header.Get("X-Plex-Client-Identifier"),
		expiresAt: now.Add(p.pinTTL),
	}
	p.pins[id] = pin
	ttl := p.pinTTL
	p.mu.Unlock()
	writeTV(w, http.StatusCreated, pinBody(pin, now, ttl))
}

func (p *PlexTV) checkPin(w http.ResponseWriter, r *http.Request) {
	idText := strings.TrimPrefix(r.URL.Path, PathPins+"/")
	id, err := strconv.ParseInt(idText, 10, 64)
	code := r.URL.Query().Get("code")
	if code == "" {
		p.t.Errorf("plextest: GET %s without the PIN code", PathPin)
	}
	p.mu.Lock()
	pin := p.pins[id]
	var body map[string]any
	ok := err == nil && pin != nil && !pin.expired && time.Now().Before(pin.expiresAt) &&
		pin.code == code && pin.clientID == r.Header.Get("X-Plex-Client-Identifier")
	if ok {
		body = pinBody(pin, time.Now().UTC(), time.Until(pin.expiresAt))
	}
	p.mu.Unlock()
	if !ok {
		tvError(w, http.StatusNotFound, "Code not found or expired")
		return
	}
	writeTV(w, http.StatusOK, body)
}

func pinBody(pin *fakePin, now time.Time, ttl time.Duration) map[string]any {
	var token any
	if pin.authToken != "" {
		token = pin.authToken
	}
	return map[string]any{
		"id": pin.id, "code": pin.code, "product": "Bunkarr", "trusted": false, "qr": "https://plex.tv/api/v2/pins/qr/" + pin.code,
		"clientIdentifier": pin.clientID, "location": map[string]any{"code": "US", "country": "United States"},
		"expiresIn": int(ttl.Seconds()), "createdAt": now.Format(time.RFC3339), "expiresAt": pin.expiresAt.Format(time.RFC3339),
		"authToken": token, "newRegistration": nil,
	}
}

// account returns the account of the request's token, or answers 401.
func (p *PlexTV) account(w http.ResponseWriter, r *http.Request) (Account, bool) {
	p.mu.Lock()
	a, ok := p.accounts[r.Header.Get("X-Plex-Token")]
	p.mu.Unlock()
	if !ok || r.Header.Get("X-Plex-Token") == "" {
		tvError(w, http.StatusUnauthorized, "User could not be authenticated")
		return Account{}, false
	}
	return a, true
}

func (p *PlexTV) user(w http.ResponseWriter, r *http.Request) {
	a, ok := p.account(w, r)
	if !ok {
		return
	}
	writeTV(w, http.StatusOK, map[string]any{
		"id": 1234567, "uuid": "0af5c1d3e2b4a697", "username": a.Username, "title": a.Username, "email": a.Email,
		"friendlyName": "", "locale": nil, "confirmed": true, "emailOnlyAuth": false, "hasPassword": true, "protected": false,
		"thumb": "https://plex.tv/users/0af5c1d3e2b4a697/avatar", "authToken": a.Token, "mailingListStatus": "active",
		"subscription": map[string]any{"active": true, "subscribedAt": "2020-01-01T00:00:00Z", "status": "Active", "plan": "lifetime"},
	})
}

func (p *PlexTV) resources(w http.ResponseWriter, r *http.Request) {
	a, ok := p.account(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	for _, k := range []string{"includeHttps", "includeRelay", "includeIPv6"} {
		if q.Get(k) != "1" {
			p.t.Errorf("plextest: GET %s without %s=1", PathResources, k)
		}
	}
	out := make([]map[string]any, 0, len(a.Servers)+1)
	for _, s := range a.Servers {
		conns := make([]map[string]any, 0, len(s.Connections))
		for _, c := range s.Connections {
			conns = append(conns, map[string]any{
				"protocol": c.Protocol, "address": c.Address, "port": c.Port, "uri": c.URI,
				"local": c.Local, "relay": c.Relay, "IPv6": c.IPv6,
			})
		}
		var token any
		if s.AccessToken != "" {
			token = s.AccessToken
		}
		out = append(out, map[string]any{
			"name": s.Name, "product": "Plex Media Server", "productVersion": s.ProductVersion, "platform": s.Platform,
			"platformVersion": "6.1", "device": "Docker", "clientIdentifier": s.ClientIdentifier, "createdAt": "2024-01-01T00:00:00Z",
			"lastSeenAt": "2026-09-25T00:00:00Z", "provides": "server", "ownerId": nil, "sourceTitle": nil,
			"publicAddress": "203.0.113.5", "accessToken": token, "owned": s.Owned, "home": false, "synced": false,
			"relay": true, "presence": true, "httpsRequired": false, "publicAddressMatches": true, "dnsRebindingProtection": false,
			"natLoopbackSupported": false, "connections": conns,
		})
	}
	// A player device: not a server, never listed by Resources.
	out = append(out, map[string]any{
		"name": "iPhone", "product": "Plex for iOS", "provides": "client,player", "clientIdentifier": "iphone-1",
		"accessToken": "player-token-never-used", "owned": true, "connections": []any{},
	})
	writeTV(w, http.StatusOK, out)
}

func writeTV(w http.ResponseWriter, status int, body any) {
	b, err := json.Marshal(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func tvError(w http.ResponseWriter, status int, msg string) {
	writeTV(w, status, map[string]any{"errors": []map[string]any{{"code": 1000 + status, "message": msg, "status": status}}})
}
