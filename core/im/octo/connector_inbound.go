package octo

import (
	"strings"

	"github.com/lml2468/octobuddy/core/clog"
	"github.com/lml2468/octobuddy/core/router"
	"github.com/lml2468/octobuddy/core/safety"
	"github.com/lml2468/octobuddy/core/trigger"
)

// onInbound maps a decoded BotMessage to a router.InboundMessage and
// feeds the gateway. The connector classifies ONCE then either enqueues
// a reply turn, observes inline, or drops silently (OBO-irrelevant fan-out).
func (c *Connector) onInbound(m BotMessage) {
	uid := c.uid()
	if m.FromUID == uid {
		return // ignore our own messages
	}
	// System/metadata frames (typing, read-receipt, mention-chip detached
	// follow-on) carry no sender uid and have no user-visible meaning.
	// Logged at debug so a future content type that legitimately lacks
	// from_uid surfaces rather than vanishing silently.
	if m.FromUID == "" {
		clog.For("octo").Debug("inbound dropped: empty from_uid",
			"type", m.Payload.Type, "channel", m.ChannelID)
		return
	}
	// Suppress streaming partial updates. Filtered BEFORE name-cache work
	// so we don't pay per-keystroke.
	if m.StreamOn {
		return
	}
	// System / control-plane payloads must NEVER reach the trigger classifier
	// (that's the group-announcement false-trigger bug). Two shapes:
	//   1. true system TYPES (CMD / 1000-2000) — Type.IsSystem();
	//   2. Text(=1) notifications carrying an `event` envelope (group_md_updated,
	//      mention_pref_updated, …) that ALSO @-mention the bot — IsControlEvent.
	// Route both to the deterministic handler, which may mirror GROUP.md but
	// never enqueues an LLM turn.
	if m.Payload.Type.IsSystem() || m.Payload.IsControlEvent() {
		c.handleSystemMessage(m)
		return
	}

	// Tier-1 inbound dedup (in-memory, O(1), no DB): drop a same-process
	// redelivery (lost ack / reconnect replay) before it fans out to the observe
	// or turn path. The authoritative cross-restart dedup for the turn path is
	// tier-2 (store-backed) on the drainTurns worker. Placed AFTER the
	// own/empty/stream/system drops so those cheap filters run first and a
	// control frame never consumes a dedup slot.
	if !c.memFirstSeen(m.MessageID) {
		return
	}

	inbound, key, tgt, ok := c.prepareInboundTurn(m, uid)
	if !ok {
		return
	}
	// Per-turn target travels with the queued turn so drainTurns sets
	// c.targets[key] at pop-time — avoids racing cron's reply target
	// when both fire on the same session.
	tgt = c.rerouteInboundReplyTarget(key, tgt)

	c.sendReadReceipt(m)
	c.dispatchInbound(key, inbound, tgt, m.ChannelID, m.Payload.Reply, m.MessageID)
}

// prepareInboundTurn classifies the inbound and returns the
// router.InboundMessage to enqueue / observe. ok=false drops silently
// (empty text, unroutable, or OBO-irrelevant — the leak guard fires
// BEFORE any session state is touched).
func (c *Connector) prepareInboundTurn(m BotMessage, botUID string) (router.InboundMessage, string, replyTarget, bool) {
	c.hydrateInboundNames(&m)

	baseText := c.resolveInboundText(m.Payload)
	if baseText == "" {
		return router.InboundMessage{}, "", replyTarget{}, false
	}

	policy, classifier := c.loadPolicyAndClassifier()
	canonical := c.buildCanonicalInbound(m, baseText, botUID)
	decision := classifier.Classify(canonical, policy)

	if decision.Reason == trigger.ReasonOBOIrrelevant {
		c.logf("OBO v2 skipped — message not relevant to persona (channel=%s from=%s)",
			m.ChannelID, m.FromUID)
		return router.InboundMessage{}, "", replyTarget{}, false
	}

	inbound := c.inboundMessageFromCanonical(canonical, m, baseText, &decision)
	key, err := inbound.SessionKey()
	if err != nil {
		return router.InboundMessage{}, "", replyTarget{}, false
	}

	tgt := c.targetFromDecision(m, decision)
	return inbound, key, tgt, true
}

func (c *Connector) buildCanonicalInbound(m BotMessage, baseText, botUID string) trigger.CanonicalInbound {
	channel := trigger.ChannelDM
	if m.ChannelType == ChannelGroup || m.ChannelType == ChannelCommunityTopic {
		channel = trigger.ChannelGroup
	}
	return trigger.CanonicalInbound{
		Source:     trigger.SourceUser,
		Channel:    channel,
		ChannelID:  m.ChannelID,
		FromUID:    m.FromUID,
		FromName:   m.FromName,
		Text:       baseText,
		MessageSeq: int64(m.MessageSeq),
		Mention:    m.TriggerMention(),
		ReplyTo:    c.replyContextFromPayload(m.Payload.Reply, botUID),
		OBO:        m.TriggerOBO(),
		Protocol:   "octo",
	}
}

