package destinations

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
)

// Marker states reported by Test.
const (
	// MarkerOK: the marker belongs to the destination being tested.
	MarkerOK = "ok"
	// MarkerMissing: there is no marker.
	MarkerMissing = "missing"
	// MarkerMismatch: the marker of an existing destination belongs to another one (or is
	// unreadable).
	MarkerMismatch = "mismatch"
	// MarkerForeign: a target being created already holds a Bunkarr marker (attach to use it).
	MarkerForeign = "foreign"
)

// TestResult is the answer of POST /destinations/test and /destinations/{id}/test (design §7).
type TestResult struct {
	// OK is false when something blocks using the target (missing, not a directory, overlap,
	// unreadable, not writable, marker missing or mismatching, filesystem changed). A local
	// filesystem or a foreign marker are warnings: Create accepts them with a confirmation.
	OK       bool   `json:"ok"`
	Marker   string `json:"marker"`
	Writable bool   `json:"writable"`
	FSType   string `json:"fsType"`
	Local    bool   `json:"local"`
	// Capabilities is set when the probe ran (only for an existing destination whose marker and
	// filesystem check out; a target being created is probed by Create).
	Capabilities *Capabilities `json:"capabilities"`
	FreeBytes    uint64        `json:"freeBytes"`
	TotalBytes   uint64        `json:"totalBytes"`
	// Entries is the number of entries at the top level of the target.
	Entries  int      `json:"entries"`
	Message  string   `json:"message"`
	Warnings []string `json:"warnings"`
}

// Test examines a target without changing anything except, for an existing destination whose
// marker and filesystem check out, the capability probe inside its .bunkarr/probe (whose result
// is stored). existingID 0 tests a target before creation: then target is required and nothing
// is written (writability is only estimated with access(2)). For an existing destination target
// may be "" (the stored target is used) but may not name another directory.
//
// Problems with the target are reported in the result; the error is for internal failures
// (database) and an unknown existingID (ErrNotFound).
func (s *Store) Test(ctx context.Context, target string, existingID int64) (TestResult, error) {
	res := TestResult{Marker: MarkerMissing, Warnings: []string{}}
	var d Destination
	if existingID != 0 {
		var err error
		if d, err = s.Get(ctx, existingID); err != nil {
			return TestResult{}, err
		}
		if target == "" {
			target = d.Target
		} else if !sameTarget(target, d.Target) {
			return TestResult{}, ValidationError("the target of a destination cannot be changed; test it without a target")
		}
	}
	fail := func(msg string) (TestResult, error) {
		res.OK = false
		res.Message = msg
		return res, nil
	}

	resolved, err := resolveTarget(target)
	if err != nil {
		var ve ValidationError
		if errors.As(err, &ve) {
			if existingID != 0 {
				return fail(string(ve) + " — destination not mounted?")
			}
			return fail(string(ve))
		}
		return TestResult{}, err
	}
	if existingID == 0 {
		if err := s.guard(ctx, resolved); err != nil {
			var ve ValidationError
			var ge *pathGuardError
			if errors.As(err, &ve) || errors.As(err, &ge) {
				return fail(err.Error())
			}
			return TestResult{}, err
		}
	}
	root, err := os.OpenRoot(resolved)
	if err != nil {
		return fail(fmt.Sprintf("cannot open %s: %v", resolved, err))
	}
	defer root.Close()

	st, err := s.statFS(root)
	if err != nil {
		return fail(fmt.Sprintf("cannot read the filesystem of %s: %v", resolved, err))
	}
	res.FSType, res.FreeBytes, res.TotalBytes = st.Type, st.Free, st.Total
	rs, err := filecopy.RootStat(root)
	if err != nil {
		return fail(fmt.Sprintf("cannot stat %s: %v", resolved, err))
	}
	res.Local = s.isLocal(st.Type, rs.Dev)
	if n, err := countEntries(root); err != nil {
		return fail(fmt.Sprintf("cannot list %s: %v", resolved, err))
	} else {
		res.Entries = n
	}

	m, present, merr := readMarker(root)
	if existingID == 0 {
		return s.testNew(ctx, res, resolved, m, present, merr)
	}

	switch {
	case merr != nil && !present:
		return fail(fmt.Sprintf("cannot read the marker: %v", merr))
	case merr != nil:
		res.Marker = MarkerMismatch
		return fail(fmt.Sprintf("the marker is unreadable (%v) — destination not mounted?", merr))
	case !present:
		res.Marker = MarkerMissing
		return fail(fmt.Sprintf("%s has no %s — destination not mounted?", resolved, filecopy.MarkerRel))
	case m.ID != d.MarkerID:
		res.Marker = MarkerMismatch
		return fail(fmt.Sprintf("the marker at %s belongs to %q — another share mounted here?", resolved, m.Name))
	}
	res.Marker = MarkerOK
	if st.Type != d.FSType {
		return fail(fmt.Sprintf("%s is on %s but was created on %s — destination not mounted?", resolved, st.Type, d.FSType))
	}
	h := &Handle{Destination: d, Root: root, Settings: d.Settings, Retention: d.Retention, Capabilities: d.Capabilities, store: s}
	caps, warnings, err := s.refresh(ctx, h)
	if err != nil {
		if ctx.Err() != nil {
			return TestResult{}, ctx.Err()
		}
		return fail(fmt.Sprintf("the probe failed: %v", err))
	}
	res.Writable = true
	res.Capabilities = &caps
	res.Warnings = append(res.Warnings, warnings...)
	res.Warnings = append(res.Warnings, capabilityWarnings(caps)...)
	res.OK = true
	res.Message = fmt.Sprintf("OK: %s, %s free", st.Type, humanBytes(st.Free))
	return res, nil
}

