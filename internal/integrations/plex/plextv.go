package plex

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// plex.tv (design §5, S19). Sign-in is a strong PIN that the user approves on app.plex.tv; the
// PIN's check returns the account token, which lists the account's servers (resources), each with
// its own access token. These helpers talk only to plex.tv and clients.plex.tv over HTTPS (http
// only for a loopback test server), never follow a redirect, send a token only in the
// X-Plex-Token header, and never put a response body into an error. The tokens they return are for
// the sign-in registry (internal/api) only: none of them is ever sent to the browser.

// The plex.tv endpoints. Only tests (and binaries built with -tags e2e) replace the first two,
// through Options.PlexTVURL and Options.ClientsPlexTVURL.
const (
	// PlexTVURL serves the PINs and the account (api/v2/pins, api/v2/user).
	PlexTVURL = "https://plex.tv"
	// ClientsPlexTVURL serves the account's resources (api/v2/resources).
	ClientsPlexTVURL = "https://clients.plex.tv"
	// AuthAppURL is the page the user opens to approve a PIN (see AuthURL).
	AuthAppURL = "https://app.plex.tv/auth"
)

// maxPlexTVBody bounds a plex.tv response.
const maxPlexTVBody = 8 << 20

// Pin is a plex.tv sign-in PIN. None of its fields is ever serialized: the PIN id never leaves
// the server, and the code only inside AuthURL.
type Pin struct {
	ID        int64     `json:"-"`
	Code      string    `json:"-"`
	AuthToken string    `json:"-"`
	ExpiresAt time.Time `json:"-"`
}

// Account is the plex.tv account a token belongs to. Only the user name is decoded (never the
// e-mail address, the token or the subscription).
type Account struct {
	Username string
}

// Resource is a plex.tv resource that provides "server". AccessToken is the server's own token;
// it is never serialized.
type Resource struct {
	Name             string       `json:"name"`
	ClientIdentifier string       `json:"clientIdentifier"`
	ProductVersion   string       `json:"productVersion"`
	Platform         string       `json:"platform"`
	Owned            bool         `json:"owned"`
	AccessToken      string       `json:"-"`
	Connections      []Connection `json:"connections"`
}

// Connection is one way plex.tv says a Resource can be reached.
type Connection struct {
	URI      string `json:"uri"`
	Address  string `json:"address"`
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
	Local    bool   `json:"local"`
	Relay    bool   `json:"relay"`
	IPv6     bool   `json:"ipv6"`
}

// CreatePin creates a strong plex.tv PIN (POST https://plex.tv/api/v2/pins?strong=true, without a
// token). A strong PIN is meant for the app.plex.tv auth page (AuthURL). CheckPin must use the same
// client identifier.
func CreatePin(ctx context.Context, opts Options) (Pin, error) {
	opts = opts.withDefaults()
	const p = "/api/v2/pins"
	body, err := tvDo(ctx, opts, svcPlexTV, http.MethodPost, p, p, url.Values{"strong": {"true"}}, "")
	if err != nil {
		return Pin{}, err
	}
	pin, err := decodePin(body, time.Now())
	if err != nil {
		return Pin{}, tvError(svcPlexTV, http.MethodPost, p, http.StatusCreated, err)
	}
	if pin.ID <= 0 || pin.Code == "" {
		return Pin{}, tvError(svcPlexTV, http.MethodPost, p, http.StatusCreated, errors.New("the response has no PIN id or code"))
	}
	return pin, nil
}

// CheckPin reads a PIN (GET https://plex.tv/api/v2/pins/{id}?code=<code>, with the same client
// identifier, as Plex documents the check). AuthToken is "" until the user approves it. A PIN that
// expired or does not exist returns an error matching ErrNotFound. Errors name the path as
// "/api/v2/pins/{id}": the PIN id is never revealed.
func CheckPin(ctx context.Context, opts Options, id int64, code string) (Pin, error) {
	opts = opts.withDefaults()
	code = strings.TrimSpace(code)
	if id <= 0 || code == "" {
		return Pin{}, fmt.Errorf("%w: a PIN id and code are required", ErrInvalidArgument)
	}
	const label = "/api/v2/pins/{id}"
	body, err := tvDo(ctx, opts, svcPlexTV, http.MethodGet, "/api/v2/pins/"+strconv.FormatInt(id, 10), label, url.Values{"code": {code}}, "")
	if err != nil {
		return Pin{}, err
	}
	pin, err := decodePin(body, time.Now())
	if err != nil {
		return Pin{}, tvError(svcPlexTV, http.MethodGet, label, http.StatusOK, err)
	}
	if pin.ID == 0 {
		pin.ID = id
	}
	if pin.ID != id {
		return Pin{}, tvError(svcPlexTV, http.MethodGet, label, http.StatusOK, errors.New("the response is for another PIN"))
	}
	return pin, nil
}

