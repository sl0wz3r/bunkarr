package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/integrations/plex/plextest"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/logging"
	"github.com/sl0wz3r/bunkarr/internal/notify"
)

const plexToken = "plex-token-ZXCVBNM1234567890"

// plexSchedules returns the plexdb_backup schedules of an integration.
func (e *env) plexSchedules(t *testing.T, id int64) []scheduleView {
	t.Helper()
	var list []scheduleView
	e.call(t, 200, "GET", "/schedules", nil, &list)
	var out []scheduleView
	for _, sc := range list {
		if sc.JobType == jobs.TypePlexDBBackup && sc.Params.IntegrationID == id {
			out = append(out, sc)
		}
	}
	return out
}

func plexSettingsOf(t *testing.T, it integrations.Integration) integrations.PlexSettings {
	t.Helper()
	ps, err := it.PlexSettings()
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

func TestIntegrationsCRUDNeverReturnTheToken(t *testing.T) {
	e := newEnv(t, nil)
	pms := plextest.NewServer(t, plexToken)
	body := map[string]any{"type": "plex", "name": "Plex", "url": pms.URL + "/", "apiKey": plexToken,
		"settings": map[string]any{"dataPath": "/plex/", "pathMappings": []any{map[string]any{"plex": "/data", "local": "/media"}}}}
	code, raw := e.raw(t, "POST", "/integrations", body)
	if code != 201 {
		t.Fatalf("create: %d %s", code, raw)
	}
	var it integrations.Integration
	if err := json.Unmarshal(raw, &it); err != nil {
		t.Fatal(err)
	}
	if !it.HasAPIKey || it.URL != pms.URL || plexSettingsOf(t, it).DataPath != "/plex" {
		t.Fatalf("created: %+v %s", it, it.Settings)
	}
	path := fmt.Sprintf("/integrations/%d", it.ID)
	for _, p := range []string{"/integrations", path} {
		code, raw := e.raw(t, "GET", p, nil)
		if code != 200 || strings.Contains(string(raw), plexToken) || strings.Contains(string(raw), "apiKey\"") {
			t.Fatalf("GET %s: %d %s", p, code, raw)
		}
	}
	if strings.Contains(string(raw), plexToken) {
		t.Fatalf("create returned the token: %s", raw)
	}
	// Update without apiKey keeps the token; clearApiKey removes it.
	e.call(t, 200, "PUT", path, map[string]any{"name": "Plex 2", "url": pms.URL}, &it)
	if !it.HasAPIKey || it.Name != "Plex 2" || plexSettingsOf(t, it).DataPath != "/plex" {
		t.Fatalf("update keeping the token: %+v", it)
	}
	tok, err := e.app.Integrations.Token(t.Context(), it.ID)
	if err != nil || tok != plexToken {
		t.Fatalf("stored token: %v", err)
	}
	for _, b := range []map[string]any{
		{"name": "", "url": pms.URL},
		{"name": "x", "url": "ftp://plex"},
		{"name": "x", "url": pms.URL + "?X-Plex-Token=abc"},
		{"name": "x", "url": pms.URL, "type": "sonarr"},
		{"name": "x", "url": pms.URL, "settings": map[string]any{"dataPath": "relative"}},
		{"name": "x", "url": pms.URL, "settings": map[string]any{"backup": map[string]any{"enabled": true, "destinationId": 42, "cron": "0 6 * * *"}}},
		{"name": "x", "url": pms.URL, "bogus": 1},
	} {
		if code, msg := e.status(t, "PUT", path, b); code != 400 {
			t.Errorf("update %v: %d %q, want 400", b, code, msg)
		}
	}
	e.call(t, 200, "PUT", path, map[string]any{"name": "Plex 2", "url": pms.URL, "clearApiKey": true}, &it)
	if it.HasAPIKey {
		t.Fatal("clearApiKey kept the key")
	}
	if code, _ := e.status(t, "POST", "/integrations", map[string]any{"type": "plex", "name": "plex 2", "url": pms.URL}); code != 400 {
		t.Errorf("duplicate name: %d", code)
	}
	e.call(t, 204, "DELETE", path, nil, nil)
	if code, _ := e.status(t, "GET", path, nil); code != 404 {
		t.Fatalf("deleted integration: %d", code)
	}
}

func TestIntegrationTest(t *testing.T) {
	e := newEnv(t, nil)
	pms := plextest.NewServer(t, plexToken)
	pms.SetPref("ButlerStartHour", 1)
	pms.SetPref("ButlerEndHour", 4)
	var res integrationTestResult
	e.call(t, 200, "POST", "/integrations/test", map[string]any{"type": "plex", "url": pms.URL, "apiKey": plexToken}, &res)
	if !res.OK || res.Version == "" || res.MachineIdentifier == "" || res.ButlerStartHour == nil || *res.ButlerStartHour != 1 || *res.ButlerEndHour != 4 {
		t.Fatalf("test with a good token: %+v", res)
	}
	code, raw := e.raw(t, "POST", "/integrations/test", map[string]any{"type": "plex", "url": pms.URL, "apiKey": "wrong-token-123456789"})
	if code != 200 || strings.Contains(string(raw), "wrong-token-123456789") {
		t.Fatalf("test with a bad token: %d %s", code, raw)
	}
	_ = json.Unmarshal(raw, &res)
	if res.OK || !strings.Contains(res.Message, "401") {
		t.Fatalf("test with a bad token: %+v", res)
	}
	// The edit form: an id and no apiKey uses the stored token.
	var it integrations.Integration
	e.call(t, 201, "POST", "/integrations", map[string]any{"type": "plex", "name": "Plex", "url": pms.URL, "apiKey": plexToken}, &it)
	e.call(t, 200, "POST", "/integrations/test", map[string]any{"type": "plex", "url": pms.URL, "id": it.ID}, &res)
	if !res.OK {
		t.Fatalf("test with the stored token: %+v", res)
	}
	for _, r := range pms.Requests() {
		if strings.Contains(r.RawQuery, plexToken) {
			t.Fatalf("token sent in a URL: %+v", r)
		}
	}
	e.call(t, 200, "POST", "/integrations/test", map[string]any{"type": "sonarr", "url": pms.URL}, &res)
	if res.OK || !strings.Contains(res.Message, "not available") {
		t.Fatalf("test of a sonarr integration: %+v", res)
	}
	for _, b := range []map[string]any{
		{"type": "plex", "url": ""},
		{"type": "plex", "url": "http://user:pass@plex:32400"},
		{"url": pms.URL},
		{"type": "nope", "url": pms.URL},
		{"type": "plex", "url": pms.URL, "id": -1},
	} {
		if code, msg := e.status(t, "POST", "/integrations/test", b); code != 400 {
			t.Errorf("test %v: %d %q, want 400", b, code, msg)
		}
	}
	if code, _ := e.status(t, "POST", "/integrations/test", map[string]any{"type": "plex", "url": pms.URL, "id": 999}); code != 404 {
		t.Errorf("test with an unknown id: %d", code)
	}
}

func TestPlexSections(t *testing.T) {
	e := newEnv(t, nil)
	pms := plextest.NewServer(t, plexToken)
	media := e.mkdir(t, "media")
	e.mkdir(t, "media/movies")
	e.mkdir(t, "media/tv")
	var it integrations.Integration
	e.call(t, 201, "POST", "/integrations", map[string]any{"type": "plex", "name": "Plex", "url": pms.URL, "apiKey": plexToken,
		"settings": map[string]any{"pathMappings": []any{map[string]any{"plex": "/data", "local": media}}}}, &it)
	var sections []plexSection
	e.call(t, 200, "GET", fmt.Sprintf("/integrations/%d/plex/sections", it.ID), nil, &sections)
	got := map[string]plexLocation{}
	for _, s := range sections {
		for _, l := range s.Locations {
			got[s.Key+" "+l.Path] = l
		}
	}
	want := map[string]plexLocation{
		"1 /data/movies":  {Path: "/data/movies", LocalPath: filepath.Join(media, "movies"), Exists: true},
		"2 /data/tv":      {Path: "/data/tv", LocalPath: filepath.Join(media, "tv"), Exists: true},
		"2 /data/tv-kids": {Path: "/data/tv-kids", LocalPath: filepath.Join(media, "tv-kids"), Exists: false},
	}
	if len(got) != len(want) {
		t.Fatalf("sections: %+v", sections)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: %+v, want %+v", k, got[k], w)
		}
	}
	// Without a mapping the local path is empty; with a bad token Plex's refusal is a 502.
	e.call(t, 200, "PUT", fmt.Sprintf("/integrations/%d", it.ID), map[string]any{"name": "Plex", "url": pms.URL, "apiKey": "bad-token-0987654321",
		"settings": map[string]any{"pathMappings": []any{}}}, &it)
	code, msg := e.status(t, "GET", fmt.Sprintf("/integrations/%d/plex/sections", it.ID), nil)
	if code != 502 || !strings.Contains(msg, "unauthorized") || strings.Contains(msg, "bad-token") {
		t.Fatalf("sections with a bad token: %d %q", code, msg)
	}
	var other integrations.Integration
	e.call(t, 201, "POST", "/integrations", map[string]any{"type": "sonarr", "name": "Sonarr", "url": "http://sonarr:8989"}, &other)
	if code, _ := e.status(t, "GET", fmt.Sprintf("/integrations/%d/plex/sections", other.ID), nil); code != 400 {
		t.Fatalf("sections of a sonarr integration: %d", code)
	}
	if code, _ := e.status(t, "GET", "/integrations/999/plex/sections", nil); code != 404 {
		t.Fatalf("sections of a missing integration: %d", code)
	}
}

