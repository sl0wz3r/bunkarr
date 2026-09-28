package restic

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
)

// This file checks the exclude translation against a port of restic's own matcher
// (internal/filter of restic 0.18: patterns are filepath.Match globs joined by "/", "**" spans
// components, a pattern matches a path when it matches a window of consecutive components, and
// an absolute pattern's window starts at the root) and of its exclude-file reader (trimmed lines,
// "#" comments, environment expansion, --iexclude lowercasing both sides). The real-binary test
// (enginebin) checks the same with restic itself.

type resticPart struct {
	pat    string
	simple bool
}

func resticSplit(p string) []string {
	parts := strings.Split(filepath.ToSlash(p), "/")
	if parts[0] == "" {
		parts[0] = "/"
	}
	return parts
}

func resticPrepare(pattern string) []resticPart {
	var out []resticPart
	for _, p := range resticSplit(filepath.Clean(pattern)) {
		simple := !strings.ContainsAny(p, "\\[]*?")
		if p == "**" {
			p = ""
		}
		out = append(out, resticPart{p, simple})
	}
	return out
}

func resticMatchParts(parts []resticPart, strs []string) bool {
	for pos, p := range parts {
		if p.pat == "" && !p.simple {
			// "**": expand it into 0..n single-component wildcards.
			for i := 0; i <= len(strs)-len(parts)+1; i++ {
				expanded := append([]resticPart{}, parts[:pos]...)
				for range i {
					expanded = append(expanded, resticPart{"*", false})
				}
				expanded = append(expanded, parts[pos+1:]...)
				if resticMatchParts(expanded, strs) {
					return true
				}
			}
			return false
		}
	}
	if len(parts) == 0 {
		return len(strs) == 0
	}
	for offset := len(strs) - len(parts); offset >= 0; offset-- {
		ok := true
		for i, p := range parts {
			var m bool
			if p.simple {
				m = p.pat == strs[offset+i]
			} else {
				m, _ = filepath.Match(p.pat, strs[offset+i])
			}
			if !m {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// resticReadPatterns reads an exclude file's lines as restic does.
func resticReadPatterns(lines []string) []string {
	var out []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		out = append(out, os.ExpandEnv(l))
	}
	return out
}

// resticExcludes reports whether restic's --exclude-file lines or --iexclude-file lines exclude
// the absolute path p.
func resticExcludes(excludes, iexcludes []string, p string) bool {
	strs := resticSplit(p)
	for _, pat := range resticReadPatterns(excludes) {
		if resticMatchParts(resticPrepare(pat), strs) {
			return true
		}
	}
	lower := resticSplit(strings.ToLower(p))
	for _, pat := range resticReadPatterns(iexcludes) {
		if resticMatchParts(resticPrepare(strings.ToLower(pat)), lower) {
			return true
		}
	}
	return false
}

func TestResticMatcherPort(t *testing.T) {
	for _, tc := range []struct {
		pattern, path string
		want          bool
	}{
		{"foo", "/a/foo", true},
		{"foo", "/a/foo/b", true},
		{"foo/bar", "/x/foo/bar/baz", true},
		{"/foo", "/a/foo", false},
		{"/a/foo", "/a/foo/x", true},
		{"/a/**/x", "/a/b/c/x", true},
		{"/a/**/x", "/a/x", true},
		{"/a/**/x", "/b/x", false},
		{"*.nfo", "/m/a.nfo", true},
		{`/a/b\*c`, "/a/b*c", true},
		{`/a/b\*c`, "/a/bxc", false},
		{"/a/[$]x", "/a/$x", true},
	} {
		if got := resticMatchParts(resticPrepare(tc.pattern), resticSplit(tc.path)); got != tc.want {
			t.Errorf("match(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

// excludeCorpus is one corpus of relative paths inside a source (files; their ancestors are
// checked as directories): case variants, nested paths, junk names, names that look like the
// patterns, and names with glob characters.
var excludeCorpus = []string{
	"Movies/A (2001)/A.mkv", "Movies/A (2001)/A.nfo", "Movies/A (2001)/A.NFO", "Movies/A (2001)/.DS_Store",
	"Movies/A (2001)/.ds_store", "Movies/@eaDir/thumb.jpg", "Movies/@EADIR", "@eaDir", "Extras/x.mkv", "Movies/Extras/x.mkv",
	"extras/x.mkv", "Show/S01/e1.srt", "Show/S01/e1.SRT", "x/Show/S01/e1.srt", "Show/S02/e1.srt", "top.mkv", "sub/top.mkv",
	"Samples", "Movie/Samples/s.mkv", "Movie/samples/s.mkv", "dl/m.mkv.part", "dl/m.mkv.PART", "dl/m.partial~", "Thumbs.db",
	"a/THUMBS.DB", "desktop.ini", "#recycle/x", "a/#recycle", ".Trash-1000/x", "lost+found/f", ".grab/x", ".bunkarr/x",
	".bunkarr-tmp-a", "x/._resource", "Media/file.mkv", "media/Media/x", "Trailer.mkv", "a/trailer-2.mkv", "$RECYCLE.BIN/x",
	"a/$RECYCLE.BIN", "costs$5.txt", "star*.mkv", "b[1].mkv", `back\slash.mkv`, "config/bunkarr.key", "backup/unas/x.mkv",
	"new\nline.mkv", "a b /c", "trailing space /x", "Show/S01/a/b.srt", "S01/e.srt", "deep/a/b/c/d.nfo",
}

// TestTranslateExcludesSubset: every path restic's translated excludes match is also excluded by
// the catalog (§14.1 exclude translation, DS-7), and the source's own patterns land in the
// case-sensitive file.
func TestTranslateExcludesSubset(t *testing.T) {
	own := []string{"*.nfo", "/Extras/", "Samples/", "Show/S01/*.srt", "/top.mkv", "[Tt]railer*", "$RECYCLE.BIN", "costs[$]5.txt",
		"**/x", "Media", `b\[1\].mkv`, "a b ", "/trailing space ", "S01/../S01/e.srt", "a[$b]c"}
	defaults := catalog.DefaultExcludePatterns()
	ownPatterns := catalog.ParseExcludePatterns(own)
	catalogPatterns := append(slices.Clone(defaults), ownPatterns...)
	for _, root := range []string{"/mnt/user/Media", "/", "/mnt/a*b[c]/x$HOME/y", "/mnt/new\nline", "/srv/trailing /media"} {
		t.Run(strings.ReplaceAll(root, "\n", `\n`), func(t *testing.T) {
			in := TranslateInput{SourceRoot: root, ConfigDir: "/config", LocalTargets: []string{"/mnt/unas", root + "/backup/unas", "/"},
				AliasDirs: []string{"config", "x*y/[alias]", "new\nline"},
				Defaults:  convert(defaults), Own: convert(ownPatterns)}
			excludes, iexcludes, dropped := TranslateExcludes(in)
			if len(iexcludes) == 0 || len(excludes) == 0 {
				t.Fatalf("excludes %q, iexcludes %q, dropped %q", excludes, iexcludes, dropped)
			}
			// Own patterns only in the case-sensitive file, defaults only in the other.
			for _, l := range iexcludes {
				if strings.Contains(l, "nfo") || strings.Contains(strings.ToLower(l), "extras") {
					t.Errorf("an own pattern in the iexclude file: %q", l)
				}
			}
			join := func(rel string) string {
				if root == "/" {
					return "/" + rel
				}
				return root + "/" + rel
			}
			skippedByScan := func(rel string) bool {
				// The scan skips the aliases and the forbidden roots inside the source (S4).
				for _, a := range []string{"config", "x*y/[alias]", "new\nline", "backup/unas"} {
					if rel == a || strings.HasPrefix(rel, a+"/") {
						return true
					}
				}
				return false
			}
			for _, rel := range append(slices.Clone(excludeCorpus), "config", "x*y/[alias]/f", "xzy/[alias]/f", "x*y/a/f", "new\nline/f", "newXline/f") {
				parts := strings.Split(rel, "/")
				for i := range parts {
					sub := strings.Join(parts[:i+1], "/")
					isDir := i < len(parts)-1
					if !resticExcludes(excludes, iexcludes, join(sub)) {
						continue
					}
					if !catalog.ExcludedWithin(catalogPatterns, sub, isDir) && !skippedByScan(sub) {
						t.Errorf("restic excludes %q (dir=%v), the catalog includes it\nexcludes %q\niexcludes %q", sub, isDir, excludes, iexcludes)
					}
				}
			}
			// What restic must still exclude (the translation is not empty-handed).
			for _, rel := range []string{"Movies/A (2001)/A.nfo", "Show/S01/e1.srt", "top.mkv", "Movies/A (2001)/.DS_Store",
				"dl/m.mkv.PART", "config", "Trailer.mkv", "$RECYCLE.BIN/x", "costs$5.txt", "x*y/[alias]/f"} {
				if !resticExcludes(excludes, iexcludes, join(rel)) {
					t.Errorf("restic does not exclude %q\nexcludes %q\niexcludes %q", rel, excludes, iexcludes)
				}
			}
			for _, want := range []string{`"@eadir/": directory-only`, `"Samples/": directory-only`, `"/Extras/": directory-only`,
				`destination target "/": the source root is inside it`} {
				if !slices.ContainsFunc(dropped, func(d string) bool { return strings.Contains(d, want) }) {
					t.Errorf("dropped %q lacks %q", dropped, want)
				}
			}
		})
	}
}

func convert(in []catalog.ExcludePattern) []ExcludePattern {
	out := make([]ExcludePattern, len(in))
	for i, p := range in {
		out[i] = ExcludePattern(p)
	}
	return out
}

// TestTranslateExcludesLines pins the escaped lines: glob characters of literal names escaped,
// "$" as a class, a trailing space protected, a line break dropped, and the forms of anchored,
// "/"-containing and bare patterns.
func TestTranslateExcludesLines(t *testing.T) {
	ex, iex, dropped := TranslateExcludes(TranslateInput{
		SourceRoot:   "/mnt/a*b",
		ConfigDir:    "/config",
		LocalTargets: []string{"/mnt/un[a]s ", "/mnt/new\nline", "relative", "/mnt"},
		AliasDirs:    []string{`back\slash`, "../x"},
		Defaults:     []ExcludePattern{{Glob: ".ds_store", Fold: true}, {Glob: "@eadir", Fold: true, DirOnly: true}},
		Own: []ExcludePattern{{Glob: "*.nfo"}, {Glob: "Extras", Anchored: true}, {Glob: "Show/S01/*.srt"}, {Glob: "$x"},
			{Glob: "**/y"}, {Glob: "a\nb"}, {Glob: "a[$x]"}, {Glob: "a//b"}, {Glob: "bad["}},
	})
	wantEx := []string{"/config", `/mnt/un\[a]s[ ]`, `/mnt/a\*b/back\\slash`, `/mnt/a\*b/**/*.nfo`, `/mnt/a\*b/Extras`,
		`/mnt/a\*b/Show/S01/*.srt`, `/mnt/a\*b/**/[$]x`, `/mnt/a\*b/*/y`}
	if !slices.Equal(ex, wantEx) {
		t.Errorf("excludes\n got %q\nwant %q", ex, wantEx)
	}
	if !slices.Equal(iex, []string{`/mnt/a\*b/**/.ds_store`}) {
		t.Errorf("iexcludes %q", iex)
	}
	for _, want := range []string{"/mnt/new", "relative", `"/mnt": the source root is inside it`, "../x", "@eadir/", "a\\nb", "a[$x]", "a//b", "bad["} {
		found := false
		for _, d := range dropped {
			found = found || strings.Contains(d, want)
		}
		if !found {
			t.Errorf("dropped %q lacks %q", dropped, want)
		}
	}
	// The escaped target matches exactly itself.
	if !resticExcludes([]string{`/mnt/un\[a]s[ ]`}, nil, "/mnt/un[a]s /x") || resticExcludes([]string{`/mnt/un\[a]s[ ]`}, nil, "/mnt/unas /x") {
		t.Error("the escaped target does not match exactly itself")
	}
}
