package octo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

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

// settingByte bit layout (socket.ts parseSettingByte).
func settingTopic(v byte) bool    { return (v>>3)&0x01 == 1 }
func settingStreamOn(v byte) bool { return (v>>1)&0x01 == 1 }

// onRecv parses a RECV body, decrypts the payload, acks, and forwards.
func (s *socketConn) onRecv(body []byte) {
	d := &decoder{buf: body}
	setting, err := d.readByte()
	if err != nil {
		return
	}
	if _, err = d.readString(); err != nil { // msgKey (unused)
		return
	}
	fromUID, _ := d.readString()
	channelID, cerr := d.readString()
	channelType, _ := d.readByte()
	if s.srvVer >= 3 {
		_, _ = d.readInt32() // expire (unused)
	}
	_, _ = d.readString() // clientMsgNo (unused)
	messageID, err := d.readInt64()
	if err != nil {
		return
	}
	messageSeq, _ := d.readInt32()
	timestamp, _ := d.readInt32()
	if settingTopic(setting) {
		_, _ = d.readString() // topic (unused)
	}
	encrypted := d.readRemaining()

	// A truncated/short frame leaves channelID empty (decoder returns errShort +
	// zero value). Acking and forwarding such a message would route an
	// unaddressable turn, so drop it before the ack (L25). messageID already
	// guarded above.
	if cerr != nil || channelID == "" {
		return
	}

	idStr := strconv.FormatUint(messageID, 10)

	plain, derr := aesDecryptPayload(encrypted, s.aesKey, s.aesIV)
	if derr != nil {
		s.handleDecryptFailure(idStr, messageID, messageSeq, derr)
		return
	}
	// Success: clear failure count, ack (after successful decrypt+parse), forward.
	payload, perr := parsePayload(plain)
	if perr != nil {
		s.handleDecryptFailure(idStr, messageID, messageSeq, perr)
		return
	}
	delete(s.decryptFails, idStr)
	_ = s.writeRaw(encodeRecvack(messageID, messageSeq))

	if s.onMessage != nil {
		s.onMessage(BotMessage{
			MessageID:   idStr,
			MessageSeq:  messageSeq,
			FromUID:     fromUID,
			ChannelID:   channelID,
			ChannelType: ChannelType(channelType),
			Timestamp:   timestamp,
			Payload:     payload,
			StreamOn:    settingStreamOn(setting),
		})
	}
}

// handleDecryptFailure implements the 3-strike poison-drop (socket.ts): below
// the cap, do NOT ack (server redelivers); at the cap, ack-and-drop.
func (s *socketConn) handleDecryptFailure(idStr string, messageID uint64, messageSeq uint32, cause error) {
	s.decryptFails[idStr]++
	if s.decryptFails[idStr] >= maxDecryptRetries {
		_ = s.writeRaw(encodeRecvack(messageID, messageSeq)) // drop poison msg
		delete(s.decryptFails, idStr)
		if s.onError != nil {
			s.onError(fmt.Errorf("dropping undecryptable message %s: %w", idStr, cause))
		}
		return
	}
	// Bound the map WITHOUT discarding this message's strike count. Resetting the
	// whole map (or evicting a high-strike entry) could zero an in-flight poison
	// message's count so it never reaches maxDecryptRetries — the server would
	// then redeliver it forever (livelock). Evict the LOWEST-strike other entry
	// (a strike-1 entry has the least progress toward a drop, so losing its count
	// costs the least), falling back to any other entry.
	for len(s.decryptFails) > maxDecryptFailKeys {
		victim, victimStrikes := "", 0
		for k, n := range s.decryptFails {
			if k == idStr {
				continue
			}
			if victim == "" || n < victimStrikes {
				victim, victimStrikes = k, n
			}
		}
		if victim == "" {
			break
		}
		delete(s.decryptFails, victim)
	}
	// else: no ack → redelivery
}

// parsePayload decodes the decrypted JSON into a MessagePayload, defaulting
// type to 0 when absent (socket.ts builds { type: type ?? 0,... }).
func parsePayload(b []byte) (MessagePayload, error) {
	var p MessagePayload
	if err := jsonUnmarshal(b, &p); err != nil {
		return MessagePayload{}, err
	}
	return p, nil
}
