package integrations

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/logging"
)

// Webhook keys (design D7, S8, S12). Each Sonarr, Radarr and Lidarr integration has its own key:
// 16 random bytes, hex-encoded, generated with the integration (and at start-up for rows created
// before migration 0003). It is sealed in integrations.webhook_key with the associated data
// "integration:<id>:webhookKey", held in the redaction registry (owner
// "integration:<id>:webhookKey"), revealed only by WebhookKey (POST
// /integrations/{id}/webhook/key) and accepted only by the webhook routes, which look it up with
// MatchWebhookKey in an in-memory map: no I/O before the credential is checked (S13).

// webhookKeyBytes is the number of random bytes of a webhook key.
const webhookKeyBytes = 16

// WebhookKeyLen is the length of a webhook key: webhookKeyBytes, hex-encoded.
const WebhookKeyLen = 2 * webhookKeyBytes

func webhookKeyAAD(id int64) string { return fmt.Sprintf("integration:%d:webhookKey", id) }

// webhookOwner names row id's webhook key in the redaction registry.
func webhookOwner(id int64) string { return fmt.Sprintf("integration:%d:webhookKey", id) }

// newWebhookKey returns a fresh random key.
func newWebhookKey() (string, error) {
	b := make([]byte, webhookKeyBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate a webhook key: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// WebhookIdentity is the integration a webhook key belongs to.
type WebhookIdentity struct {
	IntegrationID int64
	Type          Type
}

// webhookKeys is the in-memory map of integration id → webhook key that the webhook routes check
// a credential against.
type webhookKeys struct {
	mu sync.RWMutex
	m  map[int64]webhookEntry
}

type webhookEntry struct {
	typ Type
	key []byte
}

func newWebhookKeys() *webhookKeys { return &webhookKeys{m: map[int64]webhookEntry{}} }

func (w *webhookKeys) set(id int64, typ Type, key string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.m[id] = webhookEntry{typ: typ, key: []byte(key)}
}

func (w *webhookKeys) remove(id int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.m, id)
}

func (w *webhookKeys) replace(m map[int64]webhookEntry) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.m = m
}

// match compares key with every stored key in constant time (each comparison takes the same time
// whatever the bytes, and every entry is compared), and returns the one it equals.
func (w *webhookKeys) match(key string) (WebhookIdentity, bool) {
	if len(key) != WebhookKeyLen {
		return WebhookIdentity{}, false
	}
	k := []byte(key)
	w.mu.RLock()
	defer w.mu.RUnlock()
	var (
		found WebhookIdentity
		ok    bool
	)
	for id, e := range w.m {
		if subtle.ConstantTimeCompare(k, e.key) == 1 {
			found, ok = WebhookIdentity{IntegrationID: id, Type: e.typ}, true
		}
	}
	return found, ok
}

// MatchWebhookKey returns the Sonarr, Radarr or Lidarr integration whose webhook key is key. It
// reads only the in-memory map (loaded by RegisterSecrets and NormalizeStored, updated by every
// write), so a webhook request is authenticated before any I/O (S13). Whether the integration is
// enabled, and of the route's application, is for the caller to check.
func (s *Store) MatchWebhookKey(key string) (WebhookIdentity, bool) {
	return s.hooks.match(key)
}

// sealWebhookKey seals a webhook key for row id.
func (s *Store) sealWebhookKey(id int64, key string) (string, error) {
	if s.kr == nil {
		return "", errors.New("integrations: no keyring configured")
	}
	sealed, err := s.kr.Seal(key, webhookKeyAAD(id))
	if err != nil {
		return "", fmt.Errorf("seal the webhook key of integration %d: %w", id, err)
	}
	return sealed, nil
}

// openWebhookKey opens the sealed webhook key of row id.
func (s *Store) openWebhookKey(id int64, sealed string) (string, error) {
	if s.kr == nil {
		return "", errors.New("integrations: no keyring configured")
	}
	key, err := s.kr.Open(sealed, webhookKeyAAD(id))
	if err != nil {
		return "", fmt.Errorf("webhook key of integration %d: %w", id, err)
	}
	return key, nil
}

