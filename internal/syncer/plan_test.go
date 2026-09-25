package syncer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// fakeFS is the planner's filesystem view in unit tests.
type fakeFS struct {
	srcHash map[string]string        // source path → head/tail hash
	dstHash map[string]string        // destination path → head/tail hash
	dstStat map[string]filecopy.Stat // destination path → what is there
	dstErr  map[string]error
}

func newFakeFS() *fakeFS {
	return &fakeFS{srcHash: map[string]string{}, dstHash: map[string]string{}, dstStat: map[string]filecopy.Stat{}, dstErr: map[string]error{}}
}

func (f *fakeFS) sourceHeadTail(rel string) (string, error) {
	if h, ok := f.srcHash[rel]; ok {
		return h, nil
	}
	return "", fs.ErrNotExist
}

func (f *fakeFS) destHeadTail(rel string) (string, error) {
	if h, ok := f.dstHash[rel]; ok {
		return h, nil
	}
	return "", fs.ErrNotExist
}

func (f *fakeFS) destStat(rel string) (filecopy.Stat, bool, error) {
	if err := f.dstErr[rel]; err != nil {
		return filecopy.Stat{}, false, err
	}
	st, ok := f.dstStat[rel]
	return st, ok, nil
}

// regular is a destination stat of a regular file.
func regular(size, mtimeNs int64) filecopy.Stat {
	return filecopy.Stat{Size: size, MtimeNs: mtimeNs, Mode: 0o644}
}

var testCaps = filecopy.Capabilities{Hardlinks: true, TrailingDotSpace: true, MtimeGranularityNs: 1}

// pf is a catalog file.
func pf(rel string, size, mtime int64, group string) *planFile {
	return &planFile{id: int64(len(rel)), rel: rel, size: size, mtimeNs: mtime, group: group}
}

var recID int64

// rec is a live record of source 1 in folder "movies".
func rec(srcRel string, size, mtime int64, state State, linkOf int64) *Record {
	recID++
	return &Record{ID: recID, DestinationID: 1, SourceID: 1, RelPath: "movies/" + srcRel, SourceRelPath: srcRel, Size: size,
		MtimeNs: mtime, State: state, LinkOf: linkOf, Hash: fmt.Sprintf("sha256:%d", recID)}
}

type planCase struct {
	files    []*planFile
	recs     []*Record
	fs       *fakeFS
	caps     filecopy.Capabilities
	settings destinations.Settings
	names    *nameIndex
}

func (c planCase) run(t *testing.T) *sourcePlanner {
	t.Helper()
	if c.fs == nil {
		c.fs = newFakeFS()
	}
	if c.caps == (filecopy.Capabilities{}) {
		c.caps = testCaps
	}
	if c.settings == (destinations.Settings{}) {
		c.settings = destinations.DefaultSettings()
	}
	if c.names == nil {
		c.names = newNameIndex(c.caps)
		for _, r := range c.recs {
			c.names.addRecord(r.RelPath, false)
		}
	}
	sort.Slice(c.files, func(i, j int) bool { return c.files[i].rel < c.files[j].rel })
	sort.Slice(c.recs, func(i, j int) bool { return c.recs[i].SourceRelPath < c.recs[j].SourceRelPath })
	p := &sourcePlanner{sourceID: 1, destFolder: "movies", caps: c.caps, settings: c.settings, names: c.names, fs: c.fs,
		files: c.files, recs: c.recs}
	if err := p.plan(context.Background()); err != nil {
		t.Fatalf("plan: %v", err)
	}
	return p
}

// describe renders items as "action path" (+ " [status]" when not pending), in plan order.
func describe(items []*planItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		s := string(it.action) + " " + it.rel
		if it.status != "" {
			s += " [" + string(it.status) + "]"
		}
		out = append(out, s)
	}
	return out
}

func find(items []*planItem, action jobs.ItemAction, rel string) *planItem {
	for _, it := range items {
		if it.action == action && it.rel == rel {
			return it
		}
	}
	return nil
}

func assertPlan(t *testing.T, p *sourcePlanner, want ...string) {
	t.Helper()
	got := describe(p.items())
	if !slices.Equal(got, want) {
		t.Fatalf("plan\n got %q\nwant %q", got, want)
	}
}

