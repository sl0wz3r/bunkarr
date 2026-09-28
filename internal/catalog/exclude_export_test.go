package catalog

import (
	"slices"
	"testing"
)

// TestExportedMatcherIsTheCatalogs: Excluded over the exported patterns answers exactly what the
// scan's matcher answers, for the defaults and a source's own patterns, over a corpus of paths.
func TestExportedMatcherIsTheCatalogs(t *testing.T) {
	own := []string{"*.nfo", "/Extras/", "Samples/", "Show/S01/*.srt", "/top.mkv", "[Tt]railer*", "a?c", `lit\*eral`}
	m := newMatcher(own)
	patterns := append(DefaultExcludePatterns(), ParseExcludePatterns(own)...)
	corpus := []string{".DS_Store", "a/b/.ds_store", "x/._movie.mkv", "@eaDir", "a/@EADIR", "lost+found", "dl/m.PART",
		"movie.nfo", "a/movie.NFO", "Extras", "Show/Extras", "Movie/Samples", "Show/S01/e1.srt", "Show/S02/e1.srt", "top.mkv",
		"sub/top.mkv", "Trailer.mkv", "x/trailer-1.mkv", "abc", "a/axc", "lit*eral", "litXeral", ".bunkarr", "x/.bunkarr-tmp-1",
		"#recycle", "a/.Trash-1000", "movie.mkv"}
	for _, rel := range corpus {
		base := rel[lastSlash(rel)+1:]
		for _, isDir := range []bool{false, true} {
			if got, want := Excluded(patterns, rel, base, isDir), m.excluded(rel, base, isDir); got != want {
				t.Errorf("Excluded(%q, dir=%v) = %v, the scan's matcher says %v", rel, isDir, got, want)
			}
		}
	}
}

func TestExportedPatterns(t *testing.T) {
	d := DefaultExcludePatterns()
	if len(d) != len(defaultExcludes) {
		t.Fatalf("%d defaults, want %d", len(d), len(defaultExcludes))
	}
	want := ExcludePattern{Glob: "@eadir", Fold: true, DirOnly: true}
	if !slices.Contains(d, want) {
		t.Errorf("defaults %v lack %+v", d, want)
	}
	own := ParseExcludePatterns([]string{"/Extras/", "Show/S01/*.srt", "*.NFO"})
	if !slices.Equal(own, []ExcludePattern{{Glob: "Extras", Anchored: true, DirOnly: true}, {Glob: "Show/S01/*.srt"}, {Glob: "*.NFO"}}) {
		t.Errorf("own %+v", own)
	}
	d[0].Glob = "changed"
	if DefaultExcludePatterns()[0].Glob == "changed" {
		t.Error("DefaultExcludePatterns exposes shared state")
	}
}

func TestExcludedWithin(t *testing.T) {
	p := append(DefaultExcludePatterns(), ParseExcludePatterns([]string{"/Extras/", "Samples/"})...)
	for _, tc := range []struct {
		rel   string
		isDir bool
		want  bool
	}{
		{"Extras/a.mkv", false, true},
		{"Movie/Extras/a.mkv", false, false},
		{"Movie/Samples/s.mkv", false, true},
		{"Movie/Samples", false, false},
		{"a/@eaDir/SYNO.jpg", false, true},
		{"a/b/c.mkv", false, false},
		{".bunkarr/destination.json", false, true},
	} {
		if got := ExcludedWithin(p, tc.rel, tc.isDir); got != tc.want {
			t.Errorf("ExcludedWithin(%q, dir=%v) = %v, want %v", tc.rel, tc.isDir, got, tc.want)
		}
	}
}
