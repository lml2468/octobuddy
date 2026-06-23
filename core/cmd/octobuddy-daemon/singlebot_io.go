package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lml2468/octobuddy/core/agent"
	"github.com/lml2468/octobuddy/core/control"
	"github.com/lml2468/octobuddy/core/gateway"
	"github.com/lml2468/octobuddy/core/router"
	"github.com/lml2468/octobuddy/core/store"
)

// runREPL reads stdin lines and feeds each as an inbound DM through the gateway.
func runREPL(ctx context.Context, gw *gateway.Gateway, st *store.Store, fromUID string) {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for {
		fmt.Print("\n> ")
		if !sc.Scan() {
			break
		}
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		if text == "/reset" {
			_ = st.ClearResume(fromUID)
			fmt.Println("(session reset)")
			continue
		}

		d, err := gw.Handle(ctx, router.InboundMessage{
			ChannelType: router.ChannelDM,
			FromUID:     fromUID,
			FromName:    fromUID,
			Text:        text,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "handle error: %v\n", err)
			continue
		}
		if d != router.Accepted {
			fmt.Printf("(dropped: %s)\n", d)
		}
	}
}

// stdoutSink renders the live event stream and the final reply to the terminal.
type stdoutSink struct{}

func (s *stdoutSink) OnEvent(sessionKey string, ev agent.AgentEvent) {
	switch ev.Kind {
	case agent.KindSessionStarted:
		fmt.Printf("  [session] %s\n", ev.SessionID)
	case agent.KindTextDelta:
		fmt.Printf("  [text]    %s\n", oneLine(ev.Text))
	case agent.KindThinking:
		fmt.Printf("  [think]   %s\n", oneLine(ev.Text))
	case agent.KindToolUse:
		fmt.Printf("  [tool]    🔧 %s(%s)\n", ev.ToolName, ev.ToolParams)
	case agent.KindToolResult:
		fmt.Printf("  [result]  (tool returned)\n")
	case agent.KindTurnDone:
		fmt.Println(turnDoneLine(ev.Usage))
	case agent.KindError:
		fmt.Printf("  [%s]   %s\n", eventErrorTag(ev), oneLine(ev.Err))
	case agent.KindSystem:
		fmt.Printf("  [sys]     %s\n", oneLine(ev.Text))
	}
}

func turnDoneLine(usage *agent.TokenUsage) string {
	if usage == nil {
		return "  [done]"
	}
	line := fmt.Sprintf("  [done]    in=%d out=%d tokens", usage.InputTokens, usage.OutputTokens)
	if usage.CachedInputTokens > 0 {
		line += fmt.Sprintf(" (cached=%d)", usage.CachedInputTokens)
	}
	if usage.CostUSD > 0 {
		line += fmt.Sprintf(" cost=$%.4f", usage.CostUSD)
	}
	return line
}

func eventErrorTag(ev agent.AgentEvent) string {
	if ev.Recoverable {
		return "retry"
	}
	return "ERR"
}

func (s *stdoutSink) OnReply(sessionKey string, text string) {
	if text != "" {
		fmt.Printf("\n💬 %s\n", text)
	}
}

// OnUserMessage is a no-op for the REPL/stdout sink — the user just typed
// the line into stdin and sees it in their own terminal already.
func (s *stdoutSink) OnUserMessage(string, router.InboundMessage) {}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

// multiSink fans gateway events out to several sinks (stdout + control bus).
type multiSink []gateway.Sink

func (m multiSink) OnEvent(key string, ev agent.AgentEvent) {
	for _, s := range m {
		s.OnEvent(key, ev)
	}
}
func (m multiSink) OnReply(key, text string) {
	for _, s := range m {
		s.OnReply(key, text)
	}
}
func (m multiSink) OnUserMessage(key string, msg router.InboundMessage) {
	for _, s := range m {
		s.OnUserMessage(key, msg)
	}
}

// makeCommandHandler builds the single-bot control-bus dispatcher. All command
// logic lives in the shared makeHandler; this only supplies the fixed target,
// the synthetic one-bot roster, and the broadcast hook.
//
// Returns the handler AND the shared target so the caller can block on
// target.turnsWG before closing the store on shutdown (turns in flight under
// session.send would otherwise race the deferred st.Close).
func makeCommandHandler(ctx context.Context, gw *gateway.Gateway, st *store.Store, drv agent.Driver, sec *secretStore, srv *control.Server, started time.Time) (control.CommandHandler, *botTarget) {
	target := &botTarget{gateway: gw, store: st, secrets: sec}
	var broadcast func(string, any)
	if srv != nil {
		broadcast = srv.Broadcast
	}
	return makeHandler(ctx, handlerDeps{
		started:  started,
		driver:   drv.Name(),
		botCount: func() int { return 1 },
		// Single-bot mode has one implicit bot so a GUI client's bots.list
		// bootstrap works the same as in multi-bot mode (proto parity).
		list: func() []control.BotInfo {
			return []control.BotInfo{{ID: "default", Connected: true}}
		},
		resolve:   func(string) (*botTarget, error) { return target, nil },
		broadcast: broadcast,
	}), target
}
