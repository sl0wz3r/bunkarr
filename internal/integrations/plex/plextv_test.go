package plex_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/integrations/plex"
	"github.com/sl0wz3r/bunkarr/internal/integrations/plex/plextest"
	"github.com/sl0wz3r/bunkarr/internal/version"
)

const tvClientID = "7c6f7f0e-4b8e-4a51-9d0e-2f1e3c4b5a69"

// tvRequest is one request the raw fake plex.tv received.
type tvRequest struct {
	Method string
	Path   string
	Query  url.Values
	RawQ   string
	Header http.Header
}

// rawTV is a plex.tv stand-in that answers every request with handler and records it.
type rawTV struct {
	URL  string
	mu   sync.Mutex
	reqs []tvRequest
}

func newRawTV(t *testing.T, handler http.HandlerFunc) *rawTV {
	t.Helper()
	f := &rawTV{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.reqs = append(f.reqs, tvRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), RawQ: r.URL.RawQuery, Header: r.Header.Clone()})
		f.mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	f.URL = srv.URL
	return f
}

func (f *rawTV) requests() []tvRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]tvRequest(nil), f.reqs...)
}

func (f *rawTV) opts() plex.Options {
	return plex.Options{ClientIdentifier: tvClientID, PlexTVURL: f.URL + "/", ClientsPlexTVURL: f.URL, Timeout: 5 * time.Second}
}

