package arrbackup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/config"
	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr"
	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/snapshots"
)

// The Docker test of acceptance 5 for Lidarr (design D2: Lidarr gets backups like Sonarr and
// Radarr). It runs only with BUNKARR_E2E_ARR set, against a real Lidarr container of the fixture
// spike's version (BUNKARR_E2E_LIDARR_IMAGE overrides it), exactly like TestDockerArrBackup does
// against Radarr: the runner runs inside a second container from the Lidarr image (this package's
// test binary, built for Linux), which mounts Lidarr's /config/Backups read-only at
// /arr/lidarr-backups and reaches Lidarr by its container name.
//
//   - Forms login required: the folder method makes Lidarr create a backup (the Backup command,
//     API v1) and stores an ok arr snapshot holding lidarr.db; the HTTP method fails with the
//     guidance error, before any command.
//   - A fresh scheduled backup is copied and no Backup command is sent; the next job finds it
//     unchanged.
//   - "Disabled for Local Addresses": the HTTP method stores an ok snapshot.

const (
	// defaultLidarrImage is linuxserver/lidarr 3.1.0.4875-ls42 (the fixture spike's version),
	// pinned by index digest.
	defaultLidarrImage = "lscr.io/linuxserver/lidarr@sha256:044d616beb43c5e7810991242c6a9c42b93ff238c8c0850684939634ea751208"
	dockerLidarrKey    = "c1d2e3f4a5b6c7d8e9f0a1b2c3d4e5f6"
	dockerLidarrMount  = "/arr/lidarr-backups"
	lidarrInnerEnv     = "BUNKARR_ARRBACKUP_LIDARR_INNER"
)

func TestDockerArrBackupLidarr(t *testing.T) {
	if os.Getenv("BUNKARR_E2E_ARR") == "" {
		t.Skip("BUNKARR_E2E_ARR is not set (the *arr Docker suite, make test-arr)")
	}
	if os.Getenv(lidarrInnerEnv) != "" || os.Getenv(innerEnv) != "" {
		t.Skip("inside a runner container")
	}
	dockerPath, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("docker is not installed")
	}
	image := os.Getenv("BUNKARR_E2E_LIDARR_IMAGE")
	if image == "" {
		image = defaultLidarrImage
	}
	d := &dockerCLI{t: t, bin: dockerPath}
	arch := strings.TrimSpace(d.run("version", "--format", "{{.Server.Arch}}"))
	if arch != "amd64" && arch != "arm64" {
		t.Skipf("unsupported Docker architecture %q", arch)
	}
	if _, err := d.output("image", "inspect", "--format", "{{.Id}}", image); err != nil {
		d.run("pull", "--quiet", image)
	}
	work, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(work, "config")
	backups := filepath.Join(cfg, "Backups")
	binDir := filepath.Join(work, "bin")
	for _, dir := range []string{backups, binDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	build := exec.Command("go", "test", "-c", "-o", filepath.Join(binDir, "arrbackup.test"), ".")
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the test binary for linux/%s: %v\n%s", arch, err, out)
	}

	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	suffix := hex.EncodeToString(rnd[:])
	network, lidarr := "bk-arrbackup-lidarr-"+suffix, "bk-lidarr-"+suffix
	d.run("network", "create", network)
	t.Cleanup(func() {
		if t.Failed() {
			out, _ := d.output("logs", "--tail", "60", lidarr)
			t.Logf("---- docker logs %s ----\n%s", lidarr, out)
		}
		d.try("rm", "-f", lidarr)
		d.try("network", "rm", network)
	})
	start := func(authRequired string) {
		t.Helper()
		writeLidarrConfig(t, cfg, authRequired)
		d.run("run", "-d", "--name", lidarr, "--network", network, "-e", fmt.Sprintf("PUID=%d", os.Getuid()),
			"-e", fmt.Sprintf("PGID=%d", os.Getgid()), "-e", "TZ=Etc/UTC", "-v", cfg+":/config", image)
	}
	inner := func(phase string) {
		t.Helper()
		out, err := d.output("run", "--rm", "--network", network, "-v", backups+":"+dockerLidarrMount+":ro", "-v", binDir+":/test:ro",
			"-e", lidarrInnerEnv+"="+phase, "-e", "LIDARR_URL=http://"+lidarr+":8686", "-e", "LIDARR_KEY="+dockerLidarrKey, "-e", "TMPDIR=/tmp",
			"--entrypoint", "/test/arrbackup.test", image, "-test.run", "^TestDockerArrBackupLidarrInner$", "-test.v", "-test.count=1")
		t.Logf("phase %s:\n%s", phase, out)
		if err != nil {
			t.Fatalf("phase %s failed: %v", phase, err)
		}
		if strings.Contains(out, "--- SKIP") || !strings.Contains(out, "--- PASS: TestDockerArrBackupLidarrInner") {
			t.Fatalf("phase %s did not run", phase)
		}
	}

	start("Enabled") // Forms login, required for every address
	inner("forms")

	// A scheduled backup made now, as Lidarr's own weekly task would write it.
	manual, err := filepath.Glob(filepath.Join(backups, "manual", "lidarr_backup_*.zip"))
	if err != nil || len(manual) == 0 {
		t.Fatalf("no manual backup in %s (%v)", backups, err)
	}
	data, err := os.ReadFile(manual[0])
	if err != nil {
		t.Fatal(err)
	}
	name := regexp.MustCompile(`_\d{4}\.\d{2}\.\d{2}_\d{2}\.\d{2}\.\d{2}\.zip$`).ReplaceAllString(filepath.Base(manual[0]),
		time.Now().UTC().Format("_2006.01.02_15.04.05")+".zip")
	if err := os.MkdirAll(filepath.Join(backups, "scheduled"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backups, "scheduled", name), data, 0o644); err != nil {
		t.Fatal(err)
	}
	inner("scheduled")

	d.run("rm", "-f", lidarr)
	start("DisabledForLocalAddresses")
	inner("local")
}

