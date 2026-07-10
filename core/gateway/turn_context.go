package gateway

import (
	"context"

	"github.com/lml2468/octobuddy/core/agent"
	"github.com/lml2468/octobuddy/core/router"
)

// TurnContext carries all per-turn mutable state through the fixed phases of
// runTurn (admit → compose → execute → deliver), replacing the by-address bool
// and reassigned locals that the previous single-function runTurn threaded by
// hand. One value per accepted turn; never shared across turns.
//
// The phase split makes two invariants structural rather than comment-enforced:
//   - the group-cursor rewind-unless-delivered guard (captured in admit,
//     discharged by finishCursor) reads exactly one field, tc.delivered;
//   - the "build group delta BEFORE caching the current message" ordering stays
//     inside composeTurn → buildGroupPrompt (unchanged), documented as owned by
//     the compose phase.
type TurnContext struct {
	ctx        context.Context
	sessionKey string
	msg        router.InboundMessage

	// req is the composed agent request (owned by composeTurn, read by execute).
	req agent.Request
	// resume is the resume id in flight; "" after a clear-and-retry-fresh
	// (owned by executeTurn).
	resume string
	// idle guards the driver stream (owned by executeTurn, read by deliverTurn).
	idle *idleGuard

	// result is the consumed attempt outcome (set by executeTurn, read by
	// deliverTurn).
	result agentAttemptResult
	// delivered is set true exactly when a reply/apology reached the sink. The
	// deferred finishCursor rewinds the group cursor iff this stayed false.
	delivered bool

	// preCursor / hasCursor capture the group cursor at admit time so a turn
	// that fails before delivering can roll it back (the message resurfaces in
	// the next [Recent group messages] delta).
	preCursor int64
	hasCursor bool
}
