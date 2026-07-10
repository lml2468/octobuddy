package gateway

import (
	"context"
	"testing"

	"github.com/lml2468/octobuddy/core/agent"
	"github.com/lml2468/octobuddy/core/groupctx"
	"github.com/lml2468/octobuddy/core/router"
	"github.com/lml2468/octobuddy/core/trigger"
)

// groupMsg builds a group inbound addressed to the bot so runTurn accepts it.
func groupMsg(text string, seq int64) router.InboundMessage {
	return router.InboundMessage{
		ChannelType: router.ChannelGroup, ChannelID: "c1",
		FromUID: "u1", FromName: "alice", Text: text, MessageSeq: seq,
		Trigger: &trigger.TriggerDecision{Reason: trigger.ReasonExplicitBot, Source: trigger.SourceUser},
	}
}

// TestGroupCursorHeldWhenDelivered: a successful group turn advances the cursor
// and finishCursor must NOT roll it back (delivered==true).
func TestGroupCursorHeldWhenDelivered(t *testing.T) {
	st := newTestStore(t)
	gc := groupctx.New(6000)
	drv := &fakeDriver{threadID: "t", reply: "ok"}
	gw := New(drv, st, router.New(router.Config{MaxPerMinute: 100}), newCaptureSink()).WithGroupContext(gc)

	if _, err := gw.Handle(context.Background(), groupMsg("hello bot", 10)); err != nil {
		t.Fatal(err)
	}
	// A delivered turn keeps the cursor advanced (not rewound to the pre-turn 0).
	if got := gc.Cursor("c1"); got == 0 {
		t.Fatalf("delivered turn must hold advanced cursor, got 0 (rewound)")
	}
}

// TestFinishCursorRewindsOnlyWhenNotDelivered exercises the structural guard
// directly: finishCursor rolls the group cursor back to the captured preCursor
// iff the turn did not deliver, and holds it otherwise.
func TestFinishCursorRewindsOnlyWhenNotDelivered(t *testing.T) {
	st := newTestStore(t)
	gc := groupctx.New(6000)
	gw := New(&fakeDriver{}, st, router.New(router.Config{MaxPerMinute: 100}), newCaptureSink()).WithGroupContext(gc)

	// Seed a cursor, then simulate a turn that advanced it but did NOT deliver.
	gc.Push("c1", "u1", "alice", "m1", 5)
	gc.SetCursor("c1", 5)
	tc := &TurnContext{msg: groupMsg("x", 6)}
	tc.preCursor, tc.hasCursor = gw.captureGroupCursor(tc.msg)
	gc.SetCursor("c1", 9) // pretend the turn advanced it
	tc.delivered = false
	gw.finishCursor(tc)()
	if got := gc.Cursor("c1"); got != 5 {
		t.Fatalf("undelivered turn must rewind cursor to preCursor 5, got %d", got)
	}

	// Same setup but delivered → cursor held.
	tc2 := &TurnContext{msg: groupMsg("y", 7)}
	tc2.preCursor, tc2.hasCursor = gw.captureGroupCursor(tc2.msg)
	gc.SetCursor("c1", 12)
	tc2.delivered = true
	gw.finishCursor(tc2)()
	if got := gc.Cursor("c1"); got != 12 {
		t.Fatalf("delivered turn must hold cursor at 12, got %d", got)
	}
}

// TestDeliveredReplyPerBranch drives each terminal branch and asserts the
// expected reply reached the sink (delivery happened via exactly the intended
// branch).
func TestDeliveredReplyPerBranch(t *testing.T) {
	cases := []struct {
		name  string
		drv   agent.Driver
		reply string
	}{
		{"success", &fakeDriver{threadID: "t", reply: "hi"}, "hi"},
		{"transient", &scriptedDriver{script: func(agent.Request, int) ([]agent.AgentEvent, error) {
			return evTransient(), nil
		}}, busyReply},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := newCaptureSink()
			gw := New(tc.drv, newTestStore(t), router.New(router.Config{MaxPerMinute: 100}), sink)
			if _, err := gw.Handle(context.Background(), router.InboundMessage{
				ChannelType: router.ChannelDM, FromUID: "u1", FromName: "a", Text: "hi",
			}); err != nil {
				t.Fatal(err)
			}
			if got := sink.replies["u1"]; got != tc.reply {
				t.Fatalf("reply = %q, want %q (branch=%s)", got, tc.reply, tc.name)
			}
		})
	}
}
