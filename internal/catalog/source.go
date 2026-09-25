package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sl0wz3r/bunkarr/internal/db"
)

// Source is a media folder Bunkarr backs up (API: Source).
type Source struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// Path is absolute and resolved (no symlinks), as Bunkarr sees it inside the container.
	Path string `json:"path"`
	// DestFolder is the folder under each destination target that mirrors this source.
	DestFolder string `json:"destFolder"`
	// Exclude holds the source's own glob patterns (see DefaultExcludes for the built-in ones).
	// A pattern matches an entry's base name or its path relative to the source root; a leading
	// "/" matches only the relative path; a trailing "/" matches only directories.
	Exclude           []string `json:"exclude"`
	Enabled           bool     `json:"enabled"`
	PlexIntegrationID *int64   `json:"plexIntegrationId"`
	PlexSectionID     string   `json:"plexSectionId"`
	// PlexPath is the library location as Plex sees it.
	PlexPath         string `json:"plexPath"`
	ArrIntegrationID *int64 `json:"arrIntegrationId"`
	// FSType is the filesystem recorded by the last successful scan ("" before the first one and
	// after a save that clears the recorded identity, see Store.Update).
	FSType         string     `json:"fsType"`
	LastScanAt     *time.Time `json:"lastScanAt"`
	LastScanStatus string     `json:"lastScanStatus"`
	// Stats are the catalog counts as of the last successful scan.
	Stats     Stats     `json:"stats"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Last scan statuses (Source.LastScanStatus).
const (
	ScanStatusOK       = "ok"
	ScanStatusWarnings = "warnings"
	ScanStatusFailed   = "failed"
)

// Stats are catalog counts of live (not deleted) files.
type Stats struct {
	Files int64 `json:"files"`
	// Bytes counts every name; UniqueBytes counts each hardlink group once.
	Bytes           int64 `json:"bytes"`
	UniqueBytes     int64 `json:"uniqueBytes"`
	HardlinkGroups  int64 `json:"hardlinkGroups"`
	HardlinkedFiles int64 `json:"hardlinkedFiles"`
	// Skipped is the number of entries the last scan did not catalog: symlinks, devices, sockets,
	// FIFOs and directories that are a destination or the config directory (not excluded ones).
	Skipped int64 `json:"skipped"`
}

// SourceInput is what Create and Update accept.
type SourceInput struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// DestFolder "" means: on Create the slug of the name, on Update the current value.
	DestFolder string   `json:"destFolder"`
	Exclude    []string `json:"exclude"`
	// Enabled nil means: on Create true, on Update the current value.
	Enabled           *bool  `json:"enabled"`
	PlexIntegrationID *int64 `json:"plexIntegrationId"`
	PlexSectionID     string `json:"plexSectionId"`
	PlexPath          string `json:"plexPath"`
	ArrIntegrationID  *int64 `json:"arrIntegrationId"`
}

// Validation limits.
const (
	maxNameLen       = 64 // characters
	maxDestFolderLen = 1024
	maxSegmentLen    = 255
	maxPlexFieldLen  = 4096
)

// sourceColumns is the column list scanSource reads.
const sourceColumns = `id, name, path, dest_folder, exclude, enabled, plex_integration_id, plex_section_id,
	plex_path, arr_integration_id, fs_type, last_scan_at, last_scan_status, stats, created_at, updated_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanSource(r rowScanner) (Source, error) {
	var (
		src                                                    Source
		exclude, stats, createdAt, updatedAt                   string
		plexID, arrID                                          sql.NullInt64
		plexSection, plexPath, fsType, lastScanAt, lastScanStr sql.NullString
	)
	if err := r.Scan(&src.ID, &src.Name, &src.Path, &src.DestFolder, &exclude, &src.Enabled, &plexID,
		&plexSection, &plexPath, &arrID, &fsType, &lastScanAt, &lastScanStr, &stats, &createdAt, &updatedAt); err != nil {
		return Source{}, err
	}
	if err := json.Unmarshal([]byte(exclude), &src.Exclude); err != nil {
		return Source{}, fmt.Errorf("source %d: exclude: %w", src.ID, err)
	}
	if src.Exclude == nil {
		src.Exclude = []string{}
	}
	if err := json.Unmarshal([]byte(stats), &src.Stats); err != nil {
		return Source{}, fmt.Errorf("source %d: stats: %w", src.ID, err)
	}
	if plexID.Valid {
		v := plexID.Int64
		src.PlexIntegrationID = &v
	}
	if arrID.Valid {
		v := arrID.Int64
		src.ArrIntegrationID = &v
	}
	src.PlexSectionID = plexSection.String
	src.PlexPath = plexPath.String
	src.FSType = fsType.String
	src.LastScanStatus = lastScanStr.String
	if lastScanAt.Valid {
		t, err := db.ParseTime(lastScanAt.String)
		if err != nil {
			return Source{}, fmt.Errorf("source %d: last_scan_at: %w", src.ID, err)
		}
		src.LastScanAt = &t
	}
	var err error
	if src.CreatedAt, err = db.ParseTime(createdAt); err != nil {
		return Source{}, fmt.Errorf("source %d: created_at: %w", src.ID, err)
	}
	if src.UpdatedAt, err = db.ParseTime(updatedAt); err != nil {
		return Source{}, fmt.Errorf("source %d: updated_at: %w", src.ID, err)
	}
	return src, nil
}

