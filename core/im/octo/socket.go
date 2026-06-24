package octo

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// socketConn speaks the WuKongIM binary protocol over a WebSocket: it performs
// the CONNECT/CONNACK handshake, derives the AES key, then reads RECV packets,
// decrypts them, acks them, and forwards BotMessages. Outbound business
// messages go via REST (this never SENDs over WS), so only the decrypt
// direction is implemented. Ported from socket.ts.
//
// Concurrency invariant: onRecv, handleDecryptFailure, dispatch, and the
// aesKey/aesIV/srvVer/decryptFails fields are ONLY ever touched from the single
// run read-loop goroutine, so they need no lock. (writeRaw is the exception —
// it may be called from the ping loop and the read loop, so conn/closed are
// guarded by mu.) The connector dispatches turns onto its OWN worker goroutines
// AFTER onMessage returns (see Connector.enqueueTurn), so those goroutines never
// reach back into socketConn — keep it that way: do NOT call onRecv/decrypt paths
// concurrently or these fields must grow a lock.
type socketConn struct {
	wsURL string
	uid   string
	token string

	onMessage func(BotMessage)
	onError   func(error)

	conn   *websocket.Conn
	dh     dhKeyPair
	aesKey []byte
	aesIV  []byte
	srvVer int

	mu     sync.Mutex
	closed bool

	decryptFails map[string]int
}

const (
	wsPingInterval     = 60 * time.Second
	maxDecryptRetries  = 3
	maxDecryptFailKeys = 1000
)

func newSocketConn(wsURL, uid, token string, onMessage func(BotMessage), onError func(error)) *socketConn {
	return &socketConn{
		wsURL: wsURL, uid: uid, token: token,
		onMessage: onMessage, onError: onError,
		decryptFails: make(map[string]int),
	}
}

// connect dials the WS, runs the handshake, and starts the read + ping loops.
// Returns once CONNACK succeeds; runs until close or fatal error.
func (s *socketConn) connect(ctx context.Context) error {
	kp, err := generateDHKeyPair()
	if err != nil {
		return err
	}
	s.dh = kp

	c, _, err := websocket.DefaultDialer.DialContext(ctx, s.wsURL, nil)
	if err != nil {
		return fmt.Errorf("ws dial: %w", err)
	}
	// Cap a single WS message so a malicious/corrupt server can't make ReadMessage
	// buffer unbounded bytes (OOM DoS). gorilla's default is unlimited (0). A WS
	// message may carry several frames, so allow a little headroom over the
	// per-frame body cap. Defense in depth with nextFrame's remLen check (M4).
	c.SetReadLimit(maxFrameBodyBytes + 64*1024)
	s.conn = c

	// Send CONNECT.
	deviceID := uuid.NewString() + "W"
	timestampMs := uint64(time.Now().UnixMilli())
	if err := s.writeRaw(encodeConnect(deviceID, s.uid, s.token, timestampMs, kp.pubKeyBase64())); err != nil {
		c.Close()
		return err
	}

	// Read CONNACK (first frame).
	if err := s.readConnack(); err != nil {
		c.Close()
		return err
	}
	return nil
}

func (s *socketConn) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	// Close the underlying conn under the same lock as the flag flip so a
	// concurrent writeRaw can't observe closed=false and then race past
	// s.conn.Close into WriteMessage on a half-closed conn.
	if s.conn != nil {
		_ = s.conn.Close()
	}
}

func (s *socketConn) writeRaw(b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.conn == nil {
		return ErrSocketClosed
	}
	return s.conn.WriteMessage(websocket.BinaryMessage, b)
}

type connackPayload struct {
	reason    byte
	serverKey string
	salt      string
}

func (s *socketConn) readConnack() error {
	// Bound the CONNACK wait. The ctx-watcher goroutine that calls
	// s.close() on cancellation is only started later in run(); without
	// a deadline here, a peer that completes the HTTP upgrade but never
	// sends the first frame blocks the dial forever and wedges daemon
	// shutdown (Connector.Run blocks → runBot blocks → defer chain
	// can't fire → SIGTERM ineffective without SIGKILL).
	_ = s.conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	defer s.conn.SetReadDeadline(time.Time{})
	_, data, err := s.conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("read connack: %w", err)
	}
	body, hasServerVersion, err := parseConnackFrame(data)
	if err != nil {
		return err
	}
	payload, err := s.decodeConnackBody(body, hasServerVersion)
	if err != nil {
		return err
	}
	return s.applyConnackPayload(payload)
}

