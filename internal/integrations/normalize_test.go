package integrations

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/config"
	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/logging"
)

// phase1Row is an integration row as Phase 1 stored it: non-Plex types with any settings object,
// and no webhook key (migration 0003 adds the column with an empty default).
type phase1Row struct {
	id       int64
	typ      Type
	name     string
	settings string
	apiKey   string // plaintext; sealed when seeded
	enabled  bool
}

var phase1Rows = []phase1Row{
	{1, TypePlex, "Plex", `{"dataPath":"/plex","pathMappings":[],"backup":{"destinationId":0,"cron":"","enabled":false}}`, "plex-token-phase1-0001", true},
	{2, TypeRadarr, "Radarr", `{}`, "radarr-key-phase1-0002", true},
	{3, TypeSonarr, "Sonarr", `{"pathMappings":[{"arr":"/tv/","local":"/media/tv"}],"notes":"kept by Phase 1"}`, "sonarr-key-phase1-0003", true},
	{4, TypeLidarr, "Lidarr", `{"pathMappings":[{"arr":"music","local":"/media/music"}]}`, "", true},
	{5, TypeTautulli, "Tautulli", `{"apikey":"not a setting"}`, "tautulli-key-phase1-0005", true},
	{6, TypeTautulli, "Tautulli linked", `{"plexIntegrationId":1}`, "tautulli-key-phase1-0006", true},
	{7, TypeMaintainerr, "Maintainerr with key", `{"plexIntegrationId":1}`, "maint-key-phase1-0007", true},
	{8, TypeSeerr, "Seerr", `{}`, "seerr-key-phase1-0008", false},
	{9, TypeMaintainerr, "Maintainerr linked to Radarr", `{"plexIntegrationId":2}`, "", false},
}

const seedTime = "2026-09-01T00:00:00.000000000Z"

