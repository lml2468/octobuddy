// Package octoapi is a small REST client for provisioning a bot on octo-server
// from the desktop app: create a bot with the operator's User API Key (uk_…) and
// obtain its bf_ token. It exists so the "Add bot" wizard can be self-service
// instead of requiring the operator to mint a token out-of-band (BotFather
// /newbot) and paste it in.
//
// One server call:
//   - create: POST {apiURL}/v1/user/bots, authenticated with the uk_ key as
//     Authorization: Bearer uk_…, body {name, username?} → {robot_id,
//     bot_token:"bf_…"}. This is octo-server's existing user-bot endpoint
//     (modules/botfather/api_user.go createUserBot); the response is a direct
//     JSON object (no envelope).
//
// The uk_ key authenticates the operator; it stays in process memory and is
// never logged.
package octoapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lml2468/xclaw/core/config"
)

// httpTimeout bounds the provisioning request.
const httpTimeout = 30 * time.Second

// userAPIKeyPrefix is the expected prefix of an octo User API Key. We assert it
// client-side for a clearer error than a server 401.
const userAPIKeyPrefix = "uk_"

// validBaseURL applies the same SSRF policy configstore enforces on apiUrl
// (config.IsAllowedURL): https to any non-private host, or http only to a true
// loopback host. Reusing the canonical check keeps the wizard honest — a
// hand-rolled `HasPrefix("http://localhost")` would accept the lookalike host
// `http://localhost.evil.com` and exfiltrate the operator's uk_ API Key.
func validBaseURL(s string) error {
	if !config.IsAllowedURL(s) {
		return fmt.Errorf("API URL 须为 https://（或 http://localhost）")
	}
	return nil
}

// BotResult is the provisioned bot's identity returned to the wizard.
type BotResult struct {
	RobotID  string `json:"robotId"`
	BotToken string `json:"botToken"` // bf_… — stored in the keychain, never config.json
}

// AddBot provisions a bot on octo-server using the operator's User API Key and
// returns the new bot's robot id + bf_ token. apiKey is a uk_… key; apiURL is
// the octo-server base (e.g. https://im.deepminer.com.cn/api).
func AddBot(ctx context.Context, apiURL, apiKey, name string) (BotResult, error) {
	apiURL = strings.TrimRight(strings.TrimSpace(apiURL), "/")
	if err := validBaseURL(apiURL); err != nil {
		return BotResult{}, err
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return BotResult{}, fmt.Errorf("请填写 API Key")
	}
	if !strings.HasPrefix(apiKey, userAPIKeyPrefix) {
		return BotResult{}, fmt.Errorf("API Key 应以 uk_ 开头")
	}
	if strings.TrimSpace(name) == "" {
		return BotResult{}, fmt.Errorf("请填写 Bot 名称")
	}

	cli := &http.Client{Timeout: httpTimeout}

	reqBody := map[string]any{"name": name}
	var out struct {
		RobotID  string `json:"robot_id"`
		BotToken string `json:"bot_token"`
	}
	if err := postJSON(ctx, cli, apiURL+"/v1/user/bots", apiKey, reqBody, &out); err != nil {
		return BotResult{}, fmt.Errorf("创建 Bot 失败：%w", err)
	}
	if out.RobotID == "" || out.BotToken == "" {
		return BotResult{}, fmt.Errorf("创建 Bot 失败：服务返回不完整")
	}
	return BotResult{RobotID: out.RobotID, BotToken: out.BotToken}, nil
}

// postJSON POSTs body as JSON, authenticated with the uk_ key as a bearer token,
// and decodes a 2xx response into out.
func postJSON(ctx context.Context, cli *http.Client, url, apiKey string, body, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := cli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Cap the read so a misbehaving endpoint can't balloon memory.
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("API Key 无效或已失效")
		}
		if msg := serverMsg(data); msg != "" {
			return fmt.Errorf("%s", msg)
		}
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("解析响应失败：%w", err)
	}
	return nil
}

// serverMsg extracts octo-server's error message field ({"msg":"…"}) for a
// human-readable failure, if present.
func serverMsg(data []byte) string {
	var e struct {
		Msg string `json:"msg"`
	}
	if json.Unmarshal(data, &e) == nil {
		return strings.TrimSpace(e.Msg)
	}
	return ""
}
