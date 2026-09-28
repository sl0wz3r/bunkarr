// Package destinations manages Bunkarr's backup destinations (design docs/design/phase1.md §2,
// §3, §7; docs/design/phase4.md §4, §5): the destinations and destination_sources tables, the
// destination marker (.bunkarr/destination.json) and filesystem checks that keep every write on
// the right filesystem (safety rule S3), the overlap checks of S4, and the capability probe (§3).
//
// Jobs reach a filecopy destination only through Open, which returns a Handle holding an *os.Root
// on the target after checking the marker and the filesystem type; the filecopy engine
// (internal/engines/filecopy) then works through that root.
//
// Phase 4 adds restic and rclone destinations on local paths, SFTP, S3-compatible storage and
// Backblaze B2 (engine.go): kinds and strictly decoded remotes (remote.go), sealed write-only
// storage credentials and an encryption secret sealed once at create (credentials.go,
// encryption.go; S21, S22), create through a pending row and attach, Test and the host-key scan
// through the outbound guard (engine.go, hostkeys.go, b2.go; S25), the recovery kit and its
// custody (kit.go), engine settings, snapshot retention and bandwidth (engine_settings.go). Jobs
// of an engine destination read its location and secrets from one row with SecretsFor; Open
// refuses them (ErrEngineDestination).
package destinations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sl0wz3r/bunkarr/internal/config"
	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/bwlimit"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/logging"
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
	// ErrEngineDestination means filecopy-only code (Open, Test of a target) was given a restic
	// or rclone destination: jobs reach those through SecretsFor and the engine (phase4.md §13.2).
	ErrEngineDestination = errors.New("a restic or rclone destination is not a filecopy target")
	// ErrNotEngine means SecretsFor, RecoveryKit or TestStored was given a filecopy destination.
	ErrNotEngine = errors.New("not a restic or rclone destination")
	// ErrSecretNotConfirmed refuses deleting a destination whose encryption secret's recovery kit
	// custody was never confirmed without confirmLoseSecret (S21).
	ErrSecretNotConfirmed = errors.New("the recovery kit of this destination was never confirmed: deleting it deletes its encryption secret (confirm with confirmLoseSecret)")
	// ErrWrongCheckCode means a recovery kit confirmation's check code is wrong (§5.2).
	ErrWrongCheckCode = errors.New("wrong check code")
	// ErrWrongSecret means a confirmation by secret does not match the stored secret (§5.2).
	ErrWrongSecret = errors.New("the secret does not match")
)

// ValidationError is a user-facing input error.
type ValidationError string

func (e ValidationError) Error() string { return string(e) }

