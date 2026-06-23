package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/lml2468/octobuddy/core/control"
	"github.com/lml2468/octobuddy/core/cron"
	"github.com/lml2468/octobuddy/core/gateway"
	"github.com/lml2468/octobuddy/core/im/octo"
	"github.com/lml2468/octobuddy/core/router"
	"github.com/lml2468/octobuddy/core/safety"
	"github.com/lml2468/octobuddy/core/store"
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
			log.Printf("[secret] clear %s/%s: %v", b.BotID, b.Kind, err)
			return nil, fmt.Errorf("secret.inject clear failed for %s/%s", b.BotID, b.Kind)
		}
	} else if err := t.secrets.Set(b.Kind, b.Value); err != nil {
		log.Printf("[secret] set %s/%s: %v", b.BotID, b.Kind, err)
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

func (d controlCommandDispatcher) cronCreate(body json.RawMessage) (any, error) {
	b, err := decodeControlBody[control.CronCreateBody](body)
	if err != nil {
		return nil, err
	}
	t, owner, err := d.cronTarget(b.BotID, "create")
	if err != nil {
		return nil, err
	}
	coords, err := cronCreateCoords(b, owner)
	if err != nil {
		return nil, err
	}
	task, err := t.cron.Create(cron.CreateParams{
		Schedule: b.Schedule, Prompt: b.Prompt, Recurring: b.Recurring, Coords: coords, RequestUID: owner,
	})
	if err != nil {
		return nil, err
	}
	return cronTaskInfo(task), nil
}

func (d controlCommandDispatcher) cronTarget(botID, action string) (*botTarget, string, error) {
	t, err := d.deps.resolve(botID)
	if err != nil {
		return nil, "", err
	}
	if t.cron == nil {
		return nil, "", fmt.Errorf("cron is not enabled for this bot")
	}
	owner := t.cron.OwnerUID()
	if owner == "" {
		return nil, "", fmt.Errorf("bot owner not resolved yet; cannot %s scheduled tasks", action)
	}
	return t, owner, nil
}

func cronCreateCoords(b control.CronCreateBody, owner string) (cron.SessionCoords, error) {
	chType := channelTypeFor(b.ChannelType, b.ChannelID)
	fromUID, err := resolveFromUID(chType, b.FromUID, owner)
	if err != nil {
		return cron.SessionCoords{}, err
	}
	return cron.SessionCoords{
		ChannelID:   b.ChannelID,
		ChannelType: cron.ChannelKind(chType),
		FromUID:     fromUID,
		FromName:    safety.SanitizeDisplayName(b.FromName, owner),
	}, nil
}

func (d controlCommandDispatcher) cronList(body json.RawMessage) (any, error) {
	b, err := decodeControlBody[control.CronListBody](body)
	if err != nil {
		return nil, err
	}
	t, err := d.cronListTarget(b.BotID)
	if err != nil {
		return nil, err
	}
	tasks, err := t.cron.List()
	if err != nil {
		return nil, err
	}
	out := make([]control.CronTaskInfo, 0, len(tasks))
	for _, task := range tasks {
		out = append(out, cronTaskInfo(task))
	}
	return control.CronListResponse{BotID: b.BotID, Tasks: out}, nil
}

func (d controlCommandDispatcher) cronListTarget(botID string) (*botTarget, error) {
	t, err := d.deps.resolve(botID)
	if err != nil {
		return nil, err
	}
	if t.cron == nil {
		return nil, fmt.Errorf("cron is not enabled for this bot")
	}
	return t, nil
}

func (d controlCommandDispatcher) cronDelete(body json.RawMessage) (any, error) {
	b, err := decodeControlBody[control.CronDeleteBody](body)
	if err != nil {
		return nil, err
	}
	t, owner, err := d.cronTarget(b.BotID, "delete")
	if err != nil {
		return nil, err
	}
	if err := t.cron.Delete(b.ID, owner); err != nil {
		return nil, err
	}
	return control.OKBody{OK: true}, nil
}

func (d controlCommandDispatcher) cronUpdate(body json.RawMessage) (any, error) {
	b, err := decodeControlBody[control.CronUpdateBody](body)
	if err != nil {
		return nil, err
	}
	t, owner, err := d.cronTarget(b.BotID, "update")
	if err != nil {
		return nil, err
	}
	task, err := t.cron.Update(cron.UpdateParams{
		ID: b.ID, Schedule: b.Schedule, Prompt: b.Prompt, Recurring: b.Recurring,
		Coords: cronUpdateCoords(b, owner), Enabled: b.Enabled, RequestUID: owner,
	})
	if err != nil {
		return nil, err
	}
	return cronTaskInfo(task), nil
}

func cronUpdateCoords(b control.CronUpdateBody, owner string) cron.SessionCoords {
	if cronUpdateEnabledOnly(b) {
		return cron.SessionCoords{}
	}
	chType := channelTypeFor(b.ChannelType, b.ChannelID)
	fromUID := b.FromUID
	if chType == int(cron.ChannelConsole) {
		fromUID = cron.ConsoleUID
	} else if chType == int(router.ChannelGroup) {
		fromUID = owner
	}
	fromName := ""
	if b.FromName != "" {
		fromName = safety.SanitizeDisplayName(b.FromName, owner)
	}
	return cron.SessionCoords{
		ChannelID: b.ChannelID, ChannelType: cron.ChannelKind(chType), FromUID: fromUID, FromName: fromName,
	}
}

func cronUpdateEnabledOnly(b control.CronUpdateBody) bool {
	return b.Schedule == "" && b.Prompt == "" && b.Recurring == nil &&
		b.ChannelID == "" && b.ChannelType == 0 && b.FromUID == "" && b.FromName == "" &&
		b.Enabled != nil
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
		row := control.HistoryMessage{Role: string(m.Role), Content: m.Content, TS: m.Timestamp, Cron: m.Cron}
		if m.Role == store.RoleUser {
			row.FromName = safety.SanitizeDisplayName(m.FromName, "")
		}
		out = append(out, row)
	}
	return out
}

