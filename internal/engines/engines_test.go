package engines_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/config"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/bwlimit"
	"github.com/sl0wz3r/bunkarr/internal/engines/enginetest"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
	"github.com/sl0wz3r/bunkarr/internal/logging"
)

func TestDestKind(t *testing.T) {
	for _, tc := range []struct {
		k             engines.DestKind
		valid, remote bool
	}{
		{engines.Local, true, false}, {engines.SFTP, true, true}, {engines.S3, true, true}, {engines.B2, true, true},
		{"ftp", false, false}, {"", false, false},
	} {
		if tc.k.Valid() != tc.valid || tc.k.Remote() != tc.remote {
			t.Errorf("%q: Valid %v Remote %v", tc.k, tc.k.Valid(), tc.k.Remote())
		}
	}
}

func TestRemoteMarshalJSON(t *testing.T) {
	tests := []struct {
		r    engines.Remote
		want string
	}{
		{engines.Remote{}, `{}`},
		{engines.Remote{B2: &engines.B2Remote{Bucket: "b", Prefix: "p/q"}}, `{"bucket":"b","prefix":"p/q"}`},
		{engines.Remote{S3: &engines.S3Remote{Provider: "Minio", Endpoint: "https://m:9000", Region: "us-east-1", Bucket: "b", ForcePathStyle: true}},
			`{"provider":"Minio","endpoint":"https://m:9000","region":"us-east-1","bucket":"b","prefix":"","storageClass":"","forcePathStyle":true,"caCert":""}`},
		{engines.Remote{SFTP: &engines.SFTPRemote{Host: "h", Port: 22, User: "u", Path: "/p"}},
			`{"host":"h","port":22,"user":"u","path":"/p","hostKeys":[]}`},
		{engines.Remote{SFTP: &engines.SFTPRemote{Host: "h", Port: 2222, User: "u", Path: "p", HostKeys: []engines.HostKey{{Type: "ssh-ed25519", Key: "AAAA"}}}},
			`{"host":"h","port":2222,"user":"u","path":"p","hostKeys":[{"type":"ssh-ed25519","key":"AAAA"}]}`},
	}
	for _, tc := range tests {
		b, err := json.Marshal(tc.r)
		if err != nil || string(b) != tc.want {
			t.Errorf("Marshal = %s, %v; want %s", b, err, tc.want)
		}
	}
}

