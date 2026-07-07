package agent

import (
	"context"
	"strings"
	"testing"
)

// TestCodexSeamProof drives the registered codex driver end to end against a fake
// binary emitting the real codex-cli 0.142.5 JSONL arc, proving the shared
// subprocess plumbing (streamCommand) is genuinely driver-neutral: a second
// driver produces the same AgentEvent stream the gateway consumes, with no
// Claude-specific code in the path.
func TestCodexSeamProof(t *testing.T) {
	// Fake codex: the event arc a real `codex exec --json` turn produces — thread
	// start, a shell tool call (started + completed), the final assistant message,
	// and turn completion with usage (incl. reasoning tokens).
	bin := writeFakeBin(t, `cat <<'EOF'
{"type":"thread.started","thread_id":"thr-xyz"}
{"type":"turn.started"}
{"type":"item.started","item":{"id":"item_0","type":"command_execution","command":"echo hi","status":"in_progress"}}
{"type":"item.completed","item":{"id":"item_0","type":"command_execution","command":"echo hi","status":"completed"}}
{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"hello from codex"}}
{"type":"turn.completed","usage":{"input_tokens":12,"output_tokens":3,"cached_input_tokens":4,"reasoning_output_tokens":5}}
EOF`)

	d, err := New("codex", Options{Bin: bin})
	if err != nil {
		t.Fatalf("New(codex): %v", err)
	}
	if d.Name() != "codex" {
		t.Fatalf("Name()=%q want codex", d.Name())
	}
	if caps := d.Capabilities(); !caps.Resume || !caps.ToolEvents {
		t.Fatalf("codex must advertise Resume+ToolEvents now: %+v", caps)
	}

	ch, err := d.Query(context.Background(), Request{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}

	var gotText, gotSession, gotTool string
	var sawDone bool
	var usage *TokenUsage
	for ev := range ch {
		switch ev.Kind {
		case KindSessionStarted:
			gotSession = ev.SessionID
		case KindTextDelta:
			gotText += ev.Text
		case KindToolUse:
			gotTool = ev.ToolName
		case KindTurnDone:
			sawDone = true
			usage = ev.Usage
		case KindError:
			t.Fatalf("unexpected error event: %q", ev.Err)
		}
	}

	if gotSession != "thr-xyz" {
		t.Errorf("session id = %q, want thr-xyz (thread.started must surface for resume)", gotSession)
	}
	if gotText != "hello from codex" {
		t.Errorf("text = %q, want %q", gotText, "hello from codex")
	}
	if gotTool != "Shell" {
		t.Errorf("tool = %q, want Shell (command_execution must map to a tool_use)", gotTool)
	}
	if !sawDone {
		t.Error("expected KindTurnDone")
	}
	// reasoning_output_tokens (5) folds into OutputTokens (3) → 8.
	if usage == nil || usage.InputTokens != 12 || usage.OutputTokens != 8 || usage.CachedInputTokens != 4 {
		t.Errorf("usage = %+v, want input=12 output=8 cached=4", usage)
	}
}

// TestCodexResumeArgsAndSystemPrompt pins that a resume turn uses the
// `exec resume <id>` subcommand AND that the assembled System is prepended to the
// stdin prompt (codex has no --system-prompt flag). The fake bin echoes its argv
// and stdin back as agent_message text so the test can assert both.
func TestCodexResumeArgsAndSystemPrompt(t *testing.T) {
	// Echo argv (as a marker) and stdin into a single agent_message. Using printf
	// with JSON-safe content kept simple: no quotes/newlines in the markers.
	bin := writeFakeBin(t, `ARGS="$*"
STDIN="$(cat | tr '\n' ' ')"
printf '{"type":"item.completed","item":{"type":"agent_message","text":"ARGS=%s|STDIN=%s"}}\n' "$ARGS" "$STDIN"
printf '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}\n'`)

	d, _ := New("codex", Options{Bin: bin})
	ch, err := d.Query(context.Background(), Request{
		Prompt:    "USERPROMPT",
		SessionID: "prev-thread",
		System:    SystemPrompt{Mandatory: "SECPREFIX"},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	var text string
	for ev := range ch {
		if ev.Kind == KindTextDelta {
			text += ev.Text
		}
	}
	if !strings.Contains(text, "exec resume prev-thread") {
		t.Errorf("resume turn must use `exec resume <id>`; got argv in %q", text)
	}
	if !strings.Contains(text, "SECPREFIX") || !strings.Contains(text, "USERPROMPT") {
		t.Errorf("system prompt must be prepended to stdin prompt; got %q", text)
	}
}

// TestCodexEnvMapperContract pins that the SAME neutral EnvSpec config produces
// maps to Codex's own var names (OPENAI_*/CODEX_HOME).
func TestCodexEnvMapperContract(t *testing.T) {
	d := &CodexDriver{}
	got := d.Env(EnvSpec{
		GatewayBaseURL: "https://gw/v1",
		GatewayToken:   "sk-openai",
		ConfigDir:      "/root/.codex",
	})
	want := map[string]bool{
		"OPENAI_BASE_URL=https://gw/v1": true,
		"OPENAI_API_KEY=sk-openai":      true,
		"CODEX_HOME=/root/.codex":       true,
	}
	for _, e := range got {
		delete(want, e)
	}
	if len(want) != 0 {
		t.Fatalf("Env missing %v; got %v", want, got)
	}
	if n := d.ConfigDirName(); n != ".codex" {
		t.Fatalf("ConfigDirName()=%q want .codex", n)
	}
}

// TestCodexParseErrorClassification pins the in-stream error handling: a
// transient upstream 503 is flagged Transient (→ busyReply), a turn.failed is a
// terminal-shaped error, and a plain error stays recoverable.
func TestCodexParseErrorClassification(t *testing.T) {
	trans := parseCodexLine(`{"type":"error","message":"unexpected status 503 Service Unavailable"}`)
	if len(trans) != 1 || trans[0].Kind != KindError || !trans[0].Transient {
		t.Fatalf("503 error must be a transient KindError: %+v", trans)
	}
	// A mid-turn {"type":"error"} is recoverable (a retry / turn.completed may follow).
	if !trans[0].Recoverable {
		t.Fatalf("mid-turn error must be recoverable: %+v", trans)
	}
	failed := parseCodexLine(`{"type":"turn.failed","error":{"message":"unexpected status 429 too many requests"}}`)
	if len(failed) != 1 || failed[0].Kind != KindError || !failed[0].Transient {
		t.Fatalf("turn.failed 429 must be a transient KindError: %+v", failed)
	}
	// turn.failed ENDS the turn, so it must be terminal (non-recoverable) — else
	// the gateway's consumeAgentError swallows it and replies empty instead of
	// busyReply/errorReply.
	if failed[0].Recoverable {
		t.Fatalf("turn.failed must be terminal (non-recoverable): %+v", failed)
	}
	plain := parseCodexLine(`{"type":"error","message":"some other failure"}`)
	if len(plain) != 1 || plain[0].Kind != KindError || plain[0].Transient {
		t.Fatalf("non-upstream error must be a plain KindError: %+v", plain)
	}
}

// TestCodexStderrClassification pins that the per-turn "Reading additional input
// from stdin…" noise is NOT an error, and that a resume-against-unknown-thread
// stderr line is flagged ResumeInvalid so the gateway self-heals.
func TestCodexStderrClassification(t *testing.T) {
	noise := codexStderrEvent("Reading additional input from stdin...", "prev")
	if noise.Kind != KindSystem {
		t.Fatalf("stdin-noise must be KindSystem, got %+v", noise)
	}
	bad := codexStderrEvent("Error: thread/resume: thread/resume failed: no rollout found for thread id abc", "prev")
	if bad.Kind != KindError || !bad.ResumeInvalid {
		t.Fatalf("unknown-thread resume must be ResumeInvalid, got %+v", bad)
	}
	// With no resume in flight, the same wording is just a recoverable error.
	noResume := codexStderrEvent("no rollout found for thread id abc", "")
	if noResume.ResumeInvalid {
		t.Fatalf("ResumeInvalid must require an in-flight resume id: %+v", noResume)
	}
}