// summariesFromSessions projects store session summaries onto the wire type,
// folding in the IM connector's name cache so the GUI sidebar can show real
// channel / DM-peer names instead of bare ids. The resolver may be nil (REPL
// single-bot, tests) — then ChannelName is left empty and the GUI falls back
// to the prettified key. Lookups are non-blocking: a cache miss returns ""
// and kicks a background REST fetch the next call sees populated.
func summariesFromSessions(sums []store.SessionSummary, conn *octo.Connector) []control.SessionSummary {
	out := make([]control.SessionSummary, 0, len(sums))
	for _, s := range sums {
		out = append(out, summaryRow(s, conn))
	}
	return out
}

// summaryRow projects one store SessionSummary onto the wire shape — shared
// between sessions.list (the snapshot pull) and the session.upserted event
// (the per-turn push) so both surfaces always speak the same projection.
// For threads, ChannelName carries the thread's own name and
// ParentChannelName carries the parent group's name; the GUI composes each
// surface (sidebar uses ChannelName alone, chat header reads
// "<ParentChannelName> > <ChannelName>").
func summaryRow(s store.SessionSummary, conn *octo.Connector) control.SessionSummary {
	row := control.SessionSummary{
		Key:         s.Key,
		ChannelType: s.ChannelType,
		UpdatedAt:   s.UpdatedAt,
		Preview:     s.Preview,
		LastRole:    string(s.LastRole),
	}
	if conn == nil {
		return row
	}
	switch router.ChannelType(s.ChannelType) {
	case router.ChannelDM:
		if uid := dmPeerUID(s.Key); uid != "" {
			row.ChannelName = conn.UserName(uid)
		}
	case router.ChannelGroup:
		row.ChannelName = conn.ChannelName(s.Key)
		if octo.IsThreadChannelID(s.Key) {
			row.ParentChannelName = conn.ChannelName(octo.ExtractParentGroupNo(s.Key))
		}
	}
	return row
}

