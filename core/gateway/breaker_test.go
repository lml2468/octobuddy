package gateway

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lml2468/octobuddy/core/agent"
	"github.com/lml2468/octobuddy/core/router"
)

func newTestBreaker(threshold int, cooldown time.Duration, clk *time.Time) *breaker {
	return &breaker{
		now:       func() time.Time { return *clk },
		threshold: threshold,
		cooldown:  cooldown,
	}
}

// TestBreakerOpensAfterThreshold: N consecutive transient failures open it.
func TestBreakerOpensAfterThreshold(t *testing.T) {
	now := time.Unix(1000, 0)
	b := newTestBreaker(3, time.Minute, &now)
	for i := 0; i < 3; i++ {
		if !b.allow() {
			t.Fatalf("allow must be true before threshold (i=%d)", i)
		}
		b.onResult(true, false) // transient failure
	}
	if b.allow() {
		t.Fatal("breaker must be OPEN after 3 transient failures")
	}
}

// TestBreakerSuccessResetsCount: a success clears the consecutive count.
func TestBreakerSuccessResetsCount(t *testing.T) {
	now := time.Unix(1000, 0)
	b := newTestBreaker(3, time.Minute, &now)
	b.onResult(true, false)
	b.onResult(true, false)
	b.onResult(false, true) // success
	b.onResult(true, false) // one more transient
	if !b.allow() {
		t.Fatal("breaker must stay closed: success reset the count")
	}
}

// TestBreakerNonTransientDoesNotOpen: normal terminal errors never open it.
func TestBreakerNonTransientDoesNotOpen(t *testing.T) {
	now := time.Unix(1000, 0)
	b := newTestBreaker(3, time.Minute, &now)
	for i := 0; i < 5; i++ {
		b.onResult(false, false) // non-transient failure
	}
	if !b.allow() {
		t.Fatal("non-transient failures must not open the breaker")
	}
}

// TestBreakerHalfOpenProbeAfterCooldown: after cooldown the breaker allows one
// probe; a second call before a result stays gated.
func TestBreakerHalfOpenProbeAfterCooldown(t *testing.T) {
	now := time.Unix(1000, 0)
	b := newTestBreaker(3, time.Minute, &now)
	for i := 0; i < 3; i++ {
		b.onResult(true, false)
	}
	if b.allow() {
		t.Fatal("must be open immediately after tripping")
	}
	now = now.Add(2 * time.Minute) // past cooldown
	if !b.allow() {
		t.Fatal("must half-open and allow one probe after cooldown")
	}
	if b.allow() {
		t.Fatal("second probe before a result must stay gated (half-open)")
	}
}

// TestBreakerHalfOpenSuccessCloses / FailureReopens.
func TestBreakerHalfOpenTransitions(t *testing.T) {
	now := time.Unix(1000, 0)

	// Success closes.
	b := newTestBreaker(3, time.Minute, &now)
	for i := 0; i < 3; i++ {
		b.onResult(true, false)
	}
	now = now.Add(2 * time.Minute)
	_ = b.allow() // half-open probe
	b.onResult(false, true)
	if !b.allow() {
		t.Fatal("half-open probe success must close the breaker")
	}

	// Failure reopens with a fresh cooldown.
	now = time.Unix(2000, 0)
	b2 := newTestBreaker(3, time.Minute, &now)
	for i := 0; i < 3; i++ {
		b2.onResult(true, false)
	}
	now = now.Add(2 * time.Minute)
	_ = b2.allow() // half-open probe
	b2.onResult(true, false)
	if b2.allow() {
		t.Fatal("half-open probe failure must re-open the breaker")
	}
}

// TestBreakerDisabledNil: a nil breaker always allows (opt-in).
func TestBreakerDisabledNil(t *testing.T) {
	var b *breaker
	for i := 0; i < 10; i++ {
		if !b.allow() {
			t.Fatal("nil breaker must always allow")
		}
		b.onResult(true, false) // must not panic
	}
}

// countingTransientDriver counts Query calls and always emits a transient
// terminal error — to prove the breaker short-circuits WITHOUT spawning.
type countingTransientDriver struct {
	mu      sync.Mutex
	queries int
}

