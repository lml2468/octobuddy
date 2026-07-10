package gateway

import (
	"strings"

	"github.com/lml2468/octobuddy/core/agent"
	"github.com/lml2468/octobuddy/core/groupctx"
	"github.com/lml2468/octobuddy/core/prompt"
	"github.com/lml2468/octobuddy/core/router"
	"github.com/lml2468/octobuddy/core/safepath"
)

// bootstrapPromptHeader labels the first-run ritual block in the assembled
// system prompt. It aliases prompt.BootstrapHeader (the assembly package owns the
// literal now) so the compile-time cross-check against config.BootstrapName
// (TestBootstrapHeaderMatchesName) keeps guarding filename drift without the
// gateway importing config.
const bootstrapPromptHeader = prompt.BootstrapHeader

// groupDocFilename is the per-session GROUP.md the octo connector mirrors from
// the server (octo.groupDocFilename). The gateway re-reads it per group turn and
// injects it as untrusted background. Kept as a local literal — the gateway does
// not import the octo connector.
const groupDocFilename = "GROUP.md"

// GroupDocMaxInjectBytes caps how much of GROUP.md is injected into the prompt.
// Must be >= the octo connector's mirror cap (octo.GroupDocMaxBytes, 256 KiB):
// SafeRead ERRORS (not truncates) past the cap, so a smaller value here would
// silently drop a large-but-valid mirrored GROUP.md from the prompt entirely.
// Exported so the daemon (which imports both packages) can assert the
// >= invariant in a cross-package test rather than leaving it comment-only.
const GroupDocMaxInjectBytes = 256 * 1024

// buildGroupPrompt assembles the prompt for a turn. For a DM (or when group
// context is disabled) it returns the raw message text. For a group message it
// injects the [Recent group messages] delta as UNTRUSTED background and
// demarcates the real request with the current-message anchor. CRITICAL ordering
// (group-context.ts): the delta is built BEFORE the current message is cached, so
// the message isn't echoed into its own background.
func (g *Gateway) buildGroupPrompt(sessionKey string, msg router.InboundMessage) string {
	if g.groups == nil || msg.ChannelType != router.ChannelGroup || msg.ChannelID == "" {
		return msg.Text
	}

	g.backfillGroupContext(sessionKey, msg.ChannelID)
	cutoffSeq := g.botReplyCutoffSeq(sessionKey)

	cursor := g.groups.Cursor(msg.ChannelID)
	deltaText, _ := g.groups.BuildContextSince(msg.ChannelID, cursor, cutoffSeq)
	// Cache the current message AFTER reading the delta.
	g.groups.Push(msg.ChannelID, msg.FromUID, msg.FromName, msg.Text, msg.MessageSeq)
	// Advance the cursor past everything now in the channel.
	g.groups.SetCursor(msg.ChannelID, g.groups.MaxID(msg.ChannelID))

	return prompt.RenderGroup(deltaText, msg.Text)
}

func (g *Gateway) backfillGroupContext(sessionKey, channelID string) {
	// Cold-start backfill (cc G4): the FIRST time this channel is seen with an
	// empty local window, seed it from the IM REST API. Runs at most once per
	// (process, channel). The inferred cutoff (highest bot-reply seq found in the
	// backfill) primes answered/new segmentation so the first turn doesn't treat
	// already-answered history as new.
	if g.groupBackfill == nil {
		return
	}
	botUID := ""
	if g.botUID != nil {
		botUID = g.botUID()
	}
	inferred, ran := g.groups.Backfill(channelID, botUID, func() []groupctx.BackfillMessage {
		return g.groupBackfill(channelID, 0)
	})
	if ran && inferred > 0 {
		if err := g.store.SaveBotReplySeq(sessionKey, inferred); err != nil {
			glog().Error("save inferred reply seq", "session", sessionKey, "err", err)
		}
	}
}

func (g *Gateway) botReplyCutoffSeq(sessionKey string) int64 {
	// Answered/new cutoff (cc G10): the IM seq of the last message the bot
	// replied to. Messages at/below it render under [Previously answered].
	cutoffSeq, err := g.store.BotReplySeq(sessionKey)
	if err != nil {
		glog().Error("bot reply seq", "session", sessionKey, "err", err)
	}
	return cutoffSeq
}

// buildSystemPrompt gathers this turn's already-resolved raw inputs (per-turn
// resolvers, roster snapshot, GROUP.md read, owner-trust gate, persona) and hands
// them to prompt.AssembleSystem, which owns the SafeText wrapping + fixed ordering
// + injection escaping. The SecurityPrefix always stays first. (The driver's
// preset base prompt is prepended by the agent CLI.)
//
// rosterPrefix is "" for DMs and for groups with no learned members.
func (g *Gateway) buildSystemPrompt(msg router.InboundMessage, rosterPrefix, cwd string) agent.SystemPrompt {
	pg, ph := g.personaSegments()
	return prompt.AssembleSystem(prompt.SystemInputs{
		OperatorPrompt: g.effectiveSystemPrompt(),
		RosterPrefix:   rosterPrefix,
		Handbook:       g.groupHandbookBody(msg, cwd),
		IsGroup:        msg.ChannelType == router.ChannelGroup,
		PersonaGroup:   pg,
		PersonaHint:    ph,
		Bootstrap:      g.bootstrapBody(msg),
	})
}

// groupHandbookBody reads the per-session GROUP.md (mirrored from the server by
// the octo connector) and returns its RAW body for a group turn — escaping +
// fencing is prompt.AssembleSystem's job. Returns "" for DMs, when no sandbox cwd
// is set, or when GROUP.md is absent/empty/unreadable (best-effort). The
// safepath.SafeRead stays here because it is gateway filesystem I/O.
func (g *Gateway) groupHandbookBody(msg router.InboundMessage, cwd string) string {
	if cwd == "" || msg.ChannelType != router.ChannelGroup {
		return ""
	}
	raw, err := safepath.SafeRead(cwd, groupDocFilename, GroupDocMaxInjectBytes)
	if err != nil || len(raw) == 0 {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// bootstrapBody returns the first-run ritual (BOOTSTRAP.md) body — but ONLY in an
// owner-trusted channel (router.InboundMessage.OwnerTrusted: a Console turn or
// the owner's IM DM; never a group or non-owner DM). The ritual instructs the bot
// to (re)write its own SOUL.md, so letting an untrusted user drive it would be
// self-injection of the trusted prompt. Returns "" once the bot deletes
// BOOTSTRAP.md (per-turn reload → ""). The owner-trust gate stays here because it
// needs g.owner + msg.OwnerTrusted, which prompt (a leaf) must not import.
func (g *Gateway) bootstrapBody(msg router.InboundMessage) string {
	ownerUID := ""
	if g.owner != nil {
		ownerUID = g.owner()
	}
	if !msg.OwnerTrusted(ownerUID) {
		return ""
	}
	return g.effectiveBootstrap()
}

// effectiveBootstrap returns the per-turn BOOTSTRAP.md body, or "" when no
// resolver is installed (mirrors effectiveSystemPrompt so callers get the nil
// handling for free).
func (g *Gateway) effectiveBootstrap() string {
	if g.resolveBootstrapFn == nil {
		return ""
	}
	return g.resolveBootstrapFn()
}

// personaSegments returns the OBO persona-clone system segments (group hint +
// free-form hint), or ("", "") for a regular bot. AssembleSystem wraps them as
// trusted segments.
func (g *Gateway) personaSegments() (group, hint string) {
	if !g.persona.Configured() {
		return "", ""
	}
	return g.persona.BuildGroupSystemPrompt(), g.persona.ComposeHint(g.personaPrompt)
}
