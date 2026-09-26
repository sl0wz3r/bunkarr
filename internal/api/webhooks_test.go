package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/auth"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/webhooks"
)

// webhookFixture returns the body of a recorded webhook (testdata/webhooks).
func webhookFixture(t *testing.T, app, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "webhooks", app, name))
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Body json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	return capture.Body
}

// createArr adds an *arr integration (with an unreachable URL: no refresh is waited for) and
// returns it with its webhook key.
func (e *env) createArr(t *testing.T, typ integrations.Type, name string) (integrations.Integration, string) {
	t.Helper()
	var it integrations.Integration
	e.call(t, 201, "POST", "/integrations", map[string]any{"type": string(typ), "name": name, "url": "http://127.0.0.1:9", "apiKey": "0123456789abcdef"}, &it)
	var k struct {
		Key string `json:"key"`
	}
	e.call(t, 200, "POST", fmt.Sprintf("/integrations/%d/webhook/key", it.ID), map[string]bool{"rotate": false}, &k)
	if len(k.Key) != integrations.WebhookKeyLen {
		t.Fatalf("webhook key %q", k.Key)
	}
	return it, k.Key
}

// hook posts a webhook body to path (under /api/v1) with the given request tweak and returns the
// status and message.
func (e *env) hook(t *testing.T, c *http.Client, path string, body []byte, tweak func(*http.Request)) (int, string) {
	t.Helper()
	req, err := http.NewRequest("POST", e.srv.URL+"/api/v1"+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if tweak != nil {
		tweak(req)
	}
	if c == nil {
		c = http.DefaultClient
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var m struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &m)
	return res.StatusCode, m.Message
}

func basic(key string) func(*http.Request) {
	return func(r *http.Request) { r.SetBasicAuth("radarr", key) }
}

func header(key string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("X-Api-Key", key) }
}

func query(key string) func(*http.Request) {
	return func(r *http.Request) { r.URL.RawQuery = "apikey=" + key }
}

// TestWebhookAuthMatrix: the webhook key (Basic password, header or query) is the only credential
// of the webhook routes, and it opens no other route (D7, S12).
func TestWebhookAuthMatrix(t *testing.T) {
	e := newEnv(t, nil)
	it, key := e.createArr(t, integrations.TypeRadarr, "Radarr")
	test := webhookFixture(t, "radarr", "Test.json")
	own := fmt.Sprintf("/webhook/radarr/%d", it.ID)
	for name, tweak := range map[string]func(*http.Request){"basic": basic(key), "header": header(key), "query": query(key)} {
		for _, p := range []string{own, "/webhook/radarr"} {
			if code, msg := e.hook(t, nil, p, test, tweak); code != 200 {
				t.Errorf("%s on %s: %d %s", name, p, code, msg)
			}
		}
	}
	// Session: log in, then send only the cookie.
	if _, err := e.auth.Setup(context.Background(), "admin", "correct horse"); err != nil {
		t.Fatal(err)
	}
	c := e.client(t)
	if code, _, _ := e.do(t, c, "POST", "/api/v1/auth/login", `{"username":"admin","password":"correct horse"}`, nil); code != 200 {
		t.Fatal("login failed")
	}
	for name, tc := range map[string]struct {
		c     *http.Client
		tweak func(*http.Request)
	}{
		"no key":          {nil, nil},
		"wrong key":       {nil, basic(strings.Repeat("0", integrations.WebhookKeyLen))},
		"master API key":  {nil, header(e.auth.APIKey())},
		"master in query": {nil, query(e.auth.APIKey())},
		"session cookie":  {c, nil},
	} {
		if code, _ := e.hook(t, tc.c, own, test, tc.tweak); code != 401 {
			t.Errorf("%s: %d, want 401", name, code)
		}
	}
	// "Disabled for local addresses" (the test client is local) opens the API, not the webhooks.
	if err := e.auth.SetMode(context.Background(), auth.ModeLocalDisabled); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := e.do(t, nil, "GET", "/api/v1/system/status", "", nil); code != 200 {
		t.Fatalf("local mode: system status %d", code)
	}
	if code, _ := e.hook(t, nil, own, test, nil); code != 401 {
		t.Fatalf("local mode webhook without key: %d", code)
	}
	if err := e.auth.SetMode(context.Background(), auth.ModeEnabled); err != nil {
		t.Fatal(err)
	}
	// The webhook key opens no other route.
	for _, h := range []map[string]string{{"X-Api-Key": key}} {
		if code, _, _ := e.do(t, nil, "GET", "/api/v1/system/status?apikey="+key, "", h); code != 401 {
			t.Fatalf("webhook key on system/status: %d", code)
		}
	}
	// A new master API key changes nothing for the webhooks.
	e.call(t, 200, "POST", "/settings/general/apikey", nil, nil)
	if code, msg := e.hook(t, nil, own, test, basic(key)); code != 200 {
		t.Fatalf("after the master key changed: %d %s", code, msg)
	}
	// Test events queue nothing and are recorded as test.
	page, err := e.app.WebhookEvents().List(context.Background(), webhooks.Query{IntegrationID: it.ID})
	if err != nil || page.TotalRecords != 7 {
		t.Fatalf("events = %+v, %v", page, err)
	}
	for _, r := range page.Records {
		if r.Outcome == nil || *r.Outcome != webhooks.OutcomeTest {
			t.Fatalf("event %+v", r)
		}
	}
}

