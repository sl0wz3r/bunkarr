package destinations

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/rclone"
	"github.com/sl0wz3r/bunkarr/internal/netguard"
)

// sshServer runs an in-process SSH server with the given host keys that accepts no
// authentication; it counts the auth attempts it sees.
func sshServer(t *testing.T, signers ...ssh.Signer) (host string, port int, authAttempts *int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	attempts := 0
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) {
			attempts++
			return nil, errors.New("no")
		},
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			attempts++
			return nil, errors.New("no")
		},
	}
	for _, s := range signers {
		cfg.AddHostKey(s)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_ = c.SetDeadline(time.Now().Add(10 * time.Second))
				_, _, _, _ = ssh.NewServerConn(c, cfg)
				_ = c.Close()
			}()
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port, &attempts
}

func TestScanHostKeys(t *testing.T) {
	ef := newEngineFixture(t)
	edKey, edSigner := hostKey(t)
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecSigner, err := ssh.NewSignerFromKey(ec)
	if err != nil {
		t.Fatal(err)
	}
	host, port, attempts := sshServer(t, edSigner, ecSigner)
	keys, err := ef.store.ScanHostKeys(ef.ctx, host, port)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("keys %+v", keys)
	}
	fp, _ := rclone.Fingerprint(edKey)
	if keys[0].Type != "ssh-ed25519" || keys[0].Key != edKey.Key || keys[0].Fingerprint != fp || keys[1].Type != "ecdsa-sha2-nistp256" {
		t.Errorf("keys %+v", keys)
	}
	if *attempts != 0 {
		t.Errorf("the scan authenticated %d times", *attempts)
	}
	// The scanned keys pin: a remote with them validates.
	if _, err := normalizeHostKeys([]engines.HostKey{{Type: keys[1].Type, Key: keys[1].Key}}); err != nil {
		t.Errorf("scanned key does not pin: %v", err)
	}
	// A metadata address is refused by the guard (also by the dialer itself).
	if _, err := ef.store.ScanHostKeys(ef.ctx, "169.254.169.254", 22); !errors.Is(err, netguard.ErrBlocked) {
		t.Errorf("metadata address: %v", err)
	}
	ef.store.opts.CheckHost = func(context.Context, string) error { return nil }
	ctx, cancel := context.WithTimeout(ef.ctx, 5*time.Second)
	defer cancel()
	if _, err := ef.store.ScanHostKeys(ctx, "169.254.169.254", 22); !errors.Is(err, netguard.ErrBlocked) {
		t.Errorf("metadata address past CheckHost: %v", err)
	}
	if _, err := ef.store.ScanHostKeys(ef.ctx, "user@host", 22); err == nil {
		t.Error("a host with @ was scanned")
	}
	if _, err := ef.store.ScanHostKeys(ef.ctx, host, 70000); err == nil {
		t.Error("port 70000 accepted")
	}
}
