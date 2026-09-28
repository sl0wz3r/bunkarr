//go:build enginebin

package rclone

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/bwlimit"
	"github.com/sl0wz3r/bunkarr/internal/engines/enginetest"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
)

// The real-binary tests of the rclone driver (make test-engines, docs/design/phase4.md §14.4),
// against MinIO and an OpenSSH SFTP server. Each confirms an assumption of §17 before the code
// relies on it; the measured answers (rclone 1.74.1, MinIO, OpenSSH 10) are written next to the
// assertions:
//
//   - --max-delete and the backup dir: displacing one object into --backup-dir costs ONE
//     --max-delete unit on S3 (a server-side copy plus one delete; --max-delete 0 refuses it with
//     exit 7) and NONE on SFTP (a server-side rename, not a delete) (TestRealBackupDirMaxDelete).
//   - moveto onto an existing object replaces it on S3 and SFTP, as §7.3 assumes (TestRealMoves).
//   - move --files-from-raw with a listed file that is gone moves the others and exits 0
//     (TestRealMoves).
//   - check --download --combined marks a changed file '*' and a file missing at the destination
//     '+' (rclone's own source; the spike index's note had '-' and '+' the other way round).
//   - backend cleanup BKDEST:<bucket>/<prefix> removes only the unfinished multipart uploads
//     under the prefix (TestRealBackendCleanupScope).
//   - --max-duration with --cutoff-mode soft exits 10, keeps the finished file and starts no
//     other (TestRealCutoff).

func realDriver(t *testing.T) (*Driver, enginetest.RealEnvironment) {
	t.Helper()
	e := enginetest.RealEnv(t)
	runner := proc.NewExecRunner(proc.ExecOptions{ResticPath: e.ResticPath, RclonePath: e.RclonePath, TermAfter: 5 * time.Second})
	return &Driver{Runner: runner, RunDirs: proc.NewRunDirs(t.TempDir(), proc.RunDirOptions{}), RetryWait: time.Second}, e
}

// unique returns a fresh prefix for one test.
func unique(t *testing.T, name string) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("enginebin/%s-%x", name, b)
}

