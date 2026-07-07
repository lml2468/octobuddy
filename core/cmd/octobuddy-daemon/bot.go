package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/lml2468/octobuddy/core/agent"
	"github.com/lml2468/octobuddy/core/clog"
	"github.com/lml2468/octobuddy/core/config"
	"github.com/lml2468/octobuddy/core/control"
	"github.com/lml2468/octobuddy/core/control/wire"
	"github.com/lml2468/octobuddy/core/cron"
	"github.com/lml2468/octobuddy/core/gateway"
	"github.com/lml2468/octobuddy/core/groupctx"
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
func runBot(ctx context.Context, configPath string, cfg config.Resolved, reg *botRegistry, srv *control.Server) error {
	// Resolve the per-bot isolated config dir from the SELECTED driver's contract
	// (Claude → <botRoot>/.claude). config left AgentConfigDir empty so it stays
	// free of any driver-specific literal; the daemon (which imports core/agent)
	// fills it here before the driver spawns. Empty dir-name (driver doesn't
	// isolate, or inheritUserConfig) leaves AgentConfigDir "".
	if dirName := agent.ConfigDirNameFor(cfg.Agent.Driver); dirName != "" && !cfg.Agent.InheritUserConfig {
		cfg.AgentConfigDir = filepath.Join(cfg.BotRoot, dirName)
	}
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

	// Build the configured driver through the registry — agent-agnostic. The
	// per-turn seams (bin/env/mcp-config) are wired via Options so a freshly-
	// landed background install, an injected token, or a freshly-saved .mcp.json
	// all apply on the next turn without a restart. The env is supplied as a
	// driver-NEUTRAL EnvSpec; the driver maps it onto its own var names.
	drv, err := agent.New(cfg.Agent.Driver, agent.Options{
		BinFn:       resolveAgentBin(cfg.Agent.Driver),
		EnvSpecFn:   func() agent.EnvSpec { return botEnvSpec(cfg, sec) },
		MCPConfigFn: botMCPConfigFn(cfg),
	})
	if err != nil {
		return fmt.Errorf("bot %s: %w", cfg.BotID, err)
	}

	rtBot, connector, cm, err := assembleBotRuntime(ctx, configPath, cfg, srv, st, rt, sec, drv)
	if err != nil {
		return err
	}
	// MCP health-check hook for the control bus (desktop "test connection").
	// Built here where the concrete driver + secret store are in scope so the
	// probe runs with the bot's real bin + env + .mcp.json path.
	if botMCPConfigPath(cfg) != "" {
		rtBot.target.mcpCheck = makeMCPChecker(cfg, sec, drv)
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
	configPath string,
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
	gw := newBotGateway(configPath, cfg, srv, st, rt, drv, connector, grantor)
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
	// Stable per-bot WuKongIM device id: a random id every reconnect makes the
	// server kick our own prior (possibly half-open) session as a duplicate
	// login, which surfaces as "server sent disconnect". Persisting it makes a
	// reconnect look like the same device resuming.
	connector.SetDeviceID(loadOrCreateDeviceID(cfg))

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

// botEnvSpec assembles the driver-NEUTRAL env spec for one turn: the resolved
// user agent.env, the gateway routing creds (resolved live so an injected token
// wins), the octo-cli companion fallback, and the per-bot config-dir isolation.
// The selected driver maps this onto its own var names (Claude →
// ANTHROPIC_*/CLAUDE_CONFIG_DIR) via its EnvMapper — the daemon names nothing
// driver-specific here.
func botEnvSpec(cfg config.Resolved, sec *secretStore) agent.EnvSpec {
	return agent.EnvSpec{
		Extra:             cfg.AgentEnvExtra(sec.Secret),
		GatewayBaseURL:    cfg.Agent.GatewayBaseURL,
		GatewayToken:      sec.GatewayToken(),
		OctoToken:         sec.OctoToken(),
		OctoAPIURL:        cfg.APIURL,
		ConfigDir:         cfg.AgentConfigDir,
		InheritUserConfig: cfg.Agent.InheritUserConfig,
	}
}

// botMCPConfigFn returns the per-Query MCP-config path resolver for the driver,
// or nil when MCP doesn't apply to this bot (no isolated config dir, or it
// inherits the operator's user config). Resolved per turn so a freshly-saved
// .mcp.json applies next message without a restart.
func botMCPConfigFn(cfg config.Resolved) func() string {
	if botMCPConfigPath(cfg) == "" {
		return nil
	}
	root := cfg.AgentConfigDir
	return func() string { return existingFilePath(root, ".mcp.json") }
}

// botMCPConfigPath returns the bot's .mcp.json path (under its isolated
// per-bot config dir), or "" when MCP doesn't apply — no config dir, or the bot
// inherits the operator's user config (then MCP is whatever that carries,
// not a per-bot file). Single source of the path so the driver's per-turn
// loader and the health-check probe never drift.
//
// Threat model: a loaded .mcp.json causes the agent CLI to spawn each declared
// server as a child process (MCP server launch IS local command execution). This
// is NOT a privilege escalation — writing .mcp.json requires Write/Bash, which
// the agent only has when those tools are in its surface, i.e. it already has
// code execution as the same user. The relevant guard is that .mcp.json is
// loaded through existingFilePath → safepath.SafeLstat, which refuses a symlinked
// file OR a symlinked parent component, so the agent cannot redirect the load to
// an attacker-controlled path outside its own writable tree.
func botMCPConfigPath(cfg config.Resolved) string {
	if cfg.AgentConfigDir == "" || cfg.Agent.InheritUserConfig {
		return ""
	}
	return filepath.Join(cfg.AgentConfigDir, ".mcp.json")
}

// makeMCPChecker builds the control-bus MCP health probe for one bot. It probes
// the bot's saved .mcp.json with the bot's resolved bin + env (so the result
// matches a real turn) via the driver's agent.MCPProber capability — no
// concrete-type assertion. Returns nil when the driver has no MCP concept or the
// bot has no config dir. {Configured:false} when no .mcp.json exists yet. 60s cap.
func makeMCPChecker(cfg config.Resolved, sec *secretStore, drv agent.Driver) func(context.Context) (wire.MCPCheckResponse, error) {
	prober, ok := drv.(agent.MCPProber)
	if !ok {
		return nil
	}
	// EnvMapper and MCPProber are INDEPENDENT optional capabilities (see the
	// agent.Driver doc): a driver may implement one without the other, so guard
	// the env-mapping assertion too rather than risk a panic on a future driver.
	mapper, ok := drv.(agent.EnvMapper)
	if !ok {
		return nil
	}
	mcpPath := botMCPConfigPath(cfg)
	return func(ctx context.Context) (wire.MCPCheckResponse, error) {
		if mcpPath == "" || existingFilePath(cfg.AgentConfigDir, ".mcp.json") == "" {
			return wire.MCPCheckResponse{Configured: false}, nil
		}
		ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		env := mapper.Env(botEnvSpec(cfg, sec))
		// Probe under the bot's sandbox base so a server with a cwd-relative
		// command/path resolves the same way a real session turn would — but only
		// when that dir already exists. CwdBase (~/.octobuddy/<id>/workspace) is
		// created lazily on the first turn, not at startup, so on a fresh bot it's
		// absent; passing a nonexistent dir as the spawn cwd fails chdir and the
		// probe would falsely report the MCP servers unreachable. Fall back to ""
		// (the daemon's cwd) when it isn't there yet.
		statuses, err := prober.ProbeMCPConfig(ctx, existingDir(cfg.BotRoot, "workspace"), env, mcpPath)
		if err != nil {
			return wire.MCPCheckResponse{}, err
		}
		out := wire.MCPCheckResponse{Configured: true}
		for _, s := range statuses {
			out.Servers = append(out.Servers, wire.MCPServerHealth{Name: s.Name, Status: s.Status, Tools: s.Tools})
		}
		return out, nil
	}
}

// existingFilePath returns the joined root/rel path if rel is a regular,
// non-symlink, non-empty file reachable from root with no symlinked component,
// else "". Used to gate --mcp-config on the bot's .mcp.json existing.
//
// Routes the existence + symlink check through safepath.SafeLstat rather than a
// raw os.Lstat so a symlinked PARENT (e.g. an agent that plants the .claude dir
// itself as a symlink) is refused too — not just a symlinked leaf. CLAUDE.md:
// callers must not hand-roll Lstat/EvalSymlinks for paths under a root; those
// concerns live in safepath.
func existingFilePath(root, rel string) string {
	fi, err := safepath.SafeLstat(root, rel)
	if err != nil {
		return ""
	}
	if fi.Mode()&os.ModeSymlink != 0 || fi.IsDir() || fi.Size() == 0 {
		return ""
	}
	return filepath.Join(root, rel)
}

// existingDir returns the joined root/rel path if rel is an existing directory
// reachable from root with no symlinked component, else "". Used to gate the MCP
// probe's spawn cwd on the per-session sandbox base existing (it's created
// lazily on the first turn, not at startup).
//
// Routes through safepath.SafeLstat rather than a raw os.Stat: rel lives under
// ~/.octobuddy/<id>, the agent-writable tree (cwd is a starting dir, not a
// chroot), so an agent that plants `workspace` as a symlink must not redirect
// the probe's chdir to an attacker dir. CLAUDE.md: symlink concerns live in
// safepath.
func existingDir(root, rel string) string {
	if root == "" || rel == "" {
		return ""
	}
	if fi, err := safepath.SafeLstat(root, rel); err == nil && fi.IsDir() && fi.Mode()&os.ModeSymlink == 0 {
		return filepath.Join(root, rel)
	}
	return ""
}

// resolveAgentBin returns a per-Query binary resolver for the named driver: it
// prefers the desktop-managed binary at ~/.octobuddy/bin/<name> when it exists,
// falling back to the bare command on PATH otherwise. Wired as Options.BinFn so
// a freshly-completed background install lands on the next turn without a
// restart. The binary base name is the driver name (claude → "claude", codex →
// "codex"); an empty/unknown driver resolves to the default ("claude").
//
// Uses safepath.SafeLstat (not a raw os.Lstat) so the whole chain under
// ~/.octobuddy/bin/ — not just the leaf — is checked for symlinks: an agent
// that plants ~/.octobuddy/bin as a symlink to an attacker dir is refused, the
// consistent codebase stance (CLAUDE.md: symlink concerns live in safepath).
// 0-byte and non-executable files are also rejected so a crashed install temp
// doesn't masquerade as the binary.
func resolveAgentBin(driver string) func() string {
	name := driver
	if name == "" {
		name = agent.DefaultDriver
	}
	return func() string {
		home, err := os.UserHomeDir()
		if err != nil {
			return name
		}
		bin := name
		if runtime.GOOS == "windows" {
			bin = name + ".exe"
		}
		root := filepath.Join(home, ".octobuddy", "bin")
		fi, err := safepath.SafeLstat(root, bin)
		if err != nil || !safepath.UsableExecutable(fi) {
			return name
		}
		return filepath.Join(root, bin)
	}
}

func newBotGateway(
	configPath string,
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
		WithGroupBackfill(connector.BotUID, connector.BackfillFetch).
		WithOwner(connector.OwnerUID).
		WithSystemPromptResolver(botRootFileResolver(cfg, config.SystemPromptFor)).
		WithBootstrapResolver(botRootFileResolver(cfg, config.BootstrapFor)).
		WithPersona(grantor, cfg.OnBehalfOf.PersonaPrompt).
		WithModel(cfg.Agent.Model).
		WithToolResolver(botToolResolver(configPath, cfg)).
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

// botToolResolver returns the gateway's per-turn tool-surface resolver. It
// re-reads config.json (via config.ToolPolicyFor) on EVERY turn so a desktop
// edit to a conversation's tools (configstore.SetChannelTools writes config.json
// directly) takes effect on the next message WITHOUT a daemon restart —
// matching the per-Query MCP-config and binary resolvers. Resolution itself is
// the single config.ToolPolicy.Resolve — no duplicated logic in the gateway.
//
// ToolPolicyFor's ok flag separates "read failed" from "policy legitimately
// absent": on a read failure (ok=false) we fall back to the at-startup snapshot
// so a transient mid-write read never widens the surface; on a successful read
// with no policy (ok=true, p=nil) we honor it — p.Resolve(nil) returns !ok and
// the gateway uses the driver default, so CLEARING a policy at runtime actually
// takes effect (it previously reverted to the stale snapshot until restart).
func botToolResolver(configPath string, cfg config.Resolved) func(string) ([]string, bool) {
	fallback := cfg.Agent.Tools
	botID := cfg.BotID
	return func(sessionKey string) ([]string, bool) {
		p, ok := config.ToolPolicyFor(configPath, botID)
		if !ok {
			p = fallback // read failed → keep the last-known (startup) policy
		}
		return p.Resolve(sessionKey)
	}
}

// botRootFileResolver returns a per-turn resolver that re-reads an
// operator-trusted file under the bot root via readFn on EVERY turn, so a
// desktop (or the bot's own) edit applies on the next message without a daemon
// restart — the same per-Query philosophy as the tool / MCP-config / binary
// resolvers. Used for both SOUL/AGENTS (config.SystemPromptFor) and BOOTSTRAP.md
// (config.BootstrapFor); neither needs a config.json read (the files live in
// botRoot), so there is no fallback to thread — an empty result is honored by
// the gateway (cleared files → that block drops out).
func botRootFileResolver(cfg config.Resolved, readFn func(botRoot string) string) func() string {
	root := cfg.BotRoot
	return func() string { return readFn(root) }
}

// loadOrCreateDeviceID returns a stable WuKongIM device id for the bot,
// persisted at <dataDir>/device_id so it survives daemon restarts. On a read
// miss (first run, or an empty/corrupt file) it mints a fresh uuid+"W" and
// writes it back. The id is not a secret (it identifies a device slot, not the
// bot), so a write failure is non-fatal — we just return the freshly-minted id
// and fall back to a fresh one next boot. The file lives under DataDir, which
// prepareBotDirs created before this runs.
func loadOrCreateDeviceID(cfg config.Resolved) string {
	path := filepath.Join(cfg.DataDir, "device_id")
	if b, err := safepath.SafeReadAbs(path, 256); err == nil {
		if id := strings.TrimSpace(string(b)); id != "" {
			return id
		}
	}
	id := octo.NewDeviceID()
	if err := safepath.SafeWriteAbs(path, []byte(id), 0o600); err != nil {
		clog.For("octo").Warn("persist device_id failed; using ephemeral id", "bot", cfg.BotID, "err", err)
	}
	return id
}

// SafeMkdirAllAbs walks the parent chain via dirfd, refusing any symlinked
// intermediate with ErrSymlink. An agent (Bash + bypass) in any existing bot's
// cwd could otherwise plant `~/.octobuddy/<newbotID>` as a symlink to `~/.ssh/`
// before the operator adds the new bot; a bare MkdirAll would silently follow it.
func prepareBotDirs(cfg config.Resolved) error {
	if err := safepath.SafeMkdirAllAbs(cfg.DataDir, 0o755); err != nil {
		return fmt.Errorf("bot %s: mkdir data: %w", cfg.BotID, err)
	}
	// Isolated per-bot agent config dir (unless inheriting the operator's user
	// config). Created here so the agent's config root exists before it spawns.
	// AgentConfigDir was filled by runBot from the selected driver's contract.
	if cfg.AgentConfigDir != "" && !cfg.Agent.InheritUserConfig {
		if err := safepath.SafeMkdirAllAbs(cfg.AgentConfigDir, 0o700); err != nil {
			return fmt.Errorf("bot %s: mkdir agent config dir: %w", cfg.BotID, err)
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
