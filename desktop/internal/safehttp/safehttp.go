// Package safehttp builds HTTP clients with a dial-time guard against
// SSRF / DNS rebinding. The guard refuses TCP connects to private/
// loopback/link-local/CGN addresses (per core/config's allowlist), so a
// poisoned DNS or compromised mirror cannot redirect a public URL to
// 169.254.169.254 (cloud metadata) or an internal address. Used by
// every outbound HTTP from the desktop helper (octocli download +
// octoapi wizard provisioning); promoted from the per-package copies
// that lived in octocli and octoapi.
//
// Two policies: strict (refuse all private/local — for download CDNs
// that should never reach a private network) and loopback-tolerant
// (allow 127.0.0.1 / ::1 when the operator configured an http://
// localhost endpoint for dev — for the wizard's API URL).
package safehttp

import (
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"

	"github.com/lml2468/octobuddy/core/config"
)

// Options controls the dial guard's policy.
type Options struct {
	// AllowLoopback permits 127.0.0.1 / ::1 connects (otherwise refused
	// like any other private address). Set when the caller has knowingly
	// pointed at a localhost dev endpoint and the URL scheme is http://.
	AllowLoopback bool
	// Tag is the package name used in error messages so an operator can
	// tell which client refused a connect ("octocli dial: ..." vs
	// "octoapi dial: ...").
	Tag string
}

// NewClient returns an http.Client whose dialer refuses connects per opts.
// Pool sizing + timeouts mirror the prior per-package copies.
func NewClient(opts Options) *http.Client {
	if opts.Tag == "" {
		opts.Tag = "safehttp"
	}
	dial := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   guard(opts),
	}
	return &http.Client{
		Transport: &http.Transport{
			DialContext:           dial.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          8,
			MaxConnsPerHost:       4,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

// Guard returns the dialer Control func for callers that want to build
// their own http.Transport with custom sizing (octoapi sizes its pool
// down for the one-shot wizard call).
func Guard(opts Options) func(network, address string, _ syscall.RawConn) error {
	return guard(opts)
}

func guard(opts Options) func(network, address string, _ syscall.RawConn) error {
	tag := opts.Tag
	return func(_ string, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("%s dial: bad address %q: %w", tag, address, err)
		}
		// AllowLoopback honors all RFC1122 loopback (127.0.0.0/8 + ::1),
		// not just literal 127.0.0.1 — devs running a local octo-server
		// on 127.0.0.2 (multi-instance dev pattern) would otherwise see
		// an SSRF refusal for a legitimate loopback target.
		if opts.AllowLoopback {
			if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
				return nil
			}
		}
		if config.IsPrivateOrLocalAddress(host) {
			return fmt.Errorf("%s dial: refusing private/local address %s", tag, host)
		}
		return nil
	}
}