// testNew finishes Test for a target that is not a destination yet. It writes nothing.
func (s *Store) testNew(ctx context.Context, res TestResult, resolved string, m Marker, present bool, merr error) (TestResult, error) {
	res.Writable = unix.Access(resolved, unix.W_OK) == nil
	switch {
	case merr != nil && !present:
		res.OK, res.Message = false, fmt.Sprintf("cannot read the marker: %v", merr)
		return res, nil
	case merr != nil:
		res.Marker = MarkerForeign
		res.Warnings = append(res.Warnings, fmt.Sprintf("%s exists but is unreadable (%v); fix or remove it by hand", filecopy.MarkerRel, merr))
	case present:
		res.Marker = MarkerForeign
		other, err := s.byMarker(ctx, m.ID)
		if err != nil {
			return TestResult{}, err
		}
		if other != "" {
			res.OK, res.Message = false, fmt.Sprintf("this target is already destination %q", other)
			return res, nil
		}
		res.Warnings = append(res.Warnings, fmt.Sprintf("the target is already a Bunkarr destination (%q, created %s): create it with attach to use it", m.Name, m.CreatedAt.Format("2006-01-02")))
	case res.Entries > 0:
		res.Warnings = append(res.Warnings, fmt.Sprintf("the target is not empty (%d entries); Bunkarr only writes to .bunkarr and the folders of its sources", res.Entries))
	}
	if res.Local {
		res.Warnings = append(res.Warnings, fmt.Sprintf("the target is on a local filesystem (%s, or the disk holding the container or Bunkarr's config): a backup there does not survive a disk failure; create it with allowLocal to use it anyway", res.FSType))
	}
	if !res.Writable {
		res.OK, res.Message = false, fmt.Sprintf("%s is not writable by Bunkarr (check PUID/PGID and the share's permissions)", resolved)
		return res, nil
	}
	res.OK = true
	res.Message = fmt.Sprintf("OK: %s, %s free; capabilities are probed when the destination is created", res.FSType, humanBytes(res.FreeBytes))
	return res, nil
}

// capabilityWarnings explains capabilities that limit what the destination can store (S11).
func capabilityWarnings(c Capabilities) []string {
	var w []string
	if c.InvalidChars != "" || !c.TrailingDotSpace {
		w = append(w, fmt.Sprintf("names with %s or a trailing dot or space cannot be stored; use NFS or SMB with POSIX extensions (mapposix) for such libraries", quoteChars(c.InvalidChars)))
	}
	if c.CaseInsensitive {
		w = append(w, "the destination is case-insensitive: files whose paths differ only in case cannot both be stored")
	}
	if !c.Hardlinks {
		w = append(w, "the destination does not support hardlinks: each hardlinked file is stored once and its other names are only recorded")
	}
	if c.UnstableInodes {
		w = append(w, "the destination's inode numbers change from one lookup to the next (an SMB mount with noserverino?): Bunkarr decides by content whether two names are one file there, which costs extra reads when a job resumes an interrupted replacement or retains a hardlinked name")
	}
	return w
}

func quoteChars(chars string) string {
	if chars == "" {
		return "no special characters"
	}
	parts := make([]string, 0, len(chars))
	for _, c := range chars {
		parts = append(parts, string(c))
	}
	return strings.Join(parts, " ")
}

// countEntries counts the entries at the top of root.
func countEntries(root *os.Root) (int, error) {
	f, err := root.Open(".")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	n := 0
	for {
		names, err := f.Readdirnames(1024)
		n += len(names)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return n, nil
			}
			return n, err
		}
	}
}

// humanBytes renders a byte count with a binary unit.
func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
