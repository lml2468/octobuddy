package groupctx

import (
	"fmt"
	"strings"

	"github.com/lml2468/xclaw/core/safety"
)

// MentionFormatHint is the single source of truth for the structured-mention
// output-format instruction, reused by both branches of MemberListPrefix so the
// format rules can never drift between injection points. Ported from
// openclaw-channel-octo/src/mention-utils.ts (MENTION_FORMAT_HINT) and the
// MENTION FORMAT clause of cc-channel-octo/src/agent-bridge.ts. The placeholder
// slots use angle brackets (@[<uid>:<displayName>]) so the hint text itself
// never parses as a real @[uid:name] mention.
const MentionFormatHint = "To @mention a member, use @[<uid>:<displayName>] where <uid> is the member's REAL " +
	"32-char hex id, with exactly ONE colon and the square brackets are REQUIRED. " +
	"Never use a username/bot_id (e.g. @somebody_bot), never copy the literal word " +
	`"uid", never write a bare uid without brackets, never omit the brackets. ` +
	"I will convert the @[<uid>:<displayName>] form to the correct mention before sending."

// memberListInlineThreshold is the cutoff (inclusive) for inlining the roster
// vs. emitting a look-it-up hint, mirroring buildMemberListPrefix in
// openclaw-channel-octo/src/inbound.ts.
const memberListInlineThreshold = 10

// MemberListPrefix renders the operator-trusted group-roster block injected into
// the system prompt for GROUP turns, ported from
// openclaw-channel-octo/src/inbound.ts buildMemberListPrefix. With no known
// members it returns "". With ≤10 it inlines `name (uid)` one per line plus the
// mention-format hint anchored on a real member. With >10 it tells the agent to
// look members up rather than dumping the full roster. The returned text is
// gateway-authored and the caller wraps it as safety.TrustedText.
//
// Divergence from the TS source: the >10 branch there names the openclaw
// `octo_management action="group-members"` tool, which xclaw does not yet
// expose; we keep the look-it-up intent but reference the recent-messages
// context / a roster tool generically so the agent is never told to call a
// tool that is not wired up.
func (g *GroupContext) MemberListPrefix(channelID string) string {
	members := g.Members(channelID)
	if len(members) == 0 {
		return ""
	}

	if len(members) <= memberListInlineThreshold {
		var b strings.Builder
		b.WriteString("[Group Members]\n")
		for _, m := range members {
			// Name is sanitized at storage; UID is not, so escape it here too —
			// a hostile uid with brackets/line breaks could otherwise forge
			// structure inside this operator-trusted roster block.
			fmt.Fprintf(&b, "  %s (%s)\n", m.Name, safety.SanitizeDisplayName(m.UID, ""))
		}
		b.WriteString("\n")
		b.WriteString(MentionFormatHint)
		fmt.Fprintf(&b, "\n(e.g. @[%s:%s]).\n\n", safety.SanitizeDisplayName(members[0].UID, ""), members[0].Name)
		return b.String()
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[Group Info] This group has %d members — too many to list here.\n", len(members))
	b.WriteString("To @mention someone, look up their real uid and display name from the ")
	b.WriteString("[Recent group messages] context or a group-roster tool if one is available, ")
	b.WriteString("then write the mention. ")
	b.WriteString(MentionFormatHint)
	b.WriteString("\n")
	b.WriteString("Real example: @[a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6:Alice].\n\n")
	return b.String()
}
