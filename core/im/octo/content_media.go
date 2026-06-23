package octo

import (
	"net/url"
	"strconv"
	"strings"
)

// buildMediaURL resolves a relative storage path against the bot API base,
// hardened against absolute-URL smuggling and path traversal (inbound.ts
// buildMediaUrl, S1 + P1.2). Returns "" when the input is unsafe or empty so a
// malicious payload.url can never be fetched with the bot's Authorization
// header (token-leak chokepoint) nor forge a marker line.
//
// octobuddy has no separate CDN host config, so only the apiUrl host is allowed for
// absolute URLs; the relative-path branch is preserved verbatim.
func buildMediaURL(relURL, apiURL string) string {
	if relURL == "" {
		return ""
	}
	// Backslashes are a Windows-style traversal vector once normalized.
	if strings.Contains(relURL, "\\") {
		return ""
	}
	// Scheme-relative URL (`//attacker.com/path`).
	if strings.HasPrefix(relURL, "//") {
		return ""
	}

	if strings.HasPrefix(relURL, "http://") || strings.HasPrefix(relURL, "https://") {
		return buildAbsoluteMediaURL(relURL, apiURL)
	}
	return buildRelativeMediaURL(relURL, apiURL)
}

func buildAbsoluteMediaURL(relURL, apiURL string) string {
	if apiURL == "" {
		return ""
	}
	target, err := url.Parse(relURL)
	if err != nil {
		return ""
	}
	base, err := url.Parse(apiURL)
	if err != nil {
		return ""
	}
	if !isAllowedMediaOrigin(target, base) {
		return ""
	}
	return relURL
}

func isAllowedMediaOrigin(target, base *url.URL) bool {
	if !strings.EqualFold(target.Host, base.Host) {
		return false
	}
	// Same host: reject a protocol downgrade/upgrade mismatch.
	if !strings.EqualFold(target.Scheme, base.Scheme) {
		return false
	}
	return target.Scheme == "http" || target.Scheme == "https"
}

func buildRelativeMediaURL(relURL, apiURL string) string {
	// Relative path — strip the /file/ or /file/preview/ prefix, then enforce
	// no traversal. The percent-encoded-dot and %2F defenses mirror inbound.ts:
	// some servers decode them server-side and resolve dot-segments, escaping
	// the /file/ sandbox.
	storagePath := trimMediaStoragePrefix(relURL)
	if hasUnsafeMediaPath(storagePath) {
		return ""
	}
	lower := strings.ToLower(storagePath)
	if hasUnsafeMediaEncoding(lower) {
		return ""
	}

	baseURL := strings.TrimRight(apiURL, "/")
	candidate := baseURL + "/file/" + storagePath

	// WHATWG-canonical sandbox check: after normalization the path must still
	// be under /file/.
	normalized, err := url.Parse(candidate)
	if err != nil {
		return ""
	}
	if !strings.HasPrefix(normalized.Path, "/file/") {
		return ""
	}
	return candidate
}

func trimMediaStoragePrefix(storagePath string) string {
	switch {
	case strings.HasPrefix(storagePath, "file/preview/"):
		return storagePath[len("file/preview/"):]
	case strings.HasPrefix(storagePath, "file/"):
		return storagePath[len("file/"):]
	default:
		return storagePath
	}
}

func hasUnsafeMediaPath(storagePath string) bool {
	for _, seg := range strings.Split(storagePath, "/") {
		if seg == ".." || seg == "." {
			return true
		}
	}
	return strings.HasPrefix(storagePath, "/")
}

func hasUnsafeMediaEncoding(lowerStoragePath string) bool {
	if strings.Contains(lowerStoragePath, "%2f") {
		return true
	}
	if strings.Contains(lowerStoragePath, "%2e") {
		return true
	}
	// Reject an encoded percent (%25…) too: a downstream store that
	// double-decodes could turn %252e back into %2e and then "." — a traversal
	// the single-decode checks above would miss (L21).
	return strings.Contains(lowerStoragePath, "%25")
}

// toFiniteCoord coerces a user-supplied coordinate to a finite number, or
// reports ok=false (inbound.ts toFiniteCoord). Accepts only a real number
// (json float64) or a numeric string — rejects nil/bool/object so a non-numeric
// string can't forge a label and a nil can't render a bogus 0.
func toFiniteCoord(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		if isFinite(t) {
			return t, true
		}
	case int:
		return float64(t), true
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0, false
		}
		n, err := strconv.ParseFloat(s, 64)
		if err == nil && isFinite(n) {
			return n, true
		}
	}
	return 0, false
}

// isFinite reports whether f is a real finite number (not NaN or ±Inf).
func isFinite(f float64) bool {
	return f == f && f-f == 0
}

// formatCoord renders a coordinate without a trailing ".0" so an integer-valued
// float matches the JS `${lat}` template (which prints "31", not "31.0").
func formatCoord(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// truncateByBytes truncates s to at most maxBytes UTF-8 bytes on a rune
// boundary, appending marker when it had to cut (inbound.ts truncateByBytes).
func truncateByBytes(s string, maxBytes int, marker string) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	// Walk back to a rune boundary so we never split a multi-byte rune.
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + marker
}

// utf8RuneStart reports whether b is the first byte of a UTF-8 rune (i.e. not a
// 0b10xxxxxx continuation byte).
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }
