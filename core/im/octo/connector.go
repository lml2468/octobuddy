package octo

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/lml2468/xclaw/core/agent"
	"github.com/lml2468/xclaw/core/gateway"
	"github.com/lml2468/xclaw/core/groupctx"
	"github.com/lml2468/xclaw/core/persona"
	"github.com/lml2468/xclaw/core/router"
	"github.com/lml2468/xclaw/core/safety"
)

// Connector wires the Octo IM platform to the gateway: it registers the bot,
// connects the WuKongIM socket, maps inbound BotMessages to
// router.InboundMessage, and (as a gateway.Sink) delivers replies via REST. It
// is the IM-specific edge; everything inside the gateway stays IM-agnostic.
type Connector struct {
	rest    *RESTClient
	gateway *gateway.Gateway

	botUID string

	// persona, when its grantor uid is set, makes this connector a persona clone
	// (openclaw OBO): extended trigger gate, OBO v2 relevance filter, and
	// on_behalf_of reply routing. Set once at startup via SetPersona; read-only
	// thereafter, so it needs no lock.
	persona persona.Grantor

	// mentionFree lists channel ids that respond without an @mention (G12). For
	// those channels an unmentioned group message is routed through the gateway
	// (the router decides) instead of being observed-only as background. Empty by
	// default — normal groups keep the observe-on-no-mention behavior.
	mentionFree map[string]bool

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
	// never blocked by a running turn (H3): onInbound hands the turn to a per-key
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
	// fires `defer st.Close()` while a turn is still mid-flush —
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
// registry to surface per-bot status over the control bus).
func (c *Connector) OnStatus(fn func(connected bool, lastErr string)) { c.onStatus = fn }

// OnOwner registers a callback invoked with the bot owner uid after each
// (re)registration. The owner uid gates owner-only features (cron create/delete).
func (c *Connector) OnOwner(fn func(ownerUID string)) { c.onOwner = fn }

// RegisterReplyTarget is DEPRECATED — direct map writes race onInbound's
// per-turn target. Cron callers must use EnqueueCron instead, which carries
// the target alongside the queued inbound so drainTurns is the sole writer.
// Kept temporarily as a no-op shim with a loud log so a forgotten caller
// surfaces in stderr rather than producing a mis-routed reply.
func (c *Connector) RegisterReplyTarget(sessionKey, channelID string, channelType ChannelType) {
	c.logf("DEPRECATED: RegisterReplyTarget(%s) is a no-op since round-8 F1-Arch; use EnqueueCron — call ignored", sessionKey)
}

// EnqueueCron enqueues a cron-fired turn onto the per-session worker so it
// serializes with real inbound on the same key (round 8 F1-Arch). The
// target — including any persona on-behalf-of binding — travels with the
// queued turn so OnReply reads exactly the target the cron fire intended,
// even if a real inbound enqueued in between and tried to write its own
// target into the global map.
//
// Returns immediately. The actual gw.Handle call happens on the worker
// goroutine that drainTurns owns; the bot's shutdown chain
// (connector.WaitTurns + cm.Wait) ensures the in-flight turn finishes
// before the store closes.
func (c *Connector) EnqueueCron(sessionKey, channelID string, channelType ChannelType, inbound router.InboundMessage) {
	tgt := replyTarget{channelID: channelID, channelType: channelType}
	// Persona-OBO bots speak as the grantor; without this the cron reply
	// arrives from the bot identity while every other reply in the channel
	// arrives from the grantor — the persona illusion breaks visibly.
	if c.persona.UID != "" {
		tgt.onBehalfOf = c.persona.UID
	}
	c.enqueueTurn(sessionKey, inbound, tgt)
}

func (c *Connector) setStatus(connected bool, lastErr string) {
	if c.onStatus != nil {
		c.onStatus(connected, lastErr)
	}
}

func (c *Connector) notifyOwner(ownerUID string) {
	if c.onOwner != nil && ownerUID != "" {
		c.onOwner(ownerUID)
	}
}

type replyTarget struct {
	channelID   string
	channelType ChannelType
	// onBehalfOf, when set, routes the reply (and typing) as the grantor for a
	// persona clone (openclaw OBO). Empty for normal replies.
	onBehalfOf string
}

// SetPersona configures this connector as a persona clone of grantor (openclaw
// OBO). When set, the connector (a) extends the group trigger gate so an
// @grantor / @所有人 mention triggers a turn, (b) applies the OBO v2 relevance
// filter, and (c) routes the reply back to the origin channel with on_behalf_of.
// A zero Grantor (no uid) leaves the connector a regular bot.
func (c *Connector) SetPersona(grantor persona.Grantor) { c.persona = grantor }

// NewConnector builds a connector. The gateway must be constructed with this
// connector as its Sink (see AsSink note in package docs).
func NewConnector(rest *RESTClient) *Connector {
	return &Connector{
		rest:          rest,
		targets:       make(map[string]replyTarget),
		progress:      make(map[string]*toolProgressState),
		typers:        make(map[string]*typingTicker),
		turnQueues:    make(map[string]*turnQueue),
		reconnectBase: 3 * time.Second,
		reconnectMax:  60 * time.Second,
		receiptCh:     make(chan readReceiptReq, 64),
	}
}

// turnQueue is the per-session-key serial dispatch state (guarded by Connector.mu).
// pending holds turns awaiting execution in arrival order; running marks whether
// a worker goroutine is draining them. See enqueueTurn/drainTurns (H3).
//
// Each pending entry carries its OWN reply target (round 8 F1-Arch): the
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

// SetToolProgress enables/disables mirroring tool invocations to the channel as
// "🔧 Running <tool>(<params>)…" notices (opt-in; off by default). Wired from
// the bot's resolved AgentConfig.ToolProgress.
func (c *Connector) SetToolProgress(on bool) {
	c.mu.Lock()
	c.toolProgress = on
	c.mu.Unlock()
}

// SetGateway attaches the gateway (done after construction to resolve the
// Connector-is-Sink-of-Gateway cycle).
func (c *Connector) SetGateway(g *gateway.Gateway) { c.gateway = g }

// MediaAuth returns the gateway hook that host-scopes the bot token on inbound
// media downloads: the Bearer token is sent only while the current hop is
// same-host as apiUrl, so a redirect to another host drops the credential
// (inbound.ts S1 per-hop Authorization scoping). Wire it via
// gateway.WithMediaAuth so the gateway can authenticate same-host media without
// embedding an IM-specific token.
func (c *Connector) MediaAuth() gateway.MediaAuth {
	return func(rawURL string) string {
		if !isSameHost(rawURL, c.rest.APIURL()) {
			return ""
		}
		tok := c.rest.Token()
		if tok == "" {
			return ""
		}
		return "Bearer " + tok
	}
}

// BotUID returns the bot's registered uid (empty before registration). Passed to
// gateway.WithGroupBackfill so cold-start backfill can filter the bot's own
// messages once the uid is known.
func (c *Connector) BotUID() string { return c.uid() }

// BackfillFetch pulls recent group-channel history for cold-start backfill (cc
// G4), adapting octo.HistoricalMessage to the IM-agnostic groupctx.BackfillMessage.
// limit<=0 lets the REST client apply its default. Group channels only — the
// gateway only calls this for group sessions. Returns nil on any REST failure
// (the agent runs fine without history).
func (c *Connector) BackfillFetch(channelID string, limit int) []groupctx.BackfillMessage {
	hist := c.rest.GetChannelMessages(c.ctx(), channelID, ChannelGroup, limit)
	if len(hist) == 0 {
		return nil
	}
	out := make([]groupctx.BackfillMessage, 0, len(hist))
	for _, h := range hist {
		out = append(out, groupctx.BackfillMessage{
			FromUID:  h.FromUID,
			FromName: h.FromName,
			Content:  h.Content,
			Seq:      h.MessageSeq,
		})
	}
	return out
}

// SetMentionFreeGroups records the channel ids that respond without an @mention
// (G12). In those channels an unmentioned group message is handed to the gateway
// (the router applies the mention-free + bot-loop policy) rather than being
// observed-only. Must be called before Run.
func (c *Connector) SetMentionFreeGroups(channelIDs []string) {
	if len(channelIDs) == 0 {
		c.mentionFree = nil
		return
	}
	m := make(map[string]bool, len(channelIDs))
	for _, id := range channelIDs {
		if id != "" {
			m[id] = true
		}
	}
	c.mentionFree = m
}

// Run registers the bot and maintains the socket connection with reconnect
// until ctx is cancelled. The initial registration is retried with backoff so a
// transient API outage at startup doesn't kill the bot.
func (c *Connector) Run(ctx context.Context) error {
	c.setCtx(ctx)
	// REST heartbeat loop (30s), separate from the WS ping.
	go c.heartbeatLoop(ctx)
	// Single-worker read-receipt sender (see receiptCh comment).
	go c.receiptWorker(ctx)

	backoff := c.reconnectBase
	var reg RegisterResponse
	registered := false

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// No token yet (config has none and secret.inject hasn't arrived): wait
		// for one rather than hammering Register with an empty bearer. The GUI
		// injects tokens shortly after the control bus connects.
		if c.rest.Token() == "" {
			c.setStatus(false, "awaiting secret")
			sleep(ctx, awaitTokenPoll)
			continue
		}

		if !registered {
			r, err := c.rest.Register(ctx, false)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				c.setStatus(false, err.Error())
				sleep(ctx, backoff)
				backoff = min(backoff*2, c.reconnectMax)
				continue
			}
			reg = r
			c.setUID(reg.RobotID)
			c.notifyOwner(reg.OwnerUID)
			registered = true
			backoff = c.reconnectBase
		}

		c.setStatus(true, "")
		err := c.connectOnce(ctx, reg)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		errStr := ""
		if err != nil {
			errStr = err.Error()
		}
		c.setStatus(false, errStr)

		// Connection dropped: back off, then force a fresh registration (token
		// may have expired) before reconnecting. Re-check ctx after the sleep so
		// a shutdown that races the back-off doesn't issue a wasted Register.
		sleep(ctx, backoff)
		backoff = min(backoff*2, c.reconnectMax)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if fresh, rerr := c.rest.Register(ctx, true); rerr == nil {
			reg = fresh
			c.setUID(reg.RobotID)
			c.notifyOwner(reg.OwnerUID)
		} else {
			registered = false // force the retry path above
		}
	}
}

