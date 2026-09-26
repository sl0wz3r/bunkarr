package tiers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/mediaindex"
	"github.com/sl0wz3r/bunkarr/internal/testhooks"
)

// IndexProvider supplies the facts of the Plex library index, Tautulli, Seerr and Maintainerr
// (design §8.2, §8.3; Phase 3 slice 9) from the caches of internal/mediaindex. Registered in
// Options.Providers, it makes those fields available.
//
// Every join is scoped to one Plex server (DS6): a Tautulli or Maintainerr row's rating key is
// looked up only in the plex_files and plex_items of that integration's plexIntegrationId, and a
// Seerr rating key only in its own linked server's. A cache counts only while it is fresh (D15),
// while the linked Plex index is fresh, and while the integration is still linked to the Plex
// server its cache was built for; otherwise its facts are unknown (S14). Several integrations of
// one type combine: a condition is true when one of them says so, false when all do, and unknown
// otherwise; play counts add up.
type IndexProvider struct {
	index *mediaindex.Store
	ints  *integrations.Store
	// seerrUsers fetches a Seerr integration's users live (the editor's labels); nil: none.
	seerrUsers func(ctx context.Context, it integrations.Integration) ([]Suggestion, error)

	mu    sync.Mutex
	users map[int64]userCache
}

// userCache is a Seerr integration's live user list: the last successful fetch (at, list) and
// the last failure (failedAt, err), which is not retried for usersRetry.
type userCache struct {
	at       time.Time
	list     []Suggestion
	failedAt time.Time
	err      error
}

// NewIndexProvider returns the provider. seerrUsers (optional) reads a Seerr integration's users
// live, as {Value: id, Label: label} suggestions (the API wires the Seerr client).
func NewIndexProvider(index *mediaindex.Store, ints *integrations.Store, seerrUsers func(ctx context.Context, it integrations.Integration) ([]Suggestion, error)) *IndexProvider {
	return &IndexProvider{index: index, ints: ints, seerrUsers: seerrUsers, users: map[int64]userCache{}}
}

// Fields implements Provider.
func (p *IndexProvider) Fields() []string {
	return []string{FieldPlexSection, FieldSeerrRequested, FieldSeerrRequestedBy, FieldTautulliPlayCount, FieldTautulliLastWatch, FieldMaintainerrPending}
}

// Feeds implements feeder: Plex's added date is file.age's second source (design §8.2).
func (p *IndexProvider) Feeds() []string { return []string{FieldFileAge} }

// cacheState is one integration's cache as a Load sees it.
type cacheState struct {
	it    integrations.Integration
	fresh mediaindex.Freshness
	// link is the plexIntegrationId the cache was built for (Tautulli, Seerr, Maintainerr).
	link int64
	// instance is the cache's instance id (index_state.instance_id; for a Plex index
	// "<url>#<machineIdentifier>"); plexInstance, for Tautulli, Seerr and Maintainerr, is the
	// linked Plex index's instance id at the cache's refresh (stats.plexInstanceId).
	instance     string
	plexInstance string
	stats        map[string]any
}

// loadCtx is what one Load reads once and shares across the files of a source.
type loadCtx struct {
	p      *IndexProvider
	ctx    context.Context
	q      Queryer
	src    catalog.Source
	caches map[integrations.Type][]*cacheState
	byID   map[int64]*cacheState
	plex   map[int64]*plexData
	// arrSeasons are the seasons of the Sonarr files at each local path under the source.
	arrSeasons map[string][]int
	// memos holds the per-Load reads of one integration's cache rows.
	memos map[string]any
}

// plexData is one Plex integration's index as far as a Load needs it: the items of the source's
// files and their ancestors, not every item of the server (a Load runs once per source).
type plexData struct {
	st *cacheState
	// keysAt maps a file's local path to the rating keys of its items.
	keysAt map[string][]string
	// items are the items of the source's files, with their parents and grandparents.
	items map[string]mediaindex.PlexItem
	// byGUID maps a guid of the source's items to the rating keys of every item of the server
	// that has it; read on first use (guidKeys).
	byGUID map[string][]string
	// known memoizes rating-key lookups outside items (plexKnown).
	known    map[string]bool
	sections []mediaindex.PlexSection
}

func (p *IndexProvider) newLoad(ctx context.Context, q Queryer, now time.Time) (*loadCtx, error) {
	lc := &loadCtx{p: p, ctx: ctx, q: q, caches: map[integrations.Type][]*cacheState{}, byID: map[int64]*cacheState{}, plex: map[int64]*plexData{},
		memos: map[string]any{}}
	list, err := integrationsOf(ctx, p.ints, q)
	if err != nil {
		return nil, fmt.Errorf("tiers: %w", err)
	}
	states, err := p.index.States(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("tiers: %w", err)
	}
	freshNow := testhooks.FreshnessNow(now)
	for _, it := range list {
		switch it.Type {
		case integrations.TypePlex, integrations.TypeTautulli, integrations.TypeSeerr, integrations.TypeMaintainerr:
		default:
			continue
		}
		cs := &cacheState{it: it, stats: map[string]any{}}
		st, ok := states[it.ID]
		if !ok {
			st = mediaindex.State{IntegrationID: it.ID, Status: mediaindex.StatusNever}
		}
		stale, has := mediaindex.StaleAfter(it)
		switch {
		case !it.Enabled:
			cs.fresh = mediaindex.Freshness{Reason: fmt.Sprintf("%s (%s) is disabled", it.Name, it.Type.AppName())}
		case !has:
			if it.Type != integrations.TypePlex {
				cs.fresh = mediaindex.Freshness{Reason: fmt.Sprintf("%s's settings cannot be read", it.Name)}
			} else {
				cs.fresh = mediaindex.Freshness{Reason: fmt.Sprintf("the Plex library index of %s is turned off", it.Name)}
			}
		default:
			cs.fresh = mediaindex.FreshnessOf(st, it, stale, freshNow)
			if !cs.fresh.Fresh && !strings.Contains(cs.fresh.Reason, it.Name) {
				cs.fresh.Reason = it.Name + ": " + cs.fresh.Reason
			}
		}
		cs.instance = st.InstanceID
		if it.Type != integrations.TypePlex {
			var v struct {
				PlexIntegrationID int64  `json:"plexIntegrationId"`
				PlexInstanceID    string `json:"plexInstanceId"`
			}
			_ = jsonUnmarshal(st.Stats, &v)
			cs.link, cs.plexInstance = v.PlexIntegrationID, v.PlexInstanceID
			_ = jsonUnmarshal(st.Stats, &cs.stats)
		}
		lc.caches[it.Type] = append(lc.caches[it.Type], cs)
		lc.byID[it.ID] = cs
	}
	return lc, nil
}

