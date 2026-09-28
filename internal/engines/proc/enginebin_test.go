//go:build enginebin

package proc_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/sl0wz3r/bunkarr/internal/config"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/bwlimit"
	"github.com/sl0wz3r/bunkarr/internal/engines/enginetest"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
	"github.com/sl0wz3r/bunkarr/internal/engines/rclone"
)

// The smoke tests of the exec layer against the real binaries (make test-engines, §14.4): the
// version commands, rclone lsf against MinIO and SFTP with the environment-only configuration
// (the pinned known_hosts, a passphrase-protected key as KEY_PEM, an obscured password), a crypt
// round trip, and rclone's own obscure/reveal against rclone.Obscure and rclone.Reveal.

// run starts c through the real runner and returns its stdout, stderr and status.
func run(t *testing.T, r proc.Runner, c proc.Cmd) (string, string, proc.ExitStatus) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	p, err := r.Start(ctx, c)
	if err != nil {
		t.Fatalf("%s: %v", c, err)
	}
	var out, errOut strings.Builder
	for l := range p.Lines() {
		if l.Stderr {
			errOut.WriteString(l.Text + "\n")
		} else {
			out.WriteString(l.Text + "\n")
		}
	}
	st, err := p.Wait()
	if err != nil {
		t.Fatalf("%s: %v", c, err)
	}
	return out.String(), errOut.String(), st
}