// List returns every source, ordered by name.
func (s *Store) List(ctx context.Context) ([]Source, error) {
	rows, err := s.db.Reader().QueryContext(ctx, `SELECT `+sourceColumns+` FROM sources ORDER BY name COLLATE NOCASE, id`)
	if err != nil {
		return nil, fmt.Errorf("list sources: %w", err)
	}
	defer rows.Close()
	out := []Source{}
	for rows.Next() {
		src, err := scanSource(rows)
		if err != nil {
			return nil, fmt.Errorf("list sources: %w", err)
		}
		out = append(out, src)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list sources: %w", err)
	}
	return out, nil
}

// Get returns one source, or an error wrapping ErrNotFound.
func (s *Store) Get(ctx context.Context, id int64) (Source, error) {
	src, err := scanSource(s.db.Reader().QueryRowContext(ctx, `SELECT `+sourceColumns+` FROM sources WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Source{}, fmt.Errorf("source %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Source{}, fmt.Errorf("get source %d: %w", id, err)
	}
	return src, nil
}

// validSource is a validated SourceInput.
type validSource struct {
	name, path, destFolder string
	exclude                []string
	enabled                bool
	plexIntegrationID      *int64
	plexSectionID          *string
	plexPath               *string
	arrIntegrationID       *int64
}

// unchanged reports whether v holds exactly cur's settings (a plain re-save). The Plex section and
// path count only while the source is linked to Plex: deleting the integration unlinks the source
// but keeps them, and the web form does not send them for an unlinked source.
func (v validSource) unchanged(cur Source) bool {
	sameID := func(a, b *int64) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
	text := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	return v.name == cur.Name && v.path == cur.Path && v.destFolder == cur.DestFolder &&
		slices.Equal(v.exclude, cur.Exclude) && v.enabled == cur.Enabled &&
		sameID(v.plexIntegrationID, cur.PlexIntegrationID) && sameID(v.arrIntegrationID, cur.ArrIntegrationID) &&
		(cur.PlexIntegrationID == nil || text(v.plexSectionID) == cur.PlexSectionID && text(v.plexPath) == cur.PlexPath)
}

// validate checks in. cur is the stored source on Update, nil on Create.
func (s *Store) validate(ctx context.Context, in SourceInput, cur *Source) (validSource, error) {
	var v validSource
	var err error
	if v.name, err = validateName(in.Name); err != nil {
		return v, err
	}
	if cur != nil && filepath.Clean(strings.TrimSpace(in.Path)) == cur.Path {
		// Unchanged: it was resolved and checked when saved. Not re-checking it lets a source be
		// edited (e.g. disabled) while its share is unmounted.
		v.path = cur.Path
	} else if v.path, err = resolveSourcePath(in.Path); err != nil {
		return v, err
	}
	if s.opts.PathGuard != nil {
		if err := s.opts.PathGuard(ctx, v.path); err != nil {
			if ctx.Err() != nil {
				return v, ctx.Err()
			}
			return v, &ValidationError{Field: "path", Message: err.Error(), Err: err}
		}
	}
	v.destFolder = strings.TrimSpace(in.DestFolder)
	if v.destFolder == "" {
		if cur != nil {
			v.destFolder = cur.DestFolder
		} else if v.destFolder = Slug(v.name); v.destFolder == "" {
			return v, invalid("destFolder", "cannot derive a folder name from %q; set destFolder", v.name)
		}
	}
	if err := validateDestFolder(v.destFolder); err != nil {
		return v, err
	}
	if v.exclude, err = validateExcludes(in.Exclude); err != nil {
		return v, err
	}
	switch {
	case in.Enabled != nil:
		v.enabled = *in.Enabled
	case cur != nil:
		v.enabled = cur.Enabled
	default:
		v.enabled = true
	}
	v.plexIntegrationID = in.PlexIntegrationID
	v.arrIntegrationID = in.ArrIntegrationID
	if v.plexSectionID, err = optionalText("plexSectionId", in.PlexSectionID); err != nil {
		return v, err
	}
	if v.plexPath, err = optionalText("plexPath", in.PlexPath); err != nil {
		return v, err
	}
	return v, nil
}

func validateName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	n := utf8.RuneCountInString(name)
	switch {
	case n == 0:
		return "", invalid("name", "is required")
	case n > maxNameLen:
		return "", invalid("name", "must be at most %d characters", maxNameLen)
	case !utf8.ValidString(name):
		return "", invalid("name", "must be valid UTF-8")
	case strings.IndexFunc(name, unicode.IsControl) >= 0:
		return "", invalid("name", "must not contain control characters")
	}
	return name, nil
}

func optionalText(field, raw string) (*string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return nil, nil
	}
	if len(v) > maxPlexFieldLen || !utf8.ValidString(v) || strings.ContainsRune(v, 0) {
		return nil, invalid(field, "must be valid UTF-8 text of at most %d bytes", maxPlexFieldLen)
	}
	return &v, nil
}

// resolveSourcePath checks that p is an absolute path to an existing directory and returns it
// with every symlink resolved (safety rule S1: the stored path is the resolved one).
func resolveSourcePath(p string) (string, error) {
	switch {
	case strings.TrimSpace(p) == "":
		return "", invalid("path", "is required")
	case strings.ContainsRune(p, 0):
		return "", invalid("path", "must not contain a NUL byte")
	case !filepath.IsAbs(p):
		return "", invalid("path", "must be absolute (the path inside the Bunkarr container), got %q", p)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(p))
	if err != nil {
		return "", pathError(p, err)
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		return "", pathError(p, err)
	}
	if !fi.IsDir() {
		return "", invalid("path", "%s is not a directory", p)
	}
	if resolved == string(filepath.Separator) {
		return "", invalid("path", "the filesystem root cannot be a source")
	}
	return resolved, nil
}

func pathError(p string, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return &ValidationError{Field: "path", Message: fmt.Sprintf("%s does not exist (is the volume mounted into the container?)", p), Err: err}
	case errors.Is(err, fs.ErrPermission):
		return &ValidationError{Field: "path", Message: fmt.Sprintf("%s is not accessible: permission denied (check PUID/PGID)", p), Err: err}
	default:
		return &ValidationError{Field: "path", Message: fmt.Sprintf("cannot use %s: %v", p, err), Err: err}
	}
}

// Slug derives the default destination folder from a source name: lowercase, spaces to "-",
// only [a-z0-9._-] kept, no leading or trailing "." or "-". It may return "".
func Slug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_':
			b.WriteRune(r)
			dash = false
		case r == '-' || unicode.IsSpace(r):
			if !dash {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	return strings.Trim(b.String(), ".-")
}

// validateDestFolder checks a destination folder: a clean relative path of one or more segments,
// none of them empty, "..", starting with "." (reserved for .bunkarr and temp files), ending in a
// dot or space, or holding characters SMB shares cannot store.
func validateDestFolder(df string) error {
	switch {
	case len(df) > maxDestFolderLen:
		return invalid("destFolder", "must be at most %d bytes", maxDestFolderLen)
	case !utf8.ValidString(df):
		return invalid("destFolder", "must be valid UTF-8")
	case strings.HasPrefix(df, "/"):
		return invalid("destFolder", "must be a relative path, got %q", df)
	case path.Clean(df) != df:
		return invalid("destFolder", "must be a clean relative path (no \"./\", \"//\" or trailing \"/\"), got %q", df)
	}
	for _, seg := range strings.Split(df, "/") {
		switch {
		case seg == "..":
			return invalid("destFolder", "must not contain \"..\"")
		case strings.HasPrefix(seg, "."):
			return invalid("destFolder", "folder names must not start with \".\" (reserved for Bunkarr's own files), got %q", seg)
		case len(seg) > maxSegmentLen:
			return invalid("destFolder", "folder names must be at most %d bytes", maxSegmentLen)
		case strings.TrimSpace(seg) != seg, strings.HasSuffix(seg, "."):
			return invalid("destFolder", "folder names must not start or end with a space or end with \".\", got %q", seg)
		case strings.ContainsAny(seg, `\:*?"<>|`):
			return invalid("destFolder", "folder names must not contain any of \\ : * ? \" < > |, got %q", seg)
		case strings.IndexFunc(seg, unicode.IsControl) >= 0:
			return invalid("destFolder", "must not contain control characters")
		}
	}
	return nil
}

