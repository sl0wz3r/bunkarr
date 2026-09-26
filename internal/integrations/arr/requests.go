package arr

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// The allow-list of phase2-3.md §4.3. Every exported method below sends exactly one request of
// that table; nothing else can be sent (S16).

// only refuses a request of another application before anything is sent.
func (c *Client) only(k Kind, method, p string) error {
	if c.kind != k {
		return &Error{Method: method, Path: c.kind.APIPrefix() + "/" + p, Err: fmt.Errorf("%w: %s is a %s request", ErrWrongKind, p, k.AppName())}
	}
	return nil
}

// Status sends GET system/status and checks that the server is the client's application: an
// appName other than the kind's ("Radarr" for a Radarr client) returns an *Error wrapping
// ErrWrongApp, together with the status as decoded (so a caller can name the application it
// found). A 404 there also means another application (a Lidarr URL saved as Sonarr has no
// /api/v3), and is reported as ErrWrongApp too.
func (c *Client) Status(ctx context.Context) (Status, error) {
	r := c.api(http.MethodGet, "system/status", nil)
	var s Status
	if err := c.getJSON(ctx, r, &s); err != nil {
		if e, ok := err.(*Error); ok && e.StatusCode == http.StatusNotFound {
			e.Err = fmt.Errorf("%w: it has no %s (is it %s?)", ErrWrongApp, r.path, c.kind.AppName())
		}
		return Status{}, err
	}
	if !strings.EqualFold(strings.TrimSpace(s.AppName), c.kind.AppName()) {
		other := "another application"
		if appNamePattern.MatchString(s.AppName) {
			other = s.AppName
		}
		return s, c.fail(r, http.StatusOK, fmt.Errorf("%w: the server is %s, not %s", ErrWrongApp, other, c.kind.AppName()))
	}
	return s, nil
}

// QualityProfiles sends GET qualityprofile.
func (c *Client) QualityProfiles(ctx context.Context) ([]QualityProfile, error) {
	return listJSON[QualityProfile](ctx, c, c.api(http.MethodGet, "qualityprofile", nil))
}

// RootFolders sends GET rootfolder.
func (c *Client) RootFolders(ctx context.Context) ([]RootFolder, error) {
	return listJSON[RootFolder](ctx, c, c.api(http.MethodGet, "rootfolder", nil))
}

// Tags sends GET tag.
func (c *Client) Tags(ctx context.Context) ([]Tag, error) {
	return listJSON[Tag](ctx, c, c.api(http.MethodGet, "tag", nil))
}

// MediaManagement sends GET config/mediamanagement.
func (c *Client) MediaManagement(ctx context.Context) (MediaManagement, error) {
	var m MediaManagement
	err := c.getJSON(ctx, c.api(http.MethodGet, "config/mediamanagement", nil), &m)
	return m, err
}

// Backups sends GET system/backup. Entries are returned as the *arr listed them, newest first
// as it sorts them; ValidateBackup checks one before its location is built.
func (c *Client) Backups(ctx context.Context) ([]Backup, error) {
	return listJSON[Backup](ctx, c, c.api(http.MethodGet, "system/backup", nil))
}

// StartBackup sends POST command {"name":"Backup"}, which makes the *arr create a manual backup
// zip in its config folder. It is the only write the client can make (S16); a dry run never sends
// it (S9).
func (c *Client) StartBackup(ctx context.Context) (Command, error) {
	r := c.api(http.MethodPost, "command", nil)
	r.body = []byte(`{"name":"Backup"}`)
	var cmd Command
	err := c.getJSON(ctx, r, &cmd)
	return cmd, err
}

// Command sends GET command/{id}.
func (c *Client) Command(ctx context.Context, id int64) (Command, error) {
	var cmd Command
	err := c.getJSON(ctx, c.api(http.MethodGet, "command/"+strconv.FormatInt(id, 10), nil), &cmd)
	return cmd, err
}

// list builds a full-list request (the list timeout and cap).
func (c *Client) list(p string) request {
	r := c.api(http.MethodGet, p, nil)
	r.timeout, r.limit = c.lsTmo, c.maxL
	return r
}

// byID builds GET <p>?<param>=<id>.
func (c *Client) byID(p, param string, id int64) request {
	return c.api(http.MethodGet, p, url.Values{param: {strconv.FormatInt(id, 10)}})
}

// EachMovie sends GET movie (Radarr) and calls fn for each movie as the list streams in (each
// movie holds its nested movieFile). It stops at fn's first error and returns it.
func (c *Client) EachMovie(ctx context.Context, fn func(Movie) error) error {
	if err := c.only(KindRadarr, http.MethodGet, "movie"); err != nil {
		return err
	}
	return eachJSON(ctx, c, c.list("movie"), fn)
}

// Movies sends GET movie (Radarr) and returns every movie.
func (c *Client) Movies(ctx context.Context) ([]Movie, error) {
	if err := c.only(KindRadarr, http.MethodGet, "movie"); err != nil {
		return nil, err
	}
	return listJSON[Movie](ctx, c, c.list("movie"))
}