func TestPlexBackupScheduleFollowsSettings(t *testing.T) {
	e := newEnv(t, nil)
	pms := plextest.NewServer(t, plexToken)
	d1 := e.createDestination(t, "NAS", e.mkdir(t, "nas"), nil, nil)
	d2 := e.createDestination(t, "NAS2", e.mkdir(t, "nas2"), nil, nil)
	backup := func(dest int64, cron string, enabled bool) map[string]any {
		return map[string]any{"type": "plex", "name": "Plex", "url": pms.URL, "apiKey": plexToken,
			"settings": map[string]any{"dataPath": "/plex", "backup": map[string]any{"destinationId": dest, "cron": cron, "enabled": enabled}}}
	}
	// Enabled without a cron: the default 06:00 daily.
	var it integrations.Integration
	e.call(t, 201, "POST", "/integrations", backup(d1, "", true), &it)
	if b := plexSettingsOf(t, it).Backup; b.Cron != DefaultPlexBackupCron || !b.Enabled || b.DestinationID != d1 {
		t.Fatalf("backup settings: %+v", b)
	}
	scs := e.plexSchedules(t, it.ID)
	if len(scs) != 1 || scs[0].Cron != DefaultPlexBackupCron || !scs[0].Enabled || scs[0].Params.DestinationID != d1 ||
		scs[0].Description != "Plex database backup of Plex to NAS" {
		t.Fatalf("schedule: %+v", scs)
	}
	path := fmt.Sprintf("/integrations/%d", it.ID)

	// Editing the schedule on System → Tasks shows in the integration's settings.
	var sv scheduleView
	e.call(t, 200, "PUT", fmt.Sprintf("/schedules/%d", scs[0].ID), map[string]any{"cron": "15 7 * * *", "enabled": false}, &sv)
	e.call(t, 200, "GET", path, nil, &it)
	if b := plexSettingsOf(t, it).Backup; b.Cron != "15 7 * * *" || b.Enabled {
		t.Fatalf("settings after a schedule edit: %+v", b)
	}

	// Another destination replaces the schedule.
	e.call(t, 200, "PUT", path, backup(d2, "0 8 * * *", true), &it)
	scs = e.plexSchedules(t, it.ID)
	if len(scs) != 1 || scs[0].Params.DestinationID != d2 || scs[0].Cron != "0 8 * * *" || !scs[0].Enabled {
		t.Fatalf("schedule after a destination change: %+v", scs)
	}
	if code, msg := e.status(t, "PUT", path, backup(d2, "every day", true)); code != 400 || !strings.Contains(msg, "backup.cron") {
		t.Fatalf("invalid cron: %d %q", code, msg)
	}

	// A manual backup: 202 with the job (the destination defaults to the integration's).
	var j jobs.Job
	e.call(t, 202, "POST", path+"/plex/backup", map[string]any{"dryRun": true}, &j)
	if j.Type != jobs.TypePlexDBBackup || !j.DryRun || j.Params.IntegrationID != it.ID || j.Params.DestinationID != d2 {
		t.Fatalf("backup job: %+v", j)
	}
	if code, _ := e.status(t, "POST", path+"/plex/backup", map[string]any{"destinationId": 999}); code != 400 {
		t.Fatalf("backup to a missing destination: %d", code)
	}

	// Deleting the destination removes the schedule and turns the backup off.
	e.waitJob(t, j.ID)
	e.call(t, 204, "DELETE", fmt.Sprintf("/destinations/%d", d2), nil, nil)
	if scs := e.plexSchedules(t, it.ID); len(scs) != 0 {
		t.Fatalf("schedule of a deleted destination: %+v", scs)
	}
	e.call(t, 200, "GET", path, nil, &it)
	if b := plexSettingsOf(t, it).Backup; b.DestinationID != 0 || b.Enabled {
		t.Fatalf("backup settings after the destination was deleted: %+v", b)
	}

	// Disabled without a cron: no schedule. Deleting the integration removes its schedule.
	e.call(t, 200, "PUT", path, backup(d1, "", false), &it)
	if scs := e.plexSchedules(t, it.ID); len(scs) != 0 {
		t.Fatalf("schedule of a disabled backup without cron: %+v", scs)
	}
	e.call(t, 200, "PUT", path, backup(d1, "0 6 * * *", true), &it)
	if len(e.plexSchedules(t, it.ID)) != 1 {
		t.Fatal("no schedule")
	}
	e.call(t, 204, "DELETE", path, nil, nil)
	if scs := e.plexSchedules(t, it.ID); len(scs) != 0 {
		t.Fatalf("schedule of a deleted integration: %+v", scs)
	}
}