// sleep waits for d or until ctx is cancelled.
func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func (c *Connector) connectOnce(ctx context.Context, reg RegisterResponse) error {
	// onError logs socket-level events (poison-drop, kicks) that are not fatal to
	// the read loop — previously a no-op, which silently swallowed them (H4). The
	// server DISCONNECT case ends the read loop via run() returning, which Run's
	// reconnect path handles; this hook is for the informational drops.
	sock := newSocketConn(reg.WSURL, reg.RobotID, reg.IMToken, c.onInbound, func(err error) {
		c.logf("socket: %v", err)
	})
	c.mu.Lock()
	c.sock = sock
	c.mu.Unlock()
	// Always release the socket (fd + ping/watch goroutines) when this attempt
	// ends, so reconnects don't accumulate connections.
	defer sock.close()

	if err := sock.connect(ctx); err != nil {
		return err
	}
	return sock.run(ctx)
}

// ctx returns the Run context, falling back to Background if a callback somehow
// fires before Run set it (defensive — a nil context would panic downstream).
func (c *Connector) ctx() context.Context {
	if p := c.runCtx.Load(); p != nil {
		return *p
	}
	return context.Background()
}

// setCtx stores ctx as the runCtx. Used by Run at startup and by tests that
// invoke methods on the connector outside of a Run() call.
func (c *Connector) setCtx(ctx context.Context) {
	c.runCtx.Store(&ctx)
}

