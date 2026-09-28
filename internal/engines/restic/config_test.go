package restic

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
)

var hostKey = engines.HostKey{Type: "ssh-ed25519", Key: "AAAAC3NzaC1lZDI1NTE5AAAAIPRtQTiPzA3WeraYnXPR8GZeSrefhtPVznLCHWxGRDqS"}

func dests() map[string]engines.Destination {
	return map[string]engines.Destination{
		"local": {ID: 1, Kind: engines.Local, Engine: engines.Restic, Encryption: engines.EncryptionRestic, Target: "/mnt/disks/unas/restic"},
		"s3": {ID: 2, Kind: engines.S3, Engine: engines.Restic, Encryption: engines.EncryptionRestic,
			Remote: engines.Remote{S3: &engines.S3Remote{Provider: "Minio", Endpoint: "https://minio:9000", Region: "us-east-1", Bucket: "media", Prefix: "restic/x", ForcePathStyle: true}}},
		"b2": {ID: 3, Kind: engines.B2, Engine: engines.Restic, Encryption: engines.EncryptionRestic,
			Remote: engines.Remote{B2: &engines.B2Remote{Bucket: "media-b2"}}},
		"sftp": {ID: 4, Kind: engines.SFTP, Engine: engines.Restic, Encryption: engines.EncryptionRestic,
			Remote: engines.Remote{SFTP: &engines.SFTPRemote{Host: "nas", Port: 22, User: "u", Path: "restic", HostKeys: []engines.HostKey{hostKey}}}},
	}
}

func secrets() engines.Secrets {
	return engines.Secrets{
		Credentials: engines.Credentials{AccessKeyID: "AKIA1", SecretAccessKey: "s3-secret-1", KeyID: "kid", ApplicationKey: "b2-app-key-1",
			Password: "sftp-password-1"},
		Encryption: engines.EncryptionSecret{ResticPassword: "restic-password-1"},
		Obscured:   map[string]string{engines.FieldPassword: "obscured-sftp-password"},
	}
}

func TestRepository(t *testing.T) {
	want := map[string]string{
		"local": "/mnt/disks/unas/restic",
		"s3":    "rclone:BKDEST:media/restic/x",
		"b2":    "rclone:BKDEST:media-b2",
		"sftp":  "rclone:BKDEST:restic",
	}
	for name, d := range dests() {
		if got, err := Repository(d); err != nil || got != want[name] {
			t.Errorf("%s: Repository = %q, %v", name, got, err)
		}
	}
	for _, d := range []engines.Destination{
		{Kind: engines.Local, Target: "relative/repo"},
		{Kind: engines.Local, Target: "/a/../b"},
		{Kind: engines.S3},
		{Kind: "ftp"},
	} {
		if got, err := Repository(d); err == nil {
			t.Errorf("Repository(%+v) = %q", d, got)
		}
	}
}

// TestEnv: RESTIC_* plus, for remote kinds, rclone.Env's BKDEST variables and RCLONE_BWLIMIT,
// never BKCRYPT; the result passes proc.Validate for restic.
func TestEnv(t *testing.T) {
	base := []string{"RESTIC_REPOSITORY", "RESTIC_PASSWORD_FILE", "RESTIC_CACHE_DIR", "RESTIC_PROGRESS_FPS"}
	dest := func(opts ...string) []string {
		out := []string{"RCLONE_CONFIG", "RCLONE_BWLIMIT"}
		for _, o := range opts {
			out = append(out, "RCLONE_CONFIG_BKDEST_"+o)
		}
		return out
	}
	want := map[string][]string{
		"local": base,
		"s3":    append(slices.Clone(base), dest("TYPE", "PROVIDER", "ENDPOINT", "REGION", "ACCESS_KEY_ID", "SECRET_ACCESS_KEY", "ENV_AUTH", "NO_CHECK_BUCKET", "FORCE_PATH_STYLE")...),
		"b2":    append(slices.Clone(base), dest("TYPE", "ACCOUNT", "KEY", "HARD_DELETE")...),
		"sftp":  append(slices.Clone(base), dest("TYPE", "HOST", "PORT", "USER", "PASS", "KEY_USE_AGENT", "ASK_PASSWORD", "KNOWN_HOSTS_FILE")...),
	}
	for name, d := range dests() {
		t.Run(name, func(t *testing.T) {
			// A crypt setting must never reach restic's environment.
			d.Encryption = engines.EncryptionCrypt
			s := secrets()
			s.Obscured[engines.FieldCryptPassword] = "obscured-crypt"
			env, err := Env(d, s, "/dev/shm/bunkarr-run/9-a/password", CacheDir("/config", d.ID), "Mon-08:00,1024k:off Mon-23:00,off:off",
				"/dev/shm/bunkarr-run/9-a/known_hosts", "")
			if err != nil {
				t.Fatal(err)
			}
			var keys []string
			for k := range env {
				keys = append(keys, k)
				if strings.Contains(k, "BKCRYPT") {
					t.Errorf("restic environment has %s", k)
				}
			}
			slices.Sort(keys)
			w := slices.Sorted(slices.Values(want[name]))
			if !slices.Equal(keys, w) {
				t.Errorf("keys %v\nwant %v", keys, w)
			}
			if env["RESTIC_CACHE_DIR"] != "/config/cache/restic/"+strconv.FormatInt(d.ID, 10) || env["RESTIC_PROGRESS_FPS"] != "0.5" {
				t.Errorf("cache %q fps %q", env["RESTIC_CACHE_DIR"], env["RESTIC_PROGRESS_FPS"])
			}
			if err := proc.Validate(proc.Cmd{Binary: proc.Restic, Args: []string{"cat", "config", "--json", "--no-lock"}, Env: env}); err != nil {
				t.Errorf("proc.Validate: %v", err)
			}
		})
	}
	d := dests()["s3"]
	caRemote := *d.Remote.S3
	caRemote.CACert = "-----BEGIN CERTIFICATE-----"
	d.Remote.S3 = &caRemote
	env, err := Env(d, secrets(), "/p/password", "/c", "", "", "/dev/shm/bunkarr-run/9-a/ca.pem")
	if err != nil || env["RCLONE_CA_CERT"] != "/dev/shm/bunkarr-run/9-a/ca.pem" {
		t.Errorf("CA certificate: %v %q", err, env["RCLONE_CA_CERT"])
	}
	for _, bad := range []struct {
		name                string
		passwordFile, cache string
		s                   engines.Secrets
	}{
		{"relative password file", "password", "/c", secrets()},
		{"relative cache", "/p", "cache", secrets()},
		{"no password", "/p", "/c", engines.Secrets{}},
	} {
		if _, err := Env(dests()["local"], bad.s, bad.passwordFile, bad.cache, "", "", ""); err == nil {
			t.Errorf("%s accepted", bad.name)
		}
	}
	noKeys := dests()["sftp"]
	r := *noKeys.Remote.SFTP
	r.HostKeys = nil
	noKeys.Remote.SFTP = &r
	if _, err := Env(noKeys, secrets(), "/p", "/c", "", "/k", ""); err == nil {
		t.Error("an sftp repository without pinned host keys")
	}
}
