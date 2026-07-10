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
// only changes the user-facing wording, never correctness. The usage-cap
// alternatives are anchored to their full phrasing ("out of extra usage", not a
// bare "extra usage") so ordinary model prose mentioning those words doesn't trip.
var transientUpstreamRE = regexp.MustCompile(`(?i)(rate[-\s]?limit(ed)?|rate_limit_error|too many requests|\b429\b|overloaded(_error)?|server overloaded|service unavailable|\b503\b|\b529\b|high demand|try again later|temporarily unavailable|throttl(ed|ing)|throttlingexception|servicequotaexceededexception|out of extra usage|usage limit reached|usage cap reached|5[-\s]?hour limit reached|weekly limit reached)`)

// retryResetRE pulls a human-readable reset window out of a usage-limit
// message ("…usage limit reached, resets at 3pm (PST)"). Group 1 is the time
// phrase; we keep it verbatim for the reply rather than computing a timestamp.
var retryResetRE = regexp.MustCompile(`(?i)(?:usage (?:limit|cap) reached|5[-\s]?hour limit reached|weekly limit reached|out of extra usage)[\s\S]{0,80}?\bresets?\s+(?:at\s+)?([^\n().]+(?:\([^)]+\))?)`)

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

// poisonedResumeRE matches terminal errors whose cause is baked into the
// resumable conversation and re-fails identically on resume: malformed-request
// 400s, iteration/turn-limit exhaustion, explicit give-up markers. Kept narrow
// (unlike transientUpstreamRE) — a false positive here silently discards a
// session's resume continuity, so it must only match unambiguous dead-end
// signatures. The turn/iteration alternative allows a "_" or " " separator so it
// matches BOTH Claude's structured subtype ("error_max_turns" → surfaced as
// "…subtype=error_max_turns…") and its prose ("hit max turns"); the trailing
// "reached/exceeded" is optional for the same reason.
var poisonedResumeRE = regexp.MustCompile(`(?i)(invalid_request_error|\binvalid request\b|\berror_max_turns\b|max(imum)?[\s_]+(turns|iterations)|iteration limit|reached the maximum number of|gave up|giving up)`)

// isPoisonedResume reports whether s describes a deterministically-reproducing
// resume failure (baked into history), as opposed to a transient upstream blip.
// Transient always wins: a 429/503/overload is retryable, never poison.
func isPoisonedResume(s string) bool {
	if isTransientUpstream(s) {
		return false
	}
	return poisonedResumeRE.MatchString(s)
}

// tagPoisonedResume marks the terminal, non-transient, poison-shaped errors in a
// parsed event batch as Poisoned — but ONLY when the turn carried a resume id
// (sessionID != ""). A driver's Query wraps its lineParser with this so the
// "fresh turns have no poisonable history" guard is structural: a genuine 400 on
// a brand-new session is left as an ordinary terminal error (→ errorReply),
// never swallowed into the resume-retry path. Recoverable (mid-turn),
// ResumeInvalid, and transient errors are left untouched.
func tagPoisonedResume(evs []AgentEvent, sessionID string) []AgentEvent {
	if sessionID == "" {
		return evs
	}
	for i := range evs {
		ev := &evs[i]
		if ev.Kind != KindError || ev.Recoverable || ev.Transient || ev.ResumeInvalid {
			continue
		}
		if isPoisonedResume(ev.Err) {
			ev.Poisoned = true
		}
	}
	return evs
}
