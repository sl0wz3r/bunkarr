package api

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"mime"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
)

var (
	kitFileRe  = regexp.MustCompile(`^bunkarr-recovery-[a-z0-9-]{1,40}-\d{8}\.txt$`)
	checkRe    = regexp.MustCompile(`\b[A-Z2-7]{4}-[A-Z2-7]{4}\b`)
	kitWrongPw = map[string]any{"currentPassword": "wrong horse"}
)

// waitNotification waits until the fake Apprise got a message whose title contains want.
func waitNotification(t *testing.T, f *fakeApprise, want string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := f.requests()
		for _, m := range got {
			if title, _ := m["title"].(string); strings.Contains(title, want) {
				return m
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	got, _ := f.requests()
	t.Fatalf("no notification titled %q: %v", want, got)
	return nil
}

// TestRecoveryKitEndpoints checks the export and the confirmation of a recovery kit (phase4.md
// §5.2): both need a UI session (403 for the API key and the local bypass), the export the user's
// password (a wrong one is 400 and counted by the login limiter), confirmation attempts are
// counted too; the export is a text/plain attachment whose Content-Disposition stays well formed
// with a destination name holding a quote, and it sends a warning notification and a process-log
// line without content. The custody then unblocks the destination's jobs.
func TestRecoveryKitEndpoints(t *testing.T) {
	logs := &lockedBuffer{}
	ee := newEngineEnv(t, engineEnvOptions{tweak: func(o *AppOptions) {
		o.Log = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}})
	c := ee.session(t)
	apprise := newFakeApprise(t)
	ee.call(t, 201, "POST", "/notifications", map[string]any{"name": "Phone", "apiUrl": apprise.srv.URL, "configKey": "phone"}, nil)

	id := ee.createEngineDest(t, c, s3Body(`Off "site" B2`, "restic", "media", "kit"))
	values := ee.secretValues(t, id)
	path := fmt.Sprintf("/destinations/%d/recovery-kit", id)

	// Export: the API key, the local bypass, a wrong password.
	if code, msg := ee.status(t, "POST", path, map[string]any{"currentPassword": sessionPassword}); code != 403 || msg != msgKitSession {
		t.Fatalf("export with the API key: %d %q", code, msg)
	}
	if code, msg := ee.localBypass(t, "POST", path, map[string]any{"currentPassword": sessionPassword}); code != 403 {
		t.Fatalf("export through the local bypass: %d %q", code, msg)
	}
	for range 5 {
		if code, raw, _ := ee.sessionRaw(t, c, "POST", path, kitWrongPw); code != 400 || !strings.Contains(string(raw), msgWrongPassword) {
			t.Fatalf("export with a wrong password: %d %s", code, raw)
		}
	}
	if code, _, _ := ee.sessionRaw(t, c, "POST", path, map[string]any{"currentPassword": sessionPassword}); code != 429 {
		t.Fatalf("export after five wrong passwords: %d, want 429 (the limiter did not count them)", code)
	}
	ee.auth.Limiter.Success(testLocalIP)
	var before destinationView
	ee.call(t, 200, "GET", fmt.Sprintf("/destinations/%d", id), nil, &before)
	if before.Encryption.KitExportedAt != nil || before.BlockedReason != destinations.BlockedKit {
		t.Fatalf("before the export: %+v", before.Encryption)
	}

	code, kit, hdr := ee.sessionRaw(t, c, "POST", path, map[string]any{"currentPassword": sessionPassword})
	if code != 200 || !strings.HasPrefix(hdr.Get("Content-Type"), "text/plain") {
		t.Fatalf("export: %d %q", code, hdr.Get("Content-Type"))
	}
	disp, params, err := mime.ParseMediaType(hdr.Get("Content-Disposition"))
	if err != nil || disp != "attachment" || !kitFileRe.MatchString(params["filename"]) {
		t.Fatalf("Content-Disposition %q: %v %q %v", hdr.Get("Content-Disposition"), err, disp, params)
	}
	found := false
	for _, v := range values {
		found = found || bytes.Contains(kit, []byte(v))
	}
	if !found {
		t.Fatal("the kit does not hold the encryption secret")
	}
	code2 := checkRe.Find(kit)
	if code2 == nil {
		t.Fatalf("no check code in the kit:\n%s", kit)
	}
	m := waitNotification(t, apprise, `Recovery kit for Off "site" B2 exported`)
	if body, _ := m["body"].(string); !strings.Contains(body, "from "+testLocalIP) || !strings.Contains(body, "exported at") {
		t.Fatalf("notification body %q", body)
	}
	if !strings.Contains(logs.String(), "Recovery kit exported") {
		t.Fatal("no process-log line for the export")
	}
	noSecrets(t, "process log", []byte(logs.String()), values)
	if strings.Contains(logs.String(), string(code2)) {
		t.Fatal("the process log holds the check code")
	}

	// Confirmation: session only, rate limited, the right code unblocks the destination.
	confirm := path + "/confirm"
	if code, msg := ee.status(t, "POST", confirm, map[string]any{"checkCode": string(code2)}); code != 403 || msg != msgConfirmSession {
		t.Fatalf("confirm with the API key: %d %q", code, msg)
	}
	if code, _ := ee.localBypass(t, "POST", confirm, map[string]any{"checkCode": string(code2)}); code != 403 {
		t.Fatalf("confirm through the local bypass: %d", code)
	}
	for _, body := range []map[string]any{{}, {"checkCode": string(code2), "secret": testUserSecret}, {"checkCode": "   "}} {
		if code, raw, _ := ee.sessionRaw(t, c, "POST", confirm, body); code != 400 {
			t.Fatalf("confirm %v: %d %s", body, code, raw)
		}
	}
	for range 5 {
		if code, raw, _ := ee.sessionRaw(t, c, "POST", confirm, map[string]any{"checkCode": "AAAA-AAAA"}); code != 400 ||
			!strings.Contains(string(raw), "wrong check code") {
			t.Fatalf("confirm with a wrong code: %d %s", code, raw)
		}
	}
	if code, _, _ := ee.sessionRaw(t, c, "POST", confirm, map[string]any{"checkCode": string(code2)}); code != 429 {
		t.Fatalf("confirm after five wrong codes: %d, want 429", code)
	}
	ee.auth.Limiter.Success(testLocalIP)
	// Case and dashes are ignored.
	lower := strings.ToLower(strings.ReplaceAll(string(code2), "-", ""))
	ee.sessionCall(t, c, 204, "POST", confirm, map[string]any{"checkCode": lower}, nil)
	var after destinationView
	ee.call(t, 200, "GET", fmt.Sprintf("/destinations/%d", id), nil, &after)
	if after.Encryption.KitExportedAt == nil || after.Encryption.KitConfirmedAt == nil || after.BlockedReason != "" {
		t.Fatalf("after the confirmation: %+v %q", after.Encryption, after.BlockedReason)
	}

	// A secret typed at create is confirmed by typing it again.
	own := s3Body("Own password", "restic", "media", "own")
	own["encryption"] = map[string]any{"mode": "restic", "generate": false, "secret": testUserSecret}
	ownID := ee.createEngineDest(t, c, own)
	ownConfirm := fmt.Sprintf("/destinations/%d/recovery-kit/confirm", ownID)
	if code, raw, _ := ee.sessionRaw(t, c, "POST", ownConfirm, map[string]any{"secret": testUserSecret + "x"}); code != 400 {
		t.Fatalf("confirm with a wrong secret: %d %s", code, raw)
	}
	ee.sessionCall(t, c, 204, "POST", ownConfirm, map[string]any{"secret": testUserSecret}, nil)

	// A filecopy destination has no kit; a missing one is 404.
	nas := ee.createDestination(t, "NAS", ee.mkdir(t, "nas"), nil, nil)
	if code, raw, _ := ee.sessionRaw(t, c, "POST", fmt.Sprintf("/destinations/%d/recovery-kit", nas),
		map[string]any{"currentPassword": sessionPassword}); code != 400 {
		t.Fatalf("kit of a filecopy destination: %d %s", code, raw)
	}
	if code, _, _ := ee.sessionRaw(t, c, "POST", "/destinations/999/recovery-kit", map[string]any{"currentPassword": sessionPassword}); code != 404 {
		t.Fatalf("kit of a missing destination: %d", code)
	}
}

// TestKitReminders: a destination whose recovery kit custody is not confirmed gets "Recovery kit
// not confirmed for <destination>" once a day, from a day after it was created; a confirmed one
// gets none.
func TestKitReminders(t *testing.T) {
	ee := newEngineEnv(t, engineEnvOptions{})
	c := ee.session(t)
	apprise := newFakeApprise(t)
	ee.call(t, 201, "POST", "/notifications", map[string]any{"name": "Phone", "apiUrl": apprise.srv.URL, "configKey": "phone"}, nil)
	ee.createEngineDest(t, c, s3Body("Unconfirmed", "restic", "media", "a"))
	attached := s3Body("Attached", "restic", "media", "b")
	attached["attach"] = true
	attached["encryption"] = map[string]any{"mode": "restic", "generate": false, "secret": testUserSecret}
	ee.createEngineDest(t, c, attached)
	ee.createDestination(t, "NAS", ee.mkdir(t, "nas"), nil, nil)

	k := ee.app.reminders
	now := time.Now()
	count := func() int {
		got, _ := apprise.requests()
		n := 0
		for _, m := range got {
			if title, _ := m["title"].(string); strings.Contains(title, "Recovery kit not confirmed for") {
				n++
				if !strings.Contains(title, "Unconfirmed") {
					t.Errorf("reminder for another destination: %q", title)
				}
			}
		}
		return n
	}
	settle := func(want int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for count() != want && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if got := count(); got != want {
			t.Fatalf("%d reminders, want %d", got, want)
		}
	}
	at := func(d time.Duration) {
		k.now = func() time.Time { return now.Add(d) }
		k.check(context.Background())
	}
	at(time.Hour) // a new destination: not yet
	settle(0)
	at(25 * time.Hour)
	settle(1)
	at(30 * time.Hour) // once a day
	settle(1)
	at(50 * time.Hour)
	settle(2)
}
