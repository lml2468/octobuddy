package workflows

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCRUDAndValidation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := Create("deploy"); err != nil {
		t.Fatal(err)
	}
	if err := Create("deploy"); err == nil {
		t.Error("duplicate create should error")
	}
	got, _ := Read("deploy")
	if got == "" || !filepathHasExt(filepath.Join(Dir(), "deploy.js")) {
		t.Fatalf("read/scaffold failed: %q", got)
	}
	if err := Write("deploy", "export const meta={name:'deploy',description:'ship it'}\n"); err != nil {
		t.Fatal(err)
	}
	list, _ := List()
	if len(list) != 1 || list[0].Name != "deploy" || list[0].Description != "ship it" {
		t.Fatalf("list = %+v", list)
	}
	for _, bad := range []string{"../evil", "a/b", "", "..", "x.js/y"} {
		if err := Create(bad); err == nil {
			t.Errorf("invalid name %q should be rejected", bad)
		}
	}
	if err := Delete("deploy"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(Dir(), "deploy.js")); !os.IsNotExist(err) {
		t.Error("workflow should be gone after Delete")
	}
}

func filepathHasExt(p string) bool { _, err := os.Stat(p); return err == nil }

func botPath(t *testing.T, id string) string {
	t.Helper()
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".xclaw", id, "workflows")
}

func TestInstallPruneAndBroken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := Create("review"); err != nil { // catalog workflow
		t.Fatal(err)
	}
	if err := Install("bot1", "review"); err != nil {
		t.Fatal(err)
	}
	// Installed entry is a symlink, listed as installed.
	if fi, err := os.Lstat(filepath.Join(botPath(t, "bot1"), "review.js")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("installed workflow should be a symlink: %v", err)
	}
	list, _ := BotList("bot1")
	if len(list) != 1 || !list[0].Installed || list[0].Broken {
		t.Fatalf("BotList = %+v", list)
	}

	// bot2 authors its own same-named script.
	if err := BotCreate("bot2", "review"); err != nil {
		t.Fatal(err)
	}
	// Own script is read/write; installed one is read-only.
	if err := BotWrite("bot2", "review", "export const meta={name:'review'}\n"); err != nil {
		t.Errorf("own workflow should be writable: %v", err)
	}
	if err := BotWrite("bot1", "review", "x"); err == nil {
		t.Error("writing an installed workflow should be refused")
	}

	// Catalog delete prunes bot1's symlink, keeps bot2's own script.
	if err := Delete("review"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(botPath(t, "bot1"), "review.js")); !os.IsNotExist(err) {
		t.Error("bot1 install symlink should be pruned on catalog delete")
	}
	if _, err := os.Stat(filepath.Join(botPath(t, "bot2"), "review.js")); err != nil {
		t.Error("bot2's own script must survive catalog delete")
	}
}

func TestBrokenWorkflowSurfaced(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_ = Create("ghost")
	if err := Install("bot1", "ghost"); err != nil {
		t.Fatal(err)
	}
	// Remove the catalog target directly, leaving a dangling per-bot symlink.
	if err := os.Remove(filepath.Join(Dir(), "ghost.js")); err != nil {
		t.Fatal(err)
	}
	list, _ := BotList("bot1")
	if len(list) != 1 || !list[0].Broken || !list[0].Installed {
		t.Fatalf("broken workflow install should be surfaced: %+v", list)
	}
	if err := Uninstall("bot1", "ghost"); err != nil {
		t.Errorf("broken install should be uninstallable: %v", err)
	}
}
