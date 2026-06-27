package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/lml2468/octobuddy/core/clog"
	"github.com/lml2468/octobuddy/core/control"
	"github.com/lml2468/octobuddy/core/cron"
	"github.com/lml2468/octobuddy/core/gateway"
	"github.com/lml2468/octobuddy/core/im/octo"
	"github.com/lml2468/octobuddy/core/router"
	"github.com/lml2468/octobuddy/core/safety"
	"github.com/lml2468/octobuddy/core/store"
	"github.com/lml2468/octobuddy/core/trigger"
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
	// connector is the IM-edge handle used by handlers that need name
	// resolution (sessions.list → ChannelName). May be nil in tests and in
	// the REPL single-bot path where no Octo connector is wired.
	connector *octo.Connector

	// mcpCheck probes this bot's saved .mcp.json and returns each server's
	// health. Wired at assembly (it needs the bot's resolved bin + env +
	// config dir). nil when the bot has no isolated CLAUDE_CONFIG_DIR
	// (inheritUserConfig) or in test/REPL wiring; the handler then reports
	// "not configured".
	mcpCheck func(ctx context.Context) (control.MCPCheckResponse, error)

	// turnsWG tracks every in-flight session.send goroutine so the daemon
	// can wait for them before closing the store. The Octo connector tracks
	// its own queue via Connector.WaitTurns; this is the symmetric guard for
	// turns initiated over the control bus (Console GUI). Without it,
	// SIGTERM races the goroutine into the store-close path.
	turnsWG sync.WaitGroup
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
	d := controlCommandDispatcher{ctx: ctx, deps: deps}
	handlers := map[string]func(json.RawMessage) (any, error){
		"health":          func(json.RawMessage) (any, error) { return d.health(), nil },
		"bots.list":       func(json.RawMessage) (any, error) { return d.deps.list(), nil },
		"secret.inject":   d.secretInject,
		"session.send":    d.sessionSend,
		"session.history": d.sessionHistory,
		"sessions.list":   d.sessionsList,
		"usage.stats":     d.usageStats,
		"session.reset":   d.sessionReset,
		"cron.create":     d.cronCreate,
		"cron.list":       d.cronList,
		"cron.delete":     d.cronDelete,
		"cron.update":     d.cronUpdate,
		"mcp.check":       d.mcpCheck,
	}
	return func(cmdType string, body json.RawMessage) (any, error) {
		handler, ok := handlers[cmdType]
		if !ok {
			return nil, fmt.Errorf("unknown command %q", cmdType)
		}
		return handler(body)
	}
}

type controlCommandDispatcher struct {
	ctx  context.Context
	deps handlerDeps
}

func decodeControlBody[T any](body json.RawMessage) (T, error) {
	var b T
	return b, json.Unmarshal(body, &b)
}

func (d controlCommandDispatcher) health() control.HealthBody {
	return control.HealthBody{
		Uptime: int64(time.Since(d.deps.started).Seconds()),
		Driver: d.deps.driver,
		Bots:   d.deps.botCount(),
	}
}

func (d controlCommandDispatcher) secretInject(body json.RawMessage) (any, error) {
	b, err := decodeControlBody[control.SecretInjectBody](body)
	if err != nil {
		return nil, err
	}
	t, err := d.deps.resolve(b.BotID)
	if err != nil {
		return nil, err
	}
	if b.Clear {
		if err := t.secrets.Clear(b.Kind); err != nil {
			clog.For("secret").Warn("clear failed", "bot", b.BotID, "kind", b.Kind, "err", err)
			return nil, fmt.Errorf("secret.inject clear failed for %s/%s", b.BotID, b.Kind)
		}
	} else if err := t.secrets.Set(b.Kind, b.Value); err != nil {
		clog.For("secret").Warn("set failed", "bot", b.BotID, "kind", b.Kind, "err", err)
		return nil, fmt.Errorf("secret.inject set failed for %s/%s", b.BotID, b.Kind)
	}
	return control.OKBody{OK: true}, nil
}

func (d controlCommandDispatcher) sessionSend(body json.RawMessage) (any, error) {
	b, err := decodeControlBody[control.SessionSendBody](body)
	if err != nil {
		return nil, err
	}
	if b.UID == "" {
		return nil, fmt.Errorf("uid required")
	}
	t, err := d.deps.resolve(b.BotID)
	if err != nil {
		return nil, err
	}
	text, err := composerText(t.gateway, b)
	if err != nil {
		return nil, err
	}
	t.turnsWG.Add(1)
	go d.runControlTurn(t, b, text)
	return control.OKBody{OK: true}, nil
}

func composerText(gw *gateway.Gateway, b control.SessionSendBody) (string, error) {
	text := b.Text
	if len(b.Attachments) == 0 {
		return text, nil
	}
	extra, err := materializeComposerAttachments(gw, b.UID, b.Attachments)
	if err != nil {
		return "", err
	}
	if extra == "" {
		return text, nil
	}
	if text != "" {
		text += "\n\n"
	}
	return text + extra, nil
}

