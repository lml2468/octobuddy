package main

import (
	"strings"
	"sync"
	"time"

	"github.com/lml2468/octobuddy/core/control"
	"github.com/lml2468/octobuddy/core/cron"
	"github.com/lml2468/octobuddy/core/im/octo"
	"github.com/lml2468/octobuddy/core/router"
	"github.com/lml2468/octobuddy/core/store"
)

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
