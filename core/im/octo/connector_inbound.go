package octo

import (
	"strings"

	"github.com/lml2468/octobuddy/core/router"
	"github.com/lml2468/octobuddy/core/safety"
)

// onInbound maps a decoded BotMessage to a router.InboundMessage and feeds the
// gateway. Drops the bot's own messages and streaming partials; every other
// payload type is rendered to LLM-facing text by ResolveContent (content.go),
// and image/file payloads also surface media Attachments the gateway
// materializes into the session cwd (inbound.ts G1).
//
// Persona-clone path (openclaw OBO, inbound.ts): when this connector is a
// persona clone, the group trigger gate is widened (an @grantor / @所有人
// mention triggers a turn), the OBO v2 relevance filter drops irrelevant @AI
// fan-out BEFORE any session state is recorded, and the reply target carries
// on_behalf_of so the server presents the reply as the grantor.
func (c *Connector) onInbound(m BotMessage) {
	uid := c.uid()
	if m.FromUID == uid {
		return // ignore our own messages
	}
	// Suppress streaming partial updates (inbound.ts settingStreamOn / G21): a
	// streamOn message is an in-progress edit; only the final (streamOn=false)
	// message carries the settled content. Routing partials would feed the agent
	// half-typed text and re-fire turns on every keystroke. Filtered FIRST so
	// the per-message name-cache work below doesn't run on every keystroke.
	if m.StreamOn {
		return
	}

	inbound, key, tgt, ok := c.prepareInboundTurn(m, uid)
	if !ok {
		return
	}
	// Per-turn target travels with the queued turn so drainTurns can set
	// c.targets[key] AT pop-time — the prior contract had onInbound write the
	// global map directly here, which raced cron's RegisterReplyTarget. The
	// reroute is computed once here so it isn't recomputed on every target
	// read.
	tgt = c.rerouteInboundReplyTarget(key, tgt)
	// NB: also wrote c.targets[key] here "for the persona tests" —
	// that put the race back, just for inbound-during-a-mid-flight-turn
	// instead of cron-vs-inbound. If the gateway's in-flight Handle for a
	// PRIOR turn emits OnReply / onToolProgress / startTyping after this
	// onInbound runs but before its drainTurns pop, those callbacks read
	// the wrong target. Map writes now ONLY happen in drainTurns
	// (sole-writer invariant); the persona tests probe the queued item.

	// Acknowledge receipt (fire-and-forget) once we've decided to process it.
	c.sendReadReceipt(m)

	c.observeOrEnqueueInboundTurn(key, inbound, tgt, m.ChannelID, m.Payload.Reply)
}

func (c *Connector) prepareInboundTurn(m BotMessage, botUID string) (router.InboundMessage, string, replyTarget, bool) {
	c.hydrateInboundNames(&m)

	// ResolveContent covers every type and sanitizes untrusted names/bodies that
	// land in labels.
	baseText := c.resolveInboundText(m.Payload)
	if baseText == "" {
		return router.InboundMessage{}, "", replyTarget{}, false
	}

	// Only trust OBO fields when the message is sent by the configured grantor.
	oboV2 := c.isTrustedOBORelay(m)
	if oboV2 && !c.persona.Relevant(m.PersonaMention()) {
		c.logf("OBO v2 skipped — message not relevant to persona")
		return router.InboundMessage{}, "", replyTarget{}, false
	}

	inbound := c.buildInboundMessage(m, botUID, baseText)
	key, err := inbound.SessionKey()
	if err != nil {
		return router.InboundMessage{}, "", replyTarget{}, false
	}

	// OBO v2 replies to the origin channel as the grantor. Group persona
	// trigger-as-grantor replies in the same group as the grantor.
	tgt := c.inboundReplyTarget(m, botUID, inbound.ChannelType, oboV2)
	return inbound, key, tgt, true
}

func (c *Connector) rerouteInboundReplyTarget(key string, tgt replyTarget) replyTarget {
	if tgt.channelType == ChannelDM {
		return tgt
	}
	if rerouted, did := RerouteTarget(key, tgt.channelID); did {
		c.logf("reroute reply for thread session %s: target %q -> %q (issue #98)", key, tgt.channelID, rerouted)
		tgt.channelID = rerouted
		tgt.channelType = ChannelCommunityTopic
	}
	return tgt
}

