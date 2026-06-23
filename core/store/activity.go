package store

import (
	"database/sql"
	"fmt"
	"time"
)

// --- token usage (per-day buckets, per-bot store) ---

// TokenUsage is a token-accounting total over some range of days (zero value =
// no usage recorded in that range).
type TokenUsage struct {
	InputTokens      int64
	OutputTokens     int64
	CachedTokens     int64 // cache reads (cache_read_input_tokens)
	CacheWriteTokens int64 // cache writes (cache_creation_input_tokens)
	CostUSD          float64
	Turns            int64
}

// localMidnight returns the Unix-seconds timestamp of the most recent local
// midnight at or before t — the key for t's day bucket.
func localMidnight(t time.Time) int64 {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location()).Unix()
}

// AddUsage accumulates one completed turn's usage into today's bucket. A no-op
// when all deltas are zero (a turn the agent reported no usage for), so the turn
// counter only advances on real usage.
func (s *Store) AddUsage(in, out, cached, cacheWrite int, cost float64) error {
	if in == 0 && out == 0 && cached == 0 && cacheWrite == 0 && cost == 0 {
		return nil
	}
	day := localMidnight(s.now())
	_, err := s.db.Exec(
		`INSERT INTO token_usage_daily(day, input_tokens, output_tokens, cached_tokens, cache_write_tokens, cost_usd, turns)
		 VALUES(?, ?, ?, ?, ?, ?, 1)
		 ON CONFLICT(day) DO UPDATE SET
		   input_tokens       = input_tokens       + excluded.input_tokens,
		   output_tokens      = output_tokens      + excluded.output_tokens,
		   cached_tokens      = cached_tokens      + excluded.cached_tokens,
		   cache_write_tokens = cache_write_tokens + excluded.cache_write_tokens,
		   cost_usd           = cost_usd           + excluded.cost_usd,
		   turns              = turns              + 1;`,
		day, in, out, cached, cacheWrite, cost)
	if err != nil {
		return fmt.Errorf("add usage: %w", err)
	}
	return nil
}

// Usage returns the all-time cumulative totals (every bucket, including the
// day=0 legacy bucket migrated from before per-day tracking).
func (s *Store) Usage() (TokenUsage, error) {
	return s.usageWhere("")
}

// UsageSince returns totals for day buckets at or after `since` (Unix seconds at
// a local midnight). The day=0 legacy bucket is excluded from any dated range
// (since > 0), since its turns predate per-day tracking and can't be dated.
func (s *Store) UsageSince(since int64) (TokenUsage, error) {
	return s.usageWhere("WHERE day >= ?", since)
}

func (s *Store) usageWhere(cond string, args ...any) (TokenUsage, error) {
	var u TokenUsage
	err := s.db.QueryRow(
		`SELECT COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
		        COALESCE(SUM(cached_tokens),0), COALESCE(SUM(cache_write_tokens),0),
		        COALESCE(SUM(cost_usd),0), COALESCE(SUM(turns),0)
		 FROM token_usage_daily `+cond, args...).
		Scan(&u.InputTokens, &u.OutputTokens, &u.CachedTokens, &u.CacheWriteTokens, &u.CostUSD, &u.Turns)
	if err != nil {
		return u, fmt.Errorf("usage: %w", err)
	}
	return u, nil
}

// --- resume map ---

// SaveResume records (or replaces) the resume id for a (sessionKey, agent)
// pair. The agent name is part of the conflict key: a resume id minted by
// the claude CLI must not be silently fed back to a different driver
// (Codex / Gemini) that can't honor it (store key-by-agent fix).
func (s *Store) SaveResume(sessionKey, agent, resumeID string) error {
	if agent == "" {
		return fmt.Errorf("save resume: agent name required")
	}
	_, err := s.db.Exec(
		`INSERT INTO agent_sessions(session_key, agent, resume_id, updated_at)
		 VALUES(?,?,?,?)
		 ON CONFLICT(session_key, agent) DO UPDATE SET
		   resume_id=excluded.resume_id, updated_at=excluded.updated_at;`,
		sessionKey, agent, resumeID, s.now().Unix())
	if err != nil {
		return fmt.Errorf("save resume: %w", err)
	}
	return nil
}

