package trigger

import (
	"testing"
)

// TestDefaultClassifierBugFix asserts the user-visible bug surface from
// issue #105: a WuKongIM client setting AIs=truthy on follow-up messages
// must NOT trigger the bot under the new default AIBroadcast=Deny, but
// MUST still be observed into groupctx as background.
func TestDefaultClassifierBugFix(t *testing.T) {
	in := groupMsg("g1", "u_alice", &MentionPayload{AIsFlag: true})

	deny := DefaultClassifier{}.Classify(in, Policy{
		BotUID:      "bot1",
		AIBroadcast: AIBroadcastDeny,
	})
	if deny.ShouldReply() {
		t.Fatalf("deny + pure @AI must NOT reply: got reason=%s", deny.Reason)
	}
	if !deny.ShouldObserve() {
		t.Fatalf("deny + pure @AI must observe (so context flows): got reason=%s", deny.Reason)
	}

	allow := DefaultClassifier{}.Classify(in, Policy{
		BotUID:      "bot1",
		AIBroadcast: AIBroadcastAllow,
	})
	if !allow.ShouldReply() || allow.Reason != ReasonAIBroadcast {
		t.Fatalf("allow + pure @AI must preserve legacy trigger: reason=%s", allow.Reason)
	}
}

// TestDefaultClassifierA2RealAtBotInBetween asserts issue #105 A2: a
// real @bot mixed into the burst is the only one that triggers.
func TestDefaultClassifierA2RealAtBotInBetween(t *testing.T) {
	p := Policy{BotUID: "bot1", AIBroadcast: AIBroadcastDeny}
	for i := range 3 {
		in := groupMsg("g1", "u_alice", &MentionPayload{AIsFlag: true})
		if (DefaultClassifier{}).Classify(in, p).ShouldReply() {
			t.Fatalf("noise[%d] must not reply", i)
		}
	}
	real := groupMsg("g1", "u_alice", &MentionPayload{UIDs: []string{"bot1"}, AIsFlag: true})
	d := DefaultClassifier{}.Classify(real, p)
	if d.Reason != ReasonExplicitBot {
		t.Fatalf("real @bot must trigger as explicit_bot, got %s", d.Reason)
	}
}

// TestDefaultClassifierReplyToBotPreservesPersona is the regression for
// the code-review finding: a persona clone replying in a quote-thread
// must keep speaking as the grantor. Without on_behalf_of the persona
// identity breaks the moment a user quote-replies one of the bot's
// prior persona-voiced messages (no @-mention, no OBO signal — the
// bare reply-to-bot path).
func TestDefaultClassifierReplyToBotPreservesPersona(t *testing.T) {
	in := groupMsg("g1", "u_alice", nil)
	in.ReplyTo = &ReplyContext{TargetMessageID: "m_prev", TargetFromUID: "bot1", TargetIsBot: true}

	d := DefaultClassifier{}.Classify(in, Policy{
		BotUID:            "bot1",
		Grantor:           PolicyGrantor{UID: "u_admin", Name: "Admin"},
		AIBroadcast:       AIBroadcastDeny,
		ReplyToBotEnabled: true,
	})
	if d.Reason != ReasonReplyToBot {
		t.Fatalf("reply-to-bot must fire reply_to_bot: %s", d.Reason)
	}
	if d.ReplyRouting.OnBehalfOf != "u_admin" {
		t.Fatalf("persona clone quote-reply must stamp on_behalf_of=grantor: routing=%+v", d.ReplyRouting)
	}
}
func TestDefaultClassifierA3ReplyToBot(t *testing.T) {
	p := Policy{BotUID: "bot1", AIBroadcast: AIBroadcastDeny, ReplyToBotEnabled: true}

	toBot := groupMsg("g1", "u_alice", nil)
	toBot.ReplyTo = &ReplyContext{TargetMessageID: "m1", TargetFromUID: "bot1", TargetIsBot: true}
	if d := (DefaultClassifier{}.Classify(toBot, p)); d.Reason != ReasonReplyToBot {
		t.Fatalf("reply-to-bot must fire reply_to_bot, got %s", d.Reason)
	}

	toOther := groupMsg("g1", "u_alice", nil)
	toOther.ReplyTo = &ReplyContext{TargetMessageID: "m1", TargetFromUID: "u_bob", TargetIsBot: false}
	if d := (DefaultClassifier{}.Classify(toOther, p)); d.ShouldReply() {
		t.Fatalf("reply-to-other must not fire: reason=%s", d.Reason)
	}
}

