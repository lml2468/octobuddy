package store

import (
	"path/filepath"
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
	assertUnknownResume(t, s)
	assertResumeUpsert(t, s)
	assertSecondAgentResume(t, s)
	assertClearResume(t, s)
}

func assertUnknownResume(t *testing.T, s *Store) {
	t.Helper()

	if got, _ := s.Resume("group:123", "claude"); got != "" {
		t.Fatalf("expected empty for unknown key, got %q", got)
	}
}

func assertResumeUpsert(t *testing.T, s *Store) {
	t.Helper()

	saveAndAssertResume(t, s, "group:123", "claude", "sess-abc", "save: %v", "got %q want sess-abc")
	// Saving the same (key, agent) replaces.
	saveAndAssertResume(t, s, "group:123", "claude", "sess-xyz", "re-save: %v", "upsert (same agent) failed: got %q")
}

func assertSecondAgentResume(t *testing.T, s *Store) {
	t.Helper()

	saveAndAssertResume(t, s, "group:123", "codex", "thr-def", "save second agent: %v", "codex resume not stored: got %q")
	if got, _ := s.Resume("group:123", "claude"); got != "sess-xyz" {
		t.Fatalf("claude resume id should be unchanged after codex save: got %q", got)
	}
}

func assertClearResume(t *testing.T, s *Store) {
	t.Helper()

	if err := s.ClearResume("group:123"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got, _ := s.Resume("group:123", "claude"); got != "" {
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
	_ = s.AppendUser("g:1", "first", "alice", "u:alice", SourceUser)
	_ = s.AppendAssistant("g:1", "reply1", "bot", "")
	_ = s.AppendUser("g:1", "second", "bob", "u:bob", SourceUser)
	_ = s.AppendAssistant("g:1", "reply2", "bot", "")

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

// TestAppendAssistantStepsRoundTrip proves an assistant row's step JSON persists
// and reads back verbatim, while a step-less reply round-trips as "".
func TestAppendAssistantStepsRoundTrip(t *testing.T) {
	s := openTemp(t)
	if _, err := s.GetOrCreate("g:1", "1", 2); err != nil {
		t.Fatal(err)
	}
	steps := `[{"kind":"tool","text":"Read(README.md)"},{"kind":"thinking","text":"thinking…"}]`
	if err := s.AppendAssistant("g:1", "with steps", "bot", steps); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendAssistant("g:1", "no steps", "bot", ""); err != nil {
		t.Fatal(err)
	}
	msgs, err := s.RecentMessages("g:1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if msgs[0].Steps != steps {
		t.Fatalf("steps round-trip = %q, want %q", msgs[0].Steps, steps)
	}
	if msgs[1].Steps != "" {
		t.Fatalf("step-less reply Steps = %q, want empty", msgs[1].Steps)
	}
}

func TestListSessions(t *testing.T) {
	s := openTemp(t)
	got := createListedSessions(t, s)
	assertListedSessionOrder(t, got)
	assertListedSessionPreviews(t, got)
}

func createListedSessions(t *testing.T, s *Store) []SessionSummary {
	t.Helper()

	clk := time.Unix(1000, 0)
	s.SetClock(func() time.Time { return clk })

	if _, err := s.GetOrCreate("a", "a", 2); err != nil {
		t.Fatal(err)
	}
	_ = s.AppendUser("a", "hi from a", "alice", "u:alice", SourceUser)
	_ = s.AppendAssistant("a", "a-reply", "bot", "")

	clk = time.Unix(2000, 0)
	if _, err := s.GetOrCreate("b", "b", 2); err != nil {
		t.Fatal(err)
	}
	_ = s.AppendUser("b", "hi from b", "bob", "u:bob", SourceUser)

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
	return got
}

func assertListedSessionOrder(t *testing.T, got []SessionSummary) {
	t.Helper()

	if got[0].Key != "b" || got[1].Key != "c" || got[2].Key != "a" {
		t.Fatalf("order wrong: %s, %s, %s", got[0].Key, got[1].Key, got[2].Key)
	}
}

func assertListedSessionPreviews(t *testing.T, got []SessionSummary) {
	t.Helper()

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
