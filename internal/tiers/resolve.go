package tiers

import (
	"cmp"
	"fmt"
	"path"
	"slices"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/mediaindex"
)

// resolvedFlag is a flag with what it covers now (§8.7).
type resolvedFlag struct {
	flag Flag
	// items are the *arr items an *arr flag resolves to: their folders (as Bunkarr sees them)
	// cover every file under them.
	items []*itemInfo
	// path coverage: a path flag, or an *arr flag's last folder when no item resolves.
	hasPath  bool
	sourceID int64
	rel      string
	resolved bool
	reason   string
	// folder is the located folder of the first resolved item (the flag's new last folder).
	folder *catalog.Location
}

// sharesID reports whether an item of kind with ids b is the item a flag with ids a was set on:
// the same TMDB id for a movie, TVDB id for a series, MusicBrainz id for an artist (the IMDb id,
// and for series the TMDB id, when the flag has no primary id).
func sharesID(kind string, a, b mediaindex.ExternalIDs) bool {
	switch kind {
	case mediaindex.KindMovie:
		if a.TMDB != 0 {
			return a.TMDB == b.TMDB
		}
		return a.IMDB != "" && a.IMDB == b.IMDB
	case mediaindex.KindSeries:
		if a.TVDB != 0 {
			return a.TVDB == b.TVDB
		}
		if a.TMDB != 0 {
			return a.TMDB == b.TMDB
		}
		return a.IMDB != "" && a.IMDB == b.IMDB
	case mediaindex.KindArtist:
		return a.MBID != "" && a.MBID == b.MBID
	}
	return false
}

// flagIDText names the id an *arr flag follows ("tmdb 10331").
func flagIDText(kind string, ids mediaindex.ExternalIDs) string {
	switch {
	case kind == mediaindex.KindMovie && ids.TMDB != 0:
		return fmt.Sprintf("tmdb %d", ids.TMDB)
	case kind == mediaindex.KindSeries && ids.TVDB != 0:
		return fmt.Sprintf("tvdb %d", ids.TVDB)
	case kind == mediaindex.KindArtist && ids.MBID != "":
		return "mbid " + ids.MBID
	case ids.TMDB != 0:
		return fmt.Sprintf("tmdb %d", ids.TMDB)
	case ids.IMDB != "":
		return "imdb " + ids.IMDB
	}
	return "no external id"
}

// hasFlagID reports whether ids hold an id an *arr flag of kind can follow.
func hasFlagID(kind string, ids mediaindex.ExternalIDs) bool {
	switch kind {
	case mediaindex.KindMovie:
		return ids.TMDB != 0 || ids.IMDB != ""
	case mediaindex.KindSeries:
		return ids.TVDB != 0 || ids.TMDB != 0 || ids.IMDB != ""
	case mediaindex.KindArtist:
		return ids.MBID != ""
	}
	return false
}

// resolveFlags resolves every flag against the index: an *arr flag covers the files of every
// non-deleted item of its kind, in any integration, that shares its external id (arr_id is only a
// cache); when none resolves it falls back to its last folder. A path flag covers its file or
// folder.
func resolveFlags(fc *factContext, flags []Flag) []*resolvedFlag {
	out := make([]*resolvedFlag, 0, len(flags))
	for _, f := range flags {
		rf := &resolvedFlag{flag: f}
		out = append(out, rf)
		switch f.Kind {
		case FlagKindPath:
			if f.SourceID != nil && f.RelPath != nil {
				rf.hasPath, rf.sourceID, rf.rel = true, *f.SourceID, *f.RelPath
			}
		case FlagKindArr:
			for _, it := range fc.items {
				if it.facts.Kind == f.ArrKind && sharesID(f.ArrKind, f.ExternalIDs, it.facts.ExternalIDs) {
					rf.items = append(rf.items, it)
				}
			}
			sortItems(rf.items)
			if len(rf.items) > 0 {
				rf.resolved = true
				for _, it := range rf.items {
					if it.folder == "" {
						continue
					}
					if locs := fc.locator.Locate(it.folder); len(locs) > 0 && locs[0].Rel != "" {
						loc := locs[0]
						rf.folder = &loc
						break
					}
				}
				continue
			}
			rf.reason = fmt.Sprintf("no item with %s", flagIDText(f.ArrKind, f.ExternalIDs))
			if f.LastSourceID != nil && f.LastRelPath != nil {
				rf.hasPath, rf.sourceID, rf.rel = true, *f.LastSourceID, *f.LastRelPath
				rf.reason += fmt.Sprintf("; it covers its last folder %q", *f.LastRelPath)
			}
		}
	}
	return out
}

func sortItems(items []*itemInfo) {
	slices.SortFunc(items, func(a, b *itemInfo) int { return cmp.Compare(a.facts.ItemID, b.facts.ItemID) })
}

// covers reports whether the flag covers a file.
func (rf *resolvedFlag) covers(f *Facts) bool {
	if rf.hasPath && f.SourceID == rf.sourceID && under(f.RelPath, rf.rel) {
		return true
	}
	for _, it := range rf.items {
		if it.folder != "" && under(f.LocalPath, it.folder) {
			return true
		}
		if f.Arr.Item != nil && f.Arr.Item.ItemID == it.facts.ItemID {
			return true
		}
	}
	return false
}

// coversPath reports whether the flag covers a source path (a retained record's, for expiry),
// with the source's path to compare *arr item folders.
func (rf *resolvedFlag) coversPath(sourceID int64, rel, sourcePath string) bool {
	if rf.hasPath && sourceID == rf.sourceID && under(rel, rf.rel) {
		return true
	}
	if sourcePath == "" {
		return false
	}
	local := path.Join(sourcePath, rel)
	for _, it := range rf.items {
		if it.folder != "" && under(local, it.folder) {
			return true
		}
	}
	return false
}
