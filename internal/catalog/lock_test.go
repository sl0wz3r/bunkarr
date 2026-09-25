package catalog

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSourceLocks(t *testing.T) {
	ctx := context.Background()
	l := newSourceLocks()

	unlock1, err := l.lock(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !l.held(1) || l.held(2) {
		t.Fatal("held reports the wrong locks")
	}
	// Other sources are independent.
	unlock2, err := l.lock(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	unlock2()
	if _, ok := l.tryLock(1); ok {
		t.Fatal("tryLock took a held lock")
	}
	tctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := l.lock(tctx, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock of a held source = %v", err)
	}
	cctx, cancel2 := context.WithCancel(ctx)
	cancel2()
	if _, err := l.lock(cctx, 3); !errors.Is(err, context.Canceled) {
		t.Fatalf("lock with a cancelled context = %v", err)
	}

	got := make(chan func(), 1)
	go func() {
		u, err := l.lock(ctx, 1)
		if err != nil {
			t.Error(err)
		}
		got <- u
	}()
	select {
	case <-got:
		t.Fatal("second lock did not wait")
	case <-time.After(20 * time.Millisecond):
	}
	unlock1()
	unlock1() // idempotent: must not release the waiter's lock
	u := <-got
	if !l.held(1) {
		t.Fatal("waiter does not hold the lock")
	}
	u()
	u2, ok := l.tryLock(1)
	if !ok {
		t.Fatal("tryLock of a free lock failed")
	}
	u2()
	l.mu.Lock()
	n := len(l.m)
	l.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d lock entries leaked", n)
	}
}
