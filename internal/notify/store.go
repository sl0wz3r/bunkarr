package notify

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/config"
	"github.com/sl0wz3r/bunkarr/internal/db"
)

// Store persists notification targets in the notifications table.
type Store struct {
	db  *db.DB
	kr  *config.Keyring
	now func() time.Time
}

// NewStore returns a store over d; kr seals and opens the Apprise URLs.
func NewStore(d *db.DB, kr *config.Keyring) *Store {
	return &Store{db: d, kr: kr, now: time.Now}
}

// secretOwner names row id's URLs in the redaction registry (logging.SetSecrets).
func secretOwner(id int64) string { return fmt.Sprintf("notification:%d", id) }

// urlsAAD binds a sealed URL list to its row (ADR 0003).
func urlsAAD(id int64) string { return fmt.Sprintf("notification:%d:urls", id) }

// storedSettings is the notifications.settings JSON.
type storedSettings struct {
	APIURL    string `json:"apiUrl"`
	ConfigKey string `json:"configKey"`
}

const selectColumns = `id, name, kind, settings, secret, enabled, on_failure, on_warning, on_success, created_at, updated_at`

// row is a notifications row with its sealed URLs.
type row struct {
	Notification
	sealed string
}

type rowScanner interface {
	Scan(dest ...any) error
}

type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func scanRow(sc rowScanner) (row, error) {
	var (
		r                        row
		kind, settings, cAt, uAt string
	)
	if err := sc.Scan(&r.ID, &r.Name, &kind, &settings, &r.sealed, &r.Enabled, &r.OnFailure, &r.OnWarning, &r.OnSuccess, &cAt, &uAt); err != nil {
		return row{}, err
	}
	r.Kind = Kind(kind)
	var st storedSettings
	if err := json.Unmarshal([]byte(settings), &st); err != nil {
		return row{}, fmt.Errorf("notification %d: settings: %w", r.ID, err)
	}
	r.APIURL, r.ConfigKey = st.APIURL, st.ConfigKey
	r.HasURLs = r.sealed != ""
	var err error
	if r.CreatedAt, err = db.ParseTime(cAt); err != nil {
		return row{}, fmt.Errorf("notification %d: created_at: %w", r.ID, err)
	}
	if r.UpdatedAt, err = db.ParseTime(uAt); err != nil {
		return row{}, fmt.Errorf("notification %d: updated_at: %w", r.ID, err)
	}
	return r, nil
}

