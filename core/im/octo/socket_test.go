package octo

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// buildConnack assembles a minimal success CONNACK (flags=0 → no server-version
// byte, server version stays 0 → no nodeId), matching readConnack's parser.
func buildConnack(t *testing.T) []byte {
	t.Helper()
	kp, err := generateDHKeyPair() // a valid curve25519 public key for the DH
	if err != nil {
		t.Fatalf("keypair: %v", err)
	}
	var b encoder
	b.writeInt64(0) // timeDiff (unused)
	b.writeByte(1)  // reason = success
	b.writeString(base64.StdEncoding.EncodeToString(kp.pub[:]))
	b.writeString("0123456789abcdef") // 16-byte salt → 16-byte IV
	return frame(pktConnack, b.buf)
}

// newSilentHandshakeServer returns an httptest server that completes the WS
// handshake (CONNECT → CONNACK) then goes silent, draining client frames until
// the client closes. Used by the run()-lifecycle tests, which differ only in
// how they end the blocked read (ctx cancel vs read-deadline timeout).
func newSilentHandshakeServer(t *testing.T) *httptest.Server {
	t.Helper()
	connack := buildConnack(t)
	up := websocket.Upgrader{}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		if _, _, err := c.ReadMessage(); err != nil { // CONNECT
			return
		}
		if err := c.WriteMessage(websocket.BinaryMessage, connack); err != nil {
			return
		}
		for { // go silent; block until the client closes
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}))
}