// setUID / uid guard botUID with c.mu: Run rewrites it on (re)registration while
// the sink callbacks (OnReply/OnEvent → logf) and a concurrent turn read it.
func (c *Connector) setUID(id string) {
	c.mu.Lock()
	c.botUID = id
	c.mu.Unlock()
}

func (c *Connector) uid() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.botUID
}

// logf reports a recovered/handled error to stderr, tagged with the bot uid.
func (c *Connector) logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[octo %s] "+format+"\n", append([]any{c.uid()}, args...)...)
}

func (c *Connector) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = c.rest.Heartbeat(ctx)
		}
	}
}

// onInbound maps a decoded BotMessage to a router.InboundMessage and feeds the
// gateway. Drops the bot's own messages and streaming partials; every other
// payload type is rendered to LLM-facing text by ResolveContent (content.go),
// and image/file payloads also surface media Attachments the gateway
// materializes into the session cwd (inbound.ts G1).
//
// Persona-clone path (openclaw OBO, inbound.ts): when this connector is a
// persona clone, the group trigger gate is widened (an @grantor / @所有人
// mention triggers a turn), the OBO v2 relevance filter drops irrelevant @AI
// fan-out BEFORE any session state is recorded, and the reply target carries
// on_behalf_of so the server presents the reply as the grantor.
func (c *Connector) onInbound(m BotMessage) {
	uid := c.uid()
	if m.FromUID == uid {
		return // ignore our own messages
	}
	// Suppress streaming partial updates (inbound.ts settingStreamOn / G21): a
	// streamOn message is an in-progress edit; only the final (streamOn=false)
	// message carries the settled content. Routing partials would feed the agent
	// half-typed text and re-fire turns on every keystroke.
	if m.StreamOn {
		return
	}

	// Render the payload to LLM-facing text. ResolveContent covers every type
	// (text, media markers, location, card, RichText, MultipleForward) and
	// sanitizes any untrusted name/body that lands in a label.
	resolved := ResolveContent(m.Payload, c.rest.APIURL())
	baseText := strings.TrimSpace(resolved.Text)
	if baseText == "" {
		return // nothing renderable (e.g. an empty/unknown payload)
	}

	// OBO v2 detection (openclaw inbound.ts ~L2102-2116). The grantor relays a
	// fan-out message carrying an origin-channel hint so the clone replies in the
	// origin group. SECURITY: only trust OBO fields when the message is sent by
	// the configured grantor — otherwise any user could forge a reply-as-someone.
	oboV2 := c.persona.Configured() &&
		m.Payload.OBOOriginChannelID != "" &&
		oboRespondAs(m.Payload) != "" &&
		m.FromUID == c.persona.UID

	// OBO v2 relevance filter (openclaw inbound.ts ~L2122-2160): drop pure @AI
	// fan-out that does not address the grantor BEFORE recording any reply target
	// or session state, so an irrelevant message never leaks into the clone's
	// session.
	if oboV2 && !c.persona.Relevant(m.PersonaMention()) {
		c.logf("OBO v2 skipped — message not relevant to persona")
		return
	}

	// A CommunityTopic (thread / 子区) is group-like for routing: its channel id
	// is the compound "<groupNo>____<shortId>", so it lands in its OWN session
	// (distinct from the parent group and sibling threads) while membership and
	// the mention gate are inherited from the parent group. See thread.go and
	// openclaw inbound.ts thread routing.
	chType := router.ChannelDM
	if m.ChannelType == ChannelGroup || m.ChannelType == ChannelCommunityTopic {
		chType = router.ChannelGroup
	}

	// Trigger gate: persona-aware for clones (an @grantor / @所有人 mention is a
	// call to the clone); a plain @bot / @AI mention otherwise.
	triggered := m.Triggers(uid, c.persona)

	inbound := router.InboundMessage{
		FromUID:     m.FromUID,
		FromName:    m.FromName,
		ChannelID:   m.ChannelID,
		ChannelType: chType,
		Text:        baseText,
		Attachments: c.resolveAttachments(m.Payload),
		MessageSeq:  int64(m.MessageSeq),
		Mentioned:   triggered,
	}
	key, err := inbound.SessionKey()
	if err != nil {
		return // unroutable
	}

	// Resolve where (and as whom) the reply goes (openclaw inbound.ts
	// ~L2301-2337). OBO v2: reply to the origin channel as the grantor. Group
	// persona trigger-as-grantor: reply in the same group as the grantor.
	tgt := replyTarget{channelID: m.ChannelID, channelType: m.ChannelType}
	if oboV2 {
		tgt = oboReplyTarget(m.Payload, c.persona.UID)
	} else if chType == router.ChannelGroup &&
		c.persona.TriggeredAsGrantor(m.PersonaMention(), m.ExplicitlyMentionsBot(uid)) {
		tgt.onBehalfOf = c.persona.UID
	}
	// Per-turn target travels with the queued turn so drainTurns can set
	// c.targets[key] AT pop-time — the prior contract had onInbound write the
	// global map directly here, which raced cron's RegisterReplyTarget (round 8
	// F1-Arch). issue #98 reroute is computed once here so it isn't recomputed
	// on every target() read.
	if tgt.channelType != ChannelDM {
		if rerouted, did := RerouteTarget(key, tgt.channelID); did {
			c.logf("reroute reply for thread session %s: target %q -> %q (issue #98)", key, tgt.channelID, rerouted)
			tgt.channelID = rerouted
			tgt.channelType = ChannelCommunityTopic
		}
	}
	// Also record the target now so callers probing c.target(key) immediately
	// after onInbound (notably the persona tests) observe the right value.
	// drainTurns re-asserts this from the queued item right before gw.Handle,
	// so a concurrent EnqueueCron that wrote a different target between this
	// line and the pop doesn't actually corrupt either turn — the queue item
	// carries the authoritative target for THIS turn.
	c.mu.Lock()
	c.targets[key] = tgt
	c.mu.Unlock()

	// Acknowledge receipt (fire-and-forget) once we've decided to process it.
	c.sendReadReceipt(m)

	if c.gateway == nil {
		return
	}
	// A group message that doesn't trigger the bot is background context, not a
	// turn: observe it so it becomes a later @-mention's delta. (The router
	// would drop it anyway; observing first preserves group context.) Background
	// context is stored history, so it carries the plain resolved text WITHOUT
	// the quoted-reply prefix. Observe is a fast in-memory cache write, so it runs
	// inline (not worth a worker goroutine).
	//
	// Exception (G12): in a mention-free channel an unmentioned message IS a turn
	// — hand it to the gateway so the router applies the mention-free + bot-loop
	// policy. runTurn caches it into group context itself, so do NOT also Observe.
	if inbound.ChannelType == router.ChannelGroup && !inbound.Mentioned && !c.mentionFree[m.ChannelID] {
		c.gateway.Observe(inbound)
		return
	}
	// Prepend the quoted-reply context to the CURRENT turn only (never stored
	// history): the sender quoted a prior message, so give the agent that
	// context fenced ahead of the real request (inbound.ts quotePrefix).
	if prefix := resolveQuotePrefix(m.Payload.Reply, c.rest.APIURL()); prefix != "" {
		inbound.Text = prefix + inbound.Text
	}
	// Dispatch the turn on the per-key worker so the WS read loop is not blocked
	// for the whole (possibly multi-minute) turn (H3). The router still serializes
	// same-session turns; the per-key queue guarantees they reach the router in
	// arrival order despite running on a goroutine.
	c.enqueueTurn(key, inbound, tgt)
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
	c.mu.Unlock()

	if start {
		c.turnsWG.Add(1)
		go c.drainTurns(key)
	}
}

