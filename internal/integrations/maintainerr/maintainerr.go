// Package maintainerr is a read-only client for the Maintainerr API (3.4.0 and later): the
// requests of docs/design/phase2-3.md §4.5 and nothing else.
//
// Allow-list (S16). Maintainerr has no API authentication and has state-changing GET routes, so
// the client can reach only these GET paths, each built by its own method:
//   - /api/app/status
//   - /api/collections/overlay-data
//   - /api/collections
//   - /api/collections/media/?collectionId=N (the fallback for a truncated member list)
//   - /api/rules
//   - /api/rules/exclusion?rulegroupId=N
//
// Never /api/collections/activate|deactivate/*, /api/settings* or the database download. No key is
// ever sent (S8). Redirects are not followed; errors carry the method, the path and a cause only;
// the transport dials through the outbound guard.
//
// Recorded quirks (testdata/maintainerr): /api/app/status answers a JSON object as text/html, so
// bodies are parsed whatever their content type says; /api/collections lists at most two members
// per collection (mediaCount has the real number); 3.4.1 answers rules/exclusion?rulegroupId=N
// with the group's rows plus every exclusion of every group (duplicates included), so the client
// keeps only the group's rows and the global ones (ruleGroupId null). Timestamps come as RFC 3339
// or as "2026-09-26 00:00:00.000".
//
// Pending computes which members Maintainerr will delete (§6.2). Its collection handler changed
// between the recordings: 3.4.1's handles a collection without deleteAfterDays at once and deletes
// excluded members too (probes/), 3.29.0's skips the one and honours the other (v3.29.0/probes/),
// so Pending follows the server's version: NoWindowNeverFrom (3.27.0, upstream #3639) for the one,
// HandlerFixedIn for the other.
package maintainerr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/integrations/httpread"
)

// Paths of the allow-list.
const (
	PathStatus          = "/api/app/status"
	PathOverlayData     = "/api/collections/overlay-data"
	PathCollections     = "/api/collections"
	PathCollectionMedia = "/api/collections/media/"
	PathRules           = "/api/rules"
	PathExclusions      = "/api/rules/exclusion"
	// MaxListBytes caps the lists (overlay-data, collections, rules).
	MaxListBytes = 256 << 20
	// MaxMembers bounds the members of one refresh.
	MaxMembers = 500000
)

// MinVersion is the oldest supported Maintainerr: 2.x used a numeric plexId, 3.0 renamed it to
// mediaServerId, and 3.4.0 added overlay-data and the members' tvdbId.
var MinVersion = httpread.Version{3, 4, 0}

var (
	// ErrTooOld means the server is older than MinVersion.
	ErrTooOld = errors.New("Maintainerr 3.4.0 or newer is required")
	// ErrNotMaintainerr means the server answered, but not like the Maintainerr API.
	ErrNotMaintainerr = errors.New("the server did not answer like the Maintainerr API")
	// ErrTooMany means the collections hold more members than allowed.
	ErrTooMany = fmt.Errorf("the Maintainerr collections hold more than %d members", MaxMembers)
)

// Options configures New. Zero values take the defaults.
type Options struct {
	httpread.Options
}

// Client talks to one Maintainerr. It is safe for concurrent use.
type Client struct {
	c *httpread.Client
	// requests counts the requests sent (stats).
	requests atomic.Int64
}

// New returns a client for the Maintainerr at baseURL. Maintainerr has no API key.
func New(baseURL string, o Options) (*Client, error) {
	c, err := httpread.New("Maintainerr", baseURL, "", o.Options)
	if err != nil {
		return nil, err
	}
	return &Client{c: c}, nil
}

// Requests returns how many requests the client sent.
func (c *Client) Requests() int64 { return c.requests.Load() }

