package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lml2468/octobuddy/core/clog"
)

// PromptMode controls how ClaudeDriver assembles the system prompt and
// tool surface. Minimal aligns with the Agent SDK's reference invocation
// (replace the built-in prompt, silence cwd config, explicit tool
// whitelist). claude_code keeps Claude Code's built-in preamble and
// blanket tool access — escape hatch for SOUL.md authored against the
// preamble.
type PromptMode string

const (
	// PromptModeMinimal is the default: Request.SystemPrompt REPLACES
	// the built-in system prompt; cwd .claude/ is not auto-loaded; the
	// tool surface is the whitelist in Request.AllowedTools (or the
	// driver default if nil). Interactive tools are always disallowed.
	// Runs under bypassPermissions (headless has no approver) — the
	// --tools whitelist, not the permission mode, is what scopes capability.
	PromptModeMinimal PromptMode = "minimal"
	// PromptModeClaudeCode preserves the previous behavior: prompt is
	// APPENDED to the built-in one, cwd .claude/ auto-loads, and the full
	// built-in tool set is granted (no --tools whitelist). Like minimal it
	// runs under bypassPermissions. Use only for bots whose SOUL.md was
	// authored assuming Claude Code's preamble.
	PromptModeClaudeCode PromptMode = "claude_code"
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
	// Bin is the claude executable. Default "claude" on PATH; set to an
	// absolute path to pin a specific install.
	Bin string
	// BinFn, when set, overrides Bin per-Query. Lets the daemon refresh
	// the resolved path on every turn so a freshly-landed background
	// install (~/.octobuddy/bin/claude from the desktop's claudecli) is
	// picked up on the next user message — without waiting for restart.
	BinFn func() string
	// Mode selects between minimal (default) and claude_code prompt modes.
	Mode PromptMode
	// ExtraArgs are appended verbatim.
	ExtraArgs []string
	// Env are extra KEY=VALUE entries layered onto os.Environ for the spawned
	// CLI (e.g. ANTHROPIC_BASE_URL, OCTO_BOT_ID, GH_TOKEN).
	Env []string
	// EnvFn, when set, is evaluated on every Query to build the extra env,
	// overriding the static Env.
	EnvFn func() []string
	// MCPConfigFn, when set, is evaluated per Query to resolve the path to
	// this bot's .mcp.json (under its CLAUDE_CONFIG_DIR). A non-empty result
	// adds `--mcp-config <path> --strict-mcp-config` so the bot's configured
	// MCP servers load (and only those). Resolved per turn so a freshly-
	// written .mcp.json takes effect on the next message without a restart;
	// return "" when the file is absent.
	MCPConfigFn func() string

	// selfcheckOnce gates a one-time diagnostic line emitted on the
	// FIRST Query: claude path resolution, masked ANTHROPIC_AUTH_TOKEN,
	// ANTHROPIC_BASE_URL, CLAUDE_CONFIG_DIR + writability, effective
	// PromptMode + allowed-tools count. Single most useful line for
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
	return &ClaudeDriver{Bin: bin, Mode: PromptModeMinimal}
}

func (d *ClaudeDriver) Name() string { return "claude" }

func (d *ClaudeDriver) Capabilities() Capabilities {
	return Capabilities{Streaming: true, Resume: true, ToolEvents: true}
}

