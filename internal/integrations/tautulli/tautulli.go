// Package tautulli is a read-only client for the Tautulli API v2: the requests of
// docs/design/phase2-3.md §4.4 and nothing else.
//
// Allow-list (S16). The client has no general request method. Each exported method sends one
// fixed command: get_tautulli_info, get_server_info, get_library (for keep_history), get_users
// (only user_id and keep_history are decoded) and get_history with the refresh's parameters. It
// never sends refresh=true or get_library_media_info.
//
// Safety (S8). The API key is sent only in the X-Api-Key header, never in a URL. Tautulli before
// 2.18.0 accepts the key only in a query string, so it is refused (ErrTooOld). Redirects are not
// followed; errors carry the method, the path and a cause, never a URL, a query, a header or a
// response body. The transport dials through the outbound guard (internal/netguard).
//
// Decoding. Every answer is the envelope {"response":{"result","message","data"}}; strings may be
// HTML-escaped and numbers may arrive as strings. 401 means a bad key, 404 that the API is
// disabled and 400 a bad command (or, from 2.17, a key sent only in the header).
//
// History paging (ported from Dupearr). Rows are deduplicated by row_id; the total comes from the
// first page (recordsFiltered); the read fails when a later page reports a smaller total, or when
// a page comes back short before the total is reached. At most MaxHistoryRows rows are read.
package tautulli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/integrations/httpread"
)

// Limits and defaults.
const (
	// APIPath is the API root under the base URL.
	APIPath = "/api/v2"
	// DefaultPageSize is the length of a get_history page.
	DefaultPageSize = 1000
	// MaxHistoryRows bounds the history rows one refresh reads (all sections together).
	MaxHistoryRows = 250000
	// HistoryMediaTypes is the media_type filter of get_history.
	HistoryMediaTypes = "movie,episode,track"
)

// MinVersion is the oldest supported Tautulli: 2.18.0 accepts the key in the X-Api-Key header.
var MinVersion = httpread.Version{2, 18, 0}

var (
	// ErrUnauthorized means Tautulli answered 401: the API key is missing or wrong.
	ErrUnauthorized = errors.New("the API key was rejected (401)")
	// ErrAPIDisabled means Tautulli answered 404: its API is turned off (Settings → Web Interface
	// → Enable API).
	ErrAPIDisabled = errors.New("the Tautulli API is not enabled (404)")
	// ErrBadCommand means Tautulli answered 400 or an error envelope for a command.
	ErrBadCommand = errors.New("Tautulli refused the command")
	// ErrTooOld means the server is older than MinVersion (or answered like 2.17, which wants the
	// key in the URL).
	ErrTooOld = errors.New("Tautulli 2.18.0 or newer is required: older versions accept the API key only in the URL")
	// ErrNotTautulli means the server answered, but not like the Tautulli API.
	ErrNotTautulli = errors.New("the server did not answer like the Tautulli API")
	// ErrPaging means the history changed while it was read (a smaller total or a short page).
	ErrPaging = errors.New("the Tautulli history changed while it was read")
	// ErrTooManyRows means the history has more than MaxHistoryRows rows.
	ErrTooManyRows = fmt.Errorf("the Tautulli history has more than %d rows", MaxHistoryRows)
)

// Options configures New. Zero values take the defaults.
type Options struct {
	httpread.Options
	// PageSize overrides DefaultPageSize (tests).
	PageSize int
	// MaxRows overrides MaxHistoryRows (tests).
	MaxRows int
}

// Client talks to one Tautulli. It is safe for concurrent use.
type Client struct {
	c        *httpread.Client
	pageSize int
	maxRows  int
}

// New returns a client for the Tautulli at baseURL with apiKey.
func New(baseURL, apiKey string, o Options) (*Client, error) {
	c, err := httpread.New("Tautulli", baseURL, apiKey, o.Options)
	if err != nil {
		return nil, err
	}
	cl := &Client{c: c, pageSize: o.PageSize, maxRows: o.MaxRows}
	if cl.pageSize <= 0 {
		cl.pageSize = DefaultPageSize
	}
	if cl.maxRows <= 0 {
		cl.maxRows = MaxHistoryRows
	}
	return cl, nil
}

// envelope is Tautulli's answer.
type envelope[T any] struct {
	Response *struct {
		Result  string        `json:"result"`
		Message httpread.Text `json:"message"`
		Data    T             `json:"data"`
	} `json:"response"`
}