// sessionTouchBroadcaster returns a notifier suitable for
// gateway.WithSessionTouchNotifier: on every store-touch it looks up the
// session's freshly-written row and broadcasts a session.upserted event so
// GUI clients can incrementally refresh the sidebar without polling
// sessions.list. Brand-new sessions (e.g. a just-started thread) appear on
// first touch instead of staying invisible until the next pull.
//
// Reads the row via ListSessions (single SQL scan) and filters — adds one
// query per turn, negligible at typical bot scale. A future store
// optimization could expose a per-key getter; the projection logic in
// summaryRow stays identical either way.
func sessionTouchBroadcaster(srv *control.Server, botID string, st *store.Store, conn *octo.Connector) func(string, string, router.ChannelType) {
	return func(sessionKey, channelID string, channelType router.ChannelType) {
		sums, err := st.ListSessions()
		if err != nil {
			return
		}
		for _, s := range sums {
			if s.Key != sessionKey {
				continue
			}
			srv.Broadcast("session.upserted", control.SessionUpsertedBody{
				BotID:   botID,
				Session: summaryRow(s, conn),
			})
			return
		}
	}
}

// nameResolvedBroadcaster returns a hook for Connector.SetNameResolvedHook: when
// a background name fetch lands a new display name, it finds every session that
// references the resolved id and re-broadcasts session.upserted so the GUI's
// sidebar row updates from the bare id to the name without waiting for the next
// turn. summaryRow now reads a warm cache for that id, so the projected
// ChannelName is populated. Reuses the same ListSessions scan + projection as
// sessionTouchBroadcaster; resolves are far rarer than turns, so the extra
// query is negligible. No fetch is re-kicked (the cache entry is fresh), so this
// can't loop.
func nameResolvedBroadcaster(srv *control.Server, botID string, st *store.Store, conn *octo.Connector) func(octo.NameKind, string, string) {
	return func(kind octo.NameKind, key, _ string) {
		sums, err := st.ListSessions()
		if err != nil {
			return
		}
		for _, s := range sums {
			if !sessionRefersToName(s, kind, key) {
				continue
			}
			srv.Broadcast("session.upserted", control.SessionUpsertedBody{
				BotID:   botID,
				Session: summaryRow(s, conn),
			})
		}
	}
}

// sessionRefersToName reports whether session s would surface the just-resolved
// name: a DM session whose peer uid matches (NameKindUser), or a group/thread
// session whose own id — or, for a thread, its parent group id — matches
// (NameKindChannel). Mirrors the id→name lookups summaryRow does per kind.
func sessionRefersToName(s store.SessionSummary, kind octo.NameKind, key string) bool {
	switch kind {
	case octo.NameKindUser:
		return router.ChannelType(s.ChannelType) == router.ChannelDM && dmPeerUID(s.Key) == key
	case octo.NameKindChannel:
		if router.ChannelType(s.ChannelType) != router.ChannelGroup {
			return false
		}
		if s.Key == key {
			return true
		}
		return octo.IsThreadChannelID(s.Key) && octo.ExtractParentGroupNo(s.Key) == key
	}
	return false
}

// prewarmNamesForSessions populates the name cache for the given session list
// in parallel — group channel names and DM peer names fetched concurrently
// under a single wall-clock budget (otherwise the two prewarm calls would
// serialize for 2× the budget). Console-keyed DM sessions are skipped: the
// uid "gui-user" isn't a real IM peer and getUserInfo on it just earns a 500.
func prewarmNamesForSessions(conn *octo.Connector, sums []store.SessionSummary, timeout time.Duration) {
	var groupKeys, dmUids []string
	for _, s := range sums {
		switch router.ChannelType(s.ChannelType) {
		case router.ChannelGroup:
			groupKeys = append(groupKeys, s.Key)
		case router.ChannelDM:
			if uid := dmPeerUID(s.Key); uid != "" {
				dmUids = append(dmUids, uid)
			}
		}
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); conn.PrewarmChannelNames(groupKeys, timeout) }()
	go func() { defer wg.Done(); conn.PrewarmUserNames(dmUids, timeout) }()
	wg.Wait()
}

