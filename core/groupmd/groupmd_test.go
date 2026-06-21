package groupmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func TestLoadGroupFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "g123.md", "  Be concise in this group.\n")

	l := New(dir)
	got, ok := l.Load("g123")
	if !ok {
		t.Fatal("expected group instructions, got none")
	}
	if got != "Be concise in this group." {
		t.Fatalf("content = %q, want trimmed body", got)
	}
}

func TestLoadMissingFile(t *testing.T) {
	l := New(t.TempDir())
	if got, ok := l.Load("nope"); ok || got != "" {
		t.Fatalf("missing file: got (%q, %v), want (\"\", false)", got, ok)
	}
}

func TestEmptyDirNeverLoads(t *testing.T) {
	l := New("")
	if got, ok := l.Load("g1"); ok || got != "" {
		t.Fatalf("empty dir: got (%q, %v), want (\"\", false)", got, ok)
	}
}

func TestEmptyChannelID(t *testing.T) {
	l := New(t.TempDir())
	if _, ok := l.Load(""); ok {
		t.Fatal("empty channelID must not load")
	}
}

func TestBlankFileIsNoInjection(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "g1.md", "   \n\t\n")
	l := New(dir)
	if got, ok := l.Load("g1"); ok || got != "" {
		t.Fatalf("whitespace-only file: got (%q, %v), want (\"\", false)", got, ok)
	}
}

func TestSizeCap(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("a", MaxBytes+5000)
	writeFile(t, dir, "g1.md", big)

	l := New(dir)
	got, ok := l.Load("g1")
	if !ok {
		t.Fatal("oversized file should still inject (truncated)")
	}
	if len(got) > MaxBytes+len(truncationNotice) {
		t.Fatalf("content length %d exceeds cap %d", len(got), MaxBytes+len(truncationNotice))
	}
	if !strings.HasSuffix(got, truncationNotice) {
		t.Fatalf("truncated content must end with notice, got tail %q", got[len(got)-40:])
	}
}

func TestThreadPrefersThreadFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "g1.md", "group level")
	writeFile(t, dir, "g1____t9.md", "thread level")

	l := New(dir)
	got, ok := l.Load("g1____t9")
	if !ok {
		t.Fatal("expected thread instructions")
	}
	if got != "thread level" {
		t.Fatalf("thread should prefer its own file, got %q", got)
	}
}

func TestThreadFallsBackToParent(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "g1.md", "group level")
	// No g1____t9.md present.

	l := New(dir)
	got, ok := l.Load("g1____t9")
	if !ok {
		t.Fatal("expected fallback to parent group file")
	}
	if got != "group level" {
		t.Fatalf("thread should fall back to parent, got %q", got)
	}
}

func TestThreadNoFilesNothing(t *testing.T) {
	l := New(t.TempDir())
	if _, ok := l.Load("g1____t9"); ok {
		t.Fatal("thread with no files must not load")
	}
}

func TestUnsafeIDRejected(t *testing.T) {
	dir := t.TempDir()
	// A traversal id must not read outside the dir even if such a file exists.
	writeFile(t, dir, "g1.md", "in dir")
	l := New(dir)
	for _, id := range []string{"../etc/passwd", "..", ".", "a/b", "a\\b"} {
		if _, ok := l.Load(id); ok {
			t.Fatalf("unsafe id %q must be rejected", id)
		}
	}
}

func TestDMGetsNothing(t *testing.T) {
	// The loader is channel-agnostic; the gateway only calls it for group/thread
	// channels. Here we assert the loader itself never reads a peer-uid keyed file
	// that wasn't authored — i.e. a plain uid with no <uid>.md yields nothing.
	l := New(t.TempDir())
	if _, ok := l.Load("peer-uid-123"); ok {
		t.Fatal("no file for this id should yield nothing")
	}
}

func TestGroupWorldWritableRefused(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "g1.md", "trusted?")
	if err := os.Chmod(p, 0o646); err != nil { // world-writable
		t.Fatalf("chmod: %v", err)
	}
	l := New(dir)
	if got, ok := l.Load("g1"); ok || got != "" {
		t.Fatalf("world-writable file must be refused, got (%q, %v)", got, ok)
	}
}

func TestCacheRefreshOnEdit(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "g1.md", "first")
	l := New(dir)
	if got, _ := l.Load("g1"); got != "first" {
		t.Fatalf("initial load = %q", got)
	}
	// Rewrite with new content + bump mtime so the cache's mtime check fires.
	if err := os.WriteFile(p, []byte("second"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if got, _ := l.Load("g1"); got != "second" {
		t.Fatalf("after edit load = %q, want refreshed content", got)
	}
}

// TestPermCheckRunsBeforeCacheHotPath is the regression for the round-7 F2
// bug: the cache hot path returned cached content when (mtime, size) matched,
// but the world-writable defense only ran on the slow path. A `chmod 0666`
// that doesn't touch mtime kept returning the previously-cached content
// indefinitely — silently bypassing the defense-in-depth guard.
func TestPermCheckRunsBeforeCacheHotPath(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "g1.md")
	if err := os.WriteFile(p, []byte("good content"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	l := New(dir)
	if got, _ := l.Load("g1"); got != "good content" {
		t.Fatalf("initial load = %q", got)
	}
	// Operator (or attacker who got the file open) flips the mode without
	// changing mtime or size. The next Load must refuse.
	if err := os.Chmod(p, 0o666); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if got, _ := l.Load("g1"); got != "" {
		t.Fatalf("Load after chmod 0666 must return empty, got %q (perm check bypassed by cache hot path)", got)
	}
}

// TestSymlinkRefused is the regression for round-8 F2-Sec: a symlink in
// groupConfigDir whose target is 0600 would otherwise pass the
// world-writable perm check (Stat follows the link) and silently inject
// attacker-controlled content as [Group instructions]. The loader must
// refuse symlinks regardless of the target's mode.
func TestSymlinkRefused(t *testing.T) {
	dir := t.TempDir()
	// Real file (could even be operator-trusted content elsewhere).
	target := filepath.Join(t.TempDir(), "evil.md")
	if err := os.WriteFile(target, []byte("agent-controlled content"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	// Symlink at the groupConfigDir entry.
	linkPath := filepath.Join(dir, "g1.md")
	if err := os.Symlink(target, linkPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	l := New(dir)
	got, ok := l.Load("g1")
	if ok || got != "" {
		t.Errorf("symlinked groupmd entry must be refused, got ok=%v content=%q", ok, got)
	}
}
