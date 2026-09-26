// Package arr is a read-only client for the Sonarr (API v3), Radarr (API v3) and Lidarr (API v1)
// HTTP APIs: the requests of docs/design/phase2-3.md §4.3 and nothing else.
//
// Allow-list (S16). The client has no general request method. Each exported method sends one
// fixed request, and a method that belongs to another app (Movies on a Sonarr client) is refused
// before anything is sent. The only write is StartBackup (POST command {"name":"Backup"}), which
// makes the *arr create a backup zip in its own config folder (§10).
//
// Safety (S8, S18). The API key is sent only in the X-Api-Key header, never in a URL, and only to
// the base URL the client was built with. Redirects are not followed. Errors (*Error) carry the
// method, the path and a cause; never a URL, a query, a header or a response body, and the key is
// redacted from them. The client does not put the key in the redaction registry: it may be one
// typed into a Test form (integrations.Store holds the stored keys there). The path field of
// system/backup is never decoded: a backup's location is built from its validated type and name.
//
// Bounds. Each request has a timeout (30 s; 5 min for the full movie, series and artist lists; 30
// min for a backup download) and a body cap (32 MiB; 256 MiB for the full lists, decoded as a
// stream; 8 GiB for a backup download). The default transport dials through the outbound guard
// (internal/netguard, S16); a caller's Options.HTTPClient must do the same.
package arr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/logging"
	"github.com/sl0wz3r/bunkarr/internal/netguard"
	"github.com/sl0wz3r/bunkarr/internal/version"
)

// Kind is the application a client talks to.
type Kind string

// The supported applications.
const (
	KindSonarr Kind = "sonarr"
	KindRadarr Kind = "radarr"
	KindLidarr Kind = "lidarr"
)

// Valid reports whether k is one of the supported applications.
func (k Kind) Valid() bool {
	return k == KindSonarr || k == KindRadarr || k == KindLidarr
}

// AppName is the appName the application reports in system/status ("Sonarr", "Radarr",
// "Lidarr"); "" for an unknown kind.
func (k Kind) AppName() string {
	switch k {
	case KindSonarr:
		return "Sonarr"
	case KindRadarr:
		return "Radarr"
	case KindLidarr:
		return "Lidarr"
	}
	return ""
}

// APIPrefix is the API root under the base URL: /api/v3 (Sonarr, Radarr) or /api/v1 (Lidarr).
func (k Kind) APIPrefix() string {
	if k == KindLidarr {
		return "/api/v1"
	}
	return "/api/v3"
}

// Defaults and limits of the client.
const (
	// DefaultTimeout bounds each request except the full lists and backup downloads.
	DefaultTimeout = 30 * time.Second
	// DefaultListTimeout bounds GET movie, GET series and GET artist.
	DefaultListTimeout = 5 * time.Minute
	// DefaultDownloadTimeout bounds a backup download.
	DefaultDownloadTimeout = 30 * time.Minute
	// MaxResponseBytes caps every response body except the full lists and backup downloads.
	MaxResponseBytes = 32 << 20
	// MaxListBytes caps the body of GET movie, GET series and GET artist.
	MaxListBytes = 256 << 20
	// MaxBackupBytes caps a backup download.
	MaxBackupBytes = 8 << 30
)

var (
	// ErrWrongApp means the server answered system/status as another application (a Sonarr URL
	// saved as Radarr), or as nothing the client knows.
	ErrWrongApp = errors.New("the server is not the expected application")
	// ErrUnauthorized means the *arr answered 401: the API key is missing or wrong.
	ErrUnauthorized = errors.New("the API key was rejected (401 Unauthorized)")
	// ErrNotFound means the *arr answered 404 (an unknown item id, or no such API route).
	ErrNotFound = errors.New("not found (404)")
	// ErrNotArr means the server answered, but not like an *arr API (a redirect, HTML, or a body
	// that is not the expected JSON).
	ErrNotArr = errors.New("the server did not answer like the *arr API")
	// ErrLoginRequired means a backup download was answered with a redirect to the login page or
	// 401: the *arr requires a login for Bunkarr's address (Forms authentication, design D3).
	ErrLoginRequired = errors.New("the *arr requires a login to download backups")
	// ErrTooLarge means a response body exceeded its cap.
	ErrTooLarge = errors.New("the response is larger than allowed")
	// ErrWrongKind means a method of another application was called (Movies on a Sonarr client);
	// nothing was sent.
	ErrWrongKind = errors.New("this request is not allowed for this application")
	// ErrInvalidBackup means a system/backup entry has a type other than manual, scheduled or
	// update, or a name that is not a plain zip file name (design S18).
	ErrInvalidBackup = errors.New("the backup entry has an invalid type or name")
)

