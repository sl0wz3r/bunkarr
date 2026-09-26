package tiers

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/mediaindex"
)

// Options wires the engine to the stores it reads.
type Options struct {
	DB           *db.DB
	Catalog      *catalog.Store
	Index        *mediaindex.Store
	Integrations *integrations.Store
	// Providers supply the Plex index, Tautulli, Seerr and Maintainerr facts (slice 9); a field
	// no provider supplies is unavailable and evaluates unknown.
	Providers []Provider
	// Destinations lists the destinations for the preview (the API wires destinations.Store).
	Destinations func(ctx context.Context) ([]DestinationRef, error)
	// Records returns a destination's live records for the preview (the API wires
	// syncer.Store, so tiers never imports syncer, design §14.2).
	Records func(ctx context.Context, destinationID int64) ([]RecordRef, error)
	// ConfigBackups returns the Plex DB and *arr backups aimed at a destination, with the size of
	// their last version (the preview's "always full" row).
	ConfigBackups func(ctx context.Context, destinationID int64) ([]ConfigBackup, error)
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	Log *slog.Logger
}

// DestinationRef is a destination as the preview needs it.
type DestinationRef struct {
	ID        int64
	Name      string
	Enabled   bool
	SourceIDs []int64
}

// RecordRef is a live destination record as the preview needs it.
type RecordRef struct {
	SourceID      int64
	SourceRelPath string
	Size          int64
	MtimeNs       int64
	// State is present, linked, link_recorded or missing.
	State string
}

// ConfigBackup is a Plex DB or *arr backup aimed at a destination; they are always full.
type ConfigBackup struct {
	Kind          string `json:"kind"`
	IntegrationID int64  `json:"integrationId"`
	Name          string `json:"name"`
	LastBytes     int64  `json:"lastBytes"`
}

// Engine combines the rules, the facts and the flags (design §8).
type Engine struct {
	o        Options
	store    *Store
	previews *previewCache
}

// New returns an engine.
func New(o Options) *Engine {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return &Engine{o: o, store: NewStore(o.DB, o.Now), previews: newPreviewCache(o.Now)}
}

// Store returns the rules and flags store.
func (e *Engine) Store() *Store { return e.store }

// Revision returns tiers.revision now.
func (e *Engine) Revision(ctx context.Context) (int64, error) { return e.store.Revision(ctx, nil) }

// SourceDecisions are the decisions of one source's live files at one destination.
type SourceDecisions struct {
	SourceID int64
	Revision int64
	// All is set when no rule applies at the destination: every file is full by the fallback.
	All bool
	// byFile holds the decisions (All: none).
	byFile map[int64]Decision
	// arrAdded maps a source path to the dateAdded of the *arr file there (the same-path
	// replacement check, §8.5).
	arrAdded map[string]time.Time
	// Stale are the rule values no fresh index knows (one job warning each, §8.5).
	Stale []Warning
	// Unknown are the caches whose facts are unknown now.
	Unknown []UnknownSource
}

// NewSourceDecisions returns decisions made elsewhere (tests, a caller that evaluated already):
// byFile by catalog file id (a missing file is full by the fallback), arrAdded by source path.
func NewSourceDecisions(sourceID, revision int64, byFile map[int64]Decision, arrAdded map[string]time.Time) *SourceDecisions {
	return &SourceDecisions{SourceID: sourceID, Revision: revision, byFile: byFile, arrAdded: arrAdded, All: len(byFile) == 0}
}

// Decide returns a file's decision; a file the decisions do not know is full by the fallback.
func (d *SourceDecisions) Decide(fileID int64) Decision {
	if d == nil {
		return FallbackDecision(0)
	}
	if dec, ok := d.byFile[fileID]; ok {
		return dec
	}
	return FallbackDecision(d.Revision)
}

// ArrDateAdded returns when the *arr added the file it knows at a source path.
func (d *SourceDecisions) ArrDateAdded(rel string) (time.Time, bool) {
	if d == nil {
		return time.Time{}, false
	}
	t, ok := d.arrAdded[rel]
	return t, ok
}

