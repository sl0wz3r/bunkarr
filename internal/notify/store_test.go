package notify

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/logging"
)

func storedSecret(t *testing.T, d *db.DB, id int64) string {
	t.Helper()
	var s string
	if err := d.Reader().QueryRowContext(context.Background(), `SELECT secret FROM notifications WHERE id = ?`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStoreCreateGetListUpdateDelete(t *testing.T) {
	ctx := context.Background()
	s, d, kr := openStore(t)
	const urls = "discord://1234567/store-crud-token-a1b2c3, json://apprise.local/hook"

	n, err := s.Create(ctx, Input{Name: " Discord ", APIURL: "http://apprise:8000/", URLs: urls})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	created := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	want := Notification{ID: n.ID, Name: "Discord", Kind: KindApprise, Enabled: true, APIURL: "http://apprise:8000",
		HasURLs: true, OnFailure: true, OnWarning: true, CreatedAt: created, UpdatedAt: created}
	if n.ID == 0 || !reflect.DeepEqual(n, want) {
		t.Fatalf("Create = %+v\nwant     %+v", n, want)
	}

	// Sealed at rest, bound to this row.
	sealed := storedSecret(t, d, n.ID)
	if sealed == "" || strings.Contains(sealed, "store-crud-token") {
		t.Fatalf("stored secret is not sealed: %q", sealed)
	}
	if got, err := kr.Open(sealed, "notification:"+itoa(n.ID)+":urls"); err != nil || got != urls {
		t.Fatalf("open with the row's AAD = %q, %v", got, err)
	}
	if _, err := kr.Open(sealed, "notification:"+itoa(n.ID+1)+":urls"); err == nil {
		t.Fatal("sealed URLs opened with another row's AAD")
	}
	if !logging.ContainsSecret(urls) || !logging.ContainsSecret("discord://1234567/store-crud-token-a1b2c3") {
		t.Fatal("Create did not register the URLs as secrets")
	}

	// Write-only: never in the API shape.
	js, _ := json.Marshal(n)
	if strings.Contains(string(js), "store-crud-token") || !strings.Contains(string(js), `"hasUrls":true`) {
		t.Fatalf("JSON = %s", js)
	}

	got, err := s.Get(ctx, n.ID)
	if err != nil || !reflect.DeepEqual(got, n) {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	list, err := s.List(ctx)
	if err != nil || len(list) != 1 || !reflect.DeepEqual(list[0], n) {
		t.Fatalf("List = %+v, %v", list, err)
	}

	// Update: empty URLs and nil flags keep the stored values (the stored URLs only with the
	// stored API URL: TestAPIURLChangeNeedsTheURLsAgain).
	later := created.Add(time.Hour)
	s.now = func() time.Time { return later }
	u, err := s.Update(ctx, n.ID, Input{Name: "Discord alerts", APIURL: "http://apprise:8000", OnSuccess: new(true)})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	want = Notification{ID: n.ID, Name: "Discord alerts", Kind: KindApprise, Enabled: true, APIURL: "http://apprise:8000",
		HasURLs: true, OnFailure: true, OnWarning: true, OnSuccess: true, CreatedAt: created, UpdatedAt: later}
	if !reflect.DeepEqual(u, want) {
		t.Fatalf("Update = %+v\nwant     %+v", u, want)
	}
	if storedSecret(t, d, n.ID) != sealed {
		t.Fatal("Update with empty urls changed the stored URLs")
	}

	// New URLs replace the stored ones; flags can be switched off.
	const urls2 = "tgram://987654:store-crud-bot-token/42"
	if _, err := s.Update(ctx, n.ID, Input{Name: "Discord alerts", APIURL: "https://apprise.example/api", URLs: urls2, OnWarning: new(false), Enabled: new(false)}); err != nil {
		t.Fatalf("Update URLs: %v", err)
	}
	if got, err := kr.Open(storedSecret(t, d, n.ID), "notification:"+itoa(n.ID)+":urls"); err != nil || got != urls2 {
		t.Fatalf("URLs after update = %q, %v", got, err)
	}
	if g, _ := s.Get(ctx, n.ID); g.Enabled || g.OnWarning || !g.OnFailure {
		t.Fatalf("flags after update = %+v", g)
	}

	// Stateful mode drops the unused URLs; going back to stateless then needs URLs again.
	sf, err := s.Update(ctx, n.ID, Input{Name: "Discord alerts", APIURL: "https://apprise.example/api", ConfigKey: "bunkarr"})
	if err != nil || sf.HasURLs || sf.ConfigKey != "bunkarr" || storedSecret(t, d, n.ID) != "" {
		t.Fatalf("switch to stateful = %+v, %v", sf, err)
	}
	var verr ValidationError
	if _, err := s.Update(ctx, n.ID, Input{Name: "Discord alerts", APIURL: "https://apprise.example/api"}); !errors.As(err, &verr) {
		t.Fatalf("stateless without URLs: err = %v, want ValidationError", err)
	}

	// A stateful target needs no URLs at all.
	st, err := s.Create(ctx, Input{Name: "apprise-config", APIURL: "http://apprise:8000", ConfigKey: "family_alerts-1"})
	if err != nil || st.HasURLs || st.ConfigKey != "family_alerts-1" {
		t.Fatalf("Create stateful = %+v, %v", st, err)
	}
	if list, _ := s.List(ctx); len(list) != 2 || list[0].Name != "apprise-config" || list[1].Name != "Discord alerts" {
		t.Fatalf("List order = %+v", list)
	}

	if err := s.Delete(ctx, n.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	for name, err := range map[string]error{
		"Get":    func() error { _, err := s.Get(ctx, n.ID); return err }(),
		"Delete": s.Delete(ctx, n.ID),
		"Update": func() error {
			_, err := s.Update(ctx, n.ID, Input{Name: "x", APIURL: "http://a", ConfigKey: "k"})
			return err
		}(),
	} {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s after delete: err = %v, want ErrNotFound", name, err)
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// TestAPIURLChangeNeedsTheURLsAgain: the stored URLs are bound to the stored API URL, so an update
// that points a stateless notification at another Apprise API without its URLs is refused and
// changes nothing (S8: the URLs must never be posted to a host picked by a caller who does not
// know them).
func TestAPIURLChangeNeedsTheURLsAgain(t *testing.T) {
	ctx := context.Background()
	s, d, _ := openStore(t)
	const urls = "discord://1234567/bound-urls-token-d4e5"
	n, err := s.Create(ctx, Input{Name: "n", APIURL: "http://apprise:8000", URLs: urls})
	if err != nil {
		t.Fatal(err)
	}
	sealed := storedSecret(t, d, n.ID)
	_, err = s.Update(ctx, n.ID, Input{Name: "n", APIURL: "http://attacker:8000"})
	var verr ValidationError
	if !errors.As(err, &verr) || !strings.Contains(err.Error(), "enter them again") || strings.Contains(err.Error(), "attacker") {
		t.Fatalf("Update to another API URL without URLs = %v, want a ValidationError asking for the URLs", err)
	}
	if g, _ := s.Get(ctx, n.ID); g.APIURL != "http://apprise:8000" || storedSecret(t, d, n.ID) != sealed {
		t.Fatalf("a refused update changed the notification: %+v", g)
	}
	// The same API URL in another spelling keeps them; new URLs or stateful mode allow a new one.
	for _, in := range []Input{
		{Name: "n", APIURL: " http://apprise:8000/ "},
		{Name: "n", APIURL: "http://apprise2:8000", URLs: urls},
		{Name: "n", APIURL: "http://apprise3:8000", ConfigKey: "k"},
	} {
		if _, err := s.Update(ctx, n.ID, in); err != nil {
			t.Fatalf("Update %+v: %v", in, err)
		}
	}
}

// TestUnsavedURLsAreNotRegistered: only stored URLs enter the process-wide secret registry; a
// create that fails registers nothing.
func TestUnsavedURLsAreNotRegistered(t *testing.T) {
	ctx := context.Background()
	s, _, _ := openStore(t)
	if _, err := s.Create(ctx, Input{Name: "taken", APIURL: "http://apprise:8000", ConfigKey: "k"}); err != nil {
		t.Fatal(err)
	}
	const urls = "json://refused-create.example/refused-create-token-77"
	if _, err := s.Create(ctx, Input{Name: "TAKEN", APIURL: "http://apprise:8000", URLs: urls}); err == nil {
		t.Fatal("duplicate name accepted")
	}
	if logging.ContainsSecret(urls) {
		t.Fatal("the URLs of a refused create were registered")
	}
}

func TestStoreValidation(t *testing.T) {
	const secretish = "hunter2-validation-pw"
	valid := func(mod func(*Input)) Input {
		in := Input{Name: "Target", APIURL: "http://apprise:8000", URLs: "json://example.com/hook"}
		mod(&in)
		return in
	}
	cases := []struct {
		name string
		in   Input
		want string // "" = accepted
	}{
		{"empty name", valid(func(in *Input) { in.Name = "" }), "name is required"},
		{"blank name", valid(func(in *Input) { in.Name = "   " }), "name is required"},
		{"name too long", valid(func(in *Input) { in.Name = strings.Repeat("a", 65) }), "at most 64 characters"},
		{"name 64 multibyte chars", valid(func(in *Input) { in.Name = strings.Repeat("é", 64) }), ""},
		{"name control char", valid(func(in *Input) { in.Name = "bad\x07name" }), "control characters"},
		{"duplicate name, other case", valid(func(in *Input) { in.Name = "EXISTING" }), "already exists"},
		{"unknown kind", valid(func(in *Input) { in.Kind = "slack" }), `kind must be "apprise"`},
		{"explicit kind", valid(func(in *Input) { in.Kind = KindApprise }), ""},
		{"no apiUrl", valid(func(in *Input) { in.APIURL = "" }), "apiUrl is required"},
		{"apiUrl without scheme", valid(func(in *Input) { in.APIURL = "apprise:8000" }), "http:// or https://"},
		{"apiUrl ftp", valid(func(in *Input) { in.APIURL = "ftp://apprise" }), "http:// or https://"},
		{"apiUrl relative", valid(func(in *Input) { in.APIURL = "/notify" }), "http:// or https://"},
		{"apiUrl no host", valid(func(in *Input) { in.APIURL = "http://" }), "with a host"},
		{"apiUrl port only", valid(func(in *Input) { in.APIURL = "http://:8000" }), "with a host"},
		{"apiUrl userinfo", valid(func(in *Input) { in.APIURL = "http://admin:" + secretish + "@apprise:8000" }), "user name or password"},
		{"apiUrl query", valid(func(in *Input) { in.APIURL = "http://apprise:8000/?token=" + secretish }), "query or fragment"},
		{"apiUrl empty query", valid(func(in *Input) { in.APIURL = "http://apprise:8000/?" }), "query or fragment"},
		{"apiUrl fragment", valid(func(in *Input) { in.APIURL = "http://apprise:8000/#x" }), "query or fragment"},
		{"apiUrl too long", valid(func(in *Input) { in.APIURL = "http://a/" + strings.Repeat("p", MaxAPIURLLen) }), "at most 2048 bytes"},
		{"apiUrl https with path", valid(func(in *Input) { in.APIURL = "https://example.com/apprise/" }), ""},
		{"stateless without urls", valid(func(in *Input) { in.URLs = "  " }), "urls are required"},
		{"urls without scheme", valid(func(in *Input) { in.URLs = "not a url " + secretish }), "at least one Apprise URL"},
		{"urls with leading junk", valid(func(in *Input) { in.URLs = secretish + " json://x/y" }), "separated by commas or spaces"},
		{"urls control char", valid(func(in *Input) { in.URLs = "json://x/" + secretish + "\x00" }), "control characters"},
		{"urls too long", valid(func(in *Input) { in.URLs = "json://x/" + strings.Repeat("a", MaxURLsLen) }), "at most 16384 bytes"},
		{"urls on lines", valid(func(in *Input) { in.URLs = "json://x/y\r\nmailto://u:p@h?to=a@b.c,d@e.f\n" }), ""},
		{"stateful", valid(func(in *Input) { in.URLs, in.ConfigKey = "", strings.Repeat("k", 64) }), ""},
		{"config key too long", valid(func(in *Input) { in.URLs, in.ConfigKey = "", strings.Repeat("k", 65) }), "configKey must be"},
		{"config key bad chars", valid(func(in *Input) { in.URLs, in.ConfigKey = "", "my key!" }), "configKey must be"},
		{"config key path", valid(func(in *Input) { in.URLs, in.ConfigKey = "", "../get/abc" }), "configKey must be"},
		{"urls and config key", valid(func(in *Input) { in.ConfigKey = "abc" }), "not both"},
	}
	ctx := context.Background()
	s, _, _ := openStore(t)
	if _, err := s.Create(ctx, Input{Name: "Existing", APIURL: "http://apprise:8000", ConfigKey: "existing"}); err != nil {
		t.Fatal(err)
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			if tc.want == "" {
				in.Name = strings.Replace(in.Name, "Target", "Target"+itoa(int64(i)), 1)
			}
			n, err := s.Create(ctx, in)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Create: %v", err)
				}
				if strings.HasSuffix(n.APIURL, "/") {
					t.Fatalf("apiUrl not normalized: %q", n.APIURL)
				}
				return
			}
			var verr ValidationError
			if !errors.As(err, &verr) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want ValidationError containing %q", err, tc.want)
			}
			if strings.Contains(err.Error(), secretish) {
				t.Fatalf("validation error echoes the input: %v", err)
			}
		})
	}
}

func TestStoreUpdateRejectsDuplicateNameAndKeepsRow(t *testing.T) {
	ctx := context.Background()
	s, _, _ := openStore(t)
	a, err := s.Create(ctx, Input{Name: "A", APIURL: "http://apprise:8000", ConfigKey: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, Input{Name: "B", APIURL: "http://apprise:8000", ConfigKey: "b"}); err != nil {
		t.Fatal(err)
	}
	var verr ValidationError
	if _, err := s.Update(ctx, a.ID, Input{Name: "b", APIURL: "http://apprise:8000", ConfigKey: "a"}); !errors.As(err, &verr) {
		t.Fatalf("rename onto another name: err = %v", err)
	}
	// Renaming to itself in another case is fine.
	if got, err := s.Update(ctx, a.ID, Input{Name: "a", APIURL: "http://apprise:8000", ConfigKey: "a"}); err != nil || got.Name != "a" {
		t.Fatalf("case-only rename = %+v, %v", got, err)
	}
}

func TestSplitURLs(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"json://a/b", []string{"json://a/b"}},
		{"json://a/b, discord://c/d", []string{"json://a/b", "discord://c/d"}},
		{"json://a/b discord://c/d", []string{"json://a/b", "discord://c/d"}},
		{" ,json://a/b,,\n\tdiscord://c/d , ", []string{"json://a/b", "discord://c/d"}},
		// A comma not followed by a scheme belongs to the URL (Apprise does the same).
		{"mailto://u:p@h?to=a@b.c,d@e.f json://x", []string{"mailto://u:p@h?to=a@b.c,d@e.f", "json://x"}},
		{"junk json://x", []string{"json://x"}},
		{"no urls here", []string{}},
	}
	for _, tc := range cases {
		if got := splitURLs(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("splitURLs(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRegisterSecretsRegistersEveryStoredURL(t *testing.T) {
	ctx := context.Background()
	s, d, kr := openStore(t)
	const (
		good  = "discord://55555/register-secrets-token-q9, pover://register-user-key@register-app-token"
		other = "json://register-secrets-wrong-key.example/hook"
	)
	now := db.FormatTime(time.Now())
	insert := func(id int64, name, sealed string) {
		t.Helper()
		if err := d.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO notifications (id, name, kind, settings, secret, created_at, updated_at)
				VALUES (?, ?, 'apprise', '{"apiUrl":"http://apprise:8000"}', ?, ?, ?)`, id, name, sealed, now, now)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	sealedGood, err := kr.Seal(good, "notification:1:urls")
	if err != nil {
		t.Fatal(err)
	}
	sealedOther, err := testKeyring(t, 99).Seal(other, "notification:2:urls")
	if err != nil {
		t.Fatal(err)
	}
	insert(1, "good", sealedGood)
	insert(2, "bad key", sealedOther)

	err = s.RegisterSecrets(ctx)
	if err == nil || !strings.Contains(err.Error(), `"bad key"`) || strings.Contains(err.Error(), "register-secrets-wrong-key") {
		t.Fatalf("RegisterSecrets err = %v, want a redacted error naming the undecryptable row", err)
	}
	for _, v := range []string{good, "discord://55555/register-secrets-token-q9", "pover://register-user-key@register-app-token"} {
		if !logging.ContainsSecret(v) {
			t.Errorf("%q not registered", v)
		}
	}
	msg := logging.RedactSecrets("sending to discord://55555/register-secrets-token-q9 failed")
	if strings.Contains(msg, "register-secrets-token") {
		t.Fatalf("not redacted: %s", msg)
	}
}

// releaseMany makes the redaction registry release many values, more than it keeps once released.
func releaseMany(t *testing.T) {
	t.Helper()
	for i := range 5000 {
		logging.SetSecrets("test:churn", fmt.Sprintf("churned-secret-%06d", i))
	}
	logging.SetSecrets("test:churn")
}

// TestReplacedURLsAreReleased: the redaction registry holds each notification's stored URLs,
// never dropped, and releases URLs that were replaced, dropped for stateful mode or deleted, so
// saving new URL lists cannot grow it without bound.
func TestReplacedURLsAreReleased(t *testing.T) {
	ctx := context.Background()
	s, _, _ := openStore(t)
	const (
		first  = "json://released-first.example/first-url-token-111"
		second = "json://released-second.example/second-url-token-222"
		third  = "json://released-third.example/third-url-token-333"
	)
	n, err := s.Create(ctx, Input{Name: "a", APIURL: "http://apprise:8000", URLs: first})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(ctx, n.ID, Input{Name: "a", APIURL: "http://apprise:8000", URLs: second}); err != nil {
		t.Fatal(err)
	}
	other, err := s.Create(ctx, Input{Name: "b", APIURL: "http://apprise:8000", URLs: third})
	if err != nil {
		t.Fatal(err)
	}
	releaseMany(t)
	if logging.ContainsSecret(first) || !logging.ContainsSecret(second) || !logging.ContainsSecret(third) {
		t.Fatalf("after a replace: first %v, second %v, third %v; want only the stored URLs redacted",
			logging.ContainsSecret(first), logging.ContainsSecret(second), logging.ContainsSecret(third))
	}
	if _, err := s.Update(ctx, n.ID, Input{Name: "a", APIURL: "http://apprise:8000", ConfigKey: "stateful"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	if !logging.ContainsSecret(second) || !logging.ContainsSecret(third) {
		t.Fatal("just-released URLs are no longer redacted")
	}
	releaseMany(t)
	if logging.ContainsSecret(second) || logging.ContainsSecret(third) {
		t.Fatal("URLs no longer stored are still held")
	}
}
