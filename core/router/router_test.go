package router

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSessionKeyDM(t *testing.T) {
	k, err := InboundMessage{ChannelType: ChannelDM, FromUID: "u1"}.SessionKey()
	if err != nil || k != "u1" {
		t.Fatalf("dm key: %q %v", k, err)
	}
	k, _ = InboundMessage{ChannelType: ChannelDM, FromUID: "u1", SpaceID: "s9"}.SessionKey()
	if k != "s9:u1" {
		t.Fatalf("dm space key: %q", k)
	}
	if _, err := (InboundMessage{ChannelType: ChannelDM}).SessionKey(); err == nil {
		t.Fatal("dm without from_uid must be unroutable")
	}
}

func TestSessionKeyGroupSharedAcrossUsers(t *testing.T) {
	a, _ := InboundMessage{ChannelType: ChannelGroup, ChannelID: "c1", FromUID: "alice"}.SessionKey()
	b, _ := InboundMessage{ChannelType: ChannelGroup, ChannelID: "c1", FromUID: "bob"}.SessionKey()
	if a != "c1" || b != "c1" {
		t.Fatalf("group must be per-channel: alice=%q bob=%q", a, b)
	}
	if _, err := (InboundMessage{ChannelType: ChannelGroup}).SessionKey(); err == nil {
		t.Fatal("group without channel_id must be unroutable")
	}
}

func TestMentionGate(t *testing.T) {
	r := New(Config{})
	called := false
	h := func(ctx context.Context, key string, m InboundMessage) error { called = true; return nil }

	// group, not mentioned → dropped
	d, _ := r.RouteAndHandle(context.Background(),
		InboundMessage{ChannelType: ChannelGroup, ChannelID: "c1", FromUID: "u1"}, h)
	if d != DroppedNotMentioned || called {
		t.Fatalf("want not_mentioned drop, got %s called=%v", d, called)
	}

	// group, mentioned → accepted
	called = false
	d, _ = r.RouteAndHandle(context.Background(),
		InboundMessage{ChannelType: ChannelGroup, ChannelID: "c1", FromUID: "u1", Mentioned: true}, h)
	if d != Accepted || !called {
		t.Fatalf("want accepted, got %s called=%v", d, called)
	}

	// DM always accepted regardless of mention
	called = false
	d, _ = r.RouteAndHandle(context.Background(),
		InboundMessage{ChannelType: ChannelDM, FromUID: "u1"}, h)
	if d != Accepted || !called {
		t.Fatalf("DM want accepted, got %s called=%v", d, called)
	}
}

func TestUnroutableDropped(t *testing.T) {
	r := New(Config{})
	d, _ := r.RouteAndHandle(context.Background(),
		InboundMessage{ChannelType: ChannelDM}, // no from_uid
		func(ctx context.Context, key string, m InboundMessage) error { return nil })
	if d != DroppedUnroutable {
		t.Fatalf("want unroutable, got %s", d)
	}
}

func TestTooLong(t *testing.T) {
	r := New(Config{MaxContentByte: 10})
	d, _ := r.RouteAndHandle(context.Background(),
		InboundMessage{ChannelType: ChannelDM, FromUID: "u1", Text: "way too long content"},
		func(ctx context.Context, key string, m InboundMessage) error { return nil })
	if d != DroppedTooLong {
		t.Fatalf("want too_long, got %s", d)
	}
}

func TestPerSessionSerialization(t *testing.T) {
	r := New(Config{MaxPerMinute: 1000})
	var concurrent int32
	var maxConcurrent int32
	var wg sync.WaitGroup

	h := func(ctx context.Context, key string, m InboundMessage) error {
		c := atomic.AddInt32(&concurrent, 1)
		for {
			old := atomic.LoadInt32(&maxConcurrent)
			if c <= old || atomic.CompareAndSwapInt32(&maxConcurrent, old, c) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		atomic.AddInt32(&concurrent, -1)
		return nil
	}

	// 10 messages to the SAME session must run strictly serially.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.RouteAndHandle(context.Background(),
				InboundMessage{ChannelType: ChannelDM, FromUID: "same"}, h)
		}()
	}
	wg.Wait()
	if maxConcurrent != 1 {
		t.Fatalf("same-session handlers must serialize; max concurrency = %d", maxConcurrent)
	}
}

func TestDifferentSessionsRunConcurrently(t *testing.T) {
	r := New(Config{MaxPerMinute: 1000})
	var concurrent int32
	var maxConcurrent int32
	var wg sync.WaitGroup
	start := make(chan struct{})

	h := func(ctx context.Context, key string, m InboundMessage) error {
		<-start
		c := atomic.AddInt32(&concurrent, 1)
		for {
			old := atomic.LoadInt32(&maxConcurrent)
			if c <= old || atomic.CompareAndSwapInt32(&maxConcurrent, old, c) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		atomic.AddInt32(&concurrent, -1)
		return nil
	}

	for i := 0; i < 5; i++ {
		uid := string(rune('a' + i))
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.RouteAndHandle(context.Background(),
				InboundMessage{ChannelType: ChannelDM, FromUID: uid}, h)
		}()
	}
	close(start)
	wg.Wait()
	if maxConcurrent < 2 {
		t.Fatalf("distinct sessions should run concurrently; max concurrency = %d", maxConcurrent)
	}
}

