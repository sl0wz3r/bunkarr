package tiers

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// Preview limits (design §8.6).
const (
	// PreviewTTL is how long a preview's items stay pageable.
	PreviewTTL = 10 * time.Minute
	// MaxPreviews is how many previews are kept at once (the oldest goes first).
	MaxPreviews = 4
	// MaxPreviewItems bounds the items of the kept previews together (the oldest previews go
	// first; the newest is always kept).
	MaxPreviewItems = 1_000_000
)

// Preview item states.
const (
	StateStored    = "stored"
	StateToCopy    = "to-copy"
	StateKept      = "kept"
	StateNotCopied = "not-copied"
)

// Count is a number of files and their bytes.
type Count struct {
	Files int64 `json:"files"`
	Bytes int64 `json:"bytes"`
}

func (c *Count) add(size int64) {
	c.Files++
	c.Bytes += size
}

// FullCount is Count plus the bytes of the content once per hardlink group.
type FullCount struct {
	Count
	UniqueBytes int64 `json:"uniqueBytes"`
}

// RuleCount is what one rule decided at a destination.
type RuleCount struct {
	RuleID int64  `json:"ruleId"`
	Name   string `json:"name"`
	Action Tier   `json:"action"`
	Files  int64  `json:"files"`
	Bytes  int64  `json:"bytes"`
}

// DestinationPreview is a destination's part of a preview.
type DestinationPreview struct {
	DestinationID int64  `json:"destinationId"`
	Name          string `json:"name"`
	// Stored is what the live records hold now.
	Stored Count     `json:"stored"`
	Full   FullCount `json:"full"`
	// Manifest and Skip are the live files of those tiers.
	Manifest Count `json:"manifest"`
	Skip     Count `json:"skip"`
	// UnknownPromoted are full only because a fact is unknown.
	UnknownPromoted Count `json:"unknownPromoted"`
	// ToCopy are full files without an up-to-date record.
	ToCopy Count `json:"toCopy"`
	// Kept are non-full files with a record: what a release would free.
	Kept Count `json:"kept"`
	// MovedToNonFull are backed-up files whose content reappeared (same size and mtime) in
	// another source of the destination under a non-full tier.
	MovedToNonFull Count          `json:"movedToNonFull"`
	ByRule         []RuleCount    `json:"byRule"`
	ConfigBackups  []ConfigBackup `json:"configBackups"`
}

// TierPreview is POST /tiers/preview (design §8.6, §13).
type TierPreview struct {
	ID string `json:"id"`
	// Revision is the saved rules' revision, or "draft" for rules sent with the request.
	Revision        any                  `json:"revision"`
	CreatedAt       time.Time            `json:"createdAt"`
	UnknownSources  []UnknownSource      `json:"unknownSources"`
	StaleReferences []Warning            `json:"staleReferences"`
	Destinations    []DestinationPreview `json:"destinations"`
}

// PreviewItem is one file at one destination (GET /tiers/preview/{id}/items).
type PreviewItem struct {
	DestinationID   int64    `json:"destinationId"`
	FileID          int64    `json:"fileId"`
	SourceID        int64    `json:"sourceId"`
	SourceName      string   `json:"sourceName"`
	RelPath         string   `json:"relPath"`
	Size            int64    `json:"size"`
	Tier            Tier     `json:"tier"`
	RuleID          int64    `json:"ruleId"`
	RuleName        string   `json:"ruleName"`
	Reasons         []Reason `json:"reasons"`
	Unknown         []Reason `json:"unknown"`
	UnknownPromoted bool     `json:"unknownPromoted"`
	Follows         string   `json:"follows,omitempty"`
	State           string   `json:"state"`
}

// PreviewRequest is POST /tiers/preview: Rules nil previews the saved rules, otherwise the draft;
// DestinationIDs narrows the destinations (nil: every enabled one).
type PreviewRequest struct {
	Rules          *[]RuleInput `json:"rules,omitempty"`
	DestinationIDs []int64      `json:"destinationIds,omitempty"`
}