// writeLidarrConfig writes Lidarr's config.xml (a new one, or the one Lidarr completed) with the
// test's API key, Forms authentication and authRequired ("Enabled" or
// "DisabledForLocalAddresses").
func writeLidarrConfig(t *testing.T, cfg, authRequired string) {
	t.Helper()
	p := filepath.Join(cfg, "config.xml")
	body, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		body = []byte("<Config>\n  <BindAddress>*</BindAddress>\n  <Port>8686</Port>\n  <UrlBase></UrlBase>\n  <ApiKey>" + dockerLidarrKey +
			"</ApiKey>\n  <AuthenticationMethod>Forms</AuthenticationMethod>\n  <AuthenticationRequired>Enabled</AuthenticationRequired>\n" +
			"  <LogLevel>info</LogLevel>\n  <LaunchBrowser>False</LaunchBrowser>\n  <UpdateMechanism>Docker</UpdateMechanism>\n</Config>\n")
	} else if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`<AuthenticationRequired>[^<]*</AuthenticationRequired>`)
	if !re.Match(body) {
		t.Fatalf("config.xml has no AuthenticationRequired:\n%s", body)
	}
	body = re.ReplaceAll(body, []byte("<AuthenticationRequired>"+authRequired+"</AuthenticationRequired>"))
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestDockerArrBackupLidarrInner is the part of TestDockerArrBackupLidarr that runs inside the
// runner container (BUNKARR_ARRBACKUP_LIDARR_INNER names the phase).
func TestDockerArrBackupLidarrInner(t *testing.T) {
	phase := os.Getenv(lidarrInnerEnv)
	if phase == "" {
		t.Skip("runs inside the container of TestDockerArrBackupLidarr")
	}
	url, key := os.Getenv("LIDARR_URL"), os.Getenv("LIDARR_KEY")
	if err := os.WriteFile(filepath.Join(dockerLidarrMount, "write-test"), nil, 0o600); !errors.Is(err, syscall.EROFS) {
		t.Fatalf("the Backups folder is writable: %v", err)
	}
	client, err := arr.New(arr.KindLidarr, url, key, arr.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		if _, err := client.Status(ctx); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("Lidarr did not answer: %v", err)
		}
		time.Sleep(time.Second)
	}
	f := newLidarrDockerFixture(t, url, key)
	manualCount := func() int {
		t.Helper()
		list, err := client.Backups(ctx)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, b := range list {
			if b.Type == arr.BackupManual {
				n++
			}
		}
		return n
	}
	switch phase {
	case "forms":
		before := manualCount()
		st := f.lidarrBackup(t, true, jobs.StatusCompleted)
		if st.Method != FetchFolder || st.Integrity != IntegrityOK || st.ReusedScheduled || st.BackupType != arr.BackupManual {
			t.Fatalf("stats %+v", st)
		}
		if after := manualCount(); after != before+1 {
			t.Fatalf("Lidarr has %d manual backups, had %d", after, before)
		}
		f.checkLidarrStored(t, st)
		j := f.runLidarrJob(t, false, jobs.StatusFailed)
		if want := (&LoginRequiredError{App: "Lidarr"}).Error(); !strings.Contains(j.Error, want) {
			t.Fatalf("the HTTP job failed with %q, want %q", j.Error, want)
		}
		if after := manualCount(); after != before+1 {
			t.Fatalf("Lidarr has %d manual backups after the failed HTTP job, had %d", after, before+1)
		}
	case "scheduled":
		before := manualCount()
		st := f.lidarrBackup(t, true, jobs.StatusCompleted)
		if !st.ReusedScheduled || st.BackupType != arr.BackupScheduled || st.Integrity != IntegrityOK || manualCount() != before {
			t.Fatalf("stats %+v, %d manual backups (had %d)", st, manualCount(), before)
		}
		f.checkLidarrStored(t, st)
		if again := f.lidarrBackup(t, true, jobs.StatusCompleted); !again.Unchanged || manualCount() != before {
			t.Fatalf("second job %+v", again)
		}
	case "local":
		st := f.lidarrBackup(t, false, jobs.StatusCompleted)
		if st.Method != FetchHTTP || st.Integrity != IntegrityOK {
			t.Fatalf("stats %+v", st)
		}
		f.checkLidarrStored(t, st)
	default:
		t.Fatalf("unknown phase %q", phase)
	}
}

