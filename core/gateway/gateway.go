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
	"strings"
	"time"

	"github.com/lml2468/xclaw/core/agent"
	"github.com/lml2468/xclaw/core/router"
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
}

// New constructs a Gateway.
func New(d agent.Driver, st *store.Store, rt *router.Router, sink Sink) *Gateway {
	return &Gateway{driver: d, store: st, router: rt, sink: sink, now: time.Now}
}

// Handle routes one inbound message through the full pipeline. The router holds
// the per-session lock across the whole turn, so same-session turns serialize.
// Returns the router decision (so callers can log drops/limits).
func (g *Gateway) Handle(ctx context.Context, msg router.InboundMessage) (router.Decision, error) {
	return g.router.RouteAndHandle(ctx, msg, g.runTurn)
}

// runTurn executes one accepted turn under the session lock.
func (g *Gateway) runTurn(ctx context.Context, sessionKey string, msg router.InboundMessage) error {
	// Ensure the session row exists (refreshes TTL).
	if _, err := g.store.GetOrCreate(sessionKey, msg.ChannelID, int(msg.ChannelType)); err != nil {
		return err
	}
	// Persist the user message.
	if err := g.store.AppendUser(sessionKey, msg.Text, msg.FromName); err != nil {
		return err
	}

	// Resume the agent's prior session if we have one.
	resumeID, _ := g.store.Resume(sessionKey)

	events, err := g.driver.Query(ctx, agent.Request{
		Prompt:    msg.Text,
		SessionID: resumeID,
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

	// Persist resume id for continuity (only if the agent reported one).
	if newResume != "" {
		_ = g.store.SaveResume(sessionKey, g.driver.Name(), newResume)
	}

	text := reply.String()
	if err := g.store.AppendAssistant(sessionKey, text, g.driver.Name()); err != nil {
		return err
	}
	g.sink.OnReply(sessionKey, text)
	return nil
}
