package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// CodexDriver drives Codex via its app-server: a long-lived subprocess speaking
// JSON-RPC over stdio (newline-delimited). This is a fundamentally different
// protocol shape than Claude's one-shot stream-json — request/response with
// out-of-band server notifications — yet it implements the SAME agent.Driver
// interface. That is the point of this file: it proves the abstraction holds
// across protocols, not just across CLIs.
//
// Modeled on Open Island's CodexAppServer.swift (the stdio read loop + JSON-RPC
// framing + backpressure drain), extended from read-only observation to active
// turn driving (thread/start → turn/start → consume notifications).
//
// Per Query() the driver: initialize → thread/start (or thread/resume) →
// turn/start → stream notifications until turn/completed → close the channel.
type CodexDriver struct {
	Bin       string
	ExtraArgs []string
}

func NewCodexDriver(bin string) *CodexDriver {
	if bin == "" {
		bin = "codex"
	}
	return &CodexDriver{Bin: bin}
}

func (d *CodexDriver) Name() string { return "codex" }

func (d *CodexDriver) Capabilities() Capabilities {
	return Capabilities{Streaming: true, Resume: true, ToolEvents: true}
}

// --- JSON-RPC framing ---

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// codexConn manages one app-server subprocess: write requests, route responses
// back to waiters by id, and forward notifications to a handler.
type codexConn struct {
	cmd    *exec.Cmd
	stdin  *bufio.Writer
	mu     sync.Mutex
	nextID int
	waits  map[int]chan rpcMessage
	onNote func(method string, params json.RawMessage)
	writeM sync.Mutex
}

func (d *CodexDriver) Query(ctx context.Context, req Request) (<-chan AgentEvent, error) {
	out := make(chan AgentEvent, 64)

	args := append([]string{"app-server"}, d.ExtraArgs...)
	cmd := exec.CommandContext(ctx, d.Bin, args...)
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	}
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start codex app-server: %w", err)
	}

	conn := &codexConn{
		cmd:    cmd,
		stdin:  bufio.NewWriter(stdinPipe),
		nextID: 1,
		waits:  make(map[int]chan rpcMessage),
	}

	// Drain stderr so a full pipe can't block the child.
	go func() {
		sc := bufio.NewScanner(stderrPipe)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
		}
	}()

	// Notification → AgentEvent translation, with per-turn usage accumulation.
	var usage *TokenUsage
	conn.onNote = func(method string, params json.RawMessage) {
		for _, ev := range translateCodexNotification(method, params, &usage) {
			out <- ev
		}
	}

	// Reader loop: route responses to waiters, notifications to onNote.
	go conn.readLoop(stdoutPipe)

	// Driver conversation runs in its own goroutine; closes out when the turn
	// completes (or errors), making Codex's long-lived duplex protocol look
	// exactly like Claude's one-shot stream from the caller's perspective.
	go func() {
		defer close(out)
		defer func() { _ = stdinPipe.Close() }()

		if err := conn.initialize(ctx); err != nil {
			out <- AgentEvent{Kind: KindError, Err: "initialize: " + err.Error()}
			return
		}

		threadID := req.SessionID
		if threadID == "" {
			id, err := conn.threadStart(ctx, req.Cwd)
			if err != nil {
				out <- AgentEvent{Kind: KindError, Err: "thread/start: " + err.Error()}
				return
			}
			threadID = id
		} else {
			if err := conn.threadResume(ctx, threadID); err != nil {
				// Stale resume: fall back to a fresh thread (mirrors Claude
				// driver's stale-resume recovery).
				id, e2 := conn.threadStart(ctx, req.Cwd)
				if e2 != nil {
					out <- AgentEvent{Kind: KindError, Err: "thread/resume+start: " + err.Error()}
					return
				}
				threadID = id
			}
		}
		out <- AgentEvent{Kind: KindSessionStarted, SessionID: threadID}

		// turn/start blocks (over RPC) until the turn completes; notifications
		// stream the agent's output meanwhile via conn.onNote.
		if err := conn.turnStart(ctx, threadID, req.Prompt); err != nil {
			out <- AgentEvent{Kind: KindError, Err: "turn/start: " + err.Error()}
			return
		}
		out <- AgentEvent{Kind: KindTurnDone, Usage: usage}
	}()

	return out, nil
}

func (c *codexConn) readLoop(stdout interface{ Read([]byte) (int, error) }) {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var msg rpcMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue // non-JSON noise
		}
		if msg.ID != nil && (msg.Result != nil || msg.Error != nil) {
			c.mu.Lock()
			ch, ok := c.waits[*msg.ID]
			if ok {
				delete(c.waits, *msg.ID)
			}
			c.mu.Unlock()
			if ok {
				ch <- msg
			}
			continue
		}
		if msg.Method != "" && c.onNote != nil {
			c.onNote(msg.Method, msg.Params)
		}
	}
}