// newLidarrDockerFixture is newDockerFixture with a Lidarr integration.
func newLidarrDockerFixture(t *testing.T, url, key string) *dockerFixture {
	t.Helper()
	ctx := context.Background()
	base := t.TempDir()
	d, err := db.Open(ctx, filepath.Join(base, "bunkarr.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	kr, err := config.NewKeyring(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	f := &dockerFixture{ctx: ctx, jobs: jobqueue.NewStore(d), ints: integrations.NewStore(d, kr), target: filepath.Join(base, "nas")}
	dests := destinations.New(d, destinations.Options{})
	if err := os.Mkdir(f.target, 0o755); err != nil {
		t.Fatal(err)
	}
	if f.dest, err = dests.Create(ctx, destinations.Input{Name: "NAS", Target: f.target}, destinations.CreateOptions{AllowLocal: true}); err != nil {
		t.Fatal(err)
	}
	settings, _ := json.Marshal(map[string]any{"backupFolder": dockerLidarrMount, "backup": map[string]any{"destinationId": f.dest.ID}})
	if f.integ, err = f.ints.Create(ctx, integrations.Input{Type: integrations.TypeLidarr, Name: "Lidarr", URL: url, APIKey: key,
		Settings: settings}); err != nil {
		t.Fatal(err)
	}
	r, err := NewRunner(Options{DB: d, Integrations: f.ints, Destinations: dests, ConfigDir: filepath.Join(base, "config")})
	if err != nil {
		t.Fatal(err)
	}
	f.store = r.Store()
	f.mgr = jobqueue.New(d, nil, jobqueue.Options{Workers: 1})
	f.mgr.Register(jobs.TypeArrBackup, r)
	if err := f.mgr.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.mgr.Stop(context.Background()) })
	return f
}

