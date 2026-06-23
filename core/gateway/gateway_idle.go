package gateway

import (
	"context"
	"errors"
	"sync/atomic"
	"time"
)

// errDispatchTimeout is the cause attached to the per-turn idle deadline, so
// runTurn can distinguish its own timeout from a caller cancellation via
// context.Cause (M9).
var errDispatchTimeout = errors.New("dispatch idle timeout")

// idleGuard wraps the per-turn idle deadline plumbing. Reset on every event;
// expired reports whether OUR timer fired (vs a parent cancellation). When
// the timeout is <=0 every method is a no-op so callers stay branch-free.
type idleGuard struct {
	timeout time.Duration
	cancel  context.CancelCauseFunc
	timer   *time.Timer
	// done is set by the runTurn loop when it observes a successful
	// terminal event. expired() honors it so a race between AfterFunc
	// firing and the success event can't reroute a completed turn into
	// the timeout-reply branch.
	done atomic.Bool
}

// newIdleGuard returns a child ctx and a guard. With timeout <=0 the guard is
// inert (ctx unchanged, reset/stop/expired are no-ops).
func newIdleGuard(parent context.Context, timeout time.Duration) (context.Context, *idleGuard) {
	if timeout <= 0 {
		return parent, &idleGuard{}
	}
	ctx, cancel := context.WithCancelCause(parent)
	g := &idleGuard{timeout: timeout, cancel: cancel}
	// time.AfterFunc fires once after the idle window; reset Resets it. The
	// closure captures `cancel` so an expiry tags the cancellation with our
	// sentinel, letting expired tell our own timeout apart from a parent
	// cancellation (M9).
	g.timer = time.AfterFunc(timeout, func() { cancel(errDispatchTimeout) })
	return ctx, g
}

func (g *idleGuard) reset() {
	if g.timer != nil {
		g.timer.Reset(g.timeout)
	}
}

func (g *idleGuard) stop() {
	if g.timer == nil {
		return
	}
	// Only cancel with a nil cause when WE preempted the timer (Stop returns
	// true). If Stop returns false the AfterFunc has already fired (or is in
	// flight) and is about to call cancel(errDispatchTimeout); racing it with
	// cancel(nil) here would mis-classify a fired timer as a clean stop,
	// confusing context.Cause readers. cancel(nil) after a
	// non-nil cancel cause is a no-op, so this is safe either way — but
	// preferring "don't race" keeps the invariant explicit.
	if g.timer.Stop() {
		g.cancel(nil)
	}
}

func (g *idleGuard) markDone() {
	if g.timer != nil {
		g.done.Store(true)
	}
}

func (g *idleGuard) expired(ctx context.Context) bool {
	if g.timer == nil || g.done.Load() {
		return false
	}
	return errors.Is(context.Cause(ctx), errDispatchTimeout)
}
