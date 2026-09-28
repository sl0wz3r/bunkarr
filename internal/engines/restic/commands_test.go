package restic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/bwlimit"
	"github.com/sl0wz3r/bunkarr/internal/engines/enginetest"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
)

// withRun replaces the run directory's paths in args by <run>/<name>.
func withRun(call *enginetest.Call) []string {
	out := slices.Clone(call.Args)
	for i, a := range out {
		if call.Dir != nil && strings.HasPrefix(a, call.Dir.DataDir()+"/") {
			out[i] = "<run>/" + strings.TrimPrefix(a, call.Dir.DataDir()+"/")
		}
	}
	return out
}

// TestBackupCommandLine pins §6.2's backup command line, its data files and environment on a
// remote repository.
func TestBackupCommandLine(t *testing.T) {
	d, f := newTestDriver(t)
	dest, sec := s3Repo()
	dest.Bandwidth = bwlimit.Config{UploadKiBps: 1024}
	r := connect(t, d, dest, sec)
	tags := mediaTags(t, testEngineTag, 3, 42, 2)
	var args []string
	var files, excludes, iexcludes string
	script := enginetest.FixtureScript(t, "restic", "backup-incremental.jsonl")
	script.Hook = func(call *enginetest.Call) {
		args = withRun(call)
		files, excludes, iexcludes = string(call.DataFile("files")), string(call.DataFile("excludes")), string(call.DataFile("iexcludes"))
		if string(call.SecretFiles["password"]) != testPassword {
			t.Errorf("password file %q", call.SecretFiles["password"])
		}
		if call.Env["RESTIC_PASSWORD_FILE"] != call.Dir.SecretPath("password") || call.Env["RESTIC_REPOSITORY"] != "rclone:BKDEST:restic/bk" ||
			call.Env["RCLONE_BWLIMIT"] != "1024k:off" || call.Env["RESTIC_CACHE_DIR"] != d.CacheRoot+"/11" || call.Env["RESTIC_PROGRESS_FPS"] != "0.5" {
			t.Errorf("env %v", call.Env)
		}
		for k := range call.Env {
			if strings.Contains(k, "BKCRYPT") || strings.HasPrefix(k, "AWS_") {
				t.Errorf("env %s", k)
			}
		}
	}
	f.Expect(proc.Restic, enginetest.Prefix("backup"), script).Env(append([]string{"RESTIC_REPOSITORY", "RESTIC_PASSWORD_FILE",
		"RESTIC_CACHE_DIR", "RESTIC_PROGRESS_FPS", "RCLONE_CONFIG", "RCLONE_BWLIMIT"}, prefixed("RCLONE_CONFIG_BKDEST_", "TYPE",
		"PROVIDER", "ENDPOINT", "REGION", "ACCESS_KEY_ID", "SECRET_ACCESS_KEY", "ENV_AUTH", "NO_CHECK_BUCKET", "FORCE_PATH_STYLE")...)...)
	var statuses int
	res, err := r.Backup(context.Background(), BackupArgs{
		FilesFrom: []string{"/mnt/media/Movies", "/mnt/media/TV/new\nline.mkv"}, Excludes: []string{"/config", `/mnt/media/**/*.nfo`},
		IExcludes: []string{"/mnt/media/**/.ds_store"}, Parent: id(7), IgnoreInode: true, Tags: tags,
		LimitUpKiB: 512, OnStatus: func(Status) { statuses++ }})
	if err != nil {
		t.Fatal(err)
	}
	want := append([]string{"backup", "--json", "--host", "bunkarr"}, flagEach("--tag", tags)...)
	want = append(want, "--files-from-raw", "<run>/files", "--exclude-file", "<run>/excludes", "--iexclude-file", "<run>/iexcludes",
		"--exclude-if-present", "bunkarr.key", "--parent", id(7), "--ignore-inode", "--pack-size", "64",
		"-o", "rclone.program=/usr/bin/rclone", "-o", "rclone.connections=6")
	if !slices.Equal(args, want) {
		t.Fatalf("argv\n got %q\nwant %q", args, want)
	}
	if files != "/mnt/media/Movies\x00/mnt/media/TV/new\nline.mkv\x00" {
		t.Fatalf("files %q", files)
	}
	if excludes != "/config\n/mnt/media/**/*.nfo\n" || iexcludes != "/mnt/media/**/.ds_store\n" {
		t.Fatalf("excludes %q, iexcludes %q", excludes, iexcludes)
	}
	if res.SnapshotID != "9b847c8cf8f3eb11f7f53a8fec59070fa23571eac3e3e4fc967948648a9df4ba" || res.Summary.FilesChanged != 1 ||
		res.Exit != 0 || statuses != 1 {
		t.Fatalf("result %+v, statuses %d", res, statuses)
	}
}

