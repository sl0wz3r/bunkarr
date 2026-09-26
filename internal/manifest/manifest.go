// Package manifest writes and reads Bunkarr's library manifests (docs/design/phase2-3.md §11,
// safety rule S20): a JSON (canonical) and a CSV (informational) list of every *arr item and every
// library file, with what each destination holds of it, so a library can be acquired again after
// a disaster even where its media was not copied.
//
// It owns the manifests table and the version directories .bunkarr/manifests/<version>/ at each
// destination. Its parts:
//
//   - Builder.Build reads the catalog, the metadata index (internal/mediaindex), the tier
//     decisions (Tiers) and the destination's records in one read transaction, so a manifest
//     reflects one state of the database (a file renamed by a concurrent scan appears exactly
//     once).
//   - WriteJSON, WriteCSV and the SHA256SUMS helpers encode a manifest; Parse and ParseDir read
//     one back (ParseDir also checks SHA256SUMS).
//   - (*Manifest).ReimportPlan and ComparePlan compare a manifest with an *arr's live state
//     (acceptance 3); a tool that actually re-imports is deferred.
//   - Runner runs manifest_export jobs: recovery of interrupted jobs, the "unchanged" check that
//     reads the newest version back first, atomic versions, pruning.
//   - Runner.Export and Runner.Download stage a manifest before the first byte is served.
//
// Safety rules that live here (S20):
//   - Complete: every non-deleted item of every enabled *arr integration is listed in every
//     manifest, located or not, and every file with a live record at the destination is listed
//     whatever its tier; only non-*arr files of tier skip without a record are left out (counted).
//     A cache that is not fresh, or an item that does not locate, ends the job with warnings.
//   - Atomic: a version is written into .partial-job<id>/ as WriteFileAtomic writes (temp file,
//     fsync, no-replace rename; manifest.json and manifest.csv streamed, never held in memory),
//     together with SHA256SUMS, then renamed and recorded with its checksum; pruning deletes
//     only recorded version directories. Builds take turns (Options.MaxBuilds).
//   - Checked: "unchanged" holds only after the newest ok version was read back and matched its
//     checksum; a damaged version is marked and replaced; downloads verify before serving.
//   - No secrets: integrations appear by id, type, name and version, never with a URL or key.
//
// Until Phase 3 every file's tier is full (AllFull); internal/tiers plugs in through Tiers.
package manifest

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Format identifiers of manifest.json (design §11.1).
const (
	// FormatName is manifest.json's "format".
	FormatName = "bunkarr-manifest"
	// FormatVersion is the format version this package writes and reads.
	FormatVersion = 1
)

// File names of a version directory and where versions live at a destination.
const (
	// JSONName is the canonical manifest.
	JSONName = "manifest.json"
	// CSVName is the informational spreadsheet export.
	CSVName = "manifest.csv"
	// SumsName holds "<hex>  manifest.json" and "<hex>  manifest.csv".
	SumsName = "SHA256SUMS"
	// Root is the directory of the versions, relative to the destination target.
	Root = ".bunkarr/manifests"
)

// Scope kinds (Manifest.Scope.Kind).
const (
	// ScopeDestination is a destination's version: tiers, kept and backedUp are that
	// destination's.
	ScopeDestination = "destination"
	// ScopeExport is GET /manifest/export without a destination: every enabled source, tiers,
	// kept and backedUp null.
	ScopeExport = "export"
)

// Tier is a file's tier at a destination (design §8).
type Tier string

// Tiers.
const (
	TierFull     Tier = "full"
	TierManifest Tier = "manifest"
	TierSkip     Tier = "skip"
)

// Errors.
var (
	// ErrFormat means the input is not a manifest of a format and version this package reads.
	ErrFormat = errors.New("not a Bunkarr manifest of a supported format")
	// ErrDamaged means a version's files are missing or do not match their checksums.
	ErrDamaged = errors.New("manifest damaged: checksum mismatch")
	// ErrNotFound means no manifest version has the given id.
	ErrNotFound = errors.New("manifest not found")
	// ErrBusy means the most on-the-spot exports are running already (API: 429).
	ErrBusy = errors.New("too many manifest exports are running; try again shortly")
)

// Manifest is manifest.json (format 1, design §11.1).
type Manifest struct {
	Format        string    `json:"format"`
	FormatVersion int       `json:"formatVersion"`
	CreatedAt     time.Time `json:"createdAt"`
	Generator     Generator `json:"generator"`
	Scope         Scope     `json:"scope"`
	// Job is the job that wrote a destination version (nil for an export).
	Job          *JobRef       `json:"job,omitempty"`
	Integrations []Integration `json:"integrations"`
	Sources      []Source      `json:"sources"`
	// Items is every non-deleted item of every enabled *arr integration.
	Items []Item `json:"items"`
	// OtherFiles are the scope's files that belong to no item.
	OtherFiles []ExtraFile `json:"otherFiles"`
	Summary    Summary     `json:"summary"`
}

