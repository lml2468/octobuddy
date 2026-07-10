package octo

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/lml2468/octobuddy/core/persona"
	"github.com/lml2468/octobuddy/core/router"
	"github.com/lml2468/octobuddy/core/trigger"
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

// TestReconnectDoesNotReRegister proves the duplicate-login-kick fix: after the
// first successful register, a plain transport drop must reconnect by REUSING
// the cached registration, NOT by calling /v1/bot/register again (which the
// server turns into an UpdateIMToken that kicks the freshly-rebuilt session).
func TestReconnectDoesNotReRegister(t *testing.T) {
	var registerHits, wsHits int32
	connack := buildConnack(t)
	upgrader := websocket.Upgrader{}

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/v1/bot/register", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&registerHits, 1)
		// ws_url must share host:port with apiURL (validateWSURL) and be ws://
		// since the test apiURL is http://.
		wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
		_ = json.NewEncoder(w).Encode(map[string]string{
			"robot_id": "bot-uid", "im_token": "tok", "ws_url": wsURL,
			"api_url": srv.URL, "owner_uid": "owner-uid",
		})
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		atomic.AddInt32(&wsHits, 1)
		if _, _, err := c.ReadMessage(); err != nil { // CONNECT
			return
		}
		if err := c.WriteMessage(websocket.BinaryMessage, connack); err != nil {
			return
		}
		// Drop the connection right after the handshake to force a reconnect.
	})

	c := NewConnector(NewRESTClient(srv.URL, func() string { return "tok" }))
	c.reconnectBase = 5 * time.Millisecond // keep the test fast
	c.reconnectMax = 5 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_ = c.Run(ctx) // returns once ctx expires

	// Many WS reconnects (drop-after-handshake loops at reconnectBase cadence)…
	if n := atomic.LoadInt32(&wsHits); n < 2 {
		t.Fatalf("expected multiple WS reconnects, got %d", n)
	}
	// …but exactly ONE register (the initial one); reconnects reuse the cache.
	if n := atomic.LoadInt32(&registerHits); n != 1 {
		t.Fatalf("reconnect must not re-register: got %d register hits, want 1", n)
	}
}

// TestTriggerMentionWireProjection covers the wire→trigger projection's
// shape for the @bot / @AI / @所有人 / nil cases. The semantic "should
// this trigger" lives in trigger.DefaultClassifier; this test only
// asserts the wire→canonical mapping.
func TestTriggerMentionWireProjection(t *testing.T) {
	cases := []struct {
		name string
		in   *Mention
		want func(*trigger.MentionPayload) bool
	}{
		{"explicit @bot uid", &Mention{UIDs: []string{"bot1"}}, func(m *trigger.MentionPayload) bool {
			return m != nil && len(m.UIDs) == 1 && m.UIDs[0] == "bot1"
		}},
		{"@ais", &Mention{AIs: float64(1)}, func(m *trigger.MentionPayload) bool {
			return m != nil && m.AIsFlag && !m.HumansFlag && !m.AllFlag
		}},
		{"@all", &Mention{All: float64(1)}, func(m *trigger.MentionPayload) bool {
			return m != nil && m.AllFlag && !m.AIsFlag
		}},
		{"nil", nil, func(m *trigger.MentionPayload) bool { return m == nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := BotMessage{Payload: MessagePayload{Mention: tc.in}}.TriggerMention()
			if !tc.want(m) {
				t.Fatalf("TriggerMention=%+v not as expected", m)
			}
		})
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
	c.enqueueTurn(key, router.InboundMessage{ChannelID: "chanA", ChannelType: router.ChannelDM, Text: "A"}, tgtA, "")
	c.enqueueTurn(key, router.InboundMessage{ChannelID: "chanB", ChannelType: router.ChannelDM, Text: "B"}, tgtB, "")

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
	c.SetPolicy(trigger.Policy{
		BotUID:  "bot1",
		Grantor: trigger.FromPersonaGrantor(persona.Grantor{UID: "u_grantor", Name: "Admin"}),
	})
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
	c.enqueueTurn(key, router.InboundMessage{ChannelID: "chanA", ChannelType: router.ChannelDM, Text: "A"}, tgtA, "")
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
