// Package auth implements Bunkarr's authentication, modelled on the *arr apps: one UI user with
// forms login (bcrypt password, server-side sessions in SQLite), an API key for scripts and
// webhooks (X-Api-Key header or ?apikey=), and an optional "disabled for local addresses" mode.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"github.com/sl0wz3r/bunkarr/internal/config"
	"github.com/sl0wz3r/bunkarr/internal/db"
)

// Mode says when a login is required.
type Mode string

const (
	// ModeEnabled requires a login (or API key) for every client.
	ModeEnabled Mode = "enabled"
	// ModeLocalDisabled lets clients with a private/loopback address in without a login.
	ModeLocalDisabled Mode = "disabled_for_local_addresses"
)

// Valid reports whether m is a known mode.
func (m Mode) Valid() bool { return m == ModeEnabled || m == ModeLocalDisabled }

const (
	// SessionTTL is how long a login stays valid.
	SessionTTL = 30 * 24 * time.Hour
	// MinPasswordLen is the shortest accepted password.
	MinPasswordLen = 8
	// MaxPasswordBytes is bcrypt's input limit.
	MaxPasswordBytes = 72
	// MaxUsernameLen is the longest accepted username, in characters.
	MaxUsernameLen = 64
	// DefaultBcryptCost is the bcrypt work factor.
	DefaultBcryptCost = 12
	// lastSeenEvery throttles session last_seen_at writes.
	lastSeenEvery = time.Minute
)

var (
	// ErrSetupDone means the first-run user already exists.
	ErrSetupDone = errors.New("setup has already been completed")
	// ErrInvalidCredentials means the username or password is wrong.
	ErrInvalidCredentials = errors.New("invalid username or password")
)

// ValidationError is a user-facing input error.
type ValidationError string

func (e ValidationError) Error() string { return string(e) }

// User is the UI account.
type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

// Session is a new login.
type Session struct {
	Token   string
	Expires time.Time
	User    User
}

// Service is the authentication service.
type Service struct {
	db       *db.DB
	settings *config.Settings
	log      *slog.Logger
	now      func() time.Time
	cost     int
	bcrypt   chan struct{} // bounds concurrent bcrypt work
	dummy    []byte        // hash compared against for unknown users (constant-ish timing)
	Limiter  *Limiter

	mu     sync.RWMutex
	apiKey string
	mode   Mode
}

// New returns a Service. Call Init before use.
func New(d *db.DB, s *config.Settings, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{
		db:       d,
		settings: s,
		log:      log,
		now:      time.Now,
		cost:     DefaultBcryptCost,
		bcrypt:   make(chan struct{}, 4),
		Limiter:  NewLimiter(5, 15*time.Minute),
	}
}

// SetBcryptCost lowers the work factor (tests only).
func (s *Service) SetBcryptCost(c int) { s.cost = c }

// Init generates the API key on first run and loads the cached settings.
func (s *Service) Init(ctx context.Context) error {
	dummy, err := bcrypt.GenerateFromPassword([]byte("bunkarr-timing-equalizer"), s.cost)
	if err != nil {
		return err
	}
	s.dummy = dummy

	key, ok, err := s.settings.GetSecret(ctx, config.KeyAPIKey)
	if err != nil {
		return err
	}
	if !ok {
		key, err = s.RegenerateAPIKey(ctx)
		if err != nil {
			return err
		}
		s.log.Info("Generated a new API key (Settings > General)")
	}
	mode := ModeEnabled
	if v, ok, err := s.settings.Get(ctx, config.KeyAuthRequired); err != nil {
		return err
	} else if ok && Mode(v).Valid() {
		mode = Mode(v)
	}
	s.mu.Lock()
	s.apiKey, s.mode = key, mode
	s.mu.Unlock()
	return nil
}

// APIKey returns the current API key.
func (s *Service) APIKey() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.apiKey
}

// CheckAPIKey compares k with the API key in constant time.
func (s *Service) CheckAPIKey(k string) bool {
	cur := s.APIKey()
	return cur != "" && subtle.ConstantTimeCompare([]byte(k), []byte(cur)) == 1
}

