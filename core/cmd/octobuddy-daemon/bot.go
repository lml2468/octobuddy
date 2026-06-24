package main

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/lml2468/octobuddy/core/agent"
	"github.com/lml2468/octobuddy/core/clog"
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
	"github.com/lml2468/octobuddy/core/trigger"
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
		MaxPerMinute:   cfg.RateLimit.MaxPerMinute,
		KnownBotUids:   cfg.KnownBotUids,
		AllowedBotUids: cfg.AllowedBotUids,
		BotBlocklist:   cfg.BotBlocklist,
	})

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

	rtBot, connector, cm, err := assembleBotRuntime(ctx, cfg, srv, st, rt, sec, drv)
	if err != nil {
		return err
	}
	defer drainBotRuntime(cm, connector, rtBot.target)
	registerBotRuntime(rtBot, reg, srv)
	startBotCron(cm, cfg.BotID)
	// Reaper sweeps both the router lock/rate-limit maps AND the
	// group-context channel windows (issue #105 follow-on: bound memory
	// over the daemon's lifetime — the in-memory window used to grow
	// unbounded across channels).
	startRouterReaper(ctx, rt, rtBot.gateway)

	fmt.Printf("[%s] started — driver=%s api=%s data=%s\n",
		cfg.BotID, drv.Name(), cfg.APIURL, cfg.DataDir)

	err = connector.Run(ctx)
	return err
}

func assembleBotRuntime(
	ctx context.Context,
	cfg config.Resolved,
	srv *control.Server,
	st *store.Store,
	rt *router.Router,
	sec *secretStore,
	drv agent.Driver,
) (*botRuntime, *octo.Connector, *cron.Manager, error) {
	if cfg.APIURL == "" {
		return nil, nil, nil, fmt.Errorf("bot %s: config mode requires apiUrl", cfg.BotID)
	}
	connector, grantor := newBotConnector(cfg, sec)
	gw := newBotGateway(cfg, srv, st, rt, drv, connector, grantor)
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

	// Eager-init the per-bot control-handler target so every control path shares
	// the same shutdown barrier. Cron is wired in upfront for the same reason.
	rtBot := &botRuntime{
		cfg: cfg, gateway: gw, store: st, secrets: sec, cron: cm,
		connector: connector,
		target:    target,
	}
	return rtBot, connector, cm, nil
}

func newBotConnector(cfg config.Resolved, sec *secretStore) (*octo.Connector, persona.Grantor) {
	// The Octo token is read lazily; it may arrive via secret.inject after start,
	// so an empty token here is allowed (the connector waits for it).
	connector := octo.NewConnector(octo.NewRESTClient(cfg.APIURL, sec.OctoToken))
	connector.SetToolProgress(cfg.Agent.ToolProgress)

	// Persona clone (openclaw OBO): when onBehalfOf is configured, the
	// classifier widens the trigger gate + the connector routes replies
	// as the grantor via TriggerDecision.ReplyRouting. A zero grantor is
	// a no-op (regular bot).
	grantor := persona.Grantor{UID: cfg.OnBehalfOf.UID, Name: cfg.OnBehalfOf.Name}

	// Trigger policy — single source of truth for "should this message
	// reply?". The router and the connector both consult the SAME policy
	// via the same classifier (legacy mentionFree double-copy is gone,
	// issue #105 缺陷 2). Policy.BotUID is seeded with the config id
	// here, but the connector overrides it with the server-registered
	// uid at classify time (see prepareInboundTurn) — that's the uid IM
	// @-mention payloads carry.
	connector.SetPolicy(triggerPolicyFromConfig(cfg, grantor))
	return connector, grantor
}

// triggerPolicyFromConfig assembles the trigger.Policy from resolved
// config + grantor. Defaults:
//   - AIBroadcast defaults to Deny (the bug fix from issue #105). Operators
//     who want legacy behavior set trigger.aiBroadcast="allow" in config.
//   - ReplyToBotEnabled defaults to true so users keep their natural
//     "continue the thread" interaction under Deny.
func triggerPolicyFromConfig(cfg config.Resolved, grantor persona.Grantor) trigger.Policy {
	tg := cfg.Trigger
	aib := trigger.AIBroadcastPolicy(tg.AIBroadcast)
	if !aib.Valid() {
		aib = trigger.AIBroadcastDeny
		clog.For("config").Warn("trigger.aiBroadcast unset/invalid; defaulting to deny (issue #105 fix)", "bot", cfg.BotID)
	}
	return trigger.Policy{
		BotUID:               cfg.BotID,
		Grantor:              trigger.FromPersonaGrantor(grantor),
		MentionFreeGroups:    toBoolSet(tg.MentionFreeGroups),
		AIBroadcast:          aib,
		AIBroadcastAllowlist: toBoolSet(tg.AIBroadcastAllowlist),
		ReplyToBotEnabled:    tg.ReplyToBotEnabled == nil || *tg.ReplyToBotEnabled,
	}
}

func toBoolSet(vals []string) map[string]bool {
	if len(vals) == 0 {
		return nil
	}
	m := make(map[string]bool, len(vals))
	for _, v := range vals {
		if v != "" {
			m[v] = true
		}
	}
	return m
}

func newBotGateway(
	cfg config.Resolved,
	srv *control.Server,
	st *store.Store,
	rt *router.Router,
	drv agent.Driver,
	connector *octo.Connector,
	grantor persona.Grantor,
) *gateway.Gateway {
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
		connector.SetNameResolvedHook(nameResolvedBroadcaster(srv, cfg.BotID, st, connector))
	}
	return gw
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

func startRouterReaper(ctx context.Context, rt *router.Router, gw *gateway.Gateway) {
	// Periodic reaper: bound the router's per-session lock / rate-limit
	// maps AND the gateway's group-context channel windows over the
	// daemon's lifetime. Sessions/messages/sandboxes themselves are not
	// expired (persistent — only in-memory tracking maps).
	reap := func() {
		rt.Reap(routerReapIdle)
		if gw != nil {
			gw.ReapGroupContext(routerReapIdle)
		}
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