// TestDefaultClassifierReplyToBotDisabled: even with ReplyToBotEnabled=false
// a quote-reply to the bot is observation-only.
func TestDefaultClassifierReplyToBotDisabled(t *testing.T) {
	p := Policy{BotUID: "bot1", AIBroadcast: AIBroadcastDeny}
	in := groupMsg("g1", "u_alice", nil)
	in.ReplyTo = &ReplyContext{TargetIsBot: true}
	if d := (DefaultClassifier{}.Classify(in, p)); d.ShouldReply() {
		t.Fatalf("reply-to-bot OFF must not fire: reason=%s", d.Reason)
	}
}

// TestDefaultClassifierA5PersonaGrantor asserts issue #105 A5: persona
// configured + AIs=1 + grantor in UIDs → persona_grantor (beats @AI deny).
func TestDefaultClassifierA5PersonaGrantor(t *testing.T) {
	in := groupMsg("g1", "u_alice", &MentionPayload{UIDs: []string{"u_admin"}, AIsFlag: true})
	d := DefaultClassifier{}.Classify(in, Policy{
		BotUID:      "bot1",
		Grantor:     PolicyGrantor{UID: "u_admin", Name: "Admin"},
		AIBroadcast: AIBroadcastDeny,
	})
	if d.Reason != ReasonPersonaGrantor {
		t.Fatalf("persona grantor must win: reason=%s", d.Reason)
	}
	if d.ReplyRouting.OnBehalfOf != "u_admin" {
		t.Fatalf("persona grantor must stamp on_behalf_of: routing=%+v", d.ReplyRouting)
	}
}

// TestDefaultClassifierA6MentionFreeGroup asserts issue #105 A6: AI=1 in a
// mention-free group falls through to mention_free_group (AI broadcast deny
// is evaluated first; since the channel is mention-free we still trigger).
func TestDefaultClassifierA6MentionFreeGroup(t *testing.T) {
	in := groupMsg("g1", "u_alice", &MentionPayload{AIsFlag: true})
	d := DefaultClassifier{}.Classify(in, Policy{
		BotUID:            "bot1",
		AIBroadcast:       AIBroadcastDeny,
		MentionFreeGroups: map[string]bool{"g1": true},
	})
	if d.Reason != ReasonMentionFreeGroup {
		t.Fatalf("mention-free group must fire: reason=%s", d.Reason)
	}
}

// TestDefaultClassifierA7CronBypassesEverything: a cron source on a group
// with no mention triggers as cron and stamps no special routing.
func TestDefaultClassifierA7CronBypassesEverything(t *testing.T) {
	in := groupMsg("g1", "system", nil)
	in.Source = SourceCron
	d := DefaultClassifier{}.Classify(in, Policy{BotUID: "bot1", AIBroadcast: AIBroadcastDeny})
	if d.Reason != ReasonCron || !d.ShouldReply() {
		t.Fatalf("cron must reply with reason=cron, got %s", d.Reason)
	}
	if d.Source != SourceCron {
		t.Fatalf("cron decision must carry SourceCron, got %s", d.Source)
	}
}

// TestDefaultClassifierCronPersonaStampsGrantor: cron from a persona clone
// preserves on_behalf_of=grantor (parity with the legacy EnqueueCron path).
func TestDefaultClassifierCronPersonaStampsGrantor(t *testing.T) {
	in := groupMsg("g1", "system", nil)
	in.Source = SourceCron
	d := DefaultClassifier{}.Classify(in, Policy{
		BotUID:  "bot1",
		Grantor: PolicyGrantor{UID: "u_admin", Name: "Admin"},
	})
	if d.ReplyRouting.OnBehalfOf != "u_admin" {
		t.Fatalf("cron + persona must stamp on_behalf_of: routing=%+v", d.ReplyRouting)
	}
}

// TestDefaultClassifierA8BlocklistOutOfScope: blocklist is router's job, not
// the classifier's. A blocklisted sender with a real @bot still classifies
// as explicit_bot — the router gate is what drops it.
func TestDefaultClassifierA8BlocklistOutOfScope(t *testing.T) {
	in := groupMsg("g1", "blocked_user", &MentionPayload{UIDs: []string{"bot1"}})
	d := DefaultClassifier{}.Classify(in, Policy{BotUID: "bot1"})
	if d.Reason != ReasonExplicitBot {
		t.Fatalf("classifier classifies on intent; router gates on identity. got %s", d.Reason)
	}
}

