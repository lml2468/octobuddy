package octo

// Deterministic (non-LLM) handling of group system / control-plane messages.
//
// octo-server emits two shapes of control-plane message into a group channel,
// both of which the connector must keep OUT of the LLM turn path (left
// unfiltered they reach the trigger classifier and falsely trigger a reply —
// the group-announcement bug):
//
//   1. True system payload TYPES (CMD / 1000-2000, e.g. group member add/remove,
//      group settings change). Detected via MessageType.IsSystem.
//   2. Text(=1) payloads carrying an `event` envelope (group_md_updated,
//      thread_md_updated, mention_pref_updated, …) that additionally @-mention
//      the bot. These are NOT in the system type range — they are detected via
//      MessagePayload.IsControlEvent.
//
// onInbound routes both shapes here BEFORE classification, so a control-plane
// message can never enqueue a turn.
//
// Beyond suppression, the md events drive a deterministic mirror of the
// server-stored GROUP.md into the session's sandbox cwd (the gateway then
// injects that local GROUP.md as untrusted background each group turn):
//   - group_md_updated / thread_md_updated → mirror server GROUP.md → local
//     GROUP.md; the *_deleted variants remove the local copy.
//
// Member-roster events are intentionally NOT materialized: the gateway already
// injects a [Group Members] roster from the connector's name cache
// (groupctx roster), so a separate MEMBERS.md would duplicate it. Membership
// system messages are still suppressed (they never reach the LLM) — they just
// don't write a file.
//
// Best-effort: a failed fetch / write is logged and dropped, retried on the next
// event (mirrors octo-server treating its own notices as best-effort).
//
// FUTURE (extension point — not wired here): the slash-command surface
// (/groupmd-init, /groupmd-update, /groupmd-clear, /fork) will run through the
// gateway's handleCommand path (core/gateway/commands.go) and can reuse
// mirrorGroupDoc. /fork (create 子区) maps to thread.create
// (POST /v1/bot/groups/{group_no}/threads, User Bot only).

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/lml2468/octobuddy/core/clog"
	"github.com/lml2468/octobuddy/core/router"
	"github.com/lml2468/octobuddy/core/safepath"
)

const (
	// groupDocFilename is written at the root of the session sandbox cwd and
	// mirrors the server-stored GROUP.md (group_md / thread_md).
	groupDocFilename = "GROUP.md"

	// sysDebounceWindow coalesces a burst of control-plane events for one channel
	// into a single refresh. Short enough that a real later change still
	// refreshes promptly.
	sysDebounceWindow = 3 * time.Second

	// sysFetchTimeout bounds each REST fetch so a slow server can't pin a
	// refresh goroutine indefinitely.
	sysFetchTimeout = 15 * time.Second

	// GroupDocMaxBytes caps the mirrored GROUP.md size written locally. Exported
	// so the daemon can assert gateway.GroupDocMaxInjectBytes >= this in a
	// cross-package test (the gateway's SafeRead ERRORS past its cap, so it must
	// be >= this mirror cap or a large-but-valid GROUP.md silently vanishes).
	GroupDocMaxBytes = 256 * 1024
)

// handleSystemMessage is the terminal sink for control-plane payloads. It NEVER
// enqueues an LLM turn. md events mirror the server GROUP.md; every other
// control-plane message is logged and dropped (suppressed only).
func (c *Connector) handleSystemMessage(m BotMessage) {
	action := classifyControlMessage(m.Payload)
	if action == sysActionIgnore {
		clog.For("octo").Debug("control message ignored",
			"type", int(m.Payload.Type), "event", eventTypeOf(m.Payload), "channel", m.ChannelID)
		return
	}
	if m.ChannelType != ChannelGroup && m.ChannelType != ChannelCommunityTopic {
		return // no group docs to maintain outside a group / thread channel
	}
	if c.gateway == nil {
		return // no sandbox wiring (REPL / unit harness)
	}

	// Debounce per channel (the write target / session), NOT the parent group_no:
	// a thread and its parent group — or two sibling threads — share a group_no
	// but write to distinct session sandboxes. The burst itself always arrives on
	// a single channelID, so per-channel keying still coalesces it.
	if !c.claimSysRefresh(m.ChannelID) {
		return
	}

	channelID := m.ChannelID
	// Run on a turnsWG-tracked goroutine so it can't block the WS read loop and
	// graceful shutdown still waits for it. Dropped if the connector is closed.
	c.goTracked(func() {
		c.applySysAction(channelID, action)
	})
}

