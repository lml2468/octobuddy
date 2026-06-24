package store

import (
	"database/sql"
	"fmt"
	"strings"
)

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
  -- source classifies the row's origin: 'user' (human inbound), 'cron'
  -- (scheduler fire), 'assistant' (bot reply), or future origins
  -- (webhook, replay). Persisted so the desktop GUI's "cron" corner
  -- badge survives a chat-window reload (history fetch replays from
  -- here — without persistence every reopened conversation would lose
  -- the badge and conflate scheduled prompts with operator-typed ones).
  -- Replaces the legacy 'cron INTEGER' column; migration backfills.
  source     TEXT NOT NULL DEFAULT 'user',
  FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE
);

-- Maps a logical sessionKey to the agent's resume id (Claude session id /
-- Codex thread id). Separate lifecycle from sessions; cleared on /reset.
-- Composite PK on (session_key, agent) so two drivers can hold concurrent
-- resume ids for the same logical session without one overwriting the
-- other; Resume() filters by agent so a Claude id is never silently
-- handed to a Codex driver. Existing legacy DBs with
-- the old single-column-PK shape are rebuilt by Open's
-- migrateAgentSessions BEFORE this DDL runs.
CREATE TABLE IF NOT EXISTS agent_sessions (
  session_key TEXT NOT NULL,
  agent       TEXT NOT NULL,
  resume_id   TEXT NOT NULL,
  updated_at  INTEGER NOT NULL,
  PRIMARY KEY (session_key, agent)
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

// migrateAgentSessions rebuilds the agent_sessions table when it carries the
// legacy single-column PK shape. SQLite has no portable ALTER for PK,
// so we copy → drop → recreate via the current schema (run by Open right
// after) → restore the rows. Idempotent: a no-op when the table already has
// the composite shape (or when it doesn't exist yet — a fresh DB).
//
// We preserve the existing rows verbatim; legacy rows are all
// single-(session_key) so the de-dup-into-composite-PK collision can't fire.
//
// Detection uses PRAGMA table_info / index_list rather than substring-matching
// the DDL text: sqlite_master.sql is preserved as authored, but
// different SQLite versions or whitespace-rewritten DDL would defeat a literal
// substring match — an undetected legacy shape would either keep running with
// the broken PK or re-run the migration and fail on the second pass because
// the RENAME-then-rollback path isn't always atomic for DDL.
func migrateAgentSessions(db *sql.DB) error {
	// 1. Does the table exist at all? (Fresh DB → skip; schema below creates it.)
	hasTable, err := hasAgentSessionsTable(db)
	if err != nil {
		return err
	}
	if !hasTable {
		return nil
	}
	// 2. Already composite? Ask SQLite which columns are PK members via
	// PRAGMA table_info — the `pk` column is 0 for non-PK, 1+ for PK members
	// in order. Composite = >= 2 PK columns; legacy = exactly 1.
	pkCols, err := agentSessionsPKColumnCount(db)
	if err != nil {
		return err
	}
	if pkCols >= 2 {
		return nil // already composite
	}
	return rebuildAgentSessions(db)
}

func hasAgentSessionsTable(db *sql.DB) (bool, error) {
	var hasTable int
	err := db.QueryRow(
		`SELECT 1 FROM sqlite_master WHERE type='table' AND name='agent_sessions'`,
	).Scan(&hasTable)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("agent_sessions detect: %w", err)
	}
	return true, nil
}

func agentSessionsPKColumnCount(db *sql.DB) (int, error) {
	rows, err := db.Query(`PRAGMA table_info(agent_sessions)`)
	if err != nil {
		return 0, fmt.Errorf("agent_sessions table_info: %w", err)
	}
	pkCols := 0
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, ctype      string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return 0, fmt.Errorf("agent_sessions table_info scan: %w", err)
		}
		if pk > 0 {
			pkCols++
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("agent_sessions table_info rows: %w", err)
	}
	rows.Close()
	return pkCols, nil
}

func rebuildAgentSessions(db *sql.DB) error {
	// 3. Legacy shape — rebuild. Wrap in a tx so a crash mid-migration can't
	// orphan the data.
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("agent_sessions migrate tx: %w", err)
	}
	defer tx.Rollback()
	stmts := []string{
		`ALTER TABLE agent_sessions RENAME TO agent_sessions_legacy`,
		`CREATE TABLE agent_sessions (
		   session_key TEXT NOT NULL,
		   agent       TEXT NOT NULL,
		   resume_id   TEXT NOT NULL,
		   updated_at  INTEGER NOT NULL,
		   PRIMARY KEY (session_key, agent)
		 )`,
		`INSERT INTO agent_sessions(session_key, agent, resume_id, updated_at)
		 SELECT session_key, agent, resume_id, updated_at FROM agent_sessions_legacy`,
		`DROP TABLE agent_sessions_legacy`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("agent_sessions migrate: %w", err)
		}
	}
	return tx.Commit()
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
		return fmt.Errorf("migrate token_usage tx: %w", err)
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

