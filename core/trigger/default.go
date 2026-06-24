package trigger

import "slices"

// DefaultClassifier is the production classifier. Rule precedence (highest
// to lowest, evaluated top-down):
//
//  1. ReasonCron   — Source==SourceCron bypasses every gate (operator-trusted).
//  2. ReasonDM     — DM channels always trigger (no mention gate).
//  3. OBO trust gate + relevance filter (persona clone, OBO v2): an OBO
//     signal not signed by the configured grantor is stripped; a
//     grantor-signed signal that does not address the grantor is dropped
//     with ReasonOBOIrrelevant BEFORE any session-state side effect.
//  4. ReasonExplicitBot — bot's own uid in the @-mention list. Beats
//     everything below it (a direct @bot is unambiguous intent and overrides
//     persona-grantor / reply-to-bot / @AI).
//  5. ReasonPersonaHumans / ReasonPersonaGrantor — for persona clones only,
//     @所有人 / @grantor widens the trigger so the clone speaks for the
//     grantor.
//  6. ReasonReplyToBot — quote-reply to one of the bot's own messages, if
//     Policy.ReplyToBotEnabled. Recovers the natural "continue the thread"
//     UX under AIBroadcastDeny.
//  7. ReasonAIBroadcast — pure @AI, gated by Policy.AIBroadcast (Deny is
//     the new default; the bug-fix path).
//  8. ReasonMentionFreeGroup — channel id is on the mention-free list (G12).
//  9. ReasonObservation — fall-through for group messages; recorded into
//     groupctx as background context, no reply.
//
// The classifier is pure: same inputs → same TriggerDecision, always.
type DefaultClassifier struct{}

