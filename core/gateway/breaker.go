package gateway

import (
	"sync"
	"time"
)

// breakerState is the circuit-breaker's current mode.
type breakerState int

const (
	breakerClosed   breakerState = iota // normal: turns spawn
	breakerOpen                         // short-circuit: turns reply busy without spawning
	breakerHalfOpen                     // one probe turn allowed to test recovery
)

// breaker is a per-driver circuit breaker on the upstream path. After a
// threshold of CONSECUTIVE transient terminal errors it OPENS for a cooldown;
// while open, turns short-circuit to busyReply WITHOUT spawning the CLI. After
// the cooldown it half-opens: the next turn is allowed through as a single probe;
// a success closes it, a failure re-opens with a fresh cooldown. ONLY transient
// terminal errors trip it — a normal terminal error or a per-turn bug must not
// open the breaker (that would mute the bot on its own faults). All methods are
// safe on a nil *breaker (opt-in: nil ⇒ always allow, never trips).
type breaker struct {
	mu        sync.Mutex
	now       func() time.Time
	threshold int
	cooldown  time.Duration

	state       breakerState
	consecFails int
	openedAt    time.Time
}

// allow reports whether a turn may spawn the driver now. It transitions
// open→halfOpen once the cooldown elapsed and returns true to permit the single
// probe; a second call before that probe's result stays gated. nil ⇒ always true.
func (b *breaker) allow() bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case breakerOpen:
		if b.now().Sub(b.openedAt) >= b.cooldown {
			b.state = breakerHalfOpen
			return true // single probe
		}
		return false
	case breakerHalfOpen:
		return false // probe already in flight
	default: // closed
		return true
	}
}

// onResult feeds a terminal outcome back. transient marks an upstream
// rate-limit/overload terminal error; ok marks a clean turn (no terminal error).
// A success or non-transient outcome resets to closed; a transient failure
// increments and may open (or, from half-open, re-opens). nil ⇒ no-op.
func (b *breaker) onResult(transient, ok bool) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	// Any success or non-transient outcome is healthy: reset.
	if ok || !transient {
		b.state = breakerClosed
		b.consecFails = 0
		return
	}

	// Transient failure.
	if b.state == breakerHalfOpen {
		// Probe failed — re-open with a fresh cooldown.
		b.state = breakerOpen
		b.openedAt = b.now()
		return
	}
	b.consecFails++
	if b.consecFails >= b.threshold {
		b.state = breakerOpen
		b.openedAt = b.now()
	}
}
