// Package gateway orchestrates the end-to-end turn pipeline, the Go analogue of
// cc-channel's index.ts handleMessage:
//
//	inbound → router (lock + gate + rate limit) → getOrCreate session →
//	load resume id → driver.Query → stream events → deliver to sink →
//	persist assistant reply + resume id
//
// It depends only on the agent.Driver abstraction and a Sink, so it is unaware
// of which agent runs underneath or which IM (if any) is on the other end.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/lml2468/xclaw/core/agent"
	"github.com/lml2468/xclaw/core/groupctx"
	"github.com/lml2468/xclaw/core/groupmd"
	"github.com/lml2468/xclaw/core/persona"
	"github.com/lml2468/xclaw/core/router"
	"github.com/lml2468/xclaw/core/safety"
	"github.com/lml2468/xclaw/core/sandbox"
	"github.com/lml2468/xclaw/core/store"
)

// Sink receives normalized agent events for one turn, plus a final assembled
// reply. Implementations deliver to an IM, stdout, the control bus, etc.
type Sink interface {
	// OnEvent is called for each streamed AgentEvent (text/tool/etc.).
	OnEvent(sessionKey string, ev agent.AgentEvent)
	// OnReply is called once with the full assembled assistant text (may be "").
	OnReply(sessionKey string, text string)
}

// Gateway wires the router, store, and an agent driver together.
type Gateway struct {
	driver agent.Driver
	store  *store.Store
	router *router.Router
	sink   Sink

	// Optional group-context (set via WithGroupContext). When set, group
	// messages get a [Recent group messages] delta injected into the prompt.
	groups *groupctx.GroupContext
	// Optional per-conversation instruction loader (set via WithGroupMD). When
	// set, group/thread turns get an operator-authored [Group instructions] block
	// (from groupConfigDir/<channelId>.md) appended to the system prompt.
	groupMD *groupmd.Loader
	// Optional cold-start backfill fetch (set via WithGroupBackfill). Returns
	// recent channel messages from the IM REST API to seed an empty group window
	// the first time a channel is seen (cc G4). Kept as an IM-agnostic callback so
	// groupctx never imports a connector. botUID lets backfill skip the bot's own
	// replies and infer the initial answered/new cutoff.
	groupBackfill func(channelID string, limit int) []groupctx.BackfillMessage
	botUID        func() string
	// Operator-trusted system prompt (assembled from SOUL.md + AGENTS.md).
	// Appended after the non-overridable security prefix.
	systemPrompt string
	// Persona-clone grantor (openclaw OBO). When configured, a persona
	// instruction is injected into the system prompt so the bot replies in the
	// grantor's voice. Zero value = a regular (non-clone) bot. personaPrompt is
	// the optional free-form persona instruction appended after the synthesized
	// group hint.
	persona       persona.Grantor
	personaPrompt string
	// Optional model override passed to the driver (empty = driver default).
	model string
	// Per-session sandbox roots (set via WithSandbox). When cwdBase is set, each
	// turn runs in cwdBase/<hash>, with auto-memory under memoryBase/<hash> and
	// the bot's per-bot skills/workflows symlinked in. Empty cwdBase = no
	// isolation (inherit proc).
	//
	// skillsDir/workflowsDir are the bot's OWN dirs (~/.xclaw/<id>/skills|workflows).
	// They are the single link source into the sandbox: a marketplace skill reaches
	// the sandbox only via a symlink the operator "installed" into the per-bot dir
	// (~/.xclaw/<id>/skills/<name> → ~/.xclaw/skills/<name>), never linked globally.
	cwdBase, memoryBase, skillsDir, workflowsDir string
	// mediaAuth, when set, supplies the Authorization header for an inbound-media
	// download URL (scoped to the IM's apiUrl host). Set via WithMediaAuth by the
	// IM connector; keeps the gateway IM-agnostic (it never embeds a token).
	mediaAuth MediaAuth
	// assertPublic overrides the media-download SSRF guard (defaults to
	// config.AssertPublicURL). Test seam only — production never sets it.
	assertPublic func(ctx context.Context, rawURL string) error
	// mediaClient overrides the media-download HTTP client (defaults to the
	// hardened mediaHTTPClient with the rebinding-proof dial guard). Test seam
	// only — production never sets it; tests inject a client that permits the
	// loopback httptest server the dial guard would otherwise reject.
	mediaClient *http.Client

	// dispatchTimeout bounds driver.Query + the stream loop for a single turn
	// (#141). On expiry the turn's context is cancelled (which kills the claude
	// subprocess via CommandContext) and the user gets a "处理超时" apology. The
	// session lock then releases as runTurn returns, so a hung turn cannot wedge
	// the session queue forever. <=0 disables the bound. Defaults to 5 min.
	dispatchTimeout time.Duration

	// Effective settings surfaced by /config (no secrets). Set via WithCommandInfo.
	maxPerMinute int
	contextChars int
}

