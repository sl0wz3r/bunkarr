package destinations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
)

// Handle is an opened destination: a job's only way to the target (safety rules S2, S3). Every
// write goes through Root, which cannot leave the target and keeps pointing at the filesystem
// that was checked even if the share is unmounted or remounted later.
type Handle struct {
	Destination  Destination
	Root         *os.Root
	Settings     Settings
	Retention    Retention
	Capabilities Capabilities

	store *Store
	// identityFirst is the report of a check CheckIdentityFirst deferred (nil: none pending).
	identityFirst func(IdentityCheck, error)
}

// Open loads destination id, opens an os.Root on its target and checks, through that root, that
// the marker is present and carries the destination's id and that the filesystem type is the one
// recorded at creation (S3). Failures return ErrNotMounted, ErrMarkerMismatch or ErrFSChanged
// (all saying "destination not mounted?"); nothing is written, not even for the check. The
// caller closes the Handle.
func (s *Store) Open(ctx context.Context, id int64) (*Handle, error) {
	d, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(d.Target)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
			return nil, fmt.Errorf("destination %q: %w: %w", d.Name, err, ErrNotMounted)
		}
		return nil, fmt.Errorf("destination %q: open target %s: %w", d.Name, d.Target, err)
	}
	h := &Handle{Destination: d, Root: root, Settings: d.Settings, Retention: d.Retention, Capabilities: d.Capabilities, store: s}
	if err := h.Recheck(); err != nil {
		_ = root.Close()
		return nil, err
	}
	return h, nil
}

// Recheck repeats Open's marker and filesystem checks through the handle's root (cheap: one
// small read and one statfs). Jobs call it every 500 files and before retention or expiry work
// (S3).
func (h *Handle) Recheck() error {
	return h.store.check(h.Root, h.Destination)
}

// Close closes the root.
func (h *Handle) Close() error {
	return h.Root.Close()
}

// Lstat returns the metadata of rel inside the target without following a final symlink
// (filecopy.Lstat). Where two names may be one file, jobs compare what it returns only as far as
// Capabilities.InodeIdentity allows.
func (h *Handle) Lstat(rel string) (filecopy.Stat, error) {
	return h.store.lstat(h.Root, rel)
}

// RefreshStale runs the capability probe again when the handle's capabilities were found by an
// older probe (Capabilities.Current is false: a destination created before the probe checked
// its inode numbers) and stores the result, as RefreshCapabilities does; current capabilities
// are left alone. refreshed reports whether the probe ran; warnings are the probe's. Like every
// probe it writes only inside .bunkarr/probe of the (already checked) target.
func (h *Handle) RefreshStale(ctx context.Context) (refreshed bool, warnings []string, err error) {
	if h.Capabilities.Current() {
		return false, nil, nil
	}
	if _, warnings, err = h.store.refresh(ctx, h); err != nil {
		return false, nil, err
	}
	return true, warnings, nil
}

// IdentityCheck is what Handle.CheckIdentity did.
type IdentityCheck struct {
	// Stored is what the stored capabilities said before the check.
	Stored Capabilities
	// Probed reports that they were from an older probe and the whole probe ran again (RefreshStale).
	Probed bool
	// Changed reports that UnstableInodes differs from Stored (the new capabilities are stored).
	Changed bool
	// Warnings are the probe's (for instance a run directory it could not remove).
	Warnings []string
}

// CheckIdentity decides whether the job holding the handle may trust the destination's inode
// numbers (Capabilities.InodeIdentity): jobs run it (directly, or deferred with
// CheckIdentityFirst) before they compare two names at the destination. Capabilities from an
// older probe are probed again as a whole (RefreshStale);
// current ones only have the probe's identity checks run again (hardlinked names and, on a
// case-insensitive destination, two spellings must stat as one file), because a probe's finding
// does not last: a CIFS share remounted with noserverino, or a CIFS client that turns server
// inode numbers off at run time, numbers the inode of each lookup itself from then on. The
// handle's capabilities take the result for the job, and a changed UnstableInodes is stored.
// When the check fails (or its result cannot be stored) the handle's capabilities stop trusting
// inode numbers for the job (DistrustInodes; nothing is stored) and the error is returned. Like
// every probe it writes only inside .bunkarr/probe of the (already checked) target.
func (h *Handle) CheckIdentity(ctx context.Context) (IdentityCheck, error) {
	c := IdentityCheck{Stored: h.Capabilities}
	if !h.Capabilities.Current() {
		refreshed, warnings, err := h.RefreshStale(ctx)
		if err != nil {
			h.DistrustInodes()
			return c, err
		}
		c.Probed, c.Warnings = refreshed, warnings
		c.Changed = h.Capabilities.UnstableInodes != c.Stored.UnstableInodes
		return c, nil
	}
	unstable, warnings, err := probeIdentity(ctx, h.Root, h.Capabilities, h.store.lstat)
	c.Warnings = warnings
	if err != nil {
		h.DistrustInodes()
		return c, err
	}
	if unstable == h.Capabilities.UnstableInodes {
		return c, nil
	}
	if err := h.store.setUnstableInodes(ctx, h.Destination.ID, unstable); err != nil {
		h.DistrustInodes()
		return c, err
	}
	h.Capabilities.UnstableInodes = unstable
	h.Destination.Capabilities.UnstableInodes = unstable
	c.Changed = true
	return c, nil
}

