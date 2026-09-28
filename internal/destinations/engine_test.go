package destinations

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/sl0wz3r/bunkarr/internal/config"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/enginetest"
	"github.com/sl0wz3r/bunkarr/internal/engines/rclone"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/logging"
	"github.com/sl0wz3r/bunkarr/internal/netguard"
)

// engineFixture is a store with a keyring, a fake restic and a fake rclone engine, and a host
// check that resolves names through a table (metadata addresses refused like netguard).
type engineFixture struct {
	*fixture
	restic, rclone *enginetest.FakeEngine
	keyring        *config.Keyring
	// resolve maps host names to addresses for CheckHost.
	resolve map[string]string
	mu      sync.Mutex
	checked []string
}

func newEngineFixture(t *testing.T) *engineFixture {
	t.Helper()
	master := bytes.Repeat([]byte{7}, 32)
	kr, err := config.NewKeyring(master)
	if err != nil {
		t.Fatal(err)
	}
	ef := &engineFixture{keyring: kr, resolve: map[string]string{},
		restic: &enginetest.FakeEngine{EngineKind: engines.Restic}, rclone: &enginetest.FakeEngine{EngineKind: engines.Rclone}}
	n := 0
	ef.restic.OnCreate = func(_ context.Context, d engines.Destination, _ engines.Secrets, attach bool) (engines.CreateResult, error) {
		n++
		return engines.CreateResult{MarkerID: fmt.Sprintf("restic:repo%d", n), Initialized: !attach}, nil
	}
	ef.rclone.OnCreate = func(_ context.Context, d engines.Destination, _ engines.Secrets, attach bool) (engines.CreateResult, error) {
		n++
		return engines.CreateResult{MarkerID: fmt.Sprintf("marker-%d", n)}, nil
	}
	ef.fixture = newFixture(t, Options{
		Keyring: kr,
		Engines: func(k engines.Kind) (engines.Engine, bool) {
			switch k {
			case engines.Restic:
				return ef.restic, true
			case engines.Rclone:
				return ef.rclone, true
			}
			return nil, false
		},
		CheckHost: func(_ context.Context, host string) error {
			ef.mu.Lock()
			ef.checked = append(ef.checked, host)
			addr := ef.resolve[host]
			ef.mu.Unlock()
			if ip, err := netip.ParseAddr(host); err == nil && netguard.Blocked(ip) {
				return &netguard.BlockedError{Addr: host}
			}
			if ip, err := netip.ParseAddr(addr); err == nil && netguard.Blocked(ip) {
				return &netguard.BlockedError{Addr: host + " (" + addr + ")"}
			}
			return nil
		},
		LookupHost: func(_ context.Context, host string) ([]netip.Addr, error) {
			if host == "minio" {
				return []netip.Addr{netip.MustParseAddr("172.18.0.5")}, nil
			}
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		},
	})
	return ef
}

// hostKey returns a new ed25519 host key as a pinned key.
func hostKey(t *testing.T) (engines.HostKey, ssh.Signer) {
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

func rawJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func creds(t *testing.T, raw string) *CredentialsInput {
	t.Helper()
	var c CredentialsInput
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("credentials %s: %v", raw, err)
	}
	return &c
}

const (
	testAccessKey = "AKIAEXAMPLEKEY123"
	testSecretKey = "s3cr3t/Access+Key-for-tests-0001"
)

// s3Input is a restic destination on an S3 bucket.
func s3Input(t *testing.T, name, bucket, prefix string) Input {
	return Input{Name: name, Kind: engines.S3, Engine: EngineRestic,
		Remote:      rawJSON(t, map[string]any{"provider": "Minio", "endpoint": "https://s3.example.com", "region": "us-east-1", "bucket": bucket, "prefix": prefix}),
		Credentials: creds(t, fmt.Sprintf(`{"accessKeyId":%q,"secretAccessKey":%q}`, testAccessKey, testSecretKey))}
}

func (ef *engineFixture) createS3(t *testing.T, name, bucket, prefix string) Destination {
	t.Helper()
	d, err := ef.store.Create(ef.ctx, s3Input(t, name, bucket, prefix), CreateOptions{})
	if err != nil {
		t.Fatalf("Create %s: %v", name, err)
	}
	return d
}

// storedSecret reads a destination's sealed encryption_secret column.
func (ef *engineFixture) storedSecret(t *testing.T, id int64) string {
	t.Helper()
	var v sql.NullString
	if err := ef.db.Reader().QueryRow(`SELECT encryption_secret FROM destinations WHERE id = ?`, id).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v.String
}