// runLidarrJob runs an arr_backup job of the Lidarr integration with the folder method or over
// HTTP and requires the status.
func (f *dockerFixture) runLidarrJob(t *testing.T, folder bool, want jobs.Status) jobs.Job {
	t.Helper()
	settings := map[string]any{"backup": map[string]any{"destinationId": f.dest.ID}}
	if folder {
		settings["backupFolder"] = dockerLidarrMount
	}
	raw, _ := json.Marshal(settings)
	it, err := f.ints.Update(f.ctx, f.integ.ID, integrations.Input{Name: f.integ.Name, URL: f.integ.URL, Settings: raw})
	if err != nil {
		t.Fatal(err)
	}
	f.integ = it
	job, err := f.mgr.Enqueue(f.ctx, jobs.Spec{Type: jobs.TypeArrBackup, Trigger: jobs.TriggerManual, Params: jobs.Params{IntegrationID: f.integ.ID}})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Minute)
	for {
		j, err := f.jobs.GetJob(f.ctx, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if j.Status.Final() {
			if j.Status != want {
				logs, _ := f.jobs.ListLogs(f.ctx, j.ID, 0, 200)
				t.Fatalf("job %+v, want %s; logs %+v", j, want, logs)
			}
			return j
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %d did not finish", job.ID)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// lidarrBackup is runLidarrJob returning the stats.
func (f *dockerFixture) lidarrBackup(t *testing.T, folder bool, want jobs.Status) Stats {
	t.Helper()
	j := f.runLidarrJob(t, folder, want)
	var st Stats
	if err := json.Unmarshal(j.Stats, &st); err != nil {
		t.Fatalf("stats %s: %v", j.Stats, err)
	}
	return st
}

// checkLidarrStored verifies the version of st at the destination: an ok arr snapshot of Lidarr
// whose zip (0600 in a 0700 directory) holds lidarr.db, verifies again and matches the manifest.
func (f *dockerFixture) checkLidarrStored(t *testing.T, st Stats) {
	t.Helper()
	s, err := f.store.Get(f.ctx, st.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err := json.Unmarshal(s.Manifest, &m); err != nil {
		t.Fatal(err)
	}
	hasDB := false
	for _, e := range m.Entries {
		hasDB = hasDB || e.Name == "lidarr.db"
	}
	if s.Kind != snapshots.KindArr || s.Integrity != IntegrityOK || m.App != "lidarr" || !strings.HasPrefix(m.AppVersion, "3.") ||
		!m.Sensitive || !hasDB || !strings.HasPrefix(m.Zip.Name, "lidarr_backup_") {
		t.Fatalf("snapshot %+v, manifest %+v", s, m)
	}
	dir := filepath.Join(f.target, filepath.FromSlash(s.Path))
	zipPath := filepath.Join(dir, m.Zip.Name)
	for p, mode := range map[string]os.FileMode{zipPath: 0o600, dir: 0o700} {
		fi, err := os.Stat(p)
		if err != nil || fi.Mode().Perm() != mode {
			t.Fatalf("%s: %v, %v", p, fi, err)
		}
	}
	rep, err := VerifyZip(f.ctx, zipPath, "lidarr", t.TempDir())
	if err != nil || !rep.OK() {
		t.Fatalf("the stored zip does not verify: %+v, %v", rep, err)
	}
	fmt.Printf("stored %s: %d bytes, entries %v\n", s.Path, m.Zip.Size, m.Entries)
}
