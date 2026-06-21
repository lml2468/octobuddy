package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/lml2468/xclaw/core/control"
	"github.com/lml2468/xclaw/core/cron"
	"github.com/lml2468/xclaw/core/gateway"
	"github.com/lml2468/xclaw/core/router"
	"github.com/lml2468/xclaw/core/store"
)

// botTarget is the per-bot state a control command operates on. It abstracts
// over single-bot mode (one fixed target) and multi-bot mode (resolved by id),
// so the command handler below is written once for both.
type botTarget struct {
	id      string // resolved bot id (echoed in responses so the client can route)
	gateway *gateway.Gateway
	store   *store.Store
	secrets *secretStore
	cron    *cron.Manager // nil when agent.cron is disabled for this bot
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
			// Never log b.Value, and never bubble the underlying secret-store
			// error verbatim to the control bus — a future keyring lib that
			// includes the value in its error message would otherwise leak
			// the secret to the connected GUI. Log the real cause server-side
			// only; return a neutral error to the caller.
			if b.Clear {
				if err := t.secrets.Clear(b.Kind); err != nil {
					log.Printf("[secret] clear %s/%s: %v", b.BotID, b.Kind, err)
					return nil, fmt.Errorf("secret.inject clear failed for %s/%s", b.BotID, b.Kind)
				}
			} else if err := t.secrets.Set(b.Kind, b.Value); err != nil {
				log.Printf("[secret] set %s/%s: %v", b.BotID, b.Kind, err)
				return nil, fmt.Errorf("secret.inject set failed for %s/%s", b.BotID, b.Kind)
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
			// Echo botId + key so the client routes the rows to the right session
			// even if the user switched sessions while this fetch was in flight.
			return control.HistoryResponse{
				BotID:    b.BotID,
				Key:      b.SessionKey,
				Messages: historyFromMessages(msgs),
			}, nil

		case "sessions.list":
			var b control.SessionsListBody
			if err := json.Unmarshal(body, &b); err != nil {
				return nil, err
			}
			t, err := deps.resolve(b.BotID)
			if err != nil {
				return nil, err
			}
			sums, err := t.store.ListSessions()
			if err != nil {
				return nil, err
			}
			// Echo botId so the client never folds these rows into the wrong bot.
			return control.SessionsListResponse{
				BotID:    b.BotID,
				Sessions: summariesFromSessions(sums),
			}, nil

		case "usage.stats":
			var b control.UsageStatsBody
			if err := json.Unmarshal(body, &b); err != nil {
				return nil, err
			}
			t, err := deps.resolve(b.BotID)
			if err != nil {
				return nil, err
			}
			// Since == 0 means all time; otherwise sum day buckets at or after it.
			var u store.TokenUsage
			if b.Since > 0 {
				u, err = t.store.UsageSince(b.Since)
			} else {
				u, err = t.store.Usage()
			}
			if err != nil {
				return nil, err
			}
			return control.UsageStats{
				BotID:            b.BotID,
				Since:            b.Since,
				InputTokens:      u.InputTokens,
				OutputTokens:     u.OutputTokens,
				CachedTokens:     u.CachedTokens,
				CacheWriteTokens: u.CacheWriteTokens,
				CostUSD:          u.CostUSD,
				Turns:            u.Turns,
			}, nil

		case "session.reset":
			var b control.SessionSendBody // reuse {uid}
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
			// Resume state is keyed by the router-derived sessionKey, not the raw
			// uid — derive it the same way session.send does so reset actually
			// clears the right entry (a control-bus DM has no space, so this is
			// the uid today, but stays correct if a space is ever introduced).
			key, err := router.InboundMessage{ChannelType: router.ChannelDM, FromUID: b.UID}.SessionKey()
			if err != nil {
				return nil, err
			}
			_ = t.store.ClearResume(key)
			return control.OKBody{OK: true}, nil

		case "cron.create":
			var b control.CronCreateBody
			if err := json.Unmarshal(body, &b); err != nil {
				return nil, err
			}
			t, err := deps.resolve(b.BotID)
			if err != nil {
				return nil, err
			}
			if t.cron == nil {
				return nil, fmt.Errorf("cron is not enabled for this bot")
			}
			// The requester identity is the SERVER-resolved owner uid, never the
			// body uid. cron reaches the bus via an agent-controlled CLI, so a
			// body uid is a forgeable claim (a prompt-injected agent could assert
			// the owner's uid); the resolved owner is trusted server state. An
			// unregistered bot has no owner yet → reject.
			owner := t.cron.OwnerUID()
			if owner == "" {
				return nil, fmt.Errorf("bot owner not resolved yet; cannot create scheduled tasks")
			}
			// DM-bound tasks (no channelId) bind to the owner's own session; group
			// tasks bind to the named channel but still run as the owner.
			coords := cron.SessionCoords{
				ChannelID:   b.ChannelID,
				ChannelType: cron.ChannelKind(channelTypeFor(b.ChannelType, b.ChannelID)),
				FromUID:     owner,
				FromName:    b.FromName,
			}
			task, err := t.cron.Create(cron.CreateParams{
				Schedule:   b.Schedule,
				Prompt:     b.Prompt,
				Recurring:  b.Recurring,
				Coords:     coords,
				RequestUID: owner,
			})
			if err != nil {
				return nil, err
			}
			return cronTaskInfo(task), nil

		case "cron.list":
			var b control.CronListBody
			if err := json.Unmarshal(body, &b); err != nil {
				return nil, err
			}
			t, err := deps.resolve(b.BotID)
			if err != nil {
				return nil, err
			}
			if t.cron == nil {
				return nil, fmt.Errorf("cron is not enabled for this bot")
			}
			tasks, err := t.cron.List()
			if err != nil {
				return nil, err
			}
			out := make([]control.CronTaskInfo, 0, len(tasks))
			for _, task := range tasks {
				out = append(out, cronTaskInfo(task))
			}
			return out, nil

		case "cron.delete":
			var b control.CronDeleteBody
			if err := json.Unmarshal(body, &b); err != nil {
				return nil, err
			}
			t, err := deps.resolve(b.BotID)
			if err != nil {
				return nil, err
			}
			if t.cron == nil {
				return nil, fmt.Errorf("cron is not enabled for this bot")
			}
			// Gate on the verified server-resolved owner, not the body uid.
			owner := t.cron.OwnerUID()
			if owner == "" {
				return nil, fmt.Errorf("bot owner not resolved yet; cannot delete scheduled tasks")
			}
			if err := t.cron.Delete(b.ID, owner); err != nil {
				return nil, err
			}
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

// summariesFromSessions projects store session summaries onto the wire type.
func summariesFromSessions(sums []store.SessionSummary) []control.SessionSummary {
	out := make([]control.SessionSummary, 0, len(sums))
	for _, s := range sums {
		out = append(out, control.SessionSummary{
			Key:         s.Key,
			ChannelType: s.ChannelType,
			UpdatedAt:   s.UpdatedAt,
			Preview:     s.Preview,
			LastRole:    string(s.LastRole),
		})
	}
	return out
}

// channelTypeFor resolves the router/octo channel type for a cron task: an
// explicit non-zero type wins; otherwise a present channelId implies a group and
// its absence a DM. Mirrors the create-time coords binding in cron-tool.ts.
func channelTypeFor(explicit int, channelID string) int {
	if explicit == int(router.ChannelDM) || explicit == int(router.ChannelGroup) {
		return explicit
	}
	if channelID != "" {
		return int(router.ChannelGroup)
	}
	return int(router.ChannelDM)
}

// cronTaskInfo projects a stored cron task onto the wire type (nextRun rendered
// as RFC3339, mirroring cron-tool.ts summarize()).
func cronTaskInfo(t cron.Task) control.CronTaskInfo {
	next := ""
	if t.NextRun != 0 {
		next = time.UnixMilli(t.NextRun).UTC().Format(time.RFC3339)
	}
	return control.CronTaskInfo{
		ID:        t.ID,
		Schedule:  t.Schedule,
		Recurring: t.Recurring,
		Prompt:    t.Prompt,
		NextRun:   next,
		Enabled:   t.Enabled,
	}
}