// TestStoredPlexTokenOnlyGoesToTheStoredURL: a caller who does not know the token cannot make
// Bunkarr send it to a URL of its choice, through the Test button or by changing the URL (S8).
func TestStoredPlexTokenOnlyGoesToTheStoredURL(t *testing.T) {
	e := newEnv(t, nil)
	pms := plextest.NewServer(t, plexToken)
	evil := plextest.NewServer(t, "") // answers every request like Plex
	var it integrations.Integration
	e.call(t, 201, "POST", "/integrations", map[string]any{"type": "plex", "name": "Plex", "url": pms.URL, "apiKey": plexToken}, &it)
	path := fmt.Sprintf("/integrations/%d", it.ID)

	code, msg := e.status(t, "POST", "/integrations/test", map[string]any{"type": "plex", "id": it.ID, "url": evil.URL})
	if code != 400 || !strings.Contains(msg, "enter the Plex token") {
		t.Fatalf("test of another URL with the stored token: %d %q", code, msg)
	}
	code, msg = e.status(t, "PUT", path, map[string]any{"name": "Plex", "url": evil.URL})
	if code != 400 || !strings.HasPrefix(msg, "the URL changed, so enter the token again") {
		t.Fatalf("URL change without the token: %d %q", code, msg)
	}
	e.call(t, 200, "GET", path, nil, &it)
	if it.URL != pms.URL || !it.HasAPIKey {
		t.Fatalf("a refused update changed the integration: %+v", it)
	}
	e.call(t, 200, "GET", path+"/plex/sections", nil, nil)
	if reqs := evil.Requests(); len(reqs) != 0 {
		t.Fatalf("the other server was contacted: %+v", reqs)
	}

	// The stored URL (in any spelling) still tests with the stored token.
	var res integrationTestResult
	e.call(t, 200, "POST", "/integrations/test", map[string]any{"type": "plex", "id": it.ID, "url": pms.URL + "/"}, &res)
	if !res.OK {
		t.Fatalf("test of the stored URL: %+v", res)
	}
	// A new URL with the token entered again, or without a token at all, is accepted.
	moved := plextest.NewServer(t, plexToken)
	e.call(t, 200, "POST", "/integrations/test", map[string]any{"type": "plex", "id": it.ID, "url": moved.URL, "apiKey": plexToken}, &res)
	if !res.OK {
		t.Fatalf("test of a new URL with the token: %+v", res)
	}
	e.call(t, 200, "PUT", path, map[string]any{"name": "Plex", "url": moved.URL, "apiKey": plexToken}, &it)
	if it.URL != moved.URL || !it.HasAPIKey {
		t.Fatalf("URL change with the token: %+v", it)
	}
	e.call(t, 200, "PUT", path, map[string]any{"name": "Plex", "url": pms.URL, "clearApiKey": true}, &it)
	if it.URL != pms.URL || it.HasAPIKey {
		t.Fatalf("URL change clearing the token: %+v", it)
	}
}

