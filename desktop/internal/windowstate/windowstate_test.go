package windowstate

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadMissingReturnsZero: fresh install (no window.json) returns the
// zero state and a nil error so callers can blindly accept defaults.
func TestLoadMissingReturnsZero(t *testing.T) {
	withHome(t, t.TempDir())
	got, err := Load()
	if err != nil {
		t.Fatalf("Load fresh install: %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("expected zero state, got %+v", got)
	}
}

// TestSaveLoadRoundTrip is the basic persistence guarantee.
func TestSaveLoadRoundTrip(t *testing.T) {
	withHome(t, t.TempDir())
	want := State{X: 100, Y: 200, Width: 1200, Height: 800}
	if err := Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != want {
		t.Fatalf("Load got %+v, want %+v", got, want)
	}
}

// TestLoadCorruptedReturnsZero: a hand-corrupted file (e.g. partial write
// from a crash) does NOT crash the app; Load surfaces the parse error
// but the caller falls back to defaults via IsZero check.
func TestLoadCorruptedReturnsZero(t *testing.T) {
	home := t.TempDir()
	withHome(t, home)
	dir := filepath.Join(home, ".octobuddy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "window.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err == nil {
		t.Fatalf("expected decode error on corrupt file, got nil")
	}
	if !got.IsZero() {
		t.Fatalf("expected zero state on decode error, got %+v", got)
	}
}

// withHome overrides $HOME for the test (process-wide; tests serialize on
// this since UserHomeDir reads env). t.Setenv handles cleanup.
func withHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
}
