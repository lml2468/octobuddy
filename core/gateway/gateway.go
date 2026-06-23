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
	"sync/atomic"
	"time"

	"github.com/lml2468/octobuddy/core/agent"
	"github.com/lml2468/octobuddy/core/groupctx"
	"github.com/lml2468/octobuddy/core/groupmd"
	"github.com/lml2468/octobuddy/core/persona"
	"github.com/lml2468/octobuddy/core/router"
	"github.com/lml2468/octobuddy/core/safety"
	"github.com/lml2468/octobuddy/core/sandbox"
	"github.com/lml2468/octobuddy/core/store"
)

// Sink receives normalized agent events for one turn, plus a final assembled
// reply. Implementations deliver to an IM, stdout, the control bus, etc.
type Sink interface {
	// OnEvent is called for each streamed AgentEvent (text/tool/etc.).
	OnEvent(sessionKey string, ev agent.AgentEvent)
	// OnReply is called once with the full assembled assistant text (may be "").
	OnReply(sessionKey string, text string)
	// OnUserMessage is called once at the start of an accepted turn, BEFORE
	// any agent work — its purpose is to let observer sinks (control bus →
	// GUI) render the inbound user message in the chat transcript. The IM
	// connector implements this as a no-op (the message originated there),
	// the GUI's control-bus EventSink broadcasts session.user_message so a
	// desktop attached to an IM-originated session sees what the remote
	// human actually sent — without this, the GUI only saw the bot's reply
	// and the transcript read like a monologue. NOT called for messages
	// dropped/rate-limited before runTurn — those reach the user via the
	// distinct oversized/rateLimited reply path instead.
	OnUserMessage(sessionKey string, msg router.InboundMessage)
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
	// turn runs in cwdBase/<hash>, with auto-memory under memoryBase/<hash>.
	// The bot's skills + workflows are NOT linked into the sandbox: they live
	// under the per-bot CLAUDE_CONFIG_DIR (~/.octobuddy/<id>/.claude/{skills,workflows})
	// and the claude CLI auto-discovers them as user-scope assets — every spawn
	// already loads them, no per-turn link work needed. Empty cwdBase = no
	// isolation (inherit proc).
	cwdBase, memoryBase string
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

	// dispatchTimeout bounds a single turn (#141), but as an IDLE deadline: the
	// timer resets on every AgentEvent received from the driver, so a long-running
	// healthy turn (multi-agent workflow, big stream, lots of tool calls) survives
	// as long as events keep flowing — only true silence kills it. On expiry the
	// turn's context is cancelled (which kills the claude subprocess via
	// CommandContext) and the user gets a "处理超时" apology. The session lock then
	// releases as runTurn returns, so a wedged turn cannot block the queue forever.
	// <=0 disables the bound. Default 20 min — long enough to cover most complex
	// workflows between events, short enough that a truly hung turn frees up.
	dispatchTimeout time.Duration

	// Effective settings surfaced by /config (no secrets). Set via WithCommandInfo.
	maxPerMinute int
	contextChars int

	// sessionTouch, when set, is invoked after every successful AppendUser /
	// AppendAssistant — i.e. anything that changes a session row's preview /
	// updatedAt / first-existence. The GUI side subscribes here to broadcast
	// a `session.upserted` event so the sidebar reflects new/touched rows
	// without polling. Set via WithSessionTouchNotifier; nil = no-op. The
	// callback receives only the minimal coordinates (key, channel id,
	// channel type) — the subscriber builds whatever projection it needs.
	sessionTouch func(sessionKey, channelID string, channelType router.ChannelType)
}

// defaultDispatchTimeout bounds a single turn as an idle deadline (#141 — config.ts
// dispatchTimeoutMs). Reset on every AgentEvent, so it kills only a truly silent
// turn, not a long-running healthy one.
const defaultDispatchTimeout = 20 * time.Minute

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

// Handle routes one inbound message through the full pipeline. The router holds
// the per-session lock across the whole turn, so same-session turns serialize.
// Returns the router decision (so callers can log drops/limits).
//
// Friendly drop replies (ported from cc-channel session-router.ts) are emitted
// here, through the Sink, so every caller benefits without re-implementing them:
// - DroppedTooLong → "消息过长，请缩短后重试"
// - RateLimited → "请稍后再试" (deduped per rate-limit window; see router)
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

