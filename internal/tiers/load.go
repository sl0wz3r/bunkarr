package tiers

import (
	"cmp"
	"context"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/mediaindex"
	"github.com/sl0wz3r/bunkarr/internal/testhooks"
)

// factContext is what fact loading reads once per evaluation and shares across sources: the
// integrations and their freshness, the *arr items with their mapped folders, the root folders,
// and the flags.
type factContext struct {
	// now is the evaluation time; freshNow the time freshness is judged at (the e2e skew hook).
	now, freshNow time.Time
	ints          map[int64]integrations.Integration
	// arrInts are the *arr integrations, by id.
	arrInts []int64
	fresh   map[int64]mediaindex.Freshness
	meta    map[int64]mediaindex.Meta
	// items are the non-deleted *arr items by arr_items id; folders maps an item's mapped folder
	// to its items.
	items   map[int64]*itemInfo
	folders map[string][]*itemInfo
	// roots maps a mapped *arr root folder to the integrations that have it; unmapped lists, by
	// integration, the root folders (as the *arr sees them) that no path mapping covers.
	roots    map[string][]int64
	unmapped map[int64][]string
	// unmappedItems lists, by integration, the folders (as the *arr sees them) of non-deleted items
	// that no path mapping covers outside the unmapped root folders: a custom path outside every
	// root folder, or a root folder removed from the *arr's settings while its items still point
	// there.
	unmappedItems map[int64][]string
	// deletedFolders maps a folder of a deleted *arr integration to its records, and
	// deletedUnmapped are the deleted ones that had an unmapped root or item folder (DeletedArr).
	deletedFolders  map[string][]*DeletedArr
	deletedUnmapped []*DeletedArr
	flags           []*resolvedFlag
	unknown         []UnknownSource
	sources         map[int64]catalog.Source
	locator         *catalog.Locator
}

// itemInfo is an indexed item with its item-level facts.
type itemInfo struct {
	facts ArrItemFacts
	// folder is the item's folder as Bunkarr sees it ("" when unmapped).
	folder string
}