func (c *Client) get(ctx context.Context, path string, query url.Values, list bool, out any) error {
	r := httpread.Request{Path: path, Query: query}
	if list {
		r.MaxBytes = MaxListBytes
	}
	c.requests.Add(1)
	resp, err := c.c.Get(ctx, r)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		cause := fmt.Errorf("unexpected status %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
		if resp.StatusCode == http.StatusNotFound {
			cause = fmt.Errorf("%w: not found (404)", ErrNotMaintainerr)
		}
		return c.c.Fail(r, resp.StatusCode, cause)
	}
	if err := httpread.DecodeJSON(resp.Body, out); err != nil {
		return c.c.Fail(r, resp.StatusCode, fmt.Errorf("%w: %w", ErrNotMaintainerr, err))
	}
	return nil
}

// Status is /api/app/status.
type Status struct {
	Version string
}

// Status reads the version and requires MinVersion (ErrTooOld otherwise).
func (c *Client) Status(ctx context.Context) (Status, error) {
	var body struct {
		Version httpread.Text `json:"version"`
	}
	if err := c.get(ctx, PathStatus, nil, false, &body); err != nil {
		return Status{}, err
	}
	st := Status{Version: body.Version.String()}
	v, ok := httpread.ParseVersion(st.Version)
	r := httpread.Request{Path: PathStatus}
	switch {
	case !ok:
		return st, c.c.Fail(r, http.StatusOK, fmt.Errorf("%w: no version", ErrNotMaintainerr))
	case v.Less(MinVersion):
		return st, c.c.Fail(r, http.StatusOK, ErrTooOld)
	}
	return st, nil
}

// Collection levels (a collection's type).
const (
	LevelMovie   = "movie"
	LevelShow    = "show"
	LevelSeason  = "season"
	LevelEpisode = "episode"
)

// Collection is a Maintainerr collection with the settings that decide deletion.
type Collection struct {
	ID    int64
	Title string
	// LibraryID is the Plex library (section key) the collection belongs to.
	LibraryID string
	IsActive  bool
	// ArrAction: 0 delete, 1 delete (Sonarr: the show), 2 delete (Sonarr: the season), 3
	// unmonitor, 4 do nothing, 5 delete the episode (and the show when empty).
	ArrAction int
	// DeleteAfterDays is nil when not set.
	DeleteAfterDays *int
	// Type is the collection's level: movie, show, season or episode.
	Type string
	// MediaCount is the number of members Maintainerr reports (-1 when absent).
	MediaCount int
	Media      []Member
}

// Member is a collection member.
type Member struct {
	ID           int64
	CollectionID int64
	// RatingKey is the member's Plex ratingKey at the collection's level (mediaServerId).
	RatingKey string
	// TMDBID and TVDBID are the movie's or the show's ids (0 when unknown).
	TMDBID  int64
	TVDBID  int64
	AddDate *time.Time
	// IsManual: added by hand; RuleEvaluationFailed: the member's rule check failed (newer
	// versions only).
	IsManual             bool
	RuleEvaluationFailed bool
}

type wireMember struct {
	ID                   httpread.Int  `json:"id"`
	CollectionID         httpread.Int  `json:"collectionId"`
	MediaServerID        httpread.Text `json:"mediaServerId"`
	TMDBID               httpread.Int  `json:"tmdbId"`
	TVDBID               httpread.Int  `json:"tvdbId"`
	AddDate              httpread.Text `json:"addDate"`
	IsManual             bool          `json:"isManual"`
	RuleEvaluationFailed bool          `json:"ruleEvaluationFailed"`
}

func (w wireMember) member() Member {
	m := Member{ID: w.ID.V, CollectionID: w.CollectionID.V, RatingKey: w.MediaServerID.String(), TMDBID: w.TMDBID.V, TVDBID: w.TVDBID.V,
		IsManual: w.IsManual, RuleEvaluationFailed: w.RuleEvaluationFailed}
	if t, ok := httpread.ParseTime(w.AddDate.String()); ok {
		m.AddDate = &t
	}
	return m
}

type wireCollection struct {
	ID              httpread.Int  `json:"id"`
	Title           httpread.Text `json:"title"`
	LibraryID       httpread.Text `json:"libraryId"`
	IsActive        bool          `json:"isActive"`
	ArrAction       httpread.Int  `json:"arrAction"`
	DeleteAfterDays httpread.Int  `json:"deleteAfterDays"`
	Type            httpread.Text `json:"type"`
	MediaCount      httpread.Int  `json:"mediaCount"`
	Media           []wireMember  `json:"media"`
}

