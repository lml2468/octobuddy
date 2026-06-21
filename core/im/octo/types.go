// Wire enums and payload shapes shared by REST + WebSocket. See doc.go for
// the package overview and connector.go for the lifecycle entry point.
package octo

import (
	"encoding/json"

	"github.com/lml2468/xclaw/core/persona"
)

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
	MsgText            MessageType = 1
	MsgImage           MessageType = 2
	MsgGIF             MessageType = 3
	MsgVoice           MessageType = 4
	MsgVideo           MessageType = 5
	MsgLocation        MessageType = 6
	MsgCard            MessageType = 7
	MsgFile            MessageType = 8
	MsgMultipleForward MessageType = 11
	// MsgRichText is text + inline images (types.ts MessageType.RichText).
	MsgRichText MessageType = 14
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
//
// `content` is type-polymorphic on the wire: a string for Text and most types,
// but a RichText(=14) block array. UnmarshalJSON therefore decodes it into
// RichContent (any) and additionally surfaces a string form as Content when the
// raw value is a JSON string — so existing string-only callers keep working
// while RichText can read the array via RichContent.
type MessagePayload struct {
	Type    MessageType
	Content string   // string `content` (Text et al.); empty for array content
	URL     string   // media storage path / URL
	Name    string   // file name / card name
	Size    int64    // server-reported byte size (File payloads)
	UID     string   // card uid (types.ts Card payload)
	Plain   string   // RichText server-authoritative plain text
	Mention *Mention // @-mention payload
	Reply   *ReplyPayload

	// RichContent is the raw `content` value (string or []block) for RichText.
	RichContent any

	// Latitude/Longitude (+ Lat/Lng/Lon aliases) are user-controlled location
	// coordinates; kept as any so toFiniteCoord can reject non-numeric forgeries.
	Latitude  any
	Longitude any
	Lat       any
	Lng       any
	Lon       any

	// Users/Msgs are the MultipleForward(=11) nested transcript.
	Users []forwardUser
	Msgs  []forwardMessage

	// OBO v2 fan-out fields (openclaw inbound.ts ~L2102). Present only on
	// grantor-relayed fan-out messages; the connector trusts them ONLY when the
	// message is sent by the configured grantor (security gate). All optional.
	OBOOriginChannelID   string
	OBOOriginChannelType *int
	OBOOriginFromUID     string
	OBORespondAs         string
	OBOGrantorUID        string
	OBOSystemHint        string
}

// payloadWire is the on-the-wire shape used to decode MessagePayload. content is
// captured raw so we can accept either a string or an array.
type payloadWire struct {
	Type      MessageType      `json:"type"`
	Content   json.RawMessage  `json:"content"`
	URL       string           `json:"url"`
	Name      string           `json:"name"`
	Size      int64            `json:"size"`
	UID       string           `json:"uid"`
	Plain     string           `json:"plain"`
	Mention   *Mention         `json:"mention"`
	Reply     *ReplyPayload    `json:"reply"`
	Latitude  any              `json:"latitude"`
	Longitude any              `json:"longitude"`
	Lat       any              `json:"lat"`
	Lng       any              `json:"lng"`
	Lon       any              `json:"lon"`
	Users     []forwardUser    `json:"users"`
	Msgs      []forwardMessage `json:"msgs"`

	OBOOriginChannelID   string `json:"obo_origin_channel_id"`
	OBOOriginChannelType *int   `json:"obo_origin_channel_type"`
	OBOOriginFromUID     string `json:"obo_origin_from_uid"`
	OBORespondAs         string `json:"obo_respond_as"`
	OBOGrantorUID        string `json:"obo_grantor_uid"`
	OBOSystemHint        string `json:"obo_system_hint"`
}

// UnmarshalJSON decodes a MessagePayload, splitting the polymorphic `content`
// field into Content (when it is a JSON string) and RichContent (always — the
// raw decoded value, used by the RichText expander).
func (p *MessagePayload) UnmarshalJSON(b []byte) error {
	var w payloadWire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	*p = MessagePayload{
		Type:      w.Type,
		URL:       w.URL,
		Name:      w.Name,
		Size:      w.Size,
		UID:       w.UID,
		Plain:     w.Plain,
		Mention:   w.Mention,
		Reply:     w.Reply,
		Latitude:  w.Latitude,
		Longitude: w.Longitude,
		Lat:       w.Lat,
		Lng:       w.Lng,
		Lon:       w.Lon,
		Users:     w.Users,
		Msgs:      w.Msgs,

		OBOOriginChannelID:   w.OBOOriginChannelID,
		OBOOriginChannelType: w.OBOOriginChannelType,
		OBOOriginFromUID:     w.OBOOriginFromUID,
		OBORespondAs:         w.OBORespondAs,
		OBOGrantorUID:        w.OBOGrantorUID,
		OBOSystemHint:        w.OBOSystemHint,
	}
	if len(w.Content) > 0 && string(w.Content) != "null" {
		var anyVal any
		if err := json.Unmarshal(w.Content, &anyVal); err == nil {
			p.RichContent = anyVal
			if s, ok := anyVal.(string); ok {
				p.Content = s
			}
		}
	}
	return nil
}

