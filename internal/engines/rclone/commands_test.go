package rclone

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/bwlimit"
	"github.com/sl0wz3r/bunkarr/internal/engines/enginetest"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
)

const testRun = ".bunkarr/retention/20260927T010000Z-job7"

// TestCopyCommandLine pins §7.3's copy command line, its file list and its environment.
func TestCopyCommandLine(t *testing.T) {
	d, f := newTestDriver(t)
	dest, sec := jobS3(true)
	c := connect(t, d, dest, sec)
	var args []string
	var list string
	script := logScript(t, "copy.jsonl")
	script.Hook = func(call *enginetest.Call) {
		list = string(call.DataFile("files"))
		args = slices.Clone(call.Args)
		for i, a := range args {
			if a == call.Dir.DataPath("files") {
				args[i] = "<run>/files"
			}
		}
	}
	e := f.Expect(proc.Rclone, enginetest.Prefix("copy"), script)
	e.Env(append(withPrefix("BKDEST", "TYPE", "PROVIDER", "ENDPOINT", "REGION", "ACCESS_KEY_ID", "SECRET_ACCESS_KEY", "ENV_AUTH",
		"NO_CHECK_BUCKET", "FORCE_PATH_STYLE"), append(withPrefix("BKCRYPT", "TYPE", "REMOTE", "PASSWORD", "PASSWORD2",
		"FILENAME_ENCRYPTION", "DIRECTORY_NAME_ENCRYPTION", "STRICT_NAMES"), "RCLONE_CONFIG")...)...)
	var stats []Stats
	res, err := c.Copy(context.Background(), CopyInput{SourceRoot: "/mnt/user/media", DestFolder: "Movies",
		Files: []string{"Nosferatu (1922)/Nosferatu.mkv", "#hash start.srt"}, RetentionDir: testRun, MaxDelete: 2,
		MaxDuration: 90*time.Second + 500*time.Millisecond, OnStats: func(s Stats) { stats = append(stats, s) }})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"copy", "/mnt/user/media", "BKCRYPT:Movies", "--files-from-raw", "<run>/files", "--no-traverse",
		"--backup-dir", "BKCRYPT:" + testRun + "/Movies", "--max-delete", "2",
		"--use-json-log", "-v", "--stats", "5s", "--stats-log-level", "NOTICE", "--transfers", "4", "--checkers", "8",
		"--max-duration", "1m31s", "--cutoff-mode", "soft"}
	if !slices.Equal(args, want) {
		t.Fatalf("argv\n got %q\nwant %q", args, want)
	}
	if list != "Nosferatu (1922)/Nosferatu.mkv\n#hash start.srt\n" {
		t.Fatalf("file list %q", list)
	}
	if len(res.Events) != 4 || res.Events[0].Kind != EventCopiedNew || res.Code != 0 || res.Class != ExitOK {
		t.Fatalf("result %+v", res)
	}
	if len(stats) != 1 || stats[0].Transfers != 4 {
		t.Fatalf("stats %+v", stats)
	}
}

func TestCopyWithoutWindowAndCACert(t *testing.T) {
	d, f := newTestDriver(t)
	dest, sec := jobS3(false)
	dest.Remote.S3.CACert = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"
	dest.Transfers = 2
	c := connect(t, d, dest, sec)
	e := f.Expect(proc.Rclone, enginetest.Prefix("copy"), enginetest.Script{Hook: func(call *enginetest.Call) {
		if call.Has("--max-duration") || call.Has("--cutoff-mode") {
			t.Errorf("a copy without a window has a cutoff: %s", call)
		}
		if v, _ := call.Flag("--ca-cert"); v != call.Dir.SecretPath("ca.pem") || string(call.SecretFiles["ca.pem"]) != dest.Remote.S3.CACert {
			t.Errorf("--ca-cert %q, secret files %v", v, call.SecretFiles)
		}
		if v, _ := call.Flag("--transfers"); v != "2" {
			t.Errorf("--transfers %s", v)
		}
		if v, _ := call.Flag("--checkers"); v != "4" {
			t.Errorf("--checkers %s", v)
		}
		if call.Args[2] != "BKDEST:media/bk/TV" {
			t.Errorf("destination %s", call.Args[2])
		}
	}})
	if _, err := c.Copy(context.Background(), CopyInput{SourceRoot: "/src", DestFolder: "TV", Files: []string{"a"}, RetentionDir: testRun, MaxDelete: 1}); err != nil {
		t.Fatal(err)
	}
	if e.Calls() != 1 {
		t.Fatal("copy did not run")
	}
}