func TestPlanBasicActions(t *testing.T) {
	const m = int64(1_000_000_000_123)
	t.Run("new file is copied", func(t *testing.T) {
		p := planCase{files: []*planFile{pf("a.mkv", 10, m, "")}}.run(t)
		assertPlan(t, p, "copy movies/a.mkv")
		it := p.items()[0]
		if it.bytes != 10 || it.d.Reason != "new" || it.d.Source != "a.mkv" || it.change {
			t.Errorf("item %+v", it)
		}
	})
	t.Run("unchanged file needs nothing", func(t *testing.T) {
		p := planCase{files: []*planFile{pf("a.mkv", 10, m, "")}, recs: []*Record{rec("a.mkv", 10, m, StatePresent, 0)}}.run(t)
		assertPlan(t, p)
	})
	t.Run("changed file is updated", func(t *testing.T) {
		p := planCase{files: []*planFile{pf("a.mkv", 12, m+1, "")}, recs: []*Record{rec("a.mkv", 10, m, StatePresent, 0)}}.run(t)
		assertPlan(t, p, "update movies/a.mkv")
		it := p.items()[0]
		if it.d.OldSize != 10 || !it.change || it.shrink {
			t.Errorf("item %+v", it)
		}
	})
	t.Run("mtime-only change is an update", func(t *testing.T) {
		p := planCase{files: []*planFile{pf("a.mkv", 10, m+5, "")}, recs: []*Record{rec("a.mkv", 10, m, StatePresent, 0)}}.run(t)
		assertPlan(t, p, "update movies/a.mkv")
	})
	t.Run("mtime within the destination's granularity is unchanged", func(t *testing.T) {
		caps := testCaps
		caps.MtimeGranularityNs = 1_000_000_000
		p := planCase{caps: caps, files: []*planFile{pf("a.mkv", 10, m+500, "")}, recs: []*Record{rec("a.mkv", 10, m, StatePresent, 0)}}.run(t)
		assertPlan(t, p)
	})
	t.Run("vanished file is retained", func(t *testing.T) {
		p := planCase{recs: []*Record{rec("a.mkv", 10, m, StatePresent, 0)}}.run(t)
		assertPlan(t, p, "retain movies/a.mkv")
		if it := p.items()[0]; !it.change || it.bytes != 10 || it.d.Reason != "vanished" {
			t.Errorf("item %+v", it)
		}
	})
	t.Run("missing record is repaired", func(t *testing.T) {
		r := rec("a.mkv", 10, m, StateMissing, 0)
		p := planCase{files: []*planFile{pf("a.mkv", 10, m, "")}, recs: []*Record{r}}.run(t)
		assertPlan(t, p, "copy movies/a.mkv")
		if it := p.items()[0]; it.d.RecordID != r.ID || it.d.Reason != "repair" || it.change || it.shrink {
			t.Errorf("item %+v", it)
		}
		// A source that shrank to less than half the recorded size is held like an update (S10b).
		r = rec("a.mkv", 10, m, StateMissing, 0)
		p = planCase{files: []*planFile{pf("a.mkv", 4, m+1, "")}, recs: []*Record{r}}.run(t)
		if it := p.items()[0]; !it.shrink || it.d.OldSize != 10 || it.change {
			t.Errorf("item %+v", it)
		}
	})
	t.Run("execution order", func(t *testing.T) {
		fsys := newFakeFS()
		fsys.srcHash["moved.mkv"], fsys.dstHash["movies/old.mkv"] = "h", "h"
		p := planCase{fs: fsys,
			files: []*planFile{pf("moved.mkv", 5, m, ""), pf("changed.mkv", 7, m+1, ""), pf("new.mkv", 3, m, "")},
			recs: []*Record{rec("old.mkv", 5, m, StatePresent, 0), rec("changed.mkv", 7, m, StatePresent, 0),
				rec("gone.mkv", 9, m, StatePresent, 0)}}.run(t)
		assertPlan(t, p, "move movies/moved.mkv", "update movies/changed.mkv", "copy movies/new.mkv", "retain movies/gone.mkv")
	})
}

