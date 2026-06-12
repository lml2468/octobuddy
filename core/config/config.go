// Package config implements XClaw's single-file configuration.
//
// One ~/.xclaw/config.json holds shared top-level defaults
// (apiUrl/agent/rateLimit/context) plus a bots[] list where each entry inlines a
// bot's id + octoToken + per-bot overrides. The per-bot data directory
// (~/.xclaw/<id>/data) is DERIVED from baseDir + id, never configurable — so a
// bot can't escape its own subtree. The bot's persona/behavior prompt lives in
// SOUL.md + AGENTS.md in ~/.xclaw/<id>/, not in config.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// AgentConfig is the on-disk "agent" block: the model and the model-gateway
// routing (base URL + token) plus any extra env vars injected into the agent CLI.
type AgentConfig struct {
	Model          string            `json:"model,omitempty"`
	GatewayBaseURL string            `json:"gatewayBaseUrl,omitempty"`
	GatewayToken   string            `json:"gatewayToken,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	// Cron enables the per-bot scheduled-task scheduler (#115). Off by default;
	// when true the bot loads <dataDir>/cron.json at startup and fires due tasks
	// through the gateway. Owner-gated create/delete is exposed over the control
	// bus (cron.create / cron.list / cron.delete).
	Cron bool `json:"cron,omitempty"`
}

// RateLimitConfig mirrors the on-disk rateLimit block.
type RateLimitConfig struct {
	MaxPerMinute int `json:"maxPerMinute,omitempty"`
}

// ContextConfig mirrors the on-disk context block.
type ContextConfig struct {
	MaxContextChars int `json:"maxContextChars,omitempty"`
}

// BotEntry is one bot's full inline configuration in the global config's bots[]
// list. octoToken is OPTIONAL — it may be injected at runtime (secret.inject)
// instead of stored here; apiUrl/agent/rateLimit/context override the global
// top-level defaults of the same name. The bot's persona/behavior prompt is NOT
// here — it lives in SOUL.md + AGENTS.md under ~/.xclaw/<id>/.
type BotEntry struct {
	ID        string           `json:"id,omitempty"`
	OctoToken string           `json:"octoToken,omitempty"`
	APIURL    string           `json:"apiUrl,omitempty"`
	Agent     *AgentConfig     `json:"agent,omitempty"`
	RateLimit *RateLimitConfig `json:"rateLimit,omitempty"`
	Context   *ContextConfig   `json:"context,omitempty"`
	// GroupConfigDir is an operator-controlled directory holding per-conversation
	// instruction files (<channelId>.md), injected as a trusted [Group instructions]
	// block. Overrides the top-level default. MUST be outside CwdBase — see Resolved.
	GroupConfigDir string `json:"groupConfigDir,omitempty"`
}

// File is the on-disk shape of the single ~/.xclaw/config.json. The top-level
// apiUrl/agent/rateLimit/context are shared defaults; each bots[] entry may
// override them.
type File struct {
	APIURL         string           `json:"apiUrl,omitempty"`
	Agent          *AgentConfig     `json:"agent,omitempty"`
	RateLimit      *RateLimitConfig `json:"rateLimit,omitempty"`
	Context        *ContextConfig   `json:"context,omitempty"`
	GroupConfigDir string           `json:"groupConfigDir,omitempty"`
	Bots           []BotEntry       `json:"bots,omitempty"`
}

// Resolved is a single bot's fully-resolved, ready-to-run configuration.
type Resolved struct {
	BotID     string
	APIURL    string
	OctoToken string

	Agent     AgentConfig
	RateLimit RateLimitConfig
	Context   ContextConfig

	// SystemPrompt is the operator-trusted persona/behavior prompt, assembled
	// from SOUL.md + AGENTS.md in the bot dir (not from config).
	SystemPrompt string

	// GroupConfigDir is the operator-controlled directory of per-conversation
	// instruction files (<channelId>.md → trusted [Group instructions] block).
	// Empty disables the feature. Validated to be outside CwdBase — its files are
	// injected UNSANITIZED into the system prompt, so it must not be the
	// agent-writable sandbox (else a user-driven agent could write its own future
	// instructions). Mirrors cc-channel-octo's assertGroupConfigDirOutsideCwd.
	GroupConfigDir string

	// Derived per-bot directories (never from file).
	DataDir         string // ~/.xclaw/<id>/data       — SQLite + state
	CwdBase         string // ~/.xclaw/<id>/workspace   — per-session cwd sandboxes
	MemoryBase      string // ~/.xclaw/<id>/memory      — per-session auto-memory (outside CwdBase)
	SkillsDir       string // ~/.xclaw/<id>/skills      — per-bot skills (shadow global)
	GlobalSkillsDir string // ~/.xclaw/skills           — install-wide skills
}

func defaults() Resolved {
	return Resolved{
		RateLimit: RateLimitConfig{MaxPerMinute: 5},
		Context:   ContextConfig{MaxContextChars: 6000},
	}
}

var slugRE = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// validSlug reports whether id is a safe subtree name (no path separators, not
// "." or "..").
func validSlug(id string) bool {
	return slugRE.MatchString(id) && id != "." && id != ".."
}

// DefaultConfigPath is ~/.xclaw/config.json.
func DefaultConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".xclaw", "config.json")
}

// Load reads the global config at path (or DefaultConfigPath) and resolves all
// bots. baseDir is the directory containing the config file.
func Load(path string) ([]Resolved, error) {
	if path == "" {
		path = DefaultConfigPath()
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	baseDir := filepath.Dir(abs)

	global, err := readFile(abs)
	if err != nil {
		return nil, err
	}
	return resolveBots(global, baseDir)
}

// readFile parses a config.json, returning a zero File if it doesn't exist.
func readFile(path string) (File, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return File{}, nil
	}
	if err != nil {
		return File{}, err
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		return File{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return f, nil
}

// resolveBots expands the single global config into one Resolved per bot,
// applying inlineBot-over-global-default precedence. SOUL.md + AGENTS.md are
// still read from each bot's ~/.xclaw/<id>/ directory.
func resolveBots(global File, baseDir string) ([]Resolved, error) {
	entries := global.Bots
	if len(entries) == 0 {
		return nil, fmt.Errorf("no bots configured (add at least one entry to bots[])")
	}

	var out []Resolved
	seenID := map[string]bool{}

	for i, bot := range entries {
		id := bot.ID
		if id == "" {
			id = fmt.Sprintf("bot%d", i)
		}
		if !validSlug(id) {
			return nil, fmt.Errorf("bot %q: invalid id — letters, digits, dot, underscore, hyphen only (no path separators)", id)
		}
		if seenID[id] {
			return nil, fmt.Errorf("duplicate bot id %q", id)
		}
		seenID[id] = true

		botRoot := filepath.Join(baseDir, id)

		r := defaults()
		r.BotID = id
		r.DataDir = filepath.Join(botRoot, "data")
		r.CwdBase = filepath.Join(botRoot, "workspace")
		r.MemoryBase = filepath.Join(botRoot, "memory")
		r.SkillsDir = filepath.Join(botRoot, "skills")
		r.GlobalSkillsDir = filepath.Join(baseDir, "skills")

		// precedence: inlineBot ?? global default
		r.APIURL = firstNonEmpty(bot.APIURL, global.APIURL)
		r.OctoToken = bot.OctoToken

		// shallow-merge agent/rateLimit/context: global default → inline bot keys
		mergeAgent(&r.Agent, global.Agent)
		mergeAgent(&r.Agent, bot.Agent)
		mergeRate(&r.RateLimit, global.RateLimit)
		mergeRate(&r.RateLimit, bot.RateLimit)
		mergeCtx(&r.Context, global.Context)
		mergeCtx(&r.Context, bot.Context)

		// System prompt: SOUL.md (identity) + AGENTS.md (behavior), file-based.
		r.SystemPrompt = soul(botRoot)

		// Per-bot groupConfigDir overrides the top-level default. Empty = feature off.
		r.GroupConfigDir = firstNonEmpty(bot.GroupConfigDir, global.GroupConfigDir)

		// validation. octoToken is intentionally NOT required: it may be omitted
		// from the file and injected at runtime via the control bus (secret.inject)
		// from the GUI's Keychain. The connector waits for a token before connecting.
		if r.APIURL != "" && !isAllowedURL(r.APIURL) {
			return nil, fmt.Errorf("bot %q: unsafe apiUrl %q (must be https:// or http://localhost; SSRF protection)", id, r.APIURL)
		}
		if r.Agent.GatewayBaseURL != "" && !isAllowedURL(r.Agent.GatewayBaseURL) {
			return nil, fmt.Errorf("bot %q: unsafe gatewayBaseUrl %q (SSRF protection)", id, r.Agent.GatewayBaseURL)
		}
		// groupConfigDir files are injected UNSANITIZED into the system prompt, so
		// the dir must NOT be the agent-writable sandbox — otherwise a user-driven
		// agent could write its own future instructions. Mirrors cc-channel-octo's
		// assertGroupConfigDirOutsideCwd.
		if err := assertGroupConfigDirOutsideCwd(id, r.GroupConfigDir, r.CwdBase); err != nil {
			return nil, err
		}

		out = append(out, r)
	}
	return out, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func mergeAgent(dst *AgentConfig, src *AgentConfig) {
	if src == nil {
		return
	}
	if src.Model != "" {
		dst.Model = src.Model
	}
	if src.GatewayBaseURL != "" {
		dst.GatewayBaseURL = src.GatewayBaseURL
	}
	if src.GatewayToken != "" {
		dst.GatewayToken = src.GatewayToken
	}
	// Cron is a capability switch: a true at either the global or per-bot layer
	// enables it (consistent with the other fields' "non-zero overrides" merge).
	if src.Cron {
		dst.Cron = true
	}
	// env merges per-key (global base + per-bot overrides/additions), not whole
	// replacement — so a bot can add its own OCTO_BOT_ID without dropping a
	// globally-shared key.
	if len(src.Env) > 0 {
		if dst.Env == nil {
			dst.Env = map[string]string{}
		}
		for k, v := range src.Env {
			dst.Env[k] = v
		}
	}
}

func mergeRate(dst *RateLimitConfig, src *RateLimitConfig) {
	if src != nil && src.MaxPerMinute > 0 {
		dst.MaxPerMinute = src.MaxPerMinute
	}
}

func mergeCtx(dst *ContextConfig, src *ContextConfig) {
	if src != nil && src.MaxContextChars > 0 {
		dst.MaxContextChars = src.MaxContextChars
	}
}

// soul assembles the bot's operator-trusted system prompt from two files in its
// dir: SOUL.md (identity/persona) followed by AGENTS.md (behavior norms). Each
// is trimmed; missing/empty files are skipped. Returns "" if neither exists.
func soul(botRoot string) string {
	var parts []string
	for _, name := range []string{"SOUL.md", "AGENTS.md"} {
		data, err := os.ReadFile(filepath.Join(botRoot, name))
		if err != nil {
			continue
		}
		if s := strings.TrimSpace(string(data)); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n\n")
}

// assertGroupConfigDirOutsideCwd enforces that groupConfigDir (whose files are
// injected UNSANITIZED into the system prompt) is neither the agent-writable
// cwdBase nor nested under it. Otherwise a user-driven agent (default
// allowedTools '*' + bypassPermissions) could write <groupConfigDir>/<id>.md and
// inject its own future trusted instructions. Empty dir = feature off, no check.
//
// Resolves to real paths when they exist (so a symlink can't dodge the boundary)
// and falls back to a lexical clean for not-yet-created dirs. Mirrors
// cc-channel-octo config.ts assertGroupConfigDirOutsideCwd.
func assertGroupConfigDirOutsideCwd(botID, groupConfigDir, cwdBase string) error {
	if groupConfigDir == "" {
		return nil
	}
	gd := canonicalPath(groupConfigDir)
	cb := canonicalPath(cwdBase)
	if cb != "" && (gd == cb || isPathInside(gd, cb)) {
		return fmt.Errorf("bot %q: unsafe groupConfigDir %q — it is the same as or nested under the agent-writable cwdBase %q; "+
			"its files are injected into the system prompt, so it must be operator-controlled and outside the sandbox",
			botID, groupConfigDir, cwdBase)
	}
	return nil
}

// canonicalPath resolves p to its real path when it exists (defeating symlink
// dodges) and otherwise to an absolute lexical clean.
func canonicalPath(p string) string {
	if p == "" {
		return ""
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	if abs, err := filepath.Abs(p); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(p)
}

// isPathInside reports whether child is strictly nested under parent.
func isPathInside(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// DriverEnv builds the KEY=VALUE environment to layer onto the claude CLI's
// process env: the user-declared agent.env plus the model-gateway routing vars
// (mapped to the names claude understands), appended last so they win over any
// same-named agent.env entry.
//
//	ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN
func (r Resolved) DriverEnv() []string {
	return r.DriverEnvWith(r.Agent.GatewayToken)
}

// DriverEnvWith is DriverEnv with the gateway token supplied explicitly, so the
// caller can pass a runtime-injected token (from the in-memory secret store)
// instead of the config-file value. An empty gatewayToken omits the auth var.
//
// Security note: the token is handed to the spawned `claude` child as an
// environment variable. On Linux that makes it readable from
// /proc/<pid>/environ by any same-uid process (and via `ps eww`), so the
// in-memory-only secret store's guarantee does not extend past the exec
// boundary. This is the accepted tradeoff documented in SECURITY.md — the
// agent CLI takes its credentials via env, and the daemon runs as the operator.
func (r Resolved) DriverEnvWith(gatewayToken string) []string {
	var out []string
	for k, v := range r.Agent.Env {
		out = append(out, k+"="+v)
	}
	if r.Agent.GatewayBaseURL != "" {
		out = append(out, "ANTHROPIC_BASE_URL="+r.Agent.GatewayBaseURL)
	}
	if gatewayToken != "" {
		out = append(out, "ANTHROPIC_AUTH_TOKEN="+gatewayToken)
	}
	return out
}
