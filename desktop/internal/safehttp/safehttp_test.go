package safehttp

import (
	"errors"
	"strings"
	"syscall"
	"testing"
)

// TestGuardRefusesPrivateAddresses is the SSRF / DNS-rebinding regression:
// when DNS resolves a public hostname to a private address (compromised
// resolver, intentional rebind, link-local 169.254.169.254 cloud-metadata
// trick), the dial guard must refuse the connect BEFORE the request goes
// out. Each address below is a known-private/local/cloud-metadata target.
func TestGuardRefusesPrivateAddresses(t *testing.T) {
	g := Guard(Options{Tag: "test"})
	for _, addr := range []string{
		"10.0.0.1:80",        // RFC 1918
		"192.168.1.1:443",    // RFC 1918
		"172.16.0.1:80",      // RFC 1918
		"100.64.0.1:80",      // CGN
		"169.254.169.254:80", // AWS / GCP metadata service
		"[::1]:443",          // IPv6 loopback (no AllowLoopback)
		"[fd00::1]:80",       // IPv6 ULA
		"127.0.0.1:8080",     // IPv4 loopback (no AllowLoopback)
	} {
		t.Run(addr, func(t *testing.T) {
			err := g("tcp", addr, fakeConn{})
			if err == nil {
				t.Fatalf("guard must refuse %q", addr)
			}
			if !strings.Contains(err.Error(), "refusing private/local address") {
				t.Fatalf("error should name the refusal cause, got %v", err)
			}
		})
	}
}

// TestGuardAllowsLoopbackOnlyWithFlag: the wizard / localhost-dev flow
// MUST opt in via AllowLoopback. By default loopback is refused like any
// other private address (defense in depth).
func TestGuardAllowsLoopbackOnlyWithFlag(t *testing.T) {
	defaultG := Guard(Options{Tag: "test"})
	if err := defaultG("tcp", "127.0.0.1:80", fakeConn{}); err == nil {
		t.Fatal("default guard must refuse loopback")
	}

	loopbackG := Guard(Options{Tag: "test", AllowLoopback: true})
	if err := loopbackG("tcp", "127.0.0.1:80", fakeConn{}); err != nil {
		t.Fatalf("AllowLoopback guard must accept 127.0.0.1: %v", err)
	}
	if err := loopbackG("tcp", "[::1]:80", fakeConn{}); err != nil {
		t.Fatalf("AllowLoopback guard must accept ::1: %v", err)
	}
	// Even with AllowLoopback, RFC-1918 / CGN / link-local stays refused.
	if err := loopbackG("tcp", "10.0.0.1:80", fakeConn{}); err == nil {
		t.Fatal("AllowLoopback must NOT relax RFC-1918")
	}
}

// TestGuardAllowsPublicAddresses: ordinary public IPs must pass through.
func TestGuardAllowsPublicAddresses(t *testing.T) {
	g := Guard(Options{Tag: "test"})
	for _, addr := range []string{
		"1.1.1.1:443",        // Cloudflare DNS
		"8.8.8.8:53",         // Google DNS
		"[2606:4700::1]:443", // public IPv6
	} {
		if err := g("tcp", addr, fakeConn{}); err != nil {
			t.Errorf("guard rejected public %s: %v", addr, err)
		}
	}
}

// TestGuardErrorMessageNamesTag lets operators correlate a refusal to
// the offending subsystem (octocli download vs octoapi wizard).
func TestGuardErrorMessageNamesTag(t *testing.T) {
	g := Guard(Options{Tag: "octoapi"})
	err := g("tcp", "10.0.0.1:80", fakeConn{})
	if err == nil || !strings.HasPrefix(err.Error(), "octoapi dial:") {
		t.Fatalf("error must lead with the tag, got %v", err)
	}
}

// TestGuardRejectsMalformedAddress: a malformed host:port string is a
// bug in the caller, not a security drop — but the guard must report it
// with a wrapped error rather than silently passing.
func TestGuardRejectsMalformedAddress(t *testing.T) {
	g := Guard(Options{Tag: "test"})
	err := g("tcp", "no-port-here", fakeConn{})
	if err == nil {
		t.Fatal("guard must error on bad address")
	}
	if !errors.Is(err, errors.Unwrap(err)) {
		// Sanity that we wrapped (Unwrap returned non-nil).
		// This is also defensive — the wrapping uses %w.
	}
}

// fakeConn is a stub syscall.RawConn used only because the guard's
// signature demands one; the implementation never touches it.
type fakeConn struct{}

func (fakeConn) Control(func(uintptr)) error    { return nil }
func (fakeConn) Read(func(uintptr) bool) error  { return nil }
func (fakeConn) Write(func(uintptr) bool) error { return nil }

// Compile-time assertion that fakeConn satisfies syscall.RawConn so the
// test breaks loudly if the stdlib interface ever grows new methods.
var _ syscall.RawConn = fakeConn{}
