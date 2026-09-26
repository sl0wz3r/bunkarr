//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/integrations/plex/plextest"
)

// Acceptance 10 against the real binary (docs/design/phase2-3.md §15 E2E): "Sign in with Plex"
// with a fake plex.tv (BUNKARR_TEST_PLEXTV_URL, honoured only by an e2e build) and a fake PMS
// (plextest). Sign in, pick a server, test its connections and save: the stored token is the
// server's own token (the PMS receives it and no other), no API answer holds a token, the PIN id
// never appears and the PIN code only inside authUrl. The manual URL + token path still works as
// in Phase 1.

const (
	psAccountToken = "e2e-account-token-6c1d0a7e93b2"
	psServerToken  = "e2e-server-token-2f9b4c8a1d77"
	psManualToken  = "e2e-manual-token-8a3e5f10b6c4"
	psMachine      = "e2e-machine-tower-0001"
	psManualID     = "e2e-machine-manual-0002"
)

// psClient calls the binary's API and keeps every raw answer for the secret checks.
type psClient struct {
	t       *testing.T
	s       *server
	mu      sync.Mutex
	answers []string
}

func (c *psClient) do(want int, method, p string, body, out any) []byte {
	c.t.Helper()
	code, raw, err := c.s.request(method, p, body)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, p, err)
	}
	c.mu.Lock()
	c.answers = append(c.answers, string(raw))
	c.mu.Unlock()
	if code != want {
		c.t.Fatalf("%s %s: HTTP %d, want %d: %s", method, p, code, want, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			c.t.Fatalf("%s %s: decode %s: %v", method, p, raw, err)
		}
	}
	return raw
}

