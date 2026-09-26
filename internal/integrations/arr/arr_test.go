package arr_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/integrations/arr"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr/arrtest"
	"github.com/sl0wz3r/bunkarr/internal/netguard"
	"github.com/sl0wz3r/bunkarr/internal/version"
)

const testKey = "arr-test-key-0123456789abcdef"

func newClient(t *testing.T, kind arr.Kind, base string, opts arr.Options) *arr.Client {
	t.Helper()
	c, err := arr.New(kind, base, testKey, opts)
	if err != nil {
		t.Fatalf("arr.New: %v", err)
	}
	return c
}

// call is one allowed request and how to send it.
type call struct {
	route string // "GET movie", "GET moviefile?movieId=1": relative to the API prefix
	do    func(context.Context, *arr.Client) error
}

func commonCalls(cmdID int64) []call {
	return []call{
		{"GET system/status", func(ctx context.Context, c *arr.Client) error { _, err := c.Status(ctx); return err }},
		{"GET qualityprofile", func(ctx context.Context, c *arr.Client) error { _, err := c.QualityProfiles(ctx); return err }},
		{"GET rootfolder", func(ctx context.Context, c *arr.Client) error { _, err := c.RootFolders(ctx); return err }},
		{"GET tag", func(ctx context.Context, c *arr.Client) error { _, err := c.Tags(ctx); return err }},
		{"GET config/mediamanagement", func(ctx context.Context, c *arr.Client) error { _, err := c.MediaManagement(ctx); return err }},
		{"GET system/backup", func(ctx context.Context, c *arr.Client) error { _, err := c.Backups(ctx); return err }},
		{"POST command", func(ctx context.Context, c *arr.Client) error { _, err := c.StartBackup(ctx); return err }},
		{"GET command/" + itoa(cmdID), func(ctx context.Context, c *arr.Client) error { _, err := c.Command(ctx, cmdID); return err }},
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// recordingTB captures Errorf calls instead of failing the test.
type recordingTB struct {
	testing.TB
	mu   sync.Mutex
	errs []string
}

func (r *recordingTB) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

func (r *recordingTB) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.errs)
}

var kindCalls = map[arr.Kind][]call{
	arr.KindRadarr: {
		{"GET movie", func(ctx context.Context, c *arr.Client) error { _, err := c.Movies(ctx); return err }},
		{"GET movie", func(ctx context.Context, c *arr.Client) error {
			return c.EachMovie(ctx, func(arr.Movie) error { return nil })
		}},
		{"GET movie/1", func(ctx context.Context, c *arr.Client) error { _, err := c.Movie(ctx, 1); return err }},
		{"GET moviefile?movieId=1", func(ctx context.Context, c *arr.Client) error { _, err := c.MovieFiles(ctx, 1); return err }},
	},
	arr.KindSonarr: {
		{"GET series", func(ctx context.Context, c *arr.Client) error { _, err := c.AllSeries(ctx); return err }},
		{"GET series", func(ctx context.Context, c *arr.Client) error {
			return c.EachSeries(ctx, func(arr.Series) error { return nil })
		}},
		{"GET series/2", func(ctx context.Context, c *arr.Client) error { _, err := c.Series(ctx, 2); return err }},
		{"GET episodefile?seriesId=1", func(ctx context.Context, c *arr.Client) error { _, err := c.EpisodeFiles(ctx, 1); return err }},
		{"GET episode?seriesId=2", func(ctx context.Context, c *arr.Client) error { _, err := c.Episodes(ctx, 2); return err }},
	},
	arr.KindLidarr: {
		{"GET artist", func(ctx context.Context, c *arr.Client) error { _, err := c.Artists(ctx); return err }},
		{"GET artist", func(ctx context.Context, c *arr.Client) error {
			return c.EachArtist(ctx, func(arr.Artist) error { return nil })
		}},
		{"GET artist/1", func(ctx context.Context, c *arr.Client) error { _, err := c.Artist(ctx, 1); return err }},
		{"GET trackfile?artistId=1", func(ctx context.Context, c *arr.Client) error { _, err := c.TrackFiles(ctx, 1); return err }},
		{"GET album?artistId=1", func(ctx context.Context, c *arr.Client) error { _, err := c.Albums(ctx, 1); return err }},
		{"GET metadataprofile", func(ctx context.Context, c *arr.Client) error { _, err := c.MetadataProfiles(ctx); return err }},
	},
}

