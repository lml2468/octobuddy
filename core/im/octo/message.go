package octo

import "github.com/lml2468/octobuddy/core/persona"

// mentionsBot reports whether this message @-mentions the given bot uid, or
// addresses all AIs. Mirrors session-router's group mention gate (uids or ais,
// NOT humans-only @all).
func (m BotMessage) MentionsBot(botUID string) bool {
	if m.Payload.Mention == nil {
		return false
	}
	for _, u := range m.Payload.Mention.UIDs {
		if u == botUID {
			return true
		}
	}
	return truthy(m.Payload.Mention.AIs)
}

// PersonaMention projects this message's @-mention payload onto the IM-agnostic
// persona.Mention vocabulary the persona package operates on. The three-state
// bool|number wire flags are normalized to bool here.
func (m BotMessage) PersonaMention() persona.Mention {
	if m.Payload.Mention == nil {
		return persona.Mention{}
	}
	return persona.Mention{
		UIDs:   m.Payload.Mention.UIDs,
		AIs:    truthy(m.Payload.Mention.AIs),
		Humans: truthy(m.Payload.Mention.Humans),
		All:    truthy(m.Payload.Mention.All),
	}
}

// ExplicitlyMentionsBot reports whether the bot's own uid is in the explicit
// @-mention uid list (NOT @AI / @所有人). Mirrors openclaw inbound.ts
// `isExplicitBotMention`; a direct @bot mention makes a persona clone reply as
// itself rather than as the grantor.
func (m BotMessage) ExplicitlyMentionsBot(botUID string) bool {
	if m.Payload.Mention == nil {
		return false
	}
	for _, u := range m.Payload.Mention.UIDs {
		if u == botUID {
			return true
		}
	}
	return false
}

// Triggers reports whether this message should trigger a turn for the bot,
// accounting for persona-clone semantics. Mirrors openclaw inbound.ts
// `isMentioned` (~L1701-1705):
//
// - explicit @bot uid → always;
// - pure @AI (ais=1, no broadcast flags) → yes; @AI suppressed under a
// broadcast (the server rewrites @所有人 to also set ais=1);
// - @所有人 (humans=1) → only persona clones (a human is part of the
// broadcast, the clone acts for them);
// - grantor @-mentioned → persona clones (targets the grantor identity).
//
// For a non-clone (zero grantor) this collapses to the plain mention gate.
func (m BotMessage) Triggers(botUID string, grantor persona.Grantor) bool {
	if m.Payload.Mention == nil {
		return false
	}
	pm := m.PersonaMention()
	isBroadcast := pm.All || pm.Humans
	if !isBroadcast && pm.AIs {
		return true
	}
	if m.ExplicitlyMentionsBot(botUID) {
		return true
	}
	if grantor.Configured() {
		if pm.Humans {
			return true
		}
		for _, u := range pm.UIDs {
			if u == grantor.UID {
				return true
			}
		}
	}
	return false
}

// truthy interprets Octo's three-state bool|number fields.
func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case float64:
		return t != 0
	case int:
		return t != 0
	default:
		return false
	}
}
