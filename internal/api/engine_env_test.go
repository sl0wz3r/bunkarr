package api

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/sl0wz3r/bunkarr/internal/auth"
	"github.com/sl0wz3r/bunkarr/internal/config"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/enginetest"
	"github.com/sl0wz3r/bunkarr/internal/netguard"
)

// The test environment of the Phase 4 routes: the real destinations store, engine service and
// job manager, with fake restic and rclone engines (enginetest.FakeEngine) for create, test and
// attach, and an enginetest.FakeRunner as the exec runner, which fails the test on any command
// no expectation answers (so "no command ran" is checked in every test).

// The UI user of the session tests.
const (
	sessionUser     = "admin"
	sessionPassword = "correct horse"
)

// Test credentials (never real ones): every raw response is searched for them.
const (
	testAccessKey  = "AKIAEXAMPLEKEY123"
	testSecretKey  = "s3cr3t/Access+Key-for-tests-0001"
	testNewSecret  = "rotated/Access+Key-for-tests-0002"
	testUserSecret = "my own restic password 0123456789"
	testSFTPPass   = "sftp-password-for-tests-77"
)

// testLocalIP is the address the test server sees for every request.
const testLocalIP = "127.0.0.1"

// session logs the UI user in (creating it on first use) and returns a client with its cookie.
func (e *env) session(t *testing.T) *http.Client {
	t.Helper()
	if setup, err := e.auth.SetupRequired(context.Background()); err != nil {
		t.Fatal(err)
	} else if setup {
		if _, err := e.auth.Setup(context.Background(), sessionUser, sessionPassword); err != nil {
			t.Fatal(err)
		}
	}
	c := e.client(t)
	body := fmt.Sprintf(`{"username":%q,"password":%q}`, sessionUser, sessionPassword)
	if code, _, _ := e.do(t, c, "POST", "/api/v1/auth/login", body, nil); code != 200 {
		t.Fatalf("login: %d", code)
	}
	return c
}

// sessionRaw sends a request with client c (a session, or nil for no credentials at all) and
// returns the status, the body and the headers. body is JSON-encoded unless it is a string.
func (e *env) sessionRaw(t *testing.T, c *http.Client, method, path string, body any) (int, []byte, http.Header) {
	t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		enc, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(enc)
	}
	req, err := http.NewRequest(method, e.srv.URL+"/api/v1"+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c == nil {
		c = http.DefaultClient
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, out, res.Header
}

// sessionCall is sessionRaw that fails unless the status is want and decodes the answer into out.
func (e *env) sessionCall(t *testing.T, c *http.Client, want int, method, path string, body, out any) []byte {
	t.Helper()
	code, raw, _ := e.sessionRaw(t, c, method, path, body)
	if code != want {
		t.Fatalf("%s %s: status %d, want %d: %s", method, path, code, want, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s %s: decode %s: %v", method, path, raw, err)
		}
	}
	return raw
}

// localBypass sends a request without credentials while authentication is disabled for local
// addresses (the test client is local): the principal is auth.KindLocal.
func (e *env) localBypass(t *testing.T, method, path string, body any) (int, string) {
	t.Helper()
	ctx := context.Background()
	if err := e.auth.SetMode(ctx, auth.ModeLocalDisabled); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := e.auth.SetMode(ctx, auth.ModeEnabled); err != nil {
			t.Fatal(err)
		}
	}()
	code, raw, _ := e.sessionRaw(t, nil, method, path, body)
	var m struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &m)
	return code, m.Message
}

// withPassword returns a copy of body with currentPassword set.
func withPassword(body map[string]any, password string) map[string]any {
	out := make(map[string]any, len(body)+1)
	for k, v := range body {
		out[k] = v
	}
	out["currentPassword"] = password
	return out
}

// engineEnv is an env whose engines are fakes.
type engineEnv struct {
	*env
	runner         *enginetest.FakeRunner
	restic, rclone *enginetest.FakeEngine
	// versions is the version store every engine destination's Plex DB, *arr and manifest jobs get.
	versions *corruptibleStore
	// n numbers the fake repositories.
	mu sync.Mutex
	n  int
}

