package integrations

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPlexIndexSettings(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    PlexIndex
		stored  string
		wantErr string
	}{
		{name: "absent: off, not stored", raw: `{}`, want: PlexIndex{StaleAfterHours: 72}, stored: `{"dataPath":"","pathMappings":[],"backup":{"destinationId":0,"cron":"","enabled":false}}`},
		{name: "enabled without a cron", raw: `{"index":{"enabled":true}}`, want: PlexIndex{Enabled: true, Cron: DefaultPlexIndexCron, StaleAfterHours: 72},
			stored: `"index":{"enabled":true,"cron":"0 1 * * *","staleAfterHours":72}`},
		{name: "own cron and staleness", raw: `{"index":{"enabled":true,"cron":" 15 3 * * * ","staleAfterHours":24}}`,
			want: PlexIndex{Enabled: true, Cron: "15 3 * * *", StaleAfterHours: 24}},
		{name: "disabled keeps its cron", raw: `{"index":{"enabled":false,"cron":"15 3 * * *"}}`, want: PlexIndex{Cron: "15 3 * * *", StaleAfterHours: 72}},
		{name: "stale after too long", raw: `{"index":{"enabled":true,"staleAfterHours":721}}`, wantErr: "index.staleAfterHours"},
		{name: "bad cron", raw: `{"index":{"enabled":true,"cron":"every day"}}`, wantErr: "index.cron"},
		{name: "wrong type", raw: `{"index":{"enabled":"yes"}}`, wantErr: "enabled must be true or false"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stored, err := normalizeSettings(TypePlex, json.RawMessage(tt.raw))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tt.stored != "" && !strings.Contains(stored, tt.stored) {
				t.Fatalf("stored %s, want %s", stored, tt.stored)
			}
			ps, err := ParsePlexSettings(json.RawMessage(stored))
			if err != nil {
				t.Fatal(err)
			}
			if got := ps.IndexSettings(); got != tt.want {
				t.Fatalf("index = %+v, want %+v", got, tt.want)
			}
		})
	}
}