// CheckIdentityFirst defers CheckIdentity to the first time the job holding the handle asks
// InodeIdentity, which then hands the check's outcome to report (the job logs it) before it
// answers. A job that never compares two names at the destination does not check, and writes
// nothing for it: a sync that fails its source checks leaves the destination as it was.
func (h *Handle) CheckIdentityFirst(report func(IdentityCheck, error)) {
	h.identityFirst = report
}

// InodeIdentity reports whether the job holding the handle may tell by device and inode number
// whether two names at the destination are one file (Capabilities.InodeIdentity). The first call
// runs the check CheckIdentityFirst deferred; only a cancelled check returns an error (the job
// then trusts no inode numbers). Jobs ask this, never Capabilities.InodeIdentity, before they
// compare two names.
func (h *Handle) InodeIdentity(ctx context.Context) (bool, error) {
	if report := h.identityFirst; report != nil {
		h.identityFirst = nil
		c, err := h.CheckIdentity(ctx)
		if err != nil && ctx.Err() != nil {
			return false, ctx.Err()
		}
		report(c, err)
	}
	return h.Capabilities.InodeIdentity(), nil
}

// DistrustInodes makes the handle's capabilities say that inode numbers do not identify files
// (UnstableInodes), for the job holding it only: nothing is stored. The job then tells whether two
// names are one file by their content. A dry run, which writes nothing and so cannot check, does
// so; so does a job whose check failed.
func (h *Handle) DistrustInodes() {
	h.Capabilities.UnstableInodes = true
}

// setUnstableInodes stores unstable as destination id's UnstableInodes, leaving the other
// capabilities as they are stored.
func (s *Store) setUnstableInodes(ctx context.Context, id int64, unstable bool) error {
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		var raw string
		if err := tx.QueryRowContext(ctx, `SELECT capabilities FROM destinations WHERE id = ?`, id).Scan(&raw); err != nil {
			return fmt.Errorf("load capabilities: %w", err)
		}
		var caps Capabilities
		if raw != "" {
			if err := json.Unmarshal([]byte(raw), &caps); err != nil {
				return fmt.Errorf("decode capabilities: %w", err)
			}
		}
		caps.UnstableInodes = unstable
		enc, err := json.Marshal(caps)
		if err != nil {
			return fmt.Errorf("encode capabilities: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE destinations SET capabilities = ?, updated_at = ? WHERE id = ?`, string(enc), db.FormatTime(s.now()), id); err != nil {
			return fmt.Errorf("store capabilities: %w", err)
		}
		return nil
	})
}

// check verifies the marker and filesystem type of d through root.
func (s *Store) check(root *os.Root, d Destination) error {
	m, present, err := readMarker(root)
	switch {
	case err != nil && !present:
		return fmt.Errorf("destination %q: %w", d.Name, err)
	case err != nil:
		return fmt.Errorf("destination %q at %s: %w: %v", d.Name, d.Target, ErrMarkerMismatch, err)
	case !present:
		return fmt.Errorf("destination %q: %s has no %s: %w", d.Name, d.Target, filecopy.MarkerRel, ErrNotMounted)
	case m.ID != d.MarkerID:
		return fmt.Errorf("destination %q: the marker at %s belongs to %q: %w", d.Name, d.Target, m.Name, ErrMarkerMismatch)
	}
	st, err := s.statFS(root)
	if err != nil {
		return fmt.Errorf("destination %q: %w", d.Name, err)
	}
	if st.Type != d.FSType {
		return fmt.Errorf("destination %q: %s is on %s, recorded %s: %w", d.Name, d.Target, st.Type, d.FSType, ErrFSChanged)
	}
	return nil
}

// RefreshCapabilities opens destination id (with all of Open's checks), runs the capability
// probe inside its .bunkarr/probe and stores the result.
func (s *Store) RefreshCapabilities(ctx context.Context, id int64) (Capabilities, error) {
	h, err := s.Open(ctx, id)
	if err != nil {
		return Capabilities{}, err
	}
	defer h.Close()
	caps, _, err := s.refresh(ctx, h)
	return caps, err
}

// refresh probes an opened destination and stores the capabilities.
func (s *Store) refresh(ctx context.Context, h *Handle) (Capabilities, []string, error) {
	caps, warnings, err := probeRoot(ctx, h.Root, s.now(), s.lstat)
	if err != nil {
		return Capabilities{}, nil, err
	}
	raw, err := json.Marshal(caps)
	if err != nil {
		return Capabilities{}, nil, fmt.Errorf("encode capabilities: %w", err)
	}
	err = s.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE destinations SET capabilities = ?, updated_at = ? WHERE id = ?`, string(raw), db.FormatTime(s.now()), h.Destination.ID)
		if err != nil {
			return fmt.Errorf("store capabilities: %w", err)
		}
		return nil
	})
	if err != nil {
		return Capabilities{}, nil, err
	}
	h.Capabilities = caps
	h.Destination.Capabilities = caps
	return caps, warnings, nil
}
