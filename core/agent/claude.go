package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// ClaudeDriver drives Claude Code headlessly via:
//
//	claude -p - --output-format stream-json --verbose [--resume <id>]...
//
// with the prompt fed on stdin, and normalizes its line-delimited JSON
// ("stream-json") into AgentEvents. This is the concrete proof that the CLI can
// replace claude-agent-sdk: the SDK itself merely spawns this same CLI.
//
// Output is plain stream-json (one event per complete content block); the driver
// does NOT request --include-partial-messages, so there are no token-level
// deltas to de-duplicate.
//
// It is the ONE place that knows about the claude binary, its argv shape, and
// its env requirements (ANTHROPIC_*, CLAUDE_CONFIG_DIR). Keep agent-agnostic
// policy out of it.
type ClaudeDriver struct {
	// Bin is the claude executable (default "claude" on PATH).
	Bin string
	// ExtraArgs are appended verbatim.
	ExtraArgs []string
	// Env are extra KEY=VALUE entries layered onto os.Environ for the spawned
	// CLI (e.g. ANTHROPIC_BASE_URL, OCTO_BOT_ID, GH_TOKEN).
	Env []string
	// EnvFn, when set, is evaluated on every Query to build the extra env,
	// overriding the static Env. Lets a caller inject a runtime-resolved value
	// (e.g. a gateway token from the in-memory secret store) per turn.
	EnvFn func() []string

	// selfcheckOnce gates a one-time diagnostic line (claude path resolution,
	// presence of ANTHROPIC_AUTH_TOKEN with masked value, ANTHROPIC_BASE_URL,
	// CLAUDE_CONFIG_DIR + writability) emitted on the FIRST Query. This is the
	// single most useful line for diagnosing "出错了，请稍后重试" from a fresh
	// install — auth=UNSET / claude=MISSING / cwd writable=false each map to a
	// specific operator mistake and the user can paste the line to support
	// instead of describing symptoms. Per-turn is too noisy and per-driver-
	// startup is too early (the gateway-token secret is injected over the bus
	// AFTER the driver is constructed), so we sample on the first real spawn.
	selfcheckOnce sync.Once
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
		return nil, fmt.Errorf("start %s: %w", d.Bin, err)
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
	cmd := exec.CommandContext(ctx, d.Bin, d.buildArgs(req)...)
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	}
	cmd.Stdin = strings.NewReader(req.Prompt)
	cmd.Env = mergedEnv(d.queryEnv())
	d.selfcheckOnce.Do(func() { d.logSelfcheck(cmd.Env, req.Cwd) })
	cmd.WaitDelay = 10 * time.Second
	return cmd
}

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

// logSelfcheck emits one line summarizing the realized invocation environment
// for the bot's first turn. The shape is deliberately greppable + paste-able:
//
//	[selfcheck] bot=<id> claude=<path-or-MISSING:err> auth=<masked-or-UNSET> base_url=<url-or-UNSET> cwd=<path> writable=<true|false>
//
// The token is masked (first 6 + last 4) so the line is safe to paste into a
// support ticket without leaking the live key. claude=MISSING screams when the
// CLI isn't installed/on PATH; auth=UNSET screams when the gateway-token
// secret never made it into env (the actual root cause of the "出错了" report
// we got from a fresh install). Anything else worth knowing — workspace cwd
// not writable, custom base URL pointed at the wrong host — fits on the line.
func (d *ClaudeDriver) logSelfcheck(env []string, cwd string) {
	envMap := map[string]string{}
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			envMap[kv[:i]] = kv[i+1:]
		}
	}
	binStr := d.Bin
	if p, err := exec.LookPath(d.Bin); err == nil {
		binStr = p
	} else {
		binStr = "MISSING:" + err.Error()
	}
	auth := maskToken(envMap["ANTHROPIC_AUTH_TOKEN"])
	baseURL := envMap["ANTHROPIC_BASE_URL"]
	if baseURL == "" {
		baseURL = "UNSET"
	}
	botID := envMap["OCTO_BOT_ID"]
	if botID == "" {
		botID = "?"
	}
	writable := isDirWritable(cwd)
	fmt.Fprintf(os.Stderr, "[selfcheck] bot=%s claude=%s auth=%s base_url=%s cwd=%s writable=%t\n",
		botID, binStr, auth, baseURL, cwd, writable)
}

// maskToken returns a redacted form safe to log: "UNSET" if empty, the literal
// value if too short to mask meaningfully (< 10 chars), or first-6 + "..." +
// last-4 otherwise. Preserves enough surface for the operator to recognize
// which token is in play without exposing the secret.
func maskToken(s string) string {
	if s == "" {
		return "UNSET"
	}
	if len(s) < 10 {
		return "SHORT(" + s + ")"
	}
	return s[:6] + "..." + s[len(s)-4:]
}

// isDirWritable probes write access via a no-op .write-test create+remove. The
// claude CLI writes session state under CLAUDE_CONFIG_DIR and project files
// under cwd; a read-only mount (or wrong-owner dir after a HOME override)
// reproduces as a turn that fails immediately. Best-effort: returns false on
// any error including "the dir doesn't exist".
func isDirWritable(dir string) bool {
	if dir == "" {
		return false
	}
	probe := dir + "/.octobuddy-writetest"
	f, err := os.OpenFile(probe, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false
	}
	_ = f.Close()
	_ = os.Remove(probe)
	return true
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
