package config

import (
	"path/filepath"
	"testing"
)

// TestMultiBotIsolation verifies a 2-bot config resolves to two fully isolated
// configs: distinct ids, tokens, per-bot overrides, and derived data dirs under
// each bot's own subtree.
func TestMultiBotIsolation(t *testing.T) {
	a, b := loadMultiBotIsolationConfig(t)

	assertMultiBotTokensAndAPI(t, a, b)
	assertMultiBotContextIsolation(t, a, b)
	assertMultiBotDataDirs(t, a, b)
	assertMultiBotSandboxDirs(t, a, b)
}

func loadMultiBotIsolationConfig(t *testing.T) (Resolved, Resolved) {
	t.Helper()

	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, `{
	  "context":{"maxContextChars":4000},
	  "bots":[
	    {"id":"alpha","apiUrl":"https://octo.example","octoToken":"bf_alpha"},
	    {"id":"beta","apiUrl":"https://octo.example","octoToken":"bf_beta","context":{"maxContextChars":9000}}
	  ]
	}`)

	bots, err := Load(cfg)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(bots) != 2 {
		t.Fatalf("want 2 bots, got %d", len(bots))
	}

	byID := map[string]Resolved{}
	for _, b := range bots {
		byID[b.BotID] = b
	}
	a, b := byID["alpha"], byID["beta"]

	return a, b
}

func assertMultiBotTokensAndAPI(t *testing.T, a, b Resolved) {
	t.Helper()

	if a.OctoToken != "bf_alpha" || b.OctoToken != "bf_beta" {
		t.Fatalf("tokens not isolated: %q %q", a.OctoToken, b.OctoToken)
	}
	if a.APIURL != "https://octo.example" || b.APIURL != a.APIURL {
		t.Fatalf("apiUrl wrong: %q %q", a.APIURL, b.APIURL)
	}
}

func assertMultiBotContextIsolation(t *testing.T, a, b Resolved) {
	t.Helper()

	if a.Context.MaxContextChars != 4000 {
		t.Fatalf("alpha context = %d, want global 4000", a.Context.MaxContextChars)
	}
	if b.Context.MaxContextChars != 9000 {
		t.Fatalf("beta context = %d, want override 9000", b.Context.MaxContextChars)
	}
}

func assertMultiBotDataDirs(t *testing.T, a, b Resolved) {
	t.Helper()

	base := filepath.Dir(filepath.Dir(a.DataDir))
	if a.DataDir != filepath.Join(base, "alpha", "data") ||
		b.DataDir != filepath.Join(base, "beta", "data") {
		t.Fatalf("data dirs wrong: %q %q", a.DataDir, b.DataDir)
	}
	if a.DataDir == b.DataDir {
		t.Fatalf("data dirs not isolated between bots")
	}
}

func assertMultiBotSandboxDirs(t *testing.T, a, b Resolved) {
	t.Helper()

	if a.CwdBase == b.CwdBase || a.MemoryBase == b.MemoryBase || a.BotRoot == b.BotRoot {
		t.Fatalf("sandbox dirs not isolated: %+v / %+v", a, b)
	}
	base := filepath.Dir(filepath.Dir(a.CwdBase))
	if a.CwdBase != filepath.Join(base, "alpha", "workspace") {
		t.Fatalf("alpha CwdBase wrong: %q", a.CwdBase)
	}
}
