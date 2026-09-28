package rclone

import (
	"bytes"
	"maps"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/logging"
)

// testKey stands in for Keyring.Derive(ObscureKeyInfo, 32).
var testKey = bytes.Repeat([]byte{0x42}, 32)

// TestObscureMatchesRclone: the scheme is rclone's: its documented vectors, and a value obscured
// by `rclone obscure` (rclone v1.74.1, random IV) reveals to the throwaway value (§14.1 rclone).
func TestObscureMatchesRclone(t *testing.T) {
	for _, tc := range []struct{ in, iv, want string }{
		{"", "aaaaaaaaaaaaaaaa", "YWFhYWFhYWFhYWFhYWFhYQ"},
		{"potato", "aaaaaaaaaaaaaaaa", "YWFhYWFhYWFhYWFhYWFhYXMaGgIlEQ"},
		{"potato", "bbbbbbbbbbbbbbbb", "YmJiYmJiYmJiYmJiYmJiYp3gcEWbAw"},
	} {
		if got := obscureWithIV(tc.in, []byte(tc.iv)); got != tc.want {
			t.Errorf("obscure(%q, %q) = %q, want %q", tc.in, tc.iv, got, tc.want)
		}
	}
	// Recorded with: docker run --rm --entrypoint rclone bunkarr:dev obscure bunkarr-throwaway-obscure-vector
	const recorded = "H9BcQgwuKSPBBAKtm-uQJVRMc9YhtlD8uMexX2-E9eKV6KvMXJnohhRoMX1m5yXM"
	if got, err := Reveal(recorded); err != nil || got != "bunkarr-throwaway-obscure-vector" {
		t.Errorf("Reveal(rclone obscure output) = %q, %v", got, err)
	}
}

// TestObscureDeterministic: each (key, destination, field, value) has exactly one obscured form,
// so two builds of an environment are identical and the registered form is the one a child sees;
// logging.ContainsSecret finds it once SetSecrets holds it (§4.4, SE-4).
func TestObscureDeterministic(t *testing.T) {
	a := Obscure(testKey, 3, engines.FieldCryptPassword, "crypt-password-1")
	if a != Obscure(testKey, 3, engines.FieldCryptPassword, "crypt-password-1") {
		t.Fatal("Obscure is not deterministic")
	}
	for _, other := range []string{
		Obscure(testKey, 4, engines.FieldCryptPassword, "crypt-password-1"),
		Obscure(testKey, 3, engines.FieldCryptPassword2, "crypt-password-1"),
		Obscure(bytes.Repeat([]byte{1}, 32), 3, engines.FieldCryptPassword, "crypt-password-1"),
	} {
		if other == a {
			t.Error("another destination, field or key gave the same IV")
		}
	}
	if clear, err := Reveal(a); err != nil || clear != "crypt-password-1" {
		t.Errorf("Reveal = %q, %v", clear, err)
	}
	d, s := s3Dest(true)
	s2 := s
	s2.Obscured = map[string]string{
		engines.FieldCryptPassword:  Obscure(testKey, d.ID, engines.FieldCryptPassword, s.Encryption.CryptPassword),
		engines.FieldCryptPassword2: Obscure(testKey, d.ID, engines.FieldCryptPassword2, s.Encryption.CryptPassword2),
	}
	env1, err1 := Env(EnvInput{Dest: d, Secrets: s})
	env2, err2 := Env(EnvInput{Dest: d, Secrets: s2})
	if err1 != nil || err2 != nil || !maps.Equal(env1, env2) {
		t.Fatalf("two builds of the environment differ: %v %v", err1, err2)
	}
	logging.SetSecrets("destination:rclone-obscure-test", s.Values()...)
	defer logging.SetSecrets("destination:rclone-obscure-test")
	if !logging.ContainsSecret("Setting password=\"" + env1["RCLONE_CONFIG_BKCRYPT_PASSWORD"] + "\" for \"bkcrypt\"") {
		t.Error("the obscured password in the environment is not a registered secret")
	}
}

func TestObscureRandomAndReveal(t *testing.T) {
	a, b := ObscureRandom("typed-in-a-test-form"), ObscureRandom("typed-in-a-test-form")
	if a == b {
		t.Error("ObscureRandom repeated an IV")
	}
	for _, o := range []string{a, b} {
		if v, err := Reveal(o); err != nil || v != "typed-in-a-test-form" {
			t.Errorf("Reveal(%q) = %q, %v", o, v, err)
		}
	}
	for _, bad := range []string{"not base64!", "YWFh", ""} {
		if _, err := Reveal(bad); err == nil {
			t.Errorf("Reveal(%q) succeeded", bad)
		}
	}
}
