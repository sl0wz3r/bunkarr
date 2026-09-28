package enginerun

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
	"github.com/sl0wz3r/bunkarr/internal/engines/restic"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/syncer"
)

// TestDispatchFilecopy: jobs of a filecopy destination, of a destination that is gone and the
// global retention job go to the syncer's runners unchanged; a Phase 3-style filecopy sync runs
// through Dispatch exactly as through its runner (§3.4, acceptance 8).
func TestDispatchFilecopy(t *testing.T) {
	h := newRestic(t)
	target := filepath.Join(h.base, "nas")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	fc, err := h.dests.Create(h.ctx, destinations.Input{Name: "nas", Target: target, SourceIDs: []int64{h.src.ID}},
		destinations.CreateOptions{AllowLocal: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, typ := range []jobs.Type{jobs.TypeSync, jobs.TypeVerify, jobs.TypeRetention} {
		h.mustRun(typ, false, jobs.Params{DestinationID: fc.ID})
	}
	j, _, err := h.jq.CreateJob(h.ctx, jobs.Spec{Type: jobs.TypeRetention, Trigger: jobs.TriggerManual})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.runJob(j); err != nil {
		t.Fatal(err)
	}
	h.closeJob(j.ID)
	h.mustRun(jobs.TypeSync, false, jobs.Params{DestinationID: 999})
	if n := h.fc.count(); n != 5 {
		t.Errorf("%d jobs reached the filecopy runners, want 5", n)
	}
	if n := len(h.runner.Calls()); n != 0 {
		t.Errorf("%d engine commands ran for filecopy jobs", n)
	}
	// The real syncer runner behind Dispatch: a Phase 3-style filecopy sync.
	so := syncer.Options{DB: h.db, Store: h.files, Catalog: h.cat, Scanner: h.scanner, Destinations: h.dests, Now: h.now}
	h.svc.o.Filecopy.Sync = syncer.NewSyncRunner(so)
	h.writeSrc("a.mkv", content("a", 1000), 1)
	res, _ := h.mustRun(jobs.TypeSync, false, jobs.Params{DestinationID: fc.ID})
	st, ok := res.Stats.(syncer.SyncStats)
	if !ok || st.FilesCopied != 1 {
		t.Fatalf("filecopy stats %T %+v", res.Stats, res.Stats)
	}
	if _, err := os.Stat(filepath.Join(target, h.src.DestFolder, "a.mkv")); err != nil {
		t.Errorf("the filecopy target does not hold the file: %v", err)
	}
	if n := len(h.runner.Calls()); n != 0 {
		t.Errorf("%d engine commands ran for a filecopy sync", n)
	}
}

// TestCustodyGate: no engine job but a dry run runs before the recovery kit custody is
// confirmed (S21); a dry run checks the identity read-only and writes nothing.
func TestCustodyGate(t *testing.T) {
	h := newHarness(t, harnessOptions{kind: engines.Restic, unconfirmed: true})
	h.writeSrc("a.mkv", content("a", 1000), 1)
	for _, typ := range []jobs.Type{jobs.TypeSync, jobs.TypeVerify, jobs.TypeRetention} {
		j := h.newJob(typ, false, jobs.Params{})
		_, err := h.runJob(j)
		h.closeJob(j.ID)
		if err == nil || !strings.Contains(err.Error(), destinations.BlockedKit) {
			t.Errorf("%s: err = %v", typ, err)
		}
	}
	if n := len(h.runner.Calls()); n != 0 {
		t.Errorf("%d commands ran before custody was confirmed", n)
	}
	_, j := h.mustRun(jobs.TypeSync, true, jobs.Params{})
	if n := len(h.items(j.ID)); n != 1 {
		t.Errorf("the dry run planned %d items", n)
	}
	for _, c := range h.runner.Calls() {
		if c.Subcommand() != "cat" || !c.Has("--no-lock") {
			t.Errorf("a dry run ran %s", c)
		}
	}
}

// TestGateRefusals: a pending create, a disabled destination and an unavailable engine run no
// job (S25, §11.1).
func TestGateRefusals(t *testing.T) {
	h := newRestic(t)
	h.exec(`UPDATE destinations SET marker_id = 'pending:1234' WHERE id = ?`, h.dest.ID)
	j := h.newJob(jobs.TypeSync, false, jobs.Params{})
	if _, err := h.runJob(j); !errors.Is(err, engines.ErrPending) {
		t.Errorf("pending: %v", err)
	}
	h.closeJob(j.ID)
	h.exec(`UPDATE destinations SET marker_id = ?, enabled = 0 WHERE id = ?`, "restic:"+h.restic.id, h.dest.ID)
	j = h.newJob(jobs.TypeSync, false, jobs.Params{})
	if _, err := h.runJob(j); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("disabled: %v", err)
	}
	h.closeJob(j.ID)
	h.exec(`UPDATE destinations SET enabled = 1 WHERE id = ?`, h.dest.ID)
	h.avail.Restic = engines.BinaryStatus{Reason: "restic is not installed"}
	j = h.newJob(jobs.TypeSync, false, jobs.Params{})
	if _, err := h.runJob(j); !errors.Is(err, engines.ErrEngineUnavailable) {
		t.Errorf("unavailable: %v", err)
	}
	h.closeJob(j.ID)
	if _, ok := h.svc.Engines()(engines.Restic); ok {
		t.Error("Engines returned an unavailable engine")
	}
	if e, ok := h.svc.Engines()(engines.Rclone); !ok || e.Kind() != engines.Rclone {
		t.Error("Engines did not return rclone")
	}
	if len(h.runner.Calls()) != 0 {
		t.Error("commands ran for refused jobs")
	}
}

// TestIdentityMismatch: another repository at the location fails the job before anything is
// written (S25).
func TestIdentityMismatch(t *testing.T) {
	h := newRestic(t)
	h.writeSrc("a.mkv", content("a", 1000), 1)
	h.restic.id = strings.Repeat("cd", 32)
	j := h.newJob(jobs.TypeSync, false, jobs.Params{})
	if _, err := h.runJob(j); !errors.Is(err, engines.ErrAnotherRepository) {
		t.Fatalf("err = %v", err)
	}
	for _, c := range h.runner.Calls() {
		if c.Subcommand() != "cat" {
			t.Errorf("%s ran after the identity check failed", c)
		}
	}
}

// TestOpenVersionsAndForgetRequests: the config runners' VersionStore needs custody (dry runs
// excepted); on restic Remove records an engine_forget request in the caller's transaction.
func TestOpenVersionsAndForgetRequests(t *testing.T) {
	h := newHarness(t, harnessOptions{kind: engines.Restic, unconfirmed: true})
	if _, _, err := h.svc.OpenVersions(h.ctx, h.dest.ID, engines.Runtime{JobID: 1}); err == nil ||
		!strings.Contains(err.Error(), destinations.BlockedKit) {
		t.Fatalf("OpenVersions before custody: %v", err)
	}
	vs, closer, err := h.svc.OpenVersions(h.ctx, h.dest.ID, engines.Runtime{JobID: 1, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	id := strings.Repeat("e", 64)
	if err := h.db.Write(h.ctx, func(tx *sql.Tx) error {
		return vs.Remove(h.ctx, tx, engines.Ref(id), engines.VersionPlexDB)
	}); err != nil {
		t.Fatal(err)
	}
	reqs, err := h.svc.forgetRequests(h.ctx, h.dest.ID)
	if err != nil || len(reqs) != 1 || reqs[0].SnapshotID != id || reqs[0].Kind != engines.VersionPlexDB {
		t.Errorf("requests %+v (%v)", reqs, err)
	}
	if err := h.db.Write(h.ctx, func(tx *sql.Tx) error {
		return h.svc.RequestForget(h.ctx, tx, h.dest.ID, "not-an-id", engines.VersionArr, "")
	}); err == nil {
		t.Error("a bad snapshot id was accepted")
	}
	if err := h.db.Write(h.ctx, func(tx *sql.Tx) error {
		return h.svc.RequestForget(h.ctx, tx, h.dest.ID, id, engines.VersionMedia, "")
	}); err == nil {
		t.Error("a media forget request was accepted")
	}
}

// TestUnlock: POST /destinations/{id}/unlock checks the identity first, then runs the guarded
// unlock, or restic unlock --remove-all when asked (§6.7).
func TestUnlock(t *testing.T) {
	h := newRestic(t)
	h.restic.locks[strings.Repeat("1", 64)] = resticLock(h, "other-host")
	if err := h.svc.Unlock(h.ctx, h.dest.ID, false); err != nil {
		t.Fatal(err)
	}
	calls := h.runner.Calls()
	if calls[0].Subcommand() != "cat" || !calls[0].Has("--no-lock") {
		t.Errorf("first command %s, want the identity check", calls[0])
	}
	last := calls[len(calls)-1]
	if last.Subcommand() != "unlock" || last.Has("--remove-all") {
		t.Errorf("last command %s", last)
	}
	if err := h.svc.Unlock(h.ctx, h.dest.ID, true); err != nil {
		t.Fatal(err)
	}
	u := h.callsOf(proc.Restic, "unlock")
	if !u[len(u)-1].Has("--remove-all") {
		t.Error("removeAll did not run unlock --remove-all")
	}
	rc := newRclone(t)
	if err := rc.svc.Unlock(rc.ctx, rc.dest.ID, false); err == nil {
		t.Error("unlock of an rclone destination")
	}
}

// TestStateAndList: engine_state records the engine version; List serves the recorded media
// snapshots.
func TestStateAndList(t *testing.T) {
	h := newRestic(t)
	h.firstSnapshot(map[string]string{"a.mkv": content("a", 1000)})
	st, err := h.svc.State(h.ctx, h.dest.ID)
	if err != nil || st.EngineVersion != "restic 0.18.1" || st.ReadSubsetNext != 1 {
		t.Errorf("state %+v (%v)", st, err)
	}
	if got := string(st.Stats[StatSnapshotCount]); got != "1" {
		t.Errorf("snapshotCount %q, want 1", got)
	}
	// The derived count is never written back into engine_state.
	var raw string
	if err := h.db.Reader().QueryRowContext(h.ctx, `SELECT stats FROM engine_state WHERE destination_id = ?`, h.dest.ID).Scan(&raw); err != nil ||
		strings.Contains(raw, StatSnapshotCount) {
		t.Errorf("stored stats %s (%v)", raw, err)
	}
	list, err := h.svc.List(h.ctx, h.dest.ID)
	if err != nil || len(list) != 1 || list[0].Kind != engines.VersionMedia || !list[0].Complete || list[0].SourceID != h.src.ID {
		t.Errorf("list %+v (%v)", list, err)
	}
}

// TestSecretsRedacted: a secret a child prints never reaches an item's error or the job log
// (S22).
func TestSecretsRedacted(t *testing.T) {
	h := newRclone(t)
	const secret = "s3cr3t/enginerun-access-key-0001"
	h.writeSrc("a.mkv", content("a", 1000), 1)
	h.rclone.copyErr["a.mkv"] = "denied with key " + secret
	_, j := h.mustSync(jobs.Params{})
	for _, it := range h.items(j.ID) {
		if strings.Contains(it.Error, secret) {
			t.Errorf("item error holds the secret: %q", it.Error)
		}
	}
	if strings.Contains(h.rep.dump(), secret) {
		t.Error("the job log holds the secret")
	}
}

// resticLock is a lock of another host, older than restic's staleness limit.
func resticLock(h *harness, host string) restic.Lock {
	return restic.Lock{Time: h.now().Add(-2 * time.Hour), Hostname: host, PID: 42}
}
