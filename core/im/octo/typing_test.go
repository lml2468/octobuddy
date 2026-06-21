package octo

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lml2468/xclaw/core/agent"
)

// newTypingTestConnector builds a connector wired for offline typing-heartbeat
// tests: a fast tick, a counting sendTyping seam (no live IM), a known reply
// target, and a run context the test controls.
func newTypingTestConnector(ctx context.Context, interval time.Duration) (*Connector, *int32) {
	var count int32
	c := NewConnector(NewRESTClient("http://unused", func() string { return "t" }))
	c.setCtx(ctx)
	c.typingInterval = interval
	c.sendTyping = func(context.Context, string, ChannelType) error {
		atomic.AddInt32(&count, 1)
		return nil
	}
	c.targets["sess"] = replyTarget{channelID: "chan", channelType: ChannelGroup}
	return c, &count
}

// TestTypingHeartbeatStartsFiresStops proves the heartbeat: KindSessionStarted
// fires one ping immediately and then re-sends every interval; KindTurnDone
// stops it and no further pings arrive.
func TestTypingHeartbeatStartsFiresStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, count := newTypingTestConnector(ctx, 10*time.Millisecond)

	c.OnEvent("sess", agent.AgentEvent{Kind: agent.KindSessionStarted})
	if got := atomic.LoadInt32(count); got != 1 {
		t.Fatalf("expected 1 immediate typing ping, got %d", got)
	}

	// Let several ticks fire.
	time.Sleep(55 * time.Millisecond)
	if got := atomic.LoadInt32(count); got < 3 {
		t.Fatalf("expected repeated typing pings (>=3), got %d", got)
	}

	c.OnEvent("sess", agent.AgentEvent{Kind: agent.KindTurnDone})
	after := atomic.LoadInt32(count)

	// No more pings after stop.
	time.Sleep(40 * time.Millisecond)
	if got := atomic.LoadInt32(count); got != after {
		t.Fatalf("ticker kept firing after turn-done: %d -> %d", after, got)
	}

	// The ticker goroutine must be gone (map empty after stop).
	c.mu.Lock()
	n := len(c.typers)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("typers map not cleaned up: %d entries", n)
	}
}

// TestTypingHeartbeatStopsOnReply confirms OnReply stops the heartbeat (the
// normal end-of-turn cleanup path).
func TestTypingHeartbeatStopsOnReply(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, count := newTypingTestConnector(ctx, 10*time.Millisecond)
	// Empty reply so OnReply doesn't attempt a real SendText to the dummy URL.
	c.OnEvent("sess", agent.AgentEvent{Kind: agent.KindSessionStarted})
	c.OnReply("sess", "")

	stopped := atomic.LoadInt32(count)
	time.Sleep(40 * time.Millisecond)
	if got := atomic.LoadInt32(count); got != stopped {
		t.Fatalf("ticker kept firing after reply: %d -> %d", stopped, got)
	}
	c.mu.Lock()
	n := len(c.typers)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("typers map not cleaned up after reply: %d entries", n)
	}
}

// TestTypingHeartbeatStopsOnError proves a turn that errors out without ever
// producing a reply still cleans up its ticker — but only on a terminal
// (non-recoverable) error.
func TestTypingHeartbeatStopsOnError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, count := newTypingTestConnector(ctx, 10*time.Millisecond)
	c.OnEvent("sess", agent.AgentEvent{Kind: agent.KindSessionStarted})
	c.OnEvent("sess", agent.AgentEvent{Kind: agent.KindError, Err: "boom", Recoverable: false})

	stopped := atomic.LoadInt32(count)
	time.Sleep(40 * time.Millisecond)
	if got := atomic.LoadInt32(count); got != stopped {
		t.Fatalf("ticker kept firing after terminal error: %d -> %d", stopped, got)
	}
	c.mu.Lock()
	n := len(c.typers)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("typers map not cleaned up after error: %d entries", n)
	}
}

// TestTypingHeartbeatSurvivesRecoverableError proves a mid-turn recoverable
// error (a stderr warning) does NOT stop the heartbeat — the turn is still
// running and the indicator must keep refreshing.
func TestTypingHeartbeatSurvivesRecoverableError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, count := newTypingTestConnector(ctx, 10*time.Millisecond)
	c.OnEvent("sess", agent.AgentEvent{Kind: agent.KindSessionStarted})
	c.OnEvent("sess", agent.AgentEvent{Kind: agent.KindError, Err: "a warning on stderr", Recoverable: true})

	before := atomic.LoadInt32(count)
	time.Sleep(40 * time.Millisecond)
	if got := atomic.LoadInt32(count); got <= before {
		t.Fatalf("heartbeat stopped on a recoverable error: %d -> %d", before, got)
	}
	c.mu.Lock()
	n := len(c.typers)
	c.mu.Unlock()
	if n != 1 {
		t.Fatalf("ticker should still be active after recoverable error, got %d", n)
	}
	c.stopTyping("sess")
}

// TestTypingHeartbeatStopsOnCtxCancel proves cancelling the run context tears
// down the heartbeat goroutine (no leak on shutdown).
func TestTypingHeartbeatStopsOnCtxCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c, count := newTypingTestConnector(ctx, 10*time.Millisecond)
	c.OnEvent("sess", agent.AgentEvent{Kind: agent.KindSessionStarted})

	time.Sleep(25 * time.Millisecond)
	cancel()
	// Give the goroutine time to observe cancellation and exit.
	time.Sleep(40 * time.Millisecond)
	frozen := atomic.LoadInt32(count)
	time.Sleep(40 * time.Millisecond)
	if got := atomic.LoadInt32(count); got != frozen {
		t.Fatalf("ticker kept firing after ctx cancel: %d -> %d", frozen, got)
	}
	// stopTyping must still be safe (join the already-exited goroutine).
	c.stopTyping("sess")
}

// TestTypingStartIdempotent proves a second KindSessionStarted does not spawn a
// duplicate ticker for the same session.
func TestTypingStartIdempotent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, _ := newTypingTestConnector(ctx, 10*time.Millisecond)
	c.OnEvent("sess", agent.AgentEvent{Kind: agent.KindSessionStarted})
	c.OnEvent("sess", agent.AgentEvent{Kind: agent.KindSessionStarted})
	c.mu.Lock()
	n := len(c.typers)
	c.mu.Unlock()
	if n != 1 {
		t.Fatalf("expected exactly 1 ticker after double start, got %d", n)
	}
	c.stopTyping("sess")
}
