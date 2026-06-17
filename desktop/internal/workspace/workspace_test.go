package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lml2468/xclaw/core/sandbox"
)

// setHome points workspace.Dir() (os.UserHomeDir) at a temp dir on every OS:
// UserHomeDir reads $HOME on unix but %USERPROFILE% on Windows, so set both.
func setHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

// sandboxDir builds the on-disk DM sandbox dir for a session under a temp HOME
// and returns its absolute path (created).
func sandboxDir(t *testing.T, botID, sessionKey string) string {
	t.Helper()
	home := setHome(t)
	dir := filepath.Join(home, ".xclaw", botID, "workspace",
		sandbox.SessionDirName(sandbox.SessionCtx{Kind: sandbox.KindDM, SessionKey: sessionKey}))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMissingWorkspaceIsEmptyTree(t *testing.T) {
	setHome(t)
	tree, err := Tree("bot1", "never-ran")
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if tree == nil || !tree.IsDir || len(tree.Children) != 0 {
		t.Fatalf("want empty non-nil root, got %+v", tree)
	}
}

func TestTreeShapeDirsFirst(t *testing.T) {
	dir := sandboxDir(t, "bot1", "u1")
	write(t, filepath.Join(dir, "readme.md"), "# hi")
	write(t, filepath.Join(dir, "src", "main.go"), "package main")
	write(t, filepath.Join(dir, "src", "util.go"), "package main")

	tree, err := Tree("bot1", "u1")
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if len(tree.Children) != 2 {
		t.Fatalf("want 2 top-level entries, got %d (%+v)", len(tree.Children), tree.Children)
	}
	// Dir ("src") sorts before file ("readme.md").
	if !tree.Children[0].IsDir || tree.Children[0].Name != "src" {
		t.Fatalf("dirs must sort first: %+v", tree.Children[0])
	}
	if tree.Children[0].Path != "src" || len(tree.Children[0].Children) != 2 {
		t.Fatalf("src should have 2 children with rel paths: %+v", tree.Children[0])
	}
	if tree.Children[0].Children[0].Path != "src/main.go" {
		t.Fatalf("nested rel path wrong: %q", tree.Children[0].Children[0].Path)
	}
}

func TestDotClaudeNotDescended(t *testing.T) {
	dir := sandboxDir(t, "bot1", "u1")
	write(t, filepath.Join(dir, ".claude", "skills", "x", "SKILL.md"), "secret catalog")

	tree, err := Tree("bot1", "u1")
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	var claude *Node
	for _, c := range tree.Children {
		if c.Name == ".claude" {
			claude = c
		}
	}
	if claude == nil || !claude.IsDir {
		t.Fatalf(".claude should appear as a dir node: %+v", tree.Children)
	}
	if claude.Children != nil {
		t.Fatalf(".claude must not be descended, got children: %+v", claude.Children)
	}
}

func TestSymlinkNotFollowed(t *testing.T) {
	dir := sandboxDir(t, "bot1", "u1")
	// A directory outside the sandbox that a symlink would escape into.
	outside := t.TempDir()
	write(t, filepath.Join(outside, "secret.txt"), "do not surface")
	link := filepath.Join(dir, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	tree, err := Tree("bot1", "u1")
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	var esc *Node
	for _, c := range tree.Children {
		if c.Name == "escape" {
			esc = c
		}
	}
	if esc == nil {
		t.Fatal("symlink entry missing from tree")
	}
	if esc.IsDir || esc.Children != nil {
		t.Fatalf("symlink must be a non-descended leaf, got %+v", esc)
	}
	// File() must refuse to read through the symlink.
	if _, err := File("bot1", "u1", "escape/secret.txt"); err == nil {
		t.Fatal("File must not read through a symlink")
	}
}

func TestFileTextAndTruncation(t *testing.T) {
	dir := sandboxDir(t, "bot1", "u1")
	write(t, filepath.Join(dir, "note.txt"), "hello world")
	big := strings.Repeat("a", maxTextBytes+500)
	write(t, filepath.Join(dir, "big.txt"), big)

	fc, err := File("bot1", "u1", "note.txt")
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if fc.Encoding != "utf8" || fc.Content != "hello world" || fc.Truncated {
		t.Fatalf("text read wrong: %+v", fc)
	}

	bf, err := File("bot1", "u1", "big.txt")
	if err != nil {
		t.Fatalf("File big: %v", err)
	}
	if !bf.Truncated || len(bf.Content) != maxTextBytes {
		t.Fatalf("expected truncation to %d bytes, got %d (trunc=%v)", maxTextBytes, len(bf.Content), bf.Truncated)
	}
	if bf.Size != int64(len(big)) {
		t.Fatalf("Size should be the real on-disk size %d, got %d", len(big), bf.Size)
	}
}

func TestFilePDFMimeBase64(t *testing.T) {
	dir := sandboxDir(t, "bot1", "u1")
	// %PDF header → application/pdf by extension; binary → base64.
	write(t, filepath.Join(dir, "doc.pdf"), "%PDF-1.7\n\x00binary\x00")

	fc, err := File("bot1", "u1", "doc.pdf")
	if err != nil {
		t.Fatalf("File pdf: %v", err)
	}
	if fc.Mime != "application/pdf" || fc.Encoding != "base64" {
		t.Fatalf("pdf must be base64 application/pdf, got %+v", fc)
	}
	if fc.Kind != "pdf" {
		t.Fatalf("pdf Kind must be \"pdf\", got %q", fc.Kind)
	}
}

func TestFileKindClassification(t *testing.T) {
	cases := []struct {
		mime    string
		textual bool
		want    string
	}{
		{"text/markdown", true, "markdown"},
		{"text/html", true, "html"},
		{"application/pdf", false, "pdf"},
		{"image/png", false, "image"},
		{"image/svg+xml", true, "text"}, // svg is utf8 → text, not image
		{"text/x-go", true, "text"},
		{"application/octet-stream", false, "binary"},
	}
	for _, c := range cases {
		if got := kindOf(c.mime, c.textual); got != c.want {
			t.Errorf("kindOf(%q, %v) = %q, want %q", c.mime, c.textual, got, c.want)
		}
	}
}

func TestFileBinaryUsesLargerCap(t *testing.T) {
	dir := sandboxDir(t, "bot1", "u1")
	// A binary file between the text cap (1 MiB) and binary cap (8 MiB) must NOT be
	// truncated — base64 previews need the whole file for a valid data-URL. A NUL
	// byte forces non-textual classification regardless of extension.
	n := maxTextBytes + 1<<20 // 2 MiB, well under the 8 MiB binary cap
	body := make([]byte, n)
	body[0] = 0 // NUL → binary
	write(t, filepath.Join(dir, "blob.bin"), string(body))

	fc, err := File("bot1", "u1", "blob.bin")
	if err != nil {
		t.Fatalf("File bin: %v", err)
	}
	if fc.Encoding != "base64" {
		t.Fatalf("binary must be base64, got %+v", fc.Encoding)
	}
	if fc.Truncated {
		t.Fatalf("a %d-byte binary is under the binary cap and must not be truncated", n)
	}
}

func TestFileImageBase64(t *testing.T) {
	dir := sandboxDir(t, "bot1", "u1")
	// 1x1 PNG header bytes are enough to classify as image/png by extension.
	write(t, filepath.Join(dir, "pic.png"), "\x89PNG\r\n\x1a\n\x00\x00")

	fc, err := File("bot1", "u1", "pic.png")
	if err != nil {
		t.Fatalf("File png: %v", err)
	}
	if fc.Encoding != "base64" || fc.Mime != "image/png" {
		t.Fatalf("image must be base64 image/png, got %+v", fc)
	}
}

func TestFileRejectsTraversalAndDirs(t *testing.T) {
	dir := sandboxDir(t, "bot1", "u1")
	write(t, filepath.Join(dir, "sub", "a.txt"), "x")

	for _, bad := range []string{"../escape", "/etc/passwd", "sub/../../x", ""} {
		if _, err := File("bot1", "u1", bad); err == nil {
			t.Fatalf("File(%q) should be rejected", bad)
		}
	}
	if _, err := File("bot1", "u1", "sub"); err == nil {
		t.Fatal("File on a directory should error")
	}
}

func TestInvalidBotID(t *testing.T) {
	setHome(t)
	for _, bad := range []string{"..", "a/b", "", "."} {
		if _, err := Tree(bad, "u1"); err == nil {
			t.Fatalf("Tree with bot id %q should be rejected", bad)
		}
	}
}
