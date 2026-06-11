package octo

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/lml2468/xclaw/core/agent"
	"github.com/lml2468/xclaw/core/gateway"
	"github.com/lml2468/xclaw/core/router"
)

// Connector wires the Octo IM platform to the gateway: it registers the bot,
// connects the WuKongIM socket, maps inbound BotMessages to
// router.InboundMessage, and (as a gateway.Sink) delivers replies via REST. It
// is the IM-specific edge; everything inside the gateway stays IM-agnostic.
type Connector struct {
	rest    *RESTClient
	gateway *gateway.Gateway

	botUID string

	// runCtx is the context passed to Run; the sink/inbound callbacks (which the
	// gateway.Sink interface does not thread a context through) tie their work to
	// it, so a cancelled Run aborts in-flight turns and outbound REST calls.
	runCtx context.Context

	mu      sync.Mutex
	targets map[string]replyTarget   // sessionKey → where to send the reply
	typers  map[string]*typingTicker // sessionKey → active typing heartbeat
	sock    *socketConn
	closed  bool

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

	// reconnect/backoff
	reconnectBase time.Duration
	reconnectMax  time.Duration
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

func (c *Connector) setStatus(connected bool, lastErr string) {
	if c.onStatus != nil {
		c.onStatus(connected, lastErr)
	}
}

type replyTarget struct {
	channelID   string
	channelType ChannelType
}

// NewConnector builds a connector. The gateway must be constructed with this
// connector as its Sink (see AsSink note in package docs).
func NewConnector(rest *RESTClient) *Connector {
	return &Connector{
		rest:          rest,
		targets:       make(map[string]replyTarget),
		typers:        make(map[string]*typingTicker),
		reconnectBase: 3 * time.Second,
		reconnectMax:  60 * time.Second,
	}
}

// SetGateway attaches the gateway (done after construction to resolve the
// Connector-is-Sink-of-Gateway cycle).
func (c *Connector) SetGateway(g *gateway.Gateway) { c.gateway = g }

// Run registers the bot and maintains the socket connection with reconnect
// until ctx is cancelled. The initial registration is retried with backoff so a
// transient API outage at startup doesn't kill the bot.
func (c *Connector) Run(ctx context.Context) error {
	c.runCtx = ctx
	// REST heartbeat loop (30s), separate from the WS ping.
	go c.heartbeatLoop(ctx)

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
				backoff = minDur(backoff*2, c.reconnectMax)
				continue
			}
			reg = r
			c.setUID(reg.RobotID)
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
		// may have expired) before reconnecting.
		sleep(ctx, backoff)
		backoff = minDur(backoff*2, c.reconnectMax)
		if fresh, rerr := c.rest.Register(ctx, true); rerr == nil {
			reg = fresh
			c.setUID(reg.RobotID)
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
	sock := newSocketConn(reg.WSURL, reg.RobotID, reg.IMToken, c.onInbound, func(error) {})
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
	if c.runCtx != nil {
		return c.runCtx
	}
	return context.Background()
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
// gateway. Drops the bot's own messages and non-text payloads.
func (c *Connector) onInbound(m BotMessage) {
	uid := c.uid()
	if m.FromUID == uid {
		return // ignore our own messages
	}
	if m.Payload.Type != MsgText || strings.TrimSpace(m.Payload.Content) == "" {
		return // MVP handles text only
	}

	chType := router.ChannelDM
	if m.ChannelType == ChannelGroup {
		chType = router.ChannelGroup
	}

	inbound := router.InboundMessage{
		FromUID:     m.FromUID,
		FromName:    m.FromName,
		ChannelID:   m.ChannelID,
		ChannelType: chType,
		Text:        m.Payload.Content,
		Mentioned:   m.MentionsBot(uid),
	}
	key, err := inbound.SessionKey()
	if err != nil {
		return // unroutable
	}
	// Remember where to deliver the reply for this session.
	c.mu.Lock()
	c.targets[key] = replyTarget{channelID: m.ChannelID, channelType: m.ChannelType}
	c.mu.Unlock()

	if c.gateway == nil {
		return
	}
	// A group message that doesn't mention the bot is background context, not a
	// turn: observe it so it becomes a later @-mention's delta. (The router
	// would drop it anyway; observing first preserves group context.)
	if inbound.ChannelType == router.ChannelGroup && !inbound.Mentioned {
		c.gateway.Observe(inbound)
		return
	}
	if _, err := c.gateway.Handle(c.ctx(), inbound); err != nil {
		c.logf("handle turn for %s: %v", key, err)
	}
}

// --- gateway.Sink ---

// OnEvent drives the per-turn typing heartbeat. On the first activity of a turn
// (KindSessionStarted) it sends a typing indicator immediately and starts a 5s
// heartbeat that keeps re-sending it until the turn ends — without this a long
// turn lets the indicator expire and the user thinks the bot died (port of the
// setInterval(TYPING_INTERVAL_MS) loop in cc-channel-octo stream-relay.ts).
// KindTurnDone and a terminal (non-recoverable) KindError stop the heartbeat,
// so a turn that errors out without ever producing a reply still cleans up. A
// recoverable KindError is a mid-turn warning (e.g. a stderr line in
// claude.go) and must NOT stop the heartbeat — the turn is still running.
// (Token / tool events are not mirrored to IM in the MVP.)
func (c *Connector) OnEvent(sessionKey string, ev agent.AgentEvent) {
	switch {
	case ev.Kind == agent.KindSessionStarted:
		c.startTyping(sessionKey)
	case ev.Kind == agent.KindTurnDone, ev.Kind == agent.KindError && !ev.Recoverable:
		c.stopTyping(sessionKey)
	}
}

// OnReply delivers the assembled assistant reply back to the originating
// channel, split into <=3500-char segments (api/stream-relay parity). It also
// stops the typing heartbeat — the normal end-of-turn cleanup point, mirroring
// stream-relay.ts's clearInterval in the deliver() finally block.
func (c *Connector) OnReply(sessionKey string, text string) {
	c.stopTyping(sessionKey)
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	tgt, ok := c.target(sessionKey)
	if !ok {
		return
	}
	for _, seg := range splitMessage(text, 3500) {
		if _, err := c.rest.SendText(c.ctx(), tgt.channelID, tgt.channelType, seg, nil, false); err != nil {
			c.logf("send reply to %s: %v", sessionKey, err)
		}
	}
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

func (c *Connector) target(sessionKey string) (replyTarget, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.targets[sessionKey]
	return t, ok
}

func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// splitMessage breaks text into <=max-rune segments, preferring paragraph,
// newline, then space boundaries before a hard cut (stream-relay parity).
func splitMessage(text string, max int) []string {
	runes := []rune(text)
	if len(runes) <= max {
		return []string{text}
	}
	var out []string
	for len(runes) > max {
		cut := max
		// prefer a boundary within the window
		window := string(runes[:max])
		if i := strings.LastIndex(window, "\n\n"); i > 0 {
			cut = len([]rune(window[:i]))
		} else if i := strings.LastIndex(window, "\n"); i > 0 {
			cut = len([]rune(window[:i]))
		} else if i := strings.LastIndex(window, " "); i > 0 {
			cut = len([]rune(window[:i]))
		}
		if cut <= 0 {
			cut = max
		}
		out = append(out, strings.TrimRight(string(runes[:cut]), " \n"))
		runes = runes[cut:]
		// skip leading whitespace of the next segment
		for len(runes) > 0 && (runes[0] == '\n' || runes[0] == ' ') {
			runes = runes[1:]
		}
	}
	if len(runes) > 0 {
		out = append(out, string(runes))
	}
	return out
}