// TestMaxDeleteOnEveryRemovingCommand: every command that can remove or replace an object
// carries --max-delete with the count its caller gives (S23; §14.1 rclone).
func TestMaxDeleteOnEveryRemovingCommand(t *testing.T) {
	ok := func(string) bool { return true }
	for _, tc := range []struct {
		sub  string
		run  func(*Conn) error
		want string
	}{
		{"copy", func(c *Conn) error {
			_, err := c.Copy(context.Background(), CopyInput{SourceRoot: "/src", DestFolder: "M", Files: []string{"a", "b", "c"},
				RetentionDir: testRun, MaxDelete: 3})
			return err
		}, "3"},
		{"move", func(c *Conn) error {
			_, err := c.Move(context.Background(), MoveInput{SrcDir: "M", DstDir: testRun + "/M", Files: []string{"a", "b"}, MaxDelete: 2})
			return err
		}, "2"},
		{"moveto", func(c *Conn) error {
			_, err := c.MoveTo(context.Background(), "M/a", testRun+"/M/a.1")
			return err
		}, "1"},
		{"delete", func(c *Conn) error {
			_, err := c.Delete(context.Background(), []string{testRun + "/M/a", testRun + "/M/b"}, 2)
			return err
		}, "2"},
		{"deletefile", func(c *Conn) error { return c.DeleteFile(context.Background(), testRun+"/M/a") }, "1"},
		{"purge", func(c *Conn) error {
			return c.Purge(context.Background(), ".bunkarr/plex/plex-1/20260924T120000Z", 4, ok)
		}, "4"},
	} {
		t.Run(tc.sub, func(t *testing.T) {
			d, f := newTestDriver(t)
			dest, sec := jobS3(true)
			c := connect(t, d, dest, sec)
			e := f.Expect(proc.Rclone, enginetest.Prefix(tc.sub), enginetest.Script{})
			if err := tc.run(c); err != nil {
				t.Fatal(err)
			}
			calls := f.CallsOf(proc.Rclone, tc.sub)
			if e.Calls() != 1 || len(calls) != 1 {
				t.Fatalf("%d calls", len(calls))
			}
			if got := calls[0].FlagValues("--max-delete"); len(got) != 1 || got[0] != tc.want {
				t.Fatalf("--max-delete %v, want %s: %s", got, tc.want, calls[0])
			}
		})
	}
}

