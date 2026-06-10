package store

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite" // pure-Go driver: no cgo, cross-compiles cleanly
)

// SessionStore is the minimal slice the gateway needs from cc-channel's
// session-store: a map from a logical sessionKey to the agent's resume id.
// Pure-Go SQLite proves the "single static binary, zero cgo, trivial
// cross-compile" claim that motivated choosing Go for the core.
type SessionStore struct{ db *sql.DB }

func Open(path string) (*SessionStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;`); err != nil {
		return nil, err
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS agent_sessions (
			session_key TEXT PRIMARY KEY,
			agent       TEXT NOT NULL,
			resume_id   TEXT NOT NULL,
			updated_at  INTEGER NOT NULL
		);`); err != nil {
		return nil, err
	}
	return &SessionStore{db: db}, nil
}

func (s *SessionStore) Close() error { return s.db.Close() }

// SaveResume records (or replaces) the resume id for a session key.
func (s *SessionStore) SaveResume(sessionKey, agent, resumeID string, ts int64) error {
	_, err := s.db.Exec(
		`INSERT INTO agent_sessions(session_key, agent, resume_id, updated_at)
		 VALUES(?,?,?,?)
		 ON CONFLICT(session_key) DO UPDATE SET agent=excluded.agent,
		   resume_id=excluded.resume_id, updated_at=excluded.updated_at;`,
		sessionKey, agent, resumeID, ts)
	return err
}

// Resume returns the stored resume id for a session key ("" if none).
func (s *SessionStore) Resume(sessionKey string) (string, error) {
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
