package gateway

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/lml2468/xclaw/core/agent"
	"github.com/lml2468/xclaw/core/router"
	"github.com/lml2468/xclaw/core/store"
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

type captureSink struct {
	mu      sync.Mutex
	events  []agent.AgentEvent
	replies map[string]string
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

	// 1. driver was invoked with the user's text, no resume id (first turn).
	if len(drv.requests) != 1 || drv.requests[0].Prompt != "hi" || drv.requests[0].SessionID != "" {
		t.Fatalf("first request wrong: %+v", drv.requests)
	}
	// 2. reply assembled from text deltas, delivered to sink.
	if sink.replies["u1"] != "hello back" {
		t.Fatalf("reply not delivered: %q", sink.replies["u1"])
	}
	// 3. user + assistant persisted.
	msgs, _ := st.RecentMessages("u1", 10)
	if len(msgs) != 2 || msgs[0].Role != store.RoleUser || msgs[1].Role != store.RoleAssistant {
		t.Fatalf("history wrong: %+v", msgs)
	}
	if msgs[1].Content != "hello back" {
		t.Fatalf("assistant content wrong: %q", msgs[1].Content)
	}
	// 4. resume id persisted.
	if got, _ := st.Resume("u1"); got != "thr-1" {
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

	// group without mention → dropped, driver never invoked.
	d, _ := gw.Handle(context.Background(),
		router.InboundMessage{ChannelType: router.ChannelGroup, ChannelID: "c1", FromUID: "u1", Text: "hi"})
	if d != router.DroppedNotMentioned {
		t.Fatalf("want not_mentioned, got %s", d)
	}
	if len(drv.requests) != 0 {
		t.Fatalf("driver should not run for dropped message")
	}
}

func TestSandboxInjectsCwdAndMemoryPerSession(t *testing.T) {
	base := t.TempDir()
	cwdBase := filepath.Join(base, "workspace")
	memBase := filepath.Join(base, "memory")

	st := newTestStore(t)
	drv := &fakeDriver{threadID: "t", reply: "ok"}
	gw := New(drv, st, router.New(router.Config{MaxPerMinute: 100}), newCaptureSink()).
		WithSandbox(cwdBase, memBase, "", "")

	// DM turn → request carries a per-session cwd that exists on disk + a memory dir.
	_, err := gw.Handle(context.Background(),
		router.InboundMessage{ChannelType: router.ChannelDM, FromUID: "u1", FromName: "alice", Text: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	dmReq := drv.requests[0]
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

	// Group turn (mentioned) → different sandbox (kind prefix scopes the hash).
	_, err = gw.Handle(context.Background(),
		router.InboundMessage{ChannelType: router.ChannelGroup, ChannelID: "c1", FromUID: "u1", FromName: "alice", Text: "hi", Mentioned: true})
	if err != nil {
		t.Fatal(err)
	}
	grpReq := drv.requests[1]
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
