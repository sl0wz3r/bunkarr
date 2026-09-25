package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSendStatelessPayload(t *testing.T) {
	f := newFakeApprise(t, fakeOpts{})
	const urls = "json://stateless-payload.example/hook?x=1&y=2, discord://1/stateless-payload-token"
	target := Target{Name: "t", APIURL: f.URL() + "/", URLs: urls}
	msg := Message{Title: "Bunkarr: Sync failed", Body: "line 1\nline <2> & 3", Type: TypeFailure}
	if err := testClient().Send(context.Background(), target, msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	reqs := f.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(reqs))
	}
	r := reqs[0]
	if r.Method != http.MethodPost || r.Path != "/notify" || r.ContentType != "application/json" || !strings.HasPrefix(r.UserAgent, "Bunkarr/") {
		t.Fatalf("request = %+v", r)
	}
	want := map[string]any{"urls": urls, "title": msg.Title, "body": msg.Body, "type": "failure"}
	if !reflect.DeepEqual(r.Body, want) {
		t.Fatalf("body = %v\nwant   %v", r.Body, want)
	}
}

func TestSendStatefulPayload(t *testing.T) {
	f := newFakeApprise(t, fakeOpts{})
	target := Target{APIURL: f.URL() + "/apprise", ConfigKey: "bunkarr_main-1", URLs: "json://ignored-in-stateful-mode.example"}
	msg := Message{Title: "T", Body: "B", Type: TypeSuccess}
	if err := testClient().Send(context.Background(), target, msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	reqs := f.requests()
	if len(reqs) != 1 || reqs[0].Path != "/apprise/notify/bunkarr_main-1" {
		t.Fatalf("requests = %+v", reqs)
	}
	want := map[string]any{"title": "T", "body": "B", "type": "success"}
	if !reflect.DeepEqual(reqs[0].Body, want) {
		t.Fatalf("body = %v, want %v (no urls in stateful mode)", reqs[0].Body, want)
	}
}

func TestSendStatusHandling(t *testing.T) {
	cases := []struct {
		name     string
		statuses []int
		wantErr  string // "" = success
		wantCode int
		wantReqs int
	}{
		{"ok", []int{200}, "", 0, 1},
		{"retry after 500", []int{500, 200}, "", 0, 2},
		{"5xx twice", []int{502, 503}, "server error (503 Service Unavailable) (after one retry)", 503, 2},
		{"retry then 424", []int{500, 424}, "(after one retry)", 424, 2},
		{"424 is not retried", []int{424}, "at least one service could not be notified", 424, 1},
		{"400 is not retried", []int{400}, "request rejected", 400, 1},
		{"404", []int{404}, "check apiUrl", 404, 1},
		{"204 means nothing sent", []int{204}, "nothing was sent", 204, 1},
		{"401", []int{401}, "unexpected response (401 Unauthorized)", 401, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeApprise(t, fakeOpts{statuses: tc.statuses})
			err := testClient().Send(context.Background(), Target{APIURL: f.URL(), ConfigKey: "k"}, Message{Body: "b", Type: TypeInfo})
			if got := len(f.requests()); got != tc.wantReqs {
				t.Fatalf("requests = %d, want %d", got, tc.wantReqs)
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Send: %v", err)
				}
				return
			}
			var serr *SendError
			if !errors.As(err, &serr) || serr.StatusCode != tc.wantCode || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v (%T), want *SendError %d containing %q", err, err, tc.wantCode, tc.wantErr)
			}
			if !strings.HasPrefix(err.Error(), "apprise "+f.URL()+": ") {
				t.Fatalf("err = %q, want it to name the API", err)
			}
		})
	}
}

func TestSendDoesNotFollowRedirects(t *testing.T) {
	elsewhere := newFakeApprise(t, fakeOpts{})
	f := newFakeApprise(t, fakeOpts{statuses: []int{307}, location: elsewhere.URL() + "/notify"})
	err := testClient().Send(context.Background(), Target{APIURL: f.URL(), URLs: "json://redirect-test.example/secret-path"}, Message{Body: "b", Type: TypeInfo})
	var serr *SendError
	if !errors.As(err, &serr) || serr.StatusCode != 307 || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("err = %v, want a redirect error", err)
	}
	if n := len(elsewhere.requests()); n != 0 {
		t.Fatalf("redirect target got %d requests; the URLs must only go to the configured API", n)
	}
}

// resetListener accepts connections and closes them at once, counting them.
func resetListener(t *testing.T) (addr string, accepts *atomic.Int64) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepts = new(atomic.Int64)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepts.Add(1)
			_ = c.Close()
		}
	}()
	return ln.Addr().String(), accepts
}

func TestSendNetworkErrorIsRetriedOnce(t *testing.T) {
	addr, accepts := resetListener(t)
	err := testClient().Send(context.Background(), Target{APIURL: "http://" + addr, ConfigKey: "k"}, Message{Body: "b", Type: TypeInfo})
	var serr *SendError
	if !errors.As(err, &serr) || serr.StatusCode != 0 || !strings.Contains(err.Error(), "cannot reach the Apprise API") || !strings.Contains(err.Error(), "after one retry") {
		t.Fatalf("err = %v", err)
	}
	if n := accepts.Load(); n != 2 {
		t.Fatalf("connections = %d, want 2 (one retry)", n)
	}
}