func TestPlanMoveVersusCopyAndRetain(t *testing.T) {
	const m = int64(1_700_000_000_000_000_001)
	cases := []struct {
		name     string
		size     int64
		mtime    int64
		srcHash  string
		dstHash  string
		wantMove bool
	}{
		{"same size, mtime and head/tail", 100, m, "h1", "h1", true},
		{"head/tail differs", 100, m, "h1", "h2", false},
		{"mtime differs by 1ns", 100, m + 1, "h1", "h1", false},
		{"size differs", 101, m, "h1", "h1", false},
		{"source unreadable", 100, m, "", "h1", false},
		{"destination file gone", 100, m, "h1", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fsys := newFakeFS()
			if tc.srcHash != "" {
				fsys.srcHash["new/name.mkv"] = tc.srcHash
			}
			if tc.dstHash != "" {
				fsys.dstHash["movies/old.mkv"] = tc.dstHash
			}
			old := rec("old.mkv", 100, m, StatePresent, 0)
			p := planCase{fs: fsys, files: []*planFile{pf("new/name.mkv", tc.size, tc.mtime, "")}, recs: []*Record{old}}.run(t)
			if tc.wantMove {
				assertPlan(t, p, "move movies/new/name.mkv")
				if it := p.items()[0]; it.d.From != "movies/old.mkv" || it.d.RecordID != old.ID || it.change {
					t.Errorf("move %+v", it)
				}
			} else {
				assertPlan(t, p, "copy movies/new/name.mkv", "retain movies/old.mkv")
			}
		})
	}
	t.Run("one vanished record pairs with one new name", func(t *testing.T) {
		fsys := newFakeFS()
		fsys.srcHash["x1.mkv"], fsys.srcHash["x2.mkv"], fsys.dstHash["movies/old.mkv"] = "h", "h", "h"
		p := planCase{fs: fsys, files: []*planFile{pf("x1.mkv", 100, m, ""), pf("x2.mkv", 100, m, "")},
			recs: []*Record{rec("old.mkv", 100, m, StatePresent, 0)}}.run(t)
		assertPlan(t, p, "move movies/x1.mkv", "copy movies/x2.mkv")
	})
	t.Run("a link name is never a move target", func(t *testing.T) {
		fsys := newFakeFS()
		fsys.srcHash["a.mkv"], fsys.srcHash["b.mkv"], fsys.dstHash["movies/old.mkv"] = "h", "h", "h"
		p := planCase{fs: fsys, files: []*planFile{pf("a.mkv", 100, m, "1.1:1"), pf("b.mkv", 100, m, "1.1:1")},
			recs: []*Record{rec("old.mkv", 100, m, StatePresent, 0)}}.run(t)
		assertPlan(t, p, "move movies/a.mkv", "link movies/b.mkv")
	})
	t.Run("a missing record is never moved", func(t *testing.T) {
		fsys := newFakeFS()
		fsys.srcHash["n.mkv"], fsys.dstHash["movies/old.mkv"] = "h", "h"
		p := planCase{fs: fsys, files: []*planFile{pf("n.mkv", 100, m, "")}, recs: []*Record{rec("old.mkv", 100, m, StateMissing, 0)}}.run(t)
		assertPlan(t, p, "copy movies/n.mkv", "retain movies/old.mkv")
	})
	t.Run("fatal read error stops planning", func(t *testing.T) {
		fsys := newFakeFS()
		fsys.srcHash["n.mkv"] = "h"
		old := rec("old.mkv", 100, m, StatePresent, 0)
		p := &sourcePlanner{sourceID: 1, destFolder: "movies", caps: testCaps, settings: destinations.DefaultSettings(),
			names: newNameIndex(testCaps), fs: eioDest{fsys}, files: []*planFile{pf("n.mkv", 100, m, "")}, recs: []*Record{old}}
		if err := p.plan(context.Background()); err == nil || filecopy.Classify(err) != filecopy.Fatal {
			t.Fatalf("err = %v, want a fatal error", err)
		}
	})
}

// eioDest fails every destination read with EIO (a lost mount).
type eioDest struct{ *fakeFS }

func (eioDest) destHeadTail(string) (string, error) { return "", syscall.EIO }

