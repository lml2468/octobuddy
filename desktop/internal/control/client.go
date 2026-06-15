// Package control is the desktop app's client for the xclawd control bus —
// NDJSON envelopes over a Unix-domain socket (see proto/README.md). It reuses
// the wire types and codec from the daemon's control package so the GUI and the
// daemon stay locked to one contract.
//
// The client is deliberately thin: it owns the socket, a single writer, and a
// read loop that decodes envelopes and hands them to a callback. Request/response
// correlation, reconnection policy, and the view-model reducer live above this.
package control

import (
	"fmt"
	"net"
	"sync"

	wire "github.com/lml2468/xclaw/core/control/wire"
)

// Re-export the wire vocabulary so callers depend on this package, not the
// daemon's, for the contract surface they use.
type (
	Envelope            = wire.Envelope
	Kind                = wire.Kind
	SessionSendBody     = wire.SessionSendBody
	SessionHistoryBody  = wire.SessionHistoryBody
	SecretInjectBody    = wire.SecretInjectBody
	CronCreateBody      = wire.CronCreateBody
	CronListBody        = wire.CronListBody
	CronDeleteBody      = wire.CronDeleteBody
	CronTaskInfo        = wire.CronTaskInfo
	HealthBody          = wire.HealthBody
	BotInfo             = wire.BotInfo
	OKBody              = wire.OKBody
	SessionTextBody     = wire.SessionTextBody
	SessionToolBody     = wire.SessionToolBody
	SessionUsageBody    = wire.SessionUsageBody
	SessionReplyBody    = wire.SessionReplyBody
	SessionActivityBody = wire.SessionActivityBody
	ErrorBody           = wire.ErrorBody
	HistoryMessage      = wire.HistoryMessage
)

const (
	KindCommand  = wire.KindCommand
	KindResponse = wire.KindResponse
	KindEvent    = wire.KindEvent
)

// Client is a connected control-bus session. Not safe to Dial twice; create a
// new Client per connection (the supervisor does this on reconnect).
type Client struct {
	mu     sync.Mutex
	conn   net.Conn
	closed bool
	idSeq  uint64
}

// Dial connects to the control socket at path. The caller starts Read in a
// goroutine to consume the event/response stream.
func Dial(path string) (*Client, error) {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, fmt.Errorf("dial control socket %s: %w", path, err)
	}
	return &Client{conn: conn}, nil
}

// Send writes one command envelope and returns the correlation id it assigned.
// The matching response arrives asynchronously via Read as a KindResponse
// envelope carrying the same id.
func (c *Client) Send(cmdType string, body any) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.conn == nil {
		return "", fmt.Errorf("control: not connected")
	}
	c.idSeq++
	id := fmt.Sprintf("c%d", c.idSeq)
	raw, err := marshalBody(body)
	if err != nil {
		return "", err
	}
	line, err := wire.Encode(wire.Envelope{
		Kind: wire.KindCommand,
		ID:   id,
		Type: cmdType,
		Body: raw,
	})
	if err != nil {
		return "", err
	}
	if _, err := c.conn.Write(line); err != nil {
		return "", fmt.Errorf("control write: %w", err)
	}
	return id, nil
}

// Read consumes the NDJSON stream until the connection closes, invoking onEnv
// for every decoded envelope (responses and events alike). It returns the error
// that ended the loop (nil on clean close).
func (c *Client) Read(onEnv func(Envelope)) error {
	sc := wire.NewScanner(c.conn)
	for sc.Scan() {
		env, err := wire.Decode(sc.Bytes())
		if err != nil {
			// Skip an undecodable frame rather than tearing down the stream.
			continue
		}
		onEnv(env)
	}
	return sc.Err()
}

// Close shuts the socket; Read returns shortly after.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	if c.conn != nil {
		_ = c.conn.Close()
	}
}
