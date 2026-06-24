package router

import (
	"context"
	"testing"
)

// accept reports whether RouteAndHandle ran the handler with the given decision.
func mustRoute(t *testing.T, r *Router, m InboundMessage) Decision {
	t.Helper()
	d, err := r.RouteAndHandle(context.Background(), m,
		func(ctx context.Context, key string, m InboundMessage) error { return nil })
	if err != nil {
		t.Fatalf("route err: %v", err)
	}
	return d
}

// TestMentionFreeGroupAccepts: G12 — in a mention-free channel an unmentioned
// group message is Accepted (not DroppedNotMentioned), while a NON-mention-free
// channel still requires the @mention.
func TestMentionFreeGroupAccepts(t *testing.T) {
	r := New(Config{MaxPerMinute: 100, MentionFreeGroups: []string{"free"}})

	// mention-free channel, unmentioned human → accepted
	if d := mustRoute(t, r, InboundMessage{
		ChannelType: ChannelGroup, ChannelID: "free", FromUID: "alice", Text: "hi"}); d != Accepted {
		t.Fatalf("mention-free unmentioned should be accepted, got %s", d)
	}

	// normal channel, unmentioned → still dropped
	if d := mustRoute(t, r, InboundMessage{
		ChannelType: ChannelGroup, ChannelID: "normal", FromUID: "alice", Text: "hi"}); d != DroppedNotMentioned {
		t.Fatalf("normal unmentioned should be not_mentioned, got %s", d)
	}

	// mention-free channel, mentioned → accepted (mention always wins)
	if d := mustRoute(t, r, InboundMessage{
		ChannelType: ChannelGroup, ChannelID: "free", FromUID: "alice", Mentioned: true, Text: "hi"}); d != Accepted {
		t.Fatalf("mention-free mentioned should be accepted, got %s", d)
	}
}

// TestMentionFreeBotLoopGuard: G14 — in a mention-free channel, messages from
// bot-looking uids (knownBotUids OR `_bot` suffix) are dropped to prevent
// bot↔bot loops, UNLESS whitelisted in allowedBotUids. A human is accepted; an
// @-mention from a bot still goes through.
func TestMentionFreeBotLoopGuard(t *testing.T) {
	r := New(Config{
		MaxPerMinute:      100,
		MentionFreeGroups: []string{"free"},
		KnownBotUids:      []string{"peer_known"},
		AllowedBotUids:    []string{"trusted_bot"},
	})
	mf := func(uid string, mentioned bool) InboundMessage {
		return InboundMessage{ChannelType: ChannelGroup, ChannelID: "free", FromUID: uid, Mentioned: mentioned, Text: "hi"}
	}

	if d := mustRoute(t, r, mf("human", false)); d != Accepted {
		t.Fatalf("human in mention-free should be accepted, got %s", d)
	}
	if d := mustRoute(t, r, mf("some_bot", false)); d != DroppedBot {
		t.Fatalf("_bot suffix should be loop-guard dropped, got %s", d)
	}
	if d := mustRoute(t, r, mf("peer_known", false)); d != DroppedBot {
		t.Fatalf("knownBotUids should be loop-guard dropped, got %s", d)
	}
	if d := mustRoute(t, r, mf("trusted_bot", false)); d != Accepted {
		t.Fatalf("allowedBotUids should bypass the loop guard, got %s", d)
	}
	// An explicit @mention from a bot is honored (mention gate passes before the
	// mention-free loop guard runs).
	if d := mustRoute(t, r, mf("some_bot", true)); d != Accepted {
		t.Fatalf("@-mentioned bot should be accepted, got %s", d)
	}
}

// TestDMBotLoopGuard: G14 — DMs from bot-looking uids are silently dropped
// unless whitelisted; humans are accepted.
func TestDMBotLoopGuard(t *testing.T) {
	r := New(Config{
		MaxPerMinute:   100,
		KnownBotUids:   []string{"peer_known"},
		AllowedBotUids: []string{"trusted_bot"},
	})
	dm := func(uid string) InboundMessage {
		return InboundMessage{ChannelType: ChannelDM, FromUID: uid, Text: "hi"}
	}
	if d := mustRoute(t, r, dm("human")); d != Accepted {
		t.Fatalf("human DM should be accepted, got %s", d)
	}
	if d := mustRoute(t, r, dm("evil_bot")); d != DroppedBot {
		t.Fatalf("_bot DM should be dropped, got %s", d)
	}
	if d := mustRoute(t, r, dm("peer_known")); d != DroppedBot {
		t.Fatalf("knownBotUids DM should be dropped, got %s", d)
	}
	if d := mustRoute(t, r, dm("trusted_bot")); d != Accepted {
		t.Fatalf("allowedBotUids DM should bypass, got %s", d)
	}
}

