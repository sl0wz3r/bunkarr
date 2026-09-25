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
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/auth"
	"github.com/sl0wz3r/bunkarr/internal/config"
	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// openAPIDoc is the part of openapi.json these tests read.
type openAPIDoc struct {
	Info struct {
		Description string `json:"description"`
	} `json:"info"`
	Paths      map[string]map[string]json.RawMessage `json:"paths"`
	Components struct {
		Schemas map[string]schemaRequirements `json:"schemas"`
	} `json:"components"`
}

// schemaRequirements is the part of a JSON schema that says which fields a body needs and the
// smallest numbers they take.
type schemaRequirements struct {
	Required   []string `json:"required"`
	Properties map[string]struct {
		Minimum *float64 `json:"minimum"`
	} `json:"properties"`
	AnyOf []schemaRequirements `json:"anyOf"`
}

// accepts reports whether a body (decoded JSON) has every field the schema requires, with
// numbers no smaller than their minimum, and satisfies one of its anyOf alternatives.
func (s schemaRequirements) accepts(body map[string]any) bool {
	for _, n := range s.Required {
		if _, ok := body[n]; !ok {
			return false
		}
	}
	for n, p := range s.Properties {
		if v, ok := body[n].(float64); ok && p.Minimum != nil && v < *p.Minimum {
			return false
		}
	}
	if len(s.AnyOf) == 0 {
		return true
	}
	for _, alt := range s.AnyOf {
		if alt.accepts(body) {
			return true
		}
	}
	return false
}