// mode returns d.Mode, defaulting to PromptModeMinimal when unset so a
// zero-value ClaudeDriver stays headless-safe.
func (d *ClaudeDriver) mode() PromptMode {
	if d.Mode == PromptModeClaudeCode {
		return PromptModeClaudeCode
	}
	return PromptModeMinimal
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
	// whether MCP is active this turn (the minimal-mode default whitelist must
	// admit mcp__* so configured servers are actually callable).
	mcpPath := ""
	if d.MCPConfigFn != nil {
		mcpPath = d.MCPConfigFn()
	}
	if d.mode() == PromptModeMinimal {
		args = d.appendMinimalModeArgs(args, req, mcpPath != "")
	} else {
		args = d.appendClaudeCodeModeArgs(args, req)
	}
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

// appendMinimalModeArgs emits the SDK-aligned flag set: replace the
// system prompt, silence project/local config so a planted CLAUDE.md /
// skills / agents under the sandbox cwd can't influence the model,
// keep user-scope so per-bot skills under CLAUDE_CONFIG_DIR still load,
// restrict the tool SURFACE via --tools (not --allowedTools, which is
// the auto-approve list under prompt-based modes and does not actually
// scope what the model can see — confirmed against claude 2.1.187).
//
// Permission mode is bypassPermissions even in minimal mode: headless
// `-p` has no TTY to answer approval prompts and we pass no --allowedTools
// auto-approve list, so under --permission-mode default (or any
// prompt-based mode) the CLI auto-denies — or hangs on — every
// write-class tool (Bash/Write/Edit), silently breaking the turn. The
// tool SURFACE is scoped by --tools, which is orthogonal to the
// permission mode, so capability restriction is unaffected.
func (d *ClaudeDriver) appendMinimalModeArgs(args []string, req Request, mcpActive bool) []string {
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
	// Always emit --system-prompt in minimal mode (even with an empty
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
// where mcp__* admission is the operator's decision (see appendMinimalModeArgs /
// appendClaudeCodeModeArgs). No-op when mcp is inactive or the list already has
// an mcp__* glob. Returns a fresh slice; does not mutate the input.
func withMCPWildcard(tools []string, mcpActive bool) []string {
	if !mcpActive || slices.Contains(tools, "mcp__*") {
		return tools
	}
	return append(slices.Clone(tools), "mcp__*")
}

// appendClaudeCodeModeArgs preserves the previous behavior — append on top of
// the built-in prompt. Unlike minimal mode it does NOT replace the system
// prompt or restrict setting sources (the built-in preamble + its default
// scopes are the point of this mode). It DOES honor an explicit tool whitelist:
// the per-bot/per-channel policy must apply regardless of prompt mode, or a
// muzzle the operator set would silently grant the full surface. An unset
// policy (req.AllowedTools == nil) keeps blanket tool access (no --tools, so MCP
// tools are already included).
func (d *ClaudeDriver) appendClaudeCodeModeArgs(args []string, req Request) []string {
	args = append(args, "--permission-mode", "bypassPermissions")
	if sp := req.System.Flatten(); sp != "" {
		args = append(args, "--append-system-prompt", sp)
	}
	// nil = no opinion → leave the full built-in surface in force (no --tools;
	// MCP tools are part of it). A non-nil policy is applied, filtered against the
	// interactive denylist for the same headless-stall reason as minimal mode.
	// mcp__* is NOT auto-added: an explicit whitelist is the operator's exact
	// surface (a per-bot .mcp.json is shared across channels), so the operator
	// lists "mcp__*" themselves to allow MCP in a given channel.
	if req.AllowedTools != nil {
		args = append(args, "--tools", strings.Join(filterTools(req.AllowedTools), ","))
	}
	return args
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
// NOTE: probeInit always passes --setting-sources=user. For a claude_code-mode
// bot (which runs with the built-in setting scopes and may auto-load
// project-scope MCP), the probed health can therefore differ from a live turn;
// the probe reflects the user-scope .mcp.json load path, which is the one
// minimal-mode bots actually use.
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

	sc := newClaudeScanner(stdout)
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
// matches what a real minimal-mode turn would see. No API spend. A server
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
	stdout, stderr, err := commandPipes(cmd)
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		// On Start failure cmd.Wait never runs, so Go's normal pipe-close
		// path never triggers and these descriptors leak until the *Cmd is
		// GC'd. Under fd exhaustion or repeated start failures this
		// accumulates quickly — close them explicitly.
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("start %s: %w", d.binPath(), err)
	}

	out := make(chan AgentEvent, 64)
	var wg sync.WaitGroup
	wg.Add(2)
	var sawTurnDone atomic.Bool
	d.startReaders(ctx, stdout, stderr, req.SessionID, out, &wg, &sawTurnDone)
	waitAndClose(ctx, cmd, out, &wg, &sawTurnDone)
	return out, nil
}

func (d *ClaudeDriver) startReaders(ctx context.Context, stdout, stderr io.Reader, sessionID string, out chan<- AgentEvent, wg *sync.WaitGroup, sawTurnDone *atomic.Bool) {
	go func() {
		defer wg.Done()
		d.drainStderr(ctx, stderr, sessionID, out)
	}()
	go func() {
		defer wg.Done()
		d.drainStdout(ctx, stdout, out, sawTurnDone)
	}()
}

func waitAndClose(ctx context.Context, cmd *exec.Cmd, out chan AgentEvent, wg *sync.WaitGroup, sawTurnDone *atomic.Bool) {
	go func() {
		defer close(out)
		wg.Wait()
		if err := cmd.Wait(); err != nil {
			emitAgentEvent(ctx, out, AgentEvent{
				Kind:        KindError,
				Err:         fmt.Sprintf("claude exited: %v", err),
				Recoverable: sawTurnDone.Load(),
				Raw:         err.Error(),
			})
		}
	}()
}

func (d *ClaudeDriver) drainStderr(ctx context.Context, stderr io.Reader, sessionID string, out chan<- AgentEvent) {
	sc := newClaudeScanner(stderr)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		emitAgentEvent(ctx, out, stderrLineEvent(line, sessionID))
	}
	if err := sc.Err(); err != nil {
		emitAgentEvent(ctx, out, AgentEvent{Kind: KindError, Err: fmt.Sprintf("stderr scan: %v", err), Recoverable: true, Raw: err.Error()})
	}
}

