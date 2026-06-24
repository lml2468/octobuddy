package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/lml2468/octobuddy/core/safepath"
)

func defaults() Resolved {
	return Resolved{
		RateLimit: RateLimitConfig{MaxPerMinute: 30},
		Context:   ContextConfig{MaxContextChars: 6000},
	}
}

var slugRE = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// validSlug reports whether id is a safe subtree name (no path separators, not
// "." or "..").
func validSlug(id string) bool {
	return slugRE.MatchString(id) && id != "." && id != ".."
}

// DefaultConfigPath is ~/.octobuddy/config.json.
func DefaultConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".octobuddy", "config.json")
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
	bots, err := resolveBots(global, baseDir)
	if err != nil {
		return nil, err
	}
	return bots, nil
}

// readFile parses a config.json, returning a zero File if it doesn't exist.
// Routes through safepath.SafeRead so an agent (Bash + bypass) that plants
// `~/.octobuddy/config.json → /attacker-controlled.json` cannot redirect the
// operator-trusted bot roster (URLs, ports, agent dirs, on-behalf-of
// grantors) at next daemon restart.
func readFile(path string) (File, error) {
	dir := filepath.Dir(path)
	leaf := filepath.Base(path)
	data, err := safepath.SafeRead(dir, leaf, 4<<20) // 4 MiB cap (1k-bot roster fits easily)
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
// still read from each bot's ~/.octobuddy/<id>/ directory.
func resolveBots(global File, baseDir string) ([]Resolved, error) {
	entries := global.Bots
	if len(entries) == 0 {
		// Empty roster is a valid first-run state: the GUI mints config.json
		// before the user adds any bots. The daemon stays up and serves an
		// empty bots.list so the GUI can add bots over the control bus.
		return nil, nil
	}

	var out []Resolved
	seenID := map[string]bool{}

	for i, bot := range entries {
		id, err := resolveBotID(bot, i, seenID)
		if err != nil {
			return nil, err
		}
		seenID[id] = true

		botRoot := filepath.Join(baseDir, id)
		r := buildResolvedBot(global, bot, id, botRoot)
		if err := validateResolvedBot(r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func resolveBotID(bot BotEntry, index int, seenID map[string]bool) (string, error) {
	id := bot.ID
	if id == "" {
		id = fmt.Sprintf("bot%d", index)
	}
	if !validSlug(id) {
		return "", fmt.Errorf("bot %q: invalid id — letters, digits, dot, underscore, hyphen only (no path separators)", id)
	}
	if seenID[id] {
		return "", fmt.Errorf("duplicate bot id %q", id)
	}
	return id, nil
}

func buildResolvedBot(global File, bot BotEntry, id, botRoot string) Resolved {
	r := defaults()
	r.BotID = id
	r.DataDir = filepath.Join(botRoot, "data")
	r.CwdBase = filepath.Join(botRoot, "workspace")
	r.MemoryBase = filepath.Join(botRoot, "memory")
	r.ClaudeConfigDir = filepath.Join(botRoot, ".claude")

	// Bot identity/agent config is per-bot only. Top-level config carries
	// shared runtime policy (rateLimit/context), not bot defaults.
	r.APIURL = bot.APIURL
	r.OctoToken = bot.OctoToken

	// shallow-merge runtime policy defaults first, then the per-bot override.
	mergeAgent(&r.Agent, bot.Agent)
	// settingSources defaults to user-scope only (drops project/local so a
	// planted CLAUDE.md in the agent-writable cwd can't influence the model).
	if len(r.Agent.SettingSources) == 0 {
		r.Agent.SettingSources = []string{"user"}
	}
	mergeRate(&r.RateLimit, global.RateLimit)
	mergeRate(&r.RateLimit, bot.RateLimit)
	mergeCtx(&r.Context, global.Context)
	mergeCtx(&r.Context, bot.Context)

	// Gating lists are per-bot policy. Nil and empty both resolve to "unset".
	r.KnownBotUids = bot.KnownBotUids
	r.AllowedBotUids = bot.AllowedBotUids
	r.BotBlocklist = bot.BotBlocklist

	// System prompt: SOUL.md (identity) + AGENTS.md (behavior), file-based.
	r.SystemPrompt = soul(botRoot)

	// Per-bot groupConfigDir. Empty = feature off.
	r.GroupConfigDir = bot.GroupConfigDir

	// Persona clone (openclaw OBO): grantor identity comes from config, not
	// from message payloads. A nil block leaves r.OnBehalfOf zero (regular bot).
	if bot.OnBehalfOf != nil {
		r.OnBehalfOf = *bot.OnBehalfOf
	}

	// Trigger pipeline policy. nil block → zero TriggerConfig →
	// triggerPolicyFromConfig applies the safe defaults.
	if bot.Trigger != nil {
		r.Trigger = *bot.Trigger
	}
	return r
}

func validateResolvedBot(r Resolved) error {
	// validation. octoToken is intentionally NOT required: it may be omitted
	// from the file and injected at runtime via the control bus (secret.inject)
	// from the GUI's secret backend. The connector waits for a token before connecting.
	if r.APIURL != "" && !IsAllowedURL(r.APIURL) {
		return fmt.Errorf("bot %q: unsafe apiUrl %q (must be https:// or http://localhost; SSRF protection)", r.BotID, r.APIURL)
	}
	if r.Agent.GatewayBaseURL != "" && !IsAllowedURL(r.Agent.GatewayBaseURL) {
		return fmt.Errorf("bot %q: unsafe gatewayBaseUrl %q (SSRF protection)", r.BotID, r.Agent.GatewayBaseURL)
	}
	// groupConfigDir files are injected UNSANITIZED into the system prompt, so
	// the dir must NOT be the agent-writable sandbox — otherwise a user-driven
	// agent could write its own future instructions. Mirrors cc-channel-octo's
	// assertGroupConfigDirOutsideCwd.
	if err := assertGroupConfigDirOutsideCwd(r.BotID, r.GroupConfigDir, r.CwdBase); err != nil {
		return err
	}
	return validateAgentTooling(r.BotID, r.Agent)
}

var toolNameRE = regexp.MustCompile(`^[A-Za-z0-9_.*-]+$`)

// validateAgentTooling rejects malformed settingSources and tool names. Tool
// names are joined comma-separated into a single --tools value, so a comma or
// space in a name would silently split it; settingSources is restricted to the
// scopes the driver supports ("local" is intentionally excluded).
func validateAgentTooling(botID string, a AgentConfig) error {
	for _, s := range a.SettingSources {
		if s != "user" && s != "project" {
			return fmt.Errorf("bot %q: invalid settingSources %q (allowed: user, project)", botID, s)
		}
	}
	if a.Tools == nil {
		return nil
	}
	if err := validateToolNames(botID, "tools.default", a.Tools.Default); err != nil {
		return err
	}
	for ch, names := range a.Tools.Channels {
		if err := validateToolNames(botID, "tools.channels["+ch+"]", names); err != nil {
			return err
		}
	}
	return nil
}

func validateToolNames(botID, where string, names []string) error {
	for _, n := range names {
		if !toolNameRE.MatchString(n) {
			return fmt.Errorf("bot %q: invalid tool name %q in %s (letters/digits/_.*- only; no commas or spaces)", botID, n, where)
		}
	}
	return nil
}

func mergeAgent(dst *AgentConfig, src *AgentConfig) {
	if src == nil {
		return
	}
	mergeAgentScalars(dst, src)
	mergeAgentCapabilities(dst, src)
	mergeAgentEnv(dst, src.Env)
	mergeAgentTooling(dst, src)
}

// mergeAgentTooling copies the per-bot tool policy + setting sources. Both are
// per-bot only (no global agent default), so a non-nil source value replaces,
// deep-copied so the resolved bot can't be mutated through the shared File.
func mergeAgentTooling(dst *AgentConfig, src *AgentConfig) {
	if src.Tools != nil {
		dst.Tools = src.Tools.clone()
	}
	if src.SettingSources != nil {
		dst.SettingSources = cloneStrs(src.SettingSources)
	}
}

// cloneStrs deep-copies a string slice, preserving the nil vs empty-slice
// distinction (nil = unset → driver default; empty = explicit "no tools").
func cloneStrs(s []string) []string {
	if s == nil {
		return nil
	}
	return append([]string{}, s...)
}

func mergeAgentScalars(dst *AgentConfig, src *AgentConfig) {
	if src.Model != "" {
		dst.Model = src.Model
	}
	if src.GatewayBaseURL != "" {
		dst.GatewayBaseURL = src.GatewayBaseURL
	}
	if src.GatewayToken != "" {
		dst.GatewayToken = src.GatewayToken
	}
	if src.DispatchTimeoutSec > 0 {
		dst.DispatchTimeoutSec = src.DispatchTimeoutSec
	}
	if src.SystemPromptMode != "" {
		dst.SystemPromptMode = src.SystemPromptMode
	}
}

func mergeAgentCapabilities(dst *AgentConfig, src *AgentConfig) {
	// Capability switches: a true at either the global or per-bot layer enables
	// it (consistent with the other fields' "non-zero overrides" merge).
	if src.Cron != nil {
		// Pointer override (per-bot wins, true OR false), so a per-bot false can
		// disable a top-level cron:true default — what the previous OR-only
		// merge made impossible.
		dst.Cron = src.Cron
	}
	if src.ToolProgress {
		dst.ToolProgress = true
	}
	if src.InheritUserConfig {
		dst.InheritUserConfig = true
	}
}

func mergeAgentEnv(dst *AgentConfig, env map[string]EnvValue) {
	// Env is per-bot only in the canonical config. Merge per-key into dst so
	// defaults() can still seed future built-in env entries if needed.
	if len(env) > 0 {
		if dst.Env == nil {
			dst.Env = map[string]EnvValue{}
		}
		for k, v := range env {
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
//
// Routes through safepath.SafeRead so an agent (Bash + bypass) that
// plants `~/.octobuddy/<id>/SOUL.md → /Users/victim/.aws/credentials` cannot
// redirect the trusted-prompt source. The bytes from the symlink target
// would otherwise have been injected verbatim as TrustedText into every
// system prompt, leaking the file contents on next reply.
func soul(botRoot string) string {
	var parts []string
	for _, name := range []string{"SOUL.md", "AGENTS.md"} {
		data, err := safepath.SafeRead(botRoot, name, 1<<20) // 1 MiB cap; errors on oversize
		if err != nil {
			continue
		}
		if s := strings.TrimSpace(string(data)); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n\n")
}
