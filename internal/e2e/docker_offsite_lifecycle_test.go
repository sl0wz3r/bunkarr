//go:build e2e

package e2e

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// Sizes of the lifecycle fixture (random bytes, so neither engine can compress them).
const (
	lcChanged    = 1 << 20
	lcChangedNew = 1<<20 + 4096
	lcAdded      = 3 << 19
	lcRenamed    = 2 << 20
	lcDeleted    = 60000
	lcBig        = 400 << 20
)

// lifecycleCase is one engine and storage of acceptance 3.
type lifecycleCase struct {
	name, dir string
	body      func(b *bunkarrC, name, prefix string, sources []int64, extra map[string]any) map[string]any
	// minio: the destination is on MinIO, so an object can be damaged there and the container
	// killed during a large upload (the SFTP server runs emulated on arm64 and shares the rest).
	minio bool
}

// TestDockerOffsiteLifecycle is acceptance 3 (docs/design/phase4.md §14.6 item 3) for restic on
// MinIO, rclone crypt on MinIO and rclone crypt on SFTP: an initial sync; the change set (one
// changed, one added, one deleted and one renamed file) with filesCopied=1, filesUpdated=1,
// filesMoved=1, filesRetained=1 and bytesUploaded about the changed and added files only, the
// deleted and the replaced version held (S5); a verify that passes; on MinIO, an object damaged in
// the bucket (a restic pack, an rclone file object) that the next verify finds, and a
// `docker kill -s KILL` during a 400 MiB upload at 4 MiB/s, after which `docker start` resumes the
// same job as attempt 2, it completes and the destination verifies. The source tree is compared
// before and after every job (S1).
func TestDockerOffsiteLifecycle(t *testing.T) {
	o := newOffsite(t)
	o.startMinIO()
	o.startSFTP()
	b := o.startBunkarr("bunkarr", bunkarrOpts{})
	cases := []lifecycleCase{
		{name: "restic on MinIO", dir: "lc-restic", body: (*bunkarrC).resticS3, minio: true},
		{name: "rclone crypt on MinIO", dir: "lc-crypt", body: (*bunkarrC).cryptS3, minio: true},
		{name: "rclone crypt on SFTP", dir: "lc-sftp", body: (*bunkarrC).cryptSFTP},
	}
	for _, c := range cases {
		t.Logf("== %s", c.name)
		o.lifecycle(b, c)
	}
	o.auditAll()
}

