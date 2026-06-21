package octo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestConnectorAwaitsTokenBeforeRegister proves the await-token guard: with no
// token available, Run reports "awaiting secret" and never calls Register.
func TestConnectorAwaitsTokenBeforeRegister(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer srv.Close()

	c := NewConnector(NewRESTClient(srv.URL, func() string { return "" })) // token never arrives
	var awaiting int32
	c.OnStatus(func(connected bool, lastErr string) {
		if !connected && lastErr == "awaiting secret" {
			atomic.StoreInt32(&awaiting, 1)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = c.Run(ctx) // returns once ctx expires

	if atomic.LoadInt32(&awaiting) == 0 {
		t.Fatal("connector should report 'awaiting secret' when no token is set")
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("connector must not hit the API without a token, got %d requests", n)
	}
}

func TestMentionsBot(t *testing.T) {
	// explicit uid mention
	m := BotMessage{Payload: MessagePayload{Mention: &Mention{UIDs: []string{"bot1", "x"}}}}
	if !m.MentionsBot("bot1") {
		t.Fatal("should match explicit uid mention")
	}
	if m.MentionsBot("other") {
		t.Fatal("should not match a uid that isn't present")
	}
	// @ais (numbers decode as float64 from JSON, so test both)
	mAI := BotMessage{Payload: MessagePayload{Mention: &Mention{AIs: float64(1)}}}
	if !mAI.MentionsBot("bot1") {
		t.Fatal("@ais should address the bot")
	}
	// humans-only @all must NOT trigger the bot
	mAll := BotMessage{Payload: MessagePayload{Mention: &Mention{All: float64(1)}}}
	if mAll.MentionsBot("bot1") {
		t.Fatal("humans-only @all must not trigger the bot")
	}
	// no mention
	if (BotMessage{}).MentionsBot("bot1") {
		t.Fatal("no mention should be false")
	}
}

func TestSplitMessageBoundaries(t *testing.T) {
	// short text: single segment
	if got := splitMessage("hello", 100); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("short: %v", got)
	}
	// splits on paragraph boundary
	text := strings.Repeat("a", 30) + "\n\n" + strings.Repeat("b", 30)
	segs := splitMessage(text, 40)
	if len(segs) < 2 {
		t.Fatalf("expected split, got %d segments", len(segs))
	}
	if !strings.HasPrefix(segs[0], "a") || strings.Contains(segs[0], "b") {
		t.Fatalf("first segment should be the a-run: %q", segs[0])
	}
	// every segment within the cap
	for _, s := range segs {
		if len([]rune(s)) > 40 {
			t.Fatalf("segment exceeds cap: %d", len([]rune(s)))
		}
	}
}

func TestSplitMessageHardCut(t *testing.T) {
	// no boundary at all → hard cut into cap-sized chunks
	text := strings.Repeat("x", 250)
	segs := splitMessage(text, 100)
	if len(segs) != 3 {
		t.Fatalf("expected 3 hard-cut segments, got %d", len(segs))
	}
	total := 0
	for _, s := range segs {
		total += len([]rune(s))
	}
	if total != 250 {
		t.Fatalf("hard cut lost data: total=%d", total)
	}
}

func TestParsePayloadDefaults(t *testing.T) {
	p, err := parsePayload([]byte(`{"content":"hi"}`)) // no type
	if err != nil {
		t.Fatal(err)
	}
	if p.Content != "hi" || p.Type != 0 {
		t.Fatalf("payload defaults: %+v", p)
	}
	p2, err := parsePayload([]byte(`{"type":1,"content":"yo","mention":{"uids":["a"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if p2.Type != MsgText || p2.Mention == nil || p2.Mention.UIDs[0] != "a" {
		t.Fatalf("payload parse: %+v", p2)
	}
}

func TestSettingByteBits(t *testing.T) {
	// streamOn = bit1, topic = bit3
	if !settingStreamOn(0b00000010) {
		t.Fatal("streamOn bit1")
	}
	if settingStreamOn(0) {
		t.Fatal("streamOn should be false")
	}
	if !settingTopic(0b00001000) {
		t.Fatal("topic bit3")
	}
}

