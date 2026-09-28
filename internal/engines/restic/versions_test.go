package restic

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/bwlimit"
	"github.com/sl0wz3r/bunkarr/internal/engines/enginetest"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
)

// fakeRepo is a restic repository for the version store: backup records the backed-up
// directory's files and tags, snapshots and ls read them back, restore writes included files.
type fakeRepo struct {
	mu    sync.Mutex
	snaps []Snapshot
	files map[string]map[string][]byte // snapshot id -> absolute path -> content
	// dropFile leaves this file name out of the next backup (restic skipping a file silently).
	dropFile string
	next     int
	// duringBackup runs while a backup of dir runs.
	duringBackup func(dir string)
}

func (r *fakeRepo) install(t *testing.T, f *enginetest.FakeRunner) {
	f.Handle(proc.Restic, "backup", func(*enginetest.Call) enginetest.Script {
		r.mu.Lock()
		sid := id(100 + r.next)
		r.next++
		r.mu.Unlock()
		return enginetest.Script{
			Stdout: []string{`{"message_type":"summary","files_new":1,"total_files_processed":1,"snapshot_id":"` + sid + `"}`},
			Hook: func(c *enginetest.Call) {
				r.mu.Lock()
				defer r.mu.Unlock()
				dir := c.Args[len(c.Args)-1] // a local repository: no -o options
				if i := slices.Index(c.Args, "-o"); i > 0 {
					dir = c.Args[i-1]
				}
				if r.duringBackup != nil {
					r.duringBackup(dir)
				}
				files := map[string][]byte{}
				_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
					if err == nil && !d.IsDir() && filepath.Base(p) != r.dropFile {
						b, _ := os.ReadFile(p)
						files[p] = b
					}
					return err
				})
				when, _ := c.Flag("--time")
				ts, _ := time.ParseInLocation("2006-01-02 15:04:05", when, time.Local)
				r.snaps = append(r.snaps, Snapshot{ID: sid, Time: ts, Paths: []string{dir}, Hostname: Host, Tags: c.FlagValues("--tag")})
				if r.files == nil {
					r.files = map[string]map[string][]byte{}
				}
				r.files[sid] = files
			}}
	})
	f.Handle(proc.Restic, "snapshots", func(c *enginetest.Call) enginetest.Script {
		r.mu.Lock()
		defer r.mu.Unlock()
		filter, _ := c.Flag("--tag")
		want := strings.Split(filter, ",")
		var ids []string
		for _, a := range c.Args[1:] {
			if len(a) == 64 {
				ids = append(ids, a)
			}
		}
		var out []Snapshot
		for _, s := range r.snaps {
			if (len(ids) == 0 || slices.Contains(ids, s.ID)) && s.HasTags(want...) {
				out = append(out, s)
			}
		}
		b, _ := json.Marshal(out)
		return enginetest.Script{Stdout: []string{string(b)}}
	})
	f.Handle(proc.Restic, "ls", func(c *enginetest.Call) enginetest.Script {
		r.mu.Lock()
		defer r.mu.Unlock()
		sid := c.Args[3]
		var out []string
		for p, b := range r.files[sid] {
			n, _ := json.Marshal(map[string]any{"name": filepath.Base(p), "type": "file", "path": p, "size": len(b),
				"mtime": "2026-09-24T12:00:00Z", "message_type": "node"})
			out = append(out, string(n))
		}
		slices.Sort(out)
		return enginetest.Script{Stdout: out}
	})
	f.Handle(proc.Restic, "restore", func(*enginetest.Call) enginetest.Script {
		return enginetest.Script{Hook: func(c *enginetest.Call) {
			r.mu.Lock()
			defer r.mu.Unlock()
			sid, sub, _ := strings.Cut(c.Args[1], ":")
			target, _ := c.Flag("--target")
			include := strings.Split(strings.TrimSpace(string(c.DataFile("include"))), "\n")
			for p, b := range r.files[sid] {
				rel := strings.TrimPrefix(p, sub)
				if !slices.Contains(include, rel) {
					continue
				}
				dst := filepath.Join(target, rel)
				_ = os.MkdirAll(filepath.Dir(dst), 0o700)
				if err := os.WriteFile(dst, b, 0o644); err != nil {
					t.Error(err)
				}
			}
		}}
	})
}