// TestFences: nothing outside .bunkarr/retention/<run>/ reaches delete or deletefile, purge takes
// only version directories, rcat only Bunkarr's own files, move only moves a destFolder into
// retention, and no command runs for a refused path (§14.1 rclone, S2, S23).
func TestFences(t *testing.T) {
	d, f := newTestDriver(t)
	dest, sec := jobS3(false)
	c := connect(t, d, dest, sec)
	ctx := context.Background()
	yes := func(string) bool { return true }
	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{"delete a live file", func() error { _, err := c.Delete(ctx, []string{"Movies/a.mkv"}, 1); return err }},
		{"delete the retention root", func() error { _, err := c.Delete(ctx, []string{".bunkarr/retention/x"}, 1); return err }},
		{"delete a run directory itself", func() error { _, err := c.Delete(ctx, []string{testRun}, 1); return err }},
		{"delete a bad run name", func() error { _, err := c.Delete(ctx, []string{".bunkarr/retention/job7/M/a"}, 1); return err }},
		{"delete with dot-dot", func() error { _, err := c.Delete(ctx, []string{testRun + "/../../../Movies/a"}, 1); return err }},
		{"delete one bad among good", func() error {
			_, err := c.Delete(ctx, []string{testRun + "/M/a", ".bunkarr/destination.json"}, 2)
			return err
		}},
		{"delete negative max", func() error { _, err := c.Delete(ctx, []string{testRun + "/M/a"}, -1); return err }},
		{"deletefile a live file", func() error { return c.DeleteFile(ctx, "Movies/a.mkv") }},
		{"deletefile the marker", func() error { return c.DeleteFile(ctx, ".bunkarr/destination.json") }},
		{"purge without a validator", func() error { return c.Purge(ctx, ".bunkarr/plex/p-1/20260924T120000Z", 3, nil) }},
		{"purge a path the validator refuses", func() error {
			return c.Purge(ctx, ".bunkarr/plex/p-1/x", 3, func(string) bool { return false })
		}},
		{"purge retention", func() error { return c.Purge(ctx, testRun, 3, yes) }},
		{"purge a destFolder", func() error { return c.Purge(ctx, "Movies", 3, yes) }},
		{"purge the root", func() error { return c.Purge(ctx, "", 3, yes) }},
		{"rcat a live file", func() error { return c.Rcat(ctx, "Movies/a.mkv", []byte("x")) }},
		{"rcat into retention", func() error { return c.Rcat(ctx, testRun+"/x", []byte("x")) }},
		{"move into a destFolder", func() error {
			_, err := c.Move(ctx, MoveInput{SrcDir: "Movies", DstDir: "TV", Files: []string{"a"}, MaxDelete: 1})
			return err
		}},
		{"move into a run directory itself", func() error {
			_, err := c.Move(ctx, MoveInput{SrcDir: "Movies", DstDir: testRun, Files: []string{"a"}, MaxDelete: 1})
			return err
		}},
		{"move out of .bunkarr", func() error {
			_, err := c.Move(ctx, MoveInput{SrcDir: ".bunkarr/plex", DstDir: testRun + "/x", Files: []string{"a"}, MaxDelete: 1})
			return err
		}},
		{"moveto the marker", func() error { _, err := c.MoveTo(ctx, filecopyMarker, testRun+"/m"); return err }},
		{"moveto onto itself", func() error { _, err := c.MoveTo(ctx, "M/a", "M/a"); return err }},
		{"moveto a control character", func() error { _, err := c.MoveTo(ctx, "M/a\tb", "M/c"); return err }},
		{"copy into .bunkarr", func() error {
			_, err := c.Copy(ctx, CopyInput{SourceRoot: "/src", DestFolder: ".bunkarr", Files: []string{"a"}, RetentionDir: testRun, MaxDelete: 1})
			return err
		}},
		{"copy a name with a line break", func() error {
			_, err := c.Copy(ctx, CopyInput{SourceRoot: "/src", DestFolder: "M", Files: []string{"a\nb"}, RetentionDir: testRun, MaxDelete: 1})
			return err
		}},
		{"copy a relative source", func() error {
			_, err := c.Copy(ctx, CopyInput{SourceRoot: "src", DestFolder: "M", Files: []string{"a"}, RetentionDir: testRun, MaxDelete: 1})
			return err
		}},
		{"copy with another backup dir", func() error {
			_, err := c.Copy(ctx, CopyInput{SourceRoot: "/src", DestFolder: "M", Files: []string{"a"}, RetentionDir: "Movies", MaxDelete: 1})
			return err
		}},
		{"stat an absolute path", func() error { _, err := c.StatMany(ctx, "", []string{"/etc/passwd"}); return err }},
	} {
		if err := tc.run(); err == nil {
			t.Errorf("%s: no error", tc.name)
		} else if !errors.Is(err, ErrFence) && !strings.Contains(err.Error(), "negative") {
			t.Errorf("%s: %v, want a fence error", tc.name, err)
		}
	}
	if n := len(f.Calls()); n != 0 {
		t.Fatalf("%d commands ran for refused paths: %v", n, f.Calls())
	}
}

const filecopyMarker = ".bunkarr/destination.json"

