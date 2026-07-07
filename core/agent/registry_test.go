package agent

import (
	"strings"
	"testing"
)

func TestRegistryNewClaude(t *testing.T) {
	d, err := New("claude", Options{})
	if err != nil {
		t.Fatalf("New(claude): %v", err)
	}
	if d.Name() != "claude" {
		t.Fatalf("Name()=%q want claude", d.Name())
	}
	// Empty name resolves to the default driver (claude), so legacy config.json
	// files with no agent.driver keep working.
	d2, err := New("", Options{})
	if err != nil || d2.Name() != "claude" {
		t.Fatalf("New(\"\") => %v, %v; want claude driver", d2, err)
	}
}

func TestRegistryUnknownDriverErrors(t *testing.T) {
	_, err := New("does-not-exist", Options{})
	if err == nil {
		t.Fatal("New(unknown) must error, not silently fall back")
	}
	if !strings.Contains(err.Error(), "claude") {
		t.Fatalf("error should list registered drivers, got: %v", err)
	}
}

func TestRegistryWiresOptions(t *testing.T) {
	called := false
	d, _ := New("claude", Options{
		BinFn:       func() string { return "/custom/claude" },
		EnvFn:       func() []string { called = true; return []string{"X=1"} },
		MCPConfigFn: func() string { return "/cfg/.mcp.json" },
	})
	cd, ok := d.(*ClaudeDriver)
	if !ok {
		t.Fatalf("claude driver is %T", d)
	}
	if got := cd.binPath(); got != "/custom/claude" {
		t.Fatalf("BinFn not wired: binPath()=%q", got)
	}
	if got := cd.queryEnv(); len(got) != 1 || got[0] != "X=1" || !called {
		t.Fatalf("EnvFn not wired: %v", got)
	}
	if cd.MCPConfigFn == nil || cd.MCPConfigFn() != "/cfg/.mcp.json" {
		t.Fatal("MCPConfigFn not wired")
	}
}

// TestClaudeConfigDirName pins the per-bot config-dir contract the desktop and
// daemon both depend on. If this changes, skills/workflows/mcp paths move.
func TestClaudeConfigDirName(t *testing.T) {
	if n := NewClaudeDriver("").ConfigDirName(); n != ".claude" {
		t.Fatalf("ConfigDirName()=%q want .claude", n)
	}
}

// TestClaudeEnvMapperContract pins that the driver's EnvMapper emits the exact
// claude env-var names (and ordering: agent.env first, named vars last) that the
// legacy config.DriverEnv produced — the byte-identical guarantee from the
// refactor plan's verification step.
func TestClaudeEnvMapperContract(t *testing.T) {
	d := NewClaudeDriver("")
	got := d.Env(EnvSpec{
		Extra:          []string{"OCTO_BOT_ID=alpha-bot", "GH_TOKEN=ghp_actual"},
		GatewayBaseURL: "https://gw.example/v1",
		GatewayToken:   "sk-ant-xyz",
		OctoToken:      "bf_x",
		OctoAPIURL:     "https://octo.example",
		ConfigDir:      "/root/.claude",
	})
	want := []string{
		"OCTO_BOT_ID=alpha-bot",
		"GH_TOKEN=ghp_actual",
		"ANTHROPIC_BASE_URL=https://gw.example/v1",
		"ANTHROPIC_AUTH_TOKEN=sk-ant-xyz",
		"OCTO_BOT_TOKEN=bf_x",
		"OCTO_API_BASE_URL=https://octo.example",
		"CLAUDE_CONFIG_DIR=/root/.claude",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("Env mismatch:\n got=%v\nwant=%v", got, want)
	}

	// InheritUserConfig suppresses the isolation var.
	got = d.Env(EnvSpec{ConfigDir: "/root/.claude", InheritUserConfig: true})
	if len(got) != 0 {
		t.Fatalf("inheritUserConfig should suppress CLAUDE_CONFIG_DIR, got %v", got)
	}
	// Empty token omits the auth var.
	for _, e := range d.Env(EnvSpec{GatewayBaseURL: "https://gw"}) {
		if e == "ANTHROPIC_AUTH_TOKEN=" {
			t.Fatal("empty token must not emit ANTHROPIC_AUTH_TOKEN")
		}
	}
}

// TestClaudeImplementsCapabilities pins that ClaudeDriver satisfies the optional
// capability interfaces the cmd/desktop layers assert for.
func TestClaudeImplementsCapabilities(t *testing.T) {
	var d Driver = NewClaudeDriver("")
	if _, ok := d.(EnvMapper); !ok {
		t.Error("ClaudeDriver must implement EnvMapper")
	}
	if _, ok := d.(ConfigDirNamer); !ok {
		t.Error("ClaudeDriver must implement ConfigDirNamer")
	}
	if _, ok := d.(MCPProber); !ok {
		t.Error("ClaudeDriver must implement MCPProber")
	}
	if _, ok := d.(ToolProber); !ok {
		t.Error("ClaudeDriver must implement ToolProber")
	}
}
