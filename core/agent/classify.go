package agent

import "regexp"

// Error classification for Claude transport failures, ported from
// cc-channel's claude-local parse.ts (isClaudeTransientUpstreamError /
// extractClaudeRetryNotBefore). The gateway treats a "transient" terminal
// error as "服务繁忙，稍后重试" rather than a turn bug, and surfaces any reset
// window the CLI reported.

// transientUpstreamRE matches the upstream conditions that warrant a
// retry-later reply: provider rate-limit / overload (HTTP 429/503/529) and
// account usage-cap exhaustion. Kept deliberately broad — a false positive
// only changes the user-facing wording, never correctness.
var transientUpstreamRE = regexp.MustCompile(`(?i)(rate[-\s]?limit(ed)?|rate_limit_error|too many requests|\b429\b|overloaded(_error)?|server overloaded|service unavailable|\b503\b|\b529\b|high demand|try again later|temporarily unavailable|throttl(ed|ing)|throttlingexception|servicequotaexceededexception|out of extra usage|extra usage|usage limit reached|usage cap reached|5[-\s]?hour limit reached|weekly limit reached)`)

// retryResetRE pulls a human-readable reset window out of a usage-limit
// message ("…usage limit reached, resets at 3pm (PST)"). Group 1 is the time
// phrase; we keep it verbatim for the reply rather than computing a timestamp.
var retryResetRE = regexp.MustCompile(`(?i)(?:usage (?:limit|cap) reached|5[-\s]?hour limit reached|weekly limit reached|out of extra usage|extra usage)[\s\S]{0,80}?\bresets?\s+(?:at\s+)?([^\n().]+(?:\([^)]+\))?)`)

// isTransientUpstream reports whether s describes an upstream rate-limit /
// overload / usage-cap condition.
func isTransientUpstream(s string) bool {
	return transientUpstreamRE.MatchString(s)
}

// retryHint returns the reset-window phrase from s ("3pm (PST)"), or "" when
// the message carries none.
func retryHint(s string) string {
	m := retryResetRE.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	// Trim trailing punctuation/space the loose capture may include.
	hint := m[1]
	for len(hint) > 0 {
		c := hint[len(hint)-1]
		if c == ' ' || c == ',' || c == '.' || c == '!' {
			hint = hint[:len(hint)-1]
			continue
		}
		break
	}
	return hint
}
