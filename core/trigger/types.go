// Package trigger owns the IM-agnostic decision "should the bot reply to
// this inbound message, and for what reason?". IM adapters translate wire
// payloads into CanonicalInbound; a Classifier turns that plus a Policy
// into a structured TriggerDecision. The router consumes the decision; it
// does not produce it. The gateway consumes the decision; persona-as-grantor
// reply routing flows through ReplyRouting on the decision.
//
// This package replaces three places where the trigger decision used to
// live:
//
//   - core/im/octo/message.go::BotMessage.Triggers (the AIs-bool path that
//     incorrectly fired on WuKongIM follow-up messages, see issue #105);
//   - core/router/gate.go::groupGate (the mention-free fallback);
//   - core/im/octo/connector_inbound.go::isTrustedOBORelay /
//     shouldObserveBackground (the OBO trust gate + observe-vs-enqueue
//     branching).
//
// All three are now data: a Reason enum out of one Classifier.Classify call.
// New triggers (reply-to-bot, webhook, …) extend the enum, not the wiring.
package trigger

import (
	"time"

	"github.com/lml2468/octobuddy/core/persona"
)

// Reason is the closed enum of why a message did or did not trigger a turn.
// Used by the audit sink to log the actual cause of each decision (where the
// old Mentioned bool collapsed five different paths into one signal that
// operators couldn't tell apart).
type Reason string

const (
	// ReasonNone is the zero value. A decision with ReasonNone is illegal
	// (every classify call must return a concrete reason); guard with
	// ShouldReply / ShouldObserve, not equality to "".
	ReasonNone Reason = ""

	// --- accepted (turn) reasons ---

	// ReasonDM: a direct-message channel always triggers; no mention gate.
	ReasonDM Reason = "dm"
	// ReasonExplicitBot: the bot's own uid appeared in the @-mention list.
	// Highest-priority reason — beats persona, @AI, reply-to-bot.
	ReasonExplicitBot Reason = "explicit_bot"
	// ReasonPersonaGrantor: persona clone, the grantor's uid was @-mentioned.
	ReasonPersonaGrantor Reason = "persona_grantor"
	// ReasonPersonaHumans: persona clone, @所有人 (a human broadcast) — the
	// grantor is part of the addressed humans, so the clone speaks for them.
	ReasonPersonaHumans Reason = "persona_humans"
	// ReasonReplyToBot: the message is a quote-reply to the bot's own prior
	// message. Recovers the natural "continue the thread" UX that
	// AIBroadcastDeny would otherwise foreclose.
	ReasonReplyToBot Reason = "reply_to_bot"
	// ReasonAIBroadcast: pure @AI (AIs=1, no @所有人/@all). Gated by
	// Policy.AIBroadcast — Deny refuses outright (the bug fix default),
	// Allowlist refuses unless the channel is whitelisted, Allow keeps the
	// legacy behavior for migration.
	ReasonAIBroadcast Reason = "ai_broadcast"
	// ReasonMentionFreeGroup: the channel id is on the mention-free list (G12).
	ReasonMentionFreeGroup Reason = "mention_free_group"
	// ReasonCron: the message originated from the scheduler. Bypasses every
	// gate (mention, rate-limit, blocklist) — authenticity is the cron
	// owner's concern.
	ReasonCron Reason = "cron"

	// --- non-accepted reasons ---

	// ReasonObservation: the message should be remembered in groupctx as
	// background context for a later turn, but no reply runs. (The old
	// pipeline had two paths here — pre-gate Observe in the connector and
	// post-gate retroactive Observe in drainTurns — collapsed to one
	// gateway entry now.)
	ReasonObservation Reason = "observation"
	// ReasonOBOIrrelevant: an OBO v2 fan-out that does not address the
	// persona clone's grantor. Dropped BEFORE any session-state side effect,
	// so an irrelevant fan-out never leaks (openclaw R10 ordering).
	ReasonOBOIrrelevant Reason = "obo_irrelevant"
)

// AllReasons is the closed registry of every Reason constant. Used by
// tests that pin the IsAmbiguousAddressing partition: any new Reason
// added to the package must be appended here, and the partition test
// catches it if the new constant lacks an explicit ambiguous/unambiguous
// classification. Without this registry, a new Reason silently defaults
// to unambiguous (the switch's default branch) and the bot-loop guard
// stops applying — exactly the regression #118 set out to prevent.
var AllReasons = []Reason{
	ReasonNone,
	ReasonDM,
	ReasonExplicitBot,
	ReasonPersonaGrantor,
	ReasonPersonaHumans,
	ReasonReplyToBot,
	ReasonAIBroadcast,
	ReasonMentionFreeGroup,
	ReasonCron,
	ReasonObservation,
	ReasonOBOIrrelevant,
}

// IsAmbiguousAddressing reports whether the trigger reason represents a
// classification where the bot cannot tell from message metadata whether
// the sender genuinely wanted a reply. Currently only true for
// ReasonMentionFreeGroup — the chat is on the mention-free allowlist so
// every group message provisionally classifies as a reply candidate, but
// the addressing intent is unknown.
//
// The router's group bot-loop guard (G14) consults this rather than
// pattern-matching the specific enum value. Explicit @bot, persona,
// reply-to-bot, and AI-broadcast are all unambiguous addressing — peer-
// bot messages under those reasons are legitimate and must pass through
// even when the sender uid looks bot-shaped.
//
// To extend (e.g. add a future webhook reason without addressing info),
// add the constant to the switch. The router never changes.
func (r Reason) IsAmbiguousAddressing() bool {
	switch r {
	case ReasonMentionFreeGroup:
		return true
	default:
		return false
	}
}

