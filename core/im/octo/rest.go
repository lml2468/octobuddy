package octo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

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

// SendMessageResult mirrors SendMessageResult (types.ts).
type SendMessageResult struct {
	MessageID   string `json:"message_id"`
	ClientMsgNo string `json:"client_msg_no"`
	MessageSeq  int    `json:"message_seq"`
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
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("octo API %s failed (%d): %s", path, resp.StatusCode, string(data))
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}
