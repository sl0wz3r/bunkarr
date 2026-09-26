package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParseArrSettingsDefaults(t *testing.T) {
	want := ArrSettings{
		PathMappings: []ArrPathMapping{},
		Backup:       ArrBackup{MaxScheduledAgeDays: 7},
		Refresh:      RefreshSettings{Cron: "15 */6 * * *", Enabled: true, StaleAfterHours: 24},
	}
	for _, raw := range []string{"", "null", " {} ", `{"unknown":1}`, `{"refresh":{}}`, `{"refresh":{"cron":"  "}}`} {
		got, err := ParseArrSettings(json.RawMessage(raw))
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("ParseArrSettings(%q) = %+v, %v; want the defaults", raw, got, err)
		}
		if err := got.Validate("Radarr"); err != nil {
			t.Errorf("defaults do not validate: %v", err)
		}
	}
}

func TestParseArrSettingsNormalizes(t *testing.T) {
	raw := `{"pathMappings":[{"arr":" /movies/ ","local":"/media//movies/"}],"backupFolder":" /arr/radarr-backups/ ",
		"backup":{"destinationId":3,"enabled":true,"acceptInsecureModes":true},"refresh":{"enabled":false,"staleAfterHours":48},
		"extra":{"dropped":true}}`
	got, err := ParseArrSettings(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	want := ArrSettings{
		PathMappings: []ArrPathMapping{{Arr: "/movies", Local: "/media/movies"}},
		BackupFolder: "/arr/radarr-backups",
		// An enabled backup without a cron expression is weekly; missing fields keep defaults.
		Backup:  ArrBackup{DestinationID: 3, Cron: "30 6 * * 0", Enabled: true, MaxScheduledAgeDays: 7, AcceptInsecureModes: true},
		Refresh: RefreshSettings{Cron: "15 */6 * * *", Enabled: false, StaleAfterHours: 48},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseArrSettings = %+v\nwant %+v", got, want)
	}
	if err := got.Validate("Radarr"); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(got)
	if err != nil || strings.Contains(string(b), "extra") {
		t.Fatalf("encoded settings %s, %v (unknown fields must be dropped)", b, err)
	}
	// A disabled backup keeps an empty cron expression.
	got, err = ParseArrSettings(json.RawMessage(`{"backup":{"destinationId":3}}`))
	if err != nil || got.Backup.Cron != "" {
		t.Fatalf("disabled backup = %+v, %v", got.Backup, err)
	}
	if _, err := ParseArrSettings(json.RawMessage(`{"pathMappings":"x"}`)); err == nil || !strings.Contains(err.Error(), "pathMappings must be a list") {
		t.Fatalf("wrong type: %v", err)
	}
	if _, err := ParseArrSettings(json.RawMessage(`{`)); err == nil || !strings.Contains(err.Error(), "malformed JSON") {
		t.Fatalf("malformed: %v", err)
	}
}

func TestArrSettingsValidate(t *testing.T) {
	base := func(mod func(*ArrSettings)) ArrSettings {
		s, err := ParseArrSettings(nil)
		if err != nil {
			t.Fatal(err)
		}
		mod(&s)
		return s
	}
	many := make([]ArrPathMapping, MaxPathMappings+1)
	for i := range many {
		many[i] = ArrPathMapping{Arr: "/a/" + strings.Repeat("x", i+1), Local: "/b"}
	}
	tests := []struct {
		name    string
		s       ArrSettings
		wantErr string
	}{
		{"defaults", base(func(*ArrSettings) {}), ""},
		{"mappings", base(func(s *ArrSettings) {
			s.PathMappings = []ArrPathMapping{{Arr: "/tv", Local: "/media/tv"}, {Arr: "/tv/anime", Local: "/media/anime"}}
		}), ""},
		{"relative arr path", base(func(s *ArrSettings) { s.PathMappings = []ArrPathMapping{{Arr: "tv", Local: "/media/tv"}} }), "the Radarr path must be an absolute path"},
		{"unclean local path", base(func(s *ArrSettings) { s.PathMappings = []ArrPathMapping{{Arr: "/tv", Local: "/media/../tv"}} }), "local path must be an absolute path"},
		{"duplicate prefix", base(func(s *ArrSettings) {
			s.PathMappings = []ArrPathMapping{{Arr: "/tv", Local: "/a"}, {Arr: "/tv", Local: "/b"}}
		}), "mapped twice"},
		{"too many", base(func(s *ArrSettings) { s.PathMappings = many }), "at most 64"},
		{"relative backup folder", base(func(s *ArrSettings) { s.BackupFolder = "backups" }), "backupFolder must be an absolute path"},
		{"enabled backup without destination", base(func(s *ArrSettings) { s.Backup.Enabled, s.Backup.Cron = true, "30 6 * * 0" }), "need a destination"},
		{"enabled backup without cron", base(func(s *ArrSettings) { s.Backup.Enabled, s.Backup.DestinationID = true, 2 }), "need a schedule"},
		{"bad backup cron", base(func(s *ArrSettings) { s.Backup.DestinationID, s.Backup.Cron = 2, "every day" }), "backup.cron"},
		{"negative destination", base(func(s *ArrSettings) { s.Backup.DestinationID = -1 }), "destinationId"},
		{"max age 0", base(func(s *ArrSettings) { s.Backup.MaxScheduledAgeDays = 0 }), "maxScheduledAgeDays must be 1 to 90"},
		{"max age 90", base(func(s *ArrSettings) { s.Backup.MaxScheduledAgeDays = 90 }), ""},
		{"max age 91", base(func(s *ArrSettings) { s.Backup.MaxScheduledAgeDays = 91 }), "maxScheduledAgeDays"},
		{"stale 0", base(func(s *ArrSettings) { s.Refresh.StaleAfterHours = 0 }), "staleAfterHours must be 1 to 720"},
		{"stale 720", base(func(s *ArrSettings) { s.Refresh.StaleAfterHours = 720 }), ""},
		{"stale 721", base(func(s *ArrSettings) { s.Refresh.StaleAfterHours = 721 }), "staleAfterHours"},
		{"bad refresh cron", base(func(s *ArrSettings) { s.Refresh.Cron = "61 * * * *" }), "refresh.cron"},
	}
	for _, tt := range tests {
		err := tt.s.Validate("Radarr")
		if tt.wantErr == "" {
			if err != nil {
				t.Errorf("%s: %v", tt.name, err)
			}
			continue
		}
		var verr ValidationError
		if !errors.As(err, &verr) || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("%s: %v, want ValidationError containing %q", tt.name, err, tt.wantErr)
		}
	}
}