func loadOpenAPI(t *testing.T) openAPIDoc {
	t.Helper()
	var doc openAPIDoc
	if err := json.Unmarshal(openAPISpec, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// documentedStatuses returns the response codes openapi.json lists for method on a concrete API
// path such as /destinations/5/sync (nil when no operation matches).
func documentedStatuses(t *testing.T, doc openAPIDoc, method, path string) map[int]bool {
	t.Helper()
	path, _, _ = strings.Cut(path, "?")
	for tmpl, ops := range doc.Paths {
		re := regexp.MustCompile("^" + regexp.MustCompile(`\\\{[^}]+\\\}`).ReplaceAllString(regexp.QuoteMeta(tmpl), `[^/]+`) + "$")
		raw, ok := ops[strings.ToLower(method)]
		if !ok || !re.MatchString(path) {
			continue
		}
		var op struct {
			Responses map[string]json.RawMessage `json:"responses"`
		}
		if err := json.Unmarshal(raw, &op); err != nil {
			t.Fatal(err)
		}
		out := map[int]bool{}
		for code := range op.Responses {
			n, err := strconv.Atoi(code)
			if err != nil {
				t.Fatalf("%s %s: response %q is not a status code", method, tmpl, code)
			}
			out[n] = true
		}
		return out
	}
	return nil
}

// TestOpenAPIDocumentsHandlerStatuses: every status a handler answers with is documented for its
// operation. Each documented operation gets a malformed id, an unknown id, no body and a
// malformed body; then requests reach the handlers' specific refusals.
func TestOpenAPIDocumentsHandlerStatuses(t *testing.T) {
	e := newEnv(t, nil)
	doc := loadOpenAPI(t)
	check := func(method, path string, body any) int {
		t.Helper()
		code, raw := e.raw(t, method, path, body)
		st := documentedStatuses(t, doc, method, path)
		if st == nil {
			t.Fatalf("%s %s: no documented operation", method, path)
		}
		if !st[code] {
			t.Errorf("%s %s (body %v): status %d is not documented: %s", method, path, body, code, raw)
		}
		return code
	}

	for tmpl, ops := range doc.Paths {
		for method := range ops {
			m := strings.ToUpper(method)
			if m == "PARAMETERS" {
				continue
			}
			paths := []string{tmpl}
			if strings.Contains(tmpl, "{id}") {
				paths = []string{strings.ReplaceAll(tmpl, "{id}", "x"), strings.ReplaceAll(tmpl, "{id}", "999999")}
			}
			for _, p := range paths {
				check(m, p, nil)
				if m == "POST" || m == "PUT" {
					check(m, p, "[")
				}
			}
		}
	}

	// GET /filesystem on a directory Bunkarr cannot read.
	if os.Geteuid() != 0 {
		locked := e.mkdir(t, "locked")
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
		if code := check("GET", "/filesystem?path="+locked, nil); code != 403 {
			t.Errorf("unreadable directory: %d, want 403", code)
		}
	}
	// POST /auth/login with a body that is not JSON.
	if code := check("POST", "/auth/login", "not json"); code != 400 {
		t.Errorf("login without JSON: %d, want 400", code)
	}
	// POST /schedules/{id}/run while the destination is disabled.
	destID := e.createDestination(t, "NAS", e.mkdir(t, "nas"), nil, nil)
	e.call(t, 200, "PUT", fmt.Sprintf("/destinations/%d", destID), map[string]any{"enabled": false}, nil)
	for _, sc := range e.schedulesOf(t, jobs.TypeVerify, destID) {
		if code := check("POST", fmt.Sprintf("/schedules/%d/run", sc.ID), nil); code != 409 {
			t.Errorf("run of a blocked schedule: %d, want 409", code)
		}
	}
}

// TestOpenAPIDocumentsDatabaseFailures: the account and settings routes (Phase 0, no App) answer
// 500 when the database fails, as the API key's and the session cookie's requests reach it;
// those 500s are documented too.
func TestOpenAPIDocumentsDatabaseFailures(t *testing.T) {
	ctx := context.Background()
	dir := resolvedTempDir(t)
	d, err := db.Open(ctx, filepath.Join(dir, "bunkarr.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	kr, _ := config.NewKeyring(make([]byte, 32))
	a := auth.New(d, config.NewSettings(d, kr), nil)
	a.SetBcryptCost(4)
	if err := a.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Setup(ctx, "admin", "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(Options{Auth: a, DB: d, Env: config.Env{ConfigDir: dir}}).Handler())
	t.Cleanup(srv.Close)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	doc := loadOpenAPI(t)
	bodies := map[string]string{
		"/auth/setup":       `{"username": "admin2", "password": "correct horse battery"}`,
		"/auth/login":       `{"username": "admin", "password": "correct horse battery"}`,
		"/auth/credentials": `{"currentPassword": "correct horse battery", "newPassword": "another horse battery"}`,
		"/settings/general": `{"authenticationRequired": "enabled"}`,
	}
	for tmpl, ops := range doc.Paths {
		if !strings.HasPrefix(tmpl, "/auth/") && !strings.HasPrefix(tmpl, "/settings/") && tmpl != "/system/status" && tmpl != "/health" {
			continue
		}
		for method := range ops {
			m := strings.ToUpper(method)
			if m == "PARAMETERS" {
				continue
			}
			for _, cred := range []string{"apikey", "session"} {
				var body io.Reader
				if b, ok := bodies[tmpl]; ok {
					body = strings.NewReader(b)
				}
				req, err := http.NewRequest(m, srv.URL+"/api/v1"+tmpl, body)
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Content-Type", "application/json")
				if cred == "apikey" {
					req.Header.Set("X-Api-Key", a.APIKey())
				} else {
					req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: "a-session-token"})
				}
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := io.ReadAll(res.Body)
				res.Body.Close()
				if !documentedStatuses(t, doc, m, tmpl)[res.StatusCode] {
					t.Errorf("%s %s (%s, database closed): status %d is not documented: %s", m, tmpl, cred, res.StatusCode, raw)
				}
			}
		}
	}
}

// TestOpenAPIIntegrationTestRequirements: the IntegrationTest schema requires what the handler
// requires: a type, unless the id of a saved integration gives it.
func TestOpenAPIIntegrationTestRequirements(t *testing.T) {
	e := newEnv(t, nil)
	schema := loadOpenAPI(t).Components.Schemas["IntegrationTest"]
	var it struct {
		ID int64 `json:"id"`
	}
	e.call(t, 201, "POST", "/integrations", map[string]any{"type": "sonarr", "name": "Sonarr", "url": "http://127.0.0.1:9"}, &it)
	for _, body := range []map[string]any{
		{"url": "http://127.0.0.1:9"},
		{"url": "http://127.0.0.1:9", "type": "sonarr"},
		{"url": "http://127.0.0.1:9", "id": it.ID},
		// id 0 is no integration: the type is still required. A negative id is refused.
		{"url": "http://127.0.0.1:9", "id": 0},
		{"url": "http://127.0.0.1:9", "id": -1},
		{"url": "http://127.0.0.1:9", "type": "sonarr", "id": 0},
		{"url": "http://127.0.0.1:9", "type": "sonarr", "id": -1},
	} {
		// Through JSON, as a request body arrives: numbers are float64.
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		code, msg := e.status(t, "POST", "/integrations/test", body)
		if handlerAccepts := code != 400; handlerAccepts != schema.accepts(body) {
			t.Errorf("POST /integrations/test %v: the handler answers %d %q, the schema accepts: %v", body, code, msg, schema.accepts(body))
		}
	}
}

// TestOpenAPIDocumentsNoBackupServices: a server built without the Phase 1 services answers 503
// on every route that needs them; each operation documents it, and so does the error overview.
func TestOpenAPIDocumentsNoBackupServices(t *testing.T) {
	ctx := context.Background()
	dir := resolvedTempDir(t)
	d, err := db.Open(ctx, filepath.Join(dir, "bunkarr.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	kr, _ := config.NewKeyring(make([]byte, 32))
	a := auth.New(d, config.NewSettings(d, kr), nil)
	a.SetBcryptCost(4)
	if err := a.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Setup(ctx, "admin", "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(Options{Auth: a, DB: d, Env: config.Env{ConfigDir: dir}}).Handler())
	t.Cleanup(srv.Close)

	doc := loadOpenAPI(t)
	if !strings.Contains(doc.Info.Description, "503") {
		t.Errorf("the error overview in info.description does not mention 503")
	}
	phase0 := func(tmpl string) bool {
		for _, p := range []string{"/auth/", "/settings/", "/system/", "/health", "/openapi.json"} {
			if strings.HasPrefix(tmpl, p) {
				return true
			}
		}
		return false
	}
	for tmpl, ops := range doc.Paths {
		if phase0(tmpl) {
			continue
		}
		for method := range ops {
			m := strings.ToUpper(method)
			if m == "PARAMETERS" {
				continue
			}
			req, err := http.NewRequest(m, srv.URL+"/api/v1"+strings.ReplaceAll(tmpl, "{id}", "1"), strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Api-Key", a.APIKey())
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode != http.StatusServiceUnavailable {
				t.Errorf("%s %s without backup services: %d, want 503: %s", m, tmpl, res.StatusCode, raw)
			}
			if !documentedStatuses(t, doc, m, tmpl)[res.StatusCode] {
				t.Errorf("%s %s without backup services: status %d is not documented", m, tmpl, res.StatusCode)
			}
		}
	}
}
