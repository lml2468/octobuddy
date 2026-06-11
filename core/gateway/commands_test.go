package gateway

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lml2468/xclaw/core/agent"
	"github.com/lml2468/xclaw/core/router"
)

// recordSink records every OnReply call (in order), so tests can assert both the
// reply text and how many times a reply was sent (dedup).
type recordSink struct {
	mu      sync.Mutex
	replies []reply
}

type reply struct {
	key  string
	text string
}

func (s *recordSink) OnEvent(string, agent.AgentEvent) {}
func (s *recordSink) OnReply(key, text string) {
	s.mu.Lock()
	s.replies = append(s.replies, reply{key, text})
	s.mu.Unlock()
}
func (s *recordSink) all() []reply {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]reply, len(s.replies))
	copy(out, s.replies)
	return out
}
func (s *recordSink) count(text string) int {
	n := 0
	for _, r := range s.all() {
		if r.text == text {
			n++
		}
	}
	return n
}

// blockingDriver blocks in Query until ctx is cancelled (or a hard deadline), so
// the dispatch-timeout path can be exercised. It honors ctx cancellation by
// closing the event stream — exactly what ClaudeDriver does on CommandContext kill.
type blockingDriver struct {
	queried chan struct{} // closed-ish signal: receives once per Query
}

func (d *blockingDriver) Name() string                     { return "blocking" }
func (d *blockingDriver) Capabilities() agent.Capabilities { return agent.Capabilities{Resume: true} }
func (d *blockingDriver) Query(ctx context.Context, _ agent.Request) (<-chan agent.AgentEvent, error) {
	if d.queried != nil {
		select {
		case d.queried <- struct{}{}:
		default:
		}
	}
	ch := make(chan agent.AgentEvent)
	go func() {
		defer close(ch)
		<-ctx.Done() // block until the turn ctx is cancelled (timeout) — then end the stream
	}()
	return ch, nil
}

// --- parseCommand ---

func TestParseCommand(t *testing.T) {
	cases := []struct {
		in       string
		wantOK   bool
		wantName string
		wantArgs string
	}{
		{"/reset", true, "reset", ""},
		{"/RESET", true, "reset", ""},              // case-insensitive
		{"  /help  ", true, "help", ""},            // surrounding whitespace
		{"/config json", true, "config", "json"},   // arg after space
		{"@bot /reset", false, "", ""},             // mention not stripped here → not leading
		{"please /reset", false, "", ""},           // mid-sentence
		{"/reset/foo", false, "", ""},              // glued token boundary → not a command
		{"/config.json", false, "", ""},            // glued token boundary
		{"/help.md", false, "", ""},                // glued token boundary
		{"hello world", false, "", ""},             // plain text
		{"/reset\nsecond line", true, "reset", ""}, // only first line considered
		{"/", false, "", ""},                       // bare slash, no name
		{"/123", false, "", ""},                    // name must start with a letter
	}
	for _, c := range cases {
		got, ok := parseCommand(c.in)
		if ok != c.wantOK {
			t.Errorf("parseCommand(%q) ok=%v want %v", c.in, ok, c.wantOK)
			continue
		}
		if ok && (got.name != c.wantName || got.args != c.wantArgs) {
			t.Errorf("parseCommand(%q) = {%q,%q} want {%q,%q}", c.in, got.name, got.args, c.wantName, c.wantArgs)
		}
	}
}

// --- command dispatch via the full Handle path ---

