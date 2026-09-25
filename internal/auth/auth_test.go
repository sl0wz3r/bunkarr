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

	req := func(remote string, mod func(*http.Request)) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/system/status", nil)
		r.RemoteAddr = remote
		if mod != nil {
			mod(r)
		}
		return r
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

func isValidation(err error) bool {
	var v ValidationError
	return errors.As(err, &v)
}
