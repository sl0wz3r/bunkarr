package auth

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// DNS rebinding (ADR 0002). A web page the user opens on rebind.attacker.example can make that
// name resolve to this server's LAN address. The browser then treats the page and Bunkarr as one
// origin: its requests carry Sec-Fetch-Site: same-origin, so CrossOriginProtection lets them
// through, and the page reads every answer. The session cookie is host-only and never reaches such
// a page, but the two things that need no credential would: the local-address bypass (the request
// does come from the user's LAN device) and the first-run setup. Both are therefore allowed only
// when the Host header names this server in a way no outside DNS answer can produce.

// trustedSuffixes are domains the public DNS never answers, so a rebinding page cannot be served
// under a name in them: .localhost (RFC 6761; browsers resolve it to loopback themselves), .local
// (mDNS, RFC 6762; Unraid's default, tower.local), .home.arpa (RFC 8375) and .internal (reserved
// by ICANN for private use).
var trustedSuffixes = []string{".localhost", ".local", ".home.arpa", ".internal"}

// SetAllowedHosts adds names (BUNKARR_ALLOWED_HOSTS: tower.lan, a reverse proxy's
// bunkarr.example.com) that TrustedHost accepts besides the built-in ones. Call before serving.
func (s *Service) SetAllowedHosts(names []string) {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		if n = strings.ToLower(strings.TrimSpace(n)); n != "" {
			m[n] = true
		}
	}
	s.mu.Lock()
	s.allowedHosts = m
	s.mu.Unlock()
}

// TrustedHost reports whether r's Host header is one a DNS rebinding page cannot send: an IP
// address, localhost, a single-label name (tower, bunkarr-tower: only the LAN's own resolver
// answers it), a name under trustedSuffixes, or a name from SetAllowedHosts. Any other name may be
// a stranger's, rebound to this server's address.
func (s *Service) TrustedHost(r *http.Request) bool {
	h := hostName(r.Host)
	if h == "" {
		return false
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return true
	}
	if h == "localhost" || isLabel(h) {
		return true
	}
	for _, suf := range trustedSuffixes {
		if strings.HasSuffix(h, suf) {
			return true
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.allowedHosts[h]
}

// RequireTrustedHost refuses (403) a request whose Host header TrustedHost does not accept. It
// guards the first-run setup, which a rebinding page could otherwise claim while no user exists.
func (s *Service) RequireTrustedHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.TrustedHost(r) {
			h := truncate(hostName(r.Host), 100)
			s.warnThrottled(&s.refusedHostWarned, "Refused a request for a host name that is not Bunkarr's own (a DNS rebinding page, or a name missing from BUNKARR_ALLOWED_HOSTS)",
				"host", h, "path", r.URL.Path, "remote", ClientIP(r).String())
			writeJSONError(w, http.StatusForbidden, fmt.Sprintf(
				"%q is not one of Bunkarr's host names: open Bunkarr by its IP address or server name, or add the name to BUNKARR_ALLOWED_HOSTS", h))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostName is the lower-cased host of a Host header, without port or IPv6 brackets. A trailing dot
// is kept, so "tower." (an absolute name) matches neither a single label nor a suffix.
func hostName(hostport string) string {
	h := hostport
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		h = host
	} else if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		h = h[1 : len(h)-1]
	}
	return strings.ToLower(h)
}

// isLabel reports whether h is a single DNS label (letters, digits, '-', '_'; no dot).
func isLabel(h string) bool {
	if h == "" {
		return false
	}
	for _, c := range h {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
