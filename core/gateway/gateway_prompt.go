package gateway

import (
	"strings"

	"github.com/lml2468/octobuddy/core/groupctx"
	"github.com/lml2468/octobuddy/core/router"
	"github.com/lml2468/octobuddy/core/safepath"
	"github.com/lml2468/octobuddy/core/safety"
)

// bootstrapPromptHeader labels the first-run ritual block in the assembled
// system prompt. The filename is kept in sync with config.BootstrapName by a
// compile-time assertion in the test package (TestBootstrapHeaderMatchesName);
// the gateway does not import config (it stays dependent on primitives), so the
// header is a local literal rather than a derived string.
const bootstrapPromptHeader = "## BOOTSTRAP.md (first-run ritual — owner only)"

// groupDocFilename is the per-session GROUP.md the octo connector mirrors from
// the server (octo.groupDocFilename). The gateway re-reads it per group turn and
// injects it as untrusted background. Kept as a local literal — the gateway does
// not import the octo connector.
const groupDocFilename = "GROUP.md"

// groupDocMaxInjectBytes caps how much of GROUP.md is injected into the prompt.
// Must be >= the octo connector's mirror cap (octo.groupDocMaxBytes, 256 KiB):
// SafeRead ERRORS (not truncates) past the cap, so a smaller value here would
// silently drop a large-but-valid mirrored GROUP.md from the prompt entirely.
const groupDocMaxInjectBytes = 256 * 1024

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

	return renderGroupPrompt(deltaText, msg.Text)
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

func renderGroupPrompt(deltaText, currentText string) string {
	var b strings.Builder
	if deltaText != "" {
		// The whole block (header + raw bodies) is escaped once here.
		b.WriteString(safety.SanitizePromptBody(deltaText))
		b.WriteString("\n")
	}
	b.WriteString(safety.CurrentMessageAnchor)
	b.WriteString("\n")
	// Defense-in-depth: the current-message body is untrusted. Escape role labels
	// / section markers so a crafted body cannot forge prompt structure below the
	// real anchor (e.g. a second [Current message …] anchor or a fake
	// [Recent group messages] header).
	b.WriteString(safety.SafeBody(currentText).String())
	return b.String()
}

// buildSystemPrompt assembles the frozen system-prompt append: the
// non-overridable security prefix, the operator-trusted SOUL/config prompt,
// then (for GROUP/thread turns) the gateway-authored member roster +
// mention-format hint, and (for persona clones) the persona instruction. The
// SecurityPrefix always stays first and non-overridable. (The driver's preset
// base prompt is prepended by the agent CLI.)
//
// rosterPrefix is "" for DMs and for groups with no learned members. Persona
// injection mirrors openclaw inbound.ts (synthesized group hint + free-form
// persona prompt). All are config/gateway-authored (never from message
// payloads), so each is wrapped as safety.TrustedText after the SecurityPrefix.
func (g *Gateway) buildSystemPrompt(msg router.InboundMessage, rosterPrefix, cwd string) string {
	parts := []safety.SafeText{safety.TrustedText(safety.SecurityPrefix)}
	if sp := g.effectiveSystemPrompt(); sp != "" {
		parts = append(parts, safety.TrustedText(sp))
	}
	if rosterPrefix != "" {
		parts = append(parts, safety.TrustedText(rosterPrefix))
	}
	parts = g.appendGroupHandbook(parts, msg, cwd)
	parts = g.appendPersonaInstructions(parts)
	parts = g.appendBootstrap(parts, msg)
	return joinSystemPromptParts(parts)
}

// appendGroupHandbook injects the per-session GROUP.md (mirrored from the server
// by the octo connector) as UNTRUSTED background for group / thread turns. The
// content is group-member-authored, so it is escaped via safety.SafeBody and
// fenced under the [Group handbook] header that SecurityPrefix names as
// untrusted — a crafted handbook can never forge prompt structure or displace
// the operator-trusted SOUL/AGENTS above it. No-op for DMs, when no sandbox cwd
// is set, or when GROUP.md is absent/empty.
func (g *Gateway) appendGroupHandbook(parts []safety.SafeText, msg router.InboundMessage, cwd string) []safety.SafeText {
	if cwd == "" || msg.ChannelType != router.ChannelGroup {
		return parts
	}
	raw, err := safepath.SafeRead(cwd, groupDocFilename, groupDocMaxInjectBytes)
	if err != nil || len(raw) == 0 {
		return parts // absent / unreadable → skip (best-effort)
	}
	body := strings.TrimSpace(string(raw))
	if body == "" {
		return parts
	}
	// Header is a trusted literal (the privileged marker); the body is escaped so
	// a crafted GROUP.md can't forge a second marker or a role label. Assemble
	// the escaped body under the literal header, then mint the combined block as
	// SafeText (already escaped — TrustedText documents that the header is ours).
	block := safety.GroupHandbookHeader + "\n" + safety.SanitizePromptBody(body)
	return append(parts, safety.TrustedText(block))
}

// appendBootstrap injects the first-run ritual (BOOTSTRAP.md) — but ONLY in an
// owner-trusted channel (router.InboundMessage.OwnerTrusted: a Console turn or
// the owner's IM DM; never a group or non-owner DM). The ritual instructs the
// bot to (re)write its own SOUL.md, so letting an untrusted user drive it would
// be self-injection of the trusted prompt. Operator-authored content, so wrapped
// as TrustedText. No-op once the bot deletes BOOTSTRAP.md (per-turn reload → "").
func (g *Gateway) appendBootstrap(parts []safety.SafeText, msg router.InboundMessage) []safety.SafeText {
	ownerUID := ""
	if g.owner != nil {
		ownerUID = g.owner()
	}
	if !msg.OwnerTrusted(ownerUID) {
		return parts
	}
	if body := g.effectiveBootstrap(); body != "" {
		parts = append(parts, safety.TrustedText(bootstrapPromptHeader+"\n\n"+body))
	}
	return parts
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

func (g *Gateway) appendPersonaInstructions(parts []safety.SafeText) []safety.SafeText {
	if g.persona.Configured() {
		if p := g.persona.BuildGroupSystemPrompt(); p != "" {
			parts = append(parts, safety.TrustedText(p))
		}
		if h := g.persona.ComposeHint(g.personaPrompt); h != "" {
			parts = append(parts, safety.TrustedText(h))
		}
	}
	return parts
}

func joinSystemPromptParts(parts []safety.SafeText) string {
	var b strings.Builder
	for i, p := range parts {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(p.String())
	}
	return b.String()
}