func (c *codexConn) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	id := c.nextID
	c.nextID++
	ch := make(chan rpcMessage, 1)
	c.waits[id] = ch
	c.mu.Unlock()

	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	})
	if err != nil {
		return nil, err
	}
	c.writeM.Lock()
	_, werr := c.stdin.Write(append(body, '\n'))
	if werr == nil {
		werr = c.stdin.Flush()
	}
	c.writeM.Unlock()
	if werr != nil {
		return nil, werr
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case resp := <-ch:
		if resp.Error != nil {
			return nil, fmt.Errorf("rpc %s: %s (code %d)", method, resp.Error.Message, resp.Error.Code)
		}
		return resp.Result, nil
	}
}

func (c *codexConn) initialize(ctx context.Context) error {
	_, err := c.call(ctx, "initialize", map[string]any{
		"clientInfo": map[string]string{"name": "ccd-spike", "version": "0.0.1"},
	})
	return err
}

func (c *codexConn) threadStart(ctx context.Context, cwd string) (string, error) {
	params := map[string]any{}
	if cwd != "" {
		params["cwd"] = cwd
	}
	res, err := c.call(ctx, "thread/start", params)
	if err != nil {
		return "", err
	}
	return extractThreadID(res)
}

func (c *codexConn) threadResume(ctx context.Context, threadID string) error {
	_, err := c.call(ctx, "thread/resume", map[string]any{"threadId": threadID})
	return err
}

func (c *codexConn) turnStart(ctx context.Context, threadID, prompt string) error {
	_, err := c.call(ctx, "turn/start", map[string]any{
		"threadId": threadID,
		"input":    []map[string]any{{"type": "text", "text": prompt}},
	})
	return err
}

func extractThreadID(res json.RawMessage) (string, error) {
	var r struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return "", err
	}
	if r.Thread.ID == "" {
		return "", fmt.Errorf("thread id missing in response")
	}
	return r.Thread.ID, nil
}

// translateCodexNotification maps Codex app-server notifications onto the unified
// AgentEvent vocabulary — the exact same currency the Claude driver emits.
func translateCodexNotification(method string, params json.RawMessage, usage **TokenUsage) []AgentEvent {
	switch method {
	case "thread/started":
		var p struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
		}
		_ = json.Unmarshal(params, &p)
		return []AgentEvent{{Kind: KindSessionStarted, SessionID: p.Thread.ID, Raw: method}}

	case "item/agentMessage/delta":
		var p struct {
			Delta string `json:"delta"`
		}
		_ = json.Unmarshal(params, &p)
		if p.Delta == "" {
			return nil
		}
		return []AgentEvent{{Kind: KindTextDelta, Text: p.Delta, Raw: method}}

	case "item/reasoning/textDelta", "item/reasoning/summaryTextDelta":
		var p struct {
			Delta string `json:"delta"`
		}
		_ = json.Unmarshal(params, &p)
		if p.Delta == "" {
			return nil
		}
		return []AgentEvent{{Kind: KindThinking, Text: p.Delta, Raw: method}}

	case "item/started":
		// A new thread item began; if it's a command/tool, surface a ToolUse.
		name, params1 := codexItemTool(params)
		if name == "" {
			return nil
		}
		return []AgentEvent{{Kind: KindToolUse, ToolName: name, ToolParams: params1, Raw: method}}

	case "item/completed":
		name, _ := codexItemTool(params)
		if name == "" {
			return nil
		}
		return []AgentEvent{{Kind: KindToolResult, ToolName: name, Raw: method}}

	case "thread/tokenUsage/updated":
		var p struct {
			TokenUsage struct {
				InputTokens  int `json:"inputTokens"`
				OutputTokens int `json:"outputTokens"`
			} `json:"tokenUsage"`
		}
		if err := json.Unmarshal(params, &p); err == nil {
			*usage = &TokenUsage{InputTokens: p.TokenUsage.InputTokens, OutputTokens: p.TokenUsage.OutputTokens}
		}
		return nil

	case "error":
		var p struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(params, &p)
		return []AgentEvent{{Kind: KindError, Err: p.Message, Raw: method}}

	default:
		// turn/started, item/* progress, thread/status/changed, warnings, etc.
		return []AgentEvent{{Kind: KindSystem, Text: method, Raw: method}}
	}
}

// codexItemTool extracts a tool name + one-line params from an item/* payload,
// returning "" for non-tool items (e.g. agentMessage, reasoning).
func codexItemTool(params json.RawMessage) (string, string) {
	var p struct {
		Item struct {
			Type    string          `json:"type"`
			Command json.RawMessage `json:"command"`
			Tool    string          `json:"tool"`
			Name    string          `json:"name"`
		} `json:"item"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", ""
	}
	switch p.Item.Type {
	case "commandExecution":
		return "Shell", truncateParams(p.Item.Command)
	case "mcpToolCall":
		name := p.Item.Tool
		if name == "" {
			name = p.Item.Name
		}
		return name, ""
	case "fileChange":
		return "FileChange", ""
	default:
		return "", ""
	}
}
