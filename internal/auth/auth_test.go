package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/config"
	"github.com/sl0wz3r/bunkarr/internal/db"
)

func newTestService(t *testing.T) (*Service, *db.DB) {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "bunkarr.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	master := make([]byte, 32)
	kr, err := config.NewKeyring(master)
	if err != nil {
		t.Fatal(err)
	}
	s := New(d, config.NewSettings(d, kr), nil)
	s.SetBcryptCost(4)
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	return s, d
}

func TestAPIKeyGeneratedOnceAndPersisted(t *testing.T) {
	s, d := newTestService(t)
	key := s.APIKey()
	if len(key) != 32 {
		t.Fatalf("api key %q: want 32 hex chars", key)
	}
	master := make([]byte, 32)
	kr, _ := config.NewKeyring(master)
	s2 := New(d, config.NewSettings(d, kr), nil)
	s2.SetBcryptCost(4)
	if err := s2.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s2.APIKey() != key {
		t.Fatal("API key changed across restarts")
	}
	if !s.CheckAPIKey(key) || s.CheckAPIKey("") || s.CheckAPIKey(key+"x") {
		t.Fatal("CheckAPIKey gave a wrong answer")
	}
	nk, err := s.RegenerateAPIKey(context.Background())
	if err != nil || nk == key || s.CheckAPIKey(key) {
		t.Fatalf("regenerate: new=%q err=%v old still valid=%v", nk, err, s.CheckAPIKey(key))
	}
}

func TestSetupOnlyOnce(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()
	if req, _ := s.SetupRequired(ctx); !req {
		t.Fatal("fresh database should require setup")
	}
	if _, err := s.Setup(ctx, "admin", "short"); !isValidation(err) {
		t.Fatalf("short password: err = %v", err)
	}
	if _, err := s.Setup(ctx, "  ", "longenough"); !isValidation(err) {
		t.Fatalf("blank username: err = %v", err)
	}
	if _, err := s.Setup(ctx, "admin", "correct horse"); err != nil {
		t.Fatal(err)
	}
	if req, _ := s.SetupRequired(ctx); req {
		t.Fatal("setup still required after setup")
	}
	if _, err := s.Setup(ctx, "other", "correct horse"); !errors.Is(err, ErrSetupDone) {
		t.Fatalf("second setup: err = %v, want ErrSetupDone", err)
	}
}

func TestConcurrentSetupCreatesOneUser(t *testing.T) {
	s, d := newTestService(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Go(func() { _, _ = s.Setup(ctx, "user"+string(rune('a'+i)), "password123") })
	}
	wg.Wait()
	var n int
	_ = d.Reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	if n != 1 {
		t.Fatalf("users = %d, want 1", n)
	}
}

func TestLoginSessionLogout(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()
	if _, err := s.Setup(ctx, "Admin", "correct horse"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Login(ctx, "admin", "wrong password", SessionMeta{}); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong password: err = %v", err)
	}
	if _, err := s.Login(ctx, "nobody", "correct horse", SessionMeta{}); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("unknown user: err = %v", err)
	}
	sess, err := s.Login(ctx, "admin", "correct horse", SessionMeta{RemoteAddr: "10.0.0.2"})
	if err != nil {
		t.Fatalf("login (case-insensitive username): %v", err)
	}
	u, ok, err := s.SessionUser(ctx, sess.Token)
	if err != nil || !ok || u.Username != "Admin" {
		t.Fatalf("SessionUser = %+v %v %v", u, ok, err)
	}
	if _, ok, _ := s.SessionUser(ctx, sess.Token+"x"); ok {
		t.Fatal("tampered token accepted")
	}
	if err := s.Logout(ctx, sess.Token); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.SessionUser(ctx, sess.Token); ok {
		t.Fatal("session valid after logout")
	}
}

func TestSessionExpires(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()
	_, _ = s.Setup(ctx, "admin", "correct horse")
	sess, err := s.Login(ctx, "admin", "correct horse", SessionMeta{})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Now().Add(SessionTTL + time.Minute) }
	if _, ok, _ := s.SessionUser(ctx, sess.Token); ok {
		t.Fatal("expired session accepted")
	}
}

