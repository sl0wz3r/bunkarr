//go:build e2e

package e2e

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/engines/rclone"
)

// The off-site acceptance suite (docs/design/phase4.md §14.6, acceptance 1-9; make test-offsite,
// docker/test-offsite.sh). Every test builds a derived image FROM the Bunkarr image under test
// (BUNKARR_E2E_IMAGE) with an e2e build of this checkout, the argv shims of docker/offsite and
// curl; starts MinIO and an OpenSSH SFTP server (pinned by digest) on one network with a tools
// container (the same image: restic, rclone, the media volume read-write) and one or two Bunkarr
// containers (the media volume read-only); drives Bunkarr over HTTP through docker exec; and ends
// with the secrets audit of acceptance 4 on every Bunkarr container.

// Pinned images (docker/test-offsite.sh passes the same values; the engine spike recorded the
// MinIO and SFTP digests in testdata/restic/index.json, docker/engines/Dockerfile the golang one).
const (
	defaultMinIOImage  = "cgr.dev/chainguard/minio@sha256:6a1d0b45c8669726bba580ced0bfa4cb9fdeed1ed636dfabd81d1577beb6937b"
	defaultSFTPImage   = "atmoz/sftp:alpine@sha256:a81ea210713555be76075b4b2788a4addfaa54d137cd881f3a99ac539f0be2c5"
	defaultGolangImage = "golang:1.27-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414"
)

// Paths inside the derived test image.
const (
	shimDir       = "/opt/bunkarr-e2e/bin"
	argvLog       = "/argvlog/argv.log"
	testWindow    = "/config/testwindow"
	offsiteHost   = "bunkarr-e2e-offsite"
	offsiteUser   = "bunkarr"
	offsiteMedia  = "/media"
	minioAlias    = "minio"
	sftpAlias     = "sftp"
	offsiteRegion = "us-east-1"
)

// offImage returns the value of an image variable, or its pinned default.
func offImage(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// offsite is one test's off-site environment.
type offsite struct {
	t    *testing.T
	d    *dockerEnv
	root string // the module root (docker/offsite, testdata)
	arch string // GOARCH of the image under test

	net   string
	image string // the derived test image
	media string // the media volume (tools: rw at /media; Bunkarr: ro)
	tools string // restic, rclone and the media volume, on the network

	s3Key, s3Secret string
	minio           string
	sftp            *sftpServer

	secrets *secretSet

	mu       sync.Mutex
	bunkarrs []*bunkarrC
}

// newOffsite skips the test unless BUNKARR_E2E_OFFSITE=1 and BUNKARR_E2E_IMAGE name the suite
// (docker/test-offsite.sh), builds the derived image and starts the network and the tools
// container. MinIO and the SFTP server start on demand (startMinIO, startSFTP).
func newOffsite(t *testing.T) *offsite {
	t.Helper()
	if os.Getenv("BUNKARR_E2E_OFFSITE") != "1" {
		t.Skip("BUNKARR_E2E_OFFSITE is not 1 (run docker/test-offsite.sh IMAGE or make test-offsite)")
	}
	d := newDockerEnv(t)
	root, err := moduleRoot()
	if err != nil {
		t.Fatal(err)
	}
	o := &offsite{t: t, d: d, root: root, secrets: newSecretSet()}
	o.arch = d.docker("image", "inspect", "--format", "{{.Architecture}}", d.image)
	o.buildImage()
	o.net, _ = d.network("net")
	o.media = d.volume("media")
	o.tools = d.helper("tools", o.image, "--network", o.net, "-v", o.media+":"+offsiteMedia)
	d.docker("exec", o.tools, "mkdir", "-p", "/work")
	// Diagnostics of a failed test, before the containers are removed. BUNKARR_E2E_OFFSITE_HOLD
	// (a duration such as "15m") keeps them that long for inspection.
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		o.dumpDiagnostics()
		if hold, err := time.ParseDuration(os.Getenv("BUNKARR_E2E_OFFSITE_HOLD")); err == nil && hold > 0 {
			t.Logf("holding the containers of %s for %s (BUNKARR_E2E_OFFSITE_HOLD)", d.prefix, hold)
			time.Sleep(hold)
		}
	})
	return o
}

// buildImage builds the derived test image: the image under test plus curl, the e2e build of this
// checkout over /app/bunkarr, and the argv shims (root-owned, 0755) that BUNKARR_RESTIC_PATH and
// BUNKARR_RCLONE_PATH name. The files go in as one tar stream with explicit owners and modes.
func (o *offsite) buildImage() {
	t, d := o.t, o.d
	bin := o.crossBuild(t.TempDir(), "./cmd/bunkarr", "e2e timetzdata", false)
	shim, err := os.ReadFile(filepath.Join(o.root, "docker", "offsite", "argv-shim.sh"))
	if err != nil {
		t.Fatal(err)
	}
	binData, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	add := func(name string, mode int64, data []byte) {
		hdr := &tar.Header{Name: name, Mode: mode, Size: int64(len(data)), Uid: 0, Gid: 0, ModTime: time.Now(), Typeflag: tar.TypeReg}
		if data == nil {
			hdr.Typeflag, hdr.Size = tar.TypeDir, 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if data != nil {
			if _, err := tw.Write(data); err != nil {
				t.Fatal(err)
			}
		}
	}
	add("app/bunkarr", 0o755, binData)
	add("opt/bunkarr-e2e/", 0o755, nil)
	add("opt/bunkarr-e2e/bin/", 0o755, nil)
	add("opt/bunkarr-e2e/bin/restic", 0o755, shim)
	add("opt/bunkarr-e2e/bin/rclone", 0o755, shim)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	c := d.name("imagebuild")
	d.docker("run", "--name", c, d.image, "sh", "-ec",
		"apk add --no-cache curl >/dev/null && mkdir -p /argvlog && chmod 1777 /argvlog && : > "+argvLog+" && chmod 0666 "+argvLog)
	cmd := exec.Command("docker", "cp", "-", c+":/")
	cmd.Stdin = &buf
	if out, err := cmd.CombinedOutput(); err != nil {
		_, _ = d.try("rm", "-f", c)
		t.Fatalf("docker cp into %s: %v\n%s", c, err, out)
	}
	tag := d.name("offsite") + ":test"
	d.docker("commit", "--change", "CMD []",
		"--change", "ENV BUNKARR_RESTIC_PATH="+shimDir+"/restic",
		"--change", "ENV BUNKARR_RCLONE_PATH="+shimDir+"/rclone", c, tag)
	d.images = append(d.images, tag)
	d.docker("rm", c)
	o.image = tag
	// The shims must be what Bunkarr accepts (root's, not writable by the Bunkarr UID).
	out := d.oneShot("--entrypoint", "stat", tag, "-c", "%U %a %n", "/app/bunkarr", shimDir+"/restic", shimDir+"/rclone")
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) != 3 || f[0] != "root" || f[1] != "755" {
			t.Fatalf("derived image file %q: want root 755", l)
		}
	}
}

// crossBuild builds pkg for the image's platform (GOOS linux, CGO off) into dir and returns the
// binary's path: `go build -trimpath -tags <tags>`, or with test set `go test -c -tags <tags>`
// (without -trimpath, so arrtest finds its fixtures at the module's absolute path).
func (o *offsite) crossBuild(dir, pkg, tags string, test bool) string {
	o.t.Helper()
	out := filepath.Join(dir, filepath.Base(pkg))
	args := []string{"build", "-trimpath", "-tags", tags, "-o", out, pkg}
	if test {
		out += ".test"
		args = []string{"test", "-c", "-tags", tags, "-o", out, pkg}
	}
	cmd := exec.Command("go", args...)
	cmd.Dir = o.root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+o.arch)
	if b, err := cmd.CombinedOutput(); err != nil {
		o.t.Fatalf("go %s (linux/%s): %v\n%s", strings.Join(args, " "), o.arch, err, b)
	}
	return out
}

// --- storage servers ---------------------------------------------------------------------------