func (c *Connector) observeOrEnqueueInboundTurn(key string, inbound router.InboundMessage, tgt replyTarget, channelID string, reply *ReplyPayload) {
	// A group message that doesn't trigger the bot is background context, not a
	// turn: observe it so it becomes a later @-mention's delta. (The router
	// would drop it anyway; observing first preserves group context.) Background
	// context is stored history, so it carries the plain resolved text WITHOUT
	// the quoted-reply prefix. Observe is a fast in-memory cache write, so it runs
	// inline (not worth a worker goroutine). A nil gateway (tests) skips it.
	//
	// Exception (G12): in a mention-free channel an unmentioned message IS a turn
	// — hand it to the gateway so the router applies the mention-free + bot-loop
	// policy. runTurn caches it into group context itself, so do NOT also Observe.
	if c.shouldObserveBackground(inbound, channelID) {
		if c.gateway != nil {
			c.gateway.Observe(inbound)
		}
		return
	}
	// Prepend the quoted-reply context to the CURRENT turn only (never stored
	// history): the sender quoted a prior message, so give the agent that
	// context fenced ahead of the real request (inbound.ts quotePrefix).
	c.applyQuotePrefix(&inbound, reply)
	// Dispatch the turn on the per-key worker so the WS read loop is not blocked
	// for the whole (possibly multi-minute) turn. The router still serializes
	// same-session turns; the per-key queue guarantees they reach the router in
	// arrival order despite running on a goroutine. drainTurns skips dispatch
	// when gateway is nil (tests), but the queue is still populated so the
	// persona tests can assert via peekQueuedTarget.
	c.enqueueTurn(key, inbound, tgt)
}

func (c *Connector) hydrateInboundNames(m *BotMessage) {
	// WuKongIM RECV packets carry only fromUid, not a display name. Kick a
	// background fetch via the cache (non-blocking — ResolveUser returns "" on
	// miss and the next message from the same uid sees it cached) and fall back
	// to the cached value if it happens to be there already. The receive goroutine
	// MUST NOT block on REST: a slow / unreachable name service would stall ALL
	// inbound for this bot. First sight of an unseen sender lands in the chat
	// with senderName empty (the GUI falls back to senderUid for the bubble
	// label) — subsequent messages from the same uid carry the resolved name, and
	// the persisted history row gets backfilled on the next history fetch via the
	// warmed cache.
	if m.FromName == "" && m.FromUID != "" {
		m.FromName = c.names.ResolveUser(m.FromUID)
	}
	// Free-feed the name cache (no-op if FromName is empty). Also kick the
	// channel-name fetch for groups so the sidebar can show it next render.
	c.names.LearnUser(m.FromUID, m.FromName)
	if m.ChannelType == ChannelGroup || m.ChannelType == ChannelCommunityTopic {
		c.names.ResolveChannel(m.ChannelID)
	}
}

func (c *Connector) resolveInboundText(payload MessagePayload) string {
	resolved := ResolveContent(payload, c.rest.APIURL())
	return strings.TrimSpace(resolved.Text)
}

func (c *Connector) isTrustedOBORelay(m BotMessage) bool {
	return c.persona.Configured() &&
		m.Payload.OBOOriginChannelID != "" &&
		oboRespondAs(m.Payload) != "" &&
		m.FromUID == c.persona.UID
}

func (c *Connector) shouldObserveBackground(inbound router.InboundMessage, channelID string) bool {
	return inbound.ChannelType == router.ChannelGroup && !inbound.Mentioned && !c.mentionFree[channelID]
}

func (c *Connector) applyQuotePrefix(inbound *router.InboundMessage, reply *ReplyPayload) {
	if prefix := resolveQuotePrefix(reply, c.rest.APIURL()); prefix != "" {
		inbound.Text = prefix + inbound.Text
	}
}

