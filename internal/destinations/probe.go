package destinations

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
)

// probeMtimes are the times the mtime probe sets (design §3: nanoseconds 123456789): an odd and
// an even second, because a filesystem rounding up to 1 s and one rounding up to 2 s (FAT) read
// back the same value for an odd second.
var probeMtimes = []time.Time{time.Unix(1_700_000_001, 123_456_789), time.Unix(1_700_000_000, 123_456_789)}

// granularities are the mtime resolutions the probe recognizes, finest first.
var granularities = []int64{1, 10, 100, 1e3, 1e4, 1e5, 1e6, 1e7, 1e8, 1e9, 2e9}

// Probe runs the capability probe (design §3) on target. It writes only inside
// <target>/.bunkarr/probe/ (created if needed) and removes what it wrote; callers use it only on
// a destination's own target or one being created (Create, Test and RefreshCapabilities do).
func Probe(ctx context.Context, target string) (Capabilities, error) {
	root, err := os.OpenRoot(target)
	if err != nil {
		return Capabilities{}, fmt.Errorf("probe %s: %w", target, err)
	}
	defer root.Close()
	caps, _, err := probeRoot(ctx, root, time.Now(), filecopy.Lstat)
	return caps, err
}

// lstatFunc stats a path inside a root (filecopy.Lstat, or Options.Lstat).
type lstatFunc func(root *os.Root, rel string) (filecopy.Stat, error)

// probeRoot probes the filesystem of root inside a fresh run directory under .bunkarr/probe/,
// statting through lstat. warnings describe oddities that do not fail the probe.
func probeRoot(ctx context.Context, root *os.Root, now time.Time, lstat lstatFunc) (caps Capabilities, warnings []string, err error) {
	st, err := filecopy.StatFS(root)
	if err != nil {
		return Capabilities{}, nil, err
	}
	caps.FSType = st.Type
	var unstableLinks, unstableCase bool
	cleanup, err := inRunDir(root, func(pr *os.Root) error {
		steps := []func() error{
			func() (err error) { caps.Hardlinks, unstableLinks, err = probeHardlinks(pr, lstat); return err },
			func() (err error) { caps.CaseInsensitive, unstableCase, err = probeCase(pr, lstat); return err },
			func() (err error) { caps.InvalidChars, err = probeChars(pr); return err },
			func() (err error) { caps.TrailingDotSpace, err = probeTrailing(pr); return err },
			func() error {
				g, exact, err := probeGranularity(pr)
				if err != nil {
					return err
				}
				caps.MtimeGranularityNs = g
				if !exact {
					warnings = append(warnings, "the destination does not store modification times reliably; adoption by size+mtime may not match")
				}
				return nil
			},
		}
		for _, step := range steps {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := step(); err != nil {
				return fmt.Errorf("probe %s: %w", caps.FSType, err)
			}
		}
		return nil
	})
	warnings = append(warnings, cleanup...)
	if err != nil {
		return Capabilities{}, warnings, err
	}
	caps.UnstableInodes = unstableLinks || unstableCase
	caps.CheckedAt = now.UTC()
	caps.ProbeVersion = filecopy.ProbeVersion
	return caps, warnings, nil
}

// inRunDir runs fn on a fresh run directory under .bunkarr/probe/ of root (created if needed) and
// removes the run directory afterwards; warnings say when that failed.
func inRunDir(root *os.Root, fn func(pr *os.Root) error) (warnings []string, err error) {
	if err := root.MkdirAll(filecopy.ProbeDir, filecopy.DefaultDirPerm); err != nil {
		return nil, fmt.Errorf("the target is not writable: create %s: %w", filecopy.ProbeDir, err)
	}
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	run := filecopy.ProbeDir + "/run-" + hex.EncodeToString(rnd[:])
	if err := root.Mkdir(run, 0o700); err != nil {
		return nil, fmt.Errorf("the target is not writable: create %s: %w", run, err)
	}
	defer func() {
		if rerr := root.RemoveAll(run); rerr != nil {
			warnings = append(warnings, fmt.Sprintf("could not remove the probe directory %s: %v", run, rerr))
		}
	}()
	pr, err := root.OpenRoot(run)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", run, err)
	}
	defer pr.Close() // runs before the RemoveAll above: some SMB servers keep open directories
	return nil, fn(pr)
}

