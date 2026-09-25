package integrations

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/config"
	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/logging"
)

func newTestStore(t *testing.T) (*Store, *db.DB, *config.Keyring) {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "bunkarr.db"), nil)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	kr, err := config.NewKeyring(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return NewStore(d, kr), d, kr
}

func ptr[T any](v T) *T { return &v }

func rawAPIKey(t *testing.T, d *db.DB, id int64) string {
	t.Helper()
	var v string
	if err := d.Reader().QueryRow(`SELECT api_key FROM integrations WHERE id = ?`, id).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func plexInput(name, token string) Input {
	return Input{
		Type:   TypePlex,
		Name:   name,
		URL:    "http://plex:32400/",
		APIKey: token,
		Settings: json.RawMessage(`{"dataPath":"/plex/","pathMappings":[{"plex":"/data/movies","local":"/media/movies/"}],
			"backup":{"destinationId":3,"cron":" 0 6 * * * ","enabled":true}}`),
	}
}

func TestCRUD(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newTestStore(t)
	t0 := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return t0 }

	created, err := s.Create(ctx, plexInput("Home Plex", "tok-crud-0123456789"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID == 0 || created.Type != TypePlex || created.Name != "Home Plex" || created.URL != "http://plex:32400" ||
		!created.Enabled || !created.HasAPIKey || !created.CreatedAt.Equal(t0) || !created.UpdatedAt.Equal(t0) {
		t.Fatalf("Create returned %+v", created)
	}
	ps, err := created.PlexSettings()
	if err != nil {
		t.Fatal(err)
	}
	want := PlexSettings{DataPath: "/plex", PathMappings: []PathMapping{{Plex: "/data/movies", Local: "/media/movies"}},
		Backup: PlexBackup{DestinationID: 3, Cron: "0 6 * * *", Enabled: true}}
	if !equalPlexSettings(ps, want) {
		t.Fatalf("stored settings = %+v, want normalized %+v", ps, want)
	}

	got, err := s.Get(ctx, created.ID)
	if err != nil || got.Name != created.Name || string(got.Settings) != string(created.Settings) {
		t.Fatalf("Get = %+v, %v", got, err)
	}

	other, err := s.Create(ctx, Input{Type: TypeSonarr, Name: "another", URL: "https://sonarr.example/base/", Enabled: ptr(false)})
	if err != nil {
		t.Fatalf("Create sonarr: %v", err)
	}
	if other.Enabled || other.HasAPIKey || string(other.Settings) != "{}" || other.URL != "https://sonarr.example/base" {
		t.Fatalf("Create sonarr returned %+v", other)
	}

	list, err := s.List(ctx)
	if err != nil || len(list) != 2 || list[0].Name != "another" || list[1].Name != "Home Plex" {
		t.Fatalf("List = %+v, %v (want ordered by name)", list, err)
	}

	t1 := t0.Add(time.Hour)
	s.now = func() time.Time { return t1 }
	// A new URL needs the key again (TestURLChangeNeedsTheAPIKeyAgain).
	upd, err := s.Update(ctx, created.ID, Input{Name: "Plex", URL: "https://plex.local:32400", APIKey: "tok-crud-0123456789", Enabled: ptr(false)})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if upd.Name != "Plex" || upd.URL != "https://plex.local:32400" || upd.Enabled || !upd.HasAPIKey ||
		!upd.CreatedAt.Equal(t0) || !upd.UpdatedAt.Equal(t1) || string(upd.Settings) != string(created.Settings) {
		t.Fatalf("Update returned %+v (settings and key must be kept)", upd)
	}
	upd, err = s.Update(ctx, created.ID, Input{Type: TypePlex, Name: "Plex", URL: "https://plex.local:32400", Settings: json.RawMessage(`{}`)})
	if err != nil || upd.Enabled || string(upd.Settings) != `{"dataPath":"","pathMappings":[],"backup":{"destinationId":0,"cron":"","enabled":false}}` {
		t.Fatalf("Update settings = %+v, %v (nil Enabled keeps false)", upd, err)
	}

	if err := s.Delete(ctx, created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete: %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Delete: %v, want ErrNotFound", err)
	}
	if _, err := s.Update(ctx, created.ID, Input{Name: "x", URL: "http://x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update of deleted: %v, want ErrNotFound", err)
	}
	if _, err := s.Token(ctx, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Token of deleted: %v, want ErrNotFound", err)
	}
}

func equalPlexSettings(a, b PlexSettings) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return bytes.Equal(ja, jb)
}

func TestAPIKeyIsSealedAtRest(t *testing.T) {
	ctx := context.Background()
	s, d, _ := newTestStore(t)
	const token = "sealed-at-rest-TOKEN-4711"
	it, err := s.Create(ctx, plexInput("plex", "  "+token+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	raw := rawAPIKey(t, d, it.ID)
	if !strings.HasPrefix(raw, "v1:") || strings.Contains(raw, token) {
		t.Fatalf("api_key column holds %q, want a sealed value", raw)
	}
	var dump string
	if err := d.Reader().QueryRow(`SELECT type || name || url || api_key || settings FROM integrations`).Scan(&dump); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dump, token) {
		t.Fatal("token stored in plaintext")
	}
	body, err := json.Marshal(it)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), token) || !strings.Contains(string(body), `"hasApiKey":true`) {
		t.Fatalf("API representation leaks the token or lacks hasApiKey: %s", body)
	}
	got, err := s.Token(ctx, it.ID)
	if err != nil || got != token {
		t.Fatalf("Token = %q, %v; want the trimmed token", got, err)
	}
	if !logging.ContainsSecret("x " + token + " y") {
		t.Fatal("the stored token is not registered for redaction")
	}
}