// Destination is a stored destination.
type Destination struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Engine string `json:"engine"`
	// Kind is the location type (phase4.md §4.1): local for every Phase 1-3 destination.
	Kind engines.DestKind `json:"kind"`
	// Target is the resolved (symlink-free) absolute path of a local destination, or a remote's
	// display location (sftp://user@host:22/path, s3:<endpoint>/<bucket>/<prefix>,
	// b2:<bucket>/<prefix>).
	Target  string `json:"target"`
	Enabled bool   `json:"enabled"`
	// SourceIDs are the linked sources, ascending.
	SourceIDs []int64 `json:"sourceIds"`
	// FSType is the filesystem type recorded at creation (filecopy.FSStat.Type), or a remote's
	// kind.
	FSType       string       `json:"fsType"`
	Capabilities Capabilities `json:"capabilities"`
	Settings     Settings     `json:"settings"`
	Retention    Retention    `json:"retention"`
	// Remote is a remote kind's normalized, non-secret location ({} for local).
	Remote engines.Remote `json:"remote"`
	// HasCredentials names the storage credential fields that are stored (never a value, §4.3).
	HasCredentials map[string]bool `json:"hasCredentials"`
	// Encryption is the encryption mode and the recovery kit's custody (S21, §5.2).
	Encryption EncryptionInfo `json:"encryption"`
	// Bandwidth is the destination's limits, timetable and transfer window (§9.1).
	Bandwidth bwlimit.Config `json:"bandwidth"`
	// Pending is set while a create that initialized a repository has not finished (marker_id
	// "pending:<uuid>", §4.5): the row keeps its sealed secret, runs no job and can be finished
	// by creating the same location again, or deleted.
	Pending bool `json:"pending"`
	// BlockedReason says why jobs are refused (Blocked); the API adds the engine's availability.
	BlockedReason string `json:"blockedReason,omitempty"`
	// Warnings are the warnings of the create or update that returned this value (a non-empty
	// remote, another Bunkarr's recent snapshots on attach, an unrestricted B2 key, an http
	// endpoint). They are not stored.
	Warnings []string `json:"warnings,omitempty"`
	// EngineTag is a restic destination's random tag (D32).
	EngineTag string `json:"-"`
	// MarkerID is the id in the target's .bunkarr/destination.json (filecopy), "restic:<id>", the
	// rclone marker's id, or "pending:<uuid>".
	MarkerID string `json:"-"`
	// RootDev is the target's st_dev at creation (diagnostics only: network filesystems get a
	// new device number on every mount, so it is not compared).
	RootDev   uint64    `json:"-"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// EncryptionInfo is a destination's encryption and recovery-kit custody (S21, §5.2, §12).
type EncryptionInfo struct {
	Mode engines.EncryptionMode `json:"mode"`
	// Origin is who chose the secret: "generated", "user", or "" (no secret).
	Origin string `json:"origin"`
	// KitExportedAt is the last recovery kit export; KitConfirmedAt when custody was confirmed
	// (the kit's check code, the user's secret typed again, or attach).
	KitExportedAt  *time.Time `json:"kitExportedAt"`
	KitConfirmedAt *time.Time `json:"kitConfirmedAt"`
}

// Secret origins (destinations.secret_origin).
const (
	OriginGenerated = "generated"
	OriginUser      = "user"
)

// IsEngine reports whether the destination is a restic or rclone destination.
func (d Destination) IsEngine() bool { return d.Engine == EngineRestic || d.Engine == EngineRclone }

// HasSecret reports whether the destination has an encryption secret (restic, rclone crypt).
func (d Destination) HasSecret() bool { return d.Encryption.Origin != "" }

// Blocked texts (Blocked).
const (
	BlockedKit     = "export and confirm the recovery kit first"
	BlockedPending = "create did not finish"
)

// Blocked returns why no job but a dry run may run for d, or "" (S21, S25; §11.1): a create that
// did not finish, or an encryption secret whose recovery kit custody was never confirmed. The
// engine's availability is the caller's (the API's) to add.
func Blocked(d Destination) string {
	switch {
	case d.Pending:
		return BlockedPending
	case d.HasSecret() && d.Encryption.KitConfirmedAt == nil:
		return BlockedKit
	}
	return ""
}

// EngineDestination returns the engine-neutral view of d (engines.Destination). Only SecretsFor
// pairs it with secrets.
func (d Destination) EngineDestination() engines.Destination {
	e := engines.Destination{ID: d.ID, Name: d.Name, Engine: engines.Kind(d.Engine), Kind: d.Kind, Target: d.Target, Remote: d.Remote,
		MarkerID: d.MarkerID, EngineTag: d.EngineTag, Encryption: d.Encryption.Mode, FSType: d.FSType,
		Transfers: d.Settings.Transfers, Bandwidth: d.Bandwidth}
	if d.Settings.Restic != nil {
		e.PackSizeMiB = d.Settings.Restic.PackSizeMiB
	}
	return e
}

// Input is a destination to create or the changes to one. On Update, empty or nil fields keep
// the stored value; kind, engine, target, the remote's location and the encryption cannot change
// after creation.
type Input struct {
	Name string `json:"name"`
	// Kind defaults to local.
	Kind engines.DestKind `json:"kind"`
	// Engine defaults to "filecopy" for local; a remote kind needs restic or rclone.
	Engine string `json:"engine"`
	// Target is the absolute path of a local destination directory (a mounted share). It must
	// exist.
	Target string `json:"target"`
	// Remote is a remote kind's location (phase4.md §4.2), decoded strictly per kind.
	Remote json.RawMessage `json:"remote"`
	// Credentials are the storage credentials (write-only, §4.3); on Update the fields sent
	// replace the stored ones.
	Credentials *CredentialsInput `json:"credentials"`
	// Encryption chooses the encryption at create (S21); it cannot change afterwards.
	Encryption *EncryptionInput `json:"encryption"`
	// Bandwidth is the limits, timetable and window (§9.1).
	Bandwidth *bwlimit.Config `json:"bandwidth"`
	Enabled   *bool           `json:"enabled"`
	// Settings and Retention default to their engine's defaults; zero values inside take the
	// defaults too.
	Settings  *Settings  `json:"settings"`
	Retention *Retention `json:"retention"`
	// SourceIDs replaces the linked sources when not nil.
	SourceIDs []int64 `json:"sourceIds"`
}

// CreateOptions are the confirmations of design S3.
type CreateOptions struct {
	// Attach adopts an existing marker's id (filecopy, rclone) or an existing restic repository
	// instead of refusing the target.
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

	// Engine destinations (phase4.md §4, §5).
	//
	// Keyring seals and opens the credentials and encryption_secret columns (ADR 0003); an engine
	// destination cannot be created or used without it.
	Keyring *config.Keyring
	// ObscureKey is rclone.Obscure's IV key; nil derives it from Keyring
	// (Keyring.Derive(rclone.ObscureKeyInfo, 32)).
	ObscureKey []byte
	// Engines returns the driver of an engine kind when it is available (the binary was found
	// and is recent enough); nil means none is. Create, attach and Test go through it.
	Engines func(engines.Kind) (engines.Engine, bool)
	// CheckHost refuses a host the engine would dial when it is, or resolves to, a metadata or
	// link-local address (netguard; the API wires it). nil uses netguard.CheckHost.
	CheckHost func(ctx context.Context, host string) error
	// LookupHost resolves an http S3 endpoint's host (only loopback or private addresses may use
	// http); nil uses the default resolver.
	LookupHost func(ctx context.Context, host string) ([]netip.Addr, error)
	// HTTPClient calls b2_authorize_account; nil uses a client whose dials pass netguard.
	HTTPClient *http.Client
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
	// b2AuthURL is b2_authorize_account's URL (tests point it at a local server).
	b2AuthURL string
	// scanHostKeys is ScanHostKeys (tests replace it).
	scanHostKeys func(ctx context.Context, host string, port int) ([]engines.HostKeyInfo, error)
	// createBudget bounds a create or test of an engine destination (S26: 60 s).
	createBudget time.Duration
}

// New returns a Store over d.
func New(d *db.DB, o Options) *Store {
	s := &Store{db: d, opts: o, log: o.Log, now: o.Now, statFS: filecopy.StatFS, lstat: o.Lstat, b2AuthURL: b2AuthorizeURL,
		createBudget: engineCallBudget}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.lstat == nil {
		s.lstat = filecopy.Lstat
	}
	s.scanHostKeys = s.ScanHostKeys
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

const selectCols = `id, name, engine, target, marker_id, settings, retention, fs_type, root_dev, capabilities, enabled, created_at,
	updated_at, kind, remote, encryption, secret_origin, kit_exported_at, kit_confirmed_at, engine_tag, bandwidth, credentials`

type rowScanner interface {
	Scan(dest ...any) error
}

// destRow is one scanned destinations row (selectCols).
type destRow struct {
	d                                Destination
	caps, created, updated           string
	remote, encryption, bandwidth    string
	origin, exported, confirmed, tag sql.NullString
	rootDev                          int64
	// credentials is the sealed credentials column ("" = none).
	credentials string
}

// targets returns the scan targets of selectCols; settings and retention receive their JSON.
func (r *destRow) targets(settings, retention *string) []any {
	return []any{&r.d.ID, &r.d.Name, &r.d.Engine, &r.d.Target, &r.d.MarkerID, settings, retention, &r.d.FSType, &r.rootDev, &r.caps,
		&r.d.Enabled, &r.created, &r.updated, &r.d.Kind, &r.remote, &r.encryption, &r.origin, &r.exported, &r.confirmed, &r.tag,
		&r.bandwidth, &r.credentials}
}

// destination decodes the scanned row.
func (r *destRow) destination(settings, retention string) (Destination, error) {
	d := r.d
	d.RootDev = uint64(r.rootDev)
	var err error
	if d.Settings, err = ParseSettingsFor(settings, d.Engine, d.Kind); err != nil {
		return Destination{}, fmt.Errorf("destination %d: %w", d.ID, err)
	}
	if d.Retention, err = ParseRetentionFor(retention, d.Engine); err != nil {
		return Destination{}, fmt.Errorf("destination %d: %w", d.ID, err)
	}
	if r.caps != "" {
		if err := json.Unmarshal([]byte(r.caps), &d.Capabilities); err != nil {
			return Destination{}, fmt.Errorf("destination %d: parse capabilities: %w", d.ID, err)
		}
	}
	if d.CreatedAt, err = db.ParseTime(r.created); err != nil {
		return Destination{}, fmt.Errorf("destination %d: created_at: %w", d.ID, err)
	}
	if d.UpdatedAt, err = db.ParseTime(r.updated); err != nil {
		return Destination{}, fmt.Errorf("destination %d: updated_at: %w", d.ID, err)
	}
	if d.Remote, err = parseStoredRemote(r.remote, d.Kind); err != nil {
		return Destination{}, fmt.Errorf("destination %d: %w", d.ID, err)
	}
	if d.Bandwidth, err = parseBandwidth(r.bandwidth); err != nil {
		return Destination{}, fmt.Errorf("destination %d: %w", d.ID, err)
	}
	d.Encryption = EncryptionInfo{Mode: engines.EncryptionMode(r.encryption), Origin: r.origin.String}
	for _, t := range []struct {
		src sql.NullString
		dst **time.Time
	}{{r.exported, &d.Encryption.KitExportedAt}, {r.confirmed, &d.Encryption.KitConfirmedAt}} {
		if t.src.Valid {
			v, err := db.ParseTime(t.src.String)
			if err != nil {
				return Destination{}, fmt.Errorf("destination %d: kit time: %w", d.ID, err)
			}
			*t.dst = &v
		}
	}
	d.EngineTag = r.tag.String
	d.Pending = strings.HasPrefix(d.MarkerID, pendingPrefix)
	d.HasCredentials = map[string]bool{}
	d.SourceIDs = []int64{}
	d.BlockedReason = Blocked(d)
	return d, nil
}

// parseBandwidth reads a destinations.bandwidth column ("{}" and "" are no limits).
func parseBandwidth(raw string) (bwlimit.Config, error) {
	var c bwlimit.Config
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			return bwlimit.Config{}, fmt.Errorf("parse bandwidth: %w", err)
		}
	}
	n, err := c.Normalize()
	if err != nil {
		return bwlimit.Config{}, fmt.Errorf("parse bandwidth: %w", err)
	}
	return n, nil
}

func (s *Store) scanDestination(r rowScanner) (Destination, error) {
	var (
		row                 destRow
		settings, retention string
	)
	if err := r.Scan(row.targets(&settings, &retention)...); err != nil {
		return Destination{}, err
	}
	d, err := row.destination(settings, retention)
	if err != nil {
		return Destination{}, err
	}
	if row.credentials != "" {
		// hasCredentials: which fields are stored, never a value. A column that does not open
		// (another bunkarr.key) shows none; jobs report the error.
		if c, err := s.openCredentials(d.ID, row.credentials); err != nil {
			s.log.Warn("could not open a destination's credentials", "id", d.ID, "error", err.Error())
		} else {
			d.HasCredentials = c.Fields()
		}
	}
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
		d, err := s.scanDestination(rows)
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
	d, err := s.scanDestination(s.db.Reader().QueryRowContext(ctx, `SELECT `+selectCols+` FROM destinations WHERE id = ?`, id))
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

// Update changes a destination's name, enabled flag, settings, retention, bandwidth and linked
// sources and, for an engine destination, its storage credentials, an sftp remote's hostKeys and
// an s3 remote's caCert (phase4.md §4.3, §12). A different kind, engine, target or remote
// location, and any change of the encryption, are refused: a destination cannot move (its records
// describe what is at that location); create a new one instead. New credentials, host keys or a
// CA certificate are tested against the stored location first (the S25 check must pass with
// them), and the credentials are merged field by field inside the write transaction, so
// concurrent updates keep each other's fields. No statement here writes encryption_secret.
func (s *Store) Update(ctx context.Context, id int64, in Input) (Destination, error) {
	cur, err := s.Get(ctx, id)
	if err != nil {
		return Destination{}, err
	}
	if in.Kind != "" && in.Kind != cur.Kind {
		return Destination{}, ValidationError("the kind of a destination cannot be changed; create a new destination instead")
	}
	if in.Target != "" {
		if cur.Kind == engines.Local && !sameTarget(in.Target, cur.Target) || cur.Kind != engines.Local && in.Target != cur.Target {
			return Destination{}, ValidationError("the target of a destination cannot be changed; create a new destination instead")
		}
	}
	if in.Engine != "" && in.Engine != cur.Engine {
		return Destination{}, ValidationError("the engine of a destination cannot be changed")
	}
	if in.Encryption != nil && !in.Encryption.keeps(cur.Encryption.Mode) {
		return Destination{}, ValidationError("the encryption of a destination cannot be changed")
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
	if enabled && cur.Pending {
		return Destination{}, ValidationError("the create of this destination did not finish: create it again with the same location to finish it, or delete it")
	}
	settings := cur.Settings
	if in.Settings != nil {
		if settings, err = in.Settings.NormalizeFor(cur.Engine, cur.Kind); err != nil {
			return Destination{}, err
		}
	}
	retention := cur.Retention
	if in.Retention != nil {
		if retention, err = in.Retention.NormalizeFor(cur.Engine); err != nil {
			return Destination{}, err
		}
	}
	sj, rj, err := marshalConfig(settings, retention)
	if err != nil {
		return Destination{}, err
	}
	var bw sql.NullString
	if in.Bandwidth != nil {
		if bw, err = bandwidthJSON(*in.Bandwidth); err != nil {
			return Destination{}, err
		}
	}
	remote, remoteChanged, warnings, err := s.updatedRemote(ctx, cur, in.Remote)
	if err != nil {
		return Destination{}, err
	}
	var rj2 sql.NullString
	if remoteChanged {
		raw, err := json.Marshal(remote)
		if err != nil {
			return Destination{}, fmt.Errorf("encode remote: %w", err)
		}
		rj2 = sql.NullString{String: string(raw), Valid: true}
	}
	if err := in.Credentials.check(); err != nil {
		return Destination{}, err
	}
	var creds engines.Credentials
	if in.Credentials != nil {
		if !cur.IsEngine() {
			return Destination{}, ValidationError("credentials: a filecopy destination has no storage credentials")
		}
		creds = in.Credentials.Credentials
		if err := checkCredentialFields(cur.Kind, creds); err != nil {
			return Destination{}, err
		}
	}
	if in.Credentials != nil || remoteChanged {
		w, err := s.testUpdate(ctx, cur, remote, creds)
		if err != nil {
			return Destination{}, err
		}
		warnings = append(warnings, w...)
	}
	err = s.db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE destinations SET name = ?, enabled = ?, settings = ?, retention = ?,
			bandwidth = COALESCE(?, bandwidth), remote = COALESCE(?, remote), updated_at = ? WHERE id = ?`,
			name, enabled, sj, rj, bw, rj2, db.FormatTime(s.now()), id)
		if err != nil {
			return mapConstraint(err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if in.Credentials != nil {
			if err := s.mergeCredentialsTx(ctx, tx, id, cur.Kind, creds); err != nil {
				return err
			}
		}
		if in.SourceIDs != nil {
			return setSourcesTx(ctx, tx, id, in.SourceIDs)
		}
		return nil
	})
	if err != nil {
		return Destination{}, err
	}
	if in.Credentials != nil {
		if _, sec, err := s.SecretsFor(ctx, id); err == nil {
			registerSecrets(id, sec)
		} else {
			s.log.Error("could not register a destination's secrets with the log redaction", "id", id, "error", err.Error())
		}
	}
	d, err := s.Get(ctx, id)
	if err != nil {
		return Destination{}, err
	}
	d.Warnings = warnings
	return d, nil
}