// errDispatchTimeout is the cause attached to the per-turn idle deadline, so
// runTurn can distinguish its own timeout from a caller cancellation via
// context.Cause (M9).
var errDispatchTimeout = errors.New("dispatch idle timeout")

// idleGuard wraps the per-turn idle deadline plumbing. Reset on every event;
// expired reports whether OUR timer fired (vs a parent cancellation). When
// the timeout is <=0 every method is a no-op so callers stay branch-free.
type idleGuard struct {
	timeout time.Duration
	cancel  context.CancelCauseFunc
	timer   *time.Timer
	// done is set by the runTurn loop when it observes a successful
	// terminal event. expired() honors it so a race between AfterFunc
	// firing and the success event can't reroute a completed turn into
	// the timeout-reply branch.
	done atomic.Bool
}

// newIdleGuard returns a child ctx and a guard. With timeout <=0 the guard is
// inert (ctx unchanged, reset/stop/expired are no-ops).
func newIdleGuard(parent context.Context, timeout time.Duration) (context.Context, *idleGuard) {
	if timeout <= 0 {
		return parent, &idleGuard{}
	}
	ctx, cancel := context.WithCancelCause(parent)
	g := &idleGuard{timeout: timeout, cancel: cancel}
	// time.AfterFunc fires once after the idle window; reset Resets it. The
	// closure captures `cancel` so an expiry tags the cancellation with our
	// sentinel, letting expired tell our own timeout apart from a parent
	// cancellation (M9).
	g.timer = time.AfterFunc(timeout, func() { cancel(errDispatchTimeout) })
	return ctx, g
}

func (g *idleGuard) reset() {
	if g.timer != nil {
		g.timer.Reset(g.timeout)
	}
}

func (g *idleGuard) stop() {
	if g.timer == nil {
		return
	}
	// Only cancel with a nil cause when WE preempted the timer (Stop returns
	// true). If Stop returns false the AfterFunc has already fired (or is in
	// flight) and is about to call cancel(errDispatchTimeout); racing it with
	// cancel(nil) here would mis-classify a fired timer as a clean stop,
	// confusing context.Cause readers. cancel(nil) after a
	// non-nil cancel cause is a no-op, so this is safe either way — but
	// preferring "don't race" keeps the invariant explicit.
	if g.timer.Stop() {
		g.cancel(nil)
	}
}

func (g *idleGuard) markDone() {
	if g.timer != nil {
		g.done.Store(true)
	}
}

func (g *idleGuard) expired(ctx context.Context) bool {
	if g.timer == nil || g.done.Load() {
		return false
	}
	return errors.Is(context.Cause(ctx), errDispatchTimeout)
}

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

// SessionCwd resolves the on-disk sandbox cwd for a session — the directory
// the agent will run inside, and where Composer-side attachments should land
// so they share the IM-inbound .octobuddy-media/ layout. Returns ("", nil)
// when the gateway has no sandbox configured (REPL / unit tests). Public so
// control-bus handlers can prepare a turn's sandbox before calling Handle.
func (g *Gateway) SessionCwd(channelType router.ChannelType, sessionKey string) (string, error) {
	if g.cwdBase == "" {
		return "", nil
	}
	sctx := sandbox.SessionCtx{Kind: kindFor(channelType), SessionKey: sessionKey}
	cwd, err := sandbox.ResolveSessionCwd(g.cwdBase, sctx)
	if err != nil {
		return "", fmt.Errorf("resolve sandbox cwd: %w", err)
	}
	return cwd, nil
}

// resolveSandbox resolves the per-session sandbox (cwd + memory dir). Returns
// ("", "", nil) when the sandbox is disabled. A non-nil error means the cwd
// could not be built — the caller MUST abort the turn rather than fall back to
// the process cwd (which would leak across sessions). Skills + workflows are
// auto-loaded by the CLI from CLAUDE_CONFIG_DIR (~/.octobuddy/<id>/.claude/),
// not symlinked in per-turn — see CLAUDE.md.
func (g *Gateway) resolveSandbox(sessionKey string, msg router.InboundMessage) (cwd, memDir string, err error) {
	cwd, err = g.SessionCwd(msg.ChannelType, sessionKey)
	if err != nil || cwd == "" {
		return "", "", err
	}
	if g.memoryBase != "" {
		sctx := sandbox.SessionCtx{Kind: kindFor(msg.ChannelType), SessionKey: sessionKey}
		memDir = sandbox.ResolveMemoryDir(g.memoryBase, sctx)
	}
	return cwd, memDir, nil
}

