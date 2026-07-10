package octo

import (
	"path/filepath"
	"testing"

	"github.com/lml2468/octobuddy/core/store"
	"github.com/lml2468/octobuddy/core/trigger"
)

// --- tier 1: in-memory bounded seenSet ---

func TestSeenSetFirstThenDuplicate(t *testing.T) {
	s := newSeenSet(8)
	if !s.markSeen("m1") {
		t.Fatal("first sighting must be firstSeen")
	}
	if s.markSeen("m1") {
		t.Fatal("duplicate must not be firstSeen")
	}
}

func TestSeenSetDistinctIDs(t *testing.T) {
	s := newSeenSet(8)
	for _, id := range []string{"a", "b", "c"} {
		if !s.markSeen(id) {
			t.Fatalf("id %q must be firstSeen", id)
		}
	}
}

// TestSeenSetEvictsOldest: past capacity the oldest id is evicted (so it would
// be firstSeen again), while recent ids stay deduped.
func TestSeenSetEvictsOldest(t *testing.T) {
	s := newSeenSet(2)
	s.markSeen("a")
	s.markSeen("b")
	s.markSeen("c") // evicts "a"
	if s.markSeen("c") {
		t.Fatal("c is recent — must still be deduped")
	}
	if s.markSeen("b") {
		t.Fatal("b is within cap — must still be deduped")
	}
	if !s.markSeen("a") {
		t.Fatal("a was evicted — firstSeen again is acceptable")
	}
}

// --- tier 1 integration: onInbound records the id in the memory set ---

// TestOnInboundMarksSeen: a delivered addressed message records its id in the
// tier-1 set, so a redelivery is recognized as a duplicate. Asserted via the
// set's own state (deterministic, no dependence on the async worker).
func TestOnInboundMarksSeen(t *testing.T) {
	c := NewConnector(NewRESTClient("http://x", func() string { return "tk" }))
	c.setUID("bot1")

	m := BotMessage{
		MessageID: "42", FromUID: "u_alice", ChannelID: "g1", ChannelType: ChannelGroup,
		Payload: MessagePayload{Type: MsgText, Content: "hi bot", Mention: &Mention{UIDs: []string{"bot1"}}},
	}
	c.onInbound(m)
	// After processing, the id is recorded → a fresh markSeen reports duplicate.
	if c.memFirstSeen("42") {
		t.Fatal("onInbound must record the messageID in the tier-1 set")
	}
}

// TestOnInboundDropsRedeliveryBeforeDispatch: a redelivered message id already
// in the tier-1 set is dropped by onInbound before reaching prepareInboundTurn.
// Verified by pre-seeding the set and asserting no reply target is registered.
func TestOnInboundDropsRedeliveryBeforeDispatch(t *testing.T) {
	c := NewConnector(NewRESTClient("http://x", func() string { return "tk" }))
	c.setUID("bot1")
	c.SetPolicy(trigger.Policy{BotUID: "bot1", AIBroadcast: trigger.AIBroadcastDeny})
	c.memFirstSeen("42") // pre-seed: pretend we already saw it

	m := BotMessage{
		MessageID: "42", FromUID: "u_alice", ChannelID: "g1", ChannelType: ChannelGroup,
		Payload: MessagePayload{Type: MsgText, Content: "hi bot", Mention: &Mention{UIDs: []string{"bot1"}}},
	}
	c.onInbound(m)
	if _, ok := c.peekQueuedTarget("g1"); ok {
		t.Fatal("a duplicate messageID must be dropped before enqueue")
	}
}

// TestOnInboundDistinctIDEnqueues: a fresh (unseen) addressed message enqueues.
func TestOnInboundDistinctIDEnqueues(t *testing.T) {
	c := NewConnector(NewRESTClient("http://x", func() string { return "tk" }))
	c.setUID("bot1")
	c.SetPolicy(trigger.Policy{BotUID: "bot1", AIBroadcast: trigger.AIBroadcastDeny})
	m := BotMessage{
		MessageID: "1", FromUID: "u_alice", ChannelID: "g1", ChannelType: ChannelGroup,
		Payload: MessagePayload{Type: MsgText, Content: "hi bot", Mention: &Mention{UIDs: []string{"bot1"}}},
	}
	c.onInbound(m)
	if _, ok := c.peekQueuedTarget("g1"); !ok {
		t.Fatal("a fresh addressed message must enqueue a turn")
	}
}

// --- tier 2: persistent dedup on the turn worker ---

// TestPersistentDedupSkipsAcrossRestart: a messageID already recorded in the
// store (a prior process saw it) must not run a turn after restart.
func TestPersistentDedupSkipsAcrossRestart(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if first, _ := st.MarkSeenMessage("9"); !first {
		t.Fatal("precondition: first mark should be firstSeen")
	}

	c := &Connector{}
	c.SetStore(st)
	if c.turnFirstSeen("9") {
		t.Fatal("a messageID already in the store must NOT be firstSeen for the turn path")
	}
	if !c.turnFirstSeen("10") {
		t.Fatal("a fresh messageID must be firstSeen")
	}
}

// TestTurnFirstSeenFailOpenNilStore: no store → always process (dev/REPL).
func TestTurnFirstSeenFailOpenNilStore(t *testing.T) {
	c := &Connector{}
	if !c.turnFirstSeen("m1") {
		t.Fatal("nil store must fail open (process)")
	}
}

// TestTurnFirstSeenFailOpenOnStoreError: store error → process rather than drop.
func TestTurnFirstSeenFailOpenOnStoreError(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close() // subsequent Exec errors
	c := &Connector{}
	c.SetStore(st)
	if !c.turnFirstSeen("m1") {
		t.Fatal("store error must fail open (process)")
	}
}

// TestEmptyMessageIDNotDeduped: a synthetic/cron inbound with no messageID must
// never be dropped by either tier (empty id is not a real dedup key).
func TestEmptyMessageIDNotDeduped(t *testing.T) {
	s := newSeenSet(8)
	if !s.markSeen("") || s.markSeen("") {
		// empty id would collide; the connector must skip dedup for it, asserted
		// via the connector gate below.
	}
	c := &Connector{}
	if !c.turnFirstSeen("") {
		t.Fatal("empty messageID must always be processed (no dedup key)")
	}
	c2 := NewConnector(NewRESTClient("http://x", func() string { return "tk" }))
	if !c2.memFirstSeen("") || !c2.memFirstSeen("") {
		t.Fatal("empty messageID must always be first-seen in the memory tier")
	}
}

var _ = trigger.Policy{}