// RegenerateAPIKey replaces the API key with a new random one (32 hex characters, like the *arrs).
func (s *Service) RegenerateAPIKey(ctx context.Context) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	key := hex.EncodeToString(b)
	if err := s.settings.SetSecret(ctx, config.KeyAPIKey, key); err != nil {
		return "", err
	}
	s.mu.Lock()
	s.apiKey = key
	s.mu.Unlock()
	return key, nil
}

// Mode returns the authentication mode.
func (s *Service) Mode() Mode {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mode
}

// SetMode stores the authentication mode.
func (s *Service) SetMode(ctx context.Context, m Mode) error {
	if !m.Valid() {
		return ValidationError(fmt.Sprintf("authentication required must be %q or %q", ModeEnabled, ModeLocalDisabled))
	}
	if err := s.settings.Set(ctx, config.KeyAuthRequired, string(m)); err != nil {
		return err
	}
	s.mu.Lock()
	s.mode = m
	s.mu.Unlock()
	return nil
}

// SetupRequired reports whether no user exists yet (first run, or after reset-auth).
func (s *Service) SetupRequired(ctx context.Context) (bool, error) {
	var n int
	if err := s.db.Reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return false, fmt.Errorf("count users: %w", err)
	}
	return n == 0, nil
}

// Setup creates the one UI user. It fails with ErrSetupDone once a user exists.
func (s *Service) Setup(ctx context.Context, username, password string) (User, error) {
	username = strings.TrimSpace(username)
	if err := validateUsername(username); err != nil {
		return User{}, err
	}
	if err := validatePassword(password); err != nil {
		return User{}, err
	}
	hash, err := s.hash(ctx, password)
	if err != nil {
		return User{}, err
	}
	var u User
	err = s.db.Write(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrSetupDone
		}
		now := db.FormatTime(s.now())
		res, err := tx.ExecContext(ctx, `INSERT INTO users (username, password_hash, created_at, updated_at) VALUES (?, ?, ?, ?)`,
			username, string(hash), now, now)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		u = User{ID: id, Username: username}
		return err
	})
	if err != nil {
		return User{}, err
	}
	s.log.Info("Created the Bunkarr user", "user", username)
	return u, nil
}

// SessionMeta is recorded with a session for the user's own reference.
type SessionMeta struct {
	RemoteAddr string
	UserAgent  string
}

// Login checks the credentials and starts a session.
func (s *Service) Login(ctx context.Context, username, password string, meta SessionMeta) (Session, error) {
	username = strings.TrimSpace(username)
	var (
		u    User
		hash string
	)
	err := s.db.Reader().QueryRowContext(ctx, `SELECT id, username, password_hash FROM users WHERE username = ?`, username).
		Scan(&u.ID, &u.Username, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		_ = s.compare(ctx, s.dummy, password)
		return Session{}, ErrInvalidCredentials
	}
	if err != nil {
		return Session{}, fmt.Errorf("look up user: %w", err)
	}
	if err := s.compare(ctx, []byte(hash), password); err != nil {
		if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
			return Session{}, ErrInvalidCredentials
		}
		return Session{}, err
	}
	return s.newSession(ctx, u, meta)
}

func (s *Service) newSession(ctx context.Context, u User, meta SessionMeta) (Session, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return Session{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	now := s.now()
	exp := now.Add(SessionTTL)
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, db.FormatTime(now)); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO sessions (token_hash, user_id, created_at, expires_at, last_seen_at, remote_addr, user_agent)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			hashToken(token), u.ID, db.FormatTime(now), db.FormatTime(exp), db.FormatTime(now),
			truncate(meta.RemoteAddr, 64), truncate(meta.UserAgent, 256))
		return err
	})
	if err != nil {
		return Session{}, fmt.Errorf("create session: %w", err)
	}
	return Session{Token: token, Expires: exp, User: u}, nil
}

