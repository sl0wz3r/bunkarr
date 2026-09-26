//go:build e2e

package testhooks

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestE2EOverrides(t *testing.T) {
	if !Enabled {
		t.Fatal("not Enabled in an e2e build")
	}
	// Without overrides: the defaults.
	if got := WebhookQuiet(5 * time.Second); got != 5*time.Second {
		t.Errorf("WebhookQuiet without an override = %v", got)
	}
	if got := ClockSkew(); got != 0 {
		t.Errorf("ClockSkew without an override = %v", got)
	}
	setAll(t)
	if got := WebhookQuiet(5 * time.Second); got != 100*time.Millisecond {
		t.Errorf("WebhookQuiet = %v", got)
	}
	if got := WebhookCap(20 * time.Second); got != 400*time.Millisecond {
		t.Errorf("WebhookCap = %v", got)
	}
	if got := WebhookDeleteDelay(time.Minute); got != time.Second {
		t.Errorf("WebhookDeleteDelay = %v", got)
	}
	if got := WebhookUpgradeHold(30 * time.Minute); got != 3*time.Second {
		t.Errorf("WebhookUpgradeHold = %v", got)
	}
	if got := PlexTVURL("https://plex.tv"); got != "http://127.0.0.1:1" {
		t.Errorf("PlexTVURL = %q", got)
	}
	if got := ClientsPlexTVURL("https://clients.plex.tv"); got != "http://127.0.0.1:2" {
		t.Errorf("ClientsPlexTVURL = %q", got)
	}
	now := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	if got := FreshnessNow(now); !got.Equal(now.Add(25 * time.Hour)) {
		t.Errorf("FreshnessNow = %v", got)
	}
	// An invalid value falls back to the default.
	t.Setenv(EnvWebhookQuiet, "-1s")
	if got := WebhookQuiet(5 * time.Second); got != 5*time.Second {
		t.Errorf("WebhookQuiet with a negative override = %v", got)
	}
	t.Setenv(EnvPlexTVURL, "plex.tv")
	if got := PlexTVURL("https://plex.tv"); got != "https://plex.tv" {
		t.Errorf("PlexTVURL with a relative override = %q", got)
	}
}

// TestE2EFileOverrideIsReadOnEveryCall: a running binary picks up a changed _FILE value.
func TestE2EFileOverrideIsReadOnEveryCall(t *testing.T) {
	file := filepath.Join(t.TempDir(), "skew")
	t.Setenv(EnvClockSkew+fileSuffix, file)
	if got := ClockSkew(); got != 0 {
		t.Errorf("ClockSkew with a missing file = %v", got)
	}
	if err := os.WriteFile(file, []byte(" 2h \nignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ClockSkew(); got != 2*time.Hour {
		t.Errorf("ClockSkew = %v; want 2h", got)
	}
	if err := os.WriteFile(file, []byte("26h"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ClockSkew(); got != 26*time.Hour {
		t.Errorf("ClockSkew after the change = %v; want 26h", got)
	}
	// The variable itself wins over the file.
	t.Setenv(EnvClockSkew, "1h")
	if got := ClockSkew(); got != time.Hour {
		t.Errorf("ClockSkew with both = %v; want 1h", got)
	}
}
