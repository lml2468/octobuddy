package control

import (
	"time"

	"github.com/lml2468/octobuddy/core/agent"
	"github.com/lml2468/octobuddy/core/router"
	"github.com/lml2468/octobuddy/core/safety"
	"github.com/lml2468/octobuddy/core/trigger"
)

// EventSink adapts the control Server to gateway.Sink: it projects normalized
// AgentEvents onto the control-bus event vocabulary (proto/README.md) and
// broadcasts them to all connected clients. This is the join point between the
// agent-driving core and the GUI control plane. botID tags every event so a
// multi-bot GUI can attribute it to the right bot ("" in single-bot mode).
type EventSink struct {
	srv   *Server
	botID string
}

// NewEventSink wraps a Server as a gateway.Sink for the single/default bot.
func NewEventSink(srv *Server) *EventSink { return &EventSink{srv: srv} }

// NewBotEventSink wraps a Server as a gateway.Sink tagging events with botID.
func NewBotEventSink(srv *Server, botID string) *EventSink {
	return &EventSink{srv: srv, botID: botID}
}

// OnEvent projects one AgentEvent to its control-bus event.
func (s *EventSink) OnEvent(sessionKey string, ev agent.AgentEvent) {
	switch ev.Kind {
	case agent.KindSessionStarted:
		s.srv.Broadcast("session.activity", SessionActivityBody{BotID: s.botID, SessionKey: sessionKey, Kind: "turnStart"})
	case agent.KindTextDelta:
		s.srv.Broadcast("session.text", SessionTextBody{BotID: s.botID, SessionKey: sessionKey, Delta: ev.Text})
	case agent.KindThinking:
		s.srv.Broadcast("session.activity", SessionActivityBody{BotID: s.botID, SessionKey: sessionKey, Kind: "thinking"})
	case agent.KindToolUse:
		s.srv.Broadcast("session.tool", SessionToolBody{BotID: s.botID, SessionKey: sessionKey, Name: ev.ToolName, Params: ev.ToolParams})
	case agent.KindToolResult:
		s.srv.Broadcast("session.activity", SessionActivityBody{BotID: s.botID, SessionKey: sessionKey, Kind: "toolResult"})
	case agent.KindTurnDone:
		if ev.Usage != nil {
			s.srv.Broadcast("session.usage", SessionUsageBody{
				BotID: s.botID, SessionKey: sessionKey,
				InputTokens: ev.Usage.InputTokens, OutputTokens: ev.Usage.OutputTokens,
				CachedInputTokens: ev.Usage.CachedInputTokens, CostUSD: ev.Usage.CostUSD,
			})
		}
		s.srv.Broadcast("session.activity", SessionActivityBody{BotID: s.botID, SessionKey: sessionKey, Kind: "turnDone"})
	case agent.KindError:
		s.srv.Broadcast("error", ErrorBody{BotID: s.botID, Scope: "agent", Message: ev.Err, Recoverable: ev.Recoverable})
	}
}

// OnReply broadcasts the assembled assistant reply for a completed turn.
func (s *EventSink) OnReply(sessionKey string, text string) {
	s.srv.Broadcast("session.reply", SessionReplyBody{BotID: s.botID, SessionKey: sessionKey, Text: text})
}

// OnUserMessage broadcasts the inbound user message at the start of an
// accepted turn so attached GUI clients can render it in the chat
// transcript. Carries fromUid/fromName for group sessions where multiple
// humans share one session and the GUI needs to attribute messages.
//
// Source is forwarded so the renderer can distinguish a real human
// inbound ("user") from a scheduled-task trigger ("cron") — without it,
// Console cron fires would hit the CONSOLE_UID dedupe path (intended for
// Composer-typed messages with an optimistic local push) and disappear
// from the chat entirely, AND IM-rendered cron fires would look like a
// real human suddenly @-mentioned the bot with the prompt text. The
// default "user" source is elided so omitempty drops it from the wire,
// keeping the legacy on-wire shape minimal for non-cron messages.
func (s *EventSink) OnUserMessage(sessionKey string, msg router.InboundMessage) {
	srcStr := string(msg.Source)
	// Elide the human-typed sources so omitempty keeps the wire minimal and the
	// GUI's CONSOLE_UID optimistic-push dedupe (intended for Composer-typed
	// messages) still matches: a Console turn is a human Composer message, just
	// over the control bus, so it elides like a plain "user" inbound. Only a
	// non-human origin (cron) carries an explicit source for the renderer.
	if msg.Source == "" || msg.Source == trigger.SourceUser || msg.Source == trigger.SourceConsole {
		srcStr = ""
	}
	s.srv.Broadcast("session.user_message", SessionUserMessageBody{
		BotID:       s.botID,
		SessionKey:  sessionKey,
		ChannelType: int(msg.ChannelType),
		Text:        msg.Text,
		FromUID:     msg.FromUID,
		// Sanitize at the wire boundary — IM FromName arrives unsanitized
		// and would otherwise let a BiDi-override / control-char display
		// name distort the GUI sidebar and bubble label. "" fallback so
		// the GUI can drop back to FromUID when nothing survives.
		FromName: safety.SanitizeDisplayName(msg.FromName, ""),
		Ts:       time.Now().Unix(),
		Source:   srcStr,
	})
}