func TestVersionStore(t *testing.T) {
	d, f := newTestDriver(t)
	repo := &fakeRepo{}
	repo.install(t, f)
	dest, sec := s3Repo()
	r := connect(t, d, dest, sec)
	staging := t.TempDir()
	var requests []string
	vs := NewVersionStore(r, staging, func(_ context.Context, tx *sql.Tx, snapshotID, kind, reason string) error {
		requests = append(requests, snapshotID+" "+kind+" "+reason)
		return nil
	})
	ctx := context.Background()
	stageVersion := func(files map[string]string) string {
		dir := filepath.Join(staging, "job12-plex")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for n, c := range files {
			if err := os.WriteFile(filepath.Join(dir, n), []byte(c), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	manifest := `{"jobId":12,"integrationId":3}` + "\r\n"
	dir := stageVersion(map[string]string{"manifest.json": manifest, "com.plexapp.plugins.library.db": "sqlite\x00\r\nbytes"})
	logical := ".bunkarr/plex/plex-3/20260924T120000Z-job12"
	when := time.Date(2026, 9, 24, 12, 0, 0, 0, time.Local)
	ref, err := vs.Put(ctx, engines.PutVersion{Kind: engines.VersionPlexDB, LogicalPath: logical, Dir: dir, Time: when, JobID: 12, IntegrationID: 3})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
		t.Fatalf("the staged version was not moved back: %v", err)
	}
	b := f.CallsOf(proc.Restic, "backup")[0]
	wantPath := filepath.Join(staging, "versions", "11", logical)
	if got := b.Args[slices.Index(b.Args, "-o")-1]; got != wantPath {
		t.Fatalf("backup path %q, want %q", got, wantPath)
	}
	wantTags := []string{"bunkarr", "bunkarr-dest:" + testEngineTag, "bunkarr-kind:plexdb", "bunkarr-job:12", "bunkarr-integration:3",
		"bunkarr-version:20260924T120000Z-job12"}
	if !slices.Equal(b.FlagValues("--tag"), wantTags) {
		t.Fatalf("tags %q", b.FlagValues("--tag"))
	}
	if v, _ := b.Flag("--time"); v != "2026-09-24 12:00:00" || !b.Has("--retry-lock") {
		t.Fatalf("argv %q", b.Args)
	}

	list, err := vs.List(ctx, ".bunkarr/plex")
	if err != nil || len(list) != 1 {
		t.Fatalf("list %+v %v", list, err)
	}
	v := list[0]
	if v.Ref != ref || v.LogicalPath != logical || v.Version != "20260924T120000Z-job12" || v.JobID != 12 || v.IntegrationID != 3 ||
		!v.Complete || v.Files["manifest.json"] != int64(len(manifest)) || len(v.Files) != 2 {
		t.Fatalf("version %+v", v)
	}
	if s := f.CallsOf(proc.Restic, "snapshots"); !strings.Contains(strings.Join(s[len(s)-1].Args, " "), "--tag bunkarr-dest:"+testEngineTag+",bunkarr-kind:plexdb") {
		t.Fatalf("list filter %q", s[len(s)-1].Args)
	}
	if other, err := vs.List(ctx, ".bunkarr/arr"); err != nil || len(other) != 0 {
		t.Fatalf("arr list %+v %v", other, err)
	}

	got, err := vs.ReadFile(ctx, ref, "manifest.json", 1<<20)
	if err != nil || string(got) != manifest {
		t.Fatalf("ReadFile %q %v", got, err)
	}
	dst := t.TempDir()
	if err := vs.Fetch(ctx, ref, "com.plexapp.plugins.library.db", dst); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "com.plexapp.plugins.library.db")); err != nil || string(b) != "sqlite\x00\r\nbytes" {
		t.Fatalf("Fetch %q %v", b, err)
	}
	if _, err := vs.ReadFile(ctx, ref, "../x", 10); err == nil {
		t.Fatal("ReadFile outside the version")
	}
	if _, err := vs.ReadFile(ctx, ref, "missing.txt", 10); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ReadFile of a missing file: %v", err)
	}

	if err := vs.Remove(ctx, nil, ref, engines.VersionPlexDB); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(requests, []string{string(ref) + " plexdb pruned by its runner"}) {
		t.Fatalf("requests %q", requests)
	}
	if n := len(f.CallsOf(proc.Restic, "forget")); n != 0 {
		t.Fatal("Remove forgot a snapshot")
	}

	// A file restic left out fails the Put.
	repo.dropFile = "com.plexapp.plugins.library.db"
	if _, err := vs.Put(ctx, engines.PutVersion{Kind: engines.VersionPlexDB, LogicalPath: ".bunkarr/plex/plex-3/20260925T120000Z-job13",
		Dir: dir, Time: when, JobID: 13, IntegrationID: 3}); err == nil || !strings.Contains(err.Error(), "lacks com.plexapp") {
		t.Fatalf("a version without its database: %v", err)
	}
	for _, bad := range []engines.PutVersion{
		{Kind: engines.VersionPlexDB, LogicalPath: "Movies/x", Dir: dir, JobID: 1, IntegrationID: 1},
		{Kind: engines.VersionArr, LogicalPath: logical, Dir: dir, JobID: 1, IntegrationID: 1},
		{Kind: engines.VersionPlexDB, LogicalPath: logical, Dir: t.TempDir(), JobID: 1, IntegrationID: 1},
	} {
		if _, err := vs.Put(ctx, bad); err == nil {
			t.Errorf("Put(%+v) accepted", bad)
		}
	}
}