// destFoldersOverlap reports whether two destination folders are equal or one contains the other,
// ignoring case (a case-insensitive destination would merge them).
func destFoldersOverlap(a, b string) bool {
	la, lb := strings.ToLower(a), strings.ToLower(b)
	return la == lb || strings.HasPrefix(la, lb+"/") || strings.HasPrefix(lb, la+"/")
}

// checkUnique refuses a name or destination folder another source (not selfID) already uses.
// It runs inside the write transaction, so it cannot race another save.
func checkUnique(ctx context.Context, tx *sql.Tx, selfID int64, name, destFolder string) error {
	rows, err := tx.QueryContext(ctx, `SELECT name, dest_folder FROM sources WHERE id <> ?`, selfID)
	if err != nil {
		return fmt.Errorf("check source uniqueness: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var otherName, otherFolder string
		if err := rows.Scan(&otherName, &otherFolder); err != nil {
			return fmt.Errorf("check source uniqueness: %w", err)
		}
		if strings.EqualFold(otherName, name) {
			return conflict("a source named %q already exists", otherName)
		}
		if destFoldersOverlap(otherFolder, destFolder) {
			return conflict("destFolder %q overlaps the destFolder %q of source %q", destFolder, otherFolder, otherName)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("check source uniqueness: %w", err)
	}
	return nil
}

// SQLite extended result codes (stable ABI) for constraint failures.
const (
	sqliteConstraintForeignKey = 787
	sqliteConstraintUnique     = 2067
)

// mapConstraint turns constraint failures into API errors.
func mapConstraint(err error) error {
	var c interface{ Code() int }
	if !errors.As(err, &c) {
		return err
	}
	switch c.Code() {
	case sqliteConstraintForeignKey:
		return &ValidationError{Field: "plexIntegrationId", Message: "plexIntegrationId or arrIntegrationId does not refer to an existing integration", Err: err}
	case sqliteConstraintUnique:
		return &conflictError{msg: "another source already uses this name or destFolder"}
	}
	return err
}

func marshalExclude(ex []string) string {
	b, _ := json.Marshal(ex) // a []string always marshals
	return string(b)
}

// Create validates in and stores a new source.
func (s *Store) Create(ctx context.Context, in SourceInput) (Source, error) {
	v, err := s.validate(ctx, in, nil)
	if err != nil {
		return Source{}, err
	}
	var id int64
	err = s.db.Write(ctx, func(tx *sql.Tx) error {
		if err := checkUnique(ctx, tx, 0, v.name, v.destFolder); err != nil {
			return err
		}
		now := db.FormatTime(time.Now())
		res, err := tx.ExecContext(ctx, `INSERT INTO sources (name, path, dest_folder, exclude, enabled,
			plex_integration_id, plex_section_id, plex_path, arr_integration_id, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			v.name, v.path, v.destFolder, marshalExclude(v.exclude), v.enabled,
			v.plexIntegrationID, v.plexSectionID, v.plexPath, v.arrIntegrationID, now, now)
		if err != nil {
			return fmt.Errorf("create source: %w", mapConstraint(err))
		}
		id, err = res.LastInsertId()
		if err != nil {
			return fmt.Errorf("create source: %w", err)
		}
		return nil
	})
	if err != nil {
		return Source{}, err
	}
	return s.Get(ctx, id)
}

// identity is a source's recorded filesystem identity (safety rule S10a).
type identity struct {
	fsType  sql.NullString
	rootDev sql.NullInt64
	rootIno sql.NullInt64
}

func (s *Store) identity(ctx context.Context, id int64) (identity, error) {
	var ident identity
	err := s.db.Reader().QueryRowContext(ctx, `SELECT fs_type, root_dev, root_ino FROM sources WHERE id = ?`, id).
		Scan(&ident.fsType, &ident.rootDev, &ident.rootIno)
	if errors.Is(err, sql.ErrNoRows) {
		return ident, fmt.Errorf("source %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return ident, fmt.Errorf("source %d identity: %w", id, err)
	}
	return ident, nil
}

// Update validates in and replaces the source's settings. The filesystem identity the last scan
// recorded (design S10a) is kept, so scans and a later path change are still checked against it
// after an edit. It is cleared, and the next successful scan records it again, by a plain re-save
// (no setting changed) while the path exists, which is how a deliberate filesystem change a scan
// refused is accepted, and by a path change while no destination holds backups. While destinations
// hold backups of the source (StoreOptions.HasBackups), destFolder cannot change and path may only
// change to the same directory (same device and inode as recorded, e.g. a renamed mount point; see
// checkSameRoot for anonymous devices); both refusals wrap ErrConflict. Changing path or
// destFolder also needs the source's lock to be free (no scan running) and, asked while holding
// it, StoreOptions.ActiveJobs to report no queued or running jobs for the source (ErrConflict
// otherwise).
func (s *Store) Update(ctx context.Context, id int64, in SourceInput) (Source, error) {
	cur, err := s.Get(ctx, id)
	if err != nil {
		return Source{}, err
	}
	v, err := s.validate(ctx, in, &cur)
	if err != nil {
		return Source{}, err
	}
	pathChanged := v.path != cur.Path
	folderChanged := v.destFolder != cur.DestFolder
	clearIdentity := false
	if pathChanged || folderChanged {
		// A scan or a sync's scan-and-plan must not see the source change under it.
		unlock, ok := s.locks.tryLock(id)
		if !ok {
			return Source{}, conflict("source %q is being scanned or synced; change its path or destFolder when that finishes", cur.Name)
		}
		defer unlock()
		// A sync executes its plan without the lock, before its first backup makes HasBackups true.
		active, err := s.activeJobs(ctx, id)
		if err != nil {
			return Source{}, err
		}
		if active {
			return Source{}, conflict("source %q has queued or running jobs; change its path or destFolder when they finish", cur.Name)
		}
		has, err := s.hasBackups(ctx, id)
		if err != nil {
			return Source{}, err
		}
		if has && folderChanged {
			return Source{}, conflict("destFolder cannot change: destinations already hold backups of source %q in %q", cur.Name, cur.DestFolder)
		}
		if has && pathChanged {
			if err := s.checkSameRoot(ctx, cur, v.path); err != nil {
				return Source{}, err
			}
		}
		// Without backups the new path may be any directory.
		clearIdentity = pathChanged && !has
	} else if v.unchanged(cur) {
		// A plain re-save accepts what is at the path now; with nothing there there is nothing to
		// accept, and the recorded root is what a later path change is checked against.
		if _, err := os.Lstat(cur.Path); err == nil {
			clearIdentity = true
		}
	}
	clearSQL := ""
	if clearIdentity {
		clearSQL = "fs_type = NULL, root_dev = NULL, root_ino = NULL, "
	}
	err = s.db.Write(ctx, func(tx *sql.Tx) error {
		if err := checkUnique(ctx, tx, id, v.name, v.destFolder); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE sources SET name = ?, path = ?, dest_folder = ?, exclude = ?,
			enabled = ?, plex_integration_id = ?, plex_section_id = ?, plex_path = ?, arr_integration_id = ?,
			`+clearSQL+`updated_at = ? WHERE id = ?`,
			v.name, v.path, v.destFolder, marshalExclude(v.exclude), v.enabled, v.plexIntegrationID,
			v.plexSectionID, v.plexPath, v.arrIntegrationID, db.FormatTime(time.Now()), id)
		if err != nil {
			return fmt.Errorf("update source %d: %w", id, mapConstraint(err))
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("update source %d: %w", id, err)
		} else if n == 0 {
			return fmt.Errorf("source %d: %w", id, ErrNotFound)
		}
		return nil
	})
	if err != nil {
		return Source{}, err
	}
	return s.Get(ctx, id)
}

// activeJobs asks StoreOptions.ActiveJobs; the caller holds the source's lock.
func (s *Store) activeJobs(ctx context.Context, id int64) (bool, error) {
	if s.opts.ActiveJobs == nil {
		return false, nil
	}
	active, err := s.opts.ActiveJobs(ctx, id)
	if err != nil {
		return false, fmt.Errorf("check jobs of source %d: %w", id, err)
	}
	return active, nil
}

func (s *Store) hasBackups(ctx context.Context, id int64) (bool, error) {
	if s.opts.HasBackups == nil {
		return false, nil
	}
	has, err := s.opts.HasBackups(ctx, id)
	if err != nil {
		return false, fmt.Errorf("check backups of source %d: %w", id, err)
	}
	return has, nil
}

// checkSameRoot allows a backed-up source's path to change only to the same directory: the new
// path's (dev, ino) must equal the root recorded by the last scan or, when none is recorded, the
// current path's. Where inode numbers survive a remount (stableRootIno: disk filesystems, NFS,
// btrfs, ZFS) nothing else is accepted, since a snapshot, clone or replica has the same root and
// file inodes, sizes and mtimes on another device; after a remount that renumbered the device the
// source is scanned at its current path first. An anonymous device with run-time inode numbers
// (FUSE such as Unraid's /mnt/user, CIFS/SMB) keeps neither number across a remount, and the old
// path of a renamed mount point is gone, so the source cannot be scanned there first: a directory
// on another such device of the same filesystem type is recognised instead by the catalogued files
// it holds (holdsCatalog), but only while the current path no longer holds the source (a directory
// elsewhere is then a copy): it must not hold the recorded root or the catalogued files, nor be on
// a filesystem of the recorded type (a remounted source whose files were renamed since the scan).
func (s *Store) checkSameRoot(ctx context.Context, cur Source, newPath string) error {
	nid, err := pathIdentity(newPath)
	if err != nil {
		return pathError(newPath, err)
	}
	if s.identityHook != nil {
		nid = s.identityHook(nid)
	}
	differs := conflict("path cannot change to a different directory: destinations hold backups of source %q (only the same directory under a new path, such as a renamed mount point, is accepted)", cur.Name)
	ident, err := s.identity(ctx, cur.ID)
	if err != nil {
		return err
	}
	if !ident.rootDev.Valid || !ident.rootIno.Valid {
		ofi, err := os.Stat(cur.Path)
		if err != nil {
			return conflict("path cannot change: destinations hold backups of source %q and its current path %s cannot be checked (%v); scan it once first", cur.Name, cur.Path, err)
		}
		om, ok := MetaOf(ofi)
		if !ok {
			return conflict("path cannot change: destinations hold backups of source %q and its current path cannot be identified", cur.Name)
		}
		if nid.dev != om.Dev || nid.ino != om.Inode {
			return differs
		}
		return nil
	}
	recDev, recIno := uint64(ident.rootDev.Int64), uint64(ident.rootIno.Int64)
	switch {
	case nid.dev == recDev && nid.ino == recIno:
		return nil
	case nid.dev == recDev || !nid.anonDev || nid.fsType != ident.fsType.String:
		return differs
	case nid.stableIno:
		return conflict("path cannot change: destinations hold backups of source %q and %s is on another device than its last scan recorded (%d, recorded %d); %s keeps inode numbers across a remount, so only the same device and inode are accepted (a snapshot or clone has the same inodes on another device). If the filesystem was remounted, scan the source at %s first, then change its path",
			cur.Name, newPath, nid.dev, recDev, nid.fsType, cur.Path)
	}
	// Run-time inode numbers on a remounted anonymous device: the catalogued files identify it.
	sample, err := s.catalogSample(ctx, cur.ID)
	if err != nil {
		return err
	}
	if ofi, err := os.Stat(cur.Path); err == nil {
		om, ok := MetaOf(ofi)
		held := ok && om.Dev == recDev && om.Inode == recIno
		if !held {
			if held, err = holdsCatalog(cur.Path, sample); err != nil {
				return conflict("path cannot change: destinations hold backups of source %q and its current path %s cannot be checked (%v)", cur.Name, cur.Path, err)
			}
		}
		if held {
			return conflict("path cannot change: destinations hold backups of source %q and its current path %s still holds it, so %s is another directory (a copy); unmount %s first if both show the same share",
				cur.Name, cur.Path, newPath, cur.Path)
		}
		// Its files may have been renamed or replaced since the last scan (a copy made before then
		// holds them instead), so while the current path is still on a filesystem of the recorded
		// type the remounted source is taken to be there.
		fsType, _, err := fsTypeOfPath(cur.Path)
		if err != nil {
			return conflict("path cannot change: destinations hold backups of source %q and its current path %s cannot be checked (%v)", cur.Name, cur.Path, err)
		}
		if fsType == ident.fsType.String {
			return conflict("path cannot change: destinations hold backups of source %q and its current path %s is still on %s, the filesystem type of its last scan, so %s is taken to be another directory (a copy); unmount %s first, or remove it if it is an empty directory left behind",
				cur.Name, cur.Path, fsType, newPath, cur.Path)
		}
	}
	held, err := holdsCatalog(newPath, sample)
	if err != nil {
		return pathError(newPath, err)
	}
	if !held {
		return conflict("path cannot change: destinations hold backups of source %q and %s does not hold its catalogued files (its %s device number changed since the last scan, so the directory is recognised by its files)",
			cur.Name, newPath, nid.fsType)
	}
	return nil
}

// pathIdentity returns the filesystem identity of the directory at p, as a scan records it.
func pathIdentity(p string) (rootIdentity, error) {
	root, err := os.OpenRoot(p)
	if err != nil {
		return rootIdentity{}, err
	}
	defer root.Close()
	return rootIdentityOf(root)
}

// sameRootSample is how many catalogued files holdsCatalog looks up.
const sameRootSample = 16

// sampleFile is a catalogued file holdsCatalog looks up.
type sampleFile struct {
	rel         string
	size, mtime int64
}

// catalogSample returns the source's oldest live catalog rows, up to sameRootSample.
func (s *Store) catalogSample(ctx context.Context, id int64) ([]sampleFile, error) {
	rows, err := s.db.Reader().QueryContext(ctx, `SELECT rel_path, size, mtime_ns FROM catalog_files
		WHERE source_id = ? AND deleted_at IS NULL ORDER BY id LIMIT ?`, id, sameRootSample)
	if err != nil {
		return nil, fmt.Errorf("source %d catalog: %w", id, err)
	}
	defer rows.Close()
	var sample []sampleFile
	for rows.Next() {
		var f sampleFile
		if err := rows.Scan(&f.rel, &f.size, &f.mtime); err != nil {
			return nil, fmt.Errorf("source %d catalog: %w", id, err)
		}
		sample = append(sample, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("source %d catalog: %w", id, err)
	}
	return sample, nil
}

// holdsCatalog reports whether the directory at root holds the sampled catalogued files: at least
// half of them must be regular files there with their recorded size and mtime. Files changed or
// deleted since the last scan may miss; another directory misses nearly all of them.
func holdsCatalog(root string, sample []sampleFile) (bool, error) {
	dir, err := os.OpenRoot(root)
	if err != nil {
		return false, err
	}
	defer dir.Close()
	found := 0
	for _, f := range sample {
		fi, err := dir.Lstat(filepath.FromSlash(f.rel))
		if err != nil {
			continue
		}
		m, ok := MetaOf(fi)
		if ok && fi.Mode().IsRegular() && m.Size == f.size && m.MtimeNs == f.mtime {
			found++
		}
	}
	return found > 0 && 2*found >= len(sample), nil
}

// Delete removes a source and its catalog. Backups at destinations are kept (their records lose
// the source link). It fails with ErrConflict while the source's lock is held (a scan, or a sync
// scanning and planning it) and, asked while holding the lock, while StoreOptions.ActiveJobs
// reports queued or running jobs for it (a sync executes its plan without the lock).
func (s *Store) Delete(ctx context.Context, id int64) error {
	unlock, ok := s.locks.tryLock(id)
	if !ok {
		return conflict("source %d is being scanned or synced; delete it when that finishes", id)
	}
	defer unlock()
	active, err := s.activeJobs(ctx, id)
	if err != nil {
		return err
	}
	if active {
		return conflict("source %d has queued or running jobs; cancel them or wait until they finish, then delete it", id)
	}
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM sources WHERE id = ?`, id)
		if err != nil {
			return fmt.Errorf("delete source %d: %w", id, err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("delete source %d: %w", id, err)
		} else if n == 0 {
			return fmt.Errorf("source %d: %w", id, ErrNotFound)
		}
		return nil
	})
}

// TestSource is TestSource(path) plus the PathGuard check (safety rule S4): a path the guard
// rejects is not ok.
func (s *Store) TestSource(ctx context.Context, p string) TestResult {
	res := TestSource(p)
	if !res.OK || s.opts.PathGuard == nil {
		return res
	}
	if err := s.opts.PathGuard(ctx, res.Path); err != nil {
		res.OK = false
		res.Message = err.Error()
	}
	return res
}
