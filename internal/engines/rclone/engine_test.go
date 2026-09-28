package rclone

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/enginetest"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
)

// markerStore is a fake remote that only knows the marker: cat, rcat and deletefile of
// .bunkarr/destination.json, and a top-level listing of n entries.
type markerStore struct {
	mu      sync.Mutex
	content string
	entries int
	// failCat makes that many next cats exit 1.
	failCat int
	deleted int
}

func (m *markerStore) install(f *enginetest.FakeRunner) {
	f.Handle(proc.Rclone, "lsf", func(*enginetest.Call) enginetest.Script {
		m.mu.Lock()
		defer m.mu.Unlock()
		var out []string
		for i := range m.entries {
			out = append(out, strings.Repeat("x", i+1))
		}
		return enginetest.Script{Stdout: out}
	})
	f.Handle(proc.Rclone, "cat", func(*enginetest.Call) enginetest.Script {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.failCat > 0 {
			m.failCat--
			return enginetest.Script{Exit: 1, Stderr: []string{`{"time":"2026-09-27T13:53:10Z","level":"error","msg":"connection reset"}`}}
		}
		if m.content == "" {
			return enginetest.Script{Exit: 3}
		}
		return enginetest.Script{Stdout: strings.Split(strings.TrimSuffix(m.content, "\n"), "\n")}
	})
	f.Handle(proc.Rclone, "rcat", func(*enginetest.Call) enginetest.Script {
		return enginetest.Script{Hook: func(c *enginetest.Call) {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.content = string(c.Stdin)
		}}
	})
	f.Handle(proc.Rclone, "deletefile", func(*enginetest.Call) enginetest.Script {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.content = ""
		m.deleted++
		return enginetest.Script{}
	})
}