// TestIntegrationTestDoesNotRegisterTheTypedToken: a token typed into the Test form is redacted
// locally, not added to the process-wide secret registry (which request input could grow).
func TestIntegrationTestDoesNotRegisterTheTypedToken(t *testing.T) {
	e := newEnv(t, nil)
	pms := plextest.NewServer(t, plexToken)
	const typed = "typed-token-never-saved-4242"
	code, raw := e.raw(t, "POST", "/integrations/test", map[string]any{"type": "plex", "url": pms.URL, "apiKey": typed})
	if code != 200 || strings.Contains(string(raw), typed) {
		t.Fatalf("test with an unsaved token: %d %s", code, raw)
	}
	if logging.ContainsSecret("x " + typed + " y") {
		t.Fatal("the unsaved token was registered as a secret")
	}
}

// TestIntegrationDeleteRefusedWhileItsBackupIsActive: a Plex backup that is queued or running
// keeps its integration (the finished version would otherwise be left unrecorded).
func TestIntegrationDeleteRefusedWhileItsBackupIsActive(t *testing.T) {
	e := newEnvWith(t, nil, func(o *AppOptions) { o.Workers = 1 })
	pms := plextest.NewServer(t, plexToken)
	dest := e.createDestination(t, "NAS", e.mkdir(t, "nas"), nil, nil)
	var it, other integrations.Integration
	e.call(t, 201, "POST", "/integrations", map[string]any{"type": "plex", "name": "Plex", "url": pms.URL, "apiKey": plexToken,
		"settings": map[string]any{"dataPath": "/plex"}}, &it)
	e.call(t, 201, "POST", "/integrations", map[string]any{"type": "plex", "name": "Plex 2", "url": pms.URL, "apiKey": plexToken}, &other)
	// Stop the manager so the queued job stays queued.
	if err := e.app.Jobs.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	var j jobs.Job
	path := fmt.Sprintf("/integrations/%d", it.ID)
	e.call(t, 202, "POST", path+"/plex/backup", map[string]any{"destinationId": dest}, &j)
	if code, msg := e.status(t, "DELETE", path, nil); code != 409 || !strings.Contains(msg, "queued or running") {
		t.Fatalf("delete with a queued backup: %d %q", code, msg)
	}
	e.call(t, 200, "GET", path, nil, nil)
	// Another integration's jobs do not block it.
	e.call(t, 204, "DELETE", fmt.Sprintf("/integrations/%d", other.ID), nil, nil)
	e.call(t, 200, "POST", fmt.Sprintf("/jobs/%d/cancel", j.ID), nil, &j)
	if j.Status != jobs.StatusCancelled {
		t.Fatalf("cancelled job: %+v", j)
	}
	e.call(t, 204, "DELETE", path, nil, nil)
}