// linkOf returns the plexIntegrationId setting of a Tautulli, Seerr or Maintainerr integration.
func linkOf(it integrations.Integration) int64 {
	switch it.Type {
	case integrations.TypeTautulli:
		if s, err := it.TautulliSettings(); err == nil {
			return s.PlexIntegrationID
		}
	case integrations.TypeSeerr:
		if s, err := it.SeerrSettings(); err == nil {
			return s.PlexIntegrationID
		}
	case integrations.TypeMaintainerr:
		if s, err := it.MaintainerrSettings(); err == nil {
			return s.PlexIntegrationID
		}
	}
	return 0
}

// plexOf loads (once) the index of Plex integration id for the source's files; nil when there is
// no such Plex integration.
func (lc *loadCtx) plexOf(id int64) (*plexData, error) {
	if d, ok := lc.plex[id]; ok {
		return d, nil
	}
	st := lc.byID[id]
	if st == nil || st.it.Type != integrations.TypePlex {
		lc.plex[id] = nil
		return nil, nil
	}
	d := &plexData{st: st, keysAt: map[string][]string{}, items: map[string]mediaindex.PlexItem{}, known: map[string]bool{}}
	lc.plex[id] = d
	if !st.fresh.Fresh {
		return d, nil
	}
	err := lc.p.index.PlexFilesUnder(lc.ctx, lc.q, lc.src.Path, func(f mediaindex.PlexFile) error {
		if f.IntegrationID == id && !slices.Contains(d.keysAt[f.LocalPath], f.RatingKey) {
			d.keysAt[f.LocalPath] = append(d.keysAt[f.LocalPath], f.RatingKey)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("tiers: %w", err)
	}
	if err := lc.p.index.PlexItemsUnder(lc.ctx, lc.q, id, lc.src.Path, func(it mediaindex.PlexItem) error {
		d.items[it.RatingKey] = it
		return nil
	}); err != nil {
		return nil, fmt.Errorf("tiers: %w", err)
	}
	if d.sections, err = lc.p.index.PlexSections(lc.ctx, lc.q, id); err != nil {
		return nil, fmt.Errorf("tiers: %w", err)
	}
	return d, nil
}

// guidKeys returns the rating keys of every item of the server with a guid of one of the source's
// items (the guids of the whole source are read at the first call).
func (lc *loadCtx) guidKeys(d *plexData, guid string) ([]string, error) {
	if d.byGUID == nil {
		byGUID := map[string][]string{}
		if err := lc.p.index.PlexGUIDKeysUnder(lc.ctx, lc.q, d.st.it.ID, lc.src.Path, func(k mediaindex.PlexGUIDKey) error {
			byGUID[k.GUID] = append(byGUID[k.GUID], k.RatingKey)
			return nil
		}); err != nil {
			return nil, fmt.Errorf("tiers: %w", err)
		}
		d.byGUID = byGUID
	}
	return d.byGUID[guid], nil
}

// plexKnown reports whether the server's index has an item with this rating key.
func (lc *loadCtx) plexKnown(d *plexData, key string) (bool, error) {
	if _, ok := d.items[key]; ok {
		return true, nil
	}
	if v, ok := d.known[key]; ok {
		return v, nil
	}
	v, err := lc.p.index.PlexItemKnown(lc.ctx, lc.q, d.st.it.ID, key)
	if err != nil {
		return false, fmt.Errorf("tiers: %w", err)
	}
	d.known[key] = v
	return v, nil
}

// fileItems returns the Plex items of a file in one Plex index.
func (d *plexData) fileItems(f *Facts) []mediaindex.PlexItem {
	var out []mediaindex.PlexItem
	for _, k := range d.keysAt[f.LocalPath] {
		if it, ok := d.items[k]; ok {
			out = append(out, it)
		}
	}
	return out
}

// sectionsOf returns the sections that hold a file in one Plex index: its items' sections (several
// when its items are in several libraries of the server, D12), else the section location that
// contains it (longest).
func (d *plexData) sectionsOf(f *Facts, items []mediaindex.PlexItem) []string {
	var out []string
	for _, it := range items {
		if it.SectionKey != "" && !slices.Contains(out, it.SectionKey) {
			out = append(out, it.SectionKey)
		}
	}
	if len(out) > 0 {
		return out
	}
	best, bestLen := "", -1
	for _, s := range d.sections {
		for _, l := range s.Locations {
			if l.SourceID == nil || *l.SourceID != f.SourceID || l.RelPath == nil {
				continue
			}
			rel := *l.RelPath
			if (rel == "" || f.RelPath == rel || strings.HasPrefix(f.RelPath, rel+"/")) && len(rel) > bestLen {
				best, bestLen = s.Key, len(rel)
			}
		}
	}
	if best == "" {
		return nil
	}
	return []string{best}
}

// Load implements Provider: it fills the Plex, Watch, Requests and Maintainerr facts.
func (p *IndexProvider) Load(ctx context.Context, q Queryer, src catalog.Source, files []*Facts, now time.Time) error {
	return p.LoadFields(ctx, q, src, files, now, func(string) bool { return true })
}

// LoadFields is Load for the facts of the fields need reports only (the engine skips what no rule
// evaluates: the Seerr and Maintainerr joins and the Plex index reads cost time per file).
func (p *IndexProvider) LoadFields(ctx context.Context, q Queryer, src catalog.Source, files []*Facts, now time.Time, need func(string) bool) error {
	// file.age reads Plex's added date when the *arr has no dateAdded for the file.
	wantPlex := need(FieldPlexSection) || need(FieldFileAge)
	wantWatch := need(FieldTautulliPlayCount) || need(FieldTautulliLastWatch)
	wantRequests := need(FieldSeerrRequested) || need(FieldSeerrRequestedBy)
	wantMaint := need(FieldMaintainerrPending)
	if !wantPlex && !wantWatch && !wantRequests && !wantMaint {
		return nil
	}
	lc, err := p.newLoad(ctx, q, now)
	if err != nil {
		return err
	}
	lc.src = src
	if wantRequests && len(lc.caches[integrations.TypeSeerr]) > 0 {
		lc.arrSeasons = map[string][]int{}
		err := p.index.FilesUnder(ctx, q, src.Path, func(f mediaindex.File) error {
			for _, ep := range f.Detail.Episodes {
				if !slices.Contains(lc.arrSeasons[f.LocalPath], ep.SeasonNumber) {
					lc.arrSeasons[f.LocalPath] = append(lc.arrSeasons[f.LocalPath], ep.SeasonNumber)
				}
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("tiers: %w", err)
		}
	}
	for _, f := range files {
		if wantPlex {
			if err := lc.plexFacts(f); err != nil {
				return err
			}
		}
		if wantWatch {
			if f.Watch, err = lc.watchFacts(f, now); err != nil {
				return err
			}
		}
		if wantRequests {
			if f.Requests, err = lc.requestFacts(f); err != nil {
				return err
			}
		}
		if wantMaint {
			if f.Maintainerr, err = lc.maintainerrFacts(f); err != nil {
				return err
			}
		}
	}
	return nil
}

// plexFacts fills plex.section's fallback (the source names no Plex library) and Plex's added date.
func (lc *loadCtx) plexFacts(f *Facts) error {
	var ids []int64
	if lc.src.PlexIntegrationID != nil {
		ids = []int64{*lc.src.PlexIntegrationID}
	} else {
		for _, cs := range lc.caches[integrations.TypePlex] {
			if _, has := mediaindex.StaleAfter(cs.it); has {
				ids = append(ids, cs.it.ID)
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	type found struct {
		id    int64
		secs  []string
		added *time.Time
	}
	var (
		hits  []found
		stale []string
	)
	for _, id := range ids {
		d, err := lc.plexOf(id)
		if err != nil {
			return err
		}
		if d == nil {
			continue
		}
		if !d.st.fresh.Fresh {
			stale = append(stale, d.st.fresh.Reason)
			continue
		}
		items := d.fileItems(f)
		if secs := d.sectionsOf(f, items); len(secs) > 0 {
			h := found{id: id, secs: secs}
			for _, it := range items {
				if it.AddedAt != nil && (h.added == nil || it.AddedAt.Before(*h.added)) {
					h.added = it.AddedAt
				}
			}
			hits = append(hits, h)
		}
	}
	if f.Plex != nil && f.Plex.Known {
		// The source names its Plex library; the index only adds the date.
		for _, h := range hits {
			if h.id == f.Plex.IntegrationID {
				f.Plex.AddedAt = h.added
			}
		}
		return nil
	}
	switch {
	case len(hits) == 1 && len(hits[0].secs) > 1:
		// Items of the file in several libraries of the server: mixed evidence is unknown (D12).
		f.Plex = &PlexFacts{Why: fmt.Sprintf("the file is in several Plex libraries (%s)", strings.Join(hits[0].secs, ", "))}
	case len(hits) == 1 && len(stale) == 0:
		f.Plex = &PlexFacts{Known: true, IntegrationID: hits[0].id, Section: fmt.Sprintf("%d:%s", hits[0].id, hits[0].secs[0]), AddedAt: hits[0].added}
	case len(hits) > 1:
		f.Plex = &PlexFacts{Why: "the file is in the libraries of several Plex servers; set the source's Plex library"}
	case len(stale) > 0:
		f.Plex = &PlexFacts{Why: stale[0]}
	default:
		f.Plex = &PlexFacts{Why: "no Plex library holds this file"}
	}
	return nil
}

// linkedPlex checks a Tautulli, Seerr or Maintainerr cache and its linked Plex index; it returns
// the Plex data, or why the facts are unknown.
func (lc *loadCtx) linkedPlex(cs *cacheState, required bool) (*plexData, string, error) {
	if !cs.fresh.Fresh {
		return nil, cs.fresh.Reason, nil
	}
	link := linkOf(cs.it)
	if link != cs.link {
		return nil, fmt.Sprintf("%s was linked to another Plex server since its last refresh", cs.it.Name), nil
	}
	if link == 0 {
		if required {
			return nil, fmt.Sprintf("%s is not linked to a Plex server", cs.it.Name), nil
		}
		return nil, "", nil
	}
	d, err := lc.plexOf(link)
	if err != nil {
		return nil, "", err
	}
	if d == nil {
		return nil, fmt.Sprintf("the Plex server %s is linked to no longer exists", cs.it.Name), nil
	}
	if !d.st.fresh.Fresh {
		return nil, d.st.fresh.Reason, nil
	}
	if why := mismatchOf(cs); why != "" {
		return nil, why, nil
	}
	if !samePlexServer(cs.plexInstance, d.st.instance) {
		return nil, rebuiltWhy(cs, d.st), nil
	}
	return d, "", nil
}

// mismatchOf is why a Maintainerr cache's rows are not the linked Plex server's, as its refresh
// found (stats.plexMismatch), or "".
func mismatchOf(cs *cacheState) string {
	why, _ := cs.stats["plexMismatch"].(string)
	return why
}

// samePlexServer reports whether two Plex index instance ids ("<url>#<machineIdentifier>") name
// the same server: rating keys are the server's own, whatever URL reaches it. An empty or
// malformed id matches nothing.
func samePlexServer(a, b string) bool {
	ma, mb := plexMachine(a), plexMachine(b)
	return ma != "" && ma == mb
}

func plexMachine(instance string) string {
	i := strings.LastIndexByte(instance, '#')
	if i < 0 {
		return ""
	}
	return instance[i+1:]
}

// rebuiltWhy: the linked Plex index was built from another Plex server than the cache's rows.
func rebuiltWhy(cs, plexCS *cacheState) string {
	return fmt.Sprintf("the Plex library index of %s was built from another Plex server since %s's last refresh (refresh %s)",
		plexCS.it.Name, cs.it.Name, cs.it.Name)
}

// watchFacts builds the Tautulli facts of a file.
func (lc *loadCtx) watchFacts(f *Facts, now time.Time) (*WatchFacts, error) {
	list := lc.caches[integrations.TypeTautulli]
	if len(list) == 0 {
		return &WatchFacts{Why: "no Tautulli integration is set up"}, nil
	}
	out := &WatchFacts{Known: true}
	applicable := false
	for _, cs := range list {
		d, why, err := lc.linkedPlex(cs, true)
		if err != nil {
			return nil, err
		}
		if why != "" {
			return &WatchFacts{Why: why, IntegrationID: cs.it.ID}, nil
		}
		items := d.fileItems(f)
		if len(items) == 0 {
			continue
		}
		stats, err := lc.watchStats(cs.it.ID)
		if err != nil {
			return nil, err
		}
		var (
			plays int64 = -1
			last  *time.Time
		)
		for i, it := range items {
			w, ok := stats[mediaindex.KeyRatingKey+"\x00"+it.RatingKey]
			if !ok && it.GUID != "" {
				var why string
				if w, why, err = guidPlays(stats, it, func() ([]string, error) { return lc.guidKeys(d, it.GUID) }); err != nil {
					return nil, err
				}
				if why != "" {
					return &WatchFacts{Why: why, IntegrationID: cs.it.ID}, nil
				}
			} else if !ok {
				w = mediaindex.WatchStat{}
			}
			if i > 0 && (w.Plays != plays || !sameTime(w.LastWatched, last)) {
				return &WatchFacts{Why: "the file holds several Plex items with different play histories", IntegrationID: cs.it.ID}, nil
			}
			plays, last = w.Plays, w.LastWatched
		}
		applicable = true
		out.IntegrationID = cs.it.ID
		out.Plays += plays
		if last != nil && (out.LastWatched == nil || last.After(*out.LastWatched)) {
			out.LastWatched = last
		}
		if lowerBound(cs, d.sectionsOf(f, items)) {
			out.LowerBound = true
		}
	}
	if !applicable {
		return &WatchFacts{Why: "no Plex item of a Tautulli-watched server holds this file"}, nil
	}
	return out, nil
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// guidPlays is the play history of a Plex item that has no row of its own rating key, by its guid
// (a re-added item gets a new rating key; the guid stays). The guid row counts every play of every
// item with that guid, so when other live items share the guid (the same movie in an HD and a 4K
// library) only the plays of rating keys that are no longer in the index are the item's: none
// (zero plays), or, when there are some and several items share the guid, which item they belong
// to is not known (why is set). liveKeys returns the rating keys of the live items with the guid;
// it is called only when the guid has a row.
func guidPlays(stats map[string]mediaindex.WatchStat, it mediaindex.PlexItem, liveKeys func() ([]string, error)) (mediaindex.WatchStat, string, error) {
	g, ok := stats[mediaindex.KeyGUID+"\x00"+it.GUID]
	if !ok {
		return mediaindex.WatchStat{}, "", nil
	}
	live, err := liveKeys()
	if err != nil {
		return mediaindex.WatchStat{}, "", err
	}
	if len(live) <= 1 {
		return g, "", nil
	}
	orphan := g.Plays
	for _, k := range live {
		if w, ok := stats[mediaindex.KeyRatingKey+"\x00"+k]; ok {
			orphan -= w.Plays
		}
	}
	if orphan <= 0 {
		return mediaindex.WatchStat{}, "", nil
	}
	return mediaindex.WatchStat{}, fmt.Sprintf("%d Plex items share this item's guid, and %d plays of an item that is no longer in the Plex index cannot be attributed to one of them",
		len(live), orphan), nil
}

// lowerBound: a user or one of the file's sections has keep_history 0, so the counts are lower
// bounds.
func lowerBound(cs *cacheState, sections []string) bool {
	if n, ok := cs.stats["usersWithoutHistory"].(float64); ok && n > 0 {
		return true
	}
	if list, ok := cs.stats["sectionsWithoutHistory"].([]any); ok {
		for _, s := range list {
			if str, ok := s.(string); ok && slices.Contains(sections, str) {
				return true
			}
		}
	}
	return false
}

// watchStats reads (once per Load) a Tautulli integration's rows.
func (lc *loadCtx) watchStats(id int64) (map[string]mediaindex.WatchStat, error) {
	key := "watch:" + strconv.FormatInt(id, 10)
	if v, ok := lc.memo(key); ok {
		return v.(map[string]mediaindex.WatchStat), nil
	}
	out := map[string]mediaindex.WatchStat{}
	if err := lc.p.index.EachWatchStat(lc.ctx, lc.q, id, func(w mediaindex.WatchStat) error {
		out[w.KeyType+"\x00"+w.Key] = w
		return nil
	}); err != nil {
		return nil, fmt.Errorf("tiers: %w", err)
	}
	lc.setMemo(key, out)
	return out, nil
}

// requestFacts builds the Seerr facts of a file.
func (lc *loadCtx) requestFacts(f *Facts) (*RequestFacts, error) {
	list := lc.caches[integrations.TypeSeerr]
	if len(list) == 0 {
		return &RequestFacts{Requested: Unknown, Why: "no Seerr integration is set up", Users: []int64{}}, nil
	}
	results := make([]*RequestFacts, 0, len(list))
	for _, cs := range list {
		r, err := lc.seerrOne(cs, f)
		if err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return combineRequests(results), nil
}

// combineRequests ORs several Seerr integrations' facts.
func combineRequests(list []*RequestFacts) *RequestFacts {
	out := &RequestFacts{Requested: False, Users: []int64{}}
	var unknown *RequestFacts
	for _, r := range list {
		switch r.Requested {
		case True:
			out.Requested, out.IntegrationID = True, r.IntegrationID
			for _, u := range r.Users {
				if !slices.Contains(out.Users, u) {
					out.Users = append(out.Users, u)
				}
			}
		case Unknown:
			if unknown == nil {
				unknown = r
			}
		}
	}
	if out.Requested != True && unknown != nil {
		return &RequestFacts{Requested: Unknown, Why: unknown.Why, IntegrationID: unknown.IntegrationID, Users: []int64{}}
	}
	if out.Requested == False && len(list) > 0 {
		out.IntegrationID = list[0].IntegrationID
	}
	slices.Sort(out.Users)
	return out
}

// seerrOne decides seerr.requested for a file from one Seerr integration (§8.2).
func (lc *loadCtx) seerrOne(cs *cacheState, f *Facts) (*RequestFacts, error) {
	unknown := func(why string) *RequestFacts {
		return &RequestFacts{Requested: Unknown, Why: why, IntegrationID: cs.it.ID, Users: []int64{}}
	}
	d, why, err := lc.linkedPlex(cs, false)
	if err != nil {
		return nil, err
	}
	if why != "" && !(cs.fresh.Fresh && linkOf(cs.it) != 0 && linkOf(cs.it) == cs.link) {
		return unknown(why), nil
	}
	idx, err := lc.seerrIndex(cs.it.ID)
	if err != nil {
		return nil, err
	}
	var (
		mediaType  string
		tmdb, tvdb int64
		keys       []string
		seasons    []int
		seasonOK   bool
	)
	if f.Arr.State == ArrItem && f.Arr.Item != nil {
		switch f.Arr.Item.Kind {
		case mediaindex.KindMovie:
			mediaType, tmdb = "movie", f.Arr.Item.ExternalIDs.TMDB
		case mediaindex.KindSeries:
			mediaType, tmdb, tvdb = "tv", f.Arr.Item.ExternalIDs.TMDB, f.Arr.Item.ExternalIDs.TVDB
			if !f.Arr.ByFolder {
				seasons, seasonOK = lc.arrSeasons[f.LocalPath], len(lc.arrSeasons[f.LocalPath]) > 0
			}
		}
	}
	// The Plex rating key is a further fallback, only through the Seerr integration's own Plex
	// server (and only while its index is fresh).
	if d != nil {
		for _, it := range d.fileItems(f) {
			switch it.Type {
			case "movie":
				if mediaType == "" {
					mediaType = "movie"
				}
				keys = append(keys, it.RatingKey)
			case "episode":
				if mediaType == "" {
					mediaType = "tv"
				}
				if it.GrandparentKey != "" {
					keys = append(keys, it.GrandparentKey)
				}
				if !seasonOK && it.ParentIndex != nil {
					seasons, seasonOK = []int{*it.ParentIndex}, true
				}
			}
		}
	}
	if tmdb == 0 && tvdb == 0 && len(keys) == 0 {
		if why != "" {
			return unknown(why), nil
		}
		return unknown("no TMDB, TVDB or Plex id of this file is known"), nil
	}
	var matching []mediaindex.SeerrRequest
	for _, i := range idx.candidates(mediaType, tmdb, tvdb, keys) {
		r := idx.reqs[i]
		if r.MediaType != mediaType || !seerrCounted(r.Status) {
			continue
		}
		hit := false
		switch {
		case mediaType == "tv" && tvdb != 0 && r.TVDBID != 0:
			hit = r.TVDBID == tvdb
		case tmdb != 0 && r.TMDBID != 0:
			hit = r.TMDBID == tmdb
		}
		if !hit && r.RatingKey != "" && slices.Contains(keys, r.RatingKey) {
			hit = true
		}
		if hit {
			matching = append(matching, r)
		}
	}
	out := &RequestFacts{Requested: False, IntegrationID: cs.it.ID, Users: []int64{}}
	addUser := func(u int64) {
		if u != 0 && !slices.Contains(out.Users, u) {
			out.Users = append(out.Users, u)
		}
	}
	if mediaType == "movie" {
		for _, r := range matching {
			out.Requested = True
			addUser(r.UserID)
		}
		slices.Sort(out.Users)
		return out, nil
	}
	if !seasonOK {
		// An extra in the series folder: true if a counted request covers every season, false
		// if the series has none, unknown otherwise.
		if len(matching) == 0 {
			return out, nil
		}
		for _, r := range matching {
			if len(r.Seasons) == 0 {
				out.Requested = True
				addUser(r.UserID)
			}
		}
		if out.Requested != True {
			return unknown("the file's season is not known, and no request covers every season"), nil
		}
		slices.Sort(out.Users)
		return out, nil
	}
	for _, r := range matching {
		covers := len(r.Seasons) == 0
		for _, s := range seasons {
			covers = covers || slices.Contains(r.Seasons, s)
		}
		if covers {
			out.Requested = True
			addUser(r.UserID)
		}
	}
	slices.Sort(out.Users)
	return out, nil
}

// seerrCounted: statuses 1, 2, 4 and 5 count (§6.2).
func seerrCounted(status int) bool { return status == 1 || status == 2 || status == 4 || status == 5 }

// seerrIdx is a Seerr integration's cached requests, indexed by the ids a file is matched by, so
// a file costs a few lookups instead of a scan of every request.
type seerrIdx struct {
	reqs   []mediaindex.SeerrRequest
	byTMDB map[string][]int
	byTVDB map[int64][]int
	byKey  map[string][]int
}

func (lc *loadCtx) seerrIndex(id int64) (*seerrIdx, error) {
	key := "seerrIdx:" + strconv.FormatInt(id, 10)
	if v, ok := lc.memo(key); ok {
		return v.(*seerrIdx), nil
	}
	reqs, err := lc.seerrRequests(id)
	if err != nil {
		return nil, err
	}
	x := &seerrIdx{reqs: reqs, byTMDB: map[string][]int{}, byTVDB: map[int64][]int{}, byKey: map[string][]int{}}
	for i, r := range reqs {
		if r.TMDBID != 0 {
			k := r.MediaType + "\x00" + strconv.FormatInt(r.TMDBID, 10)
			x.byTMDB[k] = append(x.byTMDB[k], i)
		}
		if r.TVDBID != 0 {
			x.byTVDB[r.TVDBID] = append(x.byTVDB[r.TVDBID], i)
		}
		if r.RatingKey != "" {
			x.byKey[r.RatingKey] = append(x.byKey[r.RatingKey], i)
		}
	}
	lc.setMemo(key, x)
	return x, nil
}

// candidates returns, in cache order, the requests that can match a file with these ids: a
// superset of the matches, which the caller filters.
func (x *seerrIdx) candidates(mediaType string, tmdb, tvdb int64, keys []string) []int {
	var out []int
	if tmdb != 0 {
		out = append(out, x.byTMDB[mediaType+"\x00"+strconv.FormatInt(tmdb, 10)]...)
	}
	if tvdb != 0 {
		out = append(out, x.byTVDB[tvdb]...)
	}
	for _, k := range keys {
		out = append(out, x.byKey[k]...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func (lc *loadCtx) seerrRequests(id int64) ([]mediaindex.SeerrRequest, error) {
	key := "seerr:" + strconv.FormatInt(id, 10)
	if v, ok := lc.memo(key); ok {
		return v.([]mediaindex.SeerrRequest), nil
	}
	var out []mediaindex.SeerrRequest
	if err := lc.p.index.EachSeerrRequest(lc.ctx, lc.q, id, func(r mediaindex.SeerrRequest) error {
		out = append(out, r)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("tiers: %w", err)
	}
	lc.setMemo(key, out)
	return out, nil
}

// maintainerrFacts builds the Maintainerr facts of a file.
func (lc *loadCtx) maintainerrFacts(f *Facts) (*MaintainerrFacts, error) {
	list := lc.caches[integrations.TypeMaintainerr]
	if len(list) == 0 {
		return &MaintainerrFacts{Pending: Unknown, Why: "no Maintainerr integration is set up"}, nil
	}
	var (
		out     = &MaintainerrFacts{Pending: False}
		unknown *MaintainerrFacts
	)
	for _, cs := range list {
		m, err := lc.maintainerrOne(cs, f)
		if err != nil {
			return nil, err
		}
		switch m.Pending {
		case True:
			if out.Pending != True || (m.DeleteAfter != nil && (out.DeleteAfter == nil || m.DeleteAfter.Before(*out.DeleteAfter))) {
				out = m
			}
		case Unknown:
			if unknown == nil {
				unknown = m
			}
		case False:
			if out.Pending == False && out.IntegrationID == 0 {
				out.IntegrationID = m.IntegrationID
			}
		}
	}
	if out.Pending != True && unknown != nil {
		return unknown, nil
	}
	return out, nil
}

// maintainerrOne decides maintainerr.pendingDelete for a file from one Maintainerr integration
// (§8.2).
func (lc *loadCtx) maintainerrOne(cs *cacheState, f *Facts) (*MaintainerrFacts, error) {
	unknown := func(why string) *MaintainerrFacts {
		return &MaintainerrFacts{Pending: Unknown, Why: why, IntegrationID: cs.it.ID}
	}
	d, why, err := lc.linkedPlex(cs, true)
	if err != nil {
		return nil, err
	}
	if why != "" {
		return unknown(why), nil
	}
	idx, err := lc.maintainerrIndex(cs.it.ID)
	if err != nil {
		return nil, err
	}
	out := &MaintainerrFacts{Pending: False, IntegrationID: cs.it.ID}
	pending := func(r mediaindex.MaintainerrItem) {
		if out.Pending != True || (r.DeleteAfter != nil && (out.DeleteAfter == nil || r.DeleteAfter.Before(*out.DeleteAfter))) {
			out.DeleteAfter = r.DeleteAfter
		}
		out.Pending = True
	}
	items := d.fileItems(f)
	// By the file's Plex item: its own rating key, its season's or its show's.
	var keys []string
	for _, it := range items {
		keys = append(keys, it.RatingKey)
		if it.ParentKey != "" {
			keys = append(keys, it.ParentKey)
		}
		if it.GrandparentKey != "" {
			keys = append(keys, it.GrandparentKey)
		}
	}
	for _, i := range idx.byKeys(keys) {
		r := idx.rows[i]
		if r.PlexIntegrationID != d.st.it.ID {
			continue
		}
		if r.State != "pending" {
			return unknown(fmt.Sprintf("Maintainerr collection %q may delete this file, but whether an exclusion covers it is not known", r.CollectionTitle)), nil
		}
		pending(r)
	}
	if out.Pending == True {
		return out, nil
	}
	// The tmdb/tvdb fallback (key churn): only movie- and show-level rows whose key is not in the
	// Plex index, only in the row's own library.
	var tmdb, tvdb int64
	if f.Arr.State == ArrItem && f.Arr.Item != nil {
		tmdb, tvdb = f.Arr.Item.ExternalIDs.TMDB, f.Arr.Item.ExternalIDs.TVDB
	}
	for _, it := range items {
		ids := it.ExternalIDs
		if it.Type == "episode" || it.Type == "season" {
			if show, ok := d.items[it.GrandparentKey]; ok && it.Type == "episode" {
				ids = show.ExternalIDs
			} else if show, ok := d.items[it.ParentKey]; ok && it.Type == "season" {
				ids = show.ExternalIDs
			}
		}
		if tmdb == 0 {
			tmdb, _ = strconv.ParseInt(ids["tmdb"], 10, 64)
		}
		if tvdb == 0 {
			tvdb, _ = strconv.ParseInt(ids["tvdb"], 10, 64)
		}
	}
	sections := d.sectionsOf(f, items)
	if len(sections) == 0 && f.Plex != nil && f.Plex.Known && f.Plex.IntegrationID == d.st.it.ID {
		sections = []string{strings.TrimPrefix(f.Plex.Section, fmt.Sprintf("%d:", d.st.it.ID))}
	}
	if len(items) == 0 && tmdb == 0 && tvdb == 0 {
		return unknown("the file has no Plex item and no TMDB or TVDB id"), nil
	}
	for _, i := range idx.byIDs(tmdb, tvdb) {
		r := idx.rows[i]
		if r.PlexIntegrationID != d.st.it.ID {
			continue
		}
		idHit := (r.TMDBID != 0 && r.TMDBID == tmdb && r.Level == "movie") ||
			(r.Level != "movie" && ((r.TVDBID != 0 && r.TVDBID == tvdb) || (r.TVDBID == 0 && r.TMDBID != 0 && r.TMDBID == tmdb)))
		if !idHit {
			continue
		}
		switch r.Level {
		case "season", "episode":
			if len(items) == 0 {
				return unknown(fmt.Sprintf("the file has no Plex item, and Maintainerr collection %q holds a %s of its series", r.CollectionTitle, r.Level)), nil
			}
			continue
		}
		if !slices.Contains(sections, r.LibraryID) {
			continue
		}
		if known, err := lc.plexKnown(d, r.RatingKey); err != nil {
			return nil, err
		} else if known {
			continue
		}
		if r.State != "pending" {
			return unknown(fmt.Sprintf("Maintainerr collection %q may delete this file, but whether an exclusion covers it is not known", r.CollectionTitle)), nil
		}
		pending(r)
	}
	return out, nil
}

// maintainerrIdx is a Maintainerr integration's cached rows, indexed by rating key and by the
// TMDB and TVDB ids, so a file costs a few lookups instead of scans of every row.
type maintainerrIdx struct {
	rows   []mediaindex.MaintainerrItem
	byKey  map[string][]int
	byTMDB map[int64][]int
	byTVDB map[int64][]int
}

func (lc *loadCtx) maintainerrIndex(id int64) (*maintainerrIdx, error) {
	key := "maintainerrIdx:" + strconv.FormatInt(id, 10)
	if v, ok := lc.memo(key); ok {
		return v.(*maintainerrIdx), nil
	}
	rows, err := lc.maintainerrRows(id)
	if err != nil {
		return nil, err
	}
	x := &maintainerrIdx{rows: rows, byKey: map[string][]int{}, byTMDB: map[int64][]int{}, byTVDB: map[int64][]int{}}
	for i, r := range rows {
		x.byKey[r.RatingKey] = append(x.byKey[r.RatingKey], i)
		if r.TMDBID != 0 {
			x.byTMDB[r.TMDBID] = append(x.byTMDB[r.TMDBID], i)
		}
		if r.TVDBID != 0 {
			x.byTVDB[r.TVDBID] = append(x.byTVDB[r.TVDBID], i)
		}
	}
	lc.setMemo(key, x)
	return x, nil
}

// byKeys returns, in cache order, the rows of these rating keys.
func (x *maintainerrIdx) byKeys(keys []string) []int {
	var out []int
	for _, k := range keys {
		out = append(out, x.byKey[k]...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// byIDs returns, in cache order, the rows with this TMDB or TVDB id (a superset of the id matches,
// which the caller filters).
func (x *maintainerrIdx) byIDs(tmdb, tvdb int64) []int {
	var out []int
	if tmdb != 0 {
		out = append(out, x.byTMDB[tmdb]...)
	}
	if tvdb != 0 {
		out = append(out, x.byTVDB[tvdb]...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func (lc *loadCtx) maintainerrRows(id int64) ([]mediaindex.MaintainerrItem, error) {
	key := "maintainerr:" + strconv.FormatInt(id, 10)
	if v, ok := lc.memo(key); ok {
		return v.([]mediaindex.MaintainerrItem), nil
	}
	var out []mediaindex.MaintainerrItem
	if err := lc.p.index.EachMaintainerrItem(lc.ctx, lc.q, id, func(m mediaindex.MaintainerrItem) error {
		out = append(out, m)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("tiers: %w", err)
	}
	lc.setMemo(key, out)
	return out, nil
}

func (lc *loadCtx) memo(key string) (any, bool) {
	v, ok := lc.memos[key]
	return v, ok
}

func (lc *loadCtx) setMemo(key string, v any) { lc.memos[key] = v }

// jsonUnmarshal decodes stored stats, ignoring what does not fit.
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// Unknown implements Provider: the Plex indexes (turned on), Tautulli, Seerr and Maintainerr
// caches that are not fresh now.
func (p *IndexProvider) Unknown(ctx context.Context, q Queryer, now time.Time) ([]UnknownSource, error) {
	lc, err := p.newLoad(ctx, q, now)
	if err != nil {
		return nil, err
	}
	var out []UnknownSource
	for _, typ := range []integrations.Type{integrations.TypePlex, integrations.TypeTautulli, integrations.TypeSeerr, integrations.TypeMaintainerr} {
		for _, cs := range lc.caches[typ] {
			if typ == integrations.TypePlex {
				if _, has := mediaindex.StaleAfter(cs.it); !has {
					continue
				}
			}
			if !cs.fresh.Fresh {
				out = append(out, UnknownSource{IntegrationID: cs.it.ID, Name: cs.it.Name, Reason: cs.fresh.Reason})
			} else if typ != integrations.TypePlex && linkOf(cs.it) != cs.link {
				out = append(out, UnknownSource{IntegrationID: cs.it.ID, Name: cs.it.Name,
					Reason: fmt.Sprintf("%s was linked to another Plex server since its last refresh", cs.it.Name)})
			} else if why := mismatchOf(cs); why != "" {
				out = append(out, UnknownSource{IntegrationID: cs.it.ID, Name: cs.it.Name, Reason: why})
			} else if plexCS := lc.byID[cs.link]; typ != integrations.TypePlex && typ != integrations.TypeSeerr && plexCS != nil &&
				plexCS.it.Type == integrations.TypePlex && plexCS.fresh.Fresh && !samePlexServer(cs.plexInstance, plexCS.instance) {
				// Tautulli's and Maintainerr's rating keys are another server's (Seerr keeps its
				// TMDB and TVDB matches).
				out = append(out, UnknownSource{IntegrationID: cs.it.ID, Name: cs.it.Name, Reason: rebuiltWhy(cs, plexCS)})
			}
		}
	}
	return out, nil
}

// Suggestions implements Provider: the indexed Plex sections, and the Seerr users (read live, with
// a short timeout; a failure is not retried for usersRetry).
func (p *IndexProvider) Suggestions(ctx context.Context, field string) ([]Suggestion, error) {
	list, err := p.ints.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []Suggestion
	switch field {
	case FieldPlexSection:
		for _, it := range list {
			if it.Type != integrations.TypePlex {
				continue
			}
			secs, err := p.index.PlexSections(ctx, nil, it.ID)
			if err != nil {
				return nil, err
			}
			for _, s := range secs {
				out = append(out, Suggestion{Value: fmt.Sprintf("%d:%s", it.ID, s.Key), Label: fmt.Sprintf("%s: %s", it.Name, s.Title)})
			}
		}
	case FieldSeerrRequestedBy:
		for _, it := range list {
			if it.Type != integrations.TypeSeerr || !it.Enabled {
				continue
			}
			users, err := p.usersOf(ctx, it, true)
			if err != nil {
				return nil, err
			}
			out = append(out, users...)
		}
	}
	return out, nil
}

// The live Seerr user list: reused for usersTTL; a failed fetch is not retried for usersRetry; a
// fetch waits at most usersTimeout (the editor's suggestions, never a sync).
const (
	usersTTL     = 5 * time.Minute
	usersRetry   = time.Minute
	usersTimeout = 5 * time.Second
)

// errNoUsers: no user list was fetched yet and live fetches are not allowed here.
var errNoUsers = errors.New("the Seerr user list has not been read yet")

// usersOf returns a Seerr integration's users. live false never goes to Seerr: it returns the last
// list read (whatever its age), or errNoUsers.
func (p *IndexProvider) usersOf(ctx context.Context, it integrations.Integration, live bool) ([]Suggestion, error) {
	if p.seerrUsers == nil {
		return nil, nil
	}
	p.mu.Lock()
	c, ok := p.users[it.ID]
	p.mu.Unlock()
	now := time.Now()
	fetched := ok && !c.at.IsZero()
	switch {
	case fetched && now.Sub(c.at) < usersTTL:
		return c.list, nil
	case !live && fetched:
		return c.list, nil
	case !live:
		return nil, errNoUsers
	case ok && c.err != nil && now.Sub(c.failedAt) < usersRetry:
		if fetched {
			return c.list, nil
		}
		return nil, c.err
	}
	fctx, cancel := context.WithTimeout(ctx, usersTimeout)
	list, err := p.seerrUsers(fctx, it)
	cancel()
	p.mu.Lock()
	defer p.mu.Unlock()
	c = p.users[it.ID]
	if err != nil {
		c.failedAt, c.err = time.Now(), err
		p.users[it.ID] = c
		if !c.at.IsZero() {
			return c.list, nil
		}
		return nil, err
	}
	p.users[it.ID] = userCache{at: time.Now(), list: list}
	return list, nil
}

// knownMemoKey carries a per-call memo of the stale-reference checks (Engine.staleReferences), so
// one rule set's conditions share one read of the caches.
type knownMemoKey struct{}

type knownMemo struct {
	mu sync.Mutex
	m  map[string]any
	// live allows live reads of Seerr's users (the rule editor's save and preview; never a sync or
	// a manifest build, which must not wait on the network).
	live bool
}

// withKnownMemo returns ctx with a fresh memo for Provider.Known calls.
func withKnownMemo(ctx context.Context, live bool) context.Context {
	return context.WithValue(ctx, knownMemoKey{}, &knownMemo{m: map[string]any{}, live: live})
}

func memoOf(ctx context.Context) *knownMemo {
	m, _ := ctx.Value(knownMemoKey{}).(*knownMemo)
	return m
}

// Known implements Provider: a Plex section some fresh index has, or Seerr users that a fresh
// cache's requests or the Seerr user list know. The user list is read live only when the caller
// allows it (the editor); otherwise only what was read before counts.
func (p *IndexProvider) Known(ctx context.Context, q Queryer, field, value string, ids []int64, now time.Time) (bool, error) {
	memo := memoOf(ctx)
	load := func() (*loadCtx, error) {
		if memo == nil {
			return p.newLoad(ctx, q, now)
		}
		memo.mu.Lock()
		defer memo.mu.Unlock()
		if v, ok := memo.m["load"]; ok {
			return v.(*loadCtx), nil
		}
		lc, err := p.newLoad(ctx, q, now)
		if err == nil {
			memo.m["load"] = lc
		}
		return lc, err
	}
	switch field {
	case FieldPlexSection:
		pid, key, ok := parseSection(value)
		if !ok {
			return false, nil
		}
		lc, err := load()
		if err != nil {
			return false, err
		}
		cs := lc.byID[pid]
		if cs == nil || cs.it.Type != integrations.TypePlex || !cs.fresh.Fresh {
			return false, nil
		}
		secs, err := p.index.PlexSections(ctx, q, pid)
		if err != nil {
			return false, err
		}
		return slices.ContainsFunc(secs, func(s mediaindex.PlexSection) bool { return s.Key == key }), nil
	case FieldSeerrRequestedBy:
		lc, err := load()
		if err != nil {
			return false, err
		}
		ku, err := p.knownUsers(ctx, lc, memo)
		if err != nil {
			return false, err
		}
		for _, id := range ids {
			// Without a user list (none read since Bunkarr started, and a sync never reads one
			// live) a user no request names may well exist: that is not a stale reference.
			if !ku.known[id] && ku.complete {
				return false, nil
			}
		}
		return true, nil
	}
	return true, nil
}

// seerrUserSet is what the fresh Seerr caches know of the Seerr users: known are the users of
// their requests and user lists; complete is false when a fresh cache's user list is not
// available (never read since start, or its fetch failed), so a user outside known may exist.
type seerrUserSet struct {
	known    map[int64]bool
	complete bool
}

// knownUsers returns the Seerr users the fresh caches' requests or the user lists know (once per
// memo, so every condition of one call sees the same list).
func (p *IndexProvider) knownUsers(ctx context.Context, lc *loadCtx, memo *knownMemo) (seerrUserSet, error) {
	if memo != nil {
		memo.mu.Lock()
		v, ok := memo.m["seerrUsers"]
		memo.mu.Unlock()
		if ok {
			return v.(seerrUserSet), nil
		}
	}
	live := memo != nil && memo.live
	out := seerrUserSet{known: map[int64]bool{}, complete: true}
	for _, cs := range lc.caches[integrations.TypeSeerr] {
		if !cs.fresh.Fresh {
			continue
		}
		reqs, err := lc.seerrRequests(cs.it.ID)
		if err != nil {
			return seerrUserSet{}, err
		}
		for _, r := range reqs {
			out.known[r.UserID] = true
		}
		users, err := p.usersOf(ctx, cs.it, live)
		if err != nil {
			out.complete = false
			continue
		}
		for _, u := range users {
			if id, ok := u.Value.(int64); ok {
				out.known[id] = true
			}
		}
	}
	if memo != nil {
		memo.mu.Lock()
		memo.m["seerrUsers"] = out
		memo.mu.Unlock()
	}
	return out, nil
}
