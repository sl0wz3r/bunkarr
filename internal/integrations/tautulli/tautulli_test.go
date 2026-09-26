package tautulli_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/integrations/httpread"
	"github.com/sl0wz3r/bunkarr/internal/integrations/httpread/readtest"
	"github.com/sl0wz3r/bunkarr/internal/integrations/tautulli"
	"github.com/sl0wz3r/bunkarr/internal/integrations/tautulli/tautullitest"
	"github.com/sl0wz3r/bunkarr/internal/netguard"
)

func newClient(t *testing.T, s *tautullitest.Server, key string, o tautulli.Options) *tautulli.Client {
	t.Helper()
	o.HTTPClient = s.Client()
	c, err := tautulli.New(s.URL, key, o)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// allowed are the only commands the client may send (§4.4).
var allowed = []string{"get_tautulli_info", "get_server_info", "get_library", "get_users", "get_history"}

// checkRequests asserts the allow-list and the key rules on everything the fake received.
func checkRequests(t *testing.T, s *tautullitest.Server, key string) {
	t.Helper()
	for _, r := range s.Requests() {
		if r.Method != http.MethodGet || r.Path != "/api/v2" {
			t.Errorf("request %s %s outside the allow-list", r.Method, r.Path)
		}
		if strings.Contains(r.RawQuery, "refresh") || strings.Contains(r.RawQuery, "get_library_media_info") {
			t.Errorf("forbidden query %q", r.RawQuery)
		}
		if key != "" && strings.Contains(r.RawQuery, key) {
			t.Errorf("the key was sent in the query %q", r.RawQuery)
		}
		if got := r.Header.Get("X-Api-Key"); got != key {
			t.Errorf("X-Api-Key = %q, want the key", got)
		}
		cmd := ""
		for _, kv := range strings.Split(r.RawQuery, "&") {
			if v, ok := strings.CutPrefix(kv, "cmd="); ok {
				cmd = v
			}
		}
		if !slices.Contains(allowed, cmd) {
			t.Errorf("command %q is not allowed", cmd)
		}
	}
}

func TestInfoAndServerInfo(t *testing.T) {
	s := tautullitest.NewServer(t)
	c := newClient(t, s, tautullitest.Key, tautulli.Options{})
	ctx := context.Background()
	info, err := c.Info(ctx)
	if err != nil || info.Version != "v2.18.1" {
		t.Fatalf("Info = %+v, %v", info, err)
	}
	si, err := c.ServerInfo(ctx)
	if err != nil || si.PMSIdentifier != tautullitest.MachineIdentifier || si.PMSName != "bunkarr-fixture-plex" {
		t.Fatalf("ServerInfo = %+v, %v", si, err)
	}
	checkRequests(t, s, tautullitest.Key)
}

func TestErrors(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*tautullitest.Server)
		key   string
		want  error
		code  int
	}{
		{"no key", nil, "", tautulli.ErrUnauthorized, 401},
		{"wrong key", nil, "wrong-key-wrong-key", tautulli.ErrUnauthorized, 401},
		{"api disabled", (*tautullitest.Server).APIDisabled, tautullitest.Key, tautulli.ErrAPIDisabled, 404},
		{"2.17 refused", (*tautullitest.Server).Version217, tautullitest.Key, tautulli.ErrTooOld, 400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := tautullitest.NewServer(t)
			if tt.setup != nil {
				tt.setup(s)
			}
			c := newClient(t, s, tt.key, tautulli.Options{})
			_, err := c.Info(context.Background())
			if !errors.Is(err, tt.want) {
				t.Fatalf("Info error = %v, want %v", err, tt.want)
			}
			var he *httpread.Error
			if !errors.As(err, &he) || he.StatusCode != tt.code || he.Path != "/api/v2" {
				t.Fatalf("error = %#v", err)
			}
			msg := err.Error()
			for _, leak := range []string{s.URL, "cmd=", "Invalid apikey", "API not enabled", tautullitest.Key} {
				if strings.Contains(msg, leak) {
					t.Errorf("error %q contains %q", msg, leak)
				}
			}
		})
	}
}

