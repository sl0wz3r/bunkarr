//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
)

// TestDockerOffsiteSecrets is acceptance 4 (docs/design/phase4.md S22, §14.6 item 4) with every
// kind of secret the engines handle: S3 keys (restic and rclone crypt on MinIO), an SFTP private
// key with its passphrase and an SFTP password (rclone crypt), restic and crypt passwords, their
// obscured forms, and a wrong secret sent to POST /destinations/test (request-scoped redaction).
// Every engine job type runs (test, sync, incremental sync, verify, retention with prune), the
// storage credentials are rotated, and the transfers are slow enough (2 MiB/s) that the /proc
// sampler sees the engine processes and their secret files while they run. Then the audit of every
// test applies (argv log, sampler, process log at debug level, job logs, every API answer but the
// recovery kits, every file in the container but the sealed database, each also for rclone-obscured
// forms with any IV), and this test also requires that the sampler saw engine processes and secret
// files (0600, in a 0700 directory under /dev/shm/bunkarr-run, gone afterwards).
func TestDockerOffsiteSecrets(t *testing.T) {
	o := newOffsite(t)
	o.startMinIO()
	o.startSFTP()
	o.writeRandom(map[string]int64{
		"sec/Movies/Secret (2020)/Secret (2020).mkv": 24 << 20,
		"sec/Movies/Secret (2020)/Secret (2020).nfo": 3000,
	})
	b := o.startBunkarr("bunkarr", bunkarrOpts{})
	src := b.createSource("Secrets", "sec")
	slow := map[string]any{"bandwidth": map[string]any{"uploadKiBps": 2048}}

	// A Test with a wrong secret: the answer and the logs must not echo it.
	wrong := "wrong-secret-" + randomHex(12)
	o.secrets.add("wrong S3 secret of a Test", wrong)
	var res struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	}
	b.api.call(viaSession, 200, "POST", "/destinations/test", map[string]any{"kind": "s3", "engine": "rclone", "remote": o.s3Remote("bunkarr", "sec-test"),
		"credentials": map[string]any{"accessKeyId": o.s3Key, "secretAccessKey": wrong}, "encryption": map[string]any{"mode": "crypt"}}, &res)
	if res.OK {
		t.Fatalf("a Test with a wrong secret passed: %+v", res)
	}

	bodies := []map[string]any{
		b.resticS3("Secrets restic", "sec-restic", []int64{src.ID}, slow),
		b.cryptS3("Secrets crypt", "sec-crypt", []int64{src.ID}, slow),
		b.cryptSFTP("Secrets SFTP key", "sec-key", []int64{src.ID}, slow),
	}
	pw := b.cryptSFTP("Secrets SFTP password", "sec-pass", []int64{src.ID}, slow)
	pw["credentials"] = map[string]any{"password": o.sftp.password}
	bodies = append(bodies, pw)
	for _, body := range bodies {
		d, _ := b.createOffsite(body)
		var tr struct {
			OK bool `json:"ok"`
		}
		b.api.call(viaKey, 200, "POST", fmt.Sprintf("/destinations/%d/test", d.ID), nil, &tr)
		if !tr.OK {
			t.Fatalf("%s: test of the stored remote failed", d.Name)
		}
		requireStatus(t, b.sync(d.ID, nil), "completed")
		if d.Kind == "s3" {
			// Rotate the storage credentials (the same valid keys; tested against the stored remote).
			b.api.call(viaSession, 200, "PUT", fmt.Sprintf("/destinations/%d", d.ID), map[string]any{"credentials": o.s3Credentials(),
				"currentPassword": e2ePassword}, nil)
		}
	}
	o.toolsSh(`cd "$1" && head -c 3145728 /dev/urandom > "Movies/Secret (2020)/Extra.mkv" && rm "Movies/Secret (2020)/Secret (2020).nfo"`, offsiteMedia+"/sec")
	var dests []offDest
	b.api.call(viaKey, 200, "GET", "/destinations", nil, &dests)
	for _, d := range dests {
		requireStatus(t, b.sync(d.ID, nil), "completed")
		requireVerified(t, b.verify(d.ID))
		body := map[string]any{}
		if d.Engine == "restic" {
			body["prune"] = true
		}
		requireStatus(t, b.retention(d.ID, body), "completed")
	}

	b.audit()
	s := b.sampler
	s.mu.Lock()
	engine, dirs, files := s.engine, len(s.secretDirs), len(s.secretFile)
	var seen []string
	for f := range s.secretFile {
		seen = append(seen, f)
	}
	s.mu.Unlock()
	if engine == 0 {
		t.Fatal("the sampler never saw an engine process under the Bunkarr server")
	}
	if dirs == 0 || files == 0 {
		t.Fatalf("the sampler saw %d secret directories and %d secret files under /dev/shm/bunkarr-run: the secret files are not on the tmpfs", dirs, files)
	}
	names := map[string]bool{}
	for _, f := range seen {
		names[f[strings.LastIndexByte(f, '/')+1:]] = true
	}
	for _, n := range []string{"password", "known_hosts"} {
		if !names[n] {
			t.Errorf("the sampler never saw a secret file named %s under /dev/shm/bunkarr-run (saw %v)", n, names)
		}
	}
}
