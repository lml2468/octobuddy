// Package gateway orchestrates the end-to-end turn pipeline:
//
//	inbound → router (lock + gate + rate limit) → getOrCreate session →
//	load resume id → driver.Query → stream events → deliver to sink →
//	persist assistant reply + resume id
//
// Depends only on agent.Driver + Sink, so it is agent- and IM-agnostic.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
	// OnUserMessage fires at the start of an accepted turn so observer
	// sinks (control bus → GUI) can render the inbound in the transcript.
	// The IM connector implements this as a no-op (the message originated
	// there). NOT called for messages dropped/rate-limited before runTurn.
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
	// Optional cold-start backfill (set via WithGroupBackfill). Returns
	// recent channel messages from the IM REST API to seed an empty group
	// window the first time a channel is seen. botUID lets backfill skip
	// the bot's own replies and infer the initial answered/new cutoff.
	groupBackfill func(channelID string, limit int) []groupctx.BackfillMessage
	botUID        func() string
	// owner resolves the bot owner's uid (empty before IM registration). Set via
	// WithOwner. Gates owner-only prompt injection (the bootstrap block in the
	// owner's DM). nil → no owner known (treated as no IM-owner DM match; Console
	// is gated separately by Source).
	owner func() string
	// Operator-trusted system prompt (SOUL.md + AGENTS.md), a static snapshot set
	// via WithSystemPrompt. In production (config mode) the daemon installs
	// resolveSystemPromptFn instead and this stays "" — so this field is the
	// fallback used only by callers that set a fixed prompt without a resolver
	// (tests, and any future single-shot/REPL embedder). effectiveSystemPrompt
	// reads it only when resolveSystemPromptFn is nil.
	systemPrompt string
	// resolveSystemPromptFn, when set, re-reads SOUL.md/AGENTS.md PER TURN so a
	// desktop edit applies on the next message without a daemon restart (mirrors
	// resolveToolsFn). nil → fall back to the static systemPrompt snapshot. An
	// empty result is honored (operator cleared both files → SecurityPrefix only).
	resolveSystemPromptFn func() string
	// resolveBootstrapFn, when set, re-reads BOOTSTRAP.md PER TURN. Its content is
	// injected ONLY in an owner-trusted channel (Console or the owner's DM) and
	// only while the file exists — the bot deletes it once self-defined. nil/empty
	// → no bootstrap block.
	resolveBootstrapFn func() string
	// Persona-clone grantor (OBO). When configured, a persona instruction
	// is injected so the bot replies in the grantor's voice. Zero value =
	// a regular bot. personaPrompt is an optional free-form persona block
	// appended after the synthesized group hint.
	persona       persona.Grantor
	personaPrompt string
	// Optional model override passed to the driver (empty = driver default).
	model string
	// Tool surface policy (set via WithToolResolver). resolveToolsFn is
	// evaluated PER TURN with the session key and returns (tools, ok): ok=false
	// → leave Request.AllowedTools nil so the driver uses its probed
	// headless-safe default; ok=true → use the returned list verbatim (a
	// present channel entry / non-nil default, incl. empty = muzzle). Resolved
	// per turn (not cached at construction) so a desktop edit to a channel's
	// tools applies on the next turn without a daemon restart. nil = no policy
	// (every session falls through to the driver default).
	resolveToolsFn func(sessionKey string) (tools []string, ok bool)
	// settingSources is the per-bot claude setting-source scope list passed
	// on every turn (empty = driver default "user"). Set via WithSettingSources.
	settingSources []string
	// Per-session sandbox roots (set via WithSandbox). Each turn runs in
	// cwdBase/<hash>, with auto-memory under memoryBase/<hash>. Skills +
	// workflows live under the per-bot CLAUDE_CONFIG_DIR and the claude
	// CLI auto-discovers them; no per-turn link work needed. Empty
	// cwdBase = no isolation (inherit proc).
	cwdBase, memoryBase string
	// mediaAuth supplies the Authorization header for inbound-media
	// downloads (scoped to the IM's apiUrl host). Set via WithMediaAuth by
	// the IM connector; keeps the gateway IM-agnostic.
	mediaAuth MediaAuth
	// assertPublic / mediaClient: test seams only (override the SSRF guard
	// and the media HTTP client). Production never sets them.
	assertPublic func(ctx context.Context, rawURL string) error
	mediaClient  *http.Client

	// dispatchTimeout bounds a single turn as an IDLE deadline: the timer
	// resets on every AgentEvent, so a long-running healthy turn survives
	// as long as events flow — only true silence kills it. On expiry the
	// turn's context is cancelled (which kills the claude subprocess via
	// CommandContext) and the user gets a "处理超时" apology. <=0 disables.
	dispatchTimeout time.Duration

	// Effective settings surfaced by /config (no secrets). Set via WithCommandInfo.
	maxPerMinute int
	contextChars int

	// sessionTouch fires after every AppendUser / AppendAssistant so the
	// GUI can broadcast `session.upserted` without polling. nil = no-op.
	sessionTouch func(sessionKey, channelID string, channelType router.ChannelType)
}

