package octo

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lml2468/octobuddy/core/gateway"
	"github.com/lml2468/octobuddy/core/trigger"
)

// Connector wires the Octo IM platform to the gateway: it registers the bot,
// connects the WuKongIM socket, maps inbound BotMessages to
// router.InboundMessage, and (as a gateway.Sink) delivers replies via REST. It
// is the IM-specific edge; everything inside the gateway stays IM-agnostic.
type Connector struct {
	rest    *RESTClient
	gateway *gateway.Gateway

	botUID string

	// names resolves uid→display-name (seeded for free from inbound BotMessage)
	// and groupNo→channel-name (one REST call per group, cached). Powers the
	// sidebar conversation titles and chat-bubble sender labels.
	names *nameCache

	// policy is the trigger pipeline's configuration — bot uid, grantor,
	// mention-free groups, AI broadcast policy, allowlist, reply-to-bot.
	// Owned ONLY here (the legacy router/connector double-copy is gone —
	// issue #105 缺陷 2). Set at startup via SetPolicy; read-only after
	// (hot-reload would build a fresh Policy and call SetPolicy under
	// c.policyMu).
	policy   trigger.Policy
	policyMu sync.RWMutex
	// classifier is the rule engine. Concrete trigger.DefaultClassifier —
	// the Classifier interface was removed in #116 as speculative
	// generality (one implementation, zero swaps).
	classifier trigger.DefaultClassifier

	// runCtx is the context passed to Run; the sink/inbound callbacks (which the
	// gateway.Sink interface does not thread a context through) tie their work to
	// it, so a cancelled Run aborts in-flight turns and outbound REST calls.
	// Stored as atomic.Pointer because Run writes it once at startup while the
	// receipt worker / heartbeat / callback goroutines read it concurrently —
	// the plain field was a data race under -race.
	runCtx atomic.Pointer[context.Context]

	mu      sync.Mutex
	targets map[string]replyTarget   // sessionKey → where to send the reply
	typers  map[string]*typingTicker // sessionKey → active typing heartbeat
	sock    *socketConn
	closed  bool

	// turnQueues serializes turn dispatch PER session key so the WS read loop is
	// never blocked by a running turn: onInbound hands the turn to a per-key
	// worker goroutine and returns immediately, so the read loop keeps acking
	// frames, answering pings, and observing other channels while a long turn runs.
	// Same-key turns stay strictly FIFO (one worker drains its queue in order);
	// different keys run concurrently. Guarded by mu.
	turnQueues map[string]*turnQueue

	// toolProgress mirrors the agent's tool invocations to the channel as it runs
	// (opt-in; see config AgentConfig.ToolProgress). progress holds the per-turn
	// dedup/cap state, keyed by sessionKey; both are guarded by c.mu.
	toolProgress bool
	progress     map[string]*toolProgressState

	// typingInterval is the heartbeat period between typing pings
	// (TYPING_INTERVAL_MS = 5_000 in cc-channel-octo stream-relay.ts).
	// Overridable in tests for a fast tick.
	typingInterval time.Duration
	// sendTyping sends one typing indicator; defaults to rest.SendTyping but is
	// swappable in tests to count pings without a live IM.
	sendTyping func(ctx context.Context, channelID string, channelType ChannelType) error

	// onStatus, if set, is called when the connection state changes
	// (connected=true after a successful register+handshake; false on drop).
	onStatus func(connected bool, lastErr string)

	// onOwner, if set, is called with the bot owner uid after each (re)register
	// (BotRegisterResp.owner_uid). Used to gate owner-only features (cron).
	onOwner func(ownerUID string)

	// turnsWG tracks every in-flight drainTurns goroutine so the daemon can
	// wait for them before closing the store. Without this barrier, SIGTERM
	// fires `defer st.Close` while a turn is still mid-flush —
	// gateway.Handle's resume-id save / usage-add hit "database is closed",
	// silently breaking resume continuity AND losing accounting.
	turnsWG sync.WaitGroup

	// reconnect/backoff
	reconnectBase time.Duration
	reconnectMax  time.Duration

	// receiptCh buffers read-receipt requests for a single worker goroutine
	// (see receiptWorker). Replaces the prior fan-out where each inbound
	// message spawned its own short-lived goroutine — under a burst of
	// messages that produced N concurrent REST POSTs and N goroutine
	// allocations. Buffered so a slow API back-end can't backpressure the
	// inbound read loop; full buffer drops the receipt (logged) rather than
	// blocking.
	receiptCh chan readReceiptReq
}

