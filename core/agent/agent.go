// Package agent defines the agent-driver abstraction that replaces the
// claude-agent-sdk binding. Each agent (Claude, Codex, Gemini, …) is driven by
// spawning its CLI / app-server and normalizing its output into a single
// AgentEvent stream. The rest of the gateway (router, session-store, cron,
// stream-relay) depends only on this package — never on a specific agent.
package agent

import (
	"context"
	"os"
)

// mergedEnv returns the process environment with `extra` (KEY=VALUE entries)
// layered on top — later entries win, so callers put overrides (e.g.
// ANTHROPIC_BASE_URL) last. A nil/empty extra returns os.Environ() unchanged.
func mergedEnv(extra []string) []string {
	if len(extra) == 0 {
		return os.Environ()
	}
	return append(os.Environ(), extra...)
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
type AgentEvent struct {
	Kind EventKind `json:"kind"`

	// Text carries assistant/thinking text for KindTextDelta / KindThinking.
	// The driver emits one event per complete content block (plain stream-json,
	// no token-level partials), so consumers append text without de-duplication.
	Text string `json:"text,omitempty"`

	// SessionID is set on KindSessionStarted (used to resume next turn).
	SessionID string `json:"session_id,omitempty"`

	// Tool fields for KindToolUse / KindToolResult.
	ToolName   string `json:"tool_name,omitempty"`
	ToolParams string `json:"tool_params,omitempty"` // truncated one-liner, for progress UI

	// Usage on KindTurnDone.
	Usage *TokenUsage `json:"usage,omitempty"`

	// Err on KindError.
	Err         string `json:"err,omitempty"`
	Recoverable bool   `json:"recoverable,omitempty"`
	// ResumeInvalid marks a KindError caused by an unknown/stale resume id (the
	// agent's stored session no longer exists, e.g. after the config dir
	// changed). The gateway clears the resume mapping and retries fresh.
	// Internal control signal — not serialized.
	ResumeInvalid bool `json:"-"`
	// Transient marks a KindError caused by an upstream rate-limit / overload /
	// usage-cap condition (HTTP 429/503/529, "overloaded", "usage limit
	// reached", …) rather than a bug in the turn. The gateway surfaces a
	// distinct "服务繁忙" reply for these so the user knows to retry later.
	// RetryHint carries the human-readable reset window the agent reported
	// ("resets at 3pm"), when one was present. Internal — not serialized.
	Transient bool   `json:"-"`
	RetryHint string `json:"-"`

	// Raw holds the original line for debugging / forward-compat.
	Raw string `json:"-"`
}

// TokenUsage is the per-turn token accounting, when the agent reports it.
type TokenUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	// CachedInputTokens is the portion of InputTokens served from the prompt
	// cache (claude's cache_read_input_tokens). Zero when unreported.
	CachedInputTokens int `json:"cached_input_tokens,omitempty"`
	// CostUSD is the agent-reported turn cost (claude's total_cost_usd). Zero
	// when unreported (e.g. subscription auth that omits cost).
	CostUSD float64 `json:"cost_usd,omitempty"`
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