func TestPlanHardlinks(t *testing.T) {
	const m = int64(1_600_000_000_000_000_000)
	t.Run("new group: copy the first name, link the others", func(t *testing.T) {
		p := planCase{files: []*planFile{pf("t/x.mkv", 50, m, "1.1:1"), pf("l/x.mkv", 50, m, "1.1:1"), pf("u/x.mkv", 50, m, "1.1:1")}}.run(t)
		assertPlan(t, p, "copy movies/l/x.mkv", "link movies/t/x.mkv", "link movies/u/x.mkv")
		for _, it := range p.links {
			if it.bytes != 0 || it.d.Primary != "l/x.mkv" || it.after != p.puts[0] || it.change {
				t.Errorf("link %+v", it)
			}
		}
	})
	t.Run("a group whose members are elsewhere is a single file", func(t *testing.T) {
		p := planCase{files: []*planFile{pf("x.mkv", 50, m, "1.1:1")}}.run(t)
		assertPlan(t, p, "copy movies/x.mkv")
	})
	t.Run("new name of a recorded group links to the present one", func(t *testing.T) {
		p := planCase{files: []*planFile{pf("a.mkv", 50, m, "1.2:1"), pf("b.mkv", 50, m, "1.2:1")},
			recs: []*Record{rec("b.mkv", 50, m, StatePresent, 0)}}.run(t)
		assertPlan(t, p, "link movies/a.mkv")
		if p.links[0].d.Primary != "b.mkv" || p.links[0].after != nil {
			t.Errorf("link %+v", p.links[0])
		}
	})
	t.Run("inode numbers alone never link files", func(t *testing.T) {
		// Two files that a previous scan saw as one inode but that this scan does not group
		// (inode reuse): two copies, no link, even though their records say "linked".
		a := rec("a.mkv", 50, m, StatePresent, 0)
		b := rec("b.mkv", 50, m, StateLinked, a.ID)
		p := planCase{files: []*planFile{pf("a.mkv", 60, m+1, ""), pf("b.mkv", 70, m+2, "")}, recs: []*Record{a, b}}.run(t)
		assertPlan(t, p, "update movies/a.mkv", "update movies/b.mkv")
	})
	t.Run("vanished primary with a recorded-only link is promoted, not retained", func(t *testing.T) {
		a := rec("a.mkv", 50, m, StatePresent, 0)
		b := rec("b.mkv", 50, m, StateLinkRecorded, a.ID)
		p := planCase{files: []*planFile{pf("b.mkv", 50, m, "")}, recs: []*Record{a, b}}.run(t)
		assertPlan(t, p, "promote movies/b.mkv")
		pr := p.promotes[0]
		if pr.d.RecordID != a.ID || pr.d.TargetID != b.ID || pr.d.For != "retain" || pr.d.From != "movies/a.mkv" || !pr.change {
			t.Errorf("promote %+v", pr)
		}
	})
	t.Run("vanished primary with a hardlink: promote the record, retain the name", func(t *testing.T) {
		a := rec("a.mkv", 50, m, StatePresent, 0)
		b := rec("b.mkv", 50, m, StateLinked, a.ID)
		c := rec("c.mkv", 50, m, StateLinkRecorded, a.ID)
		p := planCase{files: []*planFile{pf("b.mkv", 50, m, "1.3:1"), pf("c.mkv", 50, m, "1.3:1")}, recs: []*Record{a, b, c}}.run(t)
		assertPlan(t, p, "promote movies/b.mkv", "retain movies/a.mkv")
		if pr := p.promotes[0]; pr.d.TargetID != b.ID || pr.after != p.retains[0] || pr.change {
			t.Errorf("promote %+v (the hardlink is preferred; it is held with the retain)", pr)
		}
	})
	t.Run("vanished dependents are retained with their primary", func(t *testing.T) {
		a := rec("a.mkv", 50, m, StatePresent, 0)
		b := rec("b.mkv", 50, m, StateLinkRecorded, a.ID)
		p := planCase{recs: []*Record{a, b}}.run(t)
		assertPlan(t, p, "retain movies/a.mkv", "retain movies/b.mkv")
	})
	t.Run("group split: the unchanged name keeps the old content", func(t *testing.T) {
		// h1 was replaced by a new file (new inode); h2 still has the old content.
		for _, st := range []State{StateLinked, StateLinkRecorded} {
			h1 := rec("h1.mkv", 50, m, StatePresent, 0)
			h2 := rec("h2.mkv", 50, m, st, h1.ID)
			p := planCase{files: []*planFile{pf("h1.mkv", 55, m+9, ""), pf("h2.mkv", 50, m, "")}, recs: []*Record{h1, h2}}.run(t)
			assertPlan(t, p, "promote movies/h2.mkv", "update movies/h1.mkv")
			if pr := p.promotes[0]; pr.d.For != "update" || pr.after != p.puts[0] {
				t.Errorf("%s: promote %+v", st, pr)
			}
		}
	})
	t.Run("both names changed together: update and relink", func(t *testing.T) {
		s1 := rec("s1.mkv", 50, m, StatePresent, 0)
		s2 := rec("s2.mkv", 50, m, StateLinked, s1.ID)
		p := planCase{files: []*planFile{pf("s1.mkv", 60, m+3, "1.4:1"), pf("s2.mkv", 60, m+3, "1.4:1")}, recs: []*Record{s1, s2}}.run(t)
		assertPlan(t, p, "update movies/s1.mkv", "link movies/s2.mkv")
		l := p.links[0]
		if l.d.RecordID != s2.ID || l.d.Reason != "relink" || !l.change || l.after != p.puts[0] {
			t.Errorf("relink %+v", l)
		}
	})
	t.Run("a renamed primary whose other name survives: the survivor takes over", func(t *testing.T) {
		// The group still has a recorded name, so the new name is a link of it (no copy, no
		// move): the surviving name gets the content by promote.
		fsys := newFakeFS()
		fsys.srcHash["a2.mkv"], fsys.dstHash["movies/a.mkv"] = "h", "h"
		a := rec("a.mkv", 50, m, StatePresent, 0)
		b := rec("b.mkv", 50, m, StateLinkRecorded, a.ID)
		p := planCase{fs: fsys, files: []*planFile{pf("a2.mkv", 50, m, "1.5:1"), pf("b.mkv", 50, m, "1.5:1")}, recs: []*Record{a, b}}.run(t)
		assertPlan(t, p, "promote movies/b.mkv", "link movies/a2.mkv")
		if p.links[0].d.Primary != "b.mkv" {
			t.Errorf("link %+v", p.links[0])
		}
	})
	t.Run("a renamed group moves once and links the other names", func(t *testing.T) {
		fsys := newFakeFS()
		fsys.srcHash["x/a.mkv"], fsys.dstHash["movies/a.mkv"] = "h", "h"
		a := rec("a.mkv", 50, m, StatePresent, 0)
		b := rec("b.mkv", 50, m, StateLinkRecorded, a.ID)
		p := planCase{fs: fsys, files: []*planFile{pf("x/a.mkv", 50, m, "1.5:1"), pf("x/b.mkv", 50, m, "1.5:1")}, recs: []*Record{a, b}}.run(t)
		assertPlan(t, p, "move movies/x/a.mkv", "link movies/x/b.mkv", "retain movies/b.mkv")
	})
	t.Run("damaged primary: its surviving names get their own copies", func(t *testing.T) {
		a := rec("a.mkv", 50, m, StateMissing, 0)
		b := rec("b.mkv", 50, m, StateLinkRecorded, a.ID)
		p := planCase{files: []*planFile{pf("b.mkv", 50, m, "")}, recs: []*Record{a, b}}.run(t)
		assertPlan(t, p, "copy movies/b.mkv", "retain movies/a.mkv")
		if it := p.puts[0]; it.d.RecordID != b.ID || it.d.Reason != "repair" {
			t.Errorf("repair %+v", it)
		}
	})
	t.Run("hardlinks of a damaged primary are relinked after its repair", func(t *testing.T) {
		// The hardlink may be the primary's damaged inode: a repair checks its old version when it
		// goes to retention.
		p1 := rec("p.mkv", 50, m, StateMissing, 0)
		d1 := rec("q.mkv", 50, m, StateLinked, p1.ID)
		p := planCase{files: []*planFile{pf("p.mkv", 50, m, "1.7:1"), pf("q.mkv", 50, m, "1.7:1")}, recs: []*Record{p1, d1}}.run(t)
		assertPlan(t, p, "copy movies/p.mkv", "link movies/q.mkv")
		if l := p.links[0]; l.d.RecordID != d1.ID || l.change || l.after != p.puts[0] || l.d.Reason != "repair" {
			t.Errorf("relink %+v", l)
		}
		// No longer grouped: the hardlink gets its own copy.
		p2 := rec("p.mkv", 50, m, StateMissing, 0)
		d2 := rec("q.mkv", 50, m, StateLinked, p2.ID)
		p = planCase{files: []*planFile{pf("p.mkv", 50, m, ""), pf("q.mkv", 50, m, "")}, recs: []*Record{p2, d2}}.run(t)
		assertPlan(t, p, "copy movies/p.mkv", "copy movies/q.mkv")
		if it := p.puts[1]; it.d.RecordID != d2.ID || it.d.Reason != "repair" {
			t.Errorf("copy %+v", it)
		}
	})
	t.Run("damaged primary with a recorded-only link: the repair restores the shared content", func(t *testing.T) {
		a := rec("a.mkv", 50, m, StateMissing, 0)
		b := rec("b.mkv", 50, m, StateLinkRecorded, a.ID)
		p := planCase{files: []*planFile{pf("a.mkv", 50, m, "1.8:1"), pf("b.mkv", 50, m, "1.8:1")}, recs: []*Record{a, b}}.run(t)
		assertPlan(t, p, "copy movies/a.mkv")
	})
	t.Run("damaged primary whose content changed: its recorded-only links get their own copies", func(t *testing.T) {
		// a was replaced at the source (new inode): the repair writes a's new content, which is
		// not the content b recorded. b's own copy comes first (a's repair is refused while b
		// still records a).
		a := rec("a.mkv", 50, m, StateMissing, 0)
		b := rec("b.mkv", 50, m, StateLinkRecorded, a.ID)
		p := planCase{files: []*planFile{pf("a.mkv", 60, m+5, ""), pf("b.mkv", 50, m, "")}, recs: []*Record{a, b}}.run(t)
		assertPlan(t, p, "copy movies/b.mkv", "copy movies/a.mkv")
		if it := p.puts[0]; it.d.RecordID != b.ID || it.d.Reason != "repair" || it.change {
			t.Errorf("repair %+v", it)
		}
	})
	t.Run("missing name of a group is relinked", func(t *testing.T) {
		a := rec("a.mkv", 50, m, StatePresent, 0)
		b := rec("b.mkv", 50, m, StateMissing, 0)
		p := planCase{files: []*planFile{pf("a.mkv", 50, m, "1.6:1"), pf("b.mkv", 50, m, "1.6:1")}, recs: []*Record{a, b}}.run(t)
		assertPlan(t, p, "link movies/b.mkv")
		if l := p.links[0]; l.d.RecordID != b.ID || l.change {
			t.Errorf("relink %+v", l)
		}
	})
}

