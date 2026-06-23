package octo

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lml2468/octobuddy/core/gateway"
	"github.com/lml2468/octobuddy/core/groupctx"
	"github.com/lml2468/octobuddy/core/persona"
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
		names:         newNameCache(rest),
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

// UserName returns the cached display name for uid, or "" if unknown. A miss
// kicks a background REST fetch so the next call can see a resolved value.
// The sender-name cache is also free-seeded from every inbound BotMessage,
// so most uids never trigger a network call.
func (c *Connector) UserName(uid string) string { return c.names.ResolveUser(uid) }

// SetNameResolvedHook registers a callback fired when a background name fetch
// resolves a DM peer (NameKindUser) or group/thread (NameKindChannel) to a new
// non-empty display name. The daemon wires it to re-broadcast session.upserted
// so a sidebar row that first painted with the bare id updates to the resolved
// name without waiting for the next turn (sessions.list's prewarm waits only a
// short budget while the fetch itself runs on a longer deadline). Set during
// bot setup, before Connect.
func (c *Connector) SetNameResolvedHook(fn func(kind NameKind, key, name string)) {
	c.names.SetResolvedHook(fn)
}

// ChannelName returns the cached display name for a channel id, or "" if
// unknown. For a bare group id it's the group's name; for a thread compound
// "<g>____<s>" it's the THREAD's own name (the parent group's name is a
// separate ChannelName call on the parent id). Composing the two for a
// breadcrumb / fallback label is the caller's job — projection layers do
// the composing to keep this cache shape simple and surface-agnostic.
// A miss kicks a background REST fetch.
func (c *Connector) ChannelName(channelID string) string {
	return c.names.ResolveChannel(channelID)
}

// PrewarmChannelNames synchronously fetches names for any of the given channel
// ids that aren't already cached, capped by timeout. Sessions.list calls this
// before building summaries so the first sidebar paint shows group names
// instead of bare ids.
func (c *Connector) PrewarmChannelNames(channelIDs []string, timeout time.Duration) {
	c.names.PrewarmChannels(channelIDs, timeout)
}

// PrewarmUserNames is the DM-peer counterpart of PrewarmChannelNames. DM rows
// usually get their name free-fed from inbound BotMessage.FromName, but a
// session with no inbound this restart (or one whose peer has only ever
// been spoken to, never spoken back) needs an explicit lookup or the sidebar
// row would stick at the bare peer uid.
func (c *Connector) PrewarmUserNames(uids []string, timeout time.Duration) {
	c.names.PrewarmUsers(uids, timeout)
}

// BackfillFetch pulls recent history for cold-start backfill (cc G4), adapting
// octo.HistoricalMessage to the IM-agnostic groupctx.BackfillMessage. limit<=0
// lets the REST client apply its default. Group-like channels only (the gateway
// calls this for group sessions, which includes threads — a thread is routed as
// router.ChannelGroup). Returns nil on any REST failure (the agent runs fine
// without history).
//
// A thread (CommunityTopic / 子区) channel id is the compound
// "<groupNo>____<shortId>", and messages/sync must be queried with
// channel_type=CommunityTopic for it: querying a thread id as a plain Group
// makes the server's membership check fail with not_group_member (the bot is a
// member of the parent group / the topic, never of a "group" by that compound
// id). Bare group ids stay ChannelGroup.
func (c *Connector) BackfillFetch(channelID string, limit int) []groupctx.BackfillMessage {
	chType := ChannelGroup
	if IsThreadChannelID(channelID) {
		chType = ChannelCommunityTopic
	}
	hist := c.rest.GetChannelMessages(c.ctx(), channelID, chType, limit)
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
	// the read loop — previously a no-op, which silently swallowed them. The
	// server DISCONNECT case ends the read loop via run returning, which Run's
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
// invoke methods on the connector outside of a Run call.
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
