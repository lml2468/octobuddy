// Command xclawd is the XClaw gateway daemon.
//
// It wires the full pipeline — store + router + gateway + agent driver — and
// drives it from an inbound source. Two front ends:
//
//	xclawd                              # REPL on stdin (claude driver)
//	xclawd -control /tmp/xclaw.sock    # serve the control bus (for the GUI app)
//
// With -control it listens on a Unix socket speaking the proto/ NDJSON protocol
// so the Swift macOS app (or any client) can send commands and receive the live
// event stream. The REPL and the control bus can run together.
//
// Each inbound becomes a DM; the gateway routes it (per-session lock, rate
// limit), drives the agent, streams events to every sink (stdout + control bus),
// and persists the assistant reply + resume id for multi-turn continuity.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lml2468/xclaw/core/agent"
	"github.com/lml2468/xclaw/core/control"
	"github.com/lml2468/xclaw/core/gateway"
	"github.com/lml2468/xclaw/core/groupctx"
	"github.com/lml2468/xclaw/core/im/octo"
	"github.com/lml2468/xclaw/core/router"
	"github.com/lml2468/xclaw/core/store"
)

func main() {
	var (
		claudeBin   = flag.String("claude-bin", "", "claude executable (default: 'claude' on PATH)")
		fromUID     = flag.String("uid", "repl-user", "synthetic from_uid for REPL inbound (DM session key)")
		dbPath      = flag.String("db", filepath.Join(os.TempDir(), "xclawd.db"), "sqlite path")
		maxPerMin   = flag.Int("rate", 30, "max messages per minute per session")
		controlSock = flag.String("control", "", "serve the control bus on this Unix socket path (enables GUI clients)")
		noREPL      = flag.Bool("no-repl", false, "disable the stdin REPL (control-bus only)")
		octoAPI     = flag.String("octo-api", "", "Octo API base URL (enables the Octo IM connector)")
		octoToken   = flag.String("octo-token", "", "Octo bot token (bf_*); or set XCLAW_OCTO_TOKEN")
		configPath  = flag.String("config", "", "load ~/.xclaw/config.json (or given path) and run all configured bots")
		exitParent  = flag.Bool("exit-with-parent", false, "exit when the parent process dies (set by the GUI so the daemon never outlives the app)")
	)
	flag.Parse()

	// Config mode: load the single ~/.xclaw/config.json and run every bot in its
	// own isolated stack. Mutually exclusive with the single-bot flag front ends.
	// `-config` with no value uses the default ~/.xclaw/config.json. `-control`
	// additionally serves the bus so a GUI can manage all bots.
	if configFlagSet() {
		runConfigMode(*configPath, *controlSock, *exitParent)
		return
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		fatal("store open: %v", err)
	}
	defer st.Close()
	if n, err := st.CleanupExpired(store.DefaultTTL); err == nil && n > 0 {
		fmt.Fprintf(os.Stderr, "swept %d expired session(s)\n", n)
	}

	drv := agent.NewClaudeDriver(*claudeBin)

	started := time.Now()

	// Run context: cancellable so an in-flight control-bus turn (session.send)
	// and the IM connector shut down on exit instead of running detached.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Lone secret store for the single-bot flag path: seeded from the flag/env
	// token, updatable via secret.inject over the control bus. The driver reads
	// any injected gateway token lazily per turn.
	sec := &secretStore{}
	drv.EnvFn = func() []string {
		if t := sec.GatewayToken(); t != "" {
			return []string{"ANTHROPIC_AUTH_TOKEN=" + t}
		}
		return nil
	}

	// Sinks fan out: stdout always, control bus + Octo connector when enabled.
	sinks := multiSink{&stdoutSink{}}
	rt := router.New(router.Config{MaxPerMinute: *maxPerMin})

	var srv *control.Server
	if *controlSock != "" {
		srv = control.NewServer(nil) // handler installed after gw exists
		sinks = append(sinks, control.NewEventSink(srv))
	}

	// Octo IM connector: it is both an inbound source (feeds the gateway) and a
	// gateway.Sink (delivers replies via REST), so build it before the gateway.
	token := *octoToken
	if token == "" {
		token = os.Getenv("XCLAW_OCTO_TOKEN")
	}
	var connector *octo.Connector
	if *octoAPI != "" {
		if token == "" {
			fatal("-octo-api set but no token (use -octo-token or XCLAW_OCTO_TOKEN)")
		}
		_ = sec.Set(secretKindOcto, token)
		connector = octo.NewConnector(octo.NewRESTClient(*octoAPI, sec.OctoToken))
		sinks = append(sinks, connector)
	}

	gw := gateway.New(drv, st, rt, sinks)
	// Group-context injection is useful whenever an IM front end is active.
	if connector != nil {
		gw = gw.WithGroupContext(groupctx.New(6000))
	}

	if srv != nil {
		srv.SetHandler(makeCommandHandler(ctx, gw, st, drv, sec, srv, started))
		ln := mustListenUnix(*controlSock)
		defer ln.Close()
		defer os.Remove(*controlSock)
		go func() {
			if err := srv.Serve(ln); err != nil {
				fmt.Fprintf(os.Stderr, "control serve: %v\n", err)
			}
		}()
		fmt.Printf("control bus listening on %s\n", *controlSock)
	}

	if connector != nil {
		connector.SetGateway(gw)
		go func() {
			if err := connector.Run(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "octo connector: %v\n", err)
			}
		}()
		fmt.Printf("octo connector started (api=%s)\n", *octoAPI)
	}

	fmt.Printf("xclawd — driver=%s caps=%+v\n", drv.Name(), drv.Capabilities())
	fmt.Printf("db=%s  session=dm:%s\n", *dbPath, *fromUID)

	if *noREPL || connector != nil {
		if *exitParent {
			watchParentExit(func() {
				fmt.Fprintln(os.Stderr, "parent exited; shutting down")
				os.Exit(0)
			})
		}
		fmt.Println("running (control bus / IM connector); press Ctrl-C to exit")
		select {} // block forever
	}

	fmt.Println("type a message and press enter; /reset clears the session; Ctrl-D to exit")

	runREPL(context.Background(), gw, st, *fromUID)
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

func mustListenUnix(path string) net.Listener {
	_ = os.Remove(path) // clear a stale socket
	ln, err := net.Listen("unix", path)
	if err != nil {
		fatal("listen %s: %v", path, err)
	}
	return ln
}

// makeCommandHandler builds the single-bot control-bus dispatcher. All command
// logic lives in the shared makeHandler; this only supplies the fixed target,
// the synthetic one-bot roster, and the broadcast hook.
func makeCommandHandler(ctx context.Context, gw *gateway.Gateway, st *store.Store, drv agent.Driver, sec *secretStore, srv *control.Server, started time.Time) control.CommandHandler {
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
	})
}