func TestResetCommandClearsResumeAndReplies(t *testing.T) {
	st := newTestStore(t)
	drv := &fakeDriver{threadID: "thr-1", reply: "hi"}
	sink := &recordSink{}
	gw := New(drv, st, router.New(router.Config{MaxPerMinute: 100}), sink)

	// First a normal turn so a resume id is persisted.
	if _, err := gw.Handle(context.Background(),
		router.InboundMessage{ChannelType: router.ChannelDM, FromUID: "u1", Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Resume("u1"); got != "thr-1" {
		t.Fatalf("resume not persisted before reset: %q", got)
	}

	// Now /reset.
	d, err := gw.Handle(context.Background(),
		router.InboundMessage{ChannelType: router.ChannelDM, FromUID: "u1", Text: "/reset"})
	if err != nil {
		t.Fatal(err)
	}
	if d != router.Accepted {
		t.Fatalf("want accepted, got %s", d)
	}
	// Resume id cleared.
	if got, _ := st.Resume("u1"); got != "" {
		t.Fatalf("resume not cleared by /reset: %q", got)
	}
	// Stored conversation history cleared too (the /reset side effect, mirroring
	// cc-channel store.deleteSession).
	if msgs, _ := st.RecentMessages("u1", 10); len(msgs) != 0 {
		t.Fatalf("/reset must clear stored history, found %d messages", len(msgs))
	}
	// Command never reached the driver (only the first normal turn did).
	if len(drv.requests) != 1 {
		t.Fatalf("/reset must not invoke the driver: %d requests", len(drv.requests))
	}
	// Confirmation reply delivered.
	last := sink.all()[len(sink.all())-1]
	if last.key != "u1" || last.text != resetReply {
		t.Fatalf("reset reply wrong: %+v", last)
	}
}

func TestHelpAndConfigCommands(t *testing.T) {
	st := newTestStore(t)
	drv := &fakeDriver{threadID: "t", reply: "x"}
	sink := &recordSink{}
	gw := New(drv, st, router.New(router.Config{MaxPerMinute: 100}), sink).
		WithModel("claude-x").
		WithCommandInfo(7, 6000)

	if _, err := gw.Handle(context.Background(),
		router.InboundMessage{ChannelType: router.ChannelDM, FromUID: "u1", Text: "/help"}); err != nil {
		t.Fatal(err)
	}
	if _, err := gw.Handle(context.Background(),
		router.InboundMessage{ChannelType: router.ChannelDM, FromUID: "u1", Text: "/config"}); err != nil {
		t.Fatal(err)
	}

	replies := sink.all()
	if len(replies) != 2 {
		t.Fatalf("want 2 replies, got %d: %+v", len(replies), replies)
	}
	if replies[0].text != helpText {
		t.Fatalf("/help reply wrong: %q", replies[0].text)
	}
	cfg := replies[1].text
	for _, want := range []string{"claude-x", "7 req/min", "6000 chars"} {
		if !strings.Contains(cfg, want) {
			t.Fatalf("/config reply missing %q: %q", want, cfg)
		}
	}
	// /config must not leak secrets.
	if drv.requests != nil {
		t.Fatalf("commands must not invoke the driver: %+v", drv.requests)
	}
}

func TestUnknownCommandReportsAndSkipsDriver(t *testing.T) {
	st := newTestStore(t)
	drv := &fakeDriver{threadID: "t", reply: "x"}
	sink := &recordSink{}
	gw := New(drv, st, router.New(router.Config{MaxPerMinute: 100}), sink)

	if _, err := gw.Handle(context.Background(),
		router.InboundMessage{ChannelType: router.ChannelDM, FromUID: "u1", Text: "/frobnicate"}); err != nil {
		t.Fatal(err)
	}
	if len(drv.requests) != 0 {
		t.Fatalf("unknown command must not reach the driver")
	}
	r := sink.all()[0].text
	if !strings.HasPrefix(r, "Unknown command: /frobnicate") || !strings.Contains(r, "Available commands") {
		t.Fatalf("unknown-command reply wrong: %q", r)
	}
}

func TestNonCommandFallsThroughToDriver(t *testing.T) {
	st := newTestStore(t)
	drv := &fakeDriver{threadID: "t", reply: "real answer"}
	sink := &recordSink{}
	gw := New(drv, st, router.New(router.Config{MaxPerMinute: 100}), sink)

	if _, err := gw.Handle(context.Background(),
		router.InboundMessage{ChannelType: router.ChannelDM, FromUID: "u1", Text: "tell me /reset means"}); err != nil {
		t.Fatal(err)
	}
	if len(drv.requests) != 1 {
		t.Fatalf("non-command must reach the driver: %d", len(drv.requests))
	}
	if sink.all()[len(sink.all())-1].text != "real answer" {
		t.Fatalf("driver reply not delivered")
	}
}

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
	// Turns 2..N: rate-limited.
	for i := 0; i < 3; i++ {
		if d, _ := gw.Handle(context.Background(), mk()); d != router.RateLimited {
			t.Fatalf("turn%d want rate_limited, got %s", i+2, d)
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