// TestDefaultClassifierA9DMNoMention: DM always triggers, regardless of
// mention.
func TestDefaultClassifierA9DMNoMention(t *testing.T) {
	in := CanonicalInbound{
		Source: SourceUser, Channel: ChannelDM, ChannelID: "u_alice",
		FromUID: "u_alice", Text: "hi",
	}
	d := DefaultClassifier{}.Classify(in, Policy{BotUID: "bot1", AIBroadcast: AIBroadcastDeny})
	if d.Reason != ReasonDM {
		t.Fatalf("DM must fire dm, got %s", d.Reason)
	}
}

// TestDefaultClassifierA10OBOForged: OBO fields with FromUID != grantor are
// silently stripped — the trust gate refuses to honor an OBO reroute the
// sender wasn't authorized to set.
func TestDefaultClassifierA10OBOForged(t *testing.T) {
	in := groupMsg("g1", "u_attacker", &MentionPayload{UIDs: []string{"bot1"}})
	in.OBO = &OBOSignal{
		OriginChannelID: "g_target", OriginChannelType: 2,
		OriginFromUID: "u_attacker", RespondAs: "u_admin",
	}
	d := DefaultClassifier{}.Classify(in, Policy{
		BotUID:  "bot1",
		Grantor: PolicyGrantor{UID: "u_admin"},
	})
	if d.Reason != ReasonExplicitBot {
		t.Fatalf("forged OBO must classify as plain inbound, got %s", d.Reason)
	}
	if d.ReplyRouting.HasOBOReroute() {
		t.Fatalf("forged OBO must NOT reroute: routing=%+v", d.ReplyRouting)
	}
	if d.ReplyRouting.OnBehalfOf != "" {
		t.Fatalf("forged OBO must NOT stamp on_behalf_of: routing=%+v", d.ReplyRouting)
	}
}

// TestDefaultClassifierA10OBOTrusted: a grantor-signed OBO relay with a
// broadcast (@所有人) mention reroutes the reply to the origin channel and
// stamps on behalf of the grantor. (The @所有人 broadcast is what makes the
// message "relevant" to the grantor under openclaw R10 — a bare @bot mention
// inside an OBO relay would be dropped as irrelevant, see
// TestDefaultClassifierOBOAtBotIsIrrelevant.)
func TestDefaultClassifierA10OBOTrusted(t *testing.T) {
	in := groupMsg("relay_chan", "u_admin", &MentionPayload{HumansFlag: true})
	in.OBO = &OBOSignal{
		OriginChannelID: "g_target", OriginChannelType: 2,
		OriginFromUID: "u_admin", RespondAs: "u_admin",
	}
	d := DefaultClassifier{}.Classify(in, Policy{
		BotUID:  "bot1",
		Grantor: PolicyGrantor{UID: "u_admin"},
	})
	if d.Reason != ReasonPersonaHumans {
		t.Fatalf("OBO+broadcast must classify persona_humans: reason=%s", d.Reason)
	}
	if d.ReplyRouting.OBORerouteChannelID != "g_target" {
		t.Fatalf("OBO reroute target wrong: %+v", d.ReplyRouting)
	}
	if d.ReplyRouting.OnBehalfOf != "u_admin" {
		t.Fatalf("OBO trusted must stamp on_behalf_of=grantor: %+v", d.ReplyRouting)
	}
}

// TestDefaultClassifierOBOAtBotIsIrrelevant preserves the openclaw R10
// behavior: a trusted OBO relay whose mention is a bare @bot uid (no
// grantor, no broadcast) is dropped as obo_irrelevant. The persona clone is
// the GRANTOR's proxy — being addressed by some random user in the relay
// channel isn't a call to the grantor.
func TestDefaultClassifierOBOAtBotIsIrrelevant(t *testing.T) {
	in := groupMsg("relay_chan", "u_admin", &MentionPayload{UIDs: []string{"bot1"}})
	in.OBO = &OBOSignal{
		OriginChannelID: "g_target", OriginChannelType: 2,
		OriginFromUID: "u_admin", RespondAs: "u_admin",
	}
	d := DefaultClassifier{}.Classify(in, Policy{
		BotUID:  "bot1",
		Grantor: PolicyGrantor{UID: "u_admin"},
	})
	if d.Reason != ReasonOBOIrrelevant {
		t.Fatalf("OBO+@bot without broadcast/grantor mention must drop, got %s", d.Reason)
	}
}