func TestChangeCredentialsEndsOtherSessions(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()
	u, _ := s.Setup(ctx, "admin", "correct horse")
	a, _ := s.Login(ctx, "admin", "correct horse", SessionMeta{})
	b, _ := s.Login(ctx, "admin", "correct horse", SessionMeta{})
	if _, err := s.ChangeCredentials(ctx, u.ID, "wrong", "", "new password!", a.Token); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong current password: err = %v", err)
	}
	if _, err := s.ChangeCredentials(ctx, u.ID, "correct horse", "root", "new password!", a.Token); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.SessionUser(ctx, a.Token); !ok {
		t.Fatal("caller's session was ended")
	}
	if _, ok, _ := s.SessionUser(ctx, b.Token); ok {
		t.Fatal("other session survived a password change")
	}
	if _, err := s.Login(ctx, "root", "new password!", SessionMeta{}); err != nil {
		t.Fatalf("login with new credentials: %v", err)
	}
}

func TestResetAuth(t *testing.T) {
	s, d := newTestService(t)
	ctx := context.Background()
	_, _ = s.Setup(ctx, "admin", "correct horse")
	sess, _ := s.Login(ctx, "admin", "correct horse", SessionMeta{})
	key := s.APIKey()
	if err := ResetAuth(ctx, d); err != nil {
		t.Fatal(err)
	}
	if req, _ := s.SetupRequired(ctx); !req {
		t.Fatal("setup not required after reset")
	}
	if _, ok, _ := s.SessionUser(ctx, sess.Token); ok {
		t.Fatal("session survived reset")
	}
	if !s.CheckAPIKey(key) {
		t.Fatal("reset-auth changed the API key")
	}
}

func TestIdentify(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()
	_, _ = s.Setup(ctx, "admin", "correct horse")
	sess, _ := s.Login(ctx, "admin", "correct horse", SessionMeta{})

	// 172.17.0.1 is the container's gateway (Docker's bridge), as Init reads it on Linux.
	s.relays = []netip.Addr{netip.MustParseAddr("172.17.0.1")}
	req := func(remote string, mod func(*http.Request)) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/system/status", nil)
		r.RemoteAddr = remote
		r.Host = "192.168.1.10:8787"
		if mod != nil {
			mod(r)
		}
		return r
	}
	// A DNS rebinding page: the user's own browser, on the LAN, under the attacker's name.
	rebound := func(r *http.Request) {
		r.Host = "rebind.attacker.example:8787"
		r.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	cases := []struct {
		name    string
		r       *http.Request
		mode    Mode
		ok      bool
		kind    string
		wantErr bool
	}{
		{"anonymous", req("203.0.113.9:1", nil), ModeEnabled, false, "", false},
		{"header key", req("203.0.113.9:1", func(r *http.Request) { r.Header.Set("X-Api-Key", s.APIKey()) }), ModeEnabled, true, KindAPIKey, false},
		{"query key", req("203.0.113.9:1", func(r *http.Request) { r.URL.RawQuery = "apikey=" + s.APIKey() }), ModeEnabled, true, KindAPIKey, false},
		{"bad key beats cookie", req("203.0.113.9:1", func(r *http.Request) {
			r.Header.Set("X-Api-Key", "nope")
			r.AddCookie(&http.Cookie{Name: SessionCookie, Value: sess.Token})
		}), ModeEnabled, false, "", true},
		{"cookie", req("203.0.113.9:1", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: SessionCookie, Value: sess.Token}) }), ModeEnabled, true, KindSession, false},
		{"local, auth enabled", req("192.168.1.5:1", nil), ModeEnabled, false, "", false},
		{"local, bypass", req("192.168.1.5:1", nil), ModeLocalDisabled, true, KindLocal, false},
		{"public, bypass mode", req("203.0.113.9:1", nil), ModeLocalDisabled, false, "", false},
		{"spoofed XFF ignored", req("203.0.113.9:1", func(r *http.Request) { r.Header.Set("X-Forwarded-For", "127.0.0.1") }), ModeLocalDisabled, false, "", false},
		{"local, bypass, server name", req("192.168.1.5:1", func(r *http.Request) { r.Host = "tower.local:8787" }), ModeLocalDisabled, true, KindLocal, false},
		{"local, bypass, rebinding host", req("192.168.1.5:1", rebound), ModeLocalDisabled, false, "", false},
		{"rebinding host keeps the session", req("192.168.1.5:1", func(r *http.Request) {
			rebound(r)
			r.AddCookie(&http.Cookie{Name: SessionCookie, Value: sess.Token})
		}), ModeLocalDisabled, true, KindSession, false},
		// docker-proxy relays an IPv6 client from anywhere as the bridge gateway.
		{"gateway, bypass", req("172.17.0.1:40000", nil), ModeLocalDisabled, false, "", false},
		{"other bridge address, bypass", req("172.17.0.5:1", nil), ModeLocalDisabled, true, KindLocal, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := s.SetMode(ctx, c.mode); err != nil {
				t.Fatal(err)
			}
			p, ok, err := s.Identify(c.r)
			if ok != c.ok || p.Kind != c.kind || (err != nil) != c.wantErr {
				t.Fatalf("Identify = %+v, %v, %v; want ok=%v kind=%q err=%v", p, ok, err, c.ok, c.kind, c.wantErr)
			}
		})
	}
}

