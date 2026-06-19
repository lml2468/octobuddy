package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setup(t *testing.T) { t.Helper(); t.Setenv("HOME", t.TempDir()) }

func TestCreateListFilesRoundTrip(t *testing.T) {
	setup(t)
	if err := Create("demo"); err != nil {
		t.Fatal(err)
	}
	if err := Create("demo"); err == nil {
		t.Error("creating an existing skill should error")
	}
	if err := WriteFile("demo", "scripts/run.sh", "#!/bin/sh\necho hi\n"); err != nil {
		t.Fatal(err)
	}
	files, err := Files("demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 { // SKILL.md + scripts/run.sh
		t.Fatalf("files = %v, want 2", files)
	}
	got, _ := ReadFile("demo", "scripts/run.sh")
	if !strings.Contains(got, "echo hi") {
		t.Errorf("read back %q", got)
	}
	list, _ := List()
	if len(list) != 1 || list[0].Name != "demo" || list[0].Files != 2 {
		t.Fatalf("list = %+v", list)
	}
	if list[0].Description == "" {
		t.Errorf("scaffolded SKILL.md should yield a description")
	}
	if err := DeleteFile("demo", "scripts/run.sh"); err != nil {
		t.Fatal(err)
	}
	if err := Delete("demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(Dir(), "demo")); !os.IsNotExist(err) {
		t.Error("skill dir should be gone after Delete")
	}
}

func TestPathTraversalRejected(t *testing.T) {
	setup(t)
	_ = Create("demo")
	// Plant a secret outside the skill dir; ensure it can't be read/written via ...
	outside := filepath.Join(Dir(), "..", "secret.txt")
	_ = os.WriteFile(outside, []byte("TOPSECRET"), 0o644)

	for _, rel := range []string{"../secret.txt", "../../secret.txt", "/etc/passwd", "a/../../secret.txt"} {
		if _, err := ReadFile("demo", rel); err == nil {
			t.Errorf("ReadFile(%q) should be rejected", rel)
		}
		if err := WriteFile("demo", rel, "x"); err == nil {
			t.Errorf("WriteFile(%q) should be rejected", rel)
		}
	}
	// the outside secret must be untouched
	if b, _ := os.ReadFile(outside); string(b) != "TOPSECRET" {
		t.Error("path traversal modified a file outside the skill dir")
	}
	// invalid skill names rejected
	if err := Create("../evil"); err == nil {
		t.Error("invalid skill name should be rejected")
	}
}

func TestInstallUninstall(t *testing.T) {
	setup(t)
	if err := Create("translator"); err != nil { // catalog skill
		t.Fatal(err)
	}
	if err := Install("bot1", "translator"); err != nil {
		t.Fatal(err)
	}
	// Per-bot entry is a symlink into the catalog.
	bp := filepath.Join(botPath(t, "bot1"), "translator")
	if fi, err := os.Lstat(bp); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("installed skill should be a symlink: %v", err)
	}
	// Idempotent.
	if err := Install("bot1", "translator"); err != nil {
		t.Errorf("re-install should be idempotent: %v", err)
	}
	// Listed as installed; files resolve through the chain.
	list, _ := BotList("bot1")
	if len(list) != 1 || !list[0].Installed || list[0].Files != 1 {
		t.Fatalf("BotList = %+v", list)
	}
	// Installing a missing catalog skill fails.
	if err := Install("bot1", "nope"); err == nil {
		t.Error("installing a non-catalog skill should fail")
	}
	// Uninstall removes only the symlink; catalog is untouched.
	if err := Uninstall("bot1", "translator"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(bp); !os.IsNotExist(err) {
		t.Error("symlink should be gone after uninstall")
	}
	if _, err := os.Stat(filepath.Join(Dir(), "translator")); err != nil {
		t.Error("catalog skill must survive uninstall")
	}
}

func TestBotOwnVsInstalledGuards(t *testing.T) {
	setup(t)
	_ = Create("shared") // catalog
	if err := BotCreate("bot1", "mine"); err != nil {
		t.Fatal(err)
	}
	if err := Install("bot1", "shared"); err != nil {
		t.Fatal(err)
	}
	// Own bundle is editable.
	if err := BotWrite("bot1", "mine", "a.txt", "x"); err != nil {
		t.Errorf("own bundle should be writable: %v", err)
	}
	// Installed bundle is read-only via per-bot API.
	if err := BotWrite("bot1", "shared", "a.txt", "x"); err == nil {
		t.Error("writing into an installed skill should be refused")
	}
	if err := BotDelete("bot1", "shared"); err == nil {
		t.Error("BotDelete on an installed skill should be refused (use Uninstall)")
	}
	// Uninstall on an own bundle is refused.
	if err := Uninstall("bot1", "mine"); err == nil {
		t.Error("Uninstall on a real per-bot bundle should be refused")
	}
	// Install refuses to clobber an own bundle of the same name.
	_ = Create("mine") // also in catalog now
	if err := Install("bot1", "mine"); err == nil {
		t.Error("Install should not overwrite a real per-bot bundle")
	}
}

// botPath returns ~/.xclaw/<id>/skills under the test HOME.
func botPath(t *testing.T, id string) string {
	t.Helper()
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".xclaw", id, "skills")
}

// Deleting a catalog skill must prune the install symlinks in every bot, but
// leave a bot's own same-named bundle alone; the orphan must never silently
// linger (regression: code review findings 1 & 2).
func TestCatalogDeletePrunesInstalls(t *testing.T) {
	setup(t)
	_ = Create("shared")
	if err := Install("bot1", "shared"); err != nil { // bot1 installs it (symlink)
		t.Fatal(err)
	}
	if err := BotCreate("bot2", "shared"); err != nil { // bot2 has its OWN same-named bundle
		t.Fatal(err)
	}

	if err := Delete("shared"); err != nil { // delete from the catalog
		t.Fatal(err)
	}

	// bot1's install symlink is pruned.
	if _, err := os.Lstat(filepath.Join(botPath(t, "bot1"), "shared")); !os.IsNotExist(err) {
		t.Error("bot1 install symlink should be pruned when the catalog entry is deleted")
	}
	// bot2's own bundle survives.
	if _, err := os.Stat(filepath.Join(botPath(t, "bot2"), "shared", "SKILL.md")); err != nil {
		t.Error("bot2's own bundle must survive a catalog delete")
	}
}

// A dangling install symlink (catalog target gone) must still appear in BotList,
// flagged Broken, so the user can uninstall it (regression: finding 2).
func TestBrokenInstallSurfaced(t *testing.T) {
	setup(t)
	_ = Create("ghost")
	if err := Install("bot1", "ghost"); err != nil {
		t.Fatal(err)
	}
	// Remove the catalog target directly, leaving the per-bot symlink dangling.
	if err := os.RemoveAll(filepath.Join(Dir(), "ghost")); err != nil {
		t.Fatal(err)
	}
	list, err := BotList("bot1")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "ghost" || !list[0].Broken || !list[0].Installed {
		t.Fatalf("broken install should be surfaced: %+v", list)
	}
	// And it must be uninstallable (it's a symlink).
	if err := Uninstall("bot1", "ghost"); err != nil {
		t.Errorf("broken install should be uninstallable: %v", err)
	}
}
