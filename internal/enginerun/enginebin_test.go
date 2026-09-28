//go:build enginebin

package enginerun

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/config"
	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/enginetest"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
	"github.com/sl0wz3r/bunkarr/internal/engines/rclone"
	"github.com/sl0wz3r/bunkarr/internal/engines/restic"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/syncer"
)

// The real-binary tests of the engine runners (make test-engines, docs/design/phase4.md §14.4):
// restic 0.18.1 on MinIO through its rclone backend, and rclone 1.74.1 with crypt on the SFTP
// server and on MinIO, each through the whole stack: the destination created by the real
// engine (restic init, the rclone marker), a sync, a change set (update, addition, deletion,
// rename), a verify and a retention job past deletedDays (restic: forget, prune and check).

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// realHarness is a harness whose Service runs the real binaries (no fakes).
func newRealHarness(t *testing.T, kind engines.Kind, destKind engines.DestKind) *harness {
	t.Helper()
	e := enginetest.RealEnv(t)
	h := &harness{t: t, ctx: context.Background(), clock: time.Now().UTC(), rep: &recReporter{}, fc: &recordingRunners{}}
	h.base = resolvedTempDir(t)
	h.srcDir, h.configDir = filepath.Join(h.base, "src"), filepath.Join(h.base, "config")
	for _, d := range []string{h.srcDir, h.configDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	h.db = openMigratedDB(t, filepath.Join(h.base, "bunkarr.db"))
	h.files = syncer.NewStore(h.db)
	h.cat = catalog.NewStore(h.db, catalog.StoreOptions{HasBackups: h.files.HasBackups})
	h.scanner = catalog.NewScanner(h.cat, catalog.ScannerOptions{})
	h.jq = jobqueue.NewStore(h.db)
	master := make([]byte, 32)
	_, _ = rand.Read(master)
	kr, err := config.NewKeyring(master)
	if err != nil {
		t.Fatal(err)
	}
	runner := proc.NewExecRunner(proc.ExecOptions{ResticPath: e.ResticPath, RclonePath: e.RclonePath, TermAfter: 20 * time.Second})
	runDirs := proc.NewRunDirs(h.configDir, proc.RunDirOptions{})
	h.avail = engines.Discover(h.ctx, runner, config.EngineBinary{Path: e.ResticPath}, config.EngineBinary{Path: e.RclonePath})
	if !h.avail.Restic.Available || !h.avail.Rclone.Available {
		t.Fatalf("engines: %+v", h.avail)
	}
	rcloneDrv := &rclone.Driver{Runner: runner, RunDirs: runDirs, Version: h.avail.Rclone.Version}
	resticDrv := &restic.Driver{Runner: runner, RunDirs: runDirs, Rclone: rcloneDrv, CacheRoot: filepath.Join(h.configDir, "cache", "restic"),
		RclonePath: e.RclonePath, Version: h.avail.Restic.Version}
	var svc *Service
	h.dests = destinations.New(h.db, destinations.Options{Keyring: kr, ConfigDir: h.configDir,
		Engines: func(k engines.Kind) (engines.Engine, bool) { return svc.Engines()(k) }})
	so := syncer.Options{DB: h.db, Store: h.files, Catalog: h.cat, Scanner: h.scanner, Destinations: h.dests}
	h.planner = syncer.NewPlanner(so)
	svc = New(Options{DB: h.db, Files: h.files, Planner: h.planner, Catalog: h.cat, Scanner: h.scanner, Destinations: h.dests,
		Filecopy: FilecopyRunners{Sync: syncer.NewSyncRunner(so), Verify: syncer.NewVerifyRunner(so),
			Retention: syncer.NewRetentionRunner(syncer.RetentionOptions{Options: so})},
		Restic: resticDrv, Rclone: rcloneDrv, Availability: func() engines.Availability { return h.avail }, ConfigDir: h.configDir,
		RunDirs: runDirs, Runner: runner, Now: h.now, Location: time.UTC, HostName: "bunkarr-enginebin", ProcessStart: time.Now()})
	h.svc = svc
	h.src, err = h.cat.Create(h.ctx, catalog.SourceInput{Name: "Movies", Path: h.srcDir})
	if err != nil {
		t.Fatal(err)
	}
	prefix := "enginerun/" + string(kind) + "-" + randHex(4)
	in := destinations.Input{Name: "real-" + string(kind) + "-" + string(destKind), Kind: destKind, Engine: string(kind),
		SourceIDs: []int64{h.src.ID}, Settings: &destinations.Settings{Verify: destinations.Verify{Mode: destinations.VerifySample, SamplePercent: 100}}}
	var creds map[string]string
	switch destKind {
	case engines.S3:
		in.Remote, _ = json.Marshal(map[string]any{"provider": "Minio", "endpoint": e.S3Endpoint, "region": "us-east-1", "bucket": e.S3Bucket,
			"prefix": prefix, "forcePathStyle": true})
		creds = map[string]string{"accessKeyId": e.S3KeyID, "secretAccessKey": e.S3Secret}
	case engines.SFTP:
		var keys []map[string]string
		for _, k := range e.SFTPHostKeys {
			keys = append(keys, map[string]string{"type": k.Type, "key": k.Key})
		}
		sub := strings.ReplaceAll(prefix, "/", "-")
		in.Remote, _ = json.Marshal(map[string]any{"host": e.SFTPHost, "port": e.SFTPPort, "user": e.SFTPUser,
			"path": e.SFTPPath + "/" + sub, "hostKeys": keys})
		creds = map[string]string{"privateKey": e.SFTPKey, "privateKeyPassphrase": e.SFTPKeyPassphrase}
		// Create needs an existing path: rcat of Bunkarr's own links.tsv creates it. The SFTP
		// destination is plain (crypt runs on S3): a plain file in a crypt root would not decrypt.
		pd, ps := e.SFTP(0, sub, false, rclone.ObscureRandom)
		c, err := rcloneDrv.Connect(pd, ps, engines.Runtime{})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Rcat(h.ctx, syncer.LinkManifestRel, []byte{}); err != nil {
			t.Fatal(err)
		}
		in.Encryption = &destinations.EncryptionInput{Mode: engines.EncryptionNone, AcceptUnencrypted: true}
	}
	raw, _ := json.Marshal(creds)
	var ci destinations.CredentialsInput
	if err := json.Unmarshal(raw, &ci); err != nil {
		t.Fatal(err)
	}
	in.Credentials = &ci
	h.dest, err = h.dests.Create(h.ctx, in, destinations.CreateOptions{})
	if err != nil {
		t.Fatalf("create %s on %s: %v", kind, destKind, err)
	}
	h.exec(`UPDATE destinations SET kit_confirmed_at = ? WHERE id = ?`, db.FormatTime(h.now()), h.dest.ID)
	return h
}

// TestRealEngineLifecycle: sync, change set, verify and retention with the real binaries.
func TestRealEngineLifecycle(t *testing.T) {
	cases := []struct {
		kind engines.Kind
		dest engines.DestKind
	}{
		{engines.Restic, engines.S3},
		{engines.Rclone, engines.SFTP},
		{engines.Rclone, engines.S3},
	}
	for _, c := range cases {
		t.Run(string(c.kind)+"-"+string(c.dest), func(t *testing.T) {
			h := newRealHarness(t, c.kind, c.dest)
			h.writeSrc("a.mkv", content("a1", 300_000), 1)
			h.writeSrc("b.mkv", content("b", 200_000), 2)
			h.writeSrc("c.mkv", content("c", 150_000), 3)
			h.writeSrc("D/e.srt", content("e", 1000), 4)
			st, _ := h.mustSync(jobs.Params{})
			if st.FilesCopied != 4 {
				t.Fatalf("initial sync %+v\n%s", st, h.rep.dump())
			}
			h.writeSrc("a.mkv", content("a2", 310_000), 20)
			h.writeSrc("new.mkv", content("n", 120_000), 21)
			h.removeSrc("c.mkv")
			h.renameSrc("b.mkv", "moved/b.mkv")
			// A file of the plan vanishes after the scan, right before the transfer: its item fails
			// (restic leaves it out of the snapshot; rclone copy of a listed file that is gone),
			// the job completes.
			h.writeSrc("fresh/vanish.mkv", content("v", 5000), 22)
			var once sync.Once
			faultinject.SetHook(func(name string) {
				if name == PointBeforeBatch {
					once.Do(func() { _ = os.Remove(h.srcPath("fresh/vanish.mkv")) })
				}
			})
			h.advance(time1h)
			st, j := h.mustSync(jobs.Params{})
			faultinject.SetHook(nil)
			if st.FilesCopied != 1 || st.FilesUpdated != 1 || st.FilesMoved != 1 || st.FilesRetained != 1 || st.FilesFailed != 1 {
				t.Fatalf("change set %+v\n%s", st, h.rep.dump())
			}
			if s, msg := itemStatus(h.items(j.ID), h.dp("fresh/vanish.mkv")); s != jobs.ItemFailed {
				t.Errorf("the vanished file's item: %s %q", s, msg)
			} else {
				t.Logf("the vanished file's item failed with %q", msg)
			}
			if c.kind == engines.Restic && len(st.Snapshots) != 1 {
				t.Errorf("snapshots %+v", st.Snapshots)
			}
			res, _ := h.mustRun(jobs.TypeVerify, false, jobs.Params{})
			vs := res.Stats.(VerifyStats)
			if vs.FilesVerified == 0 || vs.FilesMissing != 0 || res.Warnings != 0 {
				t.Errorf("verify %+v (warnings %d)\n%s", vs, res.Warnings, h.rep.dump())
			}
			h.advance(31 * 24 * time.Hour)
			res, _ = h.mustRun(jobs.TypeRetention, false, jobs.Params{Prune: c.kind == engines.Restic})
			rs := res.Stats.(RetentionStats)
			if rs.FilesExpired != 2 {
				t.Errorf("retention %+v\n%s", rs, h.rep.dump())
			}
			if c.kind == engines.Restic && !rs.Pruned {
				t.Error("no prune")
			}
			// Nothing new is planned.
			h.advance(time1h)
			st, _ = h.mustSync(jobs.Params{})
			if st.FilesPlanned != 0 {
				t.Errorf("a further sync planned %d items", st.FilesPlanned)
			}
		})
	}
}

// TestRealStaleExclusiveLock: a restic check interrupted on a remote repository (the window's
// end) leaves its exclusive lock behind, since the interrupt stops restic's rclone backend with
// it; restic 0.18 then refuses every backup with exit 11. A sync removes such a lock with the
// guarded unlock and backs up (§6.7, §20.3).
func TestRealStaleExclusiveLock(t *testing.T) {
	h := newRealHarness(t, engines.Restic, engines.S3)
	h.writeSrc("a.mkv", content("a", 300_000), 1)
	noise := make([]byte, 96<<20)
	_, _ = rand.Read(noise)
	if err := os.WriteFile(h.srcPath("noise.bin"), noise, 0o644); err != nil {
		t.Fatal(err)
	}
	h.mustSync(jobs.Params{})
	ed, sec, err := h.dests.SecretsFor(h.ctx, h.dest.ID)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := h.svc.o.Restic.Connect(ed, sec, engines.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	// Interrupt a check --read-data after a delay (as the window's end does); try longer delays
	// until one leaves its lock behind.
	left := false
	for _, delay := range []time.Duration{150, 300, 500, 800, 1200, 1800, 2500, 3500} {
		stop := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			_, err := repo.Check(h.ctx, restic.CheckArgs{ReadData: true, Interrupt: stop})
			done <- err
		}()
		time.Sleep(delay * time.Millisecond)
		close(stop)
		err := <-done
		ids, lerr := repo.ListLocks(h.ctx)
		if lerr != nil {
			t.Fatal(lerr)
		}
		t.Logf("check interrupted after %dms: %v; locks left %d", delay, err, len(ids))
		if left = len(ids) > 0; left {
			break
		}
	}
	if !left {
		t.Skip("no interrupted check left its lock behind")
	}
	h.writeSrc("b.mkv", content("b", 200_000), 2)
	h.advance(time1h)
	st, _ := h.mustSync(jobs.Params{})
	if st.FilesCopied != 1 {
		t.Errorf("sync after the stale lock %+v\n%s", st, h.rep.dump())
	}
	if ids, _ := repo.ListLocks(h.ctx); len(ids) != 0 {
		t.Errorf("locks left: %v", ids)
	}
}
