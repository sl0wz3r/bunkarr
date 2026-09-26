package plex

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The library listing of the Plex index (design §4.6, §6.3), ported from Dupearr's library.go.

// Item types of GET /library/sections/{key}/all?type=N.
const (
	TypeMovie   = 1
	TypeShow    = 2
	TypeSeason  = 3
	TypeEpisode = 4
	TypeArtist  = 8
	TypeAlbum   = 9
	TypeTrack   = 10
)

// DefaultPageSize is X-Plex-Container-Size of a listing page.
const DefaultPageSize = 500

// MaxItemsPerType bounds the rows one listing reads (a runaway server).
const MaxItemsPerType = 2_000_000

// ErrIncomplete means a listing ended before the total the server reported.
var ErrIncomplete = errors.New("the library listing ended before its total")

// TypesOf returns the item types a section of the given type lists, parents first: movie → 1;
// show → 2, 3, 4; artist → 8, 9, 10. Other sections (photo) list nothing.
func TypesOf(sectionType string) []int {
	switch sectionType {
	case "movie":
		return []int{TypeMovie}
	case "show":
		return []int{TypeShow, TypeSeason, TypeEpisode}
	case "artist":
		return []int{TypeArtist, TypeAlbum, TypeTrack}
	}
	return nil
}

// Item is one row of a library listing. Genres are not read (design D16: Plex's Genre[] of a
// listing is truncated).
type Item struct {
	RatingKey string
	// Type is movie, show, season, episode, artist, album or track.
	Type string
	// Index and ParentIndex: a season's number; an episode's (or track's) number and its season's
	// (album's). nil when Plex sends none.
	Index       *int
	ParentIndex *int
	// ParentRatingKey and GrandparentRatingKey are the season and show (album and artist) keys.
	ParentRatingKey      string
	GrandparentRatingKey string
	GUID                 string
	// GUIDs are the Guid[] ids ("imdb://tt0063350", "tmdb://10331", "tvdb://1831").
	GUIDs   []string
	Title   string
	AddedAt *time.Time
	// Files are the Media[].Part[].file paths, as Plex sees them.
	Files []string
}

type wireListing struct {
	MediaContainer *struct {
		Size      *flexInt `json:"size"`
		TotalSize *flexInt `json:"totalSize"`
		Offset    *flexInt `json:"offset"`
		Metadata  []struct {
			RatingKey            flexString  `json:"ratingKey"`
			Type                 string      `json:"type"`
			Index                *flexInt    `json:"index"`
			ParentIndex          *flexInt    `json:"parentIndex"`
			ParentRatingKey      flexString  `json:"parentRatingKey"`
			GrandparentRatingKey flexString  `json:"grandparentRatingKey"`
			GUID                 string      `json:"guid"`
			Title                string      `json:"title"`
			AddedAt              *flexInt    `json:"addedAt"`
			Guid                 list[wGUID] `json:"Guid"`
			Media                list[struct {
				Part list[struct {
					File string `json:"file"`
				}] `json:"Part"`
			}] `json:"Media"`
		} `json:"Metadata"`
	} `json:"MediaContainer"`
}

type wGUID struct {
	ID string `json:"id"`
}

// ListOptions configures AllItems.
type ListOptions struct {
	// PageSize overrides DefaultPageSize (tests).
	PageSize int
}

// AllItems lists every item of one type in a section: GET /library/sections/{key}/all?type=N&
// includeGuids=1, paged by X-Plex-Container-Start and X-Plex-Container-Size and advancing by the
// rows actually returned. fn is called for each row. It fails with ErrIncomplete when a page comes
// back empty before the total the first page reported (a listing that shrinks while it is read),
// so an index refresh never takes a partial listing for the library.
func (c *Client) AllItems(ctx context.Context, sectionKey string, typ int, o ListOptions, fn func(Item) error) error {
	if !validSectionKey(sectionKey) {
		// The key usually comes from the server's own /library/sections answer, so it is never
		// copied into the error (S16): the path is a template and the cause a fixed text.
		return &Error{Method: http.MethodGet, Path: PathSections + "/{key}/all",
			Err: fmt.Errorf("%w: a section has an invalid key", ErrInvalidArgument)}
	}
	size := o.PageSize
	if size <= 0 {
		size = DefaultPageSize
	}
	path := PathSections + "/" + sectionKey + "/all"
	total := -1
	read := 0
	for start := 0; ; {
		q := url.Values{
			"type":                   {strconv.Itoa(typ)},
			"includeGuids":           {"1"},
			"X-Plex-Container-Start": {strconv.Itoa(start)},
			"X-Plex-Container-Size":  {strconv.Itoa(size)},
		}
		var body wireListing
		if err := c.getQuery(ctx, path, q, true, &body); err != nil {
			return err
		}
		fail := func(cause error) error {
			return &Error{Method: http.MethodGet, Path: path, StatusCode: http.StatusOK, Err: cause}
		}
		mc := body.MediaContainer
		if mc == nil {
			return fail(fmt.Errorf("%w: no MediaContainer in the response", ErrNotPlex))
		}
		if total < 0 && mc.TotalSize != nil {
			total = mc.TotalSize.Int()
		}
		for _, m := range mc.Metadata {
			it := Item{RatingKey: m.RatingKey.String(), Type: m.Type, ParentRatingKey: m.ParentRatingKey.String(),
				GrandparentRatingKey: m.GrandparentRatingKey.String(), GUID: strings.TrimSpace(m.GUID), Title: m.Title}
			if it.RatingKey == "" {
				return fail(fmt.Errorf("%w: a row has no ratingKey", ErrNotPlex))
			}
			if m.Index != nil {
				v := m.Index.Int()
				it.Index = &v
			}
			if m.ParentIndex != nil {
				v := m.ParentIndex.Int()
				it.ParentIndex = &v
			}
			if m.AddedAt != nil && *m.AddedAt > 0 {
				t := unixTime(*m.AddedAt)
				it.AddedAt = &t
			}
			for _, g := range m.Guid {
				if id := strings.TrimSpace(g.ID); id != "" {
					it.GUIDs = append(it.GUIDs, id)
				}
			}
			for _, md := range m.Media {
				for _, p := range md.Part {
					if p.File != "" {
						it.Files = append(it.Files, p.File)
					}
				}
			}
			if err := fn(it); err != nil {
				return err
			}
		}
		n := len(mc.Metadata)
		read += n
		start += n
		if read > MaxItemsPerType {
			return fail(fmt.Errorf("%w: more than %d items", ErrNotPlex, MaxItemsPerType))
		}
		switch {
		case total >= 0 && start >= total:
			return nil
		case n == 0 && total > 0:
			return fail(fmt.Errorf("%w: %d of %d rows", ErrIncomplete, start, total))
		case n == 0 || (total < 0 && n < size):
			return nil
		}
	}
}

// validSectionKey accepts a section key made of digits (Plex's section ids).
func validSectionKey(s string) bool {
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
