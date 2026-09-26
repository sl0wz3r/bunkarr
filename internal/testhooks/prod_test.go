//go:build !e2e

package testhooks

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestProductionIgnoresOverrides: a production build returns the defaults whatever the
// environment says, also through the _FILE variants (SEC11).
func TestProductionIgnoresOverrides(t *testing.T) {
	if Enabled {
		t.Fatal("Enabled in a build without the e2e tag")
	}
	setAll(t)
	file := filepath.Join(t.TempDir(), "skew")
	if err := os.WriteFile(file, []byte("48h\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvClockSkew+fileSuffix, file)
	if got := WebhookQuiet(5 * time.Second); got != 5*time.Second {
		t.Errorf("WebhookQuiet = %v", got)
	}
	if got := WebhookCap(20 * time.Second); got != 20*time.Second {
		t.Errorf("WebhookCap = %v", got)
	}
	if got := WebhookDeleteDelay(time.Minute); got != time.Minute {
		t.Errorf("WebhookDeleteDelay = %v", got)
	}
	if got := WebhookUpgradeHold(30 * time.Minute); got != 30*time.Minute {
		t.Errorf("WebhookUpgradeHold = %v", got)
	}
	if got := PlexTVURL("https://plex.tv"); got != "https://plex.tv" {
		t.Errorf("PlexTVURL = %q", got)
	}
	if got := ClientsPlexTVURL("https://clients.plex.tv"); got != "https://clients.plex.tv" {
		t.Errorf("ClientsPlexTVURL = %q", got)
	}
	if got := ClockSkew(); got != 0 {
		t.Errorf("ClockSkew = %v", got)
	}
	now := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	if got := FreshnessNow(now); !got.Equal(now) {
		t.Errorf("FreshnessNow = %v", got)
	}
}
