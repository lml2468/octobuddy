package config

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestOnBehalfOfParsedAsPersonaClone(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, `{"bots":[{"id":"clone","apiUrl":"https://octo.example","octoToken":"bf_x","onBehalfOf":{"uid":"u_admin","name":"Admin","personaPrompt":"reply in English"}}]}`)

	bots, err := Load(cfg)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	b := bots[0]
	if b.OnBehalfOf.UID != "u_admin" || b.OnBehalfOf.Name != "Admin" || b.OnBehalfOf.PersonaPrompt != "reply in English" {
		t.Fatalf("onBehalfOf not parsed: %+v", b.OnBehalfOf)
	}
}

func TestNoOnBehalfOfIsRegularBot(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, `{"bots":[{"id":"plain","apiUrl":"https://octo.example","octoToken":"bf_x"}]}`)

	bots, _ := Load(cfg)
	if bots[0].OnBehalfOf.UID != "" {
		t.Fatalf("regular bot must have empty grantor uid: %+v", bots[0].OnBehalfOf)
	}
}

// TestGatingFieldsResolution verifies the G12/G14/blocklist gating lists are
// per-bot only; top-level gating fields are not part of the canonical schema.
func TestGatingFieldsResolution(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, `{
	  "bots":[
	    {"id":"plain","apiUrl":"https://octo.example","octoToken":"bf_a"},
	    {"id":"custom","apiUrl":"https://octo.example","octoToken":"bf_b",
	     "trigger":{"mentionFreeGroups":["g-bot"]},
	     "knownBotUids":[],
	     "allowedBotUids":["ab-bot"],
	     "botBlocklist":["bad2","bad3"]}
	  ]
	}`)

	bots, err := Load(cfg)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	byID := resolvedByID(bots)
	plain, custom := byID["plain"], byID["custom"]

	assertPlainGatingFields(t, plain)
	assertCustomGatingFields(t, custom)
}

func resolvedByID(bots []Resolved) map[string]Resolved {
	byID := map[string]Resolved{}
	for _, b := range bots {
		byID[b.BotID] = b
	}
	return byID
}

func assertPlainGatingFields(t *testing.T, plain Resolved) {
	t.Helper()

	if len(plain.Trigger.MentionFreeGroups) != 0 {
		t.Fatalf("plain trigger.mentionFreeGroups = %v, want []", plain.Trigger.MentionFreeGroups)
	}
	if len(plain.KnownBotUids) != 0 {
		t.Fatalf("plain knownBotUids = %v, want []", plain.KnownBotUids)
	}
	if len(plain.AllowedBotUids) != 0 {
		t.Fatalf("plain allowedBotUids = %v, want []", plain.AllowedBotUids)
	}
	if len(plain.BotBlocklist) != 0 {
		t.Fatalf("plain botBlocklist = %v, want []", plain.BotBlocklist)
	}
}

func assertCustomGatingFields(t *testing.T, custom Resolved) {
	t.Helper()

	if len(custom.Trigger.MentionFreeGroups) != 1 || custom.Trigger.MentionFreeGroups[0] != "g-bot" {
		t.Fatalf("custom trigger.mentionFreeGroups = %v, want [g-bot]", custom.Trigger.MentionFreeGroups)
	}
	if len(custom.KnownBotUids) != 0 {
		t.Fatalf("custom knownBotUids = %v, want []", custom.KnownBotUids)
	}
	if len(custom.AllowedBotUids) != 1 || custom.AllowedBotUids[0] != "ab-bot" {
		t.Fatalf("custom allowedBotUids = %v, want [ab-bot]", custom.AllowedBotUids)
	}
	if len(custom.BotBlocklist) != 2 {
		t.Fatalf("custom botBlocklist = %v, want [bad2 bad3]", custom.BotBlocklist)
	}
}

// Pre-#96 configs serialized env values as bare strings:
//
//	"env": {"OCTO_BOT_ID": "foo_bot"}
//
// The refactor switched env to map[string]{value,secretRef}. EnvValue's custom
// UnmarshalJSON keeps the legacy string shape readable so existing
// ~/.octobuddy/config.json files don't crash the daemon on first launch of the
// new build.
func TestEnvValueAcceptsLegacyString(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, `{"bots":[{"id":"alpha","apiUrl":"https://octo.example","agent":{"env":{
		"LEGACY":  "legacy-value",
		"MODERN":  {"value": "modern-value"},
		"SECRET":  {"secretRef": "env/SECRET"}
	}}}]}`)
	bots, err := Load(cfg)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	env := bots[0].Agent.Env
	if got, want := env["LEGACY"], (EnvValue{Value: "legacy-value"}); got != want {
		t.Errorf("LEGACY = %+v, want %+v", got, want)
	}
	if got, want := env["MODERN"], (EnvValue{Value: "modern-value"}); got != want {
		t.Errorf("MODERN = %+v, want %+v", got, want)
	}
	if got, want := env["SECRET"], (EnvValue{SecretRef: "env/SECRET"}); got != want {
		t.Errorf("SECRET = %+v, want %+v", got, want)
	}
}

// A non-string, non-object env payload (e.g. a number) must still surface a
// JSON type error rather than being silently coerced.
func TestEnvValueRejectsUnsupportedJSONType(t *testing.T) {
	var v EnvValue
	if err := json.Unmarshal([]byte(`42`), &v); err == nil {
		t.Fatal("expected error for numeric env value, got nil")
	}
}
