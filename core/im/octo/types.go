// Wire enums and payload shapes shared by REST + WebSocket. See doc.go for
// the package overview and connector.go for the lifecycle entry point.
package octo

import (
	"encoding/json"
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

	// --- System / control-plane payloads (octo-lib common/msg.go) ---
	// These are NOT user-authored chat — octo-server broadcasts them into the
	// channel as ordinary messages (carrying the operator's from_uid), so they
	// must be filtered out of the LLM turn path. octo-server's own search layer
	// hard-filters payload.type ∈ [1000,2000] (modules/messages_search) for the
	// same reason. MsgCMD(99) is the command frame; MsgSystemMin/Max bound the
	// system-event range.
	//
	// NOTE: octo-server ALSO emits some control-plane notifications as a plain
	// Text(=1) payload carrying a structured `event` envelope (e.g.
	// group_md_updated, and it @-mentions the bot). Those are NOT in this type
	// range — detect them via MessagePayload.IsControlEvent / the Event field,
	// not the type. See PayloadEvent below.
	MsgCMD       MessageType = 99
	MsgSystemMin MessageType = 1000
	MsgSystemMax MessageType = 2000
)

// Control-plane event.type values octo-server stamps on the `event` envelope of
// a Text-payload notification (octo-server modules/{group,thread,robot}). These
// are not chat — onInbound routes them to the deterministic handler, never the
// classifier. The md ones additionally drive the GROUP.md / thread-md mirror.
const (
	EventGroupMdUpdated    = "group_md_updated"
	EventGroupMdDeleted    = "group_md_deleted"
	EventThreadMdUpdated   = "thread_md_updated"
	EventThreadMdDeleted   = "thread_md_deleted"
	EventMentionPrefUpdate = "mention_pref_updated"
)

// IsSystem reports whether t is a system / control-plane payload TYPE rather
// than a user-authored chat message. Such messages MUST never trigger an LLM
// turn (they carry the operator's from_uid, so the empty-from_uid drop misses
// them) — the whole [1000,2000] range (group create / member add-remove-quit /
// settings change / tip / …) plus CMD is suppressed. Mirrors octo-server's
// [1000,2000] + CMD hard-filter. NOTE: this does NOT cover the Text-payload
// `event`-envelope notifications — combine with MessagePayload.IsControlEvent at
// the call site.
func (t MessageType) IsSystem() bool {
	return t == MsgCMD || (t >= MsgSystemMin && t <= MsgSystemMax)
}

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

	// Event is the control-plane envelope octo-server stamps on some Text(=1)
	// notifications (group_md_updated, thread_md_updated, mention_pref_updated,
	// …). Non-nil means "this is a control-plane event, not chat" — see
	// IsControlEvent. Nil for ordinary messages.
	Event *PayloadEvent
}

// PayloadEvent is the `event` envelope on a control-plane notification payload
// (octo-server modules/{group,thread,robot} sendGroupMdNotification etc.). Only
// the fields the connector acts on are modeled. Version/UpdatedBy accompany the
// md events; GroupNo accompanies mention_pref.
type PayloadEvent struct {
	Type      string `json:"type"`
	Version   int64  `json:"version,omitempty"`
	UpdatedBy string `json:"updated_by,omitempty"`
	GroupNo   string `json:"group_no,omitempty"`
}

// IsControlEvent reports whether this payload is a control-plane notification
// carrying an `event` envelope (group_md_updated, mention_pref_updated, …)
// rather than a chat message. Such payloads arrive as Text(=1) and @-mention
// the bot, so without this check they'd reach the classifier and trigger an LLM
// turn. Pair with Type.IsSystem() at the inbound gate to cover both control-
// plane shapes.
func (p MessagePayload) IsControlEvent() bool {
	return p.Event != nil && p.Event.Type != ""
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

	Event *PayloadEvent `json:"event"`
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
		Event:                w.Event,
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
