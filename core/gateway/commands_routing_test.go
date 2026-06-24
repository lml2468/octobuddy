package gateway

import (
	"context"
	"testing"

	"github.com/lml2468/octobuddy/core/router"
)

// --- Decision → friendly drop replies ---

func TestDroppedTooLongReplies(t *testing.T) {
	st := newTestStore(t)
	drv := &fakeDriver{threadID: "t", reply: "x"}
	sink := &recordSink{}
	// MaxContentByte tiny so a short message trips it.
	gw := New(drv, st, router.New(router.Config{MaxPerMinute: 100, MaxContentByte: 4}), sink)

	d, err := gw.Handle(context.Background(),
		router.InboundMessage{ChannelType: router.ChannelDM, FromUID: "u1", Text: "this is too long"})
	if err != nil {
		t.Fatal(err)
	}
	if d != router.DroppedTooLong {
		t.Fatalf("want too_long, got %s", d)
	}
	if len(drv.requests) != 0 {
		t.Fatalf("oversized message must not reach the driver")
	}
	got := sink.all()
	if len(got) != 1 || got[0].text != oversizedReply || got[0].key != "u1" {
		t.Fatalf("oversize reply wrong: %+v", got)
	}
}

func TestRateLimitedRepliesOncePerWindow(t *testing.T) {
	st := newTestStore(t)
	drv := &fakeDriver{threadID: "t", reply: "ok"}
	sink := &recordSink{}
	// One token: first turn passes, the rest are rate-limited.
	gw := New(drv, st, router.New(router.Config{MaxPerMinute: 1}), sink)

	mk := func() router.InboundMessage {
		return router.InboundMessage{ChannelType: router.ChannelDM, FromUID: "u1", Text: "hi"}
	}
	// Turn 1: accepted.
	if d, _ := gw.Handle(context.Background(), mk()); d != router.Accepted {
		t.Fatalf("turn1 want accepted, got %s", d)
	}
	// Turns 2..N: rate-limited. The first rejection of the window surfaces as
	// RateLimited (notify), the rest as RateLimitedSilent (deduped) — both are
	// rejections; only the reply count below must be 1.
	for i := 0; i < 3; i++ {
		if d, _ := gw.Handle(context.Background(), mk()); d != router.RateLimited && d != router.RateLimitedSilent {
			t.Fatalf("turn%d want a rate-limited decision, got %s", i+2, d)
		}
	}
	// Exactly ONE "请稍后再试" reply across the burst (deduped per window).
	if n := sink.count(rateLimitedReply); n != 1 {
		t.Fatalf("rate-limit reply should be deduped to 1, got %d", n)
	}
}

func TestNotMentionedAndUnroutableStaySilent(t *testing.T) {
	st := newTestStore(t)
	drv := &fakeDriver{threadID: "t", reply: "x"}
	sink := &recordSink{}
	gw := New(drv, st, router.New(router.Config{MaxPerMinute: 100}), sink)

	// Group without mention → silent drop.
	if d, _ := gw.Handle(context.Background(),
		router.InboundMessage{ChannelType: router.ChannelGroup, ChannelID: "c1", FromUID: "u1", Text: "hi"}); d != router.DroppedNotMentioned {
		t.Fatalf("want not_mentioned")
	}
	// Unroutable DM (no from_uid) → silent drop.
	if d, _ := gw.Handle(context.Background(),
		router.InboundMessage{ChannelType: router.ChannelDM, Text: "hi"}); d != router.DroppedUnroutable {
		t.Fatalf("want unroutable")
	}
	if len(sink.all()) != 0 {
		t.Fatalf("silent drops must not reply: %+v", sink.all())
	}
}
