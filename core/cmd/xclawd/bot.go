package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/lml2468/xclaw/core/agent"
	"github.com/lml2468/xclaw/core/config"
	"github.com/lml2468/xclaw/core/control"
	"github.com/lml2468/xclaw/core/gateway"
	"github.com/lml2468/xclaw/core/groupctx"
	"github.com/lml2468/xclaw/core/im/octo"
	"github.com/lml2468/xclaw/core/router"
	"github.com/lml2468/xclaw/core/sandbox"
	"github.com/lml2468/xclaw/core/store"
)

// configFlagSet reports whether -config was passed on the command line (so an
// empty value still selects config mode with the default path).
func configFlagSet() bool {
	set := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			set = true
		}
	})
	return set
}

// botRuntime is one running bot's externally-reachable handles: its gateway (for
// routing control-bus commands) and live status.
type botRuntime struct {
	cfg     config.Resolved
	gateway *gateway.Gateway
	store   *store.Store

	mu        sync.Mutex
	connected bool
	lastErr   string
}

func (b *botRuntime) info() control.BotInfo {
	b.mu.Lock()
	defer b.mu.Unlock()
	return control.BotInfo{ID: b.cfg.BotID, Connected: b.connected, LastError: b.lastErr}
}

func (b *botRuntime) setStatus(connected bool, lastErr string) {
	b.mu.Lock()
	b.connected, b.lastErr = connected, lastErr
	b.mu.Unlock()
}

// botRegistry tracks all running bots for control-bus routing and bots.list.
type botRegistry struct {
	mu   sync.RWMutex
	bots map[string]*botRuntime
	srv  *control.Server // for broadcasting bot.status changes
}

func newBotRegistry(srv *control.Server) *botRegistry {
	return &botRegistry{bots: map[string]*botRuntime{}, srv: srv}
}

func (r *botRegistry) add(b *botRuntime) {
	r.mu.Lock()
	r.bots[b.cfg.BotID] = b
	r.mu.Unlock()
}

func (r *botRegistry) get(id string) *botRuntime {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if id == "" && len(r.bots) == 1 {
		for _, b := range r.bots { // single-bot convenience
			return b
		}
	}
	return r.bots[id]
}

func (r *botRegistry) list() []control.BotInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]control.BotInfo, 0, len(r.bots))
	for _, b := range r.bots {
		out = append(out, b.info())
	}
	return out
}

// runConfigMode loads the single-file config, serves the control bus, and runs
// every configured bot in its own isolated goroutine until SIGINT/SIGTERM.
func runConfigMode(path, controlSock string) {
	bots, err := config.Load(path)
	if err != nil {
		fatal("config: %v", err)
	}
	if len(bots) == 0 {
		fatal("config: no bots configured")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	started := time.Now()

	var srv *control.Server
	reg := newBotRegistry(nil)
	if controlSock != "" {
		srv = control.NewServer(nil)
		reg.srv = srv
		srv.SetHandler(makeMultiBotHandler(reg, started))
		ln := mustListenUnix(controlSock)
		defer ln.Close()
		defer os.Remove(controlSock)
		go func() {
			if err := srv.Serve(ln); err != nil {
				fmt.Fprintf(os.Stderr, "control serve: %v\n", err)
			}
		}()
		fmt.Printf("control bus listening on %s\n", controlSock)
	}

	fmt.Printf("xclawd — config mode: %d bot(s)\n", len(bots))

	var wg sync.WaitGroup
	for _, cfg := range bots {
		wg.Add(1)
		go func(cfg config.Resolved) {
			defer wg.Done()
			if err := runBot(ctx, cfg, reg, srv); err != nil && ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "[%s] exited: %v\n", cfg.BotID, err)
			}
		}(cfg)
	}
	wg.Wait()
}