// User reads the account a token belongs to (GET https://plex.tv/api/v2/user). Only the user
// name is decoded.
func User(ctx context.Context, opts Options, token string) (Account, error) {
	opts = opts.withDefaults()
	token = strings.TrimSpace(token)
	if token == "" {
		return Account{}, fmt.Errorf("%w: a plex.tv token is required", ErrInvalidArgument)
	}
	const p = "/api/v2/user"
	body, err := tvDo(ctx, opts, svcPlexTV, http.MethodGet, p, p, nil, token)
	if err != nil {
		return Account{}, err
	}
	name, err := decodeUsername(body)
	if err != nil {
		return Account{}, tvError(svcPlexTV, http.MethodGet, p, http.StatusOK, err)
	}
	return Account{Username: name}, nil
}

// AuthURL returns the https://app.plex.tv/auth#?... address the user opens to approve the PIN
// with code: clientID, code and context[device][product], in the official "auth#?" form (values
// percent-encoded, spaces as %20).
func AuthURL(opts Options, code string) string {
	opts = opts.withDefaults()
	return AuthAppURL + "#?clientID=" + fragmentEscape(opts.ClientIdentifier) +
		"&code=" + fragmentEscape(strings.TrimSpace(code)) +
		"&context%5Bdevice%5D%5Bproduct%5D=" + fragmentEscape(opts.Product)
}

func fragmentEscape(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }

// Resources lists the account's servers (GET https://clients.plex.tv/api/v2/resources
// ?includeHttps=1&includeRelay=1&includeIPv6=1, token in the header) with their access tokens and
// connections. Resources whose "provides" does not include "server" are dropped. The JSON shape is
// documented only by the community, so the decoder accepts an array, a wrapped object or a single
// object, and XML.
func Resources(ctx context.Context, opts Options, token string) ([]Resource, error) {
	opts = opts.withDefaults()
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("%w: a plex.tv token is required", ErrInvalidArgument)
	}
	const p = "/api/v2/resources"
	q := url.Values{"includeHttps": {"1"}, "includeRelay": {"1"}, "includeIPv6": {"1"}}
	body, err := tvDo(ctx, opts, svcClients, http.MethodGet, p, p, q, token)
	if err != nil {
		return nil, err
	}
	dtos, err := decodeResources(body)
	if err != nil {
		// Never include the body: it carries access tokens.
		return nil, tvError(svcClients, http.MethodGet, p, http.StatusOK, err)
	}
	out := make([]Resource, 0, len(dtos))
	for _, d := range dtos {
		if !providesServer(d.Provides.String()) {
			continue
		}
		r := Resource{
			Name:             d.Name.String(),
			ClientIdentifier: d.ClientIdentifier.String(),
			ProductVersion:   d.ProductVersion.String(),
			Platform:         d.Platform.String(),
			Owned:            bool(d.Owned),
			AccessToken:      d.AccessToken.String(),
			Connections:      make([]Connection, 0, len(d.Connections)+len(d.Connection)),
		}
		conns := d.Connections
		if len(conns) == 0 {
			conns = d.Connection
		}
		for _, c := range conns {
			r.Connections = append(r.Connections, Connection{
				URI:      c.URI.String(),
				Address:  c.Address.String(),
				Port:     c.Port.Int(),
				Protocol: strings.ToLower(c.Protocol.String()),
				Local:    bool(c.Local),
				Relay:    bool(c.Relay),
				IPv6:     bool(c.IPv6),
			})
		}
		out = append(out, r)
	}
	return out, nil
}

