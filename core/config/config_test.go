package config

import (
	"encoding/json"
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
	writeFile(t, cfg, `{"bots":[{"id":"default","apiUrl":"https://octo.example","octoToken":"bf_x"}]}`)

	b := loadSingleBot(t, cfg)
	assertSingleBotIdentity(t, b)
	assertSingleBotDerivedDirs(t, dir, b)
}

func loadSingleBot(t *testing.T, cfg string) Resolved {
	t.Helper()

	bots, err := Load(cfg)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(bots) != 1 {
		t.Fatalf("want 1 bot, got %d", len(bots))
	}
	return bots[0]
}

func assertSingleBotIdentity(t *testing.T, b Resolved) {
	t.Helper()

	if b.BotID != "default" {
		t.Fatalf("botID = %q", b.BotID)
	}
	if b.APIURL != "https://octo.example" || b.OctoToken != "bf_x" {
		t.Fatalf("apiUrl/token wrong: %+v", b)
	}
	// defaults applied
	if b.RateLimit.MaxPerMinute != 30 || b.Context.MaxContextChars != 6000 {
		t.Fatalf("defaults wrong: %+v", b)
	}
}

func assertSingleBotDerivedDirs(t *testing.T, dir string, b Resolved) {
	t.Helper()

	if b.DataDir != filepath.Join(dir, "default", "data") {
		t.Fatalf("derived data dir wrong: %+v", b)
	}
	if b.CwdBase != filepath.Join(dir, "default", "workspace") ||
		b.MemoryBase != filepath.Join(dir, "default", "memory") ||
		b.ClaudeConfigDir != filepath.Join(dir, "default", ".claude") {
		t.Fatalf("derived sandbox dirs wrong: %+v", b)
	}
}

func TestBotConfigAndRuntimePolicyDefaults(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, `{
	  "context":{"maxContextChars":1000},
	  "bots":[{
	    "id":"alpha",
	    "octoToken":"bf_alpha",
	    "apiUrl":"https://bot.example",
	    "agent":{"model":"bot-model","gatewayBaseUrl":"https://gw.example/v1"},
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
	if b.Agent.GatewayBaseURL != "https://gw.example/v1" {
		t.Fatalf("bot gateway should parse: %q", b.Agent.GatewayBaseURL)
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
		"https://octo.example":    true,
		"https://1.2.3.4":         true,
		"http://localhost:8080":   true,
		"http://127.0.0.1":        true,
		"https://127.0.0.1":       false, // private IP literal over https rejected
		"https://10.0.0.1":        false,
		"https://169.254.169.254": false, // cloud metadata
		"http://evil.example":     false, // http to non-localhost
		"ftp://x":                 false,
	}
	for u, want := range cases {
		if got := IsAllowedURL(u); got != want {
			t.Fatalf("IsAllowedURL(%q) = %v, want %v", u, got, want)
		}
	}
}

// A bot with no octoToken now loads fine — the token is injected at runtime
// (secret.inject) from the GUI's secret backend. apiUrl stays in the file.
func TestMissingTokenAllowed(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, `{"bots":[{"id":"alpha","apiUrl":"https://octo.example"}]}`)
	bots, err := Load(cfg)
	if err != nil {
		t.Fatalf("tokenless bot should load (runtime injection): %v", err)
	}
	if len(bots) != 1 || bots[0].OctoToken != "" {
		t.Fatalf("unexpected: %+v", bots)
	}
}

func TestDriverEnvInjectedTokens(t *testing.T) {
	r := Resolved{Agent: AgentConfig{GatewayBaseURL: "https://gw.example/v1"}}
	// Injected token is used, regardless of the (empty) config value.
	env := r.DriverEnv("sk_injected", "", nil)
	var sawToken, sawBase bool
	for _, e := range env {
		if e == "ANTHROPIC_AUTH_TOKEN=sk_injected" {
			sawToken = true
		}
		if e == "ANTHROPIC_BASE_URL=https://gw.example/v1" {
			sawBase = true
		}
	}
	if !sawToken || !sawBase {
		t.Fatalf("env missing entries: token=%v base=%v (%v)", sawToken, sawBase, env)
	}
	// Empty token omits the auth var.
	for _, e := range r.DriverEnv("", "", nil) {
		if e == "ANTHROPIC_AUTH_TOKEN=" {
			t.Fatal("empty token must not emit ANTHROPIC_AUTH_TOKEN")
		}
	}
}

func TestNoBotsAllowed(t *testing.T) {
	// Empty bots[] is a valid first-run state — the GUI writes config.json
	// before the user adds any bots, and the daemon must stay up serving an
	// empty bots.list so SaveConfig → RestartCore can later add them.
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, `{"apiUrl":"https://o"}`) // no bots[]
	bots, err := Load(cfg)
	if err != nil {
		t.Fatalf("config with no bots must be accepted (first-run): %v", err)
	}
	if len(bots) != 0 {
		t.Fatalf("expected zero bots, got %d", len(bots))
	}
}

func TestMissingConfigAllowed(t *testing.T) {
	// Same reason as TestNoBotsAllowed: a missing config.json is the very
	// first launch — no error, empty roster.
	bots, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("missing config must be accepted (first-run): %v", err)
	}
	if len(bots) != 0 {
		t.Fatalf("expected zero bots, got %d", len(bots))
	}
}

func TestGroupConfigDirResolution(t *testing.T) {
	dir := t.TempDir()
	gcd := filepath.Join(dir, "groupcfg") // outside any bot's workspace
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, `{
		"bots":[
			{"id":"a","apiUrl":"https://o","octoToken":"x"},
			{"id":"b","octoToken":"y","groupConfigDir":`+jsonStr(gcd)+`}
		]
	}`)
	bots, err := Load(cfg)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if bots[0].GroupConfigDir != "" {
		t.Fatalf("bot a should not inherit groupConfigDir, got %q", bots[0].GroupConfigDir)
	}
	if bots[1].GroupConfigDir != gcd {
		t.Fatalf("bot b should use its own groupConfigDir, got %q", bots[1].GroupConfigDir)
	}
}

func TestGroupConfigDirInsideCwdRejected(t *testing.T) {
	dir := t.TempDir()
	// CwdBase for bot "a" is <dir>/a/workspace. A groupConfigDir nested under it
	// is an injection sink (agent-writable) and must be rejected.
	bad := filepath.Join(dir, "a", "workspace", "groupcfg")
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, `{"bots":[{"id":"a","apiUrl":"https://o","octoToken":"x","groupConfigDir":`+jsonStr(bad)+`}]}`)
	if _, err := Load(cfg); err == nil {
		t.Fatal("groupConfigDir nested under cwdBase must be rejected")
	}

	// Equal to cwdBase is also rejected.
	bad2 := filepath.Join(dir, "a", "workspace")
	writeFile(t, cfg, `{"bots":[{"id":"a","apiUrl":"https://o","octoToken":"x","groupConfigDir":`+jsonStr(bad2)+`}]}`)
	if _, err := Load(cfg); err == nil {
		t.Fatal("groupConfigDir equal to cwdBase must be rejected")
	}
}

// jsonStr renders s as a JSON string literal (handles Windows backslashes).
func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