// cancellingFS cancels the planning context at its first destination read and counts the reads.
type cancellingFS struct {
	*fakeFS
	cancel func()
	reads  int
}

func (c *cancellingFS) destStat(rel string) (filecopy.Stat, bool, error) {
	c.reads++
	c.cancel()
	return c.fakeFS.destStat(rel)
}

func (c *cancellingFS) sourceHeadTail(rel string) (string, error) {
	c.reads++
	c.cancel()
	return c.fakeFS.sourceHeadTail(rel)
}

func (c *cancellingFS) destHeadTail(rel string) (string, error) {
	c.reads++
	return c.fakeFS.destHeadTail(rel)
}

// TestPlanStopsWhenCancelled: planning reads the destination (and, for renames, the source) per
// file while the source lock is held; a cancelled job stops it at the next file (design §6.1).
func TestPlanStopsWhenCancelled(t *testing.T) {
	const m = int64(1_000)
	for _, renames := range []bool{false, true} {
		t.Run(fmt.Sprintf("renames=%v", renames), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fsys := &cancellingFS{fakeFS: newFakeFS(), cancel: cancel}
			var files []*planFile
			var recs []*Record
			for i := range 50 {
				rel := fmt.Sprintf("f%02d.mkv", i)
				files = append(files, pf(rel, 10, m, ""))
				if renames {
					recs = append(recs, rec("old/"+rel, 10, m, StatePresent, 0))
				}
			}
			p := &sourcePlanner{sourceID: 1, destFolder: "movies", caps: testCaps, settings: destinations.DefaultSettings(),
				names: newNameIndex(testCaps), fs: fsys, files: files, recs: recs}
			if err := p.plan(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("plan: %v", err)
			}
			if fsys.reads > 2 {
				t.Fatalf("%d filesystem reads after the cancel", fsys.reads)
			}
		})
	}
}

