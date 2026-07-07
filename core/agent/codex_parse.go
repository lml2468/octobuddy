package agent

import (
	"encoding/json"
	"strings"
)

// codex_parse.go normalizes the OpenAI Codex CLI's `codex exec --json` output
// (JSON Lines) into AgentEvents — the Codex counterpart to claude_parse.go. The
// event shapes below were verified against codex-cli 0.142.5; anything
// unrecognized is forwarded as KindSystem so a new line type is never fatal and
// forward-compat holds.
//
// Reference shapes (codex exec --json, abbreviated, from a live turn):
//
//	{"type":"thread.started","thread_id":"019f..."}
//	{"type":"turn.started"}
//	{"type":"item.started","item":{"id":"item_0","type":"command_execution",
//	     "command":"/bin/zsh -lc 'echo hi'","status":"in_progress"}}
//	{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"hi"}}
//	{"type":"turn.completed","usage":{"input_tokens":N,"cached_input_tokens":N,
//	     "output_tokens":N,"reasoning_output_tokens":N}}
//	{"type":"error","message":"unexpected status 503 …"}
//	{"type":"turn.failed","error":{"message":"…"}}
//
// NOTE: codex delivers errors (including upstream 429/503/529) INSIDE the stdout
// JSON stream as {"type":"error"} / {"type":"turn.failed"}, NOT on stderr — so
// transient classification lives here, not in the stderr parser.
type codexLine struct {
	Type string `json:"type"`
	// ThreadID identifies the session for resume. Surfaced as KindSessionStarted
	// so the gateway persists it and passes it back via `codex exec resume <id>`.
	ThreadID string `json:"thread_id"`
	// Message carries the text of a {"type":"error"} line.
	Message string `json:"message"`
	// Error carries the nested message of a {"type":"turn.failed"} line.
	Error *codexError    `json:"error"`
	Item  *codexItem     `json:"item"`
	Usage *codexRawUsage `json:"usage"`
}

type codexError struct {
	Message string `json:"message"`
}

type codexItem struct {
	Type    string `json:"type"` // agent_message | command_execution | reasoning | …
	Text    string `json:"text"` // agent_message
	Command string `json:"command"`
	Status  string `json:"status"` // in_progress | completed | failed
}

type codexRawUsage struct {
	InputTokens           int `json:"input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens"`
	ReasoningOutputTokens int `json:"reasoning_output_tokens"`
}

// parseCodexLine maps one JSONL line to zero or more AgentEvents. An unparseable
// line is surfaced as KindSystem (Raw) rather than dropped.
func parseCodexLine(line string) []AgentEvent {
	var cl codexLine
	if err := json.Unmarshal([]byte(line), &cl); err != nil {
		return []AgentEvent{{Kind: KindSystem, Raw: line}}
	}
	switch cl.Type {
	case "thread.started":
		// The thread id is the resume handle (`codex exec resume <id>`). Surface it
		// so the gateway persists it for the next turn.
		if cl.ThreadID != "" {
			return []AgentEvent{{Kind: KindSessionStarted, SessionID: cl.ThreadID, Raw: line}}
		}
		return []AgentEvent{{Kind: KindSystem, Raw: line}}
	case "item.completed":
		return codexItemEvents(cl.Item, line)
	case "turn.completed":
		ev := AgentEvent{Kind: KindTurnDone, Raw: line}
		if cl.Usage != nil {
			ev.Usage = &TokenUsage{
				InputTokens: cl.Usage.InputTokens,
				// Codex bills reasoning tokens separately from the visible output;
				// fold them into OutputTokens so the bot's cost/usage total matches
				// what the provider charges.
				OutputTokens:      cl.Usage.OutputTokens + cl.Usage.ReasoningOutputTokens,
				CachedInputTokens: cl.Usage.CachedInputTokens,
			}
		}
		return []AgentEvent{ev}
	case "turn.failed":
		msg := ""
		if cl.Error != nil {
			msg = cl.Error.Message
		}
		// turn.failed ENDS the turn — it must be a terminal (non-recoverable)
		// error so the gateway's consumeAgentError records it and replies
		// busyReply/errorReply instead of treating the turn as a silent success.
		return []AgentEvent{codexErrorEvent(msg, line, false)}
	case "error":
		// A mid-turn {"type":"error"} may be followed by a retry and a
		// turn.completed, so it is recoverable (informational) — a terminal
		// failure arrives separately as turn.failed above.
		return []AgentEvent{codexErrorEvent(cl.Message, line, true)}
	default:
		// turn.started, item.started, item.updated, session.created, and any
		// future line type: informational. item.started is skipped so a tool call
		// yields exactly one KindToolUse (from item.completed), mirroring claude's
		// no-partial-deltas stance.
		return []AgentEvent{{Kind: KindSystem, Raw: line}}
	}
}

// codexItemEvents maps a completed item to its AgentEvent(s): assistant text →
// KindTextDelta, a shell command → KindToolUse (the desktop step card), reasoning
// → KindThinking. Unknown item types are dropped (the turn's text arrives in a
// separate agent_message item).
func codexItemEvents(item *codexItem, line string) []AgentEvent {
	if item == nil {
		return nil
	}
	switch item.Type {
	case "agent_message":
		if item.Text == "" {
			return nil
		}
		return []AgentEvent{{Kind: KindTextDelta, Text: item.Text, Raw: line}}
	case "command_execution":
		// Codex's only tool surface in exec mode is shell. Present it like a Bash
		// tool_use so the desktop step card + IM tool-progress render identically to
		// claude. summary == detail (no separate description), so the card is
		// non-expandable — matching toolSummary's no-description branch.
		params := clip(oneLine(item.Command), 120)
		detail := "Shell(" + params + ")"
		return []AgentEvent{{
			Kind:        KindToolUse,
			ToolName:    "Shell",
			ToolParams:  params,
			ToolSummary: detail,
			ToolDetail:  detail,
			Raw:         line,
		}}
	case "reasoning":
		if item.Text == "" {
			return nil
		}
		return []AgentEvent{{Kind: KindThinking, Text: item.Text, Raw: line}}
	default:
		return nil
	}
}

// codexErrorEvent classifies a codex error-stream message. recoverable selects
// whether the turn continues: a mid-turn {"type":"error"} passes true (it may be
// followed by a retry), while a terminal {"type":"turn.failed"} passes false so
// the gateway records it and replies busyReply/errorReply rather than treating
// the turn as a silent success. Both share the transient (429/503/529/overload)
// detection so a terminal upstream failure yields busyReply rather than the
// generic errorReply. codex's error text is used verbatim (its upstream is
// OpenAI-shaped, but the transient regex is provider-neutral: HTTP codes +
// "service unavailable" + "overloaded").
func codexErrorEvent(msg, line string, recoverable bool) AgentEvent {
	ev := AgentEvent{Kind: KindError, Err: msg, Recoverable: recoverable, Raw: line}
	if isTransientUpstream(msg) {
		ev.Transient = true
		ev.RetryHint = retryHint(msg)
	}
	return ev
}

// oneLine collapses runs of whitespace (incl. newlines) into single spaces so a
// multi-line shell command renders as a one-liner in the progress UI.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
