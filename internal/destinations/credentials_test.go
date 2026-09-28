package destinations

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/sl0wz3r/bunkarr/internal/engines"
)

// clientKey returns a new ed25519 client key as OpenSSH PEM, encrypted with passphrase when it
// is set.
func clientKey(t *testing.T, passphrase string) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var block *pem.Block
	if passphrase != "" {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "bunkarr-test", []byte(passphrase))
	} else {
		block, err = ssh.MarshalPrivateKey(priv, "bunkarr-test")
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(block))
}

// credsJSON is a credentials input of the given fields.
func credsJSON(t *testing.T, fields map[string]string) *CredentialsInput {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return creds(t, string(b))
}

// An SFTP login is replaced as a whole (§4.3): a key rotated to an unencrypted one drops the old
// passphrase, and a switch between key and password drops the other method, so it is neither
// sent to the server nor printed in a kit with the storage credentials.
func TestUpdateSFTPCredentialsReplaceTheLogin(t *testing.T) {
	ef := newEngineFixture(t)
	hk, _ := hostKey(t)
	const passphrase = "old key passphrase"
	encrypted, plain := clientKey(t, passphrase), clientKey(t, "")
	d, err := ef.store.Create(ef.ctx, Input{Name: "SFTP key", Kind: engines.SFTP, Engine: EngineRclone,
		Remote:      rawJSON(t, map[string]any{"host": "sftp.example.com", "user": "bunkarr", "path": "/srv/backup", "hostKeys": []engines.HostKey{hk}}),
		Credentials: credsJSON(t, map[string]string{"privateKey": encrypted, "privateKeyPassphrase": passphrase})}, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var tested []engines.Credentials
	ef.rclone.OnTest = func(_ context.Context, td engines.Destination, s engines.Secrets) (engines.TestResult, error) {
		tested = append(tested, s.Credentials)
		return engines.TestResult{OK: true, Marker: engines.MarkerOK, ID: td.MarkerID}, nil
	}
	stored := func() engines.Credentials {
		t.Helper()
		_, sec, err := ef.store.SecretsFor(ef.ctx, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		return sec.Credentials
	}
	has := func(up Destination) string {
		var out []string
		for _, f := range []string{engines.FieldPrivateKey, engines.FieldPrivateKeyPassphrase, engines.FieldPassword} {
			if up.HasCredentials[f] {
				out = append(out, f)
			}
		}
		return strings.Join(out, ",")
	}
	for _, step := range []struct {
		name   string
		update map[string]string
		want   engines.Credentials
	}{
		{"an unencrypted key drops the old passphrase", map[string]string{"privateKey": plain},
			engines.Credentials{PrivateKey: plain}},
		{"a password drops the key", map[string]string{"password": "a new login password"},
			engines.Credentials{Password: "a new login password"}},
		{"a key drops the password", map[string]string{"privateKey": encrypted, "privateKeyPassphrase": passphrase},
			engines.Credentials{PrivateKey: encrypted, PrivateKeyPassphrase: passphrase}},
		{"key and password together are kept together", map[string]string{"privateKey": plain, "password": "a new login password"},
			engines.Credentials{PrivateKey: plain, Password: "a new login password"}},
	} {
		up, err := ef.store.Update(ef.ctx, d.ID, Input{Credentials: credsJSON(t, step.update)})
		if err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if got := stored(); got != step.want {
			t.Errorf("%s: stored fields %v, want %v", step.name, got.Fields(), step.want.Fields())
		}
		if got := tested[len(tested)-1]; got != step.want {
			t.Errorf("%s: tested with fields %v, want %v", step.name, got.Fields(), step.want.Fields())
		}
		want := []string{}
		for f, set := range step.want.Fields() {
			if set {
				want = append(want, f)
			}
		}
		if got := has(up); len(strings.Split(got, ",")) != len(want) {
			t.Errorf("%s: hasCredentials %s, want %v", step.name, got, want)
		}
	}
	// Only a passphrase: the stored key stays (and must decrypt with it).
	if _, err := ef.store.Update(ef.ctx, d.ID, Input{Credentials: credsJSON(t, map[string]string{"privateKey": encrypted, "privateKeyPassphrase": passphrase})}); err != nil {
		t.Fatal(err)
	}
	if _, err := ef.store.Update(ef.ctx, d.ID, Input{Credentials: credsJSON(t, map[string]string{"privateKeyPassphrase": "a wrong passphrase"})}); err == nil ||
		!strings.Contains(err.Error(), "does not decrypt") {
		t.Errorf("a wrong passphrase for the stored key: %v", err)
	}
	if got := stored(); got.PrivateKey != encrypted || got.PrivateKeyPassphrase != passphrase || got.Password != "" {
		t.Errorf("after a refused passphrase: %v", got.Fields())
	}
	// A kit with the storage credentials prints only the current login.
	if _, err := ef.store.Update(ef.ctx, d.ID, Input{Credentials: credsJSON(t, map[string]string{"password": "the only login password"})}); err != nil {
		t.Fatal(err)
	}
	kit, err := ef.store.RecoveryKit(ef.ctx, d.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(kit.Content, "BEGIN OPENSSH PRIVATE KEY") || strings.Contains(kit.Content, passphrase) ||
		!strings.Contains(kit.Content, "the only login password") {
		t.Error("the kit prints a dropped SFTP credential, or not the current one")
	}
}

func TestMergeCredentials(t *testing.T) {
	stored := engines.Credentials{PrivateKey: "K", PrivateKeyPassphrase: "P", Password: "W"}
	for _, c := range []struct {
		kind   engines.DestKind
		stored engines.Credentials
		update engines.Credentials
		want   engines.Credentials
	}{
		{engines.SFTP, stored, engines.Credentials{PrivateKey: "K2"}, engines.Credentials{PrivateKey: "K2"}},
		{engines.SFTP, stored, engines.Credentials{Password: "W2"}, engines.Credentials{Password: "W2"}},
		{engines.SFTP, stored, engines.Credentials{PrivateKeyPassphrase: "P2"}, engines.Credentials{PrivateKey: "K", PrivateKeyPassphrase: "P2", Password: "W"}},
		{engines.S3, engines.Credentials{AccessKeyID: "A", SecretAccessKey: "S"}, engines.Credentials{SecretAccessKey: "S2"},
			engines.Credentials{AccessKeyID: "A", SecretAccessKey: "S2"}},
		{engines.B2, engines.Credentials{KeyID: "I", ApplicationKey: "K"}, engines.Credentials{KeyID: "I2"},
			engines.Credentials{KeyID: "I2", ApplicationKey: "K"}},
	} {
		if got := mergeCredentials(c.kind, c.stored, c.update); got != c.want {
			t.Errorf("%s: merge of %v = %v, want %v", c.kind, fmt.Sprint(c.update.Fields()), got.Fields(), c.want.Fields())
		}
	}
}

// The form leaves a stored SFTP field empty to keep it: a key rotated to another one encrypted
// with the same passphrase keeps the stored passphrase; a key the stored passphrase does not open
// needs its own, and an unencrypted key still drops it.
func TestUpdateSFTPKeyKeepsThePassphraseThatOpensIt(t *testing.T) {
	ef := newEngineFixture(t)
	hk, _ := hostKey(t)
	const passphrase = "the key passphrase"
	d, err := ef.store.Create(ef.ctx, Input{Name: "SFTP key", Kind: engines.SFTP, Engine: EngineRclone,
		Remote:      rawJSON(t, map[string]any{"host": "sftp.example.com", "user": "bunkarr", "path": "/srv/backup", "hostKeys": []engines.HostKey{hk}}),
		Credentials: credsJSON(t, map[string]string{"privateKey": clientKey(t, passphrase), "privateKeyPassphrase": passphrase})}, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var tested []engines.Credentials
	ef.rclone.OnTest = func(_ context.Context, td engines.Destination, s engines.Secrets) (engines.TestResult, error) {
		tested = append(tested, s.Credentials)
		return engines.TestResult{OK: true, Marker: engines.MarkerOK, ID: td.MarkerID}, nil
	}
	stored := func() engines.Credentials {
		t.Helper()
		_, sec, err := ef.store.SecretsFor(ef.ctx, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		return sec.Credentials
	}
	rotated := clientKey(t, passphrase)
	if _, err := ef.store.Update(ef.ctx, d.ID, Input{Credentials: credsJSON(t, map[string]string{"privateKey": rotated})}); err != nil {
		t.Fatalf("a key with the same passphrase: %v", err)
	}
	want := engines.Credentials{PrivateKey: rotated, PrivateKeyPassphrase: passphrase}
	if got := stored(); got != want || len(tested) != 1 || tested[0] != want {
		t.Fatalf("stored %v, tested %d times", got.Fields(), len(tested))
	}
	if _, err := ef.store.Update(ef.ctx, d.ID, Input{Credentials: credsJSON(t, map[string]string{"privateKey": clientKey(t, "another passphrase")})}); err == nil ||
		!strings.Contains(err.Error(), "passphrase is required") {
		t.Errorf("a key the stored passphrase does not open: %v", err)
	}
	if got := stored(); got != want {
		t.Errorf("after a refused key: %v", got.Fields())
	}
	plain := clientKey(t, "")
	if _, err := ef.store.Update(ef.ctx, d.ID, Input{Credentials: credsJSON(t, map[string]string{"privateKey": plain})}); err != nil {
		t.Fatal(err)
	}
	if got := stored(); got != (engines.Credentials{PrivateKey: plain}) {
		t.Errorf("an unencrypted key: %v", got.Fields())
	}
}
