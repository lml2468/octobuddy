package configstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/lml2468/octobuddy/core/config"
	"github.com/lml2468/octobuddy/desktop/internal/secrets"
	"github.com/zalando/go-keyring"
)

// setup points the store at a temp HOME and an in-memory keyring so tests
// never touch the real ~/.octobuddy or OS credential store.
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
		Bots: []config.BotEntry{{
			APIURL:     "https://top.example",
			ID:         "a",
			RateLimit:  &config.RateLimitConfig{MaxPerMinute: 7},
			Context:    &config.ContextConfig{MaxContextChars: 1234},
			OnBehalfOf: &config.OnBehalfOf{UID: "grantor-9"},
			Trigger:    &config.TriggerConfig{MentionFreeGroups: []string{"g1", "g2"}},
			Agent: &config.AgentConfig{
				Model:              "m",
				Cron:               ptrTo(true),
				ToolProgress:       true,
				InheritUserConfig:  true,
				DispatchTimeoutSec: 3600,
			},
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
	if b.Trigger == nil || len(b.Trigger.MentionFreeGroups) != 2 {
		t.Errorf("trigger.mentionFreeGroups dropped: %+v", b.Trigger)
	}
	if b.Agent == nil || b.Agent.Cron == nil || !*b.Agent.Cron || !b.Agent.ToolProgress {
		t.Errorf("agent cron/toolProgress dropped: %+v", b.Agent)
	}
	if b.Agent == nil || !b.Agent.InheritUserConfig || b.Agent.DispatchTimeoutSec != 3600 {
		t.Errorf("unmodeled agent fields dropped: %+v", b.Agent)
	}
}

func ptrTo[T any](v T) *T { return &v }

// TestSavePreservesChannelToolOverrides is the regression for the BasicInfo
// Save clobbering per-channel tool overrides: BasicInfo owns only tools.default,
// so a save carrying a stale snapshot (or none) must NOT drop a channel override
// the chat window wrote live via SetChannelTools after the modal loaded.
func TestSavePreservesChannelToolOverrides(t *testing.T) {
	setup(t)
	writeConfig(t, config.File{Bots: []config.BotEntry{{
		ID: "b1", APIURL: "https://x.example",
		Agent: &config.AgentConfig{Model: "m", Tools: &config.ToolPolicy{
			Default:  []string{"Read"},
			Channels: map[string][]string{"chanA": {"Bash"}},
		}},
	}}})

	// The editor loaded a snapshot, then changed the bot-level default only.
	loaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a chat-window edit landing AFTER the modal loaded: add a new
	// channel override straight to disk.
	if err := SetChannelTools("b1", "chanB", []string{"Grep"}); err != nil {
		t.Fatalf("live channel set: %v", err)
	}
	// Operator edits the bot default and saves from the (now stale) modal.
	loaded[0].Tools = &config.ToolPolicy{Default: []string{"Read", "Edit"}}
	if err := Save(loaded, nil); err != nil {
		t.Fatal(err)
	}

	f := readBack(t)
	b := f.Bots[0]
	if b.Agent == nil || b.Agent.Tools == nil {
		t.Fatalf("tools dropped: %+v", b.Agent)
	}
	// Bot default reflects the editor's change.
	if !reflect.DeepEqual(b.Agent.Tools.Default, []string{"Read", "Edit"}) {
		t.Errorf("default = %v, want [Read Edit]", b.Agent.Tools.Default)
	}
	// Both the pre-existing and the live-added channel overrides survive.
	if got := b.Agent.Tools.Channels["chanA"]; !reflect.DeepEqual(got, []string{"Bash"}) {
		t.Errorf("chanA override clobbered: %v", got)
	}
	if got := b.Agent.Tools.Channels["chanB"]; !reflect.DeepEqual(got, []string{"Grep"}) {
		t.Errorf("live-added chanB override clobbered: %v", got)
	}
}

// TestSaveClearingDefaultToolsCollapsesEmptyAgent pins that clearing the
// bot-level whitelist leaves no "tools":{"default":null} cruft. (The agent block
// itself no longer collapses to nil — model is required, so it always carries at
// least Model — but the Tools sub-block must be dropped when empty.)
func TestSaveClearingDefaultToolsDropsToolsBlock(t *testing.T) {
	setup(t)
	writeConfig(t, config.File{Bots: []config.BotEntry{{
		ID: "b1", APIURL: "https://x.example",
		Agent: &config.AgentConfig{Model: "m", Tools: &config.ToolPolicy{Default: []string{"Read"}}},
	}}})
	loaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	loaded[0].Tools = &config.ToolPolicy{} // default undefined → "use driver default"
	if err := Save(loaded, nil); err != nil {
		t.Fatal(err)
	}
	b := readBack(t).Bots[0]
	if b.Agent == nil {
		t.Fatalf("agent should survive (model is required)")
	}
	if b.Agent.Tools != nil {
		t.Errorf("empty tools block should be dropped, got %+v", b.Agent.Tools)
	}
}

func TestSaveWritesPerBotFields(t *testing.T) {
	setup(t)
	writeConfig(t, config.File{
		Bots: []config.BotEntry{{ID: "a"}},
	})

	if err := Save([]BotConfig{{
		ID: "a", APIURL: "https://top.example", Model: "bot-model",
	}}, nil); err != nil {
		t.Fatal(err)
	}

	b := readBack(t).Bots[0]
	if b.APIURL != "https://top.example" {
		t.Errorf("apiUrl not written per-bot: %q", b.APIURL)
	}
	if b.Agent == nil || b.Agent.Model != "bot-model" {
		t.Errorf("model not written per-bot: %+v", b.Agent)
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
	keep := []BotConfig{{ID: "a", APIURL: "https://x.example", Model: "m"}}
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
	// so an Add-then-immediately-delete used to orphan ~/.octobuddy/<id>/ forever.
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
// TestBootstrapScaffoldOnceAndStaysDeleted pins the first-run ritual lifecycle:
// a brand-new bot gets BOOTSTRAP.md; once the bot deletes it (self-bootstrap
// complete), a later operator Save must NOT resurrect it; and an already-existing
// bot never gets one on an edit-save.
func TestBootstrapScaffoldOnceAndStaysDeleted(t *testing.T) {
	setup(t)
	bootPath := filepath.Join(botDir("fresh"), "BOOTSTRAP.md")

	if err := Save([]BotConfig{{ID: "fresh", APIURL: "https://x.example", Model: "m"}}, nil); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(bootPath); err != nil {
		t.Fatalf("BOOTSTRAP.md should be scaffolded for a new bot: %v", err)
	} else if string(b) != defaultBootstrapTemplate {
		t.Errorf("BOOTSTRAP.md content not the template:\n%s", b)
	}

	// The bot completes bootstrap and deletes the file.
	if err := os.Remove(bootPath); err != nil {
		t.Fatal(err)
	}
	// A later operator edit-save of the now-EXISTING bot must NOT recreate it.
	if err := Save([]BotConfig{{ID: "fresh", APIURL: "https://x.example", Model: "m", Soul: "I am Atlas."}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bootPath); !os.IsNotExist(err) {
		t.Fatalf("BOOTSTRAP.md must stay deleted on re-save, got err=%v", err)
	}
}

// TestBootstrapNotScaffoldedForExistingBot pins that a bot already present in
// config.json does not get a BOOTSTRAP.md when saved — only a brand-new id does.
func TestBootstrapNotScaffoldedForExistingBot(t *testing.T) {
	setup(t)
	writeConfig(t, config.File{Bots: []config.BotEntry{{ID: "existing", APIURL: "https://x.example"}}})
	if err := Save([]BotConfig{{ID: "existing", APIURL: "https://x.example", Model: "m"}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(botDir("existing"), "BOOTSTRAP.md")); !os.IsNotExist(err) {
		t.Fatalf("existing bot must NOT be scaffolded a BOOTSTRAP.md, got err=%v", err)
	}
}

func TestSaveScaffoldsTemplatesOnFirstCreate(t *testing.T) {
	setup(t)
	if err := Save([]BotConfig{{ID: "fresh", APIURL: "https://x.example", Model: "m"}}, nil); err != nil {
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

// TestDefaultTemplatesCarryLoadBearingSections guards against a careless edit
// gutting the opinionated default templates back to bare placeholders: each must
// keep its key sections so a freshly-scaffolded bot ships real guidance.
func TestDefaultTemplatesCarryLoadBearingSections(t *testing.T) {
	for _, want := range []string{"Core Truths", "Boundaries", "Vibe"} {
		if !strings.Contains(defaultSoulTemplate, want) {
			t.Errorf("defaultSoulTemplate missing %q section", want)
		}
	}
	for _, want := range []string{"Untrusted Input", "Red Lines", "External vs Internal", "Know When to Speak"} {
		if !strings.Contains(defaultAgentsTemplate, want) {
			t.Errorf("defaultAgentsTemplate missing %q section", want)
		}
	}
	// BOOTSTRAP must keep the closed-loop steps: a multi-turn clarification, an
	// owner-confirmation gate, and the REQUIRED self-delete that closes the
	// self-bootstrap loop. A careless edit dropping any of these would let a bot
	// finalize without owner sign-off or leave BOOTSTRAP.md behind.
	for _, want := range []string{"Take multiple turns", "confirm completeness", "REQUIRED last step", "Delete this BOOTSTRAP.md", "Verify the deletion"} {
		if !strings.Contains(defaultBootstrapTemplate, want) {
			t.Errorf("defaultBootstrapTemplate missing closed-loop step %q", want)
		}
	}
}

// First-time Save respects operator-provided content — the template only fills
// blanks, it never overwrites a non-empty value the editor sent.
func TestSaveFirstCreateKeepsOperatorContent(t *testing.T) {
	setup(t)
	if err := Save([]BotConfig{{
		ID: "fresh", APIURL: "https://x.example", Model: "m",
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
	if err := Save([]BotConfig{{ID: "ex", APIURL: "https://x.example", Model: "m"}}, nil); err != nil {
		t.Fatal(err)
	}
	soulPath := filepath.Join(botDir("ex"), "SOUL.md")
	agentsPath := filepath.Join(botDir("ex"), "AGENTS.md")
	before, err := os.ReadFile(soulPath)
	if err != nil {
		t.Fatalf("template should have scaffolded: %v", err)
	}
	// Second Save with blank fields must leave the scaffolded content alone.
	if err := Save([]BotConfig{{ID: "ex", APIURL: "https://x.example", Model: "m"}}, nil); err != nil {
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
// tokens into ~/.octobuddy/config.json. This test asserts the on-disk JSON contains
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

	// Sanity: after Save+Load round-trip, the secret backend restores the tokens.
	loaded, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded) != 1 || loaded[0].OctoToken != octoSecret || loaded[0].GatewayToken != gwSecret {
		t.Fatalf("tokens didn't survive the secret backend round-trip: %+v", loaded)
	}
}

func TestSaveSecretEnvUsesSecretRef(t *testing.T) {
	setup(t)
	const ghSecret = "ghp_secret_canary"
	bots := []BotConfig{{
		ID:        "alpha",
		APIURL:    "https://octo.example",
		Model:     "m",
		Env:       map[string]string{"PLAIN": "visible", "GH_TOKEN": ghSecret},
		SecretEnv: map[string]bool{"GH_TOKEN": true},
	}}
	if err := Save(bots, nil); err != nil {
		t.Fatalf("save: %v", err)
	}

	raw, err := os.ReadFile(Path())
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	if filepathContains(string(raw), ghSecret) {
		t.Fatalf("secret env value leaked to config.json:\n%s", raw)
	}
	b := readBack(t).Bots[0]
	if b.Agent == nil {
		t.Fatal("agent missing")
	}
	if got := b.Agent.Env["PLAIN"].Value; got != "visible" {
		t.Fatalf("plain env not written: %q", got)
	}
	if got := b.Agent.Env["GH_TOKEN"].SecretRef; got != "env/GH_TOKEN" {
		t.Fatalf("secretRef not written: %q", got)
	}
	if got := secrets.Get("alpha", secrets.Kind("env/GH_TOKEN")); got != ghSecret {
		t.Fatalf("secret backend value = %q", got)
	}

	loaded, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !loaded[0].SecretEnv["GH_TOKEN"] || loaded[0].Env["GH_TOKEN"] != "" {
		t.Fatalf("secret env should load locked without plaintext: %+v", loaded[0])
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

// TestSaveRejectsEmptyModel pins that model is required: a bot saved without a
// model is rejected (the backend half of the "model 必填" rule).
func TestSaveRejectsEmptyModel(t *testing.T) {
	setup(t)
	err := Save([]BotConfig{{ID: "a", APIURL: "https://x.example"}}, nil)
	if err == nil {
		t.Fatal("Save without a model must be rejected")
	}
	if !filepathContains(err.Error(), "model") {
		t.Fatalf("error should name the model field: %v", err)
	}
	// Whitespace-only model is also rejected (TrimSpace).
	if err := Save([]BotConfig{{ID: "a", APIURL: "https://x.example", Model: "  "}}, nil); err == nil {
		t.Fatal("whitespace-only model must be rejected")
	}
}

// TestSaveRejectsDuplicateOctoBotID is the regression for R3: two bots
// must not share an OCTO_BOT_ID. They would otherwise share an octo-cli disk
// profile, and deleting one bot's profile would silently break the other's
// auth on its next agent spawn.
func TestSaveRejectsDuplicateOctoBotID(t *testing.T) {
	setup(t)
	err := Save([]BotConfig{
		{ID: "a", APIURL: "https://x.example", Model: "m", Env: map[string]string{"OCTO_BOT_ID": "27abc"}},
		{ID: "b", APIURL: "https://x.example", Model: "m", Env: map[string]string{"OCTO_BOT_ID": "27abc"}},
	}, nil)
	if err == nil {
		t.Fatal("Save with duplicate OCTO_BOT_ID must be rejected")
	}
	if !filepathContains(err.Error(), "OCTO_BOT_ID") {
		t.Fatalf("error should name the offending field: %v", err)
	}
	// Distinct robot ids — and bots without OCTO_BOT_ID at all — must continue to work.
	if err := Save([]BotConfig{
		{ID: "a", APIURL: "https://x.example", Model: "m", Env: map[string]string{"OCTO_BOT_ID": "27abc"}},
		{ID: "b", APIURL: "https://x.example", Model: "m", Env: map[string]string{"OCTO_BOT_ID": "27xyz"}},
		{ID: "c", APIURL: "https://x.example", Model: "m"},
	}, nil); err != nil {
		t.Fatalf("distinct OCTO_BOT_IDs should save cleanly: %v", err)
	}
}

// TestSaveLoadToolsetPreservesBots verifies SaveToolset writes the top-level
// toolset block via read-modify-write without disturbing the bot roster, and
// LoadToolset reads it back.
func TestSaveLoadToolsetPreservesBots(t *testing.T) {
	setup(t)
	writeConfig(t, config.File{Bots: []config.BotEntry{{ID: "b1", APIURL: "https://x.example"}}})

	ts := &config.ToolsetCache{
		Version:      "2.1.187",
		ProbedAt:     123,
		Available:    []string{"Read", "Bash", "AskUserQuestion"},
		HeadlessSafe: []string{"Read", "Bash"},
	}
	if err := SaveToolset(ts); err != nil {
		t.Fatalf("SaveToolset: %v", err)
	}

	got, err := LoadToolset()
	if err != nil {
		t.Fatalf("LoadToolset: %v", err)
	}
	if got == nil || got.Version != "2.1.187" || len(got.HeadlessSafe) != 2 {
		t.Fatalf("toolset round-trip wrong: %+v", got)
	}

	// Bots must survive the toolset write.
	bots, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(bots) != 1 || bots[0].ID != "b1" {
		t.Fatalf("bot roster not preserved across SaveToolset: %+v", bots)
	}
}

// TestLoadToolsetAbsent returns nil (not an error) when never probed.
func TestLoadToolsetAbsent(t *testing.T) {
	setup(t)
	got, err := LoadToolset()
	if err != nil {
		t.Fatalf("LoadToolset on empty: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil toolset when unprobed, got %+v", got)
	}
}

// TestChannelToolsRoundTrip covers the chat-window per-channel tool override:
// set → read back; muzzle (empty) preserved; remove (nil) reverts; other
// channels + bot default untouched.
func TestChannelToolsRoundTrip(t *testing.T) {
	setup(t)
	writeConfig(t, config.File{Bots: []config.BotEntry{{
		ID: "b1", APIURL: "https://x.example",
		Agent: &config.AgentConfig{Tools: &config.ToolPolicy{
			Default:  []string{"Read", "Bash"},
			Channels: map[string][]string{"other": {"Grep"}},
		}},
	}}})

	if err := SetChannelTools("b1", "chanA", []string{"Read"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	tools, ok, err := ChannelTools("b1", "chanA")
	if err != nil || !ok || len(tools) != 1 || tools[0] != "Read" {
		t.Fatalf("read back: tools=%v ok=%v err=%v", tools, ok, err)
	}

	// Muzzle (explicit empty) survives.
	if err := SetChannelTools("b1", "chanA", []string{}); err != nil {
		t.Fatalf("muzzle set: %v", err)
	}
	tools, ok, _ = ChannelTools("b1", "chanA")
	if !ok || tools == nil || len(tools) != 0 {
		t.Fatalf("muzzle should be ok with empty list, got tools=%v ok=%v", tools, ok)
	}

	// Other channel + bot default preserved.
	other, ok, _ := ChannelTools("b1", "other")
	if !ok || len(other) != 1 || other[0] != "Grep" {
		t.Fatalf("other channel clobbered: %v ok=%v", other, ok)
	}
	bc, _, _ := LoadOne("b1")
	if bc.Tools == nil || len(bc.Tools.Default) != 2 {
		t.Fatalf("bot default clobbered: %+v", bc.Tools)
	}

	// Remove reverts to unconfigured.
	if err := SetChannelTools("b1", "chanA", nil); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, ok, _ := ChannelTools("b1", "chanA"); ok {
		t.Fatal("expected chanA override removed")
	}
}

func TestChannelToolsUnknownBot(t *testing.T) {
	setup(t)
	writeConfig(t, config.File{Bots: []config.BotEntry{{ID: "b1", APIURL: "https://x.example"}}})
	if err := SetChannelTools("nope", "c", []string{"Read"}); err == nil {
		t.Fatal("expected error for unknown bot")
	}
}
