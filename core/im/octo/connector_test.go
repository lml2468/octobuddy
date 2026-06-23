package octo

import (
	"context"
	"encoding/json"
	"io"
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

// TestQueuedTurnsCarryOwnTarget is the regression forcron and
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

// TestEnqueueCronCarriesPersonaGrantor proves that cron fires from a
// persona-OBO bot always stamp the grantor onto the queued target so the
// cron reply speaks `on_behalf_of` the same identity as live replies.
// The trust boundary is cron.SetOwnerUID's foreign-CreatedBy prune: any task that survives that pruning belongs to the
// current owner and the operator-configured persona is allowed to speak
// for it. dropped the `taskCreatedBy == c.persona.UID` filter
// here because, in production, task.CreatedBy is the bot OWNER uid (set
// from server-resolved OwnerUID), not the persona grantor uid — so the
// filter was effectively dead code and persona-clone cron always replied
// as the bot, never as the grantor.
func TestEnqueueCronCarriesPersonaGrantor(t *testing.T) {
	c := NewConnector(NewRESTClient("http://x", func() string { return "tk" }))
	c.SetPersona(persona.Grantor{UID: "u_grantor", Name: "Admin"})
	const key = "dm:bot1:peer"
	// Task authored by the bot owner (the production case): stamp onBehalfOf.
	// taskCreatedBy is accepted for tracing but doesn't gate the stamp anymore.
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

	// Sanity: with NO persona configured, no onBehalfOf stamp.
	c2 := NewConnector(NewRESTClient("http://x", func() string { return "tk" }))
	c2.EnqueueCron(key, "cron-channel", ChannelDM, router.InboundMessage{ChannelID: "cron-channel", ChannelType: router.ChannelDM, Text: "daily"})
	c2.mu.Lock()
	q2 := c2.turnQueues[key]
	if q2 == nil || len(q2.pending) != 1 {
		c2.mu.Unlock()
		t.Fatalf("expected 1 queued cron item, got %v", q2)
	}
	if got := q2.pending[0].tgt.onBehalfOf; got != "" {
		c2.mu.Unlock()
		t.Errorf("non-persona bot must not stamp onBehalfOf; got %q", got)
	}
	c2.mu.Unlock()
}

// TestNoTargetMapStompUnderConcurrentEnqueue is the regression for F1:
// kept c.targets[key] = tgt in onInbound "for the persona tests", which
// put the stomp race BACK — OnReply for an in-flight turn would read whichever
// target the LAST onInbound wrote, not the one drainTurns popped. With that
// kept-write deleted, drainTurns is the SOLE writer of c.targets and the
// race goes away.
//
// The test asserts the structural invariant: enqueueTurn (called from
// onInbound) and EnqueueCron (called from fireCronTask) both store their
// target ON THE QUEUE ITEM and do not touch c.targets. With no other writer,
// two queue items for the same key carry their own targets unconditionally.
// A future regression that re-introduces a parallel writer would show up
// here as a tgt overwrite.
func TestNoTargetMapStompUnderConcurrentEnqueue(t *testing.T) {
	c := NewConnector(NewRESTClient("http://x", func() string { return "tk" }))
	c.setUID("bot1")
	const key = "dm:bot1:peer"

	// Pre-seed the queue with running=true so drainTurns doesn't auto-spawn
	// and steal items before we can peek. This isolates the question we
	// actually want to answer: do enqueueTurn / EnqueueCron preserve each
	// caller's tgt without touching the shared c.targets map?
	c.mu.Lock()
	c.turnQueues[key] = &turnQueue{running: true}
	c.mu.Unlock()

	tgtA := replyTarget{channelID: "chanA", channelType: ChannelDM}
	c.enqueueTurn(key, router.InboundMessage{ChannelID: "chanA", ChannelType: router.ChannelDM, Text: "A"}, tgtA)
	c.EnqueueCron(key, "chanB", ChannelDM, router.InboundMessage{ChannelID: "chanB", ChannelType: router.ChannelDM, Text: "B"})

	// c.targets MUST be empty: no writer besides drainTurns (which we
	// prevented from running). A regression that re-adds an onInbound /
	// EnqueueCron direct-write would fail here.
	c.mu.Lock()
	if t0, ok := c.targets[key]; ok {
		c.mu.Unlock()
		t.Fatalf("c.targets[%q] should remain unwritten outside drainTurns, got %+v", key, t0)
	}
	q := c.turnQueues[key]
	if q == nil || len(q.pending) != 2 {
		c.mu.Unlock()
		t.Fatalf("expected 2 queued items, got %v", q)
	}
	if q.pending[0].tgt.channelID != "chanA" || q.pending[0].tgt.onBehalfOf != "" {
		c.mu.Unlock()
		t.Errorf("item 0 tgt corrupted by concurrent enqueue: %+v", q.pending[0].tgt)
	}
	if q.pending[1].tgt.channelID != "chanB" {
		c.mu.Unlock()
		t.Errorf("item 1 tgt corrupted by concurrent enqueue: %+v", q.pending[1].tgt)
	}
	c.mu.Unlock()
}

// TestBackfillFetchChannelType proves cold-start backfill sends the right
// channel_type to /v1/bot/messages/sync: ChannelGroup for a bare group id, but
// ChannelCommunityTopic for a thread (compound "<groupNo>____<shortId>") id —
// querying a thread id as a plain group is what made the server reject the sync
// with not_group_member.
func TestBackfillFetchChannelType(t *testing.T) {
	cases := []struct {
		name      string
		channelID string
		wantType  int
	}{
		{"bare group", "g1", int(ChannelGroup)},
		{"thread", "g1" + ThreadIDSeparator + "topic9", int(ChannelCommunityTopic)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(raw, &body)
				_, _ = w.Write([]byte(`{"messages":[]}`))
			}))
			defer srv.Close()

			c := NewConnector(NewRESTClient(srv.URL, func() string { return "tk" }))
			c.BackfillFetch(tc.channelID, 10)

			if body == nil {
				t.Fatal("server received no sync request")
			}
			if got := int(body["channel_type"].(float64)); got != tc.wantType {
				t.Fatalf("channel_type = %d, want %d", got, tc.wantType)
			}
			if got := body["channel_id"].(string); got != tc.channelID {
				t.Fatalf("channel_id = %q, want %q", got, tc.channelID)
			}
		})
	}
}

// TestBackfillFetchTolerates403 proves a genuine membership failure (the bot is
// really not in the channel) degrades to nil rather than propagating — the agent
// runs fine without backfilled history. This guards the swallow-on-error path so
// the not_group_member fix doesn't accidentally start surfacing the error.
func TestBackfillFetchTolerates403(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"err.server.bot_api.not_group_member"},"status":400}`))
	}))
	defer srv.Close()

	c := NewConnector(NewRESTClient(srv.URL, func() string { return "tk" }))
	if got := c.BackfillFetch("g1", 10); got != nil {
		t.Fatalf("BackfillFetch on 403 = %v, want nil", got)
	}
}