func TestPlanAdoption(t *testing.T) {
	const m = int64(1_650_000_000_123_456_789)
	cases := []struct {
		name   string
		mode   destinations.AdoptMode
		gran   int64
		window int
		dst    filecopy.Stat
		want   jobs.ItemAction
		disp   bool
	}{
		{"exact match", destinations.AdoptSizeMtime, 1, 0, regular(10, m), jobs.ActionAdopt, false},
		{"mtime 1ns off, ns granularity", destinations.AdoptSizeMtime, 1, 0, regular(10, m+1), jobs.ActionCopy, true},
		{"mtime truncated to seconds, 1s granularity", destinations.AdoptSizeMtime, 1e9, 0, regular(10, m/1e9*1e9), jobs.ActionAdopt, false},
		{"mtime 2s off, 1s granularity", destinations.AdoptSizeMtime, 1e9, 0, regular(10, m-2e9), jobs.ActionCopy, true},
		{"mtime 2s off, 2s window", destinations.AdoptSizeMtime, 1, 2, regular(10, m-2e9), jobs.ActionAdopt, false},
		{"mtime 3s off, 2s window", destinations.AdoptSizeMtime, 1, 2, regular(10, m-3e9), jobs.ActionCopy, true},
		{"size differs", destinations.AdoptSizeMtime, 1, 0, regular(11, m), jobs.ActionCopy, true},
		{"size+hash ignores mtime", destinations.AdoptSizeHash, 1, 0, regular(10, m-time.Hour.Nanoseconds()), jobs.ActionAdopt, false},
		{"size+hash needs the size", destinations.AdoptSizeHash, 1, 0, regular(9, m), jobs.ActionCopy, true},
		{"off never adopts", destinations.AdoptOff, 1, 0, regular(10, m), jobs.ActionCopy, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fsys := newFakeFS()
			fsys.dstStat["movies/a.mkv"] = tc.dst
			caps := testCaps
			caps.MtimeGranularityNs = tc.gran
			s := destinations.DefaultSettings()
			s.AdoptExisting, s.MtimeWindowSec = tc.mode, tc.window
			p := planCase{fs: fsys, caps: caps, settings: s, files: []*planFile{pf("a.mkv", 10, m, "")}}.run(t)
			it := p.items()[0]
			if it.action != tc.want || it.d.Displace != tc.disp || it.status != "" {
				t.Fatalf("got %s displace=%v status=%q, want %s displace=%v", it.action, it.d.Displace, it.status, tc.want, tc.disp)
			}
		})
	}
	t.Run("a directory in the way is left alone", func(t *testing.T) {
		fsys := newFakeFS()
		fsys.dstStat["movies/a.mkv"] = filecopy.Stat{Mode: fs.ModeDir | 0o755}
		p := planCase{fs: fsys, files: []*planFile{pf("a.mkv", 10, m, "")}}.run(t)
		if it := p.items()[0]; it.status != jobs.ItemFailed || !strings.Contains(it.errMsg, "not a regular file") {
			t.Fatalf("item %+v", it)
		}
	})
	t.Run("a parent that is a file is reported by the copy", func(t *testing.T) {
		fsys := newFakeFS()
		fsys.dstErr["movies/d/a.mkv"] = syscall.ENOTDIR
		p := planCase{fs: fsys, files: []*planFile{pf("d/a.mkv", 10, m, "")}}.run(t)
		assertPlan(t, p, "copy movies/d/a.mkv")
	})
}

