package syncer

import (
	"context"
	"log/slog"
	"path"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// prepareIdentity runs first in every sync, verify and retention job of the destination h (before
// reconcile): it settles how the job tells whether two names at the destination are one file
// (h.InodeIdentity, which every comparison asks). What the probe found does not last: a share
// probed with stable inode numbers can be remounted with noserverino, and a CIFS client turns
// server inode numbers off at run time when it finds them unreliable. So a job that writes checks
// them again through its root before it first compares two names (Handle.CheckIdentityFirst; a
// job that never compares writes nothing for it), and uses the result for the rest of the job;
// capabilities of an older probe are probed again as a whole right away. A changed finding is
// stored and logged. A check that fails adds a warning to *warnings and the job trusts no inode
// numbers; only a cancelled job returns an error. A dry run writes nothing, so it cannot check:
// it trusts no inode numbers (it compares names by content, and stores nothing).
func prepareIdentity(ctx context.Context, h *destinations.Handle, rep jobs.Reporter, dryRun bool, warnings *int) error {
	if dryRun {
		h.DistrustInodes()
		rep.Log(slog.LevelInfo, "dry run: the destination's inode numbers are neither checked (that writes) nor trusted", "inodeIdentity", false)
		return nil
	}
	report := func(c destinations.IdentityCheck, err error) { reportIdentity(h, rep, c, err, warnings) }
	if h.Capabilities.Current() {
		h.CheckIdentityFirst(report)
		return nil
	}
	c, err := h.CheckIdentity(ctx)
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	report(c, err)
	return nil
}

// reportIdentity logs the outcome of a check of the destination's inode numbers (prepareIdentity)
// and counts a failed check in *warnings.
func reportIdentity(h *destinations.Handle, rep jobs.Reporter, c destinations.IdentityCheck, err error, warnings *int) {
	if err != nil {
		*warnings++
		rep.Log(slog.LevelWarn, "could not check whether the destination's inode numbers identify files; this job does not trust them (it compares names by content)",
			"error", err.Error())
		return
	}
	caps := h.Capabilities
	switch {
	case c.Probed:
		rep.Log(slog.LevelInfo, "probed the destination again (its capabilities were from an older version)", "hardlinks", caps.Hardlinks,
			"unstableInodes", caps.UnstableInodes, "caseInsensitive", caps.CaseInsensitive, "inodeIdentity", caps.InodeIdentity())
	case c.Changed && caps.UnstableInodes:
		rep.Log(slog.LevelInfo, "the destination's inode numbers no longer identify files (a share remounted with noserverino, or a CIFS client that turned server inode numbers off?): names are compared by content from now on",
			"inodeIdentity", false)
	case c.Changed:
		rep.Log(slog.LevelInfo, "the destination's inode numbers identify files again (a share remounted with serverino?): they are trusted from now on",
			"inodeIdentity", true)
	default:
		rep.Log(slog.LevelInfo, "checked the destination's inode numbers (unchanged)", "inodeIdentity", caps.InodeIdentity())
	}
	for _, w := range c.Warnings {
		rep.Log(slog.LevelInfo, "destination probe", "warning", w)
	}
}

// sameFile reports whether two stats are of one file by device and inode number. At the
// destination that holds only where Handle.InodeIdentity says so (see sameDestFile); the
// source's numbers are the catalog's own.
func sameFile(a, b filecopy.Stat) bool { return a.Dev == b.Dev && a.Ino == b.Ino }

// sameDestFile reports whether the destination paths a and b (stats sa and sb, from
// Handle.Lstat) are one file: hardlinks of each other. Where the destination's inode numbers
// identify files (Handle.InodeIdentity, checked for the job) that is sameFile. A CIFS client mounted with
// noserverino (UnstableInodes, as Unraid mounts shares) numbers the inode of each lookup itself,
// so one file shows other numbers under its two names; capabilities of an older probe do not say.
// There the names count as one file when they hold the same content: regular files of the same
// size and mtime with the same sha256 (both are read, but only names that agree in size and mtime
// get that far). Every caller asks in order to treat b as holding what a holds — finish a
// replacement whose old version b keeps, not displace a, keep a record that vouches for a's
// content — for which the same content is what matters.
func sameDestFile(ctx context.Context, h *destinations.Handle, a string, sa filecopy.Stat, b string, sb filecopy.Stat) (bool, error) {
	if id, err := h.InodeIdentity(ctx); err != nil {
		return false, err
	} else if id {
		return sameFile(sa, sb), nil
	}
	if !sa.Regular() || !sb.Regular() || sa.Size != sb.Size || sa.MtimeNs != sb.MtimeNs {
		return false, nil
	}
	ha, _, err := filecopy.HashFile(ctx, h.Root, a, nil)
	if err != nil {
		return false, sideErr(filecopy.SideDestination, "hash", a, err)
	}
	hb, _, err := filecopy.HashFile(ctx, h.Root, b, nil)
	if err != nil {
		return false, sideErr(filecopy.SideDestination, "hash", b, err)
	}
	return ha == hb, nil
}

// hardlinkOf reports whether the destination path a (stat sa) is a hardlink of b (stat sb), for a
// link item about to record a as b's hardlink. It is sameDestFile, except where inode numbers do
// not identify files: there a separate copy of b's content (an unmanaged file, or the copy a
// re-attached destination holds) would pass the content comparison, so the link count decides
// first. A CIFS client reports the server's count even when it numbers inodes itself, and a name
// whose count is 1 has no other name: it is not a hardlink of b, and the caller adopts or
// displaces it like any file in the way. (Only a count of 1 is evidence: a name with more may be
// linked to a third file, and an unknown count is 0. A client may also report 1 for a hardlink,
// from attributes a directory listing primed; that is a hardlink kept as a copy of its own, never
// a copy recorded as a hardlink. A name the live record already has as b's hardlink is compared
// with sameDestFile instead: the link item recorded it itself, and a count of 1 there would move
// the hardlink into retention and link it again.)
func hardlinkOf(ctx context.Context, h *destinations.Handle, a string, sa filecopy.Stat, b string, sb filecopy.Stat) (bool, error) {
	id, err := h.InodeIdentity(ctx)
	if err != nil {
		return false, err
	}
	if !id && (sa.Nlink == 1 || sb.Nlink == 1) {
		return false, nil
	}
	return sameDestFile(ctx, h, a, sa, b, sb)
}

// caseVariant reports whether the destination path to, which differs from from only in case on a
// case-insensitive destination and was found there (stats fromSt and toSt), is from's own file
// under another spelling rather than a file of its own. Where inode numbers identify files that
// is sameFile; elsewhere the two lookups show two numbers for one file, so to's directory is
// listed: to is from's file when the only entry whose name folds like to's is from's name.
func caseVariant(ctx context.Context, h *destinations.Handle, from string, fromSt filecopy.Stat, to string, toSt filecopy.Stat) (bool, error) {
	if id, err := h.InodeIdentity(ctx); err != nil {
		return false, err
	} else if id {
		return sameFile(fromSt, toSt), nil
	}
	dir := path.Dir(to)
	d, err := h.Root.Open(dir)
	if err != nil {
		return false, sideErr(filecopy.SideDestination, "open", dir, err)
	}
	names, err := d.Readdirnames(-1)
	_ = d.Close()
	if err != nil {
		return false, sideErr(filecopy.SideDestination, "read", dir, err)
	}
	key, own := filecopy.FoldKey(path.Base(to)), path.Base(from)
	found := false
	for _, n := range names {
		if filecopy.FoldKey(n) != key {
			continue
		}
		if n != own {
			return false, nil // another spelling has an entry of its own
		}
		found = true
	}
	return found, nil
}
