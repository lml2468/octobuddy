package octo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// SendMessageResult mirrors SendMessageResult (types.ts). message_id is decoded
// via flexString because the octo IM server sometimes returns it as a JSON
// number (uint64) and sometimes as a string — a strict string decode used to
// fail with "cannot unmarshal number... into string", and our caller treated
// the error as a transient send failure → retried with a fresh client_msg_no
// → the user received two copies of every reply (#bug-2025-06).
type SendMessageResult struct {
	MessageID   flexString `json:"message_id"`
	ClientMsgNo string     `json:"client_msg_no"`
	MessageSeq  int        `json:"message_seq"`
}

// flexString accepts either a JSON string or a JSON number and decodes both to
// a string. Useful for server fields whose type drifts across deploys.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*f = ""
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexString(s)
		return nil
	}
	// Bare number: validate as JSON number first, then keep the literal
	// so a uint64 messageID doesn't lose precision the way json.Number →
	// float64 → strconv.FormatFloat would. Rejecting object / array /
	// boolean / garbage stops a server-side regression from silently
	// propagating {"foo":1} as the literal string MessageID (which would
	// later be sent verbatim in read-receipt batches, corrupting them).
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("flexString: want JSON string or number, got %q: %w", b, err)
	}
	*f = flexString(string(n))
	return nil
}

// SendText posts a Text message to a channel (api.ts sendMessage). mentionUIDs,
// mentionEntities, and mentionAll are optional; the mention object is only
// attached when at least one is present (stream-relay.ts sendMessage parity).
func (c *RESTClient) SendText(ctx context.Context, channelID string, channelType ChannelType, content string, mentionUIDs []string, mentionEntities []MentionEntity, mentionAll bool) (SendMessageResult, error) {
	return c.SendTextAs(ctx, channelID, channelType, content, mentionUIDs, mentionEntities, mentionAll, "")
}

// SendTextAs is SendText with an optional on_behalf_of grantor uid (openclaw OBO
// relay). When onBehalfOf is non-empty, the server presents the message as the
// grantor speaking (api-fetch.ts sendMessage `on_behalf_of`). An empty string
// is identical to SendText.
//
// Generates a fresh client_msg_no per call — appropriate for a single,
// one-shot send. Callers that retry MUST instead route through
// SendTextAsWithMsgNo with a stable id, otherwise a network blip after the
// server commits but before the response reaches us produces a duplicate
// delivery (octo-server dedup is keyed on client_msg_no).
func (c *RESTClient) SendTextAs(ctx context.Context, channelID string, channelType ChannelType, content string, mentionUIDs []string, mentionEntities []MentionEntity, mentionAll bool, onBehalfOf string) (SendMessageResult, error) {
	return c.SendTextAsWithMsgNo(ctx, channelID, channelType, content, mentionUIDs, mentionEntities, mentionAll, onBehalfOf, uuid.NewString())
}

// SendTextAsWithMsgNo is SendTextAs with a caller-supplied client_msg_no for
// idempotent retry. Server dedup is keyed on this id, so a retry MUST reuse
// the original id — otherwise a transient post-commit failure (TCP reset,
// 502, timeout that hits AFTER the server committed but BEFORE the response
// landed) produces a successful retry with a new id and the user sees the
// message twice. clientMsgNo MUST be non-empty.
func (c *RESTClient) SendTextAsWithMsgNo(ctx context.Context, channelID string, channelType ChannelType, content string, mentionUIDs []string, mentionEntities []MentionEntity, mentionAll bool, onBehalfOf, clientMsgNo string) (SendMessageResult, error) {
	// Lock the F1 fix in the type system rather than in commentary: a caller
	// passing "" would re-introduce the duplicate-IM-on-retry hazard silently
	// (octo-server's dedup is keyed on this field; an empty key collides
	// with every other empty-key send and the dedup behavior becomes
	// undefined). Refuse here so the regression is loud.
	if clientMsgNo == "" {
		return SendMessageResult{}, fmt.Errorf("octo: SendTextAsWithMsgNo requires a non-empty clientMsgNo (server dedup key)")
	}
	payload := map[string]any{
		"type":    int(MsgText),
		"content": content,
	}
	if len(mentionUIDs) > 0 || len(mentionEntities) > 0 || mentionAll {
		mention := map[string]any{}
		if len(mentionUIDs) > 0 {
			mention["uids"] = mentionUIDs
		}
		if len(mentionEntities) > 0 {
			mention["entities"] = mentionEntities
		}
		if mentionAll {
			mention["all"] = 1
		}
		payload["mention"] = mention
	}
	body := map[string]any{
		"channel_id":    channelID,
		"channel_type":  int(channelType),
		"payload":       payload,
		"client_msg_no": clientMsgNo,
	}
	if onBehalfOf != "" {
		body["on_behalf_of"] = onBehalfOf
	}
	var out SendMessageResult
	if err := c.postJSON(ctx, "/v1/bot/sendMessage", body, &out); err != nil {
		return SendMessageResult{}, err
	}
	return out, nil
}

// SendTyping posts a typing indicator (api.ts sendTyping).
func (c *RESTClient) SendTyping(ctx context.Context, channelID string, channelType ChannelType) error {
	return c.SendTypingAs(ctx, channelID, channelType, "")
}

// SendTypingAs is SendTyping with an optional on_behalf_of grantor uid (openclaw
// OBO relay). An empty string is identical to SendTyping.
func (c *RESTClient) SendTypingAs(ctx context.Context, channelID string, channelType ChannelType, onBehalfOf string) error {
	body := map[string]any{
		"channel_id": channelID, "channel_type": int(channelType),
	}
	if onBehalfOf != "" {
		body["on_behalf_of"] = onBehalfOf
	}
	return c.postJSON(ctx, "/v1/bot/typing", body, nil)
}

// Heartbeat posts the REST heartbeat (api.ts sendHeartbeat).
func (c *RESTClient) Heartbeat(ctx context.Context) error {
	return c.postJSON(ctx, "/v1/bot/heartbeat", map[string]any{}, nil)
}

// SendReadReceipt acks one or more messages as read (api.ts sendReadReceipt,
// POST /v1/bot/readReceipt). Called fire-and-forget after an inbound message is
// handled.
func (c *RESTClient) SendReadReceipt(ctx context.Context, channelID string, channelType ChannelType, messageIDs []string) error {
	return c.postJSON(ctx, "/v1/bot/readReceipt", map[string]any{
		"channel_id":   channelID,
		"channel_type": int(channelType),
		"message_ids":  messageIDs,
	}, nil)
}
