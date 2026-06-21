package main

import (
	"context"
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
	"github.com/lml2468/xclaw/core/cron"
	"github.com/lml2468/xclaw/core/gateway"
	"github.com/lml2468/xclaw/core/groupctx"
	"github.com/lml2468/xclaw/core/groupmd"
	"github.com/lml2468/xclaw/core/im/octo"
	"github.com/lml2468/xclaw/core/persona"
	"github.com/lml2468/xclaw/core/router"
	"github.com/lml2468/xclaw/core/store"
)

// Reaper cadence for a running bot. reapInterval is how often the sweep runs;
// routerReapIdle is how long a session's lock / rate-limit buckets must sit
// untouched before they're evicted (far longer than the rate-limit window so an
// evicted bucket is always fully refilled).
const (
	reapInterval   = time.Hour
	routerReapIdle = time.Hour
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
	secrets *secretStore  // in-memory tokens (seeded from cfg, updated by secret.inject)
	cron    *cron.Manager // nil when agent.cron is disabled

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
// every configured bot in its own isolated goroutine until SIGINT/SIGTERM (or,
// when exitWithParent is set, until the launching process dies).
func runConfigMode(path, controlSock string, exitWithParent bool, authStdin bool) {
	bots, err := config.Load(path)
	if err != nil {
		fatal("config: %v", err)
	}
	if len(bots) == 0 {
		fatal("config: no bots configured")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// When launched by the GUI, shut down if the app dies (even on a crash that
	// skips graceful teardown) so the daemon never lingers holding the socket.
	if exitWithParent {
		watchParentExit(stop)
	}

	started := time.Now()

	var srv *control.Server
	reg := newBotRegistry(nil)
	if controlSock != "" {
		srv = control.NewServer(nil)
		reg.srv = srv
		srv.SetHandler(makeMultiBotHandler(ctx, reg, started))
		configureBusAuth(srv, authStdin) // arm the capability-token gate before serving
		defer serveControlBus(srv, controlSock)()
	}

	fmt.Printf("xclawd — config mode: %d bot(s)\n", len(bots))

	var wg sync.WaitGroup
	for _, cfg := range bots {
		wg.Add(1)
		go func(cfg config.Resolved) {
			defer wg.Done()
			// Panic isolation: a panic in one bot's driver/connector must not crash
			// the whole daemon and take down every other bot — the promise is a
			// fully-isolated per-bot stack. Recover, mark the bot failed (so it shows
			// up in bots.list with the error instead of silently vanishing), and let
			// the others keep running.
			defer func() {
				if r := recover(); r != nil {
					fmt.Fprintf(os.Stderr, "[%s] panic: %v\n", cfg.BotID, r)
					registerFailedBot(reg, cfg, fmt.Sprintf("panic: %v", r))
				}
			}()
			if err := runBot(ctx, cfg, reg, srv); err != nil && ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "[%s] exited: %v\n", cfg.BotID, err)
				// A startup failure (store open, mkdir, …) returns before the bot
				// registers itself. Register a failed-status stub so the GUI/bots.list
				// shows it as down-with-error rather than missing entirely.
				registerFailedBot(reg, cfg, err.Error())
			}
		}(cfg)
	}
	wg.Wait()
}

// registerFailedBot records a bot that failed to start (or panicked) so it
// appears in bots.list with a down status + error, instead of silently missing.
// If the bot already registered itself, its existing runtime is marked failed.
func registerFailedBot(reg *botRegistry, cfg config.Resolved, errMsg string) {
	if b := reg.get(cfg.BotID); b != nil {
		b.setStatus(false, errMsg)
		return
	}
	b := &botRuntime{cfg: cfg, secrets: &secretStore{}}
	b.setStatus(false, errMsg)
	reg.add(b)
}