func providesServer(provides string) bool {
	for p := range strings.SplitSeq(provides, ",") {
		if strings.EqualFold(strings.TrimSpace(p), "server") {
			return true
		}
	}
	return false
}

// The plex.tv services, as errors name them.
const (
	svcPlexTV  = "plex.tv"
	svcClients = "clients.plex.tv"
)

// tvBase returns the base URL of svc.
func tvBase(opts Options, svc string) string {
	if svc == svcClients {
		return opts.ClientsPlexTVURL
	}
	return opts.PlexTVURL
}

func tvError(svc, method, label string, status int, err error) error {
	return &Error{Service: svc, Method: method, Path: label, StatusCode: status, Err: err}
}

// checkTVBase refuses a plex.tv base URL that is not https, unless it is http on a loopback host
// (a test server).
func checkTVBase(base string) (*url.URL, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%w: invalid plex.tv base URL", ErrInvalidArgument)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopbackHost(u.Hostname()) {
			return nil, fmt.Errorf("%w: plex.tv must be reached over https", ErrInvalidArgument)
		}
	default:
		return nil, fmt.Errorf("%w: invalid plex.tv base URL", ErrInvalidArgument)
	}
	return u, nil
}

func isLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(h)
	return err == nil && ip.Unmap().IsLoopback()
}

// tvDo performs one request to svc (plex.tv or clients.plex.tv) and returns the body. p is the
// request path, label the path its errors name. TLS is verified (unless the caller's HTTPClient says otherwise), redirects are
// never followed, and error bodies are never read into an error.
func tvDo(ctx context.Context, opts Options, svc, method, p, label string, q url.Values, token string) ([]byte, error) {
	fail := func(status int, cause error) error { return tvError(svc, method, label, status, cause) }
	u, err := checkTVBase(tvBase(opts, svc))
	if err != nil {
		return nil, err
	}
	if strings.ContainsAny(token, "\r\n\x00") {
		return nil, fmt.Errorf("%w: the token contains invalid characters", ErrInvalidArgument)
	}
	u.Path = strings.TrimRight(u.Path, "/") + p
	u.RawPath = ""
	u.RawQuery = q.Encode()
	if err := ctx.Err(); err != nil {
		return nil, fail(0, err)
	}
	rctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, method, u.String(), nil)
	if err != nil {
		return nil, fail(0, errors.New("cannot build the request"))
	}
	opts.setHeaders(req.Header, token)
	start := time.Now()
	resp, err := httpClient(opts.HTTPClient).Do(req)
	if err != nil {
		return nil, fail(0, transportCause(ctx, err, opts.Timeout))
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
	}()
	if opts.Logger != nil {
		opts.Logger.Debug("plex.tv request", "method", method, "path", label, "status", resp.StatusCode, "durationMs", time.Since(start).Milliseconds())
	}
	switch code := resp.StatusCode; {
	case code == http.StatusUnauthorized:
		return nil, fail(code, ErrUnauthorized)
	case code == http.StatusNotFound:
		return nil, fail(code, ErrNotFound)
	case code >= 300 && code < 400:
		return nil, fail(code, fmt.Errorf("unexpected redirect (%d); redirects are not followed", code))
	case code < 200 || code > 299:
		return nil, fail(code, fmt.Errorf("unexpected status %d %s", code, http.StatusText(code)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPlexTVBody+1))
	if err != nil {
		return nil, fail(resp.StatusCode, transportCause(ctx, err, opts.Timeout))
	}
	if len(body) > maxPlexTVBody {
		return nil, fail(resp.StatusCode, fmt.Errorf("response larger than %d bytes", maxPlexTVBody))
	}
	return body, nil
}

// transportCause reduces a client error to a cause that holds no URL, query or header: the
// *url.Error wrapper (which carries the full URL) is dropped, a network error is rebuilt from its
// Op and Err (keeping the chain for errors.Is and errors.As), and timeouts are named.
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

// ---------------------------------------------------------------------------
// PIN and user decoding
// ---------------------------------------------------------------------------

