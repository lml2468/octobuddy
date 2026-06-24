package trigger

// AIBroadcastPolicy governs the @AI broadcast trigger path. The default for
// new deployments is Deny — pure-@AI (UIDs empty, AIs=truthy) no longer
// triggers the bot, fixing the WuKongIM follow-up @AI-truthy bug (issue
// #105). Operators who relied on the legacy behavior can opt back to Allow,
// or scope it to a per-channel allowlist.
type AIBroadcastPolicy string

const (
	// AIBroadcastDeny refuses the pure-@AI path entirely. Use for new bots
	// or any bot in a group where WuKongIM clients (or any client) might set
	// AIs=truthy on follow-up messages without an explicit re-@.
	AIBroadcastDeny AIBroadcastPolicy = "deny"
	// AIBroadcastAllowlist refuses @AI unless the channel id is in
	// AIBroadcastAllowlist — used for "@AI bot only on the support channel"
	// style scoping.
	AIBroadcastAllowlist AIBroadcastPolicy = "allowlist"
	// AIBroadcastAllow keeps the legacy behavior (any @AI fires the bot).
	// Provided for explicit opt-in migration; not a recommended default.
	AIBroadcastAllow AIBroadcastPolicy = "allow"
)

// Valid reports whether p is a recognized policy constant. Callers fall back
// to a safer default on invalid values rather than panicking.
func (p AIBroadcastPolicy) Valid() bool {
	switch p {
	case AIBroadcastDeny, AIBroadcastAllowlist, AIBroadcastAllow:
		return true
	}
	return false
}

// Policy is the runtime configuration consumed by a Classifier. Pure value
// type: daemon constructs it once per bot at startup, classifier treats it
// as immutable. Hot-reload would publish a new Policy under the connector's
// own lock, not mutate this one.
type Policy struct {
	// BotUID is this bot's own server uid. Used to match explicit @bot
	// mentions and to mark "reply-to-me" quotes.
	BotUID string
	// Grantor identifies the persona-clone's grantor (empty = regular bot).
	// Triggers the persona widening rules in DefaultClassifier and the OBO
	// trust gate.
	Grantor PolicyGrantor
	// MentionFreeGroups is the set of channel ids that respond without an
	// @mention (G12). Owned ONLY here in the trigger pipeline (the old
	// router/connector double-copy is gone — see issue #105 缺陷 2).
	MentionFreeGroups map[string]bool
	// AIBroadcast governs the pure-@AI path (see AIBroadcastPolicy doc).
	AIBroadcast AIBroadcastPolicy
	// AIBroadcastAllowlist scopes AIBroadcast=allowlist to specific channels.
	AIBroadcastAllowlist map[string]bool
	// ReplyToBotEnabled lifts a quote-reply to one of the bot's own prior
	// messages into a trigger. Default true in new configs so the natural
	// "continue the thread" UX still works under AIBroadcastDeny.
	ReplyToBotEnabled bool
}

// effectiveAIBroadcast returns a valid AIBroadcastPolicy, defaulting an
// unset/invalid value to Deny (the safe, bug-free default).
func (p Policy) effectiveAIBroadcast() AIBroadcastPolicy {
	if p.AIBroadcast.Valid() {
		return p.AIBroadcast
	}
	return AIBroadcastDeny
}
