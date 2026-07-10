package gateway

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/lml2468/octobuddy/core/agent"
	"github.com/lml2468/octobuddy/core/router"
	"github.com/lml2468/octobuddy/core/store"
	"github.com/lml2468/octobuddy/core/trigger"
)

// fakeDriver returns a scripted event stream and records the requests it saw,
// so the gateway pipeline can be tested deterministically without a live agent.
type fakeDriver struct {
	mu       sync.Mutex
	requests []agent.Request
	threadID string
	reply    string
}

func (f *fakeDriver) Name() string                     { return "fake" }
func (f *fakeDriver) Capabilities() agent.Capabilities { return agent.Capabilities{Resume: true} }

func (f *fakeDriver) Query(ctx context.Context, req agent.Request) (<-chan agent.AgentEvent, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()

	ch := make(chan agent.AgentEvent, 8)
	go func() {
		defer close(ch)
		ch <- agent.AgentEvent{Kind: agent.KindSessionStarted, SessionID: f.threadID}
		ch <- agent.AgentEvent{Kind: agent.KindTextDelta, Text: f.reply}
		ch <- agent.AgentEvent{Kind: agent.KindTurnDone, Usage: &agent.TokenUsage{OutputTokens: 5}}
	}()
	return ch, nil
}

// scriptedDriver is the shared configurable fake for resilience/breaker tests:
// it records each request, counts Query calls, and streams whatever the script
// returns for that call. A script returning a non-nil error makes Query itself
// fail (a top-level fork/exec-style error). This collapses the many one-off
// driver structs that differed only in their event script. The genuinely
// distinct control-flow fakes (panic in Query, block-on-ctx) stay separate.
type scriptedDriver struct {
	mu       sync.Mutex
	scoping  bool // Capabilities.ToolScoping
	queries  int
	requests []agent.Request
	// script yields the events to stream for the (1-based) call given the request;
	// a non-nil error is returned from Query directly (no stream).
	script func(req agent.Request, call int) ([]agent.AgentEvent, error)
}

func (d *scriptedDriver) Name() string { return "fake" }
func (d *scriptedDriver) Capabilities() agent.Capabilities {
	return agent.Capabilities{Resume: true, ToolScoping: d.scoping}
}
func (d *scriptedDriver) count() int { d.mu.Lock(); defer d.mu.Unlock(); return d.queries }
func (d *scriptedDriver) Query(ctx context.Context, req agent.Request) (<-chan agent.AgentEvent, error) {
	d.mu.Lock()
	d.queries++
	d.requests = append(d.requests, req)
	call := d.queries
	d.mu.Unlock()
	evs, err := d.script(req, call)
	if err != nil {
		return nil, err
	}
	ch := make(chan agent.AgentEvent, len(evs)+1)
	go func() {
		defer close(ch)
		for _, ev := range evs {
			ch <- ev
		}
	}()
	return ch, nil
}

// Scripted event helpers shared by the resilience/breaker tests.
func evTransient() []agent.AgentEvent {
	return []agent.AgentEvent{{Kind: agent.KindError, Err: "429 rate limit", Transient: true}}
}
func evReply(text string) []agent.AgentEvent {
	return []agent.AgentEvent{
		{Kind: agent.KindSessionStarted, SessionID: "s"},
		{Kind: agent.KindTextDelta, Text: text},
		{Kind: agent.KindTurnDone},
	}
}

type captureSink struct {
	mu       sync.Mutex
	events   []agent.AgentEvent
	replies  map[string]string
	userMsgs []router.InboundMessage // captured for OnUserMessage assertions
}