// Generator names the program that wrote a manifest.
type Generator struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Scope says what a manifest covers.
type Scope struct {
	// Kind is ScopeDestination or ScopeExport.
	Kind            string `json:"kind"`
	DestinationID   int64  `json:"destinationId,omitempty"`
	DestinationName string `json:"destinationName,omitempty"`
}

// JobRef names the job that wrote a version: its id and queue time (ids start over with a new
// Bunkarr database while the destination keeps its versions).
type JobRef struct {
	ID       int64     `json:"id"`
	QueuedAt time.Time `json:"queuedAt"`
}

// Integration is an *arr integration as a manifest records it: by id, type, name and version,
// never with its URL or key (S20).
type Integration struct {
	ID          int64      `json:"id"`
	Type        string     `json:"type"`
	Name        string     `json:"name"`
	AppVersion  string     `json:"appVersion"`
	RefreshedAt *time.Time `json:"refreshedAt"`
	// Status is the index status of the last refresh attempt (never, ok, failed).
	Status string `json:"status"`
	// Fresh is whether the cache the items come from was fresh (design D15).
	Fresh     bool    `json:"fresh"`
	LastError *string `json:"lastError"`
	// QualityProfiles, MetadataProfiles (Lidarr), RootFolders and Tags are the *arr's metadata.
	QualityProfiles  []Named      `json:"qualityProfiles"`
	MetadataProfiles []Named      `json:"metadataProfiles"`
	RootFolders      []RootFolder `json:"rootFolders"`
	Tags             []Tag        `json:"tags"`
}

// Named is a quality or metadata profile.
type Named struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// Tag is an *arr tag.
type Tag struct {
	ID    int64  `json:"id"`
	Label string `json:"label"`
}

// RootFolder is an *arr root folder: its path as the *arr sees it, and where it lies for Bunkarr.
type RootFolder struct {
	ID         int64   `json:"id"`
	Path       string  `json:"path"`
	Accessible bool    `json:"accessible"`
	LocalPath  *string `json:"localPath"`
	SourceID   *int64  `json:"sourceId"`
}

// Source is a source a manifest's files refer to.
type Source struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// DestFolder is the folder that mirrors the source at every destination.
	DestFolder string `json:"destFolder"`
}

// Item is an *arr item: a movie, a series or an artist.
type Item struct {
	IntegrationID int64 `json:"integrationId"`
	// Kind is movie, series or artist.
	Kind        string      `json:"kind"`
	ArrID       int64       `json:"arrId"`
	Title       string      `json:"title"`
	Year        int         `json:"year"`
	ExternalIDs ExternalIDs `json:"externalIds"`
	// Path is the item's folder as the *arr sees it.
	Path string `json:"path"`
	// Located is false when the item's folder maps into no source; its files then have no source.
	Located    bool   `json:"located"`
	RootFolder string `json:"rootFolder"`
	// QualityProfile and MetadataProfile (Lidarr) are profile names ("" when the index does not
	// know the id).
	QualityProfile  string `json:"qualityProfile"`
	MetadataProfile string `json:"metadataProfile"`
	Monitored       bool   `json:"monitored"`
	// Tags are tag labels.
	Tags   []string   `json:"tags"`
	Genres []string   `json:"genres"`
	Added  *time.Time `json:"added"`
	Detail ItemDetail `json:"detail"`
	// Files are every *arr file of the item, whatever its tier.
	Files []File `json:"files"`
	// ExtraFiles are catalog files attributed to the item (sidecars, extras).
	ExtraFiles []ExtraFile `json:"extraFiles"`
	// SkippedFiles counts the extras left out: tier skip without a live record (design D9).
	SkippedFiles int `json:"skippedFiles"`
}

// ExternalIDs are an item's ids in the metadata databases; a zero value is unknown.
type ExternalIDs struct {
	TMDB   int64  `json:"tmdb,omitempty"`
	IMDB   string `json:"imdb,omitempty"`
	TVDB   int64  `json:"tvdb,omitempty"`
	TVMaze int64  `json:"tvmaze,omitempty"`
	MBID   string `json:"mbid,omitempty"`
}

// ItemDetail holds the kind-specific fields a re-import needs (design §11.1).
type ItemDetail struct {
	// Radarr.
	MinimumAvailability string `json:"minimumAvailability,omitempty"`
	// Sonarr.
	SeriesType        string    `json:"seriesType,omitempty"`
	SeasonFolder      *bool     `json:"seasonFolder,omitempty"`
	MonitorNewItems   string    `json:"monitorNewItems,omitempty"`
	UseSceneNumbering *bool     `json:"useSceneNumbering,omitempty"`
	LanguageProfileID int64     `json:"languageProfileId,omitempty"`
	Seasons           []Season  `json:"seasons,omitempty"`
	Episodes          []Episode `json:"episodes,omitempty"`
	// Lidarr.
	Albums []Album `json:"albums,omitempty"`
}

