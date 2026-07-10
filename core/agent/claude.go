package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/lml2468/octobuddy/core/clog"
)

// interactiveExclusions are built-in tools that need an interactive terminal
// to function. In a headless `-p` turn the model would call them and stall
// (there's no UI to render the question / plan / worktree picker), so they
// must never be offered. The headless-safe default tool surface is whatever
// the live claude binary reports (via ProbeTools) MINUS this denylist.
//
// This is a small, stable DENYLIST — deliberately not a hand-maintained
// allowlist. The allowlist drifts per claude release (tool renames,
// additions); sourcing it from the binary and subtracting these avoids the
// silent-drop hazard a hardcoded allowlist had (e.g. "Agent" vs "Task",
// "TodoWrite" vanishing).
var interactiveExclusions = map[string]bool{
	"AskUserQuestion": true,
	"EnterPlanMode":   true,
	"ExitPlanMode":    true,
	"EnterWorktree":   true,
	"ExitWorktree":    true,
}

// ClaudeDriver drives Claude Code headlessly via:
//
//	claude -p - --output-format stream-json --verbose [--resume <id>]...
//
// with the prompt fed on stdin, and normalizes its line-delimited JSON
// ("stream-json") into AgentEvents.
//
// Output is plain stream-json (one event per complete content block); the
// driver does NOT request --include-partial-messages, so there are no
// token-level deltas to de-duplicate.
//
// It is the ONE place that knows about the claude binary, its argv
// shape, and its env requirements (ANTHROPIC_*, CLAUDE_CONFIG_DIR).
type ClaudeDriver struct {
	// procDriver carries the shared process-spawning state (Bin/BinFn/EnvFn/
	// EnvSpecFn/StaticEnv/MCPConfigFn/ExtraArgs) + binPath()/queryEnv(). Embedded
	// so a second driver reuses the resolution logic instead of re-copying it.
	// StaticEnv here layers ANTHROPIC_BASE_URL/OCTO_BOT_ID/GH_TOKEN etc; EnvSpecFn
	// is mapped through this driver's Env() (wired as procDriver.envMap in init).
	procDriver

	// selfcheckOnce gates a one-time diagnostic line emitted on the
	// FIRST Query: claude path resolution, masked ANTHROPIC_AUTH_TOKEN,
	// ANTHROPIC_BASE_URL, CLAUDE_CONFIG_DIR + writability, and the
	// allowed-tools count. Single most useful line for
	// diagnosing "出错了，请稍后重试" from a fresh install.
	selfcheckOnce sync.Once

	// toolsMu guards toolsCache: the per-binary headless-safe tool list
	// (probed available set minus interactiveExclusions), resolved lazily
	// and used as the --tools value when Request.AllowedTools is nil. Keyed
	// by resolved binary path so a background install landing at a new path
	// re-probes; an in-place upgrade is re-probed on the next daemon restart
	// (the desktop restarts core after Upgrade).
	toolsMu    sync.Mutex
	toolsCache map[string]toolProbe
}

// toolProbe caches one binary's probe outcome. ok=false means the probe
// failed (binary missing/unprobeable); the caller then surfaces the CLI's
// own default tool set rather than a hand-maintained Go list. at records when
// the outcome was taken so a FAILED probe can expire and be retried.
type toolProbe struct {
	tools []string
	ok    bool
	at    time.Time
}

// toolProbeRetryInterval bounds how long a FAILED probe is cached before the
// next headlessTools() call re-probes, so a transient failure (binary mid-
// upgrade, brief resource exhaustion) self-heals instead of pinning the
// degraded CLI-default fallback for the daemon's lifetime. Successful probes
// are cached until the binary path changes or the daemon restarts.
const toolProbeRetryInterval = time.Minute

// probeToolsFn is the seam headlessTools() uses to probe the binary's tool
// surface; production points it at ProbeTools (a real claude spawn). Tests
// override it to drive headlessTools' caching decisions (empty-filter, failure)
// without a binary.
var probeToolsFn = ProbeTools

