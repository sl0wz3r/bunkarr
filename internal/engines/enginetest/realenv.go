//go:build enginebin

package enginetest

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/sl0wz3r/bunkarr/internal/engines"
)

// Environment variables of the real-binary tests, set by docker/test-engines.sh (make
// test-engines, docs/design/phase4.md §14.4).
const (
	EnvRestic            = "BUNKARR_ENGINEBIN_RESTIC"              // restic path (default: PATH lookup)
	EnvRclone            = "BUNKARR_ENGINEBIN_RCLONE"              // rclone path (default: PATH lookup)
	EnvS3Endpoint        = "BUNKARR_ENGINEBIN_S3_ENDPOINT"         // MinIO endpoint URL
	EnvS3KeyID           = "BUNKARR_ENGINEBIN_S3_KEY_ID"           // access key id
	EnvS3Secret          = "BUNKARR_ENGINEBIN_S3_SECRET"           // secret access key
	EnvS3Bucket          = "BUNKARR_ENGINEBIN_S3_BUCKET"           // an existing bucket
	EnvSFTPHost          = "BUNKARR_ENGINEBIN_SFTP_HOST"           // SFTP server host
	EnvSFTPPort          = "BUNKARR_ENGINEBIN_SFTP_PORT"           // its port (default 22)
	EnvSFTPUser          = "BUNKARR_ENGINEBIN_SFTP_USER"           // login
	EnvSFTPKey           = "BUNKARR_ENGINEBIN_SFTP_KEY"            // path of the client's private key (PEM)
	EnvSFTPKeyPassphrase = "BUNKARR_ENGINEBIN_SFTP_KEY_PASSPHRASE" // the key's passphrase ("" = none)
	EnvSFTPPassword      = "BUNKARR_ENGINEBIN_SFTP_PASSWORD"       // the login's password ("" = key only)
	EnvSFTPHostKeys      = "BUNKARR_ENGINEBIN_SFTP_HOST_KEYS"      // path of the server's public host keys (.pub lines)
	EnvSFTPPath          = "BUNKARR_ENGINEBIN_SFTP_PATH"           // a writable directory (default "upload")
)

// RealEnvironment is the real-binary test environment: the engine programs, a MinIO bucket and an
// SFTP server with pinned host keys.
type RealEnvironment struct {
	ResticPath, RclonePath string

	S3Endpoint, S3KeyID, S3Secret, S3Bucket string

	SFTPHost          string
	SFTPPort          int
	SFTPUser          string
	SFTPKey           string // PEM
	SFTPKeyPassphrase string
	SFTPPassword      string
	SFTPHostKeys      []engines.HostKey
	SFTPPath          string
}

// RealEnv reads the environment of the real-binary tests and skips t when it is not set (the
// tests run inside make test-engines).
func RealEnv(t testing.TB) RealEnvironment {
	t.Helper()
	if os.Getenv(EnvS3Endpoint) == "" {
		t.Skip("real engine binaries: run make test-engines (" + EnvS3Endpoint + " is not set)")
	}
	e := RealEnvironment{
		ResticPath:        os.Getenv(EnvRestic),
		RclonePath:        os.Getenv(EnvRclone),
		S3Endpoint:        os.Getenv(EnvS3Endpoint),
		S3KeyID:           os.Getenv(EnvS3KeyID),
		S3Secret:          os.Getenv(EnvS3Secret),
		S3Bucket:          os.Getenv(EnvS3Bucket),
		SFTPHost:          os.Getenv(EnvSFTPHost),
		SFTPPort:          22,
		SFTPUser:          os.Getenv(EnvSFTPUser),
		SFTPKeyPassphrase: os.Getenv(EnvSFTPKeyPassphrase),
		SFTPPassword:      os.Getenv(EnvSFTPPassword),
		SFTPPath:          os.Getenv(EnvSFTPPath),
	}
	for _, p := range []*string{&e.ResticPath, &e.RclonePath} {
		if *p == "" {
			name := "restic"
			if p == &e.RclonePath {
				name = "rclone"
			}
			found, err := exec.LookPath(name)
			if err != nil {
				t.Fatalf("enginetest: %s not found: %v", name, err)
			}
			*p = found
		}
	}
	if v := os.Getenv(EnvSFTPPort); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("enginetest: %s=%q", EnvSFTPPort, v)
		}
		e.SFTPPort = n
	}
	if e.SFTPPath == "" {
		e.SFTPPath = "upload"
	}
	if p := os.Getenv(EnvSFTPKey); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("enginetest: %v", err)
		}
		e.SFTPKey = string(b)
	}
	if p := os.Getenv(EnvSFTPHostKeys); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("enginetest: %v", err)
		}
		sc := bufio.NewScanner(bytes.NewReader(b))
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
			if err != nil {
				t.Fatalf("enginetest: host key %q: %v", line, err)
			}
			e.SFTPHostKeys = append(e.SFTPHostKeys, engines.HostKey{Type: pk.Type(), Key: base64.StdEncoding.EncodeToString(pk.Marshal())})
		}
	}
	return e
}