// command sends one allow-listed command and decodes its data into out.
func command[T any](ctx context.Context, c *Client, query url.Values) (T, error) {
	var zero T
	r := httpread.Request{Path: APIPath, Query: query, WithKey: true}
	resp, err := c.c.Get(ctx, r)
	if err != nil {
		return zero, err
	}
	var env envelope[T]
	decodeErr := httpread.DecodeJSON(resp.Body, &env)
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return zero, c.c.Fail(r, resp.StatusCode, ErrUnauthorized)
	case resp.StatusCode == http.StatusNotFound:
		return zero, c.c.Fail(r, resp.StatusCode, ErrAPIDisabled)
	case resp.StatusCode == http.StatusBadRequest:
		// 2.17 answers a key sent only in the header with "Parameter apikey is required".
		if decodeErr == nil && env.Response != nil && strings.Contains(strings.ToLower(env.Response.Message.String()), "apikey") {
			return zero, c.c.Fail(r, resp.StatusCode, ErrTooOld)
		}
		return zero, c.c.Fail(r, resp.StatusCode, ErrBadCommand)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return zero, c.c.Fail(r, resp.StatusCode, fmt.Errorf("unexpected status %d %s", resp.StatusCode, http.StatusText(resp.StatusCode)))
	case decodeErr != nil:
		return zero, c.c.Fail(r, resp.StatusCode, fmt.Errorf("%w: %w", ErrNotTautulli, decodeErr))
	case env.Response == nil:
		return zero, c.c.Fail(r, resp.StatusCode, fmt.Errorf("%w: no response envelope", ErrNotTautulli))
	case env.Response.Result != "success":
		return zero, c.c.Fail(r, resp.StatusCode, ErrBadCommand)
	}
	return env.Response.Data, nil
}

func cmd(name string, kv ...string) url.Values {
	q := url.Values{"cmd": {name}}
	for i := 0; i+1 < len(kv); i += 2 {
		q.Set(kv[i], kv[i+1])
	}
	return q
}

// Info is get_tautulli_info.
type Info struct {
	// Version is as Tautulli reports it ("v2.18.1").
	Version string
}

// Info reads get_tautulli_info and requires MinVersion (ErrTooOld otherwise).
func (c *Client) Info(ctx context.Context) (Info, error) {
	data, err := command[struct {
		Version httpread.Text `json:"tautulli_version"`
	}](ctx, c, cmd("get_tautulli_info"))
	if err != nil {
		return Info{}, err
	}
	info := Info{Version: data.Version.String()}
	v, ok := httpread.ParseVersion(info.Version)
	r := httpread.Request{Path: APIPath}
	switch {
	case !ok:
		return info, c.c.Fail(r, http.StatusOK, fmt.Errorf("%w: no tautulli_version", ErrNotTautulli))
	case v.Less(MinVersion):
		return info, c.c.Fail(r, http.StatusOK, ErrTooOld)
	}
	return info, nil
}

// ServerInfo is get_server_info: the Plex server Tautulli watches.
type ServerInfo struct {
	// PMSIdentifier is the server's machineIdentifier.
	PMSIdentifier string
	PMSName       string
}

// ServerInfo reads get_server_info.
func (c *Client) ServerInfo(ctx context.Context) (ServerInfo, error) {
	data, err := command[struct {
		Identifier httpread.Text `json:"pms_identifier"`
		Name       httpread.Text `json:"pms_name"`
	}](ctx, c, cmd("get_server_info"))
	if err != nil {
		return ServerInfo{}, err
	}
	return ServerInfo{PMSIdentifier: data.Identifier.String(), PMSName: data.Name.String()}, nil
}

// Library is get_library of one section: whether Tautulli keeps its history.
type Library struct {
	SectionID string
	// Known is false when Tautulli does not know the section.
	Known       bool
	KeepHistory bool
}

// Library reads get_library&section_id=<sectionID>.
func (c *Client) Library(ctx context.Context, sectionID string) (Library, error) {
	if !validSection(sectionID) {
		return Library{}, fmt.Errorf("tautulli: invalid section id %q", sectionID)
	}
	data, err := command[struct {
		SectionID   httpread.Int `json:"section_id"`
		KeepHistory httpread.Int `json:"keep_history"`
	}](ctx, c, cmd("get_library", "section_id", sectionID))
	if err != nil {
		return Library{}, err
	}
	out := Library{SectionID: sectionID, Known: data.SectionID.Valid}
	out.KeepHistory = out.Known && (!data.KeepHistory.Valid || data.KeepHistory.V != 0)
	return out, nil
}