func TestRequireMiddleware(t *testing.T) {
	s, _ := newTestService(t)
	h := s.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := PrincipalFrom(r.Context())
		_, _ = w.Write([]byte(p.Kind))
	}))
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "unauthorized") {
		t.Fatalf("anonymous: %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Api-Key", s.APIKey())
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK || rec.Body.String() != KindAPIKey {
		t.Fatalf("with key: %d %s", rec.Code, rec.Body)
	}
}

func TestIsLocal(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1": true, "::1": true, "10.1.2.3": true, "172.16.0.1": true, "192.168.0.10": true,
		"fd00::1": true, "fe80::1": true, "169.254.1.1": true,
		"8.8.8.8": false, "172.32.0.1": false, "2001:db8::1": false,
	} {
		if got := IsLocal(netip.MustParseAddr(addr)); got != want {
			t.Errorf("IsLocal(%s) = %v, want %v", addr, got, want)
		}
	}
	if IsLocal(netip.Addr{}) {
		t.Error("invalid address counted as local")
	}
}

func TestLimiter(t *testing.T) {
	now := time.Now()
	l := NewLimiter(3, time.Minute)
	l.now = func() time.Time { return now }
	for range 3 {
		if blocked, _ := l.Blocked("a"); blocked {
			t.Fatal("blocked too early")
		}
		l.Fail("a")
	}
	if blocked, wait := l.Blocked("a"); !blocked || wait <= 0 {
		t.Fatalf("not blocked after 3 failures (wait %v)", wait)
	}
	if blocked, _ := l.Blocked("b"); blocked {
		t.Fatal("other client blocked")
	}
	now = now.Add(time.Minute + time.Second)
	if blocked, _ := l.Blocked("a"); blocked {
		t.Fatal("still blocked after the window")
	}
	l.Fail("a")
	l.Success("a")
	if len(l.fails) != 0 {
		t.Fatal("Success did not clear failures")
	}
}