func TestPlanNames(t *testing.T) {
	const m = int64(1_000)
	smb := filecopy.Capabilities{Hardlinks: false, CaseInsensitive: true, InvalidChars: `:?*"<>|\`, TrailingDotSpace: false, MtimeGranularityNs: 100}
	t.Run("names the destination cannot store fail", func(t *testing.T) {
		p := planCase{caps: smb, files: []*planFile{pf("What: Now.mkv", 1, m, ""), pf("dir./a.mkv", 1, m, ""), pf("ok.mkv", 1, m, ""),
			pf(".bunkarr-tmp-x", 1, m, "")}}.run(t)
		assertPlan(t, p, "copy movies/.bunkarr-tmp-x [failed]", "copy movies/What: Now.mkv [failed]", "copy movies/dir./a.mkv [failed]", "copy movies/ok.mkv")
		for _, it := range p.puts[:3] {
			if !strings.Contains(it.errMsg, filecopy.ErrNameNotStorable.Error()) {
				t.Errorf("%s: %s", it.rel, it.errMsg)
			}
		}
		if len(p.warnings) != 3 {
			t.Errorf("warnings %v", p.warnings)
		}
	})
	t.Run("case collision on a case-insensitive destination", func(t *testing.T) {
		p := planCase{caps: smb, files: []*planFile{pf("A.mkv", 1, m, ""), pf("a.mkv", 1, m, ""), pf("Dir/x.mkv", 1, m, "")},
			recs: []*Record{rec("dir/X.MKV", 1, m, StatePresent, 0)}}.run(t)
		// "A.mkv" is first (byte order); "a.mkv" collides with it; "Dir/x.mkv" collides with the
		// recorded "dir/X.MKV" (which vanished and is retained: new before old never overwrites).
		assertPlan(t, p, "copy movies/A.mkv", "copy movies/Dir/x.mkv [failed]", "copy movies/a.mkv [failed]", "retain movies/dir/X.MKV")
		for _, it := range p.puts[1:] {
			if !strings.Contains(it.errMsg, "differ only in case") {
				t.Errorf("%s: %s", it.rel, it.errMsg)
			}
		}
	})
	t.Run("case-only rename is a move of the same file", func(t *testing.T) {
		fsys := newFakeFS()
		fsys.srcHash["movie.mkv"], fsys.dstHash["movies/Movie.mkv"] = "h", "h"
		p := planCase{caps: smb, fs: fsys, files: []*planFile{pf("movie.mkv", 1, m, "")}, recs: []*Record{rec("Movie.mkv", 1, m, StatePresent, 0)}}.run(t)
		assertPlan(t, p, "move movies/movie.mkv")
	})
	t.Run("a case-sensitive destination stores both", func(t *testing.T) {
		p := planCase{files: []*planFile{pf("A.mkv", 1, m, ""), pf("a.mkv", 1, m, "")}}.run(t)
		assertPlan(t, p, "copy movies/A.mkv", "copy movies/a.mkv")
	})
	t.Run("a path recorded for a removed source is never used", func(t *testing.T) {
		names := newNameIndex(testCaps)
		names.addRecord("movies/a.mkv", true)
		p := planCase{names: names, files: []*planFile{pf("a.mkv", 1, m, ""), pf("b.mkv", 1, m, "")}}.run(t)
		assertPlan(t, p, "copy movies/a.mkv [failed]", "copy movies/b.mkv")
	})
	t.Run("link names are checked too", func(t *testing.T) {
		p := planCase{caps: smb, files: []*planFile{pf("a.mkv", 1, m, "1.1:1"), pf("b?.mkv", 1, m, "1.1:1")}}.run(t)
		assertPlan(t, p, "copy movies/a.mkv", "link movies/b?.mkv [failed]")
	})
}

func TestGuardHolds(t *testing.T) {
	const m = int64(5_000)
	settings := destinations.DefaultSettings() // 10 %, 1000 files
	many := func(n, total int) planCase {
		var c planCase
		for i := range total {
			rel := fmt.Sprintf("f%04d.mkv", i)
			r := rec(rel, 100, m, StatePresent, 0)
			c.recs = append(c.recs, r)
			if i >= n {
				c.files = append(c.files, pf(rel, 100, m, ""))
			}
		}
		c.files = append(c.files, pf("zz-new.mkv", 5, m, ""))
		return c
	}
	cases := []struct {
		name     string
		retained int
		total    int
		s        func(*destinations.Settings)
		allow    bool
		wantHeld int64
	}{
		{"20 of 100 files: under the 20-file floor", 20, 100, nil, false, 0},
		{"21 of 100 files: more than 10 % and 20", 21, 100, nil, false, 21},
		{"21 of 1000 files: under 10 %", 21, 1000, nil, false, 0},
		{"more than maxChangeFiles", 6, 30, func(s *destinations.Settings) { s.MaxChangeFiles = 5; s.MaxChangePercent = 100 }, false, 6},
		{"allowChanges holds nothing", 50, 100, nil, true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := many(tc.retained, tc.total)
			s := settings
			if tc.s != nil {
				tc.s(&s)
			}
			p := c.run(t)
			items := p.items()
			g := applyGuard(items, int64(len(c.files)), s, tc.allow)
			if g.held != tc.wantHeld || g.changes != int64(tc.retained) {
				t.Fatalf("held %d changes %d, want %d %d", g.held, g.changes, tc.wantHeld, tc.retained)
			}
			for _, it := range items {
				wantHeld := tc.wantHeld > 0 && it.action == jobs.ActionRetain
				if (it.status == jobs.ItemHeld) != wantHeld {
					t.Errorf("%s %s status %q", it.action, it.rel, it.status)
				}
				if it.action == jobs.ActionCopy && it.status != "" {
					t.Error("copies of new files are never held")
				}
			}
		})
	}
	t.Run("shrink and zero updates are always held, with what depends on them", func(t *testing.T) {
		big := rec("big.mkv", 1000, m, StatePresent, 0)
		dep := rec("dep.mkv", 1000, m, StateLinkRecorded, big.ID) // unchanged: promote before the update
		zero := rec("zero.mkv", 10, m, StatePresent, 0)
		ok := rec("ok.mkv", 1000, m, StatePresent, 0)
		c := planCase{files: []*planFile{pf("big.mkv", 400, m+1, "1.9:1"), pf("new-link.mkv", 400, m+1, "1.9:1"), pf("dep.mkv", 1000, m, ""),
			pf("zero.mkv", 0, m+1, ""), pf("ok.mkv", 600, m+1, "")}, recs: []*Record{big, dep, zero, ok}}
		p := c.run(t)
		items := p.items()
		g := applyGuard(items, 5, settings, false)
		held := map[string]bool{}
		for _, it := range items {
			if it.status == jobs.ItemHeld {
				held[string(it.action)+" "+it.rel] = true
			}
		}
		want := map[string]bool{"update movies/big.mkv": true, "update movies/zero.mkv": true, "promote movies/dep.mkv": true, "link movies/new-link.mkv": true}
		if fmt.Sprint(held) != fmt.Sprint(want) || g.limited {
			t.Fatalf("held %v, want %v (limited %v)", held, want, g.limited)
		}
		if it := find(items, jobs.ActionUpdate, "movies/ok.mkv"); it.status != "" {
			t.Errorf("an update to more than half the size runs: %q", it.status)
		}
		// The next sync with allowChanges runs them.
		p = c.run(t)
		if g := applyGuard(p.items(), 5, settings, true); g.held != 0 {
			t.Fatalf("allowChanges held %d", g.held)
		}
	})
	t.Run("failed items are not counted", func(t *testing.T) {
		items := []*planItem{{action: jobs.ActionRetain, change: true, status: jobs.ItemFailed}}
		if g := applyGuard(items, 1, destinations.Settings{MaxChangeFiles: 0, MaxChangePercent: 1}, false); g.changes != 0 || g.held != 0 {
			t.Fatalf("%+v", g)
		}
	})
}
