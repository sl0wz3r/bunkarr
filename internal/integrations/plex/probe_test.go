package plex_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/integrations/plex"
	"github.com/sl0wz3r/bunkarr/internal/integrations/plex/plextest"
)

// The machine identifier of the recorded identity.json.
const recordedMachineID = "e004a7de01c19d82ea3a7fe4bfaa19e23a8344af"

func TestCandidates(t *testing.T) {
	owned := plex.Resource{Name: "Tower", ClientIdentifier: "abc", Owned: true, Connections: []plex.Connection{
		{URI: "https://192-168-1-10.abc.plex.direct:32400", Address: "192.168.1.10", Port: 32400, Protocol: "https", Local: true},
		{URI: "https://203-0-113-5.abc.plex.direct:32400/", Address: "203.0.113.5", Port: 32400, Protocol: "https"},
		{URI: "https://relay-1.abc.plex.direct:8443", Address: "198.51.100.1", Port: 8443, Protocol: "https", Relay: true},
		{URI: "http://192.168.1.10:32400", Address: "192.168.1.10", Port: 32400, Protocol: "http", Local: true},
		{URI: "https://fd00-1.abc.plex.direct:32400", Address: "fd00::1", Port: 32400, Protocol: "https", Local: true, IPv6: true},
		{URI: "ftp://nope", Address: "1.2.3.4", Port: 21},
		{URI: "", Address: "1.2.3.4", Port: 21},
		{URI: "https://user:pw@evil.example:1", Address: "1.2.3.4"},
	}}
	got := plex.Candidates(owned)
	want := []plex.Candidate{
		{URI: "https://192-168-1-10.abc.plex.direct:32400", Protocol: "https", Local: true},
		{URI: "http://192.168.1.10:32400", Protocol: "http", Local: true, Derived: true},
		{URI: "https://203-0-113-5.abc.plex.direct:32400", Protocol: "https"},
		{URI: "https://relay-1.abc.plex.direct:8443", Protocol: "https", Relay: true},
		// The plex.tv http connection equals the derived one: listed once.
		{URI: "https://fd00-1.abc.plex.direct:32400", Protocol: "https", Local: true},
		{URI: "http://[fd00::1]:32400", Protocol: "http", Local: true, Derived: true},
	}
	if !slices.Equal(got, want) {
		t.Errorf("Candidates =\n%+v\nwant\n%+v", got, want)
	}

	// A shared resource: no local connection and nothing derived.
	shared := owned
	shared.Owned = false
	got = plex.Candidates(shared)
	want = []plex.Candidate{
		{URI: "https://203-0-113-5.abc.plex.direct:32400", Protocol: "https"},
		{URI: "https://relay-1.abc.plex.direct:8443", Protocol: "https", Relay: true},
	}
	if !slices.Equal(got, want) {
		t.Errorf("shared Candidates =\n%+v\nwant\n%+v", got, want)
	}
	for _, c := range got {
		if c.Local || c.Derived {
			t.Errorf("shared resource candidate %+v is local or derived", c)
		}
	}

	// At most 16.
	many := plex.Resource{Owned: true}
	for i := range 20 {
		many.Connections = append(many.Connections, plex.Connection{
			URI: fmt.Sprintf("https://10-0-0-%d.abc.plex.direct:32400", i), Address: fmt.Sprintf("10.0.0.%d", i), Port: 32400, Local: true})
	}
	if n := len(plex.Candidates(many)); n != plex.MaxCandidates {
		t.Errorf("Candidates of 20 local plex.direct connections = %d, want %d", n, plex.MaxCandidates)
	}
}

// probeServer is a fake PMS whose requests are checked for the token.
func probeServer(t *testing.T, token, machineID string) *plextest.Server {
	t.Helper()
	srv := plextest.NewServer(t, token)
	srv.SetIdentity(machineID, "1.43.4.10903-e5521bd8c")
	return srv
}