func TestSealedKeyIsBoundToItsRow(t *testing.T) {
	ctx := context.Background()
	s, d, _ := newTestStore(t)
	a, err := s.Create(ctx, plexInput("a", "token-of-row-a-000001"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Create(ctx, plexInput("b", "token-of-row-b-000002"))
	if err != nil {
		t.Fatal(err)
	}
	sealedA := rawAPIKey(t, d, a.ID)
	if err := d.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE integrations SET api_key = ? WHERE id = ?`, sealedA, b.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Token(ctx, b.ID); !errors.Is(err, config.ErrKeyMismatch) {
		t.Fatalf("Token of a row holding another row's sealed key: %v, want ErrKeyMismatch", err)
	}
	if got, err := s.Token(ctx, a.ID); err != nil || got != "token-of-row-a-000001" {
		t.Fatalf("Token(a) = %q, %v", got, err)
	}
	err = s.RegisterSecrets(ctx)
	if !errors.Is(err, config.ErrKeyMismatch) || !strings.Contains(err.Error(), `"b"`) {
		t.Fatalf("RegisterSecrets = %v, want a key mismatch naming integration b", err)
	}
}

func TestSealedKeyNeedsTheSameMasterKey(t *testing.T) {
	ctx := context.Background()
	s, d, _ := newTestStore(t)
	it, err := s.Create(ctx, plexInput("plex", "token-master-key-check"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := config.NewKeyring(bytes.Repeat([]byte{8}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(d, other).Token(ctx, it.ID); !errors.Is(err, config.ErrKeyMismatch) {
		t.Fatalf("Token with another master key: %v, want ErrKeyMismatch", err)
	}
	if _, err := NewStore(d, nil).Token(ctx, it.ID); err == nil {
		t.Fatal("Token without a keyring succeeded")
	}
}

func TestUpdateAPIKey(t *testing.T) {
	ctx := context.Background()
	const original = "original-token-123456"
	tests := []struct {
		name    string
		in      Input
		want    string
		wantErr string
	}{
		{name: "empty keeps", in: Input{Name: "p", URL: "http://plex:32400"}, want: original},
		{name: "spaces keep", in: Input{Name: "p", URL: "http://plex:32400", APIKey: "   "}, want: original},
		{name: "new replaces", in: Input{Name: "p", URL: "http://plex:32400", APIKey: "replacement-token-99"}, want: "replacement-token-99"},
		{name: "clear removes", in: Input{Name: "p", URL: "http://plex:32400", ClearAPIKey: true}, want: ""},
		{name: "clear with key", in: Input{Name: "p", URL: "http://plex:32400", APIKey: "x-token-value", ClearAPIKey: true}, wantErr: "cannot be used together"},
		{name: "control characters", in: Input{Name: "p", URL: "http://plex:32400", APIKey: "abc\x00def"}, wantErr: "control characters"},
		{name: "same url spelled differently keeps", in: Input{Name: "p", URL: " http://plex:32400/ "}, want: original},
		{name: "new url without key", in: Input{Name: "p", URL: "http://attacker:32400"}, wantErr: "the URL changed, so enter the token again"},
		{name: "new url with new key", in: Input{Name: "p", URL: "http://plex2:32400", APIKey: "replacement-token-99"}, want: "replacement-token-99"},
		{name: "new url with clear", in: Input{Name: "p", URL: "http://plex2:32400", ClearAPIKey: true}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, d, _ := newTestStore(t)
			it, err := s.Create(ctx, plexInput("p", original))
			if err != nil {
				t.Fatal(err)
			}
			before := rawAPIKey(t, d, it.ID)
			upd, err := s.Update(ctx, it.ID, tt.in)
			if tt.wantErr != "" {
				var verr ValidationError
				if !errors.As(err, &verr) || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Update error = %v, want ValidationError containing %q", err, tt.wantErr)
				}
				if rawAPIKey(t, d, it.ID) != before {
					t.Fatal("a rejected update changed the stored key")
				}
				return
			}
			if err != nil {
				t.Fatalf("Update: %v", err)
			}
			if upd.HasAPIKey != (tt.want != "") {
				t.Fatalf("HasAPIKey = %v, want %v", upd.HasAPIKey, tt.want != "")
			}
			got, err := s.Token(ctx, it.ID)
			if err != nil || got != tt.want {
				t.Fatalf("Token = %q, %v; want %q", got, err, tt.want)
			}
			if tt.want == original && rawAPIKey(t, d, it.ID) != before {
				t.Fatal("keeping the key re-sealed it")
			}
		})
	}
}

// TestURLChangeNeedsTheAPIKeyAgain: the stored key is bound to the stored URL, so an update that
// points the integration at another URL without the key is refused and changes nothing (design
// S8: the key must never be sent to a host the caller picks without knowing the key).
func TestURLChangeNeedsTheAPIKeyAgain(t *testing.T) {
	ctx := context.Background()
	s, d, _ := newTestStore(t)
	it, err := s.Create(ctx, plexInput("p", "bound-token-1234567890"))
	if err != nil {
		t.Fatal(err)
	}
	before := rawAPIKey(t, d, it.ID)
	_, err = s.Update(ctx, it.ID, Input{Name: "p", URL: "http://attacker:32400"})
	var verr ValidationError
	if !errors.As(err, &verr) || !strings.HasPrefix(err.Error(), "the URL changed, so enter the token again") ||
		strings.Contains(err.Error(), "attacker") {
		t.Fatalf("Update to another URL without the key = %v, want a ValidationError asking for the key", err)
	}
	if got, _ := s.Get(ctx, it.ID); got.URL != "http://plex:32400" || !got.HasAPIKey || rawAPIKey(t, d, it.ID) != before {
		t.Fatalf("a refused update changed the integration: %+v", got)
	}
	// Without a stored key there is nothing to protect: the URL changes freely.
	other, err := s.Create(ctx, Input{Type: TypeSonarr, Name: "s", URL: "http://sonarr:8989"})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Update(ctx, other.ID, Input{Name: "s", URL: "http://sonarr2:8989"}); err != nil || got.URL != "http://sonarr2:8989" {
		t.Fatalf("URL change without a stored key = %+v, %v", got, err)
	}
}

func TestCreateValidation(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newTestStore(t)
	valid := func(mod func(*Input)) Input {
		in := Input{Type: TypeRadarr, Name: "radarr", URL: "http://radarr:7878"}
		mod(&in)
		return in
	}
	tests := []struct {
		name    string
		in      Input
		wantErr string // "" = accepted
	}{
		{"ok", valid(func(*Input) {}), ""},
		{"every type", valid(func(in *Input) { in.Type = TypeMaintainerr; in.Name = "m" }), ""},
		{"unknown type", valid(func(in *Input) { in.Type = "emby" }), "type must be one of"},
		{"empty type", valid(func(in *Input) { in.Type = "" }), "type must be one of"},
		{"empty name", valid(func(in *Input) { in.Name = "  " }), "name must be 1 to 64"},
		{"name 64 chars", valid(func(in *Input) { in.Name = strings.Repeat("é", 64) }), ""},
		{"name 65 chars", valid(func(in *Input) { in.Name = strings.Repeat("a", 65) }), "name must be 1 to 64"},
		{"name control char", valid(func(in *Input) { in.Name = "a\tb" }), "control characters"},
		{"missing url", valid(func(in *Input) { in.URL = "" }), "url is required"},
		{"relative url", valid(func(in *Input) { in.URL = "radarr:7878" }), "http:// or https://"},
		{"ftp url", valid(func(in *Input) { in.URL = "ftp://radarr" }), "http:// or https://"},
		{"userinfo", valid(func(in *Input) { in.URL = "http://admin:pa55word@radarr:7878" }), "user name or password"},
		{"query", valid(func(in *Input) { in.URL = "http://radarr:7878/?apikey=abc" }), "query"},
		{"clear on create", valid(func(in *Input) { in.ClearAPIKey = true }), "only valid when updating"},
		{"settings not object", valid(func(in *Input) { in.Settings = json.RawMessage(`[1]`) }), "JSON object"},
		{"settings too large", valid(func(in *Input) { in.Settings = json.RawMessage(`{"a":"` + strings.Repeat("x", MaxSettingsLen) + `"}`) }), "at most"},
		{"api key too long", valid(func(in *Input) { in.APIKey = strings.Repeat("k", MaxAPIKeyLen+1) }), "at most"},
		{"plex settings invalid", Input{Type: TypePlex, Name: "p", URL: "http://plex:32400", Settings: json.RawMessage(`{"dataPath":"relative"}`)}, "dataPath"},
		{"plex settings wrong type", Input{Type: TypePlex, Name: "p", URL: "http://plex:32400", Settings: json.RawMessage(`{"pathMappings":"x"}`)}, "pathMappings"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			it, err := s.Create(ctx, tt.in)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Create: %v", err)
				}
				if err := s.Delete(ctx, it.ID); err != nil {
					t.Fatal(err)
				}
				return
			}
			var verr ValidationError
			if !errors.As(err, &verr) || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Create error = %v, want ValidationError containing %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), "pa55word") || strings.Contains(err.Error(), "apikey=abc") {
				t.Fatalf("validation error repeats a credential: %v", err)
			}
		})
	}
	if list, err := s.List(ctx); err != nil || len(list) != 0 {
		t.Fatalf("rejected inputs left rows: %+v, %v", list, err)
	}
}

func TestNameMustBeUnique(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newTestStore(t)
	if _, err := s.Create(ctx, plexInput("Plex", "")); err != nil {
		t.Fatal(err)
	}
	second, err := s.Create(ctx, plexInput("Second", ""))
	if err != nil {
		t.Fatal(err)
	}
	var verr ValidationError
	if _, err := s.Create(ctx, plexInput("  pLEX ", "")); !errors.As(err, &verr) || verr != "name already used" {
		t.Fatalf("duplicate Create = %v, want ValidationError \"name already used\"", err)
	}
	if _, err := s.Update(ctx, second.ID, Input{Name: "PLEX", URL: "http://plex:32400"}); !errors.As(err, &verr) || verr != "name already used" {
		t.Fatalf("rename onto an existing name = %v, want ValidationError \"name already used\"", err)
	}
	if _, err := s.Update(ctx, second.ID, Input{Name: "SECOND", URL: "http://plex:32400"}); err != nil {
		t.Fatalf("changing only the case of its own name: %v", err)
	}
}

func TestTypeCannotChange(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newTestStore(t)
	it, err := s.Create(ctx, plexInput("plex", ""))
	if err != nil {
		t.Fatal(err)
	}
	var verr ValidationError
	if _, err := s.Update(ctx, it.ID, Input{Type: TypeSonarr, Name: "plex", URL: "http://plex:32400"}); !errors.As(err, &verr) {
		t.Fatalf("type change = %v, want ValidationError", err)
	}
	if _, err := s.Update(ctx, it.ID, Input{Name: "plex", URL: "http://plex:32400", Settings: json.RawMessage(`{"dataPath":"x"}`)}); !errors.As(err, &verr) {
		t.Fatalf("invalid plex settings on update = %v, want ValidationError", err)
	}
	if _, err := (Integration{Type: TypeSonarr}).PlexSettings(); err == nil {
		t.Fatal("PlexSettings of a sonarr integration succeeded")
	}
}

func TestTokenWithoutKey(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newTestStore(t)
	it, err := s.Create(ctx, plexInput("plex", ""))
	if err != nil {
		t.Fatal(err)
	}
	if it.HasAPIKey {
		t.Fatal("HasAPIKey without a key")
	}
	if got, err := s.Token(ctx, it.ID); err != nil || got != "" {
		t.Fatalf("Token = %q, %v; want empty", got, err)
	}
}

func TestRegisterSecrets(t *testing.T) {
	ctx := context.Background()
	s, d, kr := newTestStore(t)
	it, err := s.Create(ctx, plexInput("plex", ""))
	if err != nil {
		t.Fatal(err)
	}
	// Store a sealed key directly, as a previous process would have: nothing has registered it.
	const token = "startup-only-token-Qx81"
	sealed, err := kr.Seal(token, apiKeyAAD(it.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE integrations SET api_key = ? WHERE id = ?`, sealed, it.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if logging.ContainsSecret(token) {
		t.Fatal("token registered before RegisterSecrets")
	}
	if err := s.RegisterSecrets(ctx); err != nil {
		t.Fatalf("RegisterSecrets: %v", err)
	}
	if !logging.ContainsSecret(token) {
		t.Fatal("RegisterSecrets did not register the stored token")
	}

	var buf bytes.Buffer
	log, closer, err := logging.New(logging.Options{Level: "debug", StdoutFormat: "json", Stdout: &buf})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	log.Info("connecting with " + token)
	if strings.Contains(buf.String(), token) {
		t.Fatalf("registered token leaked into a log line: %s", buf.String())
	}
}

func TestNormalizeURL(t *testing.T) {
	tests := []struct {
		in, want string
		ok       bool
	}{
		{"http://plex:32400", "http://plex:32400", true},
		{"  http://plex:32400/  ", "http://plex:32400", true},
		{"HTTPS://Plex.Example:443/base///", "https://Plex.Example:443/base", true},
		{"http://[::1]:32400/", "http://[::1]:32400", true},
		{"http://10.0.0.5:32400/plex%20server/", "http://10.0.0.5:32400/plex%20server", true},
		{"", "", false},
		{"plex:32400", "", false},
		{"//plex:32400", "", false},
		{"http://", "", false},
		{"http:///path", "", false},
		{"http://:32400", "", false},
		{"http://user@plex:32400", "", false},
		{"http://user:secret@plex:32400", "", false},
		{"http://plex:32400/?X-Plex-Token=abc", "", false},
		{"http://plex:32400/?", "", false},
		{"http://plex:32400/#frag", "", false},
		{"http://plex:32400/#", "", false},
		{"http://plex:32400/a b", "", false},
		{"http://plex:port", "", false},
		{"file:///etc/passwd", "", false},
		{"javascript:alert(1)", "", false},
		{"http://" + strings.Repeat("a", MaxURLLen), "", false},
	}
	for _, tt := range tests {
		got, err := NormalizeURL(tt.in)
		if tt.ok != (err == nil) || got != tt.want {
			t.Errorf("NormalizeURL(%q) = %q, %v; want %q ok=%v", tt.in, got, err, tt.want, tt.ok)
		}
		var verr ValidationError
		if err != nil && !errors.As(err, &verr) {
			t.Errorf("NormalizeURL(%q) error %T is not a ValidationError", tt.in, err)
		}
		if err != nil && strings.Contains(err.Error(), "secret") {
			t.Errorf("NormalizeURL(%q) error repeats the password: %v", tt.in, err)
		}
	}
}

// TestTokenForIsBoundToTheStoredURL: TokenFor reads the URL and the token from the same row, so a
// caller that read the integration while its URL pointed elsewhere never gets the token saved
// later for the real URL (S8), and the other way round.
func TestTokenForIsBoundToTheStoredURL(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newTestStore(t)
	const real, junk = "real-token-1234567890", "junk-token-0000000000"
	it, err := s.Create(ctx, plexInput("p", real))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.TokenFor(ctx, it.ID, it.URL); err != nil || got != real {
		t.Fatalf("TokenFor(stored URL) = %q, %v", got, err)
	}
	evil, err := s.Update(ctx, it.ID, Input{Name: "p", URL: "http://evil:32400", APIKey: junk})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.TokenFor(ctx, it.ID, it.URL); !errors.Is(err, ErrURLChanged) || got != "" {
		t.Fatalf("TokenFor(old URL) after the URL changed = %q, %v; want ErrURLChanged", got, err)
	}
	back, err := s.Update(ctx, it.ID, Input{Name: "p", URL: it.URL, APIKey: real})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.TokenFor(ctx, it.ID, evil.URL); !errors.Is(err, ErrURLChanged) || got != "" {
		t.Fatalf("TokenFor(URL read before the owner restored it) = %q, %v; want ErrURLChanged", got, err)
	}
	if got, err := s.TokenFor(ctx, it.ID, back.URL); err != nil || got != real {
		t.Fatalf("TokenFor(restored URL) = %q, %v", got, err)
	}
	if _, err := s.TokenFor(ctx, 999, back.URL); !errors.Is(err, ErrNotFound) {
		t.Fatalf("TokenFor(unknown id) = %v, want ErrNotFound", err)
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

// TestReplacedTokensAreReleased: the redaction registry holds each integration's stored token,
// never dropped, and releases a token that was replaced, cleared or deleted, so saving new
// tokens cannot grow it without bound.
func TestReplacedTokensAreReleased(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newTestStore(t)
	const first, second, third = "first-token-replaced-111", "second-token-deleted-222", "third-token-cleared-333"
	it, err := s.Create(ctx, plexInput("p", first))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(ctx, it.ID, Input{Name: "p", URL: it.URL, APIKey: second}); err != nil {
		t.Fatal(err)
	}
	other, err := s.Create(ctx, plexInput("q", third))
	if err != nil {
		t.Fatal(err)
	}
	releaseMany(t)
	if logging.ContainsSecret(first) || !logging.ContainsSecret(second) || !logging.ContainsSecret(third) {
		t.Fatalf("after a replace: first %v, second %v, third %v; want only the stored tokens redacted",
			logging.ContainsSecret(first), logging.ContainsSecret(second), logging.ContainsSecret(third))
	}
	if err := s.Delete(ctx, it.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(ctx, other.ID, Input{Name: "q", URL: other.URL, ClearAPIKey: true}); err != nil {
		t.Fatal(err)
	}
	if !logging.ContainsSecret(second) || !logging.ContainsSecret(third) {
		t.Fatal("a just-released token is no longer redacted")
	}
	releaseMany(t)
	if logging.ContainsSecret(second) || logging.ContainsSecret(third) {
		t.Fatal("a deleted or cleared token is still held")
	}
}

// TestAReadDoesNotReplaceTheStoredTokenInTheRegistry: a reader (TokenFor, as GET /plex/sections
// or a Plex DB backup calls it) reads the key, the owner saves a new one, and only then does the
// reader open what it read. The registry keeps holding the stored token, not the replaced one.
func TestAReadDoesNotReplaceTheStoredTokenInTheRegistry(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newTestStore(t)
	const replaced, stored = "replaced-by-owner-token-aaaa", "stored-by-owner-token-bbbb"
	it, err := s.Create(ctx, plexInput("p", replaced))
	if err != nil {
		t.Fatal(err)
	}
	_, sealed, err := s.readKey(ctx, it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(ctx, it.ID, Input{Name: "p", URL: it.URL, APIKey: stored}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.unseal(it.ID, sealed); err != nil || got != replaced {
		t.Fatalf("unseal = %q, %v", got, err)
	}
	releaseMany(t)
	if !logging.ContainsSecret(stored) || logging.ContainsSecret(replaced) {
		t.Fatalf("stored token redacted: %v, replaced token still held: %v; want only the stored one held",
			logging.ContainsSecret(stored), logging.ContainsSecret(replaced))
	}
}

// TestTheRegistryHoldsTheLastStoredToken: a write whose registry update is late (the start-up
// registration, or another update) cannot land after the registry update of a write that
// committed later: the registry ends up holding the token that is stored.
func TestTheRegistryHoldsTheLastStoredToken(t *testing.T) {
	ctx := context.Background()
	const last = "last-saved-token-22222222"
	for _, tc := range []struct {
		name  string
		first func(s *Store, id int64) error
	}{
		{"two updates", func(s *Store, id int64) error {
			_, err := s.Update(ctx, id, Input{Name: "p", URL: "http://plex:32400", APIKey: "first-saved-token-11111111"})
			return err
		}},
		{"start-up registration and an update", func(s *Store, _ int64) error { return s.RegisterSecrets(ctx) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := newTestStore(t)
			it, err := s.Create(ctx, plexInput("p", "created-token-000000000"))
			if err != nil {
				t.Fatal(err)
			}
			firstErr, secondErr := raceAtHold(func() error { return tc.first(s, it.ID) }, func() error {
				_, err := s.Update(ctx, it.ID, Input{Name: "p", URL: it.URL, APIKey: last})
				return err
			})
			if firstErr != nil || secondErr != nil {
				t.Fatalf("first: %v, second: %v", firstErr, secondErr)
			}
			releaseMany(t)
			if !logging.ContainsSecret(last) {
				t.Fatal("the stored token is no longer redacted: a late registry update replaced it")
			}
			if got, err := s.TokenFor(ctx, it.ID, it.URL); err != nil || got != last {
				t.Fatalf("stored token = %q, %v", got, err)
			}
		})
	}
}

// raceAtHold runs first and, once it reaches pointBeforeHold (it has committed, or read, what it
// holds in the redaction registry and has not updated the registry yet), starts second and gives
// it up to 500ms to finish before first goes on. While first holds registryMu there, a second
// write waits for it; without the lock, second finishes first and its registry update lands
// before first's. raceAtHold returns once both are done.
func raceAtHold(first, second func() error) (firstErr, secondErr error) {
	var fired atomic.Bool
	done := make(chan error, 1)
	faultinject.SetHook(func(name string) {
		if name != pointBeforeHold || !fired.CompareAndSwap(false, true) {
			return
		}
		go func() { done <- second() }()
		select {
		case err := <-done:
			done <- err
		case <-time.After(500 * time.Millisecond):
		}
	})
	defer faultinject.SetHook(nil)
	firstErr = first()
	if !fired.Load() {
		return firstErr, errors.New("the first call never reached " + pointBeforeHold)
	}
	return firstErr, <-done
}

// TestCreateAndDeleteOrderTheirRegistryUpdates: Create and Delete hold registryMu from their
// write until they have updated the redaction registry, like Update. Without it in Create, a
// create whose registry update is late replaces the token an update stored after it, or holds
// the token of an integration deleted in between; without it in Delete, a delete that lands
// between the start-up registration's read and its registry update (or between a create and its
// registry update) leaves the deleted integration's token held for good.
func TestCreateAndDeleteOrderTheirRegistryUpdates(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		// existing is the token of integration "p" created before the race ("" = none yet).
		existing string
		first    func(s *Store) error
		second   func(s *Store, id int64) error
		// held must be redacted afterwards and be the stored token ("" = the integration was
		// deleted); released must no longer be held.
		held, released string
	}{
		{
			name: "a create and an update",
			first: func(s *Store) error {
				_, err := s.Create(ctx, plexInput("p", "created-then-updated-token-5555"))
				return err
			},
			second: func(s *Store, id int64) error {
				_, err := s.Update(ctx, id, Input{Name: "p", URL: "http://plex:32400", APIKey: "updated-after-create-token-6666"})
				return err
			},
			held:     "updated-after-create-token-6666",
			released: "created-then-updated-token-5555",
		},
		{
			name: "a create and a delete",
			first: func(s *Store) error {
				_, err := s.Create(ctx, plexInput("p", "created-then-deleted-token-7777"))
				return err
			},
			second:   func(s *Store, id int64) error { return s.Delete(ctx, id) },
			released: "created-then-deleted-token-7777",
		},
		{
			name:     "the start-up registration and a delete",
			existing: "registered-then-deleted-token-8888",
			first:    func(s *Store) error { return s.RegisterSecrets(ctx) },
			second:   func(s *Store, id int64) error { return s.Delete(ctx, id) },
			released: "registered-then-deleted-token-8888",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, d, _ := newTestStore(t)
			if tc.existing != "" {
				if _, err := s.Create(ctx, plexInput("p", tc.existing)); err != nil {
					t.Fatal(err)
				}
			}
			var id int64
			firstErr, secondErr := raceAtHold(func() error { return tc.first(s) }, func() error {
				if err := d.Reader().QueryRowContext(ctx, `SELECT id FROM integrations WHERE name = 'p'`).Scan(&id); err != nil {
					return err
				}
				return tc.second(s, id)
			})
			if firstErr != nil || secondErr != nil {
				t.Fatalf("first: %v, second: %v", firstErr, secondErr)
			}
			releaseMany(t)
			if tc.held != "" && !logging.ContainsSecret(tc.held) {
				t.Error("the stored token is no longer redacted: a late registry update replaced it")
			}
			if logging.ContainsSecret(tc.released) {
				t.Error("a replaced or deleted token is still held: a late registry update held it again")
			}
			got, err := s.Token(ctx, id)
			if tc.held == "" {
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("Token after the delete = %q, %v; want ErrNotFound", got, err)
				}
			} else if err != nil || got != tc.held {
				t.Fatalf("stored token = %q, %v; want %q", got, err, tc.held)
			}
		})
	}
}
