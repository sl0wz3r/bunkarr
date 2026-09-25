package plexdb

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/logging"
)

// stagingDir returns a fresh (not yet created) staging directory.
func stagingDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "staging", "plexdb-job1")
}

// checkStaged checks a staged file's recorded size and sha256 against its content.
func checkStaged(t *testing.T, f StagedFile) {
	t.Helper()
	size, sum, err := hashPath(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	if size != f.Size || sum != f.SHA256 || len(sum) != 64 {
		t.Fatalf("%s: staged %d %s, recorded %d %s", f.Name, size, sum, f.Size, f.SHA256)
	}
}

// sideFiles lists the SQLite side files (-wal, -shm, -journal) in dir.
func sideFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	for _, pat := range []string{"*-wal", "*-shm", "*-journal"} {
		m, err := filepath.Glob(filepath.Join(dir, pat))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, m...)
	}
	return out
}

func TestBackupUnderConcurrentWriter(t *testing.T) {
	data := plexDataDir(t)
	lib := makeLibrary(t, dbPath(data, LibraryDB), 3000, 3000)
	defer lib.Close()
	writePrefs(t, data)

	ctx, stop := context.WithCancel(context.Background())
	var committed atomic.Int64
	done := make(chan error, 1)
	go func() {
		for n := 1; ctx.Err() == nil; n++ {
			if err := writeTx(lib, n); err != nil {
				done <- err
				return
			}
			committed.Store(int64(n))
		}
		done <- nil
	}()
	waitUntil(t, "writer commits", func() bool { return committed.Load() >= 20 })

	overlapped := 0
	for i := range 5 {
		before := committed.Load()
		staging := stagingDir(t)
		res, err := Backup(context.Background(), BackupOptions{DataPath: data, StagingDir: staging})
		if err != nil {
			stop()
			t.Fatalf("backup %d: %v", i, err)
		}
		after := committed.Load()
		if after > before {
			overlapped++
		}
		f, ok := res.File(LibraryDB)
		if !ok || f.Method != MethodOnlineBackup || res.Method != MethodOnlineBackup || f.Attempts != 1 {
			t.Fatalf("backup %d: library %+v (a live WAL database must use the plain mode=ro backup)", i, f)
		}
		checkStaged(t, f)
		last := invariants(t, f.Path)
		if last < before || last > after {
			t.Fatalf("backup %d holds sequence %d, committed before/after the backup: %d/%d", i, last, before, after)
		}
		rep, err := Verify(context.Background(), f.Path)
		if err != nil || !rep.OK() {
			t.Fatalf("backup %d: verify %+v, %v", i, rep, err)
		}
		if !rep.PlexLibrary || rep.MetadataItems != 3000+last || rep.MediaParts != rep.MetadataItems {
			t.Fatalf("backup %d: counts %+v, sequence %d", i, rep, last)
		}
		if sf := sideFiles(t, staging); len(sf) != 0 {
			t.Fatalf("backup %d left side files in staging: %v", i, sf)
		}
	}
	stop()
	if err := <-done; err != nil {
		t.Fatalf("writer: %v", err)
	}
	if overlapped == 0 {
		t.Fatal("the writer committed nothing while a backup ran: the test did not exercise a live copy")
	}
}

