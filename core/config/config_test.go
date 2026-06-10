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
	writeFile(t, cfg, `{"apiUrl":"https://octo.example","octoToken":"bf_x"}`)

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
	// defaults applied
	if b.RateLimit.MaxPerMinute != 5 || b.Context.MaxContextChars != 6000 ||
		b.SDK.PermissionMode != "bypassPermissions" || b.MaxResponseChars != 512*1024 {
		t.Fatalf("defaults wrong: %+v", b)
	}
	// derived paths
	if b.DataDir != filepath.Join(dir, "default", "data") ||
		b.CwdBase != filepath.Join(dir, "default", "workspace") ||
		b.MemoryBase != filepath.Join(dir, "default", "memory") {
		t.Fatalf("derived paths wrong: %+v", b)
	}
}

func TestPerBotFilePrecedence(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, `{
	  "apiUrl":"https://octo.example",
	  "context":{"maxContextChars":1000},
	  "bots":[{"id":"alpha","model":"inline-model"}]
	}`)
	// per-bot file overrides inline + global
	writeFile(t, filepath.Join(dir, "alpha", "config.json"),
		`{"octoToken":"bf_alpha","sdk":{"model":"perbot-model"},"context":{"maxContextChars":2000}}`)

	bots, err := Load(cfg)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	b := bots[0]
	if b.BotID != "alpha" {
		t.Fatalf("id = %q", b.BotID)
	}
	if b.SDK.Model != "perbot-model" {
		t.Fatalf("per-bot file model should win: %q", b.SDK.Model)
	}
	if b.Context.MaxContextChars != 2000 {
		t.Fatalf("per-bot context should win: %d", b.Context.MaxContextChars)
	}
	if b.OctoToken != "bf_alpha" {
		t.Fatalf("token = %q", b.OctoToken)
	}
}

func TestSoulOverridesSystemPrompt(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, `{"apiUrl":"https://o","octoToken":"bf_x","sdk":{"systemPrompt":"global-prompt"}}`)
	writeFile(t, filepath.Join(dir, "default", "SOUL.md"), "  you are a helpful bot  ")

	bots, _ := Load(cfg)
	if bots[0].SDK.SystemPrompt != "you are a helpful bot" {
		t.Fatalf("SOUL.md should win + be trimmed: %q", bots[0].SDK.SystemPrompt)
	}
}

func TestSlugRejection(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, `{"apiUrl":"https://o","bots":[{"id":"../escape","botToken":"t"}]}`)
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
	writeFile(t, cfg, `{"apiUrl":"https://o"}`) // no token anywhere
	if _, err := Load(cfg); err == nil {
		t.Fatal("missing token must be rejected")
	}
}

func TestDuplicateTokenRejected(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, `{"apiUrl":"https://o","bots":[
	  {"id":"a","botToken":"same"},
	  {"id":"b","botToken":"same"}
	]}`)
	if _, err := Load(cfg); err == nil {
		t.Fatal("duplicate token must be rejected")
	}
}

func TestMissingConfigYieldsDefaultBotError(t *testing.T) {
	// no file → empty global → default bot with no token → error (not a crash)
	if _, err := Load(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("expected missing-token error for empty config")
	}
}
