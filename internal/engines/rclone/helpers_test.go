package rclone

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/enginetest"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
)

// obscureKey is a fixed key for the tests' deterministic obscuring.
var obscureKey = []byte("0123456789abcdef0123456789abcdef")

// newTestDriver returns a driver on a FakeRunner with real run directories and no retry wait.
func newTestDriver(t *testing.T) (*Driver, *enginetest.FakeRunner) {
	t.Helper()
	f := enginetest.NewFakeRunner(t)
	t.Cleanup(func() { assertSafeArgv(t, f) })
	return &Driver{Runner: f, RunDirs: enginetest.RunDirs(t), RetryWait: -1, Version: "1.74.1"}, f
}

// connect binds dest for a job (id 7).
func connect(t *testing.T, d *Driver, dest engines.Destination, s engines.Secrets) *Conn {
	t.Helper()
	c, err := d.Connect(dest, s, engines.Runtime{JobID: 7})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// jobS3 is an s3 destination on bucket "media" under prefix "bk", with crypt when crypt is set.
func jobS3(crypt bool) (engines.Destination, engines.Secrets) {
	d := engines.Destination{ID: 3, Name: "offsite", Engine: engines.Rclone, Kind: engines.S3, Encryption: engines.EncryptionNone,
		MarkerID: "5f3c1a2e-7d4b-4c7a-9f1e-2b3c4d5e6f70", Transfers: 4,
		Remote: engines.Remote{S3: &engines.S3Remote{Provider: "Minio", Endpoint: "https://minio.example:9000", Region: "us-east-1",
			Bucket: "media", Prefix: "bk", ForcePathStyle: true}}}
	s := engines.Secrets{Credentials: engines.Credentials{AccessKeyID: "AKIAEXAMPLEKEY01", SecretAccessKey: "s3-secret-access-key-0123456789"},
		Obscured: map[string]string{}}
	if crypt {
		d.Encryption = engines.EncryptionCrypt
		s.Encryption = engines.EncryptionSecret{CryptPassword: "crypt-password-0123456789", CryptPassword2: "crypt-salt-0123456789"}
		s.Obscured[engines.FieldCryptPassword] = Obscure(obscureKey, d.ID, engines.FieldCryptPassword, s.Encryption.CryptPassword)
		s.Obscured[engines.FieldCryptPassword2] = Obscure(obscureKey, d.ID, engines.FieldCryptPassword2, s.Encryption.CryptPassword2)
	}
	return d, s
}

// jobB2 is a plain b2 destination.
func jobB2() (engines.Destination, engines.Secrets) {
	d := engines.Destination{ID: 4, Name: "b2", Engine: engines.Rclone, Kind: engines.B2, Encryption: engines.EncryptionNone,
		MarkerID: "b2-marker", Remote: engines.Remote{B2: &engines.B2Remote{Bucket: "bkt", Prefix: "p/q"}}}
	s := engines.Secrets{Credentials: engines.Credentials{KeyID: "b2-key-id-0001", ApplicationKey: "b2-application-key-0123456789"}}
	return d, s
}

// freshHostKey returns a fresh ed25519 host key in its pinned form.
func freshHostKey(t *testing.T) engines.HostKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return engines.HostKey{Type: pk.Type(), Key: base64.StdEncoding.EncodeToString(pk.Marshal())}
}

// jobSFTP is an sftp destination on port 2222 with one pinned key and a private key login.
func jobSFTP(t *testing.T, crypt bool) (engines.Destination, engines.Secrets) {
	t.Helper()
	d := engines.Destination{ID: 5, Name: "sftp", Engine: engines.Rclone, Kind: engines.SFTP, Encryption: engines.EncryptionNone,
		MarkerID: "sftp-marker", Remote: engines.Remote{SFTP: &engines.SFTPRemote{Host: "nas.example", Port: 2222, User: "bk",
			Path: "/backups/bunkarr", HostKeys: []engines.HostKey{freshHostKey(t)}}}}
	s := engines.Secrets{Credentials: engines.Credentials{PrivateKey: "-----BEGIN OPENSSH PRIVATE KEY-----\n" +
		"b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW\n-----END OPENSSH PRIVATE KEY-----\n"},
		Obscured: map[string]string{}}
	if crypt {
		d.Encryption = engines.EncryptionCrypt
		s.Encryption = engines.EncryptionSecret{CryptPassword: "sftp-crypt-password-01", CryptPassword2: "sftp-crypt-salt-01"}
		s.Obscured[engines.FieldCryptPassword] = Obscure(obscureKey, d.ID, engines.FieldCryptPassword, s.Encryption.CryptPassword)
		s.Obscured[engines.FieldCryptPassword2] = Obscure(obscureKey, d.ID, engines.FieldCryptPassword2, s.Encryption.CryptPassword2)
	}
	return d, s
}

// forbiddenWords are subcommands and flags Bunkarr never passes to rclone (S1, S22, S23).
var forbiddenWords = []string{"sync", "bisync", "cleanup", "--track-renames", "-L", "--copy-links", "--links", "-vv",
	"--dry-run", "--delete-excluded", "--delete-before", "--delete-during", "--delete-after", "--no-check-certificate"}

// assertSafeArgv checks every recorded rclone command: no forbidden word; a top-level cleanup is
// never run (only "backend cleanup"); every removing command carries --max-delete.
func assertSafeArgv(t *testing.T, f *enginetest.FakeRunner) {
	t.Helper()
	for _, c := range f.Calls() {
		if c.Binary != proc.Rclone {
			continue
		}
		sub := c.Subcommand()
		for i, a := range c.Args {
			if i == 1 && sub == "backend cleanup" {
				continue
			}
			if slices.Contains(forbiddenWords, a) || strings.HasPrefix(a, "--delete-") {
				t.Errorf("forbidden rclone argument %q in %s", a, c)
			}
		}
		removing := sub == "move" || sub == "moveto" || sub == "delete" || sub == "deletefile" || sub == "purge" ||
			(sub == "copy" && c.Has("--backup-dir"))
		if removing && !c.Has("--max-delete") {
			t.Errorf("removing command without --max-delete: %s", c)
		}
	}
}

// lsjsonOut renders objects as rclone lsjson prints them.
func lsjsonOut(objs ...string) []string {
	out := []string{"["}
	for i, o := range objs {
		if i < len(objs)-1 {
			o += ","
		}
		out = append(out, o)
	}
	return append(out, "]")
}

// logScript answers a command with a fixture of testdata/rclone printed on stderr, where rclone
// writes its log, and the fixture's exit code.
func logScript(t *testing.T, name string) enginetest.Script {
	t.Helper()
	s := enginetest.FixtureScript(t, "rclone", name)
	s.Stderr, s.Stdout = append(s.Stderr, s.Stdout...), nil
	return s
}