// runBot assembles and runs one bot's complete, isolated stack. When srv is set,
// agent events are also broadcast to the control bus tagged with the bot id, and
// the bot is registered for command routing + bots.list. Blocks until ctx done.
func runBot(ctx context.Context, cfg config.Resolved, reg *botRegistry, srv *control.Server) error {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return fmt.Errorf("bot %s: mkdir data: %w", cfg.BotID, err)
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "xclaw.db"))
	if err != nil {
		return fmt.Errorf("bot %s: store: %w", cfg.BotID, err)
	}
	defer st.Close()
	if n, _ := st.CleanupExpired(store.DefaultTTL); n > 0 {
		fmt.Fprintf(os.Stderr, "[%s] swept %d expired session(s)\n", cfg.BotID, n)
	}
	// Reclaim per-session cwd sandboxes idle past the TTL (memory lives outside, untouched).
	sandbox.CleanupExpiredCwds(cfg.CwdBase, sandbox.DefaultCwdTTL)

	// Phase 1 ships the claude driver only; the agent.Driver seam keeps adding
	// another (Codex, …) additive to the gateway.
	drv := agent.NewClaudeDriver("")
	drv.Env = cfg.DriverEnv()

	rt := router.New(router.Config{MaxPerMinute: cfg.RateLimit.MaxPerMinute})

	if cfg.APIURL == "" || cfg.OctoToken == "" {
		return fmt.Errorf("bot %s: config mode requires apiUrl + octoToken", cfg.BotID)
	}
	connector := octo.NewConnector(octo.NewRESTClient(cfg.APIURL, cfg.OctoToken))

	// Sinks: the Octo connector (delivers replies to IM) + control bus (tagged
	// with this bot's id) when a GUI is attached.
	sinks := multiSink{connector}
	if srv != nil {
		sinks = append(sinks, control.NewBotEventSink(srv, cfg.BotID))
	}

	gw := gateway.New(drv, st, rt, sinks).
		WithGroupContext(groupctx.New(cfg.Context.MaxContextChars)).
		WithSystemPrompt(cfg.SystemPrompt).
		WithModel(cfg.Agent.Model).
		WithSandbox(cfg.CwdBase, cfg.MemoryBase, cfg.SkillsDir, cfg.GlobalSkillsDir)
	connector.SetGateway(gw)

	rtBot := &botRuntime{cfg: cfg, gateway: gw, store: st}
	if reg != nil {
		reg.add(rtBot)
	}
	connector.OnStatus(func(connected bool, lastErr string) {
		rtBot.setStatus(connected, lastErr)
		if srv != nil {
			srv.Broadcast("bot.status", rtBot.info())
		}
	})

	fmt.Printf("[%s] started — driver=%s api=%s data=%s\n",
		cfg.BotID, drv.Name(), cfg.APIURL, cfg.DataDir)

	return connector.Run(ctx)
}

// makeMultiBotHandler routes control-bus commands by botId across the registry.
func makeMultiBotHandler(reg *botRegistry, started time.Time) control.CommandHandler {
	return func(cmdType string, body json.RawMessage) (any, error) {
		switch cmdType {
		case "health":
			return control.HealthBody{
				Uptime: int64(time.Since(started).Seconds()),
				Bots:   len(reg.list()),
			}, nil

		case "bots.list":
			return reg.list(), nil

		case "session.send":
			var b control.SessionSendBody
			if err := json.Unmarshal(body, &b); err != nil {
				return nil, err
			}
			bot := reg.get(b.BotID)
			if bot == nil {
				return nil, fmt.Errorf("unknown bot %q", b.BotID)
			}
			if b.UID == "" {
				return nil, fmt.Errorf("uid required")
			}
			go func() {
				_, _ = bot.gateway.Handle(context.Background(), router.InboundMessage{
					ChannelType: router.ChannelDM, FromUID: b.UID, FromName: b.UID, Text: b.Text,
				})
			}()
			return control.OKBody{OK: true}, nil

		case "session.history":
			var b control.SessionHistoryBody
			if err := json.Unmarshal(body, &b); err != nil {
				return nil, err
			}
			bot := reg.get(b.BotID)
			if bot == nil {
				return nil, fmt.Errorf("unknown bot %q", b.BotID)
			}
			limit := b.Limit
			if limit <= 0 {
				limit = 40
			}
			msgs, err := bot.store.RecentMessages(b.SessionKey, limit)
			if err != nil {
				return nil, err
			}
			out := make([]control.HistoryMessage, 0, len(msgs))
			for _, m := range msgs {
				out = append(out, control.HistoryMessage{Role: string(m.Role), Content: m.Content, TS: m.Timestamp})
			}
			return out, nil

		case "session.reset":
			var b control.SessionSendBody
			if err := json.Unmarshal(body, &b); err != nil {
				return nil, err
			}
			bot := reg.get(b.BotID)
			if bot == nil {
				return nil, fmt.Errorf("unknown bot %q", b.BotID)
			}
			_ = bot.store.ClearResume(b.UID)
			return control.OKBody{OK: true}, nil

		default:
			return nil, fmt.Errorf("unknown command %q", cmdType)
		}
	}
}
