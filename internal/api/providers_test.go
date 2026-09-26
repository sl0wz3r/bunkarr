package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/integrations/maintainerr/maintainerrtest"
	"github.com/sl0wz3r/bunkarr/internal/integrations/plex/plextest"
	"github.com/sl0wz3r/bunkarr/internal/integrations/seerr/seerrtest"
	"github.com/sl0wz3r/bunkarr/internal/integrations/tautulli/tautullitest"
	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

const libPlexToken = "plex-token-provider-api"

// plexLibraries numbers the Plex integrations the tests create (names are unique).
var plexLibraries int

// createPlexLibrary creates a Plex integration over a fake Plex serving the recorded library of
// testdata/tautulli/plex; index turns its library index on.
func createPlexLibrary(t *testing.T, e *env, index bool) (*plextest.Server, int64) {
	t.Helper()
	plexLibraries++
	pms := plextest.NewServer(t, libPlexToken)
	pms.ServeLibrary(t)
	settings := map[string]any{}
	if index {
		settings["index"] = map[string]any{"enabled": true}
	}
	code, raw := e.raw(t, "POST", "/integrations", map[string]any{"type": "plex", "name": fmt.Sprintf("Plex %d", plexLibraries), "url": pms.URL, "apiKey": libPlexToken, "settings": settings})
	if code != http.StatusCreated {
		t.Fatalf("create plex: %d %s", code, raw)
	}
	return pms, idOf(t, raw)
}

func noSecret(t *testing.T, raw []byte, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if s != "" && strings.Contains(string(raw), s) {
			t.Fatalf("a secret appears in %s", raw)
		}
	}
	if strings.Contains(string(raw), "@example.invalid") {
		t.Fatalf("an e-mail address appears in %s", raw)
	}
}

func TestProviderTest(t *testing.T) {
	e := newEnv(t, nil)
	_, plexID := createPlexLibrary(t, e, false)
	taut := tautullitest.NewServer(t)
	link := map[string]any{"plexIntegrationId": plexID}

	var res providerTestResult
	code, raw := e.raw(t, "POST", "/integrations/test", map[string]any{"type": "tautulli", "url": taut.URL, "apiKey": tautullitest.Key, "settings": link})
	if code != 200 || json.Unmarshal(raw, &res) != nil || !res.OK || res.Version != "v2.18.1" || res.AppName != "Tautulli" || res.PlexMatches == nil || !*res.PlexMatches {
		t.Fatalf("tautulli test: %d %s", code, raw)
	}
	noSecret(t, raw, tautullitest.Key, libPlexToken)
	// Without a linked server plexMatches is null.
	code, raw = e.raw(t, "POST", "/integrations/test", map[string]any{"type": "tautulli", "url": taut.URL, "apiKey": tautullitest.Key})
	if code != 200 || !strings.Contains(string(raw), `"plexMatches":null`) {
		t.Fatalf("unlinked: %d %s", code, raw)
	}
	code, raw = e.raw(t, "POST", "/integrations/test", map[string]any{"type": "tautulli", "url": taut.URL, "apiKey": "wrong-key-wrong-key"})
	if code != 200 || !strings.Contains(string(raw), "rejected the API key") || strings.Contains(string(raw), "wrong-key-wrong-key") {
		t.Fatalf("wrong key: %d %s", code, raw)
	}
	old := tautullitest.NewServer(t)
	old.Version217()
	code, raw = e.raw(t, "POST", "/integrations/test", map[string]any{"type": "tautulli", "url": old.URL, "apiKey": tautullitest.Key})
	if code != 200 || !strings.Contains(string(raw), "2.18.0") || strings.Contains(string(raw), `"ok":true`) {
		t.Fatalf("2.17: %d %s", code, raw)
	}
	// Invalid unsaved settings are a 400.
	if code, msg := e.status(t, "POST", "/integrations/test", map[string]any{"type": "tautulli", "url": taut.URL, "apiKey": tautullitest.Key,
		"settings": map[string]any{"refresh": map[string]any{"staleAfterHours": 0}}}); code != 400 || !strings.Contains(msg, "staleAfterHours") {
		t.Fatalf("invalid settings: %d %s", code, msg)
	}
	if code, msg := e.status(t, "POST", "/integrations/test", map[string]any{"type": "tautulli", "url": taut.URL, "apiKey": tautullitest.Key,
		"settings": map[string]any{"plexIntegrationId": -1}}); code != 400 {
		t.Fatalf("negative link: %d %s", code, msg)
	}

	srr := seerrtest.NewServer(t)
	code, raw = e.raw(t, "POST", "/integrations/test", map[string]any{"type": "seerr", "url": srr.URL, "apiKey": seerrtest.Key})
	if code != 200 || json.Unmarshal(raw, &res) != nil || !res.OK || res.Version != "3.4.1" || res.AppName != "Seerr" {
		t.Fatalf("seerr test: %d %s", code, raw)
	}
	noSecret(t, raw, seerrtest.Key)

	m := maintainerrtest.NewServer(t, maintainerrtest.V341)
	if code, msg := e.status(t, "POST", "/integrations/test", map[string]any{"type": "maintainerr", "url": m.URL, "apiKey": "x"}); code != 400 ||
		!strings.Contains(msg, "no API authentication") {
		t.Fatalf("maintainerr with a key: %d %s", code, msg)
	}
	code, raw = e.raw(t, "POST", "/integrations/test", map[string]any{"type": "maintainerr", "url": m.URL, "settings": link})
	if code != 200 || json.Unmarshal(raw, &res) != nil || !res.OK || res.Version != "3.4.1" || res.PlexMatches != nil ||
		!strings.Contains(res.Message, "library index") {
		t.Fatalf("maintainerr test without an index: %d %s", code, raw)
	}
	old3 := maintainerrtest.NewServer(t, maintainerrtest.V341)
	old3.OldVersion("3.3.0")
	code, raw = e.raw(t, "POST", "/integrations/test", map[string]any{"type": "maintainerr", "url": old3.URL})
	if code != 200 || !strings.Contains(string(raw), "3.4.0") || strings.Contains(string(raw), `"ok":true`) {
		t.Fatalf("maintainerr 3.3: %d %s", code, raw)
	}
}

