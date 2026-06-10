// Command xclawd is the XClaw gateway daemon.
//
// It wires the full pipeline — store + router + gateway + agent driver — and
// drives it from an inbound source. This headless MVP ships a REPL inbound
// source (stdin) so you can talk to an agent end-to-end:
//
//	xclawd                       # claude driver, interactive REPL on stdin
//	xclawd -driver codex         # codex app-server driver
//	echo "hello" | xclawd        # one-shot piped input
//
// Each line of stdin becomes an inbound DM; the gateway routes it (per-session
// lock, rate limit), drives the agent, streams events to stdout, and persists
// the assistant reply + resume id for multi-turn continuity.
//
// A `/reset` line clears the current session's resume mapping.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lml2468/xclaw/core/agent"
	"github.com/lml2468/xclaw/core/gateway"
	"github.com/lml2468/xclaw/core/router"
	"github.com/lml2468/xclaw/core/store"
)

func main() {
	var (
		driverName = flag.String("driver", "claude", "agent driver: claude | codex")
		bin        = flag.String("bin", "", "agent executable (default: driver name)")
		fromUID    = flag.String("uid", "repl-user", "synthetic from_uid for REPL inbound (DM session key)")
		dbPath     = flag.String("db", filepath.Join(os.TempDir(), "xclawd.db"), "sqlite path")
		maxPerMin  = flag.Int("rate", 30, "max messages per minute per session")
	)
	flag.Parse()

	st, err := store.Open(*dbPath)
	if err != nil {
		fatal("store open: %v", err)
	}
	defer st.Close()
	if n, err := st.CleanupExpired(store.DefaultTTL); err == nil && n > 0 {
		fmt.Fprintf(os.Stderr, "swept %d expired session(s)\n", n)
	}

	drv, err := makeDriver(*driverName, *bin)
	if err != nil {
		fatal("%v", err)
	}

	sink := &stdoutSink{}
	gw := gateway.New(drv, st, router.New(router.Config{MaxPerMinute: *maxPerMin}), sink)

	fmt.Printf("xclawd — driver=%s caps=%+v\n", drv.Name(), drv.Capabilities())
	fmt.Printf("db=%s  session=dm:%s\n", *dbPath, *fromUID)
	fmt.Println("type a message and press enter; /reset clears the session; Ctrl-D to exit")

	runREPL(context.Background(), gw, st, *fromUID)
}

func makeDriver(name, bin string) (agent.Driver, error) {
	switch name {
	case "claude":
		return agent.NewClaudeDriver(bin), nil
	case "codex":
		return agent.NewCodexDriver(bin), nil
	default:
		return nil, fmt.Errorf("unknown driver %q (claude|codex)", name)
	}
}

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
		if ev.Usage != nil {
			fmt.Printf("  [done]    in=%d out=%d tokens\n", ev.Usage.InputTokens, ev.Usage.OutputTokens)
		} else {
			fmt.Printf("  [done]\n")
		}
	case agent.KindError:
		tag := "ERR"
		if ev.Recoverable {
			tag = "retry"
		}
		fmt.Printf("  [%s]   %s\n", tag, oneLine(ev.Err))
	case agent.KindSystem:
		fmt.Printf("  [sys]     %s\n", oneLine(ev.Text))
	}
}

func (s *stdoutSink) OnReply(sessionKey string, text string) {
	if text != "" {
		fmt.Printf("\n💬 %s\n", text)
	}
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
