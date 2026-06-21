package octo

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/lml2468/xclaw/core/agent"
)

// fakeSendServer captures the `content` of every /v1/bot/sendMessage call (the
// path SendText posts to). SendTyping hits a different path and is ignored.
// Offline only — no live IM.
type fakeSendServer struct {
	mu       sync.Mutex
	contents []string
	srv      *httptest.Server
}

func newFakeSendServer() *fakeSendServer {
	f := &fakeSendServer{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			var body struct {
				Payload struct {
					Content string `json:"content"`
				} `json:"payload"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			f.mu.Lock()
			f.contents = append(f.contents, body.Payload.Content)
			f.mu.Unlock()
		}
		_ = json.NewEncoder(w).Encode(SendMessageResult{MessageID: "m", MessageSeq: 1})
	}))
	return f
}

func (f *fakeSendServer) notices() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.contents {
		if strings.HasPrefix(c, "🔧 Running") {
			out = append(out, c)
		}
	}
	return out
}

// newProgressConnector returns a connector wired to a fake send server with a
// reply target registered for sessionKey "s" so notices have somewhere to go.
func newProgressConnector(f *fakeSendServer, on bool) *Connector {
	c := NewConnector(NewRESTClient(f.srv.URL, func() string { return "t" }))
	c.setCtx(context.Background())
	c.SetToolProgress(on)
	c.targets["s"] = replyTarget{channelID: "chan", channelType: ChannelGroup}
	return c
}

func toolUse(name, params string) agent.AgentEvent {
	return agent.AgentEvent{Kind: agent.KindToolUse, ToolName: name, ToolParams: params}
}

// TestToolProgressEmitsNotices: with toolProgress on, each distinct KindToolUse
// produces one "🔧 Running <tool>(<params>)…" notice.
func TestToolProgressEmitsNotices(t *testing.T) {
	f := newFakeSendServer()
	defer f.srv.Close()
	c := newProgressConnector(f, true)

	c.OnEvent("s", agent.AgentEvent{Kind: agent.KindSessionStarted})
	c.OnEvent("s", toolUse("Bash", `{"command":"ls"}`))
	c.OnEvent("s", toolUse("Read", `{"file":"a.go"}`))

	got := f.notices()
	want := []string{
		"🔧 Running Bash({\"command\":\"ls\"})…",
		"🔧 Running Read({\"file\":\"a.go\"})…",
	}
	if !equalStrings(got, want) {
		t.Fatalf("notices = %#v, want %#v", got, want)
	}
}

// TestToolProgressCollapsesConsecutiveDuplicates: identical consecutive
// tool+params notices are sent once; a different tool in between un-collapses.
func TestToolProgressCollapsesConsecutiveDuplicates(t *testing.T) {
	f := newFakeSendServer()
	defer f.srv.Close()
	c := newProgressConnector(f, true)

	c.OnEvent("s", agent.AgentEvent{Kind: agent.KindSessionStarted})
	c.OnEvent("s", toolUse("Bash", `{"command":"ls"}`)) // sent
	c.OnEvent("s", toolUse("Bash", `{"command":"ls"}`)) // collapsed (dup)
	c.OnEvent("s", toolUse("Read", `{"file":"a"}`))     // sent
	c.OnEvent("s", toolUse("Bash", `{"command":"ls"}`)) // sent again (not consecutive)

	got := f.notices()
	want := []string{
		"🔧 Running Bash({\"command\":\"ls\"})…",
		"🔧 Running Read({\"file\":\"a\"})…",
		"🔧 Running Bash({\"command\":\"ls\"})…",
	}
	if !equalStrings(got, want) {
		t.Fatalf("notices = %#v, want %#v", got, want)
	}
}

// TestToolProgressCapsAtMax: no more than maxToolNotices notices per turn.
func TestToolProgressCapsAtMax(t *testing.T) {
	f := newFakeSendServer()
	defer f.srv.Close()
	c := newProgressConnector(f, true)

	c.OnEvent("s", agent.AgentEvent{Kind: agent.KindSessionStarted})
	for i := 0; i < maxToolNotices+5; i++ {
		// distinct params each time so dedup never collapses them
		c.OnEvent("s", toolUse("Bash", `{"i":`+itoa(i)+`}`))
	}
	if got := len(f.notices()); got != maxToolNotices {
		t.Fatalf("sent %d notices, want cap of %d", got, maxToolNotices)
	}
}

// TestToolProgressDisabledByDefault: with toolProgress off, no notices at all.
func TestToolProgressDisabledByDefault(t *testing.T) {
	f := newFakeSendServer()
	defer f.srv.Close()
	c := newProgressConnector(f, false)

	c.OnEvent("s", agent.AgentEvent{Kind: agent.KindSessionStarted})
	c.OnEvent("s", toolUse("Bash", `{"command":"ls"}`))
	c.OnEvent("s", toolUse("Read", `{"file":"a"}`))

	if got := f.notices(); len(got) != 0 {
		t.Fatalf("disabled connector sent notices: %#v", got)
	}
}

// TestToolProgressCounterResetsPerTurn: the cap and last-notice state reset on
// turn boundaries (KindTurnDone / next KindSessionStarted), so a second turn can
// send afresh and may repeat the previous turn's last tool.
func TestToolProgressCounterResetsPerTurn(t *testing.T) {
	f := newFakeSendServer()
	defer f.srv.Close()
	c := newProgressConnector(f, true)

	// Turn 1: fill to the cap.
	c.OnEvent("s", agent.AgentEvent{Kind: agent.KindSessionStarted})
	for i := 0; i < maxToolNotices; i++ {
		c.OnEvent("s", toolUse("Bash", `{"i":`+itoa(i)+`}`))
	}
	c.OnEvent("s", toolUse("Bash", `{"i":overflow}`)) // capped — dropped
	c.OnEvent("s", agent.AgentEvent{Kind: agent.KindTurnDone})

	if got := len(f.notices()); got != maxToolNotices {
		t.Fatalf("after turn 1: %d notices, want %d", got, maxToolNotices)
	}

	// Turn 2: counter reset, so a fresh notice sends — even repeating turn 1's
	// last tool label, since lastNotice was cleared.
	c.OnEvent("s", agent.AgentEvent{Kind: agent.KindSessionStarted})
	c.OnEvent("s", toolUse("Bash", `{"i":`+itoa(maxToolNotices-1)+`}`))

	if got := len(f.notices()); got != maxToolNotices+1 {
		t.Fatalf("after turn 2: %d notices, want %d", got, maxToolNotices+1)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func itoa(i int) string { return strconv.Itoa(i) }
