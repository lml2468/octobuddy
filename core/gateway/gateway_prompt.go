package gateway

import (
	"fmt"
	"os"
	"strings"

	"github.com/lml2468/octobuddy/core/groupctx"
	"github.com/lml2468/octobuddy/core/router"
	"github.com/lml2468/octobuddy/core/safety"
)

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
			fmt.Fprintf(os.Stderr, "[gateway] save inferred reply seq %s: %v\n", sessionKey, err)
		}
	}
}

func (g *Gateway) botReplyCutoffSeq(sessionKey string) int64 {
	// Answered/new cutoff (cc G10): the IM seq of the last message the bot
	// replied to. Messages at/below it render under [Previously answered].
	cutoffSeq, err := g.store.BotReplySeq(sessionKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[gateway] bot reply seq %s: %v\n", sessionKey, err)
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
// mention-format hint, the operator-authored [Group instructions] block for
// this channel, and (for persona clones) the persona instruction. The
// SecurityPrefix always stays first and non-overridable. (The driver's preset
// base prompt is prepended by the agent CLI.)
//
// rosterPrefix is "" for DMs and for groups with no learned members. [Group
// instructions] is injected only for groups (cc-channel-octo index.ts). Persona
// injection mirrors openclaw inbound.ts (synthesized group hint + free-form
// persona prompt). All are config/gateway-authored (never from message
// payloads), so each is wrapped as safety.TrustedText after the SecurityPrefix.
func (g *Gateway) buildSystemPrompt(msg router.InboundMessage, rosterPrefix string) string {
	parts := []safety.SafeText{safety.TrustedText(safety.SecurityPrefix)}
	if g.systemPrompt != "" {
		parts = append(parts, safety.TrustedText(g.systemPrompt))
	}
	if rosterPrefix != "" {
		parts = append(parts, safety.TrustedText(rosterPrefix))
	}
	parts = g.appendGroupInstructions(parts, msg)
	parts = g.appendPersonaInstructions(parts)
	return joinSystemPromptParts(parts)
}

func (g *Gateway) appendGroupInstructions(parts []safety.SafeText, msg router.InboundMessage) []safety.SafeText {
	if g.groupMD != nil && msg.ChannelType == router.ChannelGroup && msg.ChannelID != "" {
		if instr, ok := g.groupMD.Load(msg.ChannelID); ok {
			parts = append(parts, safety.TrustedText("[Group instructions]\n"+instr))
		}
	}
	return parts
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
