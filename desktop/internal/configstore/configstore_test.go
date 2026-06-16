package configstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/lml2468/xclaw/core/config"
	"github.com/zalando/go-keyring"
)

// setup points the store at a temp HOME and an in-memory keychain so tests
// never touch the real ~/.xclaw or the OS credential store.
func setup(t *testing.T) {
	t.Helper()
	keyring.MockInit()
	t.Setenv("HOME", t.TempDir())
}

func writeConfig(t *testing.T, f config.File) {
	t.Helper()
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.MarshalIndent(f, "", "  ")
	if err := os.WriteFile(Path(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readBack(t *testing.T) config.File {
	t.Helper()
	var f config.File
	raw, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// A round-trip through Load → Save must not drop the per-bot overrides the
// editor view model doesn't carry (regression for the config-editor data-loss
// finding). Mirrors the dropped Swift configPreservesModelAndUnmanagedKeys test.
func TestSavePreservesUnmodeledPerBotFields(t *testing.T) {
	setup(t)
	writeConfig(t, config.File{
		APIURL: "https://top.example",
		Agent:  &config.AgentConfig{Model: "top-model"},
		Bots: []config.BotEntry{{
			ID:                "a",
			RateLimit:         &config.RateLimitConfig{MaxPerMinute: 7},
			Context:           &config.ContextConfig{MaxContextChars: 1234},
			GroupConfigDir:    "/srv/groups",
			OnBehalfOf:        &config.OnBehalfOf{UID: "grantor-9"},
			MentionFreeGroups: []string{"g1", "g2"},
			Agent:             &config.AgentConfig{Cron: true, ToolProgress: true},
		}},
	})

	loaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 {
		t.Fatalf("want 1 bot, got %d", len(loaded))
	}
	// Simulate an unrelated edit (change the persona text) and save.
	loaded[0].Soul = "edited"
	if err := Save(loaded, nil); err != nil {
		t.Fatal(err)
	}

	f := readBack(t)
	if len(f.Bots) != 1 {
		t.Fatalf("want 1 bot after save, got %d", len(f.Bots))
	}
	b := f.Bots[0]
	if b.OnBehalfOf == nil || b.OnBehalfOf.UID != "grantor-9" {
		t.Errorf("onBehalfOf dropped: %+v", b.OnBehalfOf)
	}
	if b.RateLimit == nil || b.RateLimit.MaxPerMinute != 7 {
		t.Errorf("rateLimit dropped: %+v", b.RateLimit)
	}
	if b.Context == nil || b.Context.MaxContextChars != 1234 {
		t.Errorf("context dropped: %+v", b.Context)
	}
	if b.GroupConfigDir != "/srv/groups" {
		t.Errorf("groupConfigDir dropped: %q", b.GroupConfigDir)
	}
	if len(b.MentionFreeGroups) != 2 {
		t.Errorf("mentionFreeGroups dropped: %v", b.MentionFreeGroups)
	}
	if b.Agent == nil || !b.Agent.Cron || !b.Agent.ToolProgress {
		t.Errorf("agent cron/toolProgress dropped: %+v", b.Agent)
	}
}

// Editor-owned values that merely equal the top-level default must NOT be
// materialized into the per-bot entry, so the default keeps propagating.
func TestSaveDoesNotMaterializeDefaults(t *testing.T) {
	setup(t)
	writeConfig(t, config.File{
		APIURL: "https://top.example",
		Agent:  &config.AgentConfig{Model: "top-model"},
		Bots:   []config.BotEntry{{ID: "a"}}, // inherits apiUrl + model
	})

	loaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded[0].APIURL != "https://top.example" || loaded[0].Model != "top-model" {
		t.Fatalf("Load should resolve defaults: %+v", loaded[0])
	}
	if err := Save(loaded, nil); err != nil {
		t.Fatal(err)
	}

	b := readBack(t).Bots[0]
	if b.APIURL != "" {
		t.Errorf("apiUrl materialized into bot entry: %q (should inherit)", b.APIURL)
	}
	if b.Agent != nil && b.Agent.Model != "" {
		t.Errorf("model materialized into bot entry: %q (should inherit)", b.Agent.Model)
	}
}

// Pruning happens ONLY for explicitly-removed ids whose data dir exists; a bot
// merely absent from the saved set (no explicit removal) is never wiped, and a
// removed id without a data/ dir is left alone.
func TestSavePrunesOnlyExplicitRemovals(t *testing.T) {
	setup(t)
	writeConfig(t, config.File{Bots: []config.BotEntry{{ID: "a"}, {ID: "b"}, {ID: "c"}}})
	for _, id := range []string{"a", "b", "c"} {
		if err := os.MkdirAll(filepath.Join(botDir(id), "data"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Keep a; drop b WITHOUT explicit removal; explicitly remove c.
	keep := []BotConfig{{ID: "a", APIURL: "https://x.example"}}
	if err := Save(keep, []string{"c"}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(botDir("a")); err != nil {
		t.Errorf("kept bot a was removed: %v", err)
	}
	if _, err := os.Stat(botDir("b")); err != nil {
		t.Errorf("bot b absent from set but NOT explicitly removed — must not be wiped: %v", err)
	}
	if _, err := os.Stat(botDir("c")); !os.IsNotExist(err) {
		t.Errorf("explicitly-removed bot c should be pruned, stat err=%v", err)
	}

	// A removed id whose data/ dir doesn't exist must be left alone.
	if err := os.MkdirAll(botDir("d"), 0o755); err != nil { // no data/ child
		t.Fatal(err)
	}
	if err := Save(keep, []string{"d"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(botDir("d")); err != nil {
		t.Errorf("removed id without data/ dir should not be RemoveAll'd: %v", err)
	}
}

// The per-bot skill allow-list must round-trip through Load → Save.
func TestSaveRoundTripsSkills(t *testing.T) {
	setup(t)
	writeConfig(t, config.File{Bots: []config.BotEntry{{ID: "a", APIURL: "https://x.example", Skills: []string{"pdf-tools", "octo-broadcast"}}}})
	loaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded[0].Skills) != 2 {
		t.Fatalf("Load skills = %v", loaded[0].Skills)
	}
	// Drop one and save.
	loaded[0].Skills = []string{"pdf-tools"}
	if err := Save(loaded, nil); err != nil {
		t.Fatal(err)
	}
	b := readBack(t).Bots[0]
	if len(b.Skills) != 1 || b.Skills[0] != "pdf-tools" {
		t.Fatalf("saved skills = %v, want [pdf-tools]", b.Skills)
	}
}
