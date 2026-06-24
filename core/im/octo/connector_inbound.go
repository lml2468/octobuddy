package octo

import (
	"strings"

	"github.com/lml2468/octobuddy/core/router"
	"github.com/lml2468/octobuddy/core/safety"
	"github.com/lml2468/octobuddy/core/trigger"
)

// onInbound maps a decoded BotMessage to a router.InboundMessage and feeds
// the gateway. Drops the bot's own messages and streaming partials; every
// other payload type is rendered to LLM-facing text by ResolveContent
// (content.go), and image/file payloads also surface media Attachments the
// gateway materializes into the session cwd (inbound.ts G1).
//
// Trigger pipeline (issue #105): the connector translates the wire payload
// to a trigger.CanonicalInbound, runs the classifier ONCE, and either
// (a) hands the message to the per-session worker for a turn, (b) calls
// gw.Observe inline for an observation-only message, or (c) drops silently
// for an OBO-irrelevant fan-out. The legacy split between pre-gate
// Observe and post-gate retroactive Observe is gone — gateway is the
// single Observe entry point now.
func (c *Connector) onInbound(m BotMessage) {
	uid := c.uid()
	if m.FromUID == uid {
		return // ignore our own messages
	}
	// Drop system/metadata frames at the door: a payload with no sender
	// uid is a WuKongIM system event (typing, read-receipt, mention-chip
	// follow-on with detached metadata, etc.), not a user message. Without
	// this filter they would still classify (correctly, as observation)
	// but spam the audit stream with from="" / text="[消息]" rows that
	// have no user-visible meaning.
	if m.FromUID == "" {
		return
	}
	// Suppress streaming partial updates (inbound.ts settingStreamOn / G21).
	// Filtered FIRST so the per-message name-cache work below doesn't run on
	// every keystroke.
	if m.StreamOn {
		return
	}

	inbound, key, tgt, ok := c.prepareInboundTurn(m, uid)
	if !ok {
		return
	}
	// Per-turn target travels with the queued turn so drainTurns can set
	// c.targets[key] AT pop-time — the prior contract had onInbound write
	// the global map directly here, which raced cron's RegisterReplyTarget.
	// The reroute is computed once here so it isn't recomputed on every
	// target read.
	tgt = c.rerouteInboundReplyTarget(key, tgt)

	// Acknowledge receipt (fire-and-forget) once we've decided to process it.
	c.sendReadReceipt(m)

	c.dispatchInbound(key, inbound, tgt, m.ChannelID, m.Payload.Reply)
}