// TestStoredPlexTokenNotSentToAURLReadBeforeItChanged: a request that read the integration while
// its URL pointed elsewhere never sends the token saved later for the real URL there. An API-key
// holder points the integration at its server with a junk token and keeps calling the sections
// and Test endpoints; the owner restores the real URL and types the real token again. The URL
// and the token must come from the same row, or a request that read the URL before the owner's
// save and the token after it leaks the token (S8).
func TestStoredPlexTokenNotSentToAURLReadBeforeItChanged(t *testing.T) {
	e := newEnv(t, nil)
	pms := plextest.NewServer(t, plexToken)
	evil := plextest.NewServer(t, "")
	var it integrations.Integration
	e.call(t, 201, "POST", "/integrations", map[string]any{"type": "plex", "name": "Plex", "url": pms.URL, "apiKey": plexToken}, &it)
	path := fmt.Sprintf("/integrations/%d", it.ID)
	key := e.auth.APIKey()
	testBody := fmt.Sprintf(`{"type":"plex","id":%d,"url":%q}`, it.ID, evil.URL)
	var stop atomic.Bool
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Go(func() {
			for !stop.Load() {
				req, _ := http.NewRequest("GET", e.srv.URL+"/api/v1"+path+"/plex/sections", nil)
				if g%2 == 1 {
					req, _ = http.NewRequest("POST", e.srv.URL+"/api/v1/integrations/test", bytes.NewReader([]byte(testBody)))
					req.Header.Set("Content-Type", "application/json")
				}
				req.Header.Set("X-Api-Key", key)
				if res, err := http.DefaultClient.Do(req); err == nil {
					_, _ = io.Copy(io.Discard, res.Body)
					_ = res.Body.Close()
				}
			}
		})
	}
	leaked := 0
	for round := 0; round < 60 && leaked == 0; round++ {
		e.call(t, 200, "PUT", path, map[string]any{"name": "Plex", "url": evil.URL, "apiKey": "junk-token-0000000000"}, nil)
		e.call(t, 200, "PUT", path, map[string]any{"name": "Plex", "url": pms.URL, "apiKey": plexToken}, nil)
		for _, r := range evil.Requests() {
			if r.Header.Get("X-Plex-Token") == plexToken {
				leaked++
			}
		}
	}
	stop.Store(true)
	wg.Wait()
	if leaked > 0 {
		t.Fatalf("the other server received the saved token %d times", leaked)
	}
}

