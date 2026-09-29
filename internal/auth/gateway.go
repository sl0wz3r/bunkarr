package auth

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// procNet holds the kernel's routing tables (a variable for tests).
var procNet = "/proc/net"

// rtfGateway is RTF_GATEWAY, the routing-table flag of a route through a next hop.
const rtfGateway = 0x2

// relayAddrs returns the next hops of the container's routes (IPv4 /proc/net/route, IPv6
// /proc/net/ipv6_route): addresses that relay connections instead of making them. On Docker's
// bridge network the default gateway is the host's end of the bridge, and every connection Docker
// relays arrives from it: docker-proxy (the userland proxy, on by default) carries IPv6 clients to
// a container on an IPv4-only bridge, as Unraid's template uses, over a new IPv4 connection, so a
// client anywhere on the internet shows up as 172.17.0.1, a private address. The local-address
// bypass never applies to them. Outside Linux, without /proc, there are none.
func relayAddrs() []netip.Addr {
	var out []netip.Addr
	add := func(a netip.Addr) {
		if a.IsValid() && !a.IsUnspecified() && !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	// Iface Destination Gateway Flags RefCnt Use Metric Mask MTU Window IRTT; the addresses are
	// the network-order bytes printed as a native-endian number.
	eachRoute(filepath.Join(procNet, "route"), func(f []string) {
		if len(f) < 4 || !gatewayFlag(f[3]) {
			return
		}
		v, err := strconv.ParseUint(f[2], 16, 32)
		if err != nil {
			return
		}
		var b [4]byte
		binary.NativeEndian.PutUint32(b[:], uint32(v))
		add(netip.AddrFrom4(b))
	})
	// Destination PrefixLen Source PrefixLen NextHop Metric RefCnt Use Flags Iface; the addresses
	// are 32 hex digits in network order.
	eachRoute(filepath.Join(procNet, "ipv6_route"), func(f []string) {
		if len(f) < 9 || !gatewayFlag(f[8]) {
			return
		}
		b, err := hex.DecodeString(f[4])
		if err != nil || len(b) != 16 {
			return
		}
		add(netip.AddrFrom16([16]byte(b)))
	})
	return out
}

func eachRoute(path string, fn func(fields []string)) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fn(strings.Fields(sc.Text()))
	}
}

func gatewayFlag(hexFlags string) bool {
	v, err := strconv.ParseUint(hexFlags, 16, 32)
	return err == nil && v&rtfGateway != 0
}
