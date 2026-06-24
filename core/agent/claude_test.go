package agent

import (
	"encoding/json"
	"testing"
	"time"
)

// Fixtures: real lines captured from `claude --output-format stream-json`
// (the system/init + api_retry lines are verbatim from this machine), plus
// canonical assistant/tool_use/result lines from the documented schema.
// This proves the normalizer works deterministically without a live API key.

const (
	lineInit    = `{"type":"system","subtype":"init","cwd":"/private/tmp","session_id":"ea4de374-800a-47d5-92fd-4e4f7aa54c9c","tools":["Bash","Read"],"model":"claude-opus-4-8","permissionMode":"default"}`
	lineHook    = `{"type":"system","subtype":"hook_started","hook_name":"SessionStart:startup","session_id":"ea4de374"}`
	lineRetry   = `{"type":"system","subtype":"api_retry","attempt":1,"error_status":401,"error":"authentication_failed","session_id":"ea4de374"}`
	lineText    = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hello from spike"}]},"session_id":"ea4de374"}`
	lineThink   = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","text":"let me think"}]},"session_id":"ea4de374"}`
	lineToolUse = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"ls -la","description":"list"}}]},"session_id":"ea4de374"}`
	lineToolRes = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"file1\nfile2"}]},"session_id":"ea4de374"}`
	lineResult  = `{"type":"result","subtype":"success","is_error":false,"result":"done","total_cost_usd":0.01,"usage":{"input_tokens":1200,"output_tokens":45,"cache_read_input_tokens":300},"session_id":"ea4de374"}`
	lineErrRes  = `{"type":"result","subtype":"error_max_turns","is_error":true,"result":"hit max turns","session_id":"ea4de374"}`
	lineGarbage = `node: warning something on stderr`
)

func TestParseInitYieldsSessionStarted(t *testing.T) {
	evs := parseClaudeLine(lineInit)
	if len(evs) != 1 || evs[0].Kind != KindSessionStarted {
		t.Fatalf("want 1 session_started, got %+v", evs)
	}
	if evs[0].SessionID != "ea4de374-800a-47d5-92fd-4e4f7aa54c9c" {
		t.Fatalf("session id not extracted: %q", evs[0].SessionID)
	}
}

func TestParseHookIsSystem(t *testing.T) {
	evs := parseClaudeLine(lineHook)
	if len(evs) != 1 || evs[0].Kind != KindSystem {
		t.Fatalf("want system, got %+v", evs)
	}
}

func TestParseRetryIsRecoverableError(t *testing.T) {
	evs := parseClaudeLine(lineRetry)
	if len(evs) != 1 || evs[0].Kind != KindError || !evs[0].Recoverable {
		t.Fatalf("want recoverable error, got %+v", evs)
	}
}

func TestParseAssistantText(t *testing.T) {
	evs := parseClaudeLine(lineText)
	if len(evs) != 1 || evs[0].Kind != KindTextDelta || evs[0].Text != "hello from spike" {
		t.Fatalf("want text delta, got %+v", evs)
	}
}

func TestParseThinking(t *testing.T) {
	evs := parseClaudeLine(lineThink)
	if len(evs) != 1 || evs[0].Kind != KindThinking {
		t.Fatalf("want thinking, got %+v", evs)
	}
}

func TestParseToolUse(t *testing.T) {
	evs := parseClaudeLine(lineToolUse)
	if len(evs) != 1 || evs[0].Kind != KindToolUse {
		t.Fatalf("want tool_use, got %+v", evs)
	}
	if evs[0].ToolName != "Bash" {
		t.Fatalf("tool name not extracted: %q", evs[0].ToolName)
	}
	if evs[0].ToolParams == "" {
		t.Fatalf("tool params should be a non-empty one-liner")
	}
}

func TestParseToolResult(t *testing.T) {
	evs := parseClaudeLine(lineToolRes)
	if len(evs) != 1 || evs[0].Kind != KindToolResult {
		t.Fatalf("want tool_result, got %+v", evs)
	}
}

func TestParseResultSuccessCarriesUsage(t *testing.T) {
	evs := parseClaudeLine(lineResult)
	if len(evs) != 1 || evs[0].Kind != KindTurnDone {
		t.Fatalf("want turn_done, got %+v", evs)
	}
	u := evs[0].Usage
	if u == nil || u.OutputTokens != 45 {
		t.Fatalf("usage not extracted: %+v", u)
	}
	if u.InputTokens != 1200 {
		t.Fatalf("input tokens not extracted: %+v", u)
	}
	if u.CachedInputTokens != 300 {
		t.Fatalf("cached input tokens not extracted: %+v", u)
	}
	if u.CostUSD != 0.01 {
		t.Fatalf("cost not extracted: %+v", u)
	}
}

