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
	"github.com/lml2468/xclaw/core/control/wire"
	"github.com/lml2468/xclaw/core/cron"
	"github.com/lml2468/xclaw/core/gateway"
	"github.com/lml2468/xclaw/core/groupctx"
	"github.com/lml2468/xclaw/core/groupmd"
	"github.com/lml2468/xclaw/core/im/octo"
	"github.com/lml2468/xclaw/core/persona"
	"github.com/lml2468/xclaw/core/router"
	"github.com/lml2468/xclaw/core/safepath"
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
	// connector is the IM-edge for this bot. The control plane reaches into it
	// for name resolution (sidebar channel titles, bubble sender labels — see
	// summariesFromSessions in control.go); the gateway holds its own ref via
	// Sink/MediaAuth/BackfillFetch wiring.
	connector *octo.Connector

	// target is the per-bot control-handler target, owned for the runtime's
	// lifetime so the embedded turnsWG is shared across all session.send
	// goroutines for this bot (a fresh botTarget per resolve would give
	// each goroutine its own WaitGroup and defeat the shutdown barrier).
	target *botTarget

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
	// An empty roster is a valid first-run state — the GUI writes config.json
	// before the user adds any bots, and adds them later via the control bus
	// (SaveConfig → RestartCore). The daemon stays up and serves an empty
	// bots.list rather than dying and leaving the GUI without a peer.

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
	var stopControl func()
	if controlSock != "" {
		srv = control.NewServer(nil)
		reg.srv = srv
		srv.SetHandler(makeMultiBotHandler(ctx, reg, started))
		configureBusAuth(srv, authStdin) // arm the capability-token gate before serving
		cleanup := serveControlBus(srv, controlSock)
		var once sync.Once
		stopControl = func() { once.Do(cleanup) }
		defer stopControl() // belt-and-suspenders: if the ctx-watcher misses it
		// Close the control listener as soon as shutdown begins so no late
		// session.send can land and race a per-bot target.turnsWG.Add(1)
		// AFTER that bot's turnsWG.Wait has already returned 0 — which both
		// violates the WaitGroup contract and would dispatch to a bot whose
		// store has already been closed by runBot's deferred st.Close.
		go func() {
			<-ctx.Done()
			stopControl()
		}()
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
	// First-run state (no bots yet): wg.Wait would return immediately and the
	// daemon would exit before the GUI ever connects. Block on ctx so the
	// control bus stays up and SaveConfig → RestartCore can populate the
	// roster. The signal-driven ctx.Done path (SIGINT/SIGTERM/exit-with-parent)
	// is the same one that ends a populated run after wg.Wait, so shutdown
	// semantics match either branch.
	if len(bots) == 0 {
		<-ctx.Done()
		return
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
	// SafeMkdirAllAbs walks the parent chain via dirfd, refusing any
	// symlinked intermediate with ErrSymlink. An agent (Bash + bypass)
	// in any existing bot's cwd could otherwise plant
	// `~/.xclaw/<newbotID>` as a symlink to `~/.ssh/` BEFORE the operator
	// adds the new bot; a bare MkdirAll would silently follow it, and
	// store.Open would then create xclaw.db/.wal/.shm under .ssh. When
	// DataDir is outside $HOME (operator-supplied absolute path)
	// SafeMkdirAllAbs falls back to bare MkdirAll — that boundary is
	// operator-trusted.
	if err := safepath.SafeMkdirAllAbs(cfg.DataDir, 0o755); err != nil {
		return fmt.Errorf("bot %s: mkdir data: %w", cfg.BotID, err)
	}
	// Isolated per-bot CLAUDE_CONFIG_DIR (unless inheriting the operator's
	// ~/.claude). Created here so the agent's config root exists before it spawns.
	if cfg.ClaudeConfigDir != "" && !cfg.Agent.InheritUserConfig {
		if err := safepath.SafeMkdirAllAbs(cfg.ClaudeConfigDir, 0o700); err != nil {
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
	// then let secret.inject (from the GUI's secret backend) override at runtime.
	sec := &secretStore{}
	_ = sec.Set(wire.SecretKindOcto, cfg.OctoToken)
	_ = sec.Set(wire.SecretKindGateway, cfg.Agent.GatewayToken)

	// Phase 1 ships the claude driver only; the agent.Driver seam keeps adding
	// another (Codex, …) additive to the gateway.
	drv := agent.NewClaudeDriver("")
	// Resolve the gateway token lazily per turn so an injected token takes effect.
	drv.EnvFn = func() []string { return cfg.DriverEnv(sec.GatewayToken(), sec.OctoToken(), sec.Secret) }

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
		WithDispatchTimeout(time.Duration(cfg.Agent.DispatchTimeoutSec) * time.Second).
		WithMediaAuth(connector.MediaAuth())
	if srv != nil {
		gw = gw.WithSessionTouchNotifier(sessionTouchBroadcaster(srv, cfg.BotID, st, connector))
	}
	connector.SetGateway(gw)

	// Eager-init the per-bot control-handler target so its embedded turnsWG is
	// pinned for runBot's shutdown barrier (see the longer note before rtBot
	// below). Declared up here so the cron fire closure can capture it for
	// the Console-target branch — Console fires bypass the IM connector and
	// go straight to gw.Handle, but the call must be wrapped in
	// target.turnsWG.Add(1)/Done() so it joins the same shutdown barrier the
	// per-bot session.send goroutines use.
	target := &botTarget{id: cfg.BotID, gateway: gw, store: st, secrets: sec, connector: connector}

	// Cron scheduler (#115): when enabled, load <dataDir>/cron.json and fire due
	// tasks through the gateway as synthetic CronFire messages. The owner uid that
	// gates create/delete is resolved from the bot registration (owner_uid).
	// Declared at this scope so the post-Run shutdown chain below can Wait on it,
	// and so it can be wired into botRuntime/target BEFORE reg.add — that way
	// the resolve handler doesn't have to lock-free write `bot.target.cron`
	// on every call, which raced concurrent control commands.
	var cm *cron.Manager
	if cfg.Agent.Cron != nil && *cfg.Agent.Cron {
		cm = cron.NewManager(cron.NewStore(filepath.Join(cfg.DataDir, "cron.json")), "", nil)
		cm.SetLabel(fmt.Sprintf("[%s] ", cfg.BotID))
		cm.OnFire(func(f cron.Fire) {
			fireCronTask(ctx, connector, gw, target, f.Task)
		})
		connector.OnOwner(func(ownerUID string) { cm.SetOwnerUID(ownerUID) })
	}
	target.cron = cm

	// Eager-init the per-bot control-handler target so its embedded turnsWG is
	// pinned for runBot's shutdown barrier: the lazy-init in
	// resolve left target nil for bots that no control-bus command ever
	// reached (headless-mode operator, octo+cron-only bot), which then
	// nil-derefs on `rtBot.target.turnsWG.Wait` in the shutdown chain. The
	// resolver still races on first call (two control commands could both see
	// nil), which would silently split session.send goroutines across two
	// targets — one outside the wait barrier. Setting it here ensures a single
	// target shared by every codepath. cron is also wired in upfront so the
	// resolve-side per-call write was racing concurrent reads.
	rtBot := &botRuntime{
		cfg: cfg, gateway: gw, store: st, secrets: sec, cron: cm,
		connector: connector,
		target:    target,
	}
	// Single drain defer covers both happy path and panic. Earlier code
	// expressed the same sequence THREE times (defer for connector/target,
	// defer for cron, and an explicit tail chain) with paragraph-long
	// rationale. All four steps are idempotent so the defer can be the
	// only call site:
	//   1. cm.Stop+Wait — no fresh tick, in-flight safeFire drained
	//   2. connector.WaitTurns — drainTurns workers done
	//   3. target.turnsWG.Wait — control-bus session.send done
	//   4. (defer st.Close from top of function fires last on LIFO)
	defer func() {
		if cm != nil {
			cm.Stop()
			cm.Wait()
		}
		connector.WaitTurns()
		rtBot.target.turnsWG.Wait()
	}()
	if reg != nil {
		reg.add(rtBot)
	}
	connector.OnStatus(func(connected bool, lastErr string) {
		rtBot.setStatus(connected, lastErr)
		if srv != nil {
			srv.Broadcast("bot.status", rtBot.info())
		}
	})

	if cm != nil {
		cm.Start()
		fmt.Printf("[%s] cron scheduler armed (tick %s)\n", cfg.BotID, cron.CronTickInterval)
	}

	fmt.Printf("[%s] started — driver=%s api=%s data=%s\n",
		cfg.BotID, drv.Name(), cfg.APIURL, cfg.DataDir)

	err = connector.Run(ctx)
	return err
}

// fireCronTask wakes the gateway as if a real inbound had arrived. For IM
// targets (DM/Group) it enqueues a synthetic CronFire message onto the octo
// connector's per-session worker so it serializes with any concurrent real
// inbound on the same sessionKey (direct gw.Handle here used to race
// onInbound's target write, mis-delivering one reply and dropping the other).
// For Console targets (ChannelConsole) the connector path is bypassed entirely
// — Console fires belong to the desktop GUI's CONSOLE_UID session, the IM
// connector has no business with them, and the reply naturally surfaces in
// the chat window via the existing session.user_message + session.reply event
// path. The Console call is wrapped in target.turnsWG.Add(1)/Done() so the
// runBot shutdown chain drains in-flight Console fires before st.Close.
//
// Best-effort: a failed enqueue or routing error is logged, never propagated,
// so the scheduler loop survives.
func fireCronTask(ctx context.Context, connector *octo.Connector, gw *gateway.Gateway, target *botTarget, t cron.Task) {
	if t.ChannelType == cron.ChannelConsole {
		// Console-target fire — bypass IM connector. The synthetic inbound
		// is shaped like a CONSOLE_UID DM so router.SessionKey derives the
		// same key the GUI's Composer-typed messages use, and the resulting
		// session.user_message / session.reply broadcasts land in the
		// Console session the user is watching.
		inbound := router.InboundMessage{
			FromUID:     t.FromUID,
			FromName:    t.FromName,
			ChannelType: router.ChannelDM,
			Text:        t.Prompt,
			CronFire:    true,
		}
		if _, err := inbound.SessionKey(); err != nil {
			fmt.Fprintf(os.Stderr, "cron: task %s console fire has unroutable coords: %v\n", t.ID, err)
			return
		}
		target.turnsWG.Add(1)
		go func() {
			defer target.turnsWG.Done()
			if _, err := gw.Handle(ctx, inbound); err != nil {
				fmt.Fprintf(os.Stderr, "cron: task %s console fire dispatch failed: %v\n", t.ID, err)
			}
		}()
		return
	}

	// IM targets — the original path through the per-session worker queue.
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
	connector.EnqueueCron(key, t.ChannelID, octoType, inbound)
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
			// A bot whose startup failed (registerFailedBot stub) carries a
			// nil gateway/store + a populated LastError. Without this check
			// the resolve-fallback would wire nil into a botTarget, and the
			// first handler dereferencing `t.gateway` / `t.store` would nil-
			// deref the whole daemon. Test fixtures also
			// carry nil gateway/store but no LastError, so they keep using
			// the test-friendly lazy fallback below.
			if bot.gateway == nil && bot.info().LastError != "" {
				return nil, fmt.Errorf("bot %q failed to start: %s", botID, bot.info().LastError)
			}
			// runBot eager-initializes target AND cron; production never
			// sees nil here. Tests that build a botRuntime by hand must
			// set target explicitly or go through runBot — surfacing a
			// clear error beats a lock-free lazy write that would race
			// concurrent control commands.
			if bot.target == nil {
				return nil, fmt.Errorf("bot %q not initialised", botID)
			}
			return bot.target, nil
		},
		broadcast: func(eventType string, body any) {
			if reg.srv != nil {
				reg.srv.Broadcast(eventType, body)
			}
		},
	})
}