// TestMapPathIsPlexMapPath checks that the generic MapPath gives exactly what
// PlexSettings.MapPath gives, for Plex's and the *arrs' pairs alike.
func TestMapPathIsPlexMapPath(t *testing.T) {
	plexPairs := []PathMapping{{Plex: "/data", Local: "/media"}, {Plex: "/data/movies", Local: "/mnt/movies/"}, {Plex: "/", Local: "/root"},
		{Plex: "relative", Local: "/x"}}
	arrPairs := make([]ArrPathMapping, len(plexPairs))
	for i, m := range plexPairs {
		arrPairs[i] = ArrPathMapping{Arr: m.Plex, Local: m.Local}
	}
	ps := PlexSettings{PathMappings: plexPairs}
	as := ArrSettings{PathMappings: arrPairs}
	for _, p := range []string{"/data", "/data/", "/data/movies/Heat (1995)/Heat.mkv", "/database/x", "/data/movies2", "/other",
		"relative/x", "", " /data/tv/../movies/x ", "/data/movies/../tv"} {
		wantLocal, wantOK := ps.MapPath(p)
		for name, got := range map[string]func(string) (string, bool){
			"MapPath(plex)":   func(p string) (string, bool) { return MapPath(plexPairs, p) },
			"MapPath(arr)":    func(p string) (string, bool) { return MapPath(arrPairs, p) },
			"ArrSettings.Map": as.MapPath,
		} {
			if l, ok := got(p); l != wantLocal || ok != wantOK {
				t.Errorf("%s(%q) = %q, %v; PlexSettings.MapPath gives %q, %v", name, p, l, ok, wantLocal, wantOK)
			}
		}
	}
	if l, ok := MapPath([]ArrPathMapping{{Arr: "/movies", Local: "/media/movies"}}, "/movies/Heat (1995)"); !ok || l != "/media/movies/Heat (1995)" {
		t.Fatalf("MapPath = %q, %v", l, ok)
	}
	if _, ok := MapPath([]ArrPathMapping{{Arr: "/movies", Local: "/media/movies"}}, "/movies-4k/Nosferatu (1922)"); ok {
		t.Fatal("/movies must not map /movies-4k")
	}
}

