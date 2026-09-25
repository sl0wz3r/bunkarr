package config

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
)

// Setting keys.
const (
	// KeyAPIKey is the API key (secret).
	KeyAPIKey = "auth.api_key"
	// KeyAuthRequired is "enabled" or "disabled_for_local_addresses".
	KeyAuthRequired = "auth.required"
)

// Settings is the key/value settings store. Secret values are sealed with the Keyring and never
// leave this type in encrypted form.
type Settings struct {
	db *db.DB
	kr *Keyring
}

// NewSettings returns a store over d.
func NewSettings(d *db.DB, kr *Keyring) *Settings {
	return &Settings{db: d, kr: kr}
}

// Get returns a plain setting. ok is false when it is not set. Reading a secret with Get fails.
func (s *Settings) Get(ctx context.Context, key string) (value string, ok bool, err error) {
	v, enc, ok, err := s.read(ctx, key)
	if err != nil || !ok {
		return "", ok, err
	}
	if enc {
		return "", false, fmt.Errorf("setting %s is a secret: use GetSecret", key)
	}
	return v, true, nil
}

// GetSecret returns a secret setting, decrypted.
func (s *Settings) GetSecret(ctx context.Context, key string) (value string, ok bool, err error) {
	v, enc, ok, err := s.read(ctx, key)
	if err != nil || !ok {
		return "", ok, err
	}
	if !enc {
		return "", false, fmt.Errorf("setting %s is not stored encrypted", key)
	}
	plain, err := s.kr.Open(v, "setting:"+key)
	if err != nil {
		return "", false, fmt.Errorf("setting %s: %w", key, err)
	}
	return plain, true, nil
}

// Set stores a plain setting.
func (s *Settings) Set(ctx context.Context, key, value string) error {
	return s.write(ctx, key, value, false)
}

// SetSecret seals and stores a secret setting.
func (s *Settings) SetSecret(ctx context.Context, key, value string) error {
	sealed, err := s.kr.Seal(value, "setting:"+key)
	if err != nil {
		return fmt.Errorf("seal setting %s: %w", key, err)
	}
	return s.write(ctx, key, sealed, true)
}

func (s *Settings) read(ctx context.Context, key string) (value string, encrypted, ok bool, err error) {
	err = s.db.Reader().QueryRowContext(ctx, `SELECT value, encrypted FROM settings WHERE key = ?`, key).Scan(&value, &encrypted)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, false, nil
	}
	if err != nil {
		return "", false, false, fmt.Errorf("read setting %s: %w", key, err)
	}
	return value, encrypted, true, nil
}

func (s *Settings) write(ctx context.Context, key, value string, encrypted bool) error {
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value, encrypted, updated_at) VALUES (?, ?, ?, ?)
			ON CONFLICT (key) DO UPDATE SET value = excluded.value, encrypted = excluded.encrypted, updated_at = excluded.updated_at`,
			key, value, encrypted, db.FormatTime(time.Now()))
		if err != nil {
			return fmt.Errorf("write setting %s: %w", key, err)
		}
		return nil
	})
}
