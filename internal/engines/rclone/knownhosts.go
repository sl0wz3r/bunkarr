package rclone

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/sl0wz3r/bunkarr/internal/engines"
)

// ParseHostKey parses a pinned host key (base64 SSH wire format) and checks that its type is the
// one recorded (§4.2, §4.6).
func ParseHostKey(k engines.HostKey) (ssh.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(k.Key)
	if err != nil {
		return nil, fmt.Errorf("host key: not base64: %w", err)
	}
	pk, err := ssh.ParsePublicKey(raw)
	if err != nil {
		return nil, fmt.Errorf("host key: %w", err)
	}
	if pk.Type() != k.Type {
		return nil, fmt.Errorf("host key: type %q does not match the key's type %q", k.Type, pk.Type())
	}
	return pk, nil
}

// Fingerprint returns a pinned host key's SHA256 fingerprint ("SHA256:…"), as the host-key scan
// shows it for confirmation (§4.6).
func Fingerprint(k engines.HostKey) (string, error) {
	pk, err := ParseHostKey(k)
	if err != nil {
		return "", err
	}
	return ssh.FingerprintSHA256(pk), nil
}

// KnownHosts renders the known_hosts file of an SFTP remote: one line per pinned key, each
// rendered only by knownhosts.Line([]string{knownhosts.Normalize(net.JoinHostPort(host, port))},
// pk) after the key parsed and its type matched, so port 2222 becomes "[host]:2222" and a key
// value can never add a line, a wildcard or @cert-authority (§4.4, review finding SE-3). Every
// presented key is pinned, because a known_hosts with only the ed25519 key fails when the server
// negotiates another type. It fails without keys (rclone would then check none) and for a host
// with whitespace, control or pattern characters.
func KnownHosts(host string, port int, keys []engines.HostKey) (string, error) {
	if len(keys) == 0 {
		return "", errors.New("known_hosts: no pinned host keys")
	}
	if host == "" || strings.ContainsFunc(host, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r) || strings.ContainsRune("@,*?![]#|", r)
	}) {
		return "", fmt.Errorf("known_hosts: host %q is not a host name or IP address", host)
	}
	if port == 0 {
		port = 22
	}
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("known_hosts: port %d is out of range", port)
	}
	address := knownhosts.Normalize(net.JoinHostPort(host, strconv.Itoa(port)))
	var b strings.Builder
	for i, k := range keys {
		pk, err := ParseHostKey(k)
		if err != nil {
			return "", fmt.Errorf("known_hosts: key %d: %w", i+1, err)
		}
		b.WriteString(knownhosts.Line([]string{address}, pk))
		b.WriteString("\n")
	}
	return b.String(), nil
}
