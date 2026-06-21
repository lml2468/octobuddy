package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setup points botDir() (os.UserHomeDir) at a fresh temp dir on every OS:
// UserHomeDir reads $HOME on unix but %USERPROFILE% on Windows, so set both —
// otherwise tests share the real home and pollute each other.
func setup(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
}

// botPath returns ~/.xclaw/<id>/.claude/skills under the test HOME.
func botPath(t *testing.T, id string) string {
	t.Helper()
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".xclaw", id, ".claude", "skills")
}

func TestBotCreateListFilesRoundTrip(t *testing.T) {
	setup(t)
	if err := BotCreate("bot1", "demo"); err != nil {
		t.Fatal(err)
	}
	if err := BotCreate("bot1", "demo"); err == nil {
		t.Error("creating an existing skill should error")
	}
	if err := BotWrite("bot1", "demo", "scripts/run.sh", "#!/bin/sh\necho hi\n"); err != nil {
		t.Fatal(err)
	}
	files, err := BotFiles("bot1", "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 { // SKILL.md + scripts/run.sh
		t.Fatalf("files = %v, want 2", files)
	}
	got, _ := BotRead("bot1", "demo", "scripts/run.sh")
	if !strings.Contains(got, "echo hi") {
		t.Errorf("read back %q", got)
	}
	list, _ := BotList("bot1")
	if len(list) != 1 || list[0].Name != "demo" || list[0].Files != 2 {
		t.Fatalf("list = %+v", list)
	}
	if list[0].Description == "" {
		t.Errorf("scaffolded SKILL.md should yield a description")
	}
	if err := BotDeleteFile("bot1", "demo", "scripts/run.sh"); err != nil {
		t.Fatal(err)
	}
	if err := BotDelete("bot1", "demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(botPath(t, "bot1"), "demo")); !os.IsNotExist(err) {
		t.Error("skill dir should be gone after BotDelete")
	}
}

func TestPathTraversalRejected(t *testing.T) {
	setup(t)
	_ = BotCreate("bot1", "demo")
	// Plant a secret outside the skill dir; ensure it can't be read/written via ...
	root := botPath(t, "bot1")
	outside := filepath.Join(root, "..", "secret.txt")
	_ = os.WriteFile(outside, []byte("TOPSECRET"), 0o644)

	for _, rel := range []string{"../secret.txt", "../../secret.txt", "/etc/passwd", "a/../../secret.txt"} {
		if _, err := BotRead("bot1", "demo", rel); err == nil {
			t.Errorf("BotRead(%q) should be rejected", rel)
		}
		if err := BotWrite("bot1", "demo", rel, "x"); err == nil {
			t.Errorf("BotWrite(%q) should be rejected", rel)
		}
	}
	if b, _ := os.ReadFile(outside); string(b) != "TOPSECRET" {
		t.Error("path traversal modified a file outside the skill dir")
	}
	// invalid skill names rejected
	if err := BotCreate("bot1", "../evil"); err == nil {
		t.Error("invalid skill name should be rejected")
	}
	// invalid bot ids rejected
	if _, err := BotList("../bot"); err == nil {
		t.Error("invalid bot id should be rejected")
	}
}

// Each bot's skills live in its own .claude/skills dir; modifying one must not
// affect another.
func TestPerBotIsolation(t *testing.T) {
	setup(t)
	if err := BotCreate("alpha", "shared"); err != nil {
		t.Fatal(err)
	}
	if err := BotCreate("beta", "shared"); err != nil {
		t.Fatal(err)
	}
	if err := BotWrite("alpha", "shared", "note.md", "alpha-only"); err != nil {
		t.Fatal(err)
	}
	// beta's same-named skill should NOT see alpha's edit.
	files, _ := BotFiles("beta", "shared")
	if len(files) != 1 || files[0] != "SKILL.md" {
		t.Fatalf("beta files leaked from alpha: %v", files)
	}
	// Deleting alpha's skill leaves beta's intact.
	if err := BotDelete("alpha", "shared"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(botPath(t, "beta"), "shared", "SKILL.md")); err != nil {
		t.Errorf("beta's skill must survive alpha's delete: %v", err)
	}
}
