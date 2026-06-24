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

// TestGetChannelMessagesFlexMessageID: WuKongIM serves message_id as either a
// JSON string or a JSON number across deploys. A strict string decode used to
// fail the whole /v1/bot/messages/sync response and silently drop every
// historical message ("json: cannot unmarshal number ... message_id of type
// string" — observed 2026-06-24 against the live server). Both shapes must
// round-trip to HistoricalMessage.MessageID as a string with the literal
// digits preserved (uint64 precision intact).
func TestGetChannelMessagesFlexMessageID(t *testing.T) {
	cases := []struct {
		name   string
		idJSON string // exact JSON literal placed at "message_id"
		wantID string
	}{
		{"string", `"m-abc-1"`, "m-abc-1"},
		{"number", `1832745982637158400`, "1832745982637158400"},
		{"null", `null`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"messages":[{"from_uid":"u","content":"hi","message_seq":1,"message_id":` + tc.idJSON + `}]}`
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()
			c := NewRESTClient(srv.URL, tok("bf"))
			msgs := c.GetChannelMessages(context.Background(), "g1", ChannelGroup, 10)
			if len(msgs) != 1 {
				t.Fatalf("expected 1 message, got %d (response body would be silently dropped on strict decode)", len(msgs))
			}
			if msgs[0].MessageID != tc.wantID {
				t.Fatalf("MessageID = %q, want %q", msgs[0].MessageID, tc.wantID)
			}
		})
	}
}
