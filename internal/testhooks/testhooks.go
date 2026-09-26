// Package testhooks holds the few values that end-to-end tests must be able to change in a real
// Bunkarr binary (docs/design/phase2-3.md §14.2, §15 E2E): the webhook processor's windows, the
// plex.tv base URLs and a clock skew for cache freshness.
//
// Overrides are honoured only in binaries built with -tags e2e (Enabled). A production build
// never reads the environment here: every accessor returns the default its caller passes (the
// constant of the calling package), so a stray BUNKARR_TEST_* variable cannot redirect the Plex
// sign-in to another host (safety rule S16, review finding SEC11) or change a safety window.
//
// In an e2e build each value comes from the environment variable named below or, when
// "<name>_FILE" is set instead, from the first line of that file, read again on every call so a
// test can change a running binary. A value that does not parse (or is out of range) is ignored
// and the default is used.
package testhooks

import (
	"net/url"
	"strings"
	"time"
)

// Enabled reports whether this binary honours the overrides (built with -tags e2e).
const Enabled = enabled

// Environment variables of the overrides (each may also be given as <name>_FILE).
const (
	// EnvWebhookQuiet is the quiet window after a change or download event (default 5 s): a
	// duration such as "200ms".
	EnvWebhookQuiet = "BUNKARR_TEST_WEBHOOK_QUIET"
	// EnvWebhookCap caps how long an item's events can keep postponing its flush (default 20 s).
	EnvWebhookCap = "BUNKARR_TEST_WEBHOOK_CAP"
	// EnvWebhookDeleteDelay is the delay of the delete class (default 60 s).
	EnvWebhookDeleteDelay = "BUNKARR_TEST_WEBHOOK_DELETE_DELAY"
	// EnvWebhookUpgradeHold is how long an upgrade delete waits for its download (default 30 min).
	EnvWebhookUpgradeHold = "BUNKARR_TEST_WEBHOOK_UPGRADE_HOLD"
	// EnvPlexTVURL replaces https://plex.tv (the PINs and the account): an absolute http(s) URL.
	// The plex.tv helper still refuses http unless the host is loopback.
	EnvPlexTVURL = "BUNKARR_TEST_PLEXTV_URL"
	// EnvClientsPlexTVURL replaces https://clients.plex.tv (the account's resources).
	EnvClientsPlexTVURL = "BUNKARR_TEST_CLIENTS_PLEXTV_URL"
	// EnvClockSkew is added to the clock that decides whether a metadata cache is fresh: a signed
	// duration such as "25h" (every cache refreshed less than 25 hours ago then looks 25 hours
	// older).
	EnvClockSkew = "BUNKARR_TEST_CLOCK_SKEW"
)

// fileSuffix names the variable that points at a file holding the value instead.
const fileSuffix = "_FILE"

// WebhookQuiet returns the webhook quiet window: def, or the positive override.
func WebhookQuiet(def time.Duration) time.Duration { return positiveDuration(EnvWebhookQuiet, def) }

// WebhookCap returns the cap of an item's quiet window: def, or the positive override.
func WebhookCap(def time.Duration) time.Duration { return positiveDuration(EnvWebhookCap, def) }

// WebhookDeleteDelay returns the delay of the delete class: def, or the positive override.
func WebhookDeleteDelay(def time.Duration) time.Duration {
	return positiveDuration(EnvWebhookDeleteDelay, def)
}

// WebhookUpgradeHold returns how long an upgrade delete is held: def, or the positive override.
func WebhookUpgradeHold(def time.Duration) time.Duration {
	return positiveDuration(EnvWebhookUpgradeHold, def)
}

// PlexTVURL returns the plex.tv base URL: def, or the override when it is an absolute http(s)
// URL.
func PlexTVURL(def string) string { return baseURL(EnvPlexTVURL, def) }

// ClientsPlexTVURL returns the clients.plex.tv base URL: def, or the override when it is an
// absolute http(s) URL.
func ClientsPlexTVURL(def string) string { return baseURL(EnvClientsPlexTVURL, def) }

// ClockSkew returns the skew added to the freshness clock: 0, or the override.
func ClockSkew() time.Duration {
	s, ok := value(EnvClockSkew)
	return parseSkew(s, ok)
}

// FreshnessNow returns now plus ClockSkew: the time a freshness check compares a cache's last
// complete refresh with.
func FreshnessNow(now time.Time) time.Time {
	return now.Add(ClockSkew())
}

// positiveDuration returns the override of name when it is a duration > 0, else def.
func positiveDuration(name string, def time.Duration) time.Duration {
	s, ok := value(name)
	return parsePositive(s, ok, def)
}

// baseURL returns the override of name when it is a base URL (parseBaseURL), else def.
func baseURL(name, def string) string {
	s, ok := value(name)
	return parseBaseURL(s, ok, def)
}

// parsePositive returns s as a duration when ok and it is one > 0, else def.
func parsePositive(s string, ok bool, def time.Duration) time.Duration {
	if !ok {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// parseSkew returns s as a signed duration when ok and it is one, else 0.
func parseSkew(s string, ok bool) time.Duration {
	if !ok {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0
	}
	return d
}

// parseBaseURL returns s when ok and it is an absolute http or https URL with a host and no
// query, fragment or credentials, else def.
func parseBaseURL(s string, ok bool, def string) string {
	if !ok {
		return def
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" {
		return def
	}
	return s
}

// value returns the trimmed override of name (see lookup), and whether there is a non-empty one.
func value(name string) (string, bool) {
	s, ok := lookup(name)
	if !ok {
		return "", false
	}
	s = strings.TrimSpace(s)
	return s, s != ""
}
