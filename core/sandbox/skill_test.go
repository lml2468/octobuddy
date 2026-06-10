package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// mkSkill creates <root>/<name>/SKILL.md so the dir looks like a skill.
func mkSkill(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLinkSkillsBasic(t *testing.T) {
	base := t.TempDir()
	global := filepath.Join(base, "gskills")
	srcA := mkSkill(t, global, "alpha")
	sandboxDir := filepath.Join(base, "sbx")

	if err := LinkSkillsIntoSandbox(sandboxDir, []string{global}); err != nil {
		t.Fatalf("link: %v", err)
	}
	link := filepath.Join(sandboxDir, ".claude", "skills", "alpha")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("alpha not symlinked: %v", err)
	}
	if target != srcA {
		t.Fatalf("alpha → %q, want %q", target, srcA)
	}
}

func TestLinkSkillsPerBotShadowsGlobal(t *testing.T) {
	base := t.TempDir()
	global := filepath.Join(base, "gskills")
	perBot := filepath.Join(base, "bskills")
	mkSkill(t, global, "dup")
	botDup := mkSkill(t, perBot, "dup")
	sandboxDir := filepath.Join(base, "sbx")

	// sources ascending precedence: [global, perBot] → perBot wins.
	if err := LinkSkillsIntoSandbox(sandboxDir, []string{global, perBot}); err != nil {
		t.Fatal(err)
	}
	target, _ := os.Readlink(filepath.Join(sandboxDir, ".claude", "skills", "dup"))
	if target != botDup {
		t.Fatalf("per-bot should shadow global: dup → %q, want %q", target, botDup)
	}
}

func TestLinkSkillsPreservesRealEntriesAndPrunesStale(t *testing.T) {
	base := t.TempDir()
	global := filepath.Join(base, "gskills")
	mkSkill(t, global, "live")
	sandboxDir := filepath.Join(base, "sbx")
	skillsRoot := filepath.Join(sandboxDir, ".claude", "skills")

	// First link round.
	if err := LinkSkillsIntoSandbox(sandboxDir, []string{global}); err != nil {
		t.Fatal(err)
	}
	// The agent created its own real dir in skillsRoot — must survive pruning.
	mine := filepath.Join(skillsRoot, "mine")
	if err := os.MkdirAll(mine, 0o755); err != nil {
		t.Fatal(err)
	}
	// A stale managed symlink pointing at a now-removed source.
	stale := filepath.Join(skillsRoot, "ghost")
	_ = os.Symlink(filepath.Join(base, "gone"), stale)

	// Re-link: only "live" is desired now.
	if err := LinkSkillsIntoSandbox(sandboxDir, []string{global}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mine); err != nil {
		t.Fatal("real dir in skillsRoot must not be pruned")
	}
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatal("stale managed symlink should be pruned")
	}
	if _, err := os.Readlink(filepath.Join(skillsRoot, "live")); err != nil {
		t.Fatal("live skill link must remain")
	}
}

func TestLinkSkillsReplacesWrongTarget(t *testing.T) {
	base := t.TempDir()
	global := filepath.Join(base, "gskills")
	correct := mkSkill(t, global, "s")
	sandboxDir := filepath.Join(base, "sbx")
	skillsRoot := filepath.Join(sandboxDir, ".claude", "skills")
	if err := os.MkdirAll(skillsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-existing symlink pointing at the wrong place.
	_ = os.Symlink(filepath.Join(base, "wrong"), filepath.Join(skillsRoot, "s"))

	if err := LinkSkillsIntoSandbox(sandboxDir, []string{global}); err != nil {
		t.Fatal(err)
	}
	target, _ := os.Readlink(filepath.Join(skillsRoot, "s"))
	if target != correct {
		t.Fatalf("wrong-target symlink not repaired: %q, want %q", target, correct)
	}
}

func TestLinkSkillsSkipsDotfiles(t *testing.T) {
	base := t.TempDir()
	global := filepath.Join(base, "gskills")
	mkSkill(t, global, ".hidden")
	mkSkill(t, global, "visible")
	sandboxDir := filepath.Join(base, "sbx")
	if err := LinkSkillsIntoSandbox(sandboxDir, []string{global}); err != nil {
		t.Fatal(err)
	}
	skillsRoot := filepath.Join(sandboxDir, ".claude", "skills")
	if _, err := os.Lstat(filepath.Join(skillsRoot, ".hidden")); !os.IsNotExist(err) {
		t.Fatal("dotfile skill must be skipped")
	}
	if _, err := os.Readlink(filepath.Join(skillsRoot, "visible")); err != nil {
		t.Fatal("visible skill must be linked")
	}
}