func prefixed(prefix string, names ...string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = prefix + n
	}
	return out
}

func flagEach(flag string, values []string) []string {
	var out []string
	for _, v := range values {
		out = append(out, flag, v)
	}
	return out
}

// TestBackupLocalAndVersions: a local repository gets --limit-upload/--limit-download and no
// rclone options; no --parent when the base is missing; a config version backup names its path,
// --time and --retry-lock.
func TestBackupLocalAndVersions(t *testing.T) {
	d, f := newTestDriver(t)
	dest, sec := localRepo(t)
	r := connect(t, d, dest, sec)
	f.Expect(proc.Restic, enginetest.Prefix("backup"), enginetest.Script{Stdout: enginetest.Fixture(t, "restic", "backup-initial.jsonl").Lines,
		Hook: func(call *enginetest.Call) {
			if call.Has("--parent") || call.Has("-o") || call.Has("--ignore-inode") || call.Has("--exclude-file") {
				t.Errorf("argv %q", call.Args)
			}
			if v, _ := call.Flag("--limit-upload"); v != "4096" {
				t.Errorf("--limit-upload %q", v)
			}
			if v, _ := call.Flag("--limit-download"); v != "2048" {
				t.Errorf("--limit-download %q", v)
			}
			if call.Env["RESTIC_REPOSITORY"] != dest.Target || len(call.Env) != 4 {
				t.Errorf("env %v", call.Env)
			}
		}}).Env("RESTIC_REPOSITORY", "RESTIC_PASSWORD_FILE", "RESTIC_CACHE_DIR", "RESTIC_PROGRESS_FPS")
	if _, err := r.Backup(context.Background(), BackupArgs{FilesFrom: []string{"/src"}, Tags: mediaTags(t, testEngineTag, 1, 1, 1),
		LimitUpKiB: 4096, LimitDownKiB: 2048}); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 9, 24, 12, 0, 0, 0, time.Local)
	f.Expect(proc.Restic, enginetest.Prefix("backup"), enginetest.Script{Stdout: enginetest.Fixture(t, "restic", "backup-initial.jsonl").Lines,
		Hook: func(call *enginetest.Call) {
			if v, _ := call.Flag("--time"); v != "2026-09-24 12:00:00" {
				t.Errorf("--time %q", v)
			}
			if v, _ := call.Flag("--retry-lock"); v != "30m0s" {
				t.Errorf("--retry-lock %q", v)
			}
			if call.Args[len(call.Args)-1] != "/config/staging/versions/12/.bunkarr/plex/p-1/20260924T120000Z" || call.Has("--files-from-raw") {
				t.Errorf("argv %q", call.Args)
			}
		}})
	if _, err := r.Backup(context.Background(), BackupArgs{Paths: []string{"/config/staging/versions/12/.bunkarr/plex/p-1/20260924T120000Z"},
		Tags: []string{"bunkarr"}, Time: &when, RetryLock: true}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []BackupArgs{
		{Tags: []string{"x"}},
		{FilesFrom: []string{"/a"}, Paths: []string{"/b"}, Tags: []string{"x"}},
		{Paths: []string{"/a", "/b"}, Tags: []string{"x"}},
		{FilesFrom: []string{"relative"}, Tags: []string{"x"}},
		{FilesFrom: []string{"/a"}},
		{FilesFrom: []string{"/a"}, Tags: []string{"x"}, Parent: "latest"},
		{FilesFrom: []string{"/a"}, Tags: []string{"x"}, Excludes: []string{"a\nb"}},
	} {
		if _, err := r.Backup(context.Background(), bad); err == nil {
			t.Errorf("Backup(%+v) accepted", bad)
		}
	}
}

