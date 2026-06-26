package octo

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lml2468/octobuddy/core/gateway"
	"github.com/lml2468/octobuddy/core/trigger"
)

// Connector wires the Octo IM platform to the gateway: registers the bot,
// connects the WuKongIM socket, maps inbound BotMessages to
// router.InboundMessage, and (as a gateway.Sink) delivers replies via REST.
type Connector struct {
	rest    *RESTClient
	gateway *gateway.Gateway

	botUID string
	// ownerUID is the bot owner's server-authoritative uid, set after each
	// (re)registration (applyRegistration → notifyOwner). Guarded by c.mu.
	// Gates owner-only behavior (cron create/delete; bootstrap injection in the
	// owner's DM). Empty before the first registration.
	ownerUID string

	// names resolves uid→display-name and groupNo→channel-name (cached).
	// Powers sidebar conversation titles and chat-bubble sender labels.
	names *nameCache

	// policy is the trigger pipeline configuration. Owned ONLY here; set
	// at startup via SetPolicy. Hot-reload would build a fresh Policy and
	// call SetPolicy under policyMu.
	policy     trigger.Policy
	policyMu   sync.RWMutex
	classifier trigger.DefaultClassifier

	// runCtx is Run's context, atomically published for sink/inbound
	// callbacks (which the Sink interface doesn't thread a context
	// through) so a cancelled Run aborts in-flight turns. Atomic because
	// receipt worker / heartbeat / callbacks read it concurrently.
	runCtx atomic.Pointer[context.Context]

	mu      sync.Mutex
	targets map[string]replyTarget   // sessionKey → where to send the reply
	typers  map[string]*typingTicker // sessionKey → active typing heartbeat
	sock    *socketConn
	closed  bool

	// turnQueues serializes turn dispatch PER session key so the WS read
	// loop is never blocked by a running turn. Same-key turns stay FIFO
	// (one worker drains its queue); different keys run concurrently.
	turnQueues map[string]*turnQueue

	// toolProgress mirrors agent tool invocations to the channel as it
	// runs (opt-in). progress holds per-turn dedup/cap state keyed by
	// sessionKey. Both guarded by c.mu.
	toolProgress bool
	progress     map[string]*toolProgressState

	// typingInterval is the heartbeat period between typing pings.
	// sendTyping defaults to rest.SendTyping; swappable in tests.
	typingInterval time.Duration
	sendTyping     func(ctx context.Context, channelID string, channelType ChannelType) error

	// onStatus fires on connection-state transitions; onOwner fires after
	// each (re)register with the bot owner uid (gates cron).
	onStatus func(connected bool, lastErr string)
	onOwner  func(ownerUID string)

	// turnsWG tracks every in-flight drainTurns goroutine so SIGTERM can
	// wait before closing the store — otherwise gateway.Handle's
	// resume-id save hits "database is closed" mid-flush, silently
	// breaking resume continuity.
	turnsWG sync.WaitGroup

	reconnectBase time.Duration
	reconnectMax  time.Duration

	// deviceID is the stable WuKongIM device id passed to every socketConn. Set
	// once at startup via SetDeviceID (empty falls back to a per-connection
	// random id); immutable after Run starts, so no lock needed. See the
	// socketConn.deviceID doc for the kick caveat.
	deviceID string

	// receiptCh buffers read-receipt requests for a single worker
	// (receiptWorker). Buffered so a slow REST back-end can't backpressure
	// the inbound read loop; a full buffer drops the receipt rather than
	// blocking.
	receiptCh chan readReceiptReq
}

type readReceiptReq struct {
	channelID   string
	channelType ChannelType
	messageID   string
}

// maxToolNotices caps the per-turn "🔧 Running …" notices so a tool-heavy
// run can't flood the channel.
const maxToolNotices = 10

type toolProgressState struct {
	lastNotice string // collapse consecutive duplicates
	count      int    // notices sent this turn
}

// awaitTokenPoll is how often Run rechecks for an injected token before
// it has one; short enough to connect promptly without busy-spinning.
const awaitTokenPoll = 2 * time.Second

