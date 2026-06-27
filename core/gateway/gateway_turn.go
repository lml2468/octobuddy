package gateway

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/lml2468/octobuddy/core/agent"
	"github.com/lml2468/octobuddy/core/router"
	"github.com/lml2468/octobuddy/core/trigger"
)

// runTurn executes one accepted turn under the session lock.
func (g *Gateway) runTurn(ctx context.Context, sessionKey string, msg router.InboundMessage) error {
	if err := g.startTurn(sessionKey, msg); err != nil {
		return ignoreConcluded(err)
	}

	turnDelivered := false
	defer g.rewindGroupCursorUnlessDelivered(msg, &turnDelivered)()
	req, err := g.prepareAgentRequest(ctx, sessionKey, msg)
	if err != nil {
		return ignoreConcluded(err)
	}

	turnCtx, idle := newIdleGuard(ctx, g.dispatchTimeout)
	defer idle.stop()

	var attemptResult agentAttemptResult
	resume := req.SessionID
	for attempt := 0; ; attempt++ {
		req.SessionID = resume
		events, err := g.driver.Query(turnCtx, req)
		if err != nil {
			return ignoreConcluded(g.failTurn(sessionKey, "driver.Query", err))
		}

		attemptResult = g.consumeAgentAttempt(sessionKey, events, idle, resume != "")

		if shouldRetryFreshResume(attemptResult, resume, attempt) {
			glog().Warn("stale resume id; clearing and retrying fresh", "session", sessionKey)
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

	g.completeSuccessfulTurn(sessionKey, msg, attemptResult)
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
	// steps accumulates this turn's process steps (tool calls / thinking) in
	// order, mirroring the live session.tool / session.activity stream the
	// desktop renders. Persisted as JSON with the assistant row so a reload
	// re-renders the step card. Only filled on the surviving attempt (a
	// stale-resume retry discards its events before reaching consumeAgentEvent).
	steps []turnStep
}

// turnStep is one persisted process step. Kind is "tool" or "thinking"; Text is
// the same display string the desktop builds live (e.g. "Read(README.md)" or
// "thinking…"), so a reloaded card reads identically to the live one. The JSON
// tags are the shape the desktop's parseSteps expects.
type turnStep struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
	// Detail is the raw Name(params) shown when a tool step is expanded in the
	// desktop card. Empty (omitted) for thinking steps and for tool calls whose
	// summary already IS the Name(params) — those render non-expandable.
	Detail string `json:"detail,omitempty"`
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
	case agent.KindToolUse:
		// Persist the readable summary as the step text + the raw Name(params)
		// as expandable detail (computed once in claude_parse, same values the
		// live session.tool carries). Detail is elided when it equals the
		// summary (no description) so the card renders it non-expandable.
		step := turnStep{Kind: "tool", Text: ev.ToolSummary}
		if ev.ToolDetail != ev.ToolSummary {
			step.Detail = ev.ToolDetail
		}
		res.steps = append(res.steps, step)
	case agent.KindThinking:
		// Coalesce consecutive thinking markers into one step, mirroring the
		// desktop fold(). The literal "thinking…" matches the FE's live label.
		if n := len(res.steps); n == 0 || res.steps[n-1].Kind != "thinking" {
			res.steps = append(res.steps, turnStep{Kind: "thinking", Text: "thinking…"})
		}
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
			glog().Error("add usage", "session", sessionKey, "err", err)
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
	// Persist Console turns as a plain human message: the store/history vocabulary
	// is user/cron/assistant, and a Console turn IS a human message for history.
	// Its console-ness is a live-trigger concern (the bootstrap gate reads
	// msg.Source directly below), not a stored distinction.
	storedSource := string(msg.Source)
	// Console turns persist as a plain human message (above) AND must drop the
	// synthetic Console uid ("gui-user"): persisting it would make the desktop
	// Bubble's showSenderLabel truthy and slap a "gui-user"/"You" label on an
	// operator-typed message that should stay unlabeled. IM turns keep their
	// real FromUID so the bubble can re-resolve / converge the display name.
	fromUID := msg.FromUID
	if msg.Source == trigger.SourceConsole {
		storedSource = string(trigger.SourceUser)
		fromUID = ""
	}
	if err := g.store.AppendUser(sessionKey, msg.Text, msg.FromName, fromUID, storedSource); err != nil {
		return agent.Request{}, g.failTurn(sessionKey, "store.AppendUser", err)
	}
	g.notifySessionTouch(sessionKey, msg.ChannelID, msg.ChannelType)

	resumeID, err := g.store.Resume(sessionKey, g.driver.Name())
	if err != nil {
		glog().Error("resume", "session", sessionKey, "err", err)
	}
	cwd, memDir, err := g.resolveSandbox(sessionKey, msg)
	if err != nil {
		return agent.Request{}, g.failTurn(sessionKey, "resolve sandbox cwd", err)
	}
	if media := g.materializeAttachments(ctx, cwd, msg.Attachments); media != "" {
		prompt += media
	}
	req := agent.Request{
		Prompt:         prompt,
		SessionID:      resumeID,
		Cwd:            cwd,
		MemoryDir:      memDir,
		Model:          g.model,
		SystemPrompt:   g.buildSystemPrompt(msg, g.rosterPrefix(msg)),
		SettingSources: g.settingSources,
	}
	// Per-channel/bot tool surface. Unconfigured sessions — the common case,
	// including the desktop Console (which is NOT special-cased; it resolves
	// by sessionKey like any DM) — leave AllowedTools nil so the driver uses
	// its probed headless-safe default.
	if tools, ok := g.resolveTools(sessionKey); ok {
		req.AllowedTools = tools
	}
	return req, nil
}

func (g *Gateway) handleDispatchTimeout(ctx context.Context, idle *idleGuard, sessionKey string, delivered *bool) bool {
	if !idle.expired(ctx) {
		return false
	}
	glog().Warn("dispatch idle timeout", "session", sessionKey, "timeout", g.dispatchTimeout)
	*delivered = true
	g.sink.OnReply(sessionKey, timeoutReply)
	return true
}

func (g *Gateway) handleTerminalAgentError(sessionKey, termErr string, transient bool, hint string, delivered *bool) bool {
	if termErr == "" {
		return false
	}
	if transient {
		glog().Warn("transient upstream error", "session", sessionKey, "err", termErr)
		reply := busyReply
		if hint != "" {
			reply = busyReply + "（" + hint + " 后恢复）"
		}
		*delivered = true
		g.sink.OnReply(sessionKey, reply)
		return true
	}
	glog().Error("terminal agent error", "session", sessionKey, "err", termErr)
	// Must mirror the transient branch's *delivered=true so the deferred
	// rewindGroupCursorUnlessDelivered doesn't rewind the group cursor —
	// otherwise the user's message resurfaces in [Recent group messages]
	// on the next turn.
	*delivered = true
	g.sink.OnReply(sessionKey, errorReply)
	return true
}

func (g *Gateway) completeSuccessfulTurn(sessionKey string, msg router.InboundMessage, res agentAttemptResult) {
	text := res.reply
	if res.newResume != "" {
		if err := g.store.SaveResume(sessionKey, g.driver.Name(), res.newResume); err != nil {
			glog().Error("save resume", "session", sessionKey, "err", err)
		}
	}
	if err := g.store.AppendAssistant(sessionKey, text, g.driver.Name(), marshalSteps(res.steps)); err != nil {
		glog().Error("append assistant", "session", sessionKey, "err", err)
	}
	g.notifySessionTouch(sessionKey, msg.ChannelID, msg.ChannelType)
	g.sink.OnReply(sessionKey, text)
	if g.groups != nil && msg.ChannelType == router.ChannelGroup && strings.TrimSpace(text) != "" {
		if err := g.store.SaveBotReplySeq(sessionKey, msg.MessageSeq); err != nil {
			glog().Error("save reply seq", "session", sessionKey, "err", err)
		}
	}
}

// marshalSteps serializes a turn's process steps to the JSON the desktop's
// parseSteps expects, or "" when there were none. Best-effort: a marshal error
// degrades to "" (no card) rather than failing the turn — matches the
// append-assistant error handling above.
func marshalSteps(steps []turnStep) string {
	if len(steps) == 0 {
		return ""
	}
	b, err := json.Marshal(steps)
	if err != nil {
		glog().Error("marshal steps", "err", err)
		return ""
	}
	return string(b)
}