func NewClaudeDriver(bin string) *ClaudeDriver {
	if bin == "" {
		bin = "claude"
	}
	d := &ClaudeDriver{}
	d.Bin = bin
	d.envMap = d.Env // map EnvSpecFn results through this driver's var names
	return d
}

// init self-registers the claude driver so importing the agent package makes
// "claude" selectable via New. The factory maps the generic Options onto the
// embedded procDriver fields the daemon used to set by hand.
func init() {
	Register("claude", func(o Options) Driver {
		d := NewClaudeDriver(o.Bin)
		d.BinFn = o.BinFn
		d.EnvFn = o.EnvFn
		d.EnvSpecFn = o.EnvSpecFn
		d.MCPConfigFn = o.MCPConfigFn
		d.ExtraArgs = o.ExtraArgs
		return d
	})
}

func (d *ClaudeDriver) Name() string { return "claude" }

// ConfigDirName is the per-bot config-dir base under the bot root. Claude reads
// CLAUDE_CONFIG_DIR; we point it at <botRoot>/.claude. (agent.ConfigDirNamer)
func (d *ClaudeDriver) ConfigDirName() string { return ".claude" }

// Env maps the neutral EnvSpec onto the env vars the claude CLI consumes
// (ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN / CLAUDE_CONFIG_DIR) plus the
// octo-cli companion fallback. Order matches the legacy config.DriverEnv: the
// user-declared agent.env first, then the named vars (so the routing/credential
// injections win over a same-named agent.env entry). (agent.EnvMapper)
//
// Security note: the gateway token is handed to the spawned claude child as an
// environment variable. On Linux that makes it readable from /proc/<pid>/environ
// by any same-uid process; this is the accepted tradeoff documented in
// SECURITY.md — the agent CLI takes credentials via env and the daemon runs as
// the operator.
func (d *ClaudeDriver) Env(spec EnvSpec) []string {
	return mapEnvSpec(spec, "ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CONFIG_DIR")
}

// ProbeMCPConfig satisfies agent.MCPProber: it probes the bot's saved MCP config
// with the bot's resolved binary, so the daemon's MCP health check no longer
// needs to type-assert the concrete driver. (agent.MCPProber)
func (d *ClaudeDriver) ProbeMCPConfig(ctx context.Context, cwd string, env []string, mcpPath string) ([]MCPServerStatus, error) {
	return ProbeMCP(ctx, d.binPath(), cwd, env, mcpPath)
}

// ProbeToolNames satisfies agent.ToolProber: the desktop tool picker probes the
// live binary's tool surface through this. (agent.ToolProber; ResolveBin below.)
func (d *ClaudeDriver) ProbeToolNames(ctx context.Context, env []string) ([]string, error) {
	return ProbeTools(ctx, d.binPath(), env)
}

func (d *ClaudeDriver) Capabilities() Capabilities {
	return Capabilities{
		Streaming: true, Resume: true, ToolEvents: true,
		NativeSystemPrompt: true, // --system-prompt (REPLACE)
		ToolScoping:        true, // --tools scopes the surface
		MCP:                true, // --mcp-config
	}
}

