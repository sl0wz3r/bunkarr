package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"time"
)

// SessionCookie is the login cookie's name.
const SessionCookie = "bunkarr_session"

// Principal kinds.
const (
	KindAPIKey  = "apikey"
	KindSession = "session"
	KindLocal   = "local"
)

// Principal is who made an authenticated request.
type Principal struct {
	Kind string
	// User is set for session logins.
	User *User
	// Token is the session token (KindSession only).
	Token string
}

type principalKey struct{}

// PrincipalFrom returns the request's principal set by Require.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// ErrBadAPIKey means a request carried an API key that is not the current one.
var ErrBadAPIKey = errors.New("invalid API key")

// Identify works out who made r. ok is false for an anonymous request. A wrong API key is an
// error even when a valid session cookie is present, so scripts notice a stale key.
func (s *Service) Identify(r *http.Request) (p Principal, ok bool, err error) {
	key := r.Header.Get("X-Api-Key")
	if key == "" {
		key = r.URL.Query().Get("apikey")
	}
	if key != "" {
		if s.CheckAPIKey(key) {
			return Principal{Kind: KindAPIKey}, true, nil
		}
		return Principal{}, false, ErrBadAPIKey
	}
	if c, err := r.Cookie(SessionCookie); err == nil && c.Value != "" {
		u, ok, err := s.SessionUser(r.Context(), c.Value)
		if err != nil {
			return Principal{}, false, err
		}
		if ok {
			return Principal{Kind: KindSession, User: &u, Token: c.Value}, true, nil
		}
	}
	if s.Mode() == ModeLocalDisabled && IsLocal(ClientIP(r)) {
		return Principal{Kind: KindLocal}, true, nil
	}
	return Principal{}, false, nil
}

// Require rejects unauthenticated requests with 401 and stores the principal in the context.
func (s *Service) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok, err := s.Identify(r)
		if err != nil && !errors.Is(err, ErrBadAPIKey) {
			s.log.Error("Authentication check failed", "error", err)
			writeJSONError(w, http.StatusInternalServerError, "authentication check failed")
			return
		}
		if !ok {
			msg := "unauthorized"
			if errors.Is(err, ErrBadAPIKey) {
				msg = ErrBadAPIKey.Error()
			}
			writeJSONError(w, http.StatusUnauthorized, msg)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}

// SetSessionCookie writes the login cookie. Secure is set for HTTPS requests, including those a
// reverse proxy terminated (X-Forwarded-Proto: https).
func SetSessionCookie(w http.ResponseWriter, r *http.Request, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteStrictMode,
	})
}

// ClearSessionCookie removes the login cookie.
func ClearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteStrictMode,
	})
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

// ClientIP is the TCP peer address. Proxy headers (X-Forwarded-For) are deliberately NOT trusted:
// anyone could send them to pose as a local client. Behind a reverse proxy every client appears
// as the proxy's address (see DEFERRED.md, trusted proxies).
func ClientIP(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return ip.Unmap()
}

// IsLocal reports whether ip is loopback, private (RFC 1918, fc00::/7) or link-local.
func IsLocal(ip netip.Addr) bool {
	if !ip.IsValid() {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
}
