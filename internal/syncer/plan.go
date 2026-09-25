package syncer

import (
	"context"
	"errors"
	"fmt"
	"path"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// planFile is a live catalog file as the planner sees it.
type planFile struct {
	id      int64
	rel     string // inside the source
	size    int64
	mtimeNs int64
	group   string // per-scan hardlink group ("" = none)

	rec       *Record // the live record of this source path, if any
	unchanged bool    // rec exists, is not missing, and has the file's size and mtime
}

// planFS is what the planner reads from the filesystems (never writes).
type planFS interface {
	// sourceHeadTail returns the head/tail hash of a file inside the source.
	sourceHeadTail(sourceRel string) (string, error)
	// destHeadTail returns the head/tail hash of a destination file.
	destHeadTail(destRel string) (string, error)
	// destStat lstats a destination path; exists is false when nothing is there.
	destStat(destRel string) (st filecopy.Stat, exists bool, err error)
}

// planItem is an item the planner emitted, before it is persisted.
type planItem struct {
	action jobs.ItemAction
	rel    string // destination path (Item.RelPath)
	fileID int64
	bytes  int64
	d      Detail
	status jobs.ItemStatus
	errMsg string

	// change marks a change the mass-change guard counts (S10b): an update, a relink of a changed
	// name, a retain, or a promote that stands for a vanished name.
	change bool
	// shrink marks an update to 0 bytes or to less than half the old size (always held).
	shrink bool
	// after is the item this one depends on: when that one is held, so is this one.
	after *planItem
}

func (it *planItem) item() jobs.Item {
	st := it.status
	if st == "" {
		st = jobs.ItemPending
	}
	return jobs.Item{FileID: it.fileID, RelPath: it.rel, Action: it.action, Status: st, Bytes: it.bytes, Error: it.errMsg, Detail: it.d.raw()}
}

func (it *planItem) fail(err error) {
	it.status = jobs.ItemFailed
	it.errMsg = err.Error()
}

// sourcePlanner plans one source (design §4.2): it diffs the source's live catalog against its live
// destination records.
type sourcePlanner struct {
	sourceID   int64
	destFolder string
	caps       filecopy.Capabilities
	settings   destinations.Settings
	names      *nameIndex
	fs         planFS

	files []*planFile // sorted by rel
	recs  []*Record   // live records of the source, sorted by SourceRelPath

	byRel    map[string]*planFile
	recBySrc map[string]*Record
	deps     map[int64][]*Record // live dependents by primary id
	groups   map[string][]*planFile

	promotes, moves, puts, links, retains []*planItem
	// itemOf is the item that writes a file's content (copy, adopt, update, move, repair).
	itemOf map[*planFile]*planItem
	// promoteFor maps a record being updated to the promote that saves its old content.
	promoteFor map[int64]*planItem
	warnings   []string
}

// items returns the plan in execution order: promote, move, copy/update/adopt, link, retain.
func (p *sourcePlanner) items() []*planItem {
	out := make([]*planItem, 0, len(p.promotes)+len(p.moves)+len(p.puts)+len(p.links)+len(p.retains))
	out = append(out, p.promotes...)
	out = append(out, p.moves...)
	out = append(out, p.puts...)
	out = append(out, p.links...)
	return append(out, p.retains...)
}

func (p *sourcePlanner) destPath(sourceRel string) string { return path.Join(p.destFolder, sourceRel) }

func (p *sourcePlanner) mtimeEqual(a, b int64) bool {
	return filecopy.MtimeMatch(a, b, p.caps.MtimeGranularityNs, 0)
}

// plan computes the items. It returns an error only for a fatal filesystem error while reading, or
// ctx's error when the job is cancelled (it reads the filesystems per file while the source's lock
// is held, so it checks ctx for every file).
func (p *sourcePlanner) plan(ctx context.Context) error {
	p.byRel = make(map[string]*planFile, len(p.files))
	p.recBySrc = make(map[string]*Record, len(p.recs))
	p.deps = map[int64][]*Record{}
	p.groups = map[string][]*planFile{}
	p.itemOf = map[*planFile]*planItem{}
	p.promoteFor = map[int64]*planItem{}
	for _, r := range p.recs {
		p.recBySrc[r.SourceRelPath] = r
		if r.LinkOf != 0 {
			p.deps[r.LinkOf] = append(p.deps[r.LinkOf], r)
		}
	}
	seen := make(map[int64]bool, len(p.recs))
	for _, f := range p.files {
		p.byRel[f.rel] = f
		if r := p.recBySrc[f.rel]; r != nil {
			f.rec = r
			seen[r.ID] = true
			f.unchanged = r.State != StateMissing && r.Size == f.size && p.mtimeEqual(r.MtimeNs, f.mtimeNs)
		}
		if f.group != "" {
			p.groups[f.group] = append(p.groups[f.group], f)
		}
	}
	for g, m := range p.groups {
		if len(m) < 2 { // the other names are excluded or outside the source: a single file
			m[0].group = ""
			delete(p.groups, g)
		}
	}
	var vanished []*Record
	for _, r := range p.recs {
		if !seen[r.ID] {
			vanished = append(vanished, r)
		}
	}

	moveFrom := map[*planFile]*Record{}
	moved := map[int64]bool{}
	if err := p.pairMoves(ctx, vanished, moveFrom, moved); err != nil {
		return err
	}

	promoteTarget := map[int64]bool{}
	needRepair := map[int64]bool{}
	for _, v := range vanished {
		if !moved[v.ID] {
			p.planVanished(v, promoteTarget, needRepair)
		}
	}
	// A changed primary whose unchanged dependents still need its old content: promote first.
	for _, f := range p.files {
		r := f.rec
		if r == nil || f.unchanged || r.State != StatePresent {
			continue
		}
		if surv := p.surviving(r); len(surv) > 0 {
			t := pickTarget(surv)
			p.promoteFor[r.ID] = p.promote(r, t, "update")
			promoteTarget[t.ID] = true
		}
	}

	// A damaged (missing) primary's recorded-only links have no file of their own: its repair
	// restores their content only when its source still has that content; otherwise each gets its
	// own copy, planned before the primary's repair (which is refused while such a link still
	// records the primary).
	for _, f := range p.files {
		if r := f.rec; r != nil && r.State == StateMissing {
			for _, d := range p.surviving(r) {
				if d.State == StateLinkRecorded && (f.size != d.Size || !p.mtimeEqual(f.mtimeNs, d.MtimeNs)) && !needRepair[d.ID] {
					needRepair[d.ID] = true
					p.repair(p.byRel[d.SourceRelPath], d)
				}
			}
		}
	}

	primary := make(map[string]*planFile, len(p.groups))
	for g, members := range p.groups {
		primary[g] = choosePrimary(members, promoteTarget, moveFrom)
	}
	// Hardlinks made at the destination of a primary that verify found missing or damaged are
	// linked again once the primary is repaired (or copied, when no longer grouped). Such a
	// hardlink's file may be the primary's damaged inode: its old version is checked when it goes
	// to retention (a repair, see itemRun.oldVersion).
	relink := map[int64]bool{}
	for _, f := range p.files {
		if f.rec == nil || f.rec.State != StateMissing {
			continue
		}
		for _, d := range p.deps[f.rec.ID] {
			if df := p.byRel[d.SourceRelPath]; d.State == StateLinked && df != nil && df.rec == d && df.unchanged {
				relink[d.ID] = true
			}
		}
	}

	var linkItems []struct {
		it *planItem
		p  *planFile
	}
	for _, f := range p.files {
		if err := ctx.Err(); err != nil {
			return err
		}
		prim := f
		if f.group != "" {
			prim = primary[f.group]
		}
		r := f.rec
		var it *planItem
		switch {
		case r == nil:
			switch {
			case moveFrom[f] != nil:
				p.move(f, moveFrom[f])
			case prim != f:
				it = p.link(f, prim, nil)
			default:
				if err := p.newFile(f); err != nil {
					return err
				}
			}
		case needRepair[r.ID]:
			if p.itemOf[f] == nil { // not planned already (before its primary's repair)
				p.repair(f, r)
			}
		case relink[r.ID]:
			if prim != f {
				it = p.link(f, prim, r)
				it.change = false // a repair, not a change of the source
				it.d.Reason = "repair"
			} else {
				p.repair(f, r)
			}
		case r.State == StateMissing:
			if prim != f {
				it = p.link(f, prim, r)
			} else {
				p.repair(f, r)
			}
		case f.unchanged:
		default:
			if prim != f {
				it = p.link(f, prim, r)
				if pr := p.promoteFor[r.ID]; pr != nil {
					pr.after = it
				}
			} else {
				up := p.update(f, r)
				if pr := p.promoteFor[r.ID]; pr != nil {
					pr.after = up
				}
			}
		}
		if it != nil {
			linkItems = append(linkItems, struct {
				it *planItem
				p  *planFile
			}{it, prim})
		}
	}
	for _, l := range linkItems {
		l.it.after = p.itemOf[l.p]
	}
	return nil
}

// pairMoves finds renames (design §4.2 move): a new path (not a link of another name) whose exact
// size and mtime equal a vanished record's, and whose head/tail hash equals the head/tail hash of
// that record's destination file. Inode numbers are never compared across scans.
func (p *sourcePlanner) pairMoves(ctx context.Context, vanished []*Record, moveFrom map[*planFile]*Record, moved map[int64]bool) error {
	type sig struct{ size, mtime int64 }
	pool := map[sig][]*Record{}
	for _, v := range vanished {
		if v.State == StatePresent || v.State == StateLinked {
			k := sig{v.Size, v.MtimeNs}
			pool[k] = append(pool[k], v)
		}
	}
	if len(pool) == 0 {
		return nil
	}
	for _, f := range p.files {
		if f.rec != nil {
			continue
		}
		if f.group != "" {
			m := p.groups[f.group]
			if m[0] != f {
				continue // another name of the group holds the content; this one is a link
			}
			recorded := false
			for _, o := range m {
				recorded = recorded || o.rec != nil
			}
			if recorded {
				continue
			}
		}
		cands := pool[sig{f.size, f.mtimeNs}]
		srcHash := ""
		for _, v := range cands {
			if moved[v.ID] {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if srcHash == "" {
				h, err := p.fs.sourceHeadTail(f.rel)
				if err != nil {
					if filecopy.Classify(err) == filecopy.Fatal {
						return fmt.Errorf("read %s: %w", f.rel, err)
					}
					break // unreadable or gone: copied (and reported) like any new file
				}
				srcHash = h
			}
			dh, err := p.fs.destHeadTail(v.RelPath)
			if err != nil {
				if filecopy.Classify(err) == filecopy.Fatal {
					return fmt.Errorf("read destination %s: %w", v.RelPath, err)
				}
				continue
			}
			if dh == srcHash {
				moveFrom[f] = v
				moved[v.ID] = true
				break
			}
		}
	}
	return nil
}

// surviving returns r's live dependents whose source file is still there with the recorded
// content: they need r's content to stay live.
func (p *sourcePlanner) surviving(r *Record) []*Record {
	var out []*Record
	for _, d := range p.deps[r.ID] {
		if d.State != StateLinked && d.State != StateLinkRecorded {
			continue
		}
		if f := p.byRel[d.SourceRelPath]; f != nil && f.rec == d && f.unchanged {
			out = append(out, d)
		}
	}
	return out
}

// pickTarget chooses the dependent that takes over a primary's content: a hardlink (its file
// already holds the content) before a recorded-only link.
func pickTarget(surv []*Record) *Record {
	for _, d := range surv {
		if d.State == StateLinked {
			return d
		}
	}
	return surv[0]
}

// choosePrimary picks the name of a hardlink group that holds the content: one already present
// and unchanged, then one that a promote makes present, then a recorded one being updated or
// repaired, then the target of a move, then the first name.
func choosePrimary(members []*planFile, promoteTarget map[int64]bool, moveFrom map[*planFile]*Record) *planFile {
	for _, ok := range []func(*planFile) bool{
		func(m *planFile) bool { return m.rec != nil && m.rec.State == StatePresent && m.unchanged },
		func(m *planFile) bool { return m.rec != nil && promoteTarget[m.rec.ID] },
		func(m *planFile) bool {
			return m.rec != nil && (m.rec.State == StatePresent || m.rec.State == StateMissing)
		},
		func(m *planFile) bool { return moveFrom[m] != nil },
	} {
		for _, m := range members {
			if ok(m) {
				return m
			}
		}
	}
	return members[0]
}

// planVanished plans a record whose source path is gone and that is not a move.
func (p *sourcePlanner) planVanished(v *Record, promoteTarget, needRepair map[int64]bool) {
	if v.State == StateLinkRecorded {
		p.retain(v)
		return
	}
	surv := p.surviving(v)
	switch {
	case v.State == StateMissing:
		// The content is not (intact) at the destination: its surviving names get their own copy.
		for _, d := range surv {
			needRepair[d.ID] = true
		}
		p.retain(v)
	case len(surv) > 0:
		t := pickTarget(surv)
		pr := p.promote(v, t, "retain")
		promoteTarget[t.ID] = true
		if t.State == StateLinked {
			pr.after = p.retain(v)
		} else {
			pr.change = true // the vanished name's record goes away with the promote
		}
	default:
		p.retain(v)
	}
}

func (p *sourcePlanner) retain(v *Record) *planItem {
	it := &planItem{action: jobs.ActionRetain, rel: v.RelPath, fileID: v.ID, bytes: v.Size, change: true,
		d: Detail{SourceID: p.sourceID, Source: v.SourceRelPath, RecordID: v.ID, Size: v.Size, MtimeNs: v.MtimeNs, Reason: "vanished"}}
	p.retains = append(p.retains, it)
	return it
}

func (p *sourcePlanner) promote(v, t *Record, forWhat string) *planItem {
	it := &planItem{action: jobs.ActionPromote, rel: t.RelPath, fileID: v.ID,
		d: Detail{SourceID: p.sourceID, Source: t.SourceRelPath, RecordID: v.ID, TargetID: t.ID, From: v.RelPath, For: forWhat,
			Size: v.Size, MtimeNs: v.MtimeNs, Reason: "hardlink"}}
	p.promotes = append(p.promotes, it)
	return it
}

func (p *sourcePlanner) fileDetail(f *planFile, reason string) Detail {
	return Detail{SourceID: p.sourceID, Source: f.rel, Size: f.size, MtimeNs: f.mtimeNs, Group: f.group, Reason: reason}
}

func shrinks(newSize, oldSize int64) bool {
	return newSize < oldSize && (newSize == 0 || newSize*2 < oldSize)
}

func (p *sourcePlanner) update(f *planFile, r *Record) *planItem {
	d := p.fileDetail(f, "changed")
	d.RecordID, d.OldSize = r.ID, r.Size
	it := &planItem{action: jobs.ActionUpdate, rel: r.RelPath, fileID: f.id, bytes: f.size, d: d, change: true, shrink: shrinks(f.size, r.Size)}
	p.puts = append(p.puts, it)
	p.itemOf[f] = it
	return it
}

// repair plans a new copy of a file whose record is missing (verify found it gone or damaged, or a
// promote moved its content to another name) or whose content was only recorded as a link of a
// damaged primary. A source that shrank to less than half the recorded size is held (S10b).
func (p *sourcePlanner) repair(f *planFile, r *Record) *planItem {
	d := p.fileDetail(f, "repair")
	d.RecordID, d.OldSize = r.ID, r.Size
	it := &planItem{action: jobs.ActionCopy, rel: r.RelPath, fileID: f.id, bytes: f.size, d: d, shrink: shrinks(f.size, r.Size)}
	p.puts = append(p.puts, it)
	p.itemOf[f] = it
	return it
}

func (p *sourcePlanner) move(f *planFile, v *Record) {
	dest := p.destPath(f.rel)
	d := p.fileDetail(f, "renamed")
	d.RecordID, d.From = v.ID, v.RelPath
	it := &planItem{action: jobs.ActionMove, rel: dest, fileID: f.id, bytes: f.size, d: d}
	if err := p.names.check(dest, v.RelPath); err != nil {
		it.fail(err)
		p.warnings = append(p.warnings, err.Error())
	} else {
		p.names.add(dest)
	}
	p.moves = append(p.moves, it)
	p.itemOf[f] = it
}

// link plans another name of a hardlink group (design §4.2 link). r is the name's existing record
// (a relink of a changed or missing name) or nil.
func (p *sourcePlanner) link(f, prim *planFile, r *Record) *planItem {
	dest := p.destPath(f.rel)
	d := p.fileDetail(f, "hardlink")
	d.Primary = prim.rel
	it := &planItem{action: jobs.ActionLink, rel: dest, fileID: f.id, d: d}
	if r != nil {
		it.rel = r.RelPath
		it.d.RecordID = r.ID
		it.d.Reason = "relink"
		if r.State != StateMissing {
			it.d.OldSize = r.Size
			it.change = true
			it.shrink = shrinks(f.size, r.Size)
		}
	} else {
		if err := p.names.check(dest, ""); err != nil {
			it.fail(err)
			p.warnings = append(p.warnings, err.Error())
		} else {
			p.names.add(dest)
		}
	}
	p.links = append(p.links, it)
	return it
}

// newFile plans a file with no record: adopt what is already there (§4.4) or copy it (an
// unrecorded file in the way is displaced into retention at execution).
func (p *sourcePlanner) newFile(f *planFile) error {
	dest := p.destPath(f.rel)
	it := &planItem{action: jobs.ActionCopy, rel: dest, fileID: f.id, bytes: f.size, d: p.fileDetail(f, "new")}
	p.puts = append(p.puts, it)
	p.itemOf[f] = it
	if err := p.names.check(dest, ""); err != nil {
		it.fail(err)
		p.warnings = append(p.warnings, err.Error())
		return nil
	}
	p.names.add(dest)
	st, exists, err := p.fs.destStat(dest)
	if err != nil {
		if filecopy.Classify(err) == filecopy.Fatal {
			return fmt.Errorf("stat destination %s: %w", dest, err)
		}
		return nil // e.g. a parent that is a file: the copy reports it
	}
	if !exists {
		return nil
	}
	if !st.Regular() {
		it.fail(fmt.Errorf("%s exists at the destination and is not a regular file (%s); it is left alone", dest, st.Mode.Type()))
		p.warnings = append(p.warnings, it.errMsg)
		return nil
	}
	if adoptable(p.settings, p.caps, st, f.size, f.mtimeNs) {
		it.action = jobs.ActionAdopt
		it.d.Reason = "adopt"
		return nil
	}
	it.d.Displace = true
	return nil
}

// adoptable reports whether an unrecorded destination file may be adopted for a source file of
// the given size and mtime (§4.4). In size+hash mode the hashes are compared at execution.
func adoptable(s destinations.Settings, caps filecopy.Capabilities, st filecopy.Stat, size, mtimeNs int64) bool {
	if !st.Regular() || st.Size != size {
		return false
	}
	switch s.AdoptExisting {
	case destinations.AdoptSizeMtime:
		return filecopy.MtimeMatch(st.MtimeNs, mtimeNs, caps.MtimeGranularityNs, s.MtimeWindowSec)
	case destinations.AdoptSizeHash:
		return true
	default:
		return false
	}
}

// errNameCollision marks a case-folded collision (S11).
var errNameCollision = errors.New("name collides at the destination")

// nameIndex checks the destination paths a plan creates (S11): names the destination cannot
// store, paths recorded for another (or a removed) source, and, on case-insensitive
// destinations, paths that differ from another live path only in case.
type nameIndex struct {
	caps filecopy.Capabilities
	// foreign holds the live paths recorded for sources this job does not plan (orphans).
	foreign map[string]bool
	// folded maps a case-folded path to the live or planned path that has it (case-insensitive
	// destinations only).
	folded map[string]string
}

func newNameIndex(caps filecopy.Capabilities) *nameIndex {
	return &nameIndex{caps: caps, foreign: map[string]bool{}, folded: map[string]string{}}
}

// addRecord registers an existing live record; foreign marks one of a source not being planned.
func (n *nameIndex) addRecord(rel string, foreign bool) {
	if foreign {
		n.foreign[rel] = true
	}
	n.add(rel)
}

// add registers a live or planned path.
func (n *nameIndex) add(rel string) {
	if !n.caps.CaseInsensitive {
		return
	}
	k := filecopy.FoldKey(rel)
	if _, ok := n.folded[k]; !ok {
		n.folded[k] = rel
	}
}

// check reports why rel cannot be created (nil when it can). self is a path that may collide
// with rel because it is the same file (the old name of a move).
func (n *nameIndex) check(rel, self string) error {
	if err := filecopy.NameCheck(rel, n.caps); err != nil {
		return err
	}
	if n.foreign[rel] {
		return fmt.Errorf("%w: %q is recorded for another (or a removed) source of this destination", errNameCollision, rel)
	}
	if n.caps.CaseInsensitive {
		if other, ok := n.folded[filecopy.FoldKey(rel)]; ok && other != rel && other != self {
			return fmt.Errorf("%w: %q and %q differ only in case and the destination is case-insensitive", errNameCollision, rel, other)
		}
	}
	return nil
}
