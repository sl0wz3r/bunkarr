package netguard

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCheckDialAddress(t *testing.T) {
	for addr, blocked := range map[string]bool{
		"169.254.169.254:80": true, "169.254.1.1:443": true, "[fe80::1%en0]:80": true, "[fe80::1]:80": true,
		"[fd00:ec2::254]:80": true, "[::ffff:169.254.169.254]:80": true, "100.100.100.200:80": true,
		"127.0.0.1:80": false, "192.168.1.10:8080": false, "[::1]:25": false, "10.0.0.1:587": false,
		"100.100.100.100:53": false, "[fd00:ec2::253]:80": false, "not-an-address": true,
	} {
		err := CheckDialAddress("tcp", addr, nil)
		if (err != nil) != blocked {
			t.Errorf("CheckDialAddress(%s) = %v, want blocked=%v", addr, err, blocked)
		}
		if err != nil && !errors.Is(err, ErrBlocked) {
			t.Errorf("CheckDialAddress(%s) = %v, which does not match ErrBlocked", addr, err)
		}
	}
}

func TestBlocked(t *testing.T) {
	tests := []struct {
		ip   string
		want bool
	}{
		{"169.254.169.254", true},
		{"169.254.0.0", true},
		{"169.255.0.1", false},
		{"fe80::abcd", true},
		{"fe80::1%eth0", true},
		{"::ffff:169.254.169.254", true},
		{"fd00:ec2::254", true},
		{"100.100.100.200", true},
		{"8.8.8.8", false},
		{"::1", false},
	}
	for _, tt := range tests {
		if got := Blocked(netip.MustParseAddr(tt.ip)); got != tt.want {
			t.Errorf("Blocked(%s) = %v, want %v", tt.ip, got, tt.want)
		}
	}
}

// With an HTTP(S) proxy configured, the dial check only sees the proxy's address; Proxy must
// refuse a blocked destination before the request reaches the proxy.
func TestProxyRefusesBlockedDestinations(t *testing.T) {
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxied.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(proxy.Close)
	pu, _ := url.Parse(proxy.URL)
	tr := &http.Transport{
		Proxy:       Proxy(http.ProxyURL(pu)),
		DialContext: NewDialer(5 * time.Second).DialContext,
	}
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	for _, u := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://[fd00:ec2::254]/latest/meta-data/",
		"http://[::ffff:169.254.169.254]/",
		"http://100.100.100.200/",
	} {
		resp, err := client.Get(u)
		if err == nil {
			_ = resp.Body.Close()
			t.Fatalf("GET %s through the proxy succeeded", u)
		}
		if !errors.Is(err, ErrBlocked) || !strings.Contains(err.Error(), "not allowed") {
			t.Errorf("GET %s: err = %v, want a refusal", u, err)
		}
	}
	if n := proxied.Load(); n != 0 {
		t.Fatalf("the proxy received %d request(s) for blocked destinations", n)
	}
	// An ordinary destination still goes through the proxy.
	resp, err := client.Get("http://192.0.2.10/")
	if err != nil {
		t.Fatalf("GET through the proxy: %v", err)
	}
	_ = resp.Body.Close()
	if proxied.Load() != 1 {
		t.Fatalf("the proxy received %d requests, want 1", proxied.Load())
	}
}

func TestProxyWithoutProxyLeavesDialCheck(t *testing.T) {
	f := Proxy(func(*http.Request) (*url.URL, error) { return nil, nil })
	req, _ := http.NewRequest(http.MethodGet, "http://169.254.169.254/", nil)
	if u, err := f(req); u != nil || err != nil {
		t.Fatalf("Proxy() = %v, %v; want no proxy and no error (the dial check applies)", u, err)
	}
}

// A proxied request whose host NAME resolves to a metadata address is refused too.
func TestProxyRefusesNameResolvingToBlockedAddress(t *testing.T) {
	old := resolver
	resolver = fakeResolver(t, map[string]string{"169-254-169-254.0123abcd.plex.direct.": "169.254.169.254"})
	t.Cleanup(func() { resolver = old })
	pu, _ := url.Parse("http://127.0.0.1:1")
	f := Proxy(http.ProxyURL(pu))
	req, _ := http.NewRequest(http.MethodGet, "https://169-254-169-254.0123abcd.plex.direct:32400/identity", nil)
	if u, err := f(req); u != nil || !errors.Is(err, ErrBlocked) {
		t.Fatalf("Proxy() = %v, %v; want a refusal", u, err)
	}
	// A name that does not resolve is left to the proxy.
	req, _ = http.NewRequest(http.MethodGet, "http://unknown.example/", nil)
	if u, err := f(req); u == nil || err != nil {
		t.Fatalf("Proxy(unresolvable) = %v, %v; want the proxy", u, err)
	}
}

