package destinations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/rclone"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
)

// kitJSONBlock parses the JSON block at the end of a kit.
func kitJSONBlock(t *testing.T, content string) kitJSON {
	t.Helper()
	var k kitJSON
	if err := json.Unmarshal([]byte(between(t, content, kitJSONBegin, kitJSONEnd)), &k); err != nil {
		t.Fatalf("kit JSON: %v", err)
	}
	return k
}

// A kit exported while the create is pending (§4.5 allows it, so the secret of an initialized
// repository is not lost) confirms with its check code after the create was finished: the code
// does not depend on the marker_id, which the finish changes from "pending:<uuid>".
func TestKitExportedWhilePendingConfirmsAfterFinish(t *testing.T) {
	ef := newEngineFixture(t)
	var initialized engines.EncryptionSecret
	ef.restic.OnCreate = func(_ context.Context, d engines.Destination, s engines.Secrets, attach bool) (engines.CreateResult, error) {
		if !strings.Contains(d.Target, "pending-kit") {
			return engines.CreateResult{MarkerID: "restic:other", Initialized: !attach}, nil
		}
		if !attach {
			initialized = s.Encryption
			return engines.CreateResult{MarkerID: "restic:fresh", Initialized: true}, nil
		}
		if s.Encryption != initialized {
			return engines.CreateResult{}, engines.ErrWrongPassword
		}
		return engines.CreateResult{MarkerID: "restic:fresh"}, nil
	}
	faultinject.SetHook(faultinject.CrashAt(PointCreateAfterInit, 1))
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("no crash")
			} else if _, ok := r.(faultinject.Crash); !ok {
				panic(r)
			}
		}()
		_, _ = ef.store.Create(ef.ctx, s3Input(t, "Pending kit", "pending-kit", ""), CreateOptions{})
	}()
	faultinject.SetHook(nil)
	all, err := ef.store.List(ef.ctx)
	if err != nil || len(all) != 1 || !all[0].Pending {
		t.Fatalf("after the crash: %v %+v", err, all)
	}
	kit, err := ef.store.RecoveryKit(ef.ctx, all[0].ID, false)
	if err != nil {
		t.Fatalf("kit of the pending row: %v", err)
	}
	code := kitCheckCode(t, kit.Content)
	if m := kitJSONBlock(t, kit.Content).MarkerID; !strings.HasPrefix(m, pendingPrefix) {
		t.Fatalf("the kit was not exported while pending (marker %q)", m)
	}
	d, err := ef.store.Create(ef.ctx, s3Input(t, "Pending kit", "pending-kit", ""), CreateOptions{})
	if err != nil {
		t.Fatalf("finish the create: %v", err)
	}
	if d.ID != all[0].ID || d.Pending || d.MarkerID != "restic:fresh" || Blocked(d) != BlockedKit {
		t.Fatalf("finished %+v (blocked %q)", d, Blocked(d))
	}
	if err := ef.store.ConfirmKit(ef.ctx, d.ID, KitConfirmation{CheckCode: code}); err != nil {
		t.Fatalf("the check code of the kit exported while pending: %v", err)
	}
	if got, _ := ef.store.Get(ef.ctx, d.ID); Blocked(got) != "" {
		t.Errorf("still blocked: %q", Blocked(got))
	}
	// A kit exported now prints the same code; another destination's code does not confirm.
	again, err := ef.store.RecoveryKit(ef.ctx, d.ID, false)
	if err != nil || kitCheckCode(t, again.Content) != code {
		t.Errorf("the finished row's kit prints another check code (%v)", err)
	}
	other := ef.createS3(t, "Other", "other-kit", "")
	if err := ef.store.ConfirmKit(ef.ctx, other.ID, KitConfirmation{CheckCode: code}); !errors.Is(err, ErrWrongCheckCode) {
		t.Errorf("another destination's code: %v", err)
	}
}