// migrateMessagesAddSource adds the messages.source column to DBs that
// pre-date the source-string migration (i.e. carry the legacy `cron`
// integer column instead). Wrapped in a transaction so the ALTER +
// backfills are atomic — a crash mid-migration rolls back, and the next
// startup re-runs cleanly without orphaning rows at the wrong source tag.
//
// Two-phase shape:
//
//  1. Schema phase (one-time): ALTER TABLE ADD COLUMN source TEXT NOT
//     NULL DEFAULT 'user', then backfill role='assistant'→'assistant' and
//     legacy cron=1→'cron'. Skipped if the column already exists.
//
//  2. Self-healing phase (every startup): re-runs the assistant backfill
//     against any rows where role='assistant' AND source!='assistant'.
//     This recovers from a previous partial migration that committed the
//     ALTER but rolled back the UPDATE (cheap idempotent UPDATE; the
//     WHERE filter is index-friendly even on large tables because legacy
//     assistant rows are stamped to the literal 'user' default and the
//     check is a string-compare).
//
// The legacy cron column is LEFT IN PLACE — SQLite's portable DROP
// COLUMN landed in 3.35 but failing it would brick the upgrade, and
// leaving the column is harmless (writes ignore it, reads SELECT source).
func migrateMessagesAddSource(db *sql.DB) error {
	hasSource, hasLegacyCron, err := messagesColumns(db)
	if err != nil {
		return err
	}
	if !hasSource {
		if err := addSourceColumn(db, hasLegacyCron); err != nil {
			return err
		}
	}
	// Self-heal: cover the case where a previous run committed ALTER but
	// a crash rolled the backfill back (pre-tx version of this code), so
	// legacy assistant rows stayed tagged with the DEFAULT 'user'.
	// EXISTS-guard the UPDATE first — on a clean DB (the overwhelmingly
	// common case after the migration ran once) SQLite short-circuits at
	// the first matching row and the UPDATE doesn't fire, avoiding the
	// full table scan on every startup. On stale DBs the UPDATE still
	// runs and fixes the drift.
	var dirty bool
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM messages WHERE role='assistant' AND source!='assistant' LIMIT 1)`).Scan(&dirty); err != nil {
		return fmt.Errorf("migrate messages.source self-heal probe: %w", err)
	}
	if dirty {
		if _, err := db.Exec(`UPDATE messages SET source='assistant' WHERE role='assistant' AND source!='assistant'`); err != nil {
			return fmt.Errorf("migrate messages.source self-heal assistant: %w", err)
		}
	}
	return nil
}

// addSourceColumn runs the ALTER + initial backfills in a transaction so
// a mid-flight crash rolls back the column add and the next startup gets
// a clean retry.
func addSourceColumn(db *sql.DB, hasLegacyCron bool) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate messages.source tx: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`ALTER TABLE messages ADD COLUMN source TEXT NOT NULL DEFAULT 'user'`); err != nil {
		return fmt.Errorf("migrate messages.source add column: %w", err)
	}
	// Backfill assistant rows first — every assistant write now stamps
	// 'assistant', so legacy assistant rows must too (otherwise the
	// reload would falsely tag them as 'user' inbound).
	if _, err := tx.Exec(`UPDATE messages SET source='assistant' WHERE role='assistant'`); err != nil {
		return fmt.Errorf("migrate messages.source backfill assistant: %w", err)
	}
	// Backfill cron-fired user rows from the legacy column when present.
	if hasLegacyCron {
		if _, err := tx.Exec(`UPDATE messages SET source='cron' WHERE role='user' AND cron=1`); err != nil {
			return fmt.Errorf("migrate messages.source backfill cron: %w", err)
		}
	}
	return tx.Commit()
}

// messagesColumns reports the presence of the new `source` column and the
// legacy `cron` column.
func messagesColumns(db *sql.DB) (hasSource, hasLegacyCron bool, err error) {
	rows, err := db.Query(`PRAGMA table_info(messages)`)
	if err != nil {
		return false, false, fmt.Errorf("migrate messages.source check: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype, dflt sql.NullString
		var notnull, pk int
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, false, fmt.Errorf("migrate messages.source scan: %w", err)
		}
		switch name.String {
		case "source":
			hasSource = true
		case "cron":
			hasLegacyCron = true
		}
	}
	if err := rows.Err(); err != nil {
		return false, false, fmt.Errorf("migrate messages.source rows: %w", err)
	}
	return hasSource, hasLegacyCron, nil
}

// dsn carries the connection pragmas in the DSN as _pragma query params so the
// modernc driver re-applies them on EVERY pooled connection it opens — not just
// the first. foreign_keys is connection-scoped, so setting it once via Exec left
// other pooled connections with FK enforcement OFF, letting an orphaned insert
// or a missed ON DELETE CASCADE slip through on whichever connection the pool
// happened to hand out. busy_timeout is likewise per-connection;
// journal_mode=WAL is a persistent database setting but is cheap and idempotent
// to assert per-connection.
func dsn(path string) string {
	sep := "?"
	if strings.ContainsRune(path, '?') {
		sep = "&"
	}
	return path + sep + "_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)"
}