func TestStatMany(t *testing.T) {
	d, f := newTestDriver(t)
	dest, sec := jobS3(true)
	c := connect(t, d, dest, sec)
	f.Expect(proc.Rclone, enginetest.Prefix("lsjson"), enginetest.Script{
		Stdout: lsjsonOut(`{"Path":"a.mkv","Name":"a.mkv","Size":3,"ModTime":"2026-09-27T13:53:10.123456789Z","IsDir":false}`,
			`{"Path":"d","Name":"d","Size":-1,"ModTime":"2026-09-27T13:53:10Z","IsDir":true}`),
		Hook: func(call *enginetest.Call) {
			if got := string(call.DataFile("files")); got != "a.mkv\nb.mkv\nd\n" {
				t.Errorf("list %q", got)
			}
			want := []string{"lsjson", "-R", "--files-only", "--no-mimetype", "--files-from-raw", call.Dir.DataPath("files"), "BKCRYPT:Movies", "--use-json-log"}
			if !slices.Equal(call.Args, want) {
				t.Errorf("argv %q", call.Args)
			}
			if call.Cmd.Budget != DefaultListingBudget {
				t.Errorf("budget %s", call.Cmd.Budget)
			}
		}})
	got, err := c.StatMany(context.Background(), "Movies", []string{"a.mkv", "b.mkv", "d"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["a.mkv"].Size != 3 || got["a.mkv"].MtimeNs()%1_000_000_000 != 123456789 {
		t.Fatalf("got %+v", got)
	}
	f.Expect(proc.Rclone, enginetest.Prefix("lsjson"), logScript(t, "exit3-bucket-not-found.txt"))
	got, err = c.StatMany(context.Background(), "gone", []string{"a"})
	if err != nil || len(got) != 0 {
		t.Fatalf("missing directory: %v %v", got, err)
	}
}

func TestLsJSON(t *testing.T) {
	d, f := newTestDriver(t)
	dest, sec := jobB2()
	c := connect(t, d, dest, sec)
	f.Expect(proc.Rclone, enginetest.Args("lsjson", "-R", "--files-only", "--no-mimetype", "BKDEST:bkt/p/q/Movies", "--use-json-log"),
		enginetest.Script{Stdout: lsjsonOut(`{"Path":"x/a","Name":"a","Size":1,"ModTime":"2026-09-27T13:53:10Z","IsDir":false}`,
			`{"Path":"b","Name":"b","Size":2,"ModTime":"2026-09-27T13:53:10Z","IsDir":false}`)})
	var paths []string
	if err := c.LsJSON(context.Background(), "Movies", true, func(o Object) error { paths = append(paths, o.Path); return nil }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(paths, []string{"x/a", "b"}) {
		t.Fatalf("paths %v", paths)
	}
	f.Expect(proc.Rclone, enginetest.Prefix("lsjson"), logScript(t, "exit3-bucket-not-found.txt"))
	if err := c.LsJSON(context.Background(), "gone", false, func(Object) error { return nil }); !errors.Is(err, ErrPathNotFound) {
		t.Fatalf("missing directory: %v", err)
	}
}

func TestCheckCombined(t *testing.T) {
	d, f := newTestDriver(t)
	dest, sec := jobS3(true)
	c := connect(t, d, dest, sec)
	writeCombined := func(content string) func(*enginetest.Call) {
		return func(call *enginetest.Call) {
			p, _ := call.Flag("--combined")
			if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, call.Dir.DataDir()) {
				t.Errorf("--combined %q must outlive the command's run directory", p)
			}
			want := []string{"check", "/src", "BKCRYPT:Movies", "--one-way", "--download", "--files-from-raw",
				call.Dir.DataPath("sample"), "--combined", p, "--checkers", "4", "--use-json-log"}
			if !slices.Equal(call.Args, want) {
				t.Errorf("argv %q", call.Args)
			}
			if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
				t.Error(err)
			}
		}
	}
	f.Expect(proc.Rclone, enginetest.Prefix("check"), enginetest.Script{Exit: 1, Hook: writeCombined("= a\n* b\n+ c\n! d\n")})
	marks, err := c.Check(context.Background(), CheckInput{SourceRoot: "/src", DestFolder: "Movies", Files: []string{"a", "b", "c", "d"}})
	if err != nil {
		t.Fatal(err)
	}
	if marks["a"] != MarkMatch || marks["b"] != MarkDiffer || marks["c"] != MarkMissingOnDest || marks["d"] != MarkError {
		t.Fatalf("marks %v", marks)
	}
	f.Expect(proc.Rclone, enginetest.Prefix("check"), logScript(t, "exit1-bad-credentials.txt"))
	if _, err := c.Check(context.Background(), CheckInput{SourceRoot: "/src", DestFolder: "Movies", Files: []string{"a"}}); !errors.Is(err, ErrFailed) {
		t.Fatalf("check without a report: %v", err)
	}
}

func TestCheckMarker(t *testing.T) {
	marker := `{"id":"5f3c1a2e-7d4b-4c7a-9f1e-2b3c4d5e6f70","name":"offsite","createdAt":"2026-09-27T13:00:00Z"}`
	for _, tc := range []struct {
		name   string
		script enginetest.Script
		want   error
	}{
		{"ok", enginetest.Script{Stdout: strings.Split("{\n  \"id\": \"5f3c1a2e-7d4b-4c7a-9f1e-2b3c4d5e6f70\",\n  \"name\": \"offsite\"\n}", "\n")}, nil},
		{"empty (an S3 directory)", enginetest.Script{}, engines.ErrMarkerMissing},
		{"object not found", enginetest.Script{Exit: 3, Stderr: []string{`{"time":"2026-09-27T13:53:10Z","level":"error","msg":"directory not found"}`}}, engines.ErrMarkerMissing},
		{"not a marker", enginetest.Script{Stdout: []string{"hello"}}, engines.ErrMarkerMissing},
		{"another destination", enginetest.Script{Stdout: []string{strings.Replace(marker, "5f3c", "0000", 1)}}, engines.ErrMarkerMismatch},
		{"host key", enginetest.Script{Exit: 1, Stderr: enginetest.Fixture(t, "restic", "rclone-backend-hostkey-mismatch.stderr.txt").Lines}, engines.ErrHostKeyChanged},
		{"credentials", logScript(t, "exit1-bad-credentials.txt"), ErrFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, f := newTestDriver(t)
			dest, sec := jobS3(true)
			c := connect(t, d, dest, sec)
			f.Expect(proc.Rclone, enginetest.Args("cat", "--count", "65536", "BKCRYPT:.bunkarr/destination.json", "--use-json-log"), tc.script)
			m, err := c.CheckMarker(context.Background())
			if !errors.Is(err, tc.want) || (tc.want == nil && m.Name != "offsite") {
				t.Fatalf("CheckMarker = %+v, %v; want %v", m, err, tc.want)
			}
		})
	}
	t.Run("pending", func(t *testing.T) {
		d, f := newTestDriver(t)
		dest, sec := jobS3(true)
		dest.MarkerID = "pending:1234"
		if _, err := connect(t, d, dest, sec).CheckMarker(context.Background()); !errors.Is(err, engines.ErrPending) {
			t.Fatalf("pending: %v", err)
		}
		if len(f.Calls()) != 0 {
			t.Fatal("a pending destination ran a command")
		}
	})
}

