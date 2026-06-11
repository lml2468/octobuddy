package router

import "time"

// bucket is a simple token bucket: `capacity` tokens that fully refill over
// `window`. peek reports whether a token is available; take consumes one.
// Not goroutine-safe on its own — callers hold Router.rlMu.
type bucket struct {
	capacity   float64
	tokens     float64
	window     time.Duration
	lastRefill time.Time
	// notified debounces the rate-limit reply: it is set the first time this
	// bucket blocks a message and cleared the next time it admits one, so a
	// flooder gets at most one "请稍后再试" per refill window (mirrors
	// cc-channel session-router.ts TokenBucket.notified).
	notified bool
}

func newBucket(capacity int, window time.Duration) *bucket {
	return &bucket{
		capacity: float64(capacity),
		tokens:   float64(capacity),
		window:   window,
	}
}

func (b *bucket) refill(now time.Time) {
	if b.lastRefill.IsZero() {
		b.lastRefill = now
		return
	}
	elapsed := now.Sub(b.lastRefill)
	if elapsed <= 0 {
		return
	}
	ratePerSec := b.capacity / b.window.Seconds()
	b.tokens += ratePerSec * elapsed.Seconds()
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
	b.lastRefill = now
}

func (b *bucket) peek(now time.Time) bool {
	b.refill(now)
	return b.tokens >= 1
}

func (b *bucket) take(now time.Time) {
	b.refill(now)
	if b.tokens >= 1 {
		b.tokens--
	}
}

// idleSince reports whether the bucket has gone untouched (no peek/take, which
// both refresh lastRefill) for longer than idle. Callers pass idle >= window, so
// such a bucket has fully refilled — evicting and recreating it yields an
// identical full bucket. A never-used bucket (zero lastRefill) is not idle.
func (b *bucket) idleSince(now time.Time, idle time.Duration) bool {
	return !b.lastRefill.IsZero() && now.Sub(b.lastRefill) > idle
}
