package auth

import (
	"sync"
	"time"
)

// Fixed-window rate limits. Login and register share both counters; a
// successful attempt is counted like any other and never refunded.
const (
	limiterWindow     = 10 * time.Minute
	limiterEmailLimit = 5
	limiterIPLimit    = 20
)

const (
	limiterEmailPrefix = "email:"
	limiterIPPrefix    = "ip:"
)

// Limiter is a single-process fixed-window attempt counter with an injectable
// clock. It never starts a goroutine: expired entries are cleaned lazily inside a
// counting request under the same mutex that guards the map, so keys that are
// never seen again still disappear and multi-instance deployments do not share
// counts.
type Limiter struct {
	mu         sync.Mutex
	now        func() time.Time
	window     time.Duration
	emailLimit int
	ipLimit    int
	entries    map[string]entry
}

// entry is one key's current fixed window.
type entry struct {
	start time.Time
	count int
}

// NewLimiter returns a limiter reading time from now; nil means time.Now.
func NewLimiter(now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{
		now:        now,
		window:     limiterWindow,
		emailLimit: limiterEmailLimit,
		ipLimit:    limiterIPLimit,
		entries:    make(map[string]entry),
	}
}

// Take checks the email and remote-IP windows and, when neither is exhausted,
// consumes one attempt from both inside the same critical section. This makes
// check and increment inseparable, so concurrent callers cannot all pass before
// any of them counts. Expired entries are pruned in the same critical section, so
// a key that is never seen again still leaves the map. A limited call consumes
// nothing and reports the time left in the exhausted window.
func (l *Limiter) Take(email, ip string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.pruneExpired(now)
	emailEntry := l.current(limiterEmailPrefix+email, now)
	ipEntry := l.current(limiterIPPrefix+ip, now)
	var retryAfter time.Duration
	limited := false
	if emailEntry.count >= l.emailLimit {
		limited = true
		retryAfter = max(retryAfter, l.window-now.Sub(emailEntry.start))
	}
	if ipEntry.count >= l.ipLimit {
		limited = true
		retryAfter = max(retryAfter, l.window-now.Sub(ipEntry.start))
	}
	if limited {
		return max(retryAfter, time.Second), false
	}
	emailEntry.count++
	ipEntry.count++
	l.entries[limiterEmailPrefix+email] = emailEntry
	l.entries[limiterIPPrefix+ip] = ipEntry
	return 0, true
}

// pruneExpired drops every window that has ended. It runs only inside a counting
// request and under the limiter mutex, so the map cannot grow forever with keys
// that are no longer used and no background goroutine is needed.
func (l *Limiter) pruneExpired(now time.Time) {
	for key, e := range l.entries {
		if now.Sub(e.start) >= l.window {
			delete(l.entries, key)
		}
	}
}

// current returns the live window for a key. pruneExpired has already removed
// every ended window in this request, so a missing key starts now.
func (l *Limiter) current(key string, now time.Time) entry {
	e, ok := l.entries[key]
	if !ok {
		return entry{start: now}
	}
	return e
}
