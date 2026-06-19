package safepath

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidSlug(t *testing.T) {
	ok := []string{"foo", "foo-bar", "foo_bar", "foo.bar", "a1", "SKILL"}
	bad := []string{"", ".", "..", "foo/bar", "foo\\bar", "../etc", "foo bar", "foo;rm"}
	for _, s := range ok {
		if !ValidSlug(s) {
			t.Errorf("ValidSlug(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if ValidSlug(s) {
			t.Errorf("ValidSlug(%q) = true, want false", s)
		}
	}
}

func TestResolveLexicalRejectsEscape(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	bad := []string{"", "/abs", "../x", "a/../../b", "../../etc/passwd"}
	for _, rel := range bad {
		if _, err := ResolveLexical(root, rel); err == nil {
			t.Errorf("ResolveLexical(%q) should error", rel)
		}
	}
	good, err := ResolveLexical(root, "a/b/c.txt")
	if err != nil {
		t.Fatalf("ResolveLexical clean path: %v", err)
	}
	if good != filepath.Join(root, "a", "b", "c.txt") {
		t.Errorf("unexpected resolved path %q", good)
	}
}

// TestAssertNoSymlinkEscape proves a symlinked intermediate component is caught
// even when the lexical path looks clean — the gap that left skills/workflows
// weaker than workspace.
func TestAssertNoSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	// A plain new file under root is allowed (parent chain is real).
	full := filepath.Join(root, "bundle", "file.txt")
	if err := AssertNoSymlinkEscape(root, full, true); err != nil {
		t.Fatalf("clean path should pass: %v", err)
	}

	// Plant a symlinked subdir inside root pointing outside, then a write through
	// it must be rejected.
	link := filepath.Join(root, "evil")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	escaping := filepath.Join(link, "pwned.txt")
	if err := AssertNoSymlinkEscape(root, escaping, true); err == nil {
		t.Fatal("write through symlinked subdir should be rejected")
	}
}
