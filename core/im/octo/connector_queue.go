package octo

import (
	"github.com/lml2468/octobuddy/core/router"
	"github.com/lml2468/octobuddy/core/trigger"
)

// EnqueueCron enqueues a cron-fired turn on the per-session worker so it
// serializes with real inbound on the same key. Returns immediately; the
// gw.Handle call runs on drainTurns's worker and the shutdown chain
// (WaitTurns + cm.Wait) ensures the in-flight turn finishes before the
// store closes.
//
// inbound.Trigger.ReplyRouting.OnBehalfOf is the single source of truth
// for the persona-grantor stamp — populated by NewCronTrigger via the
// classifier's cron rule. If caller forgot to set Trigger we auto-fill
// here so the inbound can't silently classify as a regular user message
// (and hit rate-limit / mention gates).
func (c *Connector) EnqueueCron(sessionKey, channelID string, channelType ChannelType, inbound router.InboundMessage) {
	if inbound.Trigger == nil {
		inbound.Source = trigger.SourceCron
		inbound.Trigger = c.NewCronTrigger()
	}
	tgt := replyTarget{channelID: channelID, channelType: channelType}
	tgt.onBehalfOf = inbound.Trigger.ReplyRouting.OnBehalfOf
	c.enqueueTurn(sessionKey, inbound, tgt, "") // cron: synthetic, no messageID → never deduped
}

// NewCronTrigger delegates to the production classifier with
// Source=SourceCron so the cron-fire decision shape stays byte-equal to
// what a regular inbound classification produces.
func (c *Connector) NewCronTrigger() *trigger.TriggerDecision {
	policy, classifier := c.loadPolicyAndClassifier()
	d := classifier.Classify(trigger.CanonicalInbound{Source: trigger.SourceCron}, policy)
	return &d
}

// turnQueue is the per-session-key serial dispatch state (guarded by
// Connector.mu). Each pending entry carries its own reply target so
// drainTurns is the sole writer to c.targets and a concurrent cron +
// inbound on the same key can't stomp each other's destination.
type queuedTurn struct {
	inbound router.InboundMessage
	tgt     replyTarget
	// messageID is the octo inbound id for tier-2 persistent dedup, checked on
	// this worker goroutine (off the read loop) before Handle. "" for synthetic
	// turns (cron) → never deduped.
	messageID string
}

type turnQueue struct {
	pending []queuedTurn
	running bool
}

// enqueueTurn appends a turn to the per-session-key serial queue,
// starting a worker if none is running. Same-key turns run FIFO; the
// worker exits when its queue drains so idle keys hold no goroutine.
func (c *Connector) enqueueTurn(key string, inbound router.InboundMessage, tgt replyTarget, messageID string) {
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
	q.pending = append(q.pending, queuedTurn{inbound: inbound, tgt: tgt, messageID: messageID})
	start := !q.running
	q.running = true
	// turnsWG.Add MUST happen under c.mu so it can't race WaitTurns:
	// WaitTurns flips c.closed under the same mu before Wait(). With Add
	// outside the lock, a goroutine preempted past the closed check could
	// Add(1) after Wait observed counter==0 (sync.WaitGroup misuse) and
	// the spawned drainTurns would run gw.Handle on a closed store.
	if start {
		c.turnsWG.Add(1)
	}
	c.mu.Unlock()

	if start {
		go c.drainTurns(key)
	}
}

// drainTurns runs queued turns for one session key in order, then
// retires the queue. New arrivals during a turn are picked up before
// the worker exits, so a burst is handled by a single worker.
func (c *Connector) drainTurns(key string) {
	defer c.turnsWG.Done()
	// Defensive recover at the goroutine boundary: gateway.runTurn recovers
	// panics in the turn body, but a panic in the routing/locking bookkeeping
	// around it (gateway.Handle, router.RouteAndHandle) would otherwise escape
	// this bare goroutine and crash the whole daemon, taking down every session
	// for the bot. Degrade to a logged dropped turn instead.
	defer func() {
		if r := recover(); r != nil {
			c.logf("drainTurns panic for %s: %v", key, r)
		}
	}()
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
		// queuedTurn (full InboundMessage) stays pinned in slot 0 of the
		// backing array until the slice gets reallocated, leaking under
		// sustained per-session traffic.
		q.pending[0] = queuedTurn{}
		q.pending = q.pending[1:]
		// Publish the per-turn target before releasing the lock so
		// OnReply (which reads via c.target(key)) observes the right
		// destination.
		c.targets[key] = item.tgt
		c.mu.Unlock()

		// Tests may enqueue without a gateway; let the queue drain.
		if c.gateway == nil {
			continue
		}
		// Tier-2 persistent dedup (off the read loop): a turn whose messageID a
		// prior process already ran must not spawn again after a restart. The
		// message was already ack'd on the read loop, so a duplicate is dropped
		// here silently. Fails open (turnFirstSeen).
		if !c.turnFirstSeen(item.messageID) {
			continue
		}
		if _, err := c.gateway.Handle(c.ctx(), item.inbound); err != nil {
			c.logf("handle turn for %s: %v", key, err)
		}
	}
}

// WaitTurns blocks until every drainTurns goroutine has finished.
// Idempotent. Call on graceful shutdown AFTER cancelling the Run ctx
// (closes the WS read loop, stops new enqueues) and BEFORE closing the
// store / gateway / driver.
//
// Flips c.closed first so a cron tick that lands between Run returning
// and cm.Stop firing is refused at the door rather than spawning a new
// drainTurns into a freshly-closed store.
func (c *Connector) WaitTurns() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.turnsWG.Wait()
}

// goTracked starts fn in a goroutine tracked by turnsWG so WaitTurns blocks on
// it during graceful shutdown. The Add happens under c.mu together with the
// c.closed check — WaitTurns flips c.closed under c.mu then Wait()s, so an Add
// outside the lock could land after Wait observed counter==0 (WaitGroup misuse)
// and run work against a tearing-down connector. Returns false (fn not started)
// when the connector is already closed. Used for off-queue background work
// (e.g. the system-message GROUP.md refresh); queue turns use enqueueTurn, which
// inlines the same invariant alongside its worker-start bookkeeping.
func (c *Connector) goTracked(fn func()) bool {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return false
	}
	c.turnsWG.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.turnsWG.Done()
		fn()
	}()
	return true
}