// TestLimiterAttemptCountsBeforeTheCheck checks that Attempt tests and counts under one lock: of
// 50 attempts made at once, max may run however they interleave, and a blocked client gets the
// wait of its oldest failure. undo takes back its own attempt only, once, and after Success it
// has nothing to take back.
func TestLimiterAttemptCountsBeforeTheCheck(t *testing.T) {
	now := time.Now()
	l := NewLimiter(5, time.Minute)
	l.now = func() time.Time { return now }
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		undos []func()
	)
	for range 50 {
		wg.Go(func() {
			if undo, _, ok := l.Attempt("192.168.1.9"); ok {
				mu.Lock()
				undos = append(undos, undo)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if len(undos) != 5 {
		t.Fatalf("%d of 50 attempts made at once may run, want 5", len(undos))
	}
	now = now.Add(10 * time.Second)
	if _, wait, ok := l.Attempt("192.168.1.9"); ok || wait != 50*time.Second {
		t.Fatalf("blocked client: ok %v, wait %v, want 50s", ok, wait)
	}
	if blocked, _ := l.Blocked("192.168.1.9"); !blocked {
		t.Fatal("attempts in flight do not count as failures")
	}
	undos[0]()
	undos[0]()
	if n := len(l.fails["192.168.1.9"]); n != 4 {
		t.Fatalf("undo twice left %d failures, want 4", n)
	}
	undo, _, ok := l.Attempt("192.168.1.9")
	if !ok {
		t.Fatal("undo did not free the attempt")
	}
	l.Success("192.168.1.9")
	undo()
	if len(l.fails) != 0 {
		t.Fatalf("undo after Success: %v", l.fails)
	}
}

// TestLimiterCountsIPv6By64 checks that a device rotating through the addresses of its /64
// (SLAAC) shares one allowance, and that another /64 and IPv4 addresses do not.
func TestLimiterCountsIPv6By64(t *testing.T) {
	l := NewLimiter(5, time.Minute)
	for i := range 5 {
		l.Fail(netip.AddrFrom16([16]byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 1, 15: byte(i + 1)}).String())
	}
	if blocked, _ := l.Blocked("2001:db8:0:1:dead:beef:0:6"); !blocked {
		t.Fatal("a fresh address of the same /64 was not blocked")
	}
	if blocked, _ := l.Blocked("2001:db8:0:2::1"); blocked {
		t.Fatal("another /64 was blocked")
	}
	l.Success("2001:db8:0:1::99")
	if blocked, _ := l.Blocked("2001:db8:0:1::1"); blocked {
		t.Fatal("a login from the /64 did not clear its failures")
	}
	for in, want := range map[string]string{
		"192.168.1.5": "192.168.1.5", "::ffff:192.168.1.5": "192.168.1.5", "fe80::1%eth0": "fe80::/64", "invalid IP": "invalid IP",
	} {
		if got := limiterKey(in); got != want {
			t.Errorf("limiterKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestLimiterFullDropsOnlyTheOldest checks that a full limiter drops the least recently failing
// client, not every entry: filling it must not unblock a client that is locked out.
func TestLimiterFullDropsOnlyTheOldest(t *testing.T) {
	now := time.Now()
	l := NewLimiter(3, time.Hour)
	l.now = func() time.Time { return now }
	for i := range maxTracked - 1 {
		l.Fail(netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)}).String())
	}
	now = now.Add(time.Second)
	for range 3 {
		l.Fail("192.168.1.50")
	}
	if len(l.fails) != maxTracked {
		t.Fatalf("tracked %d, want %d", len(l.fails), maxTracked)
	}
	// Failing again for a known client makes no room.
	l.Fail("10.0.0.0")
	if len(l.fails) != maxTracked {
		t.Fatalf("a known client made room: tracked %d", len(l.fails))
	}
	now = now.Add(time.Second)
	l.Fail("203.0.113.1")
	if blocked, _ := l.Blocked("192.168.1.50"); !blocked {
		t.Fatal("a full limiter unblocked a locked-out client")
	}
	if len(l.fails) != maxTracked {
		t.Fatalf("tracked %d after a new client, want %d", len(l.fails), maxTracked)
	}
	for _, c := range []string{"192.168.1.50", "10.0.0.0", "203.0.113.1"} {
		if _, ok := l.fails[c]; !ok {
			t.Errorf("%s was dropped; only one of the least recently failing clients should be", c)
		}
	}
}

func isValidation(err error) bool {
	var v ValidationError
	return errors.As(err, &v)
}