func (c *Connector) buildInboundMessage(m BotMessage, botUID, text string) router.InboundMessage {
	// A CommunityTopic (thread / 子区) is group-like for routing: its channel id
	// is the compound "<groupNo>____<shortId>", so it lands in its OWN session
	// (distinct from the parent group and sibling threads) while membership and
	// the mention gate are inherited from the parent group. See thread.go and
	// openclaw inbound.ts thread routing.
	chType := router.ChannelDM
	if m.ChannelType == ChannelGroup || m.ChannelType == ChannelCommunityTopic {
		chType = router.ChannelGroup
	}

	// Trigger gate: persona-aware for clones (an @grantor / @所有人 mention is a
	// call to the clone); a plain @bot / @AI mention otherwise.
	return router.InboundMessage{
		FromUID:     m.FromUID,
		FromName:    m.FromName,
		ChannelID:   m.ChannelID,
		ChannelType: chType,
		Text:        text,
		Attachments: c.resolveAttachments(m.Payload),
		MessageSeq:  int64(m.MessageSeq),
		Mentioned:   m.Triggers(botUID, c.persona),
	}
}

func (c *Connector) inboundReplyTarget(m BotMessage, botUID string, chType router.ChannelType, oboV2 bool) replyTarget {
	tgt := replyTarget{channelID: m.ChannelID, channelType: m.ChannelType}
	if oboV2 {
		return oboReplyTarget(m.Payload, c.persona.UID)
	}
	if chType == router.ChannelGroup &&
		c.persona.TriggeredAsGrantor(m.PersonaMention(), m.ExplicitlyMentionsBot(botUID)) {
		tgt.onBehalfOf = c.persona.UID
	}
	return tgt
}

// oboRespondAs resolves the grantor uid the payload claims to respond as,
// preferring obo_respond_as over obo_grantor_uid (openclaw inbound.ts L2104).
func oboRespondAs(p MessagePayload) string {
	if p.OBORespondAs != "" {
		return p.OBORespondAs
	}
	return p.OBOGrantorUID
}

// oboReplyTarget derives the OBO v2 reply destination from a (grantor-trusted)
// payload (openclaw inbound.ts ~L2305-2326). DM-relay origin → reply to the
// original sender's uid; group/thread → reply to the origin group. The reply
// always carries on_behalf_of=grantor (the trusted configured grantor uid, NOT
// the payload's respond_as).
func oboReplyTarget(p MessagePayload, grantorUID string) replyTarget {
	chType := ChannelGroup
	if p.OBOOriginChannelType != nil {
		chType = ChannelType(*p.OBOOriginChannelType)
	}
	channelID := p.OBOOriginChannelID
	if chType == ChannelDM && p.OBOOriginFromUID != "" {
		// DM: the bot is only friends with the grantor; reply to the original
		// sender via on_behalf_of=grantor, which bypasses the bot-friend gate.
		channelID = p.OBOOriginFromUID
	}
	return replyTarget{channelID: channelID, channelType: chType, onBehalfOf: grantorUID}
}

// resolveAttachments extracts downloadable media/file attachments from a payload
// (image/GIF/file). LLM-facing text rendering is handled by ResolveContent
// (content.go); this only surfaces the URLs the gateway materializes into the
// session cwd. Media URLs are host-validated via buildMediaURL (inbound.ts G1).
func (c *Connector) resolveAttachments(p MessagePayload) []router.Attachment {
	apiURL := c.rest.APIURL()
	switch p.Type {
	case MsgImage, MsgGIF:
		full := buildMediaURL(p.URL, apiURL)
		if full == "" {
			return nil
		}
		return []router.Attachment{{Kind: router.AttachmentImage, URL: full}}
	case MsgFile:
		// SECURITY: p.Name is user-controlled; sanitize before it flows into the
		// <file_content name="…"> attribute the gateway writes.
		filename := safety.SanitizeDisplayName(p.Name, "未知文件")
		full := buildMediaURL(p.URL, apiURL)
		if full == "" {
			return nil
		}
		return []router.Attachment{{Kind: router.AttachmentFile, URL: full, Name: filename, Size: p.Size}}
	default:
		return nil // Voice/Video/Location/etc. carry no downloadable attachment
	}
}