var commandIDs = map[arr.Kind]int64{arr.KindRadarr: 21, arr.KindSonarr: 31, arr.KindLidarr: 40}

// TestEveryAllowedRequest sends every request of the allow-list to each application's fake and
// checks what went over the wire: the route, the headers, and never a key in a URL.
func TestEveryAllowedRequest(t *testing.T) {
	for _, kind := range []arr.Kind{arr.KindSonarr, arr.KindRadarr, arr.KindLidarr} {
		t.Run(string(kind), func(t *testing.T) {
			srv := arrtest.NewServer(t, kind, testKey)
			c := newClient(t, kind, srv.URL, arr.Options{})
			ctx := context.Background()
			calls := append(commonCalls(commandIDs[kind]), kindCalls[kind]...)
			routes := srv.Routes()
			for _, cl := range calls {
				srv.ResetRequests()
				if err := cl.do(ctx, c); err != nil {
					t.Fatalf("%s: %v", cl.route, err)
				}
				reqs := srv.Requests()
				if len(reqs) != 1 {
					t.Fatalf("%s sent %d requests, want 1", cl.route, len(reqs))
				}
				r := reqs[0]
				method, target, _ := strings.Cut(cl.route, " ")
				wantKey := srv.API(method, target)
				gotKey := r.Method + " " + r.Path
				if r.RawQuery != "" {
					gotKey += "?" + r.RawQuery
				}
				if gotKey != wantKey {
					t.Errorf("%s went to %q, want %q", cl.route, gotKey, wantKey)
				}
				if !slices.Contains(routes, wantKey) {
					t.Errorf("%s is not in the fake's route table", wantKey)
				}
				if r.Header.Get("X-Api-Key") != testKey || r.Header.Get("Accept") != "application/json" ||
					r.Header.Get("User-Agent") != version.UserAgent() {
					t.Errorf("%s headers = %v", cl.route, r.Header)
				}
				if strings.Contains(r.RawQuery, testKey) || strings.Contains(r.Path, testKey) {
					t.Errorf("%s put the key in the URL", cl.route)
				}
				if method == http.MethodPost {
					if string(r.Body) != `{"name":"Backup"}` || r.Header.Get("Content-Type") != "application/json" {
						t.Errorf("POST command body %q, type %q", r.Body, r.Header.Get("Content-Type"))
					}
				} else if len(r.Body) != 0 {
					t.Errorf("%s sent a body", cl.route)
				}
			}
		})
	}
}

// TestRequestsOfAnotherAppAreRefused checks that a client cannot send another application's
// requests: nothing reaches the server.
func TestRequestsOfAnotherAppAreRefused(t *testing.T) {
	for _, kind := range []arr.Kind{arr.KindSonarr, arr.KindRadarr, arr.KindLidarr} {
		srv := arrtest.NewServer(t, kind, testKey)
		c := newClient(t, kind, srv.URL, arr.Options{})
		for other, calls := range kindCalls {
			if other == kind {
				continue
			}
			for _, cl := range calls {
				err := cl.do(context.Background(), c)
				if !errors.Is(err, arr.ErrWrongKind) {
					t.Errorf("%s client, %s: err = %v, want ErrWrongKind", kind, cl.route, err)
				}
			}
		}
		if n := len(srv.Requests()); n != 0 {
			t.Errorf("%s: %d requests reached the server", kind, n)
		}
	}
}

