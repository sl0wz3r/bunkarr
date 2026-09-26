package plextest_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/integrations/plex/plextest"
)

func tvHeaders(token string) map[string]string {
	h := map[string]string{"X-Plex-Client-Identifier": "client-1", "X-Plex-Product": "Bunkarr", "Accept": "application/json"}
	if token != "" {
		h["X-Plex-Token"] = token
	}
	return h
}

func post(t *testing.T, url string, header map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp, body
}

func TestPlexTVPinFlow(t *testing.T) {
	tv := plextest.NewPlexTV(t)
	tv.AddAccount(plextest.Account{Token: "account-token-1", Username: "alice", Email: "alice@example.com",
		Servers: []plextest.Resource{{Name: "Tower", ClientIdentifier: "m1", Owned: true, AccessToken: "server-token-1",
			Connections: []plextest.Connection{{URI: "http://10.0.0.2:32400", Address: "10.0.0.2", Port: 32400, Protocol: "http", Local: true}}}}})

	resp, pin := post(t, tv.URL+"/api/v2/pins?strong=true", tvHeaders(""))
	if resp.StatusCode != http.StatusCreated || pin["authToken"] != nil || pin["code"] == "" {
		t.Fatalf("create = %d %v", resp.StatusCode, pin)
	}
	id := int64(pin["id"].(float64))
	code := pin["code"].(string)
	if tv.PinCount() != 1 || tv.PinIDs()[0] != id {
		t.Fatalf("PinIDs = %v", tv.PinIDs())
	}
	check := tv.URL + "/api/v2/pins/" + jsonNumber(id) + "?code=" + code
	if r, body := get(t, check, tvHeaders("")); r.StatusCode != 200 || !strings.Contains(body, `"authToken":null`) {
		t.Fatalf("pending check = %d %s", r.StatusCode, body)
	}
	tv.Approve(code, "account-token-1")
	if r, body := get(t, check, tvHeaders("")); r.StatusCode != 200 || !strings.Contains(body, `"authToken":"account-token-1"`) {
		t.Fatalf("claimed check = %d %s", r.StatusCode, body)
	}
	// Another client identifier or a wrong code: 404, as plex.tv answers.
	other := tvHeaders("")
	other["X-Plex-Client-Identifier"] = "client-2"
	if r, _ := get(t, check, other); r.StatusCode != 404 {
		t.Errorf("another client: %d", r.StatusCode)
	}
	if r, _ := get(t, tv.URL+"/api/v2/pins/"+jsonNumber(id)+"?code=wrong", tvHeaders("")); r.StatusCode != 404 {
		t.Errorf("wrong code: %d", r.StatusCode)
	}
	if r, body := get(t, tv.URL+"/api/v2/user", tvHeaders("account-token-1")); r.StatusCode != 200 ||
		!strings.Contains(body, `"username":"alice"`) || !strings.Contains(body, "alice@example.com") {
		t.Errorf("user = %d %s", r.StatusCode, body)
	}
	if r, _ := get(t, tv.URL+"/api/v2/user", tvHeaders("unknown-token")); r.StatusCode != 401 {
		t.Errorf("unknown token: %d", r.StatusCode)
	}
	r, body := get(t, tv.URL+"/api/v2/resources?includeHttps=1&includeRelay=1&includeIPv6=1", tvHeaders("account-token-1"))
	if r.StatusCode != 200 || !strings.Contains(body, `"accessToken":"server-token-1"`) || !strings.Contains(body, `"provides":"client,player"`) {
		t.Errorf("resources = %d %s", r.StatusCode, body)
	}
	tv.Expire(code)
	if r, _ := get(t, check, tvHeaders("")); r.StatusCode != 404 {
		t.Errorf("expired: %d", r.StatusCode)
	}
	tv.Fail(plextest.PathUser, http.StatusServiceUnavailable)
	if r, _ := get(t, tv.URL+"/api/v2/user", tvHeaders("account-token-1")); r.StatusCode != 503 {
		t.Errorf("Fail: %d", r.StatusCode)
	}
	tv.Fail(plextest.PathUser, 0)
	if r, _ := get(t, tv.URL+"/api/v2/user", tvHeaders("account-token-1")); r.StatusCode != 200 {
		t.Errorf("Fail cleared: %d", r.StatusCode)
	}
	if n := tv.CountRequests(plextest.PathPin); n != 5 {
		t.Errorf("CountRequests(PathPin) = %d, want 5", n)
	}
}

func jsonNumber(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestPlexTVFailsTheTestOnMisuse(t *testing.T) {
	rec := &recordingTB{TB: t}
	tv := plextest.NewPlexTV(rec)
	tv.AddAccount(plextest.Account{Token: "account-token-1", Username: "alice"})
	// A token in the query.
	if r, _ := get(t, tv.URL+"/api/v2/user?X-Plex-Token=account-token-1", tvHeaders("")); r.StatusCode != 400 {
		t.Errorf("token in query: %d", r.StatusCode)
	}
	// No client identifier.
	if r, _ := get(t, tv.URL+"/api/v2/user", map[string]string{"X-Plex-Token": "account-token-1", "X-Plex-Product": "Bunkarr"}); r.StatusCode != 400 {
		t.Errorf("no client id: %d", r.StatusCode)
	}
	// A PIN created with a token, a PIN check without the code.
	post(t, tv.URL+"/api/v2/pins?strong=true", tvHeaders("account-token-1"))
	get(t, tv.URL+"/api/v2/pins/1", tvHeaders(""))
	// Approving an unknown code.
	tv.Approve("nope", "account-token-1")
	rec.mu.Lock()
	defer rec.mu.Unlock()
	want := []string{"X-Plex-Token in the URL query", "without X-Plex-Client-Identifier", "sent a token", "without the PIN code", "no PIN with the code"}
	if len(rec.errs) != len(want) {
		t.Fatalf("errors = %q", rec.errs)
	}
	for i, w := range want {
		if !strings.Contains(rec.errs[i], w) {
			t.Errorf("error %d = %q, want %q", i, rec.errs[i], w)
		}
	}
}