func (d *ClaudeDriver) buildArgs(req Request) []string {
	// Prompt on stdin (`-p -`), never argv, so a large prompt (group
	// backfill + inlined file content) can't hit ARG_MAX.
	args := []string{"-p", "-", "--output-format", "stream-json", "--verbose"}
	if req.SessionID != "" {
		args = append(args, "--resume", req.SessionID)
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	// Resolve the MCP config path up front: the tool surface depends on
	// whether MCP is active this turn (the default whitelist must
	// admit mcp__* so configured servers are actually callable).
	mcpPath := ""
	if d.MCPConfigFn != nil {
		mcpPath = d.MCPConfigFn()
	}
	args = d.appendHeadlessArgs(args, req, mcpPath != "")
	// Pin auto-memory to the per-session dir. --settings MERGES into the
	// defaults (does not replace), so project-scope skill discovery still
	// works. JSON-encode so a path with special chars can't break the flag.
	//
	// Contract: req.MemoryDir MUST live OUTSIDE req.Cwd. An agent with
	// write access to its cwd must not author the memory injected as
	// trusted context. The gateway derives the two from disjoint bases.
	if req.MemoryDir != "" {
		if b, err := json.Marshal(map[string]string{"autoMemoryDirectory": req.MemoryDir}); err == nil {
			args = append(args, "--settings", string(b))
		}
	}
	// Load this bot's MCP servers (and only those) when a .mcp.json exists.
	// .mcp.json is a project-scope file; --setting-sources=user does NOT
	// auto-scan CLAUDE_CONFIG_DIR for it, so the explicit --mcp-config is
	// required. --strict-mcp-config ignores any other MCP source. Resolved
	// per Query so a freshly-written file applies next turn.
	if mcpPath != "" {
		args = append(args, "--mcp-config", mcpPath, "--strict-mcp-config")
	}
	args = append(args, d.ExtraArgs...)
	return args
}

// appendHeadlessArgs emits the SDK-aligned flag set: replace the
// system prompt, silence project/local config so a planted CLAUDE.md /
// skills / agents under the sandbox cwd can't influence the model,
// keep user-scope so per-bot skills under CLAUDE_CONFIG_DIR still load,
// restrict the tool SURFACE via --tools (not --allowedTools, which is
// the auto-approve list under prompt-based permission modes and does not actually
// scope what the model can see — confirmed against claude 2.1.187).
//
// Permission mode is bypassPermissions: headless
// `-p` has no TTY to answer approval prompts and we pass no --allowedTools
// auto-approve list, so under --permission-mode default (or any
// prompt-based mode) the CLI auto-denies — or hangs on — every
// write-class tool (Bash/Write/Edit), silently breaking the turn. The
// tool SURFACE is scoped by --tools, which is orthogonal to the
// permission mode, so capability restriction is unaffected.
func (d *ClaudeDriver) appendHeadlessArgs(args []string, req Request, mcpActive bool) []string {
	args = append(args, "--permission-mode", "bypassPermissions")
	// =user keeps CLAUDE_CONFIG_DIR-based skill discovery alive while
	// dropping project (cwd .claude/) and local (cwd .claude.local) so a
	// planted CLAUDE.md / skills / agents in the sandbox can't influence
	// the model. The gateway may widen this (e.g. user,project) per bot;
	// empty defaults to user.
	sources := req.SettingSources
	if len(sources) == 0 {
		sources = []string{"user"}
	}
	args = append(args, "--setting-sources="+strings.Join(sources, ","))
	// Always emit --system-prompt (even with an empty
	// value) so a missing prompt is loud, not a silent fallback to
	// claude's built-in default that would drop SecurityPrefix.
	args = append(args, "--system-prompt", req.System.Flatten())
	// --tools is the surface-restrict flag: the model only sees the listed
	// names. "" disables every built-in. nil = caller expressed no opinion,
	// so we resolve the headless-safe set probed from THIS binary (minus
	// interactiveExclusions). If the probe is unavailable we fall back to the
	// CLI's own "default" set rather than a hand-maintained Go list — a
	// degraded path that DOES re-admit interactive tools, but it only applies
	// during a transient probe-failure window (a binary that can't be probed
	// generally can't run a turn either) and self-heals once the probe
	// succeeds (toolProbeRetryInterval).
	switch {
	case req.AllowedTools == nil:
		safe := d.headlessTools()
		if len(safe) > 0 {
			args = append(args, "--tools", strings.Join(withMCPWildcard(safe, mcpActive), ","))
		} else {
			// Probe unavailable (transient failure / empty filter): fall back to
			// the CLI's own "default" set. Do NOT substitute "mcp__*" here — that
			// would muzzle every built-in tool to MCP-only for the probe-retry
			// window. The CLI default already includes MCP tools when --mcp-config
			// is loaded, so the degraded path stays usable.
			args = append(args, "--tools", "default")
		}
	case len(req.AllowedTools) == 0:
		args = append(args, "--tools", "")
	default:
		// An operator/desktop-supplied whitelist must still be subtracted
		// against interactiveExclusions — a headless `-p` turn has no TTY to
		// answer an interactive tool's prompt, so a whitelisted AskUserQuestion
		// / EnterPlanMode would stall the turn until the idle timeout. The
		// denylist is enforced here too, not only on the probed default.
		//
		// mcp__* is deliberately NOT auto-added to an explicit whitelist: the
		// whitelist is the operator's exact surface for this channel, and a
		// per-bot .mcp.json is a single file shared across ALL channels, so
		// auto-admitting mcp__* would silently re-grant every MCP server tool in
		// a channel the operator scoped to (say) read-only. An operator who wants
		// MCP in a channel lists "mcp__*" (or "mcp__<server>__*") themselves.
		args = append(args, "--tools", strings.Join(filterTools(req.AllowedTools), ","))
	}
	return args
}

// withMCPWildcard appends "mcp__*" to the PROBED DEFAULT tool surface when MCP
// is active for the turn, so a bot with a .mcp.json but no explicit tool policy
// can actually call its configured servers (ProbeTools runs without
// --mcp-config, so the probed set never carries mcp__* names). It is applied
// ONLY to the nil-policy default — never to an explicit operator whitelist,
// where mcp__* admission is the operator's decision (see appendHeadlessArgs).
// No-op when mcp is inactive or the list already has
// an mcp__* glob. Returns a fresh slice; does not mutate the input.
func withMCPWildcard(tools []string, mcpActive bool) []string {
	if !mcpActive || slices.Contains(tools, "mcp__*") {
		return tools
	}
	return append(slices.Clone(tools), "mcp__*")
}

// headlessTools returns the headless-safe tool surface for the driver's
// current binary — the probed available set minus interactiveExclusions —
// caching the result per binary path. A nil/empty return means the probe is
// unavailable; the caller falls back to the CLI's own "default" tool set.
// Successful probes are cached until the binary path changes (background
// install) or the daemon restarts (in-place upgrade); FAILED probes expire
// after toolProbeRetryInterval so a transient failure self-heals.
//
// The probe runs OUTSIDE the lock: two concurrent first-turns on the same bot
// may both probe (idempotent, ~1s), which is far cheaper than serializing
// every concurrent turn behind one goroutine holding the lock across a 30s
// spawn.
func (d *ClaudeDriver) headlessTools() []string {
	bin := d.binPath()

	d.toolsMu.Lock()
	if d.toolsCache == nil {
		d.toolsCache = map[string]toolProbe{}
	}
	if p, ok := d.toolsCache[bin]; ok && (p.ok || time.Since(p.at) < toolProbeRetryInterval) {
		d.toolsMu.Unlock()
		return p.tools
	}
	d.toolsMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	available, err := probeToolsFn(ctx, bin, d.queryEnv())

	d.toolsMu.Lock()
	defer d.toolsMu.Unlock()
	if err != nil {
		clog.For("claude").Warn("tool probe failed; falling back to CLI default tool set",
			"bin", bin, "err", err)
		d.toolsCache[bin] = toolProbe{ok: false, at: time.Now()}
		return nil
	}
	safe := filterTools(available)
	// A successful probe that filters to an empty set (a release that reported
	// no tools, or whose entire surface is in interactiveExclusions) would,
	// if cached as ok, pin the bot to the degraded CLI-default fallback for the
	// daemon's lifetime — the cache-hit branch returns p.tools unconditionally
	// for ok entries. Cache it as a (retryable) failure instead so it expires
	// after toolProbeRetryInterval and self-heals once the binary recovers.
	if len(safe) == 0 {
		d.toolsCache[bin] = toolProbe{ok: false, at: time.Now()}
		return nil
	}
	d.toolsCache[bin] = toolProbe{tools: safe, ok: true, at: time.Now()}
	return safe
}

// filterTools drops interactiveExclusions from a probed tool set, yielding
// the headless-safe surface. Order is preserved.
func filterTools(tools []string) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		if !interactiveExclusions[t] {
			out = append(out, t)
		}
	}
	return out
}

