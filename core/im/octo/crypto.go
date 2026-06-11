package octo

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"

	"golang.org/x/crypto/curve25519"
)

// Crypto handshake, wire-compatible with cc-channel-octo (socket.ts):
//   - curve25519 keypair (seed clamped exactly like curve25519-js)
//   - DH shared secret = X25519(clientPriv, serverPub)
//   - AES-128 key = first 16 ASCII chars of lowercase-hex MD5(base64(secret))
//   - IV = first 16 bytes of the salt string
//   - RECV payload = base64 text of an AES-128-CBC (PKCS7) ciphertext
//
// Security note: this scheme (CBC, deterministic IV, no MAC/AEAD) is dictated by
// the WuKongIM wire protocol and matched byte-for-byte for interop — we cannot
// add authentication without breaking compatibility, and this is decrypt-only.
// Inbound IM frames therefore have NO integrity guarantee at this layer; the
// gateway treats all inbound IM text as untrusted regardless and fences it via
// core/safety + core/groupctx before it reaches the agent. Do not rely on the
// transport for authenticity.

// dhKeyPair holds a clamped curve25519 keypair.
type dhKeyPair struct {
	priv [32]byte
	pub  [32]byte
}

// generateDHKeyPair makes a keypair matching curve25519-js generateKeyPair:
// a 32-byte random seed, clamped, then public = X25519(priv, basepoint).
func generateDHKeyPair() (dhKeyPair, error) {
	var kp dhKeyPair
	if _, err := rand.Read(kp.priv[:]); err != nil {
		return dhKeyPair{}, err
	}
	clampScalar(&kp.priv)
	pub, err := curve25519.X25519(kp.priv[:], curve25519.Basepoint)
	if err != nil {
		return dhKeyPair{}, err
	}
	copy(kp.pub[:], pub)
	return kp, nil
}

// clampScalar replicates curve25519-js seed clamping.
func clampScalar(k *[32]byte) {
	k[0] &= 248
	k[31] &= 127
	k[31] |= 64
}

// pubKeyBase64 returns the standard-base64 client public key (sent in CONNECT).
func (kp dhKeyPair) pubKeyBase64() string {
	return base64.StdEncoding.EncodeToString(kp.pub[:])
}

// deriveAESKeyIV computes the AES-128 key and IV from the server's base64
// public key and salt string. See the steps above; the key is the first 16
// ASCII chars of the hex MD5 of the base64-encoded shared secret (NOT the raw
// digest bytes), and the IV is the first 16 bytes of the salt.
func deriveAESKeyIV(priv [32]byte, serverKeyB64, salt string) (key, iv []byte, err error) {
	serverPub, err := base64.StdEncoding.DecodeString(serverKeyB64)
	if err != nil {
		return nil, nil, fmt.Errorf("decode server key: %w", err)
	}
	secret, err := curve25519.X25519(priv[:], serverPub)
	if err != nil {
		return nil, nil, fmt.Errorf("x25519: %w", err)
	}
	secretB64 := base64.StdEncoding.EncodeToString(secret)
	sum := md5.Sum([]byte(secretB64))
	hexStr := hex.EncodeToString(sum[:]) // 32 lowercase hex chars
	key = []byte(hexStr[:16])            // first 16 ASCII chars = 16 bytes

	saltBytes := []byte(salt)
	if len(saltBytes) < 16 {
		return nil, nil, fmt.Errorf("salt too short (%d < 16)", len(saltBytes))
	}
	iv = saltBytes[:16]
	return key, iv, nil
}

// aesDecryptPayload decrypts a RECV payload: the raw bytes are ASCII base64 of
// the AES-128-CBC ciphertext (socket.ts aesDecrypt).
func aesDecryptPayload(payload, key, iv []byte) ([]byte, error) {
	cipherBytes, err := base64.StdEncoding.DecodeString(string(payload))
	if err != nil {
		return nil, fmt.Errorf("base64 payload: %w", err)
	}
	if len(cipherBytes) == 0 || len(cipherBytes)%aes.BlockSize != 0 {
		return nil, errors.New("ciphertext not a block multiple")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	mode := cipher.NewCBCDecrypter(block, iv)
	plain := make([]byte, len(cipherBytes))
	mode.CryptBlocks(plain, cipherBytes)
	return pkcs7Unpad(plain, aes.BlockSize)
}

func pkcs7Unpad(b []byte, blockSize int) ([]byte, error) {
	n := len(b)
	if n == 0 {
		return nil, errors.New("empty plaintext")
	}
	pad := int(b[n-1])
	if pad == 0 || pad > blockSize || pad > n {
		return nil, errors.New("invalid pkcs7 padding")
	}
	for _, c := range b[n-pad:] {
		if int(c) != pad {
			return nil, errors.New("invalid pkcs7 padding bytes")
		}
	}
	return b[:n-pad], nil
}