func TestRateLimiting(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r := New(Config{MaxPerMinute: 3})
	r.SetClock(func() time.Time { return base })
	h := func(ctx context.Context, key string, m InboundMessage) error { return nil }
	msg := InboundMessage{ChannelType: ChannelDM, FromUID: "u1"}

	// 3 allowed, 4th limited (per-session bucket = 3)
	for i := 0; i < 3; i++ {
		if d, _ := r.RouteAndHandle(context.Background(), msg, h); d != Accepted {
			t.Fatalf("msg %d should be accepted, got %s", i, d)
		}
	}
	if d, _ := r.RouteAndHandle(context.Background(), msg, h); d != RateLimited {
		t.Fatalf("4th should be rate-limited, got %s", d)
	}

	// after a full window, refilled
	r.SetClock(func() time.Time { return base.Add(time.Minute) })
	if d, _ := r.RouteAndHandle(context.Background(), msg, h); d != Accepted {
		t.Fatalf("after refill should be accepted, got %s", d)
	}
}

func TestCronFireBypassesGateAndLimit(t *testing.T) {
	r := New(Config{MaxPerMinute: 1})
	r.SetClock(func() time.Time { return time.Unix(0, 0) })
	h := func(ctx context.Context, key string, m InboundMessage) error { return nil }
	// group, NOT mentioned, but cron fire → accepted; and repeated beyond limit.
	for i := 0; i < 5; i++ {
		d, _ := r.RouteAndHandle(context.Background(),
			InboundMessage{ChannelType: ChannelGroup, ChannelID: "c1", FromUID: "sys", CronFire: true}, h)
		if d != Accepted {
			t.Fatalf("cron fire %d should bypass gates, got %s", i, d)
		}
	}
}

func TestReapEvictsIdleAndRecreates(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	r := New(Config{MaxPerMinute: 5})
	r.SetClock(func() time.Time { return now })
	h := func(ctx context.Context, key string, m InboundMessage) error { return nil }

	seedIdleRoutes(t, r, h)
	assertNothingEvictedYet(t, r)

	now = base.Add(2 * time.Hour)
	assertIdleRoutesEvicted(t, r)
	assertReapedKeyRecreated(t, r, h)
}

func seedIdleRoutes(t *testing.T, r *Router, h Handler) {
	t.Helper()
	for _, uid := range []string{"u1", "u2"} {
		if d, _ := r.RouteAndHandle(context.Background(),
			InboundMessage{ChannelType: ChannelDM, FromUID: uid}, h); d != Accepted {
			t.Fatalf("%s should be accepted, got %s", uid, d)
		}
	}
	if len(r.locks) != 2 || len(r.perUser) != 2 || len(r.perSess) != 2 {
		t.Fatalf("want 2 of each, got locks=%d perUser=%d perSess=%d",
			len(r.locks), len(r.perUser), len(r.perSess))
	}
}

func assertNothingEvictedYet(t *testing.T, r *Router) {
	t.Helper()
	if locks, buckets := r.Reap(time.Hour); locks != 0 || buckets != 0 {
		t.Fatalf("nothing should be idle yet, evicted locks=%d buckets=%d", locks, buckets)
	}
}

func assertIdleRoutesEvicted(t *testing.T, r *Router) {
	t.Helper()
	locks, buckets := r.Reap(time.Hour)
	if locks != 2 || buckets != 4 {
		t.Fatalf("want locks=2 buckets=4, got locks=%d buckets=%d", locks, buckets)
	}
	if len(r.locks) != 0 || len(r.perUser) != 0 || len(r.perSess) != 0 {
		t.Fatalf("maps not emptied: locks=%d perUser=%d perSess=%d",
			len(r.locks), len(r.perUser), len(r.perSess))
	}
}

func assertReapedKeyRecreated(t *testing.T, r *Router, h Handler) {
	t.Helper()
	if d, _ := r.RouteAndHandle(context.Background(),
		InboundMessage{ChannelType: ChannelDM, FromUID: "u1"}, h); d != Accepted {
		t.Fatalf("post-reap route should be accepted, got %s", d)
	}
	if len(r.locks) != 1 {
		t.Fatalf("want 1 lock after recreate, got %d", len(r.locks))
	}
}

// TestReapSkipsInFlight proves the refcount guard: a lock held by an in-flight
// turn is never evicted, even while idle entries around it are reaped.
func TestReapSkipsInFlight(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	r := New(Config{MaxPerMinute: 5})
	r.SetClock(func() time.Time { return now })
	noop := func(ctx context.Context, key string, m InboundMessage) error { return nil }

	// Seed an idle session "old".
	r.RouteAndHandle(context.Background(), InboundMessage{ChannelType: ChannelDM, FromUID: "old"}, noop)

	// Now run a turn for "active" whose handler reaps while it holds the lock.
	now = base.Add(2 * time.Hour)
	var reapedLocks int
	active := func(ctx context.Context, key string, m InboundMessage) error {
		reapedLocks, _ = r.Reap(time.Hour) // "old" is idle (evict); "active" is in-flight (keep)
		return nil
	}
	r.RouteAndHandle(context.Background(), InboundMessage{ChannelType: ChannelDM, FromUID: "active"}, active)

	if reapedLocks != 1 {
		t.Fatalf("only the idle lock should be reaped, got %d", reapedLocks)
	}
	if _, ok := r.locks["old"]; ok {
		t.Fatal("idle lock 'old' should have been evicted")
	}
	if _, ok := r.locks["active"]; !ok {
		t.Fatal("in-flight lock 'active' must survive reaping")
	}
}

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
