package octo

import (
	"context"
	"fmt"
	"os"
	"time"
)

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
		c.setStatus(false, errString(err))

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