// runBot assembles and runs one bot's complete, isolated stack. When srv is set,
// agent events are also broadcast to the control bus tagged with the bot id, and
// the bot is registered for command routing + bots.list. Blocks until ctx done.
func runBot(ctx context.Context, cfg config.Resolved, reg *botRegistry, srv *control.Server) error {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return fmt.Errorf("bot %s: mkdir data: %w", cfg.BotID, err)
	}
	// Isolated per-bot CLAUDE_CONFIG_DIR (unless inheriting the operator's
	// ~/.claude). Created here so the agent's config root exists before it spawns.
	if cfg.ClaudeConfigDir != "" && !cfg.Agent.InheritUserConfig {
		if err := os.MkdirAll(cfg.ClaudeConfigDir, 0o700); err != nil {
			return fmt.Errorf("bot %s: mkdir claude config dir: %w", cfg.BotID, err)
		}
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "xclaw.db"))
	if err != nil {
		return fmt.Errorf("bot %s: store: %w", cfg.BotID, err)
	}
	defer st.Close()

	rt := router.New(router.Config{
		MaxPerMinute:      cfg.RateLimit.MaxPerMinute,
		MentionFreeGroups: cfg.MentionFreeGroups,
		KnownBotUids:      cfg.KnownBotUids,
		AllowedBotUids:    cfg.AllowedBotUids,
		BotBlocklist:      cfg.BotBlocklist,
	})

	// Periodic reaper: bound the router's per-session lock / rate-limit maps over
	// the daemon's lifetime (idle entries are evicted; sessions/messages/sandboxes
	// themselves are NOT expired — they persist). Runs once now, then on a ticker.
	reap := func() {
		rt.Reap(routerReapIdle)
	}
	reap()
	go func() {
		t := time.NewTicker(reapInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				reap()
			}
		}
	}()

	// Per-bot secret store: seed from the config file (the headless fallback),
	// then let secret.inject (from the GUI's Keychain) override at runtime.
	sec := &secretStore{}
	_ = sec.Set(secretKindOcto, cfg.OctoToken)
	_ = sec.Set(secretKindGateway, cfg.Agent.GatewayToken)

	// Phase 1 ships the claude driver only; the agent.Driver seam keeps adding
	// another (Codex, …) additive to the gateway.
	drv := agent.NewClaudeDriver("")
	// Resolve the gateway token lazily per turn so an injected token takes effect.
	drv.EnvFn = func() []string { return cfg.DriverEnvForOcto(sec.GatewayToken(), sec.OctoToken()) }

	if cfg.APIURL == "" {
		return fmt.Errorf("bot %s: config mode requires apiUrl", cfg.BotID)
	}
	// The Octo token is read lazily; it may arrive via secret.inject after start,
	// so an empty token here is allowed (the connector waits for it).
	connector := octo.NewConnector(octo.NewRESTClient(cfg.APIURL, sec.OctoToken))
	connector.SetToolProgress(cfg.Agent.ToolProgress)
	connector.SetMentionFreeGroups(cfg.MentionFreeGroups)

	// Persona clone (openclaw OBO): when onBehalfOf is configured, the connector
	// widens its trigger gate + routes replies as the grantor, and the gateway
	// injects the persona system prompt. A zero grantor is a no-op (regular bot).
	grantor := persona.Grantor{UID: cfg.OnBehalfOf.UID, Name: cfg.OnBehalfOf.Name}
	connector.SetPersona(grantor)

	// Sinks: the Octo connector (delivers replies to IM) + control bus (tagged
	// with this bot's id) when a GUI is attached.
	sinks := multiSink{connector}
	if srv != nil {
		sinks = append(sinks, control.NewBotEventSink(srv, cfg.BotID))
	}

	gw := gateway.New(drv, st, rt, sinks).
		WithGroupContext(groupctx.New(cfg.Context.MaxContextChars)).
		WithGroupMD(groupmd.New(cfg.GroupConfigDir)).
		WithGroupBackfill(connector.BotUID, connector.BackfillFetch).
		WithSystemPrompt(cfg.SystemPrompt).
		WithPersona(grantor, cfg.OnBehalfOf.PersonaPrompt).
		WithModel(cfg.Agent.Model).
		WithCommandInfo(cfg.RateLimit.MaxPerMinute, cfg.Context.MaxContextChars).
		WithSandbox(cfg.CwdBase, cfg.MemoryBase).
		WithMediaAuth(connector.MediaAuth())
	connector.SetGateway(gw)

	rtBot := &botRuntime{cfg: cfg, gateway: gw, store: st, secrets: sec}
	if reg != nil {
		reg.add(rtBot)
	}
	connector.OnStatus(func(connected bool, lastErr string) {
		rtBot.setStatus(connected, lastErr)
		if srv != nil {
			srv.Broadcast("bot.status", rtBot.info())
		}
	})

	// Cron scheduler (#115): when enabled, load <dataDir>/cron.json and fire due
	// tasks through the gateway as synthetic CronFire messages. The owner uid that
	// gates create/delete is resolved from the bot registration (owner_uid).
	if cfg.Agent.Cron {
		cm := cron.NewManager(cron.NewStore(filepath.Join(cfg.DataDir, "cron.json")), "", nil)
		cm.SetLabel(fmt.Sprintf("[%s] ", cfg.BotID))
		cm.OnFire(func(f cron.Fire) {
			fireCronTask(ctx, gw, connector, f.Task)
		})
		connector.OnOwner(func(ownerUID string) { cm.SetOwnerUID(ownerUID) })
		rtBot.cron = cm
		cm.Start()
		defer cm.Stop()
		fmt.Printf("[%s] cron scheduler armed (tick %s)\n", cfg.BotID, cron.CronTickInterval)
	}

	fmt.Printf("[%s] started — driver=%s api=%s data=%s\n",
		cfg.BotID, drv.Name(), cfg.APIURL, cfg.DataDir)

	return connector.Run(ctx)
}

