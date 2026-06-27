package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestDSN(t *testing.T) {
	if got := dsn("/tmp/x.db"); got != "/tmp/x.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)" {
		t.Fatalf("plain path dsn wrong: %q", got)
	}
	if got := dsn("file:/tmp/x.db?cache=shared"); got != "file:/tmp/x.db?cache=shared&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)" {
		t.Fatalf("uri path dsn wrong: %q", got)
	}
}

// TestPragmasApplyPerPooledConnection proves the DSN pragmas are actually
// replayed on a freshly-handed-out pooled connection, not merely present in the
// DSN string (TestDSN's scope). A connection is pinned so the assertions run on
// a second, distinct connection — closing the "every connection, not just the
// pool's first" invariant for all three pragmas, not only foreign_keys.
func TestPragmasApplyPerPooledConnection(t *testing.T) {
	s := openTemp(t)
	s.db.SetMaxOpenConns(3)
	ctx := context.Background()

	pinned, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatalf("pin conn: %v", err)
	}
	defer pinned.Close()
	if err := pinned.PingContext(ctx); err != nil {
		t.Fatalf("ping pinned: %v", err)
	}

	// Pinned is still checked out, so this is a different physical connection.
	other, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatalf("second conn: %v", err)
	}
	defer other.Close()

	var busyTimeout, foreignKeys int
	var journalMode string
	if err := other.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatalf("busy_timeout: %v", err)
	}
	if err := other.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatalf("foreign_keys: %v", err)
	}
	if err := other.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if busyTimeout != 5000 {
		t.Errorf("busy_timeout = %d, want 5000 (per-connection pragma not replayed)", busyTimeout)
	}
	if foreignKeys != 1 {
		t.Errorf("foreign_keys = %d, want 1", foreignKeys)
	}
	if !strings.EqualFold(journalMode, "wal") {
		t.Errorf("journal_mode = %q, want wal", journalMode)
	}
}

// TestSaveResumeAgainstLegacySchema is the regression for the prior
// agent_sessions migration finding: changed the table's PRIMARY KEY
// from `(session_key)` to `(session_key, agent)`, but the schema declaration
// uses CREATE TABLE IF NOT EXISTS, which is a no-op against legacy
// deployments. Without the separate CREATE UNIQUE INDEX, those deployments
// would fail every SaveResume with "ON CONFLICT clause does not match any
// PRIMARY KEY or UNIQUE constraint" and silently lose resume continuity on
// every turn.
//
// The test simulates an upgrade: open a DB with the legacy schema, close,
// reopen via the production Open (which runs the current schema-plus-index
// DDL via IF NOT EXISTS), then assert SaveResume works.
func TestSaveResumeAgainstLegacySchema(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "legacy.db")

	createLegacyAgentSessionsDB(t, dbPath)

	// Step 2: reopen via the production path. The current schema runs all its
	// IF NOT EXISTS DDL (incl. the new CREATE UNIQUE INDEX) without dropping
	// the legacy table, so the table keeps its old single-column PK but
	// gains the composite unique index that ON CONFLICT can target.
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()

	assertLegacyResumeUpsert(t, s)
}