func TestCreateEngineDestination(t *testing.T) {
	ef := newEngineFixture(t)
	src := ef.addSource(t, "Movies")
	in := s3Input(t, "B2 via S3", "media-backup", "bunkarr/main")
	in.SourceIDs = []int64{src}
	d, err := ef.store.Create(ef.ctx, in, CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if d.Engine != EngineRestic || d.Kind != engines.S3 || !d.Enabled || d.Pending || d.MarkerID != "restic:repo1" {
		t.Errorf("created %+v", d)
	}
	if d.Target != "s3:https://s3.example.com/media-backup/bunkarr/main" || d.FSType != "s3" {
		t.Errorf("target %q fsType %q", d.Target, d.FSType)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(d.EngineTag) {
		t.Errorf("engine tag %q", d.EngineTag)
	}
	if d.Encryption.Mode != engines.EncryptionRestic || d.Encryption.Origin != OriginGenerated || d.Encryption.KitConfirmedAt != nil {
		t.Errorf("encryption %+v", d.Encryption)
	}
	if Blocked(d) != BlockedKit || d.BlockedReason != BlockedKit {
		t.Errorf("blocked %q / %q, want the kit gate", Blocked(d), d.BlockedReason)
	}
	if !d.HasCredentials[engines.FieldAccessKeyID] || !d.HasCredentials[engines.FieldSecretAccessKey] || len(d.HasCredentials) != 2 {
		t.Errorf("hasCredentials %v", d.HasCredentials)
	}
	if d.Settings.Transfers != DefaultTransfers || d.Settings.Restic == nil || d.Settings.Restic.PackSizeMiB != DefaultPackSizeRemoteMiB ||
		d.Settings.Verify.SampleMaxBytes != DefaultResticSampleBytes || d.Settings.Rclone != nil {
		t.Errorf("settings %+v", d.Settings)
	}
	if k := d.Retention.SnapshotKeep(); k != (SnapshotKeep{7, 4, 6, 0}) || d.Retention.SnapshotDaily == nil {
		t.Errorf("snapshot retention %+v", k)
	}
	calls := ef.restic.Calls()
	if len(calls) != 1 || calls[0].Method != "Create" || calls[0].Attach || calls[0].Dest.MarkerID[:8] != "pending:" {
		t.Fatalf("engine calls %+v", calls)
	}
	if pw := calls[0].Secrets.Encryption.ResticPassword; len(pw) != 43 || calls[0].Secrets.Credentials.SecretAccessKey != testSecretKey {
		t.Errorf("engine got secrets %v (password length %d)", calls[0].Secrets, len(pw))
	}
	// The secrets are registered with the redaction (clear and JSON-escaped).
	if !logging.ContainsSecret("x" + testSecretKey + "x") {
		t.Error("the secret access key is not redacted")
	}
	if !logging.ContainsSecret(calls[0].Secrets.Encryption.ResticPassword) {
		t.Error("the repository password is not redacted")
	}
	// Credentials never appear in the Destination's JSON.
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{testSecretKey, testAccessKey, calls[0].Secrets.Encryption.ResticPassword} {
		if strings.Contains(string(raw), v) {
			t.Errorf("destination JSON contains a secret: %s", raw)
		}
	}
	if !strings.Contains(string(raw), `"hasCredentials":{"accessKeyId":true,"secretAccessKey":true}`) {
		t.Errorf("destination JSON: %s", raw)
	}
	// SecretsFor returns the stored location with its secrets.
	ed, sec, err := ef.store.SecretsFor(ef.ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ed.Remote.S3 == nil || ed.Remote.S3.Bucket != "media-backup" || ed.Remote.S3.Prefix != "bunkarr/main" || ed.MarkerID != "restic:repo1" ||
		ed.EngineTag != d.EngineTag || ed.Kind != engines.S3 || ed.PackSizeMiB != 64 || ed.Transfers != 4 {
		t.Errorf("SecretsFor destination %+v", ed)
	}
	if sec.Credentials.SecretAccessKey != testSecretKey || sec.Encryption.ResticPassword != calls[0].Secrets.Encryption.ResticPassword {
		t.Errorf("SecretsFor secrets %v", sec)
	}
	// A filecopy destination has no SecretsFor, and Open refuses the engine destination.
	if _, _, err := ef.store.SecretsFor(ef.ctx, ef.create(t, "UNAS").ID); !errors.Is(err, ErrNotEngine) {
		t.Errorf("SecretsFor(filecopy) = %v", err)
	}
	if _, err := ef.store.Open(ef.ctx, d.ID); !errors.Is(err, ErrEngineDestination) {
		t.Errorf("Open(engine) = %v", err)
	}
	if _, err := ef.store.Test(ef.ctx, "", d.ID); !errors.Is(err, ErrEngineDestination) {
		t.Errorf("Test(engine) = %v", err)
	}
}

func TestCreateEngineValidation(t *testing.T) {
	ef := newEngineFixture(t)
	hk, _ := hostKey(t)
	sftp := func(mod func(m map[string]any)) json.RawMessage {
		m := map[string]any{"host": "backup.example.com", "port": 22, "user": "bunkarr", "path": "backups/bunkarr",
			"hostKeys": []engines.HostKey{hk}}
		if mod != nil {
			mod(m)
		}
		return rawJSON(t, m)
	}
	s3 := func(mod func(m map[string]any)) json.RawMessage {
		m := map[string]any{"provider": "AWS", "region": "eu-west-1", "bucket": "my-bucket", "prefix": ""}
		if mod != nil {
			mod(m)
		}
		return rawJSON(t, m)
	}
	s3Creds := creds(t, `{"accessKeyId":"AKIAEXAMPLE","secretAccessKey":"abcdefghijklmnop"}`)
	sftpPass := creds(t, `{"password":"correct horse battery"}`)
	secret := func(s string) *EncryptionInput { return &EncryptionInput{Secret: s} }
	cases := []struct {
		name string
		in   Input
		o    CreateOptions
		want string
	}{
		{"unknown remote field", Input{Kind: engines.S3, Engine: EngineRestic, Remote: s3(func(m map[string]any) { m["sharedCredentialsFile"] = "/x" }), Credentials: s3Creds}, CreateOptions{}, "unknown field"},
		{"host with @", Input{Kind: engines.SFTP, Engine: EngineRclone, Remote: sftp(func(m map[string]any) { m["host"] = "user@host" }), Credentials: sftpPass}, CreateOptions{}, "remote.host"},
		{"host with a space", Input{Kind: engines.SFTP, Engine: EngineRclone, Remote: sftp(func(m map[string]any) { m["host"] = "a host" }), Credentials: sftpPass}, CreateOptions{}, "remote.host"},
		{"host key with a newline", Input{Kind: engines.SFTP, Engine: EngineRclone, Remote: sftp(func(m map[string]any) {
			m["hostKeys"] = []engines.HostKey{{Type: hk.Type, Key: hk.Key[:20] + "\n" + hk.Key[20:]}}
		}), Credentials: sftpPass}, CreateOptions{}, "hostKeys[0]"},
		{"host key type mismatch", Input{Kind: engines.SFTP, Engine: EngineRclone, Remote: sftp(func(m map[string]any) {
			m["hostKeys"] = []engines.HostKey{{Type: "ssh-rsa", Key: hk.Key}}
		}), Credentials: sftpPass}, CreateOptions{}, "does not match"},
		{"sftp without host keys", Input{Kind: engines.SFTP, Engine: EngineRclone, Remote: sftp(func(m map[string]any) { delete(m, "hostKeys") }), Credentials: sftpPass}, CreateOptions{}, "pin the server's host keys"},
		{"sftp path with ..", Input{Kind: engines.SFTP, Engine: EngineRclone, Remote: sftp(func(m map[string]any) { m["path"] = "../etc" }), Credentials: sftpPass}, CreateOptions{}, "remote.path"},
		{"short sftp password", Input{Kind: engines.SFTP, Engine: EngineRclone, Remote: sftp(nil), Credentials: creds(t, `{"password":"short"}`)}, CreateOptions{}, "at least 8"},
		{"bad region", Input{Kind: engines.S3, Engine: EngineRestic, Remote: s3(func(m map[string]any) { m["region"] = "EU_West" }), Credentials: s3Creds}, CreateOptions{}, "remote.region"},
		{"bad bucket", Input{Kind: engines.S3, Engine: EngineRestic, Remote: s3(func(m map[string]any) { m["bucket"] = "My_Bucket" }), Credentials: s3Creds}, CreateOptions{}, "remote.bucket"},
		{"prefix with a newline", Input{Kind: engines.S3, Engine: EngineRestic, Remote: s3(func(m map[string]any) { m["prefix"] = "a\nb" }), Credentials: s3Creds}, CreateOptions{}, "remote.prefix"},
		{"prefix with a carriage return", Input{Kind: engines.B2, Engine: EngineRclone, Remote: rawJSON(t, map[string]any{"bucket": "bunkarr-b2", "prefix": "x\r"}),
			Credentials: creds(t, `{"keyId":"k1234567","applicationKey":"K00abcdefghijk"}`)}, CreateOptions{}, "remote.prefix"},
		{"absolute prefix", Input{Kind: engines.S3, Engine: EngineRestic, Remote: s3(func(m map[string]any) { m["prefix"] = "/abs" }), Credentials: s3Creds}, CreateOptions{}, "remote.prefix"},
		{"endpoint with userinfo", Input{Kind: engines.S3, Engine: EngineRestic, Remote: s3(func(m map[string]any) { m["endpoint"] = "https://u:p@s3.example.com" }), Credentials: s3Creds}, CreateOptions{}, "remote.endpoint"},
		{"endpoint with a query", Input{Kind: engines.S3, Engine: EngineRestic, Remote: s3(func(m map[string]any) { m["endpoint"] = "https://s3.example.com/?x=1" }), Credentials: s3Creds}, CreateOptions{}, "remote.endpoint"},
		{"public http endpoint", Input{Kind: engines.S3, Engine: EngineRestic, Remote: s3(func(m map[string]any) { m["endpoint"] = "http://s3.example.com" }), Credentials: s3Creds}, CreateOptions{}, "use https"},
		{"storage class of another provider", Input{Kind: engines.S3, Engine: EngineRestic, Remote: s3(func(m map[string]any) { m["storageClass"] = "DEEP_ARCHIVE" }), Credentials: s3Creds}, CreateOptions{}, "storageClass"},
		{"two certificates", Input{Kind: engines.S3, Engine: EngineRestic, Remote: s3(func(m map[string]any) { m["caCert"] = "not a pem" }), Credentials: s3Creds}, CreateOptions{}, "caCert"},
		{"credentials {}", Input{Kind: engines.S3, Engine: EngineRestic, Remote: s3(nil), Credentials: creds(t, `{}`)}, CreateOptions{}, "{} changes nothing"},
		{"credentials null field", Input{Kind: engines.S3, Engine: EngineRestic, Remote: s3(nil), Credentials: creds(t, `{"accessKeyId":"AKIAEXAMPLE","secretAccessKey":null}`)}, CreateOptions{}, "null is not a value"},
		{"missing secret key", Input{Kind: engines.S3, Engine: EngineRestic, Remote: s3(nil), Credentials: creds(t, `{"accessKeyId":"AKIAEXAMPLE"}`)}, CreateOptions{}, "secretAccessKey"},
		{"credentials of another kind", Input{Kind: engines.S3, Engine: EngineRestic, Remote: s3(nil), Credentials: creds(t, `{"accessKeyId":"AKIAEXAMPLE","secretAccessKey":"abcdefghijklmnop","keyId":"x1234567"}`)}, CreateOptions{}, "keyId"},
		{"user secret with surrounding whitespace", Input{Kind: engines.S3, Engine: EngineRestic, Remote: s3(nil), Credentials: s3Creds, Encryption: secret(" a long enough secret")}, CreateOptions{}, "whitespace"},
		{"short user secret", Input{Kind: engines.S3, Engine: EngineRestic, Remote: s3(nil), Credentials: s3Creds, Encryption: secret("too short")}, CreateOptions{}, "at least 16"},
		{"user secret with a control character", Input{Kind: engines.S3, Engine: EngineRestic, Remote: s3(nil), Credentials: s3Creds, Encryption: secret("a long enough\tsecret")}, CreateOptions{}, "control"},
		{"rclone none without acceptUnencrypted", Input{Kind: engines.S3, Engine: EngineRclone, Remote: s3(nil), Credentials: s3Creds, Encryption: &EncryptionInput{Mode: engines.EncryptionNone}}, CreateOptions{}, "acceptUnencrypted"},
		{"restic without encryption", Input{Kind: engines.S3, Engine: EngineRestic, Remote: s3(nil), Credentials: s3Creds, Encryption: &EncryptionInput{Mode: engines.EncryptionNone, AcceptUnencrypted: true}}, CreateOptions{}, "always encrypted"},
		{"attach without a secret", Input{Kind: engines.S3, Engine: EngineRestic, Remote: s3(nil), Credentials: s3Creds}, CreateOptions{Attach: true}, "attach needs"},
		{"filecopy on s3", Input{Kind: engines.S3, Engine: EngineFilecopy, Remote: s3(nil), Credentials: s3Creds}, CreateOptions{}, "filecopy writes to local"},
		{"remote kind without engine", Input{Kind: engines.S3, Remote: s3(nil), Credentials: s3Creds}, CreateOptions{}, "uses restic or rclone"},
		{"rclone on a local path", Input{Kind: engines.Local, Engine: EngineRclone, Target: "/tmp"}, CreateOptions{}, "local path uses filecopy or restic"},
		{"restic settings on rclone", Input{Kind: engines.S3, Engine: EngineRclone, Remote: s3(nil), Credentials: s3Creds, Settings: &Settings{Restic: &ResticSettings{}}}, CreateOptions{}, "settings.restic"},
		{"snapshot retention on rclone", Input{Kind: engines.S3, Engine: EngineRclone, Remote: s3(nil), Credentials: s3Creds, Retention: &Retention{SnapshotDaily: new(int)}}, CreateOptions{}, "snapshotDaily"},
		{"pack size out of range", Input{Kind: engines.S3, Engine: EngineRestic, Remote: s3(nil), Credentials: s3Creds, Settings: &Settings{Restic: &ResticSettings{PackSizeMiB: 256}}}, CreateOptions{}, "packSizeMiB"},
		{"max unused not a size", Input{Kind: engines.S3, Engine: EngineRestic, Remote: s3(nil), Credentials: s3Creds, Settings: &Settings{Restic: &ResticSettings{PruneMaxUnused: "10% --unsafe"}}}, CreateOptions{}, "pruneMaxUnused"},
		{"transfers out of range", Input{Kind: engines.S3, Engine: EngineRestic, Remote: s3(nil), Credentials: s3Creds, Settings: &Settings{Transfers: 64}}, CreateOptions{}, "transfers"},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.in.Name = fmt.Sprintf("dest %d", i)
			_, err := ef.store.Create(ef.ctx, c.in, c.o)
			var ve ValidationError
			if !errors.As(err, &ve) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want a ValidationError with %q", err, c.want)
			}
		})
	}
	if calls := ef.restic.Calls(); len(calls) != 0 {
		t.Errorf("an engine ran for invalid input: %+v", calls)
	}
	var n int
	if err := ef.db.Reader().QueryRow(`SELECT COUNT(*) FROM destinations`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d rows after invalid creates (%v)", n, err)
	}
	// A filecopy destination refuses engine fields and keeps its Phase 1-3 settings JSON.
	if _, err := ef.store.Create(ef.ctx, Input{Name: "fc", Target: ef.target(t, "fc"), Settings: &Settings{Transfers: 2}}, CreateOptions{AllowLocal: true}); err == nil {
		t.Error("filecopy with transfers accepted")
	}
	fc := ef.create(t, "UNAS")
	var settings, retention, bandwidth string
	if err := ef.db.Reader().QueryRow(`SELECT settings, retention, bandwidth FROM destinations WHERE id = ?`, fc.ID).Scan(&settings, &retention, &bandwidth); err != nil {
		t.Fatal(err)
	}
	if settings != `{"verify":{"mode":"sample","samplePercent":5},"hardlinks":"recreate","adoptExisting":"size+mtime","mtimeWindowSec":0,"maxChangePercent":10,"maxChangeFiles":1000}` ||
		!strings.HasPrefix(retention, `{"deletedDays":30,`) || strings.Contains(retention, "snapshot") || bandwidth != "{}" {
		t.Errorf("filecopy row: settings %s retention %s bandwidth %s", settings, retention, bandwidth)
	}
	if fc.Kind != engines.Local || fc.Encryption.Mode != engines.EncryptionNone || Blocked(fc) != "" || len(fc.HasCredentials) != 0 {
		t.Errorf("filecopy destination %+v", fc)
	}
}

