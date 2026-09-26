package manifest

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/logging"
	"github.com/sl0wz3r/bunkarr/internal/mediaindex"
	"github.com/sl0wz3r/bunkarr/internal/testhooks"
	"github.com/sl0wz3r/bunkarr/internal/version"
)

// PointBuildItem is reached once per *arr item while a manifest is built (the export's fault
// test: a build that fails half-way must not send a byte, design §15).
const PointBuildItem = "manifest.buildItem"

// GeneratorName is Manifest.Generator.Name.
const GeneratorName = "Bunkarr"

// BuilderOptions configures NewBuilder.
type BuilderOptions struct {
	// DB is Bunkarr's database (required); the manifest is read in one transaction of its read
	// pool.
	DB *db.DB
	// Catalog lists the sources (required).
	Catalog *catalog.Store
	// Integrations lists the *arr integrations (required).
	Integrations *integrations.Store
	// Index reads the metadata index; nil builds one over DB and Catalog.
	Index *mediaindex.Store
	// Tiers decides tiers; nil is AllFull.
	Tiers Tiers
	// Now is the clock (tests); nil is time.Now.
	Now func() time.Time
	// Version is Generator.Version; "" is version.Version.
	Version string
}

// Builder builds manifests (Build).
type Builder struct {
	o BuilderOptions
}

