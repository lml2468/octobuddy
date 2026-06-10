package octo

import (
	"context"
	"encoding/json"
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
	s.conn = c

	// Send CONNECT.
	deviceID := uuid.NewString() + "W"
	ts := uint64(time.Now().UnixMilli())
	if err := s.writeRaw(encodeConnect(deviceID, s.uid, s.token, ts, kp.pubKeyBase64())); err != nil {
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
	s.closed = true
	s.mu.Unlock()
	if s.conn != nil {
		_ = s.conn.Close()
	}
}

func (s *socketConn) writeRaw(b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.conn == nil {
		return fmt.Errorf("socket closed")
	}
	return s.conn.WriteMessage(websocket.BinaryMessage, b)
}

func (s *socketConn) readConnack() error {
	_, data, err := s.conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("read connack: %w", err)
	}
	pt, body, _, ok, ferr := nextFrame(data)
	if ferr != nil || !ok {
		return fmt.Errorf("connack frame: ok=%v err=%v", ok, ferr)
	}
	if pt != pktConnack {
		return fmt.Errorf("expected CONNACK, got packet %d", pt)
	}
	hasServerVersion := headerFlags(data[0])&0x01 == 1

	d := &decoder{buf: body}
	if hasServerVersion {
		v, _ := d.readByte()
		s.srvVer = int(v)
	}
	if _, err := d.readInt64(); err != nil { // timeDiff (unused)
		return err
	}
	reason, err := d.readByte()
	if err != nil {
		return err
	}
	serverKey, err := d.readString()
	if err != nil {
		return err
	}
	salt, err := d.readString()
	if err != nil {
		return err
	}
	if s.srvVer >= 4 {
		_, _ = d.readInt64() // nodeId (unused)
	}
	if reason != 1 {
		return fmt.Errorf("connack reason %d (not success)", reason)
	}
	key, iv, err := deriveAESKeyIV(s.dh.priv, serverKey, salt)
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
			s.dispatch(pt, data[0], body)
			data = data[consumed:]
		}
	}
}

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

func (s *socketConn) dispatch(pt packetType, header byte, body []byte) {
	switch pt {
	case pktPong:
		// keepalive ack; nothing to do
	case pktRecv:
		s.onRecv(header, body)
	case pktDisconnect:
		if s.onError != nil {
			s.onError(fmt.Errorf("kicked by server"))
		}
	default:
		// SENDACK and others ignored
	}
}

// settingByte bit layout (socket.ts parseSettingByte).
func settingTopic(v byte) bool    { return (v>>3)&0x01 == 1 }
func settingStreamOn(v byte) bool { return (v>>1)&0x01 == 1 }

// onRecv parses a RECV body, decrypts the payload, acks, and forwards.
func (s *socketConn) onRecv(header byte, body []byte) {
	d := &decoder{buf: body}
	setting, err := d.readByte()
	if err != nil {
		return
	}
	if _, err = d.readString(); err != nil { // msgKey (unused)
		return
	}
	fromUID, _ := d.readString()
	channelID, _ := d.readString()
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
	if len(s.decryptFails) > maxDecryptFailKeys {
		s.decryptFails = make(map[string]int)
	}
	s.decryptFails[idStr]++
	if s.decryptFails[idStr] >= maxDecryptRetries {
		_ = s.writeRaw(encodeRecvack(messageID, messageSeq)) // drop poison msg
		delete(s.decryptFails, idStr)
		if s.onError != nil {
			s.onError(fmt.Errorf("dropping undecryptable message %s: %w", idStr, cause))
		}
	}
	// else: no ack → redelivery
}

// parsePayload decodes the decrypted JSON into a MessagePayload, defaulting
// type to 0 when absent (socket.ts builds { type: type ?? 0, ... }).
func parsePayload(b []byte) (MessagePayload, error) {
	var p MessagePayload
	if err := jsonUnmarshal(b, &p); err != nil {
		return MessagePayload{}, err
	}
	return p, nil
}