// Classify applies the rules above. The MatchedRules slice records the
// specific rule label that fired (audit/debug aid; the Reason is the
// authoritative signal).
func (DefaultClassifier) Classify(in CanonicalInbound, p Policy) TriggerDecision {
	// 1. Cron bypasses everything — see SourceCron doc.
	if in.Source == SourceCron {
		return TriggerDecision{
			Source:       SourceCron,
			Reason:       ReasonCron,
			MatchedRules: []string{"cron_fire"},
			ReplyRouting: cronReplyRouting(p),
		}
	}

	src := in.Source
	if src == "" {
		src = SourceUser
	}

	// OBO trust + relevance evaluated up front so its reroute / drop applies
	// uniformly to DM and Group branches. An untrusted OBO signal is silently
	// stripped (oboTrusted=false, no reroute). A trusted but irrelevant
	// payload short-circuits with ReasonOBOIrrelevant BEFORE any session
	// state is touched (openclaw R10 ordering invariant — keep this gate
	// ahead of every accepted branch, including DM).
	oboReroute, oboTrusted := evaluateOBOTrust(in, p)
	if oboTrusted && !personaRelevant(in.Mention, p.Grantor) {
		return TriggerDecision{
			Source:       src,
			Reason:       ReasonOBOIrrelevant,
			MatchedRules: []string{"obo_v2_relevance_drop"},
		}
	}

	// Helper: layer OBO reroute / grantor stamp onto any accepted decision.
	withRouting := func(r ReplyRouting) ReplyRouting {
		if oboReroute.HasOBOReroute() {
			r.OBORerouteChannelID = oboReroute.OBORerouteChannelID
			r.OBORerouteKind = oboReroute.OBORerouteKind
		}
		// OBO v2 trusted relays always stamp the grantor uid (mirrors
		// oboReplyTarget's unconditional on_behalf_of).
		if oboTrusted && r.OnBehalfOf == "" {
			r.OnBehalfOf = p.Grantor.UID
		}
		return r
	}

	// 2. DM always triggers.
	if in.Channel == ChannelDM {
		return TriggerDecision{
			Source:       src,
			Reason:       ReasonDM,
			MatchedRules: []string{"dm_auto"},
			ReplyRouting: withRouting(ReplyRouting{}),
		}
	}

	// 3. Explicit @bot beats every group rule below.
	if in.Mention != nil && containsUID(in.Mention.UIDs, p.BotUID) {
		return TriggerDecision{
			Source:       src,
			Reason:       ReasonExplicitBot,
			MatchedRules: []string{"explicit_bot_uid"},
			ReplyRouting: withRouting(ReplyRouting{}),
		}
	}

	// 4. Persona widening (clone only).
	if p.Grantor.Configured() && in.Mention != nil {
		if in.Mention.HumansFlag || in.Mention.AllFlag {
			return TriggerDecision{
				Source:       src,
				Reason:       ReasonPersonaHumans,
				MatchedRules: []string{"persona_humans"},
				ReplyRouting: withRouting(ReplyRouting{OnBehalfOf: p.Grantor.UID}),
			}
		}
		if containsUID(in.Mention.UIDs, p.Grantor.UID) {
			return TriggerDecision{
				Source:       src,
				Reason:       ReasonPersonaGrantor,
				MatchedRules: []string{"persona_grantor_uid"},
				ReplyRouting: withRouting(ReplyRouting{OnBehalfOf: p.Grantor.UID}),
			}
		}
	}

	// 5. Reply-to-bot: a user quoted one of our messages.
	if p.ReplyToBotEnabled && in.ReplyTo != nil && in.ReplyTo.TargetIsBot {
		// Persona clones replying in a quote-thread must keep speaking
		// as the grantor — without this stamp the persona identity
		// breaks mid-conversation (the bot replies as itself to a
		// quote of its own prior persona-voiced message).
		routing := ReplyRouting{}
		if p.Grantor.Configured() {
			routing.OnBehalfOf = p.Grantor.UID
		}
		return TriggerDecision{
			Source:       src,
			Reason:       ReasonReplyToBot,
			MatchedRules: []string{"reply_to_bot"},
			ReplyRouting: withRouting(routing),
		}
	}

	// 6. Pure @AI — gated.
	if in.Mention != nil && !in.Mention.AllFlag && !in.Mention.HumansFlag && in.Mention.AIsFlag {
		if reason, rule, ok := evaluateAIBroadcast(in.ChannelID, p); ok {
			return TriggerDecision{
				Source:       src,
				Reason:       reason,
				MatchedRules: []string{rule},
				ReplyRouting: withRouting(ReplyRouting{}),
			}
		}
	}

	// 7. Mention-free group.
	if p.MentionFreeGroups[in.ChannelID] {
		return TriggerDecision{
			Source:       src,
			Reason:       ReasonMentionFreeGroup,
			MatchedRules: []string{"mention_free_group"},
			ReplyRouting: withRouting(ReplyRouting{}),
		}
	}

	// 8. Fall-through: observation only.
	return TriggerDecision{
		Source:       src,
		Reason:       ReasonObservation,
		MatchedRules: []string{"no_match"},
		ReplyRouting: ReplyRouting{},
	}
}

// containsUID reports whether uid (non-empty) is in the slice.
func containsUID(uids []string, uid string) bool {
	if uid == "" {
		return false
	}
	return slices.Contains(uids, uid)
}

// cronReplyRouting stamps on_behalf_of on cron-fired replies when persona is
// configured, mirroring the legacy EnqueueCron behavior so a cron fire from
// a persona clone still speaks in the grantor's voice.
func cronReplyRouting(p Policy) ReplyRouting {
	if p.Grantor.Configured() {
		return ReplyRouting{OnBehalfOf: p.Grantor.UID}
	}
	return ReplyRouting{}
}

// evaluateAIBroadcast returns (reason, rule, accepted) for the @AI path
// based on Policy.AIBroadcast. An unset/invalid policy defaults to Deny.
func evaluateAIBroadcast(channelID string, p Policy) (Reason, string, bool) {
	switch p.effectiveAIBroadcast() {
	case AIBroadcastAllow:
		return ReasonAIBroadcast, "ai_broadcast_allow", true
	case AIBroadcastAllowlist:
		if p.AIBroadcastAllowlist[channelID] {
			return ReasonAIBroadcast, "ai_broadcast_allowlist", true
		}
	}
	// Deny / allowlist miss: refuse the @AI path. The caller falls through
	// to mention-free / observation.
	return ReasonNone, "", false
}