// TestParseResultTransientErrorIsTagged covers an upstream rate-limit surfacing
// as a result is_error: it must yield a terminal [error, turn_done] where the
// error is flagged Transient so the gateway can reply "服务繁忙".
func TestParseResultTransientErrorIsTagged(t *testing.T) {
	line := `{"type":"result","subtype":"error","is_error":true,"result":"Claude usage limit reached, resets at 3pm (PST)","session_id":"x"}`
	evs := parseClaudeLine(line)
	if len(evs) != 2 || evs[0].Kind != KindError || evs[1].Kind != KindTurnDone {
		t.Fatalf("want [error, turn_done], got %+v", evs)
	}
	if evs[0].Recoverable {
		t.Fatalf("a result error is terminal, not recoverable")
	}
	if !evs[0].Transient {
		t.Fatalf("usage-limit result error must be tagged transient: %+v", evs[0])
	}
	if evs[0].RetryHint != "3pm (PST)" {
		t.Fatalf("retry hint = %q, want %q", evs[0].RetryHint, "3pm (PST)")
	}
}

// TestParseApiRetryRateLimitIsTransient covers an api_retry on HTTP 429 being
// tagged transient (so an eventual terminal failure reads as capacity).
func TestParseApiRetryRateLimitIsTransient(t *testing.T) {
	line := `{"type":"system","subtype":"api_retry","attempt":2,"error_status":429,"error":"rate_limit_error","session_id":"x"}`
	evs := parseClaudeLine(line)
	if len(evs) != 1 || evs[0].Kind != KindError || !evs[0].Recoverable {
		t.Fatalf("want recoverable error, got %+v", evs)
	}
	if !evs[0].Transient {
		t.Fatalf("429 api_retry must be tagged transient: %+v", evs[0])
	}
}

// TestParseStreamEventDegradesToSystem documents the post-partials behaviour:
// stream-json with no --include-partial-messages never emits stream_event lines,
// but if one arrives it degrades to a harmless system event (not dropped).
func TestParseStreamEventDegradesToSystem(t *testing.T) {
	evs := parseClaudeLine(`{"type":"stream_event","event":{"type":"content_block_delta"}}`)
	if len(evs) != 1 || evs[0].Kind != KindSystem {
		t.Fatalf("stream_event should degrade to system, got %+v", evs)
	}
}

func TestParseResultErrorYieldsErrorThenDone(t *testing.T) {
	evs := parseClaudeLine(lineErrRes)
	if len(evs) != 2 || evs[0].Kind != KindError || evs[1].Kind != KindTurnDone {
		t.Fatalf("want [error, turn_done], got %+v", evs)
	}
	if evs[0].Recoverable {
		t.Fatalf("a result error is terminal, not recoverable")
	}
}

func TestParseGarbageDegradesToSystem(t *testing.T) {
	evs := parseClaudeLine(lineGarbage)
	if len(evs) != 1 || evs[0].Kind != KindSystem {
		t.Fatalf("non-JSON should degrade to system, got %+v", evs)
	}
}

// TestFullTurnSequence simulates a complete turn as it would arrive over the
// pipe and asserts the normalized event sequence the gateway would consume.
func TestFullTurnSequence(t *testing.T) {
	lines := []string{lineInit, lineHook, lineText, lineToolUse, lineToolRes, lineText, lineResult}
	var kinds []EventKind
	for _, l := range lines {
		for _, ev := range parseClaudeLine(l) {
			kinds = append(kinds, ev.Kind)
		}
	}
	want := []EventKind{
		KindSessionStarted, KindSystem, KindTextDelta,
		KindToolUse, KindToolResult, KindTextDelta, KindTurnDone,
	}
	if len(kinds) != len(want) {
		t.Fatalf("sequence length: got %v want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("at %d: got %s want %s (full=%v)", i, kinds[i], want[i], kinds)
		}
	}
}

