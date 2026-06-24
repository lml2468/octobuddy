package gateway

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lml2468/octobuddy/core/agent"
	"github.com/lml2468/octobuddy/core/router"
	"github.com/lml2468/octobuddy/core/store"
)

// trickleDriver streams `pulses` events spaced `interval` apart, then a final
// session_started + text + turn_done, then closes. Used to prove the idle
// dispatch timeout resets on every event — a stream slower than the timeout's
// half but faster than the timeout itself must complete, not be killed.
type trickleDriver struct {
	pulses   int
	interval time.Duration
	reply    string
}

func (d *trickleDriver) Name() string                     { return "trickle" }
func (d *trickleDriver) Capabilities() agent.Capabilities { return agent.Capabilities{Resume: true} }
func (d *trickleDriver) Query(ctx context.Context, _ agent.Request) (<-chan agent.AgentEvent, error) {
	ch := make(chan agent.AgentEvent, 4)
	go func() {
		defer close(ch)
		ch <- agent.AgentEvent{Kind: agent.KindSessionStarted, SessionID: "trickle"}
		for i := 0; i < d.pulses; i++ {
			select {
			case <-ctx.Done():
				return
			case <-time.After(d.interval):
			}
			select {
			case ch <- agent.AgentEvent{Kind: agent.KindThinking}:
			case <-ctx.Done():
				return
			}
		}
		ch <- agent.AgentEvent{Kind: agent.KindTextDelta, Text: d.reply}
		ch <- agent.AgentEvent{Kind: agent.KindTurnDone}
	}()
	return ch, nil
}