// ItemQuery filters and pages a preview's items.
type ItemQuery struct {
	DestinationID int64
	Tier          Tier
	// RuleID filters by the deciding rule; HasRuleID says it was given (0 is the built-ins).
	RuleID    int64
	HasRuleID bool
	State     string
	// Search is a case-insensitive substring of the path.
	Search   string
	Page     int
	PageSize int
}

// ItemPage is one page of a preview's items.
type ItemPage struct {
	Page         int           `json:"page"`
	PageSize     int           `json:"pageSize"`
	TotalRecords int64         `json:"totalRecords"`
	Records      []PreviewItem `json:"records"`
}

// storedPreview is a preview with its items, kept in memory.
type storedPreview struct {
	TierPreview
	items []PreviewItem
}

// previewCache keeps the previews in memory. An entry leaves at the latest ttl after it was put,
// by a timer, even when no request comes (an idle page must not pin a large preview), and the
// kept previews are bounded by count and by their items together.
type previewCache struct {
	mu       sync.Mutex
	now      func() time.Time
	ttl      time.Duration
	maxItems int
	list     []*storedPreview
}

func newPreviewCache(now func() time.Time) *previewCache {
	return &previewCache{now: now, ttl: PreviewTTL, maxItems: MaxPreviewItems}
}

func (c *previewCache) put(p *storedPreview) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked()
	c.list = append(c.list, p)
	if len(c.list) > MaxPreviews {
		c.list = slices.Clone(c.list[len(c.list)-MaxPreviews:])
	}
	total := 0
	for _, q := range c.list {
		total += len(q.items)
	}
	for len(c.list) > 1 && total > c.maxItems {
		total -= len(c.list[0].items)
		c.list = slices.Clone(c.list[1:])
	}
	// The timer holds the id only, so an evicted preview is not pinned until it fires.
	id := p.ID
	time.AfterFunc(c.ttl, func() { c.remove(id) })
}

// remove drops a preview (its timer expired).
func (c *previewCache) remove(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.list = slices.DeleteFunc(c.list, func(q *storedPreview) bool { return q.ID == id })
}

func (c *previewCache) get(id string) (*storedPreview, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked()
	for _, p := range c.list {
		if p.ID == id {
			return p, true
		}
	}
	return nil, false
}

func (c *previewCache) sweepLocked() {
	now := c.now()
	c.list = slices.DeleteFunc(c.list, func(p *storedPreview) bool { return now.Sub(p.CreatedAt) >= c.ttl })
}

