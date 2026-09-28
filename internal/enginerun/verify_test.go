package enginerun

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/syncer"
)

func verifyStats(t *testing.T, res jobs.Result) VerifyStats {
	t.Helper()
	st, ok := res.Stats.(VerifyStats)
	if !ok {
		t.Fatalf("stats are %T", res.Stats)
	}
	return st
}

// TestResticVerifySubsetRotation: restic check reads the rotating subset n/t with t =
// ceil(100/samplePercent) (engine_state.read_subset_next), everything with params.readData, and
// the structure only in off mode; the guarded unlock precedes it (§6.6).
func TestResticVerifySubsetRotation(t *testing.T) {
	h := newRestic(t)
	h.settings(func(s *destinations.Settings) { s.Verify.Mode, s.Verify.SamplePercent = destinations.VerifySample, 25 })
	h.firstSnapshot(map[string]string{"a.mkv": content("a", 1000)})
	var subsets []string
	for range 5 {
		from := len(h.runner.Calls())
		res, _ := h.mustRun(jobs.TypeVerify, false, jobs.Params{})
		st := verifyStats(t, res)
		subsets = append(subsets, st.Check.ReadSubset)
		if u, c := h.callIndex("unlock", from), h.callIndex("check", from); u < 0 || u > c {
			t.Errorf("unlock %d, check %d", u, c)
		}
		h.advance(time1h)
	}
	if got := strings.Join(subsets, " "); got != "1/4 2/4 3/4 4/4 1/4" {
		t.Errorf("subsets %s", got)
	}
	res, _ := h.mustRun(jobs.TypeVerify, false, jobs.Params{ReadData: true})
	if verifyStats(t, res).Check.ReadSubset != "all" {
		t.Error("readData did not read everything")
	}
	last := h.callsOf(proc.Restic, "check")
	if !last[len(last)-1].Has("--read-data") {
		t.Error("no --read-data")
	}
	h.settings(func(s *destinations.Settings) { s.Verify.Mode = destinations.VerifyOff })
	res, j := h.mustRun(jobs.TypeVerify, false, jobs.Params{})
	c := h.callsOf(proc.Restic, "check")
	if cc := c[len(c)-1]; cc.Has("--read-data") || cc.Has("--read-data-subset") {
		t.Errorf("off mode read data: %s", cc)
	}
	if n := len(h.items(j.ID)); n != 1 || verifyStats(t, res).SampleFiles != 0 {
		t.Errorf("off mode sampled files: %d items", n)
	}
}

// TestResticVerifySample: the least recently verified present records whose source is unchanged
// are restored from their base and hashed: a match records verified_at and the hash, a mismatch
// marks the record missing and warns; check errors fail the check's item and warn.
func TestResticVerifySample(t *testing.T) {
	h := newRestic(t)
	h.settings(func(s *destinations.Settings) { s.Verify.Mode, s.Verify.SamplePercent = destinations.VerifySample, 50 })
	h.firstSnapshot(map[string]string{"a.mkv": content("a", 1000), "b.mkv": content("b", 1100), "c.mkv": content("c", 1200),
		"d.mkv": content("d", 1300)})
	res, j1 := h.mustRun(jobs.TypeVerify, false, jobs.Params{})
	st := verifyStats(t, res)
	if st.FilesVerified != 2 || st.SampleFiles != 2 || res.Warnings != 0 {
		t.Fatalf("first verify %+v (warnings %d)\n%s", st, res.Warnings, h.rep.dump())
	}
	first := map[string]bool{}
	for _, it := range h.items(j1.ID) {
		if d, _ := parseItem(it); d.Check == checkSample {
			first[it.RelPath] = true
			r := h.mustLive(it.RelPath)
			if r.VerifiedAt == nil || !strings.HasPrefix(r.Hash, "sha256:") {
				t.Errorf("%s: not recorded as verified: %+v", it.RelPath, r)
			}
		}
	}
	// The second verify takes the two never verified; one is damaged, one changed at the source.
	h.advance(time1h)
	var damaged, changed string
	for _, rel := range []string{"a.mkv", "b.mkv", "c.mkv", "d.mkv"} {
		if !first[h.dp(rel)] {
			if damaged == "" {
				damaged = rel
			} else {
				changed = rel
			}
		}
	}
	h.restic.damaged[h.srcDir+"/"+damaged] = true
	h.writeSrc(changed, content("X", 5000), 99)
	h.restic.checkErrors = 2
	res, j2 := h.mustRun(jobs.TypeVerify, false, jobs.Params{})
	st = verifyStats(t, res)
	if st.FilesMissing != 1 || st.Check.NumErrors != 2 || res.Warnings < 2 {
		t.Errorf("second verify %+v (warnings %d)", st, res.Warnings)
	}
	if r := h.mustLive(h.dp(damaged)); r.State != syncer.StateMissing {
		t.Errorf("the damaged file's record: %+v", r)
	}
	for _, it := range h.items(j2.ID) {
		if it.RelPath == h.dp(changed) {
			t.Errorf("a changed source file was sampled: %+v", it)
		}
		if d, _ := parseItem(it); d.Check == checkRepository && it.Status != jobs.ItemFailed {
			t.Errorf("check item %s despite errors", it.Status)
		}
	}
	if _, err := os.Stat(h.configDir + "/staging/verify-job" + fmt.Sprint(j2.ID)); !os.IsNotExist(err) {
		t.Errorf("the staging directory is left: %v", err)
	}
}

