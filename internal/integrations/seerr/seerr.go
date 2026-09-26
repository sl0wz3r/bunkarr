// Package seerr is a read-only client for the Seerr API v1 (seerr-team/seerr 3 and later;
// Overseerr and Jellyseerr best effort): the requests of docs/design/phase2-3.md §4.5 and nothing
// else.
//
// Allow-list (S16): GET /api/v1/status (without the key), GET /api/v1/auth/me,
// GET /api/v1/request?take=100&skip=…&sort=added&sortDirection=asc and GET /api/v1/user?take=100&
// skip=… (the rule editor's labels, read live). The client has no general request method.
//
// Safety (S8, SEC4). The API key goes in X-Api-Key only; X-API-User is never sent. A bad key
// answers 403 (no key: 401). From a request only id, status, type, is4k, seasons[].seasonNumber,
// requestedBy.id, createdAt and media{tmdbId, tvdbId, ratingKey} are decoded: no e-mail, no name,
// not serviceId or externalServiceId. From a user only id, username, plexUsername and
// jellyfinUsername are decoded: never displayName (Seerr fills it with the e-mail when a user has
// no user name) and never email. A user's label is its first non-empty name, else
// "Seerr user #<id>".
//
// Paging integrity (as for Tautulli): requests are deduplicated by id, the total comes from page
// 1 (pageInfo.results), and the read fails on a shrinking total or a short page; at most
// MaxRequests requests are read.
package seerr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/integrations/httpread"
)

// Paths and limits.
const (
	PathStatus  = "/api/v1/status"
	PathMe      = "/api/v1/auth/me"
	PathRequest = "/api/v1/request"
	PathUser    = "/api/v1/user"
	// DefaultPageSize is the take of a page.
	DefaultPageSize = 100
	// MaxRequests bounds the requests one refresh reads.
	MaxRequests = 250000
	// MaxUsers bounds the users read for labels.
	MaxUsers = 10000
)

var (
	// ErrUnauthorized means Seerr answered 401 or 403: the API key is missing or wrong.
	ErrUnauthorized = errors.New("the API key was rejected")
	// ErrNotSeerr means the server answered, but not like the Seerr API.
	ErrNotSeerr = errors.New("the server did not answer like the Seerr API")
	// ErrPaging means the list changed while it was read (a smaller total or a short page).
	ErrPaging = errors.New("the Seerr list changed while it was read")
	// ErrTooMany means the list is longer than allowed.
	ErrTooMany = errors.New("the Seerr list is longer than allowed")
)

// Request statuses (MediaRequestStatus).
const (
	StatusPending   = 1
	StatusApproved  = 2
	StatusDeclined  = 3
	StatusFailed    = 4
	StatusCompleted = 5
)

// Counted reports whether a request status counts for seerr.requested (1, 2, 4 and 5, §6.2).
func Counted(status int) bool {
	return status == StatusPending || status == StatusApproved || status == StatusFailed || status == StatusCompleted
}

// Options configures New. Zero values take the defaults.
type Options struct {
	httpread.Options
	// PageSize overrides DefaultPageSize (tests).
	PageSize int
	// MaxRequests overrides MaxRequests (tests).
	MaxRequests int
}

// Client talks to one Seerr. It is safe for concurrent use.
type Client struct {
	c        *httpread.Client
	pageSize int
	max      int
	sent     atomic.Int64
}

// Sent returns how many requests the client sent.
func (c *Client) Sent() int64 { return c.sent.Load() }

// New returns a client for the Seerr at baseURL with apiKey.
func New(baseURL, apiKey string, o Options) (*Client, error) {
	c, err := httpread.New("Seerr", baseURL, apiKey, o.Options)
	if err != nil {
		return nil, err
	}
	cl := &Client{c: c, pageSize: o.PageSize, max: o.MaxRequests}
	if cl.pageSize <= 0 {
		cl.pageSize = DefaultPageSize
	}
	if cl.max <= 0 {
		cl.max = MaxRequests
	}
	return cl, nil
}