// Decisions decides the tiers of src's live files at destination destID, reading through q (nil:
// the read pool). A Snapshot q is read alone: its transaction and its configuration, never the
// read pool (see Snapshot). With no rule applying there every file is full, and no fact is loaded.
func (e *Engine) Decisions(ctx context.Context, q Queryer, destID int64, src catalog.Source) (*SourceDecisions, error) {
	if q == nil {
		q = e.o.DB.Reader()
	}
	rs, err := e.store.Rules(ctx, q)
	if err != nil {
		return nil, err
	}
	now := e.o.Now()
	ev := NewEvaluator(rs.Rules, rs.Revision, now)
	out := &SourceDecisions{SourceID: src.ID, Revision: rs.Revision}
	if !ev.Applies(destID) {
		out.All = true
		if out.arrAdded, err = e.arrAdded(ctx, q, src); err != nil {
			return nil, err
		}
		return out, nil
	}
	fc, err := e.newContext(ctx, q, now)
	if err != nil {
		return nil, err
	}
	applies := func(r Rule) bool { return r.Enabled && r.AppliesAt(destID) }
	facts, err := e.loadFacts(ctx, q, fc, src, nil, neededFields(rs.Rules, applies))
	if err != nil {
		return nil, err
	}
	out.byFile = decideFiles(ev, destID, facts)
	out.arrAdded = map[string]time.Time{}
	for _, f := range facts {
		if f.Arr.DateAdded != nil && !f.Arr.ByFolder && f.Follows == "" {
			out.arrAdded[f.RelPath] = *f.Arr.DateAdded
		}
	}
	stale, err := e.staleReferences(ctx, q, fc, rs.Rules, applies, false)
	if err != nil {
		return nil, err
	}
	out.Stale = stale
	out.Unknown = fc.unknown
	return out, nil
}

// DecisionsByID is Decisions for a source given by id (the manifest builder's Tiers, which passes
// a Snapshot: the source is then looked up in its list).
func (e *Engine) DecisionsByID(ctx context.Context, q Queryer, destID, sourceID int64) (*SourceDecisions, error) {
	src, err := sourceOf(ctx, e.o.Catalog, q, sourceID)
	if err != nil {
		return nil, fmt.Errorf("tiers: %w", err)
	}
	return e.Decisions(ctx, q, destID, src)
}