// startMinIO starts MinIO with throwaway root credentials (the S3 destinations use them too) and
// waits until the tools container can create a bucket there.
func (o *offsite) startMinIO(buckets ...string) {
	o.t.Helper()
	o.s3Key, o.s3Secret = "bk"+randomHex(7), randomHex(16)
	o.secrets.add("S3 access key id", o.s3Key)
	o.secrets.add("S3 secret access key", o.s3Secret)
	o.minio = o.d.name("minio")
	o.d.containers = append(o.d.containers, o.minio)
	cmd := exec.Command("docker", "run", "-d", "--name", o.minio, "--network", o.net, "--network-alias", minioAlias,
		"-e", "MINIO_ROOT_USER", "-e", "MINIO_ROOT_PASSWORD", offImage("BUNKARR_E2E_MINIO_IMAGE", defaultMinIOImage), "server", "/tmp/minio-data")
	cmd.Env = append(os.Environ(), "MINIO_ROOT_USER="+o.s3Key, "MINIO_ROOT_PASSWORD="+o.s3Secret)
	if out, err := cmd.CombinedOutput(); err != nil {
		o.t.Fatalf("start MinIO: %v\n%s", err, out)
	}
	if len(buckets) == 0 {
		buckets = []string{"bunkarr"}
	}
	// Buckets are created only once MinIO reports ready: a bucket created while it initializes can
	// be half-made (HEAD and create say it exists, listings and writes say NoSuchBucket).
	deadline := time.Now().Add(90 * time.Second)
	for !o.d.execOK(o.tools, "curl", "-sf", "-o", "/dev/null", "http://"+minioAlias+":9000/minio/health/ready") {
		if time.Now().After(deadline) {
			out, _ := exec.Command("docker", "logs", "--tail", "20", o.minio).CombinedOutput()
			o.t.Fatalf("MinIO was not ready within 90 s:\n%s", out)
		}
		time.Sleep(500 * time.Millisecond)
	}
	for _, b := range buckets {
		for {
			mk, err := o.toolsTry(o.minioEnv(), "rclone", "mkdir", "-v", "minio:"+b)
			if err == nil {
				var out string
				if out, err = o.toolsTry(o.minioEnv(), "rclone", "lsf", "--dirs-only", "minio:"); err == nil {
					if strings.Contains(out, b+"/") {
						break
					}
					err = fmt.Errorf("bucket %s not listed after rclone mkdir (%s): %q", b, strings.TrimSpace(mk), out)
				}
			}
			if time.Now().After(deadline) {
				out, _ := exec.Command("docker", "logs", "--tail", "20", o.minio).CombinedOutput()
				o.t.Fatalf("MinIO did not start within 90 s (%v):\n%s", err, out)
			}
			time.Sleep(time.Second)
		}
	}
}

// minioEnv is an rclone remote "minio:" with MinIO's root credentials (for the tools container:
// buckets, listings, damaging objects).
func (o *offsite) minioEnv() map[string]string {
	return map[string]string{
		"RCLONE_CONFIG":                         "/dev/null",
		"RCLONE_CONFIG_MINIO_TYPE":              "s3",
		"RCLONE_CONFIG_MINIO_PROVIDER":          "Minio",
		"RCLONE_CONFIG_MINIO_ENDPOINT":          "http://" + minioAlias + ":9000",
		"RCLONE_CONFIG_MINIO_ACCESS_KEY_ID":     o.s3Key,
		"RCLONE_CONFIG_MINIO_SECRET_ACCESS_KEY": o.s3Secret,
		"RCLONE_CONFIG_MINIO_FORCE_PATH_STYLE":  "true",
		"RCLONE_CONFIG_MINIO_ENV_AUTH":          "false",
		"RCLONE_CONFIG_MINIO_REGION":            offsiteRegion,
	}
}

// s3Remote is the remote of an S3 destination on MinIO under prefix.
func (o *offsite) s3Remote(bucket, prefix string) map[string]any {
	return map[string]any{"provider": "Minio", "endpoint": "http://" + minioAlias + ":9000", "region": offsiteRegion,
		"bucket": bucket, "prefix": prefix, "forcePathStyle": true}
}

// s3Credentials are the S3 destinations' credentials (MinIO's root user).
func (o *offsite) s3Credentials() map[string]any {
	return map[string]any{"accessKeyId": o.s3Key, "secretAccessKey": o.s3Secret}
}

// sftpServer is the OpenSSH SFTP server: user bunkarr (uid 1001, writable "upload"), a
// passphrase-protected ed25519 client key, and the server's host keys read from its files.
type sftpServer struct {
	c          string
	keyPEM     string
	passphrase string
	// password is the account's password (password authentication works too).
	password string
	// hostKeys maps "<type> <base64>" of every /etc/ssh/ssh_host_*_key.pub to true.
	hostKeys map[string]bool
}

// startSFTP starts the SFTP server, waits for sshd, runs docker/offsite/sftp-setup.sh in it and
// reads its host key files.
func (o *offsite) startSFTP() *sftpServer {
	o.t.Helper()
	s := &sftpServer{c: o.d.name("sftp"), passphrase: randomHex(12), hostKeys: map[string]bool{}}
	o.secrets.add("SFTP key passphrase", s.passphrase)
	password := randomHex(12)
	s.password = password
	o.secrets.add("SFTP password", password)
	o.d.containers = append(o.d.containers, s.c)
	cmd := exec.Command("docker", "run", "-d", "--name", s.c, "--platform", "linux/amd64", "--network", o.net,
		"--network-alias", sftpAlias, offImage("BUNKARR_E2E_SFTP_IMAGE", defaultSFTPImage), offsiteUser+":"+password+":1001::upload")
	if out, err := cmd.CombinedOutput(); err != nil {
		o.t.Fatalf("start the SFTP server: %v\n%s", err, out)
	}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		logs, _ := exec.Command("docker", "logs", s.c).CombinedOutput()
		if strings.Contains(string(logs), "Server listening") {
			break
		}
		if time.Now().After(deadline) {
			o.t.Fatalf("the SFTP server did not start within 3 minutes:\n%s", logs)
		}
		time.Sleep(time.Second)
	}
	script, err := os.ReadFile(filepath.Join(o.root, "docker", "offsite", "sftp-setup.sh"))
	if err != nil {
		o.t.Fatal(err)
	}
	s.keyPEM = o.execStdin(s.c, map[string]string{"SFTP_KEY_PASSPHRASE": s.passphrase}, string(script), "sh", "-s", offsiteUser)
	if !strings.Contains(s.keyPEM, "BEGIN OPENSSH PRIVATE KEY") {
		o.t.Fatalf("sftp-setup.sh printed no private key: %q", s.keyPEM)
	}
	s.keyPEM = strings.TrimSpace(s.keyPEM) + "\n"
	o.secrets.addPEM("SFTP private key", s.keyPEM)
	pubs := o.d.docker("exec", s.c, "sh", "-c", "cat /etc/ssh/ssh_host_*_key.pub")
	for _, l := range strings.Split(pubs, "\n") {
		if f := strings.Fields(l); len(f) >= 2 {
			s.hostKeys[f[0]+" "+f[1]] = true
		}
	}
	if len(s.hostKeys) < 2 {
		o.t.Fatalf("SFTP host keys: %q", pubs)
	}
	o.sftp = s
	return s
}

// sftpRemote is the remote of an SFTP destination under upload/<sub>, with the host keys that
// POST /destinations/sftp/hostkeys presented after they were compared with the server's files.
func (b *bunkarrC) sftpRemote(sub string) map[string]any {
	b.t.Helper()
	var keys []struct {
		Type        string `json:"type"`
		Fingerprint string `json:"fingerprint"`
		Key         string `json:"key"`
	}
	b.api.call(viaKey, 200, "POST", "/destinations/sftp/hostkeys", map[string]any{"host": sftpAlias, "port": 22}, &keys)
	files := b.o.sftp.hostKeys
	var pinned []map[string]string
	seen := map[string]bool{}
	for _, k := range keys {
		// The server's rsa key file says ssh-rsa; so does the key the scan reports.
		if !files[k.Type+" "+k.Key] || !strings.HasPrefix(k.Fingerprint, "SHA256:") {
			b.t.Fatalf("presented host key %s %s (%s) is not one of the server's key files %v", k.Type, k.Key, k.Fingerprint, files)
		}
		seen[k.Type+" "+k.Key] = true
		pinned = append(pinned, map[string]string{"type": k.Type, "key": k.Key})
	}
	if len(seen) != len(files) {
		b.t.Fatalf("presented host keys %v, the server has %v", seen, files)
	}
	// The destination's directory exists (Create lists it: a missing SFTP path is "path not found").
	b.o.d.docker("exec", b.o.sftp.c, "sh", "-c", `mkdir -p "$1" && chown -R 1001 "$1"`, "sh", "/home/"+offsiteUser+"/upload/"+sub)
	return map[string]any{"host": sftpAlias, "port": 22, "user": offsiteUser, "path": "upload/" + sub, "hostKeys": pinned}
}

// sftpCredentials are the SFTP destinations' credentials (the client key and its passphrase).
func (o *offsite) sftpCredentials() map[string]any {
	return map[string]any{"privateKey": o.sftp.keyPEM, "privateKeyPassphrase": o.sftp.passphrase}
}

