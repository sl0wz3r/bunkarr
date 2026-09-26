package mediaindex

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/integrations/plex"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// The Plex library index (design §6.3): plex_sections, plex_items and plex_files of one Plex
// integration, for the Tautulli and Maintainerr joins (which are scoped to one Plex server) and the
// Plex-based tier facts.

// PlexIndexStats is index_state.stats of a Plex library index.
type PlexIndexStats struct {
	MachineIdentifier string           `json:"machineIdentifier"`
	Sections          int64            `json:"sections"`
	Items             int64            `json:"items"`
	ItemsByType       map[string]int64 `json:"itemsByType"`
	Files             int64            `json:"files"`
	FilesMapped       int64            `json:"filesMapped"`
	FilesUnmapped     int64            `json:"filesUnmapped"`
	// SkippedRows counts listing rows of a type the index does not keep (clips, photos).
	SkippedRows int64 `json:"skippedRows"`
}

// SectionLocation is one folder of a Plex section as Plex sees it, mapped and located (nil when
// unmapped or in no source).
type SectionLocation struct {
	Path      string  `json:"path"`
	LocalPath *string `json:"localPath"`
	SourceID  *int64  `json:"sourceId"`
	RelPath   *string `json:"relPath"`
}

// plexItemTypes are the item types plex_items keeps (its CHECK constraint).
var plexItemTypes = map[string]bool{"movie": true, "show": true, "season": true, "episode": true, "artist": true, "album": true, "track": true}

type plexSectionRow struct {
	key, title, typ string
	locations       []SectionLocation
}

type plexItemRow struct {
	it      plex.Item
	section string
	// ids is the external_ids JSON.
	ids string
}

type plexFileRow struct {
	ratingKey, file string
	local           string
	sourceID        int64
	rel             string
	located         bool
}

// externalIDs reads the imdb, tmdb and tvdb ids of Guid[] ("tmdb://10331").
func externalIDs(guids []string) map[string]string {
	out := map[string]string{}
	for _, g := range guids {
		scheme, id, ok := strings.Cut(g, "://")
		if !ok || id == "" {
			continue
		}
		switch scheme {
		case "imdb", "tmdb", "tvdb":
			if _, dup := out[scheme]; !dup {
				out[scheme] = id
			}
		}
	}
	return out
}

