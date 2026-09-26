package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/mediaindex"
	"github.com/sl0wz3r/bunkarr/internal/snapshots"
	"github.com/sl0wz3r/bunkarr/internal/tiers"
)

// Tiers (design §8, §13): the rule set, presets, fields, preview and flags. A sync's release
// params are checked by syncDestination through checkRelease.

func (s *Server) tierRoutes(r chi.Router) {
	r.Get("/tiers/rules", s.getTierRules)
	r.Put("/tiers/rules", s.putTierRules)
	r.Get("/tiers/presets", s.getTierPresets)
	r.Get("/tiers/fields", s.getTierFields)
	r.Post("/tiers/preview", s.postTierPreview)
	r.Get("/tiers/preview/{id}/items", s.getTierPreviewItems)
	r.Get("/tiers/flags", s.getTierFlags)
	r.Post("/tiers/flags", s.postTierFlag)
	r.Delete("/tiers/flags/{id}", s.deleteTierFlag)
	r.Get("/catalog/files/{id}", s.getCatalogFile)
}

// newTierEngine builds the tier engine over the app's stores. The preview reads the destinations,
// their live records (syncer.Store, so tiers never imports syncer) and the config backups aimed
// at each destination.
func (a *App) newTierEngine(o AppOptions) *tiers.Engine {
	idx := mediaindex.NewStore(o.DB, a.Catalog)
	return tiers.New(tiers.Options{
		DB:           o.DB,
		Catalog:      a.Catalog,
		Index:        idx,
		Providers:    []tiers.Provider{a.indexProvider(idx)},
		Integrations: a.Integrations,
		Log:          a.log.With("component", "tiers"),
		Destinations: func(ctx context.Context) ([]tiers.DestinationRef, error) {
			list, err := a.Destinations.List(ctx)
			if err != nil {
				return nil, err
			}
			out := make([]tiers.DestinationRef, 0, len(list))
			for _, d := range list {
				out = append(out, tiers.DestinationRef{ID: d.ID, Name: d.Name, Enabled: d.Enabled, SourceIDs: d.SourceIDs})
			}
			return out, nil
		},
		Records: func(ctx context.Context, destID int64) ([]tiers.RecordRef, error) {
			recs, err := a.Files.List(ctx, destID)
			if err != nil {
				return nil, err
			}
			out := make([]tiers.RecordRef, 0, len(recs))
			for _, r := range recs {
				if r.State.Live() {
					out = append(out, tiers.RecordRef{SourceID: r.SourceID, SourceRelPath: r.SourceRelPath, Size: r.Size, MtimeNs: r.MtimeNs,
						State: string(r.State)})
				}
			}
			return out, nil
		},
		ConfigBackups: a.configBackups,
	})
}

// configBackups lists the Plex DB and *arr backups aimed at a destination, with the size of their
// newest version (always full: they are not tiered).
func (a *App) configBackups(ctx context.Context, destID int64) ([]tiers.ConfigBackup, error) {
	list, err := a.Integrations.List(ctx)
	if err != nil {
		return nil, err
	}
	out := []tiers.ConfigBackup{}
	for _, it := range list {
		var kind snapshots.Kind
		switch {
		case it.Type == integrations.TypePlex:
			ps, err := it.PlexSettings()
			if err != nil || !ps.Backup.Enabled || ps.Backup.DestinationID != destID {
				continue
			}
			kind = snapshots.KindPlexDB
		case it.Type.IsArr():
			as, err := it.ArrSettings()
			if err != nil || !as.Backup.Enabled || as.Backup.DestinationID != destID {
				continue
			}
			kind = snapshots.KindArr
		default:
			continue
		}
		cb := tiers.ConfigBackup{Kind: string(kind), IntegrationID: it.ID, Name: it.Name}
		if a.Snapshots != nil {
			snaps, err := a.Snapshots.ListFor(ctx, kind, destID, it.ID)
			if err != nil {
				return nil, err
			}
			if len(snaps) > 0 { // newest first
				cb.LastBytes = snaps[0].Size
			}
		}
		out = append(out, cb)
	}
	return out, nil
}