// lifecycle runs acceptance 3 for one case.
func (o *offsite) lifecycle(b *bunkarrC, c lifecycleCase) {
	t := o.t
	lib := c.dir + "/lib"
	o.writeRandom(map[string]int64{
		lib + "/Movies/Changed (2001)/Changed (2001).mkv": lcChanged,
		lib + "/Movies/Deleted (2002)/Deleted (2002).nfo": lcDeleted,
		lib + "/Movies/Renamed (2003)/Renamed.mkv":        lcRenamed,
		lib + "/Movies/Kept (2004)/Kept (2004).mkv":       512 << 10,
		lib + "/Movies/Kept (2004)/Kept (2004).en.srt":    20000,
	})
	src := b.createSource(c.name, lib)
	d, k := b.createOffsite(c.body(b, c.name, c.dir, []int64{src.ID}, nil))

	j := o.untouchedTree(lib, func() apiJob { return b.sync(d.ID, nil) })
	requireStatus(t, j, "completed")
	if st := decodeStats[engineSync](t, j); st.FilesCopied != 5 || st.FilesFailed != 0 || st.Engine != d.Engine {
		t.Fatalf("%s: initial sync: %s", c.name, j.Stats)
	}

	// The change set.
	o.toolsSh(`cd "$1"
head -c 1052672 /dev/urandom > "Movies/Changed (2001)/Changed (2001).mkv"
mkdir -p "Movies/Added (2005)" && head -c 1572864 /dev/urandom > "Movies/Added (2005)/Added (2005).mkv"
rm "Movies/Deleted (2002)/Deleted (2002).nfo"
mv "Movies/Renamed (2003)/Renamed.mkv" "Movies/Renamed (2003)/Renamed (2003).mkv"`, offsiteMedia+"/"+lib)
	j = o.untouchedTree(lib, func() apiJob { return b.sync(d.ID, nil) })
	requireStatus(t, j, "completed")
	st := decodeStats[engineSync](t, j)
	if st.FilesCopied != 1 || st.FilesUpdated != 1 || st.FilesMoved != 1 || st.FilesRetained != 1 || st.FilesFailed != 0 || st.FilesHeld != 0 {
		t.Fatalf("%s: the change set: %s", c.name, j.Stats)
	}
	// Only the changed and the added file are uploaded (restic deduplicates the renamed file,
	// rclone moves it server-side); crypt and restic's metadata add a little.
	want := int64(lcChangedNew + lcAdded)
	if st.BytesUploaded < want*95/100 || st.BytesUploaded > want*11/10+256<<10 {
		// Reported, and the rest of the lifecycle still runs.
		t.Errorf("%s: bytesUploaded %d, want about %d (the changed and the added file only): %s", c.name, st.BytesUploaded, want, j.Stats)
	}
	// S5: the deleted file and the old version of the changed one are held.
	o.requireHeld(b, d, k, map[string]string{
		src.DestFolder + "/Movies/Deleted (2002)/Deleted (2002).nfo": "deleted",
		src.DestFolder + "/Movies/Changed (2001)/Changed (2001).mkv": "replaced",
	})
	requireVerified(t, o.untouchedTree(lib, func() apiJob { return b.verify(d.ID) }))

	if !c.minio {
		return
	}
	// A damaged object: the next verify finds it.
	switch d.Engine {
	case "restic":
		packs := o.objects("bunkarr/" + c.dir + "/data")
		biggest, size := "", int64(0)
		for p, n := range packs {
			if n > size {
				biggest, size = p, n
			}
		}
		if biggest == "" {
			t.Fatalf("%s: no pack under bunkarr/%s/data", c.name, c.dir)
		}
		o.damageObject("bunkarr/" + c.dir + "/data/" + biggest)
	default:
		enc, err := o.engineCLI(k, "rclone", "cryptdecode", "--reverse", "bunkarr-crypt:", src.DestFolder+"/Movies/Kept (2004)/Kept (2004).mkv")
		if err != nil {
			t.Fatal(err)
		}
		f := strings.Split(strings.TrimSpace(enc), "\t")
		if len(f) != 2 || f[1] == "" {
			t.Fatalf("%s: rclone cryptdecode --reverse: %q", c.name, enc)
		}
		o.damageObject("bunkarr/" + c.dir + "/" + strings.TrimSpace(f[1]))
	}
	v := o.untouchedTree(lib, func() apiJob { return b.verify(d.ID) })
	if !o.foundDamage(b, v) {
		t.Fatalf("%s: verify %d (%s) did not find the damaged object: %s %q", c.name, v.ID, v.Status, v.Stats, v.Error)
	}
	t.Logf("%s: verify %d found the damage (%s: %s%s)", c.name, v.ID, v.Status, v.Summary, v.Error)
	if d.Engine == "rclone" && len(v.Stats) > 2 && decodeStats[engineVerify](t, v).FilesMissing == 0 {
		// Under crypt a damaged object does not decrypt: rclone check reports it as an error ("!"),
		// the sample item fails ("could not be checked") and the record stays present, so no sync
		// repairs it (§7.6).
		t.Logf("%s: the damaged object failed its verify item; its record was not marked missing, so no repair follows", c.name)
	} else if d.Engine == "rclone" {
		// The record is missing: the next sync copies the file again and keeps the damaged object
		// in retention; then the destination verifies.
		j = o.untouchedTree(lib, func() apiJob { return b.sync(d.ID, nil) })
		requireStatus(t, j, "completed")
		if st := decodeStats[engineSync](t, j); st.FilesCopied+st.FilesUpdated != 1 || st.FilesFailed != 0 {
			t.Fatalf("%s: the repair sync: %s", c.name, j.Stats)
		}
		requireVerified(t, o.untouchedTree(lib, func() apiJob { return b.verify(d.ID) }))
	}

	o.killResume(b, c)
}

