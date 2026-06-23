package main

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/lml2468/octobuddy/core/agent"
	"github.com/lml2468/octobuddy/core/config"
	"github.com/lml2468/octobuddy/core/control"
	"github.com/lml2468/octobuddy/core/control/wire"
	"github.com/lml2468/octobuddy/core/cron"
	"github.com/lml2468/octobuddy/core/gateway"
	"github.com/lml2468/octobuddy/core/groupctx"
	"github.com/lml2468/octobuddy/core/groupmd"
	"github.com/lml2468/octobuddy/core/im/octo"
	"github.com/lml2468/octobuddy/core/persona"
	"github.com/lml2468/octobuddy/core/router"
	"github.com/lml2468/octobuddy/core/safepath"
	"github.com/lml2468/octobuddy/core/store"
)

// Reaper cadence for a running bot. reapInterval is how often the sweep runs;
// routerReapIdle is how long a session's lock / rate-limit buckets must sit
// untouched before they're evicted (far longer than the rate-limit window so an
// evicted bucket is always fully refilled).
const (
	reapInterval   = time.Hour
	routerReapIdle = time.Hour
)

// runBot assembles and runs one bot's complete, isolated stack. When srv is set,
// agent events are also broadcast to the control bus tagged with the bot id, and
// the bot is registered for command routing + bots.list. Blocks until ctx done.
func runBot(ctx context.Context, cfg config.Resolved, reg *botRegistry, srv *control.Server) error {
	if err := prepareBotDirs(cfg); err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "octobuddy.db"))
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

	startRouterReaper(ctx, rt)

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
		// Push a fresh session.upserted when a late name fetch resolves, so a
		// sidebar row that first painted with the bare id (DM peer / group name
		// not yet cached at sessions.list time) updates without a turn.
		connector.SetNameResolvedHook(nameResolvedBroadcaster(srv, cfg.BotID, st, connector))
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
	cm := newBotCronManager(ctx, cfg, connector, gw, target)
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
	defer drainBotRuntime(cm, connector, rtBot.target)
	registerBotRuntime(rtBot, reg, srv)

	startBotCron(cm, cfg.BotID)

	fmt.Printf("[%s] started — driver=%s api=%s data=%s\n",
		cfg.BotID, drv.Name(), cfg.APIURL, cfg.DataDir)

	err = connector.Run(ctx)
	return err
}

// SafeMkdirAllAbs walks the parent chain via dirfd, refusing any symlinked
// intermediate with ErrSymlink. An agent (Bash + bypass) in any existing bot's
// cwd could otherwise plant `~/.octobuddy/<newbotID>` as a symlink to `~/.ssh/`
// before the operator adds the new bot; a bare MkdirAll would silently follow it.
func prepareBotDirs(cfg config.Resolved) error {
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
	return nil
}

func startRouterReaper(ctx context.Context, rt *router.Router) {
	// Periodic reaper: bound the router's per-session lock / rate-limit maps over
	// the daemon's lifetime. Sessions/messages/sandboxes themselves are not expired.
	reap := func() { rt.Reap(routerReapIdle) }
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
}

func newBotCronManager(ctx context.Context, cfg config.Resolved, connector *octo.Connector, gw *gateway.Gateway, target *botTarget) *cron.Manager {
	if cfg.Agent.Cron == nil || !*cfg.Agent.Cron {
		return nil
	}
	cm := cron.NewManager(cron.NewStore(filepath.Join(cfg.DataDir, "cron.json")), "", nil)
	cm.SetLabel(fmt.Sprintf("[%s] ", cfg.BotID))
	cm.OnFire(func(f cron.Fire) {
		fireCronTask(ctx, connector, gw, target, f.Task)
	})
	connector.OnOwner(func(ownerUID string) { cm.SetOwnerUID(ownerUID) })
	return cm
}

func drainBotRuntime(cm *cron.Manager, connector *octo.Connector, target *botTarget) {
	if cm != nil {
		cm.Stop()
		cm.Wait()
	}
	connector.WaitTurns()
	target.turnsWG.Wait()
}

func registerBotRuntime(rtBot *botRuntime, reg *botRegistry, srv *control.Server) {
	if reg != nil {
		reg.add(rtBot)
	}
	rtBot.connector.OnStatus(func(connected bool, lastErr string) {
		rtBot.setStatus(connected, lastErr)
		if srv != nil {
			srv.Broadcast("bot.status", rtBot.info())
		}
	})
}

func startBotCron(cm *cron.Manager, botID string) {
	if cm == nil {
		return
	}
	cm.Start()
	fmt.Printf("[%s] cron scheduler armed (tick %s)\n", botID, cron.CronTickInterval)
}