// Preview evaluates the saved rules, or a draft, over every live file of every enabled
// destination's enabled sources (design §8.6) and keeps the result for PreviewTTL. Nothing is
// written: the preview reads the rules, the facts and the records only.
func (e *Engine) Preview(ctx context.Context, req PreviewRequest) (TierPreview, error) {
	q := e.o.DB.Reader()
	now := e.o.Now()
	rs, err := e.store.Rules(ctx, q)
	if err != nil {
		return TierPreview{}, err
	}
	rules := rs.Rules
	var revision any = rs.Revision
	if req.Rules != nil {
		norm, err := Validate(*req.Rules)
		if err != nil {
			return TierPreview{}, err
		}
		rules, revision = draftRules(norm), "draft"
	}
	if e.o.Destinations == nil {
		return TierPreview{}, fmt.Errorf("tiers: the preview has no destination list")
	}
	dests, err := e.o.Destinations(ctx)
	if err != nil {
		return TierPreview{}, fmt.Errorf("tiers: %w", err)
	}
	fc, err := e.newContext(ctx, q, now)
	if err != nil {
		return TierPreview{}, err
	}
	ev := NewEvaluator(rules, rs.Revision, now)
	stale, err := e.staleReferences(ctx, q, fc, rules, func(r Rule) bool { return r.Enabled }, true)
	if err != nil {
		return TierPreview{}, err
	}
	unknown := slices.Clone(fc.unknown)
	for _, p := range e.o.Providers {
		more, err := p.Unknown(ctx, q, now)
		if err != nil {
			return TierPreview{}, fmt.Errorf("tiers: %w", err)
		}
		unknown = append(unknown, more...)
	}
	sp := &storedPreview{TierPreview: TierPreview{ID: newPreviewID(), Revision: revision, CreatedAt: now,
		UnknownSources: unknown, StaleReferences: stale, Destinations: []DestinationPreview{}}}
	if sp.UnknownSources == nil {
		sp.UnknownSources = []UnknownSource{}
	}
	factsBySource := map[int64][]*Facts{}
	for _, d := range dests {
		if !d.Enabled || (len(req.DestinationIDs) > 0 && !slices.Contains(req.DestinationIDs, d.ID)) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return TierPreview{}, err
		}
		dp, items, err := e.previewDestination(ctx, q, fc, ev, rules, d, factsBySource)
		if err != nil {
			return TierPreview{}, err
		}
		sp.Destinations = append(sp.Destinations, dp)
		sp.items = append(sp.items, items...)
	}
	slices.SortStableFunc(sp.items, func(a, b PreviewItem) int {
		if c := cmp.Compare(a.DestinationID, b.DestinationID); c != 0 {
			return c
		}
		if c := cmp.Compare(a.SourceID, b.SourceID); c != 0 {
			return c
		}
		return cmp.Compare(a.RelPath, b.RelPath)
	})
	e.previews.put(sp)
	return sp.TierPreview, nil
}

// previewDestination computes one destination's counts and items.
func (e *Engine) previewDestination(ctx context.Context, q Queryer, fc *factContext, ev *Evaluator, rules []Rule,
	d DestinationRef, factsBySource map[int64][]*Facts) (DestinationPreview, []PreviewItem, error) {
	dp := DestinationPreview{DestinationID: d.ID, Name: d.Name, ByRule: []RuleCount{}, ConfigBackups: []ConfigBackup{}}
	var recs []RecordRef
	if e.o.Records != nil {
		var err error
		if recs, err = e.o.Records(ctx, d.ID); err != nil {
			return dp, nil, fmt.Errorf("tiers: %w", err)
		}
	}
	type key struct {
		src int64
		rel string
	}
	recAt := make(map[key]RecordRef, len(recs))
	for _, r := range recs {
		recAt[key{r.SourceID, r.SourceRelPath}] = r
		if r.State != "missing" {
			dp.Stored.add(r.Size)
		}
	}
	if e.o.ConfigBackups != nil {
		cb, err := e.o.ConfigBackups(ctx, d.ID)
		if err != nil {
			return dp, nil, fmt.Errorf("tiers: %w", err)
		}
		dp.ConfigBackups = append(dp.ConfigBackups, cb...)
	}
	byRule := map[[2]any]*RuleCount{}
	var ruleOrder [][2]any
	var items []PreviewItem
	live := map[key]bool{}
	type sig struct{ size, mtime int64 }
	nonFullNew := map[sig][]int64{} // non-full files without a record, by size and mtime: source ids
	groupsSeen := map[string]bool{}
	for _, srcID := range d.SourceIDs {
		src, ok := fc.sources[srcID]
		if !ok || !src.Enabled {
			continue
		}
		facts, ok := factsBySource[srcID]
		if !ok {
			var err error
			if facts, err = e.loadFacts(ctx, q, fc, src, nil, neededFields(rules, func(Rule) bool { return true })); err != nil {
				return dp, nil, err
			}
			factsBySource[srcID] = facts
		}
		decisions := decideFiles(ev, d.ID, facts)
		for _, f := range facts {
			dec := decisions[f.FileID]
			k := key{srcID, f.RelPath}
			live[k] = true
			rec, hasRec := recAt[k]
			hasRec = hasRec && rec.State != "missing"
			var state string
			switch {
			case dec.Tier == Full:
				dp.Full.add(f.Size)
				if f.Group == "" || !groupsSeen[f.Group] {
					dp.Full.UniqueBytes += f.Size
					if f.Group != "" {
						groupsSeen[f.Group] = true
					}
				}
				if dec.UnknownPromoted {
					dp.UnknownPromoted.add(f.Size)
				}
				if hasRec && rec.Size == f.Size && rec.MtimeNs == f.MtimeNs {
					state = StateStored
				} else {
					state = StateToCopy
					dp.ToCopy.add(f.Size)
				}
			default:
				if dec.Tier == Manifest {
					dp.Manifest.add(f.Size)
				} else {
					dp.Skip.add(f.Size)
				}
				if hasRec {
					state = StateKept
					dp.Kept.add(f.Size)
				} else {
					state = StateNotCopied
					s := sig{f.Size, f.MtimeNs}
					nonFullNew[s] = append(nonFullNew[s], srcID)
				}
			}
			rk := [2]any{dec.RuleID, dec.RuleName}
			rc, ok := byRule[rk]
			if !ok {
				rc = &RuleCount{RuleID: dec.RuleID, Name: dec.RuleName, Action: dec.Tier}
				if dec.RuleID != 0 {
					for _, r := range rules {
						if r.ID == dec.RuleID {
							rc.Action = r.Action
						}
					}
				}
				byRule[rk] = rc
				ruleOrder = append(ruleOrder, rk)
			}
			rc.Files++
			rc.Bytes += f.Size
			items = append(items, PreviewItem{DestinationID: d.ID, FileID: f.FileID, SourceID: srcID, SourceName: src.Name,
				RelPath: f.RelPath, Size: f.Size, Tier: dec.Tier, RuleID: dec.RuleID, RuleName: dec.RuleName, Reasons: dec.Reasons,
				Unknown: dec.Unknown, UnknownPromoted: dec.UnknownPromoted, Follows: dec.Follows, State: state})
		}
	}
	// Backed-up content that reappears in another source under a non-full tier cannot be paired
	// as a move: counted as moved to a non-full location.
	for k, r := range recAt {
		if live[k] || r.State == "missing" || r.State == "link_recorded" {
			continue
		}
		for _, s := range nonFullNew[sig{r.Size, r.MtimeNs}] {
			if s != k.src {
				dp.MovedToNonFull.add(r.Size)
				break
			}
		}
	}
	for _, rk := range ruleOrder {
		dp.ByRule = append(dp.ByRule, *byRule[rk])
	}
	return dp, items, nil
}

