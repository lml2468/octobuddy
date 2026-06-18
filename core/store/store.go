package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: no cgo, cross-compiles cleanly
)

// Role of a stored message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one durable conversation record. This is NOT the live prompt
// history (that lives in the agent's own resumable session); it is used for
// first-turn/migration injection and stale-resume recovery, exactly as in
// cc-channel's session-store.
type Message struct {
	Role      Role
	Content   string
	Timestamp int64
	FromName  string
}

// Store is the SQLite-backed persistence layer. Pure-Go SQLite keeps the core a
// single static binary with zero cgo.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
  id           TEXT PRIMARY KEY,
  channel_id   TEXT NOT NULL,
  channel_type INTEGER NOT NULL,
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS messages (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL,
  role       TEXT NOT NULL CHECK(role IN ('user','assistant')),
  content    TEXT NOT NULL,
  timestamp  INTEGER NOT NULL,
  from_name  TEXT,
  FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE
);

-- Maps a logical sessionKey to the agent's resume id (Claude session id /
-- Codex thread id). Separate lifecycle from sessions; cleared on /reset.
CREATE TABLE IF NOT EXISTS agent_sessions (
  session_key TEXT PRIMARY KEY,
  agent       TEXT NOT NULL,
  resume_id   TEXT NOT NULL,
  updated_at  INTEGER NOT NULL
);

-- Group answered/new segmentation cursor (cc G10 / openclaw lastBotReplySeqMap):
-- the IM message_seq of the last group message the bot replied to, keyed by the
-- group's sessionKey (the channel id). Group context renders messages at or
-- below this seq under [Previously answered] and newer ones under [New since
-- your last reply]. Advanced only after a successful reply to a real message.
CREATE TABLE IF NOT EXISTS group_reply_cursors (
  session_key TEXT PRIMARY KEY,
  last_seq    INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL
);

