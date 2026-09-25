//go:build e2e

package e2e

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// dockerEnv owns the containers, volumes, networks and built images one Docker test creates
// (removed when the test ends; pulled images are kept). Every name starts with a per-test prefix.
type dockerEnv struct {
	t      *testing.T
	image  string
	prefix string

	containers []string
	volumes    []string
	networks   []string
	images     []string
}

// newDockerEnv skips the test unless BUNKARR_E2E_IMAGE names a Bunkarr image (see
// docker/test-kill.sh), and requires that image to exist locally.
func newDockerEnv(t *testing.T) *dockerEnv {
	t.Helper()
	image := os.Getenv("BUNKARR_E2E_IMAGE")
	if image == "" {
		t.Skip("BUNKARR_E2E_IMAGE is not set (run docker/test-kill.sh or docker/test-plex-restore.sh)")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatalf("BUNKARR_E2E_IMAGE is set but docker is not installed: %v", err)
	}
	var rnd [3]byte
	_, _ = rand.Read(rnd[:])
	d := &dockerEnv{t: t, image: image, prefix: fmt.Sprintf("bunkarr-e2e-%d-%s", os.Getpid(), hex.EncodeToString(rnd[:]))}
	if _, err := d.try("image", "inspect", "--format", "{{.Id}}", image); err != nil {
		t.Fatalf("image %s: %v (build it with make docker)", image, err)
	}
	t.Cleanup(d.cleanup)
	return d
}

// cleanup prints the containers' logs when the test failed, then removes what the test created.
func (d *dockerEnv) cleanup() {
	if d.t.Failed() {
		for _, c := range d.containers {
			out, _ := exec.Command("docker", "logs", "--tail", "80", c).CombinedOutput()
			d.t.Logf("---- docker logs %s (last 80 lines) ----\n%s", c, out)
		}
	}
	// Newest first: a client goes before the server whose share it mounts.
	for _, c := range slices.Backward(d.containers) {
		_, _ = d.try("rm", "-f", "-v", c)
	}
	for _, v := range d.volumes {
		if _, err := d.try("volume", "rm", "-f", v); err != nil {
			d.t.Logf("remove volume %s: %v", v, err)
		}
	}
	for _, n := range d.networks {
		if _, err := d.try("network", "rm", n); err != nil {
			d.t.Logf("remove network %s: %v", n, err)
		}
	}
	for _, i := range d.images {
		if _, err := d.try("image", "rm", i); err != nil {
			d.t.Logf("remove image %s: %v", i, err)
		}
	}
}