// engineEnvOptions adjusts newEngineEnv.
type engineEnvOptions struct {
	// availability replaces "both engines available".
	availability *engines.Availability
	// tweak adjusts the App options last.
	tweak func(*AppOptions)
}

// newEngineEnv builds an env whose restic and rclone engines are fakes: Create initializes a new
// repository (restic, "restic:<64 hex>") or writes a marker (rclone); Test answers for a stored
// destination with its identity, and for a new location "missing". Both engines are available
// (restic 0.18.1, rclone 1.74.1) unless o says otherwise.
func newEngineEnv(t *testing.T, o engineEnvOptions) *engineEnv {
	t.Helper()
	ee := &engineEnv{runner: enginetest.NewFakeRunner(t), restic: &enginetest.FakeEngine{EngineKind: engines.Restic},
		rclone: &enginetest.FakeEngine{EngineKind: engines.Rclone}, versions: &corruptibleStore{FakeVersionStore: enginetest.NewFakeVersionStore()}}
	ee.restic.OnCreate = func(_ context.Context, d engines.Destination, _ engines.Secrets, attach bool) (engines.CreateResult, error) {
		return engines.CreateResult{MarkerID: "restic:" + ee.repoID(), Initialized: !attach}, nil
	}
	ee.rclone.OnCreate = func(_ context.Context, d engines.Destination, _ engines.Secrets, attach bool) (engines.CreateResult, error) {
		return engines.CreateResult{MarkerID: "marker-" + ee.repoID()[48:]}, nil
	}
	test := func(_ context.Context, d engines.Destination, _ engines.Secrets) (engines.TestResult, error) {
		if d.MarkerID == "" {
			return engines.TestResult{OK: true, Reachable: true, Repository: engines.RepositoryMissing, EngineVersion: "test"}, nil
		}
		return engines.TestResult{OK: true, Reachable: true, Repository: "exists", ID: strings.TrimPrefix(d.MarkerID, "restic:")}, nil
	}
	ee.restic.OnTest, ee.rclone.OnTest = test, test
	avail := engines.Availability{
		Restic: engines.BinaryStatus{Available: true, Version: "0.18.1", Path: "/usr/bin/restic"},
		Rclone: engines.BinaryStatus{Available: true, Version: "1.74.1", Path: "/usr/bin/rclone"},
	}
	if o.availability != nil {
		avail = *o.availability
	}
	ee.env = newEnvWith(t, nil, func(ao *AppOptions) {
		ao.Engines = EngineOptions{
			Runner:       ee.runner,
			Restic:       config.EngineBinary{Path: "/usr/bin/restic"},
			Rclone:       config.EngineBinary{Path: "/usr/bin/rclone"},
			Availability: avail,
			RunDirs:      enginetest.RunDirs(t),
			HostName:     "bunkarr-test",
			// No DNS in tests: only literal metadata and link-local addresses are refused.
			CheckHost: func(_ context.Context, host string) error {
				if ip, err := netip.ParseAddr(host); err == nil && netguard.Blocked(ip) {
					return &netguard.BlockedError{Addr: host}
				}
				return nil
			},
		}
		ao.engineRegistry = func(k engines.Kind) (engines.Engine, bool) {
			switch k {
			case engines.Restic:
				return ee.restic, true
			case engines.Rclone:
				return ee.rclone, true
			}
			return nil, false
		}
		ao.openVersions = func(context.Context, int64, engines.Runtime) (engines.VersionStore, io.Closer, error) {
			return ee.versions, io.NopCloser(nil), nil
		}
		if o.tweak != nil {
			o.tweak(ao)
		}
	})
	return ee
}

// repoID returns a new 64-hex repository id.
func (ee *engineEnv) repoID() string {
	ee.mu.Lock()
	defer ee.mu.Unlock()
	ee.n++
	return fmt.Sprintf("%064x", ee.n*7919)
}

// engineCalls counts the calls of both fake engines.
func (ee *engineEnv) engineCalls() int { return len(ee.restic.Calls()) + len(ee.rclone.Calls()) }

