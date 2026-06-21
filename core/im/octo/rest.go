package octo

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
)

// maxHistoricalPayloadBase64Len caps a base64 payload before decode (api.ts
// MAX_HISTORICAL_PAYLOAD_BASE64_LEN). 256 KiB base64 ≈ 192 KiB decoded — well
// above any legitimate IM payload — guards against a hostile/huge sync row.
const maxHistoricalPayloadBase64Len = 256 * 1024

// RESTClient talks to the Octo bot REST API (api.ts). Auth is Bearer token,
// resolved lazily per request via the token func so it can be injected/rotated
// at runtime (see secret.inject) without rebuilding the client.
type RESTClient struct {
	apiURL string
	token  func() string
	http   *http.Client
}

// NewRESTClient constructs a client. apiURL trailing slashes are stripped. token
// is read on every request; pass a getter backed by the in-memory secret store
// (or a constant func for a fixed token).
func NewRESTClient(apiURL string, token func() string) *RESTClient {
	if token == nil {
		token = func() string { return "" }
	}
	return &RESTClient{
		apiURL: strings.TrimRight(apiURL, "/"),
		token:  token,
		http:   &http.Client{Timeout: 30 * time.Second},
	}
}

// Token returns the currently-resolved bearer token (used by the connector to
// decide whether a token has been injected yet).
func (c *RESTClient) Token() string { return c.token() }

// APIURL returns the (trailing-slash-stripped) base API URL. The connector uses
// it to resolve relative media paths and to host-scope the bot token on inbound
// media downloads.
func (c *RESTClient) APIURL() string { return c.apiURL }

// RegisterResponse mirrors BotRegisterResp (types.ts) — all six fields.
type RegisterResponse struct {
	RobotID        string `json:"robot_id"`
	IMToken        string `json:"im_token"`
	WSURL          string `json:"ws_url"`
	APIURL         string `json:"api_url"`
	OwnerUID       string `json:"owner_uid"`
	OwnerChannelID string `json:"owner_channel_id"`
}

// Register performs POST /v1/bot/register. forceRefresh adds ?force_refresh=true.
func (c *RESTClient) Register(ctx context.Context, forceRefresh bool) (RegisterResponse, error) {
	path := "/v1/bot/register"
	if forceRefresh {
		path += "?force_refresh=true"
	}
	body := map[string]string{
		"agent_platform": "xclaw",
		"agent_version":  "0.1.0",
	}
	var out RegisterResponse
	if err := c.postJSON(ctx, path, body, &out); err != nil {
		return RegisterResponse{}, err
	}
	if out.RobotID == "" || out.IMToken == "" || out.WSURL == "" {
		return RegisterResponse{}, fmt.Errorf("register: incomplete response %+v", out)
	}
	return out, nil
}

