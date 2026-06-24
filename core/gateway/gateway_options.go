package gateway

import (
	"time"

	"github.com/lml2468/octobuddy/core/agent"
	"github.com/lml2468/octobuddy/core/groupctx"
	"github.com/lml2468/octobuddy/core/groupmd"
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

// WithGroupMD enables per-conversation [Group instructions] injection. The
// loader reads operator-authored files from groupConfigDir; passing a nil or
// empty-dir loader leaves injection off.
func (g *Gateway) WithGroupMD(l *groupmd.Loader) *Gateway {
	g.groupMD = l
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

// WithSystemPrompt sets the operator-trusted system prompt (SOUL.md + AGENTS.md).
func (g *Gateway) WithSystemPrompt(p string) *Gateway {
	g.systemPrompt = p
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

// WithToolPolicy sets the per-bot tool surface: def is the bot-level whitelist
// (nil = leave to the driver's probed headless-safe default), channels
// overrides it per sessionKey. A present channel entry or non-nil def is used
// verbatim (empty slice = no tools / muzzle). Unconfigured sessions — including
// the desktop Console — fall through to the driver default.
func (g *Gateway) WithToolPolicy(def []string, channels map[string][]string) *Gateway {
	g.toolDefault = def
	g.toolChannels = channels
	return g
}

// WithSettingSources sets the per-bot claude setting-source scopes passed on
// every turn (empty = driver default "user").
func (g *Gateway) WithSettingSources(ss []string) *Gateway {
	g.settingSources = ss
	return g
}

// resolveTools returns the tool whitelist for sessionKey and whether it was
// explicitly configured. !ok → the caller leaves Request.AllowedTools nil so
// the driver resolves its probed headless-safe default (the global set), which
// is also what unconfigured channels and the Console get.
//
// Mirrors config.ToolPolicy.Resolve; the logic is duplicated rather than
// importing config so the gateway stays dependent on primitives only,
// consistent with WithModel / WithSandbox et al. Keep the two in sync.
func (g *Gateway) resolveTools(sessionKey string) (tools []string, ok bool) {
	if t, has := g.toolChannels[sessionKey]; has {
		return t, true
	}
	if g.toolDefault != nil {
		return g.toolDefault, true
	}
	return nil, false
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
// meaning "unset" without breaking the fluent chain. Default 20 minutes.
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
