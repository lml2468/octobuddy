// Package envenc encrypts per-bot env-var values written to config.json so a
// leaked / screenshared / cloud-synced config file doesn't expose tokens in
// plain text. The threat model is config-file leakage (operator pastes
// config.json into a support ticket, Time Machine ships it off, iCloud Drive
// syncs ~/.xclaw, etc.) — NOT same-account adversaries, who have code
// execution as the operator and can dump /proc/<pid>/environ once the daemon
// decrypts a value into the spawned agent's env.
//
// Design choices, with rationale:
//
//   - AES-256-GCM via the stdlib (`crypto/aes` + `crypto/cipher`). 32-byte
//     keys, 12-byte nonces, 16-byte AEAD tag. No external dep.
//   - One master key per machine, generated at first use with `crypto/rand`
//     and stored at `~/.xclaw/master.key` mode 0o600 via atomic
//     tempfile+rename. Both daemon and desktop point at the same path; the
//     first caller wins the create race and everyone subsequently reads it.
//   - The ciphertext encoding is `enc:v1:<base64(nonce || ciphertext+tag)>`.
//     The `enc:v1:` prefix is the discriminator so plain-text values in
//     config.json continue to work unchanged — full backward compat with
//     pre-encryption configs. A future format bump rolls the version digit
//     (v1 → v2) without breaking the parser.
//   - Decrypt is the daemon's job (in `DriverEnv`). The GUI gets a
//     write-only EncryptSecret method so a compromised webview cannot
//     enumerate stored secrets — to change one, the user re-pastes; the
//     ciphertext is never round-tripped through the renderer.
//
// What this package deliberately doesn't do:
//
//   - Key rotation. Adding a v2 format with a re-encrypt-all migration is
//     ~20 lines but not yet needed; flagged as out of scope until rotation
//     is asked for.
//   - Per-secret access policy. Every encrypted value in a bot's env is
//     decryptable by the same master key — granular ACLs would need a new
//     wrapping scheme.
//   - Passphrase-derived KEK. The master key sits on disk because users
//     explicitly chose A over C in the design discussion: zero startup
//     friction trumps the marginal security gain over file 0o600.
package envenc

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// prefix tags a value as encrypted. Anything without it is treated as
// plaintext on decrypt (so old configs keep working) — DO NOT change without
// bumping the version digit, or every existing config silently misparses.
const prefix = "enc:v1:"

// keySize is the AES-256-GCM key length. master.key on disk MUST be exactly
// this many bytes; any other size is rejected as "not our file" rather than
// silently re-generated, because silently overwriting it would lose every
// previously encrypted secret in every bot's config.json.
const keySize = 32

// IsCiphertext reports whether s is in our enc:v1:… envelope. Cheap probe
// callers use to decide whether to attempt decryption — values without the
// prefix are valid plaintext and must pass through untouched.
func IsCiphertext(s string) bool { return strings.HasPrefix(s, prefix) }

// Encrypt seals plaintext under key and returns the `enc:v1:…` envelope.
// key MUST be keySize bytes (LoadOrCreateMaster guarantees this).
func Encrypt(key []byte, plaintext string) (string, error) {
	if len(key) != keySize {
		return "", fmt.Errorf("envenc: key must be %d bytes, got %d", keySize, len(key))
	}
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("envenc: nonce: %w", err)
	}
	// Seal(nonce, nonce, ...) prefixes the nonce to the output so the wire
	// format is self-describing: nonce || ciphertext || tag.
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return prefix + base64.RawStdEncoding.EncodeToString(sealed), nil
}

// Decrypt reverses Encrypt. A value without the `enc:v1:` prefix is returned
// unchanged (backward compat with plaintext configs). Tampered or wrong-key
// ciphertext returns ErrDecrypt — callers route this to fail-soft (drop the
// env entry, log the key) rather than aborting the turn.
func Decrypt(key []byte, s string) (string, error) {
	if !IsCiphertext(s) {
		return s, nil
	}
	if len(key) != keySize {
		return "", fmt.Errorf("envenc: key must be %d bytes, got %d", keySize, len(key))
	}
	sealed, err := base64.RawStdEncoding.DecodeString(s[len(prefix):])
	if err != nil {
		return "", fmt.Errorf("%w: base64: %v", ErrDecrypt, err)
	}
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	ns := gcm.NonceSize()
	if len(sealed) < ns+gcm.Overhead() {
		return "", fmt.Errorf("%w: ciphertext too short", ErrDecrypt)
	}
	nonce, ct := sealed[:ns], sealed[ns:]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrDecrypt, err)
	}
	return string(plain), nil
}

// ErrDecrypt is the sentinel callers wrap on for fail-soft policy
// (`errors.Is(err, ErrDecrypt)`). Returned for tampered ciphertext, wrong
// key, truncated envelopes — anything where AEAD authentication failed or
// the input was structurally invalid.
var ErrDecrypt = errors.New("envenc: decrypt failed")

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("envenc: aes init: %w", err)
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("envenc: gcm init: %w", err)
	}
	return g, nil
}

// LoadOrCreateMaster reads the master key at path, generating one with
// crypto/rand if the file doesn't exist. The new key is written via
// tempfile+rename so a crash mid-write never leaves a half-written key file
// on disk (which would be undecryptable and lose every secret).
//
// Wrong-size files are an error, not silently overwritten: if some other
// process / sync tool clobbered the file we want a loud failure, not a
// silent "all your secrets are gone now". A 0-byte file from a crashed
// writer is also an error.
//
// The parent dir is created with 0o700 if missing. The key file itself is
// 0o600. Both modes assume single-user ownership; multi-user setups are
// out of scope (XClaw is a desktop app).
func LoadOrCreateMaster(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		if len(data) != keySize {
			return nil, fmt.Errorf("envenc: master key at %s is %d bytes (expected %d) — file appears corrupted; refusing to overwrite", path, len(data), keySize)
		}
		return data, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("envenc: read master key %s: %w", path, err)
	}
	key := make([]byte, keySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("envenc: generate master key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("envenc: mkdir for master key: %w", err)
	}
	tmp := path + ".tmp"
	// O_EXCL so a stale .tmp from a prior crash forces us to retry by hand
	// rather than racing with whatever wrote it.
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("envenc: create tempfile %s: %w (delete a stale .tmp and retry)", tmp, err)
	}
	if _, err := f.Write(key); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("envenc: write tempfile: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("envenc: close tempfile: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("envenc: rename tempfile→master: %w", err)
	}
	return key, nil
}

// DefaultMasterPath is ~/.xclaw/master.key — the location both daemon and
// desktop agree on. Centralized so the two binaries don't drift if the path
// ever changes.
func DefaultMasterPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("envenc: resolve home: %w", err)
	}
	return filepath.Join(home, ".xclaw", "master.key"), nil
}
