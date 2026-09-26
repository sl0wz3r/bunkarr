package arr

import (
	"encoding/json"
	"time"
)

// The types below decode only the fields Bunkarr uses (the index, tier facts, manifests and
// backups). Field comments name the quirks the fixture spike found (phase2-3.md §4.3).

// Status is GET system/status.
type Status struct {
	// AppName is "Sonarr", "Radarr" or "Lidarr"; Status checks it against the client's kind.
	AppName      string `json:"appName"`
	InstanceName string `json:"instanceName"`
	Version      string `json:"version"`
	// URLBase is the *arr's configured URL base ("" or e.g. "/radarr").
	URLBase string `json:"urlBase"`
	// Authentication is the UI authentication method: "none", "basic", "forms" or "external".
	Authentication string `json:"authentication"`
}

// QualityProfile is an element of GET qualityprofile.
type QualityProfile struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// Cutoff is a quality id, or a quality group id (≥ 1000) when the cutoff is a group.
	Cutoff         int64 `json:"cutoff"`
	UpgradeAllowed bool  `json:"upgradeAllowed"`
}

// MetadataProfile is an element of Lidarr's GET metadataprofile.
type MetadataProfile struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// RootFolder is an element of GET rootfolder.
type RootFolder struct {
	ID   int64  `json:"id"`
	Path string `json:"path"`
	// Name is Lidarr's root folder name ("" for Sonarr and Radarr).
	Name string `json:"name"`
	// Accessible is false when the *arr cannot read the folder (an unmounted share): a refresh
	// never deletes the index's files under it (S10).
	Accessible bool  `json:"accessible"`
	FreeSpace  int64 `json:"freeSpace"`
}

// Tag is an element of GET tag. Items store tag ids; webhooks send labels.
type Tag struct {
	ID    int64  `json:"id"`
	Label string `json:"label"`
}

// MediaManagement is GET config/mediamanagement (the two settings Bunkarr checks, §6.1).
type MediaManagement struct {
	// RecycleBin is the recycle bin folder as the *arr sees it ("" = none). Inside a source it
	// doubles every upgrade's transfer unless excluded.
	RecycleBin            string `json:"recycleBin"`
	RecycleBinCleanupDays int    `json:"recycleBinCleanupDays"`
	// FileDate is "none", "localAirDate"/"utcAirDate" (Sonarr), "cinemas"/"release" (Radarr) or
	// "albumReleaseDate" (Lidarr). Anything but "none" gives every version of a title the same
	// mtime.
	FileDate string `json:"fileDate"`
}

// Backup is an element of GET system/backup. Only these fields are decoded: the path field is
// never read (S18); a backup's location is built from Type and Name (see ValidateBackup).
type Backup struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// Type is "manual", "scheduled" or "update".
	Type string    `json:"type"`
	Size int64     `json:"size"`
	Time time.Time `json:"time"`
}

// Command is a queued or finished *arr command (POST command, GET command/{id}).
type Command struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// Status is "queued", "started", "completed", "failed", "aborted", "cancelled" or "orphaned".
	Status string `json:"status"`
	// Result is "unknown" until the command ends, then "successful" or "unsuccessful".
	Result  string    `json:"result"`
	Message string    `json:"message"`
	Queued  time.Time `json:"queued"`
	Started time.Time `json:"started"`
	Ended   time.Time `json:"ended"`
}

// Quality is the quality of an *arr file ("Bluray-1080p").
type Quality struct {
	Name string
}

