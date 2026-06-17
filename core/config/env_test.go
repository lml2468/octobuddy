package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// global agent.env is the base; the inline bot's env adds/overrides per key (not
// whole replacement), and the gateway vars are mapped to the claude env names by
// DriverEnv.
func TestEnvPerKeyMergeAndGatewayVars(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, `{
	  "apiUrl":"https://octo.example",
	  "agent":{"env":{"SHARED_DEFAULT":"global","SHARED":"global"},
	         "gatewayBaseUrl":"https://gw.example/v1"},
	  "bots":[{
	    "id":"alpha",
	    "octoToken":"bf_a",
	    "agent":{"env":{"OCTO_BOT_ID":"alpha-bot","GH_TOKEN":"ghp_x","SHARED":"perbot"},
	           "gatewayToken":"sk-ant-xyz"}
	  }]
	}`)

	bots, err := Load(cfg)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	env := bots[0].Agent.Env
	if env["SHARED_DEFAULT"] != "global" {
		t.Fatalf("global env key lost: %v", env)
	}
	if env["OCTO_BOT_ID"] != "alpha-bot" {
		t.Fatalf("per-bot OCTO_BOT_ID missing: %v", env)
	}
	if env["GH_TOKEN"] != "ghp_x" {
		t.Fatalf("per-bot env key missing: %v", env)
	}
	if env["SHARED"] != "perbot" {
		t.Fatalf("per-bot should override SHARED, got %q", env["SHARED"])
	}

	// claude driver → ANTHROPIC_* names.
	de := bots[0].DriverEnv()
	joined := strings.Join(de, "\n")
	for _, want := range []string{
		"SHARED_DEFAULT=global", "OCTO_BOT_ID=alpha-bot", "GH_TOKEN=ghp_x", "SHARED=perbot",
		"ANTHROPIC_BASE_URL=https://gw.example/v1", "ANTHROPIC_AUTH_TOKEN=sk-ant-xyz",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("claude DriverEnv missing %q in:\n%s", want, joined)
		}
	}
}

func TestDriverEnvEmptyWhenUnset(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, `{"bots":[{"id":"alpha","octoToken":"t"}]}`)
	bots, _ := Load(cfg)
	// With no gateway URL/token/env, the only DriverEnv entry is the isolation
	// var (CLAUDE_CONFIG_DIR → the per-bot config root), on by default.
	env := bots[0].DriverEnv()
	if len(env) != 1 || env[0] != "CLAUDE_CONFIG_DIR="+bots[0].ClaudeConfigDir {
		t.Fatalf("expected only CLAUDE_CONFIG_DIR, got %v", env)
	}
}

func TestDriverEnvInheritUserConfigOptsOut(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, `{"bots":[{"id":"alpha","octoToken":"t","agent":{"inheritUserConfig":true}}]}`)
	bots, _ := Load(cfg)
	if len(bots[0].DriverEnv()) != 0 {
		t.Fatalf("inheritUserConfig should suppress CLAUDE_CONFIG_DIR, got %v", bots[0].DriverEnv())
	}
}