// User is a Tautulli user: only its id and whether its history is kept are decoded (no names or
// e-mail addresses).
type User struct {
	UserID      int64
	KeepHistory bool
}

// Users reads get_users.
func (c *Client) Users(ctx context.Context) ([]User, error) {
	data, err := command[[]struct {
		UserID      httpread.Int `json:"user_id"`
		KeepHistory httpread.Int `json:"keep_history"`
	}](ctx, c, cmd("get_users"))
	if err != nil {
		return nil, err
	}
	out := make([]User, 0, len(data))
	for _, u := range data {
		out = append(out, User{UserID: u.UserID.V, KeepHistory: !u.KeepHistory.Valid || u.KeepHistory.V != 0})
	}
	return out, nil
}

// Play is one get_history row (grouping=0: one row per session).
type Play struct {
	RowID     int64
	RatingKey string
	GUID      string
	// Stopped is when the session stopped (zero when Tautulli has no time).
	Stopped time.Time
}

type historyPage struct {
	RecordsFiltered httpread.Int `json:"recordsFiltered"`
	Data            []struct {
		RowID     httpread.Int  `json:"row_id"`
		RatingKey httpread.Text `json:"rating_key"`
		GUID      httpread.Text `json:"guid"`
		Stopped   httpread.Int  `json:"stopped"`
	} `json:"data"`
}

// HistoryReader reads the history of one refresh: it holds the row budget and deduplicates rows
// by row_id across sections.
type HistoryReader struct {
	c    *Client
	seen map[int64]bool
	// Rows is how many distinct rows were read; Requests how many pages were requested.
	Rows     int
	Requests int
}

// NewHistoryReader starts a history read.
func (c *Client) NewHistoryReader() *HistoryReader {
	return &HistoryReader{c: c, seen: map[int64]bool{}}
}

// Section reads the whole history of one section, oldest first, calling fn for each distinct row
// (the refresh's exact query, §4.4). It fails with ErrPaging when the history changes under it
// and with ErrTooManyRows past the row budget.
func (h *HistoryReader) Section(ctx context.Context, sectionID string, fn func(Play) error) error {
	if !validSection(sectionID) {
		return fmt.Errorf("tautulli: invalid section id %q", sectionID)
	}
	total := -1
	for start := 0; ; {
		q := cmd("get_history", "section_id", sectionID, "media_type", HistoryMediaTypes, "grouping", "0",
			"include_activity", "0", "order_column", "date", "order_dir", "asc",
			"start", strconv.Itoa(start), "length", strconv.Itoa(h.c.pageSize))
		h.Requests++
		page, err := command[historyPage](ctx, h.c, q)
		if err != nil {
			return err
		}
		fail := func(cause error) error {
			return h.c.c.Fail(httpread.Request{Path: APIPath}, http.StatusOK, cause)
		}
		if !page.RecordsFiltered.Valid {
			return fail(fmt.Errorf("%w: no recordsFiltered", ErrNotTautulli))
		}
		n := int(page.RecordsFiltered.V)
		switch {
		case total < 0:
			total = n
		case n < total:
			return fail(fmt.Errorf("%w: section %s had %d rows, now %d", ErrPaging, sectionID, total, n))
		}
		for _, row := range page.Data {
			if !row.RowID.Valid {
				return fail(fmt.Errorf("%w: a history row has no row_id", ErrNotTautulli))
			}
			if h.seen[row.RowID.V] {
				continue
			}
			h.seen[row.RowID.V] = true
			h.Rows++
			if h.Rows > h.c.maxRows {
				return fail(ErrTooManyRows)
			}
			p := Play{RowID: row.RowID.V, RatingKey: row.RatingKey.String(), GUID: row.GUID.String()}
			if row.Stopped.Valid && row.Stopped.V > 0 {
				p.Stopped = time.Unix(row.Stopped.V, 0).UTC()
			}
			if err := fn(p); err != nil {
				return err
			}
		}
		start += len(page.Data)
		if start >= total {
			return nil
		}
		if len(page.Data) < h.c.pageSize {
			return fail(fmt.Errorf("%w: section %s: a page ended at row %d of %d", ErrPaging, sectionID, start, total))
		}
	}
}

// validSection accepts a Plex section key made of digits.
func validSection(s string) bool {
	if s == "" || len(s) > 12 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
