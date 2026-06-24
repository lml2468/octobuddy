package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
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
