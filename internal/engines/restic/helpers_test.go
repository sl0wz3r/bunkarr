package restic

import (
	"fmt"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/enginetest"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
	"github.com/sl0wz3r/bunkarr/internal/engines/rclone"
)

const (
	testEngineTag = "0123456789abcdef0123456789abcdef"
	otherTag      = "fedcba9876543210fedcba9876543210"
	testRclone    = "/usr/bin/rclone"
	testPassword  = "restic-repository-password-0123"
)

// newTestDriver returns a driver on a FakeRunner with real run directories.
func newTestDriver(t *testing.T) (*Driver, *enginetest.FakeRunner) {
	t.Helper()
	f := enginetest.NewFakeRunner(t)
	t.Cleanup(func() { assertResticArgv(t, f) })
	dirs := enginetest.RunDirs(t)
	return &Driver{Runner: f, RunDirs: dirs, RclonePath: testRclone, CacheRoot: t.TempDir(), Version: "0.18.1",
		Rclone: &rclone.Driver{Runner: f, RunDirs: dirs, RetryWait: -1}}, f
}

func connect(t *testing.T, d *Driver, dest engines.Destination, s engines.Secrets) *Repo {
	t.Helper()
	r, err := d.Connect(dest, s, engines.Runtime{JobID: 9})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// s3Repo is a restic destination on s3 (through the rclone backend).
func s3Repo() (engines.Destination, engines.Secrets) {
	d := engines.Destination{ID: 11, Name: "b2-standin", Engine: engines.Restic, Kind: engines.S3, Encryption: engines.EncryptionRestic,
		MarkerID: "restic:7c153421d95efe8bacf542bdffa1e3aeabb12bf77f5f23692860428dc9c9d987", EngineTag: testEngineTag,
		Transfers: 6, PackSizeMiB: 64,
		Remote: engines.Remote{S3: &engines.S3Remote{Provider: "Minio", Endpoint: "https://minio.example:9000", Region: "us-east-1",
			Bucket: "restic", Prefix: "bk", ForcePathStyle: true}}}
	s := engines.Secrets{Credentials: engines.Credentials{AccessKeyID: "AKIARESTICKEY001", SecretAccessKey: "restic-s3-secret-0123456789"},
		Encryption: engines.EncryptionSecret{ResticPassword: testPassword}}
	return d, s
}

// localRepo is a restic destination on a local path.
func localRepo(t *testing.T) (engines.Destination, engines.Secrets) {
	d := engines.Destination{ID: 12, Name: "unas-restic", Engine: engines.Restic, Kind: engines.Local, Encryption: engines.EncryptionRestic,
		Target: t.TempDir(), MarkerID: "restic:7c153421d95efe8bacf542bdffa1e3aeabb12bf77f5f23692860428dc9c9d987", EngineTag: testEngineTag,
		PackSizeMiB: 16}
	return d, engines.Secrets{Encryption: engines.EncryptionSecret{ResticPassword: testPassword}}
}

// assertResticArgv checks every recorded restic command against the never-lists of S22 and S24.
func assertResticArgv(t *testing.T, f *enginetest.FakeRunner) {
	t.Helper()
	for _, c := range f.Calls() {
		if c.Binary != proc.Restic {
			continue
		}
		for _, a := range c.Args {
			for _, bad := range []string{"--keep-", "--unsafe", "--repack", "--insecure", "--password-command", "--dry-run", "-v"} {
				if a == bad || (len(bad) > 2 && len(a) >= len(bad) && a[:len(bad)] == bad) {
					t.Errorf("forbidden restic argument %q in %s", a, c)
				}
			}
		}
		if c.Subcommand() == "unlock" && c.Has("--remove-all") {
			t.Errorf("unlock --remove-all in %s", c)
		}
	}
}

// id returns a full snapshot id for n.
func id(n int) string { return fmt.Sprintf("%064x", n) }

// day is 02:00 UTC on a day of the pinned table's series.
func day(month time.Month, d int) time.Time { return time.Date(2026, month, d, 2, 0, 0, 0, time.UTC) }

func mediaTags(t *testing.T, engineTag string, source int64, job int64, batch int) []string {
	t.Helper()
	tags, err := Tags(TagInput{EngineTag: engineTag, Kind: engines.VersionMedia, JobID: job, SourceID: source, Batch: batch})
	if err != nil {
		t.Fatal(err)
	}
	return tags
}