func writeBody(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func TestCreatePin(t *testing.T) {
	f := newRawTV(t, func(w http.ResponseWriter, r *http.Request) {
		writeBody(w, 201, `{"id":564964751,"code":"8lzjqnq8lye02n52jq3fqxf8e","product":"Bunkarr","trusted":false,
		  "clientIdentifier":"x","expiresIn":1800,"createdAt":"2026-09-22T12:00:00Z","location":{"code":"US"},
		  "expiresAt":"2026-09-22T12:30:00Z","authToken":null,"newRegistration":null}`)
	})
	pin, err := plex.CreatePin(context.Background(), f.opts())
	if err != nil {
		t.Fatal(err)
	}
	want := plex.Pin{ID: 564964751, Code: "8lzjqnq8lye02n52jq3fqxf8e", ExpiresAt: time.Date(2026, 9, 22, 12, 30, 0, 0, time.UTC)}
	if pin != want {
		t.Errorf("pin = %+v, want %+v", pin, want)
	}
	r := f.requests()[0]
	if r.Method != http.MethodPost || r.Path != "/api/v2/pins" || r.RawQ != "strong=true" {
		t.Errorf("request = %s %s?%s", r.Method, r.Path, r.RawQ)
	}
	if r.Header.Get("X-Plex-Token") != "" {
		t.Error("CreatePin must not send a token")
	}
}

// Every plex.tv request carries the full X-Plex header set.
func TestPlexTVHeaderSet(t *testing.T) {
	tv := plextest.NewPlexTV(t)
	tv.AddAccount(plextest.Account{Token: "account-token-abcdef", Username: "alice"})
	o := plex.Options{ClientIdentifier: tvClientID, Product: "Bunkarr", Device: "Docker", DeviceName: "Bunkarr NAS",
		PlexTVURL: tv.URL, ClientsPlexTVURL: tv.URL}
	ctx := context.Background()
	pin, err := plex.CreatePin(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plex.CheckPin(ctx, o, pin.ID, pin.Code); err != nil {
		t.Fatal(err)
	}
	if _, err := plex.User(ctx, o, "account-token-abcdef"); err != nil {
		t.Fatal(err)
	}
	if _, err := plex.Resources(ctx, o, "account-token-abcdef"); err != nil {
		t.Fatal(err)
	}
	reqs := tv.Requests()
	if len(reqs) != 4 {
		t.Fatalf("requests = %d, want 4", len(reqs))
	}
	for _, r := range reqs {
		for k, v := range map[string]string{
			"Accept": "application/json", "User-Agent": version.UserAgent(), "X-Plex-Product": "Bunkarr",
			"X-Plex-Version": version.Version, "X-Plex-Client-Identifier": tvClientID, "X-Plex-Device": "Docker",
			"X-Plex-Device-Name": "Bunkarr NAS",
		} {
			if got := r.Header.Get(k); got != v {
				t.Errorf("%s %s: %s = %q, want %q", r.Method, r.Path, k, got, v)
			}
		}
		if r.Header.Get("X-Plex-Platform") == "" {
			t.Errorf("%s %s: no X-Plex-Platform", r.Method, r.Path)
		}
		if strings.Contains(r.RawQuery, "account-token") {
			t.Errorf("%s %s: token in the query", r.Method, r.Path)
		}
		withToken := r.Path == plextest.PathUser || r.Path == plextest.PathResources
		if got := r.Header.Get("X-Plex-Token"); (got != "") != withToken {
			t.Errorf("%s %s: X-Plex-Token = %q", r.Method, r.Path, got)
		}
	}
}

func TestCreatePinExpiresInFallbackAndStringID(t *testing.T) {
	f := newRawTV(t, func(w http.ResponseWriter, r *http.Request) {
		writeBody(w, 201, `{"id":"42","code":"abcd","expiresIn":"900"}`)
	})
	before := time.Now()
	pin, err := plex.CreatePin(context.Background(), f.opts())
	if err != nil {
		t.Fatal(err)
	}
	if pin.ID != 42 || pin.Code != "abcd" {
		t.Errorf("pin = %+v", pin)
	}
	if pin.ExpiresAt.Before(before.Add(899*time.Second)) || pin.ExpiresAt.After(time.Now().Add(901*time.Second)) {
		t.Errorf("ExpiresAt = %v, want ~now+900s", pin.ExpiresAt)
	}
}

func TestCreatePinErrors(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
	}{
		{"no code", 201, `{"id":1}`},
		{"html", 200, `<html>maintenance</html>`},
		{"429", 429, `{"errors":[{"code":1,"message":"slow down"}]}`},
		{"empty", 201, ``},
		{"redirect", 302, ``},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newRawTV(t, func(w http.ResponseWriter, r *http.Request) {
				if tt.status == 302 {
					w.Header().Set("Location", "https://evil.example/")
				}
				writeBody(w, tt.status, tt.body)
			})
			_, err := plex.CreatePin(context.Background(), f.opts())
			var perr *plex.Error
			if !errors.As(err, &perr) || perr.Service != "plex.tv" || perr.Path != "/api/v2/pins" {
				t.Fatalf("err = %#v (%v), want a plex.tv *plex.Error", err, err)
			}
			if strings.Contains(err.Error(), "slow down") || strings.Contains(err.Error(), "maintenance") || strings.Contains(err.Error(), "evil") {
				t.Errorf("error echoes the response: %v", err)
			}
			if len(f.requests()) != 1 {
				t.Errorf("requests = %d, want 1 (no redirect followed)", len(f.requests()))
			}
		})
	}
}

func TestCheckPin(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		token string
	}{
		{"pending", `{"id":564964751,"code":"8lzjq","authToken":null}`, ""},
		{"claimed", `{"id":564964751,"code":"8lzjq","authToken":"user-token-xyz"}`, "user-token-xyz"},
		{"xml attributes", `<?xml version="1.0" encoding="UTF-8"?><pin id="564964751" code="8lzjq" authToken="user-token-xyz"/>`, "user-token-xyz"},
		{"xml legacy elements", `<pin><id type="integer">564964751</id><code>8lzjq</code><auth-token>user-token-xyz</auth-token></pin>`, "user-token-xyz"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRawTV(t, func(w http.ResponseWriter, r *http.Request) { writeBody(w, 200, tt.body) })
			pin, err := plex.CheckPin(context.Background(), f.opts(), 564964751, "8lzjq")
			if err != nil {
				t.Fatal(err)
			}
			if pin.ID != 564964751 || pin.Code != "8lzjq" || pin.AuthToken != tt.token {
				t.Errorf("pin = %+v", pin)
			}
			r := f.requests()[0]
			if r.Method != http.MethodGet || r.Path != "/api/v2/pins/564964751" {
				t.Errorf("request = %s %s", r.Method, r.Path)
			}
			if r.Query.Get("code") != "8lzjq" {
				t.Errorf("CheckPin must send the code: query %q", r.RawQ)
			}
			if r.Header.Get("X-Plex-Client-Identifier") != tvClientID {
				t.Error("CheckPin must send the same client identifier")
			}
		})
	}
}

