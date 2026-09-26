package seerr_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/integrations/httpread"
	"github.com/sl0wz3r/bunkarr/internal/integrations/seerr"
	"github.com/sl0wz3r/bunkarr/internal/integrations/seerr/seerrtest"
)

func newClient(t *testing.T, s *seerrtest.Server, key string, o seerr.Options) *seerr.Client {
	t.Helper()
	o.HTTPClient = s.Client()
	c, err := seerr.New(s.URL, key, o)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func checkRequests(t *testing.T, s *seerrtest.Server) {
	t.Helper()
	allowed := []string{seerr.PathStatus, seerr.PathMe, seerr.PathRequest, seerr.PathUser}
	for _, r := range s.Requests() {
		if r.Method != http.MethodGet || !slices.Contains(allowed, r.Path) {
			t.Errorf("request %s %s outside the allow-list", r.Method, r.Path)
		}
		key := r.Header.Get("X-Api-Key")
		switch {
		case r.Path == seerr.PathStatus && key != "":
			t.Errorf("status was sent with the key")
		case r.Path != seerr.PathStatus && key == "":
			t.Errorf("%s was sent without the key", r.Path)
		}
		if strings.Contains(r.RawQuery, seerrtest.Key) {
			t.Errorf("the key is in the query %q", r.RawQuery)
		}
	}
}

func TestStatusAndMe(t *testing.T) {
	s := seerrtest.NewServer(t)
	c := newClient(t, s, seerrtest.Key, seerr.Options{})
	st, err := c.Status(context.Background())
	if err != nil || st.Version != "3.4.1" {
		t.Fatalf("Status = %+v, %v", st, err)
	}
	id, err := c.Me(context.Background())
	if err != nil || id != 1 {
		t.Fatalf("Me = %d, %v", id, err)
	}
	checkRequests(t, s)
}

func TestKeyErrors(t *testing.T) {
	for _, tt := range []struct {
		key  string
		code int
	}{{"", 401}, {"bad-key", 403}} {
		s := seerrtest.NewServer(t)
		c := newClient(t, s, tt.key, seerr.Options{})
		_, err := c.Me(context.Background())
		var he *httpread.Error
		if !errors.Is(err, seerr.ErrUnauthorized) || !errors.As(err, &he) || he.StatusCode != tt.code {
			t.Fatalf("key %q: %v", tt.key, err)
		}
		for _, leak := range []string{"permission", "connect.sid", s.URL} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("error %q contains %q", err, leak)
			}
		}
	}
}

func readAll(t *testing.T, c *seerr.Client) ([]seerr.Request, error) {
	t.Helper()
	var out []seerr.Request
	_, err := c.Requests(context.Background(), func(r seerr.Request) error {
		out = append(out, r)
		return nil
	})
	return out, err
}

func TestRequests(t *testing.T) {
	for _, size := range []int{100, 4} {
		t.Run(fmt.Sprintf("take %d", size), func(t *testing.T) {
			s := seerrtest.NewServer(t)
			c := newClient(t, s, seerrtest.Key, seerr.Options{PageSize: size})
			reqs, err := readAll(t, c)
			if err != nil || len(reqs) != 10 {
				t.Fatalf("%d requests, %v", len(reqs), err)
			}
			byID := map[int64]seerr.Request{}
			for _, r := range reqs {
				byID[r.ID] = r
			}
			if r := byID[6]; r.Type != "tv" || r.Status != 2 || !slices.Equal(r.Seasons, []int{1}) || r.TMDBID != 6357 || r.TVDBID != 73587 ||
				r.RatingKey != "28" || r.UserID != 2 || r.CreatedAt == nil {
				t.Errorf("request 6 = %+v", r)
			}
			if r := byID[4]; !r.Is4K || r.Status != seerr.StatusFailed || r.RatingKey != "2" || r.TMDBID != 19 {
				t.Errorf("request 4 = %+v", r)
			}
			if r := byID[9]; len(r.Seasons) != 7 || r.TVDBID != 73614 {
				t.Errorf("request 9 = %+v", r)
			}
			if r := byID[1]; r.Status != seerr.StatusPending || r.RatingKey != "" || len(r.Seasons) != 0 {
				t.Errorf("request 1 = %+v", r)
			}
			// No e-mail address or name is decoded (SEC4).
			if b, _ := json.Marshal(reqs); strings.Contains(string(b), "@") || strings.Contains(string(b), "fixture-") {
				t.Errorf("decoded requests hold a name or an e-mail: %s", b)
			}
			checkRequests(t, s)
		})
	}
}

func TestCounted(t *testing.T) {
	for status, want := range map[int]bool{1: true, 2: true, 3: false, 4: true, 5: true, 0: false, 6: false} {
		if seerr.Counted(status) != want {
			t.Errorf("Counted(%d) = %v", status, !want)
		}
	}
}

func TestRequestPaging(t *testing.T) {
	row := func(id int) map[string]any {
		return map[string]any{"id": id, "status": 1, "type": "movie", "media": map[string]any{"tmdbId": 100 + id}}
	}
	tests := []struct {
		name  string
		setup func(*seerrtest.Server)
		want  error
	}{
		{name: "page 3 fails", setup: func(s *seerrtest.Server) { s.SetStatus(seerrtest.RequestKey(4, 8), 500) }, want: errors.New("status 500")},
		{name: "shrinking total", setup: func(s *seerrtest.Server) {
			s.SetPage(4, 4, 9, []map[string]any{row(5), row(6), row(7), row(8)})
		}, want: seerr.ErrPaging},
		{name: "short page", setup: func(s *seerrtest.Server) {
			s.SetPage(4, 4, 10, []map[string]any{row(5), row(6)})
		}, want: seerr.ErrPaging},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := seerrtest.NewServer(t)
			tt.setup(s)
			c := newClient(t, s, seerrtest.Key, seerr.Options{PageSize: 4})
			_, err := readAll(t, c)
			if err == nil {
				t.Fatal("no error")
			}
			if errors.Is(tt.want, seerr.ErrPaging) && !errors.Is(err, seerr.ErrPaging) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	s := seerrtest.NewServer(t)
	c := newClient(t, s, seerrtest.Key, seerr.Options{PageSize: 4, MaxRequests: 5})
	if _, err := readAll(t, c); !errors.Is(err, seerr.ErrTooMany) {
		t.Fatalf("budget: %v", err)
	}
}

func TestUsers(t *testing.T) {
	for _, size := range []int{100, 2} {
		s := seerrtest.NewServer(t)
		c := newClient(t, s, seerrtest.Key, seerr.Options{PageSize: size})
		users, err := c.Users(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		want := []seerr.User{{ID: 1, Label: "fixture-admin"}, {ID: 2, Label: "fixture-local"}, {ID: 3, Label: "Seerr user #3"}}
		if !slices.Equal(users, want) {
			t.Fatalf("take %d: users = %+v", size, users)
		}
		checkRequests(t, s)
	}
}
