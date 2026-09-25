package auth

import (
	"sync"
	"time"
)

// maxTracked caps the limiter's memory; beyond it the oldest entries are dropped.
const maxTracked = 10000

// Limiter blocks a client (by IP) after too many failed logins within a window.
type Limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	now    func() time.Time
	fails  map[string][]time.Time
}

// NewLimiter allows max failures per client within window.
func NewLimiter(max int, window time.Duration) *Limiter {
	return &Limiter{max: max, window: window, now: time.Now, fails: map[string][]time.Time{}}
}

// Blocked reports whether client must wait, and for how long.
func (l *Limiter) Blocked(client string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.recent(client)
	if len(f) < l.max {
		return false, 0
	}
	return true, f[0].Add(l.window).Sub(l.now())
}

// Fail records a failed attempt.
func (l *Limiter) Fail(client string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.fails) >= maxTracked {
		l.prune()
	}
	l.fails[client] = append(l.recent(client), l.now())
}

// Success forgets the client's failures.
func (l *Limiter) Success(client string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, client)
}

// recent returns the client's failures inside the window (caller holds mu).
func (l *Limiter) recent(client string) []time.Time {
	cutoff := l.now().Add(-l.window)
	f := l.fails[client]
	i := 0
	for i < len(f) && !f[i].After(cutoff) {
		i++
	}
	f = f[i:]
	if len(f) == 0 {
		delete(l.fails, client)
		return nil
	}
	l.fails[client] = f
	return f
}

// prune drops expired entries, and everything if that is not enough (caller holds mu).
func (l *Limiter) prune() {
	for c := range l.fails {
		l.recent(c)
	}
	if len(l.fails) >= maxTracked {
		clear(l.fails)
	}
}