// S3 returns an s3 destination on the bucket under prefix with its credentials. With crypt it is
// encrypted with throwaway crypt passwords; obscure (rclone.ObscureRandom) fills Secrets.Obscured.
func (e RealEnvironment) S3(id int64, prefix string, crypt bool, obscure func(string) string) (engines.Destination, engines.Secrets) {
	d := engines.Destination{ID: id, Name: "enginebin-s3", Engine: engines.Rclone, Kind: engines.S3, Encryption: engines.EncryptionNone,
		Remote: engines.Remote{S3: &engines.S3Remote{Provider: "Minio", Endpoint: e.S3Endpoint, Region: "us-east-1",
			Bucket: e.S3Bucket, Prefix: prefix, ForcePathStyle: true}}}
	s := engines.Secrets{Credentials: engines.Credentials{AccessKeyID: e.S3KeyID, SecretAccessKey: e.S3Secret}, Obscured: map[string]string{}}
	if crypt {
		d.Encryption = engines.EncryptionCrypt
		s.Encryption = engines.EncryptionSecret{CryptPassword: "enginebin-crypt-password-" + prefix, CryptPassword2: "enginebin-crypt-salt-" + prefix}
		s.Obscured[engines.FieldCryptPassword] = obscure(s.Encryption.CryptPassword)
		s.Obscured[engines.FieldCryptPassword2] = obscure(s.Encryption.CryptPassword2)
	}
	return d, s
}

// SFTP returns an sftp destination under the writable path with the pinned host keys, logging in
// with the private key (and its passphrase), or with the password when password is set.
func (e RealEnvironment) SFTP(id int64, sub string, password bool, obscure func(string) string) (engines.Destination, engines.Secrets) {
	p := e.SFTPPath
	if sub != "" {
		p += "/" + sub
	}
	d := engines.Destination{ID: id, Name: "enginebin-sftp", Engine: engines.Rclone, Kind: engines.SFTP, Encryption: engines.EncryptionNone,
		Remote: engines.Remote{SFTP: &engines.SFTPRemote{Host: e.SFTPHost, Port: e.SFTPPort, User: e.SFTPUser, Path: p, HostKeys: e.SFTPHostKeys}}}
	s := engines.Secrets{Obscured: map[string]string{}}
	if password {
		s.Credentials.Password = e.SFTPPassword
		s.Obscured[engines.FieldPassword] = obscure(e.SFTPPassword)
	} else {
		s.Credentials.PrivateKey = e.SFTPKey
		if e.SFTPKeyPassphrase != "" {
			s.Credentials.PrivateKeyPassphrase = e.SFTPKeyPassphrase
			s.Obscured[engines.FieldPrivateKeyPassphrase] = obscure(e.SFTPKeyPassphrase)
		}
	}
	return d, s
}