// TestDefaultClassifierOBOTrustedDMReroute: an OBO v2 trusted relay landing
// in the bot's DM with the grantor reroutes the reply to the origin DM
// peer's uid (which lives in OBO.OriginFromUID, not OriginChannelID — see
// oboReplyTarget's DM branch). DM auto-triggers; OBO layers on the routing.
func TestDefaultClassifierOBOTrustedDMReroute(t *testing.T) {
	in := CanonicalInbound{
		Source: SourceUser, Channel: ChannelDM, ChannelID: "u_admin",
		FromUID: "u_admin", Text: "relay",
		OBO: &OBOSignal{
			OriginChannelID: "ignored_for_dm", OriginChannelType: 1, // 1 = DM
			OriginFromUID: "u_orig_sender", RespondAs: "u_admin",
		},
	}
	d := DefaultClassifier{}.Classify(in, Policy{
		BotUID:  "bot1",
		Grantor: PolicyGrantor{UID: "u_admin"},
	})
	if d.Reason != ReasonDM {
		t.Fatalf("DM with OBO must still fire dm, got %s", d.Reason)
	}
	if d.ReplyRouting.OBORerouteChannelID != "u_orig_sender" {
		t.Fatalf("DM-origin OBO must reroute to OriginFromUID, got %+v", d.ReplyRouting)
	}
	if d.ReplyRouting.OnBehalfOf != "u_admin" {
		t.Fatalf("DM OBO trusted must stamp on_behalf_of: %+v", d.ReplyRouting)
	}
}

// TestDefaultClassifierA11OBOIrrelevant: a grantor-signed OBO relay whose
// mention payload is pure @AI fan-out (not addressing the grantor) is
// dropped with reason=obo_irrelevant BEFORE any session-state side effect
// — the openclaw R10 leak guard.
func TestDefaultClassifierA11OBOIrrelevant(t *testing.T) {
	in := groupMsg("relay_chan", "u_admin", &MentionPayload{AIsFlag: true})
	in.OBO = &OBOSignal{
		OriginChannelID: "g_target", OriginChannelType: 2,
		OriginFromUID: "u_admin", RespondAs: "u_admin",
	}
	d := DefaultClassifier{}.Classify(in, Policy{
		BotUID:  "bot1",
		Grantor: PolicyGrantor{UID: "u_admin"},
	})
	if d.Reason != ReasonOBOIrrelevant {
		t.Fatalf("OBO + pure @AI must drop as obo_irrelevant, got %s", d.Reason)
	}
	if d.ShouldReply() || d.ShouldObserve() {
		t.Fatalf("OBO irrelevant must NOT reply OR observe (R10 leak guard)")
	}
}

// TestDefaultClassifierExplicitBeatsPersona: a message that BOTH @-mentions
// the bot uid AND @-mentions the grantor classifies as ExplicitBot — direct
// mention is unambiguous intent.
func TestDefaultClassifierExplicitBeatsPersona(t *testing.T) {
	in := groupMsg("g1", "u_alice", &MentionPayload{UIDs: []string{"bot1", "u_admin"}})
	d := DefaultClassifier{}.Classify(in, Policy{
		BotUID:  "bot1",
		Grantor: PolicyGrantor{UID: "u_admin"},
	})
	if d.Reason != ReasonExplicitBot {
		t.Fatalf("explicit must win over persona: reason=%s", d.Reason)
	}
}

// TestDefaultClassifierPersonaHumans: @所有人 with a persona configured
// triggers persona_humans (the grantor is part of the broadcast).
func TestDefaultClassifierPersonaHumans(t *testing.T) {
	in := groupMsg("g1", "u_alice", &MentionPayload{HumansFlag: true})
	d := DefaultClassifier{}.Classify(in, Policy{
		BotUID:  "bot1",
		Grantor: PolicyGrantor{UID: "u_admin"},
	})
	if d.Reason != ReasonPersonaHumans {
		t.Fatalf("persona + @所有人 must fire persona_humans: reason=%s", d.Reason)
	}
	if d.ReplyRouting.OnBehalfOf != "u_admin" {
		t.Fatalf("persona_humans must stamp on_behalf_of: routing=%+v", d.ReplyRouting)
	}
}

