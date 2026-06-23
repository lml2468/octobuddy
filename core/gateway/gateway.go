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

	turnDelivered := false
	defer g.rewindGroupCursorUnlessDelivered(msg, &turnDelivered)()
	req, err := g.prepareAgentRequest(ctx, sessionKey, msg)
	if err != nil {
		return err
	}

	turnCtx, idle := newIdleGuard(ctx, g.dispatchTimeout)
	defer idle.stop()

	var attemptResult agentAttemptResult
	resume := req.SessionID
	for attempt := 0; ; attempt++ {
		req.SessionID = resume
		events, err := g.driver.Query(turnCtx, req)
		if err != nil {
			return g.failTurn(sessionKey, "driver.Query", err)
		}

		attemptResult = g.consumeAgentAttempt(sessionKey, events, idle, resume != "")

		if shouldRetryFreshResume(attemptResult, resume, attempt) {
			fmt.Fprintf(os.Stderr, "[gateway] stale resume id for %s; clearing and retrying fresh\n", sessionKey)
			_ = g.store.ClearResumeForAgent(sessionKey, g.driver.Name())
			resume = ""
			continue
		}
		break
	}

	if g.handleDispatchTimeout(turnCtx, idle, sessionKey, &turnDelivered) {
		return nil
	}

	if handled := g.handleTerminalAgentError(sessionKey, attemptResult.termErr, attemptResult.termTransient, attemptResult.termHint, &turnDelivered); handled {
		return nil
	}

	g.completeSuccessfulTurn(sessionKey, msg, attemptResult.newResume, attemptResult.reply)
	turnDelivered = true
	return nil
}

func shouldRetryFreshResume(res agentAttemptResult, resume string, attempt int) bool {
	return res.resumeBad && resume != "" && attempt == 0
}

type agentAttemptResult struct {
	reply         string
	newResume     string
	termErr       string
	termTransient bool
	termHint      string
	resumeBad     bool
}

func (g *Gateway) consumeAgentAttempt(sessionKey string, events <-chan agent.AgentEvent, idle *idleGuard, gated bool) agentAttemptResult {
	var res agentAttemptResult
	var reply strings.Builder
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
		// A stale resume id dooms this attempt. Swallow its events so the failed
		// run never reaches the sink, then retry fresh in runTurn.
		if ev.ResumeInvalid {
			res.resumeBad = true
			gatedBuf = nil
			continue
		}
		if res.resumeBad {
			continue
		}
		emitToSink(ev)
		g.consumeAgentEvent(sessionKey, ev, idle, &reply, &res, releaseGate)
	}
	// Stream ended while still gated but not doomed (e.g. a valid resume that
	// produced no SessionStarted event): flush the buffer so nothing is lost.
	if !res.resumeBad {
		releaseGate()
	}
	res.reply = reply.String()
	return res
}

func (g *Gateway) consumeAgentEvent(sessionKey string, ev agent.AgentEvent, idle *idleGuard, reply *strings.Builder, res *agentAttemptResult, releaseGate func()) {
	switch ev.Kind {
	case agent.KindSessionStarted:
		if ev.SessionID != "" {
			res.newResume = ev.SessionID
		}
		// The session is live — safe to flush buffered events and stream live.
		releaseGate()
	case agent.KindTextDelta:
		reply.WriteString(ev.Text)
	case agent.KindTurnDone:
		g.consumeTurnDone(sessionKey, ev, idle, res)
	case agent.KindError:
		g.consumeAgentError(ev, res)
	}
}

func (g *Gateway) consumeTurnDone(sessionKey string, ev agent.AgentEvent, idle *idleGuard, res *agentAttemptResult) {
	// Accumulate this turn's token usage into the bot's persistent total
	// (best-effort: a write failure must not fail the turn). Skip when an earlier
	// terminal error made this a failed turn, or when this is a stale-resume run
	// that will be retried fresh.
	if shouldCommitUsage(ev, res) {
		if err := g.store.AddUsage(ev.Usage.InputTokens, ev.Usage.OutputTokens, ev.Usage.CachedInputTokens, ev.Usage.CacheCreationInputTokens, ev.Usage.CostUSD); err != nil {
			fmt.Fprintf(os.Stderr, "[gateway] add usage %s: %v\n", sessionKey, err)
		}
	}
	// Mark the idle guard done so a concurrent AfterFunc firing in the same tick
	// as this success event can't reroute the post-loop expired() check into the
	// timeout-reply branch.
	if shouldMarkTurnDone(res) {
		idle.markDone()
	}
}

func shouldCommitUsage(ev agent.AgentEvent, res *agentAttemptResult) bool {
	return res.termErr == "" && !res.resumeBad && ev.Usage != nil
}

func shouldMarkTurnDone(res *agentAttemptResult) bool {
	return res.termErr == "" && !res.resumeBad
}

func (g *Gateway) consumeAgentError(ev agent.AgentEvent, res *agentAttemptResult) {
	// Terminal (non-recoverable) errors abort the turn. Recoverable errors are
	// informational and don't gate the reply. Stale-resume errors are swallowed
	// by consumeAgentAttempt before reaching here.
	if ev.Recoverable {
		return
	}
	res.termErr = ev.Err
	res.termTransient = ev.Transient
	res.termHint = ev.RetryHint
}

func (g *Gateway) prepareAgentRequest(ctx context.Context, sessionKey string, msg router.InboundMessage) (agent.Request, error) {
	prompt := g.buildGroupPrompt(sessionKey, msg)
	if err := g.store.AppendUser(sessionKey, msg.Text, msg.FromName, msg.CronFire); err != nil {
		return agent.Request{}, g.failTurn(sessionKey, "store.AppendUser", err)
	}
	g.notifySessionTouch(sessionKey, msg.ChannelID, msg.ChannelType)

	resumeID, err := g.store.Resume(sessionKey, g.driver.Name())
	if err != nil {
		fmt.Fprintf(os.Stderr, "[gateway] resume %s: %v\n", sessionKey, err)
	}
	cwd, memDir, err := g.resolveSandbox(sessionKey, msg)
	if err != nil {
		return agent.Request{}, g.failTurn(sessionKey, "resolve sandbox cwd", err)
	}
	if media := g.materializeAttachments(ctx, cwd, msg.Attachments); media != "" {
		prompt += media
	}
	return agent.Request{
		Prompt:       prompt,
		SessionID:    resumeID,
		Cwd:          cwd,
		MemoryDir:    memDir,
		Model:        g.model,
		SystemAppend: g.buildSystemPrompt(msg, g.rosterPrefix(msg)),
	}, nil
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
