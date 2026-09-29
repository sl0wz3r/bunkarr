package auth

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestTrustedHost checks the Host names a DNS rebinding page cannot send (ADR 0002): IP
// addresses, localhost, single labels, the reserved suffixes and BUNKARR_ALLOWED_HOSTS; any other
// name, which the public DNS can point at this server, is refused.
func TestTrustedHost(t *testing.T) {
	s, _ := newTestService(t)
	s.SetAllowedHosts([]string{" Bunkarr.Example.com ", "tower.lan", ""})
	for host, want := range map[string]bool{
		"192.168.1.10:8787": true, "192.168.1.10": true, "[fd00::10]:8787": true, "[::1]": true, "127.0.0.1:8787": true,
		"localhost:8787": true, "LOCALHOST": true, "bunkarr.localhost": true,
		"tower:8787": true, "bunkarr-tower": true, "Tower": true,
		"tower.local:8787": true, "nas.home.arpa": true, "bunkarr.internal": true,
		"bunkarr.example.com": true, "BUNKARR.EXAMPLE.COM:443": true, "tower.lan:8787": true,

		"rebind.attacker.example:8787": false, "attacker.example": false, "192.168.1.10.nip.io": false,
		"example.com": false, "sub.bunkarr.example.com": false, "tower.lan.attacker.example": false,
		"local": true, "evil-local.example": false, "tower.": false, "tower.local.": false,
		"": false, "a:b:c": false, "tower%": false,
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Host = host
		if got := s.TrustedHost(r); got != want {
			t.Errorf("TrustedHost(%q) = %v, want %v", host, got, want)
		}
	}
}

// TestRequireTrustedHost checks the first-run setup's guard: a rebinding page's same-origin
// request gets 403 with the way out, one of Bunkarr's own names reaches the handler.
func TestRequireTrustedHost(t *testing.T) {
	s, _ := newTestService(t)
	h := s.RequireTrustedHost(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusCreated) }))
	for host, want := range map[string]int{"rebind.attacker.example:8787": http.StatusForbidden, "192.168.1.10:8787": http.StatusCreated} {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", nil)
		r.Host = host
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != want {
			t.Fatalf("%s: HTTP %d %s, want %d", host, rec.Code, rec.Body, want)
		}
		if want == http.StatusForbidden && !strings.Contains(rec.Body.String(), "BUNKARR_ALLOWED_HOSTS") {
			t.Fatalf("%s: the refusal does not say how to allow a name: %s", host, rec.Body)
		}
	}
}

// TestRelayAddrs reads the gateways from fixture routing tables: Docker's bridge gateway (IPv4,
// printed native-endian) and an IPv6 default route; routes without a next hop are skipped.
func TestRelayAddrs(t *testing.T) {
	dir := t.TempDir()
	old := procNet
	procNet = dir
	t.Cleanup(func() { procNet = old })
	if got := relayAddrs(); len(got) != 0 {
		t.Fatalf("without routing tables: %v", got)
	}
	v4 := func(a string) string {
		b := netip.MustParseAddr(a).As4()
		return fmt.Sprintf("%08X", binary.NativeEndian.Uint32(b[:]))
	}
	route := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"eth0\t00000000\t" + v4("172.17.0.1") + "\t0003\t0\t0\t0\t00000000\t0\t0\t0\n" +
		"eth0\t" + v4("172.17.0.0") + "\t00000000\t0001\t0\t0\t0\t" + v4("255.255.0.0") + "\t0\t0\t0\n" +
		"eth1\t" + v4("10.9.0.0") + "\t" + v4("172.17.0.1") + "\t0003\t0\t0\t0\t" + v4("255.255.0.0") + "\t0\t0\t0\n"
	ipv6 := "00000000000000000000000000000000 00 00000000000000000000000000000000 00 fd000000000000000000000000000001 00000400 00000001 00000000 00000003 eth0\n" +
		"fd000000000000000000000000000000 40 00000000000000000000000000000000 00 00000000000000000000000000000000 00000100 00000001 00000000 00000001 eth0\n" +
		"00000000000000000000000000000000 00 00000000000000000000000000000000 00 00000000000000000000000000000000 ffffffff 00000001 00000000 00200200 lo\n"
	for name, body := range map[string]string{"route": route, "ipv6_route": ipv6} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want := []netip.Addr{netip.MustParseAddr("172.17.0.1"), netip.MustParseAddr("fd00::1")}
	if got := relayAddrs(); !slices.Equal(got, want) {
		t.Fatalf("relayAddrs = %v, want %v", got, want)
	}
	s, _ := newTestService(t)
	if !slices.Equal(s.relays, want) {
		t.Fatalf("Init read the gateways %v, want %v", s.relays, want)
	}
	if err := s.SetMode(context.Background(), ModeLocalDisabled); err != nil {
		t.Fatal(err)
	}
	for remote, want := range map[string]bool{"172.17.0.1:5000": false, "[fd00::1]:5000": false, "172.17.0.2:5000": true} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/system/status", nil)
		r.RemoteAddr, r.Host = remote, "192.168.1.10:8787"
		if _, ok, _ := s.Identify(r); ok != want {
			t.Errorf("local bypass for %s: %v, want %v", remote, ok, want)
		}
	}
}