func getRow(ctx context.Context, q queryRower, id int64) (row, error) {
	r, err := scanRow(q.QueryRowContext(ctx, `SELECT `+selectColumns+` FROM notifications WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return row{}, fmt.Errorf("%w: id %d", ErrNotFound, id)
	}
	if err != nil {
		return row{}, fmt.Errorf("read notification %d: %w", id, err)
	}
	return r, nil
}

// List returns every notification, ordered by name.
func (s *Store) List(ctx context.Context) ([]Notification, error) {
	rows, err := s.queryRows(ctx, `SELECT `+selectColumns+` FROM notifications ORDER BY name, id`)
	if err != nil {
		return nil, fmt.Errorf("list notifications: %w", err)
	}
	out := make([]Notification, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Notification)
	}
	return out, nil
}

// Get returns one notification, or an error wrapping ErrNotFound.
func (s *Store) Get(ctx context.Context, id int64) (Notification, error) {
	r, err := getRow(ctx, s.db.Reader(), id)
	if err != nil {
		return Notification{}, err
	}
	return r.Notification, nil
}

// Create validates and stores a new notification. Its URLs are sealed in the same transaction as
// the insert (the AAD needs the new id) and held as secrets once stored.
func (s *Store) Create(ctx context.Context, in Input) (Notification, error) {
	v, err := normalize(in, nil)
	if err != nil {
		return Notification{}, err
	}
	settings, err := v.settingsJSON()
	if err != nil {
		return Notification{}, err
	}
	now := db.FormatTime(s.now())
	var out row
	err = s.db.Write(ctx, func(tx *sql.Tx) error {
		if err := nameFree(ctx, tx, v.name, 0); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO notifications
			(name, kind, settings, secret, enabled, on_failure, on_warning, on_success, created_at, updated_at)
			VALUES (?, ?, ?, '', ?, ?, ?, ?, ?, ?)`,
			v.name, string(KindApprise), settings, v.enabled, v.onFailure, v.onWarning, v.onSuccess, now, now)
		if err != nil {
			return fmt.Errorf("insert notification: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("insert notification: %w", err)
		}
		if v.urls != "" {
			sealed, err := s.kr.Seal(v.urls, urlsAAD(id))
			if err != nil {
				return fmt.Errorf("seal notification URLs: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE notifications SET secret = ? WHERE id = ?`, sealed, id); err != nil {
				return fmt.Errorf("store notification URLs: %w", err)
			}
		}
		out, err = getRow(ctx, tx, id)
		return err
	})
	if err != nil {
		return Notification{}, err
	}
	holdURLs(out.ID, v.urls)
	return out.Notification, nil
}

// Update validates and replaces notification id (see Input for what an empty field keeps). An
// empty URLs keeps the stored URLs only with the stored API URL: changing the API URL of a
// stateless notification without sending its URLs again is refused (validEndpoint).
func (s *Store) Update(ctx context.Context, id int64, in Input) (Notification, error) {
	var (
		urls     string
		replaced bool // the stored URLs are replaced by urls ("": removed)
		out      row
	)
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		cur, err := getRow(ctx, tx, id)
		if err != nil {
			return err
		}
		v, err := normalize(in, &cur.Notification)
		if err != nil {
			return err
		}
		if err := nameFree(ctx, tx, v.name, id); err != nil {
			return err
		}
		settings, err := v.settingsJSON()
		if err != nil {
			return err
		}
		sealed := cur.sealed
		switch {
		case v.configKey != "":
			sealed = "" // stateful mode does not use URLs; do not keep unused secrets
			replaced = true
		case v.urls != "":
			if sealed, err = s.kr.Seal(v.urls, urlsAAD(id)); err != nil {
				return fmt.Errorf("seal notification URLs: %w", err)
			}
			urls, replaced = v.urls, true
		}
		if _, err := tx.ExecContext(ctx, `UPDATE notifications SET name = ?, settings = ?, secret = ?,
			enabled = ?, on_failure = ?, on_warning = ?, on_success = ?, updated_at = ? WHERE id = ?`,
			v.name, settings, sealed, v.enabled, v.onFailure, v.onWarning, v.onSuccess,
			db.FormatTime(s.now()), id); err != nil {
			return fmt.Errorf("update notification %d: %w", id, err)
		}
		out, err = getRow(ctx, tx, id)
		return err
	})
	if err != nil {
		return Notification{}, err
	}
	if replaced {
		holdURLs(id, urls)
	}
	return out.Notification, nil
}

// Delete removes notification id, or returns an error wrapping ErrNotFound.
func (s *Store) Delete(ctx context.Context, id int64) error {
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM notifications WHERE id = ?`, id)
		if err != nil {
			return fmt.Errorf("delete notification %d: %w", id, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("delete notification %d: %w", id, err)
		}
		if n == 0 {
			return fmt.Errorf("%w: id %d", ErrNotFound, id)
		}
		return nil
	})
	if err != nil {
		return err
	}
	holdURLs(id, "")
	return nil
}

// RegisterSecrets holds every stored URL list, and each URL in it, in the redaction registry
// (logging.SetSecrets). Call it once at startup, before jobs run. A row whose URLs do not
// decrypt is reported in the returned error; the others are still registered.
func (s *Store) RegisterSecrets(ctx context.Context) error {
	rows, err := s.queryRows(ctx, `SELECT `+selectColumns+` FROM notifications WHERE secret <> ''`)
	if err != nil {
		return fmt.Errorf("register notification secrets: %w", err)
	}
	var errs []error
	for _, r := range rows {
		urls, err := s.kr.Open(r.sealed, urlsAAD(r.ID))
		if err != nil {
			errs = append(errs, fmt.Errorf("notification %q: open URLs: %w", r.Name, err))
			continue
		}
		holdURLs(r.ID, urls)
	}
	return errors.Join(errs...)
}

// subscribers returns the enabled notifications that want messages of type t.
func (s *Store) subscribers(ctx context.Context, t MessageType) ([]row, error) {
	var column string
	switch t {
	case TypeFailure:
		column = "on_failure"
	case TypeWarning:
		column = "on_warning"
	case TypeSuccess:
		column = "on_success"
	default:
		return nil, fmt.Errorf("no notification event for message type %q", t)
	}
	rows, err := s.queryRows(ctx, `SELECT `+selectColumns+` FROM notifications WHERE enabled = 1 AND `+column+` = 1 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list notification subscribers: %w", err)
	}
	return rows, nil
}

