package octo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/lml2468/octobuddy/core/clog"
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
		"agent_platform": "octobuddy",
		"agent_version":  "0.1.0",
	}
	var out RegisterResponse
	if err := c.postJSON(ctx, path, body, &out); err != nil {
		return RegisterResponse{}, err
	}
	if out.RobotID == "" || out.IMToken == "" || out.WSURL == "" {
		return RegisterResponse{}, fmt.Errorf("register: incomplete response %+v", out)
	}
	// SECURITY: validate the server-supplied ws_url BEFORE the connector dials
	// it. Without this, a compromised or MitM'd octo-server could return
	// ws://attacker/ — accepted as plaintext + arbitrary host by the WS
	// dialer, leaking the bot's IMToken in the CONNECT frame to a host the
	// operator never trusted. Policy:
	// - apiURL is https → WSURL must be wss (no plaintext downgrade)
	// - apiURL is http → only loopback apiURL allowed by config.IsAllowedURL,
	// so ws://loopback is fine (dev mode)
	// - host must match apiURL's hostname exactly (no sibling-subdomain
	// hop). Stricter than a same-eTLD+1 check; legitimate deployments
	// terminate WS on the same hostname as REST.
	if err := validateWSURL(out.WSURL, c.apiURL); err != nil {
		return RegisterResponse{}, fmt.Errorf("register: %w", err)
	}
	return out, nil
}

// validateWSURL enforces scheme + host equality between the server-returned
// WSURL and the operator-configured apiURL. See Register's SECURITY comment.
func validateWSURL(rawWSURL, rawAPIURL string) error {
	wu, err := url.Parse(rawWSURL)
	if err != nil {
		return fmt.Errorf("ws_url parse: %w", err)
	}
	au, err := url.Parse(rawAPIURL)
	if err != nil {
		return fmt.Errorf("api_url parse: %w", err)
	}
	switch wu.Scheme {
	case "wss":
		// always acceptable
	case "ws":
		// only when the configured api_url is plaintext (dev / loopback)
		if au.Scheme != "http" {
			return fmt.Errorf("ws_url uses plaintext ws:// but api_url is %s://", au.Scheme)
		}
	default:
		return fmt.Errorf("ws_url has unsupported scheme %q", wu.Scheme)
	}
	if wu.Hostname() == "" {
		return fmt.Errorf("ws_url has no host")
	}
	// EqualFold so a case-drift between apiURL ("api.example") and the
	// server-returned ws_url ("API.example") doesn't false-positive on
	// legitimate deployments; DNS itself is case-insensitive.
	if !strings.EqualFold(wu.Hostname(), au.Hostname()) {
		return fmt.Errorf("ws_url host %q does not match api_url host %q (cross-host redirect of credentialed handshake refused)", wu.Hostname(), au.Hostname())
	}
	// Port equality (with scheme-default fallback): without this, a compromised
	// server could return wss://api.example:9443/ws when apiURL is
	// https://api.example (port 443) and the handshake would proceed against an
	// attacker-controlled port on the same hostname. Defense-in-depth.
	if portFor(wu) != portFor(au) {
		return fmt.Errorf("ws_url port %q does not match api_url port %q (credentialed handshake refused)", portFor(wu), portFor(au))
	}
	return nil
}

// portFor returns u's explicit port, or the scheme default (443 for https/wss,
// 80 for http/ws). Empty Port means "use the default" per net/url.
func portFor(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch u.Scheme {
	case "https", "wss":
		return "443"
	case "http", "ws":
		return "80"
	}
	return ""
}

// nameResponse is the shared shape of GetUserInfo / GetGroupInfo replies —
// both endpoints carry the display name under "name" (plus other ignored
// fields). Declared once so a future schema tweak lands in one place.
type nameResponse struct {
	Name string `json:"name"`
}

// GetUserInfo resolves a user uid to its display name (api-fetch.ts
// fetchUserInfo, GET /v1/bot/user/info?uid={uid}). Returns "" when the server
// has no record of the uid (404) or any other transient failure — callers fall
// back to the bare uid for display. A short ctx timeout (e.g. 5s) is the
// caller's responsibility, matching the upstream TS client.
func (c *RESTClient) GetUserInfo(ctx context.Context, uid string) string {
	if uid == "" {
		return ""
	}
	var raw nameResponse
	if err := c.getJSON(ctx, "/v1/bot/user/info?uid="+url.QueryEscape(uid), &raw, true); err != nil {
		clog.For("octo").Warn("getUserInfo", "uid", uid, "err", err)
		return ""
	}
	return raw.Name
}

// GetGroupInfo resolves a bare group_no to its display name (api-fetch.ts
// getGroupInfo, GET /v1/bot/groups/{groupNo}). Same soft-degrade contract as
// GetUserInfo. Pass a thread compound id through ExtractParentGroupNo before
// calling — this endpoint speaks group_no, not the thread "<g>____<s>" form.
func (c *RESTClient) GetGroupInfo(ctx context.Context, groupNo string) string {
	if groupNo == "" {
		return ""
	}
	var raw nameResponse
	if err := c.getJSON(ctx, "/v1/bot/groups/"+url.PathEscape(groupNo), &raw, true); err != nil {
		clog.For("octo").Warn("getGroupInfo", "group_no", groupNo, "err", err)
		return ""
	}
	return raw.Name
}

// GetThreadInfo resolves a thread (CommunityTopic / 子区) to its display name
// (api-fetch.ts getThread, GET /v1/bot/groups/{groupNo}/threads/{shortId}).
// Same soft-degrade contract as GetGroupInfo. The thread's parent group name
// is a separate call (GetGroupInfo); compositing the two is the caller's job.
func (c *RESTClient) GetThreadInfo(ctx context.Context, groupNo, shortID string) string {
	if groupNo == "" || shortID == "" {
		return ""
	}
	var raw nameResponse
	path := "/v1/bot/groups/" + url.PathEscape(groupNo) + "/threads/" + url.PathEscape(shortID)
	if err := c.getJSON(ctx, path, &raw, true); err != nil {
		clog.For("octo").Warn("getThreadInfo", "group_no", groupNo, "short_id", shortID, "err", err)
		return ""
	}
	return raw.Name
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

// getJSON performs a GET with Bearer auth and decodes into out. notFoundOK
// turns a 404 into a no-op success (out is left zero) instead of an error;
// the name-lookup endpoints signal "unknown uid/group" with a 404 and the
// caller wants a negative cache hit, not a logged failure.
func (c *RESTClient) getJSON(ctx context.Context, path string, out any, notFoundOK bool) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token())
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxRESTResponseBytes))
	if notFoundOK && resp.StatusCode == http.StatusNotFound {
		return nil
	}
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
