// Package secrets stores bot tokens in the OS credential store (macOS Keychain,
// Windows Credential Manager, Linux Secret Service) via go-keyring — zero cgo,
// cross-platform. Tokens never touch config.json; the bridge reads them here and
// injects them into the daemon at runtime over the control bus (secret.inject).
package secrets

import (
	"errors"

	"github.com/zalando/go-keyring"
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

func account(botID string, kind Kind) string { return botID + "/" + string(kind) }

// Get returns the stored token, or "" if none is set.
func Get(botID string, kind Kind) string {
	v, err := keyring.Get(service, account(botID, kind))
	if err != nil {
		return ""
	}
	return v
}

// Set stores (or, with an empty value, deletes) a token.
func Set(botID string, kind Kind, value string) error {
	if value == "" {
		return Delete(botID, kind)
	}
	return keyring.Set(service, account(botID, kind), value)
}

// Delete removes a token; a missing entry is not an error.
func Delete(botID string, kind Kind) error {
	err := keyring.Delete(service, account(botID, kind))
	if err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return err
	}
	return nil
}