// waitUntil polls cond for up to 10 s.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestBackupCleanStopped(t *testing.T) {
	data := plexDataDir(t)
	lib := makeLibrary(t, dbPath(data, LibraryDB), 500, 200)
	for n := 1; n <= 10; n++ {
		if err := writeTx(lib, n); err != nil {
			t.Fatal(err)
		}
	}
	if err := lib.Close(); err != nil {
		t.Fatal(err)
	}
	blobs := makeLibrary(t, dbPath(data, BlobsDB), 1, 10)
	if err := blobs.Close(); err != nil {
		t.Fatal(err)
	}
	writePrefs(t, data)
	dbDir := filepath.Dir(dbPath(data, LibraryDB))
	if sf := sideFiles(t, dbDir); len(sf) != 0 {
		t.Fatalf("fixture: a cleanly closed database left %v", sf)
	}
	before := dirSnapshot(t, data)

	staging := stagingDir(t)
	res, err := Backup(context.Background(), BackupOptions{DataPath: data, StagingDir: staging})
	if err != nil {
		t.Fatal(err)
	}
	if got := dirSnapshot(t, data); !slices.Equal(got, before) {
		t.Fatalf("the backup changed Plex's directory:\nbefore %v\nafter  %v", before, got)
	}
	names := []string{}
	for _, f := range res.Files {
		names = append(names, f.Name)
		checkStaged(t, f)
	}
	if !slices.Equal(names, []string{LibraryDB, BlobsDB, PreferencesXML}) {
		t.Fatalf("staged %v", names)
	}
	lf, _ := res.File(LibraryDB)
	if lf.Method != MethodOnlineBackupImmutable || res.Method != MethodOnlineBackupImmutable || res.SQLiteVersion == "" {
		t.Fatalf("result %+v", res)
	}
	if last := invariants(t, lf.Path); last != 10 {
		t.Fatalf("copy holds sequence %d, want 10", last)
	}
	for _, name := range []string{LibraryDB, BlobsDB} {
		f, _ := res.File(name)
		rep, err := Verify(context.Background(), f.Path)
		if err != nil || !rep.OK() {
			t.Fatalf("verify %s: %+v, %v", name, rep, err)
		}
	}
	pf, _ := res.File(PreferencesXML)
	fi, err := os.Stat(pf.Path)
	if err != nil || fi.Mode().Perm() != 0o600 || pf.Method != MethodCopy {
		t.Fatalf("staged Preferences.xml %+v, %v", pf, err)
	}
	if !logging.ContainsSecret("token=" + testToken) {
		t.Fatal("the PlexOnlineToken of Preferences.xml was not registered as a secret")
	}
	if fi, err := os.Stat(staging); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("staging directory: %v, %v", fi, err)
	}
}

func TestBackupImmutableGuard(t *testing.T) {
	tests := []struct {
		name string
		// change runs after each copy (attempt counts from 1)
		change       func(t *testing.T, db string, attempt int)
		wantErr      error
		wantAttempts int
		wantMethod   string
	}{
		{
			name:         "unchanged",
			change:       func(*testing.T, string, int) {},
			wantAttempts: 1, wantMethod: MethodOnlineBackupImmutable,
		},
		{
			name: "modified during the first copy",
			change: func(t *testing.T, db string, attempt int) {
				if attempt == 1 {
					touch(t, db, time.Now().Add(time.Hour))
				}
			},
			wantAttempts: 2, wantMethod: MethodOnlineBackupImmutable,
		},
		{
			name: "Plex started during the first copy",
			change: func(t *testing.T, db string, attempt int) {
				if attempt == 1 {
					if err := os.WriteFile(db+"-wal", nil, 0o644); err != nil {
						t.Fatal(err)
					}
				}
			},
			wantAttempts: 2, wantMethod: MethodOnlineBackup,
		},
		{
			name: "replaced during the first copy",
			change: func(t *testing.T, db string, attempt int) {
				if attempt == 1 {
					raw, err := os.ReadFile(db)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(db+".new", raw, 0o644); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(db+".new", db); err != nil {
						t.Fatal(err)
					}
				}
			},
			wantAttempts: 2, wantMethod: MethodOnlineBackupImmutable,
		},
		{
			name: "modified during every copy",
			change: func(t *testing.T, db string, attempt int) {
				touch(t, db, time.Now().Add(time.Duration(attempt)*time.Hour))
			},
			wantErr: ErrUnstable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := plexDataDir(t)
			lib := makeLibrary(t, dbPath(data, LibraryDB), 50, 50)
			if err := lib.Close(); err != nil {
				t.Fatal(err)
			}
			writePrefs(t, data)
			staging := stagingDir(t)
			attempts := 0
			o := BackupOptions{DataPath: data, StagingDir: staging, afterCopy: func(name string, attempt int) {
				if name == LibraryDB {
					attempts = attempt
					tt.change(t, dbPath(data, LibraryDB), attempt)
				}
			}}
			res, err := Backup(context.Background(), o)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) || attempts != DefaultAttempts {
					t.Fatalf("err = %v after %d attempts, want %v after %d", err, attempts, tt.wantErr, DefaultAttempts)
				}
				if left, _ := os.ReadDir(staging); len(left) != 0 {
					t.Fatalf("a failed backup left %v in staging", left)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			f, _ := res.File(LibraryDB)
			if f.Attempts != tt.wantAttempts || f.Method != tt.wantMethod {
				t.Fatalf("library %+v, want %d attempts with %s", f, tt.wantAttempts, tt.wantMethod)
			}
			invariants(t, f.Path)
			if rep, err := Verify(context.Background(), f.Path); err != nil || !rep.OK() {
				t.Fatalf("verify: %+v, %v", rep, err)
			}
		})
	}
}