// s3Body is POST /destinations of a restic (or rclone) destination on an S3 bucket.
func s3Body(name, engine, bucket, prefix string) map[string]any {
	return map[string]any{"name": name, "kind": "s3", "engine": engine,
		"remote":      map[string]any{"provider": "Minio", "endpoint": "https://s3.example.com", "region": "us-east-1", "bucket": bucket, "prefix": prefix},
		"credentials": map[string]any{"accessKeyId": testAccessKey, "secretAccessKey": testSecretKey}}
}

// createEngineDest creates a destination with a UI session and the password (S29) and returns
// its id.
func (ee *engineEnv) createEngineDest(t *testing.T, c *http.Client, body map[string]any) int64 {
	t.Helper()
	raw := ee.sessionCall(t, c, 201, "POST", "/destinations", withPassword(body, sessionPassword), nil)
	return idOf(t, raw)
}

// secretValues returns every form of destination id's stored secrets that a response must never
// contain (clear, obscured, JSON-escaped; PEM key lines), leaving out values too short to search.
func (ee *engineEnv) secretValues(t *testing.T, id int64) []string {
	t.Helper()
	_, sec, err := ee.app.Destinations.SecretsFor(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, v := range sec.Values() {
		if len(v) >= 12 && !strings.HasPrefix(v, "-----") {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		t.Fatalf("destination %d has no secret values to search for", id)
	}
	return out
}

// noSecrets fails when raw contains one of values.
func noSecrets(t *testing.T, what string, raw []byte, values []string) {
	t.Helper()
	for _, v := range values {
		if bytes.Contains(raw, []byte(v)) {
			t.Errorf("%s: the response contains a secret (%d bytes starting %q)", what, len(v), v[:4])
		}
	}
}

// hostKeyPair returns a new ed25519 SSH host key: the pinned form and its signer.
func hostKeyPair(t *testing.T) (engines.HostKey, ssh.Signer) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pk := signer.PublicKey()
	return engines.HostKey{Type: pk.Type(), Key: base64.StdEncoding.EncodeToString(pk.Marshal())}, signer
}

// privateKeyPEM returns a new OpenSSH private key in PEM form (an SFTP credential), encrypted
// with passphrase when it is not "".
func privateKeyPEM(t *testing.T, passphrase string) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	blk, err := ssh.MarshalPrivateKey(priv, "")
	if passphrase != "" {
		blk, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte(passphrase))
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(blk))
}

// caCertPEM returns a new self-signed CA certificate in PEM form (an S3 remote's caCert).
func caCertPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "minio test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// sshServer runs an in-process SSH server with the given host keys that accepts no
// authentication and counts the authentication attempts.
func sshServer(t *testing.T, signers ...ssh.Signer) (host string, port int, attempts func() int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var (
		mu sync.Mutex
		n  int
	)
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) {
			mu.Lock()
			n++
			mu.Unlock()
			return nil, errors.New("no")
		},
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			mu.Lock()
			n++
			mu.Unlock()
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
	return addr.IP.String(), addr.Port, func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

// corruptibleStore is the fake version store with one fault the fake lacks: Fetch can flip the
// content of the file it writes (a damaged version at the destination).
type corruptibleStore struct {
	*enginetest.FakeVersionStore
	mu      sync.Mutex
	corrupt bool
}

func (c *corruptibleStore) setCorrupt(on bool) {
	c.mu.Lock()
	c.corrupt = on
	c.mu.Unlock()
}

// Fetch implements engines.VersionStore, damaging the file it writes when corrupt is set.
func (c *corruptibleStore) Fetch(ctx context.Context, ref engines.Ref, name, dstDir string) error {
	if err := c.FakeVersionStore.Fetch(ctx, ref, name, dstDir); err != nil {
		return err
	}
	c.mu.Lock()
	bad := c.corrupt
	c.mu.Unlock()
	if !bad {
		return nil
	}
	p := filepath.Join(dstDir, path.Base(name))
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	if len(b) > 0 {
		b[len(b)/2] ^= 0x55
	}
	return os.WriteFile(p, b, 0o600)
}

var _ engines.VersionStore = (*corruptibleStore)(nil)