// TestDefaultClassifierAllowlist: allowlist policy only triggers @AI on
// listed channels.
func TestDefaultClassifierAllowlist(t *testing.T) {
	in := groupMsg("g1", "u_alice", &MentionPayload{AIsFlag: true})

	miss := DefaultClassifier{}.Classify(in, Policy{
		BotUID:               "bot1",
		AIBroadcast:          AIBroadcastAllowlist,
		AIBroadcastAllowlist: map[string]bool{"g_other": true},
	})
	if miss.ShouldReply() {
		t.Fatalf("allowlist miss must not reply: reason=%s", miss.Reason)
	}

	hit := DefaultClassifier{}.Classify(in, Policy{
		BotUID:               "bot1",
		AIBroadcast:          AIBroadcastAllowlist,
		AIBroadcastAllowlist: map[string]bool{"g1": true},
	})
	if hit.Reason != ReasonAIBroadcast {
		t.Fatalf("allowlist hit must fire ai_broadcast: reason=%s", hit.Reason)
	}
}

// TestDefaultClassifierEmptyMentionPayload: a Mention struct that's
// entirely empty (no UIDs, no flags) falls through to observation.
func TestDefaultClassifierEmptyMentionPayload(t *testing.T) {
	in := groupMsg("g1", "u_alice", &MentionPayload{})
	d := DefaultClassifier{}.Classify(in, Policy{BotUID: "bot1", AIBroadcast: AIBroadcastDeny})
	if d.Reason != ReasonObservation {
		t.Fatalf("empty mention payload must observe: reason=%s", d.Reason)
	}
}

// TestDefaultClassifierNilMentionGroup: no mention struct at all (plain
// group chatter) falls through to observation.
func TestDefaultClassifierNilMentionGroup(t *testing.T) {
	in := groupMsg("g1", "u_alice", nil)
	d := DefaultClassifier{}.Classify(in, Policy{BotUID: "bot1", AIBroadcast: AIBroadcastDeny})
	if d.Reason != ReasonObservation {
		t.Fatalf("nil mention payload must observe: reason=%s", d.Reason)
	}
	if !d.ShouldObserve() {
		t.Fatalf("observation reason must ShouldObserve")
	}
}

// TestDefaultClassifierUnsetPolicyDefaultsToDeny: an AIBroadcast policy left
// unset (zero value) behaves as Deny — the safe default that fixes the bug
// for operators that don't migrate config.
func TestDefaultClassifierUnsetPolicyDefaultsToDeny(t *testing.T) {
	in := groupMsg("g1", "u_alice", &MentionPayload{AIsFlag: true})
	d := DefaultClassifier{}.Classify(in, Policy{BotUID: "bot1"}) // no AIBroadcast field
	if d.ShouldReply() {
		t.Fatalf("unset AIBroadcast must default to deny: reason=%s", d.Reason)
	}
}

// TestPolicyGrantorFromPersona round-trips through the helper without
// dropping fields.
func TestPolicyGrantorFromPersona(t *testing.T) {
	got := FromPersonaGrantor(personaGrantorFixture())
	if got.UID != "u_admin" || got.Name != "Admin" {
		t.Fatalf("FromPersonaGrantor lost fields: %+v", got)
	}
	if !got.Configured() {
		t.Fatalf("configured grantor lost truthiness")
	}
}

// --- helpers ---

// TestSourceConsoleDMTriggers pins that a Console turn (a DM carrying
// SourceConsole) triggers like any DM and preserves its Source through the
// decision (so downstream owner-trust checks can read it).
func TestSourceConsoleDMTriggers(t *testing.T) {
	in := CanonicalInbound{
		Source:  SourceConsole,
		Channel: ChannelDM,
		FromUID: "gui-user",
		Text:    "hi",
	}
	d := DefaultClassifier{}.Classify(in, Policy{BotUID: "bot1"})
	if !d.ShouldReply() || d.Reason != ReasonDM {
		t.Fatalf("Console DM must trigger as a DM: reply=%v reason=%s", d.ShouldReply(), d.Reason)
	}
	if d.Source != SourceConsole {
		t.Fatalf("Console source must survive classification: got %s", d.Source)
	}
}

func groupMsg(chID, from string, m *MentionPayload) CanonicalInbound {
	return CanonicalInbound{
		Source:    SourceUser,
		Channel:   ChannelGroup,
		ChannelID: chID,
		FromUID:   from,
		Text:      "hello",
		Mention:   m,
	}
}
