package agent

import "testing"

// TestClaudeCapabilitiesFull asserts the Claude driver advertises the full
// capability set — it enforces tool scoping (--tools), has a native
// system-prompt channel (--system-prompt), and loads MCP config.
func TestClaudeCapabilitiesFull(t *testing.T) {
	c := (&ClaudeDriver{}).Capabilities()
	if !c.Streaming || !c.Resume || !c.ToolEvents {
		t.Errorf("claude base caps regressed: %+v", c)
	}
	if !c.NativeSystemPrompt {
		t.Errorf("claude must advertise NativeSystemPrompt (--system-prompt)")
	}
	if !c.ToolScoping {
		t.Errorf("claude must advertise ToolScoping (--tools)")
	}
	if !c.MCP {
		t.Errorf("claude must advertise MCP")
	}
}

// TestCodexCapabilitiesDropScopingAndSysPrompt pins the capability GAP the
// negotiation surfaces: codex exec has no --tools flag (AllowedTools silently
// dropped) and no --system-prompt flag (System is prepended to stdin), and its
// MCP config is intentionally unwired. These must read false so the gateway can
// tell it apart from claude.
func TestCodexCapabilitiesDropScopingAndSysPrompt(t *testing.T) {
	c := (&CodexDriver{}).Capabilities()
	if !c.Streaming || !c.Resume || !c.ToolEvents {
		t.Errorf("codex base caps regressed: %+v", c)
	}
	if c.NativeSystemPrompt {
		t.Errorf("codex has no --system-prompt flag; NativeSystemPrompt must be false")
	}
	if c.ToolScoping {
		t.Errorf("codex has no --tools flag; ToolScoping must be false")
	}
	if c.MCP {
		t.Errorf("codex MCPConfigFn is unwired; MCP must be false")
	}
}