func (w wireCollection) collection() Collection {
	c := Collection{ID: w.ID.V, Title: w.Title.String(), LibraryID: w.LibraryID.String(), IsActive: w.IsActive,
		ArrAction: int(w.ArrAction.V), Type: strings.ToLower(w.Type.String()), MediaCount: -1, Media: []Member{}}
	if w.DeleteAfterDays.Valid {
		d := int(w.DeleteAfterDays.V)
		c.DeleteAfterDays = &d
	}
	if w.MediaCount.Valid {
		c.MediaCount = int(w.MediaCount.V)
	}
	for _, m := range w.Media {
		c.Media = append(c.Media, m.member())
	}
	return c
}

func (c *Client) collections(ctx context.Context, path string) ([]Collection, error) {
	var body []wireCollection
	if err := c.get(ctx, path, nil, true, &body); err != nil {
		return nil, err
	}
	out := make([]Collection, 0, len(body))
	for _, w := range body {
		if !w.ID.Valid {
			return nil, c.c.Fail(httpread.Request{Path: path}, http.StatusOK, fmt.Errorf("%w: a collection has no id", ErrNotMaintainerr))
		}
		out = append(out, w.collection())
	}
	return out, nil
}

// OverlayData reads /api/collections/overlay-data: every collection with all its members.
func (c *Client) OverlayData(ctx context.Context) ([]Collection, error) {
	return c.collections(ctx, PathOverlayData)
}

// Collections reads /api/collections (at most two members each; MediaCount is the real number).
func (c *Client) Collections(ctx context.Context) ([]Collection, error) {
	return c.collections(ctx, PathCollections)
}

// CollectionMedia reads the members of one collection (/api/collections/media/?collectionId=N).
func (c *Client) CollectionMedia(ctx context.Context, collectionID int64) ([]Member, error) {
	if collectionID <= 0 {
		return nil, fmt.Errorf("maintainerr: invalid collection id %d", collectionID)
	}
	var body []wireMember
	if err := c.get(ctx, PathCollectionMedia, url.Values{"collectionId": {strconv.FormatInt(collectionID, 10)}}, true, &body); err != nil {
		return nil, err
	}
	out := make([]Member, 0, len(body))
	for _, w := range body {
		out = append(out, w.member())
	}
	return out, nil
}

// RuleGroup is a rule group and the collection it fills.
type RuleGroup struct {
	ID           int64
	CollectionID int64
	LibraryID    string
	IsActive     bool
}

// RuleGroups reads /api/rules.
func (c *Client) RuleGroups(ctx context.Context) ([]RuleGroup, error) {
	var body []struct {
		ID           httpread.Int  `json:"id"`
		CollectionID httpread.Int  `json:"collectionId"`
		LibraryID    httpread.Text `json:"libraryId"`
		IsActive     bool          `json:"isActive"`
	}
	if err := c.get(ctx, PathRules, nil, true, &body); err != nil {
		return nil, err
	}
	out := make([]RuleGroup, 0, len(body))
	for _, g := range body {
		if !g.ID.Valid {
			return nil, c.c.Fail(httpread.Request{Path: PathRules}, http.StatusOK, fmt.Errorf("%w: a rule group has no id", ErrNotMaintainerr))
		}
		out = append(out, RuleGroup{ID: g.ID.V, CollectionID: g.CollectionID.V, LibraryID: g.LibraryID.String(), IsActive: g.IsActive})
	}
	return out, nil
}

// Exclusion is an item kept out of a rule group (RuleGroupID set) or out of every group (global,
// RuleGroupID 0). An exclusion of a show or a season covers its children.
type Exclusion struct {
	ID          int64
	RatingKey   string
	RuleGroupID int64
	// Type is movie, show, season or episode.
	Type string
}