// TestClaudeArgsMinimalMode asserts the SDK-aligned default flag set:
// the prompt REPLACES (--system-prompt always emitted), project/local
// setting sources are silenced (--setting-sources=user keeps user-scope
// for CLAUDE_CONFIG_DIR-based per-bot skill discovery), permission-mode
// is bypassPermissions (headless has no approver), and the tool surface
// is restricted via --tools.
func TestClaudeArgsMinimalMode(t *testing.T) {
	d := newTestDriver()
	args := d.buildArgs(Request{Prompt: "hi", SystemPrompt: "you are X"})
	if !contains(args, "--system-prompt") {
		t.Fatalf("--system-prompt missing: %v", args)
	}
	if !contains(args, "--setting-sources=user") {
		t.Fatalf("--setting-sources=user missing (need to keep CLAUDE_CONFIG_DIR-based per-bot skills loading): %v", args)
	}
	if !contains(args, "--permission-mode") || !contains(args, "bypassPermissions") {
		t.Fatalf("--permission-mode bypassPermissions missing (headless has no approver; --tools scopes capability, not the permission mode): %v", args)
	}
	if !contains(args, "--tools") {
		t.Fatalf("--tools missing (the surface-restrict flag — --allowedTools is auto-approve, not restrict): %v", args)
	}
	if contains(args, "--allowedTools") {
		t.Fatalf("minimal mode must NOT use --allowedTools (does not actually restrict surface): %v", args)
	}
	if contains(args, "--disallowedTools") {
		t.Fatalf("minimal mode uses --tools to restrict surface; --disallowedTools is redundant: %v", args)
	}
	if contains(args, "--append-system-prompt") {
		t.Fatalf("minimal mode must NOT use --append-system-prompt: %v", args)
	}
}

// TestClaudeArgsMinimalModeEmptyPromptStillReplaces is the regression
// guard for the SecurityPrefix-drop hazard: an empty SystemPrompt must
// still emit --system-prompt (with an empty value) so a misconfigured
// caller doesn't silently fall back to claude's built-in default.
func TestClaudeArgsMinimalModeEmptyPromptStillReplaces(t *testing.T) {
	d := newTestDriver()
	args := d.buildArgs(Request{Prompt: "hi", SystemPrompt: ""})
	idx := -1
	for i, a := range args {
		if a == "--system-prompt" {
			idx = i
		}
	}
	if idx < 0 || idx+1 >= len(args) {
		t.Fatalf("--system-prompt missing even on empty SystemPrompt: %v", args)
	}
	if args[idx+1] != "" {
		t.Fatalf("--system-prompt value should be empty string, got %q", args[idx+1])
	}
}

// TestClaudeArgsClaudeCodeModeEscapeHatch asserts the escape hatch
// preserves the previous behavior: append on top of the built-in prompt,
// blanket permissions, no surface-restrict / setting-sources / system-
// prompt flags. Used by bots whose SOUL.md assumed the Claude Code preamble.
func TestClaudeArgsClaudeCodeModeEscapeHatch(t *testing.T) {
	d := NewClaudeDriver("claude")
	d.Mode = PromptModeClaudeCode
	args := d.buildArgs(Request{Prompt: "hi", SystemPrompt: "soul"})
	if !contains(args, "--append-system-prompt") {
		t.Fatalf("claude_code mode must use --append-system-prompt: %v", args)
	}
	if !contains(args, "bypassPermissions") {
		t.Fatalf("claude_code mode must use bypassPermissions: %v", args)
	}
	if contains(args, "--system-prompt") {
		t.Fatalf("claude_code mode must NOT use --system-prompt: %v", args)
	}
	if contains(args, "--setting-sources=user") {
		t.Fatalf("claude_code mode must NOT use --setting-sources=user: %v", args)
	}
	if contains(args, "--tools") {
		t.Fatalf("claude_code mode must NOT use --tools (bypassPermissions grants everything): %v", args)
	}
}

// newTestDriver returns a minimal-mode ClaudeDriver with the headless tool
// probe pre-seeded, so buildArgs() in unit tests is deterministic and never
// spawns the real claude binary. Tests that care about the nil-tools probe
// resolution seed their own cache instead.
func newTestDriver() *ClaudeDriver {
	d := NewClaudeDriver("claude")
	d.toolsCache = map[string]toolProbe{"claude": {tools: []string{"Read", "Bash"}, ok: true}}
	return d
}

