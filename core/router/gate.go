package router

import (
	"context"

	"github.com/lml2468/octobuddy/core/trigger"
)

// Decision is the outcome of routing a message.
type Decision int

const (
	// Accepted: trigger said reply; gates passed; handler ran.
	Accepted Decision = iota
	// Observed: trigger said observation-only; gateway recorded into
	// groupctx as background, no reply. Distinct from a drop — the message
	// IS part of the conversation, just not addressed.
	Observed
	// DroppedUnroutable: missing identity (no from_uid for DM, no
	// channel_id for group). Silent.
	DroppedUnroutable
	// DroppedTooLong: text exceeds MaxContentByte. Gateway responds with
	// the oversized friendly reply.
	DroppedTooLong
	// DroppedBot: blocklist or G14 bot-loop guard. Silent.
	DroppedBot
	// DroppedOBOIrrelevant: persona clone received an OBO v2 fan-out that
	// did not address the grantor (openclaw R10 leak guard). Silent;
	// does NOT record into groupctx.
	DroppedOBOIrrelevant
	// DroppedInvariantBreak: programming-error signal. The router was
	// called with a message that doesn't satisfy its precondition (e.g.
	// the gateway is supposed to dispatch observations inline but didn't).
	// Distinct from DroppedOBOIrrelevant so audit doesn't mislabel a
	// dispatch bug as an OBO security drop — the two have very different
	// triage paths.
	DroppedInvariantBreak
	// RateLimited: rejected; first rejection of this window, gateway
	// notifies the user.
	RateLimited
	// RateLimitedSilent: rejected but already notified this window — stay
	// silent.
	RateLimitedSilent
)

// String returns the lowercase tag for the routing outcome (used in logs
// and metrics). Keep these stable — operators grep for them.
func (d Decision) String() string {
	switch d {
	case Accepted:
		return "accepted"
	case Observed:
		return "observed"
	case DroppedUnroutable:
		return "unroutable"
	case DroppedTooLong:
		return "too_long"
	case DroppedBot:
		return "bot"
	case DroppedOBOIrrelevant:
		return "obo_irrelevant"
	case DroppedInvariantBreak:
		return "invariant_break"
	case RateLimited:
		return "rate_limited"
	case RateLimitedSilent:
		return "rate_limited_silent"
	default:
		return "unknown"
	}
}

// Handler processes an accepted message under the session lock.
type Handler func(ctx context.Context, sessionKey string, msg InboundMessage) error

// RouteAndHandle applies all gates, then dispatches reply-warranting
// messages to handler under the per-session lock. Observations and OBO
// irrelevance never reach the router — the gateway handles those inline
// (single Observe entry point, issue #105 缺陷 4). Returns the gate
// decision; handler errors are returned too.
//
// Gate ordering: SessionKey (unroutable) → blocklist/loop guards →
// size → rate-limit → handler under lock.
//
// PRECONDITION: msg.ShouldReply() must be true (cron, DM auto-trigger, or
// a classifier decision with a reply-warranting Reason). The gateway
// asserts this before calling RouteAndHandle; passing an observation or
// OBO-irrelevant message here is a programming error and returns
// DroppedInvariantBreak (NOT DroppedOBOIrrelevant — auditing a dispatch
// bug as an OBO security drop would confuse triage).
func (r *Router) RouteAndHandle(ctx context.Context, msg InboundMessage, handler Handler) (Decision, error) {
	key, err := msg.SessionKey()
	if err != nil {
		return DroppedUnroutable, nil
	}
	if !msg.ShouldReply() {
		// Defensive: gateway is supposed to dispatch observations
		// without going through here. If it doesn't, refuse silently.
		// The dispatcher OBO-irrelevant short-circuit also lives in the
		// gateway; OBO drops never reach this branch in normal flow.
		return DroppedInvariantBreak, nil
	}

	// Blocklist / bot-loop guards.
	if decision := r.dmGate(msg); decision != Accepted {
		return decision, nil
	}
	if decision := r.groupBotGuards(msg); decision != Accepted {
		return decision, nil
	}

	// Content size gate.
	if len(msg.Text) > r.cfg.MaxContentByte {
		return DroppedTooLong, nil
	}

	// Rate limiting (cron fires bypass it — operator-scheduled).
	if !msg.IsCron() {
		if allowed, notify := r.allow(key, msg.FromUID); !allowed {
			if notify {
				return RateLimited, nil
			}
			return RateLimitedSilent, nil
		}
	}

	// Serialize within the session.
	e := r.acquire(key)
	defer r.release(e)

	if err := handler(ctx, key, msg); err != nil {
		return Accepted, err
	}
	return Accepted, nil
}

func (r *Router) dmGate(msg InboundMessage) Decision {
	if msg.ChannelType != ChannelDM || msg.IsCron() {
		return Accepted
	}
	if r.blocklisted[msg.FromUID] {
		return DroppedBot
	}
	if r.looksLikeBot(msg.FromUID) && !r.allowedBots[msg.FromUID] {
		return DroppedBot
	}
	return Accepted
}

// groupBotGuards applies the group-channel blocklist + (mention-free)
// bot-loop guard. Unlike the legacy groupGate, it does NOT own the
// mention-vs-observe decision — that lives in trigger.Classify.
//
// The loop guard fires ONLY for ReasonMentionFreeGroup (issue #105 fix
// after live-traffic regression: pre-refactor the legacy gate only fired
// the loop guard inside mention-free channels; a wider "all non-explicit
// reply reasons" gate dropped legitimate peer-bot @-grantor mentions and
// peer-bot quote-replies in normal groups). In a normal group the
// classifier returned ExplicitBot / PersonaGrantor / ReplyToBot — these
// pass through. In a mention-free group, an unmentioned message classified
// as MentionFreeGroup is still subject to the loop guard.
//
// Blocklist runs regardless of trigger reason (operator-driven hard drop).
func (r *Router) groupBotGuards(msg InboundMessage) Decision {
	if msg.ChannelType != ChannelGroup || msg.IsCron() {
		return Accepted
	}
	// Hard-drop blocklisted senders entirely (even if @-mentioned).
	if r.blocklisted[msg.FromUID] {
		return DroppedBot
	}
	// Loop guard scope: ONLY mention-free triggers, matching legacy
	// gate.go behavior. Explicit @bot, persona, reply-to-bot, @AI are all
	// unambiguous intent in a normal mentioned group — let them through
	// even from bot-looking senders. (G14 only fires in mention-free
	// channels where there's no @ to tell us intent.)
	if msg.Trigger == nil || msg.Trigger.Reason != trigger.ReasonMentionFreeGroup {
		return Accepted
	}
	if r.looksLikeBot(msg.FromUID) && !r.allowedBots[msg.FromUID] {
		return DroppedBot
	}
	return Accepted
}
