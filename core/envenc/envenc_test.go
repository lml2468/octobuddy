package envenc

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEncryptDecryptRoundtrip(t *testing.T) {
	key := mustKey(t)
	for _, plaintext := range []string{
		"",
		"sk-ant-abc",
		"a token with spaces and !@#$%^&* punctuation",
		strings.Repeat("x", 10_000), // long secret (npm token etc.)
		"\x00\x01\x02 binary",
	} {
		ct, err := Encrypt(key, plaintext)
		if err != nil {
			t.Fatalf("encrypt %q: %v", plaintext, err)
		}
		if !IsCiphertext(ct) {
			t.Fatalf("encrypted value not tagged: %s", ct)
		}
		pt, err := Decrypt(key, ct)
		if err != nil {
			t.Fatalf("decrypt %q: %v", ct, err)
		}
		if pt != plaintext {
			t.Fatalf("roundtrip: got %q, want %q", pt, plaintext)
		}
	}
}

func TestEncryptProducesDifferentCiphertextEachCall(t *testing.T) {
	// Same plaintext + same key MUST encrypt to different ciphertexts each
	// time (fresh nonce). Otherwise an adversary diffing two config.json
	// versions could trivially detect "token unchanged" / "token rotated".
	key := mustKey(t)
	a, _ := Encrypt(key, "samesame")
	b, _ := Encrypt(key, "samesame")
	if a == b {
		t.Fatal("two encryptions produced identical ciphertext — nonce is not random")
	}
}

func TestDecryptPlaintextPassthrough(t *testing.T) {
	// Backward compat: a value without the enc:v1: prefix must round-trip
	// unchanged so existing config.json files keep loading.
	key := mustKey(t)
	for _, plain := range []string{"", "plain string", "ghp_NotEncrypted"} {
		got, err := Decrypt(key, plain)
		if err != nil {
			t.Fatalf("plaintext %q rejected: %v", plain, err)
		}
		if got != plain {
			t.Fatalf("plaintext %q changed to %q", plain, got)
		}
	}
}

func TestDecryptWrongKeyFails(t *testing.T) {
	// Rotating master.key (or copying a config from another machine) must
	// produce ErrDecrypt — never silently return garbage.
	k1, k2 := mustKey(t), mustKey(t)
	ct, _ := Encrypt(k1, "secret")
	_, err := Decrypt(k2, ct)
	if !errors.Is(err, ErrDecrypt) {
		t.Fatalf("wrong-key decrypt should return ErrDecrypt, got %v", err)
	}
}

func TestDecryptTamperFails(t *testing.T) {
	// AEAD tag must reject any bit-flip. Without this guarantee an attacker
	// who can edit config.json (but doesn't have the key) could flip plaintext
	// bytes silently.
	key := mustKey(t)
	ct, _ := Encrypt(key, "original-value")
	body := ct[len(prefix):]
	tampered := prefix + body[:len(body)-2] + "AA" // mutate the last bytes (likely tag region)
	if _, err := Decrypt(key, tampered); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("tampered ciphertext should return ErrDecrypt, got %v", err)
	}
}

func TestDecryptTruncatedFails(t *testing.T) {
	key := mustKey(t)
	if _, err := Decrypt(key, prefix); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("empty body should be ErrDecrypt, got %v", err)
	}
	if _, err := Decrypt(key, prefix+"AAA"); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("too-short body should be ErrDecrypt, got %v", err)
	}
}

func TestDecryptInvalidBase64Fails(t *testing.T) {
	key := mustKey(t)
	if _, err := Decrypt(key, prefix+"!!!not-base64!!!"); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("bad base64 should be ErrDecrypt, got %v", err)
	}
}

func TestWrongKeySize(t *testing.T) {
	if _, err := Encrypt([]byte("short"), "x"); err == nil {
		t.Fatal("short key encrypt must error")
	}
	if _, err := Decrypt([]byte("short"), prefix+"AAAA"); err == nil {
		t.Fatal("short key decrypt must error")
	}
}

func TestLoadOrCreateMasterGeneratesAndPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")

	// First call generates.
	k1, err := LoadOrCreateMaster(path)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if len(k1) != keySize {
		t.Fatalf("generated key size = %d, want %d", len(k1), keySize)
	}

	// Mode 0600.
	fi, _ := os.Stat(path)
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("master.key perm = %o, want 0600", perm)
	}

	// Second call reads same key (stable across restarts).
	k2, err := LoadOrCreateMaster(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !bytes.Equal(k1, k2) {
		t.Fatal("reload produced a different key — would break all prior encryptions")
	}
}

func TestLoadOrCreateMasterRejectsWrongSize(t *testing.T) {
	// A corrupted / partial file (or a hand-pasted wrong value) must error
	// rather than silently regenerate — regeneration would orphan every
	// existing encrypted secret in every config.json.
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	if err := os.WriteFile(path, []byte("too short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateMaster(path); err == nil {
		t.Fatal("wrong-size master.key must return an error, not silently regenerate")
	}
}

func TestLoadOrCreateMasterCreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "subdirs", "master.key")
	if _, err := LoadOrCreateMaster(path); err != nil {
		t.Fatalf("missing parent dir should auto-mkdir: %v", err)
	}
	pdir := filepath.Dir(path)
	if fi, err := os.Stat(pdir); err != nil {
		t.Fatalf("parent dir not created: %v", err)
	} else if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Fatalf("parent dir perm = %o, want 0700", perm)
	}
}

func TestLoadOrCreateMasterRejectsStaleTempFile(t *testing.T) {
	// O_EXCL guarantees we never race against a stale tempfile from a
	// previous crashed write. If one exists we want a clear error so the
	// operator can clean it up, not a silent overwrite of unknown content.
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	stale := path + ".tmp"
	if err := os.WriteFile(stale, []byte("stale junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateMaster(path); err == nil {
		t.Fatal("stale .tmp should block generation")
	}
}

func mustKey(t *testing.T) []byte {
	t.Helper()
	k, err := LoadOrCreateMaster(filepath.Join(t.TempDir(), "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	return k
}