// TestSocketRunStopsOnContextCancel verifies the shutdown fix: gorilla's
// ReadMessage does not observe ctx, so run must close the conn on cancellation
// and return promptly (rather than blocking until the WS naturally errors).
func TestSocketRunStopsOnContextCancel(t *testing.T) {
	srv := newSilentHandshakeServer(t)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	sock := newSocketConn(wsURL, "uid", "tok", "dev-test", func(BotMessage) {}, func(error) {})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := sock.connect(ctx); err != nil {
		t.Fatalf("connect/handshake: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- sock.run(ctx) }()

	// Give run a moment to settle into the blocking read, then cancel.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run should return nil after ctx cancel (closed), got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run did not return after context cancel — ReadMessage was not unblocked")
	}
}

// TestSocketRunTimesOutOnSilentServer verifies the half-open-connection fix: a
// server that completes the handshake but then sends NO further frames must not
// wedge run() forever. The rolling read deadline must fire and return a non-nil
// error so Run's reconnect path engages.
func TestSocketRunTimesOutOnSilentServer(t *testing.T) {
	srv := newSilentHandshakeServer(t)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	sock := newSocketConn(wsURL, "uid", "tok", "dev-test", func(BotMessage) {}, func(error) {})
	sock.readTimeout = 100 * time.Millisecond // shrink for the test (per-conn, no global mutation)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := sock.connect(ctx); err != nil {
		t.Fatalf("connect/handshake: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- sock.run(ctx) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("run should return a non-nil error when the server goes silent")
		}
		if ctx.Err() != nil {
			t.Fatalf("run returned due to ctx cancel, not the read deadline: %v", err)
		}
		if !strings.Contains(err.Error(), "ws read") {
			t.Fatalf("expected a ws read (timeout) error, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run did not return after the read deadline — half-open connection not detected")
	}
}

// TestSocketReadErr verifies the close-cause plumbing that decides run()'s
// return value: an open socket reports the raw fault (→ reconnect), a clean
// close() reports nil (→ shutdown), and closeWithCause reports the cause (→
// logged reconnect), with the first cause winning over a later close().
func TestSocketReadErr(t *testing.T) {
	raw := errors.New("ws read: i/o timeout")

	t.Run("open → raw fault returned", func(t *testing.T) {
		s := &socketConn{}
		if got := s.readErr(raw); got != raw {
			t.Fatalf("open socket: want raw error, got %v", got)
		}
	})

	t.Run("clean close → nil", func(t *testing.T) {
		s := &socketConn{}
		s.close()
		if got := s.readErr(raw); got != nil {
			t.Fatalf("clean close: want nil, got %v", got)
		}
	})

	t.Run("closeWithCause → cause returned, first wins", func(t *testing.T) {
		s := &socketConn{}
		cause := errors.New("ws ping: boom")
		s.closeWithCause(cause)
		if got := s.readErr(raw); !errors.Is(got, cause) {
			t.Fatalf("want %v, got %v", cause, got)
		}
		// Idempotent: a later close() must not overwrite the recorded cause.
		s.close()
		if got := s.readErr(raw); !errors.Is(got, cause) {
			t.Fatalf("first cause must win, got %v", got)
		}
	})
}

func TestDecodeDisconnect(t *testing.T) {
	t.Run("code and reason", func(t *testing.T) {
		var b encoder
		b.writeByte(2) // reason code
		b.writeString("kicked: duplicate login")
		err := decodeDisconnect(b.buf)
		if !errors.Is(err, errServerDisconnect) {
			t.Fatalf("must wrap errServerDisconnect, got %v", err)
		}
		if !strings.Contains(err.Error(), "reason 2") || !strings.Contains(err.Error(), "duplicate login") {
			t.Fatalf("missing code/reason in %q", err.Error())
		}
	})

	t.Run("code only (no reason string)", func(t *testing.T) {
		err := decodeDisconnect([]byte{3})
		if !errors.Is(err, errServerDisconnect) {
			t.Fatalf("must wrap errServerDisconnect, got %v", err)
		}
		if !strings.Contains(err.Error(), "reason 3") {
			t.Fatalf("missing code in %q", err.Error())
		}
	})

	t.Run("empty body falls back to bare sentinel", func(t *testing.T) {
		if err := decodeDisconnect(nil); !errors.Is(err, errServerDisconnect) {
			t.Fatalf("must wrap errServerDisconnect, got %v", err)
		}
	})
}

func TestApplyConnackPayloadReason(t *testing.T) {
	newSock := func() *socketConn {
		kp, err := generateDHKeyPair()
		if err != nil {
			t.Fatalf("keypair: %v", err)
		}
		return &socketConn{dh: kp}
	}
	// A valid server key + 16-byte salt so the success path can derive the IV.
	srvKey := func() string {
		kp, err := generateDHKeyPair()
		if err != nil {
			t.Fatalf("keypair: %v", err)
		}
		return base64.StdEncoding.EncodeToString(kp.pub[:])
	}()

	t.Run("auth fail is errors.Is errConnackAuthFail", func(t *testing.T) {
		err := newSock().applyConnackPayload(connackPayload{reason: connackReasonAuthFail})
		if !errors.Is(err, errConnackAuthFail) {
			t.Fatalf("reason 2 must wrap errConnackAuthFail, got %v", err)
		}
	})

	t.Run("other non-success reason is generic (not authfail)", func(t *testing.T) {
		err := newSock().applyConnackPayload(connackPayload{reason: 3})
		if err == nil {
			t.Fatal("reason 3 must error")
		}
		if errors.Is(err, errConnackAuthFail) {
			t.Fatalf("reason 3 must NOT be errConnackAuthFail, got %v", err)
		}
	})

	t.Run("success derives key and returns nil", func(t *testing.T) {
		err := newSock().applyConnackPayload(connackPayload{
			reason: 1, serverKey: srvKey, salt: "0123456789abcdef",
		})
		if err != nil {
			t.Fatalf("reason 1 must succeed, got %v", err)
		}
	})
}

// buildTestRecvBody assembles a RECV body (srvVer 0 layout) that onRecv can
// decode: setting, msgKey, fromUID, channelID, channelType, clientMsgNo,
// messageID, messageSeq, timestamp, then the AES-encrypted payload.
func buildTestRecvBody(t *testing.T, messageID uint64, key, iv []byte) []byte {
	t.Helper()
	payload := encryptLikeServer(t, []byte(`{"type":1,"content":"hi"}`), key, iv)
	var b encoder
	b.writeByte(0)          // setting (no topic, no stream)
	b.writeString("mk")     // msgKey (unused)
	b.writeString("u-peer") // fromUID
	b.writeString("c1")     // channelID
	b.writeByte(1)          // channelType (DM)
	b.writeString("cmn")    // clientMsgNo (unused)
	b.writeInt64(messageID)
	b.writeInt32(7) // messageSeq
	b.writeInt32(0) // timestamp
	b.writeBytes(payload)
	return b.buf
}

// TestOnRecvDecryptsAndDispatches is the regression guard for the read-loop
// decrypt→ack→dispatch path (dedup now lives in the connector, not here): a
// valid frame is dispatched to onMessage exactly once and its decrypt-failure
// counter is cleared. An undecryptable frame is NOT dispatched.
func TestOnRecvDecryptsAndDispatches(t *testing.T) {
	key := []byte("0123456789abcdef")
	iv := []byte("0123456789abcdef")
	var dispatched int
	sock := &socketConn{
		aesKey:       key,
		aesIV:        iv,
		decryptFails: map[string]int{},
		onMessage:    func(BotMessage) { dispatched++ },
	}
	// Valid frame → decrypts and dispatches once (writeRaw ack to a nil conn is a
	// harmless no-op; the dispatch proves the path reached ack+forward).
	sock.onRecv(buildTestRecvBody(t, 42, key, iv))
	if dispatched != 1 {
		t.Fatalf("valid frame must dispatch once, got %d", dispatched)
	}
	if _, ok := sock.decryptFails["42"]; ok {
		t.Fatal("a successful decrypt must leave no decrypt-failure strike")
	}

	// Undecryptable frame (wrong key) → not dispatched.
	bad := &socketConn{
		aesKey:       []byte("ffffffffffffffff"),
		aesIV:        iv,
		decryptFails: map[string]int{},
		onMessage:    func(BotMessage) { dispatched++ },
	}
	bad.onRecv(buildTestRecvBody(t, 99, key, iv)) // encrypted with the other key
	if dispatched != 1 {
		t.Fatalf("undecryptable frame must not dispatch (count still 1), got %d", dispatched)
	}
}
