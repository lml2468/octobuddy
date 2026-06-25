package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/lml2468/octobuddy/core/safepath"
)

// ToolPolicyFor re-reads the config at path and returns botID's tool policy
// (agent.tools). It is the per-turn live-read backing the gateway's tool
// resolver: a desktop edit to tools.channels (configstore.SetChannelTools writes
// config.json directly) applies on the next turn without a daemon restart — the
// same per-Query philosophy as the MCP-config / binary resolvers. Cheap (one
// safepath read + parse) relative to a turn's claude spawn.
//
// The `ok` return distinguishes the two nil-policy cases the caller must treat
// DIFFERENTLY:
//   - ok=true: the read succeeded and the bot has no tools policy (or none at
//     all). The caller should use the driver default — NOT fall back to a
//     startup snapshot — so that clearing a policy at runtime actually takes
//     effect live.
//   - ok=false: the read/parse FAILED (file briefly unreadable mid-write, or an
//     unknown bot). The caller should keep its last-known/startup policy so a
//     transient error never silently widens the surface.
//
// Invalid tool names are dropped defensively (sanitizeToolPolicy): the desktop
// write paths offer only probed names, but a hand-edit could plant a name with a
// comma that would split the --tools flag, so every name is re-checked against
// toolNameValid here on read rather than trusted.
func ToolPolicyFor(path, botID string) (policy *ToolPolicy, ok bool) {
	if path == "" {
		path = DefaultConfigPath()
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, false
	}
	f, err := readFile(abs)
	if err != nil {
		return nil, false
	}
	for _, b := range f.Bots {
		if b.ID != botID {
			continue
		}
		if b.Agent == nil || b.Agent.Tools == nil {
			return nil, true // bot exists, no policy → driver default (live)
		}
		return sanitizeToolPolicy(b.Agent.Tools), true
	}
	// Bot not found in the file: treat as a read miss (ok=false) so the caller
	// keeps its startup policy rather than silently dropping to the default for a
	// bot that should exist.
	return nil, false
}

// sanitizeToolPolicy returns a deep copy of p with any tool name failing
// toolNameValid dropped, so a live (unvalidated) config edit can't inject a
// comma that splits --tools. A channel override that sanitizes away to nothing
// (all names invalid) is REMOVED from the map so it falls through to the bot
// default rather than registering as a present-but-empty muzzle.
func sanitizeToolPolicy(p *ToolPolicy) *ToolPolicy {
	cp := p.clone()
	cp.Default = filterValidToolNames(cp.Default)
	for k, v := range cp.Channels {
		sv := filterValidToolNames(v)
		if sv == nil && v != nil {
			// The override was non-nil but all-invalid → drop it (fall through to
			// default), don't leave a (nil,true) entry that Resolve would treat as
			// a configured override. An explicit empty muzzle ([]) is preserved by
			// filterValidToolNames and kept here.
			delete(cp.Channels, k)
			continue
		}
		cp.Channels[k] = sv
	}
	return cp
}

// filterValidToolNames drops malformed names from a tool list. It preserves the
// nil vs empty-slice distinction at the boundaries (nil → nil = unset/driver
// default; explicit empty → empty = muzzle), with one deliberate exception: a
// NON-EMPTY input whose names are ALL invalid returns nil (driver default), not
// empty (muzzle). A fully-malformed hand-edit (e.g. ["Read,Bash"] — one
// comma-bearing entry) expressed intent to allow tools, so failing open to the
// default better matches that intent than silently muzzling the channel. A
// partially-valid list keeps its valid subset.
func filterValidToolNames(names []string) []string {
	if names == nil {
		return nil
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		// Mirror validateToolNames via the shared predicate: drop malformed names
		// and all-wildcard tokens a hand-edit could plant; prefixed globs like
		// "mcp__*" survive.
		if toolNameValid(n) {
			out = append(out, n)
		}
	}
	if len(out) == 0 && len(names) > 0 {
		return nil // every name was invalid → driver default, not a silent muzzle
	}
	return out
}

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
	r.BotRoot = botRoot
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

	// System prompt: SOUL.md (identity) + AGENTS.md (behavior), file-based. This
	// is the startup snapshot / fallback; the gateway re-reads it per turn via
	// SystemPromptFor(r.BotRoot) so desktop edits apply without a restart.
	r.SystemPrompt = SystemPromptFor(botRoot)

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

// promptModeClaudeCode is the on-disk SystemPromptMode value that selects
// claude_code mode (mirrors agent.PromptModeClaudeCode). Kept as a literal here
// because config must not import agent (dependency direction); the string is the
// wire contract between the two.
const promptModeClaudeCode = "claude_code"

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
	// "user" must be present whenever sources are configured AND the bot uses
	// minimal mode: dropping it there disables CLAUDE_CONFIG_DIR-based per-bot
	// skill discovery (a project-only scope), which is never intended. In
	// claude_code mode the driver ignores settingSources entirely (it uses the
	// built-in scopes), so a leftover project-only value is inert — rejecting it
	// would fail a config that works. Empty is always fine (defaults to ["user"]).
	if a.SystemPromptMode != promptModeClaudeCode &&
		len(a.SettingSources) > 0 && !slices.Contains(a.SettingSources, "user") {
		return fmt.Errorf("bot %q: settingSources must include \"user\" (project-only drops per-bot skill discovery)", botID)
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
		if isAllWildcard(n) {
			return fmt.Errorf("bot %q: tool name %q in %s is an all-wildcard glob (would grant every tool); list explicit names or \"mcp__*\"", botID, n, where)
		}
	}
	return nil
}

