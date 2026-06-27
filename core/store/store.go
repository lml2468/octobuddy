package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/lml2468/octobuddy/core/clog"

	_ "modernc.org/sqlite" // pure-Go driver: no cgo, cross-compiles cleanly
)

// Role of a stored message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// SourceUser / SourceCron / SourceAssistant are the values stored in
// messages.source. They mirror trigger.Source plus an assistant tag so
// the badge logic can distinguish operator-typed text from scheduler fires
// and from bot replies. Stored as TEXT to keep the migration path open
// for future origins (webhook, replay) without further schema changes.
const (
	SourceUser      = "user"
	SourceCron      = "cron"
	SourceAssistant = "assistant"
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
	// FromUID is the IM platform's stable id of the human author of a
	// user-role row. The durable identity behind FromName (which can be
	// empty at append time and converge later): persisted so a reload can
	// re-resolve the live display name and never collapses a nameless
	// bubble to "You". Empty for assistant rows and legacy rows predating
	// the column.
	FromUID string
	// Steps is the JSON array of process steps (tool calls / thinking) the
	// agent took producing an assistant row, e.g.
	// `[{"kind":"tool","text":"Read(README.md)"}]`. Persisted so a reload
	// re-renders the step card above the reply bubble. Empty for user rows
	// and legacy assistant rows predating the column.
	Steps string
	// Source classifies the row's origin. SourceUser (default human
	// inbound), SourceCron (scheduler fire), SourceAssistant (bot reply).
	// Persisted so the desktop GUI's "cron" corner badge survives a
	// reload of the chat window — without this, every reopened
	// conversation would lose the badge on prior cron fires. Replaces
	// the legacy `Cron bool` (which collapsed every non-user-non-cron
	// origin into one bit).
	Source string
}

// Store is the SQLite-backed persistence layer. Pure-Go SQLite keeps the core a
// single static binary with zero cgo.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open initializes the database (creating the schema) at path.
//
// pre-check each of octobuddy.db / octobuddy.db-wal / octobuddy.db-shm
// for a leaf symlink before SQLite opens them. An agent (Bash + bypass)
// that plants `<dataDir>/octobuddy.db → ~/Documents/important.sqlite` would
// otherwise have SQLite open through the symlink and run schema
// migrations (ALTER, DROP TABLE agent_sessions_legacy, INSERT) on the
// operator's unrelated DB — data destruction / cross-DB write.
func Open(path string) (*Store, error) {
	for _, leaf := range []string{"", "-wal", "-shm"} {
		if err := refuseSymlinkLeaf(path + leaf); err != nil {
			return nil, fmt.Errorf("open store %s: %w", path+leaf, err)
		}
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, err
	}
	// Run pre-schema migrations FIRST so the schema's IF-NOT-EXISTS DDL
	// (which can't ALTER an existing table) doesn't lock us into the old
	// shape. agent_sessions in particular went from single-column PK
	// (legacy) to composite PK in; the IF-NOT-EXISTS form
	// is a no-op against the legacy shape, so SaveResume's ON CONFLICT(...)
	// fails until we destructively rebuild the table.
	if err := migrateAgentSessions(db); err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	if err := migrateTokenUsage(db); err != nil {
		return nil, err
	}
	if err := migrateMessagesAddSource(db); err != nil {
		return nil, err
	}
	if err := migrateMessagesAddFromUID(db); err != nil {
		return nil, err
	}
	if err := migrateMessagesAddSteps(db); err != nil {
		return nil, err
	}
	return &Store{db: db, now: time.Now}, nil
}

// SetClock overrides the time source (tests use this for deterministic timestamps).
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

// Touch creates the session if absent and refreshes updated_at so ListSessions
// orders it newest-first — without GetOrCreate's extra round-trip to read the row
// back. Use this when the caller only needs the session to exist (the gateway's
// per-turn path), not the Session value. (Sessions are persistent: there is no
// TTL reclamation; updated_at only drives ordering.)
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
// refreshed on every call so ListSessions orders active sessions newest-first.
// (Sessions are persistent: no TTL reclamation; updated_at only drives ordering.)
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

func (s *Store) AppendUser(sessionID, content, fromName, fromUID, source string) error {
	if source == "" {
		source = SourceUser
	}
	return s.appendMessage(sessionID, RoleUser, content, fromName, fromUID, source, "")
}

// AppendAssistant persists a bot reply. stepsJSON is the JSON array of process
// steps the agent took this turn (tool calls / thinking), or "" when there were
// none — replayed by the desktop to re-render the step card above the bubble.
func (s *Store) AppendAssistant(sessionID, content, botName, stepsJSON string) error {
	return s.appendMessage(sessionID, RoleAssistant, content, botName, "", SourceAssistant, stepsJSON)
}

func (s *Store) appendMessage(sessionID string, role Role, content, fromName, fromUID, source, steps string) error {
	now := s.now().Unix()
	_, err := s.db.Exec(
		`INSERT INTO messages(session_id, role, content, timestamp, from_name, from_uid, source, steps) VALUES(?,?,?,?,?,?,?,?)`,
		sessionID, string(role), content, now, fromName, fromUID, source, steps)
	if err != nil {
		return fmt.Errorf("append %s: %w", role, err)
	}
	// Bump updated_at so ListSessions orders this conversation newest-first. A
	// failure here only affects ordering (the message is already persisted), so
	// log it rather than failing the whole append.
	if _, uerr := s.db.Exec(`UPDATE sessions SET updated_at=? WHERE id=?`, now, sessionID); uerr != nil {
		clog.For("store").Warn("bump updated_at failed", "session", sessionID, "err", uerr)
	}
	return nil
}

// RecentMessages returns up to limit most-recent messages in chronological
// order (oldest first), for first-turn history injection.
func (s *Store) RecentMessages(sessionID string, limit int) ([]Message, error) {
	rows, err := s.db.Query(
		`SELECT role, content, timestamp, COALESCE(from_name,''), COALESCE(from_uid,''), COALESCE(source,'user'), COALESCE(steps,'')
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
		if err := rows.Scan(&role, &m.Content, &m.Timestamp, &m.FromName, &m.FromUID, &m.Source, &m.Steps); err != nil {
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
// already per-bot (one DB under ~/.octobuddy/<id>/), so this lists exactly that
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