// drainTurns runs queued turns for one session key in order, then retires the
// queue. New arrivals during a turn are picked up before the worker exits, so a
// burst is handled by a single worker with no lost messages.
// WaitTurns blocks until every drainTurns goroutine spawned by this
// connector has finished its queue. Call this on graceful shutdown AFTER
// the Run-ctx is cancelled (which closes the WS read loop and stops new
// turns from being enqueued) and BEFORE closing the store / gateway / driver.
//
// Idempotent: a connector that has never enqueued a turn returns immediately.
func (c *Connector) WaitTurns() { c.turnsWG.Wait() }

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
		q.pending = q.pending[1:]
		// Set the per-turn target IMMEDIATELY before releasing the lock and
		// running gw.Handle, so OnReply (which reads via c.target(key))
		// observes exactly the target the producer attached. drainTurns is
		// the sole writer to c.targets, so cron+inbound concurrent enqueues
		// no longer race (round 8 F1-Arch).
		c.targets[key] = item.tgt
		c.mu.Unlock()

		// Tests may enqueue without setting a gateway; skip dispatch in
		// that case so the queue still drains cleanly.
		if c.gateway == nil {
			continue
		}
		dec, err := c.gateway.Handle(c.ctx(), item.inbound)
		if err != nil {
			c.logf("handle turn for %s: %v", key, err)
		}
		// A mention-free unmentioned message the router declined to run (bot-loop
		// guard, or it turned out not to be mention-free after all) is still group
		// chatter the agent should see later — observe it as background. runTurn
		// already cached it on the Accepted path, so only observe on these drops.
		if item.inbound.ChannelType == router.ChannelGroup && !item.inbound.Mentioned &&
			(dec == router.DroppedBot || dec == router.DroppedNotMentioned) {
			c.gateway.Observe(item.inbound)
		}
	}
}

