package destinations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
)

// localFSTypes are filesystem types that are always local to the container or host (S3).
var localFSTypes = []string{"tmpfs", "ramfs", "overlay", "rootfs"}

// isLocal reports whether a filesystem type or device marks a local filesystem (S3).
func (s *Store) isLocal(fsType string, dev uint64) bool {
	return slices.Contains(localFSTypes, fsType) || slices.Contains(s.opts.LocalDevs, dev)
}

// resolveTarget checks that target is an absolute path of an existing directory and returns it
// with symlinks resolved. It never creates anything.
func resolveTarget(target string) (string, error) {
	if target == "" {
		return "", ValidationError("a destination needs a target directory")
	}
	if !filepath.IsAbs(target) {
		return "", ValidationError(fmt.Sprintf("target %q must be an absolute path", target))
	}
	fi, err := os.Stat(target)
	if errors.Is(err, fs.ErrNotExist) {
		return "", ValidationError(fmt.Sprintf("target %s does not exist: mount the share first (Bunkarr never creates the target or its parents)", target))
	}
	if err != nil {
		return "", ValidationError(fmt.Sprintf("target %s: %v", target, err))
	}
	if !fi.IsDir() {
		return "", ValidationError(fmt.Sprintf("target %s is not a directory", target))
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", ValidationError(fmt.Sprintf("target %s: %v", target, err))
	}
	return resolved, nil
}

// guard applies the overlap rules of S4 to a resolved target: not /, not overlapping the config
// directory or another destination's target, and the injected source check.
func (s *Store) guard(ctx context.Context, resolved string) error {
	if resolved == "/" {
		return ValidationError("the target may not be /")
	}
	if s.opts.ConfigDir != "" {
		cfg := filepath.Clean(s.opts.ConfigDir)
		if r, err := filepath.EvalSymlinks(cfg); err == nil {
			cfg = r
		}
		if overlaps(resolved, cfg) {
			return ValidationError(fmt.Sprintf("the target %s overlaps Bunkarr's config directory %s", resolved, cfg))
		}
	}
	all, err := s.List(ctx)
	if err != nil {
		return err
	}
	for _, d := range all {
		if overlaps(resolved, d.Target) {
			return ValidationError(fmt.Sprintf("the target %s overlaps destination %q (%s)", resolved, d.Name, d.Target))
		}
	}
	if s.opts.PathGuard != nil {
		if err := s.opts.PathGuard(ctx, resolved); err != nil {
			return &pathGuardError{err}
		}
	}
	return nil
}

// pathGuardError marks a refusal by Options.PathGuard; it unwraps to the guard's error.
type pathGuardError struct{ err error }

func (e *pathGuardError) Error() string { return e.err.Error() }
func (e *pathGuardError) Unwrap() error { return e.err }

