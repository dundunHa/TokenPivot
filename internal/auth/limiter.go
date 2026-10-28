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
// clock. It never starts a goroutine: expired entries are replaced when the same
// key is next seen, so multi-instance deployments do not share counts.
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
// any of them counts. A limited call consumes nothing and reports the time left
// in the exhausted window.
func (l *Limiter) Take(email, ip string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
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

// current returns the live window for a key, replacing an expired one. The
// replacement is only stored once the caller records an attempt.
func (l *Limiter) current(key string, now time.Time) entry {
	e, ok := l.entries[key]
	if !ok || now.Sub(e.start) >= l.window {
		return entry{start: now}
	}
	return e
}
