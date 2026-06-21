package octo

import "strings"

// Thread (sub-topic / 子区) channel-id helpers, ported from openclaw-channel-octo
// (src/group-md.ts extractParentGroupNo/extractThreadShortId/isThreadChannelId,
// src/constants.ts THREAD_ID_SEPARATOR, src/actions.ts issue #98 auto-reroute).
//
// Octo encodes a CommunityTopic (thread) channel id as "<groupNo>____<shortId>":
// the parent group number, the four-underscore separator, then the thread's
// short id. A bare group channel id has no separator. These functions are pure
// string operations with no Octo specifics beyond the separator, so they are
// safe to use anywhere the connector or router needs to reason about thread refs.
//
// Routing semantics: a thread message is group-like (permission/membership is
// inherited from the parent group via the parent group_no), but it gets its OWN
// session because its channel id — the compound "<groupNo>____<shortId>" — is
// what the router hashes into a sessionKey. That compound id never equals the
// parent group's bare "<groupNo>", so a thread's conversation history and
// sandbox partition are automatically isolated from the parent group and from
// sibling threads, without any special-casing in router.SessionKey.

// ThreadIDSeparator separates the parent group_no from the thread short_id in a
// CommunityTopic channel id. Four underscores, matching openclaw's
// constants.ts THREAD_ID_SEPARATOR.
const ThreadIDSeparator = "____"

// IsThreadChannelID reports whether channelID is a thread (CommunityTopic) ref,
// i.e. it carries the "<groupNo>____<shortId>" shape. Mirrors group-md.ts
// isThreadChannelId.
func IsThreadChannelID(channelID string) bool {
	return strings.Contains(channelID, ThreadIDSeparator)
}

// ExtractParentGroupNo returns the parent group number of channelID. For a
// thread ref ("<groupNo>____<shortId>") it returns the "<groupNo>" prefix; for a
// bare group id it returns the id unchanged. Mirrors group-md.ts
// extractParentGroupNo (split on the FIRST separator occurrence).
func ExtractParentGroupNo(channelID string) string {
	if i := strings.Index(channelID, ThreadIDSeparator); i >= 0 {
		return channelID[:i]
	}
	return channelID
}

// extractThreadShortID returns the thread short id of channelID, or "" when
// channelID is not a thread ref (no separator) or has an empty short-id portion
// ("<groupNo>____"). Mirrors group-md.ts extractThreadShortId, collapsing its
// null/"" distinction to "" since Go callers test for emptiness either way.
func extractThreadShortID(channelID string) string {
	i := strings.Index(channelID, ThreadIDSeparator)
	if i < 0 {
		return ""
	}
	return channelID[i+len(ThreadIDSeparator):]
}

// RerouteTarget implements openclaw's issue #98 auto-reroute on the send path:
// when the bot's active session is a thread but it addresses the bare PARENT
// group of that same thread, the send is rerouted back to the thread so the
// reply lands in the sub-topic the user is talking in — not the parent group.
//
// currentChannelID is the channel the active session is bound to (a thread ref
// when inside a thread session). targetChannelID is where the send is currently
// aimed. The returned (channelID, rerouted) is the channel to actually send to.
//
// Reroute fires only when ALL hold (mirroring actions.ts):
//   - currentChannelID is a thread ref (bot is in a thread session), and
//   - targetChannelID is NOT itself a thread ref (an explicit thread target
//     always wins — never overridden), and
//   - targetChannelID equals the parent group_no of the current thread (the
//     bare-parent mistake). Cross-group sends are left untouched.
//
// Any explicit thread target, or a target in a different group, passes through
// unchanged.
func RerouteTarget(currentChannelID, targetChannelID string) (channelID string, rerouted bool) {
	if !IsThreadChannelID(currentChannelID) {
		return targetChannelID, false
	}
	if IsThreadChannelID(targetChannelID) {
		return targetChannelID, false
	}
	if targetChannelID == ExtractParentGroupNo(currentChannelID) {
		return currentChannelID, true
	}
	return targetChannelID, false
}
