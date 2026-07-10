package octo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/lml2468/octobuddy/core/clog"
)

// Run registers the bot and maintains the socket connection with reconnect
// until ctx is cancelled. The initial registration is retried with backoff so a
// transient API outage at startup doesn't kill the bot.
func (c *Connector) Run(ctx context.Context) error {
	c.setCtx(ctx)
	c.startRunWorkers(ctx)

	backoff := c.reconnectBase
	var reg RegisterResponse
	registered := false

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if !c.waitForToken(ctx) {
			continue
		}

		if !registered {
			r, ok, err := c.registerInitial(ctx, backoff)
			if err != nil {
				return err
			}
			if !ok {
				backoff = min(backoff*2, c.reconnectMax)
				continue
			}
			reg = r
			registered = true
			backoff = c.reconnectBase
		}

		c.setStatus(true, "")
		stable, err := c.connectOnce(ctx, reg)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Log the cause so a recurring drop is diagnosable from the daemon log
		// — the status callback only surfaces it transiently in the UI. The
		// decoded reason distinguishes a duplicate-login kick (DISCONNECT
		// reason 11) from an auth failure (connack reason 2) from a plain
		// transport drop (EOF / reset / idle).
		if err != nil {
			c.logf("connection lost, reconnecting: %v", err)
		}
		c.setStatus(false, errString(err))

		// A drop after a healthy (stable) session is a fresh incident, not part
		// of a connect-fail storm, so reconnect promptly rather than inherit an
		// escalated (up to reconnectMax) backoff. Without this, now that
		// reconnect no longer re-registers, a long-lived bot that blips once
		// would stay pinned near the 60s ceiling.
		if stable {
			backoff = c.reconnectBase
		}

		sleep(ctx, backoff)
		backoff = min(backoff*2, c.reconnectMax)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Reuse the cached registration on reconnect. Re-registering every time
		// is what caused the recurring "server sent disconnect": the server's
		// register handler unconditionally calls UpdateIMToken, which kicks the
		// bot's existing (uid, deviceFlag) session for DeviceLevel=Master — so a
		// reconnect-then-register would kick the very session we just rebuilt.
		// Only an auth failure (the server's token no longer matches ours)
		// genuinely needs a fresh register; everything else just reconnects.
		if errors.Is(err, errConnackAuthFail) {
			registered = false // force the registerInitial path at the loop top
		}
	}
}

func (c *Connector) waitForToken(ctx context.Context) bool {
	// No token yet: wait rather than hammering Register with an empty bearer.
	if c.rest.Token() != "" {
		return true
	}
	c.setStatus(false, "awaiting secret")
	sleep(ctx, awaitTokenPoll)
	return false
}

func (c *Connector) registerInitial(ctx context.Context, backoff time.Duration) (RegisterResponse, bool, error) {
	reg, err := c.rest.Register(ctx, false)
	if err == nil {
		c.applyRegistration(reg)
		return reg, true, nil
	}
	if ctx.Err() != nil {
		return RegisterResponse{}, false, ctx.Err()
	}
	c.setStatus(false, err.Error())
	sleep(ctx, backoff)
	return RegisterResponse{}, false, nil
}

func (c *Connector) startRunWorkers(ctx context.Context) {
	// REST heartbeat loop (30s), separate from the WS ping.
	go c.heartbeatLoop(ctx)
	// Single-worker read-receipt sender (see receiptCh comment).
	go c.receiptWorker(ctx)
}

func (c *Connector) applyRegistration(reg RegisterResponse) {
	c.setUID(reg.RobotID)
	c.notifyOwner(reg.OwnerUID)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
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

// connectOnce dials, handshakes, and runs the read loop until the connection
// ends. It returns stable=true when the session lived at least
// connectionStableAfter past a successful CONNACK — measured from when the
// handshake completed, NOT the attempt start, so a slow connect/handshake that
// ultimately FAILS can't masquerade as uptime. A never-established attempt
// returns stable=false.
func (c *Connector) connectOnce(ctx context.Context, reg RegisterResponse) (stable bool, err error) {
	// onError logs socket-level events (poison-drop, kicks) that are not fatal to
	// the read loop — previously a no-op, which silently swallowed them. The
	// server DISCONNECT case ends the read loop via run returning, which Run's
	// reconnect path handles; this hook is for the informational drops.
	sock := newSocketConn(reg.WSURL, reg.RobotID, reg.IMToken, c.deviceID, c.onInbound, func(err error) {
		c.logf("socket: %v", err)
	})
	sock.seen = c.markSeen // inbound message-id dedup (P1); no-op when store unset
	c.mu.Lock()
	c.sock = sock
	c.mu.Unlock()
	// Always release the socket (fd + ping/watch goroutines) when this attempt
	// ends, so reconnects don't accumulate connections.
	defer sock.close()

	if err := sock.connect(ctx); err != nil {
		return false, err
	}
	establishedAt := time.Now()
	err = sock.run(ctx)
	return time.Since(establishedAt) >= connectionStableAfter, err
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

// setUID writes the post-Register server uid into both c.botUID (read
// by sink callbacks under c.mu) and c.policy.BotUID (read by the
// classifier under policyMu) so every dispatch path — inbound, cron,
// future webhook — sees the live uid without a per-callsite override.
func (c *Connector) setUID(id string) {
	c.mu.Lock()
	c.botUID = id
	c.mu.Unlock()
	c.policyMu.Lock()
	c.policy.BotUID = id
	c.policyMu.Unlock()
}

func (c *Connector) uid() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.botUID
}

// logf reports a recovered/handled error tagged with the bot uid via the
// shared slog handler. format+args follow Printf conventions for source
// compatibility with the dozens of in-place call sites; structured
// upgrades land on a per-caller basis when the line gets touched.
func (c *Connector) logf(format string, args ...any) {
	clog.For("octo").Warn(fmt.Sprintf(format, args...), "bot", c.uid())
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
