package mediaindex

import (
	"cmp"
	"context"
	"errors"
	"path"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/integrations/arr"
)

// fetched is one *arr item as a refresh read it, normalized across the three applications.
type fetched struct {
	kind       string
	arrID      int64
	title      string
	year       int
	ext        ExternalIDs
	path       string
	rootFolder string
	qp, mp     int64
	monitored  bool
	tags       []int64
	genres     []string
	added      *time.Time
	detail     ItemDetail
	files      []fetchedFile
}

// fetchedFile is one *arr file of a fetched item.
type fetchedFile struct {
	arrFileID int64
	path      string
	size      int64
	quality   string
	dateAdded *time.Time
	detail    FileDetail
}

// fetcher reads items from one *arr with the client's allow-listed requests and counts them.
type fetcher struct {
	c        *arr.Client
	kind     arr.Kind
	requests atomic.Int64
	// concurrency bounds the per-item requests running at once.
	concurrency int
}

func (f *fetcher) count(n int) { f.requests.Add(int64(n)) }

// meta is an *arr's metadata as fetched.
type meta struct {
	qualityProfiles  []arr.QualityProfile
	metadataProfiles []arr.MetadataProfile
	rootFolders      []arr.RootFolder
	tags             []arr.Tag
	mediaManagement  *arr.MediaManagement
}

// fetchMeta reads the quality profiles, root folders and tags (plus Lidarr's metadata profiles)
// and, with mm, the media management settings.
func (f *fetcher) fetchMeta(ctx context.Context, mm bool) (meta, error) {
	var (
		m   meta
		err error
	)
	f.count(1)
	if m.qualityProfiles, err = f.c.QualityProfiles(ctx); err != nil {
		return meta{}, err
	}
	f.count(1)
	if m.rootFolders, err = f.c.RootFolders(ctx); err != nil {
		return meta{}, err
	}
	f.count(1)
	if m.tags, err = f.c.Tags(ctx); err != nil {
		return meta{}, err
	}
	if f.kind == arr.KindLidarr {
		f.count(1)
		if m.metadataProfiles, err = f.c.MetadataProfiles(ctx); err != nil {
			return meta{}, err
		}
	}
	if mm {
		f.count(1)
		v, err := f.c.MediaManagement(ctx)
		if err != nil {
			return meta{}, err
		}
		m.mediaManagement = &v
	}
	return m, nil
}

// fetchAll reads every item of the *arr and calls fn for each (from one goroutine), as they
// arrive: Radarr's movie list streams in with the files nested; Sonarr's series and Lidarr's
// artists are listed first, then each one's files are read, f.concurrency at a time.
func (f *fetcher) fetchAll(ctx context.Context, fn func(fetched) error) error {
	switch f.kind {
	case arr.KindRadarr:
		f.count(1)
		return f.c.EachMovie(ctx, func(m arr.Movie) error {
			var files []arr.MovieFile
			if m.MovieFile != nil && m.MovieFile.ID != 0 {
				files = []arr.MovieFile{*m.MovieFile}
			}
			return fn(fromMovie(m, files))
		})
	case arr.KindSonarr:
		f.count(1)
		list, err := f.c.AllSeries(ctx)
		if err != nil {
			return err
		}
		return pool(ctx, f.concurrency, len(list), func(ctx context.Context, i int) (fetched, error) {
			return f.seriesFiles(ctx, list[i])
		}, fn)
	case arr.KindLidarr:
		f.count(1)
		list, err := f.c.Artists(ctx)
		if err != nil {
			return err
		}
		return pool(ctx, f.concurrency, len(list), func(ctx context.Context, i int) (fetched, error) {
			return f.artistFiles(ctx, list[i])
		}, fn)
	}
	return errors.New("unknown *arr kind")
}

// fetchOne reads one item and its files. found is false when the *arr answered 404.
func (f *fetcher) fetchOne(ctx context.Context, id int64) (item fetched, found bool, err error) {
	switch f.kind {
	case arr.KindRadarr:
		f.count(1)
		m, err := f.c.Movie(ctx, id)
		if err != nil {
			return fetched{}, false, notFound(err)
		}
		var files []arr.MovieFile
		switch {
		case m.MovieFile != nil && m.MovieFile.ID != 0:
			files = []arr.MovieFile{*m.MovieFile}
		case m.MovieFileID != 0:
			f.count(1)
			if files, err = f.c.MovieFiles(ctx, id); err != nil {
				return fetched{}, false, err
			}
		}
		return fromMovie(m, files), true, nil
	case arr.KindSonarr:
		f.count(1)
		s, err := f.c.Series(ctx, id)
		if err != nil {
			return fetched{}, false, notFound(err)
		}
		it, err := f.seriesFiles(ctx, s)
		return it, err == nil, err
	case arr.KindLidarr:
		f.count(1)
		a, err := f.c.Artist(ctx, id)
		if err != nil {
			return fetched{}, false, notFound(err)
		}
		it, err := f.artistFiles(ctx, a)
		return it, err == nil, err
	}
	return fetched{}, false, errors.New("unknown *arr kind")
}

