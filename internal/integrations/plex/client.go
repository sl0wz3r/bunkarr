// Package plex is a small client for the Plex Media Server HTTP API: the server's identity, its
// library sections with their folder locations, and the preferences Bunkarr needs (the butler
// window and Plex's own database backup task).
//
// Safety (design S8): the token is sent only in the X-Plex-Token header, never in a URL, and only
// to servers that answered /identity like Plex (Test checks the identity first; Identity itself is
// sent without the token). Redirects are not followed, so the header never reaches another host.
// Errors name the method and path only (never the full URL, a query, a header or a response
// body), and the client's own token is redacted from them. The client does not put the token in
// the process-wide redaction registry: it may be one typed into a Test form, and only stored tokens
// enter that registry. integrations.Store holds each one there (logging.SetSecrets, owner
// "integration:<id>") from the write that stores it, or from Store.RegisterSecrets at start-up,
// and releases it when it is replaced, cleared or deleted; reading it (Store.TokenFor) does not.
package plex

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
	"strconv"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/logging"
	"github.com/sl0wz3r/bunkarr/internal/version"
)

// Defaults for Options.
const (
	// DefaultTimeout bounds each request (connect, headers and body).
	DefaultTimeout = 10 * time.Second
	// DefaultClientIdentifier is sent as X-Plex-Client-Identifier when none is configured.
	DefaultClientIdentifier = "bunkarr"
	// MaxResponseBytes is the largest response body the client reads.
	MaxResponseBytes = 8 << 20
)

// Paths of the Plex endpoints the client uses.
const (
	PathIdentity = "/identity"
	PathSections = "/library/sections"
	PathPrefs    = "/:/prefs"
)

var (
	// ErrUnauthorized means Plex answered 401: the token is missing, wrong or revoked, or the
	// server does not accept unauthenticated requests from Bunkarr's address.
	ErrUnauthorized = errors.New("plex rejected the request as unauthorized (check the token)")
	// ErrNotPlex means the server answered, but not like a Plex Media Server.
	ErrNotPlex = errors.New("the server did not answer like a Plex Media Server")
)

// Error is a failed request. Its text is built from the method, the path and the cause only.
type Error struct {
	// Method and Path identify the request ("GET", "/library/sections").
	Method string
	Path   string
	// StatusCode is the HTTP status, 0 when no response was received.
	StatusCode int
	// Err is the cause: ErrUnauthorized, ErrNotPlex, a timeout wrapping
	// context.DeadlineExceeded, the caller's context error, or a network error.
	Err error
}

// Error implements error: "plex <method> <path>: <cause>".
func (e *Error) Error() string {
	return fmt.Sprintf("plex %s %s: %v", e.Method, e.Path, e.Err)
}

// Unwrap returns the cause.
func (e *Error) Unwrap() error { return e.Err }

// Options configures New. Zero values take the defaults.
type Options struct {
	// HTTPClient is the client to use (default: a client with the default transport, so TLS
	// certificates are verified). Its CheckRedirect is replaced on a copy: redirects are never
	// followed.
	HTTPClient *http.Client
	// ClientIdentifier is sent as X-Plex-Client-Identifier (default DefaultClientIdentifier).
	ClientIdentifier string
	// Timeout bounds each request (default DefaultTimeout).
	Timeout time.Duration
	// Logger receives one debug line per request (method, path, status, duration); default: none.
	Logger *slog.Logger
}

// Client talks to one Plex Media Server. It is safe for concurrent use.
type Client struct {
	base     string
	token    string
	clientID string
	timeout  time.Duration
	hc       *http.Client
	log      *slog.Logger
}

