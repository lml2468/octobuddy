package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSingleBotDefaults(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, `{"apiUrl":"https://octo.example","bots":[{"id":"default","octoToken":"bf_x"}]}`)

	bots, err := Load(cfg)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(bots) != 1 {
		t.Fatalf("want 1 bot, got %d", len(bots))
	}
	b := bots[0]
	if b.BotID != "default" {
		t.Fatalf("botID = %q", b.BotID)
	}
	if b.APIURL != "https://octo.example" || b.OctoToken != "bf_x" {
		t.Fatalf("apiUrl/token wrong: %+v", b)
	}
	// defaults applied
	if b.RateLimit.MaxPerMinute != 5 || b.Context.MaxContextChars != 6000 {
		t.Fatalf("defaults wrong: %+v", b)
	}
	// derived data dir
	if b.DataDir != filepath.Join(dir, "default", "data") {
		t.Fatalf("derived data dir wrong: %+v", b)
	}
	// derived sandbox dirs
	if b.CwdBase != filepath.Join(dir, "default", "workspace") ||
		b.MemoryBase != filepath.Join(dir, "default", "memory") ||
		b.SkillsDir != filepath.Join(dir, "default", "skills") ||
		b.GlobalSkillsDir != filepath.Join(dir, "skills") {
		t.Fatalf("derived sandbox dirs wrong: %+v", b)
	}
}

func TestInlineBotOverridesGlobalDefaults(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, `{
	  "apiUrl":"https://global.example",
	  "context":{"maxContextChars":1000},
	  "agent":{"model":"global-model","gatewayBaseUrl":"https://gw.example/v1"},
	  "bots":[{
	    "id":"alpha",
	    "octoToken":"bf_alpha",
	    "apiUrl":"https://bot.example",
	    "agent":{"model":"bot-model"},
	    "context":{"maxContextChars":2000}
	  }]
	}`)

	bots, err := Load(cfg)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	b := bots[0]
	if b.BotID != "alpha" {
		t.Fatalf("id = %q", b.BotID)
	}
	if b.APIURL != "https://bot.example" {
		t.Fatalf("bot apiUrl should win: %q", b.APIURL)
	}
	if b.Agent.Model != "bot-model" {
		t.Fatalf("bot model should win: %q", b.Agent.Model)
	}
	// gateway not set on the bot → inherits the global default
	if b.Agent.GatewayBaseURL != "https://gw.example/v1" {
		t.Fatalf("global gateway default should carry through: %q", b.Agent.GatewayBaseURL)
	}
	if b.Context.MaxContextChars != 2000 {
		t.Fatalf("bot context should win: %d", b.Context.MaxContextChars)
	}
	if b.OctoToken != "bf_alpha" {
		t.Fatalf("token = %q", b.OctoToken)
	}
}

func TestSystemPromptFromSoulOnly(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, `{"bots":[{"id":"default","octoToken":"bf_x"}]}`)
	writeFile(t, filepath.Join(dir, "default", "SOUL.md"), "  you are a helpful bot  ")

	bots, _ := Load(cfg)
	if bots[0].SystemPrompt != "you are a helpful bot" {
		t.Fatalf("SOUL.md should be trimmed: %q", bots[0].SystemPrompt)
	}
}

func TestSystemPromptCombinesSoulAndAgents(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, `{"bots":[{"id":"default","octoToken":"bf_x"}]}`)
	writeFile(t, filepath.Join(dir, "default", "SOUL.md"), "I am Nova.")
	writeFile(t, filepath.Join(dir, "default", "AGENTS.md"), "Always reply in Chinese.")

	bots, _ := Load(cfg)
	want := "I am Nova.\n\nAlways reply in Chinese."
	if bots[0].SystemPrompt != want {
		t.Fatalf("SOUL.md + AGENTS.md should combine: %q", bots[0].SystemPrompt)
	}
}

func TestSystemPromptAgentsOnly(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, `{"bots":[{"id":"default","octoToken":"bf_x"}]}`)
	writeFile(t, filepath.Join(dir, "default", "AGENTS.md"), "Be concise.")

	bots, _ := Load(cfg)
	if bots[0].SystemPrompt != "Be concise." {
		t.Fatalf("AGENTS.md alone should apply: %q", bots[0].SystemPrompt)
	}
}

func TestSlugRejection(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, `{"bots":[{"id":"../escape","octoToken":"bf_x"}]}`)
	if _, err := Load(cfg); err == nil {
		t.Fatal("path-traversal id must be rejected")
	}
}

func TestSSRFRejection(t *testing.T) {
	cases := map[string]bool{
		"https://octo.example":   true,
		"https://1.2.3.4":        true,
		"http://localhost:8080":  true,
		"http://127.0.0.1":       true,
		"https://127.0.0.1":      false, // private IP literal over https rejected
		"https://10.0.0.1":       false,
		"https://169.254.169.254": false, // cloud metadata
		"http://evil.example":    false, // http to non-localhost
		"ftp://x":                false,
	}
	for u, want := range cases {
		if got := isAllowedURL(u); got != want {
			t.Fatalf("isAllowedURL(%q) = %v, want %v", u, got, want)
		}
	}
}

func TestMissingTokenRejected(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, `{"bots":[{"id":"alpha"}]}`) // bot has no octoToken
	if _, err := Load(cfg); err == nil {
		t.Fatal("missing token must be rejected")
	}
}

func TestNoBotsRejected(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, `{"apiUrl":"https://o"}`) // no bots[]
	if _, err := Load(cfg); err == nil {
		t.Fatal("config with no bots must be rejected")
	}
}

func TestMissingConfigRejected(t *testing.T) {
	// no file → empty global → no bots → error (not a crash)
	if _, err := Load(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("expected error for missing/empty config")
	}
}