// storeNewWebhookKey generates a key for row id and stores it sealed, inside tx.
func (s *Store) storeNewWebhookKey(ctx context.Context, tx *sql.Tx, id int64) (string, error) {
	key, err := newWebhookKey()
	if err != nil {
		return "", err
	}
	sealed, err := s.sealWebhookKey(id, key)
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE integrations SET webhook_key = ? WHERE id = ?`, sealed, id); err != nil {
		return "", fmt.Errorf("store the webhook key of integration %d: %w", id, err)
	}
	return key, nil
}

// holdWebhookKey puts row id's webhook key in the redaction registry and the key map. The caller
// holds registryMu.
func (s *Store) holdWebhookKey(id int64, typ Type, key string) {
	logging.SetSecrets(webhookOwner(id), key)
	s.hooks.set(id, typ, key)
}

// releaseWebhookKey drops row id's webhook key from the redaction registry and the key map. The
// caller holds registryMu.
func (s *Store) releaseWebhookKey(id int64) {
	logging.SetSecrets(webhookOwner(id))
	s.hooks.remove(id)
}

// arrRow reads the type and sealed webhook key of an *arr integration: ErrNotFound for an
// unknown id, a ValidationError for another type.
func arrRow(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id int64) (Type, string, error) {
	var typ, sealed string
	err := q.QueryRowContext(ctx, `SELECT type, webhook_key FROM integrations WHERE id = ?`, id).Scan(&typ, &sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("read the webhook key of integration %d: %w", id, err)
	}
	if !Type(typ).IsArr() {
		return "", "", ValidationError(fmt.Sprintf("%s integrations have no webhook key: only Sonarr, Radarr and Lidarr send webhooks", Type(typ).AppName()))
	}
	return Type(typ), sealed, nil
}

// HasWebhookKey reports whether the *arr integration id has a webhook key.
func (s *Store) HasWebhookKey(ctx context.Context, id int64) (bool, error) {
	_, sealed, err := arrRow(ctx, s.db.Reader(), id)
	return sealed != "", err
}

// WebhookKey reveals the webhook key of the *arr integration id (POST
// /integrations/{id}/webhook/key with rotate false, the only response that contains it). A row
// without a key gets one first. ErrNotFound for an unknown id, a ValidationError for another type,
// and an error wrapping config.ErrKeyMismatch when the stored key does not open.
func (s *Store) WebhookKey(ctx context.Context, id int64) (string, error) {
	_, sealed, err := arrRow(ctx, s.db.Reader(), id)
	if err != nil {
		return "", err
	}
	if sealed == "" {
		return s.rotateWebhookKey(ctx, id, false)
	}
	return s.openWebhookKey(id, sealed)
}

// RotateWebhookKey replaces the webhook key of the *arr integration id with a new one and returns
// it. The old key stops working at once: the key map is updated before RotateWebhookKey returns.
func (s *Store) RotateWebhookKey(ctx context.Context, id int64) (string, error) {
	return s.rotateWebhookKey(ctx, id, true)
}

// rotateWebhookKey stores a new key for id; with always false it keeps a key another request
// stored meanwhile (WebhookKey of a row without one).
func (s *Store) rotateWebhookKey(ctx context.Context, id int64, always bool) (string, error) {
	registryMu.Lock()
	defer registryMu.Unlock()
	var (
		key  string
		typ  Type
		kept bool
	)
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		t, sealed, err := arrRow(ctx, tx, id)
		if err != nil {
			return err
		}
		typ = t
		if sealed != "" && !always {
			key, err = s.openWebhookKey(id, sealed)
			kept = true
			return err
		}
		key, err = s.storeNewWebhookKey(ctx, tx, id)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE integrations SET updated_at = ? WHERE id = ?`, db.FormatTime(s.now()), id)
		return err
	})
	if err != nil {
		return "", err
	}
	if !kept {
		faultinject.Point(pointBeforeHold)
		s.holdWebhookKey(id, typ, key)
	}
	return key, nil
}

// loadWebhookKeys unseals every stored webhook key, holds each in the redaction registry and
// replaces the key map with them. Keys that do not open are reported in the returned error (the
// others are still loaded). The caller holds registryMu.
func (s *Store) loadWebhookKeys(ctx context.Context) error {
	type row struct {
		id     int64
		typ    Type
		name   string
		sealed string
	}
	rs, err := s.db.Reader().QueryContext(ctx, `SELECT id, type, name, webhook_key FROM integrations WHERE webhook_key <> '' ORDER BY id`)
	if err != nil {
		return fmt.Errorf("read webhook keys: %w", err)
	}
	var list []row
	for rs.Next() {
		var r row
		var typ string
		if err := rs.Scan(&r.id, &typ, &r.name, &r.sealed); err != nil {
			_ = rs.Close()
			return fmt.Errorf("read webhook keys: %w", err)
		}
		r.typ = Type(typ)
		list = append(list, r)
	}
	if err := rs.Err(); err != nil {
		_ = rs.Close()
		return fmt.Errorf("read webhook keys: %w", err)
	}
	if err := rs.Close(); err != nil {
		return fmt.Errorf("read webhook keys: %w", err)
	}
	faultinject.Point(pointBeforeHold)
	m := make(map[int64]webhookEntry, len(list))
	var errs []error
	for _, r := range list {
		if !r.typ.IsArr() {
			continue
		}
		key, err := s.openWebhookKey(r.id, r.sealed)
		if err != nil {
			errs = append(errs, fmt.Errorf("integration %q: %w", r.name, err))
			continue
		}
		logging.SetSecrets(webhookOwner(r.id), key)
		m[r.id] = webhookEntry{typ: r.typ, key: []byte(key)}
	}
	s.hooks.replace(m)
	return errors.Join(errs...)
}