// Movie sends GET movie/{id} (Radarr). An unknown id returns an *Error wrapping ErrNotFound.
func (c *Client) Movie(ctx context.Context, id int64) (Movie, error) {
	var m Movie
	if err := c.only(KindRadarr, http.MethodGet, "movie/{id}"); err != nil {
		return m, err
	}
	err := c.getJSON(ctx, c.api(http.MethodGet, "movie/"+strconv.FormatInt(id, 10), nil), &m)
	return m, err
}

// MovieFiles sends GET moviefile?movieId= (Radarr).
func (c *Client) MovieFiles(ctx context.Context, movieID int64) ([]MovieFile, error) {
	if err := c.only(KindRadarr, http.MethodGet, "moviefile"); err != nil {
		return nil, err
	}
	return listJSON[MovieFile](ctx, c, c.byID("moviefile", "movieId", movieID))
}

// EachSeries sends GET series (Sonarr) and calls fn for each series as the list streams in.
func (c *Client) EachSeries(ctx context.Context, fn func(Series) error) error {
	if err := c.only(KindSonarr, http.MethodGet, "series"); err != nil {
		return err
	}
	return eachJSON(ctx, c, c.list("series"), fn)
}

// AllSeries sends GET series (Sonarr) and returns every series.
func (c *Client) AllSeries(ctx context.Context) ([]Series, error) {
	if err := c.only(KindSonarr, http.MethodGet, "series"); err != nil {
		return nil, err
	}
	return listJSON[Series](ctx, c, c.list("series"))
}

// Series sends GET series/{id} (Sonarr). An unknown id returns an *Error wrapping ErrNotFound.
func (c *Client) Series(ctx context.Context, id int64) (Series, error) {
	var s Series
	if err := c.only(KindSonarr, http.MethodGet, "series/{id}"); err != nil {
		return s, err
	}
	err := c.getJSON(ctx, c.api(http.MethodGet, "series/"+strconv.FormatInt(id, 10), nil), &s)
	return s, err
}

// EpisodeFiles sends GET episodefile?seriesId= (Sonarr).
func (c *Client) EpisodeFiles(ctx context.Context, seriesID int64) ([]EpisodeFile, error) {
	if err := c.only(KindSonarr, http.MethodGet, "episodefile"); err != nil {
		return nil, err
	}
	return listJSON[EpisodeFile](ctx, c, c.byID("episodefile", "seriesId", seriesID))
}

// Episodes sends GET episode?seriesId= (Sonarr): every episode of every season, which maps the
// episode files to season and episode numbers.
func (c *Client) Episodes(ctx context.Context, seriesID int64) ([]Episode, error) {
	if err := c.only(KindSonarr, http.MethodGet, "episode"); err != nil {
		return nil, err
	}
	return listJSON[Episode](ctx, c, c.byID("episode", "seriesId", seriesID))
}

// EachArtist sends GET artist (Lidarr) and calls fn for each artist as the list streams in.
func (c *Client) EachArtist(ctx context.Context, fn func(Artist) error) error {
	if err := c.only(KindLidarr, http.MethodGet, "artist"); err != nil {
		return err
	}
	return eachJSON(ctx, c, c.list("artist"), fn)
}

// Artists sends GET artist (Lidarr) and returns every artist.
func (c *Client) Artists(ctx context.Context) ([]Artist, error) {
	if err := c.only(KindLidarr, http.MethodGet, "artist"); err != nil {
		return nil, err
	}
	return listJSON[Artist](ctx, c, c.list("artist"))
}

// Artist sends GET artist/{id} (Lidarr). An unknown id returns an *Error wrapping ErrNotFound.
func (c *Client) Artist(ctx context.Context, id int64) (Artist, error) {
	var a Artist
	if err := c.only(KindLidarr, http.MethodGet, "artist/{id}"); err != nil {
		return a, err
	}
	err := c.getJSON(ctx, c.api(http.MethodGet, "artist/"+strconv.FormatInt(id, 10), nil), &a)
	return a, err
}

// TrackFiles sends GET trackfile?artistId= (Lidarr).
func (c *Client) TrackFiles(ctx context.Context, artistID int64) ([]TrackFile, error) {
	if err := c.only(KindLidarr, http.MethodGet, "trackfile"); err != nil {
		return nil, err
	}
	return listJSON[TrackFile](ctx, c, c.byID("trackfile", "artistId", artistID))
}

// Albums sends GET album?artistId= (Lidarr).
func (c *Client) Albums(ctx context.Context, artistID int64) ([]Album, error) {
	if err := c.only(KindLidarr, http.MethodGet, "album"); err != nil {
		return nil, err
	}
	return listJSON[Album](ctx, c, c.byID("album", "artistId", artistID))
}

// MetadataProfiles sends GET metadataprofile (Lidarr).
func (c *Client) MetadataProfiles(ctx context.Context) ([]MetadataProfile, error) {
	if err := c.only(KindLidarr, http.MethodGet, "metadataprofile"); err != nil {
		return nil, err
	}
	return listJSON[MetadataProfile](ctx, c, c.api(http.MethodGet, "metadataprofile", nil))
}
