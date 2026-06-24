// Package secrets stores per-bot secrets behind a small backend abstraction.
// The OS credential store is preferred; a local 0600 file backend is the
// writable headless fallback; env vars are the read-only CI fallback. Secret
// values never touch config.json; the bridge reads them here and injects them
// into the daemon at runtime over the control bus (secret.inject).
package secrets

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/zalando/go-keyring"

	"github.com/lml2468/octobuddy/core/control/wire"
	"github.com/lml2468/octobuddy/core/safepath"
)

// service is the credential-store service name (shared with the legacy Swift
// app so existing Keychain entries carry over on macOS).
const service = "com.mlt.octobuddy.tokens"

// Kind is a token category. The account key is "<botID>/<kind>". Aliased to
// wire.SecretKind so the desktop, the control bus, and the daemon all use one
// canonical type (was a separate `type Kind string` that duplicated wire's
// string literals).
type Kind = wire.SecretKind

const (
	OctoToken    = wire.SecretKindOcto
	GatewayToken = wire.SecretKindGateway
)

// account returns the per-(botID,kind) keyring account key. It refuses any
// botID that fails safepath.ValidSlug — without this, a caller passing an
// attacker-supplied id like "../other" would write/read another bot's
// credential namespace. configstore.Save validates first, but other callers
// (control-bus secret.inject handler, future tooling) must not have to
// re-derive this fence; validating here keeps the trust boundary local to
// the package that mints the key.
func account(botID string, kind Kind) (string, error) {
	if !safepath.ValidSlug(botID) {
		return "", fmt.Errorf("invalid bot id %q", botID)
	}
	return botID + "/" + string(kind), nil
}

type backend interface {
	Get(botID string, kind Kind) (string, error)
	Set(botID string, kind Kind, value string) error
	Delete(botID string, kind Kind) error
	// Writable reports whether Set/Delete can ever succeed. envBackend
	// returns false; the cross-backend write-through in Set/Delete uses
	// this to ignore the read-only error from envBackend instead of
	// treating it as a real failure.
	Writable() bool
}

type keyringBackend struct{}

func (keyringBackend) Get(botID string, kind Kind) (string, error) {
	acct, err := account(botID, kind)
	if err != nil {
		return "", err
	}
	return keyring.Get(service, acct)
}

func (keyringBackend) Set(botID string, kind Kind, value string) error {
	acct, err := account(botID, kind)
	if err != nil {
		return err
	}
	return keyring.Set(service, acct, value)
}

func (keyringBackend) Delete(botID string, kind Kind) error {
	acct, err := account(botID, kind)
	if err != nil {
		return err
	}
	err = keyring.Delete(service, acct)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}

func (keyringBackend) Writable() bool { return true }

// fileBackend is the headless fallback when an OS credential store is
// unavailable. It is intentionally simple and local-user scoped (0600 files);
// desktop builds still prefer the OS backend whenever it works.
type fileBackend struct{}

func secretFile(botID string, kind Kind) (string, error) {
	if !safepath.ValidSlug(botID) {
		return "", fmt.Errorf("invalid bot id %q", botID)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	name := base64.RawURLEncoding.EncodeToString([]byte(kind))
	return filepath.Join(home, ".octobuddy", "secrets", botID, name), nil
}

func (fileBackend) Get(botID string, kind Kind) (string, error) {
	path, err := secretFile(botID, kind)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func (fileBackend) Set(botID string, kind Kind, value string) error {
	path, err := secretFile(botID, kind)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(value), 0o600)
}

func (fileBackend) Delete(botID string, kind Kind) error {
	path, err := secretFile(botID, kind)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (fileBackend) Writable() bool { return true }

type envBackend struct{}

func secretEnvName(botID string, kind Kind) string {
	var b strings.Builder
	b.WriteString("OCTOBUDDY_SECRET_")
	for _, r := range botID + "_" + string(kind) {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(unicode.ToUpper(r))
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func (envBackend) Get(botID string, kind Kind) (string, error) {
	if !safepath.ValidSlug(botID) {
		return "", fmt.Errorf("invalid bot id %q", botID)
	}
	v, ok := os.LookupEnv(secretEnvName(botID, kind))
	if !ok {
		return "", os.ErrNotExist
	}
	return v, nil
}

func (envBackend) Set(string, Kind, string) error { return errors.New("env backend is read-only") }

func (envBackend) Delete(string, Kind) error { return errors.New("env backend is read-only") }

func (envBackend) Writable() bool { return false }

var backends = []backend{keyringBackend{}, fileBackend{}, envBackend{}}

// Get returns the stored token, or "" if none is set. A "not found" result and a
// real backend failure both yield "" (callers treat that as "no token to
// inject"), but real failures are logged so they aren't silently
// indistinguishable from "unset".
func Get(botID string, kind Kind) string {
	var last error
	for _, b := range backends {
		v, err := b.Get(botID, kind)
		if err == nil {
			return v
		}
		if !errors.Is(err, keyring.ErrNotFound) && !os.IsNotExist(err) {
			last = err
		}
	}
	if last != nil {
		fmt.Fprintf(os.Stderr, "[secrets] get failed for %s/%s: %v\n", botID, kind, last)
	}
	return ""
}

// Set stores (or, with an empty value, deletes) a token. Writes through
// to EVERY writable backend so a backend whose availability later toggles
// (keyring access revoked/granted mid-session) can't surface a stale
// copy via Get's read-precedence walk. envBackend is read-only and its
// "not supported" error is not counted as a failure.
func Set(botID string, kind Kind, value string) error {
	if value == "" {
		return Delete(botID, kind)
	}
	var last error
	any := false
	for _, b := range backends {
		err := b.Set(botID, kind, value)
		if err == nil {
			any = true
			continue
		}
		if !b.Writable() {
			continue
		}
		last = err
	}
	if any {
		return nil
	}
	return last
}

// Delete removes a token from every writable backend. Returns nil if
// any backend succeeded; missing entries are not errors. envBackend's
// read-only error is not counted as a failure.
func Delete(botID string, kind Kind) error {
	var last error
	any := false
	for _, b := range backends {
		err := b.Delete(botID, kind)
		if err == nil {
			any = true
			continue
		}
		if !b.Writable() {
			continue
		}
		last = err
	}
	if any {
		return nil
	}
	return last
}