// runTurn executes one accepted turn under the session lock.
func (g *Gateway) runTurn(ctx context.Context, sessionKey string, msg router.InboundMessage) error {
	if handled, err := g.startTurn(sessionKey, msg); err != nil || handled {
		return err
	}

	// Build the prompt. For group messages this injects the [Recent group
	// messages] delta as untrusted background and demarcates the real request
	// with the current-message anchor; DM messages pass through unchanged.
	// Snapshot the pre-build group cursor for this channel and defer a
	// conditional rewind. buildGroupPrompt advances the cursor past the
	// current message; any turn-aborting failure BEFORE we've actually
	// produced + delivered a reply must roll it back, or the unanswered
	// message silently drops from every subsequent [Recent group messages]
	// delta. Set turnDelivered=true once the reply is on its way out so the
	// happy path keeps the cursor advanced.
	turnDelivered := false
	defer g.rewindGroupCursorUnlessDelivered(msg, &turnDelivered)()
	prompt := g.buildGroupPrompt(sessionKey, msg)

	// Persist the (original) user message. CronFire is persisted so the
	// desktop GUI's cron badge survives a chat-window reload — without it,
	// reopening a conversation would replay every prior scheduler-fired
	// prompt as if it had been typed by a human.
	if err := g.store.AppendUser(sessionKey, msg.Text, msg.FromName, msg.CronFire); err != nil {
		return g.failTurn(sessionKey, "store.AppendUser", err)
	}
	g.notifySessionTouch(sessionKey, msg.ChannelID, msg.ChannelType)

	// Resume the agent's prior session if we have one. A real read error (not
	// "no row") degrades the turn to a fresh session — acceptable, but log it so
	// silent loss of conversation continuity is diagnosable.
	resumeID, err := g.store.Resume(sessionKey, g.driver.Name())
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

	rosterPrefix := g.rosterPrefix(msg)

	// Materialize inbound media/file attachments now that the session cwd is
	// known but before driver.Query (inbound.ts G1/G2 + media-inbound.ts #86):
	// images download into <cwd>/.octobuddy-media for the Read tool; small text files
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

	// Bound driver.Query + the stream loop with a per-turn IDLE timeout (#141).
	// Cancelling turnCtx kills the claude subprocess (CommandContext) and closes
	// the event stream, so a hung turn (stuck query, wedged tool, stalled stream)
	// can't block the session queue forever. The timer resets on every AgentEvent
	// — a long but healthy turn (multi-agent workflow, big stream) survives as
	// long as events flow; only true silence kills it. On expiry we send an
	// apology and return nil — the per-session lock then releases as runTurn
	// returns.
	turnCtx, idle := newIdleGuard(ctx, g.dispatchTimeout)
	defer idle.stop()

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
		// avoid leaking a doomed attempt's events to the sink — the
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
			// Reset the idle deadline on every event — a steady stream keeps the
			// turn alive, only true silence kills it.
			idle.reset()
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
				// skip when termErr was set earlier in this
				// turn (the parser emits KindError before KindTurnDone for an
				// is_error=true result, e.g. max_turns) — billing tokens +
				// bumping the turns counter for a turn the user is told failed
				// over-attributes both metrics.
				//
				// Also skip when resumeBad is set: the doomed attempt's Usage
				// is from a stale-resume run we're about to retry, and the
				// retry's KindTurnDone will commit a fresh Usage line. Without
				// this gate the same logical turn double-billed tokens, cost,
				// AND turn count on every self-heal.
				if termErr == "" && !resumeBad && ev.Usage != nil {
					if err := g.store.AddUsage(ev.Usage.InputTokens, ev.Usage.OutputTokens, ev.Usage.CachedInputTokens, ev.Usage.CacheCreationInputTokens, ev.Usage.CostUSD); err != nil {
						fmt.Fprintf(os.Stderr, "[gateway] add usage %s: %v\n", sessionKey, err)
					}
				}
				// Mark the idle guard done so a concurrent AfterFunc firing
				// in the same tick as this success event can't reroute the
				// post-loop expired() check into the timeout-reply branch.
				if termErr == "" && !resumeBad {
					idle.markDone()
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
			// Per-agent clear: self-heal only nukes THIS driver's
			// row, not every agent's. the prior composite-PK promise was
			// that two drivers can hold concurrent resume ids; a blanket
			// ClearResume(sessionKey) would have crossed that boundary.
			_ = g.store.ClearResumeForAgent(sessionKey, g.driver.Name())
			resume = ""
			continue
		}
		break
	}

	if g.handleDispatchTimeout(turnCtx, idle, sessionKey, &turnDelivered) {
		return nil
	}

	if handled := g.handleTerminalAgentError(sessionKey, termErr, termTransient, termHint, &turnDelivered); handled {
		return nil
	}

	text := reply.String()
	g.completeSuccessfulTurn(sessionKey, msg, newResume, text)
	turnDelivered = true
	return nil
}