// --- containers -------------------------------------------------------------------------------

// bunkarrC is one Bunkarr container of the suite.
type bunkarrC struct {
	t   *testing.T
	o   *offsite
	c   string
	cfg string // config volume
	api *offAPI

	sampler *sampler
}

// bunkarrOpts configures startBunkarr.
type bunkarrOpts struct {
	// hostname defaults to offsiteHost (§6.7: one stable host name per install).
	hostname string
	// cfg reuses a config volume ("" = a new one, set up on first start).
	cfg string
	// args are extra docker run options (volumes, environment).
	args []string
}

// startBunkarr starts a Bunkarr container of the derived image on the network (PUID 1000, debug
// log, the media volume read-only, BUNKARR_TEST_WINDOW_FILE=/config/testwindow), completes the
// first-run setup and starts the /proc sampler.
func (o *offsite) startBunkarr(short string, opt bunkarrOpts) *bunkarrC {
	o.t.Helper()
	fresh := opt.cfg == ""
	if fresh {
		opt.cfg = o.d.volume(short + "-config")
	}
	if opt.hostname == "" {
		opt.hostname = offsiteHost
	}
	args := []string{"--network", o.net, "--hostname", opt.hostname, "--stop-timeout", "60",
		"-e", "PUID=1000", "-e", "PGID=1000", "-e", "BUNKARR_LOG_LEVEL=debug",
		"-e", "BUNKARR_TEST_WINDOW_FILE=" + testWindow,
		"-v", opt.cfg + ":/config", "-v", o.media + ":" + offsiteMedia + ":ro"}
	args = append(append(args, opt.args...), o.image)
	b := &bunkarrC{t: o.t, o: o, c: o.d.run(short, args...), cfg: opt.cfg}
	b.api = newOffAPI(o, b.c)
	b.api.waitHealthy(2 * time.Minute)
	if fresh {
		b.api.setup()
	} else {
		b.api.login()
	}
	b.sampler = o.startSampler(b)
	o.mu.Lock()
	o.bunkarrs = append(o.bunkarrs, b)
	o.mu.Unlock()
	return b
}

// kill kills the container with SIGKILL (docker kill -s KILL) and requires it to be gone.
func (b *bunkarrC) kill() {
	b.t.Helper()
	b.sampler.stop()
	b.o.d.docker("kill", "-s", "KILL", b.c)
	if b.o.d.running(b.c) {
		b.t.Fatal("the container survived docker kill -s KILL")
	}
}

// start starts the stopped or killed container again (docker start), waits for /health, logs in
// and restarts the sampler.
func (b *bunkarrC) start() {
	b.t.Helper()
	b.o.d.docker("start", b.c)
	b.api.waitHealthy(2 * time.Minute)
	b.api.login()
	b.sampler = b.o.startSampler(b)
}

// stop stops the container cleanly (SIGTERM, jobs re-queued by the server).
func (b *bunkarrC) stop() {
	b.t.Helper()
	b.sampler.stop()
	b.o.d.docker("stop", "-t", "60", b.c)
}

// setWindow writes BUNKARR_TEST_WINDOW's file: destination id's window is open from open until
// close and again from reopen on (docs/design/phase4.md §9.2; e2e builds only).
func (b *bunkarrC) setWindow(id int64, open, closeAt, reopen time.Time) {
	b.t.Helper()
	v := fmt.Sprintf("%d,%s,%s,%s", id, open.UTC().Format(time.RFC3339), closeAt.UTC().Format(time.RFC3339), reopen.UTC().Format(time.RFC3339))
	b.o.d.docker("exec", b.c, "sh", "-c", `printf '%s\n' "$1" > "$2" && chmod 0644 "$2"`, "sh", v, testWindow)
}

// execStdin runs cmd in container c with stdin and extra environment variables (their values
// come from the docker client's environment, so they are never on an argv) and returns stdout.
func (o *offsite) execStdin(c string, env map[string]string, stdin string, cmd ...string) string {
	o.t.Helper()
	out, err := o.execStdinTry(c, env, stdin, cmd...)
	if err != nil {
		o.t.Fatal(err)
	}
	return out
}

