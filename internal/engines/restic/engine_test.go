package restic

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/enginetest"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
)

var repoConfig = []string{`{"version":2,"id":"7c153421d95efe8bacf542bdffa1e3aeabb12bf77f5f23692860428dc9c9d987","chunker_polynomial":"3e6d"}`}

func TestTestRemote(t *testing.T) {
	probe := enginetest.Args("lsf", "--max-depth", "1", "BKDEST:restic/bk", "--use-json-log")
	for _, tc := range []struct {
		name      string
		marker    string
		lsf       enginetest.Script
		cat       *enginetest.Script
		want      engines.TestResult
		wantInMsg string
	}{
		{name: "missing", lsf: enginetest.Script{}, cat: ptr(enginetest.FixtureScript(t, "restic", "repo-missing.stderr.jsonl")),
			want: engines.TestResult{OK: true, Reachable: true, Repository: engines.RepositoryMissing}},
		{name: "exists", lsf: enginetest.Script{Stdout: []string{"config", "data/", "index/", "keys/"}}, cat: &enginetest.Script{Stdout: repoConfig},
			want: engines.TestResult{OK: true, Reachable: true, Entries: 4, Repository: engines.RepositoryExists,
				ID: "7c153421d95efe8bacf542bdffa1e3aeabb12bf77f5f23692860428dc9c9d987"}},
		{name: "wrong password", lsf: enginetest.Script{Stdout: []string{"config"}}, cat: ptr(enginetest.FixtureScript(t, "restic", "wrong-password.stderr.jsonl")),
			want: engines.TestResult{Reachable: true, Entries: 1, Repository: engines.RepositoryWrongPassword}, wantInMsg: "wrong repository password"},
		{name: "locked", lsf: enginetest.Script{}, cat: ptr(enginetest.FixtureScript(t, "restic", "locked.stderr.jsonl")),
			want: engines.TestResult{OK: true, Reachable: true, Repository: engines.RepositoryLocked}, wantInMsg: "locked"},
		{name: "bucket not found", lsf: enginetest.Script{Exit: 3, Stderr: enginetest.Fixture(t, "rclone", "exit3-bucket-not-found.txt").Lines},
			want: engines.TestResult{}, wantInMsg: "bucket or path not found"},
		{name: "another repository", marker: "restic:0000", lsf: enginetest.Script{}, cat: &enginetest.Script{Stdout: repoConfig},
			want:      engines.TestResult{Reachable: true, Repository: engines.RepositoryExists, ID: "7c153421d95efe8bacf542bdffa1e3aeabb12bf77f5f23692860428dc9c9d987"},
			wantInMsg: "another repository"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, f := newTestDriver(t)
			dest, sec := s3Repo()
			dest.MarkerID = tc.marker
			f.Expect(proc.Rclone, probe, tc.lsf)
			if tc.cat != nil {
				f.Expect(proc.Restic, enginetest.Prefix("cat", "config", "--json", "--no-lock"), *tc.cat)
			}
			got, err := d.Test(context.Background(), dest, sec)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(got.Message, tc.wantInMsg) {
				t.Errorf("message %q, want %q", got.Message, tc.wantInMsg)
			}
			if got.EngineVersion != "0.18.1" {
				t.Errorf("engine version %q", got.EngineVersion)
			}
			got.Message, got.EngineVersion = "", ""
			if g, w := mustJSON(t, got), mustJSON(t, tc.want); g != w {
				t.Errorf("result\n got %s\nwant %s", g, w)
			}
			for _, c := range f.Calls() {
				if c.Cmd.Budget != TestBudget || c.Cmd.RetryBudget != TestRetryBudget {
					t.Errorf("%s: budget %s, retry %s", c, c.Cmd.Budget, c.Cmd.RetryBudget)
				}
				if c.Binary == proc.Restic && !strings.HasPrefix(c.Env["RESTIC_CACHE_DIR"], c.Dir.DataDir()) && dest.ID == 0 {
					t.Errorf("a test without an id uses the cache %s", c.Env["RESTIC_CACHE_DIR"])
				}
			}
		})
	}
	t.Run("sftp without host keys", func(t *testing.T) {
		d, f := newTestDriver(t)
		dest := engines.Destination{Kind: engines.SFTP, Engine: engines.Restic, EngineTag: testEngineTag,
			Remote: engines.Remote{SFTP: &engines.SFTPRemote{Host: "h", User: "u", Path: "p"}}}
		sec := engines.Secrets{Encryption: engines.EncryptionSecret{ResticPassword: testPassword}, Credentials: engines.Credentials{Password: "sftp-password-1"}}
		res, err := d.Test(context.Background(), dest, sec)
		if err != nil || res.OK || !strings.Contains(res.Message, "host keys") {
			t.Fatalf("%+v %v", res, err)
		}
		if _, err := d.Create(context.Background(), dest, sec, false); err == nil {
			t.Fatal("create without host keys")
		}
		if len(f.Calls()) != 0 {
			t.Fatal("a command ran against an unverified host")
		}
	})
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestTestLocal(t *testing.T) {
	d, f := newTestDriver(t)
	dest, sec := localRepo(t)
	for _, n := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(dest.Target, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f.Expect(proc.Restic, enginetest.Prefix("cat", "config"), enginetest.FixtureScript(t, "restic", "repo-missing.stderr.jsonl"))
	res, err := d.Test(context.Background(), dest, sec)
	if err != nil || !res.OK || !res.Reachable || res.Entries != 2 || res.FreeBytes == nil || res.Repository != engines.RepositoryMissing {
		t.Fatalf("%+v %v", res, err)
	}
	gone := dest
	gone.Target = filepath.Join(dest.Target, "missing")
	res, err = d.Test(context.Background(), gone, sec)
	if err != nil || res.Reachable || !strings.Contains(res.Message, "not found") {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestCreate(t *testing.T) {
	t.Run("new", func(t *testing.T) {
		d, f := newTestDriver(t)
		dest, sec := s3Repo()
		f.Expect(proc.Restic, enginetest.Prefix("cat", "config"), enginetest.FixtureScript(t, "restic", "repo-missing.stderr.jsonl"))
		f.Expect(proc.Restic, enginetest.Prefix("init", "--json", "--repository-version", "2"), enginetest.FixtureScript(t, "restic", "init.json"))
		f.Expect(proc.Restic, enginetest.Prefix("cat", "config"), enginetest.Script{Stdout: repoConfig})
		res, err := d.Create(context.Background(), dest, sec, false)
		if err != nil || !res.Initialized || res.MarkerID != "restic:7c153421d95efe8bacf542bdffa1e3aeabb12bf77f5f23692860428dc9c9d987" {
			t.Fatalf("%+v %v", res, err)
		}
		if c := f.CallsOf(proc.Restic, "init")[0]; c.Env["RESTIC_CACHE_DIR"] != d.CacheRoot+"/11" || c.Cmd.Budget != TestBudget {
			t.Fatalf("init cache %s budget %s", c.Env["RESTIC_CACHE_DIR"], c.Cmd.Budget)
		}
	})
	t.Run("init succeeded, read back failed", func(t *testing.T) {
		d, f := newTestDriver(t)
		dest, sec := s3Repo()
		f.Expect(proc.Restic, enginetest.Prefix("cat", "config"), enginetest.FixtureScript(t, "restic", "repo-missing.stderr.jsonl"))
		f.Expect(proc.Restic, enginetest.Prefix("init"), enginetest.FixtureScript(t, "restic", "init.json"))
		f.Expect(proc.Restic, enginetest.Prefix("cat", "config"), logFatal("connection reset"))
		res, err := d.Create(context.Background(), dest, sec, false)
		if err == nil || !res.Initialized || res.MarkerID != "" {
			t.Fatalf("%+v %v", res, err)
		}
	})
	t.Run("exists without attach", func(t *testing.T) {
		d, f := newTestDriver(t)
		dest, sec := s3Repo()
		f.Expect(proc.Restic, enginetest.Prefix("cat", "config"), enginetest.Script{Stdout: repoConfig})
		if _, err := d.Create(context.Background(), dest, sec, false); err == nil || !strings.Contains(err.Error(), "attach") {
			t.Fatalf("%v", err)
		}
		if len(f.CallsOf(proc.Restic, "init")) != 0 {
			t.Fatal("init over an existing repository")
		}
	})
	t.Run("init refused", func(t *testing.T) {
		d, f := newTestDriver(t)
		dest, sec := s3Repo()
		f.Expect(proc.Restic, enginetest.Prefix("cat", "config"), enginetest.FixtureScript(t, "restic", "repo-missing.stderr.jsonl"))
		f.Expect(proc.Restic, enginetest.Prefix("init"), enginetest.FixtureScript(t, "restic", "init-already-initialized.stderr.jsonl"))
		res, err := d.Create(context.Background(), dest, sec, false)
		if !errors.Is(err, ErrFailed) || res.Initialized {
			t.Fatalf("%+v %v", res, err)
		}
	})
	t.Run("attach", func(t *testing.T) {
		d, f := newTestDriver(t)
		d.Now = func() time.Time { return time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC) }
		dest, sec := s3Repo()
		recent := Snapshot{ID: id(1), Time: time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC), Tags: mediaTags(t, otherTag, 1, 1, 1)}
		old := Snapshot{ID: id(2), Time: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), Tags: mediaTags(t, otherTag, 1, 1, 1)}
		ours := Snapshot{ID: id(3), Time: time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC), Tags: mediaTags(t, testEngineTag, 1, 1, 1)}
		user := Snapshot{ID: id(4), Time: time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC), Tags: []string{"manual"}}
		b, _ := json.Marshal([]Snapshot{recent, old, ours, user})
		f.Expect(proc.Restic, enginetest.Prefix("cat", "config"), enginetest.Script{Stdout: repoConfig})
		f.Expect(proc.Restic, enginetest.Prefix("snapshots", "--json", "--no-lock", "-o"), enginetest.Script{Stdout: []string{string(b)}})
		res, err := d.Create(context.Background(), dest, sec, true)
		if err != nil || res.Initialized || res.MarkerID == "" || len(res.Warnings) != 1 ||
			!strings.Contains(res.Warnings[0], "another Bunkarr may be writing to this repository (1 snapshots") {
			t.Fatalf("%+v %v", res, err)
		}
	})
	t.Run("attach a missing repository", func(t *testing.T) {
		d, f := newTestDriver(t)
		dest, sec := s3Repo()
		f.Expect(proc.Restic, enginetest.Prefix("cat", "config"), enginetest.FixtureScript(t, "restic", "repo-missing.stderr.jsonl"))
		if _, err := d.Create(context.Background(), dest, sec, true); !errors.Is(err, engines.ErrRepositoryMissing) {
			t.Fatalf("%v", err)
		}
	})
}

func TestCapabilities(t *testing.T) {
	c := Capabilities()
	if !c.Hardlinks || c.CaseInsensitive || c.InvalidChars != "" || c.MtimeGranularityNs != 1 || c.UnstableInodes || !c.EnforcesModes {
		t.Fatalf("%+v", c)
	}
}

func TestConnectRefuses(t *testing.T) {
	d, _ := newTestDriver(t)
	dest, sec := s3Repo()
	if _, err := d.Connect(dest, engines.Secrets{}, engines.Runtime{}); err == nil {
		t.Fatal("no password accepted")
	}
	noRclone := *d
	noRclone.RclonePath = ""
	if _, err := noRclone.Connect(dest, sec, engines.Runtime{}); err == nil {
		t.Fatal("a remote repository without an rclone program")
	}
	local, lsec := localRepo(t)
	if _, err := noRclone.Connect(local, lsec, engines.Runtime{}); err != nil {
		t.Fatalf("a local repository needs no rclone: %v", err)
	}
	rel := local
	rel.Target = "relative/path"
	if _, err := d.Connect(rel, lsec, engines.Runtime{}); err == nil {
		t.Fatal("a relative local repository")
	}
}