// connectionStableAfter is how long a WS connection must have stayed up before
// a drop is treated as a fresh incident (reset the reconnect backoff) rather
// than part of a connect-fail storm. Comfortably above the connect+handshake
// time so a flapping endpoint still backs off, but well under any normal
// session length.
const connectionStableAfter = 30 * time.Second

// defaultTypingInterval is the typing-heartbeat period; the IM expires
// the indicator after this if it doesn't see another ping.
const defaultTypingInterval = 5 * time.Second

// OnStatus registers a connection-state callback. Setter takes c.mu so
// a late caller can't race notifyStatus reading the field.
func (c *Connector) OnStatus(fn func(connected bool, lastErr string)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onStatus = fn
}

// OnOwner registers a callback invoked with the bot owner uid after each
// (re)registration. Same lock discipline as OnStatus.
func (c *Connector) OnOwner(fn func(ownerUID string)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onOwner = fn
}

func (c *Connector) setStatus(connected bool, lastErr string) {
	c.mu.Lock()
	fn := c.onStatus
	c.mu.Unlock()
	if fn != nil {
		fn(connected, lastErr)
	}
}

func (c *Connector) notifyOwner(ownerUID string) {
	c.mu.Lock()
	if ownerUID != "" {
		c.ownerUID = ownerUID // cache for OwnerUID() (gates owner-only behavior)
	}
	fn := c.onOwner
	c.mu.Unlock()
	if fn != nil && ownerUID != "" {
		fn(ownerUID)
	}
}

// OwnerUID returns the bot owner's registered uid (empty before registration).
// Mirrors BotUID; passed to the gateway (WithOwner) to gate owner-only behavior
// such as bootstrap-prompt injection in the owner's DM.
func (c *Connector) OwnerUID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ownerUID
}

type replyTarget struct {
	channelID   string
	channelType ChannelType
	// onBehalfOf routes the reply (and typing) as the grantor for a
	// persona clone. Empty for normal replies.
	onBehalfOf string
}

// SetPolicy installs/replaces the trigger policy. Idempotent. Preserves
// the live server-registered uid: if setUID() already ran, the freshly-
// installed policy still carries the post-register uid instead of
// falling back to whatever the caller passed (typically a stale config
// id). Without this, a SetPolicy after setUID would silently regress
// @bot-mention recognition.
func (c *Connector) SetPolicy(p trigger.Policy) {
	uid := c.uid()
	c.policyMu.Lock()
	if uid != "" {
		p.BotUID = uid
	}
	c.policy = p
	c.policyMu.Unlock()
}

func (c *Connector) loadPolicyAndClassifier() (trigger.Policy, trigger.DefaultClassifier) {
	c.policyMu.RLock()
	defer c.policyMu.RUnlock()
	return c.policy, c.classifier
}

// NewConnector builds a connector. The gateway must be constructed with
// this connector as its Sink.
func NewConnector(rest *RESTClient) *Connector {
	return &Connector{
		rest:          rest,
		names:         newNameCache(rest),
		classifier:    trigger.DefaultClassifier{},
		targets:       make(map[string]replyTarget),
		progress:      make(map[string]*toolProgressState),
		typers:        make(map[string]*typingTicker),
		turnQueues:    make(map[string]*turnQueue),
		reconnectBase: 3 * time.Second,
		reconnectMax:  60 * time.Second,
		receiptCh:     make(chan readReceiptReq, 64),
	}
}

// SetToolProgress enables mirroring tool invocations to the channel as
// "🔧 Running <tool>(<params>)…" notices. Off by default.
func (c *Connector) SetToolProgress(on bool) {
	c.mu.Lock()
	c.toolProgress = on
	c.mu.Unlock()
}

// SetDeviceID installs the stable WuKongIM device id sent in every CONNECT.
// Call once before Run; the id is read only on the (single) connect path.
// Empty leaves the per-connection random fallback in place.
func (c *Connector) SetDeviceID(id string) {
	c.deviceID = id
}