// dmPeerUID extracts the peer's uid from a DM session key ("<spaceId>:<uid>"
// or bare "<uid>") and filters out the synthetic Console key — the only
// non-IM uid we'd otherwise try to resolve through the name service.
func dmPeerUID(key string) string {
	uid := key
	if i := strings.LastIndexByte(key, ':'); i >= 0 {
		uid = key[i+1:]
	}
	if uid == "" || uid == cron.ConsoleUID {
		return ""
	}
	return uid
}

// channelTypeFor resolves the router/octo channel type for a cron task: an
// explicit non-zero type wins; otherwise a present channelId implies a group and
// its absence a DM. Mirrors the create-time coords binding in cron-tool.ts.
// ChannelConsole (= 3) is honored explicitly so a Console-target task isn't
// silently demoted to DM (the default branch); the IM connector ignores it
// and bot.go's fireCronTask routes it past EnqueueCron straight to the
// gateway. Without this branch a Console body would fall through to "DM
// with empty channelId" which the connector would then try to deliver to.
// resolveFromUID picks the stored FromUID for a NEW cron task based on the
// channel type, falling back to the server-resolved owner for Group targets
// and stamping the canonical ConsoleUID for Console. DM tasks require an
// explicit body FromUID (the peer the task should DM to) — empty is a
// validation error because storing the owner uid for a "DM to alice" task
// would silently rewrite the target to "DM to self" on first fire.
// Used only by cron.create; cron.update's "blank = preserve" semantics
// live in the update handler + Manager.Update mutator.
func resolveFromUID(chType int, bodyFromUID, owner string) (string, error) {
	switch chType {
	case int(cron.ChannelConsole):
		return cron.ConsoleUID, nil
	case int(router.ChannelGroup):
		return owner, nil
	default: // DM
		if bodyFromUID == "" {
			return "", fmt.Errorf("DM target requires fromUid (peer's uid)")
		}
		return bodyFromUID, nil
	}
}

func channelTypeFor(explicit int, channelID string) int {
	if explicit == int(router.ChannelDM) || explicit == int(router.ChannelGroup) || explicit == int(cron.ChannelConsole) {
		return explicit
	}
	if channelID != "" {
		return int(router.ChannelGroup)
	}
	return int(router.ChannelDM)
}

// cronTaskInfo projects a stored cron task onto the wire type (nextRun rendered
// as RFC3339, mirroring cron-tool.ts summarize). LastRun follows the same
// formatter and is omitted entirely when zero (the task has never fired). The
// channel coords are exposed so the GUI can render "into 群 X" / "into DM @ y"
// / "into 控制台" without needing a side-channel lookup, but CreatedBy and
// FromUID are deliberately NOT included — operator-internal auth state, of no
// use to the renderer and a needless leakage surface.
func cronTaskInfo(t cron.Task) control.CronTaskInfo {
	next := ""
	if t.NextRun != 0 {
		next = time.UnixMilli(t.NextRun).UTC().Format(time.RFC3339)
	}
	last := ""
	if t.LastRun != 0 {
		last = time.UnixMilli(t.LastRun).UTC().Format(time.RFC3339)
	}
	return control.CronTaskInfo{
		ID:          t.ID,
		Schedule:    t.Schedule,
		Recurring:   t.Recurring,
		Prompt:      t.Prompt,
		NextRun:     next,
		LastRun:     last,
		ChannelID:   t.ChannelID,
		ChannelType: int(t.ChannelType),
		FromName:    t.FromName,
		Enabled:     t.Enabled,
	}
}
