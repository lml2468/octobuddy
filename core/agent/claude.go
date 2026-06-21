package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// ClaudeDriver drives Claude Code headlessly via:
//
//	claude -p - --output-format stream-json --verbose [--resume <id>] ...
//
// with the prompt fed on stdin, and normalizes its line-delimited JSON
// ("stream-json") into AgentEvents. This is the concrete proof that the CLI can
// replace claude-agent-sdk: the SDK itself merely spawns this same CLI.
//
// Output is plain stream-json (one event per complete content block); the driver
// does NOT request --include-partial-messages, so there are no token-level
// deltas to de-duplicate.
type ClaudeDriver struct {
	// Bin is the claude executable (default "claude" on PATH).
	Bin string
	// ExtraArgs are appended verbatim.
	ExtraArgs []string
	// Env are extra KEY=VALUE entries layered onto os.Environ() for the spawned
	// CLI (e.g. ANTHROPIC_BASE_URL, OCTO_BOT_ID, GH_TOKEN).
	Env []string
	// EnvFn, when set, is evaluated on every Query to build the extra env,
	// overriding the static Env. Lets a caller inject a runtime-resolved value
	// (e.g. a gateway token from the in-memory secret store) per turn.
	EnvFn func() []string
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
	// The prompt is fed on stdin (`-p -`) rather than as an argv element, so a
	// large prompt (group backfill + inlined file content) can't hit ARG_MAX.
	args := []string{"-p", "-", "--output-format", "stream-json", "--verbose"}
	// Headless gateway invariant (claude-only, fixed): bypass interactive
	// approval — there is no terminal to answer prompts, so any other permission
	// mode would hang the turn forever. bypassPermissions grants every tool, so
	// no --allowedTools is needed (and claude 2.1+ rejects "*" in allow rules,
	// which only spammed a per-turn warning; verified tools still run without it).
	args = append(args, "--permission-mode", "bypassPermissions")
	if req.SessionID != "" {
		args = append(args, "--resume", req.SessionID)
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	// --append-system-prompt is re-sent every turn (including resumes): it does
	// NOT persist across --resume, so dropping it on a resumed turn would silently
	// lose the non-overridable SecurityPrefix + SOUL identity. Its tokens are a
	// prompt-cache hit, so re-sending is cheap.
	if req.SystemAppend != "" {
		args = append(args, "--append-system-prompt", req.SystemAppend)
	}
	// Pin auto-memory to the per-session dir. --settings merges into (does not
	// replace) the defaults, so project-scope skill discovery under <cwd> still
	// works. JSON-encode so a path with special chars can't break the flag.
	//
	// Contract: req.MemoryDir MUST live OUTSIDE req.Cwd (sandbox.ResolveMemoryDir
	// computes it under a separate memoryBase). That is the safety property — an
	// agent with write access to its cwd must not be able to author the memory that
	// is later injected as trusted context. The driver trusts the caller to honor
	// this; the gateway derives the two from disjoint bases (gateway.resolveSandbox).
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
	// Feed the prompt on stdin (matches `-p -`). This is a private in-memory
	// reader holding ONLY the prompt — never os.Stdin, which on the daemon
	// carries the control-bus capability token (MLT-40). os/exec copies it to the
	// child in a goroutine and closes the pipe at EOF.
	cmd.Stdin = strings.NewReader(req.Prompt)
	extraEnv := d.Env
	if d.EnvFn != nil {
		extraEnv = d.EnvFn()
	}
	cmd.Env = mergedEnv(extraEnv)
	// On ctx cancellation, CommandContext kills the process; WaitDelay bounds how
	// long Wait then blocks if a grandchild keeps the output pipe open, so the
	// reader goroutines can't hang the turn indefinitely.
	cmd.WaitDelay = 10 * time.Second

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	// Merge stderr into the same stream so transport errors surface as events
	// rather than being silently dropped (stderr is not stream-json, so the
	// parser will pass non-JSON lines through as system/error events).
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		// On Start failure cmd.Wait never runs, so Go's normal pipe-close
		// path never triggers and these descriptors leak until the *Cmd is
		// GC'd. Under fd exhaustion or repeated start failures this
		// accumulates quickly — close them explicitly.
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("start %s: %w", d.Bin, err)
	}

	out := make(chan AgentEvent, 64)

	// Two readers feed `out`; a WaitGroup joins them so the channel is closed
	// exactly once, AFTER both have finished. Closing from a single reader (e.g.
	// stdout) while the other (stderr) is still sending would panic on a send to
	// a closed channel — and calling cmd.Wait before both pipes are fully drained
	// violates the exec contract.
	var wg sync.WaitGroup
	wg.Add(2)

	// sawTurnDone records whether claude emitted a turn-final `result` line. It
	// gates how we treat a non-zero process exit below: a turn's terminal signal
	// is the `result` (is_error), NOT the exit code. claude can stream a complete,
	// successful reply + result and THEN exit non-zero for an unrelated reason
	// (post-run hook/telemetry failure, a broken stderr pipe, a node warning
	// escalating the exit status). The WaitGroup join gives a happens-before edge
	// from the stdout reader's stores to the cmd.Wait reader's load.
	var sawTurnDone atomic.Bool

	// emit sends an event unless the turn's context is cancelled. Selecting on
	// ctx.Done() means an abandoned/cancelled consumer can't wedge a reader on a
	// full channel (which would leak the goroutine and the claude subprocess).
	emit := func(ev AgentEvent) {
		select {
		case out <- ev:
		case <-ctx.Done():
		}
	}

	// Drain stderr; emit any non-empty lines as recoverable errors (e.g. node
	// warnings) without blocking the child on a full pipe.
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stderr)
		// Match the stdout cap (16 MiB): a verbose stack trace / overload message
		// that classification depends on can exceed the default 64 KiB token, and a
		// dropped line would silently downgrade a transient error to the generic one.
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			// A stale --resume id makes claude abort at session init with
			// "No conversation found with session ID: …". Tag it so the gateway
			// can clear the resume mapping and retry the turn fresh.
			if req.SessionID != "" && strings.Contains(line, "No conversation found with session ID") {
				emit(AgentEvent{Kind: KindError, Err: line, Recoverable: true, ResumeInvalid: true, Raw: line})
				continue
			}
			// Upstream rate-limit / overload printed on stderr: tag it transient so
			// the gateway can reply "服务繁忙" instead of a generic error.
			if isTransientUpstream(line) {
				emit(AgentEvent{Kind: KindError, Err: line, Recoverable: true, Transient: true, RetryHint: retryHint(line), Raw: line})
				continue
			}
			emit(AgentEvent{Kind: KindError, Err: line, Recoverable: true, Raw: line})
		}
		// A scan error (e.g. a line exceeding the buffer cap) would otherwise be
		// swallowed; surface it as recoverable so it isn't silently lost.
		if err := sc.Err(); err != nil {
			emit(AgentEvent{Kind: KindError, Err: fmt.Sprintf("stderr scan: %v", err), Recoverable: true, Raw: err.Error()})
		}
	}()

	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stdout)
		// stream-json lines can be large (tool inputs with file contents).
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			for _, ev := range parseClaudeLine(line) {
				if ev.Kind == KindTurnDone {
					sawTurnDone.Store(true)
				}
				emit(ev)
			}
		}
		// Surface a scan error (e.g. an over-cap line) as terminal: a truncated
		// stream-json line means we may have lost the result, so don't let it pass
		// silently as a successful empty turn.
		if err := sc.Err(); err != nil {
			emit(AgentEvent{Kind: KindError, Err: fmt.Sprintf("stdout scan: %v", err), Raw: err.Error()})
		}
	}()

	go func() {
		defer close(out)
		wg.Wait()
		if err := cmd.Wait(); err != nil {
			// A non-zero exit AFTER a completed turn is not a turn failure: the
			// reply + result already streamed, so surface it as recoverable
			// (informational) and let the assembled reply stand. Only an exit
			// with NO prior result is terminal — claude died before answering.
			emit(AgentEvent{
				Kind:        KindError,
				Err:         fmt.Sprintf("claude exited: %v", err),
				Recoverable: sawTurnDone.Load(),
				Raw:         err.Error(),
			})
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
		switch cl.Subtype {
		case "init":
			// First line of a run: carries the session id to persist for resume.
			return []AgentEvent{{Kind: KindSessionStarted, SessionID: cl.SessionID, Raw: line}}
		case "api_retry":
			// claude is retrying upstream; classify rate-limit/overload as transient
			// so a terminal failure after exhausted retries reads as "服务繁忙".
			msg := fmt.Sprintf("api_retry status=%d: %s", cl.ErrorStatus, cl.Error)
			ev := AgentEvent{Kind: KindError, Err: msg, Recoverable: true, Raw: line}
			if cl.ErrorStatus == 429 || cl.ErrorStatus == 503 || cl.ErrorStatus == 529 || isTransientUpstream(cl.Error) {
				ev.Transient = true
				ev.RetryHint = retryHint(cl.Error)
			}
			return []AgentEvent{ev}
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
		if cl.IsError {
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
