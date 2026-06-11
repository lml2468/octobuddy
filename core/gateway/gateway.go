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
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lml2468/xclaw/core/agent"
	"github.com/lml2468/xclaw/core/groupctx"
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
	now    func() time.Time

	// Optional group-context (set via WithGroupContext). When set, group
	// messages get a [Recent group messages] delta injected into the prompt.
	groups *groupctx.GroupContext
	// Operator-trusted system prompt (assembled from SOUL.md + AGENTS.md).
	// Appended after the non-overridable security prefix.
	systemPrompt string
	// Optional model override passed to the driver (empty = driver default).
	model string
	// Per-session sandbox roots (set via WithSandbox). When cwdBase is set, each
	// turn runs in cwdBase/<hash>, with auto-memory under memoryBase/<hash> and
	// operator skills symlinked in. Empty cwdBase = no isolation (inherit proc).
	cwdBase, memoryBase, skillsDir, globalSkillsDir string
}

// New constructs a Gateway.
func New(d agent.Driver, st *store.Store, rt *router.Router, sink Sink) *Gateway {
	return &Gateway{driver: d, store: st, router: rt, sink: sink, now: time.Now}
}

// WithGroupContext enables group-context injection.
func (g *Gateway) WithGroupContext(gc *groupctx.GroupContext) *Gateway {
	g.groups = gc
	return g
}

// WithSystemPrompt sets the operator-trusted system prompt (SOUL.md + AGENTS.md).
func (g *Gateway) WithSystemPrompt(p string) *Gateway {
	g.systemPrompt = p
	return g
}

// WithModel sets the model override passed to the driver on each turn.
func (g *Gateway) WithModel(m string) *Gateway {
	g.model = m
	return g
}

// WithSandbox enables per-session filesystem isolation: each turn runs in a
// hashed subdir of cwdBase, with auto-memory consolidated under memoryBase and the
// operator's skills (globalSkillsDir then skillsDir, latter shadows) symlinked
// into the sandbox. An empty cwdBase disables isolation.
func (g *Gateway) WithSandbox(cwdBase, memoryBase, skillsDir, globalSkillsDir string) *Gateway {
	g.cwdBase = cwdBase
	g.memoryBase = memoryBase
	g.skillsDir = skillsDir
	g.globalSkillsDir = globalSkillsDir
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
func (g *Gateway) Handle(ctx context.Context, msg router.InboundMessage) (router.Decision, error) {
	return g.router.RouteAndHandle(ctx, msg, g.runTurn)
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
	g.groups.Push(msg.ChannelID, msg.FromUID, msg.FromName, msg.Text)
}

// runTurn executes one accepted turn under the session lock.
func (g *Gateway) runTurn(ctx context.Context, sessionKey string, msg router.InboundMessage) error {
	// Ensure the session row exists (refreshes TTL).
	if _, err := g.store.GetOrCreate(sessionKey, msg.ChannelID, int(msg.ChannelType)); err != nil {
		return err
	}

	// Build the prompt. For group messages, inject the [Recent group messages]
	// delta as UNTRUSTED background and demarcate the real request with the
	// current-message anchor. CRITICAL ordering (group-context.ts): build the
	// delta BEFORE caching the current message, so it isn't echoed into itself.
	prompt := msg.Text
	if g.groups != nil && msg.ChannelType == router.ChannelGroup && msg.ChannelID != "" {
		cursor := g.groups.Cursor(msg.ChannelID)
		deltaText, _ := g.groups.BuildContextSince(msg.ChannelID, cursor)
		// Cache the current message AFTER reading the delta.
		g.groups.Push(msg.ChannelID, msg.FromUID, msg.FromName, msg.Text)
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
		b.WriteString(msg.Text)
		prompt = b.String()
	}

	// Persist the (original) user message.
	if err := g.store.AppendUser(sessionKey, msg.Text, msg.FromName); err != nil {
		return err
	}

	// Resume the agent's prior session if we have one. A real read error (not
	// "no row") degrades the turn to a fresh session — acceptable, but log it so
	// silent loss of conversation continuity is diagnosable.
	resumeID, err := g.store.Resume(sessionKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[gateway] resume %s: %v\n", sessionKey, err)
	}

	// Resolve the per-session sandbox (cwd + memory + skills) when enabled.
	var cwd, memDir string
	if g.cwdBase != "" {
		sctx := sandbox.SessionCtx{Kind: kindFor(msg.ChannelType), SessionKey: sessionKey}
		resolved, err := sandbox.ResolveSessionCwd(g.cwdBase, sctx)
		if err != nil {
			// Building the sandbox failed — running in the process cwd would leak
			// across sessions, which is exactly what this guards against. Fail loud.
			return fmt.Errorf("resolve sandbox cwd: %w", err)
		}
		cwd = resolved
		// Best-effort: a missing skill only degrades capability, never breaks the turn.
		_ = sandbox.LinkSkillsIntoSandbox(cwd, []string{g.globalSkillsDir, g.skillsDir})
		if g.memoryBase != "" {
			memDir = sandbox.ResolveMemoryDir(g.memoryBase, sctx)
		}
	}

	events, err := g.driver.Query(ctx, agent.Request{
		Prompt:       prompt,
		SessionID:    resumeID,
		Cwd:          cwd,
		MemoryDir:    memDir,
		Model:        g.model,
		SystemAppend: g.buildSystemPrompt(),
	})
	if err != nil {
		return err
	}

	var reply strings.Builder
	var newResume string
	for ev := range events {
		g.sink.OnEvent(sessionKey, ev)
		switch ev.Kind {
		case agent.KindSessionStarted:
			if ev.SessionID != "" {
				newResume = ev.SessionID
			}
		case agent.KindTextDelta:
			reply.WriteString(ev.Text)
		}
	}

	// Persist resume id for continuity (only if the agent reported one). A write
	// failure here silently breaks continuity on the next turn, so log it.
	if newResume != "" {
		if err := g.store.SaveResume(sessionKey, g.driver.Name(), newResume); err != nil {
			fmt.Fprintf(os.Stderr, "[gateway] save resume %s: %v\n", sessionKey, err)
		}
	}

	text := reply.String()
	if err := g.store.AppendAssistant(sessionKey, text, g.driver.Name()); err != nil {
		return err
	}
	g.sink.OnReply(sessionKey, text)
	return nil
}

// buildSystemPrompt assembles the frozen system-prompt append: the
// non-overridable security prefix followed by the operator-trusted SOUL/config
// prompt. (The driver's preset base prompt is prepended by the agent CLI.)
func (g *Gateway) buildSystemPrompt() string {
	parts := []safety.SafeText{safety.TrustedText(safety.SecurityPrefix)}
	if g.systemPrompt != "" {
		parts = append(parts, safety.TrustedText(g.systemPrompt))
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