// oboRespondAs resolves the grantor uid the payload claims to respond as,
// preferring obo_respond_as over obo_grantor_uid (openclaw inbound.ts L2104).
func oboRespondAs(p MessagePayload) string {
	if p.OBORespondAs != "" {
		return p.OBORespondAs
	}
	return p.OBOGrantorUID
}

// oboReplyTarget derives the OBO v2 reply destination from a (grantor-trusted)
// payload (openclaw inbound.ts ~L2305-2326). DM-relay origin → reply to the
// original sender's uid; group/thread → reply to the origin group. The reply
// always carries on_behalf_of=grantor (the trusted configured grantor uid, NOT
// the payload's respond_as).
func oboReplyTarget(p MessagePayload, grantorUID string) replyTarget {
	chType := ChannelGroup
	if p.OBOOriginChannelType != nil {
		chType = ChannelType(*p.OBOOriginChannelType)
	}
	channelID := p.OBOOriginChannelID
	if chType == ChannelDM && p.OBOOriginFromUID != "" {
		// DM: the bot is only friends with the grantor; reply to the original
		// sender via on_behalf_of=grantor, which bypasses the bot-friend gate.
		channelID = p.OBOOriginFromUID
	}
	return replyTarget{channelID: channelID, channelType: chType, onBehalfOf: grantorUID}
}