func TestCreateHTTPEndpointOnPrivateNetwork(t *testing.T) {
	ef := newEngineFixture(t)
	in := s3Input(t, "MinIO", "bunkarr", "")
	in.Remote = rawJSON(t, map[string]any{"provider": "Minio", "endpoint": "http://MinIO:9000/", "bucket": "bunkarr"})
	d, err := ef.store.Create(ef.ctx, in, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Remote.S3.Endpoint != "http://minio:9000" || len(d.Warnings) != 1 || !strings.Contains(d.Warnings[0], "not encrypted") {
		t.Errorf("endpoint %q warnings %v", d.Remote.S3.Endpoint, d.Warnings)
	}
}

func TestRemoteOverlap(t *testing.T) {
	ef := newEngineFixture(t)
	ef.createS3(t, "One", "shared", "a")
	for _, c := range []struct {
		bucket, prefix string
		ok             bool
	}{{"shared", "a", false}, {"shared", "a/b", false}, {"shared", "", false}, {"shared", "ab", true}, {"other", "a", true}} {
		_, err := ef.store.Create(ef.ctx, s3Input(t, "x "+c.bucket+c.prefix, c.bucket, c.prefix), CreateOptions{})
		if (err == nil) != c.ok {
			t.Errorf("%s/%s: err = %v, want ok %v", c.bucket, c.prefix, err, c.ok)
		}
	}
	// A b2 destination and an s3 one on B2's S3 endpoint are the same storage.
	b2 := Input{Name: "B2", Kind: engines.B2, Engine: EngineRclone, Remote: rawJSON(t, map[string]any{"bucket": "family-b2", "prefix": "bunkarr"}),
		Credentials: creds(t, `{"keyId":"0012345678","applicationKey":"K001abcdefghijkl"}`)}
	srv := newB2Server(t, `{"apiUrl":"https://api001.backblazeb2.com","allowed":{"bucketId":"b1","bucketName":"family-b2"}}`)
	ef.store.opts.HTTPClient, ef.store.b2AuthURL = srv.Client(), srv.URL
	if _, err := ef.store.Create(ef.ctx, b2, CreateOptions{}); err != nil {
		t.Fatalf("create b2: %v", err)
	}
	viaS3 := s3Input(t, "B2 via S3", "family-b2", "bunkarr/restic")
	viaS3.Remote = rawJSON(t, map[string]any{"provider": "Other", "endpoint": "https://s3.us-west-004.backblazeb2.com", "region": "us-west-004",
		"bucket": "family-b2", "prefix": "bunkarr/restic"})
	if _, err := ef.store.Create(ef.ctx, viaS3, CreateOptions{}); err == nil || !strings.Contains(err.Error(), "overlaps destination \"B2\"") {
		t.Errorf("s3 on B2's endpoint inside the b2 destination: err = %v", err)
	}
	viaS3.Remote = rawJSON(t, map[string]any{"provider": "Other", "endpoint": "https://s3.us-west-004.backblazeb2.com", "bucket": "family-b2", "prefix": "elsewhere"})
	if _, err := ef.store.Create(ef.ctx, viaS3, CreateOptions{}); err != nil {
		t.Errorf("another prefix of the same bucket: %v", err)
	}
}

func TestCreateHostChecks(t *testing.T) {
	ef := newEngineFixture(t)
	ef.resolve["bucket.metadata.test"] = "169.254.169.254"
	in := s3Input(t, "Meta", "bucket", "")
	in.Remote = rawJSON(t, map[string]any{"provider": "Other", "endpoint": "https://metadata.test", "bucket": "bucket"})
	_, err := ef.store.Create(ef.ctx, in, CreateOptions{})
	var ve ValidationError
	if !errors.Is(err, netguard.ErrBlocked) || !errors.As(err, &ve) {
		t.Fatalf("a dial host resolving to a metadata address: err = %v", err)
	}
	if !slices.Contains(ef.checked, "bucket.metadata.test") {
		t.Errorf("checked %v, want the virtual-hosted dial host", ef.checked)
	}
	// With forcePathStyle the endpoint host itself is dialed (and checked).
	in.Remote = rawJSON(t, map[string]any{"provider": "Other", "endpoint": "https://169.254.169.254", "bucket": "bucket", "forcePathStyle": true})
	if _, err := ef.store.Create(ef.ctx, in, CreateOptions{}); !errors.Is(err, netguard.ErrBlocked) {
		t.Errorf("a metadata endpoint: err = %v", err)
	}
	if len(ef.restic.Calls()) != 0 {
		t.Error("the engine ran for a refused host")
	}
}

func TestCreateAttachConfirmsAndWarns(t *testing.T) {
	ef := newEngineFixture(t)
	ef.restic.OnCreate = func(_ context.Context, _ engines.Destination, s engines.Secrets, attach bool) (engines.CreateResult, error) {
		if !attach || s.Encryption.ResticPassword != "the existing repository password" {
			return engines.CreateResult{}, errors.New("unexpected create")
		}
		return engines.CreateResult{MarkerID: "restic:existing", Warnings: []string{"another Bunkarr may be writing to this repository"}}, nil
	}
	in := s3Input(t, "Attached", "attached", "")
	in.Encryption = &EncryptionInput{Secret: "the existing repository password"}
	d, err := ef.store.Create(ef.ctx, in, CreateOptions{Attach: true})
	if err != nil {
		t.Fatal(err)
	}
	if d.Encryption.Origin != OriginUser || d.Encryption.KitConfirmedAt == nil || Blocked(d) != "" || d.MarkerID != "restic:existing" {
		t.Errorf("attached %+v", d)
	}
	if !slices.Equal(d.Warnings, []string{"another Bunkarr may be writing to this repository"}) {
		t.Errorf("warnings %v", d.Warnings)
	}
	// The same repository again is refused (marker_id is unique), and the row is not kept.
	in.Name = "Twice"
	in.Remote = rawJSON(t, map[string]any{"provider": "Minio", "endpoint": "https://s3.example.com", "bucket": "attached-two"})
	if _, err := ef.store.Create(ef.ctx, in, CreateOptions{Attach: true}); !errors.Is(err, ErrMarkerExists) {
		t.Errorf("second attach of one repository: %v", err)
	}
	if all, _ := ef.store.List(ef.ctx); len(all) != 1 {
		t.Errorf("%d destinations after a refused attach", len(all))
	}
}

func TestCreateEngineFailureDeletesRow(t *testing.T) {
	ef := newEngineFixture(t)
	ef.restic.OnCreate = func(context.Context, engines.Destination, engines.Secrets, bool) (engines.CreateResult, error) {
		return engines.CreateResult{}, fmt.Errorf("bucket not found for key %s", testSecretKey)
	}
	_, err := ef.store.Create(ef.ctx, s3Input(t, "Broken", "broken", ""), CreateOptions{})
	if err == nil || strings.Contains(err.Error(), testSecretKey) || !strings.Contains(err.Error(), logging.Redacted) {
		t.Fatalf("err = %v (want the secret redacted)", err)
	}
	if all, _ := ef.store.List(ef.ctx); len(all) != 0 {
		t.Errorf("rows after a failed create: %+v", all)
	}
}

func TestCreateCrashAfterInitKeepsPendingRow(t *testing.T) {
	ef := newEngineFixture(t)
	var initialized engines.Secrets
	ef.restic.OnCreate = func(_ context.Context, d engines.Destination, s engines.Secrets, attach bool) (engines.CreateResult, error) {
		if !attach {
			initialized = s
			return engines.CreateResult{MarkerID: "restic:fresh", Initialized: true}, nil
		}
		if s.Encryption != initialized.Encryption {
			return engines.CreateResult{}, engines.ErrWrongPassword
		}
		return engines.CreateResult{MarkerID: "restic:fresh"}, nil
	}
	faultinject.SetHook(faultinject.CrashAt(PointCreateAfterInit, 1))
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("no crash")
			} else if _, ok := r.(faultinject.Crash); !ok {
				panic(r)
			}
		}()
		_, _ = ef.store.Create(ef.ctx, s3Input(t, "Crashed", "crashed", ""), CreateOptions{})
	}()
	faultinject.SetHook(nil)
	all, err := ef.store.List(ef.ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("after the crash: %v %+v", err, all)
	}
	p := all[0]
	if !p.Pending || p.Enabled || Blocked(p) != BlockedPending || !strings.HasPrefix(p.MarkerID, "pending:") {
		t.Errorf("pending row %+v", p)
	}
	_, sec, err := ef.store.SecretsFor(ef.ctx, p.ID)
	if err != nil || sec.Encryption != initialized.Encryption {
		t.Fatalf("the pending row's sealed secret: %v (matches %v)", err, sec.Encryption == initialized.Encryption)
	}
	// Its kit can be exported; it cannot be enabled; a create of the same location resumes it.
	if _, err := ef.store.RecoveryKit(ef.ctx, p.ID, false); err != nil {
		t.Errorf("kit of a pending row: %v", err)
	}
	on := true
	if _, err := ef.store.Update(ef.ctx, p.ID, Input{Enabled: &on}); err == nil {
		t.Error("a pending row was enabled")
	}
	if err := ef.store.Delete(ef.ctx, p.ID); !errors.Is(err, ErrSecretNotConfirmed) {
		t.Errorf("delete of a pending row without confirmLoseSecret: %v", err)
	}
	d, err := ef.store.Create(ef.ctx, s3Input(t, "Crashed", "crashed", ""), CreateOptions{})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if d.ID != p.ID || d.Pending || !d.Enabled || d.MarkerID != "restic:fresh" || d.Encryption.KitConfirmedAt != nil {
		t.Errorf("resumed %+v", d)
	}
	calls := ef.restic.Calls()
	if last := calls[len(calls)-1]; !last.Attach || last.Secrets.Encryption != initialized.Encryption {
		t.Errorf("resume call %+v", last)
	}
}

