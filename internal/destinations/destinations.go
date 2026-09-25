// Package destinations manages Bunkarr's backup destinations (design docs/design/phase1.md §2,
// §3, §7): the destinations and destination_sources tables, the destination marker
// (.bunkarr/destination.json) and filesystem checks that keep every write on the right
// filesystem (safety rule S3), the overlap checks of S4, and the capability probe (§3).
//
// Jobs reach a destination only through Open, which returns a Handle holding an *os.Root on the
// target after checking the marker and the filesystem type; the filecopy engine
// (internal/engines/filecopy) then works through that root.
package destinations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
)

// EngineFilecopy is the only engine Phase 1 implements.
const EngineFilecopy = "filecopy"

// MaxNameLen is the longest destination name, in characters.
const MaxNameLen = 100

// Capabilities is what the destination probe found (design §3); see filecopy.Capabilities.
type Capabilities = filecopy.Capabilities

var (
	// ErrNotFound means no destination has the given id.
	ErrNotFound = errors.New("destination not found")
	// ErrNameTaken means another destination has the name (names are case-insensitive).
	ErrNameTaken = errors.New("a destination with this name already exists")
	// ErrMarkerExists means the target already holds a Bunkarr marker: create with Attach to use
	// it, or it is already a destination of this Bunkarr.
	ErrMarkerExists = errors.New("the target is already a Bunkarr destination")
	// ErrLocalFilesystem means the target is on a local filesystem (tmpfs, overlay, or the disk
	// holding / or the config directory) and CreateOptions.AllowLocal was not set.
	ErrLocalFilesystem = errors.New("the target is on a local filesystem")
	// ErrNotMounted means the target or its marker is missing: the share is probably not mounted
	// and Bunkarr would otherwise write into the empty mountpoint.
	ErrNotMounted = errors.New("destination not mounted?")
	// ErrMarkerMismatch means the target's marker belongs to another destination (or is unreadable).
	ErrMarkerMismatch = errors.New("destination marker does not match (another share mounted here?)")
	// ErrFSChanged means the target's filesystem type is not the one recorded at creation.
	ErrFSChanged = errors.New("destination filesystem changed (destination not mounted?)")
)

// ValidationError is a user-facing input error.
type ValidationError string

func (e ValidationError) Error() string { return string(e) }