func TestMetaSettings(t *testing.T) {
	ta, err := ParseTautulliSettings(json.RawMessage(`{"plexIntegrationId":4,"x":1}`))
	if err != nil || ta != (TautulliSettings{PlexIntegrationID: 4, Refresh: RefreshSettings{Cron: "0 2 * * *", Enabled: true, StaleAfterHours: 72}}) ||
		ta.Validate() != nil {
		t.Fatalf("tautulli = %+v, %v", ta, err)
	}
	se, err := ParseSeerrSettings(nil)
	if err != nil || se != (SeerrSettings{Refresh: RefreshSettings{Cron: "30 2 * * *", Enabled: true, StaleAfterHours: 72}}) || se.Validate() != nil {
		t.Fatalf("seerr = %+v, %v (plexIntegrationId is optional)", se, err)
	}
	ma, err := ParseMaintainerrSettings(json.RawMessage(`{"plexIntegrationId":4,"refresh":{"enabled":false,"cron":" 0 * * * * "}}`))
	if err != nil || ma != (MaintainerrSettings{PlexIntegrationID: 4, Refresh: RefreshSettings{Cron: "0 * * * *", Enabled: false, StaleAfterHours: 24}}) ||
		ma.Validate() != nil {
		t.Fatalf("maintainerr = %+v, %v", ma, err)
	}
	for name, err := range map[string]error{
		"tautulli without plex":    TautulliSettings{Refresh: ta.Refresh}.Validate(),
		"maintainerr without plex": MaintainerrSettings{Refresh: ma.Refresh}.Validate(),
		"seerr negative plex":      SeerrSettings{PlexIntegrationID: -1, Refresh: se.Refresh}.Validate(),
		"stale 0":                  SeerrSettings{Refresh: RefreshSettings{Cron: "0 1 * * *", StaleAfterHours: 0}}.Validate(),
	} {
		var verr ValidationError
		if !errors.As(err, &verr) {
			t.Errorf("%s: %v, want a ValidationError", name, err)
		}
	}
	if _, err := ParseSeerrSettings(json.RawMessage(`{"plexIntegrationId":"4"}`)); err == nil {
		t.Fatal("a string id was accepted")
	}
	it := Integration{ID: 1, Type: TypeSeerr, Settings: json.RawMessage(`{}`)}
	if _, err := it.TautulliSettings(); err == nil {
		t.Fatal("TautulliSettings of a Seerr")
	}
	if _, err := it.SeerrSettings(); err != nil {
		t.Fatal(err)
	}
	if _, err := it.ArrSettings(); err == nil {
		t.Fatal("ArrSettings of a Seerr")
	}
}