// SendMessageResult mirrors SendMessageResult (types.ts). message_id is decoded
// via flexString because the octo IM server sometimes returns it as a JSON
// number (uint64) and sometimes as a string — a strict string decode used to
// fail with "cannot unmarshal number ... into string", and our caller treated
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
	// Bare number: keep the literal so a uint64 messageID doesn't lose precision
	// the way json.Number → float64 → strconv.FormatFloat would.
	*f = flexString(string(b))
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
func (c *RESTClient) SendTextAs(ctx context.Context, channelID string, channelType ChannelType, content string, mentionUIDs []string, mentionEntities []MentionEntity, mentionAll bool, onBehalfOf string) (SendMessageResult, error) {
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
		"client_msg_no": uuid.NewString(),
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

// HistoricalMessage is one row returned by /v1/bot/messages/sync (api.ts
// HistoricalMessage). The server ships content/type/url/name inside a
// base64-encoded JSON payload; GetChannelMessages decodes it and prefers a
// usable top-level field, falling back to the decoded payload (api.ts C1/P1.5).
type HistoricalMessage struct {
	FromUID    string
	FromName   string
	Content    string
	Timestamp  int64
	MessageID  string
	MessageSeq int64
	Type       int
	URL        string
	Name       string
}

// GetChannelMessages pulls recent messages for a channel via the WuKongIM sync
// endpoint (api.ts getChannelMessages, used by G4 cold-start backfill). limit
// defaults to 20 and caps the returned slice client-side (the server may return
// more). Returns nil on any failure — the agent runs fine without history.
func (c *RESTClient) GetChannelMessages(ctx context.Context, channelID string, channelType ChannelType, limit int) []HistoricalMessage {
	if limit <= 0 {
		limit = 20
	}
	body := map[string]any{
		"channel_id":        channelID,
		"channel_type":      int(channelType),
		"limit":             limit,
		"start_message_seq": 0,
		"end_message_seq":   0,
		"pull_mode":         1, // 1 = pull newer messages
	}
	var raw struct {
		Messages []struct {
			FromUID    string          `json:"from_uid"`
			FromName   string          `json:"from_name"`
			Content    string          `json:"content"`
			Timestamp  int64           `json:"timestamp"`
			MessageID  string          `json:"message_id"`
			MessageSeq int64           `json:"message_seq"`
			Type       int             `json:"type"`
			URL        string          `json:"url"`
			Name       string          `json:"name"`
			Payload    json.RawMessage `json:"payload"`
		} `json:"messages"`
	}
	if err := c.postJSON(ctx, "/v1/bot/messages/sync", body, &raw); err != nil {
		fmt.Fprintf(os.Stderr, "[octo] getChannelMessages error: %v\n", err)
		return nil
	}
	msgs := raw.Messages
	if len(msgs) > limit {
		msgs = msgs[:limit] // client-side cap (api.ts D1/S7)
	}
	out := make([]HistoricalMessage, 0, len(msgs))
	for _, m := range msgs {
		// Decode the base64 JSON payload (string form); object form is passed as-is.
		var pl struct {
			Content string `json:"content"`
			Type    int    `json:"type"`
			URL     string `json:"url"`
			Name    string `json:"name"`
		}
		if len(m.Payload) > 0 {
			var s string
			if json.Unmarshal(m.Payload, &s) == nil {
				if len(s) <= maxHistoricalPayloadBase64Len {
					if dec, derr := base64.StdEncoding.DecodeString(s); derr == nil {
						_ = json.Unmarshal(dec, &pl) // leave pl zero on failure
					}
				} else {
					fmt.Fprintf(os.Stderr, "[octo] getChannelMessages dropping oversized payload (%d base64 chars)\n", len(s))
				}
			} else {
				_ = json.Unmarshal(m.Payload, &pl) // object payload
			}
		}
		hm := HistoricalMessage{
			FromUID:    m.FromUID,
			FromName:   m.FromName,
			Content:    firstNonEmpty(m.Content, pl.Content),
			Timestamp:  m.Timestamp,
			MessageID:  m.MessageID,
			MessageSeq: m.MessageSeq,
			Type:       firstNonZero(m.Type, pl.Type),
			URL:        firstNonEmpty(m.URL, pl.URL),
			Name:       firstNonEmpty(m.Name, pl.Name),
		}
		out = append(out, hm)
	}
	return out
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func firstNonZero(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}

// postJSON performs a POST with Bearer auth and decodes the JSON response into
// out (out may be nil for void endpoints). message_id int64 precision is
// preserved by decoding into string fields.
func (c *RESTClient) postJSON(ctx context.Context, path string, body any, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token())

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Cap the body read so a misbehaving server can't return an unbounded
	// response, and truncate what we echo into an error so a large/sensitive
	// error body doesn't flood logs (L26).
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxRESTResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("octo API %s failed (%d): %s", path, resp.StatusCode, truncateForError(data))
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

// maxRESTResponseBytes bounds how much of a REST response body we read.
const maxRESTResponseBytes = 4 * 1024 * 1024

// truncateForError renders a response body for an error message, capping it so a
// large body doesn't bloat logs.
func truncateForError(b []byte) string {
	const max = 512
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "…(truncated)"
}