// Destination is a stored destination.
type Destination struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Engine string `json:"engine"`
	// Target is the resolved (symlink-free) absolute path of the destination directory.
	Target  string `json:"target"`
	Enabled bool   `json:"enabled"`
	// SourceIDs are the linked sources, ascending.
	SourceIDs []int64 `json:"sourceIds"`
	// FSType is the filesystem type recorded at creation (filecopy.FSStat.Type).
	FSType       string       `json:"fsType"`
	Capabilities Capabilities `json:"capabilities"`
	Settings     Settings     `json:"settings"`
	Retention    Retention    `json:"retention"`
	// MarkerID is the id in the target's .bunkarr/destination.json.
	MarkerID string `json:"-"`
	// RootDev is the target's st_dev at creation (diagnostics only: network filesystems get a
	// new device number on every mount, so it is not compared).
	RootDev   uint64    `json:"-"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Input is a destination to create or the changes to one. On Update, empty or nil fields keep
// the stored value; Target and Engine cannot change after creation.
type Input struct {
	Name string `json:"name"`
	// Engine defaults to "filecopy".
	Engine string `json:"engine"`
	// Target is the absolute path of the destination directory (a mounted share). It must exist.
	Target  string `json:"target"`
	Enabled *bool  `json:"enabled"`
	// Settings and Retention default to DefaultSettings and DefaultRetention; zero values inside
	// take the defaults too.
	Settings  *Settings  `json:"settings"`
	Retention *Retention `json:"retention"`
	// SourceIDs replaces the linked sources when not nil.
	SourceIDs []int64 `json:"sourceIds"`
}

// CreateOptions are the confirmations of design S3.
type CreateOptions struct {
	// Attach adopts an existing marker's id instead of refusing the target.
	Attach bool `json:"attach"`
	// AllowLocal accepts a target on a local filesystem.
	AllowLocal bool `json:"allowLocal"`
}

// Options configures a Store.
type Options struct {
	// LocalDevs are the st_dev values of "/" and of the config directory: a target on one of
	// them is on a local filesystem (S3). See DevsOf.
	LocalDevs []uint64
	// ConfigDir is Bunkarr's config directory; a target may not equal, contain or be inside it
	// (S4). Empty skips the check.
	ConfigDir string
	// PathGuard checks a resolved target against the sources (S4: a target may not equal,
	// contain or be inside a source). It should return an error the API reports as a validation
	// failure. nil skips the check.
	PathGuard func(ctx context.Context, target string) error
	// Log receives creation and probe events; nil discards them.
	Log *slog.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Lstat stats a path inside a destination for the capability probe and Handle.Lstat; nil
	// means filecopy.Lstat. Tests replace it to simulate a filesystem that numbers inodes per
	// lookup (Capabilities.UnstableInodes).
	Lstat func(root *os.Root, rel string) (filecopy.Stat, error)
}

// Store is the destinations store. It is safe for concurrent use.
type Store struct {
	db   *db.DB
	opts Options
	log  *slog.Logger
	now  func() time.Time
	// createMu serializes Create, so two creations cannot both pass the overlap and marker
	// checks for the same target.
	createMu sync.Mutex
	// statFS is filecopy.StatFS; tests replace it to simulate a filesystem change.
	statFS func(*os.Root) (filecopy.FSStat, error)
	// lstat is Options.Lstat.
	lstat func(root *os.Root, rel string) (filecopy.Stat, error)
}

// New returns a Store over d.
func New(d *db.DB, o Options) *Store {
	s := &Store{db: d, opts: o, log: o.Log, now: o.Now, statFS: filecopy.StatFS, lstat: o.Lstat}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.lstat == nil {
		s.lstat = filecopy.Lstat
	}
	return s
}

// DevsOf returns the st_dev of each path (for Options.LocalDevs: "/" and the config directory).
func DevsOf(paths ...string) ([]uint64, error) {
	out := make([]uint64, 0, len(paths))
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", p, err)
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return nil, fmt.Errorf("stat %s: no device number", p)
		}
		out = append(out, uint64(st.Dev))
	}
	return out, nil
}

const selectCols = `id, name, engine, target, marker_id, settings, retention, fs_type, root_dev, capabilities, enabled, created_at, updated_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanDestination(r rowScanner) (Destination, error) {
	var (
		d                                     Destination
		settings, retention, caps, created, u string
		rootDev                               int64
	)
	if err := r.Scan(&d.ID, &d.Name, &d.Engine, &d.Target, &d.MarkerID, &settings, &retention, &d.FSType, &rootDev, &caps, &d.Enabled, &created, &u); err != nil {
		return Destination{}, err
	}
	d.RootDev = uint64(rootDev)
	var err error
	if d.Settings, err = ParseSettings(settings); err != nil {
		return Destination{}, fmt.Errorf("destination %d: %w", d.ID, err)
	}
	if d.Retention, err = ParseRetention(retention); err != nil {
		return Destination{}, fmt.Errorf("destination %d: %w", d.ID, err)
	}
	if caps != "" {
		if err := json.Unmarshal([]byte(caps), &d.Capabilities); err != nil {
			return Destination{}, fmt.Errorf("destination %d: parse capabilities: %w", d.ID, err)
		}
	}
	if d.CreatedAt, err = db.ParseTime(created); err != nil {
		return Destination{}, fmt.Errorf("destination %d: created_at: %w", d.ID, err)
	}
	if d.UpdatedAt, err = db.ParseTime(u); err != nil {
		return Destination{}, fmt.Errorf("destination %d: updated_at: %w", d.ID, err)
	}
	d.SourceIDs = []int64{}
	return d, nil
}

