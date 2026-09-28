package rclone

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/sl0wz3r/bunkarr/internal/engines"
)

func newHostKey(t *testing.T, ecdsaKey bool) (engines.HostKey, ssh.PublicKey) {
	t.Helper()
	var pub any
	if ecdsaKey {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		pub = &k.PublicKey
	} else {
		p, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		pub = p
	}
	pk, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return engines.HostKey{Type: pk.Type(), Key: base64.StdEncoding.EncodeToString(pk.Marshal())}, pk
}

// TestKnownHosts: one line per pinned key rendered by knownhosts.Line (port 2222 -> [h]:2222,
// port 22 -> h), nothing without keys, and no key value or host can add a line or a pattern
// (§4.4, SE-3).
func TestKnownHosts(t *testing.T) {
	ed, edPK := newHostKey(t, false)
	ec, ecPK := newHostKey(t, true)
	got, err := KnownHosts("nas.example.org", 2222, []engines.HostKey{ed, ec})
	if err != nil {
		t.Fatal(err)
	}
	want := "[nas.example.org]:2222 " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(edPK))) + "\n" +
		"[nas.example.org]:2222 " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(ecPK))) + "\n"
	if got != want {
		t.Errorf("known_hosts:\n%s\nwant:\n%s", got, want)
	}
	if got, _ := KnownHosts("10.0.0.5", 22, []engines.HostKey{ed}); !strings.HasPrefix(got, "10.0.0.5 ssh-ed25519 ") || strings.Count(got, "\n") != 1 {
		t.Errorf("port 22: %q", got)
	}
	if got, _ := KnownHosts("fd00::5", 2222, []engines.HostKey{ed}); !strings.HasPrefix(got, "[fd00::5]:2222 ") {
		t.Errorf("IPv6: %q", got)
	}
	if got, _ := KnownHosts("h", 0, []engines.HostKey{ed}); !strings.HasPrefix(got, "h ssh-ed25519") {
		t.Errorf("default port: %q", got)
	}

	injected := ed
	injected.Key += "\n@cert-authority * ssh-ed25519 AAAA"
	for _, tc := range []struct {
		name string
		host string
		port int
		keys []engines.HostKey
	}{
		{"no keys", "h", 22, nil},
		{"type mismatch", "h", 22, []engines.HostKey{{Type: "ssh-rsa", Key: ed.Key}}},
		{"key with a line break", "h", 22, []engines.HostKey{injected}},
		{"not base64", "h", 22, []engines.HostKey{{Type: "ssh-ed25519", Key: "!!"}}},
		{"host with a space", "h evil", 22, []engines.HostKey{ed}},
		{"host with a newline", "h\n@cert-authority *", 22, []engines.HostKey{ed}},
		{"wildcard host", "*", 22, []engines.HostKey{ed}},
		{"host with a comma", "a,b", 22, []engines.HostKey{ed}},
		{"bracketed host", "[h]", 22, []engines.HostKey{ed}},
		{"port out of range", "h", 70000, []engines.HostKey{ed}},
	} {
		if out, err := KnownHosts(tc.host, tc.port, tc.keys); err == nil {
			t.Errorf("%s: accepted: %q", tc.name, out)
		}
	}
}

func TestFingerprint(t *testing.T) {
	ed, pk := newHostKey(t, false)
	got, err := Fingerprint(ed)
	if err != nil || got != ssh.FingerprintSHA256(pk) || !strings.HasPrefix(got, "SHA256:") {
		t.Errorf("Fingerprint = %q, %v", got, err)
	}
	if _, err := Fingerprint(engines.HostKey{Type: "ssh-ed25519", Key: "AAAA"}); err == nil {
		t.Error("a broken key has a fingerprint")
	}
}
