// Package integrations stores the external services Bunkarr talks to (Plex, Sonarr, Radarr,
// Lidarr, Tautulli, Seerr and Maintainerr) in the integrations table.
//
// Each integration has a URL, an optional API key (for Plex, the X-Plex-Token) and type-specific
// settings (PlexSettings, ArrSettings, TautulliSettings, SeerrSettings, MaintainerrSettings),
// validated and normalized on every write (docs/design/phase2-3.md §4.2); NormalizeStored brings
// rows saved before Phase 2 to those rules at start-up. Sonarr, Radarr and Lidarr integrations
// also have a webhook key (webhookkeys.go). The key is sealed with the keyring (ADR 0003) using the
// associated data "integration:<id>:apiKey", so a sealed value copied into another row does not
// open. It never leaves the store in any form other than through TokenFor (and Token), and the
// API only sees HasAPIKey (design S8). The key is bound to the stored URL: TokenFor returns it
// only together with that URL. The token each row stores is held in the redaction registry
// (logging.SetSecrets, owner "integration:<id>") so its value is redacted from logs, and released
// when it is replaced, cleared or deleted.
package integrations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/sl0wz3r/bunkarr/internal/config"
	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/logging"
)

// Type is the kind of service an integration talks to.
type Type string

// Integration types. The list matches the CHECK constraint of integrations.type.
const (
	TypePlex        Type = "plex"
	TypeSonarr      Type = "sonarr"
	TypeRadarr      Type = "radarr"
	TypeLidarr      Type = "lidarr"
	TypeTautulli    Type = "tautulli"
	TypeSeerr       Type = "seerr"
	TypeMaintainerr Type = "maintainerr"
)

// Valid reports whether t is a known integration type.
func (t Type) Valid() bool {
	switch t {
	case TypePlex, TypeSonarr, TypeRadarr, TypeLidarr, TypeTautulli, TypeSeerr, TypeMaintainerr:
		return true
	}
	return false
}

// Limits on user input.
const (
	// MaxNameLen is the longest accepted name, in characters.
	MaxNameLen = 64
	// MaxURLLen is the longest accepted URL, in bytes.
	MaxURLLen = 2048
	// MaxAPIKeyLen is the longest accepted API key, in bytes.
	MaxAPIKeyLen = 1024
	// MaxSettingsLen is the largest accepted settings document, in bytes.
	MaxSettingsLen = 64 << 10
)

// ErrNotFound means no integration has the requested id.
var ErrNotFound = errors.New("integration not found")

// ErrURLChanged means TokenFor was refused: the integration's stored URL is not the one the
// caller has (it changed after the caller read the integration).
var ErrURLChanged = errors.New("the integration's URL changed")

// ValidationError is a user-facing input error. Its text never contains the URL or API key the
// user sent (either may hold a credential).
type ValidationError string

// Error implements error.
func (e ValidationError) Error() string { return string(e) }