func TestStatusChecksTheApplication(t *testing.T) {
	ctx := context.Background()
	sonarr := arrtest.NewServer(t, arr.KindSonarr, testKey)

	st, err := newClient(t, arr.KindSonarr, sonarr.URL, arr.Options{}).Status(ctx)
	if err != nil || st.AppName != "Sonarr" || st.Version != "4.0.20.3014" {
		t.Fatalf("Status = %+v, %v", st, err)
	}

	// A Sonarr URL saved as Radarr: same /api/v3, wrong appName.
	st, err = newClient(t, arr.KindRadarr, sonarr.URL, arr.Options{}).Status(ctx)
	var ae *arr.Error
	if !errors.Is(err, arr.ErrWrongApp) || !errors.As(err, &ae) || ae.StatusCode != 200 || st.AppName != "Sonarr" ||
		!strings.Contains(err.Error(), "the server is Sonarr, not Radarr") {
		t.Fatalf("Radarr client against Sonarr: %+v, %v", st, err)
	}

	// A Sonarr URL saved as Lidarr: no /api/v1.
	_, err = newClient(t, arr.KindLidarr, sonarr.URL, arr.Options{}).Status(ctx)
	if !errors.Is(err, arr.ErrWrongApp) || !errors.As(err, &ae) || ae.StatusCode != 404 {
		t.Fatalf("Lidarr client against Sonarr: %v", err)
	}

	// An appName that is not a plain name is not quoted.
	radarr := arrtest.NewServer(t, arr.KindRadarr, testKey)
	radarr.SetJSON(http.MethodGet, "system/status", []byte(`{"appName":"<script>x</script>","version":"1"}`))
	_, err = newClient(t, arr.KindRadarr, radarr.URL, arr.Options{}).Status(ctx)
	if !errors.Is(err, arr.ErrWrongApp) || strings.Contains(err.Error(), "script") || !strings.Contains(err.Error(), "another application") {
		t.Fatalf("odd appName: %v", err)
	}
}