// New returns a client for the server at baseURL (http or https, no user name or password, no
// query or fragment; a path prefix is allowed). token may be "" for servers that need none.
func New(baseURL, token string, opts Options) (*Client, error) {
	base, err := parseBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	if strings.ContainsAny(token, "\r\n\x00") {
		return nil, errors.New("plex token contains invalid characters")
	}

	c := &Client{
		base:     base,
		token:    token,
		clientID: opts.ClientIdentifier,
		timeout:  opts.Timeout,
		log:      opts.Logger,
	}
	if c.clientID == "" {
		c.clientID = DefaultClientIdentifier
	}
	if c.timeout <= 0 {
		c.timeout = DefaultTimeout
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	hc := &http.Client{}
	if opts.HTTPClient != nil {
		cp := *opts.HTTPClient
		hc = &cp
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c.hc = hc
	return c, nil
}

// parseBaseURL checks baseURL and returns it without a trailing slash. Its errors do not repeat
// the URL, which could contain a credential.
func parseBaseURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("plex URL is empty")
	}
	if strings.ContainsAny(s, "?#") {
		return "", errors.New("plex URL must not contain a query or fragment")
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", errors.New("plex URL is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("plex URL must start with http:// or https://")
	}
	if u.User != nil {
		return "", errors.New("plex URL must not contain a user name or password")
	}
	if u.Opaque != "" || u.Hostname() == "" {
		return "", errors.New("plex URL has no host")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// get performs GET path and decodes the JSON body into out. withToken adds X-Plex-Token.
func (c *Client) get(ctx context.Context, path string, withToken bool, out any) error {
	const method = http.MethodGet
	fail := func(status int, cause error) error {
		return &Error{Method: method, Path: path, StatusCode: status, Err: c.sanitize(cause)}
	}
	if err := ctx.Err(); err != nil {
		return fail(0, err)
	}
	rctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(rctx, method, c.base+path, nil)
	if err != nil {
		return fail(0, errors.New("cannot build the request"))
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", version.UserAgent())
	req.Header.Set("X-Plex-Product", "Bunkarr")
	req.Header.Set("X-Plex-Version", version.Version)
	req.Header.Set("X-Plex-Client-Identifier", c.clientID)
	if withToken && c.token != "" {
		req.Header.Set("X-Plex-Token", c.token)
	}

	start := time.Now()
	resp, err := c.hc.Do(req)
	if err != nil {
		cause := c.transportCause(ctx, err)
		c.log.Debug("Plex request failed", "method", method, "path", path, "durationMs", time.Since(start).Milliseconds(), "error", c.sanitize(cause))
		return fail(0, cause)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	c.log.Debug("Plex request", "method", method, "path", path, "status", resp.StatusCode, "bytes", len(body), "durationMs", time.Since(start).Milliseconds())
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		// Plex sends 401 as text/html even when JSON was asked for: never decode it.
		return fail(resp.StatusCode, ErrUnauthorized)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return fail(resp.StatusCode, fmt.Errorf("unexpected status %d %s", resp.StatusCode, http.StatusText(resp.StatusCode)))
	case readErr != nil:
		return fail(resp.StatusCode, c.transportCause(ctx, readErr))
	case len(body) > MaxResponseBytes:
		return fail(resp.StatusCode, fmt.Errorf("response larger than %d bytes", MaxResponseBytes))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fail(resp.StatusCode, fmt.Errorf("%w: %s", ErrNotPlex, describeJSONError(err)))
	}
	return nil
}

// transportCause reduces a client error to a cause that holds no URL, query or header: the
// *url.Error wrapper (which carries the full URL) is dropped, a network error is rebuilt from its
// Op and Err, and timeouts are named.
func (c *Client) transportCause(parent context.Context, err error) error {
	if perr := parent.Err(); perr != nil {
		return perr
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("no answer within %s: %w", c.timeout, context.DeadlineExceeded)
	}
	var uerr *url.Error
	if errors.As(err, &uerr) {
		err = uerr.Err
		if uerr.Timeout() {
			return fmt.Errorf("no answer within %s: %w", c.timeout, context.DeadlineExceeded)
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

// sanitize replaces an error whose text contains the token (it never should) by a redacted copy.
func (c *Client) sanitize(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if c.token != "" && strings.Contains(msg, c.token) {
		return errors.New(strings.ReplaceAll(msg, c.token, logging.Redacted))
	}
	if logging.ContainsSecret(msg) {
		return errors.New(logging.RedactSecrets(msg))
	}
	return err
}

// describeJSONError describes a decoding error without quoting the body.
func describeJSONError(err error) string {
	var syn *json.SyntaxError
	var typ *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syn):
		return "the response is not JSON"
	case errors.As(err, &typ):
		return fmt.Sprintf("unexpected JSON type for %q", typ.Field)
	}
	return "the response could not be decoded"
}

// Identity is what GET /identity reports.
type Identity struct {
	MachineIdentifier string `json:"machineIdentifier"`
	Version           string `json:"version"`
	Claimed           bool   `json:"claimed"`
}

// Identity asks the server who it is. It is sent without the token (Plex answers /identity to
// anyone), so a URL that points at something other than Plex never receives the token.
func (c *Client) Identity(ctx context.Context) (Identity, error) {
	var body struct {
		MediaContainer *Identity `json:"MediaContainer"`
	}
	if err := c.get(ctx, PathIdentity, false, &body); err != nil {
		// Every Plex server serves /identity; a 404 there means something else answered.
		var perr *Error
		if errors.As(err, &perr) && perr.StatusCode == http.StatusNotFound {
			perr.Err = fmt.Errorf("%w: %s has no %s", ErrNotPlex, "the server", PathIdentity)
		}
		return Identity{}, err
	}
	if body.MediaContainer == nil || body.MediaContainer.MachineIdentifier == "" {
		return Identity{}, &Error{Method: http.MethodGet, Path: PathIdentity, StatusCode: http.StatusOK,
			Err: fmt.Errorf("%w: no machineIdentifier in the response", ErrNotPlex)}
	}
	return *body.MediaContainer, nil
}

// Section is a Plex library section.
type Section struct {
	// Key is the section id ("1").
	Key     string `json:"key"`
	Title   string `json:"title"`
	Type    string `json:"type"` // movie, show, artist, photo
	Agent   string `json:"agent"`
	Scanner string `json:"scanner"`
	// Locations are the section's folders as Plex sees them (a section can have several).
	Locations []Location `json:"locations"`
}

// Location is one folder of a section.
type Location struct {
	ID   int64  `json:"id"`
	Path string `json:"path"`
}

// flexString decodes a JSON string or number as text (Plex sends ids as either).
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if bytes.Equal(b, []byte("null")) {
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexString(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*f = flexString(n.String())
	return nil
}

// flexInt decodes a JSON number or numeric string.
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	var s flexString
	if err := s.UnmarshalJSON(b); err != nil {
		return err
	}
	if s == "" {
		return nil
	}
	n, err := strconv.ParseInt(string(s), 10, 64)
	if err != nil {
		return errors.New("not an integer")
	}
	*f = flexInt(n)
	return nil
}

type wireSections struct {
	MediaContainer *struct {
		Directory []struct {
			Key      flexString `json:"key"`
			Title    string     `json:"title"`
			Type     string     `json:"type"`
			Agent    string     `json:"agent"`
			Scanner  string     `json:"scanner"`
			Location []struct {
				ID   flexInt `json:"id"`
				Path string  `json:"path"`
			} `json:"Location"`
		} `json:"Directory"`
	} `json:"MediaContainer"`
}

// Sections lists the library sections with their locations.
func (c *Client) Sections(ctx context.Context) ([]Section, error) {
	var body wireSections
	if err := c.get(ctx, PathSections, true, &body); err != nil {
		return nil, err
	}
	if body.MediaContainer == nil {
		return nil, &Error{Method: http.MethodGet, Path: PathSections, StatusCode: http.StatusOK,
			Err: fmt.Errorf("%w: no MediaContainer in the response", ErrNotPlex)}
	}
	out := make([]Section, 0, len(body.MediaContainer.Directory))
	for _, d := range body.MediaContainer.Directory {
		s := Section{
			Key:       string(d.Key),
			Title:     d.Title,
			Type:      d.Type,
			Agent:     d.Agent,
			Scanner:   d.Scanner,
			Locations: make([]Location, 0, len(d.Location)),
		}
		for _, l := range d.Location {
			s.Locations = append(s.Locations, Location{ID: int64(l.ID), Path: l.Path})
		}
		out = append(out, s)
	}
	return out, nil
}

// Plex's defaults for the preferences Prefs reads.
const (
	DefaultButlerStartHour = 2
	DefaultButlerEndHour   = 5
)

// Prefs holds the server preferences Bunkarr uses. Preferences missing from the response take
// Plex's defaults.
type Prefs struct {
	// ButlerStartHour and ButlerEndHour bound the window (server-local hours, 0-23) in which Plex
	// runs its maintenance tasks, including its own database backup and optimization.
	ButlerStartHour int `json:"butlerStartHour"`
	ButlerEndHour   int `json:"butlerEndHour"`
	// ButlerTaskBackupDatabase is Plex's "Backup database every three days".
	ButlerTaskBackupDatabase bool `json:"butlerTaskBackupDatabase"`
	// ButlerDatabaseBackupPath is where Plex writes those backups (as Plex sees it).
	ButlerDatabaseBackupPath string `json:"butlerDatabaseBackupPath,omitempty"`
}

// InButlerWindow reports whether hour (0-23, server-local) falls inside the butler window
// [start, end). A window with start > end wraps past midnight; start == end is empty.
func (p Prefs) InButlerWindow(hour int) bool {
	s, e := p.ButlerStartHour, p.ButlerEndHour
	switch {
	case s < e:
		return hour >= s && hour < e
	case s > e:
		return hour >= s || hour < e
	}
	return false
}

type wirePrefs struct {
	MediaContainer *struct {
		Setting []struct {
			ID    string          `json:"id"`
			Value json.RawMessage `json:"value"`
		} `json:"Setting"`
	} `json:"MediaContainer"`
}

// Prefs reads GET /:/prefs.
func (c *Client) Prefs(ctx context.Context) (Prefs, error) {
	var body wirePrefs
	if err := c.get(ctx, PathPrefs, true, &body); err != nil {
		return Prefs{}, err
	}
	notPlex := func(msg string) error {
		return &Error{Method: http.MethodGet, Path: PathPrefs, StatusCode: http.StatusOK,
			Err: fmt.Errorf("%w: %s", ErrNotPlex, msg)}
	}
	if body.MediaContainer == nil {
		return Prefs{}, notPlex("no MediaContainer in the response")
	}
	p := Prefs{
		ButlerStartHour:          DefaultButlerStartHour,
		ButlerEndHour:            DefaultButlerEndHour,
		ButlerTaskBackupDatabase: true,
	}
	for _, s := range body.MediaContainer.Setting {
		if len(s.Value) == 0 {
			continue // no value: keep the default
		}
		var err error
		switch s.ID {
		case "ButlerStartHour":
			p.ButlerStartHour, err = prefHour(s.Value)
		case "ButlerEndHour":
			p.ButlerEndHour, err = prefHour(s.Value)
		case "ButlerTaskBackupDatabase":
			p.ButlerTaskBackupDatabase, err = prefBool(s.Value)
		case "ButlerDatabaseBackupPath":
			var v flexString
			err = json.Unmarshal(s.Value, &v)
			p.ButlerDatabaseBackupPath = string(v)
		default:
			continue
		}
		if err != nil {
			return Prefs{}, notPlex(fmt.Sprintf("unexpected value for %s", s.ID))
		}
	}
	return p, nil
}

func prefHour(raw json.RawMessage) (int, error) {
	var n flexInt
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, err
	}
	if n < 0 || n > 23 {
		return 0, fmt.Errorf("hour %d out of range", n)
	}
	return int(n), nil
}

func prefBool(raw json.RawMessage) (bool, error) {
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b, nil
	}
	var s flexString
	if err := json.Unmarshal(raw, &s); err != nil {
		return false, err
	}
	switch strings.ToLower(string(s)) {
	case "1", "true":
		return true, nil
	case "0", "false":
		return false, nil
	}
	return false, fmt.Errorf("not a boolean")
}

// TestResult is the outcome of Test, shaped for POST /integrations/test.
type TestResult struct {
	OK                bool   `json:"ok"`
	Message           string `json:"message"`
	Version           string `json:"version,omitempty"`
	MachineIdentifier string `json:"machineIdentifier,omitempty"`
}

// Test checks that the URL is a Plex Media Server (Identity, without the token) and that the
// token is accepted (Sections). The message is user-facing and never contains the token.
func (c *Client) Test(ctx context.Context) TestResult {
	id, err := c.Identity(ctx)
	if err != nil {
		return TestResult{Message: c.describe(err)}
	}
	res := TestResult{Version: id.Version, MachineIdentifier: id.MachineIdentifier}
	sections, err := c.Sections(ctx)
	if err != nil {
		res.Message = c.describe(err)
		return res
	}
	res.OK = true
	noun := "libraries"
	if len(sections) == 1 {
		noun = "library"
	}
	res.Message = fmt.Sprintf("Connected to Plex Media Server %s (%d %s).", id.Version, len(sections), noun)
	return res
}

// describe turns a client error into a user-facing sentence.
func (c *Client) describe(err error) string {
	var msg string
	switch {
	case errors.Is(err, ErrUnauthorized) && c.token == "":
		msg = "Plex requires a token (401 Unauthorized). Enter the server's X-Plex-Token."
	case errors.Is(err, ErrUnauthorized):
		msg = "Plex rejected the token (401 Unauthorized). Check the X-Plex-Token."
	case errors.Is(err, ErrNotPlex):
		msg = "The server at this URL did not answer like a Plex Media Server."
	case errors.Is(err, context.Canceled):
		msg = "The test was cancelled."
	case errors.Is(err, context.DeadlineExceeded):
		msg = fmt.Sprintf("Plex did not answer within %s.", c.timeout)
	default:
		var perr *Error
		if errors.As(err, &perr) && perr.StatusCode != 0 {
			msg = fmt.Sprintf("The server answered with an unexpected status (%d %s).", perr.StatusCode, http.StatusText(perr.StatusCode))
		} else {
			msg = "Could not reach Plex: " + err.Error()
		}
	}
	return c.sanitize(errors.New(msg)).Error()
}
