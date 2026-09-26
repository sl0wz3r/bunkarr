package tiers

import (
	"context"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/mediaindex"
)

// Facts are what the evaluator knows about one live catalog file (design §8.3). A nil provider
// part (Plex, Watch, Requests, Maintainerr) means nothing supplies it: its conditions are
// unknown.
type Facts struct {
	FileID   int64  `json:"fileId"`
	SourceID int64  `json:"sourceId"`
	RelPath  string `json:"relPath"`
	Size     int64  `json:"size"`
	MtimeNs  int64  `json:"-"`
	// Group is the file's per-scan hardlink group ("" = none).
	Group string `json:"-"`
	// FirstSeenAt is when a scan first saw the file (the last fallback of file.age).
	FirstSeenAt time.Time `json:"firstSeenAt"`
	// LocalPath is the file's path as Bunkarr sees it: the source's path joined with RelPath.
	LocalPath string `json:"localPath"`

	Arr ArrFacts `json:"arr"`
	// Plex is the file's Plex section (plex.section) and added date.
	Plex *PlexFacts `json:"plex,omitempty"`
	// Watch, Requests and Maintainerr come from a Provider (Tautulli, Seerr, Maintainerr).
	Watch       *WatchFacts       `json:"watch,omitempty"`
	Requests    *RequestFacts     `json:"requests,omitempty"`
	Maintainerr *MaintainerrFacts `json:"maintainerr,omitempty"`
	// Flags are the irreplaceable flags that cover the file (§8.7).
	Flags []int64 `json:"flags"`
	// Follows is, for a sidecar (X.en.srt, X.nfo), the media file whose decision it takes (§8.3).
	Follows string `json:"follows,omitempty"`
}

// ArrState is whether an *arr item claims a file.
type ArrState string

// *arr states.
const (
	// ArrItem: the file's *arr item is known (Facts.Arr.Item).
	ArrItem ArrState = "item"
	// ArrUnmanaged: the file lies outside every mapped root folder of every *arr integration while
	// every *arr cache is fresh, every root folder has a path mapping, and no deleted *arr
	// integration's folder holds it (DeletedArr): arr.* conditions are false.
	ArrUnmanaged ArrState = "unmanaged"
	// ArrUnknown: whether and which item claims the file cannot be decided (Why says why).
	ArrUnknown ArrState = "unknown"
)

// ArrFacts are the *arr facts of a file.
type ArrFacts struct {
	State ArrState `json:"state"`
	// Why says why the state is unknown ("Radarr cache is 31 h old", "two *arr integrations claim
	// this file").
	Why string `json:"why,omitempty"`
	// IntegrationID is the integration the facts (or the unknown) come from, when there is one.
	IntegrationID int64 `json:"integrationId,omitempty"`
	// Item is the claiming item (State item).
	Item *ArrItemFacts `json:"item,omitempty"`
	// ByFolder is set when the file has no *arr file of its own and was attributed to the item
	// whose folder contains it (an extra): episode-level facts are unknown for it.
	ByFolder bool `json:"byFolder,omitempty"`
	// FileID is the *arr file id (arr_files.arr_file_id) at the file's path; DateAdded and
	// Quality are that *arr file's.
	FileID    int64      `json:"arrFileId,omitempty"`
	DateAdded *time.Time `json:"dateAdded,omitempty"`
	Quality   string     `json:"quality,omitempty"`
}

// ArrItemFacts are the item-level facts of an *arr item.
type ArrItemFacts struct {
	IntegrationID int64  `json:"integrationId"`
	App           string `json:"app"`
	// ItemID is the arr_items row; Kind and ArrID the item in the *arr.
	ItemID      int64                  `json:"itemId"`
	Kind        string                 `json:"kind"`
	ArrID       int64                  `json:"arrId"`
	Title       string                 `json:"title"`
	Year        int                    `json:"year"`
	ExternalIDs mediaindex.ExternalIDs `json:"externalIds"`
	// Tags are labels; a tag id the index has no label for is listed as "#<id>".
	Tags []string `json:"tags"`
	// QualityProfile is the profile's name ("" when the index does not know the id).
	QualityProfile   string   `json:"qualityProfile"`
	QualityProfileID int64    `json:"qualityProfileId"`
	RootFolder       string   `json:"rootFolder"`
	Monitored        bool     `json:"monitored"`
	Genres           []string `json:"genres"`
	// Folder is the item's folder as Bunkarr sees it ("" when unmapped).
	Folder string `json:"folder,omitempty"`
}

// PlexFacts are a file's Plex facts: its section ("<plexIntegrationId>:<sectionKey>") and when
// Plex added it. Known false makes plex.section unknown (Why).
type PlexFacts struct {
	Known         bool       `json:"known"`
	Why           string     `json:"why,omitempty"`
	IntegrationID int64      `json:"integrationId,omitempty"`
	Section       string     `json:"section,omitempty"`
	AddedAt       *time.Time `json:"addedAt,omitempty"`
}

// WatchFacts are a file's Tautulli facts. With no history row, Plays is 0 and LastWatched nil.
// LowerBound is set when a user or the file's section has history turned off, so the count is
// only a lower bound (§8.2).
type WatchFacts struct {
	Known         bool       `json:"known"`
	Why           string     `json:"why,omitempty"`
	IntegrationID int64      `json:"integrationId,omitempty"`
	Plays         int64      `json:"plays"`
	LastWatched   *time.Time `json:"lastWatched,omitempty"`
	LowerBound    bool       `json:"lowerBound,omitempty"`
}

// RequestFacts are a file's Seerr facts: whether a counted request (status 1, 2, 4 or 5) covers
// it (tri-state, Why when unknown) and the users (ids only) of those requests.
type RequestFacts struct {
	Requested     Result  `json:"requested"`
	Why           string  `json:"why,omitempty"`
	IntegrationID int64   `json:"integrationId,omitempty"`
	Users         []int64 `json:"users"`
}

// MaintainerrFacts are a file's Maintainerr facts: whether it is pending deletion (tri-state).
type MaintainerrFacts struct {
	Pending       Result     `json:"pending"`
	Why           string     `json:"why,omitempty"`
	IntegrationID int64      `json:"integrationId,omitempty"`
	DeleteAfter   *time.Time `json:"deleteAfter,omitempty"`
}

// Provider supplies the facts of an application the engine does not read itself: the Plex
// library index (Plex section fallback, added date), Tautulli, Seerr and Maintainerr (Phase 3
// slice 9). A field listed by no registered provider reports available: false (D16) and
// evaluates unknown.
type Provider interface {
	// Fields are the condition fields whose facts it supplies.
	Fields() []string
	// Load fills its part of the facts (Plex, Watch, Requests or Maintainerr) of files of source
	// src, reading through q. now is the evaluation time (freshness included).
	Load(ctx context.Context, q Queryer, src catalog.Source, files []*Facts, now time.Time) error
	// Unknown lists its caches that are not fresh now, and why.
	Unknown(ctx context.Context, q Queryer, now time.Time) ([]UnknownSource, error)
	// Suggestions returns the rule-editor suggestions of one of its fields.
	Suggestions(ctx context.Context, field string) ([]Suggestion, error)
	// Known reports whether a fresh index knows a condition value of one of its fields (stale
	// references, §8.7).
	Known(ctx context.Context, q Queryer, field, value string, ids []int64, now time.Time) (bool, error)
}

// Suggestion is a value the rule editor offers for a field.
type Suggestion struct {
	Value any    `json:"value"`
	Label string `json:"label"`
}
