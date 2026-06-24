package control

import (
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
)

type client struct {
	conn      net.Conn
	sendCh    chan []byte
	done      chan struct{}
	closeOnce sync.Once
	// authed is set true once this connection presents the valid capability
	// token. Atomic: written on the connection's handleConn goroutine (dispatch)
	// and read by the Broadcast fan-out on the gateway goroutine.
	authed atomic.Bool
	// dropped counts events shed because the send queue was full (slow client).
	// Atomic; surfaced via a throttled log so backpressure is diagnosable (L31)
	// without logging on the hot enqueue path for every drop.
	dropped atomic.Uint64
}

const clientSendQueue = 256

func newClient(conn net.Conn) *client {
	c := &client{conn: conn, sendCh: make(chan []byte, clientSendQueue), done: make(chan struct{})}
	go c.writeLoop()
	return c
}

// writeLoop serializes all writes to this connection. Decoupling writes from
// Broadcast means one slow client cannot stall the broadcaster (and thus the
// agent turn) — it just fills its own queue. Exits when done is closed.
func (c *client) writeLoop() {
	for {
		select {
		case <-c.done:
			return
		case line := <-c.sendCh:
			if _, err := c.conn.Write(line); err != nil {
				return
			}
		}
	}
}

// enqueue queues a line for the client. Never blocks and never panics: sendCh is
// never closed (close signals via the separate done channel), so a send here
// can only fill the buffer (dropped via default) or lose the select to done (the
// client is going away) — neither sends on a closed channel.
func (c *client) enqueue(line []byte) {
	select {
	case c.sendCh <- line:
	case <-c.done:
		// client closing: drop.
	default:
		// queue full: drop. The client is too slow; dropping an event beats
		// stalling every other client and the agent turn. Count drops and log on a
		// power-of-two cadence so persistent backpressure is visible without
		// spamming a log line per drop on the hot path (L31).
		n := c.dropped.Add(1)
		if n&(n-1) == 0 { // n is 1, 2, 4, 8, …
			fmt.Fprintf(os.Stderr, "[control] slow client dropped %d event(s)\n", n)
		}
	}
}

// close stops the write loop and closes the connection. It does NOT close
// sendCh — producers (Broadcast/writeTo) may still hold a reference to this
// client and call enqueue concurrently; closing sendCh would let those sends
// panic. Stopping via `done` keeps enqueue panic-free.
func (c *client) close() {
	c.closeOnce.Do(func() {
		close(c.done)
		_ = c.conn.Close()
	})
}