// Error is a failed request. Its text is built from the method, the path and the cause only.
type Error struct {
	// Method and Path identify the request ("GET", "/api/v3/movie"); Path is relative to the base
	// URL and never holds a query.
	Method string
	Path   string
	// StatusCode is the HTTP status, 0 when no response was received.
	StatusCode int
	// Err is the cause: one of the Err* values of this package, a timeout wrapping
	// context.DeadlineExceeded, the caller's context error, or a network error.
	Err error
}

// Error implements error: "<method> <path>: <cause>".
func (e *Error) Error() string {
	return fmt.Sprintf("%s %s: %v", e.Method, e.Path, e.Err)
}

// Unwrap returns the cause.
func (e *Error) Unwrap() error { return e.Err }

// Options configures New. Zero values take the defaults.
type Options struct {
	// HTTPClient is the client to use; its transport must dial through internal/netguard (S16).
	// Default: a client with netguard.NewTransport(). Its CheckRedirect is replaced on a copy:
	// redirects are never followed.
	HTTPClient *http.Client
	// Timeout, ListTimeout and DownloadTimeout override DefaultTimeout, DefaultListTimeout and
	// DefaultDownloadTimeout.
	Timeout         time.Duration
	ListTimeout     time.Duration
	DownloadTimeout time.Duration
	// MaxResponseBytes and MaxListBytes override the body caps (tests); 0 takes the defaults.
	MaxResponseBytes int64
	MaxListBytes     int64
	// Logger receives one debug line per request (method, path, status, duration); default: none.
	Logger *slog.Logger
}

// Client talks to one Sonarr, Radarr or Lidarr. It is safe for concurrent use.
type Client struct {
	kind  Kind
	base  string // no trailing slash
	key   string
	hc    *http.Client
	log   *slog.Logger
	tmo   time.Duration
	lsTmo time.Duration
	dlTmo time.Duration
	maxB  int64
	maxL  int64
}

