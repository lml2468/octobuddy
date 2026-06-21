package config

import (
	"context"
	"fmt"
	"net"
	"net/url"
)

// IsAllowedURL implements the SSRF policy (url-policy.ts isAllowedApiUrl):
//   - https:// to any non-private host (private IP literals rejected)
//   - http:// only to a loopback host (localhost / 127.0.0.0-8 / ::1)
//   - any other scheme rejected
//
// This validates IP *literals* only — a hostname that resolves to a private or
// metadata IP at request time is NOT caught here (no DNS resolution / rebind
// protection). That is acceptable because these URLs are operator-trusted
// config (apiUrl / gatewayBaseUrl), never attacker-supplied; the check is a
// guardrail against fat-finger misconfig, not a defense against a hostile
// operator. If untrusted URLs ever flow here, add resolve-time IP re-checking.
func IsAllowedURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	switch u.Scheme {
	case "https":
		if ip := net.ParseIP(host); ip != nil && isPrivateOrLocal(ip) {
			return false
		}
		return host != ""
	case "http":
		// Local dev gateways only. Accept any loopback form, not just the
		// 127.0.0.1 literal (e.g. 127.0.0.2, ::1).
		if ip := net.ParseIP(host); ip != nil {
			return ip.IsLoopback()
		}
		return host == "localhost"
	default:
		return false
	}
}

// isPrivateOrLocal reports whether ip is loopback, private, link-local, CGN, or
// an unspecified address — the ranges url-policy.ts rejects.
func isPrivateOrLocal(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsPrivate() || ip.IsUnspecified() {
		return true
	}
	// Carrier-grade NAT 100.64.0.0/10 (not covered by IsPrivate).
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
			return true
		}
		if v4[0] == 0 { // 0.0.0.0/8
			return true
		}
	}
	return false
}

// IsPrivateOrLocalAddress reports whether the IP literal addr is in a
// private/loopback/link-local/CGN/unspecified range — the exported form of the
// SSRF range check (url-policy.ts isPrivateOrLocalAddress). A non-IP string
// returns false (the caller should DNS-resolve first). Used by the gateway media
// downloader, which handles attacker-supplied URLs and must re-check per hop.
func IsPrivateOrLocalAddress(addr string) bool {
	ip := net.ParseIP(addr)
	if ip == nil {
		return false
	}
	return isPrivateOrLocal(ip)
}

// AssertPublicURL validates that raw is an http(s) URL whose host is publicly
// routable, resolving DNS and rejecting if ANY resolved address is
// private/loopback/link-local (url-policy.ts assertPublicUrl). Unlike
// IsAllowedURL (operator-config, literal-only), this is the runtime guard for
// ATTACKER-SUPPLIED media URLs: an IP literal is checked directly; a hostname is
// resolved and every returned address must be public.
//
// DNS rebinding remains a residual risk (the resolver may return a different IP
// at dial time); the redirect-following downloader re-checks each hop, matching
// the TS fetchWithRedirectGuard contract.
func AssertPublicURL(ctx context.Context, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("refusing non-http(s) URL: %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("url has no host")
	}
	// IP literal — check directly, no DNS.
	if ip := net.ParseIP(host); ip != nil {
		if isPrivateOrLocal(ip) {
			return fmt.Errorf("refusing private/local address: %s", host)
		}
		return nil
	}
	// Hostname — resolve and reject if ANY address is private/local.
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("resolve %s: no addresses", host)
	}
	for _, a := range addrs {
		if isPrivateOrLocal(a.IP) {
			return fmt.Errorf("refusing %s: resolves to private/local address %s", host, a.IP)
		}
	}
	return nil
}