type pinDTO struct {
	ID        flexInt    `json:"id"`
	Code      flexString `json:"code"`
	AuthToken flexString `json:"authToken"` // null until claimed
	ExpiresAt flexString `json:"expiresAt"`
	ExpiresIn flexInt    `json:"expiresIn"`
}

// pinXML covers the v2 attribute form (<pin id=".." code=".." authToken=".."/>) and the legacy
// element form (<pin><id>..</id><code>..</code><auth-token>..</auth-token></pin>).
type pinXML struct {
	ID           string `xml:"id,attr"`
	Code         string `xml:"code,attr"`
	AuthToken    string `xml:"authToken,attr"`
	ExpiresAt    string `xml:"expiresAt,attr"`
	ExpiresIn    string `xml:"expiresIn,attr"`
	IDEl         string `xml:"id"`
	CodeEl       string `xml:"code"`
	AuthTokenEl  string `xml:"auth-token"`
	AuthTokenEl2 string `xml:"authToken"`
	ExpiresAtEl  string `xml:"expires-at"`
}

func decodePin(body []byte, now time.Time) (Pin, error) {
	b := trimBody(body)
	if len(b) == 0 {
		return Pin{}, errors.New("empty response body")
	}
	var d pinDTO
	if b[0] == '<' {
		var x pinXML
		if err := xml.Unmarshal(b, &x); err != nil {
			return Pin{}, errors.New("the response is not a PIN (XML)")
		}
		d = pinDTO{
			ID:        flexInt(parseIntText(firstNonEmpty(x.ID, x.IDEl))),
			Code:      flexString(firstNonEmpty(x.Code, x.CodeEl)),
			AuthToken: flexString(firstNonEmpty(x.AuthToken, x.AuthTokenEl, x.AuthTokenEl2)),
			ExpiresAt: flexString(firstNonEmpty(x.ExpiresAt, x.ExpiresAtEl)),
			ExpiresIn: flexInt(parseIntText(x.ExpiresIn)),
		}
	} else if err := json.Unmarshal(b, &d); err != nil {
		return Pin{}, errors.New("the response is not a PIN")
	}
	p := Pin{
		ID:        int64(d.ID),
		Code:      d.Code.String(),
		AuthToken: d.AuthToken.String(),
		ExpiresAt: parsePlexTime(d.ExpiresAt.String()),
	}
	if p.ExpiresAt.IsZero() && d.ExpiresIn > 0 {
		p.ExpiresAt = now.Add(time.Duration(d.ExpiresIn) * time.Second).UTC()
	}
	return p, nil
}

// parsePlexTime parses an RFC 3339 timestamp or a Unix epoch (seconds); zero when unparseable.
func parsePlexTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC()
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
		return unixTime(flexInt(n))
	}
	return time.Time{}
}

// decodeUsername reads only the user name of an api/v2/user answer (JSON {"username": ...}, or XML
// <user username=".."/> or <user><username>..</username></user>).
func decodeUsername(body []byte) (string, error) {
	b := trimBody(body)
	if len(b) == 0 {
		return "", errors.New("empty response body")
	}
	if b[0] == '<' {
		var x struct {
			Username   string `xml:"username,attr"`
			UsernameEl string `xml:"username"`
		}
		if err := xml.Unmarshal(b, &x); err != nil {
			return "", errors.New("the response is not a plex.tv user (XML)")
		}
		return strings.TrimSpace(firstNonEmpty(x.Username, x.UsernameEl)), nil
	}
	var d struct {
		Username flexString `json:"username"`
	}
	if b[0] != '{' || json.Unmarshal(b, &d) != nil {
		return "", errors.New("the response is not a plex.tv user")
	}
	return d.Username.String(), nil
}

// ---------------------------------------------------------------------------
// Resource decoding
// ---------------------------------------------------------------------------

type resourceDTO struct {
	Name             flexString          `json:"name"`
	ProductVersion   flexString          `json:"productVersion"`
	Platform         flexString          `json:"platform"`
	ClientIdentifier flexString          `json:"clientIdentifier"`
	Provides         flexString          `json:"provides"`
	Owned            flexBool            `json:"owned"`
	AccessToken      flexString          `json:"accessToken"`
	Connections      list[connectionDTO] `json:"connections"`
	Connection       list[connectionDTO] `json:"Connection"` // legacy / XML-converted shape
}