// HeadlessSafeTools is the exported view of filterTools: given a probed
// available set (from ProbeTools), it returns the headless-safe subset the
// driver would offer by default — the same set the desktop's tool picker
// should present. Centralizes the interactive-tool denylist so the GUI and the
// driver never diverge.
func HeadlessSafeTools(available []string) []string {
	return filterTools(available)
}

// MCPServerStatus is one MCP server's health as reported by the claude
// `system/init` line: Name + Status ("connected" | "failed" | …) and the
// mcp__<name>__* tools it contributed (empty when failed). Drives the
// desktop's MCP "test connection" feature.
type MCPServerStatus struct {
	Name   string   `json:"name"`
	Status string   `json:"status"`
	Tools  []string `json:"tools"`
}

// initInfo is the subset of the claude `system/init` stream-json line the
// probes consume.
type initInfo struct {
	Tools      []string
	MCPServers []MCPServerStatus
}

// probeInit spawns the claude binary headlessly with extraArgs, reads the
// FIRST `system/init` stream-json line (emitted BEFORE any API call — no
// spend), kills the process, and returns the parsed tool surface + MCP server
// statuses. The shared core of ProbeTools / ProbeMCP. env is layered onto the
// process environment the same way Query does (so a per-bot CLAUDE_CONFIG_DIR
// is reflected). cwd, when non-empty, sets the spawn's working directory so an
// MCP server with a cwd-relative command/path resolves the same way a real turn
// (which runs in the session sandbox) would; "" inherits the daemon's cwd.
//
// NOTE: probeInit always passes --setting-sources=user, matching the user-scope
// .mcp.json load path every turn uses.
func probeInit(ctx context.Context, bin, cwd string, env, extraArgs []string) (initInfo, error) {
	args := append([]string{
		"-p", "-", "--output-format", "stream-json", "--verbose",
		"--permission-mode", "bypassPermissions", "--setting-sources=user",
		"--system-prompt", "",
	}, extraArgs...)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = mergedEnv(env)
	if cwd != "" {
		cmd.Dir = cwd
	}
	cmd.Stdin = strings.NewReader("probe")
	// Run claude as its own process-group leader so the deferred teardown can
	// kill the WHOLE tree — claude reads the init line and we kill it early,
	// long before it would gracefully stop the MCP servers it spawned, so those
	// servers would otherwise orphan to init and run forever.
	setProcessGroup(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return initInfo{}, fmt.Errorf("probe stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		return initInfo{}, fmt.Errorf("probe start %s: %w", bin, err)
	}
	defer func() {
		killProcessGroup(cmd)
		_ = cmd.Wait()
	}()

	sc := newLineScanner(stdout)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var init struct {
			Type       string   `json:"type"`
			Subtype    string   `json:"subtype"`
			Tools      []string `json:"tools"`
			MCPServers []struct {
				Name   string `json:"name"`
				Status string `json:"status"`
			} `json:"mcp_servers"`
		}
		if json.Unmarshal([]byte(line), &init) != nil || init.Type != "system" || init.Subtype != "init" {
			continue
		}
		out := initInfo{Tools: init.Tools}
		for _, s := range init.MCPServers {
			out.MCPServers = append(out.MCPServers, MCPServerStatus{
				Name:   s.Name,
				Status: s.Status,
				Tools:  mcpToolsForServer(s.Name, init.Tools),
			})
		}
		return out, nil
	}
	if err := sc.Err(); err != nil {
		return initInfo{}, fmt.Errorf("probe scan: %w", err)
	}
	return initInfo{}, fmt.Errorf("no system/init line from %s", bin)
}

