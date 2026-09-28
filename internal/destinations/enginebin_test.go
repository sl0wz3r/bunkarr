//go:build enginebin

package destinations

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/sl0wz3r/bunkarr/internal/config"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/enginetest"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
	"github.com/sl0wz3r/bunkarr/internal/engines/rclone"
)

// The real-binary tests of the destinations store (make test-engines with
// ENGINES_PACKAGES=./internal/destinations/): create, attach, update and a failed create with
// rclone 1.74 against MinIO and the SFTP server of docker/test-engines.sh.

// realRclone is a store whose rclone engine is the real driver (wrap may replace it), with a
// fresh keyring and database: a new Bunkarr, as after the loss of /config.
func realRclone(t *testing.T, wrap func(*rclone.Driver) engines.Engine) (*fixture, *rclone.Driver, enginetest.RealEnvironment) {
	t.Helper()
	e := enginetest.RealEnv(t)
	master := make([]byte, 32)
	_, _ = rand.Read(master)
	kr, err := config.NewKeyring(master)
	if err != nil {
		t.Fatal(err)
	}
	configDir := t.TempDir()
	runner := proc.NewExecRunner(proc.ExecOptions{ResticPath: e.ResticPath, RclonePath: e.RclonePath, TermAfter: 20 * time.Second})
	drv := &rclone.Driver{Runner: runner, RunDirs: proc.NewRunDirs(configDir, proc.RunDirOptions{}), Version: "real"}
	var eng engines.Engine = rcloneEngine{drv}
	if wrap != nil {
		eng = wrap(drv)
	}
	f := newFixture(t, Options{Keyring: kr, ConfigDir: configDir, Now: func() time.Time { return time.Now().UTC() },
		Engines: func(k engines.Kind) (engines.Engine, bool) { return eng, k == engines.Rclone }})
	return f, drv, e
}

func realPrefix(name string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "destinations/" + name + "-" + hex.EncodeToString(b)
}

// rcloneEngine is the real driver as an engines.Engine; these tests run no job (no Open).
type rcloneEngine struct{ *rclone.Driver }

func (rcloneEngine) Open(context.Context, engines.Destination, engines.Secrets, engines.Runtime) (engines.Session, error) {
	return nil, errors.New("no job runs in the destinations tests")
}

// realS3Input is an rclone crypt destination on the MinIO bucket under prefix.
func realS3Input(t *testing.T, e enginetest.RealEnvironment, name, prefix string) Input {
	return Input{Name: name, Kind: engines.S3, Engine: EngineRclone,
		Remote: rawJSON(t, map[string]any{"provider": "Minio", "endpoint": e.S3Endpoint, "region": "us-east-1", "bucket": e.S3Bucket,
			"prefix": prefix, "forcePathStyle": true}),
		Credentials: credsJSON(t, map[string]string{"accessKeyId": e.S3KeyID, "secretAccessKey": e.S3Secret})}
}