// created returns a connected, created destination.
func created(t *testing.T, d *Driver, dest engines.Destination, s engines.Secrets) *Conn {
	t.Helper()
	ctx := t.Context()
	res, err := d.Create(ctx, dest, s, false)
	if err != nil {
		t.Fatalf("create %s: %v", dest.Kind, err)
	}
	dest.MarkerID = res.MarkerID
	c, err := d.Connect(dest, s, engines.Runtime{JobID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CheckMarker(ctx); err != nil {
		t.Fatalf("marker after create: %v", err)
	}
	return c
}

func writeFile(t *testing.T, dir, rel string, content []byte) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// destinations returns the plain S3, crypt S3 and SFTP destinations of a test.
func destinations(t *testing.T, e enginetest.RealEnvironment, name string) map[string]func() (engines.Destination, engines.Secrets) {
	return map[string]func() (engines.Destination, engines.Secrets){
		"s3": func() (engines.Destination, engines.Secrets) {
			return e.S3(21, unique(t, name), false, ObscureRandom)
		},
		"s3-crypt": func() (engines.Destination, engines.Secrets) {
			return e.S3(22, unique(t, name), true, ObscureRandom)
		},
		"sftp": func() (engines.Destination, engines.Secrets) {
			dest, s := e.SFTP(23, strings.ReplaceAll(unique(t, name), "/", "-"), false, ObscureRandom)
			return dest, s
		},
	}
}

// TestRealBackupDirMaxDelete: copy --backup-dir moves the replaced version into retention, and
// how many --max-delete units that costs (§17).
func TestRealBackupDirMaxDelete(t *testing.T) {
	d, e := realDriver(t)
	for name, mk := range destinations(t, e, "maxdel") {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			dest, s := mk()
			if dest.Kind == engines.SFTP {
				mkdirSFTP(t, d, dest, s)
			}
			c := created(t, d, dest, s)
			src := t.TempDir()
			writeFile(t, src, "a.mkv", []byte("version one"))
			writeFile(t, src, "b/b.srt", []byte("subtitle"))
			run1 := ".bunkarr/retention/20260927T010000Z-job1"
			res, err := c.Copy(ctx, CopyInput{SourceRoot: src, DestFolder: "Movies", Files: []string{"a.mkv", "b/b.srt"}, RetentionDir: run1, MaxDelete: 2})
			if err != nil || res.Code != 0 {
				t.Fatalf("initial copy: %+v %v", res, err)
			}
			got, err := c.StatMany(ctx, "Movies", []string{"a.mkv", "b/b.srt", "missing.mkv"})
			if err != nil || len(got) != 2 || got["a.mkv"].Size != 11 {
				t.Fatalf("statMany after the copy: %+v %v", got, err)
			}
			// An update displaces the old version. On S3 the backup-dir move is a server-side copy
			// plus a delete, and the delete counts: refused with --max-delete 0 (exit 7), done with
			// 1, so one unit per displaced object and n = the batch's copy and update items holds.
			// On SFTP it is a server-side rename, which --max-delete does not count at all (0
			// units): the displacement goes through with --max-delete 0. A rename keeps the
			// version, so nothing is lost there either way.
			writeFile(t, src, "a.mkv", []byte("version two, longer"))
			run2 := ".bunkarr/retention/20260927T020000Z-job2"
			res, err = c.Copy(ctx, CopyInput{SourceRoot: src, DestFolder: "Movies", Files: []string{"a.mkv"}, RetentionDir: run2, MaxDelete: 0})
			switch {
			case dest.Kind == engines.SFTP && err != nil:
				t.Fatalf("sftp: an update with --max-delete 0: %v (a rename was counted as a delete)", err)
			case dest.Kind == engines.SFTP:
				t.Logf("sftp: the backup-dir move costs no --max-delete unit (events %+v)", res.Events)
			case !errors.Is(err, ErrMaxDelete):
				t.Fatalf("an update with --max-delete 0: %v, want exit 7 (max-delete)", err)
			default:
				res, err = c.Copy(ctx, CopyInput{SourceRoot: src, DestFolder: "Movies", Files: []string{"a.mkv"}, RetentionDir: run2, MaxDelete: 1})
				if err != nil {
					t.Fatalf("an update with --max-delete 1: %+v %v (one displaced object costs more than one unit)", res, err)
				}
			}
			t.Logf("%s update events: %+v", name, res.Events)
			live, _ := c.StatMany(ctx, "Movies", []string{"a.mkv"})
			kept, _ := c.StatMany(ctx, run2+"/Movies", []string{"a.mkv"})
			if live["a.mkv"].Size != 19 || kept["a.mkv"].Size != 11 {
				t.Fatalf("live %+v, retention %+v", live, kept)
			}
			// An object Bunkarr never wrote is displaced the same way (S2), not overwritten.
			other := t.TempDir()
			writeFile(t, other, "c.nfo", []byte("unmanaged"))
			if err := c.CopyTo(ctx, filepath.Join(other, "c.nfo"), "Movies/c.nfo"); err != nil {
				t.Fatal(err)
			}
			writeFile(t, src, "c.nfo", []byte("from the source"))
			run3 := ".bunkarr/retention/20260927T030000Z-job3"
			if _, err := c.Copy(ctx, CopyInput{SourceRoot: src, DestFolder: "Movies", Files: []string{"c.nfo"}, RetentionDir: run3, MaxDelete: 1}); err != nil {
				t.Fatal(err)
			}
			kept, _ = c.StatMany(ctx, run3+"/Movies", []string{"c.nfo"})
			if kept["c.nfo"].Size != 9 {
				t.Fatalf("the unmanaged object was not displaced into retention: %+v", kept)
			}
		})
	}
}