// Integration is a stored integration as the API returns it: the API key is reduced to HasAPIKey.
type Integration struct {
	ID        int64     `json:"id"`
	Type      Type      `json:"type"`
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	Enabled   bool      `json:"enabled"`
	HasAPIKey bool      `json:"hasApiKey"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	// Settings is the type-specific settings object (PlexSettings for Plex), normalized.
	Settings json.RawMessage `json:"settings"`
}

// PlexSettings decodes the settings of a Plex integration.
func (i Integration) PlexSettings() (PlexSettings, error) {
	if i.Type != TypePlex {
		return PlexSettings{}, fmt.Errorf("integration %d is a %s integration, not plex", i.ID, i.Type)
	}
	return ParsePlexSettings(i.Settings)
}

// Input is what Create and Update accept; the API decodes request bodies into it.
//
// Create: Type, Name and URL are required; Enabled defaults to true; Settings defaults to {} (for
// Plex, the zero PlexSettings).
//
// Update (PUT semantics): Name and URL are required and replace the stored values. Type may be
// empty or equal to the stored type (it cannot change). A nil Enabled or empty Settings keeps the
// stored value. An empty APIKey keeps the stored key; ClearAPIKey removes it. The stored key is
// bound to the stored URL: an update that changes the URL (compared after NormalizeURL) of an
// integration that has a key must send the key again (or ClearAPIKey), otherwise it is refused
// with a ValidationError (whose text asks the user to enter the token again), so the key is never
// sent to a host picked by a caller who does not know it (design S8).
type Input struct {
	Type     Type            `json:"type"`
	Name     string          `json:"name"`
	URL      string          `json:"url"`
	Enabled  *bool           `json:"enabled"`
	APIKey   string          `json:"apiKey"`
	Settings json.RawMessage `json:"settings"`
	// ClearAPIKey removes the stored API key (Update only; not together with APIKey).
	ClearAPIKey bool `json:"clearApiKey"`
}

// Store reads and writes the integrations table.
type Store struct {
	db  *db.DB
	kr  *config.Keyring
	now func() time.Time
	// hooks is the webhook key map (webhookkeys.go).
	hooks *webhookKeys
}

// NewStore returns a store over d that seals API keys and webhook keys with kr.
func NewStore(d *db.DB, kr *config.Keyring) *Store {
	return &Store{db: d, kr: kr, now: time.Now, hooks: newWebhookKeys()}
}

const selectColumns = `SELECT id, type, name, url, api_key <> '', enabled, settings, created_at, updated_at FROM integrations`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanIntegration(r rowScanner) (Integration, error) {
	var (
		it                 Integration
		typ, settings      string
		created, updated   string
		hasKey, enabledInt bool
	)
	if err := r.Scan(&it.ID, &typ, &it.Name, &it.URL, &hasKey, &enabledInt, &settings, &created, &updated); err != nil {
		return Integration{}, err
	}
	it.Type = Type(typ)
	it.HasAPIKey = hasKey
	it.Enabled = enabledInt
	it.Settings = json.RawMessage(settings)
	var err error
	if it.CreatedAt, err = db.ParseTime(created); err != nil {
		return Integration{}, fmt.Errorf("integration %d created_at: %w", it.ID, err)
	}
	if it.UpdatedAt, err = db.ParseTime(updated); err != nil {
		return Integration{}, fmt.Errorf("integration %d updated_at: %w", it.ID, err)
	}
	return it, nil
}

// List returns every integration, ordered by name.
func (s *Store) List(ctx context.Context) ([]Integration, error) {
	rows, err := s.db.Reader().QueryContext(ctx, selectColumns+` ORDER BY name COLLATE NOCASE, id`)
	if err != nil {
		return nil, fmt.Errorf("list integrations: %w", err)
	}
	defer rows.Close()
	out := []Integration{}
	for rows.Next() {
		it, err := scanIntegration(rows)
		if err != nil {
			return nil, fmt.Errorf("list integrations: %w", err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list integrations: %w", err)
	}
	return out, nil
}

// Get returns one integration, or ErrNotFound.
func (s *Store) Get(ctx context.Context, id int64) (Integration, error) {
	it, err := scanIntegration(s.db.Reader().QueryRowContext(ctx, selectColumns+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Integration{}, ErrNotFound
	}
	if err != nil {
		return Integration{}, fmt.Errorf("get integration %d: %w", id, err)
	}
	return it, nil
}

func getTx(ctx context.Context, tx *sql.Tx, id int64) (Integration, error) {
	it, err := scanIntegration(tx.QueryRowContext(ctx, selectColumns+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Integration{}, ErrNotFound
	}
	if err != nil {
		return Integration{}, fmt.Errorf("get integration %d: %w", id, err)
	}
	return it, nil
}

// registryMu orders the redaction registry's updates for the stored tokens: Create, Update and
// Delete hold it from their write until they have updated the registry, and RegisterSecrets from
// its read, so an update cannot land after that of a later write and replace the stored token
// there (the registry then holds the replaced token, and the stored one is dropped once enough
// values are released). Reads (TokenFor, Token) never update the registry, for the same reason:
// what they read may have been replaced by the time they would. The registry is process-wide, so
// the lock is too.
var registryMu sync.Mutex

// pointBeforeHold is where a write (or the start-up registration) has committed (read) what it
// holds in the redaction registry and has not updated the registry yet.
const pointBeforeHold = "integrations.beforeHold"

// secretOwner names row id's token in the redaction registry (logging.SetSecrets).
func secretOwner(id int64) string {
	return fmt.Sprintf("integration:%d", id)
}

// apiKeyAAD is the associated data that binds a sealed API key to its row.
func apiKeyAAD(id int64) string {
	return fmt.Sprintf("integration:%d:apiKey", id)
}

func (s *Store) seal(id int64, token string) (string, error) {
	if s.kr == nil {
		return "", errors.New("integrations: no keyring configured")
	}
	sealed, err := s.kr.Seal(token, apiKeyAAD(id))
	if err != nil {
		return "", fmt.Errorf("seal API key of integration %d: %w", id, err)
	}
	return sealed, nil
}

func (s *Store) open(id int64, sealed string) (string, error) {
	if s.kr == nil {
		return "", errors.New("integrations: no keyring configured")
	}
	token, err := s.kr.Open(sealed, apiKeyAAD(id))
	if err != nil {
		return "", fmt.Errorf("API key of integration %d: %w", id, err)
	}
	return token, nil
}

// Create validates in, inserts the integration and seals its API key in the same transaction.
func (s *Store) Create(ctx context.Context, in Input) (Integration, error) {
	if !in.Type.Valid() {
		return Integration{}, ValidationError("type must be one of plex, sonarr, radarr, lidarr, tautulli, seerr, maintainerr")
	}
	if in.ClearAPIKey {
		return Integration{}, ValidationError("clearApiKey is only valid when updating an integration")
	}
	name, err := normalizeName(in.Name)
	if err != nil {
		return Integration{}, err
	}
	u, err := NormalizeURL(in.URL)
	if err != nil {
		return Integration{}, err
	}
	token, err := normalizeAPIKey(in.APIKey)
	if err != nil {
		return Integration{}, err
	}
	settings, err := normalizeSettings(in.Type, in.Settings)
	if err != nil {
		return Integration{}, err
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}

	var (
		it         Integration
		webhookKey string
	)
	registryMu.Lock()
	defer registryMu.Unlock()
	err = s.db.Write(ctx, func(tx *sql.Tx) error {
		if err := checkLinks(ctx, tx, in.Type, settings, token != ""); err != nil {
			return err
		}
		now := db.FormatTime(s.now())
		res, err := tx.ExecContext(ctx, `INSERT INTO integrations (type, name, url, api_key, enabled, settings, created_at, updated_at)
			VALUES (?, ?, ?, '', ?, ?, ?, ?)`, string(in.Type), name, u, enabled, settings, now, now)
		if err != nil {
			return mapWriteError("create integration", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("create integration: %w", err)
		}
		if in.Type.IsArr() {
			if webhookKey, err = s.storeNewWebhookKey(ctx, tx, id); err != nil {
				return err
			}
		}
		if token != "" {
			sealed, err := s.seal(id, token)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE integrations SET api_key = ? WHERE id = ?`, sealed, id); err != nil {
				return fmt.Errorf("store API key of integration %d: %w", id, err)
			}
		}
		it, err = getTx(ctx, tx, id)
		return err
	})
	if err != nil {
		return Integration{}, err
	}
	faultinject.Point(pointBeforeHold)
	logging.SetSecrets(secretOwner(it.ID), token)
	if webhookKey != "" {
		s.holdWebhookKey(it.ID, it.Type, webhookKey)
	}
	return it, nil
}

