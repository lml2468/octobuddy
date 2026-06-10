package config

import (
	"net"
	"net/url"
)

// isAllowedURL implements the SSRF policy (url-policy.ts isAllowedApiUrl):
//   - https:// to any non-private host (private IP literals rejected)
//   - http:// only to localhost / 127.0.0.1 / [::1]
//   - any other scheme rejected
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
		return host == "localhost" || host == "127.0.0.1" || host == "::1"
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
