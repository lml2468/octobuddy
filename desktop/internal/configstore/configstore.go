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
	// Skills is the bot's allow-list of global skill names (managed in the
	// Skills window, selected per bot in the editor).
	Skills []string `json:"skills"`
	// Workflows is the bot's allow-list of global workflow names.
	Workflows []string `json:"workflows"`
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
		// Skill allow-list: per-bot replaces the top-level default. Default to an
		// empty slice (not nil) so the editor checklist binds cleanly.
		bc.Skills = b.Skills
		if bc.Skills == nil {
			bc.Skills = f.Skills
		}
		if bc.Skills == nil {
			bc.Skills = []string{}
		}
		bc.Workflows = b.Workflows
		if bc.Workflows == nil {
			bc.Workflows = f.Workflows
		}
		if bc.Workflows == nil {
			bc.Workflows = []string{}
		}
		out = append(out, bc)
	}
	return out, nil
}

// Save writes the view model back to disk:
//   - config.json: each bot is MERGED onto its existing entry, so per-bot
//     overrides the editor doesn't model (rateLimit/context/groupConfigDir/
//     onBehalfOf and the mentionFreeGroups/knownBotUids/allowedBotUids/
//     botBlocklist gating lists, plus agent.cron/toolProgress) survive a Save;
//     editor-owned fields are persisted only when they DIFFER from the
//     top-level default (so the defaults keep propagating and aren't frozen
//     into N per-bot copies), and tokens are stripped (they live in the OS
//     keychain, never config.json);
//   - per-bot SOUL.md / AGENTS.md and tokens to the credential store;
//   - pruning ONLY the bots named in removedIDs — an explicit deletion list
//     from the editor — and only when their on-disk data dir actually exists.
//     Pruning is NEVER inferred from a set-difference: a stale editor snapshot
//     (a second session, a hand-edit, a restart-rewrite) would otherwise look
//     like "every other bot was removed" and irreversibly wipe their data.
//
// config.json is written (atomically) BEFORE the per-bot side effects, so a
// mid-way failure can't leave the index stale while bot files / keychain are
// already mutated; the side effects are idempotent, so a failed Save converges
// on retry.
func Save(bots []BotConfig, removedIDs []string) error {
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

	// Start from the existing File so top-level defaults + unknown keys survive.
	f, err := readFile()
	if err != nil {
		return err
	}
	existing := make(map[string]config.BotEntry, len(f.Bots))
	for _, e := range f.Bots {
		existing[e.ID] = e
	}
	var topModel, topGW string
	var topEnv map[string]string
	if f.Agent != nil {
		topModel, topGW, topEnv = f.Agent.Model, f.Agent.GatewayBaseURL, f.Agent.Env
	}

	entries := make([]config.BotEntry, 0, len(bots))
	for _, b := range bots {
		// Merge onto the existing entry (zero value for a new bot) so per-bot
		// overrides the editor doesn't model are preserved.
		entry := existing[b.ID]
		entry.ID = b.ID
		entry.OctoToken = "" // tokens live in the OS keychain, never config.json
		entry.APIURL = inheritStr(b.APIURL, f.APIURL)

		ag := config.AgentConfig{}
		if entry.Agent != nil {
			ag = *entry.Agent // preserve cron / toolProgress / any other agent fields
		}
		ag.GatewayToken = "" // keychain only
		ag.Model = inheritStr(b.Model, topModel)
		ag.GatewayBaseURL = inheritStr(b.GatewayBaseURL, topGW)
		if envEqual(b.Env, topEnv) {
			ag.Env = nil // inherited from the top-level default; don't materialize it
		} else {
			ag.Env = b.Env
		}
		if agentEmpty(ag) {
			entry.Agent = nil
		} else {
			entry.Agent = &ag
		}
		// Persist the per-bot skill allow-list (empty → omitempty drops it → the
		// bot inherits the top-level default / no global skills).
		entry.Skills = b.Skills
		entry.Workflows = b.Workflows
		entries = append(entries, entry)
	}
	f.Bots = entries

	// Write config.json first (atomically via temp+rename) — the authoritative
	// index the daemon reads.
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := Path() + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, Path()); err != nil {
		return err
	}

	// Per-bot side effects (idempotent). On failure, config.json already
	// reflects the intended set and a retry re-applies these.
	for _, b := range bots {
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

	// Prune ONLY explicitly-removed bots, and only when their data dir really
	// exists. Skip any id still present in the saved set.
	keep := make(map[string]bool, len(bots))
	for _, b := range bots {
		keep[b.ID] = true
	}
	for _, id := range removedIDs {
		if keep[id] || !validSlug(id) {
			continue
		}
		if _, err := os.Stat(filepath.Join(botDir(id), "data")); err != nil {
			continue // no data/ child → not an established bot dir; never RemoveAll
		}
		_ = os.RemoveAll(botDir(id))
		_ = secrets.Delete(id, secrets.OctoToken)
		_ = secrets.Delete(id, secrets.GatewayToken)
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

// inheritStr returns "" when v equals the top-level default def, so the per-bot
// field stays inherited (the default keeps propagating); otherwise it returns v.
// This is the inverse of Load's firstNonEmpty resolution.
func inheritStr(v, def string) string {
	if v == def {
		return ""
	}
	return v
}

func envEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

func agentEmpty(a config.AgentConfig) bool {
	return a.Model == "" && a.GatewayBaseURL == "" && a.GatewayToken == "" &&
		len(a.Env) == 0 && !a.Cron && !a.ToolProgress
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