func parseConnackFrame(data []byte) ([]byte, bool, error) {
	pt, body, _, ok, ferr := nextFrame(data)
	if ferr != nil || !ok {
		return nil, false, fmt.Errorf("connack frame: ok=%v err=%v", ok, ferr)
	}
	if pt != pktConnack {
		return nil, false, fmt.Errorf("expected CONNACK, got packet %d", pt)
	}
	return body, headerFlags(data[0])&0x01 == 1, nil
}

func (s *socketConn) decodeConnackBody(body []byte, hasServerVersion bool) (connackPayload, error) {
	d := &decoder{buf: body}
	if hasServerVersion {
		v, _ := d.readByte()
		s.srvVer = int(v)
	}
	if _, err := d.readInt64(); err != nil { // timeDiff (unused)
		return connackPayload{}, err
	}
	reason, err := d.readByte()
	if err != nil {
		return connackPayload{}, err
	}
	serverKey, err := d.readString()
	if err != nil {
		return connackPayload{}, err
	}
	salt, err := d.readString()
	if err != nil {
		return connackPayload{}, err
	}
	if s.srvVer >= 4 {
		_, _ = d.readInt64() // nodeId (unused)
	}
	return connackPayload{reason: reason, serverKey: serverKey, salt: salt}, nil
}

func (s *socketConn) applyConnackPayload(payload connackPayload) error {
	if payload.reason != 1 {
		return fmt.Errorf("connack reason %d (not success)", payload.reason)
	}
	key, iv, err := deriveAESKeyIV(s.dh.priv, payload.serverKey, payload.salt)
	if err != nil {
		return err
	}
	s.aesKey, s.aesIV = key, iv
	return nil
}

// run reads frames until the connection ends. Sends pings on an interval.
func (s *socketConn) run(ctx context.Context) error {
	// gorilla's ReadMessage below does not observe ctx, so on cancellation we
	// close the conn to unblock it and return promptly. The derived cancel +
	// defer also tears the watcher down when run returns for any other reason
	// (read error), so the goroutine never leaks.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		s.close()
	}()

	pingDone := make(chan struct{})
	go s.pingLoop(pingDone)
	defer close(pingDone)

	for {
		_, data, err := s.conn.ReadMessage()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return nil
			}
			return fmt.Errorf("ws read: %w", err)
		}
		// A WS message may contain one or more protocol frames.
		for len(data) > 0 {
			pt, body, consumed, ok, ferr := nextFrame(data)
			if ferr != nil {
				return ferr
			}
			if !ok {
				break // wait for more bytes (rare: WS preserves message boundaries)
			}
			// A server DISCONNECT must end the read loop so the caller reconnects
			// (and re-registers) — otherwise we'd block in ReadMessage on a dead
			// session, appearing "connected" while receiving nothing.
			if pt == pktDisconnect {
				return errServerDisconnect
			}
			s.dispatch(pt, body)
			data = data[consumed:]
		}
	}
}

// errServerDisconnect is returned by run when the server sends a DISCONNECT
// packet, so Run's reconnect path re-registers instead of hanging on a dead WS.
var errServerDisconnect = fmt.Errorf("server sent disconnect")

// ErrSocketClosed is returned by writeRaw after the socket has been closed.
// Exported as a sentinel so callers (connector outbound retry, tests) can
// errors.Is against it to distinguish from a WebSocket I/O failure.
var ErrSocketClosed = errors.New("socket closed")

func (s *socketConn) pingLoop(done chan struct{}) {
	t := time.NewTicker(wsPingInterval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			_ = s.writeRaw(encodePing())
		}
	}
}

func (s *socketConn) dispatch(pt packetType, body []byte) {
	switch pt {
	case pktPong:
		// keepalive ack; nothing to do
	case pktRecv:
		s.onRecv(body)
	// pktDisconnect is handled in run (it must end the read loop), not here.
	default:
		// SENDACK and others ignored
	}
}