func createLegacyAgentSessionsDB(t *testing.T, dbPath string) {
	t.Helper()

	legacy, err := Open(dbPath)
	if err != nil {
		t.Fatalf("legacy open: %v", err)
	}
	// Drop the unique index + composite-PK table, recreate the table
	// with the old single-column PK shape (post-CREATE TABLE IF NOT EXISTS is
	// a no-op so we DROP first).
	if _, err := legacy.db.Exec(`DROP INDEX IF EXISTS ux_agent_sessions_key_agent`); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	if _, err := legacy.db.Exec(`DROP TABLE agent_sessions`); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	if _, err := legacy.db.Exec(`CREATE TABLE agent_sessions (
			session_key TEXT PRIMARY KEY,
			agent       TEXT NOT NULL,
			resume_id   TEXT NOT NULL,
			updated_at  INTEGER NOT NULL
		)`); err != nil {
		t.Fatalf("legacy create: %v", err)
	}
	// Pre-existing row from a legacy daemon run.
	if _, err := legacy.db.Exec(`INSERT INTO agent_sessions(session_key, agent, resume_id, updated_at) VALUES (?,?,?,?)`,
		"dm:peer", "claude", "sess-legacy", 1); err != nil {
		t.Fatalf("legacy seed: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func assertLegacyResumeUpsert(t *testing.T, s *Store) {
	t.Helper()

	saveAndAssertResume(t, s, "dm:peer", "claude", "sess-new", "SaveResume against legacy schema must succeed: %v", "Resume after upsert = %q, want sess-new")
	// A different agent for the same session key should also work — that's
	// the whole point of the composite uniqueness.
	saveAndAssertResume(t, s, "dm:peer", "codex", "thr-new", "SaveResume for second agent must succeed: %v", "Resume codex = %q, want thr-new")
}

func saveAndAssertResume(t *testing.T, s *Store, key, agent, resumeID, saveFormat, gotFormat string) {
	t.Helper()
	if err := s.SaveResume(key, agent, resumeID); err != nil {
		t.Fatalf(saveFormat, err)
	}
	if got, _ := s.Resume(key, agent); got != resumeID {
		t.Fatalf(gotFormat, got)
	}
}

// TestMigrateMessagesPartialBackfillRecovery: simulates a pre-tx daemon
// that committed the ALTER TABLE but crashed before the assistant
// UPDATE — leaves assistant rows tagged with the DEFAULT 'user'. Next
// startup must self-heal those rows back to 'assistant' (otherwise
// chat-history reload renders the bot's replies as if they were user
// inbound, conflating speakers in the GUI).
func TestMigrateMessagesPartialBackfillRecovery(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "partial.db")
	createPartialBackfillDB(t, dbPath)

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	msgs, err := s.RecentMessages("sess", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(msgs))
	}
	var assistantSource string
	for _, m := range msgs {
		if m.Role == RoleAssistant {
			assistantSource = m.Source
		}
	}
	if assistantSource != SourceAssistant {
		t.Fatalf("assistant row stayed mis-tagged after self-heal: got %q, want %q", assistantSource, SourceAssistant)
	}
}

func createPartialBackfillDB(t *testing.T, dbPath string) {
	t.Helper()
	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO sessions(id, channel_id, channel_type, created_at, updated_at) VALUES ('sess','ch',1,1,1)`); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	// Insert an assistant row with the wrong source tag, simulating a
	// crashed partial migration (ALTER committed, backfill UPDATE didn't).
	if _, err := s.db.Exec(`INSERT INTO messages(session_id, role, content, timestamp, from_name, source) VALUES ('sess','user','hi',1,'alice','user')`); err != nil {
		t.Fatalf("seed user row: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO messages(session_id, role, content, timestamp, from_name, source) VALUES ('sess','assistant','ok',2,'bot','user')`); err != nil {
		t.Fatalf("seed mis-tagged assistant row: %v", err)
	}
	s.Close()
}

// AppendUser persists the message source and RecentMessages reads it
// back, so the GUI's "cron" badge survives a chat-window reload (history
// fetch replays from the store — without persistence, badges would be
// lost on every reopen).
func TestSourceRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_ = s.Touch("sess", "ch", 1)
	if err := s.AppendUser("sess", "real human", "alice", "u:alice", SourceUser); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendUser("sess", "cron fire", "cronbot", "", SourceCron); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendAssistant("sess", "ok", "bot", ""); err != nil {
		t.Fatal(err)
	}

	msgs, err := s.RecentMessages("sess", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("got %d msgs, want 3", len(msgs))
	}
	if msgs[0].Source != SourceUser {
		t.Fatalf("real human msg should have Source=user, got %q", msgs[0].Source)
	}
	if msgs[1].Source != SourceCron {
		t.Fatalf("cron-fire msg should have Source=cron, got %q", msgs[1].Source)
	}
	if msgs[2].Source != SourceAssistant {
		t.Fatalf("assistant msg should have Source=assistant, got %q", msgs[2].Source)
	}
}

// TestMigrateMessagesAddSourceIdempotent: opening a DB that already has
// the new schema (which includes source) twice does not double-migrate.
// The migration's job on a fresh DB is a no-op (source already exists from
// CREATE TABLE); the bigger value lives in the legacy-DB rebuild covered
// by TestMigrateMessagesLegacyCronBackfill below.
func TestMigrateMessagesAddSourceIdempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "fresh.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Touch("sess", "ch", 1)
	if err := s.AppendUser("sess", "before", "u", "", SourceUser); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer s2.Close()
	msgs, _ := s2.RecentMessages("sess", 10)
	if len(msgs) != 1 || msgs[0].Source != SourceUser {
		t.Fatalf("row should round-trip with Source=user: %+v", msgs)
	}
}