// rcloneCmd builds an rclone command for d with its run directory and secret files.
func rcloneCmd(t *testing.T, dirs *proc.RunDirs, d engines.Destination, s engines.Secrets, args ...string) proc.Cmd {
	t.Helper()
	rd, err := dirs.New(1)
	if err != nil {
		t.Fatal(err)
	}
	c := proc.Cmd{Binary: proc.Rclone, Args: args, Dir: rd, SecretFiles: map[string][]byte{}, Redact: s.Values(), Budget: time.Minute, RetryBudget: 20 * time.Second}
	in := rclone.EnvInput{Dest: d, Secrets: s}
	if d.Kind == engines.SFTP {
		kh, err := rclone.KnownHosts(d.Remote.SFTP.Host, d.Remote.SFTP.Port, d.Remote.SFTP.HostKeys)
		if err != nil {
			t.Fatal(err)
		}
		c.SecretFiles["known_hosts"] = []byte(kh)
		in.KnownHostsPath = rd.SecretPath("known_hosts")
	}
	if c.Env, err = rclone.Env(in); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestEngineBinaries(t *testing.T) {
	e := enginetest.RealEnv(t)
	r := proc.NewExecRunner(proc.ExecOptions{ResticPath: e.ResticPath, RclonePath: e.RclonePath})
	dirs := proc.NewRunDirs(t.TempDir(), proc.RunDirOptions{})
	key := bytes.Repeat([]byte{7}, 32)
	obscure := func(field string) func(string) string {
		return func(v string) string { return rclone.Obscure(key, 99, field, v) }
	}

	t.Run("versions", func(t *testing.T) {
		a := engines.Discover(context.Background(), r, config.EngineBinary{Path: e.ResticPath}, config.EngineBinary{Path: e.RclonePath})
		if !a.Restic.Available || !a.Rclone.Available {
			t.Fatalf("Discover = %+v", a)
		}
		t.Logf("restic %s, rclone %s", a.Restic.Version, a.Rclone.Version)
	})

	t.Run("tmpfs run directory", func(t *testing.T) {
		rd, err := dirs.New(1)
		if err != nil {
			t.Fatal(err)
		}
		defer rd.Remove()
		if rd.Warning() != "" {
			t.Logf("no tmpfs here: %s", rd.Warning())
		} else if !strings.HasPrefix(rd.SecretDir(), "/dev/shm/bunkarr-run/") {
			t.Errorf("secret dir %s", rd.SecretDir())
		}
	})

	t.Run("lsf MinIO", func(t *testing.T) {
		d, s := e.S3(1, "", false, rclone.ObscureRandom)
		_, stderr, st := run(t, r, rcloneCmd(t, dirs, d, s, "lsf", "--max-depth", "1", rclone.StorageRoot(d)))
		if st.Code != 0 {
			t.Fatalf("exit %+v: %s", st, stderr)
		}
	})

	// A name holding the access key ID comes back redacted from the real rclone's listing, so a
	// command naming it is refused before it runs: a job must never decide on such a listing.
	// The object is put there by a command whose Redact leaves the key ID out.
	t.Run("MinIO name holding the access key reads back redacted", func(t *testing.T) {
		d, s := e.S3(6, "keyname", false, rclone.ObscureRandom)
		name := "probe-" + e.S3KeyID + ".txt"
		lax := func(c proc.Cmd) proc.Cmd {
			c.Redact = slices.DeleteFunc(slices.Clone(c.Redact), func(v string) bool { return strings.Contains(e.S3KeyID, v) })
			return c
		}
		c := lax(rcloneCmd(t, dirs, d, s, "rcat", rclone.StorageRoot(d)+"/"+name))
		c.Stdin = strings.NewReader("probe\n")
		if _, stderr, st := run(t, r, c); st.Code != 0 {
			t.Fatalf("rcat: %+v %s", st, stderr)
		}
		defer run(t, r, lax(rcloneCmd(t, dirs, d, s, "deletefile", rclone.StorageRoot(d)+"/"+name)))
		out, stderr, st := run(t, r, rcloneCmd(t, dirs, d, s, "lsjson", "--files-only", rclone.StorageRoot(d)))
		if st.Code != 0 || strings.Contains(out, e.S3KeyID) || !strings.Contains(out, "probe-[REDACTED].txt") {
			t.Fatalf("lsjson: %q %+v %s", out, st, stderr)
		}
		c = rcloneCmd(t, dirs, d, s, "deletefile", rclone.StorageRoot(d)+"/"+name)
		if _, err := r.Start(context.Background(), c); !errors.Is(err, proc.ErrNotAllowed) {
			t.Fatalf("a command naming it: %v, want ErrNotAllowed", err)
		}
	})

	t.Run("crypt round trip on MinIO", func(t *testing.T) {
		d, s := e.S3(2, "crypt-smoke", true, obscure("crypt"))
		c := rcloneCmd(t, dirs, d, s, "rcat", rclone.Root(d)+"smoke/hello.txt")
		c.Stdin = strings.NewReader("hello through crypt\n")
		if _, stderr, st := run(t, r, c); st.Code != 0 {
			t.Fatalf("rcat: %+v %s", st, stderr)
		}
		out, stderr, st := run(t, r, rcloneCmd(t, dirs, d, s, "cat", rclone.Root(d)+"smoke/hello.txt"))
		if st.Code != 0 || out != "hello through crypt\n" {
			t.Fatalf("cat: %q %+v %s", out, st, stderr)
		}
		plain := d
		plain.Encryption = engines.EncryptionNone
		out, _, _ = run(t, r, rcloneCmd(t, dirs, plain, s, "lsjson", "-R", "--files-only", "--no-mimetype", rclone.StorageRoot(d)))
		if strings.Contains(out, "hello") || strings.TrimSpace(out) == "" {
			t.Errorf("the stored names are not encrypted: %q", out)
		}
		// rclone reveals the deterministic obscured form to the clear password.
		got, err := exec.Command(e.RclonePath, "reveal", s.Obscured[engines.FieldCryptPassword]).Output()
		if err != nil || strings.TrimSpace(string(got)) != s.Encryption.CryptPassword {
			t.Errorf("rclone reveal = %q, %v", got, err)
		}
	})

	t.Run("lsf SFTP with a passphrase-protected key", func(t *testing.T) {
		d, s := e.SFTP(3, "", false, obscure(engines.FieldPrivateKeyPassphrase))
		if s.Credentials.PrivateKeyPassphrase == "" {
			t.Fatal("the test key has no passphrase")
		}
		if _, stderr, st := run(t, r, rcloneCmd(t, dirs, d, s, "lsf", "--max-depth", "1", rclone.StorageRoot(d))); st.Code != 0 {
			t.Fatalf("exit %+v: %s", st, stderr)
		}
	})

	t.Run("lsf SFTP with a password", func(t *testing.T) {
		d, s := e.SFTP(4, "", true, obscure(engines.FieldPassword))
		if _, stderr, st := run(t, r, rcloneCmd(t, dirs, d, s, "lsf", "--max-depth", "1", rclone.StorageRoot(d))); st.Code != 0 {
			t.Fatalf("exit %+v: %s", st, stderr)
		}
	})

	t.Run("SFTP with another pinned key fails", func(t *testing.T) {
		d, s := e.SFTP(5, "", true, obscure(engines.FieldPassword))
		pub, _, _ := ed25519.GenerateKey(rand.Reader)
		pk, _ := ssh.NewPublicKey(pub)
		r2 := *d.Remote.SFTP
		r2.HostKeys = []engines.HostKey{{Type: pk.Type(), Key: base64.StdEncoding.EncodeToString(pk.Marshal())}}
		d.Remote.SFTP = &r2
		_, stderr, st := run(t, r, rcloneCmd(t, dirs, d, s, "lsf", "--max-depth", "1", "--retries", "1", "--low-level-retries", "1", rclone.StorageRoot(d)))
		if st.Code == 0 {
			t.Fatalf("an unpinned host key was accepted: %s", stderr)
		}
	})

	t.Run("rclone obscure reveals with rclone.Reveal", func(t *testing.T) {
		const throwaway = "enginebin-throwaway-value"
		out, err := exec.Command(e.RclonePath, "obscure", throwaway).Output()
		if err != nil {
			t.Fatal(err)
		}
		if got, err := rclone.Reveal(strings.TrimSpace(string(out))); err != nil || got != throwaway {
			t.Fatalf("Reveal(%q) = %q, %v", out, got, err)
		}
	})

	t.Run("rclone accepts the timetable strings", func(t *testing.T) {
		for _, cfg := range []bwlimit.Config{
			{UploadKiBps: 1024},
			{UploadKiBps: 100, Timetable: []bwlimit.Entry{{Days: []string{"mon", "tue"}, From: "23:00", To: "06:00", UploadKiBps: 4096, DownloadKiBps: 64}}},
			{Timetable: []bwlimit.Entry{{Days: []string{"sun"}, From: "22:00", To: "02:00", UploadKiBps: 8}}},
		} {
			limit, err := rclone.BWLimit(cfg)
			if err != nil {
				t.Fatal(err)
			}
			_, stderr, st := run(t, r, proc.Cmd{Binary: proc.Rclone, Args: []string{"version"}, Env: map[string]string{"RCLONE_CONFIG": "/dev/null", "RCLONE_BWLIMIT": limit}})
			if st.Code != 0 || strings.Contains(stderr, "bwlimit") {
				t.Errorf("RCLONE_BWLIMIT=%q: %+v %s", limit, st, stderr)
			}
		}
	})
}