// TestResticVerifySampleDamagedPack: a sampled file whose data sits in a damaged pack does not
// restore (restic ends "There were 1 errors", exit 1): its item fails and its record becomes
// missing, the others are compared, and the job completes with warnings and its stats (§6.6,
// §20.4). A restore that fails for another reason fails the job and marks nothing missing.
func TestResticVerifySampleDamagedPack(t *testing.T) {
	h := newRestic(t)
	h.settings(func(s *destinations.Settings) { s.Verify.Mode, s.Verify.SamplePercent = destinations.VerifySample, 100 })
	h.firstSnapshot(map[string]string{"a.mkv": content("a", 1000), "b.mkv": content("b", 1100), "c.mkv": content("c", 1200)})
	h.restic.unrestorable[h.srcDir+"/b.mkv"] = true
	res, j := h.mustRun(jobs.TypeVerify, false, jobs.Params{})
	st := verifyStats(t, res)
	if st.FilesVerified != 2 || st.FilesMissing != 1 || res.Warnings < 1 {
		t.Fatalf("verify %+v (warnings %d)\n%s", st, res.Warnings, h.rep.dump())
	}
	if r := h.mustLive(h.dp("b.mkv")); r.State != syncer.StateMissing {
		t.Errorf("b.mkv record %+v", r)
	}
	for _, it := range h.items(j.ID) {
		if d, _ := parseItem(it); d.Check == checkSample && it.RelPath == h.dp("b.mkv") &&
			(it.Status != jobs.ItemFailed || !strings.Contains(it.Error, "could not be restored")) {
			t.Errorf("b.mkv item %s %q", it.Status, it.Error)
		}
	}

	h.advance(time1h)
	h.restic.unrestorable = map[string]bool{}
	h.restic.restoreFails = true
	j2 := h.newJob(jobs.TypeVerify, false, jobs.Params{})
	if _, err := h.runJob(j2); err == nil || !strings.Contains(err.Error(), "restore the verify sample") {
		t.Fatalf("a failed restore: %v", err)
	}
	for _, rel := range []string{"a.mkv", "c.mkv"} {
		if r := h.mustLive(h.dp(rel)); r.State != syncer.StatePresent {
			t.Errorf("%s marked %s by a restore that could not run", rel, r.State)
		}
	}
}

// TestVerifySampleByteCap: the sample stops at verify.sampleMaxBytes.
func TestVerifySampleByteCap(t *testing.T) {
	h := newRclone(t)
	h.settings(func(s *destinations.Settings) {
		s.Verify.Mode, s.Verify.SamplePercent, s.Verify.SampleMaxBytes = destinations.VerifySample, 100, 1<<20
	})
	h.writeSrc("big.mkv", content("b", 700<<10), 1)
	h.writeSrc("big2.mkv", content("c", 700<<10), 2)
	h.writeSrc("small.mkv", content("s", 1000), 3)
	h.mustSync(jobs.Params{})
	res, _ := h.mustRun(jobs.TypeVerify, false, jobs.Params{})
	if st := verifyStats(t, res); st.SampleFiles != 2 || st.SampleBytes > 1<<20 {
		t.Errorf("sample %+v", st)
	}
}