// TestDialHosts: the names netguard checks are the names the engine dials (S25, SE-12).
func TestDialHosts(t *testing.T) {
	tests := []struct {
		name string
		kind engines.DestKind
		r    engines.Remote
		want []string
	}{
		{"virtual-hosted", engines.S3, engines.Remote{S3: &engines.S3Remote{Provider: "Wasabi", Endpoint: "https://s3.eu-central-1.wasabisys.com", Bucket: "media"}}, []string{"s3.eu-central-1.wasabisys.com", "media.s3.eu-central-1.wasabisys.com"}},
		{"path style", engines.S3, engines.Remote{S3: &engines.S3Remote{Provider: "Minio", Endpoint: "https://minio.lan:9000", Bucket: "media", ForcePathStyle: true}}, []string{"minio.lan", "media.minio.lan"}},
		// rclone dials path style for a bucket with a dot although forcePathStyle is false.
		{"bucket with a dot", engines.S3, engines.Remote{S3: &engines.S3Remote{Provider: "Other", Endpoint: "https://example.invalid", Bucket: "a.b"}}, []string{"example.invalid", "a.b.example.invalid"}},
		// rclone forces virtual-hosted style for AWS and Wasabi although forcePathStyle is true.
		{"AWS endpoint, forcePathStyle", engines.S3, engines.Remote{S3: &engines.S3Remote{Provider: "AWS", Endpoint: "https://example.invalid", Bucket: "abc", ForcePathStyle: true}}, []string{"example.invalid", "abc.example.invalid"}},
		// The SDK's S3 Express and table suffixes still dial <bucket>.<endpoint host>; the Outposts
		// alias suffix --op-s3 does not, so destinations refuses it (checkS3Bucket).
		{"S3 Express suffix", engines.S3, engines.Remote{S3: &engines.S3Remote{Provider: "Other", Endpoint: "https://example.invalid", Bucket: "bunkarr--usw2-az1--x-s3"}}, []string{"example.invalid", "bunkarr--usw2-az1--x-s3.example.invalid"}},
		{"table suffix", engines.S3, engines.Remote{S3: &engines.S3Remote{Provider: "Other", Endpoint: "https://example.invalid", Bucket: "bunkarr--table-s3"}}, []string{"example.invalid", "bunkarr--table-s3.example.invalid"}},
		{"no bucket", engines.S3, engines.Remote{S3: &engines.S3Remote{Provider: "Other", Endpoint: "https://example.invalid"}}, []string{"example.invalid"}},
		{"IP endpoint", engines.S3, engines.Remote{S3: &engines.S3Remote{Provider: "Other", Endpoint: "http://192.168.1.5:9000", Bucket: "media"}}, []string{"192.168.1.5"}},
		{"IPv6 endpoint", engines.S3, engines.Remote{S3: &engines.S3Remote{Provider: "Other", Endpoint: "https://[fd00::5]:9000", Bucket: "media"}}, []string{"fd00::5"}},
		{"AWS without an endpoint", engines.S3, engines.Remote{S3: &engines.S3Remote{Provider: "AWS", Region: "eu-west-1", Bucket: "media"}}, []string{"s3.eu-west-1.amazonaws.com", "media.s3.eu-west-1.amazonaws.com"}},
		{"AWS without a region", engines.S3, engines.Remote{S3: &engines.S3Remote{Provider: "AWS", Bucket: "media"}}, []string{"s3.us-east-1.amazonaws.com", "media.s3.us-east-1.amazonaws.com"}},
		{"b2", engines.B2, engines.Remote{B2: &engines.B2Remote{Bucket: "media"}}, []string{"api.backblazeb2.com"}},
		{"sftp", engines.SFTP, engines.Remote{SFTP: &engines.SFTPRemote{Host: "nas.example.org", Port: 2222}}, []string{"nas.example.org"}},
		{"local", engines.Local, engines.Remote{}, nil},
		{"missing member", engines.S3, engines.Remote{}, nil},
		{"bad endpoint", engines.S3, engines.Remote{S3: &engines.S3Remote{Provider: "Other", Endpoint: "://x", Bucket: "b"}}, nil},
	}
	for _, tc := range tests {
		if got := engines.DialHosts(tc.kind, tc.r); !slices.Equal(got, tc.want) {
			t.Errorf("%s: DialHosts = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// secretsFixture holds every kind of secret.
func secretsFixture() engines.Secrets {
	return engines.Secrets{
		Credentials: engines.Credentials{
			PrivateKey:           "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQ\nAAAAAAABAAAAMwAAAAtzc2gtZWQyNTUxOQ\n-----END OPENSSH PRIVATE KEY-----\n",
			PrivateKeyPassphrase: "key-passphrase-value",
			Password:             "sftp-password-value",
			AccessKeyID:          "AKIAEXAMPLEKEYID",
			SecretAccessKey:      "s3-secret-access-key-value",
			KeyID:                "b2-key-id-value",
			ApplicationKey:       `b2-application"key<value>`,
		},
		Encryption: engines.EncryptionSecret{ResticPassword: "restic-password-value", CryptPassword: "crypt-password-value", CryptPassword2: "crypt-password2-value"},
		Obscured:   map[string]string{engines.FieldCryptPassword: "obscuredCryptPasswordToken", engines.FieldPassword: "obscuredSftpPasswordToken"},
	}
}

// TestSecretsNeverShown: every secret-bearing type logged at debug through logging.New, printed
// with every verb and marshalled shows no secret and no key-named field value (S22, §14.1
// logging).
func TestSecretsNeverShown(t *testing.T) {
	s := secretsFixture()
	values := s.Values()
	holders := []any{s, &s, s.Credentials, &s.Credentials, s.Encryption, &s.Encryption,
		struct{ Nested engines.Secrets }{s}, map[string]any{"creds": s.Credentials}}
	for _, format := range []string{"json", "text"} {
		var buf bytes.Buffer
		log, closer, err := logging.New(logging.Options{Level: "debug", StdoutFormat: format, Stdout: &buf})
		if err != nil {
			t.Fatal(err)
		}
		for i, h := range holders {
			log.Debug("secrets", fmt.Sprintf("v%d", i), h)
		}
		_ = closer.Close()
		out := buf.String()
		for _, v := range values {
			if len(v) >= 8 && strings.Contains(out, v) {
				t.Errorf("%s log shows %q:\n%s", format, v, out)
			}
		}
		if !strings.Contains(out, "privateKey") || !strings.Contains(out, "cryptPassword") {
			t.Errorf("%s log does not say which fields are set:\n%s", format, out)
		}
	}
	for _, h := range holders {
		for _, text := range []string{fmt.Sprintf("%v %+v %#v %s", h, h, h, h), mustJSON(t, h)} {
			for _, v := range values {
				if len(v) >= 8 && strings.Contains(text, v) {
					t.Errorf("%T formatted shows %q: %s", h, v, text)
				}
			}
		}
	}
	if got := mustJSON(t, s); got != `{"credentials":{"accessKeyId":true,"applicationKey":true,"keyId":true,"password":true,"privateKey":true,"privateKeyPassphrase":true,"secretAccessKey":true},"encryption":{"cryptPassword":true,"cryptPassword2":true,"resticPassword":true},"obscured":["cryptPassword","password"]}` {
		t.Errorf("Secrets JSON = %s", got)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCredentialsFieldsAndPlainJSON(t *testing.T) {
	var c engines.Credentials
	if err := json.Unmarshal([]byte(`{"accessKeyId":"AKIA1","secretAccessKey":"s3cr3t-value"}`), &c); err != nil {
		t.Fatal(err)
	}
	if !maps(c.Fields(), engines.FieldAccessKeyID, engines.FieldSecretAccessKey) || c.Empty() {
		t.Errorf("Fields = %v", c.Fields())
	}
	plain, err := c.PlainJSON()
	if err != nil {
		t.Fatal(err)
	}
	var back engines.Credentials
	if err := json.Unmarshal(plain, &back); err != nil || back != c {
		t.Errorf("PlainJSON round trip: %s -> %+v, %v", plain, back, err)
	}
	e := engines.EncryptionSecret{CryptPassword: "a-crypt-password", CryptPassword2: "a-crypt-password-2"}
	plain, _ = e.PlainJSON()
	var eb engines.EncryptionSecret
	if err := json.Unmarshal(plain, &eb); err != nil || eb != e || !maps(e.Fields(), engines.FieldCryptPassword, engines.FieldCryptPassword2) {
		t.Errorf("EncryptionSecret round trip %s %+v %v", plain, eb, e.Fields())
	}
	if !(engines.Credentials{}).Empty() {
		t.Error("zero credentials not empty")
	}
}

func maps(m map[string]bool, keys ...string) bool {
	if len(m) != len(keys) {
		return false
	}
	for _, k := range keys {
		if !m[k] {
			return false
		}
	}
	return true
}

// TestSecretsValues: every clear, obscured, JSON-escaped and Go-quoted form, and the private
// key's lines, so a command's Redact list and the registry cover what a child can print.
func TestSecretsValues(t *testing.T) {
	s := secretsFixture()
	values := s.Values()
	bs := `\`
	for _, want := range []string{
		"sftp-password-value", "obscuredCryptPasswordToken", "restic-password-value", `b2-application"key<value>`,
		"b2-application" + bs + `"key<value>`,                                                // JSON without HTML escaping and Go-quoted
		"b2-application" + bs + `"key` + bs + "u003cvalue" + bs + "u003e",                    // encoding/json
		strings.ReplaceAll(strings.TrimSuffix(s.Credentials.PrivateKey, "\n"), "\n", bs+"n"), // KEY_PEM's escaped lines
		"b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQ",                                         // a line of the key
	} {
		if !slices.ContainsFunc(values, func(v string) bool { return strings.Contains(v, want) }) {
			t.Errorf("Values lacks %q", want)
		}
	}
	if slices.Contains(values, "") {
		t.Error("Values holds an empty value")
	}
	logging.SetSecrets("destination:test-values", values...)
	defer logging.SetSecrets("destination:test-values")
	line := `{"level":"debug","msg":"Setting key_pem=\"` + strings.ReplaceAll(strings.TrimSpace(s.Credentials.PrivateKey), "\n", bs+"n") + `\""}`
	if got := logging.RedactSecrets(line); strings.Contains(got, "b3Bl") {
		t.Errorf("an escaped key survived: %s", got)
	}
}

func TestRegistry(t *testing.T) {
	fake := &enginetest.FakeEngine{EngineKind: engines.Restic}
	r := engines.Registry{engines.Restic: fake}
	if e, err := r.Get(engines.Restic); err != nil || e != fake {
		t.Errorf("Get(restic) = %v, %v", e, err)
	}
	if _, err := r.Get(engines.Rclone); !errors.Is(err, engines.ErrEngineUnavailable) {
		t.Errorf("Get(rclone) = %v", err)
	}
}

// TestDiscover: the version commands run through the runner; versions below 0.17 and 1.66, a
// missing binary, a refused path, a failing or unparseable version command make the engine
// unavailable with the reason (§10.1).
func TestDiscover(t *testing.T) {
	ok := config.EngineBinary{Path: "/usr/bin/x"}
	tests := []struct {
		name           string
		restic, rclone config.EngineBinary
		scripts        map[proc.Binary]enginetest.Script
		wantRestic     engines.BinaryStatus
		wantRclone     engines.BinaryStatus
	}{
		{"both available", ok, ok, map[proc.Binary]enginetest.Script{
			proc.Restic: {Stdout: []string{"restic 0.18.1 compiled with go1.26.8 on linux/arm64"}},
			proc.Rclone: {Stdout: []string{"rclone v1.74.1-DEV", "- os/version: alpine 3.24.2 (64 bit)"}},
		}, engines.BinaryStatus{Available: true, Version: "0.18.1", Path: "/usr/bin/x"}, engines.BinaryStatus{Available: true, Version: "1.74.1-DEV", Path: "/usr/bin/x"}},
		{"too old", ok, ok, map[proc.Binary]enginetest.Script{
			proc.Restic: {Stdout: []string{"restic 0.16.4 compiled with go1.21 on linux/amd64"}},
			proc.Rclone: {Stdout: []string{"rclone v1.65.2"}},
		}, engines.BinaryStatus{Version: "0.16.4", Path: "/usr/bin/x", Reason: "restic 0.16.4 is older than 0.17"},
			engines.BinaryStatus{Version: "1.65.2", Path: "/usr/bin/x", Reason: "rclone 1.65.2 is older than 1.66"}},
		{"not installed and refused", config.EngineBinary{Err: "restic is not installed"}, config.EngineBinary{Path: "/x/rclone", Err: "rclone path \"/x/rclone\" is writable"}, nil,
			engines.BinaryStatus{Reason: "restic is not installed"}, engines.BinaryStatus{Path: "/x/rclone", Reason: "rclone path \"/x/rclone\" is writable"}},
		{"failing and unparseable", ok, ok, map[proc.Binary]enginetest.Script{
			proc.Restic: {Stderr: []string{"exec format error"}, Exit: 1},
			proc.Rclone: {Stdout: []string{"something else"}},
		}, engines.BinaryStatus{Path: "/usr/bin/x", Reason: "restic version failed (exit 1): exec format error"},
			engines.BinaryStatus{Path: "/usr/bin/x", Reason: `cannot read the version of rclone from "something else"`}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := enginetest.NewFakeRunner(t)
			for b, s := range tc.scripts {
				r.Expect(b, enginetest.Args("version"), s)
			}
			a := engines.Discover(context.Background(), r, tc.restic, tc.rclone)
			if a.Restic != tc.wantRestic || a.Rclone != tc.wantRclone {
				t.Errorf("Discover = %+v\nwant restic %+v rclone %+v", a, tc.wantRestic, tc.wantRclone)
			}
			for _, k := range []engines.Kind{engines.Restic, engines.Rclone} {
				if err := a.Check(k); (err == nil) != a.Of(k).Available || (err != nil && !errors.Is(err, engines.ErrEngineUnavailable)) {
					t.Errorf("Check(%s) = %v", k, err)
				}
			}
			if a.Check(engines.Filecopy) != nil {
				t.Error("filecopy unavailable")
			}
		})
	}
}

// TestWindowFor: no window is always open; a destination window answers in the container's zone.
func TestWindowFor(t *testing.T) {
	loc := time.UTC
	always := engines.WindowFor(1, bwlimit.Config{}, loc)
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, loc)
	if !always.Always() || !always.IsOpen(now) || !always.NextOpen(now).Equal(now) || !always.EndsAt(now).IsZero() || always.Describe() != "" {
		t.Errorf("always-open window: %v %v", always.IsOpen(now), always.EndsAt(now))
	}
	w := engines.WindowFor(1, bwlimit.Config{Window: &bwlimit.Window{Days: bwlimit.Days, From: "01:00", To: "07:00", GraceMinutes: 15, AllowOverrun: true}}, loc)
	if w.Always() || w.IsOpen(now) || !w.NextOpen(now).Equal(time.Date(2026, 9, 8, 1, 0, 0, 0, loc)) {
		t.Errorf("window at noon: open %v, next %v", w.IsOpen(now), w.NextOpen(now))
	}
	at3 := time.Date(2026, 9, 8, 3, 0, 0, 0, loc)
	if !w.IsOpen(at3) || !w.EndsAt(at3).Equal(time.Date(2026, 9, 8, 7, 0, 0, 0, loc)) || w.Grace() != 15*time.Minute ||
		!w.AllowOverrun() || w.Length() != 6*time.Hour || w.Describe() != "01:00–07:00" {
		t.Errorf("window at 03:00: %v %v %v %v %v %q", w.IsOpen(at3), w.EndsAt(at3), w.Grace(), w.AllowOverrun(), w.Length(), w.Describe())
	}
}
