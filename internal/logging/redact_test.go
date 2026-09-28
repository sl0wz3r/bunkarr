package logging

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSensitiveKeyPhase4Names(t *testing.T) {
	for _, k := range []string{
		"privateKey", "PrivateKey", "private_key", "privateKeyPassphrase", "passphrase",
		"applicationKey", "keyPem", "KEY_PEM", "RCLONE_CONFIG_BKDEST_KEY_PEM", "secretAccessKey",
		"cryptPassword2", "resticPassword",
	} {
		if !sensitiveKey(k) {
			t.Errorf("sensitiveKey(%q) = false", k)
		}
	}
	for _, k := range []string{"user", "host", "keyId", "accessKeyId", "bucket", "hostKeys", "fingerprint"} {
		if sensitiveKey(k) {
			t.Errorf("sensitiveKey(%q) = true", k)
		}
	}
}

func TestEscapedForms(t *testing.T) {
	const bs = "\\"
	tests := []struct {
		in   string
		want []string
	}{
		{"plain-token-1234", nil},
		{`quote"and\back`, []string{`quote\"and\\back`}},
		{"line1\nline2", []string{`line1\nline2`}},
		{"<html>&co", []string{bs + "u003chtml" + bs + "u003e" + bs + "u0026co"}},
	}
	for _, tc := range tests {
		got := EscapedForms(tc.in)
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("EscapedForms(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestJSONEscapedSecretsRegistered: registering a secret also registers the forms it takes in a
// JSON line and in Go-quoted text (S22).
func TestJSONEscapedSecretsRegistered(t *testing.T) {
	isolateSecrets(t)
	const pinned = `pinned"secret\value<1>`
	RegisterSecret(pinned)
	const owned = "-----BEGIN KEY-----\nAAAABBBBCCCC\n-----END KEY-----"
	SetSecrets("destination:1", owned)
	for _, v := range []string{pinned, owned} {
		b, _ := json.Marshal(map[string]string{"msg": "value " + v})
		if !ContainsSecret(string(b)) {
			t.Errorf("JSON form of %q not registered: %s", v, b)
		}
		if got := RedactSecrets(string(b)); strings.Contains(got, "AAAABBBB") || strings.Contains(got, `secret\\value`) {
			t.Errorf("JSON form leaked: %s", got)
		}
	}
	SetSecrets("destination:1")
	// Released, not dropped: the escaped form stays redacted with the value.
	if !ContainsSecret(`x -----BEGIN KEY-----\nAAAABBBBCCCC\n-----END KEY----- y`) {
		t.Error("the escaped form of a released value was dropped")
	}
}

type nestedSecrets struct {
	User  string `json:"user"`
	Inner struct {
		APIKey string `json:"apiKey"`
		Note   string `json:"note"`
	} `json:"inner"`
	List []struct {
		Password string
		Name     string
	} `json:"list"`
	Opaque map[string]any `json:"opaque"`
}

// TestAnyValuesRedactedRecursively: slog.Any of a struct, map or byte slice is redacted at any
// depth by field name and by registered value, in both handlers; a value without secrets keeps
// its format.
func TestAnyValuesRedactedRecursively(t *testing.T) {
	isolateSecrets(t)
	const registered = "registered-secret-value-42"
	RegisterSecret(registered)
	var v nestedSecrets
	v.User = "alice"
	v.Inner.APIKey = "inner-api-key-value"
	v.Inner.Note = "note holds " + registered
	v.List = append(v.List, struct {
		Password string
		Name     string
	}{"list-password-value", "item"})
	v.Opaque = map[string]any{"privateKey": "opaque-private-key", "nested": map[string]any{"KEY_PEM": "pem-value-12345", "x": registered}}

	for _, format := range []string{"json", "text"} {
		var out bytes.Buffer
		log, closer, err := New(Options{Level: "debug", StdoutFormat: format, Stdout: &out})
		if err != nil {
			t.Fatal(err)
		}
		log.Debug("values", "v", v, "ptr", &v, "m", v.Opaque, "raw", []byte("bytes "+registered), "ids", []int{1, 2})
		_ = closer.Close()
		got := out.String()
		for _, leak := range []string{registered, "inner-api-key-value", "list-password-value", "opaque-private-key", "pem-value-12345"} {
			if strings.Contains(got, leak) {
				t.Errorf("%s: %q leaked: %s", format, leak, got)
			}
		}
		if !strings.Contains(got, "alice") || !strings.Contains(got, "item") {
			t.Errorf("%s: a normal value was lost: %s", format, got)
		}
		if format == "text" && !strings.Contains(got, "ids=\"[1 2]\"") && !strings.Contains(got, "ids=[1 2]") {
			t.Errorf("text: an unchanged value changed its format: %s", got)
		}
	}
}

func TestRedactTruncated(t *testing.T) {
	isolateSecrets(t)
	const secret = "SECRET-abcdefghijklmnop"
	RegisterSecret(secret)
	const local = "local-value-0123456789"
	pad := strings.Repeat("x", 100)
	tests := []struct {
		name    string
		in      string
		limit   int
		partial bool
		values  []string
		want    string
	}{
		{"short line redacted", "a " + secret + " b", 1000, false, nil, "a " + Redacted + " b"},
		{"secret across the cut", pad + secret + pad, 110, false, nil, pad + Redacted[:10]},
		{"local value across the cut", pad + local + pad, 105, false, []string{local}, pad + Redacted[:5]},
		{"partial: a secret's start at the end is dropped", pad + secret[:10], 1000, true, nil, pad[:100+10-len(secret)]},
		{"partial: the last bytes are dropped, whole secrets too", pad + secret, 1000, true, nil, pad},
		{"partial: a secret before the tail is replaced", secret + pad, 1000, true, nil, Redacted + pad[:100-len(secret)]},
		{"overlapping secrets merge", "x" + secret + "-and-" + secret + "y", 1000, false, []string{secret[7:] + "-and-" + secret[:9]}, "x" + Redacted + "y"},
		{"cut backs off to a rune boundary", strings.Repeat("é", 10), 5, false, nil, "éé"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactTruncated(tc.in, tc.limit, tc.partial, tc.values...)
			if got != tc.want {
				t.Errorf("got %q\nwant %q", got, tc.want)
			}
			if len(got) > tc.limit || !utf8.ValidString(got) {
				t.Errorf("result of %d bytes (limit %d) or invalid UTF-8", len(got), tc.limit)
			}
			for _, s := range []string{secret[:8], local[:8]} {
				if strings.Contains(got, s) {
					t.Errorf("a part of a secret was kept: %q", got)
				}
			}
		})
	}
}

// TestRedactTruncatedLongLine: a 3 MiB line with secrets spread over it, one across the 1 MiB
// cut and one JSON-escaped, keeps none of them.
func TestRedactTruncatedLongLine(t *testing.T) {
	isolateSecrets(t)
	const secret = `long"line-secret-9f8e7d`
	RegisterSecret(secret)
	escaped := EscapedForms(secret)[0]
	const limit = 1 << 20
	var b strings.Builder
	for b.Len() < limit-10 {
		b.WriteString("0123456789")
	}
	b.WriteString(secret) // across the cut
	for b.Len() < 3*limit {
		b.WriteString(escaped + " filler ")
	}
	got := RedactTruncated(b.String(), limit, false)
	if len(got) > limit {
		t.Fatalf("len %d", len(got))
	}
	if strings.Contains(got, "long") || strings.Contains(got, `long\"`) {
		t.Fatal("a secret survived the cut")
	}
}
