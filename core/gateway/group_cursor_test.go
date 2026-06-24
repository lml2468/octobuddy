package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/lml2468/octobuddy/core/groupctx"
	"github.com/lml2468/octobuddy/core/router"
	"github.com/lml2468/octobuddy/core/safety"
	"github.com/lml2468/octobuddy/core/store"
)

// TestColdStartBackfillRunsOnce verifies the gateway seeds an empty group window
// from the backfill callback the first time a channel is seen (cc G4), and only
// once. The seeded message appears in the next mention turn's delta.
func TestColdStartBackfillRunsOnce(t *testing.T) {
	st := newTestStore(t)
	drv := &fakeDriver{threadID: "t", reply: "ok"}
	gc := groupctx.New(6000)
	calls := 0
	fetch := func(channelID string, limit int) []groupctx.BackfillMessage {
		calls++
		return []groupctx.BackfillMessage{
			{FromUID: "alice", FromName: "alice", Content: "backfilled-q", Seq: 10},
		}
	}
	gw := New(drv, st, router.New(router.Config{MaxPerMinute: 100}), newCaptureSink()).
		WithGroupContext(gc).
		WithGroupBackfill(func() string { return "bot" }, fetch)

	// First mention turn: window is empty -> backfill seeds it.
	_, err := gw.Handle(context.Background(), router.InboundMessage{
		ChannelType: router.ChannelGroup, ChannelID: "c1", FromUID: "bob", FromName: "bob",
		Text: "current question", Mentioned: true, MessageSeq: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("backfill fetch called %d times, want 1", calls)
	}
	p1 := drv.requests[0].Prompt
	if !strings.Contains(p1, "backfilled-q") {
		t.Fatalf("seeded backfill message missing from delta:\n%s", p1)
	}

	// Second mention turn: window now warm -> no re-fetch.
	_, err = gw.Handle(context.Background(), router.InboundMessage{
		ChannelType: router.ChannelGroup, ChannelID: "c1", FromUID: "bob", FromName: "bob",
		Text: "another question", Mentioned: true, MessageSeq: 21,
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("backfill must not re-run: calls=%d", calls)
	}
}

// TestReplyCursorAdvancesAndSegments verifies that after the bot replies to a
// group message, a later turn segments that message under [Previously answered]
// (cc G10), and that the current message is never echoed into its own delta.
func TestReplyCursorAdvancesAndSegments(t *testing.T) {
	st := newTestStore(t)
	drv := &fakeDriver{threadID: "t", reply: "answer"}
	gc := groupctx.New(6000)
	gw := New(drv, st, router.New(router.Config{MaxPerMinute: 100}), newCaptureSink()).
		WithGroupContext(gc)

	// Turn 1: bob asks; bot replies. Inbound seq 100 becomes the answered cutoff.
	_, err := gw.Handle(context.Background(), router.InboundMessage{
		ChannelType: router.ChannelGroup, ChannelID: "c1", FromUID: "bob", FromName: "bob",
		Text: "first-question", Mentioned: true, MessageSeq: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertFirstReplyCursorTurn(t, st, drv.requests[0].Prompt)

	// Between turns, two messages are observed (non-mention background). The
	// injection cursor only advances on a turn, so both land in turn 2's delta.
	// One carries a seq AT/BELOW the answered cutoff (a late-delivered earlier
	// message) and one ABOVE it, so the delta straddles the cutoff and must split.
	observeReplyCursorMessages(gw)

	// Turn 2: carol asks at a higher seq. The observed answered message (seq 90)
	// renders under [Previously answered]; the seq-150 message under [New since
	// your last reply]; the current message must not be echoed.
	_, err = gw.Handle(context.Background(), router.InboundMessage{
		ChannelType: router.ChannelGroup, ChannelID: "c1", FromUID: "carol", FromName: "carol",
		Text: "second-question", Mentioned: true, MessageSeq: 200,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertSecondReplyCursorTurn(t, drv.requests[1].Prompt)
}

func assertFirstReplyCursorTurn(t *testing.T, st *store.Store, prompt string) {
	t.Helper()

	anchor := strings.Index(prompt, safety.CurrentMessageAnchor)
	if anchor < 0 {
		t.Fatalf("missing anchor:\n%s", prompt)
	}
	if strings.Contains(prompt[:anchor], "first-question") {
		t.Fatalf("current message echoed into its own delta:\n%s", prompt)
	}
	if seq, _ := st.BotReplySeq("c1"); seq != 100 {
		t.Fatalf("reply cursor not advanced: got %d, want 100", seq)
	}
}

func observeReplyCursorMessages(gw *Gateway) {
	gw.Observe(router.InboundMessage{
		ChannelType: router.ChannelGroup, ChannelID: "c1", FromUID: "dave", FromName: "dave",
		Text: "already-handled", MessageSeq: 90, // <= cutoff 100 -> answered
	})
	gw.Observe(router.InboundMessage{
		ChannelType: router.ChannelGroup, ChannelID: "c1", FromUID: "erin", FromName: "erin",
		Text: "fresh-chatter", MessageSeq: 150, // > cutoff 100 -> new
	})
}

func assertSecondReplyCursorTurn(t *testing.T, prompt string) {
	t.Helper()

	anchor := strings.Index(prompt, safety.CurrentMessageAnchor)
	if anchor < 0 {
		t.Fatalf("missing anchor (turn 2):\n%s", prompt)
	}
	delta2 := prompt[:anchor]
	if !strings.Contains(delta2, "[Previously answered]") {
		t.Fatalf("answered segment missing in turn 2 delta:\n%s", delta2)
	}
	if !strings.Contains(delta2, "already-handled") {
		t.Fatalf("answered message missing from answered segment:\n%s", delta2)
	}
	if !strings.Contains(delta2, "[New since your last reply]") || !strings.Contains(delta2, "fresh-chatter") {
		t.Fatalf("new segment missing in turn 2 delta:\n%s", delta2)
	}
	// answered precedes new
	if strings.Index(delta2, "already-handled") > strings.Index(delta2, "fresh-chatter") {
		t.Fatalf("answered must precede new:\n%s", delta2)
	}
	if strings.Contains(delta2, "second-question") {
		t.Fatalf("current message echoed into its own delta (turn 2):\n%s", delta2)
	}
}

// TestReplyCursorNotAdvancedForCronFire: a synthetic/cron message (seq 0) must
// not advance the answered cursor.
func TestReplyCursorNotAdvancedForCronFire(t *testing.T) {
	st := newTestStore(t)
	drv := &fakeDriver{threadID: "t", reply: "ok"}
	gc := groupctx.New(6000)
	gw := New(drv, st, router.New(router.Config{MaxPerMinute: 100}), newCaptureSink()).
		WithGroupContext(gc)

	_, err := gw.Handle(context.Background(), router.InboundMessage{
		ChannelType: router.ChannelGroup, ChannelID: "c1", FromUID: "bob", FromName: "bob",
		Text: "scheduled", CronFire: true, MessageSeq: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if seq, _ := st.BotReplySeq("c1"); seq != 0 {
		t.Fatalf("cron fire (seq 0) must not advance cursor: got %d", seq)
	}
}