// TestRetryExit5: exit 5 is retried once after RetryWait, then fatal (§10.3).
func TestRetryExit5(t *testing.T) {
	d, f := newTestDriver(t)
	dest, sec := jobS3(false)
	c := connect(t, d, dest, sec)
	f.Expect(proc.Rclone, enginetest.Prefix("rmdirs"), enginetest.Script{Exit: 5})
	f.Expect(proc.Rclone, enginetest.Prefix("rmdirs"), enginetest.Script{})
	if err := c.Rmdirs(context.Background(), "Movies", true); err != nil {
		t.Fatal(err)
	}
	f.Expect(proc.Rclone, enginetest.Prefix("rmdirs"), enginetest.Script{Exit: 5}).Times(2)
	if err := c.Rmdirs(context.Background(), "Movies", true); !errors.Is(err, ErrTemporary) {
		t.Fatalf("twice exit 5: %v", err)
	}
	if n := len(f.CallsOf(proc.Rclone, "rmdirs")); n != 4 {
		t.Fatalf("%d rmdirs calls, want 4", n)
	}
	if got := f.CallsOf(proc.Rclone, "rmdirs")[0].Args; !slices.Equal(got, []string{"rmdirs", "BKDEST:media/bk/Movies", "--leave-root", "--use-json-log"}) {
		t.Fatalf("argv %q", got)
	}
	f.Expect(proc.Rclone, enginetest.Prefix("rmdirs"), enginetest.Script{Exit: 3})
	if err := c.Rmdirs(context.Background(), "gone", false); err != nil {
		t.Fatalf("rmdirs of a missing directory: %v", err)
	}
}

