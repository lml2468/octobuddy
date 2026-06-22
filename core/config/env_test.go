package config

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lml2468/xclaw/core/envenc"
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

	// claude driver → ANTHROPIC_* names. Pass the config-file gateway token to
	// emulate the headless path (cmd/xclawd injects sec.GatewayToken in -config
	// mode; either feeds the same field through).
	de := bots[0].DriverEnv(bots[0].Agent.GatewayToken, "")
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
	env := bots[0].DriverEnv("", "")
	if len(env) != 1 || env[0] != "CLAUDE_CONFIG_DIR="+bots[0].ClaudeConfigDir {
		t.Fatalf("expected only CLAUDE_CONFIG_DIR, got %v", env)
	}
}

func TestDriverEnvInheritUserConfigOptsOut(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, `{"bots":[{"id":"alpha","octoToken":"t","agent":{"inheritUserConfig":true}}]}`)
	bots, _ := Load(cfg)
	if len(bots[0].DriverEnv("", "")) != 0 {
		t.Fatalf("inheritUserConfig should suppress CLAUDE_CONFIG_DIR, got %v", bots[0].DriverEnv("", ""))
	}
}

// DriverEnv with a non-empty octo token injects the octo-cli companion
// credential (OCTO_BOT_TOKEN + OCTO_API_BASE_URL) so the spawned agent's
// octo-cli authenticates from env (the fallback path; the primary path is
// the on-disk profile written by octocli.Login).
func TestDriverEnvOctoFallbackVars(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, `{"apiUrl":"https://octo.example","bots":[{"id":"alpha","octoToken":"t"}]}`)
	bots, err := Load(cfg)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// With an injected octo token + the bot's apiUrl, both octo-cli vars appear.
	joined := strings.Join(bots[0].DriverEnv("", "bf_injected"), "\n")
	for _, want := range []string{
		"OCTO_BOT_TOKEN=bf_injected",
		"OCTO_API_BASE_URL=https://octo.example",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("DriverEnv missing %q in:\n%s", want, joined)
		}
	}

	// An empty octo token omits OCTO_BOT_TOKEN (but apiUrl still yields the base
	// URL — harmless for octo-cli, whose EnvProvider needs the token to act).
	joined = strings.Join(bots[0].DriverEnv("", ""), "\n")
	if strings.Contains(joined, "OCTO_BOT_TOKEN=") {
		t.Fatalf("empty octo token should omit OCTO_BOT_TOKEN, got:\n%s", joined)
	}
}

// Per-bot env values may be sealed with enc:v1:… so config.json never carries
// plaintext tokens. DriverEnv must decrypt with the bot's MasterKey before
// passing the value to the agent; this is the round-trip path users hit when
// the GUI stores a secret and the daemon reads it on the next turn.
func TestDriverEnvDecryptsEncryptedValues(t *testing.T) {
	dir := t.TempDir()
	// Generate a master.key in dir and use it to encrypt the secret BEFORE
	// it lands in config.json — mirrors what the GUI does on Save.
	key, err := envenc.LoadOrCreateMaster(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	ct, err := envenc.Encrypt(key, "ghp_actualtokenhere")
	if err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, fmt.Sprintf(`{"bots":[{"id":"b","agent":{"env":{"GH_TOKEN":%q,"PLAIN":"unchanged"}}}]}`, ct))

	bots, err := Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	env := bots[0].DriverEnv("", "")
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "GH_TOKEN=ghp_actualtokenhere") {
		t.Fatalf("encrypted GH_TOKEN not decrypted; env was:\n%s", joined)
	}
	if !strings.Contains(joined, "PLAIN=unchanged") {
		t.Fatalf("plaintext value mangled; env was:\n%s", joined)
	}
}

// A ciphertext sealed by a different master key (e.g. config.json copied
// from another machine without master.key) must fail-soft: the entry is
// dropped from the env so a same-named plaintext fallback can win OR the
// agent fails loudly with auth=UNSET in selfcheck — both beat silently
// injecting an empty token that a tool might accept.
func TestDriverEnvSkipsUndecryptableValues(t *testing.T) {
	dir := t.TempDir()
	// Encrypt under a DIFFERENT key than the one Load will read.
	otherKey, _ := envenc.LoadOrCreateMaster(filepath.Join(t.TempDir(), "other.key"))
	ct, _ := envenc.Encrypt(otherKey, "this-cant-be-decrypted-here")
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, fmt.Sprintf(`{"bots":[{"id":"b","agent":{"env":{"GH_TOKEN":%q}}}]}`, ct))

	bots, _ := Load(cfg)
	for _, e := range bots[0].DriverEnv("", "") {
		if strings.HasPrefix(e, "GH_TOKEN=") {
			t.Fatalf("undecryptable value should not be injected; got %q", e)
		}
	}
}

