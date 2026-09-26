package catalog

import (
	"path"
	"strings"
)

// defaultExcludes are skipped in every source; see DefaultExcludes. Never modified.
var defaultExcludes = []string{
	".DS_Store",
	"._*",
	"Thumbs.db",
	"desktop.ini",
	"@eaDir/",
	"#recycle/",
	".Trash-*/",
	"lost+found/",
	"*.partial",
	"*.part",
	"*.!qB",
	"*.!ut",
	// The temporary names of the *arrs' transactional copies (phase2-3.md §9.3): a webhook scan can
	// run while a season pack is still being copied into the folder.
	"*.partial~",
	"*.backup~",
	".grab/",
	".bunkarr/",
	".bunkarr-tmp-*",
}

// DefaultExcludes returns the patterns skipped in every source, in addition to its own: OS and
// NAS metadata, recycle bins, files still being downloaded and Bunkarr's own folders. They are
// matched case-insensitively. A trailing "/" matches directories only (the whole subtree is
// skipped).
func DefaultExcludes() []string {
	return append([]string(nil), defaultExcludes...)
}

// Limits on a source's exclude list.
const (
	maxExcludes       = 256
	maxExcludePattern = 1024
)

// pattern is one compiled exclude pattern.
type pattern struct {
	glob     string
	dirOnly  bool // trailing "/": directories only
	anchored bool // leading "/": matched against the relative path only
	fold     bool // case-insensitive (the defaults)
}

// matcher decides whether an entry is excluded.
type matcher struct {
	patterns []pattern
}

func parsePattern(raw string, fold bool) pattern {
	p := pattern{glob: raw, fold: fold}
	if strings.HasSuffix(p.glob, "/") {
		p.dirOnly = true
		p.glob = strings.TrimRight(p.glob, "/")
	}
	if strings.HasPrefix(p.glob, "/") {
		p.anchored = true
		p.glob = strings.TrimLeft(p.glob, "/")
	}
	if fold {
		p.glob = strings.ToLower(p.glob)
	}
	return p
}

// newMatcher compiles the default patterns plus a source's own (already validated) patterns.
func newMatcher(user []string) *matcher {
	m := &matcher{}
	for _, d := range defaultExcludes {
		m.patterns = append(m.patterns, parsePattern(d, true))
	}
	for _, u := range user {
		m.patterns = append(m.patterns, parsePattern(u, false))
	}
	return m
}

// excluded reports whether the entry at rel (slash-separated, relative to the source root) with
// base name base is excluded. Patterns match the base name or the whole relative path; anchored
// patterns match only the relative path.
func (m *matcher) excluded(rel, base string, isDir bool) bool {
	var lrel, lbase string
	for _, p := range m.patterns {
		if p.dirOnly && !isDir {
			continue
		}
		r, b := rel, base
		if p.fold {
			if lrel == "" {
				lrel, lbase = strings.ToLower(rel), strings.ToLower(base)
			}
			r, b = lrel, lbase
		}
		if !p.anchored {
			if ok, _ := path.Match(p.glob, b); ok {
				return true
			}
		}
		if ok, _ := path.Match(p.glob, r); ok {
			return true
		}
	}
	return false
}

// validateExcludes checks a source's exclude list and returns it normalized (nil becomes empty).
func validateExcludes(in []string) ([]string, error) {
	if len(in) > maxExcludes {
		return nil, invalid("exclude", "at most %d patterns", maxExcludes)
	}
	out := make([]string, 0, len(in))
	for i, raw := range in {
		p := strings.TrimSpace(raw)
		switch {
		case p == "":
			return nil, invalid("exclude", "pattern %d is empty", i+1)
		case len(p) > maxExcludePattern:
			return nil, invalid("exclude", "pattern %d is longer than %d bytes", i+1, maxExcludePattern)
		case strings.ContainsRune(p, 0):
			return nil, invalid("exclude", "pattern %d contains a NUL byte", i+1)
		}
		g := parsePattern(p, false).glob
		if g == "" {
			return nil, invalid("exclude", "pattern %q matches nothing", p)
		}
		if _, err := path.Match(g, ""); err != nil {
			return nil, invalid("exclude", "pattern %q is not a valid glob: %v", p, err)
		}
		out = append(out, p)
	}
	return out, nil
}
