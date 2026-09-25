package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/notify"
)

const appriseURLs = "tgram://bottoken123456789/chatid987654321, discord://webhookid/webhooktokenABCDEFG"

// fakeApprise is an Apprise API that records the requests it gets and answers status.
type fakeApprise struct {
	srv    *httptest.Server
	mu     sync.Mutex
	status int
	got    []map[string]any
	paths  []string
}

func newFakeApprise(t *testing.T) *fakeApprise {
	t.Helper()
	f := &fakeApprise{status: http.StatusOK}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		f.mu.Lock()
		f.got = append(f.got, m)
		f.paths = append(f.paths, r.URL.Path)
		status := f.status
		f.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeApprise) requests() ([]map[string]any, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.got...), append([]string(nil), f.paths...)
}

func TestNotificationsNeverReturnURLs(t *testing.T) {
	e := newEnv(t, nil)
	ap := newFakeApprise(t)
	code, raw := e.raw(t, "POST", "/notifications", map[string]any{"name": "Phone", "kind": "apprise", "apiUrl": ap.srv.URL + "/",
		"urls": appriseURLs, "onSuccess": true})
	if code != 201 {
		t.Fatalf("create: %d %s", code, raw)
	}
	var n notify.Notification
	if err := json.Unmarshal(raw, &n); err != nil {
		t.Fatal(err)
	}
	if !n.HasURLs || !n.Enabled || !n.OnFailure || !n.OnWarning || !n.OnSuccess || n.APIURL != ap.srv.URL {
		t.Fatalf("created: %+v", n)
	}
	path := fmt.Sprintf("/notifications/%d", n.ID)
	for _, check := range []struct{ method, path string }{{"GET", "/notifications"}, {"PUT", path}} {
		var body any
		if check.method == "PUT" {
			body = map[string]any{"name": "Phone", "apiUrl": ap.srv.URL, "onSuccess": false}
		}
		code, raw := e.raw(t, check.method, check.path, body)
		if code != 200 {
			t.Fatalf("%s %s: %d %s", check.method, check.path, code, raw)
		}
		for _, secret := range []string{"bottoken123456789", "webhooktokenABCDEFG", `"urls"`} {
			if strings.Contains(string(raw), secret) {
				t.Fatalf("%s %s returned %q: %s", check.method, check.path, secret, raw)
			}
		}
	}
	var list []notify.Notification
	e.call(t, 200, "GET", "/notifications", nil, &list)
	if len(list) != 1 || !list[0].HasURLs || list[0].OnSuccess {
		t.Fatalf("list after an update that kept the URLs: %+v", list)
	}
	for _, b := range []map[string]any{
		{"name": "X", "apiUrl": ap.srv.URL},
		{"name": "X", "apiUrl": "ftp://apprise", "urls": appriseURLs},
		{"name": "", "apiUrl": ap.srv.URL, "urls": appriseURLs},
		{"name": "X", "apiUrl": ap.srv.URL, "urls": appriseURLs, "kind": "email"},
		{"name": "phone", "apiUrl": ap.srv.URL, "urls": appriseURLs},
		{"name": "X", "apiUrl": ap.srv.URL, "urls": appriseURLs, "secret": "x"},
	} {
		if code, msg := e.status(t, "POST", "/notifications", b); code != 400 || strings.Contains(msg, "bottoken") {
			t.Errorf("create %v: %d %q, want 400", b, code, msg)
		}
	}
	if code, _ := e.status(t, "PUT", "/notifications/999", map[string]any{"name": "X", "apiUrl": ap.srv.URL, "urls": appriseURLs}); code != 404 {
		t.Errorf("update of a missing notification: %d", code)
	}
	e.call(t, 204, "DELETE", path, nil, nil)
	if code, _ := e.status(t, "DELETE", path, nil); code != 404 {
		t.Errorf("second delete: %d", code)
	}
}

