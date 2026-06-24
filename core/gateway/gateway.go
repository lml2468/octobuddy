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

	"github.com/lml2468/octobuddy/core/agent"
	"github.com/lml2468/octobuddy/core/groupctx"
	"github.com/lml2468/octobuddy/core/groupmd"
	"github.com/lml2468/octobuddy/core/persona"
	"github.com/lml2468/octobuddy/core/router"
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

// errTurnConcluded marks a turn a helper already finished — it sent the user
// their reply (a handled command, or a failTurn apology) and the session lock
// should release cleanly. It is swallowed at runTurn's boundary via
// ignoreConcluded and never reaches the router.
var errTurnConcluded = errors.New("gateway: turn concluded")

// ignoreConcluded maps the errTurnConcluded sentinel back to nil at runTurn's
// boundary, so a deliberately-stopped turn is not reported to the router as a
// handler failure. Any other error propagates unchanged.
func ignoreConcluded(err error) error {
	if errors.Is(err, errTurnConcluded) {
		return nil
	}
	return err
}

// failTurn logs an internal turn failure and sends the user a generic apology so
// no error path is silently swallowed (which would also leave the typing
// indicator running until it times out). It returns errTurnConcluded so any
// caller that propagates with `if err != nil { return err }` stops the turn
// correctly; runTurn translates the sentinel back to nil at its boundary.
func (g *Gateway) failTurn(sessionKey, stage string, err error) error {
	fmt.Fprintf(os.Stderr, "[gateway] turn failed at %s (session=%s): %v\n", stage, sessionKey, err)
	g.sink.OnReply(sessionKey, errorReply)
	return errTurnConcluded
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

func (g *Gateway) startTurn(sessionKey string, msg router.InboundMessage) error {
	g.sink.OnUserMessage(sessionKey, msg)
	if err := g.store.Touch(sessionKey, msg.ChannelID, int(msg.ChannelType)); err != nil {
		return g.failTurn(sessionKey, "store.Touch", err)
	}
	reply, handled := g.handleCommand(msg.Text, sessionKey)
	if !handled {
		return nil
	}
	if reply != "" {
		g.sink.OnReply(sessionKey, reply)
	}
	return errTurnConcluded
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