// A crypt remote made with a generated password2 is attached again by a new Bunkarr from the
// recovery kit's two passwords; the password alone reads nothing (rclone's default salt).
func TestRealRcloneCryptReattachFromKit(t *testing.T) {
	f, _, e := realRclone(t, nil)
	prefix := realPrefix("reattach")
	d, err := f.store.Create(f.ctx, realS3Input(t, e, "Crypt", prefix), CreateOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	kit, err := f.store.RecoveryKit(f.ctx, d.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	k := kitJSONBlock(t, kit.Content)
	if k.Encryption.CryptPassword == "" || k.Encryption.CryptPassword2 == "" {
		t.Fatal("the kit lacks a crypt password")
	}

	g, _, _ := realRclone(t, nil)
	in := realS3Input(t, e, "Crypt again", prefix)
	only := &EncryptionInput{Secret: k.Encryption.CryptPassword}
	both := &EncryptionInput{Secret: k.Encryption.CryptPassword, Secret2: k.Encryption.CryptPassword2}
	res, err := g.store.TestRemote(g.ctx, TestInput{Kind: engines.S3, Engine: EngineRclone, Remote: in.Remote, Credentials: in.Credentials, Encryption: only})
	if err != nil || res.OK {
		t.Errorf("test with the password only: %+v %v", res, err)
	}
	in.Encryption = only
	if _, err := g.store.Create(g.ctx, in, CreateOptions{Attach: true}); err == nil {
		t.Fatal("attached with the password only")
	}
	res, err = g.store.TestRemote(g.ctx, TestInput{Kind: engines.S3, Engine: EngineRclone, Remote: in.Remote, Credentials: in.Credentials, Encryption: both})
	if err != nil || !res.OK || res.ID != d.MarkerID {
		t.Errorf("test with the kit's two passwords: %+v %v", res, err)
	}
	in.Encryption = both
	back, err := g.store.Create(g.ctx, in, CreateOptions{Attach: true})
	if err != nil {
		t.Fatalf("attach with the kit's two passwords: %v", err)
	}
	if back.MarkerID != d.MarkerID || Blocked(back) != "" {
		t.Errorf("attached %+v (blocked %q)", back, Blocked(back))
	}
	if res, err := g.store.TestStored(g.ctx, back.ID); err != nil || !res.OK {
		t.Errorf("stored test after the attach: %+v %v", res, err)
	}
}

// cancelAfterCreate runs the real Create, then cancels the create's context: the budget runs out
// right after the marker was written, so the create cannot finish.
type cancelAfterCreate struct {
	rcloneEngine
	cancel context.CancelFunc
}

func (c *cancelAfterCreate) Create(ctx context.Context, d engines.Destination, s engines.Secrets, attach bool) (engines.CreateResult, error) {
	res, err := c.Driver.Create(ctx, d, s, attach)
	if c.cancel != nil {
		c.cancel()
	}
	return res, err
}

// A crypt create that fails after its marker was written removes the marker before it drops the
// row (with the only copy of the password): a later create of the location works.
func TestRealRcloneFailedCreateRemovesMarker(t *testing.T) {
	wrapper := &cancelAfterCreate{}
	f, _, e := realRclone(t, func(d *rclone.Driver) engines.Engine { wrapper.rcloneEngine = rcloneEngine{d}; return wrapper })
	prefix := realPrefix("failed")
	ctx, cancel := context.WithCancel(f.ctx)
	wrapper.cancel = cancel
	if _, err := f.store.Create(ctx, realS3Input(t, e, "Failed", prefix), CreateOptions{}); err == nil {
		t.Fatal("the create did not fail")
	}
	if all, _ := f.store.List(f.ctx); len(all) != 0 {
		t.Fatalf("rows after the failed create: %+v", all)
	}
	wrapper.cancel = nil
	d, err := f.store.Create(f.ctx, realS3Input(t, e, "Second", prefix), CreateOptions{})
	if err != nil {
		t.Fatalf("a create of the location after the failed one: %v", err)
	}
	for _, w := range d.Warnings {
		if strings.Contains(w, "not empty") || strings.Contains(w, "non-empty") {
			t.Errorf("the failed create left objects: %v", d.Warnings)
		}
	}
	if res, err := f.store.TestStored(f.ctx, d.ID); err != nil || !res.OK {
		t.Errorf("stored test: %+v %v", res, err)
	}
}

// cancelOnReadBack is a runner that cancels the create's context when its marker has been
// written and is about to be read back: the create's budget (or its caller) ends inside Create.
type cancelOnReadBack struct {
	proc.Runner
	mu     sync.Mutex
	wrote  bool
	cancel context.CancelFunc
}

func (r *cancelOnReadBack) Start(ctx context.Context, c proc.Cmd) (proc.Process, error) {
	r.mu.Lock()
	if len(c.Args) > 0 && c.Args[0] == "rcat" {
		r.wrote = true
	} else if len(c.Args) > 0 && c.Args[0] == "cat" && r.wrote && r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
	r.mu.Unlock()
	return r.Runner.Start(ctx, c)
}

// A crypt create whose context ends between the marker's write and its read-back removes the
// marker itself (Create cannot finish, and the row with the only copy of the password goes): a
// later create of the location with another password works.
func TestRealRcloneCreateCancelledBeforeReadBack(t *testing.T) {
	runner := &cancelOnReadBack{}
	f, _, e := realRclone(t, func(d *rclone.Driver) engines.Engine {
		runner.Runner, d.Runner = d.Runner, runner
		return rcloneEngine{d}
	})
	prefix := realPrefix("readback")
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	runner.cancel = cancel
	if _, err := f.store.Create(ctx, realS3Input(t, e, "Cancelled", prefix), CreateOptions{}); err == nil {
		t.Fatal("the create did not fail")
	}
	if all, _ := f.store.List(f.ctx); len(all) != 0 {
		t.Fatalf("rows after the failed create: %+v", all)
	}
	d, err := f.store.Create(f.ctx, realS3Input(t, e, "Second", prefix), CreateOptions{})
	if err != nil {
		t.Fatalf("a create of the location after the cancelled one: %v", err)
	}
	for _, w := range d.Warnings {
		if strings.Contains(w, "not empty") {
			t.Errorf("the cancelled create left objects: %v", d.Warnings)
		}
	}
}

// An SFTP login rotated from an encrypted key to the same key unencrypted, then to the password,
// then back: each update is tested with the real rclone and stored as the whole login.
func TestRealSFTPCredentialRotation(t *testing.T) {
	f, drv, e := realRclone(t, nil)
	if e.SFTPKeyPassphrase == "" {
		t.Skip("the SFTP client key is not passphrase-protected")
	}
	sub := strings.ReplaceAll(realPrefix("rotate"), "/", "-")
	// Create needs an existing path: an rcat creates it.
	pd, ps := e.SFTP(0, sub, false, rclone.ObscureRandom)
	c, err := drv.Connect(pd, ps, engines.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Rcat(f.ctx, ".bunkarr/links.tsv", []byte{}); err != nil {
		t.Fatal(err)
	}
	keys := make([]map[string]string, 0, len(e.SFTPHostKeys))
	for _, k := range e.SFTPHostKeys {
		keys = append(keys, map[string]string{"type": k.Type, "key": k.Key})
	}
	d, err := f.store.Create(f.ctx, Input{Name: "SFTP", Kind: engines.SFTP, Engine: EngineRclone,
		Remote:      rawJSON(t, map[string]any{"host": e.SFTPHost, "port": e.SFTPPort, "user": e.SFTPUser, "path": e.SFTPPath + "/" + sub, "hostKeys": keys}),
		Credentials: credsJSON(t, map[string]string{"privateKey": e.SFTPKey, "privateKeyPassphrase": e.SFTPKeyPassphrase}),
		Encryption:  &EncryptionInput{Mode: engines.EncryptionNone, AcceptUnencrypted: true}}, CreateOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	raw, err := ssh.ParseRawPrivateKeyWithPassphrase([]byte(e.SFTPKey), []byte(e.SFTPKeyPassphrase))
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(raw, "bunkarr-engines")
	if err != nil {
		t.Fatal(err)
	}
	plain := string(pem.EncodeToMemory(block))
	steps := []struct {
		name   string
		update map[string]string
		want   engines.Credentials
	}{
		{"the key unencrypted", map[string]string{"privateKey": plain}, engines.Credentials{PrivateKey: plain}},
		{"the encrypted key again", map[string]string{"privateKey": e.SFTPKey, "privateKeyPassphrase": e.SFTPKeyPassphrase},
			engines.Credentials{PrivateKey: e.SFTPKey, PrivateKeyPassphrase: e.SFTPKeyPassphrase}},
	}
	if e.SFTPPassword != "" {
		steps = append(steps[:1], append([]struct {
			name   string
			update map[string]string
			want   engines.Credentials
		}{{"the password", map[string]string{"password": e.SFTPPassword}, engines.Credentials{Password: e.SFTPPassword}}}, steps[1:]...)...)
	}
	for _, s := range steps {
		if _, err := f.store.Update(f.ctx, d.ID, Input{Credentials: credsJSON(t, s.update)}); err != nil {
			t.Fatalf("rotate to %s: %v", s.name, err)
		}
		_, sec, err := f.store.SecretsFor(f.ctx, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		if sec.Credentials != s.want {
			t.Errorf("%s: stored fields %v, want %v", s.name, sec.Credentials.Fields(), s.want.Fields())
		}
		if res, err := f.store.TestStored(f.ctx, d.ID); err != nil || !res.OK {
			t.Errorf("%s: stored test %+v %v", s.name, res, err)
		}
	}
}