func newCaptureSink() *captureSink { return &captureSink{replies: map[string]string{}} }
func (c *captureSink) OnEvent(key string, ev agent.AgentEvent) {
	c.mu.Lock()
	c.events = append(c.events, ev)
	c.mu.Unlock()
}
func (c *captureSink) OnReply(key, text string) {
	c.mu.Lock()
	c.replies[key] = text
	c.mu.Unlock()
}
func (c *captureSink) OnUserMessage(_ string, msg router.InboundMessage) {
	c.mu.Lock()
	c.userMsgs = append(c.userMsgs, msg)
	c.mu.Unlock()
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "gw.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestFullTurnPipeline(t *testing.T) {
	st := newTestStore(t)
	drv := &fakeDriver{threadID: "thr-1", reply: "hello back"}
	sink := newCaptureSink()
	gw := New(drv, st, router.New(router.Config{MaxPerMinute: 100}), sink)

	msg := router.InboundMessage{ChannelType: router.ChannelDM, FromUID: "u1", FromName: "alice", Text: "hi"}
	d, err := gw.Handle(context.Background(), msg)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if d != router.Accepted {
		t.Fatalf("want accepted, got %s", d)
	}
	assertFullTurnDriverRequest(t, drv)
	assertFullTurnReply(t, sink)
	assertFullTurnHistory(t, st)
	assertFullTurnResume(t, st)
}

func assertFullTurnDriverRequest(t *testing.T, drv *fakeDriver) {
	t.Helper()
	if len(drv.requests) != 1 || drv.requests[0].Prompt != "hi" || drv.requests[0].SessionID != "" {
		t.Fatalf("first request wrong: %+v", drv.requests)
	}
}

func assertFullTurnReply(t *testing.T, sink *captureSink) {
	t.Helper()
	if sink.replies["u1"] != "hello back" {
		t.Fatalf("reply not delivered: %q", sink.replies["u1"])
	}
}

func assertFullTurnHistory(t *testing.T, st *store.Store) {
	t.Helper()
	msgs, _ := st.RecentMessages("u1", 10)
	if len(msgs) != 2 || msgs[0].Role != store.RoleUser || msgs[1].Role != store.RoleAssistant {
		t.Fatalf("history wrong: %+v", msgs)
	}
	if msgs[1].Content != "hello back" {
		t.Fatalf("assistant content wrong: %q", msgs[1].Content)
	}
}

func assertFullTurnResume(t *testing.T, st *store.Store) {
	t.Helper()
	if got, _ := st.Resume("u1", "fake"); got != "thr-1" {
		t.Fatalf("resume not persisted: %q", got)
	}
}