// List returns every destination, by name.
func (s *Store) List(ctx context.Context) ([]Destination, error) {
	rows, err := s.db.Reader().QueryContext(ctx, `SELECT `+selectCols+` FROM destinations ORDER BY name COLLATE NOCASE, id`)
	if err != nil {
		return nil, fmt.Errorf("list destinations: %w", err)
	}
	defer rows.Close()
	out := []Destination{}
	byID := map[int64]int{}
	for rows.Next() {
		d, err := scanDestination(rows)
		if err != nil {
			return nil, fmt.Errorf("list destinations: %w", err)
		}
		byID[d.ID] = len(out)
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list destinations: %w", err)
	}
	links, err := s.db.Reader().QueryContext(ctx, `SELECT destination_id, source_id FROM destination_sources ORDER BY destination_id, source_id`)
	if err != nil {
		return nil, fmt.Errorf("list destination sources: %w", err)
	}
	defer links.Close()
	for links.Next() {
		var did, sid int64
		if err := links.Scan(&did, &sid); err != nil {
			return nil, fmt.Errorf("list destination sources: %w", err)
		}
		if i, ok := byID[did]; ok {
			out[i].SourceIDs = append(out[i].SourceIDs, sid)
		}
	}
	if err := links.Err(); err != nil {
		return nil, fmt.Errorf("list destination sources: %w", err)
	}
	return out, nil
}

// Get returns one destination (ErrNotFound when there is none).
func (s *Store) Get(ctx context.Context, id int64) (Destination, error) {
	d, err := scanDestination(s.db.Reader().QueryRowContext(ctx, `SELECT `+selectCols+` FROM destinations WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Destination{}, ErrNotFound
	}
	if err != nil {
		return Destination{}, fmt.Errorf("get destination %d: %w", id, err)
	}
	if d.SourceIDs, err = s.Sources(ctx, id); err != nil {
		return Destination{}, err
	}
	return d, nil
}

// Sources returns the ids of the sources linked to a destination, ascending.
func (s *Store) Sources(ctx context.Context, id int64) ([]int64, error) {
	return s.queryIDs(ctx, `SELECT source_id FROM destination_sources WHERE destination_id = ? ORDER BY source_id`, id)
}

// ForSource returns the ids of the destinations a source is linked to, ascending.
func (s *Store) ForSource(ctx context.Context, sourceID int64) ([]int64, error) {
	return s.queryIDs(ctx, `SELECT destination_id FROM destination_sources WHERE source_id = ? ORDER BY destination_id`, sourceID)
}

func (s *Store) queryIDs(ctx context.Context, q string, arg int64) ([]int64, error) {
	rows, err := s.db.Reader().QueryContext(ctx, q, arg)
	if err != nil {
		return nil, fmt.Errorf("query destination sources: %w", err)
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("query destination sources: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query destination sources: %w", err)
	}
	return out, nil
}

// SetSources replaces the sources linked to a destination.
func (s *Store) SetSources(ctx context.Context, id int64, sourceIDs []int64) error {
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM destinations WHERE id = ?`, id).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("set destination sources: %w", err)
		}
		if err := setSourcesTx(ctx, tx, id, sourceIDs); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE destinations SET updated_at = ? WHERE id = ?`, db.FormatTime(s.now()), id)
		if err != nil {
			return fmt.Errorf("set destination sources: %w", err)
		}
		return nil
	})
}

func setSourcesTx(ctx context.Context, tx *sql.Tx, id int64, sourceIDs []int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM destination_sources WHERE destination_id = ?`, id); err != nil {
		return fmt.Errorf("set destination sources: %w", err)
	}
	ids := slices.Clone(sourceIDs)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	for _, sid := range ids {
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM sources WHERE id = ?`, sid).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return ValidationError(fmt.Sprintf("source %d does not exist", sid))
		}
		if err != nil {
			return fmt.Errorf("set destination sources: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO destination_sources (destination_id, source_id) VALUES (?, ?)`, id, sid); err != nil {
			return fmt.Errorf("set destination sources: %w", err)
		}
	}
	return nil
}