// TestBackupOutcomes: exit 3 is a result with its error lines (the snapshot is saved), Bunkarr's
// interrupt a result, a missing summary and other exits errors.
func TestBackupOutcomes(t *testing.T) {
	args := func() BackupArgs { return BackupArgs{FilesFrom: []string{"/src2"}, Tags: []string{"bunkarr"}} }
	t.Run("partial", func(t *testing.T) {
		d, f := newTestDriver(t)
		dest, sec := s3Repo()
		f.Expect(proc.Restic, enginetest.Prefix("backup"), enginetest.Script{Exit: 3,
			Stdout: enginetest.Fixture(t, "restic", "backup-partial.jsonl").Lines,
			Stderr: enginetest.Fixture(t, "restic", "backup-partial.stderr.jsonl").Lines})
		res, err := connect(t, d, dest, sec).Backup(context.Background(), args())
		if err != nil || res.Exit != 3 || len(res.Errors) != 1 || res.Errors[0].Item != "/src2/ok/unreadable.txt" ||
			res.Errors[0].During != "archival" || !strings.Contains(res.Errors[0].Message, "permission denied") || res.SnapshotID == "" {
			t.Fatalf("%+v %v", res, err)
		}
	})
	t.Run("sigint", func(t *testing.T) {
		d, f := newTestDriver(t)
		dest, sec := s3Repo()
		script := enginetest.FixtureScript(t, "restic", "backup-sigint.stderr.txt")
		script.UntilInterrupted, script.InterruptExit = true, 1
		f.Expect(proc.Restic, enginetest.Prefix("backup"), script)
		stop := make(chan struct{})
		time.AfterFunc(20*time.Millisecond, func() { close(stop) })
		a := args()
		a.Interrupt = stop
		res, err := connect(t, d, dest, sec).Backup(context.Background(), a)
		if err != nil || !res.Interrupted {
			t.Fatalf("%+v %v", res, err)
		}
	})
	t.Run("killed", func(t *testing.T) {
		d, f := newTestDriver(t)
		dest, sec := s3Repo()
		f.Expect(proc.Restic, enginetest.Prefix("backup"), enginetest.FixtureScript(t, "restic", "backup-killed.jsonl"))
		if _, err := connect(t, d, dest, sec).Backup(context.Background(), args()); !errors.Is(err, ErrFailed) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("no summary", func(t *testing.T) {
		d, f := newTestDriver(t)
		dest, sec := s3Repo()
		f.Expect(proc.Restic, enginetest.Prefix("backup"), enginetest.Script{})
		if _, err := connect(t, d, dest, sec).Backup(context.Background(), args()); !errors.Is(err, ErrNoSummary) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("locked", func(t *testing.T) {
		d, f := newTestDriver(t)
		dest, sec := s3Repo()
		f.Expect(proc.Restic, enginetest.Prefix("backup"), enginetest.FixtureScript(t, "restic", "locked.stderr.jsonl"))
		if _, err := connect(t, d, dest, sec).Backup(context.Background(), args()); !errors.Is(err, engines.ErrLocked) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("retry budget", func(t *testing.T) {
		d, f := newTestDriver(t)
		dest, sec := s3Repo()
		script := enginetest.FixtureScript(t, "restic", "s3-bad-credentials-retrying.stderr.txt")
		script.Adjust = func(st *proc.ExitStatus) {
			st.RetryBudgetExceeded = true
			st.LastRetryLine = "Stat(<config/>) returned error, retrying after 39.2s: Stat: signature mismatch"
		}
		f.Expect(proc.Restic, enginetest.Prefix("backup"), script)
		_, err := connect(t, d, dest, sec).Backup(context.Background(), args())
		if !errors.Is(err, ErrRetryBudget) || !strings.Contains(err.Error(), "returned error, retrying") {
			t.Fatalf("%v", err)
		}
	})
}

func TestSnapshotsFilter(t *testing.T) {
	d, f := newTestDriver(t)
	dest, sec := s3Repo()
	r := connect(t, d, dest, sec)
	f.Expect(proc.Restic, enginetest.Prefix("snapshots"), enginetest.Script{Stdout: enginetest.Fixture(t, "restic", "snapshots.json").Lines,
		Hook: func(call *enginetest.Call) {
			want := []string{"snapshots", "--json", "--no-lock", "--tag", "bunkarr-dest:" + testEngineTag + ",bunkarr-job:5", id(1),
				"-o", "rclone.program=/usr/bin/rclone", "-o", "rclone.connections=6"}
			if !slices.Equal(call.Args, want) {
				t.Errorf("argv %q", call.Args)
			}
			if call.Cmd.Budget != DefaultListingBudget {
				t.Errorf("budget %s", call.Cmd.Budget)
			}
		}})
	snaps, err := r.Snapshots(context.Background(), []string{JobTag(5)}, []string{id(1)})
	if err != nil || len(snaps) != 4 {
		t.Fatalf("%d snapshots, %v", len(snaps), err)
	}
	if _, err := r.Snapshots(context.Background(), []string{"a,b"}, nil); err == nil {
		t.Fatal("a tag with a comma was accepted")
	}
	noTag := dest
	noTag.EngineTag = ""
	if _, err := connect(t, d, noTag, sec).Snapshots(context.Background(), nil, nil); err == nil {
		t.Fatal("a listing without the engine tag ran")
	}
	f.Expect(proc.Restic, enginetest.Args("snapshots", "--json", "--no-lock", "-o", "rclone.program=/usr/bin/rclone", "-o",
		"rclone.connections=6"), enginetest.Script{Stdout: []string{"[]"}})
	if all, err := r.AllSnapshots(context.Background()); err != nil || len(all) != 0 {
		t.Fatalf("%v %v", all, err)
	}
}

func TestIdentity(t *testing.T) {
	config := strings.Split("{\n  \"version\": 2,\n  \"id\": \"7c153421d95efe8bacf542bdffa1e3aeabb12bf77f5f23692860428dc9c9d987\",\n  \"chunker_polynomial\": \"3e6d\"\n}", "\n")
	for _, tc := range []struct {
		name   string
		marker string
		script *enginetest.Script
		want   error
	}{
		{"match", "", &enginetest.Script{Stdout: config}, nil},
		{"another repository", "restic:0000", &enginetest.Script{Stdout: config}, engines.ErrAnotherRepository},
		{"pending", "pending:abc", nil, engines.ErrPending},
		{"missing", "", ptr(enginetest.FixtureScript(t, "restic", "repo-missing.stderr.jsonl")), engines.ErrRepositoryMissing},
		{"wrong password", "", ptr(enginetest.FixtureScript(t, "restic", "wrong-password.stderr.jsonl")), engines.ErrWrongPassword},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, f := newTestDriver(t)
			dest, sec := s3Repo()
			if tc.marker != "" {
				dest.MarkerID = tc.marker
			}
			if tc.script != nil {
				f.Expect(proc.Restic, enginetest.Prefix("cat", "config", "--json", "--no-lock"), *tc.script)
			}
			if err := connect(t, d, dest, sec).CheckIdentity(context.Background()); !errors.Is(err, tc.want) {
				t.Fatalf("CheckIdentity = %v, want %v", err, tc.want)
			}
		})
	}
	t.Run("local filesystem type", func(t *testing.T) {
		d, f := newTestDriver(t)
		dest, sec := localRepo(t)
		dest.FSType = "nfs-that-is-not-mounted"
		if err := connect(t, d, dest, sec).CheckIdentity(context.Background()); !errors.Is(err, engines.ErrRepositoryMissing) {
			t.Fatalf("another filesystem type: %v", err)
		}
		if len(f.Calls()) != 0 {
			t.Fatal("restic ran on the wrong filesystem")
		}
	})
}

func ptr[T any](v T) *T { return &v }

func TestLs(t *testing.T) {
	d, f := newTestDriver(t)
	dest, sec := s3Repo()
	r := connect(t, d, dest, sec)
	lines := []string{
		`{"time":"2026-09-27T13:47:41Z","paths":["/src"],"id":"f71a","message_type":"snapshot","struct_type":"snapshot"}`,
		`{"name":"a","type":"file","path":"/src/a","size":3,"mtime":"2026-09-27T13:47:39.5Z","message_type":"node","struct_type":"node"}`,
		`{"name":"d","type":"dir","path":"/src/d","mtime":"2026-09-27T13:47:39Z","message_type":"node","struct_type":"node"}`,
	}
	f.Expect(proc.Restic, enginetest.Prefix("ls", "--json", "--no-lock", id(4)), enginetest.Script{Stdout: lines})
	var got []string
	if err := r.Ls(context.Background(), id(4), func(n Node) error { got = append(got, n.Type+":"+n.Path); return nil }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"file:/src/a", "dir:/src/d"}) {
		t.Fatalf("nodes %v", got)
	}
	stop := errors.New("enough")
	f.Expect(proc.Restic, enginetest.Prefix("ls"), enginetest.Script{Stdout: lines})
	if err := r.Ls(context.Background(), id(4), func(Node) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("a stopped listing: %v", err)
	}
	if err := r.Ls(context.Background(), "latest", func(Node) error { return nil }); err == nil {
		t.Fatal("ls of latest")
	}
}

// TestForgetChunks: forget by id in chunks of 100 with --retry-lock, never a policy (S24).
func TestForgetChunks(t *testing.T) {
	d, f := newTestDriver(t)
	dest, sec := s3Repo()
	r := connect(t, d, dest, sec)
	var ids []string
	for i := range 250 {
		ids = append(ids, id(i+1))
	}
	f.Expect(proc.Restic, enginetest.Prefix("forget", "--json", "--retry-lock", "30m0s"), enginetest.Script{}).Times(2)
	f.Expect(proc.Restic, enginetest.Prefix("forget"), logFatal("unable to create lock"))
	done, err := r.Forget(context.Background(), ids)
	if err == nil || len(done) != 200 || !slices.Equal(done, ids[:200]) {
		t.Fatalf("forgot %d, %v", len(done), err)
	}
	calls := f.CallsOf(proc.Restic, "forget")
	for i, n := range []int{100, 100, 50} {
		got := 0
		for _, a := range calls[i].Args {
			if len(a) == 64 {
				got++
			}
		}
		if got != n {
			t.Errorf("chunk %d names %d ids, want %d", i, got, n)
		}
	}
	if _, err := r.Forget(context.Background(), []string{"--keep-last=1"}); err == nil {
		t.Fatal("a flag as an id was accepted")
	}
}

func logFatal(msg string) enginetest.Script {
	return enginetest.Script{Exit: 1, Stderr: []string{`{"message_type":"exit_error","code":1,"message":"Fatal: ` + msg + `"}`}}
}

func TestPruneCheckRestoreDump(t *testing.T) {
	d, f := newTestDriver(t)
	dest, sec := localRepo(t)
	r := connect(t, d, dest, sec)
	ctx := context.Background()
	f.Expect(proc.Restic, enginetest.Args("prune", "--max-unused", "10%", "--retry-lock", "30m0s", "--pack-size", "16",
		"--limit-download", "100"), enginetest.FixtureScript(t, "restic", "prune-json-flag.txt"))
	if err := r.Prune(ctx, PruneArgs{MaxUnused: "10%", LimitDownKiB: 100}); err != nil {
		t.Fatal(err)
	}
	f.Expect(proc.Restic, enginetest.Args("check", "--json", "--retry-lock", "30m0s", "--read-data-subset", "3/7"),
		enginetest.FixtureScript(t, "restic", "check.json"))
	if res, err := r.Check(ctx, CheckArgs{Subset: "3/7"}); err != nil || res.NumErrors != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	f.Expect(proc.Restic, enginetest.Args("check", "--json", "--retry-lock", "30m0s", "--read-data"), enginetest.Script{Exit: 1,
		Stdout: []string{`{"message_type":"summary","num_errors":2,"broken_packs":["abc"],"suggest_repair_index":true,"suggest_prune":false}`},
		Stderr: []string{`{"message_type":"exit_error","code":1,"message":"Fatal: repository contains errors"}`}})
	if res, err := r.Check(ctx, CheckArgs{ReadData: true}); err != nil || res.NumErrors != 2 || !res.SuggestRepairIndex {
		t.Fatalf("%+v %v", res, err)
	}
	f.Expect(proc.Restic, enginetest.Args("check", "--json", "--retry-lock", "30m0s"), logFatal("unable to create lock"))
	if _, err := r.Check(ctx, CheckArgs{}); !errors.Is(err, ErrFailed) {
		t.Fatalf("check without a summary: %v", err)
	}
	if _, err := r.Check(ctx, CheckArgs{Subset: "25%"}); err == nil {
		t.Fatal("a percentage subset was accepted")
	}
	f.Expect(proc.Restic, enginetest.Prefix("restore"), enginetest.Script{Hook: func(call *enginetest.Call) {
		want := []string{"restore", id(5) + ":/mnt/media", "--target", "/config/staging/verify-job9", "--no-lock", "--include-file", "<run>/include"}
		if got := withRun(call); !slices.Equal(got, want) {
			t.Errorf("argv %q", got)
		}
		if got := string(call.DataFile("include")); got != "/Movies/A (2001)/A.mkv\n/odd/star\\*[$]x.mkv\n" {
			t.Errorf("include %q", got)
		}
	}})
	if err := r.Restore(ctx, RestoreArgs{Snapshot: id(5), Subpath: "/mnt/media", Target: "/config/staging/verify-job9",
		Include: []string{"Movies/A (2001)/A.mkv", "odd/star*$x.mkv"}}); err != nil {
		t.Fatal(err)
	}
	if err := r.Restore(ctx, RestoreArgs{Snapshot: id(5), Subpath: "/mnt/media", Target: "/t", Include: []string{"a\nb"}}); err == nil {
		t.Fatal("an include with a line break was accepted")
	}
	f.Expect(proc.Restic, enginetest.Args("dump", "--no-lock", id(5), "/v/manifest.json"), enginetest.Script{Stdout: []string{`{"a":`, `1}`}})
	var b bytes.Buffer
	if err := r.Dump(ctx, id(5), "/v/manifest.json", &b, 1<<20); err != nil || b.String() != "{\"a\":\n1}\n" {
		t.Fatalf("dump %q %v", b.String(), err)
	}
}

// TestGuardedUnlock is §6.7's lock inspection, both ways: a lock with this host name, newer than
// the process start and not a live child refuses the unlock; an older one, another host's and a
// live child's allow it; a lock that cannot be read refuses it while it is still there.
func TestGuardedUnlock(t *testing.T) {
	start := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	lock := func(host string, at time.Time, pid int) enginetest.Script {
		return enginetest.Script{Stdout: strings.Split(`{
  "time": "`+at.Format(time.RFC3339Nano)+`",
  "exclusive": false,
  "hostname": "`+host+`",
  "username": "root",
  "pid": `+strconv.Itoa(pid)+`
}`, "\n")}
	}
	for _, tc := range []struct {
		name     string
		locks    map[string]enginetest.Script
		relist   []string
		wantErr  bool
		unlocked bool
	}{
		{"no locks", nil, nil, false, true},
		{"our host, older than our start", map[string]enginetest.Script{id(1): lock("bunkarr-tower", start.Add(-time.Hour), 55)}, nil, false, true},
		{"our host, newer, not our child", map[string]enginetest.Script{id(1): lock("bunkarr-tower", start.Add(time.Minute), 55)}, nil, true, false},
		{"our host, newer, our live child", map[string]enginetest.Script{id(1): lock("bunkarr-tower", start.Add(time.Minute), 4242)}, nil, false, true},
		{"another host", map[string]enginetest.Script{id(1): lock("457fa3ca0fed", start.Add(time.Minute), 55)}, nil, false, true},
		{"unreadable and still there", map[string]enginetest.Script{id(1): logFatal("load lock: EOF")}, []string{id(1)}, true, false},
		{"unreadable and gone", map[string]enginetest.Script{id(1): logFatal("no such file")}, []string{}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, f := newTestDriver(t)
			dest, sec := s3Repo()
			r := connect(t, d, dest, sec)
			r.rt.Now = func() time.Time { return start.Add(3 * time.Hour) }
			ids := slices.Sorted(maps.Keys(tc.locks))
			f.Expect(proc.Restic, enginetest.Prefix("list", "locks", "--no-lock"), enginetest.Script{Stdout: ids})
			for _, lid := range ids {
				f.Expect(proc.Restic, enginetest.Prefix("cat", "lock", lid, "--no-lock"), tc.locks[lid])
			}
			if tc.relist != nil {
				f.Expect(proc.Restic, enginetest.Prefix("list", "locks"), enginetest.Script{Stdout: tc.relist})
			}
			unlock := f.Expect(proc.Restic, enginetest.Prefix("unlock"), enginetest.Script{}).AnyTimes()
			err := r.GuardedUnlock(context.Background(), "bunkarr-tower", start, func(pid int) bool { return pid == 4242 })
			if (err != nil) != tc.wantErr {
				t.Fatalf("GuardedUnlock = %v", err)
			}
			if tc.wantErr && tc.name == "our host, newer, not our child" &&
				(!errors.Is(err, ErrForeignLock) || !strings.Contains(err.Error(), "another restic process with host name bunkarr-tower is using this repository")) {
				t.Fatalf("message: %v", err)
			}
			if (unlock.Calls() == 1) != tc.unlocked {
				t.Fatalf("unlock ran %d times", unlock.Calls())
			}
		})
	}
}

func TestIsLiveChild(t *testing.T) {
	if IsLiveChild(0) || IsLiveChild(-1) || IsLiveChild(1) {
		t.Fatal("init or an invalid pid is a live child")
	}
}

// TestLongLines: ls --json's snapshot line holds the whole path list; a line the exec layer cut
// (proc.MaxLineBytes) is skipped by the read-back, whose nodes are short (listings:
// TestSnapshotsLongListing).
func TestLongLines(t *testing.T) {
	d, f := newTestDriver(t)
	dest, sec := s3Repo()
	r := connect(t, d, dest, sec)
	snapshotLine := `{"time":"2026-09-27T13:47:41Z","paths":["` + strings.Repeat("/mnt/media/dir", proc.MaxLineBytes/8) + `"],"message_type":"snapshot"}`
	f.Expect(proc.Restic, enginetest.Prefix("ls"), enginetest.Script{Stdout: []string{snapshotLine,
		`{"name":"a","type":"file","path":"/mnt/media/a","size":1,"mtime":"2026-09-27T13:47:39Z","message_type":"node"}`}})
	n := 0
	if err := r.Ls(context.Background(), id(1), func(Node) error { n++; return nil }); err != nil || n != 1 {
		t.Fatalf("ls with a cut snapshot line: %d nodes, %v", n, err)
	}
}

// TestGuardedUnlockOwnLeftover: a lock one of this process's own restic children left behind (an
// interrupt stops restic's rclone backend with it, so restic cannot remove the lock) does not
// refuse the unlock; the same PID outside that child's run still does.
func TestGuardedUnlockOwnLeftover(t *testing.T) {
	start := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	run := time.Date(2026, 9, 27, 13, 0, 0, 0, time.UTC)
	ownChildren.Lock()
	ownChildren.spans[31337] = []childSpan{{start: run, end: run.Add(time.Minute)}}
	ownChildren.Unlock()
	t.Cleanup(func() {
		ownChildren.Lock()
		delete(ownChildren.spans, 31337)
		ownChildren.Unlock()
	})
	for _, tc := range []struct {
		at       time.Time
		unlocked bool
	}{
		{run.Add(time.Second), true},
		{run.Add(2 * time.Hour), false},
	} {
		d, f := newTestDriver(t)
		dest, sec := s3Repo()
		f.Expect(proc.Restic, enginetest.Prefix("list", "locks"), enginetest.Script{Stdout: []string{id(1)}})
		f.Expect(proc.Restic, enginetest.Prefix("cat", "lock"), enginetest.Script{Stdout: []string{
			`{"time":"` + tc.at.Format(time.RFC3339Nano) + `","exclusive":false,"hostname":"bunkarr-tower","pid":31337}`}})
		unlock := f.Expect(proc.Restic, enginetest.Prefix("unlock"), enginetest.Script{}).AnyTimes()
		err := connect(t, d, dest, sec).GuardedUnlock(context.Background(), "bunkarr-tower", start, func(int) bool { return false })
		if (err == nil) != tc.unlocked || (unlock.Calls() == 1) != tc.unlocked {
			t.Fatalf("lock at %s: %v, unlock ran %d times", tc.at, err, unlock.Calls())
		}
	}
	if leftByOwnChild(31337, time.Time{}) || leftByOwnChild(1, run) {
		t.Fatal("a lock outside the child's run, or of another PID, counted as its own")
	}
}

// longListingRepo is a fake repository for listings too large for one line: restic snapshots
// --json prints one line (ignoring --tag next to explicit ids, as restic does), list snapshots
// one id per line, cat snapshot indented JSON without the id.
type longListingRepo struct {
	snaps []Snapshot
}

func (l *longListingRepo) install(f *enginetest.FakeRunner) {
	byID := map[string]Snapshot{}
	for _, s := range l.snaps {
		byID[s.ID] = s
	}
	f.Handle(proc.Restic, "snapshots", func(c *enginetest.Call) enginetest.Script {
		filter, _ := c.Flag("--tag")
		var ids []string
		for _, a := range c.Args[1:] {
			if fullIDRe.MatchString(a) {
				ids = append(ids, a)
			}
		}
		out := []Snapshot{}
		for _, s := range l.snaps {
			if len(ids) > 0 && slices.Contains(ids, s.ID) || len(ids) == 0 && (filter == "" || s.HasTags(strings.Split(filter, ",")...)) {
				out = append(out, s)
			}
		}
		b, _ := json.Marshal(out)
		return enginetest.Script{Stdout: []string{string(b)}}
	})
	f.Handle(proc.Restic, "list", func(c *enginetest.Call) enginetest.Script {
		if c.Args[1] != "snapshots" {
			return enginetest.Script{}
		}
		var ids []string
		for _, s := range l.snaps {
			ids = append(ids, s.ID)
		}
		return enginetest.Script{Stdout: ids}
	})
	f.Handle(proc.Restic, "cat", func(c *enginetest.Call) enginetest.Script {
		s := byID[c.Args[2]]
		b, _ := json.MarshalIndent(map[string]any{"time": s.Time, "tree": s.Tree, "paths": s.Paths, "hostname": s.Hostname,
			"username": "root", "tags": s.Tags, "program_version": "restic 0.18.1"}, "", "  ")
		return enginetest.Script{Stdout: strings.Split(string(b), "\n")}
	})
}

// TestSnapshotsLongListing: restic snapshots --json prints the whole listing as one line, and
// every snapshot holds its whole include list, so a large library's listing reaches the exec
// layer's cap (proc.MaxLineBytes). It is read in parts instead of failing (a failing listing
// stopped every sync and the retention job that could shrink it): the ids, then the snapshots in
// halving parts, and a snapshot too long for one line with restic cat snapshot. The tag filter
// still holds, and ids still narrow the listing.
func TestSnapshotsLongListing(t *testing.T) {
	paths := func(n int, prefix string) []string {
		var out []string
		for i := range n {
			out = append(out, fmt.Sprintf("/mnt/media/Movies/%s Some Long Movie Title (%d)", prefix, 1900+i))
		}
		return out
	}
	base := time.Date(2026, 9, 1, 2, 0, 0, 0, time.UTC)
	ours := mediaTags(t, testEngineTag, 3, 5, 1)
	repo := &longListingRepo{}
	for i := 1; i <= 6; i++ {
		s := Snapshot{ID: id(i), ShortID: id(i)[:8], Time: base.Add(time.Duration(7-i) * time.Hour), Tree: id(100 + i),
			Hostname: Host, Tags: ours, Paths: paths(6000, strconv.Itoa(i))}
		switch i {
		case 3:
			s.Paths = paths(25000, "held") // one snapshot longer than a line on its own
		case 6:
			s.Tags = mediaTags(t, otherTag, 3, 5, 1) // another row of the same repository
		}
		repo.snaps = append(repo.snaps, s)
	}
	d, f := newTestDriver(t)
	dest, sec := s3Repo()
	r := connect(t, d, dest, sec)
	repo.install(f)

	snaps, err := r.Snapshots(context.Background(), []string{JobTag(5)}, nil)
	if err != nil {
		t.Fatalf("a listing longer than one line: %v", err)
	}
	var got []string
	for _, s := range snaps {
		got = append(got, s.ID)
		if s.ID == id(3) && (len(s.Paths) != 25000 || s.Tree != id(103) || !s.HasTags(ours...)) {
			t.Fatalf("the snapshot read with cat snapshot: %d paths, tree %s, tags %v", len(s.Paths), s.Tree, s.Tags)
		}
		if s.ID != id(3) && len(s.Paths) != 6000 {
			t.Fatalf("snapshot %s has %d paths", s.ShortID, len(s.Paths))
		}
	}
	if want := []string{id(5), id(4), id(3), id(2), id(1)}; !slices.Equal(got, want) {
		t.Fatalf("listed %v, want %v (oldest first, without the other row's)", got, want)
	}
	if cats := f.CallsOf(proc.Restic, "cat"); len(cats) != 1 || cats[0].Args[1] != "snapshot" || cats[0].Args[2] != id(3) {
		t.Fatalf("cat snapshot ran for %v", cats)
	}

	one, err := r.Snapshots(context.Background(), nil, []string{id(3)})
	if err != nil || len(one) != 1 || one[0].ID != id(3) {
		t.Fatalf("narrowed to one long snapshot: %v %v", one, err)
	}
	if other, err := r.Snapshots(context.Background(), nil, []string{id(6)}); err != nil || len(other) != 1 {
		// Small enough for one line: restic's own answer, unchanged.
		t.Fatalf("narrowed to the other row's snapshot: %v %v", other, err)
	}
	all, err := r.AllSnapshots(context.Background())
	if err != nil || len(all) != 6 {
		t.Fatalf("all snapshots: %d, %v", len(all), err)
	}
}

// TestGuardedUnlockRecentSameHost: a lock with this host name that is older than this process's
// start may still be a live process's (restic refreshes its lock every 5 minutes; it may have
// been refreshed just before this process started) — the case §6.7 guards against, in the other
// order. A job waits until the lock is LiveLockAge old and inspects the locks again: a lock left
// by a previous run is then unlocked, a live process's refreshed lock (newer than this process)
// refuses. Outside a job (the unlock endpoint) the unlock is refused at once.
func TestGuardedUnlockRecentSameHost(t *testing.T) {
	start := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	lockLine := func(at time.Time) []string {
		return []string{`{"time":"` + at.Format(time.RFC3339Nano) + `","exclusive":false,"hostname":"bunkarr-tower","username":"root","pid":77}`}
	}
	for _, tc := range []struct {
		name      string
		job       int64
		refreshed bool
		wantErr   string
		unlocked  bool
		slept     time.Duration
	}{
		{"a previous run's lock", 9, false, "", true, 27*time.Minute + time.Second},
		{"a live process refreshes its lock", 9, true, "another restic process with host name bunkarr-tower", false, 27*time.Minute + time.Second},
		{"the unlock endpoint", 0, false, "counts as stale from 2026-09-27T12:29:00Z", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, f := newTestDriver(t)
			dest, sec := s3Repo()
			now := start.Add(2 * time.Minute)
			var slept time.Duration
			d.sleep = func(_ context.Context, dur time.Duration) error {
				slept += dur
				now = now.Add(dur)
				return nil
			}
			r, err := d.Connect(dest, sec, engines.Runtime{JobID: tc.job, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			// Lock 1 was refreshed a minute before this process started; a live process replaces
			// it by lock 2 at its next refresh.
			f.Handle(proc.Restic, "list", func(*enginetest.Call) enginetest.Script {
				if tc.refreshed && slept > 0 {
					return enginetest.Script{Stdout: []string{id(2)}}
				}
				return enginetest.Script{Stdout: []string{id(1)}}
			})
			f.Handle(proc.Restic, "cat", func(c *enginetest.Call) enginetest.Script {
				if c.Args[2] == id(2) {
					return enginetest.Script{Stdout: lockLine(start.Add(4 * time.Minute))}
				}
				return enginetest.Script{Stdout: lockLine(start.Add(-time.Minute))}
			})
			unlocks := 0
			f.Handle(proc.Restic, "unlock", func(*enginetest.Call) enginetest.Script { unlocks++; return enginetest.Script{} })
			err = r.GuardedUnlock(context.Background(), "bunkarr-tower", start, func(int) bool { return false })
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (!errors.Is(err, ErrForeignLock) || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("GuardedUnlock = %v, want %q", err, tc.wantErr)
			}
			if (unlocks == 1) != tc.unlocked || slept != tc.slept {
				t.Fatalf("unlock ran %d times after waiting %s, want unlocked %v after %s", unlocks, slept, tc.unlocked, tc.slept)
			}
		})
	}
}
