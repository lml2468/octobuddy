package trigger

// evaluateOBOTrust mirrors core/im/octo/connector_inbound.go::isTrustedOBORelay
// plus the openclaw inbound.ts OBO v2 reply-target derivation. Returns:
//
//   - (reroute, true)  → OBO fields are signed by the configured grantor
//     (FromUID == p.Grantor.UID). The reroute carries the origin channel
//     id + kind so the IM adapter can deliver the reply to the source group
//     / DM rather than echoing back to the relay channel.
//   - (zero,    false) → OBO fields untrusted (no persona / no OBO signal
//     / sender is not the grantor). The classifier treats the message as a
//     plain inbound; OBO fields are silently ignored, never echoed.
//
// The IM-specific origin-DM rewrite (chType==DM → channelID == OriginFromUID)
// is preserved here so the IM adapter doesn't have to re-implement it.
func evaluateOBOTrust(in CanonicalInbound, p Policy) (ReplyRouting, bool) {
	if !p.Grantor.Configured() || in.OBO == nil {
		return ReplyRouting{}, false
	}
	o := in.OBO
	if o.OriginChannelID == "" {
		return ReplyRouting{}, false
	}
	if o.RespondAs == "" {
		return ReplyRouting{}, false
	}
	if in.FromUID != p.Grantor.UID {
		// Untrusted: the OBO fields claim a grantor relay but the sender is
		// not the configured grantor — strip the signal so a forger can't
		// reroute the reply.
		return ReplyRouting{}, false
	}
	channelID := o.OriginChannelID
	// DM origin: the bot is only friends with the grantor, so the reply
	// goes via on_behalf_of=grantor to the original sender uid (which lives
	// in OBO.OriginFromUID, not OriginChannelID for the DM case).
	if isDMKind(o.OriginChannelType) && o.OriginFromUID != "" {
		channelID = o.OriginFromUID
	}
	return ReplyRouting{
		OBORerouteChannelID: channelID,
		OBORerouteKind:      o.OriginChannelType,
	}, true
}

// personaRelevant mirrors core/persona/persona.go::Grantor.Relevant: under
// an OBO v2 trusted relay, only messages that actually address the grantor
// (broadcast humans/all, explicit grantor uid, or no mention info) are
// relevant. Pure @AI fan-out gets dropped before any session-state side
// effect (openclaw R10 ordering invariant).
func personaRelevant(m *MentionPayload, g PolicyGrantor) bool {
	if !g.Configured() {
		return true
	}
	if m == nil {
		// No mention info → plain chatter the persona should see (the
		// relevance gate is a fan-out filter, not a silence filter).
		return true
	}
	if m.HumansFlag || m.AllFlag {
		return true
	}
	if containsUID(m.UIDs, g.UID) {
		return true
	}
	if !m.AIsFlag && len(m.UIDs) == 0 {
		// Empty mention payload — same as nil.
		return true
	}
	return false
}

// DMKindWire is the adapter-supplied channel-type code that denotes a DM in an
// OBO signal. It is the wire convention every IM adapter emitting OBO signals
// must follow (the octo connector's octo.ChannelDM == 1); trigger stays
// dependency-free (never imports an IM package), and the daemon asserts
// DMKindWire == octo.ChannelDM in a cross-package test so the agreement can't
// silently drift.
const DMKindWire = 1

// isDMKind reports whether an adapter-supplied channel-type code denotes a DM.
func isDMKind(kind int) bool { return kind == DMKindWire }