func TestSecondTurnResumes(t *testing.T) {
	st := newTestStore(t)
	drv := &fakeDriver{threadID: "thr-1", reply: "ok"}
	gw := New(drv, st, router.New(router.Config{MaxPerMinute: 100}), newCaptureSink())

	msg := router.InboundMessage{ChannelType: router.ChannelDM, FromUID: "u1", Text: "turn1"}
	if _, err := gw.Handle(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	msg.Text = "turn2"
	if _, err := gw.Handle(context.Background(), msg); err != nil {
		t.Fatal(err)
	}

	// Second request must carry the resume id from the first turn.
	if len(drv.requests) != 2 {
		t.Fatalf("want 2 requests, got %d", len(drv.requests))
	}
	if drv.requests[1].SessionID != "thr-1" {
		t.Fatalf("second turn must resume thr-1, got %q", drv.requests[1].SessionID)
	}
}

func TestGroupMentionGateAtGateway(t *testing.T) {
	st := newTestStore(t)
	drv := &fakeDriver{threadID: "t", reply: "x"}
	gw := New(drv, st, router.New(router.Config{MaxPerMinute: 100}), newCaptureSink())

	// Group without mention → connector routes to Observe (after #117 the
	// gateway no longer pre-branches observations in Handle). The driver
	// must not run.
	gw.Observe(router.InboundMessage{
		ChannelType: router.ChannelGroup, ChannelID: "c1", FromUID: "u1", Text: "hi",
	})
	if len(drv.requests) != 0 {
		t.Fatalf("Observe must not invoke driver: got %d requests", len(drv.requests))
	}

	// Precondition contract: if a future caller mis-routes a no-trigger
	// group message into Handle (single-defense), the router silently
	// returns DroppedInvariantBreak rather than crashing the daemon.
	d, _ := gw.Handle(context.Background(),
		router.InboundMessage{ChannelType: router.ChannelGroup, ChannelID: "c1", FromUID: "u1", Text: "hi"})
	if d != router.DroppedInvariantBreak {
		t.Fatalf("precondition-violation must yield invariant_break, got %s", d)
	}
}

func TestSandboxInjectsCwdAndMemoryPerSession(t *testing.T) {
	base := t.TempDir()
	cwdBase := filepath.Join(base, "workspace")
	memBase := filepath.Join(base, "memory")

	st := newTestStore(t)
	drv := &fakeDriver{threadID: "t", reply: "ok"}
	gw := New(drv, st, router.New(router.Config{MaxPerMinute: 100}), newCaptureSink()).
		WithSandbox(cwdBase, memBase)

	// DM turn → request carries a per-session cwd that exists on disk + a memory dir.
	_, err := gw.Handle(context.Background(),
		router.InboundMessage{ChannelType: router.ChannelDM, FromUID: "u1", FromName: "alice", Text: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	dmReq := drv.requests[0]
	assertDMSandboxRequest(t, cwdBase, memBase, dmReq)

	// Group turn (mentioned) → different sandbox (kind prefix scopes the hash).
	_, err = gw.Handle(context.Background(),
		router.InboundMessage{ChannelType: router.ChannelGroup, ChannelID: "c1", FromUID: "u1", FromName: "alice", Text: "hi", Trigger: &trigger.TriggerDecision{Reason: trigger.ReasonExplicitBot, Source: trigger.SourceUser}})
	if err != nil {
		t.Fatal(err)
	}
	assertGroupSandboxRequest(t, drv.requests[1], dmReq)
}

func assertDMSandboxRequest(t *testing.T, cwdBase, memBase string, dmReq agent.Request) {
	t.Helper()

	if dmReq.Cwd == "" {
		t.Fatal("DM turn missing Cwd")
	}
	if st, err := os.Stat(dmReq.Cwd); err != nil || !st.IsDir() {
		t.Fatalf("Cwd not created on disk: %v", err)
	}
	if filepath.Dir(dmReq.Cwd) != cwdBase {
		t.Fatalf("Cwd not under cwdBase: %q", dmReq.Cwd)
	}
	if dmReq.MemoryDir == "" || filepath.Dir(dmReq.MemoryDir) != memBase {
		t.Fatalf("MemoryDir wrong: %q", dmReq.MemoryDir)
	}
	// cwd and memory share the same per-session hash component.
	if filepath.Base(dmReq.Cwd) != filepath.Base(dmReq.MemoryDir) {
		t.Fatalf("cwd/memory hash mismatch: %q vs %q", dmReq.Cwd, dmReq.MemoryDir)
	}
}

func assertGroupSandboxRequest(t *testing.T, grpReq, dmReq agent.Request) {
	t.Helper()

	if filepath.Base(grpReq.Cwd) == filepath.Base(dmReq.Cwd) {
		t.Fatal("group and DM must resolve to distinct sandboxes")
	}
}

func TestNoSandboxLeavesCwdEmpty(t *testing.T) {
	st := newTestStore(t)
	drv := &fakeDriver{threadID: "t", reply: "ok"}
	gw := New(drv, st, router.New(router.Config{MaxPerMinute: 100}), newCaptureSink())
	_, err := gw.Handle(context.Background(),
		router.InboundMessage{ChannelType: router.ChannelDM, FromUID: "u1", FromName: "alice", Text: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if drv.requests[0].Cwd != "" || drv.requests[0].MemoryDir != "" {
		t.Fatalf("without WithSandbox, Cwd/MemoryDir must stay empty: %+v", drv.requests[0])
	}
}