// sysAction enumerates what a control-plane message asks the connector to do.
type sysAction int

const (
	sysActionIgnore    sysAction = iota
	sysActionMirrorDoc           // mirror server GROUP.md/thread-md → local GROUP.md
	sysActionDeleteDoc           // server md deleted → remove local GROUP.md
)

// classifyControlMessage maps a control-plane payload to the action it triggers.
// Only the md events (carried in the `event` envelope) produce a file action;
// every other control-plane message (member changes, mention_pref, CMD, …) is
// suppressed without writing anything.
func classifyControlMessage(p MessagePayload) sysAction {
	if p.IsControlEvent() {
		switch p.Event.Type {
		case EventGroupMdUpdated, EventThreadMdUpdated:
			return sysActionMirrorDoc
		case EventGroupMdDeleted, EventThreadMdDeleted:
			return sysActionDeleteDoc
		}
	}
	return sysActionIgnore
}

func eventTypeOf(p MessagePayload) string {
	if p.Event != nil {
		return p.Event.Type
	}
	return ""
}

// claimSysRefresh returns true if no refresh for key has fired within
// sysDebounceWindow, recording this one. Coalesces event bursts. key is the
// channelID (see handleSystemMessage).
func (c *Connector) claimSysRefresh(key string) bool {
	now := time.Now()
	c.sysMu.Lock()
	defer c.sysMu.Unlock()
	if last, ok := c.sysDebounce[key]; ok && now.Sub(last) < sysDebounceWindow {
		return false
	}
	c.sysDebounce[key] = now
	return true
}

// applySysAction resolves the session sandbox cwd for the channel and performs
// the action. Best-effort.
func (c *Connector) applySysAction(channelID string, action sysAction) {
	cwd, err := c.gateway.SessionCwd(router.ChannelGroup, channelID)
	if err != nil {
		c.logf("control refresh: resolve cwd for %s: %v", channelID, err)
		return
	}
	if cwd == "" {
		return // sandbox disabled
	}
	ctx, cancel := context.WithTimeout(c.ctx(), sysFetchTimeout)
	defer cancel()

	switch action {
	case sysActionMirrorDoc:
		c.mirrorGroupDoc(ctx, channelID, cwd)
	case sysActionDeleteDoc:
		c.deleteGroupDoc(cwd)
	}
}

// mirrorGroupDoc fetches the server-stored GROUP.md (group or thread, depending
// on channelID) and writes it to the local GROUP.md. An empty server doc removes
// the local copy so it never goes stale after a server-side clear. Exported
// reuse helper for the future slash-command path.
func (c *Connector) mirrorGroupDoc(ctx context.Context, channelID, cwd string) {
	groupNo := ExtractParentGroupNo(channelID)
	var (
		md  GroupMd
		err error
	)
	if shortID := extractThreadShortID(channelID); shortID != "" {
		md, err = c.rest.GetThreadMd(ctx, groupNo, shortID)
	} else {
		md, err = c.rest.GetGroupMd(ctx, groupNo)
	}
	if err != nil {
		c.logf("control refresh: get group md %s: %v", channelID, err)
		return
	}
	content := strings.TrimSpace(md.Content)
	if content == "" {
		// Server md is unset/cleared — drop the local mirror rather than leave a
		// stale copy.
		c.deleteGroupDoc(cwd)
		return
	}
	c.writeGroupDoc(cwd, groupDocFilename, truncateByBytes(content, GroupDocMaxBytes, "\n…（GROUP.md 已截断）"))
}

// deleteGroupDoc removes the local GROUP.md mirror (best-effort; a missing file
// is not an error worth logging).
func (c *Connector) deleteGroupDoc(cwd string) {
	if err := safepath.SafeRemove(cwd, groupDocFilename); err != nil && !os.IsNotExist(err) {
		c.logf("control refresh: remove %s: %v", groupDocFilename, err)
	}
}

func (c *Connector) writeGroupDoc(cwd, name, content string) {
	if err := safepath.SafeWrite(cwd, name, []byte(content), 0o644); err != nil {
		c.logf("control refresh: write %s: %v", name, err)
	}
}