-- Per-day token usage buckets for this bot's store (one DB per bot). Each row is
-- one local calendar day (day = Unix seconds at local midnight); AddUsage upserts
-- into today's row, and range queries SUM over day >= since [AND day < until].
-- The legacy all-time aggregate migrated from the old single-row table lands in
-- the day=0 bucket: counted in "All" (since=0) but excluded from dated ranges.
CREATE TABLE IF NOT EXISTS token_usage_daily (
  day                INTEGER PRIMARY KEY,
  input_tokens       INTEGER NOT NULL DEFAULT 0,
  output_tokens      INTEGER NOT NULL DEFAULT 0,
  cached_tokens      INTEGER NOT NULL DEFAULT 0,
  cache_write_tokens INTEGER NOT NULL DEFAULT 0,
  cost_usd           REAL    NOT NULL DEFAULT 0,
  turns              INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_messages_session_id ON messages(session_id, id);
`

// Open initializes the database (creating the schema) at path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	if err := migrateTokenUsage(db); err != nil {
		return nil, err
	}
	return &Store{db: db, now: time.Now}, nil
}

// migrateTokenUsage folds the legacy single-row token_usage aggregate (pre
// per-day buckets) into the day=0 bucket of token_usage_daily, then drops the old
// table. Idempotent: a no-op once the old table is gone. The day=0 bucket keeps
// pre-migration totals visible under "All" while excluding them from dated ranges
// (those days weren't recorded, so attributing them to any real day would lie).
func migrateTokenUsage(db *sql.DB) error {
	var exists int
	if err := db.QueryRow(
		`SELECT 1 FROM sqlite_master WHERE type='table' AND name='token_usage'`).Scan(&exists); err == sql.ErrNoRows {
		return nil // already migrated (or fresh DB)
	} else if err != nil {
		return fmt.Errorf("migrate token_usage check: %w", err)
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`INSERT INTO token_usage_daily(day, input_tokens, output_tokens, cached_tokens, cache_write_tokens, cost_usd, turns)
		 SELECT 0, input_tokens, output_tokens, cached_tokens, cache_write_tokens, cost_usd, turns FROM token_usage WHERE id = 1
		 ON CONFLICT(day) DO UPDATE SET
		   input_tokens       = input_tokens       + excluded.input_tokens,
		   output_tokens      = output_tokens      + excluded.output_tokens,
		   cached_tokens      = cached_tokens      + excluded.cached_tokens,
		   cache_write_tokens = cache_write_tokens + excluded.cache_write_tokens,
		   cost_usd           = cost_usd           + excluded.cost_usd,
		   turns              = turns              + excluded.turns;`); err != nil {
		return fmt.Errorf("migrate token_usage copy: %w", err)
	}
	if _, err := tx.Exec(`DROP TABLE token_usage`); err != nil {
		return fmt.Errorf("migrate token_usage drop: %w", err)
	}
	return tx.Commit()
}

// dsn carries the connection pragmas in the DSN as _pragma query params so the
// modernc driver re-applies them on EVERY pooled connection it opens — not just
// the first. foreign_keys is connection-scoped, so setting it once via Exec left
// other pooled connections with FK enforcement OFF, letting an orphaned insert
// or a missed ON DELETE CASCADE slip through on whichever connection the pool
// happened to hand out (MLT-33). busy_timeout is likewise per-connection;
// journal_mode=WAL is a persistent database setting but is cheap and idempotent
// to assert per-connection.
func dsn(path string) string {
	sep := "?"
	if strings.ContainsRune(path, '?') {
		sep = "&"
	}
	return path + sep + "_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)"
}

// SetClock overrides the time source (tests use this for deterministic TTL).
func (s *Store) SetClock(now func() time.Time) { s.now = now }

func (s *Store) Close() error { return s.db.Close() }

// --- sessions ---

// Session is the metadata row for a logical session.
type Session struct {
	ID          string
	ChannelID   string
	ChannelType int
	CreatedAt   int64
	UpdatedAt   int64
}

// Touch creates the session if absent and refreshes updated_at so the TTL sweep
// keeps it alive — without GetOrCreate's extra round-trip to read the row back.
// Use this when the caller only needs the session to exist (the gateway's
// per-turn path), not the Session value.
func (s *Store) Touch(id, channelID string, channelType int) error {
	now := s.now().Unix()
	if _, err := s.db.Exec(
		`INSERT INTO sessions(id, channel_id, channel_type, created_at, updated_at)
		 VALUES(?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET updated_at=excluded.updated_at;`,
		id, channelID, channelType, now, now); err != nil {
		return fmt.Errorf("touch: %w", err)
	}
	return nil
}

// GetOrCreate returns the session for id, creating it if absent. updated_at is
// refreshed on every call so the TTL sweep keeps active sessions alive.
func (s *Store) GetOrCreate(id, channelID string, channelType int) (Session, error) {
	now := s.now().Unix()
	_, err := s.db.Exec(
		`INSERT INTO sessions(id, channel_id, channel_type, created_at, updated_at)
		 VALUES(?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET updated_at=excluded.updated_at;`,
		id, channelID, channelType, now, now)
	if err != nil {
		return Session{}, fmt.Errorf("getOrCreate: %w", err)
	}
	var sess Session
	err = s.db.QueryRow(
		`SELECT id, channel_id, channel_type, created_at, updated_at FROM sessions WHERE id=?`, id,
	).Scan(&sess.ID, &sess.ChannelID, &sess.ChannelType, &sess.CreatedAt, &sess.UpdatedAt)
	return sess, err
}

// --- messages ---

func (s *Store) AppendUser(sessionID, content, fromName string) error {
	return s.appendMessage(sessionID, RoleUser, content, fromName)
}

func (s *Store) AppendAssistant(sessionID, content, botName string) error {
	return s.appendMessage(sessionID, RoleAssistant, content, botName)
}

func (s *Store) appendMessage(sessionID string, role Role, content, fromName string) error {
	now := s.now().Unix()
	_, err := s.db.Exec(
		`INSERT INTO messages(session_id, role, content, timestamp, from_name) VALUES(?,?,?,?,?)`,
		sessionID, string(role), content, now, fromName)
	if err != nil {
		return fmt.Errorf("append %s: %w", role, err)
	}
	_, _ = s.db.Exec(`UPDATE sessions SET updated_at=? WHERE id=?`, now, sessionID)
	return nil
}

// RecentMessages returns up to limit most-recent messages in chronological
// order (oldest first), for first-turn history injection.
func (s *Store) RecentMessages(sessionID string, limit int) ([]Message, error) {
	rows, err := s.db.Query(
		`SELECT role, content, timestamp, COALESCE(from_name,'')
		 FROM messages WHERE session_id=? ORDER BY id DESC LIMIT ?`,
		sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		var role string
		if err := rows.Scan(&role, &m.Content, &m.Timestamp, &m.FromName); err != nil {
			return nil, err
		}
		m.Role = Role(role)
		out = append(out, m)
	}
	// reverse to chronological (oldest first)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// SessionSummary is one row for the desktop conversation list: the session's
// key plus a preview drawn from its most recent message. Preview/LastRole are
// empty when the session has no messages yet.
type SessionSummary struct {
	Key         string
	ChannelType int
	UpdatedAt   int64
	Preview     string
	LastRole    Role
}

// ListSessions returns every persisted session for this bot's store, newest
// updated first, each with a preview from its latest message. The store is
// already per-bot (one DB under ~/.xclaw/<id>/), so this lists exactly that
// bot's sessions. The correlated subquery picks the highest-id (newest) message
// per session, covered by idx_messages_session_id.
func (s *Store) ListSessions() ([]SessionSummary, error) {
	rows, err := s.db.Query(
		`SELECT s.id, s.channel_type, s.updated_at,
		        COALESCE(m.content,''), COALESCE(m.role,'')
		 FROM sessions s
		 LEFT JOIN messages m ON m.id = (
		   SELECT id FROM messages WHERE session_id = s.id ORDER BY id DESC LIMIT 1
		 )
		 ORDER BY s.updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionSummary
	for rows.Next() {
		var ss SessionSummary
		var role string
		if err := rows.Scan(&ss.Key, &ss.ChannelType, &ss.UpdatedAt, &ss.Preview, &role); err != nil {
			return nil, err
		}
		ss.LastRole = Role(role)
		out = append(out, ss)
	}
	return out, rows.Err()
}

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

// SaveResume records (or replaces) the resume id for a session key.
func (s *Store) SaveResume(sessionKey, agent, resumeID string) error {
	_, err := s.db.Exec(
		`INSERT INTO agent_sessions(session_key, agent, resume_id, updated_at)
		 VALUES(?,?,?,?)
		 ON CONFLICT(session_key) DO UPDATE SET agent=excluded.agent,
		   resume_id=excluded.resume_id, updated_at=excluded.updated_at;`,
		sessionKey, agent, resumeID, s.now().Unix())
	return err
}

// Resume returns the stored resume id for a session key ("" if none).
func (s *Store) Resume(sessionKey string) (string, error) {
	var id string
	err := s.db.QueryRow(`SELECT resume_id FROM agent_sessions WHERE session_key=?`, sessionKey).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("query resume: %w", err)
	}
	return id, nil
}

// ClearResume drops the resume mapping (used by /reset).
func (s *Store) ClearResume(sessionKey string) error {
	_, err := s.db.Exec(`DELETE FROM agent_sessions WHERE session_key=?`, sessionKey)
	return err
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
	return err
}

// ClearHistory deletes the persisted conversation messages for a session (the
// /reset side effect, the Go analogue of cc-channel's store.deleteSession
// history clear). It does NOT touch the agent resume mapping (clear that with
// ClearResume) nor long-term auto-memory (which lives outside the store). The
// session row itself is kept so its TTL and channel binding survive a reset.
func (s *Store) ClearHistory(sessionID string) error {
	_, err := s.db.Exec(`DELETE FROM messages WHERE session_id=?`, sessionID)
	return err
}

// --- maintenance ---