// sendReadReceipt enqueues an ack for the bounded receipt worker (api.ts
// sendReadReceipt). Failures are logged but never block the turn; if the
// buffer is saturated (REST back-end is slow and a burst of messages is
// arriving) the receipt is dropped — read-receipts are best-effort.
func (c *Connector) sendReadReceipt(m BotMessage) {
	if m.MessageID == "" {
		return
	}
	select {
	case c.receiptCh <- readReceiptReq{m.ChannelID, m.ChannelType, m.MessageID}:
	default:
		c.logf("read receipt for %s dropped: queue full", m.MessageID)
	}
}

// receiptWorker drains receiptCh serially, ending when ctx is cancelled.
// One in-flight POST at a time bounds load on the REST API.
func (c *Connector) receiptWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-c.receiptCh:
			if err := c.rest.SendReadReceipt(ctx, r.channelID, r.channelType, []string{r.messageID}); err != nil {
				c.logf("read receipt for %s: %v", r.messageID, err)
			}
		}
	}
}

// resolveAttachments extracts downloadable media/file attachments from a payload
// (image/GIF/file). LLM-facing text rendering is handled by ResolveContent
// (content.go); this only surfaces the URLs the gateway materializes into the
// session cwd. Media URLs are host-validated via buildMediaURL (inbound.ts G1).
func (c *Connector) resolveAttachments(p MessagePayload) []router.Attachment {
	apiURL := c.rest.APIURL()
	switch p.Type {
	case MsgImage, MsgGIF:
		full := buildMediaURL(p.URL, apiURL)
		if full == "" {
			return nil
		}
		return []router.Attachment{{Kind: router.AttachmentImage, URL: full}}
	case MsgFile:
		// SECURITY: p.Name is user-controlled; sanitize before it flows into the
		// <file_content name="…"> attribute the gateway writes.
		filename := safety.SanitizeDisplayName(p.Name, "未知文件")
		full := buildMediaURL(p.URL, apiURL)
		if full == "" {
			return nil
		}
		return []router.Attachment{{Kind: router.AttachmentFile, URL: full, Name: filename, Size: p.Size}}
	default:
		return nil // Voice/Video/Location/etc. carry no downloadable attachment
	}
}

// --- gateway.Sink ---

// OnEvent drives the per-turn typing heartbeat and the optional tool-progress
// mirror. On the first activity (KindSessionStarted) it resets the per-turn
// tool-progress state and starts a 5s typing heartbeat (cc-channel-octo
// stream-relay.ts) — without this a long turn lets the indicator expire and the
// user thinks the bot died. KindToolUse mirrors a "🔧 Running <tool>(<params>)…"
// notice when tool-progress is on. KindTurnDone and a terminal (non-recoverable)
// KindError stop the heartbeat and clear the progress state, so a turn that
// errors out without a reply still cleans up. A recoverable KindError is a
// mid-turn warning (e.g. a stderr line in claude.go) and must NOT stop it.
func (c *Connector) OnEvent(sessionKey string, ev agent.AgentEvent) {
	switch {
	case ev.Kind == agent.KindSessionStarted:
		c.mu.Lock()
		if c.toolProgress {
			c.progress[sessionKey] = &toolProgressState{}
		}
		c.mu.Unlock()
		c.startTyping(sessionKey)
	case ev.Kind == agent.KindToolUse:
		c.maybeSendToolNotice(sessionKey, ev)
	case ev.Kind == agent.KindTurnDone, ev.Kind == agent.KindError && !ev.Recoverable:
		c.stopTyping(sessionKey)
		c.mu.Lock()
		delete(c.progress, sessionKey)
		c.mu.Unlock()
	}
}

