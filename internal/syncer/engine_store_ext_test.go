package syncer

import (
	"database/sql"
	"errors"
	"testing"
	"time"
)

// TestPromoteTx: a promote makes the surviving name hold the primary's content (§7.3 step 1): the
// target is present without a link and takes the primary's hash and head_tail, other dependents
// follow it; with moved, the primary is deleted (retain) or missing (update); without (restic),
// the primary stays for its own item; a recorded promote is a no-op and a target that is no
// dependent is refused.
func TestPromoteTx(t *testing.T) {
	m := baseTime
	cases := []struct {
		name    string
		moved   bool
		forWhat string
		// wantPrimary is the primary's state afterwards ("" = deleted).
		wantPrimary State
	}{
		{"moved for retain", true, PromoteForRetain, ""},
		{"moved for update", true, PromoteForUpdate, StateMissing},
		{"record only", false, PromoteForRetain, StatePresent},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			p := h.engineRecord("p.mkv", 100, m, StatePresent, 0, "headtail-sha256:pp", "")
			h.write(func(tx *sql.Tx) error {
				_, err := tx.ExecContext(h.ctx, `UPDATE destination_files SET hash = 'sha256:ab' WHERE id = ?`, p.ID)
				return err
			})
			t1 := h.engineRecord("t1.mkv", 100, m, StateLinkRecorded, p.ID, "", "")
			t2 := h.engineRecord("t2.mkv", 100, m, StateLinkRecorded, p.ID, "", "")
			at := h.now()
			h.write(func(tx *sql.Tx) error { return h.store.PromoteTx(h.ctx, tx, p.ID, t1.ID, c.moved, c.forWhat, 7, at) })
			got := h.record(t1.ID)
			if got.State != StatePresent || got.LinkOf != 0 || got.Hash != "sha256:ab" || got.HeadTail != "headtail-sha256:pp" || got.JobID != 7 {
				t.Errorf("target %+v", got)
			}
			if o := h.record(t2.ID); o.LinkOf != t1.ID {
				t.Errorf("the other dependent links to %d, want %d", o.LinkOf, t1.ID)
			}
			pr, err := h.store.Get(h.ctx, p.ID)
			switch {
			case c.wantPrimary == "" && !errors.Is(err, ErrNotFound):
				t.Errorf("the primary is still recorded: %+v", pr)
			case c.wantPrimary != "" && (err != nil || pr.State != c.wantPrimary):
				t.Errorf("primary %+v (%v), want %s", pr, err, c.wantPrimary)
			}
			// Again: a recorded promote changes nothing.
			h.write(func(tx *sql.Tx) error { return h.store.PromoteTx(h.ctx, tx, p.ID, t1.ID, c.moved, c.forWhat, 8, at) })
			if again := h.record(t1.ID); again.JobID != 7 {
				t.Errorf("a repeated promote rewrote the target: %+v", again)
			}
		})
	}
	h := newHarness(t)
	p := h.engineRecord("p.mkv", 100, m, StatePresent, 0, "", "")
	q := h.engineRecord("q.mkv", 100, m, StatePresent, 0, "", "")
	other := h.engineRecord("o.mkv", 100, m, StateLinkRecorded, q.ID, "", "")
	err := h.db.Write(h.ctx, func(tx *sql.Tx) error {
		return h.store.PromoteTx(h.ctx, tx, p.ID, other.ID, true, PromoteForRetain, 1, h.now())
	})
	if !errors.Is(err, ErrNotDependent) {
		t.Errorf("a target that is no dependent: %v", err)
	}
}

// TestVerifyMarksAndCandidates: the sample takes missing records first, then the least recently
// verified (never verified first); baseOnly keeps the NULL references; a match records
// verified_at and the hash (keeping an existing one) and sets a missing record present again.
func TestVerifyMarksAndCandidates(t *testing.T) {
	h := newHarness(t)
	m := baseTime
	a := h.engineRecord("a.mkv", 10, m, StatePresent, 0, "", "")
	h.engineRecord("b.mkv", 20, m, StatePresent, 0, "", "")
	c := h.engineRecord("c.mkv", 30, m, StateMissing, 0, "", "")
	d := h.engineRecord("d.mkv", 40, m, StatePresent, 0, "", "S1")
	h.engineRecord("e.mkv", 50, m, StateLinkRecorded, a.ID, "", "")
	h.write(func(tx *sql.Tx) error { return h.store.SetVerifiedTx(h.ctx, tx, a.ID, "sha256:aa", h.now()) })
	got, err := h.store.VerifyCandidates(h.ctx, h.dest.ID, true, true, 10)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, r := range got {
		order = append(order, r.SourceRelPath)
	}
	if want := []string{"c.mkv", "b.mkv", "a.mkv"}; !equalStrings(order, want) {
		t.Errorf("candidates %v, want %v", order, want)
	}
	all, _ := h.store.VerifyCandidates(h.ctx, h.dest.ID, false, false, 10)
	if len(all) != 3 || all[len(all)-1].ID != a.ID {
		t.Errorf("present candidates %+v", all)
	}
	files, bytes, err := h.store.CountPresent(h.ctx, h.dest.ID, true)
	if err != nil || files != 2 || bytes != 30 {
		t.Errorf("CountPresent = %d, %d, %v", files, bytes, err)
	}
	h.write(func(tx *sql.Tx) error {
		if err := h.store.SetVerifiedTx(h.ctx, tx, a.ID, "sha256:other", h.now().Add(time.Hour)); err != nil {
			return err
		}
		if err := h.store.SetVerifiedTx(h.ctx, tx, c.ID, "sha256:cc", h.now()); err != nil {
			return err
		}
		return h.store.RestorePresentTx(h.ctx, tx, c.ID)
	})
	if r := h.record(a.ID); r.Hash != "sha256:aa" || r.VerifiedAt == nil {
		t.Errorf("a %+v: the recorded hash must stay", r)
	}
	if r := h.record(c.ID); r.State != StatePresent || r.Hash != "sha256:cc" {
		t.Errorf("c %+v", r)
	}
	if r, ok, err := h.store.LiveBySourcePath(h.ctx, h.dest.ID, h.src.ID, "d.mkv"); err != nil || !ok || r.ID != d.ID {
		t.Errorf("LiveBySourcePath = %+v, %v, %v", r, ok, err)
	}
	if rows, err := h.store.RetainedBySourcePath(h.ctx, h.dest.ID, h.src.ID, "a.mkv"); err != nil || len(rows) != 0 {
		t.Errorf("RetainedBySourcePath = %+v, %v", rows, err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
