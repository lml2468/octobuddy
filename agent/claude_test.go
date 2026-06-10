package agent

import "testing"

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
	lineResult  = `{"type":"result","subtype":"success","is_error":false,"result":"done","total_cost_usd":0.01,"usage":{"input_tokens":1200,"output_tokens":45},"session_id":"ea4de374"}`
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
	if evs[0].Usage == nil || evs[0].Usage.OutputTokens != 45 {
		t.Fatalf("usage not extracted: %+v", evs[0].Usage)
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