func TestCheckPinErrors(t *testing.T) {
	f := newRawTV(t, func(w http.ResponseWriter, r *http.Request) {
		writeBody(w, 404, `{"errors":[{"code":1020,"message":"Code not found or expired"}]}`)
	})
	o := f.opts()
	_, err := plex.CheckPin(context.Background(), o, 564964751, "abcd")
	if !errors.Is(err, plex.ErrNotFound) {
		t.Errorf("expired pin: err = %v, want ErrNotFound", err)
	}
	if err != nil && strings.Contains(err.Error(), "564964751") {
		t.Errorf("the error reveals the PIN id: %v", err)
	}
	if _, err := plex.CheckPin(context.Background(), o, 0, "abcd"); !errors.Is(err, plex.ErrInvalidArgument) {
		t.Errorf("id 0: err = %v", err)
	}
	if _, err := plex.CheckPin(context.Background(), o, 1, " "); !errors.Is(err, plex.ErrInvalidArgument) {
		t.Errorf("no code: err = %v", err)
	}
	if n := len(f.requests()); n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}
	// An answer for another PIN is refused.
	g := newRawTV(t, func(w http.ResponseWriter, r *http.Request) {
		writeBody(w, 200, `{"id":7,"code":"abcd","authToken":"tok"}`)
	})
	if _, err := plex.CheckPin(context.Background(), g.opts(), 8, "abcd"); err == nil {
		t.Error("an answer for another PIN was accepted")
	}
}

func TestUserDecodesOnlyTheUsername(t *testing.T) {
	for _, tt := range []struct {
		name, body, want string
	}{
		{"json", `{"id":1,"uuid":"u","username":"alice","email":"alice@example.com","authToken":"account-token-1",
			"subscription":{"active":true,"plan":"lifetime"},"title":"Alice"}`, "alice"},
		{"xml attribute", `<user id="1" username="bob" email="bob@example.com" authToken="account-token-1"/>`, "bob"},
		{"xml element", `<user><username>carol</username><email>carol@example.com</email></user>`, "carol"},
		{"no username", `{"email":"dave@example.com"}`, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newRawTV(t, func(w http.ResponseWriter, r *http.Request) { writeBody(w, 200, tt.body) })
			acc, err := plex.User(context.Background(), f.opts(), "account-token-1")
			if err != nil {
				t.Fatal(err)
			}
			if acc != (plex.Account{Username: tt.want}) {
				t.Errorf("User = %+v, want only the username %q", acc, tt.want)
			}
			// plex.Account has one field: nothing else can be decoded.
			if n := reflect.TypeFor[plex.Account]().NumField(); n != 1 {
				t.Errorf("plex.Account has %d fields, want only Username", n)
			}
			r := f.requests()[0]
			if r.Path != "/api/v2/user" || r.Header.Get("X-Plex-Token") != "account-token-1" || r.RawQ != "" {
				t.Errorf("request = %s ?%s token=%q", r.Path, r.RawQ, r.Header.Get("X-Plex-Token"))
			}
		})
	}
	f := newRawTV(t, func(w http.ResponseWriter, r *http.Request) { writeBody(w, 401, `{"errors":[]}`) })
	if _, err := plex.User(context.Background(), f.opts(), "bad-token-123"); !errors.Is(err, plex.ErrUnauthorized) {
		t.Errorf("401: err = %v", err)
	}
	if _, err := plex.User(context.Background(), f.opts(), " "); !errors.Is(err, plex.ErrInvalidArgument) {
		t.Errorf("empty token: err = %v", err)
	}
	g := newRawTV(t, func(w http.ResponseWriter, r *http.Request) { writeBody(w, 200, `"hello"`) })
	if _, err := plex.User(context.Background(), g.opts(), "account-token-1"); err == nil {
		t.Error("a non-object answer was accepted")
	}
}

