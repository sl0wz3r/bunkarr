package jobs

import "testing"

func TestValidTargetPath(t *testing.T) {
	for p, want := range map[string]bool{
		"Heat (1995)":              true,
		"The Office/Season 1":      true,
		".hack SIGN (2002)":        true,
		"Movie..Name (2001)":       true,
		"a..b/c":                   true,
		"Show/Season 1/S01E01.mkv": true,
		"":                         false,
		".":                        false,
		"..":                       false,
		"../x":                     false,
		"a/../b":                   false,
		"./x":                      false,
		"/x":                       false,
		"a/":                       false,
		"a//b":                     false,
		"a/./b":                    false,
		"a\x00b":                   false,
	} {
		if got := ValidTargetPath(p); got != want {
			t.Errorf("ValidTargetPath(%q) = %v, want %v", p, got, want)
		}
	}
}
