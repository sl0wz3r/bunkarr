//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestDockerOffsiteTwoContainers is §6.7's two-container test (docs/design/phase4.md §14.6 item
// 11, DS-10): a second Bunkarr container with the same host name attaches the same restic
// repository (the attach counts as confirmed and warns about the other install's recent snapshots)
// and runs a sync; while its backup holds its lock, the first container starts a retention job
// with prune. The first must not remove the lock (restic would call it stale: same host name, a
// PID it does not know): its job fails with "another restic process with host name …". The
// second's sync completes with a complete snapshot, and `restic check --read-data` passes.
func TestDockerOffsiteTwoContainers(t *testing.T) {
	o := newOffsite(t)
	o.startMinIO()
	o.writeRandom(map[string]int64{
		"hosts/first/Small (2019)/Small (2019).mkv": 2 << 20,
		"hosts/second/Big (2020)/Big (2020).mkv":    160 << 20,
		"hosts/second/Big (2020)/Big (2020).nfo":    2500,
	})
	a := o.startBunkarr("first", bunkarrOpts{})
	srcA := a.createSource("First", "hosts/first")
	da, ka := a.createOffsite(a.resticS3("Shared repository", "hosts", []int64{srcA.ID}, nil))
	requireStatus(t, a.sync(da.ID, nil), "completed")

	b := o.startBunkarr("second", bunkarrOpts{})
	srcB := b.createSource("Second", "hosts/second")
	body := b.resticS3("Shared repository", "hosts", []int64{srcB.ID}, map[string]any{"attach": true,
		"bandwidth": map[string]any{"uploadKiBps": 2048}})
	body["encryption"] = map[string]any{"mode": "restic", "generate": false, "secret": ka.Encryption.ResticPassword}
	db := b.createDest(body)
	if db.Encryption.KitConfirmedAt == nil || db.Encryption.Origin != "user" {
		t.Fatalf("attached destination: %+v (attach counts as confirmed)", db.Encryption)
	}
	if !strings.Contains(strings.ToLower(strings.Join(db.Warnings, " ")), "another bunkarr") {
		t.Fatalf("attach without the warning about the other install's recent snapshots: %q", db.Warnings)
	}

	jb := b.startJob(fmt.Sprintf("/destinations/%d/sync", db.ID), nil)
	// Wait until the second's backup holds a lock in the repository.
	deadline := time.Now().Add(3 * time.Minute)
	for {
		out, err := o.engineCLI(ka, "restic", "list", "locks", "--no-lock")
		if err == nil && strings.TrimSpace(out) != "" && b.job(jb.ID).Status == "running" {
			break
		}
		if cur := b.job(jb.ID); cur.final() {
			t.Fatalf("the second's sync %d ended %s before the first's retention: %s", jb.ID, cur.Status, cur.Stats)
		}
		if time.Now().After(deadline) {
			t.Fatalf("no restic lock in the repository within 3 minutes of the second's sync (%v)", err)
		}
		time.Sleep(time.Second)
	}
	ja := a.retention(da.ID, map[string]any{"prune": true})
	if ja.Status != "failed" || !strings.Contains(ja.Error, "another restic process with host name "+offsiteHost) {
		t.Fatalf("the first's retention while the second backs up: status %s, error %q; want failed with \"another restic process with host name %s …\"",
			ja.Status, ja.Error, offsiteHost)
	}
	if cur := b.job(jb.ID); cur.Status != "running" {
		t.Fatalf("the second's sync is %s after the first's refusal (its lock must still be there)", cur.Status)
	}
	rb := b.api.client().waitJob(jb.ID, 15*time.Minute)
	requireStatus(t, rb, "completed")
	st := decodeStats[engineSync](t, rb)
	if st.FilesCopied != 2 || st.FilesFailed != 0 || len(st.Snapshots) == 0 {
		t.Fatalf("the second's sync: %s", rb.Stats)
	}
	// The second's snapshot is complete: its content listing holds both files with their sizes.
	snap := st.Snapshots[len(st.Snapshots)-1].SnapshotID
	listed := o.resticLs(ka, snap)
	for p, size := range map[string]int64{offsiteMedia + "/hosts/second/Big (2020)/Big (2020).mkv": 160 << 20,
		offsiteMedia + "/hosts/second/Big (2020)/Big (2020).nfo": 2500} {
		if listed[p] != size {
			t.Fatalf("snapshot %.8s lists %s with %d bytes, want %d", snap, p, listed[p], size)
		}
	}
	o.resticCheck(ka, true)
	requireVerified(t, b.verify(db.ID))
	// With the second idle, the first's prune runs.
	requireStatus(t, a.retention(da.ID, map[string]any{"prune": true}), "completed")
	o.resticCheck(ka, false)
	o.auditAll()
}

// resticLs returns the files of a snapshot (`restic ls --json`) with their sizes.
func (o *offsite) resticLs(k kitInfo, snapshot string) map[string]int64 {
	o.t.Helper()
	out, err := o.engineCLI(k, "restic", "ls", "--json", "--no-lock", snapshot)
	if err != nil {
		o.t.Fatal(err)
	}
	m := map[string]int64{}
	for _, l := range strings.Split(out, "\n") {
		var n struct {
			Type string `json:"type"`
			Path string `json:"path"`
			Size int64  `json:"size"`
		}
		if json.Unmarshal([]byte(l), &n) == nil && n.Type == "file" {
			m[n.Path] = n.Size
		}
	}
	return m
}
