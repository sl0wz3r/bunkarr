package proc_test

import (
	"errors"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
	"github.com/sl0wz3r/bunkarr/internal/engines/rclone"
	"github.com/sl0wz3r/bunkarr/internal/logging"
)

// TestValidateFolderNamedLikeAccessKey: with the wiring of the drivers (Redact = Secrets.Values,
// the environment of rclone.Env, the destination's values registered while it is stored), a
// source whose destination folder, or a destination whose bucket or prefix, is named like the
// destination's own S3 access key ID or B2 keyId gets its commands refused before anything runs.
// Letting them run made jobs decide on redacted listings: the lsjson of the root prints
// "[REDACTED]/a.mkv", so a resumed upload overwrote the live previous version instead of
// retaining it, a vanished file's record was dropped, every move failed and expired versions
// were forgotten. Commands that name neither run.
func TestValidateFolderNamedLikeAccessKey(t *testing.T) {
	const keyID = "photos-backup"
	s3 := func(bucket string, enc engines.EncryptionMode) engines.Destination {
		return engines.Destination{ID: 41, Name: "minio", Engine: engines.Rclone, Kind: engines.S3, Encryption: enc,
			Remote: engines.Remote{S3: &engines.S3Remote{Provider: "Minio", Endpoint: "http://minio:9000", Region: "us-east-1",
				Bucket: bucket, Prefix: "bunkarr", ForcePathStyle: true}}}
	}
	s3Sec := engines.Secrets{Credentials: engines.Credentials{AccessKeyID: keyID, SecretAccessKey: "wJalrXUtnFEMI-K7MDENG-bPxRfiCY"},
		Encryption: engines.EncryptionSecret{CryptPassword: "crypt-password-0123456789", CryptPassword2: "crypt-salt-0123456789"},
		Obscured:   map[string]string{engines.FieldCryptPassword: "obscured-crypt-password-01", engines.FieldCryptPassword2: "obscured-crypt-salt-01"}}
	plainSec := engines.Secrets{Credentials: s3Sec.Credentials}
	const b2KeyID = "0012345abcdef0000000001"
	b2 := func(prefix string) engines.Destination {
		return engines.Destination{ID: 42, Name: "b2", Engine: engines.Rclone, Kind: engines.B2, Encryption: engines.EncryptionNone,
			Remote: engines.Remote{B2: &engines.B2Remote{Bucket: "media", Prefix: prefix}}}
	}
	b2Sec := engines.Secrets{Credentials: engines.Credentials{KeyID: b2KeyID, ApplicationKey: "K001abcdefghijklmnopqrstuvwxyz0"}}

	for _, tc := range []struct {
		name     string
		d        engines.Destination
		s        engines.Secrets
		refused  []string // the destination-side argument of a copy
		allowed  []string
		rootName bool // the root itself holds the identifier: every command names it
	}{
		{name: "crypt, folder", d: s3("media", engines.EncryptionCrypt), s: s3Sec,
			refused: []string{"BKCRYPT:" + keyID, "BKCRYPT:" + keyID + "/a.mkv", "BKCRYPT:movies/" + keyID + ".mkv"},
			allowed: []string{"BKCRYPT:movies", "BKCRYPT:movies/a.mkv"}},
		{name: "plain, folder", d: s3("media", engines.EncryptionNone), s: plainSec,
			refused: []string{"BKDEST:media/bunkarr/" + keyID, "BKDEST:media/bunkarr/" + keyID + "/a.mkv"},
			allowed: []string{"BKDEST:media/bunkarr/movies"}},
		{name: "plain, bucket", d: s3(keyID, engines.EncryptionNone), s: plainSec, rootName: true},
		{name: "b2, prefix", d: b2(b2KeyID), s: b2Sec, rootName: true},
		{name: "b2, folder", d: b2("bunkarr"), s: b2Sec,
			refused: []string{"BKDEST:media/bunkarr/" + b2KeyID},
			allowed: []string{"BKDEST:media/bunkarr/movies"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logging.SetSecrets("destination:proc-identifier-test", tc.s.Values()...)
			t.Cleanup(func() { logging.SetSecrets("destination:proc-identifier-test") })
			env, err := rclone.Env(rclone.EnvInput{Dest: tc.d, Secrets: tc.s})
			if err != nil {
				t.Fatal(err)
			}
			cmd := func(args ...string) proc.Cmd {
				return proc.Cmd{Binary: proc.Rclone, Args: args, Env: env, Redact: tc.s.Values()}
			}
			roots := []proc.Cmd{
				cmd("lsjson", "-R", "--files-only", rclone.Root(tc.d)),
				cmd("backend", "cleanup", rclone.StorageRoot(tc.d)),
			}
			for _, c := range roots {
				err := proc.Validate(c)
				if tc.rootName != errors.Is(err, proc.ErrNotAllowed) {
					t.Errorf("Validate(%v) = %v", c.Args, err)
				}
			}
			for _, dst := range tc.refused {
				if err := proc.Validate(cmd("copy", "/mnt/user/movies", dst)); !errors.Is(err, proc.ErrNotAllowed) {
					t.Errorf("copy into %s: %v, want a refusal", dst, err)
				}
				// Why: the listing of the root names the folder, and reads back redacted.
				rel := dst[len(rclone.Root(tc.d)):]
				if line := `{"Path":"` + rel + `","Size":1}`; proc.RedactLine(line, false, tc.s.Values()) == line {
					t.Errorf("%s: the listing line %s is not redacted, so the refusal is not needed", dst, line)
				}
			}
			for _, dst := range tc.allowed {
				if err := proc.Validate(cmd("copy", "/mnt/user/movies", dst)); err != nil {
					t.Errorf("copy into %s: %v", dst, err)
				}
			}
			// The secret key stays refused whatever the names.
			key := tc.s.Credentials.SecretAccessKey + tc.s.Credentials.ApplicationKey
			if err := proc.Validate(cmd("lsf", rclone.Root(tc.d)+"/"+key)); !errors.Is(err, proc.ErrNotAllowed) {
				t.Errorf("a secret key in an argument: %v", err)
			}
		})
	}
}