// TestSettingsPerTypeThroughTheStore creates and updates every type through the store: settings
// are normalized, plexIntegrationId must name a Plex integration, and Maintainerr never has a
// key.
func TestSettingsPerTypeThroughTheStore(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newTestStore(t)
	plex, err := s.Create(ctx, plexInput("Plex", "tok-settings-0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	radarr, err := s.Create(ctx, Input{Type: TypeRadarr, Name: "Radarr", URL: "http://radarr:7878", APIKey: "radarr-key-0123456789",
		Settings: json.RawMessage(`{"pathMappings":[{"arr":"/movies/","local":"/media/movies"}],"junk":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	as, err := radarr.ArrSettings()
	if err != nil || len(as.PathMappings) != 1 || as.PathMappings[0].Arr != "/movies" || strings.Contains(string(radarr.Settings), "junk") {
		t.Fatalf("radarr settings %s, %v", radarr.Settings, err)
	}

	link := func(id int64) json.RawMessage {
		return json.RawMessage(`{"plexIntegrationId":` + itoa(id) + `}`)
	}
	tests := []struct {
		name    string
		in      Input
		wantErr string
	}{
		{"tautulli linked", Input{Type: TypeTautulli, Name: "t", URL: "http://t:8181", APIKey: "tautulli-key-0123", Settings: link(plex.ID)}, ""},
		{"tautulli unlinked", Input{Type: TypeTautulli, Name: "t2", URL: "http://t:8181", Settings: json.RawMessage(`{}`)}, "plexIntegrationId"},
		{"tautulli linked to radarr", Input{Type: TypeTautulli, Name: "t3", URL: "http://t:8181", Settings: link(radarr.ID)}, "not Plex"},
		{"tautulli linked to nothing", Input{Type: TypeTautulli, Name: "t4", URL: "http://t:8181", Settings: link(999)}, "does not exist"},
		{"seerr unlinked", Input{Type: TypeSeerr, Name: "s", URL: "http://s:5055", APIKey: "seerr-key-0123"}, ""},
		{"seerr linked", Input{Type: TypeSeerr, Name: "s2", URL: "http://s:5055", Settings: link(plex.ID)}, ""},
		{"maintainerr", Input{Type: TypeMaintainerr, Name: "m", URL: "http://m:6246", Settings: link(plex.ID)}, ""},
		{"maintainerr with a key", Input{Type: TypeMaintainerr, Name: "m2", URL: "http://m:6246", APIKey: "nope-0123456789", Settings: link(plex.ID)},
			"Maintainerr has no API authentication"},
		{"sonarr bad mapping", Input{Type: TypeSonarr, Name: "so", URL: "http://s:8989", Settings: json.RawMessage(`{"pathMappings":[{"arr":"tv","local":"/tv"}]}`)},
			"Sonarr path must be an absolute path"},
		{"lidarr stale 0", Input{Type: TypeLidarr, Name: "l", URL: "http://l:8686", Settings: json.RawMessage(`{"refresh":{"staleAfterHours":0}}`)},
			"staleAfterHours"},
	}
	created := map[string]Integration{}
	for _, tt := range tests {
		it, err := s.Create(ctx, tt.in)
		if tt.wantErr == "" {
			if err != nil {
				t.Errorf("%s: %v", tt.name, err)
			}
			created[tt.name] = it
			continue
		}
		var verr ValidationError
		if !errors.As(err, &verr) || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("%s: %v, want ValidationError containing %q", tt.name, err, tt.wantErr)
		}
	}

	// Updates are checked the same way.
	m := created["maintainerr"]
	if _, err := s.Update(ctx, m.ID, Input{Name: m.Name, URL: m.URL, APIKey: "nope-0123456789"}); err == nil ||
		!strings.Contains(err.Error(), "no API authentication") {
		t.Fatalf("Maintainerr update with a key: %v", err)
	}
	if _, err := s.Update(ctx, m.ID, Input{Name: m.Name, URL: m.URL, Settings: link(radarr.ID)}); err == nil || !strings.Contains(err.Error(), "not Plex") {
		t.Fatalf("Maintainerr relinked to Radarr: %v", err)
	}
	tu := created["tautulli linked"]
	upd, err := s.Update(ctx, tu.ID, Input{Name: tu.Name, URL: tu.URL, Settings: json.RawMessage(`{"plexIntegrationId":` + itoa(plex.ID) +
		`,"refresh":{"staleAfterHours":100}}`)})
	if err != nil {
		t.Fatal(err)
	}
	ts, err := upd.TautulliSettings()
	if err != nil || ts.Refresh.StaleAfterHours != 100 || ts.Refresh.Cron != "0 2 * * *" || !ts.Refresh.Enabled {
		t.Fatalf("tautulli after update = %+v, %v", ts, err)
	}
	// An update without settings keeps them (and still checks the stored link).
	if _, err := s.Update(ctx, tu.ID, Input{Name: "Tautulli", URL: tu.URL}); err != nil {
		t.Fatalf("update without settings: %v", err)
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestCheckArrIntegration(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newTestStore(t)
	plex, err := s.Create(ctx, plexInput("Plex", "tok-check-0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	for _, typ := range []Type{TypeSonarr, TypeRadarr, TypeLidarr} {
		it, err := s.Create(ctx, Input{Type: typ, Name: string(typ), URL: "http://" + string(typ)})
		if err != nil {
			t.Fatal(err)
		}
		if got, err := s.CheckArrIntegration(ctx, it.ID); err != nil || got != typ {
			t.Errorf("CheckArrIntegration(%s) = %s, %v", typ, got, err)
		}
	}
	var verr ValidationError
	if _, err := s.CheckArrIntegration(ctx, plex.ID); !errors.As(err, &verr) || !strings.Contains(err.Error(), "not Sonarr, Radarr or Lidarr") {
		t.Errorf("Plex: %v", err)
	}
	if _, err := s.CheckArrIntegration(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
	if !TypeLidarr.IsArr() || TypePlex.IsArr() || TypeSeerr.IsArr() || TypeMaintainerr.AppName() != "Maintainerr" {
		t.Error("type helpers")
	}
}