// SessionUser resolves a session token. ok is false for unknown or expired tokens.
func (s *Service) SessionUser(ctx context.Context, token string) (u User, ok bool, err error) {
	if token == "" || len(token) > 128 {
		return User{}, false, nil
	}
	th := hashToken(token)
	var exp, seen string
	err = s.db.Reader().QueryRowContext(ctx, `SELECT u.id, u.username, s.expires_at, s.last_seen_at
		FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.token_hash = ?`, th).
		Scan(&u.ID, &u.Username, &exp, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, false, nil
	}
	if err != nil {
		return User{}, false, fmt.Errorf("look up session: %w", err)
	}
	now := s.now()
	expT, err := db.ParseTime(exp)
	if err != nil || !now.Before(expT) {
		return User{}, false, nil
	}
	if seenT, err := db.ParseTime(seen); err == nil && now.Sub(seenT) > lastSeenEvery {
		_ = s.db.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ? WHERE token_hash = ?`, db.FormatTime(now), th)
			return err
		})
	}
	return u, true, nil
}

// Logout ends the session with this token (no error when it does not exist).
func (s *Service) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hashToken(token))
		return err
	})
}

// ChangeCredentials sets a new username and/or password after checking the current password, and
// ends every other session of the user. keepToken (the caller's session, may be "") stays valid.
func (s *Service) ChangeCredentials(ctx context.Context, userID int64, currentPassword, newUsername, newPassword, keepToken string) (User, error) {
	var u User
	var hash string
	err := s.db.Reader().QueryRowContext(ctx, `SELECT id, username, password_hash FROM users WHERE id = ?`, userID).
		Scan(&u.ID, &u.Username, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrInvalidCredentials
	}
	if err != nil {
		return User{}, err
	}
	if err := s.compare(ctx, []byte(hash), currentPassword); err != nil {
		if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
			return User{}, ErrInvalidCredentials
		}
		return User{}, err
	}
	newUsername = strings.TrimSpace(newUsername)
	if newUsername == "" {
		newUsername = u.Username
	}
	if err := validateUsername(newUsername); err != nil {
		return User{}, err
	}
	newHash := hash
	if newPassword != "" {
		if err := validatePassword(newPassword); err != nil {
			return User{}, err
		}
		h, err := s.hash(ctx, newPassword)
		if err != nil {
			return User{}, err
		}
		newHash = string(h)
	}
	err = s.db.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET username = ?, password_hash = ?, updated_at = ? WHERE id = ?`,
			newUsername, newHash, db.FormatTime(s.now()), userID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND token_hash <> ?`, userID, hashToken(keepToken))
		return err
	})
	if err != nil {
		return User{}, fmt.Errorf("update credentials: %w", err)
	}
	s.log.Info("Changed the Bunkarr credentials", "user", newUsername)
	return User{ID: userID, Username: newUsername}, nil
}

// ResetAuth removes the user and every session, so the next visit to the UI shows the first-run
// setup again. The API key is kept. Used by `bunkarr reset-auth` when the password is lost.
func ResetAuth(ctx context.Context, d *db.DB) error {
	return d.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM users`)
		return err
	})
}

func (s *Service) hash(ctx context.Context, password string) ([]byte, error) {
	release, err := s.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return bcrypt.GenerateFromPassword([]byte(password), s.cost)
}

func (s *Service) compare(ctx context.Context, hash []byte, password string) error {
	release, err := s.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	return bcrypt.CompareHashAndPassword(hash, []byte(password))
}

func (s *Service) acquire(ctx context.Context) (func(), error) {
	select {
	case s.bcrypt <- struct{}{}:
		return func() { <-s.bcrypt }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func validateUsername(u string) error {
	if u == "" {
		return ValidationError("username is required")
	}
	if utf8.RuneCountInString(u) > MaxUsernameLen {
		return ValidationError(fmt.Sprintf("username must be at most %d characters", MaxUsernameLen))
	}
	for _, r := range u {
		if unicode.IsControl(r) {
			return ValidationError("username must not contain control characters")
		}
	}
	return nil
}

func validatePassword(p string) error {
	if utf8.RuneCountInString(p) < MinPasswordLen {
		return ValidationError(fmt.Sprintf("password must be at least %d characters", MinPasswordLen))
	}
	if len(p) > MaxPasswordBytes {
		return ValidationError(fmt.Sprintf("password must be at most %d bytes", MaxPasswordBytes))
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