// TestRcloneVerify: the listing marks a missing object's record missing; the sample runs rclone
// check --download (= verified with the source's sha256, * missing); config versions are listed
// by the sizes their manifest names and the newest is hashed (§7.6).
func TestRcloneVerify(t *testing.T) {
	h := newRclone(t)
	h.settings(func(s *destinations.Settings) { s.Verify.Mode, s.Verify.SamplePercent = destinations.VerifySample, 100 })
	h.writeSrc("a.mkv", content("a", 1000), 1)
	h.writeSrc("b.mkv", content("b", 1100), 2)
	h.writeSrc("gone.mkv", content("g", 1200), 3)
	h.mustSync(jobs.Params{})
	h.rclone.remove(h.dp("gone.mkv"))
	o, _ := h.rclone.get(h.dp("b.mkv"))
	h.rclone.put(h.dp("b.mkv"), []byte(content("B", 1100)), o.mtime) // same size, other content
	zip := []byte("zip content of the arr backup")
	sum := sha256.Sum256(zip)
	ver := ".bunkarr/arr/radarr-3/20260901T000000Z"
	h.rclone.put(ver+"/radarr_backup.zip", zip, h.now())
	h.rclone.put(ver+"/manifest.json", []byte(fmt.Sprintf(`{"jobId":7,"integrationId":3,"zip":{"name":"radarr_backup.zip","size":%d,"sha256":%q}}`,
		len(zip), hex.EncodeToString(sum[:]))), h.now())
	res, _ := h.mustRun(jobs.TypeVerify, false, jobs.Params{})
	st := verifyStats(t, res)
	if st.FilesMissing != 2 || st.FilesVerified != 1 || st.VersionsChecked != 1 {
		t.Errorf("stats %+v\n%s", st, h.rep.dump())
	}
	for rel, want := range map[string]syncer.State{"a.mkv": syncer.StatePresent, "b.mkv": syncer.StateMissing, "gone.mkv": syncer.StateMissing} {
		if r := h.mustLive(h.dp(rel)); r.State != want {
			t.Errorf("%s: %s, want %s", rel, r.State, want)
		}
	}
	for _, c := range h.callsOf(proc.Rclone, "check") {
		if !c.Has("--download") || !c.Has("--one-way") {
			t.Errorf("check without --download: %s", c)
		}
	}
	// A tampered version fails its item.
	h.rclone.put(ver+"/radarr_backup.zip", []byte("tampered zip content of the backup"), h.now())
	res, _ = h.mustRun(jobs.TypeVerify, false, jobs.Params{})
	if res.Warnings == 0 || !h.rep.has("a config version is damaged") {
		t.Errorf("the tampered version was not reported (warnings %d)", res.Warnings)
	}
	// The next sync repairs the missing records (the damaged object goes to retention).
	h.advance(time1h)
	st2, _ := h.mustSync(jobs.Params{})
	if st2.FilesCopied != 2 {
		t.Errorf("repair sync %+v", st2)
	}
}

// TestRcloneRepairDamagedSameSize: a repair whose damaged object still has the catalog's size and
// mtime moves that object into retention (damaged) and uploads the file again; it is never
// recorded present as it is (§7.3, §7.6).
func TestRcloneRepairDamagedSameSize(t *testing.T) {
	h := newRclone(t)
	h.settings(func(s *destinations.Settings) { s.Verify.Mode, s.Verify.SamplePercent = destinations.VerifySample, 100 })
	h.writeSrc("b.mkv", content("b", 1100), 2)
	h.mustSync(jobs.Params{})
	o, _ := h.rclone.get(h.dp("b.mkv"))
	damaged := []byte(content("B", 1100))
	h.rclone.put(h.dp("b.mkv"), damaged, o.mtime) // same size and mtime, other content
	h.mustRun(jobs.TypeVerify, false, jobs.Params{})
	if r := h.mustLive(h.dp("b.mkv")); r.State != syncer.StateMissing {
		t.Fatalf("verify did not mark the damaged object: %+v", r)
	}
	h.advance(time1h)
	st, _ := h.mustSync(jobs.Params{})
	if st.FilesCopied != 1 {
		t.Errorf("repair sync %+v", st)
	}
	if got, _ := h.rclone.get(h.dp("b.mkv")); string(got.data) != content("b", 1100) {
		t.Errorf("the damaged object is still at the live path: %.12q", got.data)
	}
	var kept bool
	for _, r := range h.retained() {
		if r.Reason == syncer.ReasonDamaged {
			if obj, ok := h.rclone.get(r.RetainedPath); ok && string(obj.data) == string(damaged) {
				kept = true
			}
		}
	}
	if !kept {
		t.Errorf("the damaged object is not retained: %+v", h.retained())
	}
	h.advance(time1h)
	res, _ := h.mustRun(jobs.TypeVerify, false, jobs.Params{})
	if vs := verifyStats(t, res); vs.FilesMissing != 0 || vs.FilesVerified != 1 {
		t.Errorf("verify after the repair %+v", vs)
	}
}