func (d *countingTransientDriver) Name() string { return "fake" }
func (d *countingTransientDriver) Capabilities() agent.Capabilities {
	return agent.Capabilities{Resume: true}
}
func (d *countingTransientDriver) count() int { d.mu.Lock(); defer d.mu.Unlock(); return d.queries }
func (d *countingTransientDriver) Query(ctx context.Context, req agent.Request) (<-chan agent.AgentEvent, error) {
	d.mu.Lock()
	d.queries++
	d.mu.Unlock()
	ch := make(chan agent.AgentEvent, 2)
	go func() {
		defer close(ch)
		ch <- agent.AgentEvent{Kind: agent.KindError, Err: "429 rate limit", Transient: true}
	}()
	return ch, nil
}

func dmMsg(text string) router.InboundMessage {
	return router.InboundMessage{ChannelType: router.ChannelDM, FromUID: "u1", FromName: "a", Text: text}
}

// TestBreakerShortCircuitsWithoutSpawn: once tripped, the next turn returns
// busyReply and does NOT call driver.Query.
func TestBreakerShortCircuitsWithoutSpawn(t *testing.T) {
	drv := &countingTransientDriver{}
	sink := newCaptureSink()
	gw := New(drv, newTestStore(t), router.New(router.Config{MaxPerMinute: 100}), sink).
		WithCircuitBreaker(3, time.Minute)

	// 3 transient failures trip the breaker (each spawns).
	for i := 0; i < 3; i++ {
		if _, err := gw.Handle(context.Background(), dmMsg("hi")); err != nil {
			t.Fatal(err)
		}
	}
	tripped := drv.count()
	if tripped != 3 {
		t.Fatalf("first 3 turns must each spawn, got %d", tripped)
	}
	// 4th turn: breaker open → busyReply, NO new spawn.
	if _, err := gw.Handle(context.Background(), dmMsg("hi")); err != nil {
		t.Fatal(err)
	}
	if drv.count() != tripped {
		t.Fatalf("open breaker must not spawn: query count went %d→%d", tripped, drv.count())
	}
	if sink.replies["u1"] != busyReply {
		t.Fatalf("open breaker must reply busyReply, got %q", sink.replies["u1"])
	}
}

// TestBreakerDisabledByDefault: without WithCircuitBreaker, every transient
// failure still spawns (no short-circuit) — opt-in back-compat.
func TestBreakerDisabledByDefault(t *testing.T) {
	drv := &countingTransientDriver{}
	gw := New(drv, newTestStore(t), router.New(router.Config{MaxPerMinute: 100}), newCaptureSink())
	for i := 0; i < 5; i++ {
		if _, err := gw.Handle(context.Background(), dmMsg("hi")); err != nil {
			t.Fatal(err)
		}
	}
	if drv.count() != 5 {
		t.Fatalf("no breaker: all 5 turns must spawn, got %d", drv.count())
	}
}

// recoverableDriver fails transiently until flipped healthy, then succeeds.
type recoverableDriver struct {
	mu      sync.Mutex
	healthy bool
	queries int
}

func (d *recoverableDriver) Name() string { return "fake" }
func (d *recoverableDriver) Capabilities() agent.Capabilities {
	return agent.Capabilities{Resume: true}
}
func (d *recoverableDriver) Query(ctx context.Context, req agent.Request) (<-chan agent.AgentEvent, error) {
	d.mu.Lock()
	d.queries++
	healthy := d.healthy
	d.mu.Unlock()
	ch := make(chan agent.AgentEvent, 3)
	go func() {
		defer close(ch)
		if healthy {
			ch <- agent.AgentEvent{Kind: agent.KindSessionStarted, SessionID: "s"}
			ch <- agent.AgentEvent{Kind: agent.KindTextDelta, Text: "ok"}
			ch <- agent.AgentEvent{Kind: agent.KindTurnDone}
			return
		}
		ch <- agent.AgentEvent{Kind: agent.KindError, Err: "429 rate limit", Transient: true}
	}()
	return ch, nil
}