// fireCronTask delivers one due cron task through the full turn pipeline. It
// binds the connector's reply target to the task's session, then hands the
// task's stored prompt to the gateway as a synthetic CronFire message (which the
// router accepts past the group @mention gate and the rate limit). Best-effort:
// a failed fire is logged, never propagated, so the scheduler loop survives.
func fireCronTask(ctx context.Context, gw *gateway.Gateway, connector *octo.Connector, t cron.Task) {
	chType := router.ChannelDM
	octoType := octo.ChannelDM
	if t.ChannelType == cron.ChannelKind(router.ChannelGroup) {
		chType = router.ChannelGroup
		octoType = octo.ChannelGroup
	}
	inbound := router.InboundMessage{
		FromUID:     t.FromUID,
		FromName:    t.FromName,
		ChannelID:   t.ChannelID,
		ChannelType: chType,
		Text:        t.Prompt,
		CronFire:    true,
	}
	key, err := inbound.SessionKey()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cron: task %s has unroutable coords: %v\n", t.ID, err)
		return
	}
	connector.RegisterReplyTarget(key, t.ChannelID, octoType)
	if _, err := gw.Handle(ctx, inbound); err != nil {
		fmt.Fprintf(os.Stderr, "cron: task %s fire failed: %v\n", t.ID, err)
	}
}

// makeMultiBotHandler routes control-bus commands by botId across the registry.
// All command logic lives in the shared makeHandler; this only supplies the
// multi-bot resolution + roster + event broadcast.
func makeMultiBotHandler(ctx context.Context, reg *botRegistry, started time.Time) control.CommandHandler {
	return makeHandler(ctx, handlerDeps{
		started:  started,
		botCount: func() int { return len(reg.list()) },
		list:     reg.list,
		resolve: func(botID string) (*botTarget, error) {
			bot := reg.get(botID)
			if bot == nil {
				return nil, fmt.Errorf("unknown bot %q", botID)
			}
			return &botTarget{id: bot.cfg.BotID, gateway: bot.gateway, store: bot.store, secrets: bot.secrets, cron: bot.cron}, nil
		},
		broadcast: func(eventType string, body any) {
			if reg.srv != nil {
				reg.srv.Broadcast(eventType, body)
			}
		},
	})
}
