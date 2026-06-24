package octo

import (
	"context"
	"time"
)

// typingTicker holds the cancel hook and the done channel of one session's
// typing-heartbeat goroutine. stop cancels and waits for the goroutine to
// exit so there is never a leaked goroutine after a turn.
type typingTicker struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type typingHeartbeatConfig struct {
	interval time.Duration
	send     func(context.Context, string, ChannelType) error
}

func (c *Connector) typingHeartbeatConfig() typingHeartbeatConfig {
	interval := c.typingInterval
	if interval <= 0 {
		interval = defaultTypingInterval
	}
	send := c.sendTyping
	if send == nil {
		send = c.rest.SendTyping
	}
	return typingHeartbeatConfig{interval: interval, send: send}
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
	cfg := c.typingHeartbeatConfig()

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
	if err := cfg.send(ctx, tgt.channelID, tgt.channelType); err != nil && ctx.Err() == nil {
		c.logf("send typing for %s: %v", sessionKey, err)
	}

	go func() {
		defer close(tt.done)
		t := time.NewTicker(cfg.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := cfg.send(ctx, tgt.channelID, tgt.channelType); err != nil && ctx.Err() == nil {
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