// TestSelfUIDTreatedAsBot: SelfUID is seeded into knownBotUids, so a relayed
// self-message in a mention-free group is dropped by the loop guard.
func TestSelfUIDTreatedAsBot(t *testing.T) {
	r := New(Config{MaxPerMinute: 100, MentionFreeGroups: []string{"free"}, SelfUID: "me"})
	if d := mustRoute(t, r, InboundMessage{
		ChannelType: ChannelGroup, ChannelID: "free", FromUID: "me", Text: "echo"}); d != DroppedBot {
		t.Fatalf("self uid should be loop-guard dropped, got %s", d)
	}
}

// TestBotBlocklistDM: a blocklisted uid's DM is silently dropped (DM only).
func TestBotBlocklistDM(t *testing.T) {
	r := New(Config{MaxPerMinute: 100, BotBlocklist: []string{"blocked"}})
	if d := mustRoute(t, r, InboundMessage{ChannelType: ChannelDM, FromUID: "blocked", Text: "hi"}); d != DroppedBot {
		t.Fatalf("blocklisted DM should be dropped, got %s", d)
	}
	if d := mustRoute(t, r, InboundMessage{ChannelType: ChannelDM, FromUID: "ok", Text: "hi"}); d != Accepted {
		t.Fatalf("non-blocklisted DM should be accepted, got %s", d)
	}
}

// TestBotBlocklistGroupHardDrop: a blocklisted sender is dropped in a group even
// when @-mentioned (the blocklist beats the mention gate).
func TestBotBlocklistGroupHardDrop(t *testing.T) {
	r := New(Config{MaxPerMinute: 100, BotBlocklist: []string{"blocked"}, MentionFreeGroups: []string{"free"}})
	if d := mustRoute(t, r, InboundMessage{
		ChannelType: ChannelGroup, ChannelID: "g1", FromUID: "blocked", Mentioned: true, Text: "hi"}); d != DroppedBot {
		t.Fatalf("blocklisted mentioned group msg should be dropped, got %s", d)
	}
	// In a mention-free channel too.
	if d := mustRoute(t, r, InboundMessage{
		ChannelType: ChannelGroup, ChannelID: "free", FromUID: "blocked", Text: "hi"}); d != DroppedBot {
		t.Fatalf("blocklisted mention-free group msg should be dropped, got %s", d)
	}
}

// TestNormalGroupStillRequiresMention: regression — without any mention-free
// config, normal groups keep the @mention requirement (unchanged behavior).
func TestNormalGroupStillRequiresMention(t *testing.T) {
	r := New(Config{MaxPerMinute: 100})
	if d := mustRoute(t, r, InboundMessage{
		ChannelType: ChannelGroup, ChannelID: "g1", FromUID: "u1", Text: "hi"}); d != DroppedNotMentioned {
		t.Fatalf("normal group unmentioned should be not_mentioned, got %s", d)
	}
	if d := mustRoute(t, r, InboundMessage{
		ChannelType: ChannelGroup, ChannelID: "g1", FromUID: "u1", Mentioned: true, Text: "hi"}); d != Accepted {
		t.Fatalf("normal group mentioned should be accepted, got %s", d)
	}
}

// TestCronFireBypassesBotGuards: a cron fire bypasses the blocklist + loop guard
// (operator-scheduled, authenticity is the caller's concern).
func TestCronFireBypassesBotGuards(t *testing.T) {
	r := New(Config{MaxPerMinute: 100, BotBlocklist: []string{"sys_bot"}})
	if d := mustRoute(t, r, InboundMessage{
		ChannelType: ChannelGroup, ChannelID: "g1", FromUID: "sys_bot", CronFire: true, Text: "tick"}); d != Accepted {
		t.Fatalf("cron fire should bypass bot guards + mention gate, got %s", d)
	}
}
