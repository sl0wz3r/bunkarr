package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// publicRoutes answer without credentials.
var publicRoutes = map[string]bool{
	"GET /api/v1/health":       true,
	"GET /api/v1/openapi.json": true,
	"GET /api/v1/auth/status":  true,
	"POST /api/v1/auth/setup":  true,
	"POST /api/v1/auth/login":  true,
	"POST /api/v1/auth/logout": true,
}

// TestEveryOtherRouteRequiresAuth walks the router: every API route except the public ones
// answers 401 without an API key or session, before it looks at the request.
func TestEveryOtherRouteRequiresAuth(t *testing.T) {
	e := newEnv(t, nil)
	var routes []string
	err := chi.Walk(e.api.Handler().(chi.Routes), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		route = strings.TrimSuffix(route, "/")
		if strings.HasPrefix(route, "/api/v1/") && !publicRoutes[method+" "+route] {
			routes = append(routes, method+" "+route)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) < 45 {
		t.Fatalf("only %d protected routes found: %v", len(routes), routes)
	}
	for _, r := range routes {
		method, path, _ := strings.Cut(r, " ")
		path = strings.ReplaceAll(path, "{id}", "1")
		req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without credentials: %d, want 401", r, res.StatusCode)
		}
	}
}

// TestOpenAPIIsConsistent checks that the document is valid JSON whose every $ref resolves, and
// that every operation has an operationId (unique) and a 2xx response.
func TestOpenAPIIsConsistent(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(openAPISpec, &doc); err != nil {
		t.Fatalf("openapi.json is not valid JSON: %v", err)
	}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, c := range x {
				if k == "$ref" {
					ref, _ := c.(string)
					if !resolves(doc, ref) {
						t.Errorf("unresolved $ref %q", ref)
					}
				}
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	walk(doc)
	seen := map[string]string{}
	for p, ops := range doc["paths"].(map[string]any) {
		for m, raw := range ops.(map[string]any) {
			op := raw.(map[string]any)
			id, _ := op["operationId"].(string)
			if id == "" {
				t.Errorf("%s %s has no operationId", m, p)
			} else if other, dup := seen[id]; dup {
				t.Errorf("operationId %q used by %s %s and %s", id, m, p, other)
			}
			seen[id] = m + " " + p
			ok := false
			for code := range op["responses"].(map[string]any) {
				ok = ok || strings.HasPrefix(code, "2")
			}
			if !ok {
				t.Errorf("%s %s has no success response", m, p)
			}
			if strings.Contains(p, "{id}") && m != "parameters" {
				params, _ := op["parameters"].([]any)
				if len(params) == 0 {
					t.Errorf("%s %s does not declare its id parameter", m, p)
				}
			}
		}
	}
}

func resolves(doc map[string]any, ref string) bool {
	rest, ok := strings.CutPrefix(ref, "#/")
	if !ok {
		return false
	}
	var cur any = doc
	for _, part := range strings.Split(rest, "/") {
		m, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		if cur, ok = m[part]; !ok {
			return false
		}
	}
	return true
}

func TestPhase1RoutesWithoutAppAre503(t *testing.T) {
	e := newEnv(t, nil)
	s := New(Options{Auth: e.auth, DB: e.db})
	req, _ := http.NewRequest("GET", "/api/v1/sources", nil)
	req.Header.Set("X-Api-Key", e.auth.APIKey())
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("without App: %d, want 503", rec.Code)
	}
}