func TestVersionGateOnReportedVersion(t *testing.T) {
	s := tautullitest.NewServer(t)
	s.Set(readtest.Key("GET", "/api/v2", "cmd=get_tautulli_info"), readtest.Route{
		Body: []byte(`{"response":{"result":"success","message":null,"data":{"tautulli_version":"v2.17.9"}}}`)})
	c := newClient(t, s, tautullitest.Key, tautulli.Options{})
	if _, err := c.Info(context.Background()); !errors.Is(err, tautulli.ErrTooOld) {
		t.Fatalf("Info error = %v, want ErrTooOld", err)
	}
}

func TestLibraryAndUsers(t *testing.T) {
	s := tautullitest.NewServer(t)
	c := newClient(t, s, tautullitest.Key, tautulli.Options{})
	ctx := context.Background()
	for _, sec := range []string{"1", "2", "3"} {
		lib, err := c.Library(ctx, sec)
		if err != nil || !lib.Known || !lib.KeepHistory {
			t.Fatalf("Library(%s) = %+v, %v", sec, lib, err)
		}
	}
	users, err := c.Users(ctx)
	if err != nil || len(users) != 1 || users[0].UserID != 0 || !users[0].KeepHistory {
		t.Fatalf("Users = %+v, %v", users, err)
	}
	s.HistoryOff()
	lib, err := c.Library(ctx, "2")
	if err != nil || !lib.Known || lib.KeepHistory {
		t.Fatalf("Library(2) with history off = %+v, %v", lib, err)
	}
	users, err = c.Users(ctx)
	if err != nil || users[0].KeepHistory {
		t.Fatalf("Users with history off = %+v, %v", users, err)
	}
	if _, err := c.Library(ctx, "1&cmd=x"); err == nil {
		t.Fatal("an invalid section id was accepted")
	}
	checkRequests(t, s, tautullitest.Key)
}

func collect(t *testing.T, h *tautulli.HistoryReader, section string) ([]tautulli.Play, error) {
	t.Helper()
	var out []tautulli.Play
	err := h.Section(context.Background(), section, func(p tautulli.Play) error {
		out = append(out, p)
		return nil
	})
	return out, err
}

func TestHistory(t *testing.T) {
	s := tautullitest.NewServer(t)
	c := newClient(t, s, tautullitest.Key, tautulli.Options{})
	h := c.NewHistoryReader()
	movies, err := collect(t, h, "1")
	if err != nil || len(movies) != 5 {
		t.Fatalf("section 1: %d rows, %v", len(movies), err)
	}
	first := movies[0]
	if first.RowID != 1 || first.RatingKey != "4" || first.GUID != "plex://movie/5d7768278718ba001e311d5d" ||
		!first.Stopped.Equal(time.Unix(1790411120, 0)) {
		t.Fatalf("first play = %+v", first)
	}
	tv, err := collect(t, h, "2")
	if err != nil || len(tv) != 4 {
		t.Fatalf("section 2: %d rows, %v", len(tv), err)
	}
	music, err := collect(t, h, "3")
	if err != nil || len(music) != 4 {
		t.Fatalf("section 3: %d rows, %v", len(music), err)
	}
	// A section Tautulli does not know answers an empty history.
	none, err := collect(t, h, "99")
	if err != nil || len(none) != 0 {
		t.Fatalf("section 99: %d rows, %v", len(none), err)
	}
	if h.Rows != 13 || h.Requests != 4 {
		t.Fatalf("rows %d, requests %d", h.Rows, h.Requests)
	}
	checkRequests(t, s, tautullitest.Key)
}

