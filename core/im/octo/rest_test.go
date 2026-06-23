package octo

import (
	"context"
	"encoding/base64"
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
			RobotID: "robot1", IMToken: "imtok", WSURL: "ws://" + r.Host + "/ws",
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
	if reg.RobotID != "robot1" || reg.IMToken != "imtok" {
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
	assertSendTextBody(t, body)
}

func assertSendTextBody(t *testing.T, body map[string]any) {
	t.Helper()

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

// TestGetChannelMessagesDecodesPayload verifies the sync endpoint request shape
// and that content/type/url/name are merged from the base64 payload when absent
// at the top level (api.ts C1/P1.5).
func TestGetChannelMessagesDecodesPayload(t *testing.T) {
	var body map[string]any
	var gotPath string
	plB64 := base64.StdEncoding.EncodeToString([]byte(`{"type":1,"content":"hello world"}`))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		_, _ = w.Write([]byte(`{"messages":[{"from_uid":"user1","from_name":"Alice","timestamp":123,"message_id":"m1","message_seq":5,"payload":"` + plB64 + `"}]}`))
	}))
	defer srv.Close()

	c := NewRESTClient(srv.URL, tok("bf"))
	msgs := c.GetChannelMessages(context.Background(), "g1", ChannelGroup, 10)
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}
	assertDecodedHistoricalMessage(t, msgs[0])
	assertSyncRequestBody(t, gotPath, body)
}

func assertDecodedHistoricalMessage(t *testing.T, m HistoricalMessage) {
	t.Helper()
	if m.FromUID != "user1" || m.FromName != "Alice" || m.Content != "hello world" || m.MessageSeq != 5 || m.Type != 1 {
		t.Fatalf("message mapped wrong: %+v", m)
	}
}

func assertSyncRequestBody(t *testing.T, gotPath string, body map[string]any) {
	t.Helper()
	if gotPath != "/v1/bot/messages/sync" {
		t.Fatalf("path = %q", gotPath)
	}
	if int(body["channel_type"].(float64)) != int(ChannelGroup) || int(body["limit"].(float64)) != 10 {
		t.Fatalf("request body wrong: %+v", body)
	}
	if int(body["pull_mode"].(float64)) != 1 {
		t.Fatalf("pull_mode must be 1: %+v", body["pull_mode"])
	}
}

// TestGetChannelMessagesClientCap caps the returned slice when the server over-returns.
func TestGetChannelMessagesClientCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"messages":[{"from_uid":"a","content":"1","message_seq":1},{"from_uid":"b","content":"2","message_seq":2},{"from_uid":"c","content":"3","message_seq":3}]}`))
	}))
	defer srv.Close()
	c := NewRESTClient(srv.URL, tok("bf"))
	msgs := c.GetChannelMessages(context.Background(), "g1", ChannelGroup, 2)
	if len(msgs) != 2 {
		t.Fatalf("client cap not applied: got %d, want 2", len(msgs))
	}
}

// TestGetChannelMessagesErrorReturnsNil: any HTTP failure yields nil (the agent
// runs fine without history).
func TestGetChannelMessagesErrorReturnsNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()
	c := NewRESTClient(srv.URL, tok("bf"))
	if msgs := c.GetChannelMessages(context.Background(), "g1", ChannelGroup, 10); msgs != nil {
		t.Fatalf("expected nil on HTTP error, got %+v", msgs)
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
			RobotID: "r", IMToken: "imtok", WSURL: "ws://" + r.Host + "/ws",
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

// SendMessageResult.MessageID arrives as a JSON string from some octo deploys
// and a JSON number from others — both must decode to a string. A strict
// string-only decode caused #bug-2025-06: failed parse → "transient error" →
// retry with a fresh client_msg_no → user received two copies of every reply.
func TestSendMessageResultFlexMessageID(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"string", `{"message_id":"abc-123","client_msg_no":"x","message_seq":1}`, "abc-123"},
		{"uint64", `{"message_id":2068704596913459200,"client_msg_no":"x","message_seq":2}`, "2068704596913459200"},
		{"null", `{"message_id":null,"client_msg_no":"x","message_seq":3}`, ""},
		{"missing", `{"client_msg_no":"x","message_seq":4}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got SendMessageResult
			if err := json.Unmarshal([]byte(tc.body), &got); err != nil {
				t.Fatalf("decode failed: %v", err)
			}
			if string(got.MessageID) != tc.want {
				t.Fatalf("MessageID = %q, want %q", got.MessageID, tc.want)
			}
		})
	}
}

