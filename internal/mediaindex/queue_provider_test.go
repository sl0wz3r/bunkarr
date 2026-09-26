package mediaindex

import (
	"context"
	"strconv"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/integrations"
)

func TestNeedsRefreshProviders(t *testing.T) {
	linked := func(typ integrations.Type, url string, plexID int) integrations.Integration {
		return integrations.Integration{Type: typ, URL: url, Enabled: true, Settings: []byte(`{"plexIntegrationId":` + strconv.Itoa(plexID) + `}`)}
	}
	plexIt := func(index bool, mapping string) integrations.Integration {
		s := `{"pathMappings":[{"plex":"/data","local":"` + mapping + `"}],"index":{"enabled":` + map[bool]string{true: "true", false: "false"}[index] + `}}`
		return integrations.Integration{Type: integrations.TypePlex, URL: "http://plex:32400", Enabled: true, Settings: []byte(s)}
	}
	ta := linked(integrations.TypeTautulli, "http://t:8181", 1)
	cases := []struct {
		name   string
		before *integrations.Integration
		after  integrations.Integration
		key    bool
		want   bool
	}{
		{"tautulli created", nil, ta, false, true},
		{"tautulli unchanged", &ta, ta, false, false},
		{"tautulli key", &ta, ta, true, true},
		{"tautulli url", &ta, linked(integrations.TypeTautulli, "http://t2:8181", 1), false, true},
		{"tautulli relinked", &ta, linked(integrations.TypeTautulli, "http://t:8181", 2), false, true},
		{"seerr created", nil, linked(integrations.TypeSeerr, "http://s:5055", 0), false, true},
		{"maintainerr relinked", ptr(linked(integrations.TypeMaintainerr, "http://m:6246", 1)), linked(integrations.TypeMaintainerr, "http://m:6246", 3), false, true},
		{"plex index turned on", ptr(plexIt(false, "/media")), plexIt(true, "/media"), false, true},
		{"plex index unchanged", ptr(plexIt(true, "/media")), plexIt(true, "/media"), false, false},
		{"plex mappings", ptr(plexIt(true, "/media")), plexIt(true, "/srv"), false, true},
		{"plex index off", ptr(plexIt(true, "/media")), plexIt(false, "/media"), true, false},
	}
	for _, c := range cases {
		if got := NeedsRefresh(c.before, c.after, c.key); got != c.want {
			t.Errorf("%s: NeedsRefresh = %v, want %v", c.name, got, c.want)
		}
	}
	// Start-up refreshes stay *arr-only: a Tautulli or Plex index integration queues nothing.
	enq := &memEnqueuer{}
	if got, _ := QueueStartup(context.Background(), []integrations.Integration{ta, plexIt(true, "/media")}, enq); len(got) != 0 {
		t.Fatalf("queued %v", got)
	}
}
