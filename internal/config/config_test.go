package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/db"
)

func TestMasterKeyCreatedOnceWithPrivatePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "bunkarr.key")
	k1, created, err := LoadOrCreateMasterKey(path)
	if err != nil || !created || len(k1) != masterKeyLen {
		t.Fatalf("first load: len=%d created=%v err=%v", len(k1), created, err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key permissions = %o, want 600", perm)
	}
	k2, created, err := LoadOrCreateMasterKey(path)
	if err != nil || created || string(k1) != string(k2) {
		t.Fatalf("second load: created=%v err=%v same=%v", created, err, string(k1) == string(k2))
	}
}

func TestMasterKeyConcurrentCreateAgrees(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bunkarr.key")
	var wg sync.WaitGroup
	keys := make([][]byte, 8)
	for i := range keys {
		wg.Go(func() {
			k, _, err := LoadOrCreateMasterKey(path)
			if err != nil {
				t.Error(err)
			}
			keys[i] = k
		})
	}
	wg.Wait()
	for _, k := range keys[1:] {
		if string(k) != string(keys[0]) {
			t.Fatal("concurrent creators ended up with different keys")
		}
	}
}

func TestMasterKeyRejectsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bunkarr.key")
	if err := os.WriteFile(path, []byte("not-a-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreateMasterKey(path); err == nil {
		t.Fatal("expected an invalid key error")
	}
}

func testKeyring(t *testing.T, seed byte) *Keyring {
	t.Helper()
	master := make([]byte, masterKeyLen)
	for i := range master {
		master[i] = seed
	}
	kr, err := NewKeyring(master)
	if err != nil {
		t.Fatal(err)
	}
	return kr
}

func TestKeyringRoundTripAndBinding(t *testing.T) {
	kr := testKeyring(t, 1)
	sealed, err := kr.Seal("hunter2-secret", "setting:a")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sealed, "hunter2") {
		t.Fatal("sealed value contains the plaintext")
	}
	if got, err := kr.Open(sealed, "setting:a"); err != nil || got != "hunter2-secret" {
		t.Fatalf("Open = %q, %v", got, err)
	}
	if _, err := kr.Open(sealed, "setting:b"); !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("Open with other aad: err = %v, want ErrKeyMismatch", err)
	}
	if _, err := testKeyring(t, 2).Open(sealed, "setting:a"); !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("Open with other key: err = %v, want ErrKeyMismatch", err)
	}
	tampered := sealed[:len(sealed)-2] + "AA"
	if _, err := kr.Open(tampered, "setting:a"); err == nil {
		t.Fatal("tampered value opened")
	}
	again, _ := kr.Seal("hunter2-secret", "setting:a")
	if again == sealed {
		t.Fatal("two seals of the same value are identical (nonce reuse)")
	}
}

func TestSettingsPlainAndSecret(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "bunkarr.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s := NewSettings(d, testKeyring(t, 3))

	if _, ok, err := s.Get(ctx, "missing"); ok || err != nil {
		t.Fatalf("Get(missing) = ok %v, err %v", ok, err)
	}
	if err := s.Set(ctx, "plain", "one"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "plain", "two"); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := s.Get(ctx, "plain"); v != "two" || !ok || err != nil {
		t.Fatalf("Get(plain) = %q %v %v", v, ok, err)
	}
	if err := s.SetSecret(ctx, "secret", "sekrit-value"); err != nil {
		t.Fatal(err)
	}
	var raw string
	_ = d.Reader().QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'secret'`).Scan(&raw)
	if strings.Contains(raw, "sekrit") {
		t.Fatal("secret stored in plaintext")
	}
	if v, ok, err := s.GetSecret(ctx, "secret"); v != "sekrit-value" || !ok || err != nil {
		t.Fatalf("GetSecret = %q %v %v", v, ok, err)
	}
	if _, _, err := s.Get(ctx, "secret"); err == nil {
		t.Fatal("Get on a secret should fail")
	}
	if _, _, err := s.GetSecret(ctx, "plain"); err == nil {
		t.Fatal("GetSecret on a plain value should fail")
	}
}

func TestEnvValidate(t *testing.T) {
	e := Env{ConfigDir: "x", Port: 8787, LogLevel: "info", LogFormat: "text"}
	if err := e.Validate(); err != nil || !filepath.IsAbs(e.ConfigDir) {
		t.Fatalf("Validate = %v, dir %q", err, e.ConfigDir)
	}
	for _, bad := range []Env{
		{ConfigDir: "x", Port: 0, LogLevel: "info", LogFormat: "text"},
		{ConfigDir: "x", Port: 8787, LogLevel: "loud", LogFormat: "text"},
		{ConfigDir: "x", Port: 8787, LogLevel: "info", LogFormat: "xml"},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("Validate(%+v) succeeded", bad)
		}
	}
}

func TestEnvFromOS(t *testing.T) {
	t.Setenv("BUNKARR_CONFIG_DIR", "")
	t.Setenv("BUNKARR_DOCKER", "1")
	t.Setenv("BUNKARR_PORT", "9999")
	e, err := EnvFromOS()
	if err != nil || e.ConfigDir != "/config" || e.Port != 9999 {
		t.Fatalf("EnvFromOS = %+v, %v", e, err)
	}
	t.Setenv("BUNKARR_PORT", "abc")
	if _, err := EnvFromOS(); err == nil {
		t.Fatal("expected an error for a non-numeric port")
	}
}
