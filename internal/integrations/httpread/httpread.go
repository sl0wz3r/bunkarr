// Package httpread is the request machinery shared by the read-only Tautulli, Seerr and
// Maintainerr clients (docs/design/phase2-3.md §4.4, §4.5). It sends GET requests only: each
// client builds its own fixed list of requests on top of it and has no general request method, so
// the allow-list (S16) lives in the client packages.
//
// Safety (S8, S16):
//   - The API key, when there is one, is sent only in the X-Api-Key header, never in a URL, and
//     only to the base URL the client was built with. A request may also go without it (Seerr's
//     status).
//   - Redirects are never followed, so the header never reaches another host.
//   - Every connection dials through the outbound guard (internal/netguard); a caller's
//     Options.HTTPClient must do the same.
//   - Errors (*Error) carry the application, the method, the path and a cause: never a URL, a
//     query, a header or a response body. The key is redacted from them (it never should appear).
//   - Bodies are capped; a body over its cap fails with ErrTooLarge.
package httpread

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/logging"
	"github.com/sl0wz3r/bunkarr/internal/netguard"
	"github.com/sl0wz3r/bunkarr/internal/version"
)

// Defaults of Options.
const (
	// DefaultTimeout bounds each request (connect, headers and body).
	DefaultTimeout = 30 * time.Second
	// DefaultMaxResponseBytes caps a response body.
	DefaultMaxResponseBytes = 32 << 20
)

var (
	// ErrTooLarge means a response body exceeded its cap.
	ErrTooLarge = errors.New("the response is larger than allowed")
	// ErrRedirected means the server answered with a redirect, which is never followed.
	ErrRedirected = errors.New("the server answered with a redirect (redirects are not followed; check the URL and its URL base)")
)

// Error is a failed request. Its text is built from the application, the method, the path and
// the cause only.
type Error struct {
	// App names the application ("Tautulli").
	App string
	// Method and Path identify the request ("GET", "/api/v2"); Path is relative to the base URL
	// and never holds a query.
	Method string
	Path   string
	// StatusCode is the HTTP status, 0 when no response was received.
	StatusCode int
	// Err is the cause: a sentinel of the client package, ErrTooLarge, ErrRedirected, a timeout
	// wrapping context.DeadlineExceeded, the caller's context error, or a network error.
	Err error
}

// Error implements error: "<app> <method> <path>: <cause>".
func (e *Error) Error() string {
	return fmt.Sprintf("%s %s %s: %v", e.App, e.Method, e.Path, e.Err)
}

// Unwrap returns the cause.
func (e *Error) Unwrap() error { return e.Err }

// Options configures New. Zero values take the defaults.
type Options struct {
	// HTTPClient is the client to use; its transport must dial through internal/netguard (S16).
	// Default: a client with netguard.NewTransport(). Its CheckRedirect is replaced on a copy.
	HTTPClient *http.Client
	// Timeout overrides DefaultTimeout.
	Timeout time.Duration
	// MaxResponseBytes overrides DefaultMaxResponseBytes (a request may ask for a larger cap).
	MaxResponseBytes int64
	// Logger receives one debug line per request (method, path, status, duration).
	Logger *slog.Logger
}

// Client sends GET requests to one application. It is safe for concurrent use.
type Client struct {
	app  string
	base string
	key  string
	hc   *http.Client
	log  *slog.Logger
	tmo  time.Duration
	maxB int64
}

// New returns a client for the application app at baseURL (http or https, no user name or
// password, no query or fragment; a URL base such as http://host:8181/tautulli is kept). key is
// sent as X-Api-Key on the requests that ask for it ("" sends none).
func New(app, baseURL, key string, o Options) (*Client, error) {
	base, err := ParseBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	if strings.ContainsAny(key, "\r\n\x00") {
		return nil, errors.New("the API key contains invalid characters")
	}
	c := &Client{app: app, base: base, key: key, log: o.Logger, tmo: o.Timeout, maxB: o.MaxResponseBytes}
	if c.tmo <= 0 {
		c.tmo = DefaultTimeout
	}
	if c.maxB <= 0 {
		c.maxB = DefaultMaxResponseBytes
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	var hc http.Client
	if o.HTTPClient != nil {
		hc = *o.HTTPClient
	} else {
		hc.Transport = netguard.NewTransport()
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	hc.Timeout = 0 // each request has its own deadline
	c.hc = &hc
	return c, nil
}

// ParseBaseURL checks raw and returns it without a trailing slash. Its errors do not repeat the
// URL (it could hold a credential).
func ParseBaseURL(raw string) (string, error) {
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

// Request is one GET request.
type Request struct {
	// Path is relative to the base URL, without a query ("/api/v2").
	Path  string
	Query url.Values
	// WithKey sends the API key (X-Api-Key).
	WithKey bool
	// MaxBytes overrides the client's body cap for this request (0: the client's).
	MaxBytes int64
	// Timeout overrides the client's timeout for this request (0: the client's).
	Timeout time.Duration
}

// Response is a received answer: the status and the whole (capped) body. A non-2xx answer is a
// Response too; the client packages map its status to their own causes and never echo the body.
type Response struct {
	StatusCode  int
	ContentType string
	Body        []byte
}

// Fail builds the *Error of a request.
func (c *Client) Fail(r Request, status int, cause error) error {
	return &Error{App: c.app, Method: http.MethodGet, Path: r.Path, StatusCode: status, Err: c.sanitize(cause)}
}

// Get performs r and reads its body. A 3xx answer fails with ErrRedirected; other statuses are
// returned for the caller to map.
func (c *Client) Get(ctx context.Context, r Request) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, c.Fail(r, 0, err)
	}
	tmo := r.Timeout
	if tmo <= 0 {
		tmo = c.tmo
	}
	limit := r.MaxBytes
	if limit <= 0 {
		limit = c.maxB
	}
	rctx, cancel := context.WithTimeout(ctx, tmo)
	defer cancel()
	target := c.base + r.Path
	if len(r.Query) > 0 {
		target += "?" + r.Query.Encode()
	}
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, target, nil)
	if err != nil {
		return Response{}, c.Fail(r, 0, errors.New("cannot build the request"))
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", version.UserAgent())
	if r.WithKey && c.key != "" {
		req.Header.Set("X-Api-Key", c.key)
	}
	start := time.Now()
	resp, err := c.hc.Do(req)
	if err != nil {
		cause := transportCause(ctx, err, tmo)
		c.log.Debug("Request failed", "app", c.app, "path", r.Path, "durationMs", time.Since(start).Milliseconds(), "error", c.sanitize(cause))
		return Response{}, c.Fail(r, 0, cause)
	}
	defer func() {
		_, _ = io.CopyN(io.Discard, resp.Body, 64<<10)
		_ = resp.Body.Close()
	}()
	c.log.Debug("Request", "app", c.app, "path", r.Path, "status", resp.StatusCode, "durationMs", time.Since(start).Milliseconds())
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return Response{}, c.Fail(r, resp.StatusCode, ErrRedirected)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return Response{}, c.Fail(r, resp.StatusCode, transportCause(ctx, err, tmo))
	}
	if int64(len(body)) > limit {
		return Response{}, c.Fail(r, resp.StatusCode, ErrTooLarge)
	}
	return Response{StatusCode: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), Body: body}, nil
}

// transportCause reduces a client error to a cause that holds no URL, query or header: the
// *url.Error wrapper (which carries the full URL) is dropped, a network error is rebuilt from its
// Op and Err, and timeouts are named.
func transportCause(parent context.Context, err error, timeout time.Duration) error {
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
