package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "mlclaw.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestResumeRoundTrip(t *testing.T) {
	s := openTemp(t)
	if got, _ := s.Resume("group:123"); got != "" {
		t.Fatalf("expected empty for unknown key, got %q", got)
	}
	if err := s.SaveResume("group:123", "claude", "sess-abc"); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got, _ := s.Resume("group:123"); got != "sess-abc" {
		t.Fatalf("got %q want sess-abc", got)
	}
	// upsert replaces
	if err := s.SaveResume("group:123", "codex", "thr-def"); err != nil {
		t.Fatalf("re-save: %v", err)
	}
	if got, _ := s.Resume("group:123"); got != "thr-def" {
		t.Fatalf("upsert failed: got %q", got)
	}
	// clear
	if err := s.ClearResume("group:123"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got, _ := s.Resume("group:123"); got != "" {
		t.Fatalf("clear failed: got %q", got)
	}
}

func TestSessionGetOrCreate(t *testing.T) {
	s := openTemp(t)
	sess, err := s.GetOrCreate("dm:peer1", "peer1", 1)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sess.ID != "dm:peer1" || sess.ChannelType != 1 {
		t.Fatalf("unexpected session %+v", sess)
	}
	// idempotent
	again, err := s.GetOrCreate("dm:peer1", "peer1", 1)
	if err != nil || again.CreatedAt != sess.CreatedAt {
		t.Fatalf("getOrCreate not idempotent: %+v vs %+v", again, sess)
	}
}

func TestMessagesChronologicalAndLimited(t *testing.T) {
	s := openTemp(t)
	if _, err := s.GetOrCreate("g:1", "1", 2); err != nil {
		t.Fatal(err)
	}
	_ = s.AppendUser("g:1", "first", "alice")
	_ = s.AppendAssistant("g:1", "reply1", "bot")
	_ = s.AppendUser("g:1", "second", "bob")
	_ = s.AppendAssistant("g:1", "reply2", "bot")

	msgs, err := s.RecentMessages("g:1", 3)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("want 3 (limited), got %d", len(msgs))
	}
	// chronological (oldest first) — limit takes the 3 most recent
	if msgs[0].Content != "reply1" || msgs[2].Content != "reply2" {
		t.Fatalf("order wrong: %+v", msgs)
	}
	if msgs[0].Role != RoleAssistant || msgs[1].Role != RoleUser {
		t.Fatalf("roles wrong: %+v", msgs)
	}
}

func TestCleanupExpired(t *testing.T) {
	s := openTemp(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return base })

	// old session + message
	if _, err := s.GetOrCreate("old", "c", 1); err != nil {
		t.Fatal(err)
	}
	_ = s.AppendUser("old", "stale", "")
	_ = s.SaveResume("old", "claude", "sess-old")

	// advance 8 days, create a fresh session
	s.SetClock(func() time.Time { return base.Add(8 * 24 * time.Hour) })
	if _, err := s.GetOrCreate("new", "c", 1); err != nil {
		t.Fatal(err)
	}

	n, err := s.CleanupExpired(DefaultTTL)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 expired session, got %d", n)
	}
	// old resume gone (cascade + explicit), messages cascade-deleted
	if got, _ := s.Resume("old"); got != "" {
		t.Fatalf("expired resume should be gone, got %q", got)
	}
	msgs, _ := s.RecentMessages("old", 10)
	if len(msgs) != 0 {
		t.Fatalf("expired messages should cascade-delete, got %d", len(msgs))
	}
	// new survives
	if _, err := s.GetOrCreate("new", "c", 1); err != nil {
		t.Fatalf("new session should survive: %v", err)
	}
}

// TestCleanupExpiredCascadesAcrossPooledConnections is the MLT-33 regression
// guard. PRAGMA foreign_keys is connection-scoped, and database/sql pools
// connections, so if FK enforcement isn't set on every pooled connection the
// ON DELETE CASCADE on messages silently no-ops on whatever connection happens
// to run the session DELETE — orphaning message rows that then never expire.
// By pinning one connection out of a widened pool it steers CleanupExpired's
// DELETE toward a different connection than the one that wrote the rows. This is
// best-effort, not guaranteed (database/sql may still hand the idle writer back);
// what makes it a reliable guard is that pre-fix the writes spread across
// FK-off connections, so the cascade was overwhelmingly likely to no-op and
// leave an orphan that fails the assertion below.
func TestCleanupExpiredCascadesAcrossPooledConnections(t *testing.T) {
	s := openTemp(t)
	// Allow the pool to hold more than one connection so we can pin one open
	// and force the cleanup onto a second, freshly opened connection.
	s.db.SetMaxOpenConns(3)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return base })

	if _, err := s.GetOrCreate("old", "c", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendUser("old", "stale", "alice"); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	// Pin a connection out of the pool so the next statement is steered onto
	// another (best-effort — see the doc comment).
	pinned, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatalf("pin conn: %v", err)
	}
	defer pinned.Close()
	// Touch the pinned connection so it is genuinely checked out and busy.
	if err := pinned.PingContext(ctx); err != nil {
		t.Fatalf("ping pinned: %v", err)
	}

	s.SetClock(func() time.Time { return base.Add(8 * 24 * time.Hour) })
	n, err := s.CleanupExpired(DefaultTTL)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 expired session, got %d", n)
	}

	// The orphan check: query messages directly so a surviving row is caught
	// even though its parent session is gone.
	var orphans int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE session_id='old'`).Scan(&orphans); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if orphans != 0 {
		t.Fatalf("FK cascade did not fire across pooled connections: %d orphaned message rows remain", orphans)
	}
}

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
