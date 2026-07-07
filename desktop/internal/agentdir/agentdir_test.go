package agentdir

import (
	"os"
	"path/filepath"
	"testing"
)

// TestNameDefaultsToClaude pins that a bot with no agent.driver (the common
// case, and all legacy configs) resolves to ".claude" — the directory the
// skills/workflows/mcpconfig packages must keep reading/writing.
func TestNameDefaultsToClaude(t *testing.T) {
	withHome(t, `{"bots":[{"id":"alpha"}]}`)
	if got := Name("alpha"); got != ".claude" {
		t.Fatalf("Name(alpha) = %q, want .claude", got)
	}
	// Unknown bot id also falls back to the default.
	if got := Name("ghost"); got != ".claude" {
		t.Fatalf("Name(ghost) = %q, want .claude", got)
	}
}

// TestNameFollowsDriver pins that an explicit agent.driver picks that driver's
// config-dir name (codex → .codex), proving the desktop dir contract tracks the
// driver's own ConfigDirName() rather than a hardcoded literal.
func TestNameFollowsDriver(t *testing.T) {
	withHome(t, `{"bots":[
		{"id":"alpha","agent":{"driver":"claude"}},
		{"id":"beta","agent":{"driver":"codex"}}
	]}`)
	if got := Name("alpha"); got != ".claude" {
		t.Fatalf("Name(alpha) = %q, want .claude", got)
	}
	if got := Name("beta"); got != ".codex" {
		t.Fatalf("Name(beta) = %q, want .codex", got)
	}
}

// TestNameUnknownDriverFallsBack pins safe degradation: a driver string with no
// registered driver resolves to the default dir rather than "" (which would
// collapse the path).
func TestNameUnknownDriverFallsBack(t *testing.T) {
	withHome(t, `{"bots":[{"id":"alpha","agent":{"driver":"nope"}}]}`)
	if got := Name("alpha"); got != ".claude" {
		t.Fatalf("Name(alpha) = %q, want .claude (fallback)", got)
	}
}

// withHome points $HOME at a temp dir holding the given config.json so Name's
// read resolves deterministically.
func withHome(t *testing.T, configJSON string) {
	t.Helper()
	home := t.TempDir()
	// UserHomeDir reads $HOME on unix but %USERPROFILE% on Windows — set both.
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := filepath.Join(home, ".octobuddy")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
}
