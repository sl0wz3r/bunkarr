package catalog

import (
	"errors"
	"testing"
)

func TestMatcher(t *testing.T) {
	m := newMatcher([]string{"*.nfo", "/Extras/", "Samples/", "Show/S01/*.srt", "/top.mkv"})
	for _, tc := range []struct {
		rel   string
		isDir bool
		want  bool
	}{
		{".DS_Store", false, true},
		{"a/b/.ds_store", false, true}, // defaults ignore case
		{"a/Thumbs.db", false, true},
		{"x/._movie.mkv", false, true},
		{"@eaDir", true, true},
		{"a/@eaDir", true, true},
		{"@eaDir", false, false}, // directory-only pattern
		{"#recycle", true, true},
		{"a/.Trash-1000", true, true},
		{"lost+found", true, true},
		{"dl/m.mkv.part", false, true},
		{"dl/m.mkv.PARTIAL", false, true},
		{"dl/m.mkv.!ut", false, true},
		{".grab", true, true},
		{".bunkarr", true, true},
		{"x/.bunkarr-tmp-a.mkv-123", false, true},
		{"movie.mkv", false, false},
		{"a/movie.nfo", false, true},
		{"a/movie.NFO", false, false}, // own patterns are case-sensitive
		{"Extras", true, true},
		{"Show/Extras", true, false}, // anchored
		{"Movie/Samples", true, true},
		{"Movie/Samples", false, false},
		{"Show/S01/e1.srt", false, true},
		{"Show/S02/e1.srt", false, false},
		{"top.mkv", false, true},
		{"sub/top.mkv", false, false},
	} {
		base := tc.rel
		if i := lastSlash(tc.rel); i >= 0 {
			base = tc.rel[i+1:]
		}
		if got := m.excluded(tc.rel, base, tc.isDir); got != tc.want {
			t.Errorf("excluded(%q, dir=%v) = %v, want %v", tc.rel, tc.isDir, got, tc.want)
		}
	}
}

func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}

func TestValidateExcludes(t *testing.T) {
	got, err := validateExcludes(nil)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("nil = %v, %v", got, err)
	}
	got, err = validateExcludes([]string{" *.nfo ", "Extras/"})
	if err != nil || got[0] != "*.nfo" || got[1] != "Extras/" {
		t.Fatalf("got %v, %v", got, err)
	}
	many := make([]string, maxExcludes+1)
	for i := range many {
		many[i] = "x"
	}
	for _, bad := range [][]string{{"["}, {""}, {"a\x00b"}, {"//"}, many} {
		var ve *ValidationError
		if _, err := validateExcludes(bad); !errors.As(err, &ve) || ve.Field != "exclude" {
			t.Errorf("validateExcludes(%q) = %v", bad[0], err)
		}
	}
	if d := DefaultExcludes(); len(d) == 0 {
		t.Fatal("no default excludes")
	} else {
		d[0] = "changed"
		if DefaultExcludes()[0] == "changed" {
			t.Fatal("DefaultExcludes exposes its backing array")
		}
	}
}
