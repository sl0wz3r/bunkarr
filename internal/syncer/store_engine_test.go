package syncer

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
)

// write runs fn in a write transaction of the harness's database.
func (h *harness) write(fn func(tx *sql.Tx) error) {
	h.t.Helper()
	if err := h.db.Write(h.ctx, fn); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) record(id int64) Record {
	h.t.Helper()
	r, err := h.store.Get(h.ctx, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

func TestStoreEngineReferences(t *testing.T) {
	h := newHarness(t)
	now := h.now()
	m := baseTime.Add(time.Second)
	a := h.engineRecord("a.mkv", 10, m, StatePresent, 0, "", "")
	b := h.engineRecord("b.mkv", 20, m, StatePresent, 0, "", "S0")
	c := h.engineRecord("c.mkv", 30, m, StatePresent, 0, "", "")
	d := h.engineRecord("d.mkv", 40, m, StatePresent, 0, "", "")

	// A batch updates a.mkv: its old version gets a replaced row beside the live row (the old
	// record had no reference, so it is in the base S1), then the live row takes the new version.
	h.write(func(tx *sql.Tx) error {
		if _, err := h.store.InsertRetainedTx(h.ctx, tx, RetainedVersion{DestinationID: h.dest.ID, SourceID: h.src.ID, RelPath: a.RelPath,
			SourceRelPath: "a.mkv", Size: 10, MtimeNs: m.UnixNano(), EngineRef: "S1", RetainedPath: "/src/a.mkv", Reason: ReasonReplaced,
			RetainedAt: now, ExpiresAt: now.Add(30 * 24 * time.Hour)}); err != nil {
			return err
		}
		_, err := h.store.RecordContentTx(h.ctx, tx, ContentDone{DestinationID: h.dest.ID, SourceID: h.src.ID, RecordID: a.ID,
			RelPath: a.RelPath, SourceRelPath: "a.mkv", Size: 11, MtimeNs: m.UnixNano() + 5, HeadTail: "headtail-sha256:aa", State: StatePresent,
			JobID: 9, CopiedAt: now})
		return err
	})
	recs := h.records()
	var live, replaced int
	for _, r := range recs {
		if r.SourceRelPath == "a.mkv" {
			switch r.State {
			case StatePresent:
				live++
				if r.Size != 11 || r.HeadTail != "headtail-sha256:aa" || r.EngineRef != "" || r.JobID != 9 {
					t.Errorf("live a %+v", r)
				}
			case StateRetained:
				replaced++
				if r.EngineRef != "S1" || r.Reason != ReasonReplaced || r.RelPath != a.RelPath {
					t.Errorf("replaced a %+v", r)
				}
			}
		}
	}
	if live != 1 || replaced != 1 {
		t.Fatalf("a.mkv: %d live, %d replaced rows", live, replaced)
	}
	// The insert is idempotent.
	h.write(func(tx *sql.Tx) error {
		_, err := h.store.InsertRetainedTx(h.ctx, tx, RetainedVersion{DestinationID: h.dest.ID, SourceID: h.src.ID, RelPath: a.RelPath,
			SourceRelPath: "a.mkv", Size: 10, EngineRef: "S1", RetainedPath: "/src/a.mkv", Reason: ReasonReplaced, RetainedAt: now,
			ExpiresAt: now.Add(time.Hour)})
		return err
	})
	if n := len(h.records()); n != len(recs) {
		t.Errorf("a second insert added a row (%d → %d)", len(recs), n)
	}

	// The read-back rule after batch N (base before it: S1): N holds a (new version) and c; not b
	// (whose reference S0 must stay) and not d (which gets the base).
	holds := map[string]bool{"a.mkv": true, "c.mkv": true}
	var cleared, pinned int64
	h.write(func(tx *sql.Tx) error {
		var err error
		cleared, pinned, err = h.store.ApplyReadBackTx(h.ctx, tx, ReadBack{DestinationID: h.dest.ID, SourceID: h.src.ID, Base: "S1",
			Holds: func(r Record) bool { return holds[r.SourceRelPath] }})
		return err
	})
	if cleared != 2 || pinned != 2 {
		t.Errorf("cleared %d pinned %d", cleared, pinned)
	}
	if r := h.record(b.ID); r.EngineRef != "S0" {
		t.Errorf("b's reference moved to %q", r.EngineRef)
	}
	if r := h.record(d.ID); r.EngineRef != "S1" {
		t.Errorf("d's reference %q, want the base", r.EngineRef)
	}
	if r := h.record(c.ID); r.EngineRef != "" {
		t.Errorf("c's reference %q, want NULL", r.EngineRef)
	}
	// A later batch N2 (base N) holds nothing: the set references never move.
	h.write(func(tx *sql.Tx) error {
		_, _, err := h.store.ApplyReadBackTx(h.ctx, tx, ReadBack{DestinationID: h.dest.ID, SourceID: h.src.ID, Base: "N",
			Holds: func(Record) bool { return false }})
		return err
	})
	for id, want := range map[int64]string{a.ID: "N", b.ID: "S0", c.ID: "N", d.ID: "S1"} {
		if r := h.record(id); r.EngineRef != want {
			t.Errorf("record %s: reference %q, want %q", r.SourceRelPath, r.EngineRef, want)
		}
	}
	// The table form: a temporary table of the snapshot's files (absolute paths).
	h.write(func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(h.ctx, `CREATE TEMP TABLE readback_t (path TEXT PRIMARY KEY, size INTEGER, mtime_ns INTEGER)`); err != nil {
			return err
		}
		defer tx.ExecContext(h.ctx, `DROP TABLE temp.readback_t`) //nolint:errcheck
		// b held to the second only (the listing has no fraction), d with another size.
		if _, err := tx.ExecContext(h.ctx, `INSERT INTO readback_t VALUES (?, ?, ?), (?, ?, ?)`,
			"/data/src/b.mkv", 20, m.Truncate(time.Second).UnixNano(), "/data/src/d.mkv", 41, m.UnixNano()); err != nil {
			return err
		}
		cl, pn, err := h.store.ApplyReadBackTx(h.ctx, tx, ReadBack{DestinationID: h.dest.ID, SourceID: h.src.ID, Base: "N2",
			Table: "temp.readback_t", Root: "/data/src"})
		if err == nil && (cl != 1 || pn != 3) {
			t.Errorf("table read-back cleared %d pinned %d", cl, pn)
		}
		return err
	})
	if r := h.record(b.ID); r.EngineRef != "" {
		t.Errorf("b after the table read-back: %q", r.EngineRef)
	}
	if r := h.record(d.ID); r.EngineRef != "S1" {
		t.Errorf("d after the table read-back: %q", r.EngineRef)
	}
	if _, _, err := h.store.ApplyReadBackTx(h.ctx, nil, ReadBack{Table: "x; DROP TABLE jobs"}); err == nil {
		t.Error("a table name with SQL was accepted")
	}

	// Retain d (a release decided after the last batch): its reference stays S1 (COALESCE).
	h.write(func(tx *sql.Tx) error {
		return h.store.RetainTx(h.ctx, tx, d.ID, Retain{Base: "N2", RetainedPath: "/src/d.mkv", Reason: ReasonDeleted, JobID: 3,
			RetainedAt: now, ExpiresAt: now.Add(time.Hour)})
	})
	if r := h.record(d.ID); r.State != StateRetained || r.EngineRef != "S1" || r.RetainedPath != "/src/d.mkv" || r.ExpiresAt == nil {
		t.Errorf("retained d %+v", r)
	}
	// Retaining c (NULL reference) takes the base.
	h.write(func(tx *sql.Tx) error {
		return h.store.RetainTx(h.ctx, tx, c.ID, Retain{Base: "N2", RetainedPath: "/src/c.mkv", Reason: ReasonDeleted, RetainedAt: now,
			ExpiresAt: now.Add(time.Hour)})
	})
	if r := h.record(c.ID); r.EngineRef != "N" {
		t.Errorf("retained c reference %q (it had N)", r.EngineRef)
	}
	err := h.db.Write(h.ctx, func(tx *sql.Tx) error {
		return h.store.RetainTx(h.ctx, tx, c.ID, Retain{RetainedPath: "x", RetainedAt: now, ExpiresAt: now})
	})
	if !errors.Is(err, ErrRecordChanged) {
		t.Errorf("retaining a retained row: %v", err)
	}

	refs, err := h.store.Refs(h.ctx, h.dest.ID)
	if err != nil {
		t.Fatal(err)
	}
	// (b's S0 was cleared by the table read-back, which found b's version in N2.)
	if !slices.Equal(refs, []string{"N", "S1"}) {
		t.Errorf("refs %v", refs)
	}

	// Snapshot S1 removed outside Bunkarr: its retained rows are lost, its live records missing.
	e := h.engineRecord("e.mkv", 50, m, StatePresent, 0, "", "S1")
	var lost []Record
	h.write(func(tx *sql.Tx) error {
		var err error
		if lost, err = h.store.DeleteRetainedByRefTx(h.ctx, tx, h.dest.ID, "S1"); err != nil {
			return err
		}
		n, err := h.store.MarkMissingByRefTx(h.ctx, tx, h.dest.ID, "S1")
		if err == nil && n != 1 {
			t.Errorf("marked %d missing by reference", n)
		}
		return err
	})
	if len(lost) != 2 {
		t.Errorf("lost %d retained versions, want a's replaced row and d", len(lost))
	}
	if r := h.record(e.ID); r.State != StateMissing || r.EngineRef != "" {
		t.Errorf("e after its snapshot vanished: %+v", r)
	}
	// The base vanished: the source's NULL records become missing.
	f := h.engineRecord("f.mkv", 60, m, StatePresent, 0, "", "")
	h.write(func(tx *sql.Tx) error {
		_, err := h.store.MarkMissingBaseTx(h.ctx, tx, h.dest.ID, h.src.ID)
		return err
	})
	if r := h.record(f.ID); r.State != StateMissing {
		t.Errorf("f %+v", r)
	}
	if r := h.record(a.ID); r.State != StatePresent || r.EngineRef != "N" {
		t.Errorf("a has a reference and stays present: %+v", r)
	}
}

func TestStoreEngineContentAndIntents(t *testing.T) {
	h := newHarness(t)
	now := h.now()
	m := baseTime.Add(time.Second)
	// A new path, then a move of it.
	var id int64
	h.write(func(tx *sql.Tx) error {
		var err error
		id, err = h.store.RecordContentTx(h.ctx, tx, ContentDone{DestinationID: h.dest.ID, SourceID: h.src.ID, RelPath: "movies/n.mkv",
			SourceRelPath: "n.mkv", Size: 5, MtimeNs: m.UnixNano(), HeadTail: "ht", State: StatePresent, CopiedAt: now})
		return err
	})
	h.write(func(tx *sql.Tx) error {
		_, err := tx.ExecContext(h.ctx, `UPDATE destination_files SET hash = 'sha256:x' WHERE id = ?`, id)
		return err
	})
	h.write(func(tx *sql.Tx) error {
		_, err := h.store.RecordContentTx(h.ctx, tx, ContentDone{DestinationID: h.dest.ID, SourceID: h.src.ID, RecordID: id,
			RelPath: "movies/moved/n.mkv", SourceRelPath: "moved/n.mkv", Size: 5, MtimeNs: m.UnixNano(), HeadTail: "ht", State: StatePresent,
			CopiedAt: now, Moved: true})
		return err
	})
	r := h.record(id)
	if r.RelPath != "movies/moved/n.mkv" || r.Hash != "sha256:x" || r.HeadTail != "ht" {
		t.Errorf("moved %+v", r)
	}
	// A link_recorded name (rclone) of it, then its intent round trip.
	var link int64
	h.write(func(tx *sql.Tx) error {
		var err error
		link, err = h.store.RecordContentTx(h.ctx, tx, ContentDone{DestinationID: h.dest.ID, SourceID: h.src.ID, RelPath: "movies/l.mkv",
			SourceRelPath: "l.mkv", Size: 5, MtimeNs: m.UnixNano(), State: StateLinkRecorded, LinkOf: id, CopiedAt: now})
		return err
	})
	if r := h.record(link); r.State != StateLinkRecorded || r.LinkOf != id {
		t.Errorf("link %+v", r)
	}
	if err := h.db.Write(h.ctx, func(tx *sql.Tx) error {
		return h.store.RetainTx(h.ctx, tx, id, Retain{RetainedPath: "x", RetainedAt: now, ExpiresAt: now})
	}); !errors.Is(err, ErrRecordChanged) {
		t.Errorf("retain of a primary with a live link: %v", err)
	}
	h.write(func(tx *sql.Tx) error {
		return h.store.SetIntentTx(h.ctx, tx, h.dest.ID, "movies/moved/n.mkv", ".bunkarr/retention/r1/movies/moved/n.mkv", ReasonReplaced)
	})
	intents, err := h.store.Intents(h.ctx, h.dest.ID)
	if err != nil || len(intents) != 1 || intents[0].ID != id {
		t.Fatalf("intents %+v %v", intents, err)
	}
	if err := h.db.Write(h.ctx, func(tx *sql.Tx) error {
		return h.store.SetIntentTx(h.ctx, tx, h.dest.ID, "movies/none.mkv", "x", ReasonReplaced)
	}); !errors.Is(err, ErrRecordChanged) {
		t.Errorf("intent without a record: %v", err)
	}
	// Recording the update clears the intent.
	h.write(func(tx *sql.Tx) error {
		_, err := h.store.RecordContentTx(h.ctx, tx, ContentDone{DestinationID: h.dest.ID, SourceID: h.src.ID, RecordID: id,
			RelPath: "movies/moved/n.mkv", SourceRelPath: "moved/n.mkv", Size: 6, MtimeNs: m.UnixNano() + 1, State: StatePresent, CopiedAt: now})
		return err
	})
	if r := h.record(id); r.RetainedPath != "" || r.Reason != "" || r.Hash != "" || r.Size != 6 {
		t.Errorf("updated %+v", r)
	}
	// Another source's path is refused.
	if err := h.db.Write(h.ctx, func(tx *sql.Tx) error {
		_, err := h.store.RecordContentTx(h.ctx, tx, ContentDone{DestinationID: h.dest.ID, SourceID: h.src.ID + 1, RelPath: "movies/l.mkv",
			SourceRelPath: "l.mkv", Size: 1, State: StatePresent, CopiedAt: now})
		return err
	}); !errors.Is(err, ErrRecordChanged) {
		t.Errorf("another source's path: %v", err)
	}
	// Retention runs and the link manifest.
	h.write(func(tx *sql.Tx) error {
		for _, p := range []string{".bunkarr/retention/20260101T000000Z-job1/movies/a", ".bunkarr/retention/20260101T000000Z-job1/movies/b",
			".bunkarr/retention/20260102T000000Z-job2/movies/c"} {
			if _, err := h.store.InsertRetainedTx(h.ctx, tx, RetainedVersion{DestinationID: h.dest.ID, SourceID: h.src.ID, RelPath: "movies/x",
				SourceRelPath: p, Size: 7, RetainedPath: p, Reason: ReasonDeleted, RetainedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
				return err
			}
		}
		return nil
	})
	runs, err := h.store.RetentionRuns(h.ctx, h.dest.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].Path != filecopy.RetentionRoot+"/20260101T000000Z-job1" || runs[0].Files != 2 || runs[0].Bytes != 14 || runs[1].Files != 1 {
		t.Errorf("runs %+v", runs)
	}
	man, err := h.store.LinkManifest(h.ctx, h.dest.ID)
	if err != nil || !strings.Contains(string(man), "movies/l.mkv\tmovies/moved/n.mkv\tlink_recorded\n") {
		t.Errorf("manifest %q %v", man, err)
	}
	// A vanished link_recorded name is deleted (it has no object).
	h.write(func(tx *sql.Tx) error { return h.store.DeleteRecordTx(h.ctx, tx, link) })
	if _, err := h.store.Get(h.ctx, link); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted link: %v", err)
	}
	h.write(func(tx *sql.Tx) error { return h.store.SetHeadTailTx(h.ctx, tx, id, "ht2") })
	h.write(func(tx *sql.Tx) error { return h.store.PinTx(h.ctx, tx, id, "B1") })
	h.write(func(tx *sql.Tx) error { return h.store.PinTx(h.ctx, tx, id, "B2") })
	if r := h.record(id); r.HeadTail != "ht2" || r.EngineRef != "B1" {
		t.Errorf("head tail / pin %+v", r)
	}
	h.write(func(tx *sql.Tx) error { return h.store.MarkMissingTx(h.ctx, tx, id) })
	if r := h.record(id); r.State != StateMissing || r.EngineRef != "B1" {
		t.Errorf("missing %+v", r)
	}
}

// TestStoreEngineMoveTakesOldVersions: a moved live record takes its replaced and damaged versions
// at its old path along (the replaced-version hold then follows it), but not those of the file the
// path held before it (a deleted row there ends its history), nor their retained_path or
// engine_ref (§3.3, §6.5, §7.5).
func TestStoreEngineMoveTakesOldVersions(t *testing.T) {
	h := newHarness(t)
	now := h.now()
	m := baseTime.Add(time.Second).UnixNano()
	retain := func(reason string, at time.Time, ref string) int64 {
		var id int64
		h.write(func(tx *sql.Tx) error {
			var err error
			id, err = h.store.InsertRetainedTx(h.ctx, tx, RetainedVersion{DestinationID: h.dest.ID, SourceID: h.src.ID, RelPath: "movies/p.mkv",
				SourceRelPath: "p.mkv", Size: 3, RetainedPath: "/src/p.mkv@" + at.Format(time.RFC3339), EngineRef: ref, Reason: reason,
				RetainedAt: at, ExpiresAt: at.Add(time.Hour)})
			return err
		})
		return id
	}
	// The path's earlier file: updated, then deleted (its row retained after its replaced one).
	older := retain(ReasonReplaced, now.Add(-4*time.Hour), "snap0")
	deleted := retain(ReasonDeleted, now.Add(-3*time.Hour), "snap1")
	var id int64
	h.write(func(tx *sql.Tx) error {
		var err error
		id, err = h.store.RecordContentTx(h.ctx, tx, ContentDone{DestinationID: h.dest.ID, SourceID: h.src.ID, RelPath: "movies/p.mkv",
			SourceRelPath: "p.mkv", Size: 5, MtimeNs: m, State: StatePresent, CopiedAt: now})
		return err
	})
	// The live record's own old versions.
	replaced := retain(ReasonReplaced, now.Add(-2*time.Hour), "snap2")
	damaged := retain(ReasonDamaged, now.Add(-time.Hour), "snap3")
	h.write(func(tx *sql.Tx) error {
		_, err := h.store.RecordContentTx(h.ctx, tx, ContentDone{DestinationID: h.dest.ID, SourceID: h.src.ID, RecordID: id,
			RelPath: "movies/q.mkv", SourceRelPath: "q.mkv", Size: 5, MtimeNs: m, State: StatePresent, CopiedAt: now, Moved: true})
		return err
	})
	for _, c := range []struct {
		id            int64
		rel, src, ref string
	}{
		{older, "movies/p.mkv", "p.mkv", "snap0"}, {deleted, "movies/p.mkv", "p.mkv", "snap1"},
		{replaced, "movies/q.mkv", "q.mkv", "snap2"}, {damaged, "movies/q.mkv", "q.mkv", "snap3"},
	} {
		r := h.record(c.id)
		if r.RelPath != c.rel || r.SourceRelPath != c.src || r.EngineRef != c.ref || !strings.HasPrefix(r.RetainedPath, "/src/p.mkv@") {
			t.Errorf("row %d (%s): %+v", c.id, r.Reason, r)
		}
	}
}
