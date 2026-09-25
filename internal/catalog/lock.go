package catalog

import (
	"context"
	"sync"
)

// sourceLocks is the in-process, per-source mutual exclusion used by scans, syncs (while they
// scan and plan) and the source edits that would disturb them (path change, delete).
type sourceLocks struct {
	mu sync.Mutex
	m  map[int64]*sourceLock
}

type sourceLock struct {
	// ch holds one token while the lock is held; a buffered channel makes waiting context-aware.
	ch   chan struct{}
	refs int // holders plus waiters; the entry is dropped at zero
}

func newSourceLocks() *sourceLocks {
	return &sourceLocks{m: map[int64]*sourceLock{}}
}

func (l *sourceLocks) ref(id int64) *sourceLock {
	l.mu.Lock()
	defer l.mu.Unlock()
	sl := l.m[id]
	if sl == nil {
		sl = &sourceLock{ch: make(chan struct{}, 1)}
		l.m[id] = sl
	}
	sl.refs++
	return sl
}

func (l *sourceLocks) unref(id int64, sl *sourceLock) {
	l.mu.Lock()
	defer l.mu.Unlock()
	sl.refs--
	if sl.refs == 0 {
		delete(l.m, id)
	}
}

func (l *sourceLocks) unlocker(id int64, sl *sourceLock) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			<-sl.ch
			l.unref(id, sl)
		})
	}
}

// lock waits for the lock of source id or for ctx to end.
func (l *sourceLocks) lock(ctx context.Context, id int64) (unlock func(), err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sl := l.ref(id)
	select {
	case sl.ch <- struct{}{}:
		return l.unlocker(id, sl), nil
	case <-ctx.Done():
		l.unref(id, sl)
		return nil, ctx.Err()
	}
}

// tryLock takes the lock of source id if it is free.
func (l *sourceLocks) tryLock(id int64) (unlock func(), ok bool) {
	sl := l.ref(id)
	select {
	case sl.ch <- struct{}{}:
		return l.unlocker(id, sl), true
	default:
		l.unref(id, sl)
		return nil, false
	}
}

// held reports whether someone holds the lock of source id.
func (l *sourceLocks) held(id int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	sl := l.m[id]
	return sl != nil && len(sl.ch) == 1
}

// LockSource takes the in-process lock of a source, waiting until it is free or ctx ends (then it
// returns ctx.Err()). Scans take it; a sync holds it while it scans and plans a source (use
// Scanner.ScanLocked then); editing a source's path and deleting a source need it free. The
// returned unlock is idempotent.
func (s *Store) LockSource(ctx context.Context, sourceID int64) (unlock func(), err error) {
	return s.locks.lock(ctx, sourceID)
}
