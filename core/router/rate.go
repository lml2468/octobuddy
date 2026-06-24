package router

// allow checks all three buckets; only consumes a token from each if ALL pass
// (peek-then-commit), mirroring cc-channel's checkAllRateLimits. When a turn is
// rejected it ALSO decides — in the same critical section — whether this is the
// first rejection of the current window (notify=true) so the caller can debounce
// the "请稍后再试" reply without a second lock acquisition (M1: a second lock would
// let the buckets refill between the reject and the notify check, racing the
// blocker classification). Returns (allowed, notify); notify is meaningful only
// when allowed is false.
func (r *Router) allow(sessionKey, uid string) (allowed, notify bool) {
	r.rlMu.Lock()
	defer r.rlMu.Unlock()
	now := r.now()

	maxPer := r.cfg.MaxPerMinute
	if r.global == nil {
		r.global = newBucket(maxPer*globalRateMultiplier, rateLimitWindow)
	}
	ub := r.perUser[uid]
	if ub == nil {
		ub = newBucket(maxPer, rateLimitWindow)
		r.perUser[uid] = ub
	}
	sb := r.perSess[sessionKey]
	if sb == nil {
		sb = newBucket(maxPer, rateLimitWindow)
		r.perSess[sessionKey] = sb
	}

	// Identify the blocking bucket in precedence order (global → user → session),
	// peeking each exactly once. On a block, mark that bucket notified and report
	// whether this was the first rejection of its window — all under rlMu.
	switch {
	case !r.global.peek(now):
		return false, markNotified(r.global)
	case !ub.peek(now):
		return false, markNotified(ub)
	case !sb.peek(now):
		return false, markNotified(sb)
	}
	r.global.take(now)
	ub.take(now)
	sb.take(now)
	// A turn got through — clear the debounce flags so the NEXT time any bucket
	// blocks, the user is notified again (cc-channel checkAllRateLimits clears
	// notified on the all-pass path).
	r.global.notified = false
	ub.notified = false
	sb.notified = false
	return true, false
}

// markNotified sets the bucket's debounce flag and reports whether this was the
// first rejection of the current window (flag flipped from false to true).
func markNotified(b *bucket) bool {
	if b.notified {
		return false
	}
	b.notified = true
	return true
}
