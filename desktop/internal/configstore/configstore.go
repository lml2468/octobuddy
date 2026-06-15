// Package configstore reads and writes the daemon's ~/.xclaw/config.json and the
// per-bot SOUL.md / AGENTS.md files, presenting a flat editor view model. It
// mirrors the legacy Swift ConfigStore: tokens are NEVER written to config.json
// (they live in the OS credential store via the secrets package) and are
// overlaid onto the view model on Load and stripped on Save.
package configstore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/lml2468/xclaw/core/config"
	"github.com/lml2468/xclaw/desktop/internal/secrets"
)

// BotConfig is the flat view model the editor binds to. Tokens are populated
// from the credential store on Load and routed back to it on Save.
type BotConfig struct {
	ID             string            `json:"id"`
	APIURL         string            `json:"apiUrl"`
	Model          string            `json:"model"`
	GatewayBaseURL string            `json:"gatewayBaseUrl"`
	OctoToken      string            `json:"octoToken"`
	GatewayToken   string            `json:"gatewayToken"`
	Env            map[string]string `json:"env"`
	Soul           string            `json:"soul"`
	Agents         string            `json:"agents"`
}

// Dir is ~/.xclaw.
func Dir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".xclaw")
}

// Path is ~/.xclaw/config.json.
func Path() string { return filepath.Join(Dir(), "config.json") }

func botDir(id string) string { return filepath.Join(Dir(), id) }

// readFile parses config.json into the daemon's File shape (empty File if absent).
func readFile() (config.File, error) {
	var f config.File
	raw, err := os.ReadFile(Path())
	if err != nil {
		if os.IsNotExist(err) {
			return f, nil
		}
		return f, err
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return f, fmt.Errorf("parse config.json: %w", err)
	}
	return f, nil
}

// BotIDs returns just the configured bot ids — used at startup to inject secrets.
func BotIDs() ([]string, error) {
	f, err := readFile()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(f.Bots))
	for _, b := range f.Bots {
		if b.ID != "" {
			ids = append(ids, b.ID)
		}
	}
	return ids, nil
}

// Load returns the editor view of every configured bot: config.json fields
// resolved against the top-level defaults, persona/behavior read from disk, and
// tokens overlaid from the credential store.
func Load() ([]BotConfig, error) {
	f, err := readFile()
	if err != nil {
		return nil, err
	}
	out := make([]BotConfig, 0, len(f.Bots))
	for _, b := range f.Bots {
		bc := BotConfig{ID: b.ID, Env: map[string]string{}}

		bc.APIURL = firstNonEmpty(b.APIURL, f.APIURL)
		// Agent fields: prefer per-bot, fall back to top-level defaults.
		var topModel, topGW string
		var topEnv map[string]string
		if f.Agent != nil {
			topModel, topGW, topEnv = f.Agent.Model, f.Agent.GatewayBaseURL, f.Agent.Env
		}
		if b.Agent != nil {
			bc.Model = firstNonEmpty(b.Agent.Model, topModel)
			bc.GatewayBaseURL = firstNonEmpty(b.Agent.GatewayBaseURL, topGW)
			for k, v := range b.Agent.Env {
				bc.Env[k] = v
			}
		} else {
			bc.Model, bc.GatewayBaseURL = topModel, topGW
		}
		if len(bc.Env) == 0 {
			for k, v := range topEnv {
				bc.Env[k] = v
			}
		}

		bc.Soul = readBotFile(b.ID, "SOUL.md")
		bc.Agents = readBotFile(b.ID, "AGENTS.md")
		bc.OctoToken = secrets.Get(b.ID, secrets.OctoToken)
		bc.GatewayToken = secrets.Get(b.ID, secrets.GatewayToken)
		out = append(out, bc)
	}
	return out, nil
}

// Save writes the view model back: config.json (top-level defaults preserved,
// tokens stripped), per-bot SOUL.md/AGENTS.md, tokens to the credential store,
// and prunes the directories of bots that were removed.
func Save(bots []BotConfig) error {
	for _, b := range bots {
		if !validSlug(b.ID) {
			return fmt.Errorf("invalid bot id %q — letters, digits, . _ - only", b.ID)
		}
		if err := validURL(b.APIURL); err != nil {
			return fmt.Errorf("bot %s: apiUrl: %w", b.ID, err)
		}
		if b.GatewayBaseURL != "" {
			if err := validURL(b.GatewayBaseURL); err != nil {
				return fmt.Errorf("bot %s: gatewayBaseUrl: %w", b.ID, err)
			}
		}
	}
	if dup := firstDuplicate(bots); dup != "" {
		return fmt.Errorf("duplicate bot id %q", dup)
	}

	// Preserve top-level defaults + unknown keys by editing the existing File.
	f, err := readFile()
	if err != nil {
		return err
	}
	oldIDs := map[string]bool{}
	for _, b := range f.Bots {
		oldIDs[b.ID] = true
	}

	entries := make([]config.BotEntry, 0, len(bots))
	newIDs := map[string]bool{}
	for _, b := range bots {
		newIDs[b.ID] = true
		entry := config.BotEntry{ID: b.ID, APIURL: b.APIURL}
		agent := &config.AgentConfig{Model: b.Model, GatewayBaseURL: b.GatewayBaseURL}
		if len(b.Env) > 0 {
			agent.Env = b.Env
		}
		entry.Agent = agent
		entries = append(entries, entry)

		if err := os.MkdirAll(botDir(b.ID), 0o755); err != nil {
			return err
		}
		if err := writeBotFile(b.ID, "SOUL.md", b.Soul); err != nil {
			return err
		}
		if err := writeBotFile(b.ID, "AGENTS.md", b.Agents); err != nil {
			return err
		}
		if err := secrets.Set(b.ID, secrets.OctoToken, b.OctoToken); err != nil {
			return fmt.Errorf("store octoToken for %s: %w", b.ID, err)
		}
		if err := secrets.Set(b.ID, secrets.GatewayToken, b.GatewayToken); err != nil {
			return fmt.Errorf("store gatewayToken for %s: %w", b.ID, err)
		}
	}
	f.Bots = entries

	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(Path(), append(raw, '\n'), 0o600); err != nil {
		return err
	}

	// Prune directories of removed bots (mirrors Swift removedSlugs behavior).
	for id := range oldIDs {
		if !newIDs[id] && validSlug(id) {
			_ = os.RemoveAll(botDir(id))
			_ = secrets.Delete(id, secrets.OctoToken)
			_ = secrets.Delete(id, secrets.GatewayToken)
		}
	}
	return nil
}

func readBotFile(id, name string) string {
	raw, err := os.ReadFile(filepath.Join(botDir(id), name))
	if err != nil {
		return ""
	}
	return string(raw)
}

func writeBotFile(id, name, content string) error {
	path := filepath.Join(botDir(id), name)
	if strings.TrimSpace(content) == "" {
		_ = os.Remove(path) // empty → omit the file
		return nil
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

var slugRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func validSlug(s string) bool {
	return s != "" && s != "." && s != ".." && slugRe.MatchString(s)
}

func validURL(s string) error {
	if strings.HasPrefix(s, "https://") {
		return nil
	}
	if strings.HasPrefix(s, "http://localhost") || strings.HasPrefix(s, "http://127.0.0.1") {
		return nil
	}
	return fmt.Errorf("use https:// (or http://localhost)")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstDuplicate(bots []BotConfig) string {
	seen := map[string]bool{}
	for _, b := range bots {
		if seen[b.ID] {
			return b.ID
		}
		seen[b.ID] = true
	}
	return ""
}