// ReplyPayload is a quoted prior message (types.ts ReplyPayload).
type ReplyPayload struct {
	Payload  *MessagePayload `json:"payload,omitempty"`
	FromUID  string          `json:"from_uid,omitempty"`
	FromName string          `json:"from_name,omitempty"`
}

// forwardUser is a sender entry in a MultipleForward payload (types.ts
// ForwardUser). uid AND name are both user-controlled.
type forwardUser struct {
	UID  string `json:"uid"`
	Name string `json:"name"`
}

// forwardMessage is one nested message inside a MultipleForward payload
// (types.ts ForwardMessage).
type forwardMessage struct {
	FromUID string         `json:"from_uid"`
	Payload forwardPayload `json:"payload"`
}

// forwardPayload is a forward child's payload. Like MessagePayload its `content`
// is polymorphic (RichContent), so it uses the same split-decode approach.
type forwardPayload struct {
	Type        int
	Content     string
	URL         string
	Name        string
	Plain       string
	RichContent any
	Users       []forwardUser
	Msgs        []forwardMessage
}

func (p *forwardPayload) UnmarshalJSON(b []byte) error {
	var w struct {
		Type    int              `json:"type"`
		Content json.RawMessage  `json:"content"`
		URL     string           `json:"url"`
		Name    string           `json:"name"`
		Plain   string           `json:"plain"`
		Users   []forwardUser    `json:"users"`
		Msgs    []forwardMessage `json:"msgs"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	*p = forwardPayload{Type: w.Type, URL: w.URL, Name: w.Name, Plain: w.Plain, Users: w.Users, Msgs: w.Msgs}
	if len(w.Content) > 0 && string(w.Content) != "null" {
		var anyVal any
		if err := json.Unmarshal(w.Content, &anyVal); err == nil {
			p.RichContent = anyVal
			if s, ok := anyVal.(string); ok {
				p.Content = s
			}
		}
	}
	return nil
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

// PersonaMention projects this message's @-mention payload onto the IM-agnostic
// persona.Mention vocabulary the persona package operates on. The three-state
// bool|number wire flags are normalized to bool here.
func (m BotMessage) PersonaMention() persona.Mention {
	if m.Payload.Mention == nil {
		return persona.Mention{}
	}
	return persona.Mention{
		UIDs:   m.Payload.Mention.UIDs,
		AIs:    truthy(m.Payload.Mention.AIs),
		Humans: truthy(m.Payload.Mention.Humans),
		All:    truthy(m.Payload.Mention.All),
	}
}

// ExplicitlyMentionsBot reports whether the bot's own uid is in the explicit
// @-mention uid list (NOT @AI / @所有人). Mirrors openclaw inbound.ts
// `isExplicitBotMention`; a direct @bot mention makes a persona clone reply as
// itself rather than as the grantor.
func (m BotMessage) ExplicitlyMentionsBot(botUID string) bool {
	if m.Payload.Mention == nil {
		return false
	}
	for _, u := range m.Payload.Mention.UIDs {
		if u == botUID {
			return true
		}
	}
	return false
}

// Triggers reports whether this message should trigger a turn for the bot,
// accounting for persona-clone semantics. Mirrors openclaw inbound.ts
// `isMentioned` (~L1701-1705):
//
//   - explicit @bot uid → always;
//   - pure @AI (ais=1, no broadcast flags) → yes; @AI suppressed under a
//     broadcast (the server rewrites @所有人 to also set ais=1);
//   - @所有人 (humans=1) → only persona clones (a human is part of the
//     broadcast, the clone acts for them);
//   - grantor @-mentioned → persona clones (targets the grantor identity).
//
// For a non-clone (zero grantor) this collapses to the plain mention gate.
func (m BotMessage) Triggers(botUID string, grantor persona.Grantor) bool {
	if m.Payload.Mention == nil {
		return false
	}
	pm := m.PersonaMention()
	isBroadcast := pm.All || pm.Humans
	if !isBroadcast && pm.AIs {
		return true
	}
	if m.ExplicitlyMentionsBot(botUID) {
		return true
	}
	if grantor.Configured() {
		if pm.Humans {
			return true
		}
		for _, u := range pm.UIDs {
			if u == grantor.UID {
				return true
			}
		}
	}
	return false
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