// probeIdentity runs the probe's identity checks again on root (probeHardlinks and probeCase, in a
// fresh run directory under .bunkarr/probe/) for a destination whose capabilities are caps, and
// reports whether its inode numbers are unstable now: whether the names of one file stat as other
// files. That can change after the probe: a CIFS share remounted with noserverino, or a CIFS
// client that turns server inode numbers off at run time (it does when it finds them unreliable),
// numbers the inode of each lookup itself from then on. Each check that could compare two names
// of one file counts; when neither could (no hardlinks and a case-sensitive destination) the
// stored caps.UnstableInodes stands. A destination whose capabilities say it has hardlinks but
// where the check cannot make one is an error: its inode numbers cannot be checked.
func probeIdentity(ctx context.Context, root *os.Root, caps Capabilities, lstat lstatFunc) (unstable bool, warnings []string, err error) {
	if err := ctx.Err(); err != nil {
		return false, nil, err
	}
	warnings, err = inRunDir(root, func(pr *os.Root) error {
		hardlinks, unstableLinks, err := probeHardlinks(pr, lstat)
		if err != nil {
			return fmt.Errorf("check hardlinks: %w", err)
		}
		if caps.Hardlinks && !hardlinks && !unstableLinks {
			return errors.New("check hardlinks: the destination no longer makes hardlinks (its capabilities say it does); test it to probe it again")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		insensitive, unstableCase, err := probeCase(pr, lstat)
		if err != nil {
			return fmt.Errorf("check case spellings: %w", err)
		}
		switch {
		case hardlinks || unstableLinks || insensitive:
			unstable = unstableLinks || unstableCase
		default:
			unstable = caps.UnstableInodes
		}
		return nil
	})
	return unstable, warnings, err
}

// createProbeFile creates name exclusively with data.
func createProbeFile(r *os.Root, name string, data []byte) error {
	f, err := r.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	return errors.Join(werr, f.Close())
}

// probeHardlinks links a file to a second name and checks that the second name shares its
// content (hardlinks). It also checks whether inode numbers identify the file: every name — the
// first, the second and a third linked from the second after the content changed (on a network
// client a name nothing looked up before) — must stat with the same device and inode number and a
// link count of at least 2. unstable reports that link(2) works but they do not: a CIFS client
// mounted with noserverino drops the new name's dentry after link(2) and numbers the inode of
// each new lookup itself, so the names of one file show different numbers (and a link count of 1
// when the attributes come from a directory listing).
func probeHardlinks(r *os.Root, lstat lstatFunc) (hardlinks, unstable bool, err error) {
	if err := createProbeFile(r, "link-a", []byte("x")); err != nil {
		return false, false, fmt.Errorf("the target is not writable: %w", err)
	}
	if err := r.Link("link-a", "link-b"); err != nil {
		return false, false, nil
	}
	f, err := r.OpenFile("link-a", os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return false, false, err
	}
	_, werr := f.Write([]byte("y"))
	if err := errors.Join(werr, f.Close()); err != nil {
		return false, false, err
	}
	names := []string{"link-a", "link-b"}
	if r.Link("link-b", "link-c") == nil {
		names = append(names, "link-c")
	}
	shared, identity := false, true
	var first filecopy.Stat
	for i, name := range names {
		st, err := lstat(r, name)
		if err != nil {
			// A name link(2) just made cannot be looked up: nothing can be trusted about it.
			return false, true, nil
		}
		if i == 0 {
			first = st
		} else if st.Size == 2 {
			shared = true
		}
		identity = identity && st.Dev == first.Dev && st.Ino == first.Ino && st.Nlink >= 2
	}
	// Either observation proves a shared inode (a client may cache a name's size).
	return shared || identity, !identity, nil
}

// probeCase creates "A" and looks for "a" (insensitive when it is found). On a case-insensitive
// destination both spellings name one file, so they must stat with the same device and inode
// number; unstable reports that they do not (a CIFS client mounted with noserverino numbers the
// inode of each lookup itself).
func probeCase(r *os.Root, lstat lstatFunc) (insensitive, unstable bool, err error) {
	if err := createProbeFile(r, "A", nil); err != nil {
		return false, false, err
	}
	lower, err := lstat(r, "a")
	if errors.Is(err, fs.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	upper, err := lstat(r, "A")
	if err != nil {
		return false, false, err
	}
	return true, upper.Dev != lower.Dev || upper.Ino != lower.Ino, nil
}

// probeChars returns the characters of filecopy.ProbeChars a name cannot hold.
func probeChars(r *os.Root) (string, error) {
	var invalid []rune
	for _, c := range filecopy.ProbeChars {
		ok, err := roundTrip(r, "c"+string(c)+"x")
		if err != nil {
			return "", err
		}
		if !ok {
			invalid = append(invalid, c)
		}
	}
	return string(invalid), nil
}

// probeTrailing reports whether names ending in a dot or a space are kept.
func probeTrailing(r *os.Root) (bool, error) {
	for _, name := range []string{"t.", "s "} {
		ok, err := roundTrip(r, name)
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

// roundTrip creates name and reports whether the directory then lists exactly that name (a
// server may reject it, strip it or store it as something else, like an SMB stream for "a:b").
// Only errors that mean the storage itself failed are returned.
func roundTrip(r *os.Root, name string) (bool, error) {
	if err := createProbeFile(r, name, nil); err != nil {
		if filecopy.Classify(err) == filecopy.Fatal {
			return false, err
		}
		return false, nil
	}
	defer r.Remove(name)
	d, err := r.Open(".")
	if err != nil {
		return false, err
	}
	names, err := d.Readdirnames(-1)
	cerr := d.Close()
	if err := errors.Join(err, cerr); err != nil {
		return false, err
	}
	return slices.Contains(names, name), nil
}

// probeGranularity sets each of probeMtimes on a file and derives the mtime resolution from
// what is read back (the coarsest one seen). exact is false when a value read back fits no known
// resolution (then 2 s is assumed).
func probeGranularity(r *os.Root) (g int64, exact bool, err error) {
	if err := createProbeFile(r, "mtime", []byte("x")); err != nil {
		return 0, false, err
	}
	exact = true
	for _, t := range probeMtimes {
		if err := r.Chtimes("mtime", t, t); err != nil {
			return 0, false, fmt.Errorf("the destination does not allow setting modification times: %w", err)
		}
		fi, err := r.Lstat("mtime")
		if err != nil {
			return 0, false, err
		}
		gi, ok := granularity(t.UnixNano(), fi.ModTime().UnixNano())
		g = max(g, gi)
		exact = exact && ok
	}
	return g, exact, nil
}

// granularity returns the finest resolution g at which the time set reads back as got, whether
// the filesystem truncated or rounded up.
func granularity(set, got int64) (int64, bool) {
	for _, g := range granularities {
		lo := set - set%g
		if got == lo || got == lo+g {
			return g, true
		}
	}
	return granularities[len(granularities)-1], false
}