// Season is a Sonarr season.
type Season struct {
	SeasonNumber int  `json:"seasonNumber"`
	Monitored    bool `json:"monitored"`
}

// Episode is a Sonarr episode.
type Episode struct {
	Season    int  `json:"season"`
	Episode   int  `json:"episode"`
	Monitored bool `json:"monitored"`
}

// Album is a Lidarr album.
type Album struct {
	ID        int64  `json:"id"`
	MBID      string `json:"mbid"`
	Title     string `json:"title"`
	Monitored bool   `json:"monitored"`
}

// File is an *arr file of an item.
type File struct {
	ArrFileID int64 `json:"arrFileId"`
	// Path is as the *arr sees it; RelativePath is inside the item's folder.
	Path         string     `json:"path"`
	RelativePath string     `json:"relativePath"`
	Size         int64      `json:"size"`
	Quality      string     `json:"quality"`
	DateAdded    *time.Time `json:"dateAdded"`
	// Episodes (Sonarr) are the episodes the file holds; AlbumID (Lidarr) its album.
	Episodes []FileEpisode `json:"episodes,omitempty"`
	AlbumID  int64         `json:"albumId,omitempty"`
	// Source is where the file lies in a source (nil when it maps into none).
	Source *FileSource `json:"source"`
	// Tier, Rule, Kept and BackedUp describe the destination's copy: null in an export; tier null
	// with kept and backedUp false for a file outside the destination's sources.
	Tier     *Tier    `json:"tier"`
	Rule     *RuleRef `json:"rule"`
	Kept     *bool    `json:"kept"`
	BackedUp *bool    `json:"backedUp"`
	SHA256   *string  `json:"sha256"`
}

// FileEpisode is an episode held by a Sonarr file.
type FileEpisode struct {
	Season  int `json:"season"`
	Episode int `json:"episode"`
}

// FileSource is a file's location in a source.
type FileSource struct {
	ID      int64  `json:"id"`
	RelPath string `json:"relPath"`
}

// RuleRef names the tier rule that decided a file's tier (id 0: the built-in fallback).
type RuleRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// ExtraFile is a catalog file that is not an *arr file: an item's sidecar or extra, or a file of
// no item (Manifest.OtherFiles).
type ExtraFile struct {
	// Source is the source's id, and RelPath the path inside it. Source is nil for a record whose
	// source was deleted (or a second record of one source file): RelPath is then the record's
	// path relative to the destination target, where the file lies.
	Source   *int64 `json:"source"`
	RelPath  string `json:"relPath"`
	Size     int64  `json:"size"`
	Tier     *Tier  `json:"tier"`
	Kept     *bool  `json:"kept"`
	BackedUp *bool  `json:"backedUp"`
}

// Summary counts what a manifest lists.
type Summary struct {
	Items          int64      `json:"items"`
	UnlocatedItems int64      `json:"unlocatedItems"`
	Files          int64      `json:"files"`
	Bytes          int64      `json:"bytes"`
	Tiers          TierCounts `json:"tiers"`
	KeptFiles      int64      `json:"keptFiles"`
	BackedUpBytes  int64      `json:"backedUpBytes"`
	// LeftOut counts the files not listed: non-*arr files of tier skip without a record (D9).
	LeftOut int64 `json:"leftOut"`
}

// TierCounts are the listed files per tier.
type TierCounts struct {
	Full     Count `json:"full"`
	Manifest Count `json:"manifest"`
	Skip     Count `json:"skip"`
}

// Count is a number of files and their bytes.
type Count struct {
	Files int64 `json:"files"`
	Bytes int64 `json:"bytes"`
}

// Decision is a file's tier at a destination and the rule that decided it (design §8.1).
type Decision struct {
	Tier     Tier
	RuleID   int64
	RuleName string
}

// FallbackRuleName names the built-in fallback rule (rule id 0).
const FallbackRuleName = "no rule matched"

// Queryer runs read queries: *sql.DB and *sql.Tx satisfy it. The builder hands its read
// transaction to Tiers, so tier facts come from the same snapshot as the rest of the manifest.
type Queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Tiers decides the tiers of the live catalog files of a destination's sources. Phase 3's
// evaluator (internal/tiers) implements it; AllFull is the Phase 2 default.
type Tiers interface {
	// Decide returns the decision function of the live files of source sourceID at destination
	// destinationID, reading what it needs through q (the manifest's read transaction). The
	// function is called with catalog file ids of that source.
	Decide(ctx context.Context, q Queryer, destinationID, sourceID int64) (func(fileID int64) Decision, error)
}

// AllFull is the Tiers of Phase 2 (and of an install without rules, design D1): every file is
// full by the built-in fallback.
type AllFull struct{}

// Decide implements Tiers.
func (AllFull) Decide(context.Context, Queryer, int64, int64) (func(int64) Decision, error) {
	return func(int64) Decision { return Decision{Tier: TierFull, RuleName: FallbackRuleName} }, nil
}