// The dial check runs after name resolution: a plex.direct-style name that resolves to the cloud
// metadata address is refused without a connection, while a name resolving to an allowed address
// connects.
func TestDialRefusesNameResolvingToMetadataAddress(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var accepted atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			_ = c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	d := NewDialer(5 * time.Second)
	d.Resolver = fakeResolver(t, map[string]string{
		"169-254-169-254.0123abcd.plex.direct.": "169.254.169.254",
		"127-0-0-1.0123abcd.plex.direct.":       "127.0.0.1",
	})
	ctx := context.Background()
	if c, err := d.DialContext(ctx, "tcp", net.JoinHostPort("169-254-169-254.0123abcd.plex.direct", "32400")); err == nil {
		_ = c.Close()
		t.Fatal("dial to a name resolving to 169.254.169.254 succeeded")
	} else if !errors.Is(err, ErrBlocked) || !strings.Contains(err.Error(), "169.254.169.254") {
		t.Fatalf("dial err = %v, want the guard's refusal naming the address", err)
	}

	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort("127-0-0-1.0123abcd.plex.direct", port))
	if err != nil {
		t.Fatalf("dial to a name resolving to 127.0.0.1: %v", err)
	}
	_ = c.Close()

	// Through an HTTP client, as the Plex probe uses it: no request is sent.
	tr := &http.Transport{DialContext: d.DialContext, Proxy: Proxy(func(*http.Request) (*url.URL, error) { return nil, nil })}
	hc := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	resp, err := hc.Get("http://169-254-169-254.0123abcd.plex.direct:32400/identity")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("GET to a name resolving to 169.254.169.254 succeeded")
	}
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("GET err = %v, want ErrBlocked", err)
	}
	deadline := time.Now().Add(time.Second)
	for accepted.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := accepted.Load(); n != 1 {
		t.Fatalf("the listener accepted %d connections, want 1 (only the allowed name)", n)
	}
}

func TestNewTransportGuardsDialsAndProxies(t *testing.T) {
	tr := NewTransport()
	if tr.DialContext == nil || tr.Proxy == nil {
		t.Fatal("NewTransport must set DialContext and Proxy")
	}
	hc := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	resp, err := hc.Get("http://169.254.169.254/latest/meta-data/")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("GET 169.254.169.254 through NewTransport succeeded")
	}
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, want ErrBlocked", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	t.Cleanup(srv.Close)
	resp, err = hc.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET loopback through NewTransport: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

// fakeResolver returns a pure-Go resolver whose DNS server is in-process: it answers A queries for
// the names in records (fully qualified, lower case) and NXDOMAIN for every other name.
func fakeResolver(t *testing.T, records map[string]string) *net.Resolver {
	t.Helper()
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			client, server := net.Pipe()
			go serveDNS(server, records)
			return client, nil
		},
	}
}

// serveDNS answers DNS queries on a stream connection (2-byte length framing, which the Go
// resolver uses for connections that are not packet connections).
func serveDNS(c net.Conn, records map[string]string) {
	defer c.Close()
	for {
		var n uint16
		if err := binary.Read(c, binary.BigEndian, &n); err != nil {
			return
		}
		msg := make([]byte, n)
		if _, err := io.ReadFull(c, msg); err != nil {
			return
		}
		resp := dnsAnswer(msg, records)
		if resp == nil {
			return
		}
		out := make([]byte, 2, 2+len(resp))
		binary.BigEndian.PutUint16(out, uint16(len(resp)))
		if _, err := c.Write(append(out, resp...)); err != nil {
			return
		}
	}
}

// dnsAnswer builds the response to one query: the header and question echoed, plus an A record
// when the name is known and the type is A. Unknown names get NXDOMAIN.
func dnsAnswer(q []byte, records map[string]string) []byte {
	if len(q) < 12 {
		return nil
	}
	// Question name.
	i := 12
	var labels []string
	for i < len(q) && q[i] != 0 {
		l := int(q[i])
		if i+1+l > len(q) {
			return nil
		}
		labels = append(labels, string(q[i+1:i+1+l]))
		i += 1 + l
	}
	i++ // the root label
	if i+4 > len(q) {
		return nil
	}
	qtype := binary.BigEndian.Uint16(q[i:])
	question := q[12 : i+4]
	name := strings.ToLower(strings.Join(labels, ".")) + "."
	addr, known := records[name]

	resp := make([]byte, 12, 12+len(question)+16)
	copy(resp, q[:2]) // id
	flags := uint16(0x8180)
	if !known {
		flags |= 3 // NXDOMAIN
	}
	binary.BigEndian.PutUint16(resp[2:], flags)
	binary.BigEndian.PutUint16(resp[4:], 1) // qdcount
	var ip netip.Addr
	if known {
		ip = netip.MustParseAddr(addr)
	}
	answer := known && qtype == 1 && ip.Is4()
	if answer {
		binary.BigEndian.PutUint16(resp[6:], 1)
	}
	resp = append(resp, question...)
	if answer {
		a := ip.As4()
		resp = append(resp, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4, a[0], a[1], a[2], a[3])
	}
	return resp
}