// maybeSendToolNotice emits a "🔧 Running <tool>(<params>)…" notice for a
// KindToolUse event when tool-progress is on, collapsing consecutive identical
// notices and capping the count per turn. The dedup/cap decision is made under
// c.mu; the REST send happens after unlocking so a slow send never holds the
// connector lock (and never blocks the agent stream's other sessions).
func (c *Connector) maybeSendToolNotice(sessionKey string, ev agent.AgentEvent) {
	label := ev.ToolName
	if label == "" {
		return
	}
	if ev.ToolParams != "" {
		// ToolParams is already a whitespace-collapsed one-liner truncated to 120
		// chars by claude.go's truncateParams — mirrors MAX_TOOL_PARAM_CHARS.
		label += "(" + ev.ToolParams + ")"
	}

	c.mu.Lock()
	if !c.toolProgress {
		c.mu.Unlock()
		return
	}
	st := c.progress[sessionKey]
	if st == nil {
		// No KindSessionStarted seen for this session this turn — start fresh.
		st = &toolProgressState{}
		c.progress[sessionKey] = st
	}
	if label == st.lastNotice {
		c.mu.Unlock()
		return // collapse exact consecutive repeats
	}
	st.lastNotice = label
	if st.count >= maxToolNotices {
		c.mu.Unlock()
		return // capped — stay quiet for the rest of the turn
	}
	st.count++
	c.mu.Unlock()

	tgt, ok := c.target(sessionKey)
	if !ok {
		return
	}
	if _, err := c.rest.SendText(c.ctx(), tgt.channelID, tgt.channelType, "🔧 Running "+label+"…", nil, nil, false); err != nil {
		c.logf("send tool-progress for %s: %v", sessionKey, err)
	}
}

// OnReply delivers the assembled assistant reply back to the originating
// channel. It resolves @mentions (structured @[uid:name] + plain @name +
// @all/@所有人) ONCE over the full text — so splitting can never break a mention
// across segments — then splits into <=3500-UTF-16-unit segments, rebasing each
// entity's offset to segment-local before sending (api/stream-relay parity). For
// a persona clone replying as the grantor, each send carries on_behalf_of so the
// server presents it as the grantor (openclaw OBO). It also stops the typing
// heartbeat — the end-of-turn cleanup point (stream-relay.ts deliver() finally).
//
// Empty reply → a no-response placeholder is sent instead of silently dropping
// the turn (cc-channel-octo index.ts behavior).
func (c *Connector) OnReply(sessionKey string, text string) {
	c.stopTyping(sessionKey)
	// The reply target is only needed through this turn's delivery; drop it
	// afterwards so the map doesn't accumulate one entry per distinct session
	// forever. The next inbound (or cron fire) re-registers it, and the router
	// serializes turns per session so nothing races this delete.
	defer func() { c.mu.Lock(); delete(c.targets, sessionKey); c.mu.Unlock() }()
	text = strings.TrimSpace(text)
	tgt, ok := c.target(sessionKey)
	if !ok {
		return
	}
	if text == "" {
		// No output from the agent: deliver a placeholder so the user isn't left
		// hanging. No mentions on a fixed system string.
		if _, err := c.rest.SendTextAs(c.ctx(), tgt.channelID, tgt.channelType, noResponseFallback, nil, nil, false, tgt.onBehalfOf); err != nil {
			c.logf("send no-response fallback to %s: %v", sessionKey, err)
		}
		return
	}

	// Resolve mentions against the channel roster. Plain @name resolution and the
	// member-validity downgrade only apply to group channels (DMs have no
	// roster); for DMs memberMap is nil and structured uids are trusted
	// (isValidUid=nil), matching cc-channel-octo's "omit memberMap/isValidUid in
	// DMs" path.
	var memberMap map[string]string
	var isValidUid func(string) bool
	if tgt.channelType == ChannelGroup && c.gateway != nil {
		memberMap = c.gateway.MemberMap(tgt.channelID)
		channelID := tgt.channelID
		isValidUid = func(uid string) bool { return c.gateway.IsMember(channelID, uid) }
	}
	res := resolveMentions(text, memberMap, isValidUid)

	// Protect each resolved @name span so splitMessageProtected won't cut through it.
	ranges := make([]protectedRange, 0, len(res.mentionEntries))
	for _, e := range res.mentionEntries {
		ranges = append(ranges, protectedRange{start: e.Offset, end: e.Offset + e.Length})
	}

	mentionAllConsumed := false
	for _, seg := range splitMessageProtected(res.finalContent, 3500, ranges) {
		segStart := seg.start
		segEnd := segStart + utf16Len(seg.text)
		// Entities fully inside this segment, rebased to segment-local offsets.
		var segEntities []MentionEntity
		var segUids []string
		for _, e := range res.mentionEntries {
			if e.Offset >= segStart && e.Offset+e.Length <= segEnd {
				segEntities = append(segEntities, MentionEntity{UID: e.UID, Offset: e.Offset - segStart, Length: e.Length})
				segUids = append(segUids, e.UID)
			}
		}
		// mentionAll applies to the FIRST segment only (stream-relay parity:
		// avoids re-broadcasting @所有人 on every segment of a long reply).
		useMentionAll := res.mentionAll && !mentionAllConsumed
		if useMentionAll {
			mentionAllConsumed = true
		}
		// SendTextAs carries on_behalf_of for a persona clone (empty otherwise).
		// A failed segment send means the user never receives that part of the
		// reply, so retry once on transient errors before giving up (M7) — the turn
		// is already "done", there's no other recovery path. A final failure is
		// logged distinctly as a DROPPED segment so it's greppable in ops.
		if err := c.sendReplySegment(tgt, seg.text, segUids, segEntities, useMentionAll); err != nil {
			c.logf("DROPPED reply segment for %s (user will not see it): %v", sessionKey, err)
		}
	}
}

