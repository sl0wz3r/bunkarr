//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestDockerOffsiteS29 is acceptance 9 (docs/design/phase4.md S29, D33, §14.6 item 10): the
// credentials that can run jobs cannot choose where data goes. With the API key, and from a local
// address with the bypass on (disabled_for_local_addresses), creating an S3 destination, rotating
// its credentials, pinning SFTP host keys, linking a source to it and adding a Plex backup target
// on it answer 403, and so does accepting an unencrypted remote; a UI session with the right
// password gets 2xx; a wrong password answers 400 and is counted by the login limiter (the next
// login from the same address is refused too). Running jobs of an existing destination stays
// allowed with the API key.
func TestDockerOffsiteS29(t *testing.T) {
	o := newOffsite(t)
	o.startMinIO()
	o.startSFTP()
	o.writeRandom(map[string]int64{"s29/a/one.bin": 40000, "s29b/b/two.bin": 50000, "plex/Plex Media Server/Preferences.xml": 0})
	b := o.startBunkarr("bunkarr", bunkarrOpts{})
	srcA := b.createSource("S29 A", "s29")
	srcB := b.createSource("S29 B", "s29b")
	c := b.api.client()
	plexToken := "plex-token-" + randomHex(10)
	o.secrets.add("Plex token", plexToken)
	plexSettings := map[string]any{"dataPath": offsiteMedia + "/plex/Plex Media Server"}
	plexID := createIntegration(c, map[string]any{"type": "plex", "name": "Plex", "url": "http://plex.invalid:32400", "apiKey": plexToken,
		"settings": plexSettings})

	// The local-address bypass on: requests from 127.0.0.1 without credentials are allowed.
	b.api.call(viaSession, 200, "PUT", "/settings/general", map[string]any{"authenticationRequired": "disabled_for_local_addresses"}, nil)
	if code, body := b.api.status(viaNone, "GET", "/destinations", nil); code != 200 {
		t.Fatalf("GET /destinations through the local bypass: HTTP %d %s", code, body)
	}

	refused := func(what, method, path string, body map[string]any) {
		t.Helper()
		for _, v := range []via{viaKey, viaNone} {
			with := map[string]any{}
			for k, x := range body {
				with[k] = x
			}
			// The right password does not help without a session.
			with["currentPassword"] = e2ePassword
			code, resp := b.api.status(v, method, path, with)
			if code != http.StatusForbidden || !strings.Contains(string(resp), "log in and confirm your password") {
				t.Fatalf("%s %s: HTTP %d %s, want 403 (S29)", what, map[via]string{viaKey: "with the API key", viaNone: "through the local bypass"}[v], code, resp)
			}
		}
	}
	allowed := func(what string, want int, method, path string, body map[string]any) []byte {
		t.Helper()
		body["currentPassword"] = e2ePassword
		code, resp := b.api.status(viaSession, method, path, body)
		if code != want {
			t.Fatalf("%s with a session and the password: HTTP %d %s, want %d", what, code, resp, want)
		}
		return resp
	}

	// 1. Creating an S3 destination.
	s3 := func() map[string]any { return b.resticS3("Offsite S3", "s29", []int64{srcA.ID}, nil) }
	refused("create an S3 destination", "POST", "/destinations", s3())
	// A wrong password: 400, counted by the limiter (checked at the end).
	wrong := s3()
	wrong["currentPassword"] = "wrong-password-" + randomHex(4)
	if code, resp := b.api.status(viaSession, "POST", "/destinations", wrong); code != http.StatusBadRequest || !strings.Contains(string(resp), "password") {
		t.Fatalf("create with a wrong password: HTTP %d %s, want 400", code, resp)
	}
	var s3d offDest
	decodeInto(t, allowed("create an S3 destination", http.StatusCreated, "POST", "/destinations", s3()), &s3d)
	d3 := fmt.Sprintf("/destinations/%d", s3d.ID)

	// 2. Rotating its credentials (tested against the stored remote first; the same valid keys).
	rotate := func() map[string]any { return map[string]any{"credentials": o.s3Credentials()} }
	refused("rotate the storage credentials", "PUT", d3, rotate())
	allowed("rotate the storage credentials", http.StatusOK, "PUT", d3, rotate())

	// 3. Pinning SFTP host keys (a destination on the SFTP server; the same keys in another order
	// are a change of remote.hostKeys, as far as S29 is concerned).
	var sftpd offDest
	decodeInto(t, allowed("create an SFTP destination", http.StatusCreated, "POST", "/destinations", b.cryptSFTP("Offsite SFTP", "s29", nil, nil)), &sftpd)
	remote := b.sftpRemote("s29")
	keys := remote["hostKeys"].([]map[string]string)
	for i, j := 0, len(keys)-1; i < j; i, j = i+1, j-1 {
		keys[i], keys[j] = keys[j], keys[i]
	}
	// The whole remote is sent (the store refuses any other change of it); only hostKeys differ.
	remote["hostKeys"] = keys
	pin := func() map[string]any { return map[string]any{"remote": remote} }
	ds := fmt.Sprintf("/destinations/%d", sftpd.ID)
	refused("pin SFTP host keys", "PUT", ds, pin())
	allowed("pin SFTP host keys", http.StatusOK, "PUT", ds, pin())

	// 4. Linking a source to the S3 destination.
	link := func() map[string]any { return map[string]any{"sourceIds": []int64{srcA.ID, srcB.ID}} }
	refused("link a source", "PUT", d3, link())
	var linked offDest
	decodeInto(t, allowed("link a source", http.StatusOK, "PUT", d3, link()), &linked)
	if len(linked.SourceIDs) != 2 {
		t.Fatalf("sources after linking: %v", linked.SourceIDs)
	}

	// 5. Adding a Plex backup target on it.
	target := func() map[string]any {
		return map[string]any{"type": "plex", "name": "Plex", "url": "http://plex.invalid:32400",
			"settings": map[string]any{"dataPath": plexSettings["dataPath"], "backup": map[string]any{
				"targets": []map[string]any{{"destinationId": s3d.ID, "cron": "", "enabled": false}}}}}
	}
	ip := fmt.Sprintf("/integrations/%d", plexID)
	refused("add a Plex backup target", "PUT", ip, target())
	allowed("add a Plex backup target", http.StatusOK, "PUT", ip, target())

	// 6. Accepting an unencrypted remote.
	plain := b.cryptS3("Plain", "s29-plain", nil, nil)
	plain["encryption"] = map[string]any{"mode": "none", "acceptUnencrypted": true}
	refused("create an unencrypted rclone destination", "POST", "/destinations", plain)

	// Running jobs stays allowed with the API key.
	b.confirmKit(s3d.ID)
	requireStatus(t, b.sync(s3d.ID, nil), "completed")
	requireStatus(t, b.verify(s3d.ID), "completed")

	// The wrong password above was counted: a few more and the address is blocked, for the
	// password check and for logins alike.
	blocked := false
	for i := 0; i < 10 && !blocked; i++ {
		wrong := s3()
		wrong["currentPassword"] = "wrong-password-" + randomHex(4)
		code, resp := b.api.status(viaSession, "POST", "/destinations", wrong)
		switch code {
		case http.StatusBadRequest:
		case http.StatusTooManyRequests:
			blocked = true
		default:
			t.Fatalf("wrong password %d: HTTP %d %s", i+2, code, resp)
		}
	}
	if !blocked {
		t.Fatal("ten wrong passwords were never refused with 429: the login limiter does not count them")
	}
	if code, resp := b.api.status(viaNone, "POST", "/auth/login", map[string]any{"username": e2eUser, "password": e2ePassword}); code != http.StatusTooManyRequests {
		t.Fatalf("login after the wrong passwords: HTTP %d %s, want 429 (the same limiter)", code, resp)
	}
	t.Logf("S29: API key and local bypass refused on every off-site change; session + password accepted; wrong passwords blocked at %s", time.Now().Format(time.TimeOnly))
	o.auditAll()
}

// decodeInto decodes a JSON answer.
func decodeInto(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
}