// New returns a client for the *arr of the given kind at baseURL (http or https, no user name or
// password, no query or fragment; a URL base such as http://host:7878/radarr is kept). apiKey may
// be "" (every API request then fails with ErrUnauthorized).
func New(kind Kind, baseURL, apiKey string, opts Options) (*Client, error) {
	if !kind.Valid() {
		return nil, fmt.Errorf("unknown *arr kind %q", kind)
	}
	base, err := parseBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	if strings.ContainsAny(apiKey, "\r\n\x00") {
		return nil, errors.New("the API key contains invalid characters")
	}
	c := &Client{
		kind: kind, base: base, key: apiKey, log: opts.Logger,
		tmo: opts.Timeout, lsTmo: opts.ListTimeout, dlTmo: opts.DownloadTimeout,
		maxB: opts.MaxResponseBytes, maxL: opts.MaxListBytes,
	}
	if c.tmo <= 0 {
		c.tmo = DefaultTimeout
	}
	if c.lsTmo <= 0 {
		c.lsTmo = DefaultListTimeout
	}
	if c.dlTmo <= 0 {
		c.dlTmo = DefaultDownloadTimeout
	}
	if c.maxB <= 0 {
		c.maxB = MaxResponseBytes
	}
	if c.maxL <= 0 {
		c.maxL = MaxListBytes
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	var hc http.Client
	if opts.HTTPClient != nil {
		hc = *opts.HTTPClient
	} else {
		hc.Transport = netguard.NewTransport()
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	hc.Timeout = 0 // each request has its own deadline
	c.hc = &hc
	return c, nil
}

// Kind returns the application the client talks to.
func (c *Client) Kind() Kind { return c.kind }

// parseBaseURL checks raw and returns it without a trailing slash. Its errors do not repeat the
// URL.
func parseBaseURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("the URL is empty")
	}
	if strings.ContainsAny(s, "?#") {
		return "", errors.New("the URL must not contain a query or fragment")
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", errors.New("the URL is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("the URL must start with http:// or https://")
	}
	if u.User != nil {
		return "", errors.New("the URL must not contain a user name or password")
	}
	if u.Opaque != "" || u.Hostname() == "" {
		return "", errors.New("the URL has no host")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// request is one request of the allow-list.
type request struct {
	method string
	// path is relative to the base URL, without a query: "/api/v3/movie", "/backup/manual/x.zip".
	path  string
	query url.Values
	body  []byte
	// timeout and limit bound the request and its body.
	timeout time.Duration
	limit   int64
	// accept is the Accept header (default application/json); rangeFirst asks for the first byte.
	accept     string
	rangeFirst bool
}

// api builds a request for an API path ("movie" → "/api/v3/movie") with the default bounds.
func (c *Client) api(method, p string, query url.Values) request {
	return request{method: method, path: c.kind.APIPrefix() + "/" + p, query: query, timeout: c.tmo, limit: c.maxB}
}

// fail builds an *Error for r.
func (c *Client) fail(r request, status int, cause error) error {
	return &Error{Method: r.method, Path: r.path, StatusCode: status, Err: c.sanitize(cause)}
}

// send performs r and returns the response with its body wrapped in the cap; the caller must call
// the returned release function. A non-2xx status is returned as an error after the body was
// drained (never decoded), except that handleStatus may claim it first.
func (c *Client) send(ctx context.Context, r request) (*http.Response, io.Reader, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, c.fail(r, 0, err)
	}
	rctx, cancel := context.WithTimeout(ctx, r.timeout)
	target := c.base + r.path
	if len(r.query) > 0 {
		target += "?" + r.query.Encode()
	}
	var body io.Reader
	if r.body != nil {
		body = bytes.NewReader(r.body)
	}
	req, err := http.NewRequestWithContext(rctx, r.method, target, body)
	if err != nil {
		cancel()
		return nil, nil, nil, c.fail(r, 0, errors.New("cannot build the request"))
	}
	accept := r.accept
	if accept == "" {
		accept = "application/json"
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", version.UserAgent())
	if c.key != "" {
		req.Header.Set("X-Api-Key", c.key)
	}
	if r.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.rangeFirst {
		req.Header.Set("Range", "bytes=0-0")
	}
	start := time.Now()
	resp, err := c.hc.Do(req)
	if err != nil {
		cancel()
		cause := c.transportCause(ctx, err, r.timeout)
		c.log.Debug("*arr request failed", "app", string(c.kind), "method", r.method, "path", r.path,
			"durationMs", time.Since(start).Milliseconds(), "error", c.sanitize(cause))
		return nil, nil, nil, c.fail(r, 0, cause)
	}
	c.log.Debug("*arr request", "app", string(c.kind), "method", r.method, "path", r.path, "status", resp.StatusCode,
		"durationMs", time.Since(start).Milliseconds())
	release := func() {
		_, _ = io.CopyN(io.Discard, resp.Body, 64<<10)
		_ = resp.Body.Close()
		cancel()
	}
	return resp, &capReader{r: resp.Body, left: r.limit}, release, nil
}

// statusError maps a non-2xx status of an API request to its cause.
func statusError(code int) error {
	switch {
	case code == http.StatusUnauthorized:
		return ErrUnauthorized
	case code == http.StatusNotFound:
		return ErrNotFound
	case code >= 300 && code < 400:
		return fmt.Errorf("%w: redirected with %d (redirects are not followed; check the URL and its URL base)", ErrNotArr, code)
	}
	return fmt.Errorf("unexpected status %d %s", code, http.StatusText(code))
}

// getJSON performs an API request and decodes its JSON body into out.
func (c *Client) getJSON(ctx context.Context, r request, out any) error {
	resp, body, release, err := c.send(ctx, r)
	if err != nil {
		return err
	}
	defer release()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return c.fail(r, resp.StatusCode, statusError(resp.StatusCode))
	}
	dec := json.NewDecoder(body)
	if err := dec.Decode(out); err != nil {
		return c.fail(r, resp.StatusCode, c.decodeCause(ctx, err, r.timeout))
	}
	if err := expectEOF(dec); err != nil {
		return c.fail(r, resp.StatusCode, c.decodeCause(ctx, err, r.timeout))
	}
	return nil
}

// eachJSON performs an API request whose body is a JSON array and calls fn for each element, as
// the body streams in. It stops at fn's first error and returns it unwrapped.
func eachJSON[T any](ctx context.Context, c *Client, r request, fn func(T) error) error {
	resp, body, release, err := c.send(ctx, r)
	if err != nil {
		return err
	}
	defer release()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return c.fail(r, resp.StatusCode, statusError(resp.StatusCode))
	}
	dec := json.NewDecoder(body)
	decodeFail := func(err error) error { return c.fail(r, resp.StatusCode, c.decodeCause(ctx, err, r.timeout)) }
	tok, err := dec.Token()
	if err != nil {
		return decodeFail(err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return c.fail(r, resp.StatusCode, fmt.Errorf("%w: expected a JSON list", ErrNotArr))
	}
	for dec.More() {
		var v T
		if err := dec.Decode(&v); err != nil {
			return decodeFail(err)
		}
		if err := fn(v); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil {
		return decodeFail(err)
	}
	if err := expectEOF(dec); err != nil {
		return decodeFail(err)
	}
	return nil
}

// listJSON is eachJSON collecting the elements.
func listJSON[T any](ctx context.Context, c *Client, r request) ([]T, error) {
	out := []T{}
	err := eachJSON(ctx, c, r, func(v T) error {
		out = append(out, v)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// expectEOF fails when anything but white space follows the decoded value.
func expectEOF(dec *json.Decoder) error {
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("%w: unexpected data after the JSON value", ErrNotArr)
		}
		return err
	}
	return nil
}

// decodeCause describes a decoding failure without quoting the body.
func (c *Client) decodeCause(parent context.Context, err error, timeout time.Duration) error {
	var syn *json.SyntaxError
	var typ *json.UnmarshalTypeError
	switch {
	case errors.Is(err, ErrTooLarge):
		return err
	case errors.As(err, &syn):
		return fmt.Errorf("%w: the response is not JSON", ErrNotArr)
	case errors.As(err, &typ):
		return fmt.Errorf("%w: unexpected JSON type for %q", ErrNotArr, typ.Field)
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		if perr := parent.Err(); perr != nil {
			return perr
		}
		return fmt.Errorf("%w: the response ended early", ErrNotArr)
	}
	return c.transportCause(parent, err, timeout)
}

// transportCause reduces a client error to a cause that holds no URL, query or header: the
// *url.Error wrapper (which carries the full URL) is dropped, a network error is rebuilt from its
// Op and Err, and timeouts are named.
func (c *Client) transportCause(parent context.Context, err error, timeout time.Duration) error {
	if perr := parent.Err(); perr != nil {
		return perr
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("no answer within %s: %w", timeout, context.DeadlineExceeded)
	}
	var uerr *url.Error
	if errors.As(err, &uerr) {
		err = uerr.Err
		if uerr.Timeout() {
			return fmt.Errorf("no answer within %s: %w", timeout, context.DeadlineExceeded)
		}
	}
	var operr *net.OpError
	if errors.As(err, &operr) && operr.Err != nil {
		if operr.Net != "" {
			return fmt.Errorf("%s %s: %w", operr.Op, operr.Net, operr.Err)
		}
		return fmt.Errorf("%s: %w", operr.Op, operr.Err)
	}
	return err
}

// sanitize replaces an error whose text contains the key (it never should) by a redacted copy.
func (c *Client) sanitize(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if c.key != "" && strings.Contains(msg, c.key) {
		return errors.New(strings.ReplaceAll(msg, c.key, logging.Redacted))
	}
	if logging.ContainsSecret(msg) {
		return errors.New(logging.RedactSecrets(msg))
	}
	return err
}

// capReader returns ErrTooLarge once more than left bytes were read.
type capReader struct {
	r    io.Reader
	left int64
}

// Read implements io.Reader: it reads at most one byte past the cap, then fails.
func (c *capReader) Read(p []byte) (int, error) {
	if c.left < 0 {
		return 0, ErrTooLarge
	}
	if int64(len(p)) > c.left+1 {
		p = p[:c.left+1]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	if c.left < 0 {
		return n, ErrTooLarge
	}
	return n, err
}

// appNamePattern is what an appName must look like to be quoted in a message.
var appNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9 ]{0,31}$`)
