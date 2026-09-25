package notify

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/logging"
)

func TestEventMapping(t *testing.T) {
	ctx := context.Background()
	s, _, _ := openStore(t)
	f := newFakeApprise(t, fakeOpts{})
	// Stateful targets: the request path names the target that was notified.
	for _, tc := range []struct {
		key                          string
		enabled, fail, warn, success bool
	}{
		{"failures", true, true, false, false},
		{"warnings", true, true, true, false},
		{"everything", true, true, true, true},
		{"successes", true, false, false, true},
		{"disabled", false, true, true, true},
	} {
		if _, err := s.Create(ctx, Input{Name: tc.key, APIURL: f.URL(), ConfigKey: tc.key,
			Enabled: new(tc.enabled), OnFailure: new(tc.fail), OnWarning: new(tc.warn), OnSuccess: new(tc.success)}); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		status   jobs.Status
		dryRun   bool
		wantType string
		want     []string // notified targets
	}{
		{jobs.StatusFailed, false, "failure", []string{"everything", "failures", "warnings"}},
		{jobs.StatusFailed, true, "failure", []string{"everything", "failures", "warnings"}},
		{jobs.StatusCompletedWithWarnings, false, "warning", []string{"everything", "warnings"}},
		{jobs.StatusCompletedWithWarnings, true, "", nil},
		{jobs.StatusCompleted, false, "success", []string{"everything", "successes"}},
		{jobs.StatusCompleted, true, "", nil},
		{jobs.StatusCancelled, false, "", nil},
		{jobs.StatusCancelled, true, "", nil},
		{jobs.StatusRunning, false, "", nil},
		{jobs.StatusQueued, false, "", nil},
	}
	for _, tc := range cases {
		name := string(tc.status)
		if tc.dryRun {
			name += "/dry-run"
		}
		t.Run(name, func(t *testing.T) {
			f.reset()
			d := New(s, nil, Options{Client: testClient()})
			d.Handle(jobs.Job{ID: 7, Type: jobs.TypeSync, Status: tc.status, DryRun: tc.dryRun})
			if err := d.Close(ctx); err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, r := range f.requests() {
				got = append(got, strings.TrimPrefix(r.Path, "/notify/"))
				if r.Body["type"] != tc.wantType {
					t.Errorf("type = %v, want %s", r.Body["type"], tc.wantType)
				}
			}
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("notified %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMessageFormat(t *testing.T) {
	const secret = "msgfmt-secret-token-31337"
	logging.RegisterSecret(secret)
	start := time.Date(2026, 9, 25, 3, 0, 0, 0, time.UTC)
	end := start.Add(65*time.Second + 400*time.Millisecond)
	short := start.Add(250 * time.Millisecond)
	describe := func(_ context.Context, job jobs.Job) string {
		switch job.ID {
		case 42:
			return "Sync to UNAS"
		case 43:
			return "  Sync\nto\t" + secret + "  "
		}
		return ""
	}
	d := New(nil, nil, Options{Describe: describe, BaseURL: "http://tower:8484/"})
	defer d.Close(context.Background())

	cases := []struct {
		name      string
		job       jobs.Job
		wantTitle string
		wantBody  string
		wantType  MessageType
	}{
		{
			name: "failed with everything",
			job: jobs.Job{ID: 42, Type: jobs.TypeSync, Status: jobs.StatusFailed, Summary: "Copied 3 of 10 files",
				Error: "destination not mounted? token " + secret, Warnings: 3, StartedAt: &start, FinishedAt: &end},
			wantTitle: "Bunkarr: Sync to UNAS failed",
			wantBody: "Copied 3 of 10 files\nError: destination not mounted? token [REDACTED]\nWarnings: 3\n" +
				"Duration: 1m5s\nJob: #42\nDetails: http://tower:8484/activity/jobs/42",
			wantType: TypeFailure,
		},
		{
			name:      "failed dry run",
			job:       jobs.Job{ID: 42, Type: jobs.TypeSync, Status: jobs.StatusFailed, DryRun: true, Error: "boom"},
			wantTitle: "Bunkarr: Sync to UNAS (dry run) failed",
			wantBody:  "Error: boom\nJob: #42\nDetails: http://tower:8484/activity/jobs/42",
			wantType:  TypeFailure,
		},
		{
			name:      "describe output is one redacted line",
			job:       jobs.Job{ID: 43, Type: jobs.TypeSync, Status: jobs.StatusCompletedWithWarnings, Warnings: 12, Summary: "12 changes held by the mass-change guard"},
			wantTitle: "Bunkarr: Sync to [REDACTED] completed with warnings",
			wantBody:  "12 changes held by the mass-change guard\nWarnings: 12\nJob: #43\nDetails: http://tower:8484/activity/jobs/43",
			wantType:  TypeWarning,
		},
		{
			name:      "fallback description, sub-second duration",
			job:       jobs.Job{ID: 44, Type: jobs.TypePlexDBBackup, Status: jobs.StatusCompleted, Summary: "Backed up 2 files", StartedAt: &start, FinishedAt: &short},
			wantTitle: "Bunkarr: Plex database backup completed",
			wantBody:  "Backed up 2 files\nDuration: 250ms\nJob: #44\nDetails: http://tower:8484/activity/jobs/44",
			wantType:  TypeSuccess,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, ok := eventFor(tc.job)
			if !ok {
				t.Fatal("no event")
			}
			m := d.message(tc.job, ev)
			if m.Title != tc.wantTitle || m.Body != tc.wantBody || m.Type != tc.wantType {
				t.Fatalf("message =\n%q\n%q\n%s\nwant\n%q\n%q\n%s", m.Title, m.Body, m.Type, tc.wantTitle, tc.wantBody, tc.wantType)
			}
		})
	}

	// Without a BaseURL there is no link; long texts are bounded.
	d2 := New(nil, nil, Options{})
	defer d2.Close(context.Background())
	m := d2.message(jobs.Job{ID: 1, Type: jobs.TypeVerify, Status: jobs.StatusFailed, Error: strings.Repeat("x", 5000)}, event{TypeFailure, "failed"})
	if m.Title != "Bunkarr: Verify failed" || strings.Contains(m.Body, "Details:") || len(m.Body) > maxTextLen+100 {
		t.Fatalf("message = %q / %d bytes", m.Title, len(m.Body))
	}
}

func TestHandleDoesNotBlock(t *testing.T) {
	ctx := context.Background()
	s, _, _ := openStore(t)
	release := make(chan struct{})
	f := newFakeApprise(t, fakeOpts{block: release})
	if _, err := s.Create(ctx, Input{Name: "slow", APIURL: f.URL(), ConfigKey: "slow"}); err != nil {
		t.Fatal(err)
	}
	log, logs := testLogger()
	d := New(s, log, Options{Client: testClient(), Workers: 1, QueueSize: 2})

	const n = 20
	start := time.Now()
	for i := range n {
		d.Handle(jobs.Job{ID: int64(i + 1), Type: jobs.TypeSync, Status: jobs.StatusFailed})
	}
	if el := time.Since(start); el > 500*time.Millisecond {
		t.Fatalf("%d Handle calls took %s while Apprise hangs", n, el)
	}
	dropped := strings.Count(logs.String(), "Notification dropped: too many notifications waiting")
	// One job may already be with the worker, two wait in the queue; the rest are dropped.
	if dropped < n-3 || dropped > n-2 {
		t.Fatalf("dropped = %d, want %d or %d\n%s", dropped, n-3, n-2, logs.String())
	}
	close(release)
	if err := d.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := len(f.requests()); got != n-dropped {
		t.Fatalf("sent %d, want %d", got, n-dropped)
	}
	d.Handle(jobs.Job{ID: 99, Type: jobs.TypeSync, Status: jobs.StatusFailed}) // after Close: dropped, no panic
	if !strings.Contains(logs.String(), "shutting down") {
		t.Fatal("Handle after Close did not log the drop")
	}
}

func TestCloseCancelsHangingSends(t *testing.T) {
	ctx := context.Background()
	s, _, _ := openStore(t)
	f := newFakeApprise(t, fakeOpts{block: make(chan struct{})})
	if _, err := s.Create(ctx, Input{Name: "hang", APIURL: f.URL(), ConfigKey: "hang"}); err != nil {
		t.Fatal(err)
	}
	d := New(s, nil, Options{Client: testClient()})
	d.Handle(jobs.Job{ID: 1, Type: jobs.TypeSync, Status: jobs.StatusFailed})
	cctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := d.Close(cctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close = %v, want DeadlineExceeded", err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("Close took %s", el)
	}
}

func TestDispatcherNeverLogsURLs(t *testing.T) {
	ctx := context.Background()
	s, _, _ := openStore(t)
	const (
		pw    = "logpw-0f0f0f"
		token = "logtoken-a5a5a5"
		urls  = "mailto://me:" + pw + "@smtp.example.com, tgram://42:" + token + "/99"
	)
	failing := newFakeApprise(t, fakeOpts{statuses: []int{500, 500, 424}, respBody: urls})
	ok := newFakeApprise(t, fakeOpts{})
	for name, api := range map[string]string{"failing": failing.URL(), "ok": ok.URL()} {
		if _, err := s.Create(ctx, Input{Name: name, APIURL: api, URLs: urls, OnSuccess: new(true)}); err != nil {
			t.Fatal(err)
		}
	}
	log, logs := testLogger()
	d := New(s, log, Options{Client: testClient(), Describe: func(context.Context, jobs.Job) string { return "Sync " + urls }})
	d.Handle(jobs.Job{ID: 5, Type: jobs.TypeSync, Status: jobs.StatusFailed, Error: "failed near " + urls})
	d.Handle(jobs.Job{ID: 6, Type: jobs.TypeSync, Status: jobs.StatusCompleted})
	if err := d.Close(ctx); err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	if !strings.Contains(out, "Notification failed") || !strings.Contains(out, "Notification sent") {
		t.Fatalf("expected both a failure and a success log line:\n%s", out)
	}
	for _, secret := range []string{pw, token, "smtp.example.com", "mailto://"} {
		if strings.Contains(out, secret) {
			t.Fatalf("log contains %q:\n%s", secret, out)
		}
	}
	// The message bodies carry the redacted error and description, never the URLs.
	for _, r := range ok.requests() {
		if strings.Contains(r.Body["title"].(string), pw) || strings.Contains(r.Body["body"].(string), pw) {
			t.Fatalf("message contains the URLs: %v", r.Body)
		}
	}
}

func TestDescribePanicIsContained(t *testing.T) {
	ctx := context.Background()
	s, _, _ := openStore(t)
	f := newFakeApprise(t, fakeOpts{})
	if _, err := s.Create(ctx, Input{Name: "t", APIURL: f.URL(), ConfigKey: "t"}); err != nil {
		t.Fatal(err)
	}
	log, logs := testLogger()
	d := New(s, log, Options{Client: testClient(), Workers: 1, Describe: func(_ context.Context, job jobs.Job) string {
		if job.ID == 1 {
			panic("describe bug")
		}
		return "Scan of Movies"
	}})
	d.Handle(jobs.Job{ID: 1, Type: jobs.TypeScan, Status: jobs.StatusFailed})
	d.Handle(jobs.Job{ID: 2, Type: jobs.TypeScan, Status: jobs.StatusFailed})
	if err := d.Close(ctx); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	if len(reqs) != 1 || reqs[0].Body["title"] != "Bunkarr: Scan of Movies failed" {
		t.Fatalf("requests = %+v", reqs)
	}
	if !strings.Contains(logs.String(), "describe bug") {
		t.Fatalf("panic not logged:\n%s", logs.String())
	}
}

func TestTestButton(t *testing.T) {
	ctx := context.Background()
	s, _, _ := openStore(t)
	f := newFakeApprise(t, fakeOpts{})
	const urls = "json://testbutton.example/testbutton-token-8c8c"
	saved, err := s.Create(ctx, Input{Name: "saved", APIURL: f.URL(), URLs: urls, Enabled: new(false)})
	if err != nil {
		t.Fatal(err)
	}
	d := New(s, nil, Options{Client: testClient(), BaseURL: "http://tower:8484"})
	defer d.Close(ctx)

	check := func(t *testing.T, path, wantURLs string) {
		t.Helper()
		reqs := f.requests()
		if len(reqs) != 1 {
			t.Fatalf("requests = %d, want 1", len(reqs))
		}
		r := reqs[0]
		if r.Path != path || r.Body["type"] != "info" || r.Body["title"] != "Bunkarr: test notification" ||
			!strings.Contains(r.Body["body"].(string), "http://tower:8484/settings/connect") {
			t.Fatalf("request = %+v", r)
		}
		if got, _ := r.Body["urls"].(string); got != wantURLs {
			t.Fatalf("urls = %q, want %q", got, wantURLs)
		}
	}

	t.Run("saved (disabled) target", func(t *testing.T) {
		f.reset()
		if err := d.Test(ctx, saved.ID, nil); err != nil {
			t.Fatal(err)
		}
		check(t, "/notify", urls)
	})
	t.Run("unsaved stateful", func(t *testing.T) {
		f.reset()
		if err := d.Test(ctx, 0, &Input{APIURL: f.URL() + "/", ConfigKey: "draft"}); err != nil {
			t.Fatal(err)
		}
		check(t, "/notify/draft", "")
	})
	t.Run("edit form keeps stored URLs", func(t *testing.T) {
		f.reset()
		if err := d.Test(ctx, saved.ID, &Input{Name: "renamed", APIURL: f.URL()}); err != nil {
			t.Fatal(err)
		}
		check(t, "/notify", urls)
	})
	t.Run("edit form with new URLs", func(t *testing.T) {
		f.reset()
		const other = "json://testbutton-other.example/x"
		if err := d.Test(ctx, saved.ID, &Input{APIURL: f.URL(), URLs: other}); err != nil {
			t.Fatal(err)
		}
		check(t, "/notify", other)
	})

	var verr ValidationError
	for name, err := range map[string]error{
		"nothing":        d.Test(ctx, 0, nil),
		"no urls":        d.Test(ctx, 0, &Input{APIURL: f.URL()}),
		"bad apiUrl":     d.Test(ctx, 0, &Input{APIURL: "ftp://x", URLs: urls}),
		"urls and key":   d.Test(ctx, saved.ID, &Input{APIURL: f.URL(), URLs: urls, ConfigKey: "k"}),
		"bad config key": d.Test(ctx, 0, &Input{APIURL: f.URL(), ConfigKey: "a/b"}),
	} {
		if !errors.As(err, &verr) {
			t.Errorf("%s: err = %v, want ValidationError", name, err)
		}
	}
	if err := d.Test(ctx, 999, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id: err = %v", err)
	}
	if err := d.Test(ctx, 999, &Input{APIURL: f.URL(), ConfigKey: "k"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id with input: err = %v", err)
	}

	failing := newFakeApprise(t, fakeOpts{statuses: []int{424}})
	err = d.Test(ctx, saved.ID, &Input{APIURL: failing.URL(), URLs: urls})
	var serr *SendError
	if !errors.As(err, &serr) || serr.StatusCode != 424 || strings.Contains(err.Error(), "testbutton-token") {
		t.Fatalf("delivery failure: err = %v", err)
	}
}

// TestTestSendsStoredURLsOnlyToTheStoredAPI: the edit form's Test uses the stored URLs only with
// the stored API URL; another API URL needs the URLs entered again (S8).
func TestTestSendsStoredURLsOnlyToTheStoredAPI(t *testing.T) {
	ctx := context.Background()
	s, _, _ := openStore(t)
	f := newFakeApprise(t, fakeOpts{})
	evil := newFakeApprise(t, fakeOpts{})
	saved, err := s.Create(ctx, Input{Name: "saved", APIURL: f.URL(), URLs: "json://bound.example/bound-test-token-3f3f"})
	if err != nil {
		t.Fatal(err)
	}
	d := New(s, nil, Options{Client: testClient()})
	defer d.Close(ctx)
	err = d.Test(ctx, saved.ID, &Input{Name: "saved", APIURL: evil.URL()})
	var verr ValidationError
	if !errors.As(err, &verr) || !strings.Contains(err.Error(), "enter them again") {
		t.Fatalf("test of another API URL = %v, want a ValidationError asking for the URLs", err)
	}
	if reqs := evil.requests(); len(reqs) != 0 {
		t.Fatalf("the other API got %+v", reqs)
	}
}

// TestTestDoesNotRegisterUnsavedURLs: URLs typed into the Test form are not added to the
// process-wide secret registry (which request input could grow without bound).
func TestTestDoesNotRegisterUnsavedURLs(t *testing.T) {
	ctx := context.Background()
	s, _, _ := openStore(t)
	f := newFakeApprise(t, fakeOpts{})
	failing := newFakeApprise(t, fakeOpts{statuses: []int{400}})
	d := New(s, nil, Options{Client: testClient()})
	defer d.Close(ctx)
	const urls = "json://unsaved.example/unsaved-token-5a5a, discord://42/unsaved-discord-token-6b6b"
	if err := d.Test(ctx, 0, &Input{APIURL: f.URL(), URLs: urls}); err != nil {
		t.Fatal(err)
	}
	var serr *SendError
	if err := d.Test(ctx, 0, &Input{APIURL: failing.URL(), URLs: urls}); !errors.As(err, &serr) || strings.Contains(err.Error(), "unsaved") {
		t.Fatalf("failed test: err = %v", err)
	}
	for _, v := range []string{urls, "json://unsaved.example/unsaved-token-5a5a", "discord://42/unsaved-discord-token-6b6b"} {
		if logging.ContainsSecret(v) {
			t.Errorf("%q was registered", v)
		}
	}
}

func TestUndecryptableTargetDoesNotStopOthers(t *testing.T) {
	ctx := context.Background()
	s, d, _ := openStore(t)
	f := newFakeApprise(t, fakeOpts{})
	if _, err := s.Create(ctx, Input{Name: "sealed", APIURL: f.URL(), URLs: "json://undecryptable.example/undecryptable-token"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, Input{Name: "stateful", APIURL: f.URL(), ConfigKey: "still-sent"}); err != nil {
		t.Fatal(err)
	}
	// The same database opened with another bunkarr.key: the sealed URLs no longer open.
	log, logs := testLogger()
	disp := New(NewStore(d, testKeyring(t, 99)), log, Options{Client: testClient()})
	started := time.Now()
	finished := started.Add(time.Minute)
	job := jobs.Job{ID: 8, Type: jobs.TypeSync, Status: jobs.StatusFailed, StartedAt: &started, FinishedAt: &finished}
	disp.Handle(job)
	finished = started // the caller may reuse its memory after Handle returns
	if err := disp.Close(ctx); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	if len(reqs) != 1 || reqs[0].Path != "/notify/still-sent" || !strings.Contains(reqs[0].Body["body"].(string), "Duration: 1m0s") {
		t.Fatalf("requests = %+v", reqs)
	}
	if !strings.Contains(logs.String(), "bunkarr.key") || strings.Contains(logs.String(), "undecryptable-token") {
		t.Fatalf("log:\n%s", logs.String())
	}
}
