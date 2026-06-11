package config

import (
	"net"
	"net/url"
)

// isAllowedURL implements the SSRF policy (url-policy.ts isAllowedApiUrl):
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
func isAllowedURL(raw string) bool {
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
