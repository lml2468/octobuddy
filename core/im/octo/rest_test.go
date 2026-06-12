package octo

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// tok returns a constant token getter for tests.
func tok(s string) func() string { return func() string { return s } }

func TestRegisterRequestAndResponse(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		_, _ = io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(RegisterResponse{
			RobotID: "robot1", IMToken: "imtok", WSURL: "wss://x/ws",
			APIURL: "https://x", OwnerUID: "owner", OwnerChannelID: "oc",
		})
	}))
	defer srv.Close()

	c := NewRESTClient(srv.URL+"/", tok("bf_secret"))
	reg, err := c.Register(context.Background(), true)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if gotAuth != "Bearer bf_secret" {
		t.Fatalf("auth header = %q", gotAuth)
	}
	if gotPath != "/v1/bot/register?force_refresh=true" {
		t.Fatalf("path = %q", gotPath)
	}
	if reg.RobotID != "robot1" || reg.IMToken != "imtok" || reg.WSURL != "wss://x/ws" {
		t.Fatalf("response mapped wrong: %+v", reg)
	}
}

func TestSendTextRequestShape(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		_ = json.NewEncoder(w).Encode(SendMessageResult{MessageID: "123", MessageSeq: 7})
	}))
	defer srv.Close()

	c := NewRESTClient(srv.URL, tok("bf_secret"))
	res, err := c.SendText(context.Background(), "chan1", ChannelGroup, "hi there", []string{"u2"}, nil, false)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if res.MessageID != "123" || res.MessageSeq != 7 {
		t.Fatalf("result mapped wrong: %+v", res)
	}
	if body["channel_id"] != "chan1" {
		t.Fatalf("channel_id = %v", body["channel_id"])
	}
	if int(body["channel_type"].(float64)) != int(ChannelGroup) {
		t.Fatalf("channel_type = %v", body["channel_type"])
	}
	if body["client_msg_no"] == nil || body["client_msg_no"] == "" {
		t.Fatal("client_msg_no must be set (server dedup)")
	}
	payload := body["payload"].(map[string]any)
	if int(payload["type"].(float64)) != int(MsgText) || payload["content"] != "hi there" {
		t.Fatalf("payload wrong: %+v", payload)
	}
	mention := payload["mention"].(map[string]any)
	uids := mention["uids"].([]any)
	if len(uids) != 1 || uids[0] != "u2" {
		t.Fatalf("mention uids wrong: %+v", mention)
	}
}

func TestRESTErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte("forbidden"))
	}))
	defer srv.Close()
	c := NewRESTClient(srv.URL, tok("bf"))
	_, err := c.Register(context.Background(), false)
	if err == nil {
		t.Fatal("expected error on 403")
	}
}

// TestRESTClientTokenRotation proves the token is resolved per request: mutating
// the source between calls changes the Authorization header (this is what lets
// secret.inject rotate a token without rebuilding the client).
func TestRESTClientTokenRotation(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(RegisterResponse{
			RobotID: "r", IMToken: "imtok", WSURL: "wss://x/ws",
			APIURL: "https://x", OwnerUID: "owner", OwnerChannelID: "oc",
		})
	}))
	defer srv.Close()

	current := ""
	c := NewRESTClient(srv.URL, func() string { return current })
	if c.Token() != "" {
		t.Fatalf("expected empty token initially, got %q", c.Token())
	}

	current = "bf_first"
	if _, err := c.Register(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer bf_first" {
		t.Fatalf("first auth = %q", gotAuth)
	}

	current = "bf_rotated"
	if _, err := c.Register(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer bf_rotated" {
		t.Fatalf("rotated auth = %q", gotAuth)
	}
}