func TestDeleteNeedsConfirmLoseSecret(t *testing.T) {
	ef := newEngineFixture(t)
	d := ef.createS3(t, "Unconfirmed", "unconfirmed", "")
	if err := ef.store.Delete(ef.ctx, d.ID); !errors.Is(err, ErrSecretNotConfirmed) {
		t.Fatalf("delete: %v", err)
	}
	if err := ef.store.Delete(ef.ctx, d.ID, DeleteOptions{ConfirmLoseSecret: true}); err != nil {
		t.Fatalf("delete with confirmLoseSecret: %v", err)
	}
	confirmed := ef.createS3(t, "Confirmed", "confirmed", "")
	kit, err := ef.store.RecoveryKit(ef.ctx, confirmed.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := ef.store.ConfirmKit(ef.ctx, confirmed.ID, KitConfirmation{CheckCode: kitCheckCode(t, kit.Content)}); err != nil {
		t.Fatal(err)
	}
	if err := ef.store.Delete(ef.ctx, confirmed.ID); err != nil {
		t.Errorf("delete of a confirmed destination: %v", err)
	}
}

func TestUpdateEngineDestination(t *testing.T) {
	ef := newEngineFixture(t)
	d := ef.createS3(t, "S3", "updates", "p")
	ef.restic.OnTest = func(_ context.Context, td engines.Destination, s engines.Secrets) (engines.TestResult, error) {
		if s.Credentials.SecretAccessKey == "wrong-secret-key-000" {
			return engines.TestResult{Message: "access denied"}, nil
		}
		return engines.TestResult{OK: true, Reachable: true, Repository: engines.RepositoryExists, ID: "repo1"}, nil
	}
	for _, c := range []struct {
		name string
		in   Input
		want string
	}{
		{"kind", Input{Kind: engines.B2}, "kind"},
		{"engine", Input{Engine: EngineRclone}, "engine"},
		{"target", Input{Target: "s3:elsewhere"}, "target"},
		{"location", Input{Remote: rawJSON(t, map[string]any{"provider": "Minio", "endpoint": "https://s3.example.com", "region": "us-east-1", "bucket": "updates", "prefix": "q"})}, "location"},
		{"encryption", Input{Encryption: &EncryptionInput{Secret: "a new secret for everything"}}, "encryption"},
		{"credentials {}", Input{Credentials: creds(t, `{}`)}, "{} changes nothing"},
		{"wrong credentials", Input{Credentials: creds(t, `{"secretAccessKey":"wrong-secret-key-000"}`)}, "access denied"},
		{"snapshot retention out of range", Input{Retention: &Retention{SnapshotYearly: func() *int { v := 101; return &v }()}}, "snapshotYearly"},
	} {
		if _, err := ef.store.Update(ef.ctx, d.ID, c.in); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
	// Same mode resent: fine. Settings and retention of restic change.
	zero := 0
	up, err := ef.store.Update(ef.ctx, d.ID, Input{Name: "S3 renamed", Encryption: &EncryptionInput{Mode: engines.EncryptionRestic},
		Settings: &Settings{Transfers: 8, Restic: &ResticSettings{PruneEveryDays: 30}}, Retention: &Retention{SnapshotDaily: &zero}})
	if err != nil {
		t.Fatal(err)
	}
	if up.Name != "S3 renamed" || up.Settings.Transfers != 8 || up.Settings.Restic.PruneEveryDays != 30 || up.Settings.Restic.PackSizeMiB != 64 ||
		up.Retention.SnapshotKeep() != (SnapshotKeep{0, 4, 6, 0}) {
		t.Errorf("updated %+v %+v", up.Settings, up.Retention.SnapshotKeep())
	}
	before := ef.storedSecret(t, d.ID)
	// 50 concurrent updates rotating secretAccessKey keep the encryption secret byte-identical.
	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := ef.store.Update(ef.ctx, d.ID, Input{Credentials: creds(t, fmt.Sprintf(`{"secretAccessKey":"rotated-secret-key-%03d"}`, i))})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent update: %v", err)
		}
	}
	if after := ef.storedSecret(t, d.ID); after != before {
		t.Error("encryption_secret changed during credential rotations")
	}
	_, sec, err := ef.store.SecretsFor(ef.ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sec.Credentials.SecretAccessKey, "rotated-secret-key-") || sec.Credentials.AccessKeyID != testAccessKey {
		t.Errorf("merged credentials %v", sec)
	}
	if !logging.ContainsSecret(sec.Credentials.SecretAccessKey) {
		t.Error("the rotated key is not registered with the redaction")
	}
}

