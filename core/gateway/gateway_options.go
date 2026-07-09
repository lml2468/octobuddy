package gateway

import (
	"time"

	"github.com/lml2468/octobuddy/core/agent"
	"github.com/lml2468/octobuddy/core/groupctx"
	"github.com/lml2468/octobuddy/core/persona"
	"github.com/lml2468/octobuddy/core/router"
	"github.com/lml2468/octobuddy/core/sandbox"
	"github.com/lml2468/octobuddy/core/store"
)

// New constructs a Gateway.
func New(d agent.Driver, st *store.Store, rt *router.Router, sink Sink) *Gateway {
	return &Gateway{
		driver:          d,
		store:           st,
		router:          rt,
		sink:            sink,
		dispatchTimeout: defaultDispatchTimeout,
	}
}

// WithGroupContext enables group-context injection.
func (g *Gateway) WithGroupContext(gc *groupctx.GroupContext) *Gateway {
	g.groups = gc
	return g
}

// MemberMap exposes the channel's displayName→uid roster snapshot (or nil) for
// outbound @name mention resolution. Nil-safe: returns nil when no group context
// is attached (e.g. DM-only deployments). Keeps the connector pointing at the
// gateway rather than reaching into groupctx directly.
func (g *Gateway) MemberMap(channelID string) map[string]string {
	if g.groups == nil {
		return nil
	}
	return g.groups.MemberMap(channelID)
}

// IsMember reports whether uid is a known member of the channel, used to
// downgrade hallucinated structured mentions. Nil-safe: with no group context
// every uid is treated as valid (the connector then trusts structured uids,
// matching cc-channel-octo's "omit isValidUid" DM path).
func (g *Gateway) IsMember(channelID, uid string) bool {
	if g.groups == nil {
		return true
	}
	return g.groups.IsMember(channelID, uid)
}

// WithGroupBackfill enables cold-start group history backfill (cc G4). fetch
// pulls recent channel messages from the IM REST API; botUID resolves the bot's
// own uid (lazily — it may only be known after IM registration) so its messages
// are not echoed back as context and the initial answered cutoff can be
// inferred. Pass IM-agnostic callbacks — the gateway and groupctx never import a
// connector. No-op unless WithGroupContext is set.
func (g *Gateway) WithGroupBackfill(botUID func() string, fetch func(channelID string, limit int) []groupctx.BackfillMessage) *Gateway {
	g.botUID = botUID
	g.groupBackfill = fetch
	return g
}

// WithOwner sets the resolver for the bot owner's uid (lazy — known only after
// IM registration). Gates owner-only prompt injection (the bootstrap block in
// the owner's DM). A nil resolver or empty result means no IM-owner DM is
// recognized; the Console path is gated separately by Source. IM-agnostic
// callback — the gateway never imports a connector.
func (g *Gateway) WithOwner(owner func() string) *Gateway {
	g.owner = owner
	return g
}

// WithSessionTouchNotifier registers a callback invoked after every successful
// AppendUser / AppendAssistant — the two store writes that mutate a session
// row's projectable state (preview, updatedAt, first-existence). The GUI side
// subscribes here to push a `session.upserted` event, so the sidebar reflects
// brand-new sessions (e.g. a freshly-created thread) immediately without
// waiting for the next sessions.list pull.
//
// The callback receives only coordinates (sessionKey, channelID, channelType)
// — the subscriber owns the projection (preview from store, channelName from
// the IM cache, isThread from the key format).
func (g *Gateway) WithSessionTouchNotifier(fn func(sessionKey, channelID string, channelType router.ChannelType)) *Gateway {
	g.sessionTouch = fn
	return g
}

// notifySessionTouch fires the session-touch notifier, swallowing the nil-fn
// case. Called after every successful AppendUser / AppendAssistant.
func (g *Gateway) notifySessionTouch(sessionKey, channelID string, channelType router.ChannelType) {
	if g.sessionTouch != nil {
		g.sessionTouch(sessionKey, channelID, channelType)
	}
}

// WithSystemPrompt sets a STATIC operator-trusted system prompt (SOUL.md +
// AGENTS.md). Production (config mode) uses WithSystemPromptResolver instead for
// per-turn live reload; this fixed-snapshot setter is for callers that have no
// per-turn source — tests, and any single-shot/REPL embedder. effectiveSystemPrompt
// reads it only when no resolver is installed.
func (g *Gateway) WithSystemPrompt(p string) *Gateway {
	g.systemPrompt = p
	return g
}

// WithSystemPromptResolver installs a per-turn resolver for the operator-trusted
// system prompt (SOUL.md + AGENTS.md). fn is evaluated once per turn so a desktop
// edit to those files applies on the next message WITHOUT a daemon restart —
// mirroring WithToolResolver and the MCP-config / binary resolvers. The daemon
// backs fn with config.SystemPromptFor(botRoot). A nil fn falls back to the
// static WithSystemPrompt snapshot; an empty result is honored (operator cleared
// both files → only the SecurityPrefix remains).
func (g *Gateway) WithSystemPromptResolver(fn func() string) *Gateway {
	g.resolveSystemPromptFn = fn
	return g
}

// effectiveSystemPrompt returns the per-turn system prompt: the resolver's
// result when installed (empty honored), else the static snapshot.
func (g *Gateway) effectiveSystemPrompt() string {
	if g.resolveSystemPromptFn == nil {
		return g.systemPrompt
	}
	return g.resolveSystemPromptFn()
}

