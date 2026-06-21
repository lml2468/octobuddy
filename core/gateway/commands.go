// In-chat slash commands, the Go port of cc-channel's src/commands.ts.
//
// Users control their session without leaving the chat:
//
//	/reset   — clear this session's conversation history
//	/config  — show the effective per-session settings
//	/help    — list available commands
//
// Commands are matched on the FIRST line of the inbound body AFTER the router
// has stripped any leading @bot mention (so `@bot /reset` works in groups).
// Matching is case-insensitive and tolerant of surrounding whitespace.
//
// A command is scoped to the sessionKey of the message. In a group the session
// is shared per channel, so /reset clears the WHOLE group's conversation
// history (every member shares one session), not just the caller's. In a DM it
// clears that peer's history. /reset does NOT clear long-term auto-memory.
//
// Commands are handled in the gateway AFTER the router's rate limit has been
// applied (they ride the accepted-turn path), so a user who has exhausted their
// token bucket cannot run /reset or /help until it refills — control commands
// are rate-limited like normal messages. This is intentional: a flooder must not
// get a rate-limit bypass via slash commands (commands.ts header).
package gateway

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// commandRE matches a leading slash command on the first line. The command name
// must be followed by a TOKEN BOUNDARY — end-of-line or whitespace — before any
// args. Without this, path/route-like text such as `/reset/foo`, `/config.json`,
// or `/help.md` would parse as the bare command and could trigger a destructive
// action (/reset). Anything glued to the name (`/foo.bar`, `/a/b`) is NOT a
// command and falls through to the normal agent pipeline (commands.ts parseCommand).
var commandRE = regexp.MustCompile(`^/([a-zA-Z][a-zA-Z0-9_-]*)(?:\s+(.*))?$`)

// parsedCommand is the lowercased command name (without the leading slash) and
// the trimmed argument string.
type parsedCommand struct {
	name string
	args string
}

// parseCommand extracts the command token from a message body, or returns
// (_, false) when the text does not start with a slash command. Only the first
// whitespace-delimited token on the first line is considered, so a message that
// merely mentions "/reset" mid-sentence is NOT treated as a command — it must
// lead. Mirrors commands.ts parseCommand.
func parseCommand(body string) (parsedCommand, bool) {
	firstLine, _, _ := strings.Cut(body, "\n")
	firstLine = strings.TrimSpace(firstLine)
	m := commandRE.FindStringSubmatch(firstLine)
	if m == nil {
		return parsedCommand{}, false
	}
	return parsedCommand{name: strings.ToLower(m[1]), args: strings.TrimSpace(m[2])}, true
}

// helpText is the human-readable list of supported commands (commands.ts HELP_TEXT).
const helpText = "Available commands:\n" +
	"• `/reset` — clear the conversation history for this session (the whole group, in a group chat); does not clear long-term memory\n" +
	"• `/config` — show the current session settings\n" +
	"• `/help` — show this message"

// resetReply is the confirmation sent after /reset (commands.ts).
const resetReply = "✓ Conversation history cleared (long-term memory is kept)."

// renderConfig renders the effective, non-sensitive per-session configuration.
// Deliberately omits secrets (tokens, gateway URLs) — this reply is visible to
// any user who can message the bot (commands.ts renderConfig). Go's context
// window is char-budgeted (MaxContextChars) rather than message-counted, so the
// last line reports that budget in place of cc's historyLimit.
func (g *Gateway) renderConfig() string {
	model := g.model
	if model == "" {
		model = "(driver default)"
	}
	return fmt.Sprintf(
		"Current settings:\n• model: %s\n• rateLimit: %d req/min\n• contextBudget: %d chars",
		model, g.maxPerMinute, g.contextChars,
	)
}

// handleCommand tries to handle body as a slash command for sessionKey. Returns
// (reply, true) when body was a recognized (or unknown-but-leading-slash)
// command and any side effect has been performed; ("", false) when body is not
// a command and the caller should proceed with the normal agent pipeline.
// Mirrors commands.ts handleCommand.
func (g *Gateway) handleCommand(body, sessionKey string) (string, bool) {
	cmd, ok := parseCommand(body)
	if !ok {
		return "", false
	}

	switch cmd.name {
	case "reset":
		// Scoped to THIS sessionKey. In a group the session is shared per channel,
		// so this clears the whole group's conversation history; in a DM it clears
		// that peer's. Long-term auto-memory is NOT cleared. Clearing the resume id
		// is what actually breaks LLM continuity (the next turn starts fresh instead
		// of resuming the just-cleared conversation); deleting the stored messages
		// keeps the operator/control-bus history view consistent with that reset
		// (cc-channel store.deleteSession clears both). Each side effect is logged
		// but not fatal — a half-cleared reset is better than a refused command.
		if err := g.store.ClearResume(sessionKey); err != nil {
			fmt.Fprintf(os.Stderr, "[gateway] /reset clear resume %s: %v\n", sessionKey, err)
		}
		if err := g.store.ClearHistory(sessionKey); err != nil {
			fmt.Fprintf(os.Stderr, "[gateway] /reset clear history %s: %v\n", sessionKey, err)
		}
		return resetReply, true
	case "config":
		return g.renderConfig(), true
	case "help":
		return helpText, true
	default:
		// A leading-slash token we don't recognize. Report it rather than silently
		// forwarding to the agent, so typos are visible (commands.ts default case).
		return fmt.Sprintf("Unknown command: /%s\n\n%s", cmd.name, helpText), true
	}
}
