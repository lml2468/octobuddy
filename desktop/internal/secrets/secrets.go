// Package secrets stores bot tokens in the OS credential store (macOS Keychain,
// Windows Credential Manager, Linux Secret Service) via go-keyring — zero cgo,
// cross-platform. Tokens never touch config.json; the bridge reads them here and
// injects them into the daemon at runtime over the control bus (secret.inject).
package secrets

import (
	"errors"
	"fmt"
	"os"

	"github.com/zalando/go-keyring"

	"github.com/lml2468/xclaw/desktop/internal/safepath"
)

// service is the credential-store service name (shared with the legacy Swift
// app so existing Keychain entries carry over on macOS).
const service = "com.xclaw.tokens"

// Kind is a token category. The account key is "<botID>/<kind>".
type Kind string

const (
	OctoToken    Kind = "octoToken"
	GatewayToken Kind = "gatewayToken"
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

// Get returns the stored token, or "" if none is set. A "not found" result and a
// real keyring failure both yield "" (callers treat that as "no token to
// inject"), but a real failure (keychain access denied, service unavailable) is
// logged so it isn't silently indistinguishable from "unset" — that case is the
// common confusion after a re-signed binary prompts and is denied (L).
func Get(botID string, kind Kind) string {
	acct, err := account(botID, kind)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[secrets] %v\n", err)
		return ""
	}
	v, err := keyring.Get(service, acct)
	if err != nil {
		if !errors.Is(err, keyring.ErrNotFound) {
			fmt.Fprintf(os.Stderr, "[secrets] keyring get failed for %s/%s: %v\n", botID, kind, err)
		}
		return ""
	}
	return v
}

// Set stores (or, with an empty value, deletes) a token.
func Set(botID string, kind Kind, value string) error {
	if value == "" {
		return Delete(botID, kind)
	}
	acct, err := account(botID, kind)
	if err != nil {
		return err
	}
	return keyring.Set(service, acct, value)
}

// Delete removes a token; a missing entry is not an error.
func Delete(botID string, kind Kind) error {
	acct, err := account(botID, kind)
	if err != nil {
		return err
	}
	err = keyring.Delete(service, acct)
	if err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return err
	}
	return nil
}