// defaultDispatchTimeout bounds a single turn (#141, config.ts dispatchTimeoutMs).
const defaultDispatchTimeout = 5 * time.Minute

// New constructs a Gateway.
func New(d agent.Driver, st *store.Store, rt *router.Router, sink Sink) *Gateway {
	return &Gateway{driver: d, store: st, router: rt, sink: sink, dispatchTimeout: defaultDispatchTimeout}
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

// WithSandbox enables per-session filesystem isolation: each turn runs in a
// hashed subdir of cwdBase, with auto-memory consolidated under memoryBase and
// the bot's per-bot skills (skillsDir, ~/.xclaw/<id>/skills) symlinked into the
// sandbox. An empty cwdBase disables isolation.
func (g *Gateway) WithSandbox(cwdBase, memoryBase, skillsDir string) *Gateway {
	g.cwdBase = cwdBase
	g.memoryBase = memoryBase
	g.skillsDir = skillsDir
	return g
}

// WithWorkflows configures the bot's per-bot workflow dir (~/.xclaw/<id>/workflows),
// the single source linked into the sandbox's .claude/workflows.
func (g *Gateway) WithWorkflows(workflowsDir string) *Gateway {
	g.workflowsDir = workflowsDir
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

// WithDispatchTimeout overrides the per-turn dispatch timeout (#141). A value
// <=0 disables the bound (the turn runs unguarded). Default is 5 minutes.
func (g *Gateway) WithDispatchTimeout(d time.Duration) *Gateway {
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

// Handle routes one inbound message through the full pipeline. The router holds
// the per-session lock across the whole turn, so same-session turns serialize.
// Returns the router decision (so callers can log drops/limits).
//
// Friendly drop replies (ported from cc-channel session-router.ts) are emitted
// here, through the Sink, so every caller benefits without re-implementing them:
//   - DroppedTooLong → "消息过长，请缩短后重试"
//   - RateLimited    → "请稍后再试" (deduped per rate-limit window; see router)
//
// DroppedNotMentioned / DroppedUnroutable stay silent (legitimate group chatter
// or an unroutable payload — no user-facing reply).
func (g *Gateway) Handle(ctx context.Context, msg router.InboundMessage) (router.Decision, error) {
	d, err := g.router.RouteAndHandle(ctx, msg, g.runTurn)

	// Surface the drop reply through the sink. SessionKey is derivable for both
	// routable-drop cases (TooLong/RateLimited passed the routability gate).
	switch d {
	case router.DroppedTooLong:
		if key, kerr := msg.SessionKey(); kerr == nil {
			g.sink.OnReply(key, oversizedReply)
		}
	case router.RateLimited:
		// First rejection of this rate-limit window — notify once. The router
		// decided this atomically with the rejection (deduping subsequent
		// rejections in the same window to RateLimitedSilent), so a flooder doesn't
		// get a "请稍后再试" for every dropped message.
		if key, kerr := msg.SessionKey(); kerr == nil {
			g.sink.OnReply(key, rateLimitedReply)
		}
	}
	return d, err
}

// Friendly drop-reply strings, ported verbatim from cc-channel's
// session-router.ts (processMessage rate-limit / oversize branches).
const (
	oversizedReply   = "消息过长，请缩短后重试"
	rateLimitedReply = "请稍后再试"
	// timeoutReply is sent when a turn exceeds the dispatch timeout (#141).
	timeoutReply = "⚠️ 处理超时，请稍后重试。"
	// errorReply is sent when a turn ends in a terminal agent error (e.g.
	// max_turns) or the agent process fails. Like the timeout path, the partial
	// reply is not persisted and the resume id is not advanced.
	errorReply = "⚠️ 出错了，请稍后重试。"
	// busyReply is sent when a turn fails on an upstream rate-limit / overload /
	// usage-cap condition (KindError with Transient set). Distinct from the
	// generic errorReply so the user knows it's a capacity issue, not a bug.
	busyReply = "⏳ 服务繁忙，请稍后重试。"
)

// errDispatchTimeout is the cause attached to the per-turn dispatch deadline, so
// runTurn can distinguish its own timeout from a caller cancellation via
// context.Cause (M9).
var errDispatchTimeout = errors.New("dispatch timeout")

// Observe caches a non-triggering group message into the group context so it
// becomes background for a later @-mention turn. Call this for group messages
// that did NOT mention the bot (which Handle would drop). No-op outside groups
// or when group-context is disabled.
func (g *Gateway) Observe(msg router.InboundMessage) {
	if g.groups == nil || msg.ChannelType != router.ChannelGroup || msg.ChannelID == "" {
		return
	}
	if strings.TrimSpace(msg.Text) == "" {
		return
	}
	g.groups.Push(msg.ChannelID, msg.FromUID, msg.FromName, msg.Text, msg.MessageSeq)
}

// failTurn logs an internal turn failure and sends the user a generic apology so
// no error path is silently swallowed (which would also leave the typing
// indicator running until it times out). It returns nil because the user has been
// signalled and the session lock should release cleanly; the error is for logs.
func (g *Gateway) failTurn(sessionKey, stage string, err error) error {
	fmt.Fprintf(os.Stderr, "[gateway] turn failed at %s (session=%s): %v\n", stage, sessionKey, err)
	g.sink.OnReply(sessionKey, errorReply)
	return nil
}

// buildGroupPrompt assembles the prompt for a turn. For a DM (or when group
// context is disabled) it returns the raw message text. For a group message it
// injects the [Recent group messages] delta as UNTRUSTED background and
// demarcates the real request with the current-message anchor. CRITICAL ordering
// (group-context.ts): the delta is built BEFORE the current message is cached, so
// the message isn't echoed into its own background.
func (g *Gateway) buildGroupPrompt(sessionKey string, msg router.InboundMessage) string {
	if g.groups == nil || msg.ChannelType != router.ChannelGroup || msg.ChannelID == "" {
		return msg.Text
	}

	// Cold-start backfill (cc G4): the FIRST time this channel is seen with an
	// empty local window, seed it from the IM REST API. Runs at most once per
	// (process, channel). The inferred cutoff (highest bot-reply seq found in the
	// backfill) primes answered/new segmentation so the first turn doesn't treat
	// already-answered history as new.
	if g.groupBackfill != nil {
		channelID := msg.ChannelID
		botUID := ""
		if g.botUID != nil {
			botUID = g.botUID()
		}
		inferred, ran := g.groups.Backfill(channelID, botUID, func() []groupctx.BackfillMessage {
			return g.groupBackfill(channelID, 0)
		})
		if ran && inferred > 0 {
			if err := g.store.SaveBotReplySeq(sessionKey, inferred); err != nil {
				fmt.Fprintf(os.Stderr, "[gateway] save inferred reply seq %s: %v\n", sessionKey, err)
			}
		}
	}

	// Answered/new cutoff (cc G10): the IM seq of the last message the bot replied
	// to. Messages at/below it render under [Previously answered].
	cutoffSeq, err := g.store.BotReplySeq(sessionKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[gateway] bot reply seq %s: %v\n", sessionKey, err)
	}

	cursor := g.groups.Cursor(msg.ChannelID)
	deltaText, _ := g.groups.BuildContextSince(msg.ChannelID, cursor, cutoffSeq)
	// Cache the current message AFTER reading the delta.
	g.groups.Push(msg.ChannelID, msg.FromUID, msg.FromName, msg.Text, msg.MessageSeq)
	// Advance the cursor past everything now in the channel.
	g.groups.SetCursor(msg.ChannelID, g.groups.MaxID(msg.ChannelID))

	var b strings.Builder
	if deltaText != "" {
		// The whole block (header + raw bodies) is escaped once here.
		b.WriteString(safety.SanitizePromptBody(deltaText))
		b.WriteString("\n")
	}
	b.WriteString(safety.CurrentMessageAnchor)
	b.WriteString("\n")
	// Defense-in-depth: the current-message body is untrusted. Escape role labels
	// / section markers so a crafted body cannot forge prompt structure below the
	// real anchor (e.g. a second [Current message …] anchor or a fake
	// [Recent group messages] header).
	b.WriteString(safety.SafeBody(msg.Text).String())
	return b.String()
}

// resolveSandbox resolves the per-session sandbox (cwd + memory dir) and links
// the bot's skills/workflows into it. Returns ("", "", nil) when the sandbox is
// disabled. A non-nil error means the cwd could not be built — the caller MUST
// abort the turn rather than fall back to the process cwd (which would leak
// across sessions). Skill/workflow linking is best-effort (a missing skill only
// degrades capability, never breaks the turn).
func (g *Gateway) resolveSandbox(sessionKey string, msg router.InboundMessage) (cwd, memDir string, err error) {
	if g.cwdBase == "" {
		return "", "", nil
	}
	sctx := sandbox.SessionCtx{Kind: kindFor(msg.ChannelType), SessionKey: sessionKey}
	cwd, err = sandbox.ResolveSessionCwd(g.cwdBase, sctx)
	if err != nil {
		return "", "", fmt.Errorf("resolve sandbox cwd: %w", err)
	}
	// Link the bot's per-bot skills dir (~/.xclaw/<id>/skills) as the SINGLE
	// source: marketplace skills reach the sandbox only via symlinks the operator
	// installed there, plus the bot's own real skill dirs. The global catalog is
	// never linked directly (it's a read-only marketplace, not auto-exposed).
	_ = sandbox.LinkSkillsIntoSandbox(cwd, []sandbox.SkillSource{
		{Dir: g.skillsDir},
	})
	// Same for workflows (.claude/workflows): the per-bot dir is the only source.
	_ = sandbox.LinkWorkflowsIntoSandbox(cwd, []sandbox.SkillSource{
		{Dir: g.workflowsDir},
	})
	if g.memoryBase != "" {
		memDir = sandbox.ResolveMemoryDir(g.memoryBase, sctx)
	}
	return cwd, memDir, nil
}

// runTurn executes one accepted turn under the session lock.
func (g *Gateway) runTurn(ctx context.Context, sessionKey string, msg router.InboundMessage) error {
	// Ensure the session row exists (refreshes TTL). Touch avoids the extra
	// read-back the turn doesn't use.
	if err := g.store.Touch(sessionKey, msg.ChannelID, int(msg.ChannelType)); err != nil {
		return g.failTurn(sessionKey, "store.Touch", err)
	}

	// In-chat slash commands (/reset, /config, /help) — handled BEFORE group
	// -context caching, history append, and the agent query, so a command never
	// reaches the LLM, is not stored as a turn, and does not leak into other
	// members' group context. Scoped to this sessionKey: in a group that's the
	// whole channel's shared session (commands.ts / index.ts handleMessage).
	if reply, handled := g.handleCommand(msg.Text, sessionKey); handled {
		if reply != "" {
			g.sink.OnReply(sessionKey, reply)
		}
		return nil // skip context, history, and the agent query entirely
	}

	// Build the prompt. For group messages this injects the [Recent group
	// messages] delta as untrusted background and demarcates the real request
	// with the current-message anchor; DM messages pass through unchanged.
	prompt := g.buildGroupPrompt(sessionKey, msg)

	// Persist the (original) user message.
	if err := g.store.AppendUser(sessionKey, msg.Text, msg.FromName); err != nil {
		return g.failTurn(sessionKey, "store.AppendUser", err)
	}

	// Resume the agent's prior session if we have one. A real read error (not
	// "no row") degrades the turn to a fresh session — acceptable, but log it so
	// silent loss of conversation continuity is diagnosable.
	resumeID, err := g.store.Resume(sessionKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[gateway] resume %s: %v\n", sessionKey, err)
	}

	// Resolve the per-session sandbox (cwd + memory + skills) when enabled.
	cwd, memDir, err := g.resolveSandbox(sessionKey, msg)
	if err != nil {
		// Building the sandbox failed — running in the process cwd would leak
		// across sessions, which is exactly what this guards against. Fail loud
		// AND signal the user (don't leave them hanging on a silent failure).
		return g.failTurn(sessionKey, "resolve sandbox cwd", err)
	}

	// For GROUP turns, inject the channel roster + structured-mention format as
	// operator-trusted system context (gateway-authored, not user text). DMs get
	// no roster. Computed here where the channel id is in scope.
	var rosterPrefix string
	if g.groups != nil && msg.ChannelType == router.ChannelGroup && msg.ChannelID != "" {
		rosterPrefix = g.groups.MemberListPrefix(msg.ChannelID)
	}

	// Materialize inbound media/file attachments now that the session cwd is
	// known but before driver.Query (inbound.ts G1/G2 + media-inbound.ts #86):
	// images download into <cwd>/.xclaw-media for the Read tool; small text files
	// inline as a base64 <file_content> block. The returned hint/blocks go into
	// THIS turn's prompt ONLY — never the stored history (already persisted as the
	// original text above), so it can't accumulate stale paths or inlined bodies.
	//
	// Safety: this block sits after the current-message anchor but is NOT raw user
	// text — it is gateway-authored hint strings with only (a) filenames already
	// run through safety.SanitizeDisplayName (no brackets/newlines) and (b) file
	// bodies base64-wrapped (the base64 alphabet can't forge the </file_content>
	// tag or a section marker). So it cannot inject prompt structure even though
	// it is not re-escaped here.
	if media := g.materializeAttachments(ctx, cwd, msg.Attachments); media != "" {
		prompt += media
	}

	// Bound driver.Query + the stream loop with a per-turn dispatch timeout
	// (#141). Cancelling turnCtx kills the claude subprocess (CommandContext) and
	// closes the event stream, so a hung turn (stuck query, wedged tool, stalled
	// stream) can't block the session queue forever. On timeout we send an apology
	// and return nil — the per-session lock then releases as runTurn returns.
	turnCtx := ctx
	if g.dispatchTimeout > 0 {
		var cancel context.CancelFunc
		// WithTimeoutCause tags OUR deadline with a sentinel so we can tell our own
		// dispatch timeout apart from any caller cancellation without racily
		// comparing two ctx.Err() snapshots (M9).
		turnCtx, cancel = context.WithTimeoutCause(ctx, g.dispatchTimeout, errDispatchTimeout)
		defer cancel()
	}

	sysAppend := g.buildSystemPrompt(msg, rosterPrefix)
	var reply strings.Builder
	var newResume string
	var termErr string
	var termTransient bool
	var termHint string
	resume := resumeID
	for attempt := 0; ; attempt++ {
		events, err := g.driver.Query(turnCtx, agent.Request{
			Prompt:       prompt,
			SessionID:    resume,
			Cwd:          cwd,
			MemoryDir:    memDir,
			Model:        g.model,
			SystemAppend: sysAppend,
		})
		if err != nil {
			// Spawning/dispatching the agent failed (incl. the fresh retry after a
			// stale resume). Signal the user instead of returning silently — a bare
			// return would leave them with no reply and the typing indicator stuck.
			return g.failTurn(sessionKey, "driver.Query", err)
		}

		reply.Reset()
		newResume = ""
		termErr = ""
		termTransient = false
		termHint = ""
		resumeBad := false
		// On a resume attempt the stream may turn out doomed (stale resume id). To
		// avoid leaking a doomed attempt's events to the sink (H6) — the
		// ResumeInvalid signal arrives on stderr while content arrives on stdout,
		// with no ordering guarantee between the two reader goroutines — we GATE
		// sink emission: buffer events until the session proves valid (first
		// KindSessionStarted), then flush and stream live. If ResumeInvalid arrives
		// first, the buffer is dropped. A fresh attempt (no resume id) streams live
		// immediately. Latency is bounded by the first event, so a healthy resume is
		// unaffected.
		gated := resume != ""
		var gatedBuf []agent.AgentEvent
		emitToSink := func(ev agent.AgentEvent) {
			if gated {
				gatedBuf = append(gatedBuf, ev)
				return
			}
			g.sink.OnEvent(sessionKey, ev)
		}
		releaseGate := func() {
			if !gated {
				return
			}
			gated = false
			for _, e := range gatedBuf {
				g.sink.OnEvent(sessionKey, e)
			}
			gatedBuf = nil
		}
		for ev := range events {
			// A stale resume id (session not found, e.g. after the agent's config
			// dir changed) dooms this attempt — swallow its events so the failed
			// run never reaches the sink, then retry fresh below.
			if ev.ResumeInvalid {
				resumeBad = true
				gatedBuf = nil // drop anything buffered for the doomed attempt
				continue
			}
			if resumeBad {
				continue
			}
			emitToSink(ev)
			switch ev.Kind {
			case agent.KindSessionStarted:
				if ev.SessionID != "" {
					newResume = ev.SessionID
				}
				// The session is live — safe to flush buffered events and stream live.
				releaseGate()
			case agent.KindTextDelta:
				reply.WriteString(ev.Text)
			case agent.KindTurnDone:
				// Accumulate this turn's token usage into the bot's persistent
				// total (best-effort: a write failure must not fail the turn).
				if ev.Usage != nil {
					if err := g.store.AddUsage(ev.Usage.InputTokens, ev.Usage.OutputTokens, ev.Usage.CachedInputTokens, ev.Usage.CacheCreationInputTokens, ev.Usage.CostUSD); err != nil {
						fmt.Fprintf(os.Stderr, "[gateway] add usage %s: %v\n", sessionKey, err)
					}
				}
			case agent.KindError:
				// Terminal (non-recoverable) errors abort the turn: a result
				// is_error (e.g. max_turns), or a process exit BEFORE any
				// successful result. Recoverable errors (stderr warnings,
				// api_retry, and a non-zero exit that follows a completed turn)
				// are informational and don't gate the reply. (Stale-resume
				// errors are swallowed above via resumeBad before reaching here.)
				if !ev.Recoverable {
					termErr = ev.Err
					termTransient = ev.Transient
					termHint = ev.RetryHint
				}
			}
		}
		// Stream ended while still gated but not doomed (e.g. a valid resume that
		// produced no SessionStarted event): flush the buffer so nothing is lost.
		if !resumeBad {
			releaseGate()
		}

		// Self-heal a stale resume id: clear the mapping and retry once, fresh.
		if resumeBad && resume != "" && attempt == 0 {
			fmt.Fprintf(os.Stderr, "[gateway] stale resume id for %s; clearing and retrying fresh\n", sessionKey)
			_ = g.store.ClearResume(sessionKey)
			resume = ""
			continue
		}
		break
	}

	// If the turn was cut short by the dispatch timeout (not the caller's own
	// cancellation), apologize and release the lock. context.Cause returns our
	// sentinel ONLY when turnCtx expired on its own deadline — a parent
	// cancellation propagates the parent's cause instead, so this is unambiguous
	// even if both fire near-simultaneously (M9).
	if g.dispatchTimeout > 0 && errors.Is(context.Cause(turnCtx), errDispatchTimeout) {
		fmt.Fprintf(os.Stderr, "[gateway] dispatch timed out after %s (session=%s)\n", g.dispatchTimeout, sessionKey)
		g.sink.OnReply(sessionKey, timeoutReply)
		return nil
	}

	// Terminal agent error: treat like the timeout path. Do NOT persist the
	// partial reply as the assistant turn and do NOT advance the resume id —
	// otherwise an errored turn silently commits a truncated answer and skips
	// forward, corrupting continuity. Signal the user and release the lock. An
	// upstream rate-limit / overload gets a distinct "服务繁忙" reply (with the
	// reset window when the agent reported one) so it reads as capacity, not a bug.
	if termErr != "" {
		if termTransient {
			fmt.Fprintf(os.Stderr, "[gateway] transient upstream error (session=%s): %s\n", sessionKey, termErr)
			reply := busyReply
			if termHint != "" {
				reply = busyReply + "（" + termHint + " 后恢复）"
			}
			g.sink.OnReply(sessionKey, reply)
			return nil
		}
		fmt.Fprintf(os.Stderr, "[gateway] terminal agent error (session=%s): %s\n", sessionKey, termErr)
		g.sink.OnReply(sessionKey, errorReply)
		return nil
	}

	// Persist resume id for continuity (only if the agent reported one). A write
	// failure here silently breaks continuity on the next turn, so log it.
	if newResume != "" {
		if err := g.store.SaveResume(sessionKey, g.driver.Name(), newResume); err != nil {
			fmt.Fprintf(os.Stderr, "[gateway] save resume %s: %v\n", sessionKey, err)
		}
	}

	text := reply.String()
	// Persist the assistant turn. A write failure must NOT suppress delivery — the
	// reply was produced, so still send it; just log the persistence loss (history
	// will be missing this turn, but the user gets their answer).
	if err := g.store.AppendAssistant(sessionKey, text, g.driver.Name()); err != nil {
		fmt.Fprintf(os.Stderr, "[gateway] append assistant %s: %v\n", sessionKey, err)
	}
	g.sink.OnReply(sessionKey, text)

	// Advance the answered/new cursor (cc G10 / openclaw lastBotReplySeqMap): record
	// the inbound message_seq the bot just replied to so later turns segment this
	// message (and everything before it) as [Previously answered]. We use the
	// inbound seq (from the IM frame), NOT the send result — the send API returns
	// seq 0. Skip synthetic/cron fires (seq 0); the store write is monotonic and a
	// no-op for seq<=0. Only for group turns that actually produced a reply.
	if g.groups != nil && msg.ChannelType == router.ChannelGroup && strings.TrimSpace(text) != "" {
		if err := g.store.SaveBotReplySeq(sessionKey, msg.MessageSeq); err != nil {
			fmt.Fprintf(os.Stderr, "[gateway] save reply seq %s: %v\n", sessionKey, err)
		}
	}
	return nil
}

// buildSystemPrompt assembles the frozen system-prompt append: the
// non-overridable security prefix, the operator-trusted SOUL/config prompt,
// then (for GROUP/thread turns) the gateway-authored member roster +
// mention-format hint, the operator-authored [Group instructions] block for
// this channel, and (for persona clones) the persona instruction. The
// SecurityPrefix always stays first and non-overridable. (The driver's preset
// base prompt is prepended by the agent CLI.)
//
// rosterPrefix is "" for DMs and for groups with no learned members. [Group
// instructions] is injected only for groups (cc-channel-octo index.ts). Persona
// injection mirrors openclaw inbound.ts (synthesized group hint + free-form
// persona prompt). All are config/gateway-authored (never from message
// payloads), so each is wrapped as safety.TrustedText after the SecurityPrefix.
func (g *Gateway) buildSystemPrompt(msg router.InboundMessage, rosterPrefix string) string {
	parts := []safety.SafeText{safety.TrustedText(safety.SecurityPrefix)}
	if g.systemPrompt != "" {
		parts = append(parts, safety.TrustedText(g.systemPrompt))
	}
	if rosterPrefix != "" {
		parts = append(parts, safety.TrustedText(rosterPrefix))
	}
	if g.groupMD != nil && msg.ChannelType == router.ChannelGroup && msg.ChannelID != "" {
		if instr, ok := g.groupMD.Load(msg.ChannelID); ok {
			parts = append(parts, safety.TrustedText("[Group instructions]\n"+instr))
		}
	}
	if g.persona.Configured() {
		if p := g.persona.BuildGroupSystemPrompt(); p != "" {
			parts = append(parts, safety.TrustedText(p))
		}
		if h := g.persona.ComposeHint(g.personaPrompt); h != "" {
			parts = append(parts, safety.TrustedText(h))
		}
	}
	var b strings.Builder
	for i, p := range parts {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(p.String())
	}
	return b.String()
}