func TestAuthURL(t *testing.T) {
	got := plex.AuthURL(plex.Options{ClientIdentifier: "abc-123", Product: "Bunkarr"}, "8lzjqnq8lye02n52jq3fqxf8e")
	want := "https://app.plex.tv/auth#?clientID=abc-123&code=8lzjqnq8lye02n52jq3fqxf8e&context%5Bdevice%5D%5Bproduct%5D=Bunkarr"
	if got != want {
		t.Errorf("AuthURL =\n%s\nwant\n%s", got, want)
	}
	// Values are escaped inside the fragment; spaces as %20.
	got = plex.AuthURL(plex.Options{ClientIdentifier: "a&b=c", Product: "My App"}, " co de ")
	if !strings.Contains(got, "clientID=a%26b%3Dc") || !strings.Contains(got, "code=co%20de") ||
		!strings.HasSuffix(got, "context%5Bdevice%5D%5Bproduct%5D=My%20App") {
		t.Errorf("AuthURL escaping = %s", got)
	}
	u, err := url.Parse(got)
	if err != nil || u.Scheme != "https" || u.Host != "app.plex.tv" || u.Path != "/auth" {
		t.Errorf("AuthURL does not parse as https://app.plex.tv/auth: %v", err)
	}
	// The default product is Bunkarr; a test server never changes the auth page.
	if got := plex.AuthURL(plex.Options{PlexTVURL: "http://127.0.0.1:1"}, "c"); !strings.HasPrefix(got, plex.AuthAppURL+"#?") ||
		!strings.HasSuffix(got, "=Bunkarr") {
		t.Errorf("AuthURL defaults = %s", got)
	}
}

const resourcesJSON = `[
  { "name": "Tower", "product": "Plex Media Server", "productVersion": "1.43.4.10903-abc", "platform": "Linux",
    "provides": "server", "clientIdentifier": "0123456789abcdef", "owned": true, "accessToken": "srv-token-1",
    "httpsRequired": false, "presence": true,
    "connections": [
      { "protocol": "https", "address": "192.168.1.10", "port": 32400,
        "uri": "https://192-168-1-10.0123456789abcdef.plex.direct:32400", "local": true, "relay": false, "IPv6": false },
      { "protocol": "https", "address": "203.0.113.5", "port": "32400",
        "uri": "https://203-0-113-5.0123456789abcdef.plex.direct:32400", "local": "0", "relay": "1" } ] },
  { "name": "Friend's Server", "provides": "server,client", "clientIdentifier": "fedcba", "owned": "0",
    "accessToken": "srv-token-2", "productVersion": "1.40.0", "platform": "Windows", "connections": [] },
  { "name": "iPhone", "product": "Plex for iOS", "provides": "client,player", "clientIdentifier": "phone",
    "accessToken": "ignored" },
  { "name": "Plexamp", "provides": "player,pubsub-player", "clientIdentifier": "amp" }
]`