// TestResticStoppedExclusiveReleasesLock: a restic check (verify) or prune (retention) that the
// window's end interrupts leaves its exclusive lock behind (its rclone backend stops with it,
// §20.3); the job removes it with the guarded unlock before it defers, so backups and config
// version jobs are not refused until the deferred job runs again.
func TestResticStoppedExclusiveReleasesLock(t *testing.T) {
	for _, sub := range []string{"check", "prune"} {
		t.Run(sub, func(t *testing.T) {
			h := newHarness(t, harnessOptions{kind: engines.Restic, bandwidth: nightWindow(false, 0)})
			h.setClock(time.Date(2026, 9, 28, 2, 0, 0, 0, time.UTC))
			h.writeSrc("a.mkv", content("a", 1000), 1)
			h.mustSync(jobs.Params{})
			h.restic.hangExclusive[sub] = true
			h.restic.exclusiveHost = "nas-before-restart"
			h.setClock(time.Date(2026, 9, 28, 6, 59, 59, 950_000_000, time.UTC))
			typ, p := jobs.TypeVerify, jobs.Params{}
			if sub == "prune" {
				typ, p = jobs.TypeRetention, jobs.Params{Prune: true}
			}
			j := h.newJob(typ, false, p)
			_, err := h.runJob(j)
			h.closeJob(j.ID)
			mustDefer(t, err)
			if len(h.callsOf(proc.Restic, sub)) != 1 {
				t.Fatalf("%s did not run", sub)
			}
			if len(h.restic.locks) != 0 {
				t.Errorf("the interrupted %s's lock is left: %v", sub, h.restic.locks)
			}
		})
	}
}

// TestResticVerifyLineBreakName: a sampled file whose name has a line break (restic's include
// file cannot name it) is skipped; the verify completes and checks the others (§6.6).
func TestResticVerifyLineBreakName(t *testing.T) {
	h := newRestic(t)
	h.settings(func(s *destinations.Settings) { s.Verify.Mode, s.Verify.SamplePercent = destinations.VerifySample, 100 })
	h.firstSnapshot(map[string]string{"a.mkv": content("a", 1000), "line\nbreak.mkv": content("n", 1100)})
	res, j := h.mustRun(jobs.TypeVerify, false, jobs.Params{})
	if st := verifyStats(t, res); st.FilesVerified != 1 || st.FilesMissing != 0 {
		t.Errorf("verify %+v", st)
	}
	if s, _ := itemStatus(h.items(j.ID), h.dp("line\nbreak.mkv")); s != jobs.ItemSkipped {
		t.Errorf("the line-break name's item is %s", s)
	}
}

// withStagingFree replaces the config directory's free space for the verify staging.
func withStagingFree(t *testing.T, fn func() int64) {
	old := stagingFreeSpace
	stagingFreeSpace = func(string) (int64, bool) { return fn(), true }
	t.Cleanup(func() { stagingFreeSpace = old })
}

// TestResticVerifyStagingSpace: the restic sample is restored into the config directory only as
// far as its free space allows (the rest is skipped for a later verify); a partial restore while
// the config directory ran full fails the job and marks nothing missing, since it says nothing
// about the repository (§6.6).
func TestResticVerifyStagingSpace(t *testing.T) {
	h := newRestic(t)
	h.settings(func(s *destinations.Settings) { s.Verify.Mode, s.Verify.SamplePercent = destinations.VerifySample, 100 })
	h.firstSnapshot(map[string]string{"a.mkv": content("a", 1000), "b.mkv": content("b", 1000), "c.mkv": content("c", 1000)})
	withStagingFree(t, func() int64 { return stagingMargin + 1500 })
	res, _ := h.mustRun(jobs.TypeVerify, false, jobs.Params{})
	if st := verifyStats(t, res); st.FilesVerified != 1 || st.FilesMissing != 0 || st.FilesSkipped != 2 {
		t.Errorf("verify %+v", st)
	}
	// The disk fills during the restore: a file does not restore, the job fails, nothing is missing.
	calls := 0
	withStagingFree(t, func() int64 {
		if calls++; calls == 1 {
			return 1 << 30
		}
		return 0
	})
	h.restic.unrestorable[h.srcDir+"/b.mkv"] = true
	h.advance(time1h)
	j := h.newJob(jobs.TypeVerify, false, jobs.Params{})
	_, err := h.runJob(j)
	h.closeJob(j.ID)
	if err == nil || !strings.Contains(err.Error(), "ran out of free space") {
		t.Fatalf("err = %v", err)
	}
	for _, rel := range []string{"a.mkv", "b.mkv", "c.mkv"} {
		if r := h.mustLive(h.dp(rel)); r.State != syncer.StatePresent {
			t.Errorf("%s marked %s", rel, r.State)
		}
	}
}