// TestTransferOutcomes: a cutoff (exit 10), Bunkarr's interrupt, per-object errors, --max-delete
// reached (exit 7), an overlap and a cancellation (rclone running or not yet started) are told
// apart; the result is filled in every case.
func TestTransferOutcomes(t *testing.T) {
	in := func() CopyInput {
		return CopyInput{SourceRoot: "/src", DestFolder: "M", Files: []string{"big.mkv", "big2.mkv"}, RetentionDir: testRun, MaxDelete: 2}
	}
	t.Run("cutoff", func(t *testing.T) {
		d, f := newTestDriver(t)
		dest, sec := jobS3(false)
		f.Expect(proc.Rclone, enginetest.Prefix("copy"), logScript(t, "exit10-max-duration-soft.txt"))
		res, err := connect(t, d, dest, sec).Copy(context.Background(), in())
		if err != nil || !res.Cutoff || res.Class != ExitCutoff {
			t.Fatalf("%+v %v", res, err)
		}
	})
	t.Run("interrupt", func(t *testing.T) {
		d, f := newTestDriver(t)
		dest, sec := jobS3(false)
		f.Expect(proc.Rclone, enginetest.Prefix("copy"), enginetest.Script{UntilInterrupted: true})
		stop := make(chan struct{})
		time.AfterFunc(20*time.Millisecond, func() { close(stop) })
		x := in()
		x.Interrupt = stop
		res, err := connect(t, d, dest, sec).Copy(context.Background(), x)
		if err != nil || !res.Interrupted {
			t.Fatalf("%+v %v", res, err)
		}
	})
	t.Run("max-delete", func(t *testing.T) {
		d, f := newTestDriver(t)
		dest, sec := jobS3(false)
		f.Expect(proc.Rclone, enginetest.Prefix("copy"), logScript(t, "sync-max-delete.jsonl"))
		res, err := connect(t, d, dest, sec).Copy(context.Background(), in())
		if !errors.Is(err, ErrMaxDelete) || res.ObjectErrors["big2.mkv"] == "" || len(res.Events) != 2 {
			t.Fatalf("%+v %v", res, err)
		}
	})
	t.Run("overlap", func(t *testing.T) {
		d, f := newTestDriver(t)
		dest, sec := jobS3(false)
		f.Expect(proc.Rclone, enginetest.Prefix("copy"), logScript(t, "sync-backup-dir-overlap.txt"))
		_, err := connect(t, d, dest, sec).Copy(context.Background(), in())
		if !errors.Is(err, ErrFatal) || !strings.Contains(err.Error(), "mustn't overlap") {
			t.Fatalf("%v", err)
		}
	})
	t.Run("per-object", func(t *testing.T) {
		d, f := newTestDriver(t)
		dest, sec := jobS3(false)
		f.Expect(proc.Rclone, enginetest.Prefix("copy"), enginetest.Script{Exit: 1, Stderr: []string{
			`{"time":"2026-09-27T13:53:10Z","level":"error","msg":"Failed to copy: name too long","object":"big2.mkv","objectType":"*local.Object"}`}})
		res, err := connect(t, d, dest, sec).Copy(context.Background(), in())
		if err != nil || res.Class != ExitObjects || res.ObjectErrors["big2.mkv"] != "Failed to copy: name too long" {
			t.Fatalf("%+v %v", res, err)
		}
	})
	t.Run("progress", func(t *testing.T) {
		d, f := newTestDriver(t)
		dest, sec := jobS3(false)
		f.Expect(proc.Rclone, enginetest.Prefix("copy"), logScript(t, "copy-bwlimit-progress.jsonl"))
		var current []string
		x := in()
		x.OnStats = func(s Stats) { current = append(current, s.CurrentFile()) }
		if _, err := connect(t, d, dest, sec).Copy(context.Background(), x); err != nil {
			t.Fatal(err)
		}
		if len(current) == 0 || current[0] != "big.mkv" {
			t.Fatalf("current files %q", current)
		}
	})
	// The job is cancelled while rclone runs: cancelled from the running command's hook, not by a
	// timer, which on a busy runner can fire before Start (that refuses a cancelled context and
	// runs nothing: "cancel-before-start").
	t.Run("cancel", func(t *testing.T) {
		d, f := newTestDriver(t)
		dest, sec := jobS3(false)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		f.Expect(proc.Rclone, enginetest.Prefix("copy"), enginetest.Script{UntilInterrupted: true,
			Hook: func(*enginetest.Call) { cancel() }})
		res, err := connect(t, d, dest, sec).Copy(ctx, in())
		calls := f.CallsOf(proc.Rclone, "copy")
		if !errors.Is(err, context.Canceled) || res.Interrupted || len(calls) != 1 || !calls[0].Status.Cancelled {
			t.Fatalf("%+v %v (%d copy commands)", res, err, len(calls))
		}
	})
	// The job is cancelled before rclone starts: nothing runs, the run directory is removed and
	// the cancellation is returned (the caller lists the batch and keeps it pending).
	t.Run("cancel-before-start", func(t *testing.T) {
		d, f := newTestDriver(t)
		cfg, shm := t.TempDir(), t.TempDir()
		d.RunDirs = proc.NewRunDirs(cfg, proc.RunDirOptions{ShmDir: shm, StatFS: func(string) (int64, error) {
			return 0x01021994, nil // tmpfs, as enginetest.RunDirs
		}})
		dest, sec := jobS3(false)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		res, err := connect(t, d, dest, sec).Copy(ctx, in())
		if !errors.Is(err, context.Canceled) || res.Interrupted || res.Cutoff || len(f.Calls()) != 0 {
			t.Fatalf("%+v %v (%d commands)", res, err, len(f.Calls()))
		}
		for _, dir := range []string{filepath.Join(cfg, "run"), filepath.Join(shm, "bunkarr-run")} {
			if ents, _ := os.ReadDir(dir); len(ents) != 0 {
				t.Errorf("run directory left in %s: %v", dir, ents)
			}
		}
	})
}