// replyContextFromPayload converts an Octo Reply payload to the IM-
// agnostic trigger.ReplyContext. TargetIsBot is pre-computed so the
// classifier never needs the bot uid for this rule.
func (c *Connector) replyContextFromPayload(reply *ReplyPayload, botUID string) *trigger.ReplyContext {
	if reply == nil || reply.FromUID == "" {
		return nil
	}
	return &trigger.ReplyContext{
		TargetFromUID: reply.FromUID,
		TargetIsBot:   botUID != "" && reply.FromUID == botUID,
	}
}

func (c *Connector) inboundMessageFromCanonical(canonical trigger.CanonicalInbound, m BotMessage, baseText string, decision *trigger.TriggerDecision) router.InboundMessage {
	// A CommunityTopic (thread) is group-like for routing: its channel id
	// is the compound "<groupNo>____<shortId>", so it lands in its OWN
	// session — distinct from the parent group and sibling threads —
	// while membership and the mention gate are inherited from the parent.
	chType := router.ChannelDM
	if canonical.Channel == trigger.ChannelGroup {
		chType = router.ChannelGroup
	}
	return router.InboundMessage{
		FromUID:     m.FromUID,
		FromName:    m.FromName,
		ChannelID:   m.ChannelID,
		ChannelType: chType,
		Text:        baseText,
		Attachments: c.resolveAttachments(m.Payload),
		MessageSeq:  int64(m.MessageSeq),
		Source:      trigger.SourceUser,
		Trigger:     decision,
	}
}

// targetFromDecision derives the reply target from the classifier's
// ReplyRouting plus the IM channel coords. OBO v2 reroutes redirect to
// the origin channel; on_behalf_of stamps the grantor uid.
func (c *Connector) targetFromDecision(m BotMessage, decision trigger.TriggerDecision) replyTarget {
	tgt := replyTarget{channelID: m.ChannelID, channelType: m.ChannelType}
	if decision.ReplyRouting.HasOBOReroute() {
		rerouteKind := ChannelType(decision.ReplyRouting.OBORerouteKind)
		if rerouteKind == 0 {
			// Default to group when the origin omits an explicit kind.
			rerouteKind = ChannelGroup
		}
		tgt.channelID = decision.ReplyRouting.OBORerouteChannelID
		tgt.channelType = rerouteKind
	}
	tgt.onBehalfOf = decision.ReplyRouting.OnBehalfOf
	return tgt
}

func (c *Connector) rerouteInboundReplyTarget(key string, tgt replyTarget) replyTarget {
	if tgt.channelType == ChannelDM {
		return tgt
	}
	if rerouted, did := RerouteTarget(key, tgt.channelID); did {
		c.logf("reroute reply for thread session %s: target %q -> %q", key, tgt.channelID, rerouted)
		tgt.channelID = rerouted
		tgt.channelType = ChannelCommunityTopic
	}
	return tgt
}

// dispatchInbound routes the classified inbound: observations skip the
// session lock and write straight to groupctx; reply-warranting decisions
// enqueue for the per-session worker.
func (c *Connector) dispatchInbound(key string, inbound router.InboundMessage, tgt replyTarget, channelID string, reply *ReplyPayload, messageID string) {
	if inbound.ShouldObserve() {
		// Group-context background; stored history carries the plain
		// resolved text WITHOUT the quoted-reply prefix.
		if c.gateway != nil {
			c.gateway.Observe(inbound)
		}
		return
	}
	// Quoted-reply context fences ahead of the real request for THIS turn
	// only; never persisted in history.
	c.applyQuotePrefix(&inbound, reply)
	c.enqueueTurn(key, inbound, tgt, messageID)
}

// hydrateInboundNames resolves uid → display name via the cache without
// blocking on REST: a slow name service must not stall every inbound.
// First sight of an unseen sender lands with empty name (GUI falls back
// to uid); subsequent messages carry the resolved value once the cache
// warms.
func (c *Connector) hydrateInboundNames(m *BotMessage) {
	if m.FromName == "" && m.FromUID != "" {
		m.FromName = c.names.ResolveUser(m.FromUID)
	}
	c.names.LearnUser(m.FromUID, m.FromName)
	if m.ChannelType == ChannelGroup || m.ChannelType == ChannelCommunityTopic {
		c.names.ResolveChannel(m.ChannelID)
	}
}

func (c *Connector) resolveInboundText(payload MessagePayload) string {
	resolved := ResolveContent(payload, c.rest.APIURL())
	return strings.TrimSpace(resolved.Text)
}

func (c *Connector) applyQuotePrefix(inbound *router.InboundMessage, reply *ReplyPayload) {
	if prefix := resolveQuotePrefix(reply, c.rest.APIURL()); prefix != "" {
		inbound.Text = prefix + inbound.Text
	}
}

// oboRespondAs resolves the grantor uid the payload claims to respond as,
// preferring obo_respond_as over obo_grantor_uid.
func oboRespondAs(p MessagePayload) string {
	if p.OBORespondAs != "" {
		return p.OBORespondAs
	}
	return p.OBOGrantorUID
}

// resolveAttachments extracts downloadable media for image / GIF / file
// payloads. LLM-facing text rendering is handled by ResolveContent;
// this only surfaces the URLs the gateway materializes into the session
// cwd. Media URLs are host-validated by buildMediaURL.
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
		// p.Name is user-controlled; sanitize before it flows into the
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