func (d *ClaudeDriver) drainStdout(ctx context.Context, stdout io.Reader, out chan<- AgentEvent, sawTurnDone *atomic.Bool) {
	sc := newClaudeScanner(stdout)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		for _, ev := range parseClaudeLine(line) {
			if ev.Kind == KindTurnDone {
				sawTurnDone.Store(true)
			}
			emitAgentEvent(ctx, out, ev)
		}
	}
	if err := sc.Err(); err != nil {
		emitAgentEvent(ctx, out, AgentEvent{Kind: KindError, Err: fmt.Sprintf("stdout scan: %v", err), Raw: err.Error()})
	}
}

func newClaudeScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	return sc
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

func emitAgentEvent(ctx context.Context, out chan<- AgentEvent, ev AgentEvent) {
	select {
	case out <- ev:
	case <-ctx.Done():
	}
}

func (d *ClaudeDriver) buildCommand(ctx context.Context, req Request) *exec.Cmd {
	cmd := exec.CommandContext(ctx, d.binPath(), d.buildArgs(req)...)
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	}
	cmd.Stdin = strings.NewReader(req.Prompt)
	cmd.Env = mergedEnv(d.queryEnv())
	d.selfcheckOnce.Do(func() { d.logSelfcheck(cmd.Env, req) })
	// Run claude as its own process-group leader and, on context cancellation
	// (idle timeout / daemon shutdown), kill the WHOLE group — not just claude.
	// Claude spawns its configured MCP servers as child processes; the default
	// CommandContext cancel SIGKILLs only the direct child, orphaning those
	// servers to init where they linger forever. cmd.Cancel overrides that.
	setProcessGroup(cmd)
	cmd.Cancel = func() error {
		killProcessGroup(cmd)
		return nil
	}
	cmd.WaitDelay = 10 * time.Second
	return cmd
}

// binPath resolves d.Bin via BinFn when set so a background install can
// be picked up between turns.
func (d *ClaudeDriver) binPath() string {
	if d.BinFn != nil {
		if p := d.BinFn(); p != "" {
			return p
		}
	}
	return d.Bin
}

// ResolveBin exposes the per-turn resolved binary path (binPath) so callers
// that drive an out-of-turn probe (e.g. the daemon's MCP health check via
// ProbeMCP) use the same binary a real Query would.
func (d *ClaudeDriver) ResolveBin() string { return d.binPath() }

func (d *ClaudeDriver) queryEnv() []string {
	if d.EnvFn != nil {
		return d.EnvFn()
	}
	return d.Env
}

func commandPipes(cmd *exec.Cmd) (stdout, stderr io.ReadCloser, err error) {
	stdout, err = cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err = cmd.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		return nil, nil, fmt.Errorf("stderr pipe: %w", err)
	}
	return stdout, stderr, nil
}