// PreviewItems pages a kept preview's items (ErrNotFound once it expired).
func (e *Engine) PreviewItems(id string, iq ItemQuery) (ItemPage, error) {
	p, ok := e.previews.get(id)
	if !ok {
		return ItemPage{}, fmt.Errorf("preview %s: %w (previews are kept %s)", id, ErrNotFound, PreviewTTL)
	}
	if iq.Page < 1 {
		iq.Page = 1
	}
	if iq.PageSize < 1 {
		iq.PageSize = 50
	}
	iq.PageSize = min(iq.PageSize, 500)
	search := strings.ToLower(iq.Search)
	var match []PreviewItem
	for _, it := range p.items {
		switch {
		case iq.DestinationID != 0 && it.DestinationID != iq.DestinationID,
			iq.Tier != "" && it.Tier != iq.Tier,
			iq.HasRuleID && it.RuleID != iq.RuleID,
			iq.State != "" && it.State != iq.State,
			search != "" && !strings.Contains(strings.ToLower(it.RelPath), search):
			continue
		}
		match = append(match, it)
	}
	out := ItemPage{Page: iq.Page, PageSize: iq.PageSize, TotalRecords: int64(len(match)), Records: []PreviewItem{}}
	start := (iq.Page - 1) * iq.PageSize
	if start < len(match) {
		out.Records = match[start:min(start+iq.PageSize, len(match))]
	}
	return out, nil
}

func newPreviewID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails (Go 1.24+)
	return hex.EncodeToString(b[:])
}