func TestStatusCodes(t *testing.T) {
	ctx := context.Background()
	srv := arrtest.NewServer(t, arr.KindRadarr, testKey)

	wrong, err := arr.New(arr.KindRadarr, srv.URL, "not-the-key", arr.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrong.Tags(ctx); !errors.Is(err, arr.ErrUnauthorized) {
		t.Fatalf("wrong key: %v, want ErrUnauthorized", err)
	}
	c := newClient(t, arr.KindRadarr, srv.URL, arr.Options{})
	var ae *arr.Error
	if _, err := c.Movie(ctx, 99); !errors.Is(err, arr.ErrNotFound) || !errors.As(err, &ae) || ae.StatusCode != 404 ||
		ae.Path != "/api/v3/movie/99" || ae.Method != "GET" {
		t.Fatalf("unknown movie: %#v", err)
	}
	srv.SetStatus(http.MethodGet, "tag", http.StatusInternalServerError)
	if _, err := c.Tags(ctx); !errors.As(err, &ae) || ae.StatusCode != 500 || !strings.Contains(err.Error(), "unexpected status 500") {
		t.Fatalf("500: %v", err)
	}
	srv.SetJSON(http.MethodGet, "tag", []byte(`<html>login</html>`))
	if _, err := c.Tags(ctx); !errors.Is(err, arr.ErrNotArr) {
		t.Fatalf("HTML: %v, want ErrNotArr", err)
	}
	srv.SetJSON(http.MethodGet, "tag", []byte(`{"id":1}`))
	if _, err := c.Tags(ctx); !errors.Is(err, arr.ErrNotArr) {
		t.Fatalf("object instead of list: %v, want ErrNotArr", err)
	}
	srv.SetJSON(http.MethodGet, "tag", []byte(`[] []`))
	if _, err := c.Tags(ctx); !errors.Is(err, arr.ErrNotArr) {
		t.Fatalf("trailing data: %v, want ErrNotArr", err)
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	srv := arrtest.NewServer(t, arr.KindRadarr, testKey)
	srv.Redirect(http.MethodGet, "movie")
	c := newClient(t, arr.KindRadarr, srv.URL, arr.Options{})
	_, err := c.Movies(context.Background())
	var ae *arr.Error
	if !errors.Is(err, arr.ErrNotArr) || !errors.As(err, &ae) || ae.StatusCode != http.StatusFound {
		t.Fatalf("Movies after a redirect: %v", err)
	}
	if reqs := srv.Requests(); len(reqs) != 1 || reqs[0].Path != "/api/v3/movie" {
		t.Fatalf("requests = %+v, want only GET /api/v3/movie (the redirect not followed)", reqs)
	}
}

// TestErrorsHoldNoSecretOrBody checks that no error text carries the key, the URL, a query or a
// response body, whatever failed.
func TestErrorsHoldNoSecretOrBody(t *testing.T) {
	ctx := context.Background()
	srv := arrtest.NewServer(t, arr.KindRadarr, testKey)
	c := newClient(t, arr.KindRadarr, srv.URL, arr.Options{Timeout: 200 * time.Millisecond})
	host := strings.TrimPrefix(srv.URL, "http://")
	check := func(what string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: no error", what)
		}
		msg := err.Error()
		for _, bad := range []string{testKey, host, "secret-body", "movieId=", "?"} {
			if strings.Contains(msg, bad) {
				t.Errorf("%s: error %q contains %q", what, msg, bad)
			}
		}
	}
	srv.SetJSON(http.MethodGet, "tag", []byte(`["secret-body `+testKey+`"`))
	_, err := c.Tags(ctx)
	check("truncated JSON", err)
	srv.SetJSON(http.MethodGet, "tag", []byte(`[{"id":"secret-body `+testKey+`"}]`))
	_, err = c.Tags(ctx)
	check("wrong type", err)
	srv.SetStatus(http.MethodGet, "moviefile?movieId=7", http.StatusBadGateway)
	_, err = c.MovieFiles(ctx, 7)
	check("502", err)
	srv.Delay(http.MethodGet, "movie/1", 2*time.Second)
	_, err = c.Movie(ctx, 1)
	check("timeout", err)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("timeout: %v, want DeadlineExceeded", err)
	}
	srv.Close()
	_, err = c.Tags(ctx)
	check("connection refused", err)

	// A key that shows up in a cause (it never should) is redacted.
	c2, err := arr.New(arr.KindRadarr, "http://127.0.0.1:1", "dial", arr.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c2.Tags(ctx); err == nil || strings.Contains(strings.ReplaceAll(err.Error(), "[redacted]", ""), "dial") {
		t.Errorf("a key inside the cause was not redacted: %v", err)
	}
}

func TestBodyCaps(t *testing.T) {
	ctx := context.Background()
	srv := arrtest.NewServer(t, arr.KindRadarr, testKey)
	c := newClient(t, arr.KindRadarr, srv.URL, arr.Options{MaxResponseBytes: 64 << 10, MaxListBytes: 256 << 10})

	srv.Oversize(http.MethodGet, "tag", 128<<10)
	if _, err := c.Tags(ctx); !errors.Is(err, arr.ErrTooLarge) {
		t.Fatalf("oversized tag list: %v, want ErrTooLarge", err)
	}
	srv.Oversize(http.MethodGet, "tag", 64<<10)
	if _, err := c.Tags(ctx); errors.Is(err, arr.ErrTooLarge) {
		t.Fatalf("a body at the cap was refused: %v", err)
	}
	// The full lists have their own, larger cap.
	srv.Oversize(http.MethodGet, "movie", 200<<10)
	if err := c.EachMovie(ctx, func(arr.Movie) error { return nil }); errors.Is(err, arr.ErrTooLarge) {
		t.Fatalf("the list cap was not used: %v", err)
	}
	srv.Oversize(http.MethodGet, "movie", 512<<10)
	if _, err := c.Movies(ctx); !errors.Is(err, arr.ErrTooLarge) {
		t.Fatalf("oversized movie list: %v, want ErrTooLarge", err)
	}
	// The defaults.
	if arr.MaxResponseBytes != 32<<20 || arr.MaxListBytes != 256<<20 || arr.MaxBackupBytes != 8<<30 {
		t.Fatal("the default caps changed")
	}
}

func TestTimeouts(t *testing.T) {
	ctx := context.Background()
	srv := arrtest.NewServer(t, arr.KindRadarr, testKey)
	c := newClient(t, arr.KindRadarr, srv.URL, arr.Options{Timeout: 100 * time.Millisecond, ListTimeout: 2 * time.Second})
	srv.Delay(http.MethodGet, "movie", 300*time.Millisecond)
	if _, err := c.Movies(ctx); err != nil {
		t.Fatalf("a list slower than Timeout but within ListTimeout failed: %v", err)
	}
	srv.Delay(http.MethodGet, "tag", 300*time.Millisecond)
	if _, err := c.Tags(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("slow tag: %v, want DeadlineExceeded", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.Tags(cctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}

func TestEachStopsAtTheCallbackError(t *testing.T) {
	srv := arrtest.NewServer(t, arr.KindRadarr, testKey)
	c := newClient(t, arr.KindRadarr, srv.URL, arr.Options{})
	stop := errors.New("stop")
	var seen []int64
	err := c.EachMovie(context.Background(), func(m arr.Movie) error {
		seen = append(seen, m.ID)
		if len(seen) == 2 {
			return stop
		}
		return nil
	})
	if err != stop || !slices.Equal(seen, []int64{1, 2}) {
		t.Fatalf("EachMovie = %v after %v", err, seen)
	}
}

func TestURLBase(t *testing.T) {
	ctx := context.Background()
	srv := arrtest.NewServer(t, arr.KindRadarr, testKey, arrtest.WithURLBase("/radarr"))
	c := newClient(t, arr.KindRadarr, srv.URL+"/", arr.Options{})
	if _, err := c.Status(ctx); err != nil {
		t.Fatalf("Status: %v", err)
	}
	backups, err := c.Backups(ctx)
	if err != nil || len(backups) != 1 {
		t.Fatalf("Backups = %v, %v", backups, err)
	}
	var buf bytes.Buffer
	if _, err := c.DownloadBackup(ctx, backups[0], &buf); err != nil {
		t.Fatalf("DownloadBackup: %v", err)
	}
	var paths []string
	for _, r := range srv.Requests() {
		paths = append(paths, r.Path)
	}
	want := []string{"/radarr/api/v3/system/status", "/radarr/api/v3/system/backup", "/radarr/backup/manual/" + backups[0].Name}
	if !slices.Equal(paths, want) {
		t.Fatalf("paths = %q, want %q", paths, want)
	}
}

// TestOutboundGuard checks that the default transport refuses the cloud metadata address before
// anything is sent (S16).
func TestOutboundGuard(t *testing.T) {
	c := newClient(t, arr.KindRadarr, "http://169.254.169.254", arr.Options{Timeout: 2 * time.Second})
	_, err := c.Status(context.Background())
	if !errors.Is(err, netguard.ErrBlocked) {
		t.Fatalf("Status against the metadata address: %v, want netguard.ErrBlocked", err)
	}
}

func TestNewValidatesItsInput(t *testing.T) {
	for _, tt := range []struct {
		kind arr.Kind
		url  string
		key  string
	}{
		{"plex", "http://h", "k"},
		{arr.KindRadarr, "", "k"},
		{arr.KindRadarr, "ftp://h", "k"},
		{arr.KindRadarr, "http://h/?apikey=k", "k"},
		{arr.KindRadarr, "http://user:pw@h", "k"},
		{arr.KindRadarr, "http:///x", "k"},
		{arr.KindRadarr, "http://h", "k\r\nX-Evil: 1"},
	} {
		if _, err := arr.New(tt.kind, tt.url, tt.key, arr.Options{}); err == nil {
			t.Errorf("New(%q, %q) accepted", tt.kind, tt.url)
		} else if strings.Contains(err.Error(), "pw") || strings.Contains(err.Error(), "apikey=k") {
			t.Errorf("error repeats the URL: %v", err)
		}
	}
	for _, k := range []arr.Kind{arr.KindSonarr, arr.KindRadarr, arr.KindLidarr} {
		c, err := arr.New(k, " https://h:8989/base/ ", "", arr.Options{})
		if err != nil || c.Kind() != k {
			t.Fatalf("New(%s): %v", k, err)
		}
	}
	if arr.KindLidarr.APIPrefix() != "/api/v1" || arr.KindSonarr.APIPrefix() != "/api/v3" || arr.KindRadarr.AppName() != "Radarr" ||
		arr.Kind("x").AppName() != "" {
		t.Fatal("kind helpers")
	}
}

func TestValidateBackup(t *testing.T) {
	now := time.Now()
	for _, tt := range []struct {
		typ, name string
		ok        bool
	}{
		{"manual", "radarr_backup_v6.4.4.10685_2026.09.25_12.30.35.zip", true},
		{"scheduled", "sonarr_backup_v4.0.20.3014_2026.09.25_12.34.53.zip", true},
		{"update", "a.zip", true},
		{"Manual", "a.zip", false},
		{"other", "a.zip", false},
		{"", "a.zip", false},
		{"manual", ".zip", false},
		{"manual", "../a.zip", false},
		{"manual", "a/b.zip", false},
		{"manual", "a.ZIP", false},
		{"manual", "a.zip.exe", false},
		{"manual", "a b.zip", false},
		{"manual", "a%2f.zip", false},
		{"manual", strings.Repeat("a", 200) + ".zip", true},
		{"manual", strings.Repeat("a", 201) + ".zip", false},
	} {
		b := arr.Backup{Type: tt.typ, Name: tt.name, Time: now}
		err := arr.ValidateBackup(b)
		if (err == nil) != tt.ok {
			t.Errorf("ValidateBackup(%q, %q) = %v, want ok=%v", tt.typ, tt.name, err, tt.ok)
		}
		p, perr := arr.BackupPath(b)
		if tt.ok && (perr != nil || p != tt.typ+"/"+tt.name) {
			t.Errorf("BackupPath = %q, %v", p, perr)
		}
		if !tt.ok && !errors.Is(perr, arr.ErrInvalidBackup) {
			t.Errorf("BackupPath(%q, %q) = %v, want ErrInvalidBackup", tt.typ, tt.name, perr)
		}
	}
}

// TestBackupPathIsNeverUsed serves a system/backup entry whose path points elsewhere: the client
// neither decodes it nor goes there.
func TestBackupPathIsNeverUsed(t *testing.T) {
	ctx := context.Background()
	srv := arrtest.NewServer(t, arr.KindRadarr, testKey)
	const name = "radarr_backup_v6.4.4.10685_2026.09.25_12.30.35.zip"
	srv.SetBackupPath(name, "http://evil.example/steal?key=")
	c := newClient(t, arr.KindRadarr, srv.URL, arr.Options{})
	backups, err := c.Backups(ctx)
	if err != nil || len(backups) != 1 || backups[0].Name != name || backups[0].Type != "manual" || backups[0].Size != 92309 ||
		backups[0].ID != 670315691 || !backups[0].Time.Equal(time.Date(2026, 9, 25, 12, 30, 35, 0, time.UTC)) {
		t.Fatalf("Backups = %+v, %v", backups, err)
	}
	srv.ResetRequests()
	var buf bytes.Buffer
	n, err := c.DownloadBackup(ctx, backups[0], &buf)
	if err != nil || n != int64(buf.Len()) || n == 0 {
		t.Fatalf("DownloadBackup = %d, %v", n, err)
	}
	reqs := srv.Requests()
	if len(reqs) != 1 || reqs[0].Path != "/backup/manual/"+name || reqs[0].RawQuery != "" {
		t.Fatalf("requests = %+v, want only /backup/manual/%s", reqs, name)
	}
	if reqs[0].Header.Get("X-Api-Key") != testKey {
		t.Fatal("the download did not send the key header")
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil || len(zr.File) != 2 || zr.File[0].Name != "config.xml" || zr.File[1].Name != "radarr.db" {
		t.Fatalf("downloaded zip: %v", err)
	}
}

func TestDownloadBackup(t *testing.T) {
	ctx := context.Background()
	srv := arrtest.NewServer(t, arr.KindSonarr, testKey)
	c := newClient(t, arr.KindSonarr, srv.URL, arr.Options{})
	b := arr.Backup{Type: "scheduled", Name: "sonarr_backup_2026.09.20.zip", Size: 5}
	srv.SetBackupZip(b.Type, b.Name, []byte("12345"))
	var buf bytes.Buffer
	if n, err := c.DownloadBackup(ctx, b, &buf); err != nil || n != 5 || buf.String() != "12345" {
		t.Fatalf("download = %d %q, %v", n, buf.String(), err)
	}

	// Larger than the size system/backup reported, or than the cap.
	srv.SetBackupZip(b.Type, b.Name, []byte("123456"))
	buf.Reset()
	if _, err := c.DownloadBackup(ctx, b, &buf); !errors.Is(err, arr.ErrTooLarge) {
		t.Fatalf("oversized download: %v, want ErrTooLarge", err)
	}

	// Forms login: 302 to /login, never followed.
	srv.RequireLogin(true)
	srv.ResetRequests()
	_, err := c.DownloadBackup(ctx, b, &buf)
	var ae *arr.Error
	if !errors.Is(err, arr.ErrLoginRequired) || !errors.As(err, &ae) || ae.StatusCode != http.StatusFound {
		t.Fatalf("login required: %v", err)
	}
	if n := len(srv.Requests()); n != 1 {
		t.Fatalf("%d requests, want 1 (the redirect is not followed)", n)
	}
	srv.RequireLogin(false)

	// An invalid entry is refused before anything is sent.
	srv.ResetRequests()
	if _, err := c.DownloadBackup(ctx, arr.Backup{Type: "manual", Name: "../../config.xml"}, &buf); !errors.Is(err, arr.ErrInvalidBackup) {
		t.Fatalf("invalid entry: %v", err)
	}
	if n := len(srv.Requests()); n != 0 {
		t.Fatalf("an invalid entry sent %d requests", n)
	}
	// An unknown backup.
	if _, err := c.DownloadBackup(ctx, arr.Backup{Type: "manual", Name: "gone.zip"}, &buf); !errors.Is(err, arr.ErrNotFound) {
		t.Fatalf("missing backup: %v", err)
	}
}

func TestProbeBackup(t *testing.T) {
	ctx := context.Background()
	srv := arrtest.NewServer(t, arr.KindLidarr, testKey)
	c := newClient(t, arr.KindLidarr, srv.URL, arr.Options{})
	backups, err := c.Backups(ctx)
	if err != nil || len(backups) != 1 {
		t.Fatal(backups, err)
	}
	srv.ResetRequests()
	if got, err := c.ProbeBackup(ctx, backups[0]); err != nil || got != arr.BackupAccessOK {
		t.Fatalf("ProbeBackup = %q, %v", got, err)
	}
	if r := srv.Requests(); len(r) != 1 || r[0].Header.Get("Range") != "bytes=0-0" {
		t.Fatalf("probe request = %+v", r)
	}
	srv.RequireLogin(true)
	if got, err := c.ProbeBackup(ctx, backups[0]); err != nil || got != arr.BackupAccessLoginRequired {
		t.Fatalf("ProbeBackup with login = %q, %v", got, err)
	}
	srv.RequireLogin(false)
	if _, err := c.ProbeBackup(ctx, arr.Backup{Type: "manual", Name: "gone.zip"}); !errors.Is(err, arr.ErrNotFound) {
		t.Fatalf("missing backup: %v", err)
	}
}

func TestStartBackupAndPoll(t *testing.T) {
	ctx := context.Background()
	srv := arrtest.NewServer(t, arr.KindRadarr, testKey)
	c := newClient(t, arr.KindRadarr, srv.URL, arr.Options{})
	cmd, err := c.StartBackup(ctx)
	if err != nil || cmd.ID != 21 || cmd.Name != "Backup" || cmd.Status != "started" || cmd.Result != "unknown" ||
		!cmd.Queued.Equal(time.Date(2026, 9, 25, 12, 30, 35, 0, time.UTC)) || !cmd.Ended.IsZero() {
		t.Fatalf("StartBackup = %+v, %v", cmd, err)
	}
	cmd, err = c.Command(ctx, cmd.ID)
	if err != nil || cmd.Status != "completed" || cmd.Result != "successful" || cmd.Message != "Backup zip created" || cmd.Ended.IsZero() {
		t.Fatalf("Command = %+v, %v", cmd, err)
	}
}

// TestArrtestRouteTable pins the fake's route table: exactly the allow-list, per application.
func TestArrtestRouteTable(t *testing.T) {
	want := map[arr.Kind][]string{
		arr.KindRadarr: {"GET /api/v3/config/mediamanagement", "GET /api/v3/movie", "GET /api/v3/movie/1", "GET /api/v3/movie/2",
			"GET /api/v3/movie/3", "GET /api/v3/movie/4", "GET /api/v3/moviefile?movieId=1", "GET /api/v3/qualityprofile",
			"GET /api/v3/rootfolder", "GET /api/v3/system/backup", "GET /api/v3/system/status", "GET /api/v3/tag",
			"GET /api/v3/command/21", "POST /api/v3/command",
			"GET /backup/manual/radarr_backup_v6.4.4.10685_2026.09.25_12.30.35.zip"},
		arr.KindSonarr: {"GET /api/v3/config/mediamanagement", "GET /api/v3/episode?seriesId=1", "GET /api/v3/episode?seriesId=2",
			"GET /api/v3/episodefile?seriesId=1", "GET /api/v3/episodefile?seriesId=2", "GET /api/v3/qualityprofile",
			"GET /api/v3/rootfolder", "GET /api/v3/series", "GET /api/v3/series/1", "GET /api/v3/series/2",
			"GET /api/v3/system/backup", "GET /api/v3/system/status", "GET /api/v3/tag", "GET /api/v3/command/31",
			"POST /api/v3/command", "GET /backup/manual/sonarr_backup_v4.0.20.3014_2026.09.25_12.34.53.zip"},
		arr.KindLidarr: {"GET /api/v1/album?artistId=1", "GET /api/v1/artist", "GET /api/v1/artist/1", "GET /api/v1/config/mediamanagement",
			"GET /api/v1/metadataprofile", "GET /api/v1/qualityprofile", "GET /api/v1/rootfolder", "GET /api/v1/system/backup",
			"GET /api/v1/system/status", "GET /api/v1/tag", "GET /api/v1/trackfile?artistId=1", "GET /api/v1/command/40",
			"POST /api/v1/command", "GET /backup/manual/lidarr_backup_v3.1.0.4875_2026.09.25_12.39.28.zip"},
	}
	for kind, routes := range want {
		srv := arrtest.NewServer(t, kind, testKey)
		sort.Strings(routes)
		if got := srv.Routes(); !slices.Equal(got, routes) {
			t.Errorf("%s routes:\n got %q\nwant %q", kind, got, routes)
		}
	}
}

// TestArrtestRefusesWhatTheRealAppRefuses checks the fake's own checks.
func TestArrtestRefusesWhatTheRealAppRefuses(t *testing.T) {
	srv := arrtest.NewServer(t, arr.KindRadarr, testKey)
	do := func(method, target string, header map[string]string, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+target, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range header {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	withKey := map[string]string{"X-Api-Key": testKey}
	if r := do("GET", "/api/v3/tag", nil, ""); r.StatusCode != 401 {
		t.Errorf("no key: %d", r.StatusCode)
	}
	if r := do("GET", "/api/v3/tag-detail", withKey, ""); r.StatusCode != 404 {
		t.Errorf("a route outside the table: %d", r.StatusCode)
	}
	if r := do("GET", "/api/v3/health", withKey, ""); r.StatusCode != 404 {
		t.Errorf("health (recorded, not allowed): %d", r.StatusCode)
	}
	if r := do("DELETE", "/api/v3/movie/1", withKey, ""); r.StatusCode != 404 {
		t.Errorf("DELETE: %d", r.StatusCode)
	}
	if r := do("GET", "/api/v3/moviefile?movieId=1", withKey, ""); r.StatusCode != 200 {
		t.Errorf("moviefile: %d", r.StatusCode)
	}

	rec := &recordingTB{TB: t}
	strict := arrtest.NewServer(rec, arr.KindRadarr, testKey)
	for _, tt := range []struct {
		target, body string
	}{
		{"/api/v3/tag?apikey=" + url.QueryEscape(testKey), ""},
		{"/api/v3/command", `{"name":"DeleteMovie"}`},
		{"/api/v3/command", `{"name":"Backup","extra":1}`},
	} {
		method := "GET"
		if tt.body != "" {
			method = "POST"
		}
		req, _ := http.NewRequest(method, strict.URL+tt.target, strings.NewReader(tt.body))
		req.Header.Set("X-Api-Key", testKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("%s %s: %d, want 400", method, tt.target, resp.StatusCode)
		}
	}
	if n := rec.count(); n != 3 {
		t.Errorf("the fake reported %d test failures, want 3", n)
	}
}
