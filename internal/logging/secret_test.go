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
