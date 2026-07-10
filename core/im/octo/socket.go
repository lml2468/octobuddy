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
	// deviceID is the stable per-bot WuKongIM device id sent in CONNECT. NOTE:
	// the server's duplicate-login kick (conflicts() in octo-im presence/
	// directory.go) ignores deviceID for DeviceLevel=Master — which is how
	// octo-server registers bots — so a stable id does NOT prevent the kick for
	// the current setup. It is kept as a defensive measure (correct same-device
	// semantics) and matters only if a bot ever connects as DeviceLevel=Slave.
	// Empty falls back to a per-connection random id (dev/REPL path).
	deviceID string

	onMessage func(BotMessage)
	onError   func(error)

	conn   *websocket.Conn
	dh     dhKeyPair
	aesKey []byte
	aesIV  []byte
	srvVer int

	mu       sync.Mutex
	closed   bool
	closeErr error // cause passed to closeWithCause; nil for a clean (ctx-cancel) close

	// readTimeout bounds how long run() waits for any inbound frame before
	// declaring the link dead (rolling: reset before every read, so any frame —
	// PONG, RECV — keeps a healthy link alive while a half-open connection with
	// no FIN/RST trips it instead of blocking ReadMessage forever). A per-conn
	// field (like the connector's reconnectBase/Max) so tests can shrink it
	// without mutating global state. Defaults to wsReadTimeout.
	readTimeout time.Duration

	decryptFails map[string]int
}

const (
	wsPingInterval     = 60 * time.Second
	maxDecryptRetries  = 3
	maxDecryptFailKeys = 1000
)

// wsReadTimeout is the default socketConn.readTimeout: 2.5× the ping interval so
// one dropped ping/pong round-trip doesn't false-trip, but a dead connection is
// still detected within ~one window.
const wsReadTimeout = wsPingInterval * 5 / 2 // 150s

func newSocketConn(wsURL, uid, token, deviceID string, onMessage func(BotMessage), onError func(error)) *socketConn {
	return &socketConn{
		wsURL: wsURL, uid: uid, token: token, deviceID: deviceID,
		onMessage: onMessage, onError: onError,
		readTimeout:  wsReadTimeout,
		decryptFails: make(map[string]int),
	}
}

// NewDeviceID mints a fresh WuKongIM device id. The trailing "W" is the device
// suffix the Octo wire protocol expects; this is the single source of that
// format, shared by the random-fallback path here and the persisted-id path in
// the daemon (loadOrCreateDeviceID).
func NewDeviceID() string { return uuid.NewString() + "W" }

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

	// Send CONNECT. A stable per-bot deviceID (see field doc) makes a reconnect
	// look like the same device resuming rather than a new device kicking the
	// old session; fall back to a random id when none was persisted.
	deviceID := s.deviceID
	if deviceID == "" {
		deviceID = NewDeviceID()
	}
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

// close tears down the connection as a CLEAN shutdown (ctx cancel): run()
// returns nil. For a fault that should drive a reconnect, use closeWithCause.
func (s *socketConn) close() { s.closeWithCause(nil) }

// closeWithCause closes the connection, recording cause so run() reports it
// (and the connector logs + reconnects) rather than treating the close as a
// clean shutdown. Idempotent; the first cause wins.
func (s *socketConn) closeWithCause(cause error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	s.closeErr = cause
	// Close the underlying conn under the same lock as the flag flip so a
	// concurrent writeRaw can't observe closed=false and then race past
	// s.conn.Close into WriteMessage on a half-closed conn.
	if s.conn != nil {
		_ = s.conn.Close()
	}
}

// readErr converts a read/deadline error from run()'s loop into run's return
// value, accounting for a concurrent close():
//   - not closed       → the raw fault (live read/deadline error) → reconnect
//   - closed cleanly    → nil (ctx-cancel shutdown)
//   - closed with cause → the recorded cause (e.g. ping failure) → reconnect
//
// raw is only used in the not-closed case, so building it eagerly at the call
// site costs nothing measurable (it's the error path).
func (s *socketConn) readErr(raw error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		return raw
	}
	return s.closeErr
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
	// reason==ReasonAuthFail means the token the server has on file no longer
	// matches — only THEN must the caller re-register (the one operation that
	// triggers the server's duplicate-login kick). Any other non-success reason
	// is surfaced generically so the reconnect loop just retries.
	if payload.reason == connackReasonAuthFail {
		return fmt.Errorf("%w (connack reason %d)", errConnackAuthFail, payload.reason)
	}
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

// connackReasonAuthFail mirrors WuKongIM's frame.ReasonAuthFail enum value
// (octo-im pkg/protocol/frame/common.go: Unknown=0, Success=1, AuthFail=2). We
// hardcode it rather than importing the server proto so the connector stays a
// dependency-free leaf, matching the wire.go pkt* constants.
const connackReasonAuthFail = 2

// errConnackAuthFail is wrapped when CONNACK comes back with ReasonAuthFail —
// the only condition under which the reconnect loop should re-register (token
// the server holds is stale). errors.Is against it gates the re-register path.
var errConnackAuthFail = errors.New("connack: auth failed")

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
		// Roll the read deadline before every read so a half-open connection
		// trips it instead of blocking ReadMessage forever (see readTimeout).
		if err := s.conn.SetReadDeadline(time.Now().Add(s.readTimeout)); err != nil {
			return s.readErr(fmt.Errorf("ws set read deadline: %w", err))
		}
		_, data, err := s.conn.ReadMessage()
		if err != nil {
			return s.readErr(fmt.Errorf("ws read: %w", err))
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
			// A server DISCONNECT must end the read loop so the caller
			// reconnects — otherwise we'd block in ReadMessage on a dead
			// session, appearing "connected" while receiving nothing. Decode the
			// reason so the connector can log WHY (duplicate-login kick, token
			// expiry, idle timeout) instead of a bare "server sent disconnect".
			// The reconnect reuses the cached registration; only a CONNACK
			// auth-fail on a later attempt re-registers (see Connector.Run).
			if pt == pktDisconnect {
				return decodeDisconnect(body)
			}
			s.dispatch(pt, body)
			data = data[consumed:]
		}
	}
}

// errServerDisconnect is the sentinel wrapped by every DISCONNECT error so
// callers can errors.Is against it regardless of the decoded reason. Run's
// reconnect path reconnects (reusing the cached registration) instead of
// hanging on a dead WS.
var errServerDisconnect = errors.New("server sent disconnect")

// decodeDisconnect parses a WuKongIM DISCONNECT body (reasonCode uint8 +
// reason string; WuKongIMGoProto DisconnectPacket) and wraps errServerDisconnect
// with the human-readable cause. A malformed/empty body still yields the bare
// sentinel — the disconnect itself is the actionable signal, the reason is a
// best-effort annotation.
func decodeDisconnect(body []byte) error {
	d := &decoder{buf: body}
	code, err := d.readByte()
	if err != nil {
		return errServerDisconnect
	}
	reason, _ := d.readString() // best-effort; absent on some server versions
	if reason != "" {
		return fmt.Errorf("%w (reason %d: %s)", errServerDisconnect, code, reason)
	}
	return fmt.Errorf("%w (reason %d)", errServerDisconnect, code)
}

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
			if err := s.writeRaw(encodePing()); err != nil {
				// Write failure means the conn is already broken; close it WITH
				// the cause so the read loop unblocks and run() returns this
				// error (logged + reconnected by the connector) rather than a
				// nil that looks like a clean shutdown. The read deadline would
				// catch it too, but this surfaces a dead conn faster.
				s.closeWithCause(fmt.Errorf("ws ping: %w", err))
				return
			}
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
