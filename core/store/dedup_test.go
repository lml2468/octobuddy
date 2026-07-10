package store

import (
	"path/filepath"
	"testing"
	"time"
)

// openTestStore opens a fresh store in a temp dir for dedup tests.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "dedup.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestMarkSeenMessageFirstThenDuplicate: the first sighting of a message id
// reports firstSeen=true (caller dispatches); a second reports false (drop).
func TestMarkSeenMessageFirstThenDuplicate(t *testing.T) {
	st := openTestStore(t)
	first, err := st.MarkSeenMessage("m1")
	if err != nil || !first {
		t.Fatalf("first sighting: firstSeen=%v err=%v, want true/nil", first, err)
	}
	dup, err := st.MarkSeenMessage("m1")
	if err != nil || dup {
		t.Fatalf("duplicate: firstSeen=%v err=%v, want false/nil", dup, err)
	}
}

// TestMarkSeenMessageDistinctIDs: different ids are each first-seen.
func TestMarkSeenMessageDistinctIDs(t *testing.T) {
	st := openTestStore(t)
	for _, id := range []string{"a", "b", "c"} {
		if first, err := st.MarkSeenMessage(id); err != nil || !first {
			t.Fatalf("id %q: firstSeen=%v err=%v, want true/nil", id, first, err)
		}
	}
}

// TestMarkSeenMessagePersistsAcrossReopen proves the dedup survives a restart —
// the exact crash-recovery window an in-memory set could not cover.
func TestMarkSeenMessagePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if first, _ := st.MarkSeenMessage("m1"); !first {
		t.Fatal("first mark should be firstSeen")
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if first, _ := st2.MarkSeenMessage("m1"); first {
		t.Fatal("after reopen, the same id must NOT be first-seen (dedup persisted)")
	}
}

// TestReapSeenMessagesDropsOld: rows older than the retention window are reaped;
// fresh rows survive. Uses SetClock for deterministic time.
func TestReapSeenMessagesDropsOld(t *testing.T) {
	st := openTestStore(t)
	base := time.Unix(1_700_000_000, 0)
	st.SetClock(func() time.Time { return base })
	if _, err := st.MarkSeenMessage("old"); err != nil {
		t.Fatal(err)
	}
	// Advance well past retention, insert a fresh row, then reap the old one.
	st.SetClock(func() time.Time { return base.Add(48 * time.Hour) })
	if _, err := st.MarkSeenMessage("fresh"); err != nil {
		t.Fatal(err)
	}
	n, err := st.ReapSeenMessages(24 * time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("reaped %d rows, want 1 (only the old one)", n)
	}
	// old is gone → first-seen again; fresh still deduped.
	if first, _ := st.MarkSeenMessage("old"); !first {
		t.Error("old id should be first-seen again after reap")
	}
	if first, _ := st.MarkSeenMessage("fresh"); first {
		t.Error("fresh id must still be deduped (not reaped)")
	}
}