// mcpToolsForServer returns the mcp__<server>__* tool names from the flat tool
// list attributable to one MCP server.
func mcpToolsForServer(server string, tools []string) []string {
	prefix := "mcp__" + server + "__"
	var out []string
	for _, t := range tools {
		if strings.HasPrefix(t, prefix) {
			out = append(out, t)
		}
	}
	return out
}

// ProbeTools spawns the claude binary headlessly and returns the built-in
// tool names it actually offers, read from the `system/init` stream-json line
// the CLI emits BEFORE any API call. It makes NO API request: it reads the
// first init line and kills the process. The returned names are the
// authoritative tool surface for `bin` (which drifts per claude release), so
// the driver sources its headless default from this rather than a
// hand-maintained constant. env is layered onto the process environment the
// same way Query does, so a per-bot CLAUDE_CONFIG_DIR (and thus its MCP
// servers) is reflected in the result.
func ProbeTools(ctx context.Context, bin string, env []string) ([]string, error) {
	info, err := probeInit(ctx, bin, "", env, []string{"--tools", "default"})
	if err != nil {
		return nil, err
	}
	return info.Tools, nil
}

// ProbeMCP spawns the claude binary with the given .mcp.json loaded and
// returns each configured MCP server's health (connected/failed) plus the
// tools it contributed — the backend for the desktop's MCP "test connection"
// feature. It exercises claude's real user-scope MCP-load path (--mcp-config +
// --strict-mcp-config, tool surface scoped to mcp__*), so the reported status
// matches what a real turn would see. No API spend. A server
// reporting a non-"connected" status (with empty Tools) is the failure signal.
//
// cwd should be the session sandbox dir a real turn would run in, so a server
// whose command/args resolve cwd-relative paths is probed under the same
// working directory; "" inherits the daemon's cwd.
func ProbeMCP(ctx context.Context, bin, cwd string, env []string, mcpConfigPath string) ([]MCPServerStatus, error) {
	info, err := probeInit(ctx, bin, cwd, env, []string{
		"--tools", "mcp__*", "--mcp-config", mcpConfigPath, "--strict-mcp-config",
	})
	if err != nil {
		return nil, err
	}
	return info.MCPServers, nil
}