// sendReplySegment sends one reply segment with a single bounded retry. The reply
// is the turn's only user-visible output, so a transient send failure (network
// blip) shouldn't silently lose it; one retry covers the common case. The
// client_msg_no is generated ONCE up-front and reused on retry so server-side
// dedup (keyed on client_msg_no) actually suppresses duplicate delivery — a
// fresh uuid per attempt defeated the dedup whenever a 5xx/timeout/TCP-reset
// happened AFTER the server committed but BEFORE the response reached us.
func (c *Connector) sendReplySegment(tgt replyTarget, text string, uids []string, entities []MentionEntity, mentionAll bool) error {
	msgNo := uuid.NewString()
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			sleep(c.ctx(), 500*time.Millisecond)
			if c.ctx().Err() != nil {
				return lastErr // shutting down — don't keep retrying
			}
		}
		if _, err := c.rest.SendTextAsWithMsgNo(c.ctx(), tgt.channelID, tgt.channelType, text, uids, entities, mentionAll, tgt.onBehalfOf, msgNo); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return lastErr
}

// typingTicker holds the cancel hook and the done channel of one session's
// typing-heartbeat goroutine. stop() cancels and waits for the goroutine to
// exit so there is never a leaked goroutine after a turn.
type typingTicker struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// startTyping begins (or re-arms) the typing heartbeat for a session. It is
// idempotent: a second KindSessionStarted for an already-typing session is a
// no-op so we never spawn two tickers for one turn. It fires one typing ping
// immediately, then a goroutine re-sends every typingInterval until the turn's
// stopTyping runs or the run context is cancelled.
func (c *Connector) startTyping(sessionKey string) {
	tgt, ok := c.target(sessionKey)
	if !ok {
		return
	}

	interval := c.typingInterval
	if interval <= 0 {
		interval = defaultTypingInterval
	}
	send := c.sendTyping
	if send == nil {
		send = c.rest.SendTyping
	}

	// Tie the heartbeat to the run context so a cancelled Run stops every ticker.
	ctx, cancel := context.WithCancel(c.ctx())

	c.mu.Lock()
	if _, exists := c.typers[sessionKey]; exists {
		c.mu.Unlock()
		cancel() // already ticking — drop the spare context
		return
	}
	tt := &typingTicker{cancel: cancel, done: make(chan struct{})}
	c.typers[sessionKey] = tt
	c.mu.Unlock()

	// Fire one immediately — don't wait for the first tick (stream-relay.ts:173).
	if err := send(ctx, tgt.channelID, tgt.channelType); err != nil && ctx.Err() == nil {
		c.logf("send typing for %s: %v", sessionKey, err)
	}

	go func() {
		defer close(tt.done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := send(ctx, tgt.channelID, tgt.channelType); err != nil && ctx.Err() == nil {
					c.logf("send typing for %s: %v", sessionKey, err)
				}
			}
		}
	}()
}

// stopTyping ends a session's typing heartbeat and waits for its goroutine to
// exit. Safe to call when no ticker is active (no-op) and idempotent across the
// several turn-end paths (OnReply, KindTurnDone, KindError).
func (c *Connector) stopTyping(sessionKey string) {
	c.mu.Lock()
	tt := c.typers[sessionKey]
	delete(c.typers, sessionKey)
	c.mu.Unlock()
	if tt == nil {
		return
	}
	tt.cancel()
	<-tt.done
}

// noResponseFallback is sent when the agent produced no text (cc-channel-octo
// index.ts) so the user gets a reply rather than silence.
const noResponseFallback = "[No response generated. Please try rephrasing your question.]"

// target returns the stored reply target for a session key. It is a pure read:
// the issue-#98 thread reroute is applied ONCE when the target is registered (see
// onInbound), so calling this repeatedly per turn (tool-progress, typing, reply)
// no longer recomputes the reroute or re-logs it (L20).
func (c *Connector) target(sessionKey string) (replyTarget, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.targets[sessionKey]
	return t, ok
}