func TestSendTimeout(t *testing.T) {
	f := newFakeApprise(t, fakeOpts{block: make(chan struct{})})
	c := testClient()
	c.timeout = 100 * time.Millisecond
	start := time.Now()
	err := c.Send(context.Background(), Target{APIURL: f.URL(), ConfigKey: "k"}, Message{Body: "b", Type: TypeInfo})
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "no response within 100ms") {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if n := len(f.requests()); n != 2 {
		t.Fatalf("requests = %d, want 2 (a timeout is retried once)", n)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("Send took %s", d)
	}
	if DefaultTimeout != 15*time.Second || NewClient().timeout != DefaultTimeout {
		t.Fatal("default timeout is not 15s")
	}
}

func TestSendCancelledIsNotRetried(t *testing.T) {
	f := newFakeApprise(t, fakeOpts{block: make(chan struct{})})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	err := testClient().Send(ctx, Target{APIURL: f.URL(), ConfigKey: "k"}, Message{Body: "b", Type: TypeInfo})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if n := len(f.requests()); n != 1 {
		t.Fatalf("requests = %d, want 1", n)
	}
}

func TestSendErrorsNeverContainURLs(t *testing.T) {
	// The password and token below are parts of the URLs; only whole URLs are registered as
	// secrets, so these checks prove the errors never quote the URLs or bodies at all.
	const (
		password = "errpw-7c1d93"
		token    = "errtoken-55aa77"
		urls     = "mailto://bunkarr:" + password + "@smtp.example.com?to=me@example.com, tgram://123:" + token + "/456"
	)
	echo := "One or more notifications failed: " + urls + " " + password + " " + token
	addr, _ := resetListener(t)
	block := newFakeApprise(t, fakeOpts{block: make(chan struct{})})
	cases := map[string]struct {
		apiURL string
		fake   *fakeApprise
		setup  func(*Client)
	}{
		"424 with echoing body": {fake: newFakeApprise(t, fakeOpts{statuses: []int{424}, respBody: echo})},
		"500 twice":             {fake: newFakeApprise(t, fakeOpts{statuses: []int{500, 500}, respBody: echo})},
		"400":                   {fake: newFakeApprise(t, fakeOpts{statuses: []int{400}, respBody: echo})},
		"network error":         {apiURL: "http://" + addr},
		"timeout":               {fake: block, setup: func(c *Client) { c.timeout = 50 * time.Millisecond }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			apiURL := tc.apiURL
			if tc.fake != nil {
				apiURL = tc.fake.URL()
			}
			c := testClient()
			if tc.setup != nil {
				tc.setup(c)
			}
			err := c.Send(context.Background(), Target{Name: "n", APIURL: apiURL, URLs: urls}, Message{Title: "t", Body: "b", Type: TypeFailure})
			if err == nil {
				t.Fatal("Send succeeded")
			}
			msgs := []string{err.Error()}
			for e := errors.Unwrap(err); e != nil; e = errors.Unwrap(e) {
				msgs = append(msgs, e.Error())
			}
			for _, m := range msgs {
				for _, secret := range []string{password, token, "mailto://", "tgram://", "smtp.example.com", "/notify"} {
					if strings.Contains(m, secret) {
						t.Fatalf("error %q contains %q", m, secret)
					}
				}
			}
		})
	}
}

func TestSendRejectsBadTargetsAndMessages(t *testing.T) {
	f := newFakeApprise(t, fakeOpts{})
	ok := Message{Body: "b", Type: TypeInfo}
	cases := []struct {
		name   string
		target Target
		msg    Message
	}{
		{"credentials in apiUrl", Target{APIURL: "http://user:send-reject-pw@" + strings.TrimPrefix(f.URL(), "http://"), ConfigKey: "k"}, ok},
		{"query in apiUrl", Target{APIURL: f.URL() + "?token=abc", ConfigKey: "k"}, ok},
		{"no urls in stateless mode", Target{APIURL: f.URL()}, ok},
		{"bad config key", Target{APIURL: f.URL(), ConfigKey: "../get/key"}, ok},
		{"bad type", Target{APIURL: f.URL(), ConfigKey: "k"}, Message{Body: "b", Type: "error"}},
		{"empty body", Target{APIURL: f.URL(), ConfigKey: "k"}, Message{Title: "t", Body: " ", Type: TypeInfo}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := testClient().Send(context.Background(), tc.target, tc.msg)
			var verr ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("err = %v, want ValidationError", err)
			}
			if strings.Contains(err.Error(), "send-reject-pw") {
				t.Fatalf("error echoes the password: %v", err)
			}
		})
	}
	if n := len(f.requests()); n != 0 {
		t.Fatalf("requests = %d, want 0", n)
	}
}

func TestTargetNeverPrintsURLs(t *testing.T) {
	const secret = "json://target-print.example/target-print-token"
	tg := Target{ID: 3, Name: "n", APIURL: "http://apprise:8000", URLs: secret}
	js, err := json.Marshal(tg)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{tg.String(), tg.GoString(), tg.LogValue().String(), fmt.Sprintf("%v %+v %#v %s", tg, tg, tg, tg), string(js)} {
		if strings.Contains(s, "target-print") {
			t.Fatalf("Target printed its URLs: %s", s)
		}
	}
}