func TestDispatchTimeoutResetsOnEvents(t *testing.T) {
	st := newTestStore(t)
	// 4 pulses × 30ms = 120ms of streaming, plus the session_started/text/done
	// — well over the 80ms idle window, but no GAP between events exceeds 30ms,
	// so the idle timer must reset and the turn must complete normally.
	drv := &trickleDriver{pulses: 4, interval: 30 * time.Millisecond, reply: "ok"}
	sink := &recordSink{}
	gw := New(drv, st, router.New(router.Config{MaxPerMinute: 100}), sink).
		WithDispatchTimeout(80 * time.Millisecond)

	start := time.Now()
	if _, err := gw.Handle(context.Background(),
		router.InboundMessage{ChannelType: router.ChannelDM, FromUID: "u1", Text: "trickle"}); err != nil {
		t.Fatalf("trickle turn errored: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("trickle turn returned suspiciously fast (%s) — events may not have flowed", elapsed)
	}
	// Must NOT have apologized — the real reply should arrive.
	if n := sink.count(timeoutReply); n != 0 {
		t.Fatalf("idle timer did not reset on events: got %d timeout apologies", n)
	}
	got := sink.all()
	if len(got) == 0 || got[len(got)-1].text != "ok" {
		t.Fatalf("want trailing reply \"ok\", got %+v", got)
	}
}

// --- dispatch timeout (#141) ---

func TestDispatchTimeoutFiresApology(t *testing.T) {
	st := newTestStore(t)
	drv := &blockingDriver{}
	sink := &recordSink{}
	gw := New(drv, st, router.New(router.Config{MaxPerMinute: 100}), sink).
		WithDispatchTimeout(50 * time.Millisecond)

	start := time.Now()
	d, err := gw.Handle(context.Background(),
		router.InboundMessage{ChannelType: router.ChannelDM, FromUID: "u1", Text: "hang please"})
	if err != nil {
		t.Fatalf("timeout path must not error: %v", err)
	}
	if d != router.Accepted {
		t.Fatalf("timed-out turn is still an accepted route, got %s", d)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("turn did not release promptly on timeout: %s", elapsed)
	}
	// Apology delivered.
	got := sink.all()
	if len(got) != 1 || got[0].text != timeoutReply || got[0].key != "u1" {
		t.Fatalf("timeout apology wrong: %+v", got)
	}
}

func TestDispatchTimeoutReleasesSessionLock(t *testing.T) {
	st := newTestStore(t)
	drv := &blockingDriver{}
	sink := &recordSink{}
	gw := New(drv, st, router.New(router.Config{MaxPerMinute: 100}), sink).
		WithDispatchTimeout(40 * time.Millisecond)

	msg := router.InboundMessage{ChannelType: router.ChannelDM, FromUID: "u1", Text: "hang"}
	// First hung turn times out; the second turn must still be servable (lock
	// released), proving the queue isn't wedged.
	if _, err := gw.Handle(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		gw.Handle(context.Background(), msg) // also times out, but must return
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("second turn blocked — session lock not released after timeout")
	}
	if n := sink.count(timeoutReply); n != 2 {
		t.Fatalf("want 2 timeout apologies, got %d", n)
	}
}

// --- terminal agent error ---

// TestTerminalErrorDoesNotPersistPartialOrAdvanceResume asserts that a turn
// ending in a terminal agent error (e.g. max_turns) neither persists the partial
// reply as the assistant turn nor advances the resume id, and signals the user.
func TestTerminalErrorDoesNotPersistPartialOrAdvanceResume(t *testing.T) {
	st := newTestStore(t)
	drv := &erroringDriver{sessionID: "thr-err", partialText: "partial answer that should not persist"}
	sink := &recordSink{}
	gw := New(drv, st, router.New(router.Config{MaxPerMinute: 100}), sink)

	msg := router.InboundMessage{ChannelType: router.ChannelDM, FromUID: "u1", FromName: "alice", Text: "hi"}
	d, err := gw.Handle(context.Background(), msg)
	if err != nil {
		t.Fatalf("terminal-error path must not error the handler: %v", err)
	}
	if d != router.Accepted {
		t.Fatalf("want accepted, got %s", d)
	}

	// 1. Resume id NOT advanced — next turn must not skip past the errored work.
	if got, _ := st.Resume("u1", "fake"); got != "" {
		t.Fatalf("resume must not advance on a terminal error, got %q", got)
	}
	// 2. Partial reply NOT persisted as an assistant turn (only the user message).
	msgs, _ := st.RecentMessages("u1", 10)
	for _, m := range msgs {
		if m.Role == store.RoleAssistant {
			t.Fatalf("errored turn must not persist an assistant reply, got %q", m.Content)
		}
	}
	// 3. User is signaled with the error reply, not the partial text.
	got := sink.all()
	if len(got) != 1 || got[0].text != errorReply || got[0].key != "u1" {
		t.Fatalf("want one errorReply to u1, got %+v", got)
	}
}

// transientErroringDriver streams a terminal error tagged Transient (an upstream
// rate-limit / overload), with a reset-window hint. The gateway must reply with
// the distinct busyReply (including the hint) rather than the generic errorReply.
type transientErroringDriver struct{ hint string }

func (d *transientErroringDriver) Name() string { return "transient" }
func (d *transientErroringDriver) Capabilities() agent.Capabilities {
	return agent.Capabilities{Resume: true}
}
func (d *transientErroringDriver) Query(ctx context.Context, _ agent.Request) (<-chan agent.AgentEvent, error) {
	ch := make(chan agent.AgentEvent, 8)
	go func() {
		defer close(ch)
		ch <- agent.AgentEvent{Kind: agent.KindSessionStarted, SessionID: "thr-busy"}
		ch <- agent.AgentEvent{Kind: agent.KindError, Err: "usage limit reached", Transient: true, RetryHint: d.hint}
		ch <- agent.AgentEvent{Kind: agent.KindTurnDone}
	}()
	return ch, nil
}

// TestTransientErrorYieldsBusyReply asserts an upstream rate-limit terminal error
// produces the distinct busyReply (with the reset hint) and, like any terminal
// error, neither persists a reply nor advances the resume id.
func TestTransientErrorYieldsBusyReply(t *testing.T) {
	st := newTestStore(t)
	drv := &transientErroringDriver{hint: "3pm (PST)"}
	sink := &recordSink{}
	gw := New(drv, st, router.New(router.Config{MaxPerMinute: 100}), sink)

	msg := router.InboundMessage{ChannelType: router.ChannelDM, FromUID: "u1", FromName: "alice", Text: "hi"}
	if _, err := gw.Handle(context.Background(), msg); err != nil {
		t.Fatalf("transient-error path must not error the handler: %v", err)
	}
	if got, _ := st.Resume("u1", "fake"); got != "" {
		t.Fatalf("resume must not advance on a transient error, got %q", got)
	}
	got := sink.all()
	if len(got) != 1 || got[0].key != "u1" {
		t.Fatalf("want one reply to u1, got %+v", got)
	}
	if !strings.HasPrefix(got[0].text, busyReply) {
		t.Fatalf("want busyReply prefix, got %q", got[0].text)
	}
	if !strings.Contains(got[0].text, "3pm (PST)") {
		t.Fatalf("busy reply should carry the reset hint, got %q", got[0].text)
	}
}

// TestRecoverableErrorDoesNotAbortTurn is the mirror of the terminal-error test:
// a recoverable error (stderr warning / post-completion non-zero exit) must NOT
// gate a successful turn. The reply persists, the resume id advances, and the
// user gets the real reply — not the generic errorReply. Guards the
// `if !ev.Recoverable` discriminator against being widened to "any KindError".
func TestRecoverableErrorDoesNotAbortTurn(t *testing.T) {
	st := newTestStore(t)
	drv := &recoverableErroringDriver{sessionID: "thr-ok", reply: "the real answer"}
	sink := &recordSink{}
	gw := New(drv, st, router.New(router.Config{MaxPerMinute: 100}), sink)

	msg := router.InboundMessage{ChannelType: router.ChannelDM, FromUID: "u1", FromName: "alice", Text: "hi"}
	if _, err := gw.Handle(context.Background(), msg); err != nil {
		t.Fatalf("handle: %v", err)
	}

	// 1. Resume id advanced — the turn completed successfully.
	if got, _ := st.Resume("u1", "recoverable"); got != "thr-ok" {
		t.Fatalf("resume should advance on a recoverable error, got %q", got)
	}
	// 2. The reply persisted as an assistant turn.
	msgs, _ := st.RecentMessages("u1", 10)
	var persisted string
	for _, m := range msgs {
		if m.Role == store.RoleAssistant {
			persisted = m.Content
		}
	}
	if persisted != "the real answer" {
		t.Fatalf("assistant reply should persist, got %q", persisted)
	}
	// 3. User got the real reply, not errorReply.
	got := sink.all()
	if len(got) != 1 || got[0].text != "the real answer" || got[0].key != "u1" {
		t.Fatalf("want the real reply to u1, got %+v", got)
	}
}