func TestUpdateHostKeys(t *testing.T) {
	ef := newEngineFixture(t)
	hk1, _ := hostKey(t)
	hk2, _ := hostKey(t)
	remote := func(keys ...engines.HostKey) json.RawMessage {
		return rawJSON(t, map[string]any{"host": "Backup.Example.com", "user": "bunkarr", "path": "/srv/backup", "hostKeys": keys})
	}
	d, err := ef.store.Create(ef.ctx, Input{Name: "SFTP", Kind: engines.SFTP, Engine: EngineRclone, Remote: remote(hk1),
		Credentials: creds(t, `{"password":"correct horse battery"}`)}, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Remote.SFTP.Host != "backup.example.com" || d.Remote.SFTP.Port != 22 || d.Target != "sftp://bunkarr@backup.example.com:22/srv/backup" ||
		d.Encryption.Mode != engines.EncryptionCrypt {
		t.Errorf("sftp destination %+v %+v", d.Remote.SFTP, d.Encryption)
	}
	ef.rclone.OnTest = func(_ context.Context, td engines.Destination, _ engines.Secrets) (engines.TestResult, error) {
		return engines.TestResult{OK: true, Marker: engines.MarkerOK, ID: td.MarkerID}, nil
	}
	up, err := ef.store.Update(ef.ctx, d.ID, Input{Remote: remote(hk1, hk2)})
	if err != nil {
		t.Fatal(err)
	}
	if len(up.Remote.SFTP.HostKeys) != 2 {
		t.Errorf("host keys %v", up.Remote.SFTP.HostKeys)
	}
	calls := ef.rclone.Calls()
	if last := calls[len(calls)-1]; last.Method != "Test" || len(last.Dest.Remote.SFTP.HostKeys) != 2 {
		t.Errorf("the new host keys were not tested first: %+v", last)
	}
}

func TestSecretUpdateSQL(t *testing.T) {
	// No UPDATE statement of this package names encryption_secret: it is written once, by the
	// INSERT of Create (S21).
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	stmt := regexp.MustCompile("(?is)UPDATE\\s+destinations\\s+SET[^`]*")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range stmt.FindAllString(string(src), -1) {
			if strings.Contains(strings.ToLower(m), "encryption_secret") {
				t.Errorf("%s: an UPDATE names encryption_secret: %s", f, m)
			}
		}
	}
	if _, err := os.Stat("engine.go"); errors.Is(err, fs.ErrNotExist) {
		t.Fatal("run from the package directory")
	}
}

