package arrbackup

import (
	"bytes"
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

// The Docker test of acceptance 5 (docs/design/phase2-3.md §15, Docker suite 4). It runs only with
// BUNKARR_E2E_ARR set (part of `make test-arr`), against a real Radarr container
// (BUNKARR_E2E_RADARR_IMAGE, default lscr.io/linuxserver/radarr:latest). The runner runs inside
// a second container from the same image (this package's test binary, built for Linux), which
// mounts Radarr's /config/Backups read-only at /arr/radarr-backups, as Bunkarr's container would,
// and reaches Radarr by its container name.
//
//   - Forms login required: the folder method makes Radarr create a backup (the Backup command)
//     and stores an ok arr snapshot; the HTTP method fails with the guidance error.
//   - A fresh scheduled backup (the harness copies the manual zip into Backups/scheduled, as
//     Radarr's weekly task would write one) is copied and no Backup command is sent; the next job
//     finds it unchanged.
//   - "Disabled for Local Addresses": the HTTP method stores an ok snapshot.

const (
	dockerRadarrKey   = "b0a1c2d3e4f5a6b7c8d9e0f1a2b3c4d5"
	dockerBackupMount = "/arr/radarr-backups"
	innerEnv          = "BUNKARR_ARRBACKUP_INNER"
)

func TestDockerArrBackup(t *testing.T) {
	if os.Getenv("BUNKARR_E2E_ARR") == "" {
		t.Skip("BUNKARR_E2E_ARR is not set (the *arr Docker suite, make test-arr)")
	}
	if os.Getenv(innerEnv) != "" {
		t.Skip("inside the runner container")
	}
	dockerPath, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("docker is not installed")
	}
	image := os.Getenv("BUNKARR_E2E_RADARR_IMAGE")
	if image == "" {
		image = "lscr.io/linuxserver/radarr:latest"
	}
	d := &dockerCLI{t: t, bin: dockerPath}
	arch := strings.TrimSpace(d.run("version", "--format", "{{.Server.Arch}}"))
	if arch != "amd64" && arch != "arm64" {
		t.Skipf("unsupported Docker architecture %q", arch)
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
	network, radarr := "bk-arrbackup-"+suffix, "bk-radarr-"+suffix
	d.run("network", "create", network)
	t.Cleanup(func() {
		d.try("rm", "-f", radarr)
		d.try("network", "rm", network)
	})
	start := func(authRequired string) {
		t.Helper()
		writeRadarrConfig(t, cfg, authRequired)
		d.run("run", "-d", "--name", radarr, "--network", network, "-e", fmt.Sprintf("PUID=%d", os.Getuid()),
			"-e", fmt.Sprintf("PGID=%d", os.Getgid()), "-e", "TZ=Etc/UTC", "-v", cfg+":/config", image)
	}
	inner := func(phase string) {
		t.Helper()
		out, err := d.output("run", "--rm", "--network", network, "-v", backups+":"+dockerBackupMount+":ro", "-v", binDir+":/test:ro",
			"-e", innerEnv+"="+phase, "-e", "RADARR_URL=http://"+radarr+":7878", "-e", "RADARR_KEY="+dockerRadarrKey, "-e", "TMPDIR=/tmp",
			"--entrypoint", "/test/arrbackup.test", image, "-test.run", "^TestDockerArrBackupInner$", "-test.v", "-test.count=1")
		t.Logf("phase %s:\n%s", phase, out)
		if err != nil {
			t.Fatalf("phase %s failed: %v", phase, err)
		}
		if strings.Contains(out, "--- SKIP") || !strings.Contains(out, "--- PASS: TestDockerArrBackupInner") {
			t.Fatalf("phase %s did not run", phase)
		}
	}

	start("Enabled") // Forms login, required for every address
	inner("forms")

	// A scheduled backup made now, as Radarr's own weekly task would write it.
	manual, err := filepath.Glob(filepath.Join(backups, "manual", "radarr_backup_*.zip"))
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

	d.run("rm", "-f", radarr)
	start("DisabledForLocalAddresses")
	inner("local")
}

// writeRadarrConfig writes Radarr's config.xml (a new one, or the one Radarr completed) with the
// test's API key, Forms authentication and authRequired ("Enabled" or
// "DisabledForLocalAddresses").
func writeRadarrConfig(t *testing.T, cfg, authRequired string) {
	t.Helper()
	p := filepath.Join(cfg, "config.xml")
	body, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		body = []byte("<Config>\n  <BindAddress>*</BindAddress>\n  <Port>7878</Port>\n  <UrlBase></UrlBase>\n  <ApiKey>" + dockerRadarrKey +
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

// dockerCLI runs docker commands for a test.
type dockerCLI struct {
	t   *testing.T
	bin string
}

func (d *dockerCLI) output(args ...string) (string, error) {
	var out bytes.Buffer
	cmd := exec.Command(d.bin, args...)
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

func (d *dockerCLI) run(args ...string) string {
	d.t.Helper()
	out, err := d.output(args...)
	if err != nil {
		d.t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func (d *dockerCLI) try(args ...string) {
	_, _ = d.output(args...)
}

// TestDockerArrBackupInner is the part of TestDockerArrBackup that runs inside the runner
// container (BUNKARR_ARRBACKUP_INNER names the phase).
func TestDockerArrBackupInner(t *testing.T) {
	phase := os.Getenv(innerEnv)
	if phase == "" {
		t.Skip("runs inside the container of TestDockerArrBackup")
	}
	url, key := os.Getenv("RADARR_URL"), os.Getenv("RADARR_KEY")
	// The Backups folder is mounted read-only.
	if err := os.WriteFile(filepath.Join(dockerBackupMount, "write-test"), nil, 0o600); !errors.Is(err, syscall.EROFS) {
		t.Fatalf("the Backups folder is writable: %v", err)
	}
	client, err := arr.New(arr.KindRadarr, url, key, arr.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		if _, err := client.Status(ctx); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("Radarr did not answer: %v", err)
		}
		time.Sleep(time.Second)
	}
	f := newDockerFixture(t, url, key)
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
		st := f.backup(t, true, jobs.StatusCompleted)
		if st.Method != FetchFolder || st.Integrity != IntegrityOK || st.ReusedScheduled || st.BackupType != arr.BackupManual {
			t.Fatalf("stats %+v", st)
		}
		if after := manualCount(); after != before+1 {
			t.Fatalf("Radarr has %d manual backups, had %d", after, before)
		}
		f.checkStored(t, st)
		j := f.runJob(t, false, jobs.StatusFailed)
		want := (&LoginRequiredError{App: "Radarr"}).Error()
		if !strings.Contains(j.Error, want) {
			t.Fatalf("the HTTP job failed with %q, want %q", j.Error, want)
		}
		// The download was checked before a backup was asked for: none was added.
		if after := manualCount(); after != before+1 {
			t.Fatalf("Radarr has %d manual backups after the failed HTTP job, had %d", after, before+1)
		}
	case "scheduled":
		before := manualCount()
		st := f.backup(t, true, jobs.StatusCompleted)
		if !st.ReusedScheduled || st.BackupType != arr.BackupScheduled || st.Integrity != IntegrityOK || manualCount() != before {
			t.Fatalf("stats %+v, %d manual backups (had %d)", st, manualCount(), before)
		}
		f.checkStored(t, st)
		if again := f.backup(t, true, jobs.StatusCompleted); !again.Unchanged || manualCount() != before {
			t.Fatalf("second job %+v", again)
		}
	case "local":
		st := f.backup(t, false, jobs.StatusCompleted)
		if st.Method != FetchHTTP || st.Integrity != IntegrityOK {
			t.Fatalf("stats %+v", st)
		}
		f.checkStored(t, st)
	default:
		t.Fatalf("unknown phase %q", phase)
	}
}

// dockerFixture is Bunkarr's side inside the runner container: a database, a destination on the
// container's filesystem, a Radarr integration and a job manager with the runner.
type dockerFixture struct {
	ctx    context.Context
	jobs   *jobqueue.Store
	mgr    *jobqueue.Manager
	ints   *integrations.Store
	store  *snapshots.Store
	integ  integrations.Integration
	dest   destinations.Destination
	target string
}

func newDockerFixture(t *testing.T, url, key string) *dockerFixture {
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
	settings, _ := json.Marshal(map[string]any{"backupFolder": dockerBackupMount, "backup": map[string]any{"destinationId": f.dest.ID}})
	if f.integ, err = f.ints.Create(ctx, integrations.Input{Type: integrations.TypeRadarr, Name: "Radarr", URL: url, APIKey: key,
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

// runJob runs an arr_backup job with the folder method or over HTTP and requires the status.
func (f *dockerFixture) runJob(t *testing.T, folder bool, want jobs.Status) jobs.Job {
	t.Helper()
	settings := map[string]any{"backup": map[string]any{"destinationId": f.dest.ID}}
	if folder {
		settings["backupFolder"] = dockerBackupMount
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

// backup is runJob returning the stats.
func (f *dockerFixture) backup(t *testing.T, folder bool, want jobs.Status) Stats {
	t.Helper()
	j := f.runJob(t, folder, want)
	var st Stats
	if err := json.Unmarshal(j.Stats, &st); err != nil {
		t.Fatalf("stats %s: %v", j.Stats, err)
	}
	return st
}

// checkStored verifies the version of st at the destination: an ok arr snapshot whose zip (0600
// in a 0700 directory) verifies again and matches the manifest.
func (f *dockerFixture) checkStored(t *testing.T, st Stats) {
	t.Helper()
	s, err := f.store.Get(f.ctx, st.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err := json.Unmarshal(s.Manifest, &m); err != nil {
		t.Fatal(err)
	}
	if s.Kind != snapshots.KindArr || s.Integrity != IntegrityOK || m.App != "radarr" || m.AppVersion == "" || !m.Sensitive || len(m.Entries) < 2 {
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
	work := t.TempDir()
	rep, err := VerifyZip(f.ctx, zipPath, "radarr", work)
	if err != nil || !rep.OK() {
		t.Fatalf("the stored zip does not verify: %+v, %v", rep, err)
	}
	fmt.Printf("stored %s: %d bytes, entries %v\n", s.Path, m.Zip.Size, m.Entries)
}
