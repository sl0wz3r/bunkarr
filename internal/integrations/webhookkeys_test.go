package integrations

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/config"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/logging"
)

func rawWebhookKey(t *testing.T, s *Store, id int64) string {
	t.Helper()
	var v string
	if err := s.db.Reader().QueryRow(`SELECT webhook_key FROM integrations WHERE id = ?`, id).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestWebhookKeyIsCreatedWithAnArrIntegration(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newTestStore(t)
	radarr, err := s.Create(ctx, Input{Type: TypeRadarr, Name: "Radarr", URL: "http://radarr:7878", APIKey: "radarr-api-key-0001"})
	if err != nil {
		t.Fatal(err)
	}
	plex, err := s.Create(ctx, plexInput("Plex", "tok-hooks-0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	if has, err := s.HasWebhookKey(ctx, radarr.ID); err != nil || !has {
		t.Fatalf("HasWebhookKey(radarr) = %v, %v", has, err)
	}
	if raw := rawWebhookKey(t, s, plex.ID); raw != "" {
		t.Fatalf("a Plex row got a webhook key: %q", raw)
	}

	key, err := s.WebhookKey(ctx, radarr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != WebhookKeyLen {
		t.Fatalf("key %q has length %d, want %d", key, len(key), WebhookKeyLen)
	}
	if b, err := hex.DecodeString(key); err != nil || len(b) != 16 {
		t.Fatalf("key %q is not 16 hex-encoded bytes", key)
	}
	// Sealed at rest, bound to its row, never in the API representation.
	raw := rawWebhookKey(t, s, radarr.ID)
	if !strings.HasPrefix(raw, "v1:") || strings.Contains(raw, key) {
		t.Fatalf("webhook_key column holds %q, want a sealed value", raw)
	}
	if _, err := s.kr.Open(raw, apiKeyAAD(radarr.ID)); !errors.Is(err, config.ErrKeyMismatch) {
		t.Fatalf("the webhook key opened with the API key's associated data: %v", err)
	}
	it, err := s.Get(ctx, radarr.ID)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(it)
	if strings.Contains(string(body), key) {
		t.Fatalf("the integration's JSON holds the webhook key: %s", body)
	}
	if tok, err := s.Token(ctx, radarr.ID); err != nil || tok != "radarr-api-key-0001" {
		t.Fatalf("the API key changed: %q, %v", tok, err)
	}
	if !logging.ContainsSecret("x " + key + " y") {
		t.Fatal("the webhook key is not registered for redaction")
	}
	if again, err := s.WebhookKey(ctx, radarr.ID); err != nil || again != key {
		t.Fatalf("reveal twice = %q, %v; want the same key", again, err)
	}
	if id, ok := s.MatchWebhookKey(key); !ok || id != (WebhookIdentity{IntegrationID: radarr.ID, Type: TypeRadarr}) {
		t.Fatalf("MatchWebhookKey = %+v, %v", id, ok)
	}

	// Only *arrs have webhook keys.
	var verr ValidationError
	if _, err := s.WebhookKey(ctx, plex.ID); !errors.As(err, &verr) {
		t.Fatalf("WebhookKey(plex) = %v, want a ValidationError", err)
	}
	if _, err := s.RotateWebhookKey(ctx, plex.ID); !errors.As(err, &verr) {
		t.Fatalf("RotateWebhookKey(plex) = %v, want a ValidationError", err)
	}
	if _, err := s.WebhookKey(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("WebhookKey(999) = %v", err)
	}
	if _, err := s.HasWebhookKey(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("HasWebhookKey(999) = %v", err)
	}
}

func TestRotateWebhookKey(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newTestStore(t)
	sonarr, err := s.Create(ctx, Input{Type: TypeSonarr, Name: "Sonarr", URL: "http://sonarr:8989"})
	if err != nil {
		t.Fatal(err)
	}
	lidarr, err := s.Create(ctx, Input{Type: TypeLidarr, Name: "Lidarr", URL: "http://lidarr:8686"})
	if err != nil {
		t.Fatal(err)
	}
	old, err := s.WebhookKey(ctx, sonarr.ID)
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.WebhookKey(ctx, lidarr.ID)
	if err != nil || other == old {
		t.Fatalf("two integrations share a key: %v", err)
	}
	fresh, err := s.RotateWebhookKey(ctx, sonarr.ID)
	if err != nil || fresh == old || len(fresh) != WebhookKeyLen {
		t.Fatalf("RotateWebhookKey = %q, %v", fresh, err)
	}
	// The old key stops working at once; the new one and the other integration's work.
	if _, ok := s.MatchWebhookKey(old); ok {
		t.Fatal("the rotated key still matches")
	}
	if id, ok := s.MatchWebhookKey(fresh); !ok || id.IntegrationID != sonarr.ID || id.Type != TypeSonarr {
		t.Fatalf("MatchWebhookKey(new) = %+v, %v", id, ok)
	}
	if id, ok := s.MatchWebhookKey(other); !ok || id.IntegrationID != lidarr.ID {
		t.Fatalf("MatchWebhookKey(lidarr) = %+v, %v", id, ok)
	}
	if got, err := s.WebhookKey(ctx, sonarr.ID); err != nil || got != fresh {
		t.Fatalf("reveal after rotate = %q, %v", got, err)
	}
	releaseMany(t)
	if logging.ContainsSecret(old) || !logging.ContainsSecret(fresh) {
		t.Fatal("the registry holds the rotated key, or not the new one")
	}
	for _, bad := range []string{"", "x", strings.ToUpper(fresh), fresh + "0", fresh[:len(fresh)-1]} {
		if _, ok := s.MatchWebhookKey(bad); ok {
			t.Errorf("MatchWebhookKey(%q) matched", bad)
		}
	}

	// A new store over the same database (a restart) loads the map from RegisterSecrets.
	s2 := NewStore(s.db, s.kr)
	if _, ok := s2.MatchWebhookKey(fresh); ok {
		t.Fatal("a new store matched before RegisterSecrets")
	}
	if err := s2.RegisterSecrets(ctx); err != nil {
		t.Fatal(err)
	}
	if id, ok := s2.MatchWebhookKey(fresh); !ok || id.IntegrationID != sonarr.ID {
		t.Fatalf("after RegisterSecrets: %+v, %v", id, ok)
	}

	// Deleting the integration drops its key from the map and the registry.
	if err := s.Delete(ctx, sonarr.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.MatchWebhookKey(fresh); ok {
		t.Fatal("a deleted integration's key still matches")
	}
	releaseMany(t)
	if logging.ContainsSecret(fresh) {
		t.Fatal("a deleted integration's key is still held in the redaction registry")
	}
}

// TestWebhookKeyOfARowWithoutOne: reveal generates the key of a row that has none (a row that
// start-up normalization has not reached yet).
func TestWebhookKeyOfARowWithoutOne(t *testing.T) {
	ctx := context.Background()
	s, d, _ := newTestStore(t)
	it, err := s.Create(ctx, Input{Type: TypeRadarr, Name: "Radarr", URL: "http://radarr:7878"})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE integrations SET webhook_key = '' WHERE id = ?`, it.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if has, err := s.HasWebhookKey(ctx, it.ID); err != nil || has {
		t.Fatalf("HasWebhookKey = %v, %v", has, err)
	}
	key, err := s.WebhookKey(ctx, it.ID)
	if err != nil || len(key) != WebhookKeyLen {
		t.Fatalf("WebhookKey = %q, %v", key, err)
	}
	if again, err := s.WebhookKey(ctx, it.ID); err != nil || again != key {
		t.Fatalf("second reveal = %q, %v", again, err)
	}
	if _, ok := s.MatchWebhookKey(key); !ok {
		t.Fatal("the generated key does not match")
	}
}

// TestSealedWebhookKeyIsBoundToItsRow: a sealed key copied into another row does not open there.
func TestSealedWebhookKeyIsBoundToItsRow(t *testing.T) {
	ctx := context.Background()
	s, d, _ := newTestStore(t)
	a, err := s.Create(ctx, Input{Type: TypeRadarr, Name: "A", URL: "http://a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Create(ctx, Input{Type: TypeRadarr, Name: "B", URL: "http://b"})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE integrations SET webhook_key = (SELECT webhook_key FROM integrations WHERE id = ?) WHERE id = ?`, a.ID, b.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WebhookKey(ctx, b.ID); !errors.Is(err, config.ErrKeyMismatch) {
		t.Fatalf("WebhookKey of the copied value = %v, want ErrKeyMismatch", err)
	}
	s2 := NewStore(d, s.kr)
	if err := s2.RegisterSecrets(ctx); !errors.Is(err, config.ErrKeyMismatch) {
		t.Fatalf("RegisterSecrets = %v, want it to report the key that does not open", err)
	}
	keyA, err := s.WebhookKey(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := s2.MatchWebhookKey(keyA); !ok || id.IntegrationID != a.ID {
		t.Fatalf("the key that opens was not loaded: %+v, %v", id, ok)
	}
}

// TestMatchWebhookKeyIsSafeDuringRotation runs matches and rotations concurrently (go test -race).
func TestMatchWebhookKeyIsSafeDuringRotation(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newTestStore(t)
	it, err := s.Create(ctx, Input{Type: TypeRadarr, Name: "Radarr", URL: "http://radarr:7878"})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				s.MatchWebhookKey(strings.Repeat("a", WebhookKeyLen))
			}
		}
	}()
	for range 20 {
		if _, err := s.RotateWebhookKey(ctx, it.ID); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
}

// TestRotateCrashAfterCommit: a crash after the new key is committed and before the map is
// updated loses nothing: the restart loads the committed key, and the old one is gone.
func TestRotateCrashAfterCommit(t *testing.T) {
	ctx := context.Background()
	s, d, kr := newTestStore(t)
	it, err := s.Create(ctx, Input{Type: TypeRadarr, Name: "Radarr", URL: "http://radarr:7878"})
	if err != nil {
		t.Fatal(err)
	}
	old, err := s.WebhookKey(ctx, it.ID)
	if err != nil {
		t.Fatal(err)
	}
	faultinject.SetHook(faultinject.CrashAt(pointBeforeHold, 1))
	func() {
		defer func() {
			if _, ok := recover().(faultinject.Crash); !ok {
				t.Fatal("no crash at " + pointBeforeHold)
			}
		}()
		_, _ = s.RotateWebhookKey(ctx, it.ID)
	}()
	faultinject.SetHook(nil)

	restarted := NewStore(d, kr)
	if err := restarted.RegisterSecrets(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := restarted.MatchWebhookKey(old); ok {
		t.Fatal("the old key survived a committed rotation")
	}
	current, err := restarted.WebhookKey(ctx, it.ID)
	if err != nil || current == old {
		t.Fatalf("key after the crash = %q, %v", current, err)
	}
	if id, ok := restarted.MatchWebhookKey(current); !ok || id.IntegrationID != it.ID {
		t.Fatalf("the committed key does not match after the restart: %+v, %v", id, ok)
	}
}

// TestRotateOrdersItsRegistryUpdate: a rotation holds registryMu from its write until the map and
// the registry hold the new key, so a delete cannot land in between and leave the deleted
// integration's key in the map.
func TestRotateOrdersItsRegistryUpdate(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newTestStore(t)
	it, err := s.Create(ctx, Input{Type: TypeRadarr, Name: "Radarr", URL: "http://radarr:7878"})
	if err != nil {
		t.Fatal(err)
	}
	var key string
	rotErr, delErr := raceAtHold(
		func() (err error) { key, err = s.RotateWebhookKey(ctx, it.ID); return err },
		func() error { return s.Delete(ctx, it.ID) },
	)
	if rotErr != nil || delErr != nil {
		t.Fatalf("rotate: %v, delete: %v", rotErr, delErr)
	}
	if _, ok := s.MatchWebhookKey(key); ok {
		t.Fatal("the key of the deleted integration still matches")
	}
	releaseMany(t)
	if logging.ContainsSecret(key) {
		t.Fatal("the key of the deleted integration is still held in the redaction registry")
	}
}

// TestCreateCrashAfterCommit: a crash after an *arr integration and its webhook key are committed,
// before the key map is updated, loses nothing: the restart loads the key.
func TestCreateCrashAfterCommit(t *testing.T) {
	ctx := context.Background()
	s, d, kr := newTestStore(t)
	faultinject.SetHook(faultinject.CrashAt(pointBeforeHold, 1))
	func() {
		defer func() {
			if _, ok := recover().(faultinject.Crash); !ok {
				t.Fatal("no crash at " + pointBeforeHold)
			}
		}()
		_, _ = s.Create(ctx, Input{Type: TypeLidarr, Name: "Lidarr", URL: "http://lidarr:8686"})
	}()
	faultinject.SetHook(nil)
	restarted := NewStore(d, kr)
	if _, err := restarted.NormalizeStored(ctx); err != nil {
		t.Fatal(err)
	}
	if err := restarted.RegisterSecrets(ctx); err != nil {
		t.Fatal(err)
	}
	list, err := restarted.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %+v, %v", list, err)
	}
	key, err := restarted.WebhookKey(ctx, list[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if who, ok := restarted.MatchWebhookKey(key); !ok || who.IntegrationID != list[0].ID || who.Type != TypeLidarr {
		t.Fatalf("the committed key does not match after the restart: %+v, %v", who, ok)
	}
}