// Resume returns the stored resume id for a (sessionKey, agent) pair, or ""
// if none. The agent filter is load-bearing: prior code keyed on sessionKey
// alone and would silently return a Claude resume id to a Codex driver
// (latent multi-driver bug — only one driver exists today, but the seam is
// documented as additive). Empty agent argument returns "" (and would have
// matched anything in the old schema).
func (s *Store) Resume(sessionKey, agent string) (string, error) {
	if agent == "" {
		return "", nil
	}
	var id string
	err := s.db.QueryRow(`SELECT resume_id FROM agent_sessions WHERE session_key=? AND agent=?`, sessionKey, agent).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("query resume: %w", err)
	}
	return id, nil
}

// ClearResume drops EVERY agent's resume mapping for sessionKey. Used by
// /reset (the user-facing reset), which intentionally severs continuity
// across all drivers — a /reset on a session means "start fresh, regardless
// of which agent was last in charge."
func (s *Store) ClearResume(sessionKey string) error {
	if _, err := s.db.Exec(`DELETE FROM agent_sessions WHERE session_key=?`, sessionKey); err != nil {
		return fmt.Errorf("clear resume: %w", err)
	}
	return nil
}

// ClearResumeForAgent drops the resume mapping for ONE (sessionKey, agent)
// pair. Used by the gateway self-heal path: when ONE driver
// emits ResumeInvalid, only ITS row should be cleared — nuking every
// driver's row would contradict the composite-PK promise that
// two drivers can hold concurrent resume ids without one feeding the
// other a stale id.
func (s *Store) ClearResumeForAgent(sessionKey, agent string) error {
	if agent == "" {
		return fmt.Errorf("clear resume: agent name required")
	}
	if _, err := s.db.Exec(`DELETE FROM agent_sessions WHERE session_key=? AND agent=?`, sessionKey, agent); err != nil {
		return fmt.Errorf("clear resume (agent): %w", err)
	}
	return nil
}

// --- group reply cursor (answered/new segmentation) ---

// BotReplySeq returns the IM message_seq of the last group message the bot
// replied to for this session key (0 if none / cold start).
func (s *Store) BotReplySeq(sessionKey string) (int64, error) {
	var seq int64
	err := s.db.QueryRow(`SELECT last_seq FROM group_reply_cursors WHERE session_key=?`, sessionKey).Scan(&seq)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("query bot reply seq: %w", err)
	}
	return seq, nil
}

// SaveBotReplySeq advances the bot's last-reply cursor for a session key. The
// write is monotonic: a lower (or equal) seq is ignored, matching the
// lastBotReplySeqMap guard in openclaw inbound.ts. seq<=0 (synthetic/cron) is a
// no-op since those are never "answered".
func (s *Store) SaveBotReplySeq(sessionKey string, seq int64) error {
	if seq <= 0 {
		return nil
	}
	_, err := s.db.Exec(
		`INSERT INTO group_reply_cursors(session_key, last_seq, updated_at)
		 VALUES(?,?,?)
		 ON CONFLICT(session_key) DO UPDATE SET last_seq=excluded.last_seq,
		   updated_at=excluded.updated_at
		 WHERE excluded.last_seq > group_reply_cursors.last_seq;`,
		sessionKey, seq, s.now().Unix())
	if err != nil {
		return fmt.Errorf("save bot reply seq: %w", err)
	}
	return nil
}

// ClearHistory deletes the persisted conversation messages for a session (the
// /reset side effect, the Go analogue of cc-channel's store.deleteSession
// history clear). It does NOT touch the agent resume mapping (clear that with
// ClearResume) nor long-term auto-memory (which lives outside the store). The
// session row itself is kept so its channel binding survives a reset.
func (s *Store) ClearHistory(sessionID string) error {
	if _, err := s.db.Exec(`DELETE FROM messages WHERE session_id=?`, sessionID); err != nil {
		return fmt.Errorf("clear history: %w", err)
	}
	return nil
}
