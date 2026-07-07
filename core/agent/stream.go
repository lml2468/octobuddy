package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// This file holds the agent-AGNOSTIC subprocess→AgentEvent plumbing shared by
// every driver that spawns a CLI and reads line-delimited output: pipe setup,
// the two drain goroutines, the line scanner, and the exit-error path. A driver
// supplies only what's specific to it — how to build the *exec.Cmd, how to parse
// one stdout line into events, and how to classify one stderr line — via
// streamCommand. Claude-specific flag/JSON logic stays in claude.go /
// claude_parse.go; a second driver (codex.go) reuses everything here.

// lineParser turns one trimmed, non-empty stdout line into zero or more events.
type lineParser func(line string) []AgentEvent

// stderrParser turns one trimmed, non-empty stderr line into a single event
// (typically a KindError, possibly flagged Transient/ResumeInvalid).
type stderrParser func(line string) AgentEvent

// streamCommand starts cmd and streams its stdout/stderr as AgentEvents until
// both pipes drain and the process exits, then closes the channel. name labels
// the process in the exit-error message ("<name> exited: …"). On a non-zero exit
// it emits a trailing KindError, marked Recoverable when at least one
// KindTurnDone was already seen (the turn produced a reply before the CLI's
// non-zero teardown). The driver is responsible for cmd's args, env, cwd, stdin,
// and process-group cancel (see buildCommand); this owns only the streaming.
func streamCommand(ctx context.Context, cmd *exec.Cmd, name string, parseOut lineParser, parseErr stderrParser) (<-chan AgentEvent, error) {
	stdout, stderr, err := commandPipes(cmd)
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		// On Start failure cmd.Wait never runs, so Go's normal pipe-close path
		// never triggers and these descriptors leak until the *Cmd is GC'd. Under
		// fd exhaustion or repeated start failures this accumulates quickly —
		// close them explicitly.
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("start %s: %w", cmd.Path, err)
	}

	out := make(chan AgentEvent, 64)
	var wg sync.WaitGroup
	wg.Add(2)
	var sawTurnDone atomic.Bool
	go func() {
		defer wg.Done()
		drainStderr(ctx, stderr, out, parseErr)
	}()
	go func() {
		defer wg.Done()
		drainStdout(ctx, stdout, out, &sawTurnDone, parseOut)
	}()
	go func() {
		defer close(out)
		wg.Wait()
		if err := cmd.Wait(); err != nil {
			emitAgentEvent(ctx, out, AgentEvent{
				Kind:        KindError,
				Err:         fmt.Sprintf("%s exited: %v", name, err),
				Recoverable: sawTurnDone.Load(),
				Raw:         err.Error(),
			})
		}
	}()
	return out, nil
}

func drainStdout(ctx context.Context, stdout io.Reader, out chan<- AgentEvent, sawTurnDone *atomic.Bool, parse lineParser) {
	sc := newLineScanner(stdout)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		for _, ev := range parse(line) {
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

func drainStderr(ctx context.Context, stderr io.Reader, out chan<- AgentEvent, parse stderrParser) {
	sc := newLineScanner(stderr)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		emitAgentEvent(ctx, out, parse(line))
	}
	if err := sc.Err(); err != nil {
		emitAgentEvent(ctx, out, AgentEvent{Kind: KindError, Err: fmt.Sprintf("stderr scan: %v", err), Recoverable: true, Raw: err.Error()})
	}
}

// newLineScanner wraps a reader in a bufio.Scanner sized for the large single
// lines a stream-json turn can emit (a 16 MiB cap; one content block can carry a
// big tool result). Shared by every line-delimited driver and the probes.
func newLineScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	return sc
}

func emitAgentEvent(ctx context.Context, out chan<- AgentEvent, ev AgentEvent) {
	select {
	case out <- ev:
	case <-ctx.Done():
	}
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

// procDriver is the process-spawning state shared by every driver that runs a
// CLI and streams line-delimited output (Claude, Codex, …): binary resolution,
// per-Query env, MCP-config path, and extra argv. Embedding it lets a new driver
// reuse binPath/queryEnv (and newAgentCmd below) instead of re-copying them — it
// supplies only its own buildArgs and line parsers.
type procDriver struct {
	// Bin is the agent executable (default: the driver's name on PATH).
	Bin string
	// BinFn, when set, resolves the binary per-Query (overrides Bin) so a
	// freshly-landed background install is picked up between turns.
	BinFn func() string
	// EnvFn / EnvSpecFn build the per-Query extra env: EnvFn returns concrete
	// KEY=VALUE entries directly; EnvSpecFn returns a neutral spec that envMap
	// (the embedding driver's EnvMapper.Env) translates. EnvFn wins when both are
	// set; StaticEnv is the fallback when neither is.
	EnvFn     func() []string
	EnvSpecFn func() EnvSpec
	StaticEnv []string
	// MCPConfigFn, when set, resolves this bot's MCP config path per-Query.
	MCPConfigFn func() string
	// ExtraArgs are appended verbatim to the spawned argv.
	ExtraArgs []string
	// envMap is the embedding driver's EnvMapper.Env, wired at construction so
	// queryEnv can map an EnvSpecFn result without the base knowing the concrete
	// driver type.
	envMap func(EnvSpec) []string
}

// binPath resolves Bin via BinFn when set, so a background install is picked up
// between turns.
func (p *procDriver) binPath() string {
	if p.BinFn != nil {
		if b := p.BinFn(); b != "" {
			return b
		}
	}
	return p.Bin
}

func (p *procDriver) queryEnv() []string {
	if p.EnvFn != nil {
		return p.EnvFn()
	}
	if p.EnvSpecFn != nil && p.envMap != nil {
		return p.envMap(p.EnvSpecFn())
	}
	return p.StaticEnv
}

// newAgentCmd builds the *exec.Cmd shared by every CLI-spawning driver: cwd,
// prompt on stdin, allowlisted env, own process group + group-kill on cancel,
// and a bounded WaitDelay. The driver may layer on extras (e.g. a one-time
// self-check) after this returns.
//
// Own process group + group-kill on cancel: the CLI may spawn MCP/helper
// subprocesses; the default CommandContext SIGKILLs only the direct child,
// orphaning them to init where they linger forever. cmd.Cancel overrides that.
func newAgentCmd(ctx context.Context, bin string, args []string, req Request, env []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, args...)
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	}
	cmd.Stdin = strings.NewReader(req.Prompt)
	cmd.Env = mergedEnv(env)
	setProcessGroup(cmd)
	cmd.Cancel = func() error {
		killProcessGroup(cmd)
		return nil
	}
	cmd.WaitDelay = 10 * time.Second
	return cmd
}
