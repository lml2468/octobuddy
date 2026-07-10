package octo

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"path/filepath"
	"testing"

	"github.com/lml2468/octobuddy/core/store"
)

// aesEncryptForTest mirrors aesDecryptPayload's inverse: PKCS7-pad, AES-128-CBC
// encrypt, base64 — so a test can synthesize a RECV payload the real onRecv path
// will decrypt.
func aesEncryptForTest(t *testing.T, plain, key, iv []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	for i := 0; i < pad; i++ {
		plain = append(plain, byte(pad))
	}
	out := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, plain)
	return []byte(base64.StdEncoding.EncodeToString(out))
}

// buildRecvBody assembles a RECV body (srvVer 0 layout) onRecv can decode:
// setting, msgKey, fromUID, channelID, channelType, clientMsgNo, messageID,
// messageSeq, timestamp, then the encrypted payload as the remainder.
func buildRecvBody(t *testing.T, messageID uint64, key, iv []byte) []byte {
	t.Helper()
	payload := aesEncryptForTest(t, []byte(`{"type":1,"content":"hi"}`), key, iv)
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

// TestOnRecvDedupDropsRedelivery: feeding the same RECV frame twice must
// dispatch onMessage exactly once (the seen-set gate), while the frame is still
// ACKED both times (so the server stops redelivering).
func TestOnRecvDedupDropsRedelivery(t *testing.T) {
	key := []byte("0123456789abcdef")
	iv := []byte("0123456789abcdef")
	var dispatched, acks int
	sock := &socketConn{
		aesKey:       key,
		aesIV:        iv,
		decryptFails: map[string]int{},
		onMessage:    func(BotMessage) { dispatched++ },
		ackHook:      func(uint64) { acks++ },
	}
	seenIDs := map[string]bool{}
	sock.seen = func(id string) bool {
		if seenIDs[id] {
			return false
		}
		seenIDs[id] = true
		return true
	}

	body := buildRecvBody(t, 42, key, iv)
	sock.onRecv(body)
	sock.onRecv(body)

	if dispatched != 1 {
		t.Fatalf("onMessage dispatched %d times, want 1 (duplicate must be dropped)", dispatched)
	}
	if acks != 2 {
		t.Fatalf("both deliveries must be acked, got %d acks (want 2)", acks)
	}
}

// TestOnRecvNilSeenAlwaysDispatches: a socketConn without a seen hook (the
// pre-P1 default) dispatches every frame — proves the gate is opt-in and old
// behavior is preserved when unwired.
func TestOnRecvNilSeenAlwaysDispatches(t *testing.T) {
	key := []byte("0123456789abcdef")
	iv := []byte("0123456789abcdef")
	var dispatched int
	sock := &socketConn{
		aesKey:       key,
		aesIV:        iv,
		decryptFails: map[string]int{},
		onMessage:    func(BotMessage) { dispatched++ },
		// seen is nil
	}
	body := buildRecvBody(t, 99, key, iv)
	sock.onRecv(body)
	sock.onRecv(body)
	if dispatched != 2 {
		t.Fatalf("nil seen hook must dispatch every frame; got %d, want 2", dispatched)
	}
}

// TestMarkSeenFailOpenNilStore: a connector with no store dispatches every
// message (fail open) — the dev/REPL default.
func TestMarkSeenFailOpenNilStore(t *testing.T) {
	c := &Connector{} // store nil
	if !c.markSeen("m1") {
		t.Fatal("nil store must fail open (return true = dispatch)")
	}
}

// TestMarkSeenFailOpenOnStoreError: a store error path returns true (dispatch)
// rather than dropping. Simulated by a closed store, whose Exec errors.
func TestMarkSeenFailOpenOnStoreError(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close() // subsequent Exec errors
	c := &Connector{}
	c.SetStore(st)
	if !c.markSeen("m1") {
		t.Fatal("store error must fail open (return true = dispatch)")
	}
}
