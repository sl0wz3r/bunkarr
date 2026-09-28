package destinations

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/enginetest"
)

// removingEngine is a fake rclone engine with RemoveMarker (rclone.Driver's), recording its calls.
type removingEngine struct {
	*enginetest.FakeEngine
	mu      sync.Mutex
	removed []string
	// ctxErr is the context's error each RemoveMarker call saw.
	ctxErr []error
	err    error
}

func (e *removingEngine) RemoveMarker(ctx context.Context, _ engines.Destination, s engines.Secrets, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.removed = append(e.removed, id)
	e.ctxErr = append(e.ctxErr, ctx.Err())
	return e.err
}

// A create whose rclone marker was written but that failed afterwards (here the create's budget
// ran out right after the engine call, so finishCreate cannot write) removes the marker before
// it drops the row with the only copy of the crypt password; when the marker cannot be removed,
// the pending row stays, and a create of the same location finishes it.
func TestFailedRcloneCreateRemovesItsMarker(t *testing.T) {
	remote := func(t *testing.T, bucket string) Input {
		return Input{Name: "Crypt " + bucket, Kind: engines.S3, Engine: EngineRclone,
			Remote:      rawJSON(t, map[string]any{"provider": "Minio", "endpoint": "https://s3.example.com", "region": "us-east-1", "bucket": bucket}),
			Credentials: creds(t, fmt.Sprintf(`{"accessKeyId":%q,"secretAccessKey":%q}`, testAccessKey, testSecretKey))}
	}
	setup := func(t *testing.T, withRemove bool) (*engineFixture, *removingEngine, *context.CancelFunc) {
		ef := newEngineFixture(t)
		rm := &removingEngine{FakeEngine: ef.rclone}
		if withRemove {
			ef.store.opts.Engines = func(k engines.Kind) (engines.Engine, bool) {
				if k == engines.Rclone {
					return rm, true
				}
				return ef.restic, true
			}
		}
		var cancel context.CancelFunc
		written := map[string]engines.EncryptionSecret{}
		ef.rclone.OnCreate = func(_ context.Context, d engines.Destination, s engines.Secrets, attach bool) (engines.CreateResult, error) {
			if attach {
				if w, ok := written[d.Target]; !ok || w != s.Encryption {
					return engines.CreateResult{}, engines.ErrMarkerMissing
				}
				return engines.CreateResult{MarkerID: "marker-" + d.Target}, nil
			}
			written[d.Target] = s.Encryption
			if cancel != nil {
				cancel() // the budget runs out right after the marker was written
				cancel = nil
			}
			return engines.CreateResult{MarkerID: "marker-" + d.Target}, nil
		}
		return ef, rm, &cancel
	}
	failingCreate := func(t *testing.T, ef *engineFixture, cancel *context.CancelFunc, in Input, o CreateOptions) error {
		ctx, c := context.WithCancel(ef.ctx)
		defer c()
		*cancel = c
		_, err := ef.store.Create(ctx, in, o)
		return err
	}

	t.Run("removed", func(t *testing.T) {
		ef, rm, cancel := setup(t, true)
		if err := failingCreate(t, ef, cancel, remote(t, "removed"), CreateOptions{}); err == nil {
			t.Fatal("the create did not fail")
		}
		if len(rm.removed) != 1 || !strings.HasPrefix(rm.removed[0], "marker-") || rm.ctxErr[0] != nil {
			t.Fatalf("RemoveMarker calls %v (context errors %v)", rm.removed, rm.ctxErr)
		}
		if all, _ := ef.store.List(ef.ctx); len(all) != 0 {
			t.Errorf("rows after the failed create: %+v", all)
		}
	})

	t.Run("not removed", func(t *testing.T) {
		ef, rm, cancel := setup(t, true)
		rm.err = errors.New("the remote is unreachable")
		err := failingCreate(t, ef, cancel, remote(t, "kept"), CreateOptions{})
		if err == nil || !strings.Contains(err.Error(), "create it again to finish") {
			t.Fatalf("err = %v", err)
		}
		all, _ := ef.store.List(ef.ctx)
		if len(all) != 1 || !all[0].Pending {
			t.Fatalf("the pending row did not stay: %+v", all)
		}
		kit, err := ef.store.RecoveryKit(ef.ctx, all[0].ID, false)
		if err != nil {
			t.Fatal(err)
		}
		d, err := ef.store.Create(ef.ctx, remote(t, "kept"), CreateOptions{})
		if err != nil {
			t.Fatalf("finish the create: %v", err)
		}
		if d.ID != all[0].ID || d.Pending || !strings.HasPrefix(d.MarkerID, "marker-") {
			t.Errorf("finished %+v", d)
		}
		if err := ef.store.ConfirmKit(ef.ctx, d.ID, KitConfirmation{CheckCode: kitCheckCode(t, kit.Content)}); err != nil {
			t.Errorf("the kit exported while pending: %v", err)
		}
	})

	t.Run("engine without RemoveMarker", func(t *testing.T) {
		ef, _, cancel := setup(t, false)
		if err := failingCreate(t, ef, cancel, remote(t, "no-remover"), CreateOptions{}); err == nil {
			t.Fatal("the create did not fail")
		}
		if all, _ := ef.store.List(ef.ctx); len(all) != 1 || !all[0].Pending {
			t.Errorf("the pending row (the only copy of the secret) did not stay: %+v", all)
		}
	})

	t.Run("attach removes nothing", func(t *testing.T) {
		ef, rm, cancel := setup(t, true)
		in := remote(t, "attached")
		if _, err := ef.store.Create(ef.ctx, in, CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		d, _ := ef.store.List(ef.ctx)
		if err := ef.store.Delete(ef.ctx, d[0].ID, DeleteOptions{ConfirmLoseSecret: true}); err != nil {
			t.Fatal(err)
		}
		// An attach adopts the existing remote's marker; a failure after it must not remove it.
		in.Encryption = &EncryptionInput{Secret: "an attached crypt password", Secret2: "an attached crypt password2"}
		ef.rclone.OnCreate = func(context.Context, engines.Destination, engines.Secrets, bool) (engines.CreateResult, error) {
			if (*cancel) != nil {
				(*cancel)()
				*cancel = nil
			}
			return engines.CreateResult{MarkerID: "marker-existing"}, nil
		}
		if err := failingCreate(t, ef, cancel, in, CreateOptions{Attach: true}); err == nil {
			t.Fatal("the attach did not fail")
		}
		if len(rm.removed) != 0 {
			t.Errorf("an attach removed the existing marker: %v", rm.removed)
		}
		if all, _ := ef.store.List(ef.ctx); len(all) != 0 {
			t.Errorf("rows after the failed attach: %+v", all)
		}
	})
}

// Create itself can fail after it wrote the marker (its budget ends during the read-back) and
// not be able to remove it: it returns the marker's id with its error (engines.CreateResult). The
// store removes the marker then, and keeps the pending row, with the only copy of the crypt
// password, when it cannot; a create of the same location then finishes it.
func TestRcloneCreateThatCouldNotRemoveItsMarker(t *testing.T) {
	remote := func(t *testing.T, bucket string) Input {
		return Input{Name: "Crypt " + bucket, Kind: engines.S3, Engine: EngineRclone,
			Remote:      rawJSON(t, map[string]any{"provider": "Minio", "endpoint": "https://s3.example.com", "region": "us-east-1", "bucket": bucket}),
			Credentials: creds(t, fmt.Sprintf(`{"accessKeyId":%q,"secretAccessKey":%q}`, testAccessKey, testSecretKey))}
	}
	setup := func(t *testing.T, removeErr error) (*engineFixture, *removingEngine) {
		ef := newEngineFixture(t)
		rm := &removingEngine{FakeEngine: ef.rclone, err: removeErr}
		ef.store.opts.Engines = func(k engines.Kind) (engines.Engine, bool) {
			if k == engines.Rclone {
				return rm, true
			}
			return ef.restic, true
		}
		written := map[string]engines.EncryptionSecret{}
		ef.rclone.OnCreate = func(_ context.Context, d engines.Destination, s engines.Secrets, attach bool) (engines.CreateResult, error) {
			id := "marker-" + d.Target
			if attach {
				if w, ok := written[d.Target]; !ok || w != s.Encryption {
					return engines.CreateResult{}, engines.ErrMarkerMissing
				}
				return engines.CreateResult{MarkerID: id}, nil
			}
			written[d.Target] = s.Encryption
			return engines.CreateResult{MarkerID: id}, errors.New("rclone create: context deadline exceeded")
		}
		return ef, rm
	}

	t.Run("removed", func(t *testing.T) {
		ef, rm := setup(t, nil)
		if _, err := ef.store.Create(ef.ctx, remote(t, "removed"), CreateOptions{}); err == nil || !strings.Contains(err.Error(), "deadline") {
			t.Fatalf("err = %v", err)
		}
		if len(rm.removed) != 1 || !strings.HasPrefix(rm.removed[0], "marker-") || rm.ctxErr[0] != nil {
			t.Fatalf("RemoveMarker calls %v (context errors %v)", rm.removed, rm.ctxErr)
		}
		if all, _ := ef.store.List(ef.ctx); len(all) != 0 {
			t.Errorf("rows after the failed create: %+v", all)
		}
	})

	t.Run("not removed", func(t *testing.T) {
		ef, _ := setup(t, errors.New("the remote is unreachable"))
		_, err := ef.store.Create(ef.ctx, remote(t, "kept"), CreateOptions{})
		if err == nil || !strings.Contains(err.Error(), "create it again to finish") {
			t.Fatalf("err = %v", err)
		}
		all, _ := ef.store.List(ef.ctx)
		if len(all) != 1 || !all[0].Pending {
			t.Fatalf("the pending row (the only copy of the secret) did not stay: %+v", all)
		}
		d, err := ef.store.Create(ef.ctx, remote(t, "kept"), CreateOptions{})
		if err != nil {
			t.Fatalf("finish the create: %v", err)
		}
		if d.ID != all[0].ID || d.Pending || !strings.HasPrefix(d.MarkerID, "marker-") {
			t.Errorf("finished %+v", d)
		}
	})
}
