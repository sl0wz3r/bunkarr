package catalog

import "strings"

// The catalog's exclude patterns, exported for the restic engine's exclude translation
// (docs/design/phase4.md §6.2 step 4): restic must never exclude a file the catalog includes, so
// the translation starts from the patterns exactly as the catalog parsed them, and its tests
// compare against the catalog's own matcher. Nothing here changes what the catalog excludes.

// ExcludePattern is one parsed exclude pattern: Glob without its leading "/" (Anchored) or
// trailing "/" (DirOnly), lowercased when Fold (the defaults, matched case-insensitively). Its
// fields are those of restic.ExcludePattern, so one converts to the other.
type ExcludePattern struct {
	Glob     string
	Fold     bool
	Anchored bool
	DirOnly  bool
}

func exportPattern(p pattern) ExcludePattern {
	return ExcludePattern{Glob: p.glob, Fold: p.fold, Anchored: p.anchored, DirOnly: p.dirOnly}
}

// DefaultExcludePatterns returns the default patterns (DefaultExcludes) as the catalog parses
// them: case-insensitive.
func DefaultExcludePatterns() []ExcludePattern {
	out := make([]ExcludePattern, 0, len(defaultExcludes))
	for _, d := range defaultExcludes {
		out = append(out, exportPattern(parsePattern(d, true)))
	}
	return out
}

// ParseExcludePatterns returns a source's own patterns (its validated exclude list) as the
// catalog parses them: case-sensitive.
func ParseExcludePatterns(own []string) []ExcludePattern {
	out := make([]ExcludePattern, 0, len(own))
	for _, u := range own {
		out = append(out, exportPattern(parsePattern(u, false)))
	}
	return out
}

// Excluded reports whether the entry at rel (slash-separated, relative to the source root) with
// base name base is excluded by patterns: exactly the matcher a scan uses (patterns match the
// base name or the whole relative path; anchored ones the relative path only; directory-only
// ones directories only; Fold ones case-insensitively).
func Excluded(patterns []ExcludePattern, rel, base string, isDir bool) bool {
	m := &matcher{patterns: make([]pattern, len(patterns))}
	for i, p := range patterns {
		m.patterns[i] = pattern{glob: p.Glob, fold: p.Fold, anchored: p.Anchored, dirOnly: p.DirOnly}
	}
	return m.excluded(rel, base, isDir)
}

// ExcludedWithin reports whether a scan leaves rel out of the catalog because of patterns: rel
// itself is excluded, or one of its ancestor directories is (a scan does not descend into an
// excluded directory).
func ExcludedWithin(patterns []ExcludePattern, rel string, isDir bool) bool {
	parts := strings.Split(rel, "/")
	for i := range parts {
		dir := i < len(parts)-1 || isDir
		if Excluded(patterns, strings.Join(parts[:i+1], "/"), parts[i], dir) {
			return true
		}
	}
	return false
}