// toolNameValid is the single source of truth for "is this a usable tool name":
// it matches toolNameRE AND is not an all-wildcard glob. Both the load-time
// validator (validateToolNames, error-returning) and the per-turn sanitizer
// (filterValidToolNames, dropping) consult it so the two never drift.
func toolNameValid(n string) bool {
	return toolNameRE.MatchString(n) && !isAllWildcard(n)
}

// isAllWildcard reports whether n is composed only of "*" (e.g. "*", "**"). Such
// a token expands to claude's full tool surface, silently inverting a
// muzzle/whitelist into all-tools and re-admitting interactive tools the
// headless path excludes. Prefixed globs ("mcp__*") are fine — only an
// all-wildcard token is rejected.
func isAllWildcard(n string) bool {
	return n != "" && strings.Trim(n, "*") == ""
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

// systemPromptFiles is the ordered registry of operator-authored context files
// that compose a bot's system prompt, each with a one-line descriptor telling
// the model how to weight it (ported from openclaw's CONTEXT_FILE_ORDER +
// per-file framing in system-prompt.ts buildProjectContextSection). Adding a
// future file (e.g. USER.md) is one entry here — SystemPromptFor needs no other
// change. Order is significant: identity (SOUL) before behavior (AGENTS).
var systemPromptFiles = []struct{ Name, Descriptor string }{
	{"SOUL.md", "SOUL.md: identity, voice, boundaries. Follow it unless higher-priority instructions override."},
	{"AGENTS.md", "AGENTS.md: behavior norms and red lines. Follow it unless higher-priority instructions override."},
}

// SystemPromptFor assembles the bot's operator-trusted system prompt from the
// files in systemPromptFiles, each wrapped as a labeled Markdown section:
//
//	## SOUL.md
//	<descriptor>
//
//	<file body>
//
// Sections are joined with a blank line; an absent/empty file emits nothing (no
// orphan heading). Returns "" when no file has content.
//
// The `## NAME` labels are deliberately Markdown headings, NOT bracketed
// [section] markers: the privileged-marker namespace (safety.sectionMarkerRE) is
// reserved for the gateway's own structural anchors that untrusted text is
// escaped against. These files are operator-trusted (wrapped as
// safety.TrustedText by the gateway, never escaped), and `##` carries no
// structural authority — the security model keys on the current-message anchor +
// SecurityPrefix — so untrusted text reproducing `## SOUL.md` forges nothing.
//
// Routes through safepath.SafeRead so an agent (Bash + bypass) that plants
// `~/.octobuddy/<id>/SOUL.md → /Users/victim/.aws/credentials` cannot redirect
// the trusted-prompt source; the symlink target would otherwise be injected
// verbatim as TrustedText, leaking the file on the next reply.
//
// Re-read per turn by the gateway's system-prompt resolver, so a desktop edit to
// SOUL.md/AGENTS.md — or the BOT'S OWN edit to them — applies on the next message
// without a daemon restart. This is intentional: it gives each bot openclaw-style
// self-bootstrapping (it can refine its SOUL/AGENTS over time and have that take
// effect). Note SOUL.md/AGENTS.md live in botRoot, which an agent holding Bash
// can write (cwd is a starting dir, not a chroot), so a bot can rewrite its own
// trusted prompt. That is self-modification, not privilege escalation — an agent
// with Bash already has code execution as the user. The defense against
// INJECTION-driven self-modification in untrusted contexts is per-channel tool
// scoping (WithToolResolver): muzzle Bash/Write in untrusted group channels and
// keep them only for trusted operator DMs.
//
// botRoot must be non-empty: an empty root would make safepath.SafeRead resolve
// relative to the process cwd and read an unrelated SOUL.md/AGENTS.md, so it is
// rejected here (returns "").
func SystemPromptFor(botRoot string) string {
	if botRoot == "" {
		return ""
	}
	var parts []string
	for _, f := range systemPromptFiles {
		body := readBotRootFile(botRoot, f.Name)
		if body == "" {
			continue
		}
		parts = append(parts, "## "+f.Name+"\n"+f.Descriptor+"\n\n"+body)
	}
	return strings.Join(parts, "\n\n")
}

// readBotRootFile reads one operator-trusted file directly under botRoot and
// returns its trimmed body, or "" when botRoot is empty / the file is
// absent/unreadable/empty. Single source of the per-turn read discipline shared
// by SystemPromptFor and BootstrapFor: routes through safepath.SafeRead
// (symlink-refusing, 1 MiB cap) so a planted symlink can't redirect the injected
// trusted text, and rejects an empty botRoot (which would make SafeRead resolve
// relative to the process cwd).
func readBotRootFile(botRoot, name string) string {
	if botRoot == "" {
		return ""
	}
	data, err := safepath.SafeRead(botRoot, name, 1<<20) // 1 MiB cap; errors on oversize
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// BootstrapName is the first-run ritual file: a brand-new bot is scaffolded with
// it (desktop configstore), and while it exists the gateway injects it — ONLY in
// the owner's DM / Console — so the bot interviews the owner, writes its SOUL.md,
// then deletes this file. Once deleted, per-turn reload stops injecting it.
const BootstrapName = "BOOTSTRAP.md"

// BootstrapFor returns BOOTSTRAP.md's body for botRoot, or "" when absent/empty.
// Shares readBotRootFile's safepath discipline (symlink-refusing, 1 MiB cap,
// empty-root rejection). Re-read per turn by the gateway's bootstrap resolver so
// the bot's own deletion takes effect on the next message.
func BootstrapFor(botRoot string) string {
	return readBotRootFile(botRoot, BootstrapName)
}
