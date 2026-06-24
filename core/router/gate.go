package router

import "context"

// Decision is the outcome of routing a message.
type Decision int

const (
	Accepted            Decision = iota
	DroppedUnroutable            // no session key
	DroppedNotMentioned          // group message without a mention
	DroppedTooLong               // exceeds MaxContentByte
	DroppedBot                   // silent drop: blocklisted DM or bot-loop guard (G14)
	RateLimited                  // rejected; first rejection of this window → notify the user
	RateLimitedSilent            // rejected but already notified this window → stay silent
)

// String returns the lowercase tag for the routing outcome (used in logs
// and metrics). Keep these stable — operators grep for them.
func (d Decision) String() string {
	switch d {
	case Accepted:
		return "accepted"
	case DroppedUnroutable:
		return "unroutable"
	case DroppedNotMentioned:
		return "not_mentioned"
	case DroppedTooLong:
		return "too_long"
	case DroppedBot:
		return "bot"
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

// RouteAndHandle applies all gates, then — if accepted — runs handler while
// holding the per-session lock (so same-session messages are strictly
// serialized). Returns the gate decision; handler errors are returned too.
//
// Gate ordering mirrors session-router.ts processMessage: bot blocklist /
// loop-guard drops (silent, DroppedBot) come before the mention gate, which
// itself honours mention-free groups (G12) and a per-room loop guard (G14).
func (r *Router) RouteAndHandle(ctx context.Context, msg InboundMessage, handler Handler) (Decision, error) {
	key, err := msg.SessionKey()
	if err != nil {
		return DroppedUnroutable, nil
	}

	if decision := r.routeGate(key, msg); decision != Accepted {
		return decision, nil
	}

	// Serialize within the session.
	e := r.acquire(key)
	defer r.release(e)

	if err := handler(ctx, key, msg); err != nil {
		return Accepted, err
	}
	return Accepted, nil
}

func (r *Router) routeGate(key string, msg InboundMessage) Decision {
	// DM blocklist + bot-loop guard (silent). Mirrors session-router.ts:
	// blocklisted DM senders, and DM senders that look like bots (unless
	// whitelisted) are dropped to prevent bot↔bot reply loops.
	if decision := r.dmGate(msg); decision != Accepted {
		return decision
	}

	// Group gating.
	if decision := r.groupGate(msg); decision != Accepted {
		return decision
	}

	// Content size gate.
	if len(msg.Text) > r.cfg.MaxContentByte {
		return DroppedTooLong
	}

	// Rate limiting (cron fires bypass it — operator-scheduled). The notify
	// decision is computed atomically with the rejection so the caller can
	// debounce the reply without re-locking (M1).
	if !msg.CronFire {
		if allowed, notify := r.allow(key, msg.FromUID); !allowed {
			if notify {
				return RateLimited
			}
			return RateLimitedSilent
		}
	}
	return Accepted
}

func (r *Router) dmGate(msg InboundMessage) Decision {
	if msg.ChannelType != ChannelDM || msg.CronFire {
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

func (r *Router) groupGate(msg InboundMessage) Decision {
	if msg.ChannelType != ChannelGroup || msg.CronFire {
		return Accepted
	}
	// Hard-drop blocklisted senders entirely (even if @-mentioned).
	if r.blocklisted[msg.FromUID] {
		return DroppedBot
	}
	// Mention gate. Skip unless mentioned OR the channel is mention-free (G12).
	if msg.Mentioned {
		return Accepted
	}
	if !r.mentionFree[msg.ChannelID] {
		return DroppedNotMentioned
	}
	// In a mention-free group there is no @mention gate to stop one bot replying
	// to another bot's plain text — drop bot-looking senders (unless
	// whitelisted) so two bots can't enter an unbounded loop.
	if r.looksLikeBot(msg.FromUID) && !r.allowedBots[msg.FromUID] {
		return DroppedBot
	}
	return Accepted
}
