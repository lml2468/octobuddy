package gateway

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/lml2468/octobuddy/core/agent"
	"github.com/lml2468/octobuddy/core/router"
)

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