// get sends an allow-listed request and decodes its JSON body.
func (c *Client) get(ctx context.Context, r httpread.Request, out any) error {
	c.sent.Add(1)
	resp, err := c.c.Get(ctx, r)
	if err != nil {
		return err
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return c.c.Fail(r, resp.StatusCode, ErrUnauthorized)
	case resp.StatusCode == http.StatusNotFound:
		return c.c.Fail(r, resp.StatusCode, fmt.Errorf("%w: not found (404)", ErrNotSeerr))
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return c.c.Fail(r, resp.StatusCode, fmt.Errorf("unexpected status %d %s", resp.StatusCode, http.StatusText(resp.StatusCode)))
	}
	if err := httpread.DecodeJSON(resp.Body, out); err != nil {
		return c.c.Fail(r, resp.StatusCode, fmt.Errorf("%w: %w", ErrNotSeerr, err))
	}
	return nil
}

// Status is GET /api/v1/status.
type Status struct {
	Version string
}

// Status reads the version; it is sent without the key.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var body struct {
		Version *httpread.Text `json:"version"`
	}
	r := httpread.Request{Path: PathStatus}
	if err := c.get(ctx, r, &body); err != nil {
		return Status{}, err
	}
	if body.Version == nil || *body.Version == "" {
		return Status{}, c.c.Fail(r, http.StatusOK, fmt.Errorf("%w: no version", ErrNotSeerr))
	}
	return Status{Version: body.Version.String()}, nil
}

// Me checks the key with GET /api/v1/auth/me; only the user id is decoded.
func (c *Client) Me(ctx context.Context) (int64, error) {
	var body struct {
		ID httpread.Int `json:"id"`
	}
	r := httpread.Request{Path: PathMe, WithKey: true}
	if err := c.get(ctx, r, &body); err != nil {
		return 0, err
	}
	if !body.ID.Valid {
		return 0, c.c.Fail(r, http.StatusOK, fmt.Errorf("%w: no user id", ErrNotSeerr))
	}
	return body.ID.V, nil
}

// Request is a Seerr media request, reduced to the fields Bunkarr uses.
type Request struct {
	ID     int64
	Status int
	// Type is movie or tv.
	Type string
	Is4K bool
	// Seasons are the requested season numbers (tv); empty covers every season.
	Seasons []int
	// UserID is requestedBy.id (0 when missing).
	UserID    int64
	CreatedAt *time.Time
	TMDBID    int64
	TVDBID    int64
	// RatingKey is media.ratingKey: the movie's or the show's Plex key ("" when none).
	RatingKey string
}

type wireRequest struct {
	ID      httpread.Int  `json:"id"`
	Status  httpread.Int  `json:"status"`
	Type    httpread.Text `json:"type"`
	Is4K    bool          `json:"is4k"`
	Seasons []struct {
		SeasonNumber httpread.Int `json:"seasonNumber"`
	} `json:"seasons"`
	RequestedBy *struct {
		ID httpread.Int `json:"id"`
	} `json:"requestedBy"`
	CreatedAt httpread.Text `json:"createdAt"`
	Media     *struct {
		TMDBID    httpread.Int  `json:"tmdbId"`
		TVDBID    httpread.Int  `json:"tvdbId"`
		RatingKey httpread.Text `json:"ratingKey"`
	} `json:"media"`
}

type page[T any] struct {
	PageInfo *struct {
		Results httpread.Int `json:"results"`
	} `json:"pageInfo"`
	Results []T `json:"results"`
}

