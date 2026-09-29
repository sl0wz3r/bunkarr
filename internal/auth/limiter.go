package auth

import (
	"net/netip"
	"slices"
	"sync"
	"time"
)

// maxTracked caps the limiter's memory; beyond it the least recently failing client is dropped.
const maxTracked = 10000

// Limiter blocks a client after too many failed logins within a window. A client is an IPv4
// address or an IPv6 /64 (limiterKey).
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

// Blocked reports whether client (an address, ClientIP's String) must wait, and for how long. A
// password check uses Attempt instead: Blocked, then Fail after the check, lets requests sent
// together all pass before the first failure is counted.
func (l *Limiter) Blocked(client string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.recent(limiterKey(client))
	if len(f) < l.max {
		return false, 0
	}
	return true, f[0].Add(l.window).Sub(l.now())
}

// Attempt starts one password check of client (an address, ClientIP's String): it reports
// whether the check may run and, when it may not, how long the client must wait. A check that may
// run counts as a failure at once, under the same lock as the test, so requests sent together
// cannot all pass before the first failure lands: Blocked, then Fail after a bcrypt compare, let
// a client that sent hundreds of guesses at once have every one checked. After the check the
// caller does nothing more for a wrong answer and calls Success for a right password; it calls
// undo when no guess was checked after all (a bad request, an internal error) or when a right
// answer must leave the count as it was. undo takes back this attempt only, and only once.
func (l *Limiter) Attempt(client string) (undo func(), wait time.Duration, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := limiterKey(client)
	if f := l.recent(key); len(f) >= l.max {
		return func() {}, f[0].Add(l.window).Sub(l.now()), false
	}
	at := l.record(key)
	var once sync.Once
	return func() { once.Do(func() { l.forget(key, at) }) }, 0, true
}

// Fail records a failed attempt.
func (l *Limiter) Fail(client string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.record(limiterKey(client))
}

// record appends a failure of key now and returns its time (caller holds mu).
func (l *Limiter) record(key string) time.Time {
	if _, ok := l.fails[key]; !ok && len(l.fails) >= maxTracked {
		l.prune()
	}
	at := l.now()
	l.fails[key] = append(l.recent(key), at)
	return at
}

// forget removes one failure of key recorded at, if it is still there (the window may have
// passed, or Success cleared the key).
func (l *Limiter) forget(key string, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.fails[key]
	i := slices.IndexFunc(f, at.Equal)
	if i < 0 {
		return
	}
	if f = slices.Delete(f, i, i+1); len(f) == 0 {
		delete(l.fails, key)
		return
	}
	l.fails[key] = f
}

// Success forgets the client's failures.
func (l *Limiter) Success(client string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, limiterKey(client))
}

// limiterKey is the bucket a client's failures count in: its IPv4 address, or its IPv6 /64. A
// device can use any number of addresses from its /64 (SLAAC, privacy addresses), so counting
// IPv6 addresses one by one would give it a fresh allowance every max guesses.
func limiterKey(client string) string {
	ip, err := netip.ParseAddr(client)
	if err != nil {
		return client
	}
	if ip = ip.Unmap(); ip.Is4() {
		return ip.String()
	}
	p, err := ip.WithZone("").Prefix(64)
	if err != nil {
		return client
	}
	return p.String()
}

// recent returns the key's failures inside the window (caller holds mu).
func (l *Limiter) recent(key string) []time.Time {
	cutoff := l.now().Add(-l.window)
	f := l.fails[key]
	i := 0
	for i < len(f) && !f[i].After(cutoff) {
		i++
	}
	f = f[i:]
	if len(f) == 0 {
		delete(l.fails, key)
		return nil
	}
	l.fails[key] = f
	return f
}

// prune drops expired entries and, if the map is still full, the key whose last failure is the
// oldest (caller holds mu). Clearing everything instead handed every blocked client, including
// the one that filled the map, a fresh allowance at once.
func (l *Limiter) prune() {
	var (
		oldest string
		at     time.Time
	)
	for k := range l.fails {
		if f := l.recent(k); len(f) > 0 && (oldest == "" || f[len(f)-1].Before(at)) {
			oldest, at = k, f[len(f)-1]
		}
	}
	if len(l.fails) >= maxTracked {
		delete(l.fails, oldest)
	}
}
