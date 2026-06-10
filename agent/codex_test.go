package agent

import "testing"

// These fixtures are Codex app-server notifications (shapes taken from the
// generated JSON schema: AgentMessageDeltaNotification{delta,itemId,…},
// ThreadStartedNotification{thread}, ThreadTokenUsageUpdatedNotification, …).
// The test proves CodexDriver normalizes a totally different protocol into the
// SAME AgentEvent vocabulary the Claude driver emits.

func TestCodexThreadStarted(t *testing.T) {
	var usage *TokenUsage
	evs := translateCodexNotification("thread/started",
		[]byte(`{"thread":{"id":"thr-123"}}`), &usage)
	if len(evs) != 1 || evs[0].Kind != KindSessionStarted || evs[0].SessionID != "thr-123" {
		t.Fatalf("want session_started thr-123, got %+v", evs)
	}
}

func TestCodexAgentMessageDelta(t *testing.T) {
	var usage *TokenUsage
	evs := translateCodexNotification("item/agentMessage/delta",
		[]byte(`{"delta":"hello ","itemId":"i1","threadId":"t1","turnId":"u1"}`), &usage)
	if len(evs) != 1 || evs[0].Kind != KindTextDelta || evs[0].Text != "hello " {
		t.Fatalf("want text delta, got %+v", evs)
	}
}

func TestCodexReasoningDelta(t *testing.T) {
	var usage *TokenUsage
	evs := translateCodexNotification("item/reasoning/textDelta",
		[]byte(`{"delta":"thinking…","itemId":"i1"}`), &usage)
	if len(evs) != 1 || evs[0].Kind != KindThinking {
		t.Fatalf("want thinking, got %+v", evs)
	}
}

func TestCodexCommandToolUse(t *testing.T) {
	var usage *TokenUsage
	evs := translateCodexNotification("item/started",
		[]byte(`{"item":{"type":"commandExecution","command":{"cmd":"ls -la"}},"turnId":"u1"}`), &usage)
	if len(evs) != 1 || evs[0].Kind != KindToolUse || evs[0].ToolName != "Shell" {
		t.Fatalf("want Shell tool_use, got %+v", evs)
	}
	if evs[0].ToolParams == "" {
		t.Fatalf("want non-empty params one-liner")
	}
}

func TestCodexNonToolItemIsIgnoredForToolUse(t *testing.T) {
	var usage *TokenUsage
	// An agentMessage item/started is not a tool; should produce nothing.
	evs := translateCodexNotification("item/started",
		[]byte(`{"item":{"type":"agentMessage"},"turnId":"u1"}`), &usage)
	if len(evs) != 0 {
		t.Fatalf("agentMessage item should not be a tool_use, got %+v", evs)
	}
}

func TestCodexTokenUsageAccumulates(t *testing.T) {
	var usage *TokenUsage
	evs := translateCodexNotification("thread/tokenUsage/updated",
		[]byte(`{"threadId":"t1","turnId":"u1","tokenUsage":{"inputTokens":1200,"outputTokens":45}}`), &usage)
	if len(evs) != 0 {
		t.Fatalf("token usage is side-effect only, should emit no events; got %+v", evs)
	}
	if usage == nil || usage.OutputTokens != 45 {
		t.Fatalf("usage not accumulated: %+v", usage)
	}
}

func TestCodexError(t *testing.T) {
	var usage *TokenUsage
	evs := translateCodexNotification("error", []byte(`{"message":"boom"}`), &usage)
	if len(evs) != 1 || evs[0].Kind != KindError || evs[0].Err != "boom" {
		t.Fatalf("want error, got %+v", evs)
	}
}

func TestCodexUnknownDegradesToSystem(t *testing.T) {
	var usage *TokenUsage
	evs := translateCodexNotification("turn/started", []byte(`{"threadId":"t1"}`), &usage)
	if len(evs) != 1 || evs[0].Kind != KindSystem {
		t.Fatalf("unknown notification should degrade to system, got %+v", evs)
	}
}

// TestCrossDriverVocabularyParity is the core proof: a full Codex turn and a
// full Claude turn, driven by completely different protocols, both reduce to the
// SAME ordered AgentEvent vocabulary the gateway consumes.
func TestCrossDriverVocabularyParity(t *testing.T) {
	var usage *TokenUsage
	codexNotes := []struct {
		method string
		params string
	}{
		{"thread/started", `{"thread":{"id":"thr-1"}}`},
		{"turn/started", `{"threadId":"thr-1"}`},
		{"item/agentMessage/delta", `{"delta":"I'll check."}`},
		{"item/started", `{"item":{"type":"commandExecution","command":{"cmd":"ls"}}}`},
		{"item/completed", `{"item":{"type":"commandExecution","command":{"cmd":"ls"}}}`},
		{"item/agentMessage/delta", `{"delta":"Done."}`},
		{"thread/tokenUsage/updated", `{"tokenUsage":{"inputTokens":10,"outputTokens":3}}`},
	}
	var codexKinds []EventKind
	for _, n := range codexNotes {
		for _, ev := range translateCodexNotification(n.method, []byte(n.params), &usage) {
			codexKinds = append(codexKinds, ev.Kind)
		}
	}
	// turn/started → system; tokenUsage → (none). So expected:
	want := []EventKind{
		KindSessionStarted, KindSystem, KindTextDelta,
		KindToolUse, KindToolResult, KindTextDelta,
	}
	if len(codexKinds) != len(want) {
		t.Fatalf("codex kinds %v want %v", codexKinds, want)
	}
	for i := range want {
		if codexKinds[i] != want[i] {
			t.Fatalf("at %d codex got %s want %s", i, codexKinds[i], want[i])
		}
	}

	// The same logical turn via Claude (from claude_test fixtures) reduces to
	// the same core vocabulary (session, text, tool, result, text) — proving
	// the gateway can consume either driver identically.
	claudeLines := []string{lineInit, lineText, lineToolUse, lineToolRes, lineText}
	var claudeKinds []EventKind
	for _, l := range claudeLines {
		for _, ev := range parseClaudeLine(l) {
			claudeKinds = append(claudeKinds, ev.Kind)
		}
	}
	claudeWant := []EventKind{
		KindSessionStarted, KindTextDelta, KindToolUse, KindToolResult, KindTextDelta,
	}
	for i := range claudeWant {
		if claudeKinds[i] != claudeWant[i] {
			t.Fatalf("claude at %d got %s want %s", i, claudeKinds[i], claudeWant[i])
		}
	}
}

// Compile-time proof both drivers satisfy the same interface.
var _ Driver = (*ClaudeDriver)(nil)
var _ Driver = (*CodexDriver)(nil)