// pager reads a paged list with the integrity rules: fn gets each element's id and the element.
func pager[T any](ctx context.Context, c *Client, path string, extra url.Values, limit int, id func(T) (int64, bool), fn func(T) error) (int, error) {
	seen := map[int64]bool{}
	total := -1
	for skip := 0; ; {
		q := url.Values{"take": {strconv.Itoa(c.pageSize)}, "skip": {strconv.Itoa(skip)}}
		for k, v := range extra {
			q[k] = v
		}
		r := httpread.Request{Path: path, Query: q, WithKey: true}
		var p page[T]
		if err := c.get(ctx, r, &p); err != nil {
			return len(seen), err
		}
		fail := func(cause error) (int, error) { return len(seen), c.c.Fail(r, http.StatusOK, cause) }
		if p.PageInfo == nil || !p.PageInfo.Results.Valid {
			return fail(fmt.Errorf("%w: no pageInfo.results", ErrNotSeerr))
		}
		n := int(p.PageInfo.Results.V)
		switch {
		case total < 0:
			total = n
		case n < total:
			return fail(fmt.Errorf("%w: %d entries, now %d", ErrPaging, total, n))
		}
		for _, v := range p.Results {
			k, ok := id(v)
			if !ok {
				return fail(fmt.Errorf("%w: an entry has no id", ErrNotSeerr))
			}
			if seen[k] {
				continue
			}
			seen[k] = true
			if len(seen) > limit {
				return fail(fmt.Errorf("%w (%d)", ErrTooMany, limit))
			}
			if err := fn(v); err != nil {
				return len(seen), err
			}
		}
		skip += len(p.Results)
		if skip >= total {
			return len(seen), nil
		}
		if len(p.Results) < c.pageSize {
			return fail(fmt.Errorf("%w: a page ended at entry %d of %d", ErrPaging, skip, total))
		}
	}
}

// Requests reads every request, oldest first, calling fn for each distinct one. It returns how
// many were read.
func (c *Client) Requests(ctx context.Context, fn func(Request) error) (int, error) {
	extra := url.Values{"sort": {"added"}, "sortDirection": {"asc"}}
	return pager(ctx, c, PathRequest, extra, c.max, func(w wireRequest) (int64, bool) { return w.ID.V, w.ID.Valid },
		func(w wireRequest) error { return fn(w.request()) })
}

func (w wireRequest) request() Request {
	r := Request{ID: w.ID.V, Status: int(w.Status.V), Type: strings.ToLower(w.Type.String()), Is4K: w.Is4K, Seasons: []int{}}
	for _, s := range w.Seasons {
		if s.SeasonNumber.Valid {
			r.Seasons = append(r.Seasons, int(s.SeasonNumber.V))
		}
	}
	if w.RequestedBy != nil {
		r.UserID = w.RequestedBy.ID.V
	}
	if t, ok := httpread.ParseTime(w.CreatedAt.String()); ok {
		r.CreatedAt = &t
	}
	if w.Media != nil {
		r.TMDBID, r.TVDBID, r.RatingKey = w.Media.TMDBID.V, w.Media.TVDBID.V, w.Media.RatingKey.String()
	}
	return r
}

// User is a Seerr user as the rule editor labels it: its id and a label built from its user
// names only.
type User struct {
	ID    int64  `json:"id"`
	Label string `json:"label"`
}

type wireUser struct {
	ID               httpread.Int  `json:"id"`
	Username         httpread.Text `json:"username"`
	PlexUsername     httpread.Text `json:"plexUsername"`
	JellyfinUsername httpread.Text `json:"jellyfinUsername"`
}

// Label is the user's first non-empty name, else "Seerr user #<id>".
func (w wireUser) Label() string {
	for _, n := range []httpread.Text{w.Username, w.PlexUsername, w.JellyfinUsername} {
		if n != "" {
			return n.String()
		}
	}
	return fmt.Sprintf("Seerr user #%d", w.ID.V)
}

// Users reads every user (id and label only).
func (c *Client) Users(ctx context.Context) ([]User, error) {
	out := []User{}
	_, err := pager(ctx, c, PathUser, nil, MaxUsers, func(w wireUser) (int64, bool) { return w.ID.V, w.ID.Valid },
		func(w wireUser) error {
			out = append(out, User{ID: w.ID.V, Label: w.Label()})
			return nil
		})
	if err != nil {
		return nil, err
	}
	return out, nil
}
