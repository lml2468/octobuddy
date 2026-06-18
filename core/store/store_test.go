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

func TestListSessions(t *testing.T) {
	s := openTemp(t)
	clk := time.Unix(1000, 0)
	s.SetClock(func() time.Time { return clk })

	// older session "a", then newer "b" (updated_at advances with the clock).
	if _, err := s.GetOrCreate("a", "a", 2); err != nil {
		t.Fatal(err)
	}
	_ = s.AppendUser("a", "hi from a", "alice")
	_ = s.AppendAssistant("a", "a-reply", "bot")

	clk = time.Unix(2000, 0)
	if _, err := s.GetOrCreate("b", "b", 2); err != nil {
		t.Fatal(err)
	}
	_ = s.AppendUser("b", "hi from b", "bob")

	// "c" has no messages: preview should be empty, still listed.
	clk = time.Unix(1500, 0)
	if _, err := s.GetOrCreate("c", "c", 2); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 sessions, got %d (%+v)", len(got), got)
	}
	// newest updated_at first: b(2000), c(1500), a(1000)
	if got[0].Key != "b" || got[1].Key != "c" || got[2].Key != "a" {
		t.Fatalf("order wrong: %s, %s, %s", got[0].Key, got[1].Key, got[2].Key)
	}
	// preview is the latest message
	if got[0].Preview != "hi from b" || got[0].LastRole != RoleUser {
		t.Fatalf("b preview wrong: %+v", got[0])
	}
	if got[2].Preview != "a-reply" || got[2].LastRole != RoleAssistant {
		t.Fatalf("a preview should be its newest message: %+v", got[2])
	}
	if got[1].Preview != "" {
		t.Fatalf("c has no messages, preview should be empty: %q", got[1].Preview)
	}
}

func TestTokenUsageAccumulates(t *testing.T) {
	s := openTemp(t)

	// No turns yet → zero value.
	if u, err := s.Usage(); err != nil || u.Turns != 0 || u.InputTokens != 0 {
		t.Fatalf("fresh usage should be zero: %+v err=%v", u, err)
	}

	// All-zero deltas are a no-op (don't advance the turn counter).
	if err := s.AddUsage(0, 0, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.Usage(); u.Turns != 0 {
		t.Fatalf("zero usage must not advance turns: %+v", u)
	}

	if err := s.AddUsage(100, 20, 80, 200, 0.01); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUsage(50, 10, 40, 0, 0.005); err != nil {
		t.Fatal(err)
	}
	u, err := s.Usage()
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if u.InputTokens != 150 || u.OutputTokens != 30 || u.CachedTokens != 120 {
		t.Fatalf("tokens not accumulated: %+v", u)
	}
	if u.CacheWriteTokens != 200 {
		t.Fatalf("cache-write not accumulated: %+v", u)
	}
	if u.Turns != 2 {
		t.Fatalf("turns = %d, want 2", u.Turns)
	}
	if u.CostUSD < 0.0149 || u.CostUSD > 0.0151 {
		t.Fatalf("cost not accumulated: %v", u.CostUSD)
	}
}

func TestTokenUsageByDateRange(t *testing.T) {
	s := openTemp(t)
	// Pin "now" to a fixed wall clock so day bucketing is deterministic.
	now := time.Date(2026, 6, 18, 10, 0, 0, 0, time.Local)
	cur := now
	s.SetClock(func() time.Time { return cur })

	// Three days of usage: 10 days ago, 3 days ago, today.
	cur = now.AddDate(0, 0, -10)
	_ = s.AddUsage(1000, 0, 0, 0, 1.0)
	cur = now.AddDate(0, 0, -3)
	_ = s.AddUsage(100, 0, 0, 0, 0.1)
	cur = now
	_ = s.AddUsage(10, 0, 0, 0, 0.01)

	// All = everything.
	if u, _ := s.Usage(); u.InputTokens != 1110 || u.Turns != 3 {
		t.Fatalf("all: %+v want 1110/3", u)
	}
	// Last 7 days (since 7 days ago midnight) = the -3d and today rows.
	since7 := localMidnight(now.AddDate(0, 0, -7))
	if u, _ := s.UsageSince(since7); u.InputTokens != 110 || u.Turns != 2 {
		t.Fatalf("7d: %+v want 110/2", u)
	}
	// Today only.
	sinceToday := localMidnight(now)
	if u, _ := s.UsageSince(sinceToday); u.InputTokens != 10 || u.Turns != 1 {
		t.Fatalf("today: %+v want 10/1", u)
	}
}

func TestTokenUsageMigratesLegacyAggregate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")
	// Build a DB with the OLD single-row token_usage table + a row.
	pre, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pre.db.Exec(`DROP TABLE token_usage_daily`); err != nil {
		t.Fatal(err)
	}
	if _, err := pre.db.Exec(`CREATE TABLE token_usage (id INTEGER PRIMARY KEY CHECK(id=1),
		input_tokens INTEGER, output_tokens INTEGER, cached_tokens INTEGER,
		cache_write_tokens INTEGER, cost_usd REAL, turns INTEGER, updated_at INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pre.db.Exec(`INSERT INTO token_usage VALUES(1, 500, 60, 40, 20, 2.5, 9, 123)`); err != nil {
		t.Fatal(err)
	}
	pre.Close()

	// Reopen: migration should fold the legacy row into the day=0 bucket.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("reopen/migrate: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	u, err := s.Usage()
	if err != nil {
		t.Fatal(err)
	}
	if u.InputTokens != 500 || u.OutputTokens != 60 || u.Turns != 9 || u.CacheWriteTokens != 20 {
		t.Fatalf("legacy not migrated into All: %+v", u)
	}
	// Excluded from any dated range (its turns predate per-day tracking).
	if u2, _ := s.UsageSince(1); u2.Turns != 0 {
		t.Fatalf("legacy day=0 bucket must be excluded from dated ranges: %+v", u2)
	}
	// Old table is gone.
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='token_usage'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("old token_usage table should be dropped (n=%d err=%v)", n, err)
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
