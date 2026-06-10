package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// ClaudeDriver drives Claude Code headlessly via:
//
//	claude -p <prompt> --output-format stream-json --verbose [--resume <id>] ...
//
// and normalizes its line-delimited JSON ("stream-json") into AgentEvents.
// This is the concrete proof that the CLI can replace claude-agent-sdk: the
// SDK itself merely spawns this same CLI.
type ClaudeDriver struct {
	// Bin is the claude executable (default "claude" on PATH).
	Bin string
	// ExtraArgs are appended verbatim.
	ExtraArgs []string
	// Env are extra KEY=VALUE entries layered onto os.Environ() for the spawned
	// CLI (e.g. ANTHROPIC_BASE_URL, OCTO_BOT_ID, GH_TOKEN).
	Env []string
}

func NewClaudeDriver(bin string) *ClaudeDriver {
	if bin == "" {
		bin = "claude"
	}
	return &ClaudeDriver{Bin: bin}
}

func (d *ClaudeDriver) Name() string { return "claude" }

func (d *ClaudeDriver) Capabilities() Capabilities {
	return Capabilities{Streaming: true, Resume: true, ToolEvents: true}
}

func (d *ClaudeDriver) buildArgs(req Request) []string {
	args := []string{"-p", req.Prompt, "--output-format", "stream-json", "--verbose"}
	// Headless gateway invariants (claude-only, fixed): grant all tools and
	// bypass interactive approval — there is no terminal to answer prompts, so
	// any other permission mode would hang the turn forever.
	args = append(args, "--allowedTools", "*", "--permission-mode", "bypassPermissions")
	if req.SessionID != "" {
		args = append(args, "--resume", req.SessionID)
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.SystemAppend != "" {
		args = append(args, "--append-system-prompt", req.SystemAppend)
	}
	// Pin auto-memory to the per-session dir. --settings merges into (does not
	// replace) the defaults, so project-scope skill discovery under <cwd> still
	// works. JSON-encode so a path with special chars can't break the flag.
	if req.MemoryDir != "" {
		if b, err := json.Marshal(map[string]string{"autoMemoryDirectory": req.MemoryDir}); err == nil {
			args = append(args, "--settings", string(b))
		}
	}
	args = append(args, d.ExtraArgs...)
	return args
}

func (d *ClaudeDriver) Query(ctx context.Context, req Request) (<-chan AgentEvent, error) {
	cmd := exec.CommandContext(ctx, d.Bin, d.buildArgs(req)...)
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	}
	cmd.Env = mergedEnv(d.Env)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	// Merge stderr into the same stream so transport errors surface as events
	// rather than being silently dropped (stderr is not stream-json, so the
	// parser will pass non-JSON lines through as system/error events).
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", d.Bin, err)
	}

	out := make(chan AgentEvent, 64)

	// Drain stderr in the background; emit any non-empty lines as recoverable
	// errors (e.g. node warnings) without blocking the child on a full pipe.
	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			out <- AgentEvent{Kind: KindError, Err: line, Recoverable: true, Raw: line}
		}
	}()

	go func() {
		defer close(out)
		sc := bufio.NewScanner(stdout)
		// stream-json lines can be large (tool inputs with file contents).
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			for _, ev := range parseClaudeLine(line) {
				out <- ev
			}
		}
		if err := cmd.Wait(); err != nil {
			out <- AgentEvent{
				Kind: KindError,
				Err:  fmt.Sprintf("claude exited: %v", err),
				Raw:  err.Error(),
			}
		}
	}()

	return out, nil
}

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
	Role    string         `json:"role"`
	Content []claudeBlock  `json:"content"`
	Usage   *claudeRawUsage `json:"usage"`
}

type claudeBlock struct {
	Type  string          `json:"type"` // text | tool_use | thinking | tool_result
	Text  string          `json:"text"`
	Name  string          `json:"name"`  // tool_use
	Input json.RawMessage `json:"input"` // tool_use params
}

type claudeRawUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
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
		switch cl.Subtype {
		case "init":
			// First line of a run: carries the session id to persist for resume.
			return []AgentEvent{{Kind: KindSessionStarted, SessionID: cl.SessionID, Raw: line}}
		case "api_retry":
			return []AgentEvent{{
				Kind:        KindError,
				Err:         fmt.Sprintf("api_retry status=%d: %s", cl.ErrorStatus, cl.Error),
				Recoverable: true,
				Raw:         line,
			}}
		default:
			// hook_started / hook_response / other informational system lines.
			return []AgentEvent{{Kind: KindSystem, Text: cl.Subtype, Raw: line}}
		}

	case "assistant":
		if cl.Message == nil {
			return nil
		}
		var evs []AgentEvent
		for _, b := range cl.Message.Content {
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

	case "user":
		// tool_result blocks come back as a user-role message.
		if cl.Message == nil {
			return nil
		}
		var evs []AgentEvent
		for _, b := range cl.Message.Content {
			if b.Type == "tool_result" {
				evs = append(evs, AgentEvent{Kind: KindToolResult, Raw: line})
			}
		}
		return evs

	case "result":
		ev := AgentEvent{Kind: KindTurnDone, Raw: line}
		if cl.Usage != nil {
			ev.Usage = &TokenUsage{InputTokens: cl.Usage.InputTokens, OutputTokens: cl.Usage.OutputTokens}
		}
		if cl.IsError {
			return []AgentEvent{{
				Kind:        KindError,
				Err:         fmt.Sprintf("result error (subtype=%s): %s", cl.Subtype, cl.Result),
				Recoverable: false,
				Raw:         line,
			}, ev}
		}
		return []AgentEvent{ev}

	default:
		return []AgentEvent{{Kind: KindSystem, Text: cl.Type, Raw: line}}
	}
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
		s = s[:max] + "…"
	}
	return s
}