func TestResources(t *testing.T) {
	f := newRawTV(t, func(w http.ResponseWriter, r *http.Request) { writeBody(w, 200, resourcesJSON) })
	res, err := plex.Resources(context.Background(), f.opts(), "user-token")
	if err != nil {
		t.Fatal(err)
	}
	want := []plex.Resource{
		{Name: "Tower", ClientIdentifier: "0123456789abcdef", ProductVersion: "1.43.4.10903-abc", Platform: "Linux",
			Owned: true, AccessToken: "srv-token-1", Connections: []plex.Connection{
				{URI: "https://192-168-1-10.0123456789abcdef.plex.direct:32400", Address: "192.168.1.10", Port: 32400, Protocol: "https", Local: true},
				{URI: "https://203-0-113-5.0123456789abcdef.plex.direct:32400", Address: "203.0.113.5", Port: 32400, Protocol: "https", Relay: true},
			}},
		{Name: "Friend's Server", ClientIdentifier: "fedcba", ProductVersion: "1.40.0", Platform: "Windows",
			AccessToken: "srv-token-2", Connections: []plex.Connection{}},
	}
	if !reflect.DeepEqual(res, want) {
		t.Errorf("Resources =\n%+v\nwant\n%+v", res, want)
	}
	r := f.requests()[0]
	if r.Method != http.MethodGet || r.Path != "/api/v2/resources" {
		t.Errorf("request = %s %s", r.Method, r.Path)
	}
	for _, k := range []string{"includeHttps", "includeRelay", "includeIPv6"} {
		if r.Query.Get(k) != "1" {
			t.Errorf("query %s = %q, want 1", k, r.Query.Get(k))
		}
	}
	if r.Header.Get("X-Plex-Token") != "user-token" || strings.Contains(r.RawQ, "user-token") {
		t.Error("the plex.tv token must be sent as a header only")
	}
	// No access token in JSON.
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("srv-token")) || bytes.Contains(b, []byte("accessToken")) {
		t.Errorf("a Resource serializes its access token: %s", b)
	}
	pb, _ := json.Marshal(plex.Pin{ID: 564964751, Code: "secret-code", AuthToken: "account-token-1"})
	if string(pb) != "{}" {
		t.Errorf("a Pin serializes: %s", pb)
	}
}

func TestResourcesAlternativeShapes(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"wrapped MediaContainer", `{"MediaContainer":{"size":1,"Device":[{"name":"Tower","provides":"server","clientIdentifier":"abc","owned":1,"accessToken":"t",
			"Connection":[{"protocol":"http","address":"10.0.0.2","port":"32400","uri":"http://10.0.0.2:32400","local":"1"}]}]}}`},
		{"resources key", `{"resources":[{"name":"Tower","provides":"server","clientIdentifier":"abc","owned":true,"accessToken":"t",
			"connections":{"protocol":"http","address":"10.0.0.2","port":32400,"uri":"http://10.0.0.2:32400","local":true}}]}`},
		{"single object", `{"name":"Tower","provides":"server","clientIdentifier":"abc","owned":true,"accessToken":"t","device":"PC",
			"connections":[{"protocol":"http","address":"10.0.0.2","port":32400,"uri":"http://10.0.0.2:32400","local":true}]}`},
		{"xml v1", `<?xml version="1.0" encoding="UTF-8"?>
<MediaContainer size="2">
  <Device name="Tower" product="Plex Media Server" provides="server" clientIdentifier="abc" owned="1" accessToken="t">
    <Connection protocol="http" address="10.0.0.2" port="32400" uri="http://10.0.0.2:32400" local="1"/>
  </Device>
  <Device name="Phone" provides="client,player" clientIdentifier="p"/>
</MediaContainer>`},
		{"xml v2", `<resources><resource name="Tower" provides="server" clientIdentifier="abc" owned="true" accessToken="t">
<connections><connection protocol="http" address="10.0.0.2" port="32400" uri="http://10.0.0.2:32400" local="true" relay="false"/></connections>
</resource></resources>`},
	}
	want := []plex.Resource{{Name: "Tower", ClientIdentifier: "abc", Owned: true, AccessToken: "t", Connections: []plex.Connection{
		{URI: "http://10.0.0.2:32400", Address: "10.0.0.2", Port: 32400, Protocol: "http", Local: true}}}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRawTV(t, func(w http.ResponseWriter, r *http.Request) { writeBody(w, 200, tt.body) })
			res, err := plex.Resources(context.Background(), f.opts(), "user-token")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(res, want) {
				t.Errorf("Resources =\n%+v\nwant\n%+v", res, want)
			}
		})
	}
}

