package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/lml2468/xclaw/core/control"
	"github.com/lml2468/xclaw/core/cron"
	"github.com/lml2468/xclaw/core/gateway"
	"github.com/lml2468/xclaw/core/im/octo"
	"github.com/lml2468/xclaw/core/router"
	"github.com/lml2468/xclaw/core/safety"
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
			t.turnsWG.Add(1)
			go func() {
				defer t.turnsWG.Done()
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
			// Prewarm the name cache before projecting summaries so the first
			// sidebar paint shows names instead of bare ids. Channel names need
			// a REST fetch; DM peer names usually free-feed from inbound, but
			// a peer who hasn't spoken since restart needs an explicit lookup.
			// Both prewarms run in parallel under a single 1.5s wall-clock cap.
			if t.connector != nil {
				prewarmNamesForSessions(t.connector, sums, 1500*time.Millisecond)
			}
			// Echo botId so the client never folds these rows into the wrong bot.
			return control.SessionsListResponse{
				BotID:    b.BotID,
				Sessions: summariesFromSessions(sums, t.connector),
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
			// FromUID resolution branches by channel type:
			//   - Console → cron.ConsoleUID (must match the renderer's
			//     CONSOLE_UID so router.SessionKey hits the same session
			//     the desktop Console view watches; stamping the owner
			//     uid here would route fired replies to a phantom session)
			//   - DM      → body.FromUID (the peer the task targets);
			//     empty is a validation error at create time
			//   - Group   → owner (the bot identifies as itself in the group)
			// FromName follows the same sanitize-at-the-boundary rule —
			// store sees only the safe form (Sec L2 defense in depth;
			// the prompt path already sanitizes via groupctx, but the
			// on-disk task shouldn't carry the unsafe form forward).
			chType := channelTypeFor(b.ChannelType, b.ChannelID)
			fromUID, err := resolveFromUID(chType, b.FromUID, owner)
			if err != nil {
				return nil, err
			}
			coords := cron.SessionCoords{
				ChannelID:   b.ChannelID,
				ChannelType: cron.ChannelKind(chType),
				FromUID:     fromUID,
				FromName:    safety.SanitizeDisplayName(b.FromName, owner),
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
			// Wrap with the requested botId so the renderer's envelope handler
			// can route the response to the right bot's local schedules map
			// (the bus has no other channel for that correlation — a fast bot
			// switch mid-fetch would otherwise misroute the tasks).
			return control.CronListResponse{BotID: b.BotID, Tasks: out}, nil

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

		case "cron.update":
			var b control.CronUpdateBody
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
			owner := t.cron.OwnerUID()
			if owner == "" {
				return nil, fmt.Errorf("bot owner not resolved yet; cannot update scheduled tasks")
			}
			// Detect the enabled-only fast path UP FRONT — a body that only
			// sets Enabled (and ID) should forward zero Coords through to
			// Manager.Update.enabledOnly, which then skips schedule
			// validation entirely. Without this, channelTypeFor's DM
			// default (= 1) would always make Coords.ChannelType non-zero
			// even on a pure toggle, defeating the fast path and forcing
			// the full-update validator that requires a Schedule.
			bodyEnabledOnly := b.Schedule == "" && b.Prompt == "" && b.Recurring == nil &&
				b.ChannelID == "" && b.ChannelType == 0 && b.FromUID == "" && b.FromName == "" &&
				b.Enabled != nil
			var coords cron.SessionCoords
			if !bodyEnabledOnly {
				// Full or partial-fields update: resolve FromUID by channel
				// type. Empty body.FromUID for DM/Console targets means
				// "preserve the existing binding" (the GUI's edit modal
				// sends blank to honor the operator's "I'm only editing
				// schedule" intent). Manager.Update's mutator interprets
				// (ChannelID + FromUID + ChannelType all zero) as that
				// preserve signal.
				chType := channelTypeFor(b.ChannelType, b.ChannelID)
				fromUID := b.FromUID
				if chType == int(cron.ChannelConsole) {
					// Always stamp the canonical Console uid — the
					// renderer can't be trusted to keep this in sync and a
					// Console task is meaningless if it can't reach the
					// Console session.
					fromUID = cron.ConsoleUID
				} else if chType == int(router.ChannelGroup) {
					// Group tasks ALWAYS run as the owner; a body.FromUID
					// for Group is a category error and is ignored.
					fromUID = owner
				}
				fromName := ""
				if b.FromName != "" {
					fromName = safety.SanitizeDisplayName(b.FromName, owner)
				}
				coords = cron.SessionCoords{
					ChannelID:   b.ChannelID,
					ChannelType: cron.ChannelKind(chType),
					FromUID:     fromUID,
					FromName:    fromName,
				}
			}
			task, err := t.cron.Update(cron.UpdateParams{
				ID:         b.ID,
				Schedule:   b.Schedule,
				Prompt:     b.Prompt,
				Recurring:  b.Recurring,
				Coords:     coords,
				Enabled:    b.Enabled,
				RequestUID: owner,
			})
			if err != nil {
				return nil, err
			}
			return cronTaskInfo(task), nil

		default:
			return nil, fmt.Errorf("unknown command %q", cmdType)
		}
	}
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