// mergeCredentialsTx unseals destination id's stored credentials, replaces the fields of update,
// checks the result and seals it again, inside the caller's write transaction (§4.3).
func (s *Store) mergeCredentialsTx(ctx context.Context, tx *sql.Tx, id int64, kind engines.DestKind, update engines.Credentials) error {
	var sealed string
	if err := tx.QueryRowContext(ctx, `SELECT credentials FROM destinations WHERE id = ?`, id).Scan(&sealed); err != nil {
		return fmt.Errorf("read destination %d credentials: %w", id, err)
	}
	stored, err := s.openCredentials(id, sealed)
	if err != nil {
		return err
	}
	merged := mergeCredentials(kind, stored, update)
	if err := checkCompleteCredentials(kind, merged); err != nil {
		return err
	}
	resealed, err := s.sealCredentials(id, merged)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE destinations SET credentials = ? WHERE id = ?`, resealed, id); err != nil {
		return fmt.Errorf("store destination %d credentials: %w", id, err)
	}
	return nil
}

// bandwidthJSON validates a bandwidth configuration and returns its stored JSON.
func bandwidthJSON(c bwlimit.Config) (sql.NullString, error) {
	n, err := c.Normalize()
	if err != nil {
		var ve *bwlimit.ValidationError
		if errors.As(err, &ve) {
			return sql.NullString{}, ValidationError("bandwidth." + ve.Error())
		}
		return sql.NullString{}, ValidationError("bandwidth: " + err.Error())
	}
	raw, err := json.Marshal(n)
	if err != nil {
		return sql.NullString{}, fmt.Errorf("encode bandwidth: %w", err)
	}
	return sql.NullString{String: string(raw), Valid: true}, nil
}

// DeleteOptions are the confirmations of Delete.
type DeleteOptions struct {
	// ConfirmLoseSecret deletes a destination whose encryption secret's custody was never
	// confirmed: the secret is deleted with the row (S21).
	ConfirmLoseSecret bool `json:"confirmLoseSecret"`
}

// Delete removes a destination's row, its source links and (by cascade) its file records and
// snapshot records. Nothing at the target is touched: backup data and the marker stay (a new
// destination on the same target needs Attach). A destination with an encryption secret whose
// recovery kit custody was never confirmed is refused with ErrSecretNotConfirmed unless
// opts.ConfirmLoseSecret (at most one DeleteOptions is used).
func (s *Store) Delete(ctx context.Context, id int64, opts ...DeleteOptions) error {
	var o DeleteOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		var hasSecret, confirmed bool
		err := tx.QueryRowContext(ctx, `SELECT encryption_secret IS NOT NULL, kit_confirmed_at IS NOT NULL FROM destinations WHERE id = ?`, id).
			Scan(&hasSecret, &confirmed)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("delete destination %d: %w", id, err)
		}
		if hasSecret && !confirmed && !o.ConfirmLoseSecret {
			return ErrSecretNotConfirmed
		}
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
	if err == nil {
		logging.SetSecrets(secretOwner(id))
	}
	return err
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