// Create adds a destination (design S3). In order: the input is validated; the target must
// exist (Bunkarr never creates it or its parents) and is resolved with EvalSymlinks; the S4
// overlap checks run; a target that already has a marker is refused unless o.Attach (then the
// marker's id is reused and the marker is left as it is); a local filesystem is refused unless
// o.AllowLocal; the capability probe runs (inside .bunkarr/probe); the marker is written
// atomically; the row is inserted with marker_id, fs_type, root_dev and capabilities. When the
// insert fails, the marker written by this call is removed again.
func (s *Store) Create(ctx context.Context, in Input, o CreateOptions) (Destination, error) {
	s.createMu.Lock()
	defer s.createMu.Unlock()
	name, err := checkName(in.Name)
	if err != nil {
		return Destination{}, err
	}
	engine := in.Engine
	if engine == "" {
		engine = EngineFilecopy
	}
	if engine != EngineFilecopy {
		return Destination{}, ValidationError(fmt.Sprintf("engine %q is not available yet (only %q)", engine, EngineFilecopy))
	}
	settings, retention := DefaultSettings(), DefaultRetention()
	if in.Settings != nil {
		if settings, err = in.Settings.Normalize(); err != nil {
			return Destination{}, err
		}
	}
	if in.Retention != nil {
		if retention, err = in.Retention.Normalize(); err != nil {
			return Destination{}, err
		}
	}
	sj, rj, err := marshalConfig(settings, retention)
	if err != nil {
		return Destination{}, err
	}
	if taken, err := s.nameTaken(ctx, name); err != nil {
		return Destination{}, err
	} else if taken {
		return Destination{}, ErrNameTaken
	}

	resolved, err := resolveTarget(in.Target)
	if err != nil {
		return Destination{}, err
	}
	if err := s.guard(ctx, resolved); err != nil {
		return Destination{}, err
	}
	root, err := os.OpenRoot(resolved)
	if err != nil {
		return Destination{}, fmt.Errorf("open target %s: %w", resolved, err)
	}
	defer root.Close()

	marker, present, err := readMarker(root)
	if err != nil {
		return Destination{}, fmt.Errorf("%w: %v; remove or fix it by hand", ErrMarkerExists, err)
	}
	if present {
		if !o.Attach {
			return Destination{}, fmt.Errorf("%w (%q, created %s): create it with attach to use it", ErrMarkerExists, marker.Name, marker.CreatedAt.Format("2006-01-02"))
		}
		if other, err := s.byMarker(ctx, marker.ID); err != nil {
			return Destination{}, err
		} else if other != "" {
			return Destination{}, fmt.Errorf("%w: it is destination %q of this Bunkarr", ErrMarkerExists, other)
		}
	}

	fsStat, err := s.statFS(root)
	if err != nil {
		return Destination{}, err
	}
	rootStat, err := filecopy.RootStat(root)
	if err != nil {
		return Destination{}, fmt.Errorf("stat target %s: %w", resolved, err)
	}
	if s.isLocal(fsStat.Type, rootStat.Dev) && !o.AllowLocal {
		return Destination{}, fmt.Errorf("%w (%s, the same disk as the container or Bunkarr's config): a backup there does not survive a disk failure; set allowLocal to use it anyway", ErrLocalFilesystem, fsStat.Type)
	}

	_, metaErr := root.Lstat(filecopy.MetaDir)
	createdMeta := errors.Is(metaErr, fs.ErrNotExist)
	undoMeta := func() {
		if createdMeta {
			_ = root.RemoveAll(filecopy.ProbeDir)
			_ = root.Remove(filecopy.MetaDir) // only if empty
		}
	}
	caps, warnings, err := probeRoot(ctx, root, s.now(), s.lstat)
	if err != nil {
		undoMeta()
		return Destination{}, err
	}
	for _, w := range warnings {
		s.log.Warn("destination probe", "target", resolved, "warning", w)
	}
	capsJSON, err := json.Marshal(caps)
	if err != nil {
		undoMeta()
		return Destination{}, fmt.Errorf("encode capabilities: %w", err)
	}

	now := s.now()
	wroteMarker := false
	if !present {
		marker = Marker{ID: newUUID(), Name: name, CreatedAt: now.UTC()}
		if err := writeMarker(root, marker); err != nil {
			undoMeta()
			return Destination{}, err
		}
		wroteMarker = true
	}

	var id int64
	err = s.db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO destinations (name, engine, target, marker_id, settings, retention, fs_type, root_dev, capabilities, enabled, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			name, engine, resolved, marker.ID, sj, rj, fsStat.Type, int64(rootStat.Dev), string(capsJSON),
			in.Enabled == nil || *in.Enabled, db.FormatTime(now), db.FormatTime(now))
		if err != nil {
			return mapConstraint(err)
		}
		if id, err = res.LastInsertId(); err != nil {
			return fmt.Errorf("insert destination: %w", err)
		}
		return setSourcesTx(ctx, tx, id, in.SourceIDs)
	})
	if err != nil {
		if wroteMarker {
			if rerr := removeMarkerIfOurs(root, marker.ID); rerr != nil {
				s.log.Error("could not remove the marker of a destination that was not created", "target", resolved, "err", rerr)
			}
			undoMeta()
		}
		return Destination{}, err
	}
	s.log.Info("destination created", "id", id, "name", name, "target", resolved, "fsType", fsStat.Type, "attached", present)
	return s.Get(ctx, id)
}

// nameTaken reports whether a destination has name (case-insensitive).
func (s *Store) nameTaken(ctx context.Context, name string) (bool, error) {
	var one int
	err := s.db.Reader().QueryRowContext(ctx, `SELECT 1 FROM destinations WHERE name = ?`, name).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check destination name: %w", err)
	}
	return true, nil
}

// byMarker returns the name of the destination with marker id, or "".
func (s *Store) byMarker(ctx context.Context, markerID string) (string, error) {
	var name string
	err := s.db.Reader().QueryRowContext(ctx, `SELECT name FROM destinations WHERE marker_id = ?`, markerID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("look up marker: %w", err)
	}
	return name, nil
}
