// Package config implements OctoBuddy's single-file configuration.
//
// One ~/.octobuddy/config.json holds shared runtime policy (rateLimit/context) plus
// a bots[] list where each entry inlines a bot's identity and agent config.
// Agent/env/apiUrl settings are intentionally per-bot, never inherited. The per-bot data directory
// (~/.octobuddy/<id>/data) is DERIVED from baseDir + id, never configurable — so a
// bot can't escape its own subtree. The bot's persona/behavior prompt lives in
// SOUL.md + AGENTS.md in ~/.octobuddy/<id>/, not in config.
package config

import (
	"bytes"
	"encoding/json"
)

// EnvValue is one declared agent environment variable. Plain values live in
// config.json for reviewability; secrets live in the configured secret backend
// and are referenced here by key (for example "env/GH_TOKEN").
type EnvValue struct {
	Value     string `json:"value,omitempty"`
	SecretRef string `json:"secretRef,omitempty"`
}

// UnmarshalJSON also accepts the pre-#96 legacy shape, where each env entry
// was a bare string ("OCTO_BOT_ID": "foo_bot") rather than a {value,secretRef}
// object. Without this, every operator whose config predates the refactor
// crashes the daemon on first launch of the new build. The encoder is
// unchanged, so the next configstore write silently migrates the file to the
// new shape.
func (v *EnvValue) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		return json.Unmarshal(trimmed, &v.Value)
	}
	type raw EnvValue
	var r raw
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	*v = EnvValue(r)
	return nil
}

// AgentConfig is the on-disk "agent" block: the model and the model-gateway
// routing (base URL + token) plus any extra env vars injected into the agent CLI.
type AgentConfig struct {
	Model          string              `json:"model,omitempty"`
	GatewayBaseURL string              `json:"gatewayBaseUrl,omitempty"`
	GatewayToken   string              `json:"gatewayToken,omitempty"`
	Env            map[string]EnvValue `json:"env,omitempty"`
	// Cron enables the per-bot scheduled-task scheduler. Off by default;
	// when true the bot loads <dataDir>/cron.json at startup and fires due tasks
	// through the gateway. Owner-gated create/delete is exposed over the control
	// bus (cron.create / cron.list / cron.delete).
	//
	// *bool (not bool) so config can distinguish "unset" from an explicit false
	// if future policy needs it. In today's canonical schema cron is per-bot.
	Cron *bool `json:"cron,omitempty"`
	// ToolProgress, when true, makes the IM connector mirror each tool the agent
	// invokes back to the channel as a brief "🔧 Running <tool>(<params>)…" notice
	// (consecutive dups collapsed, capped per turn). Off by default — opt-in.
	// Ported from cc-channel-octo `sdk.toolProgress` (src/config.ts, src/index.ts).
	ToolProgress bool `json:"toolProgress,omitempty"`
	// InheritUserConfig, when true, lets the agent inherit the operator's
	// ~/.claude (user-scope skills + installed plugins). OFF by default: each bot
	// gets an isolated CLAUDE_CONFIG_DIR so operator plugins/user-skills don't
	// leak into every bot. Set true only for a trusted single-operator deployment
	// that deliberately shares its ~/.claude with the bots.
	InheritUserConfig bool `json:"inheritUserConfig,omitempty"`
	// DispatchTimeoutSec overrides the per-turn IDLE timeout (seconds) for this
	// bot. The timer resets on every AgentEvent, so a long workflow with steady
	// event flow is fine — only N seconds of silence kills the turn. <=0 leaves
	// the daemon default (20 min). Set higher when a bot routinely runs long
	// tools that can stay silent for >20 min (e.g. a slow Bash); set lower for
	// snappy DMs where a stuck turn should surface fast.
	DispatchTimeoutSec int `json:"dispatchTimeoutSec,omitempty"`
	// SystemPromptMode picks the ClaudeDriver prompt shape:
	//   - "minimal" (default): SOUL.md+AGENTS.md REPLACE the built-in
	//     prompt; cwd .claude/ is not auto-loaded; tool surface is the
	//     headless whitelist.
	//   - "claude_code": SOUL.md+AGENTS.md is APPENDED to the built-in
	//     prompt; cwd .claude/ auto-loads; every tool runs under
	//     bypassPermissions. Escape hatch for bots whose SOUL was
	//     authored assuming the built-in preamble.
	// Empty / invalid → minimal (warn on invalid).
	SystemPromptMode string `json:"systemPromptMode,omitempty"`
	// Tools scopes the tool surface the agent may call, per bot and
	// optionally per channel/DM. Nil = the binary's probed headless-safe
	// set (the driver default). See ToolPolicy.
	Tools *ToolPolicy `json:"tools,omitempty"`
	// SettingSources selects which claude filesystem setting scopes load
	// (minimal mode only). Allowed values: "user", "project". Empty →
	// ["user"] (the default: keeps CLAUDE_CONFIG_DIR-based per-bot skills,
	// drops project/local so a planted CLAUDE.md in the sandbox cwd can't
	// influence the model). Adding "project" re-enables cwd .claude/ +
	// CLAUDE.md — a prompt-injection vector for untrusted group chats, so
	// only for trusted single-operator bots. "local" is intentionally not
	// allowed.
	SettingSources []string `json:"settingSources,omitempty"`
}