// Exclusions reads /api/rules/exclusion?rulegroupId=N and returns the group's rows and the global
// ones, each once (3.4.1 answers every group's rows).
func (c *Client) Exclusions(ctx context.Context, ruleGroupID int64) ([]Exclusion, error) {
	if ruleGroupID <= 0 {
		return nil, fmt.Errorf("maintainerr: invalid rule group id %d", ruleGroupID)
	}
	var body []struct {
		ID            httpread.Int  `json:"id"`
		MediaServerID httpread.Text `json:"mediaServerId"`
		RuleGroupID   httpread.Int  `json:"ruleGroupId"`
		Type          httpread.Text `json:"type"`
	}
	if err := c.get(ctx, PathExclusions, url.Values{"rulegroupId": {strconv.FormatInt(ruleGroupID, 10)}}, true, &body); err != nil {
		return nil, err
	}
	var out []Exclusion
	seen := map[int64]bool{}
	for _, e := range body {
		if e.RuleGroupID.Valid && e.RuleGroupID.V != ruleGroupID {
			continue
		}
		if seen[e.ID.V] {
			continue
		}
		seen[e.ID.V] = true
		out = append(out, Exclusion{ID: e.ID.V, RatingKey: e.MediaServerID.String(), RuleGroupID: e.RuleGroupID.V, Type: strings.ToLower(e.Type.String())})
	}
	return out, nil
}

// Snapshot is everything a refresh reads, fetched completely or not at all.
type Snapshot struct {
	Version     string
	Collections []Collection
	Groups      []RuleGroup
	// GroupExclusions are each group's own exclusions; Global are the exclusions of every group.
	GroupExclusions map[int64][]Exclusion
	Global          []Exclusion
	// GlobalKnown is false when there is no rule group to read the global exclusions through.
	GlobalKnown bool
	// FallbackReads counts the collections whose members were read one by one.
	FallbackReads int
}

// Fetch reads the status (with the version gate), the collections with all their members
// (overlay-data, then collections/media for a collection overlay-data lists incompletely or not at
// all), the rule groups, and each group's exclusions.
func (c *Client) Fetch(ctx context.Context) (Snapshot, error) {
	st, err := c.Status(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	snap := Snapshot{Version: st.Version, GroupExclusions: map[int64][]Exclusion{}}
	overlay, err := c.OverlayData(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	list, err := c.Collections(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	byID := map[int64]int{}
	for i, col := range overlay {
		byID[col.ID] = i
	}
	for _, col := range list {
		i, ok := byID[col.ID]
		if !ok {
			overlay = append(overlay, col)
			i = len(overlay) - 1
			byID[col.ID] = i
			overlay[i].Media = nil
		}
		want := max(col.MediaCount, overlay[i].MediaCount)
		if !ok || len(overlay[i].Media) < want {
			media, err := c.CollectionMedia(ctx, col.ID)
			if err != nil {
				return Snapshot{}, err
			}
			overlay[i].Media = media
			snap.FallbackReads++
		}
	}
	members := 0
	for _, col := range overlay {
		members += len(col.Media)
	}
	if members > MaxMembers {
		return Snapshot{}, c.c.Fail(httpread.Request{Path: PathOverlayData}, http.StatusOK, ErrTooMany)
	}
	snap.Collections = overlay
	if snap.Groups, err = c.RuleGroups(ctx); err != nil {
		return Snapshot{}, err
	}
	global := map[int64]Exclusion{}
	for _, g := range snap.Groups {
		ex, err := c.Exclusions(ctx, g.ID)
		if err != nil {
			return Snapshot{}, err
		}
		snap.GlobalKnown = true
		for _, e := range ex {
			if e.RuleGroupID == 0 {
				global[e.ID] = e
			} else {
				snap.GroupExclusions[g.ID] = append(snap.GroupExclusions[g.ID], e)
			}
		}
	}
	for _, e := range global {
		snap.Global = append(snap.Global, e)
	}
	slices.SortFunc(snap.Global, func(a, b Exclusion) int { return int(a.ID - b.ID) })
	return snap, nil
}
