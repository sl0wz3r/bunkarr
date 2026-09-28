package restic

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The exclude translation (docs/design/phase4.md §6.2 step 4, review finding DS-7): restic must
// never exclude a file the catalog includes, because such a file would be recorded as backed up
// while no snapshot holds it. The translated excludes are therefore a subset of the catalog's:
//
//   - the config directory, every local destination target and every directory the scan skipped
//     as an alias (S4) go to the case-sensitive exclude file as escaped absolute paths (a path
//     that is the source root or one of its ancestors is dropped: it would exclude everything);
//   - the catalog's default patterns (matched case-insensitively by the catalog) go to the
//     --iexclude-file, the source's own (case-sensitive) patterns to the --exclude-file;
//   - an anchored pattern, and an unanchored one containing "/" (the catalog matches it against
//     the whole relative path only), become "<source root>/<pattern>";
//   - an unanchored pattern without "/" (the catalog matches it against each entry's base name)
//     becomes "<source root>/**/<pattern>", so it never matches a component of the source root
//     itself, as a bare restic pattern would;
//   - a pattern restic could match more widely than the catalog is dropped (reported in
//     dropped): a directory-only pattern (restic cannot restrict a pattern to directories), one
//     that restic's line reader would change (a line break, a "$" inside a character class), or
//     one restic would parse differently (an unclean path, a malformed glob).
//
// Excluding less only uploads junk, which is not recorded; the read-back catches any remaining
// divergence (a file restic excluded is not in the snapshot, and its item fails).
//
// restic reads an exclude file line by line: it trims surrounding whitespace, skips empty lines
// and lines starting with "#", and expands environment variables ($NAME, ${NAME}) before it
// parses the pattern as filepath.Match globs joined by "/", "**" matching any number of
// components. The escaping here keeps every translated line unchanged by that reader: glob
// characters of literal names are escaped with "\", "$" becomes "[$]", a trailing whitespace
// character becomes a one-character class, and every line starts with "/".

// ExcludePattern is one parsed exclude pattern of the catalog (catalog.ExcludePattern, which
// converts to this type): Glob without its leading or trailing "/", Fold for the
// case-insensitive defaults (Glob already lowercased), Anchored for a leading "/", DirOnly for a
// trailing "/".
type ExcludePattern struct {
	Glob     string
	Fold     bool
	Anchored bool
	DirOnly  bool
}

// String renders the pattern as the user wrote it.
func (p ExcludePattern) String() string {
	s := p.Glob
	if p.Anchored {
		s = "/" + s
	}
	if p.DirOnly {
		s += "/"
	}
	return s
}

// TranslateInput is what TranslateExcludes translates for one source.
type TranslateInput struct {
	// SourceRoot is the source's recorded root (absolute, clean).
	SourceRoot string
	// ConfigDir and LocalTargets are absolute paths (the config directory, every local
	// destination's target); AliasDirs are directories relative to SourceRoot that the scan
	// skipped as aliases of those.
	ConfigDir    string
	LocalTargets []string
	AliasDirs    []string
	// Defaults are the catalog's default patterns, Own the source's own.
	Defaults []ExcludePattern
	Own      []ExcludePattern
}

// TranslateExcludes translates a source's exclusions into restic's exclude files (see above):
// the lines of the --exclude-file (case-sensitive) and of the --iexclude-file, and a description
// of each dropped pattern or path, for a debug line.
func TranslateExcludes(in TranslateInput) (excludes, iexcludes, dropped []string) {
	root := path.Clean(in.SourceRoot)
	prefix, ok := escapeRoot(root)
	if !filepath.IsAbs(in.SourceRoot) || !ok {
		dropped = append(dropped, "every pattern: the source root "+quote(in.SourceRoot)+" cannot be written to an exclude file")
		return nil, nil, dropped
	}
	addPath := func(what, p string) {
		switch {
		case p == "":
			return
		case !filepath.IsAbs(p) || path.Clean(p) != p:
			dropped = append(dropped, what+" "+quote(p)+": not a clean absolute path")
		case p == root || strings.HasPrefix(root+"/", strings.TrimSuffix(p, "/")+"/"):
			dropped = append(dropped, what+" "+quote(p)+": the source root is inside it")
		default:
			line, err := escapeAbs(p)
			if err == nil {
				line, err = finish(line)
			}
			if err != nil {
				dropped = append(dropped, what+" "+quote(p)+": "+err.Error())
				return
			}
			excludes = append(excludes, line)
		}
	}
	addPath("config directory", in.ConfigDir)
	for _, t := range in.LocalTargets {
		addPath("destination target", t)
	}
	for _, a := range in.AliasDirs {
		if a == "" || strings.HasPrefix(a, "/") || path.Clean(a) != a || a == "." || a == ".." || strings.HasPrefix(a, "../") {
			dropped = append(dropped, "alias directory "+quote(a)+": not a clean relative path")
			continue
		}
		rel, err := escapeLiteral(a, false)
		var line string
		if err == nil {
			line, err = finish(prefix + "/" + rel)
		}
		if err != nil {
			dropped = append(dropped, "alias directory "+quote(a)+": "+err.Error())
			continue
		}
		excludes = append(excludes, line)
	}
	for _, p := range in.Defaults {
		line, err := translatePattern(prefix, p)
		if err != nil {
			dropped = append(dropped, "default pattern "+quote(p.String())+": "+err.Error())
			continue
		}
		iexcludes = append(iexcludes, line)
	}
	for _, p := range in.Own {
		line, err := translatePattern(prefix, p)
		if err != nil {
			dropped = append(dropped, "pattern "+quote(p.String())+": "+err.Error())
			continue
		}
		excludes = append(excludes, line)
	}
	return excludes, iexcludes, dropped
}