func (d *ClaudeDriver) Query(ctx context.Context, req Request) (<-chan AgentEvent, error) {
	cmd := d.buildCommand(ctx, req)
	sessionID := req.SessionID
	return streamCommand(ctx, cmd, "claude",
		func(line string) []AgentEvent { return tagPoisonedResume(parseClaudeLine(line), sessionID) },
		func(line string) AgentEvent { return stderrLineEvent(line, sessionID) },
	)
}

func stderrLineEvent(line, sessionID string) AgentEvent {
	if sessionID != "" && strings.Contains(line, "No conversation found with session ID") {
		return AgentEvent{Kind: KindError, Err: line, Recoverable: true, ResumeInvalid: true, Raw: line}
	}
	if isTransientUpstream(line) {
		return AgentEvent{Kind: KindError, Err: line, Recoverable: true, Transient: true, RetryHint: retryHint(line), Raw: line}
	}
	return AgentEvent{Kind: KindError, Err: line, Recoverable: true, Raw: line}
}

func (d *ClaudeDriver) buildCommand(ctx context.Context, req Request) *exec.Cmd {
	cmd := newAgentCmd(ctx, d.binPath(), d.buildArgs(req), req, d.queryEnv())
	d.selfcheckOnce.Do(func() { d.logSelfcheck(cmd.Env, req) })
	return cmd
}

// ResolveBin exposes the per-turn resolved binary path (binPath) so callers
// that drive an out-of-turn probe (e.g. the daemon's MCP health check via
// ProbeMCP) use the same binary a real Query would.
func (d *ClaudeDriver) ResolveBin() string { return d.binPath() }