// TestWebhookIntegrationChecks: the id route needs that integration's key; the generic route picks
// the integration by key among 0, 1 or 2 of the app; a disabled integration or one of another app
// gets 409; an unknown id 404.
func TestWebhookIntegrationChecks(t *testing.T) {
	e := newEnv(t, nil)
	test := webhookFixture(t, "radarr", "Test.json")
	// No integration at all.
	if code, _ := e.hook(t, nil, "/webhook/radarr", test, basic(strings.Repeat("a", 32))); code != 401 {
		t.Fatalf("generic route without integrations: %d", code)
	}
	r1, k1 := e.createArr(t, integrations.TypeRadarr, "Radarr")
	if code, _ := e.hook(t, nil, "/webhook/radarr", test, basic(k1)); code != 200 {
		t.Fatalf("generic route, one integration: %d", code)
	}
	r2, k2 := e.createArr(t, integrations.TypeRadarr, "Radarr 4K")
	s1, ks := e.createArr(t, integrations.TypeSonarr, "Sonarr")
	for _, tc := range []struct {
		path, key string
		want      int
		integ     int64
	}{
		{"/webhook/radarr", k2, 200, r2.ID},
		{"/webhook/radarr", k1, 200, r1.ID},
		{"/webhook/radarr", ks, 401, 0}, // a Sonarr key on the Radarr route
		{fmt.Sprintf("/webhook/radarr/%d", r1.ID), k2, 401, 0},
		{fmt.Sprintf("/webhook/radarr/%d", s1.ID), ks, 409, 0}, // Sonarr's own key on a Radarr route
		{"/webhook/radarr/9999", k1, 404, 0},
		{"/webhook/plex", k1, 404, 0},
	} {
		before, _ := e.app.WebhookEvents().List(context.Background(), webhooks.Query{})
		code, msg := e.hook(t, nil, tc.path, test, basic(tc.key))
		if code != tc.want {
			t.Errorf("%s: %d %s, want %d", tc.path, code, msg, tc.want)
			continue
		}
		after, _ := e.app.WebhookEvents().List(context.Background(), webhooks.Query{})
		if tc.want == 200 && (after.TotalRecords != before.TotalRecords+1 || *after.Records[0].IntegrationID != tc.integ) {
			t.Errorf("%s: stored %+v", tc.path, after.Records[0])
		}
	}
	// Disabled.
	var it integrations.Integration
	e.call(t, 200, "PUT", fmt.Sprintf("/integrations/%d", r2.ID), map[string]any{"type": "radarr", "name": "Radarr 4K", "url": "http://127.0.0.1:9",
		"enabled": false}, &it)
	if code, _ := e.hook(t, nil, fmt.Sprintf("/webhook/radarr/%d", r2.ID), test, basic(k2)); code != 409 {
		t.Fatalf("disabled: %d", code)
	}
	// The panel of Radarr warns that Radarr 4K and Radarr received events on the generic route.
	var info webhookInfoView
	e.call(t, 200, "GET", fmt.Sprintf("/integrations/%d/webhook", r1.ID), nil, &info)
	if info.Path != fmt.Sprintf("/api/v1/webhook/radarr/%d", r1.ID) || info.GenericPath != "/api/v1/webhook/radarr" || !info.HasKey ||
		info.LastTestAt == nil || len(info.Recent) == 0 {
		t.Fatalf("webhook info = %+v", info)
	}
	if !strings.Contains(strings.Join(info.Warnings, "\n"), `"Radarr 4K" received events on the generic route`) {
		t.Fatalf("warnings = %v", info.Warnings)
	}
	if code, _ := e.status(t, "GET", fmt.Sprintf("/integrations/%d/webhook", 9999), nil); code != 404 {
		t.Fatalf("unknown integration: %d", code)
	}
}

