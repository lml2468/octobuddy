package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestTokenUsageAccumulates(t *testing.T) {
	s := openTemp(t)
	assertFreshUsage(t, s)
	assertZeroUsageNoTurn(t, s)
	addUsageDeltas(t, s)
	assertAccumulatedUsage(t, s)
}

func assertFreshUsage(t *testing.T, s *Store) {
	t.Helper()

	if u, err := s.Usage(); err != nil || u.Turns != 0 || u.InputTokens != 0 {
		t.Fatalf("fresh usage should be zero: %+v err=%v", u, err)
	}
}

func assertZeroUsageNoTurn(t *testing.T, s *Store) {
	t.Helper()

	if err := s.AddUsage(0, 0, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.Usage(); u.Turns != 0 {
		t.Fatalf("zero usage must not advance turns: %+v", u)
	}
}

func addUsageDeltas(t *testing.T, s *Store) {
	t.Helper()

	if err := s.AddUsage(100, 20, 80, 200, 0.01); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUsage(50, 10, 40, 0, 0.005); err != nil {
		t.Fatal(err)
	}
}

func assertAccumulatedUsage(t *testing.T, s *Store) {
	t.Helper()

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
	createLegacyTokenUsageDB(t, path)

	s, err := Open(path)
	if err != nil {
		t.Fatalf("reopen/migrate: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	assertLegacyTokenUsageMigrated(t, s)
}

func createLegacyTokenUsageDB(t *testing.T, path string) {
	t.Helper()

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
}

func assertLegacyTokenUsageMigrated(t *testing.T, s *Store) {
	t.Helper()

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