// TestClaudeArgsAllowedTools pins the AllowedTools semantics in minimal mode:
// nil → the binary's probed headless-safe set; empty slice → no tools at all
// (--tools ""); non-empty → exact list; nil with an unavailable probe → the
// CLI's own "default" set (no hand-maintained Go fallback).
func TestClaudeArgsAllowedTools(t *testing.T) {
	cases := []struct {
		name      string
		allowed   []string
		seed      func(*ClaudeDriver)
		wantValue string
	}{
		{
			name:    "nil resolves probed headless-safe set",
			allowed: nil,
			seed: func(d *ClaudeDriver) {
				d.toolsCache = map[string]toolProbe{"claude": {tools: []string{"Read", "Bash", "Skill"}, ok: true}}
			},
			wantValue: "Read,Bash,Skill",
		},
		{
			name:      "nil + probe unavailable falls back to CLI default",
			allowed:   nil,
			seed:      func(d *ClaudeDriver) { d.toolsCache = map[string]toolProbe{"claude": {ok: false, at: time.Now()}} },
			wantValue: "default",
		},
		{"explicit list verbatim", []string{"Read", "Bash"}, nil, "Read,Bash"},
		{"empty slice → --tools \"\" (no surface)", []string{}, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewClaudeDriver("claude")
			if tc.seed != nil {
				tc.seed(d)
			}
			args := d.buildArgs(Request{Prompt: "hi", AllowedTools: tc.allowed})
			got := ""
			for i, a := range args {
				if a == "--tools" && i+1 < len(args) {
					got = args[i+1]
				}
			}
			if got != tc.wantValue {
				t.Fatalf("--tools = %q, want %q", got, tc.wantValue)
			}
		})
	}
}

// TestClaudeArgsSettingSources pins minimal-mode --setting-sources: empty
// request → "user" default; an explicit list is comma-joined.
func TestClaudeArgsSettingSources(t *testing.T) {
	d := newTestDriver()
	if args := d.buildArgs(Request{Prompt: "hi"}); !contains(args, "--setting-sources=user") {
		t.Fatalf("empty SettingSources should default to user: %v", args)
	}
	if args := d.buildArgs(Request{Prompt: "hi", SettingSources: []string{"user", "project"}}); !contains(args, "--setting-sources=user,project") {
		t.Fatalf("explicit SettingSources should be comma-joined: %v", args)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func TestClaudeArgsMemoryDirToSettings(t *testing.T) {
	d := newTestDriver()
	args := d.buildArgs(Request{Prompt: "hi", MemoryDir: "/m/x y"})
	// Find --settings and its JSON value.
	idx := -1
	for i, a := range args {
		if a == "--settings" {
			idx = i
			break
		}
	}
	if idx < 0 || idx+1 >= len(args) {
		t.Fatalf("missing --settings: %v", args)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(args[idx+1]), &got); err != nil {
		t.Fatalf("--settings value is not valid JSON (%v): %q", err, args[idx+1])
	}
	if got["autoMemoryDirectory"] != "/m/x y" {
		t.Fatalf("autoMemoryDirectory = %q, want %q", got["autoMemoryDirectory"], "/m/x y")
	}
}

func TestClaudeArgsNoSettingsWithoutMemoryDir(t *testing.T) {
	d := newTestDriver()
	if contains(d.buildArgs(Request{Prompt: "hi"}), "--settings") {
		t.Fatal("--settings should be absent when MemoryDir is empty")
	}
}

// maskToken redacts API tokens for the [selfcheck] log line. Keeps enough
// surface to recognize WHICH token is in play without exposing the secret;
// "UNSET" and "SHORT(...)" branches must scream loudly so an operator can
// spot a misconfigured fresh install at a glance.
func TestMaskToken(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "UNSET"},
		{"abc", "SHORT(abc)"},
		{"sk-1234567890abcdef", "sk-123...cdef"},
		{"bf_aaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bf_aaa...aaaa"},
	}
	for _, c := range cases {
		if got := maskToken(c.in); got != c.want {
			t.Errorf("maskToken(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// isDirWritable is the cwd guard inside the selfcheck line. Read-only mounts
// and wrong-owner dirs both manifest as failed first turns; the line should
// flag them. Empty / nonexistent dirs are reported as not writable.
func TestIsDirWritable(t *testing.T) {
	if isDirWritable("") {
		t.Fatal("empty dir reported writable")
	}
	if isDirWritable("/does/not/exist/octobuddy-test") {
		t.Fatal("nonexistent dir reported writable")
	}
	dir := t.TempDir()
	if !isDirWritable(dir) {
		t.Fatal("tempdir reported not writable")
	}
}

// TestFilterToolsDropsInteractive confirms the headless-safe surface is the
// probed set minus the interactive denylist, with order preserved.
func TestFilterToolsDropsInteractive(t *testing.T) {
	in := []string{"Read", "AskUserQuestion", "Bash", "EnterPlanMode", "ExitPlanMode", "Task", "EnterWorktree", "ExitWorktree", "Skill"}
	got := filterTools(in)
	want := []string{"Read", "Bash", "Task", "Skill"}
	if len(got) != len(want) {
		t.Fatalf("filterTools = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("at %d: got %q want %q (full=%v)", i, got[i], want[i], got)
		}
	}
}