func TestHistoryPaging(t *testing.T) {
	row := func(id int64, key string) map[string]any {
		return map[string]any{"row_id": id, "rating_key": key, "guid": "plex://movie/x" + key, "stopped": 1790411120 + id}
	}
	tests := []struct {
		name  string
		setup func(*tautullitest.Server)
		want  error
		rows  int
	}{
		{name: "recorded pages of 2", rows: 5},
		{name: "shrinking total", setup: func(s *tautullitest.Server) {
			s.SetHistory("1", 2, 2, 4, []map[string]any{row(3, "9"), row(4, "2")})
		}, want: tautulli.ErrPaging},
		{name: "short page before the total", setup: func(s *tautullitest.Server) {
			s.SetHistory("1", 2, 2, 5, []map[string]any{row(3, "9")})
		}, want: tautulli.ErrPaging},
		{name: "empty page before the total", setup: func(s *tautullitest.Server) {
			s.SetHistory("1", 2, 2, 5, nil)
		}, want: tautulli.ErrPaging},
		{name: "growing total is read to its end", setup: func(s *tautullitest.Server) {
			s.SetHistory("1", 4, 2, 6, []map[string]any{row(5, "3"), row(6, "3")})
			s.SetHistory("1", 6, 2, 6, nil)
		}, rows: 6},
		{name: "duplicate rows across pages are counted once", setup: func(s *tautullitest.Server) {
			s.SetHistory("1", 2, 2, 5, []map[string]any{row(2, "4"), row(3, "9")})
		}, rows: 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := tautullitest.NewServer(t)
			if tt.setup != nil {
				tt.setup(s)
			}
			c := newClient(t, s, tautullitest.Key, tautulli.Options{PageSize: 2})
			rows, err := collect(t, c.NewHistoryReader(), "1")
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
			if tt.want == nil && len(rows) != tt.rows {
				t.Fatalf("%d rows, want %d", len(rows), tt.rows)
			}
		})
	}
}

func TestHistoryRowBudget(t *testing.T) {
	s := tautullitest.NewServer(t)
	c := newClient(t, s, tautullitest.Key, tautulli.Options{MaxRows: 4})
	if _, err := collect(t, c.NewHistoryReader(), "1"); !errors.Is(err, tautulli.ErrTooManyRows) {
		t.Fatalf("error = %v, want ErrTooManyRows", err)
	}
}

func TestDecodingQuirks(t *testing.T) {
	s := tautullitest.NewServer(t)
	// HTML-escaped strings and numbers as strings, as Tautulli sends them in places.
	s.SetHistory("1", 0, 1000, 1, []map[string]any{{"row_id": "7", "rating_key": "42", "guid": "local://42?a=1&amp;b=2", "stopped": "1790411297"}})
	c := newClient(t, s, tautullitest.Key, tautulli.Options{})
	rows, err := collect(t, c.NewHistoryReader(), "1")
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows %v, %v", rows, err)
	}
	if rows[0].RowID != 7 || rows[0].RatingKey != "42" || rows[0].GUID != "local://42?a=1&b=2" || rows[0].Stopped.Unix() != 1790411297 {
		t.Fatalf("row = %+v", rows[0])
	}
}

func TestRedirectAndBodyCap(t *testing.T) {
	s := tautullitest.NewServer(t)
	s.Set(readtest.Key("GET", "/api/v2", "cmd=get_tautulli_info"), readtest.Route{Status: http.StatusFound, Body: nil})
	c := newClient(t, s, tautullitest.Key, tautulli.Options{})
	if _, err := c.Info(context.Background()); !errors.Is(err, httpread.ErrRedirected) {
		t.Fatalf("redirect: %v", err)
	}
	s2 := tautullitest.NewServer(t)
	c2 := newClient(t, s2, tautullitest.Key, tautulli.Options{Options: httpread.Options{MaxResponseBytes: 64}})
	if _, err := c2.Info(context.Background()); !errors.Is(err, httpread.ErrTooLarge) {
		t.Fatalf("cap: %v", err)
	}
}

func TestOutboundGuard(t *testing.T) {
	c, err := tautulli.New("http://169.254.169.254", tautullitest.Key, tautulli.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.Info(ctx); !errors.Is(err, netguard.ErrBlocked) {
		t.Fatalf("error = %v, want netguard.ErrBlocked", err)
	}
}
