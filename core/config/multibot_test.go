package config

import (
	"path/filepath"
	"testing"
)

// TestMultiBotIsolation verifies a 2-bot config resolves to two fully isolated
// configs: distinct ids, tokens, and derived data/workspace/memory dirs under
// each bot's own subtree.
func TestMultiBotIsolation(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, `{
	  "apiUrl":"https://octo.example",
	  "context":{"maxContextChars":4000},
	  "bots":[
	    {"id":"alpha"},
	    {"id":"beta"}
	  ]
	}`)
	writeFile(t, filepath.Join(dir, "alpha", "config.json"), `{"octoToken":"bf_alpha"}`)
	writeFile(t, filepath.Join(dir, "beta", "config.json"), `{"octoToken":"bf_beta","context":{"maxContextChars":9000}}`)

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

	// distinct tokens
	if a.OctoToken != "bf_alpha" || b.OctoToken != "bf_beta" {
		t.Fatalf("tokens not isolated: %q %q", a.OctoToken, b.OctoToken)
	}
	// shared global apiUrl inherited by both
	if a.APIURL != "https://octo.example" || b.APIURL != a.APIURL {
		t.Fatalf("apiUrl not inherited: %q %q", a.APIURL, b.APIURL)
	}
	// per-bot context override only affects beta; alpha keeps the global
	if a.Context.MaxContextChars != 4000 {
		t.Fatalf("alpha context = %d, want global 4000", a.Context.MaxContextChars)
	}
	if b.Context.MaxContextChars != 9000 {
		t.Fatalf("beta context = %d, want override 9000", b.Context.MaxContextChars)
	}
	// derived dirs are under each bot's own subtree, and distinct
	if a.DataDir != filepath.Join(dir, "alpha", "data") ||
		b.DataDir != filepath.Join(dir, "beta", "data") {
		t.Fatalf("data dirs wrong: %q %q", a.DataDir, b.DataDir)
	}
	if a.CwdBase == b.CwdBase || a.MemoryBase == b.MemoryBase {
		t.Fatalf("workspace/memory not isolated between bots")
	}
}