// The form tests before a Plex server is chosen (none connected, or several and none picked) and
// sends plexIntegrationId 0: the URL and key are tested, plexMatches is null (§13), not a 400.
func TestProviderTestBeforeAPlexServerIsChosen(t *testing.T) {
	e := newEnv(t, nil)
	unlinked := map[string]any{"plexIntegrationId": 0}
	taut := tautullitest.NewServer(t)
	var res providerTestResult
	code, raw := e.raw(t, "POST", "/integrations/test", map[string]any{"type": "tautulli", "url": taut.URL, "apiKey": tautullitest.Key, "settings": unlinked})
	if code != 200 || json.Unmarshal(raw, &res) != nil || !res.OK || res.Version != "v2.18.1" || !strings.Contains(string(raw), `"plexMatches":null`) {
		t.Fatalf("tautulli without a Plex server: %d %s", code, raw)
	}
	code, raw = e.raw(t, "POST", "/integrations/test", map[string]any{"type": "tautulli", "url": taut.URL, "apiKey": "wrong-key-wrong-key", "settings": unlinked})
	if code != 200 || !strings.Contains(string(raw), "rejected the API key") {
		t.Fatalf("wrong key without a Plex server: %d %s", code, raw)
	}
	m := maintainerrtest.NewServer(t, maintainerrtest.V341)
	code, raw = e.raw(t, "POST", "/integrations/test", map[string]any{"type": "maintainerr", "url": m.URL, "settings": unlinked})
	if code != 200 || json.Unmarshal(raw, &res) != nil || !res.OK || res.Version != "3.4.1" || !strings.Contains(string(raw), `"plexMatches":null`) {
		t.Fatalf("maintainerr without a Plex server: %d %s", code, raw)
	}
	// A save still needs the link.
	if code, msg := e.status(t, "POST", "/integrations", map[string]any{"type": "tautulli", "name": "Tautulli", "url": taut.URL, "apiKey": tautullitest.Key,
		"settings": unlinked}); code != 400 || !strings.Contains(msg, "plexIntegrationId") {
		t.Fatalf("create without a link: %d %s", code, msg)
	}
}

func TestProviderTestMaintainerrPlexMatches(t *testing.T) {
	e := newEnv(t, nil)
	_, plexID := createPlexLibrary(t, e, true)
	var job jobs.Job
	e.call(t, http.StatusAccepted, "POST", fmt.Sprintf("/integrations/%d/refresh", plexID), nil, &job)
	if j := e.waitJob(t, job.ID); j.Status != jobs.StatusCompleted {
		t.Fatalf("plex index refresh: %+v", j)
	}
	m := maintainerrtest.NewServer(t, maintainerrtest.V341)
	var res providerTestResult
	e.call(t, 200, "POST", "/integrations/test", map[string]any{"type": "maintainerr", "url": m.URL, "settings": map[string]any{"plexIntegrationId": plexID}}, &res)
	if !res.OK || res.PlexMatches == nil || !*res.PlexMatches {
		t.Fatalf("maintainerr: %+v", res)
	}
}

