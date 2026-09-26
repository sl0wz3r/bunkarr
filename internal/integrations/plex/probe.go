package plex

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/netguard"
)

// Connection probe (design §5). plex.tv lists several addresses per server: local ones, remote
// ones and relays, over https (plex.direct names) and sometimes http. Before the user picks one,
// Bunkarr tries them from where it runs: each candidate gets /identity WITHOUT the token, and only
// a candidate that answers as the chosen server (its machineIdentifier) receives the token, to
// /library/sections, and only over https (or http to a loopback address). An /identity answer over
// plain http proves nothing to an on-path attacker, so an http candidate is tested without the
// token; the user may still take it with "Use anyway" (design §5, D10).

// Probe limits.
const (
	// MaxCandidates is the most candidates Candidates returns for one resource.
	MaxCandidates = 16
	// ProbeConcurrency is how many candidates ProbeConnections tries at once.
	ProbeConcurrency = 4
)

// Probe timeouts (variables for the package's tests): per candidate and for the whole probe.
var (
	probeTimeout      = 5 * time.Second
	probeTotalTimeout = 15 * time.Second
)

// Candidate is one address at which a server may be reached.
type Candidate struct {
	// URI is the base URL ("https://192-168-1-10.<hash>.plex.direct:32400").
	URI string `json:"uri"`
	// Protocol is "https" or "http" (from the URI).
	Protocol string `json:"protocol"`
	// Local and Relay are plex.tv's flags for the connection it came from.
	Local bool `json:"local"`
	Relay bool `json:"relay"`
	// Derived is set for the http://<address>:<port> candidate Bunkarr adds for a local
	// plex.direct connection (home routers' DNS-rebind protection often blocks plex.direct names).
	Derived bool `json:"derived"`
}

// Candidates returns the addresses to probe for r, at most MaxCandidates:
//   - every plex.tv connection with an http or https URI (duplicates dropped);
//   - after each local *.plex.direct connection, a derived http://<address>:<port>;
//   - for a resource the user does not own, no local connection and nothing derived: those
//     addresses are in the owner's network, chosen by the owner.
func Candidates(r Resource) []Candidate {
	out := make([]Candidate, 0, len(r.Connections)*2)
	seen := map[string]bool{}
	add := func(c Candidate) {
		if len(out) < MaxCandidates && !seen[c.URI] {
			seen[c.URI] = true
			out = append(out, c)
		}
	}
	for _, conn := range r.Connections {
		if conn.Local && !r.Owned {
			continue
		}
		u, ok := candidateURL(conn.URI)
		if !ok {
			continue
		}
		add(Candidate{URI: u.String(), Protocol: u.Scheme, Local: conn.Local, Relay: conn.Relay})
		if !conn.Local || conn.Relay || !isPlexDirect(u.Hostname()) {
			continue
		}
		ip, err := netip.ParseAddr(strings.TrimSpace(conn.Address))
		port := conn.Port
		if port <= 0 {
			port = portOf(u)
		}
		if err != nil || port <= 0 || port > 65535 {
			continue
		}
		derived := "http://" + net.JoinHostPort(ip.Unmap().WithZone("").String(), strconv.Itoa(port))
		add(Candidate{URI: derived, Protocol: "http", Local: true, Derived: true})
	}
	return out
}

// candidateURL parses a plex.tv connection URI: http or https, a host, no user info, query or
// fragment; the path is kept without its trailing slash.
func candidateURL(raw string) (*url.URL, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, false
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, false
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u, true
}

func portOf(u *url.URL) int {
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return 0
		}
		return n
	}
	if u.Scheme == "https" {
		return 443
	}
	return 80
}

func isPlexDirect(host string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSuffix(host, ".")), ".plex.direct")
}

// ProbeResult is the outcome of probing one Candidate.
type ProbeResult struct {
	URI      string `json:"uri"`
	Local    bool   `json:"local"`
	Relay    bool   `json:"relay"`
	Derived  bool   `json:"derived"`
	Protocol string `json:"protocol"`
	// OK means the candidate answered as the chosen server and, when the token was sent,
	// accepted it.
	OK bool `json:"ok"`
	// IdentityMatches means /identity answered with the chosen server's machineIdentifier. The
	// token was sent only when it did, and only over https or to a loopback address.
	IdentityMatches bool `json:"identityMatches"`
	// TokenAccepted means /library/sections answered with the token.
	TokenAccepted bool   `json:"tokenAccepted"`
	Version       string `json:"version,omitempty"`
	// LatencyMs is the duration of the /identity request.
	LatencyMs int64 `json:"latencyMs"`
	// Message is a plain, user-facing sentence (never a token).
	Message string `json:"message"`
}

// ProbeConnections probes every candidate: ProbeConcurrency at a time, 5 s per candidate and
// 15 s in total. Each gets /identity without the token; token (when not empty) is sent, to
// /library/sections, only where the identity equals machineID and the connection is encrypted
// (tokenSafeOver): a matching http candidate is reported reachable with the token withheld.
// Every dial goes through netguard (unless opts.HTTPClient replaces the transport, which only
// tests do). The results are in the order of cands; RankResults orders them.
func ProbeConnections(ctx context.Context, opts Options, machineID, token string, cands []Candidate) []ProbeResult {
	opts = opts.withDefaults()
	opts.Timeout = probeTimeout
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, probeTotalTimeout)
	defer cancel()
	results := make([]ProbeResult, len(cands))
	sem := make(chan struct{}, ProbeConcurrency)
	var wg sync.WaitGroup
	for i, c := range cands {
		wg.Add(1)
		go func() {
			defer wg.Done()
			notTried := probeFailed(c, fmt.Sprintf("Not tried: the probe's %s limit was reached first.", probeTotalTimeout))
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[i] = notTried
				return
			}
			if ctx.Err() != nil {
				results[i] = notTried
				return
			}
			results[i] = probeOne(ctx, opts, machineID, token, c)
			if !results[i].OK && ctx.Err() != nil && parent.Err() == nil {
				results[i].Message = fmt.Sprintf("No answer before the probe's %s limit.", probeTotalTimeout)
			}
		}()
	}
	wg.Wait()
	return results
}