// requireHeld requires the retained rows of destination d (path → reason) and, for rclone, their
// objects under .bunkarr/retention.
func (o *offsite) requireHeld(b *bunkarrC, d offDest, k kitInfo, want map[string]string) {
	t := o.t
	rows, err := b.readDB().Query(`SELECT rel_path, reason, COALESCE(engine_ref, ''), COALESCE(retained_path, '') FROM destination_files
		WHERE destination_id = ? AND state = 'retained'`, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	var retained []string
	for rows.Next() {
		var rel, reason, ref, rp string
		if err := rows.Scan(&rel, &reason, &ref, &rp); err != nil {
			t.Fatal(err)
		}
		got[rel] = reason
		switch d.Engine {
		case "restic":
			if len(ref) != 64 {
				t.Fatalf("%s: retained %s (%s) has no snapshot reference: %q", d.Name, rel, reason, ref)
			}
		case "rclone":
			if !strings.HasPrefix(rp, ".bunkarr/retention/") {
				t.Fatalf("%s: retained %s at %q, want under .bunkarr/retention/", d.Name, rel, rp)
			}
			retained = append(retained, rp)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for p, r := range want {
		if got[p] != r {
			t.Fatalf("%s: retained rows %v, want %s held as %s", d.Name, got, p, r)
		}
	}
	if d.Engine == "rclone" {
		out, err := o.engineCLI(k, "rclone", "lsf", "-R", "--files-only", "bunkarr-crypt:.bunkarr/retention")
		if err != nil {
			t.Fatal(err)
		}
		listed := strings.Split(strings.TrimSpace(out), "\n")
		for _, rp := range retained {
			if !slices.Contains(listed, strings.TrimPrefix(rp, ".bunkarr/retention/")) {
				t.Fatalf("%s: retained object %s is not in the remote's retention: %q", d.Name, rp, listed)
			}
		}
	}
}

// killResume is acceptance 3's crash: a 400 MiB upload at 4 MiB/s, `docker kill -s KILL` after
// 20 s, `docker start`; the same job resumes as attempt 2, completes, and the destination
// verifies. The source is unchanged by the whole episode (S1).
func (o *offsite) killResume(b *bunkarrC, c lifecycleCase) {
	t := o.t
	dir := c.dir + "/big"
	o.writeRandom(map[string]int64{dir + "/Remux (2020)/Remux (2020).mkv": lcBig, dir + "/Remux (2020)/Remux (2020).nfo": 3000})
	src := b.createSource(c.name+" (big)", dir)
	d, _ := b.createOffsite(c.body(b, c.name+" (big)", c.dir+"-kill", []int64{src.ID},
		map[string]any{"bandwidth": map[string]any{"uploadKiBps": 4096}}))
	before := o.tree(dir)
	j := b.startJob(fmt.Sprintf("/destinations/%d/sync", d.ID), nil)
	deadline := time.Now().Add(5 * time.Minute)
	for {
		cur := b.job(j.ID)
		if cur.final() {
			t.Fatalf("%s: sync %d ended %s before the kill: %s", c.name, j.ID, cur.Status, cur.Stats)
		}
		if cur.Status == "running" && cur.StartedAt != nil && time.Since(*cur.StartedAt) > 20*time.Second {
			t.Logf("%s: killing the container during the upload (%s, %d of %d bytes processed)", c.name, cur.Progress.Phase,
				cur.Progress.BytesDone, cur.Progress.BytesTotal)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: sync %d did not run for 20 s within 5 minutes: %+v", c.name, j.ID, cur)
		}
		time.Sleep(500 * time.Millisecond)
	}
	b.kill()
	b.start()
	r := b.api.client().waitJob(j.ID, 15*time.Minute)
	requireStatus(t, r, "completed")
	if r.Attempt != 2 || r.Trigger != "resume" {
		t.Fatalf("%s: resumed job %d: attempt %d, trigger %q; want attempt 2, trigger resume", c.name, r.ID, r.Attempt, r.Trigger)
	}
	// A file the killed attempt finished is adopted by the resumed one (it counts only its own
	// attempt's outcomes, DEFERRED "Stats across a resume").
	if st := decodeStats[engineSync](t, r); st.FilesFailed != 0 || st.FilesCopied+st.FilesAdopted != 2 {
		t.Fatalf("%s: resumed sync: %s", c.name, r.Stats)
	}
	requireVerified(t, b.verify(d.ID))
	if after := o.tree(dir); !mapsEqual(before, after) {
		t.Fatalf("%s: the kill and resume changed the source (S1)", c.name)
	}
	t.Logf("%s: job %d resumed as attempt %d and completed; the destination verifies", c.name, r.ID, r.Attempt)
}

// foundDamage reports whether a verify job found damage: its stats count a repository check error
// or a damaged, missing or failed file, or one of its items failed. (A restic sample restore that
// hits the damaged pack fails the items of the files it could not restore; its check item has
// failed before.)
func (o *offsite) foundDamage(b *bunkarrC, v apiJob) bool {
	if len(v.Stats) > 2 {
		vs := decodeStats[engineVerify](o.t, v)
		if vs.FilesMissing+vs.FilesFailed > 0 || (vs.Check != nil && vs.Check.NumErrors > 0) {
			return true
		}
	}
	for _, it := range b.items(v.ID) {
		if it.Status == "failed" {
			return true
		}
	}
	return false
}
