// Package octo implements the Octo IM connector: the WuKongIM binary protocol
// (WebSocket) plus the Octo REST API, ported wire-compatibly from
// cc-channel-octo's src/octo. It produces router.InboundMessage values for the
// gateway and delivers replies via REST sendMessage.
package octo

// ChannelType mirrors Octo's channel kind (types.ts ChannelType).
type ChannelType int

const (
	ChannelDM             ChannelType = 1
	ChannelGroup          ChannelType = 2
	ChannelCommunityTopic ChannelType = 5
)

// MessageType mirrors Octo's payload type enum (types.ts MessageType).
type MessageType int

const (
	MsgText  MessageType = 1
	MsgImage MessageType = 2
)

// Mention is the @-mention payload (types.ts MentionPayload). Only the fields
// the connector needs are modeled; humans/ais/all are three-state ints.
type Mention struct {
	UIDs   []string `json:"uids,omitempty"`
	All    any      `json:"all,omitempty"`    // legacy @all (bool|number)
	Humans any      `json:"humans,omitempty"` // @all-humans (bool|number)
	AIs    any      `json:"ais,omitempty"`    // @all-AIs (bool|number)
}

// MessagePayload is the decrypted RECV payload JSON (types.ts MessagePayload).
type MessagePayload struct {
	Type    MessageType `json:"type"`
	Content string      `json:"content,omitempty"`
	URL     string      `json:"url,omitempty"`
	Name    string      `json:"name,omitempty"`
	Mention *Mention    `json:"mention,omitempty"`
}

// BotMessage is one inbound message decoded from a RECV packet (types.ts
// BotMessage). message_id is a decimal string (int64 precision).
type BotMessage struct {
	MessageID   string
	MessageSeq  uint32
	FromUID     string
	FromName    string
	ChannelID   string
	ChannelType ChannelType
	Timestamp   uint32
	Payload     MessagePayload
	StreamOn    bool
}

// mentionsBot reports whether this message @-mentions the given bot uid, or
// addresses all AIs. Mirrors session-router's group mention gate (uids or ais,
// NOT humans-only @all).
func (m BotMessage) MentionsBot(botUID string) bool {
	if m.Payload.Mention == nil {
		return false
	}
	for _, u := range m.Payload.Mention.UIDs {
		if u == botUID {
			return true
		}
	}
	return truthy(m.Payload.Mention.AIs)
}

// truthy interprets Octo's three-state bool|number fields.
func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case float64:
		return t != 0
	case int:
		return t != 0
	default:
		return false
	}
}