func probeFailed(c Candidate, msg string) ProbeResult {
	return ProbeResult{URI: c.URI, Local: c.Local, Relay: c.Relay, Derived: c.Derived, Protocol: c.Protocol, Message: msg}
}

func probeOne(ctx context.Context, opts Options, machineID, token string, c Candidate) ProbeResult {
	res := probeFailed(c, "")
	anon, err := New(c.URI, "", opts)
	if err != nil {
		res.Message = "This address is not a valid server URL."
		return res
	}
	start := time.Now()
	id, err := anon.Identity(ctx)
	res.LatencyMs = time.Since(start).Milliseconds()
	if err != nil {
		res.Message = describeProbeError(err, c)
		return res
	}
	res.Version = id.Version
	if want := strings.TrimSpace(machineID); want == "" || !strings.EqualFold(strings.TrimSpace(id.MachineIdentifier), want) {
		res.Message = "This address answers as another Plex server; the token was not sent."
		return res
	}
	res.IdentityMatches = true
	if token == "" {
		res.OK = true
		res.Message = fmt.Sprintf("Reachable (Plex Media Server %s). No token was sent.", id.Version)
		return res
	}
	if !tokenSafeOver(c.URI) {
		// Not the candidate's Protocol field: the URI is what the client dials.
		res.OK = true
		res.Message = fmt.Sprintf("Reachable (Plex Media Server %s). The token was not sent: this connection is not encrypted.", id.Version)
		return res
	}
	authed, err := New(c.URI, token, opts)
	if err != nil {
		res.Message = "This address is not a valid server URL."
		return res
	}
	if _, err := authed.Sections(ctx); err != nil {
		res.Message = describeProbeError(err, c)
		return res
	}
	res.TokenAccepted, res.OK = true, true
	res.Message = fmt.Sprintf("Connected (Plex Media Server %s).", id.Version)
	return res
}

// tokenSafeOver reports whether a token may be sent to uri during the probe: over https, or over
// http to a loopback IP literal (the request never leaves the host). Any other http address,
// a LAN one included, would carry the token in clear text before the user chose "Use anyway".
func tokenSafeOver(uri string) bool {
	u, err := url.Parse(uri)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return true
	case "http":
		ip, err := netip.ParseAddr(u.Hostname())
		return err == nil && ip.Unmap().IsLoopback()
	}
	return false
}

// describeProbeError turns a probe failure into a plain cause.
func describeProbeError(err error, c Candidate) string {
	var (
		dnsErr   *net.DNSError
		certErr  *tls.CertificateVerificationError
		hostErr  x509.HostnameError
		authErr  x509.UnknownAuthorityError
		invalErr x509.CertificateInvalidError
	)
	host := ""
	if u, perr := url.Parse(c.URI); perr == nil {
		host = u.Hostname()
	}
	switch {
	case errors.Is(err, netguard.ErrBlocked):
		return "Refused: this address is link-local or a cloud metadata address, where no Plex server lives."
	case errors.As(err, &dnsErr) && isPlexDirect(host):
		return "The plex.direct name could not be resolved. Home routers' DNS-rebind protection often blocks plex.direct names: use the local address (the derived http:// row), or allow plex.direct in the router's DNS settings."
	case errors.As(err, &dnsErr):
		return "The host name could not be resolved."
	case errors.As(err, &certErr), errors.As(err, &hostErr), errors.As(err, &authErr), errors.As(err, &invalErr):
		return "The server's TLS certificate is not valid for this address."
	case errors.Is(err, ErrUnauthorized):
		return "Plex rejected the token here (401 Unauthorized)."
	case errors.Is(err, ErrNotPlex):
		return "Something answered at this address, but not a Plex Media Server."
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("No answer within %s.", probeTimeout)
	case errors.Is(err, context.Canceled):
		return "The probe was cancelled."
	case errors.Is(err, syscall.ECONNREFUSED):
		return "Connection refused: nothing listens at this address and port."
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return "The address is not reachable from Bunkarr's network."
	}
	var perr *Error
	if errors.As(err, &perr) && perr.StatusCode != 0 {
		return fmt.Sprintf("The server answered with an unexpected status (%d).", perr.StatusCode)
	}
	return "Could not connect: " + err.Error()
}

// RankResults orders results best first and names the recommended URI ("" when none): working
// candidates first; then local non-relay, remote, and relay last; within a tier https before
// http, then by latency. A relay or an http candidate is never recommended.
func RankResults(results []ProbeResult) ([]ProbeResult, string) {
	out := slices.Clone(results)
	tier := func(r ProbeResult) int {
		switch {
		case r.Relay:
			return 2
		case r.Local:
			return 0
		}
		return 1
	}
	b2i := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	slices.SortStableFunc(out, func(a, b ProbeResult) int {
		if d := b2i(b.OK) - b2i(a.OK); d != 0 {
			return d
		}
		if d := tier(a) - tier(b); d != 0 {
			return d
		}
		if d := b2i(b.Protocol == "https") - b2i(a.Protocol == "https"); d != 0 {
			return d
		}
		switch {
		case a.LatencyMs < b.LatencyMs:
			return -1
		case a.LatencyMs > b.LatencyMs:
			return 1
		}
		return 0
	})
	for _, r := range out {
		if r.OK && !r.Relay && r.Protocol == "https" {
			return out, r.URI
		}
	}
	return out, ""
}