// ToolPolicy is the per-bot tool surface.
//   - Default is the bot-level whitelist. nil → unset (driver resolves the
//     binary's probed headless-safe set); a non-nil (incl. empty) slice is
//     used verbatim (empty = no tools).
//   - Channels overrides Default per sessionKey (group = channelId; DM =
//     "<spaceId>:<uid>" or bare uid — the same key the router derives). A
//     present entry is used verbatim (incl. empty = muzzle that channel).
//
// Unconfigured channels (and the desktop Console) fall through to the global
// headless-safe set.
type ToolPolicy struct {
	Default  []string            `json:"default,omitempty"`
	Channels map[string][]string `json:"channels,omitempty"`
}

// Resolve returns the tool whitelist for sessionKey and whether it was
// explicitly configured. ok=false means the caller should leave
// Request.AllowedTools nil so the driver resolves the probed headless-safe
// default. A present (even empty) channel entry or non-nil Default is
// "explicit" and returned verbatim.
func (p *ToolPolicy) Resolve(sessionKey string) (tools []string, ok bool) {
	if p == nil {
		return nil, false
	}
	if t, has := p.Channels[sessionKey]; has {
		return t, true
	}
	if p.Default != nil {
		return p.Default, true
	}
	return nil, false
}

func (p *ToolPolicy) clone() *ToolPolicy {
	if p == nil {
		return nil
	}
	cp := &ToolPolicy{Default: cloneStrs(p.Default)}
	if p.Channels != nil {
		cp.Channels = make(map[string][]string, len(p.Channels))
		for k, v := range p.Channels {
			cp.Channels[k] = cloneStrs(v)
		}
	}
	return cp
}

// RateLimitConfig mirrors the on-disk rateLimit block.
type RateLimitConfig struct {
	MaxPerMinute int `json:"maxPerMinute,omitempty"`
}

// ContextConfig mirrors the on-disk context block.
type ContextConfig struct {
	MaxContextChars int `json:"maxContextChars,omitempty"`
}

// TriggerConfig governs the trigger pipeline — per-bot policy for
// "should this message trigger a reply?". Defaults flip the @AI
// broadcast path closed; reply-to-bot recovers the natural
// "continue the thread" UX.
type TriggerConfig struct {
	// AIBroadcast: "deny" (default — safest), "allowlist" (only
	// channels listed in AIBroadcastAllowlist), or "allow" (legacy).
	// Unset → daemon defaults to deny with a stderr warning.
	AIBroadcast string `json:"aiBroadcast,omitempty"`
	// AIBroadcastAllowlist scopes aiBroadcast="allowlist".
	AIBroadcastAllowlist []string `json:"aiBroadcastAllowlist,omitempty"`
	// ReplyToBotEnabled lifts a quote-reply to one of the bot's own
	// messages into a trigger. Pointer so unset defaults to true; set
	// to false to disable.
	ReplyToBotEnabled *bool `json:"replyToBotEnabled,omitempty"`
	// MentionFreeGroups: channel ids where the bot answers without
	// needing an @mention.
	MentionFreeGroups []string `json:"mentionFreeGroups,omitempty"`
}

// OnBehalfOf marks a bot as a persona clone: it speaks for a grantor (a human
// identity), replying in the grantor's voice when the grantor or the group is
// @-mentioned. Ported from openclaw-channel-octo (config-schema.ts
// `account.config.onBehalfOf`); cc-channel-octo has no persona clones.
type OnBehalfOf struct {
	// UID is the grantor's server-authoritative uid. It is the security anchor:
	// only OBO v2 fields signed by this uid are trusted (see im/octo OBO relay).
	UID string `json:"uid,omitempty"`
	// Name is the grantor's display name woven into the persona instruction;
	// falls back to UID when empty.
	Name string `json:"name,omitempty"`
	// PersonaPrompt is an optional free-form instruction (e.g. "always reply in
	// English") appended to the persona system prompt. Operator-trusted.
	PersonaPrompt string `json:"personaPrompt,omitempty"`
}

