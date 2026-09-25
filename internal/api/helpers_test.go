package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// resolvedTempDir is t.TempDir with symlinks resolved (macOS: /var → /private/var), so paths
// compare equal to the resolved ones the stores keep.
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// mkdir creates dir (and parents) under e.base and returns its absolute path.
func (e *env) mkdir(t *testing.T, rel string) string {
	t.Helper()
	p := filepath.Join(e.base, rel)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// writeFile writes content to path (creating parents) with a fixed mtime.
func writeFile(t *testing.T, path, content string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// raw sends a request with the API key and returns the status and body. body is sent as is when
// it is a string, JSON-encoded otherwise (nil: no body).
func (e *env) raw(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		enc, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(enc)
	}
	req, err := http.NewRequest(method, e.srv.URL+"/api/v1"+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Api-Key", e.auth.APIKey())
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, out
}

// call is raw that decodes a JSON answer into out (when out is not nil) and fails the test when
// the status is not want.
func (e *env) call(t *testing.T, want int, method, path string, body, out any) {
	t.Helper()
	code, raw := e.raw(t, method, path, body)
	if code != want {
		t.Fatalf("%s %s: status %d, want %d: %s", method, path, code, want, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s %s: decode %s: %v", method, path, raw, err)
		}
	}
}

// status sends a request and returns only the status and the error message.
func (e *env) status(t *testing.T, method, path string, body any) (int, string) {
	t.Helper()
	code, raw := e.raw(t, method, path, body)
	var m struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &m)
	return code, m.Message
}

// waitJob polls GET /jobs/{id} until the job is final.
func (e *env) waitJob(t *testing.T, id int64) jobs.Job {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var j jobs.Job
		e.call(t, 200, "GET", fmt.Sprintf("/jobs/%d", id), nil, &j)
		if j.Status.Final() {
			return j
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %d did not finish", id)
	return jobs.Job{}
}

// idOf decodes {"id": n}.
func idOf(t *testing.T, raw []byte) int64 {
	t.Helper()
	var v struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(raw, &v); err != nil || v.ID == 0 {
		t.Fatalf("no id in %s", raw)
	}
	return v.ID
}

// createSource adds a source over the API and returns its id.
func (e *env) createSource(t *testing.T, name, path string) int64 {
	t.Helper()
	code, raw := e.raw(t, "POST", "/sources", map[string]any{"name": name, "path": path})
	if code != 201 {
		t.Fatalf("create source: %d %s", code, raw)
	}
	return idOf(t, raw)
}

// createDestination adds a local destination (allowLocal) over the API and returns its id.
func (e *env) createDestination(t *testing.T, name, target string, sourceIDs []int64, extra map[string]any) int64 {
	t.Helper()
	body := map[string]any{"name": name, "target": target, "sourceIds": sourceIDs, "allowLocal": true}
	for k, v := range extra {
		body[k] = v
	}
	code, raw := e.raw(t, "POST", "/destinations", body)
	if code != 201 {
		t.Fatalf("create destination: %d %s", code, raw)
	}
	return idOf(t, raw)
}
