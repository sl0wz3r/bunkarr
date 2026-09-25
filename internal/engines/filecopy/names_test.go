package filecopy

import (
	"errors"
	"strings"
	"testing"
)

func TestNameCheck(t *testing.T) {
	posix := Capabilities{TrailingDotSpace: true}
	smb := Capabilities{InvalidChars: ProbeChars, CaseInsensitive: true}
	tests := []struct {
		name string
		rel  string
		caps Capabilities
		ok   bool
	}{
		{"plain", "Movies/Alien (1979)/Alien.mkv", smb, true},
		{"unicode", "Musik/Björk/Jóga.flac", smb, true},
		{"colon on posix", "TV/Show: Part 1/e01.mkv", posix, true},
		{"colon on smb", "TV/Show: Part 1/e01.mkv", smb, false},
		{"question mark on smb", "Movies/What?.mkv", smb, false},
		{"backslash on smb", `Movies/a\b.mkv`, smb, false},
		{"pipe on smb", "Movies/a|b.mkv", smb, false},
		{"trailing dot on posix", "Movies/Mr. Robot./e.mkv", posix, true},
		{"trailing dot on smb", "Movies/Mr. Robot./e.mkv", smb, false},
		{"trailing space on smb", "Movies/Name /e.mkv", smb, false},
		{"leading space is fine", "Movies/ Name/e.mkv", smb, true},
		{"empty", "", posix, false},
		{"empty component", "Movies//a.mkv", posix, false},
		{"dot component", "Movies/./a.mkv", posix, false},
		{"dotdot component", "Movies/../a.mkv", posix, false},
		{"absolute", "/Movies/a.mkv", posix, false},
		{"meta dir", ".bunkarr/x", posix, false},
		{"meta dir deeper is a normal name", "Movies/.bunkarr/x", posix, true},
		{"temp prefix", "Movies/.bunkarr-tmp-a.mkv", posix, false},
		{"NUL", "Movies/a\x00b", posix, false},
		{"255 bytes", "Movies/" + strings.Repeat("a", 255), posix, true},
		{"256 bytes", "Movies/" + strings.Repeat("a", 256), posix, false},
		{"long path", strings.Repeat("abcdefghi/", 401), posix, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := NameCheck(tt.rel, tt.caps)
			if tt.ok && err != nil {
				t.Errorf("NameCheck(%q) = %v, want ok", tt.rel, err)
			}
			if !tt.ok && !errors.Is(err, ErrNameNotStorable) {
				t.Errorf("NameCheck(%q) = %v, want ErrNameNotStorable", tt.rel, err)
			}
		})
	}
}

func TestFoldKey(t *testing.T) {
	tests := []struct {
		a, b    string
		collide bool
	}{
		{"Movies/Alien.mkv", "movies/ALIEN.MKV", true},
		{"Musik/Björk", "musik/BJÖRK", true},
		{"K", "K", true}, // Kelvin sign
		{"ΣΊΣΥΦΟΣ", "σίσυφος", true},
		{"a", "b", false},
		{"Movies/a.mkv", "Movies/a.mkv ", false},
		{"é", "é", false}, // normalization is not applied
	}
	for _, tt := range tests {
		if got := FoldKey(tt.a) == FoldKey(tt.b); got != tt.collide {
			t.Errorf("FoldKey(%q) == FoldKey(%q) is %v, want %v", tt.a, tt.b, got, tt.collide)
		}
	}
}