// errItemGone marks a 404 of GET movie/{id} (series, artist).
var errItemGone = errors.New("the *arr has no such item")

// notFound turns a 404 into errItemGone (the caller checks system/status before it believes it).
func notFound(err error) error {
	if errors.Is(err, arr.ErrNotFound) {
		return errItemGone
	}
	return err
}

func (f *fetcher) seriesFiles(ctx context.Context, s arr.Series) (fetched, error) {
	f.count(2)
	efs, err := f.c.EpisodeFiles(ctx, s.ID)
	if err != nil {
		return fetched{}, err
	}
	eps, err := f.c.Episodes(ctx, s.ID)
	if err != nil {
		return fetched{}, err
	}
	return fromSeries(s, eps, efs), nil
}

func (f *fetcher) artistFiles(ctx context.Context, a arr.Artist) (fetched, error) {
	f.count(2)
	tfs, err := f.c.TrackFiles(ctx, a.ID)
	if err != nil {
		return fetched{}, err
	}
	albums, err := f.c.Albums(ctx, a.ID)
	if err != nil {
		return fetched{}, err
	}
	return fromArtist(a, albums, tfs), nil
}

// pool runs fetch for 0..n-1 with at most workers at once and hands every result to fn from the
// calling goroutine. It stops at the first error (of fetch or fn) and returns it.
func pool(ctx context.Context, workers, n int, fetch func(context.Context, int) (fetched, error), fn func(fetched) error) error {
	if workers < 1 {
		workers = 1
	}
	pctx := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		item fetched
		err  error
	}
	next := make(chan int)
	results := make(chan result)
	var wg sync.WaitGroup
	for range min(workers, max(n, 1)) {
		wg.Go(func() {
			for i := range next {
				it, err := fetch(ctx, i)
				select {
				case results <- result{it, err}:
				case <-ctx.Done():
					return
				}
			}
		})
	}
	go func() {
		defer close(next)
		for i := range n {
			select {
			case next <- i:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()
	var first error
	for r := range results {
		if first != nil {
			continue
		}
		if r.err != nil {
			first = r.err
			cancel()
			continue
		}
		if err := fn(r.item); err != nil {
			first = err
			cancel()
		}
	}
	if first == nil {
		first = pctx.Err()
	}
	return first
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func nonNilIDs(v []int64) []int64 {
	if v == nil {
		return []int64{}
	}
	out := slices.Clone(v)
	slices.Sort(out)
	return slices.Compact(out)
}

func nonNilStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return slices.Clone(v)
}

func boolPtr(b bool) *bool { return &b }

// rootOf returns an item's root folder: the one the *arr reports, else its folder's parent.
func rootOf(root, folder string) string {
	if root = strings.TrimSpace(root); root != "" {
		return strings.TrimRight(root, "/")
	}
	if folder == "" {
		return ""
	}
	return path.Dir(folder)
}

func fromMovie(m arr.Movie, files []arr.MovieFile) fetched {
	folder := m.Path
	if folder == "" {
		folder = m.FolderName // Radarr's folderName holds the full path too (spike)
	}
	it := fetched{
		kind: KindMovie, arrID: m.ID, title: m.Title, year: m.Year,
		ext:  ExternalIDs{TMDB: m.TMDBID, IMDB: m.IMDBID},
		path: folder, rootFolder: rootOf(m.RootFolderPath, folder),
		qp: m.QualityProfileID, monitored: m.Monitored,
		tags: nonNilIDs(m.Tags), genres: nonNilStrings(m.Genres), added: timePtr(m.Added),
		detail: ItemDetail{HasFile: m.HasFile || m.MovieFileID != 0, MinimumAvailability: m.MinimumAvailability},
	}
	for _, mf := range files {
		if mf.ID == 0 {
			continue
		}
		it.files = append(it.files, fetchedFile{arrFileID: mf.ID, path: mf.Path, size: mf.Size, quality: mf.Quality.Name,
			dateAdded: timePtr(mf.DateAdded), detail: FileDetail{RelativePath: mf.RelativePath}})
	}
	if len(it.files) > 0 {
		it.detail.HasFile = true
	}
	return it
}

func fromSeries(s arr.Series, eps []arr.Episode, efs []arr.EpisodeFile) fetched {
	d := ItemDetail{
		HasFile:           s.Statistics.EpisodeFileCount > 0 || len(efs) > 0,
		SeriesType:        s.SeriesType,
		SeasonFolder:      boolPtr(s.SeasonFolder),
		MonitorNewItems:   s.MonitorNewItems,
		UseSceneNumbering: boolPtr(s.UseSceneNumbering),
		LanguageProfileID: s.LanguageProfileID,
		Seasons:           []SeasonDetail{},
		Episodes:          []EpisodeDetail{},
	}
	for _, se := range s.Seasons {
		d.Seasons = append(d.Seasons, SeasonDetail{SeasonNumber: se.SeasonNumber, Monitored: se.Monitored})
	}
	slices.SortFunc(d.Seasons, func(a, b SeasonDetail) int { return cmp.Compare(a.SeasonNumber, b.SeasonNumber) })
	byFile := map[int64][]FileEpisode{}
	for _, e := range eps {
		d.Episodes = append(d.Episodes, EpisodeDetail{Season: e.SeasonNumber, Episode: e.EpisodeNumber, Monitored: e.Monitored})
		if e.EpisodeFileID != 0 {
			byFile[e.EpisodeFileID] = append(byFile[e.EpisodeFileID], FileEpisode{EpisodeID: e.ID, SeasonNumber: e.SeasonNumber,
				EpisodeNumber: e.EpisodeNumber, TVDBID: e.TVDBID})
		}
	}
	slices.SortFunc(d.Episodes, func(a, b EpisodeDetail) int {
		return cmp.Or(cmp.Compare(a.Season, b.Season), cmp.Compare(a.Episode, b.Episode))
	})
	it := fetched{
		kind: KindSeries, arrID: s.ID, title: s.Title, year: s.Year,
		ext:  ExternalIDs{TVDB: s.TVDBID, TMDB: s.TMDBID, IMDB: s.IMDBID, TVMaze: s.TVMazeID},
		path: s.Path, rootFolder: rootOf(s.RootFolderPath, s.Path),
		qp: s.QualityProfileID, monitored: s.Monitored,
		tags: nonNilIDs(s.Tags), genres: nonNilStrings(s.Genres), added: timePtr(s.Added),
		detail: d,
	}
	for _, ef := range efs {
		if ef.ID == 0 {
			continue
		}
		episodes := byFile[ef.ID]
		slices.SortFunc(episodes, func(a, b FileEpisode) int {
			return cmp.Or(cmp.Compare(a.SeasonNumber, b.SeasonNumber), cmp.Compare(a.EpisodeNumber, b.EpisodeNumber))
		})
		it.files = append(it.files, fetchedFile{arrFileID: ef.ID, path: ef.Path, size: ef.Size, quality: ef.Quality.Name,
			dateAdded: timePtr(ef.DateAdded), detail: FileDetail{RelativePath: ef.RelativePath, Episodes: episodes}})
	}
	return it
}

func fromArtist(a arr.Artist, albums []arr.Album, tfs []arr.TrackFile) fetched {
	d := ItemDetail{
		HasFile:         a.Statistics.TrackFileCount > 0 || len(tfs) > 0,
		MonitorNewItems: a.MonitorNewItems,
		Albums:          []AlbumDetail{},
	}
	byID := map[int64]arr.Album{}
	for _, al := range albums {
		byID[al.ID] = al
		d.Albums = append(d.Albums, AlbumDetail{ID: al.ID, MBID: al.ForeignAlbumID, Title: al.Title, Monitored: al.Monitored})
	}
	slices.SortFunc(d.Albums, func(x, y AlbumDetail) int { return cmp.Compare(x.ID, y.ID) })
	it := fetched{
		kind: KindArtist, arrID: a.ID, title: a.ArtistName,
		ext:  ExternalIDs{MBID: a.ForeignArtistID},
		path: a.Path, rootFolder: rootOf(a.RootFolderPath, a.Path),
		qp: a.QualityProfileID, mp: a.MetadataProfileID, monitored: a.Monitored,
		tags: nonNilIDs(a.Tags), genres: nonNilStrings(a.Genres), added: timePtr(a.Added),
		detail: d,
	}
	for _, tf := range tfs {
		if tf.ID == 0 {
			continue
		}
		fd := FileDetail{}
		if rel, ok := strings.CutPrefix(tf.Path, strings.TrimRight(a.Path, "/")+"/"); ok {
			fd.RelativePath = rel
		}
		if tf.AlbumID != 0 {
			al := byID[tf.AlbumID]
			fd.Album = &FileAlbum{ID: tf.AlbumID, MBID: al.ForeignAlbumID, Title: al.Title}
		}
		it.files = append(it.files, fetchedFile{arrFileID: tf.ID, path: tf.Path, size: tf.Size, quality: tf.Quality.Name,
			dateAdded: timePtr(tf.DateAdded), detail: fd})
	}
	return it
}
