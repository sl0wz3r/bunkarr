//go:build enginebin

package restic

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/bwlimit"
	"github.com/sl0wz3r/bunkarr/internal/engines/enginetest"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
	"github.com/sl0wz3r/bunkarr/internal/engines/rclone"
)

// The real-binary tests of the restic driver (make test-engines, docs/design/phase4.md §14.4),
// with restic 0.18.1 reaching MinIO and the SFTP server through its rclone backend (D21) and a
// local repository. Each confirms an assumption of §17; the measured answers are written next to
// the assertions:
//
//   - --files-from-raw takes whole directories and files together, --parent works across
//     snapshots with different path lists (the unchanged files count as files_unmodified), and
//     --exclude-if-present bunkarr.key leaves the directory out (TestRealLifecycle).
//   - restic ls --json gives path, type, size and mtime with nanoseconds, equal to the
//     filesystem's (TestRealLifecycle); on 200k files (400 directories, local repository) the
//     backup took 2.6 s and ls --json 1.5 s (TestRealLsScale, outside -short): the read-back is
//     cheap next to the backup, and batchFiles needs no lowering.
//   - a file of a whole directory deleted after the scan is simply not in the snapshot: exit 0,
//     no error line (read-back case a). Case (b), an unreadable file, cannot be made as root and
//     is skipped there.
//   - restore <snapshot>:<subfolder> --include-file restores just the included file, relative
//     to the subfolder.
//   - RCLONE_BWLIMIT reaches rclone serve restic: 8 MiB at 2 MiB/s took 4.06 s through the
//     backend (TestRealInterruptAndLimits asserts at least 90 % of size/rate).
//   - after a SIGINT at 5 s of a 24 MiB upload at 2 MiB/s, the next backup re-uploaded all of it
//     (data_added 25168473 of 25165824 bytes): nothing uploaded before the interrupt is reused,
//     as after the spike's kill -9 (TestRealInterruptAndLimits); batches never rely on reuse.
//   - an interrupt signals the process group, so rclone serve restic dies with restic and
//     restic cannot remove its lock: the lock stays, and GuardedUnlock recognizes it as left by
//     one of this process's children (children.go).
//   - list locks and cat lock give the lock's host name, pid, time and exclusive flag.
//   - restic snapshots --json prints the whole listing as one line, with each snapshot's whole
//     include list; restic list snapshots prints one id per line and restic cat snapshot indented
//     JSON without the id, so a listing longer than proc.MaxLineBytes is read in parts
//     (TestRealLongListing).
//   - check takes an exclusive lock (spike check-read-data-subset.txt: "create exclusive lock
//     for repository"), so GuardedUnlock runs before it in the retention and verify jobs.