// openTarget returns r as a Target, with its URLs opened (stateless mode).
func (s *Store) openTarget(r row) (Target, error) {
	t := Target{ID: r.ID, Name: r.Name, APIURL: r.APIURL, ConfigKey: r.ConfigKey}
	if t.Stateful() {
		return t, nil
	}
	if r.sealed == "" {
		return Target{}, fmt.Errorf("notification %q has no Apprise URLs", r.Name)
	}
	urls, err := s.kr.Open(r.sealed, urlsAAD(r.ID))
	if err != nil {
		return Target{}, fmt.Errorf("notification %q: open URLs: %w", r.Name, err)
	}
	holdURLs(r.ID, urls)
	t.URLs = urls
	return t, nil
}

// target returns saved notification id as a Target.
func (s *Store) target(ctx context.Context, id int64) (Target, error) {
	r, err := getRow(ctx, s.db.Reader(), id)
	if err != nil {
		return Target{}, err
	}
	return s.openTarget(r)
}

// testTarget validates an unsaved Input for a test message. With id > 0 (the edit form of a saved
// notification) an empty URLs field uses the stored URLs, only with the stored API URL
// (validEndpoint). The name is optional here. URLs from in are not held as secrets (they are not
// stored); Dispatcher.Test redacts them from its error.
func (s *Store) testTarget(ctx context.Context, id int64, in Input) (Target, error) {
	var (
		cur   *row
		saved *Notification
	)
	if id > 0 {
		r, err := getRow(ctx, s.db.Reader(), id)
		if err != nil {
			return Target{}, err
		}
		cur, saved = &r, &r.Notification
	}
	ep, err := validEndpoint(in, saved)
	if err != nil {
		return Target{}, err
	}
	t := Target{ID: id, Name: strings.TrimSpace(in.Name), APIURL: ep.apiURL, ConfigKey: ep.configKey, URLs: ep.urls}
	if t.Name == "" && cur != nil {
		t.Name = cur.Name
	}
	if !t.Stateful() && t.URLs == "" {
		stored, err := s.openTarget(*cur)
		if err != nil {
			return Target{}, err
		}
		t.URLs = stored.URLs
	}
	return t, nil
}

func (s *Store) queryRows(ctx context.Context, query string, args ...any) ([]row, error) {
	rows, err := s.db.Reader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []row
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// nameFree fails with a ValidationError when another notification (not id) has name, compared
// case-insensitively (the column is COLLATE NOCASE). Writes are serialized, so the check and the
// following insert or update cannot race.
func nameFree(ctx context.Context, tx *sql.Tx, name string, id int64) error {
	var other int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM notifications WHERE name = ? AND id <> ?`, name, id).Scan(&other)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("check notification name: %w", err)
	}
	return ValidationError(fmt.Sprintf("a notification named %q already exists", name))
}

func (v values) settingsJSON() (string, error) {
	b, err := json.Marshal(storedSettings{APIURL: v.apiURL, ConfigKey: v.configKey})
	if err != nil {
		return "", fmt.Errorf("encode notification settings: %w", err)
	}
	return string(b), nil
}