// NewBuilder returns a Builder.
func NewBuilder(o BuilderOptions) (*Builder, error) {
	switch {
	case o.DB == nil:
		return nil, errors.New("manifest: no database")
	case o.Catalog == nil:
		return nil, errors.New("manifest: no catalog")
	case o.Integrations == nil:
		return nil, errors.New("manifest: no integrations store")
	}
	if o.Index == nil {
		o.Index = mediaindex.NewStore(o.DB, o.Catalog)
	}
	if o.Tiers == nil {
		o.Tiers = AllFull{}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Version == "" {
		o.Version = version.Version
	}
	return &Builder{o: o}, nil
}

// BuildScope selects what Build covers.
type BuildScope struct {
	// Destination is the destination whose view the manifest is: its linked sources, and its
	// tiers, kept files and records. nil builds the export scope (every enabled source, no
	// tiers).
	Destination *destinations.Destination
	// Job names the job writing a destination version (nil for an export).
	Job *JobRef
}

// Build builds a manifest (design §11.1) from the catalog, the metadata index, the tier
// decisions and the destination's records, all read in one read transaction. Only the sources and
// integrations (configuration, not state) are listed before it starts; the tier decisions take
// them from these lists and read everything else through the transaction too, so a build uses one
// connection of the read pool, never two (TierRead).
func (b *Builder) Build(ctx context.Context, scope BuildScope) (*Manifest, error) {
	srcs, err := b.o.Catalog.List(ctx)
	if err != nil {
		return nil, err
	}
	ints, err := b.o.Integrations.List(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := b.o.DB.Reader().BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin the manifest's read transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	w := &builder{b: b, q: tx, scope: scope, now: b.o.Now().UTC(), locator: catalog.NewLocator(srcs),
		sources: map[int64]catalog.Source{}, scopeSources: map[int64]bool{}, entries: map[entryKey]*entry{},
		decide: map[int64]func(int64) Decision{}, used: map[int64]bool{}, folderItems: map[string]int{},
		stems: map[entryKey][]stemRef{}, tierRead: TierRead{Q: tx, Sources: srcs, Integrations: slices.Clone(ints)}}
	for _, s := range srcs {
		w.sources[s.ID] = s
	}
	if err := w.run(ctx, ints); err != nil {
		return nil, err
	}
	return w.m, nil
}

// entryKey is a file's place: its source and path inside it (a directory, for stems).
type entryKey struct {
	src int64
	rel string
}

// entry is a file of the scope: a live catalog file of a scope source, or a live record of the
// destination whose catalog file is gone (fileID 0).
type entry struct {
	key     entryKey
	size    int64
	mtimeNs int64
	fileID  int64
	rec     *recordRow
	claimed bool
}

// stemRef is a media file claimed by an *arr item, for the stem attribution of sidecars.
type stemRef struct {
	stem string
	item int
}

// builder is one Build.
type builder struct {
	b            *Builder
	q            Queryer
	tierRead     TierRead
	scope        BuildScope
	now          time.Time
	locator      *catalog.Locator
	sources      map[int64]catalog.Source
	scopeSources map[int64]bool
	entries      map[entryKey]*entry
	records      map[int64]*recordRow
	decide       map[int64]func(int64) Decision
	// used are the sources the listed files refer to.
	used map[int64]bool
	// folderItems maps an item's local folder to its index in m.Items (the first item wins).
	folderItems map[string]int
	// stems maps a directory (source, dir) to the claimed media files in it.
	stems map[entryKey][]stemRef
	m     *Manifest
}

func (w *builder) destScope() bool { return w.scope.Destination != nil }

func (w *builder) run(ctx context.Context, ints []integrations.Integration) error {
	m := &Manifest{Format: FormatName, FormatVersion: FormatVersion, CreatedAt: w.now,
		Generator: Generator{Name: GeneratorName, Version: w.b.o.Version}, Scope: Scope{Kind: ScopeExport},
		Integrations: []Integration{}, Sources: []Source{}, Items: []Item{}, OtherFiles: []ExtraFile{}}
	if w.scope.Job != nil {
		j := JobRef{ID: w.scope.Job.ID, QueuedAt: w.scope.Job.QueuedAt.UTC()}
		m.Job = &j
	}
	w.m = m
	if d := w.scope.Destination; d != nil {
		m.Scope = Scope{Kind: ScopeDestination, DestinationID: d.ID, DestinationName: d.Name}
		for _, id := range d.SourceIDs {
			if _, ok := w.sources[id]; ok {
				w.scopeSources[id] = true
			}
		}
	} else {
		for id, s := range w.sources {
			if s.Enabled {
				w.scopeSources[id] = true
			}
		}
	}
	if err := w.loadFiles(ctx); err != nil {
		return err
	}
	if err := w.loadItems(ctx, ints); err != nil {
		return err
	}
	w.attributeRest()
	w.listSources()
	w.summarize()
	return nil
}

// loadFiles reads the scope's live catalog files, the destination's live records and the tier
// decision functions.
func (w *builder) loadFiles(ctx context.Context) error {
	for _, id := range sortedKeys(w.scopeSources) {
		err := liveCatalog(ctx, w.q, id, func(c catalogRow) error {
			k := entryKey{id, c.rel}
			w.entries[k] = &entry{key: k, size: c.size, mtimeNs: c.mtimeNs, fileID: c.id}
			return nil
		})
		if err != nil {
			return err
		}
		if w.destScope() {
			fn, err := w.b.o.Tiers.Decide(ctx, w.tierRead, w.scope.Destination.ID, id)
			if err != nil {
				return fmt.Errorf("decide the tiers of source %d: %w", id, err)
			}
			w.decide[id] = fn
		}
	}
	if !w.destScope() {
		return nil
	}
	recs, err := liveRecords(ctx, w.q, w.scope.Destination.ID)
	if err != nil {
		return err
	}
	w.records = recs
	for _, id := range sortedKeys(recs) {
		r := recs[id]
		k := entryKey{r.sourceID, r.sourceRel}
		if r.sourceID != 0 {
			e, ok := w.entries[k]
			if !ok {
				// The destination holds a file whose catalog file is gone (or whose source is no
				// longer linked): it is listed all the same (S20).
				w.entries[k] = &entry{key: k, size: r.size, mtimeNs: r.mtimeNs, rec: r}
				continue
			}
			if e.rec == nil {
				e.rec = r
				continue
			}
		}
		// Every live record is listed on its own (S20). One whose source was deleted, or a second
		// record of the same source file, is listed without a source at its path at the
		// destination (unique among live records), so two deleted sources that both held
		// "Avatar (2009)/Avatar (2009).mkv" are two files, each found where a restore looks.
		k = entryKey{0, r.relPath}
		w.entries[k] = &entry{key: k, size: r.size, mtimeNs: r.mtimeNs, rec: r}
	}
	return nil
}

// loadItems lists every non-deleted item of every enabled *arr integration with its files.
func (w *builder) loadItems(ctx context.Context, ints []integrations.Integration) error {
	slices.SortFunc(ints, func(a, b integrations.Integration) int { return cmp.Compare(a.ID, b.ID) })
	states, err := w.b.o.Index.States(ctx, w.q)
	if err != nil {
		return err
	}
	for _, it := range ints {
		if !it.Enabled || !it.Type.IsArr() {
			continue
		}
		if err := w.loadIntegration(ctx, it, states[it.ID]); err != nil {
			return err
		}
	}
	return nil
}

func (w *builder) loadIntegration(ctx context.Context, it integrations.Integration, st mediaindex.State) error {
	if st.IntegrationID == 0 {
		st = mediaindex.State{IntegrationID: it.ID, Status: mediaindex.StatusNever}
	}
	fresh := false
	if stale, ok := mediaindex.StaleAfter(it); ok {
		fresh = mediaindex.FreshnessOf(st, it, stale, testhooks.FreshnessNow(w.now)).Fresh
	}
	meta, err := w.b.o.Index.Meta(ctx, w.q, it.ID)
	if err != nil {
		return err
	}
	mi := Integration{ID: it.ID, Type: string(it.Type), Name: it.Name, AppVersion: st.AppVersion, RefreshedAt: utcPtr(st.RefreshedAt),
		Status: st.Status, Fresh: fresh, QualityProfiles: []Named{}, MetadataProfiles: []Named{}, RootFolders: []RootFolder{}, Tags: []Tag{}}
	if st.Error != "" {
		e := scrubError(st.Error, it.URL)
		mi.LastError = &e
	}
	profiles, metaProfiles, tags := map[int64]string{}, map[int64]string{}, map[int64]string{}
	for _, p := range meta.QualityProfiles {
		mi.QualityProfiles = append(mi.QualityProfiles, Named{ID: p.ID, Name: p.Name})
		profiles[p.ID] = p.Name
	}
	for _, p := range meta.MetadataProfiles {
		mi.MetadataProfiles = append(mi.MetadataProfiles, Named{ID: p.ID, Name: p.Name})
		metaProfiles[p.ID] = p.Name
	}
	for _, rf := range meta.RootFolders {
		mi.RootFolders = append(mi.RootFolders, RootFolder{ID: rf.ID, Path: rf.Path, Accessible: rf.Accessible, LocalPath: rf.LocalPath, SourceID: rf.SourceID})
	}
	for _, t := range meta.Tags {
		mi.Tags = append(mi.Tags, Tag{ID: t.ID, Label: t.Label})
		tags[t.ID] = t.Label
	}
	w.m.Integrations = append(w.m.Integrations, mi)

	settings, _ := it.ArrSettings() // unreadable settings: no mappings, every item unlocated
	files := map[int64][]mediaindex.File{}
	err = w.b.o.Index.EachFile(ctx, w.q, it.ID, false, func(f mediaindex.File) error {
		files[f.ItemID] = append(files[f.ItemID], f)
		return nil
	})
	if err != nil {
		return err
	}
	return w.b.o.Index.EachItem(ctx, w.q, it.ID, false, func(src mediaindex.Item) error {
		faultinject.Point(PointBuildItem)
		idx := len(w.m.Items)
		item := Item{IntegrationID: it.ID, Kind: src.Kind, ArrID: src.ArrID, Title: src.Title, Year: src.Year,
			ExternalIDs: ExternalIDs(src.ExternalIDs), Path: src.Path, RootFolder: src.RootFolder,
			QualityProfile: profiles[src.QualityProfileID], MetadataProfile: metaProfiles[src.MetadataProfileID],
			Monitored: src.Monitored, Tags: tagLabels(src.Tags, tags), Genres: nonNil(src.Genres), Added: utcPtr(src.AddedAt),
			Detail: detailOf(src.Detail), Files: []File{}, ExtraFiles: []ExtraFile{}}
		if local, ok := settings.MapPath(src.Path); ok {
			item.Located = len(w.locator.Locate(local)) > 0
			if _, taken := w.folderItems[local]; !taken {
				w.folderItems[local] = idx
			}
		}
		for _, f := range files[src.ID] {
			item.Files = append(item.Files, w.file(f, idx))
		}
		w.m.Items = append(w.m.Items, item)
		return nil
	})
}

// file describes an *arr file, matching it with the scope's file at its mapped path and size
// (S18: a row applies to a catalog file only when both are equal).
func (w *builder) file(f mediaindex.File, item int) File {
	out := File{ArrFileID: f.ArrFileID, Path: f.Path, RelativePath: f.Detail.RelativePath, Size: f.Size, Quality: f.Quality,
		DateAdded: utcPtr(f.DateAdded)}
	for _, e := range f.Detail.Episodes {
		out.Episodes = append(out.Episodes, FileEpisode{Season: e.SeasonNumber, Episode: e.EpisodeNumber})
	}
	if f.Detail.Album != nil {
		out.AlbumID = f.Detail.Album.ID
	}
	e := w.match(f)
	switch {
	case e != nil:
		e.claimed = true
		out.Source = &FileSource{ID: e.key.src, RelPath: e.key.rel}
		dir, base := path.Split(e.key.rel)
		k := entryKey{e.key.src, strings.TrimSuffix(dir, "/")}
		w.stems[k] = append(w.stems[k], stemRef{stem: strings.TrimSuffix(base, path.Ext(base)), item: item})
	case f.Location != nil:
		out.Source = &FileSource{ID: f.Location.SourceID, RelPath: f.Location.Rel}
	}
	if out.Source != nil {
		w.used[out.Source.ID] = true
	}
	if w.destScope() {
		d := w.describe(e)
		out.Tier, out.Rule, out.Kept, out.BackedUp, out.SHA256 = d.tier, d.rule, &d.kept, &d.backedUp, d.sha256
	}
	return out
}

// match returns the scope's entry an *arr file applies to: at its mapped local path in a
// containing source (longest prefix first) with its size.
func (w *builder) match(f mediaindex.File) *entry {
	if f.LocalPath == "" {
		return nil
	}
	for _, loc := range w.locator.Locate(f.LocalPath) {
		if loc.Rel == "" {
			continue
		}
		if e, ok := w.entries[entryKey{loc.SourceID, loc.Rel}]; ok && e.size == f.Size {
			return e
		}
	}
	return nil
}

// description is what a destination holds of a file.
type description struct {
	tier     *Tier
	rule     *RuleRef
	kept     bool
	backedUp bool
	sha256   *string
}

// describe returns the destination's view of an entry (nil: no file of the scope matched).
func (w *builder) describe(e *entry) description {
	var d description
	if e == nil {
		return d
	}
	if e.fileID != 0 && w.scopeSources[e.key.src] {
		fn := w.decide[e.key.src]
		if fn == nil {
			fn, _ = AllFull{}.Decide(context.Background(), TierRead{}, 0, 0)
		}
		dec := fn(e.fileID)
		t := dec.Tier
		d.tier, d.rule = &t, &RuleRef{ID: dec.RuleID, Name: dec.RuleName}
		d.kept = e.rec != nil && t != TierFull
	}
	d.backedUp = w.backedUp(e)
	if e.rec != nil {
		h := e.rec.hash
		if p := w.records[e.rec.linkOf]; h == "" && p != nil {
			h = p.hash
		}
		if h = strings.TrimPrefix(h, filecopy.HashPrefix); h != "" {
			d.sha256 = &h
		}
	}
	return d
}

// backedUp reports whether the destination holds the entry's current content (design §11.1): a
// present record with the file's size and mtime, or a linked or link_recorded record whose
// primary is present or linked with them. A record whose catalog file is gone counts with its own.
func (w *builder) backedUp(e *entry) bool {
	r := e.rec
	if r == nil {
		return false
	}
	same := func(size, mtimeNs int64) bool {
		return e.fileID == 0 || (size == e.size && mtimeNs == e.mtimeNs)
	}
	switch r.state {
	case statePresent:
		return same(r.size, r.mtimeNs)
	case stateLinked, stateLinkRecorded:
		p := w.records[r.linkOf]
		if p == nil || (p.state != statePresent && p.state != stateLinked) {
			return false
		}
		return same(r.size, r.mtimeNs) && same(p.size, p.mtimeNs)
	}
	return false
}

// attributeRest lists the entries no *arr file claimed: as an item's extra (by stem, then by
// folder, design §8.3) or as an other file. A skip-tier file without a record is left out.
func (w *builder) attributeRest() {
	keys := make([]entryKey, 0, len(w.entries))
	for k, e := range w.entries {
		if !e.claimed {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, func(a, b entryKey) int { return cmp.Or(cmp.Compare(a.src, b.src), cmp.Compare(a.rel, b.rel)) })
	for _, k := range keys {
		e := w.entries[k]
		x := ExtraFile{RelPath: k.rel, Size: e.size}
		if k.src != 0 {
			src := k.src
			x.Source = &src
		}
		leftOut := false
		if w.destScope() {
			d := w.describe(e)
			x.Tier, x.Kept, x.BackedUp = d.tier, &d.kept, &d.backedUp
			leftOut = d.tier != nil && *d.tier == TierSkip && e.rec == nil
		}
		idx := w.attribute(e)
		switch {
		case leftOut:
			w.m.Summary.LeftOut++
			if idx >= 0 {
				w.m.Items[idx].SkippedFiles++
			}
		case idx >= 0:
			w.m.Items[idx].ExtraFiles = append(w.m.Items[idx].ExtraFiles, x)
			w.used[k.src] = true
		default:
			w.m.OtherFiles = append(w.m.OtherFiles, x)
			if k.src != 0 {
				w.used[k.src] = true
			}
		}
	}
}

// attribute returns the index of the item an unclaimed file belongs to, or -1: the item of the
// media file in the same directory whose stem is the longest prefix of its name, else the item
// whose folder is the longest prefix of its local path.
func (w *builder) attribute(e *entry) int {
	if e.key.src == 0 {
		return -1
	}
	dir, base := path.Split(e.key.rel)
	best, bestLen := -1, 0
	for _, s := range w.stems[entryKey{e.key.src, strings.TrimSuffix(dir, "/")}] {
		if s.stem != "" && strings.HasPrefix(base, s.stem) && len(s.stem) > bestLen {
			best, bestLen = s.item, len(s.stem)
		}
	}
	if best >= 0 {
		return best
	}
	local, ok := w.locator.Path(catalog.Location{SourceID: e.key.src, Rel: e.key.rel})
	if !ok {
		return -1
	}
	for p := path.Dir(local); ; p = path.Dir(p) {
		if i, ok := w.folderItems[p]; ok {
			return i
		}
		if p == "/" || p == "." {
			return -1
		}
	}
}

// listSources lists the scope's sources and every other source a listed file refers to.
func (w *builder) listSources() {
	ids := map[int64]bool{}
	for id := range w.scopeSources {
		ids[id] = true
	}
	for id := range w.used {
		ids[id] = true
	}
	for _, id := range sortedKeys(ids) {
		if s, ok := w.sources[id]; ok {
			w.m.Sources = append(w.m.Sources, Source{ID: s.ID, Name: s.Name, DestFolder: s.DestFolder})
		}
	}
}

// summarize fills m.Summary (LeftOut is counted by attributeRest).
func (w *builder) summarize() {
	s := &w.m.Summary
	count := func(size int64, tier *Tier, kept, backedUp *bool) {
		s.Files++
		s.Bytes += size
		if tier != nil {
			c := map[Tier]*Count{TierFull: &s.Tiers.Full, TierManifest: &s.Tiers.Manifest, TierSkip: &s.Tiers.Skip}[*tier]
			if c != nil {
				c.Files++
				c.Bytes += size
			}
		}
		if kept != nil && *kept {
			s.KeptFiles++
		}
		if backedUp != nil && *backedUp {
			s.BackedUpBytes += size
		}
	}
	for _, it := range w.m.Items {
		s.Items++
		if !it.Located {
			s.UnlocatedItems++
		}
		for _, f := range it.Files {
			count(f.Size, f.Tier, f.Kept, f.BackedUp)
		}
		for _, f := range it.ExtraFiles {
			count(f.Size, f.Tier, f.Kept, f.BackedUp)
		}
	}
	for _, f := range w.m.OtherFiles {
		count(f.Size, f.Tier, f.Kept, f.BackedUp)
	}
}

// StaleIntegrations returns the integrations whose cache was not fresh when m was built.
func (m *Manifest) StaleIntegrations() []Integration {
	var out []Integration
	for _, it := range m.Integrations {
		if !it.Fresh {
			out = append(out, it)
		}
	}
	return out
}

// detailOf converts an index item's detail.
func detailOf(d mediaindex.ItemDetail) ItemDetail {
	out := ItemDetail{MinimumAvailability: d.MinimumAvailability, SeriesType: d.SeriesType, SeasonFolder: d.SeasonFolder,
		MonitorNewItems: d.MonitorNewItems, UseSceneNumbering: d.UseSceneNumbering, LanguageProfileID: d.LanguageProfileID}
	for _, s := range d.Seasons {
		out.Seasons = append(out.Seasons, Season{SeasonNumber: s.SeasonNumber, Monitored: s.Monitored})
	}
	for _, e := range d.Episodes {
		out.Episodes = append(out.Episodes, Episode{Season: e.Season, Episode: e.Episode, Monitored: e.Monitored})
	}
	for _, a := range d.Albums {
		out.Albums = append(out.Albums, Album{ID: a.ID, MBID: a.MBID, Title: a.Title, Monitored: a.Monitored})
	}
	return out
}

// tagLabels returns the labels of tag ids ("#<id>" for an id the index has no label for).
func tagLabels(ids []int64, labels map[int64]string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if l, ok := labels[id]; ok {
			out = append(out, l)
		} else {
			out = append(out, fmt.Sprintf("#%d", id))
		}
	}
	return out
}

// scrubError removes secrets and the integration's address from an index error (S20: no
// integration URLs in a manifest).
func scrubError(msg, rawURL string) string {
	msg = logging.RedactSecrets(msg)
	if rawURL == "" {
		return msg
	}
	msg = strings.ReplaceAll(msg, rawURL, "<url>")
	if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		msg = strings.ReplaceAll(msg, u.Host, "<host>")
		if h := u.Hostname(); h != "" {
			msg = strings.ReplaceAll(msg, h, "<host>")
		}
	}
	return msg
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func sortedKeys[V any](m map[int64]V) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
