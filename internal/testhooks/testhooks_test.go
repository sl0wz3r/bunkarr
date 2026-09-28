package testhooks

import (
	"testing"
	"time"
)

func TestParsePositive(t *testing.T) {
	const def = 5 * time.Second
	for _, tc := range []struct {
		in   string
		ok   bool
		want time.Duration
	}{
		{"", false, def},
		{"200ms", false, def},
		{"200ms", true, 200 * time.Millisecond},
		{"2m", true, 2 * time.Minute},
		{"0s", true, def},
		{"-1s", true, def},
		{"soon", true, def},
		{"5", true, def},
	} {
		if got := parsePositive(tc.in, tc.ok, def); got != tc.want {
			t.Errorf("parsePositive(%q, %v) = %v; want %v", tc.in, tc.ok, got, tc.want)
		}
	}
}

func TestParseSkew(t *testing.T) {
	for _, tc := range []struct {
		in   string
		ok   bool
		want time.Duration
	}{
		{"25h", false, 0},
		{"25h", true, 25 * time.Hour},
		{"-90m", true, -90 * time.Minute},
		{"0", true, 0},
		{"a day", true, 0},
	} {
		if got := parseSkew(tc.in, tc.ok); got != tc.want {
			t.Errorf("parseSkew(%q, %v) = %v; want %v", tc.in, tc.ok, got, tc.want)
		}
	}
}

func TestParseBaseURL(t *testing.T) {
	const def = "https://plex.tv"
	for _, tc := range []struct {
		in   string
		ok   bool
		want string
	}{
		{"http://127.0.0.1:5555", false, def},
		{"http://127.0.0.1:5555", true, "http://127.0.0.1:5555"},
		{"https://fake.plex.test/base", true, "https://fake.plex.test/base"},
		{"ftp://127.0.0.1", true, def},
		{"127.0.0.1:5555", true, def},
		{"/relative", true, def},
		{"https://user:pw@evil.example", true, def},
		{"https://evil.example/?x=1", true, def},
		{"https://evil.example/#f", true, def},
		{"https://", true, def},
	} {
		if got := parseBaseURL(tc.in, tc.ok, def); got != tc.want {
			t.Errorf("parseBaseURL(%q, %v) = %q; want %q", tc.in, tc.ok, got, tc.want)
		}
	}
}

// setAll sets every override to a valid value.
func setAll(t *testing.T) {
	t.Helper()
	t.Setenv(EnvWebhookQuiet, "100ms")
	t.Setenv(EnvWebhookCap, "400ms")
	t.Setenv(EnvWebhookDeleteDelay, "1s")
	t.Setenv(EnvWebhookUpgradeHold, "3s")
	t.Setenv(EnvPlexTVURL, "http://127.0.0.1:1")
	t.Setenv(EnvClientsPlexTVURL, "http://127.0.0.1:2")
	t.Setenv(EnvClockSkew, "25h")
	t.Setenv(EnvWindow, testWindowValue)
}

// testWindowValue is the BUNKARR_TEST_WINDOW value setAll sets: destination 7, open at 10:00:00,
// closed at 10:00:15, open again at 10:01:00.
const testWindowValue = "7,2026-09-27T10:00:00Z,2026-09-27T10:00:15Z,2026-09-27T10:01:00Z"

func TestParseWindow(t *testing.T) {
	at := func(s string) time.Time {
		tm, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	tests := []struct {
		in string
		ok bool
		id int64
	}{
		{testWindowValue, true, 7},
		{" 7, 2026-09-27T10:00:00Z ,2026-09-27T10:00:15Z,2026-09-27T10:01:00Z", true, 7},
		{"7,2026-09-27T12:00:00+02:00,2026-09-27T10:00:15Z,2026-09-27T10:00:15Z", true, 7}, // reopen == close
		{testWindowValue, false, 8}, // another destination
		{"7,2026-09-27T10:00:15Z,2026-09-27T10:00:00Z,2026-09-27T10:01:00Z", false, 7}, // close before open
		{"7,2026-09-27T10:00:00Z,2026-09-27T10:00:15Z,2026-09-27T10:00:10Z", false, 7}, // reopen before close
		{"7,2026-09-27T10:00:00Z,2026-09-27T10:00:15Z", false, 7},                      // three fields
		{"7,yesterday,2026-09-27T10:00:15Z,2026-09-27T10:01:00Z", false, 7},            // not RFC3339
		{"x,2026-09-27T10:00:00Z,2026-09-27T10:00:15Z,2026-09-27T10:01:00Z", false, 7}, // bad id
		{"0,2026-09-27T10:00:00Z,2026-09-27T10:00:15Z,2026-09-27T10:01:00Z", false, 0}, // id 0
	}
	for _, tc := range tests {
		open, closeAt, reopen, ok := parseWindow(tc.in, true, tc.id)
		if ok != tc.ok {
			t.Errorf("parseWindow(%q, %d) ok = %v", tc.in, tc.id, ok)
			continue
		}
		if ok && tc.in == testWindowValue && (!open.Equal(at("2026-09-27T10:00:00Z")) ||
			!closeAt.Equal(at("2026-09-27T10:00:15Z")) || !reopen.Equal(at("2026-09-27T10:01:00Z"))) {
			t.Errorf("parseWindow(%q) = %v %v %v", tc.in, open, closeAt, reopen)
		}
	}
	if _, _, _, ok := parseWindow(testWindowValue, false, 7); ok {
		t.Error("parseWindow without a value found a window")
	}
}