// TestBreakerRecoversAfterCooldown: after tripping and the cooldown elapsing, a
// half-open probe against a now-healthy driver spawns again and closes the
// breaker.
func TestBreakerRecoversAfterCooldown(t *testing.T) {
	drv := &recoverableDriver{}
	sink := newCaptureSink()
	gw := New(drv, newTestStore(t), router.New(router.Config{MaxPerMinute: 100}), sink).
		WithCircuitBreaker(3, time.Minute)
	// Drive the breaker's clock deterministically.
	now := time.Unix(5000, 0)
	gw.breaker.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		if _, err := gw.Handle(context.Background(), dmMsg("hi")); err != nil {
			t.Fatal(err)
		}
	}
	// Open now: a turn short-circuits without spawning.
	spawnsAtTrip := drv.queries
	if _, err := gw.Handle(context.Background(), dmMsg("hi")); err != nil {
		t.Fatal(err)
	}
	if drv.queries != spawnsAtTrip {
		t.Fatalf("open breaker must not spawn, went %d→%d", spawnsAtTrip, drv.queries)
	}
	// Heal + advance past cooldown → half-open probe spawns and succeeds.
	drv.mu.Lock()
	drv.healthy = true
	drv.mu.Unlock()
	now = now.Add(2 * time.Minute)
	if _, err := gw.Handle(context.Background(), dmMsg("hi")); err != nil {
		t.Fatal(err)
	}
	if drv.queries != spawnsAtTrip+1 {
		t.Fatalf("half-open probe must spawn once, went %d→%d", spawnsAtTrip, drv.queries)
	}
	if sink.replies["u1"] != "ok" {
		t.Fatalf("recovered turn must deliver real reply, got %q", sink.replies["u1"])
	}
}

// queryErrorDriver returns a top-level error from Query (fork/exec-style
// failure) — the path that bypasses deliverTurn. Configurable so a probe can be
// made to fail-to-spawn.
type queryErrorDriver struct {
	mu       sync.Mutex
	failNext bool
	queries  int
}

func (d *queryErrorDriver) Name() string { return "fake" }
func (d *queryErrorDriver) Capabilities() agent.Capabilities {
	return agent.Capabilities{Resume: true}
}
func (d *queryErrorDriver) Query(ctx context.Context, req agent.Request) (<-chan agent.AgentEvent, error) {
	d.mu.Lock()
	d.queries++
	fail := d.failNext
	d.mu.Unlock()
	if fail {
		return nil, context.Canceled // top-level Query error → bypasses deliverTurn
	}
	ch := make(chan agent.AgentEvent, 3)
	go func() {
		defer close(ch)
		ch <- agent.AgentEvent{Kind: agent.KindSessionStarted, SessionID: "s"}
		ch <- agent.AgentEvent{Kind: agent.KindTextDelta, Text: "ok"}
		ch <- agent.AgentEvent{Kind: agent.KindTurnDone}
	}()
	return ch, nil
}

// TestBreakerHalfOpenProbeQueryErrorDoesNotWedge is the regression for the P5
// reviewer's Critical finding: a half-open probe whose driver.Query returns a
// top-level error (never reaching deliverTurn) must still resolve the breaker
// via executeTurn's error-path onResult — otherwise it wedges in half-open
// forever. After such a probe the breaker must not be permanently stuck: a
// following healthy turn must spawn and succeed.
func TestBreakerHalfOpenProbeQueryErrorDoesNotWedge(t *testing.T) {
	drv := &queryErrorDriver{}
	sink := newCaptureSink()
	gw := New(drv, newTestStore(t), router.New(router.Config{MaxPerMinute: 100}), sink).
		WithCircuitBreaker(1, time.Minute) // threshold 1 for brevity
	now := time.Unix(9000, 0)
	gw.breaker.now = func() time.Time { return now }

	// First turn: healthy spawn (breaker closed) — succeeds.
	if _, err := gw.Handle(context.Background(), dmMsg("hi")); err != nil {
		t.Fatal(err)
	}
	// Force the breaker OPEN by making a turn fail transiently. Simulate by
	// directly feeding a transient result (the queryErrorDriver can't emit a
	// stream transient), which is the documented onResult contract.
	gw.breaker.onResult(true, false) // 1 transient → open (threshold 1)
	if gw.breaker.allow() {
		t.Fatal("breaker should be open")
	}
	// Advance past cooldown → next allow() half-opens. Make the probe's Query
	// fail at the top level (the wedge path).
	now = now.Add(2 * time.Minute)
	drv.mu.Lock()
	drv.failNext = true
	drv.mu.Unlock()
	if _, err := gw.Handle(context.Background(), dmMsg("probe")); err != nil {
		t.Fatal(err)
	}
	// The probe failed to spawn; the breaker must NOT be wedged in half-open.
	// A healthy turn now must be allowed to spawn and succeed.
	drv.mu.Lock()
	drv.failNext = false
	drv.mu.Unlock()
	if _, err := gw.Handle(context.Background(), dmMsg("recover")); err != nil {
		t.Fatal(err)
	}
	if sink.replies["u1"] != "ok" {
		t.Fatalf("breaker wedged: recovery turn did not spawn/succeed, reply=%q", sink.replies["u1"])
	}
}