func realDriver(t *testing.T) (*Driver, enginetest.RealEnvironment) {
	t.Helper()
	e := enginetest.RealEnv(t)
	runner := proc.NewExecRunner(proc.ExecOptions{ResticPath: e.ResticPath, RclonePath: e.RclonePath, TermAfter: 20 * time.Second})
	dirs := proc.NewRunDirs(t.TempDir(), proc.RunDirOptions{})
	return &Driver{Runner: runner, RunDirs: dirs, Rclone: &rclone.Driver{Runner: runner, RunDirs: dirs}, CacheRoot: t.TempDir(),
		RclonePath: e.RclonePath}, e
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// repos returns an S3, an SFTP and a local restic destination.
func repos(t *testing.T, e enginetest.RealEnvironment) map[string]func() (engines.Destination, engines.Secrets) {
	finish := func(d engines.Destination, s engines.Secrets) (engines.Destination, engines.Secrets) {
		d.Engine, d.Encryption, d.EngineTag, d.PackSizeMiB, d.Transfers = engines.Restic, engines.EncryptionRestic, randomHex(16), 4, 4
		s.Encryption = engines.EncryptionSecret{ResticPassword: "enginebin-restic-" + randomHex(8)}
		return d, s
	}
	return map[string]func() (engines.Destination, engines.Secrets){
		"s3": func() (engines.Destination, engines.Secrets) {
			return finish(e.S3(71, "enginebin/restic-"+randomHex(4), false, rclone.ObscureRandom))
		},
		"sftp": func() (engines.Destination, engines.Secrets) {
			return finish(e.SFTP(72, "restic-"+randomHex(4), false, rclone.ObscureRandom))
		},
		"local": func() (engines.Destination, engines.Secrets) {
			return finish(engines.Destination{ID: 73, Kind: engines.Local, Target: filepath.Join(t.TempDir(), "repo")}, engines.Secrets{})
		},
	}
}

// createRepo initializes a repository and returns its bound Repo.
func createRepo(t *testing.T, d *Driver, dest engines.Destination, s engines.Secrets) *Repo {
	t.Helper()
	if dest.Kind == engines.SFTP {
		// restic's rclone backend creates the repository's directory; its parent must exist.
		c, err := d.Rclone.Connect(dest, s, engines.Runtime{})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Rcat(t.Context(), ".bunkarr/links.tsv", []byte{}); err != nil {
			t.Fatal(err)
		}
	}
	res, err := d.Create(t.Context(), dest, s, false)
	if err != nil || !res.Initialized {
		t.Fatalf("create %s: %+v %v", dest.Kind, res, err)
	}
	dest.MarkerID = res.MarkerID
	r, err := d.Connect(dest, s, engines.Runtime{JobID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.CheckIdentity(t.Context()); err != nil {
		t.Fatalf("identity after create: %v", err)
	}
	return r
}

func writeFile(t *testing.T, root, rel string, content []byte) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

// snapshotFiles reads back a snapshot: path -> node.
func snapshotFiles(t *testing.T, r *Repo, id string) map[string]Node {
	t.Helper()
	out := map[string]Node{}
	if err := r.Ls(t.Context(), id, func(n Node) error {
		if n.Type == "file" {
			out[n.Path] = n
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRealLifecycle(t *testing.T) {
	d, e := realDriver(t)
	for name, mk := range repos(t, e) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			dest, s := mk()
			r := createRepo(t, d, dest, s)
			root := t.TempDir()
			live := []string{"Movies/A/a.mkv", "Movies/A/a.srt", "Movies/B/b.mkv", "Movies/B/c.mkv", "TV/x.mkv"}
			for _, f := range live {
				writeFile(t, root, f, []byte("content of "+f))
			}
			writeFile(t, root, "config/bunkarr.key", []byte("key"))
			writeFile(t, root, "config/bunkarr.db", []byte("db"))
			tags := func(batch int) []string {
				return mediaTags(t, dest.EngineTag, 1, 5, batch)
			}
			// Batch 1: TV/x.mkv held (not included), the rest compressed to whole directories.
			inc := CompressIncludes(root, live, func(rel string) bool { return rel != "TV/x.mkv" })
			if !slices.Equal(inc, []string{root + "/Movies"}) {
				t.Fatalf("include list %q", inc)
			}
			b1, err := r.Backup(ctx, BackupArgs{FilesFrom: append(inc, root+"/config"), Tags: tags(1)})
			if err != nil || b1.Exit != 0 {
				t.Fatalf("batch 1: %+v %v", b1, err)
			}
			held := snapshotFiles(t, r, b1.SnapshotID)
			for _, f := range live[:4] {
				n, ok := held[root+"/"+f]
				fi, _ := os.Stat(filepath.Join(root, f))
				if !ok || n.Size != fi.Size() || n.MtimeNs != fi.ModTime().UnixNano() {
					t.Errorf("%s: node %+v, file %d bytes mtime %d (ls --json precision)", f, n, fi.Size(), fi.ModTime().UnixNano())
				}
			}
			if _, ok := held[root+"/TV/x.mkv"]; ok {
				t.Error("a file outside the include list was backed up")
			}
			if _, ok := held[root+"/config/bunkarr.db"]; ok {
				t.Error("--exclude-if-present bunkarr.key did not leave the config directory out")
			}
			// Batch 2: another path list, --parent the first snapshot.
			b2, err := r.Backup(ctx, BackupArgs{FilesFrom: CompressIncludes(root, live, func(string) bool { return true }), Tags: tags(2),
				Parent: b1.SnapshotID})
			if err != nil || b2.Summary.FilesUnmodified < 4 || b2.Summary.FilesNew != 1 {
				t.Fatalf("batch 2 with a parent of another path list: %+v %v", b2.Summary, err)
			}
			// Read-back case (a): a file of a whole directory deleted after the scan.
			if err := os.Remove(filepath.Join(root, "Movies/B/c.mkv")); err != nil {
				t.Fatal(err)
			}
			b3, err := r.Backup(ctx, BackupArgs{FilesFrom: []string{root + "/Movies/B"}, Tags: tags(3), Parent: b2.SnapshotID})
			if err != nil || b3.Exit != 0 || len(b3.Errors) != 0 {
				t.Fatalf("case (a): %+v %v", b3, err)
			}
			if got := snapshotFiles(t, r, b3.SnapshotID); len(got) != 1 {
				t.Fatalf("case (a) holds %v", got)
			}
			snaps, err := r.Snapshots(ctx, []string{JobTag(5)}, nil)
			if err != nil || len(snaps) != 3 {
				t.Fatalf("snapshots %v %v", snaps, err)
			}
			for _, sn := range snaps {
				if info, ok := ParseTags(sn.Tags, dest.EngineTag); !ok || info.SourceID != 1 || sn.Hostname != Host {
					t.Errorf("snapshot %+v", sn)
				}
			}
			// Restore one file of a subfolder.
			target := filepath.Join(t.TempDir(), "restore")
			if err := r.Restore(ctx, RestoreArgs{Snapshot: b2.SnapshotID, Subpath: root + "/Movies", Target: target, Include: []string{"A/a.srt"}}); err != nil {
				t.Fatal(err)
			}
			var restored []string
			_ = filepath.WalkDir(target, func(p string, de fs.DirEntry, err error) error {
				if err == nil && !de.IsDir() {
					rel, _ := filepath.Rel(target, p)
					restored = append(restored, rel)
				}
				return err
			})
			if !slices.Equal(restored, []string{"A/a.srt"}) {
				t.Fatalf("restored %q", restored)
			}
			var dump bytes.Buffer
			if err := r.Dump(ctx, b2.SnapshotID, root+"/Movies/A/a.srt", &dump, 1<<20); err != nil || dump.String() != "content of Movies/A/a.srt\n" {
				t.Fatalf("dump %q %v", dump.String(), err)
			}
			// Forget by id, prune, check a subset.
			if err := r.GuardedUnlock(ctx, "enginebin-host", time.Now().Add(-time.Hour), nil); err != nil {
				t.Fatalf("unlock without locks: %v", err)
			}
			if done, err := r.Forget(ctx, []string{b1.SnapshotID}); err != nil || len(done) != 1 {
				t.Fatalf("forget %v %v", done, err)
			}
			if err := r.Prune(ctx, PruneArgs{MaxUnused: "0%"}); err != nil {
				t.Fatal(err)
			}
			res, err := r.Check(ctx, CheckArgs{Subset: "1/2"})
			if err != nil || res.NumErrors != 0 {
				t.Fatalf("check %+v %v", res, err)
			}
			if snaps, _ := r.Snapshots(ctx, nil, nil); len(snaps) != 2 {
				t.Fatalf("after forget: %d snapshots", len(snaps))
			}
		})
	}
}

// TestRealExcludeTranslation backs up a source with the translated exclude files and compares
// the snapshot with the catalog: restic excluded nothing the catalog includes (§14.4, DS-7).
func TestRealExcludeTranslation(t *testing.T) {
	d, e := realDriver(t)
	dest, s := repos(t, e)["local"]()
	r := createRepo(t, d, dest, s)
	root := filepath.Join(t.TempDir(), "My $HOME [media]*")
	files := []string{"Movies/A/a.mkv", "Movies/A/a.nfo", "Movies/A/A.NFO", "Movies/A/.DS_Store", "Movies/A/.ds_store",
		"Movies/@eaDir/thumb.jpg", "@eaDir", "Extras/x.mkv", "Movies/Extras/x.mkv", "Show/S01/e1.srt", "x/Show/S01/e1.srt",
		"top.mkv", "sub/top.mkv", "dl/m.part", "dl/m.PART", "Thumbs.db", "a/THUMBS.DB", "x/._res", "$RECYCLE.BIN/x",
		"costs$5.txt", "star*.mkv", "b[1].mkv", `back\slash.mkv`, "Media/file.mkv", "trailing /x.mkv"}
	for _, f := range files {
		writeFile(t, root, f, []byte(f))
	}
	own := []string{"*.nfo", "/Extras/", "Show/S01/*.srt", "/top.mkv", "$RECYCLE.BIN", "costs[$]5.txt", "Media", `b\[1\].mkv`}
	defaults := catalog.DefaultExcludePatterns()
	ownPatterns := catalog.ParseExcludePatterns(own)
	conv := func(in []catalog.ExcludePattern) []ExcludePattern {
		out := make([]ExcludePattern, len(in))
		for i, p := range in {
			out[i] = ExcludePattern(p)
		}
		return out
	}
	ex, iex, dropped := TranslateExcludes(TranslateInput{SourceRoot: root, ConfigDir: "/config", Defaults: conv(defaults), Own: conv(ownPatterns)})
	t.Logf("dropped: %q", dropped)
	b, err := r.Backup(t.Context(), BackupArgs{FilesFrom: []string{root}, Excludes: ex, IExcludes: iex, Tags: mediaTags(t, dest.EngineTag, 1, 1, 1)})
	if err != nil {
		t.Fatal(err)
	}
	held := snapshotFiles(t, r, b.SnapshotID)
	all := append(slices.Clone(defaults), ownPatterns...)
	excludedByRestic := 0
	for _, f := range files {
		_, inSnapshot := held[root+"/"+f]
		included := !catalog.ExcludedWithin(all, f, false)
		if included && !inSnapshot {
			t.Errorf("restic excluded %q, which the catalog includes", f)
		}
		if !inSnapshot {
			excludedByRestic++
		}
	}
	if excludedByRestic < 10 {
		t.Errorf("restic excluded only %d files: the translation is not effective", excludedByRestic)
	}
}

// TestRealInterruptAndLimits: RCLONE_BWLIMIT reaches rclone serve restic; a SIGINT'd backup
// leaves the repository usable and its lock is inspected; what the next backup reuses is logged.
func TestRealInterruptAndLimits(t *testing.T) {
	testStart := time.Now()
	d, e := realDriver(t)
	dest, s := repos(t, e)["s3"]()
	dest.Bandwidth = bwlimit.Config{UploadKiBps: 2048}
	r := createRepo(t, d, dest, s)
	ctx := t.Context()
	root := t.TempDir()
	writeFile(t, root, "small.bin", randomBytesN(8<<20))
	start := time.Now()
	b, err := r.Backup(ctx, BackupArgs{FilesFrom: []string{root + "/small.bin"}, Tags: mediaTags(t, dest.EngineTag, 1, 1, 1)})
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	t.Logf("8 MiB at 2 MiB/s through rclone serve restic: %s (data added %d)", elapsed, b.Summary.DataAdded)
	if elapsed < 3600*time.Millisecond {
		t.Fatalf("the backup took %s: RCLONE_BWLIMIT did not reach rclone serve restic", elapsed)
	}
	// Interrupt a 24 MiB backup after 5 s; meanwhile inspect its lock.
	writeFile(t, root, "big.bin", randomBytesN(24<<20))
	stop := make(chan struct{})
	lockSeen := make(chan string, 1)
	go func() {
		time.Sleep(2500 * time.Millisecond)
		ids, err := r.ListLocks(ctx)
		if err != nil || len(ids) == 0 {
			lockSeen <- fmt.Sprintf("no lock listed (%v)", err)
			return
		}
		l, err := r.CatLock(ctx, ids[0])
		host, _ := os.Hostname()
		if err != nil || l.Hostname != host || l.PID <= 0 || l.Exclusive || l.Time.IsZero() {
			lockSeen <- fmt.Sprintf("lock %+v %v (host %s)", l, err, host)
			return
		}
		lockSeen <- ""
	}()
	time.AfterFunc(5*time.Second, func() { close(stop) })
	res, err := r.Backup(ctx, BackupArgs{FilesFrom: []string{root + "/big.bin"}, Tags: mediaTags(t, dest.EngineTag, 1, 2, 1), Interrupt: stop})
	if err != nil || !res.Interrupted {
		t.Fatalf("interrupted backup: %+v %v", res, err)
	}
	if msg := <-lockSeen; msg != "" {
		t.Errorf("lock inspection: %s", msg)
	}
	// The interrupt signals the process group, so rclone serve restic dies with restic, and
	// restic cannot remove its lock (measured: the lock stays). GuardedUnlock recognizes it as
	// left by one of this process's children and removes it.
	ids, err := r.ListLocks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("locks left by the interrupted backup: %d", len(ids))
	host, _ := os.Hostname()
	if err := r.GuardedUnlock(ctx, host, testStart, nil); err != nil {
		t.Fatalf("unlock after our own interrupted backup: %v", err)
	}
	if ids, err := r.ListLocks(ctx); err != nil || len(ids) != 0 {
		t.Fatalf("locks after the guarded unlock: %v %v", ids, err)
	}
	fast := dest
	fast.Bandwidth = bwlimit.Config{}
	r2, err := d.Connect(fast, s, engines.Runtime{JobID: 3})
	if err != nil {
		t.Fatal(err)
	}
	again, err := r2.Backup(ctx, BackupArgs{FilesFrom: []string{root + "/big.bin"}, Tags: mediaTags(t, dest.EngineTag, 1, 3, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("after a SIGINT at 5 s of a 24 MiB upload at 2 MiB/s, the next backup added %d of %d bytes (reuse: %v)",
		again.Summary.DataAdded, 24<<20, again.Summary.DataAdded < 20<<20)
	if got := snapshotFiles(t, r2, again.SnapshotID); got[root+"/big.bin"].Size != 24<<20 {
		t.Fatalf("the resumed backup holds %+v", got)
	}
}

func randomBytesN(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// TestRealLsScale times restic ls --json on a 200k-file snapshot (§17), outside -short.
func TestRealLsScale(t *testing.T) {
	if testing.Short() {
		t.Skip("200k files: not in -short")
	}
	d, e := realDriver(t)
	dest, s := repos(t, e)["local"]()
	r := createRepo(t, d, dest, s)
	root := t.TempDir()
	const dirs, per = 400, 500
	for i := range dirs {
		dir := filepath.Join(root, fmt.Sprintf("d%03d", i))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for j := range per {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%03d.nfo", j)), []byte{byte(j)}, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	start := time.Now()
	b, err := r.Backup(t.Context(), BackupArgs{FilesFrom: []string{root}, Tags: mediaTags(t, dest.EngineTag, 1, 1, 1)})
	if err != nil {
		t.Fatal(err)
	}
	backup := time.Since(start)
	start = time.Now()
	n := len(snapshotFiles(t, r, b.SnapshotID))
	t.Logf("200k files: backup %s, ls --json %s for %d files", backup.Round(time.Millisecond), time.Since(start).Round(time.Millisecond), n)
	if n != dirs*per {
		t.Fatalf("ls listed %d files", n)
	}
}

// TestRealVersionStore: Put, List, ReadFile and Fetch of a config version on a local repository.
func TestRealVersionStore(t *testing.T) {
	d, e := realDriver(t)
	dest, s := repos(t, e)["local"]()
	r := createRepo(t, d, dest, s)
	ctx := context.Background()
	staging := t.TempDir()
	vs := NewVersionStore(r, staging, nil)
	dir := filepath.Join(staging, "plex-job3")
	manifest := []byte(`{"jobId":3,"integrationId":2}` + "\r\n")
	db := randomBytesN(1 << 20)
	writeFile(t, dir, "manifest.json", manifest)
	writeFile(t, dir, "com.plexapp.plugins.library.db", db)
	when := time.Date(2026, 9, 27, 2, 0, 0, 0, time.Local)
	ref, err := vs.Put(ctx, engines.PutVersion{Kind: engines.VersionPlexDB, LogicalPath: ".bunkarr/plex/plex-2/20260927T020000Z-job3", Dir: dir,
		JobID: 3, IntegrationID: 2, Time: when})
	if err != nil {
		t.Fatal(err)
	}
	list, err := vs.List(ctx, ".bunkarr/plex")
	if err != nil || len(list) != 1 || !list[0].Time.Equal(when) || list[0].Files["com.plexapp.plugins.library.db"] != 1<<20 {
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
	if !strings.HasPrefix(list[0].LogicalPath, ".bunkarr/plex/") {
		t.Fatal(list[0].LogicalPath)
	}
}

// TestRealLongListing: a listing longer than the exec layer's line cap (one snapshot with 12000
// include paths of about 150 bytes, 1.8 MB on its own) is read in parts: the ids, restic
// snapshots --json <ids>, and restic cat snapshot for the long one. Another row's snapshot in the
// same repository stays out of the row's listing.
func TestRealLongListing(t *testing.T) {
	d, e := realDriver(t)
	all := repos(t, e)
	for _, name := range []string{"local", "s3"} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			dest, s := all[name]()
			r := createRepo(t, d, dest, s)
			root := t.TempDir()
			pad := strings.Repeat("x", 100)
			var many []string
			for i := range 12000 {
				p := filepath.Join(root, "Movies", fmt.Sprintf("%s %05d", pad, i))
				if err := os.MkdirAll(p, 0o755); err != nil {
					t.Fatal(err)
				}
				many = append(many, p)
			}
			writeFile(t, root, "Small/a.mkv", []byte("a"))
			small := []string{root + "/Small"}
			var ids []string
			for i, inc := range [][]string{small, many, small} {
				res, err := r.Backup(ctx, BackupArgs{FilesFrom: inc, Tags: mediaTags(t, dest.EngineTag, 1, 5, i+1)})
				if err != nil || res.Exit != 0 {
					t.Fatalf("backup %d: %+v %v", i+1, res, err)
				}
				ids = append(ids, res.SnapshotID)
			}
			other := dest
			other.EngineTag = randomHex(16)
			ro, err := d.Connect(other, s, engines.Runtime{JobID: 2})
			if err != nil {
				t.Fatal(err)
			}
			if res, err := ro.Backup(ctx, BackupArgs{FilesFrom: small, Tags: mediaTags(t, other.EngineTag, 1, 6, 1)}); err != nil || res.Exit != 0 {
				t.Fatalf("the other row's backup: %+v %v", res, err)
			}
			if _, err := r.snapshotsOnce(ctx, []string{DestTag(dest.EngineTag)}, nil); !errors.Is(err, ErrListingTooLarge) {
				t.Fatalf("the listing fit one line (%v): the test does not reach the parts", err)
			}
			start := time.Now()
			snaps, err := r.Snapshots(ctx, []string{JobTag(5)}, nil)
			if err != nil {
				t.Fatalf("a listing longer than a line: %v", err)
			}
			t.Logf("listing in parts: %d snapshots in %s", len(snaps), time.Since(start))
			var got []string
			for _, sn := range snaps {
				got = append(got, sn.ID)
				if info, ok := ParseTags(sn.Tags, dest.EngineTag); !ok || info.JobID != 5 || sn.Hostname != Host || sn.Time.IsZero() || sn.Tree == "" {
					t.Errorf("snapshot %s: tags %v host %q time %s tree %q", sn.ShortID, sn.Tags, sn.Hostname, sn.Time, sn.Tree)
				}
			}
			if !slices.Equal(got, ids) {
				t.Fatalf("listed %v, want %v", got, ids)
			}
			if n := len(snaps[1].Paths); n != 12000 || snaps[1].ShortID != ids[1][:8] {
				t.Fatalf("the long snapshot: %d paths, short id %q", n, snaps[1].ShortID)
			}
			if one, err := r.Snapshots(ctx, nil, []string{ids[1]}); err != nil || len(one) != 1 || len(one[0].Paths) != 12000 {
				t.Fatalf("the long snapshot by id: %d, %v", len(one), err)
			}
			if every, err := r.AllSnapshots(ctx); err != nil || len(every) != 4 {
				t.Fatalf("all snapshots: %d, %v", len(every), err)
			}
		})
	}
}