// arrAdded reads the *arr files under a source's path (the same-path replacement check).
func (e *Engine) arrAdded(ctx context.Context, q Queryer, src catalog.Source) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	err := e.o.Index.FilesUnder(ctx, q, src.Path, func(f mediaindex.File) error {
		if f.DateAdded == nil || (src.ArrIntegrationID != nil && *src.ArrIntegrationID != f.IntegrationID) {
			return nil
		}
		if rel, ok := strings.CutPrefix(f.LocalPath, src.Path+"/"); ok {
			if t, seen := out[rel]; !seen || f.DateAdded.After(t) {
				out[rel] = *f.DateAdded
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("tiers: %w", err)
	}
	return out, nil
}

// decideFiles decides every file: its own decision; every name of a hardlink group takes the
// group's most protective decision (§8.1 step 5); sidecars take their media file's (unless their
// own flag makes them full); then groups once more, for a sidecar that is itself hardlinked.
func decideFiles(ev *Evaluator, destID int64, facts []*Facts) map[int64]Decision {
	out := make(map[int64]Decision, len(facts))
	byRel := make(map[string]*Facts, len(facts))
	groups := map[string][]*Facts{}
	for _, f := range facts {
		byRel[f.RelPath] = f
		if f.Follows == "" || len(f.Flags) > 0 {
			out[f.FileID] = ev.Decide(destID, f)
		}
		if f.Group != "" {
			groups[f.Group] = append(groups[f.Group], f)
		}
	}
	unify := func() {
		for _, members := range groups {
			var best *Facts
			for _, m := range members {
				if d, ok := out[m.FileID]; ok && (best == nil || d.Tier.Protection() > out[best.FileID].Tier.Protection()) {
					best = m
				}
			}
			if best == nil {
				continue
			}
			for _, m := range members {
				if d, ok := out[m.FileID]; ok && d.Tier.Protection() < out[best.FileID].Tier.Protection() {
					d = out[best.FileID]
					d.Follows = best.RelPath
					out[m.FileID] = d
				}
			}
		}
	}
	unify()
	for _, f := range facts {
		if f.Follows == "" || len(f.Flags) > 0 {
			continue
		}
		d := FallbackDecision(ev.Revision())
		if m := byRel[f.Follows]; m != nil {
			d = out[m.FileID]
		}
		d.Follows = f.Follows
		out[f.FileID] = d
	}
	unify()
	return out
}

// FileTiers is a file's facts and its decision at each destination linked to its source (the
// Library item view, GET /catalog/files/{id}).
type FileTiers struct {
	Facts     *Facts             `json:"facts"`
	Source    catalog.Source     `json:"-"`
	Decisions map[int64]Decision `json:"decisions"`
	Unknown   []UnknownSource    `json:"unknown"`
}

// FactsFor returns a live catalog file's facts and decisions at the given destinations (§8.3).
// It loads only the file's folder and the folders of its hardlink group. ErrNotFound when the
// file is not live.
func (e *Engine) FactsFor(ctx context.Context, fileID int64, destIDs []int64) (FileTiers, error) {
	q := e.o.DB.Reader()
	row, srcID, err := fileRow(ctx, q, fileID)
	if errors.Is(err, sql.ErrNoRows) {
		return FileTiers{}, fmt.Errorf("catalog file %d: %w", fileID, ErrNotFound)
	}
	if err != nil {
		return FileTiers{}, fmt.Errorf("tiers: %w", err)
	}
	src, err := e.o.Catalog.Get(ctx, srcID)
	if err != nil {
		return FileTiers{}, fmt.Errorf("tiers: %w", err)
	}
	dirs := []string{path.Dir(row.rel)}
	if row.group != "" {
		members, err := groupRows(ctx, q, srcID, row.group)
		if err != nil {
			return FileTiers{}, err
		}
		for _, m := range members {
			if d := path.Dir(m.rel); !slices.Contains(dirs, d) {
				dirs = append(dirs, d)
			}
		}
	}
	rs, err := e.store.Rules(ctx, q)
	if err != nil {
		return FileTiers{}, err
	}
	now := e.o.Now()
	fc, err := e.newContext(ctx, q, now)
	if err != nil {
		return FileTiers{}, err
	}
	facts, err := e.loadFacts(ctx, q, fc, src, dirs, nil)
	if err != nil {
		return FileTiers{}, err
	}
	out := FileTiers{Source: src, Decisions: map[int64]Decision{}, Unknown: fc.unknown}
	for _, f := range facts {
		if f.FileID == fileID {
			out.Facts = f
		}
	}
	if out.Facts == nil {
		return FileTiers{}, fmt.Errorf("catalog file %d: %w", fileID, ErrNotFound)
	}
	ev := NewEvaluator(rs.Rules, rs.Revision, now)
	for _, id := range destIDs {
		out.Decisions[id] = decideFiles(ev, id, facts)[fileID]
	}
	return out, nil
}

// SaveRules replaces the rule set (Store.Replace) and returns it with the warnings of its values
// that no fresh index knows (§8.7), placed by their index in the request.
func (e *Engine) SaveRules(ctx context.Context, base int64, in []RuleInput) (RuleSet, []Warning, error) {
	rs, err := e.store.Replace(ctx, base, in)
	if err != nil {
		return RuleSet{}, nil, err
	}
	warnings, err := e.Warnings(ctx, rs.Rules)
	if err != nil {
		return rs, nil, err
	}
	return rs, warnings, nil
}

// Warnings returns the stale references of rules (placed by their index in rules).
func (e *Engine) Warnings(ctx context.Context, rules []Rule) ([]Warning, error) {
	q := e.o.DB.Reader()
	fc, err := e.newContext(ctx, q, e.o.Now())
	if err != nil {
		return nil, err
	}
	return e.staleReferences(ctx, q, fc, rules, func(Rule) bool { return true }, true)
}

// staleReferences returns a warning for every condition value of the rules that pass keep and
// that no fresh index knows: a tag, quality profile or root folder no fresh *arr has, a source
// that does not exist, a Plex section no source or index names, a Seerr user (§8.7). live allows
// the providers to ask their application (the rule editor); a sync or a manifest build never
// waits on the network, and the providers' reads are shared by the rules' conditions.
func (e *Engine) staleReferences(ctx context.Context, q Queryer, fc *factContext, rules []Rule, keep func(Rule) bool, live bool) ([]Warning, error) {
	ctx = withKnownMemo(ctx, live)
	out := []Warning{}
	for i, r := range rules {
		if !keep(r) {
			continue
		}
		for j, c := range r.Conditions {
			cc, err := compileCondition(c, i, j)
			if err != nil {
				msg := err.Error()
				var ve *ValidationError
				if errors.As(err, &ve) {
					msg = ve.Msg
				}
				out = append(out, Warning{RuleIndex: i, ConditionIndex: j, Message: msg})
				continue
			}
			known, what, err := e.known(ctx, q, fc, cc)
			if err != nil {
				return nil, err
			}
			if !known {
				out = append(out, Warning{RuleIndex: i, ConditionIndex: j, Message: fmt.Sprintf("no fresh index knows the %s %s", what, string(cc.Value))})
			}
		}
	}
	return out, nil
}

// known reports whether a fresh index knows a condition's value (true for fields without
// references).
func (e *Engine) known(ctx context.Context, q Queryer, fc *factContext, c cond) (bool, string, error) {
	freshMeta := func(fn func(mediaindex.Meta) bool) bool {
		for _, id := range fc.arrInts {
			if fc.fresh[id].Fresh && fn(fc.meta[id]) {
				return true
			}
		}
		return false
	}
	switch c.Field {
	case FieldArrTag:
		return freshMeta(func(m mediaindex.Meta) bool {
			return slices.ContainsFunc(m.Tags, func(t mediaindex.TagLabel) bool { return strings.EqualFold(t.Label, c.s) })
		}), "tag", nil
	case FieldArrQualityProfile:
		return freshMeta(func(m mediaindex.Meta) bool {
			return slices.ContainsFunc(m.QualityProfiles, func(p mediaindex.Named) bool { return strings.EqualFold(p.Name, c.s) })
		}), "quality profile", nil
	case FieldArrRootFolder:
		return freshMeta(func(m mediaindex.Meta) bool {
			return slices.ContainsFunc(m.RootFolders, func(r mediaindex.RootFolder) bool { return trimSlash(r.Path) == c.s })
		}), "root folder", nil
	case FieldSource:
		_, ok := fc.sources[c.n]
		return ok, "source", nil
	case FieldPlexSection:
		for _, s := range fc.sources {
			if s.PlexIntegrationID != nil && fmt.Sprintf("%d:%s", *s.PlexIntegrationID, s.PlexSectionID) == c.s {
				return true, "Plex library", nil
			}
		}
		for _, p := range e.providersOf(c.Field) {
			if ok, err := p.Known(ctx, q, c.Field, c.s, nil, fc.now); err != nil || ok {
				return ok, "Plex library", err
			}
		}
		return false, "Plex library", nil
	case FieldSeerrRequestedBy:
		ps := e.providersOf(c.Field)
		if len(ps) == 0 {
			return true, "", nil // nothing to check against: the field is unavailable
		}
		for _, p := range ps {
			if ok, err := p.Known(ctx, q, c.Field, "", c.ints, fc.now); err != nil || ok {
				return ok, "Seerr users", err
			}
		}
		return false, "Seerr users", nil
	}
	return true, "", nil
}

// providersOf returns the providers of a field.
func (e *Engine) providersOf(field string) []Provider {
	var out []Provider
	for _, p := range e.o.Providers {
		if slices.Contains(p.Fields(), field) {
			out = append(out, p)
		}
	}
	return out
}

// Flags returns every flag with whether it resolves now and, when not, why (GET /tiers/flags).
// A resolved *arr flag's last folder is refreshed.
func (e *Engine) Flags(ctx context.Context) ([]Flag, error) {
	q := e.o.DB.Reader()
	fc, err := e.newContext(ctx, q, e.o.Now())
	if err != nil {
		return nil, err
	}
	out := make([]Flag, 0, len(fc.flags))
	for _, rf := range fc.flags {
		f := rf.flag
		switch {
		case f.Kind == FlagKindArr && rf.resolved:
			f.Resolved = true
			if loc := rf.folder; loc != nil && (f.LastSourceID == nil || *f.LastSourceID != loc.SourceID || f.LastRelPath == nil || *f.LastRelPath != loc.Rel) {
				if err := e.store.setLastFolder(ctx, f.ID, loc.SourceID, loc.Rel); err != nil {
					return nil, fmt.Errorf("tiers: %w", err)
				}
				f.LastSourceID, f.LastRelPath = &loc.SourceID, &loc.Rel
			}
		case rf.hasPath:
			live, err := anyLiveUnder(ctx, q, rf.sourceID, rf.rel)
			if err != nil {
				return nil, err
			}
			if rf.rel == "" {
				live = true
			}
			f.Resolved = live && f.Kind == FlagKindPath
			f.Reason = rf.reason
			if !live {
				if f.Reason != "" {
					f.Reason += "; "
				}
				f.Reason += "folder not found"
			}
		default:
			f.Reason = rf.reason
		}
		out = append(out, f)
	}
	return out, nil
}

// AddFlag stores a new flag (POST /tiers/flags). An *arr flag copies the item's external ids
// from the index (a *ValidationError when it has none) and records its located folder.
func (e *Engine) AddFlag(ctx context.Context, in FlagInput) (Flag, error) {
	in, err := checkFlagInput(in)
	if err != nil {
		return Flag{}, err
	}
	f := Flag{Flag: in.Flag, Note: in.Note}
	t := in.Target
	if t.RelPath != nil {
		f.Kind, f.SourceID, f.RelPath = FlagKindPath, &t.SourceID, t.RelPath
		return e.store.insertFlag(ctx, f)
	}
	it, err := e.o.Integrations.Get(ctx, t.IntegrationID)
	if errors.Is(err, integrations.ErrNotFound) {
		return Flag{}, invalid(-1, -1, "integration %d does not exist", t.IntegrationID)
	}
	if err != nil {
		return Flag{}, err
	}
	if !it.Type.IsArr() || mediaindex.KindOf(it.Type) != t.Kind {
		return Flag{}, invalid(-1, -1, "integration %d (%s) has no %s items", it.ID, it.Type.AppName(), t.Kind)
	}
	item, err := e.o.Index.Item(ctx, nil, it.ID, t.Kind, t.ArrID)
	if errors.Is(err, mediaindex.ErrItemNotFound) {
		return Flag{}, invalid(-1, -1, "%s has no %s %d in Bunkarr's index (refresh it first)", it.Name, t.Kind, t.ArrID)
	}
	if err != nil {
		return Flag{}, err
	}
	if !hasFlagID(t.Kind, item.ExternalIDs) {
		return Flag{}, invalid(-1, -1, "%q has no external id (TMDB, TVDB, IMDb or MusicBrainz) to follow; flag its folder instead", item.Title)
	}
	id := it.ID
	f.Kind, f.IntegrationID, f.ArrKind, f.ArrID, f.ExternalIDs = FlagKindArr, &id, t.Kind, t.ArrID, item.ExternalIDs
	if s, err := it.ArrSettings(); err == nil {
		if local, ok := s.MapPath(item.Path); ok {
			if locs, err := e.o.Catalog.Locate(ctx, local); err == nil && len(locs) > 0 && locs[0].Rel != "" {
				f.LastSourceID, f.LastRelPath = &locs[0].SourceID, &locs[0].Rel
			}
		}
	}
	return e.store.insertFlag(ctx, f)
}

// Covered reports whether an irreplaceable flag covers a source path (a path flag, an *arr flag's
// item folder, or its last folder), and which.
func (e *Engine) Covered(ctx context.Context, sourceID int64, rel string) (int64, bool, error) {
	fn, err := e.Coverage(ctx)
	if err != nil {
		return 0, false, err
	}
	id, ok := fn(sourceID, rel)
	return id, ok, nil
}

// Coverage returns a function reporting whether an irreplaceable flag covers a source path, and
// which, read once (the retention job's hold, §8.7; see Covered).
func (e *Engine) Coverage(ctx context.Context) (func(sourceID int64, rel string) (int64, bool), error) {
	flags, err := e.store.Flags(ctx, nil)
	if err != nil {
		return nil, err
	}
	if len(flags) == 0 {
		return func(int64, string) (int64, bool) { return 0, false }, nil
	}
	fc, err := e.newContext(ctx, e.o.DB.Reader(), e.o.Now())
	if err != nil {
		return nil, err
	}
	return func(sourceID int64, rel string) (int64, bool) {
		srcPath := ""
		if s, ok := fc.sources[sourceID]; ok {
			srcPath = s.Path
		}
		for _, rf := range fc.flags {
			if rf.coversPath(sourceID, rel, srcPath) {
				return rf.flag.ID, true
			}
		}
		return 0, false
	}, nil
}

// FlagsAfterSync runs after a sync of source sourceID executed: folder flags follow the folder
// renames among its moves, and resolved *arr flags' last folders are refreshed (§8.7).
func (e *Engine) FlagsAfterSync(ctx context.Context, sourceID int64, moves []Move) error {
	if _, err := e.store.FollowFolderMoves(ctx, sourceID, moves); err != nil {
		return err
	}
	flags, err := e.store.Flags(ctx, nil)
	if err != nil {
		return err
	}
	hasArr := slices.ContainsFunc(flags, func(f Flag) bool { return f.Kind == FlagKindArr })
	if !hasArr {
		return nil
	}
	_, err = e.Flags(ctx)
	return err
}

// MoveFlagsTx is the package function MoveFlagsTx with the engine's clock (the syncer's hook).
func (e *Engine) MoveFlagsTx(ctx context.Context, tx *sql.Tx, sourceID int64, from, to string) error {
	return MoveFlagsTx(ctx, tx, sourceID, from, to, e.o.Now())
}

// Field is a condition field as GET /tiers/fields returns it.
type Field struct {
	FieldSpec
	Available   bool         `json:"available"`
	Reason      string       `json:"reason,omitempty"`
	Suggestions []Suggestion `json:"suggestions"`
}

// Fields returns the condition fields with their availability and the editor's suggestions
// (tags, profiles, root folders, sections, sources, genres; the providers' own).
func (e *Engine) Fields(ctx context.Context) ([]Field, error) {
	q := e.o.DB.Reader()
	fc, err := e.newContext(ctx, q, e.o.Now())
	if err != nil {
		return nil, err
	}
	out := make([]Field, 0, len(fieldSpecs))
	for _, spec := range fieldSpecs {
		f := Field{FieldSpec: spec, Available: true, Suggestions: []Suggestion{}}
		ps := e.providersOf(spec.Field)
		if spec.provided && len(ps) == 0 {
			f.Available = false
			f.Reason = fmt.Sprintf("Bunkarr does not read %s yet", appOfSource(spec.Source))
		}
		switch spec.Field {
		case FieldArrTag:
			f.Suggestions = fc.arrSuggestions(func(m mediaindex.Meta) []string {
				var l []string
				for _, t := range m.Tags {
					l = append(l, t.Label)
				}
				return l
			}, true)
		case FieldArrQualityProfile:
			f.Suggestions = fc.arrSuggestions(func(m mediaindex.Meta) []string {
				var l []string
				for _, p := range m.QualityProfiles {
					l = append(l, p.Name)
				}
				return l
			}, true)
		case FieldArrRootFolder:
			f.Suggestions = fc.arrSuggestions(func(m mediaindex.Meta) []string {
				var l []string
				for _, r := range m.RootFolders {
					l = append(l, trimSlash(r.Path))
				}
				return l
			}, false)
		case FieldMediaGenre:
			f.Suggestions = fc.genreSuggestions()
		case FieldSource:
			for _, s := range sortedSources(fc.sources) {
				f.Suggestions = append(f.Suggestions, Suggestion{Value: s.ID, Label: s.Name})
			}
		case FieldPlexSection:
			for _, s := range sortedSources(fc.sources) {
				if s.PlexIntegrationID != nil && s.PlexSectionID != "" {
					f.Suggestions = append(f.Suggestions, Suggestion{Value: fmt.Sprintf("%d:%s", *s.PlexIntegrationID, s.PlexSectionID),
						Label: fmt.Sprintf("Plex library %s (source %s)", s.PlexSectionID, s.Name)})
				}
			}
		}
		for _, p := range ps {
			more, err := p.Suggestions(ctx, spec.Field)
			if err != nil {
				e.o.Log.Warn("Could not read field suggestions", "field", spec.Field, "error", err)
				continue
			}
			f.Suggestions = append(f.Suggestions, more...)
		}
		out = append(out, f)
	}
	return out, nil
}

func appOfSource(kind string) string {
	switch kind {
	case SourceTautulli:
		return "Tautulli"
	case SourceSeerr:
		return "Seerr"
	case SourceMaintainerr:
		return "Maintainerr"
	case SourcePlex:
		return "the Plex library index"
	}
	return kind
}

func sortedSources(m map[int64]catalog.Source) []catalog.Source {
	out := make([]catalog.Source, 0, len(m))
	for _, s := range m {
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b catalog.Source) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// arrSuggestions lists values of every *arr's metadata, once each (case-insensitively when fold),
// labelled with the applications that have them.
func (fc *factContext) arrSuggestions(values func(mediaindex.Meta) []string, fold bool) []Suggestion {
	type entry struct {
		value string
		apps  []string
	}
	var order []string
	byKey := map[string]*entry{}
	for _, id := range fc.arrInts {
		name := fc.appName(id)
		for _, v := range values(fc.meta[id]) {
			k := v
			if fold {
				k = strings.ToLower(v)
			}
			e, ok := byKey[k]
			if !ok {
				e = &entry{value: v}
				byKey[k] = e
				order = append(order, k)
			}
			if !slices.Contains(e.apps, name) {
				e.apps = append(e.apps, name)
			}
		}
	}
	out := make([]Suggestion, 0, len(order))
	for _, k := range order {
		e := byKey[k]
		out = append(out, Suggestion{Value: e.value, Label: fmt.Sprintf("%s (%s)", e.value, strings.Join(e.apps, ", "))})
	}
	return out
}

// genreSuggestions lists the genres of the indexed items (at most 500), by name.
func (fc *factContext) genreSuggestions() []Suggestion {
	seen := map[string]string{}
	for _, it := range fc.items {
		for _, g := range it.facts.Genres {
			if _, ok := seen[strings.ToLower(g)]; !ok {
				seen[strings.ToLower(g)] = g
			}
		}
	}
	names := make([]string, 0, len(seen))
	for _, g := range seen {
		names = append(names, g)
	}
	slices.Sort(names)
	if len(names) > 500 {
		names = names[:500]
	}
	out := make([]Suggestion, 0, len(names))
	for _, g := range names {
		out = append(out, Suggestion{Value: g, Label: g})
	}
	return out
}