// defaultDispatchTimeout is the idle-deadline default — long enough for
// most multi-tool workflows between events, short enough that a hung
// turn frees its session lock.
const defaultDispatchTimeout = 20 * time.Minute

// Handle routes one reply-warranting inbound through the full pipeline,
// holding the per-session lock across the turn so same-session turns
// serialize.
//
// PRECONDITION: msg.ShouldReply() must be true. Observations route via
// Observe; OBO-irrelevant drops are filtered at the connector. A caller
// violating the contract gets DroppedInvariantBreak silently dropped at
// the router and a WARN here — chosen over panic so one bug doesn't take
// down every bot in the daemon.
//
// Friendly drop replies emitted via the Sink:
//   - DroppedTooLong → "消息过长，请缩短后重试"
//   - RateLimited   → "请稍后再试" (deduped per rate-limit window)
//
// DroppedUnroutable stays silent (no routable identity to reply to).
func (g *Gateway) Handle(ctx context.Context, msg router.InboundMessage) (router.Decision, error) {
	d, err := g.router.RouteAndHandle(ctx, msg, g.runTurn)

	switch d {
	case router.DroppedTooLong:
		if key, kerr := msg.SessionKey(); kerr == nil {
			g.sink.OnReply(key, oversizedReply)
		}
	case router.RateLimited:
		// Router decided notify-once atomically with the rejection
		// (subsequent rejections in the same window route to
		// RateLimitedSilent), so a flooder doesn't get one reply per drop.
		if key, kerr := msg.SessionKey(); kerr == nil {
			g.sink.OnReply(key, rateLimitedReply)
		}
	case router.DroppedInvariantBreak:
		// Surface caller bugs as a greppable WARN rather than letting
		// the bot silently go mute for some messages.
		key, _ := msg.SessionKey()
		glog().Warn("router invariant break — caller passed non-reply msg to Handle",
			"session", key, "channel_type", msg.ChannelType, "has_trigger", msg.Trigger != nil)
	}
	return d, err
}

// Friendly drop / failure replies.
const (
	oversizedReply   = "消息过长，请缩短后重试"
	rateLimitedReply = "请稍后再试"
	timeoutReply     = "⚠️ 处理超时，请稍后重试。"
	// errorReply is sent on a terminal agent error; the partial reply is
	// NOT persisted and the resume id is NOT advanced.
	errorReply = "⚠️ 出错了，请稍后重试。"
	// busyReply distinguishes upstream capacity issues (HTTP 429/503/529)
	// from generic bugs, so the user knows it's not their fault.
	busyReply = "⏳ 服务繁忙，请稍后重试。"
)

// Observe caches a non-triggering group message into group context so it
// becomes background for a later @-mention turn. No-op outside groups
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

// errTurnConcluded marks a turn that a helper already finished (sent the
// user their reply, or a failTurn apology). Swallowed at runTurn's
// boundary via ignoreConcluded; never reaches the router.
var errTurnConcluded = errors.New("gateway: turn concluded")

func ignoreConcluded(err error) error {
	if errors.Is(err, errTurnConcluded) {
		return nil
	}
	return err
}

// failTurn logs an internal turn failure, sends the user a generic
// apology (so no error is silently swallowed and the typing indicator
// doesn't hang), and returns errTurnConcluded so propagation stops cleanly.
func (g *Gateway) failTurn(sessionKey, stage string, err error) error {
	glog().Error("turn failed", "stage", stage, "session", sessionKey, "err", err)
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
