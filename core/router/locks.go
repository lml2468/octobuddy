package router

import "time"

// acquire returns the (locked) per-session entry for key, creating it if needed
// and bumping its in-flight refcount so Reap won't evict it mid-turn.
func (r *Router) acquire(key string) *lockEntry {
	r.locksMu.Lock()
	e, ok := r.locks[key]
	if !ok {
		e = &lockEntry{}
		r.locks[key] = e
	}
	e.refs++
	r.locksMu.Unlock()
	e.mu.Lock()
	return e
}

// release unlocks the entry and records the release time, dropping its refcount
// so it becomes eligible for reaping once idle.
func (r *Router) release(e *lockEntry) {
	e.mu.Unlock()
	r.locksMu.Lock()
	e.refs--
	e.lastUse = r.now()
	r.locksMu.Unlock()
}

// Reap evicts per-session locks and rate-limit buckets that have been idle
// longer than idle, bounding the three maps in a long-running daemon. A reaped
// entry is recreated in its initial state on the next turn for that key — which
// is equivalent to its idle state — so eviction never changes behavior. idle
// must exceed the rate-limit window (a bucket idle that long is necessarily full
// again). Returns the number of locks and buckets evicted.
func (r *Router) Reap(idle time.Duration) (locks, buckets int) {
	// Enforce the idle > rate-limit-window invariant rather than trusting the
	// caller: a bucket idle that long is necessarily full again, so evicting and
	// recreating it can't reset a flooder's partially-drained budget. Clamp up so
	// a too-small idle can never become a rate-limit bypass.
	if idle <= rateLimitWindow {
		idle = rateLimitWindow + time.Second
	}
	now := r.now()

	r.locksMu.Lock()
	for k, e := range r.locks {
		if e.refs == 0 && !e.lastUse.IsZero() && now.Sub(e.lastUse) > idle {
			delete(r.locks, k)
			locks++
		}
	}
	r.locksMu.Unlock()

	r.rlMu.Lock()
	for k, b := range r.perUser {
		if b.idleSince(now, idle) {
			delete(r.perUser, k)
			buckets++
		}
	}
	for k, b := range r.perSess {
		if b.idleSince(now, idle) {
			delete(r.perSess, k)
			buckets++
		}
	}
	r.rlMu.Unlock()
	return locks, buckets
}