// tierError maps the tier engine's errors to statuses.
func tierError(err error) error {
	var ve *tiers.ValidationError
	switch {
	case errors.As(err, &ve):
		return errorf(http.StatusBadRequest, "%s", ve.Error())
	case errors.Is(err, tiers.ErrRevisionMismatch):
		return errorf(http.StatusConflict, "%s", tiers.ErrRevisionMismatch.Error())
	case errors.Is(err, tiers.ErrNotFound):
		return errorf(http.StatusNotFound, "%s", err.Error())
	case errors.Is(err, tiers.ErrConflict):
		return errorf(http.StatusConflict, "%s", err.Error())
	}
	return err
}

func (s *Server) getTierRules(w http.ResponseWriter, r *http.Request) {
	rs, err := s.app.Tiers.Store().Rules(r.Context(), nil)
	if err != nil {
		s.fail(w, r, "read tier rules", err)
		return
	}
	writeJSON(w, http.StatusOK, rs)
}

// TierRulesSaved is PUT /tiers/rules: the saved set and the stale-reference warnings.
type TierRulesSaved struct {
	tiers.RuleSet
	Warnings []tiers.Warning `json:"warnings"`
}

func (s *Server) putTierRules(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Revision *int64            `json:"revision"`
		Rules    []tiers.RuleInput `json:"rules"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		s.fail(w, r, "save tier rules", err)
		return
	}
	if body.Revision == nil {
		s.fail(w, r, "save tier rules", errorf(http.StatusBadRequest, "revision is required: send the revision the rules were loaded at"))
		return
	}
	if body.Rules == nil {
		body.Rules = []tiers.RuleInput{}
	}
	rs, warnings, err := s.app.Tiers.SaveRules(r.Context(), *body.Revision, body.Rules)
	if err != nil {
		s.fail(w, r, "save tier rules", tierError(err))
		return
	}
	if warnings == nil {
		warnings = []tiers.Warning{}
	}
	s.log.Info("Tier rules saved", "revision", rs.Revision, "rules", len(rs.Rules), "warnings", len(warnings))
	writeJSON(w, http.StatusOK, TierRulesSaved{RuleSet: rs, Warnings: warnings})
}

func (s *Server) getTierPresets(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, tiers.Presets())
}

func (s *Server) getTierFields(w http.ResponseWriter, r *http.Request) {
	fields, err := s.app.Tiers.Fields(r.Context())
	if err != nil {
		s.fail(w, r, "read tier fields", err)
		return
	}
	writeJSON(w, http.StatusOK, fields)
}

func (s *Server) postTierPreview(w http.ResponseWriter, r *http.Request) {
	var body tiers.PreviewRequest
	if err := decodeOptionalBody(w, r, &body); err != nil {
		s.fail(w, r, "preview tiers", err)
		return
	}
	p, err := s.app.Tiers.Preview(r.Context(), body)
	if err != nil {
		s.fail(w, r, "preview tiers", tierError(err))
		return
	}
	writeJSON(w, http.StatusOK, p)
}

var previewIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (s *Server) getTierPreviewItems(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !previewIDPattern.MatchString(id) {
		s.fail(w, r, "read preview items", errorf(http.StatusNotFound, "preview not found"))
		return
	}
	q := r.URL.Query()
	page, size, err := paging(q)
	if err != nil {
		s.fail(w, r, "read preview items", err)
		return
	}
	iq := tiers.ItemQuery{Page: page, PageSize: size, Search: q.Get("search"), State: q.Get("state"), Tier: tiers.Tier(q.Get("tier"))}
	if iq.Tier != "" && !iq.Tier.Valid() {
		s.fail(w, r, "read preview items", errorf(http.StatusBadRequest, "tier must be full, manifest or skip"))
		return
	}
	switch iq.State {
	case "", tiers.StateStored, tiers.StateToCopy, tiers.StateKept, tiers.StateNotCopied:
	default:
		s.fail(w, r, "read preview items", errorf(http.StatusBadRequest, "state must be stored, to-copy, kept or not-copied"))
		return
	}
	if v := strings.TrimSpace(q.Get("destinationId")); v != "" {
		if iq.DestinationID, err = strconv.ParseInt(v, 10, 64); err != nil || iq.DestinationID < 1 {
			s.fail(w, r, "read preview items", errorf(http.StatusBadRequest, "invalid destinationId %q", v))
			return
		}
	}
	if v := strings.TrimSpace(q.Get("ruleId")); v != "" {
		if iq.RuleID, err = strconv.ParseInt(v, 10, 64); err != nil {
			s.fail(w, r, "read preview items", errorf(http.StatusBadRequest, "invalid ruleId %q", v))
			return
		}
		iq.HasRuleID = true
	}
	out, err := s.app.Tiers.PreviewItems(id, iq)
	if err != nil {
		s.fail(w, r, "read preview items", tierError(err))
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getTierFlags(w http.ResponseWriter, r *http.Request) {
	flags, err := s.app.Tiers.Flags(r.Context())
	if err != nil {
		s.fail(w, r, "read flags", err)
		return
	}
	writeJSON(w, http.StatusOK, flags)
}

func (s *Server) postTierFlag(w http.ResponseWriter, r *http.Request) {
	var body tiers.FlagInput
	if err := decodeBody(w, r, &body); err != nil {
		s.fail(w, r, "flag", err)
		return
	}
	f, err := s.app.Tiers.AddFlag(r.Context(), body)
	if err != nil {
		s.fail(w, r, "flag", tierError(err))
		return
	}
	s.log.Info("Flag added", "id", f.ID, "flag", f.Flag, "kind", f.Kind)
	writeJSON(w, http.StatusCreated, f)
}

func (s *Server) deleteTierFlag(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.fail(w, r, "remove flag", err)
		return
	}
	if err := s.app.Tiers.Store().DeleteFlag(r.Context(), id); err != nil {
		s.fail(w, r, "remove flag", tierError(err))
		return
	}
	s.log.Info("Flag removed", "id", id)
	w.WriteHeader(http.StatusNoContent)
}

// releaseParams are the release fields of POST /destinations/{id}/sync.
type releaseParams struct {
	ReleaseDemoted  bool  `json:"releaseDemoted"`
	ReleaseOf       int64 `json:"releaseOf"`
	ReleaseRevision int64 `json:"releaseRevision"`
}

// errRulesChanged is the 409 of a release whose preview no longer matches (design §12.1).
const errRulesChanged = "rules changed since the preview; run the release preview again"

// checkRelease refuses a real release sync (409) unless releaseOf names a finished dry-run sync of
// the same destination with releaseDemoted, and its stats.tierRevision, releaseRevision and the
// current tiers.revision are equal (design §8.5, §12.1). A dry run needs neither field.
func (s *Server) checkRelease(ctx context.Context, destID int64, dryRun bool, p releaseParams) error {
	if !p.ReleaseDemoted {
		if p.ReleaseOf != 0 || p.ReleaseRevision != 0 {
			return errorf(http.StatusBadRequest, "releaseOf and releaseRevision need releaseDemoted")
		}
		return nil
	}
	if dryRun {
		if p.ReleaseOf != 0 || p.ReleaseRevision != 0 {
			return errorf(http.StatusBadRequest, "a release preview (dry run) takes no releaseOf or releaseRevision")
		}
		return nil
	}
	if p.ReleaseOf < 1 || p.ReleaseRevision < 1 {
		return errorf(http.StatusBadRequest, "a release needs releaseOf (its preview, a dry run) and releaseRevision")
	}
	prev, err := s.app.Jobs.Get(ctx, p.ReleaseOf)
	if errors.Is(err, jobqueue.ErrNotFound) {
		return errorf(http.StatusConflict, "%s (job %d does not exist)", errRulesChanged, p.ReleaseOf)
	}
	if err != nil {
		return err
	}
	var stats struct {
		TierRevision int64 `json:"tierRevision"`
	}
	if len(prev.Stats) > 0 {
		_ = json.Unmarshal(prev.Stats, &stats)
	}
	rev, err := s.app.Tiers.Revision(ctx)
	if err != nil {
		return err
	}
	switch {
	case prev.Type != jobs.TypeSync || !prev.DryRun || !prev.Params.ReleaseDemoted || prev.Params.DestinationID != destID:
		return errorf(http.StatusConflict, "%s (job %d is not a release preview of this destination)", errRulesChanged, p.ReleaseOf)
	case prev.Status != jobs.StatusCompleted && prev.Status != jobs.StatusCompletedWithWarnings:
		return errorf(http.StatusConflict, "%s (job %d did not finish)", errRulesChanged, p.ReleaseOf)
	case stats.TierRevision != p.ReleaseRevision || p.ReleaseRevision != rev:
		return errorf(http.StatusConflict, "%s (the preview evaluated revision %d, the rules are at %d)", errRulesChanged, stats.TierRevision, rev)
	}
	return nil
}

// FileDetail is GET /catalog/files/{id} (the Library item view, design §13): a live catalog
// file, its facts with what is unknown and why, and its tier at every destination linked to its
// source with that destination's record.
type FileDetail struct {
	File   FileRef          `json:"file"`
	Source SourceRef        `json:"source"`
	Facts  FileFacts        `json:"facts"`
	Tiers  []FileTierAtDest `json:"tiers"`
}

// FileRef is the catalog file of a FileDetail.
type FileRef struct {
	ID            int64     `json:"id"`
	RelPath       string    `json:"relPath"`
	Size          int64     `json:"size"`
	Mtime         time.Time `json:"mtime"`
	HardlinkGroup *string   `json:"hardlinkGroup"`
}

// SourceRef names a source.
type SourceRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// FileFacts are a file's tier facts; a nil part is not known (see Unknown).
type FileFacts struct {
	Arr         tiers.ArrFacts          `json:"arr"`
	Plex        *tiers.PlexFacts        `json:"plex"`
	Watch       *tiers.WatchFacts       `json:"watch"`
	Requests    *tiers.RequestFacts     `json:"requests"`
	Maintainerr *tiers.MaintainerrFacts `json:"maintainerr"`
	Flags       []int64                 `json:"flags"`
	Follows     string                  `json:"follows,omitempty"`
	Unknown     []FactUnknown           `json:"unknown"`
}

// FactUnknown says which facts are unknown and why.
type FactUnknown struct {
	Source string `json:"source"`
	Reason string `json:"reason"`
}

// FileTierAtDest is a file's tier at one destination, with the destination's record of it.
type FileTierAtDest struct {
	DestinationID   int64          `json:"destinationId"`
	DestinationName string         `json:"destinationName"`
	Tier            tiers.Tier     `json:"tier"`
	RuleID          int64          `json:"ruleId"`
	RuleName        string         `json:"ruleName"`
	Reasons         []tiers.Reason `json:"reasons"`
	Unknown         []tiers.Reason `json:"unknown"`
	UnknownPromoted bool           `json:"unknownPromoted"`
	Follows         string         `json:"follows,omitempty"`
	Record          *FileRecord    `json:"record"`
}

// FileRecord is a destination's live record of a file.
type FileRecord struct {
	State      string     `json:"state"`
	RelPath    string     `json:"relPath"`
	Size       int64      `json:"size"`
	CopiedAt   *time.Time `json:"copiedAt"`
	VerifiedAt *time.Time `json:"verifiedAt"`
}

func (s *Server) getCatalogFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := pathID(r)
	if err != nil {
		s.fail(w, r, "read catalog file", err)
		return
	}
	dests, err := s.app.Destinations.List(ctx)
	if err != nil {
		s.fail(w, r, "read catalog file", err)
		return
	}
	var destIDs []int64
	names := map[int64]string{}
	for _, d := range dests {
		names[d.ID] = d.Name
		destIDs = append(destIDs, d.ID)
	}
	ft, err := s.app.Tiers.FactsFor(ctx, id, destIDs)
	if err != nil {
		s.fail(w, r, "read catalog file", tierError(err))
		return
	}
	its, err := s.app.Integrations.List(ctx)
	if err != nil {
		s.fail(w, r, "read catalog file", err)
		return
	}
	setUp := map[integrations.Type]bool{}
	for _, it := range its {
		setUp[it.Type] = true
	}
	f := ft.Facts
	out := FileDetail{File: FileRef{ID: f.FileID, RelPath: f.RelPath, Size: f.Size, Mtime: time.Unix(0, f.MtimeNs).UTC()},
		Source: SourceRef{ID: ft.Source.ID, Name: ft.Source.Name},
		Facts: FileFacts{Arr: f.Arr, Plex: f.Plex, Watch: f.Watch, Requests: f.Requests, Maintainerr: f.Maintainerr, Flags: f.Flags,
			Follows: f.Follows, Unknown: factUnknowns(f, setUp)}, Tiers: []FileTierAtDest{}}
	if f.Group != "" {
		g := f.Group
		out.File.HardlinkGroup = &g
	}
	for _, d := range dests {
		if !slices.Contains(d.SourceIDs, ft.Source.ID) {
			continue
		}
		dec := ft.Decisions[d.ID]
		t := FileTierAtDest{DestinationID: d.ID, DestinationName: names[d.ID], Tier: dec.Tier, RuleID: dec.RuleID, RuleName: dec.RuleName,
			Reasons: dec.Reasons, Unknown: dec.Unknown, UnknownPromoted: dec.UnknownPromoted, Follows: dec.Follows}
		rec, ok, err := s.app.Files.LiveAt(ctx, d.ID, path.Join(ft.Source.DestFolder, f.RelPath))
		if err != nil {
			s.fail(w, r, "read catalog file", err)
			return
		}
		if ok && rec.SourceID == ft.Source.ID && rec.SourceRelPath == f.RelPath {
			t.Record = &FileRecord{State: string(rec.State), RelPath: rec.RelPath, Size: rec.Size, CopiedAt: rec.CopiedAt, VerifiedAt: rec.VerifiedAt}
		}
		out.Tiers = append(out.Tiers, t)
	}
	writeJSON(w, http.StatusOK, out)
}

// factUnknowns lists the fact sources that are unknown for a file, and why. Tautulli, Seerr and
// Maintainerr are left out while no integration of that application is set up (setUp, by type):
// their facts are then unknown on every file of the install, so listing them would put the
// "some facts are unknown" warning on every file and bury a stale or undecidable cache. Their
// facts still carry the reason; a rule that reads them still reports the unknown in its decision.
func factUnknowns(f *tiers.Facts, setUp map[integrations.Type]bool) []FactUnknown {
	out := []FactUnknown{}
	if f.Arr.State == tiers.ArrUnknown {
		out = append(out, FactUnknown{Source: tiers.SourceArr, Reason: f.Arr.Why})
	}
	if f.Plex != nil && !f.Plex.Known {
		out = append(out, FactUnknown{Source: tiers.SourcePlex, Reason: f.Plex.Why})
	}
	if f.Watch != nil && !f.Watch.Known && setUp[integrations.TypeTautulli] {
		out = append(out, FactUnknown{Source: tiers.SourceTautulli, Reason: f.Watch.Why})
	}
	if f.Requests != nil && f.Requests.Requested == tiers.Unknown && setUp[integrations.TypeSeerr] {
		out = append(out, FactUnknown{Source: tiers.SourceSeerr, Reason: f.Requests.Why})
	}
	if f.Maintainerr != nil && f.Maintainerr.Pending == tiers.Unknown && setUp[integrations.TypeMaintainerr] {
		out = append(out, FactUnknown{Source: tiers.SourceMaintainerr, Reason: f.Maintainerr.Why})
	}
	return out
}
