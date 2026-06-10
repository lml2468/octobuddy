package store

import (
	"path/filepath"
	"testing"
)

func TestResumeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "ccd.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	if got, _ := s.Resume("group:123"); got != "" {
		t.Fatalf("expected empty for unknown key, got %q", got)
	}
	if err := s.SaveResume("group:123", "claude", "sess-abc", 1000); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := s.Resume("group:123")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got != "sess-abc" {
		t.Fatalf("got %q want sess-abc", got)
	}
	// Upsert replaces.
	if err := s.SaveResume("group:123", "claude", "sess-def", 2000); err != nil {
		t.Fatalf("re-save: %v", err)
	}
	if got, _ := s.Resume("group:123"); got != "sess-def" {
		t.Fatalf("upsert failed: got %q", got)
	}
}
