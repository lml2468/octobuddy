package safepath

import (
	"errors"
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

// TestSafeOpenRefusesSymlink proves the dirfd-walk refuses a symlinked
// final component AND a symlinked intermediate directory. Replaces the
// AssertNoSymlinkEscape test (helper deleted in R18).
func TestSafeOpenRefusesSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("leak"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Leaf symlink — refused.
	link := filepath.Join(root, "leak.txt")
	if err := os.Symlink(filepath.Join(outside, "secret"), link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := SafeOpen(root, "leak.txt"); err == nil {
		t.Fatal("SafeOpen on a symlink should error")
	} else if !errors.Is(err, ErrSymlink) {
		t.Errorf("expected ErrSymlink, got %v", err)
	}

	// Intermediate-dir symlink — refused too (the structural close we wanted).
	subLink := filepath.Join(root, "sub")
	_ = os.Symlink(outside, subLink)
	if _, err := SafeOpen(root, "sub/secret"); err == nil {
		t.Fatal("SafeOpen through a symlinked subdir should error")
	} else if !errors.Is(err, ErrSymlink) {
		t.Errorf("expected ErrSymlink for sub-path, got %v", err)
	}
}

// TestSafeWriteAtomicReplacesSymlink: an existing symlink at the
// destination is refused (operator learns about tampering) rather than
// silently overwritten with a regular file.
func TestSafeWriteRefusesSymlinkLeaf(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "target")
	_ = os.WriteFile(outside, []byte("orig"), 0o600)
	link := filepath.Join(root, "file.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := SafeWrite(root, "file.txt", []byte("clobber"), 0o600); err == nil {
		t.Fatal("SafeWrite over a symlink leaf should error")
	} else if !errors.Is(err, ErrSymlink) {
		t.Errorf("expected ErrSymlink, got %v", err)
	}
	// And the symlink target must be untouched.
	b, _ := os.ReadFile(outside)
	if string(b) != "orig" {
		t.Errorf("symlink target was modified: %q", string(b))
	}
}

// TestSafeWriteAtomic proves a clean write commits via temp+rename.
func TestSafeWriteAtomic(t *testing.T) {
	root := t.TempDir()
	if err := SafeMkdirAll(root, "a/b", 0o755); err != nil {
		t.Fatalf("SafeMkdirAll: %v", err)
	}
	if err := SafeWrite(root, "a/b/c.txt", []byte("hi"), 0o600); err != nil {
		t.Fatalf("SafeWrite: %v", err)
	}
	b, err := SafeRead(root, "a/b/c.txt", 0)
	if err != nil {
		t.Fatalf("SafeRead: %v", err)
	}
	if string(b) != "hi" {
		t.Errorf("round-trip mismatch: %q", string(b))
	}
	exerciseRemovalExports(t, root)
}

func exerciseRemovalExports(t *testing.T, root string) {
	t.Helper()

	if _, err := SafeReadDir(root, "a/b"); err != nil {
		t.Fatalf("SafeReadDir: %v", err)
	}
	if !SafeExists(root, "a/b/c.txt") {
		t.Fatal("SafeExists should see written file")
	}
	if err := SafeRemove(root, "a/b/c.txt"); err != nil {
		t.Fatalf("SafeRemove: %v", err)
	}
	if err := SafeWrite(root, "a/b/d.txt", []byte("bye"), 0o600); err != nil {
		t.Fatalf("SafeWrite second file: %v", err)
	}
	if err := SafeRemoveAll(root, "a"); err != nil {
		t.Fatalf("SafeRemoveAll: %v", err)
	}
	abs := filepath.Join(root, "abs")
	if err := SafeMkdirAllAbs(abs, 0o755); err != nil {
		t.Fatalf("SafeMkdirAllAbs: %v", err)
	}
	if err := SafeRemoveAllAbs(abs); err != nil {
		t.Fatalf("SafeRemoveAllAbs: %v", err)
	}
}
