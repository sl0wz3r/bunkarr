package httpread

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestInt(t *testing.T) {
	for _, tt := range []struct {
		in    string
		want  Int
		error bool
	}{
		{`4`, Int{4, true}, false},
		{`"45"`, Int{45, true}, false},
		{`""`, Int{}, false},
		{`null`, Int{}, false},
		{`2.0`, Int{2, true}, false},
		{`2.5`, Int{}, true},
		{`"x"`, Int{}, true},
		{`true`, Int{}, true},
	} {
		var v struct {
			N Int `json:"n"`
		}
		err := json.Unmarshal([]byte(`{"n":`+tt.in+`}`), &v)
		if (err != nil) != tt.error || (!tt.error && v.N != tt.want) {
			t.Errorf("%s: %+v, %v", tt.in, v.N, err)
		}
		if err != nil {
			_ = err.Error() // the error must render (no nil type inside)
		}
	}
}

func TestText(t *testing.T) {
	for in, want := range map[string]string{
		`"Rags &amp; &lt;Riches&gt;"`: "Rags & <Riches>",
		`" x "`:                       "x",
		`12`:                          "12",
		`null`:                        "",
	} {
		var v Text
		if err := json.Unmarshal([]byte(in), &v); err != nil || v.String() != want {
			t.Errorf("%s: %q, %v", in, v, err)
		}
	}
	var v Text
	if err := json.Unmarshal([]byte(`{}`), &v); err == nil {
		t.Error("an object decoded as text")
	}
}

func TestParseTimeAndVersion(t *testing.T) {
	want := time.Date(2026, 9, 26, 8, 28, 58, 0, time.UTC)
	for _, s := range []string{"2026-09-26T08:28:58.000Z", "2026-09-26 08:28:58", "2026-09-26T08:28:58Z"} {
		if got, ok := ParseTime(s); !ok || !got.Equal(want) {
			t.Errorf("%s: %v %v", s, got, ok)
		}
	}
	if _, ok := ParseTime("yesterday"); ok {
		t.Error("parsed a word")
	}
	for s, want := range map[string]Version{"v2.18.1": {2, 18, 1}, "3.4": {3, 4, 0}, "3.29.0-beta.1": {3, 29, 0}} {
		if got, ok := ParseVersion(s); !ok || got != want {
			t.Errorf("%s: %v %v", s, got, ok)
		}
	}
	if _, ok := ParseVersion("latest"); ok {
		t.Error("parsed latest")
	}
	if !(Version{2, 17, 9}).Less(Version{2, 18, 0}) || (Version{3, 4, 0}).Less(Version{3, 4, 0}) {
		t.Error("Less")
	}
}

func TestGetKeyOnlyInHeaderAndNoRedirect(t *testing.T) {
	var (
		mu   sync.Mutex
		last *http.Request
	)
	got := func() *http.Request {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		last = r.Clone(context.Background())
		mu.Unlock()
		if r.URL.Path == "/base/redirect" {
			http.Redirect(w, r, "http://127.0.0.1:1/elsewhere", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c, err := New("App", srv.URL+"/base/", "the-key", Options{HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Get(context.Background(), Request{Path: "/x", WithKey: true})
	if err != nil || resp.StatusCode != 200 || got().URL.Path != "/base/x" || got().Header.Get("X-Api-Key") != "the-key" ||
		strings.Contains(got().URL.RawQuery, "the-key") || !strings.HasPrefix(got().Header.Get("User-Agent"), "Bunkarr/") {
		t.Fatalf("%+v %v %v", resp, err, got().URL)
	}
	if _, err := c.Get(context.Background(), Request{Path: "/x"}); err != nil || got().Header.Get("X-Api-Key") != "" {
		t.Fatal("a request without WithKey sent the key")
	}
	_, err = c.Get(context.Background(), Request{Path: "/redirect"})
	var he *Error
	if !errors.Is(err, ErrRedirected) || !errors.As(err, &he) || he.StatusCode != http.StatusFound || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("redirect: %v", err)
	}
	for _, bad := range []string{"", "ftp://x", "http://u:p@x", "http://x/?q=1", "http://x/#f"} {
		if _, err := New("App", bad, "", Options{}); err == nil || strings.Contains(err.Error(), "u:p") {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if _, err := New("App", "http://x", "a\nb", Options{}); err == nil {
		t.Error("a key with a newline was accepted")
	}
}