func TestProbeSendsTheTokenOnlyWhereTheIdentityMatches(t *testing.T) {
	const token = "server-token-abcdef"
	right := probeServer(t, token, "machine-right")
	wrong := probeServer(t, token, "machine-other")
	cands := []plex.Candidate{
		{URI: wrong.URL, Protocol: "http", Local: true},
		{URI: right.URL, Protocol: "http", Local: true},
	}
	res := plex.ProbeConnections(context.Background(), plex.Options{}, "machine-right", token, cands)
	if len(res) != 2 {
		t.Fatalf("results = %d", len(res))
	}
	if res[0].OK || res[0].IdentityMatches || res[0].TokenAccepted || !strings.Contains(res[0].Message, "another Plex server") {
		t.Errorf("mismatched result = %+v", res[0])
	}
	if !res[1].OK || !res[1].IdentityMatches || !res[1].TokenAccepted || res[1].Version == "" {
		t.Errorf("matching result = %+v", res[1])
	}
	for _, r := range wrong.Requests() {
		if r.Header.Get("X-Plex-Token") != "" {
			t.Errorf("the mismatched server received X-Plex-Token on %s", r.Path)
		}
		if r.Path != plex.PathIdentity {
			t.Errorf("the mismatched server received %s", r.Path)
		}
	}
	var sawToken bool
	for _, r := range right.Requests() {
		if r.Path == plex.PathIdentity && r.Header.Get("X-Plex-Token") != "" {
			t.Error("/identity was sent with the token")
		}
		if r.Path == plex.PathSections && r.Header.Get("X-Plex-Token") == token {
			sawToken = true
		}
	}
	if !sawToken {
		t.Error("the matching server never received the token")
	}
}

func TestProbeResults(t *testing.T) {
	const token = "server-token-abcdef"
	ok := probeServer(t, token, recordedMachineID)
	rejects := probeServer(t, "another-token-123456", recordedMachineID)
	notPlex := probeServer(t, token, recordedMachineID)
	notPlex.Handle(plex.PathIdentity, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>router</html>"))
	})
	closed := freeAddr(t)
	cands := []plex.Candidate{
		{URI: ok.URL, Protocol: "http", Local: true},
		{URI: rejects.URL, Protocol: "http", Local: true},
		{URI: notPlex.URL, Protocol: "http", Local: true},
		{URI: "http://" + closed, Protocol: "http", Local: true},
		{URI: "http://169.254.169.254:32400", Protocol: "http", Local: true, Derived: true},
	}
	res := plex.ProbeConnections(context.Background(), plex.Options{}, recordedMachineID, token, cands)
	checks := []struct {
		ok      bool
		message string
	}{
		{true, "Connected (Plex Media Server 1.43.4"},
		{false, "401 Unauthorized"},
		{false, "not a Plex Media Server"},
		{false, "Connection refused"},
		{false, "link-local or a cloud metadata address"},
	}
	for i, c := range checks {
		if res[i].OK != c.ok || !strings.Contains(res[i].Message, c.message) {
			t.Errorf("result %d (%s) = %+v; want ok=%v and %q", i, cands[i].URI, res[i], c.ok, c.message)
		}
		if res[i].URI != cands[i].URI || res[i].Derived != cands[i].Derived || res[i].Protocol != cands[i].Protocol {
			t.Errorf("result %d does not describe its candidate: %+v", i, res[i])
		}
		if strings.Contains(res[i].Message, token) {
			t.Errorf("result %d message contains the token", i)
		}
	}
	// Without a token nothing but /identity is sent.
	res = plex.ProbeConnections(context.Background(), plex.Options{}, recordedMachineID, "", cands[:1])
	if !res[0].OK || res[0].TokenAccepted || !strings.Contains(res[0].Message, "No token was sent") {
		t.Errorf("tokenless result = %+v", res[0])
	}
}

