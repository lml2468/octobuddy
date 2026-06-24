package octo

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"testing"
)

// TestDeriveKeyMatchesReference pins the key-derivation algorithm against a
// known vector computed with the SAME steps cc-channel uses (verified against a
// Node reproduction): key = first16(hex(md5(base64(secret)))), iv = salt[:16].
func TestDeriveKeyShape(t *testing.T) {
	// Two independent keypairs; derive from A's priv + B's pub and confirm the
	// AES key is 16 bytes of lowercase hex and the IV is the salt prefix.
	a, err := generateDHKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	b, err := generateDHKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	salt := "0123456789abcdef-extra"
	key, iv, err := deriveAESKeyIV(a.priv, base64.StdEncoding.EncodeToString(b.pub[:]), salt)
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != 16 {
		t.Fatalf("key must be 16 bytes, got %d", len(key))
	}
	if !isLowerHexKey(key) {
		t.Fatalf("key must be lowercase hex chars, got %q", string(key))
	}
	if string(iv) != "0123456789abcdef" {
		t.Fatalf("iv must be first 16 salt bytes, got %q", string(iv))
	}
}

func isLowerHexKey(key []byte) bool {
	for _, c := range key {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// TestDHSharedSecretSymmetric confirms X25519 agreement so both sides derive the
// same AES key (the property the handshake relies on).
func TestDHSharedSecretSymmetric(t *testing.T) {
	a, _ := generateDHKeyPair()
	b, _ := generateDHKeyPair()
	salt := "saltsaltsaltsalt!!"

	keyAB, _, err := deriveAESKeyIV(a.priv, base64.StdEncoding.EncodeToString(b.pub[:]), salt)
	if err != nil {
		t.Fatal(err)
	}
	keyBA, _, err := deriveAESKeyIV(b.priv, base64.StdEncoding.EncodeToString(a.pub[:]), salt)
	if err != nil {
		t.Fatal(err)
	}
	if string(keyAB) != string(keyBA) {
		t.Fatalf("DH not symmetric: %q vs %q", keyAB, keyBA)
	}
}

// TestAESDecryptRoundTrip encrypts with the same scheme the server uses (AES-128
// CBC, PKCS7, then base64) and confirms aesDecryptPayload recovers the plaintext.
func TestAESDecryptRoundTrip(t *testing.T) {
	a, _ := generateDHKeyPair()
	b, _ := generateDHKeyPair()
	salt := "ivivivivivivivivXY"
	key, iv, err := deriveAESKeyIV(a.priv, base64.StdEncoding.EncodeToString(b.pub[:]), salt)
	if err != nil {
		t.Fatal(err)
	}

	plain := []byte(`{"type":1,"content":"hello 世界"}`)
	wire := encryptLikeServer(t, plain, key, iv)

	got, err := aesDecryptPayload(wire, key, iv)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if string(got) != string(plain) {
		t.Fatalf("round-trip mismatch:\n got %q\nwant %q", got, plain)
	}
}

// encryptLikeServer mirrors the server side of socket.ts aesDecrypt: PKCS7-pad,
// AES-128-CBC encrypt, then base64-encode the ciphertext (the on-wire form).
func encryptLikeServer(t *testing.T, plain, key, iv []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	padded := pkcs7Pad(plain, aes.BlockSize)
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, padded)
	return []byte(base64.StdEncoding.EncodeToString(ct))
}

func pkcs7Pad(b []byte, blockSize int) []byte {
	pad := blockSize - len(b)%blockSize
	out := make([]byte, len(b)+pad)
	copy(out, b)
	for i := len(b); i < len(out); i++ {
		out[i] = byte(pad)
	}
	return out
}
