package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// --- stream-json line shapes (only the fields we consume) ---

type claudeLine struct {
	Type      string `json:"type"`    // system | assistant | user | result
	Subtype   string `json:"subtype"` // init | api_retry | hook_* | success | error_*
	SessionID string `json:"session_id"`

	// assistant/user
	Message *claudeMessage `json:"message"`

	// result
	Result     string          `json:"result"`
	IsError    bool            `json:"is_error"`
	TotalCost  float64         `json:"total_cost_usd"`
	Usage      *claudeRawUsage `json:"usage"`
	NumTurns   int             `json:"num_turns"`
	DurationMs int             `json:"duration_ms"`

	// system/api_retry
	Error       string `json:"error"`
	ErrorStatus int    `json:"error_status"`
}

type claudeMessage struct {
	Role    string          `json:"role"`
	Content []claudeBlock   `json:"content"`
	Usage   *claudeRawUsage `json:"usage"`
}

type claudeBlock struct {
	Type  string          `json:"type"` // text | tool_use | thinking | tool_result
	Text  string          `json:"text"`
	Name  string          `json:"name"`  // tool_use
	Input json.RawMessage `json:"input"` // tool_use params
}

type claudeRawUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// parseClaudeLine normalizes one stream-json line into zero or more AgentEvents.
// Unknown shapes degrade to a KindSystem event carrying the raw line so nothing
// is silently dropped (forward-compatible with new line types).
func parseClaudeLine(line string) []AgentEvent {
	var cl claudeLine
	if err := json.Unmarshal([]byte(line), &cl); err != nil {
		// Not JSON (e.g. a stderr line merged in) — surface as system info.
		return []AgentEvent{{Kind: KindSystem, Text: line, Raw: line}}
	}

	switch cl.Type {
	case "system":
		return parseClaudeSystemLine(cl, line)
	case "assistant":
		return parseClaudeAssistantLine(cl.Message, line)
	case "user":
		return parseClaudeUserLine(cl.Message, line)
	case "result":
		return parseClaudeResultLine(cl, line)
	default:
		return []AgentEvent{{Kind: KindSystem, Text: cl.Type, Raw: line}}
	}
}

func parseClaudeSystemLine(cl claudeLine, line string) []AgentEvent {
	switch cl.Subtype {
	case "init":
		return []AgentEvent{{Kind: KindSessionStarted, SessionID: cl.SessionID, Raw: line}}
	case "api_retry":
		msg := fmt.Sprintf("api_retry status=%d: %s", cl.ErrorStatus, cl.Error)
		ev := AgentEvent{Kind: KindError, Err: msg, Recoverable: true, Raw: line}
		if cl.ErrorStatus == 429 || cl.ErrorStatus == 503 || cl.ErrorStatus == 529 || isTransientUpstream(cl.Error) {
			ev.Transient = true
			ev.RetryHint = retryHint(cl.Error)
		}
		return []AgentEvent{ev}
	default:
		return []AgentEvent{{Kind: KindSystem, Text: cl.Subtype, Raw: line}}
	}
}

func parseClaudeAssistantLine(msg *claudeMessage, line string) []AgentEvent {
	if msg == nil {
		return nil
	}
	var evs []AgentEvent
	for _, b := range msg.Content {
		switch b.Type {
		case "text":
			if b.Text != "" {
				evs = append(evs, AgentEvent{Kind: KindTextDelta, Text: b.Text, Raw: line})
			}
		case "thinking":
			if b.Text != "" {
				evs = append(evs, AgentEvent{Kind: KindThinking, Text: b.Text, Raw: line})
			}
		case "tool_use":
			evs = append(evs, AgentEvent{
				Kind:       KindToolUse,
				ToolName:   b.Name,
				ToolParams: truncateParams(b.Input),
				Raw:        line,
			})
		}
	}
	return evs
}

func parseClaudeUserLine(msg *claudeMessage, line string) []AgentEvent {
	if msg == nil {
		return nil
	}
	var evs []AgentEvent
	for _, b := range msg.Content {
		if b.Type == "tool_result" {
			evs = append(evs, AgentEvent{Kind: KindToolResult, Raw: line})
		}
	}
	return evs
}

func parseClaudeResultLine(cl claudeLine, line string) []AgentEvent {
	ev := AgentEvent{Kind: KindTurnDone, Raw: line}
	if cl.Usage != nil {
		ev.Usage = &TokenUsage{
			InputTokens:              cl.Usage.InputTokens,
			OutputTokens:             cl.Usage.OutputTokens,
			CachedInputTokens:        cl.Usage.CacheReadInputTokens,
			CacheCreationInputTokens: cl.Usage.CacheCreationInputTokens,
			CostUSD:                  cl.TotalCost,
		}
	} else if cl.TotalCost != 0 {
		ev.Usage = &TokenUsage{CostUSD: cl.TotalCost}
	}
	if !cl.IsError {
		return []AgentEvent{ev}
	}
	errEv := AgentEvent{
		Kind:        KindError,
		Err:         fmt.Sprintf("result error (subtype=%s): %s", cl.Subtype, cl.Result),
		Recoverable: false,
		Raw:         line,
	}
	if isTransientUpstream(cl.Result) || isTransientUpstream(cl.Subtype) {
		errEv.Transient = true
		errEv.RetryHint = retryHint(cl.Result)
	}
	return []AgentEvent{errEv, ev}
}

// truncateParams renders tool input JSON as a short one-liner for progress UI.
func truncateParams(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	s := string(raw)
	s = strings.Join(strings.Fields(s), " ") // collapse whitespace/newlines
	const max = 120
	if len(s) > max {
		// Back up to a rune boundary so we never split a multibyte codepoint
		// and emit invalid UTF-8 into the progress event.
		cut := max
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}