type connectionDTO struct {
	Protocol flexString `json:"protocol"`
	Address  flexString `json:"address"`
	Port     flexInt    `json:"port"`
	URI      flexString `json:"uri"`
	Local    flexBool   `json:"local"`
	Relay    flexBool   `json:"relay"`
	IPv6     flexBool   `json:"IPv6"`
}

// decodeResources accepts a top-level JSON array, an object wrapping it ({"MediaContainer":
// {"Device": [...]}}, {"Device": [...]}, {"resources": [...]}) or a single resource object, and
// XML (<MediaContainer><Device …><Connection …/></Device></MediaContainer> or
// <resources><resource …><connections><connection …/></connections></resource></resources>).
// Its errors never quote the body.
func decodeResources(body []byte) ([]resourceDTO, error) {
	b := trimBody(body)
	if len(b) == 0 {
		return nil, errors.New("empty response body")
	}
	switch b[0] {
	case '[':
		var l list[resourceDTO]
		if err := json.Unmarshal(b, &l); err != nil {
			return nil, errors.New("the response is not a resource list")
		}
		return l, nil
	case '{':
		var obj struct {
			MediaContainer *struct {
				Device list[resourceDTO] `json:"Device"`
			} `json:"MediaContainer"`
			Device    list[resourceDTO] `json:"Device"`
			Resources list[resourceDTO] `json:"resources"`
		}
		if err := json.Unmarshal(b, &obj); err != nil {
			return nil, errors.New("the response is not a resource list")
		}
		var out []resourceDTO
		if obj.MediaContainer != nil {
			out = append(out, obj.MediaContainer.Device...)
		}
		out = append(out, obj.Device...)
		out = append(out, obj.Resources...)
		if len(out) == 0 {
			var one resourceDTO
			if err := json.Unmarshal(b, &one); err == nil && one.ClientIdentifier.String() != "" {
				out = append(out, one)
			}
		}
		return out, nil
	case '<':
		return decodeResourcesXML(b)
	default:
		return nil, errors.New("unrecognized response format")
	}
}

func decodeResourcesXML(b []byte) ([]resourceDTO, error) {
	dec := xml.NewDecoder(bytes.NewReader(b))
	var out []resourceDTO
	var cur *resourceDTO
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, errors.New("the response is not a resource list (XML)")
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch strings.ToLower(t.Name.Local) {
			case "device", "resource":
				r := resourceFromAttrs(t.Attr)
				cur = &r
			case "connection":
				if cur != nil {
					cur.Connections = append(cur.Connections, connectionFromAttrs(t.Attr))
				}
			}
		case xml.EndElement:
			switch strings.ToLower(t.Name.Local) {
			case "device", "resource":
				if cur != nil {
					out = append(out, *cur)
					cur = nil
				}
			}
		}
	}
	return out, nil
}

func attrMap(attrs []xml.Attr) map[string]string {
	m := make(map[string]string, len(attrs))
	for _, a := range attrs {
		m[strings.ToLower(a.Name.Local)] = a.Value
	}
	return m
}

func resourceFromAttrs(attrs []xml.Attr) resourceDTO {
	m := attrMap(attrs)
	return resourceDTO{
		Name:             flexString(m["name"]),
		ProductVersion:   flexString(m["productversion"]),
		Platform:         flexString(m["platform"]),
		ClientIdentifier: flexString(m["clientidentifier"]),
		Provides:         flexString(m["provides"]),
		Owned:            flexBool(parseBoolText(m["owned"])),
		AccessToken:      flexString(m["accesstoken"]),
	}
}

func connectionFromAttrs(attrs []xml.Attr) connectionDTO {
	m := attrMap(attrs)
	return connectionDTO{
		Protocol: flexString(m["protocol"]),
		Address:  flexString(m["address"]),
		Port:     flexInt(parseIntText(m["port"])),
		URI:      flexString(m["uri"]),
		Local:    flexBool(parseBoolText(m["local"])),
		Relay:    flexBool(parseBoolText(m["relay"])),
		IPv6:     flexBool(parseBoolText(m["ipv6"])),
	}
}