func TestResourcesErrors(t *testing.T) {
	if _, err := plex.Resources(context.Background(), plex.Options{}, "  "); !errors.Is(err, plex.ErrInvalidArgument) {
		t.Errorf("empty token: err = %v", err)
	}
	t.Run("401", func(t *testing.T) {
		f := newRawTV(t, func(w http.ResponseWriter, r *http.Request) {
			writeBody(w, 401, `{"errors":[{"code":1001,"message":"User could not be authenticated"}]}`)
		})
		_, err := plex.Resources(context.Background(), f.opts(), "bad")
		var perr *plex.Error
		if !errors.Is(err, plex.ErrUnauthorized) || !errors.As(err, &perr) || perr.Service != "clients.plex.tv" {
			t.Errorf("err = %v, want ErrUnauthorized from clients.plex.tv", err)
		}
	})
	t.Run("garbage never echoes the body", func(t *testing.T) {
		f := newRawTV(t, func(w http.ResponseWriter, r *http.Request) {
			writeBody(w, 200, `[{"name":"x","accessToken":"secret-server-token","connections":[{"port":{]`)
		})
		_, err := plex.Resources(context.Background(), f.opts(), "user-token")
		if err == nil || strings.Contains(err.Error(), "secret-server-token") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("unrecognized", func(t *testing.T) {
		f := newRawTV(t, func(w http.ResponseWriter, r *http.Request) { writeBody(w, 200, `"hello"`) })
		if _, err := plex.Resources(context.Background(), f.opts(), "user-token"); err == nil {
			t.Error("expected error")
		}
	})
	t.Run("empty list", func(t *testing.T) {
		f := newRawTV(t, func(w http.ResponseWriter, r *http.Request) { writeBody(w, 200, `[]`) })
		res, err := plex.Resources(context.Background(), f.opts(), "user-token")
		if err != nil || res == nil || len(res) != 0 {
			t.Errorf("Resources = %#v, %v; want empty non-nil", res, err)
		}
	})
	t.Run("oversized", func(t *testing.T) {
		f := newRawTV(t, func(w http.ResponseWriter, r *http.Request) {
			writeBody(w, 200, "["+strings.Repeat(" ", 8<<20)+"]")
		})
		if _, err := plex.Resources(context.Background(), f.opts(), "user-token"); err == nil || !strings.Contains(err.Error(), "larger than") {
			t.Errorf("err = %v, want a size error", err)
		}
	})
}

// plex.tv is reached over https only; http only for a loopback test server.
func TestPlexTVBaseURLValidation(t *testing.T) {
	ctx := context.Background()
	for _, base := range []string{"http://plex.tv", "http://192.168.1.2:8080", "ftp://plex.tv", "https://", "https://user:pw@plex.tv", "http://[fe80::1]:80"} {
		o := plex.Options{ClientIdentifier: "x", PlexTVURL: base, ClientsPlexTVURL: base}
		if _, err := plex.CreatePin(ctx, o); !errors.Is(err, plex.ErrInvalidArgument) {
			t.Errorf("CreatePin with %q: err = %v, want ErrInvalidArgument", base, err)
		}
		if _, err := plex.Resources(ctx, o, "tok-123456"); !errors.Is(err, plex.ErrInvalidArgument) {
			t.Errorf("Resources with %q: err = %v, want ErrInvalidArgument", base, err)
		}
	}
	for _, base := range []string{"http://127.0.0.1:1", "http://localhost:1", "http://[::1]:1"} {
		o := plex.Options{PlexTVURL: base, Timeout: time.Second}
		if _, err := plex.CreatePin(ctx, o); errors.Is(err, plex.ErrInvalidArgument) {
			t.Errorf("CreatePin with loopback %q refused as invalid: %v", base, err)
		}
	}
}

// A request that never answers ends with the timeout; errors and debug logs name the PIN path as
// a template, never with the PIN id or code.
func TestPlexTVTimeoutAndLogs(t *testing.T) {
	release := make(chan struct{})
	f := newRawTV(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/pins/5" {
			<-release
			return
		}
		writeBody(w, 200, `{"id":6,"code":"secret-pin-code","authToken":null}`)
	})
	defer close(release)
	var buf bytes.Buffer
	o := f.opts()
	o.Timeout = 100 * time.Millisecond
	o.Logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	_, err := plex.CheckPin(context.Background(), o, 5, "secret-pin-code")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if strings.Contains(err.Error(), "secret-pin-code") || strings.Contains(err.Error(), "/pins/5") {
		t.Errorf("the error reveals the PIN: %v", err)
	}
	if _, err := plex.CheckPin(context.Background(), o, 6, "secret-pin-code"); err != nil {
		t.Fatal(err)
	}
	logs := buf.String()
	if !strings.Contains(logs, "/api/v2/pins/{id}") || strings.Contains(logs, "secret-pin-code") || strings.Contains(logs, "pins/6") {
		t.Errorf("logs = %s", logs)
	}
}

