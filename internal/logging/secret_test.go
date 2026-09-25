package logging

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestRegisteredSecretValuesAreRedacted(t *testing.T) {
	const token = "PLEXtok3n-value-9f8e7d"
	RegisterSecret(token)
	RegisterSecret("short") // ignored: too short to register safely

	var out bytes.Buffer
	log, closer, err := New(Options{Level: "info", StdoutFormat: "json", Stdout: &out})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	urlErr := fmt.Errorf("Get \"http://plex:32400/library/sections?X-Plex-Token=%s\": dial tcp: refused", token)
	log.Info("request to "+token+" failed", "error", urlErr, "detail", "token was "+token, "wrapped", errors.Join(urlErr))

	got := out.String()
	if strings.Contains(got, token) {
		t.Fatalf("secret value leaked: %s", got)
	}
	if strings.Count(got, Redacted) < 4 {
		t.Fatalf("expected the message and three attributes redacted: %s", got)
	}
	if RedactSecrets("a short thing") != "a short thing" {
		t.Fatal("short strings must not be redacted")
	}
	if !ContainsSecret("x"+token+"y") || ContainsSecret("nothing here") {
		t.Fatal("ContainsSecret gave a wrong answer")
	}
}

func TestOverlappingSecretsRedactedWhole(t *testing.T) {
	const token = "overlapTOKEN12345"
	const url = "json://apprise.local/overlapTOKEN12345/other-secret-part"
	// Register the short one first: order of registration must not matter.
	RegisterSecret(token)
	RegisterSecret(url)
	got := RedactSecrets("sending to " + url + " failed")
	if strings.Contains(got, "other-secret-part") || strings.Contains(got, token) {
		t.Fatalf("overlapping secret partly leaked: %q", got)
	}
	if got != "sending to "+Redacted+" failed" {
		t.Fatalf("got %q", got)
	}
}

// isolateSecrets gives the test an empty registry and restores the previous one afterwards.
func isolateSecrets(t *testing.T) {
	t.Helper()
	secretsMu.Lock()
	saved := secrets
	secrets = secretRegistry{}
	secretsMu.Unlock()
	t.Cleanup(func() {
		secretsMu.Lock()
		secrets = saved
		secretsMu.Unlock()
	})
}

// TestReleasedSecretsAreBounded: an owner's replaced values are released; the registry keeps the
// held values plus a bounded number of the most recently released ones, longest first.
func TestReleasedSecretsAreBounded(t *testing.T) {
	isolateSecrets(t)
	SetSecrets("integration:1", "stored-token-held-throughout")
	SetSecrets("notification:1", "json://first-url-released-first", "json://second-url-released-first")
	n := 3 * maxReleased
	for i := range n {
		SetSecrets("notification:2", fmt.Sprintf("bounded-secret-%06d", i))
	}
	secretsMu.RLock()
	count, held := len(secrets.sorted), len(secrets.holders)
	secretsMu.RUnlock()
	if count > held+maxReleased {
		t.Fatalf("registry holds %d values (%d held), want at most %d", count, held, held+maxReleased)
	}
	if !ContainsSecret("x stored-token-held-throughout y") || !ContainsSecret("x json://first-url-released-first y") {
		t.Fatal("a held value was dropped")
	}
	if ContainsSecret("x bounded-secret-000000 y") {
		t.Fatal("the oldest released value was kept")
	}
	if !ContainsSecret(fmt.Sprintf("x bounded-secret-%06d y", n-2)) || !ContainsSecret(fmt.Sprintf("x bounded-secret-%06d y", n-1)) {
		t.Fatal("the current or a recently released value is not redacted")
	}
	// Releasing an owner keeps its values redacted for a while; replacing them with a value they
	// share keeps it held.
	SetSecrets("notification:1", "json://second-url-released-first")
	SetSecrets("notification:1")
	if !ContainsSecret("x json://first-url-released-first y") || !ContainsSecret("x json://second-url-released-first y") {
		t.Fatal("a just-released value is no longer redacted")
	}
	// Longest first still holds after drops.
	SetSecrets("notification:3", "bounded-secret-999999-and-more")
	if got := RedactSecrets("see bounded-secret-999999-and-more"); got != "see "+Redacted {
		t.Fatalf("got %q", got)
	}
}

// TestSharedSecretsStayHeld: a value two owners hold (the same token saved twice), or that is
// also registered for good, stays held when one owner releases it; values under 8 bytes and
// duplicates are ignored.
func TestSharedSecretsStayHeld(t *testing.T) {
	isolateSecrets(t)
	SetSecrets("integration:1", "shared-token-12345", "short", "shared-token-12345")
	SetSecrets("integration:2", "shared-token-12345")
	RegisterSecret("pinned-and-owned-678")
	SetSecrets("integration:3", "pinned-and-owned-678")
	SetSecrets("integration:1")
	SetSecrets("integration:3")
	secretsMu.RLock()
	holders := secrets.holders
	secretsMu.RUnlock()
	if holders["shared-token-12345"] != 1 || holders["pinned-and-owned-678"] != 1 || len(holders) != 2 {
		t.Fatalf("holders = %v", holders)
	}
	if ContainsSecret("a short text") {
		t.Fatal("a value under 8 bytes was registered")
	}
}

func TestRedactValuesDoesNotRegister(t *testing.T) {
	isolateSecrets(t)
	const typed = "typed-into-a-form-7c1d"
	if got := RedactValues("token "+typed+" was refused", typed, "short", ""); got != "token "+Redacted+" was refused" {
		t.Fatalf("got %q", got)
	}
	if ContainsSecret("x " + typed + " y") {
		t.Fatal("RedactValues registered its value")
	}
	if got := RedactValues("short stays", "short"); got != "short stays" {
		t.Fatalf("a value under 8 bytes was redacted: %q", got)
	}
	// A registered secret that contains the local value is replaced whole, and so is a local
	// value that contains a registered secret.
	const stored = "json://apprise.local/typed-into-a-form-7c1d/rest-of-it"
	RegisterSecret(stored)
	if got := RedactValues("to "+stored, typed); got != "to "+Redacted {
		t.Fatalf("got %q", got)
	}
	const local = "discord://id/" + stored + "/extra-local-part"
	if got := RedactValues("to "+local, local); got != "to "+Redacted {
		t.Fatalf("got %q", got)
	}
}

// TestRegisteredSecretsAreNeverDropped: a value registered with RegisterSecret (a PlexOnlineToken
// read from Plex's Preferences.xml, which nothing registers again) stays redacted however many
// other values are registered or released after it.
func TestRegisteredSecretsAreNeverDropped(t *testing.T) {
	isolateSecrets(t)
	const pinned = "plex-online-token-registered-once"
	RegisterSecret(pinned)
	for i := range 5000 {
		RegisterSecret(fmt.Sprintf("later-secret-%06d", i))
	}
	if !ContainsSecret("x " + pinned + " y") {
		t.Fatal("a registered secret was dropped")
	}
}