// WithBootstrapResolver installs a per-turn resolver for the first-run ritual
// (BOOTSTRAP.md). The daemon backs it with config.BootstrapFor(botRoot). Its
// content is injected only in an owner-trusted channel (Console or the owner's
// DM); see buildSystemPrompt. nil disables bootstrap injection.
func (g *Gateway) WithBootstrapResolver(fn func() string) *Gateway {
	g.resolveBootstrapFn = fn
	return g
}

// WithPersona marks this gateway as a persona clone of the given grantor
// (openclaw OBO). When the grantor is configured, buildSystemPrompt injects an
// operator-trusted persona instruction (the synthesized group hint plus the
// optional free-form personaPrompt) so the LLM replies in the grantor's voice
// instead of returning NO_REPLY on a `@grantor` mention. A zero Grantor (no
// uid) is a no-op (regular bot).
func (g *Gateway) WithPersona(grantor persona.Grantor, personaPrompt string) *Gateway {
	g.persona = grantor
	g.personaPrompt = personaPrompt
	return g
}

// WithModel sets the model override passed to the driver on each turn.
func (g *Gateway) WithModel(m string) *Gateway {
	g.model = m
	return g
}

// WithToolResolver installs the per-turn tool-surface resolver. fn is called
// once per turn with the session key and returns (tools, ok): ok=false leaves
// Request.AllowedTools nil (the driver's probed headless-safe default — also
// what unconfigured channels and the desktop Console get); ok=true uses the
// returned list verbatim (empty slice = muzzle). A nil fn disables the policy.
//
// The daemon backs fn with a per-turn config.json re-read (config.ToolPolicyFor
// → config.ToolPolicy.Resolve), so a desktop edit to a conversation's tools
// applies on the next turn WITHOUT a daemon restart — mirroring the MCP-config
// and binary resolvers. Resolution lives in config.ToolPolicy.Resolve (one
// implementation), not duplicated here.
func (g *Gateway) WithToolResolver(fn func(sessionKey string) (tools []string, ok bool)) *Gateway {
	g.resolveToolsFn = fn
	return g
}

// resolveTools delegates to the installed per-turn resolver. !ok (or no
// resolver) → the caller leaves Request.AllowedTools nil so the driver resolves
// its probed headless-safe default.
func (g *Gateway) resolveTools(sessionKey string) (tools []string, ok bool) {
	if g.resolveToolsFn == nil {
		return nil, false
	}
	return g.resolveToolsFn(sessionKey)
}

// WithSettingSources sets the per-bot claude setting-source scopes passed on
// every turn (empty = driver default "user").
func (g *Gateway) WithSettingSources(ss []string) *Gateway {
	g.settingSources = ss
	return g
}

// WithSandbox enables per-session filesystem isolation: each turn runs in a
// hashed subdir of cwdBase, with auto-memory consolidated under memoryBase. An
// empty cwdBase disables isolation. Skills / workflows are NOT plumbed here —
// they live under the bot's CLAUDE_CONFIG_DIR and are auto-loaded by the CLI.
func (g *Gateway) WithSandbox(cwdBase, memoryBase string) *Gateway {
	g.cwdBase = cwdBase
	g.memoryBase = memoryBase
	return g
}

// WithMediaAuth sets the hook that scopes the IM credential per inbound-media
// download URL (see MediaAuth). Without it, downloads carry no Authorization
// header — fine for public CDN media, but same-host authenticated media won't
// fetch.
func (g *Gateway) WithMediaAuth(fn MediaAuth) *Gateway {
	g.mediaAuth = fn
	return g
}

// WithDispatchTimeout overrides the per-turn idle timeout (#141). The timer
// resets on every AgentEvent, so a long turn with steady event flow is fine —
// only N seconds of silence kills it. A value <=0 is a no-op (keeps the
// current default), so a caller can blindly pass a config value of zero
// meaning "unset" without breaking the fluent chain. Default 30 minutes.
func (g *Gateway) WithDispatchTimeout(d time.Duration) *Gateway {
	if d <= 0 {
		return g
	}
	g.dispatchTimeout = d
	return g
}

// WithCommandInfo records the effective, non-secret settings surfaced by the
// /config slash command (rate limit and context-char budget). The model comes
// from WithModel.
func (g *Gateway) WithCommandInfo(maxPerMinute, contextChars int) *Gateway {
	g.maxPerMinute = maxPerMinute
	g.contextChars = contextChars
	return g
}

// kindFor maps a channel type to a sandbox kind. Group → group (shared); DM and
// any unknown type → dm (the most-isolated, per-key default — never collapse
// distinct sessions into a shared sandbox).
func kindFor(ct router.ChannelType) sandbox.Kind {
	if ct == router.ChannelGroup {
		return sandbox.KindGroup
	}
	return sandbox.KindDM
}

// ReapGroupContext evicts group-context channel windows untouched for at
// least threshold. No-op when group-context is disabled. Returns the
// channels evicted (0 if disabled). Wired into the daemon's periodic
// reaper alongside router.Reap so a long-quiet group doesn't accumulate
// memory over the daemon's lifetime.
func (g *Gateway) ReapGroupContext(threshold time.Duration) int {
	if g.groups == nil {
		return 0
	}
	return g.groups.ReapIdle(threshold)
}