// TestVersionStagingLeftovers: Put moves the staged version to <staging>/versions/<dest>/<logical
// path> for the backup and back afterwards; a crash during the backup left it there (the Plex
// token in Preferences.xml, an *arr zip, in clear text) and the resumed job stages under another
// logical path. Creating a version store and every Put remove such leftovers of any destination,
// but never the directory of a Put that is running, nor anything outside <staging>/versions.
func TestVersionStagingLeftovers(t *testing.T) {
	d, f := newTestDriver(t)
	repo := &fakeRepo{}
	repo.install(t, f)
	dest, sec := s3Repo()
	r := connect(t, d, dest, sec)
	staging := t.TempDir()
	leftover := func(rel string) string {
		p := filepath.Join(staging, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("PlexOnlineToken=secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	crashed := leftover("versions/11/.bunkarr/plex/plex-3/20260920T000000Z-job7/Preferences.xml")
	otherDest := leftover("versions/99/.bunkarr/arr/sonarr-2/20260920T000000Z-job8/sonarr_backup.zip")
	ownJob := leftover("plexdb-job7/version/Preferences.xml")
	noRequests := func(context.Context, *sql.Tx, string, string, string) error { return nil }
	vs := NewVersionStore(r, staging, noRequests)
	for _, p := range []string{crashed, otherDest} {
		if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s stayed: %v", p, err)
		}
	}
	if _, err := os.Stat(ownJob); err != nil {
		t.Fatalf("a job's own staging directory was removed: %v", err)
	}

	crashed = leftover("versions/11/.bunkarr/plex/plex-3/20260921T000000Z-job9/Preferences.xml")
	dir := filepath.Join(staging, "plexdb-job12", "version")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for n, c := range map[string]string{"manifest.json": "{}\n", "Preferences.xml": "PlexOnlineToken=secret"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	logical := ".bunkarr/plex/plex-3/20260924T120000Z-job12"
	target := filepath.Join(staging, "versions", "11", filepath.FromSlash(logical))
	repo.duringBackup = func(backedUp string) {
		if backedUp != target {
			t.Errorf("backup of %s", backedUp)
		}
		if _, err := os.Stat(crashed); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the leftover of a crashed Put stayed during the next Put: %v", err)
		}
		// Another job opens the destination's store while this Put's backup runs.
		NewVersionStore(r, staging, noRequests)
		if _, err := os.Stat(filepath.Join(target, "Preferences.xml")); err != nil {
			t.Errorf("a running Put's version was removed: %v", err)
		}
	}
	if _, err := vs.Put(context.Background(), engines.PutVersion{Kind: engines.VersionPlexDB, LogicalPath: logical, Dir: dir,
		Time: time.Date(2026, 9, 24, 12, 0, 0, 0, time.Local), JobID: 12, IntegrationID: 3}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "Preferences.xml")); err != nil {
		t.Fatalf("the staged version was not moved back: %v", err)
	}
}

// TestVersionPutLimits: a config version backup to a local repository runs with the limits in
// force (§9.1: config version jobs use the destination's limits; remote kinds get RCLONE_BWLIMIT).
func TestVersionPutLimits(t *testing.T) {
	d, f := newTestDriver(t)
	repo := &fakeRepo{}
	repo.install(t, f)
	dest, sec := localRepo(t)
	dest.Bandwidth = bwlimit.Config{UploadKiBps: 4096, DownloadKiBps: 8192,
		Timetable: []bwlimit.Entry{{Days: []string{"thu"}, From: "08:00", To: "23:00", UploadKiBps: 512, DownloadKiBps: 1024}}}
	thursdayNoon := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	r, err := d.Connect(dest, sec, engines.Runtime{JobID: 12, Now: func() time.Time { return thursdayNoon }, Location: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	staging := t.TempDir()
	dir := filepath.Join(staging, "plexdb-job12", "version")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	vs := NewVersionStore(r, staging, nil)
	if _, err := vs.Put(context.Background(), engines.PutVersion{Kind: engines.VersionPlexDB, LogicalPath: ".bunkarr/plex/plex-3/v1",
		Dir: dir, Time: thursdayNoon, JobID: 12, IntegrationID: 3}); err != nil {
		t.Fatal(err)
	}
	b := f.CallsOf(proc.Restic, "backup")[0]
	up, _ := b.Flag("--limit-upload")
	down, _ := b.Flag("--limit-download")
	if up != "512" || down != "1024" {
		t.Fatalf("limits up %q down %q in %q, want the timetable's 512 and 1024", up, down, b.Args)
	}
}
