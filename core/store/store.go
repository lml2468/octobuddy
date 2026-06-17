package store

import (
	"database/sql"
	"fmt"
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

CREATE INDEX IF NOT EXISTS idx_messages_session_id ON messages(session_id, id);
`

// Open initializes the database (creating the schema) at path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;`); err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	return &Store{db: db, now: time.Now}, nil
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