// Source classifies the message origin. The router uses it to decide whether
// to bypass rate-limit/blocklist (only "cron" today). The wire envelope
// carries it so the GUI can badge cron-fired bubbles. New origins
// (webhook/replay/…) extend the enum; the router consults SourcePolicy
// rather than special-casing each constant.
type Source string

const (
	// SourceUser is a real IM-delivered message (default).
	SourceUser Source = "user"
	// SourceCron is an operator-scheduled synthetic fire (cron.json task).
	// Bypasses rate-limit and mention gates.
	SourceCron Source = "cron"
)

// ChannelKind is the IM-agnostic channel taxonomy. Only the DM/Group split
// matters for the trigger pipeline.
type ChannelKind int

const (
	ChannelUnknown ChannelKind = iota
	ChannelDM
	ChannelGroup
)

// MentionPayload is the IM-agnostic mention representation. The wire-level
// three-state bool|number flags are normalized to bool by the adapter
// (octo.truthy).
type MentionPayload struct {
	UIDs       []string
	AIsFlag    bool // @AI / @所有AI
	HumansFlag bool // @所有人 (Plan X)
	AllFlag    bool // legacy @所有人 (bool|number 1)
}

// ReplyContext describes a user-originated quote-reply relationship. The
// adapter populates TargetIsBot by comparing TargetFromUID with the bot's
// own uid, so the classifier never needs to know the bot uid for this rule.
type ReplyContext struct {
	TargetMessageID string
	TargetFromUID   string
	TargetIsBot     bool
}

// OBOSignal carries the IM-side OBO v2 (on-behalf-of) fields, untrusted
// until validated. The classifier sees these alongside the bot's own
// configured grantor; if FromUID != GrantorUID the signal is stripped
// (isTrustedOBORelay equivalence). Adapters that don't support OBO leave
// this nil.
type OBOSignal struct {
	OriginChannelID   string
	OriginChannelType int // adapter-specific channel-type code; 0 = unset
	OriginFromUID     string
	RespondAs         string // claimed grantor uid (may be forged)
}

// CanonicalInbound is the IM-agnostic inbound message that flows out of an
// adapter. It contains no policy, no persona, no router knobs — just the
// facts about what arrived.
type CanonicalInbound struct {
	Source     Source
	Channel    ChannelKind
	ChannelID  string
	SpaceID    string
	FromUID    string
	FromName   string
	Text       string
	MessageSeq int64
	Mention    *MentionPayload
	ReplyTo    *ReplyContext
	OBO        *OBOSignal
	Timestamp  time.Time
	Protocol   string // free-form tag for audit: "octo", "octo:test", "cron"
}

// ReplyRouting describes how the reply should be addressed. Filled in by
// the classifier when persona/OBO semantics warrant it; otherwise zero
// value (= reply as self to inbound channel). The IM adapter consults this
// when constructing its reply target.
type ReplyRouting struct {
	// OnBehalfOf is the grantor uid to stamp on the reply (so the IM
	// presents it as the grantor speaking). Empty when the bot replies as
	// itself.
	OnBehalfOf string
	// OBORerouteChannelID, when non-empty, redirects the reply to a
	// different channel than the inbound's own — used by OBO v2 to fan a
	// reply back to the origin group/DM. The adapter resolves the channel
	// type appropriately (an adapter-specific value is held in OBORerouteKind).
	OBORerouteChannelID string
	// OBORerouteKind is the adapter-specific channel-type code for the
	// reroute (echoing what the adapter put in CanonicalInbound.OBO.
	// OriginChannelType). 0 = no reroute / use inbound's channel.
	OBORerouteKind int
}

// HasOBOReroute reports whether the reply should go to a different channel
// than the inbound's own.
func (r ReplyRouting) HasOBOReroute() bool { return r.OBORerouteChannelID != "" }

// TriggerDecision is the classifier's output. Reason is a closed enum so
// the audit pipeline always knows exactly which rule fired. MatchedRules
// is free-form annotation for log debugging.
type TriggerDecision struct {
	Reason       Reason
	Source       Source
	ReplyRouting ReplyRouting
	MatchedRules []string
}

// ShouldReply reports whether this decision warrants running an agent turn.
// ReasonObservation / ReasonOBOIrrelevant mean "do not reply"; the former
// still records to groupctx as background, the latter is dropped silently.
func (td TriggerDecision) ShouldReply() bool {
	switch td.Reason {
	case ReasonNone, ReasonObservation, ReasonOBOIrrelevant:
		return false
	}
	return true
}

// ShouldObserve reports whether this decision should record the message
// into the group context window even though it did not warrant a reply.
// Only ReasonObservation observes; OBO-irrelevant drops are deliberately
// not recorded (the openclaw R10 leak guard).
func (td TriggerDecision) ShouldObserve() bool {
	return td.Reason == ReasonObservation
}

// PolicyGrantor is the grantor coordinates the classifier needs. Held as a
// dedicated value (rather than the full persona.Grantor) so trigger can
// stay decoupled from the prompt-building methods in core/persona.
type PolicyGrantor struct {
	UID  string
	Name string
}

// Configured reports whether a grantor is wired in (= this bot is a persona
// clone).
func (g PolicyGrantor) Configured() bool { return g.UID != "" }

// FromPersonaGrantor builds a PolicyGrantor from the persona package's
// Grantor — sole place trigger crosses to persona (a value copy). Kept as
// an explicit helper so callers don't sprinkle field-by-field copies.
func FromPersonaGrantor(g persona.Grantor) PolicyGrantor {
	return PolicyGrantor{UID: g.UID, Name: g.Name}
}