// TestWebhookFailedAuthLimiter: 10 bad keys from an address block it (429) for bad keys only; the
// correct key still gets 200 (S13).
func TestWebhookFailedAuthLimiter(t *testing.T) {
	e := newEnv(t, nil)
	it, key := e.createArr(t, integrations.TypeRadarr, "Radarr")
	test := webhookFixture(t, "radarr", "Test.json")
	own := fmt.Sprintf("/webhook/radarr/%d", it.ID)
	for i := range webhookFailMax {
		if code, _ := e.hook(t, nil, own, test, basic("bad")); code != 401 {
			t.Fatalf("failure %d: %d", i, code)
		}
	}
	if code, _ := e.hook(t, nil, own, test, basic("bad")); code != 429 {
		t.Fatalf("11th bad key: %d, want 429", code)
	}
	if code, _ := e.hook(t, nil, own, test, basic(key)); code != 200 {
		t.Fatalf("the correct key from a blocked address: %d", code)
	}
	// The login limiter is separate: the UI still logs in.
	if _, err := e.auth.Setup(context.Background(), "admin", "correct horse"); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := e.do(t, nil, "POST", "/api/v1/auth/login", `{"username":"admin","password":"correct horse"}`, nil); code != 200 {
		t.Fatalf("login after webhook failures: %d", code)
	}
}

// TestWebhookEventsAPI lists and reads stored events; no response carries the webhook key.
func TestWebhookEventsAPI(t *testing.T) {
	e := newEnv(t, nil)
	it, key := e.createArr(t, integrations.TypeRadarr, "Radarr")
	own := fmt.Sprintf("/webhook/radarr/%d", it.ID)
	for _, f := range []string{"Test.json", "Grab.json", "Download.json"} {
		if code, msg := e.hook(t, nil, own, webhookFixture(t, "radarr", f), header(key)); code != 200 {
			t.Fatalf("%s: %d %s", f, code, msg)
		}
	}
	code, raw := e.raw(t, "GET", fmt.Sprintf("/webhooks/events?integrationId=%d&pageSize=2", it.ID), nil)
	if code != 200 || strings.Contains(string(raw), key) {
		t.Fatalf("events: %d %s", code, raw)
	}
	var page webhooks.Page
	if err := json.Unmarshal(raw, &page); err != nil || page.TotalRecords != 3 || len(page.Records) != 2 {
		t.Fatalf("page = %+v, %v", page, err)
	}
	dl := page.Records[0]
	if dl.EventType != "Download" || dl.Class != webhooks.ClassDownload || dl.Summary.Title == "" || len(dl.Summary.ItemIDs) != 1 || dl.Payload != nil {
		t.Fatalf("download = %+v", dl)
	}
	var full webhooks.Record
	e.call(t, 200, "GET", fmt.Sprintf("/webhooks/events/%d", dl.ID), nil, &full)
	if len(full.Payload) == 0 {
		t.Fatal("no payload")
	}
	for _, q := range []string{"outcome=bogus", "page=0", "integrationId=-1"} {
		if code, _ := e.status(t, "GET", "/webhooks/events?"+q, nil); code != 400 {
			t.Errorf("%s: %d", q, code)
		}
	}
	if code, _ := e.status(t, "GET", "/webhooks/events/9999", nil); code != 404 {
		t.Fatalf("unknown event: %d", code)
	}
	for _, p := range []string{"/webhooks/events", fmt.Sprintf("/integrations/%d/webhook", it.ID)} {
		_, raw := e.raw(t, "GET", p, nil)
		if strings.Contains(string(raw), key) {
			t.Fatalf("%s contains the webhook key", p)
		}
	}
	// Without a session or the API key the event list is closed.
	if code, _, _ := e.do(t, nil, "GET", "/api/v1/webhooks/events", "", map[string]string{"X-Api-Key": key}); code != 401 {
		t.Fatalf("event list with the webhook key: %d", code)
	}
}