// prepareInboundTurn translates the wire payload to a trigger.CanonicalInbound,
// classifies, and returns the router.InboundMessage to enqueue or observe.
// Returns ok=false to drop silently (empty text, unroutable, or OBO
// irrelevant — the openclaw R10 leak guard, BEFORE any session state).
func (c *Connector) prepareInboundTurn(m BotMessage, botUID string) (router.InboundMessage, string, replyTarget, bool) {
	c.hydrateInboundNames(&m)

	baseText := c.resolveInboundText(m.Payload)
	if baseText == "" {
		return router.InboundMessage{}, "", replyTarget{}, false
	}

	policy, classifier := c.loadPolicyAndClassifier()
	// Policy.BotUID is seeded from cfg.BotID at startup, but the IM-side
	// @-mention payload carries the SERVER-registered uid (set post-
	// Register via setUID). Without this override the classifier would
	// never match an @bot mention in production (the regression bot.go's
	// note flagged: "policy.BotUID is the configured bot id, not the
	// post-register uid"). The caller already resolved c.uid() into
	// botUID — reuse it instead of re-acquiring c.mu on the hot path.
	if botUID != "" {
		policy.BotUID = botUID
	}
	canonical := c.buildCanonicalInbound(m, baseText, botUID)
	decision := classifier.Classify(canonical, policy)

	// OBO irrelevance is the R10 leak guard: drop BEFORE any session state.
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

// replyContextFromPayload converts an Octo Reply payload to the IM-agnostic
// trigger.ReplyContext. Nil for messages without a quote. TargetIsBot is
// computed against the bot's own uid so the classifier never needs to.
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
	// A CommunityTopic (thread / 子区) is group-like for routing: its
	// channel id is the compound "<groupNo>____<shortId>", so it lands in
	// its OWN session (distinct from the parent group and sibling threads)
	// while membership and the mention gate are inherited from the parent
	// group. See thread.go and openclaw inbound.ts thread routing.
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
// ReplyRouting plus the IM channel coords. OBO v2 reroutes redirect to the
// origin channel; on_behalf_of stamps the grantor uid.
func (c *Connector) targetFromDecision(m BotMessage, decision trigger.TriggerDecision) replyTarget {
	tgt := replyTarget{channelID: m.ChannelID, channelType: m.ChannelType}
	if decision.ReplyRouting.HasOBOReroute() {
		rerouteKind := ChannelType(decision.ReplyRouting.OBORerouteKind)
		if rerouteKind == 0 {
			// Conservative default: origin without an explicit kind = group.
			rerouteKind = ChannelGroup
		}
		tgt.channelID = decision.ReplyRouting.OBORerouteChannelID
		tgt.channelType = rerouteKind
	}
	// Persona widening without an OBO reroute keeps the inbound channel
	// as the target — only the OnBehalfOf stamp below matters, which
	// fires unconditionally from ReplyRouting.
	tgt.onBehalfOf = decision.ReplyRouting.OnBehalfOf
	return tgt
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

// dispatchInbound is the single dispatch entry: observation flows directly
// to gw.Observe (no per-key queue, no session lock — observations are
// fast in-memory writes); reply-warranting decisions enqueue. The legacy
// retroactive post-gate Observe in drainTurns is gone.
func (c *Connector) dispatchInbound(key string, inbound router.InboundMessage, tgt replyTarget, channelID string, reply *ReplyPayload) {
	if inbound.ShouldObserve() {
		// Group-context background. Stored history carries the plain
		// resolved text WITHOUT the quoted-reply prefix.
		if c.gateway != nil {
			c.gateway.Observe(inbound)
		}
		return
	}
	// Prepend the quoted-reply context to the CURRENT turn only (never
	// stored history): the sender quoted a prior message, so give the
	// agent that context fenced ahead of the real request (inbound.ts
	// quotePrefix).
	c.applyQuotePrefix(&inbound, reply)
	c.enqueueTurn(key, inbound, tgt)
}

func (c *Connector) hydrateInboundNames(m *BotMessage) {
	// WuKongIM RECV packets carry only fromUid, not a display name. Kick a
	// background fetch via the cache (non-blocking — ResolveUser returns ""
	// on miss and the next message from the same uid sees it cached) and
	// fall back to the cached value if it happens to be there already. The
	// receive goroutine MUST NOT block on REST: a slow / unreachable name
	// service would stall ALL inbound for this bot. First sight of an
	// unseen sender lands in the chat with senderName empty (the GUI falls
	// back to senderUid for the bubble label) — subsequent messages from
	// the same uid carry the resolved name, and the persisted history row
	// gets backfilled on the next history fetch via the warmed cache.
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
// preferring obo_respond_as over obo_grantor_uid (openclaw inbound.ts L2104).
func oboRespondAs(p MessagePayload) string {
	if p.OBORespondAs != "" {
		return p.OBORespondAs
	}
	return p.OBOGrantorUID
}

// resolveAttachments extracts downloadable media/file attachments from a
// payload (image/GIF/file). LLM-facing text rendering is handled by
// ResolveContent (content.go); this only surfaces the URLs the gateway
// materializes into the session cwd. Media URLs are host-validated via
// buildMediaURL (inbound.ts G1).
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
		// SECURITY: p.Name is user-controlled; sanitize before it flows
		// into the <file_content name="…"> attribute the gateway writes.
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
