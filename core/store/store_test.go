package store

import (
	"path/filepath"
	"testing"
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