func TestTestRemote(t *testing.T) {
	ef := newEngineFixture(t)
	hk, signer := hostKey(t)
	var scanned int
	ef.store.scanHostKeys = func(_ context.Context, host string, port int) ([]engines.HostKeyInfo, error) {
		scanned++
		fp, _ := rclone.Fingerprint(hk)
		_ = signer
		return []engines.HostKeyInfo{{Type: hk.Type, Fingerprint: fp, Key: hk.Key}}, nil
	}
	in := TestInput{Kind: engines.SFTP, Engine: EngineRclone,
		Remote:      rawJSON(t, map[string]any{"host": "sftp.example.com", "user": "u", "path": "b"}),
		Credentials: creds(t, `{"password":"typed-password-123"}`)}
	res, err := ef.store.TestRemote(ef.ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || len(res.HostKeys) != 1 || scanned != 1 || len(ef.rclone.Calls()) != 0 {
		t.Errorf("sftp test without host keys: %+v, scans %d, engine calls %d", res, scanned, len(ef.rclone.Calls()))
	}
	// With the keys pinned the engine tests; its answer has the request's secrets redacted.
	in.Remote = rawJSON(t, map[string]any{"host": "sftp.example.com", "user": "u", "path": "b", "hostKeys": []engines.HostKey{hk}})
	ef.rclone.OnTest = func(_ context.Context, d engines.Destination, s engines.Secrets) (engines.TestResult, error) {
		if d.ID != 0 || s.Obscured[engines.FieldPassword] == "" || s.Obscured[engines.FieldCryptPassword] == "" {
			return engines.TestResult{}, errors.New("unexpected test input")
		}
		return engines.TestResult{OK: true, Marker: engines.MarkerMissing,
			Message: "used " + s.Credentials.Password + " and " + s.Obscured[engines.FieldPassword]}, nil
	}
	res, err = ef.store.TestRemote(ef.ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || strings.Contains(res.Message, "typed-password-123") || strings.Count(res.Message, logging.Redacted) != 2 {
		t.Errorf("test result %+v", res)
	}
	if logging.ContainsSecret("typed-password-123") {
		t.Error("a Test's password entered the process-wide registry")
	}
}

func TestB2Authorize(t *testing.T) {
	ef := newEngineFixture(t)
	var auth []string
	srv := newB2Server(t, `{"apiUrl":"https://api002.backblazeb2.com","authorizationToken":"tok","allowed":{"bucketId":null,"bucketName":null}}`)
	srv.Config.Handler = b2Handler(&auth, `{"apiUrl":"https://api002.backblazeb2.com","authorizationToken":"tok","allowed":{"bucketId":null,"bucketName":null}}`)
	ef.store.opts.HTTPClient, ef.store.b2AuthURL = srv.Client(), srv.URL
	d, err := ef.store.Create(ef.ctx, Input{Name: "B2", Kind: engines.B2, Engine: EngineRestic, Remote: rawJSON(t, map[string]any{"bucket": "family-b2"}),
		Credentials: creds(t, `{"keyId":"0012345678","applicationKey":"K001abcdefghijkl"}`)}, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Warnings) != 1 || !strings.Contains(d.Warnings[0], "not restricted to bucket") {
		t.Errorf("warnings %v", d.Warnings)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("0012345678:K001abcdefghijkl"))
	if len(auth) != 1 || auth[0] != want {
		t.Errorf("authorization headers %v", auth)
	}
	if !slices.Contains(ef.checked, "api002.backblazeb2.com") || !slices.Contains(ef.checked, engines.B2APIHost) {
		t.Errorf("checked hosts %v", ef.checked)
	}
	// A key restricted to another bucket is refused.
	srv.Config.Handler = b2Handler(&auth, `{"apiUrl":"https://api002.backblazeb2.com","allowed":{"bucketId":"x","bucketName":"other"}}`)
	if _, err := ef.store.Create(ef.ctx, Input{Name: "B2 two", Kind: engines.B2, Engine: EngineRestic, Remote: rawJSON(t, map[string]any{"bucket": "second-b2"}),
		Credentials: creds(t, `{"keyId":"0012345678","applicationKey":"K001abcdefghijkl"}`)}, CreateOptions{}); err == nil || !strings.Contains(err.Error(), "restricted to bucket \"other\"") {
		t.Errorf("key of another bucket: %v", err)
	}
}

func TestCreateLocalRestic(t *testing.T) {
	ef := newEngineFixture(t)
	repo := ef.target(t, "repo")
	d, err := ef.store.Create(ef.ctx, Input{Name: "Local restic", Engine: EngineRestic, Target: repo}, CreateOptions{AllowLocal: true})
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != engines.Local || d.Target != repo || d.FSType == "" || d.FSType == "local" || d.Settings.Restic.PackSizeMiB != DefaultPackSizeLocalMiB ||
		d.Encryption.Mode != engines.EncryptionRestic || d.Encryption.Origin != OriginGenerated || d.EngineTag == "" {
		t.Errorf("local restic %+v", d)
	}
	calls := ef.restic.Calls()
	if len(calls) != 1 || calls[0].Dest.Target != repo || calls[0].Dest.Kind != engines.Local {
		t.Errorf("engine calls %+v", calls)
	}
	// The S4 rules cover it: a filecopy destination inside it, and a second repository on it.
	if _, err := ef.store.Create(ef.ctx, Input{Name: "Inside", Target: ef.target(t, "repo/inner")}, CreateOptions{AllowLocal: true}); err == nil {
		t.Error("a filecopy destination inside a restic repository was accepted")
	}
	if _, err := ef.store.Create(ef.ctx, Input{Name: "Again", Engine: EngineRestic, Target: repo}, CreateOptions{AllowLocal: true}); err == nil {
		t.Error("a second repository on the same directory was accepted")
	}
	if _, err := ef.store.Create(ef.ctx, Input{Name: "Missing", Engine: EngineRestic, Target: filepath.Join(ef.base, "nope")}, CreateOptions{AllowLocal: true}); err == nil {
		t.Error("a repository on a missing directory was accepted")
	}
	// Its secrets register at start-up; another bunkarr.key cannot open them.
	if err := ef.store.RegisterSecrets(ef.ctx); err != nil {
		t.Errorf("RegisterSecrets: %v", err)
	}
	other, err := config.NewKeyring(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	foreign := New(ef.db, Options{Keyring: other})
	if err := foreign.RegisterSecrets(ef.ctx); !errors.Is(err, config.ErrKeyMismatch) {
		t.Errorf("RegisterSecrets with another key: %v", err)
	}
	if _, _, err := foreign.SecretsFor(ef.ctx, d.ID); !errors.Is(err, config.ErrKeyMismatch) {
		t.Errorf("SecretsFor with another key: %v", err)
	}
}

func TestTestStored(t *testing.T) {
	ef := newEngineFixture(t)
	d := ef.createS3(t, "Stored", "stored", "")
	ef.restic.OnTest = func(_ context.Context, td engines.Destination, s engines.Secrets) (engines.TestResult, error) {
		if td.ID != d.ID || s.Credentials.SecretAccessKey != testSecretKey {
			return engines.TestResult{}, errors.New("not the stored destination")
		}
		return engines.TestResult{OK: true, Repository: engines.RepositoryExists, ID: "other", Message: "key " + testSecretKey}, nil
	}
	res, err := ef.store.TestStored(ef.ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || strings.Contains(res.Message, testSecretKey) {
		t.Errorf("another repository at the location: %+v", res)
	}
	ef.restic.OnTest = func(context.Context, engines.Destination, engines.Secrets) (engines.TestResult, error) {
		return engines.TestResult{OK: true, Repository: engines.RepositoryExists, ID: "repo1"}, nil
	}
	if res, err := ef.store.TestStored(ef.ctx, d.ID); err != nil || !res.OK {
		t.Errorf("the right repository: %+v %v", res, err)
	}
	if _, err := ef.store.TestStored(ef.ctx, ef.create(t, "UNAS").ID); !errors.Is(err, ErrNotEngine) {
		t.Errorf("TestStored(filecopy): %v", err)
	}
}