// UnmarshalJSON decodes {"quality":{"name":…},"revision":…}.
func (q *Quality) UnmarshalJSON(b []byte) error {
	var w struct {
		Quality struct {
			Name string `json:"name"`
		} `json:"quality"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	q.Name = w.Quality.Name
	return nil
}

// MarshalJSON encodes the quality name as a string.
func (q Quality) MarshalJSON() ([]byte, error) { return json.Marshal(q.Name) }

// Movie is a Radarr movie (GET movie, GET movie/{id}).
type Movie struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	Year  int    `json:"year"`
	// Path is the movie folder as Radarr sees it. FolderName holds the same full path (spike).
	Path           string `json:"path"`
	FolderName     string `json:"folderName"`
	RootFolderPath string `json:"rootFolderPath"`
	// HasFile is false and MovieFileID 0 (and MovieFile nil) for a movie without a file.
	HasFile             bool       `json:"hasFile"`
	MovieFileID         int64      `json:"movieFileId"`
	MovieFile           *MovieFile `json:"movieFile"`
	TMDBID              int64      `json:"tmdbId"`
	IMDBID              string     `json:"imdbId"`
	QualityProfileID    int64      `json:"qualityProfileId"`
	Monitored           bool       `json:"monitored"`
	MinimumAvailability string     `json:"minimumAvailability"`
	// Tags are tag ids (labels come from GET tag).
	Tags       []int64   `json:"tags"`
	Genres     []string  `json:"genres"`
	Added      time.Time `json:"added"`
	SizeOnDisk int64     `json:"sizeOnDisk"`
}

// MovieFile is a Radarr movie file (nested in a movie, or GET moviefile?movieId=).
type MovieFile struct {
	ID           int64     `json:"id"`
	MovieID      int64     `json:"movieId"`
	RelativePath string    `json:"relativePath"`
	Path         string    `json:"path"`
	Size         int64     `json:"size"`
	DateAdded    time.Time `json:"dateAdded"`
	Quality      Quality   `json:"quality"`
}

// Series is a Sonarr series (GET series, GET series/{id}).
type Series struct {
	ID               int64  `json:"id"`
	Title            string `json:"title"`
	Year             int    `json:"year"`
	Path             string `json:"path"`
	RootFolderPath   string `json:"rootFolderPath"`
	TVDBID           int64  `json:"tvdbId"`
	TMDBID           int64  `json:"tmdbId"`
	IMDBID           string `json:"imdbId"`
	TVMazeID         int64  `json:"tvMazeId"`
	QualityProfileID int64  `json:"qualityProfileId"`
	// LanguageProfileID is a Sonarr 3 field that Sonarr 4 still reports (1).
	LanguageProfileID int64            `json:"languageProfileId"`
	Monitored         bool             `json:"monitored"`
	MonitorNewItems   string           `json:"monitorNewItems"`
	SeriesType        string           `json:"seriesType"`
	SeasonFolder      bool             `json:"seasonFolder"`
	UseSceneNumbering bool             `json:"useSceneNumbering"`
	Tags              []int64          `json:"tags"`
	Genres            []string         `json:"genres"`
	Added             time.Time        `json:"added"`
	Seasons           []Season         `json:"seasons"`
	Statistics        SeriesStatistics `json:"statistics"`
}

// Season is a season of a Sonarr series.
type Season struct {
	SeasonNumber int  `json:"seasonNumber"`
	Monitored    bool `json:"monitored"`
}

// SeriesStatistics are a Sonarr series' file counts.
type SeriesStatistics struct {
	EpisodeFileCount int   `json:"episodeFileCount"`
	SizeOnDisk       int64 `json:"sizeOnDisk"`
}

// EpisodeFile is a Sonarr episode file (GET episodefile?seriesId=). It carries no episode
// numbers: files are mapped to episodes through Episode.EpisodeFileID (a multi-episode file is
// named by several episodes).
type EpisodeFile struct {
	ID           int64     `json:"id"`
	SeriesID     int64     `json:"seriesId"`
	SeasonNumber int       `json:"seasonNumber"`
	RelativePath string    `json:"relativePath"`
	Path         string    `json:"path"`
	Size         int64     `json:"size"`
	DateAdded    time.Time `json:"dateAdded"`
	Quality      Quality   `json:"quality"`
}

// Episode is a Sonarr episode (GET episode?seriesId=).
type Episode struct {
	ID            int64 `json:"id"`
	SeriesID      int64 `json:"seriesId"`
	TVDBID        int64 `json:"tvdbId"`
	EpisodeFileID int64 `json:"episodeFileId"`
	SeasonNumber  int   `json:"seasonNumber"`
	EpisodeNumber int   `json:"episodeNumber"`
	HasFile       bool  `json:"hasFile"`
	Monitored     bool  `json:"monitored"`
}

// Artist is a Lidarr artist (GET artist, GET artist/{id}).
type Artist struct {
	ID         int64  `json:"id"`
	ArtistName string `json:"artistName"`
	// ForeignArtistID is the MusicBrainz artist id (the API's mbId is null, spike).
	ForeignArtistID   string           `json:"foreignArtistId"`
	Path              string           `json:"path"`
	RootFolderPath    string           `json:"rootFolderPath"`
	QualityProfileID  int64            `json:"qualityProfileId"`
	MetadataProfileID int64            `json:"metadataProfileId"`
	Monitored         bool             `json:"monitored"`
	MonitorNewItems   string           `json:"monitorNewItems"`
	Tags              []int64          `json:"tags"`
	Genres            []string         `json:"genres"`
	Added             time.Time        `json:"added"`
	Statistics        ArtistStatistics `json:"statistics"`
}

// ArtistStatistics are a Lidarr artist's counts.
type ArtistStatistics struct {
	AlbumCount     int   `json:"albumCount"`
	TrackFileCount int   `json:"trackFileCount"`
	SizeOnDisk     int64 `json:"sizeOnDisk"`
}

// Album is a Lidarr album (GET album?artistId=).
type Album struct {
	ID       int64  `json:"id"`
	ArtistID int64  `json:"artistId"`
	Title    string `json:"title"`
	// ForeignAlbumID is the MusicBrainz release group id.
	ForeignAlbumID string    `json:"foreignAlbumId"`
	Monitored      bool      `json:"monitored"`
	ReleaseDate    time.Time `json:"releaseDate"`
}

// TrackFile is a Lidarr track file (GET trackfile?artistId=). It has no relative path.
type TrackFile struct {
	ID        int64     `json:"id"`
	ArtistID  int64     `json:"artistId"`
	AlbumID   int64     `json:"albumId"`
	Path      string    `json:"path"`
	Size      int64     `json:"size"`
	DateAdded time.Time `json:"dateAdded"`
	Quality   Quality   `json:"quality"`
}
