package destinations

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/rclone"
)

// b2Handler answers b2_authorize_account with body and records the Authorization headers.
func b2Handler(auth *[]string, body string) http.Handler {
	var mu sync.Mutex
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*auth = append(*auth, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.URL.RawQuery != "" {
			http.Error(w, "no query expected", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
}

// newB2Server is an HTTPS b2_authorize_account stand-in.
func newB2Server(t *testing.T, body string) *httptest.Server {
	t.Helper()
	var auth []string
	srv := httptest.NewTLSServer(b2Handler(&auth, body))
	t.Cleanup(srv.Close)
	return srv
}

var checkCodeLine = regexp.MustCompile(`(?m)^CHECK CODE: ([A-Z2-7]{4}-[A-Z2-7]{4})$`)

// kitCheckCode returns the check code printed in a kit.
func kitCheckCode(t *testing.T, content string) string {
	t.Helper()
	m := checkCodeLine.FindStringSubmatch(content)
	if m == nil {
		t.Fatalf("no check code in the kit:\n%s", content)
	}
	return m[1]
}

// between returns the text between the lines begin and end.
func between(t *testing.T, content, begin, end string) string {
	t.Helper()
	i := strings.Index(content, begin+"\n")
	j := strings.Index(content, "\n"+end)
	if i < 0 || j < i {
		t.Fatalf("no %q block in the kit:\n%s", begin, content)
	}
	return content[i+len(begin)+1 : j+1]
}

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func runSh(t *testing.T, dir, script string, args ...string) string {
	t.Helper()
	cmd := exec.Command("sh", append(args, "-c", script)...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sh: %v\n%s\nscript:\n%s", err, out, script)
	}
	return string(out)
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{"": "''", "My Backups": "'My Backups'", "a'b": `'a'\''b'`, "$(x)": "'$(x)'"} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestCheckCode(t *testing.T) {
	c := CheckCode(3, "secret")
	if !regexp.MustCompile(`^[A-Z2-7]{4}-[A-Z2-7]{4}$`).MatchString(c) || c == CheckCode(4, "secret") || c == CheckCode(3, "secreT") {
		t.Errorf("check code %q", c)
	}
	if normalizeCode(" "+strings.ToLower(c)) != strings.ReplaceAll(c, "-", "") {
		t.Errorf("normalize %q", normalizeCode(strings.ToLower(c)))
	}
	if got := kitFilename(`My "UNAS" / Off-site Backups!!`, 3, mustTime("2026-09-27T23:30:00Z")); got != "bunkarr-recovery-my-unas-off-site-backups-20260927.txt" {
		t.Errorf("filename %q", got)
	}
	if got := kitFilename("日本", 7, mustTime("2026-01-02T00:00:00Z")); got != "bunkarr-recovery-destination-7-20260102.txt" {
		t.Errorf("filename %q", got)
	}
	if got := kitFilename(strings.Repeat("x", 60), 1, mustTime("2026-01-02T00:00:00Z")); len(got) != len("bunkarr-recovery--20260102.txt")+40 {
		t.Errorf("filename %q", got)
	}
}

func TestRecoveryKitRestic(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	for _, prefix := range []string{"a$(touch pwned)", "it's mine", "My Backups/x"} {
		t.Run(prefix, func(t *testing.T) {
			ef := newEngineFixture(t)
			src := ef.addSource(t, "Movies")
			in := s3Input(t, "Kit "+prefix, "kit-bucket", prefix)
			in.SourceIDs = []int64{src}
			d, err := ef.store.Create(ef.ctx, in, CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			_, sec, err := ef.store.SecretsFor(ef.ctx, d.ID)
			if err != nil {
				t.Fatal(err)
			}
			kit, err := ef.store.RecoveryKit(ef.ctx, d.ID, false)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(kit.Filename, "bunkarr-recovery-kit-") || !strings.HasSuffix(kit.Filename, "-20260924.txt") {
				t.Errorf("filename %q", kit.Filename)
			}
			c := kit.Content
			for _, want := range []string{"Destination:     Kit " + prefix, "restic on s3", sec.Encryption.ResticPassword, "bunkarr-dest:" + d.EngineTag,
				"create a new application key for bucket kit-bucket", "restic snapshots --tag", "restic restore", "restic dump", "WARNING: whoever holds",
				kitJSONBegin, "/media/Movies"} {
				if !strings.Contains(strings.ToLower(c), strings.ToLower(want)) {
					t.Errorf("kit lacks %q", want)
				}
			}
			for _, secret := range []string{testSecretKey, testAccessKey} {
				if strings.Contains(c, secret) {
					t.Errorf("kit without storage credentials contains %q", secret)
				}
			}
			shell := between(t, c, kitShellBegin, kitShellEnd)
			runSh(t, t.TempDir(), shell, "-n")
			dir := t.TempDir()
			out := runSh(t, dir, shell+`printf '%s\n%s' "$RESTIC_REPOSITORY" "$(cat "$RESTIC_PASSWORD_FILE")"`)
			if want := "rclone:BKDEST:kit-bucket/" + prefix + "\n" + sec.Encryption.ResticPassword; out != want {
				t.Errorf("round trip = %q, want %q", out, want)
			}
			if _, err := os.Stat(filepath.Join(dir, "pwned")); err == nil {
				t.Error("the prefix ran a command")
			}
			if fi, err := os.Stat(filepath.Join(dir, "bunkarr_restic_password")); err != nil || fi.Mode().Perm() != 0o600 {
				t.Errorf("password file: %v %v", fi, err)
			}
			if !strings.Contains(shell, "RCLONE_CONFIG_BKDEST_ACCESS_KEY_ID='REPLACE_WITH_ACCESS_KEY_ID'") {
				t.Errorf("placeholder missing:\n%s", shell)
			}
			// The JSON block parses and carries the same data.
			var j map[string]any
			if err := json.Unmarshal([]byte(between(t, c, kitJSONBegin, kitJSONEnd)), &j); err != nil {
				t.Fatal(err)
			}
			if j["checkCode"] != kitCheckCode(t, c) || j["engineTag"] != d.EngineTag || j["storageCredentials"] != nil {
				t.Errorf("json %v", j)
			}
			got, _ := ef.store.Get(ef.ctx, d.ID)
			if got.Encryption.KitExportedAt == nil || Blocked(got) != BlockedKit {
				t.Errorf("after export: %+v", got.Encryption)
			}
			// Wrong code, then the right one (case and dashes ignored).
			if err := ef.store.ConfirmKit(ef.ctx, d.ID, KitConfirmation{CheckCode: "AAAA-AAAA"}); !errors.Is(err, ErrWrongCheckCode) {
				t.Errorf("wrong code: %v", err)
			}
			if err := ef.store.ConfirmKit(ef.ctx, d.ID, KitConfirmation{Secret: sec.Encryption.ResticPassword}); err == nil {
				t.Error("a generated secret was confirmed by typing it")
			}
			code := strings.ToLower(strings.ReplaceAll(kitCheckCode(t, c), "-", " "))
			if err := ef.store.ConfirmKit(ef.ctx, d.ID, KitConfirmation{CheckCode: code}); err != nil {
				t.Fatalf("confirm: %v", err)
			}
			if got, _ := ef.store.Get(ef.ctx, d.ID); got.Encryption.KitConfirmedAt == nil || Blocked(got) != "" {
				t.Errorf("after confirm: %+v", got.Encryption)
			}
			// With the storage credentials.
			full, err := ef.store.RecoveryKit(ef.ctx, d.ID, true)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(full.Content, testSecretKey) || !strings.Contains(full.Content, "export RCLONE_CONFIG_BKDEST_SECRET_ACCESS_KEY="+shellQuote(testSecretKey)) {
				t.Error("kit with storage credentials lacks them")
			}
		})
	}
}

func TestRecoveryKitRcloneSFTP(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	ef := newEngineFixture(t)
	hk, _ := hostKey(t)
	d, err := ef.store.Create(ef.ctx, Input{Name: "SFTP crypt", Kind: engines.SFTP, Engine: EngineRclone,
		Remote:      rawJSON(t, map[string]any{"host": "sftp.example.com", "port": 2222, "user": "u", "path": "it's/a$(touch pwned)", "hostKeys": []engines.HostKey{hk}}),
		Credentials: creds(t, `{"password":"correct horse battery"}`)}, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, sec, err := ef.store.SecretsFor(ef.ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	kit, err := ef.store.RecoveryKit(ef.ctx, d.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	c := kit.Content
	for _, want := range []string{sec.Encryption.CryptPassword, sec.Encryption.CryptPassword2, sec.Obscured[engines.FieldCryptPassword],
		"rclone lsd 'bunkarr-crypt:'", "rclone copy", "links.tsv", "[sftp.example.com]:2222 ssh-ed25519 ", kitCheckCode(t, c)} {
		if !strings.Contains(c, want) {
			t.Errorf("kit lacks %q", want)
		}
	}
	if strings.Contains(c, "correct horse battery") || strings.Contains(c, sec.Obscured[engines.FieldPassword]) {
		t.Error("kit without storage credentials contains the SFTP password")
	}
	shell := between(t, c, kitShellBegin, kitShellEnd)
	runSh(t, t.TempDir(), shell, "-n")
	dir := t.TempDir()
	runSh(t, dir, shell)
	if _, err := os.Stat(filepath.Join(dir, "pwned")); err == nil {
		t.Error("the path ran a command")
	}
	kh, err := os.ReadFile(filepath.Join(dir, "bunkarr_known_hosts"))
	want, _ := rclone.KnownHosts("sftp.example.com", 2222, []engines.HostKey{hk})
	if err != nil || string(kh) != want {
		t.Errorf("known_hosts %q, want %q (%v)", kh, want, err)
	}
	conf, err := os.ReadFile(filepath.Join(dir, "rclone.conf"))
	if err != nil {
		t.Fatal(err)
	}
	sections := parseINI(t, string(conf))
	if len(sections) != 2 {
		t.Fatalf("rclone.conf sections %v", sections)
	}
	destKeys := slices.Sorted(mapKeys(sections[rclone.ConfDest]))
	if !slices.Equal(destKeys, []string{"ask_password", "host", "key_use_agent", "known_hosts_file", "pass", "port", "type", "user"}) {
		t.Errorf("[%s] keys %v", rclone.ConfDest, destKeys)
	}
	cryptKeys := slices.Sorted(mapKeys(sections[rclone.ConfCrypt]))
	if !slices.Equal(cryptKeys, []string{"directory_name_encryption", "filename_encryption", "password", "password2", "remote", "strict_names", "type"}) {
		t.Errorf("[%s] keys %v", rclone.ConfCrypt, cryptKeys)
	}
	if sections[rclone.ConfCrypt]["remote"] != "bunkarr-dest:it's/a$(touch pwned)" || sections[rclone.ConfDest]["known_hosts_file"] != "./bunkarr_known_hosts" ||
		sections[rclone.ConfDest]["pass"] != "REPLACE_WITH_OBSCURED_PASSWORD" || sections[rclone.ConfCrypt]["password"] != sec.Obscured[engines.FieldCryptPassword] {
		t.Errorf("rclone.conf values %v", sections)
	}
	if r, err := rclone.Reveal(sections[rclone.ConfCrypt]["password2"]); err != nil || r != sec.Encryption.CryptPassword2 {
		t.Errorf("password2 does not reveal to the secret (%v)", err)
	}
}

func TestConfirmKitByUserSecret(t *testing.T) {
	ef := newEngineFixture(t)
	in := s3Input(t, "User secret", "user-secret", "")
	in.Encryption = &EncryptionInput{Secret: "my own long passphrase"}
	d, err := ef.store.Create(ef.ctx, in, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Encryption.Origin != OriginUser || Blocked(d) != BlockedKit {
		t.Fatalf("user secret at create: %+v", d.Encryption)
	}
	if err := ef.store.ConfirmKit(ef.ctx, d.ID, KitConfirmation{Secret: "my own long passphrasE"}); !errors.Is(err, ErrWrongSecret) {
		t.Errorf("wrong secret: %v", err)
	}
	if err := ef.store.ConfirmKit(ef.ctx, d.ID, KitConfirmation{}); err == nil {
		t.Error("empty confirmation accepted")
	}
	if err := ef.store.ConfirmKit(ef.ctx, d.ID, KitConfirmation{Secret: "my own long passphrase"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := ef.store.Get(ef.ctx, d.ID); Blocked(got) != "" {
		t.Errorf("still blocked: %q", Blocked(got))
	}
	if s := fmt.Sprint(KitConfirmation{Secret: "my own long passphrase"}); strings.Contains(s, "passphrase") {
		t.Errorf("KitConfirmation prints its secret: %s", s)
	}
	if b, _ := json.Marshal(EncryptionInput{Secret: "my own long passphrase"}); strings.Contains(string(b), "passphrase") {
		t.Errorf("EncryptionInput marshals its secret: %s", b)
	}
}

// parseINI parses rclone.conf: sections of key = value lines.
func parseINI(t *testing.T, s string) map[string]map[string]string {
	t.Helper()
	out := map[string]map[string]string{}
	var cur map[string]string
	for _, l := range strings.Split(s, "\n") {
		switch {
		case l == "":
		case strings.HasPrefix(l, "[") && strings.HasSuffix(l, "]"):
			cur = map[string]string{}
			out[l[1:len(l)-1]] = cur
		default:
			k, v, ok := strings.Cut(l, " = ")
			if !ok || cur == nil {
				t.Fatalf("rclone.conf line %q", l)
			}
			cur[k] = v
		}
	}
	return out
}

func mapKeys(m map[string]string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}
