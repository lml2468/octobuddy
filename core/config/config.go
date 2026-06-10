// Package config implements XClaw's two-layer, bot-first configuration, ported
// from cc-channel-octo's config.ts.
//
// Global ~/.xclaw/config.json holds shared defaults + a bots[] list (never a
// token). Per-bot ~/.xclaw/<id>/config.json holds that bot's token + overrides.
// Per-bot directories (data/workspace/memory/skills) are DERIVED from baseDir +
// id — never configurable — so a bot can't escape its own subtree.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// SDKConfig holds agent-driver settings (shared or per-bot).
type SDKConfig struct {
	Model           string            `json:"model,omitempty"`
	AllowedTools    json.RawMessage   `json:"allowedTools,omitempty"` // []string or "*"
	PermissionMode  string            `json:"permissionMode,omitempty"`
	MaxTurns        *int              `json:"maxTurns,omitempty"`
	SystemPrompt    string            `json:"systemPrompt,omitempty"`
	SettingSources  []string          `json:"settingSources,omitempty"`
	ToolProgress    *bool             `json:"toolProgress,omitempty"`
	AnthropicBaseURL string           `json:"anthropicBaseUrl,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	Driver          string            `json:"driver,omitempty"` // xclaw: which AgentDriver (claude|codex)
}

// RateLimitConfig mirrors rateLimit.*.
type RateLimitConfig struct {
	MaxPerMinute int `json:"maxPerMinute,omitempty"`
}

// ContextConfig mirrors context.*.
type ContextConfig struct {
	MaxContextChars int `json:"maxContextChars,omitempty"`
	HistoryLimit    int `json:"historyLimit,omitempty"`
}

// BotOverride is an inline entry in the global bots[] list. Note model /
// systemPrompt are FLAT here (vs nested sdk.* in a per-bot file).
type BotOverride struct {
	ID           string `json:"id,omitempty"`
	BotToken     string `json:"botToken,omitempty"`
	APIURL       string `json:"apiUrl,omitempty"`
	Model        string `json:"model,omitempty"`
	SystemPrompt string `json:"systemPrompt,omitempty"`
}

// File is the on-disk shape of a config.json (global or per-bot).
type File struct {
	BotToken        string           `json:"botToken,omitempty"`
	APIURL          string           `json:"apiUrl,omitempty"`
	OctoToken       string           `json:"octoToken,omitempty"` // xclaw: Octo bot token
	SDK             *SDKConfig       `json:"sdk,omitempty"`
	RateLimit       *RateLimitConfig `json:"rateLimit,omitempty"`
	Context         *ContextConfig   `json:"context,omitempty"`
	MaxResponseChars *int            `json:"maxResponseChars,omitempty"`
	Bots            []BotOverride    `json:"bots,omitempty"`
}

// Resolved is a single bot's fully-resolved, ready-to-run configuration.
type Resolved struct {
	BotID    string
	BotToken string
	APIURL   string
	OctoToken string

	SDK       SDKConfig
	RateLimit RateLimitConfig
	Context   ContextConfig
	MaxResponseChars int

	// Derived (never from file).
	BaseDir    string
	DataDir    string
	CwdBase    string
	MemoryBase string
	SkillsDir  string
	GlobalSkillsDir string
}

func defaults() Resolved {
	return Resolved{
		SDK: SDKConfig{
			PermissionMode: "bypassPermissions",
			SettingSources: []string{"project"},
			Driver:         "claude",
		},
		RateLimit:        RateLimitConfig{MaxPerMinute: 5},
		Context:          ContextConfig{MaxContextChars: 6000, HistoryLimit: 40},
		MaxResponseChars: 512 * 1024,
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

// resolveBots expands the global config into one Resolved per bot, applying the
// perBotFile ?? inlineBot ?? global precedence.
func resolveBots(global File, baseDir string) ([]Resolved, error) {
	entries := global.Bots
	if len(entries) == 0 {
		entries = []BotOverride{{ID: "default", BotToken: global.BotToken}}
	}

	var out []Resolved
	seenID := map[string]bool{}
	seenToken := map[string]bool{}

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
		perBot, err := readFile(filepath.Join(botRoot, "config.json"))
		if err != nil {
			return nil, err
		}

		r := defaults()
		r.BotID = id
		r.BaseDir = baseDir
		r.DataDir = filepath.Join(botRoot, "data")
		r.CwdBase = filepath.Join(botRoot, "workspace")
		r.MemoryBase = filepath.Join(botRoot, "memory")
		r.SkillsDir = filepath.Join(botRoot, "skills")
		r.GlobalSkillsDir = filepath.Join(baseDir, "skills")

		// precedence: perBotFile ?? inlineBot ?? global
		r.BotToken = firstNonEmpty(perBot.BotToken, bot.BotToken, global.BotToken)
		r.APIURL = firstNonEmpty(perBot.APIURL, bot.APIURL, global.APIURL)
		r.OctoToken = firstNonEmpty(perBot.OctoToken, global.OctoToken)

		// shallow-merge sdk/rateLimit/context: global → perBotFile keys
		mergeSDK(&r.SDK, global.SDK)
		mergeSDK(&r.SDK, perBot.SDK)
		// model/systemPrompt: SOUL.md > perBotFile.sdk > inlineBot > global
		if bot.Model != "" {
			r.SDK.Model = bot.Model
		}
		if perBot.SDK != nil && perBot.SDK.Model != "" {
			r.SDK.Model = perBot.SDK.Model
		}
		sysPrompt := firstNonEmpty(
			soul(botRoot),
			sdkSystemPrompt(perBot.SDK),
			bot.SystemPrompt,
			sdkSystemPrompt(global.SDK),
		)
		if sysPrompt != "" {
			r.SDK.SystemPrompt = sysPrompt
		}
		mergeRate(&r.RateLimit, global.RateLimit)
		mergeRate(&r.RateLimit, perBot.RateLimit)
		mergeCtx(&r.Context, global.Context)
		mergeCtx(&r.Context, perBot.Context)
		if global.MaxResponseChars != nil {
			r.MaxResponseChars = *global.MaxResponseChars
		}
		if perBot.MaxResponseChars != nil {
			r.MaxResponseChars = *perBot.MaxResponseChars
		}

		// validation
		if r.BotToken == "" && r.OctoToken == "" {
			return nil, fmt.Errorf("bot %q: missing botToken/octoToken", id)
		}
		if r.BotToken != "" {
			if seenToken[r.BotToken] {
				return nil, fmt.Errorf("duplicate botToken across bots")
			}
			seenToken[r.BotToken] = true
		}
		if r.APIURL != "" && !isAllowedURL(r.APIURL) {
			return nil, fmt.Errorf("bot %q: unsafe apiUrl %q (must be https:// or http://localhost; SSRF protection)", id, r.APIURL)
		}
		if r.SDK.AnthropicBaseURL != "" && !isAllowedURL(r.SDK.AnthropicBaseURL) {
			return nil, fmt.Errorf("bot %q: unsafe anthropicBaseUrl %q (SSRF protection)", id, r.SDK.AnthropicBaseURL)
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

func sdkSystemPrompt(s *SDKConfig) string {
	if s == nil {
		return ""
	}
	return s.SystemPrompt
}

func mergeSDK(dst *SDKConfig, src *SDKConfig) {
	if src == nil {
		return
	}
	if src.Model != "" {
		dst.Model = src.Model
	}
	if len(src.AllowedTools) > 0 {
		dst.AllowedTools = src.AllowedTools
	}
	if src.PermissionMode != "" {
		dst.PermissionMode = src.PermissionMode
	}
	if src.MaxTurns != nil {
		dst.MaxTurns = src.MaxTurns
	}
	if len(src.SettingSources) > 0 {
		dst.SettingSources = src.SettingSources
	}
	if src.ToolProgress != nil {
		dst.ToolProgress = src.ToolProgress
	}
	if src.AnthropicBaseURL != "" {
		dst.AnthropicBaseURL = src.AnthropicBaseURL
	}
	if len(src.Env) > 0 {
		dst.Env = src.Env
	}
	if src.Driver != "" {
		dst.Driver = src.Driver
	}
}

func mergeRate(dst *RateLimitConfig, src *RateLimitConfig) {
	if src != nil && src.MaxPerMinute > 0 {
		dst.MaxPerMinute = src.MaxPerMinute
	}
}

func mergeCtx(dst *ContextConfig, src *ContextConfig) {
	if src == nil {
		return
	}
	if src.MaxContextChars > 0 {
		dst.MaxContextChars = src.MaxContextChars
	}
	if src.HistoryLimit > 0 {
		dst.HistoryLimit = src.HistoryLimit
	}
}

// soul reads <botRoot>/SOUL.md (trimmed); "" if absent/empty. Highest-precedence
// systemPrompt source.
func soul(botRoot string) string {
	data, err := os.ReadFile(filepath.Join(botRoot, "SOUL.md"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
