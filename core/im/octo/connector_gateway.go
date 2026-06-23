package octo

import (
	"time"

	"github.com/lml2468/octobuddy/core/gateway"
	"github.com/lml2468/octobuddy/core/groupctx"
)

// SetGateway attaches the gateway (done after construction to resolve the
// Connector-is-Sink-of-Gateway cycle).
func (c *Connector) SetGateway(g *gateway.Gateway) { c.gateway = g }

// MediaAuth returns the gateway hook that host-scopes the bot token on inbound
// media downloads: the Bearer token is sent only while the current hop is
// same-host as apiUrl, so a redirect to another host drops the credential
// (inbound.ts S1 per-hop Authorization scoping). Wire it via
// gateway.WithMediaAuth so the gateway can authenticate same-host media without
// embedding an IM-specific token.
func (c *Connector) MediaAuth() gateway.MediaAuth {
	return func(rawURL string) string {
		if !isSameHost(rawURL, c.rest.APIURL()) {
			return ""
		}
		tok := c.rest.Token()
		if tok == "" {
			return ""
		}
		return "Bearer " + tok
	}
}

// BotUID returns the bot's registered uid (empty before registration). Passed to
// gateway.WithGroupBackfill so cold-start backfill can filter the bot's own
// messages once the uid is known.
func (c *Connector) BotUID() string { return c.uid() }

// UserName returns the cached display name for uid, or "" if unknown. A miss
// kicks a background REST fetch so the next call can see a resolved value.
// The sender-name cache is also free-seeded from every inbound BotMessage,
// so most uids never trigger a network call.
func (c *Connector) UserName(uid string) string { return c.names.ResolveUser(uid) }

// SetNameResolvedHook registers a callback fired when a background name fetch
// resolves a DM peer (NameKindUser) or group/thread (NameKindChannel) to a new
// non-empty display name. The daemon wires it to re-broadcast session.upserted
// so a sidebar row that first painted with the bare id updates to the resolved
// name without waiting for the next turn (sessions.list's prewarm waits only a
// short budget while the fetch itself runs on a longer deadline). Set during
// bot setup, before Connect.
func (c *Connector) SetNameResolvedHook(fn func(kind NameKind, key, name string)) {
	c.names.SetResolvedHook(fn)
}

// ChannelName returns the cached display name for a channel id, or "" if
// unknown. For a bare group id it's the group's name; for a thread compound
// "<g>____<s>" it's the THREAD's own name (the parent group's name is a
// separate ChannelName call on the parent id). Composing the two for a
// breadcrumb / fallback label is the caller's job — projection layers do
// the composing to keep this cache shape simple and surface-agnostic.
// A miss kicks a background REST fetch.
func (c *Connector) ChannelName(channelID string) string {
	return c.names.ResolveChannel(channelID)
}

// PrewarmChannelNames synchronously fetches names for any of the given channel
// ids that aren't already cached, capped by timeout. Sessions.list calls this
// before building summaries so the first sidebar paint shows group names
// instead of bare ids.
func (c *Connector) PrewarmChannelNames(channelIDs []string, timeout time.Duration) {
	c.names.PrewarmChannels(channelIDs, timeout)
}

// PrewarmUserNames is the DM-peer counterpart of PrewarmChannelNames. DM rows
// usually get their name free-fed from inbound BotMessage.FromName, but a
// session with no inbound this restart (or one whose peer has only ever
// been spoken to, never spoken back) needs an explicit lookup or the sidebar
// row would stick at the bare peer uid.
func (c *Connector) PrewarmUserNames(uids []string, timeout time.Duration) {
	c.names.PrewarmUsers(uids, timeout)
}

// BackfillFetch pulls recent history for cold-start backfill (cc G4), adapting
// octo.HistoricalMessage to the IM-agnostic groupctx.BackfillMessage. limit<=0
// lets the REST client apply its default. Group-like channels only (the gateway
// calls this for group sessions, which includes threads — a thread is routed as
// router.ChannelGroup). Returns nil on any REST failure (the agent runs fine
// without history).
//
// A thread (CommunityTopic / 子区) channel id is the compound
// "<groupNo>____<shortId>", and messages/sync must be queried with
// channel_type=CommunityTopic for it: querying a thread id as a plain Group
// makes the server's membership check fail with not_group_member (the bot is a
// member of the parent group / the topic, never of a "group" by that compound
// id). Bare group ids stay ChannelGroup.
func (c *Connector) BackfillFetch(channelID string, limit int) []groupctx.BackfillMessage {
	chType := ChannelGroup
	if IsThreadChannelID(channelID) {
		chType = ChannelCommunityTopic
	}
	hist := c.rest.GetChannelMessages(c.ctx(), channelID, chType, limit)
	if len(hist) == 0 {
		return nil
	}
	out := make([]groupctx.BackfillMessage, 0, len(hist))
	for _, h := range hist {
		out = append(out, groupctx.BackfillMessage{
			FromUID:  h.FromUID,
			FromName: h.FromName,
			Content:  h.Content,
			Seq:      h.MessageSeq,
		})
	}
	return out
}

// SetMentionFreeGroups records the channel ids that respond without an @mention
// (G12). In those channels an unmentioned group message is handed to the gateway
// (the router applies the mention-free + bot-loop policy) rather than being
// observed-only. Must be called before Run.
func (c *Connector) SetMentionFreeGroups(channelIDs []string) {
	if len(channelIDs) == 0 {
		c.mentionFree = nil
		return
	}
	m := make(map[string]bool, len(channelIDs))
	for _, id := range channelIDs {
		if id != "" {
			m[id] = true
		}
	}
	c.mentionFree = m
}
