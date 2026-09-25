package integrations

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestParsePlexSettings(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string // normalized JSON
		wantErr string
	}{
		{name: "empty", raw: ``, want: `{"dataPath":"","pathMappings":[],"backup":{"destinationId":0,"cron":"","enabled":false}}`},
		{name: "null", raw: `null`, want: `{"dataPath":"","pathMappings":[],"backup":{"destinationId":0,"cron":"","enabled":false}}`},
		{
			name: "cleans paths and trims",
			raw:  `{"dataPath":" /plex/./ ","pathMappings":[{"plex":"/data//movies/","local":"/media/movies/"}],"backup":{"destinationId":2,"cron":" 0 6 * * * ","enabled":true},"unknown":1}`,
			want: `{"dataPath":"/plex","pathMappings":[{"plex":"/data/movies","local":"/media/movies"}],"backup":{"destinationId":2,"cron":"0 6 * * *","enabled":true}}`,
		},
		{name: "malformed", raw: `{"dataPath":`, wantErr: "malformed JSON"},
		{name: "wrong type", raw: `{"backup":{"enabled":"yes"}}`, wantErr: "backup.enabled must be true or false"},
		{name: "wrong list", raw: `{"pathMappings":{"plex":"/a"}}`, wantErr: "pathMappings must be a list"},
		{name: "wrong number", raw: `{"backup":{"destinationId":"1"}}`, wantErr: "backup.destinationId must be a whole number"},
		{name: "not an object", raw: `"x"`, wantErr: "settings must be a JSON object"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ps, err := ParsePlexSettings(json.RawMessage(tt.raw))
			if tt.wantErr != "" {
				var verr ValidationError
				if !errors.As(err, &verr) || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want ValidationError containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, _ := json.Marshal(ps)
			if string(got) != tt.want {
				t.Fatalf("got %s\nwant %s", got, tt.want)
			}
		})
	}
}

func TestPlexSettingsValidate(t *testing.T) {
	base := func(mod func(*PlexSettings)) PlexSettings {
		s := PlexSettings{
			DataPath:     "/plex",
			PathMappings: []PathMapping{{Plex: "/data", Local: "/media"}, {Plex: "/data/movies", Local: "/movies"}},
			Backup:       PlexBackup{DestinationID: 1, Cron: "0 6 * * *", Enabled: true},
		}
		mod(&s)
		return s
	}
	tests := []struct {
		name    string
		s       PlexSettings
		wantErr string
	}{
		{"valid", base(func(*PlexSettings) {}), ""},
		{"zero", PlexSettings{}, ""},
		{"no data path without backup", base(func(s *PlexSettings) { s.DataPath = ""; s.Backup.Enabled = false }), ""},
		{"disabled backup needs nothing", base(func(s *PlexSettings) { s.Backup = PlexBackup{} }), ""},
		{"relative data path", base(func(s *PlexSettings) { s.DataPath = "plex" }), "dataPath"},
		{"unclean data path", base(func(s *PlexSettings) { s.DataPath = "/plex/" }), "dataPath"},
		{"relative plex path", base(func(s *PlexSettings) { s.PathMappings[0].Plex = "data" }), "mapping 1: the Plex path"},
		{"empty local path", base(func(s *PlexSettings) { s.PathMappings[1].Local = "" }), "mapping 2: the local path"},
		{"unclean local path", base(func(s *PlexSettings) { s.PathMappings[1].Local = "/a/../b" }), "mapping 2: the local path"},
		{"duplicate plex prefix", base(func(s *PlexSettings) { s.PathMappings[1].Plex = "/data" }), "mapped twice"},
		{"too many", base(func(s *PlexSettings) {
			s.PathMappings = nil
			for i := 0; i <= MaxPathMappings; i++ {
				s.PathMappings = append(s.PathMappings, PathMapping{Plex: "/p" + strings.Repeat("x", i), Local: "/l"})
			}
		}), "at most"},
		{"negative destination", base(func(s *PlexSettings) { s.Backup.DestinationID = -1; s.Backup.Enabled = false }), "destinationId"},
		{"enabled without data path", base(func(s *PlexSettings) { s.DataPath = "" }), "dataPath"},
		{"enabled without destination", base(func(s *PlexSettings) { s.Backup.DestinationID = 0 }), "destination"},
		{"enabled without cron", base(func(s *PlexSettings) { s.Backup.Cron = "" }), "cron"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.s.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			var verr ValidationError
			if !errors.As(err, &verr) || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate = %v, want ValidationError containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestMapPath(t *testing.T) {
	s := PlexSettings{PathMappings: []PathMapping{
		{Plex: "/data", Local: "/mnt/user/data"},
		{Plex: "/data/movies", Local: "/movies"},
		{Plex: "/data/movies/4k", Local: "/uhd/"},
		{Plex: "/tv", Local: "/media/tv"},
		{Plex: "relative", Local: "/ignored"},
		{Plex: "/broken", Local: ""},
	}}
	tests := []struct {
		in    string
		want  string
		found bool
	}{
		{"/data", "/mnt/user/data", true},
		{"/data/", "/mnt/user/data", true},
		{"/data/tv/Show/S01", "/mnt/user/data/tv/Show/S01", true},
		{"/data/movies", "/movies", true},
		{"/data/movies/Film (2020)/film.mkv", "/movies/Film (2020)/film.mkv", true},
		{"/data/movies4k", "/mnt/user/data/movies4k", true}, // not /data/movies: segment boundary
		{"/data/movies/4k/x.mkv", "/uhd/x.mkv", true},       // longest prefix wins, result cleaned
		{"/database", "", false},                            // /data must not match /database
		{"/datab/x", "", false},
		{"/tv/../data/x", "/mnt/user/data/x", true}, // cleaned before matching
		{"/tv", "/media/tv", true},
		{"/tvshows", "", false},
		{"/other", "", false},
		{"relative/x", "", false},
		{"relative", "", false},
		{"", "", false},
		{"/broken/x", "", false}, // a mapping with an invalid local path is ignored
	}
	for _, tt := range tests {
		got, ok := s.MapPath(tt.in)
		if got != tt.want || ok != tt.found {
			t.Errorf("MapPath(%q) = %q, %v; want %q, %v", tt.in, got, ok, tt.want, tt.found)
		}
	}

	root := PlexSettings{PathMappings: []PathMapping{{Plex: "/", Local: "/host"}, {Plex: "/media", Local: "/m"}}}
	for in, want := range map[string]string{"/": "/host", "/x/y": "/host/x/y", "/media/a": "/m/a", "/mediax": "/host/mediax"} {
		if got, ok := root.MapPath(in); !ok || got != want {
			t.Errorf("root mapping MapPath(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	if _, ok := (PlexSettings{}).MapPath("/data"); ok {
		t.Error("MapPath without mappings reported a match")
	}
}