// Update changes a destination's name, enabled flag, settings, retention and linked sources.
// A Target or Engine different from the stored one is refused: a destination cannot move (its
// records describe what is on that filesystem); create a new one instead.
func (s *Store) Update(ctx context.Context, id int64, in Input) (Destination, error) {
	cur, err := s.Get(ctx, id)
	if err != nil {
		return Destination{}, err
	}
	if in.Target != "" && !sameTarget(in.Target, cur.Target) {
		return Destination{}, ValidationError("the target of a destination cannot be changed; create a new destination instead")
	}
	if in.Engine != "" && in.Engine != cur.Engine {
		return Destination{}, ValidationError("the engine of a destination cannot be changed")
	}
	name := cur.Name
	if in.Name != "" {
		if name, err = checkName(in.Name); err != nil {
			return Destination{}, err
		}
	}
	enabled := cur.Enabled
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	settings := cur.Settings
	if in.Settings != nil {
		if settings, err = in.Settings.Normalize(); err != nil {
			return Destination{}, err
		}
	}
	retention := cur.Retention
	if in.Retention != nil {
		if retention, err = in.Retention.Normalize(); err != nil {
			return Destination{}, err
		}
	}
	sj, rj, err := marshalConfig(settings, retention)
	if err != nil {
		return Destination{}, err
	}
	err = s.db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE destinations SET name = ?, enabled = ?, settings = ?, retention = ?, updated_at = ? WHERE id = ?`,
			name, enabled, sj, rj, db.FormatTime(s.now()), id)
		if err != nil {
			return mapConstraint(err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if in.SourceIDs != nil {
			return setSourcesTx(ctx, tx, id, in.SourceIDs)
		}
		return nil
	})
	if err != nil {
		return Destination{}, err
	}
	return s.Get(ctx, id)
}

// Delete removes a destination's row, its source links and (by cascade) its file records and
// snapshot records. Nothing at the target is touched: backup data and the marker stay (a new
// destination on the same target needs Attach).
func (s *Store) Delete(ctx context.Context, id int64) error {
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		// destination_files.link_of is ON DELETE RESTRICT, which SQLite checks row by row during
		// the cascade, so a primary deleted before its dependents fails the whole delete. Remove
		// the dependent rows first, leaves first (each pass deletes rows nothing points at).
		for {
			res, err := tx.ExecContext(ctx, `DELETE FROM destination_files WHERE destination_id = ? AND link_of IS NOT NULL
				AND id NOT IN (SELECT link_of FROM destination_files WHERE link_of IS NOT NULL)`, id)
			if err != nil {
				return fmt.Errorf("delete destination %d: file records: %w", id, err)
			}
			if n, err := res.RowsAffected(); err != nil || n == 0 {
				break
			}
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM destinations WHERE id = ?`, id)
		if err != nil {
			return fmt.Errorf("delete destination %d: %w", id, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// checkName trims and validates a destination name.
func checkName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", ValidationError("a destination needs a name")
	}
	if utf8.RuneCountInString(name) > MaxNameLen {
		return "", ValidationError(fmt.Sprintf("the name is longer than %d characters", MaxNameLen))
	}
	if strings.ContainsFunc(name, unicode.IsControl) {
		return "", ValidationError("the name contains control characters")
	}
	return name, nil
}

func marshalConfig(s Settings, r Retention) (string, string, error) {
	sj, err := json.Marshal(s)
	if err != nil {
		return "", "", fmt.Errorf("encode settings: %w", err)
	}
	rj, err := json.Marshal(r)
	if err != nil {
		return "", "", fmt.Errorf("encode retention: %w", err)
	}
	return string(sj), string(rj), nil
}

// mapConstraint turns unique-constraint failures into this package's errors.
func mapConstraint(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "UNIQUE constraint failed: destinations.name"):
		return ErrNameTaken
	case strings.Contains(msg, "UNIQUE constraint failed: destinations.marker_id"):
		return fmt.Errorf("%w: its marker is already used by another destination", ErrMarkerExists)
	default:
		return fmt.Errorf("write destination: %w", err)
	}
}

// sameTarget reports whether path p names the stored (resolved) target.
func sameTarget(p, target string) bool {
	if filepath.Clean(p) == target {
		return true
	}
	r, err := filepath.EvalSymlinks(p)
	return err == nil && r == target
}

// overlaps reports whether a equals b or one contains the other (both clean and absolute).
func overlaps(a, b string) bool {
	if a == b {
		return true
	}
	within := func(inner, outer string) bool {
		if outer == "/" {
			return true
		}
		return strings.HasPrefix(inner, outer+"/")
	}
	return within(a, b) || within(b, a)
}