// touch sets a file's modification time.
func touch(t *testing.T, p string, mtime time.Time) {
	t.Helper()
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// makeReadOnly makes dir and everything under it read-only (dirs 0555, files 0444) and restores
// write permission when the test ends, so the temp directory can be removed.
func makeReadOnly(t *testing.T, dir string) {
	t.Helper()
	var paths []string
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		paths = append(paths, p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, p := range paths {
			fi, err := os.Lstat(p)
			if err != nil {
				continue
			}
			mode := os.FileMode(0o644)
			if fi.IsDir() {
				mode = 0o755
			}
			_ = os.Chmod(p, mode)
		}
	})
	for i := len(paths) - 1; i >= 0; i-- {
		fi, err := os.Lstat(paths[i])
		if err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o444)
		if fi.IsDir() {
			mode = 0o555
		}
		if err := os.Chmod(paths[i], mode); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBackupReadOnlySource(t *testing.T) {
	t.Run("stopped cleanly", func(t *testing.T) {
		data := plexDataDir(t)
		lib := makeLibrary(t, dbPath(data, LibraryDB), 200, 100)
		if err := writeTx(lib, 1); err != nil {
			t.Fatal(err)
		}
		if err := lib.Close(); err != nil {
			t.Fatal(err)
		}
		writePrefs(t, data)
		makeReadOnly(t, data)
		before := dirSnapshot(t, data)
		res, err := Backup(context.Background(), BackupOptions{DataPath: data, StagingDir: stagingDir(t)})
		if err != nil {
			t.Fatal(err)
		}
		if got := dirSnapshot(t, data); !slices.Equal(got, before) {
			t.Fatalf("the backup changed Plex's directory:\nbefore %v\nafter  %v", before, got)
		}
		f, _ := res.File(LibraryDB)
		if f.Method != MethodOnlineBackupImmutable || invariants(t, f.Path) != 1 {
			t.Fatalf("library %+v", f)
		}
		if _, ok := res.File(PreferencesXML); !ok || len(res.Warnings) != 0 {
			t.Fatalf("result %+v", res)
		}
	})

	t.Run("crashed with a WAL", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores file permissions: SQLite would open the -shm read-write")
		}
		// Build the crash image elsewhere: a database whose committed transactions are still in
		// its WAL, copied with its -wal and -shm while the writer is idle (as after kill -9).
		live := filepath.Join(t.TempDir(), "live.db")
		lib := makeLibrary(t, live, 200, 100)
		mustExec(t, lib, `PRAGMA wal_autocheckpoint = 0`)
		for n := 1; n <= 25; n++ {
			if err := writeTx(lib, n); err != nil {
				t.Fatal(err)
			}
		}
		data := plexDataDir(t)
		for _, suffix := range []string{"", "-wal", "-shm"} {
			copyFile(t, live+suffix, dbPath(data, LibraryDB)+suffix)
		}
		if err := lib.Close(); err != nil {
			t.Fatal(err)
		}
		if fi, err := os.Stat(dbPath(data, LibraryDB) + "-wal"); err != nil || fi.Size() == 0 {
			t.Fatalf("fixture: the crash image has no WAL content: %v", err)
		}
		writePrefs(t, data)
		makeReadOnly(t, data)
		before := dirSnapshot(t, data)
		res, err := Backup(context.Background(), BackupOptions{DataPath: data, StagingDir: stagingDir(t)})
		if err != nil {
			t.Fatal(err)
		}
		if got := dirSnapshot(t, data); !slices.Equal(got, before) {
			t.Fatalf("the backup changed Plex's directory:\nbefore %v\nafter  %v", before, got)
		}
		f, _ := res.File(LibraryDB)
		if f.Method != MethodOnlineBackup {
			t.Fatalf("library %+v", f)
		}
		if last := invariants(t, f.Path); last != 25 {
			t.Fatalf("the copy holds sequence %d: the committed transactions in the WAL were lost", last)
		}
		if rep, err := Verify(context.Background(), f.Path); err != nil || !rep.OK() {
			t.Fatalf("verify: %+v, %v", rep, err)
		}
	})
}

