//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// API shapes as docs/design/phase1.md §6.1 and §7 define them. They are declared here instead of
// importing the product's types, so a renamed or dropped JSON field fails the suite.

// apiJob is a Job.
type apiJob struct {
	ID       int64           `json:"id"`
	Type     string          `json:"type"`
	Status   string          `json:"status"`
	Trigger  string          `json:"trigger"`
	DryRun   bool            `json:"dryRun"`
	Attempt  int             `json:"attempt"`
	Stats    json.RawMessage `json:"stats"`
	Warnings int             `json:"warnings"`
	Summary  string          `json:"summary"`
	Error    string          `json:"error"`
}

// final reports whether the job reached a terminal status.
func (j apiJob) final() bool {
	return j.Status != "queued" && j.Status != "running"
}

// syncStats are a sync job's stats (design §6.1).
type syncStats struct {
	DryRun         bool  `json:"dryRun"`
	FilesPlanned   int64 `json:"filesPlanned"`
	FilesCopied    int64 `json:"filesCopied"`
	FilesUpdated   int64 `json:"filesUpdated"`
	FilesMoved     int64 `json:"filesMoved"`
	FilesAdopted   int64 `json:"filesAdopted"`
	FilesLinked    int64 `json:"filesLinked"`
	FilesRetained  int64 `json:"filesRetained"`
	FilesDisplaced int64 `json:"filesDisplaced"`
	FilesHeld      int64 `json:"filesHeld"`
	FilesFailed    int64 `json:"filesFailed"`
	FilesSkipped   int64 `json:"filesSkipped"`
	BytesPlanned   int64 `json:"bytesPlanned"`
	BytesCopied    int64 `json:"bytesCopied"`
}

// verifyStats are the verify job's counters this suite checks.
type verifyStats struct {
	FilesChecked  int64 `json:"filesChecked"`
	FilesVerified int64 `json:"filesVerified"`
	FilesMissing  int64 `json:"filesMissing"`
	FilesFailed   int64 `json:"filesFailed"`
}

// sourceStats are Source.stats.
type sourceStats struct {
	Files          int64 `json:"files"`
	Bytes          int64 `json:"bytes"`
	UniqueBytes    int64 `json:"uniqueBytes"`
	HardlinkGroups int64 `json:"hardlinkGroups"`
	Skipped        int64 `json:"skipped"`
}

// apiSource is a Source.
type apiSource struct {
	ID                int64       `json:"id"`
	Name              string      `json:"name"`
	Path              string      `json:"path"`
	DestFolder        string      `json:"destFolder"`
	PlexIntegrationID *int64      `json:"plexIntegrationId"`
	PlexSectionID     string      `json:"plexSectionId"`
	PlexPath          string      `json:"plexPath"`
	LastScanStatus    string      `json:"lastScanStatus"`
	Stats             sourceStats `json:"stats"`
}

// apiDestination is a Destination.
type apiDestination struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Target       string `json:"target"`
	Capabilities struct {
		Hardlinks bool `json:"hardlinks"`
	} `json:"capabilities"`
}

// apiItem is a job Item.
type apiItem struct {
	ID      int64  `json:"id"`
	RelPath string `json:"relPath"`
	Action  string `json:"action"`
	Status  string `json:"status"`
	Bytes   int64  `json:"bytes"`
	Error   string `json:"error"`
}

// apiPage is a paged answer.
type apiPage[T any] struct {
	TotalRecords int64 `json:"totalRecords"`
	Records      []T   `json:"records"`
}

// decodeStats decodes a job's stats into v.
func decodeStats[T any](t *testing.T, j apiJob) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(j.Stats, &v); err != nil {
		t.Fatalf("job %d stats %s: %v", j.ID, j.Stats, err)
	}
	return v
}

// e2e credentials of the first-run setup.
const (
	e2eUser     = "e2e"
	e2ePassword = "e2e-password-not-secret"
)

// client is the API as a test uses it; do performs one call (path relative to /api/v1). The
// native server calls it over HTTP, the Docker tests through `docker exec ... wget`.
type client struct {
	t  *testing.T
	do func(method, path string, body any) (int, []byte, error)
}

// server is one bunkarr process on a config directory. Restarts keep the directory and the port.
type server struct {
	*client
	bin  string
	root string
	cfg  string
	port int
	base string
	key  string
	hc   *http.Client
	logs []string

	proc *exec.Cmd
	done chan struct{}
}

