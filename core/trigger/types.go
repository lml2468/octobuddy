// Package trigger owns the IM-agnostic decision "should the bot reply to
// this inbound, and for what reason?". IM adapters translate wire
// payloads into CanonicalInbound; a Classifier turns that plus a Policy
// into a TriggerDecision. The router and gateway consume the decision;
// neither produces it.
package trigger

import (
	"time"

	"github.com/lml2468/octobuddy/core/persona"
)

// Reason is the closed enum of why a message did or did not trigger a
// turn. The audit pipeline logs this verbatim, so operators can grep one
// stable tag per rule.
type Reason string

const (
	// ReasonNone is the zero value and an illegal decision result —
	// guard with ShouldReply / ShouldObserve, not equality.
	ReasonNone Reason = ""

	// --- reply-warranting reasons (rule precedence top-down) ---

	ReasonDM               Reason = "dm"                 // DM channels always trigger.
	ReasonExplicitBot      Reason = "explicit_bot"       // Bot's own uid was @-mentioned.
	ReasonPersonaGrantor   Reason = "persona_grantor"    // Persona clone, grantor uid was @-mentioned.
	ReasonPersonaHumans    Reason = "persona_humans"     // Persona clone, @所有人 broadcast.
	ReasonReplyToBot       Reason = "reply_to_bot"       // Quote-reply to one of the bot's prior messages.
	ReasonAIBroadcast      Reason = "ai_broadcast"       // Pure @AI, gated by Policy.AIBroadcast.
	ReasonMentionFreeGroup Reason = "mention_free_group" // Channel id is on the mention-free allowlist.
	ReasonCron             Reason = "cron"               // Scheduler fire — bypasses every gate.

	// --- non-reply reasons ---

	// ReasonObservation: remember in groupctx as background; no reply.
	ReasonObservation Reason = "observation"
	// ReasonOBOIrrelevant: trusted OBO fan-out that doesn't address the
	// grantor. Dropped BEFORE any session-state side effect so an
	// irrelevant fan-out never leaks.
	ReasonOBOIrrelevant Reason = "obo_irrelevant"
)

// AllReasons is the closed registry; the partition test asserts every
// constant is classified ambiguous-or-not so a new Reason can't silently
// default through IsAmbiguousAddressing's switch.
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

// IsAmbiguousAddressing reports whether the bot cannot tell from message
// metadata whether the sender meant to address it. The router's bot-loop
// guard fires only on ambiguous reasons — explicit @bot / persona /
// reply-to-bot / @AI carry clear intent and let peer-bot senders through.
// To extend, add the constant to the switch.
func (r Reason) IsAmbiguousAddressing() bool {
	switch r {
	case ReasonMentionFreeGroup:
		return true
	default:
		return false
	}
}

// Source classifies the message origin. SourceCron bypasses rate-limit
// and blocklist; the wire envelope carries it so the GUI can badge
// cron-fired bubbles.
type Source string

const (
	SourceUser Source = "user"
	SourceCron Source = "cron"
	// SourceConsole is a turn from the desktop Console over the authenticated
	// control bus. It arrives as a DM (so it triggers like any DM) but is
	// operator-trusted by construction — the desktop authenticates before any
	// session.send — so the gateway may treat it as the owner's channel even
	// before the bot has an IM-registered owner uid (e.g. first-run bootstrap on
	// a bot that hasn't connected to IM yet).
	SourceConsole Source = "console"
)

// ChannelKind is the IM-agnostic channel taxonomy; only DM/Group matters
// for the trigger pipeline.
type ChannelKind int

const (
	ChannelUnknown ChannelKind = iota
	ChannelDM
	ChannelGroup
)

// MentionPayload is the IM-agnostic mention shape. The wire-level
// three-state bool|number flags are normalized to bool by the adapter.
type MentionPayload struct {
	UIDs       []string
	AIsFlag    bool // @AI / @所有AI
	HumansFlag bool // @所有人
	AllFlag    bool // legacy @所有人 (bool|number 1)
}

// ReplyContext describes a user-originated quote-reply. The adapter
// pre-computes TargetIsBot so the classifier never needs the bot uid.
type ReplyContext struct {
	TargetMessageID string
	TargetFromUID   string
	TargetIsBot     bool
}

// OBOSignal carries IM-side on-behalf-of fields, untrusted until the
// classifier validates them against the configured grantor. Adapters
// without OBO leave this nil.
type OBOSignal struct {
	OriginChannelID   string
	OriginChannelType int // adapter-specific code; 0 = unset
	OriginFromUID     string
	RespondAs         string // claimed grantor uid (may be forged)
}

// CanonicalInbound is the IM-agnostic inbound — facts about what
// arrived, no policy, no persona, no router knobs.
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
	Protocol   string // free-form audit tag, e.g. "octo", "cron"
}

// ReplyRouting describes how the reply should be addressed. Zero value =
// reply as self to the inbound channel. The IM adapter consults this
// when constructing its reply target.
type ReplyRouting struct {
	// OnBehalfOf is the grantor uid the IM should present as the sender.
	// Empty = reply as the bot itself.
	OnBehalfOf string
	// OBORerouteChannelID redirects the reply to a different channel
	// (OBO v2 fan-back to the origin). Empty = use the inbound channel.
	OBORerouteChannelID string
	// OBORerouteKind is the adapter-specific channel-type code for the
	// reroute. 0 = no reroute.
	OBORerouteKind int
}

func (r ReplyRouting) HasOBOReroute() bool { return r.OBORerouteChannelID != "" }

// TriggerDecision is the classifier's output. MatchedRules is free-form
// annotation for debug logs; Reason is the authoritative signal.
type TriggerDecision struct {
	Reason       Reason
	Source       Source
	ReplyRouting ReplyRouting
	MatchedRules []string
}

// ShouldReply reports whether this decision warrants running an agent turn.
func (td TriggerDecision) ShouldReply() bool {
	switch td.Reason {
	case ReasonNone, ReasonObservation, ReasonOBOIrrelevant:
		return false
	}
	return true
}

// ShouldObserve reports whether the message should be recorded into the
// group-context window. OBO-irrelevant drops are deliberately NOT
// observed (leak guard).
func (td TriggerDecision) ShouldObserve() bool {
	return td.Reason == ReasonObservation
}

// PolicyGrantor is the grantor coordinates the classifier needs. Kept
// separate from persona.Grantor so trigger stays decoupled from the
// prompt-building methods.
type PolicyGrantor struct {
	UID  string
	Name string
}

// Configured reports whether a grantor is wired in (= persona clone).
func (g PolicyGrantor) Configured() bool { return g.UID != "" }

// FromPersonaGrantor is the sole bridge between persona and trigger.
func FromPersonaGrantor(g persona.Grantor) PolicyGrantor {
	return PolicyGrantor{UID: g.UID, Name: g.Name}
}