// copyFile copies a file byte for byte.
func copyFile(t *testing.T, from, to string) {
	t.Helper()
	in, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBackupMissingFiles(t *testing.T) {
	t.Run("no library database", func(t *testing.T) {
		data := plexDataDir(t)
		writePrefs(t, data)
		staging := stagingDir(t)
		if _, err := Backup(context.Background(), BackupOptions{DataPath: data, StagingDir: staging}); !errors.Is(err, ErrNoDatabase) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("no blobs database, no Preferences.xml", func(t *testing.T) {
		data := plexDataDir(t)
		if err := makeLibrary(t, dbPath(data, LibraryDB), 5, 5).Close(); err != nil {
			t.Fatal(err)
		}
		var logs recReporter
		res, err := Backup(context.Background(), BackupOptions{DataPath: data, StagingDir: stagingDir(t), Log: logs.Log})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Files) != 1 || len(res.Warnings) != 1 || !logs.has("does not exist") {
			t.Fatalf("result %+v, logs %s", res, logs.text())
		}
	})
	t.Run("Preferences.xml is a symlink", func(t *testing.T) {
		data := plexDataDir(t)
		if err := makeLibrary(t, dbPath(data, LibraryDB), 5, 5).Close(); err != nil {
			t.Fatal(err)
		}
		other := filepath.Join(t.TempDir(), "other.xml")
		if err := os.WriteFile(other, []byte("<x/>"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(other, filepath.Join(data, PreferencesXML)); err != nil {
			t.Fatal(err)
		}
		res, err := Backup(context.Background(), BackupOptions{DataPath: data, StagingDir: stagingDir(t)})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := res.File(PreferencesXML); ok || len(res.Warnings) != 1 {
			t.Fatalf("result %+v", res)
		}
	})
	t.Run("Preferences.xml unreadable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores file permissions")
		}
		data := plexDataDir(t)
		if err := makeLibrary(t, dbPath(data, LibraryDB), 5, 5).Close(); err != nil {
			t.Fatal(err)
		}
		writePrefs(t, data)
		if err := os.Chmod(filepath.Join(data, PreferencesXML), 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(data, PreferencesXML), 0o600) })
		res, err := Backup(context.Background(), BackupOptions{DataPath: data, StagingDir: stagingDir(t)})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Warnings) != 1 || !containsAll(res.Warnings[0], "permission denied", "PUID") {
			t.Fatalf("result %+v", res)
		}
	})
	t.Run("relative data path", func(t *testing.T) {
		if _, err := Backup(context.Background(), BackupOptions{DataPath: "plex", StagingDir: stagingDir(t)}); err == nil {
			t.Fatal("a relative data path was accepted")
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		data := plexDataDir(t)
		if err := makeLibrary(t, dbPath(data, LibraryDB), 5, 5).Close(); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := Backup(ctx, BackupOptions{DataPath: data, StagingDir: stagingDir(t)}); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("cancelled during the copy", func(t *testing.T) {
		// The one Step(-1) cannot be interrupted; a job cancelled meanwhile stops when it
		// returns, without making the copy durable and hashing it.
		data := plexDataDir(t)
		if err := makeLibrary(t, dbPath(data, LibraryDB), 5, 5).Close(); err != nil {
			t.Fatal(err)
		}
		staging := stagingDir(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var logs []string
		o := BackupOptions{DataPath: data, StagingDir: staging,
			afterCopy: func(string, int) { cancel() },
			Log:       func(_ slog.Level, msg string, _ ...any) { logs = append(logs, msg) }}
		if _, err := Backup(ctx, o); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
		if slices.Contains(logs, "Copied the Plex database") {
			t.Fatalf("the copy was finished after the cancellation: %v", logs)
		}
		if left, _ := os.ReadDir(staging); len(left) != 0 {
			t.Fatalf("a cancelled backup left %v in staging", left)
		}
	})
}

func TestInspect(t *testing.T) {
	data := plexDataDir(t)
	lib := makeLibrary(t, dbPath(data, LibraryDB), 10, 10)
	defer lib.Close()
	if err := writeTx(lib, 1); err != nil {
		t.Fatal(err)
	}
	writePrefs(t, data)
	files, err := Inspect(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("files %+v", files)
	}
	l, b, p := files[0], files[1], files[2]
	if l.Name != LibraryDB || !l.Present || !l.Readable || !l.WALPresent || l.WALSize == 0 || !l.SHMPresent || l.Method != MethodOnlineBackup || l.Size == 0 {
		t.Fatalf("library %+v", l)
	}
	if b.Name != BlobsDB || b.Present || b.Problem != "not found" || b.Method != "" {
		t.Fatalf("blobs %+v", b)
	}
	if p.Name != PreferencesXML || !p.Present || !p.Readable || p.Method != MethodCopy || p.Rel != PreferencesXML {
		t.Fatalf("prefs %+v", p)
	}
	if _, err := Inspect(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("a missing data path was accepted")
	}
}