func TestMoveAndMoveTo(t *testing.T) {
	d, f := newTestDriver(t)
	dest, sec := jobSFTP(t, false)
	c := connect(t, d, dest, sec)
	f.Expect(proc.Rclone, enginetest.Prefix("move"), enginetest.Script{Hook: func(call *enginetest.Call) {
		want := []string{"move", "BKDEST:/backups/bunkarr/TV", "BKDEST:/backups/bunkarr/" + testRun + "/TV", "--files-from-raw",
			call.Dir.DataPath("files"), "--no-traverse", "--max-delete", "2", "--use-json-log", "-v", "--stats", "5s",
			"--stats-log-level", "NOTICE", "--transfers", "4", "--checkers", "8"}
		if !slices.Equal(call.Args, want) {
			t.Errorf("argv %q", call.Args)
		}
		if kh := call.SecretFiles["known_hosts"]; !strings.HasPrefix(string(kh), "[nas.example]:2222 ssh-ed25519 ") {
			t.Errorf("known_hosts %q", kh)
		}
		if call.Env[proc.RemoteEnv(proc.RemoteDest, "KNOWN_HOSTS_FILE")] != call.Dir.SecretPath("known_hosts") {
			t.Errorf("known_hosts path %q", call.Env[proc.RemoteEnv(proc.RemoteDest, "KNOWN_HOSTS_FILE")])
		}
	}})
	if _, err := c.Move(context.Background(), MoveInput{SrcDir: "TV", DstDir: testRun + "/TV", Files: []string{"a", "b"}, MaxDelete: 2}); err != nil {
		t.Fatal(err)
	}
	f.Expect(proc.Rclone, enginetest.Args("moveto", "BKDEST:/backups/bunkarr/TV/a", "BKDEST:/backups/bunkarr/"+testRun+"/TV/a.1",
		"--max-delete", "1", "--use-json-log", "-v", "--stats", "5s", "--stats-log-level", "NOTICE"), enginetest.Script{})
	if _, err := c.MoveTo(context.Background(), "TV/a", testRun+"/TV/a.1"); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteLists(t *testing.T) {
	d, f := newTestDriver(t)
	dest, sec := jobS3(true)
	c := connect(t, d, dest, sec)
	f.Expect(proc.Rclone, enginetest.Prefix("delete"), enginetest.Script{Hook: func(call *enginetest.Call) {
		if call.Args[1] != "BKCRYPT:.bunkarr/retention" {
			t.Errorf("delete root %q", call.Args[1])
		}
		if got := string(call.DataFile("files")); got != "20260927T010000Z-job7/M/a\n20260927T010000Z-job7/M/b c\n" {
			t.Errorf("list %q", got)
		}
	}})
	if _, err := c.Delete(context.Background(), []string{testRun + "/M/a", testRun + "/M/b c"}, 2); err != nil {
		t.Fatal(err)
	}
}

func TestSmallCommands(t *testing.T) {
	d, f := newTestDriver(t)
	dest, sec := jobS3(false)
	dest.Bandwidth = bwlimit.Config{UploadKiBps: 2048}
	c := connect(t, d, dest, sec)
	ctx := context.Background()

	f.Expect(proc.Rclone, enginetest.Args("rcat", "--size", "6", "BKDEST:media/bk/.bunkarr/links.tsv", "--use-json-log"),
		enginetest.Script{Hook: func(call *enginetest.Call) {
			if string(call.Stdin) != "a\tb\nc\n" {
				t.Errorf("stdin %q", call.Stdin)
			}
			if call.Env["RCLONE_BWLIMIT"] != "2048k:off" {
				t.Errorf("RCLONE_BWLIMIT %q", call.Env["RCLONE_BWLIMIT"])
			}
		}})
	if err := c.Rcat(ctx, ".bunkarr/links.tsv", []byte("a\tb\nc\n")); err != nil {
		t.Fatal(err)
	}
	f.Expect(proc.Rclone, enginetest.Args("cat", "--count", "5", "BKDEST:media/bk/.bunkarr/x", "--use-json-log"),
		enginetest.Script{Stdout: []string{"ab", "cdef"}})
	if b, err := c.Cat(ctx, ".bunkarr/x", 5); err != nil || string(b) != "ab\ncd" {
		t.Fatalf("cat = %q %v", b, err)
	}
	f.Expect(proc.Rclone, enginetest.Args("backend", "cleanup", "BKDEST:media/bk", "-o", "max-age=168h0m0s", "--use-json-log"), enginetest.Script{})
	if err := c.BackendCleanup(ctx, 7*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := c.BackendCleanup(ctx, 24*time.Hour); err == nil {
		t.Fatal("a cleanup max-age of one day was accepted")
	}
	f.Expect(proc.Rclone, enginetest.Args("about", "--json", "BKDEST:media/bk", "--use-json-log"),
		enginetest.Script{Stdout: []string{`{"total":100,"used":40,"free":60}`}})
	if a, err := c.About(ctx); err != nil || *a.Free != 60 || a.Objects != nil {
		t.Fatalf("about = %+v %v", a, err)
	}
	f.Expect(proc.Rclone, enginetest.Args("copyto", "/staging/v/manifest.json", "BKDEST:media/bk/.bunkarr/plex/p-1/v/manifest.json",
		"--use-json-log", "-v", "--stats", "5s", "--stats-log-level", "NOTICE"), enginetest.Script{})
	if err := c.CopyTo(ctx, "/staging/v/manifest.json", ".bunkarr/plex/p-1/v/manifest.json"); err != nil {
		t.Fatal(err)
	}
	f.Expect(proc.Rclone, enginetest.Args("copyto", "BKDEST:media/bk/.bunkarr/plex/p-1/v/manifest.json", "/staging/manifest.json",
		"--use-json-log"), enginetest.Script{Exit: 3})
	if err := c.Download(ctx, ".bunkarr/plex/p-1/v/manifest.json", "/staging/manifest.json"); !errors.Is(err, ErrPathNotFound) {
		t.Fatalf("download of a missing object: %v", err)
	}

	sd, ss := jobSFTP(t, false)
	if err := connect(t, d, sd, ss).BackendCleanup(ctx, MinCleanupAge); err == nil {
		t.Fatal("backend cleanup ran on sftp")
	}
}

func TestConnectRefuses(t *testing.T) {
	d, _ := newTestDriver(t)
	dest, sec := jobS3(false)
	local := dest
	local.Kind = engines.Local
	if _, err := d.Connect(local, sec, engines.Runtime{}); err == nil {
		t.Fatal("rclone connected a local destination")
	}
	bad := dest
	bad.Bandwidth = bwlimit.Config{Timetable: []bwlimit.Entry{{Days: []string{"mon"}, From: "25:00", To: "01:00"}}}
	if _, err := d.Connect(bad, sec, engines.Runtime{}); err == nil {
		t.Fatal("an invalid timetable was accepted")
	}
	if _, err := (&Driver{}).Connect(dest, sec, engines.Runtime{}); err == nil {
		t.Fatal("a driver without a runner connected")
	}
}

func TestMaxDurationRounding(t *testing.T) {
	for d, want := range map[time.Duration]string{time.Millisecond: "1s", time.Second: "1s", 1500 * time.Millisecond: "2s", 2 * time.Hour: "2h0m0s"} {
		if got := maxDuration(d); got != want {
			t.Errorf("maxDuration(%s) = %s, want %s", d, got, want)
		}
	}
}