func TestValidateWSURL(t *testing.T) {
	cases := []struct {
		name, ws, api string
		ok            bool
	}{
		{"wss to same host", "wss://api.example/ws", "https://api.example", true},
		// Dev mode: same loopback host AND same port (port equality is part
		// of the new defense-in-depth check; a real dev deployment terminates
		// REST and WS on the same listener).
		{"ws to loopback dev", "ws://127.0.0.1:8080/ws", "http://127.0.0.1:8080", true},
		// Same loopback host but a DIFFERENT port: refused. A compromised
		// dev server could otherwise redirect the credentialed handshake to
		// an attacker-controlled port on localhost.
		{"reject same-host different-port", "ws://127.0.0.1:9090/ws", "http://127.0.0.1:8080", false},
		{"reject plaintext over https api", "ws://api.example/ws", "https://api.example", false},
		{"reject cross-host (sibling subdomain hop)", "wss://logger.example/ws", "https://api.example", false},
		{"reject bogus scheme", "http://api.example/ws", "https://api.example", false},
		{"reject empty host", "wss:///ws", "https://api.example", false},
		{"reject unparseable", "::not-a-url", "https://api.example", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateWSURL(tc.ws, tc.api)
			if tc.ok && err != nil {
				t.Fatalf("want ok, got err: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("want err, got ok")
			}
		})
	}
}

// TestSendTextAsWithMsgNoUsesCallerID is the regression for the duplicate-IM
// hazard rounds 1-7 chased: when sendReplySegment retried after a transient
// failure, the old SendTextAs path generated a fresh client_msg_no per call,
// defeating server-side dedup. The fix is the caller passing a stable id;
// this test pins the contract that the caller's id reaches the wire body
// unchanged across calls.
func TestSendTextAsWithMsgNoUsesCallerID(t *testing.T) {
	var seenIDs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		seenIDs = append(seenIDs, body["client_msg_no"].(string))
		_ = json.NewEncoder(w).Encode(SendMessageResult{MessageID: "m", MessageSeq: 1})
	}))
	defer srv.Close()
	c := NewRESTClient(srv.URL, tok("bf"))
	// Two calls with the same caller-supplied id simulate retry; server must
	// observe the same id both times (its dedup key).
	const stable = "fixed-uuid-for-test"
	if _, err := c.SendTextAsWithMsgNo(context.Background(), "c1", ChannelDM, "hi", nil, nil, false, "", stable); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SendTextAsWithMsgNo(context.Background(), "c1", ChannelDM, "hi", nil, nil, false, "", stable); err != nil {
		t.Fatal(err)
	}
	if len(seenIDs) != 2 || seenIDs[0] != stable || seenIDs[1] != stable {
		t.Fatalf("client_msg_no must be the caller-supplied id on every call, got %v", seenIDs)
	}
	// And the legacy SendTextAs path should still generate a fresh id per call
	// (that's the contract for one-shot non-retried sends).
	prev := len(seenIDs)
	if _, err := c.SendTextAs(context.Background(), "c1", ChannelDM, "hi", nil, nil, false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SendTextAs(context.Background(), "c1", ChannelDM, "hi", nil, nil, false, ""); err != nil {
		t.Fatal(err)
	}
	if seenIDs[prev] == seenIDs[prev+1] || seenIDs[prev] == stable {
		t.Fatalf("SendTextAs should generate fresh ids, got %v", seenIDs[prev:])
	}
}