func TestNotificationTestButton(t *testing.T) {
	e := newEnv(t, nil)
	ap := newFakeApprise(t)
	// An unsaved form.
	var res testResult
	e.call(t, 200, "POST", "/notifications/test", map[string]any{"name": "Phone", "apiUrl": ap.srv.URL, "urls": appriseURLs}, &res)
	if !res.OK {
		t.Fatalf("test: %+v", res)
	}
	got, paths := ap.requests()
	if len(got) != 1 || paths[0] != "/notify" || got[0]["urls"] != appriseURLs || got[0]["type"] != "info" {
		t.Fatalf("apprise got %v %v", paths, got)
	}
	// A saved notification, by id; and its edit form without the URLs.
	var n notify.Notification
	e.call(t, 201, "POST", "/notifications", map[string]any{"name": "Phone", "apiUrl": ap.srv.URL, "urls": appriseURLs}, &n)
	e.call(t, 200, "POST", "/notifications/test", map[string]any{"id": n.ID}, &res)
	e.call(t, 200, "POST", "/notifications/test", map[string]any{"id": n.ID, "name": "Phone", "apiUrl": ap.srv.URL}, &res)
	if got, _ := ap.requests(); !res.OK || len(got) != 3 || got[2]["urls"] != appriseURLs {
		t.Fatalf("tests of the saved notification: %+v %v", res, got)
	}
	// A refused delivery is ok: false with a message that holds no URL.
	ap.mu.Lock()
	ap.status = http.StatusBadRequest
	ap.mu.Unlock()
	code, raw := e.raw(t, "POST", "/notifications/test", map[string]any{"id": n.ID})
	if code != 200 || strings.Contains(string(raw), "bottoken") {
		t.Fatalf("failed delivery: %d %s", code, raw)
	}
	_ = json.Unmarshal(raw, &res)
	if res.OK || res.Message == "" {
		t.Fatalf("failed delivery: %+v", res)
	}
	if code, _ := e.status(t, "POST", "/notifications/test", map[string]any{"id": 999}); code != 404 {
		t.Errorf("test of a missing notification: %d", code)
	}
	if code, _ := e.status(t, "POST", "/notifications/test", map[string]any{"apiUrl": ap.srv.URL}); code != 400 {
		t.Errorf("test without URLs: %d", code)
	}
}

// TestFinishedJobsNotify: the dispatcher is wired to the job manager with job descriptions.
func TestFinishedJobsNotify(t *testing.T) {
	e := newEnv(t, nil)
	ap := newFakeApprise(t)
	e.call(t, 201, "POST", "/notifications", map[string]any{"name": "Phone", "apiUrl": ap.srv.URL, "urls": appriseURLs}, nil)
	id := e.createDestination(t, "UNAS", e.mkdir(t, "nas"), nil, nil)
	// A failing sync (the marker is gone) notifies with a failure.
	if err := removeMarker(e, id); err != nil {
		t.Fatal(err)
	}
	var j jobs.Job
	e.call(t, 202, "POST", fmt.Sprintf("/destinations/%d/sync", id), nil, &j)
	e.waitJob(t, j.ID)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ := ap.requests(); len(got) > 0 {
			if got[0]["type"] != "failure" || got[0]["title"] != "Bunkarr: Sync to UNAS failed" {
				t.Fatalf("notification: %v", got[0])
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no notification was sent")
}

func removeMarker(e *env, destID int64) error {
	d, err := e.app.Destinations.Get(context.Background(), destID)
	if err != nil {
		return err
	}
	return os.Remove(filepath.Join(d.Target, ".bunkarr", "destination.json"))
}

// TestStoredAppriseURLsOnlyGoToTheStoredAPI: a caller who does not know the Apprise URLs cannot
// make Bunkarr post them to an API URL of its choice, through the Test button or by changing the
// API URL (S8).
func TestStoredAppriseURLsOnlyGoToTheStoredAPI(t *testing.T) {
	e := newEnv(t, nil)
	ap := newFakeApprise(t)
	evil := newFakeApprise(t)
	var n notify.Notification
	e.call(t, 201, "POST", "/notifications", map[string]any{"name": "Phone", "apiUrl": ap.srv.URL, "urls": appriseURLs}, &n)
	path := fmt.Sprintf("/notifications/%d", n.ID)

	code, msg := e.status(t, "POST", "/notifications/test", map[string]any{"id": n.ID, "name": "Phone", "apiUrl": evil.srv.URL})
	if code != 400 || !strings.Contains(msg, "enter them again") {
		t.Fatalf("test of another API URL with the stored URLs: %d %q", code, msg)
	}
	code, msg = e.status(t, "PUT", path, map[string]any{"name": "Phone", "apiUrl": evil.srv.URL})
	if code != 400 || !strings.Contains(msg, "enter them again") {
		t.Fatalf("API URL change without the URLs: %d %q", code, msg)
	}
	var res testResult
	e.call(t, 200, "POST", "/notifications/test", map[string]any{"id": n.ID}, &res)
	if got, _ := evil.requests(); !res.OK || len(got) != 0 {
		t.Fatalf("the other API was contacted: %+v %v", res, got)
	}
	// The same API URL (another spelling) keeps the stored URLs; new URLs allow a new API URL.
	e.call(t, 200, "POST", "/notifications/test", map[string]any{"id": n.ID, "name": "Phone", "apiUrl": ap.srv.URL + "/"}, &res)
	if got, _ := ap.requests(); !res.OK || len(got) != 2 || got[1]["urls"] != appriseURLs {
		t.Fatalf("test of the stored API URL: %+v %v", res, got)
	}
	moved := newFakeApprise(t)
	e.call(t, 200, "PUT", path, map[string]any{"name": "Phone", "apiUrl": moved.srv.URL, "urls": appriseURLs}, &n)
	if n.APIURL != moved.srv.URL || !n.HasURLs {
		t.Fatalf("API URL change with the URLs: %+v", n)
	}
}