// Update validates in and replaces the integration's fields (see Input for what is kept).
func (s *Store) Update(ctx context.Context, id int64, in Input) (Integration, error) {
	if in.ClearAPIKey && strings.TrimSpace(in.APIKey) != "" {
		return Integration{}, ValidationError("apiKey and clearApiKey cannot be used together")
	}
	name, err := normalizeName(in.Name)
	if err != nil {
		return Integration{}, err
	}
	u, err := NormalizeURL(in.URL)
	if err != nil {
		return Integration{}, err
	}
	token, err := normalizeAPIKey(in.APIKey)
	if err != nil {
		return Integration{}, err
	}

	var it Integration
	registryMu.Lock()
	defer registryMu.Unlock()
	err = s.db.Write(ctx, func(tx *sql.Tx) error {
		cur, err := getTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if in.Type != "" && in.Type != cur.Type {
			return ValidationError("the type of an integration cannot be changed")
		}
		if u != cur.URL && cur.HasAPIKey && token == "" && !in.ClearAPIKey {
			return ValidationError("the URL changed, so enter the token again: a saved token is only sent to the URL it " +
				"was saved with (API clients: send apiKey, or clearApiKey to remove the token)")
		}
		settings := string(cur.Settings)
		if !settingsAbsent(in.Settings) {
			if settings, err = normalizeSettings(cur.Type, in.Settings); err != nil {
				return err
			}
		}
		if err := checkLinks(ctx, tx, cur.Type, settings, token != "" || (cur.HasAPIKey && !in.ClearAPIKey)); err != nil {
			return err
		}
		enabled := cur.Enabled
		if in.Enabled != nil {
			enabled = *in.Enabled
		}
		now := db.FormatTime(s.now())
		if _, err := tx.ExecContext(ctx, `UPDATE integrations SET name = ?, url = ?, enabled = ?, settings = ?, updated_at = ? WHERE id = ?`,
			name, u, enabled, settings, now, id); err != nil {
			return mapWriteError(fmt.Sprintf("update integration %d", id), err)
		}
		switch {
		case token != "":
			sealed, err := s.seal(id, token)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE integrations SET api_key = ? WHERE id = ?`, sealed, id); err != nil {
				return fmt.Errorf("store API key of integration %d: %w", id, err)
			}
		case in.ClearAPIKey:
			if _, err := tx.ExecContext(ctx, `UPDATE integrations SET api_key = '' WHERE id = ?`, id); err != nil {
				return fmt.Errorf("clear API key of integration %d: %w", id, err)
			}
		}
		it, err = getTx(ctx, tx, id)
		return err
	})
	if err != nil {
		return Integration{}, err
	}
	faultinject.Point(pointBeforeHold)
	switch {
	case token != "":
		logging.SetSecrets(secretOwner(id), token)
	case in.ClearAPIKey:
		logging.SetSecrets(secretOwner(id))
	}
	return it, nil
}

// Delete removes an integration, or returns ErrNotFound. Sources and snapshots that refer to it
// keep their rows with the reference cleared (ON DELETE SET NULL). Its token is released from the
// redaction registry.
func (s *Store) Delete(ctx context.Context, id int64) error {
	registryMu.Lock()
	defer registryMu.Unlock()
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM integrations WHERE id = ?`, id)
		if err != nil {
			return fmt.Errorf("delete integration %d: %w", id, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("delete integration %d: %w", id, err)
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		return err
	}
	faultinject.Point(pointBeforeHold)
	logging.SetSecrets(secretOwner(id))
	s.releaseWebhookKey(id)
	return nil
}

// TokenFor returns the integration's API key, unsealed ("" when none is stored), only while its
// stored URL is url (the URL as the store returned it, Integration.URL). The URL and the key are
// read from the same row in one query, so a caller that read the integration before its URL
// changed gets ErrURLChanged instead of the key saved for the new URL: the key only goes to the
// URL it was saved with (design S8). Use it whenever the key is sent to the integration.
//
// The stored key is already held in the redaction registry (by RegisterSecrets or the write that
// stored it); reading it does not change the registry. TokenFor returns ErrNotFound for an unknown
// id and an error wrapping config.ErrKeyMismatch when the stored value does not open (wrong
// bunkarr.key, or a sealed value that belongs to another row).
func (s *Store) TokenFor(ctx context.Context, id int64, url string) (string, error) {
	stored, sealed, err := s.readKey(ctx, id)
	if err != nil {
		return "", err
	}
	if stored != url {
		return "", ErrURLChanged
	}
	return s.unseal(id, sealed)
}

// Token is TokenFor without the URL check. It must not be used to send the key to a URL read in
// another query (the URL may change in between): use TokenFor.
func (s *Store) Token(ctx context.Context, id int64) (string, error) {
	_, sealed, err := s.readKey(ctx, id)
	if err != nil {
		return "", err
	}
	return s.unseal(id, sealed)
}

// readKey returns the stored URL and sealed API key of integration id, from one row.
func (s *Store) readKey(ctx context.Context, id int64) (url, sealed string, err error) {
	err = s.db.Reader().QueryRowContext(ctx, `SELECT url, api_key FROM integrations WHERE id = ?`, id).Scan(&url, &sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("read API key of integration %d: %w", id, err)
	}
	return url, sealed, nil
}

// unseal opens the sealed API key of integration id ("" when none is stored). It leaves the
// redaction registry alone: the key it read may have been replaced since (see registryMu).
func (s *Store) unseal(id int64, sealed string) (string, error) {
	if sealed == "" {
		return "", nil
	}
	return s.open(id, sealed)
}

// RegisterSecrets unseals every stored API key and webhook key and holds it in the redaction
// registry (logging.SetSecrets), so the values are redacted from logs from start-up on; it also
// loads the webhook key map (MatchWebhookKey). Keys that do not open are reported in the returned
// error (all others are still registered).
func (s *Store) RegisterSecrets(ctx context.Context) error {
	type row struct {
		id     int64
		name   string
		sealed string
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	rs, err := s.db.Reader().QueryContext(ctx, `SELECT id, name, api_key FROM integrations WHERE api_key <> '' ORDER BY id`)
	if err != nil {
		return fmt.Errorf("read integration API keys: %w", err)
	}
	var list []row
	for rs.Next() {
		var r row
		if err := rs.Scan(&r.id, &r.name, &r.sealed); err != nil {
			_ = rs.Close()
			return fmt.Errorf("read integration API keys: %w", err)
		}
		list = append(list, r)
	}
	if err := rs.Err(); err != nil {
		_ = rs.Close()
		return fmt.Errorf("read integration API keys: %w", err)
	}
	if err := rs.Close(); err != nil {
		return fmt.Errorf("read integration API keys: %w", err)
	}
	faultinject.Point(pointBeforeHold)
	var errs []error
	for _, r := range list {
		token, err := s.open(r.id, r.sealed)
		if err != nil {
			errs = append(errs, fmt.Errorf("integration %q: %w", r.name, err))
			continue
		}
		logging.SetSecrets(secretOwner(r.id), token)
	}
	if err := s.loadWebhookKeys(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// mapWriteError turns the UNIQUE violation on integrations.name into a ValidationError.
func mapWriteError(op string, err error) error {
	var serr *sqlite.Error
	if errors.As(err, &serr) && serr.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE {
		return ValidationError("name already used")
	}
	return fmt.Errorf("%s: %w", op, err)
}

func normalizeName(name string) (string, error) {
	name = strings.TrimSpace(name)
	n := utf8.RuneCountInString(name)
	if n < 1 || n > MaxNameLen {
		return "", ValidationError(fmt.Sprintf("name must be 1 to %d characters", MaxNameLen))
	}
	if !utf8.ValidString(name) || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "", ValidationError("name must not contain control characters")
	}
	return name, nil
}

func normalizeAPIKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if len(key) > MaxAPIKeyLen {
		return "", ValidationError(fmt.Sprintf("apiKey must be at most %d characters", MaxAPIKeyLen))
	}
	if !utf8.ValidString(key) || strings.IndexFunc(key, unicode.IsControl) >= 0 {
		return "", ValidationError("apiKey must not contain control characters")
	}
	return key, nil
}

func settingsAbsent(raw json.RawMessage) bool {
	t := strings.TrimSpace(string(raw))
	return t == "" || t == "null"
}

// normalizeSettings validates the settings document for typ and returns its stored form.
func normalizeSettings(typ Type, raw json.RawMessage) (string, error) {
	if len(raw) > MaxSettingsLen {
		return "", ValidationError(fmt.Sprintf("settings must be at most %d bytes", MaxSettingsLen))
	}
	if typ == TypePlex {
		ps, err := ParsePlexSettings(raw)
		if err != nil {
			return "", err
		}
		if err := ps.Validate(); err != nil {
			return "", err
		}
		b, err := json.Marshal(ps)
		if err != nil {
			return "", fmt.Errorf("encode plex settings: %w", err)
		}
		return string(b), nil
	}
	if t := strings.TrimSpace(string(raw)); t != "" && t != "null" && !strings.HasPrefix(t, "{") {
		return "", ValidationError("settings must be a JSON object")
	}
	return normalizeOtherSettings(typ, raw)
}