// BotEntry is one bot's full inline configuration in the global config's bots[]
// list. octoToken is OPTIONAL — it may be injected at runtime (secret.inject)
// instead of stored here. apiUrl/agent/group/gating settings are per-bot; only
// rateLimit/context have top-level runtime-policy defaults. The bot's
// persona/behavior prompt is NOT here — it lives in SOUL.md + AGENTS.md under
// ~/.octobuddy/<id>/.
type BotEntry struct {
	ID        string           `json:"id,omitempty"`
	OctoToken string           `json:"octoToken,omitempty"`
	APIURL    string           `json:"apiUrl,omitempty"`
	Agent     *AgentConfig     `json:"agent,omitempty"`
	RateLimit *RateLimitConfig `json:"rateLimit,omitempty"`
	Context   *ContextConfig   `json:"context,omitempty"`
	// GroupConfigDir is an operator-controlled directory holding per-conversation
	// instruction files (<channelId>.md), injected as a trusted [Group instructions]
	// block. MUST be outside CwdBase — see Resolved.
	GroupConfigDir string `json:"groupConfigDir,omitempty"`
	// OnBehalfOf, when its uid is set, marks this bot a persona clone (openclaw OBO).
	OnBehalfOf *OnBehalfOf `json:"onBehalfOf,omitempty"`

	// Gating policy: DM blocklist + bot-loop guard. Per-bot only in the
	// canonical schema. The mention-free list lives under
	// trigger.mentionFreeGroups.
	KnownBotUids   []string `json:"knownBotUids,omitempty"`
	AllowedBotUids []string `json:"allowedBotUids,omitempty"`
	BotBlocklist   []string `json:"botBlocklist,omitempty"`

	// Trigger is the per-bot trigger pipeline policy. Defaults applied
	// at Load (aiBroadcast=deny, replyToBotEnabled=true).
	Trigger *TriggerConfig `json:"trigger,omitempty"`
}

// File is the on-disk shape of the single ~/.octobuddy/config.json. Top-level
// rateLimit/context are shared runtime policy; bot identity, agent, env, group
// config, and gating settings live only on each bots[] entry.
type File struct {
	RateLimit *RateLimitConfig `json:"rateLimit,omitempty"`
	Context   *ContextConfig   `json:"context,omitempty"`

	Bots []BotEntry `json:"bots,omitempty"`
}

// Resolved is a single bot's fully-resolved, ready-to-run configuration.
type Resolved struct {
	BotID     string
	APIURL    string
	OctoToken string

	Agent     AgentConfig
	RateLimit RateLimitConfig
	Context   ContextConfig

	// Gating policy. Mention-free groups live under
	// trigger.MentionFreeGroups.
	KnownBotUids   []string // uids known to be bots, for the loop guard
	AllowedBotUids []string // bot-looking uids exempt from the loop guard
	BotBlocklist   []string // uids whose DMs are silently dropped

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

	// OnBehalfOf, when its UID is set, marks this bot as a persona clone of the
	// named grantor (openclaw OBO). nil/empty UID = a regular bot.
	OnBehalfOf OnBehalfOf

	// Trigger holds the trigger-pipeline policy. Unset fields fall back
	// to safe defaults (aiBroadcast=deny, replyToBotEnabled=true).
	Trigger TriggerConfig

	// Derived per-bot directories (never from file).
	DataDir    string // ~/.octobuddy/<id>/data       — SQLite + state
	CwdBase    string // ~/.octobuddy/<id>/workspace   — per-session cwd sandboxes
	MemoryBase string // ~/.octobuddy/<id>/memory      — per-session auto-memory (outside CwdBase)
	// ClaudeConfigDir is the per-bot CLAUDE_CONFIG_DIR (~/.octobuddy/<id>/.claude).
	// Set as the agent's config root to ISOLATE it from the operator's ~/.claude
	// (user-scope skills + installed plugins would otherwise leak into every
	// bot). Empty when agent.inheritUserConfig is set. The bot's own skills /
	// workflows live under it (.claude/skills,.claude/workflows) and are
	// auto-discovered by the claude CLI as user-scope assets — no per-turn
	// sandbox symlinking needed.
	ClaudeConfigDir string
}