func (g *Gateway) handleDispatchTimeout(ctx context.Context, idle *idleGuard, sessionKey string, delivered *bool) bool {
	if !idle.expired(ctx) {
		return false
	}
	fmt.Fprintf(os.Stderr, "[gateway] dispatch idle timeout after %s (session=%s)\n", g.dispatchTimeout, sessionKey)
	*delivered = true
	g.sink.OnReply(sessionKey, timeoutReply)
	return true
}

func (g *Gateway) handleTerminalAgentError(sessionKey, termErr string, transient bool, hint string, delivered *bool) bool {
	if termErr == "" {
		return false
	}
	if transient {
		fmt.Fprintf(os.Stderr, "[gateway] transient upstream error (session=%s): %s\n", sessionKey, termErr)
		reply := busyReply
		if hint != "" {
			reply = busyReply + "（" + hint + " 后恢复）"
		}
		*delivered = true
		g.sink.OnReply(sessionKey, reply)
		return true
	}
	fmt.Fprintf(os.Stderr, "[gateway] terminal agent error (session=%s): %s\n", sessionKey, termErr)
	g.sink.OnReply(sessionKey, errorReply)
	return true
}

func (g *Gateway) completeSuccessfulTurn(sessionKey string, msg router.InboundMessage, newResume, text string) {
	if newResume != "" {
		if err := g.store.SaveResume(sessionKey, g.driver.Name(), newResume); err != nil {
			fmt.Fprintf(os.Stderr, "[gateway] save resume %s: %v\n", sessionKey, err)
		}
	}
	if err := g.store.AppendAssistant(sessionKey, text, g.driver.Name()); err != nil {
		fmt.Fprintf(os.Stderr, "[gateway] append assistant %s: %v\n", sessionKey, err)
	}
	g.notifySessionTouch(sessionKey, msg.ChannelID, msg.ChannelType)
	g.sink.OnReply(sessionKey, text)
	if g.groups != nil && msg.ChannelType == router.ChannelGroup && strings.TrimSpace(text) != "" {
		if err := g.store.SaveBotReplySeq(sessionKey, msg.MessageSeq); err != nil {
			fmt.Fprintf(os.Stderr, "[gateway] save reply seq %s: %v\n", sessionKey, err)
		}
	}
}

func (g *Gateway) startTurn(sessionKey string, msg router.InboundMessage) (bool, error) {
	g.sink.OnUserMessage(sessionKey, msg)
	if err := g.store.Touch(sessionKey, msg.ChannelID, int(msg.ChannelType)); err != nil {
		return false, g.failTurn(sessionKey, "store.Touch", err)
	}
	reply, handled := g.handleCommand(msg.Text, sessionKey)
	if !handled {
		return false, nil
	}
	if reply != "" {
		g.sink.OnReply(sessionKey, reply)
	}
	return true, nil
}

func (g *Gateway) rewindGroupCursorUnlessDelivered(msg router.InboundMessage, delivered *bool) func() {
	if g.groups == nil || msg.ChannelType != router.ChannelGroup || msg.ChannelID == "" {
		return func() {}
	}
	preCursor := g.groups.Cursor(msg.ChannelID)
	return func() {
		if !*delivered {
			g.groups.RewindCursor(msg.ChannelID, preCursor)
		}
	}
}

func (g *Gateway) rosterPrefix(msg router.InboundMessage) string {
	if g.groups == nil || msg.ChannelType != router.ChannelGroup || msg.ChannelID == "" {
		return ""
	}
	return g.groups.MemberListPrefix(msg.ChannelID)
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
