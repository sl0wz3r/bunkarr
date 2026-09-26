package tiers

import (
	"errors"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
)

func TestPreviewCountsAndItems(t *testing.T) {
	l := newLibrary(t)
	l.specPreset()
	cf := func(rel string) (int64, int64) {
		var size, mtime int64
		if err := l.db.Reader().QueryRowContext(l.ctx, `SELECT size, mtime_ns FROM catalog_files WHERE source_id = ? AND rel_path = ?`,
			l.movies.ID, rel).Scan(&size, &mtime); err != nil {
			t.Fatal(err)
		}
		return size, mtime
	}
	hs, hm := cf("Heat (1995)/Heat (1995).mkv")
	cs, cm := cf("Charade (1963)/Charade (1963).mkv")
	l.records[l.dest] = []RecordRef{
		{SourceID: l.movies.ID, SourceRelPath: "Heat (1995)/Heat (1995).mkv", Size: hs, MtimeNs: hm, State: "present"},
		{SourceID: l.movies.ID, SourceRelPath: "Charade (1963)/Charade (1963).mkv", Size: cs, MtimeNs: cm, State: "present"},
		// A backed-up file whose content now lives, unrecorded and manifest, in the Home source.
		{SourceID: l.movies.ID, SourceRelPath: "Gone/birthday.mp4", Size: 6 * mb, MtimeNs: l.fileMtime(l.home, "2019/birthday.mp4"), State: "present"},
	}
	p, err := l.eng.Preview(l.ctx, PreviewRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Revision != int64(1) || len(p.Destinations) != 1 || len(p.UnknownSources) != 0 || len(p.StaleReferences) != 0 {
		t.Fatalf("preview %+v", p)
	}
	d := p.Destinations[0]
	// Full: Heat + 2 sidecars + the extra (tag), Blob and Stray (unknown); manifest: Charade and
	// the home video.
	if d.Stored.Files != 3 || d.Full.Files != 6 || d.Manifest.Files != 2 || d.Skip.Files != 0 || d.UnknownPromoted.Files != 2 ||
		d.ToCopy.Files != 5 || d.Kept.Files != 1 || d.Kept.Bytes != cs || d.MovedToNonFull.Files != 1 || d.Full.UniqueBytes != d.Full.Bytes {
		t.Fatalf("destination %+v", d)
	}
	byRule := map[string]int64{}
	for _, r := range d.ByRule {
		byRule[r.Name] += r.Files
	}
	if byRule["Tagged bunkarr-full"] != 6 || byRule["Everything else"] != 2 {
		t.Errorf("by rule %+v", d.ByRule)
	}
	page, err := l.eng.PreviewItems(p.ID, ItemQuery{State: StateKept})
	if err != nil || page.TotalRecords != 1 || page.Records[0].RelPath != "Charade (1963)/Charade (1963).mkv" || page.Records[0].Tier != Manifest {
		t.Fatalf("kept items %v %+v", err, page)
	}
	page, _ = l.eng.PreviewItems(p.ID, ItemQuery{Search: "HEAT", PageSize: 2, Page: 2})
	if page.TotalRecords != 4 || len(page.Records) != 2 || page.Page != 2 {
		t.Fatalf("search page %+v", page)
	}
	page, _ = l.eng.PreviewItems(p.ID, ItemQuery{HasRuleID: true, RuleID: p.Destinations[0].ByRule[0].RuleID, DestinationID: l.dest})
	if page.TotalRecords == 0 {
		t.Error("rule filter found nothing")
	}
	// A draft is previewed without being saved.
	draft := []RuleInput{{Name: "skip all", Action: Skip}}
	p2, err := l.eng.Preview(l.ctx, PreviewRequest{Rules: &draft})
	if err != nil || p2.Revision != "draft" || p2.Destinations[0].Skip.Files != 8 || p2.Destinations[0].ByRule[0].RuleID != -1 {
		t.Fatalf("draft %v %+v", err, p2)
	}
	if rev, _ := l.eng.Revision(l.ctx); rev != 1 {
		t.Errorf("a preview saved rules: revision %d", rev)
	}
	bad := []RuleInput{{Name: "x", Action: "delete"}}
	var ve *ValidationError
	if _, err := l.eng.Preview(l.ctx, PreviewRequest{Rules: &bad}); !errors.As(err, &ve) {
		t.Errorf("invalid draft: %v", err)
	}
	// Previews expire after 10 minutes; at most 4 are kept.
	l.clock.Advance(PreviewTTL)
	if _, err := l.eng.PreviewItems(p.ID, ItemQuery{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired preview: %v", err)
	}
	var ids []string
	for range MaxPreviews + 1 {
		p, err := l.eng.Preview(l.ctx, PreviewRequest{})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, p.ID)
	}
	if _, err := l.eng.PreviewItems(ids[0], ItemQuery{}); !errors.Is(err, ErrNotFound) {
		t.Error("a fifth preview kept the first")
	}
	if _, err := l.eng.PreviewItems(ids[MaxPreviews], ItemQuery{}); err != nil {
		t.Error(err)
	}
}

func (e *env) fileMtime(s catalog.Source, rel string) int64 {
	e.t.Helper()
	var mtime int64
	if err := e.db.Reader().QueryRowContext(e.ctx, `SELECT mtime_ns FROM catalog_files WHERE source_id = ? AND rel_path = ?`, s.ID, rel).Scan(&mtime); err != nil {
		e.t.Fatal(err)
	}
	return mtime
}
