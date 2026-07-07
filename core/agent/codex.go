package agent

import (
	"cmp"
	"context"
	"strings"
)

// CodexDriver drives the OpenAI Codex CLI headlessly via:
//
//	codex exec --json --skip-git-repo-check --dangerously-bypass-approvals-and-sandbox [-m model] -
//	codex exec resume <thread_id> --json … -   (resume path)
//
// with the prompt fed on stdin (`-`) and its JSONL output normalized into the
// SAME AgentEvent stream the gateway consumes — reusing the shared subprocess
// plumbing (streamCommand) and the registry/Options/EnvMapper contracts, so a
// codex bot touches nothing downstream of agent.
//
// Verified against codex-cli 0.142.5. It is the ONE place that knows about the
// codex binary, its argv shape, and its env requirements (OPENAI_*/CODEX_HOME).
// The naming-behind-the-driver rule holds: Codex emits OPENAI_BASE_URL/
// OPENAI_API_KEY/CODEX_HOME from the same neutral EnvSpec Claude maps to
// ANTHROPIC_*.
//
// SCOPE / differences from ClaudeDriver, by design of the codex CLI:
//   - No `--system-prompt` flag exists; the assembled System (SecurityPrefix +
//     SOUL/AGENTS + escaped group context) is PREPENDED to the stdin prompt so the
//     non-overridable security preamble still reaches the model.
//   - No `--tools` surface flag; codex exec's only tool is shell, gated by the
//     sandbox mode. req.AllowedTools cannot be honored as a per-tool muzzle, so a
//     codex bot runs with codex's own tool model (see buildArgs' sandbox note).
//   - No MCP surface probe / ToolProber; a codex bot uses the CLI's own config.
type CodexDriver struct {
	// procDriver carries the shared process-spawning state + binPath()/queryEnv();
	// EnvSpecFn is mapped through this driver's Env() (wired as envMap in init).
	procDriver
}

func init() {
	Register("codex", func(o Options) Driver {
		d := &CodexDriver{}
		d.Bin = cmp.Or(o.Bin, "codex")
		d.BinFn = o.BinFn
		d.EnvFn = o.EnvFn
		d.EnvSpecFn = o.EnvSpecFn
		d.ExtraArgs = o.ExtraArgs
		d.envMap = d.Env
		// NOTE: o.MCPConfigFn is intentionally NOT wired — CodexDriver does not load
		// a per-bot MCP config (no --mcp-config in `codex exec`), so storing the
		// resolver would misrepresent capability. Add it here when MCP is wired.
		return d
	})
}

func (d *CodexDriver) Name() string { return "codex" }

func (d *CodexDriver) Capabilities() Capabilities {
	// Resume via `codex exec resume <thread_id>`; tool events via the
	// command_execution item stream. Streaming is per-block (no token deltas),
	// same as claude.
	return Capabilities{Streaming: true, Resume: true, ToolEvents: true}
}

// ConfigDirName isolates Codex's per-bot config under <botRoot>/.codex, the
// mirror of Claude's .claude. (agent.ConfigDirNamer)
func (d *CodexDriver) ConfigDirName() string { return ".codex" }

// Env maps the neutral EnvSpec onto Codex's env-var names — the proof that the
// env contract genuinely varies per driver from the SAME spec config produces.
// (agent.EnvMapper)
func (d *CodexDriver) Env(spec EnvSpec) []string {
	return mapEnvSpec(spec, "OPENAI_BASE_URL", "OPENAI_API_KEY", "CODEX_HOME")
}

func (d *CodexDriver) Query(ctx context.Context, req Request) (<-chan AgentEvent, error) {
	// Codex has no --system-prompt flag, so the assembled System is prepended to
	// the stdin prompt. newAgentCmd feeds req.Prompt on stdin; override it with the
	// combined text (a copy of req, so the caller's Request is untouched).
	sysReq := req
	sysReq.Prompt = codexStdin(req)
	cmd := newAgentCmd(ctx, d.binPath(), d.buildArgs(req), sysReq, d.queryEnv())
	sessionID := req.SessionID
	return streamCommand(ctx, cmd, "codex",
		parseCodexLine,
		func(line string) AgentEvent { return codexStderrEvent(line, sessionID) },
	)
}

// codexStdin builds the stdin payload: the assembled system prompt (SecurityPrefix
// + operator SOUL/AGENTS + escaped group context) followed by the user prompt.
// When System is empty (single-bot / REPL with no prompt configured), just the
// prompt is sent.
func codexStdin(req Request) string {
	sys := req.System.Flatten()
	if sys == "" {
		return req.Prompt
	}
	return sys + "\n\n" + req.Prompt
}

// buildArgs assembles the headless Codex invocation. `-` reads the prompt on
// stdin (like claude's `-p -`) so a large prompt can't hit ARG_MAX.
//
//   - --skip-git-repo-check: the per-session sandbox cwd is not a git repo, and
//     codex exec refuses to run outside one by default.
//   - --dangerously-bypass-approvals-and-sandbox: the headless analog of claude's
//     --permission-mode bypassPermissions. There is no TTY to answer codex's
//     command-approval prompts, so any sandboxed/approval mode would stall or deny
//     every shell command. The gateway already runs each turn in an isolated
//     per-session cwd; codex's own sandbox would be redundant here.
//   - resume: `codex exec resume <thread_id>` continues a prior session (the
//     thread id the gateway persisted from a KindSessionStarted event).
func (d *CodexDriver) buildArgs(req Request) []string {
	var args []string
	if req.SessionID != "" {
		args = []string{"exec", "resume", req.SessionID}
	} else {
		args = []string{"exec"}
	}
	args = append(args, "--json", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox")
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	args = append(args, d.ExtraArgs...)
	args = append(args, "-") // prompt on stdin
	return args
}

// codexStderrEvent classifies one codex stderr line. Codex delivers its real
// errors (including upstream 429/503/529) in the stdout JSON stream, so stderr
// carries only startup noise plus a couple of fatal conditions:
//   - "Reading additional input from stdin…" is emitted on every turn; it is not
//     an error, so it is surfaced as KindSystem (never KindError — the old stub
//     turned it into a spurious error each turn).
//   - A resume against an unknown thread id fails with "no rollout found for
//     thread id …" (non-zero exit). Classified as ResumeInvalid when this turn was
//     a resume, so the gateway clears the stale mapping and retries fresh — the
//     codex analog of claude's "No conversation found with session ID".
func codexStderrEvent(line, sessionID string) AgentEvent {
	if strings.Contains(line, "Reading additional input from stdin") {
		return AgentEvent{Kind: KindSystem, Raw: line}
	}
	if sessionID != "" && (strings.Contains(line, "no rollout found for thread") || strings.Contains(line, "thread/resume failed")) {
		return AgentEvent{Kind: KindError, Err: line, Recoverable: true, ResumeInvalid: true, Raw: line}
	}
	return AgentEvent{Kind: KindError, Err: line, Recoverable: true, Raw: line}
}