func (pr *providerRun) plexIndex(ctx context.Context) error {
	it := pr.it
	ps, err := it.PlexSettings()
	if err != nil {
		return err
	}
	token, err := pr.r.o.Integrations.TokenFor(ctx, it.ID, it.URL)
	if err != nil {
		return fmt.Errorf("Plex %q: %w", it.Name, err)
	}
	c, err := plex.New(it.URL, token, pr.r.o.Plex)
	if err != nil {
		return fmt.Errorf("Plex %q: %w", it.Name, err)
	}
	pr.stats.Requests++
	id, err := c.Identity(ctx)
	if err != nil {
		return err
	}
	pr.stats.AppVersion = id.Version
	instance := PlexInstanceID(it.URL, id.MachineIdentifier)
	pr.stats.Requests++
	sections, err := c.Sections(ctx)
	if err != nil {
		return err
	}
	loc, err := pr.r.o.Catalog.Locator(ctx)
	if err != nil {
		return err
	}
	mapLocate := func(p string) (string, int64, string, bool) {
		local, ok := ps.MapPath(p)
		if !ok {
			return "", 0, "", false
		}
		locs := loc.Locate(local)
		if len(locs) == 0 {
			return local, 0, "", false
		}
		return local, locs[0].SourceID, locs[0].Rel, true
	}
	st := PlexIndexStats{MachineIdentifier: id.MachineIdentifier, ItemsByType: map[string]int64{}}
	var (
		secRows  []plexSectionRow
		itemRows []plexItemRow
		fileRows []plexFileRow
	)
	seenItems := map[string]bool{}
	seenFiles := map[string]bool{}
	for _, s := range sections {
		row := plexSectionRow{key: s.Key, title: s.Title, typ: s.Type, locations: []SectionLocation{}}
		for _, l := range s.Locations {
			sl := SectionLocation{Path: l.Path}
			if local, src, rel, ok := mapLocate(l.Path); local != "" {
				sl.LocalPath = &local
				if ok {
					sl.SourceID, sl.RelPath = &src, &rel
				}
			}
			row.locations = append(row.locations, sl)
		}
		secRows = append(secRows, row)
		for _, typ := range plex.TypesOf(s.Type) {
			pr.stats.Requests++
			err := c.AllItems(ctx, s.Key, typ, plex.ListOptions{}, func(item plex.Item) error {
				if !plexItemTypes[item.Type] {
					st.SkippedRows++
					return nil
				}
				if seenItems[item.RatingKey] {
					return nil
				}
				seenItems[item.RatingKey] = true
				ids, err := json.Marshal(externalIDs(item.GUIDs))
				if err != nil {
					return err
				}
				itemRows = append(itemRows, plexItemRow{it: item, section: s.Key, ids: string(ids)})
				st.ItemsByType[item.Type]++
				for _, f := range item.Files {
					k := f + "\x00" + item.RatingKey
					if seenFiles[k] {
						continue
					}
					seenFiles[k] = true
					fr := plexFileRow{ratingKey: item.RatingKey, file: f}
					fr.local, fr.sourceID, fr.rel, fr.located = mapLocate(f)
					if fr.located {
						st.FilesMapped++
					} else {
						st.FilesUnmapped++
					}
					fileRows = append(fileRows, fr)
				}
				if n := int64(len(itemRows)); n%1000 == 0 {
					pr.env.Reporter.Progress(jobs.Progress{Phase: "indexing", FilesDone: n})
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
	}
	st.Sections, st.Items, st.Files = int64(len(secRows)), int64(len(itemRows)), int64(len(fileRows))
	pr.stats.Items, pr.stats.Files, pr.stats.FilesMapped, pr.stats.FilesUnmapped = st.Items, st.Files, st.FilesMapped, st.FilesUnmapped
	pr.stats.Cache = st
	if pr.stats.ItemsBefore, err = pr.rowCount(ctx, "plex_items"); err != nil {
		return err
	}
	pr.summary = fmt.Sprintf("Plex library index of %s: %d sections, %d items, %d files (%d in no source)", it.Name, st.Sections, st.Items, st.Files, st.FilesUnmapped)
	if st.FilesUnmapped > 0 {
		pr.info("Some Plex files map to no source: check the Plex path mappings", "files", st.FilesUnmapped)
	}
	if pr.dry {
		return nil
	}
	if changed, err := pr.instanceChanged(ctx, instance); err != nil {
		return err
	} else if changed {
		pr.info("The index described another Plex server or URL; it is replaced by this refresh")
	}
	tables, err := plexIndexTables(secRows, itemRows, fileRows)
	if err != nil {
		return err
	}
	return pr.replace(ctx, tables, instance, st)
}

// plexIndexTables returns the new rows of plex_files, plex_items and plex_sections.
func plexIndexTables(secs []plexSectionRow, items []plexItemRow, files []plexFileRow) ([]*cacheTable, error) {
	locs := make([]string, len(secs))
	for i, s := range secs {
		b, err := json.Marshal(s.locations)
		if err != nil {
			return nil, err
		}
		locs[i] = string(b)
	}
	sections := &cacheTable{name: "plex_sections", keys: []string{"section_key"}, vals: []string{"title", "type", "locations"}, n: len(secs),
		row: func(dst []any, i int) []any {
			s := secs[i]
			return append(dst, s.key, s.title, s.typ, locs[i])
		}}
	itemTable := &cacheTable{name: "plex_items", keys: []string{"rating_key"},
		vals: []string{"type", "section_key", "parent_key", "grandparent_key", "item_index", "parent_index", "guid", "external_ids", "title", "added_at"},
		n:    len(items),
		row: func(dst []any, i int) []any {
			r := &items[i]
			var added any
			if r.it.AddedAt != nil {
				added = db.FormatTime(*r.it.AddedAt)
			}
			return append(dst, r.it.RatingKey, r.it.Type, r.section, nullText(r.it.ParentRatingKey), nullText(r.it.GrandparentRatingKey),
				nullIntPtr(r.it.Index), nullIntPtr(r.it.ParentIndex), r.it.GUID, r.ids, r.it.Title, added)
		}}
	fileTable := &cacheTable{name: "plex_files", keys: []string{"file", "rating_key"}, vals: []string{"local_path", "source_id", "rel_path"}, n: len(files),
		row: func(dst []any, i int) []any {
			f := &files[i]
			var src, rel any
			if f.located {
				src, rel = f.sourceID, f.rel
			}
			return append(dst, f.file, f.ratingKey, nullText(f.local), src, rel)
		}}
	return []*cacheTable{fileTable, itemTable, sections}, nil
}
