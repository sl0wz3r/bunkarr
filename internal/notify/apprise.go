package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/logging"
	"github.com/sl0wz3r/bunkarr/internal/netguard"
	"github.com/sl0wz3r/bunkarr/internal/version"
)

// Client defaults.
const (
	// DefaultTimeout bounds one request to the Apprise API.
	DefaultTimeout = 15 * time.Second
	// DefaultRetryDelay is the pause before the single retry after a 5xx or network error.
	DefaultRetryDelay = 2 * time.Second
	// maxResponseBytes is how much of a response body is read (and discarded).
	maxResponseBytes = 64 << 10
)

// Client posts messages to an Apprise API server. It is safe for concurrent use.
type Client struct {
	http       *http.Client
	timeout    time.Duration
	retryDelay time.Duration
}

// NewClient returns a client with DefaultTimeout per request and one retry after
// DefaultRetryDelay. It never follows redirects, so the URLs in a request body only ever go to
// the configured Apprise API. Its transport dials through internal/netguard (design S16), so an
// API URL that is, or resolves to, a link-local or cloud metadata address is refused before any
// byte is sent (also through an HTTP(S) proxy).
func NewClient() *Client {
	return &Client{
		http: &http.Client{
			Transport: netguard.NewTransport(),
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		timeout:    DefaultTimeout,
		retryDelay: DefaultRetryDelay,
	}
}

// SendError is a failed delivery. Its message names the Apprise API URL (which never holds
// credentials) and the reason; it never contains the Apprise URLs, request bodies or response
// bodies, and it is passed through logging.RedactSecrets.
type SendError struct {
	// StatusCode is Apprise's HTTP status, or 0 when no response arrived (network error, timeout,
	// cancellation).
	StatusCode int
	msg        string
	err        error
	// maybeSent: the message may have reached some services (a 424, or an attempt that failed
	// after the whole request was written, e.g. a timeout). False only when nothing can have been
	// sent: refused before sending (dial, DNS, netguard) or answered 204, 3xx, 4xx other than 424,
	// or 5xx.
	maybeSent bool
}

// maybeDelivered reports whether err, from Send, leaves open that the message reached someone.
func maybeDelivered(err error) bool {
	var se *SendError
	return errors.As(err, &se) && se.maybeSent
}

// Error returns the redacted, user-facing message.
func (e *SendError) Error() string { return e.msg }

// Unwrap returns the underlying network or context error, if any (never a *url.Error, whose
// message contains the request URL).
func (e *SendError) Unwrap() error { return e.err }

// statelessPayload is the body of POST {apiUrl}/notify.
type statelessPayload struct {
	URLs  string      `json:"urls"`
	Title string      `json:"title"`
	Body  string      `json:"body"`
	Type  MessageType `json:"type"`
}

// statefulPayload is the body of POST {apiUrl}/notify/{configKey}.
type statefulPayload struct {
	Title string      `json:"title"`
	Body  string      `json:"body"`
	Type  MessageType `json:"type"`
}

// Send delivers m to t: stateless POST {apiUrl}/notify {urls, title, body, type}, or stateful
// POST {apiUrl}/notify/{configKey} {title, body, type}. A 5xx response or a network error
// (including the per-request timeout) is retried once; other failures are not (a 424 means some
// services already got the message). Errors are a ValidationError for a bad target or message,
// otherwise a *SendError; maybeDelivered tells whether one may still have reached some services.
func (c *Client) Send(ctx context.Context, t Target, m Message) error {
	endpoint, apiURL, err := t.endpoint()
	if err != nil {
		return err
	}
	if !m.Type.Valid() {
		return ValidationError(fmt.Sprintf("message type must be one of info, success, warning, failure (got %q)", m.Type))
	}
	if strings.TrimSpace(m.Body) == "" {
		return ValidationError("message body is required")
	}
	var payload any = statefulPayload{Title: m.Title, Body: m.Body, Type: m.Type}
	if !t.Stateful() {
		payload = statelessPayload{URLs: t.URLs, Title: m.Title, Body: m.Body, Type: m.Type}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		// Cannot happen for these string-only types; the error never quotes string values.
		return fmt.Errorf("encode Apprise request: %w", err)
	}
	body := buf.Bytes()

	const attempts = 2
	maybeSent := false
	for attempt := 1; ; attempt++ {
		retry, serr := c.post(ctx, endpoint, apiURL, body)
		if serr == nil {
			return nil
		}
		// A retry refused after an attempt that may have been delivered is still maybe delivered.
		maybeSent = maybeSent || serr.maybeSent
		serr.maybeSent = maybeSent
		if attempt > 1 {
			serr.msg += " (after one retry)"
		}
		if !retry || attempt == attempts || ctx.Err() != nil {
			return serr
		}
		timer := time.NewTimer(c.retryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return serr
		case <-timer.C:
		}
	}
}

// post makes one request. retry reports whether the failure is worth one more attempt.
func (c *Client) post(ctx context.Context, endpoint, apiURL string, body []byte) (retry bool, _ *SendError) {
	fail := func(status int, err error, format string, args ...any) *SendError {
		msg := "apprise " + apiURL + ": " + fmt.Sprintf(format, args...)
		return &SendError{StatusCode: status, msg: logging.RedactSecrets(msg), err: err}
	}
	actx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	// wrote: the whole request went out, so a failure after it (no answer in time, a reset,
	// cancellation) may follow a delivery. Set on the transport's goroutine.
	var wrote atomic.Bool
	tctx := httptrace.WithClientTrace(actx, &httptrace.ClientTrace{WroteRequest: func(i httptrace.WroteRequestInfo) {
		if i.Err == nil {
			wrote.Store(true)
		}
	}})
	req, err := http.NewRequestWithContext(tctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		// The parse error would quote the URL; the API URL was validated, so this is unexpected.
		return false, fail(0, nil, "invalid request URL")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", version.UserAgent())

	resp, err := c.http.Do(req)
	if err != nil {
		retry, serr := c.transportError(ctx, actx, err, apiURL)
		serr.maybeSent = wrote.Load()
		return retry, serr
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
	_ = resp.Body.Close()

	code := resp.StatusCode
	status := fmt.Sprintf("%d %s", code, http.StatusText(code))
	switch {
	case code == http.StatusNoContent:
		return false, fail(code, nil, "nothing was sent (%s): Apprise has no configuration or URLs for this target", status)
	case code >= 200 && code < 300:
		return false, nil
	case code >= 300 && code < 400:
		return false, fail(code, nil, "unexpected redirect (%s); check apiUrl (redirects are not followed)", status)
	case code == http.StatusBadRequest:
		return false, fail(code, nil, "request rejected (%s); check the Apprise URLs", status)
	case code == http.StatusNotFound:
		return false, fail(code, nil, "not found (%s); check apiUrl points to the Apprise API", status)
	case code == http.StatusFailedDependency:
		serr := fail(code, nil, "at least one service could not be notified (%s); see the Apprise logs", status)
		serr.maybeSent = true // the key's other services got it
		return false, serr
	case code >= 500:
		return true, fail(code, nil, "server error (%s)", status)
	default:
		return false, fail(code, nil, "unexpected response (%s)", status)
	}
}

// transportError maps an error of http.Client.Do (no response) to a *SendError. retry reports
// whether the failure is worth one more attempt.
func (c *Client) transportError(ctx, actx context.Context, err error, apiURL string) (retry bool, _ *SendError) {
	fail := func(err error, format string, args ...any) *SendError {
		msg := "apprise " + apiURL + ": " + fmt.Sprintf(format, args...)
		return &SendError{msg: logging.RedactSecrets(msg), err: err}
	}
	switch {
	case ctx.Err() != nil:
		return false, fail(ctx.Err(), "stopped: %s", ctx.Err())
	case errors.Is(actx.Err(), context.DeadlineExceeded):
		return true, fail(context.DeadlineExceeded, "no response within %s", c.timeout)
	case errors.Is(err, netguard.ErrBlocked):
		// Refused by the outbound guard (S16) before anything was sent; a retry cannot help.
		return false, fail(netguard.ErrBlocked, "Bunkarr does not connect to this address (a link-local or cloud metadata address)")
	}
	// *url.Error quotes the request URL; keep only its cause (dial, DNS, TLS, reset, ...).
	cause := err
	var uerr *url.Error
	if errors.As(err, &uerr) && uerr.Err != nil {
		cause = uerr.Err
	}
	return true, fail(cause, "cannot reach the Apprise API: %s", cause.Error())
}

// endpoint validates t and returns the request URL and the validated API base URL.
func (t Target) endpoint() (endpoint, apiURL string, err error) {
	apiURL, err = validAPIURL(t.APIURL)
	if err != nil {
		return "", "", err
	}
	if t.Stateful() {
		if !configKeyRE.MatchString(t.ConfigKey) {
			return "", "", ValidationError("configKey must be 1-64 letters, digits, '_' or '-'")
		}
		return apiURL + "/notify/" + t.ConfigKey, apiURL, nil
	}
	if strings.TrimSpace(t.URLs) == "" {
		return "", "", ValidationError("urls are required unless configKey is set")
	}
	return apiURL + "/notify", apiURL, nil
}