// TestMigrateMessagesLegacyCronBackfill: a DB that started life with the
// pre-source schema (cron INTEGER column) must be backfilled correctly
// when source is added — cron=1 rows become source='cron', assistant rows
// become source='assistant', everything else defaults to 'user'.
func TestMigrateMessagesLegacyCronBackfill(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")

	// Stage 1: build a "legacy" DB by hand. Can't use Open because
	// Open's migration would immediately add source; emulate the old shape
	// directly via the modernc driver.
	createLegacyMessagesDB(t, dbPath)

	// Stage 2: reopen — migration must add source and backfill.
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	msgs, err := s.RecentMessages("sess", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(msgs))
	}
	if msgs[0].Source != SourceUser {
		t.Fatalf("legacy human row → user, got %q", msgs[0].Source)
	}
	if msgs[1].Source != SourceCron {
		t.Fatalf("legacy cron row → cron, got %q", msgs[1].Source)
	}
	if msgs[2].Source != SourceAssistant {
		t.Fatalf("legacy assistant row → assistant, got %q", msgs[2].Source)
	}
}

func createLegacyMessagesDB(t *testing.T, dbPath string) {
	t.Helper()
	// Open via Open to get the rest of the schema + migrations, then
	// destructively rebuild messages with the legacy shape and reseed.
	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, stmt := range []string{
		`DROP TABLE messages`,
		`CREATE TABLE messages (
		  id         INTEGER PRIMARY KEY AUTOINCREMENT,
		  session_id TEXT NOT NULL,
		  role       TEXT NOT NULL CHECK(role IN ('user','assistant')),
		  content    TEXT NOT NULL,
		  timestamp  INTEGER NOT NULL,
		  from_name  TEXT,
		  cron       INTEGER NOT NULL DEFAULT 0
		)`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("rebuild legacy: %v", err)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO sessions(id, channel_id, channel_type, created_at, updated_at) VALUES (?,?,?,?,?)`,
		"sess", "ch", 1, 1, 1); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	for _, row := range []struct {
		role, content, fromName string
		cron                    int
	}{
		{"user", "real human", "alice", 0},
		{"user", "cron fire", "cronbot", 1},
		{"assistant", "ok", "bot", 0},
	} {
		if _, err := s.db.Exec(
			`INSERT INTO messages(session_id, role, content, timestamp, from_name, cron) VALUES (?,?,?,?,?,?)`,
			"sess", row.role, row.content, 1, row.fromName, row.cron); err != nil {
			t.Fatalf("seed row: %v", err)
		}
	}
}

// TestMigrateMessagesAddFromUID proves a legacy messages table (no from_uid
// column, seeded by createLegacyMessagesDB) gets the column added by Open's
// migration, that legacy rows round-trip with an empty FromUID, that a new
// append persists/reads back its uid, and that re-opening is a no-op (the
// migration is idempotent).
func TestMigrateMessagesAddFromUID(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "octobuddy.db")
	createLegacyMessagesDB(t, dbPath) // messages table predates from_uid

	s, err := Open(dbPath) // runs migrateMessagesAddFromUID
	if err != nil {
		t.Fatalf("open after legacy: %v", err)
	}
	cols, err := messagesColumnSet(s.db)
	if _, has := cols["from_uid"]; err != nil || !has {
		t.Fatalf("from_uid column present = %v, %v; want true, nil", cols, err)
	}
	if err := s.AppendUser("sess", "with uid", "carol", "u:carol", SourceUser); err != nil {
		t.Fatal(err)
	}
	msgs, err := s.RecentMessages("sess", 10)
	if err != nil {
		t.Fatal(err)
	}
	last := msgs[len(msgs)-1] // freshly appended, round-trips uid + name
	if last.FromUID != "u:carol" || last.FromName != "carol" {
		t.Fatalf("new row = uid %q name %q, want u:carol/carol", last.FromUID, last.FromName)
	}
	if msgs[0].FromUID != "" { // legacy rows carry empty FromUID
		t.Fatalf("legacy row FromUID = %q, want empty", msgs[0].FromUID)
	}
	s.Close()

	s2, err := Open(dbPath) // re-open: migration must be a no-op, not an error
	if err != nil {
		t.Fatalf("re-open (idempotent migration): %v", err)
	}
	defer s2.Close()
}