// try runs docker and returns its trimmed stdout; the error includes stderr.
func (d *dockerEnv) try(args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("docker", args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(stdout.String()), fmt.Errorf("docker %s: %w: %s", shortArgs(args), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// docker runs docker and fails the test on an error.
func (d *dockerEnv) docker(args ...string) string {
	d.t.Helper()
	out, err := d.try(args...)
	if err != nil {
		d.t.Fatal(err)
	}
	return out
}

// shortArgs abbreviates a docker command line for messages (long scripts are cut).
func shortArgs(args []string) string {
	var out []string
	for _, a := range args {
		if len(a) > 80 {
			a = a[:77] + "..."
		}
		out = append(out, a)
	}
	return strings.Join(out, " ")
}

// name returns the full name of a test object.
func (d *dockerEnv) name(short string) string {
	return d.prefix + "-" + short
}

// volume creates a named volume.
func (d *dockerEnv) volume(short string) string {
	d.t.Helper()
	n := d.name(short)
	d.docker("volume", "create", n)
	d.volumes = append(d.volumes, n)
	return n
}

// network creates a user-defined bridge network and returns its name and IPv4 subnet.
func (d *dockerEnv) network(short string) (string, *net.IPNet) {
	d.t.Helper()
	n := d.name(short)
	d.docker("network", "create", n)
	d.networks = append(d.networks, n)
	out := d.docker("network", "inspect", "--format", "{{range .IPAM.Config}}{{.Subnet}} {{end}}", n)
	for _, f := range strings.Fields(out) {
		if _, ipn, err := net.ParseCIDR(f); err == nil && ipn.IP.To4() != nil {
			return n, ipn
		}
	}
	d.t.Fatalf("network %s has no IPv4 subnet: %q", n, out)
	return "", nil
}

// build builds an image from a Dockerfile without a context and returns its tag.
func (d *dockerEnv) build(short, dockerfile string) string {
	d.t.Helper()
	tag := d.name(short) + ":test"
	// The builder of the current Docker context (docker driver) builds FROM local images such as
	// d.image and stores the result there; a selected docker-container builder (CI's
	// docker/setup-buildx-action selects one) sees neither.
	builder := d.docker("context", "show")
	var out bytes.Buffer
	cmd := exec.Command("docker", "build", "--builder", builder, "-q", "-t", tag, "-")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(dockerfile), &out, &out
	if err := cmd.Run(); err != nil {
		d.t.Fatalf("docker build %s: %v\n%s", tag, err, out.String())
	}
	d.images = append(d.images, tag)
	return tag
}

// run starts a detached container (args are docker run options, the image and its arguments).
func (d *dockerEnv) run(short string, args ...string) string {
	d.t.Helper()
	n := d.name(short)
	d.containers = append(d.containers, n)
	d.docker(append([]string{"run", "-d", "--name", n}, args...)...)
	return n
}

// oneShot runs a container to completion and returns its stdout.
func (d *dockerEnv) oneShot(args ...string) string {
	d.t.Helper()
	return d.docker(append([]string{"run", "--rm"}, args...)...)
}

// helper starts an idle container of image with the given docker run options (mounts), for
// docker cp and docker exec.
func (d *dockerEnv) helper(short, image string, opts ...string) string {
	d.t.Helper()
	args := append(append([]string{}, opts...), "--entrypoint", "sleep", image, "3600")
	return d.run(short, args...)
}

// remove removes a container now.
func (d *dockerEnv) remove(c string) {
	d.t.Helper()
	d.docker("rm", "-f", "-v", c)
	for i, n := range d.containers {
		if n == c {
			d.containers = append(d.containers[:i], d.containers[i+1:]...)
			break
		}
	}
}

// execOK reports whether a command in a running container exits 0.
func (d *dockerEnv) execOK(c string, cmd ...string) bool {
	_, err := d.try(append([]string{"exec", c}, cmd...)...)
	return err == nil
}

// running reports whether a container is running.
func (d *dockerEnv) running(c string) bool {
	return d.docker("inspect", "--format", "{{.State.Running}}", c) == "true"
}

// statusLine matches the status line busybox wget -S prints for each response.
var statusLine = regexp.MustCompile(`HTTP/\d(?:\.\d)?\s+(\d{3})`)

// containerAPI calls the API of a Bunkarr container with busybox wget through docker exec, so
// the test needs no published port (docker-in-docker CI runners).
type containerAPI struct {
	*client
	d         *dockerEnv
	container string
	key       string
}

// newContainerAPI returns the API of container c.
func newContainerAPI(d *dockerEnv, c string) *containerAPI {
	a := &containerAPI{d: d, container: c}
	a.client = &client{t: d.t, do: func(method, path string, body any) (int, []byte, error) {
		code, b, _, err := a.wget(method, path, body)
		return code, b, err
	}}
	return a
}

// wget performs one call and returns the status, the body (empty for errors: busybox wget does
// not print it) and the response headers.
func (a *containerAPI) wget(method, path string, body any, headers ...string) (int, []byte, string, error) {
	args := []string{"exec", a.container, "wget", "-S", "-q", "-O", "-", "-T", "60"}
	if a.key != "" {
		args = append(args, "--header", "X-Api-Key: "+a.key)
	}
	for _, h := range headers {
		args = append(args, "--header", h)
	}
	switch method {
	case "GET":
	case "POST":
		b := []byte("{}")
		if body != nil {
			var err error
			if b, err = json.Marshal(body); err != nil {
				return 0, nil, "", err
			}
		}
		args = append(args, "--header", "Content-Type: application/json", "--post-data", string(b))
	default:
		return 0, nil, "", fmt.Errorf("busybox wget cannot send %s", method)
	}
	args = append(args, "http://127.0.0.1:8787/api/v1"+path)
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("docker", args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	m := statusLine.FindAllStringSubmatch(stderr.String(), -1)
	if len(m) == 0 {
		if runErr == nil {
			runErr = errors.New("no HTTP status in the wget output")
		}
		return 0, nil, "", fmt.Errorf("%s %s in %s: %w: %s", method, path, a.container, runErr, strings.TrimSpace(stderr.String()))
	}
	var code int
	_, _ = fmt.Sscanf(m[len(m)-1][1], "%d", &code)
	return code, stdout.Bytes(), stderr.String(), nil
}

// waitHealthy waits until /api/v1/health answers 200 (the container was just started).
func (a *containerAPI) waitHealthy(timeout time.Duration) {
	a.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if code, _, _, err := a.wget("GET", "/health", nil); err == nil && code == 200 {
			return
		}
		if !a.d.running(a.container) {
			a.t.Fatalf("container %s stopped during start-up", a.container)
		}
		if time.Now().After(deadline) {
			a.t.Fatalf("container %s did not answer /api/v1/health within %s", a.container, timeout)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// sessionCookie matches the session cookie of a first-run setup answer.
var sessionCookie = regexp.MustCompile(`(?i)Set-Cookie:\s*bunkarr_session=([^;\s]+)`)

// setup completes the first-run setup and reads the API key.
func (a *containerAPI) setup() {
	a.t.Helper()
	code, _, hdr, err := a.wget("POST", "/auth/setup", map[string]string{"username": e2eUser, "password": e2ePassword})
	if err != nil || code != 201 {
		a.t.Fatalf("first-run setup: HTTP %d, %v", code, err)
	}
	m := sessionCookie.FindStringSubmatch(hdr)
	if m == nil {
		a.t.Fatalf("no session cookie after setup:\n%s", hdr)
	}
	code, b, _, err := a.wget("GET", "/settings/general", nil, "Cookie: bunkarr_session="+m[1])
	var gs struct {
		APIKey string `json:"apiKey"`
	}
	if err != nil || code != 200 || json.Unmarshal(b, &gs) != nil || gs.APIKey == "" {
		a.t.Fatalf("settings/general: HTTP %d, %v, %s", code, err, b)
	}
	a.key = gs.APIKey
}

// cpIn copies the contents of a local directory into dir of a running container.
func (d *dockerEnv) cpIn(localDir, c, dir string) {
	d.t.Helper()
	d.docker("exec", c, "mkdir", "-p", dir)
	d.docker("cp", localDir+string(filepath.Separator)+".", c+":"+dir)
}

// cpOut copies dir of a container into a new local directory and returns its path.
func (d *dockerEnv) cpOut(c, dir, localDir string) string {
	d.t.Helper()
	mkdirAll(d.t, localDir)
	d.docker("cp", c+":"+dir+"/.", localDir)
	return localDir
}