// newContext reads the shared facts through q; the sources and integrations are q's when it is a
// Snapshot (nothing is read through the read pool then), the stores' otherwise.
func (e *Engine) newContext(ctx context.Context, q Queryer, now time.Time) (*factContext, error) {
	fc := &factContext{now: now, freshNow: testhooks.FreshnessNow(now), ints: map[int64]integrations.Integration{},
		fresh: map[int64]mediaindex.Freshness{}, meta: map[int64]mediaindex.Meta{}, items: map[int64]*itemInfo{},
		folders: map[string][]*itemInfo{}, roots: map[string][]int64{}, unmapped: map[int64][]string{}, unmappedItems: map[int64][]string{},
		deletedFolders: map[string][]*DeletedArr{}, sources: map[int64]catalog.Source{}}
	list, err := integrationsOf(ctx, e.o.Integrations, q)
	if err != nil {
		return nil, fmt.Errorf("tiers: %w", err)
	}
	srcs, err := sourcesOf(ctx, e.o.Catalog, q)
	if err != nil {
		return nil, fmt.Errorf("tiers: %w", err)
	}
	for _, s := range srcs {
		fc.sources[s.ID] = s
	}
	fc.locator = catalog.NewLocator(srcs)
	states, err := e.o.Index.States(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("tiers: %w", err)
	}
	mappings := map[int64]integrations.ArrSettings{}
	for _, it := range list {
		fc.ints[it.ID] = it
		if !it.Type.IsArr() {
			continue
		}
		fc.arrInts = append(fc.arrInts, it.ID)
		settings, err := it.ArrSettings()
		if err != nil {
			fc.fresh[it.ID] = mediaindex.Freshness{Reason: fmt.Sprintf("%s's settings cannot be read", it.Name)}
		} else {
			mappings[it.ID] = settings
			fc.fresh[it.ID] = arrFreshness(it, states[it.ID], fc.freshNow)
		}
		if f := fc.fresh[it.ID]; !f.Fresh {
			fc.unknown = append(fc.unknown, UnknownSource{IntegrationID: it.ID, Name: it.Name, Reason: f.Reason})
		}
		m, err := e.o.Index.Meta(ctx, q, it.ID)
		if err != nil {
			return nil, fmt.Errorf("tiers: %w", err)
		}
		fc.meta[it.ID] = m
		for _, rf := range m.RootFolders {
			if rf.LocalPath != nil && catalog.LocatablePath(*rf.LocalPath) {
				fc.roots[*rf.LocalPath] = append(fc.roots[*rf.LocalPath], it.ID)
			} else {
				fc.unmapped[it.ID] = append(fc.unmapped[it.ID], rf.Path)
			}
		}
		// A root folder with no path mapping: Bunkarr cannot tell which files are in it, so no file
		// is unmanaged while the integration counts (S14).
		if u := fc.unmapped[it.ID]; len(u) > 0 && fc.fresh[it.ID].Fresh {
			fc.unknown = append(fc.unknown, UnknownSource{IntegrationID: it.ID, Name: it.Name, Reason: unmappedWhy(it.Name, u)})
		}
	}
	deleted, err := deletedArrQ(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("tiers: %w", err)
	}
	fc.addDeleted(deleted)
	err = e.o.Index.EachItem(ctx, q, 0, false, func(it mediaindex.Item) error {
		in, ok := fc.ints[it.IntegrationID]
		if !ok || !in.Type.IsArr() {
			return nil
		}
		info := &itemInfo{facts: itemFacts(in, fc.meta[it.IntegrationID], it)}
		if s, ok := mappings[it.IntegrationID]; ok {
			if local, ok := s.MapPath(it.Path); ok && catalog.LocatablePath(local) {
				info.folder = local
				info.facts.Folder = local
				fc.folders[local] = append(fc.folders[local], info)
			} else if it.Path != "" && !underArrRoot(it.Path, fc.unmapped[it.IntegrationID]) {
				fc.unmappedItems[it.IntegrationID] = append(fc.unmappedItems[it.IntegrationID], it.Path)
			}
		}
		fc.items[it.ID] = info
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("tiers: %w", err)
	}
	// An item folder with no path mapping: as for a root folder, Bunkarr cannot tell which files
	// are in it, so no file is unmanaged while the integration counts (S14).
	for _, id := range fc.arrInts {
		if u := fc.unmappedItems[id]; len(u) > 0 && fc.fresh[id].Fresh {
			fc.unknown = append(fc.unknown, UnknownSource{IntegrationID: id, Name: fc.appName(id), Reason: unmappedItemsWhy(fc.appName(id), u)})
		}
	}
	flags, err := e.store.Flags(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("tiers: %w", err)
	}
	fc.flags = resolveFlags(fc, flags)
	return fc, nil
}

// arrFreshness is an *arr integration's freshness; a disabled integration is not fresh.
func arrFreshness(it integrations.Integration, st mediaindex.State, now time.Time) mediaindex.Freshness {
	if !it.Enabled {
		return mediaindex.Freshness{Reason: fmt.Sprintf("%s (%s) is disabled", it.Name, it.Type.AppName())}
	}
	stale, ok := mediaindex.StaleAfter(it)
	if !ok {
		return mediaindex.Freshness{Reason: fmt.Sprintf("%s has no metadata cache", it.Type.AppName())}
	}
	if st.IntegrationID == 0 {
		st = mediaindex.State{IntegrationID: it.ID, Status: mediaindex.StatusNever}
	}
	f := mediaindex.FreshnessOf(st, it, stale, now)
	if !f.Fresh && it.Name != "" && !strings.Contains(f.Reason, it.Name) {
		f.Reason = fmt.Sprintf("%s: %s", it.Name, f.Reason)
	}
	return f
}

// itemFacts builds an item's item-level facts, with tag labels and the profile name from the
// integration's metadata.
func itemFacts(in integrations.Integration, m mediaindex.Meta, it mediaindex.Item) ArrItemFacts {
	f := ArrItemFacts{IntegrationID: in.ID, App: in.Type.AppName(), ItemID: it.ID, Kind: it.Kind, ArrID: it.ArrID, Title: it.Title,
		Year: it.Year, ExternalIDs: it.ExternalIDs, RootFolder: it.RootFolder, Monitored: it.Monitored, Genres: it.Genres,
		QualityProfileID: it.QualityProfileID, Tags: make([]string, 0, len(it.Tags))}
	for _, id := range it.Tags {
		label := fmt.Sprintf("#%d", id)
		for _, t := range m.Tags {
			if t.ID == id {
				label = t.Label
				break
			}
		}
		f.Tags = append(f.Tags, label)
	}
	for _, p := range m.QualityProfiles {
		if p.ID == it.QualityProfileID {
			f.QualityProfile = p.Name
			break
		}
	}
	if f.Genres == nil {
		f.Genres = []string{}
	}
	return f
}

// arrIntegrations returns the *arr integrations whose facts count for a source: only the
// source's own (sources.arr_integration_id) when it names one.
func (fc *factContext) arrIntegrations(src catalog.Source) []int64 {
	if src.ArrIntegrationID != nil {
		return []int64{*src.ArrIntegrationID}
	}
	return fc.arrInts
}

func (fc *factContext) counts(src catalog.Source, integrationID int64) bool {
	return src.ArrIntegrationID == nil || *src.ArrIntegrationID == integrationID
}

func (fc *factContext) appName(id int64) string {
	if it, ok := fc.ints[id]; ok {
		return it.Name
	}
	return fmt.Sprintf("integration %d", id)
}

// loadFacts builds the facts of a source's live files (design §8.3). dirs nil loads the whole
// source; otherwise only the files directly in those directories. need says which provider fields
// the caller evaluates (nil: every field): a provider none of whose fields is needed is not loaded.
// Facts are returned by path.
func (e *Engine) loadFacts(ctx context.Context, q Queryer, fc *factContext, src catalog.Source, dirs []string, need fieldSet) ([]*Facts, error) {
	var rows []catalogRow
	collect := func(r catalogRow) error {
		rows = append(rows, r)
		return nil
	}
	if dirs == nil {
		if err := liveRows(ctx, q, src.ID, "", collect); err != nil {
			return nil, err
		}
	} else {
		for _, d := range dirs {
			if err := liveRows(ctx, q, src.ID, d, collect); err != nil {
				return nil, err
			}
		}
		slices.SortFunc(rows, func(a, b catalogRow) int { return cmp.Compare(a.rel, b.rel) })
		rows = slices.CompactFunc(rows, func(a, b catalogRow) bool { return a.id == b.id })
	}
	// The *arr files at the source's paths (every integration's: sources may overlap).
	arrAt := map[string][]mediaindex.File{}
	addArr := func(f mediaindex.File) error {
		arrAt[f.LocalPath] = append(arrAt[f.LocalPath], f)
		return nil
	}
	if dirs == nil {
		if err := e.o.Index.FilesUnder(ctx, q, src.Path, addArr); err != nil {
			return nil, fmt.Errorf("tiers: %w", err)
		}
	} else {
		for _, d := range dirs {
			local := src.Path
			if d != "." {
				local = path.Join(src.Path, d)
			}
			if err := e.o.Index.FilesUnder(ctx, q, local, addArr); err != nil {
				return nil, fmt.Errorf("tiers: %w", err)
			}
		}
	}
	facts := make([]*Facts, 0, len(rows))
	byDir := map[string][]*Facts{}
	for _, r := range rows {
		f := &Facts{FileID: r.id, SourceID: src.ID, RelPath: r.rel, Size: r.size, MtimeNs: r.mtimeNs, Group: r.group,
			LocalPath: path.Join(src.Path, r.rel), Flags: []int64{}}
		if t, err := db.ParseTime(r.firstSeen); err == nil {
			f.FirstSeenAt = t
		}
		facts = append(facts, f)
		byDir[path.Dir(r.rel)] = append(byDir[path.Dir(r.rel)], f)
	}
	// Pass 1: the files the *arr knows by exact path.
	var rest []*Facts
	hasRow := map[*Facts]bool{}
	for _, f := range facts {
		var rowsHere []mediaindex.File
		for _, a := range arrAt[f.LocalPath] {
			if fc.counts(src, a.IntegrationID) {
				rowsHere = append(rowsHere, a)
			}
		}
		if len(rowsHere) == 0 {
			rest = append(rest, f)
			continue
		}
		hasRow[f] = true
		fc.exactArr(f, rowsHere)
	}
	// Pass 2: sidecars follow their media file; other extras are attributed by folder.
	stems := map[string]map[string]*Facts{}
	for _, f := range rest {
		dir := path.Dir(f.RelPath)
		st, ok := stems[dir]
		if !ok {
			st = mediaStems(byDir[dir], hasRow)
			stems[dir] = st
		}
		if m := mediaFileOf(f, st); m != nil {
			f.Follows = m.RelPath
			f.Arr = m.Arr
			continue
		}
		fc.folderArr(f, src)
	}
	for _, f := range facts {
		if src.PlexIntegrationID != nil && src.PlexSectionID != "" {
			f.Plex = &PlexFacts{Known: true, IntegrationID: *src.PlexIntegrationID, Section: fmt.Sprintf("%d:%s", *src.PlexIntegrationID, src.PlexSectionID)}
		}
		for _, rf := range fc.flags {
			if rf.covers(f) {
				f.Flags = append(f.Flags, rf.flag.ID)
			}
		}
	}
	for _, p := range e.o.Providers {
		if !need.anyOf(loadsFor(p)) {
			continue
		}
		var err error
		if fl, ok := p.(fieldsLoader); ok && need != nil {
			err = fl.LoadFields(ctx, q, src, facts, fc.now, need.has)
		} else {
			err = p.Load(ctx, q, src, facts, fc.now)
		}
		if err != nil {
			return nil, fmt.Errorf("tiers: %w", err)
		}
	}
	return facts, nil
}

// fieldSet is a set of condition fields; nil is every field.
type fieldSet map[string]bool

func (s fieldSet) has(field string) bool { return s == nil || s[field] }

func (s fieldSet) anyOf(fields []string) bool {
	if s == nil {
		return true
	}
	return slices.ContainsFunc(fields, s.has)
}

// neededFields returns the fields of the enabled rules that keep passes (never nil).
func neededFields(rules []Rule, keep func(Rule) bool) fieldSet {
	out := fieldSet{}
	for _, r := range rules {
		if !r.Enabled || !keep(r) {
			continue
		}
		for _, c := range r.Conditions {
			out[c.Field] = true
		}
	}
	return out
}

// feeder is a Provider whose facts also feed fields it does not own: Plex's added date is one of
// file.age's sources (design §8.2), so a rule on file.age needs the Plex index loaded.
type feeder interface {
	Feeds() []string
}

// loadsFor returns the fields whose evaluation needs p's facts: its own and those it feeds.
func loadsFor(p Provider) []string {
	if f, ok := p.(feeder); ok {
		return append(slices.Clone(p.Fields()), f.Feeds()...)
	}
	return p.Fields()
}

// fieldsLoader is a Provider that can load the facts of some of its fields only.
type fieldsLoader interface {
	LoadFields(ctx context.Context, q Queryer, src catalog.Source, files []*Facts, now time.Time, need func(field string) bool) error
}

// exactArr attributes a file by the *arr files at its exact path (design §8.3, S18, D12).
func (fc *factContext) exactArr(f *Facts, rows []mediaindex.File) {
	ints := map[int64]bool{}
	for _, r := range rows {
		ints[r.IntegrationID] = true
	}
	if len(ints) > 1 {
		var names []string
		for _, r := range rows {
			if n := fc.appName(r.IntegrationID); !slices.Contains(names, n) {
				names = append(names, n)
			}
		}
		f.Arr = ArrFacts{State: ArrUnknown, Why: "two *arr integrations claim this file (" + strings.Join(names, ", ") + ")"}
		return
	}
	r := rows[0]
	f.Arr = ArrFacts{IntegrationID: r.IntegrationID, FileID: r.ArrFileID}
	if r.Size != f.Size {
		f.Arr.State = ArrUnknown
		f.Arr.Why = fmt.Sprintf("%s's file at this path has another size (%d bytes; the catalog has %d)", fc.appName(r.IntegrationID), r.Size, f.Size)
		return
	}
	info := fc.items[r.ItemID]
	if info == nil {
		f.Arr.State, f.Arr.Why = ArrUnknown, fmt.Sprintf("%s no longer has the item of this file", fc.appName(r.IntegrationID))
		return
	}
	if fr := fc.fresh[r.IntegrationID]; !fr.Fresh {
		f.Arr.State, f.Arr.Why = ArrUnknown, fr.Reason
		return
	}
	item := info.facts
	f.Arr.State, f.Arr.Item, f.Arr.DateAdded, f.Arr.Quality = ArrItem, &item, r.DateAdded, r.Quality
}

// mediaStems indexes the files of one directory that the *arr knows by their name without its
// extension (the first such file wins a shared stem).
func mediaStems(siblings []*Facts, hasRow map[*Facts]bool) map[string]*Facts {
	out := map[string]*Facts{}
	for _, m := range siblings {
		if !hasRow[m] {
			continue
		}
		stem := path.Base(m.RelPath)
		if i := strings.LastIndexByte(stem, '.'); i > 0 {
			stem = stem[:i]
		}
		if _, ok := out[stem]; !ok {
			out[stem] = m
		}
	}
	return out
}

// mediaFileOf returns the media file a sidecar follows: the file in the same directory that the
// *arr knows and whose name without its extension is the longest prefix of the sidecar's name
// ("X.en.srt" and "X.nfo" follow "X.mkv"). stems is the directory's mediaStems; each dot-prefix of
// the name is looked up, longest first, so a directory of n files costs O(n), not O(n²).
func mediaFileOf(f *Facts, stems map[string]*Facts) *Facts {
	if len(stems) == 0 {
		return nil
	}
	name := path.Base(f.RelPath)
	for i := strings.LastIndexByte(name, '.'); i > 0; i = strings.LastIndexByte(name[:i], '.') {
		if m, ok := stems[name[:i]]; ok && m != f {
			return m
		}
	}
	return nil
}

// folderArr attributes a file without an *arr file of its own: to the item whose mapped folder
// is its longest containing folder (item-level facts only), else unknown inside an *arr root
// folder or a folder of a deleted *arr integration (DeletedArr), else unmanaged when every
// counted *arr cache is fresh and has every root folder mapped. Otherwise it is unknown: a
// deleted integration or an unmapped root folder can only make a file more protected (S14).
func (fc *factContext) folderArr(f *Facts, src catalog.Source) {
	for d := path.Dir(f.LocalPath); ; d = path.Dir(d) {
		var here []*itemInfo
		for _, it := range fc.folders[d] {
			if fc.counts(src, it.facts.IntegrationID) {
				here = append(here, it)
			}
		}
		if len(here) > 0 {
			ints := map[int64]bool{}
			for _, it := range here {
				ints[it.facts.IntegrationID] = true
			}
			if len(ints) > 1 {
				f.Arr = ArrFacts{State: ArrUnknown, Why: "two *arr integrations claim this file's folder"}
				return
			}
			it := here[0]
			f.Arr = ArrFacts{IntegrationID: it.facts.IntegrationID, ByFolder: true}
			if fr := fc.fresh[it.facts.IntegrationID]; !fr.Fresh {
				f.Arr.State, f.Arr.Why = ArrUnknown, fr.Reason
				return
			}
			item := it.facts
			f.Arr.State, f.Arr.Item = ArrItem, &item
			return
		}
		if d == "/" || d == "." {
			break
		}
	}
	// A deleted integration counted for the source only when the source names none now (a source
	// that named it had arr_integration_id set to NULL by the delete).
	deletedCounts := src.ArrIntegrationID == nil
	for d := path.Dir(f.LocalPath); ; d = path.Dir(d) {
		for _, id := range fc.roots[d] {
			if fc.counts(src, id) {
				f.Arr = ArrFacts{State: ArrUnknown, IntegrationID: id,
					Why: fmt.Sprintf("no %s item holds this file (it may be an import that is not indexed yet)", fc.appName(id))}
				return
			}
		}
		if list := fc.deletedFolders[d]; deletedCounts && len(list) > 0 {
			f.Arr = ArrFacts{State: ArrUnknown, Why: deletedWhy(list[0])}
			return
		}
		if d == "/" || d == "." {
			break
		}
	}
	for _, id := range fc.arrIntegrations(src) {
		fr, ok := fc.fresh[id]
		if !ok {
			fr = mediaindex.Freshness{Reason: fmt.Sprintf("integration %d does not exist", id)}
		}
		if !fr.Fresh {
			f.Arr = ArrFacts{State: ArrUnknown, IntegrationID: id, Why: fr.Reason}
			return
		}
	}
	for _, id := range fc.arrIntegrations(src) {
		if u := fc.unmapped[id]; len(u) > 0 {
			f.Arr = ArrFacts{State: ArrUnknown, IntegrationID: id, Why: unmappedWhy(fc.appName(id), u)}
			return
		}
		if u := fc.unmappedItems[id]; len(u) > 0 {
			f.Arr = ArrFacts{State: ArrUnknown, IntegrationID: id, Why: unmappedItemsWhy(fc.appName(id), u)}
			return
		}
	}
	if deletedCounts && len(fc.deletedUnmapped) > 0 {
		f.Arr = ArrFacts{State: ArrUnknown, Why: deletedWhy(fc.deletedUnmapped[0])}
		return
	}
	f.Arr = ArrFacts{State: ArrUnmanaged}
}

// unmappedWhy says why a file outside every mapped folder is unknown: an *arr root folder has no
// path mapping.
func unmappedWhy(name string, roots []string) string {
	return fmt.Sprintf("%s's root folder %s has no path mapping, so Bunkarr cannot tell whether this file is in it (add a path mapping for it)",
		name, strings.Join(roots, ", "))
}

// unmappedItemsWhy says why a file outside every mapped folder is unknown: an *arr item's folder
// (outside the root folders the *arr lists) has no path mapping. It names at most three folders.
func unmappedItemsWhy(name string, folders []string) string {
	shown := folders
	more := ""
	if len(shown) > 3 {
		shown, more = shown[:3], fmt.Sprintf(" and %d more", len(folders)-3)
	}
	return fmt.Sprintf("%s has items in folders no path mapping covers (%s%s), so Bunkarr cannot tell whether this file is in one of them (add a path mapping for them)",
		name, strings.Join(shown, ", "), more)
}

// underArrRoot reports whether the *arr path p is one of roots or under one (both as the *arr
// sees them, cleaned first: a root folder often ends in "/").
func underArrRoot(p string, roots []string) bool {
	clean := make([]string, len(roots))
	for i, r := range roots {
		clean[i] = path.Clean(r)
	}
	return underAny(path.Clean(p), clean)
}

// deletedWhy says why a file of a deleted *arr integration's folder is unknown.
func deletedWhy(d *DeletedArr) string {
	return fmt.Sprintf("%s (%s), which managed this folder, was deleted; the file stays unknown until you confirm its removal (Settings → Tiers, Deleted *arr integrations)", d.Name, d.App)
}
