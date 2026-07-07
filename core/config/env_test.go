package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// Env is per-bot only. Plain values stay reviewable in config.json; secret
// values are represented by secretRef and resolved from the runtime secret
// backend when building the agent process env.
func TestEnvPlainAndSecretRefs(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, `{
	  "bots":[{
	    "id":"alpha",
	    "apiUrl":"https://octo.example",
	    "octoToken":"bf_a",
	    "agent":{
	      "gatewayBaseUrl":"https://gw.example/v1",
	      "gatewayToken":"sk-ant-xyz",
	      "env":{
	        "OCTO_BOT_ID":{"value":"alpha-bot"},
	        "GH_TOKEN":{"secretRef":"env/GH_TOKEN"}
	      }
	    }
	  }]
	}`)

	bots, err := Load(cfg)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	env := bots[0].Agent.Env
	if env["OCTO_BOT_ID"].Value != "alpha-bot" {
		t.Fatalf("plain per-bot env missing: %v", env)
	}
	if env["GH_TOKEN"].SecretRef != "env/GH_TOKEN" {
		t.Fatalf("secret env ref missing: %v", env)
	}

	de := bots[0].AgentEnvExtra(func(ref string) string {
		if ref == "env/GH_TOKEN" {
			return "ghp_actual"
		}
		return ""
	})
	joined := strings.Join(de, "\n")
	// AgentEnvExtra carries ONLY the user-declared agent.env (resolved). The
	// gateway/octo/config-dir vars (ANTHROPIC_*, OCTO_*, CLAUDE_CONFIG_DIR) are
	// added by the daemon via the driver's EnvMapper — see
	// agent.TestClaudeEnvMapperContract.
	for _, want := range []string{
		"OCTO_BOT_ID=alpha-bot", "GH_TOKEN=ghp_actual",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("AgentEnvExtra missing %q in:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "ANTHROPIC_") {
		t.Fatalf("AgentEnvExtra must not emit driver-specific vars, got:\n%s", joined)
	}
}

func TestAgentEnvExtraSkipsMissingSecretRefs(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, `{"bots":[{"id":"b","agent":{"env":{"GH_TOKEN":{"secretRef":"env/GH_TOKEN"}}}}]}`)

	bots, err := Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range bots[0].AgentEnvExtra(nil) {
		if strings.HasPrefix(e, "GH_TOKEN=") {
			t.Fatalf("unresolved secretRef should not be injected; got %q", e)
		}
	}
}

func TestAgentEnvExtraEmptyWhenUnset(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, `{"bots":[{"id":"alpha","octoToken":"t"}]}`)
	bots, _ := Load(cfg)
	// No agent.env declared → no neutral extras. The CLAUDE_CONFIG_DIR isolation
	// var is the driver's concern now (added by the daemon), not config's.
	if env := bots[0].AgentEnvExtra(nil); len(env) != 0 {
		t.Fatalf("expected no agent.env extras, got %v", env)
	}
}