// The fake plex.tv of plextest runs the whole PIN flow.
func TestPlexTVFlowAgainstFake(t *testing.T) {
	tv := plextest.NewPlexTV(t)
	tv.AddAccount(plextest.Account{Token: "account-token-abcdef", Username: "alice", Email: "alice@example.com",
		Servers: []plextest.Resource{{Name: "Tower", ClientIdentifier: "machine-1", Owned: true, AccessToken: "server-token-123456",
			Connections: []plextest.Connection{{URI: "http://10.0.0.2:32400", Address: "10.0.0.2", Port: 32400, Protocol: "http", Local: true}}}}})
	o := plex.Options{ClientIdentifier: tvClientID, PlexTVURL: tv.URL, ClientsPlexTVURL: tv.URL}
	ctx := context.Background()
	pin, err := plex.CreatePin(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := plex.CheckPin(ctx, o, pin.ID, pin.Code); err != nil || p.AuthToken != "" {
		t.Fatalf("pending CheckPin = %+v, %v", p, err)
	}
	// Another client identifier, or a wrong code, cannot read the PIN.
	other := o
	other.ClientIdentifier = "someone-else"
	if _, err := plex.CheckPin(ctx, other, pin.ID, pin.Code); !errors.Is(err, plex.ErrNotFound) {
		t.Errorf("CheckPin with another client id: err = %v", err)
	}
	if _, err := plex.CheckPin(ctx, o, pin.ID, "wrong-code"); !errors.Is(err, plex.ErrNotFound) {
		t.Errorf("CheckPin with a wrong code: err = %v", err)
	}
	tv.Approve(pin.Code, "account-token-abcdef")
	p, err := plex.CheckPin(ctx, o, pin.ID, pin.Code)
	if err != nil || p.AuthToken != "account-token-abcdef" {
		t.Fatalf("claimed CheckPin = %+v, %v", p, err)
	}
	acc, err := plex.User(ctx, o, p.AuthToken)
	if err != nil || acc.Username != "alice" {
		t.Fatalf("User = %+v, %v", acc, err)
	}
	res, err := plex.Resources(ctx, o, p.AuthToken)
	if err != nil || len(res) != 1 || res[0].AccessToken != "server-token-123456" || res[0].ClientIdentifier != "machine-1" {
		t.Fatalf("Resources = %+v, %v", res, err)
	}
	tv.Expire(pin.Code)
	if _, err := plex.CheckPin(ctx, o, pin.ID, pin.Code); !errors.Is(err, plex.ErrNotFound) {
		t.Errorf("expired CheckPin: err = %v", err)
	}
}