func (o *offsite) execStdinTry(c string, env map[string]string, stdin string, cmd ...string) (string, error) {
	args := []string{"exec", "-i"}
	var envList []string
	for _, k := range slices.Sorted(maps.Keys(env)) {
		args = append(args, "-e", k)
		envList = append(envList, k+"="+env[k])
	}
	args = append(append(args, c), cmd...)
	x := exec.Command("docker", args...)
	x.Env = append(os.Environ(), envList...)
	x.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	x.Stdout, x.Stderr = &stdout, &stderr
	if err := x.Run(); err != nil {
		return stdout.String(), fmt.Errorf("docker exec %s %s: %w: %s", c, shortArgs(cmd), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// toolsTry runs a command in the tools container with env and returns its combined output.
func (o *offsite) toolsTry(env map[string]string, cmd ...string) (string, error) {
	args := []string{"exec", "-w", "/work"}
	var envList []string
	for _, k := range slices.Sorted(maps.Keys(env)) {
		args = append(args, "-e", k)
		envList = append(envList, k+"="+env[k])
	}
	args = append(append(args, o.tools), cmd...)
	x := exec.Command("docker", args...)
	x.Env = append(os.Environ(), envList...)
	out, err := x.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("tools: %s: %w: %s", shortArgs(cmd), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// toolsSh runs a shell script in the tools container (as root) and returns its output.
func (o *offsite) toolsSh(script string, args ...string) string {
	o.t.Helper()
	out, err := o.toolsTry(nil, append([]string{"sh", "-ec", script, "sh"}, args...)...)
	if err != nil {
		o.t.Fatal(err)
	}
	return out
}

// writeRandom writes files of random bytes into the media volume (rel → size in bytes), each
// through a temporary name, with its parent directories.
func (o *offsite) writeRandom(files map[string]int64) {
	o.t.Helper()
	var b strings.Builder
	for _, rel := range slices.Sorted(maps.Keys(files)) {
		p := offsiteMedia + "/" + rel
		fmt.Fprintf(&b, "mkdir -p %s\nhead -c %d /dev/urandom > %s.tmp\nmv %s.tmp %s\n", shq(filepath.Dir(p)), files[rel], shq(p), shq(p), shq(p))
	}
	o.toolsSh("umask 022\n" + b.String())
}

// shq single-quotes s for sh.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// sha256Of returns the sha256 of a file in the media volume.
func (o *offsite) sha256Of(rel string) string {
	o.t.Helper()
	out := o.toolsSh(`sha256sum "$1"`, offsiteMedia+"/"+rel)
	f := strings.Fields(out)
	if len(f) == 0 {
		o.t.Fatalf("sha256sum %s: %q", rel, out)
	}
	return f[0]
}

// tree is a snapshot of every entry under a media directory (type and mode, size, mtime, ctime,
// inode, link count, owner, and the sha256 of files), taken in the tools container: S1 requires
// that no job changes it.
func (o *offsite) tree(dir string) map[string]string {
	o.t.Helper()
	out := o.toolsSh(`cd "$1" && find . -exec stat -c 'S %n|%f|%s|%Y|%Z|%i|%h|%u|%g' {} + && find . -type f -exec sha256sum {} +`,
		offsiteMedia+"/"+dir)
	m := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(l, "S "):
			name, rest, _ := strings.Cut(l[2:], "|")
			m[name] += rest
		case len(l) > 66 && l[64] == ' ':
			m[strings.TrimSpace(l[65:])] += " sha256:" + l[:64]
		}
	}
	return m
}

// untouchedTree runs job and requires the media directory dir to be unchanged by it (S1).
func (o *offsite) untouchedTree(dir string, job func() apiJob) apiJob {
	o.t.Helper()
	before := o.tree(dir)
	j := job()
	after := o.tree(dir)
	if !mapsEqual(before, after) {
		var diff []string
		for k, v := range before {
			if after[k] != v {
				diff = append(diff, fmt.Sprintf("%s: %q -> %q", k, v, after[k]))
			}
		}
		for k := range after {
			if _, ok := before[k]; !ok {
				diff = append(diff, "added: "+k)
			}
		}
		sort.Strings(diff)
		o.t.Fatalf("job %d (%s) changed the source %s (S1):\n%s", j.ID, j.Type, dir, strings.Join(diff, "\n"))
	}
	return j
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// randomHex returns n random bytes in hex.
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// --- the API ----------------------------------------------------------------------------------

// via is how a request authenticates.
type via int

const (
	viaKey     via = iota // X-Api-Key (automation)
	viaSession            // the UI session cookie of the first-run user
	viaNone               // nothing (the local-address bypass when it is on)
)

// offAPI calls a Bunkarr container's API with curl through docker exec. The request (URL,
// headers, body) goes to curl as a config file on stdin, so neither the API key, the cookie nor a
// body with credentials or passwords is ever on an argv. Every answer except recovery kits is kept
// for the secrets audit (acceptance 4).
type offAPI struct {
	t      *testing.T
	o      *offsite
	c      string
	key    string
	cookie string

	mu      sync.Mutex
	answers []string
}

func newOffAPI(o *offsite, c string) *offAPI {
	return &offAPI{t: o.t, o: o, c: c}
}

// client returns the suite's generic API client over the API key (the helpers of server_test.go).
func (a *offAPI) client() *client {
	return &client{t: a.t, do: func(method, path string, body any) (int, []byte, error) {
		code, b, _, err := a.send(viaKey, method, path, body, true)
		return code, b, err
	}}
}

// curlQuote quotes s for a curl config file.
func curlQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`).Replace(s) + `"`
}

var curlStatus = regexp.MustCompile(`BKSTATUS (\d{3})`)

// send performs one request and returns the status, the body and the response headers; keep
// false leaves the answer out of the audit (the recovery kit).
func (a *offAPI) send(v via, method, path string, body any, keep bool) (int, []byte, string, error) {
	var cfg strings.Builder
	line := func(k, v string) { fmt.Fprintf(&cfg, "%s = %s\n", k, curlQuote(v)) }
	line("url", "http://127.0.0.1:8787/api/v1"+path)
	line("request", method)
	cfg.WriteString("silent\nshow-error\n")
	line("max-time", "600")
	line("dump-header", "-")
	line("write-out", "%{stderr}BKSTATUS %{http_code}\n")
	switch v {
	case viaKey:
		if a.key != "" {
			line("header", "X-Api-Key: "+a.key)
		}
	case viaSession:
		if a.cookie != "" {
			line("header", "Cookie: bunkarr_session="+a.cookie)
		}
	}
	if method != "GET" && method != "DELETE" || body != nil {
		b := []byte("{}")
		if body != nil {
			var err error
			if b, err = json.Marshal(body); err != nil {
				return 0, nil, "", err
			}
		}
		line("header", "Content-Type: application/json")
		line("data-binary", string(b))
	}
	cmd := exec.Command("docker", "exec", "-i", a.c, "curl", "-K", "-")
	cmd.Stdin = strings.NewReader(cfg.String())
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	m := curlStatus.FindStringSubmatch(stderr.String())
	if m == nil || m[1] == "000" {
		if runErr == nil {
			runErr = errors.New("no HTTP status")
		}
		return 0, nil, "", fmt.Errorf("%s %s in %s: %w: %s", method, path, a.c, runErr, strings.TrimSpace(stderr.String()))
	}
	code, _ := strconv.Atoi(m[1])
	raw := stdout.String()
	hdr, resp, ok := strings.Cut(raw, "\r\n\r\n")
	if !ok {
		hdr, resp = raw, ""
	}
	if keep {
		a.mu.Lock()
		a.answers = append(a.answers, raw)
		a.mu.Unlock()
	}
	return code, []byte(resp), hdr, nil
}

// call performs a request, requires status want and decodes the answer into out (if not nil).
func (a *offAPI) call(v via, want int, method, path string, body, out any) []byte {
	a.t.Helper()
	code, b, _, err := a.send(v, method, path, body, true)
	if err != nil {
		a.t.Fatal(err)
	}
	if code != want {
		a.t.Fatalf("%s %s: HTTP %d, want %d: %s", method, path, code, want, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			a.t.Fatalf("%s %s: decode %s: %v", method, path, b, err)
		}
	}
	return b
}

// status performs a request and returns its status and body.
func (a *offAPI) status(v via, method, path string, body any) (int, []byte) {
	a.t.Helper()
	code, b, _, err := a.send(v, method, path, body, true)
	if err != nil {
		a.t.Fatal(err)
	}
	return code, b
}

// waitHealthy waits until /api/v1/health answers 200.
func (a *offAPI) waitHealthy(timeout time.Duration) {
	a.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if code, _, _, err := a.send(viaNone, "GET", "/health", nil, false); err == nil && code == 200 {
			return
		}
		if !a.o.d.running(a.c) {
			out, _ := exec.Command("docker", "logs", "--tail", "40", a.c).CombinedOutput()
			a.t.Fatalf("container %s stopped during start-up:\n%s", a.c, out)
		}
		if time.Now().After(deadline) {
			a.t.Fatalf("container %s did not answer /api/v1/health within %s", a.c, timeout)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

var offCookie = regexp.MustCompile(`(?i)Set-Cookie:\s*bunkarr_session=([^;\s]+)`)

// setup completes the first-run setup, keeps the session cookie and reads the API key.
func (a *offAPI) setup() {
	a.t.Helper()
	code, b, hdr, err := a.send(viaNone, "POST", "/auth/setup", map[string]string{"username": e2eUser, "password": e2ePassword}, true)
	if err != nil || code != 201 {
		a.t.Fatalf("first-run setup: HTTP %d, %v: %s", code, err, b)
	}
	m := offCookie.FindStringSubmatch(hdr)
	if m == nil {
		a.t.Fatalf("no session cookie after setup:\n%s", hdr)
	}
	a.cookie = m[1]
	var gs struct {
		APIKey string `json:"apiKey"`
	}
	a.call(viaSession, 200, "GET", "/settings/general", nil, &gs)
	if gs.APIKey == "" {
		a.t.Fatal("no API key in /settings/general")
	}
	a.key = gs.APIKey
}

// login opens a new UI session (after a restart the old one is still valid, but a new cookie
// proves the password login works) and reads the API key.
func (a *offAPI) login() {
	a.t.Helper()
	code, b, hdr, err := a.send(viaNone, "POST", "/auth/login", map[string]any{"username": e2eUser, "password": e2ePassword}, true)
	if err != nil || code != 200 && code != 204 {
		a.t.Fatalf("login: HTTP %d, %v: %s", code, err, b)
	}
	if m := offCookie.FindStringSubmatch(hdr); m != nil {
		a.cookie = m[1]
	}
	var gs struct {
		APIKey string `json:"apiKey"`
	}
	a.call(viaSession, 200, "GET", "/settings/general", nil, &gs)
	a.key = gs.APIKey
}

// --- destinations, kits, jobs -----------------------------------------------------------------

// offDest is the part of a Destination the suite reads.
type offDest struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name"`
	Engine     string   `json:"engine"`
	Kind       string   `json:"kind"`
	Target     string   `json:"target"`
	SourceIDs  []int64  `json:"sourceIds"`
	Warnings   []string `json:"warnings"`
	Pending    bool     `json:"pending"`
	Blocked    string   `json:"blockedReason"`
	Encryption struct {
		Mode           string     `json:"mode"`
		Origin         string     `json:"origin"`
		KitExportedAt  *time.Time `json:"kitExportedAt"`
		KitConfirmedAt *time.Time `json:"kitConfirmedAt"`
	} `json:"encryption"`
	HasCredentials map[string]bool `json:"hasCredentials"`
}

// createDest creates a destination with a UI session and the user's password (S29 needs both
// for every remote kind; a local filecopy destination is created with the API key).
func (b *bunkarrC) createDest(body map[string]any) offDest {
	b.t.Helper()
	v := viaKey
	if k, _ := body["kind"].(string); k != "" && k != "local" {
		v = viaSession
		body["currentPassword"] = e2ePassword
	}
	var d offDest
	b.api.call(v, 201, "POST", "/destinations", body, &d)
	return d
}

// resticS3 is the body of a restic destination on MinIO under prefix.
func (b *bunkarrC) resticS3(name, prefix string, sources []int64, extra map[string]any) map[string]any {
	body := map[string]any{"name": name, "kind": "s3", "engine": "restic", "remote": b.o.s3Remote("bunkarr", prefix),
		"credentials": b.o.s3Credentials(), "encryption": map[string]any{"mode": "restic"}, "sourceIds": sources,
		"settings": map[string]any{"verify": map[string]any{"mode": "full", "samplePercent": 100}}}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

// cryptS3 is the body of an rclone crypt destination on MinIO under prefix.
func (b *bunkarrC) cryptS3(name, prefix string, sources []int64, extra map[string]any) map[string]any {
	body := b.resticS3(name, prefix, sources, extra)
	body["engine"], body["encryption"] = "rclone", map[string]any{"mode": "crypt"}
	return body
}

// cryptSFTP is the body of an rclone crypt destination on the SFTP server under upload/<sub>.
func (b *bunkarrC) cryptSFTP(name, sub string, sources []int64, extra map[string]any) map[string]any {
	body := map[string]any{"name": name, "kind": "sftp", "engine": "rclone", "remote": b.sftpRemote(sub),
		"credentials": b.o.sftpCredentials(), "encryption": map[string]any{"mode": "crypt"}, "sourceIds": sources,
		"settings": map[string]any{"verify": map[string]any{"mode": "full", "samplePercent": 100}}}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

// kitInfo is a parsed recovery kit (docs/design/phase4.md §5.2): its text and its JSON block.
type kitInfo struct {
	Text       string
	CheckCode  string            `json:"checkCode"`
	Engine     string            `json:"engine"`
	Kind       string            `json:"kind"`
	MarkerID   string            `json:"markerId"`
	EngineTag  string            `json:"engineTag"`
	Storage    map[string]string `json:"storageCredentials"`
	KnownHosts string            `json:"knownHosts"`
	RcloneConf string            `json:"rcloneConf"`
	ResticEnv  map[string]string `json:"resticEnvironment"`
	Sources    []struct {
		ID         int64  `json:"id"`
		Path       string `json:"path"`
		DestFolder string `json:"destFolder"`
	} `json:"sources"`
	Encryption struct {
		Mode           string            `json:"mode"`
		ResticPassword string            `json:"resticPassword"`
		CryptPassword  string            `json:"cryptPassword"`
		CryptPassword2 string            `json:"cryptPassword2"`
		CryptObscured  map[string]string `json:"cryptObscured"`
	} `json:"encryption"`
}

// Kit markers (internal/destinations/kit.go).
const (
	kitJSONBegin  = "-----BEGIN BUNKARR RECOVERY KIT JSON-----"
	kitJSONEnd    = "-----END BUNKARR RECOVERY KIT JSON-----"
	kitShellBegin = "----- BEGIN SHELL"
	kitShellEnd   = "----- END SHELL -----"
)

// shellBlock returns the kit's shell block (without its marker lines).
func (k kitInfo) shellBlock() string {
	_, rest, ok := strings.Cut(k.Text, kitShellBegin)
	if !ok {
		return ""
	}
	_, rest, _ = strings.Cut(rest, "\n")
	block, _, _ := strings.Cut(rest, kitShellEnd)
	return block
}

// commands returns the listing and restore lines the kit prints after its shell block.
func (k kitInfo) commands() []string {
	_, rest, ok := strings.Cut(k.Text, "Then list the backups and restore:")
	if !ok {
		return nil
	}
	var out []string
	for _, l := range strings.Split(rest, "\n")[1:] {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "-----") {
			break
		}
		out = append(out, l)
	}
	return out
}

// exportKit exports destination id's recovery kit (session + password; with the storage
// credentials so it restores on its own), parses it and registers its secrets.
func (b *bunkarrC) exportKit(id int64) kitInfo {
	b.t.Helper()
	code, body, hdr, err := b.api.send(viaSession, "POST", fmt.Sprintf("/destinations/%d/recovery-kit", id),
		map[string]any{"currentPassword": e2ePassword, "includeStorageCredentials": true}, false)
	if err != nil || code != 200 {
		b.t.Fatalf("export the recovery kit of %d: HTTP %d, %v: %s", id, code, err, body)
	}
	if !strings.Contains(strings.ToLower(hdr), "content-disposition: attachment") {
		b.t.Fatalf("recovery kit headers:\n%s", hdr)
	}
	k := kitInfo{Text: string(body)}
	_, rest, ok := strings.Cut(k.Text, kitJSONBegin)
	js, _, ok2 := strings.Cut(rest, kitJSONEnd)
	if !ok || !ok2 || json.Unmarshal([]byte(js), &k) != nil {
		b.t.Fatalf("recovery kit without its JSON block:\n%s", k.Text)
	}
	s := b.o.secrets
	s.add("restic password", k.Encryption.ResticPassword)
	s.add("crypt password", k.Encryption.CryptPassword)
	s.add("crypt password2", k.Encryption.CryptPassword2)
	for f, v := range k.Encryption.CryptObscured {
		s.add("obscured "+f, v)
	}
	for f, v := range k.Storage {
		if f == "privateKey" {
			s.addPEM("kit "+f, v)
			continue
		}
		s.add("storage "+f, v)
	}
	for _, l := range strings.Split(k.RcloneConf, "\n") {
		key, val, ok := strings.Cut(l, " = ")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "pass", "key_file_pass", "password", "password2", "secret_access_key", "key":
			s.add("rclone.conf "+key, strings.TrimSpace(val))
		}
	}
	return k
}

// confirmKit exports the kit and confirms its check code (a wrong code first: 400).
func (b *bunkarrC) confirmKit(id int64) kitInfo {
	b.t.Helper()
	k := b.exportKit(id)
	if !regexp.MustCompile(`^[A-Z2-7]{4}-[A-Z2-7]{4}$`).MatchString(k.CheckCode) || !strings.Contains(k.Text, "CHECK CODE: "+k.CheckCode) {
		b.t.Fatalf("check code %q", k.CheckCode)
	}
	wrong := "AAAA-AAAA"
	if k.CheckCode == wrong {
		wrong = "BBBB-BBBB"
	}
	if code, body := b.api.status(viaSession, "POST", fmt.Sprintf("/destinations/%d/recovery-kit/confirm", id), map[string]any{"checkCode": wrong}); code != 400 {
		b.t.Fatalf("confirm with a wrong check code: HTTP %d: %s", code, body)
	}
	// Case and dashes are ignored.
	b.api.call(viaSession, 204, "POST", fmt.Sprintf("/destinations/%d/recovery-kit/confirm", id),
		map[string]any{"checkCode": strings.ToLower(strings.ReplaceAll(k.CheckCode, "-", ""))}, nil)
	var d offDest
	b.api.call(viaKey, 200, "GET", fmt.Sprintf("/destinations/%d", id), nil, &d)
	if d.Encryption.KitConfirmedAt == nil || d.Blocked != "" {
		b.t.Fatalf("destination %d after the kit was confirmed: %+v", id, d)
	}
	return k
}

// createOffsite creates an engine destination and confirms its recovery kit.
func (b *bunkarrC) createOffsite(body map[string]any) (offDest, kitInfo) {
	b.t.Helper()
	d := b.createDest(body)
	if d.Encryption.Mode == "none" || d.Encryption.Mode == "" {
		b.t.Fatalf("destination %q created unencrypted: %+v", d.Name, d)
	}
	return d, b.confirmKit(d.ID)
}

// startJob posts to path (sync, verify, retention, backups) and returns the queued job.
func (b *bunkarrC) startJob(path string, body map[string]any) apiJob {
	b.t.Helper()
	var j apiJob
	b.api.call(viaKey, 202, "POST", path, body, &j)
	if j.ID == 0 {
		b.t.Fatalf("POST %s queued no job", path)
	}
	return j
}

// run posts to path and waits for the job (up to timeout).
func (b *bunkarrC) run(path string, body map[string]any, timeout time.Duration) apiJob {
	b.t.Helper()
	return b.api.client().waitJob(b.startJob(path, body).ID, timeout)
}

// sync, verify and retention run one job of destination id and wait for it.
func (b *bunkarrC) sync(id int64, body map[string]any) apiJob {
	b.t.Helper()
	return b.run(fmt.Sprintf("/destinations/%d/sync", id), body, 15*time.Minute)
}

func (b *bunkarrC) verify(id int64) apiJob {
	b.t.Helper()
	return b.run(fmt.Sprintf("/destinations/%d/verify", id), nil, 15*time.Minute)
}

func (b *bunkarrC) retention(id int64, body map[string]any) apiJob {
	b.t.Helper()
	return b.run(fmt.Sprintf("/destinations/%d/retention", id), body, 15*time.Minute)
}

// job reads a job with its progress.
type offJob struct {
	apiJob
	Progress struct {
		Phase            string     `json:"phase"`
		BytesDone        int64      `json:"bytesDone"`
		BytesTotal       int64      `json:"bytesTotal"`
		LimitBytesPerSec int64      `json:"limitBytesPerSec"`
		WindowEndsAt     *time.Time `json:"windowEndsAt"`
		Batch            int        `json:"batch"`
	} `json:"progress"`
	Params struct {
		DestinationID int64 `json:"destinationId"`
		IntegrationID int64 `json:"integrationId"`
	} `json:"params"`
	NotBefore  *time.Time `json:"notBefore"`
	Deferrals  int        `json:"deferrals"`
	StartedAt  *time.Time `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt"`
}

func (b *bunkarrC) job(id int64) offJob {
	b.t.Helper()
	var j offJob
	b.api.call(viaKey, 200, "GET", fmt.Sprintf("/jobs/%d", id), nil, &j)
	return j
}

// engineSync are the engine keys of an engine sync's stats (§11.4).
type engineSync struct {
	syncStats
	Engine        string `json:"engine"`
	Batches       int    `json:"batches"`
	BytesUploaded int64  `json:"bytesUploaded"`
	Unchanged     bool   `json:"unchanged"`
	Deferrals     int    `json:"deferrals"`
	Snapshots     []struct {
		SourceID   int64  `json:"sourceId"`
		SnapshotID string `json:"snapshotId"`
	} `json:"snapshots"`
}

// engineVerify are an engine verify's stats.
type engineVerify struct {
	verifyStats
	Check *struct {
		NumErrors  int    `json:"numErrors"`
		ReadSubset string `json:"readSubset"`
	} `json:"check"`
	SampleFiles int64 `json:"sampleFiles"`
}

// requireVerified requires a verify job that completed and found nothing wrong.
func requireVerified(t *testing.T, j apiJob) engineVerify {
	t.Helper()
	requireStatus(t, j, "completed")
	st := decodeStats[engineVerify](t, j)
	if st.FilesMissing != 0 || st.FilesFailed != 0 || (st.Check != nil && st.Check.NumErrors != 0) || st.FilesVerified == 0 {
		t.Fatalf("verify %d found problems or checked nothing: %s", j.ID, j.Stats)
	}
	return st
}

// offItem is a job item with its detail.
type offItem struct {
	apiItem
	Detail json.RawMessage `json:"detail"`
}

// items returns every item of a job.
func (b *bunkarrC) items(jobID int64) []offItem {
	b.t.Helper()
	var all []offItem
	for page := 1; ; page++ {
		var p apiPage[offItem]
		b.api.call(viaKey, 200, "GET", fmt.Sprintf("/jobs/%d/items?pageSize=500&page=%d", jobID, page), nil, &p)
		all = append(all, p.Records...)
		if len(p.Records) == 0 || int64(len(all)) >= p.TotalRecords {
			return all
		}
	}
}

// jobLogs returns the log lines of a job ("<level> <message> <fields>").
func (b *bunkarrC) jobLogs(jobID int64) []string {
	b.t.Helper()
	var out []string
	var after int64
	for {
		var page []struct {
			ID      int64           `json:"id"`
			Level   string          `json:"level"`
			Message string          `json:"message"`
			Fields  json.RawMessage `json:"fields"`
		}
		b.api.call(viaKey, 200, "GET", fmt.Sprintf("/jobs/%d/logs?limit=1000&afterId=%d", jobID, after), nil, &page)
		for _, r := range page {
			out = append(out, r.Level+" "+r.Message+" "+string(r.Fields))
			after = r.ID
		}
		if len(page) < 1000 {
			return out
		}
	}
}

// allJobs returns every job of the container, newest first.
func (b *bunkarrC) allJobs() []offJob {
	b.t.Helper()
	var all []offJob
	for page := 1; ; page++ {
		var p apiPage[offJob]
		b.api.call(viaKey, 200, "GET", fmt.Sprintf("/jobs?pageSize=500&page=%d", page), nil, &p)
		all = append(all, p.Records...)
		if len(p.Records) == 0 || int64(len(all)) >= p.TotalRecords {
			return all
		}
	}
}

// waitIdle waits until no job is queued or running (deferred jobs count as queued).
func (b *bunkarrC) waitIdle(timeout time.Duration) {
	b.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		busy := ""
		for _, j := range b.allJobs() {
			if !j.final() {
				busy = fmt.Sprintf("job %d (%s) %s", j.ID, j.Type, j.Status)
				break
			}
		}
		if busy == "" {
			return
		}
		if time.Now().After(deadline) {
			b.t.Fatalf("still busy after %s: %s", timeout, busy)
		}
		time.Sleep(time.Second)
	}
}

// createSource creates a source over a media directory.
func (b *bunkarrC) createSource(name, dir string) apiSource {
	b.t.Helper()
	return b.api.client().createSource(name, offsiteMedia+"/"+dir)
}

// --- the engines in the tools container -------------------------------------------------------

// kitWorkdir writes a kit's files (restic password, rclone.conf, known_hosts) into a directory of
// the tools container named after the kit's marker and returns the directory and the environment
// that the kit's shell block exports.
func (o *offsite) kitWorkdir(k kitInfo) (string, map[string]string) {
	o.t.Helper()
	dir := "/work/kit-" + regexp.MustCompile(`[^A-Za-z0-9]+`).ReplaceAllString(k.MarkerID, "-")
	env := map[string]string{}
	files := map[string]string{}
	if k.Engine == "restic" {
		for key, v := range k.ResticEnv {
			env[key] = v
		}
		env["RESTIC_PASSWORD_FILE"] = dir + "/bunkarr_restic_password"
		env["RESTIC_CACHE_DIR"] = dir + "/cache"
		files["bunkarr_restic_password"] = k.Encryption.ResticPassword
	} else {
		files["rclone.conf"] = k.RcloneConf
		env["RCLONE_CONFIG"] = dir + "/rclone.conf"
	}
	if k.KnownHosts != "" {
		files["bunkarr_known_hosts"] = k.KnownHosts
	}
	var script strings.Builder
	fmt.Fprintf(&script, "umask 077; mkdir -p %s; cd %s\n", shq(dir), shq(dir))
	for n, content := range files {
		fmt.Fprintf(&script, "cat > %s <<'BKEOF'\n%s\nBKEOF\n", shq(n), strings.TrimRight(content, "\n"))
	}
	o.execStdin(o.tools, nil, script.String(), "sh", "-s")
	return dir, env
}

// engineCLI runs restic or rclone in the tools container with a kit's configuration.
func (o *offsite) engineCLI(k kitInfo, cmd ...string) (string, error) {
	dir, env := o.kitWorkdir(k)
	args := []string{"exec", "-w", dir}
	var envList []string
	for _, key := range slices.Sorted(maps.Keys(env)) {
		args = append(args, "-e", key)
		envList = append(envList, key+"="+env[key])
	}
	args = append(append(args, o.tools), cmd...)
	x := exec.Command("docker", args...)
	x.Env = append(os.Environ(), envList...)
	var stdout, stderr bytes.Buffer
	x.Stdout, x.Stderr = &stdout, &stderr
	if err := x.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s: %w: %s", shortArgs(cmd), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// resticSnap is one snapshot of `restic snapshots --json`.
type resticSnap struct {
	ID   string    `json:"id"`
	Time time.Time `json:"time"`
	Tags []string  `json:"tags"`
	Host string    `json:"hostname"`
}

// resticSnapshots lists the repository's snapshots with the tag filter (restic's --tag syntax).
func (o *offsite) resticSnapshots(k kitInfo, tags string) []resticSnap {
	o.t.Helper()
	args := []string{"restic", "snapshots", "--json", "--no-lock"}
	if tags != "" {
		args = append(args, "--tag", tags)
	}
	out, err := o.engineCLI(k, args...)
	if err != nil {
		o.t.Fatal(err)
	}
	var snaps []resticSnap
	if err := json.Unmarshal([]byte(out), &snaps); err != nil {
		o.t.Fatalf("restic snapshots: %v: %s", err, out)
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].Time.Before(snaps[j].Time) })
	return snaps
}

// resticCheck runs `restic check` (structure; readData reads every pack) and requires it to pass.
func (o *offsite) resticCheck(k kitInfo, readData bool) {
	o.t.Helper()
	args := []string{"restic", "check"}
	if readData {
		args = append(args, "--read-data")
	}
	if out, err := o.engineCLI(k, args...); err != nil {
		o.t.Fatalf("restic check: %v\n%s", err, out)
	}
}

// damageObject overwrites 16 bytes in the middle of an object of the bucket (the object keeps its
// size and name), through MinIO's S3 API.
func (o *offsite) damageObject(path string) {
	o.t.Helper()
	script := `set -e
f=$(mktemp)
rclone cat "minio:$1" > "$f"
size=$(stat -c %s "$f")
[ "$size" -gt 64 ]
printf 'BUNKARR-DAMAGED!' | dd of="$f" bs=1 seek=$((size / 2)) conv=notrunc 2>/dev/null
rclone copyto "$f" "minio:$1"
rm -f "$f"
echo "$size"`
	if out, err := o.toolsTry(o.minioEnv(), "sh", "-c", script, "sh", path); err != nil {
		o.t.Fatalf("damage %s: %v\n%s", path, err, out)
	}
}

// objects lists the objects under a bucket path with their sizes ("path size" per line).
func (o *offsite) objects(path string) map[string]int64 {
	o.t.Helper()
	out, err := o.toolsTry(o.minioEnv(), "rclone", "lsf", "-R", "--files-only", "--format", "ps", "--separator", "\t", "minio:"+path)
	if err != nil {
		o.t.Fatal(err)
	}
	m := map[string]int64{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		p, s, ok := strings.Cut(l, "\t")
		if !ok {
			continue
		}
		n, _ := strconv.ParseInt(s, 10, 64)
		m[p] = n
	}
	return m
}

// --- the database -----------------------------------------------------------------------------

// readDB copies a running container's database out (with its WAL) and opens the copy; the copy
// is closed when the test ends. Jobs should be idle (the copy is taken file by file).
func (b *bunkarrC) readDB() *sql.DB {
	b.t.Helper()
	dir := b.t.TempDir()
	for _, n := range []string{"bunkarr.db", "bunkarr.db-wal", "bunkarr.db-shm"} {
		if _, err := b.o.d.try("cp", b.c+":/config/"+n, filepath.Join(dir, n)); err != nil && n == "bunkarr.db" {
			b.t.Fatal(err)
		}
	}
	d, err := db.Open(context.Background(), filepath.Join(dir, "bunkarr.db"), nil)
	if err != nil {
		b.t.Fatal(err)
	}
	b.t.Cleanup(func() { _ = d.Close() })
	return d.Reader()
}

// editDB stops the container, applies fn to its database in one transaction (through a helper
// container on the config volume, so it also works with a remote daemon) and starts it again.
func (b *bunkarrC) editDB(fn func(ctx context.Context, tx *sql.Tx) error) {
	b.t.Helper()
	b.stop()
	d := b.o.d
	h := d.helper("dbedit", b.o.image, "-v", b.cfg+":/config")
	dir := b.t.TempDir()
	for _, n := range []string{"bunkarr.db", "bunkarr.db-wal", "bunkarr.db-shm"} {
		if _, err := d.try("cp", h+":/config/"+n, filepath.Join(dir, n)); err != nil && n == "bunkarr.db" {
			b.t.Fatal(err)
		}
	}
	ctx := context.Background()
	conn, err := db.Open(ctx, filepath.Join(dir, "bunkarr.db"), nil)
	if err != nil {
		b.t.Fatal(err)
	}
	if err := conn.Write(ctx, func(tx *sql.Tx) error { return fn(ctx, tx) }); err != nil {
		_ = conn.Close()
		b.t.Fatalf("edit the database: %v", err)
	}
	if err := conn.Close(); err != nil {
		b.t.Fatal(err)
	}
	for _, n := range []string{"bunkarr.db-wal", "bunkarr.db-shm"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			b.t.Fatalf("%s left after closing the edited database", n)
		}
	}
	d.docker("exec", h, "rm", "-f", "/config/bunkarr.db-wal", "/config/bunkarr.db-shm")
	d.docker("cp", filepath.Join(dir, "bunkarr.db"), h+":/config/bunkarr.db")
	d.docker("exec", h, "chown", "1000:1000", "/config/bunkarr.db")
	d.remove(h)
	b.start()
}

// --- acceptance 4: the secrets audit ----------------------------------------------------------

// secretSet holds every secret the suite knows (destination credentials, encryption secrets, their
// obscured forms): a value is found in clear, JSON-escaped, or as a base64url token that
// rclone.Reveal turns into it (any IV).
type secretSet struct {
	mu   sync.Mutex
	vals map[string]string // value → label
}

func newSecretSet() *secretSet { return &secretSet{vals: map[string]string{}} }

// add registers a secret (values shorter than 8 characters are too short to search for).
func (s *secretSet) add(label, v string) {
	if len(v) < 8 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.vals[v]; !ok {
		s.vals[v] = label
	}
}

// addPEM registers each base64 line of a PEM block (the whole key is never on one line in a
// log, and each line appears verbatim in its JSON-escaped form too).
func (s *secretSet) addPEM(label, pem string) {
	for _, l := range strings.Split(pem, "\n") {
		l = strings.TrimSpace(l)
		if len(l) >= 24 && !strings.HasPrefix(l, "-----") {
			s.add(label+" (line)", l)
		}
	}
}

var b64Token = regexp.MustCompile(`[A-Za-z0-9_-]{24,}`)

// find returns the labels of the secrets text contains, with how.
func (s *secretSet) find(text string) []string {
	s.mu.Lock()
	vals := make(map[string]string, len(s.vals))
	for k, v := range s.vals {
		vals[k] = v
	}
	s.mu.Unlock()
	var out []string
	for v, label := range vals {
		if strings.Contains(text, v) {
			out = append(out, label+" (clear)")
			continue
		}
		esc, _ := json.Marshal(v)
		if e := string(esc[1 : len(esc)-1]); e != v && strings.Contains(text, e) {
			out = append(out, label+" (JSON-escaped)")
		}
	}
	seen := map[string]bool{}
	for _, tok := range b64Token.FindAllString(text, -1) {
		if seen[tok] {
			continue
		}
		seen[tok] = true
		if clear, err := rclone.Reveal(tok); err == nil {
			if label, ok := vals[clear]; ok {
				out = append(out, label+" (rclone-obscured: "+tok+")")
			}
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

// audit is acceptance 4 for one container: no secret in the argv log, the process log (debug),
// the job logs, any API answer but the recovery kits, any file in the container (the sealed
// database excepted), or the argv of any process the sampler saw; secret files only while their
// command ran, 0600 in a 0700 directory on the tmpfs, and none under <config>/run.
func (b *bunkarrC) audit() {
	b.t.Helper()
	t, d, s := b.t, b.o.d, b.o.secrets
	if len(s.vals) == 0 {
		t.Fatal("the audit has no secrets to look for")
	}
	fail := func(where string, found []string) {
		if len(found) > 0 {
			t.Errorf("secrets in %s of %s: %s", where, b.c, strings.Join(found, "; "))
		}
	}
	if d.running(b.c) {
		b.waitIdle(10 * time.Minute)
	}
	// Job logs (fetched through the API, so their answers are searched as answers too).
	if d.running(b.c) {
		for _, j := range b.allJobs() {
			fail(fmt.Sprintf("the log of job %d (%s)", j.ID, j.Type), s.find(strings.Join(b.jobLogs(j.ID), "\n")))
		}
	}
	b.api.mu.Lock()
	answers := strings.Join(b.api.answers, "\n")
	b.api.mu.Unlock()
	fail("the API answers", s.find(answers))

	if b.sampler != nil {
		b.sampler.stop()
	}
	b.sampler.report(t, b.c)

	logs, _ := exec.Command("docker", "logs", b.c).CombinedOutput()
	if !bytes.Contains(logs, []byte(`"level":"DEBUG"`)) && !bytes.Contains(logs, []byte("level=DEBUG")) {
		t.Errorf("the process log of %s has no debug lines (BUNKARR_LOG_LEVEL=debug)", b.c)
	}
	fail("the process log", s.find(string(logs)))

	argv := d.docker("exec", b.c, "cat", argvLog)
	fail("the argv log", s.find(argv))
	if strings.Contains(argv, "restic\narg: backup") && !strings.Contains(argv, "arg: serve") {
		t.Errorf("the argv log of %s shows restic backups but no `rclone serve restic`: the shims did not see restic's rclone backend", b.c)
	}

	// Files: the container's own layer (docker export, restricted to what docker diff lists) and
	// its mounts (except the read-only media and the sealed database), secret run directories
	// included.
	changed := map[string]bool{}
	for _, l := range strings.Split(d.docker("diff", b.c), "\n") {
		if f := strings.Fields(l); len(f) == 2 && (f[0] == "A" || f[0] == "C") {
			changed[strings.TrimPrefix(f[1], "/")] = true
		}
	}
	b.scanTar(exec.Command("docker", "export", b.c), func(name string) bool { return changed[name] }, fail)
	if d.running(b.c) {
		if left := d.docker("exec", b.c, "sh", "-c", "find /dev/shm/bunkarr-run /config/run -mindepth 1 2>/dev/null || true"); left != "" {
			t.Errorf("run directories left in %s after the jobs ended:\n%s", b.c, left)
		}
		// The writable mounts (the config volume, a filecopy destination) and the tmpfs.
		dirs := []string{"dev/shm"}
		for _, m := range strings.Fields(d.docker("inspect", "-f", "{{range .Mounts}}{{if .RW}}{{.Destination}} {{end}}{{end}}", b.c)) {
			dirs = append(dirs, strings.TrimPrefix(m, "/"))
		}
		b.scanTar(exec.Command("docker", append([]string{"exec", b.c, "tar", "-cf", "-", "-C", "/"}, dirs...)...), func(name string) bool {
			base := filepath.Base(name)
			return !strings.HasPrefix(base, "bunkarr.db")
		}, fail)
	}
}

// scanTar reads a tar stream from cmd and searches every regular file that keep accepts.
func (b *bunkarrC) scanTar(cmd *exec.Cmd, keep func(name string) bool, fail func(string, []string)) {
	b.t.Helper()
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		b.t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		b.t.Fatal(err)
	}
	tr := tar.NewReader(pipe)
	files := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			b.t.Fatalf("read %s: %v", shortArgs(cmd.Args), err)
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		if hdr.Typeflag != tar.TypeReg || !keep(name) {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			b.t.Fatal(err)
		}
		files++
		fail("the file /"+name, b.o.secrets.find(string(data)))
	}
	if err := cmd.Wait(); err != nil {
		b.t.Fatalf("%s: %v: %s", shortArgs(cmd.Args), err, stderr.String())
	}
	if files == 0 {
		b.t.Logf("audit: %s listed no file to search", shortArgs(cmd.Args))
	}
}

// auditAll audits every Bunkarr container of the test (acceptance 4 applies to every engine job
// of the suite).
func (o *offsite) auditAll() {
	o.t.Helper()
	o.mu.Lock()
	list := slices.Clone(o.bunkarrs)
	o.mu.Unlock()
	for _, b := range list {
		b.audit()
	}
}

// sampler reads docker/offsite/sampler.sh's samples of one container.
type sampler struct {
	b    *bunkarrC
	cmd  *exec.Cmd
	done chan struct{}

	mu         sync.Mutex
	samples    int
	engine     int             // samples with a restic or rclone process under the server
	secretDirs map[string]bool // per-command directories seen under /dev/shm/bunkarr-run
	secretFile map[string]bool // files seen in them
	problems   []string
}

// startSampler starts the sampler in container b (as root) and parses its output until stop.
func (o *offsite) startSampler(b *bunkarrC) *sampler {
	o.t.Helper()
	script, err := os.ReadFile(filepath.Join(o.root, "docker", "offsite", "sampler.sh"))
	if err != nil {
		o.t.Fatal(err)
	}
	s := &sampler{b: b, done: make(chan struct{}), secretDirs: map[string]bool{}, secretFile: map[string]bool{}}
	s.cmd = exec.Command("docker", "exec", "-i", "-u", "0", b.c, "sh", "-s", "/config")
	s.cmd.Stdin = bytes.NewReader(script)
	out, err := s.cmd.StdoutPipe()
	if err != nil {
		o.t.Fatal(err)
	}
	if err := s.cmd.Start(); err != nil {
		o.t.Fatal(err)
	}
	go func() {
		defer close(s.done)
		s.read(out)
		_ = s.cmd.Wait()
	}()
	return s
}

// proc is one process of a sample.
type proc struct {
	pid, ppid int
	argv      []string
}

func (s *sampler) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	var procs []proc
	var files []string
	for sc.Scan() {
		l := sc.Text()
		switch {
		case strings.HasPrefix(l, "T "):
			procs, files = procs[:0], files[:0]
		case strings.HasPrefix(l, "P "):
			stat := l[2:]
			i := strings.LastIndexByte(stat, ')')
			if i < 0 {
				continue
			}
			pid, _ := strconv.Atoi(strings.Fields(stat[:strings.IndexByte(stat, ' ')+1])[0])
			f := strings.Fields(stat[i+1:])
			if len(f) < 2 {
				continue
			}
			ppid, _ := strconv.Atoi(f[1])
			procs = append(procs, proc{pid: pid, ppid: ppid})
		case strings.HasPrefix(l, "C "):
			if len(procs) > 0 {
				procs[len(procs)-1].argv = strings.Split(strings.TrimSuffix(l[2:], "\x01"), "\x01")
			}
		case strings.HasPrefix(l, "F "):
			files = append(files, l[2:])
		case l == "E":
			s.evaluate(procs, files)
		}
	}
}

// evaluate checks one sample.
func (s *sampler) evaluate(procs []proc, files []string) {
	parent := map[int]int{}
	server := 0
	for _, p := range procs {
		parent[p.pid] = p.ppid
		if len(p.argv) >= 2 && p.argv[0] == "/app/bunkarr" && p.argv[1] == "serve" {
			server = p.pid
		}
	}
	under := func(pid int) bool {
		for i := 0; i < 64 && pid > 1; i++ {
			if pid = parent[pid]; pid == server {
				return true
			}
		}
		return false
	}
	engine := false
	var problems []string
	for _, p := range procs {
		if server == 0 || p.pid == server || !under(p.pid) {
			continue
		}
		joined := strings.Join(p.argv, " ")
		if strings.Contains(joined, "restic") || strings.Contains(joined, "rclone") {
			engine = true
		}
		if found := s.b.o.secrets.find(strings.Join(p.argv, "\n")); len(found) > 0 {
			problems = append(problems, fmt.Sprintf("argv of pid %d (%s): %s", p.pid, shortArgs(p.argv), strings.Join(found, "; ")))
		}
	}
	for _, f := range files {
		// "<perm> <uid> <raw mode hex> <path>" (sampler.sh uses stat -c 'F %a %u %F %n', read below)
		parts := strings.SplitN(f, " ", 3)
		if len(parts) < 3 {
			continue
		}
		perm, rest := parts[0], parts[2]
		isDir := strings.HasPrefix(rest, "directory ")
		path := rest[strings.Index(rest, " /")+1:]
		switch {
		case strings.HasPrefix(path, "/config/run/"):
			if base := filepath.Base(path); base == "password" || base == "known_hosts" || base == "ca.pem" {
				problems = append(problems, "a secret file under <config>/run although /dev/shm is a tmpfs: "+path)
			}
		case strings.HasPrefix(path, "/dev/shm/bunkarr-run/"):
			rel := strings.TrimPrefix(path, "/dev/shm/bunkarr-run/")
			depth := strings.Count(rel, "/")
			switch {
			case isDir && depth == 0:
				s.mu.Lock()
				s.secretDirs[path] = true
				s.mu.Unlock()
				if perm != "700" {
					problems = append(problems, fmt.Sprintf("secret directory %s has mode %s, want 700", path, perm))
				}
			case !isDir:
				s.mu.Lock()
				s.secretFile[path] = true
				s.mu.Unlock()
				if perm != "600" {
					problems = append(problems, fmt.Sprintf("secret file %s has mode %s, want 600", path, perm))
				}
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.samples++
	if engine {
		s.engine++
	}
	if len(s.problems) < 50 {
		s.problems = append(s.problems, problems...)
	}
}

// stop ends the sampler (the docker exec is killed; a killed container ends it by itself).
func (s *sampler) stop() {
	if s == nil || s.cmd == nil {
		return
	}
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	select {
	case <-s.done:
	case <-time.After(30 * time.Second):
	}
}

// report fails the test on every problem the sampler found and logs what it saw.
func (s *sampler) report(t *testing.T, c string) {
	t.Helper()
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.problems {
		t.Errorf("sampler of %s: %s", c, p)
	}
	t.Logf("sampler of %s: %d samples, %d with an engine process, %d secret run directories and %d secret files seen", c,
		s.samples, s.engine, len(s.secretDirs), len(s.secretFile))
}

// dumpDiagnostics logs the argv logs and the MinIO and SFTP logs of a failed test.
func (o *offsite) dumpDiagnostics() {
	o.mu.Lock()
	list := slices.Clone(o.bunkarrs)
	o.mu.Unlock()
	for _, b := range list {
		out, _ := exec.Command("docker", "exec", b.c, "tail", "-n", "60", argvLog).CombinedOutput()
		o.t.Logf("---- argv log of %s (last 60 lines) ----\n%s", b.c, out)
	}
	for _, c := range []string{o.minio, o.sftpContainer()} {
		if c != "" {
			out, _ := exec.Command("docker", "logs", "--tail", "40", c).CombinedOutput()
			o.t.Logf("---- docker logs %s (last 40 lines) ----\n%s", c, out)
		}
	}
}

// sftpContainer is the SFTP server's container ("" when none was started).
func (o *offsite) sftpContainer() string {
	if o.sftp == nil {
		return ""
	}
	return o.sftp.c
}