// A metadata address is refused by netguard before any request: nothing listens there, and the
// refusal is immediate rather than a timeout.
func TestProbeRefusesMetadataAddressWithoutARequest(t *testing.T) {
	start := time.Now()
	res := plex.ProbeConnections(context.Background(), plex.Options{}, "m", "server-token-abcdef", []plex.Candidate{
		{URI: "http://169.254.169.254", Protocol: "http"},
		{URI: "https://[fe80::1]:32400", Protocol: "https"},
		{URI: "http://100.100.100.200:32400", Protocol: "http"},
	})
	for _, r := range res {
		if r.OK || !strings.Contains(r.Message, "Refused") {
			t.Errorf("%s: %+v, want the netguard refusal", r.URI, r)
		}
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("the refusals took %s; they must happen before connecting", d)
	}
}

func TestProbeRunsFourAtATime(t *testing.T) {
	var cur, peak atomic.Int32
	var mu sync.Mutex
	var servers []*plextest.Server
	for range 10 {
		srv := probeServer(t, "", "m")
		srv.Handle(plex.PathIdentity, func(w http.ResponseWriter, _ *http.Request) {
			n := cur.Add(1)
			mu.Lock()
			if n > peak.Load() {
				peak.Store(n)
			}
			mu.Unlock()
			time.Sleep(50 * time.Millisecond)
			cur.Add(-1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"MediaContainer":{"machineIdentifier":"m","version":"1"}}`))
		})
		servers = append(servers, srv)
	}
	var cands []plex.Candidate
	for _, s := range servers {
		cands = append(cands, plex.Candidate{URI: s.URL, Protocol: "http"})
	}
	res := plex.ProbeConnections(context.Background(), plex.Options{}, "m", "", cands)
	for _, r := range res {
		if !r.OK {
			t.Errorf("%+v", r)
		}
	}
	if p := peak.Load(); p > plex.ProbeConcurrency || p < 2 {
		t.Errorf("peak concurrency = %d, want 2..%d", p, plex.ProbeConcurrency)
	}
}

func TestProbeHTTPSIsRecommended(t *testing.T) {
	const token = "server-token-abcdef"
	tlsSrv := plextest.NewTLSServer(t, token)
	plain := probeServer(t, token, recordedMachineID)
	cands := []plex.Candidate{
		{URI: plain.URL, Protocol: "http", Local: true, Derived: true},
		{URI: tlsSrv.URL, Protocol: "https", Local: true},
	}
	res := plex.ProbeConnections(context.Background(), plex.Options{HTTPClient: tlsSrv.Client()}, recordedMachineID, token, cands)
	ranked, rec := plex.RankResults(res)
	if rec != tlsSrv.URL || ranked[0].URI != tlsSrv.URL {
		t.Fatalf("recommended = %q, ranked = %+v; want the https candidate", rec, ranked)
	}
	// Without a trusting client, the self-signed certificate is refused with a plain cause.
	res = plex.ProbeConnections(context.Background(), plex.Options{}, recordedMachineID, token, cands[1:])
	if res[0].OK || !strings.Contains(res[0].Message, "TLS certificate") {
		t.Errorf("self-signed result = %+v", res[0])
	}
}

// An /identity match over plain http proves nothing to an on-path attacker, so a LAN or remote
// http candidate (the derived one included) is tested without the token: the server token, or the
// plex.tv account token, never crosses the network in clear text before "Use anyway". Only a
// loopback http address, which never leaves the host, still gets it.
func TestProbeNeverSendsTheTokenOverNonLoopbackHTTP(t *testing.T) {
	const token = "account-token-abcdef"
	lan := probeServer(t, token, recordedMachineID)
	loop := probeServer(t, token, recordedMachineID)
	owned := plex.Resource{ClientIdentifier: recordedMachineID, Owned: true, Connections: []plex.Connection{
		{URI: "https://192-168-1-10.abc.plex.direct:32400", Address: "192.168.1.10", Port: 32400, Protocol: "https", Local: true},
		{URI: "http://203.0.113.5:32400", Address: "203.0.113.5", Port: 32400, Protocol: "http"},
	}}
	var cands []plex.Candidate
	for _, c := range plex.Candidates(owned) {
		if c.Protocol == "http" {
			cands = append(cands, c)
		}
	}
	if len(cands) != 2 || !cands[0].Derived || cands[0].URI != "http://192.168.1.10:32400" {
		t.Fatalf("http candidates = %+v", cands)
	}
	cands = append(cands, plex.Candidate{URI: loop.URL, Protocol: "http", Local: true})
	// Every non-loopback address is answered by lan (the transport dials it whatever the host).
	lanAddr := strings.TrimPrefix(lan.URL, "http://")
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		if !strings.HasPrefix(addr, "127.0.0.1:") {
			addr = lanAddr
		}
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}}
	res := plex.ProbeConnections(context.Background(), plex.Options{HTTPClient: hc}, recordedMachineID, token, cands)
	for _, r := range res[:2] {
		if !r.OK || !r.IdentityMatches || r.TokenAccepted || !strings.Contains(r.Message, "token was not sent") {
			t.Errorf("%s: %+v; want reachable with the token withheld", r.URI, r)
		}
	}
	if reqs := lan.Requests(); len(reqs) != 2 {
		t.Errorf("the LAN/remote server received %d requests, want 2 (/identity each)", len(reqs))
	}
	for _, r := range lan.Requests() {
		if r.Header.Get("X-Plex-Token") != "" || r.Path != plex.PathIdentity {
			t.Errorf("an http non-loopback candidate received %s with X-Plex-Token %q", r.Path, r.Header.Get("X-Plex-Token"))
		}
	}
	if r := res[2]; !r.OK || !r.TokenAccepted {
		t.Errorf("loopback http: %+v; want the token tested", r)
	}
	// The withheld rows stay unrecommended and ranked as before.
	if _, rec := plex.RankResults(res); rec != "" {
		t.Errorf("recommended = %q, want none", rec)
	}
}

func TestRankResults(t *testing.T) {
	in := []plex.ProbeResult{
		{URI: "https://relay", Relay: true, Protocol: "https", OK: true, LatencyMs: 1},
		{URI: "http://derived", Local: true, Derived: true, Protocol: "http", OK: true, LatencyMs: 2},
		{URI: "https://remote", Protocol: "https", OK: true, LatencyMs: 5},
		{URI: "https://local-slow", Local: true, Protocol: "https", OK: true, LatencyMs: 30},
		{URI: "https://local-fast", Local: true, Protocol: "https", OK: true, LatencyMs: 3},
		{URI: "https://local-broken", Local: true, Protocol: "https", OK: false, LatencyMs: 0},
		{URI: "http://remote-http", Protocol: "http", OK: true, LatencyMs: 1},
	}
	ranked, rec := plex.RankResults(in)
	var order []string
	for _, r := range ranked {
		order = append(order, r.URI)
	}
	want := []string{"https://local-fast", "https://local-slow", "http://derived", "https://remote", "http://remote-http", "https://relay", "https://local-broken"}
	if !slices.Equal(order, want) {
		t.Errorf("order = %v\nwant    %v", order, want)
	}
	if rec != "https://local-fast" {
		t.Errorf("recommended = %q", rec)
	}
	// Relays and http are never recommended, even when they are all that works.
	_, rec = plex.RankResults([]plex.ProbeResult{
		{URI: "http://derived", Local: true, Derived: true, Protocol: "http", OK: true},
		{URI: "https://relay", Relay: true, Protocol: "https", OK: true},
		{URI: "https://broken", Protocol: "https"},
	})
	if rec != "" {
		t.Errorf("recommended = %q, want none", rec)
	}
	if in[0].URI != "https://relay" {
		t.Error("RankResults changed its input")
	}
}

// freeAddr returns a loopback address where nothing listens.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}