func TestPlexSignInE2E(t *testing.T) {
	tv := plextest.NewPlexTV(t)
	pms := plextest.NewServer(t, psServerToken)
	pms.SetIdentity(psMachine, "1.43.4.10903-e5521bd8c")
	manual := plextest.NewServer(t, psManualToken)
	manual.SetIdentity(psManualID, "1.42.0")
	pu, err := url.Parse(pms.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(pu.Port())
	tv.AddAccount(plextest.Account{Token: psAccountToken, Username: "alice", Email: "alice@example.com", Servers: []plextest.Resource{
		{Name: "Tower", ClientIdentifier: psMachine, Owned: true, AccessToken: psServerToken, ProductVersion: "1.43.4", Platform: "Linux",
			Connections: []plextest.Connection{{URI: pms.URL, Address: pu.Hostname(), Port: port, Protocol: "http", Local: true}}},
	}})

	root := resolvedTempDir(t)
	s := newServer(t, root)
	s.start("BUNKARR_TEST_PLEXTV_URL="+tv.URL, "BUNKARR_TEST_CLIENTS_PLEXTV_URL="+tv.URL)
	s.setup()
	c := &psClient{t: t, s: s}

	// Sign in: the PIN code only inside authUrl.
	var created struct {
		ID        string    `json:"id"`
		AuthURL   string    `json:"authUrl"`
		ExpiresAt time.Time `json:"expiresAt"`
	}
	c.do(http.StatusCreated, "POST", "/plex/signin", nil, &created)
	au, err := url.Parse(created.AuthURL)
	if err != nil || au.Scheme != "https" || au.Host != "app.plex.tv" || au.Path != "/auth" {
		t.Fatalf("authUrl %q", created.AuthURL)
	}
	frag, err := url.ParseQuery(strings.TrimPrefix(au.Fragment, "?"))
	code := frag.Get("code")
	if err != nil || code == "" || frag.Get("clientID") == "" || frag.Get("clientID") == "bunkarr" {
		t.Fatalf("authUrl fragment %q", au.Fragment)
	}
	type status struct {
		Status   string `json:"status"`
		Username string `json:"username"`
	}
	var st status
	c.do(http.StatusOK, "GET", "/plex/signin/"+created.ID, nil, &st)
	if st.Status != "pending" {
		t.Fatalf("status before approval %+v", st)
	}
	tv.Approve(code, psAccountToken)
	deadline := time.Now().Add(30 * time.Second)
	for st.Status != "authenticated" {
		if time.Now().After(deadline) {
			t.Fatalf("the sign-in was not authenticated: %+v", st)
		}
		time.Sleep(250 * time.Millisecond)
		c.do(http.StatusOK, "GET", "/plex/signin/"+created.ID, nil, &st)
	}
	if st.Username != "alice" {
		t.Fatalf("status %+v", st)
	}

	// Pick the server and test its connections.
	var servers []struct {
		ID             string `json:"id"`
		Name           string `json:"name"`
		Owned          bool   `json:"owned"`
		HasAccessToken bool   `json:"hasAccessToken"`
	}
	c.do(http.StatusOK, "GET", "/plex/signin/"+created.ID+"/servers", nil, &servers)
	if len(servers) != 1 || servers[0].ID != psMachine || !servers[0].Owned || !servers[0].HasAccessToken {
		t.Fatalf("servers %+v", servers)
	}
	var probe struct {
		Results []struct {
			URI           string `json:"uri"`
			OK            bool   `json:"ok"`
			TokenAccepted bool   `json:"tokenAccepted"`
		} `json:"results"`
	}
	c.do(http.StatusOK, "POST", "/plex/signin/"+created.ID+"/servers/"+psMachine+"/test", nil, &probe)
	if len(probe.Results) != 1 || !probe.Results[0].OK || !probe.Results[0].TokenAccepted || probe.Results[0].URI != pms.URL {
		t.Fatalf("probe %+v", probe)
	}

	// Save: the integration's token is the server's own (the PMS accepts only that one).
	var it struct {
		ID        int64  `json:"id"`
		URL       string `json:"url"`
		HasAPIKey bool   `json:"hasApiKey"`
	}
	c.do(http.StatusCreated, "POST", "/integrations", map[string]any{"type": "plex", "name": "Tower", "url": pms.URL,
		"plexSignIn": map[string]any{"id": created.ID, "serverId": psMachine}}, &it)
	if it.ID == 0 || !it.HasAPIKey || it.URL != pms.URL {
		t.Fatalf("created %+v", it)
	}
	before := len(pms.Requests())
	c.do(http.StatusOK, "GET", fmt.Sprintf("/integrations/%d/plex/sections", it.ID), nil, nil)
	after := pms.Requests()[before:]
	if len(after) == 0 {
		t.Fatal("the saved integration sent the PMS nothing")
	}
	for _, r := range after {
		if r.Header.Get("X-Plex-Token") != psServerToken {
			t.Fatalf("the saved integration sent %s without the server's token", r.Path)
		}
	}
	for _, r := range pms.Requests() {
		if tok := r.Header.Get("X-Plex-Token"); tok != "" && tok != psServerToken {
			t.Fatalf("the PMS received another token on %s", r.Path)
		}
	}
	// The sign-in is consumed.
	c.do(http.StatusNotFound, "GET", "/plex/signin/"+created.ID, nil, nil)

	// A second sign-in closed by the form is deleted.
	var second struct {
		ID string `json:"id"`
	}
	c.do(http.StatusCreated, "POST", "/plex/signin", nil, &second)
	c.do(http.StatusNoContent, "DELETE", "/plex/signin/"+second.ID, nil, nil)
	c.do(http.StatusNotFound, "GET", "/plex/signin/"+second.ID, nil, nil)

	// The manual URL + token path, as in Phase 1.
	var test struct {
		OK                bool   `json:"ok"`
		MachineIdentifier string `json:"machineIdentifier"`
	}
	c.do(http.StatusOK, "POST", "/integrations/test", map[string]any{"type": "plex", "url": manual.URL, "apiKey": psManualToken}, &test)
	if !test.OK || test.MachineIdentifier != psManualID {
		t.Fatalf("manual test %+v", test)
	}
	var man struct {
		ID        int64 `json:"id"`
		HasAPIKey bool  `json:"hasApiKey"`
	}
	c.do(http.StatusCreated, "POST", "/integrations", map[string]any{"type": "plex", "name": "Manual", "url": manual.URL, "apiKey": psManualToken}, &man)
	if !man.HasAPIKey {
		t.Fatalf("manual %+v", man)
	}
	c.do(http.StatusOK, "GET", fmt.Sprintf("/integrations/%d/plex/sections", man.ID), nil, nil)
	c.do(http.StatusOK, "GET", "/integrations", nil, nil)
	c.do(http.StatusOK, "GET", fmt.Sprintf("/integrations/%d", it.ID), nil, nil)

	// No answer holds a token or the PIN id; the PIN code only inside authUrl.
	secrets := []string{psAccountToken, psServerToken, psManualToken, "alice@example.com", "accessToken", "authToken"}
	for _, id := range tv.PinIDs() { // the fake's PIN ids are nine-digit numbers
		secrets = append(secrets, strconv.FormatInt(id, 10))
	}
	codeRe := regexp.MustCompile(regexp.QuoteMeta(code))
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, a := range c.answers {
		for _, sec := range secrets {
			if strings.Contains(a, sec) {
				t.Errorf("an answer contains %q: %s", sec, a)
			}
		}
		for _, loc := range codeRe.FindAllStringIndex(a, -1) {
			if i := strings.LastIndex(a[:loc[0]], `"authUrl":"https://app.plex.tv/auth#?`); i < 0 || strings.Contains(a[i:loc[0]], `",`) {
				t.Errorf("an answer contains the PIN code outside authUrl: %s", a)
			}
		}
	}
	// Neither does the server log.
	for _, l := range s.logs {
		b, err := os.ReadFile(l)
		if err != nil {
			t.Fatal(err)
		}
		for _, sec := range []string{psAccountToken, psServerToken, psManualToken, code, created.ID} {
			if strings.Contains(string(b), sec) {
				t.Errorf("the server log %s contains %q", l, sec)
			}
		}
	}
}
