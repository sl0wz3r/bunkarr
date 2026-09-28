package destinations

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/rclone"
	"github.com/sl0wz3r/bunkarr/internal/netguard"
)

// hostKeyAlgorithms are the host key types the scan asks for, one handshake each (§4.6): every
// presented key is pinned, because a known_hosts with only the ed25519 key fails when the server
// negotiates another type.
var hostKeyAlgorithms = []string{
	ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521, ssh.KeyAlgoRSASHA512,
}

// hostKeyTimeout bounds one handshake of the scan.
const hostKeyTimeout = 10 * time.Second

// errKeySeen ends a handshake once the server presented its host key: the scan never
// authenticates.
var errKeySeen = errors.New("host key received")

// ScanHostKeys connects to an SFTP server with golang.org/x/crypto/ssh through the outbound guard,
// once per host key algorithm, and returns every key it presents with its SHA256 fingerprint
// (POST /destinations/sftp/hostkeys, §4.6). It never authenticates: each handshake stops as soon
// as the server's key is received. An algorithm the server does not offer is skipped; no key at
// all is an error.
func (s *Store) ScanHostKeys(ctx context.Context, host string, port int) ([]engines.HostKeyInfo, error) {
	h, ok := validHost(host)
	if !ok {
		return nil, ValidationError(fmt.Sprintf("host %q: use a host name or an IP address", host))
	}
	if port == 0 {
		port = 22
	}
	if port < 1 || port > 65535 {
		return nil, ValidationError(fmt.Sprintf("port %d is out of range (1-65535)", port))
	}
	if err := s.checkHost(ctx, h); err != nil {
		return nil, err
	}
	addr := net.JoinHostPort(h, strconv.Itoa(port))
	var (
		out     []engines.HostKeyInfo
		lastErr error
	)
	for _, algo := range hostKeyAlgorithms {
		pk, err := presentedKey(ctx, addr, algo)
		if err != nil {
			if errors.Is(err, netguard.ErrBlocked) {
				return nil, hostRefusedError(h, err)
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			continue
		}
		k := engines.HostKey{Type: pk.Type(), Key: base64.StdEncoding.EncodeToString(pk.Marshal())}
		fp, err := rclone.Fingerprint(k)
		if err != nil {
			lastErr = err
			continue
		}
		info := engines.HostKeyInfo{Type: k.Type, Fingerprint: fp, Key: k.Key}
		if !slices.Contains(out, info) {
			out = append(out, info)
		}
	}
	if len(out) == 0 {
		if lastErr == nil {
			lastErr = errors.New("no host key")
		}
		return nil, fmt.Errorf("scan the host keys of %s: %w", addr, lastErr)
	}
	return out, nil
}

// presentedKey runs one SSH handshake with addr that offers only the host key algorithm algo and
// returns the key the server presents, without authenticating.
func presentedKey(ctx context.Context, addr, algo string) (ssh.PublicKey, error) {
	ctx, cancel := context.WithTimeout(ctx, hostKeyTimeout)
	defer cancel()
	conn, err := netguard.NewDialer(hostKeyTimeout).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	var key ssh.PublicKey
	cfg := &ssh.ClientConfig{
		User:              "bunkarr-hostkey-scan",
		HostKeyAlgorithms: []string{algo},
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
			key = k
			return errKeySeen
		},
		Timeout: hostKeyTimeout,
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err == nil {
		// Unreachable: the callback always refuses. Close what was opened anyway.
		go ssh.DiscardRequests(reqs)
		go func() {
			for ch := range chans {
				_ = ch.Reject(ssh.Prohibited, "")
			}
		}()
		_ = c.Close()
	}
	if key != nil {
		return key, nil
	}
	if err == nil {
		err = errors.New("the server presented no host key")
	}
	return nil, err
}