func quote(s string) string { return `"` + strings.ReplaceAll(s, "\n", `\n`) + `"` }

// Reasons a pattern is dropped.
var (
	errDirOnly     = errors.New("directory-only (restic cannot restrict a pattern to directories)")
	errLineBreak   = errors.New("contains a line break")
	errDollarClass = errors.New(`contains "$" inside a character class (restic would expand it)`)
	errUnclean     = errors.New("restic would read it as another path")
	errBadGlob     = errors.New("not a glob restic parses the same way")
	errEmpty       = errors.New("empty")
)

// translatePattern translates one catalog pattern (see TranslateExcludes).
func translatePattern(prefix string, p ExcludePattern) (string, error) {
	if p.DirOnly {
		return "", errDirOnly
	}
	glob := p.Glob
	if glob == "" {
		return "", errEmpty
	}
	if strings.ContainsAny(glob, "\n\r") {
		return "", errLineBreak
	}
	// In the catalog's path.Match a "**" component is "*" (one component); in restic it spans
	// components.
	parts := strings.Split(glob, "/")
	for i, c := range parts {
		if c == "**" {
			parts[i] = "*"
		}
	}
	glob = strings.Join(parts, "/")
	glob, err := escapeDollar(glob)
	if err != nil {
		return "", err
	}
	var line string
	if p.Anchored || strings.Contains(p.Glob, "/") {
		line = prefix + "/" + glob
	} else {
		line = prefix + "/**/" + glob
	}
	return finish(line)
}

// escapeRoot escapes the source root for the start of a pattern ("" for "/"). A line break,
// which no exclude-file line can hold, becomes "?": every path of the backup starts with the
// root itself, so the wildcard matches nothing else there.
func escapeRoot(root string) (string, bool) {
	if root == "/" {
		return "", true
	}
	s, err := escapeLiteral(root, true)
	return s, err == nil
}

// escapeAbs escapes an absolute literal path.
func escapeAbs(p string) (string, error) {
	return escapeLiteral(p, false)
}

// escapeLiteral escapes a literal path for a glob: "\", "*", "?" and "[" with a backslash, "$"
// as "[$]". A line break is an error, or "?" when newlineWildcard (the source root only).
func escapeLiteral(s string, newlineWildcard bool) (string, error) {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\', '*', '?', '[':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '$':
			b.WriteString("[$]")
		case '\n':
			if !newlineWildcard {
				return "", errLineBreak
			}
			b.WriteByte('?')
		case '\r':
			if !newlineWildcard {
				return "", errLineBreak
			}
			b.WriteByte('?')
		default:
			b.WriteRune(r)
		}
	}
	return b.String(), nil
}

// escapeDollar rewrites each "$" of a glob outside a character class as "[$]" (a "\$" too),
// which restic's variable expansion leaves alone. A "$" inside a class stays; finish drops the
// line when the expansion would change it.
func escapeDollar(glob string) (string, error) {
	var b strings.Builder
	inClass := false
	for i := 0; i < len(glob); i++ {
		c := glob[i]
		switch {
		case c == '\\' && i+1 < len(glob):
			i++
			if glob[i] == '$' && !inClass {
				b.WriteString("[$]")
			} else {
				b.WriteByte(c)
				b.WriteByte(glob[i])
			}
		case c == '[' && !inClass:
			inClass = true
			b.WriteByte(c)
			if i+1 < len(glob) && glob[i+1] == '^' {
				b.WriteByte('^')
				i++
			}
		case c == ']' && inClass:
			inClass = false
			b.WriteByte(c)
		case c == '$' && !inClass:
			b.WriteString("[$]")
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), nil
}

// finish protects a line's end from restic's whitespace trimming and checks that restic reads
// it unchanged and parses it as intended.
func finish(line string) (string, error) {
	if r, size := utf8.DecodeLastRuneInString(line); unicode.IsSpace(r) {
		body := line[:len(line)-size]
		n := len(body) - len(strings.TrimRight(body, `\`))
		if n%2 == 1 {
			body = body[:len(body)-1] // an escaped space: the class escapes it now
		}
		line = body + "[" + string(r) + "]"
	}
	if strings.ContainsAny(line, "\n") {
		return "", errLineBreak
	}
	if !strings.HasPrefix(line, "/") || strings.TrimSpace(line) != line {
		return "", errUnclean
	}
	expandCalled := false
	if os.Expand(line, func(string) string { expandCalled = true; return "" }) != line || expandCalled {
		return "", errDollarClass
	}
	if filepath.Clean(line) != line {
		return "", errUnclean
	}
	for _, c := range strings.Split(line[1:], "/") {
		if c == "" || c == "." || c == ".." {
			return "", errUnclean
		}
		if c == "**" {
			continue
		}
		if _, err := filepath.Match(c, ""); err != nil {
			return "", errBadGlob
		}
	}
	return line, nil
}
