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
