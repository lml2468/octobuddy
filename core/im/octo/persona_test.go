package octo

import (
	"testing"

	"github.com/lml2468/xclaw/core/persona"
)

// TestTriggersPersonaClone exercises the persona-aware group trigger gate
// (openclaw inbound.ts isMentioned), distinguishing a clone from a plain bot.
func TestTriggersPersonaClone(t *testing.T) {
	clone := persona.Grantor{UID: "u_admin", Name: "Admin"}
	none := persona.Grantor{}
	bot := "bot1"

	cases := []struct {
		name    string
		mention *Mention
		grantor persona.Grantor
		want    bool
	}{
		{"explicit @bot → trigger", &Mention{UIDs: []string{bot}}, clone, true},
		{"pure @AI → trigger", &Mention{AIs: float64(1)}, clone, true},
		{"@所有人 + clone → trigger", &Mention{Humans: float64(1)}, clone, true},
		{"@所有人 + non-clone → no trigger", &Mention{Humans: float64(1)}, none, false},
		{"grantor @ + clone → trigger", &Mention{UIDs: []string{"u_admin"}}, clone, true},
		{"grantor @ + non-clone → no trigger", &Mention{UIDs: []string{"u_admin"}}, none, false},
		{"@AI suppressed under broadcast → no trigger", &Mention{All: float64(1), AIs: float64(1)}, none, false},
		{"@AI suppressed under broadcast, clone but not grantor-relevant → no trigger", &Mention{All: float64(1), AIs: float64(1)}, persona.Grantor{UID: "u_other"}, false},
		{"no mention → no trigger", nil, clone, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := BotMessage{Payload: MessagePayload{Mention: tc.mention}}
			if got := m.Triggers(bot, tc.grantor); got != tc.want {
				t.Fatalf("Triggers = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestExplicitlyMentionsBot(t *testing.T) {
	m := BotMessage{Payload: MessagePayload{Mention: &Mention{UIDs: []string{"bot1"}, AIs: float64(1)}}}
	if !m.ExplicitlyMentionsBot("bot1") {
		t.Fatal("explicit uid should count")
	}
	// @AI alone is NOT an explicit bot mention.
	mAI := BotMessage{Payload: MessagePayload{Mention: &Mention{AIs: float64(1)}}}
	if mAI.ExplicitlyMentionsBot("bot1") {
		t.Fatal("@AI is not an explicit bot mention")
	}
}

func TestPersonaMentionProjection(t *testing.T) {
	m := BotMessage{Payload: MessagePayload{Mention: &Mention{
		UIDs:   []string{"u_admin"},
		AIs:    float64(1),
		Humans: true,
		All:    float64(0),
	}}}
	pm := m.PersonaMention()
	if !pm.AIs || !pm.Humans || pm.All {
		t.Fatalf("projection wrong: %+v", pm)
	}
	if len(pm.UIDs) != 1 || pm.UIDs[0] != "u_admin" {
		t.Fatalf("uids not carried: %+v", pm.UIDs)
	}
}

// TestOBOReplyTarget covers the OBO v2 reply-destination derivation (openclaw
// inbound.ts ~L2305-2326): group origin → origin channel; DM origin → original
// sender uid; both carry on_behalf_of = the trusted grantor uid.
func TestOBOReplyTarget(t *testing.T) {
	group := ChannelGroup
	groupT := int(group)
	dm := ChannelDM
	dmT := int(dm)

	t.Run("group origin", func(t *testing.T) {
		p := MessagePayload{OBOOriginChannelID: "grp1", OBOOriginChannelType: &groupT}
		tgt := oboReplyTarget(p, "u_admin")
		if tgt.channelID != "grp1" || tgt.channelType != ChannelGroup || tgt.onBehalfOf != "u_admin" {
			t.Fatalf("group target wrong: %+v", tgt)
		}
	})

	t.Run("DM origin replies to original sender", func(t *testing.T) {
		p := MessagePayload{OBOOriginChannelID: "grp1", OBOOriginChannelType: &dmT, OBOOriginFromUID: "bob"}
		tgt := oboReplyTarget(p, "u_admin")
		if tgt.channelID != "bob" || tgt.channelType != ChannelDM || tgt.onBehalfOf != "u_admin" {
			t.Fatalf("DM target wrong: %+v", tgt)
		}
	})

	t.Run("missing channel type defaults to group", func(t *testing.T) {
		p := MessagePayload{OBOOriginChannelID: "grp1"}
		tgt := oboReplyTarget(p, "u_admin")
		if tgt.channelType != ChannelGroup {
			t.Fatalf("expected default group, got %v", tgt.channelType)
		}
	})
}

func TestOBORespondAsPrefersRespondAs(t *testing.T) {
	if got := oboRespondAs(MessagePayload{OBORespondAs: "a", OBOGrantorUID: "b"}); got != "a" {
		t.Fatalf("expected obo_respond_as preference, got %q", got)
	}
	if got := oboRespondAs(MessagePayload{OBOGrantorUID: "b"}); got != "b" {
		t.Fatalf("expected fallback to grantor_uid, got %q", got)
	}
	if got := oboRespondAs(MessagePayload{}); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

// TestOnInboundOBOSecurityGate proves the OBO v2 fields are only honored when
// the message is sent by the configured grantor — a forged OBO payload from
// another uid must fall through to a normal (non-OBO) reply target.
func TestOnInboundOBOSecurityGate(t *testing.T) {
	groupT := int(ChannelGroup)
	c := NewConnector(NewRESTClient("http://x", func() string { return "tk" }))
	c.SetPersona(persona.Grantor{UID: "u_admin", Name: "Admin"})
	c.setUID("bot1")

	// Forged OBO payload from a non-grantor, with an explicit @bot mention so it
	// still triggers a turn. The OBO origin hint must be ignored.
	m := BotMessage{
		FromUID:     "u_attacker",
		ChannelID:   "dmchan",
		ChannelType: ChannelDM,
		Payload: MessagePayload{
			Type:                 MsgText,
			Content:              "hi",
			Mention:              &Mention{UIDs: []string{"bot1"}},
			OBOOriginChannelID:   "victim-group",
			OBOOriginChannelType: &groupT,
			OBORespondAs:         "u_admin",
		},
	}
	// gateway is nil → onInbound records the target then returns before Handle.
	c.onInbound(m)

	key := "u_attacker" // DM session key
	tgt, ok := c.peekQueuedTarget(key)
	if !ok {
		t.Fatal("expected a reply target to be recorded")
	}
	if tgt.channelID != "dmchan" || tgt.onBehalfOf != "" {
		t.Fatalf("forged OBO must not reroute or set on_behalf_of: %+v", tgt)
	}
}

// TestOnInboundOBOTrustedRelay proves a grantor-signed OBO v2 relay reroutes
// the reply to the origin group with on_behalf_of=grantor.
func TestOnInboundOBOTrustedRelay(t *testing.T) {
	groupT := int(ChannelGroup)
	c := NewConnector(NewRESTClient("http://x", func() string { return "tk" }))
	c.SetPersona(persona.Grantor{UID: "u_admin", Name: "Admin"})
	c.setUID("bot1")

	m := BotMessage{
		FromUID:     "u_admin", // the configured grantor relays
		ChannelID:   "dmchan",
		ChannelType: ChannelDM,
		Payload: MessagePayload{
			Type:                 MsgText,
			Content:              "relay this",
			Mention:              &Mention{Humans: float64(1)}, // @所有人 in origin → relevant
			OBOOriginChannelID:   "origin-group",
			OBOOriginChannelType: &groupT,
			OBORespondAs:         "u_admin",
		},
	}
	c.onInbound(m)

	tgt, ok := c.peekQueuedTarget("u_admin")
	if !ok {
		t.Fatal("expected a reply target")
	}
	if tgt.channelID != "origin-group" || tgt.channelType != ChannelGroup || tgt.onBehalfOf != "u_admin" {
		t.Fatalf("OBO relay target wrong: %+v", tgt)
	}
}

// TestOnInboundOBORelevanceFilterDrops proves a grantor-signed OBO relay that
// is a pure @AI fan-out (not addressing the grantor) is dropped before any
// reply target is recorded (openclaw R10 leak guard).
func TestOnInboundOBORelevanceFilterDrops(t *testing.T) {
	groupT := int(ChannelGroup)
	c := NewConnector(NewRESTClient("http://x", func() string { return "tk" }))
	c.SetPersona(persona.Grantor{UID: "u_admin", Name: "Admin"})
	c.setUID("bot1")

	m := BotMessage{
		FromUID:     "u_admin",
		ChannelID:   "dmchan",
		ChannelType: ChannelDM,
		Payload: MessagePayload{
			Type:                 MsgText,
			Content:              "ai only",
			Mention:              &Mention{AIs: float64(1)}, // pure @AI, no grantor / broadcast
			OBOOriginChannelID:   "origin-group",
			OBOOriginChannelType: &groupT,
			OBORespondAs:         "u_admin",
		},
	}
	c.onInbound(m)

	if _, ok := c.peekQueuedTarget("u_admin"); ok {
		t.Fatal("irrelevant @AI OBO fan-out must NOT record a session/reply target")
	}
}