func TestProviderCreateSchedulesAndRefresh(t *testing.T) {
	e := newEnv(t, nil)
	_, plexID := createPlexLibrary(t, e, true)
	taut := tautullitest.NewServer(t)
	code, raw := e.raw(t, "POST", "/integrations", map[string]any{"type": "tautulli", "name": "Tautulli", "url": taut.URL, "apiKey": tautullitest.Key,
		"settings": map[string]any{"plexIntegrationId": plexID}})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, raw)
	}
	noSecret(t, raw, tautullitest.Key)
	tautID := idOf(t, raw)
	scheds, err := e.app.Jobs.Store().ListSchedules(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	find := func(list []jobqueue.Schedule, id int64) *jobqueue.Schedule {
		for i, sc := range list {
			if sc.JobType == jobs.TypeRefresh && sc.Params.IntegrationID == id {
				return &list[i]
			}
		}
		return nil
	}
	if sc := find(scheds, tautID); sc == nil || sc.Cron != integrations.DefaultTautulliRefreshCron || !sc.Enabled {
		t.Fatalf("tautulli schedule %+v", sc)
	}
	if sc := find(scheds, plexID); sc == nil || sc.Cron != integrations.DefaultPlexIndexCron || !sc.Enabled {
		t.Fatalf("plex index schedule %+v", sc)
	}
	// The create queued a refresh of both (the Plex index first; Tautulli waits for it or fails).
	list, err := e.app.Jobs.Store().ListJobs(t.Context(), jobqueue.JobQuery{Type: jobs.TypeRefresh})
	if err != nil {
		t.Fatal(err)
	}
	queued := map[int64]bool{}
	for _, j := range list.Records {
		queued[j.Params.IntegrationID] = true
	}
	if !queued[tautID] || !queued[plexID] {
		t.Fatalf("refreshes queued for %v", queued)
	}
	// Turning the refresh off disables the schedule; the view shows the schedule's state.
	var it integrations.Integration
	e.call(t, 200, "PUT", fmt.Sprintf("/integrations/%d", tautID), map[string]any{"name": "Tautulli", "url": taut.URL,
		"settings": map[string]any{"plexIntegrationId": plexID, "refresh": map[string]any{"enabled": false, "cron": "5 3 * * *"}}}, &it)
	scheds, _ = e.app.Jobs.Store().ListSchedules(t.Context())
	if sc := find(scheds, tautID); sc == nil || sc.Enabled || sc.Cron != "5 3 * * *" {
		t.Fatalf("schedule after the update %+v", sc)
	}
	if !strings.Contains(string(it.Settings), `"cron":"5 3 * * *"`) {
		t.Fatalf("settings %s", it.Settings)
	}
	// A Plex integration without the index has no refresh schedule and refuses a refresh.
	_, plain := createPlexLibrary(t, e, false)
	scheds, _ = e.app.Jobs.Store().ListSchedules(t.Context())
	if sc := find(scheds, plain); sc != nil {
		t.Fatalf("schedule for a Plex integration without the index: %+v", sc)
	}
	// The message points to where the switch is: Settings → Connect's library index card.
	if code, msg := e.status(t, "POST", fmt.Sprintf("/integrations/%d/refresh", plain), nil); code != 400 || !strings.Contains(msg, "turned off") ||
		!strings.Contains(msg, "Settings → Connect (Library index · Plex ") || strings.Contains(msg, "Settings → Plex") {
		t.Fatalf("refresh without the index: %d %s", code, msg)
	}
	var job jobs.Job
	e.call(t, http.StatusAccepted, "POST", fmt.Sprintf("/integrations/%d/refresh", tautID), map[string]any{"dryRun": true}, &job)
	if job.Type != jobs.TypeRefresh || !job.DryRun {
		t.Fatalf("job %+v", job)
	}
}

func TestSeerrUsers(t *testing.T) {
	e := newEnv(t, nil)
	srr := seerrtest.NewServer(t)
	code, raw := e.raw(t, "POST", "/integrations", map[string]any{"type": "seerr", "name": "Seerr", "url": srr.URL, "apiKey": seerrtest.Key})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, raw)
	}
	id := idOf(t, raw)
	code, raw = e.raw(t, "GET", fmt.Sprintf("/integrations/%d/seerr/users", id), nil)
	if code != 200 {
		t.Fatalf("users: %d %s", code, raw)
	}
	noSecret(t, raw, seerrtest.Key)
	var users []struct {
		ID    int64  `json:"id"`
		Label string `json:"label"`
	}
	if err := json.Unmarshal(raw, &users); err != nil || len(users) != 3 || users[0].Label != "fixture-admin" || users[2].Label != "Seerr user #3" {
		t.Fatalf("users %s", raw)
	}
	_, plexID := createPlexLibrary(t, e, false)
	if code, _ := e.status(t, "GET", fmt.Sprintf("/integrations/%d/seerr/users", plexID), nil); code != 400 {
		t.Fatalf("users of a Plex integration: %d", code)
	}
	srr.Close()
	if code, msg := e.status(t, "GET", fmt.Sprintf("/integrations/%d/seerr/users", id), nil); code != http.StatusBadGateway || strings.Contains(msg, srr.URL) {
		t.Fatalf("upstream down: %d %s", code, msg)
	}
	// The tier editor's field suggestions carry the labels.
	var fields []struct {
		Field       string `json:"field"`
		Available   bool   `json:"available"`
		Suggestions []struct {
			Value any    `json:"value"`
			Label string `json:"label"`
		} `json:"suggestions"`
	}
	e.call(t, 200, "GET", "/tiers/fields", nil, &fields)
	for _, f := range fields {
		if !f.Available {
			t.Errorf("field %s unavailable", f.Field)
		}
	}
}