// TestWebhookToTargetedSync is the whole path in one process: a Radarr Download → a targeted
// refresh with trigger webhook → a targeted sync of the movie's folder that copies only it.
func TestWebhookToTargetedSync(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the 5 s quiet window")
	}
	e := newEnv(t, nil)
	fake, it, srcID := arrSetup(t, e)
	_ = fake
	dst := e.mkdir(t, "dst")
	destID := e.createDestination(t, "NAS", dst, []int64{srcID}, nil)
	var k struct {
		Key string `json:"key"`
	}
	e.call(t, 200, "POST", fmt.Sprintf("/integrations/%d/webhook/key", it.ID), map[string]bool{"rotate": false}, &k)
	body := webhookFixture(t, "radarr", "Download-upgrade.json") // names movie 1
	if code, msg := e.hook(t, nil, fmt.Sprintf("/webhook/radarr/%d", it.ID), body, basic(k.Key)); code != 200 {
		t.Fatalf("webhook: %d %s", code, msg)
	}
	deadline := time.Now().Add(45 * time.Second)
	var sync jobs.Job
	for time.Now().Before(deadline) && sync.ID == 0 {
		var page jobqueue.Page[jobs.Job]
		e.call(t, 200, "GET", "/jobs?type=sync&pageSize=50", nil, &page)
		for _, j := range page.Records {
			if j.Trigger == jobs.TriggerWebhook && j.Params.DestinationID == destID && j.Status.Final() {
				sync = j
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if sync.ID == 0 {
		t.Fatal("no webhook sync finished")
	}
	if sync.Status != jobs.StatusCompleted || len(sync.Params.Paths) != 1 || sync.Params.Paths[0] != "Night of the Living Dead (1968)" {
		t.Fatalf("sync = %+v", sync)
	}
	var items jobqueue.Page[jobs.Item]
	e.call(t, 200, "GET", fmt.Sprintf("/jobs/%d/items?pageSize=100", sync.ID), nil, &items)
	for _, item := range items.Records {
		if item.Action != jobs.ActionCopy || item.Status != jobs.ItemDone || !strings.HasPrefix(item.RelPath, "movies/Night of the Living Dead (1968)/") {
			t.Fatalf("item %+v", item)
		}
	}
	if len(items.Records) == 0 {
		t.Fatal("nothing copied")
	}
	page, _ := e.app.WebhookEvents().List(context.Background(), webhooks.Query{})
	if page.Records[0].Outcome == nil || *page.Records[0].Outcome != webhooks.OutcomeQueued {
		t.Fatalf("event = %+v", page.Records[0])
	}

	// A burst of 200 events for three movies: one refresh, then one sync per destination.
	last := sync.ID
	refreshesBefore := len(webhookJobs(t, e, "refresh", 0))
	bodies := [][]byte{webhookFixture(t, "radarr", "Download.json"), webhookFixture(t, "radarr", "Rename.json"), body}
	for i := range 200 {
		if code, msg := e.hook(t, nil, fmt.Sprintf("/webhook/radarr/%d", it.ID), bodies[i%3], basic(k.Key)); code != 200 {
			t.Fatalf("burst event %d: %d %s", i, code, msg)
		}
	}
	deadline = time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		syncs := webhookJobs(t, e, "sync", last)
		if len(syncs) > 0 && syncs[0].Status.Final() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(time.Second) // nothing else follows
	refreshes := webhookJobs(t, e, "refresh", 0)[refreshesBefore:]
	syncs := webhookJobs(t, e, "sync", last)
	if len(refreshes) != 1 || len(refreshes[0].Params.ArrItemIDs) != 3 || len(syncs) != 1 || len(syncs[0].Params.Paths) != 3 {
		t.Fatalf("after the burst: refreshes %+v, syncs %+v", refreshes, syncs)
	}
}

// webhookJobs returns the jobs of a type with trigger webhook and an id above after, oldest first.
func webhookJobs(t *testing.T, e *env, typ string, after int64) []jobs.Job {
	t.Helper()
	var page jobqueue.Page[jobs.Job]
	e.call(t, 200, "GET", "/jobs?pageSize=500&type="+typ, nil, &page)
	var out []jobs.Job
	for i := len(page.Records) - 1; i >= 0; i-- {
		if j := page.Records[i]; j.Trigger == jobs.TriggerWebhook && j.ID > after {
			out = append(out, j)
		}
	}
	return out
}

// TestManifestAfterSyncAuto: a full sync is followed by a manifest export only while an enabled
// *arr integration exists (manifest.afterSync "auto", design §9.2).
func TestManifestAfterSyncAuto(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	if ok, err := e.app.manifestAfterSync(ctx, 1); err != nil || ok {
		t.Fatalf("without *arr integrations: %v, %v", ok, err)
	}
	it, _ := e.createArr(t, integrations.TypeSonarr, "Sonarr")
	if ok, err := e.app.manifestAfterSync(ctx, 1); err != nil || !ok {
		t.Fatalf("with Sonarr: %v, %v", ok, err)
	}
	e.call(t, 200, "PUT", fmt.Sprintf("/integrations/%d", it.ID), map[string]any{"type": "sonarr", "name": "Sonarr", "url": "http://127.0.0.1:9",
		"enabled": false}, nil)
	if ok, err := e.app.manifestAfterSync(ctx, 1); err != nil || ok {
		t.Fatalf("with a disabled Sonarr: %v, %v", ok, err)
	}
}