// readReceiptReq is a queued ack request handled by receiptWorker.
type readReceiptReq struct {
	channelID   string
	channelType ChannelType
	messageID   string
}

// maxToolNotices caps how many "🔧 Running …" notices a single turn may emit, so
// a tool-heavy run can't flood the channel. Mirrors cc-channel-octo's
// MAX_TOOL_NOTICES (src/index.ts).
const maxToolNotices = 10

// toolProgressState is the per-turn dedup/cap state for tool-progress notices.
type toolProgressState struct {
	lastNotice string // last label sent, to collapse consecutive duplicates
	count      int    // notices sent this turn
}

// awaitTokenPoll is how often Run rechecks for an injected token before it has
// one (see secret.inject). Short enough that the bot connects promptly once the
// GUI injects, without busy-spinning.
const awaitTokenPoll = 2 * time.Second

// defaultTypingInterval is the typing-heartbeat period: re-send the typing
// indicator every 5s while a turn runs so it doesn't expire on a long turn
// (TYPING_INTERVAL_MS = 5_000 in cc-channel-octo stream-relay.ts).
const defaultTypingInterval = 5 * time.Second

// OnStatus registers a connection-state callback (used by the daemon's bot
// supervisor + control-bus). The setter takes c.mu so a late caller can't
// race notifyStatus reading the field. In practice runBot
// wires this before connector.Run, but tests / future callers may not.
func (c *Connector) OnStatus(fn func(connected bool, lastErr string)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onStatus = fn
}

// OnOwner registers a callback invoked with the bot owner uid after each
// (re)registration. The owner uid gates owner-only features (cron create/delete).
// Same lock discipline as OnStatus.
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
	fn := c.onOwner
	c.mu.Unlock()
	if fn != nil && ownerUID != "" {
		fn(ownerUID)
	}
}

type replyTarget struct {
	channelID   string
	channelType ChannelType
	// onBehalfOf, when set, routes the reply (and typing) as the grantor for a
	// persona clone (openclaw OBO). Empty for normal replies.
	onBehalfOf string
}

// SetPolicy installs/replaces the trigger policy. Must be called before
// Run for the IM-side classifier to see correct config. Idempotent (safe
// to call multiple times); future hot-reload paths land here.
//
// Preserves a live server-registered uid: if setUID() already ran (Run
// completed registration), the freshly-installed policy still carries
// the post-Register uid instead of falling back to whatever the caller
// passed for BotUID (typically the stale config id). Without this,
// SetPolicy after setUID — production startup or any future hot-reload
// — would silently re-introduce the @bot-mention regression #116 set
// out to fix.
func (c *Connector) SetPolicy(p trigger.Policy) {
	uid := c.uid()
	c.policyMu.Lock()
	if uid != "" {
		p.BotUID = uid
	}
	c.policy = p
	c.policyMu.Unlock()
}

// loadPolicyAndClassifier snapshots both under the policy lock for one
// inbound classification.
func (c *Connector) loadPolicyAndClassifier() (trigger.Policy, trigger.DefaultClassifier) {
	c.policyMu.RLock()
	defer c.policyMu.RUnlock()
	return c.policy, c.classifier
}

// NewConnector builds a connector. The gateway must be constructed with this
// connector as its Sink (see AsSink note in package docs).
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

// SetToolProgress enables/disables mirroring tool invocations to the channel as
// "🔧 Running <tool>(<params>)…" notices (opt-in; off by default). Wired from
// the bot's resolved AgentConfig.ToolProgress.
func (c *Connector) SetToolProgress(on bool) {
	c.mu.Lock()
	c.toolProgress = on
	c.mu.Unlock()
}