// seedPhase1 writes phase1Rows into a fresh database as Phase 1 left them.
func seedPhase1(t *testing.T) (*db.DB, *config.Keyring) {
	t.Helper()
	_, d, kr := newTestStore(t)
	ctx := context.Background()
	err := d.Write(ctx, func(tx *sql.Tx) error {
		for _, r := range phase1Rows {
			sealed := ""
			if r.apiKey != "" {
				var err error
				if sealed, err = kr.Seal(r.apiKey, apiKeyAAD(r.id)); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO integrations (id, type, name, url, api_key, enabled, settings, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, r.id, string(r.typ), r.name, "http://"+string(r.typ)+":1", sealed, r.enabled, r.settings,
				seedTime, seedTime); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return d, kr
}

type rowState struct {
	settings string
	enabled  bool
	apiKey   string
	hook     string
}

func rowStates(t *testing.T, d *db.DB) map[int64]rowState {
	t.Helper()
	rs, err := d.Reader().Query(`SELECT id, settings, enabled, api_key, webhook_key FROM integrations`)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	out := map[int64]rowState{}
	for rs.Next() {
		var id int64
		var st rowState
		if err := rs.Scan(&id, &st.settings, &st.enabled, &st.apiKey, &st.hook); err != nil {
			t.Fatal(err)
		}
		out[id] = st
	}
	return out
}

func invalidIDs(rep StartupReport) []int64 {
	var ids []int64
	for _, inv := range rep.Invalid {
		ids = append(ids, inv.ID)
	}
	return ids
}

// TestNormalizeStoredPhase1Rows runs the start-up normalization over a seeded Phase 1 database.
func TestNormalizeStoredPhase1Rows(t *testing.T) {
	ctx := context.Background()
	d, kr := seedPhase1(t)
	before := rowStates(t, d)
	s := NewStore(d, kr)
	rep, err := s.NormalizeStored(ctx)
	if err != nil {
		t.Fatal(err)
	}
	after := rowStates(t, d)

	// Valid rows are normalized: unknown fields dropped, defaults applied.
	if !slices.Equal(rep.Normalized, []int64{2, 3, 6, 8}) {
		t.Errorf("Normalized = %v, want [2 3 6 8]", rep.Normalized)
	}
	var sonarr ArrSettings
	if err := json.Unmarshal([]byte(after[3].settings), &sonarr); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sonarr.PathMappings, []ArrPathMapping{{Arr: "/tv", Local: "/media/tv"}}) || sonarr.Refresh.Cron != DefaultArrRefreshCron ||
		!sonarr.Refresh.Enabled || strings.Contains(after[3].settings, "notes") {
		t.Errorf("sonarr settings = %s", after[3].settings)
	}
	if after[6].settings != `{"plexIntegrationId":1,"refresh":{"cron":"0 2 * * *","enabled":true,"staleAfterHours":72}}` {
		t.Errorf("tautulli settings = %s", after[6].settings)
	}
	// Invalid rows are disabled and keep their settings.
	if got := invalidIDs(rep); !slices.Equal(got, []int64{4, 5, 7, 9}) {
		t.Errorf("Invalid = %v, want [4 5 7 9]", got)
	}
	reasons := map[int64]string{}
	for _, inv := range rep.Invalid {
		reasons[inv.ID] = inv.Reason
		if inv.Disabled != before[inv.ID].enabled {
			t.Errorf("row %d: Disabled = %v, but it was enabled = %v", inv.ID, inv.Disabled, before[inv.ID].enabled)
		}
		if after[inv.ID].enabled || after[inv.ID].settings != before[inv.ID].settings {
			t.Errorf("invalid row %d: enabled %v, settings %s", inv.ID, after[inv.ID].enabled, after[inv.ID].settings)
		}
	}
	for id, want := range map[int64]string{4: "Lidarr path must be an absolute path", 5: "plexIntegrationId", 7: "no API authentication",
		9: "not Plex"} {
		if !strings.Contains(reasons[id], want) {
			t.Errorf("row %d reason %q, want it to contain %q", id, reasons[id], want)
		}
	}
	// Plex rows and API keys are untouched.
	if after[1] != before[1] {
		t.Errorf("the Plex row changed: %+v", after[1])
	}
	for id := range before {
		if after[id].apiKey != before[id].apiKey {
			t.Errorf("row %d: the API key changed", id)
		}
	}
	// Every *arr row gets a webhook key, held in the map and the registry.
	if !slices.Equal(rep.WebhookKeys, []int64{2, 3, 4}) {
		t.Errorf("WebhookKeys = %v, want [2 3 4]", rep.WebhookKeys)
	}
	for id, st := range after {
		arr := id >= 2 && id <= 4
		if (st.hook != "") != arr {
			t.Errorf("row %d webhook_key %q", id, st.hook)
		}
		if !arr {
			continue
		}
		key, err := s.WebhookKey(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if who, ok := s.MatchWebhookKey(key); !ok || who.IntegrationID != id {
			t.Errorf("row %d: its key does not match (%+v, %v)", id, who, ok)
		}
		if !logging.ContainsSecret(key) {
			t.Errorf("row %d: its key is not held for redaction", id)
		}
	}

	// A second run changes nothing.
	rep2, err := s.NormalizeStored(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep2.Normalized) != 0 || len(rep2.WebhookKeys) != 0 || !slices.Equal(invalidIDs(rep2), []int64{4, 5, 7, 9}) {
		t.Errorf("second run = %+v", rep2)
	}
	for _, inv := range rep2.Invalid {
		if inv.Disabled {
			t.Errorf("second run disabled row %d again", inv.ID)
		}
	}
	if !reflect.DeepEqual(rowStates(t, d), after) {
		t.Error("the second run changed rows")
	}

	// A fixed row can be enabled again and saved.
	if _, err := s.Update(ctx, 7, Input{Name: "Maintainerr with key", URL: "http://maintainerr:1", ClearAPIKey: true, Enabled: ptr(true)}); err != nil {
		t.Fatalf("fixing the Maintainerr row: %v", err)
	}
}

// TestCrashMatrixNormalize crashes the start-up normalization at each of its points and restarts:
// before the commit nothing changed; after it, the restart finds the rows normalized, loads the
// keys, and a further run changes nothing. Either way the end state equals an uncrashed run's.
func TestCrashMatrixNormalize(t *testing.T) {
	ctx := context.Background()
	// The reference: an uncrashed run.
	dRef, krRef := seedPhase1(t)
	if _, err := NewStore(dRef, krRef).NormalizeStored(ctx); err != nil {
		t.Fatal(err)
	}
	ref := rowStates(t, dRef)

	for _, point := range []string{PointNormalizeBeforeCommit, pointBeforeHold} {
		t.Run(point, func(t *testing.T) {
			d, kr := seedPhase1(t)
			before := rowStates(t, d)
			faultinject.SetHook(faultinject.CrashAt(point, 1))
			func() {
				defer func() {
					if _, ok := recover().(faultinject.Crash); !ok {
						t.Fatalf("no crash at %s", point)
					}
				}()
				_, _ = NewStore(d, kr).NormalizeStored(ctx)
			}()
			faultinject.SetHook(nil)
			crashed := rowStates(t, d)
			if point == PointNormalizeBeforeCommit && !reflect.DeepEqual(crashed, before) {
				t.Fatal("a crash before the commit changed rows")
			}
			if point == pointBeforeHold && reflect.DeepEqual(crashed, before) {
				t.Fatal("a crash after the commit lost the changes")
			}

			// Restart: normalize, then register the secrets, as App does.
			s := NewStore(d, kr)
			rep, err := s.NormalizeStored(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.RegisterSecrets(ctx); err != nil {
				t.Fatal(err)
			}
			if point == pointBeforeHold && (len(rep.Normalized) != 0 || len(rep.WebhookKeys) != 0) {
				t.Errorf("the restart redid committed work: %+v", rep)
			}
			got := rowStates(t, d)
			for id, want := range ref {
				g := got[id]
				// Webhook keys are random: compare their presence only.
				if g.settings != want.settings || g.enabled != want.enabled || (g.hook == "") != (want.hook == "") {
					t.Errorf("row %d after the restart = %+v, want %+v", id, g, want)
				}
				if g.hook == "" {
					continue
				}
				key, err := s.WebhookKey(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if who, ok := s.MatchWebhookKey(key); !ok || who.IntegrationID != id {
					t.Errorf("row %d: its key does not match after the restart", id)
				}
			}
		})
	}
}

// TestNormalizeStoredOnAnEmptyDatabase: an upgraded Phase 1 install without *arr integrations
// gets nothing new.
func TestNormalizeStoredOnAnEmptyDatabase(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newTestStore(t)
	if _, err := s.Create(ctx, plexInput("Plex", "tok-empty-0123456789")); err != nil {
		t.Fatal(err)
	}
	rep, err := s.NormalizeStored(ctx)
	if err != nil || len(rep.Normalized)+len(rep.Invalid)+len(rep.WebhookKeys) != 0 {
		t.Fatalf("NormalizeStored = %+v, %v", rep, err)
	}
}
