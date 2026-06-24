package gateway

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/lml2468/octobuddy/core/agent"
	"github.com/lml2468/octobuddy/core/router"
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

func (s *recordSink) OnEvent(string, agent.AgentEvent)            {}
func (s *recordSink) OnUserMessage(string, router.InboundMessage) {}
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

// erroringDriver emits a session id and some partial text, then a terminal
// (non-recoverable) error followed by turn_done — mirroring a result is_error
// turn (e.g. max_turns) that still streamed a partial answer.
type erroringDriver struct {
	sessionID   string
	partialText string
}

func (d *erroringDriver) Name() string                     { return "erroring" }
func (d *erroringDriver) Capabilities() agent.Capabilities { return agent.Capabilities{Resume: true} }
func (d *erroringDriver) Query(ctx context.Context, _ agent.Request) (<-chan agent.AgentEvent, error) {
	ch := make(chan agent.AgentEvent, 8)
	go func() {
		defer close(ch)
		ch <- agent.AgentEvent{Kind: agent.KindSessionStarted, SessionID: d.sessionID}
		ch <- agent.AgentEvent{Kind: agent.KindTextDelta, Text: d.partialText}
		ch <- agent.AgentEvent{Kind: agent.KindError, Err: "result error (subtype=error_max_turns): hit max turns"}
		ch <- agent.AgentEvent{Kind: agent.KindTurnDone}
	}()
	return ch, nil
}

// recoverableErroringDriver streams a full successful reply but interleaves a
// RECOVERABLE error (e.g. a stderr node warning, or a non-zero process exit that
// follows a completed turn). The turn succeeded, so the reply must persist and
// the resume id must advance — the recoverable error is informational only.
type recoverableErroringDriver struct {
	sessionID string
	reply     string
}

func (d *recoverableErroringDriver) Name() string { return "recoverable" }
func (d *recoverableErroringDriver) Capabilities() agent.Capabilities {
	return agent.Capabilities{Resume: true}
}
func (d *recoverableErroringDriver) Query(ctx context.Context, _ agent.Request) (<-chan agent.AgentEvent, error) {
	ch := make(chan agent.AgentEvent, 8)
	go func() {
		defer close(ch)
		ch <- agent.AgentEvent{Kind: agent.KindSessionStarted, SessionID: d.sessionID}
		ch <- agent.AgentEvent{Kind: agent.KindTextDelta, Text: d.reply}
		ch <- agent.AgentEvent{Kind: agent.KindTurnDone}
		// Arrives AFTER the completed turn (mirrors claude.go marking a post-result
		// non-zero exit recoverable). Must not abort the turn.
		ch <- agent.AgentEvent{Kind: agent.KindError, Err: "claude exited: exit status 1", Recoverable: true}
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
	if got, _ := st.Resume("u1", "fake"); got != "thr-1" {
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
	if got, _ := st.Resume("u1", "fake"); got != "" {
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
