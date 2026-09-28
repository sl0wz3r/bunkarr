package jobs

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

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

func TestAsDeferred(t *testing.T) {
	until := time.Date(2026, 9, 28, 1, 0, 0, 0, time.UTC)
	err := fmt.Errorf("sync: %w", Defer(until, "waiting for the transfer window"))
	d, ok := AsDeferred(err)
	if !ok || !d.Until.Equal(until) || d.Reason != "waiting for the transfer window" {
		t.Fatalf("AsDeferred(%v) = %+v, %v", err, d, ok)
	}
	if _, ok := AsDeferred(errors.New("other")); ok {
		t.Error("AsDeferred accepted a plain error")
	}
	if _, ok := AsDeferred(nil); ok {
		t.Error("AsDeferred accepted nil")
	}
}
