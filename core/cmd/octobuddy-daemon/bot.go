package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
	drv.Mode = resolvePromptMode(cfg.Agent.SystemPromptMode, cfg.BotID)
	// BinFn runs per Query so a freshly-landed background install
	// (~/.octobuddy/bin/claude from claudecli) is picked up on the very
	// next turn — no restart required.
	drv.BinFn = resolveClaudeBin
	// Resolve the gateway token lazily per turn so an injected token takes effect.
	drv.EnvFn = func() []string { return cfg.DriverEnv(sec.GatewayToken(), sec.OctoToken(), sec.Secret) }

	rtBot, connector, cm, err := assembleBotRuntime(ctx, cfg, srv, st, rt, sec, drv)
	if err != nil {
		return err
	}
	defer drainBotRuntime(cm, connector, rtBot.target)
	registerBotRuntime(rtBot, reg, srv)
	startBotCron(cm, cfg.BotID)
	// Reaper sweeps router lock/rate-limit maps + group-context channel
	// windows so the in-memory state stays bounded over the daemon's
	// lifetime.
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

	// Cron scheduler: when enabled, load <dataDir>/cron.json and fire due
	// tasks through the gateway as synthetic cron messages. The owner uid
	// that gates create/delete is resolved from the bot registration.
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
	// reply?". Policy.BotUID is seeded with the config id; the connector
	// rewrites it with the server-registered uid at register time.
	connector.SetPolicy(triggerPolicyFromConfig(cfg, grantor))
	return connector, grantor
}

// triggerPolicyFromConfig assembles trigger.Policy from resolved config.
// AIBroadcast defaults to Deny if unset/invalid; ReplyToBotEnabled
// defaults to true so users keep the "continue the thread" UX.
func triggerPolicyFromConfig(cfg config.Resolved, grantor persona.Grantor) trigger.Policy {
	tg := cfg.Trigger
	aib := trigger.AIBroadcastPolicy(tg.AIBroadcast)
	if !aib.Valid() {
		aib = trigger.AIBroadcastDeny
		clog.For("config").Warn("trigger.aiBroadcast unset/invalid; defaulting to deny", "bot", cfg.BotID)
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

// toolPolicyArgs unpacks a (possibly nil) per-bot ToolPolicy into the
// (default, channels) pair WithToolPolicy expects. nil policy → both nil,
// leaving the driver's probed headless-safe default in force.
func toolPolicyArgs(p *config.ToolPolicy) ([]string, map[string][]string) {
	if p == nil {
		return nil, nil
	}
	return p.Default, p.Channels
}

// resolvePromptMode maps the on-disk string to ClaudeDriver's typed
// PromptMode constant. Unset → minimal. Unknown values warn and default
// to minimal so a typo can't silently change behavior.
func resolvePromptMode(raw, botID string) agent.PromptMode {
	switch raw {
	case "", string(agent.PromptModeMinimal):
		return agent.PromptModeMinimal
	case string(agent.PromptModeClaudeCode):
		return agent.PromptModeClaudeCode
	default:
		clog.For("config").Warn("agent.systemPromptMode unknown; defaulting to minimal",
			"bot", botID, "got", raw)
		return agent.PromptModeMinimal
	}
}

// resolveClaudeBin returns the desktop-managed binary at
// ~/.octobuddy/bin/claude when it exists, falling back to "claude" on
// PATH otherwise. Called per Query via ClaudeDriver.BinFn so a
// freshly-completed background install lands on the next turn without
// requiring a restart.
//
// Uses Lstat + symlink check: anything under ~/.octobuddy/bin/ that
// resolves through a symlink is rejected (the rest of the codebase
// treats symlinks under ~/.octobuddy as hostile via safepath; this is
// the consistent stance). 0-byte and non-executable files are also
// rejected so a crashed install temp doesn't masquerade as the binary.
func resolveClaudeBin() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "claude"
	}
	name := "claude"
	if runtime.GOOS == "windows" {
		name = "claude.exe"
	}
	path := filepath.Join(home, ".octobuddy", "bin", name)
	fi, err := os.Lstat(path)
	if err != nil {
		return "claude"
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return "claude"
	}
	if fi.IsDir() || fi.Size() == 0 {
		return "claude"
	}
	// Executable bit unset would still let claude run on Windows (no
	// posix x bit), so this check is unix-only.
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o111 == 0 {
		return "claude"
	}
	return path
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
		WithToolPolicy(toolPolicyArgs(cfg.Agent.Tools)).
		WithSettingSources(cfg.Agent.SettingSources).
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