func TestTestResults(t *testing.T) {
	ours := `{"id":"5f3c1a2e-7d4b-4c7a-9f1e-2b3c4d5e6f70","name":"offsite"}`
	for _, tc := range []struct {
		name      string
		crypt     bool
		markerID  string
		expect    func(*enginetest.FakeRunner)
		want      engines.TestResult
		wantInMsg string
	}{
		{name: "empty bucket, no marker", crypt: true, expect: func(f *enginetest.FakeRunner) {
			f.Expect(proc.Rclone, enginetest.Args("lsf", "--max-depth", "1", "BKDEST:media/bk", "--use-json-log"), enginetest.Script{})
			f.Expect(proc.Rclone, enginetest.Prefix("cat"), enginetest.Script{})
		}, want: engines.TestResult{OK: true, Reachable: true, Marker: engines.MarkerMissing}},
		{name: "bucket not found", expect: func(f *enginetest.FakeRunner) {
			f.Expect(proc.Rclone, enginetest.Prefix("lsf"), logScript(t, "exit3-bucket-not-found.txt"))
		}, want: engines.TestResult{}, wantInMsg: "bucket or path not found"},
		{name: "credentials", expect: func(f *enginetest.FakeRunner) {
			f.Expect(proc.Rclone, enginetest.Prefix("lsf"), logScript(t, "exit1-bad-credentials.txt"))
		}, want: engines.TestResult{}, wantInMsg: "credentials were refused"},
		{name: "another crypt remote", crypt: true, expect: func(f *enginetest.FakeRunner) {
			f.Expect(proc.Rclone, enginetest.Args("lsf", "--max-depth", "1", "BKDEST:media/bk", "--use-json-log"),
				enginetest.Script{Stdout: []string{"a/", "b/", "c"}})
			f.Expect(proc.Rclone, enginetest.Args("lsf", "--max-depth", "1", "BKCRYPT:", "--use-json-log"),
				logScript(t, "crypt-wrong-password-strict-names.txt"))
		}, want: engines.TestResult{Reachable: true, Entries: 3, Marker: engines.MarkerUnreadable}, wantInMsg: "cannot be decrypted"},
		{name: "marker ok", crypt: true, expect: func(f *enginetest.FakeRunner) {
			f.Expect(proc.Rclone, enginetest.Args("lsf", "--max-depth", "1", "BKDEST:media/bk", "--use-json-log"),
				enginetest.Script{Stdout: []string{"a/"}})
			f.Expect(proc.Rclone, enginetest.Args("lsf", "--max-depth", "1", "BKCRYPT:", "--use-json-log"), enginetest.Script{Stdout: []string{".bunkarr/"}})
			f.Expect(proc.Rclone, enginetest.Prefix("cat"), enginetest.Script{Stdout: []string{ours}})
		}, want: engines.TestResult{OK: true, Reachable: true, Entries: 1, Marker: engines.MarkerOK,
			ID: "5f3c1a2e-7d4b-4c7a-9f1e-2b3c4d5e6f70", MarkerName: "offsite"}},
		{name: "marker of another destination", markerID: "other", expect: func(f *enginetest.FakeRunner) {
			f.Expect(proc.Rclone, enginetest.Prefix("lsf"), enginetest.Script{Stdout: []string{".bunkarr/"}})
			f.Expect(proc.Rclone, enginetest.Prefix("cat"), enginetest.Script{Stdout: []string{ours}})
		}, want: engines.TestResult{Reachable: true, Entries: 1, Marker: engines.MarkerForeign,
			ID: "5f3c1a2e-7d4b-4c7a-9f1e-2b3c4d5e6f70", MarkerName: "offsite"}, wantInMsg: "does not match"},
		{name: "not a marker", expect: func(f *enginetest.FakeRunner) {
			f.Expect(proc.Rclone, enginetest.Prefix("lsf"), enginetest.Script{Stdout: []string{".bunkarr/"}})
			f.Expect(proc.Rclone, enginetest.Prefix("cat"), enginetest.Script{Stdout: []string{"<html>"}})
		}, want: engines.TestResult{Reachable: true, Entries: 1, Marker: engines.MarkerUnreadable}, wantInMsg: "not a Bunkarr marker"},
		{name: "non-empty without a marker", expect: func(f *enginetest.FakeRunner) {
			var many []string
			for i := range 1500 {
				many = append(many, strings.Repeat("f", 1+i%50)+"-"+string(rune('a'+i%26)))
			}
			f.Expect(proc.Rclone, enginetest.Prefix("lsf"), enginetest.Script{Stdout: many})
			f.Expect(proc.Rclone, enginetest.Prefix("cat"), enginetest.Script{Exit: 3})
		}, want: engines.TestResult{OK: true, Reachable: true, Entries: MaxTestEntries, Marker: engines.MarkerMissing}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, f := newTestDriver(t)
			dest, sec := jobS3(tc.crypt)
			dest.MarkerID = tc.markerID
			tc.expect(f)
			got, err := d.Test(context.Background(), dest, sec)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(got.Message, tc.wantInMsg) {
				t.Errorf("message %q, want it to contain %q", got.Message, tc.wantInMsg)
			}
			got.Message, got.Warnings, got.EngineVersion = "", nil, ""
			if g, w := mustJSON(t, got), mustJSON(t, tc.want); g != w {
				t.Errorf("result\n got %s\nwant %s", g, w)
			}
			for _, c := range f.Calls() {
				if c.Cmd.Budget != TestBudget || c.Cmd.RetryBudget != TestRetryBudget {
					t.Errorf("%s: budget %s, retry budget %s", c, c.Cmd.Budget, c.Cmd.RetryBudget)
				}
			}
		})
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

func TestTestSFTP(t *testing.T) {
	d, f := newTestDriver(t)
	dest, sec := jobSFTP(t, false)
	unpinned := dest
	r := *dest.Remote.SFTP
	r.HostKeys = nil
	unpinned.Remote.SFTP = &r
	res, err := d.Test(context.Background(), unpinned, sec)
	if err != nil || res.OK || !strings.Contains(res.Message, "host keys") {
		t.Fatalf("unpinned: %+v %v", res, err)
	}
	if _, err := d.Create(context.Background(), unpinned, sec, false); err == nil {
		t.Fatal("create without pinned host keys")
	}
	if n := len(f.Calls()); n != 0 {
		t.Fatalf("%d commands ran against an unverified host", n)
	}
	f.Expect(proc.Rclone, enginetest.Args("lsf", "--max-depth", "1", "BKDEST:/backups/bunkarr", "--use-json-log"), enginetest.Script{})
	f.Expect(proc.Rclone, enginetest.Prefix("cat"), enginetest.Script{Exit: 3})
	f.Expect(proc.Rclone, enginetest.Args("about", "--json", "BKDEST:/backups/bunkarr", "--use-json-log"),
		enginetest.Script{Stdout: []string{`{"total": 1000, "used": 10, "free": 990}`}})
	res, err = d.Test(context.Background(), dest, sec)
	if err != nil || !res.OK || res.FreeBytes == nil || *res.FreeBytes != 990 || res.EngineVersion != "1.74.1" {
		t.Fatalf("sftp test: %+v %v", res, err)
	}
	f.Expect(proc.Rclone, enginetest.Prefix("lsf"), enginetest.Script{Exit: 1,
		Stderr: enginetest.Fixture(t, "restic", "rclone-backend-hostkey-mismatch.stderr.txt").Lines[:1]})
	res, err = d.Test(context.Background(), dest, sec)
	if err != nil || res.OK || res.Reachable || !strings.Contains(res.Message, "host key changed") {
		t.Fatalf("host key: %+v %v", res, err)
	}
}

func TestCreate(t *testing.T) {
	t.Run("new", func(t *testing.T) {
		d, f := newTestDriver(t)
		d.Now = func() time.Time { return time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC) }
		m := &markerStore{entries: 2}
		m.install(f)
		dest, sec := jobS3(true)
		res, err := d.Create(context.Background(), dest, sec, false)
		if err != nil {
			t.Fatal(err)
		}
		var mk Marker
		if err := json.Unmarshal([]byte(m.content), &mk); err != nil || mk.ID != res.MarkerID || mk.Name != "offsite" ||
			!mk.CreatedAt.Equal(d.Now()) || !strings.HasSuffix(m.content, "}\n") {
			t.Fatalf("marker %q, result %+v", m.content, res)
		}
		if res.Initialized || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "not empty") {
			t.Fatalf("result %+v", res)
		}
		rcats := f.CallsOf(proc.Rclone, "rcat")
		if len(rcats) != 1 || rcats[0].Args[3] != "BKCRYPT:.bunkarr/destination.json" {
			t.Fatalf("rcat calls %v", rcats)
		}
	})
	t.Run("exists", func(t *testing.T) {
		d, f := newTestDriver(t)
		m := &markerStore{content: `{"id":"abc","name":"old"}`}
		m.install(f)
		dest, sec := jobS3(false)
		if _, err := d.Create(context.Background(), dest, sec, false); err == nil || !strings.Contains(err.Error(), "attach") {
			t.Fatalf("create over a marker: %v", err)
		}
		res, err := d.Create(context.Background(), dest, sec, true)
		if err != nil || res.MarkerID != "abc" {
			t.Fatalf("attach: %+v %v", res, err)
		}
		if n := len(f.CallsOf(proc.Rclone, "rcat")); n != 0 {
			t.Fatalf("%d marker writes", n)
		}
	})
	t.Run("attach without a marker", func(t *testing.T) {
		d, f := newTestDriver(t)
		(&markerStore{}).install(f)
		dest, sec := jobS3(false)
		if _, err := d.Create(context.Background(), dest, sec, true); !errors.Is(err, engines.ErrMarkerMissing) {
			t.Fatalf("attach: %v", err)
		}
	})
	t.Run("not a marker", func(t *testing.T) {
		d, f := newTestDriver(t)
		(&markerStore{content: "hello"}).install(f)
		dest, sec := jobS3(false)
		if _, err := d.Create(context.Background(), dest, sec, false); err == nil || !strings.Contains(err.Error(), "never replaced") {
			t.Fatalf("create over a foreign file: %v", err)
		}
		if n := len(f.CallsOf(proc.Rclone, "rcat")); n != 0 {
			t.Fatal("a foreign file was replaced")
		}
	})
	t.Run("read-back fails", func(t *testing.T) {
		d, f := newTestDriver(t)
		m := &markerStore{}
		m.install(f)
		dest, sec := jobS3(false)
		// The first cat (the check) finds nothing; after the write the read-back fails once.
		f.Expect(proc.Rclone, enginetest.Prefix("cat"), enginetest.Script{Exit: 3})
		f.Expect(proc.Rclone, enginetest.Prefix("cat"), enginetest.Script{Exit: 1, Stderr: []string{
			`{"time":"2026-09-27T13:53:10Z","level":"error","msg":"connection reset"}`}})
		if _, err := d.Create(context.Background(), dest, sec, false); err == nil {
			t.Fatal("create succeeded without a read-back")
		}
		if m.deleted != 1 || m.content != "" {
			t.Fatalf("the written marker was not removed (deleted %d, content %q)", m.deleted, m.content)
		}
	})
	// The create's budget (or its caller) ends while the marker is written or read back: the
	// marker is still removed, on a context of its own, and when it cannot be, Create returns its
	// id with the error, so the caller keeps what reads it (crypt: the only copy of the secret).
	cancelled := func(t *testing.T, during string, cleanupFails bool) (*markerStore, engines.CreateResult, error) {
		t.Helper()
		d, f := newTestDriver(t)
		m := &markerStore{}
		m.install(f)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		f.Expect(proc.Rclone, enginetest.Prefix("cat"), enginetest.Script{Exit: 3})
		write := func(c *enginetest.Call) {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.content = string(c.Stdin)
			if cleanupFails {
				m.failCat = 1
			}
		}
		if during == "write" {
			f.Expect(proc.Rclone, enginetest.Prefix("rcat"), enginetest.Script{Hook: func(c *enginetest.Call) { write(c); cancel() }})
		} else {
			f.Expect(proc.Rclone, enginetest.Prefix("rcat"), enginetest.Script{Hook: write})
			f.Expect(proc.Rclone, enginetest.Prefix("cat"), enginetest.Script{Hook: func(*enginetest.Call) { cancel() }})
		}
		dest, sec := jobS3(true)
		res, err := d.Create(ctx, dest, sec, false)
		if err == nil {
			t.Fatal("the create succeeded")
		}
		return m, res, err
	}
	for _, during := range []string{"write", "read-back"} {
		t.Run("cancelled during the "+during, func(t *testing.T) {
			m, res, err := cancelled(t, during, false)
			if m.deleted != 1 || m.content != "" || res.MarkerID != "" {
				t.Fatalf("the written marker was not removed (deleted %d, content %q, result %+v, error %v)", m.deleted, m.content, res, err)
			}
		})
	}
	t.Run("cancelled, and the marker cannot be removed", func(t *testing.T) {
		m, res, err := cancelled(t, "read-back", true)
		var mk Marker
		if jerr := json.Unmarshal([]byte(m.content), &mk); jerr != nil || mk.ID == "" || res.MarkerID != mk.ID || m.deleted != 0 {
			t.Fatalf("result %+v (error %v) for the marker %q that stayed (deleted %d)", res, err, m.content, m.deleted)
		}
	})
	t.Run("remove marker", func(t *testing.T) {
		d, f := newTestDriver(t)
		m := &markerStore{content: `{"id":"abc","name":"x"}`}
		m.install(f)
		dest, sec := jobS3(false)
		if err := d.RemoveMarker(context.Background(), dest, sec, "other"); err != nil || m.deleted != 0 {
			t.Fatalf("another id: %v, deleted %d", err, m.deleted)
		}
		if err := d.RemoveMarker(context.Background(), dest, sec, "abc"); err != nil || m.deleted != 1 {
			t.Fatalf("own id: %v, deleted %d", err, m.deleted)
		}
		dels := f.CallsOf(proc.Rclone, "deletefile")
		if len(dels) != 1 || dels[0].Args[1] != "BKDEST:media/bk/.bunkarr/destination.json" || dels[0].Args[3] != "1" {
			t.Fatalf("deletefile %v", dels)
		}
	})
}

func TestCapabilities(t *testing.T) {
	for _, tc := range []struct {
		kind  engines.DestKind
		enc   engines.EncryptionMode
		gran  int64
		modes bool
	}{
		{engines.S3, engines.EncryptionCrypt, 1, true},
		{engines.B2, engines.EncryptionNone, 1_000_000, false},
		{engines.SFTP, engines.EncryptionCrypt, 1_000_000_000, true},
	} {
		c := Capabilities(engines.Destination{Kind: tc.kind, Encryption: tc.enc})
		if c.Hardlinks || c.CaseInsensitive || c.InvalidChars != "" || c.UnstableInodes || !c.TrailingDotSpace ||
			c.MtimeGranularityNs != tc.gran || c.EnforcesModes != tc.modes {
			t.Errorf("%s/%s: %+v", tc.kind, tc.enc, c)
		}
	}
}