func (d controlCommandDispatcher) runControlTurn(t *botTarget, b control.SessionSendBody, text string) {
	defer t.turnsWG.Done()
	decision, err := t.gateway.Handle(d.ctx, router.InboundMessage{
		ChannelType: router.ChannelDM, FromUID: b.UID, FromName: b.UID, Text: text,
		Source: trigger.SourceConsole,
	})
	if d.deps.broadcast == nil {
		return
	}
	if err != nil {
		d.deps.broadcast("error", control.ErrorBody{BotID: b.BotID, Scope: "turn", Message: err.Error()})
		return
	}
	if decision != router.Accepted {
		d.deps.broadcast("error", control.ErrorBody{
			BotID: b.BotID, Scope: "turn",
			Message: "message dropped: " + decision.String(), Recoverable: true,
		})
	}
}

func (d controlCommandDispatcher) sessionHistory(body json.RawMessage) (any, error) {
	b, err := decodeControlBody[control.SessionHistoryBody](body)
	if err != nil {
		return nil, err
	}
	t, err := d.deps.resolve(b.BotID)
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
	return control.HistoryResponse{
		BotID: b.BotID, Key: b.SessionKey, Messages: historyFromMessages(msgs),
	}, nil
}

func (d controlCommandDispatcher) sessionsList(body json.RawMessage) (any, error) {
	b, err := decodeControlBody[control.SessionsListBody](body)
	if err != nil {
		return nil, err
	}
	t, err := d.deps.resolve(b.BotID)
	if err != nil {
		return nil, err
	}
	sums, err := t.store.ListSessions()
	if err != nil {
		return nil, err
	}
	if t.connector != nil {
		prewarmNamesForSessions(t.connector, sums, 1500*time.Millisecond)
	}
	return control.SessionsListResponse{
		BotID: b.BotID, Sessions: summariesFromSessions(sums, t.connector),
	}, nil
}

func (d controlCommandDispatcher) usageStats(body json.RawMessage) (any, error) {
	b, err := decodeControlBody[control.UsageStatsBody](body)
	if err != nil {
		return nil, err
	}
	t, err := d.deps.resolve(b.BotID)
	if err != nil {
		return nil, err
	}
	u, err := usageForRequest(t.store, b.Since)
	if err != nil {
		return nil, err
	}
	return control.UsageStats{
		BotID: b.BotID, Since: b.Since, InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
		CachedTokens: u.CachedTokens, CacheWriteTokens: u.CacheWriteTokens, CostUSD: u.CostUSD, Turns: u.Turns,
	}, nil
}

func usageForRequest(st *store.Store, since int64) (store.TokenUsage, error) {
	if since > 0 {
		return st.UsageSince(since)
	}
	return st.Usage()
}

// mcpCheck probes the addressed bot's saved .mcp.json and reports each MCP
// server's health (the desktop's "test connection"). The probe is wired at
// bot assembly (it needs the bot's resolved bin + env + config dir); a nil
// hook means MCP isn't applicable to this bot (no isolated config dir, or
// test/REPL wiring) — reported as "not configured", not an error.
func (d controlCommandDispatcher) mcpCheck(body json.RawMessage) (any, error) {
	b, err := decodeControlBody[control.MCPCheckBody](body)
	if err != nil {
		return nil, err
	}
	t, err := d.deps.resolve(b.BotID)
	if err != nil {
		return nil, err
	}
	if t.mcpCheck == nil {
		return control.MCPCheckResponse{BotID: b.BotID, Configured: false}, nil
	}
	res, err := t.mcpCheck(d.ctx)
	if err != nil {
		return nil, err
	}
	res.BotID = b.BotID
	return res, nil
}

func (d controlCommandDispatcher) sessionReset(body json.RawMessage) (any, error) {
	b, err := decodeControlBody[control.SessionSendBody](body)
	if err != nil {
		return nil, err
	}
	if b.UID == "" {
		return nil, fmt.Errorf("uid required")
	}
	t, err := d.deps.resolve(b.BotID)
	if err != nil {
		return nil, err
	}
	key, err := (router.InboundMessage{ChannelType: router.ChannelDM, FromUID: b.UID}).SessionKey()
	if err != nil {
		return nil, err
	}
	_ = t.store.ClearResume(key)
	return control.OKBody{OK: true}, nil
}

// historyFromMessages projects store messages onto the wire history type.
// FromName is forwarded only for user-role rows so a multi-author group
// session can attribute persisted bubbles to the right speaker (assistant
// rows also carry from_name in the store — that's the bot's own name and
// has no UI use, plus assistant bubbles never read the field). Sanitized
// at this wire boundary because IM-side FromName landed in the store
// unprocessed; a name with BiDi overrides or control chars would otherwise
// distort the rendered sender label.
func historyFromMessages(msgs []store.Message) []control.HistoryMessage {
	out := make([]control.HistoryMessage, 0, len(msgs))
	for _, m := range msgs {
		// Elide the default "user" source so wire stays minimal for
		// non-cron messages (matches OnUserMessage's elision).
		src := m.Source
		if src == store.SourceUser {
			src = ""
		}
		row := control.HistoryMessage{Role: string(m.Role), Content: m.Content, TS: m.Timestamp, Source: src}
		if m.Role == store.RoleUser {
			row.FromName = safety.SanitizeDisplayName(m.FromName, "")
			// Forward the durable uid so the desktop can re-resolve the live
			// name through its converging name map — the stored from_name may
			// have been empty (cache miss) at append time.
			row.FromUID = m.FromUID
		}
		if m.Role == store.RoleAssistant {
			// Forward the persisted step JSON verbatim so a reloaded reply
			// bubble re-renders its step card.
			row.Steps = m.Steps
		}
		out = append(out, row)
	}
	return out
}