func TestPlanEncryptionSecret2(t *testing.T) {
	const pw, pw2 = "my own crypt password", "my own crypt password2"
	yes, no := true, false
	for _, c := range []struct {
		name   string
		engine string
		in     EncryptionInput
		want   string
	}{
		{"restic", EngineRestic, EncryptionInput{Secret: pw, Secret2: pw2}, "restic repository has one password"},
		{"filecopy", EngineFilecopy, EncryptionInput{Secret2: pw2}, "filecopy"},
		{"none", EngineRclone, EncryptionInput{Mode: engines.EncryptionNone, AcceptUnencrypted: true, Secret2: pw2}, "mode none has no secret"},
		{"generated", EngineRclone, EncryptionInput{Secret2: pw2}, "needs encryption.secret"},
		{"generate true", EngineRclone, EncryptionInput{Generate: &yes, Secret2: pw2}, "needs encryption.secret"},
		{"short", EngineRclone, EncryptionInput{Secret: pw, Secret2: "short salt"}, "encryption.secret2: at least 16"},
		{"whitespace", EngineRclone, EncryptionInput{Secret: pw, Secret2: " " + pw2}, "encryption.secret2: no leading"},
		{"control", EngineRclone, EncryptionInput{Secret: pw, Secret2: pw2 + "\x07"}, "encryption.secret2 contains control"},
	} {
		if _, err := planEncryption(c.engine, engines.S3, &c.in, false); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
	for _, attach := range []bool{false, true} {
		p, err := planEncryption(EngineRclone, engines.S3, &EncryptionInput{Generate: &no, Secret: pw, Secret2: pw2}, attach)
		if err != nil || p.mode != engines.EncryptionCrypt || p.origin != OriginUser ||
			p.secret != (engines.EncryptionSecret{CryptPassword: pw, CryptPassword2: pw2}) {
			t.Errorf("attach %t: plan %+v, %v", attach, p.secret.Fields(), err)
		}
	}
	// Without secret2, password2 stays rclone's default.
	if p, err := planEncryption(EngineRclone, engines.S3, &EncryptionInput{Secret: pw}, true); err != nil || p.secret.CryptPassword2 != "" {
		t.Errorf("secret only: %v %v", p.secret.Fields(), err)
	}
	e := EncryptionInput{Secret: pw, Secret2: pw2}
	for _, s := range []string{fmt.Sprint(e), fmt.Sprintf("%#v", e), string(must(json.Marshal(e)))} {
		if strings.Contains(s, "password") {
			t.Errorf("EncryptionInput shows a secret: %s", s)
		}
	}
	if (&EncryptionInput{Secret2: pw2}).keeps(engines.EncryptionCrypt) {
		t.Error("an update with secret2 keeps the encryption")
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// A crypt remote created with a generated password2 is attached again from its recovery kit
// alone (§5.3): the kit's two crypt passwords go to encryption.secret and encryption.secret2.
func TestRcloneCryptReattachFromKit(t *testing.T) {
	ef := newEngineFixture(t)
	type remoteState struct {
		marker string
		secret engines.EncryptionSecret
	}
	remotes := map[string]*remoteState{}
	ef.rclone.OnCreate = func(_ context.Context, d engines.Destination, s engines.Secrets, attach bool) (engines.CreateResult, error) {
		r := remotes[d.Target]
		if !attach {
			if r != nil {
				return engines.CreateResult{}, errors.New("a Bunkarr destination already exists at this location: attach it instead")
			}
			remotes[d.Target] = &remoteState{marker: fmt.Sprintf("marker-%d", len(remotes)+1), secret: s.Encryption}
			return engines.CreateResult{MarkerID: remotes[d.Target].marker}, nil
		}
		switch {
		case r == nil:
			return engines.CreateResult{}, engines.ErrMarkerMissing
		case s.Encryption != r.secret:
			// crypt with another password or salt: strict_names fails on the marker.
			return engines.CreateResult{}, fmt.Errorf("unreadable: %w", rclone.ErrUndecryptable)
		}
		return engines.CreateResult{MarkerID: r.marker}, nil
	}
	remote := rawJSON(t, map[string]any{"provider": "Minio", "endpoint": "https://s3.example.com", "region": "us-east-1", "bucket": "crypt-kit"})
	in := Input{Name: "Crypt", Kind: engines.S3, Engine: EngineRclone, Remote: remote,
		Credentials: creds(t, fmt.Sprintf(`{"accessKeyId":%q,"secretAccessKey":%q}`, testAccessKey, testSecretKey))}
	d, err := ef.store.Create(ef.ctx, in, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Encryption.Mode != engines.EncryptionCrypt || d.Encryption.Origin != OriginGenerated {
		t.Fatalf("encryption %+v", d.Encryption)
	}
	kit, err := ef.store.RecoveryKit(ef.ctx, d.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	k := kitJSONBlock(t, kit.Content)
	if k.Encryption.CryptPassword == "" || k.Encryption.CryptPassword2 == "" {
		t.Fatalf("the kit lacks a crypt password: %+v", k.Encryption.CryptObscured)
	}
	// /config is lost: the row goes, the remote stays.
	if err := ef.store.Delete(ef.ctx, d.ID, DeleteOptions{ConfirmLoseSecret: true}); err != nil {
		t.Fatal(err)
	}
	attach := in
	attach.Name = "Crypt again"
	attach.Encryption = &EncryptionInput{Secret: k.Encryption.CryptPassword}
	if _, err := ef.store.Create(ef.ctx, attach, CreateOptions{Attach: true}); err == nil || !strings.Contains(err.Error(), "unreadable") ||
		!strings.Contains(err.Error(), `the recovery kit's "rclone crypt password2" as encryption.secret2`) {
		t.Fatalf("attach with password only: %v (password2 is the kit's, not rclone's default, and the error says so)", err)
	}
	attach.Encryption = &EncryptionInput{Secret: k.Encryption.CryptPassword, Secret2: "not the kit's crypt password2"}
	if _, err := ef.store.Create(ef.ctx, attach, CreateOptions{Attach: true}); err == nil || !strings.Contains(err.Error(), "unreadable") ||
		strings.Contains(err.Error(), "encryption.secret2") {
		t.Fatalf("attach with a wrong password2: %v", err)
	}
	attach.Encryption = &EncryptionInput{Secret: k.Encryption.CryptPassword, Secret2: k.Encryption.CryptPassword2}
	back, err := ef.store.Create(ef.ctx, attach, CreateOptions{Attach: true})
	if err != nil {
		t.Fatalf("attach with the kit's two passwords: %v", err)
	}
	if back.MarkerID != d.MarkerID || back.Encryption.Origin != OriginUser || Blocked(back) != "" {
		t.Errorf("attached %+v (blocked %q)", back, Blocked(back))
	}
	_, sec, err := ef.store.SecretsFor(ef.ctx, back.ID)
	if err != nil || sec.Encryption.CryptPassword2 != k.Encryption.CryptPassword2 || sec.Obscured[engines.FieldCryptPassword2] == "" {
		t.Errorf("stored password2: %v", err)
	}
	// TestRemote takes secret2 too (test before attach).
	ef.rclone.OnTest = func(_ context.Context, _ engines.Destination, s engines.Secrets) (engines.TestResult, error) {
		if s.Encryption.CryptPassword2 != k.Encryption.CryptPassword2 {
			return engines.TestResult{Message: "unreadable"}, nil
		}
		return engines.TestResult{OK: true, Reachable: true, Marker: engines.MarkerOK, ID: d.MarkerID}, nil
	}
	res, err := ef.store.TestRemote(ef.ctx, TestInput{Kind: engines.S3, Engine: EngineRclone, Remote: remote, Credentials: in.Credentials,
		Encryption: &EncryptionInput{Secret: k.Encryption.CryptPassword, Secret2: k.Encryption.CryptPassword2}})
	if err != nil || !res.OK {
		t.Errorf("TestRemote with secret2: %+v %v", res, err)
	}
}

// Two crypt passwords typed at create are confirmed with the kit's check code, not by
// re-entering the first one only.
func TestConfirmKitBySecretWithPassword2(t *testing.T) {
	ef := newEngineFixture(t)
	in := Input{Name: "Own salt", Kind: engines.S3, Engine: EngineRclone,
		Remote:      rawJSON(t, map[string]any{"provider": "Minio", "endpoint": "https://s3.example.com", "region": "us-east-1", "bucket": "own-salt"}),
		Credentials: creds(t, fmt.Sprintf(`{"accessKeyId":%q,"secretAccessKey":%q}`, testAccessKey, testSecretKey)),
		Encryption:  &EncryptionInput{Secret: "my own crypt password", Secret2: "my own crypt password2"}}
	d, err := ef.store.Create(ef.ctx, in, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Encryption.Origin != OriginUser || Blocked(d) != BlockedKit {
		t.Fatalf("created %+v", d.Encryption)
	}
	if err := ef.store.ConfirmKit(ef.ctx, d.ID, KitConfirmation{Secret: "my own crypt password"}); err == nil || !strings.Contains(err.Error(), "check code") {
		t.Fatalf("confirm by the first password: %v", err)
	}
	kit, err := ef.store.RecoveryKit(ef.ctx, d.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := ef.store.ConfirmKit(ef.ctx, d.ID, KitConfirmation{CheckCode: kitCheckCode(t, kit.Content)}); err != nil {
		t.Fatal(err)
	}
}
