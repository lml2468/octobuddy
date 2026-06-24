package octo

import (
	"github.com/lml2468/octobuddy/core/router"
	"github.com/lml2468/octobuddy/core/trigger"
)

// EnqueueCron enqueues a cron-fired turn onto the per-session worker so it
// serializes with real inbound on the same key. The
// target — including any persona on-behalf-of binding — travels with the
// queued turn so OnReply reads exactly the target the cron fire intended,
// even if a real inbound enqueued in between and tried to write its own
// target into the global map.
//
// Returns immediately. The actual gw.Handle call happens on the worker
// goroutine that drainTurns owns; the bot's shutdown chain
// (connector.WaitTurns + cm.Wait) ensures the in-flight turn finishes
// before the store closes.
//
// Persona-grantor stamp: when persona is configured, the cron reply
// speaks `on_behalf_of` the configured grantor — same identity as live
// replies. The trust boundary is cron.SetOwnerUID's foreign-CreatedBy
// prune: any task that survives that fence is operator-authored on this
// bot, and the operator-configured persona is allowed to speak for it.
// The persona is the cron's identity by design.
//
// Caller is expected to build inbound with Source=trigger.SourceCron and
// a Trigger=ReasonCron decision (so router.IsCron() bypasses the
// rate-limit + mention/blocklist gates). bot_cron.go's fireCronTask does
// this; tests may set it via NewCronTrigger below.
func (c *Connector) EnqueueCron(sessionKey, channelID string, channelType ChannelType, inbound router.InboundMessage) {
	tgt := replyTarget{channelID: channelID, channelType: channelType}
	policy, _ := c.loadPolicyAndClassifier()
	if policy.Grantor.Configured() {
		tgt.onBehalfOf = policy.Grantor.UID
	}
	c.enqueueTurn(sessionKey, inbound, tgt)
}

// NewCronTrigger is the canonical cron-fire trigger decision: delegates
// to the production classifier with Source=SourceCron, so the wire shape
// stays byte-equal to whatever an inbound classification produces. The
// previous hand-built TriggerDecision was a second cron-decision
// constructor that had to stay in sync with the classifier's cron rule
// (rule precedence #1); #116 collapsed both onto one path. Used by
// bot_cron.go (and any future cron-fire source).
func (c *Connector) NewCronTrigger() *trigger.TriggerDecision {
	policy, classifier := c.loadPolicyAndClassifier()
	d := classifier.Classify(trigger.CanonicalInbound{Source: trigger.SourceCron}, policy)
	return &d
}

// turnQueue is the per-session-key serial dispatch state (guarded by Connector.mu).
// pending holds turns awaiting execution in arrival order; running marks whether
// a worker goroutine is draining them. See enqueueTurn/drainTurns.
//
// Each pending entry carries its OWN reply target: the
// prior contract stored a SINGLE target per session key in c.targets, which
// onInbound and RegisterReplyTarget both wrote. Real inbound + a concurrent
// cron fire on the same session key would stomp the map and produce a
// mis-delivered reply + a silently-dropped one. With the target traveling
// with the queued turn, drainTurns is the only writer to c.targets, the
// write happens immediately before gw.Handle, and the per-turn OnReply
// reads exactly the target the producer attached.
type queuedTurn struct {
	inbound router.InboundMessage
	tgt     replyTarget
}

type turnQueue struct {
	pending []queuedTurn
	running bool
}

// enqueueTurn appends a turn to the per-session-key serial queue, starting a
// worker goroutine for the key if none is running. Same-key turns run FIFO; the
// worker exits when its queue drains, so idle keys hold no goroutine.
func (c *Connector) enqueueTurn(key string, inbound router.InboundMessage, tgt replyTarget) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	q := c.turnQueues[key]
	if q == nil {
		q = &turnQueue{}
		c.turnQueues[key] = q
	}
	q.pending = append(q.pending, queuedTurn{inbound: inbound, tgt: tgt})
	start := !q.running
	q.running = true
	// turnsWG.Add(1) MUST happen under c.mu so it cannot race WaitTurns:
	// WaitTurns sets c.closed=true under the same mu before calling
	// turnsWG.Wait(). With Add() outside the lock, a goroutine that passed
	// the closed check and was preempted could call Add(1) after WaitTurns
	// observed counter==0 and returned — that's sync.WaitGroup misuse
	// (Add concurrently with Wait) and the spawned drainTurns would run
	// gw.Handle on a closed store.
	if start {
		c.turnsWG.Add(1)
	}
	c.mu.Unlock()

	if start {
		go c.drainTurns(key)
	}
}

// drainTurns runs queued turns for one session key in order, then retires
// the queue. New arrivals during a turn are picked up before the worker
// exits, so a burst is handled by a single worker with no lost messages.
// The retroactive post-gate Observe is GONE — the trigger pipeline marks
// observations at classification time and gw.Observe runs inline (see
// dispatchInbound), so a "dropped" turn here is a real drop, not a
// disguised observation.
func (c *Connector) drainTurns(key string) {
	defer c.turnsWG.Done()
	for {
		c.mu.Lock()
		q := c.turnQueues[key]
		if q == nil || len(q.pending) == 0 {
			if q != nil {
				delete(c.turnQueues, key)
			}
			c.mu.Unlock()
			return
		}
		item := q.pending[0]
		// Zero the popped slot before reslicing — otherwise the popped
		// queuedTurn (carrying a full InboundMessage with potentially
		// large Text + Attachments) stays pinned in slot 0 of the backing
		// array until the slice gets reallocated by future appends. Under
		// bursty per-session traffic the queue can hold dead payloads for
		// its entire lifetime.
		q.pending[0] = queuedTurn{}
		q.pending = q.pending[1:]
		// Set the per-turn target IMMEDIATELY before releasing the lock
		// and running gw.Handle, so OnReply (which reads via
		// c.target(key)) observes exactly the target the producer
		// attached. drainTurns is the sole writer to c.targets, so
		// cron+inbound concurrent enqueues no longer race.
		c.targets[key] = item.tgt
		c.mu.Unlock()

		// Tests may enqueue without setting a gateway; skip dispatch in
		// that case so the queue still drains cleanly.
		if c.gateway == nil {
			continue
		}
		if _, err := c.gateway.Handle(c.ctx(), item.inbound); err != nil {
			c.logf("handle turn for %s: %v", key, err)
		}
	}
}

// WaitTurns blocks until every drainTurns goroutine spawned by this
// connector has finished its queue. Call this on graceful shutdown AFTER
// the Run-ctx is cancelled (which closes the WS read loop and stops new
// turns from being enqueued) and BEFORE closing the store / gateway / driver.
//
// Idempotent: a connector that has never enqueued a turn returns immediately.
//
// Sets the `closed` flag first so any late enqueueTurn call (a cron tick
// that landed between Run returning and the bot's cm.Stop firing) is
// refused at the door rather than spawning a fresh drainTurns into a
// freshly-closed store.
func (c *Connector) WaitTurns() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.turnsWG.Wait()
}
