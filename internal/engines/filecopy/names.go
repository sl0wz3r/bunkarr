package filecopy

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// Capabilities is what the destination probe found out about a destination's filesystem (design
// §3), stored as JSON in destinations.capabilities and used by the planner (S11).
type Capabilities struct {
	// Hardlinks reports whether link(2) works (and really shares the content).
	Hardlinks bool `json:"hardlinks"`
	// UnstableInodes reports that device and inode numbers do not tell whether two names at the
	// destination are one file: a hardlinked name (or, on a case-insensitive destination, another
	// spelling of a name) stats with other numbers than the name it is one file with. A CIFS
	// client mounted with noserverino (as Unraid's Unassigned Devices mounts shares) numbers
	// inodes itself on each lookup. Hardlinks still work there; Bunkarr then tells them by
	// content (see InodeIdentity). A share can change after the probe (remounted with
	// noserverino, or a CIFS client that turns server inode numbers off at run time): a job that
	// writes checks this again before it first compares two names and stores a change.
	UnstableInodes bool `json:"unstableInodes"`
	// CaseInsensitive reports whether "A" and "a" name the same file.
	CaseInsensitive bool `json:"caseInsensitive"`
	// InvalidChars lists the characters of ProbeChars that cannot be stored in a name.
	InvalidChars string `json:"invalidChars"`
	// TrailingDotSpace reports whether names ending in "." or " " are stored as given.
	TrailingDotSpace bool `json:"trailingDotSpace"`
	// MtimeGranularityNs is the resolution of stored modification times (1 = nanoseconds).
	MtimeGranularityNs int64 `json:"mtimeGranularityNs"`
	// FSType is the filesystem type (FSStat.Type).
	FSType string `json:"fsType"`
	// CheckedAt is when the probe ran.
	CheckedAt time.Time `json:"checkedAt"`
	// ProbeVersion is the version of the probe that found these capabilities (0: a probe older
	// than ProbeVersion 1, which did not check UnstableInodes).
	ProbeVersion int `json:"probeVersion"`
}

// ProbeVersion is the version of the destination capability probe. Version 1 checks that the
// names of one file stat as one file (UnstableInodes).
const ProbeVersion = 1

// Current reports whether c was found by the current probe. Older capabilities do not say
// whether the destination's inode numbers can be trusted: a job that writes probes such a
// destination again before it relies on them.
func (c Capabilities) Current() bool { return c.ProbeVersion >= ProbeVersion }

// InodeIdentity reports whether device and inode numbers tell whether two names at the
// destination are one file: the current probe found them stable. Where they are not (or not
// known to be), two names are one file only as far as their content shows.
func (c Capabilities) InodeIdentity() bool { return c.Current() && !c.UnstableInodes }

// ProbeChars are the characters the destination probe tests (design §3): the ones SMB without
// POSIX extensions rejects.
const ProbeChars = `:?*"<>|\`

const (
	// MaxNameBytes is the longest name component (NAME_MAX; SMB counts UTF-16 units, which are
	// never more than the UTF-8 bytes).
	MaxNameBytes = 255
	// MaxPathBytes is the longest destination-relative path NameCheck accepts: PATH_MAX (4096)
	// less room for the retention prefix a retained copy gets.
	MaxPathBytes = 4000
)

// ErrNameNotStorable means the destination cannot store a name (S11): the item fails with a
// warning, it is never written under another name.
var ErrNameNotStorable = errors.New("name cannot be stored at the destination")

// NameCheck reports whether the destination-relative path rel can be stored at a destination
// with the given capabilities (S11): every component must be non-empty, not "." or "..", at most
// MaxNameBytes long, free of NUL and of the destination's invalid characters, without a trailing
// dot or space unless the destination keeps them, and not start with TempPrefix (Bunkarr's temp
// files); the first component may not be Bunkarr's MetaDir; the whole path is at most
// MaxPathBytes. Errors match ErrNameNotStorable.
func NameCheck(rel string, caps Capabilities) error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%w: %q: %s", ErrNameNotStorable, rel, fmt.Sprintf(format, args...))
	}
	if rel == "" {
		return fail("empty path")
	}
	if len(rel) > MaxPathBytes {
		return fail("path is %d bytes long (limit %d)", len(rel), MaxPathBytes)
	}
	for i, comp := range strings.Split(rel, "/") {
		switch comp {
		case "":
			return fail("empty path component")
		case ".", "..":
			return fail("relative path component %q", comp)
		}
		if i == 0 && comp == MetaDir {
			return fail("%s is reserved for Bunkarr", MetaDir)
		}
		if IsTempName(comp) {
			return fail("names starting with %s are reserved for Bunkarr's temp files", TempPrefix)
		}
		if len(comp) > MaxNameBytes || utf16Len(comp) > MaxNameBytes {
			return fail("name %q is longer than %d bytes", comp, MaxNameBytes)
		}
		if strings.ContainsRune(comp, 0) {
			return fail("name contains NUL")
		}
		if j := strings.IndexAny(comp, caps.InvalidChars); j >= 0 {
			r, _ := utf8.DecodeRuneInString(comp[j:])
			return fail("the destination does not accept %q in names", r)
		}
		if !caps.TrailingDotSpace && (strings.HasSuffix(comp, ".") || strings.HasSuffix(comp, " ")) {
			return fail("the destination does not keep a trailing dot or space in %q", comp)
		}
	}
	return nil
}

// utf16Len is the length of s in UTF-16 code units (invalid bytes count as one unit each).
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// FoldKey returns a case-folded key of rel for collision detection on case-insensitive
// destinations (S11): two paths collide when their keys are equal. Each rune is mapped to the
// smallest rune of its Unicode simple case-folding orbit, so "K", "k" and the Kelvin sign fold
// together. It is deliberately at least as aggressive as the filesystems it models (SMB, NTFS,
// APFS/HFS+ case-insensitive): a false collision fails an item with a warning, a missed one could
// make two names share one file. Unicode normalization (NFC/NFD) is not applied.
func FoldKey(rel string) string {
	return strings.Map(foldRune, rel)
}

func foldRune(r rune) rune {
	m := r
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		if f < m {
			m = f
		}
	}
	return m
}
