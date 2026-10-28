package auth

import (
	"sync"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock; no test sleeps to cross a window.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestLimiterEmailWindow(t *testing.T) {
	clock := newFakeClock()
	l := NewLimiter(clock.Now)
	for i := range limiterEmailLimit {
		if retryAfter, ok := l.Take("synthetic@example.com", "127.0.0.1"); !ok || retryAfter != 0 {
			t.Fatalf("attempt %d limited: retryAfter=%s", i+1, retryAfter)
		}
	}
	retryAfter, ok := l.Take("synthetic@example.com", "127.0.0.1")
	if ok {
		t.Fatal("email window did not limit the sixth attempt")
	}
	if retryAfter != limiterWindow {
		t.Fatalf("retryAfter = %s, want %s", retryAfter, limiterWindow)
	}
	clock.Advance(4 * time.Minute)
	retryAfter, ok = l.Take("synthetic@example.com", "127.0.0.1")
	if ok || retryAfter != 6*time.Minute {
		t.Fatalf("retryAfter mid window = %s ok=%t", retryAfter, ok)
	}
	// A different email from the same IP is still limited by the IP window only.
	if _, ok := l.Take("other@example.com", "127.0.0.1"); !ok {
		t.Fatal("second email was limited before its own window filled")
	}
	clock.Advance(6 * time.Minute)
	if _, ok := l.Take("synthetic@example.com", "127.0.0.1"); !ok {
		t.Fatal("window did not reset exactly at its boundary")
	}
}

func TestLimiterIPWindow(t *testing.T) {
	clock := newFakeClock()
	l := NewLimiter(clock.Now)
	for i := range limiterIPLimit {
		if _, ok := l.Take("user"+string(rune('a'+i%26))+"@example.com", "10.0.0.1"); !ok {
			t.Fatalf("attempt %d limited", i+1)
		}
	}
	retryAfter, ok := l.Take("fresh@example.com", "10.0.0.1")
	if ok || retryAfter != limiterWindow {
		t.Fatalf("IP window not enforced: retryAfter=%s ok=%t", retryAfter, ok)
	}
	// Another IP is unaffected.
	if _, ok := l.Take("fresh@example.com", "10.0.0.2"); !ok {
		t.Fatal("a different IP shared the exhausted window")
	}
}

func TestLimiterSuccessIsNotRefunded(t *testing.T) {
	clock := newFakeClock()
	l := NewLimiter(clock.Now)
	// A successful registration consumes a slot like a failed attempt.
	for range limiterEmailLimit {
		if _, ok := l.Take("synthetic@example.com", "127.0.0.1"); !ok {
			t.Fatal("early attempt limited")
		}
	}
	if _, ok := l.Take("synthetic@example.com", "127.0.0.1"); ok {
		t.Fatal("successful attempts were refunded")
	}
}

func TestLimiterConcurrentAttemptsStayWithinLimit(t *testing.T) {
	l := NewLimiter(nil)
	const callers = 64
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := l.Take("synthetic@example.com", "172.16.0.9"); ok {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != limiterEmailLimit {
		t.Fatalf("allowed = %d, want %d", allowed, limiterEmailLimit)
	}
}

func TestLimiterLazyExpiry(t *testing.T) {
	clock := newFakeClock()
	l := NewLimiter(clock.Now)
	if _, ok := l.Take("synthetic@example.com", "127.0.0.1"); !ok {
		t.Fatal("first attempt limited")
	}
	clock.Advance(limiterWindow)
	if _, ok := l.Take("synthetic@example.com", "127.0.0.1"); !ok {
		t.Fatal("expired entry not replaced")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.entries) != 2 {
		t.Fatalf("entries = %d, want only the two live keys", len(l.entries))
	}
}

// TestLimiterPrunesExpiredKeysOfOtherCallers proves a counting request removes
// windows of keys it does not touch: the old email and IP entries disappear
// without ever being visited again, so a long-running process cannot accumulate
// dead keys and no background goroutine is needed.
func TestLimiterPrunesExpiredKeysOfOtherCallers(t *testing.T) {
	clock := newFakeClock()
	l := NewLimiter(clock.Now)
	for i := range limiterEmailLimit {
		if _, ok := l.Take("stale@example.com", "10.0.0.1"); !ok {
			t.Fatalf("stale attempt %d limited", i+1)
		}
	}
	countEntries := func() int {
		l.mu.Lock()
		defer l.mu.Unlock()
		return len(l.entries)
	}
	if got := countEntries(); got != 2 {
		t.Fatalf("entries = %d, want the two keys of one email and one IP", got)
	}
	// A different email from a different IP is a brand new pair. Its own counting
	// request must sweep the now expired stale keys it never sees again.
	clock.Advance(limiterWindow)
	if _, ok := l.Take("fresh@example.com", "10.0.0.2"); !ok {
		t.Fatal("fresh attempt limited")
	}
	l.mu.Lock()
	_, staleEmail := l.entries[limiterEmailPrefix+"stale@example.com"]
	_, staleIP := l.entries[limiterIPPrefix+"10.0.0.1"]
	_, freshEmail := l.entries[limiterEmailPrefix+"fresh@example.com"]
	_, freshIP := l.entries[limiterIPPrefix+"10.0.0.2"]
	total := len(l.entries)
	l.mu.Unlock()
	if staleEmail || staleIP {
		t.Fatal("expired keys of other callers survived a counting request")
	}
	if !freshEmail || !freshIP || total != 2 {
		t.Fatalf("fresh keys = %t/%t total = %d, want the two live keys", freshEmail, freshIP, total)
	}
	// The pruned keys are usable again as fresh windows, not limited by stale state.
	if _, ok := l.Take("stale@example.com", "10.0.0.1"); !ok {
		t.Fatal("a pruned key stayed limited")
	}
}
