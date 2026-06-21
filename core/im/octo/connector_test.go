package octo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lml2468/xclaw/core/persona"
	"github.com/lml2468/xclaw/core/router"
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

// TestQueuedTurnsCarryOwnTarget is the regression for round-8 F1-Arch: cron and
// real inbound used to share c.targets[key], so a concurrent enqueue could
// stomp one turn's target → the other turn's reply went to the wrong channel
// AND one reply was silently dropped. The fix attaches the target to each
// queued item; drainTurns rewrites c.targets[key] right before gw.Handle.
// This test simulates two items in the queue with DIFFERENT targets and
// verifies each reads back its own.
func TestQueuedTurnsCarryOwnTarget(t *testing.T) {
	c := NewConnector(NewRESTClient("http://x", func() string { return "tk" }))
	c.setUID("bot1")
	const key = "dm:bot1:peer"
	tgtA := replyTarget{channelID: "chanA", channelType: ChannelDM}
	tgtB := replyTarget{channelID: "chanB", channelType: ChannelDM, onBehalfOf: "u_grantor"}
	// Two queued items for the same key — order: A then B.
	c.enqueueTurn(key, router.InboundMessage{ChannelID: "chanA", ChannelType: router.ChannelDM, Text: "A"}, tgtA)
	c.enqueueTurn(key, router.InboundMessage{ChannelID: "chanB", ChannelType: router.ChannelDM, Text: "B"}, tgtB)

	// Peek at the queue under lock — both items must carry distinct targets.
	c.mu.Lock()
	q := c.turnQueues[key]
	if q == nil || len(q.pending) != 2 {
		c.mu.Unlock()
		t.Fatalf("expected 2 queued items, got %v", q)
	}
	if q.pending[0].tgt.channelID != "chanA" || q.pending[0].tgt.onBehalfOf != "" {
		c.mu.Unlock()
		t.Errorf("item 0 target lost: %+v", q.pending[0].tgt)
	}
	if q.pending[1].tgt.channelID != "chanB" || q.pending[1].tgt.onBehalfOf != "u_grantor" {
		c.mu.Unlock()
		t.Errorf("item 1 target lost / grantor stripped: %+v", q.pending[1].tgt)
	}
	c.mu.Unlock()
}

// TestEnqueueCronCarriesPersonaGrantor is the regression for the round-8
// F1-Arch persona slice: cron fires from a persona-OBO bot used to write a
// target with onBehalfOf="" via the deprecated RegisterReplyTarget, so the
// cron reply arrived from the bot identity while every other reply in the
// channel arrived from the grantor (visible persona breakage). EnqueueCron
// now stamps the persona grantor onto the queued target.
func TestEnqueueCronCarriesPersonaGrantor(t *testing.T) {
	c := NewConnector(NewRESTClient("http://x", func() string { return "tk" }))
	c.SetPersona(persona.Grantor{UID: "u_grantor", Name: "Admin"})
	const key = "dm:bot1:peer"
	c.EnqueueCron(key, "cron-channel", ChannelDM, router.InboundMessage{ChannelID: "cron-channel", ChannelType: router.ChannelDM, Text: "daily"})
	c.mu.Lock()
	q := c.turnQueues[key]
	if q == nil || len(q.pending) != 1 {
		c.mu.Unlock()
		t.Fatalf("expected 1 queued cron item, got %v", q)
	}
	if got := q.pending[0].tgt.onBehalfOf; got != "u_grantor" {
		c.mu.Unlock()
		t.Errorf("cron reply target dropped persona grantor: onBehalfOf=%q, want %q", got, "u_grantor")
	}
	c.mu.Unlock()
}
