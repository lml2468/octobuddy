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
	// UserHomeDir reads $HOME on unix but %USERPROFILE% on Windows — set both.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
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
			Agent:             &config.AgentConfig{Cron: ptrTo(true), ToolProgress: true},
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
	if b.Agent == nil || b.Agent.Cron == nil || !*b.Agent.Cron || !b.Agent.ToolProgress {
		t.Errorf("agent cron/toolProgress dropped: %+v", b.Agent)
	}
}

func ptrTo[T any](v T) *T { return &v }

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

// Pruning happens ONLY for explicitly-removed ids whose bot dir exists; a bot
// silently absent from the bots[] list (e.g. a hand-edited config.json without
// going through the GUI) must never be wiped — that would dataloss any bot the
// GUI hadn't yet learned about. The bot dir is the gate (not data/, which only
// the daemon creates) so a wizard Add-then-delete still prunes.
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

	// A removed id whose dir exists (even without daemon-created data/) MUST
	// be pruned — the wizard scaffolds SOUL/AGENTS before any daemon restart,
	// so an Add-then-immediately-delete used to orphan ~/.xclaw/<id>/ forever.
	if err := os.MkdirAll(botDir("d"), 0o755); err != nil { // no data/ child
		t.Fatal(err)
	}
	if err := Save(keep, []string{"d"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(botDir("d")); !os.IsNotExist(err) {
		t.Errorf("explicitly-removed bot d (no data/ child) should still be pruned, stat err=%v", err)
	}
}

// First-time Add-bot scaffolds SOUL.md + AGENTS.md with starter templates when
// the operator left the fields blank, so the bot dir is never naked after a
// successful Add-bot. Detected by botDir not existing before the Save.
func TestSaveScaffoldsTemplatesOnFirstCreate(t *testing.T) {
	setup(t)
	if err := Save([]BotConfig{{ID: "fresh", APIURL: "https://x.example"}}, nil); err != nil {
		t.Fatal(err)
	}
	soul, err := os.ReadFile(filepath.Join(botDir("fresh"), "SOUL.md"))
	if err != nil {
		t.Fatalf("SOUL.md should be scaffolded on first Save: %v", err)
	}
	if string(soul) != defaultSoulTemplate {
		t.Errorf("SOUL.md content not the template:\n%s", soul)
	}
	agents, err := os.ReadFile(filepath.Join(botDir("fresh"), "AGENTS.md"))
	if err != nil {
		t.Fatalf("AGENTS.md should be scaffolded on first Save: %v", err)
	}
	if string(agents) != defaultAgentsTemplate {
		t.Errorf("AGENTS.md content not the template:\n%s", agents)
	}
}

// First-time Save respects operator-provided content — the template only fills
// blanks, it never overwrites a non-empty value the editor sent.
func TestSaveFirstCreateKeepsOperatorContent(t *testing.T) {
	setup(t)
	if err := Save([]BotConfig{{
		ID: "fresh", APIURL: "https://x.example",
		Soul:   "I am Atlas.",
		Agents: "Be concise.",
	}}, nil); err != nil {
		t.Fatal(err)
	}
	soul, _ := os.ReadFile(filepath.Join(botDir("fresh"), "SOUL.md"))
	if string(soul) != "I am Atlas." {
		t.Errorf("operator SOUL.md overwritten by template: %q", soul)
	}
	agents, _ := os.ReadFile(filepath.Join(botDir("fresh"), "AGENTS.md"))
	if string(agents) != "Be concise." {
		t.Errorf("operator AGENTS.md overwritten by template: %q", agents)
	}
}

// A Save that leaves SOUL/AGENTS blank must not silently destroy the file
// that was scaffolded on first-save (or that an agent legitimately created).
// Prior behavior treated empty content as "delete the file"; that was a
// footgun + TOCTOU vector (Stat-then-write would overwrite an agent-planted
// SOUL.md whenever the operator's field happened to be blank). New
// semantics: blank field on an existing bot is a NO-OP; the file is
// preserved. Operators delete files from disk if they really want to.
func TestSaveBlankFieldsAreNoOpOnExistingBot(t *testing.T) {
	setup(t)
	// First Save creates the bot dir + scaffolds templates.
	if err := Save([]BotConfig{{ID: "ex", APIURL: "https://x.example"}}, nil); err != nil {
		t.Fatal(err)
	}
	soulPath := filepath.Join(botDir("ex"), "SOUL.md")
	agentsPath := filepath.Join(botDir("ex"), "AGENTS.md")
	before, err := os.ReadFile(soulPath)
	if err != nil {
		t.Fatalf("template should have scaffolded: %v", err)
	}
	// Second Save with blank fields must leave the scaffolded content alone.
	if err := Save([]BotConfig{{ID: "ex", APIURL: "https://x.example"}}, nil); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(soulPath)
	if err != nil {
		t.Fatalf("SOUL.md should NOT be removed on blank save: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("SOUL.md content changed on blank save")
	}
	if _, err := os.Stat(agentsPath); err != nil {
		t.Errorf("AGENTS.md should NOT be removed on blank save: %v", err)
	}
}

// TestSaveNeverWritesTokensToDisk is the regression for a credential-disclosure
// hazard: BotConfig still carries `json:"octoToken"` / `json:"gatewayToken"`
// tags (the headless daemon also reads from these fields), so a future refactor
// that forgets to strip them before MarshalIndent would silently leak both
// tokens into ~/.xclaw/config.json. This test asserts the on-disk JSON contains
// neither the raw token values nor the field names after Save.
func TestSaveNeverWritesTokensToDisk(t *testing.T) {
	setup(t)
	const octoSecret = "bf_test_octo_leak_canary"
	const gwSecret = "sk_test_gateway_leak_canary"
	bots := []BotConfig{{
		ID:           "alpha",
		APIURL:       "https://octo.example",
		Model:        "claude-opus-4-8",
		OctoToken:    octoSecret,
		GatewayToken: gwSecret,
		Env:          map[string]string{"OCTO_BOT_ID": "alpha_bot"},
	}}
	if err := Save(bots, nil); err != nil {
		t.Fatalf("save: %v", err)
	}

	raw, err := os.ReadFile(Path())
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	disk := string(raw)
	for _, banned := range []string{
		octoSecret,    // raw bf_ value
		gwSecret,      // raw sk_ value
		`"octoToken"`, // JSON tag — even an empty-valued field shouldn't appear
		`"gatewayToken"`,
	} {
		if filepathContains(disk, banned) {
			t.Fatalf("config.json must not contain %q — Save leaked it:\n%s", banned, disk)
		}
	}

	// Sanity: after Save+Load round-trip, the keychain restores the tokens.
	loaded, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded) != 1 || loaded[0].OctoToken != octoSecret || loaded[0].GatewayToken != gwSecret {
		t.Fatalf("tokens didn't survive the keychain round-trip: %+v", loaded)
	}
}

// filepathContains is a tiny indirection so the helper reads as a substring
// match without importing strings just for one call.
func filepathContains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestSaveRejectsDuplicateOctoBotID is the regression for R3: two bots
// must not share an OCTO_BOT_ID. They would otherwise share an octo-cli disk
// profile, and deleting one bot's profile would silently break the other's
// auth on its next agent spawn.
func TestSaveRejectsDuplicateOctoBotID(t *testing.T) {
	setup(t)
	err := Save([]BotConfig{
		{ID: "a", APIURL: "https://x.example", Env: map[string]string{"OCTO_BOT_ID": "27abc"}},
		{ID: "b", APIURL: "https://x.example", Env: map[string]string{"OCTO_BOT_ID": "27abc"}},
	}, nil)
	if err == nil {
		t.Fatal("Save with duplicate OCTO_BOT_ID must be rejected")
	}
	if !filepathContains(err.Error(), "OCTO_BOT_ID") {
		t.Fatalf("error should name the offending field: %v", err)
	}
	// Distinct robot ids — and bots without OCTO_BOT_ID at all — must continue to work.
	if err := Save([]BotConfig{
		{ID: "a", APIURL: "https://x.example", Env: map[string]string{"OCTO_BOT_ID": "27abc"}},
		{ID: "b", APIURL: "https://x.example", Env: map[string]string{"OCTO_BOT_ID": "27xyz"}},
		{ID: "c", APIURL: "https://x.example"},
	}, nil); err != nil {
		t.Fatalf("distinct OCTO_BOT_IDs should save cleanly: %v", err)
	}
}