// TestStoredSecretsStayRedactedWhenOthersAreReplaced: storing and replacing many notification
// URLs never drops a secret that is still stored (the Plex token) from log redaction.
func TestStoredSecretsStayRedactedWhenOthersAreReplaced(t *testing.T) {
	e := newEnv(t, nil)
	pms := plextest.NewServer(t, plexToken)
	var it integrations.Integration
	e.call(t, 201, "POST", "/integrations", map[string]any{"type": "plex", "name": "Plex", "url": pms.URL, "apiKey": plexToken}, &it)
	var n notify.Notification
	for round := range 4 {
		var b strings.Builder
		for i := 0; b.Len() < 16000; i++ {
			fmt.Fprintf(&b, "a://%d-%06d,", round, i)
		}
		urls := strings.TrimSuffix(b.String(), ",")
		if round == 0 {
			e.call(t, 201, "POST", "/notifications", map[string]any{"name": "many", "apiUrl": "http://127.0.0.1:1", "urls": urls}, &n)
		} else {
			e.call(t, 200, "PUT", fmt.Sprintf("/notifications/%d", n.ID), map[string]any{"name": "many", "apiUrl": "http://127.0.0.1:1", "urls": urls}, &n)
		}
	}
	if !logging.ContainsSecret("x " + plexToken + " y") {
		t.Fatalf("the stored Plex token is no longer redacted: %q", logging.RedactSecrets("token="+plexToken))
	}
}