// newServer returns a stopped server whose config directory is <root>/config. Its process is
// killed when the test ends; the server logs are printed when the test failed.
func newServer(t *testing.T, root string) *server {
	t.Helper()
	s := &server{bin: bunkarrBinary(t), root: root, cfg: filepath.Join(root, "config"), port: freePort(t),
		hc: &http.Client{Timeout: 30 * time.Second}}
	s.client = &client{t: t, do: s.request}
	s.base = "http://127.0.0.1:" + strconv.Itoa(s.port) + "/api/v1"
	t.Cleanup(func() {
		if s.running() {
			_ = s.proc.Process.Kill()
			<-s.done
		}
		if t.Failed() {
			for _, l := range s.logs {
				t.Logf("---- %s (last 80 lines) ----\n%s", filepath.Base(l), tail(l, 80))
			}
		}
	})
	return s
}

// freePort returns a TCP port that was free a moment ago.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// baseEnv is the process environment without BUNKARR_* variables of the developer's shell.
func baseEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "BUNKARR_") {
			env = append(env, kv)
		}
	}
	return env
}

// running reports whether the process is alive.
func (s *server) running() bool {
	if s.proc == nil {
		return false
	}
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// start runs the binary with extra environment variables and waits until /health answers.
func (s *server) start(env ...string) {
	s.t.Helper()
	if s.running() {
		s.t.Fatal("start: bunkarr is already running")
	}
	logPath := filepath.Join(s.root, fmt.Sprintf("server-%d.log", len(s.logs)+1))
	f, err := os.Create(logPath)
	if err != nil {
		s.t.Fatal(err)
	}
	s.logs = append(s.logs, logPath)
	cmd := exec.Command(s.bin, "serve", "--config", s.cfg, "--bind", "127.0.0.1", "--port", strconv.Itoa(s.port), "--log-level", "debug")
	cmd.Env = append(baseEnv(), env...)
	cmd.Stdout, cmd.Stderr = f, f
	if err := cmd.Start(); err != nil {
		_ = f.Close()
		s.t.Fatalf("start bunkarr: %v", err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		_ = f.Close()
		close(done)
	}()
	s.proc, s.done = cmd, done
	deadline := time.Now().Add(60 * time.Second)
	for {
		select {
		case <-done:
			s.t.Fatalf("bunkarr exited during start-up (%v); log:\n%s", cmd.ProcessState, tail(logPath, 60))
		default:
		}
		if code, _, err := s.request("GET", "/health", nil); err == nil && code == http.StatusOK {
			return
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("bunkarr did not answer /api/v1/health within 60 s; log:\n%s", tail(logPath, 60))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// stop sends SIGTERM and requires a clean exit (running jobs are re-queued by the server).
func (s *server) stop() {
	s.t.Helper()
	if !s.running() {
		return
	}
	if err := s.proc.Process.Signal(syscall.SIGTERM); err != nil {
		s.t.Fatalf("SIGTERM: %v", err)
	}
	select {
	case <-s.done:
	case <-time.After(60 * time.Second):
		_ = s.proc.Process.Kill()
		<-s.done
		s.t.Fatal("bunkarr did not stop within 60 s of SIGTERM")
	}
	if st := s.proc.ProcessState; !st.Success() {
		s.t.Fatalf("bunkarr exited with %v after SIGTERM", st)
	}
}

// kill sends SIGKILL (kill -9) and waits until the process is gone.
func (s *server) kill() {
	s.t.Helper()
	if !s.running() {
		s.t.Fatal("kill: bunkarr is not running")
	}
	if err := s.proc.Process.Signal(syscall.SIGKILL); err != nil {
		s.t.Fatalf("SIGKILL: %v", err)
	}
	select {
	case <-s.done:
	case <-time.After(30 * time.Second):
		s.t.Fatal("bunkarr survived SIGKILL for 30 s")
	}
}

// request performs one API call (path is relative to /api/v1) with the API key once known.
func (s *server) request(method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, s.base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if s.key != "" {
		req.Header.Set("X-Api-Key", s.key)
	}
	res, err := s.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	return res.StatusCode, b, err
}

// call performs an API call, requires status want and decodes the answer into out (if not nil).
func (s *client) call(want int, method, path string, body, out any) {
	s.t.Helper()
	code, b, err := s.do(method, path, body)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	if code != want {
		s.t.Fatalf("%s %s: HTTP %d, want %d: %s", method, path, code, want, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			s.t.Fatalf("%s %s: decode %s: %v", method, path, b, err)
		}
	}
}

// setup completes the first-run setup (a user, logged in by cookie) and reads the API key every
// later call uses.
func (s *server) setup() {
	s.t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		s.t.Fatal(err)
	}
	hc := &http.Client{Jar: jar, Timeout: 30 * time.Second}
	post := func(path string, body any) *http.Response {
		b, _ := json.Marshal(body)
		res, err := hc.Post(s.base+path, "application/json", bytes.NewReader(b))
		if err != nil {
			s.t.Fatalf("POST %s: %v", path, err)
		}
		return res
	}
	res := post("/auth/setup", map[string]string{"username": e2eUser, "password": e2ePassword})
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		s.t.Fatalf("first-run setup: HTTP %d: %s", res.StatusCode, b)
	}
	res, err = hc.Get(s.base + "/settings/general")
	if err != nil {
		s.t.Fatal(err)
	}
	defer res.Body.Close()
	var gs struct {
		APIKey string `json:"apiKey"`
	}
	if err := json.NewDecoder(res.Body).Decode(&gs); err != nil || res.StatusCode != http.StatusOK || gs.APIKey == "" {
		s.t.Fatalf("settings/general after setup: HTTP %d, %v", res.StatusCode, err)
	}
	s.key = gs.APIKey
}

// job returns job id.
func (s *client) job(id int64) apiJob {
	s.t.Helper()
	var j apiJob
	s.call(http.StatusOK, "GET", fmt.Sprintf("/jobs/%d", id), nil, &j)
	return j
}

// waitJob polls job id until it is final.
func (s *client) waitJob(id int64, timeout time.Duration) apiJob {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		j := s.job(id)
		if j.final() {
			return j
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("job %d still %s after %s: %+v", id, j.Status, timeout, j)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// startSync queues a sync of destination destID (body: dryRun, allowChanges).
func (s *client) startSync(destID int64, body map[string]any) apiJob {
	s.t.Helper()
	var j apiJob
	s.call(http.StatusAccepted, "POST", fmt.Sprintf("/destinations/%d/sync", destID), body, &j)
	if j.Type != "sync" || j.ID == 0 {
		s.t.Fatalf("queued sync: %+v", j)
	}
	return j
}

// runSync queues a sync and waits for it.
func (s *client) runSync(destID int64, body map[string]any) apiJob {
	s.t.Helper()
	return s.waitJob(s.startSync(destID, body).ID, 5*time.Minute)
}

// runVerify queues a verify job of destination destID and waits for it.
func (s *client) runVerify(destID int64) apiJob {
	s.t.Helper()
	var j apiJob
	s.call(http.StatusAccepted, "POST", fmt.Sprintf("/destinations/%d/verify", destID), nil, &j)
	return s.waitJob(j.ID, 5*time.Minute)
}

// items returns all items of a job matching query (e.g. "status=held").
func (s *client) items(jobID int64, query string) []apiItem {
	s.t.Helper()
	var all []apiItem
	for page := 1; ; page++ {
		var p apiPage[apiItem]
		s.call(http.StatusOK, "GET", fmt.Sprintf("/jobs/%d/items?pageSize=500&page=%d&%s", jobID, page, query), nil, &p)
		all = append(all, p.Records...)
		if len(p.Records) == 0 || int64(len(all)) >= p.TotalRecords {
			return all
		}
	}
}

// createSource saves a source over path.
func (s *client) createSource(name, path string) apiSource {
	s.t.Helper()
	var src apiSource
	s.call(http.StatusCreated, "POST", "/sources", map[string]any{"name": name, "path": path}, &src)
	return src
}

// source reads source id.
func (s *client) source(id int64) apiSource {
	s.t.Helper()
	var src apiSource
	s.call(http.StatusOK, "GET", fmt.Sprintf("/sources/%d", id), nil, &src)
	return src
}

// createDestination creates a local destination (allowLocal) at target for the sources, with
// verify mode full so every copy is re-read and every verify job re-hashes every file.
func (s *client) createDestination(name, target string, sourceIDs ...int64) apiDestination {
	s.t.Helper()
	var d apiDestination
	s.call(http.StatusCreated, "POST", "/destinations", map[string]any{
		"name": name, "target": target, "sourceIds": sourceIDs, "allowLocal": true,
		"settings": map[string]any{"verify": map[string]any{"mode": "full"}},
	}, &d)
	return d
}

// destination reads destination id.
func (s *client) destination(id int64) apiDestination {
	s.t.Helper()
	var d apiDestination
	s.call(http.StatusOK, "GET", fmt.Sprintf("/destinations/%d", id), nil, &d)
	return d
}

// tail returns the last n lines of a file.
func tail(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return err.Error()
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if len(lines) > n {
			lines = lines[1:]
		}
	}
	return strings.Join(lines, "\n")
}
