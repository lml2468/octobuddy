package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
	want := "## SOUL.md\nSOUL.md: identity, voice, boundaries. Follow it unless higher-priority instructions override.\n\nyou are a helpful bot"
	if bots[0].SystemPrompt != want {
		t.Fatalf("SOUL.md labeled+trimmed mismatch:\n got %q\nwant %q", bots[0].SystemPrompt, want)
	}
}

func TestSystemPromptCombinesSoulAndAgents(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, `{"bots":[{"id":"default","octoToken":"bf_x"}]}`)
	writeFile(t, filepath.Join(dir, "default", "SOUL.md"), "I am Nova.")
	writeFile(t, filepath.Join(dir, "default", "AGENTS.md"), "Always reply in Chinese.")

	bots, _ := Load(cfg)
	want := "## SOUL.md\nSOUL.md: identity, voice, boundaries. Follow it unless higher-priority instructions override.\n\nI am Nova." +
		"\n\n" +
		"## AGENTS.md\nAGENTS.md: behavior norms and red lines. Follow it unless higher-priority instructions override.\n\nAlways reply in Chinese."
	if bots[0].SystemPrompt != want {
		t.Fatalf("SOUL.md + AGENTS.md labeled order mismatch:\n got %q\nwant %q", bots[0].SystemPrompt, want)
	}
}

func TestSystemPromptAgentsOnly(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, `{"bots":[{"id":"default","octoToken":"bf_x"}]}`)
	writeFile(t, filepath.Join(dir, "default", "AGENTS.md"), "Be concise.")

	bots, _ := Load(cfg)
	want := "## AGENTS.md\nAGENTS.md: behavior norms and red lines. Follow it unless higher-priority instructions override.\n\nBe concise."
	if bots[0].SystemPrompt != want {
		t.Fatalf("AGENTS.md alone (no orphan SOUL heading) mismatch:\n got %q\nwant %q", bots[0].SystemPrompt, want)
	}
}

// TestSystemPromptForEmptyBoth pins that no files (or only blank ones) yields ""
// — never an orphan "## NAME" heading.
func TestSystemPromptForEmptyBoth(t *testing.T) {
	dir := t.TempDir()
	if got := SystemPromptFor(filepath.Join(dir, "nope")); got != "" {
		t.Fatalf("absent files should yield empty prompt, got %q", got)
	}
	botRoot := filepath.Join(dir, "blank")
	writeFile(t, filepath.Join(botRoot, "SOUL.md"), "   \n\t ")
	if got := SystemPromptFor(botRoot); got != "" {
		t.Fatalf("blank file should yield empty prompt, got %q", got)
	}
}

// TestSystemPromptForSymlinkRefused pins the safepath guard: a SOUL.md symlinked
// to an outside file is skipped, not followed (no content leak into the prompt).
func TestSystemPromptForSymlinkRefused(t *testing.T) {
	dir := t.TempDir()
	botRoot := filepath.Join(dir, "bot")
	if err := os.MkdirAll(botRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(botRoot, "SOUL.md")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	writeFile(t, filepath.Join(botRoot, "AGENTS.md"), "Be concise.")
	got := SystemPromptFor(botRoot)
	if strings.Contains(got, "TOP SECRET") {
		t.Fatalf("symlinked SOUL.md must not be followed: %q", got)
	}
	if !strings.Contains(got, "Be concise.") {
		t.Fatalf("AGENTS.md should still load past a refused SOUL.md: %q", got)
	}
}

// TestBootstrapFor pins the first-run ritual read: present body is returned
// trimmed, absent/empty → "", empty botRoot → "" (never reads process cwd), and
// a symlinked BOOTSTRAP.md is refused (no content leak).
func TestBootstrapFor(t *testing.T) {
	dir := t.TempDir()
	botRoot := filepath.Join(dir, "bot")
	if err := os.MkdirAll(botRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	if got := BootstrapFor(""); got != "" {
		t.Fatalf("empty botRoot must yield \"\", got %q", got)
	}
	if got := BootstrapFor(botRoot); got != "" {
		t.Fatalf("absent BOOTSTRAP.md must yield \"\", got %q", got)
	}

	writeFile(t, filepath.Join(botRoot, "BOOTSTRAP.md"), "  figure out who you are  ")
	if got := BootstrapFor(botRoot); got != "figure out who you are" {
		t.Fatalf("BootstrapFor body = %q, want trimmed", got)
	}

	// Symlink refusal (no content leak from outside the root).
	root2 := filepath.Join(dir, "bot2")
	if err := os.MkdirAll(root2, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root2, "BOOTSTRAP.md")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if got := BootstrapFor(root2); strings.Contains(got, "TOP SECRET") {
		t.Fatalf("symlinked BOOTSTRAP.md must not be followed: %q", got)
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