// mkdirSFTP creates the SFTP destination's directory (Create requires an existing path): rcat
// creates the directories on the way, and .bunkarr/links.tsv is Bunkarr's own file.
func mkdirSFTP(t *testing.T, d *Driver, dest engines.Destination, s engines.Secrets) {
	t.Helper()
	c, err := d.Connect(dest, s, engines.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Rcat(t.Context(), ".bunkarr/links.tsv", []byte{}); err != nil {
		t.Fatal(err)
	}
}

// TestRealMoves: move into retention with a file that is gone, and moveto onto an existing
// object (§17).
func TestRealMoves(t *testing.T) {
	d, e := realDriver(t)
	for _, name := range []string{"s3", "sftp"} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			dest, s := destinations(t, e, "moves")[name]()
			if dest.Kind == engines.SFTP {
				mkdirSFTP(t, d, dest, s)
			}
			c := created(t, d, dest, s)
			src := t.TempDir()
			for _, f := range []string{"a", "b", "sub/c"} {
				writeFile(t, src, f, []byte("content of "+f))
			}
			if _, err := c.Copy(ctx, CopyInput{SourceRoot: src, DestFolder: "M", Files: []string{"a", "b", "sub/c"},
				RetentionDir: ".bunkarr/retention/20260927T010000Z-job1", MaxDelete: 3}); err != nil {
				t.Fatal(err)
			}
			run := ".bunkarr/retention/20260927T020000Z-job2"
			res, err := c.Move(ctx, MoveInput{SrcDir: "M", DstDir: run + "/M", Files: []string{"a", "gone", "sub/c"}, MaxDelete: 3})
			if err != nil {
				t.Fatalf("move with a gone file: %+v %v", res, err)
			}
			t.Logf("%s: move with a gone file exits %d with events %+v", name, res.Code, res.Events)
			moved, _ := c.StatMany(ctx, run+"/M", []string{"a", "sub/c", "gone"})
			left, _ := c.StatMany(ctx, "M", []string{"a", "b", "sub/c"})
			if len(moved) != 2 || len(left) != 1 {
				t.Fatalf("moved %v, left %v", moved, left)
			}
			// moveto onto an existing object replaces it (§7.3 displaces first for that reason).
			writeFile(t, src, "big", bytes.Repeat([]byte("x"), 100))
			if err := c.CopyTo(ctx, filepath.Join(src, "big"), "M/big"); err != nil {
				t.Fatal(err)
			}
			if _, err := c.MoveTo(ctx, "M/b", "M/big"); err != nil {
				t.Fatalf("moveto onto an existing object: %v", err)
			}
			got, _ := c.StatMany(ctx, "M", []string{"b", "big"})
			if _, ok := got["b"]; ok || got["big"].Size != int64(len("content of b")) {
				t.Fatalf("moveto did not replace the target: %+v", got)
			}
			if err := c.Rmdirs(ctx, "M", true); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestRealCryptAndCheck: crypt with strict_names (a wrong password is unreadable, not empty),
// the obscured passwords read back, and check --download --combined.
func TestRealCryptAndCheck(t *testing.T) {
	d, e := realDriver(t)
	ctx := t.Context()
	prefix := unique(t, "crypt")
	dest, s := e.S3(31, prefix, true, ObscureRandom)
	c := created(t, d, dest, s)
	src := t.TempDir()
	writeFile(t, src, "same.mkv", []byte("same content"))
	writeFile(t, src, "changed.mkv", []byte("original one"))
	writeFile(t, src, "gone.mkv", []byte("will vanish"))
	if _, err := c.Copy(ctx, CopyInput{SourceRoot: src, DestFolder: "M", Files: []string{"same.mkv", "changed.mkv", "gone.mkv"},
		RetentionDir: ".bunkarr/retention/20260927T010000Z-job1", MaxDelete: 3}); err != nil {
		t.Fatal(err)
	}
	// Damage: equal size, other content; and one object removed at the destination.
	writeFile(t, src, "changed.mkv", []byte("modified one"))
	if err := c.DeleteFile(ctx, ".bunkarr/retention/20260927T010000Z-job1/nothing"); err != nil && !errors.Is(err, ErrPathNotFound) &&
		!errors.Is(err, ErrObjectNotFound) {
		t.Logf("deletefile of a missing retained file: %v", err)
	}
	if _, err := c.Move(ctx, MoveInput{SrcDir: "M", DstDir: ".bunkarr/retention/20260927T020000Z-job2/M", Files: []string{"gone.mkv"}, MaxDelete: 1}); err != nil {
		t.Fatal(err)
	}
	marks, err := c.Check(ctx, CheckInput{SourceRoot: src, DestFolder: "M", Files: []string{"same.mkv", "changed.mkv", "gone.mkv"}})
	if err != nil {
		t.Fatal(err)
	}
	if marks["same.mkv"] != MarkMatch || marks["changed.mkv"] != MarkDiffer || marks["gone.mkv"] != MarkMissingOnDest {
		t.Fatalf("combined marks %q (want = * +)", marks)
	}
	// Another crypt password over the same storage: unreadable under strict_names.
	ws := s
	ws.Encryption = engines.EncryptionSecret{CryptPassword: "another-crypt-password-1", CryptPassword2: "another-crypt-salt-1"}
	ws.Obscured = map[string]string{engines.FieldCryptPassword: ObscureRandom(ws.Encryption.CryptPassword),
		engines.FieldCryptPassword2: ObscureRandom(ws.Encryption.CryptPassword2)}
	res, err := d.Test(ctx, dest, ws)
	if err != nil || res.Marker != engines.MarkerUnreadable || res.OK {
		t.Fatalf("a wrong crypt password: %+v %v", res, err)
	}
	// The right one reads the marker.
	res, err = d.Test(ctx, dest, s)
	if err != nil || res.Marker != engines.MarkerOK || !res.OK {
		t.Fatalf("the right crypt password: %+v %v", res, err)
	}
}

// TestRealCutoff: --max-duration with --cutoff-mode soft (§9.2).
func TestRealCutoff(t *testing.T) {
	d, e := realDriver(t)
	ctx := t.Context()
	dest, s := e.S3(41, unique(t, "cutoff"), false, ObscureRandom)
	dest.Bandwidth = bwlimit.Config{UploadKiBps: 4096}
	c := created(t, d, dest, s)
	src := t.TempDir()
	writeFile(t, src, "one.bin", randomBytes(8<<20))
	writeFile(t, src, "two.bin", randomBytes(8<<20))
	dest.Transfers = 1
	c.dest.Transfers = 1
	res, err := c.Copy(ctx, CopyInput{SourceRoot: src, DestFolder: "M", Files: []string{"one.bin", "two.bin"},
		RetentionDir: ".bunkarr/retention/20260927T010000Z-job1", MaxDelete: 2, MaxDuration: time.Second})
	if err != nil || !res.Cutoff {
		t.Fatalf("cutoff: %+v %v", res, err)
	}
	got, _ := c.StatMany(ctx, "M", []string{"one.bin", "two.bin"})
	if len(got) != 1 {
		t.Fatalf("after the cutoff: %+v (want the first file only)", got)
	}
}

// TestRealBackendCleanupScope: backend cleanup on BKDEST:<bucket>/<prefix> removes only the
// unfinished multipart uploads under the prefix (§7.5, §17). The uploads are left by rclone
// processes killed with SIGKILL mid-upload (rclone aborts them itself on SIGINT).
func TestRealBackendCleanupScope(t *testing.T) {
	d, e := realDriver(t)
	ctx := t.Context()
	ours, s := e.S3(51, unique(t, "cleanup"), false, ObscureRandom)
	other, _ := e.S3(52, unique(t, "cleanup-other"), false, ObscureRandom)
	big := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(big, randomBytes(24<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, dest := range []engines.Destination{ours, other} {
		env, err := Env(EnvInput{Dest: dest, Secrets: s})
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(ctx, e.RclonePath, "copyto", big, StorageRoot(dest)+"/big.bin", "--s3-chunk-size", "5M",
			"--s3-upload-cutoff", "5M", "--bwlimit", "2M", "--s3-upload-concurrency", "1")
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(4 * time.Second)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	uploads := func(path string) string {
		env, _ := Env(EnvInput{Dest: ours, Secrets: s})
		cmd := exec.CommandContext(ctx, e.RclonePath, "backend", "list-multipart-uploads", "BKDEST:"+path)
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("list-multipart-uploads: %v: %s", err, out)
		}
		return string(out)
	}
	before := uploads(e.S3Bucket)
	if !strings.Contains(before, ours.Remote.S3.Prefix+"/big.bin") || !strings.Contains(before, other.Remote.S3.Prefix+"/big.bin") {
		t.Skipf("no unfinished uploads were left to clean up (the server may have finished or aborted them): %s", before)
	}
	c, err := d.Connect(ours, s, engines.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	// The production floor is 7 days; the scope is what this test checks, with a 1 s age.
	cmd := command{words: []string{"backend", "cleanup"}, args: func(*proc.RunDir) []string {
		return append([]string{StorageRoot(ours), "-o", "max-age=1s"}, jsonLog...)
	}}
	time.Sleep(2 * time.Second)
	out, err := c.run(ctx, cmd)
	if err != nil || check(cmd, out) != nil {
		t.Fatalf("backend cleanup: %v %v", err, check(cmd, out))
	}
	after := uploads(e.S3Bucket)
	if !strings.Contains(after, other.Remote.S3.Prefix+"/big.bin") {
		t.Fatalf("backend cleanup removed an upload under another prefix: %s", after)
	}
	if strings.Contains(after, ours.Remote.S3.Prefix+"/big.bin") {
		// MinIO answers ListMultipartUploads with a prefix that is not a whole object name with
		// nothing, so a cleanup scoped to a prefix finds nothing to remove there. The scope (never
		// another prefix) holds; the removal is confirmed against real S3 and B2 by hand (§14.6
		// item 9).
		scoped := uploads(StorageRoot(ours)[len("BKDEST:"):])
		if strings.Contains(scoped, "big.bin") {
			t.Fatalf("the upload under the prefix is listed there but was not removed: %s", scoped)
		}
		t.Logf("MinIO lists no multipart uploads under a prefix; the scoped cleanup removed nothing and left the other prefix alone")
	}
}

// TestRealVersionStore: Put, List, ReadFile, Fetch and Remove against crypt on MinIO.
func TestRealVersionStore(t *testing.T) {
	d, e := realDriver(t)
	ctx := t.Context()
	dest, s := e.S3(61, unique(t, "versions"), true, ObscureRandom)
	c := created(t, d, dest, s)
	isVersion := func(rel string) bool {
		parts := strings.Split(rel, "/")
		return len(parts) == 4 && parts[0] == ".bunkarr" && parts[1] == "plex" && versionTimeRe.MatchString(parts[3])
	}
	vs := NewVersionStore(c, isVersion)
	dir := t.TempDir()
	manifest := []byte(`{"jobId":3,"integrationId":2}` + "\r\n")
	db := randomBytes(1 << 20)
	writeFile(t, dir, "manifest.json", manifest)
	writeFile(t, dir, "com.plexapp.plugins.library.db", db)
	ref, err := vs.Put(ctx, engines.PutVersion{Kind: engines.VersionPlexDB, LogicalPath: ".bunkarr/plex/plex-2/20260927T020000Z-job3", Dir: dir,
		JobID: 3, IntegrationID: 2, Time: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	list, err := vs.List(ctx, ".bunkarr/plex")
	if err != nil || len(list) != 1 || !list[0].Complete || list[0].JobID != 3 || list[0].IntegrationID != 2 ||
		list[0].Files["com.plexapp.plugins.library.db"] != 1<<20 {
		t.Fatalf("list %+v %v", list, err)
	}
	got, err := vs.ReadFile(ctx, ref, "manifest.json", 1<<20)
	if err != nil || !bytes.Equal(got, manifest) {
		t.Fatalf("ReadFile %q %v", got, err)
	}
	out := t.TempDir()
	if err := vs.Fetch(ctx, ref, "com.plexapp.plugins.library.db", out); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "com.plexapp.plugins.library.db")); !bytes.Equal(b, db) {
		t.Fatal("Fetch is not byte-exact")
	}
	if err := vs.Remove(ctx, nil, ref, engines.VersionPlexDB); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if list, err := vs.List(ctx, ".bunkarr/plex"); err != nil || len(list) != 0 {
		t.Fatalf("after Remove: %+v %v", list, err)
	}
}

// TestRealConfFileRoundTrip: the recovery kit's rclone.conf reaches what the jobs wrote (§5.2).
// rclone 1.74.1 reads every value ConfFile writes back as the value the jobs' environment passed
// verbatim, white space, quotes, ';', '#', '=' and '%' inside a value included, and lists a file
// a crypt job copied under such a prefix. The values checkConfValue refuses are the ones rclone's
// config file reads as another value: trimmed leading and trailing white space (NBSP too), a
// leading ` or """ quote stripped, a %(name)s reference expanded.
func TestRealConfFileRoundTrip(t *testing.T) {
	d, e := realDriver(t)
	ctx := t.Context()
	dump := func(t *testing.T, conf string) map[string]map[string]string {
		t.Helper()
		dir := t.TempDir()
		p := filepath.Join(dir, "rclone.conf")
		if err := os.WriteFile(p, []byte(conf), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(ctx, e.RclonePath, "config", "dump")
		cmd.Env = []string{"RCLONE_CONFIG=" + p, "HOME=" + dir}
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("rclone config dump: %v", err)
		}
		var got map[string]map[string]string
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("rclone config dump: %v", err)
		}
		return got
	}

	// Values rclone would reread: written raw, as a renderer without the check would.
	for _, v := range []string{"/x/backups ", "/x/backups\t", "/x/backups\u00a0", " /x/backups", "`/x/backups`",
		`"""/x/backups"""`, "/x/%(type)s"} {
		got := dump(t, "[a]\ntype = alias\nremote = "+v+"\n")["a"]["remote"]
		if got == v {
			t.Errorf("rclone read %q back unchanged; checkConfValue need not refuse it", v)
		}
		if checkConfValue("remote", v) == nil {
			t.Errorf("checkConfValue accepts %q, which rclone reads as %q", v, got)
		}
	}

	// A crypt job under a prefix with white space, quotes and '%' inside, and the kit over it.
	dest, s := e.S3(35, unique(t, "conf")+"/my backups ; # = %d `x` \"q\" x", true, ObscureRandom)
	c := created(t, d, dest, s)
	src := t.TempDir()
	writeFile(t, src, "a b.mkv", []byte("kit round trip"))
	if _, err := c.Copy(ctx, CopyInput{SourceRoot: src, DestFolder: "M", Files: []string{"a b.mkv"},
		RetentionDir: ".bunkarr/retention/20260927T010000Z-job1", MaxDelete: 1}); err != nil {
		t.Fatal(err)
	}
	sftp, ss := e.SFTP(36, "", false, ObscureRandom)
	for name, tc := range map[string]struct {
		dest engines.Destination
		s    engines.Secrets
	}{"s3-crypt": {dest, s}, "sftp": {sftp, ss}} {
		conf, err := ConfFile(tc.dest, tc.s, "/kit/bunkarr_known_hosts")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		env, err := Env(EnvInput{Dest: tc.dest, Secrets: tc.s, KnownHostsPath: "/kit/bunkarr_known_hosts"})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := dump(t, conf)
		n := 0
		for section, remote := range map[string]string{ConfDest: proc.RemoteDest, ConfCrypt: proc.RemoteCrypt} {
			for key, v := range got[section] {
				want, ok := env[proc.RemoteEnv(remote, strings.ToUpper(key))]
				if key == "remote" {
					v, want = strings.TrimPrefix(v, ConfDest+":"), strings.TrimPrefix(want, proc.RemoteDest+":")
				}
				if !ok || v != want {
					t.Errorf("%s: rclone reads [%s] %s = %q from the kit; the jobs pass %q", name, section, key, v, want)
				}
				n++
			}
		}
		want := 0
		for _, l := range strings.Split(conf, "\n") {
			if l != "" && !strings.HasPrefix(l, "[") {
				want++
			}
		}
		if n != want {
			t.Errorf("%s: rclone read %d values of the kit's %d", name, n, want)
		}
	}
	dir := t.TempDir()
	conf, err := ConfFile(dest, s, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rclone.conf"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, e.RclonePath, "lsf", "-R", ConfCrypt+":M")
	cmd.Env = []string{"RCLONE_CONFIG=" + filepath.Join(dir, "rclone.conf"), "HOME=" + dir}
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "a b.mkv" {
		t.Fatalf("the kit's rclone.conf lists %q (%v), want the job's file", out, err)
	}
}
