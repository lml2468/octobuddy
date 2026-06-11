package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/lml2468/xclaw/core/control"
	"github.com/lml2468/xclaw/core/gateway"
	"github.com/lml2468/xclaw/core/router"
	"github.com/lml2468/xclaw/core/store"
)

// botTarget is the per-bot state a control command operates on. It abstracts
// over single-bot mode (one fixed target) and multi-bot mode (resolved by id),
// so the command handler below is written once for both.
type botTarget struct {
	gateway *gateway.Gateway
	store   *store.Store
	secrets *secretStore
}

// handlerDeps adapts single-bot vs multi-bot wiring to one command dispatcher.
type handlerDeps struct {
	started  time.Time
	driver   string                   // health: driver name (empty in multi-bot)
	botCount func() int               // health: number of bots
	list     func() []control.BotInfo // bots.list
	resolve  func(botID string) (*botTarget, error)
	// broadcast surfaces async turn outcomes (errors / drops) to clients so a
	// fire-and-forget session.send never leaves the GUI waiting forever. May be
	// nil when no control server is attached.
	broadcast func(eventType string, body any)
}

// makeHandler builds the control-bus command dispatcher shared by single-bot and
// multi-bot modes. session.send runs the turn in a goroutine tied to ctx (so
// daemon shutdown cancels the in-flight agent subprocess) and reports a
// non-accept decision or error back as an event.
func makeHandler(ctx context.Context, deps handlerDeps) control.CommandHandler {
	return func(cmdType string, body json.RawMessage) (any, error) {
		switch cmdType {
		case "health":
			return control.HealthBody{
				Uptime: int64(time.Since(deps.started).Seconds()),
				Driver: deps.driver,
				Bots:   deps.botCount(),
			}, nil

		case "bots.list":
			return deps.list(), nil

		case "secret.inject":
			var b control.SecretInjectBody
			if err := json.Unmarshal(body, &b); err != nil {
				return nil, err
			}
			t, err := deps.resolve(b.BotID)
			if err != nil {
				return nil, err
			}
			// Never log b.Value.
			if err := t.secrets.Set(b.Kind, b.Value); err != nil {
				return nil, err
			}
			return control.OKBody{OK: true}, nil

		case "session.send":
			var b control.SessionSendBody
			if err := json.Unmarshal(body, &b); err != nil {
				return nil, err
			}
			if b.UID == "" {
				return nil, fmt.Errorf("uid required")
			}
			t, err := deps.resolve(b.BotID)
			if err != nil {
				return nil, err
			}
			go func() {
				d, herr := t.gateway.Handle(ctx, router.InboundMessage{
					ChannelType: router.ChannelDM, FromUID: b.UID, FromName: b.UID, Text: b.Text,
				})
				if deps.broadcast == nil {
					return
				}
				switch {
				case herr != nil:
					deps.broadcast("error", control.ErrorBody{
						BotID: b.BotID, Scope: "turn", Message: herr.Error()})
				case d != router.Accepted:
					deps.broadcast("error", control.ErrorBody{
						BotID: b.BotID, Scope: "turn",
						Message: "message dropped: " + d.String(), Recoverable: true})
				}
			}()
			return control.OKBody{OK: true}, nil

		case "session.history":
			var b control.SessionHistoryBody
			if err := json.Unmarshal(body, &b); err != nil {
				return nil, err
			}
			t, err := deps.resolve(b.BotID)
			if err != nil {
				return nil, err
			}
			limit := b.Limit
			if limit <= 0 {
				limit = 40
			}
			msgs, err := t.store.RecentMessages(b.SessionKey, limit)
			if err != nil {
				return nil, err
			}
			return historyFromMessages(msgs), nil

		case "session.reset":
			var b control.SessionSendBody // reuse {uid}
			if err := json.Unmarshal(body, &b); err != nil {
				return nil, err
			}
			t, err := deps.resolve(b.BotID)
			if err != nil {
				return nil, err
			}
			_ = t.store.ClearResume(b.UID)
			return control.OKBody{OK: true}, nil

		default:
			return nil, fmt.Errorf("unknown command %q", cmdType)
		}
	}
}

// historyFromMessages projects store messages onto the wire history type.
func historyFromMessages(msgs []store.Message) []control.HistoryMessage {
	out := make([]control.HistoryMessage, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, control.HistoryMessage{Role: string(m.Role), Content: m.Content, TS: m.Timestamp})
	}
	return out
}
